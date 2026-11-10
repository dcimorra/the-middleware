package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/tasks"
)

// `middleware site tasks` round-trips through the real API: what it
// saves is the site's, it lists only the site's, it refuses a name the
// house holds, and every mistake in the arguments is an error (a non-zero
// exit), never a quiet success.
func TestSiteTasksCommandsRoundTripThroughTheSocket(t *testing.T) {
	root, cl := realServer(t)
	for _, d := range []string{"a.test", "b.test"} {
		if out, err := run(t, cl, "site", "add", d); err != nil {
			t.Fatalf("add %s: %q %v", d, out, err)
		}
	}
	os.MkdirAll(filepath.Join(root, "tasks"), 0o755)
	os.WriteFile(tasks.Path(root, "house"), []byte(`{"version": 2, "schedule": "0 3 * * *", "command": ["true"]}`), 0o644)

	out, err := run(t, cl, "site", "tasks", "a.test")
	if err != nil || strings.TrimSpace(out) != "no tasks for a.test" {
		t.Fatalf("empty list: %q %v", out, err)
	}

	file := filepath.Join(t.TempDir(), "mine.json")
	os.WriteFile(file, []byte(`{"version": 2, "schedule": "0 5 * * *", "command": ["sh", "-c", "echo one; echo two"]}`), 0o644)
	out, err = run(t, cl, "site", "tasks", "a.test", "save", "mine", file)
	if err != nil || !strings.Contains(out, "Saved mine for a.test") {
		t.Fatalf("save from a file: %q %v", out, err)
	}
	if saved, err := tasks.Load(root, "mine"); err != nil || saved.Site != "a.test" {
		t.Fatalf("saved: %+v %v", saved, err)
	}
	out, err = runIn(t, cl, `{"version": 2, "schedule": "0 6 * * *", "disabled": true, "command": ["true"]}`, "site", "tasks", "a.test", "save", "other", "-", "--json")
	if err != nil || !strings.HasPrefix(out, "{") || !strings.Contains(out, `"site": "a.test"`) {
		t.Fatalf("save from stdin, --json: %q %v", out, err)
	}

	out, err = run(t, cl, "site", "tasks", "a.test", "list")
	if err != nil || !strings.HasPrefix(out, "NAME") || !strings.Contains(out, "mine") || !strings.Contains(out, "other  0 6 * * *  disabled") || strings.Contains(out, "house") {
		t.Fatalf("list:\n%s%v", out, err)
	}
	out, err = run(t, cl, "site", "tasks", "a.test", "--json")
	var list []tasks.Status
	if err != nil || json.Unmarshal([]byte(out), &list) != nil || len(list) != 2 {
		t.Fatalf("list --json: %q %v", out, err)
	}

	// The house's task is not a site's to replace, nor b.test's to see.
	if out, err = runIn(t, cl, `{"version": 2, "schedule": "* * * * *", "command": ["true"]}`, "site", "tasks", "a.test", "save", "house", "-"); err == nil || !strings.Contains(err.Error(), "the house's") {
		t.Fatalf("save over the house's task: %q %v", out, err)
	}
	if out, err = run(t, cl, "site", "tasks", "b.test"); err != nil || !strings.Contains(out, "no tasks for b.test") {
		t.Fatalf("b.test's list: %q %v", out, err)
	}
	if _, err = run(t, cl, "site", "tasks", "b.test", "rm", "mine"); err == nil {
		t.Fatal("b.test removed a.test's task")
	}

	out, err = run(t, cl, "site", "tasks", "a.test", "run", "mine")
	if err != nil || !strings.Contains(out, "Started mine") {
		t.Fatalf("run: %q %v", out, err)
	}
	for wait := time.Now().Add(10 * time.Second); len(tasks.LoadRecord(root, "mine").Runs) == 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(wait) {
			t.Fatal("the task did not run")
		}
	}
	if out, err = run(t, cl, "site", "tasks", "a.test", "log", "mine"); err != nil || !strings.HasSuffix(out, "one\ntwo\n") {
		t.Fatalf("log: %q %v", out, err)
	}
	if out, err = run(t, cl, "site", "tasks", "a.test", "log", "mine", "--lines", "1"); err != nil || out != "two\n" {
		t.Fatalf("log --lines 1: %q %v", out, err)
	}
	// The run's record is written before the scheduler lets go of the
	// task, so for a moment the list still says "running" (seen on CI).
	for wait := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if out, err = run(t, cl, "site", "tasks", "a.test", "list"); err == nil && strings.Contains(out, " ok (") {
			break
		}
		if time.Now().After(wait) {
			t.Fatalf("list after the run:\n%s%v", out, err)
		}
	}
	if _, err = run(t, cl, "site", "tasks", "a.test", "stop", "mine"); err == nil {
		t.Fatal("stop with nothing running succeeded")
	}

	out, err = run(t, cl, "site", "tasks", "a.test", "rm", "mine")
	if err != nil || !strings.Contains(out, "Deleted mine") {
		t.Fatalf("rm: %q %v", out, err)
	}
	if _, err := os.Stat(tasks.Path(root, "mine")); !os.IsNotExist(err) {
		t.Fatal("the task is still there")
	}
	if _, err = run(t, cl, "site", "tasks", "a.test", "rm", "house"); err == nil {
		t.Fatal("a site removed the house's task")
	}
	if _, err := os.Stat(tasks.Path(root, "house")); err != nil {
		t.Fatal(err)
	}

	// The scope is one a token can be made with from here.
	if out, err = run(t, cl, "token", "add", "cron-ui", "--scope", "tasks:a.test"); err != nil || !strings.Contains(out, "mw_") {
		t.Fatalf("token add --scope tasks:a.test: %q %v", out, err)
	}

	for _, args := range [][]string{
		{"site", "tasks"},
		{"site", "tasks", "a.test", "save", "x"},
		{"site", "tasks", "a.test", "save", "x", filepath.Join(t.TempDir(), "none.json")},
		{"site", "tasks", "a.test", "run"},
		{"site", "tasks", "a.test", "run", "a", "b"},
		{"site", "tasks", "a.test", "log", "Not A Name"},
		{"site", "tasks", "a.test", "list", "extra"},
		{"site", "tasks", "a.test", "bogus"},
		{"site", "tasks", "nope.test"},
	} {
		if out, err := run(t, cl, args...); err == nil {
			t.Errorf("%v succeeded: %q", args, out)
		}
	}
}
