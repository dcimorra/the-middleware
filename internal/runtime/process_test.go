package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/manifest"
)

func TestTheChildEnvironmentHasNoDuplicateKeysAndPortLast(t *testing.T) {
	t.Setenv("PATH", "/bin")
	env := buildEnv(map[string]string{"PORT": "1", "A": "x", "PATH": "/site/bin"}, 4321, "a.test", "/s", "/h")
	if hasDuplicateKeys(env) {
		t.Fatalf("duplicates: %v", env)
	}
	if env[len(env)-1] != "PORT=4321" {
		t.Fatalf("PORT is not last: %v", env)
	}
	for _, kv := range env {
		if kv == "PATH=/site/bin" {
			return
		}
	}
	t.Fatalf("the manifest's PATH did not win: %v", env)
}

func TestStartWritesOutputToTheLogFileAndStopReachesTheGroup(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "run.sh")
	// A shell that spawns a grandchild: Stop must take both down.
	os.WriteFile(script, []byte("#!/bin/sh\necho started on $PORT\nsleep 300 &\nwait\n"), 0o755)
	spec := &manifest.Spec{Domain: "a.test", SiteDir: dir, Cwd: dir, Command: []string{"./run.sh"}, Env: map[string]string{}, Revision: "r1"}
	inst, err := Process{}.Start(context.Background(), spec, 45999, filepath.Join(dir, "out.log"), filepath.Join(dir, "home"))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if !inst.Alive() {
		t.Fatal("not alive")
	}
	if err := inst.Stop(context.Background(), 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if inst.Alive() {
		t.Fatal("still alive after Stop")
	}
	data, _ := os.ReadFile(filepath.Join(dir, "out.log"))
	if !strings.Contains(string(data), "started on 45999") {
		t.Fatalf("log: %q", data)
	}
}

// `sh -c "a & b"` ends while a goes on in its group, holding the port.
// Stop returned at once when the leader was gone, and every restart the
// backoff made left one more of them.
func TestStopReachesTheGroupAfterTheLeaderHasExited(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	spec := &manifest.Spec{Domain: "a.test", SiteDir: dir, Cwd: dir, Command: []string{"/bin/sh", "-c", "sleep 300 & echo $! > " + pidFile + "; exit 1"}, Env: map[string]string{}, Revision: "r1"}
	inst, err := Process{}.Start(context.Background(), spec, 45996, filepath.Join(dir, "out.log"), filepath.Join(dir, "home"))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-inst.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the shell did not exit")
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	child, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(child, syscall.SIGKILL)
	if !pidAlive(child) {
		t.Fatal("the background child is not running, so the test proves nothing")
	}
	if err := inst.Stop(context.Background(), 2*time.Second); err != nil {
		t.Fatal(err)
	}
	// pidAlive counts a zombie as dead: an orphan killed here may stay
	// one where PID 1 does not reap.
	if pidAlive(child) {
		t.Fatal("the leader's child survived Stop")
	}
}

func TestAdoptRefusesADeadPid(t *testing.T) {
	_, err := Process{}.Adopt(context.Background(), State{PID: 999999})
	if err != ErrGone {
		t.Fatalf("got %v", err)
	}
}

func TestWaitReadyFailsFastWhenTheInstanceDies(t *testing.T) {
	dir := t.TempDir()
	spec := &manifest.Spec{Domain: "a.test", SiteDir: dir, Cwd: dir, Command: []string{"/bin/sh", "-c", "exit 3"}, Env: map[string]string{}, Revision: "r1"}
	inst, err := Process{}.Start(context.Background(), spec, 45998, filepath.Join(dir, "out.log"), filepath.Join(dir, "home"))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = WaitReady(context.Background(), inst, manifest.Health{}, 30*time.Second)
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("err=%v after %s", err, time.Since(start))
	}
}

func TestAReusedPidIsNotAdopted(t *testing.T) {
	began, ok := processStart(os.Getpid())
	if !ok {
		t.Skip("no /proc")
	}
	if d := time.Since(began); d < 0 || d > 5*time.Minute {
		t.Fatalf("this process started %s ago, which is not plausible", d)
	}
	st := State{Domain: "a.test", Runtime: "process", PID: os.Getpid(), StartedAt: began}
	if _, err := (Process{}).Adopt(context.Background(), st); err != nil {
		t.Fatalf("the process that was started was not adopted: %v", err)
	}
	st.StartedAt = began.Add(-time.Hour)
	if _, err := (Process{}).Adopt(context.Background(), st); err != ErrGone {
		t.Fatalf("a pid started an hour after the recorded start was adopted: %v", err)
	}
}

