package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/dcimorra/the-middleware/internal/auth"
	"github.com/dcimorra/the-middleware/internal/manifest"
)

// A delegate with code may write their site's manifest, but not the keys
// that reach past the site: the names it claims beyond its own (aliases
// could take another site's), where its requests go instead of its
// process (upstream could point at the panel's loopback port), whether
// it runs in a container and what the container mounts, the files read
// into its environment from anywhere on the machine, its place in the
// network, the operator's VPN devices, the agents' monthly cap and the
// gateway the operator pays for. Those stay as the operator wrote them; a request
// that changes one is refused naming it.
//
// This holds against the API. For a site that runs as a process, code
// also means a shell as the supervisor's user, which can write the file
// itself (docs/security.md, "Delegated users"); in a container the
// manifest is mounted read-only, and these keys are the fence.

// lockedKeys are the parts compared, by the name the refusal gives.
func lockedKeys(m *manifest.Manifest) map[string]any {
	out := map[string]any{
		"aliases":    m.Aliases,
		"upstream":   m.Upstream,
		"move":       m.Move,
		"vpn":        m.VPN,
		"claude_api": m.ClaudeAPI,
		"claude":     m.Claude,
		"chat":       m.Chat,
		"needs":      m.Needs,
		// Other nodes' machines and this one's databases, opened to them.
		"cluster": m.Cluster,
		"replica": m.Replica,
		// The site's budget on the machine, as run.docker is a
		// container's: an account that could raise it could take the
		// machine from every other site.
		"limits": m.Limits,
		// What the GitHub module's token reaches: a delegate who could
		// list a repository here could push to any the token opens.
		"repositories": m.Repositories,
		// Which production a copy is promoted into, and what of that
		// production is kept: a delegate of the copy who could point it
		// at another site would promote over a site that is not theirs.
		"staging":  m.Staging,
		"previews": m.Previews,
	}
	// The agent's provider and model are the user's to pick among those
	// the operator turned on (the chat picker writes them here on the
	// first message); the monthly cap is what the operator pays, and
	// stays theirs.
	out["agent.monthly_cap_usd"] = 0.0
	if m.Agent != nil {
		out["agent.monthly_cap_usd"] = m.Agent.MonthlyCapUSD
	}
	if m.Run != nil {
		out["run.runtime"] = runtimeOf(m.Run.Runtime)
		out["run.docker"] = m.Run.Docker
		out["run.env_files"] = m.Run.EnvFiles
		out["run.env_from_file"] = m.Run.EnvFromFile
	} else {
		out["run.runtime"] = runtimeOf("")
	}
	// A service's names are what aliases are, and its run section is
	// locked where a site's is.
	for name, sv := range m.Services {
		if sv == nil {
			continue
		}
		out["services."+name+".domains"] = sv.Domains
		if sv.Run == nil {
			out["services."+name+".run.runtime"] = runtimeOf("")
			continue
		}
		out["services."+name+".run.runtime"] = runtimeOf(sv.Run.Runtime)
		out["services."+name+".run.docker"] = sv.Run.Docker
		out["services."+name+".run.env_files"] = sv.Run.EnvFiles
		out["services."+name+".run.env_from_file"] = sv.Run.EnvFromFile
	}
	for name, w := range m.Workers {
		if w == nil {
			continue
		}
		out["workers."+name+".runtime"] = runtimeOf(w.Runtime)
		out["workers."+name+".docker"] = w.Docker
		out["workers."+name+".env_files"] = w.EnvFiles
		out["workers."+name+".env_from_file"] = w.EnvFromFile
		out["workers."+name+".move"] = w.Move
	}
	return out
}

func runtimeOf(r string) string {
	if r == "" {
		return "process"
	}
	return r
}

// lockedChange is the first locked key next changes from current, or "".
// A worker that is added or removed changes only what it carries.
func lockedChange(current, next *manifest.Manifest) string {
	a, b := lockedKeys(current), lockedKeys(next)
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	for k := range keys {
		x, y := a[k], b[k]
		if strings.HasPrefix(k, "workers.") || strings.HasPrefix(k, "services.") {
			// A worker new or gone counts as one that ran as a process
			// with nothing locked.
			if x == nil {
				x = zeroLike(k)
			}
			if y == nil {
				y = zeroLike(k)
			}
		}
		if !sameJSON(x, y) {
			return k
		}
	}
	return ""
}

func zeroLike(k string) any {
	if strings.HasSuffix(k, ".runtime") {
		return "process"
	}
	return nil
}

// sameJSON compares as the manifest is written: a nil map and an empty
// one are both absent.
func sameJSON(x, y any) bool {
	jx, _ := json.Marshal(x)
	jy, _ := json.Marshal(y)
	empty := func(b []byte) bool {
		s := string(b)
		return s == "null" || s == "{}" || s == "[]" || s == `""` || s == "false"
	}
	if empty(jx) && empty(jy) {
		return true
	}
	return reflect.DeepEqual(jx, jy)
}

// checkDelegateManifest refuses a manifest a delegate wrote when it
// changes a locked key of the site's current one. next is the whole
// manifest as it would be saved.
func (s *Server) checkDelegateManifest(r *http.Request, domain string, next []byte) error {
	if !auth.FromContext(r.Context()).Delegate() {
		return nil
	}
	m, err := s.sitesManager()
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(filepath.Join(m.Dir(domain), manifest.FileName))
	if err != nil {
		return err
	}
	cur, err := manifest.Parse(raw)
	if err != nil {
		return err
	}
	nm, err := manifest.Parse(next)
	if err != nil {
		// The manager says what is wrong with it, as for anyone.
		return nil
	}
	if k := lockedChange(cur, nm); k != "" {
		return Errorf(http.StatusForbidden, "%s is decided by the owner of this installation and cannot be changed from your account", k)
	}
	return nil
}

// patchedManifest is the site's manifest with partial merged in, the way
// the manager merges a PATCH: top-level keys, null removing one.
func (s *Server) patchedManifest(domain string, partial map[string]json.RawMessage) ([]byte, error) {
	m, err := s.sitesManager()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(m.Dir(domain), manifest.FileName))
	if err != nil {
		return nil, err
	}
	current := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &current); err != nil {
		return nil, err
	}
	for k, v := range partial {
		if string(v) == "null" {
			delete(current, k)
		} else {
			current[k] = v
		}
	}
	return json.Marshal(current)
}

// isManifestPath says whether rel, a path the file routes take, is the
// site's manifest.
func isManifestPath(rel string) bool {
	return path.Clean("/"+filepath.ToSlash(rel)) == "/"+manifest.FileName
}
