package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dcimorra/the-middleware/internal/auth"
	"github.com/dcimorra/the-middleware/internal/chat"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/sitetoken"
)

// The site door is the third door of the house: the panel is for whoever
// runs the machine, the API tokens for scripts, and this one for the code
// of a published site. It exists because a site that wanted Claude used
// to write its own engine: find the binary, build the command line, write
// a jail if it remembered, read the JSON at the end, record the cost if it
// felt like it. V1's comparaloo had exactly that file, a second engine of
// conversations on the machine with no resume, no bill and a credential
// that opened everything. What the house already had was one call away.
//
// A site's conversation is a normal conversation: kept, costed, readable
// and stoppable from the panel. It does not notify anyone: a website at
// work is not "your turn". And it cannot reach the panel's conversation of
// the same site, where the machine is discussed with every tool on.
//
// The contract is V1's, field for field and path for path: a site's code
// written against V1 (MIDDLEWARE_API is the bare address, the routes are
// /api/site/…, the answers carry chat, state, text, model, turns,
// cost_usd, json, question) moves here without a line changed. The same
// routes answer under /api/v1/site as well.

// SiteDoorOptions wire the site door.
type SiteDoorOptions struct {
	Chat   *chat.Manager
	Tokens *sitetoken.Keys
	Bus    *events.Bus
	// Lookup returns a site's manifest as the engine has it now.
	Lookup func(domain string) (*manifest.Manifest, bool)
	// SiteDir is the site's folder, for files named in a request.
	SiteDir func(domain string) string
	// HouseDaily is the daily cap of a site whose manifest names none.
	HouseDaily func() float64
	// Ledger reads what a book spent on a day.
	Spent func(book string, at time.Time) float64
	// Available says whether an agent can run on this machine now;
	// nil asks the chat's registry.
	Available func(provider string) bool
	// DefaultModel is the house's model of an agent, for a site that
	// names none; empty lets the agent take its own.
	DefaultModel func(provider string) string
	// Models are the models a request to an agent may name; nil asks
	// the chat's registry.
	Models func(provider string) []string
	// Agents are the agents a door may be given, for the panel: every
	// one that can run now. nil lists the chat's registry.
	Agents func() []string
	// Domains lists the sites, for the usage of doors open at zero.
	Domains func() []string
	// API and Socket are what the panel shows a developer: where the
	// door answers, from the machine and from a container.
	API, Socket string
}

// DefaultMaxChats is how many conversations a site keeps when its manifest
// says nothing.
const DefaultMaxChats = 20

// DefaultDailyTurns caps a day's door turns for an agent whose turns cost
// nothing that can be read (Codex on ChatGPT, Antigravity): the dollar cap
// never trips on those, and traffic, not a person, decides how many come.
const DefaultDailyTurns = 200

// doorRules are a site's limits resolved: what its manifest says and,
// where it is silent, the defaults. One place, so the panel never shows
// one number while the door applies another.
type doorRules struct {
	Provider    string   `json:"provider"`
	Tools       string   `json:"tools"`
	Permissions []string `json:"permissions"`
	DailyUSD    float64  `json:"daily_usd"`
	// DailyTurns is 0 for no cap on turns (Claude reports its cost).
	DailyTurns int    `json:"daily_turns"`
	MaxChats   int    `json:"max_chats"`
	Model      string `json:"model,omitempty"`
	// Models are the ones the site's code may pick from, call by call;
	// empty is any of its agent's.
	Models []string `json:"models,omitempty"`
	Hide   bool     `json:"hide_chats"`
}

// rulesOf resolves a manifest's block. nil is what a door being opened
// starts with: Claude, every permission (the operator's choice: the door
// is opened by someone who knows what the site does, and narrowed from
// there), the house's cap. A block that is there but silent is the door
// as it always was: Claude, and no tools.
func rulesOf(c *manifest.ClaudeAPI, houseDaily float64) doorRules {
	r := doorRules{Provider: "claude", Tools: "write", DailyUSD: houseDaily, MaxChats: DefaultMaxChats}
	if c != nil {
		r.Tools = chat.Level(c.Tools).Name
		if len(c.Permissions) > 0 {
			if name, err := chat.PermsName(c.Permissions); err == nil {
				r.Tools = name
			} else {
				r.Tools = "none"
			}
		}
		if c.Provider != "" {
			r.Provider = c.Provider
		}
		if c.DailyUSD > 0 {
			r.DailyUSD = c.DailyUSD
		}
		if c.MaxChats > 0 {
			r.MaxChats = c.MaxChats
		}
		r.DailyTurns = c.DailyTurns
		r.Model, r.Hide = c.Model, c.HideChats
		r.Models = append([]string(nil), c.Models...)
	}
	if r.DailyTurns == 0 && r.Provider != "claude" {
		r.DailyTurns = DefaultDailyTurns
	}
	r.Permissions = chat.PermsOf(r.Tools)
	return r
}

type doorCaller struct {
	Domain string
	Rules  doorRules
}

