// Package agents holds the agent modules: agent-claude and agent-agy
// here, the vendors' in vendors.go and OpenCode's in opencode.go. Enabling one is the whole setup: the CLI is installed
// under modules/node if the machine has none (and kept current from
// then on), the subscription is signed in from the panel with the
// vendor's own interactive flow run on a terminal of ours, and the
// session that comes out is stored encrypted and handed to every turn.
// An API key is the alternative for whoever has one. The module never
// talks to the vendor itself: the only thing it runs is `<bin>
// --version`, so a bad session is found by the first turn, and the
// panel says so.
package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/agent"
	"github.com/dcimorra/the-middleware/internal/agent/agy"
	"github.com/dcimorra/the-middleware/internal/agent/claude"
	"github.com/dcimorra/the-middleware/internal/deps"
	"github.com/dcimorra/the-middleware/internal/modules"
)

// Keys of the two modules.
const (
	ClaudeKey = "agent-claude"
	AgyKey    = "agent-agy"
)

// Module is one agent module.
type Module struct {
	key, name, desc, bin string
	needs                []modules.Need
	build                func(cfg modules.Config) agent.Provider
	reg                  *agent.Registry
	// fallback is what Disable puts back: the provider the process started
	// with, so turning the module off returns the chat to the CLI's own
	// login rather than to no provider at all.
	fallback agent.Provider
	// secretKey is the config key the signed-in session is stored under.
	secretKey string
	// removeOnDisable takes the provider out of the chat when the module
	// is turned off, for agents that exist only through their module.
	removeOnDisable bool
	loginSpec       func(bin string) loginSpec
	// models are the agent's models as the module knows them now, offered
	// as the choices of its "model" need; labels say what each runs.
	models func() []string
	labels map[string]string

	// Deps installs and finds the CLI; nil means PATH only.
	Deps *deps.Manager
	// Store is the manager this module is registered in, for the login
	// to write the session into the encrypted store. Set by the wiring.
	Store *modules.Manager

	// LookPath and Version are replaced by tests.
	LookPath func(string) (string, error)
	Version  func(ctx context.Context, bin string) (string, error)

	// Usage is the book of subscription limits, shared with the chat.
	Usage *agent.UsageBook
	// home is the module's own folder, where an agent keeps its state.
	home string

	mu       sync.Mutex
	login    *Login
	usageAt  time.Time
	usageErr string
}

// Claude builds the agent-claude module over reg.
func Claude(reg *agent.Registry) *Module {
	m := &Module{
		key:  ClaudeKey,
		name: "Claude",
		desc: "Conversations run by Claude Code. Sign in with your Claude subscription from here, or give it an API key.",
		bin:  "claude",
		needs: []modules.Need{
			{Key: "model", Label: "Default model", Optional: true, Help: "Offered first in the panel. Empty: the first one listed."},
			{Key: "api_key", Label: "API key (instead of a subscription)", Secret: true, Optional: true, Help: "Passed to the CLI as ANTHROPIC_API_KEY. Leave empty and sign in below to use your subscription."},
			{Key: "bin", Label: "Binary", Optional: true, Help: "Path to the claude executable. Empty: the one this installation keeps under modules/node, installed and updated for you.", Local: true},
			{Key: "oauth_token", Secret: true, Optional: true, Hidden: true},
			{Key: "oauth_since", Optional: true, Hidden: true},
		},
		reg:       reg,
		secretKey: "oauth_token",
		loginSpec: claudeLoginSpec,
		models:    func() []string { return claude.New(claude.Config{}).Models() },
		labels:    claude.Aliases,
	}
	m.build = func(cfg modules.Config) agent.Provider {
		return claude.New(claude.Config{
			Bin:        cfg["bin"],
			APIKey:     cfg["api_key"],
			OAuthToken: cfg["oauth_token"],
			Models:     modelsWith(cfg["model"], claude.New(claude.Config{}).Models()),
		})
	}
	m.init()
	return m
}

