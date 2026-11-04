package cgroups

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// unitTree is a folder shaped like the unit's cgroup, delegating ctrl.
func unitTree(t *testing.T, ctrl string) string {
	t.Helper()
	old := Root
	t.Cleanup(func() { Root = old })
	Root = t.TempDir()
	unit := filepath.Join(Root, "system.slice", "middleware.service")
	os.MkdirAll(filepath.Join(unit, "house"), 0o755)
	os.WriteFile(filepath.Join(unit, "cgroup.subtree_control"), nil, 0o644)
	os.WriteFile(filepath.Join(unit, "cgroup.controllers"), []byte(ctrl), 0o644)
	return unit
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	return strings.TrimSpace(string(b))
}

// sites/ is a third sibling with the controllers handed down, and a
// site's group is made on its first start with an equal share and the
// process cap: CPU per site, not per process.
func TestEverySiteGetsAGroupBesideTheHouse(t *testing.T) {
	unit := unitTree(t, "cpuset cpu io memory pids")
	tree, err := Setup("/system.slice/middleware.service/house", Limits{CPUWeight: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !tree.Sites() || len(tree.Notes()) != 0 {
		t.Fatalf("sites %v notes %v", tree.Sites(), tree.Notes())
	}
	for f, want := range map[string]string{"cgroup.subtree_control": "+cpu +memory +pids", "sites/cgroup.subtree_control": "+cpu +memory +pids"} {
		if got := readFile(t, filepath.Join(unit, f)); got != want {
			t.Errorf("%s = %q, want %q", f, got, want)
		}
	}
	// A worker's spec domain counts against its site.
	attr, release, err := tree.SiteAttr("a.test/queue")
	if err != nil || attr == nil || !attr.UseCgroupFD || !attr.Setpgid {
		t.Fatalf("attr %+v %v", attr, err)
	}
	release()
	site := filepath.Join(unit, "sites", "a.test")
	for f, want := range map[string]string{"cpu.weight": "100", "cpu.max": "max 100000", "memory.max": "max", "memory.high": "max", "pids.max": "4096"} {
		if got := readFile(t, filepath.Join(site, f)); got != want {
			t.Errorf("%s = %q, want %q", f, got, want)
		}
	}
	if tree.SitePath("a.test") != site {
		t.Fatalf("path %q", tree.SitePath("a.test"))
	}
	for _, bad := range []string{"", "..", "/x"} {
		if _, _, err := tree.SiteAttr(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// A unit installed before pids was delegated still splits: CPU and
// memory per site, no process cap, said once.
func TestAUnitWithoutPidsStillGivesSitesTheirGroups(t *testing.T) {
	unit := unitTree(t, "cpu memory")
	tree, err := Setup("/system.slice/middleware.service/house", Limits{CPUWeight: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !tree.Sites() || len(tree.Notes()) != 1 || !strings.Contains(tree.Notes()[0], "pids") {
		t.Fatalf("sites %v notes %v", tree.Sites(), tree.Notes())
	}
	if got := readFile(t, filepath.Join(unit, "cgroup.subtree_control")); got != "+cpu +memory" {
		t.Fatalf("subtree_control %q", got)
	}
	if err := tree.SetSite("b.test", SiteLimits{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(unit, "sites", "b.test", "pids.max")); !os.IsNotExist(err) {
		t.Fatalf("pids.max written without the controller: %v", err)
	}
}

// Limits written to a group that exists change it at once; written
// before the group exists they are what it is born with; taken away,
// they go back to "max".
func TestASitesLimitsAreWrittenLive(t *testing.T) {
	unit := unitTree(t, "cpu memory pids")
	tree, _ := Setup("/system.slice/middleware.service/house", Limits{CPUWeight: 10})
	if err := tree.SetSite("a.test", SiteLimits{MemoryMax: 1 << 30, MemoryHigh: 768 << 20, CPUs: 1.5, CPUWeight: 200, Pids: 2048}); err != nil {
		t.Fatal(err)
	}
	site := filepath.Join(unit, "sites", "a.test")
	for f, want := range map[string]string{"cpu.weight": "200", "cpu.max": "150000 100000", "memory.max": "1073741824", "memory.high": "805306368", "pids.max": "2048"} {
		if got := readFile(t, filepath.Join(site, f)); got != want {
			t.Errorf("%s = %q, want %q", f, got, want)
		}
	}
	tree.SetSite("a.test", SiteLimits{})
	if got := readFile(t, filepath.Join(site, "memory.max")); got != "max" {
		t.Fatalf("memory.max after clearing = %q", got)
	}
	// Removed (the folder stands for an empty group) and made again on
	// the next start, with the limits last set.
	tree.SetSite("a.test", SiteLimits{MemoryMax: 1 << 30})
	defer func(f func(string) error) { removeGroup = f }(removeGroup)
	removeGroup = os.RemoveAll
	os.RemoveAll(site)
	if _, release, err := tree.SiteAttr("a.test"); err != nil {
		t.Fatal(err)
	} else {
		release()
	}
	if got := readFile(t, filepath.Join(site, "memory.max")); got != "1073741824" {
		t.Fatalf("a group made again lost its limits: %q", got)
	}
	// A sweep takes the groups of sites that are gone, and only those.
	tree.SetSite("gone.test", SiteLimits{})
	tree.SweepSites(func(s string) bool { return s == "a.test" })
	if _, err := os.Stat(filepath.Join(unit, "sites", "gone.test")); !os.IsNotExist(err) {
		t.Fatal("the group of a site that is gone was kept")
	}
	if _, err := os.Stat(site); err != nil {
		t.Fatal("a live site's group was swept")
	}
}

// A site re-adopted from a supervisor that started it in house/ moves
// there with everything it started: its descendants and its process
// group, in passes, while the supervisor and another site's processes
// stay.
func TestAReadoptedSiteMovesOutOfTheHouseWithItsChildren(t *testing.T) {
	unit := unitTree(t, "cpu memory pids")
	tree, _ := Setup("/system.slice/middleware.service/house", Limits{CPUWeight: 10})
	house := filepath.Join(unit, "house")
	// pid: ppid, pgid. 1 is the supervisor; 100 the site's leader; 101
	// its child; 102 a grandchild listed before its parent 103; 200 a
	// process of its group whose parent left (reparented to 1); 300 is
	// another site.
	procs := map[int][2]int{1: {0, 1}, 100: {1, 100}, 101: {100, 100}, 102: {103, 103}, 103: {101, 103}, 200: {1, 100}, 300: {1, 300}}
	write := func() {
		var l []string
		for pid := range procs {
			l = append(l, strconv.Itoa(pid))
		}
		sort.Strings(l)
		// 102 before 103 in the list, as the kernel may give them.
		os.WriteFile(filepath.Join(house, "cgroup.procs"), []byte(strings.Join(l, "\n")+"\n"), 0o644)
	}
	write()
	defer func(f func(int) (int, int, bool)) { procLinks = f }(procLinks)
	procLinks = func(pid int) (int, int, bool) { l, ok := procs[pid]; return l[0], l[1], ok }
	defer func(f func(string, string) error) { movePID = f }(movePID)
	var moved []string
	movePID = func(cg, pid string) error {
		if cg != filepath.Join(unit, "sites", "a.test") {
			t.Fatalf("moved into %s", cg)
		}
		moved = append(moved, pid)
		n, _ := strconv.Atoi(pid)
		delete(procs, n)
		write()
		return nil
	}
	if err := tree.AdoptSite("a.test", 100); err != nil {
		t.Fatal(err)
	}
	sort.Strings(moved)
	if strings.Join(moved, ",") != "100,101,102,103,200" {
		t.Fatalf("moved %v", moved)
	}
	if _, ok := procs[1]; !ok {
		t.Fatal("the supervisor moved")
	}
	if _, ok := procs[300]; !ok {
		t.Fatal("another site's process moved")
	}
	// Adopted again (from this version) nothing is in house/: no move.
	moved = nil
	if err := tree.AdoptSite("a.test", 100); err != nil || len(moved) != 0 {
		t.Fatalf("second adoption moved %v (%v)", moved, err)
	}
}
