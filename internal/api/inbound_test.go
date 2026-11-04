package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/acme"
	"github.com/dcimorra/the-middleware/internal/certs"
	"github.com/dcimorra/the-middleware/internal/inbound"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/publish"
)

// fakeDNS records what the publisher asked it to write.
type fakeDNS struct {
	mu          sync.Mutex
	enabled     bool
	published   []string
	unpublished []string
	// outside are names in a zone the provider does not have.
	outside map[string]bool
}

func (d *fakeDNS) Enabled() bool { return d.enabled }
func (d *fakeDNS) Describe(_ context.Context, domains []string) (map[string]DNSInfo, error) {
	out := map[string]DNSInfo{}
	for _, dom := range domains {
		out[dom] = DNSInfo{DNS: "CNAME tunnel.example", TunnelRoute: true}
	}
	return out, nil
}
func (d *fakeDNS) Publish(_ context.Context, domain, door, ip string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.published = append(d.published, domain+" "+door+" "+ip)
	return nil
}
func (d *fakeDNS) Unpublish(_ context.Context, domain, ip string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.unpublished = append(d.unpublished, domain)
	return nil
}
func (d *fakeDNS) Covers(_ context.Context, name string) (bool, error) { return !d.outside[name], nil }
func (d *fakeDNS) calls() (pub, unpub []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.published...), append([]string(nil), d.unpublished...)
}

// inboundFixture wires the real publisher over a fake DNS. The names are
// x.test plus whatever site the test creates, as the engine would say.
func inboundFixture(t *testing.T, dns *fakeDNS) (*fixture, *inbound.Hook) {
	t.Helper()
	f := newFixture(t).withToken()
	store := certs.New(filepath.Join(f.root, "data", "certs"), nil)
	h := inbound.New(f.root, store, acme.NewHTTP01())
	hosts := func() map[string]string {
		out := map[string]string{"x.test": "x.test"}
		entries, _ := os.ReadDir(filepath.Join(f.root, "sites"))
		for _, e := range entries {
			if m, err := manifest.Load(filepath.Join(f.root, "sites", e.Name())); err == nil {
				for _, n := range m.Hosts(e.Name()) {
					out[n] = e.Name()
				}
			}
		}
		return out
	}
	h.Sites = func() []string {
		var out []string
		for n := range hosts() {
			out = append(out, n)
		}
		return out
	}
	var pubDNS publish.DNS
	var apiDNS DNSProvider
	if dns != nil {
		pubDNS, apiDNS = dns, dns
	}
	pub, err := publish.New(publish.Options{Root: f.root, Bus: f.bus, Doors: h, Hosts: hosts,
		DNS:      func() publish.DNS { return pubDNS },
		PublicIP: func(context.Context) (string, error) { return "203.0.113.10", nil }})
	if err != nil {
		t.Fatal(err)
	}
	f.srv.RegisterInbound(InboundOptions{Hook: h, DNS: apiDNS, Publisher: pub})
	return f, h
}

func TestInboundStatusCarriesTheDNSColumnsWhenTheProviderIsOn(t *testing.T) {
	f, _ := inboundFixture(t, &fakeDNS{enabled: true})
	resp, body := f.call("GET", "/api/v1/inbound", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d %s", resp.StatusCode, text(body))
	}
	doors := body["doors"].(map[string]any)
	if doors["tunnel"].(map[string]any)["open"] != true {
		t.Fatalf("fresh state: tunnel must be open: %v", doors)
	}
	rows := body["domains"].([]any)
	if len(rows) != 1 {
		t.Fatalf("rows: %v", rows)
	}
	row := rows[0].(map[string]any)
	if row["domain"] != "x.test" || row["dns"] != "CNAME tunnel.example" || row["tunnel_route"] != "yes" {
		t.Fatalf("row: %v", row)
	}
}

func TestInboundStatusLeavesTheDNSColumnsEmptyWhenTheProviderIsOff(t *testing.T) {
	f, _ := inboundFixture(t, &fakeDNS{enabled: false})
	_, body := f.call("GET", "/api/v1/inbound", nil)
	row := body["domains"].([]any)[0].(map[string]any)
	if row["dns"] != "" || row["tunnel_route"] != "" {
		t.Fatalf("columns must be empty: %v", row)
	}
}