// Agy builds the agent-agy module over reg: Google's Antigravity CLI,
// which took the Gemini CLI's place. It signs in with a Google account
// only; home is the module's own folder, where the usage is asked from.
func Agy(reg *agent.Registry, home string) *Module {
	m := &Module{
		key:  AgyKey,
		name: "Antigravity (agy)",
		desc: "Conversations run by Google's Antigravity CLI. Sign in with your Google account from here.",
		bin:  "agy",
		needs: []modules.Need{
			{Key: "model", Label: "Default model", Optional: true, Help: "Offered first in the panel. Empty: the first one listed."},
			{Key: "bin", Label: "Binary", Optional: true, Help: "Path to the agy executable. Empty: the one this installation keeps under modules/node, downloaded from Google and updated for you.", Local: true},
			{Key: "oauth_token", Secret: true, Optional: true, Hidden: true},
			{Key: "oauth_since", Optional: true, Hidden: true},
		},
		reg:       reg,
		secretKey: "oauth_token",
		loginSpec: agyLoginSpec,
		home:      home,
		models:    agy.DefaultModels,
	}
	m.build = func(cfg modules.Config) agent.Provider {
		return agy.New(agy.Config{
			Bin:    cfg["bin"],
			Token:  cfg["oauth_token"],
			Models: modelsWith(cfg["model"], agy.DefaultModels()),
		})
	}
	m.init()
	return m
}

func (m *Module) init() {
	m.LookPath = exec.LookPath
	m.Version = runVersion
	if m.reg == nil {
		m.reg = agent.NewRegistry()
	}
	if p, err := m.reg.Get(m.provider()); err == nil {
		m.fallback = p
	} else {
		m.fallback = m.build(modules.Config{})
	}
}

// provider is the registry key the module manages.
func (m *Module) provider() string { return strings.TrimPrefix(m.key, "agent-") }

// Key implements modules.Module.
func (m *Module) Key() string { return m.key }

// Name implements modules.Module.
func (m *Module) Name() string { return m.name }

// Description implements modules.Module.
func (m *Module) Description() string { return m.desc }

// Needs implements modules.Module.
func (m *Module) Needs() []modules.Need { return append([]modules.Need(nil), m.needs...) }

// Choices implements modules.Chooser: the default model is picked from
// the agent's models, not typed from memory.
func (m *Module) Choices(key string) ([]string, map[string]string) {
	if key != "model" || m.models == nil {
		return nil, nil
	}
	return m.models(), m.labels
}

// binary resolves what would run: the configured path, the managed copy,
// or the default name through PATH.
func (m *Module) binary(cfg modules.Config) (string, error) {
	bin := strings.TrimSpace(cfg["bin"])
	if bin == "" {
		if m.Deps != nil {
			if p, _, err := m.Deps.Find(m.bin); err == nil {
				return p, nil
			}
		}
		bin = m.bin
	}
	if strings.ContainsRune(bin, os.PathSeparator) {
		info, err := os.Stat(bin)
		if err != nil {
			return "", err
		}
		if info.IsDir() || info.Mode().Perm()&0o111 == 0 {
			return "", fmt.Errorf("%s is not an executable file", bin)
		}
		return bin, nil
	}
	return m.LookPath(bin)
}

// extraPath is what a run of the CLI needs in PATH beyond the
// supervisor's: the managed Node, so the CLI's shebang finds it.
func (m *Module) extraPath() string {
	if m.Deps == nil {
		return ""
	}
	return m.Deps.Path()
}

// Verify implements modules.Module: the binary answers --version (or is
// about to be installed), and which credential a turn would use is said
// out loud rather than probed, because probing would spend a turn.
func (m *Module) Verify(ctx context.Context, cfg modules.Config) []modules.Line {
	var lines []modules.Line
	path, err := m.binary(cfg)
	switch {
	case err != nil && strings.TrimSpace(cfg["bin"]) == "" && m.Deps != nil:
		lines = append(lines, modules.Skip(m.bin+" is not installed yet: enabling installs it under modules/node"))
	case err != nil:
		lines = append(lines, modules.Fail(fmt.Sprintf("%s: %v", m.bin, err)))
	default:
		vctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		v, err := m.Version(vctx, path)
		cancel()
		if err != nil {
			lines = append(lines, modules.Fail(fmt.Sprintf("%s --version: %v", path, err)))
		} else {
			lines = append(lines, modules.OK(fmt.Sprintf("%s answers --version: %s", path, v)))
		}
	}
	s := sessionOf(cfg, m.secretKey)
	switch s.Kind {
	case "api_key":
		lines = append(lines, modules.Skip("an API key is set and is handed to the CLI; it is not probed here, the first turn says if it is wrong"))
		if cfg[m.secretKey] != "" {
			lines = append(lines, modules.Skip("a signed-in session is stored too; the API key wins, forget it to use the session"))
		}
	case "subscription":
		lines = append(lines, modules.Skip("signed in with your subscription since "+s.Since+"; not probed here, the first turn says if it still works"))
	default:
		lines = append(lines, modules.Skip("no session and no API key: the CLI's own login under each site's home is used. Sign in from the panel to use your subscription"))
	}
	if model := strings.TrimSpace(cfg["model"]); model != "" {
		lines = append(lines, modules.OK(model+" will be the default model"))
	}
	return lines
}

