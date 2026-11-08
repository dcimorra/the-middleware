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

	"github.com/dcimorra/the-middleware/internal/api"
	"github.com/dcimorra/the-middleware/internal/auth"
	"github.com/dcimorra/the-middleware/internal/autopilot"
	"github.com/dcimorra/the-middleware/internal/backups"
	"github.com/dcimorra/the-middleware/internal/chat"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/settings"
	"github.com/dcimorra/the-middleware/internal/sites"
	"github.com/dcimorra/the-middleware/internal/tasks"
	"github.com/dcimorra/the-middleware/internal/visits"
)

// pilotServer is a real API with every socket connection taken for a turn
// of the autonomous agent (auth.WithSocketAutopilot), the caller its turns
// are when they run the CLI, and a pilot switched on. out.test's manifest
// keeps the agent out.
func pilotServer(t *testing.T) (root string, cl *Client, p *autopilot.Pilot, asked *[]string) {
	t.Helper()
	root = t.TempDir()
	for _, d := range []string{"a.test", "out.test"} {
		os.MkdirAll(filepath.Join(root, "sites", d), 0o755)
		m := `{"version": 2, "static": {"root": "."}}`
		if d == "out.test" {
			m = `{"version": 2, "static": {"root": "."}, "autopilot": false}`
		}
		os.WriteFile(filepath.Join(root, "sites", d, "site.json"), []byte(m), 0o644)
	}
	bus := events.New("")
	var mu sync.Mutex
	cur := settings.Defaults()
	cur.Autopilot.Enabled = true
	get := func() settings.Settings { mu.Lock(); defer mu.Unlock(); return cur }
	save := func(s settings.Settings) error { mu.Lock(); cur = s; mu.Unlock(); return nil }
	mgr := sites.New(root, nil, bus)
	srv := api.New(api.Options{Root: root, Version: "test", Auth: auth.New(root, bus), Sites: mgr, Bus: bus, Settings: get, SaveSettings: save})
	srv.RegisterTasks(api.TasksOptions{Scheduler: tasks.NewScheduler(tasks.Options{Root: root, Bus: bus}), Root: root})
	srv.RegisterBackups(backups.New(backups.Options{Root: root, Bus: bus}))
	srv.RegisterVisits(visits.New(filepath.Join(root, "data", "visits")))
	var list []string
	p = autopilot.New(autopilot.Options{
		DataDir:  filepath.Join(root, "data"),
		Bus:      bus,
		Settings: func() settings.Autopilot { return get().Autopilot },
		SetEnabled: func(on bool) error {
			s := get()
			s.Autopilot.Enabled = on
			return save(s)
		},
		Sites: func() []autopilot.Site {
			return []autopilot.Site{{Domain: "a.test", Mode: "static", Alive: true}, {Domain: "out.test", Mode: "static", OptOut: true}}
		},
		AskSite: func(domain, prompt, provider, model string) (string, string, error) {
			mu.Lock()
			list = append(list, domain)
			mu.Unlock()
			return chat.Name(domain, "ab"), "t1", nil
		},
		Stop:    func(string) error { return nil },
		Running: func(string) bool { return false },
	})
	srv.RegisterAutopilot(p, nil)
	ln, err := api.ListenSocket(api.SocketPath(root))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	hs := &http.Server{Handler: srv.Panel(), BaseContext: func(net.Listener) context.Context { return auth.WithSocketAutopilot(ctx) }}
	go hs.Serve(ln)
	t.Cleanup(func() { cancel(); hs.Close() })
	return root, SocketClient(api.SocketPath(root)), p, &list
}

