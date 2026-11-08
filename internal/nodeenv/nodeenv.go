// Package nodeenv finds a Node for a site, and installs one when the
// machine has none or not the one the site asks for: through nvm, under
// the installation's own modules/nvm, on the fly.
//
// The rule that shapes it is that the version is never what stops a site.
// What the site asks for (.nvmrc, .node-version, package.json's
// engines.node) is tried first; if that cannot be had, then the current
// LTS, then any Node nvm already has, then the one the house keeps for the
// agent CLIs, then whatever node the machine has. A site that wanted 18
// and runs on 22 is said in its log; a site that does not start because
// 18 could not be downloaded is the failure this package exists to avoid.
// Only with no Node anywhere at all does the start fail, and it says why.
//
// A site moved to a node that never ran Node, or a site taken out of its
// container, is the case it was written for: its command says `node`, and
// the machine had none.
package nodeenv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/fsx"
)

// Manager hands out Node bin folders, installing through nvm as needed.
type Manager struct {
	// Root is the installation's root: nvm lives in modules/nvm.
	Root string
	// Note hears what was chosen and why, for the site's log or an event.
	Note func(siteDir, msg string)

	// LookPath, Run and fetchNVM are replaced by tests.
	LookPath func(string) (string, error)
	Run      func(ctx context.Context, env []string, name string, args ...string) ([]byte, error)
	FetchNVM func(ctx context.Context, dir string) error

	mu    sync.Mutex
	cache map[string]string
	locks map[string]*sync.Mutex
}

// NVMDir is where nvm and the Nodes it installs live.
func (m *Manager) NVMDir() string { return filepath.Join(m.Root, "modules", "nvm") }

func (m *Manager) lookPath(name string) (string, error) {
	if m.LookPath != nil {
		return m.LookPath(name)
	}
	return exec.LookPath(name)
}

func (m *Manager) run(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	if m.Run != nil {
		return m.Run(ctx, env, name, args...)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(errb.String())
		if len(msg) > 400 {
			msg = msg[len(msg)-400:]
		}
		return out.Bytes(), fmt.Errorf("%v: %s", err, msg)
	}
	return out.Bytes(), nil
}

func (m *Manager) note(siteDir, msg string) {
	if m.Note != nil {
		m.Note(siteDir, msg)
	}
}

// Needs says whether a command in a site wants Node: its program is one
// of Node's, a shell line mentions one, or the site has a package.json.
func Needs(siteDir string, argv []string) bool {
	if len(argv) > 0 {
		base := filepath.Base(argv[0])
		if nodeTools[base] {
			return true
		}
		if base == "sh" || base == "bash" {
			for _, a := range argv[1:] {
				for t := range nodeTools {
					if regexp.MustCompile(`(^|[\s;&|(])` + regexp.QuoteMeta(t) + `(\s|$)`).MatchString(a) {
						return true
					}
				}
			}
		}
	}
	if siteDir != "" {
		if _, err := os.Stat(filepath.Join(siteDir, "package.json")); err == nil {
			return true
		}
	}
	return false
}

var nodeTools = map[string]bool{"node": true, "npm": true, "npx": true, "pnpm": true, "yarn": true, "corepack": true}

// Wanted is the Node major a site asks for, "" for none: its manifest's
// node, then .nvmrc, then
// .node-version, then package.json's engines.node. A range gives its
// first number ("^20.1" and ">=20" are 20); "lts/*" and "lts" are "lts".
func Wanted(siteDir string) string {
	// The manifest's "node" first: what the house wrote (a move) or the
	// operator did, over what the files ask.
	if raw, err := os.ReadFile(filepath.Join(siteDir, "site.json")); err == nil {
		var m struct {
			Node string `json:"node"`
		}
		if json.Unmarshal(raw, &m) == nil {
			if v := normalize(m.Node); v != "" {
				return v
			}
		}
	}
	for _, f := range []string{".nvmrc", ".node-version"} {
		if raw, err := os.ReadFile(filepath.Join(siteDir, f)); err == nil {
			if v := normalize(string(raw)); v != "" {
				return v
			}
		}
	}
	raw, err := os.ReadFile(filepath.Join(siteDir, "package.json"))
	if err != nil {
		return ""
	}
	var pkg struct {
		Engines struct {
			Node string `json:"node"`
		} `json:"engines"`
	}
	if json.Unmarshal(raw, &pkg) != nil {
		return ""
	}
	return normalize(pkg.Engines.Node)
}