// Install implements modules.Module: when no binary is configured and
// none is found, the CLI is installed under modules/node, Node included
// if the machine has none that will do.
func (m *Module) Install(ctx context.Context, cfg modules.Config, progress func(string)) error {
	if progress == nil {
		progress = func(string) {}
	}
	if path, err := m.binary(cfg); err == nil {
		progress("using " + path)
		return nil
	} else if strings.TrimSpace(cfg["bin"]) != "" || m.Deps == nil {
		return err
	}
	if err := m.Deps.Install(ctx, m.bin, progress); err != nil {
		return err
	}
	path, err := m.binary(cfg)
	if err != nil {
		return err
	}
	progress("using " + path)
	return nil
}

// Enable implements modules.Module: the provider the conversations use is
// replaced by one built from cfg. The managed binary is named by path so
// the provider finds it without PATH.
func (m *Module) Enable(ctx context.Context, cfg modules.Config) error {
	cfg = withBin(cfg, m)
	m.reg.Register(m.build(cfg))
	return nil
}

func withBin(cfg modules.Config, m *Module) modules.Config {
	out := modules.Config{}
	for k, v := range cfg {
		out[k] = v
	}
	if strings.TrimSpace(out["bin"]) == "" {
		if p, err := m.binary(cfg); err == nil {
			out["bin"] = p
		}
	}
	return out
}

// Disable implements modules.Module: the default provider is put back.
func (m *Module) Disable(ctx context.Context) error {
	if m.removeOnDisable {
		m.reg.Remove(m.provider())
		return nil
	}
	m.reg.Register(m.fallback)
	return nil
}

// Session says what a turn would authenticate with, worked out from the
// config without spending anything.
type Session struct {
	// Kind is "subscription", "api_key" or "none".
	Kind  string `json:"kind"`
	Since string `json:"since,omitempty"`
}

func sessionOf(cfg modules.Config, secretKey string) Session {
	switch {
	case strings.TrimSpace(cfg["api_key"]) != "":
		return Session{Kind: "api_key"}
	case strings.TrimSpace(cfg[secretKey]) != "":
		return Session{Kind: "subscription", Since: cfg["oauth_since"]}
	}
	return Session{Kind: "none"}
}

func (m *Module) config() modules.Config {
	if m.Store == nil {
		return modules.Config{}
	}
	cfg, err := m.Store.Config(m.key)
	if err != nil {
		return modules.Config{}
	}
	return cfg
}

// Session reads the stored config and reports it.
func (m *Module) Session() Session {
	if m.Store == nil {
		return Session{Kind: "none"}
	}
	cfg, err := m.Store.Config(m.key)
	if err != nil {
		return Session{Kind: "none"}
	}
	return sessionOf(cfg, m.secretKey)
}

// ---------- sign in ----------

// StartLogin runs the vendor's sign-in on a terminal of ours. One at a
// time: a second would fight the first for the account.
func (m *Module) StartLogin(ctx context.Context) (*Login, error) {
	m.mu.Lock()
	if m.login != nil && m.login.Live() {
		l := m.login
		m.mu.Unlock()
		return l, nil
	}
	m.mu.Unlock()
	cfg := modules.Config{}
	if m.Store != nil {
		if c, err := m.Store.Config(m.key); err == nil {
			cfg = c
		}
	}
	path, err := m.binary(cfg)
	if err != nil {
		if m.Deps == nil || strings.TrimSpace(cfg["bin"]) != "" {
			return nil, fmt.Errorf("%s is not installed: %w", m.bin, err)
		}
		// Signing in before enabling: the CLI is what signs in, so it is
		// installed now rather than refusing and asking for a click.
		if err := m.Deps.Install(ctx, m.bin, nil); err != nil {
			return nil, err
		}
		if path, err = m.binary(cfg); err != nil {
			return nil, err
		}
	}
	l, err := startLogin(m.loginSpec(path), path, m.extraPath())
	if l != nil {
		m.mu.Lock()
		m.login = l
		m.mu.Unlock()
	}
	return l, err
}

