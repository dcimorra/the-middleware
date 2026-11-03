package traffic

import (
	"bufio"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)} }

func TestTheSnapshotCountsTheLastMinuteOnly(t *testing.T) {
	c := newClock()
	rec := New(Options{Now: c.now})
	rec.Record("a.example", 200, 100, 5*time.Millisecond)
	rec.Record("a.example", 503, 50, 300*time.Millisecond)
	rec.Record("b.example", 404, 10, time.Millisecond)
	c.advance(30 * time.Second)
	rec.Record("a.example", 200, 100, 5*time.Millisecond)
	snap := rec.Snapshot()
	if snap.Requests != 4 || snap.Bytes != 260 {
		t.Fatalf("requests %d bytes %d", snap.Requests, snap.Bytes)
	}
	a := snap.ByDomain["a.example"]
	if a.Requests != 3 || a.ByStatus["2xx"] != 2 || a.ByStatus["5xx"] != 1 {
		t.Fatalf("a.example: %+v", a)
	}
	if snap.ByStatus["4xx"] != 1 {
		t.Fatalf("by_status: %+v", snap.ByStatus)
	}
	if a.P50Ms > 20 || a.P95Ms < 200 {
		t.Fatalf("p50 %v p95 %v: the slow request must show in p95 and not in p50", a.P50Ms, a.P95Ms)
	}
	if snap.Seconds[29] != 3 || snap.Seconds[59] != 1 {
		t.Fatalf("seconds: %v", snap.Seconds)
	}
	// Sixty-one seconds later the first three are gone and the last is at
	// the edge.
	c.advance(31 * time.Second)
	snap = rec.Snapshot()
	if snap.Requests != 1 || snap.ByDomain["b.example"].Requests != 0 {
		t.Fatalf("after a minute: %+v", snap)
	}
	c.advance(time.Minute)
	rec.Prune()
	if len(rec.Domains()) != 0 {
		t.Fatalf("idle domains should be pruned, got %v", rec.Domains())
	}
}

func TestWrapRecordsStatusBytesAndDuration(t *testing.T) {
	c := newClock()
	rec := New(Options{Now: c.now})
	h := rec.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.advance(40 * time.Millisecond)
		switch r.URL.Path {
		case "/created":
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, "made")
		case "/silent":
			// Neither WriteHeader nor Write: an implicit 200 with nothing.
		default:
			io.WriteString(w, "hello")
		}
	}))
	for _, p := range []string{"/", "/created", "/silent"} {
		r := httptest.NewRequest("GET", "http://A.Example:8080"+p, nil)
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	snap := rec.Snapshot()
	d, ok := snap.ByDomain["a.example"]
	if !ok {
		t.Fatalf("host not lower-cased or port not stripped: %v", snap.ByDomain)
	}
	if d.Requests != 3 || d.Bytes != 9 || d.ByStatus["2xx"] != 3 {
		t.Fatalf("%+v", d)
	}
	if d.P50Ms < 40 {
		t.Fatalf("p50 %v, want at least the 40ms the handler took", d.P50Ms)
	}
}

func TestTheWrapperStillHijacksForWebSockets(t *testing.T) {
	rec := New(Options{})
	hijacked := make(chan bool, 1)
	h := rec.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			hijacked <- false
			return
		}
		conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\n\r\n"))
		conn.Close()
		hijacked <- true
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	io.WriteString(conn, "GET / HTTP/1.1\r\nHost: ws.example\r\n\r\n")
	line, _ := bufio.NewReader(conn).ReadString('\n')
	if !strings.Contains(line, "101") {
		t.Fatalf("got %q", line)
	}
	select {
	case ok := <-hijacked:
		if !ok {
			t.Fatal("hijack failed through the recording writer")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler never ran")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rec.Snapshot().ByDomain["ws.example"].ByStatus["1xx"] == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("a hijacked request should count as 1xx: %+v", rec.Snapshot().ByDomain)
}

func TestTheAccessLogTurnsOverEveryWeekAndNothingIsDeleted(t *testing.T) {
	dir := t.TempDir()
	c := newClock()
	rec := New(Options{LogDir: dir, Now: c.now, MaxLogSize: 400})
	h := rec.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	for i := 0; i < 12; i++ {
		r := httptest.NewRequest("GET", "http://log.example/page?i="+strings.Repeat("x", 20), nil)
		r.RemoteAddr = "203.0.113.9:1234"
		r.Header.Set("User-Agent", `curl "7"`)
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	rec.Flush()
	// A domain that could escape the log folder is never logged.
	r := httptest.NewRequest("GET", "http://x/", nil)
	r.Host = "../etc"
	h.ServeHTTP(httptest.NewRecorder(), r)
	rec.Close()
	base := filepath.Join(dir, "log.example", "access.log")
	data, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.SplitN(string(data), "\n", 2)[0]
	fields := strings.Fields(line)
	if len(fields) < 8 || fields[1] != "203.0.113.9" || fields[2] != "GET" || fields[4] != "200" || fields[5] != "2" {
		t.Fatalf("line %q", line)
	}
	if strings.Contains(line, `"7"`) {
		t.Fatal("quotes inside the agent must be neutralised so the line stays parseable")
	}
	// Twelve ~90-byte lines over a 400-byte cap split the week in parts,
	// every one kept and compressed, none of them a .log the guard reads.
	parts, _ := filepath.Glob(base + ".*")
	if len(parts) < 2 {
		t.Fatalf("twelve ~90-byte lines with a 400-byte cap should have split the week: %v", parts)
	}
	lines := strings.Count(string(data), "\n")
	for _, p := range parts {
		if !strings.HasPrefix(p, base+".2026-W43") || !strings.HasSuffix(p, ".gz") {
			t.Fatalf("%s: not an archive of the week, compressed", p)
		}
		lines += strings.Count(gunzip(t, p), "\n")
	}
	if lines != 12 {
		t.Fatalf("%d lines kept of 12: a rotation lost some", lines)
	}

	// A week later the log starts again, and last week's is an archive
	// under last week's name, after a restart too.
	c.advance(7 * 24 * time.Hour)
	rec = New(Options{LogDir: dir, Now: c.now, MaxLogSize: 1 << 20})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec.Run(ctx)
	r = httptest.NewRequest("GET", "http://log.example/next-week", nil)
	rec.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})).ServeHTTP(httptest.NewRecorder(), r)
	rec.Close()
	if data, _ := os.ReadFile(base); strings.Count(string(data), "\n") != 1 || !strings.Contains(string(data), "/next-week") {
		t.Fatalf("the new week's log: %q", data)
	}
	after, _ := filepath.Glob(base + ".*")
	if len(after) != len(parts)+1 {
		t.Fatalf("archives before %v, after %v: last week's was not kept apart", parts, after)
	}
	for _, p := range after {
		if !strings.HasSuffix(p, ".gz") {
			t.Fatalf("%s was left uncompressed", p)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "etc")); err == nil {
		t.Fatal("a hostile Host header wrote outside the log folder")
	}
}

