package installer

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Exec is every way the installer touches the machine. The real one shells
// out; tests use Recorder and assert the exact sequence, so the thing that
// decides whether to run `groupadd` is tested without a root shell.
type Exec interface {
	// Run runs a command as the current user and returns combined output.
	Run(name string, args ...string) (string, error)
	// Sudo runs a command as root. The real implementation prefixes sudo;
	// the installer never calls it for anything that a user can do.
	Sudo(name string, args ...string) (string, error)
	// LookPath says whether a program is on PATH.
	LookPath(name string) (string, error)
	// HasLib says whether the dynamic loader finds a shared library
	// (libatomic.so.1) in the system's library folders.
	HasLib(name string) bool
	// WriteRoot puts a file where only root can write, with the given mode.
	// Comparing before writing is the caller's job.
	WriteRoot(path string, data []byte, perm os.FileMode) error
	// RemoveRoot deletes a root-owned file; a missing file is not an error.
	RemoveRoot(path string) error
}

// System is the Exec that touches the real machine.
type System struct{}

func (System) Run(name string, args ...string) (string, error) {
	return run(exec.Command(name, args...))
}

func (System) Sudo(name string, args ...string) (string, error) {
	if os.Geteuid() == 0 {
		return run(exec.Command(name, args...))
	}
	return run(exec.Command("sudo", append([]string{name}, args...)...))
}

func (System) LookPath(name string) (string, error) { return exec.LookPath(name) }

// libDirs are where distributions put shared libraries: the multiarch
// folders (Debian, Ubuntu) by glob, the plain ones (Fedora, Alpine, Arch)
// by name. /lib is a link to usr/lib on a merged /usr; both are looked at.
var libDirs = []string{"/lib", "/lib64", "/usr/lib", "/usr/lib64", "/lib/*-linux-*", "/usr/lib/*-linux-*"}

func (System) HasLib(name string) bool {
	for _, d := range libDirs {
		if m, _ := filepath.Glob(filepath.Join(d, name)); len(m) > 0 {
			return true
		}
	}
	return false
}

// WriteRoot stages the file in a temp dir the user owns and lets install(1)
// move it: `sudo tee` would need a shell and a pipe, and `sudo cp` would
// keep the temp file's mode.
func (s System) WriteRoot(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.MkdirTemp("", "middleware-install-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	staged := filepath.Join(tmp, filepath.Base(path))
	if err := os.WriteFile(staged, data, 0o644); err != nil {
		return err
	}
	_, err = s.Sudo("install", "-D", "-m", fmt.Sprintf("%04o", perm), staged, path)
	return err
}

func (s System) RemoveRoot(path string) error {
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return nil
	}
	_, err := s.Sudo("rm", "-f", path)
	return err
}

func run(cmd *exec.Cmd) (string, error) {
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	// sudo may need to ask for a password; it does so on the controlling
	// terminal, which it finds through stdin.
	cmd.Stdin = os.Stdin
	err := cmd.Run()
	s := strings.TrimSpace(out.String())
	if err != nil {
		if s != "" {
			return s, fmt.Errorf("%s: %w: %s", cmd.Args[0], err, s)
		}
		return s, fmt.Errorf("%s: %w", cmd.Args[0], err)
	}
	return s, nil
}

// Recorder is the Exec tests use. It records every call, answers from a
// table of canned results, and performs root writes into a directory the
// test owns so a second run sees what the first one wrote.
type Recorder struct {
	mu sync.Mutex
	// Calls is every call in order, formatted as "run name args",
	// "sudo name args", "write path" or "remove path".
	Calls []string
	// Results maps a formatted command (without the run/sudo prefix) to
	// what it should return. Missing entries succeed with empty output.
	Results map[string]Result
	// Path is what LookPath knows about; programs not in it are missing.
	Path map[string]bool
	// Fail makes WriteRoot of this path fail, to test the ✗ line.
	Fail map[string]error
	// NoLibs are the shared libraries the machine lacks; every other one
	// is there, so a test about something else never installs a library.
	NoLibs map[string]bool
	// SplitDockerCLI is a distribution whose docker.io has no `docker`
	// command (Debian 12: it is in docker-cli).
	SplitDockerCLI bool

	// A thin model of the machine, so a second run against the same
	// recorder sees what the first one did: groups created, units enabled
	// and started. Results entries override it.
	groups     map[string]bool
	userGroups map[string][]string
	enabled    map[string]bool
	active     map[string]bool
}

// Result is a canned command outcome.
type Result struct {
	Out string
	Err error
}

func NewRecorder() *Recorder {
	return &Recorder{
		Results: map[string]Result{}, Path: map[string]bool{}, Fail: map[string]error{},
		groups: map[string]bool{}, userGroups: map[string][]string{}, enabled: map[string]bool{}, active: map[string]bool{},
	}
}

func isPackageManager(name string) bool {
	for _, pm := range packageManagers {
		if pm.bin == name {
			return true
		}
	}
	return false
}

// errExit is what a command that "fails" in the model returns.
var errExit = errors.New("exit status 1")

