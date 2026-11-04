//go:build linux

package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/cgroups"
	"github.com/dcimorra/the-middleware/internal/jail"
	"github.com/dcimorra/the-middleware/internal/manifest"
)

// liveTree splits the cgroup this test runs in, which must be delegated:
// MIDDLEWARE_TEST_CGROUPS=1 says it is. CI runs these tests as
//
//	systemd-run --wait --pipe -p Delegate=yes -p DelegateSubgroup=house \
//	  --setenv=MIDDLEWARE_TEST_CGROUPS=1 ./runtime.test -test.run Cgroup
//
// the shape the house's unit gives the supervisor. Anywhere else they
// skip: a folder cannot show that the kernel put a process where it was
// told.
func liveTree(t *testing.T) *cgroups.Tree {
	t.Helper()
	if os.Getenv("MIDDLEWARE_TEST_CGROUPS") != "1" {
		t.Skip("needs a delegated cgroup: MIDDLEWARE_TEST_CGROUPS=1")
	}
	tree, err := cgroups.Setup(cgroups.SelfPath(), cgroups.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if !tree.Sites() {
		t.Fatalf("no sites' branch: %v", tree.Notes())
	}
	SiteGroups = tree
	t.Cleanup(func() { SiteGroups = nil })
	return tree
}

func procsOf(t *testing.T, cg string) map[int]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(cg, "cgroup.procs"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[int]bool{}
	for _, f := range strings.Fields(string(raw)) {
		n, _ := strconv.Atoi(f)
		out[n] = true
	}
	return out
}

func houseOf(t *testing.T) string {
	return filepath.Join(cgroups.Root, cgroups.SelfPath())
}

// A process site and what it starts are in its site's cgroup from the
// first instruction, not in house/; jailed too, since the jail keeps the
// attributes it is given.
func TestCgroupLiveASitesProcessesAreInItsGroup(t *testing.T) {
	tree := liveTree(t)
	for _, jailed := range []bool{false, true} {
		name := map[bool]string{false: "plain", true: "jailed"}[jailed]
		t.Run(name, func(t *testing.T) {
			if jailed {
				if err := jail.Available(); err != nil {
					t.Skipf("no jail: %v", err)
				}
				Enclose = func(cmd *exec.Cmd, spec *manifest.Spec, home string) error {
					return jail.Wrap(cmd, jail.System(spec.Domain).Write(spec.SiteDir, home))
				}
				defer func() { Enclose = nil }()
			}
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "run.sh"), []byte("#!/bin/sh\nsleep 300 &\nwait\n"), 0o755)
			domain := name + ".test"
			spec := &manifest.Spec{Domain: domain, SiteDir: dir, Cwd: dir, Command: []string{"./run.sh"}, Env: map[string]string{}, Revision: "r1"}
			inst, err := Process{}.Start(context.Background(), spec, 45998, filepath.Join(dir, "out.log"), filepath.Join(dir, "home"))
			if err != nil {
				t.Fatal(err)
			}
			defer inst.Stop(context.Background(), 2*time.Second)
			pid := inst.State().PID
			time.Sleep(300 * time.Millisecond)
			site := procsOf(t, tree.SitePath(domain))
			if !site[pid] || len(site) < 2 {
				t.Fatalf("site group holds %v, want %d and its child", site, pid)
			}
			for p := range procsOf(t, houseOf(t)) {
				if site[p] {
					t.Fatalf("%d is in house/ too", p)
				}
			}
		})
	}
}

// A site started by a supervisor from before site groups is in house/;
// adopted, it moves with its children into its group.
func TestCgroupLiveAnOldSiteMovesAtAdoption(t *testing.T) {
	tree := liveTree(t)
	cmd := exec.Command("/bin/sh", "-c", "sleep 300 & wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	defer func() { syscall.Kill(-pid, syscall.SIGKILL); cmd.Wait() }()
	time.Sleep(200 * time.Millisecond)
	if !procsOf(t, houseOf(t))[pid] {
		t.Fatal("the old site is not in house/ to begin with")
	}
	if _, err := (Process{}).Adopt(context.Background(), State{Domain: "old.test", Runtime: "process", PID: pid}); err != nil {
		t.Fatal(err)
	}
	site := procsOf(t, tree.SitePath("old.test"))
	if !site[pid] || len(site) < 2 {
		t.Fatalf("site group after adoption holds %v, want %d and its child", site, pid)
	}
	for p := range procsOf(t, houseOf(t)) {
		if site[p] {
			t.Fatalf("%d left in house/", p)
		}
	}
}
