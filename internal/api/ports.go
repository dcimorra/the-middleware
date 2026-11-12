package api

import (
	"net/http"

	"github.com/dcimorra/the-middleware/internal/auth"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/pubports"
)

// PortsOptions is what the public-ports routes need of the house.
type PortsOptions struct {
	// Book keeps what the owner approved.
	Book *pubports.Book
	// Reserved are the ports the house itself holds here; PublicIP the
	// address this node is reached at, "" while unknown.
	Reserved func() []manifest.HeldPort
	PublicIP func() string
}

// SitePorts is what a site asks of the machine and where that stands.
type SitePorts struct {
	// PublicIP is this node's address, the one $MIDDLEWARE_PUBLIC_IP is.
	PublicIP string `json:"public_ip,omitempty"`
	// Asked are the lines of the manifest's grant, each with whether the
	// owner approved it.
	Asked []PortLine `json:"asked"`
	// Conflicts are the ports that cannot be the site's on this node: one
	// the house holds, or one another site has.
	Conflicts []string `json:"conflicts,omitempty"`
}

// PortLine is one thing a service asks: a port, or the host's network.
type PortLine struct {
	Line     string `json:"line"`
	Approved bool   `json:"approved"`
}

// owner says the caller is the installation's owner in person: not a
// delegate, a scoped token, a site's agent, the house's own agents.
// Those are who a manifest's public ports are kept from.
func owner(r *http.Request) bool {
	c := auth.FromContext(r.Context())
	return !(c.Delegate() || c.Turn != "" || c.Autopilot || c.Crew != "" || len(c.Scopes) > 0)
}

// RegisterPublicPorts mounts a site's public ports: what it asks, and
// the owner's approval of it.
func (s *Server) RegisterPublicPorts(o PortsOptions) {
	s.mu.Lock()
	s.ports = &o
	s.mu.Unlock()
	p := Prefix + "/sites/{domain}/ports"
	s.Handle("GET", p, "The site's public ports (services.<name>.ports, network: host): each line its manifest asks with whether the owner approved it, this node's public address, and the ports that are taken here", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		return s.sitePorts(domain, !owner(r))
	})
	s.Handle("POST", p+"/approve", "The owner only: approve the public ports the site's manifest asks now; its services with them start", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		if !owner(r) {
			return nil, Errorf(http.StatusForbidden, "only the owner of this installation approves a site's public ports")
		}
		mf, err := s.portsManifest(domain)
		if err != nil {
			return nil, err
		}
		if c := s.portConflicts(domain, mf, false); len(c) > 0 {
			return nil, Errorf(http.StatusConflict, "%s", c[0])
		}
		added, err := o.Book.Approve(domain, mf)
		if err != nil {
			return nil, err
		}
		if len(added) > 0 {
			s.emitSite("ports.approved", domain, "the owner approved its public ports", map[string]any{"approved": added})
		}
		s.kick()
		return s.sitePorts(domain, false)
	})
	s.Handle("POST", p+"/revoke", "The owner only: take back the approval of the site's public ports; its services with them stop", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		if !owner(r) {
			return nil, Errorf(http.StatusForbidden, "only the owner of this installation decides about a site's public ports")
		}
		if err := o.Book.Revoke(domain); err != nil {
			return nil, err
		}
		s.emitSite("ports.revoked", domain, "the owner took back the approval of its public ports", nil)
		s.kick()
		return s.sitePorts(domain, false)
	})
}

func (s *Server) portsOptions() *PortsOptions {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ports
}

func (s *Server) kick() { s.opts.Engine.Kick() }

func (s *Server) emitSite(typ, domain, msg string, data map[string]any) {
	if s.opts.Bus != nil {
		s.opts.Bus.Emit(events.Event{Type: typ, Subject: domain, Actor: "operator", Message: msg, Data: data})
	}
}

// portsManifest is the site's manifest as it is on disk now.
func (s *Server) portsManifest(domain string) (*manifest.Manifest, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	site, err := m.Get(domain)
	if err != nil {
		return nil, err
	}
	if site.Manifest == nil {
		return nil, Errorf(http.StatusConflict, "%s has no manifest the house can read: %s", domain, site.Error)
	}
	return site.Manifest, nil
}

// portConflicts are the reasons mf's public ports cannot be domain's
// here; hide keeps the other sites' names out.
func (s *Server) portConflicts(domain string, mf *manifest.Manifest, hide bool) []string {
	o := s.portsOptions()
	if o == nil || !mf.HasPublic() {
		return nil
	}
	others := map[string]*manifest.Manifest{}
	if m, err := s.sitesManager(); err == nil {
		if list, err := m.List(); err == nil {
			for _, x := range list {
				if x.Domain != domain && x.Manifest != nil && x.Manifest.HasPublic() {
					others[x.Domain] = x.Manifest
				}
			}
		}
	}
	var reserved []manifest.HeldPort
	if o.Reserved != nil {
		reserved = o.Reserved()
	}
	return pubports.Conflicts(domain, mf, others, reserved, hide)
}

func (s *Server) sitePorts(domain string, hide bool) (*SitePorts, error) {
	o := s.portsOptions()
	mf, err := s.portsManifest(domain)
	if err != nil {
		return nil, err
	}
	out := &SitePorts{Asked: []PortLine{}}
	if o.PublicIP != nil {
		out.PublicIP = o.PublicIP()
	}
	pending := o.Book.Pending(domain, mf)
	for _, line := range mf.PublicGrant() {
		approved := true
		for _, p := range pending {
			if p == line {
				approved = false
			}
		}
		out.Asked = append(out.Asked, PortLine{Line: line, Approved: approved})
	}
	out.Conflicts = s.portConflicts(domain, mf, hide)
	return out, nil
}

// ownerWrote is called after the owner saved a site's manifest through
// the house (the panel's manifest tab, the API, a restore of its
// history): what it asks of the machine is approved by the hand that
// wrote it. Anyone else's write leaves the book as it was, and what they
// added waits.
func (s *Server) ownerWrote(r *http.Request, domain string) {
	o := s.portsOptions()
	if o == nil || !owner(r) {
		return
	}
	mf, err := s.portsManifest(domain)
	if err != nil || (!mf.HasPublic() && len(o.Book.Approved(domain)) == 0) {
		return
	}
	if added, err := o.Book.Approve(domain, mf); err == nil && len(added) > 0 {
		s.emitSite("ports.approved", domain, "public ports approved with the manifest the owner saved", map[string]any{"approved": added})
	}
}

// checkPorts refuses a manifest whose public ports are the house's or
// another site's, before it is written: after, the service would only
// say so in its error, and the port would look granted.
func (s *Server) checkPorts(r *http.Request, domain string, next []byte) error {
	mf, err := manifest.Parse(next)
	if err != nil {
		return nil // the manager says what is wrong with it
	}
	if c := s.portConflicts(domain, mf, !owner(r)); len(c) > 0 {
		return Errorf(http.StatusConflict, "%s", c[0])
	}
	return nil
}
