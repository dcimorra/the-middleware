package proxy

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/engine"
	"github.com/dcimorra/the-middleware/internal/manifest"
)

func staticSite(t *testing.T, manifestJSON string) (string, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "public"), 0o755)
	m, err := manifest.Parse([]byte(manifestJSON))
	if err != nil {
		t.Fatal(err)
	}
	return dir, New(lookupFor(map[string]engine.Snapshot{"a.test": {Domain: "a.test", Dir: dir, Mode: manifest.ModeStatic, Manifest: m}}))
}

func get(h http.Handler, path string, header ...string) *http.Response {
	r := httptest.NewRequest("GET", "http://a.test"+path, nil)
	r.Host = "a.test"
	for i := 0; i+1 < len(header); i += 2 {
		r.Header.Set(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Result()
}

// The path asked for was checked, and then the folder's index.html, the
// SPA's shell and the 404 page were served wherever they pointed: a link
// named index.html to a file of another site, or of the house, was
// answered to anyone.
func TestThePagesServedInAPathsPlaceDoNotLeaveTheSiteEither(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(secret, []byte("another site's secret"), 0o600)
	for name, c := range map[string]struct{ manifest, link, ask string }{
		"a folder's index":  {`{"version":2}`, "public/docs/index.html", "/docs/"},
		"the SPA's shell":   {`{"version":2,"static":{"root":"public","spa":true}}`, "public/index.html", "/any/route"},
		"the 404 page":      {`{"version":2}`, "public/404.html", "/missing"},
		"the root's index":  {`{"version":2}`, "public/index.html", "/"},
		"a file, as before": {`{"version":2}`, "public/notes.txt", "/notes.txt"},
	} {
		dir, h := staticSite(t, c.manifest)
		os.MkdirAll(filepath.Dir(filepath.Join(dir, c.link)), 0o755)
		if err := os.Symlink(secret, filepath.Join(dir, c.link)); err != nil {
			t.Fatal(err)
		}
		resp := get(h, c.ask)
		body, _ := io.ReadAll(resp.Body)
		if strings.Contains(string(body), "secret") {
			t.Errorf("%s: a link out of the site was served (%d)", name, resp.StatusCode)
		}
		if resp.StatusCode != 404 {
			t.Errorf("%s: status %d, want 404", name, resp.StatusCode)
		}
	}
}

// Links that stay in the site are served, relative or absolute: a site
// whose public/ is a link to its build folder is a common layout, and
// os.Root alone refuses the absolute one.
func TestLinksThatStayInTheSiteAreFollowed(t *testing.T) {
	dir, h := staticSite(t, `{"version":2,"static":{"root":"current"}}`)
	os.MkdirAll(filepath.Join(dir, "releases", "7"), 0o755)
	os.WriteFile(filepath.Join(dir, "releases", "7", "index.html"), []byte("release seven"), 0o644)
	os.WriteFile(filepath.Join(dir, "releases", "7", "app.txt"), []byte("the app"), 0o644)
	for _, target := range []string{filepath.Join("releases", "7"), filepath.Join(dir, "releases", "7")} {
		os.Remove(filepath.Join(dir, "current"))
		if err := os.Symlink(target, filepath.Join(dir, "current")); err != nil {
			t.Fatal(err)
		}
		for ask, want := range map[string]string{"/": "release seven", "/app.txt": "the app"} {
			resp := get(h, ask)
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 200 || string(body) != want {
				t.Errorf("link to %s, %s: %d %q", target, ask, resp.StatusCode, body)
			}
		}
		if resp := get(h, "/nothing.txt"); resp.StatusCode != 404 {
			t.Errorf("link to %s: a missing file answered %d", target, resp.StatusCode)
		}
	}
}

func TestStaticKeepsItsAnswersForFoldersPagesAndWhatIsMissing(t *testing.T) {
	dir, h := staticSite(t, `{"version":2}`)
	os.MkdirAll(filepath.Join(dir, "public", "docs"), 0o755)
	os.WriteFile(filepath.Join(dir, "public", "index.html"), []byte("home"), 0o644)
	os.WriteFile(filepath.Join(dir, "public", "docs", "index.html"), []byte("docs"), 0o644)
	os.WriteFile(filepath.Join(dir, "public", "404.html"), []byte("<h1>not here</h1>"), 0o644)
	if resp := get(h, "/docs"); resp.StatusCode != 301 || resp.Header.Get("Location") != "/docs/" {
		t.Errorf("a folder without its slash: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp := get(h, "/docs/index.html"); resp.StatusCode != 301 || resp.Header.Get("Location") != "/docs/" {
		t.Errorf("index.html by name: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp := get(h, "/docs/")
	if body, _ := io.ReadAll(resp.Body); resp.StatusCode != 200 || string(body) != "docs" || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Errorf("a folder's page: %d %q %q", resp.StatusCode, body, resp.Header.Get("Content-Type"))
	}
	for _, ask := range []string{"/missing", "/index.html/under-a-file", "/docs/missing.css"} {
		resp = get(h, ask)
		if body, _ := io.ReadAll(resp.Body); resp.StatusCode != 404 || string(body) != "<h1>not here</h1>" {
			t.Errorf("%s: %d %q, want the site's own 404 page", ask, resp.StatusCode, body)
		}
	}
	// Not modified since: nothing is sent again.
	os.WriteFile(filepath.Join(dir, "public", "big.html"), []byte(strings.Repeat("<p>a page</p>", 500)), 0o644)
	for _, ask := range []string{"/", "/big.html"} {
		first := get(h, ask, "Accept-Encoding", "gzip")
		resp := get(h, ask, "Accept-Encoding", "gzip", "If-Modified-Since", first.Header.Get("Last-Modified"))
		if resp.StatusCode != 304 || resp.Header.Get("Content-Length") != "" {
			t.Errorf("%s, If-Modified-Since: %d, Content-Length %q", ask, resp.StatusCode, resp.Header.Get("Content-Length"))
		}
	}
}

func TestStaticTextIsCompressedForWhoeverTakesIt(t *testing.T) {
	dir, h := staticSite(t, `{"version":2}`)
	css := strings.Repeat("body { margin: 0; padding: 0 }\n", 400)
	file := filepath.Join(dir, "public", "app.css")
	os.WriteFile(file, []byte(css), 0o644)
	os.WriteFile(filepath.Join(dir, "public", "tiny.css"), []byte("a{}"), 0o644)
	png := bytes.Repeat([]byte{0x89, 'P', 'N', 'G', 0, 0, 0, 0}, 1000)
	os.WriteFile(filepath.Join(dir, "public", "pic.png"), png, 0o644)

	unpack := func(resp *http.Response) string {
		t.Helper()
		zr, err := gzip.NewReader(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := io.ReadAll(zr)
		return string(out)
	}
	for i := 0; i < 2; i++ { // the second from the cache
		resp := get(h, "/app.css", "Accept-Encoding", "br, gzip")
		if resp.Header.Get("Content-Encoding") != "gzip" || resp.Header.Get("Vary") != "Accept-Encoding" {
			t.Fatalf("headers: %v", resp.Header)
		}
		if resp.ContentLength <= 0 || resp.ContentLength > int64(len(css))/5 {
			t.Errorf("Content-Length %d of %d", resp.ContentLength, len(css))
		}
		if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/css") {
			t.Errorf("Content-Type %q", resp.Header.Get("Content-Type"))
		}
		if unpack(resp) != css {
			t.Error("the file did not come back as it is")
		}
	}
	// Without the header, with a range, small, or not text: as it is.
	for name, resp := range map[string]*http.Response{
		"no Accept-Encoding": get(h, "/app.css"),
		"gzip refused":       get(h, "/app.css", "Accept-Encoding", "gzip;q=0"),
		"a range":            get(h, "/app.css", "Accept-Encoding", "gzip", "Range", "bytes=0-9"),
		"a small file":       get(h, "/tiny.css", "Accept-Encoding", "gzip"),
		"a picture":          get(h, "/pic.png", "Accept-Encoding", "gzip"),
	} {
		if resp.Header.Get("Content-Encoding") != "" {
			t.Errorf("%s: compressed", name)
		}
	}
	if resp := get(h, "/app.css", "Accept-Encoding", "gzip", "Range", "bytes=0-9"); resp.StatusCode != 206 {
		t.Errorf("a range answered %d", resp.StatusCode)
	} else if body, _ := io.ReadAll(resp.Body); string(body) != css[:10] {
		t.Errorf("a range answered %q", body)
	}
	// Written again: the new content, not what was kept.
	changed := strings.Repeat("html { color: red }\n", 400)
	os.WriteFile(file, []byte(changed), 0o644)
	os.Chtimes(file, time.Now().Add(time.Hour), time.Now().Add(time.Hour))
	if got := unpack(get(h, "/app.css", "Accept-Encoding", "gzip")); got != changed {
		t.Error("a file written again was answered from the old copy")
	}
	// A HEAD says what a GET would, with no body.
	r := httptest.NewRequest("HEAD", "http://a.test/app.css", nil)
	r.Host = "a.test"
	r.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Body.Len() != 0 || w.Header().Get("Content-Encoding") != "gzip" {
		t.Errorf("HEAD: %d bytes, %v", w.Body.Len(), w.Header())
	}
}

func TestABundlersHashedFilesAreKeptForGood(t *testing.T) {
	for name, want := range map[string]bool{
		"main.8f3c2a1b.js":              true,
		"index-Bx2Yz9Qa.css":            true,
		"chunk-ABCD1234.mjs":            true,
		"framework-2d2a8c1e4f3b5a6c.js": true,
		"inter-latin.4f2a9c1e.woff2":    true,
		"jquery-3.7.1.min.js":           false,
		"app.bundle.js":                 false,
		"logo-2024.css":                 false,
		"vendor-12345678.js":            false,
		"styles.css":                    false,
		"index-Bx2Yz9Qa.html":           false,
		"report-2024q3a1.pdf":           false,
		"photo-a1b2c3d4.png":            false,
		"-a1b2c3d4.js":                  false,
		"x.a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c.js": false,
	} {
		if got := Fingerprinted(name); got != want {
			t.Errorf("%s: fingerprinted %v, want %v", name, got, want)
		}
	}
	dir, h := staticSite(t, `{"version":2}`)
	os.MkdirAll(filepath.Join(dir, "public", "assets"), 0o755)
	os.WriteFile(filepath.Join(dir, "public", "assets", "index-Bx2Yz9Qa.js"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "public", "assets", "plain.js"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "public", "index.html"), []byte("x"), 0o644)
	if cc := get(h, "/assets/index-Bx2Yz9Qa.js").Header.Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Errorf("a hashed file: Cache-Control %q", cc)
	}
	for _, ask := range []string{"/assets/plain.js", "/", "/assets/missing-Bx2Yz9Qa.js"} {
		if cc := get(h, ask).Header.Get("Cache-Control"); cc != "" {
			t.Errorf("%s: Cache-Control %q", ask, cc)
		}
	}
	// What the manifest says wins over what the name suggests.
	dir, h = staticSite(t, `{"version":2,"headers":{"Cache-Control":"no-cache"}}`)
	os.WriteFile(filepath.Join(dir, "public", "index-Bx2Yz9Qa.js"), []byte("x"), 0o644)
	if cc := get(h, "/index-Bx2Yz9Qa.js").Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("the manifest's header: Cache-Control %q", cc)
	}
	dir, h = staticSite(t, `{"version":2,"cache":{"static_max_age":"1h"}}`)
	os.WriteFile(filepath.Join(dir, "public", "index-Bx2Yz9Qa.js"), []byte("x"), 0o644)
	if cc := get(h, "/index-Bx2Yz9Qa.js").Header.Get("Cache-Control"); cc != "public, max-age=3600" {
		t.Errorf("static_max_age: Cache-Control %q", cc)
	}
}

// Resolving links by hand is the slow road, there for absolute links
// only. A site with no public/ asked for "/" took it on every request:
// its path was the machine's root, which the os.Root refuses.
func TestOrdinaryPathsAreOpenedWithoutResolvingLinks(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "docs"), 0o755)
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("home"), 0o644)
	os.WriteFile(filepath.Join(dir, "docs", "index.html"), []byte("docs"), 0o644)
	os.Symlink("docs", filepath.Join(dir, "manual"))
	m, _ := manifest.Parse([]byte(`{"version":2}`))
	h := New(lookupFor(map[string]engine.Snapshot{"a.test": {Domain: "a.test", Dir: dir, Mode: manifest.ModeStatic, Manifest: m}}))
	for ask, want := range map[string]string{"/": "home", "/docs/": "docs", "/manual/": "docs"} {
		resp := get(h, ask)
		if body, _ := io.ReadAll(resp.Body); resp.StatusCode != 200 || string(body) != want {
			t.Errorf("%s: %d %q", ask, resp.StatusCode, body)
		}
	}
	site, err := openSite(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer site.Close()
	for _, name := range []string{".", "index.html", "docs", filepath.Join("docs", "index.html"), filepath.Join("manual", "index.html"), "missing.html"} {
		if f, _, err := site.open(name); err == nil {
			f.Close()
		}
	}
	if site.resolved != 0 {
		t.Errorf("%d ordinary opens resolved links by hand", site.resolved)
	}
}
