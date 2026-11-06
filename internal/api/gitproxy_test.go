package api

import (
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dcimorra/the-middleware/internal/sitetoken"
)

// gitUpstream is GitHub played by git's own http-backend: it wants the
// module's token as x-access-token and serves the bare repositories
// under dir as <owner>/<name>.git.
func gitUpstream(t *testing.T, dir, token string) *httptest.Server {
	t.Helper()
	exec, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skip("git is not installed")
	}
	backend := filepath.Join(strings.TrimSpace(string(exec)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skip("git-http-backend is not installed")
	}
	cg := &cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + dir, "GIT_HTTP_EXPORT_ALL=1"}}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "x-access-token" || p != token {
			w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		cg.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func gitRun(t *testing.T, dir string, env []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestAConversationPushesToItsSitesRepositoryAndNowhereElse(t *testing.T) {
	f := newFixture(t)
	remotes := t.TempDir()
	up := gitUpstream(t, remotes, "module-secret")
	home := t.TempDir()
	base := []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t"}
	for _, name := range []string{"repo", "repo-other", "lib"} {
		bare := filepath.Join(remotes, "owner", name+".git")
		if out, err := gitRun(t, remotes, base, "init", "-q", "--bare", bare); err != nil {
			t.Fatal(out)
		}
		gitRun(t, bare, base, "config", "http.receivepack", "true")
	}
	keys := sitetoken.New(filepath.Join(f.root, "data"))
	f.srv.RegisterGitProxy(GitProxyOptions{
		Tokens: keys,
		Repositories: func(d string) []string {
			if d == "a.example" {
				// The deploy repository and one under repositories.
				return []string{"Owner/Repo", "owner/lib"}
			}
			return nil
		},
		Token:    func() string { return "module-secret" },
		Upstream: up.URL,
		Bus:      f.bus,
	})

	work := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"commit", "-q", "--allow-empty", "-m", "first"},
		{"remote", "add", "origin", "https://github.com/owner/repo.git"}, {"remote", "add", "other", "https://github.com/owner/repo-other.git"},
		{"remote", "add", "lib", "https://github.com/owner/lib.git"}} {
		if out, err := gitRun(t, work, base, args...); err != nil {
			t.Fatal(out)
		}
	}
	push := func(token string, args ...string) (string, error) {
		env := append([]string(nil), base...)
		for k, v := range GitEnv(f.http.URL, []string{"owner/repo", "owner/lib"}, token) {
			env = append(env, k+"="+v)
		}
		return gitRun(t, work, env, append([]string{"push"}, args...)...)
	}
	head := func(name string) string {
		out, _ := gitRun(t, filepath.Join(remotes, "owner", name+".git"), base, "rev-parse", "--verify", "-q", "refs/heads/main")
		return strings.TrimSpace(out)
	}
	local, _ := gitRun(t, work, base, "rev-parse", "HEAD")
	local = strings.TrimSpace(local)

	tok, _ := keys.GitToken("a.example")
	if out, err := push(tok, "origin", "main"); err != nil {
		t.Fatalf("the site's own push failed: %v\n%s", err, out)
	}
	if head("repo") != local {
		t.Fatalf("the push did not reach the repository: %q, want %q", head("repo"), local)
	}

	// A second repository the manifest lists pushes the same way.
	if out, err := push(tok, "lib", "main"); err != nil {
		t.Fatalf("the listed repository's push failed: %v\n%s", err, out)
	}
	if head("lib") != local {
		t.Fatal("the push did not reach the listed repository")
	}

	// A name that only starts like the site's is rewritten to the house
	// too, and refused there: it must not go out with the token.
	if out, err := push(tok, "other", "main"); err == nil {
		t.Fatalf("pushed to another repository:\n%s", out)
	}
	if head("repo-other") != "" {
		t.Fatal("another repository got the commit")
	}

	if out, err := push(tok, "origin", ":main"); err == nil {
		t.Fatalf("a branch was deleted from a conversation:\n%s", out)
	}
	if head("repo") != local {
		t.Fatal("the branch is gone")
	}

	// The door token (the site's running process has it) does not push,
	// nor a push token of a site that declares no repository.
	door, _ := keys.Token("a.example")
	stranger, _ := keys.GitToken("b.example")
	gitRun(t, work, base, "commit", "-q", "--allow-empty", "-m", "second")
	for _, bad := range []string{door, stranger, "nonsense"} {
		if out, err := push(bad, "origin", "main"); err == nil {
			t.Fatalf("token %q pushed:\n%s", bad, out)
		}
	}
	if head("repo") != local {
		t.Fatal("a refused push moved the branch")
	}
}

func TestTheTurnGetsTheGitSettingsAsVariablesWithNoFile(t *testing.T) {
	env := GitEnv("http://127.0.0.1:46100", []string{"owner/repo", "owner/lib"}, "tok")
	if env["GIT_CONFIG_COUNT"] != "7" {
		t.Fatalf("count %q", env["GIT_CONFIG_COUNT"])
	}
	if env["GIT_CONFIG_KEY_3"] != "url.http://127.0.0.1:46100"+GitPrefix+"/owner/lib.insteadOf" || env["GIT_CONFIG_VALUE_3"] != "https://github.com/owner/lib" {
		t.Fatalf("the second repository is not rewritten: %v", env)
	}
	if env["GIT_CONFIG_KEY_6"] != "http.http://127.0.0.1:46100"+GitPrefix+"/.extraHeader" || env["GIT_CONFIG_VALUE_6"] != "Authorization: Bearer tok" {
		t.Fatalf("the header goes elsewhere: %v", env)
	}
}
