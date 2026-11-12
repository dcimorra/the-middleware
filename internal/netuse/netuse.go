// Package netuse is how much network each site moves: bytes in and bytes
// out, as a rate now and as totals by hour, day and month, with a budget
// the owner may set and what happens when it is reached.
//
// It answers "what does this site cost in transfer", which nothing else
// in the house could: package traffic counts requests over the last
// minute and forgets, and the access log would have to be read back whole.
//
// The request path pays two atomic additions per read and per write and
// nothing else. A site's Counter is handed to whoever moves its bytes
// (the request chain, a hijacked WebSocket); once a second Tick folds
// what the counters gained into the site's hour of the day and into the
// last minute's ring, and judges the budget. The disk is written once a
// minute. Bytes that never cross the house's proxy (a service's public
// ports) are added by whoever reads them from the firewall (AddPorts).
//
// What is counted is what the house reads and writes for the site: the
// request and the response as HTTP, without the TLS and TCP/IP framing
// around them, so a provider's bill reads a few percent higher.
package netuse

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dcimorra/the-middleware/internal/fsx"
)

// Dir is the book's folder under data/: <domain>/<YYYY-MM>.json, so a
// delete and a rename take it along (sites.named).
const Dir = "netuse"

// window is how far back the per-second ring looks.
const window = 60

// rateOver is the seconds a rate is averaged over: long enough that a
// burst does not read as the site's speed, short enough to be "now".
const rateOver = 5

// DefaultWarnAt is the share of a budget at which the house warns when
// the manifest does not say.
const DefaultWarnAt = 80

// Limits are a site's budget, in bytes in plus out. Zero is no budget.
type Limits struct {
	Monthly int64
	Daily   int64
	// WarnAt is the percent of a budget at which a warning is said; zero
	// takes DefaultWarnAt.
	WarnAt int
	// Cut closes the site when a budget is reached, until its day or its
	// month ends or the budget is raised. Without it the house only says.
	Cut bool
}

func (l Limits) warnAt() int {
	if l.WarnAt <= 0 || l.WarnAt >= 100 {
		return DefaultWarnAt
	}
	return l.WarnAt
}

// Counter is a site's bytes since the process started. The request path
// holds one for the life of a request and adds to it as bytes move, so a
// download of an hour shows in the rate while it runs, not when it ends.
type Counter struct {
	in, out atomic.Int64
	cut     atomic.Bool
}

// In adds bytes received from the visitor.
func (c *Counter) In(n int64) {
	if n > 0 {
		c.in.Add(n)
	}
}

// Out adds bytes sent to the visitor.
func (c *Counter) Out(n int64) {
	if n > 0 {
		c.out.Add(n)
	}
}

// Cut says whether the site is closed for having reached its budget.
func (c *Counter) Cut() bool { return c.cut.Load() }

// Event is something the book has to say about a site's budget.
type Event struct {
	Domain string
	// Kind is "warn" (the warning share was reached), "limit" (the
	// budget was) or "resumed" (a site that was cut serves again).
	Kind string
	// Period is "month" or "day".
	Period string
	Used   int64
	Limit  int64
	// Cut is whether reaching the budget closed the site.
	Cut bool
}

// Options configure a Book.
type Options struct {
	// Dir is data/netuse.
	Dir string
	// Limits resolves a site's budget now (its manifest over the house's
	// defaults). It is called outside the book's lock. Nil is no budgets.
	Limits func(domain string) Limits
	// Notify is told what there is to say, outside the book's lock.
	Notify func(Event)
	Now    func() time.Time
}

// Book is every site's use.
type Book struct {
	opts Options
	now  func() time.Time

	mu    sync.RWMutex
	sites map[string]*site
	ticks int
}

// New builds a book over dir.
func New(opts Options) *Book {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Book{opts: opts, now: opts.Now, sites: map[string]*site{}}
}

// day is one day of a site: every byte by the hour it moved in, and the
// part of the day's bytes that came by the site's public ports.
type day struct {
	In       []int64 `json:"in"`
	Out      []int64 `json:"out"`
	PortsIn  int64   `json:"ports_in,omitempty"`
	PortsOut int64   `json:"ports_out,omitempty"`
}

