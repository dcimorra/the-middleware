package inbound

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dcimorra/the-middleware/internal/acme"
	"github.com/dcimorra/the-middleware/internal/app"
	"github.com/dcimorra/the-middleware/internal/certs"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/localdns"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/pages"
	"github.com/dcimorra/the-middleware/internal/proxy"
)

// Hook is the inbound side of the app: it wraps the request chain and runs
// the ports door's listeners. One per process.
type Hook struct {
	// Root is the installation root; the state file is data/inbound.json.
	Root string
	// Store serves and obtains certificates for the ports door.
	Store *certs.Store
	// Solver answers HTTP-01 on every door.
	Solver *acme.HTTP01
	// PanelDomain always answers, whichever door it comes in by. Run takes
	// it from the app's settings when empty.
	PanelDomain string
	// PanelDomainNow, when set, is asked instead of PanelDomain: the
	// panel's name read live, as the proxy holds it.
	PanelDomainNow func() string
	// Sites lists the domains this installation serves. Run plugs the
	// engine in when nil.
	Sites func() []string
	// Bus receives inbound.* events. Run takes the app's when nil.
	Bus *events.Bus
	// Handler is what the 443 listener serves: the app's full chain. Run
	// takes the app's when nil.
	Handler http.Handler
	// Tick is how often the listeners are reconciled with the state file.
	// Default 2 seconds.
	Tick time.Duration
	// VPNAddress is the server's address inside the VPN, ok false while
	// the VPN is down (vpn.go).
	VPNAddress func() (netip.Addr, bool)
	// VPNOnly says whether host is a VPN-only site, and for which devices.
	VPNOnly func(host string) (devices []string, ok bool)
	// PanelVPNOnly says the panel answers only through the VPN door (and
	// the machine's own loopback, for SSH and the CLI).
	PanelVPNOnly func() bool
	// VPNPaths, when set and not empty for host, are the only paths of it
	// kept inside the VPN; the rest of the site is public.
	VPNPaths func(host string) []string
	// DeviceOf names the VPN peer with an address.
	DeviceOf func(ip netip.Addr) string
	// Elsewhere says another node of the network answers for host; Relay
	// passes it a VPN connection for host (relay.go). Both nil outside a
	// network.
	Elsewhere func(host string) bool
	Relay     func(c net.Conn, host string, pre []byte, device string) error
	// ForceHTTPS says host is a name its site keeps to https (the
	// manifest's force_https). Nil keeps every name as it comes.
	ForceHTTPS func(host string) bool
	// VPNPorts overrides the VPN door's ports (tests).
	VPNPorts VPNPorts

	file  *stateFile
	mu    sync.Mutex
	http  *listener
	https *listener
	// The local door: its plain listener and its name server.
	local        *listener
	localDNS     *localdns.Server
	localDNSWant string
	// lastErr dedupes inbound.failed: a port held by something else would
	// otherwise emit on every tick, which is noise and not information.
	lastErr      string
	lastLocalErr string
	// The VPN door (vpn.go).
	vpnHTTP, vpnHTTPS *listener
	vpnDNS            *localdns.Server
	vpnWant           netip.Addr
	lastVPNErr        string
	// vpnCovered: the ports door listens on every address, the VPN's
	// included, so the VPN door has no 80/443 of its own (two listeners
	// on one port cannot coexist: whichever opened second failed, and
	// after a restart that was the public one). Its connections are told
	// apart by the address they arrived at; vpnLocal is that address,
	// read by every new connection.
	vpnCovered bool
	vpnLocal   atomic.Pointer[netip.Addr]
	// relayIn serves what other nodes relay here (relay.go).
	relayIn *chanListener
}

// IdleTimeout is how long a keep-alive connection through a door may wait
// for its next request before it is closed. With none, an idle connection
// was held for good, and anyone on the internet could open enough of them
// to run the process out of descriptors. Nothing times the whole read or
// write, on purpose: that would cut long uploads, websockets and event
// streams. A variable so a test need not wait it out.
var IdleTimeout = 120 * time.Second

