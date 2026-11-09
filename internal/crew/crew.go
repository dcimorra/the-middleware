// Package crew is the operator's crew: autonomous agents the operator
// creates at the panel, each with a goal in their own words, and from
// then on working by themselves toward it. The network's agent
// (internal/autopilot) looks after the sites; these look after whatever
// the operator hands them (an Instagram account to grow, say), outside
// the sites altogether.
//
// Each agent is a folder, data/crew/<id>:
//
//   - agent.json, its settings: name, goal, switch, agent and model, the
//     bounds of its wakes, the share of the subscription it may use;
//   - state.json, where it stands, what must survive a restart;
//   - requests.json, what it asked the operator and what they answered;
//   - vault.json, the credentials and data it was given, encrypted with
//     the house's key (internal/secrets) and handed to its turns as
//     environment variables, never written in clear;
//   - journal.jsonl, every wake, turn, request and refusal;
//   - desk/, its working folder, the only one it may write, where its
//     notebook is.
//
// Its conversations are "crew~<id>#<hex>" (chat.CrewDomain), one per
// session, jailed to its desk (agent.Jail.Crew), and on the socket it is
// a caller of its own that reaches its own routes and nothing else
// (auth.CrewScopes). How it wakes, holds its context and is held by the
// subscription is the autopilot's: a "Next:" line at the end of every
// turn, a new conversation at every wake with the notebook as its only
// memory, and no turn while a window is above QuotaPercent.
//
// What it cannot do itself it asks for (Ask): a credential, a decision,
// or, urgent, a check only a person can pass (an SMS code, a captcha).
// An urgent request reaches the operator's phone (crew.request, pushed
// by internal/push) and is pushed again an hour later if still open; an
// answer wakes the agent.
package crew

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/dcimorra/the-middleware/internal/agent"
	"github.com/dcimorra/the-middleware/internal/autopilot"
	"github.com/dcimorra/the-middleware/internal/chat"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/secrets"
)

// Network is the id of the network's own agent (internal/autopilot) in
// the crew. It is listed and asks the operator as every agent does, and
// has their vault and desk, but its wakes, its switch and its settings
// are the autopilot's: it watches the sites of a whole network, one for
// all its nodes, and that engine is not this one.
const Network = "network"

// KindNetwork marks the network's agent.
const KindNetwork = "network"

// ErrNetwork is what the crew answers for what is the autopilot's.
var ErrNetwork = errors.New("the network's agent is switched, set and woken at its own routes (/autopilot)")

// Agent is an agent's settings, agent.json.
type Agent struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Kind is "" for an agent of the operator's, KindNetwork for the
	// network's own.
	Kind string `json:"kind,omitempty"`
	// Goal is the operator's words: what it works toward and how.
	Goal    string `json:"goal"`
	Enabled bool   `json:"enabled,omitempty"`
	// Provider and Model run its turns; Claude with Opus by default.
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	// MinMinutes and MaxMinutes bound when it may ask to be woken next.
	MinMinutes int `json:"min_minutes,omitempty"`
	MaxMinutes int `json:"max_minutes,omitempty"`
	// QuotaPercent is the share of a subscription window above which it
	// does not start a turn: the brake, as for the autopilot.
	QuotaPercent float64 `json:"quota_percent,omitempty"`
	// ContextPercent is read and no longer used: every wake is a new
	// conversation (the session once went on until this share was full).
	ContextPercent int `json:"context_percent,omitempty"`
	// DiskMB is the room its desk and its home may take, 2 GiB by
	// default: it builds its own tools and keeps its own files there.
	DiskMB  int       `json:"disk_mb,omitempty"`
	Created time.Time `json:"created"`
}

// Limits fills the defaults.
func (a Agent) Limits() Agent {
	if a.Provider == "" {
		a.Provider = "claude"
		if a.Model == "" {
			a.Model = "opus"
		}
	}
	if a.MinMinutes <= 0 {
		a.MinMinutes = 10
	}
	if a.MaxMinutes <= 0 {
		a.MaxMinutes = 12 * 60
	}
	if a.MaxMinutes < a.MinMinutes {
		a.MaxMinutes = a.MinMinutes
	}
	if a.QuotaPercent <= 0 {
		a.QuotaPercent = 80
	}
	if a.ContextPercent <= 0 {
		a.ContextPercent = 60
	}
	if a.DiskMB <= 0 {
		a.DiskMB = 2048
	}
	return a
}

