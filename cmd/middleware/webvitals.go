package main

import (
	"context"
	"net/http"
	"path/filepath"
	"time"

	"github.com/dcimorra/the-middleware/internal/app"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/proxy"
	"github.com/dcimorra/the-middleware/internal/webvitals"
)

// orderWebVitals is inside the doors, like the visits (a page the
// password door kept is not a page anyone saw), and outside the cache,
// so the cache keeps the page as the site wrote it: turning the
// measuring off takes the script out of the next page served, not of the
// next page the cache throws away.
const orderWebVitals = orderVisits + 1

var theWebVitals *webvitals.Book

func init() {
	hookBuilders = append(hookBuilders, hookBuilder{Name: "webvitals", Order: orderWebVitals, Build: buildWebVitals})
}

func buildWebVitals(a *app.App) app.Hook {
	theWebVitals = webvitals.New(filepath.Join(dataDir(a), "webvitals"))
	return webVitalsHook{a: a}
}

type webVitalsHook struct{ a *app.App }

func (h webVitalsHook) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Every request crosses this line; all but pages and the two
		// paths of the house leave at once, before the engine is asked.
		path := r.URL.Path
		script, beacon := path == webvitals.ScriptPath, path == webvitals.BeaconPath
		if !script && !beacon && !webvitals.Wants(r) {
			next.ServeHTTP(w, r)
			return
		}
		host := proxy.Host(r)
		sn, ok := h.a.Engine.Lookup(host)
		if !ok || sn.Manifest == nil || !sn.Manifest.WebVitals || sn.Mode == manifest.ModeProject {
			next.ServeHTTP(w, r)
			return
		}
		switch {
		case script:
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			webvitals.ServeScript(w)
		case beacon:
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			if !theWebVitals.Allow(sn.Domain) {
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			s, err := webvitals.ParseBeacon(r, host)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			_ = theWebVitals.Note(sn.Domain, s)
			w.WriteHeader(http.StatusNoContent)
		default:
			iw, r2, done := webvitals.Inject(w, r)
			next.ServeHTTP(iw, r2)
			done()
		}
	})
}

func (h webVitalsHook) Run(ctx context.Context, _ *app.App) error {
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				theWebVitals.Flush()
			}
		}
	}()
	return nil
}

func (h webVitalsHook) Stop() { theWebVitals.Flush() }
