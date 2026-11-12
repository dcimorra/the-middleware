package api

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/relocate"
	"github.com/dcimorra/the-middleware/internal/tasks"
)

// fastRecovery makes a move put right after a restart begin at once and
// wait little for the target.
func fastRecovery(t *testing.T) {
	first, ready, poll := recoverFirst, recoverReady, recoverPoll
	recoverFirst, recoverReady, recoverPoll = 0, 300*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { recoverFirst, recoverReady, recoverPoll = first, ready, poll })
}

// stoppedForAMove is r.test on f as a move left it when the house died:
// its nightly task turned off, the site stopped, and the journal written.
func stoppedForAMove(t *testing.T, f *fixture, step string) []byte {
	t.Helper()
	dir := filepath.Join(f.root, "sites", "r.test")
	os.MkdirAll(filepath.Join(dir, "public"), 0o755)
	os.WriteFile(filepath.Join(dir, "public", "index.html"), []byte("here"), 0o644)
	original := []byte(`{"version":2,"static":{"root":"public"}}`)
	os.WriteFile(filepath.Join(dir, "site.json"), original, 0o644)
	os.MkdirAll(filepath.Dir(tasks.Path(f.root, "r-nightly")), 0o755)
	os.WriteFile(tasks.Path(f.root, "r-nightly"), []byte(`{"version":2,"schedule":"0 3 * * *","site":"r.test","command":["true"]}`), 0o644)

	mgr, _ := f.srv.sitesManager()
	src := siteSource{s: f.srv, mgr: mgr}
	held, err := src.HoldTasks("r.test")
	if err != nil || len(held) != 1 {
		t.Fatalf("held %v: %v", held, err)
	}
	// What StopForMove writes, with an engine that would obey it.
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(`{"version":2,"disabled":true,"static":{"root":"public"}}`), 0o644)
	job, _ := f.srv.newMove("r.test", "node-b", movePlan(false, false, false))
	f.srv.advance(job, relocate.StepStop, "stopping r.test here for the last copy")
	os.MkdirAll(filepath.Dir(relocate.JournalPath(f.root, "r.test")), 0o755)
	j := relocate.Journal{Domain: "r.test", Target: "b-id", ID: job.ID, Step: step, Manifest: original, Held: held, Started: time.Now()}
	writeJSON(t, relocate.JournalPath(f.root, "r.test"), j)
	return original
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	raw, _ := json.Marshal(v)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// afterRestart is f's house started again over the same folder, putting
// right what its journals say, against b.
func afterRestart(t *testing.T, f, b *fixture) moveJob {
	t.Helper()
	again := New(Options{Root: f.root, Sites: f.srv.opts.Sites, Bus: f.bus})
	again.recoverMoves(func(id string) (relocate.Target, bool) { return meshTo(b), id == "b-id" })
	if !again.MovesRunning() {
		t.Fatal("a move being put right is not said to be running")
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if list := again.listMoves("r.test"); len(list) == 1 && list[0].Done {
			if again.MovesRunning() {
				t.Fatal("said running once it ended")
			}
			return list[0]
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the move was never put right")
	return moveJob{}
}

func taskOn(t *testing.T, root string) bool {
	t.Helper()
	raw, _ := os.ReadFile(tasks.Path(root, "r-nightly"))
	tk, err := tasks.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return !tk.Disabled
}

// A restart of the house mid-move used to leave the site stopped here for
// good, its tasks off: what it had was in the move's memory. Before the
// commit, the house starts it again as it was when it comes back, turns
// its tasks on, and has the target throw away what it received.
func TestARestartBeforeTheCommitStartsTheSiteHereAgain(t *testing.T) {
	fastRecovery(t)
	a, b := newFixture(t).withToken(), newFixture(t).withToken()
	original := stoppedForAMove(t, a, relocate.JournalStop)
	staging := relocate.Receiver{Root: b.root}.StagingSite("r.test")
	os.MkdirAll(staging, 0o755)

	job := afterRestart(t, a, b)
	if !strings.Contains(job.Error, "runs here again") {
		t.Fatalf("job: %+v", job)
	}
	if got, _ := os.ReadFile(filepath.Join(a.root, "sites", "r.test", "site.json")); string(got) != string(original) {
		t.Fatalf("manifest here: %s", got)
	}
	if !taskOn(t, a.root) {
		t.Fatal("its task is still off")
	}
	if _, err := os.Stat(filepath.Dir(staging)); err == nil {
		t.Fatal("the target kept what it received")
	}
	if _, err := os.Stat(relocate.JournalPath(a.root, "r.test")); err == nil {
		t.Fatal("the journal outlived the move")
	}
}

// After the commit only the target can say where the site is: one that
// answers there is finished (archived here, its tasks there), one that is
// not there is started here again.
func TestARestartAfterTheCommitAsksTheTarget(t *testing.T) {
	t.Run("up there", func(t *testing.T) {
		fastRecovery(t)
		a, b := newFixture(t).withToken(), newFixture(t).withToken()
		original := stoppedForAMove(t, a, relocate.JournalCommit)
		there := filepath.Join(b.root, "sites", "r.test")
		os.MkdirAll(filepath.Join(there, "public"), 0o755)
		os.WriteFile(filepath.Join(there, "site.json"), original, 0o644)

		job := afterRestart(t, a, b)
		if job.Error != "" || job.Report == nil || job.Report.Archive == "" {
			t.Fatalf("job: %+v", job)
		}
		if _, err := os.Stat(filepath.Join(a.root, "sites", "r.test")); err == nil {
			t.Fatal("the copy here is still in sites/")
		}
		if taskOn(t, a.root) {
			t.Fatal("its task runs here again, and there")
		}
		if _, err := os.Stat(relocate.JournalPath(a.root, "r.test")); err == nil {
			t.Fatal("the journal outlived the move")
		}
	})
	t.Run("not there", func(t *testing.T) {
		fastRecovery(t)
		a, b := newFixture(t).withToken(), newFixture(t).withToken()
		original := stoppedForAMove(t, a, relocate.JournalCommit)
		job := afterRestart(t, a, b)
		if !strings.Contains(job.Error, "runs here again") {
			t.Fatalf("job: %+v", job)
		}
		if got, _ := os.ReadFile(filepath.Join(a.root, "sites", "r.test", "site.json")); string(got) != string(original) {
			t.Fatalf("manifest here: %s", got)
		}
		if !taskOn(t, a.root) {
			t.Fatal("its task is still off")
		}
	})
	t.Run("no answer", func(t *testing.T) {
		fastRecovery(t)
		every, keep := recoverEvery, recoverFor
		recoverEvery, recoverFor = 20*time.Millisecond, 200*time.Millisecond
		t.Cleanup(func() { recoverEvery, recoverFor = every, keep })
		a, b := newFixture(t).withToken(), newFixture(t).withToken()
		stoppedForAMove(t, a, relocate.JournalCommit)
		b.http.Close()
		job := afterRestart(t, a, b)
		if !strings.Contains(job.Error, "did not answer") {
			t.Fatalf("job: %+v", job)
		}
		// Nothing guessed: still stopped here, the journal kept for the
		// next start.
		if got, _ := os.ReadFile(filepath.Join(a.root, "sites", "r.test", "site.json")); !strings.Contains(string(got), "disabled") {
			t.Fatalf("started here without the target's word: %s", got)
		}
		if _, err := os.Stat(relocate.JournalPath(a.root, "r.test")); err != nil {
			t.Fatal("the journal went")
		}
	})
}

// A move keeps its journal from just before the stop until it ends.
func TestAMoveWritesItsJournalBeforeItStopsTheSite(t *testing.T) {
	a, b := newFixture(t).withToken(), newFixture(t).withToken()
	dir := filepath.Join(a.root, "sites", "j.test")
	os.MkdirAll(filepath.Join(dir, "public"), 0o755)
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(`{"version":2,"static":{"root":"public"}}`), 0o644)
	mgr, _ := a.srv.sitesManager()
	var seen []string
	m := &relocate.Mover{Root: a.root, Source: siteSource{s: a.srv, mgr: mgr}, Poll: 20 * time.Millisecond, ReadyWait: 5 * time.Second,
		Journal: &relocate.Journal{Target: "b-id", ID: "x"}}
	_, err := m.Move(context.Background(), "j.test", meshTo(b), func(step, _ string) {
		if raw, err := os.ReadFile(relocate.JournalPath(a.root, "j.test")); err == nil {
			var j relocate.Journal
			json.Unmarshal(raw, &j)
			seen = append(seen, step+":"+j.Step)
		} else {
			seen = append(seen, step+":-")
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(seen, " ")
	if !strings.HasPrefix(got, "check:- copy:- stop:- final:stop commit:stop start:commit") || !strings.Contains(got, "cleanup:ready") {
		t.Fatalf("journal at each step: %s", got)
	}
	if _, err := os.Stat(relocate.JournalPath(a.root, "j.test")); err == nil {
		t.Fatal("the journal outlived the move")
	}
}