// RegisterSiteDoor adds /site and /site/claude/…. The routes are public to
// the API's own authentication and check the site token themselves.
func (s *Server) RegisterSiteDoor(o SiteDoorOptions) {
	s.registerDoorPanel(o)
	p := Prefix + "/site"
	route := func(method, path, summary string, h func(http.ResponseWriter, *http.Request, doorCaller) (any, error)) {
		s.Handle(method, path, summary, func(w http.ResponseWriter, r *http.Request) (any, error) {
			c, err := o.door(r)
			if err != nil {
				return nil, err
			}
			return h(w, r, c)
		})
		s.Public(method, path)
	}
	route("GET", p, "Who this site is and what it may ask: tools, model, limits, spent and left today, conversations open and at work", func(w http.ResponseWriter, r *http.Request, c doorCaller) (any, error) {
		spent := o.spent(c.Domain)
		return map[string]any{"domain": c.Domain, "claude": map[string]any{
			"available":  o.available(c.Rules.Provider),
			"provider":   c.Rules.Provider,
			"model":      o.model(c, ""),
			"models":     o.choices(c),
			"tools":      c.Rules.Tools,
			"limits":     c.Rules,
			"spent_usd":  round4(spent),
			"left_usd":   round4(max(0, c.Rules.DailyUSD-spent)),
			"turns":      o.turns(c.Domain),
			"chats_open": len(o.siteChats(c.Domain)),
			"busy":       o.busy(c.Domain),
		}}, nil
	})
	route("POST", p+"/claude/ask", "One question and its answer in one call (body: prompt, system?, model?, title?, format?: json, schema?, wait?, files?)", func(w http.ResponseWriter, r *http.Request, c doorCaller) (any, error) {
		var b askBody
		if err := ReadJSON(r, &b); err != nil {
			return nil, err
		}
		if b.said() == "" && len(b.Files) == 0 {
			return nil, Errorf(http.StatusBadRequest, "prompt: empty")
		}
		o.fitModel(c.Rules.Provider, &b)
		if err := o.allowedModel(c, b.Model); err != nil {
			return nil, err
		}
		name, err := o.open(c, b)
		if err != nil {
			return nil, err
		}
		return o.sendAndAnswer(w, r, c, name, b)
	})
	route("GET", p+"/claude/chats", "This site's conversations", func(w http.ResponseWriter, r *http.Request, c doorCaller) (any, error) {
		out := []map[string]any{}
		for _, sm := range o.siteChats(c.Domain) {
			if conv, err := o.Chat.Conversation(sm.Name); err == nil {
				out = append(out, infoOf(conv))
			}
		}
		return map[string]any{"chats": out}, nil
	})
	route("POST", p+"/claude/chats", "Open a conversation (body: title?, system?, model?, format?, schema?; prompt? sends a first message)", func(w http.ResponseWriter, r *http.Request, c doorCaller) (any, error) {
		var b askBody
		// No body is fine: open now, talk later.
		if r.ContentLength != 0 {
			if err := ReadJSON(r, &b); err != nil {
				return nil, err
			}
		}
		o.fitModel(c.Rules.Provider, &b)
		if err := o.allowedModel(c, b.Model); err != nil {
			return nil, err
		}
		name, err := o.open(c, b)
		if err != nil {
			return nil, err
		}
		if b.said() == "" && len(b.Files) == 0 {
			conv, err := o.Chat.Conversation(name)
			if err != nil {
				return nil, err
			}
			return map[string]any{"chat": infoOf(conv)}, nil
		}
		return o.sendAndAnswer(w, r, c, name, b)
	})
	route("GET", p+"/claude/chats/{id}", "One conversation: its state, the last answer and its messages (?limit=150, the last ones)", func(w http.ResponseWriter, r *http.Request, c doorCaller) (any, error) {
		name, err := o.siteChat(r, c)
		if err != nil {
			return nil, err
		}
		conv, err := o.Chat.Conversation(name)
		if err != nil {
			return nil, err
		}
		limit := 150
		if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
			limit = v
		}
		msgs := MessagesOf(conv)
		more := false
		if len(msgs) > limit {
			msgs, more = msgs[len(msgs)-limit:], true
		}
		state := "done"
		if conv.Running {
			state = "running"
		}
		return map[string]any{"chat": infoOf(conv), "state": state, "text": said(conv), "messages": msgs, "more": more}, nil
	})
	route("POST", p+"/claude/chats/{id}/messages", "Send the next message (body: prompt, model?, format?, schema?, wait?, files?)", func(w http.ResponseWriter, r *http.Request, c doorCaller) (any, error) {
		name, err := o.siteChat(r, c)
		if err != nil {
			return nil, err
		}
		var b askBody
		if err := ReadJSON(r, &b); err != nil {
			return nil, err
		}
		// The limits are checked here too and not only on opening: a
		// conversation opened yesterday under yesterday's cap must not be
		// the way around today's.
		if !o.Chat.Running(name) {
			if err := o.allowance(c); err != nil {
				return nil, err
			}
		}
		o.fitModel(c.Rules.Provider, &b)
		if err := o.allowedModel(c, b.Model); err != nil {
			return nil, err
		}
		return o.sendAndAnswer(w, r, c, name, b)
	})
	route("GET", p+"/claude/chats/{id}/stream", "The conversation as it happens, as server-sent events: a message per event, and the conversation's state after each turn", func(w http.ResponseWriter, r *http.Request, c doorCaller) (any, error) {
		name, err := o.siteChat(r, c)
		if err != nil {
			return nil, err
		}
		o.stream(w, r, name)
		return Handled, nil
	})
	route("POST", p+"/claude/chats/{id}/files", "Attach a file to the next message: multipart (field \"file\") or the raw body with ?name=; 25 MB at most", func(w http.ResponseWriter, r *http.Request, c doorCaller) (any, error) {
		name, err := o.siteChat(r, c)
		if err != nil {
			return nil, err
		}
		r.Body = http.MaxBytesReader(w, r.Body, chat.MaxUpload+1<<20)
		file, body, err := oneUpload(r)
		if err != nil {
			return nil, Errorf(http.StatusBadRequest, "%v", err)
		}
		defer body.Close()
		up, err := o.Chat.AddUpload(name, file, body)
		if err != nil {
			return nil, Errorf(http.StatusBadRequest, "%v", err)
		}
		return map[string]any{"ok": true, "file": attachmentOf(up)}, nil
	})
	route("DELETE", p+"/claude/chats/{id}/files/{name}", "Drop an attachment before it is sent", func(w http.ResponseWriter, r *http.Request, c doorCaller) (any, error) {
		name, err := o.siteChat(r, c)
		if err != nil {
			return nil, err
		}
		if err := o.Chat.RemoveUpload(name, r.PathValue("name")); err != nil {
			return nil, Errorf(http.StatusNotFound, "%v", err)
		}
		return OK, nil
	})
	route("POST", p+"/claude/chats/{id}/stop", "Stop the running turn", func(w http.ResponseWriter, r *http.Request, c doorCaller) (any, error) {
		name, err := o.siteChat(r, c)
		if err != nil {
			return nil, err
		}
		_ = o.Chat.Stop(name)
		return OK, nil
	})
	route("POST", p+"/claude/chats/{id}/reap", "Stop the turn and kill what the conversation left running", func(w http.ResponseWriter, r *http.Request, c doorCaller) (any, error) {
		name, err := o.siteChat(r, c)
		if err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "killed": o.Chat.Reap(name)}, nil
	})
	route("DELETE", p+"/claude/chats/{id}", "Delete a conversation", func(w http.ResponseWriter, r *http.Request, c doorCaller) (any, error) {
		name, err := o.siteChat(r, c)
		if err != nil {
			return nil, err
		}
		if err := o.Chat.Delete(name); err != nil {
			return nil, chatError(err)
		}
		return OK, nil
	})
}

