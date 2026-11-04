package manifest

import (
	"strings"
	"testing"

	"github.com/dcimorra/the-middleware/internal/cgroups"
)

func TestASitesLimitsAreHeldToTheirRules(t *testing.T) {
	ok := &Limits{Memory: "1g", MemoryHigh: "768m", CPUs: 1.5, CPUWeight: 100, Pids: 2048}
	if err := ok.Validate("limits"); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]*Limits{
		"negative cpus":   {CPUs: -1},
		"negative pids":   {Pids: -1},
		"docker's weight": {CPUWeight: 10240},
		"not a size":      {Memory: "lots"},
		"high above max":  {Memory: "512m", MemoryHigh: "1g"},
	} {
		if err := bad.Validate("limits"); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	m := &Manifest{Version: Version, Limits: &Limits{Memory: "64m", MemoryHigh: "1g"}}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "limits.memory_high") {
		t.Fatalf("manifest with high above max: %v", err)
	}
}

// The manifest wins field by field over the house's defaults, and a
// site's own ceiling drops the house's squeeze point rather than leaving
// it above.
func TestASitesLimitsGoOverTheHouseDefaults(t *testing.T) {
	def := &Limits{Memory: "2g", MemoryHigh: "1536m", Pids: 1000}
	got := (&Limits{Memory: "512m", CPUs: 2}).Over(def).Group()
	want := cgroups.SiteLimits{MemoryMax: 512 << 20, CPUs: 2, Pids: 1000}
	if got != want {
		t.Fatalf("over = %+v, want %+v", got, want)
	}
	var none *Limits
	if g := none.Over(def).Group(); g.MemoryMax != 2<<30 || g.MemoryHigh != 1536<<20 {
		t.Fatalf("defaults alone = %+v", g)
	}
	if g := none.Over(nil).Group(); g != (cgroups.SiteLimits{}) {
		t.Fatalf("nothing = %+v", g)
	}
}
