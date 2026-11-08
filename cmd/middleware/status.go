package main

import (
	"context"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/dcimorra/the-middleware/internal/app"
	"github.com/dcimorra/the-middleware/internal/engine"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/proxy"
	"github.com/dcimorra/the-middleware/internal/status"
)

// orderStatus is inside the doors (a private site's status is private
// too) and outside the cache and the site: the page is the house's, and
// it has to answer when the site does not.
const orderStatus = orderWebVitals + 1

var theStatus *status.Book

func init() {
	hookBuilders = append(hookBuilders, hookBuilder{Name: "status", Order: orderStatus, Build: buildStatus})
}

func buildStatus(a *app.App) app.Hook {
	theStatus = status.New(filepath.Join(dataDir(a), "status"))
	h := &statusHook{a: a}
	h.refresh()
	return h
}

// statusHook counts each site's minutes and answers the status pages.
// paths is every path some site's page answers at: a request whose path
// is in none of them, nearly all, leaves without the engine being asked.
type statusHook struct {
	a     *app.App
	paths atomic.Pointer[map[string]bool]
}

func (h *statusHook) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.Method != http.MethodGet && r.Method != http.MethodHead) || !(*h.paths.Load())[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		sn, ok := h.a.Engine.Lookup(proxy.Host(r))
		if !ok || sn.Manifest == nil || sn.Manifest.StatusPage == nil || sn.Manifest.StatusPage.Where() != r.URL.Path {
			next.ServeHTTP(w, r)
			return
		}
		status.Serve(w, r, sn.Manifest.StatusPage.Title, status.PublicOf(sn.Domain, theStatus.Report(sn.Domain), time.Now()))
	})
}

func (h *statusHook) Run(ctx context.Context, a *app.App) error {
	h.refresh()
	go func() {
		// The paths are read again every few seconds, so a page turned on
		// answers about as soon as the engine has read the manifest, not
		// at the next minute. Reading every site's snapshot is a map walk.
		paths := time.NewTicker(5 * time.Second)
		defer paths.Stop()
		minute := time.NewTicker(time.Minute)
		defer minute.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-paths.C:
				h.refresh()
			case <-minute.C:
				h.observe()
				theStatus.Flush()
			}
		}
	}()
	return nil
}

func (h *statusHook) Stop() { theStatus.Flush() }

func (h *statusHook) refresh() {
	// The default path is always looked at: most pages use it, and a page
	// turned on there answers before the next refresh.
	paths := map[string]bool{manifest.DefaultStatusPath: true}
	for _, sn := range h.a.Engine.Sites() {
		if sn.Manifest != nil && sn.Manifest.StatusPage != nil {
			paths[sn.Manifest.StatusPage.Where()] = true
		}
	}
	h.paths.Store(&paths)
}

// observe counts this minute for every site that answers names, whether
// or not it shows a page: a page turned on later starts with the history
// the house already has.
func (h *statusHook) observe() {
	for _, sn := range h.a.Engine.Sites() {
		if sn.Mode == manifest.ModeProject || manifest.IsProject(sn.Domain) {
			continue
		}
		s, reason := siteStatus(sn)
		theStatus.Observe(sn.Domain, s, reason)
	}
}

// siteStatus is what a minute of a site counts as. A hold, a stop or a
// disabled site is maintenance, the operator's choice; a static site is
// up whenever the house is, because the house is what serves it.
func siteStatus(sn engine.Snapshot) (status.State, string) {
	m := sn.Manifest
	switch {
	case m != nil && m.Hold != nil, sn.Mode == manifest.ModeDisabled, m != nil && m.Disabled:
		return status.Maintenance, ""
	case sn.Mode == manifest.ModeStatic:
		return status.Up, ""
	case sn.Health == engine.HealthOK:
		return status.Up, ""
	case sn.Health == engine.HealthDown:
		return status.Down, sn.Error
	case !sn.Alive && sn.Error != "":
		return status.Down, sn.Error
	}
	return status.Unknown, ""
}
