package chat

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/agent"
	"github.com/dcimorra/the-middleware/internal/events"
)

// fakeProvider replays canned events and records the requests it got. A
// run blocks before its last event when hold is set, so a test can see two
// turns collide.
type fakeProvider struct {
	key    string
	script []agent.Event
	// scripts, when set, are the script of each run in turn; the last one
	// is repeated.
	scripts [][]agent.Event
	hold    chan struct{}
	mu      sync.Mutex
	reqs    []agent.Request
}

func (f *fakeProvider) Key() string               { return f.key }
func (f *fakeProvider) Available() (bool, string) { return true, "fake" }
func (f *fakeProvider) Models() []string          { return []string{"fake-1"} }

func (f *fakeProvider) Start(ctx context.Context, req agent.Request) (agent.Run, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	script := f.script
	if len(f.scripts) > 0 {
		script = f.scripts[min(len(f.reqs), len(f.scripts))-1]
	}
	f.mu.Unlock()
	// Where a CLI would write its output, as the real ones do.
	if req.Stream != "" && os.MkdirAll(filepath.Dir(req.Stream), 0o700) == nil {
		_ = os.WriteFile(req.Stream, []byte("{\"type\":\"fake\"}\n"), 0o600)
	}
	r := &fakeRun{ch: make(chan agent.Event), stop: make(chan struct{})}
	go func() {
		defer close(r.ch)
		for i, ev := range script {
			if i == len(script)-1 && f.hold != nil {
				select {
				case <-f.hold:
				case <-r.stop:
					r.ch <- agent.Event{Kind: agent.KindError, Text: "stopped"}
					return
				case <-ctx.Done():
					return
				}
			}
			select {
			case r.ch <- ev:
			case <-r.stop:
				return
			}
		}
	}()
	return r, nil
}

func (f *fakeProvider) last() agent.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqs[len(f.reqs)-1]
}

type fakeRun struct {
	ch   chan agent.Event
	stop chan struct{}
	once sync.Once
}

func (r *fakeRun) Events() <-chan agent.Event { return r.ch }
func (r *fakeRun) Stop()                      { r.once.Do(func() { close(r.stop) }) }

func newManager(t *testing.T, p *fakeProvider) (*Manager, string, *events.Bus) {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"sites/a.test", "sites/b.test", "data", "tasks"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	os.WriteFile(filepath.Join(root, "sites", "a.test", "site.json"), []byte(`{"version":2,"agent":{"provider":"fake","model":"fake-1","monthly_cap_usd":1}}`), 0o644)
	bus := events.New("")
	reg := agent.NewRegistry()
	reg.Register(p)
	m := New(Options{Root: root, Bus: bus, Registry: reg, DefaultProvider: "fake"})
	return m, root, bus
}

func waitFor(t *testing.T, ch <-chan events.Event, typ string) events.Event {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Type == typ {
				return ev
			}
			if ev.Type == "agent.error" && typ != "agent.error" {
				t.Fatalf("agent.error while waiting for %s: %s", typ, ev.Message)
			}
		case <-deadline:
			t.Fatalf("no %s event", typ)
		}
	}
}

func TestATurnStreamsEventsAndPersistsBothSidesWithTheSession(t *testing.T) {
	p := &fakeProvider{key: "fake", script: []agent.Event{
		{Kind: agent.KindText, Text: "Reading.", SessionID: "s1"},
		{Kind: agent.KindTool, Tool: "Read", Input: map[string]any{"file_path": "README.md"}},
		{Kind: agent.KindText, Text: "Done."},
		{Kind: agent.KindCost, Cost: 0.25},
		{Kind: agent.KindResult, Text: "Done.", SessionID: "s1"},
	}}
	m, root, bus := newManager(t, p)
	ch, unsub := bus.Subscribe()
	defer unsub()
	id, err := m.Send("a.test", "look around", "")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, "agent.text")
	waitFor(t, ch, "agent.tool")
	done := waitFor(t, ch, "agent.done")
	if done.Subject != "a.test" || done.Data["turn_id"] != id || done.Data["cost"] != 0.25 {
		t.Fatalf("done = %+v", done)
	}
	req := p.last()
	if req.Workdir != filepath.Join(root, "sites", "a.test") || req.Model != "fake-1" || req.SessionID != "" {
		t.Fatalf("request = %+v", req)
	}
	if !strings.Contains(req.System, "a.test") || !strings.Contains(req.System, filepath.Join(root, "sites", "b.test")) {
		t.Fatalf("preamble does not name the site and the denied sibling:\n%s", req.System)
	}
	if req.Jail.Home != filepath.Join(root, "data", "homes", "a.test") {
		t.Fatalf("home = %s", req.Jail.Home)
	}
	c, err := m.History("a.test")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Turns) != 2 || c.Turns[0].Role != "user" || c.Turns[1].Role != "assistant" || c.Turns[1].Text != "Reading.\n\nDone." || c.Turns[1].Cost != 0.25 || c.SessionID != "s1" || c.Running {
		t.Fatalf("conversation = %+v", c)
	}
	if len(c.Turns[1].Tools) != 1 || c.Turns[1].Tools[0] != "Read" {
		t.Fatalf("tools = %v", c.Turns[1].Tools)
	}
	// The second turn resumes the session.
	if _, err := m.Send("a.test", "again", "other-model"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, "agent.done")
	if req := p.last(); req.SessionID != "s1" || req.Model != "other-model" {
		t.Fatalf("second request = %+v", req)
	}
	if got := m.opts.Ledger.Month("a.test", time.Now()); got < 0.49 || got > 0.51 {
		t.Fatalf("ledger month = %v", got)
	}
}

