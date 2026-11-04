package api

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/auth"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/nodes"
	"github.com/dcimorra/the-middleware/internal/sites"
)

// A delegate signs in at one node and works on sites that may live on
// another. Everything they ask goes to this node's own /api/v1, with no
// node in the path: when the site is not here, the request is relayed
// through the mesh to the node that holds it, with who they are and what
// they may do there in nodes.DelegateHeader, and that node checks it
// again before answering. The answer comes back as it is; what would
// name the other node (its refusal of this one, its address when it does
// not answer, the events about nodes and moves) is said without it. The
// operator's own panel works on another node through /nodes/{id}/api,
// which no grant opens.

// delegation is what travels in nodes.DelegateHeader: one site, because
// a relayed request is about one.
type delegation struct {
	User  string   `json:"user"`
	Site  string   `json:"site"`
	Perms []string `json:"perms"`
	// Agents are the user's limits on agents (auth.CheckAgents), held
	// where the site's turns run; nil for none. Never omitempty: an
	// empty map is no agent at all and must not arrive as none.
	Agents map[string][]string `json:"agents"`
}

func encodeDelegation(c auth.Caller, site string) string {
	raw, _ := json.Marshal(delegation{User: c.User, Site: site, Perms: c.Grants[site], Agents: c.Agents})
	return base64.RawURLEncoding.EncodeToString(raw)
}

// delegateOf reads the header of a request that came through the mesh
// door. A header that does not decode is a delegate with nothing granted,
// never the node's full access: a node that meant to pass a user on and
// got it wrong must not hand the user the house.
func delegateOf(r *http.Request) (delegation, bool) {
	v := r.Header.Get(nodes.DelegateHeader)
	if v == "" {
		return delegation{}, false
	}
	var d delegation
	raw, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil || json.Unmarshal(raw, &d) != nil || d.User == "" {
		return delegation{User: "?"}, true
	}
	return d, true
}

// gateDelegate decides a delegate's request before the mux sees it: true
// when it may go on. A site that is not theirs is not there (404), so a
// delegate cannot learn which other sites exist by asking.
func (s *Server) gateDelegate(w http.ResponseWriter, r *http.Request, c auth.Caller, pattern string) bool {
	site := siteIn(pattern, r.URL.Path)
	if auth.UserAllows(c.Grants, pattern, site) {
		return true
	}
	if _, granted := c.Grants[site]; site != "" && granted {
		s.writeError(w, r, Errorf(http.StatusForbidden, "your access to %s does not include this", site))
		return false
	}
	if site != "" {
		s.writeError(w, r, Errorf(http.StatusNotFound, "no such site"))
		return false
	}
	s.writeError(w, r, Errorf(http.StatusNotFound, "no such route"))
	return false
}

// relaySite is the site of a delegate's request that another node may
// have to answer: the {domain} of a site route, or the subject of the
// events asked about one site. "" for the rest.
func relaySite(c auth.Caller, pattern string, r *http.Request) string {
	if site := siteIn(pattern, r.URL.Path); site != "" {
		return site
	}
	if pattern == "GET "+Prefix+"/events" || pattern == "GET "+Prefix+"/events/stream" {
		if sub := r.URL.Query().Get("subject"); sub != "" {
			if _, ok := c.Grants[sub]; ok {
				return sub
			}
		}
	}
	return ""
}

// localSite says whether the site's folder is on this node.
func (s *Server) localSite(domain string) bool {
	if s.opts.Root == "" || domain == "" {
		return false
	}
	return fsx.Exists(filepath.Join(s.opts.Root, "sites", domain))
}

// located is where a site was found, kept for a minute: a delegate's
// panel asks a dozen routes of a site on every screen, and asking every
// node every time would be a dozen rounds through the mesh.
type located struct {
	id    string
	until time.Time
}

type locator struct {
	mu   sync.Mutex
	byID map[string]located
}

const locateFor = time.Minute

