// Package cgroups keeps the agents' work from competing with the sites.
//
// On 16 October 2026 V1's machine ran out of air with three headless
// Chromiums at once, started by one site's conversations to test its games:
// each rendered WebGL in software and its GPU process ate one to two cores.
// The site was in a container with limits; its conversations were not: they
// ran on the host as children of the supervisor, and what they started
// competed with the sites as an equal. The limits were on the site, and the
// cost was born in its conversation.
//
// So the unit's cgroup is split in two siblings:
//
//	middleware.service/
//	├── house/   the supervisor, the sites and the tasks
//	└── agents/  every agent turn and whatever it starts
//
// and agents get a low CPU weight and a ceiling. A weight is not a cap: with
// the machine idle they use every core as before, and only when a site asks
// for CPU does the kernel serve it first. Turns start inside agents/
// directly (clone into the cgroup), so nothing a turn starts is ever
// briefly outside it.
//
// It needs Delegate=cpu memory in the unit: without delegation the cgroup
// is root's and the supervisor cannot create a folder in it, and everything
// runs as before, said once at start. DelegateSubgroup=house (systemd 254
// or newer) has systemd start the supervisor in house/ already; an older
// systemd ignores that line and starts it in the unit's own cgroup, so the
// supervisor makes house/ and moves itself and what it started there
// first, which is all DelegateSubgroup does (Debian 12 has systemd 252,
// and its first node said "not delegated" with Delegate= in place).
package cgroups

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// The branches. House must match the unit's DelegateSubgroup=: it is where
// systemd puts the supervisor at start.
const (
	HouseName  = "house"
	AgentsName = "agents"
	// UnitName is the house's service, whose cgroup an older systemd
	// starts it in.
	UnitName = "middleware.service"
)

// Limits are what the agents' branch gets.
type Limits struct {
	// CPUWeight against the house's 100: with both asking, agents get a
	// small share.
	CPUWeight int
	// CPUCores caps agents in cores. The weight shares under contention,
	// but the scheduler takes a while to turn and a site feels it as
	// latency spikes; with a core that is never theirs, it does not.
	CPUCores int
	// MemoryHigh is where the kernel starts squeezing (reclaiming,
	// slowing) without killing anyone.
	MemoryHigh int64
	// MemoryMax is the net: past it the kernel kills, inside this branch,
	// whatever holds the most, which is a browser. On a machine without
	// swap the alternative is a site dying.
	MemoryMax int64
}

// Machine is what the defaults are sized from: logical CPUs and memory.
type Machine struct {
	CPUs   int
	Memory int64
}

var (
	thisOnce sync.Once
	this     Machine
)

// ThisMachine reads the machine once: neither number moves while the
// process lives.
func ThisMachine() Machine {
	thisOnce.Do(func() { this = Machine{CPUs: runtime.NumCPU(), Memory: totalMemory()} })
	return this
}

// DefaultLimits are this machine's.
func DefaultLimits() Limits { return ThisMachine().DefaultLimits() }

// DefaultLimits keeps a quarter of the cores (at least one) out of the
// agents' reach, and a third of the memory as their ceiling. The reserve
// was one core: on two cores that kept half for the sites, on twelve
// threads 8 %, while the reason for it (the weight turns slowly and a
// site feels it as latency) grows with the number of sites the machine
// holds.
func (m Machine) DefaultLimits() Limits {
	reserve := m.CPUs / 4
	if reserve < 1 {
		reserve = 1
	}
	cores := m.CPUs - reserve
	if cores < 1 {
		cores = 1
	}
	l := Limits{CPUWeight: 10, CPUCores: cores}
	if m.Memory > 0 {
		l.MemoryMax = m.Memory / 3
		l.MemoryHigh = m.Memory / 4
	}
	return l
}

// ErrNotDelegated is a unit without delegation.
var ErrNotDelegated = errors.New("the supervisor's cgroup is not delegated (the unit needs Delegate=cpu memory: run middleware install again); agent turns share the sites' CPU and memory")

// Tree is the split, once made.
type Tree struct {
	agents string
}

// Root is where the unified hierarchy is mounted; tests use a folder.
var Root = "/sys/fs/cgroup"

