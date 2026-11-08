package attack

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"html/template"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dcimorra/the-middleware/internal/edge"
	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/pages"
)

const (
	// passCookie is the solved pass.
	passCookie = "mw_attack"
	// ChallengePath is where a solution is posted. It is under the prefix
	// no site uses, like the password form: the request stays here and
	// never reaches the process behind.
	ChallengePath = "/__mw/challenge"
	// challengeLife is how long an issued challenge may be solved in.
	// Short on purpose: it bounds how long a solved one can be kept
	// before being handed around.
	challengeLife = 5 * time.Minute
)

// Site is what the guard needs to know about the site a request is for.
type Site struct {
	Domain       string
	Rules        *Rules
	Title, Theme string
}

// Options configure a Guard.
type Options struct {
	// DataDir is data/; the key lives at data/attack/key.
	DataDir string
	Lookup  func(host string) (Site, bool)
	// LookupRequest, when set, resolves the request instead of its host:
	// a host and a path name one of a site's services.
	LookupRequest func(r *http.Request) (Site, bool)
	// Host is the site name a request is for (proxy.Host).
	Host func(r *http.Request) string
	// Global is the installation-wide rules, which apply to every site
	// that has none switched on and to the panel.
	Global func() *Rules
	// Panel says whether a host is the panel's.
	Panel func(host string) bool
	// AllowAgents are lower-cased User-Agent fragments let through (the
	// WAF policy's allow_agents: crawlers the operator wants indexing even
	// on a bad day). Spoofable, and the operator's choice.
	AllowAgents func() []string
	// ClientIP is the edge's; nil uses RemoteAddr.
	ClientIP func(r *http.Request) net.IP
	Now      func() time.Time
}

// Counter is what one site's challenge has done since the start. Two
// numbers, not one: served alone cannot tell "this is stopping a flood"
// from "this is keeping everyone out", which is exactly what needs
// looking at on the day it is switched on.
type Counter struct {
	Served int64 `json:"served"`
	Solved int64 `json:"solved"`
}

type counter struct{ served, solved atomic.Int64 }

// Guard is the hook.
type Guard struct {
	opts   Options
	key    []byte
	mu     sync.Mutex
	counts map[string]*counter
}

// PanelName is the key the panel's counter is kept under.
const PanelName = "panel"

// New loads or creates the key. It is on disk, unlike V2's first
// challenge key, because a restart must not invalidate every pass.
func New(opts Options) (*Guard, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Global == nil {
		opts.Global = func() *Rules { return nil }
	}
	if opts.Host == nil {
		opts.Host = func(r *http.Request) string {
			h := r.Host
			if host, _, err := net.SplitHostPort(h); err == nil {
				h = host
			}
			return strings.ToLower(h)
		}
	}
	if opts.Panel == nil {
		opts.Panel = func(string) bool { return false }
	}
	keyPath := filepath.Join(opts.DataDir, "attack", "key")
	key, err := os.ReadFile(keyPath)
	if err != nil {
		if !fsx.IsNotExist(err) {
			return nil, err
		}
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := fsx.WriteAtomic(keyPath, key, 0o600); err != nil {
			return nil, err
		}
	}
	if len(key) < 16 {
		return nil, os.ErrInvalid
	}
	return &Guard{opts: opts, key: key, counts: map[string]*counter{}}, nil
}

// Counts is every counter, by domain (and PanelName).
func (g *Guard) Counts() map[string]Counter {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]Counter, len(g.counts))
	for k, c := range g.counts {
		out[k] = Counter{Served: c.served.Load(), Solved: c.solved.Load()}
	}
	return out
}

func (g *Guard) counter(name string) *counter {
	g.mu.Lock()
	defer g.mu.Unlock()
	c := g.counts[name]
	if c == nil {
		c = &counter{}
		g.counts[name] = c
	}
	return c
}

// panelExempt are the panel's requests that never get the challenge, and
// they are written here, not in a skip list someone can empty on a bad
// day. A request carrying credentials of its own (a bearer token, the
// site door's token) is a program: none can solve a challenge, and none
// costs a key derivation to check, so there is no flood to make dearer
// there and a great deal to break. The login, which does cost one, is
// challenged.
func panelExempt(r *http.Request) bool {
	p := r.URL.Path
	// The login is never exempt, whatever it carries: the API ignores an
	// Authorization header on it, so "Authorization: x" on a password
	// guess was a way around the challenge straight to the key derivation.
	if p == "/api/v1/auth/login" {
		return false
	}
	if r.Header.Get("Authorization") != "" {
		return true
	}
	return p == "/api/v1/site" || strings.HasPrefix(p, "/api/v1/site/")
}

