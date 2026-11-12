package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/agent"
	"github.com/dcimorra/the-middleware/internal/jail"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/runtime"
)

// jailedHouse is a root with two sites and the house's data, and the
// runtimes pointed at its jails for the length of the test.
func jailedHouse(t *testing.T) (root, site, home string) {
	t.Helper()
	if err := jail.Available(); err != nil {
		if os.Getenv("MIDDLEWARE_JAIL_REQUIRED") != "" {
			t.Fatalf("no jail on this machine: %v", err)
		}
		t.Skipf("no jail on this machine: %v", err)
	}
	root = t.TempDir()
	site = filepath.Join(root, "sites", "a.test")
	home = filepath.Join(root, "data", "homes", "a.test")
	for p, s := range map[string]string{
		filepath.Join(site, "index.html"):                    "a",
		filepath.Join(root, "sites", "b.test", "secret.txt"): "b",
		filepath.Join(root, "data", "auth", "password"):      "pw",
		filepath.Join(home, ".keep"):                         "",
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	setJails(root)
	t.Cleanup(func() { runtime.Enclose, agent.Enclose = nil, nil })
	return root, site, home
}

// probe is the script each path runs: what of the installation it can
// list and read.
func probe(root string) string {
	return `ls ` + filepath.Join(root, "sites") + `; ls ` + filepath.Join(root, "data") +
		`; cat ` + filepath.Join(root, "sites", "b.test", "secret.txt") + ` 2>/dev/null && echo NEIGHBOUR` +
		`; cat ` + filepath.Join(root, "data", "auth", "password") + ` 2>/dev/null && echo HOUSE; cat index.html; echo`
}

func checkSeen(t *testing.T, out string) {
	t.Helper()
	if strings.Contains(out, "b.test") || strings.Contains(out, "NEIGHBOUR") || strings.Contains(out, "HOUSE") || strings.Contains(out, "auth") {
		t.Errorf("the site sees past its folder:\n%s", out)
	}
	if !strings.Contains(out, "a.test") || !strings.HasSuffix(strings.TrimSpace(out), "a") {
		t.Errorf("the site does not see its own folder:\n%s", out)
	}
}

// A command run for a site (the panel's "run a command", a deploy's
// build) runs in the site's jail.
func TestJailedExec(t *testing.T) {
	root, site, home := jailedHouse(t)
	spec := &manifest.Spec{Domain: "a.test", SiteDir: site, Cwd: site}
	var out bytes.Buffer
	code, err := runtime.Exec(context.Background(), spec, home, runtime.ExecOptions{Args: []string{"sh", "-c", probe(root)}, Stdout: &out, Stderr: &out})
	if err != nil || code != 0 {
		t.Fatalf("exit %d, err %v: %s", code, err, out.String())
	}
	checkSeen(t, out.String())
}

// A command named by its path through merged /usr (/bin/sh, as a task's
// file usually says) runs: it used to be lent as a mount of its own under
// the /bin link and the jail failed before the command started.
func TestJailedCommandThroughTheBinLink(t *testing.T) {
	_, site, home := jailedHouse(t)
	if st, err := os.Lstat("/bin"); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Skip("/bin is not a link to usr/bin here")
	}
	spec := &manifest.Spec{Domain: "a.test", SiteDir: site, Cwd: site}
	var out bytes.Buffer
	code, err := runtime.Exec(context.Background(), spec, home, runtime.ExecOptions{Args: []string{"/bin/sh", "-c", "echo inside"}, Stdout: &out, Stderr: &out})
	if err != nil || code != 0 || strings.TrimSpace(out.String()) != "inside" {
		t.Fatalf("exit %d, err %v: %s", code, err, out.String())
	}
}

