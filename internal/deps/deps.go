// Package deps keeps the external programs the modules run: the agent
// CLIs (Claude Code, Codex, Antigravity…) and the Node they need. They are not
// Go imports and never will be; they are npm packages with release
// channels of their own, so they live outside the binary, in
// <root>/modules/node, and this package installs them when a module is
// enabled and updates them on its own afterwards. The operator never
// types an npm command: enabling the module is the install, and the
// panel says which version runs and when it was last checked.
//
// Node itself is fetched from nodejs.org as a tarball verified against
// the published SHASUMS256, because a distribution's nodejs is often too
// old for the CLIs (Debian 12 ships 18, Qwen Code wants 20). The
// container image ships both already; what this package adds there is
// the update.
package deps

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
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
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/fsx"
)

// Package is one npm package that provides one command.
type Package struct {
	// Name is the command (claude, agy): what modules look for.
	Name string `json:"name"`
	// NPM is the package name on the registry.
	NPM string `json:"npm"`
	// MinNode is the Node major the package needs.
	MinNode int `json:"-"`
	// Binary and PyPI are the other two kinds (kinds.go); NPM is empty
	// for them.
	Binary *Binary `json:"-"`
	PyPI   string  `json:"pypi,omitempty"`
}

// Packages is the table. A module names its command; this is where the
// command comes from.
var Packages = []Package{
	{Name: "claude", NPM: "@anthropic-ai/claude-code", MinNode: 18},
	// opencode-ai is a launcher; the binary comes in a per-platform
	// optional dependency npm picks for this machine.
	{Name: "opencode", NPM: "opencode-ai", MinNode: 20},
	{Name: "codex", NPM: "@openai/codex", MinNode: 18},
	{Name: "qwen", NPM: "@qwen-code/qwen-code", MinNode: 20},
	// Kimi Code wants Node 22.19+; the Node this package fetches is 22.22.
	{Name: "kimi", NPM: "@moonshot-ai/kimi-code", MinNode: 22},
	// OpenSpec, for spec-driven work in the sites' projects. It wants Node
	// 20.19+, which a bare major cannot say: 22, as Kimi.
	{Name: "openspec", NPM: "@fission-ai/openspec", MinNode: 22},
	{Name: "grok", Binary: &Binary{
		Latest: "https://x.ai/cli/stable",
		URL:    "https://x.ai/cli/grok-{version}-linux-{arch}",
		Arch:   map[string]string{"amd64": "x86_64", "arm64": "aarch64"},
	}},
	{Name: "vibe", PyPI: "mistral-vibe"},
	// Google's Antigravity CLI: the manifest its own install.sh reads.
	{Name: "agy", Binary: &Binary{
		Manifest: "https://antigravity-cli-auto-updater-974169037036.us-central1.run.app/manifests/linux_{arch}.json",
		Member:   "antigravity",
		Arch:     map[string]string{"amd64": "amd64", "arm64": "arm64"},
	}},
}

// Node release the installer fetches when the machine has none that will
// do. Pinned, with the checksums from nodejs.org/dist/<v>/SHASUMS256.txt
// copied here: the download is verified against what this binary says,
// not against a file fetched over the same connection.
const (
	NodeVersion = "v22.22.0"
	nodeDist    = "https://nodejs.org/dist/"
)

var nodeSHA = map[string]string{
	"x64":   "c33c39ed9c80deddde77c960d00119918b9e352426fd604ba41638d6526a4744",
	"arm64": "25ba95dfb96871fa2ef977f11f95ea90818c8fa15c0f2110771db08d4ba423be",
}

// Download is the HTTP client, replaceable by tests.
var Download = &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 30 * time.Second, Proxy: http.ProxyFromEnvironment}}

// Status is one package as the panel and the CLI see it.
type Status struct {
	Name      string `json:"name"`
	NPM       string `json:"npm"`
	Path      string `json:"path,omitempty"`
	Version   string `json:"version,omitempty"`
	Latest    string `json:"latest,omitempty"`
	Outdated  bool   `json:"outdated"`
	Installed bool   `json:"installed"`
	// Managed says the copy that runs is ours (under modules/node), so an
	// update replaces it; a copy found elsewhere in PATH is reported but
	// left alone.
	Managed   bool      `json:"managed"`
	CheckedAt time.Time `json:"checked_at,omitempty"`
	Error     string    `json:"error,omitempty"`
}

