// Package deploy brings new code into a site folder and builds it while
// the current build keeps serving. A deployment is: fetch the branch,
// check it out, run the build commands, and only if every one of them
// succeeded write the pointer file the manifest's env_from_file reads.
// The engine sees the pointer change on its next tick and hands over; this
// package never touches a process. Without a pointer, it asks the engine
// for a restart through the hook the app plugs in.
package deploy

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/hostgit"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/runtime"
)

// Options configure a Runner.
type Options struct {
	// DataDir is data/: logs go to logs/<domain>/deploy.log and the last
	// status to deploys/<domain>.json.
	DataDir string
	Bus     *events.Bus
	// Restart is called after a successful build when the manifest has no
	// pointer: nothing else would make the engine notice. Nil means the
	// log says so and nothing restarts.
	Restart func(ctx context.Context, domain string) error
	// Token returns the token for a repository (empty for public ones).
	// It is called at deploy time so a rotated token is used without a
	// restart and so it lives nowhere but in memory for the one command.
	Token func(repository string) string
	// GitURL turns owner/name into a clone URL; defaults to github.com.
	GitURL func(repository string) string
	// Timeout bounds one deployment. Default one hour.
	Timeout time.Duration
}

// Status is the record of a site's last deployment, in
// data/deploys/<domain>.json.
type Status struct {
	Domain     string    `json:"domain"`
	Repository string    `json:"repository"`
	Branch     string    `json:"branch"`
	Commit     string    `json:"commit,omitempty"`
	BuildID    string    `json:"build_id"`
	Actor      string    `json:"actor"`
	Status     string    `json:"status"` // "running" | "ok" | "failed"
	Error      string    `json:"error,omitempty"`
	Started    time.Time `json:"started"`
	Finished   time.Time `json:"finished,omitempty"`
	Log        string    `json:"log"`
}

// Runner serialises deployments per site.
type Runner struct {
	opts  Options
	mu    sync.Mutex
	sites map[string]*siteState
}

type siteState struct {
	mu      sync.Mutex // held for the whole deployment
	running bool
	again   bool
}

// New builds a Runner.
func New(opts Options) *Runner {
	if opts.Bus == nil {
		opts.Bus = events.New("")
	}
	if opts.Timeout == 0 {
		opts.Timeout = time.Hour
	}
	if opts.GitURL == nil {
		opts.GitURL = func(repo string) string { return "https://github.com/" + repo + ".git" }
	}
	return &Runner{opts: opts, sites: map[string]*siteState{}}
}

// Match reports whether a push to repository on ref ("refs/heads/main")
// is one m wants. The branch defaults to main; the repository name is
// compared without case because GitHub preserves the owner's spelling
// and nobody types it the same twice.
func Match(m *manifest.Manifest, repository, ref string) bool {
	if m == nil || m.Deploy == nil || m.Deploy.GitHub == nil {
		return false
	}
	g := m.Deploy.GitHub
	if !strings.EqualFold(strings.TrimSuffix(g.Repository, ".git"), strings.TrimSuffix(repository, ".git")) {
		return false
	}
	return ref == "refs/heads/"+Branch(g)
}

// Branch is the configured branch or main.
func Branch(g *manifest.GitHubDeploy) string {
	if g.Branch == "" {
		return "main"
	}
	return g.Branch
}

func (r *Runner) state(domain string) *siteState {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sites[domain]
	if !ok {
		s = &siteState{}
		r.sites[domain] = s
	}
	return s
}

// SetToken sets how the runner finds a repository's token. The runner is
// made with the sites before the GitHub module exists, and shared with it
// so the panel's Deploy and a push take one lock per site.
func (r *Runner) SetToken(f func(repository string) string) {
	r.mu.Lock()
	r.opts.Token = f
	r.mu.Unlock()
}

// Source says where a deployment's code comes from.
type Source int

const (
	// InPlace builds what is already in the site folder: the panel's
	// Deploy button on a site with no repository, or a pointer bump.
	InPlace Source = iota
	// FromGitHub fetches deploy.github's branch first.
	FromGitHub
)

