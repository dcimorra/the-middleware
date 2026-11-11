package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/auth"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/installer"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/nodes"
)

// RegisterNodes mounts the mesh: this node and the others of its network,
// the invitation that brings a machine in, a summary of each for the
// dashboard and the door through which this panel works on another node.
func (s *Server) RegisterNodes(m *nodes.Mesh, canClose func() error) {
	s.mu.Lock()
	s.mesh = m
	s.meshCanClose = canClose
	s.mu.Unlock()
	s.Handle("PUT", Prefix+"/nodes/settings", "Change the network's settings (the panel of every node only through a VPN), for every node", s.nodesSettings)
	s.Handle("GET", Prefix+"/nodes", "This node, its network and the other nodes, with what the tunnel says of each", s.nodesList)
	s.Handle("PUT", Prefix+"/nodes/self", "Rename this node", s.nodesRename)
	s.Handle("POST", Prefix+"/nodes/invite", "Make a single-use code, valid fifteen minutes, for another machine to join; creates the network the first time", s.nodesInvite)
	s.Handle("POST", Prefix+"/nodes/join", "Join the network an invitation code was made in", s.nodesJoin)
	s.Handle("POST", Prefix+"/nodes/leave", "Take this node out of its network", s.nodesLeave)
	s.Handle("DELETE", Prefix+"/nodes/{id}", "Remove a node from the network, for every node", s.nodesExpel)
	s.Handle("GET", Prefix+"/nodes/overview", "Every node's summary, this one first, asked in parallel; an unreachable node says why", s.nodesOverview)
	s.Handle("GET", Prefix+"/node/summary", "What the dashboard shows of this node: machine, sites, conversations, recent trouble", s.nodeSummary)
	// Under /system, not /nodes: another node's panel asks it through the
	// mesh, and /nodes is what a node may not ask of another (nodeMay).
	s.Handle("POST", Prefix+"/system/panel/network-only", "Take this node's panel off its domain: its DNS record and tunnel route are retired and it is managed only from the network (another node's panel, through the mesh) or locally. Refused outside a network or with no other node answering", s.panelNetworkOnly)
	s.registerRelocateSource(m)
	for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE"} {
		s.Handle(method, Prefix+"/nodes/{id}/api/{path...}", "The API of another node, through the mesh: what the panel uses to work on another machine", s.nodesProxy)
	}
}

func (s *Server) meshOf() (*nodes.Mesh, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mesh == nil {
		return nil, Errorf(http.StatusNotImplemented, "nodes are not available")
	}
	return s.mesh, nil
}

// fromNode says whether the caller is another node of the mesh.
func (s *Server) fromNode(c auth.Caller) (string, bool) {
	return c.Token, c.Via == auth.ViaNode
}

// nodeMay is what another node may ask of this one: everything an operator
// can, except the network itself and the operator's credentials, which are
// decided at each machine's own panel. A request that came through the
// mesh cannot go on through it either: the node at the far end of a chain
// would be hidden from the one that answers.
func nodeMay(pattern string) bool {
	_, path, _ := strings.Cut(pattern, " ")
	if pattern == "GET "+Prefix+"/auth/me" || pattern == "GET "+Prefix+"/node/summary" {
		return true
	}
	return !strings.HasPrefix(path, Prefix+"/nodes") && !strings.HasPrefix(path, Prefix+"/auth/")
}

func meshErr(err error) error {
	switch {
	case errors.Is(err, nodes.ErrNotFound):
		return Errorf(http.StatusNotFound, "%v", err)
	case errors.Is(err, nodes.ErrNoHelper):
		return Errorf(http.StatusServiceUnavailable, "%v", err)
	case errors.Is(err, nodes.ErrInNetwork), errors.Is(err, nodes.ErrNoNetwork):
		return Errorf(http.StatusConflict, "%v", err)
	case errors.Is(err, nodes.ErrBadInvite), errors.Is(err, nodes.ErrBadName), errors.Is(err, nodes.ErrNoEndpoint), errors.Is(err, nodes.ErrBadEndpoint):
		return Errorf(http.StatusBadRequest, "%v", err)
	}
	return Errorf(http.StatusBadGateway, "%v", err)
}

func (s *Server) nodesList(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.meshOf()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	return m.Info(ctx), nil
}

func (s *Server) nodesSettings(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.meshOf()
	if err != nil {
		return nil, err
	}
	var body nodes.NetSettings
	if err := ReadJSON(r, &body); err != nil {
		return nil, err
	}
	// Closing every panel is decided where a way in is known to exist: a
	// VPN with a device here. From a node without one, the operator could
	// not say whether any node of the network still has a door.
	if body.PanelVPNOnly && !m.Settings().PanelVPNOnly {
		s.mu.Lock()
		check := s.meshCanClose
		s.mu.Unlock()
		if check != nil {
			if err := check(); err != nil {
				return nil, Errorf(http.StatusConflict, "%v", err)
			}
		}
	}
	if err := m.SetSettings(body); err != nil {
		return nil, meshErr(err)
	}
	return m.Settings(), nil
}

