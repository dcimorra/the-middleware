// Package inbound is the doors traffic comes in by. The tunnel door is
// the loopback listener the app already opens, which a reverse tunnel
// pushes to; the ports door is 80 and 443 with certificates of our own;
// the local door is the development bench: plain HTTP on this machine's
// loopback plus a name server that resolves every site here, so a
// browser reaches misite.middleware with no internet and no hosts file.
// Any of them is open, the panel answers through all of them, and an
// ACME challenge is answered on all of them, because a domain that moves
// from one door to the other gets its certificate before its DNS changes.
package inbound

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/manifest"
)

// Door names one of the three.
type Door string

// The doors.
const (
	Tunnel Door = "tunnel"
	Ports  Door = "ports"
	Local  Door = "local"
)

// ParseDoor validates a name from the CLI or the API.
func ParseDoor(s string) (Door, error) {
	switch Door(s) {
	case Tunnel, Ports, Local:
		return Door(s), nil
	}
	return "", fmt.Errorf("inbound: unknown door %q (want tunnel, ports or local)", s)
}

// ParsePublicDoor is ParseDoor for the doors a domain is put on. The
// local door serves every site at once and is nobody's "door of record".
func ParsePublicDoor(s string) (Door, error) {
	d, err := ParseDoor(s)
	if err != nil {
		return "", err
	}
	if d == Local {
		return "", fmt.Errorf("inbound: domains are not moved to the local door; it serves all of them")
	}
	return d, nil
}

// Defaults of the local door. 80 so the browser needs no port in the
// address; 5353 for DNS because 53 is usually the system resolver's, and
// the resolver is told to forward one suffix here.
const (
	DefaultLocalHTTP = "127.0.0.1:80"
	DefaultLocalDNS  = "127.0.0.1:5353"
)

// State is data/inbound.json. It is the whole decision: which doors are
// open, where the ports door listens, which CA issues, and which door each
// domain was put on. The CLI edits it with the process running and the
// process picks the change up on the next request.
type State struct {
	Tunnel struct {
		Open bool `json:"open"`
	} `json:"tunnel"`
	Ports struct {
		Open  bool   `json:"open"`
		HTTP  string `json:"http"`
		HTTPS string `json:"https"`
	} `json:"ports"`
	Local struct {
		Open bool `json:"open"`
		// HTTP is the plain listener; no TLS and no redirect to it.
		HTTP string `json:"http"`
		// DNS is where the name server answers (UDP and TCP).
		DNS string `json:"dns"`
	} `json:"local"`
	ACME struct {
		// Directory is the CA; empty is Let's Encrypt production.
		Directory string `json:"directory,omitempty"`
		// Email is the account contact.
		Email string `json:"email,omitempty"`
	} `json:"acme"`
	// NewSites is the door a site is published on when nobody names one:
	// what "publish" means on creating a site. Empty is the tunnel.
	NewSites Door `json:"new_sites,omitempty"`
	// PublicIP is the address the ports door is reached at, for the A
	// records. Empty means detect it (see internal/publish).
	PublicIP string `json:"public_ip,omitempty"`
	// Domains records the door each domain was moved to. A domain absent
	// here is on the ports door if it has a certificate and on the tunnel
	// otherwise, which is what "nobody decided yet" looks like.
	Domains map[string]Door `json:"domains,omitempty"`
	// PanelKey, when set, hides the panel from whoever does not bring
	// its key (panelkey.go).
	PanelKey PanelKey `json:"panel_key,omitzero"`
}

// DefaultState is a fresh installation: tunnel open, ports closed. The
// ports door needs CAP_NET_BIND_SERVICE and a public address, neither of
// which an installer can assume.
func DefaultState() State {
	var s State
	s.Tunnel.Open = true
	s.Ports.HTTP = ":80"
	s.Ports.HTTPS = ":443"
	s.Local.HTTP = localDefault("MIDDLEWARE_LOCAL_HTTP", DefaultLocalHTTP)
	s.Local.DNS = localDefault("MIDDLEWARE_LOCAL_DNS", DefaultLocalDNS)
	return s
}

// localDefault lets the container image put the local door on every
// interface of the container (0.0.0.0:8080, 0.0.0.0:5353): inside one,
// loopback is unreachable from the host and 80 needs a capability the
// image does not have. The file, once written, wins over the variable.
func localDefault(env, fallback string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	return fallback
}

