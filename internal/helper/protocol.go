// Package helper is the privileged side of the house and the client that
// talks to it. The supervisor never runs as root; everything that needs it
// (nftables, WireGuard, sysctl) is one operation in the closed list below,
// each with a typed argument struct that is validated before any binary is
// executed. A feature that needs root gets a new operation here, with its
// own test, never a wider one: the moment an operation takes a free-form
// string that reaches a shell, the socket is a root shell for whoever can
// open it.
package helper

import (
	"encoding/json"
	"net/netip"
	"time"

	"github.com/dcimorra/the-middleware/internal/wireguard"
)

// Version of the protocol, returned by ping so a client built against a
// newer binary can tell the helper service has not been restarted yet.
const Version = "1"

// DefaultSocket is where the helper service listens.
const DefaultSocket = "/run/middleware/helper.sock"

// Request is one line on the socket.
type Request struct {
	ID   string          `json:"id"`
	Op   string          `json:"op"`
	Args json.RawMessage `json:"args,omitempty"`
}

// Response is the answer line.
type Response struct {
	ID     string          `json:"id"`
	OK     bool            `json:"ok"`
	Error  string          `json:"error,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

// The closed list of operations.
const (
	OpPing            = "ping"
	OpFirewallApply   = "firewall.apply"
	OpFirewallBlock   = "firewall.block"
	OpFirewallUnblock = "firewall.unblock"
	OpFirewallStatus  = "firewall.status"
	OpVPNApply        = "vpn.apply"
	OpVPNUp           = "vpn.up"
	OpVPNDown         = "vpn.down"
	OpVPNStatus       = "vpn.status"
	OpSysctlReserve   = "sysctl.reserve_ports"
	OpUpdateInstall   = "update.install"
	OpUpdateConfirm   = "update.confirm"
	OpMeshFirewall    = "mesh.firewall"
	OpInstallRun      = "requirements.install"
	OpPortsOpen       = "ports.open"
)

// Ops lists every operation the helper answers, for the usage text and for
// tests that make sure nothing was added without a handler.
var Ops = []string{OpPing, OpFirewallApply, OpFirewallBlock, OpFirewallUnblock, OpFirewallStatus, OpVPNApply, OpVPNUp, OpVPNDown, OpVPNStatus, OpSysctlReserve, OpUpdateInstall, OpUpdateConfirm, OpMeshFirewall, OpInstallRun, OpPortsOpen}

// FirewallPolicy is the input of firewall.apply.
type FirewallPolicy struct {
	// AllowPorts are always accepted, from anywhere, before any other rule
	// except the block set: the WireGuard port and the public doors go here
	// so a vpn_only policy can never close the way in.
	AllowPorts []int `json:"allow_ports,omitempty"`
	// VPNOnly restricts PanelPorts to traffic arriving on VPNInterface (and
	// loopback, so the CLI and the tunnel keep working).
	VPNOnly      bool   `json:"vpn_only,omitempty"`
	VPNInterface string `json:"vpn_interface,omitempty"`
	PanelPorts   []int  `json:"panel_ports,omitempty"`
}

// MeshFirewallArgs is the input of mesh.firewall: the interface between
// the nodes admits the nodes' API port and nothing else. Down removes it.
type MeshFirewallArgs struct {
	Interface  string `json:"interface"`
	APIPort    int    `json:"api_port"`
	RelayPort  int    `json:"relay_port,omitempty"`
	ListenPort int    `json:"listen_port"`
	Down       bool   `json:"down,omitempty"`
}

// BlockArgs is the input of firewall.block.
type BlockArgs struct {
	IP      string `json:"ip"`
	Seconds int    `json:"seconds"`
	Reason  string `json:"reason,omitempty"`
}

// UnblockArgs is the input of firewall.unblock.
type UnblockArgs struct {
	IP string `json:"ip"`
}

// Blocked is one entry of the block set.
type Blocked struct {
	IP      string        `json:"ip"`
	Expires time.Duration `json:"expires,omitempty"` // time left; zero when the entry has no timeout
}

// FirewallStatus is the result of firewall.status.
type FirewallStatus struct {
	Blocked []Blocked `json:"blocked"`
}

// VPNApplyArgs is the input of vpn.apply.
type VPNApplyArgs struct {
	Config wireguard.ServerConfig `json:"config"`
}

// InterfaceArgs names an interface, for vpn.up, vpn.down and vpn.status.
type InterfaceArgs struct {
	Interface string `json:"interface"`
}

// VPNStatus is the result of vpn.status.
type VPNStatus struct {
	Up         bool                   `json:"up"`
	PublicKey  wireguard.Key          `json:"public_key"`
	ListenPort int                    `json:"listen_port"`
	Peers      []wireguard.PeerStatus `json:"peers"`
	Checked    time.Time              `json:"checked"`
}

// ReservePortsArgs is the input of sysctl.reserve_ports.
type ReservePortsArgs struct {
	From int `json:"from"`
	To   int `json:"to"`
}

// PingResult is the result of ping.
type PingResult struct {
	Version string `json:"version"`
	Build   string `json:"build,omitempty"`
}

// parseIP accepts a plain address; a zone or a prefix is refused because
// neither belongs in a block set element.
func parseIP(s string) (netip.Addr, error) {
	a, err := netip.ParseAddr(s)
	if err != nil {
		return a, err
	}
	if a.Zone() != "" {
		return a, errBadIP
	}
	return a.Unmap(), nil
}
