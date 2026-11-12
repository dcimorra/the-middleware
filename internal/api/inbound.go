package api

import (
	"context"
	"github.com/dcimorra/the-middleware/internal/auth"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/inbound"
	"github.com/dcimorra/the-middleware/internal/publish"
)

// DNSInfo is what a DNS provider knows about one name.
type DNSInfo struct {
	// DNS is the record as "A 1.2.3.4" or "CNAME x.cfargotunnel.com";
	// empty when there is none or nobody can tell.
	DNS string `json:"dns"`
	// TunnelRoute says an ingress rule exists for the name.
	TunnelRoute bool `json:"tunnel_route"`
}

// DNSProvider is what the cloudflare module offers the inbound section:
// the two extra columns of GET /inbound. Writing records is the
// publisher's job, not this one's.
type DNSProvider interface {
	Enabled() bool
	// Describe fills a row per domain; names it knows nothing about are
	// left out. One call for all rows, because the provider lists zones
	// once and the panel asks for forty domains at a time.
	Describe(ctx context.Context, domains []string) (map[string]DNSInfo, error)
}

// InboundOptions wire the inbound section.
type InboundOptions struct {
	Hook      *inbound.Hook
	DNS       DNSProvider
	Publisher *publish.Publisher
}

// InboundRow is one domain of GET /inbound.
type InboundRow struct {
	inbound.DomainStatus
	DNS         string `json:"dns"`
	TunnelRoute string `json:"tunnel_route"`
	// Published is the publisher's state for the name ("published",
	// "failed", "manual"…), and PublishError why, when it is not.
	Published    string `json:"published,omitempty"`
	PublishError string `json:"publish_error,omitempty"`
}

