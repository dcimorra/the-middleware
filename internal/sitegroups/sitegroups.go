// Package sitegroups wires each site's cgroup (internal/cgroups) to the
// rest of the house: the runtime starts a site's processes in it, the
// engine's reconcile writes the manifest's limits into it, a removed
// site's group is taken away, and an OOM kill inside it is said on the
// bus.
package sitegroups

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/dcimorra/the-middleware/internal/cgroups"
	"github.com/dcimorra/the-middleware/internal/engine"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/runtime"
)

// siteGroups is the tree as the runtime sees it, with what fails said on
// the bus: a site that cannot have its group still runs (uncapped, in
// house/), and the operator learns why once per site rather than never.
type siteGroups struct {
	tree *cgroups.Tree
	bus  *events.Bus
	mu   sync.Mutex
	said map[string]bool
	// limits is each site's manifest block as last read, written is what
	// its group was last given, and kills the oom_kill count last seen.
	limits  map[string]*manifest.Limits
	written map[string]cgroups.SiteLimits
	kills   map[string]int64
}

// setManifest takes a site's block from a reconcile and applies it with
// the house's defaults; nothing is written while nothing changed.
func (g *siteGroups) setManifest(domain string, l *manifest.Limits, def *manifest.Limits) {
	g.mu.Lock()
	g.limits[domain] = l
	g.mu.Unlock()
	g.apply(domain, def)
}

func (g *siteGroups) apply(domain string, def *manifest.Limits) {
	g.mu.Lock()
	want := g.limits[domain].Over(def).Group()
	last, ok := g.written[domain]
	g.mu.Unlock()
	if ok && last == want {
		return
	}
	if err := g.tree.SetSite(domain, want); err != nil {
		g.warn(domain, "limits not applied", err)
		return
	}
	g.mu.Lock()
	g.written[domain] = want
	g.mu.Unlock()
	if ok {
		g.bus.Emit(events.Event{Type: "site.limits", Subject: domain, Actor: "engine", Message: "limits in force", Data: map[string]any{"limits": want}})
	}
}

// forget is a site gone: its next group starts from its manifest again.
func (g *siteGroups) forget(domain string) {
	g.mu.Lock()
	delete(g.limits, domain)
	delete(g.written, domain)
	delete(g.kills, domain)
	g.mu.Unlock()
}

// watch applies changed house defaults to every site and says each OOM
// kill inside a site's group: the kernel kills quietly, and a site whose
// process was killed at its ceiling would otherwise only look like one
// that crashed. The first reading of a group is its baseline, so kills
// from before this supervisor are not said again.
func (g *siteGroups) watch(def func() *manifest.Limits) {
	for {
		time.Sleep(Every)
		d := def()
		g.mu.Lock()
		var known []string
		for domain := range g.limits {
			known = append(known, domain)
		}
		g.mu.Unlock()
		for _, domain := range known {
			g.apply(domain, d)
		}
		for _, site := range g.tree.SiteNames() {
			m, ok := g.tree.Memory(site)
			if !ok {
				continue
			}
			g.mu.Lock()
			before, seen := g.kills[site]
			g.kills[site] = m.OOMKills
			g.mu.Unlock()
			if seen && m.OOMKills > before {
				g.bus.Emit(events.Event{Type: "site.oom", Subject: site, Actor: "engine", Level: "warn",
					Message: fmt.Sprintf("the kernel killed %d process(es) of the site at its memory limit (%s, held %s)", m.OOMKills-before, sizeOrNone(m.Max), size(m.Held)),
					Data:    map[string]any{"kills": m.OOMKills - before, "limit": m.Max, "held": m.Held}})
			}
		}
	}
}

func size(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%d MiB", n>>20)
	default:
		return fmt.Sprintf("%d KiB", n>>10)
	}
}

func sizeOrNone(n int64) string {
	if n <= 0 {
		return "no ceiling"
	}
	return size(n)
}

func (g *siteGroups) warn(domain, what string, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.said[domain+what] {
		return
	}
	g.said[domain+what] = true
	g.bus.Emit(events.Event{Type: "cgroups.site", Subject: domain, Actor: "engine", Level: "warn", Message: what + ": " + err.Error()})
}

func (g *siteGroups) SiteAttr(domain string) (*syscall.SysProcAttr, func(), error) {
	attr, release, err := g.tree.SiteAttr(domain)
	if err != nil {
		g.warn(domain, "started outside its site's group", err)
	}
	return attr, release, err
}

func (g *siteGroups) AdoptSite(domain string, pid int) error {
	err := g.tree.AdoptSite(domain, pid)
	if err != nil {
		g.warn(domain, "re-adopted but left in house/", err)
	}
	return err
}

// Every is how often the house's defaults are applied again and the
// groups read for OOM kills; tests shorten it.
var Every = 5 * time.Second

// Wire hands the sites' branch to the runtime and the engine, takes away
// the groups of sites deleted while the supervisor was down, and the
// group of a site removed from now on once its processes are gone. def
// is the settings' sites.default_limits as they are now.
func Wire(sitesDir string, bus *events.Bus, eng *engine.Engine, tree *cgroups.Tree, def func() *manifest.Limits) {
	for _, n := range tree.Notes() {
		bus.Emit(events.Event{Type: "cgroups.sites", Actor: "engine", Level: "warn", Message: n})
	}
	if !tree.Sites() {
		return
	}
	g := &siteGroups{tree: tree, bus: bus, said: map[string]bool{}, limits: map[string]*manifest.Limits{}, written: map[string]cgroups.SiteLimits{}, kills: map[string]int64{}}
	runtime.SiteGroups = g
	// Before a site's first process: the reconcile that starts it reads
	// the manifest first, and its group is made with these limits.
	eng.SetOnManifest(func(domain string, m *manifest.Manifest) { g.setManifest(domain, m.Limits, def()) })
	go g.watch(def)
	exists := func(site string) bool {
		st, err := os.Stat(filepath.Join(sitesDir, site))
		return err == nil && st.IsDir()
	}
	tree.SweepSites(exists)
	ch, _ := bus.Subscribe()
	go func() {
		for e := range ch {
			if e.Type != "site.removed" || e.Subject == "" {
				continue
			}
			domain := e.Subject
			g.forget(domain)
			// The engine stops the site's processes with a grace of
			// eight seconds after it says so: the group empties after.
			go func() {
				for i := 0; i < 30; i++ {
					time.Sleep(2 * time.Second)
					if exists(domain) || tree.RemoveSite(domain) == nil {
						return
					}
				}
			}()
		}
	}()
}
