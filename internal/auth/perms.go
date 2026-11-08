package auth

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// A delegated user is someone the operator lets manage some sites and
// nothing else: their own sites, their tasks, their conversations. What
// each permission opens is a closed list of routes, as for scopes, and it
// lives here so it is read in one place. Every route under
// /sites/{domain} is in exactly one of these lists or in ownerOnly; the
// wired process's test fails when one is added without being placed, so
// a new route is the operator's until someone decides otherwise rather
// than open to every delegate the day it ships.
//
// view is implied by every grant: a site you may restart but not look at
// is a button with no screen.

// The permissions a grant can carry.
const (
	PermView    = "view"
	PermOperate = "operate"
	PermCode    = "code"
	PermAgents  = "agents"
)

// Perms is the closed list, in the order the panel shows it.
func Perms() []string { return []string{PermView, PermOperate, PermCode, PermAgents} }

const siteBase = "/api/v1/sites/{domain}"

var permRoutes = map[string][]string{
	PermView: {
		"GET " + siteBase,
		"GET " + siteBase + "/logs",
		"POST " + siteBase + "/validate",
		"GET " + siteBase + "/routes",
		"GET " + siteBase + "/manifest/history",
		"GET " + siteBase + "/manifest/history/{id}",
		"GET " + siteBase + "/files",
		"GET " + siteBase + "/file",
		"GET " + siteBase + "/specs",
		"GET " + siteBase + "/env",
		"GET " + siteBase + "/shot.png",
		"GET " + siteBase + "/cache",
		"GET " + siteBase + "/security/seen",
		"GET " + siteBase + "/visits",
		"GET " + siteBase + "/webvitals",
		"GET " + siteBase + "/uptime",
		"GET " + siteBase + "/canary",
		"GET " + siteBase + "/security/blocks",
		"GET " + siteBase + "/resources",
		"GET " + siteBase + "/download.zip",
		"GET " + siteBase + "/tasks",
		"GET " + siteBase + "/tasks/{name}/log",
		"GET " + siteBase + "/backups",
		"GET " + siteBase + "/backups/{name}/log",
	},
	// operate changes the site's state, never what it runs.
	PermOperate: {
		"POST " + siteBase + "/restart",
		"POST " + siteBase + "/canary/promote",
		"POST " + siteBase + "/canary/rollback",
		"POST " + siteBase + "/stop",
		"POST " + siteBase + "/start",
		"POST " + siteBase + "/hold",
		"POST " + siteBase + "/workers/{name}/restart",
		"POST " + siteBase + "/services/{name}/restart",
		"POST " + siteBase + "/services/{name}/stop",
		"POST " + siteBase + "/services/{name}/start",
		"POST " + siteBase + "/services/{name}/hold",
		"POST " + siteBase + "/workers/{name}/stop",
		"POST " + siteBase + "/workers/{name}/start",
		"POST " + siteBase + "/shot",
		"POST " + siteBase + "/audit",
		"DELETE " + siteBase + "/cache",
		"POST " + siteBase + "/security/seen/forget",
		"POST " + siteBase + "/tasks/{name}/run",
		"POST " + siteBase + "/tasks/{name}/stop",
		"POST " + siteBase + "/tasks/{name}/disable",
		"POST " + siteBase + "/tasks/{name}/enable",
		"POST " + siteBase + "/backups/{name}/run",
	},
	// code decides what runs: the files, the manifest, the build, the
	// tasks and a command where the site runs. Everything here runs as
	// the supervisor's user unless the site is in a container, and a
	// task always does (docs/security.md, "Delegated users").
	PermCode: {
		"PUT " + siteBase + "/manifest",
		"PATCH " + siteBase + "/manifest",
		"POST " + siteBase + "/manifest/history/{id}/restore",
		"PUT " + siteBase + "/file",
		"POST " + siteBase + "/deploy",
		"POST " + siteBase + "/exec",
		// The database browser: what the site keeps, read where it is.
		"GET " + siteBase + "/databases",
		"GET " + siteBase + "/databases/tables",
		"POST " + siteBase + "/databases/query",
		"POST " + siteBase + "/specs/init",
		"PUT " + siteBase + "/tasks/{name}",
		"DELETE " + siteBase + "/tasks/{name}",
		// A copy is the database's whole content, and a restore replaces
		// it: what code already reaches through exec, and no less.
		"GET " + siteBase + "/backups/{name}/copies/{id}",
		"DELETE " + siteBase + "/backups/{name}/copies/{id}",
		"POST " + siteBase + "/backups/{name}/copies/{id}/restore",
	},
	// agents is the site's conversations, which spend the operator's
	// subscriptions.
	PermAgents: {
		"GET " + siteBase + "/agents",
		"GET " + siteBase + "/chat",
		"GET " + siteBase + "/chat/export",
		"POST " + siteBase + "/chat",
		"DELETE " + siteBase + "/chat",
		"POST " + siteBase + "/chat/stop",
		"POST " + siteBase + "/chat/reap",
		"GET " + siteBase + "/chat/processes",
		"POST " + siteBase + "/chat/processes/{pid}/kill",
		"GET " + siteBase + "/chat/files",
		"POST " + siteBase + "/chat/files",
		"DELETE " + siteBase + "/chat/files/{file}",
		"GET " + siteBase + "/chats",
		"POST " + siteBase + "/chats",
		"DELETE " + siteBase + "/chats",
		"GET " + siteBase + "/chats/{id}",
		"GET " + siteBase + "/chats/{id}/export",
		"POST " + siteBase + "/chats/{id}",
		"DELETE " + siteBase + "/chats/{id}",
		"POST " + siteBase + "/chats/{id}/stop",
		"POST " + siteBase + "/chats/{id}/reap",
		"GET " + siteBase + "/chats/{id}/processes",
		"POST " + siteBase + "/chats/{id}/processes/{pid}/kill",
		"GET " + siteBase + "/chats/{id}/files",
		"POST " + siteBase + "/chats/{id}/files",
		"DELETE " + siteBase + "/chats/{id}/files/{file}",
	},
}