// ErrDeploying says a deployment is already running for the site.
var ErrDeploying = errors.New("a deployment is already running")

// NewBuildID is the id of a deployment: sortable, and a valid folder name
// for a build that writes into a folder named after it.
func NewBuildID() string { return time.Now().UTC().Format("20060102T150405Z") }

// Run deploys one site and blocks until it is done. Deployments of the
// same site run one at a time; different sites run in parallel.
func (r *Runner) Run(ctx context.Context, domain, siteDir string, m *manifest.Manifest, actor string, from Source) (Status, error) {
	s := r.state(domain)
	s.mu.Lock()
	defer s.mu.Unlock()
	return r.run(ctx, domain, siteDir, m, actor, from, NewBuildID())
}

// Start runs a deployment in the background and returns its build id at
// once, or ErrDeploying when one is running: the operator pressing the
// button wants this build, not one queued behind another.
func (r *Runner) Start(domain, siteDir string, m *manifest.Manifest, actor string, from Source) (string, error) {
	s := r.state(domain)
	r.mu.Lock()
	if s.running {
		r.mu.Unlock()
		return "", ErrDeploying
	}
	s.running = true
	r.mu.Unlock()
	id := NewBuildID()
	r.background(s, domain, siteDir, m, actor, from, id)
	return id, nil
}

// background runs the claimed deployment and then, while a trigger arrived
// mid-build, runs it again. Start and Trigger share this loop: a push that
// lands during the operator's Deploy used to set `again` that only Trigger
// watched, so the newest commit was not built until the next push. An
// `again` is always a push, so every pass after the first is a GitHub
// build of the newest commit, whatever the first pass was. The caller has
// already set running under r.mu.
func (r *Runner) background(s *siteState, domain, siteDir string, m *manifest.Manifest, actor string, from Source, id string) {
	go func() {
		for {
			cur := m
			if from == FromGitHub {
				// Re-read: the push being built may have changed the manifest.
				if fresh, err := manifest.Load(siteDir); err == nil {
					cur = fresh
				}
			}
			s.mu.Lock()
			_, _ = r.run(context.Background(), domain, siteDir, cur, actor, from, id)
			s.mu.Unlock()
			r.mu.Lock()
			if !s.again {
				s.running = false
				r.mu.Unlock()
				return
			}
			s.again = false
			r.mu.Unlock()
			from, id = FromGitHub, NewBuildID()
		}
	}()
}

// Deploying reports whether a deployment of domain is running.
func (r *Runner) Deploying(domain string) bool {
	s := r.state(domain)
	r.mu.Lock()
	defer r.mu.Unlock()
	return s.running
}

// Trigger starts a deployment from GitHub in the background. A trigger
// that arrives while the site is deploying marks it to run once more when
// done: two pushes in a row end with the newest commit built, and ten
// pushes in a row do not queue ten builds.
func (r *Runner) Trigger(domain, siteDir string, m *manifest.Manifest, actor string) {
	if m == nil || m.Deploy == nil || m.Deploy.GitHub == nil {
		return
	}
	s := r.state(domain)
	r.mu.Lock()
	if s.running {
		s.again = true
		r.mu.Unlock()
		return
	}
	s.running = true
	r.mu.Unlock()
	r.background(s, domain, siteDir, m, actor, FromGitHub, NewBuildID())
}

// Last returns the last recorded status of a site.
func (r *Runner) Last(domain string) (Status, error) {
	var st Status
	err := fsx.ReadJSON(r.statusPath(domain), &st)
	return st, err
}

// All returns every recorded status, by domain.
func (r *Runner) All() []Status {
	entries, _ := os.ReadDir(filepath.Join(r.opts.DataDir, "deploys"))
	var out []Status
	for _, e := range entries {
		var st Status
		if fsx.ReadJSON(filepath.Join(r.opts.DataDir, "deploys", e.Name()), &st) == nil {
			out = append(out, st)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Domain < out[j].Domain })
	return out
}

func (r *Runner) statusPath(domain string) string {
	return filepath.Join(r.opts.DataDir, "deploys", domain+".json")
}

func (r *Runner) save(st Status) {
	_ = fsx.WriteJSON(r.statusPath(st.Domain), st, 0o644)
}