// simulate answers the checks from the model and applies the mutations to
// it. ok is false when the model has nothing to say and the Results table
// (or the default empty success) decides.
func (r *Recorder) simulate(sudo bool, name string, args []string) (out string, err error, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case name == "getent" && len(args) == 2 && args[0] == "group":
		if r.groups[args[1]] {
			return args[1] + ":x:999:", nil, true
		}
		return "", errExit, true
	case name == "id" && len(args) == 2 && args[0] == "-nG":
		gs := r.userGroups[args[1]]
		return strings.Join(append([]string{args[1]}, gs...), " "), nil, true
	case sudo && isPackageManager(name):
		for _, a := range args {
			if !strings.HasPrefix(a, "-") && a != "install" && a != "add" {
				r.Path[a] = true
				// The docker engine's packages give the docker command,
				// except where the distribution splits it out (Debian 12).
				if (a == "docker.io" && !r.SplitDockerCLI) || a == "moby-engine" || a == "docker-cli" {
					r.Path["docker"] = true
				}
				if a == "fail2ban" {
					r.Path["fail2ban-client"] = true
				}
			}
		}
		return "", nil, true
	case name == "docker" && len(args) > 0 && args[0] == "version":
		if r.active["docker"] {
			return "27.0.0", nil, true
		}
		return "", errExit, true
	case name == "systemctl" && sudo && len(args) == 3 && args[0] == "enable" && args[1] == "--now":
		r.enabled[args[2]], r.active[args[2]] = true, true
		return "", nil, true
	case name == "groupadd" && sudo:
		r.groups[args[len(args)-1]] = true
		return "", nil, true
	case name == "usermod" && sudo && len(args) == 3 && args[0] == "-aG":
		r.userGroups[args[2]] = append(r.userGroups[args[2]], args[1])
		return "", nil, true
	case name == "systemctl" && len(args) == 2:
		u := args[1]
		switch args[0] {
		case "is-enabled":
			if r.enabled[u] {
				return "enabled", nil, true
			}
			return "disabled", errExit, true
		case "is-active":
			if r.active[u] {
				return "active", nil, true
			}
			return "inactive", errExit, true
		case "enable":
			r.enabled[u] = true
			return "", nil, true
		case "disable":
			r.enabled[u] = false
			return "", nil, true
		case "start", "restart":
			r.active[u] = true
			return "", nil, true
		case "stop":
			r.active[u] = false
			return "", nil, true
		}
	}
	return "", nil, false
}

func (r *Recorder) record(s string) {
	r.mu.Lock()
	r.Calls = append(r.Calls, s)
	r.mu.Unlock()
}

func (r *Recorder) answer(key string) (string, error) {
	r.mu.Lock()
	res, ok := r.Results[key]
	r.mu.Unlock()
	if !ok {
		return "", nil
	}
	return res.Out, res.Err
}

// Set makes a command answer with out and err from now on.
func (r *Recorder) Set(cmdline string, out string, err error) {
	r.mu.Lock()
	r.Results[cmdline] = Result{Out: out, Err: err}
	r.mu.Unlock()
}

func join(name string, args []string) string {
	return strings.TrimSpace(name + " " + strings.Join(args, " "))
}

func (r *Recorder) Run(name string, args ...string) (string, error) {
	k := join(name, args)
	r.record("run " + k)
	return r.answerOrSimulate(false, k, name, args)
}

func (r *Recorder) Sudo(name string, args ...string) (string, error) {
	k := join(name, args)
	r.record("sudo " + k)
	return r.answerOrSimulate(true, k, name, args)
}

func (r *Recorder) answerOrSimulate(sudo bool, key, name string, args []string) (string, error) {
	r.mu.Lock()
	res, canned := r.Results[key]
	r.mu.Unlock()
	if canned {
		return res.Out, res.Err
	}
	if out, err, ok := r.simulate(sudo, name, args); ok {
		return out, err
	}
	return r.answer(key)
}

func (r *Recorder) LookPath(name string) (string, error) {
	r.mu.Lock()
	ok := r.Path[name]
	r.mu.Unlock()
	if ok {
		return "/usr/bin/" + name, nil
	}
	return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
}

func (r *Recorder) HasLib(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.NoLibs[name]
}

func (r *Recorder) WriteRoot(path string, data []byte, perm os.FileMode) error {
	r.record("write " + path)
	r.mu.Lock()
	err := r.Fail[path]
	r.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, perm)
}

func (r *Recorder) RemoveRoot(path string) error {
	r.record("remove " + path)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Mutations returns the recorded calls that change the machine: everything
// but the checks the installer runs to decide whether a step is needed.
func (r *Recorder) Mutations() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, c := range r.Calls {
		if isCheck(c) {
			continue
		}
		out = append(out, c)
	}
	return out
}

func isCheck(call string) bool {
	for _, p := range []string{"run getent ", "run id ", "run systemctl is-", "run systemctl show", "run sysctl -n", "run docker version", "run docker buildx version", "run wg ", "sudo sysctl -n"} {
		if strings.HasPrefix(call, p) {
			return true
		}
	}
	return false
}