func TestMovingToTheTunnelMarksTheDomainAndPublishesDNS(t *testing.T) {
	dns := &fakeDNS{enabled: true}
	f, h := inboundFixture(t, dns)
	resp, body := f.call("POST", "/api/v1/inbound/move", map[string]any{"domains": []string{"x.test", "nope.test"}, "to": "tunnel"})
	if resp.StatusCode != 200 {
		t.Fatalf("move: %d %s", resp.StatusCode, text(body))
	}
	if body["ok"] != false {
		t.Fatalf("a domain that is not a site must fail the move: %v", body)
	}
	if h.State().DoorOf("x.test") != inbound.Tunnel {
		t.Fatal("x.test was not marked")
	}
	if pub, _ := dns.calls(); len(pub) != 1 || pub[0] != "x.test tunnel " {
		t.Fatalf("published: %v (only the marked domain, and only once)", pub)
	}
	resp, _ = f.call("POST", "/api/v1/inbound/move", map[string]any{"domains": []string{"x.test"}, "to": "garage"})
	if resp.StatusCode != 400 {
		t.Fatalf("unknown door: %d", resp.StatusCode)
	}
}

func TestMovingToThePortsDoorWithoutAnIPSkipsTheDNSStepAndSaysWhy(t *testing.T) {
	dns := &fakeDNS{enabled: true}
	f, h := inboundFixture(t, dns)
	// Without an issuer the certificate cannot be obtained and the domain
	// must not be marked, let alone published.
	resp, body := f.call("POST", "/api/v1/inbound/move", map[string]any{"domains": []string{"x.test"}, "to": "ports"})
	if resp.StatusCode != 200 || body["ok"] != false {
		t.Fatalf("move without issuer: %d %v", resp.StatusCode, body)
	}
	if pub, _ := dns.calls(); h.State().DoorOf("x.test") == inbound.Ports || len(pub) != 0 {
		t.Fatalf("a domain without a certificate must not be marked or published: %v", dns.published)
	}
	if !strings.Contains(text(body), "certificate not obtained") {
		t.Fatalf("lines must say why: %s", text(body))
	}
}

func TestDoorsOpenAndCloseThroughTheAPI(t *testing.T) {
	f, h := inboundFixture(t, nil)
	resp, body := f.call("POST", "/api/v1/inbound/doors/tunnel", map[string]any{"open": false})
	if resp.StatusCode != 200 {
		t.Fatalf("close: %d %s", resp.StatusCode, text(body))
	}
	if h.State().Tunnel.Open {
		t.Fatal("tunnel still open")
	}
	if st, err := inbound.LoadState(f.root); err != nil || st.Tunnel.Open {
		t.Fatalf("the decision must be on disk: %v %v", st, err)
	}
	resp, _ = f.call("POST", "/api/v1/inbound/doors/window", map[string]any{"open": true})
	if resp.StatusCode != 400 {
		t.Fatalf("unknown door: %d", resp.StatusCode)
	}
	resp, body = f.call("GET", "/api/v1/inbound/certificates", nil)
	if resp.StatusCode != 200 || strings.TrimSpace(text(body)) != "[]" {
		t.Fatalf("certificates: %d %q", resp.StatusCode, text(body))
	}
	resp, _ = f.call("POST", "/api/v1/inbound/certificates/x.test/renew", nil)
	if resp.StatusCode != 502 {
		t.Fatalf("renew without an issuer must be 502, got %d", resp.StatusCode)
	}
}

