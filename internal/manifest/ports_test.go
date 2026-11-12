package manifest

import (
	"strings"
	"testing"

	"github.com/dcimorra/the-middleware/internal/ports"
)

// turn is a site with an app behind the house and a relay beside it, the
// case public ports exist for.
const turn = `{"version":2,"services":{
	"web":{"run":{"command":["node","server.js"]},"domains":["p2p.test"]},
	"turn":{"run":{"runtime":"docker","docker":{"image":"coturn/coturn:4.6.3"},
	        "command":["-n","--external-ip=$MIDDLEWARE_PUBLIC_IP","--min-port=49160","--max-port=49400"],
	        "health":{"command":["turnutils_stunclient","127.0.0.1"]}},
	        "network":"host","ports":["3478/udp","3478/tcp","49160-49400/udp"]}}}`

func TestAPublicPortIsAPortOrARangeAndItsProtocol(t *testing.T) {
	for in, want := range map[string]PublicPort{
		"3478":            {3478, 3478, "tcp"},
		"3478/udp":        {3478, 3478, "udp"},
		"49160-49400/udp": {49160, 49400, "udp"},
	} {
		got, err := ParsePublicPort(in)
		if err != nil || got != want {
			t.Errorf("%s: %+v, %v", in, got, err)
		}
	}
	if got := (PublicPort{49160, 49400, "udp"}).String(); got != "49160-49400/udp" {
		t.Errorf("written back as %s", got)
	}
	for _, bad := range []string{"", "udp", "3478/sctp", "0/udp", "70000/udp", "500-400/udp", "3478/udp/x", "a-b", "3478 /udp"} {
		if _, err := ParsePublicPort(bad); err == nil {
			t.Errorf("%q was read as a port", bad)
		}
	}
}

func TestARelayBesideAnAppIsAManifest(t *testing.T) {
	m, err := Parse([]byte(turn))
	if err != nil {
		t.Fatal(err)
	}
	sv := m.Services["turn"]
	if !sv.Public() || !sv.HostNetwork() || sv.HandsOver() {
		t.Fatalf("public %v host %v hands over %v", sv.Public(), sv.HostNetwork(), sv.HandsOver())
	}
	if m.Published("turn") {
		t.Fatal("a container on the host's network has no port for the house to publish")
	}
	want := []string{"turn 3478/tcp", "turn 3478/udp", "turn 49160-49400/udp", "turn network=host"}
	if got := m.PublicGrant(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("what the owner is asked: %v", got)
	}
	if got := m.PublicPorts(); len(got) != 3 || got[0].Service != "turn" {
		t.Fatalf("ports: %+v", got)
	}
	m.Services["turn"].Disabled = true
	if got := m.PublicPorts(); len(got) != 0 {
		t.Fatalf("a stopped service holds %v", got)
	}
}

