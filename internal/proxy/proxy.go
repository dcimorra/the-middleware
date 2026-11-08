// Package proxy is the request path: Host → site → instance. It is deliberately
// small. Everything that can be decided outside the hot path (which site, which
// port, which mode) is read from an engine snapshot; everything that must be
// decided per request (access, rate, headers) is a middleware around this.
package proxy

import (
	"context"
	"errors"
	"html/template"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dcimorra/the-middleware/internal/fsx"

	"github.com/dcimorra/the-middleware/internal/edge"
	"github.com/dcimorra/the-middleware/internal/engine"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/pages"
)

// Lookup resolves a host to a site snapshot.
type Lookup func(host string) (engine.Snapshot, bool)

// PathLookup resolves a host and a path: which of a site's services
// answers it.
type PathLookup func(host, path string) (engine.Snapshot, bool)

// Handler routes by Host.
type Handler struct {
	lookup Lookup
	// ByPath, when set, is asked instead of lookup for the request
	// itself, with its path: a site with services sends /api of a name
	// to one service and the rest to another.
	ByPath PathLookup
	// Panel, when set, answers the panel domain and the loopback host.
	Panel     http.Handler
	panel     atomic.Pointer[string]
	transport http.RoundTripper
	// Unrouted records the names nobody serves; DoorOf and ClientIP say,
	// for its samples, the door a request came by and who sent it.
	Unrouted *Unrouted
	DoorOf   func(*http.Request) string
	ClientIP func(*http.Request) string
}

// SetPanelDomain changes the name the panel answers on, at once: the
// panel's Settings and `middleware setup --domain` change it while the
// process runs, and every piece that asks reads this one.
func (h *Handler) SetPanelDomain(d string) { h.panel.Store(&d) }

// PanelDomain is the name the panel answers on, or "".
func (h *Handler) PanelDomain() string {
	if p := h.panel.Load(); p != nil {
		return *p
	}
	return ""
}

// New builds the router.
func New(lookup Lookup) *Handler {
	return &Handler{lookup: lookup, transport: &http.Transport{
		// Loopback only: a dial that takes longer than this means the
		// instance is wedged, not slow.
		DialContext:           (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
		DisableCompression:    true, // what the instance sends, the client gets
	}}
}

// Host is edge.Host, the name every door reads.
func Host(r *http.Request) string { return edge.Host(r) }

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := Host(r)
	if h.Panel != nil && ((host != "" && host == h.PanelDomain()) || host == "localhost" || host == "127.0.0.1") {
		h.Panel.ServeHTTP(w, r)
		return
	}
	var sn engine.Snapshot
	var ok bool
	if h.ByPath != nil {
		sn, ok = h.ByPath(host, r.URL.Path)
	} else {
		sn, ok = h.lookup(host)
	}
	if ok && (sn.Mode == manifest.ModeProject || manifest.IsProject(sn.Domain)) {
		// A project has no hostname; a Host header that happens to spell
		// its folder's name is a stranger asking for something else.
		ok = false
	}
	if !ok {
		// www.example.com with only example.com configured, and the reverse:
		// a redirect is kinder than "nothing here" and costs nothing.
		if alt, ok := h.lookup(wwwAlternate(host)); ok && alt.Mode != manifest.ModeDisabled {
			u := *r.URL
			u.Scheme = scheme(r)
			u.Host = alt.Domain
			http.Redirect(w, r, u.String(), http.StatusMovedPermanently)
			return
		}
		h.noteUnrouted(host, r)
		pages.Write(w, r, "unknown", http.StatusNotFound)
		return
	}
	h.serveSite(w, r, sn)
}

func (h *Handler) serveSite(w http.ResponseWriter, r *http.Request, sn engine.Snapshot) {
	m := sn.Manifest
	// The site's redirects come first: an old address is sent on even
	// while the site is stopped or behind its holding page. One that would
	// send a request to itself is not followed (a loop, written by hand).
	if to, code, ok := m.RedirectFor(Host(r), r.URL.Path, r.URL.RawQuery); ok {
		if !strings.HasPrefix(to, "/") || strings.SplitN(to, "?", 2)[0] != r.URL.Path {
			http.Redirect(w, r, to, code)
			return
		}
	}
	// What the house answers itself carries the manifest's headers from
	// here; what a site answers gets them in forward, over its own.
	if !forwards(sn) {
		setHeaders(w.Header(), m)
	}
	switch sn.Mode {
	case manifest.ModeDisabled:
		pages.Render(w, r, SiteNotice(sn, "stopped"))
	case manifest.ModeHold:
		pages.Render(w, r, holdNotice(sn))
	case manifest.ModeRun:
		if !sn.Alive || sn.Port == 0 {
			// Starting is worth a reload in five seconds; a site that
			// crashed and is backing off is not, and the page says which.
			key := "starting"
			if sn.Error != "" && !sn.Handing {
				key = "down"
			}
			pages.Render(w, r, SiteNotice(sn, key))
			return
		}
		h.forward(w, r, sn, "http://127.0.0.1:"+strconv.Itoa(sn.Port))
	case manifest.ModeUpstream:
		h.forward(w, r, sn, m.Upstream)
	default:
		h.static(w, r, sn)
	}
}