// locate finds the node that holds domain: the one it was last found at,
// or the first of the live nodes that says it has it.
func (s *Server) locate(ctx context.Context, m *nodes.Mesh, domain string) (nodes.Member, bool) {
	s.loc.mu.Lock()
	if l, ok := s.loc.byID[domain]; ok && time.Now().Before(l.until) {
		s.loc.mu.Unlock()
		if x, ok := m.Member(l.id); ok {
			return x, true
		}
	} else {
		s.loc.mu.Unlock()
	}
	members := m.Members()
	if len(members) == 0 {
		return nodes.Member{}, false
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	found := make(chan nodes.Member, len(members))
	var wg sync.WaitGroup
	for _, x := range members {
		wg.Add(1)
		go func(x nodes.Member) {
			defer wg.Done()
			// A replica (internal/cluster) has the name too, and is not
			// where the site is: its folder is replaced at every copy.
			var got struct {
				Manifest *manifest.Manifest `json:"manifest"`
			}
			if err := m.Call(ctx, x, "GET", "/sites/"+domain, nil, &got); err == nil && !got.Manifest.IsReplica() {
				found <- x
			}
		}(x)
	}
	go func() { wg.Wait(); close(found) }()
	x, ok := <-found
	if !ok {
		return nodes.Member{}, false
	}
	s.loc.mu.Lock()
	if s.loc.byID == nil {
		s.loc.byID = map[string]located{}
	}
	s.loc.byID[domain] = located{id: x.ID, until: time.Now().Add(locateFor)}
	s.loc.mu.Unlock()
	return x, true
}

func (s *Server) forgetLocation(domain string) {
	s.loc.mu.Lock()
	delete(s.loc.byID, domain)
	s.loc.mu.Unlock()
}

// errUnreachable is what a delegate reads when the node that holds their
// site does not answer: the site is unreachable, not "node C at 10.x".
const errUnreachable = "this site cannot be reached right now; try again in a moment"

// relay answers a delegate's request for site from the node that holds
// it. false when no node does, and the request is answered here (with
// the 404 the handler gives a site that is not anywhere).
func (s *Server) relay(w http.ResponseWriter, r *http.Request, c auth.Caller, site string) bool {
	m, err := s.meshOf()
	if err != nil || !m.InNetwork() {
		return false
	}
	x, ok := s.locate(r.Context(), m, site)
	if !ok {
		return false
	}
	rp := m.Proxy(x, strings.TrimPrefix(r.URL.Path, Prefix+"/"))
	rewrite := rp.Rewrite
	who := encodeDelegation(c, site)
	rp.Rewrite = func(pr *httputil.ProxyRequest) {
		rewrite(pr)
		pr.Out.Header.Set(nodes.DelegateHeader, who)
	}
	rp.ModifyResponse = func(res *http.Response) error {
		res.Header.Del("Set-Cookie")
		switch res.StatusCode {
		case http.StatusNotFound:
			// It may have moved; the next request looks again.
			s.forgetLocation(site)
		case http.StatusUnauthorized:
			// That node no longer takes this one: a delegate's 401 would
			// sign them out of a session that is fine.
			res.Body.Close()
			replaceBody(res, http.StatusBadGateway, errUnreachable)
		}
		return nil
	}
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		if errors.Is(err, context.Canceled) {
			return
		}
		s.forgetLocation(site)
		WriteJSON(w, http.StatusBadGateway, map[string]string{"error": errUnreachable})
	}
	rp.ServeHTTP(w, r)
	return true
}

func replaceBody(res *http.Response, status int, msg string) {
	body, _ := json.Marshal(map[string]string{"error": msg})
	body = append(body, '\n')
	res.StatusCode, res.Status = status, http.StatusText(status)
	res.Header = http.Header{"Content-Type": {"application/json; charset=utf-8"}}
	res.Body = io.NopCloser(strings.NewReader(string(body)))
	res.ContentLength = int64(len(body))
}

