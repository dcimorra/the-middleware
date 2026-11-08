package security

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	chrome  = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36"
	edgeUA  = "Mozilla/5.0 (Windows NT 10.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36 Edg/130.0"
	gptbot  = "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; GPTBot/1.2; +https://openai.com/gptbot)"
	google  = "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)"
	appleAI = "Mozilla/5.0 (Macintosh) Safari/605.1.15 (Applebot-Extended/0.1)"
	newbot  = "Mozilla/5.0 (compatible; Somethingnew-Bot/1.0)"
)

func compile(t *testing.T, r *Rules) *Filter {
	t.Helper()
	f, err := Compile(r)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestAMisspeltRuleIsAnErrorNotASkippedRule(t *testing.T) {
	bad := []*Rules{
		{Bots: &Bots{Deny: []string{"ia"}}},
		{Browsers: &List{Allow: []string{"chromium"}}},
		{Countries: &List{Deny: []string{"ESP"}}},
		{Agents: &List{Deny: []string{"x"}}},
		{Paths: []string{"wp-login.php"}},
		{Paths: []string{"/"}},
		{Paths: []string{"/.well-known/acme-challenge"}},
		{Bots: &Bots{Window: &Window{From: "25:00", To: "07:00"}}},
		{Bots: &Bots{Window: &Window{From: "00:00", To: "07:00", Zone: "Mars/Olympus"}}},
	}
	for _, r := range bad {
		if _, err := Compile(r); err == nil {
			t.Errorf("%+v compiled", r)
		}
	}
}

func TestTheMostSpecificReasonWins(t *testing.T) {
	f := compile(t, &Rules{
		Agents:    &List{Deny: []string{"gptbot"}},
		Bots:      &Bots{Deny: []string{"ai"}},
		Countries: &List{Deny: []string{"RU"}},
	})
	if v := f.Decide("RU", gptbot, time.Now()); v.Reason != ByAgent || v.What != "gptbot" {
		t.Fatalf("got %+v", v)
	}
}

func TestRobotsAreClosedByFamilyWithExceptionsByName(t *testing.T) {
	f := compile(t, &Rules{Bots: &Bots{Deny: []string{"search", "ai", "other"}, Except: []string{"googlebot"}}})
	now := time.Now()
	cases := []struct {
		ua   string
		pass bool
	}{
		{chrome, true}, {google, true}, {gptbot, false}, {newbot, false},
		// Apple's AI crawler is not read as Apple's search engine.
		{appleAI, false},
		{"", true},
	}
	for _, c := range cases {
		if v := f.Decide("", c.ua, now); v.Pass != c.pass {
			t.Errorf("%q: %+v", c.ua, v)
		}
	}
	if r, _ := Bot(strings.ToLower(appleAI)); r.Family != "ai" {
		t.Fatalf("Applebot-Extended read as %+v", r)
	}
}

func TestTheWindowLetsAFamilyInAtItsHoursInItsZone(t *testing.T) {
	f := compile(t, &Rules{Bots: &Bots{Deny: []string{"ai"}, Window: &Window{From: "22:00", To: "07:00", Zone: "Europe/Madrid"}}})
	madrid, _ := time.LoadLocation("Europe/Madrid")
	for hour, pass := range map[int]bool{23: true, 3: true, 6: true, 7: false, 12: false, 21: false} {
		at := time.Date(2026, 10, 20, hour, 30, 0, 0, madrid)
		if v := f.Decide("", gptbot, at); v.Pass != pass {
			t.Errorf("%02d:30 Madrid: %+v", hour, v)
		}
	}
}

func TestBrowsersAndCountriesAreAllowLists(t *testing.T) {
	f := compile(t, &Rules{Browsers: &List{Allow: []string{"chrome", "firefox"}}, Countries: &List{Allow: []string{"es"}}})
	now := time.Now()
	if v := f.Decide("ES", chrome, now); !v.Pass {
		t.Fatalf("chrome from ES: %+v", v)
	}
	if v := f.Decide("ES", edgeUA, now); v.Pass || v.What != "edge" {
		t.Fatalf("edge passed as chrome: %+v", v)
	}
	if v := f.Decide("ES", "curl/8.0", now); v.Pass || v.What != BrowserOther {
		t.Fatalf("curl: %+v", v)
	}
	// No country is not a country on the list.
	if v := f.Decide("", chrome, now); v.Pass || v.What != "XX" {
		t.Fatalf("no country: %+v", v)
	}
}

func TestProbePathsAreCutAndWellKnownNever(t *testing.T) {
	f := compile(t, &Rules{Paths: []string{"/wp-login.php", "/admin/"}})
	cut := []string{"/.git/config", "/old/.git/HEAD", "/.env", "/.env.production", "/a/../.ssh/id_rsa", "//.SSH/x",
		"/home/u/id_ed25519", "/etc/passwd", "/proc/self/environ", "/wp-login.php", "/admin", "/admin/x", "/WP-LOGIN.PHP"}
	for _, p := range cut {
		if v := f.DecidePath(p); v.Pass {
			t.Errorf("%s served", p)
		}
	}
	served := []string{"/", "/environment", "/git/config", "/.well-known/acme-challenge/x", "/administration", "/wp-login.php5", "/blog/env"}
	for _, p := range served {
		if v := f.DecidePath(p); !v.Pass {
			t.Errorf("%s cut by %s", p, v.What)
		}
	}
	// A site without a section still has the house's list.
	if v := (*Filter)(nil).DecidePath("/.git/config"); v.Pass {
		t.Fatal("no rules let .git through")
	}
}

func TestTheDoorRefusesRecordsAndRecompilesAReloadedManifest(t *testing.T) {
	rules := &Rules{Bots: &Bots{Deny: []string{"ai"}, Window: &Window{From: "00:00", To: "07:00", Zone: "UTC"}}}
	noon := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	d := New(Options{
		Lookup: func(host string) (Site, bool) {
			return Site{Domain: host, Rules: rules, Title: "Shop"}, host == "shop.example"
		},
		Now: func() time.Time { return noon },
	})
	h := d.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	do := func(path, ua string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "http://shop.example"+path, nil)
		r.Header.Set("User-Agent", ua)
		r.Header.Set("Accept", "text/html")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	if rec := do("/", chrome); rec.Code != 200 {
		t.Fatalf("a browser got %d", rec.Code)
	}
	rec := do("/", gptbot)
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), "00:00 and 07:00 (UTC") || rec.Header().Get("X-Robots-Tag") != "noindex, nofollow" {
		t.Fatalf("the crawler got %d, robots %q:\n%s", rec.Code, rec.Header().Get("X-Robots-Tag"), rec.Body.String())
	}
	if rec := do("/.git/config", chrome); rec.Code != 404 || !strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("a probe got %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	blocks := d.Recent("shop.example")
	if len(blocks) != 2 || blocks[0].Reason != ByPath || blocks[1].What != "GPTBot" {
		t.Fatalf("recent: %+v", blocks)
	}
	// A new manifest is a new pointer, and the door reads it.
	rules = &Rules{Message: "Not today."}
	if rec := do("/", gptbot); rec.Code != 200 {
		t.Fatalf("after the reload the crawler got %d", rec.Code)
	}
	rules = &Rules{Message: "Not today.", Agents: &List{Deny: []string{"gptbot"}}}
	if rec := do("/", gptbot); rec.Code != 403 || !strings.Contains(rec.Body.String(), "Not today.") {
		t.Fatalf("the site's message was not used: %d", rec.Code)
	}
	for i := 0; i < 60; i++ {
		do("/.env", chrome)
	}
	if n := len(d.Recent("shop.example")); n != keep {
		t.Fatalf("kept %d refusals", n)
	}
}
