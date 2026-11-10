package httpcache

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type origin struct {
	calls  atomic.Int64
	header map[string]string
	status int
	delay  time.Duration
}

func (o *origin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := o.calls.Add(1)
	if o.delay > 0 {
		time.Sleep(o.delay)
	}
	for k, v := range o.header {
		w.Header().Set(k, v)
	}
	if o.status != 0 {
		w.WriteHeader(o.status)
	}
	fmt.Fprintf(w, "answer %d for %s %s", n, r.URL.Path, r.Header.Get("Accept-Encoding"))
}

func setup(rules []Rule, o *origin) (*Cache, http.Handler, *Site) {
	c := New(64 << 20)
	site := &Site{Domain: "a.example", Rules: rules, Generation: "g1"}
	return c, c.Wrap(o, func(*http.Request) (Site, bool) { return *site, true }), site
}

func get(h http.Handler, path string, headers ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "http://a.example"+path, nil)
	for _, kv := range headers {
		k, v, _ := strings.Cut(kv, ": ")
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestOriginModeKeepsWhatTheSiteMarksAndOnlyThat(t *testing.T) {
	o := &origin{header: map[string]string{"Cache-Control": "public, s-maxage=60, max-age=0"}}
	_, h, _ := setup([]Rule{{Path: "/", Mode: ModeOrigin}}, o)
	first := get(h, "/product/1")
	second := get(h, "/product/1")
	if first.Header().Get("X-Middleware-Cache") != "miss" || second.Header().Get("X-Middleware-Cache") != "hit" || second.Body.String() != first.Body.String() || o.calls.Load() != 1 {
		t.Fatalf("calls %d, %q then %q", o.calls.Load(), first.Header().Get("X-Middleware-Cache"), second.Header().Get("X-Middleware-Cache"))
	}
	// A site that says nothing is not cached in origin mode.
	quiet := &origin{}
	_, h2, _ := setup([]Rule{{Path: "/", Mode: ModeOrigin}}, quiet)
	get(h2, "/x")
	get(h2, "/x")
	if quiet.calls.Load() != 2 {
		t.Fatal("origin mode cached a response the site did not mark")
	}
}

func TestTheWallsAManifestCannotRemove(t *testing.T) {
	cases := map[string]*origin{
		"set-cookie":  {header: map[string]string{"Set-Cookie": "s=1"}},
		"private":     {header: map[string]string{"Cache-Control": "private, max-age=60"}},
		"no-store":    {header: map[string]string{"Cache-Control": "no-store"}},
		"vary-cookie": {header: map[string]string{"Vary": "Cookie"}},
		"vary-star":   {header: map[string]string{"Vary": "*"}},
		"404":         {status: 404},
		"stream":      {header: map[string]string{"Content-Type": "text/event-stream"}},
	}
	for name, o := range cases {
		_, h, _ := setup([]Rule{{Path: "/", Mode: ModeForce, TTL: 60}}, o)
		get(h, "/x")
		get(h, "/x")
		if o.calls.Load() != 2 {
			t.Errorf("%s was cached under force", name)
		}
	}
	o := &origin{}
	_, h, _ := setup([]Rule{{Path: "/", Mode: ModeForce, TTL: 60}}, o)
	get(h, "/x", "Authorization: Bearer a")
	get(h, "/x", "Authorization: Bearer b")
	if o.calls.Load() != 2 {
		t.Error("a request with Authorization was answered from the cache")
	}
}

func TestAPageForHttpIsNotGivenToHttps(t *testing.T) {
	o := &origin{}
	_, h, _ := setup([]Rule{{Path: "/", Mode: ModeForce, TTL: 60}}, o)
	visit := func(url string, headers ...string) string {
		r := httptest.NewRequest("GET", url, nil) // https:// sets r.TLS
		for _, kv := range headers {
			k, v, _ := strings.Cut(kv, ": ")
			r.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Header().Get("X-Middleware-Cache")
	}
	visit("http://a.example/x")
	if got := visit("https://a.example/x"); got != "miss" {
		t.Fatalf("https was answered %q from what http kept", got)
	}
	if got := visit("https://a.example/x"); got != "hit" {
		t.Fatalf("https did not keep its own: %q", got)
	}
	// A direct client that says https is not believed, by the cache as
	// by the site: it is plain http, and gets plain http's page.
	if got := visit("http://a.example/x", "X-Forwarded-Proto: https"); got != "hit" || o.calls.Load() != 2 {
		t.Fatalf("a forged X-Forwarded-Proto: %q after %d calls", got, o.calls.Load())
	}
}

func TestEveryCacheControlLineIsRead(t *testing.T) {
	h := http.Header{}
	h.Add("Cache-Control", "public, max-age=60")
	h.Add("Cache-Control", "private")
	for _, mode := range []string{ModeOrigin, ModeForce} {
		if ttl := ttlFor(Rule{Path: "/", Mode: mode, TTL: 60}, h, http.StatusOK); ttl != 0 {
			t.Errorf("%s kept for %s a page whose second line says private", mode, ttl)
		}
	}
}

func TestASignedInPageIsNeitherKeptNorAnsweredFromTheCache(t *testing.T) {
	// A site that renders the signed-in user's page and says nothing
	// about caching it: force would keep it for everyone.
	o := &origin{}
	_, h, _ := setup([]Rule{{Path: "/", Mode: ModeForce, TTL: 60}}, o)
	get(h, "/account", "Cookie: sid=alice")
	anon := get(h, "/account")
	if anon.Header().Get("X-Middleware-Cache") != "miss" || o.calls.Load() != 2 {
		t.Fatalf("an anonymous visitor got %q after a signed-in one: %q", anon.Header().Get("X-Middleware-Cache"), anon.Body)
	}
	// What an anonymous visit kept is not given to the signed-in one.
	if r := get(h, "/account", "Cookie: theme=dark; sid=bob"); r.Header().Get("X-Middleware-Cache") == "hit" {
		t.Fatal("a request with a cookie was answered from the cache")
	}
	// A cookie the site's own parser reads although Go's does not.
	if r := get(h, "/account", "Cookie: s{id=x"); r.Header().Get("X-Middleware-Cache") == "hit" {
		t.Fatal("a cookie Go cannot parse was taken as no cookie")
	}
	// The house's own pass is not the visitor's: the page behind a
	// password is kept for everyone past the door.
	if r := get(h, "/account", "Cookie: mw_access=pass; mw_access_abc=z; mw_attack=ok"); r.Header().Get("X-Middleware-Cache") != "hit" {
		t.Fatalf("the house's own cookies kept a request out of the cache: %q", r.Header().Get("X-Middleware-Cache"))
	}

	// ignore_cookies: the rule says its pages read none.
	o2 := &origin{}
	_, h2, _ := setup([]Rule{{Path: "/", Mode: ModeForce, TTL: 60, IgnoreCookies: true}}, o2)
	get(h2, "/logo.png", "Cookie: _ga=1")
	if r := get(h2, "/logo.png", "Cookie: _ga=2"); r.Header().Get("X-Middleware-Cache") != "hit" || o2.calls.Load() != 1 {
		t.Fatalf("ignore_cookies: %q after %d calls", r.Header().Get("X-Middleware-Cache"), o2.calls.Load())
	}
}

func TestRulesBySegmentAndNever(t *testing.T) {
	o := &origin{}
	_, h, _ := setup([]Rule{{Path: "/product", Mode: ModeForce, TTL: 60}, {Path: "/product/admin", Mode: ModeNever}}, o)
	get(h, "/product/1")
	get(h, "/product/1")
	get(h, "/products-cheap")
	get(h, "/products-cheap")
	get(h, "/product/admin/x")
	get(h, "/product/admin/x")
	if o.calls.Load() != 5 {
		t.Fatalf("calls %d, want 5: /product/1 once, the rest twice", o.calls.Load())
	}
}

func TestVaryKeepsVariantsApart(t *testing.T) {
	o := &origin{header: map[string]string{"Vary": "Accept-Encoding"}}
	_, h, _ := setup([]Rule{{Path: "/", Mode: ModeForce, TTL: 60}}, o)
	gz := get(h, "/x", "Accept-Encoding: gzip")
	br := get(h, "/x", "Accept-Encoding: br")
	gz2 := get(h, "/x", "Accept-Encoding: gzip")
	if gz.Body.String() == br.Body.String() || gz2.Body.String() != gz.Body.String() || o.calls.Load() != 2 {
		t.Fatalf("calls %d: %q %q %q", o.calls.Load(), gz.Body, br.Body, gz2.Body)
	}
}

func TestFortySimultaneousMissesReachTheSiteOnce(t *testing.T) {
	o := &origin{header: map[string]string{"Cache-Control": "s-maxage=60"}, delay: 100 * time.Millisecond}
	_, h, _ := setup([]Rule{{Path: "/", Mode: ModeOrigin}}, o)
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); get(h, "/expensive") }()
	}
	wg.Wait()
	if n := o.calls.Load(); n != 1 {
		t.Fatalf("%d calls to the site, want 1", n)
	}
}

