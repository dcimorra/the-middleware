package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/dcimorra/the-middleware/internal/app"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/helper"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/netuse"
	"github.com/dcimorra/the-middleware/internal/pages"
	"github.com/dcimorra/the-middleware/internal/proxy"
)

// orderNetUse counts right behind the doors, outside everything that
// answers for a site: what the WAF refuses, a password page and an error
// page are bytes the site's name moved, and a site that is cut for
// having spent its budget is refused here, before anything is spent on
// the request. The ACME challenge is answered by the hook outside this
// one, so a cut site still renews its certificate.
const orderNetUse = orderInbound + 5

// theNetUse is the book of what each site moves over the network.
var theNetUse *netuse.Book

func init() {
	hookBuilders = append(hookBuilders, hookBuilder{Name: "netuse", Order: orderNetUse, Build: buildNetUse})
}

func buildNetUse(a *app.App) app.Hook {
	theNetUse = netuse.New(netuse.Options{
		Dir:    filepath.Join(dataDir(a), netuse.Dir),
		Limits: func(domain string) netuse.Limits { return netLimits(a, domain) },
		Notify: func(e netuse.Event) { a.Bus.Emit(netEvent(e)) },
	})
	return &netUseHook{a: a, helper: helper.Dial(""), last: map[string]helper.MeterCount{}}
}

// netLimits is a site's transfer budget now: its manifest's
// limits.network over the house's default.
func netLimits(a *app.App, domain string) netuse.Limits {
	sn, ok := a.Engine.Lookup(domain)
	if !ok || sn.Manifest == nil {
		return netuse.Limits{}
	}
	n := sn.Manifest.Limits.Over(a.CurrentSettings().Sites.DefaultLimits).Network
	if n == nil {
		return netuse.Limits{}
	}
	monthly, daily := n.Bytes()
	return netuse.Limits{Monthly: monthly, Daily: daily, WarnAt: n.WarnAt, Cut: n.Cut}
}

// netEvent is what the journal, the panel and the operator's phone are
// told about a site's budget.
func netEvent(e netuse.Event) events.Event {
	period := "monthly"
	until := "the month ends"
	if e.Period == "day" {
		period, until = "daily", "the day ends"
	}
	ev := events.Event{Subject: e.Domain, Actor: "engine", Data: map[string]any{"period": e.Period, "used": e.Used, "limit": e.Limit, "cut": e.Cut}}
	switch e.Kind {
	case "warn":
		ev.Type, ev.Level = "site.network.warn", "warn"
		ev.Message = fmt.Sprintf("%s has moved %d%% of its %s network budget (%s of %s)", e.Domain, e.Used*100/max(e.Limit, 1), period, netBytes(e.Used), netBytes(e.Limit))
	case "limit":
		ev.Type, ev.Level = "site.network.limit", "warn"
		ev.Message = fmt.Sprintf("%s reached its %s network budget (%s): it keeps serving, since limits.network.cut is off", e.Domain, period, netBytes(e.Limit))
		if e.Cut {
			ev.Level = "error"
			ev.Message = fmt.Sprintf("%s reached its %s network budget (%s) and is closed until %s or the budget is raised", e.Domain, period, netBytes(e.Limit), until)
		}
	default:
		ev.Type = "site.network.resumed"
		ev.Message = e.Domain + " is inside its network budget again and serves"
		ev.Data = nil
	}
	return ev
}

// netBytes writes a size for a person, in the units the manifest's sizes
// and the panel use (1 GB is 1024 MB).
func netBytes(n int64) string {
	f, u := float64(n), 0
	units := []string{"B", "KB", "MB", "GB", "TB"}
	for f >= 1024 && u < len(units)-1 {
		f /= 1024
		u++
	}
	if u == 0 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f %s", f, units[u])
}

type netUseHook struct {
	a      *app.App
	helper *helper.Client

	// The ports' meter, its goroutine's alone: what each port's rules
	// had counted at the last reading, when that was, and whether the
	// trouble reading them was already said.
	last   map[string]helper.MeterCount
	primed bool
	read   time.Time
	said   bool
}

func (h *netUseHook) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// siteOf's test, with the snapshot kept for the page of a cut
		// site: this is asked once per request of every site.
		sn, ok := h.a.Engine.Lookup(proxy.Host(r))
		if !ok || sn.Mode == manifest.ModeProject || manifest.IsProject(sn.Domain) {
			next.ServeHTTP(w, r)
			return
		}
		c := theNetUse.Counter(sn.Domain)
		c.In(requestHead(r))
		if r.Body != nil && r.Body != http.NoBody {
			r.Body = &meteredBody{ReadCloser: r.Body, c: c}
		}
		mw := &meteredWriter{ResponseWriter: w, c: c}
		if c.Cut() {
			pages.Render(mw, r, proxy.SiteNotice(sn, "overbudget"))
			return
		}
		next.ServeHTTP(mw, r)
		// A handler that wrote nothing still had a status line and its
		// headers sent for it.
		if !mw.hijacked {
			mw.head(http.StatusOK)
		}
	})
}

func (h *netUseHook) Run(ctx context.Context, _ *app.App) error {
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		n := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				theNetUse.Tick()
				if n++; n%60 == 0 {
					theNetUse.Flush()
				}
			}
		}
	}()
	// The ports apart: reading them asks the helper, which runs
	// iptables, and a slow answer must not stop the seconds above.
	go func() {
		t := time.NewTicker(meterEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				h.meterPorts(ctx)
			}
		}
	}()
	return nil
}

func (h *netUseHook) Stop() {
	theNetUse.Tick()
	theNetUse.Flush()
}

// meterEvery is how often the public ports' counters are read.
const meterEvery = 5 * time.Second

