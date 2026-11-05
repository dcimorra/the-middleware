package chat

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dcimorra/the-middleware/internal/agent"
)

// A site's agent asked why its tasks failed answered that their logs were
// out of its reach, more than once: the preamble said "its tasks are off
// limits" and named the commands nowhere. Now every site conversation is
// told, before its first word, what it may ask the house, the rule to try
// that before saying it cannot, and what each denied house path is read
// with.
func TestASiteConversationIsToldWhatItCanAskTheHouse(t *testing.T) {
	root := "/srv/middleware"
	j := agent.Jail{Deny: []string{
		filepath.Join(root, "tasks"),
		filepath.Join(root, "data", "logs", "tasks"),
		filepath.Join(root, "data", "events"),
		filepath.Join(root, "settings.json"),
	}}
	s := Preamble("shop.test", filepath.Join(root, "sites", "shop.test"), j)
	for _, c := range TurnCommands {
		want := "`middleware " + strings.ReplaceAll(c.Args, "<domain>", "shop.test") + "`"
		if !strings.Contains(s, want) {
			t.Errorf("preamble lacks %s", want)
		}
	}
	for _, want := range []string{
		"Before you say you cannot see or do something about this site",
		"Never write a denied path in a reply or a command",
		filepath.Join(root, "tasks") + " (for your site's tasks: `middleware site tasks shop.test`)",
		filepath.Join(root, "data", "logs", "tasks") + " (for a task's log: `middleware site tasks shop.test log <task>`)",
		filepath.Join(root, "data", "events") + " (for your site's events: `middleware events --subject shop.test`)",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("preamble lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "settings.json (") {
		t.Errorf("a path the house reads nothing from for a site got a hint:\n%s", s)
	}
	if strings.Contains(s, "its tasks or its root are off limits, and") {
		t.Error("the bare \"its tasks are off limits\" is back")
	}

	// The house's own conversation reads tasks/ and data/ directly.
	h := Preamble(Middleware, root, j)
	if strings.Contains(h, "middleware site tasks <domain>") || strings.Contains(h, "(for ") {
		t.Errorf("the house's conversation got the site's list:\n%s", h)
	}
}

// The guide says the same as the preamble: each command, the way a site's
// agent finds it in § 2 when it does open the guide.
func TestTheGuideListsEveryCommandASiteMayRun(t *testing.T) {
	skill := string(SkillContent())
	for _, c := range TurnCommands {
		var words []string
		for _, w := range strings.Fields(c.Args) {
			if w == "<domain>" {
				w = "shop.example.com"
			} else if strings.HasPrefix(w, "<") || strings.HasPrefix(w, "[") {
				break
			}
			words = append(words, w)
		}
		if want := "middleware " + strings.Join(words, " "); !strings.Contains(skill, want) {
			t.Errorf("SKILL.md lacks %q", want)
		}
	}
	if !strings.Contains(skill, "before saying you cannot see or do something") {
		t.Error("the skill's description does not say to read it before saying you cannot")
	}
}
