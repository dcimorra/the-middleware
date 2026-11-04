package chat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/agent"
)

// The HOME the attachments live in is the site's to write, and what it
// plants there (links to another site's folder) never makes the house
// write, move or delete outside it: not the upload, not the sweep of
// what was sent, not removing one, not the purge, not the skill.
func TestLinksPlantedInTheHomeDoNotLeadTheHouseOut(t *testing.T) {
	p := &fakeProvider{key: "fake", script: []agent.Event{{Kind: agent.KindResult, Text: "ok"}}}
	m, root, _ := newManager(t, p)
	home := filepath.Join(root, "data", "homes", "a.test")
	victim := filepath.Join(root, "sites", "b.test")
	write := func(p, s string) {
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	intact := func(p, want string) {
		t.Helper()
		if got, err := os.ReadFile(p); err != nil || string(got) != want {
			t.Fatalf("%s was touched: %q %v", p, got, err)
		}
	}
	link := func(target, at string) {
		os.RemoveAll(at)
		os.MkdirAll(filepath.Dir(at), 0o755)
		if err := os.Symlink(target, at); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-48 * time.Hour)

	// sent/ made a link to the other site: the day's sweep removed what
	// was there.
	write(filepath.Join(victim, "keep", "index.html"), "b's page")
	os.Chtimes(filepath.Join(victim, "keep"), old, old)
	link(victim, filepath.Join(home, "uploads", "main", "sent"))
	m.sweep("a.test")
	intact(filepath.Join(victim, "keep", "index.html"), "b's page")

	// pending/<name> a link to the other site's file: the upload wrote
	// through it. Now the link goes and the upload is a file of its own.
	write(filepath.Join(victim, "secret.txt"), "b's secret")
	link(filepath.Join(victim, "secret.txt"), filepath.Join(home, "uploads", "main", "pending", "notes.txt"))
	if _, err := m.AddUpload("a.test", "notes.txt", strings.NewReader("mine")); err != nil {
		t.Fatal(err)
	}
	intact(filepath.Join(victim, "secret.txt"), "b's secret")
	if fi, err := os.Lstat(filepath.Join(home, "uploads", "main", "pending", "notes.txt")); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("the upload is not a file of its own: %v", err)
	}

	// pending/ itself a link: removing an upload removed the other's file,
	// and the upload went there.
	link(victim, filepath.Join(home, "uploads", "main", "pending"))
	if err := m.RemoveUpload("a.test", "secret.txt"); err == nil {
		t.Fatal("an upload was removed through a link")
	}
	if _, err := m.AddUpload("a.test", "secret.txt", strings.NewReader("overwritten")); err == nil {
		t.Fatal("an upload was written through a link")
	}
	intact(filepath.Join(victim, "secret.txt"), "b's secret")
	if ups := m.Uploads("a.test"); len(ups) != 0 {
		t.Fatalf("the other site's files are listed as attachments: %+v", ups)
	}

	// uploads/ a link: the purge removed the other site's folder.
	link(filepath.Join(root, "sites"), filepath.Join(home, "uploads"))
	write(filepath.Join(root, "sites", "main", "x"), "kept")
	if _, err := m.Purge("a.test"); err != nil {
		t.Fatal(err)
	}
	intact(filepath.Join(root, "sites", "main", "x"), "kept")
	intact(filepath.Join(victim, "secret.txt"), "b's secret")

	// .claude a link: the skill went into the other site.
	os.RemoveAll(filepath.Join(home, "uploads"))
	link(victim, filepath.Join(home, ".claude"))
	if err := installSkillFor(agent.Jail{Home: home}, "fake"); err == nil {
		t.Fatal("the skill was installed through a link")
	}
	if _, err := os.Stat(filepath.Join(victim, "skills")); err == nil {
		t.Fatal("the skill was written into another site")
	}
}
