package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/agent"
	"github.com/dcimorra/the-middleware/internal/chat"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/sitetoken"
)

// answeringProvider answers every turn with a fixed text and cost, and
// records what it was asked to run with.
type answeringProvider struct {
	answer string
	// key is the agent it stands for; empty is claude. cost is what each
	// turn reports: Claude's 0.25 unless set below zero, which is none.
	key  string
	cost float64
	mu   sync.Mutex
	reqs []agent.Request
}

func (p *answeringProvider) Key() string {
	if p.key == "" {
		return "claude"
	}
	return p.key
}
func (p *answeringProvider) Available() (bool, string) { return true, "fake" }
func (p *answeringProvider) Models() []string {
	if p.key == "openai" {
		return []string{"gpt-5.5"}
	}
	return []string{"sonnet"}
}

// codexLike is an agent that cannot hold "shell" apart, as Codex.
type codexLike struct{ *answeringProvider }

func (codexLike) Permissions() []string { return []string{"read", "edit", "web"} }
func (p *answeringProvider) Start(ctx context.Context, req agent.Request) (agent.Run, error) {
	p.mu.Lock()
	p.reqs = append(p.reqs, req)
	p.mu.Unlock()
	ch := make(chan agent.Event, 64)
	if req.Partial {
		// As Claude Code with --include-partial-messages: the pieces
		// first, then the whole message.
		for _, w := range strings.SplitAfter(p.answer, " ") {
			ch <- agent.Event{Kind: agent.KindDelta, Text: w, SessionID: "s1"}
		}
	}
	ch <- agent.Event{Kind: agent.KindText, Text: p.answer, SessionID: "s1"}
	cost := 0.25
	if p.cost < 0 {
		cost = 0
	}
	ch <- agent.Event{Kind: agent.KindResult, Cost: cost}
	close(ch)
	return chanRun(ch), nil
}
func (p *answeringProvider) last() agent.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reqs[len(p.reqs)-1]
}

type chanRun chan agent.Event

func (r chanRun) Events() <-chan agent.Event { return r }
func (r chanRun) Stop()                      {}

func doorFixture(t *testing.T, manifests map[string]string, more ...agent.Provider) (*fixture, *sitetoken.Keys, *answeringProvider, *chat.Manager) {
	f := newFixture(t).withToken()
	for d, m := range manifests {
		os.MkdirAll(filepath.Join(f.root, "sites", d), 0o755)
		os.WriteFile(filepath.Join(f.root, "sites", d, "site.json"), []byte(m), 0o644)
	}
	p := &answeringProvider{answer: `{"ok": true}`}
	reg := agent.NewRegistry()
	reg.Register(p)
	for _, o := range more {
		reg.Register(o)
	}
	m := chat.New(chat.Options{Root: f.root, Bus: f.bus, Registry: reg})
	keys := sitetoken.New(filepath.Join(f.root, "data"))
	f.srv.RegisterSiteDoor(SiteDoorOptions{
		Chat: m, Tokens: keys,
		Lookup: func(d string) (*manifest.Manifest, bool) {
			mf, err := manifest.Load(filepath.Join(f.root, "sites", d))
			return mf, err == nil
		},
		HouseDaily: func() float64 { return 1 },
		Spent:      m.Ledger().Day,
		Bus:        f.bus,
		SiteDir:    func(d string) string { return filepath.Join(f.root, "sites", d) },
		Models: func(provider string) []string {
			_, ms := m.ProviderInfo(provider)
			if provider == "claude" {
				ms = append(ms, "haiku")
			}
			return ms
		},
	})
	return f, keys, p, m
}

func (f *fixture) asSite(keys *sitetoken.Keys, domain, method, path string, body any) (int, map[string]any) {
	tok, _ := keys.Token(domain)
	saved := f.token
	f.token = tok
	defer func() { f.token = saved }()
	resp, out := f.call(method, path, body)
	return resp.StatusCode, out
}

