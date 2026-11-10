package tasks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/events"
)

// clock is a movable Now for the scheduler: a test sets the minute and the
// next tick sees it, instead of waiting for a real minute to pass.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"tasks", "data", "sites/a.test"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	os.WriteFile(filepath.Join(root, "sites", "a.test", ".pointer"), []byte("build-7\n"), 0o644)
	os.WriteFile(filepath.Join(root, "sites", "a.test", "site.json"), []byte(`{"version":2,"run":{"command":["./server"],"env":{"FOO":"bar"},"env_from_file":{"BUILD":".pointer"}}}`), 0o644)
	return root
}

func writeTask(t *testing.T, root, name, body string) {
	t.Helper()
	if err := os.WriteFile(Path(root, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func collect(bus *events.Bus) (func(typ string) []events.Event, func()) {
	ch, unsub := bus.Subscribe()
	var mu sync.Mutex
	var all []events.Event
	done := make(chan struct{})
	go func() {
		for ev := range ch {
			mu.Lock()
			all = append(all, ev)
			mu.Unlock()
			select {
			case <-done:
				return
			default:
			}
		}
	}()
	return func(typ string) []events.Event {
			mu.Lock()
			defer mu.Unlock()
			var out []events.Event
			for _, e := range all {
				if e.Type == typ {
					out = append(out, e)
				}
			}
			return out
		}, func() {
			close(done)
			unsub()
		}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestADueTaskRunsInsideItsSiteWithTheSitesEnvironmentAndIsRecorded(t *testing.T) {
	root := newRoot(t)
	out := filepath.Join(root, "sites", "a.test", "out.txt")
	writeTask(t, root, "nightly", `{"version":2,"schedule":"30 3 * * *","site":"a.test","command":["/bin/sh","-c","echo $FOO $BUILD $MIDDLEWARE_DOMAIN $(pwd) $EXTRA > out.txt; echo logged"],"env":{"EXTRA":"$DOMAIN"}}`)
	c := &clock{t: time.Date(2026, 10, 19, 3, 29, 0, 0, time.Local)}
	bus := events.New("")
	get, stop := collect(bus)
	defer stop()
	s := NewScheduler(Options{Root: root, Bus: bus, Tick: 20 * time.Millisecond, Now: c.now})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(out); err == nil {
		t.Fatal("ran before its minute")
	}
	c.advance(time.Minute)
	waitUntil(t, "the run to be recorded", func() bool { return len(LoadRecord(root, "nightly").Runs) == 1 })
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	want := "bar build-7 a.test " + filepath.Join(root, "sites", "a.test") + " a.test"
	if strings.TrimSpace(string(data)) != want {
		t.Fatalf("got %q, want %q", strings.TrimSpace(string(data)), want)
	}
	logData, _ := os.ReadFile(LogPath(root, "nightly"))
	if !strings.Contains(string(logData), "logged") {
		t.Fatalf("log: %q", logData)
	}
	run := LoadRecord(root, "nightly").Runs[0]
	if run.Status != StatusOK || run.ExitCode != 0 || run.Trigger != "schedule" || !run.At.Equal(c.now()) {
		t.Fatalf("run = %+v", run)
	}
	if len(get("task.started")) != 1 || len(get("task.done")) != 1 {
		t.Fatalf("events: started=%d done=%d", len(get("task.started")), len(get("task.done")))
	}
	// The same minute does not fire twice; the next day does.
	time.Sleep(100 * time.Millisecond)
	if n := len(LoadRecord(root, "nightly").Runs); n != 1 {
		t.Fatalf("ran %d times in one minute", n)
	}
	c.advance(24 * time.Hour)
	waitUntil(t, "the second run", func() bool { return len(LoadRecord(root, "nightly").Runs) == 2 })
	st := s.List()
	if len(st) != 1 || st[0].Name != "nightly" || st[0].Last == nil || st[0].Next.IsZero() || st[0].Next.Before(c.now()) {
		t.Fatalf("list = %+v", st)
	}
}

func TestAFailingCommandIsRecordedWithItsExitCode(t *testing.T) {
	root := newRoot(t)
	writeTask(t, root, "broken", `{"version":2,"schedule":"@daily","command":["/bin/sh","-c","echo oops; exit 3"]}`)
	bus := events.New("")
	get, stop := collect(bus)
	defer stop()
	s := NewScheduler(Options{Root: root, Bus: bus})
	err := s.RunNow("broken")
	if err == nil || !strings.Contains(err.Error(), "exit 3") {
		t.Fatalf("err = %v", err)
	}
	run := LoadRecord(root, "broken").Runs[0]
	if run.Status != StatusFailed || run.ExitCode != 3 || run.Trigger != "operator" {
		t.Fatalf("run = %+v", run)
	}
	waitUntil(t, "task.failed", func() bool { return len(get("task.failed")) == 1 })
}

func TestATimeoutKillsTheTaskAndItsChildren(t *testing.T) {
	root := newRoot(t)
	marker := filepath.Join(root, "alive")
	writeTask(t, root, "slow", `{"version":2,"schedule":"@daily","command":["/bin/sh","-c","(sleep 300; touch `+marker+`) & sleep 300"],"timeout":"300ms"}`)
	s := NewScheduler(Options{Root: root})
	start := time.Now()
	err := s.RunNow("slow")
	if err == nil || !strings.Contains(err.Error(), "killed after") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 15*time.Second {
		t.Fatalf("took %s", time.Since(start))
	}
	if run := LoadRecord(root, "slow").Runs[0]; run.Status != StatusTimeout {
		t.Fatalf("run = %+v", run)
	}
}

func TestOverlapSkipRecordsASkipAndQueueRunsAgain(t *testing.T) {
	root := newRoot(t)
	gate := filepath.Join(root, "gate")
	body := func(overlap string) string {
		return `{"version":2,"schedule":"* * * * *","command":["/bin/sh","-c","while [ ! -e ` + gate + ` ]; do sleep 0.05; done"],"overlap":"` + overlap + `"}`
	}
	writeTask(t, root, "skipper", body("skip"))
	writeTask(t, root, "queuer", body("queue"))
	c := &clock{t: time.Date(2026, 10, 19, 10, 0, 0, 0, time.Local)}
	bus := events.New("")
	get, stop := collect(bus)
	defer stop()
	s := NewScheduler(Options{Root: root, Bus: bus, Tick: 20 * time.Millisecond, Now: c.now})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	c.advance(time.Minute)
	waitUntil(t, "both to start", func() bool { return len(get("task.started")) == 2 })
	c.advance(time.Minute)
	waitUntil(t, "the skip and the queue", func() bool { return len(get("task.skipped")) == 1 && len(get("task.queued")) == 1 })
	if runs := LoadRecord(root, "skipper").Runs; len(runs) != 1 || runs[0].Status != StatusSkipped {
		t.Fatalf("skipper runs = %+v", runs)
	}
	if err := s.RunNow("queuer"); err != ErrRunning {
		t.Fatalf("RunNow while running: %v", err)
	}
	os.WriteFile(gate, nil, 0o644)
	waitUntil(t, "the queued run", func() bool {
		runs := LoadRecord(root, "queuer").Runs
		return len(runs) == 2 && runs[1].Status == StatusOK
	})
	cancel()
	s.Wait()
	if runs := LoadRecord(root, "skipper").Runs; len(runs) != 2 {
		t.Fatalf("skipper runs = %+v", runs)
	}
}

func TestAnInvalidTaskFileIsReportedAndListedNotIgnored(t *testing.T) {
	root := newRoot(t)
	writeTask(t, root, "typo", `{"version":2,"scheduel":"@daily","command":["true"]}`)
	bus := events.New("")
	get, stop := collect(bus)
	defer stop()
	s := NewScheduler(Options{Root: root, Bus: bus})
	s.tick()
	s.tick()
	waitUntil(t, "one task.invalid", func() bool { return len(get("task.invalid")) == 1 })
	if evs := get("task.invalid"); !strings.Contains(evs[0].Message, "scheduel") {
		t.Fatalf("invalid events = %+v", evs)
	}
	st := s.List()
	if len(st) != 1 || st[0].Error == "" {
		t.Fatalf("list = %+v", st)
	}
	writeTask(t, root, "typo", `{"version":2,"schedule":"@daily","command":["true"]}`)
	s.tick()
	waitUntil(t, "task.valid after the fix", func() bool { return len(get("task.valid")) == 1 })
	if err := s.RunNow("typo"); err != nil {
		t.Fatal(err)
	}
}

func TestTaskValidationRejectsWhatCannotRun(t *testing.T) {
	for _, bad := range []string{
		`{"version":1,"schedule":"@daily","command":["true"]}`,
		`{"version":2,"schedule":"every day","command":["true"]}`,
		`{"version":2,"schedule":"@daily","command":[]}`,
		`{"version":2,"schedule":"@daily","command":["true"],"site":"Not A Domain"}`,
		`{"version":2,"schedule":"@daily","command":["true"],"overlap":"wait"}`,
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("accepted: %s", bad)
		}
	}
	root := newRoot(t)
	if err := Save(root, "../escape", &Task{Version: 2, Schedule: "@daily", Command: []string{"true"}}); err == nil {
		t.Fatal("a name with a slash was accepted")
	}
	if err := Save(root, "ok-task", &Task{Version: 2, Schedule: "@daily", Command: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	if names := Names(root); len(names) != 1 || names[0] != "ok-task" {
		t.Fatalf("names = %v", names)
	}
}

func TestATaskForAMissingSiteFailsVisibly(t *testing.T) {
	root := newRoot(t)
	writeTask(t, root, "orphan", `{"version":2,"schedule":"@daily","site":"gone.test","command":["true"]}`)
	bus := events.New("")
	get, stop := collect(bus)
	defer stop()
	s := NewScheduler(Options{Root: root, Bus: bus})
	if err := s.RunNow("orphan"); err == nil || !strings.Contains(err.Error(), "no such folder") {
		t.Fatalf("err = %v", err)
	}
	if run := LoadRecord(root, "orphan").Runs[0]; run.Status != StatusError {
		t.Fatalf("run = %+v", run)
	}
	waitUntil(t, "task.failed", func() bool { return len(get("task.failed")) == 1 })
}

// The schedule of a task whose site is not there (archived, moved to
// another node) says so once and runs nothing: it failed at every run
// before, an error in the journal each minute. The folder back, it runs.
func TestAScheduledTaskWaitsForItsSitesFolderInsteadOfFailingEveryRun(t *testing.T) {
	root := newRoot(t)
	writeTask(t, root, "orphan", `{"version":2,"schedule":"* * * * *","site":"away.test","command":["/bin/sh","-c","echo ran > out.txt"]}`)
	c := &clock{t: time.Date(2026, 10, 19, 3, 29, 30, 0, time.Local)}
	bus := events.New("")
	get, stop := collect(bus)
	defer stop()
	s := NewScheduler(Options{Root: root, Bus: bus, Now: c.now})
	s.tick()
	for range 3 {
		c.advance(time.Minute)
		s.tick()
	}
	s.Wait()
	waitUntil(t, "one task.invalid", func() bool { return len(get("task.invalid")) == 1 })
	if n := len(LoadRecord(root, "orphan").Runs); n != 0 {
		t.Fatalf("%d runs recorded for a site that is not there", n)
	}
	if failed := get("task.failed"); len(failed) != 0 {
		t.Fatalf("failed %d times", len(failed))
	}
	if st := s.List(); len(st) != 1 || !strings.Contains(st[0].Error, "no such folder") {
		t.Fatalf("the list does not say why it is not running: %+v", st)
	}
	if err := os.Rename(filepath.Join(root, "sites", "a.test"), filepath.Join(root, "sites", "away.test")); err != nil {
		t.Fatal(err)
	}
	c.advance(time.Minute)
	s.tick()
	waitUntil(t, "a run once the folder is back", func() bool {
		_, err := os.Stat(filepath.Join(root, "sites", "away.test", "out.txt"))
		return err == nil
	})
	waitUntil(t, "task.valid", func() bool { return len(get("task.valid")) == 1 })
}

// A run that outlives its supervisor is adopted by the next one and
// recorded when it ends, instead of running unrecorded and leaving its
// task looking stuck.
func TestARunInProgressIsAdoptedAfterARestart(t *testing.T) {
	root := newRoot(t)
	writeTask(t, root, "slow", `{"version":2,"schedule":"@daily","command":["sh","-c","sleep 1"]}`)
	a := NewScheduler(Options{Root: root, Bus: events.New("")})
	done := make(chan error, 1)
	go func() { done <- a.RunNow("slow") }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(root, "data", "tasks", "running", "slow.json")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the run was not saved as running")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The second supervisor adopts it while it still runs.
	b := NewScheduler(Options{Root: root, Bus: events.New("")})
	b.adoptAll()
	if l := b.List(); !l[0].Running {
		t.Fatalf("not adopted: %+v", l)
	}
	<-done
	b.Wait()
	runs := LoadRecord(root, "slow").Runs
	statuses := ""
	for _, r := range runs {
		statuses += r.Status + " "
	}
	if !strings.Contains(statuses, StatusEnded) {
		t.Fatalf("the adopted run was not recorded: %s", statuses)
	}
	if _, err := os.Stat(filepath.Join(root, "data", "tasks", "running", "slow.json")); err == nil {
		t.Fatal("the running file was left behind")
	}
}

func TestARunningTaskCanBeStopped(t *testing.T) {
	root := newRoot(t)
	writeTask(t, root, "long", `{"version":2,"schedule":"@daily","command":["sleep","30"]}`)
	s := NewScheduler(Options{Root: root, Bus: events.New("")})
	done := make(chan error, 1)
	go func() { done <- s.RunNow("long") }()
	time.Sleep(300 * time.Millisecond)
	if err := s.Stop("long"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the run did not stop")
	}
	runs := LoadRecord(root, "long").Runs
	if runs[len(runs)-1].Status != StatusStopped {
		t.Fatalf("status %s", runs[len(runs)-1].Status)
	}
}

// A schedule is read in its task's zone: 00:50 in Madrid is 22:50 UTC in
// summer and 23:50 in winter, on a machine whose clock is UTC.
func TestATasksScheduleIsReadInItsTimezone(t *testing.T) {
	root := newRoot(t)
	writeTask(t, root, "night", `{"version":2,"schedule":"50 0 * * *","command":["/bin/true"],"timezone":"Europe/Madrid"}`)
	summer := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	s := NewScheduler(Options{Root: root, Now: func() time.Time { return summer }})
	if next := s.List()[0].Next.UTC(); !next.Equal(time.Date(2026, 7, 1, 22, 50, 0, 0, time.UTC)) {
		t.Fatalf("summer: next %s", next)
	}
	winter := time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)
	s = NewScheduler(Options{Root: root, Now: func() time.Time { return winter }})
	if next := s.List()[0].Next.UTC(); !next.Equal(time.Date(2026, 12, 1, 23, 50, 0, 0, time.UTC)) {
		t.Fatalf("winter: next %s", next)
	}
	if _, err := Parse([]byte(`{"version":2,"schedule":"* * * * *","command":["x"],"timezone":"Mars/Olympus"}`)); err == nil {
		t.Fatal("a zone that does not exist was accepted")
	}
}

// Two writers of a task's history at once lose nothing: it is a read, an
// append and a write, and without its lock one run vanished, which made
// TestARunInProgressIsAdoptedAfterARestart fail now and then.
func TestConcurrentRunsAreAllRecorded(t *testing.T) {
	root := newRoot(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := appendRun(root, "busy", Run{At: time.Now(), Status: StatusOK}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := len(LoadRecord(root, "busy").Runs); n != 20 {
		t.Fatalf("%d of 20 runs recorded", n)
	}
}
