package sitegroups

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/cgroups"
	"github.com/dcimorra/the-middleware/internal/engine"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/ports"
	"github.com/dcimorra/the-middleware/internal/runtime"
)

// TestMain lets the test binary be the site under supervision (as the
// engine's tests do): MW_TEST_SERVER answers its BUILD on /, holds
// MW_TEST_HOLD megabytes from before it listens, and /eat takes memory
// until the kernel stops it.
func TestMain(m *testing.M) {
	if os.Getenv("MW_TEST_SERVER") != "" {
		serveAsSite()
		return
	}
	Every = 200 * time.Millisecond
	os.Exit(m.Run())
}

var held [][]byte

func touch(mb int) []byte {
	b := make([]byte, mb<<20)
	for i := 0; i < len(b); i += 4096 {
		b[i] = 1
	}
	return b
}

func serveAsSite() {
	var n int
	fmt.Sscan(os.Getenv("MW_TEST_HOLD"), &n)
	if n > 0 {
		held = append(held, touch(n))
	}
	l, err := net.Listen("tcp", "127.0.0.1:"+os.Getenv("PORT"))
	if err != nil {
		os.Exit(1)
	}
	rev := os.Getenv("BUILD")
	_ = http.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/eat" {
			for {
				held = append(held, touch(16))
			}
		}
		fmt.Fprint(w, rev)
	}))
}

// house is an engine over a sites folder with the groups wired, and the
// bus it says things on.
type house struct {
	eng   *engine.Engine
	sites string
	bus   *events.Bus
	tree  *cgroups.Tree
	def   *manifest.Limits
	mu    sync.Mutex
}

func newHouse(t *testing.T, tree *cgroups.Tree) *house {
	t.Helper()
	root := t.TempDir()
	h := &house{sites: filepath.Join(root, "sites"), bus: events.New(""), tree: tree}
	os.MkdirAll(h.sites, 0o755)
	h.eng = engine.New(engine.Options{SitesDir: h.sites, DataDir: filepath.Join(root, "data"), Ports: ports.New(41200, 41300), Bus: h.bus, Tick: 200 * time.Millisecond,
		Runtimes: map[string]runtime.Runtime{"process": runtime.Process{}}})
	Wire(h.sites, h.bus, h.eng, tree, func() *manifest.Limits { h.mu.Lock(); defer h.mu.Unlock(); return h.def })
	t.Cleanup(func() { runtime.SiteGroups = nil })
	return h
}

func (h *house) setDefault(l *manifest.Limits) {
	h.mu.Lock()
	h.def = l
	h.mu.Unlock()
}

func (h *house) site(t *testing.T, domain, build, limits, env string) string {
	t.Helper()
	dir := filepath.Join(h.sites, domain)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, ".current"), []byte(build+"\n"), 0o644)
	run := fmt.Sprintf(`"run":{"command":[%q],"env":{"MW_TEST_SERVER":"1"%s},"env_from_file":{"BUILD":".current"},"drain":"300ms","health":{"timeout":"5s"}}`, os.Args[0], env)
	if limits != "" {
		run += `,"limits":` + limits
	}
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(`{"version":2,`+run+`}`), 0o644)
	return dir
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("never: %s", what)
}

func folderTree(t *testing.T) (*cgroups.Tree, string) {
	t.Helper()
	old := cgroups.Root
	t.Cleanup(func() { cgroups.Root = old })
	cgroups.Root = t.TempDir()
	unit := filepath.Join(cgroups.Root, "system.slice", "middleware.service")
	os.MkdirAll(filepath.Join(unit, "house"), 0o755)
	os.WriteFile(filepath.Join(unit, "cgroup.subtree_control"), nil, 0o644)
	os.WriteFile(filepath.Join(unit, "cgroup.controllers"), []byte("cpu memory pids"), 0o644)
	tree, err := cgroups.Setup("/system.slice/middleware.service/house", cgroups.Limits{CPUWeight: 10})
	if err != nil {
		t.Fatal(err)
	}
	return tree, filepath.Join(unit, "sites")
}

func read(p string) string {
	b, _ := os.ReadFile(p)
	return strings.TrimSpace(string(b))
}

