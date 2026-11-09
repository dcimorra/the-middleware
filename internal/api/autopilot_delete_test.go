package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dcimorra/the-middleware/internal/autopilot"
	"github.com/dcimorra/the-middleware/internal/settings"
)

// The operator deletes one of the agent's sites from its page: the
// folder goes, the agent no longer holds it, the record stays with the
// reason, and its next wake is told. A site it did not make is not
// deleted through here.
func TestTheOperatorDeletesASiteTheAgentMade(t *testing.T) {
	f := newFixture(t).withToken()
	for _, d := range []string{"mine.example", "other.example"} {
		if resp, body := f.call("POST", "/api/v1/sites", map[string]any{"domain": d}); resp.StatusCode != 201 {
			t.Fatalf("create %s: %d %v", d, resp.StatusCode, body)
		}
	}
	p := autopilot.New(autopilot.Options{
		DataDir:  filepath.Join(f.root, "data"),
		Settings: func() settings.Autopilot { return settings.Defaults().Autopilot },
	})
	p.Created("mine.example")
	f.srv.RegisterAutopilot(p, nil)

	if resp, _ := f.call("DELETE", "/api/v1/autopilot/sites/other.example", nil); resp.StatusCode != 404 {
		t.Errorf("a site it did not make: %d", resp.StatusCode)
	}
	resp, body := f.call("DELETE", "/api/v1/autopilot/sites/mine.example?reason=Not+what+I+want", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("delete: %d %v", resp.StatusCode, body)
	}
	if _, err := os.Stat(filepath.Join(f.root, "sites", "mine.example")); !os.IsNotExist(err) {
		t.Errorf("the folder is still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "sites", "other.example")); err != nil {
		t.Errorf("the other site went too: %v", err)
	}
	if p.Mine("mine.example") {
		t.Error("the agent still holds the deleted site")
	}
	st := p.Status()
	if len(st.State.Created) != 1 || st.State.Created[0].Deleted.IsZero() || st.State.Created[0].Reason != "Not what I want" {
		t.Errorf("the record: %+v", st.State.Created)
	}
	if len(st.State.Alerts) != 1 || !strings.Contains(st.State.Alerts[0], "Not what I want") {
		t.Errorf("its next wake is not told: %v", st.State.Alerts)
	}
	found := false
	for _, e := range p.Journal(0) {
		if e.Kind == autopilot.KindSite && strings.Contains(e.Message, "deleted by the operator: Not what I want") {
			found = true
		}
	}
	if !found {
		t.Errorf("not in its journal: %v", p.Journal(0))
	}
	if resp, _ := f.call("DELETE", "/api/v1/autopilot/sites/mine.example", nil); resp.StatusCode != 404 {
		t.Errorf("deleted twice: %d", resp.StatusCode)
	}
}