// RegisterInbound adds the /inbound routes.
func (s *Server) RegisterInbound(o InboundOptions) {
	h := o.Hook
	p := Prefix + "/inbound"
	s.publisher = o.Publisher
	if o.DNS != nil {
		s.dnsEnabled = o.DNS.Enabled
	}
	if o.Publisher != nil {
		s.registerPublish(o.Publisher, h)
	}
	// One domain at a time, by the same path as the move below.
	s.registerMigration(func(ctx context.Context, domain string, to inbound.Door) []inbound.Line {
		if o.Publisher != nil {
			return o.Publisher.Move(ctx, []string{domain}, to)
		}
		return h.Move(ctx, []string{domain}, to)
	})
	s.Handle("GET", p, "The doors and every domain's door, certificate and DNS", func(w http.ResponseWriter, r *http.Request) (any, error) {
		st := h.Status()
		rows := make([]InboundRow, 0, len(st.Domains))
		names := make([]string, 0, len(st.Domains))
		for _, d := range st.Domains {
			rows = append(rows, InboundRow{DomainStatus: d})
			names = append(names, d.Domain)
		}
		var dnsErr string
		if o.DNS != nil && o.DNS.Enabled() && len(names) > 0 {
			ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
			defer cancel()
			info, err := o.DNS.Describe(ctx, names)
			if err != nil {
				// The doors are the answer; a DNS provider that is down
				// is a column that says so, not a 502 over the whole page.
				dnsErr = err.Error()
			}
			for i := range rows {
				if d, ok := info[rows[i].Domain]; ok {
					rows[i].DNS = d.DNS
					if d.TunnelRoute {
						rows[i].TunnelRoute = "yes"
					} else {
						rows[i].TunnelRoute = "no"
					}
				}
			}
		}
		if o.Publisher != nil {
			recs := o.Publisher.Records()
			for i := range rows {
				if r, ok := recs[rows[i].Domain]; ok {
					rows[i].Published, rows[i].PublishError = r.State, r.Error
				}
			}
		}
		out := map[string]any{"doors": st.Doors, "domains": rows, "new_sites": h.State().DoorForNew(), "public_ip": h.State().PublicIP}
		if dnsErr != "" {
			out["dns_error"] = dnsErr
		}
		return out, nil
	})
	s.Handle("POST", p+"/move", "Put domains on a door, obtaining certificates and publishing DNS as needed", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var body struct {
			Domains []string `json:"domains"`
			To      string   `json:"to"`
		}
		if err := ReadJSON(r, &body); err != nil {
			return nil, err
		}
		door, err := inbound.ParsePublicDoor(body.To)
		if err != nil {
			return nil, Errorf(http.StatusBadRequest, "%v", err)
		}
		if len(body.Domains) == 0 {
			return nil, Errorf(http.StatusBadRequest, "domains: none given")
		}
		// Obtaining a certificate takes the CA's time, not the client's:
		// the move continues if the panel's tab goes away.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		var lines []inbound.Line
		if o.Publisher != nil {
			// The same path as publishing a site: certificate, door,
			// DNS, under the one lock that guards the tunnel's rules.
			lines = o.Publisher.Move(ctx, body.Domains, door)
		} else {
			lines = h.Move(ctx, body.Domains, door)
		}
		ok := true
		for _, l := range lines {
			if l.Status == "fail" {
				ok = false
			}
		}
		return map[string]any{"ok": ok, "lines": lines}, nil
	})
	s.Handle("POST", p+"/doors/{door}", "Open or close a door", func(w http.ResponseWriter, r *http.Request) (any, error) {
		door, err := inbound.ParseDoor(r.PathValue("door"))
		if err != nil {
			return nil, Errorf(http.StatusBadRequest, "%v", err)
		}
		var body struct {
			Open bool `json:"open"`
		}
		if err := ReadJSON(r, &body); err != nil {
			return nil, err
		}
		if err := h.SetDoor(door, body.Open); err != nil {
			return nil, err
		}
		return h.Status().Doors, nil
	})
	s.Handle("GET", p+"/panel-key", "Whether the panel answers only with its key, and since when", func(w http.ResponseWriter, r *http.Request) (any, error) {
		k := h.PanelKeyState()
		out := map[string]any{"on": k.On(), "param": inbound.PanelKeyParam}
		if k.On() {
			out["since"] = k.Since
		}
		return out, nil
	})
	// The key is in this answer and nowhere else: only its hash is kept.
	// The caller's browser gets the cookie with it, or the panel would
	// close on the one who just turned it on at their next request.
	s.Handle("POST", p+"/panel-key", "Hide the panel behind a new key; the key is in this answer only", func(w http.ResponseWriter, r *http.Request) (any, error) {
		key, err := h.SetPanelKey(true)
		if err != nil {
			return nil, err
		}
		inbound.KeyCookie(w, r, key)
		return map[string]any{"on": true, "key": key, "param": inbound.PanelKeyParam, "since": h.PanelKeyState().Since}, nil
	})
	s.Handle("DELETE", p+"/panel-key", "Stop hiding the panel behind a key", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if _, err := h.SetPanelKey(false); err != nil {
			return nil, err
		}
		return map[string]any{"on": false}, nil
	})
	s.Handle("PUT", p+"/local", "Set the local door: open flag and the http and dns addresses it listens on", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var body struct {
			Open bool   `json:"open"`
			HTTP string `json:"http"`
			DNS  string `json:"dns"`
		}
		if err := ReadJSON(r, &body); err != nil {
			return nil, err
		}
		if err := h.SetLocal(body.Open, body.HTTP, body.DNS); err != nil {
			return nil, Errorf(http.StatusBadRequest, "%v", err)
		}
		return h.Status().Doors, nil
	})
	s.Handle("GET", p+"/certificates", "The certificates on disk", func(w http.ResponseWriter, r *http.Request) (any, error) {
		list := h.Store.List()
		if list == nil {
			return []struct{}{}, nil
		}
		return list, nil
	})
	s.Handle("POST", p+"/certificates/{domain}/renew", "Renew a certificate now", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := h.Store.Renew(ctx, domain); err != nil {
			return nil, Errorf(http.StatusBadGateway, "%v", err)
		}
		return OK, nil
	})
}

func (s *Server) registerPublish(pub *publish.Publisher, h *inbound.Hook) {
	s.Handle("GET", Prefix+"/publish", "What the house published: every name, its door and its state", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return pub.Records(), nil
	})
	s.Handle("POST", Prefix+"/publish/takeover", "Point names found in conflict at their site, replacing what the zone had on them (body: names); only the operator's yes does this", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var body struct {
			Names []string `json:"names"`
		}
		if err := ReadJSON(r, &body); err != nil {
			return nil, err
		}
		if len(body.Names) == 0 {
			return nil, Errorf(http.StatusBadRequest, "names: empty")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		return map[string]any{"lines": pub.Takeover(ctx, body.Names)}, nil
	})
	s.Handle("POST", Prefix+"/publish/retry", "Retry every name that is not published now, manual ones included", func(w http.ResponseWriter, r *http.Request) (any, error) {
		pub.Retry()
		return OK, nil
	})
	s.Handle("POST", Prefix+"/sites/{domain}/publish", "Publish every name of the site on a door (body: door; empty is the door for new sites)", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		var body struct {
			Door inbound.Door `json:"door"`
			// Over is a move's: the site arrived from another node, and its
			// names point there. Only a node may ask it; the operator takes
			// a name in conflict with /publish/takeover, one by one.
			Over bool `json:"over"`
		}
		if r.ContentLength != 0 {
			if err := ReadJSON(r, &body); err != nil {
				return nil, err
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		publishFn := pub.Publish
		if body.Over {
			// A node moving the site here, or the import token the operator
			// made in this panel for this one site: the names follow the
			// site, as the operator asked when making it. Anyone else takes
			// a name in conflict one by one.
			c := auth.FromContext(r.Context())
			if _, ok := s.fromNode(c); !ok && !slices.Contains(c.Scopes, "import:"+domain) {
				return nil, Errorf(http.StatusForbidden, "over is for a site moving in from another node or imported with its token; take a name in conflict with POST /publish/takeover")
			}
			publishFn = pub.PublishOver
		}
		lines, err := publishFn(ctx, domain, body.Door)
		if err != nil {
			return nil, Errorf(http.StatusBadRequest, "%v", err)
		}
		out := map[string]any{"lines": lines}
		if s.vulnerable != nil {
			if v := s.vulnerable(domain); v != "" {
				out["vulnerable"] = v
			}
		}
		return out, nil
	})
	s.Handle("POST", Prefix+"/sites/{domain}/unpublish", "Remove the site's tunnel rules and the records that point at this house", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		return map[string]any{"lines": pub.Unpublish(ctx, domain)}, nil
	})
	s.Handle("PUT", Prefix+"/inbound/publishing", "Set the door for new sites and the public address of the ports door (empty public_ip detects it)", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var body struct {
			NewSites inbound.Door `json:"new_sites"`
			PublicIP *string      `json:"public_ip"`
		}
		if err := ReadJSON(r, &body); err != nil {
			return nil, err
		}
		if body.NewSites != "" {
			if _, err := inbound.ParsePublicDoor(string(body.NewSites)); err != nil {
				return nil, Errorf(http.StatusBadRequest, "%v", err)
			}
		}
		ip, clear := "", false
		if body.PublicIP != nil {
			ip = strings.TrimSpace(*body.PublicIP)
			clear = ip == ""
			if !clear {
				if parsed := net.ParseIP(ip); parsed == nil || parsed.To4() == nil {
					return nil, Errorf(http.StatusBadRequest, "public_ip: %q is not an IPv4 address", ip)
				}
			}
		}
		if err := h.SetPublishing(body.NewSites, ip, clear); err != nil {
			return nil, err
		}
		st := h.State()
		return map[string]any{"new_sites": st.DoorForNew(), "public_ip": st.PublicIP}, nil
	})
}
