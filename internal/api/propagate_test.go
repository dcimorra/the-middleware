package api

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dcimorra/the-middleware/internal/modules"
)

// keyModule asks for a secret token, a shared field and a field that is
// the machine's own, and refuses a token its node does not accept.
type keyModule struct {
	mu      sync.Mutex
	key     string
	refuse  string // a token this node's verify fails
	bound   bool
	enabled bool
}

func (m *keyModule) Key() string         { return m.key }
func (m *keyModule) Name() string        { return m.key }
func (m *keyModule) Description() string { return "keys" }
func (m *keyModule) Needs() []modules.Need {
	return []modules.Need{
		{Key: "token", Label: "Token", Secret: true},
		{Key: "account", Label: "Account", Optional: true},
		{Key: "tunnel", Label: "Tunnel", Optional: true, Local: true},
	}
}
func (m *keyModule) Verify(_ context.Context, cfg modules.Config) []modules.Line {
	if m.refuse != "" && cfg["token"] == m.refuse {
		return []modules.Line{modules.Fail("this token is not accepted here")}
	}
	return []modules.Line{modules.OK("token works")}
}
func (m *keyModule) Install(context.Context, modules.Config, func(string)) error { return nil }
func (m *keyModule) Enable(context.Context, modules.Config) error {
	m.mu.Lock()
	m.enabled = true
	m.mu.Unlock()
	return nil
}
func (m *keyModule) Disable(context.Context) error   { return nil }
func (m *keyModule) Routes() map[string]http.Handler { return nil }

type boundModule struct{ keyModule }

func (m *boundModule) NodeBound() bool { return true }

func nodeWithModules(t *testing.T, f *fixture, mods ...modules.Module) *modules.Manager {
	t.Helper()
	os.MkdirAll(filepath.Join(f.root, "data"), 0o755)
	mgr, err := modules.New(f.root, f.bus)
	if err != nil {
		t.Fatal(err)
	}
	for _, mod := range mods {
		mgr.Register(mod)
	}
	f.srv.RegisterModules(ModulesOptions{Manager: mgr})
	return mgr
}

// Credentials set up on one node reach the others through the mesh and
// are taken there by that node's own verify-install-enable: the shared
// values arrive, the machine's own value stays what it was there, the
// file on disk holds the token sealed with the far node's key, and a node
// whose verify refuses the token keeps nothing and says why.
func TestModuleCredentialsPropagateThroughTheMeshToTheNodesChosen(t *testing.T) {
	port := meshPort(t)
	a, _ := nodeFixture(t, "alpha", port)
	b, bm := nodeFixture(t, "beta", port)
	c, _ := nodeFixture(t, "gamma", port)
	amgr := nodeWithModules(t, a, &keyModule{key: "cloud"}, &boundModule{keyModule{key: "tun"}})
	bmgr := nodeWithModules(t, b, &keyModule{key: "cloud"}, &boundModule{keyModule{key: "tun"}})
	cmgr := nodeWithModules(t, c, &keyModule{key: "cloud", refuse: "tok-alpha"}, &boundModule{keyModule{key: "tun"}})

	_, inv := a.call("POST", "/api/v1/nodes/invite", map[string]string{"endpoint": "198.51.100.1:51821"})
	if res, out := b.call("POST", "/api/v1/nodes/join", map[string]string{"code": inv["code"].(string)}); res.StatusCode != 200 {
		t.Fatalf("join: %d %s", res.StatusCode, text(out))
	}
	_, inv2 := a.call("POST", "/api/v1/nodes/invite", map[string]string{})
	if res, out := c.call("POST", "/api/v1/nodes/join", map[string]string{"code": inv2["code"].(string)}); res.StatusCode != 200 {
		t.Fatalf("join 2: %d %s", res.StatusCode, text(out))
	}

	// Nothing set up here yet: nothing to carry.
	if res, out := a.call("POST", "/api/v1/modules/cloud/propagate", map[string]any{}); res.StatusCode != http.StatusConflict {
		t.Fatalf("propagating a module never set up: %d %s", res.StatusCode, text(out))
	}
	if _, err := amgr.Enable(context.Background(), "cloud", modules.Config{"token": "tok-alpha", "account": "acc-1", "tunnel": "tunnel-of-alpha"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := bmgr.Enable(context.Background(), "cloud", modules.Config{"token": "old", "tunnel": "tunnel-of-beta"}, nil); err != nil {
		t.Fatal(err)
	}

	// Only beta, by name.
	res, out := a.call("POST", "/api/v1/modules/cloud/propagate", map[string]any{"nodes": []string{bm.Self().ID}})
	if res.StatusCode != 200 || out["ok"] != true {
		t.Fatalf("propagate to beta: %d %s", res.StatusCode, text(out))
	}
	if got := out["results"].([]any); len(got) != 1 || got[0].(map[string]any)["name"] != "beta" {
		t.Fatalf("results: %v", got)
	}
	cfg, err := bmgr.Config("cloud")
	if err != nil {
		t.Fatal(err)
	}
	if cfg["token"] != "tok-alpha" || cfg["account"] != "acc-1" || cfg["tunnel"] != "tunnel-of-beta" {
		t.Fatalf("beta's config after the propagation: %v", cfg)
	}
	raw, _ := os.ReadFile(filepath.Join(b.root, "data", modules.StoreFile))
	if strings.Contains(string(raw), "tok-alpha") {
		t.Fatal("beta stored the token in clear")
	}
	if _, err := cmgr.Config("cloud"); err != nil {
		t.Fatal(err)
	}
	if cmgr.Enabled("cloud") {
		t.Fatal("gamma was not chosen and got the module")
	}

	// Every node by default: beta takes it again, gamma's verify refuses.
	res, out = a.call("POST", "/api/v1/modules/cloud/propagate", nil)
	if res.StatusCode != 200 || out["ok"] != false {
		t.Fatalf("propagate to all: %d %s", res.StatusCode, text(out))
	}
	byName := map[string]map[string]any{}
	for _, r := range out["results"].([]any) {
		byName[r.(map[string]any)["name"].(string)] = r.(map[string]any)
	}
	if len(byName) != 2 || byName["beta"]["ok"] != true || byName["gamma"]["ok"] != false {
		t.Fatalf("results: %v", byName)
	}
	if !strings.Contains(text(out), "this token is not accepted here") {
		t.Fatalf("gamma's refusal does not say why: %s", text(out))
	}
	if cmgr.Enabled("cloud") {
		t.Fatal("gamma saved a token its verify refused")
	}

	// A module that is the machine's own is never carried.
	if _, err := amgr.Enable(context.Background(), "tun", modules.Config{"token": "wg-key"}, nil); err != nil {
		t.Fatal(err)
	}
	if res, out := a.call("POST", "/api/v1/modules/tun/propagate", nil); res.StatusCode != http.StatusConflict {
		t.Fatalf("a node-bound module was propagated: %d %s", res.StatusCode, text(out))
	}
	if bmgr.Enabled("tun") {
		t.Fatal("beta got a node-bound module")
	}
	// A node that is not in the network is not a target.
	if res, out := a.call("POST", "/api/v1/modules/cloud/propagate", map[string]any{"nodes": []string{"nope"}}); res.StatusCode != http.StatusNotFound {
		t.Fatalf("an unknown node: %d %s", res.StatusCode, text(out))
	}
	var list []modules.Info
	getJSON(t, a, "/api/v1/modules", &list)
	for _, m := range list {
		if (m.Key == "cloud") != m.Shareable {
			t.Fatalf("%s shareable = %v", m.Key, m.Shareable)
		}
	}
}