func TestAHandoverEmptiesTheSite(t *testing.T) {
	o := &origin{header: map[string]string{"Cache-Control": "s-maxage=60"}}
	_, h, site := setup([]Rule{{Path: "/", Mode: ModeOrigin}}, o)
	get(h, "/x")
	site.Generation = "g2"
	if r := get(h, "/x"); r.Header().Get("X-Middleware-Cache") != "miss" {
		t.Fatal("the old instance's answer was served after a handover")
	}
}

func TestTheBudgetIsTakenFromTheLargestSite(t *testing.T) {
	c := New(4000)
	big := &origin{}
	small := &origin{}
	hb := c.Wrap(big, func(*http.Request) (Site, bool) {
		return Site{Domain: "big.example", Rules: []Rule{{Path: "/", Mode: ModeForce, TTL: 60}}}, true
	})
	hs := c.Wrap(small, func(*http.Request) (Site, bool) {
		return Site{Domain: "small.example", Rules: []Rule{{Path: "/", Mode: ModeForce, TTL: 60}}}, true
	})
	for i := 0; i < 3; i++ {
		get(hs, fmt.Sprintf("/s%d", i))
	}
	before := c.Stats("small.example").Bytes
	for i := 0; i < 200; i++ {
		get(hb, fmt.Sprintf("/b%d", i))
	}
	total, budget, _ := c.Total()
	if total > budget {
		t.Fatalf("%d bytes over a budget of %d", total, budget)
	}
	if c.Stats("small.example").Bytes != before {
		t.Fatalf("the small site paid for the big one: %d -> %d", before, c.Stats("small.example").Bytes)
	}
	_ = io.Discard
}

