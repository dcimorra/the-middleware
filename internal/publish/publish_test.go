package publish

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/inbound"
)

type fakeDNS struct {
	mu      sync.Mutex
	calls   []string
	zones   []string
	failFor map[string]error
	block   chan struct{}
}

func (d *fakeDNS) Covers(_ context.Context, name string) (bool, error) {
	for _, z := range d.zones {
		if name == z || strings.HasSuffix(name, "."+z) {
			return true, nil
		}
	}
	return false, nil
}

func (d *fakeDNS) Publish(_ context.Context, name, door, ip string) error {
	if d.block != nil {
		<-d.block
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.failFor[name]; err != nil {
		return err
	}
	d.calls = append(d.calls, fmt.Sprintf("publish %s %s %s", name, door, ip))
	return nil
}

func (d *fakeDNS) Unpublish(_ context.Context, name, ip string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, fmt.Sprintf("unpublish %s %s", name, ip))
	return nil
}

func (d *fakeDNS) log() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return strings.Join(d.calls, "\n")
}

type fakeDoors struct {
	mu       sync.Mutex
	st       inbound.State
	noCert   map[string]bool
	forgot   []string
	dnsCalls *fakeDNS
}

func (f *fakeDoors) Move(_ context.Context, domains []string, to inbound.Door) []inbound.Line {
	f.mu.Lock()
	defer f.mu.Unlock()
	var lines []inbound.Line
	for _, d := range domains {
		if to == inbound.Ports && f.noCert[d] {
			lines = append(lines, inbound.Line{Status: "fail", Text: d + ": certificate not obtained: CA said no"})
			continue
		}
		// The order matters: the door (and its certificate) before DNS.
		f.dnsCalls.mu.Lock()
		f.dnsCalls.calls = append(f.dnsCalls.calls, "door "+d+" "+string(to))
		f.dnsCalls.mu.Unlock()
		if f.st.Domains == nil {
			f.st.Domains = map[string]inbound.Door{}
		}
		f.st.Domains[d] = to
		lines = append(lines, inbound.Line{Status: "ok", Text: d + ": on the door"})
	}
	return lines
}

func (f *fakeDoors) State() inbound.State { f.mu.Lock(); defer f.mu.Unlock(); return f.st }
func (f *fakeDoors) Forget(d string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forgot = append(f.forgot, d)
	delete(f.st.Domains, d)
	return nil
}

