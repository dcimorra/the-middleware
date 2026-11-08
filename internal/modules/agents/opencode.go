package agents

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/agent"
	"github.com/dcimorra/the-middleware/internal/agent/opencode"
	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/modules"
)

// OpenCodeKey is the OpenCode module.
const OpenCodeKey = "agent-opencode"

// OpenCode is the agent-opencode module. OpenCode speaks to many vendors,
// so instead of one sign-in it keeps one key per provider, chosen from the
// CLI's own catalog, and the models a turn can use are the ones the CLI
// lists with those keys: connecting a provider is what makes its models
// appear in the chat.
type OpenCode struct {
	*Module
	// Home is a HOME of the module's own (data/agents/opencode) where the
	// CLI is asked for the catalog and the models; the turns run under the
	// site's HOME, never this one.
	Home string

	rmu      sync.Mutex
	cmu      sync.Mutex
	catalog  []CatalogProvider
	catAt    time.Time
	models   []string
	modelsAt time.Time
	modErr   string
	busy     bool
	// offered are the connected providers' models, as of the last build,
	// under a lock of their own: Needs reads them, and Needs is reached
	// from inside cmu (Session asks the store, which asks Needs).
	omu     sync.Mutex
	offered []string
}

// CatalogProvider is one provider OpenCode can talk to.
type CatalogProvider struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Env    []string `json:"env,omitempty"`
	Models int      `json:"models"`
	Doc    string   `json:"doc,omitempty"`
}

var providerID = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// NewOpenCode builds the module over reg, with its own HOME under home.
func NewOpenCode(reg *agent.Registry, home string) *OpenCode {
	o := &OpenCode{Home: home}
	m := &Module{
		key:  OpenCodeKey,
		name: "OpenCode",
		desc: "Conversations run by OpenCode, with whichever providers you connect here (OpenAI, OpenRouter, Anthropic, Groq, DeepSeek…): the chat lists the models of the ones connected.",
		bin:  "opencode",
		needs: []modules.Need{
			{Key: "model", Label: "Default model", Optional: true, Help: "Offered first in the chat, from the providers connected. Empty: the first one listed."},
			{Key: "bin", Label: "Binary", Optional: true, Help: "Path to the opencode executable. Empty: the one this installation keeps under modules/node, installed and updated for you.", Local: true},
			{Key: "providers", Secret: true, Optional: true, Hidden: true},
		},
		reg:       reg,
		secretKey: "providers",
	}
	// The models of the providers connected when the agent was last built
	// (every key added or removed builds it again). Not read from the
	// store here: Needs is called with the manager's lock held, and asking
	// it for the config from inside was a deadlock.
	m.models = func() []string {
		o.omu.Lock()
		defer o.omu.Unlock()
		return append([]string(nil), o.offered...)
	}
	m.build = func(cfg modules.Config) agent.Provider {
		connected := o.connectedModels(o.authOf(cfg))
		o.omu.Lock()
		o.offered = connected
		o.omu.Unlock()
		return opencode.New(opencode.Config{
			Key:    o.provider(),
			Bin:    cfg["bin"],
			Auth:   o.authOf(cfg),
			Models: modelsWith(cfg["model"], connected),
		})
	}
	o.Module = m
	m.init()
	o.loadCachedModels()
	return o
}

// authOf is the keys a config holds, one per provider.
func (o *OpenCode) authOf(cfg modules.Config) map[string]string {
	return parseAuth(cfg["providers"])
}

func parseAuth(raw string) map[string]string {
	out := map[string]string{}
	if strings.TrimSpace(raw) != "" {
		_ = json.Unmarshal([]byte(raw), &out)
	}
	return out
}

func (o *OpenCode) config() modules.Config {
	if o.Store == nil {
		return modules.Config{}
	}
	cfg, err := o.Store.Config(o.key)
	if err != nil {
		return modules.Config{}
	}
	return cfg
}

func (o *OpenCode) modelsPath() string { return filepath.Join(o.Home, "models.txt") }

