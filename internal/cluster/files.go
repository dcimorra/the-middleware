package cluster

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/dcimorra/the-middleware/internal/archive"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/relocate"
)

// What a replica receives is the site's folder and nothing the house keeps
// of it elsewhere: not its conversations, its HOME, its history or its
// logs, which are the primary's. Left out of the folder too:
//
//   - .git: a replica never pulls, and a repository is often the biggest
//     part of a folder;
//   - the dependencies another machine installs for itself (a package's
//     node_modules, relocate.Reinstalled): the Oracle node is aarch64,
//     and a native module built on an x86 primary does not load there;
//   - what move.exclude names: what the site makes again on demand, and
//     what it writes while it runs (a cache), which would otherwise
//     change the fingerprint on every look and copy the site forever.

// parts is what goes to a replica.
func parts(siteDir string, m *manifest.Manifest) []archive.Part {
	skip := func(p string, dir bool) bool {
		if dir && filepath.Base(p) == ".git" {
			return true
		}
		if relocate.SkipReinstalled(p, dir) {
			return true
		}
		if m != nil && m.Move != nil && len(m.Move.Exclude) > 0 {
			rel, err := filepath.Rel(siteDir, p)
			return err == nil && m.Excluded(filepath.ToSlash(rel))
		}
		return false
	}
	return []archive.Part{{Name: "site", Path: siteDir, Skip: skip}}
}

// Fingerprint is what a site's copy is of: every name in what goes to a
// replica with its kind, size and time, hashed. Not the engine's
// revision, which follows the manifest and the environment and not the
// code: a deploy that pulls and builds without touching site.json would
// leave every replica serving the old build under the same revision.
// Sizes and times rather than contents so that looking costs a walk and
// not a read of every byte, every few seconds.
func Fingerprint(siteDir string, m *manifest.Manifest) (string, error) {
	p := parts(siteDir, m)[0]
	type entry struct {
		rel  string
		line string
	}
	var list []entry
	err := filepath.WalkDir(siteDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == siteDir {
				return err
			}
			return nil
		}
		if path != siteDir && p.Skip(path, d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(siteDir, path)
		line := fmt.Sprintf("%o %d %d", info.Mode(), info.Size(), info.ModTime().UnixNano())
		if d.IsDir() {
			// A folder's time moves with every file written in it, the
			// ones left out included.
			line = "dir"
		} else if info.Mode()&os.ModeSymlink != 0 {
			to, _ := os.Readlink(path)
			line = "link " + to
		}
		list = append(list, entry{filepath.ToSlash(rel), line})
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Slice(list, func(i, j int) bool { return list[i].rel < list[j].rel })
	h := sha256.New()
	for _, e := range list {
		fmt.Fprintf(h, "%s\x00%s\n", e.rel, e.line)
	}
	return hex.EncodeToString(h.Sum(nil))[:24], nil
}

// lockfiles are what decides what an install puts in node_modules.
var lockfiles = []string{"package.json", "package-lock.json", "npm-shrinkwrap.json", "yarn.lock", "pnpm-lock.yaml", "bun.lock", "bun.lockb"}

// sameDeps says two folders would install the same dependencies.
func sameDeps(a, b string) bool {
	for _, f := range lockfiles {
		x, errA := os.ReadFile(filepath.Join(a, f))
		y, errB := os.ReadFile(filepath.Join(b, f))
		if (errA == nil) != (errB == nil) || !bytes.Equal(x, y) {
			return false
		}
	}
	return true
}

// linkTree makes dst a copy of src made of hard links: the running copy
// keeps its node_modules while the new one starts with the same files,
// at no cost in space or time. A file that cannot be linked (another
// filesystem) is copied.
func linkTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		to := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(to, info.Mode().Perm()|0o700)
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(target, to)
		case info.Mode().IsRegular():
			if os.Link(p, to) == nil {
				return nil
			}
			return copyFile(p, to, info.Mode().Perm())
		}
		return nil
	})
}

func copyFile(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
