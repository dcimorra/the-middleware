package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dcimorra/the-middleware/internal/manifest"
)

// Process runs an instance as a child of the supervisor.
type Process struct{}

// SiteGroups, when set, is the sites' cgroup branch (internal/cgroups):
// every process a site starts here (its instances, its process workers
// and services, its tasks) is cloned straight into the site's group, and
// one re-adopted from a supervisor that started it in house/ is moved
// there. Nil runs them where the supervisor is. Exec is left out on
// purpose: an install or a migration runs uncapped (see Exec).
var SiteGroups interface {
	SiteAttr(domain string) (*syscall.SysProcAttr, func(), error)
	AdoptSite(domain string, pid int) error
}

// Name implements Runtime.
func (Process) Name() string { return "process" }

type procInstance struct {
	cmd      *exec.Cmd
	pid      int
	port     int
	revision string
	domain   string
	logFile  string
	started  time.Time
	// ticks and boot are the process's identity (State.StartTicks and
	// State.BootID): what a later supervisor compares before adopting.
	ticks   uint64
	boot    string
	adopted bool
	done    chan error
	once    sync.Once
}

// Start implements Runtime. The child gets its own process group so Stop
// reaches grandchildren (`npm start` → node, `go run` → the binary), and its
// output goes to a file opened here: handing exec.Cmd a pipe hangs Wait
// while a grandchild keeps the write end, and a pipe nobody reads kills the
// child with SIGPIPE when the supervisor restarts.
func (Process) Start(ctx context.Context, spec *manifest.Spec, port int, logFile, home string) (Instance, error) {
	argv, env := spec.WithPort(port)
	if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	out, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	defer out.Close() // the child holds its own descriptor after Start
	bin := argv[0]
	if strings.HasPrefix(bin, "./") || strings.HasPrefix(bin, "../") {
		bin = filepath.Join(spec.Cwd, bin)
	}
	// A command that wants Node gets one, the site's version when it can:
	// never a start that fails because the machine has no node.
	if nb := withNode(ctx, spec.SiteDir, argv, env); nb != "" {
		bin = nb
	}
	cmd := exec.Command(bin, argv[1:]...)
	cmd.Dir = spec.Cwd
	cmd.Env = buildEnv(env, port, spec.Domain, spec.SiteDir, home)
	if hasDuplicateKeys(cmd.Env) {
		return nil, errors.New("duplicate keys in the child environment")
	}
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// A house task (no domain) stays with the house. A group that cannot
	// be made leaves the site running uncapped, as before groups existed,
	// rather than not running: the wiring reports it.
	if g := SiteGroups; g != nil && spec.Domain != "" {
		if attr, release, err := g.SiteAttr(spec.Domain); err == nil && attr != nil {
			defer release()
			cmd.SysProcAttr = attr
		}
	}
	// The jail keeps the SysProcAttr it is given, the cgroup with it.
	if err := enclose(cmd, spec, home); err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "\n--- %s middleware: start %s rev %s on port %d\n", time.Now().Format(time.RFC3339), spec.Domain, spec.Revision, port)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &procInstance{cmd: cmd, pid: cmd.Process.Pid, port: port, revision: spec.Revision, domain: spec.Domain, logFile: logFile, started: time.Now(), done: make(chan error, 1)}
	// Read before Wait can reap the child: its stat stays until then,
	// even if it has already exited.
	p.ticks, _ = startTicks(p.pid)
	p.boot = bootID()
	go func() {
		err := cmd.Wait()
		p.done <- err
		close(p.done)
	}()
	return p, nil
}

// Adopt implements Runtime. A process started by a previous supervisor is
// not our child, so Wait is not available: liveness is polled with signal 0
// and Done closes when the pid disappears.
func (Process) Adopt(ctx context.Context, st State) (Instance, error) {
	if st.PID <= 0 || !pidAlive(st.PID) {
		return nil, ErrGone
	}
	// A pid is reused after a reboot or a long uptime. Adopting a stranger
	// would mean killing it on the next restart, so the process must be
	// the one recorded when it was started: the same boot and the same
	// start tick, both counted by the kernel and untouched by the wall
	// clock. Only a state written before those were recorded is checked
	// by wall-clock time, which a clock step defeats (see State).
	ticks, haveTicks := startTicks(st.PID)
	boot := bootID()
	if st.StartTicks != 0 && st.BootID != "" {
		if boot != "" && boot != st.BootID {
			return nil, ErrGone
		}
		if haveTicks && ticks != st.StartTicks {
			return nil, ErrGone
		}
	} else if !st.StartedAt.IsZero() {
		if began, ok := processStart(st.PID); ok {
			if d := began.Sub(st.StartedAt); d > 10*time.Second || d < -10*time.Second {
				return nil, ErrGone
			}
		}
	}
	// Started by a supervisor from before site groups: it and everything
	// it started are in house/ and move to the site's group now. One
	// started by this version is there already and nothing moves.
	if g := SiteGroups; g != nil && st.Domain != "" {
		_ = g.AdoptSite(st.Domain, st.PID)
	}
	p := &procInstance{pid: st.PID, port: st.Port, revision: st.Revision, domain: st.Domain, logFile: st.LogFile, started: st.StartedAt, adopted: true, done: make(chan error, 1),
		ticks: st.StartTicks, boot: st.BootID}
	if p.ticks == 0 || p.boot == "" {
		// An older state that passed the old check learns its identity
		// here, so its next adoption no longer depends on the clock.
		p.ticks, p.boot = ticks, boot
	}
	go func() {
		for pidAlive(p.pid) {
			time.Sleep(time.Second)
		}
		p.done <- errors.New("adopted process exited")
		close(p.done)
	}()
	return p, nil
}