// delegateSites is GET /sites for a delegate: their sites, from this
// node and from the nodes that hold the others, as one list. A site no
// node answers for now is listed by name with unreachable set, so it
// does not look deleted.
func (s *Server) delegateSites(ctx context.Context, c auth.Caller) ([]any, error) {
	out := []any{}
	var remote []string
	for site := range c.Grants {
		if !s.localSite(site) {
			remote = append(remote, site)
			continue
		}
		if s.opts.Sites == nil {
			continue
		}
		got, err := s.opts.Sites.Get(site)
		if err != nil {
			continue
		}
		out = append(out, got)
	}
	m, err := s.meshOf()
	if len(remote) > 0 && err == nil && m.InNetwork() {
		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, site := range remote {
			wg.Add(1)
			go func(site string) {
				defer wg.Done()
				var got sites.Site
				x, ok := s.locate(ctx, m, site)
				if ok {
					cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
					err := m.Call(cctx, x, "GET", "/sites/"+site, nil, &got)
					cancel()
					ok = err == nil
				}
				mu.Lock()
				defer mu.Unlock()
				if ok {
					out = append(out, got)
				} else {
					out = append(out, map[string]any{"domain": site, "managed": true, "unreachable": true})
				}
			}(site)
		}
		wg.Wait()
	}
	sort.Slice(out, func(i, j int) bool { return domainOfAny(out[i]) < domainOfAny(out[j]) })
	return out, nil
}

func domainOfAny(v any) string {
	switch x := v.(type) {
	case sites.Site:
		return x.Domain
	case map[string]any:
		d, _ := x["domain"].(string)
		return d
	}
	return ""
}

// hiddenFromDelegates are the event types a delegate is not shown even
// about their own site: where it lives and how it got there (moves,
// nodes, the network), the operator's DNS and firewall, and the house's
// own machinery.
var hiddenFromDelegates = []string{"move.", "moves.", "relocate.", "repair.", "node.", "nodes.", "mesh.", "publish.", "dns.", "firewall.", "api.", "auth.", "update.", "module.", "settings.", "middleware.", "panel."}

func delegateSees(c auth.Caller, e events.Event) bool {
	site, _, _ := strings.Cut(e.Subject, "#")
	if _, ok := c.Grants[site]; !ok || site == "" {
		return false
	}
	for _, p := range hiddenFromDelegates {
		if strings.HasPrefix(e.Type, p) {
			return false
		}
	}
	return true
}

// remoteSites are the delegate's sites that are not on this node, which
// the events of the other nodes are read for.
func (s *Server) remoteSites(c auth.Caller) []string {
	var out []string
	for site := range c.Grants {
		if !s.localSite(site) {
			out = append(out, site)
		}
	}
	sort.Strings(out)
	return out
}

// remoteEvents is GET /events for a delegate's sites on other nodes,
// filtered here as the local ones are.
func (s *Server) remoteEvents(ctx context.Context, c auth.Caller, f eventFilter) []events.Event {
	m, err := s.meshOf()
	if err != nil || !m.InNetwork() {
		return nil
	}
	var out []events.Event
	for _, site := range s.remoteSites(c) {
		x, ok := s.locate(ctx, m, site)
		if !ok {
			continue
		}
		q := "?subject=" + site
		if !f.since.IsZero() {
			q += "&since=" + f.since.UTC().Format(time.RFC3339Nano)
		}
		var list []events.Event
		cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
		if err := m.Call(cctx, x, "GET", "/events"+q, nil, &list); err == nil {
			out = append(out, list...)
		}
		cancel()
	}
	return out
}

// remoteStream feeds ch with the live events of the delegate's sites on
// other nodes until ctx ends: one stream from each, reopened when it
// drops, filtered here.
func (s *Server) remoteStream(ctx context.Context, c auth.Caller) <-chan events.Event {
	m, err := s.meshOf()
	if err != nil || !m.InNetwork() {
		return nil
	}
	remote := s.remoteSites(c)
	if len(remote) == 0 {
		return nil
	}
	ch := make(chan events.Event, 64)
	for _, site := range remote {
		go func(site string) {
			for ctx.Err() == nil {
				if x, ok := s.locate(ctx, m, site); ok {
					s.readStream(ctx, m, x, site, c, ch)
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Second):
				}
			}
		}(site)
	}
	return ch
}

func (s *Server) readStream(ctx context.Context, m *nodes.Mesh, x nodes.Member, site string, c auth.Caller, ch chan<- events.Event) {
	res, err := m.Open(ctx, x, "/events/stream?subject="+site, nil)
	if err != nil {
		return
	}
	defer res.Body.Close()
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var e events.Event
		if json.Unmarshal([]byte(data), &e) != nil || !delegateSees(c, e) {
			continue
		}
		select {
		case ch <- e:
		case <-ctx.Done():
			return
		}
	}
}
