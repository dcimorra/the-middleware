package api

import (
	"errors"
	"net/http"

	"github.com/dcimorra/the-middleware/internal/engine"
)

func (s *Server) registerCanary() {
	p := Prefix + "/sites/{domain}/canary"
	s.Handle("GET", p, "The site's canary: the one running now (its share, its end, requests and errors on each side) and the last generation a canary rolled back", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		sn, ok := s.opts.Engine.Site(domain)
		if !ok {
			return nil, Errorf(http.StatusNotFound, "%s: no such site", domain)
		}
		out := map[string]any{"running": sn.Canary, "failed": nil}
		if f, ok := s.opts.Engine.CanaryFailed(domain); ok {
			out["failed"] = f
		}
		return out, nil
	})
	decide := func(promote bool) func(http.ResponseWriter, *http.Request) (any, error) {
		return func(w http.ResponseWriter, r *http.Request) (any, error) {
			domain, err := domainOf(r)
			if err != nil {
				return nil, err
			}
			if err := s.opts.Engine.DecideCanary(domain, promote); err != nil {
				if errors.Is(err, engine.ErrNoCanary) {
					return nil, Errorf(http.StatusConflict, "%s: %v", domain, err)
				}
				return nil, err
			}
			return map[string]any{"ok": true}, nil
		}
	}
	s.Handle("POST", p+"/promote", "Hand the running canary all the traffic now", decide(true))
	s.Handle("POST", p+"/rollback", "Stop the running canary now; the serving generation keeps everything and the canary's revision is not started again until a restart or a new revision", decide(false))
}