// forwards says whether serveSite hands the request to a process.
func forwards(sn engine.Snapshot) bool {
	switch sn.Mode {
	case manifest.ModeRun:
		return sn.Alive && sn.Port != 0
	case manifest.ModeUpstream:
		return true
	}
	return false
}

func setHeaders(h http.Header, m *manifest.Manifest) {
	if m == nil {
		return
	}
	for k, v := range m.Headers {
		h.Set(k, v)
	}
}

// overSite puts the manifest's headers over a site's answer. A manifest's
// Cache-Control replaces the site's, except for what the site keeps to one
// person or to nobody (private, no-store, no-cache), which stays: a line
// written for the public pages, "public, max-age=60", would otherwise make
// the signed-in ones public, for the house's cache and the edge's alike.
func overSite(h http.Header, m *manifest.Manifest) {
	if m == nil {
		return
	}
	var keep []string
	for k := range m.Headers {
		if http.CanonicalHeaderKey(k) != "Cache-Control" {
			continue
		}
		for _, v := range h.Values("Cache-Control") {
			for _, d := range strings.Split(v, ",") {
				d = strings.TrimSpace(d)
				name, _, _ := strings.Cut(d, "=")
				switch strings.ToLower(strings.TrimSpace(name)) {
				case "private", "no-store", "no-cache":
					keep = append(keep, d)
				}
			}
		}
		break
	}
	setHeaders(h, m)
	if len(keep) > 0 {
		h.Set("Cache-Control", strings.Join(append([]string{h.Get("Cache-Control")}, keep...), ", "))
	}
}

// forward proxies to target. httputil.ReverseProxy handles WebSocket
// upgrades by hijacking both ends, so a long-lived socket keeps flowing;
// what it cannot survive is this process restarting, which is documented.
func (h *Handler) forward(w http.ResponseWriter, r *http.Request, sn engine.Snapshot, target string) {
	u, err := url.Parse(target)
	if err != nil {
		setHeaders(w.Header(), sn.Manifest)
		pages.Render(w, r, SiteNotice(sn, "down"))
		return
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(u)
			pr.Out.Host = pr.In.Host
			// What a site reads as "the client" is what the house acted
			// on, never what the client wrote: at the 80/443 door a forged
			// CF-Connecting-IP used to reach the site as X-Forwarded-For.
			edge.Scrub(pr.In, pr.Out.Header)
			pr.SetXForwarded()
			if ip := edge.ClientIP(pr.In); ip != nil {
				pr.Out.Header.Set("X-Forwarded-For", ip.String())
			}
			pr.Out.Header.Set("X-Forwarded-Proto", scheme(pr.In))
		},
		Transport:     h.transport,
		FlushInterval: -1, // streams (SSE, chunked) flush as they come
		// The manifest's headers replace the site's own. Set on the writer
		// before the proxy ran, they were sent next to the site's (the
		// proxy adds what the site answers): two Access-Control-Allow-Origin
		// break CORS, and the cache read the manifest's Cache-Control and
		// kept a page the site had marked private.
		ModifyResponse: func(resp *http.Response) error {
			overSite(resp.Header, sn.Manifest)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				return
			}
			setHeaders(w.Header(), sn.Manifest)
			// 503 and not 502: Cloudflare replaces a 502 from the origin
			// with its own page, and the visitor never sees this one.
			pages.Render(w, r, SiteNotice(sn, "down"))
		},
	}
	rp.ServeHTTP(w, r)
}

