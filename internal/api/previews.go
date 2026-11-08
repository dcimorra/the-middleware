package api

import (
	"net/http"

	"github.com/dcimorra/the-middleware/internal/sites"
)

func (s *Server) registerPreviews() {
	s.Handle("GET", Prefix+"/sites/{domain}/previews", "The site's previews (manifest previews): one test copy per branch pushed to its repository, each with its branch and its last push", func(w http.ResponseWriter, r *http.Request) (any, error) {
		m, err := s.sitesManager()
		if err != nil {
			return nil, err
		}
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		list := m.Previews(domain)
		if list == nil {
			list = []sites.PreviewInfo{}
		}
		return map[string]any{"previews": list}, nil
	})
}
