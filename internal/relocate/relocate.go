// Package relocate moves a site from this node to another of its network,
// and receives one: the site comes back up on the other machine with its
// folder, its HOME, its conversations, its cost and its history, and its
// names point there.
//
// The stop is short and said: a first copy goes while the site serves,
// then the site stops, and a second copy carries only what changed since
// the first (and the list of what was there, so what was deleted here is
// deleted there). On the far side the site is put in place only when the
// last byte arrived; until then it is a staging folder nobody serves, and
// throwing it away is deleting a folder. The order of the last steps is
// what keeps a mistake from losing anything:
//
//   - the source is stopped, never deleted, until the target answers
//     healthy: a target that does not come up is removed there and the
//     source started again here, as it was;
//   - the names move only after: the target publishes them over what
//     pointed here, and only then does this node take its own route down;
//   - the source folder is archived here, not deleted, so going back is a
//     restore.
package relocate

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/archive"
	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/pubports"
	"github.com/dcimorra/the-middleware/internal/runtime"
	"github.com/dcimorra/the-middleware/internal/tasks"
)

// Parts are what moves with a site: its folder, and what the house keeps
// of it under data/ by its name. The same list as a rename moves, plus the
// deploy record; logs stay, they are this machine's.
// MoveParts are Parts without the site's dependencies, which the target
// installs again for its own machine (Mover.Install).
func MoveParts(root, domain string) []archive.Part {
	parts := Parts(root, domain)
	parts[0].Skip = SkipReinstalled
	return parts
}

// LeftBehind is every folder of a site whose dependencies are left behind
// for the target to install (Reinstalled), relative to the site, "." for
// its root: what the target installs, folder by folder. A site whose
// package.json is in frontend/ (litterae.cc) arrived with its root
// installed, which has none, and frontend/ without node_modules: "fork/exec
// frontend/node_modules/.bin/vite: no such file or directory".
//
// skip is the site part's Skip: nothing is installed inside what the move
// leaves out (move.exclude), and nil is Reinstalled alone.
func LeftBehind(siteDir string, skip func(string, bool) bool) []string {
	var out []string
	_ = filepath.WalkDir(siteDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if d.Name() == ".git" {
			return filepath.SkipDir
		}
		if skip != nil && p != siteDir && !Reinstalled(p) && skip(p, true) {
			return filepath.SkipDir
		}
		if Reinstalled(p) {
			rel, _ := filepath.Rel(siteDir, filepath.Dir(p))
			out = append(out, filepath.ToSlash(rel))
			return filepath.SkipDir
		}
		if d.Name() == "node_modules" {
			return filepath.SkipDir // a build's own: travels, nothing to look for inside
		}
		return nil
	})
	sort.Strings(out) // "." first
	return out
}

// Excluding returns parts with the site's folder (the part named "site")
// also leaving out what m's move.exclude names: the one place that rule is
// applied, for the copy, the plan a move shows first and the installs.
func Excluding(parts []archive.Part, siteDir string, m *manifest.Manifest) []archive.Part {
	if m == nil || m.Move == nil || len(m.Move.Exclude) == 0 {
		return parts
	}
	out := append([]archive.Part(nil), parts...)
	for i := range out {
		if out[i].Name != "site" {
			continue
		}
		prev := out[i].Skip
		out[i].Skip = func(p string, dir bool) bool {
			if prev != nil && prev(p, dir) {
				return true
			}
			rel, err := filepath.Rel(siteDir, p)
			return err == nil && m.Excluded(filepath.ToSlash(rel))
		}
	}
	return out
}

// siteSkip is the Skip of the part that is the site's folder.
func siteSkip(parts []archive.Part) func(string, bool) bool {
	for _, p := range parts {
		if p.Name == "site" {
			return p.Skip
		}
	}
	return nil
}

// SkipReinstalled is Reinstalled as a part's Skip.
func SkipReinstalled(path string, dir bool) bool { return dir && Reinstalled(path) }