// Manager installs and updates under a root.
type Manager struct {
	Root string
	Bus  *events.Bus
	// Interval between update checks; a day by default.
	Interval time.Duration
	// AutoUpdate installs newer versions when the check finds them;
	// otherwise the check only reports.
	AutoUpdate func() bool
	// Exec runs a command; tests replace it.
	Exec func(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error)

	mu      sync.Mutex
	last    map[string]Status
	running bool
}

// New builds a manager for root.
func New(root string, bus *events.Bus) *Manager {
	return &Manager{Root: root, Bus: bus, Interval: 24 * time.Hour, AutoUpdate: func() bool { return true }, Exec: run, last: map[string]Status{}}
}

// Prefix is the npm prefix: binaries in <prefix>/bin, packages in
// <prefix>/lib/node_modules.
func (m *Manager) Prefix() string { return filepath.Join(m.Root, "modules", "node") }

// BinDir is where the commands land.
func (m *Manager) BinDir() string { return filepath.Join(m.Prefix(), "bin") }

// RuntimeDir is where a downloaded Node lives.
func (m *Manager) RuntimeDir() string { return filepath.Join(m.Root, "modules", "node-runtime") }

// Path is PATH for anything that should find what this package installed:
// the managed binaries first, then the runtime, then the process's own.
func (m *Manager) Path() string {
	parts := []string{m.BinDir(), filepath.Join(m.RuntimeDir(), "bin")}
	if p := os.Getenv("PATH"); p != "" {
		parts = append(parts, p)
	}
	return strings.Join(parts, string(os.PathListSeparator))
}

// Find locates a command: the managed copy, else PATH. managed is true
// for our copy and for the image's seed (MIDDLEWARE_DEPS_SEED names its
// bin directory): both are ours to replace with a newer release, which
// lands under the prefix and shadows the seed from then on. A copy found
// elsewhere in PATH is somebody else's and is reported, never touched.
func (m *Manager) Find(name string) (path string, managed bool, err error) {
	managedPath := filepath.Join(m.BinDir(), name)
	if st, err := os.Stat(managedPath); err == nil && !st.IsDir() && st.Mode().Perm()&0o111 != 0 {
		return managedPath, true, nil
	}
	p, err := exec.LookPath(name)
	if err != nil {
		return "", false, err
	}
	if seed := os.Getenv("MIDDLEWARE_DEPS_SEED"); seed != "" && filepath.Dir(p) == filepath.Clean(seed) {
		return p, true, nil
	}
	return p, false, nil
}

func (m *Manager) pkg(name string) (Package, bool) {
	for _, p := range Packages {
		if p.Name == name {
			return p, true
		}
	}
	return Package{}, false
}

// Version of the managed copy, from its package.json; empty when not
// installed there.
func (m *Manager) installedVersion(p Package) string {
	switch {
	case p.Binary != nil:
		raw, err := os.ReadFile(m.binaryVersionPath(p))
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(raw))
	case p.PyPI != "":
		return m.uvVersion(p)
	}
	raw, err := os.ReadFile(filepath.Join(m.Prefix(), "lib", "node_modules", p.NPM, "package.json"))
	if err != nil {
		return ""
	}
	var pj struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(raw, &pj) != nil {
		return ""
	}
	return pj.Version
}

// Statuses reports every package, with the last check's result.
func (m *Manager) Statuses() []Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Status, 0, len(Packages))
	for _, p := range Packages {
		s := m.last[p.Name]
		s.Name, s.NPM = p.Name, p.NPM
		path, managed, err := m.Find(p.Name)
		s.Path, s.Managed, s.Installed = path, managed, err == nil
		if v := m.installedVersion(p); v != "" && strings.HasPrefix(path, m.Prefix()) {
			s.Version = v
		} else if s.Installed && s.Version == "" {
			s.Version = m.versionOf(path)
			m.last[p.Name] = Status{Latest: s.Latest, CheckedAt: s.CheckedAt, Error: s.Error, Version: s.Version}
		}
		s.Outdated = s.Latest != "" && s.Version != "" && newer(s.Latest, s.Version)
		out = append(out, s)
	}
	return out
}

