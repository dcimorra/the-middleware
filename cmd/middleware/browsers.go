package main

import (
	"path/filepath"
	"time"

	"github.com/dcimorra/the-middleware/internal/api"
	"github.com/dcimorra/the-middleware/internal/app"
	"github.com/dcimorra/the-middleware/internal/browsers"
	"github.com/dcimorra/the-middleware/internal/cgroups"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/shots"
)

// wireBrowsers builds the node's pool of browsers for its sites and
// mounts its routes. The browser is the one the pictures use, looked for
// at each start of one, so a browser the installer puts there later is
// used without a restart; the size and the idle time are read from the
// settings each time, so a change in the panel applies at once.
func wireBrowsers(a *app.App, srv *api.Server) *browsers.Pool {
	machine := cgroups.ThisMachine()
	pool, err := browsers.New(browsers.Options{
		Dir:    filepath.Join(dataDir(a), browsers.Dir),
		Bus:    a.Bus,
		Binary: func() string { return shots.FindIn(a.Root) },
		Count: func() int {
			n, _ := a.CurrentSettings().Browsers.In(machine)
			return n
		},
		Idle: func() time.Duration { return a.CurrentSettings().Browsers.Idle() },
	})
	if err != nil {
		a.Bus.Emit(events.Event{Type: "browsers.error", Actor: "engine", Level: "error", Message: "the sites' browsers are off: " + err.Error()})
		return nil
	}
	srv.RegisterBrowsers(pool, machine)
	return pool
}
