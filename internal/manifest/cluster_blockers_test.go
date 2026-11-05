package manifest

import (
	"strings"
	"testing"
)

// The case that was refused: a Next app as a process with claude_api,
// beside a Postgres in docker whose volume keeps it on the primary.
func TestClaudeAPIBesideAKeptContainerCanHaveCopies(t *testing.T) {
	m := &Manifest{
		ClaudeAPI: &ClaudeAPI{},
		Cluster:   &Cluster{Replicas: 1},
		Services: map[string]*Service{
			"db":  {Run: &Run{Runtime: "docker", Docker: &Docker{Image: "postgres:17", Volumes: []string{"db:/var/lib/postgresql/data"}}}},
			"web": {Run: &Run{Command: []string{"next", "start"}}},
		},
	}
	if b := m.Blockers(); len(b) > 0 {
		t.Fatalf("refused: %v", b)
	}
	// A container the copies would run cannot reach the door.
	m.Services["api"] = &Service{Run: &Run{Runtime: "docker", Docker: &Docker{Image: "api:1"}}}
	if b := strings.Join(m.Blockers(), " "); !strings.Contains(b, "AI") {
		t.Fatalf("a copied container with claude_api was not refused: %q", b)
	}
	// Kept by name, it runs only here.
	m.Cluster.Keep = []string{"api"}
	if b := m.Blockers(); len(b) > 0 {
		t.Fatalf("refused with the container kept: %v", b)
	}
}
