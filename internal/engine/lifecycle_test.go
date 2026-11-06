package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/ports"
	"github.com/dcimorra/the-middleware/internal/runtime"
)

// zombie reports whether pid has exited and waits to be reaped. In a
// container whose PID 1 does not reap, a killed orphan stays like that, and
// for these tests it is dead.
func zombie(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		// Reaped between dead's kill(pid, 0) and this read: gone, which
		// is deader than a zombie. Answering false here made a reaped
		// child read as running (seen on CI in
		// TestAnExitedInstanceTakesItsGroupWithIt).
		return os.IsNotExist(err)
	}
	i := strings.LastIndexByte(string(data), ')')
	return i >= 0 && i+2 < len(data) && data[i+2] == 'Z'
}

func dead(pid int) bool {
	return syscall.Kill(pid, 0) != nil || zombie(pid)
}

func waitDead(t *testing.T, pid int, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	// One answer per look: asking again after the loop could see a pid
	// the loop saw dead come back as another process.
	gone := dead(pid)
	for !gone && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		gone = dead(pid)
	}
	if !gone {
		t.Fatalf("%s (pid %d) is still running", what, pid)
	}
}

// gatedRuntime is the process runtime with a hand on Start: gate, when
// set, runs before every start and may block it, the way a docker build
// or an nvm install does. Every instance it started is kept.
type gatedRuntime struct {
	runtime.Process
	gate    func(spec *manifest.Spec)
	mu      sync.Mutex
	started []runtime.Instance
	starts  map[string]int
}

func (g *gatedRuntime) Start(ctx context.Context, spec *manifest.Spec, port int, logFile, home string) (runtime.Instance, error) {
	g.mu.Lock()
	if g.starts == nil {
		g.starts = map[string]int{}
	}
	g.starts[spec.Domain]++
	g.mu.Unlock()
	if g.gate != nil {
		g.gate(spec)
	}
	inst, err := g.Process.Start(ctx, spec, port, logFile, home)
	if err == nil {
		g.mu.Lock()
		g.started = append(g.started, inst)
		g.mu.Unlock()
	}
	return inst, err
}

func (g *gatedRuntime) instances() []runtime.Instance {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]runtime.Instance(nil), g.started...)
}

func (g *gatedRuntime) startsOf(domain string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.starts[domain]
}

// newGatedEngine is newTestEngine on a gatedRuntime.
func newGatedEngine(t *testing.T, g *gatedRuntime) (*Engine, string) {
	t.Helper()
	root := t.TempDir()
	sites := filepath.Join(root, "sites")
	os.MkdirAll(sites, 0o755)
	e := New(Options{SitesDir: sites, DataDir: filepath.Join(root, "data"), Ports: ports.New(41000, 41100), Bus: events.New(""), Tick: 200 * time.Millisecond,
		Runtimes: map[string]runtime.Runtime{"process": g}})
	t.Cleanup(func() {
		stopEverything(e)
		for _, inst := range g.instances() {
			_ = inst.Stop(context.Background(), time.Second)
		}
	})
	return e, sites
}

// A worker's start ran under the engine's lock, which every request takes
// to find its site: a docker build or an nvm install stalled the traffic of
// every site on the machine for as long as it took.
func TestASlowWorkerStartDoesNotStallLookups(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	g := &gatedRuntime{gate: func(spec *manifest.Spec) {
		if spec.Domain == "slow.test/api" {
			once.Do(func() { close(entered) })
			<-release
		}
	}}
	e, sites := newGatedEngine(t, g)
	dir := filepath.Join(sites, "slow.test")
	os.MkdirAll(dir, 0o755)
	m := fmt.Sprintf(`{"version":2,"static":{},"workers":{"api":{"command":[%q],"env":{"MW_TEST_SERVER":"1","BUILD":"w"}}}}`, os.Args[0])
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(m), 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker's start never began")
	}
	// Several ticks' worth of requests while the start hangs.
	for end := time.Now().Add(time.Second); time.Now().Before(end); {
		found := make(chan bool, 1)
		go func() { _, ok := e.Lookup("slow.test"); found <- ok }()
		select {
		case ok := <-found:
			if !ok {
				close(release)
				t.Fatal("the site is not known")
			}
		case <-time.After(200 * time.Millisecond):
			close(release) // or the cleanup waits on the lock for good
			t.Fatal("a lookup waited on a worker's start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := g.startsOf("slow.test/api"); n != 1 {
		close(release)
		t.Fatalf("the worker was started %d times while its first start hung", n)
	}
	close(release)
	sn := waitFor(t, e, "slow.test", func(s Snapshot) bool { return s.Workers["api"].Alive })
	if body, err := get(sn.Workers["api"].Port); err != nil || body != "w" {
		t.Fatalf("the worker does not answer: %q %v", body, err)
	}
}

// runEngine runs e until the returned stop is called, which returns once
// Run has.
func runEngine(e *Engine) (stop func() time.Duration) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	return func() time.Duration {
		began := time.Now()
		cancel()
		<-done
		return time.Since(began)
	}
}

