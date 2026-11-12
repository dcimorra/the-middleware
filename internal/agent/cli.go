package agent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// CLIRun is the one subprocess runner both adapters share: a child in its
// own process group whose output goes to a file, read back line by line
// through a parser, and an end that never waits on a grandchild.
//
// A file and not a pipe, for two reasons. A pipe ties the child's life to
// this process: when the supervisor restarts, the child's next write gets
// SIGPIPE and the turn dies with nobody recording it. And a file can be
// read again: the next supervisor finds the turn still running and adopts
// it (AdoptCLI), or finds it finished and reads its result anyway. The
// moment the parser says the run is over the child gets a short grace and
// then SIGKILL to its whole group: a tool it left running (a dev server
// started from a Bash call) must not keep the turn open forever.
type CLIRun struct {
	events chan Event
	cmd    *exec.Cmd // nil for an adopted run
	pid    int
	stream string
	temp   bool // stream is ours to remove
	stop   chan struct{}
	once   sync.Once
	exited chan struct{}
	exitMu sync.Mutex
	exitEr error
}

// Parser turns one stdout line into zero or more events and says whether
// the line was the run's final one.
type Parser interface {
	Line(line string) (evs []Event, done bool)
	// Flush emits what a stream that ended without a final line still
	// holds (a json output format produces a single document at exit).
	Flush() []Event
}

// SpawnAttr, when set, gives the process attributes an agent turn starts
// with (the wiring sets it to start turns inside the agents' cgroup). It
// returns a release for whatever it opened, called once Start returns.
var SpawnAttr func() (*syscall.SysProcAttr, func(), error)

// StartCLI launches bin with args in dir, its output to a temporary file.
func StartCLI(ctx context.Context, p Parser, dir string, env []string, bin string, args ...string) (*CLIRun, error) {
	return StartCLIStream(ctx, p, "", dir, env, bin, args...)
}

// StartCLIStream is StartCLI with the output in stream, which survives
// this process: what AdoptCLI reads after a restart. stream's .err file
// beside it holds stderr.
func StartCLIStream(ctx context.Context, p Parser, stream, dir string, env []string, bin string, args ...string) (*CLIRun, error) {
	return startCLI(ctx, p, stream, dir, env, nil, "", bin, args...)
}

// MaxArg is the longest single argument or environment string a command
// may carry. The kernel refuses a longer one whatever the total is
// (MAX_ARG_STRLEN, 32 pages: 128 KiB with the terminating byte on 4 KiB
// pages), and execve then fails with E2BIG for the whole command.
const MaxArg = 128*1024 - 1

// ErrTooLong is a turn whose command line the kernel would not take. It
// is the same answer however often it is asked: nothing that starts turns
// may try it again as it is.
var ErrTooLong = errors.New("too long for a command line")

// fits says the kernel will take args and env, string by string. The
// total has its own limit (a quarter of the stack's), which only the
// kernel knows for sure: that one comes back from Start as E2BIG.
func fits(args, env []string) error {
	for _, a := range args {
		if len(a) > MaxArg {
			return fmt.Errorf("an argument of %d KiB: %w (the limit is %d KiB each)", len(a)/1024, ErrTooLong, (MaxArg+1)/1024)
		}
	}
	for _, e := range env {
		if len(e) > MaxArg {
			name, _, _ := strings.Cut(e, "=")
			return fmt.Errorf("the variable %s of %d KiB: %w (the limit is %d KiB each)", name, len(e)/1024, ErrTooLong, (MaxArg+1)/1024)
		}
	}
	return nil
}

// Enclose, when set, puts a site's turn in the site's jail (internal/jail)
// before it starts: what Jail names to the CLI as a rule becomes what the
// turn's processes can see at all. Set by the wiring when the machine can
// build jails. It decides by the jail: the house's own conversation (its
// folder is the root) is the operator's and is left as it is. bin is the
// CLI itself, which cmd may start through setpriv.
var Enclose func(cmd *exec.Cmd, j Jail, bin string) error

// Start is StartCLIStream for a turn run in j: enclosed, when the house
// encloses turns.
func (j Jail) Start(ctx context.Context, p Parser, stream, dir string, env []string, bin string, args ...string) (*CLIRun, error) {
	return startCLI(ctx, p, stream, dir, env, &j, "", bin, args...)
}

// StartInput is Start for a CLI that reads the message on its standard
// input: input is what it reads there. A message is as long as whoever
// wrote it made it, and one of 128 KiB as an argument is one the kernel
// refuses to start at all (a site's code sent its conversation a book
// and every call came back "fork/exec /proc/self/exe: argument list too
// long").
func (j Jail) StartInput(ctx context.Context, p Parser, stream, dir string, env []string, input, bin string, args ...string) (*CLIRun, error) {
	return startCLI(ctx, p, stream, dir, env, &j, input, bin, args...)
}

