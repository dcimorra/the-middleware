package helper

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync/atomic"
	"time"
)

// Client talks to the helper over its socket. Each call opens its own
// connection: the helper is called a few times an hour, and a persistent
// connection would need reconnect logic for a restart of the service that
// a fresh dial gets for free.
type Client struct {
	path    string
	timeout time.Duration
	seq     atomic.Int64
}

// Dial returns a client for the socket at path (empty means DefaultSocket).
// It does not connect: Available reports whether anybody answers.
func Dial(path string) *Client {
	if path == "" {
		path = DefaultSocket
	}
	return &Client{path: path, timeout: 5 * time.Second}
}

// Path is the socket the client dials.
func (c *Client) Path() string { return c.path }

// Available reports whether the helper answers a ping. A missing socket
// file is the common case on an installation without the helper service
// and is answered without dialing.
func (c *Client) Available() bool {
	if _, err := os.Stat(c.path); err != nil {
		return false
	}
	_, err := c.Ping(context.Background())
	return err == nil
}

// Call runs one operation. Unknown-op and validation errors come back as
// errors with the helper's message; a socket that cannot be opened is
// ErrUnavailable so callers can treat it as "no helper" rather than "the
// firewall is broken".
func (c *Client) Call(ctx context.Context, op string, args any, result any) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.path)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	req := Request{ID: strconv.FormatInt(c.seq.Add(1), 10), Op: op}
	if args != nil {
		req.Args, err = json.Marshal(args)
		if err != nil {
			return err
		}
	}
	// A failed write is not the end: a server that refused the peer may
	// have queued its answer and closed, and that answer says why.
	sendErr := json.NewEncoder(conn).Encode(req)
	line, err := bufio.NewReaderSize(conn, 1<<20).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		if sendErr != nil {
			return fmt.Errorf("helper: send %s: %w", op, sendErr)
		}
		return fmt.Errorf("helper: %s: no answer: %w", op, err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return fmt.Errorf("helper: %s: malformed answer: %w", op, err)
	}
	if !resp.OK {
		if resp.Error == "" {
			resp.Error = "unknown error"
		}
		return fmt.Errorf("helper: %s: %s", op, resp.Error)
	}
	if resp.ID != req.ID {
		return errors.New("helper: answer to a different request")
	}
	if result != nil && len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, result); err != nil {
			return fmt.Errorf("helper: %s: decode result: %w", op, err)
		}
	}
	return nil
}

// Ping returns the helper's version.
func (c *Client) Ping(ctx context.Context) (PingResult, error) {
	var r PingResult
	err := c.Call(ctx, OpPing, nil, &r)
	return r, err
}

// FirewallApply loads the policy as the middleware nftables table.
func (c *Client) FirewallApply(ctx context.Context, p FirewallPolicy) error {
	return c.Call(ctx, OpFirewallApply, p, nil)
}

// FirewallBlock adds ip to the block set for d.
func (c *Client) FirewallBlock(ctx context.Context, ip string, d time.Duration, reason string) error {
	secs := int(d / time.Second)
	if secs < 1 {
		secs = 1
	}
	return c.Call(ctx, OpFirewallBlock, BlockArgs{IP: ip, Seconds: secs, Reason: reason}, nil)
}

// FirewallUnblock removes ip from the block set.
func (c *Client) FirewallUnblock(ctx context.Context, ip string) error {
	return c.Call(ctx, OpFirewallUnblock, UnblockArgs{IP: ip}, nil)
}

// FirewallStatus lists the block set.
func (c *Client) FirewallStatus(ctx context.Context) (FirewallStatus, error) {
	var r FirewallStatus
	err := c.Call(ctx, OpFirewallStatus, nil, &r)
	return r, err
}

// VPNApply writes the interface config and syncs it if the interface is up.
func (c *Client) VPNApply(ctx context.Context, a VPNApplyArgs) error {
	return c.Call(ctx, OpVPNApply, a, nil)
}

// VPNUp brings the interface up.
func (c *Client) VPNUp(ctx context.Context, iface string) error {
	return c.Call(ctx, OpVPNUp, InterfaceArgs{Interface: iface}, nil)
}

// VPNDown brings the interface down.
func (c *Client) VPNDown(ctx context.Context, iface string) error {
	return c.Call(ctx, OpVPNDown, InterfaceArgs{Interface: iface}, nil)
}

// VPNStatus reads the interface and its peers.
func (c *Client) VPNStatus(ctx context.Context, iface string) (VPNStatus, error) {
	var r VPNStatus
	err := c.Call(ctx, OpVPNStatus, InterfaceArgs{Interface: iface}, &r)
	return r, err
}

// ReservePorts keeps from-to out of the kernel's ephemeral range.
func (c *Client) ReservePorts(ctx context.Context, from, to int) error {
	return c.Call(ctx, OpSysctlReserve, ReservePortsArgs{From: from, To: to}, nil)
}

// MeshFirewall loads (or with Down removes) the rules of the nodes' mesh.
func (c *Client) MeshFirewall(ctx context.Context, a MeshFirewallArgs) error {
	return c.Call(ctx, OpMeshFirewall, a, nil)
}

// PortsOpen opens the sites' public ports in the machine's INPUT chain,
// replacing what it opened before (PortsChain).
func (c *Client) PortsOpen(ctx context.Context, ports []PortRule) (PortsResult, error) {
	var res PortsResult
	err := c.Call(ctx, OpPortsOpen, PortsArgs{Ports: ports}, &res)
	return res, err
}