var firstNumber = regexp.MustCompile(`\d+`)

func normalize(v string) string {
	v = strings.ToLower(strings.TrimSpace(strings.SplitN(v, "\n", 2)[0]))
	if v == "" {
		return ""
	}
	if strings.HasPrefix(v, "lts") || v == "node" || v == "latest" || v == "stable" || v == "*" {
		return "lts"
	}
	return firstNumber.FindString(v)
}

// major is the major of a `node --version` answer ("v22.3.0" → 22).
func major(version string) int {
	n, _ := strconv.Atoi(firstNumber.FindString(version))
	return n
}

// For returns the folder whose node runs siteDir: empty when the
// machine's own will do (or is the only one there is), never an error the
// start has to stop on. The first call for a version may download it.
func (m *Manager) For(ctx context.Context, siteDir string) string {
	want := Wanted(siteDir)
	sys, _ := m.lookPath("node")
	if sys != "" {
		if want == "" || want == "lts" {
			return ""
		}
		if out, err := m.run(ctx, os.Environ(), sys, "--version"); err == nil && strconv.Itoa(major(string(out))) == want {
			return ""
		}
	}
	key := want
	if key == "" {
		key = "lts"
	}
	lock := m.lockFor(key)
	lock.Lock()
	defer lock.Unlock()
	m.mu.Lock()
	if dir, ok := m.cache[key]; ok {
		m.mu.Unlock()
		return dir
	}
	m.mu.Unlock()
	dir, why := m.resolve(ctx, key, sys)
	if why != "" {
		m.note(siteDir, why)
	}
	m.mu.Lock()
	if m.cache == nil {
		m.cache = map[string]string{}
	}
	m.cache[key] = dir
	m.mu.Unlock()
	return dir
}

func (m *Manager) lockFor(key string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.locks == nil {
		m.locks = map[string]*sync.Mutex{}
	}
	if m.locks[key] == nil {
		m.locks[key] = &sync.Mutex{}
	}
	return m.locks[key]
}

// resolve is the fall-through: the version asked for, the LTS, any Node
// nvm has, the house's own, the machine's. It says what it settled on
// when that is not what was asked.
func (m *Manager) resolve(ctx context.Context, key, sys string) (string, string) {
	var tried []string
	if err := m.ensureNVM(ctx); err != nil {
		tried = append(tried, "nvm could not be installed ("+err.Error()+")")
	} else {
		for _, v := range []string{key, "lts"} {
			if v == "lts" && key == "lts" && len(tried) > 0 {
				break
			}
			dir, err := m.nvmInstall(ctx, v)
			if err == nil {
				if v != key {
					return dir, fmt.Sprintf("Node %s could not be installed (%s); running on the current LTS instead", key, strings.Join(tried, "; "))
				}
				return dir, fmt.Sprintf("Node %s installed with nvm in %s", label(key), dir)
			}
			tried = append(tried, label(v)+": "+err.Error())
		}
		if dir := newestIn(filepath.Join(m.NVMDir(), "versions", "node")); dir != "" {
			return dir, fmt.Sprintf("Node %s could not be installed (%s); running on %s, the newest nvm already had", label(key), strings.Join(tried, "; "), filepath.Base(filepath.Dir(dir)))
		}
	}
	if house := filepath.Join(m.Root, "modules", "node-runtime", "bin"); fsx.IsFile(filepath.Join(house, "node")) {
		return house, fmt.Sprintf("Node %s could not be installed (%s); running on the house's own Node", label(key), strings.Join(tried, "; "))
	}
	if sys != "" {
		return "", fmt.Sprintf("Node %s could not be installed (%s); running on the machine's own node", label(key), strings.Join(tried, "; "))
	}
	return "", fmt.Sprintf("there is no Node on this machine and none could be installed (%s)", strings.Join(tried, "; "))
}

func label(v string) string {
	if v == "lts" {
		return "LTS"
	}
	return v
}