// run is one deployment, with the site's lock held by the caller.
func (r *Runner) run(ctx context.Context, domain, siteDir string, m *manifest.Manifest, actor string, from Source, buildID string) (Status, error) {
	// A disabled site is an operator's or the guard's decision, and a
	// deploy would build it and (lacking a pointer) start it again. A push
	// must not undo that: the site is built only once it is switched back
	// on. The decision is read from the local manifest, before any fetch,
	// so a repository that ships disabled:false cannot talk its way past it.
	if m != nil && m.Disabled {
		r.opts.Bus.Emit(events.Event{Type: "deploy.skipped", Subject: domain, Actor: actor, Level: "warn", Message: "the site is disabled; not deploying", Data: map[string]any{"build_id": buildID}})
		now := time.Now()
		return Status{Domain: domain, BuildID: buildID, Actor: actor, Status: "skipped", Started: now, Finished: now}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
	defer cancel()
	logPath := filepath.Join(r.opts.DataDir, "logs", domain, "deploy.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return Status{}, err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return Status{}, err
	}
	defer logFile.Close()
	var g manifest.GitHubDeploy
	if m != nil && m.Deploy != nil && m.Deploy.GitHub != nil {
		g = *m.Deploy.GitHub
	}
	st := Status{Domain: domain, BuildID: buildID, Actor: actor, Status: "running", Started: time.Now(), Log: logPath}
	if from == FromGitHub {
		st.Repository, st.Branch = g.Repository, Branch(&g)
	}
	r.save(st)
	r.opts.Bus.Emit(events.Event{Type: "deploy.started", Subject: domain, Actor: actor, Message: "build " + buildID, Data: map[string]any{"repository": st.Repository, "branch": st.Branch, "build_id": buildID}})
	fmt.Fprintf(logFile, "\n=== %s deploy %s (by %s)\n", st.Started.Format(time.RFC3339), buildID, actor)
	fail := func(err error) (Status, error) {
		st.Finished, st.Status, st.Error = time.Now(), "failed", err.Error()
		fmt.Fprintf(logFile, "=== failed: %v\n", err)
		r.save(st)
		r.opts.Bus.Emit(events.Event{Type: "deploy.failed", Subject: domain, Actor: actor, Level: "error", Message: err.Error(), Data: map[string]any{"build_id": buildID, "commit": st.Commit, "log": logPath}})
		return st, err
	}
	home := filepath.Join(r.opts.DataDir, "homes", domain)
	if from == FromGitHub {
		if !strings.Contains(g.Repository, "/") {
			return fail(errors.New("deploy.github.repository: want owner/name"))
		}
		token := ""
		r.mu.Lock()
		tokenOf := r.opts.Token
		r.mu.Unlock()
		if tokenOf != nil {
			token = tokenOf(g.Repository)
		}
		commit, err := Fetch(ctx, siteDir, &g, logFile, FetchOptions{GitURL: r.opts.GitURL(g.Repository), Token: token, Home: home})
		st.Commit = commit
		if err != nil {
			return fail(err)
		}
		// The checkout may have brought the repository's own manifest.
		if fresh, err := manifest.Load(siteDir); err == nil {
			m = fresh
			if m.Deploy != nil && m.Deploy.GitHub != nil {
				g = *m.Deploy.GitHub
			}
		}
	}
	pointerWritten, err := Build(ctx, domain, siteDir, m, &g, logFile, BuildOptions{BuildID: buildID, Commit: st.Commit, Home: home})
	if err != nil {
		return fail(err)
	}
	switch {
	case pointerWritten:
		fmt.Fprintf(logFile, "=== pointer %s -> %s; the engine hands over on its next tick\n", g.Pointer, buildID)
	case m != nil && m.Run != nil && r.opts.Restart != nil:
		if err := r.opts.Restart(ctx, domain); err != nil && !strings.Contains(err.Error(), "not a running site") {
			fmt.Fprintf(logFile, "=== built, but the restart failed: %v\n", err)
		} else {
			fmt.Fprintln(logFile, "=== no pointer: asked the engine to restart the site")
		}
	default:
		fmt.Fprintln(logFile, "=== no pointer and nothing to restart")
	}
	st.Status, st.Finished = "ok", time.Now()
	fmt.Fprintf(logFile, "=== done in %s\n", st.Finished.Sub(st.Started).Round(time.Millisecond))
	r.save(st)
	r.opts.Bus.Emit(events.Event{Type: "deploy.done", Subject: domain, Actor: actor, Message: "build " + buildID + " deployed", Data: map[string]any{"build_id": buildID, "commit": st.Commit, "pointer": pointerWritten, "duration": st.Finished.Sub(st.Started).String()}})
	return st, nil
}

// FetchOptions parametrise Fetch.
type FetchOptions struct {
	GitURL string
	// Token, when set, authenticates the fetch as x-access-token without
	// ever reaching .git/config: it travels in a header set on the one
	// command line, and the remote URL stays the public one.
	Token string
	// Home is the HOME git runs with, the site's own.
	Home string
}

// Fetch brings siteDir to the head of the deploy branch and returns the
// commit.
func Fetch(ctx context.Context, siteDir string, g *manifest.GitHubDeploy, log io.Writer, o FetchOptions) (string, error) {
	if o.GitURL == "" {
		o.GitURL = "https://github.com/" + g.Repository + ".git"
	}
	if err := os.MkdirAll(o.Home, 0o700); err != nil {
		return "", err
	}
	ro := RunOptions{Token: o.Token, Home: o.Home}
	branch := Branch(g)
	git := func(args ...string) error { return runGit(ctx, siteDir, ro, log, args...) }
	if !fsx.Exists(filepath.Join(siteDir, ".git")) {
		// The folder already holds site.json, so `git clone` into it is
		// refused; init + fetch + checkout gets the same result and keeps
		// the manifest. If the repository ships its own site.json, the
		// checkout overwrites the local one and the log says so.
		if err := git("init", "-q"); err != nil {
			return "", err
		}
		if err := git("remote", "add", "origin", o.GitURL); err != nil {
			return "", err
		}
	} else if err := git("remote", "set-url", "origin", o.GitURL); err != nil {
		if err := git("remote", "add", "origin", o.GitURL); err != nil {
			return "", err
		}
	}
	if err := git("fetch", "--prune", "--no-tags", "origin", "+refs/heads/"+branch+":refs/remotes/origin/"+branch); err != nil {
		return "", fmt.Errorf("fetch %s: %w", branch, err)
	}
	before, _ := os.ReadFile(filepath.Join(siteDir, manifest.FileName))
	if err := git("checkout", "-q", "-f", "-B", branch, "origin/"+branch); err != nil {
		return "", fmt.Errorf("checkout %s: %w", branch, err)
	}
	after, _ := os.ReadFile(filepath.Join(siteDir, manifest.FileName))
	if len(before) > 0 && string(after) != string(before) {
		fmt.Fprintln(log, "note: the repository ships its own site.json and it replaced the local one")
		// The repository's manifest does not get to undo the operator's and
		// the guard's decisions. Those live in the manifest precisely so
		// they survive a restart, and a checkout is a restart of the file:
		// carry them from the manifest that was here onto the new one, so a
		// push cannot revive a disabled site, lift a hold, start a stopped
		// worker or drop under-attack mode.
		if prior, perr := manifest.Parse(before); perr == nil {
			if fresh, ferr := manifest.Load(siteDir); ferr == nil && (carryCopy(fresh, prior) || carryDecisions(fresh, prior)) {
				if err := manifest.Save(siteDir, fresh); err != nil {
					return "", fmt.Errorf("keeping the site's decisions over its new site.json: %w", err)
				}
				fmt.Fprintln(log, "note: kept the site's disabled, hold, stopped-worker and under-attack decisions")
			}
		}
	}
	commit, err := gitOutput(ctx, siteDir, ro, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	commit = strings.TrimSpace(commit)
	fmt.Fprintf(log, "at %s\n", commit)
	return commit, nil
}

// carryDecisions copies onto m the decisions the house writes into a
// manifest at runtime and that must outlive a checkout that replaced
// site.json: a stopped site, a hold, stopped workers, under-attack mode.
// It only ever switches a decision on, never off, so neither the operator
// nor the repository can be overridden into a weaker state: a worker the
// repository ships disabled and one the operator stopped both stay down. It
// reports whether it changed anything, so the caller saves only when it
// must. Deploy configuration, build steps and everything else are the
// repository's to set.
func carryDecisions(m, prior *manifest.Manifest) bool {
	if m == nil || prior == nil {
		return false
	}
	changed := false
	if prior.Disabled && !m.Disabled {
		m.Disabled, changed = true, true
	}
	if prior.Hold != nil {
		h := *prior.Hold
		m.Hold, changed = &h, true
	}
	if prior.Attack != nil {
		m.Attack, changed = prior.Attack, true
	}
	if prior.WAF != nil && prior.WAF.UnderAttack {
		if m.WAF == nil {
			m.WAF = &manifest.WAF{}
		}
		if !m.WAF.UnderAttack {
			m.WAF.UnderAttack, changed = true, true
		}
	}
	for name, pw := range prior.Workers {
		if pw == nil || !pw.Disabled {
			continue
		}
		if w := m.Workers[name]; w != nil && !w.Disabled {
			w.Disabled, changed = true, true
		}
	}
	return changed
}

// carryCopy keeps a test copy a copy when its repository's site.json, which
// is production's, lands on it: the copy's mark, its password door, its
// branch (a preview's) and the names it answers are the house's, and a
// checkout that put production's back made the copy deploy production's
// branch, drop its password and claim production's aliases. carryDecisions
// still runs after it.
func carryCopy(m, prior *manifest.Manifest) bool {
	if m == nil || prior == nil || prior.Staging == nil {
		return false
	}
	carryDecisions(m, prior)
	s := *prior.Staging
	m.Staging = &s
	m.Access = prior.Access
	m.Aliases, m.ForceHTTPS, m.WWW = prior.Aliases, prior.ForceHTTPS, prior.WWW
	if prior.Title != "" {
		m.Title = prior.Title
	}
	m.Cluster, m.Replica, m.Backups, m.Previews = nil, nil, nil, nil
	if prior.Deploy != nil && prior.Deploy.GitHub != nil && m.Deploy != nil && m.Deploy.GitHub != nil {
		g := *m.Deploy.GitHub
		g.Branch = prior.Deploy.GitHub.Branch
		m.Deploy.GitHub = &g
	}
	mark := func(to, from *manifest.Run) {
		if to == nil || from == nil {
			return
		}
		to.Port = from.Port
		for _, k := range []string{"MIDDLEWARE_STAGING", "MIDDLEWARE_PREVIEW"} {
			if v, ok := from.Env[k]; ok {
				if to.Env == nil {
					to.Env = map[string]string{}
				}
				to.Env[k] = v
			}
		}
	}
	mark(m.Run, prior.Run)
	for name, sv := range m.Services {
		if p := prior.Services[name]; sv != nil && p != nil {
			mark(sv.Run, p.Run)
			sv.Domains = p.Domains
		}
	}
	return true
}

// BuildOptions parametrise Build.
type BuildOptions struct {
	BuildID string
	Commit  string
	Home    string
}

// Build runs deploy.github.build in the site folder, where the site runs
// (inside its image for a docker site: run on the host, native modules
// are built for the wrong system), each step in its own process group so
// a timeout kills the whole tree and not just the shell npm spawned. Then
// it writes the pointer, if there is one, and says so.
//
// Every step sees the site's resolved environment plus MIDDLEWARE_BUILD
// (the build id), DEPLOY_COMMIT and CI=true, and every env_from_file
// variable that reads the pointer set to the new build id, so a
// `next build` that reads NEXT_DIST_DIR writes into the folder the pointer
// is about to name. $MIDDLEWARE_BUILD, $DOMAIN and $SITE_DIR expand in the
// commands.
func Build(ctx context.Context, domain, siteDir string, m *manifest.Manifest, g *manifest.GitHubDeploy, log io.Writer, o BuildOptions) (pointerWritten bool, err error) {
	if g.Pointer != "" {
		p := filepath.Join(siteDir, g.Pointer)
		if !strings.HasPrefix(filepath.Clean(p), filepath.Clean(siteDir)+string(filepath.Separator)) {
			return false, fmt.Errorf("pointer %q leaves the site folder", g.Pointer)
		}
	}
	spec := &manifest.Spec{Domain: domain, SiteDir: siteDir, Cwd: siteDir, Env: map[string]string{}, Runtime: "process"}
	if m != nil && m.Run != nil {
		if resolved, err := manifest.Resolve(domain, siteDir, m); err == nil {
			spec = resolved
			spec.Cwd = siteDir
			for k, file := range m.Run.EnvFromFile {
				if g.Pointer != "" && filepath.Clean(file) == filepath.Clean(g.Pointer) {
					spec.Env[k] = o.BuildID
				}
			}
		}
	}
	extra := map[string]string{"MIDDLEWARE_BUILD": o.BuildID, "CI": "true"}
	if o.Commit != "" {
		extra["DEPLOY_COMMIT"] = o.Commit
	}
	vars := map[string]string{"MIDDLEWARE_BUILD": o.BuildID, "DOMAIN": domain, "SITE_DIR": siteDir}
	for i, raw := range g.Build {
		if len(raw) == 0 {
			return false, fmt.Errorf("step %d: empty command", i+1)
		}
		argv := make([]string, len(raw))
		for j, a := range raw {
			argv[j] = manifest.Expand(a, vars)
		}
		if strings.HasPrefix(argv[0], "./") || strings.HasPrefix(argv[0], "../") {
			argv[0] = filepath.Join(siteDir, argv[0])
		}
		fmt.Fprintf(log, "--- step %d/%d: %s\n", i+1, len(g.Build), strings.Join(raw, " "))
		code, err := runtime.Exec(ctx, spec, o.Home, runtime.ExecOptions{Args: argv, Stdout: log, Stderr: log, Env: extra})
		if err == nil && code != 0 {
			err = fmt.Errorf("exit %d", code)
		}
		if err != nil {
			return false, fmt.Errorf("step %d (%s): %w", i+1, raw[0], err)
		}
	}
	if g.Pointer == "" {
		return false, nil
	}
	if err := fsx.WriteAtomic(filepath.Join(siteDir, g.Pointer), []byte(o.BuildID+"\n"), 0o644); err != nil {
		return false, fmt.Errorf("pointer: %w", err)
	}
	fmt.Fprintf(log, "--- pointer %s -> %s\n", g.Pointer, o.BuildID)
	return true, nil
}

// RunOptions are what git runs with.
type RunOptions struct {
	Token string
	Home  string
}

// gitConfig is the token auth header for the one command when a token is
// set, passed to hostgit as an extra -c. The header is what GitHub's own
// checkout action sends; a URL with the token in it would be saved by
// `remote add` into .git/config, where it would outlive the command and
// reach the panel.
func gitConfig(o RunOptions) []string {
	if o.Token == "" {
		return nil
	}
	auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + o.Token))
	return []string{"-c", "http.extraheader=AUTHORIZATION: basic " + auth}
}

func hostOpts(dir string, o RunOptions) hostgit.Options {
	// A deploy runs in the site's own isolated home, so nothing but the
	// repository's own config (which hostgit vets) can speak to git.
	return hostgit.Options{Dir: dir, Home: o.Home, Config: gitConfig(o)}
}

func runGit(ctx context.Context, dir string, o RunOptions, log io.Writer, args ...string) error {
	fmt.Fprintf(log, "$ git %s\n", strings.Join(args, " "))
	cmd, err := hostgit.Command(ctx, hostOpts(dir, o), args...)
	if err != nil {
		fmt.Fprintf(log, "%v\n", err)
		return err
	}
	cmd.Stdout, cmd.Stderr = log, log
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w", args[0], err)
	}
	return nil
}

func gitOutput(ctx context.Context, dir string, o RunOptions, args ...string) (string, error) {
	cmd, err := hostgit.Command(ctx, hostOpts(dir, o), args...)
	if err != nil {
		return "", err
	}
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return string(out), nil
}
