package api

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/relocate"
	goruntime "github.com/dcimorra/the-middleware/internal/runtime"
)

// volumeDockers gives each root a docker of its own that knows volumes
// and nothing else: a volume is the tar it holds and the site it is
// labelled with, under <root>/docker. What the house asks of docker to
// list, read, make and fill a volume is answered; nothing runs.
func volumeDockers(t *testing.T) {
	t.Helper()
	old := volumeDocker
	t.Cleanup(func() { volumeDocker = old })
	volumeDocker = func(root string) goruntime.Docker {
		state := filepath.Join(root, "docker")
		bin := filepath.Join(state, "docker")
		if _, err := os.Stat(bin); err != nil {
			os.MkdirAll(state, 0o755)
			os.WriteFile(bin, []byte(`#!/bin/sh
S=`+state+`
last() { for a; do :; done; echo "$a"; }
case "$1 $2" in
"volume ls")
	site=${5#label=middleware.site=}
	for f in "$S"/*.site; do [ -f "$f" ] && [ "$(cat "$f")" = "$site" ] && basename "$f" .site; done; exit 0;;
"volume inspect")
	n=$(last "$@"); [ -f "$S/$n.site" ] || exit 1
	printf '{"middleware.site":"%s"}\n' "$(cat "$S/$n.site")"; exit 0;;
"volume create")
	n=$(last "$@"); printf '%s' "${4#middleware.site=}" > "$S/$n.site"; : > "$S/$n.tar"; exit 0;;
"volume rm")
	n=$(last "$@"); rm -f "$S/$n.site" "$S/$n.tar"; exit 0;;
"image inspect") exit 0;;
"cp -") id=${3%:/}; cat > "$S/$id.tar"; exit 0;;
esac
case "$1" in
ps) exit 0;;
create) v=${5%%:*}; echo "$v"; exit 0;;
cp) id=${2%:/volume}; cat "$S/$id.tar"; exit 0;;
rm) exit 0;;
esac
exit 1
`), 0o755)
		}
		return goruntime.Docker{Bin: bin}
	}
}

func putVolume(t *testing.T, root, name, site string, tar []byte) {
	t.Helper()
	d := volumeDocker(root)
	state := filepath.Dir(d.Bin)
	if err := os.WriteFile(filepath.Join(state, name+".site"), []byte(site), 0o644); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(state, name+".tar"), tar, 0o644)
}

