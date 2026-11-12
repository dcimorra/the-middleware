package publish

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/engine"
	"github.com/dcimorra/the-middleware/internal/events"
)

// The supervisor starts the publisher and the engine side by side, and the
// publisher's first pass ran before the engine had read a single folder:
// every published site was warned as publish.orphan at each start, in the
// same second it was adopted and answering. This is that start, with a
// real engine: nothing is said until its first pass is over, and a folder
// that then really goes is still said, once.
func TestAStartCallsNoPublishedSiteAnOrphanBeforeTheEngineHasReadTheFolders(t *testing.T) {
	root := t.TempDir()
	sites := filepath.Join(root, "sites")
	for _, d := range []string{"a.example.com", "b.example.com"} {
		if err := os.MkdirAll(filepath.Join(sites, d), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sites, d, "site.json"), []byte(`{"version":2}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e := engine.New(engine.Options{SitesDir: sites, DataDir: filepath.Join(root, "data"), Bus: events.New(""), Tick: 50 * time.Millisecond})

	f := newFixture(t)
	f.p.opts.Hosts = e.Hosts
	f.p.opts.Loaded = e.Loaded
	f.p.opts.Settle = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Published by an earlier supervisor: the intents are in the file.
	f.p.mu.Lock()
	for _, d := range []string{"a.example.com", "b.example.com"} {
		f.p.f.Sites[d] = Intent{Door: "tunnel"}
		f.p.f.Names[d] = &Record{Site: d, Door: "tunnel", State: StatePublished}
	}
	f.p.mu.Unlock()

	ch, unsub := f.bus.Subscribe()
	defer unsub()
	orphans := func() (n int) {
		for len(ch) > 0 {
			if (<-ch).Type == "publish.orphan" {
				n++
			}
		}
		return n
	}

	// The publisher's first pass, the engine not yet running.
	f.p.Reconcile(ctx)
	if n := orphans(); n != 0 {
		t.Fatalf("%d orphan warnings before the engine read its folders", n)
	}
	if got := len(f.p.Records()); got != 2 {
		t.Fatalf("records touched before the engine read its folders: %d", got)
	}

	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	wait := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); !ok(); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal(what)
			}
		}
	}
	wait("the engine never finished its first pass", e.Loaded)
	f.p.Reconcile(ctx)
	if n := orphans(); n != 0 {
		t.Fatalf("%d orphan warnings for sites that are there", n)
	}

	// A folder removed by hand is still an orphan, said once.
	if err := os.RemoveAll(filepath.Join(sites, "b.example.com")); err != nil {
		t.Fatal(err)
	}
	wait("the engine never dropped the removed folder", func() bool { _, ok := e.Hosts()["b.example.com"]; return !ok })
	f.p.Reconcile(ctx)
	f.p.Reconcile(ctx)
	if n := orphans(); n != 1 {
		t.Fatalf("%d orphan warnings for the removed folder, want 1", n)
	}
}
