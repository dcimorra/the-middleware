package modules

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/secrets"
)

// Manager holds the registered modules and the store.
type Manager struct {
	root  string
	bus   *events.Bus
	mu    sync.Mutex
	mods  map[string]Module
	order []string
	st    *store
}

// ErrUnknown says no module has that key.
var ErrUnknown = errors.New("unknown module")

// ErrVerifyFailed is returned by Enable when a line failed; the lines say
// which one.
var ErrVerifyFailed = errors.New("verification failed")

// New opens the store under root. bus may be nil (events are dropped).
func New(root string, bus *events.Bus) (*Manager, error) {
	box, err := secrets.Open(root)
	if err != nil {
		return nil, err
	}
	if bus == nil {
		bus = events.New("")
	}
	m := &Manager{root: root, bus: bus, mods: map[string]Module{}, st: newStore(root, box)}
	if err := m.st.reload(); err != nil {
		return nil, err
	}
	return m, nil
}

// Root is the installation root.
func (m *Manager) Root() string { return m.root }

// BinDir is where modules put the binaries they install.
func (m *Manager) BinDir() string { return filepath.Join(m.root, "modules", "bin") }

// Register adds a module. A duplicate or invalid key is a programming
// error, so it panics like http.Handle does.
func (m *Manager) Register(mod Module) {
	key := mod.Key()
	if !ValidKey(key) {
		panic(fmt.Sprintf("modules: %q is not a valid key", key))
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.mods[key]; dup {
		panic(fmt.Sprintf("modules: %q registered twice", key))
	}
	m.mods[key] = mod
	m.order = append(m.order, key)
}

// Get returns a registered module.
func (m *Manager) Get(key string) (Module, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mod, ok := m.mods[key]
	return mod, ok
}

// Reload re-reads the store if it changed on disk. Every read below calls
// it, so the CLI writing the file is enough for the running process to
// see it on its next look; the explicit method exists for callers that
// want the error.
func (m *Manager) Reload() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.st.reload()
}

// List returns every registered module with its state, in registration
// order.
func (m *Manager) List() []Info {
	out, mods := m.list()
	for i, mod := range mods {
		c, ok := mod.(Chooser)
		if !ok {
			continue
		}
		for j := range out[i].Needs {
			out[i].Needs[j].Options, out[i].Needs[j].Labels = c.Choices(out[i].Needs[j].Key)
		}
	}
	return out
}

func (m *Manager) list() ([]Info, []Module) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_ = m.st.reload()
	out := make([]Info, 0, len(m.order))
	mods := make([]Module, 0, len(m.order))
	for _, key := range m.order {
		mod := m.mods[key]
		needs := mod.Needs()
		if needs == nil {
			needs = []Need{}
		}
		stored, configured := m.st.data.Config[key]
		needs = append([]Need(nil), needs...)
		for i, n := range needs {
			if !n.Secret && !n.Hidden {
				needs[i].Value = stored[n.Key]
			}
		}
		out = append(out, Info{
			Key:         key,
			Name:        mod.Name(),
			Description: mod.Description(),
			Enabled:     m.st.data.Enabled[key],
			Installed:   m.installed(mod),
			Configured:  configured,
			Shareable:   configured && !nodeBound(mod),
			Needs:       needs,
		})
		mods = append(mods, mod)
	}
	return out, mods
}

// Enabled reports whether key is on.
func (m *Manager) Enabled(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_ = m.st.reload()
	return m.st.data.Enabled[key]
}

// Installed reports whether every binary the module declares exists and
// is executable under modules/bin. A module that declares none is
// installed by definition.
func (m *Manager) Installed(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	mod, ok := m.mods[key]
	return ok && m.installed(mod)
}

func (m *Manager) installed(mod Module) bool {
	var names []string
	if bf, ok := mod.(BinariesFor); ok {
		cfg, err := m.st.decrypt(mod.Needs(), m.st.data.Config[mod.Key()])
		if err != nil {
			cfg = Config{}
		}
		names = bf.BinariesFor(cfg)
	} else if b, ok := mod.(Binaries); ok {
		names = b.Binaries()
	}
	for _, name := range names {
		info, err := os.Stat(filepath.Join(m.BinDir(), name))
		if err != nil || info.IsDir() || info.Mode().Perm()&0o100 == 0 {
			return false
		}
	}
	return true
}

// Config returns the stored config for key, decrypted, for the module's
// own use. Missing config is an empty, non-nil map.
func (m *Manager) Config(key string) (Config, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mod, ok := m.mods[key]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknown, key)
	}
	if err := m.st.reload(); err != nil {
		return nil, err
	}
	return m.st.decrypt(mod.Needs(), m.st.data.Config[key])
}