func volumeTarOf(t *testing.T, root, name string) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := volumeDocker(root).ReadVolume(context.Background(), name, &buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A site with a docker volume moves between two servers through their
// own routes, the volume gzipped over the node's PUT and answered with
// what the node read back; its database copies under data/backups go in
// the bundle. The volume stays where the site was, listed.
func TestASitesVolumeAndCopiesCrossBetweenTwoServers(t *testing.T) {
	volumeDockers(t)
	a, b := newFixture(t).withToken(), newFixture(t).withToken()
	dir := filepath.Join(a.root, "sites", "v.test")
	os.MkdirAll(filepath.Join(dir, "public"), 0o755)
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(`{"version":2,"static":{"root":"public"}}`), 0o644)
	copyPath := filepath.Join("data", "backups", "v.test", "db", "20261112T033000Z-scheduled.sql.gz")
	os.MkdirAll(filepath.Dir(filepath.Join(a.root, copyPath)), 0o700)
	os.WriteFile(filepath.Join(a.root, copyPath), []byte("the night's dump"), 0o600)
	db := relocateTar(t, "every order")
	putVolume(t, a.root, "mw-v-test-db", "v.test", db)
	putVolume(t, a.root, "mw-other-test-db", "other.test", relocateTar(t, "another site's"))

	mgr, _ := a.srv.sitesManager()
	m := &relocate.Mover{Root: a.root, Source: siteSource{s: a.srv, mgr: mgr}, Poll: 20 * time.Millisecond, ReadyWait: 5 * time.Second}
	var said []string
	if _, err := m.Move(context.Background(), "v.test", meshTo(b), func(step, msg string) {
		if step == relocate.StepVolumes {
			said = append(said, msg)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if got := volumeTarOf(t, b.root, "mw-v-test-db"); !bytes.Equal(got, db) {
		t.Fatalf("the volume on the target: %d bytes of %d", len(got), len(db))
	}
	if owner, _ := os.ReadFile(filepath.Join(b.root, "docker", "mw-v-test-db.site")); string(owner) != "v.test" {
		t.Fatalf("the volume there is %q's", owner)
	}
	if _, err := os.Stat(filepath.Join(b.root, "docker", "mw-other-test-db.tar")); err == nil {
		t.Fatal("another site's volume went along")
	}
	if len(said) != 2 || !strings.Contains(said[1], "1 files") {
		t.Fatalf("what the move said: %q", said)
	}
	if got, _ := os.ReadFile(filepath.Join(b.root, copyPath)); string(got) != "the night's dump" {
		t.Fatal("the copies of its database did not come along")
	}
	if got := volumeTarOf(t, a.root, "mw-v-test-db"); !bytes.Equal(got, db) {
		t.Fatal("the volume where the site was is not as it was")
	}
	listed := false
	for _, l := range relocate.Leftovers(a.root, time.Now()) {
		listed = listed || l.Path == "docker-volume:mw-v-test-db"
	}
	if !listed {
		t.Fatal("the volume left where the site was is not listed")
	}

	// The same site again: the node that has its volume says so before
	// anything moves, and nothing is written over.
	os.MkdirAll(filepath.Join(dir, "public"), 0o755)
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(`{"version":2,"static":{"root":"public"}}`), 0o644)
	bm, _ := b.srv.sitesManager()
	if _, err := bm.Delete(context.Background(), "v.test", false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Move(context.Background(), "v.test", meshTo(b), nil); err == nil || !strings.Contains(err.Error(), "mw-v-test-db is already on this node") {
		t.Fatalf("moving onto a node that has the volume: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "site.json")); strings.Contains(string(got), "disabled") {
		t.Fatal("a refused move stopped the site")
	}
}

// A node from before volumes went with a site sends its folder alone.
// Received, the site would start its database on an empty volume: the
// first copy is refused, while the site still serves where it is.
func TestASiteSentWithoutItsVolumesIsRefusedAtTheFirstCopy(t *testing.T) {
	volumeDockers(t)
	a, b := newFixture(t).withToken(), newFixture(t).withToken()
	dir := filepath.Join(a.root, "sites", "o.test")
	os.MkdirAll(dir, 0o755)
	manifest := `{"version":2,"disabled":true,"services":{"db":{"internal":true,"run":{"command":["postgres"],"runtime":"docker","docker":{"image":"postgres:17","volumes":["db:/var/lib/postgresql/data"]}}}}}`
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(manifest), 0o644)
	// httpTarget sends as a node did before: no word about volumes.
	m := &relocate.Mover{Root: a.root, Source: olderSource{root: a.root}, Poll: 20 * time.Millisecond, ReadyWait: time.Second}
	_, err := m.Move(context.Background(), "o.test", httpTarget{f: b}, nil)
	if err == nil || !strings.Contains(err.Error(), "leaves a site's docker volumes behind (mw-o-test-db)") {
		t.Fatalf("err: %v", err)
	}
	if _, err := os.Stat(filepath.Join(b.root, "data", "relocate", "o.test")); err == nil {
		t.Fatal("what was received stayed in the staging")
	}
	if _, err := os.Stat(filepath.Join(b.root, "sites", "o.test")); err == nil {
		t.Fatal("the site was put in place")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "site.json")); string(got) != manifest {
		t.Fatal("the site was touched where it is")
	}
}

// olderSource is a source that knows nothing of volumes.
type olderSource struct{ root string }

func (s olderSource) Stop(_ context.Context, d string) ([]byte, error) {
	return os.ReadFile(filepath.Join(s.root, "sites", d, "site.json"))
}
func (s olderSource) Start(string, []byte) error                              { return nil }
func (s olderSource) Published(string) (string, bool)                         { return "", false }
func (s olderSource) Names(string) []string                                   { return nil }
func (s olderSource) Unpublish(context.Context, string)                       {}
func (s olderSource) Archive(context.Context, string, []byte) (string, error) { return "", nil }

// relocateTar is a volume's tar with one file in it.
func relocateTar(t *testing.T, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range []*tar.Header{
		{Name: "volume/", Typeflag: tar.TypeDir, Mode: 0o1777, Uid: 999, Gid: 999},
		{Name: "volume/pgdata/", Typeflag: tar.TypeDir, Mode: 0o700, Uid: 1000, Gid: 989},
		{Name: "volume/pgdata/orders", Typeflag: tar.TypeReg, Mode: 0o600, Uid: 1000, Gid: 989, Size: int64(len(body))},
	} {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write([]byte(body))
		}
	}
	tw.Close()
	return buf.Bytes()
}
