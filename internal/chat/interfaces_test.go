package chat

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dcimorra/the-middleware/internal/agent"
)

// Every agent that may draw something is told the rule up front. It used
// to be said by hand in prompt after prompt ("no system icons"), which is
// how it is known to work, and how it is known to be forgotten.
func TestEveryPreambleCarriesTheInterfaceRule(t *testing.T) {
	root := t.TempDir()
	j := agent.Jail{Home: filepath.Join(root, "home")}
	for name, s := range map[string]string{
		"a site":                               Preamble("shop.test", filepath.Join(root, "sites", "shop.test"), j),
		"the house":                            Preamble(Middleware, root, j),
		"the network agent":                    Preamble(Autopilot, root, j),
		"a crew agent":                         Preamble(CrewDomain("helper"), CrewDesk(root, "helper"), j),
		"a site's own conversation that edits": SiteSystem(Conversation{Domain: "shop.test", Tools: "perms:read,edit"}),
		"a site's own conversation with every tool": SiteSystem(Conversation{Domain: "shop.test", Tools: "write"}),
	} {
		if !strings.Contains(s, InterfaceRule) {
			t.Errorf("the preamble of %s lacks the interface rule:\n%s", name, s)
		}
	}
	// One that answers a program and cannot write has nothing to draw.
	if s := SiteSystem(Conversation{Domain: "shop.test", Tools: "none"}); strings.Contains(s, InterfaceRule) {
		t.Errorf("a conversation without tools is told the interface rule:\n%s", s)
	}
}

func TestTheRuleSaysWhatAnIconIsAndThatNothingIsBare(t *testing.T) {
	for _, want := range []string{"No emoji", "Unicode symbol", "inline SVG line icon", "Lucide", "stroke=\"currentColor\"", "never a bare one", "hover, focus, active and disabled", "empty, loading and error"} {
		if !strings.Contains(InterfaceRule, want) {
			t.Errorf("the rule lacks %q", want)
		}
	}
}

// The guide says the same, with the example to copy.
func TestTheSkillCarriesTheInterfaceRule(t *testing.T) {
	s := string(SkillContent())
	for _, want := range []string{"### Interfaces: icons and finish", "Icons are never system icons", "inline SVG line icon from one consistent outline set", "An interface is finished work, never a bare one", "No system icons, no bare interface"} {
		if !strings.Contains(s, want) {
			t.Errorf("the skill lacks %q", want)
		}
	}
}