// held is a site whose answers stay open until the test lets them end, the
// way an event stream, a socket or a long download does. entered says each
// time a request reaches it, after its headers (and body) went out.
type held struct {
	entered chan string
	end     chan struct{}
	write   func(w http.ResponseWriter)
}

func (s *held) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.write(w)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	s.entered <- r.URL.Path
	<-s.end
}

func TestTwoStreamsOnOneAddressBothFlow(t *testing.T) {
	sse := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: hello\n\n")
	}
	plain := func(w http.ResponseWriter) { io.WriteString(w, "a plain page, which force would keep") }
	big := func(w http.ResponseWriter) {
		w.Header().Set("Cache-Control", "public, max-age=60")
		w.Write(make([]byte, maxEntry+1))
	}
	cases := []struct {
		name    string
		rule    Rule
		write   func(w http.ResponseWriter)
		headers []string
	}{
		// The site's answer says it: released when its headers go out.
		{"event stream", Rule{Path: "/", Mode: ModeOrigin}, sse, nil},
		// The request says it, whatever the answer looks like (force
		// would keep this one): the cache is not entered at all.
		{"eventsource", Rule{Path: "/", Mode: ModeForce, TTL: 60}, plain, []string{"Accept: text/event-stream"}},
		{"websocket", Rule{Path: "/", Mode: ModeForce, TTL: 60}, plain, []string{"Connection: Upgrade", "Upgrade: websocket"}},
		// Kept until it outgrew maxEntry, then released.
		{"long download", Rule{Path: "/", Mode: ModeForce, TTL: 60}, big, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &held{entered: make(chan string, 2), end: make(chan struct{}), write: tc.write}
			c := New(64 << 20)
			h := c.Wrap(s, func(*http.Request) (Site, bool) {
				return Site{Domain: "a.example", Rules: []Rule{tc.rule}, Generation: "g1"}, true
			})
			var wg sync.WaitGroup
			for i := 0; i < 2; i++ {
				wg.Add(1)
				go func() { defer wg.Done(); get(h, "/events", tc.headers...) }()
			}
			for i := 0; i < 2; i++ {
				select {
				case <-s.entered:
				case <-time.After(3 * time.Second):
					close(s.end)
					wg.Wait()
					t.Fatalf("only %d of two clients reached the site: the second waits for the first one's stream to end", i)
				}
			}
			close(s.end)
			wg.Wait()
			if c.Stats("a.example").Entries != 0 {
				t.Fatal("a stream was kept")
			}
		})
	}
}

