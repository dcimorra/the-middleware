package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/agent"
	"github.com/dcimorra/the-middleware/internal/agent/claude"
	"github.com/dcimorra/the-middleware/internal/agent/codex"
	"github.com/dcimorra/the-middleware/internal/agent/grok"
	"github.com/dcimorra/the-middleware/internal/agent/kimi"
	"github.com/dcimorra/the-middleware/internal/agent/qwen"
	"github.com/dcimorra/the-middleware/internal/agent/vibe"
	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/modules"
)

// A vendor's module runs the vendor's own agent when it has one that works
// detached, the way `claude -p` does: OpenAI's Codex, Alibaba's Qwen Code,
// Moonshot's Kimi Code, xAI's Grok Build and Mistral's Vibe. DeepSeek and
// Z.AI have none, and both document Claude Code against their
// Anthropic-compatible APIs, so that is what runs them: the Claude
// adapter with their address and key, never the Claude module's.
//
// The models a card and the chat offer are read from the vendor's own
// /models with the key (OpenAI-compatible, every one of them that has it),
// filtered to the ones that chat; a short list stands in until then and
// when the vendor has no such endpoint.

// Vendor is a vendor's module.
type Vendor struct {
	// Key is the agent's name in the chat and the manifests; the module is
	// agent-<Key>.
	Key  string
	Name string
	Desc string
	// Bin is the command that runs the turns, as deps names it.
	Bin     string
	KeyHelp string
	// BaseURL is the default API address; with BaseURLHelp the card offers
	// the field, for vendors with more than one (a plan, a region).
	BaseURL     string
	BaseURLHelp string
	// ModelsURL, when set, lists the models (OpenAI-compatible) from the
	// base URL ("{base}/models") or a fixed address.
	ModelsURL string
	Keep      func(id string) bool
	Defaults  []string
	Build     func(a BuildArgs) agent.Provider
	// Login, for a vendor whose agent signs in with a subscription (Codex
	// with ChatGPT), is that sign-in; the API key stays the alternative.
	Login func(bin string) loginSpec
	// PlanModels lists the models a subscription may use, from the agent
	// signed in as the account; home is the module's own folder.
	PlanModels func(ctx context.Context, bin, extraPath, home, login string) ([]string, error)
}

// BuildArgs is what a vendor's agent is built from.
type BuildArgs struct {
	Key, Base, Bin string
	Models         []string
	// Login is the stored sign-in, and Home a folder of the module's own
	// for an agent that keeps its session there.
	Login, Home string
}

func excludes(id string, words ...string) bool {
	for _, w := range words {
		if strings.Contains(id, w) {
			return false
		}
	}
	return true
}

// anthropicVendor is the environment Claude Code runs a vendor's model
// with, as DeepSeek and Z.AI document it.
func anthropicVendor(base, key string, models []string) map[string]string {
	env := map[string]string{
		"ANTHROPIC_BASE_URL":   base,
		"ANTHROPIC_AUTH_TOKEN": key,
		"API_TIMEOUT_MS":       "3000000",
		// Claude Code's own background calls (titles, update checks) would
		// go to the vendor too, or nowhere.
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
		// A key the vendor refuses was retried for minutes with the CLI's
		// default, the turn hanging; three retries still cover a blip.
		"CLAUDE_CODE_MAX_RETRIES": "3",
	}
	if len(models) > 0 {
		m := models[0]
		for _, k := range []string{"ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL", "CLAUDE_CODE_SUBAGENT_MODEL"} {
			env[k] = m
		}
	}
	return env
}