type fixture struct {
	p     *Publisher
	dns   *fakeDNS
	doors *fakeDoors
	hosts map[string]string
	hmu   sync.Mutex
	now   time.Time
	bus   *events.Bus
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{dns: &fakeDNS{zones: []string{"example.com"}, failFor: map[string]error{}}, now: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC), bus: events.New("")}
	f.doors = &fakeDoors{noCert: map[string]bool{}, dnsCalls: f.dns}
	f.hosts = map[string]string{"a.example.com": "a.example.com", "www.a.example.com": "a.example.com", "b.example.com": "b.example.com"}
	var dns DNS = f.dns
	p, err := New(Options{
		Root: t.TempDir(), Bus: f.bus, Doors: f.doors,
		DNS: func() DNS { return dns },
		Hosts: func() map[string]string {
			f.hmu.Lock()
			defer f.hmu.Unlock()
			out := map[string]string{}
			for k, v := range f.hosts {
				out[k] = v
			}
			return out
		},
		PublicIP: func(context.Context) (string, error) { return "203.0.113.10", nil },
		Now:      func() time.Time { return f.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	f.p = p
	return f
}

func (f *fixture) setHosts(h map[string]string) { f.hmu.Lock(); f.hosts = h; f.hmu.Unlock() }

func TestPublishingASitePutsEveryNameOnTheDoorAndKeepsItThere(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	lines, err := f.p.Publish(ctx, "a.example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	want := "door a.example.com tunnel\npublish a.example.com tunnel \ndoor www.a.example.com tunnel\npublish www.a.example.com tunnel "
	if f.dns.log() != want {
		t.Fatalf("calls:\n%s\nwant:\n%s", f.dns.log(), want)
	}
	if len(lines) != 2 || lines[0].Status != "ok" {
		t.Fatalf("lines %+v", lines)
	}
	// Nothing to do on the next pass.
	f.dns.calls = nil
	f.p.Reconcile(ctx)
	if f.dns.log() != "" {
		t.Fatalf("a published site was published again: %s", f.dns.log())
	}
	// An alias added later is published; one removed is unpublished.
	f.setHosts(map[string]string{"a.example.com": "a.example.com", "shop.example.com": "a.example.com", "b.example.com": "b.example.com"})
	f.p.Reconcile(ctx)
	got := f.dns.log()
	if !strings.Contains(got, "publish shop.example.com tunnel") || !strings.Contains(got, "unpublish www.a.example.com") {
		t.Fatalf("aliases not followed:\n%s", got)
	}
	if _, ok := f.p.Records()["www.a.example.com"]; ok {
		t.Fatal("an unpublished name is still recorded")
	}
}

func TestThePortsDoorGetsItsCertificateBeforeItsDNS(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.doors.st.NewSites = inbound.Ports
	f.doors.noCert["www.a.example.com"] = true
	f.p.Publish(ctx, "a.example.com", "")
	log := f.dns.log()
	if !strings.Contains(log, "door a.example.com ports\npublish a.example.com ports 203.0.113.10") {
		t.Fatalf("calls:\n%s", log)
	}
	if strings.Contains(log, "publish www.a.example.com") {
		t.Fatal("DNS was written for a name whose certificate failed")
	}
	r := f.p.Records()["www.a.example.com"]
	if r.State != StateFailed || !strings.Contains(r.Error, "CA said no") || !r.Next.After(f.now) {
		t.Fatalf("record %+v", r)
	}
	// Before the backoff ends nothing is tried; after, it is.
	f.doors.noCert["www.a.example.com"] = false
	f.p.Reconcile(ctx)
	if f.p.Records()["www.a.example.com"].State != StateFailed {
		t.Fatal("retried before the backoff")
	}
	f.now = f.now.Add(2 * time.Minute)
	f.p.Reconcile(ctx)
	if r := f.p.Records()["www.a.example.com"]; r.State != StatePublished || r.IP != "203.0.113.10" {
		t.Fatalf("after the backoff: %+v", r)
	}
	// The override in the inbound state wins over detection.
	f.doors.st.PublicIP = "198.51.100.1"
	f.p.Publish(ctx, "b.example.com", inbound.Ports)
	if !strings.Contains(f.dns.log(), "publish b.example.com ports 198.51.100.1") {
		t.Fatalf("the public_ip override was not used:\n%s", f.dns.log())
	}
}

func TestANameOutsideTheZonesOrWithoutAProviderIsLeftToAPerson(t *testing.T) {
	f := newFixture(t)
	f.setHosts(map[string]string{"c.other.org": "c.other.org"})
	f.p.Publish(context.Background(), "c.other.org", "")
	if r := f.p.Records()["c.other.org"]; r.State != StateManual || !strings.Contains(r.Error, "by hand") {
		t.Fatalf("%+v", r)
	}
	g := newFixture(t)
	g.p.opts.DNS = func() DNS { return nil }
	g.p.Publish(context.Background(), "b.example.com", "")
	if r := g.p.Records()["b.example.com"]; r.State != StateManual {
		t.Fatalf("%+v", r)
	}
}

func TestUnpublishingRemovesEverythingAndADeletedFolderAloneRemovesNothing(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.p.Publish(ctx, "a.example.com", "")
	f.p.Publish(ctx, "b.example.com", "")
	// b's folder disappears by hand: its records stay, said once.
	ch, unsub := f.bus.Subscribe()
	defer unsub()
	f.setHosts(map[string]string{"a.example.com": "a.example.com", "www.a.example.com": "a.example.com"})
	f.dns.calls = nil
	f.p.Reconcile(ctx)
	f.p.Reconcile(ctx)
	if strings.Contains(f.dns.log(), "unpublish") {
		t.Fatalf("a vanished folder was unpublished: %s", f.dns.log())
	}
	orphans := 0
	for len(ch) > 0 {
		if (<-ch).Type == "publish.orphan" {
			orphans++
		}
	}
	if orphans != 1 {
		t.Fatalf("%d orphan events, want 1", orphans)
	}
	// Deleting through the house unpublishes every name.
	f.p.Unpublish(ctx, "a.example.com")
	log := f.dns.log()
	if !strings.Contains(log, "unpublish a.example.com") || !strings.Contains(log, "unpublish www.a.example.com") {
		t.Fatalf("calls:\n%s", log)
	}
	if len(f.doors.forgot) != 2 {
		t.Fatalf("doors forgotten: %v", f.doors.forgot)
	}
	if _, ok := f.p.Published("a.example.com"); ok {
		t.Fatal("the intent survived")
	}
}

func TestTheStateIsReadableWhileAPublishWaits(t *testing.T) {
	f := newFixture(t)
	f.dns.block = make(chan struct{})
	done := make(chan struct{})
	go func() {
		f.p.Publish(context.Background(), "b.example.com", "")
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	read := make(chan struct{})
	go func() { f.p.Records(); close(read) }()
	select {
	case <-read:
	case <-time.After(time.Second):
		t.Fatal("reading the state waited for a publish in flight")
	}
	close(f.dns.block)
	<-done
}

func TestFailuresBackOffUpToAnHour(t *testing.T) {
	f := newFixture(t)
	f.dns.failFor["b.example.com"] = errors.New("token lacks DNS:Edit")
	for i := 0; i < 10; i++ {
		f.p.Publish(context.Background(), "b.example.com", "")
		f.now = f.now.Add(2 * time.Hour)
	}
	r := f.p.Records()["b.example.com"]
	if r.State != StateFailed || r.Attempts < 5 || r.Next.Sub(f.now.Add(-2*time.Hour)) > time.Hour {
		t.Fatalf("%+v", r)
	}
}

func TestMovingOneNameKeepsItThereAndTheRestOnTheSiteDoor(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.p.Move(ctx, []string{"www.a.example.com"}, inbound.Ports)
	f.p.Reconcile(ctx)
	recs := f.p.Records()
	if recs["www.a.example.com"].State != StatePublished || recs["www.a.example.com"].Door != inbound.Ports {
		t.Fatalf("the moved name: %+v", recs["www.a.example.com"])
	}
	if recs["a.example.com"].Door != inbound.Ports {
		t.Fatalf("a site published by a move takes that door for its other names: %+v", recs["a.example.com"])
	}
	f.p.Publish(ctx, "b.example.com", inbound.Tunnel)
	f.setHosts(map[string]string{"a.example.com": "a.example.com", "www.a.example.com": "a.example.com", "b.example.com": "b.example.com", "shop.example.com": "b.example.com"})
	f.p.Move(ctx, []string{"b.example.com"}, inbound.Ports)
	f.p.Reconcile(ctx)
	recs = f.p.Records()
	if recs["b.example.com"].Door != inbound.Ports || recs["shop.example.com"].Door != inbound.Tunnel {
		t.Fatalf("a move changed more than the name moved: %+v / %+v", recs["b.example.com"], recs["shop.example.com"])
	}
	if strings.Contains(f.dns.log(), "unpublish") {
		t.Fatalf("a move unpublished something:\n%s", f.dns.log())
	}
}

func TestAVPNOnlySiteIsNeverPublishedAndOneThatBecomesItIsTakenDown(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	vpnOnly := false
	f.p.opts.VPNOnly = func(site string) bool { return vpnOnly && site == "a.example.com" }
	if _, err := f.p.Publish(ctx, "a.example.com", ""); err != nil {
		t.Fatal(err)
	}
	vpnOnly = true
	if _, err := f.p.Publish(ctx, "a.example.com", ""); err == nil || !strings.Contains(err.Error(), "VPN") {
		t.Fatalf("publishing a VPN-only site: %v", err)
	}
	f.p.Reconcile(ctx)
	if _, ok := f.p.Published("a.example.com"); ok {
		t.Fatal("a site that became VPN-only kept its public names")
	}
	if !strings.Contains(f.dns.log(), "unpublish a.example.com") {
		t.Fatalf("its record was not removed:\n%s", f.dns.log())
	}
	// Out of the VPN: published again, on the door it was on, by itself.
	vpnOnly = false
	f.p.Reconcile(ctx)
	if door, ok := f.p.Published("a.example.com"); !ok || door == "" {
		t.Fatal("a site that left the VPN was not published again")
	}
	written := 0
	for _, l := range strings.Split(f.dns.log(), "\n") {
		if strings.HasPrefix(l, "publish a.example.com") {
			written++
		}
	}
	if written != 2 {
		t.Fatalf("its record was not written again (want 2 writes):\n%s", f.dns.log())
	}
}

// inspectingDNS is a provider that can say what a name points at.
type inspectingDNS struct {
	*fakeDNS
	foreign map[string]string
	asked   []string
}

func (d *inspectingDNS) Foreign(_ context.Context, name string, ips ...string) (string, error) {
	d.asked = append(d.asked, name+" "+strings.Join(ips, ","))
	return d.foreign[name], nil
}

func TestANameThatPointsElsewhereWaitsForTheOperatorsYes(t *testing.T) {
	f := newFixture(t)
	ins := &inspectingDNS{fakeDNS: f.dns, foreign: map[string]string{"www.a.example.com": "CNAME shop.elsewhere.net"}}
	f.p.opts.DNS = func() DNS { return ins }
	ctx := context.Background()
	f.p.Publish(ctx, "a.example.com", "")
	if got := f.dns.log(); strings.Contains(got, "www.a.example.com") || !strings.Contains(got, "publish a.example.com tunnel") {
		t.Fatalf("calls:\n%s", got)
	}
	r := f.p.Records()["www.a.example.com"]
	if r.State != StateConflict || r.Found != "CNAME shop.elsewhere.net" {
		t.Fatalf("record %+v", r)
	}
	// The loop leaves it alone; only the operator takes it.
	f.dns.calls = nil
	f.p.Reconcile(ctx)
	if f.dns.log() != "" {
		t.Fatalf("a conflict was touched by the loop: %s", f.dns.log())
	}
	f.p.Takeover(ctx, []string{"www.a.example.com"})
	if got := f.dns.log(); got != "door www.a.example.com tunnel\npublish www.a.example.com tunnel " {
		t.Fatalf("takeover calls:\n%s", got)
	}
	if r := f.p.Records()["www.a.example.com"]; r.State != StatePublished || r.Found != "" || r.Takeover {
		t.Fatalf("after takeover %+v", r)
	}
	// The house's own address counts as the house's: asked with it.
	if !strings.Contains(strings.Join(ins.asked, "\n"), "a.example.com 203.0.113.10") {
		t.Fatalf("asked %v", ins.asked)
	}
}

// An import of a site that serves files is ready the moment its folder is
// in place, and its publish arrived before the engine had scanned it: the
// names stayed pointing at the node it had just been stopped on.
func TestASiteTheEngineHasNotSeenYetIsWaitedForBeforeItsNamesAreRefused(t *testing.T) {
	f := newFixture(t)
	f.p.opts.Settle = 5 * time.Second
	go func() {
		time.Sleep(300 * time.Millisecond)
		f.setHosts(map[string]string{"c.example.com": "c.example.com", "www.c.example.com": "c.example.com"})
	}()
	lines, err := f.p.PublishOver(context.Background(), "c.example.com", "")
	if err != nil {
		t.Fatalf("a site that arrives a moment late was refused: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("lines %+v", lines)
	}

	f.p.opts.Settle = 200 * time.Millisecond
	start := time.Now()
	if _, err := f.p.Publish(context.Background(), "nobody.example.com", ""); err == nil || !strings.Contains(err.Error(), "answers no name") {
		t.Fatalf("a site that never appears: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("the wait for a site that never appears is not bounded by Settle")
	}
}

// A site renamed to one of its own aliases answers the same names after
// as before: nothing is taken down or written again, the records change
// owner, and taking the old name away afterwards unpublishes it alone.
func TestASiteRenamedToItsAliasKeepsEveryNamePublished(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.setHosts(map[string]string{"otter.example.com": "otter.example.com", "shop.example.com": "otter.example.com"})
	if _, err := f.p.Publish(ctx, "otter.example.com", ""); err != nil {
		t.Fatal(err)
	}
	f.dns.calls = nil
	f.setHosts(map[string]string{"otter.example.com": "shop.example.com", "shop.example.com": "shop.example.com"})
	f.p.Renamed("otter.example.com", "shop.example.com")
	f.p.Reconcile(ctx)
	if f.dns.log() != "" {
		t.Fatalf("a rename to an alias touched DNS: %s", f.dns.log())
	}
	if door, ok := f.p.Published("shop.example.com"); !ok || door != "tunnel" {
		t.Fatalf("the intent did not follow: %q %v", door, ok)
	}
	if _, ok := f.p.Published("otter.example.com"); ok {
		t.Fatal("the old name still has an intent")
	}
	for n, r := range f.p.Records() {
		if r.Site != "shop.example.com" || r.State != StatePublished {
			t.Errorf("%s: %+v", n, r)
		}
	}
	f.setHosts(map[string]string{"shop.example.com": "shop.example.com"})
	f.p.Reconcile(ctx)
	if log := f.dns.log(); !strings.Contains(log, "unpublish otter.example.com") || strings.Contains(log, "unpublish shop.example.com") {
		t.Fatalf("calls:\n%s", log)
	}
}
