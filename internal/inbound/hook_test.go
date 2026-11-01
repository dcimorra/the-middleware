package inbound_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/acme"
	"github.com/dcimorra/the-middleware/internal/acme/acmetest"
	"github.com/dcimorra/the-middleware/internal/certs"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/inbound"
	"github.com/dcimorra/the-middleware/internal/localdns"
)

type house struct {
	root  string
	ca    *acmetest.Server
	store *certs.Store
	hook  *inbound.Hook
	bus   *events.Bus
	http  string
	https string
}

// newHouse opens the ports door on two free loopback ports with a fake CA
// that validates HTTP-01 by knocking on the real port-80 listener.
func newHouse(t *testing.T, sites ...string) *house {
	return newHouseWith(t, nil, sites...)
}

// newHouseWith sets the hook's fields with configure before it runs: set
// after, they race with its loop, which reads them on every tick (the race
// detector caught a test doing that).
func newHouseWith(t *testing.T, configure func(*inbound.Hook), sites ...string) *house {
	t.Helper()
	root := t.TempDir()
	ca := acmetest.New(t)
	solver := acme.NewHTTP01()
	store := certs.New(filepath.Join(root, "data", "certs"), &certs.ACMEIssuer{
		Dir: filepath.Join(root, "data", "certs"), Directory: ca.URL, HTTP01: solver,
	})
	isSite := func(d string) bool {
		if d == "panel.test" {
			return true
		}
		for _, s := range sites {
			if s == d {
				return true
			}
		}
		return false
	}
	store.Allowed = isSite
	bus := events.New("")
	store.Bus = bus
	st := inbound.DefaultState()
	st.Ports.Open = true
	st.Ports.HTTP = "127.0.0.1:0"
	st.Ports.HTTPS = "127.0.0.1:0"
	if err := inbound.SaveState(root, st); err != nil {
		t.Fatal(err)
	}
	hook := inbound.New(root, store, solver)
	hook.PanelDomain = "panel.test"
	hook.Sites = func() []string { return sites }
	hook.Bus = bus
	hook.Tick = 50 * time.Millisecond
	// The app handler, as the app would build it: the hook wrapped around
	// a site that echoes what it was told.
	hook.Handler = hook.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "site="+r.Host+" proto="+r.Header.Get("X-Forwarded-Proto"))
	}))
	if configure != nil {
		configure(hook)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := hook.Run(ctx, nil); err != nil {
		t.Fatal(err)
	}
	h := &house{root: root, ca: ca, store: store, hook: hook, bus: bus}
	h.http, h.https = hook.Addrs()
	if h.http == "" || h.https == "" {
		t.Fatalf("ports door not open: %+v", hook.Status())
	}
	ca.ValidateAddr = h.http
	return h
}

// client talks to the house's 443 for whatever host the URL names.
func (h *house) client(pool bool) *http.Client {
	cfg := &tls.Config{InsecureSkipVerify: !pool}
	if pool {
		cfg.RootCAs = h.ca.Pool()
	}
	return &http.Client{
		Timeout: 40 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig:   cfg,
			ForceAttemptHTTP2: true,
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, h.https)
			},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func get(t *testing.T, c *http.Client, url string) (*http.Response, string) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func eventTypes(bus *events.Bus) []string {
	var out []string
	for _, e := range bus.Recent() {
		out = append(out, e.Type)
	}
	return out
}