// Vendors are the best known, and only those: every other provider is in
// the OpenCode module's catalog.
var Vendors = []Vendor{
	{Key: "openai", Name: "OpenAI (Codex)", Bin: "codex",
		Desc:      "OpenAI's models run by Codex CLI, OpenAI's own agent, with an OpenAI API key.",
		KeyHelp:   "From platform.openai.com → API keys. Or leave it empty and sign in with ChatGPT below.",
		ModelsURL: "https://api.openai.com/v1/models",
		Keep: func(id string) bool {
			return (strings.HasPrefix(id, "gpt-5") || strings.Contains(id, "codex")) && excludes(id, "image", "audio", "realtime", "tts", "transcribe", "search", "chat-latest")
		},
		Defaults: []string{"gpt-5.5", "gpt-5.6", "gpt-5.3-codex"},
		Login:    codexLoginSpec,
		PlanModels: func(ctx context.Context, bin, extraPath, home, login string) ([]string, error) {
			return codex.PlanModels(ctx, bin, extraPath, filepath.Join(home, "codex-home"), login)
		},
		Build: func(a BuildArgs) agent.Provider {
			return codex.New(codex.Config{Bin: a.Bin, APIKey: a.Key, Login: a.Login, Home: filepath.Join(a.Home, "codex-home"), Models: a.Models})
		}},
	{Key: "deepseek", Name: "DeepSeek", Bin: "claude",
		Desc:      "DeepSeek's models run by Claude Code against DeepSeek's Anthropic-compatible API, as DeepSeek documents it.",
		KeyHelp:   "From platform.deepseek.com → API keys.",
		BaseURL:   "https://api.deepseek.com/anthropic",
		ModelsURL: "https://api.deepseek.com/models",
		Defaults:  []string{"deepseek-flash", "deepseek-v4-pro"},
		Build: func(a BuildArgs) agent.Provider {
			return claude.New(claude.Config{Key: "deepseek", Bin: a.Bin, Env: anthropicVendor(a.Base, a.Key, a.Models), Models: a.Models})
		}},
	{Key: "zai", Name: "Z.AI (GLM)", Bin: "claude",
		Desc:     "Zhipu's GLM models run by Claude Code against Z.AI's Anthropic-compatible API, as Z.AI documents it for the GLM Coding Plan.",
		KeyHelp:  "From z.ai → API keys. A GLM Coding Plan key works too.",
		BaseURL:  "https://api.z.ai/api/anthropic",
		Defaults: []string{"glm-5.3", "glm-5.3-flash"},
		Build: func(a BuildArgs) agent.Provider {
			return claude.New(claude.Config{Key: "zai", Bin: a.Bin, Env: anthropicVendor(a.Base, a.Key, a.Models), Models: a.Models})
		}},
	{Key: "kimi", Name: "Kimi (Kimi Code)", Bin: "kimi",
		Desc:        "Moonshot's Kimi models run by Kimi Code, Moonshot's own agent.",
		KeyHelp:     "From platform.moonshot.ai → API keys.",
		BaseURL:     kimi.DefaultBaseURL,
		BaseURLHelp: "https://api.moonshot.ai/v1 for a platform key; https://api.kimi.com/coding/v1 for a Kimi Code membership.",
		ModelsURL:   "{base}/models",
		Keep:        func(id string) bool { return strings.HasPrefix(id, "kimi") && excludes(id, "vision", "latest-8k") },
		Defaults:    []string{"kimi-k3", "kimi-k2.7-code"},
		Build: func(a BuildArgs) agent.Provider {
			return kimi.New(kimi.Config{Bin: a.Bin, APIKey: a.Key, BaseURL: a.Base, Models: a.Models})
		}},
	{Key: "qwen", Name: "Qwen (Qwen Code)", Bin: "qwen",
		Desc:        "Alibaba's Qwen models run by Qwen Code, Alibaba's own agent.",
		KeyHelp:     "A QwenCloud / Model Studio API key.",
		BaseURL:     qwen.DefaultBaseURL,
		BaseURLHelp: "QwenCloud international (default); https://coding-intl.dashscope.aliyuncs.com/v1 for the Coding Plan; https://maas.qianwenaiapi.com/compatible-mode/v1 in China.",
		ModelsURL:   "{base}/models",
		Keep: func(id string) bool {
			return strings.HasPrefix(id, "qwen") && excludes(id, "omni", "-vl", "audio", "tts", "asr", "embedding", "image", "mt-", "realtime")
		},
		Defaults: []string{"qwen3.8-max", "qwen3-coder-plus"},
		Build: func(a BuildArgs) agent.Provider {
			return qwen.New(qwen.Config{Bin: a.Bin, APIKey: a.Key, BaseURL: a.Base, Models: a.Models})
		}},
	{Key: "mistral", Name: "Mistral (Vibe)", Bin: "vibe",
		Desc:    "Mistral's model run by Vibe, Mistral's own agent (installed with uv, which brings its own Python).",
		KeyHelp: "From console.mistral.ai → API keys.",
		// Vibe runs the model it is built around.
		Defaults: []string{"mistral-medium-3.5"},
		Build: func(a BuildArgs) agent.Provider {
			return vibe.New(vibe.Config{Bin: a.Bin, APIKey: a.Key, Models: a.Models})
		}},
	{Key: "xai", Name: "xAI (Grok Build)", Bin: "grok",
		Desc:      "xAI's Grok models run by Grok Build, xAI's own agent (a binary downloaded from x.ai).",
		KeyHelp:   "From console.x.ai → API keys.",
		ModelsURL: "https://api.x.ai/v1/models",
		Keep: func(id string) bool {
			return strings.HasPrefix(id, "grok") && excludes(id, "imagine", "image", "vision")
		},
		Defaults: []string{"grok-4.7", "grok-build-0.1"},
		Build: func(a BuildArgs) agent.Provider {
			return grok.New(grok.Config{Bin: a.Bin, APIKey: a.Key, Models: a.Models})
		}},
}

