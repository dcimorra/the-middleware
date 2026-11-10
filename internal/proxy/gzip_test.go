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

	"github.com/dcimorra/the-middleware/internal/engine"
	"github.com/dcimorra/the-middleware/internal/manifest"
)

func get(h http.Handler, host, path string, header map[string]string) *http.Response {
	r := httptest.NewRequest("GET", "http://"+host+path, nil)
	r.Host = host
	for k, v := range header {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Result()
}

// A site's static text is sent compressed to a client that takes gzip,
// as it is to one that does not, and two sites with a file of the same
// name never answer each other's.
func TestStaticTextIsCompressed(t *testing.T) {
	m, _ := manifest.Parse([]byte(`{"version":2}`))
	snaps := map[string]engine.Snapshot{}
	for _, site := range []string{"a.test", "b.test"} {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "app.css"), []byte(strings.Repeat("body{color:red} /* "+site+" */\n", 200)), 0o644)
		os.WriteFile(filepath.Join(dir, "logo.png"), bytes.Repeat([]byte{0x89, 'P', 'N', 'G'}, 1000), 0o644)
		snaps[site] = engine.Snapshot{Domain: site, Dir: dir, Mode: manifest.ModeStatic, Manifest: m}
	}
	h := New(lookupFor(snaps))
	for _, site := range []string{"a.test", "b.test"} {
		resp := get(h, site, "/app.css", map[string]string{"Accept-Encoding": "gzip, br"})
		if resp.Header.Get("Content-Encoding") != "gzip" || !strings.Contains(resp.Header.Get("Vary"), "Accept-Encoding") {
			t.Fatalf("%s: not compressed: %v", site, resp.Header)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
			t.Fatalf("%s: type %q", site, ct)
		}
		zr, err := gzip.NewReader(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(zr)
		if !strings.Contains(string(body), "/* "+site+" */") || strings.Contains(string(body), "/* "+other(site)+" */") {
			t.Fatalf("%s answered with another site's file", site)
		}
	}
	plain := get(h, "a.test", "/app.css", nil)
	if plain.Header.Get("Content-Encoding") != "" || !strings.Contains(plain.Header.Get("Vary"), "Accept-Encoding") {
		t.Fatalf("a client without gzip got %v", plain.Header)
	}
	ranged := get(h, "a.test", "/app.css", map[string]string{"Accept-Encoding": "gzip", "Range": "bytes=0-9"})
	if ranged.StatusCode != http.StatusPartialContent || ranged.Header.Get("Content-Encoding") != "" {
		t.Fatalf("a range was answered %d %v", ranged.StatusCode, ranged.Header)
	}
	if b, _ := io.ReadAll(ranged.Body); string(b) != "body{color" {
		t.Fatalf("range body %q", b)
	}
	if png := get(h, "a.test", "/logo.png", map[string]string{"Accept-Encoding": "gzip"}); png.Header.Get("Content-Encoding") != "" {
		t.Fatal("an image was compressed")
	}
}

func other(site string) string {
	if site == "a.test" {
		return "b.test"
	}
	return "a.test"
}

func TestFingerprinted(t *testing.T) {
	for name, want := range map[string]bool{
		"main.8f3c2a1b.js":              true,
		"index.Bx2Yz9Qa.css":            true,
		"chunk-ABCD1234.js":             true,
		"framework-2d2a8c1e4f3b5a6c.js": true,
		"font.a1b2c3d4e5.woff2":         true,
		"logo-2024.png":                 false,
		"logo-20241105.png":             false,
		"jquery-3.7.1.min.js":           false,
		"app.bundle.js":                 false,
		"index.html":                    false,
		"about-8f3c2a1b.html":           false,
		"styles.css":                    false,
		"8f3c2a1b.js":                   false,
		"vendor-abcdefgh.js":            false,
	} {
		if got := Fingerprinted(name); got != want {
			t.Errorf("%s: %v, want %v", name, got, want)
		}
	}
}

// A bundler's hashed file is kept for good; a page, a plain name and a
// SPA's fallback for a hashed path that is not there are not.
func TestAHashedFileIsImmutable(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "assets"), 0o755)
	os.WriteFile(filepath.Join(dir, "assets", "index-4f2a9c1e.js"), []byte("console.log(1)"), 0o644)
	os.WriteFile(filepath.Join(dir, "app.js"), []byte("console.log(2)"), 0o644)
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("<p>spa</p>"), 0o644)
	m, _ := manifest.Parse([]byte(`{"version":2,"static":{"root":".","spa":true}}`))
	h := New(lookupFor(map[string]engine.Snapshot{"a.test": {Domain: "a.test", Dir: dir, Mode: manifest.ModeStatic, Manifest: m}}))
	if cc := get(h, "a.test", "/assets/index-4f2a9c1e.js", nil).Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("hashed file: %q", cc)
	}
	for _, p := range []string{"/app.js", "/", "/assets/index-00000000aa.js"} {
		if cc := get(h, "a.test", p, nil).Header.Get("Cache-Control"); cc != "" {
			t.Fatalf("%s: %q", p, cc)
		}
	}
	// The manifest's own choice wins.
	m2, _ := manifest.Parse([]byte(`{"version":2,"static":{"root":"."},"cache":{"static_max_age":"1h"}}`))
	h2 := New(lookupFor(map[string]engine.Snapshot{"a.test": {Domain: "a.test", Dir: dir, Mode: manifest.ModeStatic, Manifest: m2}}))
	if cc := get(h2, "a.test", "/assets/index-4f2a9c1e.js", nil).Header.Get("Cache-Control"); cc != "public, max-age=3600" {
		t.Fatalf("manifest: %q", cc)
	}
}
