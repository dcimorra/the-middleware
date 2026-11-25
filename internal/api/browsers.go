package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/dcimorra/the-middleware/internal/browsers"
	"github.com/dcimorra/the-middleware/internal/cgroups"
	"github.com/dcimorra/the-middleware/internal/settings"
	"github.com/dcimorra/the-middleware/internal/sites"
)

// BrowsersView is the node's pool of browsers.
type BrowsersView struct {
	// Count is the pool's size in force; Recommended is the machine's
	// number, which applies while Configured is null.
	Count       int   `json:"count"`
	Recommended int   `json:"recommended"`
	Configured  *int  `json:"configured"`
	Max         int   `json:"max"`
	IdleMinutes int   `json:"idle_minutes"`
	Available   bool  `json:"available"`
	Free        int   `json:"free"`
	CPUs        int   `json:"cpus"`
	Memory      int64 `json:"memory"`
	// PerBrowser is the memory one is counted as holding.
	PerBrowser int64           `json:"per_browser"`
	Slots      []browsers.Slot `json:"slots"`
	// Mine is the caller site's own, with its endpoint.
	Mine *browsers.Slot `json:"mine,omitempty"`
}

// RegisterBrowsers mounts the pool: the operator's view and freeing one,
// and each site's view, take and give-back.
func (s *Server) RegisterBrowsers(p *browsers.Pool, machine cgroups.Machine) {
	view := func(site string) BrowsersView {
		var conf settings.Browsers
		if s.opts.Settings != nil {
			conf = s.opts.Settings().Browsers
		}
		v := BrowsersView{Count: p.Size(), Recommended: settings.RecommendedBrowsers(machine), Configured: conf.Count, Max: settings.MaxBrowsers,
			IdleMinutes: int(conf.Idle() / time.Minute), Available: p.Available(), CPUs: machine.CPUs, Memory: machine.Memory,
			PerBrowser: settings.BrowserMemory, Slots: p.List(site)}
		for i := range v.Slots {
			sl := v.Slots[i]
			if sl.Free {
				v.Free++
			}
			if site != "" && sl.Site == site {
				v.Mine = &sl
			}
		}
		return v
	}
	siteOf := func(r *http.Request) (string, error) {
		domain, err := domainOf(r)
		if err != nil {
			return "", err
		}
		m, err := s.sitesManager()
		if err != nil {
			return "", err
		}
		if _, err := m.Get(domain); err != nil {
			if errors.Is(err, sites.ErrNotFound) {
				return "", Errorf(http.StatusNotFound, "%v", err)
			}
			return "", err
		}
		return domain, nil
	}
	s.Handle("GET", Prefix+"/browsers", "The node's browsers for its sites: how many (the operator's number, or the one recommended for the machine), whether there is a browser to start, and each one free or held by a site, since when, for what and when it is taken back unused", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return view(""), nil
	})
	s.Handle("DELETE", Prefix+"/browsers/{slot}", "Free a browser a site holds: its browser and profile end, and the site is told by the event", func(w http.ResponseWriter, r *http.Request) (any, error) {
		n, err := strconv.Atoi(r.PathValue("slot"))
		if err != nil {
			return nil, Errorf(http.StatusBadRequest, "slot: a number")
		}
		if _, err := p.Release("", n, actorOf(r)); err != nil {
			return nil, browsersError(err)
		}
		return view(""), nil
	})
	s.Handle("GET", Prefix+"/sites/{domain}/browsers", "The node's browsers as the site sees them: which are free, who holds the rest, and the one it holds with its DevTools endpoint", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := siteOf(r)
		if err != nil {
			return nil, err
		}
		return view(domain), nil
	})
	s.Handle("POST", Prefix+"/sites/{domain}/browsers", "Take a free browser for the site, saying what for ({for}): → the slot with its DevTools endpoint. A site holds one at a time; taking again renews it. 409 when every one is held", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := siteOf(r)
		if err != nil {
			return nil, err
		}
		var body struct {
			For string `json:"for"`
		}
		if r.ContentLength != 0 {
			if err := ReadJSON(r, &body); err != nil {
				return nil, err
			}
		}
		sl, again, err := p.Take(domain, body.For, actorOf(r))
		if err != nil {
			return nil, browsersError(err)
		}
		v := view(domain)
		return map[string]any{"browser": sl, "again": again, "idle_minutes": v.IdleMinutes, "free": v.Free, "count": v.Count}, nil
	})
	s.Handle("DELETE", Prefix+"/sites/{domain}/browsers", "Give back the browser the site holds: its browser and profile end", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := siteOf(r)
		if err != nil {
			return nil, err
		}
		n, err := p.Release(domain, 0, actorOf(r))
		if err != nil {
			return nil, browsersError(err)
		}
		return map[string]any{"ok": true, "slot": n}, nil
	})
}

func browsersError(err error) error {
	var busy *browsers.BusyError
	switch {
	case errors.As(err, &busy):
		return Errorf(http.StatusConflict, "%s", err.Error())
	case errors.Is(err, browsers.ErrNone), errors.Is(err, browsers.ErrNoBinary):
		return Errorf(http.StatusServiceUnavailable, "%s", err.Error())
	case errors.Is(err, browsers.ErrNotTaken), errors.Is(err, browsers.ErrNoSuchOne):
		return Errorf(http.StatusNotFound, "%s", err.Error())
	case errors.Is(err, browsers.ErrNotYours):
		return Errorf(http.StatusForbidden, "%s", err.Error())
	}
	return err
}
