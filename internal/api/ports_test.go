package api

import (
	"context"
	"encoding/json"
	"github.com/dcimorra/the-middleware/internal/relocate"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/auth"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/pubports"
)

func relayManifest(domain string, ports ...string) map[string]any {
	return map[string]any{"version": 2, "services": map[string]any{
		"web":  map[string]any{"run": map[string]any{"command": []string{"node", "server.js"}}, "domains": []string{domain}},
		"turn": map[string]any{"run": map[string]any{"runtime": "docker", "docker": map[string]any{"image": "coturn/coturn:4.6.3"}}, "network": "host", "ports": ports},
	}}
}

func (f *fixture) withPorts() *pubports.Book {
	book := &pubports.Book{Root: f.root}
	f.srv.RegisterPublicPorts(PortsOptions{Book: book,
		Reserved: func() []manifest.HeldPort { return []manifest.HeldPort{{From: 51820, To: 51821, What: "the VPN"}} },
		PublicIP: func() string { return "203.0.113.7" }})
	return book
}

func asked(t *testing.T, out map[string]any) map[string]bool {
	t.Helper()
	var p SitePorts
	if err := json.Unmarshal([]byte(text(out)), &p); err != nil {
		t.Fatalf("%v: %s", err, text(out))
	}
	got := map[string]bool{}
	for _, l := range p.Asked {
		got[l.Line] = l.Approved
	}
	return got
}

// The owner's own save approves what it asks. What reaches the file any
// other way (the site's agent, a shell) waits, and only the owner's yes
// lets it through: not a scoped token's, not a save of another section.
func TestPublicPortsAreTheOwnersToGive(t *testing.T) {
	f := newFixture(t).withToken()
	f.withPorts()
	f.makeSite("p2p.test")
	if res, out := f.call("PUT", "/api/v1/sites/p2p.test/manifest", relayManifest("p2p.test", "3478/udp", "49160-49400/udp")); res.StatusCode != 200 {
		t.Fatalf("the owner's manifest: %d %s", res.StatusCode, text(out))
	}
	res, out := f.call("GET", "/api/v1/sites/p2p.test/ports", nil)
	got := asked(t, out)
	if res.StatusCode != 200 || len(got) != 3 || !got["turn 3478/udp"] || !got["turn network=host"] || out["public_ip"] != "203.0.113.7" {
		t.Fatalf("after the owner's save: %d %s", res.StatusCode, text(out))
	}

	// Written into the file, as the site's agent would.
	path := filepath.Join(f.root, "sites", "p2p.test", "site.json")
	raw, _ := json.Marshal(relayManifest("p2p.test", "3478/udp", "49160-49400/udp", "5349/tcp"))
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	_, out = f.call("GET", "/api/v1/sites/p2p.test/ports", nil)
	if got := asked(t, out); got["turn 5349/tcp"] || !got["turn 3478/udp"] {
		t.Fatalf("a port written into the file is approved: %v", got)
	}
	// The owner saving the access form is not a yes to it.
	if res, out := f.call("PATCH", "/api/v1/sites/p2p.test/manifest", map[string]any{"title": "P2P"}); res.StatusCode != 200 {
		t.Fatalf("patch: %d %s", res.StatusCode, text(out))
	}
	_, out = f.call("GET", "/api/v1/sites/p2p.test/ports", nil)
	if got := asked(t, out); got["turn 5349/tcp"] {
		t.Fatalf("saving another section approved the port: %v", got)
	}
	// Nor is a service token's, whatever it may otherwise do there.
	owner := f.token
	_, body := f.call("POST", "/api/v1/auth/tokens", map[string]any{"name": "ci", "scopes": []string{"read", "restart:p2p.test", "deploy:p2p.test"}})
	f.token = body["token"].(string)
	if res, _ := f.call("POST", "/api/v1/sites/p2p.test/ports/approve", nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("a scoped token approved: %d", res.StatusCode)
	}
	if res, out := f.call("GET", "/api/v1/sites/p2p.test/ports", nil); res.StatusCode != 200 || asked(t, out)["turn 5349/tcp"] {
		t.Fatalf("a scoped token reads what is asked: %d %s", res.StatusCode, text(out))
	}
	f.token = owner
	res, out = f.call("POST", "/api/v1/sites/p2p.test/ports/approve", nil)
	if got := asked(t, out); res.StatusCode != 200 || !got["turn 5349/tcp"] {
		t.Fatalf("the owner's approval: %d %s", res.StatusCode, text(out))
	}
	res, out = f.call("POST", "/api/v1/sites/p2p.test/ports/revoke", nil)
	if got := asked(t, out); res.StatusCode != 200 || got["turn 3478/udp"] {
		t.Fatalf("revoke: %d %s", res.StatusCode, text(out))
	}
}

