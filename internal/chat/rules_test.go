package chat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dcimorra/the-middleware/internal/agent"
	"github.com/dcimorra/the-middleware/internal/settings"
)

const testRules = "Never use emoji as icons.\nAlways finish the interface."

// The operator's rules reach the agent of every kind of conversation, by
// running a turn of each and reading what the provider was given.
func TestTheOperatorsRulesCloseEveryPreamble(t *testing.T) {
	p := &fakeProvider{key: "fake", script: []agent.Event{{Kind: agent.KindResult, Text: "ok", SessionID: "s"}}}
	m, root, bus := newManager(t, p)
	os.MkdirAll(CrewDesk(root, "helper"), 0o700)
	os.WriteFile(filepath.Join(root, "data", "crew", "helper", "vault.json"), []byte("{}"), 0o600)
	os.WriteFile(filepath.Join(root, "data", "crew", "helper", "agent.json"), []byte(`{"id":"helper","name":"Helper"}`), 0o600)
	m.SetCrew(func(string) map[string]string { return nil }, func(string) (string, string) { return "fake", "fake-1" })
	a := settings.DefaultAgents()
	a.DefaultProvider = "fake"
	a.Rules = testRules
	m.SetLimits(a)
	ch, unsub := bus.Subscribe()
	defer unsub()
	want := "<operator-rules>\n" + testRules + "\n</operator-rules>"
	turn := func(what, domain string, o CreateOptions) string {
		t.Helper()
		name, err := m.Create(domain, o)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if _, err := m.SendWith(name, "go", SendOptions{}); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		waitFor(t, ch, "agent.done")
		return p.last().System
	}
	for what, sys := range map[string]string{
		"a site":                        turn("a site", "a.test", CreateOptions{Origin: OriginPanel}),
		"the house":                     turn("the house", Middleware, CreateOptions{Origin: OriginPanel}),
		"a crew agent":                  turn("a crew agent", CrewDomain("helper"), CreateOptions{Origin: OriginPanel}),
		"a site's own that can write":   turn("door write", "a.test", CreateOptions{Origin: OriginSite, Tools: "write", Provider: "fake"}),
		"a site's own with edit rights": turn("door edit", "a.test", CreateOptions{Origin: OriginSite, Tools: "perms:read,edit", Provider: "fake"}),
	} {
		if !strings.HasSuffix(strings.TrimSpace(sys), want) {
			t.Errorf("the preamble of %s does not end with the operator's rules:\n%s", what, sys)
		}
	}
	// One that answers a program and cannot write has nothing to build.
	if sys := turn("door none", "a.test", CreateOptions{Origin: OriginSite, Tools: "none", Provider: "fake"}); strings.Contains(sys, "operator-rules") {
		t.Errorf("a conversation without tools is told the operator's rules:\n%s", sys)
	}
	// Taken away, they are gone from the next turn.
	a.Rules = "  \n"
	m.SetLimits(a)
	if sys := turn("a site, no rules", "a.test", CreateOptions{Origin: OriginPanel}); strings.Contains(sys, "operator-rules") {
		t.Errorf("no rules, and the preamble still speaks of them:\n%s", sys)
	}
}

// The house ships nobody's taste: what an operator wants of every agent
// is theirs to write. A rule about icons was once in the guide and in
// every preamble, in a project anyone installs.
func TestTheHouseShipsNoRulesOfItsOwn(t *testing.T) {
	j := agent.Jail{Home: "/h"}
	texts := map[string]string{
		"the guide":           string(SkillContent()),
		"a site's preamble":   Preamble("shop.test", "/srv/mw/sites/shop.test", j),
		"the house's":         Preamble(Middleware, "/srv/mw", j),
		"the network agent's": Preamble(Autopilot, "/srv/mw", j),
		"a crew agent's":      Preamble(CrewDomain("helper"), CrewDesk("/srv/mw", "helper"), j),
	}
	for what, s := range texts {
		for _, word := range []string{"emoji", "Lucide", "system icon"} {
			if strings.Contains(s, word) {
				t.Errorf("%s speaks of %q: a design rule is the operator's (settings: agents.rules)", what, word)
			}
		}
	}
	if OperatorRules("") != "" || OperatorRules(" \n\t") != "" {
		t.Error("no rules still say something")
	}
}
