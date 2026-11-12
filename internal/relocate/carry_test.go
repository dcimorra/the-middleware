package relocate

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/manifest"
)

// entry is one name of a volume's tar in the tests.
type entry struct {
	name, body, link string
	kind             byte
	mode             int64
	uid, gid         int
}

// volumeTar is a volume as runtime.Docker.ReadVolume writes one: the
// volume's own folder first, every name under it.
func volumeTar(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	all := append([]entry{{name: "volume/", kind: tar.TypeDir, mode: 0o1777, uid: 999, gid: 999}}, entries...)
	for _, e := range all {
		h := &tar.Header{Name: e.name, Typeflag: e.kind, Mode: e.mode, Uid: e.uid, Gid: e.gid, Linkname: e.link, Size: int64(len(e.body)), Format: tar.FormatPAX}
		if h.Typeflag == 0 {
			h.Typeflag = tar.TypeReg
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		io.WriteString(tw, e.body)
	}
	tw.Close()
	return buf.Bytes()
}

// aDatabase is a volume shaped like the one that was left behind: a
// Postgres data folder owned by a user the machine does not know.
func aDatabase(t *testing.T, orders string) []byte {
	return volumeTar(t,
		entry{name: "volume/pgdata/", kind: tar.TypeDir, mode: 0o700, uid: 1000, gid: 989},
		entry{name: "volume/pgdata/PG_VERSION", body: "17\n", mode: 0o600, uid: 1000, gid: 989},
		entry{name: "volume/pgdata/base/16384/orders", body: orders, mode: 0o600, uid: 1000, gid: 989},
		entry{name: "volume/pgdata/base/16384/orders.link", kind: tar.TypeLink, link: "volume/pgdata/base/16384/orders", mode: 0o600, uid: 1000, gid: 989},
		entry{name: "volume/pgdata/current", kind: tar.TypeSymlink, link: "base/16384", mode: 0o777, uid: 1000, gid: 989},
	)
}

// memDocker is a node's docker volumes, each the tar it holds.
type memDocker struct {
	mu    sync.Mutex
	vols  map[string][]byte
	users map[string][]string
	reads []string
}

func (d *memDocker) names() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for v := range d.vols {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// carrySource is a source with docker volumes.
type carrySource struct {
	*localSource
	docker   *memDocker
	noAnswer bool
}

func (s *carrySource) Volumes(context.Context, string) ([]string, error) {
	if s.noAnswer {
		return nil, errors.New("the daemon does not answer")
	}
	return s.docker.names(), nil
}

func (s *carrySource) VolumeUsers(_ context.Context, v string) ([]string, error) {
	return s.docker.users[v], nil
}

func (s *carrySource) ReadVolume(_ context.Context, v string, w io.Writer) error {
	s.docker.mu.Lock()
	raw := s.docker.vols[v]
	s.docker.reads = append(s.docker.reads, v)
	s.docker.mu.Unlock()
	_, err := w.Write(raw)
	return err
}

// carryTarget takes volumes into a docker of its own, or fails to.
type carryTarget struct {
	localTarget
	docker *memDocker
	// fail breaks the connection halfway; lose answers well for a volume
	// that was written without its last file.
	fail      bool
	lose      []byte
	committed bool
}

func (t *carryTarget) TakeVolume(_ context.Context, _, vol string, body io.Reader) (VolumeSum, error) {
	if t.fail {
		io.CopyN(io.Discard, body, 512)
		return VolumeSum{}, errors.New("connection reset by peer")
	}
	raw, err := io.ReadAll(body)
	if err != nil {
		return VolumeSum{}, err
	}
	if t.lose != nil {
		raw = t.lose
	}
	t.docker.mu.Lock()
	t.docker.vols[vol] = raw
	t.docker.mu.Unlock()
	return Summarize(bytes.NewReader(raw), nil)
}

func (t *carryTarget) Commit(ctx context.Context, d string, m []byte, ts map[string][]byte) error {
	t.committed = true
	return t.localTarget.Commit(ctx, d, m, ts)
}

func (t *carryTarget) Discard(ctx context.Context, d string) error {
	t.docker.mu.Lock()
	t.docker.vols = map[string][]byte{}
	t.docker.mu.Unlock()
	return t.localTarget.Discard(ctx, d)
}

const servicesManifest = `{"version":2,"services":{
 "web":{"run":{"command":["node","server.js"],"runtime":"docker","docker":{"image":"node:22"}}},
 "db":{"internal":true,"run":{"command":["postgres"],"runtime":"docker","docker":{"image":"postgres:17","volumes":["db:/var/lib/postgresql/data"]}}}}}`

func carrySeed(t *testing.T, root string) {
	seed(t, root)
	os.WriteFile(filepath.Join(root, "sites/a.test/site.json"), []byte(servicesManifest), 0o644)
	p := filepath.Join(root, "data/backups/a.test/db/20261112T033000Z-scheduled.sql.gz")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte("last night's dump"), 0o600)
	os.MkdirAll(filepath.Join(root, "data/visits"), 0o755)
	os.WriteFile(filepath.Join(root, "data/visits/a.test.json"), []byte("{}"), 0o644)
}

func carryMove(t *testing.T, tg *carryTarget, s *carrySource) ([]string, error) {
	t.Helper()
	m := &Mover{Root: s.root, Source: s, Poll: 10 * time.Millisecond, ReadyWait: 200 * time.Millisecond}
	var steps []string
	_, err := m.Move(context.Background(), "a.test", tg, func(step, _ string) {
		if len(steps) == 0 || steps[len(steps)-1] != step {
			steps = append(steps, step)
		}
	})
	return steps, err
}

// verdelaurel.es kept its shop in a Postgres service whose data was the
// volume mw-verdelaurel-es-db. Its move took the folder: the service
// found an empty volume on the other node, ran initdb, and the web seeded
// a demo catalogue where the orders had been; the copies of the database
// under data/backups stayed behind too.
func TestASitesVolumesAndItsCopiesGoWithIt(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	carrySeed(t, src)
	db := aDatabase(t, "every order since the fourth of November")
	s := &carrySource{localSource: &localSource{root: src}, docker: &memDocker{vols: map[string][]byte{"mw-a-test-db": db}}}
	tg := &carryTarget{localTarget: localTarget{rc: Receiver{Root: dst}, ready: true}, docker: &memDocker{vols: map[string][]byte{}}}
	steps, err := carryMove(t, tg, s)
	if err != nil {
		t.Fatal(err)
	}
	// With the site stopped, and before it is put in place there.
	if got := strings.Join(steps, " "); got != "check copy stop volumes final commit start dns cleanup done" {
		t.Fatalf("steps: %s", got)
	}
	if !bytes.Equal(tg.docker.vols["mw-a-test-db"], db) {
		t.Fatal("the volume is not on the target as it was")
	}
	// Never removed where it was: listed, for the operator.
	if !bytes.Equal(s.docker.vols["mw-a-test-db"], db) {
		t.Fatal("the volume was touched where the site was")
	}
	listed := false
	for _, l := range Leftovers(src, time.Now()) {
		listed = listed || l.Path == "docker-volume:mw-a-test-db" && l.Kind == "volume"
	}
	if !listed {
		t.Fatalf("the volume left here is not among the leftovers: %+v", Leftovers(src, time.Now()))
	}
	if raw, _ := os.ReadFile(filepath.Join(dst, "data/backups/a.test/db/20261112T033000Z-scheduled.sql.gz")); string(raw) != "last night's dump" {
		t.Fatal("the copies of its database did not come along")
	}
	if _, err := os.Stat(filepath.Join(dst, "data/visits/a.test.json")); err != nil {
		t.Fatal("its visits did not come along")
	}
}

// A copy that fails, one the target answers well for without having it
// whole, and a volume something still writes: each ends with the site
// running where it was on the volume nothing touched, and never put in
// place on the other node.
func TestAVolumeThatDoesNotArriveWholeUndoesTheMove(t *testing.T) {
	old := quietWait
	quietWait = 50 * time.Millisecond
	defer func() { quietWait = old }()
	for _, c := range []struct {
		name, want string
		set        func(*carryTarget, *carrySource)
	}{
		{"the connection breaks", "connection reset", func(tg *carryTarget, _ *carrySource) { tg.fail = true }},
		{"a file is missing there", "not to be trusted", func(tg *carryTarget, _ *carrySource) {
			tg.lose = volumeTar(t, entry{name: "volume/pgdata/", kind: tar.TypeDir, mode: 0o700, uid: 1000, gid: 989})
		}},
		{"a container still mounts it", "still mounted by a running container (mw-a-test-db-5432)", func(_ *carryTarget, s *carrySource) {
			s.docker.users = map[string][]string{"mw-a-test-db": {"mw-a-test-db-5432"}}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			src, dst := t.TempDir(), t.TempDir()
			carrySeed(t, src)
			db := aDatabase(t, "orders")
			s := &carrySource{localSource: &localSource{root: src}, docker: &memDocker{vols: map[string][]byte{"mw-a-test-db": db}}}
			tg := &carryTarget{localTarget: localTarget{rc: Receiver{Root: dst}, ready: true}, docker: &memDocker{vols: map[string][]byte{}}}
			c.set(tg, s)
			steps, err := carryMove(t, tg, s)
			if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "runs here again") {
				t.Fatalf("err: %v", err)
			}
			if got := strings.Join(steps, " "); got != "check copy stop volumes restore" {
				t.Fatalf("steps: %s", got)
			}
			if tg.committed {
				t.Fatal("the site was put in place on a node its volume did not reach")
			}
			if _, err := os.Stat(filepath.Join(dst, "sites/a.test")); err == nil {
				t.Fatal("the site is on the target")
			}
			if len(tg.docker.vols) != 0 {
				t.Fatalf("what reached the target of its volume stayed there: %v", tg.docker.names())
			}
			if !bytes.Equal(s.docker.vols["mw-a-test-db"], db) || !strings.Contains(string(s.started), `"services"`) || s.archived {
				t.Fatalf("where it was: started %q archived %v", s.started, s.archived)
			}
		})
	}
}

