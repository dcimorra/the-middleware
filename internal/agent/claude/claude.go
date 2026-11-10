// Package claude drives the `claude` CLI (Claude Code) as a subprocess and
// turns its stream-json output into agent events. The command line is
// data, not code: a renamed flag is a Config change, because the CLI has
// renamed flags before and a release of this binary must not be needed to
// follow it.
package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/agent"
	"github.com/dcimorra/the-middleware/internal/fsx"
)

// Config is the command template.
type Config struct {
	// Key is the provider's name in the chat: "claude", or a vendor whose
	// models Claude Code drives through an Anthropic-compatible API
	// ("deepseek", "zai").
	Key string
	// Env is that vendor's: ANTHROPIC_BASE_URL, ANTHROPIC_AUTH_TOKEN and
	// the model names. With it, the subscription and the API key of the
	// Claude module are never passed: a turn for DeepSeek must not be able
	// to spend on Anthropic.
	Env map[string]string
	// Bin is the executable; "claude" is looked up in PATH.
	Bin string
	// ExtraArgs are appended to every invocation, after the fixed ones.
	ExtraArgs []string
	// APIKey is passed as ANTHROPIC_API_KEY when set. Empty relies on the
	// CLI's own login under the run's HOME, or on ConfigDir.
	APIKey string
	// ConfigDir, when set, is passed as CLAUDE_CONFIG_DIR so every site
	// shares one login instead of needing one per home.
	ConfigDir string
	// OAuthToken is the long-lived token `claude setup-token` gives for a
	// subscription, passed as CLAUDE_CODE_OAUTH_TOKEN. The API key wins
	// when both are set, which is the CLI's rule, not ours.
	OAuthToken string
	// Models offered to the panel; the first is the default. Aliases are
	// used by default because the CLI resolves them to the current
	// release, which a full model name would pin.
	Models []string
	// PermissionMode is the CLI's --permission-mode; acceptEdits lets the
	// agent edit inside the site without a prompt nobody is there to answer.
	PermissionMode string
}

// Provider implements agent.Provider.
type Provider struct {
	cfg Config
}

// New returns a provider with defaults filled in.
func New(cfg Config) *Provider {
	if cfg.Bin == "" {
		cfg.Bin = "claude"
	}
	if cfg.Key == "" {
		cfg.Key = "claude"
	}
	if len(cfg.Models) == 0 {
		cfg.Models = append([]string(nil), DefaultModels...)
	}
	if cfg.PermissionMode == "" {
		// A turn has nobody to approve a prompt: under acceptEdits every
		// Bash call of a headless run is refused, so the agent could not
		// as much as run npm. Permissions are skipped and the jail's deny
		// rules (which win in every mode) are the wall, as in V1.
		cfg.PermissionMode = "bypassPermissions"
	}
	return &Provider{cfg: cfg}
}

// Key implements agent.Provider.
func (p *Provider) Key() string { return p.cfg.Key }

// Available implements agent.Provider.
func (p *Provider) Available() (bool, string) {
	path, err := exec.LookPath(p.cfg.Bin)
	if err != nil {
		return false, fmt.Sprintf("%s not found in PATH", p.cfg.Bin)
	}
	return true, path
}

// Models implements agent.Provider.
func (p *Provider) Models() []string { return append([]string(nil), p.cfg.Models...) }

// DefaultModels are the aliases offered when the module names none: every
// family the CLI has an alias for. Fable was missing until alpha.135 and
// could not be picked anywhere (module model, user limits, a site's door),
// although the CLI ran it; a new family goes here and in Aliases.
var DefaultModels = []string{"sonnet", "opus", "haiku", "fable"}

// Aliases are the model each of Claude Code's aliases runs today: the CLI
// resolves them to the newest of each family, which is why they are what
// is offered, but "sonnet" on a screen does not say which Sonnet. Kept
// here by hand: when a new model comes out the alias moves on its own and
// only this label is behind, which is the cheap way round.
var Aliases = map[string]string{
	"opus":   "claude-opus-5-5",
	"sonnet": "claude-sonnet-5-5",
	"haiku":  "claude-haiku-5-5",
	"fable":  "claude-fable-5-2",
}

