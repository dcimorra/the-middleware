package github

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/deploy"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/modules"
	"github.com/dcimorra/the-middleware/internal/secrets"
)

func TestTheSignatureIsCheckedInFullAndAnEmptySecretAcceptsNothing(t *testing.T) {
	body := []byte(`{"ref":"refs/heads/main"}`)
	good := Sign("s3cret", body)
	if !VerifySignature("s3cret", body, good) {
		t.Fatal("a correct signature must verify")
	}
	if VerifySignature("other", body, good) {
		t.Fatal("another secret must not verify")
	}
	if VerifySignature("s3cret", append(body, ' '), good) {
		t.Fatal("a changed body must not verify")
	}
	if VerifySignature("s3cret", body, strings.TrimPrefix(good, "sha256=")) {
		t.Fatal("the sha256= prefix is part of the header")
	}
	if VerifySignature("s3cret", body, "") || VerifySignature("", body, Sign("", body)) {
		t.Fatal("a missing header or an empty secret must never verify")
	}
}

func newModule(t *testing.T) (*Module, *modules.Manager, *events.Bus) {
	t.Helper()
	t.Setenv(secrets.EnvKey, "")
	root := t.TempDir()
	bus := events.New("")
	mgr, err := modules.New(root, bus)
	if err != nil {
		t.Fatal(err)
	}
	m := New(mgr, bus, nil)
	mgr.Register(m)
	return m, mgr, bus
}

