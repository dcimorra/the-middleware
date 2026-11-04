// Package cluster keeps copies of a site on other nodes of the network and
// hands them part of its traffic.
//
// The node a site's names point at is its primary; it stays the only way
// in. A site whose manifest asks for a cluster gets replicas on other
// nodes, chosen by name or by load, which receive its folder through the
// mesh at every change and install its dependencies for their own
// machine. A replica is not published and has no door of its own: it
// answers only what its primary hands it through the mesh (the closed
// door). The primary serves everything itself while it is comfortable,
// and when its load per core passes the site's spill, or its own instance
// is down, it shares the requests out by the free capacity of each node,
// the same visitor to the same node.
//
// What is copied is code, not data: a site whose state lives in its
// folder or a volume cannot be clustered (manifest.Blockers).
package cluster

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/archive"
	"github.com/dcimorra/the-middleware/internal/engine"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/nodes"
	"github.com/dcimorra/the-middleware/internal/relocate"
)

// Mesh is what the cluster needs of the network (*nodes.Mesh).
type Mesh interface {
	Self() nodes.Self
	InNetwork() bool
	Members() []nodes.Member
	Call(ctx context.Context, x nodes.Member, method, path string, body, out any) error
	Stream(ctx context.Context, x nodes.Member, method, path, contentType string, body io.Reader, out any) error
	SiteProxy(x nodes.Member, who func(in *http.Request, out http.Header), fail func(w http.ResponseWriter, r *http.Request, err error)) *httputil.ReverseProxy
	Member(id string) (nodes.Member, bool)
	Tunnel(ctx context.Context, x nodes.Member, site, addr string) (net.Conn, error)
}

// Sites is what the cluster needs of the engine (*engine.Engine).
type Sites interface {
	Site(domain string) (engine.Snapshot, bool)
	Sites() []engine.Snapshot
	Owner(host string) (string, bool)
}

// Load is a node's load as the cluster weighs it.
type Load struct {
	// PerCore is the one-minute load average over the cores: 1 is every
	// core busy.
	PerCore float64 `json:"per_core"`
	Cores   int     `json:"cores"`
}

// Replica states, as the node that keeps it reports them.
const (
	StateReady   = "ready"
	StateSyncing = "syncing"
	StateFailed  = "failed"
)

// ReplicaStatus is one replica on a node.
type ReplicaStatus struct {
	Of string `json:"of"`
	// Revision is the primary's fingerprint the replica serves now.
	Revision string `json:"revision"`
	Alive    bool   `json:"alive"`
	State    string `json:"state"`
	// Pending is the fingerprint being installed, Error why the last
	// one was not.
	Pending string    `json:"pending,omitempty"`
	Error   string    `json:"error,omitempty"`
	Synced  time.Time `json:"synced,omitzero"`
}

// Status is a node's answer to its primaries' round.
type Status struct {
	Load     Load                     `json:"load"`
	Replicas map[string]ReplicaStatus `json:"replicas"`
}

// StatusRequest is a primary's round: the sites it keeps on the node it
// asks. The node removes the replicas of that primary not named.
type StatusRequest struct {
	Expect []string `json:"expect"`
}

// CommitRequest puts a received copy in place.
type CommitRequest struct {
	Manifest string `json:"manifest"` // base64, the primary's site.json
	Revision string `json:"revision"`
	Primary  string `json:"primary"`
	// Dirs are the site's folders whose dependencies were left behind,
	// relative to it, for this node to install.
	Dirs []string `json:"dirs"`
}

// Options configure a Manager.
type Options struct {
	Root  string
	Sites Sites
	Bus   *events.Bus
	// Install installs a folder's dependencies for this machine.
	Install func(ctx context.Context, dir string) (string, error)
	// Load reads this node's load; nil reads /proc/loadavg.
	Load func() Load
	// Tick is a round's period (default 5 s); Settle is how long after
	// the start nothing is decided, while the engine reads the sites
	// (default 30 s).
	Tick   time.Duration
	Settle time.Duration
	// Gap is the least time between two copies of a site to one node,
	// and a failed copy's wait (default retryAfter).
	Gap time.Duration
}

