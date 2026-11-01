package main

import (
	"flag"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/dcimorra/the-middleware/internal/acme"
	"github.com/dcimorra/the-middleware/internal/app"
	"github.com/dcimorra/the-middleware/internal/auth"
	"github.com/dcimorra/the-middleware/internal/certs"
	"github.com/dcimorra/the-middleware/internal/edge"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/inbound"
	"github.com/dcimorra/the-middleware/internal/proxy"
	"github.com/dcimorra/the-middleware/internal/settings"
)

// inboundHook is the doors of the running process, for the API routes.
var inboundHook *inbound.Hook

// acmeIssuer is kept so the wiring can plug in dns-01 once the DNS
// module is up.
var acmeIssuer *certs.ACMEIssuer

func init() {
	hookBuilders = append(hookBuilders, hookBuilder{Name: "inbound", Order: orderInbound, Build: buildInbound})
	subcommands["inbound"] = inboundCmd
	extraUsage += `  inbound      status | open <door> | close <door> | local [on|off] [--http addr] [--dns addr]
`
}

// buildInbound wires the doors into the app. Certificates are only ever
// requested for names the engine serves or the panel's own, so the CA's
// quota is spent on this installation and nobody else.
func buildInbound(a *app.App) app.Hook {
	// The edge's ranges come first: the inbound hook is the outermost and
	// every hook after it asks the edge who the client is.
	if n, used := edge.Load(filepath.Join(a.Root, "data", "edge", "cloudflare-ips.txt")); used {
		a.Bus.Emit(events.Event{Type: "edge.ranges", Actor: "engine", Message: fmt.Sprintf("%d edge ranges from data/edge/cloudflare-ips.txt", n)})
	}
	certDir := filepath.Join(a.Root, "data", "certs")
	solver := acme.NewHTTP01()
	alpn := acme.NewTLSALPN01()
	st, err := inbound.LoadState(a.Root)
	if err != nil {
		// A broken file must not take the doors down with it: the hook
		// keeps re-reading and the defaults apply until it parses.
		fmt.Fprintln(os.Stderr, "middleware: inbound:", err)
		st = inbound.DefaultState()
	}
	acmeIssuer = &certs.ACMEIssuer{
		Dir:       certDir,
		Directory: st.ACME.Directory,
		Email:     st.ACME.Email,
		HTTP01:    solver,
		TLSALPN:   alpn,
		UserAgent: "middleware/" + Version,
	}
	store := certs.New(certDir, acmeIssuer)
	store.ALPN = alpn
	store.Bus = a.Bus
	store.Allowed = func(domain string) bool {
		// Read on every request: the panel's name can change while the
		// process runs, and its certificate must follow.
		if panel := a.Proxy.PanelDomain(); panel != "" && domain == panel {
			return true
		}
		// Every name a site answers to, aliases and its www. included,
		// and nothing for a project, which answers none.
		_, ok := a.Engine.Hosts()[domain]
		return ok
	}
	inboundHook = inbound.New(a.Root, store, solver)
	// The names nobody serves, with the door they came by.
	if a.Proxy.Unrouted == nil {
		a.Proxy.Unrouted = proxy.NewUnrouted()
	}
	a.Proxy.DoorOf = inbound.DoorOf
	a.Proxy.ClientIP = auth.ClientIP
	inboundHook.PanelDomainNow = a.Proxy.PanelDomain
	// The VPN door: open while the VPN module is on, VPN-only sites served
	// nowhere else, narrowed to devices by their address in the tunnel.
	inboundHook.VPNAddress = func() (netip.Addr, bool) {
		if theVPN == nil {
			return netip.Addr{}, false
		}
		return theVPN.ServerAddress()
	}
	inboundHook.DeviceOf = func(ip netip.Addr) string {
		if theVPN == nil {
			return ""
		}
		return theVPN.PeerName(ip)
	}
	inboundHook.VPNOnly = func(host string) ([]string, bool) {
		sn, ok := a.Engine.Lookup(host)
		if !ok || sn.Manifest == nil || sn.Manifest.VPN == nil {
			return nil, false
		}
		return sn.Manifest.VPN.Devices, true
	}
	// Closed by this node's VPN, or by the network for every node: one
	// panel open while the others are closed is the network's way in.
	inboundHook.PanelVPNOnly = func() bool {
		return theVPN != nil && theVPN.PanelOnly() || theMesh != nil && theMesh.Settings().PanelVPNOnly
	}
	inboundHook.VPNPaths = func(host string) []string {
		sn, ok := a.Engine.Lookup(host)
		if !ok || sn.Manifest == nil || sn.Manifest.VPN == nil {
			return nil
		}
		return sn.Manifest.VPN.Paths
	}
	inboundHook.ForceHTTPS = func(host string) bool {
		sn, ok := a.Engine.Lookup(host)
		return ok && sn.Manifest.ForcesHTTPS(host)
	}
	return inboundHook
}

