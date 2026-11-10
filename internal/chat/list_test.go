package chat

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// List keeps each conversation's summary and reads its file again only
// when it changed: what it answers must still be what the files say,
// after a turn more, after one is deleted, and with a file that is not a
// conversation among them.
func TestListFollowsTheFilesItNoLongerReadsEveryTime(t *testing.T) {
	m, root, _ := newManager(t, &fakeProvider{key: "fake"})
	now := time.Now().UTC()
	write(t, m, "a.test", "d0", OriginSite, "the first", now.Add(-time.Hour), false)
	write(t, m, "a.test", "d1", OriginSite, "the second", now.Add(-time.Minute), false)
	os.WriteFile(filepath.Join(root, "data", "chat", "a.test", "bad.json"), []byte("{not json"), 0o600)

	ids := func() []string {
		var out []string
		for _, s := range m.List("a.test") {
			out = append(out, s.ID+":"+s.Title)
		}
		return out
	}
	same := func(got []string, want ...string) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}
	for i := 0; i < 2; i++ { // the second time from what was kept
		if got := ids(); !same(got, "d1:the second", "d0:the first") {
			t.Fatalf("pass %d: %v", i, got)
		}
	}
	if len(m.lists) != 3 {
		t.Fatalf("%d summaries kept for three files", len(m.lists))
	}
	// A turn more in the older one: it is the newest now, by its file.
	write(t, m, "a.test", "d0", OriginSite, "the first, again", now, false)
	if got := ids(); !same(got, "d0:the first, again", "d1:the second") {
		t.Fatalf("after a turn: %v", got)
	}
	// One deleted: gone from the list and from what is kept.
	if err := os.Remove(filepath.Join(root, "data", "chat", "a.test", "d1.json")); err != nil {
		t.Fatal(err)
	}
	if got := ids(); !same(got, "d0:the first, again") {
		t.Fatalf("after a delete: %v", got)
	}
	if _, kept := m.lists[Name("a.test", "d1")]; kept {
		t.Fatal("a deleted conversation's summary was kept")
	}
}