// nvmInstall installs (or finds) a version and returns its bin folder.
func (m *Manager) nvmInstall(ctx context.Context, v string) (string, error) {
	arg := v
	if v == "lts" {
		arg = "--lts"
	}
	which := v
	if v == "lts" {
		which = "lts/*"
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	script := `. "$NVM_DIR/nvm.sh" && nvm install ` + arg + ` >&2 && nvm which "` + which + `"`
	env := []string{"NVM_DIR=" + m.NVMDir(), "HOME=" + m.NVMDir(), "PATH=/usr/local/bin:/usr/bin:/bin"}
	for _, k := range []string{"HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "no_proxy"} {
		if val := os.Getenv(k); val != "" {
			env = append(env, k+"="+val)
		}
	}
	out, err := m.run(ctx, env, "bash", "-c", script)
	if err != nil {
		return "", err
	}
	node := strings.TrimSpace(string(out))
	if i := strings.LastIndexByte(node, '\n'); i >= 0 {
		node = node[i+1:]
	}
	if !filepath.IsAbs(node) || !fsx.IsFile(node) {
		return "", fmt.Errorf("nvm said %q, not a node binary", node)
	}
	return filepath.Dir(node), nil
}

// ensureNVM installs nvm under modules/nvm the first time: the latest
// release's tag, cloned with git (which the installer puts on every
// machine). nvm.sh is a script sourced by bash; it is not a daemon and
// touches nothing outside its folder with NVM_DIR set there.
func (m *Manager) ensureNVM(ctx context.Context) error {
	if fsx.IsFile(filepath.Join(m.NVMDir(), "nvm.sh")) {
		return nil
	}
	if m.FetchNVM != nil {
		return m.FetchNVM(ctx, m.NVMDir())
	}
	if _, err := m.lookPath("bash"); err != nil {
		return errors.New("bash is not installed")
	}
	if _, err := m.lookPath("git"); err != nil {
		return errors.New("git is not installed")
	}
	tag := latestNVM(ctx)
	if err := os.MkdirAll(filepath.Dir(m.NVMDir()), 0o755); err != nil {
		return err
	}
	tmp := m.NVMDir() + ".partial"
	_ = os.RemoveAll(tmp)
	args := []string{"clone", "--depth", "1"}
	if tag != "" {
		args = append(args, "--branch", tag)
	}
	args = append(args, "https://github.com/nvm-sh/nvm.git", tmp)
	cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if _, err := m.run(cctx, os.Environ(), "git", args...); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	return os.Rename(tmp, m.NVMDir())
}

// latestNVM is the tag of nvm's latest release, or "" (the default branch)
// when GitHub cannot be asked.
func latestNVM(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/nvm-sh/nvm/releases/latest", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer res.Body.Close()
	var r struct {
		Tag string `json:"tag_name"`
	}
	if res.StatusCode != http.StatusOK || json.NewDecoder(res.Body).Decode(&r) != nil {
		return ""
	}
	if !regexp.MustCompile(`^v\d+\.\d+\.\d+$`).MatchString(r.Tag) {
		return ""
	}
	return r.Tag
}

// newestIn is the bin folder of the highest version under versions/node.
func newestIn(dir string) string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var vs []string
	for _, e := range ents {
		if fsx.IsFile(filepath.Join(dir, e.Name(), "bin", "node")) {
			vs = append(vs, e.Name())
		}
	}
	if len(vs) == 0 {
		return ""
	}
	sort.Slice(vs, func(i, j int) bool { return versionLess(vs[i], vs[j]) })
	return filepath.Join(dir, vs[len(vs)-1], "bin")
}

func versionLess(a, b string) bool {
	pa, pb := firstNumber.FindAllString(a, 3), firstNumber.FindAllString(b, 3)
	for i := 0; i < 3; i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x < y
		}
	}
	return false
}

// Prepare installs nvm under the installation and, with it, the Node v
// ("lts" or a major): what the installer leaves on every machine, so the
// first site that asks for a version does not wait for nvm itself. It
// says whether anything was installed.
func (m *Manager) Prepare(ctx context.Context, v string) (bool, string, error) {
	had := fsx.IsFile(filepath.Join(m.NVMDir(), "nvm.sh"))
	if err := m.ensureNVM(ctx); err != nil {
		return false, "", fmt.Errorf("nvm: %w", err)
	}
	before := newestIn(filepath.Join(m.NVMDir(), "versions", "node"))
	dir, err := m.nvmInstall(ctx, v)
	if err != nil {
		return !had, "", fmt.Errorf("Node %s: %w", label(v), err)
	}
	changed := !had || before == "" || newestIn(filepath.Join(m.NVMDir(), "versions", "node")) != before
	return changed, "nvm in " + m.NVMDir() + ", Node " + label(v) + " in " + dir, nil
}