func (o *OpenCode) loadCachedModels() {
	raw, err := os.ReadFile(o.modelsPath())
	if err != nil {
		return
	}
	o.cmu.Lock()
	o.models = splitModels(string(raw))
	o.cmu.Unlock()
}

// connectedModels are the listed models of the providers with a key: the
// CLI also lists its own free tier, which only its own app may use.
func (o *OpenCode) connectedModels(auth map[string]string) []string {
	o.cmu.Lock()
	defer o.cmu.Unlock()
	var out []string
	for _, m := range o.models {
		p, _, _ := strings.Cut(m, "/")
		if _, ok := auth[p]; ok {
			out = append(out, m)
		}
	}
	return out
}

var modelLine = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*/\S+$`)

func splitModels(text string) []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); modelLine.MatchString(l) {
			out = append(out, l)
		}
	}
	return out
}

// RefreshModels asks the CLI which models the connected providers offer,
// and rebuilds the provider so the chat lists them. The CLI also fills its
// catalog cache under Home on the way.
func (o *OpenCode) RefreshModels(ctx context.Context) error {
	// One reading at a time; a second waits and reads again, because it
	// was asked for after something changed (a key just stored).
	o.rmu.Lock()
	defer o.rmu.Unlock()
	o.cmu.Lock()
	o.busy = true
	o.cmu.Unlock()
	defer func() {
		o.cmu.Lock()
		o.busy = false
		o.cmu.Unlock()
	}()
	cfg := o.config()
	bin, err := o.binary(cfg)
	if err == nil {
		err = os.MkdirAll(o.Home, 0o700)
	}
	if err == nil {
		err = opencode.WriteAuth(o.Home, o.authOf(cfg))
	}
	var out bytes.Buffer
	if err == nil {
		cctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		// --refresh: without it the CLI lists from the catalog it was
		// built with, whose names a turn (which fetches the current one)
		// no longer accepts: deepseek-chat was listed and then "Model not
		// found" on 22 October 2026.
		cmd := exec.CommandContext(cctx, bin, "models", "--refresh")
		path := os.Getenv("PATH")
		if extra := o.extraPath(); extra != "" {
			path = extra + string(os.PathListSeparator) + path
		}
		// A clean environment, as the turns get: an XDG variable of the
		// service's would put the keys and the catalog somewhere else.
		cmd.Env = []string{"HOME=" + o.Home, "PATH=" + path, "LANG=C.UTF-8", "OPENCODE_DISABLE_AUTOUPDATE=1"}
		cmd.Dir = o.Home
		var errBuf bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errBuf
		if runErr := cmd.Run(); runErr != nil {
			err = fmt.Errorf("%s models: %v %s", bin, runErr, strings.TrimSpace(lastLines(errBuf.String(), 3)))
		}
	}
	o.cmu.Lock()
	o.modelsAt = time.Now()
	if err != nil {
		o.modErr = err.Error()
		o.cmu.Unlock()
		return err
	}
	o.modErr = ""
	o.models = splitModels(out.String())
	o.catAt = time.Time{}
	o.cmu.Unlock()
	_ = fsx.WriteAtomic(o.modelsPath(), []byte(strings.Join(o.models, "\n")+"\n"), 0o600)
	if o.Store != nil && o.Store.Enabled(o.key) {
		_ = o.Enable(ctx, cfg)
	}
	return nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

// Catalog is every provider the CLI knows, from the cache it keeps under
// Home (filled by the first models listing).
func (o *OpenCode) Catalog() []CatalogProvider {
	path := filepath.Join(o.Home, ".cache", "opencode", "models.json")
	st, err := os.Stat(path)
	if err != nil {
		return []CatalogProvider{}
	}
	o.cmu.Lock()
	if o.catalog != nil && !st.ModTime().After(o.catAt) {
		out := o.catalog
		o.cmu.Unlock()
		return out
	}
	o.cmu.Unlock()
	raw, err := os.ReadFile(path)
	if err != nil {
		return []CatalogProvider{}
	}
	var doc map[string]struct {
		Name   string          `json:"name"`
		Env    []string        `json:"env"`
		Doc    string          `json:"doc"`
		Models json.RawMessage `json:"models"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return []CatalogProvider{}
	}
	out := make([]CatalogProvider, 0, len(doc))
	for id, p := range doc {
		var models map[string]json.RawMessage
		_ = json.Unmarshal(p.Models, &models)
		name := p.Name
		if name == "" {
			name = id
		}
		out = append(out, CatalogProvider{ID: id, Name: name, Env: p.Env, Doc: p.Doc, Models: len(models)})
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	o.cmu.Lock()
	o.catalog, o.catAt = out, st.ModTime()
	o.cmu.Unlock()
	return out
}

