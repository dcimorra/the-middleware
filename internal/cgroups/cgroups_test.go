package cgroups

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The split is written against a folder shaped like the unit's cgroup:
// what can be checked without a delegated cgroup is what is written where.
func TestTheAgentsBranchGetsItsWeightAndCeiling(t *testing.T) {
	defer func(r string) { Root = r }(Root)
	Root = t.TempDir()
	unit := filepath.Join(Root, "system.slice", "middleware.service")
	os.MkdirAll(filepath.Join(unit, "house"), 0o755)
	os.WriteFile(filepath.Join(unit, "cgroup.subtree_control"), nil, 0o644)
	tree, err := Setup("/system.slice/middleware.service/house", Limits{CPUWeight: 10, CPUCores: 2, MemoryHigh: 3 << 30, MemoryMax: 4 << 30})
	if err != nil {
		t.Fatal(err)
	}
	read := func(f string) string {
		b, _ := os.ReadFile(filepath.Join(unit, f))
		return strings.TrimSpace(string(b))
	}
	for f, want := range map[string]string{
		"cgroup.subtree_control": "+cpu +memory", "agents/cpu.weight": "10", "agents/cpu.max": "200000 100000",
		"agents/memory.high": "3221225472", "agents/memory.max": "4294967296",
	} {
		if got := read(f); got != want {
			t.Errorf("%s = %q, want %q", f, got, want)
		}
	}
	attr, release, err := tree.AgentsAttr()
	if err != nil || attr == nil || !attr.UseCgroupFD {
		t.Fatalf("attr %+v %v", attr, err)
	}
	release()
}

func TestWithoutDelegationNothingIsTouched(t *testing.T) {
	defer func(r string) { Root = r }(Root)
	Root = t.TempDir()
	if _, err := Setup("/user.slice/session-1.scope", DefaultLimits()); err != ErrNotDelegated {
		t.Fatalf("%v", err)
	}
	var none *Tree
	if attr, _, err := none.AgentsAttr(); attr != nil || err != nil {
		t.Fatal("a nil tree must give no attributes")
	}
}

// With a systemd older than 254 (no DelegateSubgroup) the supervisor
// starts in the unit's own cgroup: it makes house/, moves every process
// there, and then splits as usual.
func TestWithoutDelegateSubgroupTheHouseMovesItselfFirst(t *testing.T) {
	defer func(r string) { Root = r }(Root)
	defer func(f func(string, string) error) { movePID = f }(movePID)
	Root = t.TempDir()
	unit := filepath.Join(Root, "system.slice", "middleware.service")
	os.MkdirAll(unit, 0o755)
	os.WriteFile(filepath.Join(unit, "cgroup.subtree_control"), nil, 0o644)
	os.WriteFile(filepath.Join(unit, "cgroup.procs"), []byte("100\n200\n"), 0o644)
	var moved []string
	// What the kernel does: the process leaves the unit's list.
	movePID = func(cgroup, pid string) error {
		if filepath.Base(cgroup) != HouseName {
			t.Fatalf("moved into %s", cgroup)
		}
		moved = append(moved, pid)
		raw, _ := os.ReadFile(filepath.Join(unit, "cgroup.procs"))
		rest := strings.Replace(string(raw), pid+"\n", "", 1)
		return os.WriteFile(filepath.Join(unit, "cgroup.procs"), []byte(rest), 0o644)
	}
	if _, err := Setup("/system.slice/middleware.service", Limits{CPUWeight: 10}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(moved, ",") != "100,200" {
		t.Fatalf("moved %v", moved)
	}
	if b, _ := os.ReadFile(filepath.Join(unit, "agents", "cpu.weight")); string(b) != "10" {
		t.Fatalf("agents/ was not set up after the move: %q", b)
	}
	// A unit whose cgroup is not ours (no delegation) is left alone.
	os.Chmod(filepath.Join(unit, "cgroup.subtree_control"), 0o444)
	if os.Geteuid() != 0 {
		if _, err := Setup("/system.slice/middleware.service", Limits{}); err != ErrNotDelegated {
			t.Fatalf("not delegated: %v", err)
		}
	}
}

// The reserve kept from the agents grows with the machine: a quarter of
// the cores, at least one.
func TestTheAgentsReserveIsAQuarterOfTheMachine(t *testing.T) {
	for _, c := range []struct {
		cpus, cores int
	}{{1, 1}, {2, 1}, {4, 3}, {8, 6}, {12, 9}, {64, 48}} {
		l := Machine{CPUs: c.cpus, Memory: 12 << 30}.DefaultLimits()
		if l.CPUCores != c.cores {
			t.Errorf("%d CPUs: agents get %d cores, want %d", c.cpus, l.CPUCores, c.cores)
		}
		if l.MemoryHigh != 3<<30 || l.MemoryMax != 4<<30 {
			t.Errorf("memory %d/%d, want a quarter and a third", l.MemoryHigh, l.MemoryMax)
		}
	}
}

// New limits reach the files of a branch that already exists, and a
// limit taken away is written as "max" rather than left as it was.
func TestTheAgentsLimitsChangeLive(t *testing.T) {
	defer func(r string) { Root = r }(Root)
	Root = t.TempDir()
	unit := filepath.Join(Root, "system.slice", "middleware.service")
	os.MkdirAll(filepath.Join(unit, "house"), 0o755)
	os.WriteFile(filepath.Join(unit, "cgroup.subtree_control"), nil, 0o644)
	tree, err := Setup("/system.slice/middleware.service/house", Limits{CPUWeight: 10, CPUCores: 2, MemoryHigh: 3 << 30, MemoryMax: 4 << 30})
	if err != nil {
		t.Fatal(err)
	}
	if err := tree.SetAgents(Limits{CPUWeight: 10, CPUCores: 5}); err != nil {
		t.Fatal(err)
	}
	for f, want := range map[string]string{"cpu.max": "500000 100000", "memory.high": "max", "memory.max": "max", "cpu.weight": "10"} {
		b, _ := os.ReadFile(filepath.Join(unit, "agents", f))
		if string(b) != want {
			t.Errorf("%s = %q, want %q", f, b, want)
		}
	}
}

func TestSizesReadAsDockerReadsThem(t *testing.T) {
	for in, want := range map[string]int64{"2048": 2048, "512m": 512 << 20, "1g": 1 << 30, "1G": 1 << 30, "1.5g": 3 << 29, "1GiB": 1 << 30, "768mb": 768 << 20, "4k": 4096, "2 g": 2 << 30} {
		got, err := ParseBytes(in)
		if err != nil || got != want {
			t.Errorf("%q = %d %v, want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "g", "-1g", "1x", "1e9", "abc", "1gg"} {
		if _, err := ParseBytes(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