// meteredMark is written while the helper holds the chains that count,
// so a supervisor that starts with no public port left knows there is
// something to take away. A name no site can have.
func (h *netUseHook) meteredMark() string {
	return filepath.Join(dataDir(h.a), netuse.Dir, ".metered")
}

// meterPorts adds to each site what its public ports moved since the
// last reading: those bytes go from the internet to the service and
// never cross the house's proxy, and for a relay they are the bill.
func (h *netUseHook) meterPorts(ctx context.Context) {
	var rules []helper.MeterRule
	owner := map[string]string{}
	for _, sn := range h.a.Engine.Sites() {
		ports := sn.Manifest.PublicPorts()
		// What the owner has not approved is not open, and is not the
		// site's to be billed for.
		if len(ports) == 0 || thePorts == nil || len(thePorts.Pending(sn.Domain, sn.Manifest)) > 0 {
			continue
		}
		cut := theNetUse.Cut(sn.Domain)
		for _, p := range ports {
			key := meterKey(p.Proto, p.From, p.To)
			if owner[key] != "" {
				continue
			}
			owner[key] = sn.Domain
			rules = append(rules, helper.MeterRule{From: p.From, To: p.To, Proto: p.Proto, Cut: cut})
		}
	}
	_, had := os.Stat(h.meteredMark())
	if len(rules) == 0 && had != nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	res, err := h.helper.PortsMeter(cctx, rules)
	cancel()
	if err != nil {
		// Said once and again only after it worked: every five seconds
		// would bury the journal under one sentence.
		if !h.said && ctx.Err() == nil {
			h.said = true
			h.a.Bus.Emit(events.Event{Type: "network.unmetered", Actor: "engine", Level: "warn",
				Message: "the sites' public ports are not counted in their network use, and a site cut at its budget keeps them open: " + err.Error()})
		}
		return
	}
	h.said = false
	if len(rules) == 0 {
		_ = os.Remove(h.meteredMark())
		h.last, h.primed = map[string]helper.MeterCount{}, false
		return
	}
	if had != nil {
		if err := os.MkdirAll(filepath.Dir(h.meteredMark()), 0o755); err == nil {
			_ = os.WriteFile(h.meteredMark(), nil, 0o644)
		}
	}
	now := time.Now()
	span := now.Sub(h.read)
	h.read = now
	// The first reading of a process is where it starts from: the rules
	// may have counted for days under the process before this one, which
	// already added those bytes.
	first := !h.primed
	h.primed = true
	for _, c := range res.Counts {
		key := meterKey(c.Proto, c.From, c.To)
		was := h.last[key]
		in, out := c.In-was.In, c.Out-was.Out
		// Counters that went back were zeroed by someone else: what
		// they hold is what moved since.
		if in < 0 {
			in = c.In
		}
		if out < 0 {
			out = c.Out
		}
		if res.Reset {
			delete(h.last, key)
		} else {
			h.last[key] = c
		}
		if !first {
			theNetUse.AddPorts(owner[key], in, out, span)
		}
	}
}

func meterKey(proto string, from, to int) string {
	return fmt.Sprintf("%s %d-%d", proto, from, to)
}

// requestHead is the size of a request's line and headers as they came
// over HTTP/1.1. Over HTTP/2 they came compressed and this reads a
// little high; the body, which is nearly all of what a site receives, is
// counted as it is read.
func requestHead(r *http.Request) int64 {
	n := len(r.Method) + len(r.RequestURI) + len(r.Proto) + 4 + len("Host: ") + len(r.Host) + 2 + 2
	for k, vs := range r.Header {
		for _, v := range vs {
			n += len(k) + len(v) + 4
		}
	}
	return int64(n)
}

// meteredBody counts a request's body as the site reads it.
type meteredBody struct {
	io.ReadCloser
	c *netuse.Counter
}

func (b *meteredBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.c.In(int64(n))
	return n, err
}

// meteredWriter counts a response as it is written, headers included. It
// forwards Flush and Hijack and exposes Unwrap, as every writer in this
// chain must: the reverse proxy hijacks for WebSockets through it. A
// hijacked connection is counted from then on by its reads and writes.
type meteredWriter struct {
	http.ResponseWriter
	c        *netuse.Counter
	headed   bool
	hijacked bool
}

// head counts the status line and the headers, once: the ones the
// handler set, and some forty bytes for the Date and the length the
// server adds.
func (w *meteredWriter) head(code int) {
	if w.headed {
		return
	}
	w.headed = true
	n := len("HTTP/1.1 200 ") + len(http.StatusText(code)) + 2 + 2 + 40
	for k, vs := range w.Header() {
		for _, v := range vs {
			n += len(k) + len(v) + 4
		}
	}
	w.c.Out(int64(n))
}

func (w *meteredWriter) WriteHeader(code int) {
	// An informational answer (103 Early Hints) is followed by the real
	// one, whose headers are the ones worth counting.
	if code >= 200 || code == http.StatusSwitchingProtocols {
		w.head(code)
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *meteredWriter) Write(b []byte) (int, error) {
	w.head(http.StatusOK)
	n, err := w.ResponseWriter.Write(b)
	w.c.Out(int64(n))
	return n, err
}

func (w *meteredWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *meteredWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return nil, nil, err
	}
	w.hijacked = true
	return &meteredConn{Conn: conn, c: w.c}, rw, nil
}

func (w *meteredWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// meteredConn is a hijacked connection (a WebSocket), counted both ways
// for as long as it lives.
type meteredConn struct {
	net.Conn
	c *netuse.Counter
}

func (c *meteredConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.c.In(int64(n))
	return n, err
}

func (c *meteredConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.c.Out(int64(n))
	return n, err
}
