package engine

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/ports"
	"github.com/dcimorra/the-middleware/internal/runtime"
)

// TestMain lets the test binary double as the site under supervision: with
// MW_TEST_SERVER set it is a small HTTP server that answers its revision on
// every path, so the handover test needs no other binary on the machine.
func TestMain(m *testing.M) {
	if os.Getenv("MW_TEST_SERVER") != "" {
		serveAsSite()
		return
	}
	os.Exit(m.Run())
}

func serveAsSite() {
	rev := os.Getenv("BUILD")
	if os.Getenv("MW_TEST_CRASH") != "" {
		os.Exit(7)
	}
	if os.Getenv("MW_TEST_NO_LIBATOMIC") != "" {
		// What the loader says for a Node built against a library the
		// machine lacks, before a line of the program runs.
		fmt.Fprintln(os.Stderr, "node: error while loading shared libraries: libatomic.so.1: cannot open shared object file: No such file or directory")
		os.Exit(127)
	}
	if d := os.Getenv("MW_TEST_SLOW_START"); d != "" {
		dur, _ := time.ParseDuration(d)
		time.Sleep(dur)
	}
	l, err := net.Listen("tcp", "127.0.0.1:"+os.Getenv("PORT"))
	if err != nil {
		os.Exit(1)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Millisecond) // long enough for a drain to matter
		fmt.Fprint(w, rev)
	})}
	_ = srv.Serve(l)
}

func newTestEngine(t *testing.T) (*Engine, string) {
	t.Helper()
	root := t.TempDir()
	sites := filepath.Join(root, "sites")
	data := filepath.Join(root, "data")
	os.MkdirAll(sites, 0o755)
	e := New(Options{SitesDir: sites, DataDir: data, Ports: ports.New(41000, 41100), Bus: events.New(""), Tick: 200 * time.Millisecond,
		Runtimes: map[string]runtime.Runtime{"process": runtime.Process{}}})
	// Sites outlive the engine on purpose, so a test has to stop what it
	// started or every run leaves processes behind.
	t.Cleanup(func() { stopEverything(e) })
	return e, sites
}

func stopEverything(e *Engine) {
	e.mu.RLock()
	var insts []runtime.Instance
	for _, s := range e.sites {
		if s.Instance != nil {
			insts = append(insts, s.Instance)
		}
		for _, w := range s.Workers {
			if w.inst != nil {
				insts = append(insts, w.inst)
			}
		}
		for _, svc := range s.Services {
			if svc.inst != nil {
				insts = append(insts, svc.inst)
			}
		}
	}
	e.mu.RUnlock()
	for _, inst := range insts {
		_ = inst.Stop(context.Background(), time.Second)
	}
}

func writeSite(t *testing.T, sites, domain, build string, extraEnv string) string {
	t.Helper()
	dir := filepath.Join(sites, domain)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, ".current"), []byte(build+"\n"), 0o644)
	m := fmt.Sprintf(`{"version":2,"run":{"command":[%q],"env":{"MW_TEST_SERVER":"1"%s},"env_from_file":{"BUILD":".current"},"drain":"300ms","health":{"timeout":"10s"}}}`, os.Args[0], extraEnv)
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(m), 0o644)
	return dir
}

func waitFor(t *testing.T, e *Engine, domain string, cond func(Snapshot) bool) Snapshot {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if sn, ok := e.Lookup(domain); ok && cond(sn) {
			return sn
		}
		time.Sleep(50 * time.Millisecond)
	}
	sn, _ := e.Lookup(domain)
	t.Fatalf("condition not met for %s: %+v", domain, sn)
	return sn
}

// freePort is a loopback port nothing listens on now.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func get(port int) (string, error) {
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b := make([]byte, 64)
	n, _ := resp.Body.Read(b)
	return string(b[:n]), nil
}

