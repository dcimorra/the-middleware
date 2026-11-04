// Package nodes joins installations into one network: a WireGuard mesh in
// which every node sees and manages every other, with no master and no
// link to make pair by pair.
//
// Each node has an interface (mwmesh) with an address in 10.66.0.0/16 and
// one WireGuard peer per other node, addressed by its /32 only: what goes
// to another node goes through the tunnel, encrypted and authenticated by
// key, and nothing else of the machine's traffic changes. The nodes' API
// listens on that address alone and the helper closes the interface to any
// other port, so being in the network gives a node the middleware of the
// others and nothing more of their machines.
//
// Membership is a roster every node keeps and pulls from the others every
// twenty seconds: an entry per node, which only that node changes, and a
// removal is an entry marked removed, final until the node joins again
// (mergeLocked says why each rule is there). Joining one
// node is joining all of them: the others learn the newcomer from the
// roster within a round, and the newcomer learns them from the node that
// invited it.
//
// An invitation carries a key pair made for the newcomer, so the first
// contact is already through the tunnel and no part of the joining crosses
// the internet in clear or through the public panel. The newcomer replaces
// that key with its own as soon as it is in: a code read later from a chat
// log opens nothing.
package nodes

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/helper"
	"github.com/dcimorra/the-middleware/internal/wireguard"
)

// Defaults of the mesh.
const (
	// Interface is the mesh's WireGuard interface; the VPN for people has
	// its own, and the two never share peers or addresses.
	Interface = "mwmesh"
	// DefaultPort is the UDP port the mesh listens on: one above the VPN's,
	// because two interfaces cannot share one.
	DefaultPort = 51821
	// DefaultAPIPort is where the nodes' API listens, on the mesh address
	// only.
	DefaultAPIPort = 46101
	// DefaultRelayPort is where a node takes the connections another
	// node's VPN door relays to it (relay.go), on the mesh address only.
	DefaultRelayPort = 46102
	// Keepalive keeps a node behind NAT reachable and every tunnel warm, at
	// the cost of one small packet every 25 seconds per node.
	Keepalive = 25
	// InviteTTL is how long an invitation works.
	InviteTTL = 15 * time.Minute
	// GossipEvery is how often a node reads the others' rosters.
	GossipEvery = 20 * time.Second
)

// DefaultSubnet is the mesh's network. Not the VPN's 10.77.0.0/24, not
// Docker's, not a home router's; a /16 so addresses can be drawn at random
// and two nodes inviting at the same moment do not hand out the same one.
var DefaultSubnet = netip.MustParsePrefix("10.66.0.0/16")

// Errors the API maps to statuses.
var (
	ErrNotFound    = errors.New("no such node")
	ErrBadInvite   = errors.New("the invitation code is not valid or has expired")
	ErrNoNetwork   = errors.New("this node is not in a network")
	ErrInNetwork   = errors.New("this node is already in a network; leave it first")
	ErrBadName     = errors.New("a node's name is 1 to 40 characters")
	ErrNoEndpoint  = errors.New("the public address of this node (ip:port) is needed")
	ErrBadEndpoint = errors.New("the public address must be ip:port or host:port")
	ErrNoHelper    = errors.New("the mesh needs the helper service (WireGuard and nftables run as root there)")
)

// Member is one node of the network as the roster has it.
type Member struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	IP        netip.Addr    `json:"ip"`
	PublicKey wireguard.Key `json:"public_key"`
	// Endpoint is the node's public ip:port for WireGuard; empty when it
	// is behind NAT and only ever calls out.
	Endpoint string    `json:"endpoint,omitempty"`
	Version  string    `json:"version,omitempty"`
	Updated  time.Time `json:"updated"`
	Removed  bool      `json:"removed,omitempty"`
	// Joined and By are the admission: when the node let in through an
	// invitation, and which node let it in. They are the one way a
	// removed id comes back (mergeLocked), and travel unchanged with the
	// entry whoever relays it.
	Joined time.Time `json:"joined,omitzero"`
	By     string    `json:"by,omitempty"`
}

// MaxSkew is how far ahead of this node's clock a roster entry may be
// dated. An honest node's clock is off by seconds; an entry dated in the
// year 2100 is a node that wants to win every comparison from now on.
const MaxSkew = time.Minute

// Self is this installation's identity, which outlives any network.
type Self struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type invite struct {
	Hash      string        `json:"hash"`
	IP        netip.Addr    `json:"ip"`
	PublicKey wireguard.Key `json:"public_key"`
	Expires   time.Time     `json:"expires"`
}

