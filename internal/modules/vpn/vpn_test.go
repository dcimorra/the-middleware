package vpn

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/helper"
	"github.com/dcimorra/the-middleware/internal/wireguard"
)

// plainSecrets is reversible on purpose so a test can read back what was
// stored and prove it is not the clear key.
type plainSecrets struct{}

// scramble reverses the bytes so the stored form never contains the clear
// base64 of the key, which is what the "not in clear" assertions look for.
func scramble(p []byte) []byte {
	out := make([]byte, len(p))
	for i, b := range p {
		out[len(p)-1-i] = b
	}
	return out
}

func (plainSecrets) Encrypt(p []byte) (string, error) {
	return "enc:" + base64.StdEncoding.EncodeToString(scramble(p)), nil
}

func (plainSecrets) Decrypt(s string) ([]byte, error) {
	if !strings.HasPrefix(s, "enc:") {
		return nil, errors.New("not encrypted by the test secrets")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, "enc:"))
	return scramble(raw), err
}

type fixture struct {
	root string
	srv  *helper.Server
	rec  *helper.Recorder
	bus  *events.Bus
	vpn  *VPN
	conf string // path of the server file the helper writes
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir, err := os.MkdirTemp("", "mwvpn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	f := &fixture{root: filepath.Join(dir, "root"), rec: &helper.Recorder{Outputs: map[string]string{}, Fail: map[string]error{}}, bus: events.New("")}
	f.srv = &helper.Server{
		Socket: filepath.Join(dir, "h.sock"), SocketGroup: -1, AllowUID: -1, AllowGID: -1, Exec: f.rec,
		Credentials:  func(net.Conn) (helper.Peer, error) { return helper.Peer{UID: 0}, nil },
		WireguardDir: filepath.Join(dir, "wireguard"),
		SysNetDir:    filepath.Join(dir, "net"),
		Log:          log.New(io.Discard, "", 0),
	}
	f.conf = filepath.Join(f.srv.WireguardDir, "mw0.conf")
	ln, err := f.srv.Listen()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = f.srv.Serve(ctx, ln) }()
	t.Cleanup(func() { cancel(); <-done })
	f.vpn = f.open(t)
	return f
}