// door resolves the caller: a site token, or the operator (session or API
// token) naming the site in X-Middleware-Site. What decides is the manifest
// as it is now: a site whose door was closed still carries its token in
// its environment until it restarts, and must not get in with it.
func (o SiteDoorOptions) door(r *http.Request) (doorCaller, error) {
	domain := ""
	if tok, ok := strings.CutPrefix(strings.TrimSpace(r.Header.Get("Authorization")), "Bearer "); ok && o.Tokens != nil {
		domain = o.Tokens.Domain(strings.TrimSpace(tok))
	}
	if domain == "" {
		// Only the operator's full access names any site it likes. A
		// scoped token is a service door for a closed list of routes,
		// often for one site, and these routes are public to the gate, so
		// the scope check never saw them: a deploy token for one site, or
		// an import token handed to another installation, could open
		// conversations with write tools on every site of the house.
		if c := auth.FromContext(r.Context()); c.Authenticated() && len(c.Scopes) == 0 {
			domain = strings.ToLower(strings.TrimSpace(r.Header.Get("X-Middleware-Site")))
			if domain == "" {
				return doorCaller{}, Errorf(http.StatusBadRequest, "as the operator, name the site in X-Middleware-Site")
			}
		}
	}
	if domain == "" {
		// The same pause as the other doors: a token that can be tried a
		// thousand times a second is shorter than it looks.
		time.Sleep(400 * time.Millisecond)
		return doorCaller{}, Errorf(http.StatusUnauthorized, "this site's token is required (MIDDLEWARE_TOKEN)")
	}
	m, ok := o.Lookup(domain)
	if !ok || m == nil {
		return doorCaller{}, Errorf(http.StatusNotFound, "%s: no such site", domain)
	}
	if m.ClaudeAPI == nil {
		return doorCaller{}, Errorf(http.StatusForbidden, "%s has no Claude door open: it is opened in its page in the panel (or with claude_api in its manifest)", domain)
	}
	return doorCaller{Domain: domain, Rules: rulesOf(m.ClaudeAPI, o.houseDaily())}, nil
}

func (o SiteDoorOptions) houseDaily() float64 {
	if o.HouseDaily != nil {
		return o.HouseDaily()
	}
	return 5
}

func (o SiteDoorOptions) spent(domain string) float64 {
	if o.Spent == nil {
		return 0
	}
	return o.Spent(chat.SiteDoorBook(domain), time.Now())
}

// siteChats are the conversations the site's code opened, newest first.
func (o SiteDoorOptions) siteChats(domain string) []chat.Summary {
	var out []chat.Summary
	for _, sm := range o.Chat.List(domain) {
		if sm.Origin == chat.OriginSite {
			out = append(out, sm)
		}
	}
	return out
}