// state is data/nodes/mesh.json. It holds the private key and the
// network's secret, hence 0600.
type state struct {
	Network    string        `json:"network,omitempty"`
	Secret     string        `json:"secret,omitempty"`
	PrivateKey wireguard.Key `json:"private_key"`
	IP         netip.Addr    `json:"ip"`
	Port       int           `json:"port,omitempty"`
	Endpoint   string        `json:"endpoint,omitempty"`
	// Updated dates this node's own entry in the roster; Version is the
	// one it was dated with, so an update is announced like a rename.
	Updated time.Time `json:"updated"`
	Version string    `json:"version,omitempty"`
	Members []Member  `json:"members,omitempty"`
	Invites []invite  `json:"invites,omitempty"`
	// Settings are the network's, the same on every node.
	Settings NetSettings `json:"settings,omitzero"`
}

// Network is what the mesh needs from the machine: the interface and its
// firewall. The helper in production, a recorder in tests.
type Network interface {
	Apply(ctx context.Context, cfg wireguard.ServerConfig, restart bool) error
	Down(ctx context.Context) error
	Firewall(ctx context.Context, a helper.MeshFirewallArgs) error
	Status(ctx context.Context) (helper.VPNStatus, error)
}

// Options configure a Mesh.
type Options struct {
	Dir      string
	Hostname string
	Version  string
	Net      Network
	Subnet   netip.Prefix
	APIPort  int
	// RelayPort is the relay door's (relay.go).
	RelayPort int
	// Names lists what this node answers for (its sites and its panel),
	// for the other nodes' VPN doors.
	Names func() []string
	// OnSettings runs when the network's settings change, here or by a
	// round, and once at start.
	OnSettings func(NetSettings)
	// OnRelay serves a connection relayed by another node's VPN door, for
	// a VPN device of that node.
	OnRelay func(c net.Conn, device string)
	// PublicIP finds this machine's public address, to suggest the
	// endpoint; nil suggests nothing.
	PublicIP func(ctx context.Context) (string, error)
	Bus      *events.Bus
}

// Mesh is this node's membership.
type Mesh struct {
	opts Options
	now  func() time.Time

	mu      sync.Mutex
	self    Self
	st      state
	applied struct {
		ip   netip.Addr
		port int
	}
	dirty   bool
	lastErr string
	seen    map[string]time.Time // member id -> last roster read from it
	suggest struct {
		at       time.Time
		endpoint string
	}

	applyMu sync.Mutex
	door    *doorServer
	api     http.Handler
	// sites serves the requests another node hands to a replica here
	// (cluster.go).
	sites http.Handler
	// tunnels serves a replica's connections to its primary's database.
	tunnels http.Handler
	// trans is the HTTP transport bound to the mesh address, rebuilt when
	// the address changes.
	trans   *http.Transport
	transIP netip.Addr
	// names are what each member answers for, as read in the last round.
	names map[string][]string
	relay net.Listener
}

// Open reads the mesh state, creating this node's identity the first time.
func Open(opts Options) (*Mesh, error) {
	if !opts.Subnet.IsValid() {
		opts.Subnet = DefaultSubnet
	}
	if opts.APIPort == 0 {
		opts.APIPort = DefaultAPIPort
	}
	if opts.RelayPort == 0 {
		opts.RelayPort = DefaultRelayPort
	}
	m := &Mesh{opts: opts, now: time.Now, seen: map[string]time.Time{}, names: map[string][]string{}}
	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		return nil, err
	}
	if err := fsx.ReadJSON(m.selfPath(), &m.self); err != nil && !fsx.IsNotExist(err) {
		return nil, err
	}
	if m.self.ID == "" {
		m.self.ID = random(8)
		m.self.Name = cleanName(opts.Hostname)
		if m.self.Name == "" {
			m.self.Name = "node-" + m.self.ID[:6]
		}
		if err := fsx.WriteJSON(m.selfPath(), m.self, 0o600); err != nil {
			return nil, err
		}
	}
	if err := fsx.ReadJSON(m.statePath(), &m.st); err != nil && !fsx.IsNotExist(err) {
		return nil, err
	}
	m.dirty = m.st.Network != ""
	if m.st.Network != "" && m.st.Version != opts.Version {
		m.st.Version, m.st.Updated = opts.Version, m.now()
		_ = m.saveLocked()
	}
	return m, nil
}

func (m *Mesh) selfPath() string  { return filepath.Join(m.opts.Dir, "self.json") }
func (m *Mesh) statePath() string { return filepath.Join(m.opts.Dir, "mesh.json") }

// LegacyPath is where the links of the first design (tokens over HTTPS)
// were kept; wire revokes their tokens and removes it.
func (m *Mesh) LegacyPath() string { return filepath.Join(m.opts.Dir, "peers.json") }

func (m *Mesh) emit(typ, level, subject, msg string) {
	if m.opts.Bus != nil {
		m.opts.Bus.Emit(events.Event{Type: typ, Actor: "engine", Level: level, Subject: subject, Message: msg})
	}
}

func (m *Mesh) saveLocked() error { return fsx.WriteJSON(m.statePath(), m.st, 0o600) }

