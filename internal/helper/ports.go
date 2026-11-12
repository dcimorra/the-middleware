package helper

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// PortsChain is the chain the helper keeps the sites' public ports in,
// jumped to from the top of INPUT. A chain of its own, flushed and
// refilled whole: what is open is what the manifests say now, and a port
// whose site left is closed by the next call without anyone remembering
// which rule was its.
//
// iptables and not the middleware nft table: that table's chain accepts
// by policy, and an accept there does not undo a drop in another table.
// What closes a port on the machines this runs on is the INPUT chain the
// distribution or its firewall filled (Oracle's Ubuntu images reject
// everything but SSH there; ufw puts its own chains in it), so the accept
// has to come first in that same chain. Rules made with iptables do not
// survive a reboot, and need not: the supervisor asks again at every
// start, from the manifests.
const PortsChain = "MIDDLEWARE-PORTS"

// MaxPortRules bounds a ports.open call.
const MaxPortRules = 256

// PortRule is one port or range of one protocol.
type PortRule struct {
	From  int    `json:"from"`
	To    int    `json:"to"`
	Proto string `json:"proto"`
}

// PortsArgs is the input of ports.open: every public port of every site,
// which replaces what was open before. None closes them all and takes the
// chain away.
type PortsArgs struct {
	Ports []PortRule `json:"ports"`
}

// PortsResult says what ports.open did: Skipped names a family whose
// tool is not on the machine (no iptables means no firewall of that kind
// to open).
type PortsResult struct {
	Opened  int      `json:"opened"`
	Skipped []string `json:"skipped,omitempty"`
}

func (r PortRule) dport() string {
	if r.From == r.To {
		return strconv.Itoa(r.From)
	}
	return strconv.Itoa(r.From) + ":" + strconv.Itoa(r.To)
}

func validPortRules(rules []PortRule) error {
	if len(rules) > MaxPortRules {
		return fmt.Errorf("%w: %d port rules, %d at most", errBadArgument, len(rules), MaxPortRules)
	}
	for _, r := range rules {
		if r.Proto != "tcp" && r.Proto != "udp" {
			return fmt.Errorf("%w: protocol %q is not tcp or udp", errBadArgument, r.Proto)
		}
		if r.From < 1 || r.To > 65535 || r.From > r.To {
			return fmt.Errorf("%w: ports %d-%d out of range", errBadArgument, r.From, r.To)
		}
	}
	return nil
}

func (s *Server) portsOpen(ctx context.Context, raw json.RawMessage) (any, error) {
	var a PortsArgs
	if err := decode(raw, &a); err != nil {
		return nil, err
	}
	if err := validPortRules(a.Ports); err != nil {
		return nil, err
	}
	res := PortsResult{Opened: len(a.Ports)}
	for _, bin := range []string{"iptables", "ip6tables"} {
		if err := s.portsApply(ctx, bin, a.Ports); err != nil {
			if strings.Contains(err.Error(), "is not installed") {
				res.Skipped = append(res.Skipped, bin)
				continue
			}
			// IPv6 off in the kernel makes ip6tables fail on a machine
			// whose IPv4 rules went in: that is not a port left closed.
			if bin == "ip6tables" {
				res.Skipped = append(res.Skipped, bin)
				continue
			}
			return nil, err
		}
	}
	return res, nil
}

func (s *Server) portsApply(ctx context.Context, bin string, rules []PortRule) error {
	run := func(args ...string) error {
		_, err := s.Exec.Run(ctx, bin, append([]string{"-w"}, args...), "")
		return err
	}
	if len(rules) == 0 {
		// Each may fail because it was never there, which is the state
		// asked for; a tool that is missing says so on the first.
		if err := run("-D", "INPUT", "-j", PortsChain); err != nil && strings.Contains(err.Error(), "is not installed") {
			return err
		}
		_ = run("-F", PortsChain)
		_ = run("-X", PortsChain)
		return nil
	}
	if err := run("-N", PortsChain); err != nil {
		if strings.Contains(err.Error(), "is not installed") {
			return err
		}
		// "Chain already exists" is the usual answer; anything else shows
		// in the flush.
	}
	if err := run("-F", PortsChain); err != nil {
		return err
	}
	for _, r := range rules {
		if err := run("-A", PortsChain, "-p", r.Proto, "--dport", r.dport(), "-j", "ACCEPT"); err != nil {
			return err
		}
	}
	if run("-C", "INPUT", "-j", PortsChain) != nil {
		return run("-I", "INPUT", "1", "-j", PortsChain)
	}
	return nil
}
