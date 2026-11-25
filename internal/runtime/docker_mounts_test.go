package runtime

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dcimorra/the-middleware/internal/manifest"
)

// A site in a container writes its own folder, and the house reads that
// folder's site.json on the machine: if the container could rewrite it, a
// `runtime: "process"` of its own would run outside the walls.
func TestAContainerCannotRewriteItsManifest(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, manifest.FileName), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(dir)
	spec := &manifest.Spec{Domain: "a.test", SiteDir: dir}
	args, err := siteMounts(spec, filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(real, manifest.FileName) + ":" + filepath.Join(dir, manifest.FileName) + ":ro"
	i := slices.Index(args, want)
	if i < 1 || args[i-1] != "-v" {
		t.Fatalf("the manifest is not mounted read-only: %v", args)
	}
	if folder := slices.Index(args, real+":"+dir); folder < 0 || folder > i {
		t.Fatalf("the read-only manifest must come after the folder's mount, or the folder covers it: %v", args)
	}
}

func TestALinkedManifestDoesNotGoIntoAContainer(t *testing.T) {
	dir := t.TempDir()
	other := filepath.Join(t.TempDir(), "site.json")
	if err := os.WriteFile(other, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(dir, manifest.FileName)); err != nil {
		t.Fatal(err)
	}
	spec := &manifest.Spec{Domain: "a.test", SiteDir: dir}
	if args, err := siteMounts(spec, filepath.Join(t.TempDir(), "home")); err == nil {
		t.Fatalf("a linked manifest was mounted: %v", args)
	}
}

// A container sees the folders of the sites that take its site in, each
// at its own path: read-only for a pass to read, and for one to write
// the folder writable with the host's manifest read-only over it, since
// a container that could rewrite any manifest the house reads would run
// a process of its choosing outside its walls. A service's container is
// its site's.
func TestAContainerSeesTheSitesThatTakeItsSiteIn(t *testing.T) {
	dir, reads, writes, linked := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	for _, d := range []string{dir, reads, writes} {
		if err := os.WriteFile(filepath.Join(d, manifest.FileName), []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(reads, manifest.FileName), filepath.Join(linked, manifest.FileName)); err != nil {
		t.Fatal(err)
	}
	Guests = func(site string) ([]string, []string) {
		if site != "a.test" {
			return nil, nil
		}
		return []string{reads}, []string{writes, linked}
	}
	t.Cleanup(func() { Guests = nil })
	real := func(p string) string { r, _ := filepath.EvalSymlinks(p); return r }
	for _, domain := range []string{"a.test", "a.test/web"} {
		args, err := siteMounts(&manifest.Spec{Domain: domain, SiteDir: dir}, filepath.Join(t.TempDir(), "home"))
		if err != nil {
			t.Fatal(err)
		}
		folder := slices.Index(args, real(writes)+":"+writes)
		mf := slices.Index(args, filepath.Join(real(writes), manifest.FileName)+":"+filepath.Join(writes, manifest.FileName)+":ro")
		if folder < 0 || mf < folder {
			t.Fatalf("%s: the host it may write, with its manifest read-only after it: %v", domain, args)
		}
		for _, ro := range []string{reads, linked} {
			if !slices.Contains(args, real(ro)+":"+ro+":ro") {
				t.Fatalf("%s: %s is not mounted read-only: %v", domain, ro, args)
			}
		}
	}
	args, _ := siteMounts(&manifest.Spec{Domain: "b.test", SiteDir: dir}, filepath.Join(t.TempDir(), "home"))
	for _, a := range args {
		if strings.Contains(a, reads) || strings.Contains(a, writes) {
			t.Fatalf("a site nobody takes in got %s", a)
		}
	}
}