// Reinstalled says a folder is a package's dependencies, which the target
// installs again: a node_modules beside a package.json that declares some.
// Not any folder of that name: Next's Turbopack keeps .next/node_modules
// with links named like "@prisma/client-2c3a283f" that the build resolves
// by, and nothing reinstalls them. The first moves left every node_modules
// behind and the moved site answered its front page and failed every page
// that used the database with "Cannot find module '@prisma/client-…'".
// A package.json beside it was the fix, until Next started writing one
// into .next too, {"type": "commonjs"} and nothing else: the links were
// left behind again, and the install there had nothing to put back. So the
// package.json must declare dependencies, or have a lockfile beside it.
func Reinstalled(dir string) bool {
	if filepath.Base(dir) != "node_modules" {
		return false
	}
	pkgDir := filepath.Dir(dir)
	for _, lock := range []string{"package-lock.json", "npm-shrinkwrap.json", "pnpm-lock.yaml", "yarn.lock"} {
		if fsx.Exists(filepath.Join(pkgDir, lock)) && fsx.Exists(filepath.Join(pkgDir, "package.json")) {
			return true
		}
	}
	raw, err := os.ReadFile(filepath.Join(pkgDir, "package.json"))
	if err != nil {
		return false
	}
	var pkg map[string]json.RawMessage
	if json.Unmarshal(raw, &pkg) != nil {
		// One that does not read is a package all the same: its install
		// fails there and says why, instead of a folder carried in silence.
		return true
	}
	for _, k := range []string{"dependencies", "devDependencies", "optionalDependencies", "peerDependencies"} {
		var deps map[string]any
		if json.Unmarshal(pkg[k], &deps) == nil && len(deps) > 0 {
			return true
		}
	}
	return false
}

func Parts(root, domain string) []archive.Part {
	data := filepath.Join(root, "data")
	return []archive.Part{
		{Name: "site", Path: filepath.Join(root, "sites", domain)},
		{Name: "data/homes/" + domain, Path: filepath.Join(data, "homes", domain)},
		{Name: "data/chat/" + domain + ".json", Path: filepath.Join(data, "chat", domain+".json")},
		{Name: "data/chat/" + domain, Path: filepath.Join(data, "chat", domain)},
		{Name: "data/agents/cost/" + domain + ".json", Path: filepath.Join(data, "agents", "cost", domain+".json")},
		{Name: "data/history/" + domain, Path: filepath.Join(data, "history", domain)},
		{Name: "data/deploys/" + domain + ".json", Path: filepath.Join(data, "deploys", domain+".json")},
		// The owner's approval of its public ports: given once for the
		// site, not for the node it happened to be on. Without it the
		// site arrived with its relay waiting for a yes nobody knew was
		// being asked again.
		{Name: "data/" + pubports.Dir + "/" + domain + ".json", Path: filepath.Join(data, pubports.Dir, domain+".json")},
	}
}

// Errors the receiving side answers with.
var (
	ErrBadName    = errors.New("not a site's name")
	ErrExists     = errors.New("a site with that name already exists here")
	ErrNoStaging  = errors.New("nothing was received for that site")
	ErrNotAFolder = errors.New("the site's folder is a link to somewhere else; move what it points at instead")
)

// Receiver is the target's side.
type Receiver struct {
	Root string
	// Docker brings up a moving worker's container (Migrate) and removes
	// the volumes of a move that is undone; nil is the docker in PATH.
	Docker *runtime.Docker
}

func (r Receiver) staging(domain string) string {
	return filepath.Join(r.Root, "data", "relocate", domain)
}

func (r Receiver) check(domain string) error {
	if !manifest.ValidName(domain) {
		return ErrBadName
	}
	if _, err := os.Lstat(filepath.Join(r.Root, "sites", domain)); err == nil {
		return ErrExists
	}
	return nil
}

// Receive takes a whole bundle into the staging folder, replacing what
// an earlier attempt left there.
func (r Receiver) Receive(ctx context.Context, domain string, body io.Reader) (archive.Report, error) {
	if err := r.check(domain); err != nil {
		return archive.Report{}, err
	}
	dir := r.staging(domain)
	if err := os.RemoveAll(dir); err != nil {
		return archive.Report{}, err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return archive.Report{}, err
	}
	return archive.UnpackFrom(ctx, body, dir)
}

