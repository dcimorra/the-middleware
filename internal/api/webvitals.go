package api

import (
	"net/http"

	"github.com/dcimorra/the-middleware/internal/webvitals"
)

// RegisterWebVitals mounts what browsers measured of a site's pages.
func (s *Server) RegisterWebVitals(b *webvitals.Book) {
	s.Handle("GET", Prefix+"/sites/{domain}/webvitals", "How the site's pages feel in its visitors' browsers (LCP, INP, CLS, FCP, TTFB at the 75th percentile, by day, device and page), when the manifest has web_vitals on. ?month=YYYY-MM or ?days=N (30 by default)", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		from, to, err := periodOf(r)
		if err != nil {
			return nil, err
		}
		return b.Report(domain, from, to), nil
	})
}