// Session implements what the panel shows: connected means some provider
// has a key.
func (o *OpenCode) Session() Session {
	if len(o.authOf(o.config())) > 0 {
		return Session{Kind: "api_key"}
	}
	return Session{Kind: "none"}
}

// Verify implements modules.Module.
func (o *OpenCode) Verify(ctx context.Context, cfg modules.Config) []modules.Line {
	var lines []modules.Line
	path, err := o.binary(cfg)
	switch {
	case err != nil && strings.TrimSpace(cfg["bin"]) == "" && o.Deps != nil:
		lines = append(lines, modules.Skip("opencode is not installed yet: enabling installs it under modules/node"))
	case err != nil:
		lines = append(lines, modules.Fail(fmt.Sprintf("opencode: %v", err)))
	default:
		vctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		v, err := o.Version(vctx, path)
		cancel()
		if err != nil {
			lines = append(lines, modules.Fail(fmt.Sprintf("%s --version: %v", path, err)))
		} else {
			lines = append(lines, modules.OK(fmt.Sprintf("%s answers --version: %s", path, v)))
		}
	}
	auth := o.authOf(cfg)
	if len(auth) == 0 {
		lines = append(lines, modules.Skip("no provider connected yet: connect one below, its models appear in the chat"))
	} else {
		ids := make([]string, 0, len(auth))
		for id := range auth {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		lines = append(lines, modules.OK(fmt.Sprintf("%d provider(s) connected: %s; %d model(s) listed", len(auth), strings.Join(ids, ", "), len(o.connectedModels(auth)))))
	}
	return lines
}

// Routes implements modules.Module.
func (o *OpenCode) Routes() map[string]http.Handler {
	return map[string]http.Handler{
		"GET /{$}":               http.HandlerFunc(o.handleProviders),
		"GET /providers":         http.HandlerFunc(o.handleProviders),
		"PUT /providers/{id}":    http.HandlerFunc(o.handleConnect),
		"DELETE /providers/{id}": http.HandlerFunc(o.handleDisconnect),
		"POST /models/refresh":   http.HandlerFunc(o.handleRefresh),
		"POST /binary/sync":      http.HandlerFunc(o.handleBinarySync),
		"GET /session":           http.HandlerFunc(o.handleProviders),
	}
}

// SetupRoutes implements modules.Setup: providers are connected before
// the module is turned on, which is when it has anything to run with.
func (o *OpenCode) SetupRoutes() []string {
	return []string{"GET /providers", "PUT /providers/{id}", "DELETE /providers/{id}", "POST /models/refresh", "POST /binary/sync", "GET /session"}
}

type connected struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Models int    `json:"models"`
}