// CurrentLogin is the attempt in flight, if any.
func (m *Module) CurrentLogin() *Login {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.login
}

// CancelLogin throws away the attempt and forgets it.
func (m *Module) CancelLogin() {
	m.mu.Lock()
	l := m.login
	m.login = nil
	m.mu.Unlock()
	if l != nil {
		l.Cancel()
	}
}

// FinishLogin stores the session and enables the module if it was not:
// signing in is the operator saying "use this", and a module that then
// needed a second click to turn on was the first version's complaint.
func (m *Module) FinishLogin(ctx context.Context, l *Login) error {
	if l == nil {
		return errors.New("no sign-in in flight")
	}
	secret := l.Secret()
	if secret == "" {
		return errors.New("that sign-in did not finish")
	}
	if m.Store == nil {
		return errors.New("no store to keep the session in")
	}
	patch := modules.Config{m.secretKey: secret, "oauth_since": time.Now().UTC().Format(time.RFC3339)}
	if err := m.Store.Patch(ctx, m.key, patch); err != nil {
		return err
	}
	if !m.Store.Enabled(m.key) {
		if _, err := m.Store.Enable(ctx, m.key, nil, nil); err != nil {
			return err
		}
	}
	m.mu.Lock()
	if m.login == l {
		m.login = nil
	}
	m.mu.Unlock()
	return nil
}

// loginState is the one shape the login handlers answer with.
func (m *Module) loginState(l *Login) map[string]any {
	out := map[string]any{"state": "", "session": m.Session(), "provider": m.provider()}
	if l == nil {
		return out
	}
	state, url, fail := l.State()
	out["state"] = state
	if url != "" {
		out["url"] = url
	}
	if c := l.UserCode(); c != "" {
		out["device_code"] = c
	}
	if fail != "" {
		out["error"] = fail
	}
	// What the program has on screen, when what matters is why it does
	// not move on. Not in authorize, where only the link matters.
	if state == LoginStarting || state == LoginChecking || state == LoginFailed {
		if tail := l.Tail(6); tail != "" {
			out["tail"] = tail
		}
	}
	return out
}

// Routes implements modules.Module: the sign-in, under
// /api/v1/modules/<key>/login.
func (m *Module) Routes() map[string]http.Handler {
	return map[string]http.Handler{
		"GET /{$}":          http.HandlerFunc(m.handleStatus),
		"GET /login":        http.HandlerFunc(m.handleLoginState),
		"POST /login":       http.HandlerFunc(m.handleLoginStart),
		"POST /login/code":  http.HandlerFunc(m.handleLoginCode),
		"DELETE /login":     http.HandlerFunc(m.handleLoginCancel),
		"GET /session":      http.HandlerFunc(m.handleStatus),
		"GET /usage":        http.HandlerFunc(m.handleUsage),
		"POST /binary/sync": http.HandlerFunc(m.handleBinarySync),
	}
}

// SetupRoutes implements modules.Setup: the sign-in and the binary are
// reachable before the module is enabled, because they are how it gets
// enabled.
func (m *Module) SetupRoutes() []string {
	return []string{"GET /login", "POST /login", "POST /login/code", "DELETE /login", "GET /session", "POST /binary/sync"}
}

