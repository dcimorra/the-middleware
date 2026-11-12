package main

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/api"
	"github.com/dcimorra/the-middleware/internal/app"
	"github.com/dcimorra/the-middleware/internal/helper"
	"github.com/dcimorra/the-middleware/internal/inbound"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/modules/vpn"
	"github.com/dcimorra/the-middleware/internal/nodes"
	"github.com/dcimorra/the-middleware/internal/publish"
	"github.com/dcimorra/the-middleware/internal/pubports"
)

// thePorts is the book of approved public ports, for the release a
// site's rename or delete asks of every book.
var thePorts *pubports.Book

// publicHost answers the engine about the sites' public ports
// (engine.PublicHost): this node's public address, the owner's
// approvals, the house's own ports, and the machine's firewall through
// the helper.
type publicHost struct {
	root     string
	book     *pubports.Book
	helper   *helper.Client
	reserved []manifest.HeldPort

	mu       sync.Mutex
	ip       string
	asked    time.Time
	fetching bool
}

// publicIPFresh is how long a found address is believed; publicIPRetry
// how soon it is looked for again while there is none.
const (
	publicIPFresh = 10 * time.Minute
	publicIPRetry = 20 * time.Second
)

// PublicIP implements engine.PublicHost: what is known now, looked for
// again in the background when it is old. The engine asks under its
// lock, so this never waits for the network; an address that changed (a
// line with a dynamic one) is a new revision of the services that read
// it, at most ten minutes later.
func (h *publicHost) PublicIP() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	every := publicIPFresh
	if h.ip == "" {
		every = publicIPRetry
	}
	if !h.fetching && time.Since(h.asked) > every {
		h.fetching = true
		go h.find()
	}
	return h.ip
}

// find looks for the address: the one the operator wrote for the ports
// door when there is one, since that is the one the names point at;
// otherwise what the internet sees this machine come from.
func (h *publicHost) find() {
	ip := ""
	if st, err := inbound.LoadState(h.root); err == nil && st.PublicIP != "" {
		ip = st.PublicIP
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		ip, _ = publish.DetectPublicIP(ctx)
		cancel()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.fetching, h.asked = false, time.Now()
	if net.ParseIP(ip) != nil {
		// One that could not be found now keeps the last one: a relay
		// restarted with no address because a lookup timed out is worse
		// than one that waits ten minutes to learn of a change.
		h.ip = ip
	}
}

// Pending implements engine.PublicHost.
func (h *publicHost) Pending(domain string, m *manifest.Manifest) []string {
	return h.book.Pending(domain, m)
}

// Reserved implements engine.PublicHost.
func (h *publicHost) Reserved() []manifest.HeldPort { return h.reserved }

// openedMark is written while the helper holds rules of ours, so that a
// supervisor that starts with no public port left knows there is
// something to take away. A name no site can have, in the book's folder.
func (h *publicHost) openedMark() string {
	return filepath.Join(h.root, "data", pubports.Dir, ".opened")
}

// Open implements engine.PublicHost.
func (h *publicHost) Open(ctx context.Context, ports []manifest.PublicPort) error {
	_, had := os.Stat(h.openedMark())
	if len(ports) == 0 && had != nil {
		return nil // never opened anything: nothing to ask of the helper
	}
	rules := make([]helper.PortRule, len(ports))
	for i, p := range ports {
		rules[i] = helper.PortRule{From: p.From, To: p.To, Proto: p.Proto}
	}
	if _, err := h.helper.PortsOpen(ctx, rules); err != nil {
		if errors.Is(err, helper.ErrUnavailable) {
			return errors.New("the helper service is not running, so the house opened nothing: the ports answer only if this machine's firewall already lets them through")
		}
		return err
	}
	if len(ports) == 0 {
		return os.Remove(h.openedMark())
	}
	if err := os.MkdirAll(filepath.Dir(h.openedMark()), 0o755); err != nil {
		return err
	}
	return os.WriteFile(h.openedMark(), nil, 0o644)
}

// housePorts are the ports the house listens on in this installation
// beyond the doors and the sites' range, which the manifest's own check
// knows: the panel's, the nodes' mesh and the VPN.
func housePorts(a *app.App) []manifest.HeldPort {
	out := []manifest.HeldPort{
		{From: nodes.DefaultAPIPort, To: nodes.DefaultRelayPort, What: "the nodes' mesh"},
		{From: vpn.DefaultListenPort, To: nodes.DefaultPort, What: "the VPN's and the mesh's WireGuard"},
	}
	if _, port, err := net.SplitHostPort(app.ListenAddr(a.Settings.Listen)); err == nil {
		if n, err := strconv.Atoi(port); err == nil {
			out = append(out, manifest.HeldPort{From: n, To: n, What: "the house's own listener"})
		}
	}
	if s := a.Settings.Ports; s.From != 0 && s.To != 0 {
		out = append(out, manifest.HeldPort{From: s.From, To: s.To, What: "the range the house gives its sites on loopback"})
	}
	return out
}

// wirePublicPorts gives the engine and the API the sites' public ports.
func wirePublicPorts(a *app.App, srv *api.Server) {
	thePorts = &pubports.Book{Root: a.Root}
	host := &publicHost{root: a.Root, book: thePorts, helper: helper.Dial(""), reserved: housePorts(a)}
	a.Engine.SetPublic(host)
	srv.RegisterPublicPorts(api.PortsOptions{Book: thePorts, Reserved: host.Reserved, PublicIP: host.PublicIP})
}
