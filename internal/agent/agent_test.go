package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func makeRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{
		"sites/a.test", "sites/b.test", "data/homes/a.test", "data/homes/b.test",
		"data/state", "data/events", "tasks", "modules",
	} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(root, "data", "settings.json"), []byte("{}"), 0o644)
	return root
}

func TestASiteJailDeniesSiblingsDataAndTheRootButNotItself(t *testing.T) {
	root := makeRoot(t)
	site := filepath.Join(root, "sites", "a.test")
	home := filepath.Join(root, "data", "homes", "a.test")
	j := NewJail(root, site, home)
	denied := strings.Join(j.Deny, "\n")
	for _, want := range []string{
		filepath.Join(root, "sites", "b.test"),
		filepath.Join(root, "data", "homes", "b.test"),
		filepath.Join(root, "data", "state"),
		filepath.Join(root, "data", "settings.json"),
		filepath.Join(root, "tasks"),
		filepath.Join(root, "modules"),
	} {
		if !strings.Contains(denied, want) {
			t.Errorf("missing %s in deny list:\n%s", want, denied)
		}
	}
	for _, d := range j.Deny {
		if d == site || d == home || strings.HasPrefix(d, site+"/") || d == root {
			t.Errorf("the jail denies its own ground: %s", d)
		}
	}
}

func TestTheMiddlewareJailDeniesEachExistingSiteButNotTheSitesFolder(t *testing.T) {
	root := makeRoot(t)
	home := filepath.Join(root, "data", "homes", "middleware")
	j := NewJail(root, root, home)
	denied := strings.Join(j.Deny, "\n")
	for _, want := range []string{
		filepath.Join(root, "sites", "a.test"),
		filepath.Join(root, "sites", "b.test"),
		filepath.Join(root, "data", "homes", "a.test"),
		filepath.Join(root, "data", "state"),
		filepath.Join(root, "data", "settings.json"),
		filepath.Join(root, "modules"),
	} {
		if !strings.Contains(denied, want) {
			t.Errorf("missing %s in deny list:\n%s", want, denied)
		}
	}
	for _, d := range j.Deny {
		if d == filepath.Join(root, "sites") || d == filepath.Join(root, "tasks") || d == home || d == root {
			t.Errorf("the house jail denies what it must not: %s", d)
		}
	}
	// A site created during the turn is not in the list, so the audit lets
	// it through, while a site that was already there is flagged.
	v := j.Violations("Write " + filepath.Join(root, "sites", "new.test", "site.json") + "\nRead " + filepath.Join(root, "sites", "a.test", ".env"))
	if len(v) != 1 || v[0] != filepath.Join(root, "sites", "a.test") {
		t.Fatalf("violations = %v", v)
	}
}

func TestTheAuditFindsADeniedPathInATranscript(t *testing.T) {
	root := makeRoot(t)
	site := filepath.Join(root, "sites", "a.test")
	j := NewJail(root, site, filepath.Join(root, "data", "homes", "a.test"))
	evs := []Event{
		{Kind: KindText, Text: "Let me look around."},
		{Kind: KindTool, Tool: "Read", Input: map[string]any{"file_path": filepath.Join(root, "sites", "b.test", ".env")}},
		{Kind: KindTool, Tool: "Read", Input: map[string]any{"file_path": filepath.Join(site, "README.md")}},
	}
	v := j.Violations(Transcript(evs))
	if len(v) != 1 || v[0] != filepath.Join(root, "sites", "b.test") {
		t.Fatalf("violations = %v", v)
	}
}

func TestTheJailEnvironmentHasHomeInsideAndNoDuplicates(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	j := Jail{SiteDir: "/s", Home: "/h"}
	env := j.Env(map[string]string{"HOME": "/elsewhere", "FOO": "bar"})
	seen := map[string]bool{}
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if seen[k] {
			t.Fatalf("duplicate %s in %v", k, env)
		}
		seen[k] = true
	}
	if !contains(env, "HOME=/h") || !contains(env, "FOO=bar") || !contains(env, "PATH=/usr/bin") {
		t.Fatalf("env = %v", env)
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func TestTheLedgerSumsByDayAndEnforcesTheMonthlyCap(t *testing.T) {
	l := NewLedger(t.TempDir())
	day1 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	lastMonth := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	if err := l.Add("a.test", 0.4, day1); err != nil {
		t.Fatal(err)
	}
	if err := l.Add("a.test", 0.7, day2); err != nil {
		t.Fatal(err)
	}
	if err := l.Add("a.test", 5, lastMonth); err != nil {
		t.Fatal(err)
	}
	_ = l.Add("a.test", 0, day2) // ignored
	if got := l.Month("a.test", day2); got < 1.09 || got > 1.11 {
		t.Fatalf("month = %v", got)
	}
	if got := l.Total("a.test"); got < 6.09 || got > 6.11 {
		t.Fatalf("total = %v", got)
	}
	if err := l.Allow("a.test", 2, day2); err != nil {
		t.Fatalf("under the cap: %v", err)
	}
	err := l.Allow("a.test", 1, day2)
	if _, ok := err.(*ErrOverCap); !ok {
		t.Fatalf("want ErrOverCap, got %v", err)
	}
	if err := l.Allow("a.test", 0, day2); err != nil {
		t.Fatalf("no cap means allowed: %v", err)
	}
}

func TestTheMonthlyCapIsReadFromTheRawManifest(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(`{"version":2,"agent":{"provider":"claude","monthly_cap_usd":12.5}}`), 0o644)
	if got := ReadSiteSettings(dir).MonthlyCapUSD; got != 12.5 {
		t.Fatalf("cap = %v", got)
	}
	if got := ReadSiteSettings(t.TempDir()).MonthlyCapUSD; got != 0 {
		t.Fatalf("missing manifest: cap = %v", got)
	}
}

