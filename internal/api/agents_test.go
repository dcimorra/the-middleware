package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/agent"
	"github.com/dcimorra/the-middleware/internal/auth"
	"github.com/dcimorra/the-middleware/internal/cgroups"
	"github.com/dcimorra/the-middleware/internal/chat"
	"github.com/dcimorra/the-middleware/internal/settings"
)

// fakeProvider is a provider that is never available, which is all the
// listing needs; starting a run is the chat package's business.
type fakeProvider struct{ key string }

func (p fakeProvider) Key() string               { return p.key }
func (p fakeProvider) Available() (bool, string) { return false, p.key + " not installed" }
func (p fakeProvider) Models() []string          { return []string{"m1", "m2"} }
func (p fakeProvider) Start(context.Context, agent.Request) (agent.Run, error) {
	return nil, agent.ErrUnknownProvider
}

func TestAgentsAreListedAndTheConversationsAnswer(t *testing.T) {
	f := newFixture(t).withToken()
	reg := agent.NewRegistry()
	reg.Register(fakeProvider{key: "claude"})
	os.MkdirAll(filepath.Join(f.root, "sites", "x.test"), 0o755)
	// The cap is computed from the machine when the settings leave it:
	// 24 GiB is 5 turns.
	defer func(f func() cgroups.Machine) { settings.Machine = f }(settings.Machine)
	settings.Machine = func() cgroups.Machine { return cgroups.Machine{CPUs: 8, Memory: 24 << 30} }
	m := chat.New(chat.Options{Root: f.root, Bus: f.bus, Registry: reg})
	f.srv.RegisterAgents(AgentsOptions{Registry: reg, Chat: m})

	resp, body := f.call("GET", "/api/v1/agents", nil)
	if resp.StatusCode != 200 || !strings.Contains(text(body), `"provider": "claude"`) || !strings.Contains(text(body), `"capacity"`) || !strings.Contains(text(body), `"cpu_cores": 6`) {
		t.Fatalf("agents: %d %s", resp.StatusCode, text(body))
	}
	resp, body = f.call("GET", "/api/v1/agents/capacity", nil)
	if resp.StatusCode != 200 || body["max"] != float64(5) || body["max_computed"] != true || body["used"] != float64(0) {
		t.Fatalf("capacity: %d %v", resp.StatusCode, body)
	}
	resp, body = f.call("GET", "/api/v1/chat/middleware", nil)
	if resp.StatusCode != 200 || body["domain"] != chat.Middleware || body["provider"] != "claude" {
		t.Fatalf("middleware conversation: %d %v", resp.StatusCode, body)
	}
	resp, body = f.call("GET", "/api/v1/sites/x.test/chat", nil)
	if resp.StatusCode != 200 || body["domain"] != "x.test" {
		t.Fatalf("site conversation: %d %v", resp.StatusCode, body)
	}
	resp, _ = f.call("GET", "/api/v1/sites/nope.test/chat", nil)
	if resp.StatusCode != 404 {
		t.Fatalf("unknown site: %d", resp.StatusCode)
	}
	resp, _ = f.call("POST", "/api/v1/sites/x.test/chat", map[string]string{"text": ""})
	if resp.StatusCode != 400 {
		t.Fatalf("empty message: %d", resp.StatusCode)
	}
	// The provider cannot run, so the turn is refused before it starts.
	resp, body = f.call("POST", "/api/v1/sites/x.test/chat", map[string]string{"text": "hello"})
	if resp.StatusCode == 200 {
		t.Fatalf("a turn without a runnable provider must not start: %v", body)
	}
	resp, _ = f.call("POST", "/api/v1/sites/x.test/chat/stop", nil)
	if resp.StatusCode != 409 {
		t.Fatalf("stop with nothing running: %d", resp.StatusCode)
	}
	resp, _ = f.call("POST", "/api/v1/sites/x.test/chat/send-now", nil)
	if resp.StatusCode != 409 {
		t.Fatalf("send now with nothing running: %d", resp.StatusCode)
	}
	// The export is the same conversation, as a file to save.
	req, _ := http.NewRequest("GET", f.http.URL+"/api/v1/sites/x.test/chat/export", nil)
	req.Header.Set("Authorization", "Bearer "+f.token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var exp struct {
		Exported     time.Time         `json:"exported"`
		Conversation chat.Conversation `json:"conversation"`
	}
	_ = json.NewDecoder(res.Body).Decode(&exp)
	res.Body.Close()
	if cd := res.Header.Get("Content-Disposition"); res.StatusCode != 200 || !strings.Contains(cd, `attachment; filename="x.test-chat-`) || exp.Conversation.Domain != "x.test" || exp.Exported.IsZero() {
		t.Fatalf("export: %d %q %+v", res.StatusCode, cd, exp)
	}
	resp, _ = f.call("DELETE", "/api/v1/chat/middleware", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("reset: %d", resp.StatusCode)
	}
}

// holdingProvider runs turns that stay open until release is closed, so
// the capacity can be filled from the API.
type holdingProvider struct {
	key     string
	release chan struct{}
}

func (p holdingProvider) Key() string               { return p.key }
func (p holdingProvider) Available() (bool, string) { return true, "fake" }
func (p holdingProvider) Models() []string          { return []string{"m1"} }
func (p holdingProvider) Start(ctx context.Context, _ agent.Request) (agent.Run, error) {
	r := &holdingRun{ch: make(chan agent.Event), stop: make(chan struct{})}
	go func() {
		defer close(r.ch)
		select {
		case <-p.release:
			r.ch <- agent.Event{Kind: agent.KindResult, Text: "ok"}
		case <-r.stop:
			r.ch <- agent.Event{Kind: agent.KindError, Text: "stopped"}
		case <-ctx.Done():
		}
	}()
	return r, nil
}

type holdingRun struct {
	ch   chan agent.Event
	stop chan struct{}
	once sync.Once
}

func (r *holdingRun) Events() <-chan agent.Event { return r.ch }
func (r *holdingRun) Stop()                      { r.once.Do(func() { close(r.stop) }) }

func TestAFullHouseAnswers429AndANewCapFromSettingsAppliesAtOnce(t *testing.T) {
	f := newFixture(t).withToken()
	release := make(chan struct{})
	reg := agent.NewRegistry()
	reg.Register(holdingProvider{key: "claude", release: release})
	for _, d := range []string{"a.test", "b.test"} {
		os.MkdirAll(filepath.Join(f.root, "sites", d), 0o755)
	}
	m := chat.New(chat.Options{Root: f.root, Bus: f.bus, Registry: reg, Limits: settings.Agents{MaxConcurrent: 2, ReservedForHouse: 1, DefaultProvider: "claude"}})
	// The held turns finish after the test body and write the conversation
	// under data/chat; waiting for them here, before the temp dir goes,
	// is what kept this test from failing one run in ten.
	t.Cleanup(func() {
		close(release)
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if !m.Running("a.test") && !m.Running("b.test") {
				return
			}
		}
	})
	f.srv.RegisterAgents(AgentsOptions{Registry: reg, Chat: m})
	var saved settings.Settings
	f.srv.opts.SaveSettings = func(s settings.Settings) error { saved = s; return nil }
	f.srv.opts.Settings = func() settings.Settings { return saved }
	ch, unsub := f.bus.Subscribe()
	defer unsub()

	resp, body := f.call("POST", "/api/v1/sites/a.test/chat", map[string]string{"text": "one"})
	if resp.StatusCode != 200 {
		t.Fatalf("first turn: %d %s", resp.StatusCode, text(body))
	}
	resp, body = f.call("POST", "/api/v1/sites/b.test/chat", map[string]string{"text": "two"})
	if resp.StatusCode != 429 || !strings.Contains(text(body), "1 of 2") {
		t.Fatalf("second turn: %d %s", resp.StatusCode, text(body))
	}
	deadline := time.After(5 * time.Second)
	for warned := false; !warned; {
		select {
		case ev := <-ch:
			warned = ev.Type == "agent.capacity" && ev.Level == "warn"
		case <-deadline:
			t.Fatal("no agent.capacity event")
		}
	}
	resp, body = f.call("GET", "/api/v1/agents/capacity", nil)
	running, _ := body["running"].([]any)
	if resp.StatusCode != 200 || body["used"] != float64(1) || len(running) != 1 {
		t.Fatalf("capacity: %d %v", resp.StatusCode, body)
	}

	// The settings are held to their rules…
	st := settings.Defaults()
	st.Agents = settings.Agents{MaxConcurrent: 4, ReservedForHouse: 4, DefaultProvider: "claude"}
	resp, body = f.call("PUT", "/api/v1/settings", st)
	if resp.StatusCode != 400 || !strings.Contains(text(body), "reserved_for_house") {
		t.Fatalf("bad settings: %d %s", resp.StatusCode, text(body))
	}
	// …and a good cap is in force for the next message without a restart.
	st.Agents.ReservedForHouse = 1
	resp, body = f.call("PUT", "/api/v1/settings", st)
	if resp.StatusCode != 200 || saved.Agents.MaxConcurrent != 4 {
		t.Fatalf("save settings: %d %s", resp.StatusCode, text(body))
	}
	resp, body = f.call("POST", "/api/v1/sites/b.test/chat", map[string]string{"text": "two again"})
	if resp.StatusCode != 200 {
		t.Fatalf("after raising the cap: %d %s", resp.StatusCode, text(body))
	}
	// A body without the section keeps the defaults rather than zeroing
	// the cap: the computed one, with its slot for the house.
	resp, body = f.call("PUT", "/api/v1/settings", map[string]any{"listen": settings.Defaults().Listen})
	if resp.StatusCode != 200 || saved.Agents.MaxConcurrent != 0 || saved.Agents.ReservedForHouse != 1 || saved.Agents.DefaultProvider != "claude" {
		t.Fatalf("settings without agents: %d %s saved=%+v", resp.StatusCode, text(body), saved.Agents)
	}
}

