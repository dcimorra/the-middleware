package cli

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/agent"
	"github.com/dcimorra/the-middleware/internal/api"
	"github.com/dcimorra/the-middleware/internal/auth"
	"github.com/dcimorra/the-middleware/internal/chat"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/settings"
	"github.com/dcimorra/the-middleware/internal/sites"
)

// loggedIn stands for the Claude module as the supervisor builds it: it
// holds the login (a token only the supervisor's registry has), and a turn
// started without it answers what the real CLI answered.
type loggedIn struct {
	token string
	mu    sync.Mutex
	reqs  []agent.Request
}

func (p *loggedIn) Key() string               { return "claude" }
func (p *loggedIn) Available() (bool, string) { return true, "" }
func (p *loggedIn) Models() []string          { return []string{"sonnet"} }
func (p *loggedIn) Start(ctx context.Context, req agent.Request) (agent.Run, error) {
	p.mu.Lock()
	p.reqs = append(p.reqs, req)
	p.mu.Unlock()
	script := []agent.Event{{Kind: agent.KindError, Text: "Not logged in · Please run /login"}}
	if p.token != "" {
		script = []agent.Event{
			{Kind: agent.KindText, Text: "Brief received: " + req.Prompt, SessionID: "s1"},
			{Kind: agent.KindTool, Tool: "Read", Input: map[string]any{"file_path": "site.json"}},
			{Kind: agent.KindCost, Cost: 0.5},
			{Kind: agent.KindResult, Text: "done", SessionID: "s1"},
		}
	}
	ch := make(chan agent.Event)
	go func() {
		defer close(ch)
		for _, ev := range script {
			select {
			case ch <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return scriptRun(ch), nil
}

type scriptRun <-chan agent.Event

func (r scriptRun) Events() <-chan agent.Event { return r }
func (r scriptRun) Stop()                      {}

// The house conversation sent a site its brief with `middleware chat`, and
// the site answered "Not logged in" while the panel worked on the same
// site: the command ran the turn itself, with providers that had none of
// the module's login. This sends through a real server whose registry is
// the only one with the login, and the turn must be the server's.
func TestChatSendsTheTurnThroughTheSupervisorWithItsLogin(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "sites", "book.test"), 0o755)
	os.MkdirAll(filepath.Join(root, "data"), 0o755)
	bus := events.New("")
	cur := settings.Defaults()
	srv := api.New(api.Options{Root: root, Version: "test", Auth: auth.New(root, bus), Sites: sites.New(root, nil, bus), Bus: bus,
		Settings: func() settings.Settings { return cur }})
	srv.Heartbeat = 50 * time.Millisecond
	p := &loggedIn{token: "the module's login"}
	reg := agent.NewRegistry()
	reg.Register(p)
	m := chat.New(chat.Options{Root: root, Bus: bus, Registry: reg, DefaultProvider: "claude"})
	srv.RegisterAgents(api.AgentsOptions{Registry: reg, Chat: m})
	sock := api.SocketPath(root)
	ln, err := api.ListenSocket(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	hs := &http.Server{Handler: srv.Panel(), BaseContext: func(net.Listener) context.Context { return auth.WithSocket(ctx) }}
	go hs.Serve(ln)
	t.Cleanup(func() { cancel(); hs.Close() })
	cl := SocketClient(sock)

	out, err := run(t, cl, "chat", "book.test", "build", "the", "platform")
	if err != nil {
		t.Fatalf("chat: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Brief received: build the platform") || !strings.Contains(out, "[Read]") || !strings.Contains(out, "cost: $0.5000") {
		t.Fatalf("output:\n%s", out)
	}
	p.mu.Lock()
	n := len(p.reqs)
	p.mu.Unlock()
	if n != 1 {
		t.Fatalf("the server's provider started %d turns, want 1", n)
	}
	// The conversation is the panel's one: the panel sees both sides.
	c, err := m.Conversation("book.test")
	if err != nil || len(c.Turns) == 0 {
		t.Fatalf("conversation: %v %+v", err, c)
	}

	// A turn that fails says why, and the command fails with it.
	p.token = ""
	out, err = run(t, cl, "chat", "book.test", "again")
	if err == nil || !strings.Contains(err.Error(), "Not logged in") {
		t.Fatalf("a failed turn: %v\n%s", err, out)
	}

	if _, err := run(t, cl, "chat", "book.test"); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("no text: %v", err)
	}
}