// inboundCmd works on the state file alone, so it needs no running
// process and no credentials: whoever can write data/ owns the doors.
// The running process notices the file's new mtime on its next request.
func inboundCmd(args []string) error {
	fs := flag.NewFlagSet("inbound", flag.ContinueOnError)
	root := fs.String("root", "", "installation root (default $MIDDLEWARE_ROOT or /srv/middleware)")
	// The local door's flags live on the one flag set because the
	// interleaved parser sees every flag before the verb is known.
	httpAddr := fs.String("http", "", "local door: plain http listener (default "+inbound.DefaultLocalHTTP+")")
	dnsAddr := fs.String("dns", "", "local door: name server, udp and tcp (default "+inbound.DefaultLocalDNS+")")
	rest, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return fmt.Errorf("usage: middleware inbound status | open <door> | close <door> | local [on|off] [--http addr] [--dns addr]")
	}
	dir := settings.Root(*root)
	switch rest[0] {
	case "status":
		return inboundStatus(dir)
	case "local":
		return inboundLocal(dir, rest[1:], *httpAddr, *dnsAddr)
	case "open", "close":
		if len(rest) != 2 {
			return fmt.Errorf("usage: middleware inbound %s <tunnel|ports|local>", rest[0])
		}
		door, err := inbound.ParseDoor(rest[1])
		if err != nil {
			return err
		}
		st, err := inbound.SetDoor(dir, door, rest[0] == "open")
		if err != nil {
			return err
		}
		state := "closed"
		if (door == inbound.Tunnel && st.Tunnel.Open) || (door == inbound.Ports && st.Ports.Open) || (door == inbound.Local && st.Local.Open) {
			state = "open"
		}
		fmt.Printf("%s door %s (%s)\n", door, state, inbound.Path(dir))
		return nil
	}
	return fmt.Errorf("unknown inbound command %q", rest[0])
}

func inboundStatus(dir string) error {
	st, err := inbound.LoadState(dir)
	if err != nil {
		return err
	}
	onOff := func(b bool) string {
		if b {
			return "open"
		}
		return "closed"
	}
	fmt.Printf("tunnel  %s\n", onOff(st.Tunnel.Open))
	fmt.Printf("ports   %s  http %s  https %s\n", onOff(st.Ports.Open), st.Ports.HTTP, st.Ports.HTTPS)
	fmt.Printf("local   %s  http %s  dns %s\n", onOff(st.Local.Open), st.Local.HTTP, st.Local.DNS)
	if st.ACME.Directory != "" {
		fmt.Printf("acme    %s\n", st.ACME.Directory)
	}
	store := certs.New(filepath.Join(dir, "data", "certs"), nil)
	rows := map[string]inbound.DomainStatus{}
	for d, door := range st.Domains {
		rows[d] = inbound.DomainStatus{Domain: d, Door: door}
	}
	for _, info := range store.List() {
		row := rows[info.Domain]
		row.Domain = info.Domain
		exp := info.Expires
		row.CertExpires = &exp
		if row.Door == "" {
			row.Door = inbound.Ports
		}
		rows[info.Domain] = row
	}
	if len(rows) == 0 {
		return nil
	}
	fmt.Println()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "DOMAIN\tDOOR\tCERTIFICATE")
	names := make([]string, 0, len(rows))
	for d := range rows {
		names = append(names, d)
	}
	sort.Strings(names)
	for _, d := range names {
		row := rows[d]
		cert := "-"
		if row.CertExpires != nil {
			left := time.Until(*row.CertExpires).Round(24*time.Hour) / (24 * time.Hour)
			cert = fmt.Sprintf("expires %s (%dd)", row.CertExpires.Format("2006-01-02"), left)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", row.Domain, row.Door, cert)
	}
	return w.Flush()
}

// inboundLocal is the development bench switch: `local on` opens the
// door, `--http`/`--dns` move its listeners, and the resolver snippet
// is printed so the operator can paste it, because the door is useless
// until the machine's resolver forwards the suffix here.
func inboundLocal(dir string, rest []string, httpAddr, dnsAddr string) error {
	cur, err := inbound.LoadState(dir)
	if err != nil {
		return err
	}
	open := cur.Local.Open
	if len(rest) > 1 {
		return fmt.Errorf("usage: middleware inbound local [on|off] [--http addr] [--dns addr]")
	}
	if len(rest) == 1 {
		switch rest[0] {
		case "on":
			open = true
		case "off":
			open = false
		default:
			return fmt.Errorf("usage: middleware inbound local [on|off] [--http addr] [--dns addr]")
		}
	}
	st, err := inbound.SetLocal(dir, open, httpAddr, dnsAddr)
	if err != nil {
		return err
	}
	state := "closed"
	if st.Local.Open {
		state = "open"
	}
	fmt.Printf("local door %s  http %s  dns %s  (%s)\n", state, st.Local.HTTP, st.Local.DNS, inbound.Path(dir))
	if st.Local.Open {
		fmt.Print(inbound.ResolverHelp(st.Local.DNS, "middleware"))
	}
	return nil
}