type listener struct {
	want string // address from the state file
	ln   net.Listener
	srv  *http.Server
}

// New builds the hook. Store and solver are shared with whatever else
// needs them (the API for renewals, the CLI for status).
func New(root string, store *certs.Store, solver *acme.HTTP01) *Hook {
	return &Hook{
		Root:   root,
		Store:  store,
		Solver: solver,
		Tick:   2 * time.Second,
		file:   &stateFile{path: Path(root)},
	}
}

// State is the current content of the file.
func (h *Hook) State() State { return h.file.get() }

type ctxKey struct{}

// DoorOf is doorOf for other packages: "tunnel", "ports", "local", "vpn".
func DoorOf(r *http.Request) string { return string(doorOf(r)) }

// doorOf tells which listener a request came in by. The ports servers tag
// their connections; everything else is the loopback listener, which is
// the tunnel door.
func doorOf(r *http.Request) Door {
	if d, ok := r.Context().Value(ctxKey{}).(Door); ok {
		return d
	}
	return Tunnel
}

func (h *Hook) panelDomain() string {
	if h.PanelDomainNow != nil {
		return h.PanelDomainNow()
	}
	return h.PanelDomain
}

func (h *Hook) isPanel(host string) bool {
	p := h.panelDomain()
	return host == "localhost" || host == "127.0.0.1" || host == "::1" || (p != "" && host == p)
}

// Wrap implements app.Hook. Order matters and is the point of the hook:
// the challenge first, because the CA must be answered even through a
// closed door; then the door's state; then the scheme the request really
// arrived with, which a site behind the proxy uses for redirects.
func (h *Hook) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, acme.ChallengePath) {
			h.Solver.ServeHTTP(w, r)
			return
		}
		door := doorOf(r)
		host := proxy.Host(r)
		if !h.vpnAllows(r, host, door) {
			// The same page as a name nobody serves: through any other
			// door, a VPN-only site does not exist, and saying "closed"
			// would say it does. No header changes that.
			pages.Write(w, r, "unknown", http.StatusNotFound)
			return
		}
		if h.isPanel(host) && !h.panelAllows(r, door) {
			pages.Write(w, r, "unknown", http.StatusNotFound)
			return
		}
		if !h.isPanel(host) {
			st := h.file.get()
			if (door == Tunnel && !st.Tunnel.Open) || (door == Ports && !st.Ports.Open) || (door == Local && !st.Local.Open) {
				pages.Write(w, r, "doorclosed", http.StatusServiceUnavailable)
				return
			}
		}
		if door == Ports {
			// Ours to say: a client on the public port cannot have been
			// through an edge that set it.
			if r.TLS != nil {
				r.Header.Set("X-Forwarded-Proto", "https")
			} else {
				r.Header.Set("X-Forwarded-Proto", "http")
			}
		} else if door == VPN {
			// Another node's name over plain HTTP: the relay goes by the
			// name in the TLS handshake, so the device is sent to https.
			if r.TLS == nil && h.elsewhere(host) {
				http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusFound)
				return
			}
			if r.TLS != nil {
				r.Header.Set("X-Forwarded-Proto", "https")
			} else {
				r.Header.Set("X-Forwarded-Proto", "http")
			}
		} else if door == Local {
			// The bench is plain HTTP and says so; a site that insists on
			// https would redirect the developer to a door that is not
			// there.
			r.Header.Set("X-Forwarded-Proto", "http")
		} else if r.Header.Get("X-Forwarded-Proto") == "" {
			// A tunnel sets it; a plain loopback client did not, and the
			// hop really was http.
			r.Header.Set("X-Forwarded-Proto", "http")
		}
		if door == Tunnel && h.upgrade(w, r, host) {
			return
		}
		next.ServeHTTP(w, r)
	})
}