func TestHandoverDropsNoRequests(t *testing.T) {
	e, sites := newTestEngine(t)
	dir := writeSite(t, sites, "a.test", "build-1", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)
	waitFor(t, e, "a.test", func(s Snapshot) bool { return s.Alive })

	var failed, total atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				sn, _ := e.Lookup("a.test")
				total.Add(1)
				if _, err := get(sn.Port); err != nil {
					failed.Add(1)
				}
			}
		}()
	}
	// Two deployments by pointer while the load runs.
	for _, b := range []string{"build-2", "build-3"} {
		time.Sleep(500 * time.Millisecond)
		os.WriteFile(filepath.Join(dir, ".current"), []byte(b+"\n"), 0o644)
		waitFor(t, e, "a.test", func(s Snapshot) bool {
			if !s.Alive || s.Handing {
				return false
			}
			body, err := get(s.Port)
			return err == nil && body == b
		})
	}
	time.Sleep(500 * time.Millisecond)
	close(stop)
	wg.Wait()
	if total.Load() < 50 {
		t.Fatalf("the load generator barely ran: %d requests", total.Load())
	}
	if f := failed.Load(); f != 0 {
		t.Fatalf("%d of %d requests failed across two handovers", f, total.Load())
	}
}

func TestACrashingSiteIsRestartedWithBackoff(t *testing.T) {
	e, sites := newTestEngine(t)
	writeSite(t, sites, "b.test", "x", `,"MW_TEST_CRASH":"1"`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)
	waitFor(t, e, "b.test", func(s Snapshot) bool { return s.Error != "" })
	time.Sleep(2 * time.Second)
	e.mu.RLock()
	failures := e.sites["b.test"].failures
	e.mu.RUnlock()
	if failures < 2 || failures > 4 {
		t.Fatalf("expected a backed-off retry or two in 2s, got %d failures", failures)
	}
}

func TestAHoldStopsTheInstanceAndRemovingItStartsAgain(t *testing.T) {
	e, sites := newTestEngine(t)
	dir := writeSite(t, sites, "c.test", "x", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)
	first := waitFor(t, e, "c.test", func(s Snapshot) bool { return s.Alive })
	m := fmt.Sprintf(`{"version":2,"hold":{"enabled":true},"run":{"command":[%q],"env":{"MW_TEST_SERVER":"1"},"env_from_file":{"BUILD":".current"}}}`, os.Args[0])
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(m), 0o644)
	waitFor(t, e, "c.test", func(s Snapshot) bool { return !s.Alive && s.Mode == "hold" })
	if _, err := get(first.Port); err == nil {
		t.Fatal("the old instance still answers")
	}
	writeSite(t, sites, "c.test", "x", "")
	waitFor(t, e, "c.test", func(s Snapshot) bool { return s.Alive })
}

func TestAFailedStartKeepsTheOldInstanceServing(t *testing.T) {
	e, sites := newTestEngine(t)
	dir := writeSite(t, sites, "d.test", "x", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)
	first := waitFor(t, e, "d.test", func(s Snapshot) bool { return s.Alive })
	m := fmt.Sprintf(`{"version":2,"run":{"command":[%q],"env":{"MW_TEST_SERVER":"1","MW_TEST_CRASH":"1"},"env_from_file":{"BUILD":".current"}}}`, os.Args[0])
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(m), 0o644)
	waitFor(t, e, "d.test", func(s Snapshot) bool { return s.Error != "" && s.Alive })
	if body, err := get(first.Port); err != nil || body != "x" {
		t.Fatalf("old instance not serving: %q %v", body, err)
	}
}

func TestInstancesAreReAdoptedByANewEngine(t *testing.T) {
	e, sites := newTestEngine(t)
	writeSite(t, sites, "e.test", "x", "")
	ctx, cancel := context.WithCancel(context.Background())
	go e.Run(ctx)
	first := waitFor(t, e, "e.test", func(s Snapshot) bool { return s.Alive })
	cancel()
	time.Sleep(300 * time.Millisecond)

	e2 := New(Options{SitesDir: e.opts.SitesDir, DataDir: e.opts.DataDir, Ports: ports.New(41000, 41100), Bus: events.New(""), Tick: 200 * time.Millisecond,
		Runtimes: map[string]runtime.Runtime{"process": runtime.Process{}}})
	t.Cleanup(func() { stopEverything(e2) })
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go e2.Run(ctx2)
	sn := waitFor(t, e2, "e.test", func(s Snapshot) bool { return s.Alive })
	if sn.Port != first.Port {
		t.Fatalf("adopted on port %d, was %d: it was restarted instead of adopted", sn.Port, first.Port)
	}
	e.mu.RLock()
	inst := e.sites["e.test"].Instance
	e.mu.RUnlock()
	inst.Stop(context.Background(), time.Second)
}

// A running site whose manifest loses its run section must stop cleanly.
// The stop used to read the drain from the new manifest, which has no run
// section, and the nil dereference took the supervisor down.
func TestARunningSiteTurnedStaticStopsWithoutAPanic(t *testing.T) {
	e, sites := newTestEngine(t)
	dir := writeSite(t, sites, "f.test", "x", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)
	first := waitFor(t, e, "f.test", func(s Snapshot) bool { return s.Alive })
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(`{"version":2,"static":{}}`), 0o644)
	waitFor(t, e, "f.test", func(s Snapshot) bool { return !s.Alive && s.Mode == "static" })
	if _, err := get(first.Port); err == nil {
		t.Fatal("the old instance still answers")
	}
}

func workerPID(e *Engine, domain, name string) int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	s, ok := e.sites[domain]
	if !ok || s.Workers[name] == nil || s.Workers[name].inst == nil {
		return 0
	}
	return s.Workers[name].inst.State().PID
}

// Before workers were saved, every supervisor restart started a second copy
// next to the one still running.
func TestAWorkerWithoutAPortGetsOneAndIsReAdopted(t *testing.T) {
	e, sites := newTestEngine(t)
	dir := filepath.Join(sites, "g.test")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "build"), []byte("w1"), 0o644)
	m := fmt.Sprintf(`{"version":2,"static":{},"workers":{"api":{"command":[%q],"env":{"MW_TEST_SERVER":"1"},"env_from_file":{"BUILD":"build"}}}}`, os.Args[0])
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(m), 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	go e.Run(ctx)
	// Alive is not listening: a worker has no readiness check, so the
	// test waits for the answer itself.
	sn := waitFor(t, e, "g.test", func(s Snapshot) bool {
		body, err := get(s.Workers["api"].Port)
		return s.Workers["api"].Alive && s.Workers["api"].Port != 0 && err == nil && body == "w1"
	})
	port := sn.Workers["api"].Port
	_ = port
	pid := workerPID(e, "g.test", "api")
	cancel()
	time.Sleep(300 * time.Millisecond)

	e2 := New(Options{SitesDir: e.opts.SitesDir, DataDir: e.opts.DataDir, Ports: ports.New(41000, 41100), Bus: events.New(""), Tick: 200 * time.Millisecond,
		Runtimes: map[string]runtime.Runtime{"process": runtime.Process{}}})
	t.Cleanup(func() { stopEverything(e2) })
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go e2.Run(ctx2)
	waitFor(t, e2, "g.test", func(s Snapshot) bool { return s.Workers["api"].Alive })
	time.Sleep(600 * time.Millisecond) // a few ticks: a duplicate would start here
	if got := workerPID(e2, "g.test", "api"); got != pid {
		t.Fatalf("worker pid %d after the restart, was %d: it was started again instead of adopted", got, pid)
	}
	// A changed definition restarts it, on an assigned port again.
	os.WriteFile(filepath.Join(dir, "build"), []byte("w2"), 0o644)
	sn = waitFor(t, e2, "g.test", func(s Snapshot) bool {
		body, err := get(s.Workers["api"].Port)
		return err == nil && body == "w2"
	})
	// Disabling it stops it.
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(strings.Replace(m, `"api":{`, `"api":{"disabled":true,`, 1)), 0o644)
	waitFor(t, e2, "g.test", func(s Snapshot) bool { _, ok := s.Workers["api"]; return !ok })
	if _, err := get(sn.Workers["api"].Port); err == nil {
		t.Fatal("a disabled worker still answers")
	}
}

