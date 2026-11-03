// Package visits is each site's audience: how many people came each day
// and each month, from where, on what, to which pages and from which
// links. It answers "who reads this site", where package seen answers
// "who calls at it", robots included.
//
// It costs next to nothing because it does almost nothing per request.
// What is counted is decided from the request's headers before the site
// answers, and nearly every request is out at the first test: only a
// browser's top-level navigation (Sec-Fetch-Dest: document) gets further,
// so images, scripts, API calls and every robot that does not dress up as
// a browser cost two header reads. A page view that is counted is a few
// map increments under a lock; the disk is written once a minute, never
// per request, and nothing is parsed afterwards: there is no log to read
// back, the totals are the record.
//
// A visitor is not a cookie and not an address. It is a hash of the
// address, the User-Agent and the day (or the month), with a key kept on
// this machine: the same browser counts once a day and once a month, and
// the set of hashes is thrown away when its day or its month ends, so
// nothing written here can say later who came.
//
// Robots are kept out three times. A User-Agent any table knows as a
// robot, or that is no known browser, is not a visitor; a known browser's
// name in a request that does not navigate like one (no Sec-Fetch
// headers, no Accept-Language) is a program wearing it; and a headless
// browser that says so is a robot too. What is left is people, and the
// robot that imitates a whole browser perfectly, which no counter that
// does not run code in the page can tell apart either.
package visits

import (
	"crypto/rand"
	"encoding/hex"
	"hash/fnv"
	"net"
	"net/http"
	"net/url"
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

// The bounds of a day's open tables. Countries, devices, browsers and
// systems are bounded by the world; pages, links and regions by hand,
// because a site that answers every path would otherwise let its
// visitors decide how much memory this takes. Past twice the bound a
// table is cut back to it, least seen first: the sort is paid once per
// bound's worth of new names, not per page view.
const (
	pagesKept     = 300
	referrersKept = 150
	regionsKept   = 150
	hostsKept     = 50
)

// keysKept bounds a site's set of visitor hashes for the day and for the
// month. Past it a new hash is not remembered and every page view it
// makes counts as a new visitor: a flood overcounts instead of eating
// the memory.
const keysKept = 200_000

// agentsCached bounds the classified User-Agents kept per house.
const agentsCached = 4000

// Request is what a page view says about itself, read by the caller from
// the request: this package does not know which hop is believed about the
// address or the country, package edge does.
type Request struct {
	// Domain is the site's own name: an alias and every service's name
	// count as the site, and Host says which of them was asked for.
	Domain string
	Host   string
	Path   string
	// Referrer is the Referer header, whole; only its host is kept, and
	// only when it is not one of the site's own names.
	Referrer string
	UA       string
	IP       net.IP
	// Country is a two-letter code and Region a region's name, both from
	// the edge, empty when nothing believed says.
	Country string
	Region  string
	// Mobile is Sec-CH-UA-Mobile, which a Chromium on a phone sends
	// whatever its User-Agent pretends.
	Mobile string
}

// Navigation says whether r is a browser opening a page, from its headers
// alone: the test every request goes through, so it reads as little as
// it can. A fetch, an image, a script, a prefetch and a program that does
// not send what every browser of the last five years sends on a
// navigation are not.
func Navigation(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	h := r.Header
	if h.Get("Sec-Fetch-Dest") != "document" || h.Get("Sec-Fetch-Mode") != "navigate" {
		return false
	}
	// A page fetched ahead in case it is opened is not a page opened; when
	// it is, the browser asks again or shows it without asking, and the
	// first is counted while the second, like a page from the cache, is
	// not seen by anyone on this side.
	if strings.Contains(h.Get("Sec-Purpose"), "prefetch") || strings.EqualFold(h.Get("Purpose"), "prefetch") || strings.EqualFold(h.Get("X-Moz"), "prefetch") {
		return false
	}
	// Every browser says which languages its person reads; libraries that
	// copy a browser's User-Agent rarely copy this too.
	return h.Get("Accept-Language") != ""
}

// Answered says whether the site's answer to a navigation was a page: a
// redirect, an error or a download is not a page view.
func Answered(status int, contentType string) bool {
	if status == http.StatusNotModified {
		return true
	}
	if status < 200 || status > 299 {
		return false
	}
	ct := strings.ToLower(contentType)
	return ct == "" || strings.HasPrefix(ct, "text/html") || strings.HasPrefix(ct, "application/xhtml")
}

// Book is every site's audience, under data/visits/<domain>/<YYYY-MM>.json.
type Book struct {
	dir    string
	secret []byte
	now    func() time.Time

	mu    sync.Mutex
	sites map[string]*registry

	cmu   sync.Mutex
	agent map[string]agentClass
}

// New opens the book under dir. Its key is made the first time and kept,
// so a restart does not count everyone who came today a second time.
func New(dir string) *Book {
	return newBook(dir, time.Now)
}

func newBook(dir string, now func() time.Time) *Book {
	b := &Book{dir: dir, now: now, sites: map[string]*registry{}, agent: map[string]agentClass{}}
	b.secret = loadSecret(filepath.Join(dir, ".key"))
	return b
}

func loadSecret(path string) []byte {
	if s, err := fsx.ReadTrimmed(path); err == nil {
		if k, err := hex.DecodeString(s); err == nil && len(k) >= 16 {
			return k
		}
	}
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	// A key that cannot be written still counts, for as long as the
	// process lives: the price is a day counted twice after a restart.
	_ = fsx.WriteAtomic(path, []byte(hex.EncodeToString(k)+"\n"), 0o600)
	return k
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
		r = &registry{dir: filepath.Join(b.dir, domain), past: map[string]MonthRow{}}
		b.sites[domain] = r
	}
	return r
}

