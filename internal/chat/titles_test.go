package chat

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/agent"
	"github.com/dcimorra/the-middleware/internal/events"
)

func send(t *testing.T, m *Manager, name, text string, ch <-chan events.Event) {
	t.Helper()
	if _, err := m.Send(name, text, ""); err != nil {
		t.Fatal(err)
	}
	for {
		if ev := waitFor(t, ch, "agent.done"); ev.Subject == name {
			break
		}
	}
	m.Wait(context.Background(), name, 5*time.Second)
}

func TestAConversationIsTitledByItsFirstMessageAndListedOnlyOnceItHasOne(t *testing.T) {
	p := &fakeProvider{key: "fake", script: []agent.Event{{Kind: agent.KindResult, Text: "ok"}}}
	m, _, bus := newManager(t, p)
	ch, unsub := bus.Subscribe()
	defer unsub()

	empty, _ := m.Create("a.test", CreateOptions{Origin: OriginPanel})
	if len(m.List("a.test")) != 0 {
		t.Fatal("a conversation without a message is listed")
	}
	if _, ok := m.Main("a.test"); ok {
		t.Fatal("an untouched first conversation is listed")
	}
	send(t, m, "a.test", "Arregla el formulario de contacto\nque no envía", ch)
	time.Sleep(10 * time.Millisecond)
	send(t, m, empty, "  Cambia   el color del botón  ", ch)

	list := m.List("a.test")
	if len(list) != 1 || list[0].Title != "Cambia el color del botón" {
		t.Fatalf("%+v", list)
	}
	main, ok := m.Main("a.test")
	if !ok || main.Title != "Arregla el formulario de contacto" || main.ID != "" {
		t.Fatalf("%+v", main)
	}
	// The last used first.
	if !list[0].Updated.After(main.Updated) {
		t.Fatalf("%v is not after %v", list[0].Updated, main.Updated)
	}
	// A later message does not rename it.
	send(t, m, empty, "otra cosa", ch)
	if got := m.List("a.test")[0].Title; got != "Cambia el color del botón" {
		t.Fatal(got)
	}
}

func TestTitleOfCutsALongFirstLine(t *testing.T) {
	long := "Necesito que revises todas las páginas del sitio y me digas cuáles cargan lento"
	got := TitleOf("\n\n" + long)
	if []rune(got)[len([]rune(got))-1] != '…' || len([]rune(got)) > 61 {
		t.Fatalf("%q", got)
	}
}

func TestPurgeForgetsEveryConversationOfTheSiteTheDoorsToo(t *testing.T) {
	p := &fakeProvider{key: "fake", script: []agent.Event{{Kind: agent.KindResult, Text: "ok"}}}
	m, root, bus := newManager(t, p)
	ch, unsub := bus.Subscribe()
	defer unsub()
	other, _ := m.Create("a.test", CreateOptions{Origin: OriginPanel})
	door, _ := m.Create("a.test", CreateOptions{Origin: OriginSite})
	send(t, m, "a.test", "uno", ch)
	send(t, m, other, "dos", ch)
	// The door's turns run Claude; written here, the way a turn leaves it.
	c, _ := m.Conversation(door)
	c.Turns = append(c.Turns, Turn{ID: "t", Role: "user", Text: "tres", At: time.Now()})
	if err := m.save(c); err != nil {
		t.Fatal(err)
	}
	up := m.uploadsDir(other)
	os.MkdirAll(up, 0o700)
	os.WriteFile(filepath.Join(up, "a.txt"), []byte("x"), 0o600)

	n, err := m.Purge("a.test")
	if err != nil || n != 3 {
		t.Fatalf("%d %v", n, err)
	}
	if _, ok := m.Main("a.test"); ok {
		t.Fatal("the first conversation survived")
	}
	// The site door's went too: they filled the site's list after the
	// operator had deleted everything.
	if list := m.List("a.test"); len(list) != 0 {
		t.Fatalf("%+v", list)
	}
	if _, err := os.Stat(up); !os.IsNotExist(err) {
		t.Fatal("the attachments survived")
	}
	if _, err := os.Stat(filepath.Join(root, "data", "chat", "a.test.json")); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestPurgeRefusesWhileATurnIsAtWork(t *testing.T) {
	p := &fakeProvider{key: "fake", script: []agent.Event{{Kind: agent.KindResult, Text: "ok"}}, hold: make(chan struct{})}
	m, _, _ := newManager(t, p)
	if _, err := m.Send("a.test", "uno", ""); err != nil {
		t.Fatal(err)
	}
	defer m.Wait(context.Background(), "a.test", 5*time.Second)
	defer close(p.hold)
	if _, err := m.Purge("a.test"); !errors.Is(err, ErrBusy) {
		t.Fatalf("%v", err)
	}
}

// A turn of the site's code at work does not hold the site's purge back:
// a program calling in a loop would keep the button from ever working.
// That one stays, the rest go.
func TestPurgePassesOverADoorTurnAtWork(t *testing.T) {
	p := &fakeProvider{key: "fake", script: []agent.Event{{Kind: agent.KindResult, Text: "ok"}}, hold: make(chan struct{})}
	m, _, _ := newManager(t, p)
	now := time.Now().UTC()
	write(t, m, "a.test", "", OriginPanel, "la del panel", now, false)
	write(t, m, "a.test", "d0", OriginSite, "de un programa", now, false)
	door, _ := m.Create("a.test", CreateOptions{Origin: OriginSite, Provider: "fake"})
	if _, err := m.Send(door, "otra", ""); err != nil {
		t.Fatal(err)
	}
	defer m.Wait(context.Background(), door, 5*time.Second)
	defer close(p.hold)

	n, err := m.Purge("a.test")
	if err != nil || n != 2 {
		t.Fatalf("%d %v", n, err)
	}
	list := m.List("a.test")
	if len(list) != 1 || list[0].Name != door {
		t.Fatalf("%+v", list)
	}
}