func (o *OpenCode) handleProviders(w http.ResponseWriter, r *http.Request) {
	cfg := o.config()
	auth := o.authOf(cfg)
	cat := o.Catalog()
	names := map[string]string{}
	for _, c := range cat {
		names[c.ID] = c.Name
	}
	models := o.connectedModels(auth)
	count := map[string]int{}
	for _, m := range models {
		p, _, _ := strings.Cut(m, "/")
		count[p]++
	}
	conn := []connected{}
	for id := range auth {
		n := names[id]
		if n == "" {
			n = id
		}
		conn = append(conn, connected{ID: id, Name: n, Models: count[id]})
	}
	sort.Slice(conn, func(i, j int) bool { return conn[i].Name < conn[j].Name })
	o.cmu.Lock()
	out := map[string]any{
		"provider": o.provider(), "session": o.Session(), "catalog": cat, "connected": conn, "models": modelsWith(cfg["model"], models),
		"default_model": strings.TrimSpace(cfg["model"]), "models_error": o.modErr, "reading": o.busy,
	}
	if !o.modelsAt.IsZero() {
		out["models_at"] = o.modelsAt
	}
	o.cmu.Unlock()
	if o.Deps != nil {
		for _, s := range o.Deps.Statuses() {
			if s.Name == o.bin {
				out["binary"] = s
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (o *OpenCode) setAuth(ctx context.Context, change func(map[string]string)) error {
	if o.Store == nil {
		return errors.New("no store to keep the keys in")
	}
	// Reading from here on: storing the key redraws the panel, which asks
	// before the reading has started and must be told to ask again.
	o.cmu.Lock()
	o.busy = true
	o.cmu.Unlock()
	auth := o.authOf(o.config())
	change(auth)
	raw, err := json.Marshal(auth)
	if err != nil {
		return err
	}
	return o.Store.Patch(ctx, o.key, modules.Config{"providers": string(raw)})
}

// handleConnect stores one provider's key and reads the models again: the
// list the chat offers is what the CLI says with the keys it has.
func (o *OpenCode) handleConnect(w http.ResponseWriter, r *http.Request) {
	id := strings.ToLower(strings.TrimSpace(r.PathValue("id")))
	if !providerID.MatchString(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "not a provider id"})
		return
	}
	if cat := o.Catalog(); len(cat) > 0 {
		known := false
		for _, c := range cat {
			if c.ID == id {
				known = true
			}
		}
		if !known {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": id + " is not in OpenCode's catalog"})
			return
		}
	}
	var body struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil || strings.TrimSpace(body.Key) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be {\"key\": \"...\"}"})
		return
	}
	if err := o.setAuth(r.Context(), func(a map[string]string) { a[id] = strings.TrimSpace(body.Key) }); err != nil {
		o.notBusy()
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	o.refreshAfter(w, r)
}

func (o *OpenCode) handleDisconnect(w http.ResponseWriter, r *http.Request) {
	id := strings.ToLower(strings.TrimSpace(r.PathValue("id")))
	if err := o.setAuth(r.Context(), func(a map[string]string) { delete(a, id) }); err != nil {
		o.notBusy()
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	o.refreshAfter(w, r)
}

func (o *OpenCode) notBusy() {
	o.cmu.Lock()
	o.busy = false
	o.cmu.Unlock()
}

func (o *OpenCode) handleRefresh(w http.ResponseWriter, r *http.Request) { o.refreshAfter(w, r) }

// refreshAfter reads the models (installing the CLI first if this is the
// first provider, since the CLI is what reads them) and answers with the
// state.
func (o *OpenCode) refreshAfter(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	if _, err := o.binary(o.config()); err != nil && o.Deps != nil && strings.TrimSpace(o.config()["bin"]) == "" {
		if err := o.Deps.Install(ctx, o.bin, nil); err != nil {
			o.notBusy()
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "installing opencode: " + err.Error()})
			return
		}
	}
	_ = o.RefreshModels(ctx)
	o.handleProviders(w, r)
}

// Enable implements modules.Module: the provider is registered at once
// with the models read last time, and they are read again in the
// background (a new release of the CLI, a provider's new model).
func (o *OpenCode) Enable(ctx context.Context, cfg modules.Config) error {
	if err := o.Module.Enable(ctx, cfg); err != nil {
		return err
	}
	o.cmu.Lock()
	stale := time.Since(o.modelsAt) > 12*time.Hour && !o.busy
	o.cmu.Unlock()
	if stale && len(o.authOf(cfg)) > 0 {
		go func() {
			rctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			_ = o.RefreshModels(rctx)
		}()
	}
	return nil
}

var _ modules.Module = (*OpenCode)(nil)
