package crew

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/agent"
	"github.com/dcimorra/the-middleware/internal/chat"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/secrets"
)

type fakeChat struct {
	mu       sync.Mutex
	sessions int
	prompts  []string
	running  bool
}

func newCrew(t *testing.T, now *time.Time, usage []agent.Window) (*Crew, *fakeChat, *events.Bus) {
	t.Helper()
	box, err := secrets.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	fc := &fakeChat{}
	bus := events.New("")
	c := New(Options{
		Root: t.TempDir(), Bus: bus, Box: box,
		NewSession: func(id string) (string, error) {
			fc.mu.Lock()
			defer fc.mu.Unlock()
			fc.sessions++
			return chat.CrewDomain(id) + "#a" + strings.Repeat("0", fc.sessions), nil
		},
		Send: func(name, prompt, provider, model string) (string, error) {
			fc.mu.Lock()
			defer fc.mu.Unlock()
			fc.prompts = append(fc.prompts, prompt)
			fc.running = true
			return "t" + string(rune('0'+len(fc.prompts))), nil
		},
		Running: func(string) bool { fc.mu.Lock(); defer fc.mu.Unlock(); return fc.running },
		Stop:    func(string) error { return nil },
		Usage:   func(string) []agent.Window { return usage },
		Now:     func() time.Time { return *now },
	})
	return c, fc, bus
}

func (c *Crew) end(t *testing.T, fc *fakeChat, id, text string) {
	t.Helper()
	st, _ := c.Get(id)
	fc.mu.Lock()
	fc.running = false
	fc.mu.Unlock()
	c.Finished(events.Event{Type: "agent.done", Subject: st.State.Session, Data: map[string]any{"turn_id": st.State.Turn, "text": text, "context": 1000, "window": 200000}})
}

// An agent is made off, cannot be switched on without a goal, wakes at
// once when it is, is told its goal and what it lacks, and sleeps for
// what its turn's Next: line says, within its bounds.
func TestAnAgentWakesTowardItsGoalAndSleepsAsItSays(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	c, fc, _ := newCrew(t, &now, nil)
	a, err := c.Create(Agent{Name: "Ayudante de Contenido"})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != "ayudante-de-contenido" || a.Enabled {
		t.Fatalf("created: %+v", a)
	}
	if b, _ := c.Create(Agent{Name: "Ayudante de Contenido"}); b.ID != "ayudante-de-contenido-2" {
		t.Errorf("a second one of the same name: %q", b.ID)
	}
	if err := c.SetEnabled(a.ID, true); err == nil {
		t.Error("switched on with no goal")
	}
	if _, err := c.Update(a.ID, Agent{Name: a.Name, Goal: "Write one useful post a day."}); err != nil {
		t.Fatal(err)
	}
	if err := c.SetEnabled(a.ID, true); err != nil {
		t.Fatal(err)
	}
	c.Pass()
	if len(fc.prompts) != 1 {
		t.Fatalf("turns started: %d", len(fc.prompts))
	}
	p := fc.prompts[0]
	for _, want := range []string{"Write one useful post a day.", "Every wake is a new conversation", "Your vault is empty", "Next:"} {
		if !strings.Contains(p, want) {
			t.Errorf("the prompt does not say %q:\n%s", want, p)
		}
	}
	c.Pass()
	if len(fc.prompts) != 1 {
		t.Error("a second turn started while one runs")
	}
	c.end(t, fc, a.ID, "Did the first thing.\nSummary: read the goal\nNext: 2h, to check the replies")
	st, _ := c.Get(a.ID)
	if st.State.Turn != "" || !st.State.NextWake.Equal(now.Add(2*time.Hour)) || st.State.LastResult != "read the goal" || st.ContextPercent != 0 {
		t.Errorf("after the turn: %+v", st.State)
	}
	now = now.Add(time.Hour)
	c.Pass()
	if len(fc.prompts) != 1 {
		t.Error("woken before its time")
	}
	now = now.Add(61 * time.Minute)
	c.Pass()
	if len(fc.prompts) != 2 {
		t.Errorf("not woken at its time: %d", len(fc.prompts))
	}
	// Each wake in a conversation of its own: nothing of the last one
	// rides along, only what it chose to write in its notebook.
	if st, _ := c.Get(a.ID); fc.sessions != 2 || !strings.HasSuffix(st.State.Session, "#a00") {
		t.Errorf("sessions opened: %d, now in %s", fc.sessions, st.State.Session)
	}
	// Off: a turn at work ends, and nothing else starts.
	c.end(t, fc, a.ID, "Summary: x\nNext: 10m, y")
	_ = c.SetEnabled(a.ID, false)
	now = now.Add(24 * time.Hour)
	c.Pass()
	if len(fc.prompts) != 2 {
		t.Error("woken while off")
	}
	// Survives a restart.
	again := New(Options{Root: c.opts.Root, Now: c.opts.Now})
	if st, err := again.Get(a.ID); err != nil || st.State.Turns != 2 || st.Agent.Goal != "Write one useful post a day." {
		t.Errorf("after a restart: %+v %v", st, err)
	}
}

