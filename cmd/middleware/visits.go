package main

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"path/filepath"
	"time"

	"github.com/dcimorra/the-middleware/internal/app"
	"github.com/dcimorra/the-middleware/internal/edge"
	"github.com/dcimorra/the-middleware/internal/proxy"
	"github.com/dcimorra/the-middleware/internal/visits"
)

// orderVisits counts inside the doors (the WAF, the security rules, the
// password door: who they turn away read nothing) and outside the edge
// cache, so a page served from it is a page read all the same.
const orderVisits = orderTraffic + 2

var theVisits *visits.Book

func init() {
	hookBuilders = append(hookBuilders, hookBuilder{Name: "visits", Order: orderVisits, Build: buildVisits})
}

func buildVisits(a *app.App) app.Hook {
	theVisits = visits.New(filepath.Join(dataDir(a), "visits"))
	return visitsHook{a: a}
}

// visitsHook counts page views, flushes every minute, and once more when
// the process stops (app.Stopper).
type visitsHook struct{ a *app.App }

func (h visitsHook) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Everything that is not a browser opening a page leaves here,
		// before the engine is asked anything and with the writer as it
		// came: this is the line every request of every site crosses.
		if !visits.Navigation(r) {
			next.ServeHTTP(w, r)
			return
		}
		domain, ok := siteOf(h.a, proxy.Host(r))
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		// What the request says is read now: the site may change nothing
		// of it, but the handlers below may.
		q := visits.Request{
			Domain: domain, Host: proxy.Host(r), Path: r.URL.Path, Referrer: r.Referer(), UA: r.UserAgent(),
			IP: edge.ClientIP(r), Country: edge.Country(r), Mobile: r.Header.Get("Sec-CH-UA-Mobile"),
		}
		// Cloudflare's visitor location headers, when the zone has them
		// on; believed from the edge only, like the country.
		if q.Country != "" {
			q.Region = r.Header.Get("CF-Region")
		}
		rw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(rw, r)
		status := rw.status
		if status == 0 {
			status = http.StatusOK
		}
		if visits.Answered(status, rw.Header().Get("Content-Type")) {
			theVisits.Note(q)
		}
	})
}

func (h visitsHook) Run(ctx context.Context, _ *app.App) error {
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				theVisits.Flush()
			}
		}
	}()
	return nil
}

func (h visitsHook) Stop() { theVisits.Flush() }

// statusWriter keeps the status of a page's answer. It passes Flush and
// Hijack through and exposes Unwrap, so a page that streams as it renders
// streams all the same.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return h.Hijack()
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