func (m *Module) handleStatus(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"session": m.Session(), "provider": m.provider()}
	if m.Deps != nil {
		for _, s := range m.Deps.Statuses() {
			if s.Name == m.bin {
				out["binary"] = s
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleLoginStart runs the program and answers with where to authorize.
func (m *Module) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	l, err := m.StartLogin(ctx)
	if err != nil {
		out := map[string]any{"error": err.Error()}
		if l != nil {
			for k, v := range m.loginState(l) {
				out[k] = v
			}
		}
		writeJSON(w, http.StatusBadGateway, out)
		return
	}
	writeJSON(w, http.StatusOK, m.loginState(l))
}

// handleLoginState is the poll: it also settles a finished attempt, so
// the session is stored by whichever request sees it first.
func (m *Module) handleLoginState(w http.ResponseWriter, r *http.Request) {
	l := m.CurrentLogin()
	if l == nil {
		writeJSON(w, http.StatusOK, m.loginState(nil))
		return
	}
	state, _, _ := l.State()
	if state == LoginDone {
		if err := m.FinishLogin(r.Context(), l); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"state": LoginFailed, "error": err.Error()})
			return
		}
		out := m.loginState(nil)
		out["state"] = LoginDone
		writeJSON(w, http.StatusOK, out)
		return
	}
	out := m.loginState(l)
	if state == LoginFailed {
		// A failed attempt is reported once and forgotten, so the next
		// start is clean.
		m.CancelLogin()
	}
	writeJSON(w, http.StatusOK, out)
}

func (m *Module) handleLoginCode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be {\"code\": \"...\"}"})
		return
	}
	l := m.CurrentLogin()
	if l == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "no sign-in in flight; start one first"})
		return
	}
	if err := l.SendCode(body.Code); err != nil {
		out := m.loginState(l)
		out["error"] = err.Error()
		writeJSON(w, http.StatusBadRequest, out)
		return
	}
	writeJSON(w, http.StatusOK, m.loginState(l))
}

func (m *Module) handleLoginCancel(w http.ResponseWriter, r *http.Request) {
	m.CancelLogin()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleBinarySync installs or updates the CLI now, without waiting for
// the daily check.
func (m *Module) handleBinarySync(w http.ResponseWriter, r *http.Request) {
	if m.Deps == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this installation does not manage binaries"})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	var progress []string
	if err := m.Deps.Install(ctx, m.bin, func(s string) { progress = append(progress, s) }); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "progress": progress})
		return
	}
	// The provider is rebuilt so a path that changed is the one that runs.
	if m.Store != nil && m.Store.Enabled(m.key) {
		if cfg, err := m.Store.Config(m.key); err == nil {
			_ = m.Enable(ctx, cfg)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "progress": progress})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// Restore re-applies every enabled module at start. The manager keeps the
// config and the enabled flag across restarts but calls nobody; without
// this the panel would say "enabled" over a registry still holding the
// defaults, which is the silent failure the modules package warns about.
func Restore(ctx context.Context, mgr *modules.Manager, mods ...Restorable) error {
	var errs []error
	for _, m := range mods {
		m.useStore(mgr)
		if !mgr.Enabled(m.Key()) {
			continue
		}
		cfg, err := mgr.Config(m.Key())
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", m.Key(), err))
			continue
		}
		if err := m.Enable(ctx, cfg); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", m.Key(), err))
		}
	}
	return errors.Join(errs...)
}

// Restorable is an agent module Restore can bring back.
type Restorable interface {
	Key() string
	Enable(ctx context.Context, cfg modules.Config) error
	useStore(mgr *modules.Manager)
}

func (m *Module) useStore(mgr *modules.Manager) {
	if m.Store == nil {
		m.Store = mgr
	}
}

func modelsWith(first string, defaults []string) []string {
	first = strings.TrimSpace(first)
	if first == "" {
		return defaults
	}
	out := []string{first}
	for _, d := range defaults {
		if d != first {
			out = append(out, d)
		}
	}
	return out
}

func runVersion(ctx context.Context, bin string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, "--version")
	// The CLIs read their config from HOME; a version probe must not
	// create one for the service user as a side effect. PATH carries the
	// managed Node when the binary is the managed one.
	path := os.Getenv("PATH")
	if dir := filepath.Dir(bin); strings.HasSuffix(filepath.Dir(dir), "node") {
		path = filepath.Join(filepath.Dir(filepath.Dir(dir)), "node-runtime", "bin") + string(os.PathListSeparator) + path
	}
	cmd.Env = append(os.Environ(), "HOME="+os.TempDir(), "PATH="+path)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		text := strings.TrimSpace(out.String())
		if text != "" {
			return "", fmt.Errorf("%v: %s", err, text)
		}
		return "", err
	}
	v := strings.TrimSpace(out.String())
	if i := strings.IndexByte(v, '\n'); i >= 0 {
		v = v[:i]
	}
	if v == "" {
		v = "(no output)"
	}
	return v, nil
}

var _ modules.Module = (*Module)(nil)
