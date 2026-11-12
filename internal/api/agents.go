package api

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/agent"
	"github.com/dcimorra/the-middleware/internal/auth"
	"github.com/dcimorra/the-middleware/internal/chat"
	"github.com/dcimorra/the-middleware/internal/settings"
)

// AgentsOptions wire the agents section.
type AgentsOptions struct {
	Registry *agent.Registry
	Chat     *chat.Manager
	// Usage is every subscription's windows, as the agent modules know
	// them (refresh asks the vendors past their cache), or only provider's
	// when it is not empty; nil is none.
	Usage func(ctx context.Context, refresh bool, provider string) any
}

// AgentInfo is one provider as GET /agents lists it.
type AgentInfo struct {
	Provider  string   `json:"provider"`
	Available bool     `json:"available"`
	Where     string   `json:"where,omitempty"`
	Models    []string `json:"models"`
	// Labels say what a model name that does not say it runs: Claude's
	// "sonnet" is claude-sonnet-5-5 today.
	Labels map[string]string `json:"labels,omitempty"`
}

// Agents is what GET /agents answers: the providers and the live capacity.
type Agents struct {
	Providers []AgentInfo    `json:"providers"`
	Capacity  *chat.Capacity `json:"capacity,omitempty"`
	// Computed is what the settings' empty fields stand for on this
	// machine, so the panel can show the number in force and where it
	// came from.
	Computed *ComputedAgents `json:"computed,omitempty"`
}

// ComputedAgents are the agents' numbers this machine computes.
type ComputedAgents struct {
	CPUs          int   `json:"cpus"`
	Memory        int64 `json:"memory"`
	MaxConcurrent int   `json:"max_concurrent"`
	CPUCores      int   `json:"cpu_cores"`
	MemoryMax     int64 `json:"memory_max"`
}

// computedAgents is the machine's numbers for a section: the cap follows
// the agents' memory ceiling in force, the operator's if set.
func computedAgents(a settings.Agents) *ComputedAgents {
	m := settings.Machine()
	l := m.DefaultLimits()
	n, _ := settings.Agents{MemoryMax: a.MemoryMax}.Concurrency(m)
	return &ComputedAgents{CPUs: m.CPUs, Memory: m.Memory, MaxConcurrent: n, CPUCores: l.CPUCores, MemoryMax: l.MemoryMax}
}