// Note counts one page view, already known to be a browser's navigation
// that the site answered with a page. It decides here whether the browser
// is a person.
func (b *Book) Note(q Request) {
	cl := b.classify(q.UA)
	if !cl.person {
		return
	}
	r := b.site(q.Domain)
	if r == nil {
		return
	}
	device := cl.device
	// A phone's Chromium in desktop mode says Macintosh or Linux, and
	// still says ?1 here.
	if q.Mobile == "?1" && device == "desktop" {
		device = "mobile"
	}
	now := b.now()
	day := now.Format("2006-01-02")
	month := now.Format("2006-01")
	addr := ""
	if q.IP != nil {
		addr = q.IP.String()
	}
	dayKey := b.hash(day, addr, q.UA)
	monthKey := b.hash(month, addr, q.UA)

	country := strings.ToUpper(strings.TrimSpace(q.Country))
	// XX and T1 are Cloudflare's "unknown" and "Tor", not countries.
	if !countryCode.MatchString(country) || country == "XX" || country == "T1" {
		country = ""
	}
	region := ""
	if country != "" && q.Region != "" {
		region = country + "/" + short(strings.TrimSpace(q.Region), 60)
	}
	host := strings.ToLower(q.Host)
	if i := strings.LastIndexByte(host, ':'); i > 0 && !strings.Contains(host[i:], "]") {
		host = host[:i]
	}
	r.note(now, month, day, dayKey, monthKey, view{
		host: host, path: cleanPath(q.Path), referrer: referrerOf(q.Referrer, host, q.Domain),
		country: country, region: region, device: device, browser: cl.browser, os: cl.os,
	})
}

func (b *Book) hash(period, addr, ua string) uint64 {
	h := fnv.New64a()
	h.Write(b.secret)
	h.Write([]byte{0})
	h.Write([]byte(period))
	h.Write([]byte{0})
	h.Write([]byte(addr))
	h.Write([]byte{0})
	h.Write([]byte(ua))
	return h.Sum64()
}

var countryCode = regexp.MustCompile(`^[A-Z]{2}$`)

// cleanPath is a page as a list shows it: no query, which carries
// sessions and tracking ids and would make every visit its own page.
func cleanPath(p string) string {
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	if p == "" {
		p = "/"
	}
	return short(p, 200)
}