// ModelLabels implements agent.Labeler: the model behind each alias, for
// Claude itself (a vendor run through Claude Code names its own models).
func (p *Provider) ModelLabels() map[string]string {
	if p.cfg.Key != "claude" {
		return nil
	}
	out := map[string]string{}
	for k, v := range Aliases {
		out[k] = v
	}
	return out
}

// Args builds the command line for req. It is exported so a test, or an
// operator wondering what will run, can see it without running anything.
func (p *Provider) Args(req agent.Request, settingsFile string) []string {
	args := []string{"-p", req.Prompt, "--output-format", "stream-json", "--verbose", "--permission-mode", p.cfg.PermissionMode}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.SessionID != "" {
		args = append(args, "--resume", req.SessionID)
	}
	if req.Partial {
		// stream_event lines with the API's own deltas, besides the whole
		// messages, which still come (the parser keeps both).
		args = append(args, "--include-partial-messages")
	}
	if req.System != "" {
		args = append(args, "--append-system-prompt", req.System)
	}
	if req.Jail.SiteDir != "" {
		args = append(args, "--add-dir", req.Jail.SiteDir)
	}
	for _, d := range req.Jail.Lent {
		args = append(args, "--add-dir", d)
	}
	if settingsFile != "" {
		args = append(args, "--settings", settingsFile)
	}
	// Only the user's settings, which are the house's own folder
	// (CLAUDE_CONFIG_DIR), and the file above. The project's
	// .claude/settings.json and settings.local.json are the site's to
	// write, and Claude Code runs commands from them without asking in
	// print mode: tried on 2.1.312, a project's apiKeyHelper ran on the
	// host even with hooks off, and its .mcp.json servers started. No MCP
	// server at all either, from any file: the house configures none.
	args = append(args, "--setting-sources", "user", "--strict-mcp-config")
	if req.Tools != nil {
		args = append(args, "--tools", strings.Join(req.Tools, ","))
	}
	if len(req.Disallowed) > 0 {
		args = append(args, "--disallowed-tools", strings.Join(req.Disallowed, ","))
	}
	return append(args, p.cfg.ExtraArgs...)
}

// Settings is the settings file the jail becomes: a deny rule per tool per
// denied path, and one per denied command. Deny wins over allow and over
// the permission mode in Claude Code, which is why the jail never lists the
// root itself (see agent.NewJail).
//
// An absolute path is written with two slashes: in Claude Code's rules
// "/x" is relative to the settings file and only "//x" is the root's /x.
// V2's first draft wrote one slash, so every path rule matched nothing and
// only the audit after the turn stood between a site and its neighbours.
// Bash is matched on the command text, so a path spelled differently
// slips through here and is caught by the audit instead.
func Settings(j agent.Jail) map[string]any {
	var deny []string
	for _, d := range j.Deny {
		for _, tool := range []string{"Read", "Edit", "Write", "MultiEdit", "Glob", "Grep", "NotebookEdit"} {
			deny = append(deny, fmt.Sprintf("%s(/%s/**)", tool, d))
			deny = append(deny, fmt.Sprintf("%s(/%s)", tool, d))
		}
		deny = append(deny, fmt.Sprintf("Bash(*%s*)", d))
	}
	for _, c := range j.Commands {
		deny = append(deny, fmt.Sprintf("Bash(%s:*)", c))
	}
	for _, d := range j.Lent {
		for _, tool := range []string{"Edit", "Write", "MultiEdit", "NotebookEdit"} {
			deny = append(deny, fmt.Sprintf("%s(/%s/**)", tool, d))
		}
		// Bash is matched on its text and could write as easily as read;
		// the Read tool is how a lent folder is read. In a jail the folder
		// is mounted read-only and a command cannot write it, so the rule
		// only refused an ls chained into a longer command (chat.Preamble
		// says which of the two holds).
		if agent.Enclose == nil {
			deny = append(deny, fmt.Sprintf("Bash(*%s*)", d))
		}
	}
	for _, s := range j.Secrets {
		for _, tool := range []string{"Read", "Grep", "Glob"} {
			deny = append(deny, fmt.Sprintf("%s(/%s)", tool, s))
		}
	}
	// No hook runs, from any settings file: a hook is a command Claude
	// Code runs on the host by itself, and the files that can hold one
	// were the site's to write (its HOME is mounted into its container,
	// its folder comes from its repository). The --settings file outranks
	// every file but the machine's managed policy, so a project that sets
	// it back to false does not win.
	return map[string]any{"disableAllHooks": true, "permissions": map[string]any{"deny": deny}}
}

