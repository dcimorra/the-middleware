package sites

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/access"
	"github.com/dcimorra/the-middleware/internal/archive"
	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/manifest"
)

// A test copy ("staging") is a second site made from a production one,
// under its own name, that can later be promoted back over it. Both
// directions are the same operation, a copy of one folder into another
// with the target's manifest kept, and the difference is what else is
// kept: a new copy takes production's data with it (testing against the
// real thing is the point), a promotion takes only the code and leaves
// production's data where it is (Staging.KeepGlobs).
//
// A promotion never edits production's folder in place. The new folder
// is built beside it, production's kept paths are hard-linked into it
// (the same files, so a database the running process is writing keeps
// every write, before and after), and the two folders are exchanged in
// one rename. The folder that served until then is kept, and putting it
// back is the same promotion run the other way: the undo.

// StagingResult is what making or promoting a copy did.
type StagingResult struct {
	Site Site `json:"site"`
	// Password is the copy's own password, when the house gave it one
	// (production had none): shown once, stored only as a hash.
	Password string `json:"password,omitempty"`
	// Notes are what the copy did not carry, for whoever decides
	// whether it is a test of the real thing.
	Notes []string `json:"notes,omitempty"`
}

// StagingState is a site's side of the pair.
type StagingState struct {
	// Of is the production site, when this one is a copy.
	Of string `json:"of,omitempty"`
	// Copies are this site's test copies, when it is production.
	Copies []string `json:"copies,omitempty"`
	// Keep is what a promotion leaves in production.
	Keep []string `json:"keep,omitempty"`
	// Before is when the folder an undo would put back was set aside;
	// zero when there is none.
	Before time.Time `json:"before,omitempty"`
}

// ErrNotStaging says the site is not a test copy.
var ErrNotStaging = errors.New("this site is not a test copy of another")

// StagingName is the name a copy gets when none is given.
func StagingName(domain string) string { return "staging." + domain }

func (m *Manager) stagingDir(domain string) string {
	return filepath.Join(m.Root, "data", "staging", domain)
}

// beforeDir is the folder a promotion set aside, which an undo puts back.
func (m *Manager) beforeDir(domain string) string {
	return filepath.Join(m.stagingDir(domain), "before")
}

// Staging is domain's side of the pair.
func (m *Manager) Staging(domain string) (StagingState, error) {
	dir, err := m.managedDir(domain)
	if err != nil {
		return StagingState{}, err
	}
	var st StagingState
	if mf, err := manifest.Load(dir); err == nil && mf.Staging != nil {
		st.Of = mf.Staging.Of
		st.Keep = mf.Staging.KeepGlobs()
		return st, nil
	}
	st.Keep = manifest.DefaultKeep
	list, _ := m.List()
	for _, s := range list {
		if s.Manifest != nil && s.Manifest.Staging != nil && s.Manifest.Staging.Of == domain {
			st.Copies = append(st.Copies, s.Domain)
			st.Keep = s.Manifest.Staging.KeepGlobs()
		}
	}
	sort.Strings(st.Copies)
	if info, err := os.Stat(m.beforeDir(domain)); err == nil {
		st.Before = info.ModTime()
	}
	return st, nil
}

// CreateStaging makes name a test copy of domain: its folder, files and
// databases as they are now, under a name of its own, behind a password.
func (m *Manager) CreateStaging(ctx context.Context, domain, name string) (StagingResult, error) {
	var res StagingResult
	dir, err := m.managedDir(domain)
	if err != nil {
		return res, err
	}
	if name == "" {
		name = StagingName(domain)
	}
	name = strings.ToLower(strings.TrimSpace(name))
	if !manifest.ValidDomain(name) || name == domain {
		return res, ErrBadDomain
	}
	if manifest.IsProject(domain) {
		return res, errors.New("a project answers no name: there is nothing to test it under")
	}
	if fsx.Exists(m.Dir(name)) {
		return res, ErrExists
	}
	prod, err := manifest.Load(dir)
	if err != nil {
		return res, fmt.Errorf("manifest: %w", err)
	}
	if prod.Staging != nil {
		return res, fmt.Errorf("%s is itself a test copy of %s: make the copy from production", domain, prod.Staging.Of)
	}
	if prod.Replica != nil {
		return res, errors.New("this is a cluster replica: make the copy on the primary")
	}
	mf, notes, password, err := stagingManifest(prod, domain, name)
	if err != nil {
		return res, err
	}
	// Named after the copy: a manifest is checked against its folder.
	work := filepath.Join(m.stagingDir(name), name)
	_ = os.RemoveAll(work)
	if err := os.MkdirAll(filepath.Dir(work), 0o755); err != nil {
		return res, err
	}
	defer os.RemoveAll(filepath.Dir(work))
	if err := copyTree(ctx, dir, work, nil); err != nil {
		return res, err
	}
	notes = append(notes, sqliteSnapshots(ctx, dir, work)...)
	if err := manifest.Save(work, mf); err != nil {
		return res, err
	}
	if fsx.Exists(m.Dir(name)) {
		return res, ErrExists
	}
	if err := os.Rename(work, m.Dir(name)); err != nil {
		return res, err
	}
	m.emit("staging.created", name, "", "test copy of "+domain, map[string]any{"of": domain})
	return StagingResult{Site: m.describe(name), Password: password, Notes: notes}, nil
}