// busy is how many of them are at work now.
func (o SiteDoorOptions) busy(domain string) int {
	n := 0
	for _, sm := range o.siteChats(domain) {
		if sm.Running {
			n++
		}
	}
	return n
}

func (o SiteDoorOptions) model(c doorCaller, asked string) string {
	if m := strings.TrimSpace(asked); m != "" {
		return m
	}
	if c.Rules.Model != "" {
		return c.Rules.Model
	}
	// With a list and no default, the first of the list: the house's
	// default may well be one the site was not given.
	if len(c.Rules.Models) > 0 {
		return c.Rules.Models[0]
	}
	if o.DefaultModel != nil {
		return o.DefaultModel(c.Rules.Provider)
	}
	return ""
}

func (o SiteDoorOptions) available(provider string) bool {
	if o.Available != nil {
		return o.Available(provider)
	}
	if o.Chat == nil {
		return false
	}
	ok, _ := o.Chat.ProviderInfo(provider)
	return ok
}

func (o SiteDoorOptions) models(provider string) []string {
	if o.Models != nil {
		return o.Models(provider)
	}
	if o.Chat == nil {
		return nil
	}
	_, ms := o.Chat.ProviderInfo(provider)
	return ms
}

// agents are the agents a door may be given now, in the registry's order.
func (o SiteDoorOptions) agents() []string {
	if o.Agents != nil {
		return o.Agents()
	}
	var out []string
	if o.Chat != nil {
		for _, k := range o.Chat.Providers() {
			if o.available(k) {
				out = append(out, k)
			}
		}
	}
	return out
}

func (o SiteDoorOptions) turns(domain string) int {
	if o.Chat == nil {
		return 0
	}
	return o.Chat.Ledger().DayTurns(chat.SiteDoorBook(domain), time.Now())
}

// fitModel drops a model the site's code asks for that the door's agent
// does not have, and the door runs the one chosen in the panel. A site
// written when the door was Claude's names "sonnet" in every call; the
// operator who moved its door to Codex had chosen a model there, and a
// 400 on every call said that choice did not count. What was dropped is
// said back (model_ignored), so the code can tell. Only another agent's
// model: one that no agent has is a typo, and still a 400 before
// anything is spent.
func (o SiteDoorOptions) fitModel(provider string, b *askBody) {
	m := strings.TrimSpace(b.Model)
	if m == "" || o.knownModel(provider, m) == nil {
		return
	}
	// Every agent the house knows, running or not: a site written for
	// Claude names Claude's model even on a machine where Claude's module
	// is off now.
	others := o.agents()
	if o.Chat != nil {
		others = o.Chat.Providers()
	}
	for _, other := range others {
		if other != provider && o.knownModel(other, m) == nil && len(o.models(other)) > 0 {
			b.ignored, b.Model = m, ""
			return
		}
	}
}

// choices are the models the site's code may name: the door's list, or
// every model of its agent when the door has none.
func (o SiteDoorOptions) choices(c doorCaller) []string {
	if len(c.Rules.Models) > 0 {
		return c.Rules.Models
	}
	if ms := o.models(c.Rules.Provider); ms != nil {
		return ms
	}
	return []string{}
}

// allowedModel refuses a model the door was not given. The site's code
// picks the model call by call (a cheap one to classify, a strong one to
// write), but among the ones a person allowed: the bill is the house's,
// and a prompt injected into the site must not be able to move every call
// to the dearest model. An alias and the model it runs are the same
// model ("opus" and "claude-opus-5-5").
func (o SiteDoorOptions) allowedModel(c doorCaller, m string) error {
	m = strings.TrimSpace(m)
	if m == "" || len(c.Rules.Models) == 0 {
		return nil
	}
	var labels map[string]string
	if o.Chat != nil {
		labels = o.Chat.ProviderLabels(c.Rules.Provider)
	}
	for _, k := range c.Rules.Models {
		if strings.EqualFold(k, m) || (labels[k] != "" && strings.EqualFold(labels[k], m)) || (labels[m] != "" && strings.EqualFold(labels[m], k)) {
			return nil
		}
	}
	return Errorf(http.StatusBadRequest, "model %q is not one this site's gateway may use (claude_api.models: %s)", m, strings.Join(c.Rules.Models, ", "))
}

// knownModel refuses a model that is not on the agent's list: a typo
// would otherwise reach the CLI and come back as a failed turn that was
// paid for.
func (o SiteDoorOptions) knownModel(provider, m string) error {
	m = strings.TrimSpace(m)
	if m == "" {
		return nil
	}
	list := o.models(provider)
	if len(list) == 0 {
		return nil
	}
	for _, k := range list {
		if strings.EqualFold(k, m) {
			return nil
		}
	}
	return Errorf(http.StatusBadRequest, "model %q is not on %s's list (%s)", m, provider, strings.Join(list, ", "))
}

