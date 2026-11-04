package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dcimorra/the-middleware/internal/auth"
	"github.com/dcimorra/the-middleware/internal/sitetoken"
)

// What a copy sends through the mesh reaches the site door with the
// site's own token on it, never the copy's word, never another site's
// name, and nothing but the door's routes.
func TestACopysDoorCallCarriesTheSitesTokenAndNothingElse(t *testing.T) {
	tokens := sitetoken.New(t.TempDir())
	want, err := tokens.Token("ai.example")
	if err != nil {
		t.Fatal(err)
	}
	var seen *http.Request
	h := doorForCopies(tokens, "ai.example", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = r }))
	r := httptest.NewRequest("POST", "/api/v1/site/chat", nil)
	r.Header.Set("Authorization", "Bearer through-the-primary")
	r.Header.Set("X-Middleware-Site", "other.example")
	r.RemoteAddr = "10.77.0.2:41000" // the other node, through the mesh
	h.ServeHTTP(httptest.NewRecorder(), r)
	if seen == nil || seen.Header.Get("Authorization") != "Bearer "+want || seen.Header.Get("X-Middleware-Site") != "" || !auth.Direct(seen) {
		t.Fatalf("the door saw %v", seen)
	}
	seen = nil
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/sites", nil))
	if seen != nil || w.Code != 404 {
		t.Fatalf("a panel route went through: %d", w.Code)
	}
}
