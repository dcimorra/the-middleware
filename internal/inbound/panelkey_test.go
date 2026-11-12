package inbound_test

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/dcimorra/the-middleware/internal/inbound"
)

// The panel behind its key, crossed for real: through the public 443 a
// browser without the key gets the page of a name nobody serves; with it
// once in the address, a cookie and
// the address without the key; and the CLI's `panel key off`, written to
// the file alone, opens it again with the process running.
func TestThePanelBehindItsKeyAnswersOnlyWhoBringsIt(t *testing.T) {
	h := newHouse(t, "site.test")
	key, err := inbound.SetPanelKey(h.root, true)
	if err != nil {
		t.Fatal(err)
	}
	c := h.client(false)
	jar, _ := cookiejar.New(nil)
	c.Jar = jar

	resp, hidden := get(t, c, "https://panel.test/")
	if resp.StatusCode != http.StatusNotFound || strings.Contains(hidden, "site=") {
		t.Fatalf("without the key: %d %q", resp.StatusCode, hidden)
	}
	if resp, _ := get(t, c, "https://panel.test/?access-token=wrong"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a wrong key: %d", resp.StatusCode)
	}
	// Other names are not touched by it.
	if resp, body := get(t, c, "https://site.test/"); resp.StatusCode != 200 || !strings.Contains(body, "site=site.test") {
		t.Fatalf("a site: %d %q", resp.StatusCode, body)
	}

	resp, _ = get(t, c, "https://panel.test/sites?tab=x&"+inbound.PanelKeyParam+"="+url.QueryEscape(key))
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/sites?tab=x" {
		t.Fatalf("with the key: %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	var ck *http.Cookie
	for _, x := range resp.Cookies() {
		if x.Name == inbound.PanelKeyCookie {
			ck = x
		}
	}
	if ck == nil || !ck.HttpOnly || !ck.Secure {
		t.Fatalf("cookie: %+v", ck)
	}
	resp, body := get(t, c, "https://panel.test/sites?tab=x")
	if resp.StatusCode != 200 || !strings.Contains(body, "site=panel.test") {
		t.Fatalf("with the cookie: %d %q", resp.StatusCode, body)
	}

	// A new key ends the old one's cookie.
	if _, err := inbound.SetPanelKey(h.root, true); err != nil {
		t.Fatal(err)
	}
	if resp, _ := get(t, c, "https://panel.test/"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("an old key's cookie after a new key: %d", resp.StatusCode)
	}
	// The way back from the shell.
	if _, err := inbound.SetPanelKey(h.root, false); err != nil {
		t.Fatal(err)
	}
	if resp, body := get(t, h.client(false), "https://panel.test/"); resp.StatusCode != 200 || !strings.Contains(body, "site=panel.test") {
		t.Fatalf("after off: %d %q", resp.StatusCode, body)
	}
}

// What no edge relayed still reaches the panel with the key on: the CLI,
// a site calling its door and an SSH tunnel come from loopback with no
// proxy header. A tunnel connector's request says whom it relays for and
// is the internet's.
func TestThePanelKeyLeavesTheMachineItself(t *testing.T) {
	h := newHouse(t, "site.test")
	if _, err := h.hook.SetPanelKey(true); err != nil {
		t.Fatal(err)
	}
	call := func(headers ...string) (int, string) {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "http://panel.test/", nil)
		r.RemoteAddr = "127.0.0.1:5000"
		for _, kv := range headers {
			k, v, _ := strings.Cut(kv, ": ")
			r.Header.Set(k, v)
		}
		h.hook.Handler.ServeHTTP(rec, r)
		body, _ := io.ReadAll(rec.Body)
		return rec.Code, string(body)
	}
	if code, _ := call(); code != 200 {
		t.Fatalf("from the machine itself: %d", code)
	}
	code, hidden := call("Cf-Connecting-Ip: 203.0.113.9")
	if code != http.StatusNotFound {
		t.Fatalf("through the tunnel: %d", code)
	}
	// The very page the panel kept inside the VPN gives outside it.
	if _, err := h.hook.SetPanelKey(false); err != nil {
		t.Fatal(err)
	}
	h.hook.PanelVPNOnly = func() bool { return true }
	if code, vpn := call("Cf-Connecting-Ip: 203.0.113.9"); code != http.StatusNotFound || vpn != hidden {
		t.Fatalf("not the VPN-only page: %d\n%s\n---\n%s", code, vpn, hidden)
	}
	h.hook.PanelVPNOnly = nil
	if _, err := h.hook.SetPanelKey(true); err != nil {
		t.Fatal(err)
	}
	if !h.hook.PanelKeyState().On() {
		t.Fatal("state says off")
	}
}