// scope is the rules and the name a request is judged under, or ok false.
func (g *Guard) scope(r *http.Request) (rules *Rules, name, title, theme string, ok bool) {
	now := g.opts.Now()
	host := g.opts.Host(r)
	if g.opts.Panel(host) {
		if gl := g.opts.Global(); gl.Active(now) && !panelExempt(r) {
			return gl, PanelName, "", "", true
		}
		return nil, "", "", "", false
	}
	var site Site
	var found bool
	if g.opts.LookupRequest != nil {
		site, found = g.opts.LookupRequest(r)
	} else {
		site, found = g.opts.Lookup(host)
	}
	if !found {
		return nil, "", "", "", false
	}
	rules = site.Rules
	if !rules.Active(now) {
		gl := g.opts.Global()
		if !gl.Active(now) {
			return nil, "", "", "", false
		}
		// The global switch with the site's own exemptions: a site that
		// wrote its webhook into skip does not lose it because the whole
		// house went under attack.
		merged := *gl
		if site.Rules != nil {
			merged.Skip = append(append([]string(nil), gl.Skip...), site.Rules.Skip...)
		}
		rules = &merged
	}
	return rules, site.Domain, site.Title, site.Theme, true
}

// Wrap implements the hook.
func (g *Guard) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rules, name, title, theme, ok := g.scope(r)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == ChallengePath {
			g.solve(w, r, rules, name, title, theme)
			return
		}
		// Exempt paths come before the pass: a payment gateway carries no
		// cookie and need not.
		if rules.Skips(cleanPath(r.URL.Path)) || g.allowedAgent(r) || g.passes(r, name) {
			next.ServeHTTP(w, r)
			return
		}
		g.ask(w, r, rules, name, title, theme, false)
	})
}

func (g *Guard) allowedAgent(r *http.Request) bool {
	if g.opts.AllowAgents == nil {
		return false
	}
	ua := strings.ToLower(r.UserAgent())
	for _, s := range g.opts.AllowAgents() {
		if s != "" && strings.Contains(ua, s) {
			return true
		}
	}
	return false
}

func cleanPath(p string) string {
	c := path.Clean("/" + p)
	return strings.ToLower(c)
}

// client is who the pass is handed to: the address and the User-Agent,
// summed with the key. Not an identity (the User-Agent is the caller's to
// write, an address is shared behind NAT), and it need not be one: it is
// what stops a pass solved once from being handed to a thousand machines,
// which is the cheap way around this. Changing networks costs another
// fraction of a second, and that is all.
func (g *Guard) client(r *http.Request) string {
	ip := ""
	if g.opts.ClientIP != nil {
		if v := g.opts.ClientIP(r); v != nil {
			ip = v.String()
		}
	} else if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		ip = h
	}
	return g.sign("client\x00" + ip + "\x00" + r.Header.Get("User-Agent"))[:16]
}

func (g *Guard) sign(what string) string {
	m := hmac.New(sha256.New, g.key)
	m.Write([]byte(what))
	return hex.EncodeToString(m.Sum(nil))
}

func (g *Guard) passValue(r *http.Request, name, exp string) string {
	return exp + "." + g.sign("pass\x00"+name+"\x00"+g.client(r)+"\x00"+exp)
}

func (g *Guard) passes(r *http.Request, name string) bool {
	c, err := r.Cookie(passCookie)
	if err != nil {
		return false
	}
	exp, _, ok := strings.Cut(c.Value, ".")
	if !ok {
		return false
	}
	n, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || g.opts.Now().Unix() >= n {
		return false
	}
	return hmac.Equal([]byte(c.Value), []byte(g.passValue(r, name, exp)))
}

// challenge issues one, signed with its instant inside, so nothing has to
// be remembered: what comes back is checked with the key. The bits are in
// the signature, so lowering the difficulty does not validate a challenge
// issued when it was high, nor the other way round.
func (g *Guard) challenge(r *http.Request, name string, bits int) string {
	var seed [8]byte
	_, _ = rand.Read(seed[:])
	head := strconv.FormatInt(g.opts.Now().Unix(), 10) + "." + hex.EncodeToString(seed[:])
	return head + "." + g.sign(g.body(r, name, bits, head))
}

func (g *Guard) body(r *http.Request, name string, bits int, head string) string {
	return "challenge\x00" + name + "\x00" + g.client(r) + "\x00" + strconv.Itoa(bits) + "\x00" + head
}

func (g *Guard) valid(r *http.Request, name string, bits int, token string) bool {
	i := strings.LastIndexByte(token, '.')
	if i < 0 {
		return false
	}
	head, sig := token[:i], token[i+1:]
	if !hmac.Equal([]byte(sig), []byte(g.sign(g.body(r, name, bits, head)))) {
		return false
	}
	when, _, _ := strings.Cut(head, ".")
	n, err := strconv.ParseInt(when, 10, 64)
	if err != nil {
		return false
	}
	age := g.opts.Now().Sub(time.Unix(n, 0))
	return age >= 0 && age < challengeLife
}

