// Package github is the deploy-on-push module: GitHub calls
// /api/v1/modules/github/hook, the signature is checked, and every site
// whose manifest names that repository and branch is deployed through
// internal/deploy. The module owns the webhook secret and the optional
// token for private repositories; the deploy itself knows nothing about
// GitHub beyond a clone URL.
package github

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/deploy"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/modules"
)

// Key is the module's key.
const Key = "github"

// maxBody bounds a webhook payload. GitHub's own limit is 25 MB; a push
// event is a few KB and this house has no use for the rest.
const maxBody = 2 << 20

// Site is a folder the hook may deploy.
type Site struct {
	Domain   string
	Dir      string
	Manifest *manifest.Manifest
}

// Module is the github module.
type Module struct {
	mgr *modules.Manager
	bus *events.Bus
	// Sites lists the deployable sites. The default scans sites/ and
	// reads each manifest, so the hook works with no engine wired in; the
	// app may replace it with the engine's snapshots.
	Sites func() []Site
	// APIBase is GitHub's API, for the token probe; tests override it.
	APIBase string
	HTTP    *http.Client
	runner  *deploy.Runner
	// Previews, when set, is told every push, a deleted branch's too, and
	// returns the previews it made, deployed or removed (the app's, which
	// owns the sites and the published names).
	Previews func(push Push, actor string) []string
}

// New builds the module. restart is what a deploy without a pointer
// calls (the engine's Restart); nil is allowed and logged.
func New(mgr *modules.Manager, bus *events.Bus, restart func(ctx context.Context, domain string) error) *Module {
	if bus == nil {
		bus = events.New("")
	}
	m := &Module{mgr: mgr, bus: bus, APIBase: "https://api.github.com"}
	root := mgr.Root()
	m.Sites = func() []Site { return scanSites(filepath.Join(root, "sites")) }
	m.runner = deploy.New(deploy.Options{
		DataDir: filepath.Join(root, "data"),
		Bus:     bus,
		Restart: restart,
		Token:   m.token,
	})
	return m
}

// Runner is the deploy runner, for the API's manual deploy endpoint.
func (m *Module) Runner() *deploy.Runner { return m.runner }

// UseRunner replaces the runner: tests point its GitURL at a local
// repository, and the app shares the sites' runner so the panel's Deploy
// and a push take one lock per site. It is given this module's token.
func (m *Module) UseRunner(r *deploy.Runner) {
	r.SetToken(m.token)
	m.runner = r
}

func (m *Module) Key() string  { return Key }
func (m *Module) Name() string { return "GitHub" }
func (m *Module) Description() string {
	return "Deploy on push: a webhook builds the site and hands over by pointer"
}

func (m *Module) Needs() []modules.Need {
	return []modules.Need{
		{Key: "webhook_secret", Label: "Webhook secret", Secret: true, Optional: true, Help: "Generated when empty. Paste it in the repository's webhook settings, content type application/json, push events only."},
		{Key: "token", Label: "Access token", Secret: true, Optional: true, Help: "For private repositories, a fine-grained token with Contents: Read on them; with Read and write, a site's conversation can also git push to the repository its manifest deploys from. Never written to disk in clear, and never shown to the conversation."},
	}
}

// token is what the deploy runner authenticates with, read from the
// store at deploy time so a rotated token is used without a restart.
func (m *Module) token(string) string {
	cfg, err := m.mgr.Config(Key)
	if err != nil {
		return ""
	}
	return cfg["token"]
}

// PushToken is what the house's git proxy pushes a conversation's work
// with: the module's token, while the module is enabled.
func (m *Module) PushToken() string {
	if !m.mgr.Enabled(Key) {
		return ""
	}
	return m.token("")
}

// Verify checks what can be checked from here: the token, when given,
// against the API. The webhook cannot be probed because GitHub is the
// one who calls, and the line says so.
func (m *Module) Verify(ctx context.Context, cfg modules.Config) []modules.Line {
	var lines []modules.Line
	if cfg["webhook_secret"] == "" {
		lines = append(lines, modules.OK("webhook secret will be generated and shown once enabled"))
	} else if len(cfg["webhook_secret"]) < 16 {
		lines = append(lines, modules.Fail("webhook secret is shorter than 16 characters; leave it empty to get a generated one"))
	} else {
		lines = append(lines, modules.OK("webhook secret set"))
	}
	if cfg["token"] == "" {
		lines = append(lines, modules.Skip("no access token: public repositories only"))
	} else {
		login, err := m.whoami(ctx, cfg["token"])
		if err != nil {
			lines = append(lines, modules.Fail("token: "+err.Error()))
		} else {
			lines = append(lines, modules.OK("token is valid, acts as "+login))
		}
	}
	lines = append(lines, modules.Skip("the webhook is not probed: GitHub calls this installation, not the other way round. The first push shows in the events as deploy.started or as github.hook_rejected."))
	return lines
}

func (m *Module) whoami(ctx context.Context, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(m.APIBase, "/")+"/user", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "middleware")
	client := m.HTTP
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github answered %s", resp.Status)
	}
	var u struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&u); err != nil {
		return "", err
	}
	return u.Login, nil
}

// Install has nothing to fetch: git is the only tool and it is the
// system's.
func (m *Module) Install(ctx context.Context, cfg modules.Config, progress func(string)) error {
	path, err := exec.LookPath("git")
	if err != nil {
		return errors.New("git is not installed on this machine")
	}
	progress("git found at " + path)
	return nil
}