func TestASiteWithAWrittenPortRestartsOnIt(t *testing.T) {
	e, sites := newTestEngine(t)
	dir := writeSite(t, sites, "h.test", "x", "")
	port := freePort(t) // not a written number: see freePort's other caller
	m := fmt.Sprintf(`{"version":2,"run":{"command":[%q],"port":%d,"env":{"MW_TEST_SERVER":"1"},"env_from_file":{"BUILD":".current"},"health":{"timeout":"10s"}}}`, os.Args[0], port)
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(m), 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)
	waitFor(t, e, "h.test", func(s Snapshot) bool { return s.Alive && s.Port == port })
	os.WriteFile(filepath.Join(dir, ".current"), []byte("y\n"), 0o644)
	waitFor(t, e, "h.test", func(s Snapshot) bool {
		body, err := get(port)
		return s.Alive && s.Port == port && err == nil && body == "y"
	})
}

// Health used to be checked only at start: a site that hung while alive was
// never noticed.
func TestARunningSiteIsCheckedAndAHungOneShowsDown(t *testing.T) {
	e, sites := newTestEngine(t)
	dir := writeSite(t, sites, "i.test", "x", "")
	m := fmt.Sprintf(`{"version":2,"run":{"command":[%q],"env":{"MW_TEST_SERVER":"1"},"env_from_file":{"BUILD":".current"},"health":{"path":"/","timeout":"1s","interval":"1s"}}}`, os.Args[0])
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(m), 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)
	sn := waitFor(t, e, "i.test", func(s Snapshot) bool { return s.Alive && s.Health == HealthOK })
	// Freeze the process: alive, port open, answering nothing.
	e.mu.RLock()
	pid := e.sites["i.test"].Instance.State().PID
	e.mu.RUnlock()
	syscall.Kill(pid, syscall.SIGSTOP)
	defer syscall.Kill(pid, syscall.SIGCONT)
	waitFor(t, e, "i.test", func(s Snapshot) bool { return s.Health == HealthDown })
	syscall.Kill(pid, syscall.SIGCONT)
	sn = waitFor(t, e, "i.test", func(s Snapshot) bool { return s.Health == HealthOK })
	if len(sn.Checks) < 3 {
		t.Fatalf("only %d checks recorded", len(sn.Checks))
	}
}

func TestAStaticSiteWithoutItsFilesShowsDown(t *testing.T) {
	e, sites := newTestEngine(t)
	dir := filepath.Join(sites, "j.test")
	os.MkdirAll(filepath.Join(dir, "public"), 0o755)
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(`{"version":2}`), 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)
	waitFor(t, e, "j.test", func(s Snapshot) bool { return s.Health == HealthDown })
}

func TestAnAliasServesItsSiteUnlessAnotherSiteHasThatName(t *testing.T) {
	e, sites := newTestEngine(t)
	for _, d := range []string{"k.test", "l.test"} {
		os.MkdirAll(filepath.Join(sites, d, "public"), 0o755)
	}
	os.WriteFile(filepath.Join(sites, "k.test", "site.json"), []byte(`{"version":2,"aliases":["k-alias.test","l.test"]}`), 0o644)
	os.WriteFile(filepath.Join(sites, "l.test", "site.json"), []byte(`{"version":2}`), 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)
	waitFor(t, e, "k.test", func(s Snapshot) bool { return s.Manifest != nil })
	time.Sleep(300 * time.Millisecond)
	if sn, ok := e.Lookup("k-alias.test"); !ok || sn.Domain != "k.test" {
		t.Fatalf("the alias resolved to %+v %v", sn.Domain, ok)
	}
	if sn, _ := e.Lookup("l.test"); sn.Domain != "l.test" {
		t.Fatalf("an alias took over a site of its own: %s", sn.Domain)
	}
	// The www of an extra domain goes with it, and not with a site that
	// only tried to claim another site's name.
	hosts := e.Hosts()
	if hosts["www.k-alias.test"] != "k.test" || hosts["www.l.test"] != "l.test" {
		t.Fatalf("www owners: k-alias %q, l %q", hosts["www.k-alias.test"], hosts["www.l.test"])
	}
}

