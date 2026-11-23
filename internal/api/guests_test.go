package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dcimorra/the-middleware/internal/auth"
	"github.com/dcimorra/the-middleware/internal/backups"
)

// flat is text without its indentation, for matching a piece of JSON.
func flat(v map[string]any) string {
	return strings.Join(strings.Fields(text(v)), "")
}

// asTurn makes a request as a turn of site's conversation makes it over
// the socket: no credentials, the connection marked with the site.
func (f *fixture) asTurn(site, method, path string) (int, string) {
	f.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.srv.Panel().ServeHTTP(rec, req.WithContext(auth.WithSocketTurn(context.Background(), site)))
	return rec.Code, rec.Body.String()
}

// The site that is entered names its guests, and a guest's turn then
// reaches it at the socket as far as the pass says: to read, how the
// host is, its logs, its events and the copies of its database, and
// nothing that changes it; to write, what the host's own turn has. A
// site that is not named reaches nothing, whatever its own manifest
// says, and a pass taken back is gone at the next request.
func TestAGuestsTurnReachesItsHostAsFarAsThePassSays(t *testing.T) {
	f := newFixture(t).withToken()
	f.srv.RegisterBackups(backups.New(backups.Options{Root: f.root, Bus: f.bus}))
	for _, d := range []string{"old.test", "new.test", "other.test"} {
		f.makeSite(d)
	}
	// The visitor's own manifest opens nothing.
	if res, out := f.call("PATCH", "/api/v1/sites/new.test/manifest", `{"guests":{"new.test":"write"},"chat":{"reads":["old.test"]}}`); res.StatusCode != 200 {
		t.Fatalf("patch: %d %s", res.StatusCode, flat(out))
	}
	if code, body := f.asTurn("new.test", "GET", "/api/v1/sites/old.test"); code != 403 || !strings.Contains(body, "does not take new.test in") {
		t.Fatalf("a site let itself in: %d %s", code, body)
	}

	if res, out := f.call("PUT", "/api/v1/sites/old.test/guests/new.test", map[string]any{}); res.StatusCode != 200 || !strings.Contains(flat(out), `"level":"read"`) {
		t.Fatalf("add: %d %s", res.StatusCode, flat(out))
	}
	raw, _ := os.ReadFile(filepath.Join(f.root, "sites", "old.test", "site.json"))
	if !strings.Contains(string(raw), `"new.test": "read"`) {
		t.Fatalf("the pass is not in the host's manifest:\n%s", raw)
	}
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/api/v1/sites/old.test", 200},
		{"GET", "/api/v1/sites/old.test/logs", 200},
		{"GET", "/api/v1/sites/old.test/backups", 200},
		{"GET", "/api/v1/sites/old.test/guests", 200},
		{"GET", "/api/v1/events?subject=old.test", 200},
		{"POST", "/api/v1/sites/old.test/restart", 403},
		{"POST", "/api/v1/sites/old.test/deploy", 403},
		{"POST", "/api/v1/sites/old.test/exec", 403},
		{"POST", "/api/v1/sites/old.test/backups/db/run", 403},
		{"PUT", "/api/v1/sites/old.test/guests/other.test", 403},
		{"DELETE", "/api/v1/sites/old.test", 403},
		{"GET", "/api/v1/sites/other.test", 403},
		{"GET", "/api/v1/events?subject=other.test", 403},
		// Its own site, as before.
		{"GET", "/api/v1/sites/new.test", 200},
		{"PUT", "/api/v1/sites/new.test/guests/other.test", 403},
	} {
		if code, body := f.asTurn("new.test", c.method, c.path); code != c.want {
			t.Errorf("read guest: %s %s: %d, want %d: %s", c.method, c.path, code, c.want, body)
		}
	}
	if _, body := f.asTurn("new.test", "POST", "/api/v1/sites/old.test/restart"); !strings.Contains(body, "takes new.test in to read") {
		t.Errorf("the refusal does not say the pass is to read: %s", body)
	}
	// One way: the host's turn does not reach its guest.
	if code, _ := f.asTurn("old.test", "GET", "/api/v1/sites/new.test"); code != 403 {
		t.Errorf("the host reached its guest: %d", code)
	}

	if res, out := f.call("PUT", "/api/v1/sites/old.test/guests/new.test", map[string]any{"level": "write"}); res.StatusCode != 200 {
		t.Fatalf("write: %d %s", res.StatusCode, flat(out))
	}
	for _, path := range []string{"/restart", "/deploy", "/backups/db/run"} {
		if code, body := f.asTurn("new.test", "POST", "/api/v1/sites/old.test"+path); code == 403 || code == 401 {
			t.Errorf("write guest: POST %s refused: %d %s", path, code, body)
		}
	}
	for _, c := range []struct{ method, path string }{{"DELETE", "/api/v1/sites/old.test"}, {"PUT", "/api/v1/sites/old.test/guests/other.test"}, {"GET", "/api/v1/sites/other.test"}} {
		if code, _ := f.asTurn("new.test", c.method, c.path); code != 403 {
			t.Errorf("write guest: %s %s: %d, want 403", c.method, c.path, code)
		}
	}

	if res, out := f.call("DELETE", "/api/v1/sites/old.test/guests/new.test", nil); res.StatusCode != 200 || strings.Contains(flat(out), `"site":"new.test"`) {
		t.Fatalf("remove: %d %s", res.StatusCode, flat(out))
	}
	if code, _ := f.asTurn("new.test", "GET", "/api/v1/sites/old.test"); code != 403 {
		t.Fatalf("a pass taken back still opens: %d", code)
	}
	if raw, _ := os.ReadFile(filepath.Join(f.root, "sites", "old.test", "site.json")); strings.Contains(string(raw), "guests") {
		t.Fatalf("an empty list stayed in the manifest:\n%s", raw)
	}
}

