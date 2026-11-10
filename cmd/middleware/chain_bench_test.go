package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/api"
	"github.com/dcimorra/the-middleware/internal/app"
	"github.com/dcimorra/the-middleware/internal/settings"
)

// The request chain, measured as it is wired: every hook `serve` builds,
// in its order, in front of a static site and of an upstream. Every
// request a site gets crosses all of it, so a hook that starts reading a
// file or taking a global lock per request shows up here before it shows
// up on a busy machine. Run with
//
//	go test ./cmd/middleware -run '^$' -bench Chain -benchmem
func BenchmarkChainStatic(b *testing.B) { benchChain(b, "static.example", "/") }

func BenchmarkChainUpstream(b *testing.B) { benchChain(b, "up.example", "/hello") }

// BenchmarkChainCached is an upstream's page answered by the house cache.
func BenchmarkChainCached(b *testing.B) { benchChain(b, "cached.example", "/hello") }

func benchChain(b *testing.B, host, path string) {
	addr := startChain(b)
	tr := &http.Transport{MaxIdleConnsPerHost: 64}
	client := &http.Client{Transport: tr}
	defer tr.CloseIdleConnections()
	url := "http://" + addr + path
	ua := "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36"
	get := func() error {
		req, _ := http.NewRequest("GET", url, nil)
		req.Host = host
		req.Header.Set("User-Agent", ua)
		req.Header.Set("Sec-Fetch-Mode", "navigate")
		req.Header.Set("Sec-Fetch-Dest", "document")
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return &statusErr{resp.StatusCode}
		}
		return nil
	}
	if err := get(); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := get(); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

type statusErr struct{ code int }

func (e *statusErr) Error() string { return http.StatusText(e.code) }

func startChain(b *testing.B) string {
	b.Helper()
	log.SetOutput(io.Discard)
	b.Cleanup(func() { log.SetOutput(os.Stderr) })
	root := b.TempDir()
	os.MkdirAll(filepath.Join(root, "data"), 0o755)
	s := settings.Defaults()
	s.Listen = "127.0.0.1:0"
	if err := settings.Save(root, s); err != nil {
		b.Fatal(err)
	}
	up := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<html><head></head><body>hello</body></html>")
	})}
	upLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	go up.Serve(upLn)
	b.Cleanup(func() { up.Close() })
	for name, manifest := range map[string]string{
		"static.example": `{"version":2,"waf":{"rate":{"requests":100000000,"window":"1s","burst":100000000}}}`,
		"up.example":     `{"version":2,"waf":{"rate":{"requests":100000000,"window":"1s","burst":100000000}},"upstream":"http://` + upLn.Addr().String() + `"}`,
		"cached.example": `{"version":2,"waf":{"rate":{"requests":100000000,"window":"1s","burst":100000000}},"cache":{"rules":[{"path":"/","mode":"force","ttl":600}]},"upstream":"http://` + upLn.Addr().String() + `"}`,
	} {
		dir := filepath.Join(root, "sites", name)
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html><head></head><body>"+strings.Repeat("x", 2048)+"</body></html>"), 0o644)
		os.WriteFile(filepath.Join(dir, "site.json"), []byte(manifest), 0o644)
	}
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
			b.Error("serve did not stop")
		}
	})
	sock := api.SocketPath(root)
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}}
	for wait := time.Now().Add(15 * time.Second); ; {
		if resp, err := client.Get("http://unix" + api.Prefix + "/events?type=middleware.started"); err == nil {
			var evs []struct {
				Data struct {
					Listen string `json:"listen"`
				} `json:"data"`
			}
			json.NewDecoder(resp.Body).Decode(&evs)
			resp.Body.Close()
			if len(evs) > 0 && evs[len(evs)-1].Data.Listen != "" {
				addr := evs[len(evs)-1].Data.Listen
				// The engine routes on its first tick; wait for both sites.
				for ready := time.Now().Add(15 * time.Second); time.Now().Before(ready); time.Sleep(50 * time.Millisecond) {
					if hs := a.Engine.Hosts(); hs["up.example"] != "" && hs["static.example"] != "" && hs["cached.example"] != "" {
						return addr
					}
				}
				b.Fatal("the sites were never routed")
			}
		}
		if time.Now().After(wait) {
			b.Fatal("middleware.started never said where it listens")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