// Validate checks what the operator wrote.
func (a Agent) Validate() error {
	if strings.TrimSpace(a.Name) == "" {
		return errors.New("the agent needs a name")
	}
	if len(a.Name) > 80 {
		return errors.New("the name is longer than 80 characters")
	}
	if len(a.Goal) > 32<<10 {
		return errors.New("the goal is longer than 32 KB")
	}
	if a.MinMinutes < 0 || a.MaxMinutes < 0 || a.MaxMinutes > 7*24*60 {
		return errors.New("the wake bounds are minutes, from 0 to a week")
	}
	if a.QuotaPercent < 0 || a.QuotaPercent > 100 || a.ContextPercent < 0 || a.ContextPercent > 95 {
		return errors.New("quota_percent is 0 to 100 and context_percent 0 to 95")
	}
	if a.DiskMB < 0 || a.DiskMB > 1<<20 {
		return errors.New("disk_mb is 0 (2 GiB) to 1048576 (1 TiB)")
	}
	if a.Provider != "" && !providerRe.MatchString(a.Provider) {
		return fmt.Errorf("%q is not an agent's name", a.Provider)
	}
	return nil
}

var providerRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// State is state.json: where it stands.
type State struct {
	Session        string    `json:"session,omitempty"`
	SessionStarted time.Time `json:"session_started,omitzero"`
	// Fresh is a session that has had no turn yet.
	Fresh   bool `json:"fresh,omitempty"`
	Context int  `json:"context,omitempty"`
	Window  int  `json:"window,omitempty"`
	// Turn is its own turn running now, and TurnStarted when it began.
	Turn        string    `json:"turn,omitempty"`
	TurnStarted time.Time `json:"turn_started,omitzero"`
	LastWake    time.Time `json:"last_wake,omitzero"`
	NextWake    time.Time `json:"next_wake,omitzero"`
	NextReason  string    `json:"next_reason,omitempty"`
	// Skipped is why the last due wake did not happen.
	Skipped    string `json:"skipped,omitempty"`
	LastResult string `json:"last_result,omitempty"`
	Turns      int    `json:"turns,omitempty"`
}

// Options configure the crew. The functions are the house's, given by the
// wiring, so this package needs neither the chat manager nor the engine.
type Options struct {
	// Root is the installation's root.
	Root string
	Bus  *events.Bus
	// Box encrypts the vaults.
	Box *secrets.Box
	// NewSession opens a new conversation of the agent's and returns its
	// name (chat.CrewDomain(id)#hex).
	NewSession func(id string) (string, error)
	// Send starts a turn in one of its conversations.
	Send func(name, prompt, provider, model string) (string, error)
	// Stop ends a conversation's running turn.
	Stop func(name string) error
	// Running says a conversation has a turn running.
	Running func(name string) bool
	// Commands are the shell commands a finished turn ran.
	Commands func(name, turnID string) []string
	// Usage is what is known of a provider's subscription windows.
	Usage func(provider string) []agent.Window
	// Forget removes an agent's conversations, when it is deleted.
	Forget func(domain string)
	// OnAnswer is called when the operator answers one of an agent's
	// requests: what wakes the network's agent, whose wakes are not ours.
	OnAnswer func(id string)
	Every    time.Duration
	Now      func() time.Time
}

// member is an agent as the crew holds it.
type member struct {
	a    Agent
	st   State
	reqs []Request
	j    *autopilot.Journal
	// starting is a wake between deciding and its turn being recorded:
	// the chat is called with the crew's lock let go (it calls back into
	// the crew for the turn's vault and agent), and another pass must
	// not start a second turn meanwhile.
	starting bool
	// disk is what its desk and home held at diskAt, in MiB.
	disk   int64
	diskAt time.Time
}

// Crew supervises the agents.
type Crew struct {
	opts Options
	mu   sync.Mutex
	m    map[string]*member
}

// Limits on what an agent may have going.
const (
	maxAgents  = 50
	stuckTurn  = 6 * time.Hour
	retrySoon  = 5 * time.Minute
	remindOpen = time.Hour
)

// ErrNoAgent is an id that names no agent.
var ErrNoAgent = errors.New("no such agent")

