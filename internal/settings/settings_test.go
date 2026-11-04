package settings

import (
	"os"

	"github.com/dcimorra/the-middleware/internal/cgroups"
	"path/filepath"
	"testing"
)

func TestAFileWithoutTheAgentsSectionLoadsWithItsDefaults(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "data"), 0o755)
	os.WriteFile(Path(root), []byte(`{"panel_domain":"panel.test"}`), 0o644)
	s, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if s.PanelDomain != "panel.test" || s.Agents.MaxConcurrent != 0 || s.Agents.ReservedForHouse != 1 || s.Agents.DefaultProvider != "claude" || s.Agents.PerProvider["claude"] != 0 {
		t.Fatalf("settings = %+v", s)
	}
	// A partial section keeps what it says and fills the rest.
	os.WriteFile(Path(root), []byte(`{"agents":{"max_concurrent":2,"reserved_for_house":0}}`), 0o644)
	s, _ = Load(root)
	if s.Agents.MaxConcurrent != 2 || s.Agents.ReservedForHouse != 0 || s.Agents.DefaultProvider != "claude" || s.Agents.PerProvider == nil {
		t.Fatalf("partial section = %+v", s.Agents)
	}
}

func TestAListenAddressNeedsANumericPortInRange(t *testing.T) {
	for _, ok := range []string{"127.0.0.1:46100", ":8080", "[::1]:1", "localhost:65535"} {
		if err := ValidListen(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"127.0.0.1", "", "127.0.0.1:", "127.0.0.1:http", "127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:-1", "::1:80"} {
		if err := ValidListen(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestTheAgentsSectionIsValidatedAsAWhole(t *testing.T) {
	ok := DefaultAgents()
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]Agents{
		"negative cap":   {MaxConcurrent: -1, DefaultProvider: "claude"},
		"too many":       {MaxConcurrent: 65, DefaultProvider: "claude"},
		"reserved = max": {MaxConcurrent: 2, ReservedForHouse: 2, DefaultProvider: "claude"},
		"negative":       {MaxConcurrent: 2, ReservedForHouse: -1, DefaultProvider: "claude"},
		"provider < 0":   {MaxConcurrent: 2, PerProvider: map[string]int{"claude": -1}, DefaultProvider: "claude"},
		"no provider":    {MaxConcurrent: 2},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// The cap is the agents' memory ceiling over 1.5 GiB a turn, from 2 to
// 64, and the ceiling a third of the machine unless the operator set one.
func TestTheCapIsComputedFromTheMachine(t *testing.T) {
	const gb = 1 << 30
	for _, c := range []struct {
		mem  int64
		want int
	}{{4 * gb, 2}, {24 * gb, 5}, {64 * gb, 14}, {128 * gb, 28}, {1024 * gb, 64}, {0, 8}} {
		m := cgroups.Machine{CPUs: 8, Memory: c.mem}
		n, computed := DefaultAgents().Concurrency(m)
		if n != c.want || !computed {
			t.Errorf("%d GiB: %d (computed %v), want %d", c.mem/gb, n, computed, c.want)
		}
	}
	m := cgroups.Machine{CPUs: 12, Memory: 64 * gb}
	// The operator's numbers win, and a lower memory ceiling brings the
	// computed cap and the squeeze point down with it.
	a := DefaultAgents()
	a.MemoryMax, a.CPUCores = "6g", 4
	if l := a.Limits(m); l.MemoryMax != 6*gb || l.MemoryHigh != 6*gb/4*3 || l.CPUCores != 4 {
		t.Fatalf("operator's limits = %+v", l)
	}
	if n, _ := a.Concurrency(m); n != 4 {
		t.Fatalf("cap under a 6g ceiling = %d, want 4", n)
	}
	a.MaxConcurrent = 11
	if n, computed := a.Concurrency(m); n != 11 || computed {
		t.Fatalf("operator's cap = %d %v", n, computed)
	}
	// Left alone, the machine decides: nine of twelve cores, a third and
	// a quarter of the memory.
	if l := DefaultAgents().Limits(m); l.CPUCores != 9 || l.MemoryMax != 64*gb/3 || l.MemoryHigh != 16*gb {
		t.Fatalf("computed limits = %+v", l)
	}
	// Reserved slots are held to the computed cap on the machine, and
	// kept under it when resolved.
	small := cgroups.Machine{CPUs: 2, Memory: 4 * gb}
	r := DefaultAgents()
	r.ReservedForHouse = 3
	if err := r.ValidateOn(small); err == nil {
		t.Fatal("3 reserved of a computed 2 accepted")
	}
	if got := r.Resolved(small); got.MaxConcurrent != 2 || got.ReservedForHouse != 1 {
		t.Fatalf("resolved = %+v", got)
	}
	for _, bad := range []Agents{{DefaultProvider: "claude", CPUCores: -1}, {DefaultProvider: "claude", MemoryMax: "lots"}, {DefaultProvider: "claude", MemoryMax: "10m"}} {
		if err := bad.ValidateOn(m); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

// A file from before the cap could be computed holds the 8 every save
// wrote: that one becomes computed, any other number stays the
// operator's, and once the file is in the new shape 8 is a choice too.
func TestAnOldEightBecomesComputedAndAChosenNumberStays(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "data"), 0o755)
	load := func(body string) Agents {
		t.Helper()
		os.WriteFile(Path(root), []byte(body), 0o644)
		s, err := Load(root)
		if err != nil {
			t.Fatal(err)
		}
		return s.Agents
	}
	if a := load(`{"agents":{"max_concurrent":8,"reserved_for_house":1,"default_provider":"claude"}}`); a.MaxConcurrent != 0 {
		t.Fatalf("old default 8 = %d, want computed", a.MaxConcurrent)
	}
	if a := load(`{"agents":{"max_concurrent":3,"reserved_for_house":1,"default_provider":"claude"}}`); a.MaxConcurrent != 3 {
		t.Fatalf("operator's 3 = %d", a.MaxConcurrent)
	}
	if a := load(`{"format":2,"agents":{"max_concurrent":8,"reserved_for_house":1,"default_provider":"claude"}}`); a.MaxConcurrent != 8 {
		t.Fatalf("an 8 saved in the new shape = %d", a.MaxConcurrent)
	}
	// Saving writes the shape, so the 8 chosen after the upgrade survives
	// the next start.
	s := Defaults()
	s.Agents.MaxConcurrent = 8
	if err := Save(root, s); err != nil {
		t.Fatal(err)
	}
	if again, _ := Load(root); again.Agents.MaxConcurrent != 8 || again.Format != Format {
		t.Fatalf("after save: %+v", again)
	}
}
