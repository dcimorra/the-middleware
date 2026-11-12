package helper

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
)

// The chains the helper counts the sites' public ports in, and closes
// them in when a site has spent its transfer budget.
//
// They hang from the mangle table's PREROUTING and POSTROUTING and not
// from INPUT, where PortsChain opens the ports: a port a container
// publishes is forwarded and never crosses INPUT, while every packet,
// whichever way it is going, crosses these two. In the first a packet is
// told by the port it is for, in the second by the port it comes from.
//
// A rule counts whether or not anything reads it, so the chains hold a
// rule per port and the supervisor asks for the numbers. Traffic with
// the machine itself and with private networks returns before the ports:
// the house's proxy talks to the sites on loopback and to containers on
// their bridges from ports of the same range a relay is given, the nodes
// talk over the VPN, and none of that is the transfer a provider bills.
// It is also what keeps a service's own health check answering while the
// service is cut.
const (
	MeterChainIn  = "MIDDLEWARE-METER-IN"
	MeterChainOut = "MIDDLEWARE-METER-OUT"
)

var meterLocal = map[string][]string{
	"iptables":  {"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "100.64.0.0/10"},
	"ip6tables": {"::1/128", "fc00::/7", "fe80::/10"},
}

// MeterRule is one public port or range, and whether its site is cut:
// a cut port drops what is not local instead of counting it through.
type MeterRule struct {
	From  int    `json:"from"`
	To    int    `json:"to"`
	Proto string `json:"proto"`
	Cut   bool   `json:"cut,omitempty"`
}

// MeterArgs is the input of ports.meter: every public port of every
// site. None takes the chains away.
type MeterArgs struct {
	Ports []MeterRule `json:"ports"`
}

// MeterCount is the bytes a port's rules counted, both families added.
type MeterCount struct {
	From  int    `json:"from"`
	To    int    `json:"to"`
	Proto string `json:"proto"`
	In    int64  `json:"in"`
	Out   int64  `json:"out"`
}

// MeterResult is what the rules had counted when they were read. Reset
// says the chains were written again after the reading, so the next one
// starts from zero; Skipped names a family whose tool is not there.
type MeterResult struct {
	Counts  []MeterCount `json:"counts"`
	Reset   bool         `json:"reset,omitempty"`
	Skipped []string     `json:"skipped,omitempty"`
}

func (r MeterRule) ports() string { return PortRule{From: r.From, To: r.To}.dport() }

// meterLine is a rule as it is appended and as it is told apart when
// read back.
type meterLine struct {
	args []string
	key  string
}

// meterWant is what a chain of bin should hold for rules, in order.
func meterWant(bin string, in bool, rules []MeterRule) []meterLine {
	addr, port := "-d", "--sport"
	if in {
		addr, port = "-s", "--dport"
	}
	var out []meterLine
	for _, n := range meterLocal[bin] {
		out = append(out, meterLine{args: []string{addr, n, "-j", "RETURN"}, key: "net " + n})
	}
	for _, r := range rules {
		target := "RETURN"
		if r.Cut {
			target = "DROP"
		}
		out = append(out, meterLine{
			args: []string{"-p", r.Proto, port, r.ports(), "-j", target},
			key:  "port " + r.Proto + " " + r.ports() + " " + target,
		})
	}
	return out
}

// meterRead parses `iptables -S <chain> -v`: the key of each rule, as
// meterWant writes it, and the bytes it counted.
func meterRead(chain, listing string) (keys []string, bytes map[string]int64) {
	bytes = map[string]int64{}
	for _, line := range strings.Split(listing, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != "-A" || f[1] != chain {
			continue
		}
		var net, proto, ports, target string
		var n int64
		for i := 2; i < len(f)-1; i++ {
			switch f[i] {
			case "-s", "-d":
				net = f[i+1]
			case "-p":
				proto = f[i+1]
			case "--dport", "--sport":
				ports = f[i+1]
			case "-j":
				target = f[i+1]
			case "-c":
				if i+2 < len(f) {
					n, _ = strconv.ParseInt(f[i+2], 10, 64)
				}
			}
		}
		if ports == "" {
			keys = append(keys, "net "+net)
			continue
		}
		keys = append(keys, "port "+proto+" "+ports+" "+target)
		bytes[proto+" "+ports] += n
	}
	return keys, bytes
}

func (s *Server) portsMeter(ctx context.Context, raw json.RawMessage) (any, error) {
	var a MeterArgs
	if err := decode(raw, &a); err != nil {
		return nil, err
	}
	open := make([]PortRule, len(a.Ports))
	for i, r := range a.Ports {
		open[i] = PortRule{From: r.From, To: r.To, Proto: r.Proto}
	}
	if err := validPortRules(open); err != nil {
		return nil, err
	}
	res := MeterResult{Counts: make([]MeterCount, len(a.Ports))}
	for i, r := range a.Ports {
		res.Counts[i] = MeterCount{From: r.From, To: r.To, Proto: r.Proto}
	}
	for _, bin := range []string{"iptables", "ip6tables"} {
		for _, c := range []struct {
			chain, hook string
			in          bool
		}{{MeterChainIn, "PREROUTING", true}, {MeterChainOut, "POSTROUTING", false}} {
			counted, reset, err := s.meterChain(ctx, bin, c.chain, c.hook, meterWant(bin, c.in, a.Ports), len(a.Ports) == 0)
			if err != nil {
				// As for ports.open: no tool is no firewall of that
				// kind, and IPv6 off in the kernel is not IPv4 uncounted.
				if strings.Contains(err.Error(), "is not installed") || bin == "ip6tables" {
					if !slices.Contains(res.Skipped, bin) {
						res.Skipped = append(res.Skipped, bin)
					}
					break
				}
				return nil, err
			}
			res.Reset = res.Reset || reset
			for i, r := range a.Ports {
				if c.in {
					res.Counts[i].In += counted[r.Proto+" "+r.ports()]
				} else {
					res.Counts[i].Out += counted[r.Proto+" "+r.ports()]
				}
			}
		}
	}
	return res, nil
}

// meterChain reads what chain counted and leaves it holding want. A
// chain that already holds it is not touched, so its counters go on; one
// that does not is emptied and filled, after the reading, so nothing
// counted is lost to the change.
func (s *Server) meterChain(ctx context.Context, bin, chain, hook string, want []meterLine, none bool) (map[string]int64, bool, error) {
	run := func(args ...string) (string, error) {
		return s.Exec.Run(ctx, bin, append([]string{"-w", "-t", "mangle"}, args...), "")
	}
	listing, err := run("-S", chain, "-v")
	if err != nil && strings.Contains(err.Error(), "is not installed") {
		return nil, false, err
	}
	have, counted := meterRead(chain, listing)
	if none {
		// Each may fail because it was never there, which is the state
		// asked for.
		_, _ = run("-D", hook, "-j", chain)
		_, _ = run("-F", chain)
		_, _ = run("-X", chain)
		return counted, false, nil
	}
	keys := make([]string, len(want))
	for i, l := range want {
		keys[i] = l.key
	}
	reset := false
	if err != nil || !slices.Equal(have, keys) {
		reset = true
		// "Chain already exists" is the usual answer; anything else
		// shows in the flush.
		_, _ = run("-N", chain)
		if _, err := run("-F", chain); err != nil {
			return nil, false, err
		}
		for _, l := range want {
			if _, err := run(append([]string{"-A", chain}, l.args...)...); err != nil {
				return nil, false, err
			}
		}
	}
	// Asked every time and not only when the chain is written: a
	// firewall reloaded by someone else takes the jump and leaves the
	// chain, which would then count nothing and say nothing.
	if _, err := run("-C", hook, "-j", chain); err != nil {
		if _, err := run("-I", hook, "1", "-j", chain); err != nil {
			return nil, false, err
		}
	}
	return counted, reset, nil
}
