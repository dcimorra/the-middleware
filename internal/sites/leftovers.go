package sites

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/tasks"
)

// What the house keeps about a site outside its folder, by its name. A
// delete that removed only the folder left all of it behind for good, and
// a rename that carried only part of it left the site, under its new
// name, without its availability, its visits or the copies of its
// database, while the tasks that ran in it failed at every run looking
// for a folder that was no longer called that.

// named are the folders under data/ that hold a folder "<domain>" or a
// file "<domain>.json" for a site: both are looked for in each, since a
// site's instance has a file where its workers have a folder
// (data/resources) and nothing is lost by asking.
var named = []string{
	"homes", "history", "chat", "deploys", "resources", "status", "seen",
	"visits", "webvitals", "canary", "audit", "ports",
	filepath.Join("agents", "cost"), filepath.Join("agents", "config"),
}

// accessLog is the name the access logs of a site begin with, the one
// being written and the weeks turned over (access.log.2026-W45.gz).
const accessLog = "access.log"

// owned is every path named after domain under data/, but its logs and
// the copies of its database.
func (m *Manager) owned(domain string) []string {
	data := filepath.Join(m.Root, "data")
	var out []string
	for _, parent := range named {
		out = append(out, filepath.Join(data, parent, domain), filepath.Join(data, parent, domain+".json"))
	}
	out = append(out, filepath.Join(data, "shots", domain+".png"))
	// A conversation's waiting messages are filed by its name: the site's
	// first is "<domain>", the others "<domain>#<id>".
	queued, _ := filepath.Glob(filepath.Join(data, "chat", "queued", domain+"#*.json"))
	return append(append(out, filepath.Join(data, "chat", "queued", domain+".json")), queued...)
}

// release has whoever keeps counts of domain in memory write them and
// let go, so that what is moved or removed next is not written again
// under the old name a minute later.
func (m *Manager) release(domain string) {
	if m.Release != nil {
		m.Release(domain)
	}
}

// forget removes what the house kept about a site that was deleted, and
// the tasks that ran in it. Two things stay: its access logs (dropLogs)
// and the copies of its database (data/backups), which are the one thing
// someone who deleted a site by mistake comes back for.
func (m *Manager) forget(domain string) {
	m.release(domain)
	for _, p := range m.owned(domain) {
		_ = os.RemoveAll(p)
	}
	m.dropLogs(domain)
	mine, _ := tasks.OfSite(m.Root, domain)
	for name := range mine {
		if os.Remove(tasks.Path(m.Root, name)) != nil {
			continue
		}
		_ = os.Remove(tasks.RecordPath(m.Root, name))
		_ = os.Remove(tasks.LogPath(m.Root, name))
		m.emit("task.deleted", name, "", "its site was deleted", map[string]any{"site": domain})
	}
}

// dropLogs removes what a site's processes wrote and keeps what the house
// wrote of who called it: the access logs are kept by law whatever
// happens to the site, and both deleting and archiving one took them.
func (m *Manager) dropLogs(domain string) {
	logs := m.logsDir(domain)
	entries, _ := os.ReadDir(logs)
	kept := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), accessLog) {
			kept = true
			continue
		}
		_ = os.RemoveAll(filepath.Join(logs, e.Name()))
	}
	if !kept {
		_ = os.Remove(logs)
	}
}

// carry takes what the house kept about from to its new name, and points
// the tasks that ran in it at the folder it has now. Nothing already
// under the new name is written over: it is some earlier site's, and
// which of the two is right is not something to decide here.
func (m *Manager) carry(from, to string) {
	m.release(from)
	m.release(to)
	data := filepath.Join(m.Root, "data")
	move := func(src, dst string) {
		if fsx.Exists(src) && !fsx.Exists(dst) {
			_ = os.Rename(src, dst)
		}
	}
	for _, src := range m.owned(from) {
		rel, err := filepath.Rel(data, src)
		if err != nil {
			continue
		}
		dir, base := filepath.Split(rel)
		move(src, filepath.Join(data, dir, to+strings.TrimPrefix(base, from)))
	}
	move(m.logsDir(from), m.logsDir(to))
	move(filepath.Join(data, "backups", from), filepath.Join(data, "backups", to))
	mine, _ := tasks.OfSite(m.Root, from)
	for name := range mine {
		t, err := tasks.Load(m.Root, name)
		if err != nil {
			continue
		}
		t.Site = to
		if tasks.Save(m.Root, name, t) == nil {
			m.emit("task.saved", name, "", "its site is now "+to, map[string]any{"schedule": t.Schedule, "site": to})
		}
	}
}
