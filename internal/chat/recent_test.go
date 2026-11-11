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
	// The agents' sessions are in the bar, so they go with the rest.
	write(t, m, Autopilot, "c1", OriginPanel, "Session of 2026-11-11 06:06", now, false)
	if err := os.MkdirAll(CrewDesk(root, "instagram"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, m, CrewPrefix+"instagram", "c2", OriginPanel, "Session of 2026-11-09 14:54", now, false)
	if got := names(m.Recent(0)); len(got) != 5 {
		t.Fatalf("before the purge the bar lists %v", got)
	}
	up := m.uploadsDir("a.test#a1")
	os.MkdirAll(up, 0o700)
	os.WriteFile(filepath.Join(up, "a.txt"), []byte("x"), 0o600)
	if _, err := m.Send("a.test", "uno", ""); err != nil {
		t.Fatal(err)
	}
	defer m.Wait(context.Background(), "a.test", 5*time.Second)
	defer close(p.hold)

	n, busy, err := m.PurgeAll()
	if err != nil || n != 6 || busy != 1 {
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

// A site that moved to another node leaves its conversations on this one,
// and when it comes back they are set aside as "<domain>.before-…" to make
// room for the ones that travelled with it. Neither can be opened here
// ("no such site"), so neither is in the bar nor counted as waiting: one
// pending question showed three times after a move there and back.
func TestRecentAndWaitingLeaveOutConversationsWhoseSiteIsNotHere(t *testing.T) {
	p := &fakeProvider{key: "fake", script: []agent.Event{{Kind: agent.KindResult, Text: "ok"}}}
	m, root, _ := newManager(t, p)
	now := time.Now().UTC()
	write(t, m, "a.test", "", OriginPanel, "la que viajó con el sitio", now, true)
	write(t, m, "a.test", "a1", OriginPanel, "otra", now.Add(-time.Minute), true)
	// What relocate's commit does with the copies an earlier life left.
	chats := filepath.Join(root, "data", "chat")
	for _, name := range []string{"a.test.json", "a.test"} {
		if err := os.Rename(filepath.Join(chats, name), filepath.Join(chats, name+".before-20261111T115200Z")); err != nil {
			t.Fatal(err)
		}
	}
	write(t, m, "a.test", "", OriginPanel, "la que viajó con el sitio", now, true)
	// And a site that left: its folder is gone, its conversations are not.
	write(t, m, "b.test", "", OriginPanel, "pendiente", now, true)
	if err := os.RemoveAll(filepath.Join(root, "sites", "b.test")); err != nil {
		t.Fatal(err)
	}

	if got := names(m.Recent(0)); len(got) != 1 || got[0] != "a.test" {
		t.Fatalf("the bar lists %v, want only a.test", got)
	}
	if w := m.Waiting(); len(w.Asking) != 1 || w.Asking[0].Name != "a.test" {
		t.Fatalf("waiting: %+v", w.Asking)
	}
	// The site back, its conversations are listed again: nothing was removed.
	if err := os.MkdirAll(filepath.Join(root, "sites", "b.test"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := names(m.Recent(0)); len(got) != 2 {
		t.Fatalf("with b.test back the bar lists %v", got)
	}
}
