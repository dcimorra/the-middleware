package main

import (
	"slices"
	"testing"

	"github.com/dcimorra/the-middleware/internal/engine"
)

// The guard is told a relay's approved ports are its site's (it reported
// the first one as a port nobody had assigned), and is not told the ones
// that still wait for the owner.
func TestTheGuardKnowsAServicesApprovedPublicPorts(t *testing.T) {
	sn := engine.Snapshot{Domain: "p2p.test", Dir: t.TempDir(), Port: 33001, Services: map[string]engine.ServiceSnapshot{
		"turn": {Network: "host", Ports: []string{"3478/udp", "3478/tcp", "49160-49162/udp"}},
	}}
	got := guardSite(sn).Ports
	for _, want := range []int{33001, 3478, 49160, 49161, 49162} {
		if !slices.Contains(got, want) {
			t.Fatalf("%d is not among the site's ports: %v", want, got)
		}
	}
	turn := sn.Services["turn"]
	turn.Pending = "turn 3478/udp"
	sn.Services["turn"] = turn
	if got := guardSite(sn).Ports; slices.Contains(got, 3478) {
		t.Fatalf("a port the owner has not approved is the site's already: %v", got)
	}
}