func (d *day) totals() Totals {
	t := Totals{PortsIn: d.PortsIn, PortsOut: d.PortsOut}
	for _, n := range d.In {
		t.In += n
	}
	for _, n := range d.Out {
		t.Out += n
	}
	return t
}

// month is the file.
type month struct {
	Month string          `json:"month"`
	Days  map[string]*day `json:"days"`
	// Said is how far each budget's warnings went, so a restart does not
	// say them again: "month", or a day's date, to 1 (warned) or 2 (the
	// budget was reached).
	Said map[string]int `json:"said,omitempty"`
}

type sample struct {
	sec     int64
	in, out float64
}

type site struct {
	Counter
	domain string
	// Everything below is the book's, under its lock.
	seenIn, seenOut int64
	cur             *month
	dirty           bool
	limits          Limits
	// judged is false until the first look at the budget, which finds
	// what a process before this one already said.
	judged bool
	ring   [window]sample
}

type write struct {
	path string
	data []byte
}

func (b *Book) path(domain, month string) string {
	return filepath.Join(b.opts.Dir, domain, month+".json")
}

func readMonth(path, key string) *month {
	m := &month{}
	if err := fsx.ReadJSON(path, m); err != nil || m.Days == nil {
		m = &month{Days: map[string]*day{}}
	}
	m.Month = key
	for _, d := range m.Days {
		// A file cut short or written by hand: the hours are always 24.
		d.In, d.Out = hours(d.In), hours(d.Out)
	}
	return m
}

func hours(h []int64) []int64 {
	if len(h) == 24 {
		return h
	}
	out := make([]int64, 24)
	copy(out, h)
	return out
}

// site is domain's entry, read from disk the first time. Its budget is
// judged before it is handed out: the first request after a restart of a
// site that was cut must find it cut.
func (b *Book) site(domain string) *site {
	b.mu.RLock()
	s := b.sites[domain]
	b.mu.RUnlock()
	if s != nil {
		return s
	}
	var lim Limits
	if b.opts.Limits != nil {
		lim = b.opts.Limits(domain)
	}
	now := b.now()
	key := now.Format("2006-01")
	cur := readMonth(b.path(domain, key), key)
	b.mu.Lock()
	if s = b.sites[domain]; s != nil {
		b.mu.Unlock()
		return s
	}
	s = &site{domain: domain, cur: cur, limits: lim}
	b.sites[domain] = s
	evs := s.judge(now)
	b.mu.Unlock()
	b.notify(evs)
	return s
}

// Counter is domain's, for whoever moves its bytes.
func (b *Book) Counter(domain string) *Counter { return &b.site(domain).Counter }

// Cut says whether domain is closed for having reached its budget.
func (b *Book) Cut(domain string) bool { return b.site(domain).Cut() }

func (b *Book) notify(evs []Event) {
	if b.opts.Notify == nil {
		return
	}
	for _, e := range evs {
		b.opts.Notify(e)
	}
}

// today is the site's day at now, turning the month over when it ended:
// the month that ended is returned to be written.
func (s *site) today(b *Book, now time.Time) (*day, *write) {
	var w *write
	if key := now.Format("2006-01"); s.cur.Month != key {
		if s.dirty {
			if data, err := json.Marshal(s.cur); err == nil {
				w = &write{b.path(s.domain, s.cur.Month), data}
			}
		}
		s.cur, s.dirty = &month{Month: key, Days: map[string]*day{}}, false
	}
	date := now.Format("2006-01-02")
	d := s.cur.Days[date]
	if d == nil {
		d = &day{In: make([]int64, 24), Out: make([]int64, 24)}
		s.cur.Days[date] = d
	}
	return d, w
}

func (s *site) slot(sec int64) *sample {
	sl := &s.ring[((sec%window)+window)%window]
	if sl.sec != sec {
		*sl = sample{sec: sec}
	}
	return sl
}