// Manager is this node's side of every cluster: primary of some sites,
// keeper of others' replicas.
type Manager struct {
	opts Options
	now  func() time.Time

	mu      sync.Mutex
	mesh    Mesh
	started time.Time
	local   Load
	// The primary's view.
	views     map[string]*nodeView // member id
	prims     map[string]*primary  // domain
	placement map[string][]string  // domain → member ids, data/cluster/placement.json
	over      map[string]bool      // domain → sharing because of load
	// The keeper's.
	jobs    map[string]*job // domain
	orphans map[string]time.Time
	gws     map[string]*gw    // address listened at
	gwErrs  map[string]string // domain → why its gateway is not there
}

type nodeView struct {
	member nodes.Member
	status Status
	at     time.Time
	err    string
	down   time.Time
}

type primary struct {
	fingerprint string
	fpErr       string
	syncing     map[string]bool
	tried       map[string]time.Time
	errs        map[string]string
	refused     map[string]string
	synced      map[string]time.Time
}

type job struct {
	revision string
	running  bool
	err      string
	placed   time.Time
}

// New builds a manager; it does nothing until Run and SetMesh.
func New(opts Options) *Manager {
	if opts.Tick == 0 {
		opts.Tick = 5 * time.Second
	}
	if opts.Settle == 0 {
		opts.Settle = 30 * time.Second
	}
	if opts.Gap == 0 {
		opts.Gap = retryAfter
	}
	if opts.Load == nil {
		opts.Load = ReadLoad
	}
	if opts.Bus == nil {
		opts.Bus = events.New("")
	}
	m := &Manager{opts: opts, now: time.Now,
		views: map[string]*nodeView{}, prims: map[string]*primary{}, placement: map[string][]string{},
		over: map[string]bool{}, jobs: map[string]*job{}, orphans: map[string]time.Time{}}
	_ = fsx.ReadJSON(m.placementPath(), &m.placement)
	if m.placement == nil {
		m.placement = map[string][]string{}
	}
	return m
}

// SetMesh plugs the network in; the mesh is built after the hooks.
func (m *Manager) SetMesh(x Mesh) {
	m.mu.Lock()
	m.mesh = x
	m.mu.Unlock()
}

func (m *Manager) getMesh() Mesh {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mesh
}

func (m *Manager) dir() string { return filepath.Join(m.opts.Root, "data", "cluster") }
func (m *Manager) placementPath() string {
	return filepath.Join(m.dir(), "placement.json")
}
func (m *Manager) siteDir(domain string) string {
	return filepath.Join(m.opts.Root, "sites", domain)
}

func (m *Manager) emit(typ, level, domain, msg string, data map[string]any) {
	m.opts.Bus.Emit(events.Event{Type: typ, Level: level, Subject: domain, Actor: "cluster", Message: msg, Data: data})
}

// ReadLoad is the machine's one-minute load over its cores.
func ReadLoad() Load {
	cores := runtime.NumCPU()
	l := Load{Cores: cores}
	raw, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return l
	}
	f := strings.Fields(string(raw))
	if len(f) == 0 {
		return l
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err == nil && cores > 0 {
		l.PerCore = v / float64(cores)
	}
	return l
}

// Run is the round, for the life of the process.
func (m *Manager) Run(ctx context.Context) {
	m.mu.Lock()
	m.started = m.now()
	m.mu.Unlock()
	t := time.NewTicker(m.opts.Tick)
	defer t.Stop()
	for {
		m.Round(ctx)
		select {
		case <-ctx.Done():
			m.closeGateways()
			return
		case <-t.C:
		}
	}
}

// Round is one pass: this node's load, the other nodes' state, where each
// clustered site has its replicas, and the copies that are behind.
func (m *Manager) Round(ctx context.Context) {
	l := m.opts.Load()
	m.mu.Lock()
	m.local = l
	settled := !m.started.IsZero() && m.now().Sub(m.started) >= m.opts.Settle
	m.mu.Unlock()
	mesh := m.getMesh()
	if mesh == nil || !mesh.InNetwork() {
		m.closeGateways()
		return
	}
	// Before settling: a replica that restarts with the house needs its
	// database at once.
	m.reconcileGateways(mesh)
	if !settled {
		return
	}
	members := mesh.Members()
	m.sweepOrphans(members)
	clustered := m.clustered()
	m.fingerprints(clustered)
	m.poll(ctx, mesh, members)
	m.place(clustered, members)
	m.syncAll(ctx, mesh, clustered)
}

// clustered are this node's sites that ask for replicas.
func (m *Manager) clustered() map[string]engine.Snapshot {
	out := map[string]engine.Snapshot{}
	for _, sn := range m.opts.Sites.Sites() {
		if sn.Manifest.Clustered() && len(sn.Manifest.Blockers()) == 0 {
			out[sn.Domain] = sn
		}
	}
	return out
}