// VendorModule is a vendor's module.
type VendorModule struct {
	*Module
	V    Vendor
	Home string

	rmu      sync.Mutex
	cmu      sync.Mutex
	models   []string
	modelsAt time.Time
	modErr   string
	busy     bool
}

// NewVendor builds a vendor's module over reg; homes is data/agents.
func NewVendor(reg *agent.Registry, homes string, v Vendor) *VendorModule {
	vm := &VendorModule{V: v, Home: filepath.Join(homes, v.Key)}
	needs := []modules.Need{{Key: "api_key", Label: "API key", Secret: true, Help: v.KeyHelp, Optional: v.Login != nil}}
	if v.BaseURLHelp != "" {
		needs = append(needs, modules.Need{Key: "base_url", Label: "API address", Optional: true, Help: v.BaseURLHelp})
	}
	needs = append(needs,
		modules.Need{Key: "model", Label: "Default model", Optional: true, Help: "Offered first in the chat. Empty: the first one listed."},
		modules.Need{Key: "bin", Label: "Binary", Optional: true, Help: "Path to the " + v.Bin + " executable. Empty: the one this installation keeps under modules/node, installed and updated for you.", Local: true},
	)
	m := &Module{
		key: "agent-" + v.Key, name: v.Name, desc: v.Desc, bin: v.Bin, needs: needs,
		reg: reg, secretKey: "api_key", removeOnDisable: true, home: vm.Home,
		models: vm.list,
	}
	if v.Login != nil {
		// The sign-in is stored apart from the key; with both, the key wins.
		m.secretKey, m.loginSpec = "subscription", v.Login
		m.needs = append(m.needs, modules.Need{Key: "subscription", Secret: true, Optional: true, Hidden: true}, modules.Need{Key: "oauth_since", Optional: true, Hidden: true})
	}
	m.build = func(cfg modules.Config) agent.Provider {
		return v.Build(BuildArgs{Key: strings.TrimSpace(cfg["api_key"]), Base: vm.base(cfg), Bin: vm.binOf(cfg), Models: modelsWith(cfg["model"], vm.list()), Login: cfg["subscription"], Home: vm.Home})
	}
	vm.Module = m
	m.init()
	vm.forgetOpenCode()
	if raw, err := os.ReadFile(vm.cachePath()); err == nil {
		vm.models = nonEmpty(strings.Split(string(raw), "\n"))
	}
	return vm
}