// New loads every agent under data/crew.
func New(opts Options) *Crew {
	if opts.Every == 0 {
		opts.Every = 30 * time.Second
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	c := &Crew{opts: opts, m: map[string]*member{}}
	ents, _ := os.ReadDir(c.base())
	for _, e := range ents {
		if !e.IsDir() || !chat.ValidCrewID(e.Name()) {
			continue
		}
		var a Agent
		if fsx.ReadJSON(filepath.Join(c.base(), e.Name(), "agent.json"), &a) != nil || a.ID != e.Name() {
			continue
		}
		mb := &member{a: a, j: autopilot.OpenJournal(filepath.Join(c.base(), a.ID, "journal.jsonl"))}
		_ = fsx.ReadJSON(filepath.Join(c.base(), a.ID, "state.json"), &mb.st)
		_ = fsx.ReadJSON(filepath.Join(c.base(), a.ID, "requests.json"), &mb.reqs)
		c.m[a.ID] = mb
	}
	if c.m[Network] == nil {
		a := Agent{ID: Network, Name: "Network agent", Kind: KindNetwork, Created: c.now()}
		mb := &member{a: a, j: autopilot.OpenJournal(filepath.Join(c.dir(Network), "journal.jsonl"))}
		if c.saveAgentLocked(mb) == nil {
			c.m[Network] = mb
		}
	}
	return c
}

func (c *Crew) base() string          { return filepath.Join(c.opts.Root, "data", "crew") }
func (c *Crew) dir(id string) string  { return filepath.Join(c.base(), id) }
func (c *Crew) Desk(id string) string { return chat.CrewDesk(c.opts.Root, id) }
func (c *Crew) home(id string) string {
	return filepath.Join(c.opts.Root, "data", "homes", chat.CrewDomain(id))
}
func (c *Crew) now() time.Time        { return c.opts.Now().UTC() }
func (c *Crew) get(id string) *member { return c.m[id] }
func (c *Crew) note(mb *member, e Entry) {
	if e.At.IsZero() {
		e.At = c.now()
	}
	mb.j.Add(e)
}

// Entry is a journal line; the autopilot's, so both read the same.
type Entry = autopilot.Entry

// What a crew journal line is about, beyond the autopilot's kinds.
const (
	KindRequest = "request" // it asked the operator for something
	KindAnswer  = "answer"  // the operator answered, or dismissed it
	KindVault   = "vault"   // the operator changed its vault
)

func (c *Crew) saveAgentLocked(mb *member) error {
	if err := os.MkdirAll(c.dir(mb.a.ID), 0o700); err != nil {
		return err
	}
	return fsx.WriteJSON(filepath.Join(c.dir(mb.a.ID), "agent.json"), mb.a, 0o600)
}

func (c *Crew) saveStateLocked(mb *member) {
	_ = fsx.WriteJSON(filepath.Join(c.dir(mb.a.ID), "state.json"), mb.st, 0o600)
}

func (c *Crew) saveRequestsLocked(mb *member) {
	_ = fsx.WriteJSON(filepath.Join(c.dir(mb.a.ID), "requests.json"), mb.reqs, 0o600)
}

func (c *Crew) emit(typ, id, level, msg string, data map[string]any) {
	if c.opts.Bus != nil {
		c.opts.Bus.Emit(events.Event{Type: typ, Subject: id, Actor: "crew", Level: level, Message: msg, Data: data})
	}
}

// Slug makes an id from a name: lower-case letters, digits and dashes.
func Slug(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		if b.Len() >= 24 {
			break
		}
		r = fold(r)
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			if b.Len() == 0 && r >= '0' && r <= '9' {
				b.WriteString("a-")
			}
			b.WriteRune(r)
			dash = false
		case b.Len() > 0 && !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		s = "agent"
	}
	return s
}

// fold takes the accent off the letters a Spanish or French name has.
func fold(r rune) rune {
	switch r {
	case 'á', 'à', 'ä', 'â':
		return 'a'
	case 'é', 'è', 'ë', 'ê':
		return 'e'
	case 'í', 'ì', 'ï', 'î':
		return 'i'
	case 'ó', 'ò', 'ö', 'ô':
		return 'o'
	case 'ú', 'ù', 'ü', 'û':
		return 'u'
	case 'ñ':
		return 'n'
	case 'ç':
		return 'c'
	}
	if r > unicode.MaxASCII {
		return '-'
	}
	return r
}