func TestTheHTTPDoorAnswersTheChallengeAndRedirectsEverythingElse(t *testing.T) {
	h := newHouse(t, "a.test")
	plain := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest("GET", "http://"+h.http+"/some/path?q=1", nil)
	req.Host = "a.test"
	resp, err := plain.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 301 || resp.Header.Get("Location") != "https://a.test/some/path?q=1" {
		t.Fatalf("%d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	h.hook.Solver.Present(context.Background(), "a.test", "tok", "tok.thumb")
	resp, body := get(t, plain, "http://"+h.http+acme.ChallengePath+"tok")
	if resp.StatusCode != 200 || body != "tok.thumb" {
		t.Fatalf("challenge: %d %q", resp.StatusCode, body)
	}
}

func TestTheHTTPSDoorIssuesOnFirstVisitAndSpeaksHTTP2(t *testing.T) {
	h := newHouse(t, "a.test")
	resp, body := get(t, h.client(true), "https://a.test/hello")
	if resp.StatusCode != 200 || body != "site=a.test proto=https" {
		t.Fatalf("%d %q", resp.StatusCode, body)
	}
	if resp.ProtoMajor != 2 {
		t.Fatalf("negotiated %s, want HTTP/2", resp.Proto)
	}
	if h.ca.Issued() != 1 || !h.store.Has("a.test") {
		t.Fatalf("issued %d, has %v", h.ca.Issued(), h.store.Has("a.test"))
	}
	st := h.hook.Status()
	if len(st.Domains) != 2 || st.Domains[0].Domain != "a.test" || st.Domains[0].Door != inbound.Ports || st.Domains[0].CertExpires == nil {
		t.Fatalf("status %+v", st.Domains)
	}
	if st.Domains[1].Domain != "panel.test" || st.Domains[1].Door != inbound.Tunnel || st.Domains[1].CertExpires != nil {
		t.Fatalf("panel row %+v", st.Domains[1])
	}
	if !st.Doors.Ports.Listening || st.Doors.Ports.Error != "" {
		t.Fatalf("doors %+v", st.Doors)
	}
}

func TestANameWithoutACertificateGetsTheNoCertPageNotTheSite(t *testing.T) {
	h := newHouse(t, "a.test")
	resp, body := get(t, h.client(false), "https://stranger.test/")
	if resp.StatusCode != 503 || !strings.Contains(body, "No certificate yet") {
		t.Fatalf("%d %q", resp.StatusCode, body)
	}
	if h.ca.Issued() != 0 {
		t.Fatal("a stranger made us order a certificate")
	}
}

func TestAClosedTunnelShowsTheDoorClosedPageButThePanelAndTheCAStillGetThrough(t *testing.T) {
	h := newHouse(t, "a.test")
	if _, err := inbound.SetDoor(h.root, inbound.Tunnel, false); err != nil {
		t.Fatal(err)
	}
	// Requests with no door in their context are the loopback listener.
	do := func(host, path string) (int, string) {
		r := httptest.NewRequest("GET", "http://"+host+path, nil)
		r.Host = host
		w := httptest.NewRecorder()
		h.hook.Handler.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	if code, body := do("a.test", "/"); code != 503 || !strings.Contains(body, "This entrance is closed") {
		t.Fatalf("site through closed tunnel: %d %q", code, body)
	}
	if code, body := do("panel.test", "/"); code != 200 || body != "site=panel.test proto=http" {
		t.Fatalf("panel through closed tunnel: %d %q", code, body)
	}
	h.hook.Solver.Present(context.Background(), "a.test", "tok", "tok.thumb")
	if code, body := do("a.test", acme.ChallengePath+"tok"); code != 200 || body != "tok.thumb" {
		t.Fatalf("challenge through closed tunnel: %d %q", code, body)
	}
	// A tunnel that set the scheme keeps it.
	r := httptest.NewRequest("GET", "http://panel.test/", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()
	h.hook.Handler.ServeHTTP(w, r)
	if w.Body.String() != "site=panel.test proto=https" {
		t.Fatalf("%q", w.Body.String())
	}
	// Reopening from the file is seen without a restart.
	inbound.SetDoor(h.root, inbound.Tunnel, true)
	if code, _ := do("a.test", "/"); code != 200 {
		t.Fatalf("after reopening: %d", code)
	}
}

func TestClosingThePortsDoorInTheFileStopsTheListeners(t *testing.T) {
	h := newHouse(t, "a.test")
	if _, err := inbound.SetDoor(h.root, inbound.Ports, false); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if a, b := h.hook.Addrs(); a == "" && b == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("listeners still up after the file closed the door")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := net.DialTimeout("tcp", h.https, time.Second); err == nil {
		t.Fatal("443 still accepts")
	}
	if st := h.hook.Status(); st.Doors.Ports.Open || st.Doors.Ports.Listening {
		t.Fatalf("%+v", st.Doors)
	}
	if err := h.hook.SetDoor(inbound.Ports, true); err != nil {
		t.Fatal(err)
	}
	if a, b := h.hook.Addrs(); a == "" || b == "" {
		t.Fatal("not reopened")
	}
	got := strings.Join(eventTypes(h.bus), " ")
	for _, want := range []string{"inbound.listening", "inbound.stopped", "inbound.opened"} {
		if !strings.Contains(got, want) {
			t.Fatalf("events %q lack %s", got, want)
		}
	}
	st, _ := inbound.LoadState(h.root)
	if !st.Ports.Open {
		t.Fatal("SetDoor did not persist")
	}
}

func TestAPortHeldBySomeoneElseIsReportedOnceNotEveryTick(t *testing.T) {
	h := newHouse(t, "a.test")
	taken, _ := net.Listen("tcp", "127.0.0.1:0")
	defer taken.Close()
	st, _ := inbound.LoadState(h.root)
	st.Ports.HTTP = taken.Addr().String()
	inbound.SaveState(h.root, st)
	deadline := time.Now().Add(5 * time.Second)
	for h.hook.Status().Doors.Ports.Error == "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // a few ticks more
	failed := 0
	for _, e := range h.bus.Recent() {
		if e.Type == "inbound.failed" {
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("inbound.failed emitted %d times", failed)
	}
	if s := h.hook.Status(); s.Doors.Ports.Listening || !strings.Contains(s.Doors.Ports.Error, "address already in use") {
		t.Fatalf("%+v", s.Doors)
	}
}

func TestAMoveToPortsObtainsTheCertificateFirstAndOnlyThenMarks(t *testing.T) {
	h := newHouse(t, "a.test", "b.test")
	h.ca.SetRateLimited(true)
	lines := h.hook.Move(context.Background(), []string{"a.test"}, inbound.Ports)
	if len(lines) != 1 || lines[0].Status != "fail" || !strings.Contains(lines[0].Text, "rateLimited") {
		t.Fatalf("%+v", lines)
	}
	if st, _ := inbound.LoadState(h.root); st.DoorOf("a.test") != "" {
		t.Fatal("marked without a certificate")
	}
	h.ca.SetRateLimited(false)
	h.store.Backoff = 0
	lines = h.hook.Move(context.Background(), []string{"a.test", "nobody.test", "bad domain"}, inbound.Ports)
	want := []string{"ok", "ok", "fail", "fail"}
	for i, l := range lines {
		if l.Status != want[i] {
			t.Fatalf("line %d: %+v", i, lines)
		}
	}
	st, _ := inbound.LoadState(h.root)
	if st.DoorOf("a.test") != inbound.Ports || !h.store.Has("a.test") {
		t.Fatalf("state %+v", st.Domains)
	}
	// Back to the tunnel: marked, certificate kept for the way back.
	lines = h.hook.Move(context.Background(), []string{"a.test"}, inbound.Tunnel)
	if len(lines) != 1 || lines[0].Status != "ok" {
		t.Fatalf("%+v", lines)
	}
	st, _ = inbound.LoadState(h.root)
	if st.DoorOf("a.test") != inbound.Tunnel || !h.store.Has("a.test") {
		t.Fatalf("state %+v has %v", st.Domains, h.store.Has("a.test"))
	}
	row := h.hook.Status().Domains[0]
	if row.Domain != "a.test" || row.Door != inbound.Tunnel || row.CertExpires == nil {
		t.Fatalf("%+v", row)
	}
	moved := 0
	for _, e := range h.bus.Recent() {
		if e.Type == "inbound.moved" {
			moved++
		}
	}
	if moved != 2 {
		t.Fatalf("inbound.moved emitted %d times", moved)
	}
}

func TestTheStateFileDefaultsAndRefusesWhatCannotListen(t *testing.T) {
	root := t.TempDir()
	st, err := inbound.LoadState(root)
	if err != nil || !st.Tunnel.Open || st.Ports.Open || st.Ports.HTTP != ":80" || st.Ports.HTTPS != ":443" {
		t.Fatalf("%+v %v", st, err)
	}
	st.Ports.HTTPS = ":80"
	if err := inbound.SaveState(root, st); err == nil {
		t.Fatal("same address for both accepted")
	}
	st.Ports.HTTPS = ":443"
	st.Domains = map[string]inbound.Door{"a.test": "garage"}
	if err := inbound.SaveState(root, st); err == nil {
		t.Fatal("unknown door accepted")
	}
	if _, err := inbound.ParseDoor("garage"); err == nil {
		t.Fatal("garage is not a door")
	}
	os.MkdirAll(filepath.Dir(inbound.Path(root)), 0o755)
	os.WriteFile(inbound.Path(root), []byte("{not json"), 0o644)
	if _, err := inbound.LoadState(root); err == nil {
		t.Fatal("broken file read as fine")
	}
}

// TestTheLocalDoorServesPlainHTTPAndResolvesTheSites is the development
// bench contract: a browser on this machine resolves misite.test through
// the door's own name server and gets the site over plain http, with no
// redirect to a 443 that is not there.
func TestTheLocalDoorServesPlainHTTPAndResolvesTheSites(t *testing.T) {
	h := newHouse(t, "misite.test")
	if err := h.hook.SetLocal(true, "127.0.0.1:0", "127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	httpAddr, dnsAddr := h.hook.LocalAddrs()
	if httpAddr == "" || dnsAddr == "" {
		t.Fatalf("local door not open: %+v", h.hook.Status().Doors.Local)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ips, err := localdns.Query(ctx, dnsAddr, "misite.test", false)
	if err != nil || len(ips) != 1 || !ips[0].IsLoopback() {
		t.Fatalf("misite.test did not resolve to loopback: %v %v", ips, err)
	}
	if ips, _ := localdns.Query(ctx, dnsAddr, "panel.test", false); len(ips) != 1 {
		t.Fatalf("the panel's own name must resolve too: %v", ips)
	}
	if ips, _ := localdns.Query(ctx, dnsAddr, "other.test", false); ips != nil {
		t.Fatalf("a name nobody serves must be NXDOMAIN, got %v", ips)
	}

	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest("GET", "http://"+httpAddr+"/", nil)
	req.Host = "misite.test"
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "site=misite.test proto=http" {
		t.Fatalf("local door answered %d %q", resp.StatusCode, body)
	}

	// Closing the door takes both sockets down and the file says so.
	if err := h.hook.SetDoor(inbound.Local, false); err != nil {
		t.Fatal(err)
	}
	if a, b := h.hook.LocalAddrs(); a != "" || b != "" {
		t.Fatalf("still listening after close: %q %q", a, b)
	}
	st, _ := inbound.LoadState(h.root)
	if st.Local.Open || st.Local.HTTP != "127.0.0.1:0" {
		t.Fatalf("file not updated: %+v", st.Local)
	}
}

// TestTheLocalAndPortsDoorsCannotShareAPort: both default to 80, and a
// file that opens both on it must be refused rather than half-applied.
func TestTheLocalAndPortsDoorsCannotShareAPort(t *testing.T) {
	st := inbound.DefaultState()
	st.Ports.Open, st.Local.Open = true, true
	if err := st.Validate(); err == nil {
		t.Fatal("ports :80 and local 127.0.0.1:80 both open must not validate")
	}
	st.Local.HTTP = "127.0.0.1:8081"
	if err := st.Validate(); err != nil {
		t.Fatal(err)
	}
	st.Domains = map[string]inbound.Door{"a.test": inbound.Local}
	if err := st.Validate(); err == nil {
		t.Fatal("a domain cannot be recorded on the local door")
	}
}

// TestAVPNOnlySiteExistsOnlyThroughTheVPNDoor opens the VPN door on
// loopback (standing in for the address inside the tunnel) and asks for a
// VPN-only site through it and through the other doors, with and without
// the headers an edge would set.
func TestAVPNOnlySiteExistsOnlyThroughTheVPNDoor(t *testing.T) {
	h := newHouse(t, "vpn.test", "open.test")
	h.hook.VPNPorts = inbound.VPNPorts{HTTP: "0", HTTPS: "0", DNS: "0"}
	h.hook.VPNAddress = func() (netip.Addr, bool) { return netip.MustParseAddr("127.0.0.1"), true }
	devices := []string{"phone"}
	h.hook.VPNOnly = func(host string) ([]string, bool) { return devices, host == "vpn.test" }
	h.hook.DeviceOf = func(ip netip.Addr) string {
		if ip == netip.MustParseAddr("127.0.0.1") {
			return "phone"
		}
		return ""
	}
	var vhttp, vdns string
	for deadline := time.Now().Add(5 * time.Second); vhttp == ""; {
		vhttp, _, vdns = h.hook.VPNAddrs()
		if time.Now().After(deadline) {
			t.Fatal("the VPN door did not open")
		}
		time.Sleep(20 * time.Millisecond)
	}
	ask := func(addr, host string, headers ...string) (int, string) {
		req, _ := http.NewRequest("GET", "http://"+addr+"/", nil)
		req.Host = host
		for _, kv := range headers {
			k, v, _ := strings.Cut(kv, ": ")
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b)
	}
	if code, body := ask(vhttp, "vpn.test"); code != 200 || !strings.Contains(body, "site=vpn.test") {
		t.Fatalf("through the VPN door: %d %s", code, body)
	}
	// Through the handler without the VPN door's tag, which is what the
	// tunnel and the ports door are: it does not exist, whatever the
	// headers say.
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "http://vpn.test/", nil)
	r.Header.Set("CF-Connecting-IP", "127.0.0.1")
	h.hook.Handler.ServeHTTP(rec, r)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("through the tunnel: %d", rec.Code)
	}
	if code, _ := ask(vhttp, "open.test"); code != 200 {
		t.Fatalf("a public site through the VPN door: %d", code)
	}
	// Narrowed to a device that is not this one.
	devices = []string{"laptop"}
	if code, _ := ask(vhttp, "vpn.test"); code != http.StatusNotFound {
		t.Fatalf("another device: %d", code)
	}
	// The VPN's DNS answers the house's names with the door's address,
	// and no AAAA.
	a, aaaa := dnsAsk(t, vdns, "vpn.test", 1), dnsAsk(t, vdns, "vpn.test", 28)
	if a != "127.0.0.1" || aaaa != "" {
		t.Fatalf("A %q AAAA %q", a, aaaa)
	}
}

// dnsAsk sends one query over UDP and returns the first answer's address.
func dnsAsk(t *testing.T, addr, name string, qtype uint16) string {
	t.Helper()
	q := []byte{0x12, 0x34, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0}
	for _, l := range strings.Split(name, ".") {
		q = append(q, byte(len(l)))
		q = append(q, l...)
	}
	q = append(q, 0, byte(qtype>>8), byte(qtype), 0, 1)
	c, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	c.Write(q)
	buf := make([]byte, 512)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if buf[7] == 0 { // no answers
		return ""
	}
	// The answer is the last record: its data is the last 4 or 16 bytes.
	if qtype == 1 {
		return net.IP(buf[n-4 : n]).String()
	}
	return net.IP(buf[n-16 : n]).String()
}

// With paths, only those are inside the VPN: the rest of the site is
// public through every door, and /admin answers only through the VPN.
func TestOnlyTheVPNPathsOfASiteNeedTheVPN(t *testing.T) {
	h := newHouse(t, "mixed.test")
	h.hook.VPNPorts = inbound.VPNPorts{HTTP: "0", HTTPS: "0", DNS: "0"}
	h.hook.VPNAddress = func() (netip.Addr, bool) { return netip.MustParseAddr("127.0.0.1"), true }
	h.hook.VPNOnly = func(host string) ([]string, bool) { return nil, host == "mixed.test" }
	h.hook.VPNPaths = func(host string) []string { return []string{"/admin"} }
	var vhttp string
	for deadline := time.Now().Add(5 * time.Second); vhttp == ""; {
		vhttp, _, _ = h.hook.VPNAddrs()
		if time.Now().After(deadline) {
			t.Fatal("the VPN door did not open")
		}
		time.Sleep(20 * time.Millisecond)
	}
	outside := func(path string) int {
		rec := httptest.NewRecorder()
		h.hook.Handler.ServeHTTP(rec, httptest.NewRequest("GET", "http://mixed.test"+path, nil))
		return rec.Code
	}
	for path, want := range map[string]int{"/": 200, "/administrator": 200, "/admin": 404, "/admin/users": 404, "//admin": 404, "/x/../admin/": 404} {
		if got := outside(path); got != want {
			t.Errorf("outside the VPN, %s: %d, want %d", path, got, want)
		}
	}
	req, _ := http.NewRequest("GET", "http://"+vhttp+"/admin/users", nil)
	req.Host = "mixed.test"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("through the VPN: %d", resp.StatusCode)
	}
}

// With the panel inside the VPN, it answers through the VPN door and the
// machine's own loopback (SSH, the CLI), never what a tunnel relays.
func TestThePanelInsideTheVPNAnswersOnlyThroughItAndTheMachine(t *testing.T) {
	h := newHouse(t, "site.test")
	h.hook.PanelDomain = "panel.test"
	only := true
	h.hook.PanelVPNOnly = func() bool { return only }
	call := func(remote string, headers ...string) int {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "http://panel.test/", nil)
		r.RemoteAddr = remote
		for _, kv := range headers {
			k, v, _ := strings.Cut(kv, ": ")
			r.Header.Set(k, v)
		}
		h.hook.Handler.ServeHTTP(rec, r)
		return rec.Code
	}
	if code := call("127.0.0.1:5000", "Cf-Connecting-Ip: 203.0.113.9"); code != http.StatusNotFound {
		t.Fatalf("through the tunnel: %d", code)
	}
	if code := call("127.0.0.1:5000"); code == http.StatusNotFound {
		t.Fatalf("from the machine itself: %d", code)
	}
	// The sites are not touched by it.
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "http://site.test/", nil)
	r.Header.Set("Cf-Connecting-Ip", "203.0.113.9")
	h.hook.Handler.ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatalf("a site: %d", rec.Code)
	}
	only = false
	if code := call("127.0.0.1:5000", "Cf-Connecting-Ip: 203.0.113.9"); code == http.StatusNotFound {
		t.Fatalf("off, through the tunnel: %d", code)
	}
}

// The ports door on every address and the VPN up: the VPN has no 80/443
// of its own (two listeners on one port cannot coexist, and after a
// restart the public one was the loser), and what arrives at the VPN's
// address through the public listener is the VPN's.
func TestThePortsDoorAndTheVPNShareThePortsWithoutLockingEachOtherOut(t *testing.T) {
	h := newHouse(t, "vpn.test", "open.test")
	h.hook.VPNPorts = inbound.VPNPorts{HTTP: "0", HTTPS: "0", DNS: "0"}
	h.hook.VPNOnly = func(host string) ([]string, bool) { return nil, host == "vpn.test" }
	h.hook.VPNAddress = func() (netip.Addr, bool) { return netip.MustParseAddr("127.0.0.1"), true }
	st, _ := inbound.LoadState(h.root)
	st.Ports.HTTP, st.Ports.HTTPS = ":0", ":0"
	if err := inbound.SaveState(h.root, st); err != nil {
		t.Fatal(err)
	}
	var httpAddr, vpnHTTP string
	for deadline := time.Now().Add(5 * time.Second); ; {
		httpAddr, _ = h.hook.Addrs()
		vpnHTTP, _, _ = h.hook.VPNAddrs()
		if httpAddr != "" && vpnHTTP != "" && !strings.HasPrefix(httpAddr, "127.0.0.1") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ports %q, vpn %q", httpAddr, vpnHTTP)
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, port, _ := net.SplitHostPort(httpAddr)
	req, _ := http.NewRequest("GET", "http://127.0.0.1:"+port+"/", nil)
	req.Host = "vpn.test"
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), "site=vpn.test") {
		t.Fatalf("at the VPN's address: %d %s", resp.StatusCode, b)
	}
}

