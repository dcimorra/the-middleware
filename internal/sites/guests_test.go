package sites

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/dcimorra/the-middleware/internal/guests"
)

func (f *fixture) invite(t *testing.T, host string, in map[string]string) {
	t.Helper()
	raw, _ := json.Marshal(in)
	if _, err := f.m.Patch(host, map[string]json.RawMessage{"guests": raw}); err != nil {
		t.Fatal(err)
	}
}

// A pass is given to a site and the manifest can only write its name: a
// guest that is deleted or archived leaves every list it was on, so the
// next site made with that name does not enter where the first one did.
// A host whose manifest no longer parses is rewritten all the same.
func TestADeletedGuestLeavesEveryList(t *testing.T) {
	for _, keep := range []bool{false, true} {
		f := newFixture(t, false)
		for _, d := range []string{"old.test", "broken.test", "new.test", "other.test"} {
			f.m.Create(d, nil, "static")
		}
		f.invite(t, "old.test", map[string]string{"new.test": "read", "other.test": "write"})
		broken := filepath.Join(f.m.Dir("broken.test"), "site.json")
		os.WriteFile(broken, []byte(`{"version":2,"run":{"command":7},"guests":{"new.test":"write"}}`), 0o644)
		if got := guests.Of(f.root, "new.test"); len(got) != 2 {
			t.Fatalf("before: %+v", got)
		}
		if _, err := f.m.Delete(context.Background(), "new.test", keep); err != nil {
			t.Fatal(err)
		}
		// The name is taken again by another site.
		f.m.Create("new.test", nil, "static")
		if got := guests.Of(f.root, "new.test"); len(got) != 0 {
			t.Fatalf("keep=%v: a new site with a deleted guest's name enters %+v", keep, got)
		}
		if in := guests.In(f.m.Dir("old.test")); !reflect.DeepEqual(in, map[string]string{"other.test": "write"}) {
			t.Fatalf("keep=%v: the other guest's pass: %v", keep, in)
		}
		if raw, _ := os.ReadFile(broken); !json.Valid(raw) || len(guests.In(f.m.Dir("broken.test"))) != 0 {
			t.Fatalf("keep=%v: the unparsed host still names it:\n%s", keep, raw)
		}
		f.waitEvent(t, "guests.removed")
	}
}

// A rename is the same site: its passes follow it to the new name, and
// the lists it keeps itself are its own and go with its folder.
func TestARenamedGuestKeepsItsPasses(t *testing.T) {
	f := newFixture(t, false)
	for _, d := range []string{"old.test", "new.test", "other.test"} {
		f.m.Create(d, nil, "static")
	}
	f.invite(t, "old.test", map[string]string{"new.test": "write"})
	f.invite(t, "new.test", map[string]string{"other.test": "read"})
	if _, err := f.m.Rename(context.Background(), "new.test", "next.test"); err != nil {
		t.Fatal(err)
	}
	if in := guests.In(f.m.Dir("old.test")); !reflect.DeepEqual(in, map[string]string{"next.test": "write"}) {
		t.Fatalf("the host's list: %v", in)
	}
	if got := guests.Of(f.root, "other.test"); len(got) != 1 || got[0].Host != "next.test" {
		t.Fatalf("the renamed site's own guests: %+v", got)
	}
}