func TestTheSiteDoorAnswersItsSiteWithTheLevelItsManifestSays(t *testing.T) {
	f, keys, p, _ := doorFixture(t, map[string]string{
		"open.test":   `{"version":2,"claude_api":{"tools":"read","model":"haiku"}}`,
		"closed.test": `{"version":2}`,
	})
	if code, _ := f.asSite(keys, "closed.test", "GET", "/api/v1/site", nil); code != 403 {
		t.Fatalf("a site without the door got %d", code)
	}
	code, me := f.asSite(keys, "open.test", "GET", "/api/v1/site", nil)
	if code != 200 || me["domain"] != "open.test" {
		t.Fatalf("me: %d %v", code, me)
	}
	code, out := f.asSite(keys, "open.test", "POST", "/api/v1/site/claude/ask", map[string]any{"prompt": "is it ok?", "format": "json"})
	if code != 200 || out["text"] != `{"ok": true}` || out["json"].(map[string]any)["ok"] != true {
		t.Fatalf("ask: %d %v", code, out)
	}
	req := p.last()
	if req.Model != "haiku" || strings.Join(req.Tools, ",") != "Read,Glob,Grep" || !strings.Contains(strings.Join(req.Jail.Secrets, ","), ".env") {
		t.Fatalf("the turn ran with model %q tools %v secrets %v", req.Model, req.Tools, req.Jail.Secrets)
	}
	if !strings.Contains(req.System, "program, not a person") || strings.Contains(req.System, "question") {
		t.Fatalf("the site's turn got the panel's preamble:\n%s", req.System)
	}
	// Its conversation is listed and continued; the panel's is never
	// reachable from here, nor another site's.
	id := out["chat"].(string)
	if out["state"] != "done" || out["turns"].(float64) != 1 || out["cost_usd"].(float64) != 0.25 || out["model"] != "haiku" {
		t.Fatalf("the answer's fields: %v", out)
	}
	code, list := f.callList(keys, "open.test", "/api/v1/site/claude/chats")
	if code != 200 || len(list) != 1 {
		t.Fatalf("list: %d %v", code, list)
	}
	if code, _ := f.asSite(keys, "open.test", "POST", "/api/v1/site/claude/chats/"+id+"/messages", map[string]any{"prompt": "and now?"}); code != 200 {
		t.Fatalf("continue: %d", code)
	}
	if p.last().SessionID != "s1" {
		t.Fatal("the second turn did not resume the first")
	}
	if code, _ := f.asSite(keys, "closed.test", "GET", "/api/v1/site/claude/chats/"+id, nil); code != 403 {
		t.Fatalf("another site's token read the conversation: %d", code)
	}
	if code, _ := f.asSite(keys, "open.test", "GET", "/api/v1/site/claude/chats/ffff", nil); code != 404 {
		t.Fatalf("an unknown conversation: %d", code)
	}
	// A forged token is refused, slowly.
	start := time.Now()
	f.token = "open.test.forged"
	if resp, _ := f.call("GET", "/api/v1/site", nil); resp.StatusCode != 401 || time.Since(start) < 300*time.Millisecond {
		t.Fatalf("forged token: %d after %s", resp.StatusCode, time.Since(start))
	}
}

func TestTheDailyCapIsCheckedBeforeTheTurn(t *testing.T) {
	f, keys, _, _ := doorFixture(t, map[string]string{"a.test": `{"version":2,"claude_api":{"daily_usd":0.3}}`})
	if code, _ := f.asSite(keys, "a.test", "POST", "/api/v1/site/claude/ask", map[string]any{"prompt": "one"}); code != 200 {
		t.Fatalf("first: %d", code)
	}
	if code, _ := f.asSite(keys, "a.test", "POST", "/api/v1/site/claude/ask", map[string]any{"prompt": "two"}); code != 200 {
		t.Fatalf("second: %d", code)
	}
	code, out := f.asSite(keys, "a.test", "POST", "/api/v1/site/claude/ask", map[string]any{"prompt": "three"})
	if code != 429 || !strings.Contains(text(out), "daily_usd") {
		t.Fatalf("over the cap: %d %v", code, out)
	}
}

func TestNoneMeansNoToolsAndWriteMeansTheJail(t *testing.T) {
	f, keys, p, _ := doorFixture(t, map[string]string{
		"n.test": `{"version":2,"claude_api":{}}`,
		"w.test": `{"version":2,"claude_api":{"tools":"write"}}`,
	})
	f.asSite(keys, "n.test", "POST", "/api/v1/site/claude/ask", map[string]any{"prompt": "x"})
	if r := p.last(); r.Tools == nil || len(r.Tools) != 0 || strings.Join(r.Disallowed, ",") != "Task" {
		t.Fatalf("none: tools %#v disallowed %v", r.Tools, r.Disallowed)
	}
	f.asSite(keys, "w.test", "POST", "/api/v1/site/claude/ask", map[string]any{"prompt": "x"})
	if r := p.last(); r.Tools != nil || len(r.Jail.Secrets) != 0 {
		t.Fatalf("write: tools %#v secrets %v", r.Tools, r.Jail.Secrets)
	}
}

