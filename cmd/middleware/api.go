package main

import (
	"github.com/dcimorra/the-middleware/internal/api"
	"github.com/dcimorra/the-middleware/internal/app"
	"github.com/dcimorra/the-middleware/internal/cli"
)

// The API hook and the operator CLI. The hook answers the panel host and
// the unix socket; the CLI commands talk to it.
// apiHook is the API of the running process; wire.go adds the routes of
// the other packages to its server.
var apiHook *api.Hook

func init() {
	hookBuilders = append(hookBuilders, hookBuilder{Name: "api", Order: orderAPI, Build: func(a *app.App) app.Hook {
		apiHook = api.NewHook(a, Version)
		return apiHook
	}})
	for _, name := range []string{"setup", "site", "events", "system", "token", "nodes", "autopilot"} {
		subcommands[name] = cli.Command(name)
	}
	extraUsage += cli.Usage
}