// Path is where the file lives under a root.
func Path(root string) string { return filepath.Join(root, "data", "inbound.json") }

// LoadState reads the file, returning defaults when it does not exist.
func LoadState(root string) (State, error) {
	s := DefaultState()
	err := fsx.ReadJSON(Path(root), &s)
	if err != nil && !os.IsNotExist(err) {
		return s, err
	}
	if s.Ports.HTTP == "" {
		s.Ports.HTTP = ":80"
	}
	if s.Ports.HTTPS == "" {
		s.Ports.HTTPS = ":443"
	}
	if s.Local.HTTP == "" {
		s.Local.HTTP = localDefault("MIDDLEWARE_LOCAL_HTTP", DefaultLocalHTTP)
	}
	if s.Local.DNS == "" {
		s.Local.DNS = localDefault("MIDDLEWARE_LOCAL_DNS", DefaultLocalDNS)
	}
	return s, nil
}

// SaveState writes the file atomically after validating it.
func SaveState(root string, s State) error {
	if err := s.Validate(); err != nil {
		return err
	}
	return fsx.WriteJSON(Path(root), s, 0o644)
}

// Validate refuses what the listeners could not use.
func (s State) Validate() error {
	if s.Ports.HTTP == "" || s.Ports.HTTPS == "" {
		return fmt.Errorf("inbound: ports door needs both http and https addresses")
	}
	// Port 0 is "any free port" and two of them never collide; tests use
	// it to pick real listeners without racing for a number.
	if s.Ports.HTTP == s.Ports.HTTPS && !strings.HasSuffix(s.Ports.HTTP, ":0") {
		return fmt.Errorf("inbound: http and https cannot share %q", s.Ports.HTTP)
	}
	for _, a := range []string{s.Local.HTTP, s.Local.DNS} {
		if _, _, err := net.SplitHostPort(a); err != nil {
			return fmt.Errorf("inbound: local door address %q: %w", a, err)
		}
	}
	if s.Local.Open && s.Ports.Open && samePort(s.Local.HTTP, s.Ports.HTTP) {
		return fmt.Errorf("inbound: the local and ports doors cannot both listen on port %s", portOf(s.Local.HTTP))
	}
	if s.NewSites != "" {
		if _, err := ParsePublicDoor(string(s.NewSites)); err != nil {
			return err
		}
	}
	if s.PublicIP != "" {
		if ip := net.ParseIP(s.PublicIP); ip == nil || ip.To4() == nil {
			return fmt.Errorf("inbound: public_ip %q is not an IPv4 address", s.PublicIP)
		}
	}
	for d, door := range s.Domains {
		if !manifest.ValidDomain(d) {
			return fmt.Errorf("inbound: invalid domain %q", d)
		}
		if _, err := ParsePublicDoor(string(door)); err != nil {
			return err
		}
	}
	return nil
}

func portOf(addr string) string {
	_, p, _ := net.SplitHostPort(addr)
	return p
}

// samePort says whether two listeners would collide: the same port and
// one of them on every interface, or both on the same host. Port 0 is
// never a collision (tests).
func samePort(a, b string) bool {
	ha, pa, _ := net.SplitHostPort(a)
	hb, pb, _ := net.SplitHostPort(b)
	if pa != pb || pa == "0" {
		return false
	}
	return ha == "" || hb == "" || ha == hb || ha == "0.0.0.0" || hb == "0.0.0.0"
}

// DoorForNew is the door new sites are published on.
func (s State) DoorForNew() Door {
	if s.NewSites == "" {
		return Tunnel
	}
	return s.NewSites
}

// DoorOf returns the recorded door for a domain, or "" when nobody
// decided.
func (s State) DoorOf(domain string) Door { return s.Domains[domain] }