func (f *fixture) callList(keys *sitetoken.Keys, domain, path string) (int, []any) {
	tok, _ := keys.Token(domain)
	saved := f.token
	f.token = tok
	defer func() { f.token = saved }()
	resp, out := f.call("GET", path, nil)
	list, _ := out["chats"].([]any)
	return resp.StatusCode, list
}

// A site's code written for V1 calls $MIDDLEWARE_API/api/site/…, and
// reads chat, state, text, turns and cost_usd: it must not notice the move.
func TestASiteWrittenForV1WorksUnchanged(t *testing.T) {
	f, keys, _, m := doorFixture(t, map[string]string{"v1.test": `{"version":2,"claude_api":{}}`})
	os.WriteFile(filepath.Join(f.root, "sites", "v1.test", "notes.txt"), []byte("hello"), 0o644)
	code, me := f.asSite(keys, "v1.test", "GET", "/api/site", nil)
	cl, _ := me["claude"].(map[string]any)
	if code != 200 || cl == nil || cl["available"] != true || cl["tools"] != "none" || cl["busy"].(float64) != 0 || cl["limits"].(map[string]any)["max_chats"].(float64) != 20 {
		t.Fatalf("GET /api/site: %d %v", code, me)
	}
	// Files of its own folder go with the message; one outside it does not.
	code, out := f.asSite(keys, "v1.test", "POST", "/api/site/claude/ask", map[string]any{"prompt": "read it", "files": []string{"notes.txt"}})
	if code != 200 || out["chat"] == nil || out["text"] == nil {
		t.Fatalf("ask with files: %d %v", code, out)
	}
	if code, _ := f.asSite(keys, "v1.test", "POST", "/api/site/claude/ask", map[string]any{"prompt": "x", "files": []string{"../../data/site-door.key"}}); code != 400 {
		t.Fatalf("a file outside the folder: %d", code)
	}
	if code, _ := f.asSite(keys, "v1.test", "POST", "/api/site/claude/ask", map[string]any{"prompt": "x", "model": "gpt-4"}); code != 400 {
		t.Fatalf("an unknown model: %d", code)
	}
	// Opening without a word answers the conversation; wait 0 answers 202.
	code, opened := f.asSite(keys, "v1.test", "POST", "/api/site/claude/chats", map[string]any{"title": "t"})
	chatInfo, _ := opened["chat"].(map[string]any)
	if code != 200 || chatInfo == nil || chatInfo["origin"] != "site" {
		t.Fatalf("open: %d %v", code, opened)
	}
	id := chatInfo["id"].(string)
	code, out = f.asSite(keys, "v1.test", "POST", "/api/site/claude/chats/"+id+"/messages", map[string]any{"prompt": "go", "wait": "0"})
	if code != 202 || out["state"] != "running" || out["chat"] != id {
		t.Fatalf("wait 0: %d %v", code, out)
	}
	name := chat.Name("v1.test", id)
	m.Wait(context.Background(), name, 5*time.Second)
	code, got := f.asSite(keys, "v1.test", "GET", "/api/site/claude/chats/"+id, nil)
	msgs, _ := got["messages"].([]any)
	if code != 200 || got["state"] != "done" || got["text"] == "" || len(msgs) < 3 {
		t.Fatalf("get: %d %v", code, got)
	}
	// An upload, raw with ?name=, and its removal.
	tok, _ := keys.Token("v1.test")
	req, _ := http.NewRequest("POST", f.http.URL+"/api/site/claude/chats/"+id+"/files?name=a.csv", strings.NewReader("x,y"))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("upload: %v %v", resp, err)
	}
	resp.Body.Close()
	if code, _ := f.asSite(keys, "v1.test", "DELETE", "/api/site/claude/chats/"+id+"/files/a.csv", nil); code != 200 {
		t.Fatalf("remove upload: %d", code)
	}
}