func TestAFloodOfMadeUpNamesOpensNoFileAndNoFolder(t *testing.T) {
	dir := t.TempDir()
	c := newClock()
	sites := map[string]string{"shop.example": "shop.example", "www.shop.example": "shop.example", "alias.example": "shop.example"}
	rec := New(Options{LogDir: dir, Now: c.now, Site: func(r *http.Request) (string, bool) {
		d, ok := sites[strings.ToLower(r.Host)]
		return d, ok
	}})
	h := rec.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	visit := func(host string) {
		r := httptest.NewRequest("GET", "http://x/", nil)
		r.Host = host
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	for i := 0; i < 5000; i++ {
		visit(fmt.Sprintf("scan-%d.example", i))
	}
	visit("www.shop.example")
	visit("alias.example")
	visit("shop.example")
	visit(`evil "name" with spaces`)

	if n := len(rec.logs); n != 2 {
		t.Fatalf("%d access logs open after a flood of names, want 2 (the site's and the shared one)", n)
	}
	ents, _ := os.ReadDir(dir)
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	if len(names) != 2 || names[0] != "access.log" || names[1] != "shop.example" {
		t.Fatalf("data/logs holds %d entries (%v), want the shared log and the site's folder", len(names), names[:min(len(names), 5)])
	}
	// maxRings, the shared "-" past it and the site.
	if n := len(rec.rings); n > maxRings+2 {
		t.Fatalf("%d rings for one minute of made-up names", n)
	}
	// The site counts its aliases and its www. as itself.
	if got := rec.Snapshot().ByDomain["shop.example"].Requests; got != 3 {
		t.Fatalf("shop.example counted %d requests, want 3 with its aliases", got)
	}
	rec.Flush()
	site, _ := os.ReadFile(filepath.Join(dir, "shop.example", "access.log"))
	if strings.Count(string(site), "\n") != 3 {
		t.Fatalf("the site's log: %q", site)
	}
	shared, _ := os.ReadFile(filepath.Join(dir, "access.log"))
	lines := strings.Split(strings.TrimSpace(string(shared)), "\n")
	if len(lines) != 5001 || strings.Fields(lines[0])[1] != "scan-0.example" {
		t.Fatalf("the shared log has %d lines, the first %q", len(lines), lines[0])
	}
	if f := strings.Fields(lines[5000]); f[1] != "evil__name__with_spaces" || f[3] != "GET" {
		t.Fatalf("a name with spaces and quotes shifted the line: %q", lines[5000])
	}

	// Logs nothing writes to are closed, and open again when needed.
	c.advance(logIdle + time.Minute)
	rec.Prune()
	if len(rec.logs) != 0 {
		t.Fatalf("%d idle logs still open", len(rec.logs))
	}
	visit("shop.example")
	rec.Close()
	site, _ = os.ReadFile(filepath.Join(dir, "shop.example", "access.log"))
	if strings.Count(string(site), "\n") != 4 {
		t.Fatalf("the log did not reopen: %q", site)
	}
}

func TestRunFlushesInTheBackground(t *testing.T) {
	dir := t.TempDir()
	rec := New(Options{LogDir: dir})
	ctx, cancel := context.WithCancel(context.Background())
	rec.Run(ctx)
	h := rec.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://bg.example/", nil))
	path := filepath.Join(dir, "bg.example", "access.log")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st, err := os.Stat(path); err == nil && st.Size() > 0 {
			cancel()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	t.Fatal("the background flush never wrote the line")
}

func gunzip(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return string(b)
}

// A stop in the middle of compressing leaves the week in text; the next
// start compresses it and keeps every line.
func TestAnArchiveLeftInTextIsCompressedAtTheNextStart(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "a.example"), 0o755)
	left := filepath.Join(dir, "a.example", "access.log.2026-W40")
	os.WriteFile(left, []byte("one\ntwo\n"), 0o644)
	os.WriteFile(left+".gz.tmp", []byte("half"), 0o644)
	rec := New(Options{LogDir: dir})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec.Run(ctx)
	rec.Close()
	if _, err := os.Stat(left); err == nil {
		t.Fatal("the text was not compressed")
	}
	if got := gunzip(t, left+".gz"); got != "one\ntwo\n" {
		t.Fatalf("compressed %q", got)
	}
}
