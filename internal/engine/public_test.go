package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dcimorra/the-middleware/internal/manifest"
)

// fakeHost is the house as the engine asks it about public ports: an
// address, the owner's yes by site, and what was opened.
type fakeHost struct {
	mu       sync.Mutex
	ip       string
	approved map[string]bool
	reserved []manifest.HeldPort
	opened   [][]string
}

func (h *fakeHost) PublicIP() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ip
}

func (h *fakeHost) Pending(domain string, m *manifest.Manifest) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.approved[domain] {
		return nil
	}
	return m.PublicGrant()
}

func (h *fakeHost) Reserved() []manifest.HeldPort { return h.reserved }

func (h *fakeHost) Open(_ context.Context, ports []manifest.PublicPort) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	list := make([]string, len(ports))
	for i, p := range ports {
		list[i] = p.String()
	}
	h.opened = append(h.opened, list)
	return nil
}

func (h *fakeHost) set(f func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f()
}

func (h *fakeHost) lastOpened() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.opened) == 0 {
		return "never"
	}
	return strings.Join(h.opened[len(h.opened)-1], ",")
}

// writeRelay writes a site with an app on its name and a "relay" beside
// it that asks for a public port and reads the node's address: the test
// binary again, which says the address it was started with.
func writeRelay(t *testing.T, sites, domain, port string) {
	t.Helper()
	dir := filepath.Join(sites, domain)
	os.MkdirAll(dir, 0o755)
	m := fmt.Sprintf(`{"version":2,"services":{
		"web":{"run":{"command":[%q],"env":{"MW_TEST_SERVER":"1","BUILD":"web"},"health":{"timeout":"10s"}},"domains":[%q]},
		"relay":{"run":{"command":[%q],"env":{"MW_TEST_SERVER":"1","BUILD":"relay at $MIDDLEWARE_PUBLIC_IP"},"health":{"timeout":"10s"}},"ports":[%q]}}}`,
		os.Args[0], domain, os.Args[0], port)
	if err := os.WriteFile(filepath.Join(dir, "site.json"), []byte(m), 0o644); err != nil {
		t.Fatal(err)
	}
}

func relayOf(e *Engine, domain string) ServiceSnapshot {
	sn, _ := e.Site(domain)
	return sn.Services["relay"]
}

func waitRelay(t *testing.T, e *Engine, domain string, cond func(ServiceSnapshot) bool) ServiceSnapshot {
	t.Helper()
	waitFor(t, e, domain, func(sn Snapshot) bool { return cond(sn.Services["relay"]) })
	return relayOf(e, domain)
}

// A service with public ports does not start on the word of a file in
// the site's folder: it waits for the owner, the rest of the site up
// meanwhile, starts with the node's address when approved, and stops
// when the approval is taken back.
func TestPublicPortsWaitForTheOwner(t *testing.T) {
	e, sites := newTestEngine(t)
	host := &fakeHost{ip: "203.0.113.7", approved: map[string]bool{}}
	e.SetPublic(host)
	writeRelay(t, sites, "p2p.test", "5000/udp")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)

	site := waitFor(t, e, "p2p.test", func(sn Snapshot) bool { return sn.Alive && sn.Services["relay"].Stopped == stoppedApproval })
	if r := site.Services["relay"]; r.Alive || r.Pending != "5000/udp" || site.Error != "" || site.Health == HealthDown {
		t.Fatalf("waiting for approval: relay %+v, site error %q health %v (the site itself is up)", r, site.Error, site.Health)
	}
	if got := host.lastOpened(); got != "" && got != "never" {
		t.Fatalf("a port was opened before the owner said yes: %s", got)
	}
	if err := e.RestartService(ctx, "p2p.test", "relay"); err == nil || !strings.Contains(err.Error(), "approve") {
		t.Fatalf("a restart started what waits for approval: %v", err)
	}

	host.set(func() { host.approved["p2p.test"] = true })
	r := waitRelay(t, e, "p2p.test", func(r ServiceSnapshot) bool { return r.Alive })
	if got := getBody(t, r.Port); got != "relay at 203.0.113.7" {
		t.Fatalf("the relay was started with %q", got)
	}
	if r.PublicIP != "203.0.113.7" || len(r.Ports) != 1 || r.Stopped != "" {
		t.Fatalf("its snapshot: %+v", r)
	}
	waitFor(t, e, "p2p.test", func(Snapshot) bool { return host.lastOpened() == "5000/udp" })

	// The node's address changes (a move, a new lease): a new generation.
	host.set(func() { host.ip = "198.51.100.9" })
	waitRelay(t, e, "p2p.test", func(r ServiceSnapshot) bool { return r.Alive && getBody(t, r.Port) == "relay at 198.51.100.9" })

	host.set(func() { host.approved["p2p.test"] = false })
	waitRelay(t, e, "p2p.test", func(r ServiceSnapshot) bool { return !r.Alive && r.Stopped == stoppedApproval })
	waitFor(t, e, "p2p.test", func(Snapshot) bool { return host.lastOpened() == "" })
	if web, ok := e.LookupPath("p2p.test", "/"); !ok || !web.Alive {
		t.Fatalf("the app went down with the relay's approval: %+v", web)
	}
}

