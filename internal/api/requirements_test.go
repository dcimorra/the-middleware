package api

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/helper"
	"github.com/dcimorra/the-middleware/internal/installer"
)

// The panel follows the run by its log alone: its last line says how it
// ended, a new run never shows the last one's ending, and a log that went
// quiet without one is said as cut.
func TestTheRequirementsRunIsFollowedByItsLog(t *testing.T) {
	f := newFixture(t).withToken()
	var asked []bool
	log := helper.InstallLog(t.TempDir())
	f.srv.RegisterRequirements(log, func(_ context.Context, browser bool) error {
		asked = append(asked, browser)
		return nil
	})
	state := func() map[string]any {
		_, out := f.call("GET", "/api/v1/system/requirements", nil)
		return out
	}
	if st := state(); st["state"] != "none" {
		t.Fatalf("before any run: %v", st)
	}
	os.WriteFile(log, []byte("docker   present\n\n"+installer.PanelFinished+"\n"), 0o644)
	if st := state(); st["state"] != "finished" {
		t.Fatalf("finished: %v", st)
	}
	if resp, out := f.call("POST", "/api/v1/system/requirements", map[string]bool{"browser": true}); resp.StatusCode != 200 || out["state"] != "running" {
		t.Fatalf("start: %d %v", resp.StatusCode, out)
	}
	if len(asked) != 1 || !asked[0] {
		t.Fatalf("the helper was asked %v", asked)
	}
	if st := state(); st["state"] != "none" {
		t.Fatalf("the last run's ending is still shown: %v", st)
	}
	os.WriteFile(log, []byte("installing…\n"), 0o644)
	if st := state(); st["state"] != "running" {
		t.Fatalf("running: %v", st)
	}
	if resp, _ := f.call("POST", "/api/v1/system/requirements", nil); resp.StatusCode != 409 {
		t.Fatalf("a second run while one runs: %d", resp.StatusCode)
	}
	os.WriteFile(log, []byte("x\n"+installer.PanelFailed+"E: unable to locate package\n"), 0o644)
	if st := state(); st["state"] != "failed" || st["error"] != "E: unable to locate package" {
		t.Fatalf("failed: %v", st)
	}
	os.WriteFile(log, []byte("x\n"+installer.PanelRestart+"\n"), 0o644)
	if st := state(); st["state"] != "restart" {
		t.Fatalf("restart: %v", st)
	}
	// The house restarted after the run: done, not "restart" for ever.
	before := time.Now().Add(-time.Hour)
	os.Chtimes(log, before, before)
	if st := state(); st["state"] != "finished" {
		t.Fatalf("after the restart: %v", st)
	}
	os.WriteFile(log, []byte("installing…\n"), 0o644)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(log, old, old)
	if st := state(); st["state"] != "interrupted" {
		t.Fatalf("a quiet log: %v", st)
	}
}

// A run's report lines reach the panel as steps, so it checks every node
// by them without reading a log: a name padded to its column, one too
// long for it, an optional step that failed and one that stopped the run.
func TestTheRequirementsRunSaysEachStep(t *testing.T) {
	log := "Installing\n" +
		installer.Check{Name: "docker", OK: true, Detail: "present"}.String() + "\n" +
		"✓ git, curl, wireguard, nftables, libatomic present\n" +
		"! headless browser           the panel works without the pictures: no network\n" +
		installer.Check{Name: "cgroups", OK: false, Detail: "permission denied"}.String() + "\n" +
		installer.PanelFailed + "cgroups\n"
	got := stepsOf(log)
	want := []RequirementsStep{
		{Name: "docker", OK: true, Detail: "present"},
		{Name: "git, curl, wireguard, nftables, libatomic present", OK: true},
		{Name: "headless browser", Optional: true, Detail: "the panel works without the pictures: no network"},
		{Name: "cgroups", Detail: "permission denied"},
	}
	if len(got) != len(want) {
		t.Fatalf("steps %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("step %d: %+v, want %+v", i, got[i], want[i])
		}
	}
}
