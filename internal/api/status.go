package api

import (
	"net/http"

	"github.com/dcimorra/the-middleware/internal/status"
)

// RegisterStatus mounts a site's availability as the house counted it.
func (s *Server) RegisterStatus(b *status.Book) {
	s.Handle("GET", Prefix+"/sites/{domain}/uptime", "The site's availability as the house counted it, minute by minute: state now, uptime over 1, 7, 30 and 90 days, a row per day and the outages with what the house knew of why. The public status page (manifest status_page) shows the same without the reasons", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		return b.Report(domain), nil
	})
}