// A site's process (and so its workers and tasks, which start the same
// way) runs in the site's jail, and stops when told to.
func TestJailedProcess(t *testing.T) {
	root, site, home := jailedHouse(t)
	spec := &manifest.Spec{Domain: "a.test", SiteDir: site, Cwd: site, Command: []string{"sh", "-c", probe(root) + "; exec sleep 600"}}
	log := filepath.Join(root, "data", "logs", "a.test", "out.log")
	inst, err := runtime.Process{}.Start(context.Background(), spec, 0, log, home)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	var out []byte
	for time.Now().Before(deadline) {
		out, _ = os.ReadFile(log)
		if bytes.Count(out, []byte("\n")) >= 4 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	_, body, _ := strings.Cut(string(out), "\n--- ")
	_, body, _ = strings.Cut(body, "\n")
	checkSeen(t, body)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := inst.Stop(ctx, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if inst.Alive() {
		t.Error("still alive after Stop")
	}
}

// The house's own tasks (no site) are the operator's and stay outside.
func TestHouseTaskIsNotJailed(t *testing.T) {
	root, _, _ := jailedHouse(t)
	spec := &manifest.Spec{SiteDir: root, Cwd: root}
	var out bytes.Buffer
	if _, err := runtime.Exec(context.Background(), spec, filepath.Join(root, "data", "homes", "tasks"), runtime.ExecOptions{Args: []string{"ls", filepath.Join(root, "sites")}, Stdout: &out}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "b.test") {
		t.Errorf("the house's task lost the installation: %s", out.String())
	}
}

// A site's conversation turn runs in the site's jail.
func TestJailedTurn(t *testing.T) {
	root, site, home := jailedHouse(t)
	cli := filepath.Join(t.TempDir(), "fake-cli")
	if err := os.WriteFile(cli, []byte("#!/bin/sh\n"+probe(root)+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	j := agent.Jail{SiteDir: site, Home: home}
	stream := filepath.Join(t.TempDir(), "turn.out")
	run, err := j.Start(context.Background(), nopParser{}, stream, site, j.Env(nil), cli)
	if err != nil {
		t.Fatal(err)
	}
	timeout := time.After(10 * time.Second)
	for done := false; !done; {
		select {
		case _, ok := <-run.Events():
			done = !ok
		case <-timeout:
			t.Fatal("turn did not end")
		}
	}
	out, _ := os.ReadFile(stream)
	errOut, _ := os.ReadFile(stream + ".err")
	checkSeen(t, string(out)+string(errOut))
}

type nopParser struct{}

func (nopParser) Line(string) ([]agent.Event, bool) { return nil, false }
func (nopParser) Flush() []agent.Event              { return nil }

// A message longer than the kernel takes as an argument reaches a jailed
// turn whole, on its standard input: as an argument the turn never
// started ("fork/exec /proc/self/exe: argument list too long"), and the
// site's code asked again every few seconds for the same answer.
func TestJailedTurnReadsALongMessage(t *testing.T) {
	_, site, home := jailedHouse(t)
	cli := filepath.Join(t.TempDir(), "fake-cli")
	if err := os.WriteFile(cli, []byte("#!/bin/sh\nwc -c\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	j := agent.Jail{SiteDir: site, Home: home}
	stream := filepath.Join(t.TempDir(), "turn.out")
	message := strings.Repeat("a book. ", 64*1024)
	run, err := j.StartInput(context.Background(), nopParser{}, stream, site, j.Env(nil), message, cli)
	if err != nil {
		t.Fatal(err)
	}
	timeout := time.After(10 * time.Second)
	for done := false; !done; {
		select {
		case _, ok := <-run.Events():
			done = !ok
		case <-timeout:
			t.Fatal("turn did not end")
		}
	}
	out, _ := os.ReadFile(stream)
	if got := strings.TrimSpace(string(out)); got != "524288" {
		errOut, _ := os.ReadFile(stream + ".err")
		t.Fatalf("the turn read %q bytes of 524288: %s", got, errOut)
	}
	if _, err := os.Stat(stream + ".in"); err == nil {
		t.Fatal("the message's file outlived the start of the turn")
	}
	// The same message as an argument is refused before the kernel is
	// asked, as what it is.
	if _, err := j.Start(context.Background(), nopParser{}, stream, site, j.Env(nil), cli, message); !errors.Is(err, agent.ErrTooLong) {
		t.Fatalf("err = %v, want agent.ErrTooLong", err)
	}
}