func (s *Server) nodesRename(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.meshOf()
	if err != nil {
		return nil, err
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := ReadJSON(r, &body); err != nil {
		return nil, err
	}
	if err := m.SetName(body.Name); err != nil {
		return nil, meshErr(err)
	}
	return m.Self(), nil
}

func (s *Server) nodesInvite(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.meshOf()
	if err != nil {
		return nil, err
	}
	var body struct {
		// Endpoint is this machine's public ip:port, needed only when the
		// invitation creates the network.
		Endpoint string `json:"endpoint"`
	}
	if err := ReadJSON(r, &body); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	code, exp, err := m.Invite(ctx, body.Endpoint)
	if err != nil {
		return nil, meshErr(err)
	}
	s.opts.Bus.Emit(events.Event{Type: "node.invited", Actor: "operator", Message: "an invitation to join the network was made", Data: map[string]any{"expires": exp}})
	out := map[string]any{"code": code, "expires": exp}
	// The same code inside a cloud-init file, for a server that is being
	// rented now: pasted where the provider asks for its user data, the
	// machine installs the house and joins by itself.
	if cc, err := installer.CloudConfig(code); err == nil {
		out["cloud_config"] = cc
	}
	return out, nil
}

func (s *Server) nodesJoin(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.meshOf()
	if err != nil {
		return nil, err
	}
	var body struct {
		Code     string `json:"code"`
		Endpoint string `json:"endpoint"`
	}
	if err := ReadJSON(r, &body); err != nil {
		return nil, err
	}
	// The tunnel's first handshake and the retries take their time; the
	// tab's request is not what decides it.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	inviter, err := m.Join(ctx, body.Code, body.Endpoint)
	if err != nil {
		return nil, meshErr(err)
	}
	return map[string]any{"ok": true, "inviter": inviter.Name, "members": len(m.Members())}, nil
}

func (s *Server) nodesLeave(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.meshOf()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := m.Leave(ctx); err != nil {
		return nil, meshErr(err)
	}
	return OK, nil
}

func (s *Server) nodesExpel(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.meshOf()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	x, err := m.Expel(ctx, r.PathValue("id"))
	if err != nil {
		return nil, meshErr(err)
	}
	return map[string]any{"ok": true, "name": x.Name}, nil
}

func (s *Server) nodesProxy(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.meshOf()
	if err != nil {
		return nil, err
	}
	x, ok := m.Member(r.PathValue("id"))
	if !ok {
		return nil, Errorf(http.StatusNotFound, "%v", nodes.ErrNotFound)
	}
	m.Proxy(x, r.PathValue("path")).ServeHTTP(w, r)
	return Handled, nil
}

// Summarizer adds a part to the node summary under its key.
type Summarizer func() any

// AddSummary registers a part of GET /node/summary. The packages that know
// a thing (the vitals, the conversations, the updater) put it there; the
// summary itself knows none of them.
func (s *Server) AddSummary(key string, fn Summarizer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.summaries == nil {
		s.summaries = map[string]Summarizer{}
	}
	s.summaries[key] = fn
}

// SiteCount is the sites part of a summary.
type SiteCount struct {
	Sites    int `json:"sites"`
	Projects int `json:"projects"`
	Serving  int `json:"serving"`
	Stopped  int `json:"stopped"`
	// Trouble is every site that should answer and does not, by name: the
	// one list the dashboard exists to show.
	Trouble []SiteTrouble `json:"trouble"`
}

// SiteTrouble is one site in trouble.
type SiteTrouble struct {
	Domain string `json:"domain"`
	Why    string `json:"why"`
}

// panelNetworkOnly takes the panel off its domain, for a node in a network.
// What makes it safe is the network: another node's panel reaches this one
// through the mesh, and a node with no other node answering would be left
// with an SSH tunnel as the only way in, so it is refused there. Undone by
// giving the panel a domain again (Settings).
func (s *Server) panelNetworkOnly(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.meshOf()
	if err != nil {
		return nil, err
	}
	if !m.InNetwork() {
		return nil, Errorf(http.StatusConflict, "this node is in no network: without its domain its panel would only be reached through an SSH tunnel; join a network first")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	var from []string
	for _, x := range m.Info(ctx).Members {
		if x.Online {
			from = append(from, x.Name)
		}
	}
	if len(from) == 0 {
		return nil, Errorf(http.StatusConflict, "no other node of the network answers now: without its domain this panel could not be reached from anywhere but an SSH tunnel")
	}
	if s.opts.SaveSettings == nil {
		return nil, Errorf(http.StatusNotImplemented, "settings cannot be saved here")
	}
	st := s.opts.Settings()
	was := st.PanelDomain
	if was == "" {
		return nil, Errorf(http.StatusConflict, "the panel has no domain already")
	}
	st.PanelDomain = ""
	if err := s.opts.SaveSettings(st); err != nil {
		return nil, err
	}
	if s.opts.Bus != nil {
		s.opts.Bus.Emit(events.Event{Type: "panel.network_only", Actor: "operator", Level: "warn", Message: "the panel left " + was + ": its record and route are retired, and it is managed from the network (" + strings.Join(from, ", ") + ")", Data: map[string]any{"was": was}})
	}
	return map[string]any{"ok": true, "was": was, "manage_from": from}, nil
}

func (s *Server) nodeSummary(w http.ResponseWriter, r *http.Request) (any, error) {
	out := map[string]any{
		"version":  s.opts.Version,
		"started":  s.opts.Started,
		"hostname": hostname(),
	}
	if s.opts.Settings != nil {
		out["panel_domain"] = s.opts.Settings().PanelDomain
	}
	if m, err := s.meshOf(); err == nil {
		self := m.Self()
		out["id"], out["name"] = self.ID, self.Name
	}
	if s.opts.Sites != nil {
		list, err := s.opts.Sites.List()
		if err == nil {
			c := SiteCount{Trouble: []SiteTrouble{}}
			for _, site := range list {
				mode := site.Mode
				if mode == manifest.ModeProject {
					c.Projects++
					continue
				}
				c.Sites++
				switch {
				case !site.Managed:
					c.Trouble = append(c.Trouble, SiteTrouble{Domain: site.Domain, Why: "unmanaged"})
				case mode == manifest.ModeDisabled || mode == manifest.ModeHold:
					c.Stopped++
				case site.Handing:
					c.Serving++
				case site.Health == "down" && (mode == manifest.ModeRun || mode == manifest.ModeStatic):
					c.Trouble = append(c.Trouble, SiteTrouble{Domain: site.Domain, Why: "not_answering"})
				case mode == manifest.ModeRun && !site.Alive:
					c.Trouble = append(c.Trouble, SiteTrouble{Domain: site.Domain, Why: "down"})
				default:
					c.Serving++
				}
			}
			out["sites"] = c
		}
	}
	// The last warnings and errors: what went wrong lately, newest first.
	recent := s.opts.Bus.Recent()
	trouble := []events.Event{}
	for i := len(recent) - 1; i >= 0 && len(trouble) < 8; i-- {
		if e := recent[i]; e.Level == "warn" || e.Level == "error" {
			trouble = append(trouble, e)
		}
	}
	out["events"] = trouble
	s.mu.Lock()
	parts := make(map[string]Summarizer, len(s.summaries))
	for k, fn := range s.summaries {
		parts[k] = fn
	}
	s.mu.Unlock()
	for k, fn := range parts {
		out[k] = fn()
	}
	return out, nil
}

// NodeCard is one node in the overview.
type NodeCard struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	IP        string         `json:"ip,omitempty"`
	Version   string         `json:"version,omitempty"`
	Self      bool           `json:"self,omitempty"`
	OK        bool           `json:"ok"`
	Error     string         `json:"error,omitempty"`
	LatencyMS int64          `json:"latency_ms,omitempty"`
	Summary   map[string]any `json:"summary,omitempty"`
}

func (s *Server) nodesOverview(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.meshOf()
	if err != nil {
		return nil, err
	}
	self := m.Self()
	members := m.Members()
	cards := make([]NodeCard, len(members)+1)
	mine, _ := s.nodeSummary(w, r)
	cards[0] = NodeCard{ID: self.ID, Name: self.Name, Self: true, OK: true, Summary: mine.(map[string]any)}
	var wg sync.WaitGroup
	for i, x := range members {
		wg.Add(1)
		go func(i int, x nodes.Member) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
			defer cancel()
			c := NodeCard{ID: x.ID, Name: x.Name, IP: x.IP.String(), Version: x.Version}
			start := time.Now()
			var sum map[string]any
			if err := m.Call(ctx, x, "GET", "/node/summary", nil, &sum); err != nil {
				c.Error = err.Error()
			} else {
				c.OK, c.Summary, c.LatencyMS = true, sum, time.Since(start).Milliseconds()
			}
			cards[i+1] = c
		}(i, x)
	}
	wg.Wait()
	return cards, nil
}