type echoParser struct{ lines []string }

func (p *echoParser) Line(line string) ([]Event, bool) {
	p.lines = append(p.lines, line)
	if line == "END" {
		return []Event{{Kind: KindResult, Text: "ok"}}, true
	}
	return []Event{{Kind: KindText, Text: line}}, false
}

func (p *echoParser) Flush() []Event { return nil }

func TestACLIRunEndsOnTheResultLineEvenIfAGrandchildKeepsStdout(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake.sh")
	// The grandchild inherits stdout and sleeps far longer than the test:
	// the run must still end when the parser sees END.
	os.WriteFile(script, []byte("#!/bin/sh\necho hello\nsleep 300 &\necho END\nsleep 300\n"), 0o755)
	start := time.Now()
	run, err := StartCLI(context.Background(), &echoParser{}, dir, Jail{Home: dir}.Env(nil), script)
	if err != nil {
		t.Fatal(err)
	}
	evs := Collect(context.Background(), run)
	if time.Since(start) > 10*time.Second {
		t.Fatalf("took %s", time.Since(start))
	}
	if len(evs) != 2 || evs[0].Text != "hello" || evs[1].Kind != KindResult {
		t.Fatalf("events = %+v", evs)
	}
}

func TestACLIRunThatExitsWithoutAResultReportsAnErrorWithStderr(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake.sh")
	os.WriteFile(script, []byte("#!/bin/sh\necho partial\necho 'not logged in' >&2\nexit 1\n"), 0o755)
	run, err := StartCLI(context.Background(), &echoParser{}, dir, Jail{Home: dir}.Env(nil), script)
	if err != nil {
		t.Fatal(err)
	}
	evs := Collect(context.Background(), run)
	last := evs[len(evs)-1]
	if last.Kind != KindError || !strings.Contains(last.Text, "not logged in") || !strings.Contains(last.Text, "exit status 1") {
		t.Fatalf("events = %+v", evs)
	}
}

func TestStopEndsACLIRunAndItsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake.sh")
	os.WriteFile(script, []byte("#!/bin/sh\necho started\nsleep 300\n"), 0o755)
	run, err := StartCLI(context.Background(), &echoParser{}, dir, Jail{Home: dir}.Env(nil), script)
	if err != nil {
		t.Fatal(err)
	}
	<-run.Events()
	run.Stop()
	run.Stop() // twice is fine
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-run.Events():
			if !ok {
				return
			}
			if ev.Kind == KindError && ev.Text != "stopped" {
				t.Fatalf("unexpected %+v", ev)
			}
		case <-deadline:
			t.Fatal("the run did not end after Stop")
		}
	}
}

// The supervisor's own keys and the commands that reach past the site are
// denied to every conversation; docker only to sites (on a rootless daemon
// it mounts the supervisor's whole HOME).
func TestTheJailDeniesTheMachinesKeysAndCommands(t *testing.T) {
	defer func(f func() string) { realHome = f }(realHome)
	h := t.TempDir()
	realHome = func() string { return h }
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "sites", "a.test"), 0o755)
	site := NewJail(root, filepath.Join(root, "sites", "a.test"), filepath.Join(root, "data", "homes", "a.test"))
	house := NewJail(root, root, filepath.Join(root, "data", "homes", "middleware"))
	for _, j := range []Jail{site, house} {
		deny := strings.Join(j.Deny, "\n")
		for _, s := range []string{".ssh", ".claude", ".cloudflare", ".aws"} {
			if !strings.Contains(deny, filepath.Join(h, s)) {
				t.Errorf("%s not denied to %s", s, j.SiteDir)
			}
		}
		if !strings.Contains(strings.Join(j.Commands, ","), "sudo") || !strings.Contains(strings.Join(j.Commands, ","), "git push --force") {
			t.Errorf("commands: %v", j.Commands)
		}
	}
	if !strings.Contains(strings.Join(site.Commands, ","), "docker") || strings.Contains(strings.Join(house.Commands, ","), "docker") {
		t.Fatalf("docker: site %v, house %v", site.Commands, house.Commands)
	}
}