// RegisterAgents adds /agents, the per-site conversations and the
// installation's own one. The two conversation sets are the same handlers
// over different names: chat.Middleware is not a valid domain, which is
// what keeps the routes from colliding.
func (s *Server) RegisterAgents(o AgentsOptions) {
	if o.Chat != nil {
		s.AddSummary("chats", func() any { return o.Chat.Waiting() })
	}
	providers := func() []AgentInfo {
		out := []AgentInfo{}
		if o.Registry != nil {
			for _, key := range o.Registry.Keys() {
				p, err := o.Registry.Get(key)
				if err != nil {
					continue
				}
				ok, where := p.Available()
				models := p.Models()
				if models == nil {
					models = []string{}
				}
				info := AgentInfo{Provider: key, Available: ok, Where: where, Models: models}
				if l, isL := p.(agent.Labeler); isL {
					info.Labels = l.ModelLabels()
				}
				out = append(out, info)
			}
		}
		return out
	}
	s.Handle("GET", Prefix+"/agents", "The agent providers, whether each can run and its models, with the live capacity", func(w http.ResponseWriter, r *http.Request) (any, error) {
		out := Agents{Providers: providers()}
		if o.Chat != nil {
			c := o.Chat.Capacity()
			out.Capacity = &c
		}
		out.Computed = computedAgents(s.opts.Settings().Agents)
		return out, nil
	})
	// The providers a site's conversation can use, from the node that
	// holds the site: what a delegate's panel asks, since /agents is the
	// house's (its capacity names every conversation at work) and this
	// node's, which may not be the one the site's turns run on.
	s.Handle("GET", Prefix+"/sites/{domain}/agents", "The agent providers a conversation of this site can use, with their models, without the house's capacity", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if _, err := domainOf(r); err != nil {
			return nil, err
		}
		all := providers()
		c := auth.FromContext(r.Context())
		if !c.Delegate() || c.Agents == nil {
			return Agents{Providers: all}, nil
		}
		// A delegate is shown only what their limits leave them, so the
		// picker offers nothing the send would refuse.
		out := []AgentInfo{}
		for _, p := range all {
			if !auth.AgentAllowed(c.Agents, p.Provider, "") {
				continue
			}
			if allowed := c.Agents[p.Provider]; len(allowed) > 0 {
				p.Models = allowed
			}
			out = append(out, p)
		}
		return Agents{Providers: out}, nil
	})
	if o.Chat == nil {
		return
	}
	s.Handle("GET", Prefix+"/agents/capacity", "How many agent turns run now, of how many, per provider, and which conversations hold them", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return o.Chat.Capacity(), nil
	})
	s.Handle("GET", Prefix+"/agents/waiting", "Whose turn it is to speak: the conversations waiting for the operator's answer, and how many turns are at work", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return o.Chat.Waiting(), nil
	})
	// The bar's list of conversations, open on every screen: the operator's
	// way back to one without going node, site, chat and list. Not a
	// delegate's route (auth.userRoutes): theirs names every site.
	s.Handle("GET", Prefix+"/agents/recent", "The conversations last used on this node, the last first, of every site and of the house (?limit=15); beyond the limit, every one at work or waiting for an answer", func(w http.ResponseWriter, r *http.Request) (any, error) {
		limit := 15
		if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 200 {
			limit = v
		}
		return o.Chat.Recent(limit), nil
	})
	s.Handle("DELETE", Prefix+"/agents/chats", "Forget every conversation the panel holds on this node, of every site and of the house, with their attachments; the ones at work stay and are counted (busy), and so do the ones a site's code opened", func(w http.ResponseWriter, r *http.Request) (any, error) {
		n, busy, err := o.Chat.PurgeAll()
		if err != nil {
			return nil, chatError(err)
		}
		return map[string]int{"purged": n, "busy": busy}, nil
	})
	s.Handle("GET", Prefix+"/agents/usage", "Each subscription's usage windows (Claude, Codex, Antigravity…): how much is spent and when it resets (?provider=agy for one, ?refresh=1 asks the vendors now)", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if o.Usage == nil {
			return []any{}, nil
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		q := r.URL.Query()
		return o.Usage(ctx, q.Get("refresh") == "1", q.Get("provider")), nil
	})
	s.Handle("GET", Prefix+"/agents/cost", "What each project spent on agents, per day, model and turns (?days=30)", func(w http.ResponseWriter, r *http.Request) (any, error) {
		days := 30
		if v, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && v > 0 && v <= 400 {
			days = v
		}
		since := time.Now().UTC().AddDate(0, 0, -days+1).Format("2006-01-02")
		out := []map[string]any{}
		l := o.Chat.Ledger()
		for _, book := range l.Books() {
			f := l.File(book)
			dayCost := map[string]float64{}
			total := 0.0
			turns := 0
			models := map[string]float64{}
			for d, v := range f.Days {
				if d >= since {
					dayCost[d] = v
					total += v
					turns += f.Turns[d]
					for mdl, c := range f.Models[d] {
						models[mdl] += c
					}
				}
			}
			if total == 0 {
				continue
			}
			out = append(out, map[string]any{"book": book, "total_usd": round4(total), "turns": turns, "days": dayCost, "models": models})
		}
		sort.Slice(out, func(i, j int) bool { return out[i]["total_usd"].(float64) > out[j]["total_usd"].(float64) })
		return map[string]any{"days": days, "books": out}, nil
	})
	// A new cap applies to the next turn, not to the next restart.
	s.OnSettingsSaved(func(st settings.Settings) { o.Chat.SetLimits(st.Agents) })
	s.registerChat(o.Chat, Prefix+"/sites/{domain}/chat", domainOf)
	s.registerChat(o.Chat, Prefix+"/chat/middleware", func(*http.Request) (string, error) { return chat.Middleware, nil })
	// More conversations per site: the panel's own beyond the first, and
	// the ones the site's code opened through the site door, which the
	// panel reads and stops but does not write into (a program is on the
	// other side of those, and its turns follow its own rules).
	s.registerChats(o.Chat, Prefix+"/sites/{domain}", domainOf)
	s.registerChats(o.Chat, Prefix+"/chat/middleware", func(*http.Request) (string, error) { return chat.Middleware, nil })
}

// registerChats is the conversations of a site, or of the house, beyond
// the first: the list, a new one, each by its id, and the purge.
func (s *Server) registerChats(m *chat.Manager, base string, domainOf func(*http.Request) (string, error)) {
	s.registerChat(m, base+"/chats/{id}", func(r *http.Request) (string, error) {
		domain, err := domainOf(r)
		if err != nil {
			return "", err
		}
		name := chat.Name(domain, r.PathValue("id"))
		if _, err := m.Conversation(name); err != nil || r.PathValue("id") == "" {
			return "", Errorf(http.StatusNotFound, "no such conversation")
		}
		return name, nil
	})
	s.Handle("GET", base+"/chats", "The other conversations, the last used first: the panel's and the ones the site's code opened (?all=1 includes those the manifest hides, ?main=1 the first conversation too, with an empty id)", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		out := []chat.Summary{}
		for _, sm := range m.List(domain) {
			if sm.Hidden && !isTrue(r.URL.Query().Get("all")) {
				continue
			}
			out = append(out, sm)
		}
		if isTrue(r.URL.Query().Get("main")) {
			if sm, ok := m.Main(domain); ok {
				out = append(out, sm)
				sort.SliceStable(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
			}
		}
		return out, nil
	})
	s.Handle("POST", base+"/chats", "Open another conversation (body: title?); the panel opens it when its first message goes, and one without messages is not listed", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		var body struct {
			Title string `json:"title"`
		}
		if r.ContentLength != 0 {
			if err := ReadJSON(r, &body); err != nil {
				return nil, err
			}
		}
		name, err := m.Create(domain, chat.CreateOptions{Origin: chat.OriginPanel, Title: body.Title})
		if err != nil {
			return nil, chatError(err)
		}
		_, id, _ := strings.Cut(name, "#")
		WriteJSON(w, http.StatusCreated, map[string]string{"name": name, "id": id})
		return Handled, nil
	})
	s.Handle("DELETE", base+"/chats", "Forget every conversation the panel holds, the first included, with their attachments; the site's code's own stay. 409 while one is at work", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		n, err := m.Purge(domain)
		if err != nil {
			return nil, chatError(err)
		}
		return map[string]int{"purged": n}, nil
	})
}

