package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/engine"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/nodes"
	"github.com/dcimorra/the-middleware/internal/runtime"
)

// The database gateway (cluster.gateway): a replica's site connects to
// 127.0.0.1:3306 as it does on its primary, and the house there passes
// the connection through the mesh to the primary's 127.0.0.1:3306. Every
// node reads and writes the one database; nothing of it is copied. What
// it costs is a round trip between the nodes per query, and the replicas
// lose their database when the primary is down, which is when they are
// least needed: requests only reach them through it.
//
// SQLite cannot go through it (a file, not a server), and a container's
// 127.0.0.1 is its own, so the gateway serves sites that run as a
// process.

// gw is one address a replica's site expects its database at, listened at
// here.
type gw struct {
	// target is what the primary connects it to: an address of its
	// loopback, or "service:<name>" for one of the site's kept services.
	addr, domain, of, target string
	ln                       net.Listener
}

// servicePrefix marks a tunnel's target as a kept service of the site.
const servicePrefix = "service:"

// reconcileGateways listens where this node's replicas expect their
// databases, and stops listening where none does any more.
func (m *Manager) reconcileGateways(mesh Mesh) {
	type want struct{ domain, of, target string }
	wanted := map[string]want{}
	errs := map[string]string{}
	// Read from the folders, not the engine: a copy just put in place
	// starts within a tick, and its database must be there when it does.
	entries, _ := os.ReadDir(filepath.Join(m.opts.Root, "sites"))
	for _, e := range entries {
		man, err := manifest.Load(filepath.Join(m.opts.Root, "sites", e.Name()))
		if err != nil || !man.IsReplica() {
			continue
		}
		r := man.Replica
		add := func(a, target string) {
			if w, taken := wanted[a]; taken && w.domain != e.Name() {
				errs[e.Name()] = fmt.Sprintf("the gateway's %s is already %s's", a, w.domain)
				return
			}
			wanted[a] = want{e.Name(), r.Of, target}
		}
		for _, a := range r.Gateway {
			add(a, a)
		}
		// The services kept on the primary: where the site's processes
		// reach each one ($SERVICE_<name>_PORT on loopback), and where its
		// containers do (the name, which their hosts file points at the
		// site network's gateway, on the port inside).
		var gwIP string
		for _, k := range r.Keep {
			if port, ok := m.opts.Sites.ServicePort(e.Name(), k); ok {
				add(net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), servicePrefix+k)
			}
			if !hasContainers(man) {
				continue
			}
			if gwIP == "" {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				ip, err := m.networkGateway(ctx, manifest.DockerNetwork(e.Name()))
				cancel()
				if err != nil {
					continue // no container of it has started yet
				}
				gwIP = ip
			}
			add(net.JoinHostPort(gwIP, strconv.Itoa(containerPort(man.Services[k]))), servicePrefix+k)
		}
	}
	m.mu.Lock()
	if m.gws == nil {
		m.gws = map[string]*gw{}
	}
	for a, g := range m.gws {
		if w, ok := wanted[a]; !ok || w.domain != g.domain || w.of != g.of || w.target != g.target {
			_ = g.ln.Close()
			delete(m.gws, a)
		}
	}
	var open []string
	for a := range wanted {
		if m.gws[a] == nil {
			open = append(open, a)
		}
	}
	m.mu.Unlock()
	for _, a := range open {
		w := wanted[a]
		ln, err := net.Listen("tcp", a)
		if err != nil {
			errs[w.domain] = fmt.Sprintf("the database gateway cannot listen at %s here (%v): something of this machine already uses it", a, err)
			continue
		}
		g := &gw{addr: a, domain: w.domain, of: w.of, target: w.target, ln: ln}
		m.mu.Lock()
		m.gws[a] = g
		m.mu.Unlock()
		go m.serveGateway(mesh, g)
	}
	m.mu.Lock()
	m.gwErrs = errs
	m.mu.Unlock()
}

func (m *Manager) serveGateway(mesh Mesh, g *gw) {
	for {
		c, err := g.ln.Accept()
		if err != nil {
			return
		}
		go func() {
			x, ok := mesh.Member(g.of)
			if !ok {
				_ = c.Close()
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			t, err := mesh.Tunnel(ctx, x, g.domain, g.target)
			cancel()
			if err != nil {
				_ = c.Close()
				m.emit("cluster.gateway_failed", "warn", g.domain, fmt.Sprintf("%s on %s could not be reached: %v", g.target, x.Name, err), nil)
				return
			}
			pipe(c, t)
		}()
	}
}

// closeGateways stops every listener (the process is stopping).
func (m *Manager) closeGateways() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for a, g := range m.gws {
		_ = g.ln.Close()
		delete(m.gws, a)
	}
}

// pipe copies both ways until either side ends, then closes both.
func pipe(a, b net.Conn) {
	var once sync.Once
	done := func() {
		once.Do(func() {
			_ = a.Close()
			_ = b.Close()
		})
	}
	go func() {
		_, _ = io.Copy(a, b)
		done()
	}()
	_, _ = io.Copy(b, a)
	done()
}