// upgrade sends a plain-http request for a name kept to https to the same
// address on https, and says whether it did. Here, before every other
// hook, and not in the proxy: the cache, the access door and the attack
// page all answer before the proxy is asked, and a password typed into the
// door's form over http would cross the internet in the clear.
//
// Only the tunnel door: port 80 of the ports door already sends everything
// to https (serveHTTP); the bench is plain http only, and sending it to
// https sends the developer nowhere; inside the VPN plain http is already
// encrypted and a VPN-only name may have no public certificate. Through
// the tunnel Cloudflare serves http:// unless told otherwise, and
// cloudflared says which it was in X-Forwarded-Proto. A plain loopback
// client that names the site (another site calling it by name) has no
// header and is sent to https too, as any client would be.
func (h *Hook) upgrade(w http.ResponseWriter, r *http.Request, host string) bool {
	if h.ForceHTTPS == nil || host == "" || h.isPanel(host) {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https") || !h.ForceHTTPS(host) {
		return false
	}
	code := http.StatusPermanentRedirect
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		code = http.StatusMovedPermanently
	}
	http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), code)
	return true
}

// Run implements app.Hook: fills what the app knows, opens the ports door
// if the file says so, and keeps the listeners equal to the file for the
// life of the process. The first reconcile is synchronous so a caller can
// read Addrs right after.
func (h *Hook) Run(ctx context.Context, a *app.App) error {
	if a != nil {
		if h.Handler == nil {
			h.Handler = a.Handler
		}
		if h.PanelDomain == "" {
			h.PanelDomain = a.CurrentSettings().PanelDomain
		}
		if h.Bus == nil {
			h.Bus = a.Bus
		}
		if h.Sites == nil && a.Engine != nil {
			eng := a.Engine
			h.Sites = func() []string {
				hosts := eng.Hosts()
				out := make([]string, 0, len(hosts))
				for h := range hosts {
					out = append(out, h)
				}
				return out
			}
		}
	}
	if h.Handler == nil || h.Store == nil || h.Solver == nil {
		return errors.New("inbound: hook needs a handler, a store and a solver")
	}
	h.reconcile()
	go h.Store.Run(ctx)
	go func() {
		t := time.NewTicker(h.Tick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				h.mu.Lock()
				h.closeLocked()
				h.mu.Unlock()
				return
			case <-t.C:
				h.reconcile()
			}
		}
	}()
	return nil
}

// LocalAddrs returns where the local door listens (http, dns), empty
// when closed.
func (h *Hook) LocalAddrs() (httpAddr, dnsAddr string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.local != nil {
		httpAddr = h.local.ln.Addr().String()
	}
	if h.localDNS != nil {
		dnsAddr = h.localDNS.Addr()
	}
	return
}

// Addrs returns where the ports door is listening, empty when closed.
func (h *Hook) Addrs() (httpAddr, httpsAddr string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.http != nil {
		httpAddr = h.http.ln.Addr().String()
	}
	if h.https != nil {
		httpsAddr = h.https.ln.Addr().String()
	}
	return
}

func (h *Hook) reconcile() {
	st := h.file.get()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reconcileLocalLocked(st)
	// The VPN's 80/443 go first when the ports door will cover them, and
	// after it otherwise: never two listeners on the same port.
	covered := st.Ports.Open && wildcard(st.Ports.HTTP) && wildcard(st.Ports.HTTPS)
	if covered {
		h.reconcileVPNLocked(true)
	} else {
		defer h.reconcileVPNLocked(false)
	}
	if !st.Ports.Open {
		if h.http != nil || h.https != nil {
			h.closeLocked()
			h.emit("engine", "inbound.stopped", string(Ports), "", "ports door closed", nil)
		}
		return
	}
	if h.http != nil && h.https != nil && h.http.want == st.Ports.HTTP && h.https.want == st.Ports.HTTPS {
		return
	}
	h.closeLocked()
	if err := h.openLocked(st); err != nil {
		h.closeLocked()
		if err.Error() != h.lastErr {
			h.lastErr = err.Error()
			h.emit("engine", "inbound.failed", string(Ports), "error", err.Error(), map[string]any{"http": st.Ports.HTTP, "https": st.Ports.HTTPS})
		}
		return
	}
	h.lastErr = ""
	h.emit("engine", "inbound.listening", string(Ports), "", "ports door listening", map[string]any{
		"http": h.http.ln.Addr().String(), "https": h.https.ln.Addr().String(),
	})
}