// A clock stepped after the start (NTP's first sync, a VM resumed) moves
// the boot time /proc/stat reports, and with it every start time computed
// from it. The kernel's own count does not move: a process recorded by its
// start tick and boot id is adopted whatever the wall clock says now, and
// a different tick or boot is still a stranger.
func TestAdoptionSurvivesAClockStep(t *testing.T) {
	dir := t.TempDir()
	spec := &manifest.Spec{Domain: "a.test", SiteDir: dir, Cwd: dir, Command: []string{"/bin/sh", "-c", "exec sleep 300"}, Env: map[string]string{}, Revision: "r1"}
	inst, err := Process{}.Start(context.Background(), spec, 45997, filepath.Join(dir, "out.log"), filepath.Join(dir, "home"))
	if err != nil {
		t.Fatal(err)
	}
	defer inst.Stop(context.Background(), time.Second)
	st := inst.State()
	if st.StartTicks == 0 || st.BootID == "" {
		t.Skip("no start ticks or boot id in /proc")
	}
	// The clock jumped an hour since the start.
	st.StartedAt = st.StartedAt.Add(-time.Hour)
	adopted, err := Process{}.Adopt(context.Background(), st)
	if err != nil {
		t.Fatalf("the process that was started was not adopted after a clock step: %v", err)
	}
	if got := adopted.State(); got.StartTicks != st.StartTicks || got.BootID != st.BootID {
		t.Fatalf("the adopted state lost its identity: %+v", got)
	}
	other := st
	other.StartTicks++
	if _, err := (Process{}).Adopt(context.Background(), other); err != ErrGone {
		t.Fatalf("a pid with another start tick was adopted: %v", err)
	}
	other = st
	other.BootID = "00000000-0000-0000-0000-000000000000"
	if _, err := (Process{}).Adopt(context.Background(), other); err != ErrGone {
		t.Fatalf("a pid from another boot was adopted: %v", err)
	}
	// A state from before the identity was recorded keeps the old check,
	// and learns its identity when it passes.
	old := st
	old.StartTicks, old.BootID = 0, ""
	if _, err := (Process{}).Adopt(context.Background(), old); err != ErrGone {
		t.Fatalf("an old state an hour off was adopted: %v", err)
	}
	old.StartedAt = old.StartedAt.Add(time.Hour)
	adopted, err = Process{}.Adopt(context.Background(), old)
	if err != nil {
		t.Fatalf("an old state that matches was not adopted: %v", err)
	}
	if got := adopted.State(); got.StartTicks != st.StartTicks || got.BootID != st.BootID {
		t.Fatalf("an old state did not learn its identity: %+v", got)
	}
}

// A site whose command is `node` starts with the Node it was given, found
// by the child's PATH and not the supervisor's.
func TestASiteThatSaysNodeRunsWithTheNodeItWasGiven(t *testing.T) {
	dir := t.TempDir()
	nodeDir := filepath.Join(dir, "nvm", "bin")
	os.MkdirAll(nodeDir, 0o755)
	os.WriteFile(filepath.Join(nodeDir, "node-made-up"), []byte("#!/bin/sh\necho \"this node, PATH starts $PATH\" | cut -c1-200\nsleep 300\n"), 0o755)
	NodeDir = func(_ context.Context, siteDir string, argv []string) string { return nodeDir }
	defer func() { NodeDir = nil }()
	spec := &manifest.Spec{Domain: "a.test", SiteDir: dir, Cwd: dir, Command: []string{"node-made-up", "server.js"}, Env: map[string]string{}, Revision: "r1"}
	inst, err := Process{}.Start(context.Background(), spec, 45998, filepath.Join(dir, "out.log"), filepath.Join(dir, "home"))
	if err != nil {
		t.Fatal(err)
	}
	defer inst.Stop(context.Background(), time.Second)
	time.Sleep(300 * time.Millisecond)
	data, _ := os.ReadFile(filepath.Join(dir, "out.log"))
	if !strings.Contains(string(data), "this node, PATH starts "+nodeDir) {
		t.Fatalf("log: %q", data)
	}
}

type fakeGroups struct{ asked, adopted []string }

func (f *fakeGroups) SiteAttr(domain string) (*syscall.SysProcAttr, func(), error) {
	f.asked = append(f.asked, domain)
	return &syscall.SysProcAttr{Setpgid: true}, func() {}, nil
}
func (f *fakeGroups) AdoptSite(domain string, pid int) error {
	f.adopted = append(f.adopted, domain)
	return nil
}

// A site's process asks for its site's group; the house's own task (no
// domain) does not, and stays with the supervisor.
func TestASitesProcessAsksForItsGroupAndAHouseTaskDoesNot(t *testing.T) {
	f := &fakeGroups{}
	SiteGroups = f
	defer func() { SiteGroups = nil }()
	dir := t.TempDir()
	for _, d := range []string{"a.test/queue", ""} {
		spec := &manifest.Spec{Domain: d, SiteDir: dir, Cwd: dir, Command: []string{"/bin/true"}, Env: map[string]string{}, Revision: "r"}
		inst, err := Process{}.Start(context.Background(), spec, 0, filepath.Join(dir, "out.log"), filepath.Join(dir, "home"))
		if err != nil {
			t.Fatal(err)
		}
		<-inst.Done()
	}
	if strings.Join(f.asked, ",") != "a.test/queue" {
		t.Fatalf("asked for %v", f.asked)
	}
	if _, err := (Process{}).Adopt(context.Background(), State{Domain: "a.test", PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.adopted, ",") != "a.test" {
		t.Fatalf("adopted %v", f.adopted)
	}
}

// A generation on trial is born with TrialScore, not given it once it
// runs, and the supervisor's own score is back where it was.
func TestATrialIsBornMarkedForTheOOMKiller(t *testing.T) {
	before, err := os.ReadFile("/proc/self/oom_score_adj")
	if err != nil {
		t.Skip("no /proc")
	}
	dir := t.TempDir()
	spec := &manifest.Spec{Domain: "a.test", SiteDir: dir, Cwd: dir, Command: []string{"/bin/sh", "-c", "cat /proc/self/oom_score_adj"}, Env: map[string]string{}, Revision: "r"}
	inst, err := Process{}.Start(WithTrial(context.Background()), spec, 0, filepath.Join(dir, "out.log"), filepath.Join(dir, "home"))
	if err != nil {
		t.Fatal(err)
	}
	<-inst.Done()
	out, _ := os.ReadFile(filepath.Join(dir, "out.log"))
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if got := lines[len(lines)-1]; got != strconv.Itoa(TrialScore) {
		t.Fatalf("the child started with %q, want %d", got, TrialScore)
	}
	if after, _ := os.ReadFile("/proc/self/oom_score_adj"); string(after) != string(before) {
		t.Fatalf("supervisor's score %q, was %q", after, before)
	}
}
