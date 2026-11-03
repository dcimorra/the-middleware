package auth

import (
	"errors"
	"fmt"
	"strings"
)

// A token with scopes is a service door: it opens a closed list of
// routes, optionally for one site, and nothing else. It exists for the
// programs that act on the house from outside (an app that builds a site
// and needs to restart it, a deploy script that reads logs), which V1
// gave a full token or a door of their own written route by route.
// A token without scopes is the operator's, and opens everything, as
// every token did before scopes existed.
//
// The list is closed and lives here, so what a scope opens is read in one
// place; the API asks Allows with the route it matched, never the caller.

// scopeRoutes are the routes each scope opens, as the API registers them.
var scopeRoutes = map[string][]string{
	"read": {
		"GET /api/v1/sites",
		"GET /api/v1/sites/{domain}",
		"GET /api/v1/sites/{domain}/logs",
		"POST /api/v1/sites/{domain}/validate",
		"GET /api/v1/sites/{domain}/routes",
		"GET /api/v1/events",
	},
	"restart": {
		"POST /api/v1/sites/{domain}/restart",
		"POST /api/v1/sites/{domain}/workers/{name}/restart",
		"POST /api/v1/sites/{domain}/services/{name}/restart",
	},
	"deploy": {
		"POST /api/v1/sites/{domain}/deploy",
	},
	// exec is a one-off command where the site runs, inside its container
	// or a service's: what a site's turn needs for a docker site, whose
	// jail has no docker socket and must never have one (the socket is
	// root on the machine). The house runs it with the container's own
	// limits, as `middleware exec` would; it is no wider than deploy,
	// which already runs the site's code there.
	"exec": {
		"POST /api/v1/sites/{domain}/exec",
	},
	// tasks is a site's own scheduled work: the tasks whose site is that
	// one, and nothing under /tasks, which is every task of the house. The
	// routes check the task is the site's; the scope only lets the caller
	// ask. A site that can deploy its code already decides what runs in
	// its folder, and a task runs there and nowhere else.
	"tasks": {
		"GET /api/v1/sites/{domain}/tasks",
		"PUT /api/v1/sites/{domain}/tasks/{name}",
		"DELETE /api/v1/sites/{domain}/tasks/{name}",
		"POST /api/v1/sites/{domain}/tasks/{name}/run",
		"POST /api/v1/sites/{domain}/tasks/{name}/stop",
		"POST /api/v1/sites/{domain}/tasks/{name}/disable",
		"POST /api/v1/sites/{domain}/tasks/{name}/enable",
		"GET /api/v1/sites/{domain}/tasks/{name}/log",
	},
	// backups is a site's database copies: list them, take one, read the
	// log, download, delete and restore one. What is copied and when is
	// the manifest's, which the site writes already. The site's agent is
	// asked for "a copy of the database every night" and for "put back
	// yesterday's", and does both here.
	"backups": {
		"GET /api/v1/sites/{domain}/backups",
		"POST /api/v1/sites/{domain}/backups/{name}/run",
		"GET /api/v1/sites/{domain}/backups/{name}/log",
		"GET /api/v1/sites/{domain}/backups/{name}/copies/{id}",
		"DELETE /api/v1/sites/{domain}/backups/{name}/copies/{id}",
		"POST /api/v1/sites/{domain}/backups/{name}/copies/{id}/restore",
	},
	// import is a site arriving from another installation (V1): its
	// bundle, its last copy, the commit, and seeing whether it came up or
	// removing it when it did not. Always for one site, and made to
	// expire (POST /relocate/import-token).
	"import": {
		"POST /api/v1/relocate/{domain}/check",
		"PUT /api/v1/relocate/{domain}",
		"PUT /api/v1/relocate/{domain}/delta",
		"POST /api/v1/relocate/{domain}/prepare",
		"GET /api/v1/relocate/{domain}/prepare",
		"POST /api/v1/relocate/{domain}/migrate",
		"GET /api/v1/relocate/{domain}/migrate",
		"POST /api/v1/relocate/{domain}/commit",
		"POST /api/v1/relocate/{domain}/placed",
		"POST /api/v1/relocate/{domain}/repair",
		"GET /api/v1/relocate/{domain}/repair",
		"POST /api/v1/sites/{domain}/publish",
		"DELETE /api/v1/relocate/{domain}",
		"GET /api/v1/sites/{domain}",
		"DELETE /api/v1/sites/{domain}",
	},
}

// Scopes is the closed list, for the panel and for the message that
// refuses a scope outside it. import is left out: it is made for one
// site by POST /relocate/import-token, never picked from a list.
func Scopes() []string { return []string{"read", "restart", "deploy", "exec", "tasks", "backups"} }

// TurnScopes are what a site's agent turn opens when it calls the house
// over the socket (the middleware CLI from one of its Bash calls): what a
// service door for that site opens, and not tokens, settings, the house's
// conversation or any other site. The turn runs as the supervisor's user,
// so the socket used to take it for the operator. Its site's tasks are
// in: before, the skill had the agent propose the task JSON and ask the
// person to add it, which made the person the courier of a file that
// runs in the folder the agent already writes and deploys. The routes
// keep it to tasks that run there.
func TurnScopes(site string) []string {
	return []string{"read:" + site, "restart:" + site, "deploy:" + site, "exec:" + site, "tasks:" + site, "backups:" + site}
}

// ErrBadScope is a scope outside the list.
var ErrBadScope = errors.New("scope")

// CheckScopes validates "action" or "action:site" entries.
func CheckScopes(scopes []string, validSite func(string) bool) error {
	for _, sc := range scopes {
		action, site, hasSite := strings.Cut(sc, ":")
		if _, ok := scopeRoutes[action]; !ok {
			return fmt.Errorf("%w: %q is not one of %s", ErrBadScope, sc, strings.Join(Scopes(), ", "))
		}
		if hasSite && !validSite(site) {
			return fmt.Errorf("%w: %q names no valid site", ErrBadScope, sc)
		}
		if action == "import" && !hasSite {
			return fmt.Errorf("%w: import is for one site (import:<domain>)", ErrBadScope)
		}
	}
	return nil
}

// Allows says whether scopes open the route pattern (as the mux matched
// it, "POST /api/v1/sites/{domain}/restart") for the site in it, if any.
// Empty scopes open everything.
func Allows(scopes []string, pattern, site string) bool {
	if len(scopes) == 0 {
		return true
	}
	for _, sc := range scopes {
		action, only, hasSite := strings.Cut(sc, ":")
		if hasSite && only != site {
			continue
		}
		for _, p := range scopeRoutes[action] {
			if p == pattern {
				return true
			}
		}
	}
	return false
}

// ScopeRoutes is every route a scope opens, for the test that checks
// each one is still registered.
func ScopeRoutes() []string {
	var out []string
	for _, rs := range scopeRoutes {
		out = append(out, rs...)
	}
	return out
}
