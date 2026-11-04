package nodes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A site's request crosses the mesh door as the site's: its own
// Authorization and Host reach the other side, the network's secret does
// not, the hop looks like the tunnel's connector, and without the secret
// the door does not open.
func TestASitesRequestCrossesTheDoorWithItsOwnHeaders(t *testing.T) {
	ctx := context.Background()
	port := freePort(t)
	a, b := newNode(t, "alpha", port), newNode(t, "beta", port)
	code, _, err := a.Invite(ctx, "198.51.100.1:51821")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Join(ctx, code, ""); err != nil {
		t.Fatal(err)
	}
	a.Gossip(ctx)
	var seen *http.Request
	b.ServeSites(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Clone(context.Background())
		who, _ := MemberFrom(r.Context())
		w.Header().Set("Cache-Control", "public, max-age=60")
		_, _ = w.Write([]byte("from beta for " + who.Name))
	}))
	bm, _ := a.Member(b.Self().ID)
	px := a.SiteProxy(bm, func(in *http.Request, out http.Header) { out.Set("CF-Connecting-IP", "203.0.113.9") }, nil)
	req := httptest.NewRequest("GET", "http://shop.example/cart?x=1", nil)
	req.Header.Set("Authorization", "Basic dmlzaXRvcg==")
	req.Header.Set(ClusterKeyHeader, "forged")
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req)
	if rec.Body.String() != "from beta for alpha" {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Values("Cache-Control"); len(got) != 1 || got[0] != "public, max-age=60" {
		t.Fatalf("Cache-Control %v: the door's own no-store was added to the site's", got)
	}
	if seen.Host != "shop.example" || seen.URL.RequestURI() != "/cart?x=1" || seen.Header.Get("Authorization") != "Basic dmlzaXRvcg==" {
		t.Fatalf("host %q uri %q auth %q", seen.Host, seen.URL.RequestURI(), seen.Header.Get("Authorization"))
	}
	if seen.Header.Get(ClusterKeyHeader) != "" || seen.Header.Get(ClusterHeader) != "" {
		t.Fatal("the cluster's headers reached the site")
	}
	if !strings.HasPrefix(seen.RemoteAddr, "127.0.0.1:") || seen.Header.Get("CF-Connecting-IP") != "203.0.113.9" {
		t.Fatalf("remote %q client %q", seen.RemoteAddr, seen.Header.Get("CF-Connecting-IP"))
	}
	// Without the secret: refused.
	raw, _ := http.NewRequest("GET", a.base(bm.IP)+"/", nil)
	raw.Host = "shop.example"
	raw.Header.Set(ClusterHeader, "1")
	raw.Header.Set(ClusterKeyHeader, "wrong")
	res, err := (&http.Client{Transport: a.transport()}).Do(raw)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a wrong secret got %d", res.StatusCode)
	}
}
