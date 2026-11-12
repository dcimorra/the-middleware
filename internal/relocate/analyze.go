package relocate

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dcimorra/the-middleware/internal/archive"
	"github.com/dcimorra/the-middleware/internal/manifest"
)

// What a move will do, said before it does it: what is copied, where the
// target installs dependencies and with what, which Node, which names
// follow, and what will not travel well. The failures of the first moves
// were each found halfway (a node_modules left behind in frontend/, links
// a build resolved by, a body over a limit); seen first, most of them are a
// line to read rather than a site down for three minutes. Nothing here
// depends on a folder's name: dependencies are found as node_modules beside
// a package.json wherever they are, and what will not travel as a link
// that leaves the site's folder, whatever it is for (a Python .venv
// pointing at this machine's interpreter is one).

// Install is one folder the target installs dependencies in.
type Install struct {
	Dir     string   `json:"dir"` // relative to the site, "." for its root
	Command []string `json:"command"`
	// Lockfile is false when there is none: the target resolves versions
	// anew, which may not be the ones that ran here.
	Lockfile bool `json:"lockfile"`
}

// OutsideLink is a link in the site that points out of its folder: on
// the target it points at whatever that path is there, or nothing.
type OutsideLink struct {
	Path   string `json:"path"`
	Target string `json:"target"`
}

// Analysis is a move's plan for one site.
type Analysis struct {
	Bytes    int64     `json:"bytes"`
	Files    int       `json:"files"`
	Installs []Install `json:"installs"`
	Node     string    `json:"node,omitempty"`
	Docker   bool      `json:"docker,omitempty"`
	// Ports are the public ports its services answer on, which the
	// target must have free and reachable.
	Ports    []string      `json:"ports,omitempty"`
	Names    []string      `json:"names,omitempty"`
	Links    []OutsideLink `json:"outside_links,omitempty"`
	Warnings []string      `json:"warnings"`
}

// InstallCommand is how a folder's dependencies are installed: the
// lockfile says the tool, dev dependencies always (a start that builds
// needs them, and npm leaves them out where the machine says production).
func InstallCommand(dir string) (cmd []string, lockfile bool) {
	switch {
	case fileIn(dir, "package-lock.json"):
		return []string{"npm", "ci", "--include=dev", "--no-audit", "--no-fund"}, true
	case fileIn(dir, "pnpm-lock.yaml"):
		return []string{"corepack", "pnpm", "install", "--frozen-lockfile", "--prod=false"}, true
	case fileIn(dir, "yarn.lock"):
		return []string{"corepack", "yarn", "install", "--frozen-lockfile", "--production=false"}, true
	}
	return []string{"npm", "install", "--include=dev", "--no-audit", "--no-fund"}, false
}

// maxLinks bounds what is listed; the count says the rest.
const maxLinks = 20

// Analyze reads a site's folder and the parts that go with it. install
// says the target installs dependencies (they are left behind); names are
// the ones that follow it.
func Analyze(siteDir string, parts []archive.Part, install bool, names []string) Analysis {
	a := Analysis{Installs: []Install{}, Warnings: []string{}, Names: names}
	for _, p := range parts {
		_ = filepath.WalkDir(p.Path, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if path != p.Path && p.Skip != nil && p.Skip(path, d.IsDir()) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.Type().IsRegular() {
				if info, err := d.Info(); err == nil {
					a.Bytes += info.Size()
					a.Files++
				}
			}
			return nil
		})
	}
	if install {
		for _, dir := range LeftBehind(siteDir, siteSkip(parts)) {
			cmd, lock := InstallCommand(filepath.Join(siteDir, filepath.FromSlash(dir)))
			a.Installs = append(a.Installs, Install{Dir: dir, Command: cmd, Lockfile: lock})
			if !lock {
				a.Warnings = append(a.Warnings, fmt.Sprintf("%s has no lockfile: the target resolves its versions anew, and they may not be the ones that ran here", dir))
			}
		}
	}
	if mf, err := manifest.Load(siteDir); err == nil {
		a.Node = mf.Node
		a.Docker = UsesDocker(mf)
		for _, p := range mf.PublicPorts() {
			a.Ports = append(a.Ports, p.PublicPort.String())
		}
		if len(a.Ports) > 0 {
			a.Warnings = append(a.Warnings, fmt.Sprintf("its services answer on public ports (%s): the house opens them in the target's own firewall, but a firewall of the provider's in front of that node (a security list, a cloud firewall) is yours to open there", strings.Join(a.Ports, ", ")))
		}
		a.Warnings = append(a.Warnings, Unmanaged(siteDir, mf, nil)...)
	}
	real, err := filepath.EvalSymlinks(siteDir)
	if err != nil {
		real = siteDir
	}
	extra := 0
	_ = filepath.WalkDir(siteDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && (d.Name() == ".git" || Reinstalled(path)) {
			return filepath.SkipDir
		}
		if d.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		target, err := os.Readlink(path)
		if err != nil {
			return nil
		}
		abs := target
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(filepath.Dir(path), target)
		}
		abs = filepath.Clean(abs)
		if abs == real || strings.HasPrefix(abs, real+string(filepath.Separator)) || abs == siteDir || strings.HasPrefix(abs, filepath.Clean(siteDir)+string(filepath.Separator)) {
			return nil
		}
		if len(a.Links) >= maxLinks {
			extra++
			return nil
		}
		rel, _ := filepath.Rel(siteDir, path)
		a.Links = append(a.Links, OutsideLink{Path: filepath.ToSlash(rel), Target: target})
		return nil
	})
	if n := len(a.Links) + extra; n > 0 {
		a.Warnings = append(a.Warnings, fmt.Sprintf("%d links point out of the site's folder: on the target they point at whatever those paths are there, or nothing (a Python virtualenv is made on the target with its own interpreter)", n))
	}
	sort.Slice(a.Installs, func(i, j int) bool { return a.Installs[i].Dir < a.Installs[j].Dir })
	return a
}
