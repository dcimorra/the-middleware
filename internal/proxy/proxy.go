// Package proxy is the request path: Host → site → instance. It is deliberately
// small. Everything that can be decided outside the hot path (which site, which
// port, which mode) is read from an engine snapshot; everything that must be
// decided per request (access, rate, headers) is a middleware around this.
package proxy

import (
	"bytes"
	"context"
	"errors"
	"html/template"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dcimorra/the-middleware/internal/edge"
	"github.com/dcimorra/the-middleware/internal/engine"
	"github.com/dcimorra/the-middleware/internal/gz"
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
	// NoteCanary counts an answer of a site in canary, on its side
	// (canary.go). Nil counts nothing, and a canary then takes over at
	// the end of its time without being judged.
	NoteCanary func(domain string, canary bool, status int)
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
		if sn.Canary != nil {
			h.forwardCanary(w, r, sn)
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
		BufferPool:    copyBuffers,
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

// copyBuffers lends the proxy the buffer an answer is copied through.
// Without a pool each forwarded answer allocated its own 32 KiB, more than
// half of everything a proxied request allocated (BenchmarkChain).
var copyBuffers = &bufferPool{}

type bufferPool struct{ p sync.Pool }

func (b *bufferPool) Get() []byte {
	if v, ok := b.p.Get().(*[]byte); ok {
		return *v
	}
	return make([]byte, 32<<10)
}

func (b *bufferPool) Put(buf []byte) { b.p.Put(&buf) }

// static serves files from the site folder. Paths are cleaned and rooted so
// "../" cannot leave the folder, and dotfiles (.git, .env) are never served.
func (h *Handler) static(w http.ResponseWriter, r *http.Request, sn engine.Snapshot) {
	root := ""
	spa := false
	if sn.Manifest != nil && sn.Manifest.Static != nil {
		root = sn.Manifest.Static.Root
		spa = sn.Manifest.Static.SPA
	} else if dirExists(filepath.Join(sn.Dir, "public")) {
		root = "public"
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
	site, err := openSite(sn.Dir)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer site.Close()
	// A path in the site folder, never rooted: "/" for a site with no
	// public/ would be the machine's root, which the os.Root refuses.
	name := filepath.Join(".", root, filepath.FromSlash(clean))
	f, st, err := site.open(name)
	if err == nil && st.IsDir() {
		f.Close()
		if !strings.HasSuffix(r.URL.Path, "/") {
			http.Redirect(w, r, clean+"/", http.StatusMovedPermanently)
			return
		}
		name = filepath.Join(name, "index.html")
		f, st, err = site.openFile(name)
	}
	if err == nil {
		defer f.Close()
		serveFile(w, r, sn, name, f, st)
		return
	}
	if errors.Is(err, errOutside) {
		// A link out of the site is not there, and neither the SPA's shell
		// nor the site's own 404 page is owed to whoever asked for it.
		http.NotFound(w, r)
		return
	}
	if spa {
		name = filepath.Join(root, "index.html")
		if f, st, err := site.openFile(name); err == nil {
			defer f.Close()
			serveFile(w, r, sn, name, f, st)
			return
		}
	}
	// A site with a 404.html gets it; the rest get the plain one.
	if f, st, err := site.openFile(filepath.Join(root, "404.html")); err == nil {
		defer f.Close()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
		w.WriteHeader(http.StatusNotFound)
		if r.Method != http.MethodHead {
			_, _ = io.Copy(w, f)
		}
		return
	}
	http.NotFound(w, r)
}

// serveFile answers with an open file of the site. name is its path in
// the site folder, the one asked for and not where a link led: the type
// is the one the address promises.
func serveFile(w http.ResponseWriter, r *http.Request, sn engine.Snapshot, name string, f *os.File, st os.FileInfo) {
	base := filepath.Base(name)
	// One address per page, as http.ServeFile did: /docs/index.html and
	// /docs/ are otherwise two pages to a cache and to a search engine.
	if strings.HasSuffix(r.URL.Path, "/index.html") {
		http.Redirect(w, r, "./", http.StatusMovedPermanently)
		return
	}
	hd := w.Header()
	if sn.Manifest != nil && sn.Manifest.Cache != nil && sn.Manifest.Cache.StaticMaxAge > 0 {
		hd.Set("Cache-Control", "public, max-age="+strconv.Itoa(int(time.Duration(sn.Manifest.Cache.StaticMaxAge).Seconds())))
	} else if hd.Get("Cache-Control") == "" && path.Base(r.URL.Path) == base && Fingerprinted(base) {
		hd.Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	ct := mime.TypeByExtension(filepath.Ext(base))
	if ct == "" || !gz.Compressible(ct) {
		http.ServeContent(w, r, base, st.ModTime(), f)
		return
	}
	hd.Set("Content-Type", ct)
	hd.Add("Vary", "Accept-Encoding")
	// A range is of the file as it is on disk: whoever asks for one gets
	// the file as it is. So does a file too large to hold in memory twice.
	if st.Size() < gz.MinSize || st.Size() > maxGzip || r.Header.Get("Range") != "" || !gz.Accepts(r) {
		http.ServeContent(w, r, base, st.ModTime(), f)
		return
	}
	// The file's own identity is the key: a file written again has
	// another size or another time and is compressed again, and the old
	// entry falls out of the cache by itself.
	key := sn.Dir + "\x00" + name + "\x00" + strconv.FormatInt(st.Size(), 36) + "\x00" + strconv.FormatInt(st.ModTime().UnixNano(), 36)
	packed, ok := staticGzip.Get(key)
	if !ok {
		raw, err := io.ReadAll(io.LimitReader(f, st.Size()+1))
		if err != nil || int64(len(raw)) != st.Size() {
			// Being written as it is read: sent as it comes, nothing kept.
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				http.Error(w, "cannot read the file", http.StatusInternalServerError)
				return
			}
			http.ServeContent(w, r, base, st.ModTime(), f)
			return
		}
		packed = gz.Smaller(raw)
		staticGzip.Put(key, packed)
		if packed == nil {
			http.ServeContent(w, r, base, st.ModTime(), bytes.NewReader(raw))
			return
		}
	}
	if packed == nil {
		http.ServeContent(w, r, base, st.ModTime(), f)
		return
	}
	hd.Set("Content-Encoding", "gzip")
	// ServeContent leaves the length out of an encoded answer; without it
	// the answer goes out in chunks and a client cannot show progress.
	hd.Set("Content-Length", strconv.Itoa(len(packed)))
	http.ServeContent(w, r, base, st.ModTime(), bytes.NewReader(packed))
}

// maxGzip is the largest static file compressed, and staticGzip holds the
// compressed files of every site, 48 MiB at most: a stylesheet is
// compressed once per change, not once per visit.
const maxGzip = 4 << 20

var staticGzip = gz.NewCache(48 << 20)

// Fingerprinted says whether a script, a stylesheet or a font carries a
// hash of its content in its name, the way bundlers write their output:
// main.8f3c2a1b.js (webpack), index-Bx2Yz9Qa.css (Vite, Astro),
// chunk-ABCD1234.js (esbuild), framework-2d2a8c1e4f3b5a6c.js (Next). The
// name changes when the file does, so a browser and the edge may keep it
// for good; without this a returning visitor asked again for every script
// of every page.
//
// The hash is what stands between the last dot or dash and the
// extension: eight characters or more, letters and digits with at least
// one of each. jquery-3.7.1.min.js, app.bundle.js and logo-2024.css are
// not. Only what a bundler names: a page or a document kept for a year
// could not be corrected, and report-2024q3a1.pdf is a name a person
// writes.
func Fingerprinted(name string) bool {
	ext := path.Ext(name)
	switch strings.ToLower(ext) {
	case ".js", ".mjs", ".css", ".woff", ".woff2":
	default:
		return false
	}
	stem := strings.TrimSuffix(name, ext)
	i := strings.LastIndexAny(stem, ".-")
	if i <= 0 {
		return false
	}
	hash := stem[i+1:]
	if len(hash) < 8 || len(hash) > 64 {
		return false
	}
	digit, letter := false, false
	for _, c := range hash {
		switch {
		case c >= '0' && c <= '9':
			digit = true
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
			letter = true
		case c == '_':
		default:
			return false
		}
	}
	return digit && letter
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

// errOutside is a path that, its links followed, leaves the site folder.
var errOutside = errors.New("outside the site")

// siteRoot opens files of one site and nothing outside it. The site's
// agent can write a link in public/ to ~/.ssh as easily as an index.html,
// so the file opened is the file judged: the path asked for used to be
// resolved and checked, and then its folder's index.html, the SPA's shell
// or the 404 page were served unchecked, wherever they pointed.
//
// The walk goes through an os.Root, where the kernel refuses a link out
// as it meets it and a link swapped in after a check is not followed.
// Resolving the links of every path beforehand was also a third of what a
// static request cost (BenchmarkChain).
type siteRoot struct {
	dir  string
	root *os.Root
	// resolved counts the opens that had to resolve links by hand, for
	// the test that keeps the common paths off that slow road.
	resolved int
}

func openSite(dir string) (*siteRoot, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &siteRoot{dir: dir, root: root}, nil
}

func (s *siteRoot) Close() { _ = s.root.Close() }

// open opens name, a path in the site folder. What is not there is
// os.ErrNotExist; what leads out of the site is errOutside.
func (s *siteRoot) open(name string) (*os.File, os.FileInfo, error) {
	f, err := s.root.Open(name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, err
		}
		s.resolved++
		// os.Root refuses every absolute link, also one that stays in
		// the site (public -> /srv/middleware/sites/x/dist, which sites
		// have). Only then are the links resolved by hand, and what they
		// lead to opened through the root again.
		real, rerr := s.resolve(name)
		if rerr != nil {
			return nil, nil, rerr
		}
		if f, err = s.root.Open(real); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, nil, err
			}
			return nil, nil, errOutside
		}
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, st, nil
}

// openFile is open for what must be a file: a folder is not there.
func (s *siteRoot) openFile(name string) (*os.File, os.FileInfo, error) {
	f, st, err := s.open(name)
	if err == nil && st.IsDir() {
		f.Close()
		return nil, nil, os.ErrNotExist
	}
	return f, st, err
}

// resolve follows the links of name and gives where it really is, as a
// path in the site folder.
func (s *siteRoot) resolve(name string) (string, error) {
	base, err := filepath.EvalSymlinks(s.dir)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(filepath.Join(s.dir, name))
	if err != nil {
		// A link to nothing, a path through a file: not there. A link
		// out resolves, and is judged below.
		return "", os.ErrNotExist
	}
	rel, err := filepath.Rel(base, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errOutside
	}
	return rel, nil
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
