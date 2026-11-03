package visits

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	chromeLinux  = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36"
	safariPhone  = "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1"
	androidTab   = "Mozilla/5.0 (Linux; Android 14; SM-X700) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36"
	firefoxWin   = "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:132.0) Gecko/20100101 Firefox/132.0"
	googlebot    = "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)"
	headless     = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) HeadlessChrome/130.0 Safari/537.36"
	pythonClient = "python-requests/2.32"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func at(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, time.Local)
	if err != nil {
		panic(err)
	}
	return t
}

func page(ua, ip string) Request {
	return Request{Domain: "shop.example", Host: "shop.example", Path: "/", UA: ua, IP: net.ParseIP(ip)}
}

func TestOnlyABrowserOpeningAPageIsANavigation(t *testing.T) {
	nav := func(mut func(h http.Header), method string) bool {
		r, _ := http.NewRequest(method, "http://shop.example/", nil)
		r.Header.Set("Sec-Fetch-Dest", "document")
		r.Header.Set("Sec-Fetch-Mode", "navigate")
		r.Header.Set("Accept-Language", "es")
		if mut != nil {
			mut(r.Header)
		}
		return Navigation(r)
	}
	if !nav(nil, "GET") {
		t.Fatal("a browser's navigation was not one")
	}
	cases := map[string]func(h http.Header){
		"an image":            func(h http.Header) { h.Set("Sec-Fetch-Dest", "image") },
		"a fetch":             func(h http.Header) { h.Set("Sec-Fetch-Dest", "empty"); h.Set("Sec-Fetch-Mode", "cors") },
		"a program":           func(h http.Header) { h.Del("Sec-Fetch-Dest"); h.Del("Sec-Fetch-Mode") },
		"no languages":        func(h http.Header) { h.Del("Accept-Language") },
		"a prefetch":          func(h http.Header) { h.Set("Sec-Purpose", "prefetch;prerender") },
		"an old prefetch":     func(h http.Header) { h.Set("Purpose", "prefetch") },
		"a frame in the page": func(h http.Header) { h.Set("Sec-Fetch-Dest", "iframe") },
	}
	for name, mut := range cases {
		if nav(mut, "GET") {
			t.Errorf("%s was taken for a navigation", name)
		}
	}
	if nav(nil, "POST") || nav(nil, "HEAD") {
		t.Error("a form post or a HEAD was taken for opening a page")
	}
	for _, c := range []struct {
		status int
		ct     string
		want   bool
	}{{200, "text/html; charset=utf-8", true}, {304, "", true}, {200, "", true}, {200, "application/pdf", false}, {301, "text/html", false}, {404, "text/html", false}, {503, "text/html", false}} {
		if Answered(c.status, c.ct) != c.want {
			t.Errorf("Answered(%d, %q) = %v", c.status, c.ct, !c.want)
		}
	}
}

func TestRobotsAndProgramsAreNotVisitors(t *testing.T) {
	c := &clock{at("2026-11-03 10:00")}
	b := newBook(t.TempDir(), c.now)
	for _, ua := range []string{googlebot, headless, pythonClient, "", "curl/8.5", "Mozilla/5.0 (compatible; GPTBot/1.2; +https://openai.com/gptbot)", "something nobody knows"} {
		b.Note(page(ua, "203.0.113.9"))
	}
	if rep := b.Report("shop.example", c.t, c.t); rep.Pageviews != 0 || rep.Visits != 0 {
		t.Fatalf("robots were counted: %+v", rep)
	}
}

func TestAVisitorCountsOnceADayAndOnceAMonth(t *testing.T) {
	dir := t.TempDir()
	c := &clock{at("2026-11-03 10:00")}
	b := newBook(dir, c.now)
	b.Note(page(chromeLinux, "203.0.113.9"))
	c.t = at("2026-11-03 10:05")
	q := page(chromeLinux, "203.0.113.9")
	q.Path = "/precios?utm_source=x"
	b.Note(q)
	// The same address on another browser is another person: a house, an
	// office.
	b.Note(page(safariPhone, "203.0.113.9"))
	b.Flush()

	// A restart in the middle of the day does not count them again.
	c.t = at("2026-11-03 18:00")
	b = newBook(dir, c.now)
	b.Note(page(chromeLinux, "203.0.113.9"))
	rep := b.Report("shop.example", c.t, c.t)
	if rep.Visits != 2 || rep.Pageviews != 4 {
		t.Fatalf("after a restart: visits %d pageviews %d, want 2 and 4", rep.Visits, rep.Pageviews)
	}
	if got := rowsMap(rep.Pages); got["/"] != 3 || got["/precios"] != 1 {
		t.Errorf("pages %v: the query is not a page of its own", rep.Pages)
	}
	if got := rowsMap(rep.Devices); got["desktop"] != 1 || got["mobile"] != 1 {
		t.Errorf("devices %v", rep.Devices)
	}
	if rep.Hours[10] != 3 || rep.Hours[18] != 1 {
		t.Errorf("hours %v", rep.Hours)
	}

	// The next day they are visitors again, and still the same people of
	// the month.
	c.t = at("2026-11-04 09:00")
	b.Note(page(chromeLinux, "203.0.113.9"))
	b.Note(page(firefoxWin, "198.51.100.4"))
	month := b.Report("shop.example", at("2026-11-01 00:00"), at("2026-11-30 00:00"))
	if month.Visits != 4 || month.Unique != 3 || len(month.Days) != 30 {
		t.Fatalf("the month: visits %d unique %d days %d, want 4, 3, 30", month.Visits, month.Unique, len(month.Days))
	}
	if month.Days[2].Visitors != 2 || month.Days[3].Visitors != 2 {
		t.Errorf("the chart: %+v", month.Days[2:4])
	}
	// Two days are not a month: people cannot be added up across months
	// without keeping who they were.
	if r := b.Report("shop.example", at("2026-11-03 00:00"), at("2026-11-04 00:00")); r.Unique != 0 {
		t.Errorf("unique on a range that is no month: %d", r.Unique)
	}
}

