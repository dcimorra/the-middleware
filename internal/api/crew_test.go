package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dcimorra/the-middleware/internal/chat"
	"github.com/dcimorra/the-middleware/internal/crew"
	"github.com/dcimorra/the-middleware/internal/secrets"
)

// A process under a crew agent's turn, told by the mark every turn
// carries, reaches the socket as that agent: its own routes answer, and
// the operator's, another agent's, a site's and the tokens do not. Before
// the crew existed the house read a mark it did not know as nobody in
// particular, which on the socket is the operator: this is what keeps an
// agent's turn from being the operator.
func TestACrewTurnOnTheSocketIsConfinedToItsAgent(t *testing.T) {
	f := newFixture(t).withToken()
	box, _ := secrets.New(make([]byte, 32))
	c := crew.New(crew.Options{Root: f.root, Box: box, Running: func(string) bool { return false }})
	me, _ := c.Create(crew.Agent{Name: "helper", Goal: "g"})
	other, _ := c.Create(crew.Agent{Name: "other", Goal: "g"})
	f.srv.RegisterCrew(c, nil)
	if resp, body := f.call("POST", "/api/v1/sites", map[string]any{"domain": "a.example"}); resp.StatusCode != 201 {
		t.Fatalf("create: %d %v", resp.StatusCode, body)
	}
	sock := filepath.Join(t.TempDir(), "api.sock")
	ln, err := ListenSocket(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := SocketServer(ctx, f.srv.Panel(), func(pid int) (string, error) { return chat.SiteOfProcess("/proc", pid, nil) })
	go srv.Serve(ln)
	defer srv.Close()
	calls := []string{
		"GET /api/v1/crew/" + me.ID,
		`POST /api/v1/crew/` + me.ID + `/requests {"title":"a key","fields":[{"key":"API_KEY","secret":true}]}`,
		"GET /api/v1/crew/" + me.ID + "/vault",
		"GET /api/v1/crew",
		"GET /api/v1/crew/" + other.ID,
		`PUT /api/v1/crew/` + me.ID + `/vault/API_KEY {"value":"x"}`,
		`PUT /api/v1/crew/` + other.ID + `/vault/API_KEY {"value":"x"}`,
		`POST /api/v1/crew/` + me.ID + `/on`,
		`POST /api/v1/auth/tokens {"name":"mine"}`,
		"GET /api/v1/sites",
		"GET /api/v1/sites/a.example",
		"GET /api/v1/settings",
	}
	// Its own vault it may write (what it creates is kept there); not
	// another agent's.
	want := []int{200, 201, 200, 403, 403, 200, 403, 403, 403, 403, 403, 403}
	got := peer(t, sock, []string{chat.MarkVar + "=" + chat.CrewDomain(me.ID) + "#0123abcd"}, calls...)
	for i := range calls {
		if got[i] != want[i] {
			t.Errorf("crew turn: %s -> %d, want %d", calls[i], got[i], want[i])
		}
	}
	if toks, _ := f.store.Tokens(); len(toks) != 1 {
		t.Fatalf("a crew turn minted a token: %+v", toks)
	}
	// And the operator, with a full token, has all of it.
	if resp, body := f.call("PUT", "/api/v1/crew/"+me.ID+"/vault/API_KEY", map[string]string{"value": "secret"}); resp.StatusCode != 200 {
		t.Fatalf("the operator's vault write: %d %v", resp.StatusCode, body)
	}
	raw, _ := os.ReadFile(filepath.Join(f.root, "data", "crew", me.ID, "vault.json"))
	if len(raw) == 0 || strings.Contains(string(raw), "secret\"") {
		t.Errorf("the vault on disk: %s", raw)
	}
}
