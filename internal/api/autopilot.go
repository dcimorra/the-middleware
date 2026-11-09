package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/auth"
	"github.com/dcimorra/the-middleware/internal/autopilot"
	"github.com/dcimorra/the-middleware/internal/chat"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/settings"
)

// RegisterAutopilot adds the autonomous agent's routes: what the operator
// switches, reads and approves, and the few the agent's own turns call
// (auth.AutopilotScopes). What is the operator's refuses the agent here
// too, not only by its scopes: a route added to its scope by mistake must
// still not let it loosen its own brakes.
func (s *Server) RegisterAutopilot(p *autopilot.Pilot, m *chat.Manager) {
	s.mu.Lock()
	s.pilot = p
	s.mu.Unlock()
	operator := func(r *http.Request) error {
		if c := auth.FromContext(r.Context()); c.Autopilot || c.Turn != "" || c.Delegate() || len(c.Scopes) > 0 {
			return Errorf(http.StatusForbidden, "only the operator decides about the autonomous agent")
		}
		return nil
	}
	base := Prefix + "/autopilot"
	s.Handle("GET", base, "The autonomous agent: on or off, its limits, where it stands (context, next wake, site turns at work, sites it made), the subscription's windows, its notebook and guidelines", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return p.Status(), nil
	})
	s.Handle("GET", base+"/journal", "The autonomous agent's journal, oldest first (?limit=N, 200 by default): every wake, turn, command, site, call and refusal", func(w http.ResponseWriter, r *http.Request) (any, error) {
		n := 200
		if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
			n = min(v, 5000)
		}
		return p.Journal(n), nil
	})
	s.Handle("PUT", base+"/settings", "Replace the autonomous agent's settings (settings.autopilot): switch, agent and model, sites a week, names, publishing, wake bounds, the subscription share it may use, the context share that starts a new session", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := operator(r); err != nil {
			return nil, err
		}
		var body settings.Autopilot
		if err := ReadJSON(r, &body); err != nil {
			return nil, err
		}
		for i, z := range body.Zones {
			body.Zones[i] = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(z), "."))
		}
		if err := body.Validate(); err != nil {
			return nil, Errorf(http.StatusBadRequest, "%s", err.Error())
		}
		cur := s.opts.Settings()
		was := cur.Autopilot.Enabled
		if body.Enabled && !was {
			// Asked before anything is written: one agent for the network.
			ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
			err := p.Alone(ctx)
			cancel()
			if err != nil {
				return nil, Errorf(http.StatusConflict, "%v", err)
			}
		}
		cur.Autopilot = body
		if err := s.opts.SaveSettings(cur); err != nil {
			return nil, err
		}
		if body.Enabled != was {
			// The switch's journal line and its "look now".
			if err := p.SetEnabled(body.Enabled); err != nil {
				return nil, err
			}
		}
		s.opts.Bus.Emit(events.Event{Type: "autopilot.settings", Actor: "operator", Message: "the autonomous agent's settings were changed"})
		return p.Status(), nil
	})
	s.Handle("PUT", base+"/guidelines", "Replace the guidelines the autonomous agent follows (body: text, Markdown; empty goes back to the default ones)", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := operator(r); err != nil {
			return nil, err
		}
		var body struct {
			Text string `json:"text"`
		}
		if err := ReadJSON(r, &body); err != nil {
			return nil, err
		}
		if err := p.SetGuidelines(body.Text); err != nil {
			return nil, Errorf(http.StatusBadRequest, "%v", err)
		}
		p.Note(autopilot.Entry{Kind: autopilot.KindOn, Message: "the operator changed the guidelines"})
		return p.Status(), nil
	})
	for _, sw := range []struct {
		path, what string
		on         bool
	}{{"/on", "Switch the autonomous agent on: it looks at once", true}, {"/off", "Switch the autonomous agent off; a turn at work finishes", false}} {
		s.Handle("POST", base+sw.path, sw.what, func(w http.ResponseWriter, r *http.Request) (any, error) {
			if err := operator(r); err != nil {
				return nil, err
			}
			if err := p.SetEnabled(sw.on); err != nil {
				return nil, Errorf(http.StatusConflict, "%v", err)
			}
			return p.Status(), nil
		})
	}
	s.Handle("POST", base+"/stop", "The kill switch: the autonomous agent switched off and its turn and every site turn it started cut now", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := operator(r); err != nil {
			return nil, err
		}
		if err := p.Halt(); err != nil {
			return nil, err
		}
		return p.Status(), nil
	})
	s.Handle("POST", base+"/move", "Move the autonomous agent to another node of the network (body: node, its id or name): its notebook, guidelines, limits and the sites it made go with it, and it is switched off here", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := operator(r); err != nil {
			return nil, err
		}
		var body struct {
			Node string `json:"node"`
		}
		if err := ReadJSON(r, &body); err != nil {
			return nil, err
		}
		if strings.TrimSpace(body.Node) == "" {
			return nil, Errorf(http.StatusBadRequest, "node: which one")
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		if err := p.Move(ctx, strings.TrimSpace(body.Node)); err != nil {
			return nil, Errorf(http.StatusConflict, "%v", err)
		}
		return p.Status(), nil
	})
	s.Handle("POST", base+"/adopt", "Take the autonomous agent from another node (body: what Move sends); the node it leaves calls it over the mesh", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := operator(r); err != nil {
			return nil, err
		}
		var b autopilot.Bundle
		if err := ReadJSON(r, &b); err != nil {
			return nil, err
		}
		if err := b.Limits.Validate(); err != nil {
			return nil, Errorf(http.StatusBadRequest, "%s", err.Error())
		}
		if err := p.Adopt(b); err != nil {
			return nil, err
		}
		return p.Status(), nil
	})
	s.Handle("POST", base+"/wake", "Have the autonomous agent look now, as if its time had come", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := operator(r); err != nil {
			return nil, err
		}
		p.Wake()
		return OK, nil
	})
	s.Handle("POST", base+"/ask", "From the autonomous agent: hand work to a site's conversation (body: domain, text); the answer reaches it on its next wake", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var body struct {
			Domain string `json:"domain"`
			Text   string `json:"text"`
		}
		if err := ReadJSON(r, &body); err != nil {
			return nil, err
		}
		id, err := p.Ask(strings.ToLower(strings.TrimSpace(body.Domain)), body.Text)
		if err != nil {
			p.Note(autopilot.Entry{Kind: autopilot.KindRefused, Domain: body.Domain, Message: "work not handed out: " + err.Error()})
			return nil, Errorf(http.StatusConflict, "%v", err)
		}
		return map[string]string{"turn_id": id}, nil
	})
	s.Handle("GET", base+"/proposals", "What the autonomous agent proposes and the operator decides, newest first: open, sent (to be fixed), fixed, failed or dismissed", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return p.Proposals(), nil
	})
	s.Handle("POST", base+"/proposals", "From the autonomous agent: propose a change it may not make itself, to a site (body: domain, title, text) or to the house (no domain); one open with the same title is rewritten", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var body struct {
			Domain string `json:"domain"`
			Title  string `json:"title"`
			Text   string `json:"text"`
		}
		if err := ReadJSON(r, &body); err != nil {
			return nil, err
		}
		x, err := p.Propose(strings.ToLower(strings.TrimSpace(body.Domain)), body.Title, body.Text)
		if err != nil {
			p.Note(autopilot.Entry{Kind: autopilot.KindRefused, Domain: body.Domain, Message: "proposal not filed: " + err.Error()})
			return nil, Errorf(http.StatusConflict, "%v", err)
		}
		return x, nil
	})
	for _, d := range []struct {
		path, what string
		do         func(id, note string) (autopilot.Proposal, error)
	}{
		{"/send", "Send a proposal of the autonomous agent to be fixed (body: note, optional): a new conversation of its site, or of the house, starts in the operator's name with the proposal as its message", p.Send},
		{"/dismiss", "Dismiss a proposal of the autonomous agent (body: note, optional: why, which it reads)", p.Dismiss},
	} {
		s.Handle("POST", base+"/proposals/{id}"+d.path, d.what, func(w http.ResponseWriter, r *http.Request) (any, error) {
			if err := operator(r); err != nil {
				return nil, err
			}
			var body struct {
				Note string `json:"note"`
			}
			if r.ContentLength != 0 {
				if err := ReadJSON(r, &body); err != nil {
					return nil, err
				}
			}
			x, err := d.do(r.PathValue("id"), body.Note)
			if errors.Is(err, autopilot.ErrNoProposal) {
				return nil, Errorf(http.StatusNotFound, "%v", err)
			}
			if err != nil {
				return nil, Errorf(http.StatusConflict, "%v", err)
			}
			return x, nil
		})
	}
	publishNow := func(domain string) func() error {
		return func() error {
			if s.publisher == nil {
				return Errorf(http.StatusServiceUnavailable, "no publisher: the DNS module is off")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			_, err := s.publisher.Publish(ctx, domain, "")
			return err
		}
	}
	s.Handle("POST", base+"/sites/{domain}/ready", "From the autonomous agent: one of its sites is finished; it is published, or the operator is asked when the settings say ask", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		published, err := p.Ready(domain, publishNow(domain))
		if err != nil {
			return nil, Errorf(http.StatusConflict, "%v", err)
		}
		return map[string]bool{"published": published, "waiting": !published}, nil
	})
	s.Handle("POST", base+"/sites/{domain}/publish", "Publish a site the autonomous agent made and finished", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := operator(r); err != nil {
			return nil, err
		}
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		if !p.Mine(domain) {
			return nil, Errorf(http.StatusNotFound, "%s is not a site the autonomous agent made", domain)
		}
		if err := publishNow(domain)(); err != nil {
			return nil, err
		}
		p.Published(domain, true)
		return p.Status(), nil
	})
	s.Handle("POST", base+"/sites/{domain}/decline", "Keep a site the autonomous agent made off the internet; it is told", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := operator(r); err != nil {
			return nil, err
		}
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		if !p.Mine(domain) {
			return nil, Errorf(http.StatusNotFound, "%s is not a site the autonomous agent made", domain)
		}
		p.Published(domain, false)
		return p.Status(), nil
	})
	if m != nil {
		// Its conversations, one per session: read in the panel, and
		// written into by the operator, who can talk to it there.
		s.registerChats(m, base, func(*http.Request) (string, error) { return chat.Autopilot, nil })
	}
}
