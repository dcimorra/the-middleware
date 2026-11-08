package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/engine"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/ports"
	"github.com/dcimorra/the-middleware/internal/runtime"
)

// The canary tests run real generations of a site: this test binary,
// started by the engine with MW_CANARY_SITE set, answering its BUILD, and
// a 500 when the build is called "bad".
func TestMain(m *testing.M) {
	if os.Getenv("MW_CANARY_SITE") != "" {
		build := os.Getenv("BUILD")
		l, err := net.Listen("tcp", "127.0.0.1:"+os.Getenv("PORT"))
		if err != nil {
			os.Exit(1)
		}
		_ = http.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if build == "bad" && r.URL.Path != "/health" {
				http.Error(w, "broken", http.StatusInternalServerError)
				return
			}
			fmt.Fprint(w, build)
		}))
		return
	}
	os.Exit(m.Run())
}

type canaryHouse struct {
	e   *engine.Engine
	h   *Handler
	dir string
	bus *events.Bus
}

func newCanaryHouse(t *testing.T, canary string) *canaryHouse {
	t.Helper()
	root := t.TempDir()
	sites := filepath.Join(root, "sites")
	dir := filepath.Join(sites, "c.test")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, ".current"), []byte("one\n"), 0o644)
	m := fmt.Sprintf(`{"version":2,"run":{"command":[%q],"env":{"MW_CANARY_SITE":"1"},"env_from_file":{"BUILD":".current"},"drain":"200ms","health":{"path":"/health","timeout":"10s"},"canary":%s}}`, os.Args[0], canary)
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(m), 0o644)
	bus := events.New("")
	e := engine.New(engine.Options{SitesDir: sites, DataDir: filepath.Join(root, "data"), Ports: ports.New(41200, 41300), Bus: bus, Tick: 200 * time.Millisecond,
		Runtimes: map[string]runtime.Runtime{"process": runtime.Process{}}})
	ctx, cancel := context.WithCancel(context.Background())
	go e.Run(ctx)
	t.Cleanup(func() {
		// Generations outlive the engine on purpose: the site's folder
		// goes first, the engine stops what it ran, and only then does
		// the engine stop (which stops a canary still running).
		os.RemoveAll(dir)
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, ok := e.Lookup("c.test"); !ok {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		cancel()
		time.Sleep(300 * time.Millisecond)
	})
	h := New(e.Lookup)
	h.NoteCanary = e.NoteCanary
	house := &canaryHouse{e: e, h: h, dir: dir, bus: bus}
	house.wait(t, "the first generation", func(sn engine.Snapshot) bool { return sn.Alive && !sn.Handing })
	return house
}

func (c *canaryHouse) wait(t *testing.T, what string, cond func(engine.Snapshot) bool) engine.Snapshot {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if sn, ok := c.e.Lookup("c.test"); ok && cond(sn) {
			return sn
		}
		time.Sleep(50 * time.Millisecond)
	}
	sn, _ := c.e.Lookup("c.test")
	t.Fatalf("%s: never came: %+v", what, sn)
	return sn
}

// ask sends a request through the router, with a side's cookie or none.
func (c *canaryHouse) ask(cookie string) (int, string, string) {
	r := httptest.NewRequest("GET", "http://c.test/", nil)
	if cookie != "" {
		r.Header.Set("Cookie", cookie)
	}
	w := httptest.NewRecorder()
	c.h.ServeHTTP(w, r)
	body, _ := io.ReadAll(w.Result().Body)
	return w.Code, strings.TrimSpace(string(body)), w.Result().Header.Get("Set-Cookie")
}

func TestACanaryThatFailsIsRolledBackAndNotStartedAgain(t *testing.T) {
	c := newCanaryHouse(t, `{"percent":50,"min_requests":10}`)
	os.WriteFile(filepath.Join(c.dir, ".current"), []byte("bad\n"), 0o644)
	sn := c.wait(t, "the canary", func(sn engine.Snapshot) bool { return sn.Canary != nil })
	rev := sn.Canary.Revision
	// Visitors without a side draw one each; about half land on the
	// broken generation, which the engine counts.
	served := map[string]int{}
	for i := 0; i < 200; i++ {
		if sn, _ := c.e.Lookup("c.test"); sn.Canary == nil {
			break
		}
		code, body, _ := c.ask("")
		served[fmt.Sprint(code, " ", body)]++
		time.Sleep(5 * time.Millisecond)
	}
	sn = c.wait(t, "the rollback", func(sn engine.Snapshot) bool { return sn.Canary == nil && !sn.Handing })
	if !strings.Contains(sn.Error, "canary rolled back") || served["200 one"] == 0 || served["500 broken"] == 0 {
		t.Fatalf("error %q, served %v", sn.Error, served)
	}
	// Everyone is on the serving generation again, and it stays so: the
	// same revision is not tried a second time.
	time.Sleep(time.Second)
	if sn, _ := c.e.Lookup("c.test"); sn.Canary != nil || sn.Handing {
		t.Fatalf("the rolled back generation was started again: %+v", sn.Canary)
	}
	if code, body, _ := c.ask(canaryCookie + "=" + rev + ".c"); code != 200 || body != "one" {
		t.Fatalf("a visitor of the canary after the rollback: %d %q", code, body)
	}
	if f, ok := c.e.CanaryFailed("c.test"); !ok || f.Revision != rev {
		t.Fatalf("the rollback is not written down: %+v", f)
	}
	// A restart is the operator trying again.
	if err := c.e.Restart(context.Background(), "c.test"); err != nil {
		t.Fatal(err)
	}
	c.wait(t, "the canary again", func(sn engine.Snapshot) bool { return sn.Canary != nil })
}

func TestEachVisitorStaysOnTheirSideUntilTheCanaryIsPromoted(t *testing.T) {
	c := newCanaryHouse(t, `{"percent":30}`)
	os.WriteFile(filepath.Join(c.dir, ".current"), []byte("two\n"), 0o644)
	sn := c.wait(t, "the canary", func(sn engine.Snapshot) bool { return sn.Canary != nil })
	rev := sn.Canary.Revision
	for i := 0; i < 5; i++ {
		if _, body, _ := c.ask(canaryCookie + "=" + rev + ".c"); body != "two" {
			t.Fatalf("the canary's visitor got %q", body)
		}
		if _, body, _ := c.ask(canaryCookie + "=" + rev + ".s"); body != "one" {
			t.Fatalf("the serving side's visitor got %q", body)
		}
	}
	// A new visitor is given a side, and keeps it.
	_, first, set := c.ask("")
	if !strings.HasPrefix(set, canaryCookie+"="+rev+".") {
		t.Fatalf("no side given: %q", set)
	}
	cookie := strings.SplitN(set, ";", 2)[0]
	for i := 0; i < 5; i++ {
		if _, body, again := c.ask(cookie); body != first || again != "" {
			t.Fatalf("the visitor changed sides: %q then %q (%q)", first, body, again)
		}
	}
	if err := c.e.DecideCanary("c.test", true); err != nil {
		t.Fatal(err)
	}
	c.wait(t, "the promotion", func(sn engine.Snapshot) bool { return sn.Canary == nil && !sn.Handing && sn.Revision == rev })
	if _, body, _ := c.ask(canaryCookie + "=" + rev + ".s"); body != "two" {
		t.Fatalf("after the promotion: %q", body)
	}
	if err := c.e.DecideCanary("c.test", true); err != engine.ErrNoCanary {
		t.Fatalf("a decision with no canary: %v", err)
	}
}
