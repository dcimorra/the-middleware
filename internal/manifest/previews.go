package manifest

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
)

// Previews give each branch pushed to the site's repository a test copy
// of its own, at <branch>-preview.<domain>: made from production (files,
// databases, behind a password like any test copy), deploying that
// branch on every push to it, and removed when the branch is deleted or
// has not been pushed to for Keep.
type Previews struct {
	// Branches are the branches that get one, as globs ("feature/*");
	// empty is every branch but the one the site deploys.
	Branches []string `json:"branches,omitempty"`
	// Keep is how long a preview lives after its last push; empty is
	// DefaultPreviewKeep.
	Keep Duration `json:"keep,omitempty"`
}

// DefaultPreviewKeep is a week: long enough for a review, short enough
// that forgotten branches do not pile up copies of the databases.
const DefaultPreviewKeep = 7 * 24 * time.Hour

// Lifetime is how long a preview lives after its last push.
func (p *Previews) Lifetime() time.Duration { return p.Keep.Or(DefaultPreviewKeep) }

// Wants says whether branch gets a preview.
func (p *Previews) Wants(branch string) bool {
	if p == nil || branch == "" {
		return false
	}
	if len(p.Branches) == 0 {
		return true
	}
	for _, g := range p.Branches {
		if g == "*" || g == branch {
			return true
		}
		if ok, _ := path.Match(g, branch); ok {
			return true
		}
	}
	return false
}

var slugCut = regexp.MustCompile(`[^a-z0-9]+`)

// PreviewName is the name of branch's preview of domain: one label, so a
// wildcard certificate of the domain (Cloudflare's own) covers it.
func PreviewName(domain, branch string) string {
	slug := strings.Trim(slugCut.ReplaceAllString(strings.ToLower(branch), "-"), "-")
	if len(slug) > 40 {
		slug = strings.TrimRight(slug[:40], "-")
	}
	if slug == "" {
		slug = "branch"
	}
	return slug + "-preview." + domain
}

func (m *Manifest) validPreviews() error {
	p := m.Previews
	if p == nil {
		return nil
	}
	if m.Deploy == nil || m.Deploy.GitHub == nil {
		return errors.New("previews: the site deploys from no repository (deploy.github)")
	}
	if m.Staging != nil {
		return errors.New("previews: a test copy has no previews of its own; set them on production")
	}
	for _, g := range p.Branches {
		if _, err := path.Match(g, ""); err != nil || g == "" {
			return fmt.Errorf("previews.branches: %q is not a glob", g)
		}
	}
	if p.Keep != 0 && time.Duration(p.Keep) < time.Hour {
		return errors.New("previews.keep: at least 1h")
	}
	return nil
}
