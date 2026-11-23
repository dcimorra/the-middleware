package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/runtime"
)

// guestProbe says what a.test's jail can do in b.test and c.test.
func guestProbe(root string) string {
	b, c := filepath.Join(root, "sites", "b.test"), filepath.Join(root, "sites", "c.test")
	return `cat ` + b + `/secret.txt 2>/dev/null && echo READS-B; ` +
		`(echo x > ` + b + `/made) 2>/dev/null && echo WRITES-B; ` +
		`cat ` + c + `/secret.txt 2>/dev/null && echo READS-C; true`
}

func runAs(t *testing.T, root, site, script string) string {
	t.Helper()
	dir := filepath.Join(root, "sites", site)
	spec := &manifest.Spec{Domain: site, SiteDir: dir, Cwd: dir}
	var out bytes.Buffer
	code, err := runtime.Exec(context.Background(), spec, filepath.Join(root, "data", "homes", "a.test"), runtime.ExecOptions{Args: []string{"sh", "-c", script}, Stdout: &out, Stderr: &out})
	if err != nil || code != 0 {
		t.Fatalf("exit %d, err %v: %s", code, err, out.String())
	}
	return out.String()
}

// A site enters another only when that one's manifest names it: then its
// jail holds the host's folder, read-only or writable as the host said,
// and still nothing of a third site. What the visitor writes in its own
// manifest (the old chat.reads) opens nothing.
func TestAJailHoldsTheSitesThatTakeItsSiteIn(t *testing.T) {
	root, site, _ := jailedHouse(t)
	b := filepath.Join(root, "sites", "b.test")
	os.MkdirAll(filepath.Join(root, "sites", "c.test"), 0o755)
	os.WriteFile(filepath.Join(root, "sites", "c.test", "secret.txt"), []byte("c"), 0o644)
	os.WriteFile(filepath.Join(root, "sites", "c.test", "site.json"), []byte(`{"version":2,"static":{}}`), 0o644)

	// The visitor asks for itself: nothing.
	os.WriteFile(filepath.Join(site, "site.json"), []byte(`{"version":2,"static":{},"chat":{"reads":["b.test"]},"guests":{"a.test":"write"}}`), 0o644)
	os.WriteFile(filepath.Join(b, "site.json"), []byte(`{"version":2,"static":{}}`), 0o644)
	if out := runAs(t, root, "a.test", guestProbe(root)); strings.TrimSpace(out) != "" {
		t.Fatalf("a site let itself into another:\n%s", out)
	}

	// The host takes it in to read.
	os.WriteFile(filepath.Join(b, "site.json"), []byte(`{"version":2,"static":{},"guests":{"a.test":"read"}}`), 0o644)
	if out := runAs(t, root, "a.test", guestProbe(root)); !strings.Contains(out, "READS-B") || strings.Contains(out, "WRITES-B") || strings.Contains(out, "READS-C") {
		t.Fatalf("a guest to read: want b readable only, got:\n%s", out)
	}
	// A worker of the guest's is the guest's.
	dir := filepath.Join(root, "sites", "a.test")
	var out bytes.Buffer
	if _, err := runtime.Exec(context.Background(), &manifest.Spec{Domain: "a.test/queue", SiteDir: dir, Cwd: dir}, filepath.Join(root, "data", "homes", "a.test"), runtime.ExecOptions{Args: []string{"sh", "-c", guestProbe(root)}, Stdout: &out, Stderr: &out}); err != nil || !strings.Contains(out.String(), "READS-B") {
		t.Fatalf("a worker of the guest's does not see the host: %v\n%s", err, out.String())
	}

	// To write.
	os.WriteFile(filepath.Join(b, "site.json"), []byte(`{"version":2,"static":{},"guests":{"a.test":"write"}}`), 0o644)
	if out := runAs(t, root, "a.test", guestProbe(root)); !strings.Contains(out, "READS-B") || !strings.Contains(out, "WRITES-B") || strings.Contains(out, "READS-C") {
		t.Fatalf("a guest to write: want b writable, got:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(b, "made")); err != nil {
		t.Fatalf("what the guest wrote is not in the host's folder: %v", err)
	}

	// It is one way: the host does not enter its guest.
	if out := runAs(t, root, "b.test", `cat `+filepath.Join(site, "index.html")+` 2>/dev/null && echo READS-A; true`); strings.Contains(out, "READS-A") {
		t.Fatalf("taking a site in let the host into it:\n%s", out)
	}

	// Taken back: gone at the next start.
	os.WriteFile(filepath.Join(b, "site.json"), []byte(`{"version":2,"static":{}}`), 0o644)
	if out := runAs(t, root, "a.test", guestProbe(root)); strings.TrimSpace(out) != "" {
		t.Fatalf("a pass taken back still opens:\n%s", out)
	}
}
