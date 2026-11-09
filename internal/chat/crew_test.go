package chat

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dcimorra/the-middleware/internal/agent"
)

// A crew agent's turn runs in its desk, with its vault in its environment
// and its own mark, and is denied the other agents, its own settings,
// requests and vault on disk, the sites and the house's data: the desk
// is the one folder it has.
func TestACrewTurnRunsInItsDeskWithItsVault(t *testing.T) {
	p := &fakeProvider{key: "fake", script: []agent.Event{{Kind: agent.KindResult, Text: "ok"}}}
	m, root, bus := newManager(t, p)
	for _, id := range []string{"helper", "other"} {
		os.MkdirAll(CrewDesk(root, id), 0o700)
		os.WriteFile(filepath.Join(root, "data", "crew", id, "vault.json"), []byte("{}"), 0o600)
	}
	os.WriteFile(filepath.Join(root, "data", "crew", "helper", "agent.json"), []byte(`{"id":"helper","name":"Helper"}`), 0o600)
	m.SetCrew(func(id string) map[string]string { return map[string]string{"SMTP_HOST": "mail." + id + ".example"} }, func(string) (string, string) { return "fake", "fake-2" })
	ch, unsub := bus.Subscribe()
	defer unsub()
	if _, err := m.Create("crew~nobody", CreateOptions{}); err == nil {
		t.Error("a conversation of an agent that does not exist")
	}
	name, err := m.Create(CrewDomain("helper"), CreateOptions{Origin: OriginPanel})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.SendWith(name, "wake", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, "agent.done")
	req := p.last()
	if req.Workdir != CrewDesk(root, "helper") || req.Model != "fake-2" {
		t.Errorf("ran in %s with %q", req.Workdir, req.Model)
	}
	if req.Env["SMTP_HOST"] != "mail.helper.example" || req.Env["MIDDLEWARE_CREW"] != "helper" || req.Env[MarkVar] != name {
		t.Errorf("env: %v", req.Env)
	}
	for _, d := range []string{
		filepath.Join(root, "data", "crew", "other"),
		filepath.Join(root, "data", "crew", "helper", "vault.json"),
		filepath.Join(root, "data", "crew", "helper", "agent.json"),
		filepath.Join(root, "sites", "a.test"),
		filepath.Join(root, "tasks"),
	} {
		if !slices.Contains(req.Jail.Deny, d) {
			t.Errorf("%s is not denied: %v", d, req.Jail.Deny)
		}
	}
	for _, d := range req.Jail.Deny {
		if d == CrewDesk(root, "helper") || strings.HasPrefix(CrewDesk(root, "helper"), d+"/") {
			t.Errorf("its own desk is denied by %s", d)
		}
	}
	if !strings.Contains(req.System, "You are Helper") || !strings.Contains(req.System, "middleware crew request") {
		t.Errorf("preamble: %s", req.System)
	}
	if got, err := SiteOfProcess("/proc", os.Getpid(), map[int]string{os.Getpid(): name}); err != nil || got != CrewDomain("helper") {
		t.Errorf("its turn is told as %q, %v", got, err)
	}
}
