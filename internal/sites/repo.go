package sites

import (
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/manifest"
)

// Repo is what a site made from a GitHub repository is decided from: the
// repository's names, and what its root holds.
type Repo struct {
	FullName      string   `json:"full_name"`
	DefaultBranch string   `json:"default_branch"`
	Private       bool     `json:"private"`
	Description   string   `json:"description,omitempty"`
	Files         []string `json:"files,omitempty"`
	// PackageJSON is the root package.json, when there is one.
	PackageJSON []byte `json:"-"`
}

var (
	repoName  = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	sshRemote = regexp.MustCompile(`^git@github\.com:([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+?)(\.git)?$`)
)

// ErrBadRepo is a repository given in a shape that is not GitHub's.
var ErrBadRepo = errors.New("not a GitHub repository: give owner/name or its github.com address")

// ParseRepo reads owner/name out of what a person pastes: owner/name, the
// repository's page, its clone address (https or ssh), with or without
// .git and a trailing slash or path.
func ParseRepo(s string) (string, error) {
	s = strings.TrimSpace(s)
	if m := sshRemote.FindStringSubmatch(s); m != nil {
		return m[1], nil
	}
	if strings.Contains(s, "://") || strings.HasPrefix(s, "github.com/") {
		if !strings.Contains(s, "://") {
			s = "https://" + s
		}
		u, err := url.Parse(s)
		if err != nil || (u.Host != "github.com" && u.Host != "www.github.com") {
			return "", ErrBadRepo
		}
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) < 2 {
			return "", ErrBadRepo
		}
		s = parts[0] + "/" + strings.TrimSuffix(parts[1], ".git")
	}
	s = strings.TrimSuffix(s, ".git")
	if !repoName.MatchString(s) || strings.Contains(s, "..") {
		return "", ErrBadRepo
	}
	return s, nil
}

func (r Repo) has(name string) bool {
	for _, f := range r.Files {
		if f == name {
			return true
		}
	}
	return false
}

// Kind is what a repository's root says it is: "node", "static-build"
// (a front end that is built and then served as files), "go" or
// "static".
func (r Repo) Kind() string {
	if r.has("package.json") {
		var pkg struct {
			Scripts map[string]string `json:"scripts"`
		}
		_ = json.Unmarshal(r.PackageJSON, &pkg)
		if pkg.Scripts["start"] == "" && pkg.Scripts["build"] != "" {
			return "static-build"
		}
		return "node"
	}
	if r.has("go.mod") {
		return "go"
	}
	return "static"
}

// RepoManifest is the manifest of a site made from r: run as its kind says
// (or as template says, when one is given), with deploy.github naming the
// repository and the branch, so the first deploy clones it and every push
// after it deploys again.
func RepoManifest(r Repo, branch, template string) (*manifest.Manifest, string) {
	kind := template
	if kind == "" || kind == "auto" {
		kind = r.Kind()
	}
	if branch == "" {
		branch = r.DefaultBranch
	}
	gh := &manifest.GitHubDeploy{Repository: r.FullName, Branch: branch}
	m := &manifest.Manifest{Version: manifest.Version, Deploy: &manifest.Deploy{GitHub: gh}}
	npmInstall := []string{"npm", "install", "--no-audit", "--no-fund"}
	if r.has("package-lock.json") {
		npmInstall = []string{"npm", "ci", "--no-audit", "--no-fund"}
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	_ = json.Unmarshal(r.PackageJSON, &pkg)
	switch kind {
	case "node":
		gh.Build = [][]string{npmInstall}
		if pkg.Scripts["build"] != "" {
			gh.Build = append(gh.Build, []string{"npm", "run", "build"})
		}
		m.Run = &manifest.Run{Command: []string{"npm", "start"}, Env: map[string]string{"NODE_ENV": "production"}, Health: &manifest.Health{Path: "/"}}
	case "static-build":
		gh.Build = [][]string{npmInstall, {"npm", "run", "build"}}
		root := "dist"
		switch b := pkg.Scripts["build"]; {
		case strings.Contains(b, "react-scripts"):
			root = "build"
		case strings.Contains(b, "next"):
			// A Next.js app with no start script is a static export
			// (output: "export"), which writes out/.
			root = "out"
		}
		m.Static = &manifest.Static{Root: root}
	case "go":
		m.Run = &manifest.Run{Command: []string{"go", "run", "."}, Health: &manifest.Health{Path: "/", Timeout: manifest.Duration(5 * time.Minute)}}
	default:
		kind = "static"
		// Dotfiles and site.json are never served, so the repository's
		// root can be the site's.
		root := ""
		if !r.has("index.html") && r.has("public") {
			root = "public"
		}
		m.Static = &manifest.Static{Root: root}
	}
	return m, kind
}
