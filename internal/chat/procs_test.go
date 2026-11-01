package chat

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/agent"
	"github.com/dcimorra/the-middleware/internal/fsx"
)

// What a turn leaves behind with setsid escapes the group kill. The mark in
// its environment is how the house still lists it and ends it, and a pid
// with another conversation's mark (or none) is refused.
func TestWhatATurnLeftBehindIsListedAndReaped(t *testing.T) {
	p := &fakeProvider{key: "fake", script: []agent.Event{{Kind: agent.KindResult, Text: "ok"}}}
	m, _, bus := newManager(t, p)
	ch, unsub := bus.Subscribe()
	defer unsub()
	if _, err := m.Send("a.test", "hi", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, "agent.done")
	if p.last().Env[MarkVar] != "a.test" {
		t.Fatalf("the turn's environment has no mark: %v", p.last().Env)
	}
	start := func(mark string) *exec.Cmd {
		cmd := exec.Command("sleep", "60")
		cmd.Env = append(os.Environ(), MarkVar+"="+mark)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		go cmd.Wait()
		t.Cleanup(func() { cmd.Process.Kill() })
		return cmd
	}
	ours := start("a.test")
	theirs := start("b.test")
	// a.test.example would match a prefix search; the mark is whole.
	near := start("a.test.example")
	time.Sleep(100 * time.Millisecond)
	procs := m.Processes("a.test")
	if len(procs) != 1 || procs[0].PID != ours.Process.Pid {
		t.Fatalf("listed %+v", procs)
	}
	if m.Kill("a.test", theirs.Process.Pid) {
		t.Fatal("killed another conversation's process")
	}
	if n := m.Reap("a.test"); n != 1 {
		t.Fatalf("reaped %d", n)
	}
	time.Sleep(200 * time.Millisecond)
	if syscall.Kill(ours.Process.Pid, 0) == nil && !zombie(ours.Process.Pid) {
		t.Fatal("still alive after the reap")
	}
	for _, c := range []*exec.Cmd{theirs, near} {
		if syscall.Kill(c.Process.Pid, 0) != nil {
			t.Fatal("a process that was not this conversation's was killed")
		}
	}
}

func TestATurnIsCutAtItsLimit(t *testing.T) {
	defer func(d time.Duration) { TurnLimit = d }(TurnLimit)
	TurnLimit = 300 * time.Millisecond
	hold := make(chan struct{})
	defer close(hold)
	p := &fakeProvider{key: "fake", script: []agent.Event{{Kind: agent.KindText, Text: "working"}, {Kind: agent.KindResult}}, hold: hold}
	m, _, bus := newManager(t, p)
	ch, unsub := bus.Subscribe()
	defer unsub()
	if _, err := m.Send("a.test", "loop forever", ""); err != nil {
		t.Fatal(err)
	}
	ev := waitFor(t, ch, "agent.turn_limit")
	if ev.Subject != "a.test" {
		t.Fatalf("%+v", ev)
	}
	deadline := time.Now().Add(5 * time.Second)
	for m.Running("a.test") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if m.Running("a.test") {
		t.Fatal("the turn still holds its slot")
	}
}

// A lent folder is readable and not writable: it leaves the deny list and
// the adapter closes it to every writing tool.
func TestALentFolderIsReadNotWritten(t *testing.T) {
	p := &fakeProvider{key: "fake", script: []agent.Event{{Kind: agent.KindResult, Text: "ok"}}}
	m, root, bus := newManager(t, p)
	os.WriteFile(root+"/sites/a.test/site.json", []byte(`{"version":2,"chat":{"reads":["b.test","missing.test"]}}`), 0o644)
	ch, unsub := bus.Subscribe()
	defer unsub()
	if _, err := m.Send("a.test", "read b", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, "agent.done")
	j := p.last().Jail
	lent := root + "/sites/b.test"
	if len(j.Lent) != 1 || j.Lent[0] != lent {
		t.Fatalf("lent: %v", j.Lent)
	}
	for _, d := range j.Deny {
		if d == lent {
			t.Fatal("the lent folder is still denied")
		}
	}
}