// prefixed gives back the bytes the VPN door read before relaying.
type prefixed struct {
	net.Conn
	r io.Reader
}

func (p *prefixed) Read(b []byte) (int, error) { return p.r.Read(b) }

// A device on alpha's VPN opens a VPN-only site of beta, a node with no
// VPN of its own: alpha's resolver answers the name, alpha's VPN door
// passes the connection still encrypted, and beta serves it with its own
// certificate as one that came through the VPN, from that device.
func TestOneNodesVPNReachesAnotherNodesVPNOnlySite(t *testing.T) {
	b := newHouseWith(t, func(h *inbound.Hook) {
		h.VPNOnly = func(host string) ([]string, bool) { return []string{"phone"}, host == "vpnb.test" }
	}, "vpnb.test")
	relayed := make(chan string, 1)
	a := newHouseWith(t, func(h *inbound.Hook) {
		h.VPNPorts = inbound.VPNPorts{HTTP: "0", HTTPS: "0", DNS: "0"}
		h.VPNAddress = func() (netip.Addr, bool) { return netip.MustParseAddr("127.0.0.1"), true }
		h.DeviceOf = func(netip.Addr) string { return "phone" }
		h.Elsewhere = func(host string) bool { return host == "vpnb.test" }
		h.Relay = func(c net.Conn, host string, pre []byte, device string) error {
			relayed <- host + " " + device
			b.hook.AcceptRelayed(&prefixed{Conn: c, r: io.MultiReader(bytes.NewReader(pre), c)}, device)
			return nil
		}
	}, "a.test")
	var vhttps, vdns string
	for deadline := time.Now().Add(5 * time.Second); vhttps == ""; {
		_, vhttps, vdns = a.hook.VPNAddrs()
		if time.Now().After(deadline) {
			t.Fatal("the VPN door did not open")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := dnsAsk(t, vdns, "vpnb.test", 1); got != "127.0.0.1" {
		t.Fatalf("alpha's VPN resolver does not answer beta's name: %q", got)
	}
	c := &http.Client{Timeout: 40 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: b.ca.Pool()},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, vhttps)
		},
	}}
	resp, body := get(t, c, "https://vpnb.test/")
	if resp.StatusCode != 200 || !strings.Contains(body, "site=vpnb.test") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if got := <-relayed; got != "vpnb.test phone" {
		t.Fatalf("relayed %q", got)
	}
	// alpha's own site through the same door is served by alpha.
	ca := &http.Client{Timeout: 40 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: a.ca.Pool()},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, vhttps)
		},
	}}
	if resp, body := get(t, ca, "https://a.test/"); resp.StatusCode != 200 || !strings.Contains(body, "site=a.test") {
		t.Fatalf("alpha's own: %d %s", resp.StatusCode, body)
	}
	// Another device, relayed: the site is narrowed to "phone".
	a.hook.DeviceOf = func(netip.Addr) string { return "laptop" }
	c.CloseIdleConnections()
	if resp, _ := get(t, c, "https://vpnb.test/"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("another device got %d", resp.StatusCode)
	}
}

