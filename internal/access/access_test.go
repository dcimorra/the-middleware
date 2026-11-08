package access

import (
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/manifest"
)

func TestAPasswordHashRoundTripsAndRejectsTheWrongOne(t *testing.T) {
	h, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$pbkdf2-sha256$50000$") || !IsHash(h) {
		t.Fatalf("unexpected format %q", h)
	}
	if !CheckPassword(h, "correct horse") {
		t.Fatal("the right password does not check")
	}
	if CheckPassword(h, "battery staple") || CheckPassword(h, "") || CheckPassword("", "x") {
		t.Fatal("a wrong or empty password checked")
	}
	if CheckPassword("$pbkdf2-sha256$notanumber$abc$def", "x") || CheckPassword("plain", "plain") {
		t.Fatal("a malformed hash must never match")
	}
	h2, _ := HashPassword("correct horse")
	if h2 == h {
		t.Fatal("two hashes of the same password share a salt")
	}
	if _, err := HashPassword(""); err == nil {
		t.Fatal("an empty password must be refused")
	}
}

func siteWith(ac *manifest.Access) Lookup {
	return func(host string) (Site, bool) {
		if host == "p.example" {
			return Site{Domain: host, Access: ac}, true
		}
		return Site{}, false
	}
}

func newAccess(t *testing.T, lookup Lookup) *Access {
	t.Helper()
	a, err := New(Options{DataDir: t.TempDir(), Lookup: lookup})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
}