func tunnelRefuse(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// TunnelHandler is the primary's end: a replica's connection for a site's
// database. Only the nodes that keep a replica of the site, only to an
// address its manifest lists, and only to this machine's loopback.
func (m *Manager) TunnelHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		from, _ := nodes.MemberFrom(r.Context())
		site, addr, _ := strings.Cut(r.Header.Get(nodes.TunnelHeader), " ")
		if !strings.EqualFold(r.Header.Get("Upgrade"), nodes.TunnelProtocol) {
			tunnelRefuse(w, http.StatusBadRequest, "not a tunnel")
			return
		}
		sn, ok := m.opts.Sites.Site(site)
		if !ok || !sn.Manifest.Clustered() {
			tunnelRefuse(w, http.StatusForbidden, "no clustered site of that name here")
			return
		}
		var allowed []string
		if sn.Manifest.ClaudeAPI != nil {
			allowed = append(allowed, DoorTarget)
		}
		for _, g := range sn.Manifest.Cluster.Gateway {
			if a, err := manifest.GatewayAddr(g); err == nil {
				allowed = append(allowed, a)
			}
		}
		for _, k := range sn.Manifest.KeptServices() {
			allowed = append(allowed, servicePrefix+k)
		}
		if !slices.Contains(allowed, addr) {
			tunnelRefuse(w, http.StatusForbidden, "that address is not in the site's gateway nor one of its kept services")
			return
		}
		m.mu.Lock()
		placed := slices.Contains(m.placement[site], from.ID)
		m.mu.Unlock()
		if !placed {
			tunnelRefuse(w, http.StatusForbidden, "that node keeps no replica of the site")
			return
		}
		if addr == DoorTarget {
			hj, ok := w.(http.Hijacker)
			if !ok {
				tunnelRefuse(w, http.StatusInternalServerError, "the door cannot hand the connection over")
				return
			}
			c, brw, err := hj.Hijack()
			if err != nil {
				return
			}
			_, _ = io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: "+nodes.TunnelProtocol+"\r\n\r\n")
			m.serveDoor(c, brw.Reader, site)
			return
		}
		dial, err := addr, error(nil)
		if k, ok := strings.CutPrefix(addr, servicePrefix); ok {
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			dial, err = m.serviceAddr(ctx, sn, k)
			cancel()
			if err != nil {
				tunnelRefuse(w, http.StatusBadGateway, fmt.Sprintf("services.%s is not reachable here: %v", k, err))
				return
			}
		}
		up, err := net.DialTimeout("tcp", dial, 5*time.Second)
		if err != nil {
			tunnelRefuse(w, http.StatusBadGateway, fmt.Sprintf("the database at %s does not answer here: %v", addr, err))
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			_ = up.Close()
			tunnelRefuse(w, http.StatusInternalServerError, "the door cannot hand the connection over")
			return
		}
		c, brw, err := hj.Hijack()
		if err != nil {
			_ = up.Close()
			return
		}
		_, _ = io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: "+nodes.TunnelProtocol+"\r\n\r\n")
		// What the replica sent behind the request, already read.
		if n := brw.Reader.Buffered(); n > 0 {
			buf, _ := brw.Reader.Peek(n)
			if _, err := up.Write(buf); err != nil {
				_ = c.Close()
				_ = up.Close()
				return
			}
		}
		pipe(c, up)
	})
}

func hasContainers(m *manifest.Manifest) bool {
	for _, sv := range m.Services {
		if sv != nil && sv.Run != nil && sv.Run.Runtime == "docker" {
			return true
		}
	}
	return false
}

// containerPort is the port a service's container listens at inside.
func containerPort(sv *manifest.Service) int {
	if sv != nil && sv.Run != nil && sv.Run.Docker != nil && sv.Run.Docker.Port != 0 {
		return sv.Run.Docker.Port
	}
	return manifest.DefaultServicePort
}

// serviceAddr is where this machine reaches one of a site's services: its
// port on loopback when it has one there (a process, or a container
// published for the site's processes), else its container's address on
// the site's network, which the machine routes to.
func (m *Manager) serviceAddr(ctx context.Context, sn engine.Snapshot, service string) (string, error) {
	sv := sn.Manifest.Services[service]
	if sv == nil || sv.Run == nil {
		return "", errors.New("no such service")
	}
	v := sn.Services[service]
	if sv.Run.Runtime != "docker" || v.Published {
		if port, ok := m.opts.Sites.ServicePort(sn.Domain, service); ok {
			return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), nil
		}
		if v.Port != 0 {
			return net.JoinHostPort("127.0.0.1", strconv.Itoa(v.Port)), nil
		}
		return "", errors.New("it has no port yet")
	}
	ip, err := m.containerIP(ctx, sn.Domain, service)
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(ip, strconv.Itoa(containerPort(sv))), nil
}

func (m *Manager) networkGateway(ctx context.Context, network string) (string, error) {
	if m.opts.NetworkGateway != nil {
		return m.opts.NetworkGateway(ctx, network)
	}
	return runtime.Docker{}.NetworkGateway(ctx, network)
}

func (m *Manager) containerIP(ctx context.Context, site, service string) (string, error) {
	if m.opts.ContainerIP != nil {
		return m.opts.ContainerIP(ctx, site, service)
	}
	return runtime.Docker{}.ServiceIP(ctx, site, service, manifest.DockerNetwork(site))
}
