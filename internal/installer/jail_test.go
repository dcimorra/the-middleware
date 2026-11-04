package installer

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dcimorra/the-middleware/internal/cgroups"
)

// On a machine that wants a profile for user namespaces, the binary gets
// one and it is loaded; a second run finds it current and does nothing.
func TestJailStepWritesAndLoadsTheAppArmorProfile(t *testing.T) {
	in, rec, _ := testInstaller(t)
	rec.Set("sysctl -n kernel.apparmor_restrict_unprivileged_userns", "1\n", nil)
	changed, _, err := in.stepJail()
	if err != nil || !changed {
		t.Fatalf("changed %v, err %v", changed, err)
	}
	p := filepath.Join(in.Paths.AppArmorDir, AppArmorProfile)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"profile middleware " + in.Params.Binary + " ", "userns,"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("profile lacks %q:\n%s", want, b)
		}
	}
	if !slices.Contains(rec.Mutations(), "sudo apparmor_parser -r "+p) {
		t.Errorf("profile not loaded: %v", rec.Mutations())
	}
	before := len(rec.Mutations())
	if changed, _, err := in.stepJail(); err != nil || changed || len(rec.Mutations()) != before {
		t.Errorf("second run: changed %v, err %v, calls %v", changed, err, rec.Mutations()[before:])
	}
}

// Where nothing restricts user namespaces no profile is written.
func TestJailStepLeavesAppArmorAloneWhereUnneeded(t *testing.T) {
	in, rec, _ := testInstaller(t)
	rec.Set("sysctl -n kernel.apparmor_restrict_unprivileged_userns", "0\n", nil)
	if _, _, err := in.stepJail(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(in.Paths.AppArmorDir, AppArmorProfile)); err == nil {
		t.Error("profile written on a machine that does not ask for one")
	}
}

// A unit rendered before the jail keeps ProtectKernelTunables; the drop-in
// turns it off and the supervisor is told to restart. A unit rendered now
// does not have it and gets no drop-in.
func TestJailStepTurnsOffProtectKernelTunablesOfOldUnits(t *testing.T) {
	in, rec, _ := testInstaller(t)
	unit := in.pathFor(ServiceUnit)
	if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unit, []byte("[Service]\nNoNewPrivileges=no\nProtectKernelTunables=yes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, _, err := in.stepJail()
	if err != nil || !changed {
		t.Fatalf("changed %v, err %v", changed, err)
	}
	b, err := os.ReadFile(filepath.Join(in.Paths.SystemdDir, JailDropIn))
	if err != nil || !strings.Contains(string(b), "ProtectKernelTunables=no") {
		t.Fatalf("drop-in: %q %v", b, err)
	}
	if !slices.Contains(rec.Mutations(), "sudo systemctl daemon-reload") {
		t.Errorf("no daemon-reload: %v", rec.Mutations())
	}

	in2, _, _ := testInstaller(t)
	files, err := Render(in2.Params)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(files[ServiceUnit], "ProtectKernelTunables=yes") {
		t.Fatal("the rendered unit still protects the kernel tunables")
	}
	if err := os.MkdirAll(filepath.Dir(in2.pathFor(ServiceUnit)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(in2.pathFor(ServiceUnit), []byte(files[ServiceUnit]), 0o644); err != nil {
		t.Fatal(err)
	}
	if changed, _, err := in2.stepJail(); err != nil || changed {
		t.Errorf("current unit: changed %v, err %v", changed, err)
	}
}

// A unit that mounts the cgroups read-only or does not delegate pids gets
// a drop-in that hands the supervisor its cgroup, and the supervisor is
// told to restart; a unit rendered now needs none.
func TestCgroupsStepGivesOldUnitsTheirCgroup(t *testing.T) {
	for name, body := range map[string]string{
		"read-only": "[Service]\nDelegate=cpu memory pids\nProtectControlGroups=yes\n",
		"no pids":   "[Service]\nDelegate=cpu memory\nProtectControlGroups=no\n",
	} {
		in, rec, _ := testInstaller(t)
		unit := in.pathFor(ServiceUnit)
		os.MkdirAll(filepath.Dir(unit), 0o755)
		os.WriteFile(unit, []byte(body), 0o644)
		changed, _, err := in.stepCgroups()
		if err != nil || !changed {
			t.Fatalf("%s: changed %v, err %v", name, changed, err)
		}
		b, _ := os.ReadFile(filepath.Join(in.Paths.SystemdDir, CgroupsDropIn))
		if !strings.Contains(string(b), "ProtectControlGroups=no") || !strings.Contains(string(b), "Delegate=cpu memory pids") {
			t.Fatalf("%s: drop-in %q", name, b)
		}
		if !slices.Contains(rec.Mutations(), "sudo systemctl daemon-reload") {
			t.Errorf("%s: no daemon-reload: %v", name, rec.Mutations())
		}
		if changed, _, _ := in.stepCgroups(); changed {
			t.Errorf("%s: written twice", name)
		}
	}
	in, _, _ := testInstaller(t)
	files, err := Render(in.Params)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(files[ServiceUnit], "ProtectControlGroups=yes") || !strings.Contains(files[ServiceUnit], "\nDelegate=cpu memory pids") {
		t.Fatal("the rendered unit keeps the supervisor from its cgroup")
	}
	os.MkdirAll(filepath.Dir(in.pathFor(ServiceUnit)), 0o755)
	os.WriteFile(in.pathFor(ServiceUnit), []byte(files[ServiceUnit]), 0o644)
	if changed, _, err := in.stepCgroups(); err != nil || changed {
		t.Errorf("current unit: changed %v, err %v", changed, err)
	}
}

// The doctor says whether the running supervisor split its cgroup, and
// what to do when it did not: install the drop-in for a unit that keeps
// it out, restart for one that allows it.
func TestTheDoctorSaysWhetherTheHouseSplitItsCgroup(t *testing.T) {
	in, rec, _ := testInstaller(t)
	defer func(r string) { cgroups.Root = r }(cgroups.Root)
	cgroups.Root = t.TempDir()
	if c := in.checkCgroups(); !c.OK {
		t.Fatalf("no unified hierarchy is not a failure: %+v", c)
	}
	os.WriteFile(filepath.Join(cgroups.Root, "cgroup.controllers"), []byte("cpu memory pids"), 0o644)
	rec.Set("systemctl show -p ControlGroup --value "+ServiceUnit, "/system.slice/middleware.service", nil)
	unit := filepath.Join(cgroups.Root, "system.slice", "middleware.service")
	os.MkdirAll(filepath.Join(unit, "house"), 0o755)
	os.MkdirAll(in.Paths.SystemdDir, 0o755)
	os.WriteFile(in.pathFor(ServiceUnit), []byte("[Service]\nProtectControlGroups=yes\nDelegate=yes\n"), 0o644)
	if c := in.checkCgroups(); c.OK || !strings.Contains(c.Detail, "Install requirements") {
		t.Fatalf("an old unit: %+v", c)
	}
	os.MkdirAll(filepath.Join(in.Paths.SystemdDir, "middleware.service.d"), 0o755)
	os.WriteFile(filepath.Join(in.Paths.SystemdDir, CgroupsDropIn), []byte(cgroupsDropInContent), 0o644)
	if c := in.checkCgroups(); c.OK || !strings.Contains(c.Detail, "restart the house") || strings.Contains(c.Detail, "Install") {
		t.Fatalf("the drop-in written, not restarted: %+v", c)
	}
	os.MkdirAll(filepath.Join(unit, "sites"), 0o755)
	if c := in.checkCgroups(); !c.OK {
		t.Fatalf("split: %+v", c)
	}
}