func TestOnlyOneTurnRunsPerSiteAtATimeAndStopEndsIt(t *testing.T) {
	hold := make(chan struct{})
	p := &fakeProvider{key: "fake", hold: hold, script: []agent.Event{
		{Kind: agent.KindText, Text: "working"},
		{Kind: agent.KindResult, Text: "ok"},
	}}
	m, _, bus := newManager(t, p)
	ch, unsub := bus.Subscribe()
	defer unsub()
	if _, err := m.Send("a.test", "one", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, "agent.text")
	if _, err := m.Send("a.test", "two", ""); err != ErrBusy {
		t.Fatalf("second send: %v", err)
	}
	if err := m.Reset("a.test"); err != ErrBusy {
		t.Fatalf("reset while running: %v", err)
	}
	// Another site is not blocked by this one.
	if _, err := m.Send("b.test", "hello", ""); err != nil {
		t.Fatalf("b.test blocked: %v", err)
	}
	if err := m.Stop("a.test"); err != nil {
		t.Fatal(err)
	}
	ev := waitFor(t, ch, "agent.error")
	if ev.Subject != "a.test" {
		t.Fatalf("error on %s", ev.Subject)
	}
	deadline := time.Now().Add(5 * time.Second)
	for m.Running("a.test") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if m.Running("a.test") {
		t.Fatal("still running after Stop")
	}
	c, _ := m.History("a.test")
	if last := c.Turns[len(c.Turns)-1]; last.Role != "error" {
		t.Fatalf("last turn = %+v", last)
	}
	close(hold)
	waitFor(t, ch, "agent.done")
}

func TestTheMonthlyCapRefusesANewTurn(t *testing.T) {
	p := &fakeProvider{key: "fake", script: []agent.Event{{Kind: agent.KindResult, Cost: 1.5}}}
	m, _, bus := newManager(t, p)
	ch, unsub := bus.Subscribe()
	defer unsub()
	if _, err := m.Send("a.test", "spend", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, "agent.done")
	_, err := m.Send("a.test", "more", "")
	if _, ok := err.(*agent.ErrOverCap); !ok {
		t.Fatalf("want ErrOverCap, got %v", err)
	}
	waitFor(t, ch, "agent.cost_cap")
}

func TestAJailViolationIsReportedAfterTheTurn(t *testing.T) {
	m, root, bus := newManager(t, &fakeProvider{key: "fake"})
	other := filepath.Join(root, "sites", "b.test", ".env")
	p := &fakeProvider{key: "fake", script: []agent.Event{
		{Kind: agent.KindTool, Tool: "Read", Input: map[string]any{"file_path": other}},
		{Kind: agent.KindResult, Text: "peeked"},
	}}
	m.opts.Registry.Register(p)
	ch, unsub := bus.Subscribe()
	defer unsub()
	if _, err := m.Send("a.test", "peek", ""); err != nil {
		t.Fatal(err)
	}
	ev := waitFor(t, ch, "agent.jail_violation")
	paths, _ := ev.Data["paths"].([]string)
	if ev.Level != "error" || len(paths) != 1 || paths[0] != filepath.Join(root, "sites", "b.test") {
		t.Fatalf("violation = %+v", ev)
	}
	// The violation is reported before the turn is saved; the temp dir
	// must not be removed under a save still in flight.
	waitFor(t, ch, "agent.done")
}