// Tick folds what every site's counter gained since the last one into its
// hour and its ring, and judges the budgets. The caller runs it once a
// second.
func (b *Book) Tick() {
	now := b.now()
	// The budgets are read again every few seconds, outside the lock: a
	// manifest saved now is a budget in force within that time.
	var limits map[string]Limits
	b.mu.Lock()
	b.ticks++
	refresh := b.opts.Limits != nil && b.ticks%rateOver == 0
	var names []string
	if refresh {
		for d := range b.sites {
			names = append(names, d)
		}
	}
	b.mu.Unlock()
	if refresh {
		limits = make(map[string]Limits, len(names))
		for _, d := range names {
			limits[d] = b.opts.Limits(d)
		}
	}

	var evs []Event
	var writes []write
	b.mu.Lock()
	for _, s := range b.sites {
		in, out := s.in.Load(), s.out.Load()
		din, dout := in-s.seenIn, out-s.seenOut
		s.seenIn, s.seenOut = in, out
		if din != 0 || dout != 0 {
			d, w := s.today(b, now)
			if w != nil {
				writes = append(writes, *w)
			}
			h := now.Hour()
			d.In[h] += din
			d.Out[h] += dout
			sl := s.slot(now.Unix())
			sl.in += float64(din)
			sl.out += float64(dout)
			s.dirty = true
		}
		if l, ok := limits[s.domain]; ok {
			s.limits = l
		}
		evs = append(evs, s.judge(now)...)
	}
	b.mu.Unlock()
	save(writes)
	b.notify(evs)
}

// AddPorts adds bytes that moved by domain's public ports over the last
// span, read from the firewall by the caller. They are spread over the
// span to come and not put in this second: a counter read every few
// seconds would otherwise draw a comb, and a rate read between two teeth
// would say the relay stopped.
func (b *Book) AddPorts(domain string, in, out int64, span time.Duration) {
	if in <= 0 && out <= 0 {
		return
	}
	s := b.site(domain)
	now := b.now()
	n := int64(span / time.Second)
	if n < 1 {
		n = 1
	}
	if n > window/2 {
		n = window / 2
	}
	b.mu.Lock()
	d, w := s.today(b, now)
	h := now.Hour()
	d.In[h] += in
	d.Out[h] += out
	d.PortsIn += in
	d.PortsOut += out
	for i := int64(1); i <= n; i++ {
		sl := s.slot(now.Unix() + i)
		sl.in += float64(in) / float64(n)
		sl.out += float64(out) / float64(n)
	}
	s.dirty = true
	b.mu.Unlock()
	if w != nil {
		save([]write{*w})
	}
}

// used is the site's bytes this month and today.
func (s *site) used(now time.Time) (monthly, daily Totals) {
	if s.cur.Month != now.Format("2006-01") {
		return
	}
	date := now.Format("2006-01-02")
	for k, d := range s.cur.Days {
		t := d.totals()
		monthly.add(t)
		if k == date {
			daily = t
		}
	}
	return
}

// level is how far used went into a budget: 0, 1 past the warning share,
// 2 at the budget.
func level(used, limit int64, warnAt int) int {
	switch {
	case limit <= 0:
		return 0
	case used >= limit:
		return 2
	case used >= limit/100*int64(warnAt)+limit%100*int64(warnAt)/100:
		return 1
	}
	return 0
}

// judge compares the site's use with its budget, says what was not said
// yet and sets whether the site is cut. Called with the book's lock held.
func (s *site) judge(now time.Time) []Event {
	monthly, daily := s.used(now)
	date := now.Format("2006-01-02")
	var evs []Event
	reached := false
	var at Event
	for _, p := range []struct {
		key, period string
		used, limit int64
	}{
		{"month", "month", monthly.In + monthly.Out, s.limits.Monthly},
		{date, "day", daily.In + daily.Out, s.limits.Daily},
	} {
		lv := level(p.used, p.limit, s.limits.warnAt())
		if lv == 2 && !reached {
			reached = true
			at = Event{Domain: s.domain, Kind: "limit", Period: p.period, Used: p.used, Limit: p.limit, Cut: true}
		}
		said := 0
		if s.cur.Month == now.Format("2006-01") {
			said = s.cur.Said[p.key]
		}
		if lv == said {
			continue
		}
		if lv > said {
			kind := "warn"
			if lv == 2 {
				kind = "limit"
			}
			evs = append(evs, Event{Domain: s.domain, Kind: kind, Period: p.period, Used: p.used, Limit: p.limit, Cut: lv == 2 && s.limits.Cut})
		}
		// A budget raised over what was used is warned about again when
		// the use catches up with it.
		if s.cur.Month != now.Format("2006-01") {
			continue
		}
		if s.cur.Said == nil {
			s.cur.Said = map[string]int{}
		}
		if lv == 0 {
			delete(s.cur.Said, p.key)
		} else {
			s.cur.Said[p.key] = lv
		}
		s.dirty = true
	}
	// Yesterday's warnings say nothing about today.
	for k := range s.cur.Said {
		if k != "month" && k != date {
			delete(s.cur.Said, k)
			s.dirty = true
		}
	}
	cut := reached && s.limits.Cut
	was := s.cut.Swap(cut)
	switch {
	case was && !cut:
		evs = append(evs, Event{Domain: s.domain, Kind: "resumed"})
	case cut && !was && s.judged && !slicesHasLimit(evs):
		// The budget was reached, and said, while the site only warned;
		// the owner turned the cut on since. A site that closes says so.
		evs = append(evs, at)
	}
	s.judged = true
	return evs
}