// Above its share of a window it is not woken, and says why once.
func TestTheSubscriptionHoldsAnAgentBack(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	c, fc, _ := newCrew(t, &now, []agent.Window{{Name: "five_hour", Used: 91, ResetsAt: now.Add(time.Hour)}})
	a, _ := c.Create(Agent{Name: "x", Goal: "g"})
	_ = c.SetEnabled(a.ID, true)
	c.Pass()
	if len(fc.prompts) != 0 {
		t.Fatal("woken above the window's share")
	}
	st, _ := c.Get(a.ID)
	if !strings.Contains(st.State.Skipped, "91%") || !st.State.NextWake.Equal(now.Add(time.Hour)) {
		t.Errorf("held back: %+v", st.State)
	}
}

// A request reaches the operator (urgent: critical, and again after an
// hour); what they type into its fields goes to the vault encrypted, the
// turn gets it in its environment, and the agent is woken within a minute
// and told what was answered.
func TestARequestIsAnsweredIntoTheVaultAndWakesTheAgent(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	c, fc, bus := newCrew(t, &now, nil)
	sub, stop := bus.Subscribe()
	defer stop()
	a, _ := c.Create(Agent{Name: "x", Goal: "g"})
	_ = c.SetEnabled(a.ID, true)
	c.Pass()
	r, err := c.Ask(a.ID, Request{Title: "A code was sent to your phone", Text: "Paste it.", Fields: []Field{{Key: "sms_code", Secret: true}}, Urgent: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Ask(a.ID, Request{Title: "A code was sent to your phone"}); err == nil {
		t.Error("the same request twice")
	}
	if _, err := c.Ask(a.ID, Request{Title: "t", Fields: []Field{{Key: "PATH"}}}); err == nil {
		t.Error("a vault name the turn's environment is built with")
	}
	seen := func(want string, again bool) {
		t.Helper()
		deadline := time.After(2 * time.Second)
		for {
			select {
			case e := <-sub:
				if e.Type == want && (e.Data["again"] == again) {
					return
				}
			case <-deadline:
				t.Fatalf("no %s event (again %v)", want, again)
			}
		}
	}
	seen("crew.request", false)
	c.end(t, fc, a.ID, "Summary: waiting\nNext: 6h, nothing to do until then")
	now = now.Add(61 * time.Minute)
	c.Pass()
	seen("crew.request", true)
	if len(fc.prompts) != 1 {
		t.Fatal("woken with nothing answered")
	}
	if _, err := c.Answer(a.ID, r.ID, Reply{Values: map[string]string{"OTHER": "x"}}); err == nil {
		t.Error("a value for a field the request does not have")
	}
	if _, err := c.Answer(a.ID, r.ID, Reply{Answer: "here it is", Values: map[string]string{"SMS_CODE": "482913"}}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(c.opts.Root, "data", "crew", a.ID, "vault.json"))
	if strings.Contains(string(raw), "482913") || !strings.Contains(string(raw), "SMS_CODE") {
		t.Errorf("the vault on disk: %s", raw)
	}
	if env := c.Env(a.ID); env["SMS_CODE"] != "482913" {
		t.Errorf("the turn's environment: %v", env)
	}
	c.Pass()
	if len(fc.prompts) != 2 {
		t.Fatal("not woken by the answer")
	}
	for _, want := range []string{"answered your request", "Now in your vault: SMS_CODE", "here it is"} {
		if !strings.Contains(fc.prompts[1], want) {
			t.Errorf("the wake does not say %q:\n%s", want, fc.prompts[1])
		}
	}
	if strings.Contains(fc.prompts[1], "482913") {
		t.Error("the secret is in the prompt")
	}
	c.end(t, fc, a.ID, "Summary: used it\nNext: 6h, later")
	now = now.Add(2 * time.Minute)
	c.Pass()
	if len(fc.prompts) != 2 {
		t.Error("woken twice for one answer")
	}
}

func TestSlugs(t *testing.T) {
	for in, want := range map[string]string{"Instagram growth": "instagram-growth", "Señor Café!": "senor-cafe", "  ": "agent", "2nd one": "a-2nd-one", "Ayudante 🤖 ya": "ayudante-ya"} {
		if got := Slug(in); got != want || !chat.ValidCrewID(got) {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

// The network's agent is in the crew from the start for its requests and
// vault, but its own engine wakes it: the crew never sends it a turn,
// nor switches, wakes or deletes it; an answer is handed to that engine,
// and the text its next wake carries marks the answer as told.
func TestTheNetworksAgentIsOneOfTheCrewWithItsOwnEngine(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	c, fc, _ := newCrew(t, &now, nil)
	woke := make(chan string, 1)
	c.opts.OnAnswer = func(id string) { woke <- id }
	if !c.Has(Network) {
		t.Fatal("no network member")
	}
	for name, err := range map[string]error{
		"on": c.SetEnabled(Network, true), "wake": c.Wake(Network), "stop": c.Halt(Network), "delete": c.Delete(Network),
	} {
		if !errors.Is(err, ErrNetwork) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if a, err := c.Create(Agent{Name: "network", Goal: "g"}); err == nil && a.ID == Network {
		t.Error("a new agent took the network's id")
	}
	r, err := c.Ask(Network, Request{Title: "A token for the new zone", Fields: []Field{{Key: "CF_TOKEN", Secret: true}}})
	if err != nil {
		t.Fatal(err)
	}
	c.Pass()
	if len(fc.prompts) != 0 {
		t.Fatal("the crew sent the network's agent a turn")
	}
	if !strings.Contains(c.AskText(Network), "A token for the new zone") {
		t.Error("its open request is not in its wake's text")
	}
	if _, err := c.Answer(Network, r.ID, Reply{Answer: "done", Values: map[string]string{"CF_TOKEN": "s3cret"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-woke:
		if id != Network {
			t.Errorf("woke %q", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the answer did not wake its engine")
	}
	text := c.AskText(Network)
	if !strings.Contains(text, "CF_TOKEN") || strings.Contains(text, "s3cret") {
		t.Errorf("its wake's text: %s", text)
	}
	if reqs, _ := c.Requests(Network); len(reqs) != 1 || !reqs[0].Told {
		t.Errorf("the answer is not marked told: %+v", reqs)
	}
	if strings.Contains(c.AskText(Network), "answered your request") {
		t.Error("the answer is told twice")
	}
}

// Starting a turn calls back into the crew (the chat asks for the agent's
// vault and settings), and a wake once waited on itself there for ever:
// the crew froze with every page that reads it. Here the chat calls back,
// and the list is read while the turn is being started.
func TestAWakeDoesNotHoldTheCrewWhileTheChatStartsTheTurn(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	c, fc, _ := newCrew(t, &now, nil)
	send := c.opts.Send
	inSend := make(chan struct{})
	release := make(chan struct{})
	c.opts.Send = func(name, prompt, provider, model string) (string, error) {
		id, _ := chat.CrewID(strings.SplitN(name, "#", 2)[0])
		c.Env(id)
		c.Settings(id)
		close(inSend)
		<-release
		return send(name, prompt, provider, model)
	}
	newSession := c.opts.NewSession
	c.opts.NewSession = func(id string) (string, error) {
		c.Settings(id)
		return newSession(id)
	}
	a, _ := c.Create(Agent{Name: "x", Goal: "g"})
	if err := c.SetEnabled(a.ID, true); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { c.Pass(); close(done) }()
	select {
	case <-inSend:
	case <-time.After(3 * time.Second):
		t.Fatal("the chat's call back into the crew never returned")
	}
	listed := make(chan int)
	go func() { listed <- len(c.List()); c.Pass() }()
	select {
	case n := <-listed:
		if n != 2 {
			t.Errorf("listed %d", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the list waits on a turn being started")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the wake never finished")
	}
	st, _ := c.Get(a.ID)
	fc.mu.Lock()
	n := len(fc.prompts)
	fc.mu.Unlock()
	if st.State.Turn == "" || n != 1 {
		t.Errorf("turn %q, %d prompts: a second pass started another", st.State.Turn, n)
	}
}

// An agent's room (its desk and home) is weighed, shown, and its wake
// told to make room first once it is over what the operator gave it.
func TestAnAgentIsToldWhenItsRoomIsFull(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	c, fc, _ := newCrew(t, &now, nil)
	a, _ := c.Create(Agent{Name: "x", Goal: "g", DiskMB: 1})
	if err := os.WriteFile(filepath.Join(c.Desk(a.ID), "big"), make([]byte, 3<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = c.SetEnabled(a.ID, true)
	c.Pass()
	if len(fc.prompts) != 1 || !strings.Contains(fc.prompts[0], "Your room is FULL: your desk and home hold 3 MiB, over the 1 MiB") {
		t.Fatalf("the wake does not say the room is full: %v", fc.prompts)
	}
	if st, _ := c.Get(a.ID); st.DiskUsed != 3 {
		t.Errorf("disk used %d", st.DiskUsed)
	}
	if _, err := c.Create(Agent{Name: "y", Goal: "g", DiskMB: -1}); err == nil {
		t.Error("a negative room")
	}
}