// stagingManifest is production's manifest made into its copy's: what
// names production (its aliases, its other names, its fixed ports, its
// replicas, its backups) goes, a password door goes up when production
// has none, and the copy is marked.
func stagingManifest(prod *manifest.Manifest, domain, name string) (*manifest.Manifest, []string, string, error) {
	raw, err := json.Marshal(prod)
	if err != nil {
		return nil, nil, "", err
	}
	mf, err := manifest.Parse(raw)
	if err != nil {
		return nil, nil, "", err
	}
	var notes []string
	rename := func(host string) (string, bool) {
		switch {
		case host == domain:
			return name, true
		case strings.HasSuffix(host, "."+domain):
			return strings.TrimSuffix(host, "."+domain) + "." + name, true
		}
		return "", false
	}
	if len(mf.Aliases) > 0 {
		notes = append(notes, "aliases are production's names and were left out: "+strings.Join(mf.Aliases, ", "))
	}
	mf.Aliases = nil
	mf.ForceHTTPS = nil
	off := false
	mf.WWW = &off
	if mf.Cluster != nil {
		notes = append(notes, "the copy has no cluster replicas")
	}
	mf.Cluster, mf.Replica = nil, nil
	if len(mf.Backups) > 0 {
		notes = append(notes, "backups were not carried: the copy's databases are copies already")
	}
	mf.Backups = nil
	if mf.Title != "" {
		mf.Title += " (staging)"
	}
	markEnv := func(r *manifest.Run) {
		if r == nil {
			return
		}
		r.Port = 0
		if r.Env == nil {
			r.Env = map[string]string{}
		}
		// The code can tell it is the copy: a payment in test mode, no
		// mail to real customers.
		r.Env["MIDDLEWARE_STAGING"] = domain
		if r.Docker != nil && namedVolumes(r.Docker.Volumes) {
			notes = append(notes, volumeNote)
		}
	}
	markEnv(mf.Run)
	for _, w := range mf.Workers {
		if w != nil {
			w.Port = 0
			if w.Docker != nil && namedVolumes(w.Docker.Volumes) {
				notes = append(notes, volumeNote)
			}
		}
	}
	for svcName, sv := range mf.Services {
		if sv == nil {
			continue
		}
		markEnv(sv.Run)
		var hosts []string
		for _, h := range sv.Domains {
			if n, ok := rename(h); ok {
				hosts = append(hosts, n)
			} else {
				notes = append(notes, fmt.Sprintf("services.%s: %s is not under %s and was left out", svcName, h, domain))
			}
		}
		sv.Domains = hosts
	}
	password := ""
	if mf.Access == nil || mf.Access.Password == "" {
		password = newPassword()
		hash, err := access.HashPassword(password)
		if err != nil {
			return nil, nil, "", err
		}
		if mf.Access == nil {
			mf.Access = &manifest.Access{}
		}
		mf.Access.Password = hash
		mf.Access.Message = "Test copy of " + domain
	}
	mf.Staging = &manifest.Staging{Of: domain}
	if err := mf.Validate(); err != nil {
		return nil, nil, "", fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return mf, dedupe(notes), password, nil
}

// PromoteStaging puts the test copy's code into its production site,
// leaving production's manifest and its kept paths as they are.
func (m *Manager) PromoteStaging(ctx context.Context, name string) (StagingResult, error) {
	dir, err := m.managedDir(name)
	if err != nil {
		return StagingResult{}, err
	}
	mf, err := manifest.Load(dir)
	if err != nil {
		return StagingResult{}, fmt.Errorf("manifest: %w", err)
	}
	if mf.Staging == nil {
		return StagingResult{}, ErrNotStaging
	}
	prod := mf.Staging.Of
	if _, err := m.managedDir(prod); err != nil {
		return StagingResult{}, fmt.Errorf("%s, the site this is a copy of: %w", prod, err)
	}
	if err := m.swapIn(ctx, dir, prod, mf.Staging.KeepGlobs(), nil); err != nil {
		return StagingResult{}, err
	}
	m.emit("staging.promoted", prod, "", name+" promoted to "+prod, map[string]any{"from": name})
	return StagingResult{Site: m.describe(prod)}, nil
}

// UndoPromotion puts back the folder the last promotion set aside, with
// production's kept paths as they are now: the data written since the
// promotion stays.
func (m *Manager) UndoPromotion(ctx context.Context, domain string) (StagingResult, error) {
	if _, err := m.managedDir(domain); err != nil {
		return StagingResult{}, err
	}
	before := m.beforeDir(domain)
	if !fsx.Exists(before) {
		return StagingResult{}, errors.New("there is no promotion to undo")
	}
	keep := manifest.DefaultKeep
	if st, err := m.Staging(domain); err == nil {
		keep = st.Keep
	}
	// The folder set aside is moved out of the way first: swapIn sets
	// the one it replaces aside under the same name.
	held := before + ".undo"
	_ = os.RemoveAll(held)
	if err := os.Rename(before, held); err != nil {
		return StagingResult{}, err
	}
	if err := m.swapIn(ctx, held, domain, keep, nil); err != nil {
		_ = os.Rename(held, before)
		return StagingResult{}, err
	}
	_ = os.RemoveAll(held)
	m.emit("staging.undone", domain, "", "the last promotion was undone", nil)
	return StagingResult{Site: m.describe(domain)}, nil
}

// RefreshStaging copies production into its test copy again, data too,
// keeping the copy's manifest (its name, its password).
func (m *Manager) RefreshStaging(ctx context.Context, name string) (StagingResult, error) {
	dir, err := m.managedDir(name)
	if err != nil {
		return StagingResult{}, err
	}
	mf, err := manifest.Load(dir)
	if err != nil {
		return StagingResult{}, fmt.Errorf("manifest: %w", err)
	}
	if mf.Staging == nil {
		return StagingResult{}, ErrNotStaging
	}
	prodDir, err := m.managedDir(mf.Staging.Of)
	if err != nil {
		return StagingResult{}, fmt.Errorf("%s, the site this is a copy of: %w", mf.Staging.Of, err)
	}
	var notes []string
	if err := m.swapIn(ctx, prodDir, name, nil, func(next string) { notes = sqliteSnapshots(ctx, prodDir, next) }); err != nil {
		return StagingResult{}, err
	}
	// What was set aside is the copy's old self: nothing undoes a
	// refresh, and keeping it would only hold the disk.
	_ = os.RemoveAll(m.beforeDir(name))
	m.emit("staging.refreshed", name, "", "copied again from "+mf.Staging.Of, map[string]any{"of": mf.Staging.Of})
	return StagingResult{Site: m.describe(name), Notes: notes}, nil
}

// swapIn makes target's folder a copy of src, with target's manifest and
// target's paths that match keep, then exchanges the two folders and
// hands the site over. The folder that was target's is left at
// beforeDir(target).
func (m *Manager) swapIn(ctx context.Context, src, target string, keep []string, prepare func(next string)) error {
	tdir := m.Dir(target)
	base := m.stagingDir(target)
	next := filepath.Join(base, "next")
	_ = os.RemoveAll(next)
	if err := os.MkdirAll(base, 0o755); err != nil {
		return err
	}
	skip := func(p string, _ bool) bool {
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return false
		}
		rel = filepath.ToSlash(rel)
		return rel == manifest.FileName || (len(keep) > 0 && manifest.Kept(keep, rel))
	}
	if err := copyTree(ctx, src, next, skip); err != nil {
		_ = os.RemoveAll(next)
		return err
	}
	if err := linkKept(tdir, next, keep); err != nil {
		_ = os.RemoveAll(next)
		return err
	}
	if prepare != nil {
		prepare(next)
	}
	own, err := os.ReadFile(filepath.Join(tdir, manifest.FileName))
	if err != nil {
		_ = os.RemoveAll(next)
		return err
	}
	if err := fsx.WriteAtomic(filepath.Join(next, manifest.FileName), own, 0o644); err != nil {
		_ = os.RemoveAll(next)
		return err
	}
	if err := exchange(next, tdir); err != nil {
		_ = os.RemoveAll(next)
		return err
	}
	// next is now the folder that served until this moment.
	before := m.beforeDir(target)
	_ = os.RemoveAll(before)
	if err := os.Rename(next, before); err != nil {
		return err
	}
	// The files changed and the pointer may not have: a new generation
	// is started from the new folder. One the engine already started on
	// its own (a changed pointer) is the same outcome.
	if m.Engine != nil {
		if err := m.Engine.Restart(ctx, target); err != nil && !strings.Contains(err.Error(), "already in progress") && !strings.Contains(err.Error(), "not a running site") {
			m.emit("staging.restart", target, "", "the new folder is in place but the restart failed: "+err.Error(), nil)
		}
		if mf, err := manifest.Load(tdir); err == nil {
			for w := range mf.Workers {
				_ = m.Engine.RestartWorker(ctx, target, w)
			}
		}
	}
	return nil
}

