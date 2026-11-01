package manifest

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestForceHTTPSIsPerNameAndChecked(t *testing.T) {
	var m Manifest
	if err := json.Unmarshal([]byte(`{"version":2,"static":{},"aliases":["b.test"],"force_https":["A.test.","www.a.test"]}`), &m); err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("valid manifest refused: %v", err)
	}
	for host, want := range map[string]bool{"a.test": true, "www.a.test": true, "b.test": false, "": false} {
		if got := m.ForcesHTTPS(host); got != want {
			t.Errorf("ForcesHTTPS(%q) = %v, want %v", host, got, want)
		}
	}
	var none *Manifest
	if none.ForcesHTTPS("a.test") {
		t.Fatal("a nil manifest keeps a name to https")
	}
	m.ForceHTTPS = []string{"https://a.test/"}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "force_https") {
		t.Fatalf("an address taken for a name: %v", err)
	}
}
