package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dcimorra/the-middleware/internal/cgroups"
	"github.com/dcimorra/the-middleware/internal/manifest"
)

// The «Recursos» card draws its bars from one answer: the machine as the
// scale, the manifest's limits, and what the site's group holds now; on a
// node that does not split its cgroup it says the limits wait.
func TestASitesResourcesComeWithTheMachineAndWhatItUses(t *testing.T) {
	f := newFixture(t).withToken()
	dir := filepath.Join(f.root, "sites", "a.test")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, manifest.FileName), []byte(`{"version":2,"run":{"command":["x"]},"limits":{"memory":"1g","cpu_weight":50}}`), 0o644)
	applied := false
	f.srv.RegisterSiteResources(cgroups.Machine{CPUs: 8, Memory: 16 << 30}, func() bool { return applied }, func(domain string) (cgroups.SiteMemory, bool) {
		return cgroups.SiteMemory{Current: 300 << 20, Held: 400 << 20, OOMKills: 2, CPUUsec: 1234}, domain == "a.test"
	})
	resp, out := f.call("GET", "/api/v1/sites/a.test/limits", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
	if out["applied"] != false || out["use"] != nil {
		t.Fatalf("not split: %v", out)
	}
	if m := out["machine"].(map[string]any); m["cpus"] != 8.0 || m["memory"] != float64(16<<30) {
		t.Fatalf("machine %v", m)
	}
	if l := out["limits"].(map[string]any); l["memory"] != "1g" || l["cpu_weight"] != 50.0 {
		t.Fatalf("limits %v", l)
	}
	applied = true
	_, out = f.call("GET", "/api/v1/sites/a.test/limits", nil)
	u, _ := out["use"].(map[string]any)
	if out["applied"] != true || u["memory"] != float64(300<<20) || u["peak"] != float64(400<<20) || u["oom_kills"] != 2.0 || u["cpu_usec"] != 1234.0 {
		t.Fatalf("split: %v", out)
	}
	if resp, _ := f.call("GET", "/api/v1/sites/none.test/limits", nil); resp.StatusCode != 404 {
		t.Fatalf("a site that is not here: %d", resp.StatusCode)
	}
}

// A delegate's account changes its site's code, never what the site may
// take from the machine.
func TestADelegateCannotChangeItsSitesLimits(t *testing.T) {
	cur, _ := manifest.Parse([]byte(`{"version":2,"run":{"command":["x"]},"limits":{"memory":"512m"}}`))
	next, _ := manifest.Parse([]byte(`{"version":2,"run":{"command":["x"]},"limits":{"memory":"8g"}}`))
	if got := lockedChange(cur, next); got != "limits" {
		t.Fatalf("lockedChange = %q", got)
	}
	gone, _ := manifest.Parse([]byte(`{"version":2,"run":{"command":["x"]}}`))
	if got := lockedChange(cur, gone); got != "limits" {
		t.Fatalf("removing them: lockedChange = %q", got)
	}
}