// A browser that already has the page kept gets a 304 and no body: the
// site would have answered so, and the cache sent the whole page again
// on every visit instead.
func TestAHitTheBrowserAlreadyHasIsNotSentAgain(t *testing.T) {
	o := &origin{header: map[string]string{"ETag": `"v1"`, "Last-Modified": "Mon, 02 Nov 2026 10:00:00 GMT", "Content-Type": "text/html"}}
	_, h, _ := setup([]Rule{{Path: "/", Mode: ModeForce, TTL: 60}}, o)
	first := get(h, "/page")
	for name, headers := range map[string][]string{
		"its tag":           {`If-None-Match: "v1"`},
		"its tag, weak":     {`If-None-Match: W/"v1"`},
		"among others":      {`If-None-Match: "v0", "v1"`},
		"its date":          {"If-Modified-Since: Mon, 02 Nov 2026 10:00:00 GMT"},
		"a later date":      {"If-Modified-Since: Tue, 03 Nov 2026 10:00:00 GMT"},
		"tag over the date": {`If-None-Match: "v1"`, "If-Modified-Since: Sun, 01 Nov 2026 10:00:00 GMT"},
	} {
		rec := get(h, "/page", headers...)
		if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 || rec.Header().Get("ETag") != `"v1"` || rec.Header().Get("X-Middleware-Cache") != "hit" {
			t.Errorf("%s: %d, %d bytes, %v", name, rec.Code, rec.Body.Len(), rec.Header())
		}
	}
	for name, headers := range map[string][]string{
		"another tag":            {`If-None-Match: "v0"`},
		"an earlier date":        {"If-Modified-Since: Sun, 01 Nov 2026 10:00:00 GMT"},
		"another tag, same date": {`If-None-Match: "v0"`, "If-Modified-Since: Mon, 02 Nov 2026 10:00:00 GMT"},
		"nothing":                nil,
	} {
		rec := get(h, "/page", headers...)
		if rec.Code != 200 || rec.Body.String() != first.Body.String() {
			t.Errorf("%s: %d %q", name, rec.Code, rec.Body.String())
		}
	}
	if o.calls.Load() != 1 {
		t.Errorf("the site was asked %d times", o.calls.Load())
	}
	// An answer with no validator is always sent whole.
	bare := &origin{}
	_, h2, _ := setup([]Rule{{Path: "/", Mode: ModeForce, TTL: 60}}, bare)
	get(h2, "/x")
	if rec := get(h2, "/x", `If-None-Match: "v1"`, "If-Modified-Since: Tue, 03 Nov 2026 10:00:00 GMT"); rec.Code != 200 {
		t.Errorf("no validator kept: %d", rec.Code)
	}
}

// marking is a writer outside the cache that changes the answer's headers
// as it passes, the way the Web Vitals hook marks a page it compresses.
type marking struct {
	http.ResponseWriter
	done bool
}

func (m *marking) WriteHeader(code int) {
	if !m.done {
		m.done = true
		m.Header().Set("Content-Encoding", "gzip")
		m.Header().Del("Content-Length")
		m.Header().Set("ETag", `W/"changed"`)
	}
	m.ResponseWriter.WriteHeader(code)
}

func (m *marking) Write(p []byte) (int, error) {
	if !m.done {
		m.WriteHeader(http.StatusOK)
	}
	return m.ResponseWriter.Write(p)
}

// What is kept is what the site sent. The hooks outside the cache share
// the header map and change it on the way out; read after the handler
// returned, their marks were kept with the site's body, and a page kept
// while Web Vitals compressed it was answered ever after as gzip, plain.
func TestWhatAnOuterHookDoesToTheAnswerIsNotKeptWithIt(t *testing.T) {
	o := &origin{header: map[string]string{"Content-Type": "text/html", "ETag": `"v1"`}}
	_, h, _ := setup([]Rule{{Path: "/", Mode: ModeForce, TTL: 60}}, o)
	outer := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(&marking{ResponseWriter: w}, r) })
	first := get(outer, "/page")
	if first.Header().Get("Content-Encoding") != "gzip" {
		t.Fatal("the outer hook did not mark the first answer")
	}
	// Asked again past the outer hook: the cache's own answer.
	hit := get(h, "/page")
	if hit.Header().Get("X-Middleware-Cache") != "hit" || o.calls.Load() != 1 {
		t.Fatalf("not a hit: %v, %d calls", hit.Header(), o.calls.Load())
	}
	if ce := hit.Header().Get("Content-Encoding"); ce != "" {
		t.Errorf("the hit says Content-Encoding %q over a plain body", ce)
	}
	if et := hit.Header().Get("ETag"); et != `"v1"` {
		t.Errorf("the hit's ETag is %q, not the site's", et)
	}
	if hit.Body.String() != first.Body.String() {
		t.Errorf("the hit's body: %q", hit.Body.String())
	}
}