// A delegate who may write the site's code may not give it a port of the
// machine, nor move its container to the machine's network.
func TestADelegateCannotOpenAPort(t *testing.T) {
	f := newFixture(t).withToken()
	f.withPorts()
	f.makeSite("p2p.test")
	if res, out := f.call("PUT", "/api/v1/sites/p2p.test/manifest", relayManifest("p2p.test", "3478/udp")); res.StatusCode != 200 {
		t.Fatalf("%d %s", res.StatusCode, text(out))
	}
	f.makeUser("bob", auth.Grant{Site: "p2p.test", Perms: []string{"code", "operate"}})
	bob := f.login("bob")
	res, out := bob.call("PUT", "/api/v1/sites/p2p.test/manifest", relayManifest("p2p.test", "3478/udp", "5349/tcp"))
	if res.StatusCode != http.StatusForbidden || !strings.Contains(text(out), "services.turn.ports") {
		t.Fatalf("a delegate added a port: %d %s", res.StatusCode, text(out))
	}
	bridged := relayManifest("p2p.test", "3478/udp")
	delete(bridged["services"].(map[string]any)["turn"].(map[string]any), "network")
	res, out = bob.call("PUT", "/api/v1/sites/p2p.test/manifest", bridged)
	if res.StatusCode != http.StatusForbidden || !strings.Contains(text(out), "services.turn.network") {
		t.Fatalf("a delegate changed the network: %d %s", res.StatusCode, text(out))
	}
	if res, _ := bob.call("POST", "/api/v1/sites/p2p.test/ports/approve", nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("a delegate approved: %d", res.StatusCode)
	}
	if res, out := bob.call("GET", "/api/v1/sites/p2p.test/ports", nil); res.StatusCode != 200 {
		t.Fatalf("a delegate cannot see what their site asks: %d %s", res.StatusCode, text(out))
	}
	// The same manifest, nothing locked changed, is theirs to save.
	same := relayManifest("p2p.test", "3478/udp")
	same["title"] = "by bob"
	if res, out := bob.call("PUT", "/api/v1/sites/p2p.test/manifest", same); res.StatusCode != 200 {
		t.Fatalf("a delegate's save that changes no port: %d %s", res.StatusCode, text(out))
	}
}