// A message sent while a turn runs is kept and sent when it ends.
func TestAMessageDuringATurnIsSentWhenItEnds(t *testing.T) {
	hold := make(chan struct{})
	p := &fakeProvider{key: "fake", script: []agent.Event{{Kind: agent.KindText, Text: "working"}, {Kind: agent.KindResult}}, hold: hold}
	m, _, bus := newManager(t, p)
	ch, unsub := bus.Subscribe()
	defer unsub()
	if _, err := m.Send("a.test", "first", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SendWith("a.test", "also this", SendOptions{Queue: true}); err != ErrQueued {
		t.Fatalf("queue: %v", err)
	}
	if _, err := m.SendWith("a.test", "and this", SendOptions{Queue: true}); err != ErrQueued {
		t.Fatalf("queue: %v", err)
	}
	// A panel that comes back meanwhile sees both, in order, and the
	// house has them on disk for a restart.
	c, _ := m.History("a.test")
	if len(c.Queued) != 2 || c.Queued[0].Text != "also this" || c.Queued[1].Text != "and this" {
		t.Fatalf("history queued: %+v", c.Queued)
	}
	var q queueFile
	if err := fsx.ReadJSON(m.queuePath("a.test"), &q); err != nil || len(q.Messages) != 2 || q.Messages[1].Text != "and this" {
		t.Fatalf("queue file: %+v %v", q, err)
	}
	close(hold)
	waitFor(t, ch, "agent.done")
	waitFor(t, ch, "agent.started")
	if p.last().Prompt != "also this\n\nand this" {
		t.Fatalf("next prompt %q", p.last().Prompt)
	}
	if _, err := os.Stat(m.queuePath("a.test")); !os.IsNotExist(err) {
		t.Fatalf("the queue file outlived its turn: %v", err)
	}
	if c, _ := m.History("a.test"); len(c.Queued) != 0 {
		t.Fatalf("still queued: %+v", c.Queued)
	}
	waitFor(t, ch, "agent.done")
}

// What waited for a turn that a restart lost is sent by the next
// supervisor, in the order it was written.
func TestAQueueSurvivesARestart(t *testing.T) {
	p := &fakeProvider{key: "fake", script: []agent.Event{{Kind: agent.KindResult, Text: "ok"}}}
	m, _, bus := newManager(t, p)
	ch, unsub := bus.Subscribe()
	defer unsub()
	if err := fsx.WriteJSON(m.queuePath("a.test"), queueFile{Messages: []Queued{{Text: "one"}, {Text: "two"}}}, 0o600); err != nil {
		t.Fatal(err)
	}
	m.Recover(context.Background())
	waitFor(t, ch, "agent.done")
	if p.last().Prompt != "one\n\ntwo" {
		t.Fatalf("prompt %q", p.last().Prompt)
	}
	if _, err := os.Stat(m.queuePath("a.test")); !os.IsNotExist(err) {
		t.Fatalf("the queue file is still there: %v", err)
	}
}

// An attachment goes with the next message, readable and not writable,
// and moves out of the way when the turn ends.
func TestAnAttachmentGoesWithTheNextMessage(t *testing.T) {
	p := &fakeProvider{key: "fake", script: []agent.Event{{Kind: agent.KindResult, Text: "ok"}}}
	m, root, bus := newManager(t, p)
	ch, unsub := bus.Subscribe()
	defer unsub()
	if _, err := m.AddUpload("a.test", "../../etc/passwd", strings.NewReader("x")); err != nil {
		// A path is reduced to its base name; ".." itself is refused.
		t.Logf("refused: %v", err)
	}
	if _, err := m.AddUpload("a.test", ".env", strings.NewReader("x")); err == nil {
		t.Fatal("a dotfile was accepted")
	}
	if _, err := m.AddUpload("a.test", "notes.txt", strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Send("a.test", "read the notes", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, "agent.done")
	req := p.last()
	pending := root + "/data/homes/a.test/uploads/main/pending/notes.txt"
	if !strings.Contains(req.Prompt, pending) {
		t.Fatalf("prompt: %s", req.Prompt)
	}
	if len(req.Jail.Lent) == 0 || !strings.HasSuffix(req.Jail.Lent[len(req.Jail.Lent)-1], "uploads/main") {
		t.Fatalf("lent: %v", req.Jail.Lent)
	}
	if len(m.Uploads("a.test")) != 0 {
		t.Fatal("the attachment is still pending after the turn")
	}
}

// What a subagent says is kept apart from the main answer.
func TestASubagentsWordsAreKeptApart(t *testing.T) {
	p := &fakeProvider{key: "fake", script: []agent.Event{
		{Kind: agent.KindText, Text: "let me delegate"},
		{Kind: agent.KindText, Text: "sub: found it", Thread: "task1"},
		{Kind: agent.KindTool, Tool: "Grep", Thread: "task1"},
		{Kind: agent.KindText, Text: "done"},
		{Kind: agent.KindResult},
	}}
	m, _, bus := newManager(t, p)
	ch, unsub := bus.Subscribe()
	defer unsub()
	m.Send("a.test", "go", "")
	waitFor(t, ch, "agent.done")
	c, _ := m.History("a.test")
	last := c.Turns[len(c.Turns)-1]
	if last.Text != "let me delegate\n\ndone" || len(last.Threads) != 1 || last.Threads[0].Text != "sub: found it" || last.Threads[0].Tools[0] != "Grep" {
		t.Fatalf("%+v", last)
	}
}
