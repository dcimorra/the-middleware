// Package repos lists the git repositories inside the sites (where each is,
// its branch, its last commit, what is unpushed, unpulled or uncommitted)
// and, for the ones marked, keeps them up to date with their remote.
//
// What the watcher does, and above all what it does not:
//
//   - It is `git pull`, and nothing else: fetch, then merge the upstream
//     branch, fast-forward when it can and with a merge commit when the
//     two have diverged. Never reset, never force, never stash. A site's
//     folder belongs to its project and a conversation may be working in
//     it right now: when the merge collides (a conflict, an uncommitted
//     change it would overwrite) it is undone with `merge --abort`, the
//     folder stays as it was, and that is said.
//   - What fails shows. A marked repository that has not been able to pull
//     for three days must not look like one with nothing to pull: every
//     attempt leaves its result, the panel paints it, and the first failure
//     of a streak is an error event.
//   - The mark survives a restart: it is written to data/repos.json when it
//     is set.
//   - The house says which repository is meant. The API sends an id, which
//     is looked up among the repositories found under sites/; a path from
//     outside is never used.
//
// Pulling the code is not deploying it: a site that is built (Next, Go)
// serves what it served until it is built and handed over.
package repos

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/hostgit"
	"github.com/dcimorra/the-middleware/internal/manifest"
)

// Site is what the manager needs to know about a site.
type Site struct {
	Domain string
	Dir    string
	// Repositories are the site's GitHub repositories (its manifest's
	// deploy.github.repository, Path "" because it may be anywhere in the
	// folder, and repositories, each at its Path). Only these are fetched
	// with the GitHub module's token.
	Repositories []manifest.Repository
}

// Options configure a Manager.
type Options struct {
	DataDir string
	Sites   func() []Site
	Bus     *events.Bus
	// Every is the watcher's period; zero is two minutes.
	Every time.Duration
	// Token is the GitHub module's token, "" when the module is off or has
	// none. Read per pull, so a rotated token is used at once.
	Token func() string
	// GitHub is https://github.com; tests point it at a git http-backend.
	GitHub string
}

const (
	defaultEvery = 2 * time.Minute
	// perCommand bounds one git command: a fetch that the network does not
	// answer must not hold the watcher or a button.
	perCommand = 90 * time.Second
	// depth is how far below a site's folder repositories are looked for.
	// V1 found sites with two of them under build/.
	depth = 3
)

// skip are folders never descended into. Not archive.Rebuildable: that
// list answers "does a build give this back", and build/ holds real
// repositories in some sites; this one answers "can a repository of the
// site's own be in here".
var skip = map[string]bool{"node_modules": true, "vendor": true, "bower_components": true, "venv": true, ".venv": true, "__pycache__": true}

// Repo is one repository as listed.
type Repo struct {
	ID     string `json:"id"`
	Domain string `json:"domain"`
	// Path is relative to the site's folder; "." is the folder itself.
	Path     string `json:"path"`
	Branch   string `json:"branch"`
	Remote   string `json:"remote,omitempty"`
	Upstream string `json:"upstream,omitempty"`
	Head     Commit `json:"head"`
	// Ahead and Behind are against the upstream as of the last fetch.
	Ahead  int `json:"ahead"`
	Behind int `json:"behind"`
	// Dirty is how many paths have uncommitted changes.
	Dirty int     `json:"dirty"`
	Auto  bool    `json:"auto"`
	Last  *Result `json:"last,omitempty"`
	Error string  `json:"error,omitempty"`
}

// Commit is one commit.
type Commit struct {
	Hash    string    `json:"hash"`
	Subject string    `json:"subject"`
	Author  string    `json:"author"`
	At      time.Time `json:"at"`
}

// Result is what one pull did.
type Result struct {
	At      time.Time `json:"at"`
	OK      bool      `json:"ok"`
	Message string    `json:"message"`
	// Pulled is how many commits it brought.
	Pulled int `json:"pulled"`
	// Failures counts the failures in a row up to this one.
	Failures int `json:"failures,omitempty"`
}

// state is data/repos.json.
type state struct {
	Auto map[string]bool    `json:"auto"`
	Last map[string]*Result `json:"last"`
}

// Manager finds and pulls repositories.
type Manager struct {
	opts Options
	mu   sync.Mutex
	st   state
	// busy holds one lock per repository, so the watcher and a button
	// never run two pulls in one folder.
	busy sync.Map
}

