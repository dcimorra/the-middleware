// Package opencode drives the `opencode` CLI as a subprocess. OpenCode is
// not one vendor but a front to many: which models a turn can use depends
// on which providers have a key, so the keys come from the module (one per
// provider, written to the CLI's auth.json under the run's HOME) and so
// does the list of models, read from the CLI with those keys.
//
// `opencode run --format json` prints one JSON object per line: "text"
// when a block of the answer is complete, "tool_use" when a tool call has
// finished (with its input and output together), "step_start" and
// "step_finish" (with the step's cost) and "error". There is no closing
// event: the run ends when the process does, and the end is made here.
package opencode

import (
	"context"
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
	// Key is the name the provider has in the registry and the manifests:
	// "opencode" for the module that connects any provider, the vendor's
	// own ("deepseek", "zai"…) for a vendor's module run by OpenCode.
	Key string
	// Bin is the executable; "opencode" is looked up in PATH.
	Bin string
	// Auth is one API key per provider id ("openai", "openrouter"…), as
	// OpenCode's catalog names them.
	Auth map[string]string
	// Models offered to the panel, "provider/model"; the first is the
	// default.
	Models []string
}

// Provider implements agent.Provider.
type Provider struct {
	cfg Config
}

// New returns a provider with defaults filled in.
func New(cfg Config) *Provider {
	if cfg.Bin == "" {
		cfg.Bin = "opencode"
	}
	if cfg.Key == "" {
		cfg.Key = "opencode"
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
	if len(p.cfg.Auth) == 0 {
		if p.cfg.Key != "opencode" {
			return false, "no API key: add it in Modules"
		}
		return false, "no provider connected: add a provider's key in Modules → OpenCode"
	}
	return true, path
}

// Models implements agent.Provider.
func (p *Provider) Models() []string { return append([]string(nil), p.cfg.Models...) }

// Args builds the command line for req.
func (p *Provider) Args(req agent.Request) []string {
	prompt := req.Prompt
	if req.System != "" {
		// `run` has no system-prompt flag; the preamble goes ahead of the
		// prompt, set off so the model reads it as context.
		prompt = req.System + "\n\n---\n\n" + req.Prompt
	}
	// A message starting with a dash would be read as a flag.
	if strings.HasPrefix(prompt, "-") {
		prompt = " " + prompt
	}
	args := []string{"run", "--format", "json"}
	model := req.Model
	if model == "" && len(p.cfg.Models) > 0 {
		model = p.cfg.Models[0]
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	if req.SessionID != "" {
		args = append(args, "--session", req.SessionID)
	}
	return append(args, prompt)
}

// claudeTools maps the tool names the house's levels speak (Claude Code's)
// to OpenCode's permission keys.
var claudeTools = map[string][]string{
	"Read": {"read"}, "Glob": {"glob"}, "Grep": {"grep"}, "LS": {"list"},
	"Write": {"edit"}, "Edit": {"edit"}, "MultiEdit": {"edit"}, "NotebookEdit": {"edit"},
	"Bash": {"bash"}, "WebFetch": {"webfetch"}, "WebSearch": {"websearch", "codesearch"},
	"Task": {"task"}, "TodoWrite": {"todowrite"},
}

// every tool OpenCode asks permission for.
var allTools = []string{"read", "glob", "grep", "list", "edit", "bash", "webfetch", "websearch", "codesearch", "task", "todowrite", "lsp", "skill"}

// Permissions is the jail as OpenCode's permission rules. `run` refuses
// whatever would ask, so "deny" and "ask" both stop a call; what is not
// listed is allowed. Outside the folder is denied wholesale (the other
// sites, the data, the machine), and a folder lent for reading is let in
// for reading and denied to edits.
func Permissions(req agent.Request) map[string]any {
	perm := map[string]any{}
	allowed := map[string]bool{}
	if req.Tools != nil {
		for _, t := range req.Tools {
			for _, k := range claudeTools[t] {
				allowed[k] = true
			}
		}
		for _, k := range allTools {
			if !allowed[k] {
				perm[k] = "deny"
			}
		}
	}
	for _, t := range req.Disallowed {
		for _, k := range claudeTools[t] {
			perm[k] = "deny"
		}
	}
	ext := map[string]any{"*": "deny"}
	for _, d := range append(append([]string(nil), req.Jail.Lent...), req.Jail.Shared...) {
		ext[filepath.Join(d, "**")] = "allow"
	}
	perm["external_directory"] = ext
	if _, closed := perm["bash"]; !closed && len(req.Jail.Commands) > 0 {
		// Later rules win: everything first, the forbidden prefixes after.
		bash := map[string]any{"*": "allow"}
		for _, c := range req.Jail.Commands {
			bash[c] = "deny"
			bash[c+" *"] = "deny"
		}
		perm["bash"] = bash
	}
	if _, closed := perm["edit"]; !closed && len(req.Jail.Lent) > 0 {
		edit := map[string]any{"*": "allow"}
		for _, d := range req.Jail.Lent {
			edit[filepath.Join(d, "**")] = "deny"
		}
		perm["edit"] = edit
	}
	if _, closed := perm["read"]; !closed && len(req.Jail.Secrets) > 0 {
		read := map[string]any{"*": "allow"}
		for _, s := range req.Jail.Secrets {
			read[s] = "deny"
		}
		perm["read"] = read
	}
	return perm
}

// orderedJSON writes a rule set with "*" first: OpenCode applies the last
// matching rule, so the catch-all must come before the exceptions.
func orderedJSON(v any) string {
	var b strings.Builder
	writeOrdered(&b, v)
	return b.String()
}

func writeOrdered(b *strings.Builder, v any) {
	m, ok := v.(map[string]any)
	if !ok {
		raw, _ := json.Marshal(v)
		b.Write(raw)
		return
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i] == "*" || keys[j] == "*" {
			return keys[i] == "*"
		}
		return keys[i] < keys[j]
	})
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		raw, _ := json.Marshal(k)
		b.Write(raw)
		b.WriteByte(':')
		writeOrdered(b, m[k])
	}
	b.WriteByte('}')
}