// A manifest's limits are in the site's group before the site is found
// running, a change is written live, the house's defaults fill what the
// manifest leaves out, and a kill the kernel makes is said.
func TestTheManifestsLimitsReachTheGroupLive(t *testing.T) {
	tree, sites := folderTree(t)
	h := newHouse(t, tree)
	// A static site: the limits are written on the reconcile whatever the
	// site runs, so this needs no process.
	dir := filepath.Join(h.sites, "a.test")
	os.MkdirAll(dir, 0o755)
	write := func(limits string) {
		os.WriteFile(filepath.Join(dir, "site.json"), []byte(`{"version":2,"static":{},"limits":`+limits+`}`), 0o644)
	}
	write(`{"memory":"1g","memory_high":"768m","cpus":1.5,"cpu_weight":200}`)
	ch, cancelSub := h.bus.Subscribe()
	defer cancelSub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.eng.Run(ctx)
	group := filepath.Join(sites, "a.test")
	eventually(t, "limits written", func() bool { return read(filepath.Join(group, "memory.max")) == "1073741824" })
	if got := read(filepath.Join(group, "cpu.max")); got != "150000 100000" {
		t.Fatalf("cpu.max %q", got)
	}
	if got := read(filepath.Join(group, "memory.swap.max")); got != "0" {
		t.Fatalf("a ceiling that swaps: %q", got)
	}
	write(`{"memory":"512m"}`)
	eventually(t, "a lower ceiling written live", func() bool { return read(filepath.Join(group, "memory.max")) == "536870912" })
	if got := read(filepath.Join(group, "memory.high")); got != "max" {
		t.Fatalf("memory.high after the change %q", got)
	}
	h.setDefault(&manifest.Limits{Pids: 1000, CPUs: 4})
	eventually(t, "the house's defaults under the manifest", func() bool {
		return read(filepath.Join(group, "pids.max")) == "1000" && read(filepath.Join(group, "cpu.max")) == "400000 100000"
	})
	// The kernel's count moves: said once, with the limit.
	os.WriteFile(filepath.Join(group, "memory.events"), []byte("low 0\nhigh 0\nmax 3\noom 1\noom_kill 0\n"), 0o644)
	time.Sleep(3 * Every)
	os.WriteFile(filepath.Join(group, "memory.events"), []byte("low 0\nhigh 0\nmax 9\noom 2\noom_kill 2\n"), 0o644)
	deadline := time.After(10 * time.Second)
	for {
		select {
		case e := <-ch:
			if e.Type != "site.oom" {
				continue
			}
			if e.Subject != "a.test" || e.Level != "warn" || e.Data["kills"] != int64(2) || e.Data["limit"] != int64(512<<20) {
				t.Fatalf("site.oom %+v", e)
			}
			return
		case <-deadline:
			t.Fatal("no site.oom")
		}
	}
}

// liveTree splits the delegated cgroup this test runs in; see
// internal/runtime's cgroups_live_test.go for how CI starts it.
func liveTree(t *testing.T) *cgroups.Tree {
	t.Helper()
	if os.Getenv("MIDDLEWARE_TEST_CGROUPS") != "1" {
		t.Skip("needs a delegated cgroup: MIDDLEWARE_TEST_CGROUPS=1")
	}
	tree, err := cgroups.Setup(cgroups.SelfPath(), cgroups.DefaultLimits())
	if err != nil || !tree.Sites() {
		t.Fatalf("%v %v", err, tree.Notes())
	}
	return tree
}

func get(port int, path string) (string, error) {
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d%s", port, path))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b := make([]byte, 64)
	n, _ := resp.Body.Read(b)
	return string(b[:n]), nil
}

// stopAll kills what the sites run, whole groups at once: sites outlive
// the engine on purpose, and a test leaves nothing behind.
func stopAll(h *house, domains ...string) {
	for _, d := range domains {
		_ = os.WriteFile(filepath.Join(h.tree.SitePath(d), "cgroup.kill"), []byte("1"), 0o644)
	}
}

