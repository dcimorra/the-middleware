package auth

import (
	"slices"
	"strings"
	"testing"
)

// The tasks scope opens a site's own task routes and none of the house's:
// /tasks is every task of the machine, root and other sites included.
func TestTheTasksScopeOpensItsSitesTaskRoutesOnly(t *testing.T) {
	site := func(s string) bool { return !strings.ContainsAny(s, " /") }
	if err := CheckScopes([]string{"tasks:a.test", "tasks"}, site); err != nil {
		t.Fatalf("tasks refused: %v", err)
	}
	if !slices.Contains(Scopes(), "tasks") {
		t.Fatalf("the panel's list has no tasks: %v", Scopes())
	}
	if slices.Contains(Scopes(), "import") {
		t.Fatal("import is made for one site, never picked from the list")
	}
	scopes := []string{"tasks:a.test"}
	for _, p := range []string{
		"GET /api/v1/sites/{domain}/tasks",
		"PUT /api/v1/sites/{domain}/tasks/{name}",
		"DELETE /api/v1/sites/{domain}/tasks/{name}",
		"POST /api/v1/sites/{domain}/tasks/{name}/run",
		"POST /api/v1/sites/{domain}/tasks/{name}/stop",
		"POST /api/v1/sites/{domain}/tasks/{name}/disable",
		"POST /api/v1/sites/{domain}/tasks/{name}/enable",
		"GET /api/v1/sites/{domain}/tasks/{name}/log",
	} {
		if !Allows(scopes, p, "a.test") {
			t.Errorf("%s is closed to its own site", p)
		}
		if Allows(scopes, p, "b.test") {
			t.Errorf("%s is open to another site", p)
		}
	}
	for _, p := range []string{"GET /api/v1/tasks", "PUT /api/v1/tasks/{name}", "POST /api/v1/tasks/{name}/run", "GET /api/v1/sites/{domain}", "POST /api/v1/sites/{domain}/restart"} {
		if Allows(scopes, p, "a.test") {
			t.Errorf("tasks:a.test opens %s", p)
		}
	}
}

// A site's agent turn manages its own site's tasks, and the rest of what
// it opens is what it opened before: no other site, no import.
func TestATurnManagesItsOwnSitesTasks(t *testing.T) {
	got := TurnScopes("a.test")
	want := []string{"read:a.test", "restart:a.test", "deploy:a.test", "exec:a.test", "tasks:a.test", "backups:a.test"}
	if !slices.Equal(got, want) {
		t.Fatalf("TurnScopes: %v, want %v", got, want)
	}
	// Its database copies, its own site's only: the agent is asked to set
	// them up and to put one back.
	if !Allows(got, "POST /api/v1/sites/{domain}/backups/{name}/copies/{id}/restore", "a.test") || Allows(got, "GET /api/v1/sites/{domain}/backups", "b.test") {
		t.Fatal("a turn's backups are not its own site's")
	}
	// exec, so a docker site's turn runs its commands through the house
	// rather than through a docker socket it must never have; its own
	// site's only.
	if !Allows(got, "POST /api/v1/sites/{domain}/exec", "a.test") || Allows(got, "POST /api/v1/sites/{domain}/exec", "b.test") {
		t.Fatal("a turn's exec is not its own site's")
	}
	if !Allows(got, "PUT /api/v1/sites/{domain}/tasks/{name}", "a.test") || Allows(got, "PUT /api/v1/sites/{domain}/tasks/{name}", "b.test") {
		t.Fatal("a turn's tasks are not its own site's")
	}
	if Allows(got, "PUT /api/v1/tasks/{name}", "") || Allows(got, "PUT /api/v1/relocate/{domain}", "a.test") {
		t.Fatal("a turn reaches the house's tasks or an import")
	}
}
