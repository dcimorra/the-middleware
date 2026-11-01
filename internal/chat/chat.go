// Package chat runs the conversations with the agents: the panel's one per
// site, the reserved one named "middleware" about the installation itself,
// and the ones a site's own code opens through the site door.
//
// A conversation has a name. The panel's is the domain; the others are
// "<domain>#<id>". Everything else (the jail, the caps, the processes a
// turn leaves behind, the cost) follows from the name, so one machinery
// serves all of them and none runs silently on the side. A turn is a
// provider run inside the site's jail; its events are re-emitted on the bus
// with the name as subject, and both sides of the exchange are written to
// data/chat/<domain>.json (the panel's) or data/chat/<domain>/<id>.json
// with the provider's session id, so the next turn resumes where this one
// ended.
package chat

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dcimorra/the-middleware/internal/agent"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/settings"
)

// Middleware is the reserved conversation name. It is not a valid domain,
// so it can never collide with a site folder.
const Middleware = "middleware"

// Turn is one message, either side.
type Turn struct {
	ID        string    `json:"id"`
	Role      string    `json:"role"` // "user" | "assistant" | "error"
	Text      string    `json:"text"`
	Cost      float64   `json:"cost,omitempty"`
	At        time.Time `json:"at"`
	SessionID string    `json:"session_id,omitempty"`
	// Provider is the agent that answered, on its side's turns: a
	// conversation can change agent between two messages.
	Provider string   `json:"provider,omitempty"`
	Tools    []string `json:"tools,omitempty"`
	// Questions are the decisions the agent asked for in this turn.
	Questions []Question `json:"questions,omitempty"`
	// Threads are what subagents said, apart from the main answer: kept,
	// readable, and out of the way of what the agent itself replied.
	Threads []Thread `json:"threads,omitempty"`
	// Steps are the turn as it happened, in order: what the agent said
	// and each tool it used, with its answer. Text above is the words
	// alone, for whoever only wants the reply.
	Steps []Step `json:"steps,omitempty"`
}

// Thread is one subagent's part of a turn.
type Thread struct {
	ID    string   `json:"id"`
	Text  string   `json:"text,omitempty"`
	Tools []string `json:"tools,omitempty"`
	// Description and Prompt are what it was asked, as the call that
	// launched it said; Run its state (RunRunning, RunCut, or what the
	// provider said: "completed", "failed"…).
	Description string `json:"description,omitempty"`
	Prompt      string `json:"prompt,omitempty"`
	Run         string `json:"run,omitempty"`
	Background  bool   `json:"background,omitempty"`
	Steps       []Step `json:"steps,omitempty"`
}

// Step is one thing in a turn: words, or a tool used.
type Step struct {
	Kind string   `json:"kind"` // "text" | "tool"
	Text string   `json:"text,omitempty"`
	Tool *ToolUse `json:"tool,omitempty"`
}

// ToolUse is one use of a tool, with its answer once it came back.
type ToolUse struct {
	ID    string         `json:"id,omitempty"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input,omitempty"`
	// Output is the answer, cut at maxToolText; Error says it failed.
	Output string `json:"output,omitempty"`
	Error  bool   `json:"error,omitempty"`
	Done   bool   `json:"done,omitempty"`
	// Run is set on a call that launched a subagent: its state, which is
	// not Done (one in the background answers "launched" at once and
	// goes on working).
	Run string `json:"run,omitempty"`
}

// A subagent's states of the house's own; the rest are the provider's.
const (
	RunRunning = "running"
	// RunCut is a subagent whose turn ended under it: without it, it would
	// show as running for ever.
	RunCut = "cut"
)

// maxToolText is how much of a tool's input string or output is kept: the
// panel shows a fold, not a file.
const maxToolText = 4000

// isAgentTool says a call launches a subagent.
func isAgentTool(name string) bool { return name == "Task" || name == "Agent" }

func clipText(s string) string {
	if len(s) <= maxToolText {
		return s
	}
	cut := maxToolText
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// clipInput keeps a tool's input readable: long strings (a file written
// whole) are cut, the rest kept as it came.
func clipInput(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		if s, ok := v.(string); ok {
			out[k] = clipText(s)
		} else {
			out[k] = v
		}
	}
	return out
}

// Origin says who opened a conversation.
const (
	OriginPanel = "panel"
	// OriginSite is the site's own code, through the site door: a program,
	// not a person, on the other side.
	OriginSite = "site"
	// OriginRepair is the house's automatic repairs of a site (autofix):
	// nobody on the other side, and a prompt made of the site's own log.
	OriginRepair = "repair"
)

