package relocate

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/runtime"
)

// originSource is a source whose volumes are, on the one docker both
// "nodes" of the test share, under another name than the one they move
// as: two volumes of one name cannot be on one daemon.
type originSource struct {
	*localSource
	d      runtime.Docker
	origin map[string]string
}

func (s *originSource) Volumes(context.Context, string) ([]string, error) {
	var out []string
	for v := range s.origin {
		out = append(out, v)
	}
	return out, nil
}

func (s *originSource) VolumeUsers(ctx context.Context, v string) ([]string, error) {
	return s.d.VolumeUsers(ctx, s.origin[v])
}

func (s *originSource) ReadVolume(ctx context.Context, v string, w io.Writer) error {
	return s.d.ReadVolume(ctx, s.origin[v], w)
}

// takingTarget takes volumes through the real Receiver.
type takingTarget struct{ localTarget }

func (t *takingTarget) TakeVolume(ctx context.Context, d, vol string, body io.Reader) (VolumeSum, error) {
	return t.rc.TakeVolume(ctx, d, vol, body)
}

// volumeEntries reads a volume back as name -> "kind mode uid:gid link body".
func volumeEntries(t *testing.T, d runtime.Docker, vol string) map[string]string {
	t.Helper()
	var buf bytes.Buffer
	if err := d.ReadVolume(context.Background(), vol, &buf); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	tr := tar.NewReader(&buf)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		if len(body) > 40 {
			body = []byte(fmt.Sprintf("%d bytes", len(body)))
		}
		out[strings.TrimSuffix(h.Name, "/")] = fmt.Sprintf("%c %o %d:%d %s %s", h.Typeflag, h.Mode&0o7777, h.Uid, h.Gid, h.Linkname, body)
	}
}

// A site whose service keeps a database in a named volume moves with it,
// through a real docker: every file with its owner, its mode and its
// links, the volume's own folder included, into a volume labelled as the
// site's; and the one where it was is still there, untouched. Then the
// same move to a node where the site does not come up: its volume is
// taken out there with it, and nothing is lost where it was.
// DOCKER=1 runs it; it pulls nothing (the volumes are reached through an
// image made here from an empty tar).
func TestARealVolumeMovesWithItsSiteFileForFile(t *testing.T) {
	if os.Getenv("DOCKER") != "1" {
		t.Skip("DOCKER=1 runs it against the machine's docker")
	}
	d := runtime.Docker{}
	ctx := context.Background()
	domain := fmt.Sprintf("carry%d.test", os.Getpid())
	vol := runtime.NamedVolume(domain, "db")
	origin := vol + "-origin"
	rm := func() {
		exec.Command("docker", "volume", "rm", "-f", vol).Run()
		exec.Command("docker", "volume", "rm", "-f", origin).Run()
	}
	rm()
	t.Cleanup(rm)
	if err := d.ClaimVolume(ctx, domain, origin); err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("a row of the orders table\n", 80000)
	filled := volumeTar(t,
		entry{name: "volume/pgdata/", kind: tar.TypeDir, mode: 0o700, uid: 1000, gid: 989},
		entry{name: "volume/pgdata/PG_VERSION", body: "17\n", mode: 0o600, uid: 1000, gid: 989},
		entry{name: "volume/pgdata/base/", kind: tar.TypeDir, mode: 0o700, uid: 1000, gid: 989},
		entry{name: "volume/pgdata/base/16384", body: big, mode: 0o600, uid: 1000, gid: 989},
		entry{name: "volume/pgdata/base/16384.link", kind: tar.TypeLink, link: "volume/pgdata/base/16384", mode: 0o600, uid: 1000, gid: 989},
		entry{name: "volume/pgdata/pg_wal/", kind: tar.TypeDir, mode: 0o700, uid: 1000, gid: 989},
		entry{name: "volume/pgdata/current", kind: tar.TypeSymlink, link: "base/16384", mode: 0o777, uid: 1000, gid: 989},
		entry{name: "volume/run.sh", body: "#!/bin/sh\n", mode: 0o4755, uid: 0, gid: 0},
	)
	if err := d.WriteVolume(ctx, origin, bytes.NewReader(filled)); err != nil {
		t.Fatal(err)
	}
	before := volumeEntries(t, d, origin)
	// The volume's own folder too: a database that is not root and cannot
	// chown has to find its volume its own.
	if before["volume"] != "5 1777 999:999  " || before["volume/pgdata/PG_VERSION"] != "0 600 1000:989  17\n" || before["volume/pgdata"] != "5 700 1000:989  " {
		t.Fatalf("the volume was not filled as the test wrote it: %q", before)
	}
	if got, err := d.SiteVolumes(ctx, domain); err != nil || strings.Join(got, " ") != origin {
		t.Fatalf("the site's volumes: %v %v", got, err)
	}

	move := func(ready bool) (string, error, *originSource) {
		src, dst := t.TempDir(), t.TempDir()
		p := filepath.Join(src, "sites", domain, "site.json")
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(servicesManifest), 0o644)
		s := &originSource{localSource: &localSource{root: src}, d: d, origin: map[string]string{vol: origin}}
		tg := &takingTarget{localTarget{rc: Receiver{Root: dst, Docker: &d}, ready: ready}}
		m := &Mover{Root: src, Source: s, Poll: 10 * time.Millisecond, ReadyWait: 300 * time.Millisecond}
		var said []string
		_, err := m.Move(ctx, domain, tg, func(step, msg string) {
			if step == StepVolumes {
				said = append(said, msg)
			}
		})
		return strings.Join(said, "\n"), err, s
	}

	// It does not come up there: undone, and its volume there with it.
	_, err, s := move(false)
	if err == nil || !strings.Contains(err.Error(), "runs here again") {
		t.Fatalf("err: %v", err)
	}
	if d.VolumeExists(ctx, vol) {
		t.Fatalf("the volume of a move that was undone stayed on the target: %v", err)
	}
	if !strings.Contains(string(s.started), `"services"`) {
		t.Fatalf("it was not started again where it was: %q", s.started)
	}

	said, err, _ := move(true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(said, "3 files") || !strings.Contains(said, "read back from its disk") {
		t.Fatalf("what the move said of the volume: %s", said)
	}
	after := volumeEntries(t, d, vol)
	if len(after) != len(before) {
		t.Fatalf("%d names arrived of %d", len(after), len(before))
	}
	for name, want := range before {
		if after[name] != want {
			t.Fatalf("%s arrived as %q, was %q", name, after[name], want)
		}
	}
	if out := dockerOut(t, "volume", "inspect", "--format", `{{index .Labels "middleware.site"}}`, vol); out != domain {
		t.Fatalf("the volume there is labelled %q", out)
	}
	// Where it was: there, as it was.
	still := volumeEntries(t, d, origin)
	for name, want := range before {
		if still[name] != want {
			t.Fatalf("%s changed where the site was: %q, was %q", name, still[name], want)
		}
	}
	// A second arrival never lays over a volume that is there.
	if _, err, _ := move(true); err == nil || !strings.Contains(err.Error(), "already on this node") {
		t.Fatalf("a volume was written over: %v", err)
	}
	if got := volumeEntries(t, d, vol); got["volume/pgdata/PG_VERSION"] != before["volume/pgdata/PG_VERSION"] {
		t.Fatal("the volume that was there was touched")
	}
	// No container was left behind by the reads and the writes.
	if out := dockerOut(t, "ps", "-aq", "--filter", "volume="+vol, "--filter", "volume="+origin); out != "" {
		t.Fatalf("containers left mounting the volumes: %s", out)
	}
}
