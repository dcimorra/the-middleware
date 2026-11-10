// Package seen is the tally of who actually called at each site: which
// countries, which robots, which browsers, and which User-Agents are in no
// table at all.
//
// It exists because the security tab asked people to type from memory:
// closing a country meant knowing which one was the problem, closing a
// crawler knowing its name, and the only thing that knows is the traffic,
// which passed through and was thrown away. It is not a log: one row per
// distinct thing, with a count and two dates, so a second request from the
// same crawler is one integer going up. That is what makes it cheap enough
// for the path of every request.
//
// Every table is bounded. Countries, robots and browsers are bounded by the
// world; raw User-Agents by hand, because version numbers make them
// infinite and a crawler would otherwise decide how much memory this takes.
package seen

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/security"
)

// agentsKept is how many unrecognised User-Agents a site remembers; when
// full the least seen goes, which is the one to lose: the list is for
// spotting the one calling ten thousand times.
const agentsKept = 200

// cacheKept bounds the per-site cache of classified User-Agents. Past it,
// new strings are classified on every request, which is what the security
// door already pays: the worst case is no worse than without this.
const cacheKept = 2000

// Entry is one distinct thing that called, and how much. Last says whether
// it is still happening; First whether it started the day the bill went up.
type Entry struct {
	Key   string    `json:"key"`
	Name  string    `json:"name,omitempty"`
	Extra string    `json:"extra,omitempty"` // a robot's family
	Count int64     `json:"count"`
	First time.Time `json:"first"`
	Last  time.Time `json:"last"`
}

// Table is a site's tally, heaviest first.
type Table struct {
	Countries []Entry   `json:"countries"`
	Bots      []Entry   `json:"bots"`
	Browsers  []Entry   `json:"browsers"`
	Agents    []Entry   `json:"agents"`
	Since     time.Time `json:"since"`
	Total     int64     `json:"total"`
}

// Book is every site's tally, kept under data/seen/<domain>.json.
type Book struct {
	dir   string
	mu    sync.Mutex
	sites map[string]*registry
	now   func() time.Time
}

// New opens the book. A site's file is read the first time it is noted or
// asked for: a tally that started empty at the last restart would offer
// nothing exactly when someone restarted and went to look why.
func New(dir string) *Book {
	return &Book{dir: dir, sites: map[string]*registry{}, now: time.Now}
}

var validDomain = regexp.MustCompile(`^[a-z0-9.-]+$`)