// Conversation is one conversation's file.
type Conversation struct {
	Domain string `json:"domain"`
	// ID is empty for the panel's conversation of a site.
	ID      string    `json:"id,omitempty"`
	Origin  string    `json:"origin,omitempty"`
	Title   string    `json:"title,omitempty"`
	Created time.Time `json:"created,omitzero"`
	// System, when set, replaces the house's preamble: a site door
	// conversation says who Claude is for it.
	System string `json:"system,omitempty"`
	// Tools is the tool level of a site door conversation (see Levels).
	Tools string `json:"tools,omitempty"`
	// Hidden keeps it out of the panel's list; it is still kept, costed
	// and readable by its name.
	Hidden    bool   `json:"hidden,omitempty"`
	Provider  string `json:"provider"`
	Model     string `json:"model,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	// Sessions is each agent's own session in this conversation: one
	// agent's id means nothing to another, and Codex handed Claude's
	// failed the turn. SessionID stays as the last agent's, for what
	// reads it.
	Sessions map[string]string `json:"sessions,omitempty"`
	Turns    []Turn            `json:"turns"`
	// Running is not stored: it is filled by History from the manager.
	Running bool `json:"running,omitempty"`
	// Live is the running turn so far, filled by History too.
	Live []Live `json:"live,omitempty"`
	// Queued are the messages waiting for the running turn to end, in the
	// order they were written; filled by History too.
	Queued []Queued `json:"queued,omitempty"`
}

// Queued is one message written while a turn ran.
type Queued struct {
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

// queueFile is data/chat/queued/<name>.json: what waits for the running
// turn, so a supervisor that restarts mid-turn still sends it.
type queueFile struct {
	Messages []Queued `json:"messages"`
	Model    string   `json:"model,omitempty"`
	Provider string   `json:"provider,omitempty"`
}

// Options configure a Manager.
type Options struct {
	Root     string
	Bus      *events.Bus
	Registry *agent.Registry
	// DefaultProvider is used when the site's manifest names none. It
	// overrides Limits.DefaultProvider; empty takes the latter.
	DefaultProvider string
	// Ledger defaults to one under Root/data.
	Ledger *agent.Ledger
	// Limits is the concurrency section of the settings; a zero value
	// takes the defaults. SetLimits changes it live.
	Limits settings.Agents
	// Path, when set, is the PATH a turn runs with instead of the
	// supervisor's: the wiring puts the managed CLIs and their Node first.
	Path func() string
	// ProcRoot is /proc; tests point it elsewhere.
	ProcRoot string
	// Usage keeps what a turn's stream says of the subscription's limits;
	// defaults to one under Root/data.
	Usage *agent.UsageBook
	// TurnEnv, when set, adds variables to the turns of a site's own
	// conversations (the panel's, not the ones its code opens through the
	// door): what lets `git push` reach the site's repository.
	TurnEnv func(domain string) map[string]string
}

// Usage is the book of subscription limits the turns fill.
func (m *Manager) Usage() *agent.UsageBook { return m.opts.Usage }

// Manager runs the conversations.
type Manager struct {
	// asks caches, per conversation file, whether it ends asking (Waiting).
	askMu sync.Mutex
	asks  map[string]askState

	// repairMu keeps two repairs from opening two conversations.
	repairMu sync.Mutex

	opts    Options
	mu      sync.Mutex
	running map[string]*running
	limits  settings.Agents
}

type running struct {
	turnID   string
	provider string
	origin   string
	model    string
	started  time.Time
	deadline time.Time
	run      agent.Run
	cancel   context.CancelFunc
	// done closes when the turn is written and its slot free.
	done chan struct{}
	// queued are messages sent while the turn ran, kept for the next, in
	// the order they came. They are also written to data/chat/queued/ on
	// every arrival: kept only here, leaving the page and coming back
	// showed none of them, and a restart lost them.
	queued         []Queued
	queuedModel    string
	queuedProvider string
	// live is what the turn showed so far, so a panel that reloads (or
	// opens later, on another device) sees the turn where it is and not
	// an empty bubble until it ends.
	live []Live
}

// Live is one progress event of a running turn, as the stream sent it.
// Seq lets a panel that read them drop the same events arriving again on
// the stream.
type Live struct {
	Seq  int            `json:"seq"`
	Type string         `json:"type"`
	Data map[string]any `json:"data"`
}

// maxLive bounds what a turn keeps in memory: each piece is a whole
// message or tool call, not a token, so a long turn stays far below it.
const maxLive = 5000

// progress emits a progress event of a running turn and keeps it for
// History.
func (m *Manager) progress(typ, name, msg string, data map[string]any) {
	m.mu.Lock()
	if r := m.running[name]; r != nil {
		data["seq"] = len(r.live) + 1
		if len(r.live) < maxLive {
			r.live = append(r.live, Live{Seq: len(r.live) + 1, Type: typ, Data: data})
		}
	}
	m.mu.Unlock()
	m.emit(typ, name, "", msg, data)
}

// AgentPIDs are the agent CLIs of the turns running now. The guard asks
// for them so it does not judge an agent as a site process: an agent's
// prompt names other sites' folders (the ones it is denied), and the
// guard killed every turn a second in for "working in another site's
// folder". The jail is what bounds an agent.
func (m *Manager) AgentPIDs() []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []int
	for _, r := range m.running {
		if pr, ok := r.run.(interface{ PID() int }); ok && pr.PID() > 1 {
			out = append(out, pr.PID())
		}
	}
	return out
}

// withLimit is the turn's context: cut at TurnLimit when there is one.
func withLimit(started time.Time) (context.Context, context.CancelFunc, time.Time) {
	if TurnLimit <= 0 {
		ctx, cancel := context.WithCancel(context.Background())
		return ctx, cancel, time.Time{}
	}
	deadline := started.Add(TurnLimit)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	return ctx, cancel, deadline
}

// Name is a conversation's name: the domain for the panel's, and
// "<domain>#<id>" for the others.
func Name(domain, id string) string {
	if id == "" {
		return domain
	}
	return domain + "#" + id
}

// split takes a name apart.
func split(name string) (domain, id string) {
	domain, id, _ = strings.Cut(name, "#")
	return domain, id
}

func validID(id string) bool {
	if id == "" || len(id) > 32 {
		return id == ""
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// ErrBusy says a turn is already running for that conversation.
var ErrBusy = errors.New("a turn is already running")

// ErrCapacity says the house, or one provider, has no free slot. The
// numbers are what the caller shows: a 429 that says "8 of 8" is one the
// operator can act on in Settings.
type ErrCapacity struct {
	Used, Max int
	// Provider is set when the provider's own cap was the one hit.
	Provider                  string
	ProviderUsed, ProviderMax int
}

// Error implements error.
func (e *ErrCapacity) Error() string {
	if e.Provider != "" {
		return fmt.Sprintf("no capacity for %s: %d of %d %s turns running; raise agents.per_provider.%s in Settings or wait", e.Provider, e.ProviderUsed, e.ProviderMax, e.Provider, e.Provider)
	}
	return fmt.Sprintf("no capacity: %d of %d agent turns running; raise agents.max_concurrent in Settings or wait", e.Used, e.Max)
}

// New returns a manager. Nothing runs until Send.
func New(opts Options) *Manager {
	if opts.Bus == nil {
		opts.Bus = events.New("")
	}
	if opts.Registry == nil {
		opts.Registry = agent.NewRegistry()
	}
	if opts.Usage == nil {
		opts.Usage = agent.OpenUsageBook(filepath.Join(opts.Root, "data", "agents", "usage.json"))
	}
	if opts.Ledger == nil {
		opts.Ledger = agent.NewLedger(filepath.Join(opts.Root, "data"))
	}
	// A relative root would put relative paths in the jail's deny list and
	// in the child's HOME, and a deny rule is only as good as its path.
	if abs, err := filepath.Abs(opts.Root); err == nil {
		opts.Root = abs
	}
	m := &Manager{opts: opts, running: map[string]*running{}}
	m.SetLimits(opts.Limits)
	return m
}

// SetLimits replaces the concurrency limits at once. Turns already
// running are not stopped by a lower cap: they finish, and the next Send
// sees the new numbers. The default provider follows the settings unless
// the Options pinned one.
func (m *Manager) SetLimits(a settings.Agents) {
	d := settings.DefaultAgents()
	if a.MaxConcurrent == 0 && a.DefaultProvider == "" && a.PerProvider == nil {
		a = d
	}
	if a.Validate() != nil {
		if a.MaxConcurrent < 1 || a.MaxConcurrent > settings.MaxAgents {
			a.MaxConcurrent = d.MaxConcurrent
		}
		if a.ReservedForHouse < 0 || a.ReservedForHouse >= a.MaxConcurrent {
			a.ReservedForHouse = 0
			if a.MaxConcurrent > 1 {
				a.ReservedForHouse = d.ReservedForHouse
			}
		}
		if a.DefaultProvider == "" {
			a.DefaultProvider = d.DefaultProvider
		}
	}
	if m.opts.DefaultProvider != "" {
		a.DefaultProvider = m.opts.DefaultProvider
	}
	per := map[string]int{}
	for k, v := range a.PerProvider {
		if v > 0 {
			per[k] = v
		}
	}
	a.PerProvider = per
	m.mu.Lock()
	m.limits = a
	m.mu.Unlock()
}

// Capacity is the live picture: slots used and available, per provider,
// and which conversations hold them.
type Capacity struct {
	Used      int                         `json:"used"`
	Max       int                         `json:"max"`
	Reserved  int                         `json:"reserved_for_house"`
	Providers map[string]ProviderCapacity `json:"providers"`
	Running   []RunningTurn               `json:"running"`
}

// ProviderCapacity is one provider's share; Max 0 means no cap of its own.
type ProviderCapacity struct {
	Used int `json:"used"`
	Max  int `json:"max"`
}

// RunningTurn is one conversation with a turn in progress.
type RunningTurn struct {
	Domain   string    `json:"domain"`
	Provider string    `json:"provider"`
	Started  time.Time `json:"started"`
	TurnID   string    `json:"turn_id"`
}

// Capacity returns the live numbers.
func (m *Manager) Capacity() Capacity {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.capacityLocked()
}

func (m *Manager) capacityLocked() Capacity {
	c := Capacity{Used: len(m.running), Max: m.limits.MaxConcurrent, Reserved: m.limits.ReservedForHouse, Providers: map[string]ProviderCapacity{}, Running: []RunningTurn{}}
	for _, key := range m.opts.Registry.Keys() {
		c.Providers[key] = ProviderCapacity{Max: m.limits.PerProvider[key]}
	}
	for domain, r := range m.running {
		p := c.Providers[r.provider]
		p.Used++
		if p.Max == 0 {
			p.Max = m.limits.PerProvider[r.provider]
		}
		c.Providers[r.provider] = p
		c.Running = append(c.Running, RunningTurn{Domain: domain, Provider: r.provider, Started: r.started, TurnID: r.turnID})
	}
	sort.Slice(c.Running, func(i, j int) bool { return c.Running[i].Started.Before(c.Running[j].Started) })
	return c
}

// admit decides, under the lock, whether a turn for domain on provider
// may start now. The house keeps its reserved slots: a site is refused
// once used reaches max minus reserved, the house once it reaches max.
func (m *Manager) admit(domain, provider, origin string) error {
	used := len(m.running)
	limit := m.limits.MaxConcurrent
	if domain != Middleware {
		limit -= m.limits.ReservedForHouse
	}
	if origin == OriginSite {
		// The last free slot is never a site's: if a loop in a site's code
		// could take every one, whoever is at the panel at three in the
		// morning would have no way in to stop it.
		limit--
	}
	if used >= limit {
		return &ErrCapacity{Used: used, Max: m.limits.MaxConcurrent}
	}
	if pmax := m.limits.PerProvider[provider]; pmax > 0 {
		pused := 0
		for _, r := range m.running {
			if r.provider == provider {
				pused++
			}
		}
		if pused >= pmax {
			return &ErrCapacity{Used: used, Max: m.limits.MaxConcurrent, Provider: provider, ProviderUsed: pused, ProviderMax: pmax}
		}
	}
	return nil
}

func (m *Manager) file(name string) string {
	domain, id := split(name)
	if id == "" {
		return filepath.Join(m.opts.Root, "data", "chat", domain+".json")
	}
	return filepath.Join(m.opts.Root, "data", "chat", domain, id+".json")
}

// workdir resolves where a conversation runs and fails for anything that
// is not a site folder or the reserved name: a domain that merely looks
// right must not open a conversation in a folder that does not exist.
func (m *Manager) workdir(name string) (string, error) {
	domain, id := split(name)
	if !validID(id) {
		return "", fmt.Errorf("%q is not a conversation", name)
	}
	if domain == Middleware {
		return m.opts.Root, nil
	}
	if !manifest.ValidName(domain) {
		return "", fmt.Errorf("%q is not a site or project", domain)
	}
	dir := filepath.Join(m.opts.Root, "sites", domain)
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return "", fmt.Errorf("%s: no such site", domain)
	}
	return dir, nil
}

// Settings is what a conversation would run with: provider and model
// from the manifest with the manager's default, the cap, the jail.
type Settings struct {
	Provider string
	Model    string
	Cap      float64
	Workdir  string
	Jail     agent.Jail
}

// Resolve computes the settings for a conversation without starting
// anything.
func (m *Manager) Resolve(name string) (Settings, error) {
	domain, _ := split(name)
	dir, err := m.workdir(name)
	if err != nil {
		return Settings{}, err
	}
	m.mu.Lock()
	s := Settings{Provider: m.limits.DefaultProvider, Workdir: dir}
	m.mu.Unlock()
	if domain != Middleware {
		site := agent.ReadSiteSettings(dir)
		if site.Provider != "" {
			s.Provider = site.Provider
		}
		s.Model = site.Model
		s.Cap = site.MonthlyCapUSD
	}
	home := filepath.Join(m.opts.Root, "data", "homes", domain)
	s.Jail = agent.NewJail(m.opts.Root, dir, home)
	if domain != Middleware {
		var dirs []string
		for _, r := range lentTo(dir) {
			other := filepath.Join(m.opts.Root, "sites", r)
			if r != domain && manifest.ValidName(r) {
				if st, err := os.Stat(other); err == nil && st.IsDir() {
					dirs = append(dirs, other)
				}
			}
		}
		s.Jail.Lend(dirs...)
	}
	return s, nil
}

// History returns the conversation, empty when it has not started.
func (m *Manager) History(domain string) (Conversation, error) {
	if _, err := m.workdir(domain); err != nil {
		return Conversation{}, err
	}
	c := m.load(domain)
	if c.Provider == "" {
		if s, err := m.Resolve(domain); err == nil {
			c.Provider, c.Model = s.Provider, s.Model
		}
	}
	m.mu.Lock()
	if r, ok := m.running[domain]; ok {
		c.Running = true
		c.Live = append([]Live(nil), r.live...)
		c.Queued = append([]Queued(nil), r.queued...)
	}
	m.mu.Unlock()
	return c, nil
}

func (m *Manager) load(name string) Conversation {
	var c Conversation
	if err := fsx.ReadJSON(m.file(name), &c); err != nil {
		c = Conversation{}
	}
	c.Domain, c.ID = split(name)
	if c.Turns == nil {
		c.Turns = []Turn{}
	}
	return c
}

func (m *Manager) save(c Conversation) error {
	c.Running = false
	return fsx.WriteJSON(m.file(Name(c.Domain, c.ID)), c, 0o600)
}

// Reset forgets the conversation: the next turn starts a new session.
func (m *Manager) Reset(domain string) error {
	if _, err := m.workdir(domain); err != nil {
		return err
	}
	m.mu.Lock()
	_, busy := m.running[domain]
	m.mu.Unlock()
	if busy {
		return ErrBusy
	}
	if err := os.Remove(m.file(domain)); err != nil && !os.IsNotExist(err) {
		return err
	}
	m.emit("chat.reset", domain, "", "conversation reset", nil)
	return nil
}

// Running reports whether a turn is in progress.
func (m *Manager) Running(domain string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.running[domain]
	return ok
}

// Stop ends the running turn, if any.
func (m *Manager) Stop(domain string) error {
	m.mu.Lock()
	r, ok := m.running[domain]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("%s: no turn is running", domain)
	}
	r.run.Stop()
	r.cancel()
	return nil
}

// Send starts a turn and returns at once with its id; progress arrives on
// the bus. At most one turn runs per conversation: two agents editing the
// same folder at the same time would each undo the other.
func (m *Manager) Send(name, text, model string) (string, error) {
	return m.SendWith(name, text, SendOptions{Model: model})
}

// SendOptions are a turn's own choices.
type SendOptions struct {
	Model string
	// Provider runs this message with that agent instead of the one the
	// manifest (or the house's settings) names; the house had a picker
	// that went nowhere without it.
	Provider string
	// Queue keeps a message that arrives while a turn runs and sends it
	// when the turn ends, instead of refusing it: the operator who thinks
	// of something mid-turn should not have to wait and retype it.
	Queue bool
	// Allow, when set, has the last word on the agent and model a turn
	// runs with: a delegate's limits (auth.PickAgentModel). It is asked
	// after the defaults are settled, so a default the user may not use
	// is refused or swapped too, and with the provider's models here.
	Allow func(provider, model string, known []string) (string, error)
}

// ErrQueued says the message was kept for when the running turn ends.
var ErrQueued = errors.New("queued: it is sent when the running turn ends")

// SendWith is Send with options.
func (m *Manager) SendWith(name, text string, o SendOptions) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", errors.New("empty message")
	}
	domain, _ := split(name)
	s, err := m.Resolve(name)
	if err != nil {
		return "", err
	}
	c := m.load(name)
	if o.Provider != "" && o.Provider != s.Provider && c.Origin != OriginSite {
		// The manifest's model is its own agent's.
		s.Provider, s.Model = o.Provider, ""
	}
	if c.Origin == OriginSite {
		// The site door runs the agent its manifest names now (the caller
		// says it in Provider), not the one the site's panel conversation
		// uses: a door moved to another agent moves its old conversations
		// too, each agent keeping its own session. Without it, the one
		// the conversation last ran with (Claude for one from before
		// there was a choice).
		door := c.Provider
		if o.Provider != "" {
			door = o.Provider
		}
		if door == "" {
			door = "claude"
		}
		if s.Provider != door {
			// The site's own model is another provider's name.
			s.Model = ""
		}
		s.Provider = door
	}
	provider, err := m.opts.Registry.Get(s.Provider)
	model := o.Model
	// The conversation's model is its last agent's (or the one it was
	// opened with, before any agent ran it): another agent takes its own
	// default, not a name it does not know.
	if model == "" && (c.Provider == "" || c.Provider == s.Provider) {
		model = c.Model
	}
	if model == "" {
		model = s.Model
	}
	// The limits answer before anything about the provider here does: a
	// delegate refused an agent learns nothing of whether the house has it.
	if o.Allow != nil {
		var known []string
		if err == nil {
			known = provider.Models()
		}
		var aerr error
		if model, aerr = o.Allow(s.Provider, model, known); aerr != nil {
			return "", aerr
		}
	}
	if err != nil {
		return "", err
	}
	if ok, why := provider.Available(); !ok {
		return "", fmt.Errorf("%s: %s", s.Provider, why)
	}
	if err := m.opts.Ledger.Allow(domain, s.Cap, time.Now()); err != nil {
		m.emit("agent.cost_cap", name, "warn", err.Error(), map[string]any{"cap": s.Cap})
		return "", err
	}
	m.mu.Lock()
	if r, busy := m.running[name]; busy {
		if o.Queue {
			r.queued = append(r.queued, Queued{Text: text, At: time.Now()})
			if model != "" {
				r.queuedModel = model
			}
			if o.Provider != "" {
				r.queuedProvider = o.Provider
			}
			n := len(r.queued)
			werr := fsx.WriteJSON(m.queuePath(name), queueFile{Messages: r.queued, Model: r.queuedModel, Provider: r.queuedProvider}, 0o600)
			m.mu.Unlock()
			if werr != nil {
				m.emit("agent.error", name, "warn", "the queued message is kept in memory only: "+werr.Error(), nil)
			}
			m.emit("agent.queued", name, "", "", map[string]any{"queued": n, "text": text})
			return "", ErrQueued
		}
		m.mu.Unlock()
		return "", ErrBusy
	}
	if err := m.admit(domain, s.Provider, c.Origin); err != nil {
		cap := m.capacityLocked()
		m.mu.Unlock()
		m.emit("agent.capacity", name, "warn", err.Error(), map[string]any{"used": cap.Used, "max": cap.Max, "provider": s.Provider, "providers": cap.Providers})
		return "", err
	}
	turnID := newID()
	// One instant for both: the deadline the turn is cut at and the one
	// the end of the turn is judged by. Two time.Now() calls a moment
	// apart made a turn cut at its limit look, now and then, like one that
	// ended on its own without a result.
	started := time.Now()
	ctx, cancel, deadline := withLimit(started)
	m.running[name] = &running{turnID: turnID, provider: s.Provider, origin: c.Origin, model: model, started: started, deadline: deadline, cancel: cancel, done: make(chan struct{})}
	m.mu.Unlock()

	fail := func(err error) (string, error) {
		cancel()
		m.mu.Lock()
		if r := m.running[name]; r != nil {
			close(r.done)
		}
		delete(m.running, name)
		m.mu.Unlock()
		return "", err
	}
	// A conversation written before each agent kept its own session: what
	// it has is its last agent's, and so are its answers.
	if c.Sessions == nil && c.Provider != "" {
		if c.SessionID != "" {
			c.Sessions = map[string]string{c.Provider: c.SessionID}
		}
		for i := range c.Turns {
			if c.Turns[i].Role != "user" && c.Turns[i].Provider == "" {
				c.Turns[i].Provider = c.Provider
			}
		}
	}
	session, catchUp := sessionFor(c, s.Provider)
	c.Provider, c.Model = s.Provider, model
	if c.Title == "" {
		c.Title = TitleOf(text)
	}
	c.Turns = append(c.Turns, Turn{ID: turnID, Role: "user", Text: text, At: time.Now()})
	if err := m.save(c); err != nil {
		return fail(err)
	}
	// The skill goes in before every turn, not once at start: the home
	// may be new, and the binary may have been upgraded since the last one.
	if err := installSkillFor(s.Jail, s.Provider); err != nil {
		return fail(fmt.Errorf("skill not installed: %w", err))
	}
	var system string
	prompt, lend := m.attach(name, catchUp+text)
	req := agent.Request{
		Workdir:   s.Workdir,
		Prompt:    prompt,
		Model:     model,
		SessionID: session,
		Jail:      s.Jail,
		// OpenSpec's CLI reports usage unless told not to; a turn that
		// runs it on a site's project tells it not to.
		Env:    map[string]string{"PATH": m.agentPath(), MarkVar: name, "OPENSPEC_TELEMETRY": "0"},
		Name:   name,
		Stream: m.streamPath(name, turnID),
	}
	if c.Origin == OriginSite {
		system = SiteSystem(c)
		level := Level(c.Tools)
		req.Tools = level.Tools
		req.Disallowed = level.Disallowed
		if !level.Writes {
			// The jail closes every other site's .env, and with permissions
			// skipped what is not denied is allowed: a level that cannot
			// write must not read this site's own secrets either. V1's "web"
			// level read and quoted its own .env before this.
			req.Jail.Secrets = append(req.Jail.Secrets, filepath.Join(s.Workdir, "**", ".env*"))
		}
	} else {
		system = Preamble(domain, s.Workdir, s.Jail)
		// Only Claude Code loads the skill by itself; the others are told
		// to read it, and the project's own CLAUDE.md when it has no
		// AGENTS.md (the file the others read), so a site written with
		// Claude keeps its rules whatever agent works on it next.
		if needsSkillHint[s.Provider] {
			if h := projectHint(s.Workdir); h != "" {
				system = h + "\n\n" + system
			}
			system = SkillHint + "\n\n" + system
		}
	}
	req.System = system
	m.mu.Lock()
	turnEnv := m.opts.TurnEnv
	m.mu.Unlock()
	// The house's own conversation pushes with whatever the machine has;
	// a site's code, which answers the internet, gets nothing here, and
	// neither does a repair: it runs unattended on a prompt that quotes
	// the site's log, which any visitor writes, and a push is how what a
	// prompt injected there would leave the machine.
	if c.Origin == OriginRepair {
		system += "\nThis is an automatic repair with nobody watching: `git push` is not available in it, so leave committing and pushing to the operator.\n"
		req.System = system
	}
	if turnEnv != nil && domain != Middleware && c.Origin != OriginSite && c.Origin != OriginRepair {
		for k, v := range turnEnv(domain) {
			if _, set := req.Env[k]; !set {
				req.Env[k] = v
			}
		}
	}
	if lend != "" {
		req.Jail.Lend(lend)
	}
	run, err := provider.Start(ctx, req)
	if err != nil {
		m.emit("agent.error", name, "error", err.Error(), map[string]any{"turn_id": turnID})
		return fail(err)
	}
	m.mu.Lock()
	m.running[name].run = run
	m.mu.Unlock()
	if pr, ok := run.(interface{ PID() int }); ok {
		_ = fsx.WriteJSON(m.runningPath(name), runningFile{Name: name, TurnID: turnID, PID: pr.PID(), Stream: req.Stream, Provider: s.Provider, Model: model, Origin: c.Origin, Started: time.Now()}, 0o600)
	}
	m.emit("agent.started", name, "", "", map[string]any{"turn_id": turnID, "provider": s.Provider, "model": model})
	go m.follow(name, turnID, s, run, cancel)
	return turnID, nil
}

// follow consumes a run to its end and records the outcome.
func (m *Manager) follow(name, turnID string, s Settings, run agent.Run, cancel context.CancelFunc) {
	defer cancel()
	domain, _ := split(name)
	origin, model := "", s.Model
	var deadline time.Time
	m.mu.Lock()
	if r := m.running[name]; r != nil {
		origin, model, deadline = r.origin, r.model, r.deadline
	}
	m.mu.Unlock()
	var all []agent.Event
	var text strings.Builder
	var tools []string
	var questions []Question
	var cost float64
	sessionID := ""
	var errText string
	ended := false
	// Each text event is one complete piece of what the agent said, so a
	// question in it is whole and goes out as the piece arrives, not when
	// the turn ends: the operator can answer while the agent still works.
	ask := func(piece string) {
		for _, q := range ParseQuestions(piece) {
			questions = append(questions, q)
			m.progress("agent.question", name, q.Question, map[string]any{"turn_id": turnID, "question": q.Question, "options": q.Options, "recommended": q.Recommended})
		}
	}
	var threads []Thread
	thread := func(id string) *Thread {
		for i := range threads {
			if threads[i].ID == id {
				return &threads[i]
			}
		}
		threads = append(threads, Thread{ID: id})
		return &threads[len(threads)-1]
	}
	var steps []Step
	// Where each tool call is, so its answer (and a subagent's state) is
	// put on it: by thread ("" is the main one) and index. Pointers into
	// a slice that grows would go stale.
	type at struct {
		thread string
		i      int
	}
	calls := map[string]at{}
	stepsOf := func(id string) *[]Step {
		if id == "" {
			return &steps
		}
		return &thread(id).Steps
	}
	toolOf := func(id string) *ToolUse {
		w, ok := calls[id]
		if !ok {
			return nil
		}
		return (*stepsOf(w.thread))[w.i].Tool
	}
	for ev := range run.Events() {
		all = append(all, ev)
		if ev.SessionID != "" {
			sessionID = ev.SessionID
		}
		switch ev.Kind {
		case agent.KindText:
			ss := stepsOf(ev.Thread)
			*ss = append(*ss, Step{Kind: "text", Text: ev.Text})
			if ev.Thread != "" {
				th := thread(ev.Thread)
				if th.Text != "" {
					th.Text += "\n\n"
				}
				th.Text += ev.Text
				m.progress("agent.text", name, "", map[string]any{"turn_id": turnID, "text": ev.Text, "thread": ev.Thread})
				continue
			}
			if text.Len() > 0 {
				text.WriteString("\n\n")
			}
			text.WriteString(ev.Text)
			m.progress("agent.text", name, "", map[string]any{"turn_id": turnID, "text": ev.Text})
			ask(ev.Text)
			continue
		case agent.KindTool:
			tu := &ToolUse{ID: ev.ToolID, Name: ev.Tool, Input: clipInput(ev.Input)}
			data := map[string]any{"turn_id": turnID, "id": ev.ToolID, "tool": ev.Tool, "input": tu.Input}
			if isAgentTool(ev.Tool) && ev.ToolID != "" {
				tu.Run = RunRunning
				th := thread(ev.ToolID)
				th.Run = RunRunning
				th.Description, _ = ev.Input["description"].(string)
				if pr, ok := ev.Input["prompt"].(string); ok {
					th.Prompt = clipText(pr)
				}
				th.Background, _ = ev.Input["run_in_background"].(bool)
				data["run"] = RunRunning
			}
			ss := stepsOf(ev.Thread)
			*ss = append(*ss, Step{Kind: "tool", Tool: tu})
			if ev.ToolID != "" {
				calls[ev.ToolID] = at{ev.Thread, len(*ss) - 1}
			}
			if ev.Thread != "" {
				th := thread(ev.Thread)
				th.Tools = append(th.Tools, ev.Tool)
				data["thread"] = ev.Thread
			} else {
				tools = append(tools, ev.Tool)
			}
			m.progress("agent.tool", name, "", data)
			continue
		case agent.KindToolResult:
			tu := toolOf(ev.ToolID)
			if tu == nil {
				continue
			}
			tu.Output, tu.Error, tu.Done = clipText(ev.Output), ev.IsError, true
			data := map[string]any{"turn_id": turnID, "id": ev.ToolID, "output": tu.Output, "error": tu.Error}
			if ev.Thread != "" {
				data["thread"] = ev.Thread
			}
			if tu.Run != "" {
				// A subagent in the foreground is done when its call
				// answers; one in the background answers "launched" and
				// goes on, so only a failure ends it here.
				th := thread(ev.ToolID)
				switch {
				case ev.IsError:
					tu.Run = "failed"
				case !th.Background:
					tu.Run = "completed"
				}
				th.Run = tu.Run
				data["run"] = tu.Run
			}
			m.progress("agent.tool.result", name, "", data)
			continue
		case agent.KindLimits:
			m.opts.Usage.Record(s.Provider, "", "turn", ev.Limits)
			continue
		case agent.KindAgentStatus:
			tu := toolOf(ev.ToolID)
			if tu == nil || tu.Run == "" {
				continue // a background shell, not a subagent
			}
			tu.Run = ev.Status
			thread(ev.ToolID).Run = ev.Status
			m.progress("agent.subagent", name, "", map[string]any{"turn_id": turnID, "id": ev.ToolID, "run": ev.Status})
			continue
		}
		switch ev.Kind {
		case agent.KindCost:
			if ev.Cost > cost {
				cost = ev.Cost
			}
		case agent.KindResult:
			ended = true
			if ev.Cost > cost {
				cost = ev.Cost
			}
			if text.Len() == 0 && ev.Text != "" {
				text.WriteString(ev.Text)
				steps = append(steps, Step{Kind: "text", Text: ev.Text})
				ask(ev.Text)
			}
		case agent.KindError:
			ended = true
			errText = ev.Text
			if ev.Cost > cost {
				cost = ev.Cost
			}
		}
	}
	if !ended && errText == "" {
		errText = "the agent ended without a result"
	}
	if !deadline.IsZero() && !time.Now().Before(deadline) {
		errText = fmt.Sprintf("the turn ran for %s and was cut off; what it left running is listed under its processes", TurnLimit)
		m.emit("agent.turn_limit", name, "error", errText, map[string]any{"turn_id": turnID})
	}
	if cost <= 0 && origin == OriginSite {
		// A door turn on a subscription costs nothing that can be read,
		// and a dollar cap never trips on it: it is counted instead.
		_ = m.opts.Ledger.CountUnpriced(SiteDoorBook(domain), time.Now())
	}
	if cost > 0 {
		if origin == OriginSite {
			// The site door's own book: its daily cap is decided by
			// traffic, not by a person watching, so it is counted apart.
			_ = m.opts.Ledger.AddTurn(SiteDoorBook(domain), model, cost, time.Now())
		}
		if err := m.opts.Ledger.AddTurn(domain, model, cost, time.Now()); err != nil {
			m.emit("agent.error", name, "warn", "cost not recorded: "+err.Error(), map[string]any{"turn_id": turnID})
		}
	}
	// The audit runs on every turn, finished or not: a stopped turn may
	// already have read what it should not have.
	if v := s.Jail.Violations(agent.Transcript(all)); len(v) > 0 {
		m.emit("agent.jail_violation", name, "error", fmt.Sprintf("the agent referred to %d denied path(s)", len(v)), map[string]any{"turn_id": turnID, "paths": v})
	}

	// A subagent still "running" when its turn is over: one that ran to
	// the end without saying so finished with it; one under a turn that
	// failed or was stopped was cut.
	for _, w := range calls {
		tu := (*stepsOf(w.thread))[w.i].Tool
		if tu.Run != RunRunning {
			continue
		}
		tu.Run = "completed"
		if errText != "" {
			tu.Run = RunCut
		}
		thread(tu.ID).Run = tu.Run
	}

	c := m.load(name)
	if sessionID != "" {
		c.SessionID = sessionID
		if c.Sessions == nil {
			c.Sessions = map[string]string{}
		}
		c.Sessions[s.Provider] = sessionID
	}
	turn := Turn{ID: turnID, Role: "assistant", Text: text.String(), Cost: cost, At: time.Now(), SessionID: sessionID, Provider: s.Provider, Tools: tools, Questions: questions, Threads: threads, Steps: steps}
	if errText != "" {
		turn.Role = "error"
		if turn.Text == "" {
			turn.Text = errText
		} else {
			turn.Text += "\n\n" + errText
		}
	}
	c.Turns = append(c.Turns, turn)
	if err := m.save(c); err != nil {
		m.emit("agent.error", name, "error", "conversation not saved: "+err.Error(), map[string]any{"turn_id": turnID})
	}
	// The turn counts as running until its file is written, and the final
	// event goes out only after that: whoever reacts to agent.done can
	// Send again and read the turn from History at once.
	m.sent(name, turnID)
	_ = os.Remove(m.runningPath(name))
	_ = os.Remove(m.streamPath(name, turnID))
	_ = os.Remove(m.streamPath(name, turnID) + ".err")
	var queued []Queued
	queuedModel, queuedProvider := "", ""
	m.mu.Lock()
	if r := m.running[name]; r != nil {
		close(r.done)
		queued, queuedModel, queuedProvider = r.queued, r.queuedModel, r.queuedProvider
	}
	delete(m.running, name)
	_ = os.Remove(m.queuePath(name))
	m.mu.Unlock()
	if len(queued) > 0 {
		// After this turn's final event, so whoever follows the stream sees
		// one turn end and the next begin.
		defer m.sendQueued(name, queueFile{Messages: queued, Model: queuedModel, Provider: queuedProvider})
	}
	if errText != "" {
		m.emit("agent.error", name, "error", errText, map[string]any{"turn_id": turnID, "cost": cost, "origin": c.Origin})
		return
	}
	m.emit("agent.done", name, "", "", map[string]any{"turn_id": turnID, "cost": cost, "session_id": sessionID, "text": text.String(), "tools": len(tools), "origin": c.Origin})
}

func (m *Manager) emit(typ, domain, level, msg string, data map[string]any) {
	m.opts.Bus.Emit(events.Event{Type: typ, Subject: domain, Actor: "agent", Level: level, Message: msg, Data: data})
}

// ProviderInfo says whether a provider can run now and which models it
// knows; the site door answers with it.
func (m *Manager) ProviderInfo(key string) (bool, []string) {
	p, err := m.opts.Registry.Get(key)
	if err != nil {
		return false, nil
	}
	ok, _ := p.Available()
	return ok, p.Models()
}

// Providers are the agents the registry holds, by key.
func (m *Manager) Providers() []string { return m.opts.Registry.Keys() }

// ProviderLabels are what an agent's model names run, when they do not
// say (agent.Labeler); nil when they do.
func (m *Manager) ProviderLabels(key string) map[string]string {
	p, err := m.opts.Registry.Get(key)
	if err != nil {
		return nil
	}
	if l, ok := p.(agent.Labeler); ok {
		return l.ModelLabels()
	}
	return nil
}

// ProviderPermissions are the site door's permissions an agent can hold
// apart: what it says (agent.Permissioner), or all of them.
func (m *Manager) ProviderPermissions(key string) []string {
	p, err := m.opts.Registry.Get(key)
	if err != nil {
		return nil
	}
	if pp, ok := p.(agent.Permissioner); ok {
		return pp.Permissions()
	}
	return append([]string(nil), Permissions...)
}

// Ledger is the cost record the manager writes.
func (m *Manager) Ledger() *agent.Ledger { return m.opts.Ledger }

// SetPath replaces the PATH source a turn runs with.
func (m *Manager) SetPath(f func() string) {
	m.mu.Lock()
	m.opts.Path = f
	m.mu.Unlock()
}

// SetTurnEnv sets Options.TurnEnv after the manager exists: the wiring
// has the site tokens and the modules only later.
func (m *Manager) SetTurnEnv(f func(domain string) map[string]string) {
	m.mu.Lock()
	m.opts.TurnEnv = f
	m.mu.Unlock()
}

// agentPath is the PATH the agent runs with: the supervisor's, plus the
// folder this binary lives in when it is not there yet, so `middleware
// site add` works from a conversation whether the binary was installed
// under /usr/local/bin or is being run from a build folder.
func (m *Manager) agentPath() string {
	path := os.Getenv("PATH")
	if m.opts.Path != nil {
		if p := m.opts.Path(); p != "" {
			path = p
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return path
	}
	dir := filepath.Dir(exe)
	for _, p := range filepath.SplitList(path) {
		if p == dir {
			return path
		}
	}
	if path == "" {
		return dir
	}
	return path + string(os.PathListSeparator) + dir
}

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// Preamble tells the agent where it is and what it may not do. It is
// passed as system context so the user's own words stay the prompt.
func Preamble(domain, workdir string, j agent.Jail) string {
	var b strings.Builder
	if domain == Middleware {
		b.WriteString("You are working on the middleware installation itself, the supervisor that serves every site on this machine. ")
		b.WriteString("Your working directory is the installation root: " + workdir + ". ")
		b.WriteString("You may read and edit the installation's own files, its settings and its tasks. ")
		b.WriteString("You may create new sites, in either of two ways: make the folder sites/<domain>/ with the project inside and a site.json (the engine picks it up within a few seconds), ")
		b.WriteString("or run `middleware site add <domain> --template <static|node|go|docker>` or `middleware site add <domain> --manifest -` with the manifest on stdin; the command talks to the supervisor over its unix socket and needs no credentials. ")
		b.WriteString("Once a site exists, hand detailed work on it to that site's own conversation unless you are asked to continue yourself. ")
		b.WriteString("You must not edit a site that already existed when this turn began: each of those has its own conversation, and the paths listed below are denied.\n")
	} else {
		b.WriteString("You are working on the site " + domain + ", served by the middleware on this machine. ")
		b.WriteString("Your working directory is the site's folder: " + workdir + ". The whole project lives there, including site.json, its manifest. ")
		b.WriteString("You may read and edit anything inside that folder. ")
		// The one exception is named: an agent that does not load skills
		// is told to read the guide in its HOME, and Codex, given only the
		// rule, refused to (Claude Code never asked, it loads it itself).
		b.WriteString("You must not read, write or run anything outside it: other sites, the middleware's data, its tasks or its root are off limits, and every tool call is audited for that. ")
		// The guide by its full path, not ~: told "~/.claude/…", an agent
		// on a V2 node went for the root's .claude, which is denied, and
		// concluded the guide was off limits.
		guide := "~/.claude/skills/middleware/SKILL.md"
		if j.Home != "" {
			guide = filepath.Join(j.Home, ".claude", "skills", SkillName, "SKILL.md")
		}
		b.WriteString("The exceptions are two. The house's guide, " + guide + ", which you may always read. ")
		if logs := agent.OwnLogs(filepath.Dir(filepath.Dir(workdir)), domain); logs != "" {
			// Said as it is for this turn: told only "with the Read tool",
			// an agent chained an ls of the folder into a longer command,
			// which was refused whole, and lost a step to it.
			how := "Read them with your file-reading tool, never with a shell command: a shell command that names that folder is refused whole, even chained with others, so do not put it in one (no ls, cat, tail or grep of it). "
			if agent.Enclose != nil {
				how = "The folder is mounted read-only in your jail, so you may read it with any tool, shell commands included. "
			}
			b.WriteString("And your own logs, in " + logs + ", which the middleware writes and you may read, not write. " + how + "site.log is the site's process, worker-<name>.log each worker's, move-<name>.log a move's commands, access.log the requests that reached it.\n")
		} else {
			b.WriteString("Your own logs, once the site has run, are in data/logs/" + domain + " under the installation's root, and you may read them.\n")
		}
		b.WriteString("When the manifest names deploy.github.repository and the GitHub module has a token, `git push` to that repository works as it is: the house lends the credential through its own address, without showing it, and refuses any other repository and any deletion. Do not ask for a token or write one to a file.\n")
	}
	b.WriteString("Do not start long-running processes: the middleware supervises the site's process from its manifest, and a change to the manifest or to a pointer file is how a deployment happens.\n")
	b.WriteString("When this turn ends, its whole process group is killed. Anything that takes longer than a turn (a build, an install, a deployment) must be started detached, exactly like this: `setsid nohup <cmd> >/tmp/<name>.log 2>&1 </dev/null &`, and your reply must state the log's path so the next message can check how it went.\n")
	b.WriteString("When you need the operator to decide something, ask with a fenced block the panel turns into buttons; two to four short options, the one you would pick as recommended, or an empty options list for a free-text answer:\n")
	b.WriteString("```question\n{\"question\": \"Which database?\", \"options\": [\"SQLite\", \"Postgres\"], \"recommended\": \"SQLite\"}\n```\n")
	b.WriteString("Ask one thing at a time, then end your turn; the answer arrives as the next message.\n")
	if names := siteServices(workdir); len(names) > 0 {
		b.WriteString("This site runs as services (" + strings.Join(names, ", ") + "), each with its own process or container, names and log. Run a command in one with `middleware exec " + domain + " --service <name> -- <command>` (an install, a migration: it is on the site's network, so it reaches the others by name); `middleware site show " + domain + "` and `middleware site routes " + domain + "` say how each is doing and which name goes where, `middleware site logs " + domain + " --service <name>` (with --build for its image's build) is its output, and `middleware site validate " + domain + " --file <draft>` checks a manifest before you write it.\n")
	}
	if image := dockerImage(workdir); image != "" {
		b.WriteString("This site runs inside a container (" + image + "). Install its dependencies and run its tools where it runs: `middleware exec " + domain + " -- npm install` (or pip, a build, a script). Run on this machine instead, native modules are built for the wrong system and the site fails at start. Reading and editing files is direct: it is the same folder.\n")
	}
	if len(j.Commands) > 0 {
		b.WriteString("Commands you must not run: " + strings.Join(j.Commands, ", ") + ".\n")
	}
	if len(j.Deny) > 0 {
		b.WriteString("Paths you must not touch:\n")
		for _, d := range j.Deny {
			b.WriteString("  - " + d + "\n")
		}
	}
	return b.String()
}

// dockerImage is the image a site runs in, or "" when it runs as a process.
// siteServices are the services of a site that has them, sorted.
func siteServices(siteDir string) []string {
	m, err := manifest.Load(siteDir)
	if err != nil {
		return nil
	}
	return m.ServiceNames()
}

func dockerImage(siteDir string) string {
	m, err := manifest.Load(siteDir)
	if err != nil || m.Run == nil || m.Run.Runtime != "docker" || m.Run.Docker == nil {
		return ""
	}
	if m.Run.Docker.Image != "" {
		return m.Run.Docker.Image
	}
	return "built from " + m.Run.Docker.Build
}

// lentTo reads chat.reads straight from site.json, like the agent block:
// a manifest the engine refuses (a typo elsewhere) must still let the
// conversation open, since the conversation is how the typo gets fixed.
func lentTo(siteDir string) []string {
	data, err := os.ReadFile(filepath.Join(siteDir, manifest.FileName))
	if err != nil {
		return nil
	}
	var raw struct {
		Chat struct {
			Reads []string `json:"reads"`
		} `json:"chat"`
	}
	if json.Unmarshal(data, &raw) != nil {
		return nil
	}
	return raw.Chat.Reads
}

// runningFile is data/chat/running/<name>.json: a turn in progress, so a
// supervisor that restarts mid-turn adopts it instead of losing it.
type runningFile struct {
	Name     string    `json:"name"`
	TurnID   string    `json:"turn_id"`
	PID      int       `json:"pid"`
	Stream   string    `json:"stream"`
	Provider string    `json:"provider"`
	Model    string    `json:"model,omitempty"`
	Origin   string    `json:"origin,omitempty"`
	Started  time.Time `json:"started"`
}

func (m *Manager) runningPath(name string) string {
	return filepath.Join(m.opts.Root, "data", "chat", "running", name+".json")
}

func (m *Manager) queuePath(name string) string {
	return filepath.Join(m.opts.Root, "data", "chat", "queued", name+".json")
}

// sendQueued sends what waited as one message, each piece in the order it
// was written.
func (m *Manager) sendQueued(name string, q queueFile) {
	texts := make([]string, 0, len(q.Messages))
	for _, x := range q.Messages {
		texts = append(texts, x.Text)
	}
	if _, err := m.SendWith(name, strings.Join(texts, "\n\n"), SendOptions{Model: q.Model, Provider: q.Provider}); err != nil {
		m.emit("agent.error", name, "error", "the queued message was not sent: "+err.Error(), nil)
	}
}

func (m *Manager) streamPath(name, turnID string) string {
	return filepath.Join(m.opts.Root, "data", "chat", "streams", name+"-"+turnID+".out")
}

// Recover adopts the turns a previous supervisor left: still running, it
// is followed to its end; finished while nobody listened, its output is
// read to its result all the same. Before, a restart lost every turn in
// flight: the CLI died on its next write to a pipe nobody read, and the
// conversation kept a question with no answer and no word about why.
func (m *Manager) Recover(ctx context.Context) {
	ents, _ := os.ReadDir(filepath.Join(m.opts.Root, "data", "chat", "running"))
	for _, e := range ents {
		var rf runningFile
		path := filepath.Join(m.opts.Root, "data", "chat", "running", e.Name())
		if fsx.ReadJSON(path, &rf) != nil || rf.Name == "" {
			_ = os.Remove(path)
			continue
		}
		s, err := m.Resolve(rf.Name)
		provider, perr := m.opts.Registry.Get(rf.Provider)
		adopter, ok := provider.(agent.Adopter)
		if err != nil || perr != nil || !ok {
			_ = os.Remove(path)
			m.emit("agent.error", rf.Name, "error", "a turn in flight when the supervisor restarted could not be picked up", map[string]any{"turn_id": rf.TurnID})
			continue
		}
		s.Model = rf.Model
		cctx, cancel, deadline := withLimit(rf.Started)
		run := adopter.Adopt(cctx, rf.PID, rf.Stream)
		var q queueFile
		_ = fsx.ReadJSON(m.queuePath(rf.Name), &q)
		m.mu.Lock()
		m.running[rf.Name] = &running{turnID: rf.TurnID, provider: rf.Provider, origin: rf.Origin, model: rf.Model, started: rf.Started, deadline: deadline, cancel: cancel, done: make(chan struct{}), run: run, queued: q.Messages, queuedModel: q.Model, queuedProvider: q.Provider}
		m.mu.Unlock()
		m.emit("agent.adopted", rf.Name, "", "a turn from before the restart, picked up", map[string]any{"turn_id": rf.TurnID, "pid": rf.PID})
		go m.follow(rf.Name, rf.TurnID, s, run, cancel)
	}
	// What waited for a turn that is not coming back (it could not be
	// picked up, or it ended as the old supervisor went) is sent now: it
	// was written to be sent, and nothing else would.
	qdir := filepath.Join(m.opts.Root, "data", "chat", "queued")
	qents, _ := os.ReadDir(qdir)
	for _, e := range qents {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		m.mu.Lock()
		_, busy := m.running[name]
		m.mu.Unlock()
		if busy {
			continue
		}
		var q queueFile
		path := filepath.Join(qdir, e.Name())
		err := fsx.ReadJSON(path, &q)
		_ = os.Remove(path)
		if err == nil && len(q.Messages) > 0 {
			m.sendQueued(name, q)
		}
	}
}