func (m *Manager) prim(domain string) *primary {
	p := m.prims[domain]
	if p == nil {
		p = &primary{syncing: map[string]bool{}, tried: map[string]time.Time{}, errs: map[string]string{}, refused: map[string]string{}, synced: map[string]time.Time{}}
		m.prims[domain] = p
	}
	return p
}

func (m *Manager) fingerprints(clustered map[string]engine.Snapshot) {
	for domain, sn := range clustered {
		fp, err := Fingerprint(m.siteDir(domain), sn.Manifest)
		m.mu.Lock()
		p := m.prim(domain)
		if err != nil {
			p.fpErr = err.Error()
		} else {
			p.fingerprint, p.fpErr = fp, ""
		}
		m.mu.Unlock()
	}
	m.mu.Lock()
	for domain := range m.prims {
		if _, ok := clustered[domain]; !ok {
			delete(m.prims, domain)
			delete(m.over, domain)
		}
	}
	m.mu.Unlock()
}

// poll asks every live node for its load and its replicas, telling each
// which of this node's sites it should keep.
func (m *Manager) poll(ctx context.Context, mesh Mesh, members []nodes.Member) {
	var wg sync.WaitGroup
	for _, x := range members {
		m.mu.Lock()
		var expect []string
		for domain, ids := range m.placement {
			for _, id := range ids {
				if id == x.ID {
					expect = append(expect, domain)
				}
			}
		}
		m.mu.Unlock()
		sort.Strings(expect)
		if expect == nil {
			expect = []string{}
		}
		wg.Add(1)
		go func(x nodes.Member, expect []string) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, m.opts.Tick)
			defer cancel()
			var st Status
			err := mesh.Call(cctx, x, http.MethodPost, "/cluster/status", StatusRequest{Expect: expect}, &st)
			m.mu.Lock()
			defer m.mu.Unlock()
			v := m.views[x.ID]
			if v == nil {
				v = &nodeView{}
				m.views[x.ID] = v
			}
			v.member = x
			if err != nil {
				v.err = err.Error()
				return
			}
			v.status, v.at, v.err = st, m.now(), ""
		}(x, expect)
	}
	wg.Wait()
	live := map[string]bool{}
	for _, x := range members {
		live[x.ID] = true
	}
	m.mu.Lock()
	for id := range m.views {
		if !live[id] {
			delete(m.views, id)
		}
	}
	m.mu.Unlock()
}

// fresh says a node answered recently. Called under m.mu.
func (m *Manager) freshLocked(v *nodeView) bool {
	return v != nil && !v.at.IsZero() && m.now().Sub(v.at) <= 3*m.opts.Tick
}

// place decides where each clustered site keeps its replicas: the nodes
// it names, then the ones it already has, then the least loaded of the
// rest. A choice stays until its node leaves or refuses, so a replica is
// not copied again elsewhere because another node is a little quieter.
func (m *Manager) place(clustered map[string]engine.Snapshot, members []nodes.Member) {
	m.mu.Lock()
	defer m.mu.Unlock()
	byID := map[string]nodes.Member{}
	for _, x := range members {
		byID[x.ID] = x
	}
	changed := false
	next := map[string][]string{}
	for domain, sn := range clustered {
		c := sn.Manifest.Cluster
		p := m.prim(domain)
		want := c.Want()
		var out []string
		has := map[string]bool{}
		add := func(id string) {
			if !has[id] && len(out) < want {
				has[id] = true
				out = append(out, id)
			}
		}
		for _, n := range c.Nodes {
			for _, x := range members {
				if x.ID == n || strings.EqualFold(x.Name, n) {
					add(x.ID)
				}
			}
		}
		for _, id := range m.placement[domain] {
			if _, live := byID[id]; live && p.refused[id] == "" {
				add(id)
			}
		}
		var rest []nodes.Member
		for _, x := range members {
			if v := m.views[x.ID]; !has[x.ID] && p.refused[x.ID] == "" && m.freshLocked(v) {
				rest = append(rest, x)
			}
		}
		sort.SliceStable(rest, func(i, j int) bool {
			return m.views[rest[i].ID].status.Load.PerCore < m.views[rest[j].ID].status.Load.PerCore
		})
		for _, x := range rest {
			add(x.ID)
		}
		if strings.Join(out, ",") != strings.Join(m.placement[domain], ",") {
			changed = true
			var names []string
			for _, id := range out {
				names = append(names, byID[id].Name)
			}
			m.emit("cluster.placed", "", domain, fmt.Sprintf("replicas on %s", orNone(names)), map[string]any{"nodes": names})
		}
		if len(out) > 0 {
			next[domain] = out
		}
	}
	if len(next) != len(m.placement) {
		changed = true
	}
	m.placement = next
	if changed {
		_ = fsx.WriteJSON(m.placementPath(), m.placement, 0o644)
	}
}

