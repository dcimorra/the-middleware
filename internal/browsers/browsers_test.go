package browsers

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/shots"
)

// echoBrowser is a "browser" that answers every command with itself, on
// the same pipes a Chromium uses: what the relay does is tested without
// one, and with one below.
func echoBrowser(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "echo-browser")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexec cat <&3 >&4\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newPool(t *testing.T, dir, bin string, count int, clk *clock, bus *events.Bus) *Pool {
	t.Helper()
	p, err := New(Options{Dir: dir, Bus: bus, Binary: func() string { return bin }, Count: func() int { return count },
		Idle: func() time.Duration { return 15 * time.Minute }, Now: clk.now})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return p
}

func TestASiteTakesAFreeBrowserHoldsOneAndGivesItBack(t *testing.T) {
	clk := &clock{t: time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)}
	bus := events.New("")
	p := newPool(t, t.TempDir(), echoBrowser(t), 2, clk, bus)

	a, again, err := p.Take("a.test", "check the\nhome page", "agent")
	if err != nil || again || a.Slot != 1 || a.For != "check the home page" || a.Endpoint == "" {
		t.Fatalf("take: %+v %v %v", a, again, err)
	}
	// A second take is the one it holds, not another.
	a2, again, err := p.Take("a.test", "", "agent")
	if err != nil || !again || a2.Slot != 1 || a2.Endpoint != a.Endpoint || a2.For != "check the home page" {
		t.Fatalf("second take: %+v %v %v", a2, again, err)
	}
	b, _, err := p.Take("b.test", "", "agent")
	if err != nil || b.Slot != 2 {
		t.Fatalf("b take: %+v %v", b, err)
	}
	_, _, err = p.Take("c.test", "", "agent")
	var busy *BusyError
	if !errors.As(err, &busy) || !strings.Contains(err.Error(), "1 by a.test since 10:00 (check the home page)") || !strings.Contains(err.Error(), "2 by b.test") {
		t.Fatalf("third site: %v", err)
	}
	// Each sees who holds what, and only its own endpoint.
	for _, s := range p.List("c.test") {
		if s.Endpoint != "" || s.Free {
			t.Fatalf("c.test sees %+v", s)
		}
	}
	if _, err := p.Release("c.test", 1, "agent"); !errors.Is(err, ErrNotYours) {
		t.Fatalf("another site gave it back: %v", err)
	}
	if n, err := p.Release("a.test", 0, "agent"); err != nil || n != 1 {
		t.Fatalf("release: %d %v", n, err)
	}
	if _, err := p.Release("a.test", 0, "agent"); !errors.Is(err, ErrNotTaken) {
		t.Fatalf("second release: %v", err)
	}
	c, _, err := p.Take("c.test", "", "agent")
	if err != nil || c.Slot != 1 {
		t.Fatalf("c after a: %+v %v", c, err)
	}
	// The operator frees any.
	if _, err := p.Release("", 2, "agent"); err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, e := range bus.Recent() {
		if strings.HasPrefix(e.Type, "browser.") {
			types = append(types, e.Type+" "+e.Subject)
		}
	}
	want := "browser.taken a.test,browser.taken b.test,browser.released a.test,browser.taken c.test,browser.freed b.test"
	if strings.Join(types, ",") != want {
		t.Fatalf("events:\n got %s\nwant %s", strings.Join(types, ","), want)
	}
}

func TestNoBrowsersAndNoBinaryAreSaidAsSuch(t *testing.T) {
	clk := &clock{t: time.Now()}
	p := newPool(t, t.TempDir(), echoBrowser(t), 0, clk, nil)
	if _, _, err := p.Take("a.test", "", "agent"); !errors.Is(err, ErrNone) {
		t.Fatalf("none: %v", err)
	}
	q := newPool(t, t.TempDir(), "", 2, clk, nil)
	if _, _, err := q.Take("a.test", "", "agent"); !errors.Is(err, ErrNoBinary) {
		t.Fatalf("no binary: %v", err)
	}
}

