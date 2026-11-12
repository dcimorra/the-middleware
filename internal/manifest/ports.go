package manifest

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// A service's public ports: the ones it answers on the machine's own
// addresses, straight from the internet, with no proxy of the house in
// between. Everything else a site has is HTTP behind the house, on a
// loopback port; a TURN relay next to a WebRTC app is neither. Before
// these, such a relay was a container somebody started by hand on the
// machine: the house did not know it was there, did not move it with the
// site (p2p-webrtc arrived on a new node without its relay, and nothing
// said so), and its --external-ip was a line in an .env that still named
// the old node.
//
// They are the one thing in a manifest that opens the machine itself, so
// they are the owner's to grant (PublicGrant) and the house refuses a
// port it uses or another site has.

// PublicIPVar is the variable a service reads the node's public address
// from, in its command and its env: the address changes when the site
// moves, and written by hand it stayed the old node's.
const PublicIPVar = "MIDDLEWARE_PUBLIC_IP"

// NetworkHost is services.<name>.network for a container on the machine's
// own network stack.
const NetworkHost = "host"

// MaxBridgePorts is how many ports a container publishes through docker
// before the manifest is told to use the host's network instead: docker
// starts a proxy per published port, and a relay's range of thousands
// took minutes to start and a process each.
const MaxBridgePorts = 512

// PublicPort is one entry of services.<name>.ports: a port or a range,
// and its protocol.
type PublicPort struct {
	From  int    `json:"from"`
	To    int    `json:"to"`
	Proto string `json:"proto"`
}

// String is the entry as the manifest writes it: "3478/udp",
// "49160-49400/udp".
func (p PublicPort) String() string {
	if p.From == p.To {
		return strconv.Itoa(p.From) + "/" + p.Proto
	}
	return strconv.Itoa(p.From) + "-" + strconv.Itoa(p.To) + "/" + p.Proto
}

// Count is how many ports the entry is.
func (p PublicPort) Count() int { return p.To - p.From + 1 }

// Overlaps says whether two entries share a port of the same protocol.
func (p PublicPort) Overlaps(o PublicPort) bool {
	return p.Proto == o.Proto && p.From <= o.To && o.From <= p.To
}

// Has says whether port is one of the entry's.
func (p PublicPort) Has(port int) bool { return p.From <= port && port <= p.To }

// ParsePublicPort reads "3478", "3478/udp" or "49160-49400/udp". No
// protocol is tcp, as docker reads it.
func ParsePublicPort(s string) (PublicPort, error) {
	bad := func() (PublicPort, error) {
		return PublicPort{}, fmt.Errorf("%q is not a port: want 3478/udp, 3478/tcp or a range, 49160-49400/udp", s)
	}
	rng, proto, hasProto := strings.Cut(strings.TrimSpace(s), "/")
	if !hasProto {
		proto = "tcp"
	}
	if proto != "tcp" && proto != "udp" {
		return bad()
	}
	from, to, isRange := strings.Cut(rng, "-")
	if !isRange {
		to = from
	}
	a, err1 := strconv.Atoi(from)
	b, err2 := strconv.Atoi(to)
	if err1 != nil || err2 != nil || a < 1 || b > 65535 || a > b {
		return bad()
	}
	return PublicPort{From: a, To: b, Proto: proto}, nil
}

// Public says the service opens something on the machine itself: ports
// of its own, or the machine's network.
func (s *Service) Public() bool {
	return s != nil && (len(s.Ports) > 0 || s.Network != "")
}

// HostNetwork says the service's container runs on the machine's network.
func (s *Service) HostNetwork() bool { return s != nil && s.Network == NetworkHost }