// Enable generates the webhook secret when the operator left it empty.
// It goes into cfg, which the manager saves, and the panel shows it from
// the config once; a secret the operator never sees cannot be pasted
// into GitHub.
func (m *Module) Enable(ctx context.Context, cfg modules.Config) error {
	if cfg["webhook_secret"] == "" {
		b := make([]byte, 24)
		if _, err := rand.Read(b); err != nil {
			return err
		}
		cfg["webhook_secret"] = hex.EncodeToString(b)
	}
	return nil
}

func (m *Module) Disable(ctx context.Context) error { return nil }

// Routes: "hook" is what GitHub calls; "status" lists the last deploy of
// every site and the hook's path for the panel to show.
func (m *Module) Routes() map[string]http.Handler {
	return map[string]http.Handler{
		"POST /hook":                http.HandlerFunc(m.handleHook),
		"GET /status":               http.HandlerFunc(m.handleStatus),
		"GET /repos":                http.HandlerFunc(m.handleRepos),
		"GET /repos/{owner}/{name}": http.HandlerFunc(m.handleInspect),
	}
}

// Push is the part of GitHub's push event this house reads.
type Push struct {
	Ref        string `json:"ref"`
	After      string `json:"after"`
	Deleted    bool   `json:"deleted"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Pusher struct {
		Name string `json:"name"`
	} `json:"pusher"`
}

// VerifySignature checks X-Hub-Signature-256 over body with secret, in
// constant time. An empty secret never verifies: a module enabled with
// no secret must reject everything, not accept everything.
func VerifySignature(secret string, body []byte, header string) bool {
	if secret == "" {
		return false
	}
	sig, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	want, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), want)
}

// Sign produces the header GitHub would send; tests and the panel's
// "send a test push" use it.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func (m *Module) handleHook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, errors.New("POST only"))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		writeError(w, http.StatusRequestEntityTooLarge, errors.New("payload too large"))
		return
	}
	cfg, err := m.mgr.Config(Key)
	if err != nil || !VerifySignature(cfg["webhook_secret"], body, r.Header.Get("X-Hub-Signature-256")) {
		m.bus.Emit(events.Event{Type: "github.hook_rejected", Actor: "github", Level: "warn", Message: "bad or missing signature", Data: map[string]any{"delivery": r.Header.Get("X-GitHub-Delivery"), "remote": r.RemoteAddr}})
		writeError(w, http.StatusUnauthorized, errors.New("bad signature"))
		return
	}
	event := r.Header.Get("X-GitHub-Event")
	switch event {
	case "ping":
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pong": true})
		return
	case "push":
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ignored": event})
		return
	}
	var push Push
	if err := json.Unmarshal(body, &push); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("not a push event"))
		return
	}
	if push.Deleted {
		// A deleted branch deploys nothing; its preview, if it had one,
		// goes with it.
		var gone []string
		if m.Previews != nil {
			gone = m.Previews(push, "github:"+push.Pusher.Name)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ignored": "branch deleted", "previews": gone})
		return
	}
	matched := m.Dispatch(push, "github:"+push.Pusher.Name)
	m.bus.Emit(events.Event{Type: "github.push", Actor: "github", Message: push.Repository.FullName + " " + push.Ref, Data: map[string]any{"repository": push.Repository.FullName, "ref": push.Ref, "commit": push.After, "matched": matched, "delivery": r.Header.Get("X-GitHub-Delivery")}})
	status := http.StatusAccepted
	if len(matched) == 0 {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"ok": true, "matched": matched})
}

// Dispatch triggers a deploy of every site that wants push and returns
// their domains. It is what the hook does after the signature, exposed
// so a test or the panel can push without GitHub.
func (m *Module) Dispatch(push Push, actor string) []string {
	matched := []string{}
	for _, s := range m.Sites() {
		if !deploy.Match(s.Manifest, push.Repository.FullName, push.Ref) {
			continue
		}
		matched = append(matched, s.Domain)
		m.runner.Trigger(s.Domain, s.Dir, s.Manifest, actor)
	}
	if m.Previews != nil {
		matched = append(matched, m.Previews(push, actor)...)
	}
	return matched
}

func (m *Module) handleStatus(w http.ResponseWriter, r *http.Request) {
	var sites []map[string]any
	for _, s := range m.Sites() {
		if s.Manifest == nil || s.Manifest.Deploy == nil || s.Manifest.Deploy.GitHub == nil {
			continue
		}
		g := s.Manifest.Deploy.GitHub
		entry := map[string]any{"domain": s.Domain, "repository": g.Repository, "branch": deploy.Branch(g), "pointer": g.Pointer}
		if last, err := m.runner.Last(s.Domain); err == nil {
			entry["last"] = last
		}
		sites = append(sites, entry)
	}
	cfg, _ := m.mgr.Config(Key)
	writeJSON(w, http.StatusOK, map[string]any{
		"hook_path":      "/api/v1/modules/github/hook",
		"webhook_secret": cfg["webhook_secret"],
		"has_token":      cfg["token"] != "",
		"sites":          sites,
	})
}

// scanSites reads every manifest under sitesDir. A folder whose manifest
// does not parse is skipped here; the engine already reports it.
func scanSites(sitesDir string) []Site {
	entries, err := os.ReadDir(sitesDir)
	if err != nil {
		return nil
	}
	var out []Site
	for _, e := range entries {
		domain := e.Name()
		if !manifest.ValidName(domain) {
			continue
		}
		dir := filepath.Join(sitesDir, domain)
		if !fsx.Exists(filepath.Join(dir, manifest.FileName)) {
			continue
		}
		mf, err := manifest.Load(dir)
		if err != nil {
			continue
		}
		out = append(out, Site{Domain: domain, Dir: dir, Manifest: mf})
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