func TestAtItsLimitASiteLosesItsOldestConversationNotItsNextOne(t *testing.T) {
	f, keys, _, m := doorFixture(t, map[string]string{"p.test": `{"version":2,"claude_api":{"max_chats":2}}`})
	var ids []string
	for i := 0; i < 3; i++ {
		code, out := f.asSite(keys, "p.test", "POST", "/api/site/claude/chats", nil)
		if code != 200 {
			t.Fatalf("open %d: %d %v", i, code, out)
		}
		ids = append(ids, out["chat"].(map[string]any)["id"].(string))
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := m.Conversation(chat.Name("p.test", ids[0])); err == nil {
		t.Fatal("the oldest was kept")
	}
	if _, err := m.Conversation(chat.Name("p.test", ids[2])); err != nil {
		t.Fatal("the newest is missing")
	}
}

func TestTheStreamSendsAConversationsMessagesAndItsEnd(t *testing.T) {
	f, keys, _, _ := doorFixture(t, map[string]string{"s.test": `{"version":2,"claude_api":{}}`})
	_, opened := f.asSite(keys, "s.test", "POST", "/api/site/claude/chats", nil)
	id := opened["chat"].(map[string]any)["id"].(string)
	tok, _ := keys.Token("s.test")
	req, _ := http.NewRequest("GET", f.http.URL+"/api/site/claude/chats/"+id+"/stream", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("stream: %v %v", resp, err)
	}
	defer resp.Body.Close()
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	<-lines // ": connected"
	f.asSite(keys, "s.test", "POST", "/api/site/claude/chats/"+id+"/messages", map[string]any{"prompt": "hi", "wait": "0"})
	// The answer comes as it is written (event: delta), its pieces all
	// before the whole message, and adds up to it.
	var seen []string
	var draft strings.Builder
	last := ""
	deadline := time.After(5 * time.Second)
	for {
		select {
		case l := <-lines:
			if ev, ok := strings.CutPrefix(l, "event: "); ok {
				last = ev
				if ev != "delta" || len(seen) == 0 || seen[len(seen)-1] != "delta" {
					seen = append(seen, ev)
				}
			}
			if data, ok := strings.CutPrefix(l, "data: "); ok && last == "delta" {
				var d struct{ TurnID, Text string }
				if json.Unmarshal([]byte(data), &d) != nil || d.Text == "" {
					t.Fatalf("delta: %s", data)
				}
				draft.WriteString(d.Text)
			}
			if data, ok := strings.CutPrefix(l, "data: "); ok && last == "message" && strings.Contains(data, `"role":"assistant"`) {
				var msg struct{ Text string }
				if json.Unmarshal([]byte(data), &msg) != nil || msg.Text != draft.String() {
					t.Fatalf("the pieces %q are not the message %s", draft.String(), data)
				}
			}
			if l == "event: session" {
				if strings.Join(seen, ",") != "delta,message,message,session" {
					t.Fatalf("events %v", seen)
				}
				return
			}
		case <-deadline:
			t.Fatalf("no session event; saw %v", seen)
		}
	}
}

func TestTheGatewayRunsTheAgentItsManifestNamesWithThePermissionsItGrants(t *testing.T) {
	codex := &answeringProvider{answer: "from codex", key: "openai", cost: -1}
	f, keys, claude, _ := doorFixture(t, map[string]string{
		"o.test": `{"version":2,"claude_api":{"provider":"openai","permissions":["read","edit"],"model":"gpt-5.5","daily_turns":3}}`,
		"c.test": `{"version":2,"claude_api":{"permissions":["web"]}}`,
	}, codex)
	code, out := f.asSite(keys, "o.test", "POST", "/api/v1/site/claude/ask", map[string]any{"prompt": "hi"})
	if code != 200 || out["text"] != "from codex" || out["model"] != "gpt-5.5" {
		t.Fatalf("ask: %d %v", code, out)
	}
	r := codex.last()
	if strings.Join(r.Tools, ",") != "Read,Glob,Grep,Edit,Write,MultiEdit,NotebookEdit" || strings.Join(r.Disallowed, ",") != "Task" {
		t.Fatalf("codex ran with %v / %v", r.Tools, r.Disallowed)
	}
	// A site written for Claude names "sonnet" in every call: the agent and
	// model chosen in the panel run instead, and the answer says so.
	if code, out := f.asSite(keys, "o.test", "POST", "/api/v1/site/claude/ask", map[string]any{"prompt": "x", "model": "sonnet"}); code != 200 || out["model"] != "gpt-5.5" || out["model_ignored"] != "sonnet" {
		t.Fatalf("a model of another agent: %d %v", code, out)
	}
	// Its turns cost nothing that can be read: they are counted, and the
	// cap on turns is what stops the third.
	f.asSite(keys, "o.test", "POST", "/api/v1/site/claude/ask", map[string]any{"prompt": "two"})
	code, out = f.asSite(keys, "o.test", "POST", "/api/v1/site/claude/ask", map[string]any{"prompt": "three"})
	if code != 429 || !strings.Contains(text(out), "daily_turns") {
		t.Fatalf("over the turns cap: %d %v", code, out)
	}
	// "web" alone is the web and not the folder.
	f.asSite(keys, "c.test", "POST", "/api/v1/site/claude/ask", map[string]any{"prompt": "x"})
	if r := claude.last(); strings.Join(r.Tools, ",") != "WebSearch,WebFetch" {
		t.Fatalf("web alone ran with %v", r.Tools)
	}
}

func TestOpeningTheGatewayGivesEveryPermissionAndRefusesWhatTheAgentCannotHoldApart(t *testing.T) {
	codex := codexLike{&answeringProvider{key: "openai", cost: -1}}
	f, _, _, _ := doorFixture(t, map[string]string{
		"g.test": `{"version":2,"run":{"command":["true"]}}`,
	}, codex)
	_, d := f.call("GET", "/api/v1/sites/g.test/claude-api", nil)
	def := d["defaults"].(map[string]any)
	if def["provider"] != "claude" || len(def["permissions"].([]any)) != 4 {
		t.Fatalf("defaults: %v", def)
	}
	var codexPerms []any
	for _, a := range d["agents"].([]any) {
		if a.(map[string]any)["provider"] == "openai" {
			codexPerms = a.(map[string]any)["permissions"].([]any)
		}
	}
	if len(codexPerms) != 3 {
		t.Fatalf("agents: %v", d["agents"])
	}
	// Opened with the defaults, the manifest says so out loud.
	if resp, out := f.call("POST", "/api/v1/sites/g.test/claude-api", map[string]any{"enabled": true, "provider": "claude", "permissions": def["permissions"]}); resp.StatusCode != 200 {
		t.Fatalf("open: %d %v", resp.StatusCode, out)
	}
	mf, _ := manifest.Load(filepath.Join(f.root, "sites", "g.test"))
	if mf.ClaudeAPI == nil || strings.Join(mf.ClaudeAPI.Permissions, ",") != "read,edit,web,shell" {
		t.Fatalf("written: %+v", mf.ClaudeAPI)
	}
	if resp, out := f.call("POST", "/api/v1/sites/g.test/claude-api", map[string]any{"enabled": true, "provider": "openai", "permissions": []string{"read", "shell"}}); resp.StatusCode != 400 || !strings.Contains(text(out), "shell") {
		t.Fatalf("shell for codex: %d %v", resp.StatusCode, out)
	}
	if resp, _ := f.call("POST", "/api/v1/sites/g.test/claude-api", map[string]any{"enabled": true, "provider": "nobody"}); resp.StatusCode != 400 {
		t.Fatalf("an agent that is not here: %d", resp.StatusCode)
	}
	if resp, out := f.call("POST", "/api/v1/sites/g.test/claude-api", map[string]any{"enabled": true, "provider": "openai", "permissions": []string{"read"}, "model": "gpt-5.5"}); resp.StatusCode != 200 {
		t.Fatalf("codex reading: %d %v", resp.StatusCode, out)
	}
	mf, _ = manifest.Load(filepath.Join(f.root, "sites", "g.test"))
	if mf.ClaudeAPI.Provider != "openai" || strings.Join(mf.ClaudeAPI.Permissions, ",") != "read" || mf.ClaudeAPI.Model != "gpt-5.5" {
		t.Fatalf("written: %+v", mf.ClaudeAPI)
	}
}

func TestAConversationOpenedUnderClaudeMovesWithTheGateway(t *testing.T) {
	codex := &answeringProvider{answer: "from codex", key: "openai", cost: -1}
	f, keys, claude, _ := doorFixture(t, map[string]string{"m.test": `{"version":2,"claude_api":{"tools":"read"}}`}, codex)
	code, out := f.asSite(keys, "m.test", "POST", "/api/v1/site/claude/chats", map[string]any{"prompt": "first"})
	if code != 200 || len(claude.reqs) != 1 {
		t.Fatalf("first: %d %v", code, out)
	}
	id := out["chat"].(string)
	// The operator moves the door to Codex with a model of its own.
	os.WriteFile(filepath.Join(f.root, "sites", "m.test", "site.json"), []byte(`{"version":2,"claude_api":{"provider":"openai","permissions":["read"],"model":"gpt-5.5"}}`), 0o644)
	code, out = f.asSite(keys, "m.test", "POST", "/api/v1/site/claude/chats/"+id+"/messages", map[string]any{"prompt": "second", "model": "sonnet"})
	if code != 200 || out["text"] != "from codex" || out["model"] != "gpt-5.5" || len(codex.reqs) != 1 {
		t.Fatalf("second: %d %v", code, out)
	}
}

// The engine reads a manifest on its next pass; the gateway's screen reads
// right after it saved. What it shows is the file.
func TestTheGatewayScreenShowsWhatWasJustSavedNotTheEnginesOldCopy(t *testing.T) {
	f := newFixture(t).withToken()
	dir := filepath.Join(f.root, "sites", "s.test")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(`{"version":2,"run":{"command":["true"]},"claude_api":{}}`), 0o644)
	stale, _ := manifest.Load(dir)
	reg := agent.NewRegistry()
	reg.Register(&answeringProvider{})
	reg.Register(&answeringProvider{key: "openai"})
	m := chat.New(chat.Options{Root: f.root, Bus: f.bus, Registry: reg})
	f.srv.RegisterSiteDoor(SiteDoorOptions{
		Chat: m, Bus: f.bus, HouseDaily: func() float64 { return 1 }, Spent: m.Ledger().Day,
		// The engine's copy, from before the save.
		Lookup: func(string) (*manifest.Manifest, bool) { return stale, true },
	})
	if resp, out := f.call("POST", "/api/v1/sites/s.test/claude-api", map[string]any{"enabled": true, "provider": "openai", "permissions": []string{"read"}, "model": "gpt-5.5"}); resp.StatusCode != 200 {
		t.Fatalf("save: %d %v", resp.StatusCode, out)
	}
	_, d := f.call("GET", "/api/v1/sites/s.test/claude-api", nil)
	lim, _ := d["limits"].(map[string]any)
	if lim["provider"] != "openai" || lim["model"] != "gpt-5.5" {
		t.Fatalf("the screen shows %v", lim)
	}
}