// New loads data/repos.json.
func New(opts Options) *Manager {
	if opts.Every == 0 {
		opts.Every = defaultEvery
	}
	if opts.Bus == nil {
		opts.Bus = events.New("")
	}
	if opts.GitHub == "" {
		opts.GitHub = "https://github.com"
	}
	m := &Manager{opts: opts, st: state{Auto: map[string]bool{}, Last: map[string]*Result{}}}
	_ = fsx.ReadJSON(m.path(), &m.st)
	if m.st.Auto == nil {
		m.st.Auto = map[string]bool{}
	}
	if m.st.Last == nil {
		m.st.Last = map[string]*Result{}
	}
	return m
}

func (m *Manager) path() string { return filepath.Join(m.opts.DataDir, "repos.json") }

func (m *Manager) saveLocked() error { return fsx.WriteJSON(m.path(), m.st, 0o600) }

// found is a repository's place, before git is asked anything.
type found struct {
	id, domain, rel, dir string
	// repositories are the site's (Site.Repositories).
	repositories []manifest.Repository
}

// idOf is stable across restarts and renames nothing: the site and the
// path inside it.
func idOf(domain, rel string) string {
	sum := sha256.Sum256([]byte(domain + "\x00" + rel))
	return hex.EncodeToString(sum[:6])
}

func (m *Manager) find() []found {
	var out []found
	for _, s := range m.opts.Sites() {
		var walk func(dir, rel string, level int)
		walk = func(dir, rel string, level int) {
			if fi, err := os.Stat(filepath.Join(dir, ".git")); err == nil && (fi.IsDir() || fi.Mode().IsRegular()) {
				out = append(out, found{id: idOf(s.Domain, rel), domain: s.Domain, rel: rel, dir: dir, repositories: s.Repositories})
			}
			if level == depth {
				return
			}
			entries, _ := os.ReadDir(dir)
			for _, e := range entries {
				// Links are not followed: one pointing out of the site
				// would make its repositories this site's.
				if !e.IsDir() || e.Type()&os.ModeSymlink != 0 || strings.HasPrefix(e.Name(), ".") || skip[e.Name()] {
					continue
				}
				walk(filepath.Join(dir, e.Name()), filepath.Join(rel, e.Name()), level+1)
			}
		}
		walk(s.Dir, ".", 0)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].domain != out[j].domain {
			return out[i].domain < out[j].domain
		}
		return out[i].rel < out[j].rel
	})
	return out
}

func (m *Manager) lookup(id string) (found, bool) {
	for _, f := range m.find() {
		if f.id == id {
			return f, true
		}
	}
	return found{}, false
}

// git runs one git command in dir, hardened by hostgit: the repositories
// live in site folders a project or a conversation works in, so git must
// not be allowed to run a program the folder's own config or hooks name.
// The environment is minimal and secret-free (the supervisor's
// MIDDLEWARE_SECRET_KEY and the rest used to ride in on os.Environ), with
// HOME kept at the supervisor's so an ssh deploy key and ssh-agent still
// reach a pull; global config is kept out all the same, so only the
// repository's own vetted config speaks.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	return gitWith(ctx, dir, nil, args...)
}

// gitWith is git with config that carries a secret (see hostgit.Secret).
func gitWith(ctx context.Context, dir string, secret [][2]string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, perCommand)
	defer cancel()
	cmd, err := hostgit.Command(ctx, hostgit.Options{Dir: dir, Home: os.Getenv("HOME"), Passthrough: []string{"SSH_AUTH_SOCK"}, Secret: secret}, args...)
	if err != nil {
		return "", err
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		if msg == "" {
			msg = err.Error()
		}
		return out.String(), errors.New(msg)
	}
	return out.String(), nil
}