// referrerOf is where a visitor came from: another site's host without
// its www., or "" when it came from nowhere (typed, a bookmark, an app)
// or from the site itself.
func referrerOf(ref, host, domain string) string {
	if ref == "" {
		return ""
	}
	u, err := url.Parse(ref)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	rh := strings.ToLower(u.Hostname())
	bare := func(s string) string { return strings.TrimPrefix(s, "www.") }
	if bare(rh) == bare(host) || bare(rh) == bare(domain) {
		return ""
	}
	return short(bare(rh), 100)
}

func short(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// agentClass is a User-Agent already understood.
type agentClass struct {
	person              bool
	browser, os, device string
}

// notPeople are browsers that say they are driven by a program. They are
// not robots to the security door, which may let them in; they are not
// readers here.
var notPeople = []string{"headless", "phantomjs", "slimerjs", "lighthouse", "pagespeed", "gtmetrix", "ptst/", "prerender", "puppeteer", "playwright", "selenium", "webdriver", "cypress", "electron/"}

func (b *Book) classify(ua string) agentClass {
	b.cmu.Lock()
	cl, ok := b.agent[ua]
	b.cmu.Unlock()
	if ok {
		return cl
	}
	cl = classify(ua)
	b.cmu.Lock()
	if len(b.agent) >= agentsCached {
		// Version numbers make User-Agents endless: the cache starts over
		// rather than keep the first few thousand it met for good.
		b.agent = map[string]agentClass{}
	}
	b.agent[ua] = cl
	b.cmu.Unlock()
	return cl
}

func classify(ua string) agentClass {
	low := strings.ToLower(strings.TrimSpace(ua))
	if low == "" {
		return agentClass{}
	}
	if _, bot := security.Bot(low); bot {
		return agentClass{}
	}
	browser := security.Browser(low)
	if browser == security.BrowserOther || browser == security.BrowserNone {
		return agentClass{}
	}
	for _, m := range notPeople {
		if strings.Contains(low, m) {
			return agentClass{}
		}
	}
	return agentClass{person: true, browser: browser, os: osOf(low), device: deviceOf(low)}
}

func osOf(ua string) string {
	switch {
	case strings.Contains(ua, "windows"):
		return "windows"
	case strings.Contains(ua, "iphone"), strings.Contains(ua, "ipad"), strings.Contains(ua, "ipod"):
		return "ios"
	case strings.Contains(ua, "android"):
		return "android"
	case strings.Contains(ua, "cros"):
		return "chromeos"
	case strings.Contains(ua, "mac os x"), strings.Contains(ua, "macintosh"):
		return "macos"
	case strings.Contains(ua, "linux"):
		return "linux"
	}
	return "other"
}

func deviceOf(ua string) string {
	switch {
	case strings.Contains(ua, "ipad"), strings.Contains(ua, "tablet"), strings.Contains(ua, "kindle"), strings.Contains(ua, "silk/"):
		return "tablet"
	case strings.Contains(ua, "android") && !strings.Contains(ua, "mobile"):
		// Android's own rule: a phone says Mobile, a tablet does not.
		return "tablet"
	case strings.Contains(ua, "mobi"), strings.Contains(ua, "iphone"), strings.Contains(ua, "ipod"), strings.Contains(ua, "android"):
		return "mobile"
	}
	return "desktop"
}

// view is one counted page view, already reduced to what is kept.
type view struct {
	host, path, referrer                 string
	country, region, device, browser, os string
}

// Day is one day of a site's audience. Visitors are people, once each that
// day; every table but Pages and Hours counts visitors, by what they had
// when they first came that day; Pages and Hours count page views.
type Day struct {
	Visitors  int64            `json:"visitors"`
	Pageviews int64            `json:"pageviews"`
	Countries map[string]int64 `json:"countries,omitempty"`
	Regions   map[string]int64 `json:"regions,omitempty"`
	Devices   map[string]int64 `json:"devices,omitempty"`
	Browsers  map[string]int64 `json:"browsers,omitempty"`
	OS        map[string]int64 `json:"os,omitempty"`
	Referrers map[string]int64 `json:"referrers,omitempty"`
	Hosts     map[string]int64 `json:"hosts,omitempty"`
	Pages     map[string]int64 `json:"pages,omitempty"`
	Hours     [24]int64        `json:"hours"`
}

// monthFile is one month of a site. The hashes are written only while the
// month, or the day, is the current one: they are what keeps a restart
// from counting everyone again, and nothing once it is over.
type monthFile struct {
	Month     string          `json:"month"`
	Unique    int64           `json:"unique"`
	Days      map[string]*Day `json:"days"`
	MonthKeys []uint64        `json:"month_keys,omitempty"`
	KeysDay   string          `json:"keys_day,omitempty"`
	DayKeys   []uint64        `json:"day_keys,omitempty"`
}

type registry struct {
	dir string

	mu        sync.Mutex
	cur       *monthFile
	day       string
	dayKeys   map[uint64]struct{}
	monthKeys map[uint64]struct{}
	dirty     bool
	// past holds the totals of months that are over: they no longer
	// change, and the list of months is asked for on every visit to the
	// tab.
	past map[string]MonthRow
}

func (r *registry) path(month string) string { return filepath.Join(r.dir, month+".json") }

func readMonth(path, month string) *monthFile {
	m := &monthFile{}
	// A file that does not read is a month that starts over, and nothing
	// else: no site depends on this.
	if err := fsx.ReadJSON(path, m); err != nil || m.Month != month {
		m = &monthFile{Month: month}
	}
	if m.Days == nil {
		m.Days = map[string]*Day{}
	}
	return m
}

// open makes month and day the current ones, writing out the month that
// ended. Called with r.mu held.
func (r *registry) open(month, day string) {
	if r.cur != nil && r.cur.Month != month {
		r.writeLocked(false)
		r.past[r.cur.Month] = totals(r.cur)
		r.cur = nil
	}
	if r.cur == nil {
		r.cur = readMonth(r.path(month), month)
		r.monthKeys = setOf(r.cur.MonthKeys)
		r.cur.MonthKeys = nil
		r.day = r.cur.KeysDay
		r.dayKeys = setOf(r.cur.DayKeys)
		r.cur.DayKeys, r.cur.KeysDay = nil, ""
	}
	if r.day != day {
		r.day = day
		r.dayKeys = map[uint64]struct{}{}
	}
}

func setOf(keys []uint64) map[uint64]struct{} {
	s := make(map[uint64]struct{}, len(keys))
	for _, k := range keys {
		s[k] = struct{}{}
	}
	return s
}

func (r *registry) note(now time.Time, month, day string, dayKey, monthKey uint64, v view) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.open(month, day)
	d := r.cur.Days[day]
	if d == nil {
		d = &Day{}
		r.cur.Days[day] = d
	}
	r.dirty = true
	d.Pageviews++
	d.Hours[now.Hour()]++
	bump(&d.Pages, v.path, pagesKept)
	if _, ok := r.monthKeys[monthKey]; !ok {
		r.cur.Unique++
		if len(r.monthKeys) < keysKept {
			r.monthKeys[monthKey] = struct{}{}
		}
	}
	if _, ok := r.dayKeys[dayKey]; ok {
		return
	}
	if len(r.dayKeys) < keysKept {
		r.dayKeys[dayKey] = struct{}{}
	}
	d.Visitors++
	bump(&d.Devices, v.device, 0)
	bump(&d.Browsers, v.browser, 0)
	bump(&d.OS, v.os, 0)
	bump(&d.Hosts, v.host, hostsKept)
	ref := v.referrer
	if ref == "" {
		ref = Direct
	}
	bump(&d.Referrers, ref, referrersKept)
	if v.country != "" {
		bump(&d.Countries, v.country, 0)
	}
	if v.region != "" {
		bump(&d.Regions, v.region, regionsKept)
	}
}