// PublicPorts are the service's ports, parsed; the entries that do not
// parse are left out (Validate has refused them already).
func (s *Service) PublicPorts() []PublicPort {
	if s == nil {
		return nil
	}
	out := make([]PublicPort, 0, len(s.Ports))
	for _, raw := range s.Ports {
		if p, err := ParsePublicPort(raw); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// ServicePort is a public port and the service that has it.
type ServicePort struct {
	Service string
	PublicPort
}

// PublicPorts are every public port of the site's services that run:
// one that is stopped or disabled holds nothing.
func (m *Manifest) PublicPorts() []ServicePort {
	if m == nil || m.Disabled {
		return nil
	}
	var out []ServicePort
	for _, name := range m.ServiceNames() {
		sv := m.Services[name]
		if sv == nil || sv.Disabled {
			continue
		}
		for _, p := range sv.PublicPorts() {
			out = append(out, ServicePort{Service: name, PublicPort: p})
		}
	}
	return out
}

// HasPublic says whether any service of the site opens something on the
// machine.
func (m *Manifest) HasPublic() bool {
	if m == nil {
		return false
	}
	for _, sv := range m.Services {
		if sv.Public() {
			return true
		}
	}
	return false
}

// PublicGrant is what the site's manifest asks of the machine, one line
// per thing asked, sorted: "<service> 3478/udp", "<service> network=host".
// It is what the owner approves (internal/pubports): a manifest is a file
// in the site's folder, which the site's own agent and a delegate's shell
// can write, and a port open to the internet or a container on the
// machine's network is not theirs to give themselves. A line approved
// stays approved; a new one waits.
func (m *Manifest) PublicGrant() []string {
	if m == nil {
		return nil
	}
	var out []string
	for _, name := range m.ServiceNames() {
		sv := m.Services[name]
		if sv == nil {
			continue
		}
		if sv.Network != "" {
			out = append(out, name+" network="+sv.Network)
		}
		for _, p := range sv.PublicPorts() {
			out = append(out, name+" "+p.String())
		}
	}
	sort.Strings(out)
	return out
}

// HeldPort is a port, or a range, that something of the house holds in
// this installation, and what.
type HeldPort struct {
	From, To int
	What     string
}

// Holds says which of held shares a port with p, if one does.
func Holds(held []HeldPort, p PublicPort) (HeldPort, bool) {
	for _, h := range held {
		if p.From <= h.To && h.From <= p.To {
			return h, true
		}
	}
	return HeldPort{}, false
}

// String names the port or the range.
func (h HeldPort) String() string {
	if h.From == h.To {
		return strconv.Itoa(h.From)
	}
	return strconv.Itoa(h.From) + "-" + strconv.Itoa(h.To)
}

// reservedPort says what of the house a port is, or "": the doors, SSH,
// and the range the house gives its sites' processes on loopback, which a
// service bound to every address would take from under them. What the
// house holds only on some installations (the panel's port, the VPN's)
// is refused where it is known, at the start (engine.PublicHost).
func reservedPort(p PublicPort) string {
	for _, r := range []struct {
		port int
		what string
	}{{22, "SSH"}, {80, "the house's HTTP door"}, {443, "the house's HTTPS door"}} {
		if p.Has(r.port) {
			return fmt.Sprintf("%d is %s", r.port, r.what)
		}
	}
	if p.From <= SitePortsTo && SitePortsFrom <= p.To {
		return fmt.Sprintf("%d-%d are the ports the house gives its sites on loopback", SitePortsFrom, SitePortsTo)
	}
	return ""
}

// SitePortsFrom and SitePortsTo are the loopback range of internal/ports,
// written here because that package is the engine's and this one is
// everybody's; a test keeps the two equal.
const (
	SitePortsFrom = 33000
	SitePortsTo   = 47000
)

// validPublic checks one service's ports and network; seen are the
// entries of the services before it, to refuse two that share a port.
func (m *Manifest) validPublic(name string, sv *Service, seen *[]ServicePort) error {
	field := "services." + name
	if !sv.Public() {
		return nil
	}
	docker := runtimeOf(sv.Run.Runtime) == "docker"
	switch sv.Network {
	case "":
	case NetworkHost:
		if !docker {
			return fmt.Errorf("%s.network: host is a container's choice; a process is on the machine's network already", field)
		}
		if len(sv.Ports) == 0 {
			// With none, nothing would say what it listens on: neither
			// the firewall, nor the next site that wants the port, nor
			// the node it moves to.
			return fmt.Errorf("%s.network: host needs ports, the ones the service listens on", field)
		}
		if len(sv.Domains) > 0 {
			return fmt.Errorf("%s.network: a service on the host's network answers no names through the house (it is not on the site's network, and its port is not the house's to hand over); give the names to another service", field)
		}
	default:
		return fmt.Errorf("%s.network: %q is not host (empty is the site's own network)", field, sv.Network)
	}
	if sv.Handover != nil && *sv.Handover {
		return fmt.Errorf("%s: a service with public ports cannot hand over: two generations cannot hold the same port", field)
	}
	total := 0
	for i, raw := range sv.Ports {
		p, err := ParsePublicPort(raw)
		if err != nil {
			return fmt.Errorf("%s.ports[%d]: %v", field, i, err)
		}
		if why := reservedPort(p); why != "" {
			return fmt.Errorf("%s.ports: %s: %s", field, raw, why)
		}
		// On the machine's network a port is bound by the process itself,
		// which the house runs with no capabilities: below 1024 the bind
		// is refused, and the log says only "permission denied".
		if p.From < 1024 && (!docker || sv.HostNetwork()) {
			return fmt.Errorf("%s.ports: %s: a port below 1024 needs a container on the site's network (no network: host), where docker publishes it", field, raw)
		}
		for _, o := range *seen {
			if o.Overlaps(p) {
				if o.Service == name {
					return fmt.Errorf("%s.ports: %s overlaps %s", field, raw, o.PublicPort)
				}
				return fmt.Errorf("%s.ports: %s overlaps %s of services.%s; a port has one service", field, raw, o.PublicPort, o.Service)
			}
		}
		*seen = append(*seen, ServicePort{Service: name, PublicPort: p})
		total += p.Count()
	}
	if docker && !sv.HostNetwork() && total > MaxBridgePorts {
		return fmt.Errorf("%s.ports: %d ports through docker's proxy (one process each) is more than %d: write network: host, which a relay with a range wants anyway", field, total, MaxBridgePorts)
	}
	return nil
}

// usesPublicIP says whether a run section names $MIDDLEWARE_PUBLIC_IP in
// its command or its env.
func usesPublicIP(r *Run) bool {
	if r == nil {
		return false
	}
	has := func(s string) bool {
		return strings.Contains(s, "$"+PublicIPVar) || strings.Contains(s, "${"+PublicIPVar+"}")
	}
	for _, a := range r.Command {
		if has(a) {
			return true
		}
	}
	for _, v := range r.Env {
		if has(v) {
			return true
		}
	}
	return false
}