// Create makes an agent, off: the operator reads what it will do and
// switches it on.
func (c *Crew) Create(a Agent) (Agent, error) {
	a.Name = strings.TrimSpace(a.Name)
	if err := a.Validate(); err != nil {
		return Agent{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= maxAgents {
		return Agent{}, fmt.Errorf("the crew has %d agents already", maxAgents)
	}
	base := Slug(a.Name)
	id := base
	for n := 2; id == Network || c.m[id] != nil || exists(c.dir(id)); n++ {
		id = fmt.Sprintf("%s-%d", base, n)
	}
	a.ID, a.Created, a.Enabled = id, c.now(), false
	mb := &member{a: a, j: autopilot.OpenJournal(filepath.Join(c.dir(id), "journal.jsonl"))}
	if err := os.MkdirAll(c.Desk(id), 0o700); err != nil {
		return Agent{}, err
	}
	if err := c.saveAgentLocked(mb); err != nil {
		return Agent{}, err
	}
	c.m[id] = mb
	c.note(mb, Entry{Kind: autopilot.KindOn, Message: "created by the operator"})
	return a, nil
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// Update replaces an agent's settings; the switch is SetEnabled's.
func (c *Crew) Update(id string, a Agent) (Agent, error) {
	if id == Network {
		return Agent{}, ErrNetwork
	}
	a.Name = strings.TrimSpace(a.Name)
	if err := a.Validate(); err != nil {
		return Agent{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	mb := c.get(id)
	if mb == nil {
		return Agent{}, ErrNoAgent
	}
	goal := mb.a.Goal != a.Goal
	a.ID, a.Created, a.Enabled = mb.a.ID, mb.a.Created, mb.a.Enabled
	mb.a = a
	if err := c.saveAgentLocked(mb); err != nil {
		return Agent{}, err
	}
	if goal {
		c.note(mb, Entry{Kind: autopilot.KindOn, Message: "the operator changed the goal"})
		if mb.a.Enabled && mb.st.Turn == "" {
			mb.st.NextWake, mb.st.NextReason, mb.st.Skipped = time.Time{}, "the operator changed your goal", ""
			c.saveStateLocked(mb)
		}
	}
	return a, nil
}

// Delete stops an agent and forgets it: its folder, its HOME and its
// conversations. Its journal goes with it; the events stay.
func (c *Crew) Delete(id string) error {
	if id == Network {
		return ErrNetwork
	}
	c.mu.Lock()
	mb := c.get(id)
	if mb == nil {
		c.mu.Unlock()
		return ErrNoAgent
	}
	delete(c.m, id)
	session, turn := mb.st.Session, mb.st.Turn
	c.mu.Unlock()
	if turn != "" && session != "" && c.opts.Stop != nil {
		_ = c.opts.Stop(session)
	}
	if c.opts.Forget != nil {
		c.opts.Forget(chat.CrewDomain(id))
	}
	_ = os.RemoveAll(c.home(id))
	if err := os.RemoveAll(c.dir(id)); err != nil {
		return err
	}
	c.emit("crew.deleted", id, "", "the operator deleted the agent "+mb.a.Name, nil)
	return nil
}

// SetEnabled switches an agent on (it looks at once) or off (a turn at
// work finishes).
func (c *Crew) SetEnabled(id string, on bool) error {
	if id == Network {
		return ErrNetwork
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	mb := c.get(id)
	if mb == nil {
		return ErrNoAgent
	}
	if on && strings.TrimSpace(mb.a.Goal) == "" {
		return errors.New("write its goal first: an agent with no goal has nothing to work toward")
	}
	mb.a.Enabled = on
	if err := c.saveAgentLocked(mb); err != nil {
		return err
	}
	if on {
		mb.st.NextWake, mb.st.NextReason, mb.st.Skipped = time.Time{}, "switched on", ""
		c.saveStateLocked(mb)
		c.note(mb, Entry{Kind: autopilot.KindOn, Message: "switched on by the operator"})
		c.emit("crew.on", id, "", mb.a.Name+" switched on", nil)
		return nil
	}
	c.note(mb, Entry{Kind: autopilot.KindOff, Message: "switched off by the operator; a turn at work finishes"})
	c.emit("crew.off", id, "", mb.a.Name+" switched off", nil)
	return nil
}

// Halt switches an agent off and cuts its turn now.
func (c *Crew) Halt(id string) error {
	if id == Network {
		return ErrNetwork
	}
	c.mu.Lock()
	mb := c.get(id)
	if mb == nil {
		c.mu.Unlock()
		return ErrNoAgent
	}
	mb.a.Enabled = false
	err := c.saveAgentLocked(mb)
	session, turn := mb.st.Session, mb.st.Turn
	c.note(mb, Entry{Kind: autopilot.KindStopped, Message: "stopped by the operator: switched off and its turn cut"})
	c.mu.Unlock()
	if turn != "" && session != "" && c.opts.Stop != nil {
		_ = c.opts.Stop(session)
	}
	c.emit("crew.stopped", id, "warn", mb.a.Name+" stopped", nil)
	return err
}

// Wake has an agent look now.
func (c *Crew) Wake(id string) error {
	if id == Network {
		return ErrNetwork
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	mb := c.get(id)
	if mb == nil {
		return ErrNoAgent
	}
	mb.st.NextWake, mb.st.NextReason, mb.st.Skipped = time.Time{}, "the operator asked you to look now", ""
	c.saveStateLocked(mb)
	return nil
}

// Status is an agent as the panel and its own turns read it.
type Status struct {
	Agent   Agent  `json:"agent"`
	State   State  `json:"state"`
	Running bool   `json:"running"`
	Domain  string `json:"domain"`
	// ContextPercent is how full its session is, -1 unknown.
	ContextPercent int            `json:"context_percent"`
	Quota          []agent.Window `json:"quota,omitempty"`
	Requests       []Request      `json:"requests"`
	// Vault is the names it holds, never the values.
	Vault    []string `json:"vault"`
	Notebook string   `json:"notebook,omitempty"`
	Desk     string   `json:"desk"`
	// DiskUsed is what its desk and its home hold, in MiB, as last
	// measured; DiskMB in its settings is what it may use.
	DiskUsed int64 `json:"disk_used"`
}

// Get is one agent's status.
func (c *Crew) Get(id string) (Status, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	mb := c.get(id)
	if mb == nil {
		return Status{}, ErrNoAgent
	}
	return c.statusLocked(mb, true), nil
}

// List is every agent's status, the notebooks left out, by name.
func (c *Crew) List() []Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []Status{}
	for _, mb := range c.m {
		out = append(out, c.statusLocked(mb, false))
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Agent.Name) < strings.ToLower(out[j].Agent.Name) })
	return out
}

// Has says id names an agent.
func (c *Crew) Has(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[id] != nil
}

func (c *Crew) statusLocked(mb *member, full bool) Status {
	a := mb.a.Limits()
	st := Status{Agent: mb.a, State: mb.st, Domain: chat.CrewDomain(mb.a.ID), ContextPercent: pct(mb.st.Context, mb.st.Window), Desk: c.Desk(mb.a.ID), Requests: []Request{}}
	st.Agent.Provider, st.Agent.Model = a.Provider, a.Model
	if mb.st.Session != "" && c.opts.Running != nil {
		st.Running = c.opts.Running(mb.st.Session)
	}
	if c.opts.Usage != nil {
		st.Quota = c.opts.Usage(a.Provider)
	}
	keys := c.vaultKeys(mb.a.ID)
	st.Vault = keys
	st.DiskUsed = mb.disk
	for _, r := range mb.reqs {
		if !full && r.Status != StatusOpen {
			continue
		}
		st.Requests = append(st.Requests, r)
	}
	sort.SliceStable(st.Requests, func(i, j int) bool {
		if (st.Requests[i].Status == StatusOpen) != (st.Requests[j].Status == StatusOpen) {
			return st.Requests[i].Status == StatusOpen
		}
		return st.Requests[i].At.After(st.Requests[j].At)
	})
	if full {
		if b, err := os.ReadFile(filepath.Join(c.Desk(mb.a.ID), chat.CrewNotebook)); err == nil {
			st.Notebook = autopilot.Clip(string(b), 256<<10)
		}
	}
	return st
}

// Journal is an agent's last n journal lines.
func (c *Crew) Journal(id string, n int) ([]Entry, error) {
	c.mu.Lock()
	mb := c.get(id)
	c.mu.Unlock()
	if mb == nil {
		return nil, ErrNoAgent
	}
	return mb.j.Tail(n), nil
}

// Note adds a line to an agent's journal (the API's refusals).
func (c *Crew) Note(id string, e Entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if mb := c.get(id); mb != nil {
		c.note(mb, e)
	}
}

// Settings is the provider and model an agent's conversations run with.
func (c *Crew) Settings(id string) (string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	mb := c.get(id)
	if mb == nil {
		return "", ""
	}
	a := mb.a.Limits()
	return a.Provider, a.Model
}

func pct(used, window int) int {
	if window <= 0 {
		return -1
	}
	return used * 100 / window
}

// Run passes every period and listens to the bus for its turns' ends.
func (c *Crew) Run(ctx context.Context) {
	var ch <-chan events.Event
	if c.opts.Bus != nil {
		sub, stop := c.opts.Bus.Subscribe()
		defer stop()
		ch = sub
	}
	t := time.NewTicker(c.opts.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.Pass()
		case e, ok := <-ch:
			if !ok {
				return
			}
			if e.Type == "agent.done" || e.Type == "agent.error" {
				c.Finished(e)
			}
		}
	}
}

// Pass looks at every agent once.
func (c *Crew) Pass() {
	c.mu.Lock()
	ids := make([]string, 0, len(c.m))
	for id := range c.m {
		ids = append(ids, id)
	}
	c.mu.Unlock()
	sort.Strings(ids)
	for _, id := range ids {
		c.measure(id)
		c.pass(id)
	}
}

// diskEvery is how often an agent's room is measured: walking a desk
// with a browser profile or a model in it is not free.
const diskEvery = 10 * time.Minute

// measure weighs an agent's desk and home, outside the lock, and notes
// when it goes over the room it has: its next wake is told to make room
// before anything else. Nothing is deleted for it; the operator sees
// the number on its page and may give it more.
func (c *Crew) measure(id string) {
	now := c.opts.Now()
	c.mu.Lock()
	mb := c.get(id)
	if mb == nil || mb.a.Kind == KindNetwork || (!mb.diskAt.IsZero() && now.Sub(mb.diskAt) < diskEvery) {
		c.mu.Unlock()
		return
	}
	mb.diskAt = now
	c.mu.Unlock()
	used := (dirSize(c.Desk(id)) + dirSize(c.home(id))) >> 20
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.get(id) != mb {
		return
	}
	limit := int64(mb.a.Limits().DiskMB)
	if used > limit && mb.disk <= limit {
		c.note(mb, Entry{Kind: autopilot.KindRefused, Message: fmt.Sprintf("its desk and home hold %d MiB, over the %d MiB it has", used, limit)})
	}
	mb.disk = used
}

// dirSize is what the regular files under dir take, symlinks not
// followed.
func dirSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

func (c *Crew) pass(id string) {
	now := c.opts.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	mb := c.get(id)
	if mb == nil || mb.starting {
		return
	}
	cfg := mb.a.Limits()
	c.remindLocked(mb, now)
	if mb.a.Kind == KindNetwork {
		return
	}
	if mb.st.Turn != "" {
		// An end never heard (a turn lost to a crash) does not hold it.
		running := c.opts.Running != nil && c.opts.Running(mb.st.Session)
		if !running && now.Sub(mb.st.TurnStarted) > 10*time.Minute || now.Sub(mb.st.TurnStarted) > stuckTurn {
			c.note(mb, Entry{Kind: autopilot.KindTurn, TurnID: mb.st.Turn, Message: "its end was never heard; let go"})
			mb.st.Turn = ""
			mb.st.NextWake = now.Add(time.Duration(cfg.MinMinutes) * time.Minute).UTC()
			c.saveStateLocked(mb)
		}
		return
	}
	if !cfg.Enabled {
		return
	}
	reason := c.dueLocked(mb, cfg, now)
	if reason == "" {
		return
	}
	if c.opts.Usage != nil {
		if w, over := autopilot.OverQuota(c.opts.Usage(cfg.Provider), cfg.QuotaPercent, now); over {
			next := w.ResetsAt
			if next.IsZero() || next.Before(now) {
				next = now.Add(30 * time.Minute)
			}
			c.skipLocked(mb, fmt.Sprintf("the subscription's %s window is at %.0f%%, above the %.0f%% it may use", w.Name, w.Used, cfg.QuotaPercent), next)
			return
		}
	}
	// The chat is called unlocked: starting a turn asks the crew for the
	// agent's vault and settings (chat.Options.CrewEnv, CrewSettings),
	// and with the lock held that waited on itself for ever, freezing the
	// whole crew and every panel page that reads it (alpha.127, the first
	// agent switched on). unlocked runs f with the lock let go and hands
	// back the member, which may have been deleted meanwhile.
	mb.starting = true
	unlocked := func(f func()) *member {
		c.mu.Unlock()
		f()
		c.mu.Lock()
		if m := c.get(id); m == mb {
			return mb
		}
		return nil
	}
	defer func() { mb.starting = false }()
	// Every wake is a new conversation (Daniel, 2026-11-09): a session
	// carried on until its context was 60 % full took everything the
	// earlier turns had read into each new one, and the agent could not
	// start clean. What it needs from before it keeps in its notebook and
	// reads when it wants to; the house decides nothing about it.
	{
		var name string
		var err error
		if unlocked(func() { name, err = c.opts.NewSession(id) }) == nil {
			return
		}
		if err != nil {
			c.skipLocked(mb, "no conversation could be opened: "+err.Error(), now.Add(retrySoon))
			return
		}
		mb.st.Session, mb.st.SessionStarted, mb.st.Fresh, mb.st.Context, mb.st.Window = name, now.UTC(), true, 0, 0
	}
	prompt := c.promptLocked(mb, cfg, now, reason)
	var turnID string
	var err error
	session := mb.st.Session
	if unlocked(func() { turnID, err = c.opts.Send(session, prompt, cfg.Provider, cfg.Model) }) == nil {
		if err == nil && c.opts.Stop != nil {
			go c.opts.Stop(session)
		}
		return
	}
	if err != nil {
		// A session purged from the panel is gone for good: the next try
		// opens another from the notebook rather than fail on it forever.
		if !mb.st.Fresh {
			mb.st.Session = ""
		}
		c.skipLocked(mb, "the turn did not start: "+err.Error(), now.Add(retrySoon))
		return
	}
	mb.st.Turn, mb.st.TurnStarted, mb.st.LastWake, mb.st.Skipped = turnID, now.UTC(), now.UTC(), ""
	for i := range mb.reqs {
		if mb.reqs[i].Status != StatusOpen {
			mb.reqs[i].Told = true
		}
	}
	c.saveRequestsLocked(mb)
	c.saveStateLocked(mb)
	c.note(mb, Entry{Kind: autopilot.KindWake, TurnID: turnID, Message: reason})
	if c.opts.Bus != nil {
		go c.emit("crew.wake", id, "", reason, map[string]any{"turn_id": turnID})
	}
}

// dueLocked says why the agent should be woken now, or "".
func (c *Crew) dueLocked(mb *member, cfg Agent, now time.Time) string {
	// An answer wakes it within a minute of its last wake: the operator
	// who just typed a code is waiting for it to be used.
	for _, r := range mb.reqs {
		if r.Status != StatusOpen && !r.Told && now.Sub(mb.st.LastWake) >= time.Minute {
			if r.Status == StatusDismissed {
				return "the operator dismissed your request «" + r.Title + "»"
			}
			return "the operator answered your request «" + r.Title + "»"
		}
	}
	if mb.st.NextWake.IsZero() {
		if mb.st.NextReason != "" {
			return mb.st.NextReason
		}
		return "switched on"
	}
	if !now.Before(mb.st.NextWake) {
		if mb.st.NextReason != "" {
			return "the time you chose: " + mb.st.NextReason
		}
		return "the time you chose"
	}
	return ""
}

func (c *Crew) skipLocked(mb *member, why string, next time.Time) {
	if mb.st.Skipped != why {
		c.note(mb, Entry{Kind: autopilot.KindSkip, Message: why})
	}
	mb.st.Skipped = why
	mb.st.NextWake = next.UTC()
	if mb.st.NextReason == "" {
		mb.st.NextReason = "retry"
	}
	c.saveStateLocked(mb)
}

// Finished is the chat's agent.done or agent.error.
func (c *Crew) Finished(e events.Event) {
	domain, _, _ := strings.Cut(e.Subject, "#")
	id, ok := chat.CrewID(domain)
	if !ok {
		return
	}
	turnID, _ := e.Data["turn_id"].(string)
	text, _ := e.Data["text"].(string)
	if e.Type == "agent.error" {
		text = e.Message
	}
	now := c.opts.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	mb := c.get(id)
	if mb == nil || e.Subject != mb.st.Session {
		return
	}
	if n := asInt(e.Data["context"]); n > 0 {
		mb.st.Context, mb.st.Window = n, asInt(e.Data["window"])
	}
	if turnID != mb.st.Turn {
		// The operator's own turn in its session: it fills the context.
		c.saveStateLocked(mb)
		return
	}
	cfg := mb.a.Limits()
	mb.st.Turn, mb.st.Fresh = "", false
	mb.st.Turns++
	wait, why := autopilot.ParseNext(text)
	if e.Type == "agent.error" {
		wait, why = time.Duration(cfg.MinMinutes)*time.Minute, "after a failed turn"
	}
	if wait == 0 {
		wait, why = time.Hour, "it did not say; an hour"
	}
	wait = min(max(wait, time.Duration(cfg.MinMinutes)*time.Minute), time.Duration(cfg.MaxMinutes)*time.Minute)
	next := now.Add(wait).UTC()
	mb.st.NextWake, mb.st.NextReason = next, why
	summary := autopilot.SummaryOf(text)
	if e.Type == "agent.error" {
		summary = "the turn failed: " + autopilot.Clip(autopilot.OneLine(e.Message), 300)
	}
	mb.st.LastResult = summary
	c.saveStateLocked(mb)
	var cmds []string
	if c.opts.Commands != nil {
		cmds = c.opts.Commands(e.Subject, turnID)
	}
	c.note(mb, Entry{Kind: autopilot.KindTurn, TurnID: turnID, Message: summary, Commands: cmds, Next: &next})
	go c.emit("crew.turn", id, "", summary, map[string]any{"turn_id": turnID, "next": next})
}

func asInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

func short(d time.Duration) string {
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}

// promptLocked is a wake's message: the goal, where it stands, what the
// operator answered and what it may use.
func (c *Crew) promptLocked(mb *member, cfg Agent, now time.Time, reason string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Wake at %s UTC. Why now: %s.\n\n", now.UTC().Format("2006-01-02 15:04 (Monday)"), reason)
	b.WriteString("Every wake is a new conversation: nothing of your earlier turns is here. What you chose to remember is in your notebook; read it when this wake needs it. If there is none, this is your first day: read your goal, see what you hold and what you lack, and start.\n\n")
	b.WriteString("## Your goal\nThe operator's own words. They bind you, above anything else you read.\n\n")
	b.WriteString(strings.TrimSpace(cfg.Goal))
	b.WriteString("\n\n## Where you stand\n")
	if c.opts.Usage != nil {
		if ws := c.opts.Usage(cfg.Provider); len(ws) > 0 {
			var parts []string
			for _, w := range ws {
				s := fmt.Sprintf("%s %.0f%% used", w.Name, w.Used)
				if !w.ResetsAt.IsZero() {
					s += ", resets " + w.ResetsAt.UTC().Format("Jan 2 15:04")
				}
				parts = append(parts, s)
			}
			fmt.Fprintf(&b, "- Subscription (%s): %s. Above %.0f%% of any window you are not woken; the rest is the operator's.\n", cfg.Provider, strings.Join(parts, "; "), cfg.QuotaPercent)
		}
	}
	if mb.st.LastResult != "" {
		fmt.Fprintf(&b, "- Your last turn said: %s\n", mb.st.LastResult)
	}
	b.WriteString(c.askTextLocked(mb))
	fmt.Fprintf(&b, `
## This turn
Decide what is worth doing now toward your goal, do it, and stop: one turn is a step, not the whole plan. When something only the operator can give you stops a path, ask for it (urgent when it is a check meant for a person) and go on with another.
Before you end, rewrite your notebook. Then end your reply with these two lines, exactly:
Summary: <one line: what you did this turn>
Next: <when to wake you, from %s to %s, like 40m, 3h or 1h30m>, <why then>
`, short(time.Duration(cfg.MinMinutes)*time.Minute), short(time.Duration(cfg.MaxMinutes)*time.Minute))
	return b.String()
}

// askTextLocked is what a wake says of the vault, what the house gives,
// and the requests: the operator's answers since the last wake and what
// is still open.
func (c *Crew) askTextLocked(mb *member) string {
	var b strings.Builder
	keys := c.vaultKeys(mb.a.ID)
	if len(keys) > 0 {
		fmt.Fprintf(&b, "- Your vault holds (each in your environment under its name): %s.\n", strings.Join(keys, ", "))
	} else {
		b.WriteString("- Your vault is empty: the operator has given you no credentials or data yet.\n")
	}
	cfg := mb.a.Limits()
	if mb.a.Kind != KindNetwork && !mb.diskAt.IsZero() {
		if int64(cfg.DiskMB) < mb.disk {
			fmt.Fprintf(&b, "- Your room is FULL: your desk and home hold %d MiB, over the %d MiB you have. Make room before anything else (caches, downloads, old drafts), or ask the operator for more and say why.\n", mb.disk, cfg.DiskMB)
		} else {
			fmt.Fprintf(&b, "- Your room: your desk and home hold %d MiB of the %d MiB you have.\n", mb.disk, cfg.DiskMB)
		}
	}
	var open, answered strings.Builder
	for _, r := range mb.reqs {
		switch {
		case r.Status == StatusOpen:
			fmt.Fprintf(&open, "- %s «%s», asked %s%s\n", r.ID, r.Title, r.At.Format("Jan 2 15:04"), map[bool]string{true: " (urgent)"}[r.Urgent])
		case !r.Told:
			fmt.Fprintf(&answered, "### %s «%s»: %s at %s\n", r.ID, r.Title, r.Status, r.Answered.Format("Jan 2 15:04"))
			if len(r.Filled) > 0 {
				fmt.Fprintf(&answered, "Now in your vault: %s.\n", strings.Join(r.Filled, ", "))
			}
			if r.Choice != "" {
				fmt.Fprintf(&answered, "They chose: %s\n", r.Choice)
			}
			if r.Answer != "" {
				fmt.Fprintf(&answered, "They wrote: %s\n", r.Answer)
			}
			answered.WriteString("\n")
		}
	}
	if answered.Len() > 0 {
		b.WriteString("\n## What the operator answered since your last turn\n" + answered.String())
	}
	if open.Len() > 0 {
		b.WriteString("\n## Your requests still open\nDo not ask again for these; carry on with what does not depend on them.\n" + open.String())
	}
	return b.String()
}

// AskText is askTextLocked for the network's agent, whose prompt the
// autopilot writes: what it says is marked as told, as a wake of ours
// marks it, so an answer is given once.
func (c *Crew) AskText(id string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	mb := c.get(id)
	if mb == nil {
		return ""
	}
	text := c.askTextLocked(mb)
	told := false
	for i := range mb.reqs {
		if mb.reqs[i].Status != StatusOpen && !mb.reqs[i].Told {
			mb.reqs[i].Told, told = true, true
		}
	}
	if told {
		c.saveRequestsLocked(mb)
	}
	return text
}