func TestEnableGeneratesTheWebhookSecretAndTheStatusRouteShowsIt(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user" || r.Header.Get("Authorization") != "Bearer ghp_good" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"login":"octocat"}`))
	}))
	defer api.Close()
	m, mgr, _ := newModule(t)
	m.APIBase = api.URL

	lines := m.Verify(context.Background(), modules.Config{"token": "ghp_bad"})
	if !modules.Failed(lines) {
		t.Fatalf("a token GitHub rejects must fail its line: %v", lines)
	}
	lines, err := mgr.Enable(context.Background(), Key, modules.Config{"token": "ghp_good"}, nil)
	if err != nil {
		t.Fatalf("%v %v", err, lines)
	}
	var sawLogin bool
	for _, l := range lines {
		if strings.Contains(l.Text, "acts as octocat") {
			sawLogin = true
		}
	}
	if !sawLogin {
		t.Fatalf("the token line must name the account: %v", lines)
	}
	cfg, _ := mgr.Config(Key)
	if len(cfg["webhook_secret"]) < 32 {
		t.Fatalf("a secret must be generated: %q", cfg["webhook_secret"])
	}
	raw, _ := os.ReadFile(filepath.Join(mgr.Root(), "data", modules.StoreFile))
	if strings.Contains(string(raw), cfg["webhook_secret"]) || strings.Contains(string(raw), "ghp_good") {
		t.Fatal("secrets must be encrypted on disk")
	}
	rec := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/github/status", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), cfg["webhook_secret"]) || !strings.Contains(rec.Body.String(), `"has_token":true`) {
		t.Fatalf("status: %d %s", rec.Code, rec.Body.String())
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@test", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestASignedPushDeploysTheSitesThatWantItAndNothingElse(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	m, mgr, bus := newModule(t)
	root := mgr.Root()
	if _, err := mgr.Enable(context.Background(), Key, modules.Config{}, nil); err != nil {
		t.Fatal(err)
	}
	cfg, _ := mgr.Config(Key)
	secret := cfg["webhook_secret"]

	// A bare repository stands in for github.com/acme/app.
	base := t.TempDir()
	work, bare := filepath.Join(base, "work"), filepath.Join(base, "origin.git")
	os.MkdirAll(work, 0o755)
	git(t, base, "init", "-q", "--bare", bare)
	git(t, work, "init", "-q")
	os.WriteFile(filepath.Join(work, "build.sh"), []byte("#!/bin/sh\necho built > built.txt\n"), 0o755)
	git(t, work, "add", ".")
	git(t, work, "commit", "-q", "-m", "first")
	git(t, work, "remote", "add", "origin", bare)
	git(t, work, "push", "-q", "origin", "main")
	m.UseRunner(deploy.New(deploy.Options{DataDir: filepath.Join(root, "data"), Bus: bus, GitURL: func(string) string { return bare }}))

	want := &manifest.Manifest{Version: 2, Deploy: &manifest.Deploy{GitHub: &manifest.GitHubDeploy{Repository: "acme/app", Build: [][]string{{"./build.sh"}}, Pointer: ".current"}}}
	other := &manifest.Manifest{Version: 2, Deploy: &manifest.Deploy{GitHub: &manifest.GitHubDeploy{Repository: "acme/app", Branch: "staging"}}}
	none := &manifest.Manifest{Version: 2}
	for domain, mf := range map[string]*manifest.Manifest{"app.test": want, "staging.test": other, "static.test": none} {
		dir := filepath.Join(root, "sites", domain)
		os.MkdirAll(dir, 0o755)
		if err := manifest.Save(dir, mf); err != nil {
			t.Fatal(err)
		}
	}
	events, stop := bus.Subscribe()
	defer stop()

	h := mgr.Handler()
	body := []byte(`{"ref":"refs/heads/main","after":"abc","repository":{"full_name":"Acme/App"},"pusher":{"name":"dev"}}`)
	post := func(sig, event string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/github/hook", bytes.NewReader(body))
		req.Header.Set("X-GitHub-Event", event)
		req.Header.Set("X-GitHub-Delivery", "d1")
		if sig != "" {
			req.Header.Set("X-Hub-Signature-256", sig)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := post("", "push"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("an unsigned push must be rejected, got %d", rec.Code)
	}
	if rec := post(Sign("wrong", body), "push"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a push signed with another secret must be rejected, got %d", rec.Code)
	}
	if rec := post(Sign(secret, body), "ping"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "pong") {
		t.Fatalf("ping: %d %s", rec.Code, rec.Body.String())
	}
	rec := post(Sign(secret, body), "push")
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"matched":["app.test"]`) {
		t.Fatalf("push: %d %s", rec.Code, rec.Body.String())
	}
	deadline := time.After(30 * time.Second)
	var rejected, done bool
	for !done {
		select {
		case e := <-events:
			switch e.Type {
			case "github.hook_rejected":
				rejected = true
			case "deploy.done":
				if e.Subject != "app.test" {
					t.Fatalf("deployed the wrong site: %s", e.Subject)
				}
				done = true
			case "deploy.failed":
				log, _ := os.ReadFile(filepath.Join(root, "data", "logs", "app.test", "deploy.log"))
				t.Fatalf("deploy failed: %s\n%s", e.Message, log)
			}
		case <-deadline:
			t.Fatal("the push did not end in a deploy")
		}
	}
	if !rejected {
		t.Fatal("rejected hooks must be visible as events")
	}
	if _, err := os.Stat(filepath.Join(root, "sites", "app.test", "built.txt")); err != nil {
		t.Fatal("the build must have run in the site folder")
	}
	if _, err := os.Stat(filepath.Join(root, "sites", "app.test", ".current")); err != nil {
		t.Fatal("the pointer must be written")
	}
	if _, err := os.Stat(filepath.Join(root, "sites", "staging.test", ".git")); err == nil {
		t.Fatal("the site on another branch must not be touched")
	}
	if _, err := m.Runner().Last("app.test"); err != nil {
		t.Fatal("the last status must be recorded:", err)
	}
}

func TestEveryPushReachesThePreviewsADeletedBranchToo(t *testing.T) {
	m, mgr, _ := newModule(t)
	if _, err := mgr.Enable(context.Background(), Key, modules.Config{}, nil); err != nil {
		t.Fatal(err)
	}
	cfg, _ := mgr.Config(Key)
	var told []Push
	m.Previews = func(p Push, actor string) []string {
		told = append(told, p)
		return []string{"x-preview.app.test"}
	}
	h := mgr.Handler()
	send := func(body string) string {
		req := httptest.NewRequest("POST", "/github/hook", strings.NewReader(body))
		req.Header.Set("X-GitHub-Event", "push")
		req.Header.Set("X-Hub-Signature-256", Sign(cfg["webhook_secret"], []byte(body)))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Body.String()
	}
	if out := send(`{"ref":"refs/heads/x","repository":{"full_name":"acme/app"},"pusher":{"name":"dev"}}`); !strings.Contains(out, "x-preview.app.test") {
		t.Fatalf("a push: %s", out)
	}
	if out := send(`{"ref":"refs/heads/x","deleted":true,"repository":{"full_name":"acme/app"},"pusher":{"name":"dev"}}`); !strings.Contains(out, `"previews":["x-preview.app.test"]`) {
		t.Fatalf("a deleted branch: %s", out)
	}
	if len(told) != 2 || told[0].Deleted || !told[1].Deleted {
		t.Fatalf("told: %+v", told)
	}
}