// fetchAuth is the config that lets a fetch of one of the site's own
// repositories carry the GitHub module's token, nil when it should carry
// none.
//
// Before this, the watcher only had what the supervisor's HOME offered
// (an ssh key, an ssh-agent) and a private repository answered "Repository
// not found" over https, while the same site's conversation pushed to it
// through the house's git proxy with the module's token.
//
// The token goes as an http.<url>.extraHeader whose url is the repository
// itself, so git sends it to that repository on GitHub and nowhere else:
// not to another remote, not to a host a folder's own insteadOf points
// at. It is limited to the repositories the site's manifest names
// (deploy.github.repository anywhere in the folder, each of repositories
// in its own path) for the same reason the git proxy is: a fine-grained token usually reaches every
// repository of its owner, and a site's folder is one its conversation
// writes to, remotes included, so "any GitHub remote" would let it pull
// someone else's private code into a folder it can read. Nothing is
// written to .git/config and nothing is on the command line.
func (m *Manager) fetchAuth(ctx context.Context, f found) [][2]string {
	if len(f.repositories) == 0 || m.opts.Token == nil {
		return nil
	}
	token := m.opts.Token()
	if token == "" {
		return nil
	}
	// What `git fetch` with no arguments will fetch: the branch's remote,
	// or origin, with the repository's own insteadOf already applied.
	raw, err := git(ctx, f.dir, "ls-remote", "--get-url")
	if err != nil {
		return nil
	}
	remote := strings.TrimSpace(raw)
	base := strings.TrimSuffix(m.opts.GitHub, "/")
	var path string
	for _, prefix := range []string{base + "/", "git@github.com:", "ssh://git@github.com/"} {
		if rest, ok := strings.CutPrefix(remote, prefix); ok {
			path = rest
			break
		}
	}
	// GitHub names are not case sensitive; git's URL match is, so the
	// header is keyed on the remote as written.
	name := strings.TrimSuffix(strings.TrimSuffix(path, "/"), ".git")
	if path == "" || !f.declares(name) {
		return nil
	}
	https := base + "/" + name
	auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	pairs := [][2]string{
		// The trailing slash makes git match on a path boundary: the key
		// for owner/repo must not also match owner/repo-other.
		{"http." + https + "/.extraHeader", "Authorization: Basic " + auth},
		{"http." + https + ".git/.extraHeader", "Authorization: Basic " + auth},
	}
	if !strings.HasPrefix(remote, base+"/") {
		// An ssh remote: the house has the token, not necessarily a key.
		pairs = append(pairs, [2]string{"url." + https + ".git.insteadOf", remote})
	}
	return pairs
}

// declares says whether the site's manifest names repository for this
// folder.
func (f found) declares(repository string) bool {
	for _, r := range f.repositories {
		if strings.EqualFold(r.Repository, repository) && (r.Path == "" || filepath.Clean(r.Path) == filepath.Clean(f.rel)) {
			return true
		}
	}
	return false
}

// cleanRemote drops credentials from a remote URL: a token written into
// it (https://user:token@github.com/…) must not reach the panel.
func cleanRemote(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = nil
	return u.String()
}

// describe asks git about one repository, without the network.
func (m *Manager) describe(ctx context.Context, f found) Repo {
	r := Repo{ID: f.id, Domain: f.domain, Path: f.rel}
	m.mu.Lock()
	r.Auto = m.st.Auto[f.id]
	if l := m.st.Last[f.id]; l != nil {
		c := *l
		r.Last = &c
	}
	m.mu.Unlock()
	branch, err := git(ctx, f.dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		r.Error = err.Error()
		return r
	}
	r.Branch = strings.TrimSpace(branch)
	if c, err := lastCommits(ctx, f.dir, 1); err == nil && len(c) == 1 {
		r.Head = c[0]
	}
	if remote, err := git(ctx, f.dir, "remote", "get-url", "origin"); err == nil {
		r.Remote = cleanRemote(strings.TrimSpace(remote))
	}
	if up, err := git(ctx, f.dir, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}"); err == nil {
		r.Upstream = strings.TrimSpace(up)
		if counts, err := git(ctx, f.dir, "rev-list", "--left-right", "--count", "HEAD...@{u}"); err == nil {
			if fs := strings.Fields(counts); len(fs) == 2 {
				r.Ahead, _ = strconv.Atoi(fs[0])
				r.Behind, _ = strconv.Atoi(fs[1])
			}
		}
	}
	if st, err := git(ctx, f.dir, "status", "--porcelain"); err == nil {
		for _, l := range strings.Split(st, "\n") {
			if strings.TrimSpace(l) != "" {
				r.Dirty++
			}
		}
	}
	return r
}

func lastCommits(ctx context.Context, dir string, n int) ([]Commit, error) {
	out, err := git(ctx, dir, "log", "-n", strconv.Itoa(n), "--format=%H%x1f%s%x1f%an%x1f%cI")
	if err != nil {
		return nil, err
	}
	var cs []Commit
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		p := strings.Split(l, "\x1f")
		if len(p) != 4 {
			continue
		}
		at, _ := time.Parse(time.RFC3339, p[3])
		cs = append(cs, Commit{Hash: p[0], Subject: p[1], Author: p[2], At: at})
	}
	return cs, nil
}

// ErrNotFound is an id that is no repository found under sites/.
var ErrNotFound = errors.New("no such repository")

// List describes every repository under sites/.
func (m *Manager) List(ctx context.Context) []Repo {
	fs := m.find()
	out := make([]Repo, 0, len(fs))
	for _, f := range fs {
		out = append(out, m.describe(ctx, f))
	}
	return out
}

// Log is a repository's last n commits.
func (m *Manager) Log(ctx context.Context, id string, n int) ([]Commit, error) {
	f, ok := m.lookup(id)
	if !ok {
		return nil, ErrNotFound
	}
	return lastCommits(ctx, f.dir, n)
}