func TestAScopedTokenCannotSpeakForAnySiteThroughTheDoor(t *testing.T) {
	f, _, p, _ := doorFixture(t, map[string]string{"open.test": `{"version":2,"claude_api":{}}`})
	// The operator's full token may name the site: the door is open to it.
	if resp, out := f.call("GET", "/api/v1/site", nil, "X-Middleware-Site: open.test"); resp.StatusCode != 200 {
		t.Fatalf("the operator's token: %d %v", resp.StatusCode, out)
	}
	_, secret, err := f.store.CreateScopedToken("ci", []string{"deploy:other.test"})
	if err != nil {
		t.Fatal(err)
	}
	f.token = secret
	resp, out := f.call("POST", "/api/v1/site/claude/ask", map[string]any{"prompt": "write a file"}, "X-Middleware-Site: open.test")
	if resp.StatusCode != 401 {
		t.Fatalf("a deploy token for another site used the door: %d %v", resp.StatusCode, out)
	}
	p.mu.Lock()
	n := len(p.reqs)
	p.mu.Unlock()
	if n != 0 {
		t.Fatalf("a turn ran for the scoped token: %d", n)
	}
}

func TestTheSitesCodePicksAmongTheModelsItsGatewayWasGiven(t *testing.T) {
	f, keys, p, _ := doorFixture(t, map[string]string{
		"m.test": `{"version":2,"claude_api":{"models":["haiku","sonnet"]}}`,
	})
	code, me := f.asSite(keys, "m.test", "GET", "/api/v1/site", nil)
	cl, _ := me["claude"].(map[string]any)
	if code != 200 || cl["model"] != "haiku" || fmt.Sprint(cl["models"]) != "[haiku sonnet]" {
		t.Fatalf("what the code is told: %d %v", code, cl)
	}
	// No model named: the first of the list, not the house's default.
	if code, out := f.asSite(keys, "m.test", "POST", "/api/v1/site/claude/ask", map[string]any{"prompt": "x"}); code != 200 || p.last().Model != "haiku" {
		t.Fatalf("the default: %d %v %q", code, out, p.last().Model)
	}
	// Another of the list, call by call, in the same conversation too.
	code, out := f.asSite(keys, "m.test", "POST", "/api/v1/site/claude/ask", map[string]any{"prompt": "x", "model": "sonnet"})
	if code != 200 || p.last().Model != "sonnet" {
		t.Fatalf("sonnet: %d %v %q", code, out, p.last().Model)
	}
	id := out["chat"].(string)
	if code, out := f.asSite(keys, "m.test", "POST", "/api/v1/site/claude/chats/"+id+"/messages", map[string]any{"prompt": "y", "model": "haiku"}); code != 200 || p.last().Model != "haiku" {
		t.Fatalf("haiku on the next message: %d %v %q", code, out, p.last().Model)
	}
	// One the agent has but the gateway was not given: refused before a
	// turn is spent.
	n := len(p.reqs)
	if code, _ := f.asSite(keys, "m.test", "POST", "/api/v1/site/claude/ask", map[string]any{"prompt": "x", "model": "opus"}); code != 400 {
		t.Fatalf("a model off the list: %d", code)
	}
	if code, _ := f.asSite(keys, "m.test", "POST", "/api/v1/site/claude/chats/"+id+"/messages", map[string]any{"prompt": "x", "model": "opus"}); code != 400 || len(p.reqs) != n {
		t.Fatalf("a model off the list on a message: %d, %d turns ran", code, len(p.reqs)-n)
	}
}