func nonEmpty(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// cachePath is not alpha.19's models.txt, which held OpenCode's names
// (provider/model) for these same modules.
func (vm *VendorModule) cachePath() string { return filepath.Join(vm.Home, "vendor-models.txt") }

// forgetOpenCode removes what these modules left when OpenCode ran them
// (alpha.19): its models, its catalog and its auth.json, which held the
// key in clear under this HOME.
func (vm *VendorModule) forgetOpenCode() {
	for _, p := range []string{"models.txt", ".local", ".cache", ".config"} {
		_ = os.RemoveAll(filepath.Join(vm.Home, p))
	}
}

// binOf is the configured binary, unless it is the OpenCode that ran this
// vendor before it had its own agent.
func (vm *VendorModule) binOf(cfg modules.Config) string {
	b := strings.TrimSpace(cfg["bin"])
	if filepath.Base(b) == "opencode" && vm.V.Bin != "opencode" {
		return ""
	}
	return b
}

func (vm *VendorModule) base(cfg modules.Config) string {
	if b := strings.TrimRight(strings.TrimSpace(cfg["base_url"]), "/"); b != "" {
		return b
	}
	return vm.V.BaseURL
}

// list is the models read last, or the defaults until there are some.
func (vm *VendorModule) list() []string {
	vm.cmu.Lock()
	defer vm.cmu.Unlock()
	if len(vm.models) > 0 {
		return append([]string(nil), vm.models...)
	}
	return append([]string(nil), vm.V.Defaults...)
}

// modelsHTTP is the client that asks the vendors; tests replace it.
var modelsHTTP = &http.Client{Timeout: 20 * time.Second}

// RefreshModels asks the vendor which models the key can use.
func (vm *VendorModule) RefreshModels(ctx context.Context) error {
	if vm.V.ModelsURL == "" {
		return nil
	}
	vm.rmu.Lock()
	defer vm.rmu.Unlock()
	vm.cmu.Lock()
	vm.busy = true
	vm.cmu.Unlock()
	defer func() {
		vm.cmu.Lock()
		vm.busy = false
		vm.cmu.Unlock()
	}()
	cfg := vm.config()
	cfg["bin"] = vm.binOf(cfg)
	key := strings.TrimSpace(cfg["api_key"])
	if key == "" {
		if cfg["subscription"] == "" || vm.V.PlanModels == nil {
			if cfg["subscription"] != "" {
				return nil
			}
			return errors.New("no API key")
		}
		// A subscription's models are the plan's: the vendor's agent says
		// which, signed in as the account.
		bin, err := vm.binary(cfg)
		var ids []string
		if err == nil {
			ids, err = vm.V.PlanModels(ctx, bin, vm.extraPath(), vm.Home, cfg["subscription"])
		}
		if err == nil && len(ids) == 0 {
			err = errors.New("the plan lists no model")
		}
		vm.cmu.Lock()
		vm.modelsAt = time.Now()
		if err != nil {
			vm.modErr = err.Error()
		} else {
			vm.modErr, vm.models = "", ids
		}
		vm.cmu.Unlock()
		if err != nil {
			return err
		}
		_ = os.MkdirAll(vm.Home, 0o700)
		_ = fsx.WriteAtomic(vm.cachePath(), []byte(strings.Join(ids, "\n")+"\n"), 0o600)
		if vm.Store != nil && vm.Store.Enabled(vm.key) {
			vm.reg.Register(vm.build(withBin(cfg, vm.Module)))
		}
		return nil
	}
	url := strings.ReplaceAll(vm.V.ModelsURL, "{base}", vm.base(cfg))
	ids, err := listModels(ctx, url, key)
	if err == nil {
		var keep []string
		for _, id := range ids {
			if vm.V.Keep == nil || vm.V.Keep(id) {
				keep = append(keep, id)
			}
		}
		ids = order(keep, vm.V.Defaults)
		if len(ids) == 0 {
			err = fmt.Errorf("%s lists no model for an agent", url)
		}
	}
	vm.cmu.Lock()
	vm.modelsAt = time.Now()
	if err != nil {
		vm.modErr = err.Error()
	} else {
		vm.modErr, vm.models = "", ids
	}
	vm.cmu.Unlock()
	if err != nil {
		return err
	}
	_ = os.MkdirAll(vm.Home, 0o700)
	_ = fsx.WriteAtomic(vm.cachePath(), []byte(strings.Join(ids, "\n")+"\n"), 0o600)
	if vm.Store != nil && vm.Store.Enabled(vm.key) {
		vm.reg.Register(vm.build(withBin(cfg, vm.Module)))
	}
	return nil
}

// order puts the defaults that exist first, the rest after, by name.
func order(ids, first []string) []string {
	have := map[string]bool{}
	for _, id := range ids {
		have[id] = true
	}
	var out []string
	used := map[string]bool{}
	for _, f := range first {
		if have[f] && !used[f] {
			out = append(out, f)
			used[f] = true
		}
	}
	rest := []string{}
	for _, id := range ids {
		if !used[id] {
			rest = append(rest, id)
			used[id] = true
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(rest)))
	return append(out, rest...)
}

func listModels(ctx context.Context, url, key string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	res, err := modelsHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		err := fmt.Errorf("%s answered %s: %s", url, res.Status, msg)
		if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
			return nil, &refusedError{err}
		}
		return nil, err
	}
	var doc struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", url, err)
	}
	var out []string
	for _, d := range doc.Data {
		if d.ID != "" {
			out = append(out, d.ID)
		}
	}
	return out, nil
}

// Session is "api_key" once there is one.
func (vm *VendorModule) Session() Session {
	return sessionOf(vm.config(), vm.secretKey)
}