// A port another site has, or the house itself, is refused when the
// manifest is written and when a site asks to move in.
func TestATakenPortIsRefusedBeforeItIsWritten(t *testing.T) {
	f := newFixture(t).withToken()
	f.withPorts()
	f.makeSite("a.test")
	f.makeSite("b.test")
	if res, out := f.call("PUT", "/api/v1/sites/a.test/manifest", relayManifest("a.test", "3478/udp", "49160-49400/udp")); res.StatusCode != 200 {
		t.Fatalf("%d %s", res.StatusCode, text(out))
	}
	res, out := f.call("PUT", "/api/v1/sites/b.test/manifest", relayManifest("b.test", "49300/udp"))
	if res.StatusCode != http.StatusConflict || !strings.Contains(text(out), "a.test has 49160-49400/udp") {
		t.Fatalf("another site's port: %d %s", res.StatusCode, text(out))
	}
	res, out = f.call("PUT", "/api/v1/sites/b.test/manifest", relayManifest("b.test", "51820/udp"))
	if res.StatusCode != http.StatusConflict || !strings.Contains(text(out), "the VPN") {
		t.Fatalf("the house's port: %d %s", res.StatusCode, text(out))
	}
	if res, out := f.call("PUT", "/api/v1/sites/b.test/manifest", relayManifest("b.test", "3478/tcp")); res.StatusCode != 200 {
		t.Fatalf("the other protocol is another port: %d %s", res.StatusCode, text(out))
	}
	res, out = f.call("POST", "/api/v1/relocate/c.test/check", map[string]any{"public": []string{"3478/udp"}})
	if res.StatusCode != http.StatusConflict || !strings.Contains(text(out), "a.test has 3478/udp") {
		t.Fatalf("a site moving in with a taken port: %d %s", res.StatusCode, text(out))
	}
	if res, out := f.call("POST", "/api/v1/relocate/c.test/check", map[string]any{"public": []string{"6000/udp"}}); res.StatusCode != 200 {
		t.Fatalf("a site moving in with a free port: %d %s", res.StatusCode, text(out))
	}
}

// upTarget is a server whose sites come up: these fixtures have no engine
// to start one.
type upTarget struct{ httpTarget }

func (upTarget) Ready(context.Context, string) (bool, string, error) { return true, "", nil }

// A site moved between two servers arrives with what its owner approved
// (nobody is there to approve it again), and what the source says stayed
// behind is said where the site now is.
func TestAMovedSiteKeepsItsPortsAndHearsWhatStayedBehind(t *testing.T) {
	a, b := newFixture(t).withToken(), newFixture(t).withToken()
	a.withPorts()
	onB := b.withPorts()
	a.makeSite("p2p.test")
	m := map[string]any{"version": 2, "services": map[string]any{
		"web":   map[string]any{"run": map[string]any{"command": []string{"node", "server.js"}}, "domains": []string{"p2p.test"}},
		"relay": map[string]any{"run": map[string]any{"command": []string{"./relay"}}, "ports": []string{"6000-6010/udp"}},
	}}
	if res, out := a.call("PUT", "/api/v1/sites/p2p.test/manifest", m); res.StatusCode != 200 {
		t.Fatalf("%d %s", res.StatusCode, text(out))
	}
	mgr, _ := a.srv.sitesManager()
	mv := &relocate.Mover{Root: a.root, Source: siteSource{s: a.srv, mgr: mgr}, Poll: 20 * time.Millisecond, ReadyWait: 5 * time.Second}
	if _, err := mv.Move(context.Background(), "p2p.test", upTarget{httpTarget{f: b}}, nil); err != nil {
		t.Fatal(err)
	}
	moved, err := manifest.Load(filepath.Join(b.root, "sites", "p2p.test"))
	if err != nil {
		t.Fatal(err)
	}
	if pending := onB.Pending("p2p.test", moved); len(pending) != 0 {
		t.Fatalf("it arrived waiting for an approval nobody is there to give: %v", pending)
	}
	if _, err := os.Stat(filepath.Join(a.root, "data", "ports", "p2p.test.json")); err == nil {
		t.Fatal("the approval stayed on the node the site left")
	}

	ch, _, stop := b.bus.SubscribeRecent()
	defer stop()
	if res, out := b.call("POST", "/api/v1/relocate/p2p.test/notes", map[string]any{"warnings": []string{"infra/docker-compose.yml is not read by the house"}}); res.StatusCode != 200 {
		t.Fatalf("notes: %d %s", res.StatusCode, text(out))
	}
	select {
	case e := <-ch:
		if e.Type != "relocate.incomplete" || e.Subject != "p2p.test" || !strings.Contains(e.Message, "infra/docker-compose.yml") {
			t.Fatalf("event: %+v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("nothing was said on the node the site arrived at")
	}
}