// A project kept inside another site and linked into sites/ (V1's Mask
// portals): the other site is an ancestor of this one's real folder, so it
// is denied entry by entry around the way down, never whole, or the
// conversation could not write its own code.
func TestASiteInsideAnotherIsNotLockedOutOfItself(t *testing.T) {
	defer func(f func() string) { realHome = f }(realHome)
	realHome = func() string { return "" }
	root := t.TempDir()
	host := filepath.Join(root, "sites", "host.test")
	inner := filepath.Join(host, "portals", "p1")
	os.MkdirAll(inner, 0o755)
	os.MkdirAll(filepath.Join(host, "portals", "p2"), 0o755)
	os.WriteFile(filepath.Join(host, ".env"), []byte("SECRET=1"), 0o600)
	os.Symlink(inner, filepath.Join(root, "sites", "p1.test"))
	j := NewJail(root, filepath.Join(root, "sites", "p1.test"), filepath.Join(root, "data", "homes", "p1.test"))
	deny := map[string]bool{}
	for _, d := range j.Deny {
		deny[d] = true
	}
	if deny[host] || deny[filepath.Join(host, "portals")] || deny[inner] {
		t.Fatalf("the site's own way down is denied: %v", j.Deny)
	}
	for _, want := range []string{filepath.Join(host, ".env"), filepath.Join(host, "portals", "p2")} {
		if !deny[want] {
			t.Errorf("%s is not denied: %v", want, j.Deny)
		}
	}
}

// A supervisor holding an ambient capability starts its agents through
// setpriv, which drops it: the real setpriv, with this process's status
// said to carry one, and the agent reading its own.
func TestAnAgentStartsWithoutTheSupervisorsAmbientCapabilities(t *testing.T) {
	if _, err := os.Stat("/usr/bin/setpriv"); err != nil {
		t.Skip("no setpriv here")
	}
	dir := t.TempDir()
	status := filepath.Join(dir, "status")
	os.WriteFile(status, []byte("Name:\tmiddleware\nCapAmb:\t0000000000000400\n"), 0o644)
	old := procStatus
	procStatus = status
	defer func() { procStatus = old }()
	if p, a := withoutAmbientCaps("/bin/agent", []string{"-p", "x"}); p != "/usr/bin/setpriv" || strings.Join(a, " ") != "--ambient-caps -all --inh-caps -all -- /bin/agent -p x" {
		t.Fatalf("%s %v", p, a)
	}
	script := filepath.Join(dir, "fake.sh")
	os.WriteFile(script, []byte("#!/bin/sh\ngrep CapAmb /proc/self/status\necho \"args:$*\"\necho END\n"), 0o755)
	run, err := StartCLI(context.Background(), &echoParser{}, dir, Jail{Home: dir}.Env(nil), script, "one", "two")
	if err != nil {
		t.Fatal(err)
	}
	evs := Collect(context.Background(), run)
	if len(evs) != 3 || !strings.HasSuffix(evs[0].Text, "0000000000000000") || evs[1].Text != "args:one two" {
		t.Fatalf("events = %+v", evs)
	}
	// Without one, nothing changes.
	os.WriteFile(status, []byte("CapAmb:\t0000000000000000\n"), 0o644)
	if p, _ := withoutAmbientCaps("/bin/agent", nil); p != "/bin/agent" {
		t.Fatalf("wrapped without a reason: %s", p)
	}
}

// A site's conversation reads its own logs, and only them: asked to look at
// its errors, comparaloo's could only ask for them to be pasted, since the
// whole of data/ was denied. They are lent (readable, not writable); the
// other sites' logs and the tasks' stay denied. And the guide in its HOME
// is never denied.
func TestASiteJailLendsItsOwnLogsAndNotTheOthers(t *testing.T) {
	root := makeRoot(t)
	for _, d := range []string{"data/logs/a.test", "data/logs/b.test", "data/logs/tasks"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	site := filepath.Join(root, "sites", "a.test")
	home := filepath.Join(root, "data", "homes", "a.test")
	j := NewJail(root, site, home)
	own := filepath.Join(root, "data", "logs", "a.test")
	guide := filepath.Join(home, ".claude", "skills", "middleware", "SKILL.md")
	for _, d := range j.Deny {
		if d == own || strings.HasPrefix(own, d+"/") || d == filepath.Join(root, "data", "logs") {
			t.Errorf("its own logs are denied by %s", d)
		}
		if strings.HasPrefix(guide, d+"/") {
			t.Errorf("the guide is denied by %s", d)
		}
	}
	lent := strings.Join(j.Lent, "\n")
	if !strings.Contains(lent, own) {
		t.Errorf("its own logs are not lent (readable, not writable): %v", j.Lent)
	}
	denied := strings.Join(j.Deny, "\n")
	for _, want := range []string{filepath.Join(root, "data", "logs", "b.test"), filepath.Join(root, "data", "logs", "tasks")} {
		if !strings.Contains(denied, want) {
			t.Errorf("%s is not denied", want)
		}
	}
}