func (f *fixture) open(t *testing.T) *VPN {
	t.Helper()
	v, err := New(Options{
		Root: f.root, Helper: helper.Dial(f.srv.Socket), Secrets: plainSecrets{}, Bus: f.bus,
		PanelPorts: []int{46100},
		LookPath:   func(string) (string, error) { return "/usr/bin/x", nil },
		Now:        func() time.Time { return time.Unix(1760000000, 0) },
		PublicIP:   func(context.Context) (string, error) { return "198.51.100.7", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (f *fixture) up() { os.MkdirAll(filepath.Join(f.srv.SysNetDir, "mw0"), 0o755) }

func (f *fixture) enable(t *testing.T) []Line {
	t.Helper()
	lines, err := f.vpn.Enable(context.Background(), map[string]string{"endpoint": "vpn.example.com:51820"})
	if err != nil {
		t.Fatalf("enable: %v\n%+v", err, lines)
	}
	f.up()
	return lines
}

func (f *fixture) serverFile(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(f.conf)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func eventsOf(bus *events.Bus, typ string) []events.Event {
	var out []events.Event
	for _, e := range bus.Recent() {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func TestEnableGeneratesAKeyStoresItEncryptedAndBringsTheInterfaceUp(t *testing.T) {
	f := newFixture(t)
	lines := f.enable(t)
	if lines[len(lines)-1].Status != "ok" {
		t.Fatalf("lines: %+v", lines)
	}
	if got := f.rec.Lines(); len(got) != 1 || got[0] != "wg-quick up "+f.conf {
		t.Fatalf("got %q", got)
	}
	file := f.serverFile(t)
	if !strings.Contains(file, "ListenPort = 51820\n") || !strings.Contains(file, "Address = 10.77.0.1/24\n") {
		t.Fatalf("server file:\n%s", file)
	}
	privLine := ""
	for _, l := range strings.Split(file, "\n") {
		if strings.HasPrefix(l, "PrivateKey = ") {
			privLine = strings.TrimPrefix(l, "PrivateKey = ")
		}
	}
	priv, err := wireguard.ParseKey(privLine)
	if err != nil {
		t.Fatalf("no private key in the server file: %q", privLine)
	}
	stateData, _ := os.ReadFile(filepath.Join(f.vpn.Dir(), "state.json"))
	if strings.Contains(string(stateData), priv.String()) {
		t.Fatal("the private key is in clear in state.json")
	}
	if !strings.Contains(string(stateData), `"private_key": "enc:`) {
		t.Fatalf("state.json:\n%s", stateData)
	}
	pub, _ := priv.Public()
	if st := f.vpn.Status(context.Background()); st.PublicKey != pub.String() || !st.Enabled || st.Endpoint != "vpn.example.com:51820" {
		t.Fatalf("status: %+v", st)
	}
	if len(eventsOf(f.bus, "vpn.enabled")) != 1 {
		t.Fatal("no vpn.enabled event")
	}
	// Enabling again keeps the key: client configs already handed out
	// must keep working.
	again := f.open(t)
	if _, err := again.Enable(context.Background(), map[string]string{"endpoint": "vpn.example.com:51820", "listen_port": "51821"}); err != nil {
		t.Fatal(err)
	}
	if st := again.Status(context.Background()); st.PublicKey != pub.String() || st.ListenPort != 51821 {
		t.Fatalf("key changed or port not updated: %+v", st)
	}
}

func TestEnableRefusesWhenWgIsMissingAndRunsNothing(t *testing.T) {
	f := newFixture(t)
	v, err := New(Options{Root: f.root, Helper: helper.Dial(f.srv.Socket), Secrets: plainSecrets{}, PanelPorts: []int{1},
		LookPath: func(string) (string, error) { return "", errors.New("not found") }})
	if err != nil {
		t.Fatal(err)
	}
	lines, err := v.Enable(context.Background(), map[string]string{"endpoint": "vpn.example.com:51820"})
	if err == nil {
		t.Fatal("enabled without wg")
	}
	found := false
	for _, l := range lines {
		if l.Status == "fail" && strings.Contains(l.Text, "wireguard-tools") {
			found = true
		}
	}
	if !found {
		t.Fatalf("lines: %+v", lines)
	}
	if len(f.rec.Calls) != 0 || len(lines) == 0 {
		t.Fatalf("executed: %v", f.rec.Lines())
	}
	if _, err := f.vpn.Enable(context.Background(), map[string]string{"endpoint": "no-port"}); err == nil {
		t.Fatal("bad endpoint accepted")
	}
	if inst := f.vpn.Install(context.Background(), nil); inst[0].Status != "ok" {
		t.Fatalf("install: %+v", inst)
	}
}

func TestAddingAPeerReturnsItsConfigOnceAndStoresOnlyItsPublicKey(t *testing.T) {
	f := newFixture(t)
	if _, err := f.vpn.AddPeer(context.Background(), "laptop"); !errors.Is(err, ErrNotEnabled) {
		t.Fatalf("got %v", err)
	}
	f.enable(t)
	f.rec.Reset()
	f.rec.Outputs["wg-quick strip "+f.conf] = "stripped"
	text, err := f.vpn.AddPeer(context.Background(), "laptop")
	if err != nil {
		t.Fatal(err)
	}
	// The interface was up, so the helper synced the running config.
	if lines := f.rec.Lines(); len(lines) != 2 || lines[1] != "wg syncconf mw0 /dev/stdin" || f.rec.Calls[1].Stdin != "stripped" {
		t.Fatalf("got %q", lines)
	}
	st := f.vpn.Status(context.Background())
	if !strings.Contains(text, "Address = 10.77.0.2/32\n") || !strings.Contains(text, "Endpoint = vpn.example.com:51820\n") || !strings.Contains(text, "PublicKey = "+st.PublicKey+"\n") || !strings.Contains(text, "AllowedIPs = 10.77.0.0/24\n") {
		t.Fatalf("client config:\n%s", text)
	}
	var clientPriv string
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, "PrivateKey = ") {
			clientPriv = strings.TrimPrefix(l, "PrivateKey = ")
		}
	}
	k, err := wireguard.ParseKey(clientPriv)
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := k.Public()
	// The client's private key is nowhere on disk, in any encoding we store.
	filepath.Walk(filepath.Dir(f.root), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		data, _ := os.ReadFile(path)
		if strings.Contains(string(data), clientPriv) || strings.Contains(string(data), "enc:"+base64.StdEncoding.EncodeToString(scramble(k[:]))) {
			t.Errorf("client private key found in %s", path)
		}
		return nil
	})
	file := f.serverFile(t)
	if !strings.Contains(file, "# laptop\nPublicKey = "+pub.String()+"\n") || !strings.Contains(file, "AllowedIPs = 10.77.0.2/32\n") || !strings.Contains(file, "PresharedKey = ") {
		t.Fatalf("server file:\n%s", file)
	}
	if st.Peers[0].Name != "laptop" || st.Peers[0].Address != "10.77.0.2" || st.Peers[0].PublicKey != pub.String() {
		t.Fatalf("status peers: %+v", st.Peers)
	}
	if _, err := f.vpn.AddPeer(context.Background(), "laptop"); !errors.Is(err, ErrPeerExists) {
		t.Fatalf("got %v", err)
	}
	if _, err := f.vpn.AddPeer(context.Background(), "../etc"); err == nil {
		t.Fatal("bad name accepted")
	}
	// A second peer gets the next address; a reopened module sees both.
	if _, err := f.vpn.AddPeer(context.Background(), "phone"); err != nil {
		t.Fatal(err)
	}
	again := f.open(t)
	st = again.Status(context.Background())
	if len(st.Peers) != 2 || st.Peers[1].Name != "phone" || st.Peers[1].Address != "10.77.0.3" {
		t.Fatalf("after reopen: %+v", st.Peers)
	}
	if len(eventsOf(f.bus, "vpn.peer.added")) != 2 {
		t.Fatal("expected two vpn.peer.added events")
	}
}

func TestRemovingAPeerRewritesTheServerFileWithoutIt(t *testing.T) {
	f := newFixture(t)
	f.enable(t)
	f.vpn.AddPeer(context.Background(), "laptop")
	f.vpn.AddPeer(context.Background(), "phone")
	if err := f.vpn.RemovePeer(context.Background(), "laptop"); err != nil {
		t.Fatal(err)
	}
	file := f.serverFile(t)
	if strings.Contains(file, "# laptop") || !strings.Contains(file, "# phone") {
		t.Fatalf("server file:\n%s", file)
	}
	if err := f.vpn.RemovePeer(context.Background(), "laptop"); !errors.Is(err, ErrPeerNotFound) {
		t.Fatalf("got %v", err)
	}
	// The freed address is handed out again.
	text, _ := f.vpn.AddPeer(context.Background(), "tablet")
	if !strings.Contains(text, "Address = 10.77.0.2/32\n") {
		t.Fatalf("address not reused:\n%s", text)
	}
}

func TestVPNOnlyLoadsAFirewallPolicyAndDisableLiftsIt(t *testing.T) {
	f := newFixture(t)
	f.enable(t)
	if err := f.vpn.SetOnly(context.Background(), true); err == nil {
		t.Fatal("vpn-only with no peers would lock everyone out")
	}
	f.vpn.AddPeer(context.Background(), "laptop")
	f.rec.Reset()
	if err := f.vpn.SetOnly(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	last, _ := f.rec.Last()
	if last.Line() != "nft -f -" || !strings.Contains(last.Stdin, `iifname "mw0" tcp dport { 46100 } accept`) || !strings.Contains(last.Stdin, "udp dport { 51820 } accept") {
		t.Fatalf("ruleset:\n%s", last.Stdin)
	}
	if !f.open(t).Status(context.Background()).VPNOnly {
		t.Fatal("vpn_only did not survive a reopen")
	}
	f.rec.Reset()
	if err := f.vpn.Disable(context.Background()); err != nil {
		t.Fatal(err)
	}
	lines := f.rec.Lines()
	if len(lines) != 2 || lines[0] != "nft -f -" || lines[1] != "wg-quick down "+f.conf {
		t.Fatalf("got %q", lines)
	}
	if strings.Contains(f.rec.Calls[0].Stdin, "46100") {
		t.Fatalf("the restriction was not lifted:\n%s", f.rec.Calls[0].Stdin)
	}
	st := f.open(t).Status(context.Background())
	if st.Enabled || st.VPNOnly || st.PublicKey == "" || len(st.Peers) != 1 {
		t.Fatalf("after disable: %+v", st)
	}
	if len(eventsOf(f.bus, "vpn.disabled")) != 1 {
		t.Fatal("no vpn.disabled event")
	}
	// Without a working nft (none installed, say) the decision still
	// holds: the house closes the panel itself, the kernel rule is a second
	// wall, and the failure is said as a warning.
	f.vpn.Enable(context.Background(), map[string]string{"endpoint": "vpn.example.com:51820"})
	f.rec.Fail["nft -f -"] = errors.New("nft is not installed")
	if err := f.vpn.SetOnly(context.Background(), true); err != nil || !f.vpn.Status(context.Background()).VPNOnly {
		t.Fatalf("vpn_only refused because nft failed: %v", err)
	}
	if len(eventsOf(f.bus, "firewall.apply.failed")) == 0 {
		t.Fatal("the failure was not said")
	}
	if err := f.vpn.Disable(context.Background()); err != nil {
		t.Fatalf("disable refused because nft failed: %v", err)
	}
}

func TestStatusMergesTheKernelHandshakesIntoTheStoredPeers(t *testing.T) {
	f := newFixture(t)
	f.enable(t)
	f.vpn.AddPeer(context.Background(), "laptop")
	f.vpn.AddPeer(context.Background(), "phone")
	st := f.vpn.Status(context.Background())
	pub, _ := wireguard.ParseKey(st.PublicKey)
	laptop, _ := wireguard.ParseKey(st.Peers[0].PublicKey)
	f.rec.Outputs["wg show mw0 dump"] = "(none)\t" + pub.String() + "\t51820\toff\n" +
		laptop.String() + "\t(none)\t203.0.113.5:40000\t10.77.0.2/32\t1759999990\t100\t200\toff\n"
	st = f.vpn.Status(context.Background())
	if !st.Up || !st.Helper || st.Error != "" {
		t.Fatalf("status: %+v", st)
	}
	if l := st.Peers[0]; !l.Online || l.LastHandshake.Unix() != 1759999990 || l.Endpoint != "203.0.113.5:40000" || l.ReceiveBytes != 100 {
		t.Fatalf("laptop: %+v", l)
	}
	if p := st.Peers[1]; p.Online || !p.LastHandshake.IsZero() {
		t.Fatalf("phone: %+v", p)
	}
}

func TestTheRoutesDriveThePeersOverHTTP(t *testing.T) {
	f := newFixture(t)
	f.enable(t)
	srv := httptest.NewServer(f.vpn.Handler())
	defer srv.Close()
	do := func(method, path, body string) (int, map[string]any) {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	code, out := do("POST", "/peers", `{"name":"laptop"}`)
	if code != 201 || !strings.Contains(out["config"].(string), "[Interface]") {
		t.Fatalf("%d %v", code, out)
	}
	if code, out = do("POST", "/peers", `{"name":"laptop"}`); code != 409 {
		t.Fatalf("%d %v", code, out)
	}
	if code, out = do("POST", "/peers", `{"name":"Bad Name"}`); code != 400 {
		t.Fatalf("%d %v", code, out)
	}
	if code, out = do("GET", "/", ""); code != 200 || len(out["peers"].([]any)) != 1 {
		t.Fatalf("%d %v", code, out)
	}
	if code, out = do("POST", "/only", `{"enabled":true}`); code != 200 || out["enabled"] != true {
		t.Fatalf("%d %v", code, out)
	}
	if code, out = do("DELETE", "/peers/nobody", ""); code != 404 {
		t.Fatalf("%d %v", code, out)
	}
	if code, out = do("DELETE", "/peers/laptop", ""); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if code, _ = do("GET", "/peers/whatever", ""); code != 404 && code != 405 {
		t.Fatalf("status page answered an unknown path: %d", code)
	}
}

func TestTurningTheVPNOnAsksNothing(t *testing.T) {
	f := newFixture(t)
	lines, err := f.vpn.Enable(context.Background(), map[string]string{})
	if err != nil {
		t.Fatalf("enable with nothing: %v\n%+v", err, lines)
	}
	f.up()
	st := f.vpn.Status(context.Background())
	if st.Endpoint != "198.51.100.7:51820" {
		t.Fatalf("endpoint %q", st.Endpoint)
	}
}

func TestAnInterfaceFoundDownIsBroughtUpAgain(t *testing.T) {
	f := newFixture(t)
	f.enable(t)
	// The machine rebooted: the interface is gone.
	os.RemoveAll(filepath.Join(f.srv.SysNetDir, "mw0"))
	f.rec.Reset()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.vpn.Keep(ctx, time.Hour); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		up := false
		for _, l := range f.rec.Lines() {
			if strings.HasPrefix(l, "wg-quick up") {
				up = true
			}
		}
		if up {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not brought up: %v", f.rec.Lines())
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if len(eventsOf(f.bus, "vpn.up")) != 1 {
		t.Fatal("no vpn.up event")
	}
}

// `middleware modules disable vpn` is another process: the service must
// see the VPN off, or the panel stays inside a VPN that is not there.
func TestAChangeMadeByAnotherProcessIsSeen(t *testing.T) {
	f := newFixture(t)
	f.enable(t)
	f.vpn.AddPeer(context.Background(), "phone")
	if err := f.vpn.SetOnly(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if !f.vpn.PanelOnly() {
		t.Fatal("not inside")
	}
	time.Sleep(20 * time.Millisecond) // a new mtime
	other := f.open(t)
	if err := other.Disable(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.vpn.PanelOnly() {
		t.Fatal("the service still holds the panel inside a VPN another process turned off")
	}
}