func TestALeaseOutlivesARestartAndAForgottenOneIsTakenBack(t *testing.T) {
	clk := &clock{t: time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)}
	dir := t.TempDir()
	bin := echoBrowser(t)
	bus := events.New("")
	p, err := New(Options{Dir: dir, Bus: bus, Binary: func() string { return bin }, Count: func() int { return 1 }, Now: clk.now})
	if err != nil {
		t.Fatal(err)
	}
	a, _, err := p.Take("a.test", "", "agent")
	if err != nil {
		t.Fatal(err)
	}
	p.Close()

	q, err := New(Options{Dir: dir, Bus: bus, Binary: func() string { return bin }, Count: func() int { return 1 }, Now: clk.now})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	got, ok := q.Holding("a.test")
	if !ok || got.Endpoint != a.Endpoint {
		t.Fatalf("after the restart: %+v %v (was %s)", got, ok, a.Endpoint)
	}
	// Fourteen minutes is still the site's; past fifteen it is not.
	clk.add(14 * time.Minute)
	q.Expire()
	if _, ok := q.Holding("a.test"); !ok {
		t.Fatal("taken back before its time")
	}
	clk.add(2 * time.Minute)
	q.Expire()
	if _, ok := q.Holding("a.test"); ok {
		t.Fatal("a forgotten browser was not taken back")
	}
	rec := bus.Recent()
	last := rec[len(rec)-1]
	if last.Type != "browser.expired" || last.Subject != "a.test" || last.Level != "warn" || !strings.Contains(last.Message, "did not give it back") {
		t.Fatalf("event: %+v", last)
	}
}

func TestASmallerPoolKeepsTheTakenUntilTheyAreGivenBack(t *testing.T) {
	clk := &clock{t: time.Now()}
	count := 2
	var mu sync.Mutex
	p, err := New(Options{Dir: t.TempDir(), Binary: func() string { return "/bin/true" }, Count: func() int { mu.Lock(); defer mu.Unlock(); return count }, Now: clk.now})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.Take("a.test", "", "agent")
	p.Take("b.test", "", "agent")
	mu.Lock()
	count = 1
	mu.Unlock()
	list := p.List("")
	if len(list) != 2 || list[1].Slot != 2 || !list[1].Leaving || list[1].Site != "b.test" {
		t.Fatalf("list: %+v", list)
	}
	p.Release("b.test", 0, "agent")
	if list := p.List(""); len(list) != 1 {
		t.Fatalf("after: %+v", list)
	}
	if _, _, err := p.Take("c.test", "", "agent"); err == nil {
		t.Fatal("a slot above the pool was handed out")
	}
}

// The relay, crossed: a client on the websocket reaches the browser's
// pipe and back, a wrong token is refused, a second client replaces the
// first, and giving the browser back ends the process.
func TestTheRelayCarriesMessagesToTheHoldersBrowserOnly(t *testing.T) {
	clk := &clock{t: time.Now()}
	p := newPool(t, t.TempDir(), echoBrowser(t), 1, clk, nil)
	s, _, err := p.Take("a.test", "", "agent")
	if err != nil {
		t.Fatal(err)
	}
	bad := strings.Replace(s.Endpoint, s.Endpoint[len(s.Endpoint)-6:], "000000", 1)
	if _, err := dial(bad); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("wrong token: %v", err)
	}
	c1, err := dial(s.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	c1.send(t, `{"id":1,"method":"Browser.getVersion"}`)
	if got := c1.recv(t); got != `{"id":1,"method":"Browser.getVersion"}` {
		t.Fatalf("echo: %q", got)
	}
	// A large message crosses whole, in both directions.
	big := `{"id":2,"params":"` + strings.Repeat("x", 200000) + `"}`
	c1.send(t, big)
	if got := c1.recv(t); got != big {
		t.Fatalf("big echo: %d bytes", len(got))
	}
	if h, _ := p.Holding("a.test"); !h.Running || !h.Connected {
		t.Fatalf("holding: %+v", h)
	}
	c2, err := dial(s.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c1.read(); err == nil {
		t.Fatal("the first client was not closed when the second came")
	}
	c2.send(t, `{"id":3}`)
	if got := c2.recv(t); got != `{"id":3}` {
		t.Fatalf("second client: %q", got)
	}
	// json/version points at the websocket.
	res, err := http.Get(strings.Replace(s.Endpoint, "ws://", "http://", 1) + "/json/version")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		WS string `json:"webSocketDebuggerUrl"`
	}
	json.NewDecoder(res.Body).Decode(&v)
	res.Body.Close()
	if v.WS != s.Endpoint {
		t.Fatalf("json/version: %q", v.WS)
	}
	p.mu.Lock()
	pid := p.live[1].cmd.Process.Pid
	p.mu.Unlock()
	if _, err := p.Release("a.test", 0, "agent"); err != nil {
		t.Fatal(err)
	}
	if _, err := c2.read(); err == nil {
		t.Fatal("the client stayed connected to a browser given back")
	}
	if alive(pid) {
		t.Fatalf("browser %d still runs after it was given back", pid)
	}
	if _, err := dial(s.Endpoint); err == nil {
		t.Fatal("the endpoint still answers after the browser was given back")
	}
}

