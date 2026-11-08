// Package access decides who gets into a site, from the manifest's `access`
// section: networks that may enter or not, countries that may not, a
// password for the whole site, and zones (path prefixes) with rules of
// their own. It runs after the WAF (which has already refused the abusive)
// and before the proxy, so a site process never sees a request its
// operator closed the door on.
//
// Every rule adds to the ones above it, never replaces them: a zone's
// `allow` narrows the site's, a zone's password is asked for after the
// site's, if there is one. A zone that could loosen the site would make
// reading the site's rules no longer enough to know who gets in.
//
// Each door has its own pass. The zone's prefix goes into the signature, so
// the password for /admin does not open /billing; into the cookie name, so
// two doors on one domain do not overwrite each other; and into the
// cookie's Path, so one zone's pass does not even travel to the others.
package access

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"html/template"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/brake"
	"github.com/dcimorra/the-middleware/internal/edge"
	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/pages"
)

// Site is what access needs to know about the site a request is for.
type Site struct {
	Domain string
	Access *manifest.Access
	// More is the access of the service that answers the request, for a
	// site with services: judged after the site's, never instead of it.
	More *manifest.Access
	// Title and Theme dress the pages this door shows (see pages).
	Title, Theme string
}

// Lookup resolves a host to its site.
type Lookup func(host string) (Site, bool)

// Options configure Access.
type Options struct {
	// DataDir is data/; the session key lives at data/access/key.
	DataDir string
	Lookup  Lookup
	// LookupRequest, when set, resolves a request rather than its host:
	// a site with services answers a host and a path with one of them,
	// and each has its own rules. Given the path the door judges (for
	// its own sign-in page, the page the visitor is going back to).
	LookupRequest func(r *http.Request, urlPath string) (Site, bool)
	// ClientIP resolves the visitor's address; nil uses RemoteAddr only.
	ClientIP func(r *http.Request) net.IP
	// Country resolves a request's country code; nil means "".
	Country func(r *http.Request, ip net.IP) string
	Now     func() time.Time
}

// Access is the per-site door.
type Access struct {
	opts Options
	now  func() time.Time
	key  []byte

	// verified caches positive password checks by digest of (hash, plain)
	// so a tool sending HTTP basic auth on every request does not pay a
	// key derivation each time. Only successes are cached: a wrong guess
	// must cost the full derivation every time.
	mu       sync.Mutex
	verified map[[32]byte]struct{}

	// brake counts wrong passwords per domain, zone and address. Without
	// it each guess was a free key derivation, by form or by basic auth.
	brake *brake.Brake
}

// doorPolicy is gentler than the panel's at the top end: a site's
// visitors are not its operator, and a day locked out of a shared family
// album for some mistyping is the wrong price.
var doorPolicy = brake.Policy{Failures: 5, Window: time.Minute, MaxLock: 15 * time.Minute}

const (
	sessionCookie = "mw_access"
	sessionTTL    = 7 * 24 * time.Hour
	// LoginPath is where the form posts. It is under a prefix no site
	// uses, and it is answered only when the site has a password.
	LoginPath  = "/__mw/login"
	LogoutPath = "/__mw/logout"
)

// New loads or creates the session key. The key is kept on disk, unlike the
// WAF's challenge key, because a restart of the supervisor must not log
// every visitor out of every protected site.
func New(opts Options) (*Access, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	a := &Access{opts: opts, now: opts.Now, verified: map[[32]byte]struct{}{}, brake: brake.New(doorPolicy)}
	path := filepath.Join(opts.DataDir, "access", "key")
	data, err := os.ReadFile(path)
	if err != nil {
		if !fsx.IsNotExist(err) {
			return nil, err
		}
		data = make([]byte, 32)
		if _, err := rand.Read(data); err != nil {
			return nil, err
		}
		if err := fsx.WriteAtomic(path, data, 0o600); err != nil {
			return nil, err
		}
	}
	if len(data) < 16 {
		return nil, os.ErrInvalid
	}
	a.key = data
	return a, nil
}

