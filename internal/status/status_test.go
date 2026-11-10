package status

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAnOutageIsCountedFromItsFirstMinuteToItsLast(t *testing.T) {
	b := New(t.TempDir())
	clock := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return clock }
	tick := func(s State, why string) {
		b.Observe("a.test", s, why)
		clock = clock.Add(time.Minute)
	}
	for i := 0; i < 10; i++ {
		tick(Up, "")
	}
	// One bad minute is a restart nobody saw: counted, but no outage.
	tick(Down, "exit status 1")
	tick(Up, "")
	began := clock
	for i := 0; i < 3; i++ {
		tick(Down, "exit status 1\n/home/x/site.log")
	}
	ended := clock
	tick(Up, "")
	// Maintenance is shown and not held against the site.
	for i := 0; i < 5; i++ {
		tick(Maintenance, "")
	}
	tick(Unknown, "")
	b.Flush()

	again := New(b.dir)
	again.now = b.now
	rep := again.Report("a.test")
	if rep.State != Maintenance {
		t.Fatalf("state after a restart: %q", rep.State)
	}
	if len(rep.Incidents) != 1 || !rep.Incidents[0].From.Equal(began) || !rep.Incidents[0].To.Equal(ended) || rep.Incidents[0].Reason != "exit status 1" {
		t.Fatalf("incidents: %+v (want %v → %v)", rep.Incidents, began, ended)
	}
	today := rep.Days[len(rep.Days)-1]
	if today.Up != 12 || today.Down != 4 || today.Maintenance != 5 || rep.Uptime["1"] != 12.0/16 || rep.Uptime["90"] != 12.0/16 {
		t.Fatalf("today: %+v uptime %v", today, rep.Uptime)
	}
	if rep.Days[0].Uptime != -1 {
		t.Fatalf("a day with nothing counted has an uptime: %+v", rep.Days[0])
	}

	pub := PublicOf("a.test", rep, clock)
	html := string(HTML("", pub))
	if !strings.Contains(html, "Under maintenance") || strings.Contains(html, "exit status") || !strings.Contains(html, "3 min") {
		t.Fatalf("page: %s", html)
	}
	r := httptest.NewRequest("GET", "/status?format=json", nil)
	w := httptest.NewRecorder()
	Serve(w, r, "", pub)
	if !strings.Contains(w.Body.String(), `"state":"maintenance"`) || strings.Contains(w.Body.String(), "exit status") {
		t.Fatalf("json: %s", w.Body.String())
	}
}

func TestAnOutageOpenWhenTheHouseStoppedIsClosedByTheNextMinuteUp(t *testing.T) {
	b := New(t.TempDir())
	clock := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return clock }
	for i := 0; i < 3; i++ {
		b.Observe("a.test", Down, "x")
		clock = clock.Add(time.Minute)
	}
	b.Flush()
	again := New(b.dir)
	again.now = b.now
	if rep := again.Report("a.test"); len(rep.Incidents) != 1 || !rep.Incidents[0].To.IsZero() {
		t.Fatalf("not open: %+v", rep.Incidents)
	}
	again.Observe("a.test", Up, "")
	if rep := again.Report("a.test"); !rep.Incidents[0].To.Equal(clock) {
		t.Fatalf("not closed: %+v", rep.Incidents)
	}
}

func TestAMinuteDownIsNeverShownAsAHundredPercent(t *testing.T) {
	if got := percent(1 - 1.0/100000); got != "99.99%" {
		t.Fatal(got)
	}
	if got := percent(0.995); got != "99.5%" {
		t.Fatal(got)
	}
	if percent(1) != "100%" || percent(-1) != "—" {
		t.Fatal("ends")
	}
}

// A renamed site's record follows its file: released, the old name is not
// written again and the new one has the minutes counted.
func TestAReleasedRecordIsWrittenAndNotBroughtBack(t *testing.T) {
	dir := t.TempDir()
	b := New(dir)
	b.Observe("old.test", Up, "")
	b.Release("old.test")
	if err := os.Rename(filepath.Join(dir, "old.test.json"), filepath.Join(dir, "new.test.json")); err != nil {
		t.Fatalf("released without writing: %v", err)
	}
	b.Flush()
	if _, err := os.Stat(filepath.Join(dir, "old.test.json")); !os.IsNotExist(err) {
		t.Fatal("the old name was written again")
	}
	if rep := b.Report("new.test"); rep.Uptime["1"] != 1 {
		t.Fatalf("the new name does not have what was counted: %+v", rep.Uptime)
	}
}
