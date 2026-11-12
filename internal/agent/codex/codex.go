// Package codex drives OpenAI's Codex CLI as a subprocess: `codex exec
// --json`, which prints one event per line (thread.started, item.started,
// item.completed, turn.completed, turn.failed, error) and resumes a
// thread with `codex exec resume <id>`.
//
// The jail is the house's, as with every agent, plus Codex's own sandbox:
// a turn that may write runs in workspace-write (writes only in the site's
// folder and /tmp, network allowed, because installing a dependency is
// part of the work); one that may not runs read-only. Codex cannot be told
// "do not read that folder", so reading another site is caught by the
// audit after the turn, as with Qwen Code.
package codex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dcimorra/the-middleware/internal/agent"
	"github.com/dcimorra/the-middleware/internal/fsx"
)

// Config is the command template.
type Config struct {
	Bin    string
	APIKey string
	// Login is the auth.json a ChatGPT sign-in produced, for a turn with no
	// API key; Home is the one CODEX_HOME every such turn shares. Codex
	// refreshes the session and rewrites auth.json itself, so copies per
	// site would drift apart and the old ones stop working: there is one,
	// seeded from Login only when Login changes (a new sign-in).
	Login  string
	Home   string
	Models []string
}

// Provider implements agent.Provider.
type Provider struct{ cfg Config }

// New returns a provider with defaults filled in.
func New(cfg Config) *Provider {
	if cfg.Bin == "" {
		cfg.Bin = "codex"
	}
	return &Provider{cfg: cfg}
}

// Key implements agent.Provider: Codex is OpenAI's agent, so it is the
// "openai" agent of the chat.
func (p *Provider) Key() string { return "openai" }

// Available implements agent.Provider.
func (p *Provider) Available() (bool, string) {
	path, err := exec.LookPath(p.cfg.Bin)
	if err != nil {
		return false, fmt.Sprintf("%s not found in PATH", p.cfg.Bin)
	}
	if p.cfg.APIKey == "" && p.cfg.Login == "" {
		return false, "no API key and no ChatGPT sign-in: add one in Modules"
	}
	return true, path
}

// Models implements agent.Provider.
func (p *Provider) Models() []string { return append([]string(nil), p.cfg.Models...) }

// writes says whether a request lets the agent change files.
func writes(req agent.Request) bool {
	return has(req, "Write", "Edit", "MultiEdit", "Bash", "NotebookEdit")
}

// has says the request lets the agent use any of tools (Claude Code's
// names, as the house says them); nil Tools is every tool.
func has(req agent.Request, tools ...string) bool {
	if req.Tools == nil {
		return true
	}
	for _, t := range req.Tools {
		for _, w := range tools {
			if t == w {
				return true
			}
		}
	}
	return false
}

// Permissions implements agent.Permissioner. Codex reads and edits
// through its shell (it has no file tool apart), so the shell is not a
// permission it can be given or denied on its own: reading turns it on
// in a read-only sandbox, editing in a writable one, and with neither it
// is off. The web is its own switch.
func (p *Provider) Permissions() []string { return []string{"read", "edit", "web"} }