// Setup splits the unit's cgroup. self is this process's cgroup path as
// /proc/self/cgroup says ("/system.slice/middleware.service/house").
func Setup(self string, l Limits) (*Tree, error) {
	if self == "" || self == "/" {
		return nil, ErrNotDelegated
	}
	unitPath := filepath.Dir(self)
	if filepath.Base(self) != HouseName {
		// Started in the unit's own cgroup: an older systemd without
		// DelegateSubgroup, or a unit from before it was written. Only
		// the house's own unit: a supervisor started by hand inside a
		// delegated session scope would otherwise move that session's
		// shell and whatever else lives there.
		if filepath.Base(self) != UnitName {
			return nil, ErrNotDelegated
		}
		unitPath = self
	}
	unit := filepath.Join(Root, unitPath)
	if syscall.Access(filepath.Join(unit, "cgroup.subtree_control"), 2) != nil {
		return nil, ErrNotDelegated
	}
	if unitPath == self {
		if err := moveIntoHouse(unit); err != nil {
			return nil, err
		}
	}
	// Controllers have to be enabled on the parent for the children to
	// have the files; "+cpu +memory" on a cgroup that already has them is
	// a no-op.
	if err := os.WriteFile(filepath.Join(unit, "cgroup.subtree_control"), []byte("+cpu +memory"), 0o644); err != nil {
		return nil, fmt.Errorf("cgroups: enable controllers: %w", err)
	}
	agents := filepath.Join(unit, AgentsName)
	if err := os.MkdirAll(agents, 0o755); err != nil {
		return nil, err
	}
	t := &Tree{agents: agents}
	if err := t.SetAgents(l); err != nil {
		return nil, err
	}
	return t, nil
}

// SetAgents writes the agents' limits, at start and whenever the settings
// change them: a cgroup takes new numbers live. A zero is no limit
// ("max"), so clearing an override the caller did not replace lifts it
// instead of leaving the old number in force.
func (t *Tree) SetAgents(l Limits) error {
	if t == nil {
		return nil
	}
	weight := l.CPUWeight
	if weight <= 0 {
		weight = 100
	}
	writes := [][2]string{
		{"cpu.weight", strconv.Itoa(weight)},
		{"cpu.max", "max 100000"},
		// high before max on the way down: memory.max under memory.high
		// is accepted, but the order keeps a lowered pair coherent at
		// every step.
		{"memory.high", "max"},
		{"memory.max", "max"},
	}
	if l.CPUCores > 0 {
		writes[1][1] = strconv.Itoa(l.CPUCores*100000) + " 100000"
	}
	if l.MemoryHigh > 0 {
		writes[2][1] = strconv.FormatInt(l.MemoryHigh, 10)
	}
	if l.MemoryMax > 0 {
		writes[3][1] = strconv.FormatInt(l.MemoryMax, 10)
	}
	for _, w := range writes {
		if err := os.WriteFile(filepath.Join(t.agents, w[0]), []byte(w[1]), 0o644); err != nil {
			return fmt.Errorf("cgroups: %s: %w", w[0], err)
		}
	}
	return nil
}

// movePID moves a process (its whole thread group) into a cgroup; tests
// replace it, as a folder does not move anything by being written.
var movePID = func(cgroup, pid string) error {
	return os.WriteFile(filepath.Join(cgroup, "cgroup.procs"), []byte(pid), 0o644)
}

// moveIntoHouse does what DelegateSubgroup=house does at start: every
// process in the unit's own cgroup (the supervisor and the sites it
// re-adopted there) into house/. A cgroup with processes cannot hand
// controllers to its children ("no internal processes"), which is why the
// unit has to be emptied before agents/ can have limits. Passes until it
// is empty: a process may fork while the others move.
func moveIntoHouse(unit string) error {
	house := filepath.Join(unit, HouseName)
	if err := os.MkdirAll(house, 0o755); err != nil {
		return fmt.Errorf("cgroups: %s: %w", house, err)
	}
	for pass := 0; pass < 5; pass++ {
		raw, err := os.ReadFile(filepath.Join(unit, "cgroup.procs"))
		if err != nil {
			return fmt.Errorf("cgroups: %w", err)
		}
		pids := strings.Fields(string(raw))
		if len(pids) == 0 {
			return nil
		}
		for _, pid := range pids {
			// A process that ended meanwhile is not a failure.
			if err := movePID(house, pid); err != nil && !errors.Is(err, syscall.ESRCH) {
				return fmt.Errorf("cgroups: moving %s into %s: %w", pid, house, err)
			}
		}
	}
	return errors.New("cgroups: the unit's cgroup did not empty into house/ after five passes")
}

// SelfPath reads this process's cgroup v2 path.
func SelfPath() string {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if p, ok := strings.CutPrefix(line, "0::"); ok {
			return strings.TrimSpace(p)
		}
	}
	return ""
}

// AgentsAttr is a SysProcAttr that starts a process inside agents/, or
// nil without a tree. The caller owns the descriptor until Start returns.
func (t *Tree) AgentsAttr() (*syscall.SysProcAttr, func(), error) {
	if t == nil {
		return nil, func() {}, nil
	}
	fd, err := syscall.Open(t.agents, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, func() {}, err
	}
	return &syscall.SysProcAttr{Setpgid: true, UseCgroupFD: true, CgroupFD: fd}, func() { syscall.Close(fd) }, nil
}

// Agents is the agents' branch path, for the panel.
func (t *Tree) Agents() string {
	if t == nil {
		return ""
	}
	return t.agents
}

func totalMemory() int64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "MemTotal:"); ok {
			f := strings.Fields(v)
			if len(f) > 0 {
				kb, _ := strconv.ParseInt(f[0], 10, 64)
				return kb << 10
			}
		}
	}
	return 0
}