func TestACreatedSiteIsPublishedAndADeletedOneIsTakenBack(t *testing.T) {
	dns := &fakeDNS{enabled: true}
	f, _ := inboundFixture(t, dns)
	resp, body := f.call("POST", "/api/v1/sites", map[string]any{"domain": "new.test", "template": "static"})
	if resp.StatusCode != 201 || body["publishing"] != true {
		t.Fatalf("create: %d %v", resp.StatusCode, body)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		pub, _ := dns.calls()
		if strings.Join(pub, ",") == "new.test tunnel ,www.new.test tunnel " {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("published: %v", pub)
		}
		time.Sleep(100 * time.Millisecond)
	}
	resp, body = f.call("GET", "/api/v1/publish", nil)
	if resp.StatusCode != 200 || body["new.test"].(map[string]any)["state"] != "published" {
		t.Fatalf("records: %v", body)
	}
	resp, body = f.call("DELETE", "/api/v1/sites/new.test?archive=true", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("delete: %d %v", resp.StatusCode, body)
	}
	if _, unpub := dns.calls(); strings.Join(unpub, ",") != "new.test,www.new.test" {
		t.Fatalf("unpublished: %v", unpub)
	}
	// Asking not to publish is honoured.
	resp, body = f.call("POST", "/api/v1/sites", map[string]any{"domain": "quiet.test", "publish": false})
	if resp.StatusCode != 201 || body["publishing"] != nil {
		t.Fatalf("create without publishing: %d %v", resp.StatusCode, body)
	}
}

func TestTheDoorForNewSitesAndThePublicAddressAreSetThroughTheAPI(t *testing.T) {
	f, h := inboundFixture(t, &fakeDNS{enabled: true})
	resp, body := f.call("PUT", "/api/v1/inbound/publishing", map[string]any{"new_sites": "ports", "public_ip": "198.51.100.4"})
	if resp.StatusCode != 200 || h.State().DoorForNew() != inbound.Ports || h.State().PublicIP != "198.51.100.4" {
		t.Fatalf("set: %d %v", resp.StatusCode, body)
	}
	resp, _ = f.call("PUT", "/api/v1/inbound/publishing", map[string]any{"public_ip": "2001:db8::1"})
	if resp.StatusCode != 400 {
		t.Fatalf("an IPv6 address for an A record: %d", resp.StatusCode)
	}
	resp, _ = f.call("PUT", "/api/v1/inbound/publishing", map[string]any{"public_ip": ""})
	if resp.StatusCode != 200 || h.State().PublicIP != "" {
		t.Fatal("clearing the address")
	}
	if st, _ := inbound.LoadState(f.root); st.NewSites != inbound.Ports {
		t.Fatal("the door for new sites is not on disk")
	}
}

func TestAMigrationMovesOneDomainAtATimeAndStopsWhenAsked(t *testing.T) {
	f := newFixture(t).withToken()
	release := make(chan struct{})
	// entered says the migration is on a domain. Cancelling right after
	// releasing a.test raced the start of b.test: a cancel seen before it
	// began moved one domain, and the send for b.test waited for ever.
	entered := make(chan string, 3)
	var moved []string
	var mu sync.Mutex
	f.srv.registerMigration(func(ctx context.Context, d string, to inbound.Door) []inbound.Line {
		entered <- d
		<-release
		mu.Lock()
		moved = append(moved, d)
		mu.Unlock()
		return []inbound.Line{{Status: "ok", Text: d + " on " + string(to)}}
	})
	resp, body := f.call("POST", "/api/v1/inbound/migration", map[string]any{"domains": []string{"a.test", "b.test", "c.test"}, "to": "ports"})
	if resp.StatusCode != 202 || body["running"] != true {
		t.Fatalf("start: %d %v", resp.StatusCode, body)
	}
	if resp, _ := f.call("POST", "/api/v1/inbound/migration", map[string]any{"domains": []string{"x.test"}, "to": "ports"}); resp.StatusCode != 409 {
		t.Fatalf("a second one: %d", resp.StatusCode)
	}
	<-entered
	release <- struct{}{} // a.test
	if d := <-entered; d != "b.test" {
		t.Fatalf("second domain %q", d)
	}
	f.call("POST", "/api/v1/inbound/migration/cancel", nil)
	release <- struct{}{} // b.test, the one it was on
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, st := f.call("GET", "/api/v1/inbound/migration", nil)
		if st["running"] == false {
			if st["cancelled"] != true || st["done"].(float64) != 2 {
				t.Fatalf("after cancel: %v", st)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("it did not stop")
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(moved) != 2 {
		t.Fatalf("moved %v", moved)
	}
}
