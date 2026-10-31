package sites

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/access"
	"github.com/dcimorra/the-middleware/internal/engine"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/ports"
	"github.com/dcimorra/the-middleware/internal/runtime"
	"os/exec"
)

// TestMain lets the test binary double as the supervised site, the same
// trick internal/engine uses: with MW_TEST_SERVER set it is a tiny HTTP
// server on PORT, so the tests that stop, archive and restart a running
// site run a real process.
func TestMain(m *testing.M) {
	if os.Getenv("MW_TEST_SERVER") != "" {
		l, err := net.Listen("tcp", "127.0.0.1:"+os.Getenv("PORT"))
		if err != nil {
			os.Exit(1)
		}
		fmt.Println("site up")
		_ = http.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") }))
		return
	}
	os.Exit(m.Run())
}

type fixture struct {
	m      *Manager
	root   string
	bus    *events.Bus
	events []events.Event
	mu     sync.Mutex
	cancel context.CancelFunc
}

// newFixture builds a manager over a temp root. With engine=true a real
// engine ticks every 200ms.
func newFixture(t *testing.T, withEngine bool) *fixture {
	t.Helper()
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "sites"), 0o755)
	bus := events.New("")
	f := &fixture{root: root, bus: bus}
	ch, unsub := bus.Subscribe()
	go func() {
		for e := range ch {
			f.mu.Lock()
			f.events = append(f.events, e)
			f.mu.Unlock()
		}
	}()
	t.Cleanup(unsub)
	var eng *engine.Engine
	if withEngine {
		eng = engine.New(engine.Options{SitesDir: filepath.Join(root, "sites"), DataDir: filepath.Join(root, "data"),
			Ports: ports.New(42000, 42100), Bus: bus, Tick: 200 * time.Millisecond,
			Runtimes: map[string]runtime.Runtime{"process": runtime.Process{}}})
		ctx, cancel := context.WithCancel(context.Background())
		f.cancel = cancel
		go eng.Run(ctx)
		t.Cleanup(func() {
			cancel()
			time.Sleep(300 * time.Millisecond)
		})
	}
	f.m = New(root, eng, bus)
	return f
}

func (f *fixture) sawEvent(typ string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.events {
		if e.Type == typ {
			return true
		}
	}
	return false
}

func (f *fixture) waitEvent(t *testing.T, typ string) events.Event {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		for _, e := range f.events {
			if e.Type == typ {
				f.mu.Unlock()
				return e
			}
		}
		f.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no %s event", typ)
	return events.Event{}
}

func runManifest() *manifest.Manifest {
	return &manifest.Manifest{Version: 2, Run: &manifest.Run{Command: []string{os.Args[0]}, Env: map[string]string{"MW_TEST_SERVER": "1", "API_KEY": "hunter2"}, Drain: manifest.Duration(100 * time.Millisecond), Health: &manifest.Health{Timeout: manifest.Duration(10 * time.Second)}}}
}

func waitSite(t *testing.T, m *Manager, domain string, cond func(Site) bool) Site {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if s, err := m.Get(domain); err == nil && cond(s) {
			return s
		}
		time.Sleep(50 * time.Millisecond)
	}
	s, _ := m.Get(domain)
	t.Fatalf("condition not met for %s: %+v", domain, s)
	return s
}

func TestListShowsManagedAndUnmanagedFoldersAndSkipsTheRest(t *testing.T) {
	f := newFixture(t, false)
	os.MkdirAll(filepath.Join(f.root, "sites", "bare.test"), 0o755)
	os.MkdirAll(filepath.Join(f.root, "sites", "Not A Domain"), 0o755)
	os.WriteFile(filepath.Join(f.root, "sites", "stray-file"), nil, 0o644)
	if _, err := f.m.Create("hello.test", nil, "static"); err != nil {
		t.Fatal(err)
	}
	list, err := f.m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("got %d sites: %+v", len(list), list)
	}
	if list[0].Domain != "bare.test" || list[0].Managed || list[0].Mode != "" {
		t.Fatalf("unmanaged folder: %+v", list[0])
	}
	if list[1].Domain != "hello.test" || !list[1].Managed || list[1].Mode != manifest.ModeStatic || list[1].Manifest == nil {
		t.Fatalf("managed folder: %+v", list[1])
	}
}