// A process site over its memory.max is killed inside its own group and
// site.oom says so, while the neighbour goes on answering.
func TestCgroupLiveASiteOverItsMemoryIsKilledAndItsNeighbourAnswers(t *testing.T) {
	h := newHouse(t, liveTree(t))
	h.site(t, "fat.test", "fat", `{"memory":"64m"}`, "")
	h.site(t, "thin.test", "thin", "", "")
	ch, cancelSub := h.bus.Subscribe()
	defer cancelSub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.eng.Run(ctx)
	defer stopAll(h, "fat.test", "thin.test")
	defer cancel()
	var fat, thin engine.Snapshot
	eventually(t, "both up", func() bool {
		fat, _ = h.eng.Lookup("fat.test")
		thin, _ = h.eng.Lookup("thin.test")
		return fat.Alive && thin.Alive
	})
	stop := make(chan struct{})
	var failed, total int
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			total++
			if body, err := get(thin.Port, "/"); err != nil || body != "thin" {
				failed++
			}
		}
	}()
	go get(fat.Port, "/eat")
	deadline := time.After(20 * time.Second)
	var oom events.Event
wait:
	for {
		select {
		case e := <-ch:
			if e.Type == "site.oom" {
				oom = e
				break wait
			}
		case <-deadline:
			t.Fatal("no site.oom")
		}
	}
	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
	if oom.Subject != "fat.test" || oom.Data["limit"] != int64(64<<20) {
		t.Fatalf("site.oom %+v", oom)
	}
	if failed != 0 || total < 20 {
		t.Fatalf("the neighbour failed %d of %d requests", failed, total)
	}
}

// A deploy whose new generation does not fit in the site's memory beside
// the old one: the new one is the one killed, it fails its health check,
// and the old one serves every request throughout.
func TestCgroupLiveAHandoverAtTheMemoryLimitDropsNoRequest(t *testing.T) {
	h := newHouse(t, liveTree(t))
	dir := h.site(t, "full.test", "build-1", `{"memory":"112m"}`, `,"MW_TEST_HOLD":"64"`)
	ch, cancelSub := h.bus.Subscribe()
	defer cancelSub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.eng.Run(ctx)
	defer stopAll(h, "full.test")
	defer cancel()
	eventually(t, "up", func() bool { sn, _ := h.eng.Lookup("full.test"); return sn.Alive })
	stop := make(chan struct{})
	var failed, total int
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			sn, _ := h.eng.Lookup("full.test")
			total++
			if _, err := get(sn.Port, "/"); err != nil {
				failed++
			}
		}
	}()
	os.WriteFile(filepath.Join(dir, ".current"), []byte("build-2\n"), 0o644)
	deadline := time.After(30 * time.Second)
	var sawOOM, sawFail bool
	// Everything the site said, to read when it fails: which generation
	// went, and in what order.
	var said []string
	defer func() {
		if t.Failed() {
			t.Logf("events:\n%s", strings.Join(said, "\n"))
			log, _ := os.ReadFile(filepath.Join(filepath.Dir(h.sites), "data", "logs", "full.test", "site.log"))
			t.Logf("site.log:\n%s", log)
		}
	}()
	for !sawOOM || !sawFail {
		select {
		case e := <-ch:
			if e.Subject == "full.test" {
				said = append(said, fmt.Sprintf("%s %s %s %v", e.Time.Format("15:04:05.000"), e.Type, e.Message, e.Data))
			}
			switch e.Type {
			case "site.oom":
				sawOOM = true
			case "instance.failed":
				sawFail = true
			case "handover.switched":
				t.Fatal("the new generation was switched to although it could not fit")
			}
		case <-deadline:
			t.Fatalf("oom %v, failed start %v", sawOOM, sawFail)
		}
	}
	sleepUntil := time.After(500 * time.Millisecond)
drain:
	for {
		select {
		case e := <-ch:
			if e.Subject == "full.test" {
				said = append(said, fmt.Sprintf("%s %s %s %v", e.Time.Format("15:04:05.000"), e.Type, e.Message, e.Data))
			}
		case <-sleepUntil:
			break drain
		}
	}
	close(stop)
	wg.Wait()
	sn, _ := h.eng.Lookup("full.test")
	if body, err := get(sn.Port, "/"); body != "build-1" {
		t.Fatalf("serving %q (%v), want the old generation; %d of %d requests failed", body, err, failed, total)
	}
	if failed != 0 || total < 50 {
		t.Fatalf("%d of %d requests failed across the handover", failed, total)
	}
}