func readState(t *testing.T, e *Engine, key string) (runtime.State, bool) {
	t.Helper()
	data, err := os.ReadFile(e.statePath(key))
	if err != nil {
		return runtime.State{}, false
	}
	var st runtime.State
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	return st, true
}

// A SIGTERM during a handover's drain, after the new generation's state was
// written: Run returned at once and the process exited, and the old
// generation went on running outside every state file, holding its port.
// Run now ends the handover first: the drain is cut short and the old one
// stopped.
func TestAShutdownDuringTheDrainStopsTheOldGeneration(t *testing.T) {
	g := &gatedRuntime{}
	e, sites := newGatedEngine(t, g)
	dir := filepath.Join(sites, "drain.test")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, ".current"), []byte("one\n"), 0o644)
	m := fmt.Sprintf(`{"version":2,"run":{"command":[%q],"env":{"MW_TEST_SERVER":"1"},"env_from_file":{"BUILD":".current"},"drain":"60s","health":{"timeout":"10s"}}}`, os.Args[0])
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(m), 0o644)
	stop := runEngine(e)
	first := waitFor(t, e, "drain.test", func(s Snapshot) bool { return s.Alive })
	os.WriteFile(filepath.Join(dir, ".current"), []byte("two\n"), 0o644)
	waitFor(t, e, "drain.test", func(s Snapshot) bool { return s.Alive && s.Port != first.Port })
	insts := g.instances()
	if len(insts) != 2 {
		t.Fatalf("%d instances started", len(insts))
	}
	old, next := insts[0], insts[1]
	if d := stop(); d > 15*time.Second {
		t.Fatalf("Run took %s to return: the drain was not cut short", d)
	}
	if !dead(old.State().PID) {
		t.Fatal("Run returned with the old generation still running")
	}
	if !next.Alive() {
		t.Fatal("the new generation did not outlive the supervisor")
	}
	if st, ok := readState(t, e, "drain.test"); !ok || st.PID != next.State().PID {
		t.Fatalf("the state is not the new generation's: %+v %v", st, ok)
	}
}

// A SIGTERM while the new generation is still getting ready: Run returned
// at once and it was orphaned, running with no state file. Now it is
// stopped, and the old one, still the one in the state file, is what the
// next supervisor adopts.
func TestAShutdownBeforeTheSwitchStopsTheNewGeneration(t *testing.T) {
	g := &gatedRuntime{}
	e, sites := newGatedEngine(t, g)
	writeSite(t, sites, "ready.test", "one", "")
	stop := runEngine(e)
	waitFor(t, e, "ready.test", func(s Snapshot) bool { return s.Alive })
	// The next generation takes its time to listen.
	writeSite(t, sites, "ready.test", "two", `,"MW_TEST_SLOW_START":"30s"`)
	deadline := time.Now().Add(10 * time.Second)
	for len(g.instances()) < 2 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	insts := g.instances()
	if len(insts) != 2 {
		t.Fatalf("%d instances started", len(insts))
	}
	old, next := insts[0], insts[1]
	if d := stop(); d > 15*time.Second {
		t.Fatalf("Run took %s to return", d)
	}
	if !dead(next.State().PID) {
		t.Fatal("Run returned with the new generation running and in no state file")
	}
	if !old.Alive() {
		t.Fatal("the old generation did not outlive the supervisor")
	}
	if st, ok := readState(t, e, "ready.test"); !ok || st.PID != old.State().PID {
		t.Fatalf("the state is not the old generation's: %+v %v", st, ok)
	}
}

// A handover that finishes after its site's folder was removed used to
// install the new generation on the detached site and save its state: it
// ran on, on its port, and nothing would ever stop it.
func TestAHandoverThatEndsAfterItsSiteWasRemovedStopsWhatItStarted(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	g := &gatedRuntime{gate: func(spec *manifest.Spec) {
		if spec.Domain == "gone.test" {
			once.Do(func() { close(entered) })
			<-release
		}
	}}
	e, sites := newGatedEngine(t, g)
	dir := writeSite(t, sites, "gone.test", "x", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the handover never started")
	}
	// The manifest goes, not the folder: a start in a folder that is gone
	// fails by itself, and this is about the start that succeeds.
	os.Remove(filepath.Join(dir, "site.json"))
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := e.Lookup("gone.test"); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the removed site is still known")
		}
		time.Sleep(50 * time.Millisecond)
	}
	close(release)
	deadline = time.Now().Add(15 * time.Second)
	for len(g.instances()) == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	insts := g.instances()
	if len(insts) != 1 {
		t.Fatalf("%d instances started", len(insts))
	}
	waitDead(t, insts[0].State().PID, "the generation that started after its site was removed")
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(e.statePath("gone.test")); !os.IsNotExist(err) {
		t.Fatalf("its state was left behind: %v", err)
	}
}