func TestAMonthThatEndsForgetsWhoCame(t *testing.T) {
	dir := t.TempDir()
	c := &clock{at("2026-10-31 23:50")}
	b := newBook(dir, c.now)
	b.Note(page(chromeLinux, "203.0.113.9"))
	b.Flush()
	raw, _ := os.ReadFile(filepath.Join(dir, "shop.example", "2026-10.json"))
	if !strings.Contains(string(raw), "month_keys") {
		t.Fatalf("the current month does not keep its hashes for a restart: %s", raw)
	}
	c.t = at("2026-11-01 00:10")
	b.Flush()
	raw, _ = os.ReadFile(filepath.Join(dir, "shop.example", "2026-10.json"))
	if strings.Contains(string(raw), "keys") {
		t.Fatalf("a month that is over still holds who came: %s", raw)
	}
	b.Note(page(chromeLinux, "203.0.113.9"))
	rep := b.Report("shop.example", at("2026-11-01 00:00"), at("2026-11-30 00:00"))
	if rep.Unique != 1 || len(rep.Months) != 2 || rep.Months[0].Month != "2026-11" || rep.Months[1].Unique != 1 || rep.Months[1].Visits != 1 {
		t.Fatalf("months: %+v unique %d", rep.Months, rep.Unique)
	}
	// Nothing in the files can be turned back into an address.
	for _, name := range []string{"2026-10.json", "2026-11.json"} {
		b.Flush()
		raw, _ := os.ReadFile(filepath.Join(dir, "shop.example", name))
		if strings.Contains(string(raw), "203.0.113.9") || strings.Contains(string(raw), "Chrome/130") {
			t.Errorf("%s holds the visitor itself: %s", name, raw)
		}
	}
}

func TestWhereFromAndOnWhat(t *testing.T) {
	c := &clock{at("2026-11-03 10:00")}
	b := newBook(t.TempDir(), c.now)
	q := page(androidTab, "203.0.113.1")
	q.Country, q.Region, q.Referrer = "es", "Madrid", "https://www.google.com/search?q=x"
	b.Note(q)
	q = page(chromeLinux, "203.0.113.2")
	q.Country, q.Referrer, q.Host = "XX", "https://www.shop.example/otra", "www.shop.example:443"
	b.Note(q)
	q = page(chromeLinux, "203.0.113.3")
	q.Mobile = "?1"
	b.Note(q)
	rep := b.Report("shop.example", c.t, c.t)
	if got := rowsMap(rep.Countries); len(got) != 1 || got["ES"] != 1 {
		t.Errorf("countries %v: XX is no country", rep.Countries)
	}
	if got := rowsMap(rep.Regions); got["ES/Madrid"] != 1 {
		t.Errorf("regions %v", rep.Regions)
	}
	if got := rowsMap(rep.Referrers); got["google.com"] != 1 || got[Direct] != 2 {
		t.Errorf("referrers %v: the site's own pages are not a link from elsewhere", rep.Referrers)
	}
	if got := rowsMap(rep.Devices); got["tablet"] != 1 || got["desktop"] != 1 || got["mobile"] != 1 {
		t.Errorf("devices %v", rep.Devices)
	}
	if got := rowsMap(rep.OS); got["android"] != 1 || got["linux"] != 2 {
		t.Errorf("systems %v", rep.OS)
	}
	if got := rowsMap(rep.Hosts); got["shop.example"] != 2 || got["www.shop.example"] != 1 {
		t.Errorf("names %v", rep.Hosts)
	}
}

func TestEndlessPagesDoNotGrowWithoutBound(t *testing.T) {
	c := &clock{at("2026-11-03 10:00")}
	b := newBook(t.TempDir(), c.now)
	for i := 0; i < 5*pagesKept; i++ {
		q := page(chromeLinux, "203.0.113.9")
		q.Path = "/p/" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + "/" + strconv.Itoa(i)
		b.Note(q)
	}
	r := b.site("shop.example")
	if n := len(r.cur.Days["2026-11-03"].Pages); n > 2*pagesKept {
		t.Fatalf("%d pages kept", n)
	}
	if rep := b.Report("shop.example", c.t, c.t); rep.Pageviews != 5*pagesKept || len(rep.Pages) > rowsKept {
		t.Fatalf("pageviews %d, %d rows", rep.Pageviews, len(rep.Pages))
	}
	if b.site("../etc") != nil || b.site("Shop.example") != nil {
		t.Fatal("a name that is no site's got a folder")
	}
}

func rowsMap(rows []Row) map[string]int64 {
	m := map[string]int64{}
	for _, r := range rows {
		m[r.Key] = r.Count
	}
	return m
}