// Direct is the referrer of a visitor that came from no link.
const Direct = "(direct)"

func bump(m *map[string]int64, key string, keep int) {
	if key == "" {
		return
	}
	if *m == nil {
		*m = map[string]int64{}
	}
	(*m)[key]++
	if keep > 0 && len(*m) > 2*keep {
		trim(*m, keep)
	}
}

// trim leaves the keep most seen.
func trim(m map[string]int64, keep int) {
	rows := rowsOf(m)
	for _, e := range rows[keep:] {
		delete(m, e.Key)
	}
}

// Flush writes every site whose month changed. Called every minute and
// when the process stops, never per request.
func (b *Book) Flush() {
	b.mu.Lock()
	list := make([]*registry, 0, len(b.sites))
	for _, r := range b.sites {
		list = append(list, r)
	}
	b.mu.Unlock()
	month := b.now().Format("2006-01")
	for _, r := range list {
		r.mu.Lock()
		switch {
		case r.cur != nil && r.cur.Month != month:
			// A month that ended with no visit since: its hashes go now,
			// not with the next visitor, who may not come for weeks.
			r.writeLocked(false)
			r.past[r.cur.Month] = totals(r.cur)
			r.cur, r.dayKeys, r.monthKeys, r.day = nil, nil, nil, ""
		case r.dirty && r.cur != nil:
			r.writeLocked(true)
		}
		r.mu.Unlock()
	}
}