// versionOf asks a binary found outside our prefix for its version, so
// the panel can still say what runs. Cached in last[].
func (m *Manager) versionOf(path string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := m.Exec(ctx, os.TempDir(), []string{"HOME=" + os.TempDir(), "PATH=" + m.Path()}, path, "--version")
	if err != nil {
		return ""
	}
	v := strings.TrimSpace(string(out))
	if i := strings.IndexByte(v, '\n'); i >= 0 {
		v = v[:i]
	}
	if mm := semverRe.FindString(v); mm != "" {
		return mm
	}
	return v
}

var semverRe = regexp.MustCompile(`\d+\.\d+\.\d+[0-9A-Za-z.+-]*`)

// newer reports whether a is a higher semver than b.
func newer(a, b string) bool {
	pa, pb := semverParts(a), semverParts(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

func semverParts(v string) [3]int {
	var out [3]int
	v = strings.TrimPrefix(semverRe.FindString(v), "v")
	for i, f := range strings.SplitN(strings.SplitN(v, "-", 2)[0], ".", 3) {
		if i < 3 {
			out[i], _ = strconv.Atoi(strings.TrimFunc(f, func(r rune) bool { return r < '0' || r > '9' }))
		}
	}
	return out
}

// Install puts the package in the prefix (fetching Node first when none
// will do) and reports each step through progress. Idempotent: an
// installed package is reinstalled only when version is given and
// differs, or when latest is asked for.
func (m *Manager) Install(ctx context.Context, name string, progress func(string)) error {
	return m.install(ctx, name, "latest", progress)
}

func (m *Manager) install(ctx context.Context, name, want string, progress func(string)) error {
	if progress == nil {
		progress = func(string) {}
	}
	p, ok := m.pkg(name)
	if !ok {
		return fmt.Errorf("deps: %q is not a package this installation manages", name)
	}
	if p.Binary != nil || p.PyPI != "" {
		var v string
		var err error
		if p.Binary != nil {
			v, err = m.installBinary(ctx, p, want, progress)
		} else {
			v, err = m.installUV(ctx, p, want, progress)
		}
		if err != nil {
			return err
		}
		progress(fmt.Sprintf("%s %s installed", p.Name, v))
		m.mu.Lock()
		s := m.last[p.Name]
		s.Version = v
		m.last[p.Name] = s
		m.mu.Unlock()
		m.emit("deps.installed", p.Name, "", p.Name+" "+v+" installed", map[string]any{"version": v})
		return nil
	}
	node, err := m.ensureNode(ctx, p.MinNode, progress)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.Prefix(), 0o755); err != nil {
		return err
	}
	progress(fmt.Sprintf("npm install %s@%s into %s", p.NPM, want, m.Prefix()))
	env := []string{
		"PATH=" + filepath.Dir(node) + string(os.PathListSeparator) + m.Path(),
		"HOME=" + m.Prefix(),
		// npm writes a cache under HOME; in a container running as another
		// user that is a permission denied halfway through the install.
		"npm_config_cache=" + filepath.Join(m.Prefix(), ".npm"),
		"npm_config_update_notifier=false",
		"npm_config_fund=false",
		"npm_config_audit=false",
	}
	for _, k := range []string{"HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "no_proxy", "SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	npm := filepath.Join(filepath.Dir(node), "npm")
	ictx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	out, err := m.Exec(ictx, m.Prefix(), env, node, npm, "install", "-g", "--prefix", m.Prefix(), p.NPM+"@"+want)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			progress(line)
		}
	}
	if err != nil {
		return fmt.Errorf("deps: npm install %s: %w", p.NPM, err)
	}
	v := m.installedVersion(p)
	progress(fmt.Sprintf("%s %s installed", p.Name, v))
	m.mu.Lock()
	s := m.last[p.Name]
	s.Version = v
	m.last[p.Name] = s
	m.mu.Unlock()
	m.emit("deps.installed", p.Name, "", p.Name+" "+v+" installed", map[string]any{"version": v})
	return nil
}