func TestWhatAPublicServiceMayNotAsk(t *testing.T) {
	svc := func(body string) string {
		return `{"version":2,"services":{"web":{"run":{"command":["x"]},"domains":["p2p.test"]},` + body + `}}`
	}
	docker := `"run":{"runtime":"docker","docker":{"image":"coturn/coturn:4.6.3"}}`
	for name, tc := range map[string]struct{ in, want string }{
		"the house's door":       {svc(`"t":{` + docker + `,"ports":["443/tcp"]}`), "HTTPS door"},
		"ssh inside a range":     {svc(`"t":{` + docker + `,"ports":["20-30/tcp"]}`), "SSH"},
		"the sites' range":       {svc(`"t":{` + docker + `,"ports":["40000/udp"]}`), "gives its sites"},
		"host with no ports":     {svc(`"t":{` + docker + `,"network":"host"}`), "needs ports"},
		"host for a process":     {svc(`"t":{"run":{"command":["x"]},"network":"host","ports":["5000/udp"]}`), "container's choice"},
		"another network":        {svc(`"t":{` + docker + `,"network":"bridge","ports":["5000/udp"]}`), "is not host"},
		"names on the host":      {svc(`"t":{` + docker + `,"network":"host","ports":["5000/udp"],"domains":["t.p2p.test"]}`), "answers no names"},
		"a handover":             {svc(`"t":{` + docker + `,"ports":["5000/udp"],"handover":true}`), "cannot hand over"},
		"low port on the host":   {svc(`"t":{` + docker + `,"network":"host","ports":["53/udp"]}`), "below 1024"},
		"low port for a process": {svc(`"t":{"run":{"command":["x"]},"ports":["53/udp"]}`), "below 1024"},
		"twice in one service":   {svc(`"t":{` + docker + `,"ports":["5000-5010/udp","5005/udp"]}`), "overlaps"},
		"two services, one port": {svc(`"t":{` + docker + `,"ports":["5000/udp"]},"u":{` + docker + `,"ports":["5000/udp"]}`), "a port has one service"},
		"a range through docker": {svc(`"t":{` + docker + `,"ports":["10000-12000/udp"]}`), "network: host"},
		"a name from the host":   {svc(`"t":{"run":{"runtime":"docker","docker":{"image":"a:1"},"env":{"W":"$SERVICE_u_HOST"}},"network":"host","ports":["5000/udp"]},"u":{` + docker + `}`), "host's network"},
		"garbage":                {svc(`"t":{` + docker + `,"ports":["turn"]}`), "is not a port"},
	} {
		_, err := Parse([]byte(tc.in))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want it to say %q", name, err, tc.want)
		}
	}
	// What is allowed: the same port on both protocols, a low port that
	// docker publishes, a process with a port of its own.
	for name, in := range map[string]string{
		"both protocols":  svc(`"t":{` + docker + `,"ports":["3478/udp","3478/tcp"]}`),
		"dns in a bridge": svc(`"t":{` + docker + `,"ports":["53/udp"]}`),
		"a process":       svc(`"t":{"run":{"command":["x"]},"ports":["5000/udp"]}`),
	} {
		if _, err := Parse([]byte(in)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The manifest's copy of the loopback range is the allocator's.
func TestTheSitesRangeIsTheAllocators(t *testing.T) {
	if SitePortsFrom != ports.DefaultFrom || SitePortsTo != ports.DefaultTo {
		t.Fatalf("manifest %d-%d, ports %d-%d", SitePortsFrom, SitePortsTo, ports.DefaultFrom, ports.DefaultTo)
	}
}

func TestAServiceReadsTheNodesAddress(t *testing.T) {
	m, err := Parse([]byte(turn))
	if err != nil {
		t.Fatal(err)
	}
	addr := func(string, bool) (string, int, bool) { return "", 0, true }
	dir := t.TempDir()
	// Read without it, the variable stays as written and the spec says it
	// is not one to start from.
	loose, err := ServiceSpec("p2p.test", dir, "turn", m, addr)
	if err != nil {
		t.Fatal(err)
	}
	if !loose.NeedsPublicIP || loose.Command[1] != "--external-ip=$MIDDLEWARE_PUBLIC_IP" {
		t.Fatalf("without an address: needs %v, %q", loose.NeedsPublicIP, loose.Command[1])
	}
	here, err := ServiceSpecOn("p2p.test", dir, "turn", m, addr, "203.0.113.7")
	if err != nil {
		t.Fatal(err)
	}
	if here.NeedsPublicIP || here.Command[1] != "--external-ip=203.0.113.7" || here.Env[PublicIPVar] != "203.0.113.7" {
		t.Fatalf("on a node: %q, env %q", here.Command[1], here.Env[PublicIPVar])
	}
	if !here.HostNetwork || !here.Unpublished || here.Network != "" || len(here.PublicPorts) != 3 {
		t.Fatalf("host %v unpublished %v network %q ports %v", here.HostNetwork, here.Unpublished, here.Network, here.PublicPorts)
	}
	// Moved to another node, it is another generation: the relay must
	// not go on telling its peers the old address.
	there, _ := ServiceSpecOn("p2p.test", dir, "turn", m, addr, "198.51.100.9")
	if there.Revision == here.Revision {
		t.Fatal("the revision does not change with the node's address")
	}
	// The app beside it reads no address and has no ports: its revision
	// is what it was before any of this, and it needs nothing.
	web, _ := ServiceSpecOn("p2p.test", dir, "web", m, addr, "203.0.113.7")
	plain, _ := ServiceSpec("p2p.test", dir, "web", m, addr)
	if web.Revision != plain.Revision || web.NeedsPublicIP || web.Env[PublicIPVar] != "" {
		t.Fatalf("a service without ports changed: %s / %s", web.Revision, plain.Revision)
	}
	// A port is part of what runs.
	m.Services["turn"].Ports = append(m.Services["turn"].Ports, "5349/tcp")
	more, _ := ServiceSpecOn("p2p.test", dir, "turn", m, addr, "203.0.113.7")
	if more.Revision == here.Revision {
		t.Fatal("a new port is not a new generation")
	}
}
