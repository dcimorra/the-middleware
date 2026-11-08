package sites

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/deploy"
	"github.com/dcimorra/the-middleware/internal/manifest"
)

// gitRepo makes a repository with main and feature/login, each with its
// own index.html, and the site.json production has: the case where a
// checkout puts production's manifest on top of the copy's.
func gitRepo(t *testing.T, siteJSON string) string {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(siteJSON), 0o644)
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("main"), 0o644)
	git("add", ".")
	git("commit", "-q", "-m", "main")
	git("checkout", "-q", "-b", "feature/login")
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("login"), 0o644)
	git("commit", "-q", "-am", "login")
	git("checkout", "-q", "main")
	return dir
}

func TestABranchGetsAPreviewThatDeploysItAndGoesWithIt(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	f := newFixture(t, false)
	prodJSON := `{"version":2,"title":"Shop","aliases":["shop.example"],"deploy":{"github":{"repository":"acme/shop","branch":"main"}},"previews":{"branches":["feature/*"],"keep":"24h"}}`
	repo := gitRepo(t, prodJSON)
	f.m.Deploys = deploy.New(deploy.Options{DataDir: filepath.Join(f.root, "data"), Bus: f.bus, GitURL: func(string) string { return repo }})
	prod := f.m.Dir("shop.test")
	os.MkdirAll(prod, 0o755)
	os.WriteFile(filepath.Join(prod, "site.json"), []byte(prodJSON), 0o644)
	os.WriteFile(filepath.Join(prod, "index.html"), []byte("production"), 0o644)
	os.WriteFile(filepath.Join(prod, "shop.db"), []byte("customers"), 0o644)
	ctx := context.Background()

	if _, _, err := f.m.Preview(ctx, "shop.test", "main", "test"); err == nil {
		t.Fatal("the deployed branch got a preview")
	}
	if _, _, err := f.m.Preview(ctx, "shop.test", "hotfix", "test"); err == nil {
		t.Fatal("a branch the globs leave out got a preview")
	}
	name, created, err := f.m.Preview(ctx, "shop.test", "feature/login", "test")
	if err != nil || !created || name != "feature-login-preview.shop.test" {
		t.Fatalf("%q %v %v", name, created, err)
	}
	// Each wait is for a deploy after the one before: the record of the
	// last one is there until the next one writes its own.
	var lastStart time.Time
	waitDeploy := func() deploy.Status {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if st, err := f.m.Deploys.Last(name); err == nil && st.Status != "running" && !st.Started.Equal(lastStart) {
				lastStart = st.Started
				return st
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("the preview's deploy never ended")
		return deploy.Status{}
	}
	if st := waitDeploy(); st.Status != "ok" || st.Branch != "feature/login" {
		t.Fatalf("deploy: %+v", st)
	}
	dir := f.m.Dir(name)
	if b, _ := os.ReadFile(filepath.Join(dir, "index.html")); string(b) != "login" {
		t.Fatalf("the preview serves %q, not its branch", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "shop.db")); string(b) != "customers" {
		t.Fatal("the preview did not start from production's data")
	}
	if b, _ := os.ReadFile(filepath.Join(prod, "index.html")); string(b) != "production" {
		t.Fatal("production was touched")
	}
	// The repository's site.json (production's) landed on the copy; the
	// copy stays a copy of the branch, behind its password, without
	// production's names.
	mf, err := manifest.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if mf.Staging == nil || mf.Staging.Branch != "feature/login" || mf.Deploy.GitHub.Branch != "feature/login" || mf.Access == nil || mf.Access.Password == "" || len(mf.Aliases) != 0 || mf.Previews != nil || !strings.Contains(mf.Title, "feature/login") {
		t.Fatalf("the checkout undid the copy: %+v %+v %+v", mf.Staging, mf.Deploy.GitHub, mf.Access)
	}
	if list := f.m.Previews("shop.test"); len(list) != 1 || list[0].Name != name || list[0].Branch != "feature/login" {
		t.Fatalf("list: %+v", list)
	}

	// A second push deploys the same preview.
	if again, created, err := f.m.Preview(ctx, "shop.test", "feature/login", "test"); err != nil || created || again != name {
		t.Fatalf("second push: %q %v %v", again, created, err)
	}
	waitDeploy()

	// Within its time it stays; past it, it goes.
	if gone := f.m.ExpirePreviews(ctx, time.Now()); len(gone) != 0 {
		t.Fatalf("expired too early: %v", gone)
	}
	if gone := f.m.ExpirePreviews(ctx, time.Now().Add(25*time.Hour)); len(gone) != 1 || gone[0] != name {
		t.Fatalf("not expired: %v", gone)
	}
	// Made again and dropped with its branch.
	if _, _, err := f.m.Preview(ctx, "shop.test", "feature/login", "test"); err != nil {
		t.Fatal(err)
	}
	waitDeploy()
	if gone, err := f.m.DropPreview(ctx, "shop.test", "feature/login"); err != nil || gone != name {
		t.Fatalf("drop: %q %v", gone, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("the preview's folder is still there")
	}
}
