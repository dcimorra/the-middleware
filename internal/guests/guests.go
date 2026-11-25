// Package guests answers which sites a site may enter.
//
// A site had no way into another: each is a jail holding its own folder
// and nothing else, and a site made to replace an old one could not read
// the code and the data it was replacing. The one door there was,
// chat.reads, was written in the manifest of the site that reads, and a
// site's agent writes its own manifest: the visitor signed its own pass.
//
// So the pass is the host's. A site lists, in its own manifest, the sites
// of this node it takes in (`guests`), each to read or to write, and
// nothing a guest writes in its own gives it anything. What a guest gets
// is the host's folder in every jail of its own (its conversation, its
// process, its tasks, its builds, its containers), and the house's
// commands about the host from its conversation (auth.GuestScopes).
//
// Nothing is kept outside the manifests: the answer is read from them
// each time it is asked, so a pass taken back is gone at the guest's next
// turn, and one written by hand in site.json counts like one made in the
// panel. A process that is already running keeps the mounts it started
// with: it sees a new pass, or loses one, at its next start.
package guests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

	"github.com/dcimorra/the-middleware/internal/manifest"
)

// Grant is one site that takes a guest in.
type Grant struct {
	Host string `json:"site"`
	// Dir is the host's folder, sites/<host>.
	Dir   string `json:"-"`
	Write bool   `json:"write"`
}

// Level is the manifest's word for g.
func (g Grant) Level() string {
	if g.Write {
		return manifest.GuestWrite
	}
	return manifest.GuestRead
}

// In are the guests siteDir's manifest names, as written. It is read
// straight from site.json and not through manifest.Load: a manifest the
// engine refuses for a typo elsewhere still says who its site lets in,
// and a guest in the middle of fixing things must not lose the folder to
// it. An entry that is not a site's name with "read" or "write" is no
// pass at all.
func In(siteDir string) map[string]string {
	data, err := os.ReadFile(filepath.Join(siteDir, manifest.FileName))
	if err != nil {
		return nil
	}
	var raw struct {
		Guests map[string]json.RawMessage `json:"guests"`
	}
	if json.Unmarshal(data, &raw) != nil {
		return nil
	}
	out := map[string]string{}
	for name, v := range raw.Guests {
		var level string
		if json.Unmarshal(v, &level) != nil || !manifest.ValidName(name) {
			continue
		}
		if level == manifest.GuestRead || level == manifest.GuestWrite {
			out[name] = level
		}
	}
	return out
}

// Of are the sites of root that take guest in, by name. A site is never
// its own guest, and a host is a folder under sites/ with a manifest: a
// name alone lends nothing.
func Of(root, guest string) []Grant {
	if guest == "" || !manifest.ValidName(guest) {
		return nil
	}
	sitesDir := filepath.Join(root, "sites")
	entries, err := os.ReadDir(sitesDir)
	if err != nil {
		return nil
	}
	var out []Grant
	for _, e := range entries {
		host := e.Name()
		if host == guest || !manifest.ValidName(host) {
			continue
		}
		dir := filepath.Join(sitesDir, host)
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			continue
		}
		if level, ok := In(dir)[guest]; ok {
			out = append(out, Grant{Host: host, Dir: dir, Write: level == manifest.GuestWrite})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

// Folders are the hosts' folders, those to read and those to write.
func Folders(grants []Grant) (read, write []string) {
	for _, g := range grants {
		if g.Write {
			write = append(write, g.Dir)
		} else {
			read = append(read, g.Dir)
		}
	}
	return read, write
}
