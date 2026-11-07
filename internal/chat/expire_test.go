package chat

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/agent"
)

// The site door's conversations go three days after their last turn, with
// their attachments, on every site; the panel's stay, and so does a recent
// one, and one asking a question goes all the same.
func TestExpireDoorsForgetsTheSiteCodesOldConversations(t *testing.T) {
	p := &fakeProvider{key: "fake", script: []agent.Event{{Kind: agent.KindResult, Text: "ok"}}}
	m, root, _ := newManager(t, p)
	now := time.Now().UTC()
	old := now.Add(-DoorMaxAge - time.Hour)
	write(t, m, "a.test", "", OriginPanel, "la del panel", old, false)
	write(t, m, "a.test", "c1", OriginPanel, "otra del panel", old, false)
	write(t, m, "a.test", "d0", OriginSite, "vieja", old, false)
	write(t, m, "a.test", "d1", OriginSite, "pregunta vieja", old, true)
	write(t, m, "a.test", "d2", OriginSite, "reciente", now.Add(-DoorMaxAge+time.Hour), false)
	write(t, m, "b.test", "d0", OriginSite, "vieja de b", old, false)
	up := filepath.Join(root, "data", "homes", "a.test", "uploads", "d0")
	if err := os.MkdirAll(up, 0o700); err != nil {
		t.Fatal(err)
	}

	if n := m.ExpireDoors(DoorMaxAge, now); n != 3 {
		t.Fatalf("expired %d", n)
	}
	var left []string
	for _, s := range m.List("a.test") {
		left = append(left, s.ID)
	}
	if len(left) != 2 || left[0] != "d2" || left[1] != "c1" {
		t.Fatalf("left in a.test: %v", left)
	}
	if _, ok := m.Main("a.test"); !ok {
		t.Fatal("the panel's first conversation went")
	}
	if l := m.List("b.test"); len(l) != 0 {
		t.Fatalf("left in b.test: %+v", l)
	}
	if _, err := os.Stat(up); !os.IsNotExist(err) {
		t.Fatal("the attachments survived:", err)
	}
}

// A door turn at work is passed over and the others go: the program is
// using it now.
func TestExpireDoorsPassesOverATurnAtWork(t *testing.T) {
	p := &fakeProvider{key: "fake", script: []agent.Event{{Kind: agent.KindResult, Text: "ok"}}, hold: make(chan struct{})}
	m, _, _ := newManager(t, p)
	now := time.Now().UTC()
	write(t, m, "a.test", "d0", OriginSite, "vieja", now, false)
	door, _ := m.Create("a.test", CreateOptions{Origin: OriginSite, Provider: "fake"})
	if _, err := m.Send(door, "hola", ""); err != nil {
		t.Fatal(err)
	}
	defer m.Wait(context.Background(), door, 5*time.Second)
	defer close(p.hold)

	if n := m.ExpireDoors(DoorMaxAge, now.Add(2*DoorMaxAge)); n != 1 {
		t.Fatalf("expired %d", n)
	}
	if l := m.List("a.test"); len(l) != 1 || l[0].Name != door {
		t.Fatalf("%+v", l)
	}
}