// ensureNode returns a node binary of at least minMajor: the runtime we
// downloaded, else one in PATH that is new enough, else a fresh download.
func (m *Manager) ensureNode(ctx context.Context, minMajor int, progress func(string)) (string, error) {
	own := filepath.Join(m.RuntimeDir(), "bin", "node")
	if major := m.nodeMajor(own); major >= minMajor && major > 0 {
		return own, nil
	}
	if p, err := exec.LookPath("node"); err == nil {
		if major := m.nodeMajor(p); major >= minMajor {
			// npm must sit next to it or the install line cannot find it.
			if _, err := os.Stat(filepath.Join(filepath.Dir(p), "npm")); err == nil {
				return p, nil
			}
		}
	}
	progress("no Node " + strconv.Itoa(minMajor) + "+ on this machine: fetching " + NodeVersion + " from nodejs.org")
	if err := m.fetchNode(ctx, progress); err != nil {
		return "", err
	}
	if major := m.nodeMajor(own); major < minMajor {
		return "", fmt.Errorf("deps: the downloaded node reports major %d, want %d+", major, minMajor)
	}
	return own, nil
}

func (m *Manager) nodeMajor(path string) int {
	if _, err := os.Stat(path); err != nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := m.Exec(ctx, os.TempDir(), []string{"PATH=" + os.Getenv("PATH")}, path, "--version")
	if err != nil {
		return 0
	}
	v := strings.TrimPrefix(strings.TrimSpace(string(out)), "v")
	major, _ := strconv.Atoi(strings.SplitN(v, ".", 2)[0])
	return major
}

// fetchNode downloads the pinned release for this architecture, verifies
// it and unpacks it into RuntimeDir through a temp dir renamed into place.
func (m *Manager) fetchNode(ctx context.Context, progress func(string)) error {
	arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[runtime.GOARCH]
	if arch == "" || runtime.GOOS != "linux" {
		return fmt.Errorf("deps: no Node download for %s/%s; install node 20+ and npm in PATH", runtime.GOOS, runtime.GOARCH)
	}
	want := nodeSHA[arch]
	name := fmt.Sprintf("node-%s-linux-%s", NodeVersion, arch)
	url := nodeDist + NodeVersion + "/" + name + ".tar.gz"
	progress("downloading " + url)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := Download.Do(req)
	if err != nil {
		return fmt.Errorf("deps: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("deps: %s answered %s", url, resp.Status)
	}
	parent := filepath.Dir(m.RuntimeDir())
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	tmpFile, err := os.CreateTemp(parent, ".node.*.tar.gz")
	if err != nil {
		return err
	}
	defer os.Remove(tmpFile.Name())
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmpFile, h), resp.Body); err != nil {
		tmpFile.Close()
		return err
	}
	tmpFile.Close()
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("deps: %s: sha256 %s, want %s (the download is not what nodejs.org published)", name, got, want)
	}
	progress("checksum ok, unpacking")
	tmpDir, err := os.MkdirTemp(parent, ".node-runtime.*")
	if err != nil {
		return err
	}
	if err := untarGz(tmpFile.Name(), tmpDir, name+"/"); err != nil {
		os.RemoveAll(tmpDir)
		return err
	}
	_ = os.RemoveAll(m.RuntimeDir())
	if err := os.Rename(tmpDir, m.RuntimeDir()); err != nil {
		os.RemoveAll(tmpDir)
		return err
	}
	_ = fsx.WriteAtomic(filepath.Join(m.RuntimeDir(), "VERSION"), []byte(NodeVersion+"\n"), 0o644)
	progress("node " + NodeVersion + " ready")
	return nil
}

// untarGz extracts the entries under strip into dir. Only regular files,
// directories and symlinks inside the tree are written; anything that
// would escape dir is refused.
func untarGz(archive, dir, strip string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(bufio.NewReader(f))
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(hdr.Name, strip)
		if rel == hdr.Name || rel == "" {
			continue
		}
		target := filepath.Join(dir, rel)
		if !strings.HasPrefix(target, filepath.Clean(dir)+string(os.PathSeparator)) {
			return fmt.Errorf("deps: archive entry escapes the target: %s", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode).Perm()|0o600)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			out.Close()
		case tar.TypeSymlink:
			// The link's target is judged on a separator: a bare prefix let
			// "../../node-evil/x", from bin/ of a package unpacked in .../node, through
			// as inside .../node.
			if to := filepath.Clean(filepath.Join(filepath.Dir(target), hdr.Linkname)); filepath.IsAbs(hdr.Linkname) || (to != filepath.Clean(dir) && !strings.HasPrefix(to, filepath.Clean(dir)+string(os.PathSeparator))) {
				return fmt.Errorf("deps: symlink escapes the target: %s -> %s", hdr.Name, hdr.Linkname)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		}
	}
}