// allowance checks the money, and the turns where money cannot be read,
// before a turn starts, not on the invoice.
func (o SiteDoorOptions) allowance(c doorCaller) error {
	if !o.available(c.Rules.Provider) {
		return Errorf(http.StatusServiceUnavailable, "%s cannot run on this machine now: enable its module and sign in under Modules", c.Rules.Provider)
	}
	if spent := o.spent(c.Domain); spent >= c.Rules.DailyUSD {
		return Errorf(http.StatusTooManyRequests, "%s spent $%.2f through its door today and its cap is $%.2f; raise claude_api.daily_usd or wait until tomorrow (UTC)", c.Domain, spent, c.Rules.DailyUSD)
	}
	if c.Rules.DailyTurns > 0 {
		if n := o.turns(c.Domain); n >= c.Rules.DailyTurns {
			return Errorf(http.StatusTooManyRequests, "%s ran %d turns through its door today and its cap is %d; raise claude_api.daily_turns or wait until tomorrow (UTC)", c.Domain, n, c.Rules.DailyTurns)
		}
	}
	return nil
}

// open opens a conversation of the site with its limits in front: the
// money, the model, and room made among its old conversations.
func (o SiteDoorOptions) open(c doorCaller, b askBody) (string, error) {
	if err := o.allowance(c); err != nil {
		return "", err
	}
	// The model it is opened with is the one it carries on with, the
	// house's written in when the site names none: changing model halfway
	// and not saying so is the explanation nobody finds afterwards.
	model := o.model(c, b.Model)
	if err := o.knownModel(c.Rules.Provider, model); err != nil {
		return "", err
	}
	o.Chat.Prune(c.Domain, chat.OriginSite, c.Rules.MaxChats)
	name, err := o.Chat.Create(c.Domain, chat.CreateOptions{
		Origin: chat.OriginSite, Title: strings.TrimSpace(b.Title), System: systemFor(c.Domain, b), Tools: c.Rules.Tools,
		Provider: c.Rules.Provider, Model: model, Hidden: c.Rules.Hide, Max: c.Rules.MaxChats,
	})
	if errors.Is(err, chat.ErrTooMany) {
		// Only when every one of them is at work or waiting for an answer.
		return "", Errorf(http.StatusTooManyRequests, "%v: all of them are at work (claude_api.max_chats)", err)
	}
	return name, err
}

// siteChat resolves {id} to a conversation the site's code opened, and
// refuses any other: the panel's own is never reachable from here.
func (o SiteDoorOptions) siteChat(r *http.Request, c doorCaller) (string, error) {
	name := chat.Name(c.Domain, r.PathValue("id"))
	conv, err := o.Chat.Conversation(name)
	if err != nil || conv.Origin != chat.OriginSite || r.PathValue("id") == "" {
		return "", Errorf(http.StatusNotFound, "no such conversation")
	}
	return name, nil
}

// askBody is what may be asked; only the text is required.
type askBody struct {
	// Prompt and Text are the same: who comes from Anthropic's API writes
	// one, who comes from the panel the other, and refusing the wrong one
	// teaches nobody anything.
	Prompt string `json:"prompt"`
	Text   string `json:"text"`
	// System is the conversation's brief: who Claude is in it. It is kept
	// and holds for every turn.
	System string `json:"system"`
	Model  string `json:"model"`
	Title  string `json:"title"`
	// ignored is a model the site asked for that its agent does not have;
	// said back in the answer (model_ignored).
	ignored string
	// Format "json" asks for an answer that is only JSON; with Schema,
	// one that fits it.
	Format string          `json:"format"`
	Schema json.RawMessage `json:"schema"`
	// Wait is how long this request waits for the answer: "2m" by
	// default, at most 15m; "0" or "no" returns once the turn starts and
	// leaves polling to the caller.
	Wait string `json:"wait"`
	// Files are files of the site's own folder that go with the message.
	// They are copied, so they work with tools "none" too, and what goes
	// is the file as it is now.
	Files []string `json:"files"`
}

func (b askBody) said() string {
	if s := strings.TrimSpace(b.Prompt); s != "" {
		return s
	}
	return strings.TrimSpace(b.Text)
}

func (b askBody) wantsJSON() bool {
	return strings.EqualFold(strings.TrimSpace(b.Format), "json") || len(b.Schema) > 0
}

// systemFor is the conversation's brief: the site's own, with the JSON
// instruction when the answer is for a program. In the brief and not in
// the message, so it holds for every turn and is said once.
func systemFor(domain string, b askBody) string {
	own := strings.TrimSpace(b.System)
	if b.wantsJSON() {
		if own == "" {
			own = chat.SiteIntro(domain)
		}
		own += "\n\n" + jsonBrief(b.Schema)
	}
	return own
}

func jsonBrief(schema json.RawMessage) string {
	s := "Answer with valid JSON only: no text before or after it and no code fence."
	if len(schema) > 0 {
		s += " It must fit this JSON Schema:\n\n" + string(schema)
	}
	return s
}

func waitFor(s string) time.Duration {
	d := 2 * time.Minute
	switch strings.TrimSpace(s) {
	case "":
	case "0", "no":
		return 0
	default:
		if v, err := time.ParseDuration(s); err == nil {
			d = v
		}
	}
	return min(max(d, 0), 15*time.Minute)
}