func (h *Hook) openLocked(st State) error {
	tag := func(ctx context.Context, c net.Conn) context.Context {
		// A connection to the VPN's address came through the tunnel.
		if a := h.vpnLocal.Load(); a != nil {
			if ap, err := netip.ParseAddrPort(c.LocalAddr().String()); err == nil && ap.Addr().Unmap() == *a {
				return context.WithValue(ctx, ctxKey{}, VPN)
			}
		}
		return context.WithValue(ctx, ctxKey{}, Ports)
	}
	ln, err := net.Listen("tcp", st.Ports.HTTP)
	if err != nil {
		return fmt.Errorf("inbound: http %s: %w", st.Ports.HTTP, err)
	}
	h.http = &listener{want: st.Ports.HTTP, ln: ln, srv: &http.Server{
		Handler:           http.HandlerFunc(h.serveHTTP),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       IdleTimeout,
		ConnContext:       tag,
	}}
	go h.http.srv.Serve(ln)

	ln, err = net.Listen("tcp", st.Ports.HTTPS)
	if err != nil {
		return fmt.Errorf("inbound: https %s: %w", st.Ports.HTTPS, err)
	}
	h.https = &listener{want: st.Ports.HTTPS, ln: ln, srv: &http.Server{
		Handler:           http.HandlerFunc(h.serveHTTPS),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       IdleTimeout,
		ConnContext:       tag,
		TLSConfig: &tls.Config{
			GetCertificate: h.Store.GetCertificate,
			MinVersion:     tls.VersionTLS12,
			// h2 first so browsers pick it; the ACME protocol last so a
			// CA probing TLS-ALPN-01 is the only one that lands on it.
			NextProtos: []string{"h2", "http/1.1", acme.ALPNProto},
		},
	}}
	// ServeTLS with no files uses TLSConfig.GetCertificate and turns on
	// HTTP/2; tls.NewListener by hand would not. Under it, the VPN's
	// connections for another node's names are taken out and relayed.
	go h.https.srv.ServeTLS(h.relayed(ln, func(c net.Conn) bool {
		a := h.vpnLocal.Load()
		ap, err := netip.ParseAddrPort(c.LocalAddr().String())
		return a != nil && err == nil && ap.Addr().Unmap() == *a
	}), "", "")
	return nil
}

// reconcileLocalLocked keeps the local door's two sockets equal to the
// file, the same way the ports door is kept: close what differs, open
// what the file says, report once when a port is taken.
func (h *Hook) reconcileLocalLocked(st State) {
	if !st.Local.Open {
		if h.local != nil || h.localDNS != nil {
			h.closeLocalLocked()
			h.emit("engine", "inbound.stopped", string(Local), "", "local door closed", nil)
		}
		return
	}
	if h.local != nil && h.localDNS != nil && h.local.want == st.Local.HTTP && h.localDNSWant == st.Local.DNS {
		return
	}
	h.closeLocalLocked()
	if err := h.openLocalLocked(st); err != nil {
		h.closeLocalLocked()
		if err.Error() != h.lastLocalErr {
			h.lastLocalErr = err.Error()
			h.emit("engine", "inbound.failed", string(Local), "error", err.Error(), map[string]any{"http": st.Local.HTTP, "dns": st.Local.DNS})
		}
		return
	}
	h.lastLocalErr = ""
	h.emit("engine", "inbound.listening", string(Local), "", "local door listening", map[string]any{
		"http": h.local.ln.Addr().String(), "dns": h.localDNS.Addr(),
	})
}