func TestAProjectRunsItsWorkersAndServesNothing(t *testing.T) {
	e, sites := newTestEngine(t)
	dir := filepath.Join(sites, "tools")
	os.MkdirAll(dir, 0o755)
	m := fmt.Sprintf(`{"version":2,"workers":{"bot":{"command":[%q],"env":{"MW_TEST_SERVER":"1","BUILD":"b"}}}}`, os.Args[0])
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(m), 0o644)
	bad := filepath.Join(sites, "other")
	os.MkdirAll(bad, 0o755)
	os.WriteFile(filepath.Join(bad, "site.json"), []byte(fmt.Sprintf(`{"version":2,"run":{"command":[%q]}}`, os.Args[0])), 0o644)
	reserved := filepath.Join(sites, "middleware")
	os.MkdirAll(reserved, 0o755)
	os.WriteFile(filepath.Join(reserved, "site.json"), []byte(`{"version":2}`), 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)
	sn := waitFor(t, e, "tools", func(s Snapshot) bool { return s.Workers["bot"].Alive })
	if sn.Mode != "project" {
		t.Fatalf("mode %q", sn.Mode)
	}
	waitFor(t, e, "other", func(s Snapshot) bool { return s.Error != "" && !s.Alive })
	if _, ok := e.Lookup("middleware"); ok {
		t.Fatal("a folder named after the house's own conversation was taken as a project")
	}
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(`{"version":2}`), 0o644)
	waitFor(t, e, "tools", func(s Snapshot) bool { return len(s.Workers) == 0 })
}

// A whole machine coming up starts in order: a site waits for what it
// needs, and the snapshot names what is missing.
func TestASiteWaitsForWhatItNeedsAndSaysWhatIsMissing(t *testing.T) {
	e, sites := newTestEngine(t)
	m := fmt.Sprintf(`{"version":2,"needs":["api.test","w@api.test"],"run":{"command":[%q],"env":{"MW_TEST_SERVER":"1","BUILD":"front"}}}`, os.Args[0])
	os.MkdirAll(filepath.Join(sites, "front.test"), 0o755)
	os.WriteFile(filepath.Join(sites, "front.test", "site.json"), []byte(m), 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)
	sn := waitFor(t, e, "front.test", func(s Snapshot) bool { return len(s.Missing) == 2 })
	time.Sleep(600 * time.Millisecond)
	if sn, _ = e.Lookup("front.test"); sn.Alive {
		t.Fatal("started before what it needs")
	}
	api := fmt.Sprintf(`{"version":2,"run":{"command":[%q],"env":{"MW_TEST_SERVER":"1","BUILD":"api"}},"workers":{"w":{"command":[%q],"env":{"MW_TEST_SERVER":"1","BUILD":"w"}}}}`, os.Args[0], os.Args[0])
	os.MkdirAll(filepath.Join(sites, "api.test"), 0o755)
	os.WriteFile(filepath.Join(sites, "api.test", "site.json"), []byte(api), 0o644)
	waitFor(t, e, "front.test", func(s Snapshot) bool { return s.Alive && len(s.Missing) == 0 })
	if api, _ := e.Lookup("api.test"); !api.Alive {
		t.Fatal("front started before api")
	}
}

// Opening the site door puts the token in the process's environment, and
// closing it restarts the site: a process must not keep a token its
// manifest no longer grants.
func TestOpeningTheSiteDoorRestartsTheSiteWithItsToken(t *testing.T) {
	e, sites := newTestEngine(t)
	e.SetSiteEnv(func(domain string, m *manifest.Manifest) (map[string]string, []string) {
		if m.ClaudeAPI == nil {
			return nil, nil
		}
		return map[string]string{"BUILD": "token-for-" + domain}, nil
	})
	dir := filepath.Join(sites, "door.test")
	os.MkdirAll(dir, 0o755)
	base := fmt.Sprintf(`{"version":2,"run":{"command":[%q],"env":{"MW_TEST_SERVER":"1","BUILD":"none"}}%%s}`, os.Args[0])
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(fmt.Sprintf(base, "")), 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)
	waitFor(t, e, "door.test", func(s Snapshot) bool { body, err := get(s.Port); return s.Alive && err == nil && body == "none" })
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(fmt.Sprintf(base, `,"claude_api":{}`)), 0o644)
	waitFor(t, e, "door.test", func(s Snapshot) bool {
		body, err := get(s.Port)
		return s.Alive && err == nil && body == "token-for-door.test"
	})
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(fmt.Sprintf(base, "")), 0o644)
	waitFor(t, e, "door.test", func(s Snapshot) bool { body, err := get(s.Port); return s.Alive && err == nil && body == "none" })
}

