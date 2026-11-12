package relocate

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/runtime"
)

// What runs next to a site without the house knowing: a compose file in
// its folder, and the containers somebody started from it by hand. A move
// carries the folder and what the manifest declares; these stay where
// they were, running, and the site arrives without them. p2p-webrtc moved
// with its app and without its TURN relay, a coturn started with
// `docker compose up` on the node it left, and the first to learn of it
// was a visitor on mobile data. A move cannot carry them (it does not
// know what they keep, nor on which ports); it can say so before it
// starts and again where the site arrives.

// composeNames are the names docker compose reads on its own.
var composeNames = map[string]bool{"docker-compose.yml": true, "docker-compose.yaml": true, "compose.yml": true, "compose.yaml": true}

// packages are the folders of other people's code, whose compose files
// (a package ships its own for its tests) are not the site's.
var packages = map[string]bool{"node_modules": true, "vendor": true}

// composeDepth is how deep under the site's folder a compose file is
// looked for: infra/docker-compose.yml, deploy/prod/compose.yaml.
const composeDepth = 3

// ComposeFiles are the compose files in a site's folder, relative to it.
func ComposeFiles(siteDir string) []string {
	var out []string
	_ = filepath.WalkDir(siteDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(siteDir, path)
		if d.IsDir() {
			if rel != "." && (strings.HasPrefix(d.Name(), ".") || packages[d.Name()] || strings.Count(filepath.ToSlash(rel), "/") >= composeDepth-1) {
				return filepath.SkipDir
			}
			return nil
		}
		if composeNames[d.Name()] {
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// Unmanaged are a move's warnings about what will not travel: the
// compose files of the folder and the containers found running from it
// (containers may be nil: no docker here, or nobody asked).
func Unmanaged(siteDir string, m *manifest.Manifest, containers []runtime.Foreign) []string {
	var out []string
	files := ComposeFiles(siteDir)
	if len(containers) > 0 {
		names := make([]string, len(containers))
		for i, c := range containers {
			names[i] = c.String()
		}
		out = append(out, fmt.Sprintf("containers of this site that the house did not start are running on this node and do not move with it: %s. The site will arrive without them. What it needs next to it belongs in its manifest as a service (one with public ports, if it answers on ports of its own: docs/services.md), which the house starts, watches and moves", strings.Join(names, "; ")))
	}
	if len(files) > 0 && len(containers) == 0 {
		out = append(out, fmt.Sprintf("%s in the site's folder is not read by the house: whatever was started from it by hand (docker compose up) does not move with the site. If the site needs one of its services, declare it in the manifest's services", strings.Join(files, ", ")))
	}
	return out
}
