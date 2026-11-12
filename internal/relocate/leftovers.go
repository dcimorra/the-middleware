package relocate

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/runtime"
)

// What moves leave behind on a node, to be seen and collected from the
// panel: receptions that never finished (the moving node died, or its
// undo did not reach here), copies of a site's data that a commit set
// aside to make room for what arrived (".before-…", one per retry, each a
// whole HOME), the records of sites placed here whose move never said how
// it ended, and the docker volumes of sites that moved away. Nothing here is removed without the operator: an earlier
// life set aside may be the only copy of something.

// stagingIdle is how long a reception must have been still to count as
// abandoned: one in progress writes all the time.
const stagingIdle = time.Hour

// Leftover is one thing a move left on this node.
type Leftover struct {
	// Path is relative to the root, and is what Clean takes; a volume's
	// is "docker-volume:" and its name.
	Path string `json:"path"`
	// Kind: staging (a reception that never finished), aside (an earlier
	// copy a commit set aside), placed (a site placed here that did not
	// come up and was not taken out), record (a site that came up, whose
	// move never said it ended), volume (a docker volume a site that moved
	// away left here).
	Kind     string    `json:"kind"`
	Domain   string    `json:"domain"`
	Bytes    int64     `json:"bytes"`
	Modified time.Time `json:"modified"`
}

// asideParents are the folders a commit sets things aside in: the parents
// of the parts under data/ (Parts).
var asideParents = []string{"data/homes", "data/chat", "data/history", "data/deploys", "data/agents/cost", "data/ports",
	"data/backups", "data/visits", "data/webvitals", "data/status", "data/seen", "data/audit", "data/agents/config"}

// Leftovers lists what moves left under root, the largest first.
func Leftovers(root string, now time.Time) []Leftover {
	var out []Leftover
	add := func(rel, kind, domain string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Lstat(p)
		if err != nil {
			return
		}
		out = append(out, Leftover{Path: rel, Kind: kind, Domain: domain, Bytes: sizeOf(p), Modified: newest(p, info.ModTime())})
	}
	relocate := filepath.Join(root, "data", "relocate")
	entries, _ := os.ReadDir(relocate)
	for _, e := range entries {
		name := e.Name()
		// Dot names are the house's own records (no site is named so).
		if !e.IsDir() || name == "placed" || strings.HasPrefix(name, ".") {
			continue
		}
		rel := "data/relocate/" + name
		if now.Sub(newest(filepath.Join(relocate, name), time.Time{})) < stagingIdle {
			continue // being received now
		}
		add(rel, "staging", strings.TrimSuffix(name, ".delta"))
	}
	records, _ := os.ReadDir(filepath.Join(relocate, "placed"))
	for _, e := range records {
		domain, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		kind := "placed"
		if _, err := os.Lstat(filepath.Join(root, "sites", domain)); err == nil {
			kind = "record"
		}
		add("data/relocate/placed/"+e.Name(), kind, domain)
	}
	for _, parent := range asideParents {
		entries, _ := os.ReadDir(filepath.Join(root, filepath.FromSlash(parent)))
		for _, e := range entries {
			base, _, ok := strings.Cut(e.Name(), ".before-")
			if !ok {
				continue
			}
			add(parent+"/"+e.Name(), "aside", strings.TrimSuffix(base, ".json"))
		}
	}
	// The docker volumes moves left (sourceVolumes), by their name: their
	// size would be docker's to say, and asking it here is a call per
	// volume on every listing.
	for vol, lv := range leftVolumes(root) {
		out = append(out, Leftover{Path: volumePrefix + vol, Kind: "volume", Domain: lv.Domain, Modified: lv.At})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Bytes != out[j].Bytes {
			return out[i].Bytes > out[j].Bytes
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// cleanDocker is the docker Clean removes volumes with; the tests give it
// a docker of their own.
var cleanDocker = runtime.Docker{}

// ErrNotALeftover is a path Clean was given that Leftovers does not list.
var ErrNotALeftover = errors.New("not a leftover of a move")

// Clean removes one leftover, by the path Leftovers gave: only what it
// lists now, so no path from outside reaches os.RemoveAll. A placed site
// is taken out as a failed move would have (Unplace: what arrived removed,
// what was there before put back).
func Clean(root, path string, now time.Time) error {
	for _, l := range Leftovers(root, now) {
		if l.Path != path {
			continue
		}
		switch l.Kind {
		case "placed":
			return Receiver{Root: root}.Unplace(l.Domain)
		case "volume":
			vol := strings.TrimPrefix(l.Path, volumePrefix)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := cleanDocker.RemoveVolume(ctx, vol); err != nil {
				return err
			}
			return forgetVolume(root, vol)
		}
		return os.RemoveAll(filepath.Join(root, filepath.FromSlash(l.Path)))
	}
	return ErrNotALeftover
}

func sizeOf(p string) int64 {
	var n int64
	_ = filepath.WalkDir(p, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

// newest is the latest modification under p, or since when nothing is.
func newest(p string, since time.Time) time.Time {
	t := since
	_ = filepath.WalkDir(p, func(_ string, d fs.DirEntry, err error) error {
		if err == nil {
			if info, err := d.Info(); err == nil && info.ModTime().After(t) {
				t = info.ModTime()
			}
		}
		return nil
	})
	return t
}
