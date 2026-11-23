package guests

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func site(t *testing.T, root, name, manifest string) string {
	t.Helper()
	dir := filepath.Join(root, "sites", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if manifest != "" {
		if err := os.WriteFile(filepath.Join(dir, "site.json"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// Only the host's manifest gives a pass: what the guest writes in its
// own (guests naming itself, the old chat.reads) gives nothing, and an
// entry that is not exactly "read" or "write" is no pass.
func TestOnlyTheHostGivesAPass(t *testing.T) {
	root := t.TempDir()
	old := site(t, root, "old.test", `{"version":2,"static":{},"guests":{"new.test":"read","admin":"write","x.test":"all","y.test":true}}`)
	site(t, root, "new.test", `{"version":2,"static":{},"chat":{"reads":["other.test"]},"guests":{"new.test":"write"}}`)
	site(t, root, "other.test", `{"version":2,"static":{}}`)
	site(t, root, "admin", `{"version":2}`)
	// A manifest the engine would refuse still says who its site takes in.
	broken := site(t, root, "broken.test", `{"version":2,"run":{"command":7},"guests":{"new.test":"write"}}`)
	// And a file that is not JSON says nothing.
	site(t, root, "garbage.test", `{"guests":{"new.test":"write"}`)

	got := Of(root, "new.test")
	want := []Grant{{Host: "broken.test", Dir: broken, Write: true}, {Host: "old.test", Dir: old}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if got := Of(root, "admin"); len(got) != 1 || got[0].Host != "old.test" || !got[0].Write {
		t.Fatalf("a project is a guest like a site: %+v", got)
	}
	for _, none := range []string{"other.test", "x.test", "y.test", "", "../old.test"} {
		if got := Of(root, none); len(got) != 0 {
			t.Errorf("%q enters %+v", none, got)
		}
	}
	read, write := Folders(got)
	if !reflect.DeepEqual(read, []string{old}) || !reflect.DeepEqual(write, []string{broken}) {
		t.Fatalf("folders: read %v write %v", read, write)
	}
	if in := In(old); !reflect.DeepEqual(in, map[string]string{"new.test": "read", "admin": "write"}) {
		t.Fatalf("In: %v", in)
	}
}