// Start implements agent.Provider.
func (p *Provider) Start(ctx context.Context, req agent.Request) (agent.Run, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("claude: empty prompt")
	}
	if req.Jail.SiteDir == "" {
		req.Jail.SiteDir = req.Workdir
	}
	if err := req.Jail.Prepare(); err != nil {
		return nil, err
	}
	// The CLI's own folder is the house's (agent.Jail.ConfigHome), not the
	// site's HOME. The sessions it resumes come with it once, and so does
	// a login made under that HOME, except for a vendor's turn, which must
	// not be able to spend on Anthropic.
	var carry []string
	from := ""
	if req.Jail.Home != "" {
		from = filepath.Join(req.Jail.Home, ".claude")
		carry = []string{"projects"}
		if len(p.cfg.Env) == 0 {
			carry = append(carry, ".credentials.json")
		}
	}
	config, err := req.Jail.ConfigHome(p.cfg.Key, from, carry...)
	if err != nil {
		return nil, err
	}
	// The settings file goes there too: written in HOME, the container
	// could swap it between the house writing it and the CLI reading it.
	dir := config
	if dir == "" {
		dir = req.Jail.Home
		if dir == "" {
			dir = req.Workdir
		}
		dir = filepath.Join(dir, ".middleware")
	}
	// One per conversation: two conversations of one site with different
	// tool levels used to rewrite each other's file without a single
	// error anywhere (V1). A '#' is fine in a file name. Never
	// settings.json, which in the config folder is the user's settings.
	file := "claude-settings.json"
	if req.Name != "" && strings.Contains(req.Name, "#") {
		file = "claude-settings-" + strings.ReplaceAll(req.Name, "/", "_") + ".json"
	}
	settingsFile := filepath.Join(dir, file)
	if err := fsx.WriteJSON(settingsFile, Settings(req.Jail), 0o600); err != nil {
		return nil, err
	}
	env := map[string]string{}
	for k, v := range req.Env {
		env[k] = v
	}
	if len(p.cfg.Env) > 0 {
		for k, v := range p.cfg.Env {
			env[k] = v
		}
	} else if p.cfg.APIKey != "" {
		env["ANTHROPIC_API_KEY"] = p.cfg.APIKey
	}
	// In print mode the CLI ends a background subagent after ten minutes
	// unless told not to wait with a ceiling; V1 lost long agents that way.
	env["CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS"] = "0"
	if p.cfg.ConfigDir != "" && len(p.cfg.Env) == 0 {
		env["CLAUDE_CONFIG_DIR"] = p.cfg.ConfigDir
	} else if config != "" {
		env["CLAUDE_CONFIG_DIR"] = config
	}
	// --setting-sources user drops the project's CLAUDE.md with the
	// project's settings (tried on 2.1.312); this has it read again from
	// the --add-dir folders, the site's own first, and none of their
	// settings with it.
	env["CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD"] = "1"
	if p.cfg.OAuthToken != "" && p.cfg.APIKey == "" && len(p.cfg.Env) == 0 {
		env["CLAUDE_CODE_OAUTH_TOKEN"] = p.cfg.OAuthToken
	}
	return req.Jail.Start(ctx, &parser{}, req.Stream, req.Workdir, req.Jail.Env(env), p.cfg.Bin, p.Args(req, settingsFile)...)
}

// Adopt implements agent.Adopter.
func (p *Provider) Adopt(ctx context.Context, pid int, stream string) agent.Run {
	return agent.AdoptCLI(ctx, &parser{}, pid, stream)
}