func (h *Hook) openLocalLocked(st State) error {
	tag := func(ctx context.Context, _ net.Conn) context.Context {
		return context.WithValue(ctx, ctxKey{}, Local)
	}
	ln, err := net.Listen("tcp", st.Local.HTTP)
	if err != nil {
		return fmt.Errorf("inbound: local http %s: %w", st.Local.HTTP, err)
	}
	// The full chain, straight: no certificate check, no redirect. The
	// challenge is answered here too, in case a CA is pointed at this
	// machine on purpose (pebble in a test).
	h.local = &listener{want: st.Local.HTTP, ln: ln, srv: &http.Server{
		Handler:           h.Handler,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       IdleTimeout,
		ConnContext:       tag,
	}}
	go h.local.srv.Serve(ln)

	dns := &localdns.Server{Serves: h.knownSite}
	if err := dns.Listen(context.Background(), st.Local.DNS); err != nil {
		return fmt.Errorf("inbound: local dns %s: %w", st.Local.DNS, err)
	}
	h.localDNS, h.localDNSWant = dns, st.Local.DNS
	return nil
}

func (h *Hook) closeLocalLocked() {
	if h.local != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = h.local.srv.Shutdown(ctx)
		cancel()
		_ = h.local.ln.Close()
		h.local = nil
	}
	if h.localDNS != nil {
		h.localDNS.Close()
		h.localDNS = nil
	}
}

// closeLocked stops both servers, giving in-flight responses a moment.
// The listeners close at once so a reopen on the same address does not
// race the old socket.
func (h *Hook) closeLocked() {
	for _, l := range []*listener{h.http, h.https} {
		if l == nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = l.srv.Shutdown(ctx)
		cancel()
		_ = l.ln.Close()
	}
	h.http, h.https = nil, nil
	h.closeLocalLocked()
}

// serveHTTP is port 80: the challenge, then a redirect. Everything else
// over plain HTTP is a mistake the redirect corrects, including the
// panel.
func (h *Hook) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if doorOf(r) == VPN {
		// Inside the tunnel plain HTTP is already encrypted: served as is.
		h.Handler.ServeHTTP(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, acme.ChallengePath) {
		h.Solver.ServeHTTP(w, r)
		return
	}
	host := proxy.Host(r)
	if host == "" {
		http.Error(w, "missing host", http.StatusBadRequest)
		return
	}
	// A non-standard https port (Docker publishing 8443) has to be in the
	// redirect or the browser goes to 443 and finds nothing.
	st := h.file.get()
	if _, port, err := net.SplitHostPort(st.Ports.HTTPS); err == nil && port != "443" && port != "0" {
		host = net.JoinHostPort(host, port)
	}
	code := http.StatusPermanentRedirect
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		code = http.StatusMovedPermanently
	}
	http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), code)
}

// serveHTTPS is port 443. A name without a certificate reached this far
// on the fallback certificate, and the honest answer is a page that says
// so; the chain behind would serve the site under a certificate no
// browser trusts, which is worse than a clear 503.
func (h *Hook) serveHTTPS(w http.ResponseWriter, r *http.Request) {
	if doorOf(r) == VPN {
		h.Handler.ServeHTTP(w, r)
		return
	}
	host := proxy.Host(r)
	if !h.Store.Has(host) {
		pages.Write(w, r, "nocert", http.StatusServiceUnavailable)
		return
	}
	h.Handler.ServeHTTP(w, r)
}

func (h *Hook) emit(actor, typ, subject, level, msg string, data map[string]any) {
	if h.Bus == nil {
		return
	}
	h.Bus.Emit(events.Event{Type: typ, Subject: subject, Actor: actor, Level: level, Message: msg, Data: data})
}

// SetDoor opens or closes a door, persists it and acts on it at once.
func (h *Hook) SetDoor(door Door, open bool) error {
	st := h.file.get()
	switch door {
	case Tunnel:
		st.Tunnel.Open = open
	case Ports:
		st.Ports.Open = open
	case Local:
		st.Local.Open = open
	default:
		return fmt.Errorf("inbound: unknown door %q", door)
	}
	if err := h.file.set(st); err != nil {
		return err
	}
	typ := "inbound.closed"
	if open {
		typ = "inbound.opened"
	}
	h.emit("operator", typ, string(door), "", "", nil)
	h.reconcile()
	return nil
}