func (o SiteDoorOptions) sendAndAnswer(w http.ResponseWriter, r *http.Request, c doorCaller, name string, b askBody) (any, error) {
	text := b.said()
	if err := o.attachPaths(c, name, b.Files); err != nil {
		return nil, err
	}
	if text == "" && len(b.Files) > 0 {
		text = "Look at what I sent you."
	}
	if text == "" {
		return nil, Errorf(http.StatusBadRequest, "prompt: empty")
	}
	// A conversation opened without the JSON brief and asked for JSON now:
	// the instruction goes with this message.
	if b.wantsJSON() {
		if conv, err := o.Chat.Conversation(name); err == nil && !strings.Contains(conv.System, "valid JSON only") {
			text += "\n\n" + jsonBrief(b.Schema)
		}
	}
	_, id, _ := strings.Cut(name, "#")
	// The door's agent and model as the panel says them now: a
	// conversation opened under another agent carries on with this one.
	model := strings.TrimSpace(b.Model)
	if conv, err := o.Chat.Conversation(name); err == nil && model == "" && conv.Provider != c.Rules.Provider {
		model = o.model(c, "")
	}
	if _, err := o.Chat.SendWith(name, text, chat.SendOptions{Model: model, Provider: c.Rules.Provider}); err != nil {
		if errors.Is(err, chat.ErrBusy) {
			WriteJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "chat": id})
			return Handled, nil
		}
		return nil, chatError(err)
	}
	running := func(why string) (any, error) {
		out := map[string]any{"chat": id, "state": "running"}
		if why != "" {
			out["why"] = why
		}
		if b.ignored != "" {
			out["model_ignored"] = b.ignored
		}
		WriteJSON(w, http.StatusAccepted, out)
		return Handled, nil
	}
	wait := waitFor(b.Wait)
	if wait == 0 {
		return running("")
	}
	// The request's context and the deadline: if the caller goes away
	// this stops waiting, but the turn goes on and its answer is written
	// in the conversation. Killing it would throw away what was paid for.
	if _, done := o.Chat.Wait(r.Context(), name, wait); !done {
		return running("still running after " + wait.String() + ": GET the conversation, or its stream, for the answer")
	}
	conv, err := o.Chat.Conversation(name)
	if err != nil {
		return nil, err
	}
	return answerOf(conv, b), nil
}

// answerOf is the answer as the caller reads it.
func answerOf(conv chat.Conversation, b askBody) map[string]any {
	info := infoOf(conv)
	text := said(conv)
	out := map[string]any{
		"chat":     conv.ID,
		"state":    "done",
		"text":     text,
		"model":    conv.Model,
		"turns":    info["turns"],
		"cost_usd": info["cost_usd"],
	}
	if n := len(conv.Turns); n > 0 && conv.Turns[n-1].Role == "error" {
		out["error"] = conv.Turns[n-1].Text
	}
	if b.ignored != "" {
		out["model_ignored"] = b.ignored
		out["warn"] = "model " + b.ignored + " is not one of " + conv.Provider + "'s: the gateway ran " + conv.Model + ", the one chosen in the panel"
	}
	if b.wantsJSON() {
		if v, err := looseJSON(text); err == nil {
			out["json"] = v
		} else {
			// Said here, not discovered by the caller's JSON.parse with an
			// error that names nobody.
			out["json_error"] = err.Error()
		}
	}
	// Claude may end by asking, and on the other side nobody presses a
	// button: said apart so the code can tell "answered" from "asked".
	if q := question(conv); q != "" {
		out["question"] = q
	}
	return out
}

// said is the last thing the agent answered.
func said(conv chat.Conversation) string {
	for i := len(conv.Turns) - 1; i >= 0; i-- {
		if t := conv.Turns[i]; t.Role != "user" {
			return t.Text
		}
	}
	return ""
}

func question(conv chat.Conversation) string {
	if !chat.Asking(conv) {
		return ""
	}
	return conv.Turns[len(conv.Turns)-1].Questions[0].Question
}

// infoOf is a conversation as V1's chat.Info described it.
func infoOf(c chat.Conversation) map[string]any {
	turns, cost := 0, 0.0
	updated := c.Created
	for _, t := range c.Turns {
		if t.Role != "user" {
			turns++
			cost += t.Cost
		}
		if t.At.After(updated) {
			updated = t.At
		}
	}
	return map[string]any{
		"id": c.ID, "domain": c.Domain, "title": c.Title, "model": c.Model,
		"created": c.Created, "updated": updated, "turns": turns, "cost_usd": round4(cost),
		"busy": c.Running, "messages": len(MessagesOf(c)), "origin": c.Origin,
		"system": c.System, "tools": c.Tools, "asking": chat.Asking(c),
	}
}

// Message is one line of a conversation as V1's panel and site door
// showed it: what someone said, a tool used (with its answer once back),
// or a turn's end.
type Message struct {
	Seq     int64     `json:"seq"`
	At      time.Time `json:"at"`
	Role    string    `json:"role"` // user, assistant, tool, result
	Text    string    `json:"text,omitempty"`
	Tool    *DoorTool `json:"tool,omitempty"`
	Agent   string    `json:"agent,omitempty"`
	CostUSD float64   `json:"cost_usd,omitempty"`
	Error   bool      `json:"error,omitempty"`
}

// DoorTool is a tool use in a Message.
type DoorTool struct {
	ID         string `json:"id"`
	Name       string `json:"name,omitempty"`
	Input      string `json:"input,omitempty"`
	Result     string `json:"result,omitempty"`
	Error      bool   `json:"error,omitempty"`
	Done       bool   `json:"done"`
	Run        string `json:"run,omitempty"`
	Background bool   `json:"background,omitempty"`
}