// With a real Chromium: a site's client opens its page through the pool
// and reads what the page says. This is the reason for the pool.
func TestARealBrowserOfThePoolOpensASitesPage(t *testing.T) {
	bin := shots.Find()
	if bin == "" {
		t.Skip("no headless Chromium on this machine")
	}
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<title>the site at work</title><p>hello")
	}))
	defer page.Close()
	clk := &clock{t: time.Now()}
	p := newPool(t, t.TempDir(), bin, 1, clk, nil)
	s, _, err := p.Take("a.test", "read its title", "agent")
	if err != nil {
		t.Fatal(err)
	}
	c, err := dial(s.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	call := func(id int, session, method string, params any) map[string]any {
		t.Helper()
		m := map[string]any{"id": id, "method": method, "params": params}
		if session != "" {
			m["sessionId"] = session
		}
		b, _ := json.Marshal(m)
		c.send(t, string(b))
		for {
			raw := c.recv(t)
			var got map[string]any
			json.Unmarshal([]byte(raw), &got)
			if got["id"] == float64(id) {
				if got["error"] != nil {
					t.Fatalf("%s: %v", method, got["error"])
				}
				res, _ := got["result"].(map[string]any)
				return res
			}
		}
	}
	tgt := call(1, "", "Target.createTarget", map[string]any{"url": "about:blank"})
	att := call(2, "", "Target.attachToTarget", map[string]any{"targetId": tgt["targetId"], "flatten": true})
	sess, _ := att["sessionId"].(string)
	call(3, sess, "Page.enable", map[string]any{})
	call(4, sess, "Page.navigate", map[string]any{"url": page.URL})
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		res := call(5, sess, "Runtime.evaluate", map[string]any{"expression": "document.title", "returnByValue": true})
		if r, _ := res["result"].(map[string]any); r != nil && r["value"] == "the site at work" {
			p.Release("a.test", 0, "agent")
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("the page's title was never read through the pool's browser")
}

func alive(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	// A zombie is gone for this purpose.
	f := strings.Fields(string(b))
	return len(f) > 2 && f[2] != "Z"
}

// A websocket client, the half the package does not need.
type client struct {
	c  net.Conn
	br *bufio.Reader
}

func dial(endpoint string) (*client, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	c, err := net.DialTimeout("tcp", u.Host, 5*time.Second)
	if err != nil {
		return nil, err
	}
	key := make([]byte, 16)
	rand.Read(key)
	fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", u.Path, u.Host, base64.StdEncoding.EncodeToString(key))
	br := bufio.NewReader(c)
	res, err := http.ReadResponse(br, nil)
	if err != nil {
		c.Close()
		return nil, err
	}
	if res.StatusCode != http.StatusSwitchingProtocols {
		c.Close()
		return nil, fmt.Errorf("handshake: %d", res.StatusCode)
	}
	return &client{c: c, br: br}, nil
}

func (c *client) send(t *testing.T, msg string) {
	t.Helper()
	p := []byte(msg)
	h := []byte{0x81}
	switch n := len(p); {
	case n < 126:
		h = append(h, 0x80|byte(n))
	case n <= 0xFFFF:
		h = append(h, 0x80|126, byte(n>>8), byte(n))
	default:
		h = append(h, 0x80|127)
		h = binary.BigEndian.AppendUint64(h, uint64(n))
	}
	mask := []byte{1, 2, 3, 4}
	h = append(h, mask...)
	out := make([]byte, len(p))
	for i := range p {
		out[i] = p[i] ^ mask[i%4]
	}
	if _, err := c.c.Write(append(h, out...)); err != nil {
		t.Fatal(err)
	}
}

func (c *client) read() (string, error) {
	_ = c.c.SetReadDeadline(time.Now().Add(15 * time.Second))
	var h [2]byte
	if _, err := io.ReadFull(c.br, h[:]); err != nil {
		return "", err
	}
	n := uint64(h[1] & 0x7F)
	switch n {
	case 126:
		var b [2]byte
		io.ReadFull(c.br, b[:])
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		io.ReadFull(c.br, b[:])
		n = binary.BigEndian.Uint64(b[:])
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(c.br, p); err != nil {
		return "", err
	}
	if h[0]&0x0F == opClose {
		return "", io.EOF
	}
	return string(p), nil
}

func (c *client) recv(t *testing.T) string {
	t.Helper()
	s, err := c.read()
	if err != nil {
		t.Fatal(err)
	}
	return s
}
