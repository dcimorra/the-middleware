package helper

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Exec runs one of the known binaries. The real one shells out; tests use
// a Recorder and assert the exact command lines, which is the only way to
// test a root-only path on a machine where nobody is root.
type Exec interface {
	// Run executes name with args, feeding stdin, and returns the combined
	// output. name is a logical name ("nft", "wg", "wg-quick", "sysctl"),
	// never a path: the real Exec resolves it once at start.
	Run(ctx context.Context, name string, args []string, stdin string) (string, error)
}

// Binaries the helper may run. A name outside this list is refused by
// SystemExec even if an operation asked for it, so a bug in an operation
// cannot widen what the helper executes.
var Binaries = []string{"nft", "wg", "wg-quick", "sysctl", "systemd-run", "systemctl", "iptables", "ip6tables"}

// SystemExec runs the real binaries with a timeout.
type SystemExec struct {
	Timeout time.Duration
	// LockDir is a folder the helper can write, for iptables' lock when
	// its own place cannot be (see Run); empty never moves it.
	LockDir string
	paths   map[string]string
	missing map[string]error
	once    sync.Once
}

// resolve looks the binaries up once. Missing ones are not an error until
// an operation needs them: a machine without WireGuard still needs the
// firewall operations.
func (e *SystemExec) resolve() {
	e.paths = map[string]string{}
	e.missing = map[string]error{}
	for _, name := range Binaries {
		p, err := exec.LookPath(name)
		if err != nil {
			e.missing[name] = fmt.Errorf("%s is not installed", name)
			continue
		}
		e.paths[name] = p
	}
}

// Run implements Exec. Output is captured, never streamed: every command the
// helper runs finishes in well under a second or is broken.
func (e *SystemExec) Run(ctx context.Context, name string, args []string, stdin string) (string, error) {
	e.once.Do(e.resolve)
	path, ok := e.paths[name]
	if !ok {
		if err := e.missing[name]; err != nil {
			return "", err
		}
		return "", fmt.Errorf("%s is not a binary the helper runs", name)
	}
	timeout := e.Timeout
	if timeout == 0 {
		timeout = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var out bytes.Buffer
	run := func(env []string) error {
		out.Reset()
		cmd := exec.CommandContext(ctx, path, args...)
		if stdin != "" {
			cmd.Stdin = strings.NewReader(stdin)
		}
		cmd.Env = env
		cmd.Stdout = &out
		cmd.Stderr = &out
		return cmd.Run()
	}
	err := run(nil)
	// The legacy iptables takes a lock in /run, which the helper's unit
	// mounts read-only (every unit installed before ports.open has no
	// line for it, and an update does not rewrite units). Its lock is
	// then one of the helper's own: two rules of the helper's never race,
	// and the kernel swaps a table whole either way.
	if err != nil && ctx.Err() == nil && e.LockDir != "" && (name == "iptables" || name == "ip6tables") && strings.Contains(out.String(), "xtables.lock") {
		err = run(append(os.Environ(), "XTABLES_LOCKFILE="+filepath.Join(e.LockDir, "xtables.lock")))
	}
	if ctx.Err() != nil {
		return out.String(), fmt.Errorf("%s timed out after %s", name, timeout)
	}
	if err != nil {
		msg := strings.TrimSpace(out.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.String(), fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), msg)
	}
	return out.String(), nil
}

// Call is one recorded invocation.
type Call struct {
	Name  string
	Args  []string
	Stdin string
}

// Line is the command as one string, for assertions.
func (c Call) Line() string { return strings.TrimSpace(c.Name + " " + strings.Join(c.Args, " ")) }

// Recorder is the Exec tests use. Outputs maps a command line (Call.Line)
// to what it prints; Fail maps a command line to the error it returns.
type Recorder struct {
	mu      sync.Mutex
	Calls   []Call
	Outputs map[string]string
	Fail    map[string]error
}

// Run implements Exec.
func (r *Recorder) Run(_ context.Context, name string, args []string, stdin string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := Call{Name: name, Args: append([]string(nil), args...), Stdin: stdin}
	r.Calls = append(r.Calls, c)
	if err, ok := r.Fail[c.Line()]; ok {
		return "", err
	}
	return r.Outputs[c.Line()], nil
}

// Lines returns every recorded command line in order.
func (r *Recorder) Lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.Calls))
	for i, c := range r.Calls {
		out[i] = c.Line()
	}
	return out
}

// Last returns the last call, or an error when nothing ran.
func (r *Recorder) Last() (Call, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.Calls) == 0 {
		return Call{}, errors.New("nothing was executed")
	}
	return r.Calls[len(r.Calls)-1], nil
}

// Reset forgets the calls so far.
func (r *Recorder) Reset() {
	r.mu.Lock()
	r.Calls = nil
	r.mu.Unlock()
}