// A target that cannot take a volume, or a docker here that cannot say
// which volumes the site has, is found before anything is copied or
// stopped: "none" must never be what a failed question reads as.
func TestAMoveThatCouldNotTakeTheVolumesStopsAtTheCheck(t *testing.T) {
	t.Run("the target takes none", func(t *testing.T) {
		src, dst := t.TempDir(), t.TempDir()
		carrySeed(t, src)
		s := &carrySource{localSource: &localSource{root: src}, docker: &memDocker{vols: map[string][]byte{"mw-a-test-db": aDatabase(t, "orders")}}}
		m := &Mover{Root: src, Source: s}
		var steps []string
		_, err := m.Move(context.Background(), "a.test", &localTarget{rc: Receiver{Root: dst}, ready: true}, func(step, _ string) { steps = append(steps, step) })
		if err == nil || !strings.Contains(err.Error(), "it would start there on empty ones") || strings.Join(steps, " ") != "check" {
			t.Fatalf("err %v after %v", err, steps)
		}
	})
	t.Run("docker does not answer here", func(t *testing.T) {
		src, dst := t.TempDir(), t.TempDir()
		carrySeed(t, src)
		s := &carrySource{localSource: &localSource{root: src}, docker: &memDocker{}, noAnswer: true}
		tg := &carryTarget{localTarget: localTarget{rc: Receiver{Root: dst}, ready: true}, docker: &memDocker{vols: map[string][]byte{}}}
		steps, err := carryMove(t, tg, s)
		if err == nil || !strings.Contains(err.Error(), "would leave them behind") || strings.Join(steps, " ") != "check" {
			t.Fatalf("err %v after %v", err, steps)
		}
	})
}

