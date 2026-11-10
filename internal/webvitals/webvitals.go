// Package webvitals measures how a site's pages feel to the people who
// open them: Largest Contentful Paint, Interaction to Next Paint,
// Cumulative Layout Shift, First Contentful Paint and Time to First Byte,
// taken in their own browsers (field data, not a lab run), for the sites
// whose manifest turns it on. It is off unless the operator turns it on:
// it adds a script to every page the site serves.
//
// The house adds one same-origin script to the page's <head>; the script
// sends what it measured once, when the page is hidden, to a path the
// house answers itself. What is kept is a histogram per metric, per day
// and device, and per page for the most measured pages: enough for the
// 75th percentile Google ranks by, and nothing that tells one person from
// another (no address, no agent string).
package webvitals

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/fsx"
)

// The paths the house answers on a site that has Web Vitals on. Under
// /.well-known/ so no application route is taken from the site.
const (
	ScriptPath = "/.well-known/middleware/vitals.js"
	BeaconPath = "/.well-known/middleware/vitals"
)

// Metric is one of the measures, with the thresholds web.dev publishes:
// at or under Good is good, over Poor is poor, between needs improvement.
// CLS is kept in thousandths so every metric is an integer.
type Metric struct {
	Key   string
	Good  int
	Poor  int
	Edges []int
	Max   int
}

var msEdges = []int{50, 100, 150, 200, 300, 400, 500, 600, 800, 1000, 1200, 1500, 1800, 2000, 2500, 3000, 3500, 4000, 5000, 6000, 8000, 10000, 15000, 20000, 30000}

// Metrics are in the order the panel shows them.
var Metrics = []Metric{
	{Key: "lcp", Good: 2500, Poor: 4000, Edges: msEdges, Max: 120000},
	{Key: "inp", Good: 200, Poor: 500, Edges: msEdges, Max: 60000},
	{Key: "cls", Good: 100, Poor: 250, Edges: []int{10, 25, 50, 75, 100, 150, 200, 250, 300, 400, 500, 750, 1000, 2000}, Max: 10000},
	{Key: "fcp", Good: 1800, Poor: 3000, Edges: msEdges, Max: 120000},
	{Key: "ttfb", Good: 800, Poor: 1800, Edges: msEdges, Max: 120000},
}

func metric(key string) (Metric, bool) {
	for _, m := range Metrics {
		if m.Key == key {
			return m, true
		}
	}
	return Metric{}, false
}

// Hist is what is kept of one metric: how many, how many good and poor,
// and how many fell in each bucket (the last one is everything over the
// last edge).
type Hist struct {
	N       int64   `json:"n"`
	Good    int64   `json:"good"`
	Poor    int64   `json:"poor"`
	Buckets []int64 `json:"buckets"`
}

func (h *Hist) add(m Metric, v int) {
	if len(h.Buckets) != len(m.Edges)+1 {
		h.Buckets = make([]int64, len(m.Edges)+1)
	}
	h.N++
	switch {
	case v <= m.Good:
		h.Good++
	case v > m.Poor:
		h.Poor++
	}
	i := sort.SearchInts(m.Edges, v)
	h.Buckets[i]++
}

func (h *Hist) merge(o *Hist) {
	if o == nil || o.N == 0 {
		return
	}
	if len(h.Buckets) < len(o.Buckets) {
		h.Buckets = append(h.Buckets, make([]int64, len(o.Buckets)-len(h.Buckets))...)
	}
	h.N += o.N
	h.Good += o.Good
	h.Poor += o.Poor
	for i, c := range o.Buckets {
		h.Buckets[i] += c
	}
}

// P75 is the 75th percentile, interpolated inside its bucket: the buckets
// are what is kept, so the answer is as fine as they are, which is finer
// than the thresholds it is compared with.
func (h *Hist) P75(m Metric) int {
	if h == nil || h.N == 0 {
		return 0
	}
	target := float64(h.N) * 0.75
	var seen float64
	for i, c := range h.Buckets {
		if c == 0 {
			continue
		}
		if seen+float64(c) >= target {
			lo := 0
			if i > 0 {
				lo = m.Edges[i-1]
			}
			hi := m.Max
			if i < len(m.Edges) {
				hi = m.Edges[i]
			}
			frac := (target - seen) / float64(c)
			return lo + int(math.Round(frac*float64(hi-lo)))
		}
		seen += float64(c)
	}
	return m.Max
}

// Rating is good, needs-improvement or poor for a value.
func Rating(m Metric, v int) string {
	switch {
	case v <= m.Good:
		return "good"
	case v > m.Poor:
		return "poor"
	}
	return "needs-improvement"
}

// Sample is what one page sent. A metric the browser did not measure (no
// interaction, a browser without the API) is absent.
type Sample struct {
	Path   string
	Mobile bool
	Values map[string]int
}

// maxPaths is how many pages a month keeps apart; the rest only count in
// the site's totals. A site with ten thousand product pages would
// otherwise write ten thousand histograms a month.
const maxPaths = 50

