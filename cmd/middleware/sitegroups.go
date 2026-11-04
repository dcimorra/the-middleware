package main

import (
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/dcimorra/the-middleware/internal/app"
	"github.com/dcimorra/the-middleware/internal/cgroups"
	"github.com/dcimorra/the-middleware/internal/events"
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

// wireSiteGroups hands the sites' branch to the runtime, takes away the
// groups of sites deleted while the supervisor was down, and the group of
// a site removed from now on once its processes are gone.
func wireSiteGroups(a *app.App, tree *cgroups.Tree) {
	for _, n := range tree.Notes() {
		a.Bus.Emit(events.Event{Type: "cgroups.sites", Actor: "engine", Level: "warn", Message: n})
	}
	if !tree.Sites() {
		return
	}
	runtime.SiteGroups = &siteGroups{tree: tree, bus: a.Bus, said: map[string]bool{}}
	sitesDir := filepath.Join(a.Root, "sites")
	exists := func(site string) bool {
		st, err := os.Stat(filepath.Join(sitesDir, site))
		return err == nil && st.IsDir()
	}
	tree.SweepSites(exists)
	ch, _ := a.Bus.Subscribe()
	go func() {
		for e := range ch {
			if e.Type != "site.removed" || e.Subject == "" {
				continue
			}
			domain := e.Subject
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