// Adopted reports whether this instance was adopted rather than started.
func (p *procInstance) Adopted() bool { return p.adopted }

func (p *procInstance) Port() int          { return p.port }
func (p *procInstance) Revision() string   { return p.revision }
func (p *procInstance) Done() <-chan error { return p.done }
func (p *procInstance) Alive() bool        { return pidAlive(p.pid) }

func (p *procInstance) State() State {
	return State{Domain: p.domain, Runtime: "process", Port: p.port, Revision: p.revision, PID: p.pid, LogFile: p.logFile, StartedAt: p.started,
		StartTicks: p.ticks, BootID: p.boot}
}

// Stop sends SIGTERM to the whole group, waits up to grace for all of it
// to go, then SIGKILL. The group matters: killing only the shell that
// `npm start` spawned leaves node alive on the port, and the next start
// fails to bind. It is signalled also when the leader has already exited,
// which is what the engine does when Done fires: Stop used to return at
// once then, and the children of `sh -c "a & b"` went on holding the port,
// one more set of them with every restart the backoff made.
//
// The group's id is the leader's pid, which the kernel does not hand out
// again while any process of the group is left, so the only window for
// reaching a stranger is a leader that has been gone, with its whole
// group, long enough for the pid to come round: the engine stops as soon
// as Done fires, a second at most for an adopted pid.
func (p *procInstance) Stop(ctx context.Context, grace time.Duration) error {
	if p.gone() {
		return nil
	}
	_ = syscall.Kill(-p.pid, syscall.SIGTERM) // ESRCH: nothing left, fine
	if p.waitGone(ctx, grace) {
		return nil
	}
	_ = syscall.Kill(-p.pid, syscall.SIGKILL)
	if p.waitGone(context.Background(), 5*time.Second) {
		return nil
	}
	if p.Alive() {
		return fmt.Errorf("pid %d survived SIGKILL", p.pid)
	}
	return fmt.Errorf("process group %d survived SIGKILL", p.pid)
}

// gone reports whether the leader and every process of its group have
// exited. Polled rather than read from done: done carries the exit error
// for the engine, and a Stop that took it left the engine reading nil.
func (p *procInstance) gone() bool {
	return !p.Alive() && !groupAlive(p.pid)
}

func (p *procInstance) waitGone(ctx context.Context, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for !p.gone() {
		if ctx.Err() != nil || time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
	return true
}

// groupAlive reports whether a process other than a zombie is left in
// process group pgid. Signal 0 to the group answers for zombies too, and an
// orphan killed in a container whose PID 1 does not reap stays one for
// good, so a yes from it is checked against /proc.
func groupAlive(pgid int) bool {
	if pgid <= 0 {
		return false
	}
	if err := syscall.Kill(-pgid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return true // no /proc to tell: the signal's answer stands
	}
	want := strconv.Itoa(pgid)
	for _, ent := range entries {
		pid, err := strconv.Atoi(ent.Name())
		if err != nil {
			continue
		}
		// fields[0] is the state (field 3), [2] the group (field 5).
		if f, ok := statFields(pid); ok && len(f) > 2 && f[2] == want && f[0] != "Z" {
			return true
		}
	}
	return false
}

// pidAlive uses signal 0, which checks existence without delivering
// anything. A zombie still answers, so adopted pids are also checked for
// the port being open by the engine before they count as alive.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return !isZombie(pid)
	}
	return errors.Is(err, syscall.EPERM)
}

func isZombie(pid int) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	// The state is the field after the parenthesised command name.
	i := strings.LastIndexByte(string(data), ')')
	if i < 0 || i+2 >= len(data) {
		return false
	}
	return data[i+2] == 'Z'
}

// statFields is /proc/<pid>/stat from field 3 (the state) on. What comes
// before is the command name in parentheses, which may hold spaces and
// parentheses of its own, so the split starts after the last ')'.
func statFields(pid int) ([]string, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return nil, false
	}
	i := strings.LastIndexByte(string(data), ')')
	if i < 0 {
		return nil, false
	}
	return strings.Fields(string(data[i+1:])), true
}

// startTicks is when pid started, in clock ticks since boot: field 22 of
// /proc/<pid>/stat, and fields[0] is field 3, so it is [19].
func startTicks(pid int) (uint64, bool) {
	fields, ok := statFields(pid)
	if !ok || len(fields) < 20 {
		return 0, false
	}
	ticks, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0, false
	}
	return ticks, true
}

// bootID is the kernel's id for this boot: new at every boot and never
// moved by the clock. "" where it cannot be read.
func bootID() string {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// processStart is when pid started, from its start ticks and the boot time
// in /proc/stat. USER_HZ is 100 on every Linux the house runs on; the 10 s
// tolerance in Adopt absorbs the rest. Only states written before
// StartTicks existed are checked with it, because that boot time moves
// with the clock.
func processStart(pid int) (time.Time, bool) {
	ticks, ok := startTicks(pid)
	if !ok {
		return time.Time{}, false
	}
	stat, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, false
	}
	for _, line := range strings.Split(string(stat), "\n") {
		if v, ok := strings.CutPrefix(line, "btime "); ok {
			boot, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return time.Time{}, false
			}
			return time.Unix(boot, 0).Add(time.Duration(ticks) * time.Second / 100), true
		}
	}
	return time.Time{}, false
}
