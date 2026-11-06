package chat

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/agent"
)

// write leaves a conversation on disk the way a turn would, with its last
// message at at.
func write(t *testing.T, m *Manager, domain, id, origin, text string, at time.Time, asks bool) {
	t.Helper()
	c := Conversation{Domain: domain, ID: id, Origin: origin, Created: at.Add(-time.Minute)}
	if text != "" {
		c.Turns = []Turn{{ID: "u", Role: "user", Text: text, At: at.Add(-time.Second)}, {ID: "a", Role: "assistant", Text: "ok", At: at}}
		if asks {
			c.Turns[1].Questions = []Question{{Question: "¿Sí o no?", Options: []string{"sí", "no"}}}
		}
	}
	if err := m.save(c); err != nil {
		t.Fatal(err)
	}
}

func names(rs []RecentChat) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Name)
	}
	return out
}

// The bar's list is the conversations last used, of every site and of the
// house, the last first: the way back to one without going node, site,
// chat and list. What waits for an answer stays in it past the limit,
// since the button counts it; a site's own and an empty one are not in it.
func TestRecentListsTheLastUsedFirstAndKeepsWhatWaits(t *testing.T) {
	p := &fakeProvider{key: "fake", script: []agent.Event{{Kind: agent.KindResult, Text: "ok"}}}
	m, _, _ := newManager(t, p)
	now := time.Now().UTC()
	write(t, m, "a.test", "", OriginPanel, "primera de a", now.Add(-4*time.Hour), true)
	write(t, m, "a.test", "a1", OriginPanel, "otra de a", now.Add(-time.Hour), false)
	write(t, m, Middleware, "", OriginPanel, "la casa", now.Add(-2*time.Hour), false)
	write(t, m, "b.test", "b2", OriginPanel, "la de b", now.Add(-3*time.Hour), false)
	write(t, m, "a.test", "d0", OriginSite, "de un programa", now, false)
	write(t, m, "b.test", "e0", OriginPanel, "", now, false)

	all := m.Recent(0)
	if got := names(all); len(got) != 4 || got[0] != "a.test#a1" || got[1] != Middleware || got[2] != "b.test#b2" || got[3] != "a.test" {
		t.Fatalf("%v", got)
	}
	if all[1].Title != "la casa" || all[1].Turns != 2 || !all[3].Asking || all[0].Asking {
		t.Fatalf("%+v", all)
	}
	// Two by use, and the oldest because it asks.
	if got := names(m.Recent(2)); len(got) != 3 || got[0] != "a.test#a1" || got[1] != Middleware || got[2] != "a.test" {
		t.Fatalf("%v", got)
	}
	// Written again, it is read again: the cache follows the file.
	write(t, m, "b.test", "b2", OriginPanel, "la de b", now.Add(time.Minute), false)
	if got := names(m.Recent(1)); got[0] != "b.test#b2" {
		t.Fatalf("%v", got)
	}
	// Answered, it falls out of the short list.
	write(t, m, "a.test", "", OriginPanel, "primera de a", now.Add(-4*time.Hour), false)
	if got := names(m.Recent(1)); len(got) != 1 {
		t.Fatalf("%v", got)
	}
}

// The whole purge forgets every conversation the panel holds on the node,
// every site's and the house's, with their attachments, and passes over
// the one at work instead of refusing all of them; a site's own go too.
func TestPurgeAllForgetsEveryPanelConversationButTheOneAtWork(t *testing.T) {
	p := &fakeProvider{key: "fake", script: []agent.Event{{Kind: agent.KindResult, Text: "ok"}}, hold: make(chan struct{})}
	m, root, _ := newManager(t, p)
	now := time.Now().UTC()
	write(t, m, "a.test", "a1", OriginPanel, "otra de a", now, false)
	write(t, m, Middleware, "", OriginPanel, "la casa", now, false)
	write(t, m, "b.test", "", OriginPanel, "la de b", now, true)
	write(t, m, "a.test", "d0", OriginSite, "de un programa", now, false)
	up := m.uploadsDir("a.test#a1")
	os.MkdirAll(up, 0o700)
	os.WriteFile(filepath.Join(up, "a.txt"), []byte("x"), 0o600)
	if _, err := m.Send("a.test", "uno", ""); err != nil {
		t.Fatal(err)
	}
	defer m.Wait(context.Background(), "a.test", 5*time.Second)
	defer close(p.hold)

	n, busy, err := m.PurgeAll()
	if err != nil || n != 4 || busy != 1 {
		t.Fatalf("purged %d, busy %d, %v", n, busy, err)
	}
	if got := names(m.Recent(0)); len(got) != 1 || got[0] != "a.test" {
		t.Fatalf("left: %v", got)
	}
	if _, err := os.Stat(filepath.Join(root, "data", "chat", "a.test", "d0.json")); !os.IsNotExist(err) {
		t.Fatal("the site's own conversation survived:", err)
	}
	if _, err := os.Stat(up); !os.IsNotExist(err) {
		t.Fatal("the attachments survived")
	}
	if w := m.Waiting(); len(w.Asking) != 0 {
		t.Fatalf("a purged conversation still waits: %+v", w.Asking)
	}
}
