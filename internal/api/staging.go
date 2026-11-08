package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/inbound"
	"github.com/dcimorra/the-middleware/internal/sites"
)

// A test copy is a site of its own and is the owner's to make, like any
// new site; see internal/sites, staging.go.
func (s *Server) registerStaging() {
	p := Prefix + "/sites/{domain}/staging"
	s.Handle("GET", p, "The site's test copies, or the production it is a copy of; what a promotion keeps; whether an undo is there", s.stagingGet)
	s.Handle("POST", p, "Make a test copy of the site (body: name, default staging.<domain>; publish): its files and databases as they are now, behind a password", s.stagingCreate)
	s.Handle("POST", p+"/promote", "Put this test copy's code into its production site: production's manifest and data (staging.keep) stay; the folder it replaces is kept for an undo", s.stagingPromote)
	s.Handle("POST", p+"/undo", "Put back the folder the last promotion into this site replaced, with the data written since", s.stagingUndo)
	s.Handle("POST", p+"/refresh", "Copy production into this test copy again, data too; the copy keeps its manifest", s.stagingRefresh)
}

func (s *Server) stagingGet(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	return m.Staging(domain)
}

func (s *Server) stagingCreate(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	var body struct {
		Name    string       `json:"name"`
		Publish *bool        `json:"publish"`
		Door    inbound.Door `json:"door"`
	}
	if r.ContentLength != 0 {
		if err := ReadJSON(r, &body); err != nil {
			return nil, err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	res, err := m.CreateStaging(ctx, domain, body.Name)
	if err != nil {
		return nil, stagingError(err)
	}
	out := map[string]any{}
	raw, _ := json.Marshal(res)
	_ = json.Unmarshal(raw, &out)
	want := s.dnsEnabled != nil && s.dnsEnabled()
	if body.Publish != nil {
		want = *body.Publish
	}
	if name := res.Site.Domain; want && s.publisher != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if _, err := s.publisher.Publish(ctx, name, body.Door); err != nil {
				s.opts.Bus.Emit(events.Event{Type: "publish.failed", Subject: name, Actor: "operator", Level: "error", Message: err.Error()})
			}
		}()
		out["publishing"] = true
	}
	WriteJSON(w, http.StatusCreated, out)
	return Handled, nil
}

func (s *Server) stagingPromote(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.stagingDo(r, (*sites.Manager).PromoteStaging)
}

func (s *Server) stagingUndo(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.stagingDo(r, (*sites.Manager).UndoPromotion)
}

func (s *Server) stagingRefresh(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.stagingDo(r, (*sites.Manager).RefreshStaging)
}

func (s *Server) stagingDo(r *http.Request, do func(*sites.Manager, context.Context, string) (sites.StagingResult, error)) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	// The house's context, not the request's: a copy cut halfway by a
	// client that left is a folder half built for nothing.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	res, err := do(m, ctx, domain)
	if err != nil {
		return nil, stagingError(err)
	}
	return res, nil
}

func stagingError(err error) error {
	switch {
	case errors.Is(err, sites.ErrNotFound):
		return Errorf(http.StatusNotFound, "%v", err)
	case errors.Is(err, sites.ErrBadDomain), errors.Is(err, sites.ErrInvalid):
		return Errorf(http.StatusBadRequest, "%v", err)
	}
	return Errorf(http.StatusConflict, "%v", err)
}