func TestTheGatewayScreenSavesSeveralModelsWithTheDefaultAmongThem(t *testing.T) {
	f, _, _, _ := doorFixture(t, map[string]string{
		"s.test": `{"version":2,"run":{"command":["true"]}}`,
	})
	if resp, out := f.call("POST", "/api/v1/sites/s.test/claude-api", map[string]any{"enabled": true, "permissions": []string{"read"}, "model": "sonnet", "models": []string{"haiku", "haiku"}}); resp.StatusCode != 200 {
		t.Fatalf("save: %d %v", resp.StatusCode, out)
	}
	mf, err := manifest.Load(filepath.Join(f.root, "sites", "s.test"))
	if err != nil || mf.ClaudeAPI == nil || fmt.Sprint(mf.ClaudeAPI.Models) != "[sonnet haiku]" || mf.ClaudeAPI.Model != "sonnet" {
		t.Fatalf("written: %v %+v", err, mf.ClaudeAPI)
	}
	if resp, _ := f.call("POST", "/api/v1/sites/s.test/claude-api", map[string]any{"enabled": true, "models": []string{"nonsense"}}); resp.StatusCode != 400 {
		t.Fatalf("a model the agent does not have: %d", resp.StatusCode)
	}
}

// The site's code names the files and owns the folder they are in, links
// included: a link that leaves the folder, at the end or halfway, is
// refused at the open itself, so swapping a file for one between a check
// and the open has nothing to win.
func TestAFileAttachedThroughALinkOutOfTheFolderIsRefused(t *testing.T) {
	f, keys, _, _ := doorFixture(t, map[string]string{"j.test": `{"version":2,"claude_api":{}}`})
	dir := filepath.Join(f.root, "sites", "j.test")
	secret := filepath.Join(f.root, "data", "secret.txt")
	os.WriteFile(secret, []byte("the house's"), 0o600)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hello"), 0o644)
	os.Symlink(secret, filepath.Join(dir, "out.txt"))
	os.Symlink(filepath.Join(f.root, "data"), filepath.Join(dir, "updir"))
	os.Symlink("notes.txt", filepath.Join(dir, "in.txt"))
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	ask := func(file string) (int, map[string]any) {
		return f.asSite(keys, "j.test", "POST", "/api/v1/site/claude/ask", map[string]any{"prompt": "x", "files": []string{file}})
	}
	for _, bad := range []string{"out.txt", "updir/secret.txt", secret, "../../data/secret.txt", "pipe", "."} {
		done := make(chan int, 1)
		go func() { code, _ := ask(bad); done <- code }()
		select {
		case code := <-done:
			if code != 400 {
				t.Fatalf("%q was attached: %d", bad, code)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%q held the request", bad)
		}
	}
	// A link that stays inside is the file it points to.
	if code, out := ask("in.txt"); code != 200 {
		t.Fatalf("a link inside the folder: %d %v", code, out)
	}
}