// Start implements agent.Provider.
func (p *Provider) Start(ctx context.Context, req agent.Request) (agent.Run, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("opencode: empty prompt")
	}
	if req.Jail.SiteDir == "" {
		req.Jail.SiteDir = req.Workdir
	}
	if err := req.Jail.Prepare(); err != nil {
		return nil, err
	}
	if req.Jail.Home != "" {
		if err := WriteAuth(req.Jail.Home, p.cfg.Auth); err != nil {
			return nil, err
		}
	}
	env := map[string]string{}
	for k, v := range req.Env {
		env[k] = v
	}
	cfg := map[string]any{
		"$schema":    "https://opencode.ai/config.json",
		"permission": Permissions(req),
		"autoupdate": false,
		"share":      "disabled",
	}
	env["OPENCODE_CONFIG_CONTENT"] = orderedJSON(cfg)
	env["OPENCODE_DISABLE_AUTOUPDATE"] = "1"
	env["OPENCODE_DISABLE_SHARE"] = "1"
	return req.Jail.Start(ctx, &parser{}, req.Stream, req.Workdir, req.Jail.Env(env), p.cfg.Bin, p.Args(req)...)
}

// Adopt implements agent.Adopter.
func (p *Provider) Adopt(ctx context.Context, pid int, stream string) agent.Run {
	return agent.AdoptCLI(ctx, &parser{}, pid, stream)
}

// AuthPath is where the CLI keeps its keys under home.
func AuthPath(home string) string {
	return filepath.Join(home, ".local", "share", "opencode", "auth.json")
}