func TestTheMiddlewareConversationRunsInTheRootAndDeniesTheExistingSites(t *testing.T) {
	p := &fakeProvider{key: "fake", script: []agent.Event{{Kind: agent.KindResult, Text: "ok"}}}
	m, root, bus := newManager(t, p)
	ch, unsub := bus.Subscribe()
	defer unsub()
	if _, err := m.Send(Middleware, "what is running?", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, "agent.done")
	req := p.last()
	denied := strings.Join(req.Jail.Deny, "\n")
	if req.Workdir != root || !strings.Contains(denied, filepath.Join(root, "sites", "a.test")) || !strings.Contains(denied, filepath.Join(root, "sites", "b.test")) {
		t.Fatalf("request = %+v", req)
	}
	for _, d := range req.Jail.Deny {
		if d == filepath.Join(root, "sites") {
			t.Fatalf("the sites folder itself is denied: %v", req.Jail.Deny)
		}
	}
	if !strings.Contains(req.System, "installation itself") || !strings.Contains(req.System, "middleware site add") {
		t.Fatalf("preamble:\n%s", req.System)
	}
	exe, _ := os.Executable()
	if !strings.Contains(req.Env["PATH"], filepath.Dir(exe)) {
		t.Fatalf("PATH does not hold the binary's folder: %q", req.Env["PATH"])
	}
	if _, err := os.Stat(filepath.Join(root, "data", "chat", "middleware.json")); err != nil {
		t.Fatal(err)
	}
}

// creator is a provider that writes a site folder while its run is in
// progress, the way an agent creating a site through Write would, and
// reports the tool call in its transcript.
type creator struct {
	fakeProvider
	root string
}

func (c *creator) Start(ctx context.Context, req agent.Request) (agent.Run, error) {
	dir := filepath.Join(c.root, "sites", "new.test")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "site.json"), []byte(`{"version":2,"static":{"root":"."}}`), 0o644); err != nil {
		return nil, err
	}
	c.script = []agent.Event{
		{Kind: agent.KindTool, Tool: "Write", Input: map[string]any{"file_path": filepath.Join(dir, "site.json")}},
		{Kind: agent.KindResult, Text: "created"},
	}
	return c.fakeProvider.Start(ctx, req)
}

func TestTheHouseConversationMayCreateASiteButNotTouchAnExistingOne(t *testing.T) {
	m, root, bus := newManager(t, &fakeProvider{key: "fake"})
	os.Rename(filepath.Join(root, "sites", "b.test"), filepath.Join(root, "sites", "old.test"))
	c := &creator{fakeProvider: fakeProvider{key: "fake"}, root: root}
	m.opts.Registry.Register(c)
	ch, unsub := bus.Subscribe()
	defer unsub()
	if _, err := m.Send(Middleware, "create new.test", ""); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(10 * time.Second)
wait:
	for {
		select {
		case ev := <-ch:
			if ev.Type == "agent.jail_violation" {
				t.Fatalf("creating a site was flagged: %v", ev.Data["paths"])
			}
			if ev.Type == "agent.done" {
				break wait
			}
		case <-deadline:
			t.Fatal("the turn did not end")
		}
	}
	if _, err := os.Stat(filepath.Join(root, "sites", "new.test", "site.json")); err != nil {
		t.Fatal(err)
	}
	// The next turn sees new.test as an existing site, and touching
	// old.test was never allowed.
	p := &fakeProvider{key: "fake", script: []agent.Event{
		{Kind: agent.KindTool, Tool: "Edit", Input: map[string]any{"file_path": filepath.Join(root, "sites", "old.test", "index.html")}},
		{Kind: agent.KindResult, Text: "edited"},
	}}
	m.opts.Registry.Register(p)
	if _, err := m.Send(Middleware, "edit old.test", ""); err != nil {
		t.Fatal(err)
	}
	ev := waitFor(t, ch, "agent.jail_violation")
	paths, _ := ev.Data["paths"].([]string)
	if len(paths) != 1 || paths[0] != filepath.Join(root, "sites", "old.test") {
		t.Fatalf("violation = %+v", ev)
	}
	denied := strings.Join(p.last().Jail.Deny, "\n")
	if !strings.Contains(denied, filepath.Join(root, "sites", "new.test")) {
		t.Fatalf("the site created last turn is not denied this turn:\n%s", denied)
	}
	waitFor(t, ch, "agent.done")
}

