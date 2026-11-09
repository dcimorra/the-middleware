package chat

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/fsx"
)

// An agent opens a conversation at every wake: Keep leaves the last n,
// by their last turn, and never one at work.
func TestKeepLeavesTheLastConversations(t *testing.T) {
	root := t.TempDir()
	m := New(Options{Root: root})
	dir := filepath.Join(root, "data", "chat", "crew~x")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	for i, id := range []string{"a1", "a2", "a3", "a4"} {
		c := Conversation{Origin: OriginPanel, Created: base.Add(time.Duration(i) * time.Hour)}
		if err := fsx.WriteJSON(filepath.Join(dir, id+".json"), c, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if n := m.Keep("crew~x", 2); n != 2 {
		t.Fatalf("forgot %d", n)
	}
	for id, want := range map[string]bool{"a1": false, "a2": false, "a3": true, "a4": true} {
		_, err := os.Stat(filepath.Join(dir, id+".json"))
		if (err == nil) != want {
			t.Errorf("%s kept %v, want %v", id, err == nil, want)
		}
	}
	if n := m.Keep("crew~x", 2); n != 0 {
		t.Errorf("forgot %d more", n)
	}
}
