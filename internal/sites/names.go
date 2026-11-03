package sites

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/dcimorra/the-middleware/internal/manifest"
)

// ErrMainName is a request to take away the name the site's folder is
// called: every site answers its own name, so another one has to be made
// the main one first.
var ErrMainName = errors.New("that is the site's main name; make another of its names the main one first")

// A site is born with a name it may not keep: one made up at creation
// (quiet-otter-41.example.net) until the real domain is ready. Adding the
// real one as an alias made it answer both, and a rename alone dropped
// the made-up name from DNS before the new one was published. Making an
// alias the main name is a rename with the two swapped in the manifest:
// the site keeps answering every name it answered, and the old one is
// then an alias like any other, taken away with its own button when its
// visitors have moved.

// SetMain makes name, one the site answers already, the name of its
// folder: the site is renamed to it (its HOME, logs, conversations and
// cost come along, as with Rename) and the old name stays as an alias.
// A site with services keeps its manifest as it is, since the old name is
// still in a service's domains; the new one must be a domain of a service
// with no path, which is what a site's own name has to be.
func (m *Manager) SetMain(ctx context.Context, domain, name string) (Site, error) {
	name = cleanName(name)
	if name == domain {
		return m.describe(domain), nil
	}
	if !manifest.ValidDomain(domain) || !manifest.ValidDomain(name) {
		return Site{}, ErrBadDomain
	}
	dir, err := m.managedDir(domain)
	if err != nil {
		return Site{}, err
	}
	mf, err := manifest.Load(dir)
	if err != nil {
		return Site{}, fmt.Errorf("manifest: %w", err)
	}
	if len(mf.Services) > 0 {
		root := false
		for _, r := range mf.Routes() {
			if r.Host == name && r.Prefix == "" && !derivedWWW(mf, name) {
				root = true
			}
		}
		if !root {
			return Site{}, fmt.Errorf("%s is not a domain of any of %s's services without a path: the main name must be one", name, domain)
		}
		return m.rename(ctx, domain, name, nil)
	}
	if !slices.Contains(mf.Aliases, name) {
		if derivedWWW(mf, name) {
			return Site{}, fmt.Errorf("%s follows its apex: add it as a domain of its own to make it the main name", name)
		}
		return Site{}, fmt.Errorf("%s is not one of %s's domains: add it first", name, domain)
	}
	return m.rename(ctx, domain, name, func(original []byte) ([]byte, error) {
		next, err := manifest.Parse(original)
		if err != nil {
			return nil, fmt.Errorf("manifest: %w", err)
		}
		aliases := []string{domain}
		for _, a := range next.Aliases {
			if a != name && a != domain {
				aliases = append(aliases, a)
			}
		}
		next.Aliases = aliases
		if err := next.Validate(); err != nil {
			return nil, fmt.Errorf("manifest: %w", err)
		}
		data, err := json.MarshalIndent(next, "", "  ")
		if err != nil {
			return nil, err
		}
		return append(data, '\n'), nil
	})
}

// RemoveName takes a name away from a site: an alias, or every domain of
// its services on that host. The site's own name cannot be taken away
// (ErrMainName), and a www. that follows its apex goes with the www
// switch, not here. A service left with no name would become an internal
// one, which changes how it is handed over and who reaches it: that is a
// decision for its manifest, so it is refused here and said why.
//
// The name's force_https line goes with it, and the publisher takes it
// off the internet on its next pass, since it is no longer among the
// site's hosts; another site may claim it from then on.
func (m *Manager) RemoveName(domain, name string) (Site, error) {
	name = cleanName(name)
	if name == domain {
		return Site{}, ErrMainName
	}
	dir, err := m.managedDir(domain)
	if err != nil {
		return Site{}, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, manifest.FileName))
	if err != nil {
		return Site{}, err
	}
	mf, err := manifest.Parse(raw)
	if err != nil {
		return Site{}, fmt.Errorf("manifest: %w", err)
	}
	found := false
	if i := slices.Index(mf.Aliases, name); i >= 0 {
		mf.Aliases = slices.Delete(mf.Aliases, i, i+1)
		if len(mf.Aliases) == 0 {
			mf.Aliases = nil
		}
		found = true
	}
	for _, sn := range mf.ServiceNames() {
		sv := mf.Services[sn]
		kept := sv.Domains[:0:0]
		for _, d := range sv.Domains {
			if host, _ := manifest.SplitDomain(d); host == name {
				found = true
				continue
			}
			kept = append(kept, d)
		}
		if len(kept) == 0 && len(sv.Domains) > 0 {
			return Site{}, fmt.Errorf("%s is the only name of the service %s: give it another domain first, or make it internal in the manifest", name, sn)
		}
		if len(kept) == 0 {
			kept = nil
		}
		sv.Domains = kept
	}
	if !found {
		if derivedWWW(mf, name) {
			return Site{}, fmt.Errorf("%s follows its apex: turn off www instead", name)
		}
		return Site{}, fmt.Errorf("%w: %s does not answer %s", ErrNotFound, domain, name)
	}
	hosts := mf.Hosts(domain)
	var forced []string
	for _, h := range mf.ForceHTTPS {
		if slices.Contains(hosts, cleanName(h)) {
			forced = append(forced, h)
		}
	}
	mf.ForceHTTPS = forced
	if err := manifest.Save(dir, mf); err != nil {
		return Site{}, fmt.Errorf("manifest: %w", err)
	}
	m.emit("manifest.updated", domain, "", name+" is no longer one of its names", map[string]any{"removed": name})
	return m.describe(domain), nil
}

// derivedWWW says whether name is a www. the site answers only because
// its apex is one of its names.
func derivedWWW(mf *manifest.Manifest, name string) bool {
	apex, ok := strings.CutPrefix(name, "www.")
	if !ok || strings.Count(apex, ".") != 1 || (mf.WWW != nil && !*mf.WWW) {
		return false
	}
	if slices.Contains(mf.Aliases, name) {
		return false
	}
	for _, sn := range mf.ServiceNames() {
		for _, d := range mf.Services[sn].Domains {
			if host, _ := manifest.SplitDomain(d); host == name {
				return false
			}
		}
	}
	return true
}

func cleanName(n string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(n), "."))
}
