// Package status keeps each site's availability, minute by minute, and
// draws the public status page a site can turn on (manifest
// `status_page`): what state it is in now, how much of each of the last
// ninety days it was up, and the outages, with when they began and how
// long they lasted.
//
// It is the house's view, not a probe from outside: the house serves the
// site, so when it says a site is down every visitor is getting the
// error page. A whole machine down is the one thing a page served by that
// machine cannot report, and the page says so in its footer instead of
// pretending to be an external monitor.
package status

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/fsx"
)

// State is what a site is in during a minute.
type State string

// The states. Maintenance is a hold or a stop the operator chose: it is
// shown, and it is not counted against the uptime, which is the
// convention of every status page people know.
const (
	Up          State = "up"
	Down        State = "down"
	Maintenance State = "maintenance"
	// Unknown is a site starting or not checked yet: not counted at all.
	Unknown State = ""
)

// Day is a day's minutes in each state.
type Day struct {
	Up          int `json:"up"`
	Down        int `json:"down"`
	Maintenance int `json:"maintenance,omitempty"`
}

// Uptime is the share of the counted minutes the site was up, -1 when
// no minute was counted.
func (d Day) Uptime() float64 {
	n := d.Up + d.Down
	if n == 0 {
		return -1
	}
	return float64(d.Up) / float64(n)
}

// Incident is an outage: from the first minute down to the first one up
// again (zero while it lasts).
type Incident struct {
	From   time.Time `json:"from"`
	To     time.Time `json:"to,omitzero"`
	Reason string    `json:"reason,omitempty"`
}

// keepDays and keepIncidents bound a site's file: ninety bars on the page
// and the outages a visitor scrolls through.
const (
	keepDays      = 90
	keepIncidents = 50
	// openAfter is how many minutes in a row down make an outage: a
	// single bad minute is a handover or a restart a visitor did not see.
	openAfter = 2
)

type record struct {
	Days      map[string]*Day `json:"days"`
	Incidents []Incident      `json:"incidents"`
	State     State           `json:"state"`
	Since     time.Time       `json:"since"`
	Reason    string          `json:"reason,omitempty"`
	downRun   int
	downFrom  time.Time
	dirty     bool
}

// Book keeps every site's availability in data/status/<domain>.json.
type Book struct {
	dir string
	now func() time.Time

	mu    sync.Mutex
	sites map[string]*record
}

// New opens the book under dir.
func New(dir string) *Book {
	return &Book{dir: dir, now: time.Now, sites: map[string]*record{}}
}

func (b *Book) path(domain string) string { return filepath.Join(b.dir, domain+".json") }

func (b *Book) site(domain string) *record {
	if r, ok := b.sites[domain]; ok {
		return r
	}
	r := &record{}
	if raw, err := os.ReadFile(b.path(domain)); err == nil {
		_ = json.Unmarshal(raw, r)
	}
	if r.Days == nil {
		r.Days = map[string]*Day{}
	}
	// An outage open when the house stopped is still open: the minutes
	// the house was not looking are not counted either way, and the next
	// minute up closes it.
	if n := len(r.Incidents); n > 0 && r.Incidents[n-1].To.IsZero() {
		r.downRun, r.downFrom = openAfter, r.Incidents[n-1].From
	}
	b.sites[domain] = r
	return r
}

// Observe counts one minute of domain in state s. It is meant to be
// called once a minute per site; reason is what the house knows of why a
// site is down (its last error), shown on the page.
func (b *Book) Observe(domain string, s State, reason string) {
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.site(domain)
	day := now.Format("2006-01-02")
	d := r.Days[day]
	if d == nil {
		d = &Day{}
		r.Days[day] = d
		r.trim(now)
	}
	switch s {
	case Up:
		d.Up++
	case Down:
		d.Down++
	case Maintenance:
		d.Maintenance++
	}
	if s != Unknown && s != r.State {
		r.State, r.Since = s, now
	}
	if s == Down {
		r.Reason = short(reason)
		if r.downRun == 0 {
			r.downFrom = now
		}
		r.downRun++
		if r.downRun == openAfter {
			r.Incidents = append(r.Incidents, Incident{From: r.downFrom, Reason: r.Reason})
			if len(r.Incidents) > keepIncidents {
				r.Incidents = r.Incidents[len(r.Incidents)-keepIncidents:]
			}
		}
	} else if s != Unknown {
		if r.downRun >= openAfter {
			if n := len(r.Incidents); n > 0 && r.Incidents[n-1].To.IsZero() {
				r.Incidents[n-1].To = now
			}
		}
		r.downRun = 0
		r.Reason = ""
	}
	r.dirty = true
}

func (r *record) trim(now time.Time) {
	cut := now.AddDate(0, 0, -keepDays).Format("2006-01-02")
	for k := range r.Days {
		if k < cut {
			delete(r.Days, k)
		}
	}
}

// short keeps a reason to one line a visitor can read; the details are
// for the operator's panel.
func short(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

// Flush writes what changed.
func (b *Book) Flush() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for domain, r := range b.sites {
		if !r.dirty {
			continue
		}
		raw, err := json.Marshal(r)
		if err == nil && fsx.WriteAtomic(b.path(domain), raw, 0o644) == nil {
			r.dirty = false
		}
	}
}

// Release writes what changed and lets go of domain: its file is about
// to be moved or removed.
func (b *Book) Release(domain string) {
	b.Flush()
	b.mu.Lock()
	delete(b.sites, domain)
	b.mu.Unlock()
}

// Forget drops a site that is gone.
func (b *Book) Forget(domain string) {
	b.mu.Lock()
	delete(b.sites, domain)
	b.mu.Unlock()
	_ = os.Remove(b.path(domain))
}

// DayRow is one bar of the page.
type DayRow struct {
	Date string `json:"date"`
	Day
	// Uptime is -1 for a day with nothing counted (before the site was
	// watched, or all of it in maintenance).
	Uptime float64 `json:"uptime"`
}

// Report is what the page and the API show.
type Report struct {
	State  State     `json:"state"`
	Since  time.Time `json:"since,omitzero"`
	Reason string    `json:"reason,omitempty"`
	// Uptime over the last 1, 7, 30 and 90 days, -1 when nothing was
	// counted.
	Uptime    map[string]float64 `json:"uptime"`
	Days      []DayRow           `json:"days"`
	Incidents []Incident         `json:"incidents"`
}

// Report reads the last ninety days of domain, oldest first, and its
// outages, newest first.
func (b *Book) Report(domain string) Report {
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.site(domain)
	rep := Report{State: r.State, Since: r.Since, Reason: r.Reason, Uptime: map[string]float64{}}
	for i := keepDays - 1; i >= 0; i-- {
		date := now.AddDate(0, 0, -i).Format("2006-01-02")
		row := DayRow{Date: date, Uptime: -1}
		if d := r.Days[date]; d != nil {
			row.Day, row.Uptime = *d, d.Uptime()
		}
		rep.Days = append(rep.Days, row)
	}
	for _, n := range []int{1, 7, 30, 90} {
		var sum Day
		for _, row := range rep.Days[len(rep.Days)-n:] {
			sum.Up += row.Up
			sum.Down += row.Down
		}
		rep.Uptime[strconv.Itoa(n)] = sum.Uptime()
	}
	rep.Incidents = append([]Incident(nil), r.Incidents...)
	sort.Slice(rep.Incidents, func(i, j int) bool { return rep.Incidents[i].From.After(rep.Incidents[j].From) })
	return rep
}