// ownerOnly are the site routes no grant opens: the site's existence and
// name, where it lives, the names it is published under (the operator's
// DNS), its container and its LLM gateway (the operator's money). A
// delegate who could move a site would learn the nodes; one who could
// rename it could take a name that is not theirs.
var ownerOnly = []string{
	"DELETE " + siteBase,
	"POST " + siteBase + "/rename",
	"POST " + siteBase + "/names/{name}/main",
	"DELETE " + siteBase + "/names/{name}",
	"POST " + siteBase + "/publish",
	"POST " + siteBase + "/unpublish",
	"GET " + siteBase + "/containerize",
	"POST " + siteBase + "/containerize",
	"GET " + siteBase + "/move",
	// Its cluster names the other nodes, as a move does.
	"GET " + siteBase + "/cluster",
	"GET " + siteBase + "/move/plan",
	"POST " + siteBase + "/move",
	"GET " + siteBase + "/claude-api",
	"POST " + siteBase + "/claude-api",
	// Its budget on the machine, and the machine's size with it: the
	// owner's to set (the manifest's limits are locked for a delegate).
	"GET " + siteBase + "/limits",
	// A test copy is a new site, and promoting one rewrites production.
	"GET " + siteBase + "/staging",
	"POST " + siteBase + "/staging",
	"POST " + siteBase + "/staging/promote",
	"POST " + siteBase + "/staging/undo",
	"POST " + siteBase + "/staging/refresh",
}

// userRoutes are the routes outside a site a delegate reaches: the
// session, the list of their sites and the events about them. The
// handlers narrow each answer to the caller's sites.
var userRoutes = []string{
	"POST /api/v1/auth/logout",
	"PUT /api/v1/auth/password",
	"GET /api/v1/sites",
	"GET /api/v1/events",
	"GET /api/v1/events/stream",
}

// Grant is what a user may do on one site.
type Grant struct {
	Site  string   `json:"site"`
	Perms []string `json:"perms"`
}

// ErrBadPerm is a permission outside the list, or a grant without a site.
var ErrBadPerm = errors.New("permission")

// CheckGrants validates and normalises grants: known permissions only,
// view added, one grant per site, sorted as Perms lists them.
func CheckGrants(gs []Grant, validSite func(string) bool) ([]Grant, error) {
	seen := map[string]bool{}
	out := make([]Grant, 0, len(gs))
	for _, g := range gs {
		g.Site = strings.ToLower(strings.TrimSpace(g.Site))
		if !validSite(g.Site) {
			return nil, fmt.Errorf("%w: %q is not a site", ErrBadPerm, g.Site)
		}
		if seen[g.Site] {
			return nil, fmt.Errorf("%w: %s is granted twice", ErrBadPerm, g.Site)
		}
		seen[g.Site] = true
		has := map[string]bool{PermView: true}
		for _, p := range g.Perms {
			if _, ok := permRoutes[p]; !ok {
				return nil, fmt.Errorf("%w: %q is not one of %s", ErrBadPerm, p, strings.Join(Perms(), ", "))
			}
			has[p] = true
		}
		var perms []string
		for _, p := range Perms() {
			if has[p] {
				perms = append(perms, p)
			}
		}
		out = append(out, Grant{Site: g.Site, Perms: perms})
	}
	return out, nil
}

// UserAllows says whether a user with grants may use pattern (as the mux
// matched it) for site, the {domain} in it or "".
func UserAllows(grants map[string][]string, pattern, site string) bool {
	if slices.Contains(userRoutes, pattern) {
		return true
	}
	perms, ok := grants[site]
	if site == "" || !ok {
		return false
	}
	for _, p := range append([]string{PermView}, perms...) {
		if slices.Contains(permRoutes[p], pattern) {
			return true
		}
	}
	return false
}

// SiteRoute says whether pattern is a site's route that some grant or
// the owner decides: what the wired process's test checks is placed.
func SiteRoute(pattern string) (placed bool) {
	if slices.Contains(ownerOnly, pattern) {
		return true
	}
	for _, rs := range permRoutes {
		if slices.Contains(rs, pattern) {
			return true
		}
	}
	return false
}

// PermRoutes is every route a permission or the owner decides, for the
// test that checks each one is still registered.
func PermRoutes() []string {
	out := append([]string(nil), ownerOnly...)
	for _, rs := range permRoutes {
		out = append(out, rs...)
	}
	return append(out, userRoutes...)
}

// NeedsJail says whether pattern makes the site run what a delegate
// wrote or asked for: code (files, the manifest, deploys, commands,
// tasks) and the conversations. A node that cannot build jails
// (internal/jail) closes these to delegates: there the code would run
// with the run of the house.
func NeedsJail(pattern string) bool {
	return slices.Contains(permRoutes[PermCode], pattern) || slices.Contains(permRoutes[PermAgents], pattern)
}