// Verify merges cfg over what is stored and runs the module's probes. It
// writes nothing.
func (m *Manager) Verify(ctx context.Context, key string, cfg Config) ([]Line, error) {
	mod, merged, err := m.prepare(key, cfg)
	if err != nil {
		return nil, err
	}
	return mod.Verify(ctx, merged), nil
}

// Enable verifies, installs, enables and only then saves. A failed line
// stops everything before any write: the store never holds credentials
// that were not seen working. progress may be nil.
func (m *Manager) Enable(ctx context.Context, key string, cfg Config, progress func(string)) ([]Line, error) {
	if progress == nil {
		progress = func(string) {}
	}
	mod, merged, err := m.prepare(key, cfg)
	if err != nil {
		return nil, err
	}
	lines := mod.Verify(ctx, merged)
	if Failed(lines) {
		m.bus.Emit(events.Event{Type: "module.verify_failed", Subject: key, Actor: "operator", Level: "warn", Message: firstFailure(lines), Data: map[string]any{"lines": lines}})
		return lines, ErrVerifyFailed
	}
	if err := mod.Install(ctx, merged, progress); err != nil {
		return lines, fmt.Errorf("install: %w", err)
	}
	if err := mod.Enable(ctx, merged); err != nil {
		return lines, fmt.Errorf("enable: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.st.reload(); err != nil {
		return lines, err
	}
	sealed, err := m.st.encrypt(mod.Needs(), merged)
	if err != nil {
		return lines, err
	}
	m.st.data.Config[key] = sealed
	m.st.data.Enabled[key] = true
	if err := m.st.save(); err != nil {
		return lines, err
	}
	m.bus.Emit(events.Event{Type: "module.enabled", Subject: key, Actor: "operator", Data: map[string]any{"lines": lines}})
	return lines, nil
}

// Patch merges values into the stored config without a verify: it is how
// a module writes what it obtained itself (a session from a sign-in), as
// opposed to what the operator typed. An enabled module is re-enabled
// with the result so the running provider carries the new value.
func (m *Manager) Patch(ctx context.Context, key string, values Config) error {
	m.mu.Lock()
	mod, ok := m.mods[key]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrUnknown, key)
	}
	needs := mod.Needs()
	known := map[string]bool{}
	for _, n := range needs {
		known[n.Key] = true
	}
	for k := range values {
		if !known[k] {
			m.mu.Unlock()
			return fmt.Errorf("%s: %q is not something this module asks for", key, k)
		}
	}
	if err := m.st.reload(); err != nil {
		m.mu.Unlock()
		return err
	}
	merged, err := m.st.decrypt(needs, m.st.data.Config[key])
	if err != nil {
		m.mu.Unlock()
		return err
	}
	for k, v := range values {
		merged[k] = v
	}
	sealed, err := m.st.encrypt(needs, merged)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	m.st.data.Config[key] = sealed
	if err := m.st.save(); err != nil {
		m.mu.Unlock()
		return err
	}
	enabled := m.st.data.Enabled[key]
	m.mu.Unlock()
	if enabled {
		if err := mod.Enable(ctx, merged); err != nil {
			return err
		}
	}
	m.bus.Emit(events.Event{Type: "module.updated", Subject: key, Actor: "operator"})
	return nil
}

// Disable turns the module off and keeps its credentials, so turning it
// back on is one click.
func (m *Manager) Disable(ctx context.Context, key string) error {
	mod, ok := m.Get(key)
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknown, key)
	}
	if err := mod.Disable(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.st.reload(); err != nil {
		return err
	}
	delete(m.st.data.Enabled, key)
	if err := m.st.save(); err != nil {
		return err
	}
	m.bus.Emit(events.Event{Type: "module.disabled", Subject: key, Actor: "operator"})
	return nil
}

// Forget disables the module and deletes its credentials.
func (m *Manager) Forget(ctx context.Context, key string) error {
	mod, ok := m.Get(key)
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknown, key)
	}
	m.mu.Lock()
	if err := m.st.reload(); err != nil {
		m.mu.Unlock()
		return err
	}
	wasEnabled := m.st.data.Enabled[key]
	m.mu.Unlock()
	if wasEnabled {
		if err := mod.Disable(ctx); err != nil {
			return err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.st.data.Enabled, key)
	delete(m.st.data.Config, key)
	if err := m.st.save(); err != nil {
		return err
	}
	if wasEnabled {
		m.bus.Emit(events.Event{Type: "module.disabled", Subject: key, Actor: "operator"})
	}
	m.bus.Emit(events.Event{Type: "module.forgotten", Subject: key, Actor: "operator", Message: "credentials deleted"})
	return nil
}

