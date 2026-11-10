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
