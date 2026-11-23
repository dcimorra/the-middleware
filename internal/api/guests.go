package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/dcimorra/the-middleware/internal/auth"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/guests"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/sites"
)

// Who a site takes in (internal/guests). The routes are under the site
// that is entered, so a caller confined to a site (a delegate, a turn)
// reaches only its own list: there is no route by which the site that
// enters asks for anything. The list is the manifest's `guests`, written
// through the same patch as the rest of it.
func (s *Server) registerGuests() {
	p := Prefix + "/sites/{domain}/guests"
	s.Handle("GET", p, "The sites this one takes in (guests: site, level read or write, whether a site of this node has that name), the sites that take this one in (hosts), and the sites the caller may add (candidates)", s.guestsGet)
	s.Handle("PUT", p+"/{guest}", "Take another site of this node in (body: level, \"read\" or \"write\"; default read): the guest gets this site's folder in its own jail, read-only or writable, and its conversation the house's commands about this site", s.guestsPut)
	s.Handle("DELETE", p+"/{guest}", "Stop taking a site in", s.guestsDelete)
}

// guestRow is one line of a site's list.
type guestRow struct {
	Site  string `json:"site"`
	Level string `json:"level"`
	// Here is whether a site of this node carries the name now. A pass
	// for a name nobody carries lends nothing until a site does, and is
	// shown as such: it would be that site's from its first turn.
	Here bool `json:"here"`
}

type guestsView struct {
	Guests []guestRow     `json:"guests"`
	Hosts  []guests.Grant `json:"hosts"`
	// Candidates are the sites the caller may add: every other site of
	// this node for the operator, a delegate's own for a delegate.
	Candidates []string `json:"candidates"`
}

func (s *Server) guestsView(r *http.Request, m *sites.Manager, domain string) (guestsView, error) {
	if _, err := m.Get(domain); err != nil {
		if errors.Is(err, sites.ErrNotFound) {
			return guestsView{}, Errorf(http.StatusNotFound, "%v", err)
		}
		return guestsView{}, err
	}
	v := guestsView{Guests: []guestRow{}, Hosts: guests.Of(s.opts.Root, domain), Candidates: []string{}}
	if v.Hosts == nil {
		v.Hosts = []guests.Grant{}
	}
	in := guests.In(m.Dir(domain))
	local := map[string]bool{}
	all, _ := m.List()
	for _, st := range all {
		local[st.Domain] = true
	}
	for _, name := range manifest.GuestNames(in) {
		v.Guests = append(v.Guests, guestRow{Site: name, Level: in[name], Here: local[name]})
	}
	c := auth.FromContext(r.Context())
	if c.Turn != "" || (len(c.Scopes) > 0 && !c.Delegate()) {
		// A turn and a scoped token read the list and add nobody: the
		// other sites of the house are not theirs to be told about.
		return v, nil
	}
	for name := range local {
		if name == domain || in[name] != "" {
			continue
		}
		if c.Delegate() {
			if _, mine := c.Grants[name]; !mine {
				continue
			}
		}
		v.Candidates = append(v.Candidates, name)
	}
	sort.Strings(v.Candidates)
	return v, nil
}

func (s *Server) guestsGet(w http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	return s.guestsView(r, m, domain)
}

func (s *Server) guestsPut(w http.ResponseWriter, r *http.Request) (any, error) {
	var body struct {
		Level string `json:"level"`
	}
	if err := ReadJSON(r, &body); err != nil {
		return nil, err
	}
	if body.Level == "" {
		body.Level = manifest.GuestRead
	}
	if body.Level != manifest.GuestRead && body.Level != manifest.GuestWrite {
		return nil, Errorf(http.StatusBadRequest, "level: want %q or %q, got %q", manifest.GuestRead, manifest.GuestWrite, body.Level)
	}
	return s.setGuest(r, body.Level)
}

func (s *Server) guestsDelete(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.setGuest(r, "")
}

// setGuest writes one guest of the site in the path, level "" to take it
// out. A guest that is added must be a site of this node the caller may
// name: a delegate names only their own, and is answered for any other
// as for one that does not exist, so the list is no way to learn which
// sites the house holds.
func (s *Server) setGuest(r *http.Request, level string) (any, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	domain, err := domainOf(r)
	if err != nil {
		return nil, err
	}
	guest := strings.ToLower(strings.TrimSpace(r.PathValue("guest")))
	if !manifest.ValidName(guest) {
		return nil, Errorf(http.StatusBadRequest, "%q is not a site or project", guest)
	}
	if _, err := m.Get(domain); err != nil {
		if errors.Is(err, sites.ErrNotFound) {
			return nil, Errorf(http.StatusNotFound, "%v", err)
		}
		return nil, err
	}
	in := guests.In(m.Dir(domain))
	if in == nil {
		in = map[string]string{}
	}
	was := in[guest]
	if level == "" {
		if was == "" {
			return nil, Errorf(http.StatusNotFound, "%s does not take %s in", domain, guest)
		}
		delete(in, guest)
	} else {
		if guest == domain {
			return nil, Errorf(http.StatusBadRequest, "a site is not its own guest")
		}
		c := auth.FromContext(r.Context())
		_, mine := c.Grants[guest]
		if _, err := m.Get(guest); err != nil || (c.Delegate() && !mine) {
			return nil, Errorf(http.StatusNotFound, "%s: no such site on this node. A site takes in the sites of its own node", guest)
		}
		in[guest] = level
	}
	raw := []byte("null")
	if len(in) > 0 {
		if raw, err = json.Marshal(in); err != nil {
			return nil, err
		}
	}
	if _, err := m.Patch(domain, map[string]json.RawMessage{"guests": raw}); err != nil {
		return nil, err
	}
	if s.opts.Bus != nil && was != level {
		typ, msg := "guests.added", guest+" may now enter this site to "+level
		switch {
		case level == "":
			typ, msg = "guests.removed", guest+" may no longer enter this site"
		case was != "":
			typ, msg = "guests.changed", guest+" enters this site to "+level+" (it was "+was+")"
		}
		s.opts.Bus.Emit(events.Event{Type: typ, Subject: domain, Actor: actorOf(r), Message: msg, Data: map[string]any{"guest": guest, "level": level}})
	}
	return s.guestsView(r, m, domain)
}