// The target is told which volumes come and which of the newer parts
// under data/ the site has, so one that has such a volume, or would not
// place such a part, refuses before anything moves.
func TestTheCheckNamesTheVolumesAndTheDataThatCome(t *testing.T) {
	src := t.TempDir()
	carrySeed(t, src)
	s := &carrySource{localSource: &localSource{root: src}, docker: &memDocker{vols: map[string][]byte{"mw-a-test-db": nil}}}
	seen := &needsTarget{}
	m := &Mover{Root: src, Source: s}
	if _, err := m.Check(context.Background(), "a.test", seen); err != nil {
		t.Fatal(err)
	}
	if strings.Join(seen.need.Carried, " ") != "mw-a-test-db" {
		t.Fatalf("carried: %v", seen.need.Carried)
	}
	if got := strings.Join(seen.need.Data, " "); got != "data/backups/a.test data/visits/a.test.json" {
		t.Fatalf("data: %s", got)
	}
	if name, ok := KnownData("a.test", seen.need.Data); !ok {
		t.Fatalf("this house would not place %s", name)
	}
	if name, ok := KnownData("a.test", []string{"data/tomorrow/a.test"}); ok || name != "data/tomorrow/a.test" {
		t.Fatal("a part this house does not know was taken as known")
	}
	d, _ := fakeDocker(t, "mw-a-test-db")
	err := Receiver{Root: t.TempDir(), Docker: &d}.CheckNeeds(context.Background(), seen.need)
	if err == nil || !strings.Contains(err.Error(), "mw-a-test-db is already on this node") {
		t.Fatalf("a node that has the volume: %v", err)
	}
}

