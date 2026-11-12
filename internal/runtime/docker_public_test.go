package runtime

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/manifest"
)

func publicSpec(t *testing.T, site, services string) (*manifest.Manifest, func(name string) *manifest.Spec) {
	t.Helper()
	m, err := manifest.Parse([]byte(`{"version":2,"services":{"web":{"run":{"command":["x"]},"domains":["p2p.test"]},` + services + `}}`))
	if err != nil {
		t.Fatal(err)
	}
	return m, func(name string) *manifest.Spec {
		spec, err := manifest.ServiceSpecOn("p2p.test", site, name, m, func(string, bool) (string, int, bool) { return "", 0, true }, "203.0.113.7")
		if err != nil {
			t.Fatal(err)
		}
		return spec
	}
}

// What a service with public ports is run with: the machine's network
// when it asked for it, and then nothing published and no network of the
// site's; otherwise each port published on every address, the same number
// inside and out. The walls are any container's, with the one
// capability publicArgs explains.
func TestAPublicServicesContainer(t *testing.T) {
	bin, calls := fakeServicesDocker(t, goruntime.GOARCH)
	site := t.TempDir()
	_, spec := publicSpec(t, site, `
		"turn":{"run":{"runtime":"docker","docker":{"image":"coturn/coturn:4.6.3"},"command":["-n","--external-ip=$MIDDLEWARE_PUBLIC_IP"]},
		        "network":"host","ports":["3478/udp","3478/tcp","49160-49400/udp"]},
		"dns":{"run":{"runtime":"docker","docker":{"image":"acme/dns:1","port":5353}},"ports":["53/udp","5300-5310/tcp"]}`)
	d := Docker{Bin: bin, House: "/srv/test"}
	logs := filepath.Join(t.TempDir(), "logs")
	for i, name := range []string{"turn", "dns"} {
		if _, err := d.Start(context.Background(), spec(name), 41300+i, filepath.Join(logs, name+".log"), t.TempDir()); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	runs := callsOf(t, calls, "run ")
	if len(runs) != 2 {
		t.Fatalf("runs: %q", runs)
	}
	turn, dns := runs[0], runs[1]
	for _, want := range []string{"--network host", "--cap-drop ALL", "--cap-add NET_BIND_SERVICE", "--security-opt no-new-privileges", "-e MIDDLEWARE_PUBLIC_IP=203.0.113.7", "coturn/coturn:4.6.3 -n --external-ip=203.0.113.7"} {
		if !strings.Contains(turn, want) {
			t.Errorf("turn's run lacks %q: %s", want, turn)
		}
	}
	if strings.Count(turn, "--cap-add") != 1 {
		t.Errorf("one capability comes back, the one a network daemon's binary is marked with: %s", turn)
	}
	for _, not := range []string{" -p ", "--network mw-", "--network-alias", "--privileged"} {
		if strings.Contains(turn, not) {
			t.Errorf("turn's run has %q: %s", not, turn)
		}
	}
	for _, want := range []string{"-p 53:53/udp", "-p 5300-5310:5300-5310/tcp", "--network mw-p2p-test-net", "--cap-drop ALL"} {
		if !strings.Contains(dns, want) {
			t.Errorf("dns's run lacks %q: %s", want, dns)
		}
	}
	// It answers no name and no process of the site reaches it, so its
	// own port stays off the machine's loopback as any internal one's.
	if strings.Contains(dns, "127.0.0.1:") {
		t.Errorf("dns's port inside was published on loopback: %s", dns)
	}
	time.Sleep(50 * time.Millisecond)
}

// stunBinding sends a STUN binding request to addr over UDP and returns
// once a binding success comes back with the same transaction.
func stunBinding(addr string, wait time.Duration) error {
	c, err := net.Dial("udp", addr)
	if err != nil {
		return err
	}
	defer c.Close()
	req := make([]byte, 20)
	binary.BigEndian.PutUint16(req[0:], 0x0001)     // binding request
	binary.BigEndian.PutUint32(req[4:], 0x2112A442) // magic cookie
	_, _ = rand.Read(req[8:])
	if _, err := c.Write(req); err != nil {
		return err
	}
	_ = c.SetReadDeadline(time.Now().Add(wait))
	buf := make([]byte, 1500)
	n, err := c.Read(buf)
	if err != nil {
		return err
	}
	if n < 20 || binary.BigEndian.Uint16(buf[0:]) != 0x0101 || string(buf[8:20]) != string(req[8:20]) {
		return fmt.Errorf("not a binding success: % x", buf[:min(n, 20)])
	}
	return nil
}

// TestARealRelayAnswersSTUN is the thing itself, with DOCKER=1 and the
// coturn image on the machine: a relay declared as a service, started by
// the house's own runtime with nothing done by hand, answers a STUN
// request over UDP. Once on the machine's network (what a relay with a
// range of ports wants) and once with its port published through docker.
func TestARealRelayAnswersSTUN(t *testing.T) {
	if os.Getenv("DOCKER") != "1" {
		t.Skip("DOCKER=1 runs a real relay")
	}
	image := os.Getenv("TURN_IMAGE")
	if image == "" {
		image = "coturn/coturn:4.6.3-alpine"
	}
	root := t.TempDir()
	site := filepath.Join(root, "sites", "p2p.test")
	os.MkdirAll(site, 0o755)
	relay := func(name, network, port string) string {
		// As a user that is not root, which is what the house's user is
		// on a real node; the test itself may well run as root.
		return fmt.Sprintf(`%q:{"run":{"runtime":"docker","docker":{"image":%q,"memory":"128m","user":"1000:1000"},
			"command":["-n","--no-cli","--log-file=stdout","--listening-port=%s","--external-ip=$MIDDLEWARE_PUBLIC_IP","--min-port=49160","--max-port=49170","--use-auth-secret","--static-auth-secret=test","--realm=p2p.test"],
			"health":{"command":["turnutils_stunclient","-p","%s","127.0.0.1"]}},
			%s"ports":["%s/udp","%s/tcp"]}`, name, image, port, port, network, port, port)
	}
	_, spec := publicSpec(t, site, relay("host", `"network":"host",`, "3478")+","+relay("bridge", "", "3479"))
	d := Docker{House: site}
	for i, tc := range []struct{ name, addr string }{{"host", "127.0.0.1:3478"}, {"bridge", "127.0.0.1:3479"}} {
		t.Run(tc.name, func(t *testing.T) {
			logFile := filepath.Join(root, "data", "logs", tc.name+".log")
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			inst, err := d.Start(ctx, spec(tc.name), 41400+i, logFile, filepath.Join(root, "data", "homes", "p2p.test"))
			if err != nil {
				t.Fatal(err)
			}
			defer inst.Stop(context.Background(), 2*time.Second)
			var last error
			for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
				if last = stunBinding(tc.addr, 2*time.Second); last == nil {
					break
				}
			}
			if last != nil {
				log, _ := os.ReadFile(logFile)
				t.Fatalf("no STUN answer from %s: %v\n%s", tc.addr, last, log)
			}
			// The house's own check, run inside it: what health.command is.
			if err := inst.(Checker).Check(ctx, spec(tc.name).Health.Command); err != nil {
				t.Fatalf("health.command: %v", err)
			}
			// And its output is the service's log, where `site logs
			// --service` reads it.
			time.Sleep(time.Second)
			if log, _ := os.ReadFile(logFile); len(log) == 0 {
				t.Error("the relay's log is empty")
			}
		})
	}
}