func doorTool(t *chat.ToolUse, background bool) *DoorTool {
	in := ""
	if len(t.Input) > 0 {
		b, _ := json.Marshal(t.Input)
		in = string(b)
	}
	return &DoorTool{ID: t.ID, Name: t.Name, Input: in, Result: t.Output, Error: t.Error, Done: t.Done, Run: t.Run, Background: background}
}

// MessagesOf lays a conversation out as messages, oldest first.
func MessagesOf(c chat.Conversation) []Message {
	var out []Message
	add := func(m Message) {
		m.Seq = int64(len(out) + 1)
		out = append(out, m)
	}
	for _, t := range c.Turns {
		if t.Role == "user" {
			add(Message{At: t.At, Role: "user", Text: t.Text})
			continue
		}
		bg := map[string]bool{}
		for _, th := range t.Threads {
			bg[th.ID] = th.Background
		}
		steps := func(steps []chat.Step, agent string) {
			for _, st := range steps {
				if st.Tool != nil {
					add(Message{At: t.At, Role: "tool", Tool: doorTool(st.Tool, bg[st.Tool.ID]), Agent: agent})
				} else if st.Text != "" {
					add(Message{At: t.At, Role: "assistant", Text: st.Text, Agent: agent})
				}
			}
		}
		if len(t.Steps) > 0 {
			steps(t.Steps, "")
			for _, th := range t.Threads {
				steps(th.Steps, th.ID)
			}
		} else if t.Text != "" && t.Role != "error" {
			add(Message{At: t.At, Role: "assistant", Text: t.Text})
		}
		res := Message{At: t.At, Role: "result", CostUSD: round4(t.Cost), Error: t.Role == "error"}
		if t.Role == "error" {
			res.Text = t.Text
		}
		add(res)
	}
	if out == nil {
		out = []Message{}
	}
	return out
}

// attachPaths puts files of the site's own folder on the next message.
// They are copied into the conversation's attachments rather than passed
// by path: so they work with tools "none", where Claude cannot open a
// file by itself, and what goes is the file as it is now.
func (o SiteDoorOptions) attachPaths(c doorCaller, name string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	if o.SiteDir == nil {
		return Errorf(http.StatusBadRequest, "files cannot be attached here")
	}
	root, err := os.OpenRoot(o.SiteDir(c.Domain))
	if err != nil {
		return Errorf(http.StatusBadRequest, "files cannot be attached here: %v", err)
	}
	defer root.Close()
	for _, p := range paths {
		f, err := openInside(root, p)
		if err != nil {
			return Errorf(http.StatusBadRequest, "%v", err)
		}
		_, err = o.Chat.AddUpload(name, filepath.Base(filepath.Clean(p)), f)
		f.Close()
		if err != nil {
			return Errorf(http.StatusBadRequest, "%s: %v", p, err)
		}
	}
	return nil
}

// openInside opens a regular file of the site's folder and refuses one
// that leaves it: what arrives here is written by the site's code, and a
// path that ends in an open is how the neighbour's .env, or the house's
// own sessions and door key, is read with the house's eyes.
//
// It used to resolve the links with EvalSymlinks, check the result was
// inside, and then os.Open the path again. Between the check and the open
// a site in a container swaps the file for a link to
// data/auth/sessions.json, in a loop, and wins the race often enough.
// os.Root resolves every component at the moment of the open and refuses
// one that leaves the folder, so what is checked is what is opened; the
// handle is then asked what it is, never the path again.
//
// O_NONBLOCK because a FIFO the site made would otherwise hold the open,
// and the request, until something wrote to it; on a regular file it
// changes nothing. A directory, a socket or a device is not an
// attachment, and what would be read from /dev/zero is a full disk.
func openInside(root *os.Root, name string) (*os.File, error) {
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("a file in the list has no name")
	}
	if filepath.IsAbs(name) {
		return nil, fmt.Errorf("%q: name it from the site's folder, not from /", name)
	}
	f, err := root.OpenFile(filepath.Clean(name), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		// os.Root says "path escapes from parent" and does not export the
		// error; the site's code deserves to be told plainly.
		if strings.Contains(err.Error(), "escapes") {
			return nil, fmt.Errorf("%q is outside this site's folder", name)
		}
		return nil, fmt.Errorf("%q: %v", name, err)
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%q: not a file", name)
	}
	return f, nil
}

// oneUpload takes the file from the request however it comes: a form, as
// a browser sends it, or the raw body with the name in the address, as
// three lines of code send it.
func oneUpload(r *http.Request) (string, io.ReadCloser, error) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		mr, err := r.MultipartReader()
		if err != nil {
			return "", nil, errors.New("not a file upload")
		}
		for {
			part, err := mr.NextPart()
			if err != nil {
				return "", nil, errors.New("no file came with it (field \"file\")")
			}
			if part.FormName() == "file" && part.FileName() != "" {
				return part.FileName(), part, nil
			}
			_ = part.Close()
		}
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		return "", nil, errors.New("name the file in ?name=")
	}
	return name, r.Body, nil
}