func (b *Book) site(domain string) *registry {
	if !validDomain.MatchString(domain) || strings.Contains(domain, "..") {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.sites[domain]
	if r == nil {
		r = newRegistry(filepath.Join(b.dir, domain+".json"), b.now)
		b.sites[domain] = r
	}
	return r
}

// Note writes down one request to domain.
func (b *Book) Note(domain, country, ua string) {
	if r := b.site(domain); r != nil {
		r.note(country, ua)
	}
}

// Table is domain's tally.
func (b *Book) Table(domain string) Table {
	if r := b.site(domain); r != nil {
		return r.table()
	}
	return Table{Countries: []Entry{}, Bots: []Entry{}, Browsers: []Entry{}, Agents: []Entry{}}
}

// Forget empties domain's tally: a rule was written, the crawler stopped
// calling, and its name should not stay at the top for weeks.
func (b *Book) Forget(domain string) {
	if r := b.site(domain); r != nil {
		r.forget()
		r.flush()
	}
}

// Release writes domain's tally and lets go of it: its file is about to
// be moved or removed, and the next request for that name reads whatever
// is on disk then.
func (b *Book) Release(domain string) {
	b.mu.Lock()
	r := b.sites[domain]
	delete(b.sites, domain)
	b.mu.Unlock()
	if r != nil {
		r.flush()
	}
}

// Flush writes every tally that changed. Called every minute and when the
// process stops, never per request: losing a minute of counting in a hard
// crash is not worth a disk write per visit.
func (b *Book) Flush() {
	b.mu.Lock()
	list := make([]*registry, 0, len(b.sites))
	for _, r := range b.sites {
		list = append(list, r)
	}
	b.mu.Unlock()
	for _, r := range list {
		r.flush()
	}
}

type registry struct {
	path      string
	now       func() time.Time
	mu        sync.Mutex
	countries map[string]*Entry
	bots      map[string]*Entry
	browsers  map[string]*Entry
	agents    map[string]*Entry
	cache     map[string]class
	since     time.Time
	total     int64
	dirty     bool
}

// class is a User-Agent already understood.
type class struct {
	botName, botFamily string
	browser            string
	// raw: neither a known robot nor a known browser, the only case where
	// the string itself is worth keeping, because no selector offers it.
	raw bool
}

type stored struct {
	Countries []Entry   `json:"countries,omitempty"`
	Bots      []Entry   `json:"bots,omitempty"`
	Browsers  []Entry   `json:"browsers,omitempty"`
	Agents    []Entry   `json:"agents,omitempty"`
	Since     time.Time `json:"since"`
	Total     int64     `json:"total"`
}

func newRegistry(path string, now func() time.Time) *registry {
	r := &registry{path: path, now: now, cache: map[string]class{}, since: now()}
	r.reset()
	var s stored
	// A file that does not read is a tally that starts over, and nothing
	// else: this fills in a form, no site depends on it.
	if raw, err := os.ReadFile(path); err == nil && json.Unmarshal(raw, &s) == nil {
		fill := func(dst map[string]*Entry, rows []Entry) {
			for i := range rows {
				e := rows[i]
				dst[e.Key] = &e
			}
		}
		fill(r.countries, s.Countries)
		fill(r.bots, s.Bots)
		fill(r.browsers, s.Browsers)
		fill(r.agents, s.Agents)
		if !s.Since.IsZero() {
			r.since = s.Since
		}
		r.total = s.Total
	}
	return r
}

func (r *registry) reset() {
	r.countries, r.bots, r.browsers, r.agents = map[string]*Entry{}, map[string]*Entry{}, map[string]*Entry{}, map[string]*Entry{}
}

var countryCode = regexp.MustCompile(`^[A-Z]{2}$`)

func (r *registry) note(country, ua string) {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.total++
	r.dirty = true
	// XX and T1 are Cloudflare's "unknown" and "Tor", not countries.
	if c := strings.ToUpper(strings.TrimSpace(country)); countryCode.MatchString(c) && c != "XX" && c != "T1" {
		bump(r.countries, c, c, "", now)
	}
	ua = strings.TrimSpace(ua)
	if ua == "" {
		// Whoever will not say what it is still gets a row: "shut out
		// whoever sends no User-Agent" is a switch in the same tab, and
		// deciding it blind is what this is here to stop.
		bump(r.browsers, security.BrowserNone, browserName(security.BrowserNone), "", now)
		return
	}
	cl, ok := r.cache[ua]
	if !ok {
		cl = classify(ua)
		if len(r.cache) < cacheKept {
			r.cache[ua] = cl
		}
	}
	// Not exclusive: an unidentified robot goes into both the robots
	// table, under the shared name that says the "other" family is being
	// hit, and the raw one with its own string, the only thing a rule can
	// be written against.
	if cl.botName != "" {
		bump(r.bots, cl.botName, cl.botName, cl.botFamily, now)
	}
	if cl.raw {
		bump(r.agents, short(ua), short(ua), "", now)
		trim(r.agents, agentsKept)
	}
	if cl.browser != "" {
		bump(r.browsers, cl.browser, browserName(cl.browser), "", now)
	}
}

func classify(ua string) class {
	low := strings.ToLower(ua)
	var c class
	if b, ok := security.Bot(low); ok {
		c.botName, c.botFamily = b.Name, b.Family
	}
	c.browser = security.Browser(low)
	if c.browser == security.BrowserOther && (c.botName == "" || c.botFamily == "other") {
		c.raw = true
	}
	return c
}

// browserNames is the catalogue by key, made once: browserName is asked
// for every request, and building the whole catalogue to find one name in
// it was most of what counting a request allocated.
var browserNames = sync.OnceValue(func() map[string]string {
	m := map[string]string{}
	for _, o := range security.Browsers() {
		m[o.Key] = o.Name
	}
	return m
})

func browserName(key string) string {
	if n, ok := browserNames()[key]; ok {
		return n
	}
	return key
}

// short keeps a User-Agent to one line in a list.
func short(ua string) string {
	if len(ua) > 160 {
		return ua[:160] + "…"
	}
	return ua
}

func bump(m map[string]*Entry, key, name, extra string, now time.Time) {
	e, ok := m[key]
	if !ok {
		e = &Entry{Key: key, Name: name, Extra: extra, First: now}
		m[key] = e
	}
	e.Count++
	e.Last = now
}

// trim drops the least seen when a table is over its bound.
func trim(m map[string]*Entry, keep int) {
	if len(m) <= keep {
		return
	}
	rows := make([]*Entry, 0, len(m))
	for _, e := range m {
		rows = append(rows, e)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Count < rows[j].Count })
	for _, e := range rows[:len(m)-keep] {
		delete(m, e.Key)
	}
}

func (r *registry) table() Table {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Table{Countries: rows(r.countries), Bots: rows(r.bots), Browsers: rows(r.browsers), Agents: rows(r.agents), Since: r.since, Total: r.total}
}

// rows is a table heaviest first: the one eating the bandwidth on top.
func rows(m map[string]*Entry) []Entry {
	out := make([]Entry, 0, len(m))
	for _, e := range m {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Key < out[j].Key
	})
	return out
}

func (r *registry) forget() {
	r.mu.Lock()
	r.reset()
	r.cache = map[string]class{}
	r.since = r.now()
	r.total = 0
	r.dirty = true
	r.mu.Unlock()
}

func (r *registry) flush() {
	r.mu.Lock()
	if !r.dirty {
		r.mu.Unlock()
		return
	}
	s := stored{Countries: rows(r.countries), Bots: rows(r.bots), Browsers: rows(r.browsers), Agents: rows(r.agents), Since: r.since, Total: r.total}
	r.dirty = false
	r.mu.Unlock()
	_ = os.MkdirAll(filepath.Dir(r.path), 0o755)
	_ = fsx.WriteJSON(r.path, s, 0o644)
}