// The bar's list and the whole purge cross the API as the panel uses
// them, and are the operator's: a delegate's panel would see every site's
// conversations in the one and lose them all to the other.
func TestTheRecentConversationsAndTheWholePurgeAreTheOperators(t *testing.T) {
	f := newFixture(t).withToken()
	f.makeSite("mine.example")
	reg := agent.NewRegistry()
	reg.Register(fakeProvider{key: "claude"})
	m := chat.New(chat.Options{Root: f.root, Bus: f.bus, Registry: reg})
	f.srv.RegisterAgents(AgentsOptions{Registry: reg, Chat: m})
	at := time.Now().UTC()
	for i, c := range []chat.Conversation{
		{Domain: "mine.example", Origin: chat.OriginPanel},
		{Domain: "mine.example", ID: "a1", Origin: chat.OriginPanel},
		{Domain: chat.Middleware, Origin: chat.OriginPanel},
	} {
		c.Turns = []chat.Turn{{ID: "u", Role: "user", Text: "hola", At: at.Add(time.Duration(i) * time.Minute)}}
		dir := filepath.Join(f.root, "data", "chat")
		file := filepath.Join(dir, c.Domain+".json")
		if c.ID != "" {
			dir = filepath.Join(dir, c.Domain)
			file = filepath.Join(dir, c.ID+".json")
		}
		os.MkdirAll(dir, 0o700)
		b, _ := json.Marshal(c)
		if err := os.WriteFile(file, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f.makeUser("bob", auth.Grant{Site: "mine.example", Perms: []string{"operate", "agents"}})
	bob := f.login("bob")
	for _, c := range []struct{ method, path string }{{"GET", "/api/v1/agents/recent"}, {"DELETE", "/api/v1/agents/chats"}} {
		if res, out := bob.call(c.method, c.path, nil); res.StatusCode != 404 {
			t.Fatalf("a delegate's %s %s: %d %s", c.method, c.path, res.StatusCode, text(out))
		}
	}

	var recent []chat.RecentChat
	getJSON(t, f, "/api/v1/agents/recent?limit=2", &recent)
	if len(recent) != 2 || recent[0].Name != chat.Middleware || recent[1].Name != "mine.example#a1" {
		t.Fatalf("recent: %+v", recent)
	}
	res, out := f.call("DELETE", "/api/v1/agents/chats", nil)
	if res.StatusCode != 200 || out["purged"] != float64(3) || out["busy"] != float64(0) {
		t.Fatalf("purge: %d %s", res.StatusCode, text(out))
	}
	recent = nil
	getJSON(t, f, "/api/v1/agents/recent", &recent)
	if len(recent) != 0 {
		t.Fatalf("after the purge: %+v", recent)
	}
}
