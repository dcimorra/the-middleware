package deps

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dcimorra/the-middleware/internal/events"
)

func TestNewerComparesSemverNotStrings(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"2.1.310", "2.1.304", true},
		{"2.1.304", "2.1.310", false},
		{"10.0.0", "9.9.9", true},
		{"0.64.0", "0.64.0", false},
		{"v1.2.3", "1.2.2", true},
		{"1.2.3-beta.1", "1.2.3", false},
	}
	for _, c := range cases {
		if got := newer(c.a, c.b); got != c.want {
			t.Errorf("newer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestAnArchiveEntryCannotEscapeTheTarget(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "evil.tar.gz")
	f, _ := os.Create(archive)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	body := []byte("x")
	tw.WriteHeader(&tar.Header{Name: "node/../../escape", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
	tw.Write(body)
	tw.Close()
	gz.Close()
	f.Close()
	if err := untarGz(archive, filepath.Join(dir, "out"), "node/"); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("want an escape error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "escape")); err == nil {
		t.Fatal("the file escaped")
	}
}

// TestACheckFindsTheNewerVersionAndUpdatesTheManagedCopy fakes the
// registry and npm: the manager must ask the dist-tags, compare with the
// package.json of the copy it installed, and run the install line only
// for a managed copy that is behind.
func TestACheckFindsTheNewerVersionAndUpdatesTheManagedCopy(t *testing.T) {
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"latest":"9.9.9"}`))
	}))
	defer reg.Close()
	old := Download
	Download = &http.Client{Transport: rewriteTo(reg.URL)}
	defer func() { Download = old }()

	root := t.TempDir()
	m := New(root, events.New(""))
	// A managed claude at 1.0.0, a fake node that is new enough.
	pkgDir := filepath.Join(m.Prefix(), "lib", "node_modules", "@anthropic-ai", "claude-code")
	os.MkdirAll(pkgDir, 0o755)
	os.WriteFile(filepath.Join(pkgDir, "package.json"), []byte(`{"version":"1.0.0"}`), 0o644)
	os.MkdirAll(m.BinDir(), 0o755)
	os.WriteFile(filepath.Join(m.BinDir(), "claude"), []byte("#!/bin/sh\n"), 0o755)
	rt := filepath.Join(m.RuntimeDir(), "bin")
	os.MkdirAll(rt, 0o755)
	os.WriteFile(filepath.Join(rt, "node"), []byte("#!/bin/sh\n"), 0o755)
	os.WriteFile(filepath.Join(rt, "npm"), []byte(""), 0o644)

	var ran []string
	m.Exec = func(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
		ran = append(ran, name+" "+strings.Join(args, " "))
		if len(args) > 0 && args[0] == "--version" {
			return []byte("v22.0.0\n"), nil
		}
		// The install "succeeds" by bumping package.json.
		os.WriteFile(filepath.Join(pkgDir, "package.json"), []byte(`{"version":"9.9.9"}`), 0o644)
		return []byte("added 1 package"), nil
	}
	sts := m.Check(context.Background(), nil)
	var claude Status
	for _, s := range sts {
		if s.Name == "claude" {
			claude = s
		}
	}
	if claude.Latest != "9.9.9" || claude.Version != "9.9.9" || claude.Outdated {
		t.Fatalf("after the check: %+v", claude)
	}
	found := false
	for _, r := range ran {
		if strings.Contains(r, "install -g --prefix "+m.Prefix()+" @anthropic-ai/claude-code@9.9.9") {
			found = true
		}
	}
	if !found {
		t.Fatalf("npm install was not run: %v", ran)
	}
	// qwen is not managed here: reported, never installed on its own.
	for _, r := range ran {
		if strings.Contains(r, "qwen-code") {
			t.Fatalf("an unmanaged package must not be installed by a check: %v", ran)
		}
	}
	// With auto-update off, the next check reports and leaves it.
	m.AutoUpdate = func() bool { return false }
	os.WriteFile(filepath.Join(pkgDir, "package.json"), []byte(`{"version":"1.0.0"}`), 0o644)
	ran = nil
	sts = m.Check(context.Background(), nil)
	for _, s := range sts {
		if s.Name == "claude" && !s.Outdated {
			t.Fatalf("want outdated reported, got %+v", s)
		}
	}
	for _, r := range ran {
		if strings.Contains(r, "install") {
			t.Fatalf("auto-update off must not install: %v", ran)
		}
	}
}

// TestInstallingForRealFetchesTheCLI runs the real npm against the real
// registry; DEPS=1 because it takes a minute and a network.
func TestInstallingForRealFetchesTheCLI(t *testing.T) {
	if os.Getenv("DEPS") != "1" {
		t.Skip("set DEPS=1 to install claude for real")
	}
	root := t.TempDir()
	m := New(root, events.New(""))
	var lines []string
	if err := m.Install(context.Background(), "claude", func(s string) { lines = append(lines, s) }); err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(lines, "\n"))
	}
	path, managed, err := m.Find("claude")
	if err != nil || !managed {
		t.Fatalf("claude not managed after install: %s %v %v", path, managed, err)
	}
	if v := m.installedVersion(Packages[0]); v == "" {
		t.Fatal("no version in package.json")
	}
}

type rewrite struct{ base string }

func rewriteTo(base string) http.RoundTripper { return rewrite{base} }

func (r rewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	u := strings.Replace(req.URL.String(), "https://registry.npmjs.org", r.base, 1)
	nr, _ := http.NewRequestWithContext(req.Context(), req.Method, u, req.Body)
	return http.DefaultTransport.RoundTrip(nr)
}

func TestAPackageLinkCannotPointBesideItsFolder(t *testing.T) {
	base := t.TempDir()
	write := func(links map[string]string) string {
		path := filepath.Join(base, "pkg.tgz")
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		gz := gzip.NewWriter(f)
		tw := tar.NewWriter(gz)
		for name, to := range links {
			if err := tw.WriteHeader(&tar.Header{Name: "package/" + name, Typeflag: tar.TypeSymlink, Linkname: to}); err != nil {
				t.Fatal(err)
			}
		}
		tw.Close()
		gz.Close()
		f.Close()
		return path
	}
	dir := filepath.Join(base, "node")
	for _, to := range []string{"../../node-evil/x", "../../../etc", "/etc/passwd"} {
		os.RemoveAll(dir)
		if err := untarGz(write(map[string]string{"bin/out": to}), dir, "package/"); err == nil {
			t.Errorf("a link to %q was written", to)
		}
	}
	os.RemoveAll(dir)
	if err := untarGz(write(map[string]string{"bin/cli": "../lib/cli.js", "lib/self": "."}), dir, "package/"); err != nil {
		t.Fatalf("a link inside the package was refused: %v", err)
	}
}
