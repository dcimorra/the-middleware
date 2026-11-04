package cluster

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/engine"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/nodes"
	"github.com/dcimorra/the-middleware/internal/proxy"
)

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// house is one installation of the test: its root, its engine, its mesh
// and its cluster, the door serving the cluster's routes as the API does.
type house struct {
	root  string
	eng   *engine.Engine
	mesh  *nodes.Mesh
	cl    *Manager
	load  *Load
	loadM *sync.Mutex
	site  http.Handler // what answers a replica's request here
}

func newHouse(t *testing.T, ctx context.Context, name string, port int) *house {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"sites", "data"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	eng := engine.New(engine.Options{SitesDir: filepath.Join(root, "sites"), DataDir: filepath.Join(root, "data"), Tick: 50 * time.Millisecond})
	go eng.Run(ctx)
	m, err := nodes.Open(nodes.Options{Dir: filepath.Join(root, "data", "nodes"), Hostname: name, Version: "test", Net: nodes.LoopbackNetwork{}, Subnet: netip.MustParsePrefix("127.77.0.0/16"), APIPort: port})
	if err != nil {
		t.Fatal(err)
	}
	h := &house{root: root, eng: eng, mesh: m, load: &Load{PerCore: 0.1, Cores: 2}, loadM: &sync.Mutex{}}
	h.cl = New(Options{Root: root, Sites: eng, Tick: 100 * time.Millisecond, Settle: time.Millisecond, Gap: 200 * time.Millisecond,
		Load: func() Load { h.loadM.Lock(); defer h.loadM.Unlock(); return *h.load }})
	h.cl.SetMesh(m)
	px := proxy.New(eng.Lookup)
	h.site = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Served-By", name)
		w.Header().Set("X-Seen-Client", r.Header.Get("CF-Connecting-IP"))
		px.ServeHTTP(w, r)
	})
	m.ServeSites(h.cl.ReplicaHandler(h.site))
	m.ServeTunnels(h.cl.TunnelHandler())
	m.Serve(h.api())
	go h.cl.Run(ctx)
	return h
}

func (h *house) setLoad(l Load) {
	h.loadM.Lock()
	*h.load = l
	h.loadM.Unlock()
}

