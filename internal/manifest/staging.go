package manifest

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// Staging marks a site as the test copy of another, written by the house
// when the copy is made (internal/sites, staging.go). The copy is a site
// like any other; what the mark adds is the pair: the panel shows one
// next to the other, and promoting the copy knows where it goes.
type Staging struct {
	// Of is the production site this is a copy of.
	Of string `json:"of"`
	// Keep are globs, relative to the folder, of what is production's own
	// and stays in production when the copy is promoted: its databases,
	// what its visitors uploaded, its secrets. A glob without a slash
	// matches a name at any depth ("*.sqlite"), one with a slash the path
	// from the folder ("storage/app/*"). Empty is DefaultKeep.
	Keep []string `json:"keep,omitempty"`
	// Branch makes the copy a preview: it deploys this branch of the
	// production site's repository, and goes when the branch does.
	Branch string `json:"branch,omitempty"`
}

// DefaultKeep is what a promotion leaves in production when the copy
// names nothing: what a site keeps, as opposed to what it is. Leaving
// a database out of it would replace the production one with the test
// copy's, and that is a loss no undo the next day gets back whole, so
// the list errs on keeping.
var DefaultKeep = []string{
	"*.db", "*.db-wal", "*.db-shm", "*.db-journal",
	"*.sqlite", "*.sqlite-wal", "*.sqlite-shm", "*.sqlite-journal",
	"*.sqlite3", "*.sqlite3-wal", "*.sqlite3-shm", "*.sqlite3-journal",
	".env", ".env.*",
	"storage", "uploads", "data",
}

// KeepGlobs are the copy's Keep, or DefaultKeep.
func (s *Staging) KeepGlobs() []string {
	if s == nil || len(s.Keep) == 0 {
		return DefaultKeep
	}
	return s.Keep
}

// Kept reports whether rel (a slash-separated path inside the folder)
// is one of globs.
func Kept(globs []string, rel string) bool {
	rel = strings.Trim(path.Clean("/"+rel), "/")
	base := path.Base(rel)
	for _, g := range globs {
		g = strings.Trim(g, "/")
		if strings.Contains(g, "/") {
			if ok, _ := path.Match(g, rel); ok {
				return true
			}
			continue
		}
		if ok, _ := path.Match(g, base); ok {
			return true
		}
	}
	return false
}

func validStaging(s *Staging, self string) error {
	if s == nil {
		return nil
	}
	if !ValidDomain(s.Of) {
		return fmt.Errorf("staging.of: %q is not a site's domain", s.Of)
	}
	if self != "" && s.Of == self {
		return errors.New("staging.of: a site is not a copy of itself")
	}
	for _, g := range s.Keep {
		c := strings.Trim(g, "/")
		if c == "" || strings.HasPrefix(g, "/") || c == ".." || strings.HasPrefix(c, "../") || strings.Contains(c, "/../") {
			return fmt.Errorf("staging.keep: %q must be a path inside the folder", g)
		}
		if _, err := path.Match(c, ""); err != nil {
			return fmt.Errorf("staging.keep: %q: %v", g, err)
		}
	}
	return nil
}
