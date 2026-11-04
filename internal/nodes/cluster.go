package nodes

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"strings"
	"time"
)

// A site in cluster mode (internal/cluster) has copies on other nodes,
// and the node its names point at hands some of its requests to them.
// They travel on the mesh door, the one port the other nodes may reach
// here, as the site's own request: same method, path, body and headers,
// the visitor's Authorization and cookies included. So the network's
// secret cannot ride in Authorization as the API's calls carry it; it
// goes in its own header, and ClusterHeader says the request is a site's
// and not the API's.

// ClusterHeader marks a request on the mesh door as a site's, for a
// replica here; ClusterKeyHeader carries the network's secret for it.
const (
	ClusterHeader    = "X-Middleware-Cluster"
	ClusterKeyHeader = "X-Middleware-Cluster-Key"
)

// ServeSites sets what the door hands a site's request from another node
// to (the replicas' handler). Nil answers them 404.
func (m *Mesh) ServeSites(h http.Handler) {
	m.mu.Lock()
	m.sites = h
	m.mu.Unlock()
}

func (m *Mesh) serveSite(w http.ResponseWriter, r *http.Request) {
	who, ok := m.authenticateWith(r, r.Header.Get(ClusterKeyHeader), true)
	if !ok {
		doorJSON(w, http.StatusUnauthorized, map[string]string{"error": "not a node of this network"})
		return
	}
	m.mu.Lock()
	h := m.sites
	m.mu.Unlock()
	if h == nil {
		doorJSON(w, http.StatusNotFound, map[string]string{"error": "this node keeps no replicas"})
		return
	}
	r.Header.Del(ClusterKeyHeader)
	r.Header.Del(ClusterHeader)
	// The node that sent it already decided who the visitor is and wrote
	// it the way the tunnel's connector does (CF-Connecting-IP,
	// X-Forwarded-Proto, CF-IPCountry); the hop is made to look like
	// that connector, on loopback, so the house here believes the same
	// visitor and not the other node's mesh address. Only an
	// authenticated node gets this far.
	r.RemoteAddr = "127.0.0.1:0"
	h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), memberKey{}, who)))
}

// SiteProxy hands a site's request to member x, where a replica of the
// site answers it. who writes, on the outgoing headers, the visitor as
// this node sees it (the replica believes it); fail runs instead of the
// default error page when x cannot be reached, so the caller may serve
// the request itself.
func (m *Mesh) SiteProxy(x Member, who func(in *http.Request, out http.Header), fail func(w http.ResponseWriter, r *http.Request, err error)) *httputil.ReverseProxy {
	secret := m.secret()
	return &httputil.ReverseProxy{
		Transport:     m.transport(),
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			out := pr.Out
			out.URL.Scheme, out.URL.Host = "http", netip.AddrPortFrom(x.IP, uint16(m.opts.APIPort)).String()
			// The site's own name: the replica finds the site by it.
			out.Host = pr.In.Host
			for k := range out.Header {
				if strings.HasPrefix(http.CanonicalHeaderKey(k), "X-Middleware-") {
					out.Header.Del(k)
				}
			}
			if who != nil {
				who(pr.In, out.Header)
			}
			out.Header.Set(ClusterHeader, "1")
			out.Header.Set(ClusterKeyHeader, secret)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				return
			}
			if fail != nil {
				fail(w, r, err)
				return
			}
			http.Error(w, "the node that serves this request cannot be reached", http.StatusServiceUnavailable)
		},
	}
}

// A replica's database is its primary's: the site on another node writes
// and reads the same one. A replica's house listens where the site
// expects its database (127.0.0.1:3306) and passes each connection
// through the mesh door to the primary, which connects it to the real
// one. The door's port is the only one the nodes reach on each other, so
// the connection is an HTTP upgrade on it, as a websocket is.

// TunnelHeader names, on such an upgrade, the site and the address on
// the primary ("example.com 127.0.0.1:3306"); TunnelProtocol is its
// Upgrade.
const (
	TunnelHeader   = "X-Middleware-Tunnel"
	TunnelProtocol = "mw-tunnel"
)

// ServeTunnels sets what decides and serves an upgrade for a tunnel
// (internal/cluster). Nil refuses them.
func (m *Mesh) ServeTunnels(h http.Handler) {
	m.mu.Lock()
	m.tunnels = h
	m.mu.Unlock()
}

func (m *Mesh) serveTunnel(w http.ResponseWriter, r *http.Request) {
	who, ok := m.authenticateWith(r, r.Header.Get(ClusterKeyHeader), true)
	if !ok {
		doorJSON(w, http.StatusUnauthorized, map[string]string{"error": "not a node of this network"})
		return
	}
	m.mu.Lock()
	h := m.tunnels
	m.mu.Unlock()
	if h == nil {
		doorJSON(w, http.StatusNotFound, map[string]string{"error": "this node opens no tunnels"})
		return
	}
	h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), memberKey{}, who)))
}

// Tunnel opens a tunnel to x for site's address on it: a connection whose
// bytes reach that address there, once x accepted it.
func (m *Mesh) Tunnel(ctx context.Context, x Member, site, addr string) (net.Conn, error) {
	d := m.transport().DialContext
	c, err := d(ctx, "tcp", netip.AddrPortFrom(x.IP, uint16(m.opts.APIPort)).String())
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://"+x.IP.String()+APIPrefix+"/cluster/tunnel", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", TunnelProtocol)
	req.Header.Set(TunnelHeader, site+" "+addr)
	req.Header.Set(ClusterKeyHeader, m.secret())
	if err := req.Write(c); err != nil {
		c.Close()
		return nil, err
	}
	br := bufio.NewReader(c)
	res, err := http.ReadResponse(br, req)
	if err != nil {
		c.Close()
		return nil, err
	}
	if res.StatusCode != http.StatusSwitchingProtocols {
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		res.Body.Close()
		c.Close()
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		return nil, &RemoteError{Status: res.StatusCode, Message: e.Error}
	}
	_ = c.SetDeadline(time.Time{})
	return &bufConn{Conn: c, r: br}, nil
}

// bufConn reads what the response's reader already took first.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }
