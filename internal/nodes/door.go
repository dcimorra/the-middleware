package nodes

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"strings"
	"time"
)

// APIPrefix is where every node's API lives.
const APIPrefix = "/api/v1"

// DelegateHeader carries, from the node a delegated user signed in at to
// the node that holds their site, who the user is and what they may do
// there (api/delegate.go). It is believed only on the mesh door, from a
// node that proved it is one; the proxy below drops whatever a browser
// put in it.
const DelegateHeader = "X-Middleware-Delegate"

type memberKey struct{}

// MemberFrom is the node a request came from, when it came through the
// mesh door and proved it is one.
func MemberFrom(ctx context.Context) (Member, bool) {
	m, ok := ctx.Value(memberKey{}).(Member)
	return m, ok
}

// Serve sets what the door hands an authenticated node's request to: the
// API, which sees the caller as that node.
func (m *Mesh) Serve(api http.Handler) {
	m.mu.Lock()
	m.api = api
	m.mu.Unlock()
}

type doorServer struct {
	ip  netip.Addr
	srv *http.Server
}

// openDoor listens on the mesh address, and only there: the API for the
// other nodes is not on the loopback listener, not on the panel and not on
// any public address, so the only way to it is the tunnel.
func (m *Mesh) openDoor(ip netip.Addr) error {
	m.mu.Lock()
	d := m.door
	m.mu.Unlock()
	if d != nil && d.ip == ip {
		return nil
	}
	m.closeDoor()
	ln, err := net.Listen("tcp", netip.AddrPortFrom(ip, uint16(m.opts.APIPort)).String())
	if err != nil {
		return fmt.Errorf("the nodes' door on %s:%d: %w", ip, m.opts.APIPort, err)
	}
	srv := &http.Server{Handler: m.doorHandler(), ReadHeaderTimeout: 15 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	m.mu.Lock()
	m.door = &doorServer{ip: ip, srv: srv}
	m.mu.Unlock()
	return nil
}

func (m *Mesh) closeDoor() {
	m.mu.Lock()
	d := m.door
	m.door = nil
	m.mu.Unlock()
	if d != nil {
		_ = d.srv.Close()
	}
}

func remoteAddr(r *http.Request) netip.Addr {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	return ap.Addr().Unmap()
}

// authenticate is who a request on the door is: the address it came from
// must be a live member's (WireGuard binds that address to the member's
// key, so it cannot be borrowed from outside), and it must carry the
// network's secret, which a process on this same machine dialling the
// door does not have.
func (m *Mesh) authenticate(r *http.Request) (Member, bool) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return m.authenticateWith(r, tok, ok)
}

func (m *Mesh) authenticateWith(r *http.Request, tok string, ok bool) (Member, bool) {
	from := remoteAddr(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	if !ok || m.st.Secret == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(m.st.Secret)) != 1 {
		return Member{}, false
	}
	for _, x := range m.liveLocked() {
		if x.IP == from {
			return x, true
		}
	}
	return Member{}, false
}

func doorJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (m *Mesh) doorHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A site's request comes first, before the API's no-store: what
		// the site answers about caching is the site's.
		if r.Header.Get(ClusterHeader) != "" {
			m.serveSite(w, r)
			return
		}
		if r.Header.Get(TunnelHeader) != "" {
			m.serveTunnel(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		// The join is the one request without the network's secret: the
		// newcomer has only its invitation, and only from the address the
		// invitation was made for.
		if r.Method == http.MethodPost && r.URL.Path == APIPrefix+"/mesh/join" {
			var req joinRequest
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
				doorJSON(w, http.StatusBadRequest, map[string]string{"error": "body: " + err.Error()})
				return
			}
			ans, err := m.admit(remoteAddr(r), req)
			if err != nil {
				doorJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
				return
			}
			doorJSON(w, http.StatusOK, ans)
			// The newcomer's own key replaces the invitation's after the
			// answer has left: replacing it first would drop the tunnel the
			// answer travels in.
			go func() {
				time.Sleep(2 * time.Second)
				_ = m.Apply(context.Background())
			}()
			return
		}
		who, ok := m.authenticate(r)
		if !ok {
			doorJSON(w, http.StatusUnauthorized, map[string]string{"error": "not a node of this network"})
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == APIPrefix+"/mesh/roster":
			m.mu.Lock()
			out := rosterAnswer{Members: m.rosterLocked(), Settings: m.st.Settings}
			m.seen[who.ID] = m.now()
			m.mu.Unlock()
			doorJSON(w, http.StatusOK, out)
			return
		case r.Method == http.MethodGet && r.URL.Path == APIPrefix+"/mesh/names":
			names := []string{}
			if m.opts.Names != nil {
				names = m.opts.Names()
			}
			doorJSON(w, http.StatusOK, names)
			return
		case r.Method == http.MethodPost && r.URL.Path == APIPrefix+"/mesh/leave":
			m.left(who.ID)
			doorJSON(w, http.StatusOK, map[string]bool{"ok": true})
			go func() { _ = m.Apply(context.Background()) }()
			return
		case r.Method == http.MethodPost && r.URL.Path == APIPrefix+"/mesh/expelled":
			doorJSON(w, http.StatusOK, map[string]bool{"ok": true})
			go m.wipe("mesh.expelled", "node "+who.Name+" removed this one from the network")
			return
		}
		m.mu.Lock()
		api := m.api
		m.mu.Unlock()
		if api == nil || !(r.URL.Path == APIPrefix || strings.HasPrefix(r.URL.Path, APIPrefix+"/")) {
			doorJSON(w, http.StatusNotFound, map[string]string{"error": "no such route"})
			return
		}
		api.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), memberKey{}, who)))
	})
}