func startCLI(ctx context.Context, p Parser, stream, dir string, env []string, j *Jail, input, bin string, args ...string) (*CLIRun, error) {
	path, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", bin, err)
	}
	temp := false
	if stream == "" {
		f, err := os.CreateTemp("", "agent-*.out")
		if err != nil {
			return nil, err
		}
		f.Close()
		stream, temp = f.Name(), true
	}
	if err := os.MkdirAll(filepath.Dir(stream), 0o700); err != nil {
		return nil, err
	}
	out, err := os.OpenFile(stream, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	defer out.Close()
	errf, err := os.OpenFile(stream+".err", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	defer errf.Close()
	cli := path
	path, args = withoutAmbientCaps(path, args)
	cmd := exec.Command(path, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = nil
	if input != "" {
		// A file, as the output is, and not a pipe: nothing here has to
		// stay to write it, so a supervisor that restarts while the CLI
		// is still reading takes nothing from it. The descriptor is
		// opened here and inherited (through setpriv and the jail's
		// init), so the file needs no place inside the jail; removed
		// once the child holds it, it lasts exactly as long as the turn.
		in := stream + ".in"
		if err := os.WriteFile(in, []byte(input), 0o600); err != nil {
			return nil, err
		}
		defer os.Remove(in)
		inf, err := os.Open(in)
		if err != nil {
			return nil, err
		}
		defer inf.Close()
		cmd.Stdin = inf
	}
	// The files themselves, never a writer: Wait would then hang on a
	// grandchild that keeps the descriptor.
	cmd.Stdout, cmd.Stderr = out, errf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	release := func() {}
	if SpawnAttr != nil {
		if attr, rel, err := SpawnAttr(); err == nil && attr != nil {
			cmd.SysProcAttr, release = attr, rel
		}
	}
	defer release()
	if j != nil && Enclose != nil {
		if err := Enclose(cmd, *j, cli); err != nil {
			return nil, err
		}
	}
	// Asked before the kernel is: its own refusal names the binary that
	// was to run ("fork/exec /proc/self/exe") and not what was wrong.
	if err := fits(cmd.Args, cmd.Env); err != nil {
		return nil, fmt.Errorf("%s: %w", bin, err)
	}
	if err := cmd.Start(); err != nil {
		if errors.Is(err, syscall.E2BIG) {
			return nil, fmt.Errorf("%s: the command line and environment together: %w", bin, ErrTooLong)
		}
		return nil, err
	}
	r := &CLIRun{events: make(chan Event, 64), cmd: cmd, pid: cmd.Process.Pid, stream: stream, temp: temp, stop: make(chan struct{}), exited: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		r.exitMu.Lock()
		r.exitEr = err
		r.exitMu.Unlock()
		close(r.exited)
	}()
	go r.loop(ctx, p)
	return r, nil
}

// AdoptCLI picks up a run a previous supervisor started: the process if
// it is still alive (pid checked against the stream it writes to), and in
// any case its stream from the beginning, so a turn that finished while
// nobody was listening is still read to its result.
func AdoptCLI(ctx context.Context, p Parser, pid int, stream string) *CLIRun {
	r := &CLIRun{events: make(chan Event, 64), pid: pid, stream: stream, stop: make(chan struct{}), exited: make(chan struct{})}
	if !writesTo(pid, stream) {
		close(r.exited)
	} else {
		go func() {
			for syscall.Kill(pid, 0) == nil && !zombie(pid) {
				time.Sleep(time.Second)
			}
			close(r.exited)
		}()
	}
	go r.loop(ctx, p)
	return r
}

// writesTo says pid is alive and has stream open: a reused pid is not.
func writesTo(pid int, stream string) bool {
	if pid <= 0 || syscall.Kill(pid, 0) != nil || zombie(pid) {
		return false
	}
	fds, err := os.ReadDir("/proc/" + strconv.Itoa(pid) + "/fd")
	if err != nil {
		return false
	}
	for _, fd := range fds {
		if target, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/fd/" + fd.Name()); err == nil && target == stream {
			return true
		}
	}
	return false
}

func zombie(pid int) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	i := bytes.LastIndexByte(data, ')')
	return i < 0 || i+2 >= len(data) || data[i+2] == 'Z'
}

// Events implements Run.
func (r *CLIRun) Events() <-chan Event { return r.events }

// Stop implements Run.
func (r *CLIRun) Stop() { r.once.Do(func() { close(r.stop) }) }

// PID is the process group leader of the run.
func (r *CLIRun) PID() int { return r.pid }

// Stream is the file the run's output goes to.
func (r *CLIRun) Stream() string { return r.stream }

func (r *CLIRun) stderrText() string {
	data, _ := os.ReadFile(r.stream + ".err")
	if len(data) > 16*1024 {
		data = data[len(data)-16*1024:]
	}
	return strings.TrimSpace(string(data))
}

