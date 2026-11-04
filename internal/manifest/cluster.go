package manifest

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// Cluster puts copies of a site on other nodes of the network, which the
// node that holds it (the primary) hands requests to when it is saturated
// or its own instance is down. The names keep pointing at the primary:
// a replica is reached only through the mesh, never through its own
// doors (docs/cluster.md).
//
// A replica runs the site's code, not its data. Two copies of a database
// that both write are two databases from the first write on, so a site
// whose state lives in its folder or a volume cannot be one (Blockers).
type Cluster struct {
	// Replicas is how many other nodes run a copy. Zero is one, or as
	// many as Nodes names.
	Replicas int `json:"replicas,omitempty"`
	// Nodes are the ones to use first, by name or id; the rest are
	// chosen by the primary among the live nodes, the least loaded first.
	Nodes []string `json:"nodes,omitempty"`
	// Mode is "overflow" (the default: the primary serves everything
	// until it is saturated) or "spread" (always shared by free
	// capacity).
	Mode string `json:"mode,omitempty"`
	// Spill is the primary's load per core, in percent, from which it
	// shares. Zero is DefaultSpill.
	Spill int `json:"spill,omitempty"`
	// Gateway are the addresses on this node the site's databases listen
	// at ("127.0.0.1:3306", or "3306"): on each replica the house listens
	// at the same address and passes every connection through the mesh
	// to this one, so every node reads and writes the same database.
	// Loopback only: the gateway opens to the other nodes nothing but
	// what the site itself already reaches here.
	Gateway []string `json:"gateway,omitempty"`
}

// Cluster modes.
const (
	ClusterOverflow = "overflow"
	ClusterSpread   = "spread"
)

// DefaultSpill is the load per core, in percent, at which a primary
// starts sharing: under it a node answers comfortably and a hop through
// the mesh would only add latency.
const DefaultSpill = 70

// Replica marks a site as another node's copy, written by the house when
// it places one. A replica is not published, has no certificate and
// answers through no door of its own; its folder is replaced whole at
// every new revision of the primary.
type Replica struct {
	// Of is the primary node's id; Primary its name when the copy was
	// made, for people.
	Of      string `json:"of"`
	Primary string `json:"primary,omitempty"`
	// Revision is the primary's revision this copy is of: the primary
	// sends a request only to a copy at its own revision.
	Revision string `json:"revision"`
	// Gateway are the primary's addresses this replica reaches through
	// the mesh (Cluster.Gateway), listened at here.
	Gateway []string `json:"gateway,omitempty"`
}

// GatewayAddr is a gateway entry as an address: "3306" and
// "localhost:3306" are 127.0.0.1:3306. Anything but loopback is refused.
func GatewayAddr(s string) (string, error) {
	s = strings.TrimSpace(s)
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		host, port = "127.0.0.1", s
	}
	if host == "localhost" || host == "" {
		host = "127.0.0.1"
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.IsLoopback() {
		return "", fmt.Errorf("%q is not an address on this machine's loopback (127.0.0.1:port)", s)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("%q has no valid port", s)
	}
	return net.JoinHostPort(ip.String(), strconv.Itoa(n)), nil
}

// Want is how many replicas the block asks for.
func (c *Cluster) Want() int {
	if c == nil {
		return 0
	}
	if c.Replicas > 0 {
		return c.Replicas
	}
	if len(c.Nodes) > 0 {
		return len(c.Nodes)
	}
	return 1
}

// SpillAt is the load per core (1.0 = every core busy) at which the
// primary shares.
func (c *Cluster) SpillAt() float64 {
	if c == nil || c.Spill <= 0 {
		return DefaultSpill / 100.0
	}
	return float64(c.Spill) / 100.0
}

// Spreads says the requests are always shared, not only on overflow.
func (c *Cluster) Spreads() bool { return c != nil && c.Mode == ClusterSpread }

// IsReplica says the site is another node's copy.
func (m *Manifest) IsReplica() bool { return m != nil && m.Replica != nil }

// Clustered says the site is a primary with replicas asked for.
func (m *Manifest) Clustered() bool { return m != nil && m.Cluster != nil && m.Replica == nil }

// Blockers are why this site cannot have replicas, each a sentence; none
// is a site whose copies would all be the same site. Each one is state a
// copy would fork, or something only the primary's machine has.
func (m *Manifest) Blockers() []string {
	var out []string
	for _, name := range m.BackupNames() {
		if b := m.Backups[name]; b != nil && b.Engine == "sqlite" {
			out = append(out, "its database is SQLite ("+name+"), a file in its folder: each copy would write its own, and a file cannot be shared through the network")
			break
		}
	}
	if len(m.Workers) > 0 {
		out = append(out, "it has workers: a database or a queue that runs beside it would run once per copy")
	}
	if len(m.Services) > 0 {
		out = append(out, "it has services: several processes, some of them usually a database")
	}
	if m.Run != nil && m.Run.Docker != nil && len(m.Run.Docker.Volumes) > 0 {
		out = append(out, "its container has volumes: what it keeps there would differ on each node")
	}
	if m.Upstream != "" {
		out = append(out, "it forwards to an upstream: there is nothing of its own to copy")
	}
	if m.VPN != nil {
		out = append(out, "it lives in the VPN, which reaches the node that holds it")
	}
	if len(m.Needs) > 0 {
		out = append(out, "it needs other sites of this node, which the other nodes do not have")
	}
	if m.ClaudeAPI != nil || m.Claude {
		out = append(out, "it talks to an AI through this house, whose token and spending are this node's")
	}
	return out
}

func (m *Manifest) validCluster() error {
	if m.Cluster != nil && m.Replica != nil {
		return errors.New("cluster: a replica cannot have replicas of its own")
	}
	if r := m.Replica; r != nil {
		if r.Of == "" || r.Revision == "" {
			return errors.New("replica: of and revision are written by the house")
		}
	}
	c := m.Cluster
	if c == nil {
		return nil
	}
	if c.Replicas < 0 || c.Replicas > 16 {
		return fmt.Errorf("cluster.replicas: %d is not 1 to 16", c.Replicas)
	}
	seen := map[string]bool{}
	for _, n := range c.Nodes {
		k := strings.ToLower(strings.TrimSpace(n))
		if k == "" || len(k) > 64 {
			return fmt.Errorf("cluster.nodes: %q is not a node's name", n)
		}
		if seen[k] {
			return fmt.Errorf("cluster.nodes: %q twice", n)
		}
		seen[k] = true
	}
	if c.Replicas > 0 && len(c.Nodes) > c.Replicas {
		return fmt.Errorf("cluster: %d nodes named for %d replicas", len(c.Nodes), c.Replicas)
	}
	switch c.Mode {
	case "", ClusterOverflow, ClusterSpread:
	default:
		return fmt.Errorf("cluster.mode: %q is not overflow or spread", c.Mode)
	}
	seenAddr := map[string]bool{}
	for _, g := range c.Gateway {
		a, err := GatewayAddr(g)
		if err != nil {
			return fmt.Errorf("cluster.gateway: %v", err)
		}
		if seenAddr[a] {
			return fmt.Errorf("cluster.gateway: %s twice", a)
		}
		seenAddr[a] = true
	}
	if c.Spill < 0 || c.Spill > 400 {
		return fmt.Errorf("cluster.spill: %d is not a load per core in percent (1 to 400)", c.Spill)
	}
	if b := m.Blockers(); len(b) > 0 {
		return fmt.Errorf("cluster: this site cannot have replicas: %s", strings.Join(b, "; "))
	}
	return nil
}
