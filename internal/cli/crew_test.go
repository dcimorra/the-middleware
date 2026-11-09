package cli

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dcimorra/the-middleware/internal/api"
	"github.com/dcimorra/the-middleware/internal/auth"
	"github.com/dcimorra/the-middleware/internal/chat"
	"github.com/dcimorra/the-middleware/internal/crew"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/secrets"
	"github.com/dcimorra/the-middleware/internal/settings"
	"github.com/dcimorra/the-middleware/internal/sites"
)

// crewServer is a real API with every socket connection taken for a turn
// of the crew agent "helper" (auth.WithSocketCrew), the caller its turns
// are when they run the CLI. A second agent, "other", is there to be
// refused.
func crewServer(t *testing.T) (*Client, *crew.Crew, string) {
	t.Helper()
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "sites", "a.test"), 0o755)
	os.WriteFile(filepath.Join(root, "sites", "a.test", "site.json"), []byte(`{"version": 2, "static": {"root": "."}}`), 0o644)
	bus := events.New("")
	cur := settings.Defaults()
	get := func() settings.Settings { return cur }
	srv := api.New(api.Options{Root: root, Version: "test", Auth: auth.New(root, bus), Sites: sites.New(root, nil, bus), Bus: bus, Settings: get, SaveSettings: func(settings.Settings) error { return nil }})
	box, err := secrets.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	c := crew.New(crew.Options{Root: root, Bus: bus, Box: box, Running: func(string) bool { return false }})
	me, err := c.Create(crew.Agent{Name: "Helper", Goal: "Keep the operator's inbox tidy."})
	if err != nil {
		t.Fatal(err)
	}
	other, err := c.Create(crew.Agent{Name: "Other", Goal: "Something else."})
	if err != nil {
		t.Fatal(err)
	}
	srv.RegisterCrew(c, nil)
	ln, err := api.ListenSocket(api.SocketPath(root))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	hs := &http.Server{Handler: srv.Panel(), BaseContext: func(net.Listener) context.Context { return auth.WithSocketCrew(ctx, me.ID) }}
	go hs.Serve(ln)
	t.Cleanup(func() { cancel(); hs.Close() })
	t.Setenv("MIDDLEWARE_CREW", me.ID)
	return SocketClient(api.SocketPath(root)), c, other.ID
}

// Every command a crew agent's preamble promises runs here as its turns
// run it, and none is refused for want of permission; and what is
// the operator's, another agent's or a site's is refused to it.
func TestACrewAgentHasWhatItIsToldAndNothingElse(t *testing.T) {
	cl, c, other := crewServer(t)
	tries := []struct {
		args  string
		stdin string
		try   []string
	}{
		{"crew status", "", []string{"crew", "status"}},
		{"crew journal [--limit 50]", "", []string{"crew", "journal", "--limit", "5"}},
		{"crew request --title <title> [--field KEY] [--secret KEY] [--option <choice>] [--urgent] -", "I need the mailbox's details.", []string{"crew", "request", "--title", "Mailbox", "--field", "SMTP_HOST", "--secret", "SMTP_PASSWORD", "-"}},
		{"crew requests [<id>]", "", []string{"crew", "requests"}},
		{"crew vault", "", []string{"crew", "vault"}},
		{"crew vault set KEY -", "s3cret\n", []string{"crew", "vault", "set", "ACCOUNT_PASSWORD", "-"}},
		{"crew vault rm KEY", "", []string{"crew", "vault", "rm", "ACCOUNT_PASSWORD"}},
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
	for _, cmd := range chat.CrewCommands {
		if !covered[cmd.Args] {
			t.Errorf("the preamble promises `middleware %s` and nothing here runs it as the agent's turn", cmd.Args)
		}
	}
	for _, try := range [][]string{
		{"crew", "list"},
		{"crew", "on"},
		{"crew", "off"},
		{"crew", "stop"},
		{"crew", "wake"},
		{"crew", "status", "--agent", other},
		{"crew", "vault", "--agent", other},
		{"crew", "answer", "rx", "--text", "yes"},
		{"site", "list"},
		{"site", "show", "a.test"},
		{"token", "add", "mine"},
	} {
		out, err := run(t, cl, try...)
		var apiErr *APIError
		if !errorsAs(err, &apiErr) || (apiErr.Status != http.StatusForbidden && apiErr.Status != http.StatusUnauthorized) {
			t.Errorf("%s: not refused to the agent: %v (%s)", strings.Join(try, " "), err, out)
		}
	}
	for _, raw := range []struct{ method, path string }{
		{"PUT", "/api/v1/crew/helper"},
		{"PUT", "/api/v1/crew/other/vault/SMTP_HOST"},
		{"DELETE", "/api/v1/crew/other/vault/SMTP_HOST"},
		{"DELETE", "/api/v1/crew/helper"},
		{"POST", "/api/v1/crew"},
		{"PUT", "/api/v1/settings"},
	} {
		err := cl.Do(raw.method, raw.path, map[string]any{"value": "x", "name": "x"}, nil)
		var apiErr *APIError
		if !errorsAs(err, &apiErr) || apiErr.Status != http.StatusForbidden {
			t.Errorf("%s %s: not refused to the agent: %v", raw.method, raw.path, err)
		}
	}
	// What it kept in its vault itself was there, and is its own in the
	// journal; the value went in without the newline echo put there.
	kept := false
	for _, e := range mustJournal(t, c, "helper") {
		if e.Message == "it set ACCOUNT_PASSWORD" {
			kept = true
		}
	}
	if !kept {
		t.Error("its own vault value is not in its journal")
	}
	if out, err := runIn(t, cl, "pw\n", "crew", "vault", "set", "token", "-"); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if v := c.Env("helper")["TOKEN"]; v != "pw" {
		t.Errorf("kept %q", v)
	}
	reqs, err := c.Requests("helper")
	if err != nil || len(reqs) != 1 || len(reqs[0].Fields) != 2 || !reqs[0].Fields[1].Secret {
		t.Errorf("its request: %+v %v", reqs, err)
	}
}

func mustJournal(t *testing.T, c *crew.Crew, id string) []crew.Entry {
	t.Helper()
	list, err := c.Journal(id, 100)
	if err != nil {
		t.Fatal(err)
	}
	return list
}