// WriteAuth puts the keys where the CLI reads them, only when they
// changed, so a turn every few seconds does not rewrite the file.
func WriteAuth(home string, auth map[string]string) error {
	doc := map[string]any{}
	for id, key := range auth {
		doc[id] = map[string]string{"type": "api", "key": key}
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	path := AuthPath(home)
	if cur, err := os.ReadFile(path); err == nil && string(cur) == string(raw) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return fsx.WriteAtomic(path, raw, 0o600)
}

// parser reads the JSON lines. Lines that are not JSON (a first run's
// database migration, a crash's stack) are kept aside and shown only if
// the run produced nothing else.
type parser struct {
	sessionID string
	last      string
	cost      float64
	sawError  bool
	sawAny    bool
	noise     []string
}

type line struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionID"`
	Part      struct {
		Type   string  `json:"type"`
		Text   string  `json:"text"`
		Tool   string  `json:"tool"`
		CallID string  `json:"callID"`
		Cost   float64 `json:"cost"`
		State  struct {
			Status string         `json:"status"`
			Input  map[string]any `json:"input"`
			Output string         `json:"output"`
			Error  string         `json:"error"`
		} `json:"state"`
	} `json:"part"`
	Error struct {
		Name string `json:"name"`
		Data struct {
			Message string `json:"message"`
		} `json:"data"`
	} `json:"error"`
}

// toolNames are OpenCode's tools by the names the panel draws (Claude
// Code's), so a step reads the same whichever agent took it.
var toolNames = map[string]string{
	"bash": "Bash", "read": "Read", "write": "Write", "edit": "Edit", "glob": "Glob", "grep": "Grep",
	"list": "LS", "webfetch": "WebFetch", "websearch": "WebSearch", "task": "Task", "todowrite": "TodoWrite",
	"patch": "Edit", "multiedit": "MultiEdit",
}

func toolInput(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := map[string]any{}
	for k, v := range in {
		switch k {
		case "filePath":
			out["file_path"] = v
		case "oldString":
			out["old_string"] = v
		case "newString":
			out["new_string"] = v
		default:
			out[k] = v
		}
	}
	return out
}

func (p *parser) Line(raw string) ([]agent.Event, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, false
	}
	var l line
	if !strings.HasPrefix(trimmed, "{") || json.Unmarshal([]byte(trimmed), &l) != nil || l.Type == "" {
		if len(p.noise) < 40 {
			p.noise = append(p.noise, trimmed)
		}
		return nil, false
	}
	p.sawAny = true
	if l.SessionID != "" {
		p.sessionID = l.SessionID
	}
	switch l.Type {
	case "text":
		text := strings.TrimSpace(l.Part.Text)
		if text == "" {
			return nil, false
		}
		p.last = text
		return []agent.Event{{Kind: agent.KindText, Text: text, SessionID: p.sessionID}}, false
	case "tool_use":
		name := toolNames[l.Part.Tool]
		if name == "" {
			name = l.Part.Tool
		}
		out := l.Part.State.Output
		failed := l.Part.State.Status == "error"
		if failed && out == "" {
			out = l.Part.State.Error
		}
		return []agent.Event{
			{Kind: agent.KindTool, Tool: name, Input: toolInput(l.Part.State.Input), ToolID: l.Part.CallID, SessionID: p.sessionID},
			{Kind: agent.KindToolResult, ToolID: l.Part.CallID, Output: out, IsError: failed, SessionID: p.sessionID},
		}, false
	case "step_finish":
		p.cost += l.Part.Cost
		return nil, false
	case "error":
		msg := l.Error.Data.Message
		if msg == "" {
			msg = l.Error.Name
		}
		if msg == "" {
			msg = "opencode reported an error"
		}
		p.sawError = true
		return []agent.Event{{Kind: agent.KindCost, Cost: p.cost, SessionID: p.sessionID}, {Kind: agent.KindError, Text: msg, SessionID: p.sessionID}}, true
	}
	return nil, false
}

func (p *parser) Flush() []agent.Event {
	if p.sawError {
		return nil
	}
	if !p.sawAny {
		// Nothing a turn says came out: what did is the reason.
		msg := strings.Join(p.noise, "\n")
		if msg == "" {
			msg = "opencode ended without answering"
		}
		return []agent.Event{{Kind: agent.KindError, Text: msg, SessionID: p.sessionID}}
	}
	return []agent.Event{
		{Kind: agent.KindCost, Cost: p.cost, SessionID: p.sessionID},
		{Kind: agent.KindResult, Text: p.last, Cost: p.cost, SessionID: p.sessionID},
	}
}
