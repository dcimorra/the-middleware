package access

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/dcimorra/the-middleware/internal/edge"
	"github.com/dcimorra/the-middleware/internal/manifest"
)

// byPath is a site with services as the house resolves it: the site's
// access on every path, the api service's on /api.
func byPath(site, api *manifest.Access) func(r *http.Request, p string) (Site, bool) {
	return func(r *http.Request, p string) (Site, bool) {
		if edge.Host(r) != "p.example" {
			return Site{}, false
		}
		s := Site{Domain: "p.example", Access: site}
		if p == "/api" || strings.HasPrefix(p, "/api/") {
			s.More = api
		}
		return s, true
	}
}

func TestAServiceZoneUnderASiteZoneStillAsksTheSitesPassword(t *testing.T) {
	sitePass, _ := HashPassword("site pass")
	svcPass, _ := HashPassword("service pass")
	site := &manifest.Access{Routes: map[string]*manifest.RouteAccess{"/api": {Password: sitePass}}}
	api := &manifest.Access{Routes: map[string]*manifest.RouteAccess{"/api/admin": {Password: svcPass}}}
	a, err := New(Options{DataDir: t.TempDir(), LookupRequest: byPath(site, api)})
	if err != nil {
		t.Fatal(err)
	}
	h := a.Wrap(okHandler())
	// Merged into one list the service's zone was the longest prefix and
	// the site's password was never asked: both are.
	r := req("GET", "/api/admin/x", "203.0.113.1:1")
	r.SetBasicAuth("x", "service pass")
	if rec := do(h, r); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the service's password alone got %d", rec.Code)
	}
	r = req("GET", "/api/admin/x", "203.0.113.1:1")
	r.SetBasicAuth("x", "site pass")
	if rec := do(h, r); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the site's password alone got %d", rec.Code)
	}
	r = req("GET", "/api/other", "203.0.113.1:1")
	r.SetBasicAuth("x", "site pass")
	if rec := do(h, r); rec.Code != 200 {
		t.Fatalf("the site's zone with its password got %d", rec.Code)
	}
}

func TestAServicesDenyAndAllowOnlyCloseMore(t *testing.T) {
	site := &manifest.Access{Deny: []string{"198.51.100.0/24"}}
	api := &manifest.Access{Allow: []string{"10.0.0.0/8"}}
	a, err := New(Options{DataDir: t.TempDir(), LookupRequest: byPath(site, api)})
	if err != nil {
		t.Fatal(err)
	}
	h := a.Wrap(okHandler())
	for _, c := range []struct {
		path, from string
		want       int
	}{
		{"/", "203.0.113.1:1", 200},
		{"/", "198.51.100.9:1", http.StatusForbidden},
		{"/api/x", "203.0.113.1:1", http.StatusForbidden},
		{"/api/x", "10.1.2.3:1", 200},
	} {
		if rec := do(h, req("GET", c.path, c.from)); rec.Code != c.want {
			t.Errorf("%s from %s: %d, want %d", c.path, c.from, rec.Code, c.want)
		}
	}
}

func TestTheSignInFormOfAServiceBehindAPrefixIsThatServicesDoor(t *testing.T) {
	svcPass, _ := HashPassword("service pass")
	a, err := New(Options{DataDir: t.TempDir(), LookupRequest: byPath(nil, &manifest.Access{Password: svcPass})})
	if err != nil {
		t.Fatal(err)
	}
	h := a.Wrap(okHandler())
	if rec := do(h, req("GET", "/", "203.0.113.1:1")); rec.Code != 200 {
		t.Fatalf("the other service got %d", rec.Code)
	}
	// The form is posted to the house's own path, which is the root
	// service's: judged as the page it goes back to, it is the api's.
	form := url.Values{"password": {"service pass"}, "next": {"/api/x?y=1"}}
	r := httptest.NewRequest("POST", "http://p.example"+LoginPath, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := do(h, r)
	if rec.Code != http.StatusSeeOther || len(rec.Result().Cookies()) != 1 {
		t.Fatalf("login got %d with %d cookies", rec.Code, len(rec.Result().Cookies()))
	}
	r = req("GET", "/api/x", "203.0.113.1:1")
	r.AddCookie(rec.Result().Cookies()[0])
	if rec := do(h, r); rec.Code != 200 {
		t.Fatalf("with the session got %d", rec.Code)
	}
}

func TestWithoutItsKeyAServicesPasswordStillClosesItsNames(t *testing.T) {
	svcPass, _ := HashPassword("service pass")
	h := ClosedRequest(byPath(nil, &manifest.Access{Password: svcPass}))(okHandler())
	if rec := do(h, req("GET", "/", "203.0.113.1:1")); rec.Code != 200 {
		t.Fatalf("an open service got %d", rec.Code)
	}
	if rec := do(h, req("GET", "/api/x", "203.0.113.1:1")); rec.Code == 200 {
		t.Fatal("a service with a password was open without the session key")
	}
}