// Verify implements modules.Module.
func (vm *VendorModule) Verify(ctx context.Context, cfg modules.Config) []modules.Line {
	var lines []modules.Line
	cfg = withConfig(cfg, "bin", vm.binOf(cfg))
	path, err := vm.binary(cfg)
	switch {
	case err != nil && strings.TrimSpace(cfg["bin"]) == "" && vm.Deps != nil:
		lines = append(lines, modules.Skip(vm.bin+" is not installed yet: enabling installs it"))
	case err != nil:
		lines = append(lines, modules.Fail(fmt.Sprintf("%s: %v", vm.bin, err)))
	default:
		vctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		v, err := vm.Version(vctx, path)
		cancel()
		if err != nil {
			lines = append(lines, modules.Fail(fmt.Sprintf("%s --version: %v", path, err)))
		} else {
			lines = append(lines, modules.OK(fmt.Sprintf("%s answers --version: %s", path, v)))
		}
	}
	key := strings.TrimSpace(cfg["api_key"])
	switch {
	case key == "" && vm.V.Login != nil && strings.TrimSpace(cfg["subscription"]) != "":
		lines = append(lines, modules.OK("signed in with a subscription since "+cfg["oauth_since"]+"; not probed here, the first turn says if it still works"))
	case key == "" && vm.V.Login != nil:
		lines = append(lines, modules.Fail("no API key and no sign-in: give a key, or sign in below"))
	case key == "":
		lines = append(lines, modules.Fail("no API key"))
	case vm.V.ModelsURL == "":
		lines = append(lines, modules.Skip("the key is not probed here: the first turn says if it is wrong"))
	default:
		url := strings.ReplaceAll(vm.V.ModelsURL, "{base}", vm.base(cfg))
		vctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		ids, err := listModels(vctx, url, key)
		cancel()
		var refused *refusedError
		switch {
		case errors.As(err, &refused):
			// Refused is the key; anything else (an address of a plan with
			// no model list, the vendor down) is not a reason to keep the
			// agent out.
			lines = append(lines, modules.Fail("the vendor refuses the key: "+err.Error()))
		case err != nil:
			lines = append(lines, modules.Skip("the models could not be listed, the default ones are offered: "+err.Error()))
		default:
			lines = append(lines, modules.OK(fmt.Sprintf("the key lists %d models at %s", len(ids), url)))
		}
	}
	return lines
}

// Enable implements modules.Module: the agent goes into the chat with the
// models read last, and they are read again in the background.
func (vm *VendorModule) Enable(ctx context.Context, cfg modules.Config) error {
	cfg = withConfig(cfg, "bin", vm.binOf(cfg))
	if err := vm.Module.Enable(ctx, cfg); err != nil {
		return err
	}
	go func() {
		rctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = vm.RefreshModels(rctx)
	}()
	return nil
}

// Install implements modules.Module, past an OpenCode left in the config.
func (vm *VendorModule) Install(ctx context.Context, cfg modules.Config, progress func(string)) error {
	return vm.Module.Install(ctx, withConfig(cfg, "bin", vm.binOf(cfg)), progress)
}

// Routes implements modules.Module.
func (vm *VendorModule) Routes() map[string]http.Handler {
	out := map[string]http.Handler{
		"GET /{$}":             http.HandlerFunc(vm.handleModels),
		"GET /models":          http.HandlerFunc(vm.handleModels),
		"POST /models/refresh": http.HandlerFunc(vm.handleRefresh),
		"POST /binary/sync":    http.HandlerFunc(vm.handleBinarySync),
	}
	if vm.V.Login != nil {
		for k, h := range vm.Module.Routes() {
			if strings.Contains(k, "/login") || strings.Contains(k, "/session") || strings.Contains(k, "/usage") {
				out[k] = h
			}
		}
	}
	return out
}

// SetupRoutes implements modules.Setup: the sign-in is how a module with
// one gets enabled.
func (vm *VendorModule) SetupRoutes() []string {
	if vm.V.Login != nil {
		return vm.Module.SetupRoutes()
	}
	return []string{"POST /binary/sync"}
}

func (vm *VendorModule) handleModels(w http.ResponseWriter, r *http.Request) {
	cfg := vm.config()
	vm.cmu.Lock()
	out := map[string]any{"provider": vm.V.Key, "session": vm.Session(), "models_error": vm.modErr, "reading": vm.busy}
	if !vm.modelsAt.IsZero() {
		out["models_at"] = vm.modelsAt
	}
	vm.cmu.Unlock()
	out["models"] = modelsWith(cfg["model"], vm.list())
	if vm.Deps != nil {
		for _, s := range vm.Deps.Statuses() {
			if s.Name == vm.bin {
				out["binary"] = s
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (vm *VendorModule) handleRefresh(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	_ = vm.RefreshModels(ctx)
	vm.handleModels(w, r)
}

var _ modules.Module = (*VendorModule)(nil)

// refusedError is the vendor saying no to the key.
type refusedError struct{ error }

func (e *refusedError) Unwrap() error { return e.error }

func withConfig(cfg modules.Config, k, v string) modules.Config {
	out := modules.Config{}
	for a, b := range cfg {
		out[a] = b
	}
	out[k] = v
	return out
}
