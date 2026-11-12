package api

import (
	"strings"
	"testing"

	"github.com/dcimorra/the-middleware/internal/access"
)

// The key is made by the house, answered once, and only its hash reaches
// the manifest, next to whatever else the access section held.
func TestASiteKeyIsAnsweredOnceAndItsHashWritten(t *testing.T) {
	f := newFixture(t).withToken()
	if resp, body := f.call("POST", "/api/v1/sites", map[string]any{"domain": "k.test", "template": "node"}); resp.StatusCode != 201 {
		t.Fatalf("create: %d %v", resp.StatusCode, body)
	}
	if resp, body := f.call("PATCH", "/api/v1/sites/k.test/manifest", `{"access":{"allow":["192.0.2.0/24"]}}`); resp.StatusCode != 200 {
		t.Fatalf("patch: %d %v", resp.StatusCode, body)
	}
	resp, body := f.call("POST", "/api/v1/sites/k.test/access/key", nil)
	key, _ := body["key"].(string)
	if resp.StatusCode != 200 || key == "" || body["url"] != "https://k.test/?access-token="+key {
		t.Fatalf("on: %d %v", resp.StatusCode, body)
	}
	_, site := f.call("GET", "/api/v1/sites/k.test", nil)
	raw := text(site)
	if strings.Contains(raw, key) || !strings.Contains(raw, access.HashKey(key)) || !strings.Contains(raw, "192.0.2.0/24") {
		t.Fatalf("manifest after on: %s", raw)
	}
	if resp, body := f.call("DELETE", "/api/v1/sites/k.test/access/key", nil); resp.StatusCode != 200 {
		t.Fatalf("off: %d %v", resp.StatusCode, body)
	}
	_, site = f.call("GET", "/api/v1/sites/k.test", nil)
	if raw := text(site); strings.Contains(raw, "sha256:") || !strings.Contains(raw, "192.0.2.0/24") {
		t.Fatalf("manifest after off: %s", raw)
	}
}