// SetLocal changes the local door's addresses and open flag, persists
// and acts on it at once. Empty addresses keep what the file has.
func (h *Hook) SetLocal(open bool, httpAddr, dnsAddr string) error {
	st := h.file.get()
	was := st.Local.Open
	st.Local.Open = open
	if httpAddr != "" {
		st.Local.HTTP = httpAddr
	}
	if dnsAddr != "" {
		st.Local.DNS = dnsAddr
	}
	if err := h.file.set(st); err != nil {
		return err
	}
	if was != open {
		typ := "inbound.closed"
		if open {
			typ = "inbound.opened"
		}
		h.emit("operator", typ, string(Local), "", "", nil)
	}
	h.reconcile()
	return nil
}

// Status is what GET /inbound answers, minus the DNS and tunnel-route
// columns the cloudflare module fills in.
type Status struct {
	Doors   DoorsStatus    `json:"doors"`
	Domains []DomainStatus `json:"domains"`
}

// DoorsStatus is both doors. Listening and Error are what the file cannot
// say: whether the ports door is really up, and why not.
type DoorsStatus struct {
	Tunnel struct {
		Open bool `json:"open"`
	} `json:"tunnel"`
	Ports struct {
		Open      bool   `json:"open"`
		HTTP      string `json:"http"`
		HTTPS     string `json:"https"`
		Listening bool   `json:"listening"`
		Error     string `json:"error,omitempty"`
	} `json:"ports"`
	Local struct {
		Open      bool   `json:"open"`
		HTTP      string `json:"http"`
		DNS       string `json:"dns"`
		Listening bool   `json:"listening"`
		Error     string `json:"error,omitempty"`
		// Help is the resolver snippet for this door's dns address.
		Help string `json:"help"`
	} `json:"local"`
}

// DomainStatus is one row.
type DomainStatus struct {
	Domain      string     `json:"domain"`
	Door        Door       `json:"door"`
	CertExpires *time.Time `json:"cert_expires,omitempty"`
}

// Status reports the doors and every domain's door.
func (h *Hook) Status() Status {
	st := h.file.get()
	var out Status
	out.Doors.Tunnel.Open = st.Tunnel.Open
	out.Doors.Ports.Open = st.Ports.Open
	out.Doors.Ports.HTTP = st.Ports.HTTP
	out.Doors.Ports.HTTPS = st.Ports.HTTPS
	out.Doors.Local.Open = st.Local.Open
	out.Doors.Local.HTTP = st.Local.HTTP
	out.Doors.Local.DNS = st.Local.DNS
	out.Doors.Local.Help = ResolverHelp(st.Local.DNS, "middleware")
	h.mu.Lock()
	out.Doors.Ports.Listening = h.http != nil && h.https != nil
	out.Doors.Ports.Error = h.lastErr
	out.Doors.Local.Listening = h.local != nil && h.localDNS != nil
	out.Doors.Local.Error = h.lastLocalErr
	h.mu.Unlock()
	for _, d := range h.domains(st) {
		row := DomainStatus{Domain: d, Door: st.DoorOf(d)}
		if info, ok := h.Store.Info(d); ok {
			exp := info.Expires
			row.CertExpires = &exp
			if row.Door == "" {
				row.Door = Ports
			}
		}
		if row.Door == "" {
			row.Door = Tunnel
		}
		out.Domains = append(out.Domains, row)
	}
	return out
}

