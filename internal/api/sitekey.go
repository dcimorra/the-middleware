package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/dcimorra/the-middleware/internal/access"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/sites"
)

// The site's key (internal/access/key.go) is made here and not by the
// panel's page: the page may be plain http through an SSH tunnel, where
// the browser has no SHA-256 to hash it with, and the key must come from
// a source of randomness the house trusts. The key is in the answer and
// nowhere else; the manifest keeps its hash.
func (s *Server) registerSiteKey() {
	p := Prefix + "/sites/{domain}/access/key"
	s.Handle("POST", p, "Hide the site behind a new key in its address; the key is in this answer only, and an earlier one stops working", s.siteKeyOn)
	s.Handle("DELETE", p, "Stop hiding the site behind a key", s.siteKeyOff)
}

func (s *Server) siteKeyOn(w http.ResponseWriter, r *http.Request) (any, error) {
	key, stored := access.NewKey()
	site, err := s.setSiteKey(r, stored)
	if err != nil {
		return nil, err
	}
	return map[string]any{"key": key, "param": access.KeyParam, "url": "https://" + site + "/?" + access.KeyParam + "=" + key}, nil
}

func (s *Server) siteKeyOff(w http.ResponseWriter, r *http.Request) (any, error) {
	if _, err := s.setSiteKey(r, ""); err != nil {
		return nil, err
	}
	return OK, nil
}

// setSiteKey writes access.key through the same patch the access form
// saves with, so the rest of the section stays as it is.
func (s *Server) setSiteKey(r *http.Request, stored string) (string, error) {
	m, err := s.sitesManager()
	if err != nil {
		return "", err
	}
	domain, err := domainOf(r)
	if err != nil {
		return "", err
	}
	cur, err := m.Get(domain)
	if err != nil {
		if errors.Is(err, sites.ErrNotFound) {
			return "", Errorf(http.StatusNotFound, "%v", err)
		}
		return "", err
	}
	ac := manifest.Access{}
	if cur.Manifest != nil && cur.Manifest.Access != nil {
		ac = *cur.Manifest.Access
	}
	ac.Key = stored
	raw, err := json.Marshal(ac)
	if err != nil {
		return "", err
	}
	if string(raw) == "{}" {
		raw = []byte("null")
	}
	if _, err := m.Patch(domain, map[string]json.RawMessage{"access": raw}); err != nil {
		return "", err
	}
	return domain, nil
}