// ReceiveDelta lays a partial bundle over the staging folder: what it
// carries replaces what was there, and whatever its index does not name
// is removed.
func (r Receiver) ReceiveDelta(ctx context.Context, domain string, body io.Reader) (archive.Report, error) {
	if err := r.check(domain); err != nil {
		return archive.Report{}, err
	}
	dir := r.staging(domain)
	if !fsx.Exists(dir) {
		return archive.Report{}, ErrNoStaging
	}
	delta := dir + ".delta"
	_ = os.RemoveAll(delta)
	rep, err := archive.UnpackFrom(ctx, body, delta)
	if err != nil {
		return rep, err
	}
	defer os.RemoveAll(delta)
	raw, err := os.ReadFile(filepath.Join(delta, archive.IndexName))
	if err != nil {
		return rep, errors.New("the last copy carries no index: it would leave deleted files behind")
	}
	keep := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		// Every folder on the way to a name is kept too: the index lists
		// the parts ("data/homes/x"), not the folders they sit in.
		for line != "" && line != "." && !keep[line] {
			keep[line] = true
			line = filepath.ToSlash(filepath.Dir(line))
		}
	}
	// What arrived replaces what was there, deepest last so a folder
	// exists before what goes in it.
	err = filepath.WalkDir(delta, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(delta, p)
		if rel == "." || rel == archive.IndexName {
			return nil
		}
		dst := filepath.Join(dir, rel)
		if d.IsDir() {
			if info, err := os.Lstat(dst); err == nil && !info.IsDir() {
				_ = os.RemoveAll(dst)
			}
			return os.MkdirAll(dst, 0o755)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		_ = os.RemoveAll(dst)
		return os.Rename(p, dst)
	})
	if err != nil {
		return rep, err
	}
	// Whatever the index does not name was deleted at the source.
	var gone []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "." {
			return nil
		}
		if d.IsDir() && Reinstalled(p) {
			// Never named by the index because never sent: the source leaves
			// it behind and the target installs it (MoveParts, Prepare)
			// between the first copy and this one. Removing it here, as the
			// first version did, took away what was just installed, and the
			// moved site failed at its start with "tailwindcss: not found"
			// on any machine, however big.
			return filepath.SkipDir
		}
		if !keep[filepath.ToSlash(rel)] {
			gone = append(gone, p)
			if d.IsDir() {
				return filepath.SkipDir
			}
		}
		return nil
	})
	for _, p := range gone {
		_ = os.RemoveAll(p)
	}
	return rep, nil
}