func TestResetForgetsTheSessionAndUnknownDomainsAreRefused(t *testing.T) {
	p := &fakeProvider{key: "fake", script: []agent.Event{{Kind: agent.KindResult, Text: "ok", SessionID: "s9"}}}
	m, _, bus := newManager(t, p)
	ch, unsub := bus.Subscribe()
	defer unsub()
	if _, err := m.Send("a.test", "hi", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, "agent.done")
	if c, _ := m.History("a.test"); c.SessionID != "s9" {
		t.Fatalf("session = %q", c.SessionID)
	}
	if err := m.Reset("a.test"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, "chat.reset")
	c, err := m.History("a.test")
	if err != nil || len(c.Turns) != 0 || c.SessionID != "" || c.Provider != "fake" {
		t.Fatalf("after reset: %+v err=%v", c, err)
	}
	for _, bad := range []string{"nope.test", "../etc", "A.TEST", ""} {
		if _, err := m.Send(bad, "hi", ""); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	if _, err := m.Send("a.test", "   ", ""); err == nil {
		t.Fatal("empty message accepted")
	}
}

func TestARunningTurnShowsWhatItDidSoFarToWhoeverOpensItLater(t *testing.T) {
	hold := make(chan struct{})
	p := &fakeProvider{key: "fake", hold: hold, script: []agent.Event{
		{Kind: agent.KindText, Text: "working"},
		{Kind: agent.KindTool, Tool: "Bash"},
		{Kind: agent.KindResult, Text: "ok"},
	}}
	m, _, bus := newManager(t, p)
	ch, unsub := bus.Subscribe()
	defer unsub()
	if _, err := m.Send("a.test", "one", ""); err != nil {
		t.Fatal(err)
	}
	ev := waitFor(t, ch, "agent.tool")
	if ev.Data["seq"] != 2 {
		t.Fatalf("the stream's event has no seq: %v", ev.Data)
	}
	// A panel reloaded now: it reads the conversation, not the stream.
	c, _ := m.History("a.test")
	if !c.Running || len(c.Live) != 2 || c.Live[0].Type != "agent.text" || c.Live[0].Data["text"] != "working" || c.Live[1].Seq != 2 {
		t.Fatalf("running %v, live %+v", c.Running, c.Live)
	}
	close(hold)
	waitFor(t, ch, "agent.done")
	if c, _ := m.History("a.test"); c.Running || len(c.Live) != 0 || c.Turns[len(c.Turns)-1].Text != "working" {
		t.Fatalf("after the end: %+v", c)
	}
}

func TestATurnKeepsItsStepsInOrderWithEachToolsAnswerAndItsSubagents(t *testing.T) {
	p := &fakeProvider{key: "fake", script: []agent.Event{
		{Kind: agent.KindText, Text: "Looking."},
		{Kind: agent.KindTool, Tool: "Bash", ToolID: "b1", Input: map[string]any{"command": "ls"}},
		{Kind: agent.KindToolResult, ToolID: "b1", Output: "a\nb"},
		{Kind: agent.KindTool, Tool: "Task", ToolID: "t1", Input: map[string]any{"description": "check the logs", "prompt": "read them", "run_in_background": true}},
		{Kind: agent.KindToolResult, ToolID: "t1", Output: "launched"},
		{Kind: agent.KindText, Text: "sub says", Thread: "t1"},
		{Kind: agent.KindTool, Tool: "Read", ToolID: "r1", Input: map[string]any{"file_path": "x"}, Thread: "t1"},
		{Kind: agent.KindToolResult, ToolID: "r1", Output: "nope", IsError: true, Thread: "t1"},
		{Kind: agent.KindAgentStatus, ToolID: "t1", Status: "completed"},
		{Kind: agent.KindAgentStatus, ToolID: "b9", Status: "completed"},
		{Kind: agent.KindText, Text: "Done."},
		{Kind: agent.KindResult, Text: "Done."},
	}}
	m, _, bus := newManager(t, p)
	ch, unsub := bus.Subscribe()
	defer unsub()
	if _, err := m.Send("a.test", "go", ""); err != nil {
		t.Fatal(err)
	}
	ev := waitFor(t, ch, "agent.tool.result")
	if ev.Data["id"] != "b1" || ev.Data["output"] != "a\nb" {
		t.Fatalf("%v", ev.Data)
	}
	if ev := waitFor(t, ch, "agent.subagent"); ev.Data["id"] != "t1" || ev.Data["run"] != "completed" {
		t.Fatalf("%v", ev.Data)
	}
	waitFor(t, ch, "agent.done")
	c, _ := m.History("a.test")
	tn := c.Turns[len(c.Turns)-1]
	var kinds []string
	for _, s := range tn.Steps {
		k := s.Kind
		if s.Tool != nil {
			k += ":" + s.Tool.Name
		}
		kinds = append(kinds, k)
	}
	if strings.Join(kinds, " ") != "text tool:Bash tool:Task text" {
		t.Fatalf("steps %v", kinds)
	}
	if b := tn.Steps[1].Tool; !b.Done || b.Output != "a\nb" {
		t.Fatalf("bash %+v", b)
	}
	if task := tn.Steps[2].Tool; task.Run != "completed" {
		t.Fatalf("task %+v", task)
	}
	if len(tn.Threads) != 1 {
		t.Fatalf("threads %+v", tn.Threads)
	}
	th := tn.Threads[0]
	if th.Description != "check the logs" || !th.Background || th.Run != "completed" || len(th.Steps) != 2 || !th.Steps[1].Tool.Error {
		t.Fatalf("thread %+v", th)
	}
}

func TestWaitingCountsTheConversationsThatEndedAskingTheOperator(t *testing.T) {
	q := "Which one?\n\n```question\n{\"question\": \"Which?\", \"options\": [\"a\", \"b\"]}\n```"
	p := &fakeProvider{key: "fake", script: []agent.Event{{Kind: agent.KindText, Text: q}, {Kind: agent.KindResult}}}
	m, _, bus := newManager(t, p)
	ch, unsub := bus.Subscribe()
	defer unsub()
	if w := m.Waiting(); len(w.Asking) != 0 {
		t.Fatalf("before: %+v", w)
	}
	m.Send("a.test", "go", "")
	waitFor(t, ch, "agent.done")
	w := m.Waiting()
	if len(w.Asking) != 1 || w.Asking[0].Name != "a.test" {
		t.Fatalf("after a question: %+v", w)
	}
	p.script = []agent.Event{{Kind: agent.KindText, Text: "done"}, {Kind: agent.KindResult}}
	time.Sleep(10 * time.Millisecond) // a new mtime
	m.Send("a.test", "a", "")
	waitFor(t, ch, "agent.done")
	if w := m.Waiting(); len(w.Asking) != 0 {
		t.Fatalf("answered: %+v", w)
	}
}

// The conversations at work are listed too, for the panel's bar, and leave
// the list when their turn ends.
func TestWaitingListsTheConversationsAtWork(t *testing.T) {
	p := &fakeProvider{key: "fake", hold: make(chan struct{}), script: []agent.Event{{Kind: agent.KindText, Text: "done"}, {Kind: agent.KindResult}}}
	m, _, bus := newManager(t, p)
	ch, unsub := bus.Subscribe()
	defer unsub()
	m.Send("a.test", "go", "")
	deadline := time.Now().Add(5 * time.Second)
	for len(m.Waiting().Running) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	w := m.Waiting()
	if len(w.Running) != 1 || w.Running[0].Name != "a.test" || w.Running[0].Updated.IsZero() || w.Working != 1 {
		t.Fatalf("at work: %+v", w)
	}
	close(p.hold)
	waitFor(t, ch, "agent.done")
	if w := m.Waiting(); len(w.Running) != 0 {
		t.Fatalf("after the turn: %+v", w)
	}
}

func TestASitesConversationGetsItsPushSettingsAndTheHousesDoesNot(t *testing.T) {
	p := &fakeProvider{key: "fake", script: []agent.Event{{Kind: agent.KindResult, Text: "ok", SessionID: "s1"}}}
	m, _, bus := newManager(t, p)
	asked := []string{}
	m.SetTurnEnv(func(domain string) map[string]string {
		asked = append(asked, domain)
		return map[string]string{"GIT_CONFIG_COUNT": "1", "PATH": "/nowhere"}
	})
	ch, unsub := bus.Subscribe()
	defer unsub()
	if _, err := m.Send("a.test", "push it", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, "agent.done")
	req := p.last()
	if req.Env["GIT_CONFIG_COUNT"] != "1" {
		t.Fatalf("the site's turn has no push settings: %v", req.Env)
	}
	if req.Env["PATH"] == "/nowhere" {
		t.Fatal("the turn's own variables were overwritten")
	}
	if _, err := m.Send(Middleware, "hello", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, "agent.done")
	if _, ok := p.last().Env["GIT_CONFIG_COUNT"]; ok || len(asked) != 1 {
		t.Fatalf("the house's conversation got a site's push settings (asked for %v)", asked)
	}
}