// api is the API's cluster routes, as a node calls them.
func (h *house) api() http.Handler {
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, v any, err error) {
		if err != nil {
			code := http.StatusUnprocessableEntity
			if err == ErrTaken || err == ErrBusy {
				code = http.StatusConflict
			}
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("POST /api/v1/cluster/status", func(w http.ResponseWriter, r *http.Request) {
		x, _ := nodes.MemberFrom(r.Context())
		var req StatusRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		reply(w, h.cl.Status(x, req), nil)
	})
	mux.HandleFunc("PUT /api/v1/cluster/replicas/{domain}", func(w http.ResponseWriter, r *http.Request) {
		x, _ := nodes.MemberFrom(r.Context())
		_, err := h.cl.Receive(r.Context(), x, r.PathValue("domain"), r.Body)
		reply(w, map[string]bool{"ok": true}, err)
	})
	mux.HandleFunc("POST /api/v1/cluster/replicas/{domain}/commit", func(w http.ResponseWriter, r *http.Request) {
		x, _ := nodes.MemberFrom(r.Context())
		var req CommitRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		reply(w, map[string]bool{"ok": true}, h.cl.Commit(x, r.PathValue("domain"), req))
	})
	return mux
}

func writeSite(t *testing.T, root, domain string, m map[string]any, files map[string]string) {
	t.Helper()
	dir := filepath.Join(root, "sites", domain)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(dir, manifest.FileName), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, what string, d time.Duration, ok func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func join(t *testing.T, ctx context.Context, a, b *house) {
	t.Helper()
	code, _, err := a.mesh.Invite(ctx, "198.51.100.1:51821")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.mesh.Join(ctx, code, ""); err != nil {
		t.Fatal(err)
	}
	a.mesh.Gossip(ctx)
}

// The whole path, done and not read: a site asks for a replica; the
// primary copies it to the other node, which puts it in place behind its
// closed door; while the primary is comfortable it serves every request,
// and once saturated its requests cross the mesh to the replica, the
// visitor's address with them; a new version reaches the replica and is
// not served from there until it arrived; a site no longer clustered
// loses its replica.
func TestAClusteredSiteIsCopiedAndSharesItsRequestsWhenSaturated(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port := freePort(t)
	a := newHouse(t, ctx, "alpha", port)
	b := newHouse(t, ctx, "beta", port)
	join(t, ctx, a, b)

	man := map[string]any{"version": 2, "static": map[string]any{"root": "."}, "cluster": map[string]any{"replicas": 1}}
	writeSite(t, a.root, "shop.example", man, map[string]string{"index.html": "v1", ".git/HEAD": "ref", "cache/x": "skip"})

	replica := filepath.Join(b.root, "sites", "shop.example")
	defer func() {
		if t.Failed() {
			v, _ := a.cl.View("shop.example")
			t.Logf("members %+v view %+v kept %+v", a.mesh.Members(), v, b.cl.Kept())
		}
	}()
	waitFor(t, "the replica to arrive", 10*time.Second, func() bool {
		sn, ok := b.eng.Site("shop.example")
		return ok && sn.Manifest.IsReplica()
	})
	got, _ := manifest.Load(replica)
	if got.Cluster != nil || got.Replica.Primary != "alpha" || got.Replica.Of != a.mesh.Self().ID {
		t.Fatalf("replica manifest %+v %+v", got.Cluster, got.Replica)
	}
	if _, err := os.Stat(filepath.Join(replica, ".git")); err == nil {
		t.Fatal(".git travelled")
	}
	// The closed door: beta does not publish it, and its own chain
	// answers its name as nobody's.
	if _, ok := b.eng.Hosts()["shop.example"]; ok {
		t.Fatal("the replica is among the names beta publishes")
	}
	rec := httptest.NewRecorder()
	b.cl.Wrap(http.NotFoundHandler()).ServeHTTP(rec, httptest.NewRequest("GET", "http://shop.example/", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("beta's own door served its replica: %d", rec.Code)
	}

	front := a.cl.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Served-By", "alpha")
		proxy.New(a.eng.Lookup).ServeHTTP(w, r)
	}))
	get := func(client string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "http://shop.example/", nil)
		req.RemoteAddr = client + ":5555"
		req.Header.Set("Authorization", "Bearer visitor")
		rec := httptest.NewRecorder()
		front.ServeHTTP(rec, req)
		return rec
	}
	waitFor(t, "the replica to be ready", 10*time.Second, func() bool {
		v, _ := a.cl.View("shop.example")
		return len(v.Replicas) == 1 && v.Replicas[0].State == StateReady && v.Replicas[0].Alive
	})
	// Comfortable: everything here.
	for i := range 20 {
		if r := get("203.0.113." + itoa(i)); r.Header().Get("X-Served-By") != "alpha" || r.Body.String() != "v1" {
			t.Fatalf("comfortable, request %d went to %s", i, r.Header().Get("X-Served-By"))
		}
	}
	// Saturated: some visitors go to beta, each always to the same node.
	a.setLoad(Load{PerCore: 1.5, Cores: 2})
	waitFor(t, "the load to be read", 2*time.Second, func() bool { return a.cl.LocalLoad().PerCore > 1 })
	seen := map[string]int{}
	for i := range 40 {
		ip := "203.0.113." + itoa(i)
		r := get(ip)
		by := r.Header().Get("X-Served-By")
		seen[by]++
		if r.Body.String() != "v1" {
			t.Fatalf("%s answered %q", by, r.Body.String())
		}
		if by == "beta" && r.Header().Get("X-Seen-Client") != ip {
			t.Fatalf("beta saw the visitor as %q, not %s", r.Header().Get("X-Seen-Client"), ip)
		}
		if again := get(ip).Header().Get("X-Served-By"); again != by {
			t.Fatalf("visitor %s went to %s then %s", ip, by, again)
		}
	}
	if seen["beta"] == 0 || seen["alpha"] == 0 {
		t.Fatalf("saturated, the requests went %v", seen)
	}

	// A new version: not served from beta until beta has it.
	if err := os.WriteFile(filepath.Join(a.root, "sites", "shop.example", "index.html"), []byte("v2-longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := range 200 {
		if body := get("203.0.113." + itoa(i%40)).Body.String(); body != "v1" && body != "v2-longer" {
			t.Fatalf("answered %q", body)
		}
		if i > 50 && func() bool { v, _ := a.cl.View("shop.example"); return v.Replicas[0].State == StateReady }() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	waitFor(t, "beta to have v2", 10*time.Second, func() bool {
		raw, _ := os.ReadFile(filepath.Join(replica, "index.html"))
		return string(raw) == "v2-longer"
	})
	for i := range 40 {
		if body := get("203.0.113." + itoa(i)).Body.String(); body != "v2-longer" {
			// Before beta reported the new version, alpha serves; after,
			// either does, and both have it.
			v, _ := a.cl.View("shop.example")
			if v.Replicas[0].State == StateReady {
				t.Fatalf("an old copy answered after the new one was ready: %q", body)
			}
		}
	}

	// Taken out of the cluster: beta drops it once its grace is over.
	delete(man, "cluster")
	raw, _ := json.Marshal(man)
	_ = os.WriteFile(filepath.Join(a.root, "sites", "shop.example", manifest.FileName), raw, 0o644)
	b.cl.mu.Lock()
	for _, j := range b.cl.jobs {
		j.placed = time.Now().Add(-time.Hour)
	}
	b.cl.mu.Unlock()
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(filepath.Join(replica, manifest.FileName), old, old)
	waitFor(t, "the replica to go", 10*time.Second, func() bool {
		_, err := os.Stat(replica)
		return os.IsNotExist(err)
	})
}

func itoa(i int) string {
	return strings.TrimSpace(func() string { b, _ := json.Marshal(i); return string(b) }())
}

// The database gateway, done: a replica's site connects where its
// database is on the primary, and its bytes reach the primary's. Only
// for the address the site names and the node that keeps its replica.
func TestTheGatewayReachesThePrimarysDatabase(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port := freePort(t)
	a := newHouse(t, ctx, "alpha", port)
	b := newHouse(t, ctx, "beta", port)
	join(t, ctx, a, b)

	// The primary's "database": an echo on loopback.
	db, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	go func() {
		for {
			c, err := db.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	dbAddr := db.Addr().String()
	writeSite(t, a.root, "app.example", map[string]any{"version": 2, "static": map[string]any{"root": "."},
		"cluster": map[string]any{"replicas": 1, "gateway": []string{dbAddr}}}, map[string]string{"index.html": "x"})
	waitFor(t, "the replica to be placed", 10*time.Second, func() bool {
		a.cl.mu.Lock()
		defer a.cl.mu.Unlock()
		return len(a.cl.placement["app.example"]) == 1
	})
	alpha, _ := b.mesh.Member(a.mesh.Self().ID)
	tctx, tcancel := context.WithTimeout(ctx, 5*time.Second)
	defer tcancel()
	c, err := b.mesh.Tunnel(tctx, alpha, "app.example", dbAddr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("SELECT 1\n")); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil || line != "SELECT 1\n" {
		t.Fatalf("%q %v", line, err)
	}
	c.Close()
	// Another address of alpha's loopback is not reachable through it.
	if _, err := b.mesh.Tunnel(tctx, alpha, "app.example", "127.0.0.1:22"); err == nil {
		t.Fatal("the gateway opened an address the site does not name")
	}
	// The replica's site, on beta, has its gateway at the same address;
	// on one machine the primary already holds it, which beta reports
	// instead of serving a site without its database.
	waitFor(t, "beta to report its gateway", 10*time.Second, func() bool {
		st := b.cl.Replicas("")["app.example"]
		return strings.Contains(st.Error, "gateway cannot listen") && !st.Alive
	})
}

// The gateway's listener on a replica, with the primary's end faked: what
// the site writes to its database address arrives through the mesh.
func TestAReplicasGatewayListensWhereItsSiteExpectsTheDatabase(t *testing.T) {
	root := t.TempDir()
	addr := "127.0.0.1:" + itoa(freePort(t))
	writeSite(t, root, "app.example", map[string]any{"version": 2, "static": map[string]any{"root": "."},
		"replica": map[string]any{"of": "primary-id", "revision": "r1", "gateway": []string{addr}}}, nil)
	m := New(Options{Root: root, Sites: fakeSites{}})
	fm := &fakeMesh{}
	m.reconcileGateways(fm)
	defer m.closeGateways()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = c.Write([]byte("ping\n"))
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil || line != "ping\n" {
		t.Fatalf("%q %v", line, err)
	}
	fm.mu.Lock()
	defer fm.mu.Unlock()
	if fm.site != "app.example" || fm.addr != addr || fm.to != "primary-id" {
		t.Fatalf("tunnel asked for %+v", fm)
	}
}

type fakeSites struct{ sn []engine.Snapshot }

func (f fakeSites) Site(d string) (engine.Snapshot, bool) {
	for _, s := range f.sn {
		if s.Domain == d {
			return s, true
		}
	}
	return engine.Snapshot{}, false
}
func (f fakeSites) Sites() []engine.Snapshot { return f.sn }
func (f fakeSites) Owner(h string) (string, bool) {
	_, ok := f.Site(h)
	return h, ok
}

type fakeMesh struct {
	nodes.Mesh
	mu             sync.Mutex
	site, addr, to string
}

func (f *fakeMesh) Member(id string) (nodes.Member, bool) { return nodes.Member{ID: id}, true }
func (f *fakeMesh) Tunnel(_ context.Context, x nodes.Member, site, addr string) (net.Conn, error) {
	f.mu.Lock()
	f.site, f.addr, f.to = site, addr, x.ID
	f.mu.Unlock()
	a, b := net.Pipe()
	go func() { defer b.Close(); _, _ = io.Copy(b, b) }()
	return a, nil
}

// Pick, alone: comfortable is here; a replica behind the primary's
// revision, or silent, is never chosen; a primary whose own instance is
// down sends everything to the replicas.
func TestPickServesHereUnlessSaturatedOrDown(t *testing.T) {
	m := New(Options{Root: t.TempDir(), Sites: fakeSites{}})
	now := time.Now()
	m.now = func() time.Time { return now }
	man := &manifest.Manifest{Version: 2, Run: &manifest.Run{Command: []string{"x"}}, Cluster: &manifest.Cluster{Replicas: 1}}
	sn := engine.Snapshot{Domain: "s.example", Mode: manifest.ModeRun, Manifest: man, Alive: true, Port: 1}
	m.prims["s.example"] = &primary{fingerprint: "f2"}
	m.placement["s.example"] = []string{"b"}
	m.views["b"] = &nodeView{member: nodes.Member{ID: "b", Name: "beta"}, at: now,
		status: Status{Load: Load{PerCore: 0.1, Cores: 4}, Replicas: map[string]ReplicaStatus{"s.example": {Revision: "f2", Alive: true}}}}
	m.local = Load{PerCore: 0.3, Cores: 4}
	if _, ok := m.Pick("s.example", sn, "1.2.3.4"); ok {
		t.Fatal("comfortable, a request left")
	}
	m.local.PerCore = 2
	left := 0
	for i := range 100 {
		if _, ok := m.Pick("s.example", sn, "10.0.0."+itoa(i)); ok {
			left++
		}
	}
	if left == 0 || left == 100 {
		t.Fatalf("saturated, %d of 100 left", left)
	}
	m.views["b"].status.Replicas["s.example"] = ReplicaStatus{Revision: "f1", Alive: true}
	for i := range 50 {
		if _, ok := m.Pick("s.example", sn, "10.0.0."+itoa(i)); ok {
			t.Fatal("a replica behind the primary was chosen")
		}
	}
	m.views["b"].status.Replicas["s.example"] = ReplicaStatus{Revision: "f2", Alive: true}
	m.views["b"].at = now.Add(-time.Hour)
	if _, ok := m.Pick("s.example", sn, "10.0.0.1"); ok {
		t.Fatal("a silent node was chosen")
	}
	m.views["b"].at = now
	m.local.PerCore = 0.1
	sn.Alive = false
	for i := range 20 {
		if _, ok := m.Pick("s.example", sn, "10.0.0."+itoa(i)); !ok {
			t.Fatal("the primary's instance is down and a request stayed")
		}
	}
}

func TestBlockersRefuseWhatACopyWouldFork(t *testing.T) {
	cases := map[string]string{
		`{"version":2,"run":{"command":["x"]},"cluster":{},"backups":{"db":{"engine":"sqlite","file":"db.sqlite"}}}`:        "SQLite",
		`{"version":2,"run":{"command":["x"]},"cluster":{},"workers":{"db":{"command":["mariadbd"]}}}`:                      "workers",
		`{"version":2,"run":{"command":["x"],"runtime":"docker","docker":{"image":"a:1","volumes":["d:/d"]}},"cluster":{}}`: "volumes",
		`{"version":2,"run":{"command":["x"]},"cluster":{"gateway":["10.0.0.5:3306"]}}`:                                     "loopback",
	}
	for raw, want := range cases {
		if _, err := manifest.Parse([]byte(raw)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", raw, err)
		}
	}
	m, err := manifest.Parse([]byte(`{"version":2,"run":{"command":["x"]},"cluster":{"replicas":2,"gateway":["3306"]},"backups":{"db":{"engine":"mysql"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := manifest.GatewayAddr(m.Cluster.Gateway[0]); a != "127.0.0.1:3306" {
		t.Fatal(a)
	}
}

// A copy whose lockfiles did not change keeps the node_modules the
// running one has, linked and not installed again.
func TestACopyCarriesItsDependenciesWhenTheLockfileDidNotChange(t *testing.T) {
	root := t.TempDir()
	m := New(Options{Root: root, Sites: fakeSites{}})
	installs := 0
	m.SetInstall(func(ctx context.Context, dir string) (string, error) {
		installs++
		return "", os.MkdirAll(filepath.Join(dir, "node_modules", "fresh"), 0o755)
	})
	from := nodes.Member{ID: "p", Name: "alpha"}
	writeSite(t, root, "n.example", map[string]any{"version": 2, "static": map[string]any{}, "replica": map[string]any{"of": "p", "revision": "r1"}},
		map[string]string{"package.json": `{"dependencies":{"a":"1"}}`, "node_modules/a/index.js": "old"})
	put := func(pkg string) {
		t.Helper()
		in := filepath.Join(m.incoming("n.example"), "site")
		_ = os.RemoveAll(m.incoming("n.example"))
		_ = os.MkdirAll(in, 0o755)
		_ = os.WriteFile(filepath.Join(in, "package.json"), []byte(pkg), 0o644)
		raw := `{"version":2,"static":{},"cluster":{}}`
		if err := m.Commit(from, "n.example", CommitRequest{Manifest: b64(raw), Revision: "r2", Dirs: []string{"."}}); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the commit", 5*time.Second, func() bool {
			m.mu.Lock()
			defer m.mu.Unlock()
			return !m.jobs["n.example"].running
		})
		if e := m.jobs["n.example"].err; e != "" {
			t.Fatal(e)
		}
	}
	put(`{"dependencies":{"a":"1"}}`)
	if installs != 0 {
		t.Fatal("installed although the lockfiles are the same")
	}
	if raw, _ := os.ReadFile(filepath.Join(root, "sites", "n.example", "node_modules", "a", "index.js")); string(raw) != "old" {
		t.Fatal("node_modules did not come over")
	}
	put(`{"dependencies":{"a":"2"}}`)
	if installs != 1 {
		t.Fatal("not installed after package.json changed")
	}
	// Another primary's name is refused.
	if err := m.mayKeep(nodes.Member{ID: "q"}, "n.example"); err != ErrTaken {
		t.Fatal(err)
	}
}

func b64(s string) string {
	return base64Std(s)
}

func base64Std(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