// parser reads stream-json: one JSON object per line with a "type" of
// system (init, carries session_id; task_started and task_notification,
// a background task's start and end), assistant (a message whose content
// blocks are text or tool_use), user (tool results, matched to their call
// by id) and result (the end of a turn, with total_cost_usd). Anything
// that is not JSON is passed on as text so a CLI that prints a warning
// before the stream is still read.
//
// A result is not always the end of the run. In print mode the CLI
// writes one when the agent stops talking, then waits for what it left
// running in the background (CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS=0) and,
// when a monitor fires or a subagent finishes, gives the agent another
// turn and writes another result. Taking the first one as the end killed
// the CLI three seconds later with its monitor: the agent said "I'll tell
// you when it finishes" and nobody was left to. So a result while a
// monitor or a subagent is still out is held, and the run goes on.
type parser struct {
	sessionID string
	// tools names each tool call by its id, to know what a task_started
	// belongs to.
	tools map[string]string
	// waiting are the background tasks the run waits for, by task id.
	waiting map[string]bool
	// held is a result written while something was waiting: what Flush
	// gives if the CLI exits without writing another.
	held []agent.Event
	// context is the main agent's last call's tokens (usage.total).
	context int
}

// waitedFor are the tools whose background tasks keep a run open: a
// monitor, whose events are the reason it was started, and subagents.
// Not a background Bash call: a dev server started with it never ends,
// and would hold the turn for ever.
var waitedFor = map[string]bool{"Monitor": true, "Task": true, "Agent": true, "Workflow": true}

type line struct {
	Type      string  `json:"type"`
	Subtype   string  `json:"subtype"`
	SessionID string  `json:"session_id"`
	IsError   bool    `json:"is_error"`
	Result    string  `json:"result"`
	Cost      float64 `json:"total_cost_usd"`
	// Error is where Qwen Code, which prints this same format, puts a
	// failed turn's reason; Claude Code puts it in Result. Raw, like
	// Event: a line whose field has another shape than expected must
	// still decode (see Event).
	Error json.RawMessage `json:"error"`
	// ParentToolUseID marks a subagent's messages: the Task call that
	// started it.
	ParentToolUseID string `json:"parent_tool_use_id"`
	// A task_started's or task_notification's task, call and state.
	TaskID    string `json:"task_id"`
	TaskType  string `json:"task_type"`
	ToolUseID string `json:"tool_use_id"`
	Status    string `json:"status"`
	Message   struct {
		Content json.RawMessage `json:"content"`
		Usage   *usage          `json:"usage"`
	} `json:"message"`
	// ModelUsage is the result's per-model account; its contextWindow is
	// the window's size.
	ModelUsage map[string]struct {
		ContextWindow int `json:"contextWindow"`
	} `json:"modelUsage"`
	// Event is a stream_event's: the API's streaming event as it came
	// (--include-partial-messages). Kept raw and read only on a
	// stream_event: newer CLIs send system lines whose "event" is a
	// string ({"type":"system","subtype":"ui_invalidate","event":
	// "ui.render"}), and with a struct here the whole line failed to
	// decode and was shown in the chat as the agent's words.
	Event json.RawMessage `json:"event"`
}