func attachmentOf(u chat.Upload) map[string]any {
	kind := "document"
	switch strings.ToLower(filepath.Ext(u.Name)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		kind = "image"
	}
	return map[string]any{"name": u.Name, "size": u.Bytes, "kind": kind, "at": time.Now().UTC()}
}

// stream is the conversation as it happens: an "event: message" per
// message, as GET returns them, an "event: delta" per piece of the answer
// while it is written ({turn_id, text}: the pieces since the last message
// are the start of the next one, which replaces them when it comes), and
// an "event: session" with the conversation after each turn ends, which
// is when a client puts its button back.
func (o SiteDoorOptions) stream(w http.ResponseWriter, r *http.Request, name string) {
	flusher, ok := w.(http.Flusher)
	if !ok || o.Chat == nil {
		WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming is not available"})
		return
	}
	// The conversation's own watch, not the bus: the pieces of the answer
	// come on it too, in the order the turn made them. Watched before the
	// headers go, so a client that sends once the stream is open misses
	// nothing of the turn.
	ch, unwatch := o.Chat.Watch(name)
	defer unwatch()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()
	tick := time.NewTicker(20 * time.Second)
	defer tick.Stop()
	var seq int64
	send := func(event string, v any) bool {
		b, err := json.Marshal(v)
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case it := <-ch:
			if it.Delta != nil {
				if !send("delta", it.Delta) {
					return
				}
				continue
			}
			if it.Event == nil {
				continue
			}
			m, end := liveMessage(*it.Event)
			if m == nil {
				continue
			}
			seq++
			m.Seq = seq
			if !send("message", m) {
				return
			}
			if end {
				if conv, err := o.Chat.Conversation(name); err == nil && !send("session", infoOf(conv)) {
					return
				}
			}
		}
	}
}

// liveMessage turns a progress event into the message it stands for.
func liveMessage(e events.Event) (*Message, bool) {
	d := e.Data
	str := func(k string) string { s, _ := d[k].(string); return s }
	b := func(k string) bool { v, _ := d[k].(bool); return v }
	m := &Message{At: e.Time, Agent: str("thread")}
	switch e.Type {
	case "agent.text":
		m.Role, m.Text = "assistant", str("text")
	case "agent.tool":
		in := ""
		if v, ok := d["input"]; ok {
			raw, _ := json.Marshal(v)
			in = string(raw)
		}
		bg := false
		if inp, ok := d["input"].(map[string]any); ok {
			bg, _ = inp["run_in_background"].(bool)
		}
		m.Role, m.Tool = "tool", &DoorTool{ID: str("id"), Name: str("tool"), Input: in, Run: str("run"), Background: bg}
	case "agent.tool.result":
		m.Role, m.Tool = "tool", &DoorTool{ID: str("id"), Result: str("output"), Error: b("error"), Done: true, Run: str("run")}
	case "agent.subagent":
		m.Role, m.Tool = "tool", &DoorTool{ID: str("id"), Run: str("run")}
	case "agent.done":
		cost, _ := d["cost"].(float64)
		m.Role, m.CostUSD, m.Agent = "result", round4(cost), ""
		return m, true
	case "agent.error":
		if str("turn_id") == "" {
			return nil, false
		}
		cost, _ := d["cost"].(float64)
		m.Role, m.CostUSD, m.Error, m.Text, m.Agent = "result", round4(cost), true, e.Message, ""
		return m, true
	default:
		return nil, false
	}
	return m, false
}

// looseJSON reads the JSON in an answer that was asked to be only JSON
// and sometimes still comes in a fence or after a line of prose. It does
// not repair JSON: what is between the braces either reads or is said not
// to.
func looseJSON(said string) (any, error) {
	s := strings.TrimSpace(said)
	if i := strings.Index(s, "```"); i >= 0 {
		rest := s[i+3:]
		if j := strings.Index(rest, "\n"); j >= 0 {
			rest = rest[j+1:]
		}
		if k := strings.Index(rest, "```"); k >= 0 {
			s = strings.TrimSpace(rest[:k])
		}
	}
	var v any
	if json.Unmarshal([]byte(s), &v) == nil {
		return v, nil
	}
	if i := strings.IndexAny(s, "{["); i > 0 {
		s = s[i:]
	}
	if i := strings.LastIndexAny(s, "}]"); i >= 0 && i < len(s)-1 {
		s = s[:i+1]
	}
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return nil, fmt.Errorf("the answer is not JSON: %w", err)
	}
	return v, nil
}

func round4(v float64) float64 { return float64(int64(v*10000+0.5)) / 10000 }

// V1Site is where V1 served the site door. A site's code written for V1
// calls $MIDDLEWARE_API/api/site/…; the panel's handler maps it onto
// Prefix/site/….
const V1Site = "/api/site"

// SiteDoorPath maps a V1 site door path to this API's, and says whether
// it is one.
func SiteDoorPath(path string) (string, bool) {
	if path == V1Site || strings.HasPrefix(path, V1Site+"/") {
		return Prefix + strings.TrimPrefix(path, "/api"), true
	}
	if path == Prefix+"/site" || strings.HasPrefix(path, Prefix+"/site/") {
		return path, true
	}
	return path, false
}