// static serves files from the site folder. Paths are cleaned and rooted so
// "../" cannot leave the folder, and dotfiles (.git, .env) are never served.
func (h *Handler) static(w http.ResponseWriter, r *http.Request, sn engine.Snapshot) {
	root := sn.Dir
	spa := false
	if sn.Manifest != nil && sn.Manifest.Static != nil {
		if sn.Manifest.Static.Root != "" {
			root = filepath.Join(sn.Dir, sn.Manifest.Static.Root)
		}
		spa = sn.Manifest.Static.SPA
	} else if dirExists(filepath.Join(sn.Dir, "public")) {
		root = filepath.Join(sn.Dir, "public")
	}
	clean := path.Clean("/" + r.URL.Path)
	for _, seg := range strings.Split(clean, "/") {
		if strings.HasPrefix(seg, ".") && seg != "." && seg != "" {
			http.NotFound(w, r)
			return
		}
	}
	if clean == "/site.json" {
		http.NotFound(w, r)
		return
	}
	full := filepath.Join(root, filepath.FromSlash(clean))
	if !insideSite(sn.Dir, full) {
		http.NotFound(w, r)
		return
	}
	st, err := os.Stat(full)
	if err == nil && st.IsDir() {
		if !strings.HasSuffix(r.URL.Path, "/") {
			http.Redirect(w, r, clean+"/", http.StatusMovedPermanently)
			return
		}
		full = filepath.Join(full, "index.html")
		st, err = os.Stat(full)
	}
	if err != nil {
		if spa {
			full = filepath.Join(root, "index.html")
			if _, err := os.Stat(full); err == nil {
				serveFile(w, r, full, sn)
				return
			}
		}
		// A site with a 404.html gets it; the rest get the plain one.
		if custom := filepath.Join(root, "404.html"); fsx.IsFile(custom) {
			w.WriteHeader(http.StatusNotFound)
			http.ServeFile(w, r, custom)
			return
		}
		http.NotFound(w, r)
		return
	}
	serveFile(w, r, full, sn)
}

func serveFile(w http.ResponseWriter, r *http.Request, full string, sn engine.Snapshot) {
	if sn.Manifest != nil && sn.Manifest.Cache != nil && sn.Manifest.Cache.StaticMaxAge > 0 {
		w.Header().Set("Cache-Control", "public, max-age="+strconv.Itoa(int(time.Duration(sn.Manifest.Cache.StaticMaxAge).Seconds())))
	}
	http.ServeFile(w, r, full)
}

func wwwAlternate(host string) string {
	if strings.HasPrefix(host, "www.") {
		return host[4:]
	}
	return "www." + host
}

func scheme(r *http.Request) string {
	if edge.HTTPS(r) {
		return "https"
	}
	return "http"
}

// insideSite reports whether full, with its links resolved, is still in the
// site folder. The site's agent can write a link in public/ to ~/.ssh as
// easily as an index.html, and a lexical check would serve what it points
// at. A path that does not exist yet is judged by its nearest existing
// parent, so a 404 stays a 404.
func insideSite(siteDir, full string) bool {
	base, err := filepath.EvalSymlinks(siteDir)
	if err != nil {
		return false
	}
	p := full
	for {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			rel, err := filepath.Rel(base, real)
			return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
		}
		parent := filepath.Dir(p)
		if parent == p {
			return false
		}
		p = parent
	}
}

func dirExists(p string) bool { st, err := os.Stat(p); return err == nil && st.IsDir() }

// SiteNotice is one of a site's pages: its name as the title, its domain
// at the foot, its design.
func SiteNotice(sn engine.Snapshot, key string) pages.Notice {
	title, theme := "", ""
	if m := sn.Manifest; m != nil {
		title = m.Title
		if m.Notice != nil {
			theme = m.Notice.Theme
		}
	}
	return pages.ForSite(key, title, sn.Domain, theme)
}

// holdNotice is the page a held site shows: its mode, its own message if
// it wrote one, and when it is due back.
func holdNotice(sn engine.Snapshot) pages.Notice {
	hold := sn.Manifest.Hold
	key := "hold"
	if hold != nil && hold.Mode != "" && hold.Mode != "maintenance" {
		key = "hold-" + hold.Mode
	}
	n := SiteNotice(sn, key)
	if hold == nil {
		return n
	}
	if hold.Title != "" {
		n.Title = hold.Title
	}
	if hold.Message != "" {
		n.Line = hold.Message
	}
	if until, err := time.Parse(time.RFC3339, hold.Until); err == nil && until.After(time.Now()) {
		n.RetryAfter = int(time.Until(until).Seconds()) + 1
		n.Extra = template.HTML(`<p class="when">Back <time datetime="` + until.UTC().Format(time.RFC3339) + `">` +
			template.HTMLEscapeString(until.UTC().Format("Mon 2 Jan, 15:04 UTC")) + `</time></p>`)
	}
	return n
}

func (h *Handler) noteUnrouted(host string, r *http.Request) {
	if h.Unrouted == nil {
		return
	}
	door, ip := "", r.RemoteAddr
	if h.DoorOf != nil {
		door = h.DoorOf(r)
	}
	if h.ClientIP != nil {
		ip = h.ClientIP(r)
	}
	h.Unrouted.Note(host, door, ip, r)
}