type dayFile map[string]map[string]*Hist // device → metric → hist

type monthFile struct {
	Days  map[string]dayFile          `json:"days"`
	Paths map[string]map[string]*Hist `json:"paths"`
	dirty bool
}

// Book keeps every site's measures, in data/webvitals/<domain>/<YYYY-MM>.json.
type Book struct {
	dir string
	now func() time.Time

	mu     sync.Mutex
	months map[string]*monthFile // domain + "/" + month
	// rate is beacons taken this minute, per site: a page sends one, so
	// more than perMinute a minute is someone sending them by hand.
	rate   map[string]int
	minute int64
}

// perMinute caps what a site takes: far over what its visitors send,
// and a bound on what a flood of forged beacons can cost.
const perMinute = 3000

// Allow counts a beacon for domain and says whether to take it.
func (b *Book) Allow(domain string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if m := b.now().Unix() / 60; m != b.minute {
		b.minute, b.rate = m, map[string]int{}
	}
	b.rate[domain]++
	return b.rate[domain] <= perMinute
}

// New opens the book under dir.
func New(dir string) *Book {
	return &Book{dir: dir, now: time.Now, months: map[string]*monthFile{}, rate: map[string]int{}}
}

func (b *Book) path(domain, month string) string {
	return filepath.Join(b.dir, domain, month+".json")
}

func (b *Book) month(domain, month string) *monthFile {
	key := domain + "/" + month
	if m, ok := b.months[key]; ok {
		return m
	}
	m := &monthFile{}
	if raw, err := os.ReadFile(b.path(domain, month)); err == nil {
		_ = json.Unmarshal(raw, m)
	}
	if m.Days == nil {
		m.Days = map[string]dayFile{}
	}
	if m.Paths == nil {
		m.Paths = map[string]map[string]*Hist{}
	}
	b.months[key] = m
	return m
}

// ErrEmpty is a sample with nothing that could be kept in it.
var ErrEmpty = errors.New("webvitals: nothing measured")

// Note keeps a sample. Values out of range are dropped, not clamped: a
// browser that reports an LCP of two minutes measured a page left in a
// background tab, and keeping it would only move the percentile.
func (b *Book) Note(domain string, s Sample) error {
	kept := map[string]int{}
	for k, v := range s.Values {
		m, ok := metric(k)
		if !ok || v < 0 || v > m.Max {
			continue
		}
		kept[k] = v
	}
	if len(kept) == 0 {
		return ErrEmpty
	}
	now := b.now()
	device := "desktop"
	if s.Mobile {
		device = "mobile"
	}
	path := cleanPath(s.Path)
	b.mu.Lock()
	defer b.mu.Unlock()
	mf := b.month(domain, now.Format("2006-01"))
	day := now.Format("02")
	d := mf.Days[day]
	if d == nil {
		d = dayFile{}
		mf.Days[day] = d
	}
	dev := d[device]
	if dev == nil {
		dev = map[string]*Hist{}
		d[device] = dev
	}
	p := mf.Paths[path]
	if p == nil && len(mf.Paths) < maxPaths {
		p = map[string]*Hist{}
		mf.Paths[path] = p
	}
	for k, v := range kept {
		m, _ := metric(k)
		h := dev[k]
		if h == nil {
			h = &Hist{}
			dev[k] = h
		}
		h.add(m, v)
		if p != nil {
			ph := p[k]
			if ph == nil {
				ph = &Hist{}
				p[k] = ph
			}
			ph.add(m, v)
		}
	}
	mf.dirty = true
	return nil
}

// cleanPath keeps the path without its query (where personal data
// travels) and cut to a length a table can show.
func cleanPath(p string) string {
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if len(p) > 200 {
		p = p[:200]
	}
	return p
}

// Release writes what changed and lets go of domain: its folder is about
// to be moved or removed.
func (b *Book) Release(domain string) {
	b.Flush()
	b.mu.Lock()
	defer b.mu.Unlock()
	for key := range b.months {
		if strings.HasPrefix(key, domain+"/") {
			delete(b.months, key)
		}
	}
}

// Flush writes every month that changed, and forgets the ones of months
// gone by: only the current one is still written to.
func (b *Book) Flush() {
	b.mu.Lock()
	defer b.mu.Unlock()
	current := b.now().Format("2006-01")
	for key, mf := range b.months {
		domain, month, _ := strings.Cut(key, "/")
		if mf.dirty {
			raw, err := json.Marshal(mf)
			if err == nil {
				_ = os.MkdirAll(filepath.Join(b.dir, domain), 0o755)
				if fsx.WriteAtomic(b.path(domain, month), raw, 0o644) == nil {
					mf.dirty = false
				}
			}
		}
		if month != current && !mf.dirty {
			delete(b.months, key)
		}
	}
}

// Summary is one metric over a stretch.
type Summary struct {
	Metric string  `json:"metric"`
	N      int64   `json:"n"`
	P75    int     `json:"p75"`
	Rating string  `json:"rating"`
	Good   float64 `json:"good"`
	Poor   float64 `json:"poor"`
}

