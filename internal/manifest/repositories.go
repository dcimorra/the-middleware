package manifest

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Repository is one GitHub repository kept in the site's folder.
type Repository struct {
	// Repository is owner/name on GitHub.
	Repository string `json:"repository"`
	// Path is the clone's folder, relative to the site's: "build/oss".
	// The watcher that keeps repositories pulled fetches this one with
	// the module's token only from there; "" or "." is the site's folder.
	Path string `json:"path,omitempty"`
}

// GitRepositories are every repository the GitHub module's token may
// reach for this site: the one it deploys from (anywhere in its folder,
// as it always was) and the ones it lists, the first spelling of a name
// kept. GitHub's names are not case sensitive, and neither is the match.
func (m *Manifest) GitRepositories() []Repository {
	if m == nil {
		return nil
	}
	var out []Repository
	seen := map[string]bool{}
	add := func(r Repository) {
		k := strings.ToLower(r.Repository) + "\x00" + r.Path
		if r.Repository == "" || seen[k] {
			return
		}
		seen[k] = true
		out = append(out, r)
	}
	if m.Deploy != nil && m.Deploy.GitHub != nil {
		add(Repository{Repository: m.Deploy.GitHub.Repository})
	}
	for _, r := range m.Repositories {
		r.Path = cleanRepoPath(r.Path)
		add(r)
	}
	return out
}

// GitRepositoryNames are GitRepositories' owner/name, once each.
func (m *Manifest) GitRepositoryNames() []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range m.GitRepositories() {
		if k := strings.ToLower(r.Repository); !seen[k] {
			seen[k] = true
			out = append(out, r.Repository)
		}
	}
	return out
}

func cleanRepoPath(p string) string {
	if p == "" {
		return "."
	}
	return filepath.Clean(p)
}

func validRepositories(rs []Repository) error {
	for i, r := range rs {
		owner, name, ok := strings.Cut(r.Repository, "/")
		if !ok || owner == "" || name == "" || strings.ContainsAny(r.Repository, " :@\\") || strings.Contains(name, "/") {
			return fmt.Errorf("repositories[%d].repository: want owner/name, got %q", i, r.Repository)
		}
		if p := cleanRepoPath(r.Path); filepath.IsAbs(p) || p == ".." || strings.HasPrefix(p, "../") {
			return fmt.Errorf("repositories[%d].path: %q is not a folder inside the site", i, r.Path)
		}
	}
	return nil
}