func TestEveryTemplateCreatesAValidSiteWithItsFiles(t *testing.T) {
	f := newFixture(t, false)
	want := map[string][]string{
		"static":  {"public/index.html"},
		"node":    {"package.json", "server.js", "public/index.html", "README.md"},
		"go":      {"go.mod", "main.go", "public/index.html"},
		"docker":  {"Dockerfile", "server.js", "public/index.html"},
		"project": {"README.md"},
	}
	for i, name := range Templates() {
		domain := fmt.Sprintf("t%d.test", i)
		if name == "project" {
			domain = fmt.Sprintf("t%d", i)
		}
		s, err := f.m.Create(domain, nil, name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, file := range want[name] {
			if st, err := os.Stat(filepath.Join(f.m.Dir(domain), file)); err != nil || st.Size() == 0 {
				t.Fatalf("%s: %s missing or empty", name, file)
			}
		}
		if _, err := manifest.Load(f.m.Dir(domain)); err != nil {
			t.Fatalf("%s: manifest does not load back: %v", name, err)
		}
		want := map[string]manifest.Mode{"static": manifest.ModeStatic, "project": manifest.ModeProject}[name]
		if want == "" {
			want = manifest.ModeRun
		}
		if s.Mode != want {
			t.Fatalf("%s: mode %s", name, s.Mode)
		}
	}
	if !f.sawEvent("site.created") {
		t.Fatal("no site.created event")
	}
}

func TestCreateRefusesBadDomainsDuplicatesAndUnknownTemplates(t *testing.T) {
	f := newFixture(t, false)
	if _, err := f.m.Create("Not.Valid", nil, "static"); err != ErrBadDomain {
		t.Fatalf("bad domain: %v", err)
	}
	if _, err := f.m.Create("a.test", nil, "rails"); err != ErrBadTemplate {
		t.Fatalf("bad template: %v", err)
	}
	if _, err := f.m.Create("a.test", nil, "static"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Create("a.test", nil, "static"); err != ErrExists {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := f.m.Create("b.test", &manifest.Manifest{Run: &manifest.Run{}}, ""); err == nil || !strings.Contains(err.Error(), "run.command") {
		t.Fatalf("invalid manifest accepted: %v", err)
	}
	if _, err := os.Stat(f.m.Dir("b.test")); !os.IsNotExist(err) {
		t.Fatal("a folder was left behind by a refused create")
	}
}

func TestPatchMergesTopLevelKeysAndRejectsUnknownOnes(t *testing.T) {
	f := newFixture(t, false)
	f.m.Create("a.test", nil, "static")
	s, err := f.m.Patch("a.test", map[string]json.RawMessage{
		"headers": json.RawMessage(`{"X-Frame-Options":"DENY"}`),
		"hold":    json.RawMessage(`{"enabled":true,"title":"Soon"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.Manifest.Static == nil || s.Manifest.Headers["X-Frame-Options"] != "DENY" || s.Mode != manifest.ModeHold {
		t.Fatalf("after patch: %+v", s.Manifest)
	}
	s, err = f.m.Patch("a.test", map[string]json.RawMessage{"hold": json.RawMessage(`null`)})
	if err != nil || s.Manifest.Hold != nil || s.Manifest.Headers == nil {
		t.Fatalf("null did not remove the key: %+v %v", s.Manifest, err)
	}
	if _, err := f.m.Patch("a.test", map[string]json.RawMessage{"comand": json.RawMessage(`[]`)}); err == nil {
		t.Fatal("an unknown key was merged")
	}
	if _, err := f.m.Patch("nope.test", nil); err != ErrNotFound {
		t.Fatalf("missing site: %v", err)
	}
}

func TestStopHoldAndStartAreWrittenToTheManifestAndObeyedByTheEngine(t *testing.T) {
	f := newFixture(t, true)
	if _, err := f.m.Create("a.test", runManifest(), ""); err != nil {
		t.Fatal(err)
	}
	waitSite(t, f.m, "a.test", func(s Site) bool { return s.Alive })
	if _, err := f.m.SetDisabled("a.test", true); err != nil {
		t.Fatal(err)
	}
	waitSite(t, f.m, "a.test", func(s Site) bool { return !s.Alive && s.Mode == manifest.ModeDisabled })
	mf, _ := manifest.Load(f.m.Dir("a.test"))
	if !mf.Disabled {
		t.Fatal("disabled not written")
	}
	if _, err := f.m.SetDisabled("a.test", false); err != nil {
		t.Fatal(err)
	}
	waitSite(t, f.m, "a.test", func(s Site) bool { return s.Alive })
	if _, err := f.m.SetHold("a.test", manifest.Hold{Enabled: true, Title: "Back soon"}); err != nil {
		t.Fatal(err)
	}
	s := waitSite(t, f.m, "a.test", func(s Site) bool { return !s.Alive && s.Mode == manifest.ModeHold })
	if s.Manifest.Hold.Title != "Back soon" {
		t.Fatalf("hold: %+v", s.Manifest.Hold)
	}
	f.m.SetHold("a.test", manifest.Hold{Enabled: false})
	waitSite(t, f.m, "a.test", func(s Site) bool { return s.Alive })
	if err := f.m.Restart(context.Background(), "a.test"); err != nil {
		t.Fatal(err)
	}
	f.waitEvent(t, "handover.done")
}

func TestArchivingStopsTheInstanceFirstAndKeepsTheManifestAsItWas(t *testing.T) {
	f := newFixture(t, true)
	f.m.Create("a.test", runManifest(), "")
	first := waitSite(t, f.m, "a.test", func(s Site) bool { return s.Alive })
	dest, err := f.m.Delete(context.Background(), "a.test", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.m.Dir("a.test")); !os.IsNotExist(err) {
		t.Fatal("folder still in sites/")
	}
	if !strings.HasPrefix(dest, f.m.ArchiveDir()) {
		t.Fatalf("archived to %s", dest)
	}
	// One file; restoring it gives the site back as it was, not stopped.
	archives := f.m.Archives()
	if len(archives) != 1 || archives[0].Domain != "a.test" || filepath.Join(f.m.ArchiveDir(), archives[0].Name) != dest {
		t.Fatalf("archives: %+v", archives)
	}
	if _, err := f.m.Restore(context.Background(), archives[0].Name); err != nil {
		t.Fatal(err)
	}
	mf, err := manifest.Load(f.m.Dir("a.test"))
	if err != nil || mf.Disabled {
		t.Fatalf("restored manifest: %+v %v", mf, err)
	}
	if _, err := f.m.Restore(context.Background(), archives[0].Name); err != ErrExists {
		t.Fatalf("restored over a live site: %v", err)
	}
	waitSite(t, f.m, "a.test", func(s Site) bool { return s.Alive })
	f.m.Delete(context.Background(), "a.test", false)
	if _, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", first.Port), time.Second); err == nil {
		t.Fatal("the instance is still listening after the archive")
	}

	if !f.sawEvent("site.archived") {
		t.Fatal("no site.archived event")
	}
	if _, err := f.m.Delete(context.Background(), "a.test", false); err != ErrNotFound {
		t.Fatalf("deleting twice: %v", err)
	}
}

func TestDeleteRemovesTheFolderAndItsLogs(t *testing.T) {
	f := newFixture(t, false)
	f.m.Create("a.test", nil, "static")
	os.MkdirAll(filepath.Join(f.root, "data", "logs", "a.test"), 0o755)
	os.WriteFile(filepath.Join(f.root, "data", "logs", "a.test", "site.log"), []byte("x\n"), 0o644)
	if _, err := f.m.Delete(context.Background(), "a.test", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.m.Dir("a.test")); !os.IsNotExist(err) {
		t.Fatal("folder still there")
	}
	if _, err := os.Stat(filepath.Join(f.root, "data", "logs", "a.test")); !os.IsNotExist(err) {
		t.Fatal("logs still there")
	}
}

func TestLogsReturnTheLastLinesAndFollowStreamsNewOnes(t *testing.T) {
	f := newFixture(t, false)
	f.m.Create("a.test", nil, "static")
	if out, err := f.m.Logs("a.test", "", 10); err != nil || out != "" {
		t.Fatalf("missing log: %q %v", out, err)
	}
	p, _ := f.m.LogPath("a.test", "")
	os.MkdirAll(filepath.Dir(p), 0o755)
	var big bytes.Buffer
	for i := 1; i <= 5000; i++ {
		fmt.Fprintf(&big, "line %d with some padding to cross the block size\n", i)
	}
	os.WriteFile(p, big.Bytes(), 0o644)
	out, err := f.m.Logs("a.test", "", 2)
	if err != nil || out != "line 4999 with some padding to cross the block size\nline 5000 with some padding to cross the block size\n" {
		t.Fatalf("tail: %q %v", out, err)
	}
	if _, err := f.m.Logs("a.test", "../x", 2); err == nil {
		t.Fatal("a worker name with a slash was accepted")
	}
	wp, _ := f.m.LogPath("a.test", "api")
	if filepath.Base(wp) != "worker-api.log" {
		t.Fatal(wp)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pr, pw := net.Pipe()
	go f.m.Follow(ctx, "a.test", "", 1, pw)
	buf := make([]byte, 4096)
	n, _ := pr.Read(buf)
	if !strings.HasPrefix(string(buf[:n]), "line 5000") {
		t.Fatalf("initial tail: %q", buf[:n])
	}
	af, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	af.WriteString("appended\n")
	af.Close()
	pr.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err = pr.Read(buf)
	if err != nil || string(buf[:n]) != "appended\n" {
		t.Fatalf("follow: %q %v", buf[:n], err)
	}
	cancel()
}

func TestFilesCannotLeaveTheSiteFolderEvenThroughASymlink(t *testing.T) {
	f := newFixture(t, false)
	f.m.Create("a.test", nil, "static")
	outside := filepath.Join(f.root, "secret.txt")
	os.WriteFile(outside, []byte("secret"), 0o644)
	dir := f.m.Dir("a.test")
	os.Symlink(outside, filepath.Join(dir, "link.txt"))
	os.Symlink(f.root, filepath.Join(dir, "updir"))

	for _, p := range []string{"../secret.txt", "/../secret.txt", "link.txt", "updir/secret.txt", "public/../../secret.txt", "updir/newfile"} {
		if _, err := f.m.ReadFile("a.test", p); err != ErrOutside {
			t.Fatalf("%s: read gave %v", p, err)
		}
		if err := f.m.WriteFile("a.test", p, []byte("x")); err != ErrOutside {
			t.Fatalf("%s: write gave %v", p, err)
		}
	}
	if string(mustRead(t, outside)) != "secret" {
		t.Fatal("the outside file was written")
	}
	entries, err := f.m.Files("a.test", "/")
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name)
	}
	if strings.Join(names, ",") != "public,updir,link.txt,site.json" {
		t.Fatalf("listing: %v", names)
	}
	if err := f.m.WriteFile("a.test", "deep/er/file.txt", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	data, err := f.m.ReadFile("a.test", "deep/er/file.txt")
	if err != nil || string(data) != "hello" {
		t.Fatalf("round trip: %q %v", data, err)
	}
	if _, err := f.m.ReadFile("a.test", "public"); err == nil {
		t.Fatal("reading a folder succeeded")
	}
	if err := f.m.WriteFile("a.test", "site.json", []byte(`{"version":2,"comand":[]}`)); err == nil {
		t.Fatal("an invalid manifest was written through the editor")
	}
	if _, err := f.m.Files("nope.test", "/"); err != ErrNotFound {
		t.Fatalf("missing site: %v", err)
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestResolvedEnvMasksSecretsAndShowsTheRest(t *testing.T) {
	f := newFixture(t, false)
	f.m.Create("a.test", runManifest(), "")
	os.WriteFile(filepath.Join(f.m.Dir("a.test"), ".env"), []byte("DB_PASSWORD=pw\nPLAIN=yes\n"), 0o644)
	f.m.Patch("a.test", map[string]json.RawMessage{"run": json.RawMessage(fmt.Sprintf(`{"command":[%q],"env":{"API_KEY":"hunter2","MW_TEST_SERVER":"1"},"env_files":[".env"]}`, os.Args[0]))})
	env, err := f.m.ResolvedEnv("a.test")
	if err != nil {
		t.Fatal(err)
	}
	if env["API_KEY"] != Masked || env["DB_PASSWORD"] != Masked {
		t.Fatalf("secrets not masked: %v", env)
	}
	if env["PLAIN"] != "yes" || env["MIDDLEWARE_DOMAIN"] != "a.test" || env["PORT"] != "$PORT" {
		t.Fatalf("env: %v", env)
	}
	f.m.Create("s.test", nil, "static")
	if env, err := f.m.ResolvedEnv("s.test"); err != nil || len(env) != 0 {
		t.Fatalf("static site env: %v %v", env, err)
	}
}

func TestDeployRunsTheBuildStepsAndWritesThePointer(t *testing.T) {
	f := newFixture(t, false)
	mf := runManifest()
	mf.Deploy = &manifest.Deploy{GitHub: &manifest.GitHubDeploy{Repository: "acme/app", Pointer: ".current",
		Build: [][]string{{"sh", "-c", "echo building $MIDDLEWARE_BUILD > built.txt"}, {"sh", "-c", "test -n \"$HOME\""}}}}
	f.m.Create("a.test", mf, "")
	id, err := f.m.Deploy("a.test")
	if err != nil {
		t.Fatal(err)
	}
	f.waitEvent(t, "deploy.done")
	dir := f.m.Dir("a.test")
	if got := strings.TrimSpace(string(mustRead(t, filepath.Join(dir, ".current")))); got != id {
		t.Fatalf("pointer %q, build %q", got, id)
	}
	if got := strings.TrimSpace(string(mustRead(t, filepath.Join(dir, "built.txt")))); got != "building "+id {
		t.Fatalf("build output %q", got)
	}
	log := string(mustRead(t, filepath.Join(f.root, "data", "logs", "a.test", "deploy.log")))
	if !strings.Contains(log, "step 1/2") || !strings.Contains(log, "pointer .current") {
		t.Fatalf("deploy log: %s", log)
	}

	mf.Deploy.GitHub.Build = [][]string{{"sh", "-c", "exit 3"}}
	f.m.Update("a.test", mf)
	if _, err := f.m.Deploy("a.test"); err != nil {
		t.Fatal(err)
	}
	e := f.waitEvent(t, "deploy.failed")
	if !strings.Contains(e.Message, "step 1") {
		t.Fatalf("failure event: %+v", e)
	}
	if got := strings.TrimSpace(string(mustRead(t, filepath.Join(dir, ".current")))); got != id {
		t.Fatal("a failed build moved the pointer")
	}
}

func TestASitePasswordIsHashedBeforeItReachesDisk(t *testing.T) {
	f := newFixture(t, false)
	m := f.m
	if _, err := m.Create("p.test", nil, "static"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Patch("p.test", map[string]json.RawMessage{"access": json.RawMessage(`{"password":"hunter2pass"}`)}); err != nil {
		t.Fatal(err)
	}
	mf, err := manifest.Load(filepath.Join(m.Root, "sites", "p.test"))
	if err != nil {
		t.Fatal(err)
	}
	if !access.IsHash(mf.Access.Password) || !access.CheckPassword(mf.Access.Password, "hunter2pass") {
		t.Fatalf("stored password = %q", mf.Access.Password)
	}
	// Saving the manifest again must not hash the hash.
	before := mf.Access.Password
	if _, err := m.Update("p.test", mf); err != nil {
		t.Fatal(err)
	}
	mf, _ = manifest.Load(filepath.Join(m.Root, "sites", "p.test"))
	if mf.Access.Password != before {
		t.Fatal("a stored hash was re-hashed")
	}
}

// A renamed site is the same site: its conversation and HOME come along,
// and it comes back up under the new name.
func TestARenamedSiteKeepsItsThingsAndRunsAgain(t *testing.T) {
	f := newFixture(t, true)
	f.m.Create("old.test", runManifest(), "")
	waitSite(t, f.m, "old.test", func(s Site) bool { return s.Alive })
	os.MkdirAll(filepath.Join(f.m.Root, "data", "homes", "old.test"), 0o700)
	os.WriteFile(filepath.Join(f.m.Root, "data", "homes", "old.test", "mark"), []byte("1"), 0o600)
	os.MkdirAll(filepath.Join(f.m.Root, "data", "chat"), 0o700)
	os.WriteFile(filepath.Join(f.m.Root, "data", "chat", "old.test.json"), []byte(`{"turns":[]}`), 0o600)
	if _, err := f.m.Rename(context.Background(), "old.test", "new.test"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"data/homes/new.test/mark", "data/chat/new.test.json", "sites/new.test/site.json"} {
		if _, err := os.Stat(filepath.Join(f.m.Root, p)); err != nil {
			t.Errorf("%s did not move: %v", p, err)
		}
	}
	waitSite(t, f.m, "new.test", func(s Site) bool { return s.Alive })
	// It runs a process for a hostname: it cannot become a project, and
	// says why.
	if _, err := f.m.Rename(context.Background(), "new.test", "tools"); err == nil || !strings.Contains(err.Error(), "project") {
		t.Fatalf("a site that serves a hostname renamed to a project: %v", err)
	}
	f.m.Delete(context.Background(), "new.test", false)
}

// A project is given a domain by renaming it: its things come along.
func TestAProjectIsGivenADomain(t *testing.T) {
	f := newFixture(t, false)
	if _, err := f.m.Create("tools", &manifest.Manifest{Version: manifest.Version}, ""); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(f.m.Root, "data", "homes", "tools"), 0o700)
	os.WriteFile(filepath.Join(f.m.Root, "data", "homes", "tools", "mark"), []byte("1"), 0o600)
	if _, err := f.m.Rename(context.Background(), "tools", "tools.example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.m.Root, "data", "homes", "tools.example.com", "mark")); err != nil {
		t.Fatal("its HOME did not come along")
	}
	// And back: with nothing that answers a hostname, it may be a project
	// again.
	if _, err := f.m.Rename(context.Background(), "tools.example.com", "tools"); err != nil {
		t.Fatalf("back to a project: %v", err)
	}
}

// Stopped for a move whose data is exported with the site stopped, a site
// is held, not disabled: its instance and its other workers stop, and the
// worker that holds the data stays up for the export. Disabled, the engine
// stopped the database too, and there was nothing to export from.
func TestASiteStoppedForAMoveKeepsTheWorkerItsDataIsIn(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 stands in for the processes")
	}
	f := newFixture(t, true)
	dir := filepath.Join(f.root, "sites", "h.test")
	os.MkdirAll(dir, 0o755)
	serve := `["python3","-m","http.server","$PORT","--bind","127.0.0.1"]`
	original := []byte(`{"version":2,"run":{"command":` + serve + `},"workers":{
 "db":{"command":` + serve + `,"move":{"export":["true"],"import":["true"]}},
 "queue":{"command":` + serve + `}}}`)
	os.WriteFile(filepath.Join(dir, "site.json"), original, 0o644)
	alive := func(cond func(sn engine.Snapshot) bool) bool {
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if sn, ok := f.m.lookup("h.test"); ok && cond(sn) {
				return true
			}
			time.Sleep(100 * time.Millisecond)
		}
		return false
	}
	if !alive(func(sn engine.Snapshot) bool { return sn.Alive && sn.Workers["db"].Alive && sn.Workers["queue"].Alive }) {
		t.Fatal("the site never came up")
	}
	t.Cleanup(func() {
		os.WriteFile(filepath.Join(dir, "site.json"), []byte(`{"version":2,"disabled":true,"run":{"command":`+serve+`},"workers":{"db":{"command":`+serve+`},"queue":{"command":`+serve+`}}}`), 0o644)
		alive(func(sn engine.Snapshot) bool {
			return !sn.Alive && !sn.Workers["db"].Alive && !sn.Workers["queue"].Alive
		})
	})
	got, err := f.m.StopForMove(context.Background(), "h.test")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("the manifest it had: %s", got)
	}
	sn, _ := f.m.lookup("h.test")
	if sn.Alive || !sn.Workers["db"].Alive || sn.Workers["queue"].Alive {
		t.Fatalf("instance %v, db %v, queue %v: want only db up", sn.Alive, sn.Workers["db"].Alive, sn.Workers["queue"].Alive)
	}
	held, _ := manifest.Load(dir)
	if held.Hold == nil || !held.Hold.Enabled || held.Disabled {
		t.Fatalf("not held: %+v", held)
	}
	if err := f.m.Resume("h.test", got); err != nil {
		t.Fatal(err)
	}
	if !alive(func(sn engine.Snapshot) bool { return sn.Alive && sn.Workers["queue"].Alive }) {
		t.Fatal("it did not come back as it was")
	}
}

// The panel's "what the repository says" reached a site made from a
// template and was refused as an unknown template; it means the default.
func TestCreateTakesAutoWithoutARepositoryAsTheDefault(t *testing.T) {
	f := newFixture(t, false)
	s, err := f.m.Create("auto.test", nil, "auto")
	if err != nil {
		t.Fatal(err)
	}
	if s.Mode != manifest.ModeStatic {
		t.Fatalf("mode %s, want static", s.Mode)
	}
	p, err := f.m.Create("autoproject", nil, "auto")
	if err != nil {
		t.Fatal(err)
	}
	if p.Mode != manifest.ModeProject {
		t.Fatalf("mode %s, want project", p.Mode)
	}
}