// numeric keeps both ends hashing the same bytes. The browser hashes
// one-byte characters and Go hashes UTF-8; while both sides speak digits
// and hex they agree, and with anything else in there they would stop
// agreeing silently. The challenge is ASCII by construction; this makes
// sure of the other half.
func numeric(s string) bool {
	if s == "" || len(s) > 20 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// Solved says whether nonce solves token at bits: the first bits of
// SHA-256(token "|" nonce) are zero. Checking is one hash; finding is
// 2^bits on average, and that asymmetry is the whole point.
func Solved(token, nonce string, bits int) bool {
	sum := sha256.Sum256([]byte(token + "|" + nonce))
	return binary.BigEndian.Uint32(sum[:4])>>(32-bits) == 0
}

func (g *Guard) solve(w http.ResponseWriter, r *http.Request, rules *Rules, name, title, theme string) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	_ = r.ParseForm()
	bits := rules.Difficulty()
	token, nonce := r.PostFormValue("challenge"), r.PostFormValue("nonce")
	if !numeric(nonce) || !g.valid(r, name, bits, token) || !Solved(token, nonce, bits) {
		// Another challenge, no wait: there is nothing to guess here, the
		// challenge itself sets the price, and whoever brings an expired
		// one is nearly always someone who left the tab open.
		g.ask(w, r, rules, name, title, theme, true)
		return
	}
	life := rules.PassLife()
	exp := strconv.FormatInt(g.opts.Now().Add(life).Unix(), 10)
	http.SetCookie(w, &http.Cookie{
		Name: passCookie, Value: g.passValue(r, name, exp), Path: "/",
		MaxAge: int(life.Seconds()), HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: edge.HTTPS(r),
	})
	g.counter(name).solved.Add(1)
	http.Redirect(w, r, pages.LocalPath(r.PostFormValue("back")), http.StatusSeeOther)
}

// csp is the challenge page's own policy. The page carries a script, and
// the panel sends `script-src 'self'` on everything it answers; V1 found
// that the panel's challenge then never ran, the page came back, solved
// nothing, came back, and nothing anywhere said so. The page declares
// what it runs, because it is where that is known.
const csp = "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; img-src data:; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

func (g *Guard) ask(w http.ResponseWriter, r *http.Request, rules *Rules, name, title, theme string, again bool) {
	g.counter(name).served.Add(1)
	// Says this is a challenge and not a site that is down: from outside
	// both are a 503 with a page, and a program (the panel asking for its
	// own state) needs to tell them apart.
	w.Header().Set("X-Middleware-Challenge", "1")
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("Cache-Control", "no-store")
	back := r.URL.RequestURI()
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		// A POST cannot be resumed after the challenge: its body is gone.
		// Back to the front page, as the edge does; worth knowing before
		// switching this on in front of a shop's cart.
		back = "/"
	}
	foot := name
	if name == PanelName {
		foot = g.opts.Host(r)
	}
	n := pages.ForSite("challenge", title, foot, theme)
	if m := strings.TrimSpace(rules.Message); m != "" {
		n.Line = m
	}
	bits := rules.Difficulty()
	var form strings.Builder
	_ = formTpl.Execute(&form, map[string]any{
		"Action": ChallengePath, "Back": pages.LocalPath(back), "Token": g.challenge(r, name, bits), "Bits": bits,
		"Again": again, "Text": pages.ChallengeText, "Script": template.JS(script),
	})
	n.Extra = template.HTML(form.String())
	pages.Render(w, r, n)
}

// formTpl carries the data in attributes, not inside the script, so each
// value is escaped once, as HTML, and the script reads it with
// getAttribute. The words come from the pages catalogue.
var formTpl = template.Must(template.New("challenge").Parse(`<form class="password" id="mw-challenge" method="post" action="{{.Action}}" data-challenge="{{.Token}}" data-bits="{{.Bits}}" data-nocookie="{{.Text.nocookie}}" data-done="{{.Text.done}}">
<input type="hidden" name="back" value="{{.Back}}"><input type="hidden" name="challenge" value="{{.Token}}"><input type="hidden" name="nonce" id="mw-nonce" value="">
<p class="waiting" id="mw-state">{{.Text.working}} <span id="mw-pct">0%</span></p>
{{if .Again}}<p class="wrong">{{.Text.again}}</p>{{end}}
</form>
<noscript><p class="wrong">{{.Text.noscript}}</p></noscript>
<script>{{.Script}}</script>`))