// transport is the client side of the mesh: every call leaves from this
// node's mesh address, which is what the other side checks, and has no
// overall timeout because the proxy carries streams.
func (m *Mesh) transport() *http.Transport {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.trans != nil && m.transIP == m.st.IP {
		return m.trans
	}
	if m.trans != nil {
		m.trans.CloseIdleConnections()
	}
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	if m.st.IP.IsValid() {
		d.LocalAddr = &net.TCPAddr{IP: net.IP(m.st.IP.AsSlice())}
	}
	m.trans = &http.Transport{DialContext: d.DialContext, MaxIdleConnsPerHost: 8, IdleConnTimeout: 90 * time.Second}
	m.transIP = m.st.IP
	return m.trans
}

func (m *Mesh) base(ip netip.Addr) string {
	return "http://" + netip.AddrPortFrom(ip, uint16(m.opts.APIPort)).String()
}

func (m *Mesh) secret() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.st.Secret
}

// Call does one JSON request to a member's API (path under /api/v1).
func (m *Mesh) Call(ctx context.Context, x Member, method, path string, body, out any) error {
	return m.call(ctx, x.IP, m.secret(), method, path, body, out)
}

func (m *Mesh) call(ctx context.Context, ip netip.Addr, secret, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, m.base(ip)+APIPrefix+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	res, err := (&http.Client{Transport: m.transport()}).Do(req)
	if err != nil {
		var ue interface{ Unwrap() error }
		if errors.As(err, &ue) && ue.Unwrap() != nil {
			return ue.Unwrap()
		}
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if res.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		return &RemoteError{Status: res.StatusCode, Message: e.Error}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// Stream sends body as it is (a site's bundle) to a member's API and reads
// the JSON answer into out. The body is never held whole: a site of
// gigabytes goes through the mesh as it is packed.
func (m *Mesh) Stream(ctx context.Context, x Member, method, path, contentType string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, m.base(x.IP)+APIPrefix+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", contentType)
	if secret := m.secret(); secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	res, err := (&http.Client{Transport: m.transport()}).Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if res.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		return &RemoteError{Status: res.StatusCode, Message: e.Error}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// Open starts a GET on a member's API whose body keeps coming (the event
// stream), with header added; the caller closes the body. A status other
// than 200 is a RemoteError.
func (m *Mesh) Open(ctx context.Context, x Member, path string, header http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.base(x.IP)+APIPrefix+path, nil)
	if err != nil {
		return nil, err
	}
	for k, vs := range header {
		req.Header[k] = vs
	}
	if secret := m.secret(); secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	res, err := (&http.Client{Transport: m.transport()}).Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		return nil, &RemoteError{Status: res.StatusCode}
	}
	return res, nil
}

// RemoteError is a node's own refusal, with its status.
type RemoteError struct {
	Status  int
	Message string
}

func (e *RemoteError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("the node answered %d", e.Status)
	}
	return e.Message
}

// Proxy forwards a request to a member's API: rest is the path after
// /api/v1. The browser's credentials for this panel stay here; on the
// other side this node is the network's secret from its mesh address. The
// answer streams back as it comes, which keeps the event stream and a
// chat's turn live through the hop.
func (m *Mesh) Proxy(x Member, rest string) *httputil.ReverseProxy {
	secret := m.secret()
	return &httputil.ReverseProxy{
		Transport:     m.transport(),
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			out := pr.Out
			out.URL.Scheme, out.URL.Host = "http", netip.AddrPortFrom(x.IP, uint16(m.opts.APIPort)).String()
			out.URL.Path, out.URL.RawPath = APIPrefix+"/"+strings.TrimPrefix(rest, "/"), ""
			out.URL.RawQuery = pr.In.URL.RawQuery
			out.Host = out.URL.Host
			for _, h := range []string{"Cookie", "Authorization", "X-Requested-With", "Origin", "Referer", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip", "Forwarded", "Cf-Connecting-Ip", "Cf-Ipcountry", "True-Client-Ip", DelegateHeader} {
				out.Header.Del(h)
			}
			out.Header.Set("Authorization", "Bearer "+secret)
		},
		ModifyResponse: func(res *http.Response) error {
			res.Header.Del("Set-Cookie")
			// The other node refusing this one is not the operator's session
			// ending here: passed on as a 401 it would log them out of this
			// panel. It is this node not being in that network any more.
			if res.StatusCode == http.StatusUnauthorized {
				res.Body.Close()
				msg, _ := json.Marshal(map[string]string{"error": fmt.Sprintf("node %s does not recognise this one as part of its network", x.Name)})
				msg = append(msg, '\n')
				res.StatusCode, res.Status = http.StatusBadGateway, "502 Bad Gateway"
				res.Header = http.Header{"Content-Type": {"application/json; charset=utf-8"}}
				res.Body = io.NopCloser(bytes.NewReader(msg))
				res.ContentLength = int64(len(msg))
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				return
			}
			doorJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("node %s (%s) cannot be reached through the mesh: %v", x.Name, x.IP, err)})
		},
	}
}
