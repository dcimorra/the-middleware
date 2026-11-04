package chat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dcimorra/the-middleware/internal/agent"
)

// What Daniel saw: a turn that began and stopped with no reply, its cost
// shown and nothing else, and the same message written again worked. The
// house now writes it again itself, once, on the same session.
func TestAnEmptyAnswerIsAskedAgainOnceOnTheSameSession(t *testing.T) {
	p := &fakeProvider{key: "fake", scripts: [][]agent.Event{
		{{Kind: agent.KindCost, Cost: 0.25, SessionID: "s1"}, {Kind: agent.KindResult, Text: "", Cost: 0.25, SessionID: "s1"}},
		{{Kind: agent.KindTool, Tool: "Bash", ToolID: "t1", Input: map[string]any{"command": "git push"}, SessionID: "s1"}, {Kind: agent.KindText, Text: "Pushed.", SessionID: "s1"}, {Kind: agent.KindResult, Text: "Pushed.", Cost: 0.125, SessionID: "s1"}},
	}}
	m, root, bus := newManager(t, p)
	ch, unsub := bus.Subscribe()
	defer unsub()
	if _, err := m.Send("a.test", "Commit & push", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, "agent.empty")
	done := waitFor(t, ch, "agent.done")
	if done.Data["text"] != "Pushed." || done.Data["cost"] != 0.375 {
		t.Fatalf("done = %+v", done.Data)
	}
	p.mu.Lock()
	reqs := append([]agent.Request(nil), p.reqs...)
	p.mu.Unlock()
	if len(reqs) != 2 {
		t.Fatalf("%d runs, want 2", len(reqs))
	}
	if r := reqs[1]; r.SessionID != "s1" || !strings.HasPrefix(r.Prompt, EmptyRetry) || !strings.HasSuffix(r.Prompt, "Commit & push") || r.Workdir != reqs[0].Workdir || r.System != reqs[0].System {
		t.Fatalf("second run = %+v", r)
	}
	c, _ := m.History("a.test")
	if len(c.Turns) != 2 || c.Turns[1].Role != "assistant" || c.Turns[1].Text != "Pushed." || c.Turns[1].Cost != 0.375 || len(c.Turns[1].Tools) != 1 {
		t.Fatalf("turns = %+v", c.Turns)
	}
	if got := m.opts.Ledger.Month("a.test", c.Turns[1].At); got != 0.375 {
		t.Fatalf("ledger = %v", got)
	}
	kept, _ := os.ReadDir(filepath.Join(root, "data", "chat", "empty"))
	if len(kept) != 1 {
		t.Fatalf("kept %d empty streams, want 1", len(kept))
	}
}

// Asked again only once: an agent that stays silent ends its turn, it is
// not asked for ever.
func TestAnAnswerEmptyTwiceEndsTheTurn(t *testing.T) {
	p := &fakeProvider{key: "fake", scripts: [][]agent.Event{
		{{Kind: agent.KindResult, Text: "", SessionID: "s1"}},
	}}
	m, _, bus := newManager(t, p)
	ch, unsub := bus.Subscribe()
	defer unsub()
	if _, err := m.Send("a.test", "hello", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, "agent.done")
	p.mu.Lock()
	n := len(p.reqs)
	p.mu.Unlock()
	if n != 2 {
		t.Fatalf("%d runs, want 2", n)
	}
	if m.Running("a.test") {
		t.Fatal("still running")
	}
}

// A turn that said something is never run twice, nor one that only used
// tools.
func TestAnAnswerWithToolsOnlyIsNotAskedAgain(t *testing.T) {
	p := &fakeProvider{key: "fake", script: []agent.Event{
		{Kind: agent.KindTool, Tool: "Bash", ToolID: "t1", Input: map[string]any{"command": "true"}},
		{Kind: agent.KindResult, Text: "", SessionID: "s1"},
	}}
	m, _, bus := newManager(t, p)
	ch, unsub := bus.Subscribe()
	defer unsub()
	if _, err := m.Send("a.test", "run it", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, "agent.done")
	p.mu.Lock()
	n := len(p.reqs)
	p.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d runs, want 1", n)
	}
}
