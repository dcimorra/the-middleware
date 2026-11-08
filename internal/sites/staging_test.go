package sites

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/access"
	"github.com/dcimorra/the-middleware/internal/manifest"
)

func served(t *testing.T, m *Manager, domain, want string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	got := ""
	for time.Now().Before(deadline) {
		if s, err := m.Get(domain); err == nil && s.Alive && s.Port != 0 {
			if resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(s.Port) + "/"); err == nil {
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if got = string(b); got == want {
					return
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s serves %q, want %q", domain, got, want)
}

func write(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}

// The whole round, on running sites: a copy is made from production with
// its data, it is changed, promoted back with production's data kept
// (and written to while the copy waits), undone, and refreshed.
func TestATestCopyIsMadePromotedUndoneAndRefreshed(t *testing.T) {
	f := newFixture(t, true)
	mf := runManifest()
	mf.Run.Env["MW_TEST_FILE"] = "version.txt"
	mf.Aliases = []string{"shop-old.test"}
	if _, err := f.m.Create("shop.test", mf, ""); err != nil {
		t.Fatal(err)
	}
	prod := f.m.Dir("shop.test")
	write(t, prod, "version.txt", "v1")
	write(t, prod, "app.db", "orders: 1")
	write(t, prod, "uploads/a.jpg", "a")
	write(t, prod, ".env", "STRIPE=live")
	served(t, f.m, "shop.test", "v1")

	res, err := f.m.CreateStaging(context.Background(), "shop.test", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Site.Domain != "staging.shop.test" || res.Password == "" {
		t.Fatalf("result %+v", res)
	}
	copyDir := f.m.Dir("staging.shop.test")
	cm, err := manifest.Load(copyDir)
	if err != nil {
		t.Fatal(err)
	}
	if cm.Staging == nil || cm.Staging.Of != "shop.test" || len(cm.Aliases) != 0 || cm.Run.Env["MIDDLEWARE_STAGING"] != "shop.test" {
		t.Fatalf("copy's manifest: %+v", cm)
	}
	if cm.Access == nil || !access.CheckPassword(cm.Access.Password, res.Password) {
		t.Fatal("the copy is not behind the password it was given")
	}
	for rel, want := range map[string]string{"version.txt": "v1", "app.db": "orders: 1", "uploads/a.jpg": "a", ".env": "STRIPE=live"} {
		if got := read(t, copyDir, rel); got != want {
			t.Errorf("copy's %s = %q, want %q", rel, got, want)
		}
	}
	served(t, f.m, "staging.shop.test", "v1")
	if st, _ := f.m.Staging("shop.test"); len(st.Copies) != 1 || st.Copies[0] != "staging.shop.test" {
		t.Fatalf("production does not know its copy: %+v", st)
	}

	// Work on the copy; production goes on taking orders.
	write(t, copyDir, "version.txt", "v2")
	write(t, copyDir, "app.db", "orders: test")
	write(t, copyDir, "uploads/b.jpg", "b")
	write(t, copyDir, ".env", "STRIPE=test")
	write(t, prod, "app.db", "orders: 2")

	if _, err := f.m.PromoteStaging(context.Background(), "staging.shop.test"); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{"version.txt": "v2", "app.db": "orders: 2", "uploads/a.jpg": "a", ".env": "STRIPE=live"} {
		if got := read(t, prod, rel); got != want {
			t.Errorf("after promoting, production's %s = %q, want %q", rel, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(prod, "uploads", "b.jpg")); err == nil {
		t.Error("the copy's upload replaced production's uploads")
	}
	if pm, err := manifest.Load(prod); err != nil || pm.Staging != nil || len(pm.Aliases) != 1 {
		t.Fatalf("production's manifest was not kept: %+v %v", pm, err)
	}
	served(t, f.m, "shop.test", "v2")

	// Orders after the promotion survive its undo.
	write(t, prod, "app.db", "orders: 3")
	if st, _ := f.m.Staging("shop.test"); st.Before.IsZero() {
		t.Fatal("no folder set aside to undo with")
	}
	if _, err := f.m.UndoPromotion(context.Background(), "shop.test"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, prod, "version.txt"); got != "v1" {
		t.Fatalf("after the undo production has %q", got)
	}
	if got := read(t, prod, "app.db"); got != "orders: 3" {
		t.Fatalf("the undo lost the orders taken since the promotion: %q", got)
	}
	served(t, f.m, "shop.test", "v1")

	// The copy is made the image of production again, data too, and
	// stays the copy.
	if _, err := f.m.RefreshStaging(context.Background(), "staging.shop.test"); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{"version.txt": "v1", "app.db": "orders: 3", ".env": "STRIPE=live"} {
		if got := read(t, copyDir, rel); got != want {
			t.Errorf("after refreshing, the copy's %s = %q, want %q", rel, got, want)
		}
	}
	if cm, err := manifest.Load(copyDir); err != nil || cm.Staging == nil {
		t.Fatalf("the refreshed copy lost its mark: %v", err)
	}
	served(t, f.m, "staging.shop.test", "v1")

	for _, d := range []string{"staging.shop.test", "shop.test"} {
		f.m.Delete(context.Background(), d, false)
	}
}

func TestACopysManifestNamesOnlyTheCopy(t *testing.T) {
	prod := &manifest.Manifest{
		Version: 2, Title: "Shop", ForceHTTPS: []string{"shop.test"},
		Access: &manifest.Access{Password: "$2a$already"},
		Services: map[string]*manifest.Service{
			"web": {Run: &manifest.Run{Command: []string{"web"}, Port: 8080}, Domains: []string{"shop.test", "www.shop.test", "elsewhere.test"}},
			"api": {Run: &manifest.Run{Command: []string{"api"}}, Domains: []string{"api.shop.test"}},
		},
		Cluster: &manifest.Cluster{Replicas: 1},
	}
	mf, notes, password, err := stagingManifest(prod, "shop.test", "staging.shop.test")
	if err != nil {
		t.Fatal(err)
	}
	if password != "" {
		t.Error("production has a password and the copy got another")
	}
	if got := strings.Join(mf.Services["web"].Domains, ","); got != "staging.shop.test,www.staging.shop.test" {
		t.Errorf("web domains %q", got)
	}
	if got := strings.Join(mf.Services["api"].Domains, ","); got != "api.staging.shop.test" {
		t.Errorf("api domains %q", got)
	}
	if mf.Services["web"].Run.Port != 0 || mf.Cluster != nil || len(mf.ForceHTTPS) != 0 || len(mf.Aliases) != 0 || mf.Title != "Shop (staging)" {
		t.Errorf("copy kept production's: %+v", mf)
	}
	if prod.Services["web"].Run.Port != 8080 || len(prod.ForceHTTPS) != 1 {
		t.Error("making the copy's manifest changed production's")
	}
	if !strings.Contains(strings.Join(notes, "\n"), "elsewhere.test") {
		t.Errorf("a name left out is not said: %v", notes)
	}
}

func TestOnlyACopyIsPromotedAndACopyIsNotCopied(t *testing.T) {
	f := newFixture(t, false)
	if _, err := f.m.Create("a.test", nil, "static"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.PromoteStaging(context.Background(), "a.test"); err != ErrNotStaging {
		t.Fatalf("promoting production: %v", err)
	}
	if _, err := f.m.CreateStaging(context.Background(), "a.test", "b.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.CreateStaging(context.Background(), "b.test", ""); err == nil {
		t.Fatal("a copy of a copy was made")
	}
	if _, err := f.m.CreateStaging(context.Background(), "a.test", "b.test"); err != ErrExists {
		t.Fatalf("a copy over an existing site: %v", err)
	}
	if _, err := f.m.UndoPromotion(context.Background(), "a.test"); err == nil {
		t.Fatal("an undo with nothing to undo")
	}
}

func TestKeepGlobsMatchNamesAnywhereAndPathsFromTheFolder(t *testing.T) {
	for rel, want := range map[string]bool{
		"app.db": true, "var/app.sqlite3": true, "uploads": true, "a/uploads": true,
		"storage/app/x": false, ".env.local": true, "src/index.js": false,
	} {
		if got := manifest.Kept(manifest.DefaultKeep, rel); got != want {
			t.Errorf("default %s: %v", rel, got)
		}
	}
	if !manifest.Kept([]string{"storage/app/*"}, "storage/app/x") || manifest.Kept([]string{"storage/app/*"}, "other/storage/app/x") {
		t.Error("a glob with a slash is a path from the folder")
	}
}
