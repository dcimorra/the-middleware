// Package panel is the operator console: static HTML, CSS and JavaScript
// embedded in the binary and served as a single-page app. It has no build
// step on purpose: what is in ui/ is what the browser gets, so a fix is a
// commit and a restart, and a contributor needs Go and nothing else.
//
// The panel talks to /api/v1 (docs/api.md) and nothing here knows about
// sites, manifests or the engine; it is a file server with the headers a
// console on a public domain must send.
package panel

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"

	"github.com/dcimorra/the-middleware/internal/gz"
)

//go:embed ui
var ui embed.FS

// AssetPrefix is where the embedded files answer. Everything else is the
// app shell, so a deep link (/sites/example.com) that someone pastes into a
// browser lands on index.html and the hash router does the rest.
const AssetPrefix = "/assets/"

// CSP is the policy every panel response carries. 'self' only, no inline:
// the whole UI is written so nothing needs 'unsafe-inline', and keeping it
// that way is what makes a stored string from a site name or a log line
// unable to run as script. connect-src covers fetch and EventSource; data:
// in img-src is for icons drawn as data URIs.
const CSP = "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'"

// FS is the embedded ui/ directory, exported for tests and for anyone who
// needs to read the catalog or the shell without serving it.
var FS fs.FS

func init() {
	sub, err := fs.Sub(ui, "ui")
	if err != nil {
		panic(err) // the directory is part of the binary; this cannot happen
	}
	FS = sub
}

// Handler serves the panel. GET and HEAD only. index.html answers every
// path outside AssetPrefix with Cache-Control: no-cache; assets answer with
// an ETag derived from their content, so a browser revalidates for free
// and never keeps a stale app.js across an upgrade. There is no build step
// that could put a hash in the file name, so the hash goes in the ETag.
//
// Text goes out compressed to whoever takes it. The panel is ninety files
// and a megabyte and a half with no build step to shrink them, and through
// the 80/443 door or the VPN nobody else compresses: a first load on a
// phone was all of it, as written.
func Handler() http.Handler {
	return &handler{files: map[string]*file{}}
}

type handler struct {
	mu    sync.Mutex
	files map[string]*file
}

// file is one embedded file ready to send. The files are part of the
// binary and never change while it runs, so each is read, hashed and
// compressed once, the first time it is asked for.
type file struct {
	data []byte
	// packed is data gzipped, nil when that saves nothing (the icons).
	packed []byte
	tag    string
	ctype  string
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Security-Policy", CSP)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("X-Frame-Options", "DENY")

	// The worker and the manifest answer at the root: a worker controls
	// only the paths under its own, and one at /assets/sw.js would control
	// /assets/ and none of the panel.
	switch r.URL.Path {
	case "/sw.js":
		w.Header().Set("Service-Worker-Allowed", "/")
		h.serve(w, r, "sw.js", false)
		return
	case "/manifest.webmanifest":
		h.serve(w, r, "manifest.webmanifest", false)
		return
	}
	if strings.HasPrefix(r.URL.Path, AssetPrefix) {
		name := strings.TrimPrefix(path.Clean("/"+strings.TrimPrefix(r.URL.Path, AssetPrefix)), "/")
		// Under this build's own prefix a file never changes: kept for
		// good. Under another build's (a tab left open across an upgrade)
		// it is this build's file, and revalidated like any other.
		forever := false
		if strings.HasPrefix(name, versionMark) {
			var ver string
			ver, name, _ = strings.Cut(name, "/")
			forever = ver == versionMark+Version()
		}
		// index.html is the shell, not an asset: a module request that got
		// HTML back shows a syntax error with no clue in the console.
		if name == "" || name == "." || name == "index.html" {
			http.NotFound(w, r)
			return
		}
		h.serve(w, r, name, forever)
		return
	}
	h.serve(w, r, "index.html", false)
}

// versionMark opens the path segment that names a build: no file of the
// panel starts with it.
const versionMark = "~"

