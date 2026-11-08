package manifest

import (
	"strings"
	"testing"
	"time"
)

func TestACanaryIsRefusedWhereTwoGenerationsCannotServeSideBySide(t *testing.T) {
	ok := `{"version":2,"run":{"command":["x"],"canary":{}}}`
	m, err := Parse([]byte(ok))
	if err != nil {
		t.Fatal(err)
	}
	c := m.Run.Canary
	if c.Share() != 10 || c.Window() != 10*time.Minute || c.MaxErrors() != 0.05 || c.Enough() != 20 {
		t.Fatalf("defaults: %d %s %v %d", c.Share(), c.Window(), c.MaxErrors(), c.Enough())
	}
	spec, err := Resolve("a.test", t.TempDir(), m)
	if err != nil || spec.Canary == nil {
		t.Fatalf("the spec lost the canary: %v", err)
	}
	plain, _ := Parse([]byte(`{"version":2,"run":{"command":["x"]}}`))
	if p, _ := Resolve("a.test", spec.SiteDir, plain); p.Revision != spec.Revision {
		t.Fatal("turning the canary on changed the revision, which would start a generation")
	}
	for body, want := range map[string]string{
		`{"version":2,"run":{"command":["x"],"port":4000,"canary":{}}}`:                               "run.port",
		`{"version":2,"run":{"command":["x"],"canary":{"percent":100}}}`:                              "percent",
		`{"version":2,"run":{"command":["x"],"canary":{"for":"1s"}}}`:                                 "for",
		`{"version":2,"run":{"command":["x"],"canary":{"max_error_rate":1.5}}}`:                       "max_error_rate",
		`{"version":2,"services":{"web":{"run":{"command":["x"],"canary":{}},"domains":["a.test"]}}}`: "service",
	} {
		if _, err := Parse([]byte(body)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", body, err)
		}
	}
}