// tail sends the stream's complete lines until the process has exited and
// everything it wrote was read, or quit closes.
func (r *CLIRun) tail(lines chan<- string, quit <-chan struct{}) {
	defer close(lines)
	f, err := os.Open(r.stream)
	if err != nil {
		return
	}
	defer f.Close()
	rd := bufio.NewReaderSize(f, 64*1024)
	var partial strings.Builder
	for {
		chunk, err := rd.ReadString('\n')
		partial.WriteString(chunk)
		if err == nil {
			line := strings.TrimRight(partial.String(), "\n")
			partial.Reset()
			select {
			case lines <- line:
			case <-quit:
				return
			}
			continue
		}
		if !errors.Is(err, io.EOF) {
			return
		}
		select {
		case <-r.exited:
			// One more pass for what was written between the last read
			// and the exit, then the unterminated rest.
			for {
				chunk, err := rd.ReadString('\n')
				partial.WriteString(chunk)
				if err != nil {
					break
				}
				select {
				case lines <- strings.TrimRight(partial.String(), "\n"):
				case <-quit:
					return
				}
				partial.Reset()
			}
			if s := strings.TrimSpace(partial.String()); s != "" {
				select {
				case lines <- s:
				case <-quit:
				}
			}
			return
		case <-quit:
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (r *CLIRun) loop(ctx context.Context, p Parser) {
	defer close(r.events)
	defer func() {
		if r.temp {
			os.Remove(r.stream)
			os.Remove(r.stream + ".err")
		}
	}()
	lines := make(chan string, 16)
	quit := make(chan struct{})
	defer close(quit)
	go r.tail(lines, quit)
	sawEnd, stopped := false, false
	emit := func(evs []Event) {
		for _, ev := range evs {
			if ev.Kind == KindResult || ev.Kind == KindError {
				sawEnd = true
			}
			r.send(ev)
		}
	}
read:
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				break read
			}
			evs, done := p.Line(line)
			emit(evs)
			if done {
				break read
			}
		case <-r.stop:
			stopped = true
			break read
		case <-ctx.Done():
			stopped = true
			break read
		}
	}
	if !stopped {
		emit(p.Flush())
	}
	exitErr := r.end(stopped)
	if stopped {
		r.send(Event{Kind: KindError, Text: "stopped"})
		return
	}
	if !sawEnd {
		text := "agent exited without a result"
		if exitErr != nil {
			text = exitErr.Error()
		}
		if se := r.stderrText(); se != "" {
			text += ": " + se
		}
		r.send(Event{Kind: KindError, Text: text})
	}
}

// end lets the child exit: three seconds after a clean end, at once after
// a stop. Either way the whole group goes, and nothing here waits on a
// grandchild: the output is a file, not a pipe.
func (r *CLIRun) end(stopped bool) error {
	grace := 3 * time.Second
	if stopped {
		_ = syscall.Kill(-r.pid, syscall.SIGTERM)
		grace = 2 * time.Second
	}
	select {
	case <-r.exited:
	case <-time.After(grace):
		_ = syscall.Kill(-r.pid, syscall.SIGKILL)
		select {
		case <-r.exited:
		case <-time.After(5 * time.Second):
			return errors.New("agent process survived SIGKILL")
		}
	}
	// Whatever it left in its group goes too.
	_ = syscall.Kill(-r.pid, syscall.SIGKILL)
	r.exitMu.Lock()
	defer r.exitMu.Unlock()
	return r.exitEr
}

func (r *CLIRun) send(ev Event) {
	select {
	case r.events <- ev:
	case <-time.After(30 * time.Second):
		// A consumer that stopped reading must not pin the process: the
		// event is dropped and the run goes on to its end.
	}
}

// The supervisor runs with CAP_NET_BIND_SERVICE as an ambient capability
// (the unit's AmbientCapabilities), and ambient capabilities pass to every
// child across exec. An agent needs none of it, and one of them breaks on
// it: Codex sandboxes its shell with bubblewrap, which refuses to start
// when it holds capabilities without being setuid ("bwrap: Unexpected
// capabilities but not setuid"), so every command of a Codex turn failed
// on a server installed with the unit while Claude, which has no such
// sandbox, worked. Go can only raise ambient capabilities in a child, not
// clear them, so the turn starts through setpriv, which drops them and
// execs the agent in the same process.

// procStatus is /proc/self/status; tests point it elsewhere.
var procStatus = "/proc/self/status"

// setprivPaths are where util-linux puts setpriv.
var setprivPaths = []string{"/usr/bin/setpriv", "/bin/setpriv"}

// hasAmbientCaps says this process holds ambient capabilities.
func hasAmbientCaps() bool {
	raw, err := os.ReadFile(procStatus)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(line, "CapAmb:"); ok {
			return strings.Trim(strings.TrimSpace(v), "0") != ""
		}
	}
	return false
}

// withoutAmbientCaps is the command line that starts path without them:
// itself when there are none, or when setpriv is not there (the turn then
// runs as before, and an agent that minds says so in its own words).
func withoutAmbientCaps(path string, args []string) (string, []string) {
	if !hasAmbientCaps() {
		return path, args
	}
	for _, sp := range setprivPaths {
		if info, err := os.Stat(sp); err == nil && !info.IsDir() {
			return sp, append([]string{"--ambient-caps", "-all", "--inh-caps", "-all", "--", path}, args...)
		}
	}
	return path, args
}