func orNone(list []string) string {
	if len(list) == 0 {
		return "no node (none is free to take it)"
	}
	return strings.Join(list, ", ")
}

// retryAfter is how long a failed copy waits before it is tried again,
// and the least time between two copies of one site to one node: a site
// that writes its folder as it runs would otherwise be copied without
// pause (move.exclude is the cure, and the panel says so).
const retryAfter = time.Minute

func (m *Manager) syncAll(ctx context.Context, mesh Mesh, clustered map[string]engine.Snapshot) {
	m.mu.Lock()
	type todo struct {
		domain, fp string
		x          nodes.Member
	}
	var list []todo
	for domain, ids := range m.placement {
		p := m.prims[domain]
		if p == nil || p.fingerprint == "" {
			continue
		}
		for _, id := range ids {
			v := m.views[id]
			if !m.freshLocked(v) || p.syncing[id] {
				continue
			}
			rs, has := v.status.Replicas[domain]
			if has && rs.Revision == p.fingerprint {
				continue
			}
			if has && rs.State == StateSyncing && rs.Pending == p.fingerprint {
				continue
			}
			if t := p.tried[id]; !t.IsZero() && m.now().Sub(t) < m.opts.Gap {
				continue
			}
			p.syncing[id], p.tried[id] = true, m.now()
			list = append(list, todo{domain, p.fingerprint, v.member})
		}
	}
	m.mu.Unlock()
	for _, t := range list {
		sn := clustered[t.domain]
		go func() {
			err := m.send(ctx, mesh, t.x, t.domain, t.fp, sn.Manifest)
			m.mu.Lock()
			defer m.mu.Unlock()
			p := m.prim(t.domain)
			delete(p.syncing, t.x.ID)
			if err == nil {
				delete(p.errs, t.x.ID)
				p.synced[t.x.ID] = m.now()
				return
			}
			p.errs[t.x.ID] = err.Error()
			var re *nodes.RemoteError
			if errors.As(err, &re) && re.Status == http.StatusConflict {
				// The name is another site's there, or another
				// primary's replica: that node is not used for this
				// site, and the next round places it elsewhere.
				p.refused[t.x.ID] = err.Error()
			}
			m.emit("cluster.sync_failed", "warn", t.domain, fmt.Sprintf("the copy to %s failed: %v", t.x.Name, err), map[string]any{"node": t.x.Name})
		}()
	}
}

// send streams the site to x and asks it to put the copy in place, which
// it does on its own (installing takes minutes): the next rounds see it
// syncing, then ready at the new revision.
func (m *Manager) send(ctx context.Context, mesh Mesh, x nodes.Member, domain, fp string, man *manifest.Manifest) error {
	dir := m.siteDir(domain)
	raw, err := os.ReadFile(filepath.Join(dir, manifest.FileName))
	if err != nil {
		return err
	}
	ps := parts(dir, man)
	pr, pw := io.Pipe()
	go func() {
		_, err := archive.PackParts(ctx, pw, ps)
		pw.CloseWithError(err)
	}()
	path := "/cluster/replicas/" + domain + "?revision=" + fp
	if err := mesh.Stream(ctx, x, http.MethodPut, path, "application/gzip", pr, nil); err != nil {
		pr.CloseWithError(err)
		return err
	}
	req := CommitRequest{
		Manifest: base64.StdEncoding.EncodeToString(raw),
		Revision: fp,
		Primary:  mesh.Self().Name,
		Dirs:     relocate.LeftBehind(dir, ps[0].Skip),
	}
	return mesh.Call(ctx, x, http.MethodPost, "/cluster/replicas/"+domain+"/commit", req, nil)
}

// SetInstall sets how a copy's dependencies are installed here (the API's
// install, the one a move uses).
func (m *Manager) SetInstall(f func(ctx context.Context, dir string) (string, error)) {
	m.mu.Lock()
	m.opts.Install = f
	m.mu.Unlock()
}
