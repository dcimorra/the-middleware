package api

import (
	"path/filepath"
	"testing"

	"github.com/dcimorra/the-middleware/internal/autopilot"
	"github.com/dcimorra/the-middleware/internal/settings"
)

// The operator asks the agent for a site from its page and can take the
// order back; the route answers what the agent will read.
func TestTheOperatorAsksTheAgentForASite(t *testing.T) {
	f := newFixture(t).withToken()
	cfg := settings.Defaults().Autopilot
	cfg.Enabled = true
	p := autopilot.New(autopilot.Options{
		DataDir:  filepath.Join(f.root, "data"),
		Settings: func() settings.Autopilot { return cfg },
	})
	f.srv.RegisterAutopilot(p, nil)

	resp, body := f.call("POST", "/api/v1/autopilot/orders", map[string]any{"text": "A recipes site"})
	if resp.StatusCode != 200 {
		t.Fatalf("order: %d %v", resp.StatusCode, body)
	}
	id, _ := body["id"].(string)
	if id == "" || body["text"] != "A recipes site" {
		t.Fatalf("the order: %v", body)
	}
	if st := p.Status(); len(st.State.Orders) != 1 || st.State.NextReason != "the operator asked for a new site" {
		t.Errorf("not woken for it: %+v", st.State)
	}
	if resp, _ := f.call("POST", "/api/v1/autopilot/orders", nil); resp.StatusCode != 200 {
		t.Errorf("an order with no words: %d", resp.StatusCode)
	}
	if resp, _ := f.call("DELETE", "/api/v1/autopilot/orders/"+id, nil); resp.StatusCode != 200 {
		t.Errorf("take back: %d", resp.StatusCode)
	}
	if resp, _ := f.call("DELETE", "/api/v1/autopilot/orders/"+id, nil); resp.StatusCode != 404 {
		t.Errorf("taken back twice: %d", resp.StatusCode)
	}
}