// What is refused when a guest is added: a name that is no site of this
// node, the site itself, a level that is neither. And the view says who
// could be added and who takes this site in.
func TestAGuestIsASiteOfThisNode(t *testing.T) {
	f := newFixture(t).withToken()
	for _, d := range []string{"old.test", "new.test", "other.test"} {
		f.makeSite(d)
	}
	for _, c := range []struct {
		guest string
		body  any
		want  int
	}{
		{"nowhere.test", map[string]any{}, 404},
		{"old.test", map[string]any{}, 400},
		{"new.test", map[string]any{"level": "all"}, 400},
		{"New_Test", map[string]any{}, 400},
	} {
		if res, out := f.call("PUT", "/api/v1/sites/old.test/guests/"+c.guest, c.body); res.StatusCode != c.want {
			t.Errorf("%s %v: %d, want %d: %s", c.guest, c.body, res.StatusCode, c.want, flat(out))
		}
	}
	if res, _ := f.call("DELETE", "/api/v1/sites/old.test/guests/new.test", nil); res.StatusCode != 404 {
		t.Errorf("removing a guest that is not one: %d", res.StatusCode)
	}
	f.call("PUT", "/api/v1/sites/old.test/guests/new.test", map[string]any{"level": "write"})
	_, view := f.call("GET", "/api/v1/sites/old.test/guests", nil)
	if got := flat(view); !strings.Contains(got, `"candidates":["other.test"]`) || !strings.Contains(got, `{"site":"new.test","level":"write","here":true}`) {
		t.Fatalf("host's view: %s", got)
	}
	_, view = f.call("GET", "/api/v1/sites/new.test/guests", nil)
	if got := flat(view); !strings.Contains(got, `"hosts":[{"site":"old.test","write":true}]`) || !strings.Contains(got, `"guests":[]`) {
		t.Fatalf("guest's view: %s", got)
	}
}

// A delegate who may change a site's code names its guests, among the
// sites that are theirs: any other is answered as one that does not
// exist, so the list tells them nothing about the rest of the house. A
// delegate who only operates reads the list and changes nothing.
func TestADelegateTakesInOnlyTheirOwnSites(t *testing.T) {
	f := newFixture(t).withToken()
	for _, d := range []string{"old.example", "new.example", "theirs.example"} {
		f.makeSite(d)
	}
	f.makeUser("ann", auth.Grant{Site: "old.example", Perms: []string{"operate", "code"}}, auth.Grant{Site: "new.example", Perms: []string{"operate"}})
	f.makeUser("bob", auth.Grant{Site: "old.example", Perms: []string{"operate"}})
	ann, bob := f.login("ann"), f.login("bob")

	res, out := ann.call("GET", "/api/v1/sites/old.example/guests", nil)
	if res.StatusCode != 200 || !strings.Contains(flat(out), `"candidates":["new.example"]`) {
		t.Fatalf("ann's view: %d %s", res.StatusCode, flat(out))
	}
	if res, out := ann.call("PUT", "/api/v1/sites/old.example/guests/theirs.example", map[string]any{}); res.StatusCode != 404 {
		t.Fatalf("a delegate named a site that is not theirs: %d %s", res.StatusCode, flat(out))
	}
	resMissing, outMissing := ann.call("PUT", "/api/v1/sites/old.example/guests/nowhere.example", map[string]any{})
	resTheirs, outTheirs := ann.call("PUT", "/api/v1/sites/old.example/guests/theirs.example", map[string]any{})
	if resMissing.StatusCode != resTheirs.StatusCode || strings.ReplaceAll(flat(outMissing), "nowhere", "theirs") != flat(outTheirs) {
		t.Fatalf("the answers tell a site that exists from one that does not:\n%s\n%s", flat(outMissing), flat(outTheirs))
	}
	if res, out := ann.call("PUT", "/api/v1/sites/old.example/guests/new.example", map[string]any{}); res.StatusCode != 200 {
		t.Fatalf("ann adds her own: %d %s", res.StatusCode, flat(out))
	}
	if res, _ := ann.call("PUT", "/api/v1/sites/new.example/guests/old.example", map[string]any{}); res.StatusCode != http.StatusForbidden {
		t.Fatalf("without code on the host: %d", res.StatusCode)
	}
	if res, _ := bob.call("GET", "/api/v1/sites/old.example/guests", nil); res.StatusCode != 200 {
		t.Fatalf("bob reads: %d", res.StatusCode)
	}
	if res, _ := bob.call("DELETE", "/api/v1/sites/old.example/guests/new.example", nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("bob removes: %d", res.StatusCode)
	}
}
