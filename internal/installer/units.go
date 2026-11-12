// Package installer puts middleware on a Linux machine and checks it is
// well: `middleware install`, `middleware uninstall` and `middleware doctor`.
//
// It runs as the operator's user and reaches for sudo only where the
// kernel insists: the system group, the units, sysctl, tmpfiles and the
// package manager. Every step is re-runnable and says whether it changed
// anything, because an installer that cannot be run twice is an installer
// nobody dares to run after the first time.
package installer

import (
	"bytes"
	"fmt"
	"path/filepath"
	"text/template"
)

// Params is everything the rendered units and the steps depend on.
type Params struct {
	// User runs the supervisor and owns the root directory.
	User string
	// Group is the system group the helper socket is shared with.
	Group string
	// Root is the installation root (sites/, tasks/, data/, modules/).
	Root string
	// Binary is where the executable is installed.
	Binary string
	// Listen is the loopback address the socket unit binds.
	Listen string
	// Domain is the panel's name, asked at install: with it the operator
	// signs in at https://<domain> from the start, instead of through an
	// SSH tunnel to a loopback port. Empty keeps the panel loopback only.
	Domain string
	// Door is how that name is reached: "ports" (80/443) or "tunnel".
	Door string
	// Join is an invitation code from another node's panel: instead of a
	// domain, the node joins that network and is managed from it.
	// MeshEndpoint is this machine's ip:port for the network, empty for
	// the one the house suggests.
	Join, MeshEndpoint string
	// Browser is "yes" to install a headless Chromium when there is none
	// (for the site pictures), "no" or empty to leave it.
	Browser string
	// RunDir holds the helper socket; created by tmpfiles.d on boot.
	RunDir string
	// HelperSocket is the helper's unix socket path.
	HelperSocket string
	// Helper says whether the privileged helper unit is installed at all.
	Helper bool
	// PortsFrom/PortsTo is the site port range reserved from the kernel's
	// ephemeral range.
	PortsFrom, PortsTo int
	// Version is stamped in the units so `doctor` can tell what rendered them.
	Version string
}

// DefaultParams are what a plain `middleware install` uses.
func DefaultParams() Params {
	return Params{
		Group:        "middleware",
		Root:         "/srv/middleware",
		Binary:       "/usr/local/bin/middleware",
		Listen:       "127.0.0.1:46100",
		RunDir:       "/run/middleware",
		HelperSocket: "/run/middleware/helper.sock",
		Helper:       true,
		PortsFrom:    33000,
		PortsTo:      47000,
		Version:      "dev",
	}
}

// File names of what the installer writes outside the root.
const (
	ServiceUnit = "middleware.service"
	SocketUnit  = "middleware.socket"
	HelperUnit  = "middleware-helper.service"
	TmpfilesIn  = "middleware.conf"
	SysctlIn    = "60-middleware-reserved-ports.conf"
)

// Validate refuses parameters that would render a unit nobody can run.
func (p Params) Validate() error {
	if p.User == "" {
		return fmt.Errorf("user is empty")
	}
	if p.Group == "" {
		return fmt.Errorf("group is empty")
	}
	for _, s := range []struct{ name, v string }{{"root", p.Root}, {"binary", p.Binary}, {"run dir", p.RunDir}, {"helper socket", p.HelperSocket}} {
		if s.v == "" || s.v[0] != '/' {
			return fmt.Errorf("%s must be an absolute path, got %q", s.name, s.v)
		}
	}
	if p.Listen == "" {
		return fmt.Errorf("listen address is empty")
	}
	if p.PortsFrom <= 0 || p.PortsTo <= p.PortsFrom || p.PortsTo > 65535 {
		return fmt.Errorf("port range %d-%d is not usable", p.PortsFrom, p.PortsTo)
	}
	return nil
}

// Render produces every file the installer writes outside the root, keyed
// by file name. The helper unit is left out when Params.Helper is false.
func Render(p Params) (map[string]string, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for name, tpl := range templates {
		if name == HelperUnit && !p.Helper {
			continue
		}
		t, err := template.New(name).Parse(tpl)
		if err != nil {
			return nil, fmt.Errorf("template %s: %w", name, err)
		}
		var b bytes.Buffer
		if err := t.Execute(&b, p); err != nil {
			return nil, fmt.Errorf("render %s: %w", name, err)
		}
		out[name] = b.String()
	}
	return out, nil
}

// SocketUnitName and ServiceUnitName let the templates name the other units
// without a second copy of the constants.
func (Params) SocketUnitName() string  { return SocketUnit }
func (Params) ServiceUnitName() string { return ServiceUnit }

// BinaryDir is the folder of the executable, which the helper writes to
// when it installs an update.
func (p Params) BinaryDir() string { return filepath.Dir(p.Binary) }