// SetAuto marks a repository to be kept up to date, or unmarks it. Marking
// pulls once at once, so the mark is seen to work the moment it is set.
func (m *Manager) SetAuto(ctx context.Context, id string, on bool) (Repo, error) {
	f, ok := m.lookup(id)
	if !ok {
		return Repo{}, ErrNotFound
	}
	m.mu.Lock()
	if on {
		m.st.Auto[id] = true
	} else {
		delete(m.st.Auto, id)
	}
	err := m.saveLocked()
	m.mu.Unlock()
	if err != nil {
		return Repo{}, err
	}
	if on {
		m.pull(ctx, f, "operator")
	}
	return m.describe(ctx, f), nil
}

// Pull fetches and merges one repository now.
func (m *Manager) Pull(ctx context.Context, id string) (Repo, error) {
	f, ok := m.lookup(id)
	if !ok {
		return Repo{}, ErrNotFound
	}
	m.pull(ctx, f, "operator")
	return m.describe(ctx, f), nil
}

// ErrBusy is a pull already running in that repository.
var errBusy = errors.New("a pull is already running in this repository")

func (m *Manager) pull(ctx context.Context, f found, actor string) *Result {
	lock, _ := m.busy.LoadOrStore(f.id, &sync.Mutex{})
	mu := lock.(*sync.Mutex)
	if !mu.TryLock() {
		return &Result{At: time.Now().UTC(), Message: errBusy.Error()}
	}
	defer mu.Unlock()

	res := &Result{At: time.Now().UTC()}
	where := f.domain
	if f.rel != "." {
		where += "/" + f.rel
	}
	before, _ := git(ctx, f.dir, "rev-parse", "HEAD")
	_, err := gitWith(ctx, f.dir, m.fetchAuth(ctx, f), "fetch", "--quiet")
	if err == nil {
		if _, uerr := git(ctx, f.dir, "rev-parse", "@{u}"); uerr != nil {
			err = errors.New("the branch has no upstream to pull from")
		}
	}
	if err == nil {
		if _, merr := git(ctx, f.dir, "merge", "--no-edit", "@{u}"); merr != nil {
			// Undo whatever the merge started; the folder stays as it was.
			_, _ = git(ctx, f.dir, "merge", "--abort")
			err = fmt.Errorf("the merge did not go through and was undone: %v", merr)
		}
	}
	after, _ := git(ctx, f.dir, "rev-parse", "HEAD")
	if err == nil {
		res.OK = true
		if strings.TrimSpace(before) != strings.TrimSpace(after) {
			if n, cerr := git(ctx, f.dir, "rev-list", "--count", strings.TrimSpace(before)+".."+strings.TrimSpace(after)); cerr == nil {
				res.Pulled, _ = strconv.Atoi(strings.TrimSpace(n))
			}
			res.Message = fmt.Sprintf("%d commits pulled", res.Pulled)
			if res.Pulled == 1 {
				res.Message = "1 commit pulled"
			}
		} else {
			res.Message = "already up to date"
		}
	} else {
		res.Message = err.Error()
	}

	m.mu.Lock()
	prev := m.st.Last[f.id]
	if !res.OK {
		res.Failures = 1
		if prev != nil && !prev.OK {
			res.Failures = prev.Failures + 1
		}
	}
	m.st.Last[f.id] = res
	_ = m.saveLocked()
	m.mu.Unlock()

	data := map[string]any{"repo": f.id, "path": f.rel}
	switch {
	case !res.OK && res.Failures == 1:
		// The first failure of a streak is loud; the rest are recorded
		// and painted, not repeated every two minutes.
		m.opts.Bus.Emit(events.Event{Type: "repo.failed", Subject: f.domain, Actor: actor, Level: "error", Message: where + ": " + res.Message, Data: data})
	case !res.OK:
	case res.Pulled > 0:
		m.opts.Bus.Emit(events.Event{Type: "repo.pulled", Subject: f.domain, Actor: actor, Message: where + ": " + res.Message, Data: data})
	case prev != nil && !prev.OK:
		m.opts.Bus.Emit(events.Event{Type: "repo.recovered", Subject: f.domain, Actor: actor, Message: where + ": pulls again", Data: data})
	}
	return res
}

// Run is the watcher: every period, a pull in each marked repository.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(m.opts.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		m.mu.Lock()
		auto := make(map[string]bool, len(m.st.Auto))
		for id := range m.st.Auto {
			auto[id] = true
		}
		m.mu.Unlock()
		if len(auto) == 0 {
			continue
		}
		for _, f := range m.find() {
			if auto[f.id] && ctx.Err() == nil {
				m.pull(ctx, f, "engine")
			}
		}
	}
}