func slicesHasLimit(evs []Event) bool {
	for _, e := range evs {
		if e.Kind == "limit" {
			return true
		}
	}
	return false
}

func save(writes []write) {
	for _, w := range writes {
		if err := os.MkdirAll(filepath.Dir(w.path), 0o755); err != nil {
			continue
		}
		_ = fsx.WriteAtomic(w.path, w.data, 0o644)
	}
}

// Flush writes every site that changed. Called every minute and when the
// process stops, never per request.
func (b *Book) Flush() {
	var writes []write
	b.mu.Lock()
	for _, s := range b.sites {
		if !s.dirty {
			continue
		}
		if data, err := json.Marshal(s.cur); err == nil {
			writes = append(writes, write{b.path(s.domain, s.cur.Month), data})
			s.dirty = false
		}
	}
	b.mu.Unlock()
	save(writes)
}

// Release writes and forgets domain, which is about to be renamed or
// removed: the next request reads it again from disk under whatever name
// it has then.
func (b *Book) Release(domain string) {
	b.Tick()
	b.Flush()
	b.mu.Lock()
	delete(b.sites, domain)
	b.mu.Unlock()
}

// Totals are bytes over some stretch. In and Out are everything; PortsIn
// and PortsOut the part of them that came by the site's public ports
// and not through the house's proxy.
type Totals struct {
	In       int64 `json:"in"`
	Out      int64 `json:"out"`
	PortsIn  int64 `json:"ports_in"`
	PortsOut int64 `json:"ports_out"`
}

func (t *Totals) add(o Totals) {
	t.In += o.In
	t.Out += o.Out
	t.PortsIn += o.PortsIn
	t.PortsOut += o.PortsOut
}

// DayRow is one day of a report.
type DayRow struct {
	Date string `json:"date"`
	In   int64  `json:"in"`
	Out  int64  `json:"out"`
}

// Budget is a site's limits and where it stands in them.
type Budget struct {
	Monthly int64 `json:"monthly,omitempty"`
	Daily   int64 `json:"daily,omitempty"`
	WarnAt  int   `json:"warn_at"`
	Cut     bool  `json:"cut"`
}

// Report is what GET /sites/{domain}/network answers.
type Report struct {
	From string `json:"from"`
	To   string `json:"to"`
	// RateIn and RateOut are bytes per second over the last few seconds.
	RateIn  float64 `json:"rate_in"`
	RateOut float64 `json:"rate_out"`
	// SecondsIn and SecondsOut are the last minute, oldest first.
	SecondsIn  []int64 `json:"seconds_in"`
	SecondsOut []int64 `json:"seconds_out"`
	Today      Totals  `json:"today"`
	Month      Totals  `json:"month"`
	// Period sums the days asked for.
	Period Totals   `json:"period"`
	Days   []DayRow `json:"days"`
	// HoursIn and HoursOut are today by the hour.
	HoursIn  []int64 `json:"hours_in"`
	HoursOut []int64 `json:"hours_out"`
	// Limits is the budget in force, nil when the site has none.
	Limits *Budget `json:"limits,omitempty"`
	// State is "ok", "warn" (past the warning share of a budget), "over"
	// (a budget reached, the site still served) or "cut".
	State string `json:"state"`
}

