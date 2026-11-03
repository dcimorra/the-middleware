package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/dcimorra/the-middleware/internal/visits"
)

// RegisterVisits mounts a site's audience.
func (s *Server) RegisterVisits(b *visits.Book) {
	s.Handle("GET", Prefix+"/sites/{domain}/visits", "The site's audience: people per day and per month, by country, region, device, browser, system, link and page, robots left out. ?month=YYYY-MM for a calendar month, or ?days=N for the last N days (30 by default)", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		now := time.Now()
		if m := r.URL.Query().Get("month"); m != "" {
			start, err := time.ParseInLocation("2006-01", m, time.Local)
			if err != nil {
				return nil, Errorf(http.StatusBadRequest, "month %q is not YYYY-MM", m)
			}
			return b.Report(domain, start, start.AddDate(0, 1, -1)), nil
		}
		days := 30
		if v := r.URL.Query().Get("days"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 366 {
				return nil, Errorf(http.StatusBadRequest, "days must be between 1 and 366")
			}
			days = n
		}
		return b.Report(domain, now.AddDate(0, 0, 1-days), now), nil
	})
}
