//go:build linux

package jail

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func need(t *testing.T) {
	t.Helper()
	if err := Available(); err != nil {
		if os.Getenv("MIDDLEWARE_JAIL_REQUIRED") != "" {
			t.Fatalf("no jail on this machine: %v", err)
		}
		t.Skipf("no jail on this machine: %v", err)
	}
}

// house is an installation root with two sites and the data a site must
// not see.
func house(t *testing.T) (root, site, home string) {
	t.Helper()
	root = t.TempDir()
	site = filepath.Join(root, "sites", "a.test")
	home = filepath.Join(root, "data", "homes", "a.test")
	for _, d := range []string{site, home, filepath.Join(root, "sites", "b.test"), filepath.Join(root, "data", "auth")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join(site, "index.html"):                     "a",
		filepath.Join(root, "sites", "b.test", "secret.txt"):  "b",
		filepath.Join(root, "data", "auth", "password"):       "pw",
		filepath.Join(root, "data", "homes", "b.test", ".rc"): "b home",
	}
	for p, s := range files {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root, site, home
}

func jailed(t *testing.T, plan *Plan, dir, script string) (string, int) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	cmd := exec.Command(sh, "-c", script)
	cmd.Dir = dir
	if err := Wrap(cmd, plan); err != nil {
		t.Fatal(err)
	}
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

// What the jail shows is what the plan names, and nothing of the
// installation besides: the neighbour, the other homes and the house's
// data are not there to be listed or read.
func TestJailShowsOnlyThePlan(t *testing.T) {
	need(t)
	root, site, home := house(t)
	plan := System("a.test").Write(site, home)
	out, code := jailed(t, plan, site, `
		cat index.html; echo
		ls `+filepath.Join(root, "sites")+`; echo
		ls `+filepath.Join(root, "data")+`; echo
		ls `+filepath.Join(root, "data", "homes")+`; echo
		test -e `+filepath.Join(root, "sites", "b.test", "secret.txt")+` && echo NEIGHBOUR
		test -e `+filepath.Join(root, "data", "auth", "password")+` && echo HOUSE
		echo hi > `+filepath.Join(home, "note")+` && echo wrote-home
		echo hi > /usr/x 2>/dev/null || echo usr-read-only
		hostname
		ls /proc | grep -c '^[0-9]'
		true`)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	lines := strings.Split(out, "\n")
	if lines[0] != "a" {
		t.Errorf("own file not read: %q", out)
	}
	if lines[1] != "a.test" {
		t.Errorf("sites/ inside shows %q, want only a.test", lines[1])
	}
	if lines[3] != "homes" {
		t.Errorf("data/ inside shows %q, want only homes", lines[3])
	}
	if lines[5] != "a.test" {
		t.Errorf("data/homes/ inside shows %q, want only a.test", lines[5])
	}
	for _, bad := range []string{"NEIGHBOUR", "HOUSE"} {
		if strings.Contains(out, bad) {
			t.Errorf("%s visible inside: %s", bad, out)
		}
	}
	for _, good := range []string{"wrote-home", "usr-read-only", "a.test\n"} {
		if !strings.Contains(out, good) {
			t.Errorf("missing %q in %s", good, out)
		}
	}
	if b, err := os.ReadFile(filepath.Join(home, "note")); err != nil || string(b) != "hi\n" {
		t.Errorf("write to home did not reach the real home: %q %v", b, err)
	}
}

// The process tree it sees is its own: its shell, what the shell runs and
// the init, nothing of the machine.
func TestJailHasItsOwnProcesses(t *testing.T) {
	need(t)
	_, site, home := house(t)
	out, code := jailed(t, System("a.test").Write(site, home), site, `ls /proc | grep '^[0-9]' | wc -l`)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if n := strings.TrimSpace(out); n == "" || len(n) > 1 {
		t.Errorf("%s processes visible inside, want a handful", n)
	}
}

// The command's status is the init's: what the runtimes read back.
func TestJailExitStatus(t *testing.T) {
	need(t)
	_, site, home := house(t)
	if _, code := jailed(t, System("a.test").Write(site, home), site, "exit 7"); code != 7 {
		t.Errorf("exit %d, want 7", code)
	}
	if _, code := jailed(t, System("a.test").Write(site, home), site, "kill -TERM $$"); code != 128+15 {
		t.Errorf("exit %d, want %d", code, 128+15)
	}
}

// A path lent read-only stays so.
func TestJailReadOnlyLend(t *testing.T) {
	need(t)
	root, site, home := house(t)
	lent := filepath.Join(root, "modules")
	if err := os.MkdirAll(lent, 0o755); err != nil {
		t.Fatal(err)
	}
	out, _ := jailed(t, System("a.test").Write(site, home).Read(lent), site, "touch "+lent+"/x 2>/dev/null && echo WROTE; ls -d "+lent)
	if strings.Contains(out, "WROTE") || !strings.Contains(out, lent) {
		t.Errorf("lent folder: %s", out)
	}
}

// The command keeps none of what the init needed to build the jail.
func TestJailCommandHoldsNoCapabilities(t *testing.T) {
	need(t)
	_, site, home := house(t)
	out, code := jailed(t, System("a.test").Write(site, home), site, `grep -E '^(CapPrm|CapEff|CapBnd|CapAmb|NoNewPrivs):' /proc/self/status`)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		k, v, _ := strings.Cut(line, ":")
		v = strings.TrimSpace(v)
		want := "0000000000000000"
		if k == "NoNewPrivs" {
			want = "1"
		}
		if v != want {
			t.Errorf("%s = %s, want %s", k, v, want)
		}
	}
}

// A site's process is cloned into its site's cgroup (internal/cgroups):
// the attributes the runtime set before the jail wraps the command, the
// cgroup descriptor and the process group, must reach the clone, or the
// jailed site would start in house/ with no budget. Checked on the
// attributes without needing a jail on the machine; the live cgroup test
// (internal/runtime, MIDDLEWARE_TEST_CGROUPS) starts a jailed one.
func TestWrapKeepsTheCgroupItIsGiven(t *testing.T) {
	cmd := exec.Command("/bin/true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, UseCgroupFD: true, CgroupFD: 42}
	if err := Wrap(cmd, &Plan{Dir: "/"}); err != nil {
		t.Fatal(err)
	}
	a := cmd.SysProcAttr
	if !a.UseCgroupFD || a.CgroupFD != 42 || !a.Setpgid || a.Cloneflags == 0 {
		t.Fatalf("attributes after Wrap: %+v", a)
	}
}