func do(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func req(method, path, remote string) *http.Request {
	r := httptest.NewRequest(method, "http://p.example"+path, nil)
	r.RemoteAddr = remote
	return r
}

func TestAnAddressOutsideTheAllowListGets403(t *testing.T) {
	a := newAccess(t, siteWith(&manifest.Access{Allow: []string{"10.0.0.0/8", "192.0.2.7"}}))
	h := a.Wrap(okHandler())
	if rec := do(h, req("GET", "/", "203.0.113.1:1")); rec.Code != http.StatusForbidden {
		t.Fatalf("outside got %d", rec.Code)
	}
	if rec := do(h, req("GET", "/", "10.9.8.7:1")); rec.Code != 200 {
		t.Fatalf("inside got %d", rec.Code)
	}
	if rec := do(h, req("GET", "/", "192.0.2.7:1")); rec.Code != 200 {
		t.Fatalf("single address got %d", rec.Code)
	}
	// A site without an access section is open.
	r := httptest.NewRequest("GET", "http://other.example/", nil)
	r.RemoteAddr = "203.0.113.1:1"
	if rec := do(h, r); rec.Code != 200 {
		t.Fatalf("unrelated site got %d", rec.Code)
	}
}

func TestTheLongestRoutePrefixWins(t *testing.T) {
	ac := &manifest.Access{
		Allow: []string{"10.0.0.0/8"},
		Routes: map[string]*manifest.RouteAccess{
			"/public":       {Allow: []string{"10.1.0.0/16"}},
			"/public/inner": {Allow: []string{"10.1.2.0/24"}},
		},
	}
	a := newAccess(t, siteWith(ac))
	h := a.Wrap(okHandler())
	cases := []struct {
		path, remote string
		want         int
	}{
		{"/", "203.0.113.1:1", 403},
		{"/", "10.5.0.1:1", 200},
		{"/public", "10.1.9.9:1", 200},
		{"/public/x", "10.5.0.1:1", 403},
		{"/publicity", "10.5.0.1:1", 200},
		{"/public/inner/x", "10.1.9.9:1", 403},
		{"/public/inner/x", "10.1.2.9:1", 200},
	}
	for _, tc := range cases {
		if rec := do(h, req("GET", tc.path, tc.remote)); rec.Code != tc.want {
			t.Errorf("%s from %s: got %d, want %d", tc.path, tc.remote, rec.Code, tc.want)
		}
	}
}

func TestTheLoginFormSetsASessionThatLetsTheVisitorIn(t *testing.T) {
	hash, _ := HashPassword("open sesame")
	a := newAccess(t, siteWith(&manifest.Access{Password: hash}))
	h := a.Wrap(okHandler())
	r := req("GET", "/secret?x=1", "203.0.113.1:1")
	r.Header.Set("Accept", "text/html")
	rec := do(h, r)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `name="password"`) {
		t.Fatalf("browser got %d, want the form; body %q", rec.Code, rec.Body.String()[:80])
	}
	if !strings.Contains(rec.Body.String(), `value="/secret?x=1"`) {
		t.Fatal("the form must remember where the visitor was going")
	}
	// Wrong password: the form again, no cookie.
	form := url.Values{"password": {"nope"}, "next": {"/secret?x=1"}}
	r = httptest.NewRequest("POST", "http://p.example"+LoginPath, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Accept", "text/html")
	rec = do(h, r)
	if rec.Code != http.StatusUnauthorized || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("wrong password got %d with %d cookies", rec.Code, len(rec.Result().Cookies()))
	}
	// Right password: cookie and redirect to next.
	form.Set("password", "open sesame")
	r = httptest.NewRequest("POST", "http://p.example"+LoginPath, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = do(h, r)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/secret?x=1" {
		t.Fatalf("login got %d → %q", rec.Code, rec.Header().Get("Location"))
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != sessionCookie {
		t.Fatalf("cookies %+v", cookies)
	}
	r = req("GET", "/secret", "203.0.113.1:1")
	r.AddCookie(cookies[0])
	if rec := do(h, r); rec.Code != 200 {
		t.Fatalf("with the session got %d", rec.Code)
	}
	// Changing the password ends the session.
	newHash, _ := HashPassword("something else")
	a2 := &Access{opts: a.opts, now: a.now, key: a.key, verified: map[[32]byte]struct{}{}}
	a2.opts.Lookup = siteWith(&manifest.Access{Password: newHash})
	r = req("GET", "/secret", "203.0.113.1:1")
	r.AddCookie(cookies[0])
	if rec := do(a2.Wrap(okHandler()), r); rec.Code != http.StatusUnauthorized {
		t.Fatalf("old session after a password change got %d", rec.Code)
	}
	// An open redirect is not a redirect.
	form.Set("next", "//evil.example/x")
	r = httptest.NewRequest("POST", "http://p.example"+LoginPath, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if rec := do(h, r); rec.Header().Get("Location") != "/" {
		t.Fatalf("next=//evil redirected to %q", rec.Header().Get("Location"))
	}
	// Nor is one a browser makes out of a tab: it drops the tab and reads
	// "//evil.example/x".
	form.Set("next", "/\t/evil.example/x")
	r = httptest.NewRequest("POST", "http://p.example"+LoginPath, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if rec := do(h, r); rec.Header().Get("Location") != "/" {
		t.Fatalf("next=/<tab>/evil redirected to %q", rec.Header().Get("Location"))
	}
}

// Signing out goes through a browser: the jar keeps each cookie under its
// Path, and a logout that expires a zone pass under another Path leaves
// the zone open.
func TestSigningOutClosesEveryZoneTheBrowserHoldsAPassFor(t *testing.T) {
	admin, _ := HashPassword("admin")
	a := newAccess(t, siteWith(&manifest.Access{Routes: map[string]*manifest.RouteAccess{
		"/admin/": {Password: admin}, "/open": {},
	}}))
	srv := httptest.NewServer(a.Wrap(okHandler()))
	defer srv.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	send := func(method, path string, form url.Values) int {
		t.Helper()
		var body io.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		}
		r, _ := http.NewRequest(method, srv.URL+path, body)
		r.Host = "p.example"
		r.Header.Set("Accept", "text/html")
		if form != nil {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := send("POST", LoginPath, url.Values{"password": {"admin"}, "next": {"/admin"}, "zone": {"/admin"}}); c != http.StatusSeeOther {
		t.Fatalf("signing in to /admin got %d", c)
	}
	if c := send("GET", "/admin/panel", nil); c != 200 {
		t.Fatalf("signed in, /admin/panel got %d", c)
	}
	if c := send("GET", LogoutPath, nil); c != http.StatusSeeOther {
		t.Fatalf("logout got %d", c)
	}
	if c := send("GET", "/admin/panel", nil); c != http.StatusUnauthorized {
		t.Fatalf("after signing out /admin/panel got %d", c)
	}
}

func TestHTTPBasicAuthWorksForTools(t *testing.T) {
	hash, _ := HashPassword("tool pass")
	a := newAccess(t, siteWith(&manifest.Access{Password: hash}))
	h := a.Wrap(okHandler())
	rec := do(h, req("GET", "/api", "203.0.113.1:1"))
	if rec.Code != http.StatusUnauthorized || !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Basic") {
		t.Fatalf("a non-browser got %d without a Basic challenge", rec.Code)
	}
	r := req("GET", "/api", "203.0.113.1:1")
	r.SetBasicAuth("anyone", "tool pass")
	if rec := do(h, r); rec.Code != 200 {
		t.Fatalf("basic auth got %d", rec.Code)
	}
	// The second check comes from the cache, which must still say yes,
	// and a wrong password must still say no.
	if rec := do(h, r); rec.Code != 200 {
		t.Fatalf("cached basic auth got %d", rec.Code)
	}
	r.SetBasicAuth("anyone", "wrong")
	if rec := do(h, r); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong basic auth got %d", rec.Code)
	}
}

func TestARoutePasswordLeavesTheRestOfTheSiteOpen(t *testing.T) {
	hash, _ := HashPassword("admin pass")
	ac := &manifest.Access{Routes: map[string]*manifest.RouteAccess{"/admin": {Password: hash}}}
	a := newAccess(t, siteWith(ac))
	h := a.Wrap(okHandler())
	if rec := do(h, req("GET", "/", "203.0.113.1:1")); rec.Code != 200 {
		t.Fatalf("the open part got %d", rec.Code)
	}
	if rec := do(h, req("GET", "/admin/users", "203.0.113.1:1")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the protected part got %d", rec.Code)
	}
}

func TestDeniedCountriesGet403(t *testing.T) {
	a := newAccess(t, siteWith(&manifest.Access{DenyCountries: []string{"xx"}}))
	a.opts.Country = func(_ *http.Request, ip net.IP) string {
		if ip.String() == "203.0.113.1" {
			return "XX"
		}
		return "YY"
	}
	h := a.Wrap(okHandler())
	if rec := do(h, req("GET", "/", "203.0.113.1:1")); rec.Code != http.StatusForbidden {
		t.Fatalf("denied country got %d", rec.Code)
	}
	if rec := do(h, req("GET", "/", "203.0.113.2:1")); rec.Code != 200 {
		t.Fatalf("other country got %d", rec.Code)
	}
}

func TestTheSessionKeySurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	a1, err := New(Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	a2, err := New(Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if string(a1.key) != string(a2.key) {
		t.Fatal("a new key per boot would log everyone out on restart")
	}
	st, err := os.Stat(filepath.Join(dir, "access", "key"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %o, want 0600", st.Mode().Perm())
	}
}

func TestAZoneCannotBeSkippedWithDotsSlashesOrCase(t *testing.T) {
	hash, _ := HashPassword("pw")
	a := newAccess(t, siteWith(&manifest.Access{Routes: map[string]*manifest.RouteAccess{"/admin": {Password: hash}}}))
	h := a.Wrap(okHandler())
	if rec := do(h, req("GET", "/", "203.0.113.1:1")); rec.Code != 200 {
		t.Fatalf("the open part got %d", rec.Code)
	}
	for _, p := range []string{"/admin", "/x/../admin", "//admin/users", "/ADMIN", "/Admin/", "/admin;x=1/users", "/./admin"} {
		r := req("GET", "/", "203.0.113.1:1")
		r.URL.Path = p
		if rec := do(h, r); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s got %d", p, rec.Code)
		}
	}
}

func TestWithoutItsKeyTheDoorStaysShut(t *testing.T) {
	hash, _ := HashPassword("pw")
	sites := map[string]*manifest.Access{"p.example": {Password: hash}, "o.example": nil}
	lookup := func(host string) (Site, bool) { ac, ok := sites[host]; return Site{Domain: host, Access: ac}, ok }
	h := Closed(lookup)(okHandler())
	r := httptest.NewRequest("GET", "http://p.example/", nil)
	if rec := do(h, r); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a protected site was served open: %d", rec.Code)
	}
	r = httptest.NewRequest("GET", "http://o.example/", nil)
	if rec := do(h, r); rec.Code != 200 {
		t.Fatalf("an open site was closed: %d", rec.Code)
	}
}

func login(t *testing.T, h http.Handler, zone, password string) *http.Cookie {
	t.Helper()
	form := url.Values{"password": {password}, "next": {"/"}, "zone": {zone}}
	r := httptest.NewRequest("POST", "http://p.example"+LoginPath, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := do(h, r)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login to %q got %d", zone, rec.Code)
	}
	return rec.Result().Cookies()[0]
}

func TestEachZoneHasItsOwnPassAndTheSitePasswordStillApplies(t *testing.T) {
	site, _ := HashPassword("site")
	admin, _ := HashPassword("admin")
	billing, _ := HashPassword("billing")
	a := newAccess(t, siteWith(&manifest.Access{Password: site, Routes: map[string]*manifest.RouteAccess{
		"/admin": {Password: admin}, "/billing/": {Password: billing},
	}}))
	h := a.Wrap(okHandler())
	browser := func(path string, cookies ...*http.Cookie) int {
		r := req("GET", path, "203.0.113.1:1")
		r.Header.Set("Accept", "text/html")
		for _, c := range cookies {
			r.AddCookie(c)
		}
		return do(h, r).Code
	}
	siteC := login(t, h, "", "site")
	adminC := login(t, h, "/admin", "admin")
	if adminC.Path != "/admin" || adminC.Name == siteC.Name {
		t.Fatalf("the zone pass is not scoped: %+v", adminC)
	}
	if c := browser("/", siteC); c != 200 {
		t.Fatalf("site pass on / got %d", c)
	}
	if c := browser("/admin", siteC); c != http.StatusUnauthorized {
		t.Fatalf("the site pass opened the zone: %d", c)
	}
	if c := browser("/admin", adminC); c != http.StatusUnauthorized {
		t.Fatalf("the zone pass skipped the site password: %d", c)
	}
	if c := browser("/admin/x", siteC, adminC); c != 200 {
		t.Fatalf("both passes got %d", c)
	}
	// The admin pass, renamed to the billing cookie, is worth nothing there.
	forged := &http.Cookie{Name: cookieName(door{zone: "/billing"}), Value: adminC.Value}
	if c := browser("/billing", siteC, forged); c != http.StatusUnauthorized {
		t.Fatalf("one zone's pass opened another: %d", c)
	}
}

func TestAZoneNarrowsTheSiteAndNeverWidensIt(t *testing.T) {
	a := newAccess(t, siteWith(&manifest.Access{
		Allow: []string{"10.0.0.0/8"}, Deny: []string{"10.9.9.9"},
		Routes: map[string]*manifest.RouteAccess{
			"/ops":  {Allow: []string{"10.1.0.0/16"}},
			"/open": {Allow: []string{"0.0.0.0/0"}},
		},
	}))
	h := a.Wrap(okHandler())
	cases := []struct {
		path, ip string
		want     int
	}{
		{"/", "10.2.0.1:1", 200},
		{"/", "203.0.113.1:1", 403},
		{"/", "10.9.9.9:1", 403},
		{"/ops", "10.1.0.5:1", 200},
		{"/ops", "10.2.0.1:1", 403},
		{"/open", "203.0.113.1:1", 403},
	}
	for _, c := range cases {
		if got := do(h, req("GET", c.path, c.ip)).Code; got != c.want {
			t.Errorf("%s from %s: got %d, want %d", c.path, c.ip, got, c.want)
		}
	}
}

func formLogin(h http.Handler, remote, zone, password string, html bool) *httptest.ResponseRecorder {
	form := url.Values{"password": {password}, "next": {"/"}, "zone": {zone}}
	r := httptest.NewRequest("POST", "http://p.example"+LoginPath, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if html {
		r.Header.Set("Accept", "text/html")
	}
	r.RemoteAddr = remote
	return do(h, r)
}

func TestWrongPasswordsLockAnAddressOutOfThatDoorOnly(t *testing.T) {
	site, _ := HashPassword("site pass")
	zone, _ := HashPassword("zone pass")
	a := newAccess(t, siteWith(&manifest.Access{Password: site, Routes: map[string]*manifest.RouteAccess{"/admin": {Password: zone}}}))
	now := time.Now()
	a.brake.Now = func() time.Time { return now }
	h := a.Wrap(okHandler())

	const guesser = "203.0.113.9:1"
	for i := 0; i < 5; i++ {
		if rec := formLogin(h, guesser, "", "guess", true); rec.Code != http.StatusUnauthorized {
			t.Fatalf("guess %d got %d", i, rec.Code)
		}
	}
	// Now even the right password is not looked at, by form or by basic
	// auth, and the visitor is told why and for how long.
	rec := formLogin(h, guesser, "", "site pass", true)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "60" || !strings.Contains(rec.Body.String(), "Try again in a minute") {
		t.Fatalf("locked form got %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	r := req("GET", "/", guesser)
	r.SetBasicAuth("x", "site pass")
	if rec := do(h, r); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("locked basic auth got %d", rec.Code)
	}
	// Someone else, and the same address at another door, are untouched.
	if rec := formLogin(h, "203.0.113.10:1", "", "site pass", false); rec.Code != http.StatusSeeOther {
		t.Fatalf("another address got %d", rec.Code)
	}
	if rec := formLogin(h, guesser, "/admin", "zone pass", false); rec.Code != http.StatusSeeOther {
		t.Fatalf("the zone's door got %d", rec.Code)
	}
	// A pass already given still opens: the brake is on guessing, not on
	// visitors who are inside.
	now = now.Add(61 * time.Second)
	if rec := formLogin(h, guesser, "", "site pass", false); rec.Code != http.StatusSeeOther {
		t.Fatalf("after the lock got %d", rec.Code)
	}
}

func TestAnIPv6GuesserCannotDodgeTheBrakeInsideItsSlash64(t *testing.T) {
	site, _ := HashPassword("site pass")
	a := newAccess(t, siteWith(&manifest.Access{Password: site}))
	h := a.Wrap(okHandler())
	for i := 1; i <= 5; i++ {
		formLogin(h, "[2001:db8:1:2::"+strconv.Itoa(i)+"]:1", "", "guess", false)
	}
	if rec := formLogin(h, "[2001:db8:1:2::ffff]:1", "", "site pass", false); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("a neighbour in the /64 got %d", rec.Code)
	}
	if rec := formLogin(h, "[2001:db8:1:3::1]:1", "", "site pass", false); rec.Code != http.StatusSeeOther {
		t.Fatalf("the next /64 got %d", rec.Code)
	}
}