// Check asks the registry for the latest version of every package and
// records it; with AutoUpdate on, a managed copy that is behind is
// updated. It is what the loop runs and what `deps check` runs by hand.
func (m *Manager) Check(ctx context.Context, progress func(string)) []Status {
	if progress == nil {
		progress = func(string) {}
	}
	for _, p := range Packages {
		latest, err := m.latest(ctx, p)
		m.mu.Lock()
		s := m.last[p.Name]
		s.CheckedAt = time.Now()
		s.Error = ""
		if err != nil {
			s.Error = err.Error()
		} else {
			s.Latest = latest
		}
		m.last[p.Name] = s
		m.mu.Unlock()
		if err != nil {
			progress(p.Name + ": " + err.Error())
			continue
		}
		path, managed, ferr := m.Find(p.Name)
		if ferr != nil || !managed {
			continue
		}
		cur := m.installedVersion(p)
		if !strings.HasPrefix(path, m.Prefix()) {
			// The seed: its version is what it answers.
			cur = m.versionOf(path)
		}
		if newer(latest, cur) {
			progress(fmt.Sprintf("%s: %s installed, %s available", p.Name, cur, latest))
			if m.AutoUpdate != nil && m.AutoUpdate() {
				if err := m.install(ctx, p.Name, latest, progress); err != nil {
					m.emit("deps.update_failed", p.Name, "warn", err.Error(), nil)
					progress(err.Error())
				} else {
					m.emit("deps.updated", p.Name, "", fmt.Sprintf("%s updated %s → %s", p.Name, cur, latest), map[string]any{"from": cur, "to": latest})
				}
			}
		}
	}
	return m.Statuses()
}

// latest asks npm's registry for the dist-tag, without npm: one GET.
func (m *Manager) latest(ctx context.Context, p Package) (string, error) {
	switch {
	case p.Binary != nil:
		return m.binaryLatest(ctx, p.Binary)
	case p.PyPI != "":
		return m.pypiLatest(ctx, p.PyPI)
	}
	url := "https://registry.npmjs.org/-/package/" + p.NPM + "/dist-tags"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := Download.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry answered %s", resp.Status)
	}
	var tags map[string]string
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&tags); err != nil {
		return "", err
	}
	if tags["latest"] == "" {
		return "", errors.New("registry has no latest tag")
	}
	return tags["latest"], nil
}

// Run checks once shortly after start and then every Interval. One at a
// time; a check that overlaps the next is skipped.
func (m *Manager) Run(ctx context.Context) {
	first := time.NewTimer(2 * time.Minute)
	defer first.Stop()
	select {
	case <-ctx.Done():
		return
	case <-first.C:
	}
	for {
		m.tryCheck(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(m.Interval):
		}
	}
}

func (m *Manager) tryCheck(ctx context.Context) {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return
	}
	m.running = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.running = false
		m.mu.Unlock()
	}()
	// A check only runs for packages somebody has: nothing is installed
	// behind the operator's back, only kept current.
	any := false
	for _, p := range Packages {
		if _, managed, err := m.Find(p.Name); err == nil && managed {
			any = true
		}
	}
	if !any {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	m.Check(cctx, nil)
}

func (m *Manager) emit(typ, subject, level, msg string, data map[string]any) {
	if m.Bus == nil {
		return
	}
	m.Bus.Emit(events.Event{Type: typ, Subject: subject, Actor: "deps", Level: level, Message: msg, Data: data})
}

// Names lists the managed commands, sorted.
func Names() []string {
	out := make([]string, 0, len(Packages))
	for _, p := range Packages {
		out = append(out, p.Name)
	}
	sort.Strings(out)
	return out
}

func run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	return cmd.CombinedOutput()
}