func (s *Server) registerChat(m *chat.Manager, p string, name func(*http.Request) (string, error)) {
	s.Handle("GET", p, "The conversation so far", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := name(r)
		if err != nil {
			return nil, err
		}
		c, err := m.History(domain)
		if err != nil {
			return nil, chatError(err)
		}
		return c, nil
	})
	// The conversation as a file, to hand to someone (or another agent)
	// to read: every turn with its steps and its subagents' steps, which
	// the panel shows folded and a copy and paste of the screen loses.
	s.Handle("GET", p+"/export", "The conversation as a JSON file to download: every turn, its steps and its subagents'", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := name(r)
		if err != nil {
			return nil, err
		}
		c, err := m.History(domain)
		if err != nil {
			return nil, chatError(err)
		}
		now := time.Now().UTC()
		file := strings.NewReplacer("#", "-", "/", "-").Replace(domain) + "-chat-" + now.Format("20060102-150405") + ".json"
		w.Header().Set("Content-Disposition", `attachment; filename="`+file+`"`)
		WriteJSON(w, http.StatusOK, map[string]any{"exported": now, "version": s.opts.Version, "conversation": c})
		return Handled, nil
	})
	s.Handle("POST", p, "Send a message; progress arrives on the event stream as agent.*", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := name(r)
		if err != nil {
			return nil, err
		}
		var body struct {
			Text     string `json:"text"`
			Model    string `json:"model"`
			Provider string `json:"provider"`
		}
		if err := ReadJSON(r, &body); err != nil {
			return nil, err
		}
		if c, err := m.Conversation(domain); err == nil && c.Origin == chat.OriginSite {
			return nil, Errorf(http.StatusConflict, "this conversation belongs to the site's code; open one of your own to talk about the site")
		}
		opts := chat.SendOptions{Model: body.Model, Provider: body.Provider, Queue: true}
		if c := auth.FromContext(r.Context()); c.Delegate() && c.Agents != nil {
			opts.Allow = func(provider, model string, known []string) (string, error) {
				return auth.PickAgentModel(c.Agents, provider, model, known)
			}
		}
		id, err := m.SendWith(domain, body.Text, opts)
		if errors.Is(err, chat.ErrQueued) {
			WriteJSON(w, http.StatusAccepted, map[string]any{"queued": true})
			return Handled, nil
		}
		if err != nil {
			return nil, chatError(err)
		}
		return map[string]string{"turn_id": id}, nil
	})
	s.Handle("POST", p+"/stop", "Stop the running turn", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := name(r)
		if err != nil {
			return nil, err
		}
		if err := m.Stop(domain); err != nil {
			return nil, Errorf(http.StatusConflict, "%v", err)
		}
		return OK, nil
	})
	s.Handle("POST", p+"/send-now", "Cut the running turn so the queued messages go now (what it said is kept)", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := name(r)
		if err != nil {
			return nil, err
		}
		if err := m.SendNow(domain); err != nil {
			return nil, Errorf(http.StatusConflict, "%v", err)
		}
		return OK, nil
	})
	s.Handle("DELETE", p, "Forget the conversation; the next message starts a new one", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := name(r)
		if err != nil {
			return nil, err
		}
		if err := m.Reset(domain); err != nil {
			return nil, chatError(err)
		}
		return OK, nil
	})
	s.Handle("GET", p+"/files", "The attachments that go with the next message", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := name(r)
		if err != nil {
			return nil, err
		}
		return m.Uploads(domain), nil
	})
	s.Handle("POST", p+"/files", "Attach a file to the next message (raw body, ?name=file.ext, 25 MB at most)", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := name(r)
		if err != nil {
			return nil, err
		}
		up, err := m.AddUpload(domain, r.URL.Query().Get("name"), http.MaxBytesReader(w, r.Body, chat.MaxUpload+1))
		if err != nil {
			return nil, Errorf(http.StatusBadRequest, "%v", err)
		}
		return up, nil
	})
	s.Handle("DELETE", p+"/files/{file}", "Drop an attachment before it is sent", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := name(r)
		if err != nil {
			return nil, err
		}
		if err := m.RemoveUpload(domain, r.PathValue("file")); err != nil {
			return nil, Errorf(http.StatusNotFound, "%v", err)
		}
		return OK, nil
	})
	s.Handle("GET", p+"/processes", "What the conversation has running now, also what escaped its turn with setsid", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := name(r)
		if err != nil {
			return nil, err
		}
		procs := m.Processes(domain)
		if procs == nil {
			procs = []chat.Proc{}
		}
		return procs, nil
	})
	s.Handle("POST", p+"/reap", "Stop the turn and kill everything the conversation left running", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := name(r)
		if err != nil {
			return nil, err
		}
		return map[string]int{"killed": m.Reap(domain)}, nil
	})
	s.Handle("POST", p+"/processes/{pid}/kill", "Kill one process the conversation left; any other pid is refused", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := name(r)
		if err != nil {
			return nil, err
		}
		pid, err := strconv.Atoi(r.PathValue("pid"))
		if err != nil || pid <= 1 {
			return nil, Errorf(http.StatusBadRequest, "pid: not a process id")
		}
		if !m.Kill(domain, pid) {
			return nil, Errorf(http.StatusNotFound, "%d is not a process of this conversation", pid)
		}
		return OK, nil
	})
}

// chatError maps the manager's errors: a busy conversation is 409, a
// full house 429, an unknown site 404, an empty message or an unknown
// provider 400, a message its agent's command line cannot carry 413 (the
// same answer every time, and a 5xx is what a client tries again).
func chatError(err error) error {
	msg := err.Error()
	var capacity *chat.ErrCapacity
	switch {
	case errors.Is(err, chat.ErrBusy):
		return Errorf(http.StatusConflict, "%s", msg)
	case errors.As(err, &capacity):
		return Errorf(http.StatusTooManyRequests, "%s", msg)
	case errors.Is(err, auth.ErrAgentNotAllowed):
		return Errorf(http.StatusForbidden, "%s", msg)
	case errors.Is(err, agent.ErrUnknownProvider):
		return Errorf(http.StatusBadRequest, "%s", msg)
	case errors.Is(err, agent.ErrTooLong):
		return Errorf(http.StatusRequestEntityTooLarge, "%s", msg)
	case strings.HasSuffix(msg, "no such site"), strings.HasSuffix(msg, "is not a domain"):
		return Errorf(http.StatusNotFound, "%s", msg)
	case msg == "empty message":
		return Errorf(http.StatusBadRequest, "%s", msg)
	}
	return err
}