var templates = map[string]string{
	ServiceUnit: `# middleware.service, rendered by middleware {{.Version}} install.
# Re-run "middleware install" to regenerate; edits here are overwritten.
[Unit]
Description=middleware supervisor
Documentation=https://github.com/dcimorra/the-middleware
After=network-online.target
Wants=network-online.target
Requires={{.SocketUnitName}}

[Service]
Type=simple
User={{.User}}
Group={{.Group}}
WorkingDirectory={{.Root}}
Environment=MIDDLEWARE_ROOT={{.Root}}
ExecStart={{.Binary}} serve --root {{.Root}}
Restart=always
RestartSec=2

# Sites outlive the supervisor. With the default KillMode=control-group a
# restart of this unit would take every site down with it; with "process"
# only the supervisor is signalled and the sites are re-adopted when it is
# back.
KillMode=process
TimeoutStopSec=20

# The unit's cgroup is the supervisor's to split: house/ (itself and the
# house's tasks), agents/ (every agent turn and what it starts, with a low
# CPU weight and a ceiling) and sites/<domain>/ (each site's processes,
# with its own share and budget). pids caps each site's processes; a unit
# without it still runs, without that cap. DelegateSubgroup is where
# systemd puts the supervisor at start. See internal/cgroups.
Delegate=cpu memory pids
DelegateSubgroup=house

# 80/443 without root: the supervisor opens them itself when the ports door
# is enabled. Nothing else needs a capability; everything else goes through
# the helper.
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE

# One descriptor per connection, per log file, per site. 1024 is reached by
# a few dozen sites on a busy afternoon.
LimitNOFILE=65536

# Hardening that does not break sites. NoNewPrivileges stays off on purpose:
# sites are children of this unit and one may legitimately run a setuid
# binary (ping, a container runtime shim). The supervisor itself never
# escalates; the helper is a separate unit for that.
# ProtectSystem and ProtectHome are also off: the root may live anywhere,
# including under the operator's home, and sites build inside it.
# ProtectKernelTunables is off too: it over-mounts parts of /proc, and the
# kernel then refuses the sites' jails a /proc of their own.
# ProtectControlGroups is off: it mounts /sys/fs/cgroup read-only for the
# whole unit, the delegated cgroup included (systemd 255 makes no
# exception for it), and the supervisor could split nothing: the agents'
# and the sites' cgroups were never made on a node with it on.
NoNewPrivileges=no
ProtectKernelModules=yes
ProtectControlGroups=no
RestrictRealtime=yes

[Install]
WantedBy=multi-user.target
`,
	SocketUnit: `# middleware.socket, rendered by middleware {{.Version}} install.
# systemd holds the loopback port and hands it to the service, so a restart
# never refuses a connection: it waits in the kernel's accept queue instead.
[Unit]
Description=middleware loopback socket

[Socket]
ListenStream={{.Listen}}
ReuseAddr=yes
NoDelay=yes
Backlog=1024

[Install]
WantedBy=sockets.target
`,
	HelperUnit: `# middleware-helper.service, rendered by middleware {{.Version}} install.
# The only thing in this installation that runs as root. It answers a closed
# list of operations (firewall, vpn, sysctl, update) on a unix socket shared with the
# {{.Group}} group, and never takes a string that reaches a shell.
[Unit]
Description=middleware privileged helper
Documentation=https://github.com/dcimorra/the-middleware/blob/main/docs/security.md
After=network-online.target
Before={{.ServiceUnitName}}

[Service]
Type=simple
User=root
# The group is the supervisor's, so the runtime directory systemd creates
# (root and this group, 0750) can be crossed by the supervisor to reach
# the socket. With the default group, root's, it could not.
Group={{.Group}}
RuntimeDirectory=middleware
RuntimeDirectoryMode=0750
RuntimeDirectoryPreserve=yes
ExecStart={{.Binary}} helper --socket {{.HelperSocket}} --allow-gid {{.Group}}
Restart=always
RestartSec=2

# Strict by default; the places the operations write are listed and
# nothing else on the filesystem is reachable for writing. The binary's
# folder is for update.install, which replaces it with a newer release
# and keeps there the one it replaced (middleware.prev) and the record
# of the update on trial (middleware.update.json) that a rollback reads.
ProtectSystem=strict
ReadWritePaths=/etc/wireguard /etc/sysctl.d {{.RunDir}} {{.BinaryDir}}
# iptables' lock, for the sites' public ports (ports.open); "-" because
# a machine whose iptables is the nft one has no such file.
ReadWritePaths=-/run/xtables.lock
ProtectHome=yes
PrivateTmp=yes
NoNewPrivileges=yes
# NET_ADMIN for nftables and wireguard, NET_RAW for the interface, SYS_ADMIN
# for sysctl writes under /proc/sys.
# CHOWN to hand the socket to the supervisor's group: without it the
# helper died at its first breath and no installation ever had one.
CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_RAW CAP_SYS_ADMIN CAP_CHOWN
AmbientCapabilities=CAP_NET_ADMIN CAP_NET_RAW CAP_SYS_ADMIN CAP_CHOWN

[Install]
WantedBy=multi-user.target
`,
	TmpfilesIn: `# middleware.conf, rendered by middleware {{.Version}} install.
# {{.RunDir}} lives on a tmpfs and is gone after a boot; this recreates it
# before the helper and the supervisor start.
d {{.RunDir}} 0750 root {{.Group}} -
`,
	SysctlIn: `# 60-middleware-reserved-ports.conf, rendered by middleware {{.Version}} install.
# The site port range sits inside the kernel's ephemeral range; without this
# any outgoing connection can grab a port a site is about to be assigned and
# the site fails to bind with no obvious culprit.
net.ipv4.ip_local_reserved_ports = {{.PortsFrom}}-{{.PortsTo}}
`,
}