// Version names this build's files: a hash of all of them, so it changes
// when any does and only then.
//
// The panel has no build step to put a hash in each file's name, and with
// ETags alone every load of the panel asked again for each of its ninety
// files, to be told ninety times that nothing changed: through the tunnel
// from a phone, that was the wait before every screen. The shell now
// points at /assets/~<version>/app.js, the modules import each other by
// relative paths and so stay under that prefix, and what is under it is
// immutable: a second load asks for the shell and nothing else. An upgrade
// changes the version, and with it every address.
var Version = sync.OnceValue(func() string {
	sum := sha256.New()
	_ = fs.WalkDir(FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(FS, p)
		if err != nil {
			return err
		}
		fmt.Fprintf(sum, "%s %d\n", p, len(data))
		sum.Write(data)
		return nil
	})
	return hex.EncodeToString(sum.Sum(nil)[:6])
})

// shell is index.html pointing at this build's files, with every module
// announced: the browser otherwise learns of them as it parses the ones
// before, four rounds deep, and a first load after an upgrade took a
// round trip for each.
var shell = sync.OnceValue(func() []byte {
	data, err := fs.ReadFile(FS, "index.html")
	if err != nil {
		panic(err) // part of the binary
	}
	prefix := AssetPrefix + versionMark + Version() + "/"
	out := string(data)
	for _, name := range []string{"app.css", "app.js"} {
		out = strings.Replace(out, `"`+AssetPrefix+name+`"`, `"`+prefix+name+`"`, 1)
	}
	var preload strings.Builder
	_ = fs.WalkDir(FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".js") && p != "sw.js" && p != "app.js" {
			preload.WriteString(`  <link rel="modulepreload" href="` + prefix + p + "\">\n")
		}
		return err
	})
	return []byte(strings.Replace(out, "</head>", preload.String()+"</head>", 1))
})

func (h *handler) serve(w http.ResponseWriter, r *http.Request, name string, forever bool) {
	f := h.file(name)
	if f == nil {
		http.NotFound(w, r)
		return
	}
	hd := w.Header()
	body, tag := f.data, f.tag
	if f.packed != nil {
		hd.Add("Vary", "Accept-Encoding")
		if gz.Accepts(r) {
			// Another body, another tag: a cache between the browser and
			// here must not answer one for the other.
			body, tag = f.packed, strings.TrimSuffix(f.tag, `"`)+`-gz"`
			hd.Set("Content-Encoding", "gzip")
		}
	}
	hd.Set("ETag", tag)
	if forever {
		hd.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		hd.Set("Cache-Control", "no-cache")
	}
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, tag) {
		hd.Del("Content-Encoding")
		w.WriteHeader(http.StatusNotModified)
		return
	}
	hd.Set("Content-Type", f.ctype)
	hd.Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(body)
}

func (h *handler) file(name string) *file {
	h.mu.Lock()
	defer h.mu.Unlock()
	if f, ok := h.files[name]; ok {
		return f
	}
	var data []byte
	var err error
	if name == "index.html" {
		data = shell()
	} else {
		data, err = fs.ReadFile(FS, name)
	}
	if err != nil {
		// Not kept: the names that are not there are whatever a scanner
		// asks for, without end.
		return nil
	}
	sum := sha256.Sum256(data)
	f := &file{data: data, tag: `"` + hex.EncodeToString(sum[:8]) + `"`, ctype: contentType(name, data)}
	if gz.Compressible(f.ctype) {
		f.packed = gz.Smaller(data)
	}
	h.files[name] = f
	return f
}

// contentType pins the common types. Go's table knows them, but a
// stripped container may ship a bare /etc/mime.types, and a module script
// served as text/plain is refused by the browser. The rest ask the table.
func contentType(name string, data []byte) string {
	switch path.Ext(name) {
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".json":
		return "application/json"
	case ".html":
		return "text/html; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".webmanifest":
		return "application/manifest+json"
	case ".png":
		return "image/png"
	}
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		return ct
	}
	return http.DetectContentType(data)
}
