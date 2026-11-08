package pages

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestLocalPathNeverLeavesTheSite(t *testing.T) {
	for in, want := range map[string]string{
		"":                      "/",
		"/":                     "/",
		"/admin?x=1":            "/admin?x=1",
		"/a/b#c":                "/a/b#c",
		"//evil.example":        "/",
		"/\\evil.example":       "/",
		"\\\\evil.example":      "/",
		"/\t/evil.example":      "/",
		"/\n/evil.example":      "/",
		"/\r/evil.example":      "/",
		"/\x0b/evil.example":    "/",
		"/\x7f/evil.example":    "/",
		"https://evil.example/": "/",
		"evil.example":          "/",
		"/%09/evil.example":     "/%09/evil.example", // a path: escaped, nothing drops it
		"/%2F%2Fevil.example/x": "/%2F%2Fevil.example/x",
		"/café":                 "/café",
		"javascript:alert(1)":   "/",
	} {
		if got := LocalPath(in); got != want {
			t.Errorf("LocalPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// The redirect a browser follows is what matters: read the Location the
// way a browser does (tabs and newlines dropped, backslashes as slashes)
// and it must still name no host.
func TestLocalPathRedirectsStayOnTheSiteAsABrowserReadsThem(t *testing.T) {
	browser := strings.NewReplacer("\t", "", "\n", "", "\r", "", "\\", "/")
	for _, in := range []string{"/\t/evil.example", "/\n/evil.example", "/\\evil.example", "/ok", "//evil.example"} {
		w := httptest.NewRecorder()
		http.Redirect(w, httptest.NewRequest("POST", "https://site.example/__mw/login", nil), LocalPath(in), http.StatusSeeOther)
		loc := browser.Replace(w.Header().Get("Location"))
		u, err := url.Parse("https://site.example/__mw/login")
		if err != nil {
			t.Fatal(err)
		}
		ref, err := url.Parse(loc)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got := u.ResolveReference(ref).Host; got != "site.example" {
			t.Errorf("%q: the browser goes to %q", in, got)
		}
	}
}