func TestRestartingAWorkerOnAWrittenPortGivesANewProcessThatAnswers(t *testing.T) {
	e, sites := newTestEngine(t)
	dir := filepath.Join(sites, "w.test")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "build"), []byte("w1"), 0o644)
	// A port free now, not a number written here: 41088 sits in the
	// kernel's ephemeral range, which the house reserves on its machines
	// and a CI runner does not, and with the packages' tests running side
	// by side a loopback client took it often enough to fail two releases.
	port := freePort(t)
	m := fmt.Sprintf(`{"version":2,"static":{},"workers":{"api":{"command":[%q],"port":%d,"env":{"MW_TEST_SERVER":"1"},"env_from_file":{"BUILD":"build"}}}}`, os.Args[0], port)
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(m), 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)
	waitFor(t, e, "w.test", func(s Snapshot) bool {
		body, err := get(port)
		return s.Workers["api"].Alive && err == nil && body == "w1"
	})
	before := workerPID(e, "w.test", "api")
	if err := e.RestartWorker(ctx, "w.test", "api"); err != nil {
		t.Fatal(err)
	}
	// The old process held the written port; the new one must get it,
	// not fail to bind and back off for a minute.
	waitFor(t, e, "w.test", func(s Snapshot) bool {
		body, err := get(port)
		return s.Workers["api"].Alive && err == nil && body == "w1" && workerPID(e, "w.test", "api") != before
	})
	if sn, _ := e.Lookup("w.test"); sn.Workers["api"].Error != "" {
		t.Fatalf("the restart left an error: %s", sn.Workers["api"].Error)
	}
	if err := e.RestartWorker(ctx, "w.test", "nope"); err == nil {
		t.Fatal("restarting a worker that does not exist")
	}
}

// A restart asked by a request outlives the request: the handover runs
// under the engine, not under a context that ends when the API answers
// (it was cancelled halfway, "docker run: context canceled").
func TestARestartOutlivesTheRequestThatAskedForIt(t *testing.T) {
	e, sites := newTestEngine(t)
	dir := filepath.Join(sites, "r.test")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "build"), []byte("r1"), 0o644)
	m := fmt.Sprintf(`{"version":2,"run":{"command":[%q],"env":{"MW_TEST_SERVER":"1"},"env_from_file":{"BUILD":"build"}}}`, os.Args[0])
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(m), 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)
	waitFor(t, e, "r.test", func(s Snapshot) bool { return s.Alive && s.Port != 0 })
	sn, _ := e.Lookup("r.test")
	before := sn.Port
	req, end := context.WithCancel(context.Background())
	if err := e.Restart(req, "r.test"); err != nil {
		t.Fatal(err)
	}
	end() // the request answered
	waitFor(t, e, "r.test", func(s Snapshot) bool { return s.Alive && s.Port != before && !s.Handing })
	if sn, _ := e.Lookup("r.test"); sn.Error != "" {
		t.Fatalf("the restart failed: %s", sn.Error)
	}
}

// A Node that cannot load libatomic said only that in the site's error;
// the error now says what installs it.
func TestASiteMissingASharedLibrarySaysWhatInstallsIt(t *testing.T) {
	e, sites := newTestEngine(t)
	writeSite(t, sites, "lib.test", "x", `,"MW_TEST_NO_LIBATOMIC":"1"`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)
	sn := waitFor(t, e, "lib.test", func(s Snapshot) bool { return s.Error != "" })
	if !strings.Contains(sn.Error, "libatomic.so.1") || !strings.Contains(sn.Error, "the house's requirements installs it") {
		t.Fatalf("error does not say what installs libatomic: %s", sn.Error)
	}
}
