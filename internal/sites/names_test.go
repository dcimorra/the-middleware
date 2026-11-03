package sites

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dcimorra/the-middleware/internal/manifest"
)

// A site born with a made-up name is given its real domain as an alias,
// that one is made the main name, and the made-up one is then taken away:
// the site runs under the real name, with its things, and answers only
// it. Done on a running site, since a site that did not come back up
// under the new name is what this must not do.
func TestAnAliasBecomesTheMainNameAndTheOldOneCanThenGo(t *testing.T) {
	f := newFixture(t, true)
	mf := runManifest()
	mf.Aliases = []string{"real.test", "other.test"}
	mf.ForceHTTPS = []string{"quiet-otter.test", "real.test"}
	if _, err := f.m.Create("quiet-otter.test", mf, ""); err != nil {
		t.Fatal(err)
	}
	waitSite(t, f.m, "quiet-otter.test", func(s Site) bool { return s.Alive })
	os.MkdirAll(filepath.Join(f.m.Root, "data", "homes", "quiet-otter.test"), 0o700)
	os.WriteFile(filepath.Join(f.m.Root, "data", "homes", "quiet-otter.test", "mark"), []byte("1"), 0o600)

	if _, err := f.m.SetMain(context.Background(), "quiet-otter.test", "nope.test"); err == nil {
		t.Fatal("a name the site does not answer was made its main one")
	}
	s, err := f.m.SetMain(context.Background(), "quiet-otter.test", "real.test")
	if err != nil {
		t.Fatal(err)
	}
	if s.Domain != "real.test" {
		t.Fatalf("site is %q", s.Domain)
	}
	if _, err := os.Stat(filepath.Join(f.m.Root, "data", "homes", "real.test", "mark")); err != nil {
		t.Error("its HOME did not come along")
	}
	got, err := manifest.Load(f.m.Dir("real.test"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Aliases, []string{"quiet-otter.test", "other.test"}) {
		t.Errorf("aliases = %v, want the old name in place of the new one", got.Aliases)
	}
	if got.Disabled {
		t.Error("the site came back disabled")
	}
	waitSite(t, f.m, "real.test", func(s Site) bool { return s.Alive })

	if _, err := f.m.RemoveName("real.test", "real.test"); !errors.Is(err, ErrMainName) {
		t.Fatalf("taking away the main name: %v", err)
	}
	if _, err := f.m.RemoveName("real.test", "nope.test"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("taking away a name it does not answer: %v", err)
	}
	if _, err := f.m.RemoveName("real.test", "quiet-otter.test"); err != nil {
		t.Fatal(err)
	}
	got, _ = manifest.Load(f.m.Dir("real.test"))
	if !slices.Equal(got.Aliases, []string{"other.test"}) {
		t.Errorf("aliases = %v", got.Aliases)
	}
	if !slices.Equal(got.ForceHTTPS, []string{"real.test"}) {
		t.Errorf("force_https = %v, the removed name's line must go with it", got.ForceHTTPS)
	}
	if slices.Contains(got.Hosts("real.test"), "quiet-otter.test") {
		t.Error("the removed name is still one of the site's hosts")
	}
	f.m.Delete(context.Background(), "real.test", false)
}

// A site with services: the main name must be a service's domain with no
// path, the old one stays in its service's domains, and taking a name
// away takes every route on it, but never a service's last name.
func TestNamesOfASiteWithServices(t *testing.T) {
	f := newFixture(t, false)
	dir := filepath.Join(f.root, "sites", "random-1.test")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(`{"version":2,"services":{
		"web":{"run":{"command":["node","web.js"]},"domains":["random-1.test","bakery.test","bakery.test/api-docs"]},
		"api":{"run":{"command":["node","api.js"]},"domains":["api.bakery.test","bakery.test/api"]},
		"cms":{"run":{"command":["node","cms.js"]},"domains":["cms.bakery.test"]}},
		"force_https":["random-1.test","bakery.test"]}`), 0o644)

	if _, err := f.m.SetMain(context.Background(), "random-1.test", "www.bakery.test"); err == nil {
		t.Error("a www. that follows its apex was made the main name")
	}
	s, err := f.m.SetMain(context.Background(), "random-1.test", "bakery.test")
	if err != nil {
		t.Fatal(err)
	}
	if s.Domain != "bakery.test" {
		t.Fatalf("site is %q", s.Domain)
	}
	if _, err := f.m.RemoveName("bakery.test", "cms.bakery.test"); err == nil || !strings.Contains(err.Error(), "cms") {
		t.Errorf("a service's last name was taken away: %v", err)
	}
	if _, err := f.m.RemoveName("bakery.test", "www.bakery.test"); err == nil || !strings.Contains(err.Error(), "www") {
		t.Errorf("the apex's www: %v", err)
	}
	if _, err := f.m.RemoveName("bakery.test", "random-1.test"); err != nil {
		t.Fatal(err)
	}
	got, err := manifest.Load(f.m.Dir("bakery.test"))
	if err != nil {
		t.Fatal(err)
	}
	if d := got.Services["web"].Domains; !slices.Equal(d, []string{"bakery.test", "bakery.test/api-docs"}) {
		t.Errorf("web's domains = %v", d)
	}
	if !slices.Equal(got.ForceHTTPS, []string{"bakery.test"}) {
		t.Errorf("force_https = %v", got.ForceHTTPS)
	}
	// The main name cannot go even through a service.
	if _, err := f.m.RemoveName("bakery.test", "bakery.test"); !errors.Is(err, ErrMainName) {
		t.Errorf("the main name: %v", err)
	}
}