// streamEvent is what a stream_event's Event holds.
type streamEvent struct {
	Type  string `json:"type"`
	Delta struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"delta"`
}

// errorText reads a failed turn's error: an object with a message, or a
// plain string.
func errorText(raw json.RawMessage) string {
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &e) == nil {
		return e.Message
	}
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

// usage is one model call's tokens. What the call read (fresh, written to
// the cache and read from it) plus what it wrote is what the session
// holds after it: the context in use.
type usage struct {
	Input         int `json:"input_tokens"`
	CacheCreation int `json:"cache_creation_input_tokens"`
	CacheRead     int `json:"cache_read_input_tokens"`
	Output        int `json:"output_tokens"`
}

func (u usage) total() int { return u.Input + u.CacheCreation + u.CacheRead + u.Output }

type block struct {
	Type  string         `json:"type"`
	Text  string         `json:"text"`
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
	// A tool_result, in a "user" line.
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// textOf reads a tool result's content: a string, or text blocks.
func textOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return string(raw)
}

func (p *parser) Line(raw string) ([]agent.Event, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, false
	}
	var l line
	if !strings.HasPrefix(raw, "{") || !json.Valid([]byte(raw)) {
		// Not the stream: what the CLI printed plainly ("Not logged
		// in", a crash) is worth reading.
		return []agent.Event{{Kind: agent.KindText, Text: raw}}, false
	}
	if json.Unmarshal([]byte(raw), &l) != nil {
		// A JSON line is the CLI talking to its own interface, never the
		// agent's words: one this parser cannot read is dropped, not
		// shown. Its session still counts.
		var head struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal([]byte(raw), &head) == nil && head.SessionID != "" {
			p.sessionID = head.SessionID
		}
		return nil, false
	}
	if l.SessionID != "" {
		p.sessionID = l.SessionID
	}
	switch l.Type {
	case "assistant":
		if l.ParentToolUseID == "" && l.Message.Usage != nil {
			// The main agent's last call is the session's size; a
			// subagent's calls are its own context, not this one.
			p.context = l.Message.Usage.total()
		}
		var blocks []block
		if err := json.Unmarshal(l.Message.Content, &blocks); err != nil {
			var s string
			if json.Unmarshal(l.Message.Content, &s) == nil && s != "" {
				return []agent.Event{{Kind: agent.KindText, Text: s, SessionID: p.sessionID}}, false
			}
			return nil, false
		}
		var evs []agent.Event
		for _, b := range blocks {
			switch b.Type {
			case "text":
				if b.Text != "" {
					evs = append(evs, agent.Event{Kind: agent.KindText, Text: b.Text, SessionID: p.sessionID, Thread: l.ParentToolUseID})
				}
			case "tool_use":
				if b.ID != "" {
					if p.tools == nil {
						p.tools = map[string]string{}
					}
					p.tools[b.ID] = b.Name
				}
				evs = append(evs, agent.Event{Kind: agent.KindTool, Tool: b.Name, ToolID: b.ID, Input: b.Input, SessionID: p.sessionID, Thread: l.ParentToolUseID})
			}
		}
		return evs, false
	case "user":
		// What comes back to the agent: tool results, matched by id.
		var blocks []block
		if json.Unmarshal(l.Message.Content, &blocks) != nil {
			return nil, false
		}
		var evs []agent.Event
		for _, b := range blocks {
			if b.Type == "tool_result" && b.ToolUseID != "" {
				evs = append(evs, agent.Event{Kind: agent.KindToolResult, ToolID: b.ToolUseID, Output: textOf(b.Content), IsError: b.IsError, SessionID: p.sessionID, Thread: l.ParentToolUseID})
			}
		}
		return evs, false
	case "system":
		if l.Subtype == "task_started" && l.TaskID != "" {
			if waitedFor[p.tools[l.ToolUseID]] || l.TaskType == "local_agent" || l.TaskType == "local_workflow" || l.TaskType == "remote_agent" {
				if p.waiting == nil {
					p.waiting = map[string]bool{}
				}
				p.waiting[l.TaskID] = true
			}
			return nil, false
		}
		if l.Subtype == "task_notification" {
			// The CLI writes it once per task, when the task is over.
			delete(p.waiting, l.TaskID)
		}
		// A subagent that ends (or fails, or is stopped) says so here,
		// with the id of the call that launched it. Background shells send
		// it too; the chat keeps only the ones that name a subagent.
		if l.Subtype == "task_notification" && l.ToolUseID != "" && l.Status != "" {
			return []agent.Event{{Kind: agent.KindAgentStatus, ToolID: l.ToolUseID, Status: l.Status, SessionID: p.sessionID}}, false
		}
		return nil, false
	case "stream_event":
		// Only the main agent's text: a subagent's is its own, and what
		// the model thinks or the input of a tool it calls is not the
		// answer being written.
		var e streamEvent
		if l.ParentToolUseID == "" && json.Unmarshal(l.Event, &e) == nil && e.Type == "content_block_delta" && e.Delta.Type == "text_delta" && e.Delta.Text != "" {
			return []agent.Event{{Kind: agent.KindDelta, Text: e.Delta.Text, SessionID: p.sessionID}}, false
		}
		return nil, false
	case "rate_limit_event":
		if ws := limitsOf(raw); len(ws) > 0 {
			return []agent.Event{{Kind: agent.KindLimits, Limits: ws, SessionID: p.sessionID}}, false
		}
		return nil, false
	case "result":
		evs := []agent.Event{{Kind: agent.KindCost, Cost: l.Cost, SessionID: p.sessionID}}
		if l.IsError || (l.Subtype != "" && l.Subtype != "success") {
			text := l.Result
			if text == "" {
				text = errorText(l.Error)
			}
			if text == "" {
				text = l.Subtype
			}
			evs = append(evs, agent.Event{Kind: agent.KindError, Text: text, Cost: l.Cost, SessionID: p.sessionID})
		} else {
			ev := agent.Event{Kind: agent.KindResult, Text: l.Result, Cost: l.Cost, SessionID: p.sessionID, Context: p.context}
			for _, mu := range l.ModelUsage {
				ev.Window = max(ev.Window, mu.ContextWindow)
			}
			evs = append(evs, ev)
		}
		if len(p.waiting) > 0 {
			// The cost so far goes out (it is the run's total, so the
			// last one counts); the end waits for the next result.
			p.held = evs[1:]
			return evs[:1], false
		}
		p.held = nil
		return evs, true
	}
	return nil, false
}

// Flush gives the result held for a task that never said it ended: the
// CLI exiting is the end too, and the run must not read as one that died
// without a result.
func (p *parser) Flush() []agent.Event {
	evs := p.held
	p.held = nil
	return evs
}

// claudeWindows are Claude Code's rateLimitType names for the house's.
var claudeWindows = map[string]string{"five_hour": "5h", "seven_day": "7d", "seven_day_opus": "7d_opus", "seven_day_sonnet": "7d_sonnet", "seven_day_overage_included": "7d_extra"}

// limitsOf reads a rate_limit_event: the subscription's windows, which
// Claude Code passes on from the API's anthropic-ratelimit-unified-*
// headers. The event names the window that binds now (rateLimitType), and
// unifiedWindows, when it comes, every window with its figure: without it
// a week that binds hid the five hours. utilization is a fraction of 1, as
// in the headers, and the top-level one comes only when the API sent it
// (past a warning, mostly); the window's state and reset are worth
// keeping without it.
func limitsOf(raw string) []agent.Window {
	type figure struct {
		Utilization *float64 `json:"utilization"`
		ResetsAt    float64  `json:"resetsAt"`
	}
	var e struct {
		Info struct {
			Status        string            `json:"status"`
			ResetsAt      float64           `json:"resetsAt"`
			RateLimitType string            `json:"rateLimitType"`
			Utilization   *float64          `json:"utilization"`
			Windows       map[string]figure `json:"unifiedWindows"`
		} `json:"rate_limit_info"`
	}
	if json.Unmarshal([]byte(raw), &e) != nil {
		return nil
	}
	window := func(name string, f figure) agent.Window {
		w := agent.Window{Name: name, Used: -1}
		if f.Utilization != nil {
			w.Used = *f.Utilization * 100
		}
		if f.ResetsAt > 0 {
			w.ResetsAt = time.Unix(int64(f.ResetsAt), 0).UTC()
		}
		return w
	}
	byName := map[string]agent.Window{}
	for key, f := range e.Info.Windows {
		if name, ok := claudeWindows[key]; ok {
			byName[name] = window(name, f)
		}
	}
	if name, ok := claudeWindows[e.Info.RateLimitType]; ok {
		w := window(name, figure{Utilization: e.Info.Utilization, ResetsAt: e.Info.ResetsAt})
		if old, ok := byName[name]; ok && w.Used < 0 {
			w.Used = old.Used
		}
		w.Status = e.Info.Status
		byName[name] = w
	}
	var out []agent.Window
	for _, name := range []string{"5h", "7d", "7d_opus", "7d_sonnet", "7d_extra"} {
		if w, ok := byName[name]; ok {
			out = append(out, w)
		}
	}
	return out
}

// NewParser is the stream-json reader, for CLIs that print Claude Code's
// format (Qwen Code does).
func NewParser() agent.Parser { return &parser{} }