// Args builds the command line for req.
func (p *Provider) Args(req agent.Request) []string {
	args := []string{"exec"}
	if req.SessionID != "" {
		args = append(args, "resume")
	}
	args = append(args, "--json", "--skip-git-repo-check",
		// Nobody is there to approve: never ask, and let the sandbox be
		// the wall instead.
		"-c", `approval_policy="never"`)
	if writes(req) {
		args = append(args, "-c", `sandbox_mode="workspace-write"`, "-c", "sandbox_workspace_write.network_access=true")
	} else {
		args = append(args, "-c", `sandbox_mode="read-only"`)
	}
	if !has(req, "Read", "Glob", "Grep", "Write", "Edit", "MultiEdit", "Bash", "NotebookEdit") {
		// Nothing to read or change: no shell, which is how Codex reads
		// and runs, and no view_image, which reads an image by its path.
		// Tried against a fake Responses endpoint on 0.164: with both off
		// the request carries neither exec_command nor view_image; what is
		// left (apply_patch) is held by the read-only sandbox.
		args = append(args, "--disable", "shell_tool", "--disable", "view_image")
	}
	if req.Tools != nil && !has(req, "WebSearch", "WebFetch") {
		args = append(args, "-c", `web_search="disabled"`)
	}
	model := req.Model
	// With a ChatGPT sign-in the plan decides which models it may use, and
	// one it does not include is a 400 on the first request: no model is
	// imposed, and Codex runs the plan's default.
	if model == "" && len(p.cfg.Models) > 0 && p.cfg.APIKey != "" {
		model = p.cfg.Models[0]
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	prompt := message(req)
	if len(prompt) > agent.MaxArg/2 {
		// Read from the standard input (Start): an argument stops at
		// 128 KiB and the kernel refuses to start the command at all.
		prompt = "-"
	} else if strings.HasPrefix(prompt, "-") {
		prompt = " " + prompt
	}
	if req.SessionID != "" {
		args = append(args, req.SessionID)
	}
	return append(args, prompt)
}

// message is what Codex is told: it has no system text of its own, so
// the house's goes first.
func message(req agent.Request) string {
	if req.System != "" {
		return req.System + "\n\n---\n\n" + req.Prompt
	}
	return req.Prompt
}

// start runs the turn, its message on the standard input when Args left
// it out of the command line.
func (p *Provider) start(ctx context.Context, req agent.Request, env map[string]string) (agent.Run, error) {
	args := p.Args(req)
	input := ""
	if args[len(args)-1] == "-" {
		input = message(req)
	}
	return req.Jail.StartInput(ctx, &parser{}, req.Stream, req.Workdir, req.Jail.Env(env), input, p.cfg.Bin, args...)
}

// Start implements agent.Provider.
func (p *Provider) Start(ctx context.Context, req agent.Request) (agent.Run, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("codex: empty prompt")
	}
	if req.Jail.SiteDir == "" {
		req.Jail.SiteDir = req.Workdir
	}
	if err := req.Jail.Prepare(); err != nil {
		return nil, err
	}
	env := map[string]string{}
	for k, v := range req.Env {
		env[k] = v
	}
	if p.cfg.APIKey == "" && p.cfg.Login != "" && p.cfg.Home != "" {
		if err := Seed(p.cfg.Home, p.cfg.Login); err != nil {
			return nil, err
		}
		env["CODEX_HOME"] = p.cfg.Home
		return p.start(ctx, req, env)
	}
	// CODEX_API_KEY is what `codex exec` reads; nothing is written to
	// auth.json under the site's HOME.
	env["CODEX_API_KEY"] = p.cfg.APIKey
	if req.Jail.Home != "" {
		// Codex refuses a CODEX_HOME that does not exist. It is the house's
		// folder for the CLI, with the sessions an earlier release kept
		// under the site's HOME carried in once: in HOME, which a docker
		// site's container writes, its config.toml was the site's to plant,
		// and a `notify` or an MCP server there is a command Codex runs on
		// the host by itself.
		old := filepath.Join(req.Jail.Home, ".codex")
		home, err := req.Jail.ConfigHome(p.Key(), old, "sessions")
		if err != nil {
			return nil, err
		}
		if home == "" {
			home = old
			if err := os.MkdirAll(home, 0o700); err != nil {
				return nil, err
			}
		}
		env["CODEX_HOME"] = home
	}
	return p.start(ctx, req, env)
}

// Adopt implements agent.Adopter.
func (p *Provider) Adopt(ctx context.Context, pid int, stream string) agent.Run {
	return agent.AdoptCLI(ctx, &parser{}, pid, stream)
}

type parser struct {
	thread  string
	last    string
	lastErr string
	started map[string]bool
}

type item struct {
	ID               string          `json:"id"`
	Type             string          `json:"type"`
	Text             string          `json:"text"`
	Command          string          `json:"command"`
	AggregatedOutput string          `json:"aggregated_output"`
	ExitCode         *int            `json:"exit_code"`
	Status           string          `json:"status"`
	Changes          json.RawMessage `json:"changes"`
	Query            string          `json:"query"`
	Server           string          `json:"server"`
	Tool             string          `json:"tool"`
	Arguments        json.RawMessage `json:"arguments"`
	Result           json.RawMessage `json:"result"`
	Error            json.RawMessage `json:"error"`
	Message          string          `json:"message"`
}

type event struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Message  string `json:"message"`
	Item     item   `json:"item"`
	Error    struct {
		Message string `json:"message"`
	} `json:"error"`
}

// tool is an item as a tool call in the panel's names, or "" when the
// item is not one.
func tool(it item) (string, map[string]any) {
	switch it.Type {
	case "command_execution":
		return "Bash", map[string]any{"command": it.Command}
	case "file_change":
		in := map[string]any{"changes": rawAny(it.Changes)}
		var ch []struct {
			Path string `json:"path"`
		}
		if json.Unmarshal(it.Changes, &ch) == nil && len(ch) > 0 {
			in["file_path"] = ch[0].Path
		}
		return "Edit", in
	case "web_search":
		return "WebSearch", map[string]any{"query": it.Query}
	case "mcp_tool_call":
		return it.Server + "." + it.Tool, map[string]any{"arguments": rawAny(it.Arguments)}
	}
	return "", nil
}

func rawAny(r json.RawMessage) any {
	var v any
	if len(r) > 0 {
		_ = json.Unmarshal(r, &v)
	}
	return v
}

