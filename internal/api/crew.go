package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/dcimorra/the-middleware/internal/auth"
	"github.com/dcimorra/the-middleware/internal/chat"
	"github.com/dcimorra/the-middleware/internal/crew"
)

func (s *Server) crewOf() *crew.Crew {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.crew
}

// RegisterCrew adds the routes of the operator's crew of agents: what the
// operator creates, switches, answers and fills, and the few an agent's
// own turns call (auth.CrewScopes, for its own id only). Everything that
// is the operator's refuses an agent here too, not only by its scope.
func (s *Server) RegisterCrew(c *crew.Crew, m *chat.Manager) {
	s.mu.Lock()
	s.crew = c
	s.mu.Unlock()
	operator := func(r *http.Request) error {
		if cl := auth.FromContext(r.Context()); cl.Crew != "" || cl.Autopilot || cl.Turn != "" || cl.Delegate() || len(cl.Scopes) > 0 {
			return Errorf(http.StatusForbidden, "only the operator decides about the crew")
		}
		return nil
	}
	// mine is the operator or the agent itself (the gate holds an agent
	// to its own {agent} already).
	mine := func(r *http.Request) error {
		if cl := auth.FromContext(r.Context()); cl.Crew != "" || (cl.Autopilot && r.PathValue("agent") == crew.Network) {
			return nil
		}
		return operator(r)
	}
	agentOf := func(r *http.Request) (string, error) {
		id := r.PathValue("agent")
		if !c.Has(id) {
			return "", Errorf(http.StatusNotFound, "%s: no such agent", id)
		}
		return id, nil
	}
	fail := func(err error) error {
		if err == nil {
			return nil
		}
		if errors.Is(err, crew.ErrNoAgent) {
			return Errorf(http.StatusNotFound, "%v", err)
		}
		var e *Error
		if errors.As(err, &e) {
			return err
		}
		return Errorf(http.StatusBadRequest, "%v", err)
	}
	base := Prefix + "/crew"
	s.Handle("GET", base, "The operator's crew of autonomous agents: each one's settings, where it stands and its open requests", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := operator(r); err != nil {
			return nil, err
		}
		return c.List(), nil
	})
	s.Handle("POST", base, "Create an agent (body: name, goal, provider?, model?, min_minutes?, max_minutes?, quota_percent?, context_percent?); it starts off", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := operator(r); err != nil {
			return nil, err
		}
		var body crew.Agent
		if err := ReadJSON(r, &body); err != nil {
			return nil, err
		}
		a, err := c.Create(body)
		if err != nil {
			return nil, fail(err)
		}
		st, _ := c.Get(a.ID)
		WriteJSON(w, http.StatusCreated, st)
		return Handled, nil
	})
	one := base + "/{agent}"
	s.Handle("GET", one, "An agent: its settings and goal, where it stands, the subscription's windows, its requests, the names in its vault, what the house gives it and its notebook", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := mine(r); err != nil {
			return nil, err
		}
		id, err := agentOf(r)
		if err != nil {
			return nil, err
		}
		st, err := c.Get(id)
		return st, fail(err)
	})
	s.Handle("PUT", one, "Replace an agent's settings and goal (the switch is /on and /off)", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := operator(r); err != nil {
			return nil, err
		}
		id, err := agentOf(r)
		if err != nil {
			return nil, err
		}
		var body crew.Agent
		if err := ReadJSON(r, &body); err != nil {
			return nil, err
		}
		if _, err := c.Update(id, body); err != nil {
			return nil, fail(err)
		}
		st, err := c.Get(id)
		return st, fail(err)
	})
	s.Handle("DELETE", one, "Delete an agent: its turn cut, its folder, vault, journal and conversations gone", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := operator(r); err != nil {
			return nil, err
		}
		id, err := agentOf(r)
		if err != nil {
			return nil, err
		}
		if err := c.Delete(id); err != nil {
			return nil, fail(err)
		}
		return map[string]bool{"deleted": true}, nil
	})
	for _, sw := range []struct{ path, what string }{
		{"/on", "Switch an agent on: it looks at once"},
		{"/off", "Switch an agent off; a turn at work finishes"},
		{"/stop", "Stop an agent: switched off and its turn cut now"},
		{"/wake", "Have an agent look now"},
	} {
		s.Handle("POST", one+sw.path, sw.what, func(w http.ResponseWriter, r *http.Request) (any, error) {
			if err := operator(r); err != nil {
				return nil, err
			}
			id, err := agentOf(r)
			if err != nil {
				return nil, err
			}
			switch sw.path {
			case "/on":
				err = c.SetEnabled(id, true)
			case "/off":
				err = c.SetEnabled(id, false)
			case "/stop":
				err = c.Halt(id)
			default:
				err = c.Wake(id)
			}
			if err != nil {
				return nil, Errorf(http.StatusConflict, "%v", err)
			}
			st, err := c.Get(id)
			return st, fail(err)
		})
	}
	s.Handle("GET", one+"/journal", "An agent's journal, oldest first (?limit=N, 200 by default)", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := mine(r); err != nil {
			return nil, err
		}
		id, err := agentOf(r)
		if err != nil {
			return nil, err
		}
		n := 200
		if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
			n = min(v, 5000)
		}
		list, err := c.Journal(id, n)
		return list, fail(err)
	})
	s.Handle("GET", one+"/requests", "An agent's requests to the operator, the open ones first", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := mine(r); err != nil {
			return nil, err
		}
		id, err := agentOf(r)
		if err != nil {
			return nil, err
		}
		list, err := c.Requests(id)
		return list, fail(err)
	})
	s.Handle("POST", one+"/requests", "(The agent) ask the operator for something (body: title, text, fields [{key, secret}], options, urgent); an urgent one reaches their phone at once", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := mine(r); err != nil {
			return nil, err
		}
		id, err := agentOf(r)
		if err != nil {
			return nil, err
		}
		var body crew.Request
		if err := ReadJSON(r, &body); err != nil {
			return nil, err
		}
		req, err := c.Ask(id, body)
		if err != nil {
			return nil, Errorf(http.StatusConflict, "%v", err)
		}
		WriteJSON(w, http.StatusCreated, req)
		return Handled, nil
	})
	s.Handle("POST", one+"/requests/{id}/answer", "Answer an agent's request (body: answer?, choice?, values {KEY: value} into its vault, dismiss?); it is woken within a minute", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := operator(r); err != nil {
			return nil, err
		}
		id, err := agentOf(r)
		if err != nil {
			return nil, err
		}
		var body crew.Reply
		if err := ReadJSON(r, &body); err != nil {
			return nil, err
		}
		req, err := c.Answer(id, r.PathValue("id"), body)
		if err != nil {
			return nil, Errorf(http.StatusConflict, "%v", err)
		}
		return req, nil
	})
	s.Handle("GET", one+"/vault", "The names in an agent's vault, never the values", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := mine(r); err != nil {
			return nil, err
		}
		id, err := agentOf(r)
		if err != nil {
			return nil, err
		}
		st, err := c.Get(id)
		if err != nil {
			return nil, fail(err)
		}
		return st.Vault, nil
	})
	s.Handle("PUT", one+"/vault/{key}", "Set a value in an agent's vault (body: value); encrypted, in its turns' environment from the next one", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := operator(r); err != nil {
			return nil, err
		}
		id, err := agentOf(r)
		if err != nil {
			return nil, err
		}
		var body struct {
			Value string `json:"value"`
		}
		if err := ReadJSON(r, &body); err != nil {
			return nil, err
		}
		if err := c.SetSecret(id, r.PathValue("key"), body.Value); err != nil {
			return nil, fail(err)
		}
		st, err := c.Get(id)
		return st.Vault, fail(err)
	})
	s.Handle("DELETE", one+"/vault/{key}", "Remove a value from an agent's vault", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := operator(r); err != nil {
			return nil, err
		}
		id, err := agentOf(r)
		if err != nil {
			return nil, err
		}
		if err := c.DeleteSecret(id, strings.ToUpper(r.PathValue("key"))); err != nil {
			return nil, fail(err)
		}
		st, err := c.Get(id)
		return st.Vault, fail(err)
	})
	s.Handle("POST", one+"/mail", "Send an email from the agent's account (body: to, subject, body, html?), with its vault's SMTP details", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := mine(r); err != nil {
			return nil, err
		}
		id, err := agentOf(r)
		if err != nil {
			return nil, err
		}
		var body crew.Outgoing
		if err := ReadJSON(r, &body); err != nil {
			return nil, err
		}
		if err := c.SendMail(r.Context(), id, body); err != nil {
			return nil, Errorf(http.StatusBadGateway, "%v", err)
		}
		return map[string]bool{"sent": true}, nil
	})
	s.Handle("GET", one+"/mail", "The agent's inbox, newest first (?unseen=1, ?limit=N up to 100)", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := mine(r); err != nil {
			return nil, err
		}
		id, err := agentOf(r)
		if err != nil {
			return nil, err
		}
		n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		list, err := c.ListMail(r.Context(), id, isTrue(r.URL.Query().Get("unseen")), n)
		if err != nil {
			return nil, Errorf(http.StatusBadGateway, "%v", err)
		}
		return list, nil
	})
	s.Handle("GET", one+"/mail/{uid}", "One message of the agent's inbox, as text, with the links of its HTML; marked as read", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := mine(r); err != nil {
			return nil, err
		}
		id, err := agentOf(r)
		if err != nil {
			return nil, err
		}
		uid, err := strconv.ParseUint(r.PathValue("uid"), 10, 32)
		if err != nil || uid == 0 {
			return nil, Errorf(http.StatusBadRequest, "%q is not a message's uid", r.PathValue("uid"))
		}
		msg, err := c.ReadMail(r.Context(), id, uint32(uid))
		if err != nil {
			return nil, Errorf(http.StatusBadGateway, "%v", err)
		}
		return msg, nil
	})
	if m != nil {
		// Its sessions, read in the panel and written into by the
		// operator, who talks to it there; never by the agent itself.
		s.registerChats(m, one, func(r *http.Request) (string, error) {
			if err := operator(r); err != nil {
				return "", err
			}
			id, err := agentOf(r)
			if err != nil {
				return "", err
			}
			return chat.CrewDomain(id), nil
		})
	}
}
