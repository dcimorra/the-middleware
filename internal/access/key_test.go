package access

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/pages"
)

// A site behind its key, crossed over a real connection with a browser's
// cookie jar: without the key, the page of a name nobody serves, ahead of
// the site's own forbidden page and password box; with it once, a pass
// and the same address without it; a new key ends the old pass.
func TestASiteBehindItsKeyAnswersOnlyWhoBringsIt(t *testing.T) {
	key, stored := NewKey()
	pw, _ := HashPassword("open sesame")
	ac := &manifest.Access{Key: stored, Password: pw, Deny: []string{"10.0.0.0/8"}}
	cur := ac
	a := newAccess(t, func(host string) (Site, bool) {
		if strings.HasPrefix(host, "p.example") {
			return Site{Domain: "p.example", Access: cur}, true
		}
		return Site{}, false
	})
	srv := httptest.NewServer(a.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "the site")
	})))
	defer srv.Close()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(path string) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		req.Host = "p.example"
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "http://p.example/", nil)
	pages.Write(rec, r, "unknown", http.StatusNotFound)
	unknown := rec.Body.String()

	for _, path := range []string{"/", "/admin", LoginPath, "/?" + KeyParam + "=wrong"} {
		resp, body := get(path)
		if resp.StatusCode != http.StatusNotFound || body != unknown {
			t.Fatalf("%s without the key: %d\n%s", path, resp.StatusCode, body)
		}
	}

	resp, _ := get("/shop?x=1&" + KeyParam + "=" + url.QueryEscape(key))
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/shop?x=1" {
		t.Fatalf("with the key: %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	// Past the key, the site's own rules as before: its password.
	if resp, body := get("/shop?x=1"); resp.StatusCode != http.StatusUnauthorized || body == unknown {
		t.Fatalf("with the pass, the password door: %d", resp.StatusCode)
	}
	cur = &manifest.Access{Key: stored}
	if resp, body := get("/"); resp.StatusCode != 200 || body != "the site" {
		t.Fatalf("with the pass: %d %q", resp.StatusCode, body)
	}

	// A new key: the old pass opens nothing.
	_, other := NewKey()
	cur = &manifest.Access{Key: other}
	if resp, _ := get("/"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("the old pass after a new key: %d", resp.StatusCode)
	}
	cur = &manifest.Access{}
	if resp, body := get("/"); resp.StatusCode != 200 || body != "the site" {
		t.Fatalf("no key: %d %q", resp.StatusCode, body)
	}
}

func TestAKeyInTheManifestMustBeAHash(t *testing.T) {
	_, stored := NewKey()
	for k, ok := range map[string]bool{stored: true, "my-secret": false, "sha256:abc": false, strings.ToUpper(stored): false} {
		_, err := manifest.Parse([]byte(`{"version":2,"run":{"command":["x"]},"access":{"key":"` + k + `"}}`))
		if (err == nil) != ok {
			t.Errorf("%q: %v", k, err)
		}
	}
}