// Commit puts the received site in place: its data first, its folder
// last, because the engine serves a folder the moment it sees one with a
// manifest. manifestData, when not empty, is the manifest the site had
// before it was stopped for the move.
func (r Receiver) Commit(domain string, manifestData []byte, taskFiles map[string][]byte) error {
	if err := r.check(domain); err != nil {
		return err
	}
	// Every task is checked before anything is placed: one that does not
	// read here, or whose name is taken, refuses the commit whole rather
	// than leaving the site here with part of its jobs.
	for _, name := range SortedNames(taskFiles) {
		if err := r.CheckTask(domain, name, taskFiles[name]); err != nil {
			return err
		}
	}
	dir := r.staging(domain)
	site := filepath.Join(dir, "site")
	if !fsx.Exists(site) {
		return ErrNoStaging
	}
	if len(manifestData) > 0 {
		if _, err := manifest.Parse(manifestData); err != nil {
			return fmt.Errorf("the manifest sent with the move: %w", err)
		}
		if err := fsx.WriteAtomic(filepath.Join(site, manifest.FileName), manifestData, 0o644); err != nil {
			return err
		}
	}
	// The parts under data/, by the same list they were packed with. What
	// each one set aside is written down (placed), so a site that does not
	// come up is taken out exactly: what arrived removed, what was there
	// before put back (Unplace).
	placed := placedRecord{Domain: domain, Aside: map[string]string{}}
	for _, part := range Parts(r.Root, domain) {
		if !strings.HasPrefix(part.Name, "data/") {
			continue
		}
		src := filepath.Join(dir, filepath.FromSlash(part.Name))
		if _, err := os.Lstat(src); err != nil {
			continue
		}
		dst := filepath.Join(r.Root, filepath.FromSlash(part.Name))
		if _, err := os.Lstat(dst); err == nil {
			// Something of an earlier life of this name here: set aside,
			// never mixed with or lost under what arrived.
			aside := dst + ".before-" + time.Now().UTC().Format("20060102T150405Z")
			if os.Rename(dst, aside) == nil {
				placed.Aside[part.Name] = aside
			}
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			_ = fsx.WriteJSON(r.placedPath(domain), placed, 0o644)
			return err
		}
		if err := os.Rename(src, dst); err != nil {
			_ = fsx.WriteJSON(r.placedPath(domain), placed, 0o644)
			return err
		}
		placed.Parts = append(placed.Parts, part.Name)
	}
	// The tasks are placed like the parts under data/: written down, so a
	// site that does not come up takes them out with it.
	for _, name := range SortedNames(taskFiles) {
		if err := fsx.WriteAtomic(tasks.Path(r.Root, name), taskFiles[name], 0o644); err != nil {
			_ = fsx.WriteJSON(r.placedPath(domain), placed, 0o644)
			return err
		}
		placed.Parts = append(placed.Parts, "tasks/"+name+".json")
	}
	// The volumes its migration filled, taken out with it if it does not
	// come up.
	if raw, err := os.ReadFile(r.volumesFile(domain)); err == nil {
		_ = json.Unmarshal(raw, &placed.Volumes)
	}
	if err := os.MkdirAll(filepath.Join(r.Root, "sites"), 0o755); err != nil {
		return err
	}
	if err := fsx.WriteJSON(r.placedPath(domain), placed, 0o644); err != nil {
		return err
	}
	if err := os.Rename(site, filepath.Join(r.Root, "sites", domain)); err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// CheckTask says a task of domain's can be placed here: it reads, it is
// the site's, and its name is free.
func (r Receiver) CheckTask(domain, name string, data []byte) error {
	if !tasks.ValidName(name) {
		return fmt.Errorf("task %q: not a task name", name)
	}
	if fsx.Exists(tasks.Path(r.Root, name)) {
		return fmt.Errorf("task %s: one with that name is already here", name)
	}
	if data == nil {
		return nil
	}
	t, err := tasks.Parse(data)
	if err != nil {
		return fmt.Errorf("task %s: %w", name, err)
	}
	if t.Site != domain {
		return fmt.Errorf("task %s: it runs in %q, not in %s", name, t.Site, domain)
	}
	return nil
}

// placedRecord is what Commit put under data/ for a site, and what it set
// aside to make room, until the move ends.
type placedRecord struct {
	Domain string            `json:"domain"`
	Parts  []string          `json:"parts"`
	Aside  map[string]string `json:"aside,omitempty"`
	// Volumes are the docker volumes a migration filled for it.
	Volumes []string `json:"volumes,omitempty"`
}

func (r Receiver) placedPath(domain string) string {
	return filepath.Join(r.Root, "data", "relocate", "placed", domain+".json")
}

// Unplace takes out what Commit put under data/ for a site that did not
// come up, and puts back what it set aside: the target as it was before
// the move. The site's folder is the sites manager's to delete (it stops
// the engine's instance first). Before this, a failed move left the site's
// HOME, conversations and history behind, and every retry set the last
// one aside as ".before-…", a full copy of a HOME each time.
func (r Receiver) Unplace(domain string) error {
	if !manifest.ValidName(domain) {
		return ErrBadName
	}
	raw, err := os.ReadFile(r.placedPath(domain))
	if err != nil {
		return nil // nothing placed, or already taken out
	}
	// Never under a site that is here: what it placed is that site's HOME
	// and conversations now. The folder goes first (Remove), then this.
	if _, err := os.Lstat(filepath.Join(r.Root, "sites", domain)); err == nil {
		return fmt.Errorf("%s is here: its data is not taken out while its folder is", domain)
	}
	var rec placedRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return fmt.Errorf("the record of what was placed: %w", err)
	}
	var failed []string
	for _, name := range rec.Parts {
		dst := filepath.Join(r.Root, filepath.FromSlash(name))
		if err := os.RemoveAll(dst); err != nil {
			failed = append(failed, name+": "+err.Error())
			continue
		}
		if aside := rec.Aside[name]; aside != "" {
			if err := os.Rename(aside, dst); err != nil {
				failed = append(failed, "putting back "+aside+": "+err.Error())
			}
		}
	}
	if err := r.removeVolumeList(rec.Volumes); err != nil {
		failed = append(failed, err.Error())
	}
	if len(failed) > 0 {
		return errors.New(strings.Join(failed, "; "))
	}
	return os.Remove(r.placedPath(domain))
}

// Placed ends a move that worked: the record goes, what was set aside
// stays (it is an earlier life of the name, the operator's to keep or
// clean as a leftover).
func (r Receiver) Placed(domain string) error {
	if !manifest.ValidName(domain) {
		return ErrBadName
	}
	if err := os.Remove(r.placedPath(domain)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// StagingSite is where a received site's folder waits for its commit.
func (r Receiver) StagingSite(domain string) string {
	return filepath.Join(r.staging(domain), "site")
}

// Discard throws away what was received for domain.
func (r Receiver) Discard(domain string) error {
	if !manifest.ValidName(domain) {
		return ErrBadName
	}
	dir := r.staging(domain)
	_ = os.RemoveAll(dir + ".delta")
	// Before the folder that lists them goes: a migration that failed
	// before the commit leaves its volumes only there.
	if err := r.removeVolumes(r.volumesFile(domain)); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	// And what a commit put under data/, when it got that far: a commit
	// that failed halfway, or a site removed because it did not come up.
	return r.Unplace(domain)
}