// writeLocked writes the current month; with keys, the hashes that keep
// it from counting again after a restart. Called with r.mu held: a minute
// of a site's page views is a few kilobytes, and writing them while the
// next one waits a millisecond is simpler than copying them out.
func (r *registry) writeLocked(keys bool) {
	m := *r.cur
	if keys {
		m.MonthKeys = keysOf(r.monthKeys)
		m.KeysDay = r.day
		m.DayKeys = keysOf(r.dayKeys)
	}
	_ = os.MkdirAll(r.dir, 0o755)
	if fsx.WriteJSON(r.path(m.Month), m, 0o644) == nil {
		r.dirty = false
	}
}

func keysOf(s map[uint64]struct{}) []uint64 {
	out := make([]uint64, 0, len(s))
	for k := range s {
		out = append(out, k)
	}
	return out
}

// Row is one line of a table, heaviest first.
type Row struct {
	Key   string `json:"key"`
	Count int64  `json:"count"`
}

// DayRow is one day of the chart.
type DayRow struct {
	Date      string `json:"date"`
	Visitors  int64  `json:"visitors"`
	Pageviews int64  `json:"pageviews"`
}

// MonthRow is one month: Unique is people, once each that month; Visits
// is the sum of each day's visitors.
type MonthRow struct {
	Month     string `json:"month"`
	Unique    int64  `json:"unique"`
	Visits    int64  `json:"visits"`
	Pageviews int64  `json:"pageviews"`
}

// Report is a site's audience between two days, both included.
type Report struct {
	From      string   `json:"from"`
	To        string   `json:"to"`
	Days      []DayRow `json:"days"`
	Visits    int64    `json:"visits"`
	Pageviews int64    `json:"pageviews"`
	// Unique is set when the range is one whole calendar month: people
	// cannot be added up across months without keeping who they were.
	Unique    int64      `json:"unique,omitempty"`
	Countries []Row      `json:"countries"`
	Regions   []Row      `json:"regions"`
	Devices   []Row      `json:"devices"`
	Browsers  []Row      `json:"browsers"`
	OS        []Row      `json:"os"`
	Referrers []Row      `json:"referrers"`
	Hosts     []Row      `json:"hosts"`
	Pages     []Row      `json:"pages"`
	Hours     [24]int64  `json:"hours"`
	Months    []MonthRow `json:"months"`
}

// rowsKept is how many lines of each table a report carries.
const rowsKept = 100

// Report reads domain's audience from from to to, both days included.
func (b *Book) Report(domain string, from, to time.Time) Report {
	from = time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.Local)
	to = time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, time.Local)
	rep := Report{From: from.Format("2006-01-02"), To: to.Format("2006-01-02"), Days: []DayRow{}, Months: []MonthRow{}}
	r := b.site(domain)
	if r == nil || to.Before(from) {
		return rep.withRows(nil)
	}
	months := map[string]*monthFile{}
	for m := time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, time.Local); !m.After(to); m = m.AddDate(0, 1, 0) {
		key := m.Format("2006-01")
		months[key] = r.month(key)
	}
	sum := &Day{}
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		day := months[d.Format("2006-01")].Days[d.Format("2006-01-02")]
		if day == nil {
			rep.Days = append(rep.Days, DayRow{Date: d.Format("2006-01-02")})
			continue
		}
		rep.Days = append(rep.Days, DayRow{Date: d.Format("2006-01-02"), Visitors: day.Visitors, Pageviews: day.Pageviews})
		add(sum, day)
	}
	rep.Visits, rep.Pageviews, rep.Hours = sum.Visitors, sum.Pageviews, sum.Hours
	if from.Day() == 1 && to.Equal(from.AddDate(0, 1, -1)) {
		rep.Unique = months[from.Format("2006-01")].Unique
	}
	rep.Months = r.months()
	return rep.withRows(sum)
}