func summarize(m Metric, h *Hist) Summary {
	s := Summary{Metric: m.Key}
	if h == nil || h.N == 0 {
		return s
	}
	s.N, s.P75 = h.N, h.P75(m)
	s.Rating = Rating(m, s.P75)
	s.Good = float64(h.Good) / float64(h.N)
	s.Poor = float64(h.Poor) / float64(h.N)
	return s
}

// DayRow is one day's 75th percentiles.
type DayRow struct {
	Date string         `json:"date"`
	N    int64          `json:"n"`
	P75  map[string]int `json:"p75"`
}

// PathRow is one page's.
type PathRow struct {
	Path    string    `json:"path"`
	Metrics []Summary `json:"metrics"`
}

// Report is a site's measures over a stretch of days.
type Report struct {
	From    string               `json:"from"`
	To      string               `json:"to"`
	Metrics []Summary            `json:"metrics"`
	Devices map[string][]Summary `json:"devices"`
	Days    []DayRow             `json:"days"`
	// Paths are the pages measured most, worst LCP first: the page to fix
	// is the slow one many people open.
	Paths  []PathRow `json:"paths"`
	Months []string  `json:"months"`
}

// Report reads from..to (days, inclusive). Per-page rows are for the
// months the stretch touches, as a page's histograms are kept per month.
func (b *Book) Report(domain string, from, to time.Time) Report {
	b.Flush()
	rep := Report{From: from.Format("2006-01-02"), To: to.Format("2006-01-02"), Devices: map[string][]Summary{}}
	total := map[string]*Hist{}
	byDevice := map[string]map[string]*Hist{}
	paths := map[string]map[string]*Hist{}
	b.mu.Lock()
	for d := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, from.Location()); !d.After(to); d = d.AddDate(0, 0, 1) {
		mf := b.month(domain, d.Format("2006-01"))
		day := mf.Days[d.Format("02")]
		row := DayRow{Date: d.Format("2006-01-02"), P75: map[string]int{}}
		dayTotal := map[string]*Hist{}
		for device, hs := range day {
			if byDevice[device] == nil {
				byDevice[device] = map[string]*Hist{}
			}
			for k, h := range hs {
				for _, into := range []map[string]*Hist{total, byDevice[device], dayTotal} {
					if into[k] == nil {
						into[k] = &Hist{}
					}
					into[k].merge(h)
				}
			}
		}
		for _, m := range Metrics {
			if h := dayTotal[m.Key]; h != nil && h.N > 0 {
				row.P75[m.Key] = h.P75(m)
				if h.N > row.N {
					row.N = h.N
				}
			}
		}
		rep.Days = append(rep.Days, row)
	}
	for d := time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, from.Location()); !d.After(to); d = d.AddDate(0, 1, 0) {
		for p, hs := range b.month(domain, d.Format("2006-01")).Paths {
			if paths[p] == nil {
				paths[p] = map[string]*Hist{}
			}
			for k, h := range hs {
				if paths[p][k] == nil {
					paths[p][k] = &Hist{}
				}
				paths[p][k].merge(h)
			}
		}
	}
	b.mu.Unlock()
	rep.Months = b.monthsOf(domain)
	for _, m := range Metrics {
		rep.Metrics = append(rep.Metrics, summarize(m, total[m.Key]))
	}
	for device, hs := range byDevice {
		for _, m := range Metrics {
			rep.Devices[device] = append(rep.Devices[device], summarize(m, hs[m.Key]))
		}
	}
	for p, hs := range paths {
		row := PathRow{Path: p}
		for _, m := range Metrics {
			row.Metrics = append(row.Metrics, summarize(m, hs[m.Key]))
		}
		rep.Paths = append(rep.Paths, row)
	}
	// Measured by enough people to mean something first, then the
	// slowest: one visit on a phone in a tunnel is not a slow page.
	sort.Slice(rep.Paths, func(i, j int) bool {
		a, c := rep.Paths[i].Metrics[0], rep.Paths[j].Metrics[0]
		ea, ec := a.N >= 5, c.N >= 5
		if ea != ec {
			return ea
		}
		if a.P75 != c.P75 {
			return a.P75 > c.P75
		}
		return rep.Paths[i].Path < rep.Paths[j].Path
	})
	if len(rep.Paths) > 20 {
		rep.Paths = rep.Paths[:20]
	}
	return rep
}

// monthsOf lists the months the site has measures for, newest first.
func (b *Book) monthsOf(domain string) []string {
	entries, _ := os.ReadDir(filepath.Join(b.dir, domain))
	var out []string
	for _, e := range entries {
		if name, ok := strings.CutSuffix(e.Name(), ".json"); ok && len(name) == 7 {
			out = append(out, name)
		}
	}
	b.mu.Lock()
	for key, mf := range b.months {
		d, month, _ := strings.Cut(key, "/")
		if d == domain && mf.dirty {
			out = append(out, month)
		}
	}
	b.mu.Unlock()
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return dedupe(out)
}

func dedupe(s []string) []string {
	var out []string
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}