type needsTarget struct {
	carryTarget
	need Needs
}

func (t *needsTarget) Check(_ context.Context, _ string, need Needs) error {
	t.need = need
	return nil
}

func TestWhatAVolumeHoldsIsCountedByNameOwnerModeAndContents(t *testing.T) {
	base, err := Summarize(bytes.NewReader(aDatabase(t, "orders")), nil)
	if err != nil || base.Files != 2 || base.Bytes != int64(len("17\n")+len("orders")) {
		t.Fatalf("%+v %v", base, err)
	}
	// The same volume read in another order is the same volume.
	shuffled := volumeTar(t,
		entry{name: "volume/pgdata/", kind: tar.TypeDir, mode: 0o700, uid: 1000, gid: 989},
		entry{name: "volume/pgdata/current", kind: tar.TypeSymlink, link: "base/16384", mode: 0o777, uid: 1000, gid: 989},
		entry{name: "volume/pgdata/base/16384/orders", body: "orders", mode: 0o600, uid: 1000, gid: 989},
		entry{name: "volume/pgdata/base/16384/orders.link", kind: tar.TypeLink, link: "volume/pgdata/base/16384/orders", mode: 0o600, uid: 1000, gid: 989},
		entry{name: "volume/pgdata/PG_VERSION", body: "17\n", mode: 0o600, uid: 1000, gid: 989},
	)
	if again, _ := Summarize(bytes.NewReader(shuffled), nil); again != base {
		t.Fatal("the order of the names changed what the volume is")
	}
	for name, other := range map[string][]byte{
		"contents": aDatabase(t, "0rders"),
		"owner": volumeTar(t,
			entry{name: "volume/pgdata/", kind: tar.TypeDir, mode: 0o700, uid: 0, gid: 0},
			entry{name: "volume/pgdata/PG_VERSION", body: "17\n", mode: 0o600, uid: 1000, gid: 989}),
		"mode": volumeTar(t,
			entry{name: "volume/pgdata/", kind: tar.TypeDir, mode: 0o755, uid: 1000, gid: 989},
			entry{name: "volume/pgdata/PG_VERSION", body: "17\n", mode: 0o600, uid: 1000, gid: 989}),
	} {
		a, _ := Summarize(bytes.NewReader(other), nil)
		b, _ := Summarize(bytes.NewReader(volumeTar(t,
			entry{name: "volume/pgdata/", kind: tar.TypeDir, mode: 0o700, uid: 1000, gid: 989},
			entry{name: "volume/pgdata/PG_VERSION", body: "17\n", mode: 0o600, uid: 1000, gid: 989})), nil)
		if a.Digest == b.Digest || a.Digest == base.Digest {
			t.Fatalf("a different %s is the same volume", name)
		}
	}
	// Every byte goes on to whoever is behind, the end blocks included.
	var out bytes.Buffer
	raw := aDatabase(t, "orders")
	if _, err := Summarize(bytes.NewReader(raw), &out); err != nil || !bytes.Equal(out.Bytes(), raw) {
		t.Fatalf("passed on %d of %d bytes: %v", out.Len(), len(raw), err)
	}
	// A cut tar is an error, not a smaller volume.
	if _, err := Summarize(bytes.NewReader(raw[:len(raw)/2]), nil); err == nil {
		t.Fatal("half a tar was counted as a volume")
	}
	for _, bad := range []entry{
		{name: "etc/passwd", body: "x"},
		{name: "volume/../etc/passwd", body: "x"},
		{name: "volume/l", kind: tar.TypeLink, link: "etc/passwd"},
	} {
		if _, err := Summarize(bytes.NewReader(volumeTar(t, bad)), nil); err == nil || !strings.Contains(err.Error(), "outside the volume") {
			t.Fatalf("%s: %v", bad.name, err)
		}
	}
}

// What a node that sends no volumes would have this one make empty: the
// named volumes no move command fills.
func TestTheVolumesAnOlderNodeWouldLeaveBehind(t *testing.T) {
	m, err := manifest.Parse([]byte(servicesManifest))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(Uncarried("a.test", m), " "); got != "mw-a-test-db" {
		t.Fatalf("a service's volume: %q", got)
	}
	m, err = manifest.Parse([]byte(strings.ReplaceAll(volumeManifest, "%PORT%", "5432")))
	if err != nil {
		t.Fatal(err)
	}
	if got := Uncarried("a.test", m); len(got) != 0 {
		t.Fatalf("volumes its workers' commands fill: %v", got)
	}
}