// Handler serves every module's routes: the API mounts it at
// /api/v1/modules/ with the prefix stripped, and a request to
// "github/hook" reaches the github module's "hook" route. Routes of a
// disabled module answer 404, so a webhook keeps being rejected the moment
// the operator turns the module off.
func (m *Manager) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		mod, ok := m.Get(key)
		if !ok || !m.Enabled(key) {
			http.NotFound(w, r)
			return
		}
		routes := mod.Routes()
		if routes == nil {
			http.NotFound(w, r)
			return
		}
		if h, ok := routes[strings.TrimSuffix(rest, "/")]; ok {
			h.ServeHTTP(w, r)
			return
		}
		// Routes written as ServeMux patterns ("GET /zones/{zone}/records"),
		// served the way the API serves them.
		mux := http.NewServeMux()
		for pattern, h := range routes {
			if strings.HasPrefix(pattern, "/") || strings.Contains(pattern, " ") {
				mux.Handle(pattern, h)
			}
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/" + rest
		r2.URL.RawPath = ""
		mux.ServeHTTP(w, r2)
	})
}

// prepare looks the module up, checks cfg only names declared needs, and
// fills what cfg leaves out from the store. The panel sends secrets back
// empty rather than echo them, so an empty secret means "keep the one you
// have", never "clear it"; Forget is how one clears.
func (m *Manager) prepare(key string, cfg Config) (Module, Config, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mod, ok := m.mods[key]
	if !ok {
		return nil, nil, fmt.Errorf("%w: %s", ErrUnknown, key)
	}
	needs := mod.Needs()
	known := map[string]bool{}
	for _, n := range needs {
		known[n.Key] = true
	}
	for k := range cfg {
		if !known[k] {
			return nil, nil, fmt.Errorf("%s: %q is not something this module asks for", key, k)
		}
	}
	if err := m.st.reload(); err != nil {
		return nil, nil, err
	}
	stored, err := m.st.decrypt(needs, m.st.data.Config[key])
	if err != nil {
		return nil, nil, err
	}
	merged := Config{}
	for k, v := range stored {
		merged[k] = v
	}
	secret := map[string]bool{}
	for _, n := range needs {
		secret[n.Key] = n.Secret
	}
	for k, v := range cfg {
		v = strings.TrimSpace(v)
		// Only a secret sent empty is kept: the panel never echoes one.
		// Any other field is shown with its value, so sent empty it was
		// emptied (a model list set back to "the first one listed").
		if v == "" && merged[k] != "" && secret[k] {
			continue
		}
		merged[k] = v
	}
	for _, n := range needs {
		if !n.Optional && merged[n.Key] == "" {
			return nil, nil, fmt.Errorf("%s: %s is required", key, n.Label)
		}
	}
	return mod, merged, nil
}

func firstFailure(lines []Line) string {
	for _, l := range lines {
		if l.Status == StatusFail {
			return l.Text
		}
	}
	return ""
}

// ErrNodeBound says the module's setup is this machine's alone.
var ErrNodeBound = errors.New("belongs to this machine and cannot be propagated")

// ErrNotConfigured says there is nothing stored to propagate.
var ErrNotConfigured = errors.New("is not set up on this node")

func nodeBound(mod Module) bool {
	b, ok := mod.(NodeBound)
	return ok && b.NodeBound()
}

// Shareable is what of key's stored config another node may take, in
// clear, for propagating over the mesh: every value but the Local ones
// and the empty ones. An empty value is left out rather than sent,
// because a field sent empty clears it at the far end (prepare), and a
// default model nobody chose here is no reason to undo one chosen there.
func (m *Manager) Shareable(key string) (Config, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mod, ok := m.mods[key]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknown, key)
	}
	if nodeBound(mod) {
		return nil, fmt.Errorf("%s %w", key, ErrNodeBound)
	}
	if err := m.st.reload(); err != nil {
		return nil, err
	}
	stored, configured := m.st.data.Config[key]
	if !configured {
		return nil, fmt.Errorf("%s %w", key, ErrNotConfigured)
	}
	needs := mod.Needs()
	cfg, err := m.st.decrypt(needs, stored)
	if err != nil {
		return nil, err
	}
	local := map[string]bool{}
	for _, n := range needs {
		if n.Local {
			local[n.Key] = true
		}
	}
	out := Config{}
	for k, v := range cfg {
		if v != "" && !local[k] {
			out[k] = v
		}
	}
	return out, nil
}

// Keys returns the registered keys, sorted, for the CLI's usage text.
func (m *Manager) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := append([]string(nil), m.order...)
	sort.Strings(out)
	return out
}