func (rep Report) withRows(sum *Day) Report {
	if sum == nil {
		sum = &Day{}
	}
	rep.Countries, rep.Regions, rep.Devices = top(sum.Countries), top(sum.Regions), top(sum.Devices)
	rep.Browsers, rep.OS, rep.Referrers = top(sum.Browsers), top(sum.OS), top(sum.Referrers)
	rep.Hosts, rep.Pages = top(sum.Hosts), top(sum.Pages)
	return rep
}

func top(m map[string]int64) []Row {
	rows := rowsOf(m)
	if len(rows) > rowsKept {
		rows = rows[:rowsKept]
	}
	return rows
}

func add(dst, src *Day) {
	dst.Visitors += src.Visitors
	dst.Pageviews += src.Pageviews
	for i := range dst.Hours {
		dst.Hours[i] += src.Hours[i]
	}
	merge := func(d *map[string]int64, s map[string]int64) {
		for k, v := range s {
			if *d == nil {
				*d = map[string]int64{}
			}
			(*d)[k] += v
		}
	}
	merge(&dst.Countries, src.Countries)
	merge(&dst.Regions, src.Regions)
	merge(&dst.Devices, src.Devices)
	merge(&dst.Browsers, src.Browsers)
	merge(&dst.OS, src.OS)
	merge(&dst.Referrers, src.Referrers)
	merge(&dst.Hosts, src.Hosts)
	merge(&dst.Pages, src.Pages)
}

// month is a copy of month's file, from memory when it is the current one.
func (r *registry) month(month string) *monthFile {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur != nil && r.cur.Month == month {
		c := &monthFile{Month: month, Unique: r.cur.Unique, Days: make(map[string]*Day, len(r.cur.Days))}
		for k, d := range r.cur.Days {
			c.Days[k] = copyDay(d)
		}
		return c
	}
	return readMonth(r.path(month), month)
}

func copyDay(d *Day) *Day {
	c := &Day{}
	add(c, d)
	return c
}

func totals(m *monthFile) MonthRow {
	row := MonthRow{Month: m.Month, Unique: m.Unique}
	for _, d := range m.Days {
		row.Visits += d.Visitors
		row.Pageviews += d.Pageviews
	}
	return row
}

var monthName = regexp.MustCompile(`^\d{4}-\d{2}\.json$`)

// months is every month the site has, newest first.
func (r *registry) months() []MonthRow {
	entries, _ := os.ReadDir(r.dir)
	names := map[string]bool{}
	for _, e := range entries {
		if monthName.MatchString(e.Name()) {
			names[strings.TrimSuffix(e.Name(), ".json")] = true
		}
	}
	r.mu.Lock()
	cur := ""
	var curRow MonthRow
	if r.cur != nil {
		cur, curRow = r.cur.Month, totals(r.cur)
		names[cur] = true
	}
	r.mu.Unlock()
	out := make([]MonthRow, 0, len(names))
	for name := range names {
		if name == cur {
			out = append(out, curRow)
			continue
		}
		r.mu.Lock()
		row, ok := r.past[name]
		r.mu.Unlock()
		if !ok {
			row = totals(readMonth(r.path(name), name))
			r.mu.Lock()
			r.past[name] = row
			r.mu.Unlock()
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Month > out[j].Month })
	return out
}

func rowsOf(m map[string]int64) []Row {
	out := make([]Row, 0, len(m))
	for k, v := range m {
		out = append(out, Row{Key: k, Count: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Key < out[j].Key
	})
	return out
}
