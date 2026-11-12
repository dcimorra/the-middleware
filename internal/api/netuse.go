package api

import (
	"net/http"

	"github.com/dcimorra/the-middleware/internal/netuse"
)

// RegisterNetUse mounts what the sites move over the network. known are
// the sites there are, for the house's summary.
func (s *Server) RegisterNetUse(b *netuse.Book, known func() []string) {
	s.Handle("GET", Prefix+"/sites/{domain}/network", "The site's network use: bytes in and out as a rate now (bytes per second, and the last minute second by second), today by the hour, this month, and day by day over ?days=N (30 by default) or ?month=YYYY-MM; the part that came by its public ports; its budget (limits.network) and where it stands in it (state: ok, warn, over, cut)", func(w http.ResponseWriter, r *http.Request) (any, error) {
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
	s.Handle("GET", Prefix+"/network", "Every site's network use on this node, the one that moved most this month first: rate now, today, this month, budget and state", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return map[string]any{"sites": b.Summary(known())}, nil
	})
}
