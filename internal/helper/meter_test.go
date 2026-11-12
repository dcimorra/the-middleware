package helper

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const meterS = "iptables -w -t mangle -S "

// The first call finds no chains: it makes both, the local networks
// first, hangs them from the mangle table and says it starts from zero.
func TestPortsMeterMakesItsChains(t *testing.T) {
	h := newTestHelper(t)
	for _, bin := range []string{"iptables", "ip6tables"} {
		h.rec.Fail[bin+" -w -t mangle -S "+MeterChainIn+" -v"] = errors.New("No chain/target/match by that name.")
		h.rec.Fail[bin+" -w -t mangle -S "+MeterChainOut+" -v"] = errors.New("No chain/target/match by that name.")
		h.rec.Fail[bin+" -w -t mangle -C PREROUTING -j "+MeterChainIn] = errors.New("Bad rule")
	}
	res, err := h.cli.PortsMeter(context.Background(), []MeterRule{{From: 3478, To: 3478, Proto: "udp"}, {From: 49160, To: 49400, Proto: "udp", Cut: true}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Reset || len(res.Counts) != 2 || res.Counts[0].In != 0 {
		t.Fatalf("%+v", res)
	}
	got := strings.Join(h.rec.Lines(), "\n")
	for _, want := range []string{
		"iptables -w -t mangle -N " + MeterChainIn,
		"iptables -w -t mangle -A " + MeterChainIn + " -s 127.0.0.0/8 -j RETURN",
		"iptables -w -t mangle -A " + MeterChainIn + " -p udp --dport 3478 -j RETURN",
		"iptables -w -t mangle -A " + MeterChainIn + " -p udp --dport 49160:49400 -j DROP",
		"iptables -w -t mangle -I PREROUTING 1 -j " + MeterChainIn,
		"iptables -w -t mangle -A " + MeterChainOut + " -d 10.0.0.0/8 -j RETURN",
		"iptables -w -t mangle -A " + MeterChainOut + " -p udp --sport 3478 -j RETURN",
		"ip6tables -w -t mangle -A " + MeterChainIn + " -s fc00::/7 -j RETURN",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// The jump that was there (POSTROUTING's -C passed) is not put twice.
	if strings.Contains(got, "-I POSTROUTING") {
		t.Errorf("the jump was inserted where it already was:\n%s", got)
	}
	// A cut port's local traffic returns before the drop, or the
	// service's own health check would fail and restart it in a loop.
	if strings.Index(got, "-s 127.0.0.0/8 -j RETURN") > strings.Index(got, "-j DROP") {
		t.Error("the drop comes before loopback returns")
	}
}

// Chains that hold what is asked are read and left alone, so their
// counters go on; a port whose site was cut since rewrites them, after
// the reading.
func TestPortsMeterReadsWithoutTouchingWhatIsRight(t *testing.T) {
	h := newTestHelper(t)
	in := "-N " + MeterChainIn + "\n"
	out := "-N " + MeterChainOut + "\n"
	for _, n := range meterLocal["iptables"] {
		in += "-A " + MeterChainIn + " -s " + n + " -c 9 900 -j RETURN\n"
		out += "-A " + MeterChainOut + " -d " + n + " -c 9 900 -j RETURN\n"
	}
	in += "-A " + MeterChainIn + " -p udp -m udp --dport 3478 -c 12 840 -j RETURN\n"
	out += "-A " + MeterChainOut + " -p udp -m udp --sport 3478 -c 30 5000 -j RETURN\n"
	h.rec.Outputs = map[string]string{meterS + MeterChainIn + " -v": in, meterS + MeterChainOut + " -v": out}
	h.rec.Fail["ip6tables -w -t mangle -S "+MeterChainIn+" -v"] = errors.New("ip6tables is not installed")

	res, err := h.cli.PortsMeter(context.Background(), []MeterRule{{From: 3478, To: 3478, Proto: "udp"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Reset || res.Counts[0].In != 840 || res.Counts[0].Out != 5000 || len(res.Skipped) != 1 {
		t.Fatalf("%+v", res)
	}
	for _, line := range h.rec.Lines() {
		if strings.Contains(line, " -F ") || strings.Contains(line, " -A ") || strings.Contains(line, " -I ") {
			t.Fatalf("a chain that was right was written: %s", line)
		}
	}

	h.rec.Reset()
	res, err = h.cli.PortsMeter(context.Background(), []MeterRule{{From: 3478, To: 3478, Proto: "udp", Cut: true}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Reset || res.Counts[0].In != 840 || res.Counts[0].Out != 5000 {
		t.Fatalf("cut: %+v", res)
	}
	got := strings.Join(h.rec.Lines(), "\n")
	if !strings.Contains(got, "-A "+MeterChainIn+" -p udp --dport 3478 -j DROP") || !strings.Contains(got, "-A "+MeterChainOut+" -p udp --sport 3478 -j DROP") {
		t.Fatalf("the cut port is not dropped:\n%s", got)
	}
}

func TestPortsMeterWithNoneTakesTheChainsAway(t *testing.T) {
	h := newTestHelper(t)
	if _, err := h.cli.PortsMeter(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(h.rec.Lines(), "\n")
	for _, want := range []string{"iptables -w -t mangle -D PREROUTING -j " + MeterChainIn, "iptables -w -t mangle -X " + MeterChainOut} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, " -A ") || strings.Contains(got, " -I ") {
		t.Errorf("something was written:\n%s", got)
	}
}

func TestPortsMeterTakesOnlyPorts(t *testing.T) {
	h := newTestHelper(t)
	for name, rules := range map[string][]MeterRule{
		"a protocol that is not one": {{From: 1, To: 1, Proto: "udp -j ACCEPT; rm"}},
		"a port out of range":        {{From: 0, To: 10, Proto: "udp"}},
		"too many":                   make([]MeterRule, MaxPortRules+1),
	} {
		if _, err := h.cli.PortsMeter(context.Background(), rules); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if n := len(h.rec.Lines()); n != 0 {
		t.Fatalf("%d commands ran for arguments that were refused", n)
	}
}