// A site whose shell exits while what it started in the background goes
// on: the engine used to forget the instance and release its port with the
// child still on it, one more child with every restart the backoff made.
func TestAnExitedInstanceTakesItsGroupWithIt(t *testing.T) {
	e, sites := newTestEngine(t)
	dir := filepath.Join(sites, "group.test")
	os.MkdirAll(dir, 0o755)
	pidFile := filepath.Join(dir, "child.pid")
	script := fmt.Sprintf(`"$MW_BIN" & echo $! > %s; sleep 1; exit 1`, pidFile)
	m := fmt.Sprintf(`{"version":2,"run":{"command":["/bin/sh","-c",%q],"env":{"MW_TEST_SERVER":"1","BUILD":"g","MW_BIN":%q},"health":{"timeout":"10s"}}}`, script, os.Args[0])
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(m), 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)
	waitFor(t, e, "group.test", func(s Snapshot) bool { return s.Alive })
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	var child int
	fmt.Sscan(strings.TrimSpace(string(data)), &child)
	if child <= 0 {
		t.Fatalf("no child pid in %q", data)
	}
	defer syscall.Kill(child, syscall.SIGKILL)
	waitFor(t, e, "group.test", func(s Snapshot) bool { return s.Failures >= 1 })
	waitDead(t, child, "the child of an instance whose shell exited")
}

// tickRecovering runs one tick and returns what it panicked with. A panic
// under the engine's lock leaves the lock held, and the cleanup that stops
// what the test started then waited for it for good: the test hung instead
// of failing. The lock is freed whoever holds it, since nobody does now.
func tickRecovering(ctx context.Context, e *Engine) (panicked any) {
	defer func() {
		if p := recover(); p != nil {
			panicked = p
			e.mu.TryLock()
			e.mu.Unlock()
		}
	}()
	e.tick(ctx)
	return nil
}

// startForAdoption starts the test binary as a site's process the way a
// previous supervisor would have, and leaves its state behind.
func startForAdoption(t *testing.T, e *Engine, sites, domain string) runtime.Instance {
	t.Helper()
	dir := writeSite(t, sites, domain, "x", "")
	spec := &manifest.Spec{Domain: domain, SiteDir: dir, Cwd: dir, Command: []string{os.Args[0]}, Env: map[string]string{"MW_TEST_SERVER": "1", "BUILD": "x"}, Revision: "previous"}
	inst, err := runtime.Process{}.Start(context.Background(), spec, freePort(t), filepath.Join(e.opts.DataDir, "logs", domain, "site.log"), filepath.Join(e.opts.DataDir, "homes", domain))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inst.Stop(context.Background(), time.Second) })
	if err := runtime.WaitReady(context.Background(), inst, manifest.Health{}, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	e.saveState(inst.State())
	return inst
}

// An adopted instance has no spec until its first reconcile, and one that
// exited before it took the supervisor down with a nil dereference in the
// crash branch.
func TestAnAdoptedInstanceThatExitsBeforeTheFirstTickIsRestarted(t *testing.T) {
	e, sites := newTestEngine(t)
	inst := startForAdoption(t, e, sites, "early.test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.adoptAll(ctx)
	e.mu.RLock()
	adopted := e.sites["early.test"].Instance
	e.mu.RUnlock()
	if adopted == nil {
		t.Fatal("not adopted")
	}
	_ = inst.Stop(context.Background(), time.Second)
	select {
	case <-adopted.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the adopted instance never noticed its process exited")
	}
	if p := tickRecovering(ctx, e); p != nil {
		t.Fatalf("the tick panicked: %v", p)
	}
	sn, _ := e.Lookup("early.test")
	if sn.Alive || sn.Failures != 1 {
		t.Fatalf("the exit was not counted: %+v", sn)
	}
}

// An adopted process that is the one recorded but no longer listens is
// stopped, not forgotten: forgotten, it kept running outside every state
// file while the next tick started a second copy next to it.
func TestAnAdoptedInstanceThatDoesNotListenIsStopped(t *testing.T) {
	e, sites := newTestEngine(t)
	dir := filepath.Join(sites, "deaf.test")
	os.MkdirAll(dir, 0o755)
	spec := &manifest.Spec{Domain: "deaf.test", SiteDir: dir, Cwd: dir, Command: []string{"/bin/sh", "-c", "exec sleep 300"}, Env: map[string]string{}, Revision: "r1"}
	inst, err := runtime.Process{}.Start(context.Background(), spec, freePort(t), filepath.Join(e.opts.DataDir, "logs", "deaf.test", "site.log"), filepath.Join(e.opts.DataDir, "homes", "deaf.test"))
	if err != nil {
		t.Fatal(err)
	}
	defer inst.Stop(context.Background(), time.Second)
	e.saveState(inst.State())
	e.adoptAll(context.Background())
	waitDead(t, inst.State().PID, "the adopted process that does not listen")
	if _, err := os.Stat(e.statePath("deaf.test")); !os.IsNotExist(err) {
		t.Fatalf("its state is still there: %v", err)
	}
}