func (p *parser) Line(raw string) ([]agent.Event, bool) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "{") {
		return nil, false
	}
	var e event
	if json.Unmarshal([]byte(raw), &e) != nil {
		return nil, false
	}
	if p.started == nil {
		p.started = map[string]bool{}
	}
	switch e.Type {
	case "thread.started":
		p.thread = e.ThreadID
	case "item.started":
		if name, in := tool(e.Item); name != "" && !p.started[e.Item.ID] {
			p.started[e.Item.ID] = true
			return []agent.Event{{Kind: agent.KindTool, Tool: name, Input: in, ToolID: e.Item.ID, SessionID: p.thread}}, false
		}
	case "item.completed":
		it := e.Item
		if it.Type == "agent_message" {
			text := strings.TrimSpace(it.Text)
			if text == "" {
				return nil, false
			}
			p.last = text
			return []agent.Event{{Kind: agent.KindText, Text: text, SessionID: p.thread}}, false
		}
		name, in := tool(it)
		if name == "" {
			return nil, false
		}
		var evs []agent.Event
		if !p.started[it.ID] {
			p.started[it.ID] = true
			evs = append(evs, agent.Event{Kind: agent.KindTool, Tool: name, Input: in, ToolID: it.ID, SessionID: p.thread})
		}
		out := it.AggregatedOutput
		if out == "" && len(it.Result) > 0 {
			out = string(it.Result)
		}
		failed := it.Status == "failed" || (it.ExitCode != nil && *it.ExitCode != 0) || (len(it.Error) > 0 && string(it.Error) != "null")
		if out == "" && len(it.Error) > 0 && string(it.Error) != "null" {
			out = string(it.Error)
		}
		return append(evs, agent.Event{Kind: agent.KindToolResult, ToolID: it.ID, Output: out, IsError: failed, SessionID: p.thread}), false
	case "error":
		// "Reconnecting... n/5" is Codex retrying; the turn's end says how
		// it went.
		if !strings.HasPrefix(e.Message, "Reconnecting") {
			p.lastErr = e.Message
		}
	case "turn.failed":
		msg := e.Error.Message
		if msg == "" {
			msg = p.lastErr
		}
		if msg == "" {
			msg = "codex: the turn failed"
		}
		return []agent.Event{{Kind: agent.KindError, Text: msg, SessionID: p.thread}}, true
	case "turn.completed":
		// Codex reports tokens, not money: the cost is the account's to
		// read at OpenAI.
		return []agent.Event{{Kind: agent.KindCost, SessionID: p.thread}, {Kind: agent.KindResult, Text: p.last, SessionID: p.thread}}, true
	}
	return nil, false
}

func (p *parser) Flush() []agent.Event {
	if p.lastErr != "" {
		return []agent.Event{{Kind: agent.KindError, Text: p.lastErr, SessionID: p.thread}}
	}
	return nil
}

// Seed writes the sign-in's auth.json into the shared home when the
// sign-in is new, and leaves it alone otherwise: what is there may be a
// refresh Codex already did, newer than what the module stored.
func Seed(home, login string) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(login))
	mark := hex.EncodeToString(sum[:])
	markPath := filepath.Join(home, ".seeded")
	if cur, err := os.ReadFile(markPath); err == nil && strings.TrimSpace(string(cur)) == mark {
		if _, err := os.Stat(filepath.Join(home, "auth.json")); err == nil {
			return nil
		}
	}
	if err := fsx.WriteAtomic(filepath.Join(home, "auth.json"), []byte(login), 0o600); err != nil {
		return err
	}
	return fsx.WriteAtomic(markPath, []byte(mark+"\n"), 0o600)
}

// PlanModels asks Codex which models the signed-in account may use (`codex
// debug models`, which reads the catalog for that account), in Codex's own
// order. path is the binary, extraPath the managed Node for its shebang.
func PlanModels(ctx context.Context, path, extraPath, home, login string) ([]string, error) {
	if err := Seed(home, login); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, path, "debug", "models")
	envPath := os.Getenv("PATH")
	if extraPath != "" {
		envPath = extraPath + string(os.PathListSeparator) + envPath
	}
	cmd.Env = []string{"HOME=" + home, "CODEX_HOME=" + home, "PATH=" + envPath, "LANG=C.UTF-8"}
	cmd.Dir = home
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("codex debug models: %w", err)
	}
	var list []struct {
		Slug       string `json:"slug"`
		Visibility string `json:"visibility"`
		Priority   int    `json:"priority"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		var wrapped struct {
			Models json.RawMessage `json:"models"`
		}
		if json.Unmarshal(out, &wrapped) != nil || json.Unmarshal(wrapped.Models, &list) != nil {
			return nil, fmt.Errorf("codex debug models: %w", err)
		}
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].Priority < list[j].Priority })
	var ids []string
	for _, m := range list {
		if m.Slug != "" && m.Visibility == "list" {
			ids = append(ids, m.Slug)
		}
	}
	return ids, nil
}
