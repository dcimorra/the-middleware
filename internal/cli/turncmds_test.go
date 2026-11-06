package cli

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/api"
	"github.com/dcimorra/the-middleware/internal/auth"
	"github.com/dcimorra/the-middleware/internal/backups"
	"github.com/dcimorra/the-middleware/internal/chat"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/security"
	"github.com/dcimorra/the-middleware/internal/seen"
	"github.com/dcimorra/the-middleware/internal/settings"
	"github.com/dcimorra/the-middleware/internal/sites"
	"github.com/dcimorra/the-middleware/internal/tasks"
)

// turnServer is realServer with every connection taken for a turn of
// site's conversation, as SocketServer marks one whose process carries
// that site's mark: the caller a site's agent is when it runs the CLI.
func turnServer(t *testing.T, site string) (root string, cl *Client) {
	t.Helper()
	root = t.TempDir()
	os.MkdirAll(filepath.Join(root, "sites", site), 0o755)
	os.WriteFile(filepath.Join(root, "sites", site, "site.json"), []byte(`{"version": 2, "static": {"root": "."}}`), 0o644)
	os.MkdirAll(filepath.Join(root, "sites", "other.test"), 0o755)
	os.WriteFile(filepath.Join(root, "sites", "other.test", "site.json"), []byte(`{"version": 2, "static": {"root": "."}}`), 0o644)
	bus := events.New("")
	cur := settings.Defaults()
	srv := api.New(api.Options{Root: root, Version: "test", Auth: auth.New(root, bus), Sites: sites.New(root, nil, bus), Bus: bus,
		Settings: func() settings.Settings { return cur }})
	srv.Heartbeat = 50 * time.Millisecond
	srv.RegisterTasks(api.TasksOptions{Scheduler: tasks.NewScheduler(tasks.Options{Root: root, Bus: bus}), Root: root})
	srv.RegisterBackups(backups.New(backups.Options{Root: root, Bus: bus}))
	srv.RegisterSecurity(api.SecurityOptions{Door: security.New(security.Options{Lookup: func(string) (security.Site, bool) { return security.Site{}, false }})})
	srv.RegisterSeen(seen.New(filepath.Join(root, "data", "seen")))
	ln, err := api.ListenSocket(api.SocketPath(root))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	hs := &http.Server{Handler: srv.Panel(), BaseContext: func(net.Listener) context.Context { return auth.WithSocketTurn(ctx, site) }}
	go hs.Serve(ln)
	t.Cleanup(func() { cancel(); hs.Close() })
	return root, SocketClient(api.SocketPath(root))
}

// Every command the preamble promises a site's conversation is run here
// as that conversation's turn runs it, and none may be refused for want
// of permission: the preamble now tells the agent to try these before
// saying it cannot, and one the house refuses would teach it the opposite.
// A command may still fail for its own reasons (a task not running, a
// site with nothing to deploy); what it may not say is "the operator's".
func TestEveryCommandASiteConversationIsToldAboutIsOpenToItsTurn(t *testing.T) {
	_, cl := turnServer(t, "a.test")
	tries := []struct {
		args  string // the TurnCommand it stands for
		stdin string
		try   []string
	}{
		{"site show <domain>", "", []string{"site", "show", "a.test"}},
		{"site logs <domain> --lines 200 [--worker <name> | --service <name>]", "", []string{"site", "logs", "a.test", "--lines", "5"}},
		{"events --subject <domain> [--type <prefix>] [--since 24h] [--limit 500]", "", []string{"events", "--subject", "a.test", "--since", "1h"}},
		{"site tasks <domain> save <task> -", `{"version": 2, "schedule": "0 5 * * *", "command": ["sh", "-c", "echo hi"]}`, []string{"site", "tasks", "a.test", "save", "mine", "-"}},
		{"site tasks <domain>", "", []string{"site", "tasks", "a.test"}},
		{"site tasks <domain> run <task>", "", []string{"site", "tasks", "a.test", "run", "mine"}},
		{"site tasks <domain> log <task> [--lines 200]", "", []string{"site", "tasks", "a.test", "log", "mine"}},
		{"site tasks <domain> stop <task>", "", []string{"site", "tasks", "a.test", "stop", "mine"}},
		{"site tasks <domain> rm <task>", "", []string{"site", "tasks", "a.test", "rm", "mine"}},
		{"site backups <domain> [log <name> | run <name> | restore <name> <copy>]", "", []string{"site", "backups", "a.test"}},
		{"site security <domain> [seen | blocks | catalog]", "", []string{"site", "security", "a.test"}},
		{"site security <domain> [seen | blocks | catalog]", "", []string{"site", "security", "a.test", "blocks"}},
		{"site security <domain> [seen | blocks | catalog]", "", []string{"site", "security", "a.test", "catalog"}},
		{"site restart <domain> [--service <name>]", "", []string{"site", "restart", "a.test"}},
		{"site deploy <domain>", "", []string{"site", "deploy", "a.test"}},
		{"site validate <domain> [--file <draft>]", "", []string{"site", "validate", "a.test"}},
		{"site routes <domain>", "", []string{"site", "routes", "a.test"}},
	}
	// exec is cmd/middleware's own command, not this package's: it checks
	// the turn's site itself (cmd/middleware/exec_test.go) and goes to the
	// exec route, which TurnScopes opens.
	covered := map[string]bool{"exec <domain> [--service <name>] -- <command>": true}
	for _, tr := range tries {
		covered[tr.args] = true
		out, err := runIn(t, cl, tr.stdin, tr.try...)
		var apiErr *APIError
		if err != nil && errorsAs(err, &apiErr) && (apiErr.Status == http.StatusForbidden || apiErr.Status == http.StatusNotFound && strings.Contains(apiErr.Message, "no such")) {
			t.Errorf("%s: refused to the site's turn: %v (%s)", strings.Join(tr.try, " "), err, out)
		}
		if tr.try[1] == "tasks" && len(tr.try) == 6 && tr.try[3] == "run" {
			// Let the run end before stop and rm, which is how an agent
			// is told to use it: run, then read its log.
			time.Sleep(300 * time.Millisecond)
		}
	}
	for _, c := range chat.TurnCommands {
		if !covered[c.Args] {
			t.Errorf("the preamble promises `middleware %s` and nothing here runs it as a site's turn", c.Args)
		}
	}

	// And the harness does confine: what is not the site's is refused,
	// so the passes above are not an open door's.
	for _, try := range [][]string{
		{"site", "show", "other.test"},
		{"site", "tasks", "other.test"},
		{"events"},
		{"site", "list"},
		{"site", "security", "other.test"},
		{"site", "security", "other.test", "blocks"},
	} {
		_, err := run(t, cl, try...)
		var apiErr *APIError
		if !errorsAs(err, &apiErr) || apiErr.Status != http.StatusForbidden {
			t.Errorf("%s: not refused to a.test's turn: %v", strings.Join(try, " "), err)
		}
	}
}
