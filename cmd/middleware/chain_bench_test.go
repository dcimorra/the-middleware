package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/app"
	"github.com/dcimorra/the-middleware/internal/settings"
)

// benchChain builds the process as `serve` does, every hook included, with
// one site per way of answering, and returns the whole chain. What a
// request costs the house is the sum of every hook it crosses, and only
// the wired chain shows it: each hook measured alone looked free.
func benchChain(b *testing.B) http.Handler {
	b.Helper()
	root := b.TempDir()
	os.MkdirAll(filepath.Join(root, "data"), 0o755)
	s := settings.Defaults()
	s.Listen = "127.0.0.1:0"
	if err := settings.Save(root, s); err != nil {
		b.Fatal(err)
	}
	back := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, strings.Repeat("<p>hello from the site</p>\n", 200))
	}))
	b.Cleanup(back.Close)
	write := func(domain, manifest string, files map[string]string) {
		dir := filepath.Join(root, "sites", domain)
		os.MkdirAll(filepath.Join(dir, "assets"), 0o755)
		for name, body := range files {
			os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
		}
		os.WriteFile(filepath.Join(dir, "site.json"), []byte(manifest), 0o644)
	}
	page := "<html><head><title>t</title></head><body>" + strings.Repeat("<p>static page</p>\n", 200) + "</body></html>"
	write("static.example", `{"version":2}`, map[string]string{"index.html": page, "assets/app.css": strings.Repeat("body{margin:0}\n", 500)})
	write("proxied.example", `{"version":2,"upstream":"`+back.URL+`"}`, nil)
	write("cached.example", `{"version":2,"upstream":"`+back.URL+`","cache":{"rules":[{"path":"/","mode":"force","ttl":600}]}}`, nil)
	a, err := app.New(root)
	if err != nil {
		b.Fatal(err)
	}
	a.Hooks = hooks(a)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	b.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, ok := a.Engine.Lookup("proxied.example"); ok {
			break
		}
		if time.Now().After(deadline) {
			b.Fatal("the engine never loaded the sites")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	return a.Handler
}

type discard struct {
	h    http.Header
	code int
}

func (d *discard) Header() http.Header         { return d.h }
func (d *discard) Write(p []byte) (int, error) { return len(p), nil }
func (d *discard) WriteHeader(c int)           { d.code = c }
func (d *discard) Flush()                      {}

func BenchmarkChain(b *testing.B) {
	h := benchChain(b)
	for _, c := range []struct{ name, host, path string }{
		{"static-page", "static.example", "/"},
		{"static-asset", "static.example", "/assets/app.css"},
		{"proxied", "proxied.example", "/"},
		{"cached", "cached.example", "/"},
		{"unknown", "nobody.example", "/"},
	} {
		var next atomic.Uint32
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					r := httptest.NewRequest("GET", "http://"+c.host+c.path, nil)
					r.RemoteAddr = "127.0.0.1:40000"
					// An address per request: one address asking this fast is
					// what the rate limit exists to refuse.
					n := next.Add(1)
					r.Header.Set("CF-Connecting-IP", netip.AddrFrom4([4]byte{11, byte(n >> 16), byte(n >> 8), byte(n)}).String())
					r.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36")
					r.Header.Set("Accept", "text/html")
					r.Header.Set("Sec-Fetch-Mode", "navigate")
					r.Header.Set("Sec-Fetch-Dest", "document")
					w := &discard{h: http.Header{}}
					h.ServeHTTP(w, r)
					if w.code >= 400 && c.name != "unknown" {
						b.Fatalf("%s answered %d", c.name, w.code)
					}
				}
			})
		})
	}
}