// Without the node's address, a service that reads it waits and says so;
// nothing is started with the variable still a variable.
func TestAServiceWaitsForTheNodesAddress(t *testing.T) {
	e, sites := newTestEngine(t)
	host := &fakeHost{approved: map[string]bool{"p2p.test": true}}
	e.SetPublic(host)
	writeRelay(t, sites, "p2p.test", "5000/udp")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)
	r := waitRelay(t, e, "p2p.test", func(r ServiceSnapshot) bool { return r.Error != "" })
	if r.Alive || !strings.Contains(r.Error, "public address") {
		t.Fatalf("without an address: %+v", r)
	}
	host.set(func() { host.ip = "203.0.113.7" })
	waitRelay(t, e, "p2p.test", func(r ServiceSnapshot) bool { return r.Alive && r.Error == "" })
}

// The first site to hold a port keeps it; the second says who has it,
// and a port of the house's own is refused to both.
func TestAPublicPortHasOneSite(t *testing.T) {
	e, sites := newTestEngine(t)
	host := &fakeHost{ip: "203.0.113.7", approved: map[string]bool{"a.test": true, "b.test": true, "c.test": true},
		reserved: []manifest.HeldPort{{From: 51820, To: 51821, What: "the VPN"}}}
	e.SetPublic(host)
	writeRelay(t, sites, "a.test", "5000-5010/udp")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)
	waitRelay(t, e, "a.test", func(r ServiceSnapshot) bool { return r.Alive })

	writeRelay(t, sites, "b.test", "5005/udp")
	b := waitRelay(t, e, "b.test", func(r ServiceSnapshot) bool { return r.Error != "" })
	if b.Alive || !strings.Contains(b.Error, "a.test has 5000-5010/udp") {
		t.Fatalf("the second site: %+v", b)
	}
	if !relayOf(e, "a.test").Alive {
		t.Fatal("the first site lost its port to the second")
	}
	writeRelay(t, sites, "c.test", "51820/udp")
	c := waitRelay(t, e, "c.test", func(r ServiceSnapshot) bool { return r.Error != "" })
	if c.Alive || !strings.Contains(c.Error, "the VPN") {
		t.Fatalf("the house's own port: %+v", c)
	}
	// The same number on the other protocol is another port.
	writeRelay(t, sites, "b.test", "5005/tcp")
	waitRelay(t, e, "b.test", func(r ServiceSnapshot) bool { return r.Alive })
}

// A house nobody wired for public ports starts none: the rest of the
// site runs.
func TestWithoutAHostPublicPortsStayClosed(t *testing.T) {
	e, sites := newTestEngine(t)
	writeRelay(t, sites, "p2p.test", "5000/udp")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)
	site := waitFor(t, e, "p2p.test", func(sn Snapshot) bool { return sn.Alive })
	if r := site.Services["relay"]; r.Alive || r.Stopped != stoppedApproval {
		t.Fatalf("relay %+v", r)
	}
}
