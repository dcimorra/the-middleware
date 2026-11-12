package helper

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The helper opens the sites' ports in a chain of its own at the top of
// INPUT, whole each time: the lines it runs are read here, since nobody
// is root where the tests run.
func TestPortsOpenRefillsItsOwnChain(t *testing.T) {
	h := newTestHelper(t)
	h.rec.Fail["iptables -w -C INPUT -j "+PortsChain] = errors.New("iptables: Bad rule (does a matching rule exist in that chain?).")
	res, err := h.cli.PortsOpen(context.Background(), []PortRule{{From: 3478, To: 3478, Proto: "udp"}, {From: 49160, To: 49400, Proto: "udp"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Opened != 2 {
		t.Fatalf("opened %d", res.Opened)
	}
	got := strings.Join(h.rec.Lines(), "\n")
	for _, want := range []string{
		"iptables -w -N " + PortsChain,
		"iptables -w -F " + PortsChain,
		"iptables -w -A " + PortsChain + " -p udp --dport 3478 -j ACCEPT",
		"iptables -w -A " + PortsChain + " -p udp --dport 49160:49400 -j ACCEPT",
		"iptables -w -I INPUT 1 -j " + PortsChain,
		"ip6tables -w -A " + PortsChain + " -p udp --dport 49160:49400 -j ACCEPT",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// The jump is there already for ip6tables (its -C passed): not twice.
	if strings.Contains(got, "ip6tables -w -I INPUT") {
		t.Errorf("the jump was inserted where it already was:\n%s", got)
	}
	if strings.Index(got, "-F "+PortsChain) > strings.Index(got, "-A "+PortsChain) {
		t.Error("the chain is filled before it is emptied: a port whose site left would stay open")
	}
}

func TestPortsOpenWithNoneTakesTheChainAway(t *testing.T) {
	h := newTestHelper(t)
	if _, err := h.cli.PortsOpen(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(h.rec.Lines(), "\n")
	for _, want := range []string{"iptables -w -D INPUT -j " + PortsChain, "iptables -w -X " + PortsChain} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "-A ") {
		t.Errorf("something was opened:\n%s", got)
	}
}

func TestPortsOpenTakesOnlyPorts(t *testing.T) {
	h := newTestHelper(t)
	for name, rules := range map[string][]PortRule{
		"a protocol that is not one": {{From: 1, To: 1, Proto: "udp -j ACCEPT; rm"}},
		"a port out of range":        {{From: 0, To: 10, Proto: "udp"}},
		"a range backwards":          {{From: 10, To: 5, Proto: "tcp"}},
		"too many":                   make([]PortRule, MaxPortRules+1),
	} {
		if _, err := h.cli.PortsOpen(context.Background(), rules); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if n := len(h.rec.Lines()); n != 0 {
		t.Fatalf("%d commands ran for arguments that were refused", n)
	}
}

// A machine without ip6tables, or with IPv6 off, still gets its IPv4
// ports; one without iptables at all has no such firewall to open.
func TestPortsOpenWithoutTheTools(t *testing.T) {
	h := newTestHelper(t)
	h.rec.Fail["ip6tables -w -N "+PortsChain] = errors.New("ip6tables is not installed")
	res, err := h.cli.PortsOpen(context.Background(), []PortRule{{From: 3478, To: 3478, Proto: "udp"}})
	if err != nil || len(res.Skipped) != 1 || res.Skipped[0] != "ip6tables" {
		t.Fatalf("%+v, %v", res, err)
	}
	h.rec.Fail["iptables -w -N "+PortsChain] = errors.New("iptables is not installed")
	res, err = h.cli.PortsOpen(context.Background(), []PortRule{{From: 3478, To: 3478, Proto: "udp"}})
	if err != nil || len(res.Skipped) != 2 {
		t.Fatalf("%+v, %v", res, err)
	}
	// A failure that is not a missing tool is said.
	delete(h.rec.Fail, "iptables -w -N "+PortsChain)
	h.rec.Fail["iptables -w -F "+PortsChain] = errors.New("iptables: Permission denied")
	if _, err := h.cli.PortsOpen(context.Background(), []PortRule{{From: 3478, To: 3478, Proto: "udp"}}); err == nil {
		t.Fatal("a failed flush passed for ports opened")
	}
}