// TestANameKeptToHTTPSIsSentThereBeforeAnythingElse crosses the hook as a
// tunnel request does: Cloudflare says http in X-Forwarded-Proto, or says
// nothing for a loopback client, and only the names marked go to https.
func TestANameKeptToHTTPSIsSentThereBeforeAnythingElse(t *testing.T) {
	h := newHouseWith(t, func(hook *inbound.Hook) {
		hook.ForceHTTPS = func(host string) bool { return host == "a.test" || host == "panel.test" }
	}, "a.test", "b.test")
	do := func(method, host, path, proto string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://"+host+path, nil)
		r.Host = host
		if proto != "" {
			r.Header.Set("X-Forwarded-Proto", proto)
		}
		w := httptest.NewRecorder()
		h.hook.Handler.ServeHTTP(w, r)
		return w
	}
	for _, proto := range []string{"http", ""} {
		w := do("GET", "a.test", "/x?y=1", proto)
		if w.Code != 301 || w.Header().Get("Location") != "https://a.test/x?y=1" {
			t.Fatalf("GET over %q: %d %q", proto, w.Code, w.Header().Get("Location"))
		}
	}
	// A form posted over http keeps its method and body on the way.
	if w := do("POST", "a.test", "/login", "http"); w.Code != 308 || w.Header().Get("Location") != "https://a.test/login" {
		t.Fatalf("POST: %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := do("GET", "a.test", "/x", "https"); w.Code != 200 || w.Body.String() != "site=a.test proto=https" {
		t.Fatalf("over https: %d %q", w.Code, w.Body.String())
	}
	// Another name of the house, not marked, is served as it came.
	if w := do("GET", "b.test", "/", "http"); w.Code != 200 {
		t.Fatalf("unmarked name: %d %q", w.Code, w.Header().Get("Location"))
	}
	// The panel is never sent anywhere by a site's setting.
	if w := do("GET", "panel.test", "/", "http"); w.Code != 200 {
		t.Fatalf("panel: %d", w.Code)
	}
	// The CA still reads its challenge over plain http.
	h.hook.Solver.Present(context.Background(), "a.test", "tok", "tok.thumb")
	if w := do("GET", "a.test", acme.ChallengePath+"tok", "http"); w.Code != 200 || w.Body.String() != "tok.thumb" {
		t.Fatalf("challenge: %d %q", w.Code, w.Body.String())
	}
}
