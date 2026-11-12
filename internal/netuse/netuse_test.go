package netuse

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newBook(t *testing.T, dir string, c *clock, lim *Limits, said *[]Event) *Book {
	t.Helper()
	return New(Options{Dir: dir, Now: c.now,
		Limits: func(string) Limits {
			if lim == nil {
				return Limits{}
			}
			return *lim
		},
		Notify: func(e Event) {
			if said != nil {
				*said = append(*said, e)
			}
		}})
}

// Bytes added to a site's counter show in its rate while they move, in
// its hour, its day and its month, and are there again after a restart.
func TestBytesAreCountedByRateHourDayAndMonth(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: time.Date(2026, 11, 12, 14, 0, 0, 0, time.Local)}
	b := newBook(t, dir, c, nil, nil)
	ctr := b.Counter("shop.example")
	for i := 0; i < 10; i++ {
		ctr.In(1000)
		ctr.Out(5000)
		b.Tick()
		c.add(time.Second)
	}
	b.AddPorts("shop.example", 300, 700, 5*time.Second)
	r := b.Report("shop.example", c.now().AddDate(0, 0, -6), c.now())
	if r.RateIn != 1000 || r.RateOut != 5000 {
		t.Fatalf("rate %v in, %v out", r.RateIn, r.RateOut)
	}
	if r.Today.In != 10300 || r.Today.Out != 50700 || r.Today.PortsIn != 300 || r.Today.PortsOut != 700 {
		t.Fatalf("today %+v", r.Today)
	}
	if r.Month != r.Today || r.Period != r.Today || len(r.Days) != 7 || r.Days[6].Out != 50700 {
		t.Fatalf("month %+v, period %+v, days %+v", r.Month, r.Period, r.Days)
	}
	if r.HoursIn[14] != 10300 || r.HoursOut[14] != 50700 {
		t.Fatalf("hours %v %v", r.HoursIn, r.HoursOut)
	}
	if r.State != "ok" || r.Limits != nil {
		t.Fatalf("a site with no budget is %q, %+v", r.State, r.Limits)
	}
	// What came by the ports is drawn over the seconds after it was
	// read, whole.
	c.add(6 * time.Second)
	b.Tick()
	var sum int64
	for _, n := range b.Report("shop.example", c.now(), c.now()).SecondsOut {
		sum += n
	}
	if sum != 50700 {
		t.Fatalf("the last minute holds %d bytes out", sum)
	}

	b.Flush()
	again := newBook(t, dir, c, nil, nil)
	if got := again.Report("shop.example", c.now(), c.now()).Today; got != r.Today {
		t.Fatalf("after a restart %+v, was %+v", got, r.Today)
	}
	// The next month starts from nothing and keeps the one that ended.
	c.add(30 * 24 * time.Hour)
	again.Counter("shop.example").Out(42)
	again.Tick()
	now := again.Report("shop.example", c.now().AddDate(0, 0, -40), c.now())
	if now.Month.Out != 42 || now.Period.Out != 50742 {
		t.Fatalf("the month after: %+v, period %+v", now.Month, now.Period)
	}
	if rows := again.Summary([]string{"shop.example", "idle.example"}); len(rows) != 2 || rows[0].Domain != "shop.example" || rows[0].Month.Out != 42 {
		t.Fatalf("summary %+v", rows)
	}
}

// A budget warns once at its share, says once that it was reached, cuts
// only when told to, survives a restart without saying it all again, and
// lets the site back when the day ends or the budget is raised.
func TestABudgetWarnsThenCutsAndLetsGo(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: time.Date(2026, 11, 12, 10, 0, 0, 0, time.Local)}
	lim := &Limits{Daily: 1000, WarnAt: 50, Cut: true}
	var said []Event
	b := newBook(t, dir, c, lim, &said)
	ctr := b.Counter("shop.example")
	step := func(n int64) {
		ctr.Out(n)
		b.Tick()
		c.add(time.Second)
	}
	step(400)
	if len(said) != 0 || ctr.Cut() {
		t.Fatalf("under the warning share: %+v", said)
	}
	step(200)
	step(10)
	if len(said) != 1 || said[0].Kind != "warn" || said[0].Period != "day" || said[0].Used != 600 || ctr.Cut() {
		t.Fatalf("at the warning share: %+v", said)
	}
	step(500)
	step(1)
	if len(said) != 2 || said[1].Kind != "limit" || !said[1].Cut || !ctr.Cut() {
		t.Fatalf("at the budget: %+v, cut %v", said, ctr.Cut())
	}
	if st := b.Report("shop.example", c.now(), c.now()); st.State != "cut" || st.Limits == nil || st.Limits.Daily != 1000 {
		t.Fatalf("report %q %+v", st.State, st.Limits)
	}

	// A new process finds the site cut at its first look, and says
	// nothing it already said.
	b.Flush()
	said = nil
	again := newBook(t, dir, c, lim, &said)
	if !again.Cut("shop.example") || len(said) != 0 {
		t.Fatalf("after a restart: cut %v, said %+v", again.Cut("shop.example"), said)
	}
	// Raised, the site serves within the seconds the budgets are read.
	lim.Daily = 5000
	for i := 0; i < rateOver; i++ {
		again.Tick()
		c.add(time.Second)
	}
	if again.Cut("shop.example") || len(said) != 1 || said[0].Kind != "resumed" {
		t.Fatalf("after raising it: cut %v, said %+v", again.Cut("shop.example"), said)
	}
	// Without cut the same budget is said and the site goes on; the
	// next day it starts again.
	lim.Daily, lim.Cut = 1000, false
	said = nil
	for i := 0; i < rateOver; i++ {
		again.Tick()
		c.add(time.Second)
	}
	if again.Cut("shop.example") || len(said) != 1 || said[0].Kind != "limit" || said[0].Cut {
		t.Fatalf("a budget that only warns: cut %v, said %+v", again.Cut("shop.example"), said)
	}
	if st := again.Report("shop.example", c.now(), c.now()).State; st != "over" {
		t.Fatalf("state %q", st)
	}
	// Cut turned on over a budget already reached and said closes the
	// site and says that it did.
	lim.Cut = true
	said = nil
	for i := 0; i < rateOver; i++ {
		again.Tick()
		c.add(time.Second)
	}
	if !again.Cut("shop.example") || len(said) != 1 || said[0].Kind != "limit" || !said[0].Cut {
		t.Fatalf("cut turned on later: cut %v, said %+v", again.Cut("shop.example"), said)
	}
	c.add(24 * time.Hour)
	again.Tick()
	if st := again.Report("shop.example", c.now(), c.now()).State; st != "ok" {
		t.Fatalf("the day after: %q", st)
	}
}

// Released, a site's bytes are on disk and out of memory, where a rename
// or a delete finds them.
func TestReleaseWritesAndForgets(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: time.Date(2026, 11, 12, 10, 0, 0, 0, time.Local)}
	b := newBook(t, dir, c, nil, nil)
	b.Counter("a.example").In(7)
	b.Release("a.example")
	if _, err := os.Stat(filepath.Join(dir, "a.example", "2026-11.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "a.example"), filepath.Join(dir, "b.example")); err != nil {
		t.Fatal(err)
	}
	if got := b.Report("b.example", c.now(), c.now()).Today.In; got != 7 {
		t.Fatalf("under its new name: %d", got)
	}
	if got := b.Report("a.example", c.now(), c.now()).Today.In; got != 0 {
		t.Fatalf("the old name kept %d", got)
	}
}