// rate reads the ring: the last minute and the average of the last
// seconds before this one, which is still filling.
func (s *site) rate(now time.Time) (in, out float64, secIn, secOut []int64) {
	secIn, secOut = make([]int64, window), make([]int64, window)
	last := now.Unix() - 1
	oldest := last - window + 1
	for i := range s.ring {
		sl := &s.ring[i]
		if sl.sec < oldest || sl.sec > last {
			continue
		}
		secIn[sl.sec-oldest] = int64(sl.in)
		secOut[sl.sec-oldest] = int64(sl.out)
		if sl.sec > last-rateOver {
			in += sl.in
			out += sl.out
		}
	}
	return in / rateOver, out / rateOver, secIn, secOut
}

func (s *site) state(now time.Time) string {
	monthly, daily := s.used(now)
	lv := max(level(monthly.In+monthly.Out, s.limits.Monthly, s.limits.warnAt()), level(daily.In+daily.Out, s.limits.Daily, s.limits.warnAt()))
	switch {
	case lv == 2 && s.limits.Cut:
		return "cut"
	case lv == 2:
		return "over"
	case lv == 1:
		return "warn"
	}
	return "ok"
}

// Report is domain's use from the day of from to the day of to, with the
// rate now and the budget.
func (b *Book) Report(domain string, from, to time.Time) Report {
	s := b.site(domain)
	now := b.now()
	out := Report{From: from.Format("2006-01-02"), To: to.Format("2006-01-02"), HoursIn: make([]int64, 24), HoursOut: make([]int64, 24)}
	// The months before this one are read from disk, outside the lock.
	months := map[string]*month{}
	curKey := now.Format("2006-01")
	for t := time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, from.Location()); !t.After(to); t = t.AddDate(0, 1, 0) {
		if key := t.Format("2006-01"); key != curKey {
			months[key] = readMonth(b.path(domain, key), key)
		}
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	out.RateIn, out.RateOut, out.SecondsIn, out.SecondsOut = s.rate(now)
	out.Month, out.Today = s.used(now)
	if d := s.cur.Days[now.Format("2006-01-02")]; d != nil && s.cur.Month == curKey {
		copy(out.HoursIn, d.In)
		copy(out.HoursOut, d.Out)
	}
	if s.cur.Month == curKey {
		months[curKey] = s.cur
	}
	for t := from; !t.After(to); t = t.AddDate(0, 0, 1) {
		date := t.Format("2006-01-02")
		row := DayRow{Date: date}
		if m := months[t.Format("2006-01")]; m != nil {
			if d := m.Days[date]; d != nil {
				tot := d.totals()
				row.In, row.Out = tot.In, tot.Out
				out.Period.add(tot)
			}
		}
		out.Days = append(out.Days, row)
	}
	if s.limits.Monthly > 0 || s.limits.Daily > 0 {
		out.Limits = &Budget{Monthly: s.limits.Monthly, Daily: s.limits.Daily, WarnAt: s.limits.warnAt(), Cut: s.limits.Cut}
	}
	out.State = s.state(now)
	return out
}

// Row is one site in the house's summary.
type Row struct {
	Domain  string  `json:"domain"`
	RateIn  float64 `json:"rate_in"`
	RateOut float64 `json:"rate_out"`
	Today   Totals  `json:"today"`
	Month   Totals  `json:"month"`
	Limits  *Budget `json:"limits,omitempty"`
	State   string  `json:"state"`
}

// Summary is every site that moved anything this month or has a budget,
// the one that moved most this month first. known are the sites there
// are: a folder left by one that is gone is not a site.
func (b *Book) Summary(known []string) []Row {
	for _, d := range known {
		b.site(d)
	}
	now := b.now()
	is := map[string]bool{}
	for _, d := range known {
		is[d] = true
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := []Row{}
	for _, s := range b.sites {
		if !is[s.domain] {
			continue
		}
		r := Row{Domain: s.domain, State: s.state(now)}
		r.RateIn, r.RateOut, _, _ = s.rate(now)
		r.Month, r.Today = s.used(now)
		if s.limits.Monthly > 0 || s.limits.Daily > 0 {
			r.Limits = &Budget{Monthly: s.limits.Monthly, Daily: s.limits.Daily, WarnAt: s.limits.warnAt(), Cut: s.limits.Cut}
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		a, c := out[i].Month.In+out[i].Month.Out, out[j].Month.In+out[j].Month.Out
		if a != c {
			return a > c
		}
		return out[i].Domain < out[j].Domain
	})
	return out
}