// copyTree copies src to dst (which must not exist) with the archive's
// rules: links stored as links, nothing written through one, modes kept.
func copyTree(ctx context.Context, src, dst string, skip func(string, bool) bool) error {
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		_, err := archive.PackParts(ctx, pw, []archive.Part{{Name: "", Path: src, Skip: skip}})
		pw.CloseWithError(err)
		done <- err
	}()
	_, uerr := archive.UnpackFrom(ctx, pr, dst)
	pr.CloseWithError(uerr)
	if perr := <-done; perr != nil && uerr == nil {
		return perr
	}
	return uerr
}

// linkKept puts into dst every path of src that keep matches, by hard
// link: the very files production writes, so nothing written between the
// copy and the exchange is lost. A link across file systems is a copy.
func linkKept(src, dst string, keep []string) error {
	if len(keep) == 0 {
		return nil
	}
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(src, p)
		if rel == "." {
			return nil
		}
		slash := filepath.ToSlash(rel)
		if slash == manifest.FileName || !manifest.Kept(keep, slash) {
			return nil
		}
		to := filepath.Join(dst, rel)
		// What the copy had in its place goes: production's is the one.
		_ = os.RemoveAll(to)
		if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
			return err
		}
		if d.IsDir() {
			if err := linkTree(p, to); err != nil {
				return err
			}
			return fs.SkipDir
		}
		return linkOne(p, to, d)
	})
}

func linkTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(src, p)
		to := filepath.Join(dst, rel)
		if d.IsDir() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			return os.MkdirAll(to, info.Mode().Perm()|0o700)
		}
		return linkOne(p, to, d)
	})
}

func linkOne(src, dst string, d fs.DirEntry) error {
	if d.Type()&fs.ModeSymlink != 0 {
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)
	}
	if !d.Type().IsRegular() {
		return nil
	}
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	info, err := d.Info()
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// sqliteSnapshots replaces every SQLite database copied from src into
// dst with a consistent copy made by sqlite3 itself: a file copied while
// the site writes it can be torn, and a test copy whose database will not
// open is no test of anything. Without sqlite3 the plain copy stays and a
// note says so.
func sqliteSnapshots(ctx context.Context, src, dst string) []string {
	var dbs []string
	_ = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && d.Name() == "node_modules" {
			return fs.SkipDir
		}
		if d.Type().IsRegular() && isSQLite(p) {
			dbs = append(dbs, p)
		}
		return nil
	})
	if len(dbs) == 0 {
		return nil
	}
	bin, err := exec.LookPath("sqlite3")
	if err != nil {
		return []string{"sqlite3 is not installed: the databases were copied as files, which a write during the copy can leave torn"}
	}
	var notes []string
	for _, db := range dbs {
		rel, _ := filepath.Rel(src, db)
		to := filepath.Join(dst, rel)
		tmp := to + ".snapshot"
		_ = os.Remove(tmp)
		cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		out, err := exec.CommandContext(cctx, bin, db, ".backup '"+strings.ReplaceAll(tmp, "'", "''")+"'").CombinedOutput()
		cancel()
		if err != nil {
			_ = os.Remove(tmp)
			notes = append(notes, fmt.Sprintf("%s: copied as a file (sqlite3: %s)", rel, strings.TrimSpace(string(out))))
			continue
		}
		if err := os.Rename(tmp, to); err != nil {
			notes = append(notes, fmt.Sprintf("%s: %v", rel, err))
			continue
		}
		for _, side := range []string{"-wal", "-shm", "-journal"} {
			_ = os.Remove(to + side)
		}
	}
	return notes
}

func isSQLite(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 16)
	n, _ := io.ReadFull(f, head)
	return n == 16 && bytes.Equal(head, []byte("SQLite format 3\x00"))
}

const volumeNote = "named docker volumes start empty in the copy: what a container keeps in one (a database) was not copied"

// namedVolumes reports whether any volume is a named one ("pgdata:/var/
// lib/postgresql/data"), kept by docker outside the folder, rather than a
// folder of the site mounted in, which the copy carries.
func namedVolumes(vols []string) bool {
	for _, v := range vols {
		src, _, ok := strings.Cut(v, ":")
		if ok && !strings.HasPrefix(src, ".") && !strings.HasPrefix(src, "/") {
			return true
		}
	}
	return false
}

func newPassword() string {
	const alphabet = "abcdefghijkmnpqrstuvwxyz23456789"
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b[:4]) + "-" + string(b[4:8]) + "-" + string(b[8:12]) + "-" + string(b[12:])
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