// Domains sorted, for stable output.
func (s State) sortedDomains() []string {
	out := make([]string, 0, len(s.Domains))
	for d := range s.Domains {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// SetDoor opens or closes a door in the file. It is what the CLI does; the
// running process sees the new mtime on its next request.
func SetDoor(root string, door Door, open bool) (State, error) {
	s, err := LoadState(root)
	if err != nil {
		return s, err
	}
	switch door {
	case Tunnel:
		s.Tunnel.Open = open
	case Ports:
		s.Ports.Open = open
	case Local:
		s.Local.Open = open
	default:
		return s, fmt.Errorf("inbound: unknown door %q", door)
	}
	return s, SaveState(root, s)
}

// SetLocal writes the local door's addresses (and open flag) in the file.
func SetLocal(root string, open bool, httpAddr, dnsAddr string) (State, error) {
	s, err := LoadState(root)
	if err != nil {
		return s, err
	}
	s.Local.Open = open
	if httpAddr != "" {
		s.Local.HTTP = httpAddr
	}
	if dnsAddr != "" {
		s.Local.DNS = dnsAddr
	}
	return s, SaveState(root, s)
}

// stateFile re-reads the file when it changed. Stat per request is cheap
// (one syscall, the inode is cached) and means a CLI edit is honoured
// without a signal, a tick, or a restart. Size is compared along with
// mtime because two writes inside one timestamp tick are possible on a
// filesystem with coarse mtimes.
//
// What every request reads is one pointer, and the stat is made with no
// lock held: every request of every site asks this, and with the syscall
// under one mutex the whole request path took turns at it, on however
// many cores. The mutex is for the readers that found the file changed.
type stateFile struct {
	path string
	mu   sync.Mutex
	cur  atomic.Pointer[stateRead]
}

// stateRead is the file as last read: never changed once stored.
type stateRead struct {
	mtime int64
	size  int64
	state State
}

func (f *stateFile) get() State {
	st, err := os.Stat(f.path)
	cur := f.cur.Load()
	if err == nil && cur != nil && st.ModTime().UnixNano() == cur.mtime && st.Size() == cur.size {
		return cur.state
	}
	if err != nil && os.IsNotExist(err) && cur != nil && cur.mtime == 0 && cur.size == 0 {
		// No file, and the defaults already stand for it.
		return cur.state
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	// Again under the lock: of the requests that found the file changed
	// at the same moment, one reads it and the rest take what it read.
	st, err = os.Stat(f.path)
	cur = f.cur.Load()
	if err != nil {
		if cur == nil || (os.IsNotExist(err) && (cur.mtime != 0 || cur.size != 0)) {
			// No file yet, or the file was removed: defaults, as a fresh
			// install.
			cur = &stateRead{state: DefaultState()}
			f.cur.Store(cur)
		}
		return cur.state
	}
	if cur != nil && st.ModTime().UnixNano() == cur.mtime && st.Size() == cur.size {
		return cur.state
	}
	root := filepath.Dir(filepath.Dir(f.path))
	s, err := LoadState(root)
	if err != nil {
		// A half-typed edit must not close every door: keep the last good
		// state until the file parses again.
		if cur == nil {
			cur = &stateRead{state: DefaultState()}
			f.cur.Store(cur)
		}
		return cur.state
	}
	f.cur.Store(&stateRead{mtime: st.ModTime().UnixNano(), size: st.Size(), state: s})
	return s
}

// set writes a new state and primes the cache so the writer's own next
// request sees it even if the mtime did not move.
func (f *stateFile) set(s State) error {
	root := filepath.Dir(filepath.Dir(f.path))
	if err := SaveState(root, s); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	next := &stateRead{state: s}
	if st, err := os.Stat(f.path); err == nil {
		next.mtime, next.size = st.ModTime().UnixNano(), st.Size()
	}
	f.cur.Store(next)
	return nil
}

// ResolverHelp is the text that makes the local door useful: how to tell
// this machine's resolver to send one suffix to the door's name server.
// Printed by the CLI and shown by the panel; the same words in both so
// an operator who read one recognises the other.
func ResolverHelp(dnsAddr, suffix string) string {
	host, port, err := net.SplitHostPort(dnsAddr)
	if err != nil {
		host, port = dnsAddr, "53"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf(`Point the resolver of this machine at the door for the suffix ".%[3]s"
(any suffix works; name your sites something.%[3]s):

  Linux (systemd-resolved):
    sudo mkdir -p /etc/systemd/resolved.conf.d
    printf '[Resolve]\nDNS=%[1]s:%[2]s\nDomains=~%[3]s\n' | sudo tee /etc/systemd/resolved.conf.d/middleware.conf
    sudo systemctl restart systemd-resolved

  macOS:
    printf 'nameserver %[1]s\nport %[2]s\n' | sudo tee /etc/resolver/%[3]s

  Windows (PowerShell as administrator):
    Add-DnsClientNrptRule -Namespace ".%[3]s" -NameServers "%[1]s"   # NRPT has no port: run the door's dns on %[1]s:53

Browsers resolve *.localhost to this machine on their own, so something.localhost
works with no resolver change at all.
`, host, port, suffix)
}
