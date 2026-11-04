package chat

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dcimorra/the-middleware/internal/agent"
	"github.com/dcimorra/the-middleware/internal/cgroups"
	"github.com/dcimorra/the-middleware/internal/settings"
)

// holding returns a provider whose runs stay open until the returned
// channel is closed, so several turns can be in flight at once.
func holding(key string) (*fakeProvider, chan struct{}) {
	hold := make(chan struct{})
	return &fakeProvider{key: key, hold: hold, script: []agent.Event{
		{Kind: agent.KindText, Text: "working"},
		{Kind: agent.KindResult, Text: "ok"},
	}}, hold
}

func TestTheHouseWideCapRefusesSitesButKeepsTheReservedSlotForTheHouse(t *testing.T) {
	p, hold := holding("fake")
	m, root, bus := newManager(t, p)
	os.MkdirAll(filepath.Join(root, "sites", "c.test"), 0o755)
	m.SetLimits(settings.Agents{MaxConcurrent: 2, ReservedForHouse: 1, DefaultProvider: "fake"})
	ch, unsub := bus.Subscribe()
	defer unsub()
	if _, err := m.Send("a.test", "one", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, "agent.text")
	// One of two slots is reserved for the house: the second site waits.
	_, err := m.Send("b.test", "two", "")
	var ec *ErrCapacity
	if !errors.As(err, &ec) || ec.Used != 1 || ec.Max != 2 || ec.Provider != "" {
		t.Fatalf("second site: %v", err)
	}
	ev := waitFor(t, ch, "agent.capacity")
	if ev.Level != "warn" || ev.Subject != "b.test" || ev.Data["used"] != 1 || ev.Data["max"] != 2 {
		t.Fatalf("capacity event = %+v", ev)
	}
	// The house takes the reserved slot, then even the house is refused.
	if _, err := m.Send(Middleware, "what is going on?", ""); err != nil {
		t.Fatalf("the house was refused its reserved slot: %v", err)
	}
	if _, err := m.Send("c.test", "three", ""); !errors.As(err, &ec) || ec.Used != 2 {
		t.Fatalf("third: %v", err)
	}
	c := m.Capacity()
	if c.Used != 2 || c.Max != 2 || c.Reserved != 1 || len(c.Running) != 2 || c.Providers["fake"].Used != 2 || c.Providers["fake"].Max != 0 {
		t.Fatalf("capacity = %+v", c)
	}
	if c.Running[0].Domain != "a.test" || c.Running[1].Domain != Middleware || c.Running[0].Provider != "fake" || c.Running[0].Started.IsZero() || c.Running[0].TurnID == "" {
		t.Fatalf("running = %+v", c.Running)
	}
	// A higher cap applies to the next Send without a restart.
	m.SetLimits(settings.Agents{MaxConcurrent: 4, ReservedForHouse: 1, DefaultProvider: "fake"})
	if _, err := m.Send("b.test", "two again", ""); err != nil {
		t.Fatalf("after raising the cap: %v", err)
	}
	close(hold)
	for i := 0; i < 3; i++ {
		waitFor(t, ch, "agent.done")
	}
	if c := m.Capacity(); c.Used != 0 || len(c.Running) != 0 {
		t.Fatalf("after the turns: %+v", c)
	}
}

func TestAProviderCapIsCountedOnItsOwn(t *testing.T) {
	p, hold := holding("fake")
	m, root, bus := newManager(t, p)
	other, otherHold := holding("other")
	m.opts.Registry.Register(other)
	os.WriteFile(filepath.Join(root, "sites", "b.test", "site.json"), []byte(`{"version":2,"agent":{"provider":"other"}}`), 0o644)
	os.MkdirAll(filepath.Join(root, "sites", "c.test"), 0o755)
	m.SetLimits(settings.Agents{MaxConcurrent: 8, ReservedForHouse: 1, PerProvider: map[string]int{"fake": 1, "other": 0}, DefaultProvider: "fake"})
	ch, unsub := bus.Subscribe()
	defer unsub()
	if _, err := m.Send("a.test", "one", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, "agent.text")
	_, err := m.Send("c.test", "two", "")
	var ec *ErrCapacity
	if !errors.As(err, &ec) || ec.Provider != "fake" || ec.ProviderUsed != 1 || ec.ProviderMax != 1 || ec.Used != 1 || ec.Max != 8 {
		t.Fatalf("second fake turn: %v", err)
	}
	// The other provider has no cap of its own and the house has room.
	if _, err := m.Send("b.test", "on the other", ""); err != nil {
		t.Fatalf("other provider refused: %v", err)
	}
	c := m.Capacity()
	if c.Providers["fake"] != (ProviderCapacity{Used: 1, Max: 1}) || c.Providers["other"] != (ProviderCapacity{Used: 1, Max: 0}) {
		t.Fatalf("providers = %+v", c.Providers)
	}
	close(hold)
	close(otherHold)
	waitFor(t, ch, "agent.done")
	waitFor(t, ch, "agent.done")
}

func TestTheDefaultProviderComesFromTheSettingsUnlessPinned(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "sites", "x.test"), 0o755)
	reg := agent.NewRegistry()
	m := New(Options{Root: root, Registry: reg, Limits: settings.Agents{MaxConcurrent: 1, DefaultProvider: "agy"}})
	if s, _ := m.Resolve("x.test"); s.Provider != "agy" {
		t.Fatalf("provider = %s", s.Provider)
	}
	m.SetLimits(settings.Agents{MaxConcurrent: 1, DefaultProvider: "claude"})
	if s, _ := m.Resolve("x.test"); s.Provider != "claude" {
		t.Fatalf("after SetLimits: %s", s.Provider)
	}
	pinned := New(Options{Root: root, Registry: reg, DefaultProvider: "fake", Limits: settings.Agents{MaxConcurrent: 1, DefaultProvider: "agy"}})
	if s, _ := pinned.Resolve("x.test"); s.Provider != "fake" {
		t.Fatalf("pinned provider = %s", s.Provider)
	}
	// A zero value takes the defaults rather than "nothing may run": the
	// cap computed from the machine, 24 GiB giving 5.
	defer func(f func() cgroups.Machine) { settings.Machine = f }(settings.Machine)
	settings.Machine = func() cgroups.Machine { return cgroups.Machine{CPUs: 8, Memory: 24 << 30} }
	bare := New(Options{Root: root, Registry: reg})
	if c := bare.Capacity(); c.Max != 5 || !c.MaxComputed || c.Reserved != 1 {
		t.Fatalf("defaults = %+v", c)
	}
	if s, _ := bare.Resolve("x.test"); s.Provider != "claude" {
		t.Fatalf("bare provider = %s", s.Provider)
	}
}