// Every command the autonomous agent's preamble promises is run here as
// its turns run it, and none may be refused for want of permission; and
// what holds it back (its own switch and settings, tokens, deleting a
// site, publishing one it did not make, a site that keeps it out) is
// refused to it, so the passes are not an open door's.
func TestTheAutonomousAgentHasWhatItIsToldAndNothingThatHoldsItBack(t *testing.T) {
	root, cl, p, asked := pilotServer(t)
	tries := []struct {
		args  string
		stdin string
		try   []string
	}{
		{"autopilot status", "", []string{"autopilot", "status"}},
		{"autopilot journal [--limit 50]", "", []string{"autopilot", "journal", "--limit", "5"}},
		{"autopilot sites", "", []string{"autopilot", "sites"}},
		{"autopilot ask <domain> -", "Add a contact page.", []string{"autopilot", "ask", "a.test", "-"}},
		{"site list", "", []string{"site", "list"}},
		{"site show <domain>", "", []string{"site", "show", "a.test"}},
		{"site logs <domain> --lines 200", "", []string{"site", "logs", "a.test", "--lines", "5"}},
		{"events [--subject <domain>] [--type <prefix>] [--since 24h]", "", []string{"events", "--since", "1h"}},
		{"site visits <domain>", "", []string{"site", "visits", "a.test"}},
		{"site restart <domain>", "", []string{"site", "restart", "a.test"}},
		{"site deploy <domain>", "", []string{"site", "deploy", "a.test"}},
		{"site tasks <domain> [log <task>]", "", []string{"site", "tasks", "a.test"}},
		{"site add <domain> --template <static|node|go|docker>", "", []string{"site", "add", "new.test", "--template", "static"}},
		{"autopilot publish <domain>", "", []string{"autopilot", "publish", "new.test"}},
	}
	covered := map[string]bool{}
	for _, tr := range tries {
		covered[tr.args] = true
		out, err := runIn(t, cl, tr.stdin, tr.try...)
		var apiErr *APIError
		if err != nil && errorsAs(err, &apiErr) && (apiErr.Status == http.StatusForbidden || apiErr.Status == http.StatusUnauthorized) {
			t.Errorf("%s: refused to the agent: %v (%s)", strings.Join(tr.try, " "), err, out)
		}
	}
	for _, c := range chat.AutopilotCommands {
		if !covered[c.Args] {
			t.Errorf("the preamble promises `middleware %s` and nothing here runs it as the agent's turn", c.Args)
		}
	}
	if len(*asked) != 1 || (*asked)[0] != "a.test" {
		t.Errorf("work handed out: %v", *asked)
	}
	if !p.Mine("new.test") {
		t.Error("the site it created is not recorded as its own")
	}
	// Created off the internet: the site add above asked for nothing, and
	// with a DNS module on it would have been published at once.
	if _, err := os.Stat(filepath.Join(root, "sites", "new.test", "site.json")); err != nil {
		t.Errorf("the site was not created: %v", err)
	}

	for _, try := range [][]string{
		{"autopilot", "on"},
		{"autopilot", "off"},
		{"autopilot", "stop"},
		{"autopilot", "wake"},
		{"token", "add", "mine"},
		{"site", "rm", "a.test"},
		{"site", "hold", "a.test"},
		{"site", "show", "out.test"},
		{"site", "logs", "out.test"},
		{"site", "restart", "out.test"},
		{"site", "stop", "a.test"},
		{"autopilot", "publish", "a.test"},
	} {
		out, err := run(t, cl, try...)
		var apiErr *APIError
		if !errorsAs(err, &apiErr) || (apiErr.Status != http.StatusForbidden && apiErr.Status != http.StatusConflict) {
			t.Errorf("%s: not refused to the agent: %v (%s)", strings.Join(try, " "), err, out)
		}
	}
	for _, raw := range []struct{ method, path string }{
		{"PUT", "/api/v1/settings"},
		{"PUT", "/api/v1/autopilot/settings"},
		{"PUT", "/api/v1/autopilot/guidelines"},
		{"PUT", "/api/v1/sites/a.test/manifest"},
		{"DELETE", "/api/v1/sites/a.test"},
	} {
		err := cl.Do(raw.method, raw.path, map[string]any{"text": "x", "enabled": false}, nil)
		var apiErr *APIError
		if !errorsAs(err, &apiErr) || apiErr.Status != http.StatusForbidden {
			t.Errorf("%s %s: not refused to the agent: %v", raw.method, raw.path, err)
		}
	}
	// What it did and was refused is in its journal.
	kinds := map[string]bool{}
	for _, e := range p.Journal(0) {
		kinds[e.Kind] = true
	}
	for _, k := range []string{autopilot.KindAsk, autopilot.KindSite, autopilot.KindCall, autopilot.KindRefused} {
		if !kinds[k] {
			t.Errorf("no %q line in the journal: %v", k, p.Journal(0))
		}
	}
}

// The weekly allowance and the names it may use are the house's, not the
// prompt's: a third site in seven days, and a name outside the zones,
// are refused at creation.
func TestTheAgentCreatesSitesWithinItsAllowance(t *testing.T) {
	_, cl, _, _ := pilotServer(t)
	for _, d := range []string{"one.test", "two.test"} {
		if out, err := run(t, cl, "site", "add", d); err != nil {
			t.Fatalf("%s: %v (%s)", d, err, out)
		}
	}
	_, err := run(t, cl, "site", "add", "three.test")
	var apiErr *APIError
	if !errorsAs(err, &apiErr) || apiErr.Status != http.StatusForbidden || !strings.Contains(apiErr.Message, "seven days") {
		t.Fatalf("a third site in a week: %v", err)
	}
}
