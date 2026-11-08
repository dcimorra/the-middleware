package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/fsx"

	"github.com/dcimorra/the-middleware/internal/inbound"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/settings"
)

// setPanelDomain gives the panel its name and puts it on a door: ports
// (80 and 443, with a certificate the house obtains for that name) or
// tunnel (a Cloudflare tunnel, with no port open). Neither is required:
// the door chosen is also where new sites go. The settings go through the
// running supervisor when there is one, because it reads its settings
// once at start; without one, into the file it will read. The door's file
// is read live either way.
func (e *Env) setPanelDomain(root, domain string, door inbound.Door) error {
	domain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))
	if !manifest.ValidDomain(domain) {
		return fmt.Errorf("%q is not a domain name", domain)
	}
	if door == "" {
		door = inbound.Ports
	}
	if _, err := inbound.ParsePublicDoor(string(door)); err != nil {
		return err
	}
	// The installer starts the service just before this: give its socket
	// a moment, or the settings would go to a file the running process
	// read already and will not read again until it restarts.
	cl, err := e.Connect(root)
	for i := 0; err != nil && i < 20 && fsx.Exists(filepath.Join(root, "data", "api.sock")); i++ {
		time.Sleep(500 * time.Millisecond)
		cl, err = e.Connect(root)
	}
	running := err == nil
	if running {
		var raw []byte
		if err := cl.Do("GET", "/api/v1/settings", nil, &raw); err != nil {
			return err
		}
		var cur map[string]any
		if err := json.Unmarshal(raw, &cur); err != nil {
			return err
		}
		cur["panel_domain"] = domain
		if err := cl.Do("PUT", "/api/v1/settings", cur, nil); err != nil {
			return err
		}
	} else {
		st, err := settings.Load(root)
		if err != nil {
			return err
		}
		st.PanelDomain = domain
		if err := settings.Save(root, st); err != nil {
			return err
		}
	}
	doors, err := inbound.LoadState(root)
	if err != nil {
		return err
	}
	// Only the chosen door is opened: a house reached through the tunnel
	// has no reason to listen on 80 and 443, nor the other way round.
	if door == inbound.Ports {
		doors.Ports.Open = true
	} else {
		doors.Tunnel.Open = true
	}
	doors.NewSites = door
	if err := inbound.SaveState(root, doors); err != nil {
		return err
	}
	if door == inbound.Ports {
		fmt.Fprintf(e.Stdout, "Panel domain: %s, on the ports door (80 and 443).\n", domain)
	} else {
		fmt.Fprintf(e.Stdout, "Panel domain: %s, on the tunnel door (no port opened).\n", domain)
	}
	if !running {
		if fsx.Exists(filepath.Join(root, "data", "api.sock")) {
			fmt.Fprintln(e.Stdout, "The service did not answer: restart it (systemctl restart middleware) for the domain to apply.")
		}
	} else {
		// Publishing is the same as for a site: a DNS record (and a
		// tunnel route) when the cloudflare module is on, and a line
		// saying what to do by hand when it is not.
		var out struct {
			Lines []inbound.Line `json:"lines"`
		}
		if err := cl.Do("POST", "/api/v1/inbound/move", map[string]any{"domains": []string{domain}, "to": door}, &out); err != nil {
			fmt.Fprintf(e.Stdout, "Publish: %v\n", err)
		}
		for _, l := range out.Lines {
			fmt.Fprintf(e.Stdout, "[%s] %s\n", l.Status, l.Text)
		}
	}
	if door == inbound.Tunnel {
		st, _ := settings.Load(root)
		listen := st.Listen
		if listen == "" {
			listen = "127.0.0.1:46100"
		}
		fmt.Fprintf(e.Stdout, "The tunnel needs the cloudflare module, with tunnel_id set to this server's tunnel. Until then, reach the panel from your computer with ssh -L %s:%s <this-server>, open http://%s/ and enable it under Modules.\n", portOf(listen), listen, listen)
		return nil
	}
	fmt.Fprintln(e.Stdout, dnsLine(domain))
	return nil
}

// dnsLine says whether the name already points at this machine, because
// the certificate cannot be obtained until it does, and a sign-in page
// that does not load says nothing about why.
func dnsLine(domain string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupHost(ctx, domain)
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound || (err == nil && len(addrs) == 0) {
		return fmt.Sprintf("DNS: %s does not resolve yet. Point an A record (and AAAA, if this machine has IPv6) at this machine; the certificate follows on its own once it does.", domain)
	}
	if err != nil {
		return "DNS: could not check (" + err.Error() + ")."
	}
	mine := map[netip.Addr]bool{}
	if ifs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range ifs {
			if p, err := netip.ParsePrefix(a.String()); err == nil {
				mine[p.Addr().Unmap()] = true
			}
		}
	}
	for _, a := range addrs {
		if ip, err := netip.ParseAddr(a); err == nil && mine[ip.Unmap()] {
			return fmt.Sprintf("DNS: %s points here (%s).", domain, a)
		}
	}
	return fmt.Sprintf("DNS: %s points at %s, which is not an address of this machine. Behind a NAT or Cloudflare's proxy that can be right; otherwise point it here.", domain, strings.Join(addrs, ", "))
}
