package chat

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dcimorra/the-middleware/internal/manifest"
)

// MarkVar is put in every turn's environment, and everything a turn
// starts inherits it, also what escapes the turn's process group with
// setsid on purpose (a build, a dev server, as the skill tells agents to
// do for long work). The group kill at the end of a turn cannot reach
// those; the mark is how the house still finds them. V1 learned it from
// luma32.com, whose agents left headless browsers and test servers running
// for days, one of them on more than a core: the site could not clean them
// up because it runs in a container and does not see them.
const MarkVar = "MIDDLEWARE_CHAT_ID"

// TurnLimit cuts a turn that runs this long; zero, the default, never
// cuts one. A two-hour cut lost the long jobs operators leave running
// overnight; a turn stuck in a loop is stopped from the panel, and what it
// left running is listed under its processes either way.
var TurnLimit time.Duration

// Proc is a process a conversation left running.
type Proc struct {
	PID     int       `json:"pid"`
	Command string    `json:"command"`
	Started time.Time `json:"started,omitempty"`
}

// Processes lists what the conversation of domain has running now: the
// turn itself and whatever it left behind.
func (m *Manager) Processes(domain string) []Proc {
	return marked(m.procRoot(), []byte(MarkVar+"="+domain+"\x00"))
}

// Reap ends the turn of domain if one runs and kills everything its
// conversation left alive. It returns how many processes were signalled.
func (m *Manager) Reap(domain string) int {
	_ = m.Stop(domain)
	return reap(m.procRoot(), []byte(MarkVar+"="+domain+"\x00"), 0)
}

// Kill ends one process the conversation of domain left, and refuses any
// other: the permission is checked by the house, never by the caller.
func (m *Manager) Kill(domain string, pid int) bool {
	return reap(m.procRoot(), []byte(MarkVar+"="+domain+"\x00"), pid) > 0
}

func (m *Manager) procRoot() string {
	if m.opts.ProcRoot != "" {
		return m.opts.ProcRoot
	}
	return "/proc"
}

// marked are this uid's processes whose environment carries mark.
func marked(procRoot string, mark []byte) []Proc {
	ents, err := os.ReadDir(procRoot)
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var out []Proc
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		env, err := os.ReadFile(filepath.Join(procRoot, e.Name(), "environ"))
		if err != nil || !bytes.Contains(append(env, 0), mark) {
			continue
		}
		cmd, _ := os.ReadFile(filepath.Join(procRoot, e.Name(), "cmdline"))
		p := Proc{PID: pid, Command: strings.TrimSpace(strings.ReplaceAll(string(cmd), "\x00", " "))}
		if st, err := os.Stat(filepath.Join(procRoot, e.Name())); err == nil {
			p.Started = st.ModTime()
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}

// reap sends SIGTERM to every marked process (only to `only` when it is
// not 0), and SIGKILL to what is left after a grace. A group leader gets
// it on the whole group, which is where what it started hangs.
func reap(procRoot string, mark []byte, only int) int {
	var pids []int
	for _, p := range marked(procRoot, mark) {
		if only == 0 || p.PID == only {
			pids = append(pids, p.PID)
		}
	}
	signal := func(sig syscall.Signal, pid int) {
		if pgid, err := syscall.Getpgid(pid); err == nil && pgid == pid {
			_ = syscall.Kill(-pid, sig)
		}
		_ = syscall.Kill(pid, sig)
	}
	for _, pid := range pids {
		signal(syscall.SIGTERM, pid)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && anyAlive(pids) {
		time.Sleep(100 * time.Millisecond)
	}
	for _, pid := range pids {
		if syscall.Kill(pid, 0) == nil {
			signal(syscall.SIGKILL, pid)
		}
	}
	return len(pids)
}

func anyAlive(pids []int) bool {
	for _, pid := range pids {
		if syscall.Kill(pid, 0) == nil && !zombie(pid) {
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

// A site's turn runs as the supervisor's own user, and the middleware
// binary is in its PATH: whatever it starts can open data/api.sock, which
// trusted every connection as the operator. A site's conversation (or a
// prompt injected into it through a page it read) could mint itself a
// full token or read another site's secrets with one command. SiteOfProcess
// is how the house tells such a caller from the operator: the process and
// its ancestors, each checked against the turns running now (turns, pid
// to conversation) and against the mark every turn's environment carries.
//
// Both, because each alone has a way round. A process can scrub the mark
// from its own environment, but not from its ancestors', which keep the
// environment they were started with; and a process detached with setsid
// is handed to init and leaves the chain, but keeps the mark unless it
// also scrubs it. Any site found anywhere counts, and a house mark never
// lifts one: a turn that sets MIDDLEWARE_CHAT_ID=middleware for its child
// still has its own turn above it.
//
// The answer is the site, or "" for a caller that no site's turn started
// (the operator, the house's own conversation). An error says the caller
// cannot be told: the process is gone before it was looked at (a client
// can connect and exit, leaving the connection to a child), or the chain
// names two sites, which only a forged mark does. The caller refuses it.
func SiteOfProcess(procRoot string, pid int, turns map[int]string) (string, error) {
	site := ""
	note := func(name string) error {
		domain, _ := split(name)
		if domain == Middleware || (domain != Autopilot && !manifest.ValidName(domain)) {
			return nil
		}
		if site != "" && site != domain {
			return fmt.Errorf("process %d runs under the turns of both %s and %s", pid, site, domain)
		}
		site = domain
		return nil
	}
	mark := []byte(MarkVar + "=")
	p := pid
	for depth := 0; p > 0 && depth < 256; depth++ {
		parent, err := parentOf(procRoot, p)
		if err != nil {
			if p == pid {
				return "", err
			}
			break
		}
		if name, ok := turns[p]; ok {
			if err := note(name); err != nil {
				return "", err
			}
		}
		// Another user's environment is not readable, and need not be: a
		// turn runs as this one.
		if env, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(p), "environ")); err == nil {
			for _, kv := range bytes.Split(env, []byte{0}) {
				if v, ok := bytes.CutPrefix(kv, mark); ok {
					if err := note(string(v)); err != nil {
						return "", err
					}
				}
			}
		}
		if parent == p {
			break
		}
		p = parent
	}
	return site, nil
}

// errNoProcess says a pid has no readable stat.
var errNoProcess = errors.New("no such process")

// parentOf reads a process's parent from its stat: the field after the
// state, which follows the last ')' (a command name may hold one).
func parentOf(procRoot string, pid int) (int, error) {
	data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, fmt.Errorf("process %d: %w", pid, errNoProcess)
	}
	i := bytes.LastIndexByte(data, ')')
	if i < 0 {
		return 0, fmt.Errorf("process %d: %w", pid, errNoProcess)
	}
	f := strings.Fields(string(data[i+1:]))
	if len(f) < 2 {
		return 0, fmt.Errorf("process %d: %w", pid, errNoProcess)
	}
	return strconv.Atoi(f[1])
}

// TurnPIDs are the agent CLIs of the turns running now, by the name of
// their conversation.
func (m *Manager) TurnPIDs() map[int]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[int]string{}
	for name, r := range m.running {
		if pr, ok := r.run.(interface{ PID() int }); ok && pr.PID() > 1 {
			out[pr.PID()] = name
		}
	}
	return out
}

// SiteOf is SiteOfProcess with the turns this manager runs now.
func (m *Manager) SiteOf(pid int) (string, error) {
	return SiteOfProcess(m.procRoot(), pid, m.TurnPIDs())
}
