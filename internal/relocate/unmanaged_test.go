package relocate

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/pubports"
	"github.com/dcimorra/the-middleware/internal/runtime"
)

// The case this exists for: a compose file under infra/ and a relay
// started from it by hand. The move has to say it stays behind.
func TestAMoveSaysWhatOfTheFolderItDoesNotCarry(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"infra/docker-compose.yml", "compose.yaml", "node_modules/pkg/docker-compose.yml", ".git/compose.yml", "a/b/c/d/compose.yml"} {
		os.MkdirAll(filepath.Join(dir, filepath.Dir(f)), 0o755)
		os.WriteFile(filepath.Join(dir, f), []byte("services: {}\n"), 0o644)
	}
	if got := ComposeFiles(dir); !slices.Equal(got, []string{"compose.yaml", "infra/docker-compose.yml"}) {
		t.Fatalf("compose files: %v", got)
	}
	m := &manifest.Manifest{Version: 2}
	got := Unmanaged(dir, m, nil)
	if len(got) != 1 || !strings.Contains(got[0], "infra/docker-compose.yml") || !strings.Contains(got[0], "does not move") {
		t.Fatalf("with a compose file and nothing running: %q", got)
	}
	got = Unmanaged(dir, m, []runtime.Foreign{{Name: "infra-coturn-1", Image: "coturn/coturn:4.6", Why: "compose project in infra"}})
	if len(got) != 1 || !strings.Contains(got[0], "infra-coturn-1") || !strings.Contains(got[0], "coturn/coturn:4.6") {
		t.Fatalf("with its container running: %q", got)
	}
	if got := Unmanaged(t.TempDir(), m, nil); len(got) != 0 {
		t.Fatalf("a folder with none of it: %q", got)
	}
}

// A site whose only container is a service still needs docker where it
// goes, and what its owner approved goes with it: on the node it arrives
// at nobody is there to approve, and the relay would wait stopped.
func TestAServicesPortsAndTheirApprovalTravel(t *testing.T) {
	m := &manifest.Manifest{Version: 2, Services: map[string]*manifest.Service{
		"web":  {Run: &manifest.Run{Command: []string{"node", "server.js"}}, Domains: []string{"p2p.test"}},
		"turn": {Run: &manifest.Run{Runtime: "docker", Docker: &manifest.Docker{Image: "coturn/coturn:4.6"}}, Network: "host", Ports: []string{"3478/udp", "49160-49400/udp"}},
	}}
	if !UsesDocker(m) {
		t.Fatal("a site with a container service does not count as using docker")
	}
	root := t.TempDir()
	book := &pubports.Book{Root: root}
	if _, err := book.Approve("p2p.test", m); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range Parts(root, "p2p.test") {
		if strings.HasSuffix(filepath.ToSlash(p.Path), "data/ports/p2p.test.json") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the approval is not among what a move carries: %+v", Parts(root, "p2p.test"))
	}
}
