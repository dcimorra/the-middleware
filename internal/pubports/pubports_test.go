package pubports

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dcimorra/the-middleware/internal/manifest"
)

func site(t *testing.T, services string) *manifest.Manifest {
	t.Helper()
	m, err := manifest.Parse([]byte(`{"version":2,"services":{"web":{"run":{"command":["x"]},"domains":["a.test"]},` + services + `}}`))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

const relay = `"turn":{"run":{"runtime":"docker","docker":{"image":"coturn/coturn:4.6.3"}},"network":"host","ports":["3478/udp","49160-49400/udp"]}`

// What the manifest asks waits until the owner says yes, and a yes is for
// what was asked then: a port added later waits on its own.
func TestApprovalIsForWhatWasAsked(t *testing.T) {
	b := &Book{Root: t.TempDir()}
	m := site(t, relay)
	if got := b.Pending("a.test", m); len(got) != 3 {
		t.Fatalf("pending before any approval: %v", got)
	}
	added, err := b.Approve("a.test", m)
	if err != nil || len(added) != 3 {
		t.Fatalf("approve: %v, %v", added, err)
	}
	if got := b.Pending("a.test", m); len(got) != 0 {
		t.Fatalf("pending after the approval: %v", got)
	}
	// Another book on the same folder reads it: it is on disk.
	if got := (&Book{Root: b.Root}).Pending("a.test", m); len(got) != 0 {
		t.Fatalf("the approval did not reach the disk: %v", got)
	}
	// Someone adds a port to the file: only that one waits.
	more := site(t, strings.Replace(relay, `"3478/udp"`, `"3478/udp","5349/tcp"`, 1))
	if got := b.Pending("a.test", more); len(got) != 1 || got[0] != "turn 5349/tcp" {
		t.Fatalf("pending after a port was added: %v", got)
	}
	// Another site's name has nothing approved.
	if got := b.Pending("b.test", m); len(got) != 3 {
		t.Fatalf("an approval crossed to another site: %v", got)
	}
	// Approving a manifest that asks less takes back the rest.
	less := site(t, `"turn":{"run":{"runtime":"docker","docker":{"image":"coturn/coturn:4.6.3"}},"ports":["3478/udp"]}`)
	if _, err := b.Approve("a.test", less); err != nil {
		t.Fatal(err)
	}
	if got := b.Pending("a.test", m); len(got) != 2 {
		t.Fatalf("what was taken out of the manifest stayed approved: %v", got)
	}
	if err := b.Revoke("a.test"); err != nil {
		t.Fatal(err)
	}
	if got := b.Pending("a.test", less); len(got) != 1 {
		t.Fatalf("pending after revoke: %v", got)
	}
	// A manifest that asks nothing leaves no file behind.
	if _, err := b.Approve("a.test", site(t, `"x":{"run":{"command":["x"]}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(b.Root, "data", Dir, "a.test.json")); !os.IsNotExist(err) {
		t.Fatalf("a file for a site that asks nothing: %v", err)
	}
}

// A file moved under the book (a rename, a move that arrives) is read
// again once the site is released.
func TestReleaseReadsTheFileAgain(t *testing.T) {
	b := &Book{Root: t.TempDir()}
	m := site(t, relay)
	if _, err := b.Approve("a.test", m); err != nil {
		t.Fatal(err)
	}
	if got := b.Pending("b.test", m); len(got) != 3 {
		t.Fatal(got)
	}
	dir := filepath.Join(b.Root, "data", Dir)
	if err := os.Rename(filepath.Join(dir, "a.test.json"), filepath.Join(dir, "b.test.json")); err != nil {
		t.Fatal(err)
	}
	b.Release("a.test")
	b.Release("b.test")
	if got := b.Pending("b.test", m); len(got) != 0 {
		t.Fatalf("the renamed site lost its approval: %v", got)
	}
	if got := b.Pending("a.test", m); len(got) != 3 {
		t.Fatalf("the old name kept an approval: %v", got)
	}
}

func TestAPortOfTheHousesOrAnotherSitesIsRefused(t *testing.T) {
	m := site(t, relay)
	others := map[string]*manifest.Manifest{
		"a.test": m, // itself, not a conflict
		"b.test": site(t, `"game":{"run":{"command":["x"]},"ports":["49200/udp"]}`),
		"c.test": site(t, `"dns":{"run":{"runtime":"docker","docker":{"image":"a:1"}},"ports":["3478/tcp"]}`), // another protocol
	}
	reserved := []manifest.HeldPort{{From: 51820, To: 51821, What: "the VPN"}}
	got := Conflicts("a.test", m, others, reserved, false)
	if len(got) != 1 || !strings.Contains(got[0], "b.test has 49200/udp") {
		t.Fatalf("conflicts: %v", got)
	}
	// Who has it is not said to someone who sees one site.
	if got := Conflicts("a.test", m, others, reserved, true); len(got) != 1 || strings.Contains(got[0], "b.test") {
		t.Fatalf("hidden: %v", got)
	}
	vpn := site(t, `"x":{"run":{"command":["x"]},"ports":["51800-51830/udp"]}`)
	if got := Conflicts("d.test", vpn, nil, reserved, false); len(got) != 1 || !strings.Contains(got[0], "the VPN") {
		t.Fatalf("the house's port: %v", got)
	}
	// A stopped service holds nothing.
	others["b.test"].Services["game"].Disabled = true
	if got := Conflicts("a.test", m, others, nil, false); len(got) != 0 {
		t.Fatalf("a stopped service's port is taken: %v", got)
	}
}

func TestBoundFindsWhatAlreadyListens(t *testing.T) {
	c, err := net.ListenPacket("udp", ":0")
	if err != nil {
		t.Skip(err)
	}
	defer c.Close()
	taken := c.LocalAddr().(*net.UDPAddr).Port
	got := Bound([]manifest.PublicPort{{From: taken, To: taken, Proto: "udp"}, {From: 1, To: 1, Proto: "tcp"}})
	if len(got) != 1 || !strings.HasSuffix(got[0], "/udp") {
		t.Fatalf("bound: %v (a port below 1024 that cannot be tried is not one in use)", got)
	}
}