// domains is every name worth a row: served sites, the panel, and
// anything the file or the store still knows about.
func (h *Hook) domains(st State) []string {
	seen := map[string]bool{}
	var out []string
	add := func(d string) {
		if d != "" && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	if h.Sites != nil {
		for _, d := range h.Sites() {
			add(d)
		}
	}
	add(h.panelDomain())
	for _, d := range st.sortedDomains() {
		add(d)
	}
	for _, info := range h.Store.List() {
		add(info.Domain)
	}
	sort.Strings(out)
	return out
}

func (h *Hook) knownSite(domain string) bool {
	if p := h.panelDomain(); p != "" && domain == p {
		return true
	}
	if h.Sites == nil {
		return false
	}
	for _, d := range h.Sites() {
		if d == domain {
			return true
		}
	}
	return false
}

// Line is one verification line of a move, in the shape modules use.
type Line struct {
	Status string `json:"status"` // "ok", "skip" or "fail"
	Text   string `json:"text"`
}

// Move puts domains on a door. To the ports door it obtains the
// certificate first, through whichever door is open, and marks the domain
// only when that worked: a domain marked without a certificate would be a
// DNS change away from an outage. To the tunnel it only marks; the
// certificate stays on disk for the way back.
func (h *Hook) Move(ctx context.Context, domains []string, to Door) []Line {
	if _, err := ParsePublicDoor(string(to)); err != nil {
		return []Line{{Status: "fail", Text: err.Error()}}
	}
	var lines []Line
	moved := map[string]Door{}
	for _, d := range domains {
		d = strings.ToLower(strings.TrimSpace(d))
		switch {
		case !manifest.ValidDomain(d):
			lines = append(lines, Line{Status: "fail", Text: fmt.Sprintf("%s: not a valid domain", d)})
			continue
		case !h.knownSite(d):
			lines = append(lines, Line{Status: "fail", Text: fmt.Sprintf("%s: not a site of this installation", d)})
			continue
		}
		if to == Ports {
			if h.Store.Has(d) {
				lines = append(lines, Line{Status: "ok", Text: fmt.Sprintf("%s: certificate already present", d)})
			} else {
				cert, err := h.Store.Obtain(ctx, d)
				if err != nil {
					lines = append(lines, Line{Status: "fail", Text: fmt.Sprintf("%s: certificate not obtained: %v", d, err)})
					continue
				}
				lines = append(lines, Line{Status: "ok", Text: fmt.Sprintf("%s: certificate obtained, expires %s", d, cert.Leaf.NotAfter.Format("2006-01-02"))})
			}
		}
		moved[d] = to
		lines = append(lines, Line{Status: "ok", Text: fmt.Sprintf("%s: on the %s door", d, to)})
	}
	if len(moved) == 0 {
		return lines
	}
	st := h.file.get()
	// A fresh map: the cached state's map is read by every request, and
	// writing into it here was a data race with them.
	marked := make(map[string]Door, len(st.Domains)+len(moved))
	for d, door := range st.Domains {
		marked[d] = door
	}
	for d, door := range moved {
		marked[d] = door
	}
	st.Domains = marked
	if err := h.file.set(st); err != nil {
		return append(lines, Line{Status: "fail", Text: "state not saved: " + err.Error()})
	}
	for d, door := range moved {
		h.emit("operator", "inbound.moved", d, "", "", map[string]any{"to": string(door)})
	}
	return lines
}

// Forget drops a domain's recorded door: what unpublishing it leaves.
func (h *Hook) Forget(domain string) error {
	st := h.file.get()
	if _, ok := st.Domains[domain]; !ok {
		return nil
	}
	next := st
	next.Domains = map[string]Door{}
	for d, door := range st.Domains {
		if d != domain {
			next.Domains[d] = door
		}
	}
	return h.file.set(next)
}

// SetPublishing writes the door for new sites and the public address
// override; empty values are kept as they are.
func (h *Hook) SetPublishing(newSites Door, publicIP string, clearIP bool) error {
	st := h.file.get()
	if newSites != "" {
		st.NewSites = newSites
	}
	if clearIP {
		st.PublicIP = ""
	} else if publicIP != "" {
		st.PublicIP = publicIP
	}
	return h.file.set(st)
}

var _ app.Hook = (*Hook)(nil)

// wildcard says a listen address is every address of the machine.
func wildcard(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	return err == nil && (host == "" || host == "0.0.0.0" || host == "::")
}