// Self is this node.
func (m *Mesh) Self() Self {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.self
}

// InNetwork says whether this node belongs to a mesh.
func (m *Mesh) InNetwork() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.st.Network != ""
}

func (m *Mesh) selfMemberLocked() Member {
	pub, _ := m.st.PrivateKey.Public()
	return Member{ID: m.self.ID, Name: m.self.Name, IP: m.st.IP, PublicKey: pub, Endpoint: m.st.Endpoint, Version: m.opts.Version, Updated: m.st.Updated}
}

// Members are the other live nodes, by name.
func (m *Mesh) Members() []Member {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.liveLocked()
}

func (m *Mesh) liveLocked() []Member {
	out := []Member{}
	for _, x := range m.st.Members {
		if !x.Removed && x.ID != m.self.ID {
			out = append(out, x)
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

// Member is one live node by id.
func (m *Mesh) Member(id string) (Member, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.liveLocked() {
		if x.ID == id {
			return x, true
		}
	}
	return Member{}, false
}

// SetName renames this node; the others read it in the next round.
func (m *Mesh) SetName(name string) error {
	name = cleanName(name)
	if name == "" {
		return ErrBadName
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	next := m.self
	next.Name = name
	if err := fsx.WriteJSON(m.selfPath(), next, 0o600); err != nil {
		return err
	}
	m.self = next
	if m.st.Network != "" {
		m.st.Updated = m.now()
		return m.saveLocked()
	}
	return nil
}

// Info is what the panel shows of the mesh.
type Info struct {
	Self      Member         `json:"self"`
	Network   string         `json:"network,omitempty"`
	Port      int            `json:"port,omitempty"`
	APIPort   int            `json:"api_port"`
	Members   []MemberStatus `json:"members"`
	Invites   int            `json:"invites"`
	Up        bool           `json:"up"`
	Error     string         `json:"error,omitempty"`
	Suggested string         `json:"suggested_endpoint,omitempty"`
	Settings  NetSettings    `json:"settings"`
}

// MemberStatus is a member with what the tunnel says about it.
type MemberStatus struct {
	Member
	Handshake time.Time `json:"handshake,omitzero"`
	Online    bool      `json:"online"`
	LastRead  time.Time `json:"last_read,omitzero"`
}

// Info reads the mesh and, when in one, the interface.
func (m *Mesh) Info(ctx context.Context) Info {
	m.mu.Lock()
	in := Info{Self: m.selfMemberLocked(), Network: m.st.Network, Port: m.st.Port, APIPort: m.opts.APIPort, Error: m.lastErr, Members: []MemberStatus{}, Settings: m.st.Settings}
	if in.Network == "" {
		in.Self = Member{ID: m.self.ID, Name: m.self.Name}
	}
	now := m.now()
	for _, iv := range m.st.Invites {
		if now.Before(iv.Expires) {
			in.Invites++
		}
	}
	for _, x := range m.liveLocked() {
		in.Members = append(in.Members, MemberStatus{Member: x, LastRead: m.seen[x.ID]})
	}
	m.mu.Unlock()
	in.Suggested = m.SuggestEndpoint(ctx)
	if in.Network == "" || m.opts.Net == nil {
		return in
	}
	st, err := m.opts.Net.Status(ctx)
	if err != nil {
		in.Error = err.Error()
		return in
	}
	in.Up = st.Up
	byKey := map[wireguard.Key]wireguard.PeerStatus{}
	for _, p := range st.Peers {
		byKey[p.PublicKey] = p
	}
	for i := range in.Members {
		if p, ok := byKey[in.Members[i].PublicKey]; ok {
			in.Members[i].Handshake = p.LastHandshake
			in.Members[i].Online = p.Online(now)
		}
	}
	return in
}

// SuggestEndpoint is this machine's public address with the mesh port,
// asked at most every ten minutes.
func (m *Mesh) SuggestEndpoint(ctx context.Context) string {
	m.mu.Lock()
	port := m.st.Port
	if port == 0 {
		port = DefaultPort
	}
	if m.now().Sub(m.suggest.at) < 10*time.Minute && m.suggest.endpoint != "" {
		e := m.suggest.endpoint
		m.mu.Unlock()
		return e
	}
	m.mu.Unlock()
	if m.opts.PublicIP == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	ip, err := m.opts.PublicIP(ctx)
	if err != nil || ip == "" {
		return ""
	}
	e := net.JoinHostPort(ip, fmt.Sprint(port))
	m.mu.Lock()
	m.suggest.at, m.suggest.endpoint = m.now(), e
	m.mu.Unlock()
	return e
}

// code is what an invitation code carries.
type code struct {
	Network string        `json:"n"`
	Subnet  netip.Prefix  `json:"s"`
	IP      netip.Addr    `json:"ip"`
	Key     wireguard.Key `json:"k"`
	Secret  string        `json:"x"`
	APIPort int           `json:"a"`
	Inviter Member        `json:"i"`
}

// Invite makes a code for another machine to join. Without a network this
// node creates one first, which needs its public endpoint (ip:port).
func (m *Mesh) Invite(ctx context.Context, endpoint string) (string, time.Time, error) {
	if m.opts.Net == nil {
		return "", time.Time{}, ErrNoHelper
	}
	m.mu.Lock()
	if m.st.Network == "" {
		endpoint = strings.TrimSpace(endpoint)
		if endpoint == "" {
			m.mu.Unlock()
			return "", time.Time{}, ErrNoEndpoint
		}
		if !wireguard.ValidEndpoint(endpoint) {
			m.mu.Unlock()
			return "", time.Time{}, ErrBadEndpoint
		}
		key, err := wireguard.GeneratePrivateKey()
		if err != nil {
			m.mu.Unlock()
			return "", time.Time{}, err
		}
		ip, err := m.freeIPLocked()
		if err != nil {
			m.mu.Unlock()
			return "", time.Time{}, err
		}
		m.st = state{Network: random(8), Secret: random(32), PrivateKey: key, IP: ip, Port: portOf(endpoint), Endpoint: endpoint, Updated: m.now(), Version: m.opts.Version}
		m.emit("mesh.created", "", m.self.Name, "a network of nodes was created with this node in it, at "+ip.String())
	}
	key, err := wireguard.GeneratePrivateKey()
	if err != nil {
		m.mu.Unlock()
		return "", time.Time{}, err
	}
	pub, _ := key.Public()
	ip, err := m.freeIPLocked()
	if err != nil {
		m.mu.Unlock()
		return "", time.Time{}, err
	}
	secret := random(32)
	exp := m.now().Add(InviteTTL)
	m.st.Invites = append(m.pruneInvitesLocked(), invite{Hash: hash(secret), IP: ip, PublicKey: pub, Expires: exp})
	c := code{Network: m.st.Network, Subnet: m.opts.Subnet, IP: ip, Key: key, Secret: secret, APIPort: m.opts.APIPort, Inviter: m.selfMemberLocked()}
	m.dirty = true
	err = m.saveLocked()
	m.mu.Unlock()
	if err != nil {
		return "", time.Time{}, err
	}
	// The newcomer's key must be a peer here before it knocks.
	if err := m.Apply(ctx); err != nil {
		return "", time.Time{}, err
	}
	raw, _ := json.Marshal(c)
	return "mwmesh_" + base64.RawURLEncoding.EncodeToString(raw), exp, nil
}

func (m *Mesh) pruneInvitesLocked() []invite {
	now := m.now()
	out := []invite{}
	for _, iv := range m.st.Invites {
		if now.Before(iv.Expires) {
			out = append(out, iv)
		}
	}
	return out
}

// freeIPLocked draws an unused address of the subnet at random.
func (m *Mesh) freeIPLocked() (netip.Addr, error) {
	used := map[netip.Addr]bool{m.st.IP: true}
	for _, x := range m.st.Members {
		used[x.IP] = true
	}
	for _, iv := range m.st.Invites {
		used[iv.IP] = true
	}
	p := m.opts.Subnet.Masked()
	base := p.Addr().As4()
	hostBits := 32 - p.Bits()
	if hostBits < 2 || hostBits > 24 {
		return netip.Addr{}, errors.New("the mesh subnet must be between /8 and /30")
	}
	n := binary.BigEndian.Uint32(base[:])
	for range 200 {
		var b [4]byte
		_, _ = rand.Read(b[:])
		off := binary.BigEndian.Uint32(b[:]) & (1<<hostBits - 1)
		if off == 0 || off == 1<<hostBits-1 {
			continue
		}
		var out [4]byte
		binary.BigEndian.PutUint32(out[:], n|off)
		a := netip.AddrFrom4(out)
		if !used[a] {
			return a, nil
		}
	}
	return netip.Addr{}, errors.New("no free address in the mesh subnet")
}

func portOf(endpoint string) int {
	if ap, err := netip.ParseAddrPort(endpoint); err == nil {
		return int(ap.Port())
	}
	if i := strings.LastIndex(endpoint, ":"); i > 0 {
		var p int
		if _, err := fmt.Sscan(endpoint[i+1:], &p); err == nil && p > 0 && p < 65536 {
			return p
		}
	}
	return DefaultPort
}

// joinRequest is what the newcomer sends through the tunnel.
type joinRequest struct {
	Secret    string        `json:"secret"`
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Endpoint  string        `json:"endpoint"`
	PublicKey wireguard.Key `json:"public_key"`
	Version   string        `json:"version"`
}

type joinAnswer struct {
	Network  string      `json:"network"`
	Secret   string      `json:"secret"`
	Roster   []Member    `json:"roster"`
	Settings NetSettings `json:"settings"`
}

// Join enters the network a code was made in. endpoint is this node's
// public ip:port, empty when it is behind NAT (it then only calls out).
func (m *Mesh) Join(ctx context.Context, raw, endpoint string) (Member, error) {
	if m.opts.Net == nil {
		return Member{}, ErrNoHelper
	}
	c, err := decodeCode(raw)
	if err != nil {
		return Member{}, err
	}
	endpoint = strings.TrimSpace(endpoint)
	if endpoint != "" && !wireguard.ValidEndpoint(endpoint) {
		return Member{}, ErrBadEndpoint
	}
	m.mu.Lock()
	if m.st.Network != "" {
		m.mu.Unlock()
		return Member{}, ErrInNetwork
	}
	if c.Inviter.ID == m.self.ID {
		m.mu.Unlock()
		return Member{}, errors.New("that code was made by this same node")
	}
	if c.Subnet.IsValid() && c.Subnet != m.opts.Subnet {
		m.mu.Unlock()
		return Member{}, fmt.Errorf("that network uses %s and this node %s", c.Subnet, m.opts.Subnet)
	}
	port := DefaultPort
	if endpoint != "" {
		port = portOf(endpoint)
	}
	// In with the key that came in the code, the inviter as the only peer.
	m.st = state{Network: c.Network, PrivateKey: c.Key, IP: c.IP, Port: port, Endpoint: endpoint, Updated: m.now(), Version: m.opts.Version, Members: []Member{c.Inviter}}
	m.dirty = true
	self := m.self
	m.mu.Unlock()
	fail := func(err error) (Member, error) {
		m.mu.Lock()
		m.st = state{}
		m.dirty = true
		m.mu.Unlock()
		_ = m.Apply(context.Background())
		return Member{}, err
	}
	if err := m.Apply(ctx); err != nil {
		return fail(err)
	}
	// And at once out of it: the key the code carried is replaced by one
	// that never left this machine, announced in the join itself.
	key, err := wireguard.GeneratePrivateKey()
	if err != nil {
		return fail(err)
	}
	pub, _ := key.Public()
	req := joinRequest{Secret: c.Secret, ID: self.ID, Name: self.Name, Endpoint: endpoint, PublicKey: pub, Version: m.opts.Version}
	var ans joinAnswer
	deadline := time.Now().Add(40 * time.Second)
	for {
		cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
		err = m.call(cctx, c.Inviter.IP, "", "POST", "/mesh/join", req, &ans)
		cancel()
		var re *RemoteError
		if err == nil || errors.As(err, &re) || time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
		// The first handshake takes a moment, and an endpoint that does not
		// answer takes all of it: retry until the deadline.
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		return fail(fmt.Errorf("the inviting node (%s, %s) did not answer through the tunnel: %v. Check that UDP %d is open on both machines and in any cloud firewall", c.Inviter.Name, c.Inviter.Endpoint, err, portOf(c.Inviter.Endpoint)))
	}
	m.mu.Lock()
	m.st.PrivateKey = key
	m.st.Secret = ans.Secret
	m.st.Members = nil
	m.mergeLocked(c.Inviter.ID, ans.Roster)
	m.st.Settings = ans.Settings
	m.dirty = true
	err = m.saveLocked()
	m.mu.Unlock()
	if err != nil {
		return fail(err)
	}
	if err := m.Apply(ctx); err != nil {
		return Member{}, err
	}
	m.emit("mesh.joined", "", c.Inviter.Name, "this node joined the network of "+c.Inviter.Name)
	m.settingsChanged()
	return c.Inviter, nil
}

func decodeCode(raw string) (code, error) {
	raw = strings.TrimSpace(raw)
	var c code
	if !strings.HasPrefix(raw, "mwmesh_") {
		return c, ErrBadInvite
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(raw, "mwmesh_"))
	if err != nil || json.Unmarshal(b, &c) != nil || c.Network == "" || c.Secret == "" || !c.IP.IsValid() || c.Key.IsZero() || !c.Inviter.IP.IsValid() || c.Inviter.PublicKey.IsZero() {
		return code{}, ErrBadInvite
	}
	return c, nil
}

// admit is the inviting side of a join, answered on the mesh door to the
// address the invitation was made for.
func (m *Mesh) admit(from netip.Addr, req joinRequest) (joinAnswer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h := hash(req.Secret)
	idx := -1
	for i, iv := range m.st.Invites {
		if subtle.ConstantTimeCompare([]byte(iv.Hash), []byte(h)) == 1 && iv.IP == from && m.now().Before(iv.Expires) {
			idx = i
		}
	}
	if idx < 0 || req.ID == "" || req.ID == m.self.ID || req.PublicKey.IsZero() {
		return joinAnswer{}, ErrBadInvite
	}
	if req.Endpoint != "" && !wireguard.ValidEndpoint(req.Endpoint) {
		return joinAnswer{}, ErrBadEndpoint
	}
	iv := m.st.Invites[idx]
	m.st.Invites = append(m.st.Invites[:idx], m.st.Invites[idx+1:]...)
	name := cleanName(req.Name)
	if name == "" {
		name = "node-" + req.ID[:min(6, len(req.ID))]
	}
	nm := Member{ID: req.ID, Name: name, IP: iv.IP, PublicKey: req.PublicKey, Endpoint: req.Endpoint, Version: req.Version, Updated: m.now(), Joined: m.now(), By: m.self.ID}
	kept := m.st.Members[:0]
	for _, x := range m.st.Members {
		if x.ID != nm.ID {
			kept = append(kept, x)
		}
	}
	m.st.Members = append(kept, nm)
	m.dirty = true
	if err := m.saveLocked(); err != nil {
		return joinAnswer{}, err
	}
	m.emit("mesh.member_added", "", name, "node "+name+" joined the network through this node")
	return joinAnswer{Network: m.st.Network, Secret: m.st.Secret, Roster: m.rosterLocked(), Settings: m.st.Settings}, nil
}

func (m *Mesh) rosterLocked() []Member {
	out := append([]Member{m.selfMemberLocked()}, m.st.Members...)
	return out
}

// mergeLocked takes in the roster read from the member from (its id). It
// reports whether anything changed and whether this node's own entry says
// it was removed by someone after its last change.
//
// It used to be "per node, the newest entry wins", from whoever said it.
// A node can write anything in the roster it serves, and that made one
// compromised node the network's master: it dated its own entry in 2100
// and no expel could ever be newer, and it could rewrite another node's
// key so that node's address went through the tunnel to it. So now:
//
//   - every date is clamped to MaxSkew ahead of this clock;
//   - a node it does not know yet is taken from whoever names it, since
//     that is how a node joined through another is learnt by all; but
//     once known, its entry changes only when read from that node itself
//     (every live member is read directly every round, so nothing honest
//     waits for a relay);
//   - a removal is taken from anyone, as any member may expel any other,
//     unless the node's admission is newer than it;
//   - a removal is final: a removed id comes back only through a new
//     join, an entry admitted by the node it is read from, dated after
//     the removal, at an address nobody has had. A node cannot admit
//     itself, so an expelled node cannot vote itself back in;
//   - a live entry must have an address in the mesh's subnet that no
//     other node has: the address goes into WireGuard's AllowedIPs, and a
//     node that announced a public address, or a neighbour's, would have
//     that traffic routed to it.
//
// What it cannot stop without signatures, which the roster does not
// carry: a member introducing a node nobody knows yet under any key it
// likes, or expelling one. Both are things any member may do anyway, by
// inviting or by expelling.
func (m *Mesh) mergeLocked(from string, in []Member) (changed, expelled bool) {
	subnet := m.opts.Subnet
	if !subnet.IsValid() {
		subnet = DefaultSubnet
	}
	limit := m.now().Add(MaxSkew)
	for _, r := range in {
		if r.ID == "" || !r.IP.IsValid() || r.PublicKey.IsZero() {
			continue
		}
		if r.Updated.After(limit) {
			r.Updated = limit
		}
		if r.Joined.After(limit) {
			r.Joined = limit
		}
		if r.ID == m.self.ID {
			if r.Removed && r.Updated.After(m.st.Updated) {
				expelled = true
			}
			continue
		}
		i := slices.IndexFunc(m.st.Members, func(x Member) bool { return x.ID == r.ID })
		if i < 0 {
			if r.Removed || m.addressFreeLocked(r.ID, r.IP, subnet) {
				m.st.Members = append(m.st.Members, r)
				changed = true
			}
			continue
		}
		cur := m.st.Members[i]
		// A join is admitted by the node it is read from, never by the
		// node itself, and newer than what is known of the node: after
		// its removal, or after its last admission.
		since := cur.Joined
		if cur.Removed {
			since = cur.Updated
		}
		join := !r.Removed && r.By != "" && r.By == from && r.By != r.ID && r.Joined.After(since)
		switch {
		case join:
			// A fresh address too, its own old one included: an admission
			// always draws one nobody had (freeIPLocked), and one that
			// kept the old address with another key would take over the
			// node it names.
			if r.IP == cur.IP || !m.addressFreeLocked(r.ID, r.IP, subnet) {
				continue
			}
		case cur.Removed:
			continue
		case r.Removed:
			// Newer than the admission, not than the node's last word
			// about itself: that one the node dates, and a node that
			// dated itself a minute ahead every round would outrun every
			// expel. An entry from before admissions were dated has none,
			// and any removal ends it; what that costs is a node that left
			// and came back before this version, while another was off
			// the whole time with the old removal: it would bring that
			// removal back, and the node joins again.
			if !r.Updated.After(cur.Joined) {
				continue
			}
			// Only the mark and its date: the address and key stay what
			// they were, so the address stays taken.
			upd := cur
			upd.Removed, upd.Updated = true, r.Updated
			r = upd
		default:
			if from != r.ID || !r.Updated.After(cur.Updated) || !m.addressFreeLocked(r.ID, r.IP, subnet) {
				continue
			}
			r.Joined, r.By = cur.Joined, cur.By
		}
		// The same entry dated later is not a change: a node that dates
		// itself ahead would otherwise have the interface applied anew
		// every round.
		same := cur
		same.Updated = r.Updated
		m.st.Members[i] = r
		if same != r {
			changed = true
		}
	}
	if changed {
		m.dirty = true
	}
	return changed, expelled
}

// addressFreeLocked says whether ip may be the mesh address of the node
// id: inside the subnet, and nobody else's, this node's, a pending
// invitation's or a removed node's (whose address freeIPLocked does not
// hand out again either).
func (m *Mesh) addressFreeLocked(id string, ip netip.Addr, subnet netip.Prefix) bool {
	if !subnet.Contains(ip) || ip == m.st.IP {
		return false
	}
	for _, x := range m.st.Members {
		if x.ID != id && x.IP == ip {
			return false
		}
	}
	for _, iv := range m.st.Invites {
		if iv.IP == ip {
			return false
		}
	}
	return true
}

// Expel removes a node from the network: here at once, everywhere within a
// round, and on the node itself if it can still be told.
func (m *Mesh) Expel(ctx context.Context, id string) (Member, error) {
	x, ok := m.Member(id)
	if !ok {
		return Member{}, ErrNotFound
	}
	cctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	told := m.Call(cctx, x, "POST", "/mesh/expelled", map[string]any{}, nil)
	cancel()
	m.mu.Lock()
	for i := range m.st.Members {
		if m.st.Members[i].ID == id {
			m.st.Members[i].Removed = true
			m.st.Members[i].Updated = m.now()
		}
	}
	m.dirty = true
	err := m.saveLocked()
	m.mu.Unlock()
	if err != nil {
		return x, err
	}
	msg := "node " + x.Name + " was removed from the network"
	if told != nil {
		msg += " (it could not be told: " + told.Error() + ")"
	}
	m.emit("mesh.member_removed", "", x.Name, msg)
	return x, m.Apply(ctx)
}

// Leave takes this node out of the network, telling the others first.
func (m *Mesh) Leave(ctx context.Context) error {
	if !m.InNetwork() {
		return ErrNoNetwork
	}
	var wg sync.WaitGroup
	for _, x := range m.Members() {
		wg.Add(1)
		go func(x Member) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 6*time.Second)
			defer cancel()
			_ = m.Call(cctx, x, "POST", "/mesh/leave", map[string]any{}, nil)
		}(x)
	}
	wg.Wait()
	m.wipe("mesh.left", "this node left the network")
	return nil
}

// wipe forgets the network and takes the interface down.
func (m *Mesh) wipe(typ, msg string) {
	m.mu.Lock()
	m.st = state{}
	m.dirty = true
	_ = m.saveLocked()
	name := m.self.Name
	m.mu.Unlock()
	_ = m.Apply(context.Background())
	m.emit(typ, "warn", name, msg)
	m.settingsChanged()
}

// left records that a member said it is leaving.
func (m *Mesh) left(id string) {
	m.mu.Lock()
	var name string
	for i := range m.st.Members {
		if m.st.Members[i].ID == id {
			m.st.Members[i].Removed = true
			m.st.Members[i].Updated = m.now()
			name = m.st.Members[i].Name
		}
	}
	m.dirty = true
	_ = m.saveLocked()
	m.mu.Unlock()
	m.emit("mesh.member_removed", "", name, "node "+name+" left the network")
}

// Apply puts the interface and its firewall as the state says, or takes
// them down outside a network.
func (m *Mesh) Apply(ctx context.Context) error {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	if m.opts.Net == nil {
		return ErrNoHelper
	}
	m.mu.Lock()
	st := m.st
	st.Invites = m.pruneInvitesLocked()
	live := m.liveLocked()
	restart := m.applied.ip.IsValid() && (m.applied.ip != st.IP || m.applied.port != st.Port)
	m.mu.Unlock()
	var err error
	if st.Network == "" {
		err = errors.Join(m.opts.Net.Firewall(ctx, helper.MeshFirewallArgs{Interface: Interface, Down: true}), m.opts.Net.Down(ctx))
		m.closeDoor()
		m.closeRelay()
		m.mu.Lock()
		m.applied.ip, m.applied.port = netip.Addr{}, 0
	} else {
		cfg := wireguard.ServerConfig{Interface: Interface, PrivateKey: st.PrivateKey, Address: netip.PrefixFrom(st.IP, m.opts.Subnet.Bits()), ListenPort: st.Port}
		for _, x := range live {
			cfg.Peers = append(cfg.Peers, wireguard.Peer{Name: "node-" + safeID(x.ID), PublicKey: x.PublicKey, AllowedIPs: []netip.Prefix{netip.PrefixFrom(x.IP, 32)}, Endpoint: x.Endpoint, PersistentKeepalive: Keepalive})
		}
		for i, iv := range st.Invites {
			cfg.Peers = append(cfg.Peers, wireguard.Peer{Name: fmt.Sprintf("invite-%d", i), PublicKey: iv.PublicKey, AllowedIPs: []netip.Prefix{netip.PrefixFrom(iv.IP, 32)}, PersistentKeepalive: Keepalive})
		}
		err = m.opts.Net.Apply(ctx, cfg, restart)
		if err == nil {
			err = m.opts.Net.Firewall(ctx, helper.MeshFirewallArgs{Interface: Interface, APIPort: m.opts.APIPort, RelayPort: m.opts.RelayPort, ListenPort: st.Port})
		}
		if err == nil {
			err = m.openDoor(st.IP)
		}
		if err == nil {
			err = m.openRelay(st.IP)
		}
		m.mu.Lock()
		if err == nil {
			m.applied.ip, m.applied.port = st.IP, st.Port
		}
	}
	if err != nil {
		m.lastErr = err.Error()
	} else {
		m.lastErr = ""
		m.dirty = false
	}
	m.mu.Unlock()
	if err != nil {
		m.emit("mesh.error", "error", "", "the mesh interface could not be set: "+err.Error())
	}
	return err
}

func safeID(id string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(id) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	s := b.String()
	if len(s) > 24 {
		s = s[:24]
	}
	if s == "" {
		s = "x"
	}
	return s
}

// Run keeps the mesh: the interface up, the rosters read from the others
// every round. It returns when ctx ends.
func (m *Mesh) Run(ctx context.Context) {
	if m.InNetwork() {
		_ = m.Apply(ctx)
	}
	m.settingsChanged()
	t := time.NewTicker(GossipEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			m.closeDoor()
			m.closeRelay()
			return
		case <-t.C:
		}
		m.round(ctx)
	}
}

// round is one pass: read every live member's roster, merge, re-apply when
// something changed or the interface went away.
func (m *Mesh) round(ctx context.Context) {
	if !m.InNetwork() {
		return
	}
	type got struct {
		id     string
		roster rosterAnswer
		names  []string
	}
	members := m.Members()
	results := make(chan got, len(members))
	var wg sync.WaitGroup
	for _, x := range members {
		wg.Add(1)
		go func(x Member) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			var r rosterAnswer
			if m.Call(cctx, x, "GET", "/mesh/roster", nil, &r) == nil {
				var names []string
				_ = m.Call(cctx, x, "GET", "/mesh/names", nil, &names)
				results <- got{x.ID, r, names}
			}
		}(x)
	}
	wg.Wait()
	close(results)
	expelled, settings := false, false
	m.mu.Lock()
	for g := range results {
		m.seen[g.id] = m.now()
		m.names[g.id] = g.names
		_, ex := m.mergeLocked(g.id, g.roster.Members)
		expelled = expelled || ex
		// Dated as the roster's entries are (mergeLocked), and only a
		// change of what they say is a change: a node that re-dates the
		// same settings ahead every round would otherwise have every node
		// save and announce them every round.
		s := g.roster.Settings
		if limit := m.now().Add(MaxSkew); s.Updated.After(limit) {
			s.Updated = limit
		}
		if s.Updated.After(m.st.Settings.Updated) {
			if s.PanelVPNOnly != m.st.Settings.PanelVPNOnly {
				settings, m.dirty = true, true
			}
			m.st.Settings = s
		}
	}
	if len(m.st.Invites) != len(m.pruneInvitesLocked()) {
		m.st.Invites = m.pruneInvitesLocked()
		m.dirty = true
	}
	dirty := m.dirty
	if dirty {
		_ = m.saveLocked()
	}
	m.mu.Unlock()
	if expelled {
		m.wipe("mesh.expelled", "another node removed this one from the network")
		return
	}
	if settings {
		m.settingsChanged()
	}
	if !dirty && m.opts.Net != nil {
		if st, err := m.opts.Net.Status(ctx); err == nil && !st.Up {
			dirty = true
		}
	}
	if dirty {
		_ = m.Apply(ctx)
	}
}

// Gossip runs one round now: for tests and for the panel's "refresh".
func (m *Mesh) Gossip(ctx context.Context) { m.round(ctx) }

func cleanName(name string) string {
	name = strings.Join(strings.Fields(name), " ")
	if len([]rune(name)) > 40 {
		return ""
	}
	return name
}

func random(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	return hex.EncodeToString(b)
}

func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