func (a *Access) clientIP(r *http.Request) net.IP {
	if a.opts.ClientIP != nil {
		return a.opts.ClientIP(r)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return net.ParseIP(strings.Trim(host, "[]"))
}

// door is one password between a visitor and part of a site.
type door struct {
	zone    string // "" for the whole site, else the cleaned prefix
	hash    string
	message string
}

// Wrap implements the hook.
func (a *Access) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.opts.Lookup == nil && a.opts.LookupRequest == nil {
			next.ServeHTTP(w, r)
			return
		}
		site, ok := a.lookup(r)
		if !ok || (site.Access == nil && site.More == nil) {
			next.ServeHTTP(w, r)
			return
		}
		ip := a.clientIP(r)
		clean := CleanPath(r.URL.Path)
		var doors []door
		// Each level is judged on its own, the site's and then its
		// service's, so a service's rules only ever add: merged into one,
		// the service's zone under a site's zone was the longest prefix
		// and the site's password was never asked.
		for _, ac := range []*manifest.Access{site.Access, site.More} {
			if ac == nil {
				continue
			}
			zone, route := matchRoute(ac.Routes, clean)
			if !admitted(ip, ac.Allow, ac.Deny) || (route != nil && !admitted(ip, route.Allow, route.Deny)) {
				pages.Render(w, r, pages.ForSite("forbidden", site.Title, site.Domain, site.Theme))
				return
			}
			if len(ac.DenyCountries) > 0 && a.opts.Country != nil && ip != nil {
				cc := strings.ToUpper(a.opts.Country(r, ip))
				for _, d := range ac.DenyCountries {
					if cc != "" && cc == strings.ToUpper(d) {
						pages.Render(w, r, pages.ForSite("forbidden", site.Title, site.Domain, site.Theme))
						return
					}
				}
			}
			if ac.Password != "" {
				doors = append(doors, door{hash: ac.Password, message: ac.Message})
			}
			if route != nil && route.Password != "" {
				doors = append(doors, door{zone: zone, hash: route.Password, message: route.Message})
			}
		}
		switch r.URL.Path {
		case LoginPath:
			for _, ac := range []*manifest.Access{site.Access, site.More} {
				if d, ok := doorFor(ac, r.FormValue("zone")); ok {
					a.login(w, r, site, d)
					return
				}
			}
		case LogoutPath:
			a.clearSessions(w, r, site)
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		for _, d := range doors {
			if a.hasSession(r, site, d) {
				continue
			}
			if _, pw, ok := r.BasicAuth(); ok {
				good, wait := a.attempt(r, site, d, pw)
				if good {
					continue
				}
				if wait > 0 {
					a.tooMany(w, r, site, d, wait)
					return
				}
			}
			a.askPassword(w, r, site, d, false)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// lookup is the site a request is for, and for one with services, the
// service's. The sign-in page is the house's path on every name, so it is
// judged as the page it goes back to: on a name split by prefixes, the
// service behind /api is the one whose password the form was for.
func (a *Access) lookup(r *http.Request) (Site, bool) {
	if a.opts.LookupRequest == nil {
		return a.opts.Lookup(edge.Host(r))
	}
	p := r.URL.Path
	if p == LoginPath {
		p = "/"
		if next := pages.LocalPath(r.FormValue("next")); next != "" {
			p, _, _ = strings.Cut(next, "?")
		}
	}
	return a.opts.LookupRequest(r, p)
}

// doorFor finds the door a login form was posted for.
func doorFor(ac *manifest.Access, zone string) (door, bool) {
	if ac == nil {
		return door{}, false
	}
	if zone == "" {
		return door{hash: ac.Password, message: ac.Message}, ac.Password != ""
	}
	for prefix, ra := range ac.Routes {
		if ra != nil && ra.Password != "" && zoneOf(prefix) == zone {
			return door{zone: zone, hash: ra.Password, message: ra.Message}, true
		}
	}
	return door{}, false
}

// admitted applies one level's allow and deny lists.
func admitted(ip net.IP, allow, deny []string) bool {
	if len(allow) > 0 && !inAny(ip, allow) {
		return false
	}
	return len(deny) == 0 || !inAny(ip, deny)
}

// CleanPath is the path as the site behind will understand it, which is
// what a rule has to decide on. The raw path cannot be used: "/x/../admin",
// "//admin" and "/%2e%2e/admin" do not start with "/admin", but Next,
// Express and file servers clean them and serve /admin. V1 found it live on
// 1 October 2026: the zone answered 401 on its own path and 200 with the
// page on any of those. Case is folded too, because Express and others do
// not tell /ADMIN from /admin, and for a door covering too much is better
// than covering too little. Backslashes count as slashes and ";params" are
// dropped for the same reason: some servers read them that way.
func CleanPath(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	if i := strings.IndexByte(p, ';'); i >= 0 {
		segs := strings.Split(p, "/")
		for i, s := range segs {
			if j := strings.IndexByte(s, ';'); j >= 0 {
				segs[i] = s[:j]
			}
		}
		p = strings.Join(segs, "/")
	}
	return strings.ToLower(path.Clean("/" + p))
}

// matchRoute returns the zone with the longest prefix matching path, on
// segment boundaries: "/admin" matches "/admin" and "/admin/x", never
// "/administrator", which is the mistake plain HasPrefix would make. The
// path is expected already cleaned by CleanPath; prefixes are folded the
// same way so a route written "/Admin" still covers the request.
func matchRoute(routes map[string]*manifest.RouteAccess, path string) (string, *manifest.RouteAccess) {
	var best *manifest.RouteAccess
	bestZone := ""
	bestLen := -1
	for prefix, ra := range routes {
		if ra == nil {
			continue
		}
		z := zoneOf(prefix)
		p := strings.TrimSuffix(z, "/")
		if (p == "" || path == p || strings.HasPrefix(path, p+"/")) && len(p) > bestLen {
			best, bestZone, bestLen = ra, z, len(p)
		}
	}
	return bestZone, best
}

// zoneOf is a route prefix as the door compares it.
func zoneOf(prefix string) string {
	z := CleanPath(prefix)
	if z != "/" {
		z = strings.TrimSuffix(z, "/")
	}
	return z
}

func inAny(ip net.IP, cidrs []string) bool {
	if ip == nil {
		return false
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	for _, c := range cidrs {
		c = strings.TrimSpace(c)
		if p, err := netip.ParsePrefix(c); err == nil {
			if p.Contains(addr) {
				return true
			}
			continue
		}
		if a, err := netip.ParseAddr(c); err == nil && a.Unmap() == addr {
			return true
		}
	}
	return false
}

// attempt checks one password at one door. wait > 0 means it was not
// checked at all: the address is locked out of this door, or every
// derivation slot in the house stayed busy.
func (a *Access) attempt(r *http.Request, site Site, d door, plain string) (ok bool, wait time.Duration) {
	if d.hash == "" || plain == "" {
		return false, 0
	}
	k := sha256.Sum256([]byte(d.hash + "\x00" + plain))
	a.mu.Lock()
	_, hit := a.verified[k]
	a.mu.Unlock()
	if hit {
		return true, 0
	}
	who := a.guesser(r, site, d)
	if blocked, left := a.brake.Blocked(who); blocked {
		return false, left
	}
	ok, ran := brake.Derive(r.Context(), func() bool { return CheckPassword(d.hash, plain) })
	if !ran {
		return false, time.Second
	}
	if !ok {
		a.brake.Failed(who)
		return false, 0
	}
	a.brake.Passed(who)
	a.mu.Lock()
	if len(a.verified) > 256 {
		a.verified = map[[32]byte]struct{}{}
	}
	a.verified[k] = struct{}{}
	a.mu.Unlock()
	return true, 0
}

// guesser is who a wrong password is counted against: per door, so a
// lockout on one site or zone does not close another, and per address
// (a /64 for IPv6, see brake.Key).
func (a *Access) guesser(r *http.Request, site Site, d door) string {
	who := r.RemoteAddr
	if ip := a.clientIP(r); ip != nil {
		if addr, ok := netip.AddrFromSlice(ip); ok {
			who = brake.Key(addr)
		}
	}
	return site.Domain + "\x00" + d.zone + "\x00" + who
}

// Passes are signed with the on-disk key over the domain, the zone, the
// stored hash and the expiry, so changing a password ends every pass it
// gave without keeping a list of them.
func (a *Access) sign(site Site, d door, exp int64) string {
	mac := hmac.New(sha256.New, a.key)
	for _, part := range []string{site.Domain, d.zone, d.hash, strconv.FormatInt(exp, 10)} {
		mac.Write([]byte(part))
		mac.Write([]byte{0})
	}
	return strconv.FormatInt(exp, 10) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// cookieName is the pass's name for a door: the whole site's, or one per
// zone so two doors on a domain do not overwrite each other.
func cookieName(d door) string {
	if d.zone == "" {
		return sessionCookie
	}
	sum := sha256.Sum256([]byte(d.zone))
	return sessionCookie + "_" + base64.RawURLEncoding.EncodeToString(sum[:6])
}

func cookiePath(d door) string {
	if d.zone == "" {
		return "/"
	}
	return d.zone
}

func (a *Access) hasSession(r *http.Request, site Site, d door) bool {
	c, err := r.Cookie(cookieName(d))
	if err != nil {
		return false
	}
	expStr, _, ok := strings.Cut(c.Value, ".")
	if !ok {
		return false
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || exp < a.now().Unix() {
		return false
	}
	return hmac.Equal([]byte(a.sign(site, d, exp)), []byte(c.Value))
}

func (a *Access) setSession(w http.ResponseWriter, r *http.Request, site Site, d door) {
	exp := a.now().Add(sessionTTL).Unix()
	http.SetCookie(w, &http.Cookie{
		Name: cookieName(d), Value: a.sign(site, d, exp), Path: cookiePath(d),
		MaxAge: int(sessionTTL.Seconds()), HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: edge.HTTPS(r),
	})
}

// clearSessions drops the site's pass and the pass of every zone the
// site and its service have. It used to drop the zone passes the browser
// sent, with Path "/": a zone pass has its zone as its Path, so the
// browser never sends it to /__mw/logout, and a deletion with another
// Path is another cookie. Signing out left every zone open. The zones are
// read from the manifest instead, and each pass is expired where it
// lives.
func (a *Access) clearSessions(w http.ResponseWriter, r *http.Request, site Site) {
	expire := func(d door) {
		http.SetCookie(w, &http.Cookie{
			Name: cookieName(d), Value: "", Path: cookiePath(d), MaxAge: -1,
			HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: edge.HTTPS(r),
		})
	}
	expire(door{})
	seen := map[string]bool{"": true}
	for _, ac := range []*manifest.Access{site.Access, site.More} {
		if ac == nil {
			continue
		}
		for prefix, ra := range ac.Routes {
			if z := zoneOf(prefix); ra != nil && ra.Password != "" && !seen[z] {
				seen[z] = true
				expire(door{zone: z})
			}
		}
	}
}

func (a *Access) login(w http.ResponseWriter, r *http.Request, site Site, d door) {
	if r.Method != http.MethodPost {
		a.askPassword(w, r, site, d, false)
		return
	}
	if err := r.ParseForm(); err != nil {
		a.askPassword(w, r, site, d, true)
		return
	}
	good, wait := a.attempt(r, site, d, r.PostForm.Get("password"))
	if wait > 0 {
		a.tooMany(w, r, site, d, wait)
		return
	}
	if !good {
		a.askPassword(w, r, site, d, true)
		return
	}
	a.setSession(w, r, site, d)
	http.Redirect(w, r, pages.LocalPath(r.PostForm.Get("next")), http.StatusSeeOther)
}

// askPassword answers 401. Browsers get the form, inside the site's own
// design; anything that does not ask for HTML (curl, a deploy script) gets
// a Basic challenge instead, because the form would only get in its way.
func (a *Access) askPassword(w http.ResponseWriter, r *http.Request, site Site, d door, failed bool) {
	w.Header().Set("Cache-Control", "no-store")
	if !strings.Contains(r.Header.Get("Accept"), "text/html") {
		w.Header().Set("WWW-Authenticate", `Basic realm="site", charset="UTF-8"`)
		http.Error(w, "password required", http.StatusUnauthorized)
		return
	}
	next := r.URL.RequestURI()
	if r.URL.Path == LoginPath {
		next = r.FormValue("next")
	}
	n := pages.ForSite("password", site.Title, site.Domain, site.Theme)
	if d.message != "" {
		n.Line = d.message
	}
	var form strings.Builder
	_ = formTpl.Execute(&form, map[string]any{"Action": LoginPath, "Next": pages.LocalPath(next), "Zone": d.zone, "Failed": failed})
	n.Extra = template.HTML(form.String())
	pages.Render(w, r, n)
}

// tooMany answers 429 to an attempt the brake refused. A browser gets the
// form again, saying why and for how long, so the visitor is not left
// guessing whether the password or the site is wrong.
func (a *Access) tooMany(w http.ResponseWriter, r *http.Request, site Site, d door, wait time.Duration) {
	secs := int(wait.Round(time.Second).Seconds())
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Cache-Control", "no-store")
	if !strings.Contains(r.Header.Get("Accept"), "text/html") {
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		http.Error(w, "too many wrong passwords; try again later", http.StatusTooManyRequests)
		return
	}
	n := pages.ForSite("password", site.Title, site.Domain, site.Theme)
	n.Status, n.RetryAfter = http.StatusTooManyRequests, secs
	if d.message != "" {
		n.Line = d.message
	}
	next := r.URL.RequestURI()
	if r.URL.Path == LoginPath {
		next = r.FormValue("next")
	}
	minutes := (secs + 59) / 60
	var form strings.Builder
	_ = formTpl.Execute(&form, map[string]any{"Action": LoginPath, "Next": pages.LocalPath(next), "Zone": d.zone, "Minutes": minutes})
	n.Extra = template.HTML(form.String())
	pages.Render(w, r, n)
}

// formTpl is the password form every design styles (.password, .wrong).
var formTpl = template.Must(template.New("form").Parse(`<form class="password" method="post" action="{{.Action}}">
<input type="hidden" name="next" value="{{.Next}}"><input type="hidden" name="zone" value="{{.Zone}}">
<input type="password" name="password" autocomplete="current-password" autofocus required aria-label="Password" placeholder="Password">
<button type="submit">Enter</button>
{{if .Failed}}<p class="wrong">That password is not right.</p>{{end}}
{{with .Minutes}}<p class="wrong">Too many wrong passwords. Try again in {{if eq . 1}}a minute{{else}}{{.}} minutes{{end}}.</p>{{end}}
</form>`))

// Closed is the door when Access could not start (its key unreadable, the
// disk full). Every site that has an access rule answers 503 and the rest
// pass. Letting them through instead would serve every password-protected
// site open with nothing but an event saying so, which is the failure that
// looks exactly like success.
func Closed(lookup Lookup) func(http.Handler) http.Handler {
	if lookup == nil {
		return ClosedRequest(nil)
	}
	return ClosedRequest(func(r *http.Request, _ string) (Site, bool) { return lookup(edge.Host(r)) })
}

// ClosedRequest is Closed for a lookup by request (Options.LookupRequest),
// so a service's own password closes its names too.
func ClosedRequest(lookup func(r *http.Request, urlPath string) (Site, bool)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if lookup != nil {
				if site, ok := lookup(r, r.URL.Path); ok && (guarded(site.Access) || guarded(site.More)) {
					w.Header().Set("Cache-Control", "no-store")
					pages.Render(w, r, pages.ForSite("down", site.Title, site.Domain, site.Theme))
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

func guarded(ac *manifest.Access) bool {
	return ac != nil && (ac.Password != "" || len(ac.Allow) > 0 || len(ac.Deny) > 0 || len(ac.DenyCountries) > 0 || len(ac.Routes) > 0)
}
