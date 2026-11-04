package cluster

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/nodes"
)

type nodesMember = nodes.Member

// The site door through the primary (Replica.Door). A copy's processes
// get MIDDLEWARE_API at an address of this node's loopback, listened at
// here and passed through the mesh to the primary, which serves the site
// door there with the site's own token put on every request. So the
// token, the rules and the spending are the primary's, as if the copy
// were the primary: before this, a site with claude_api could have no
// copies at all, since each node's door has its own token and its own
// ledger.
//
// The address is kept across restarts (data/cluster/doors.json): a
// process started by an earlier supervisor is re-adopted with the
// environment it was given, and a door at another port would leave it
// calling nothing.

// DoorTarget is the tunnel address that means the site door.
const DoorTarget = "door"

// DoorToken is what a copy's processes find in MIDDLEWARE_TOKEN: a word,
// not a secret. The primary replaces whatever the request carries with
// the site's own token.
const DoorToken = "through-the-primary"

type doorState struct {
	mu    sync.Mutex
	open  map[string]*gw // domain
	ports map[string]int // domain → port, as saved
}

func (m *Manager) doorsFile() string {
	return filepath.Join(m.opts.Root, "data", "cluster", "doors.json")
}

func (m *Manager) loadDoorPorts() map[string]int {
	out := map[string]int{}
	if raw, err := os.ReadFile(m.doorsFile()); err == nil {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

// DoorEnv is what a copy's processes need to call the site door: nil for
// a site that is not a copy with Door. The listener is opened here if it
// is not yet, since a copy's process can start before the next round.
func (m *Manager) DoorEnv(domain string, man *manifest.Manifest) map[string]string {
	if !man.IsReplica() || !man.Replica.Door {
		return nil
	}
	addr, err := m.doorAddr(domain, man.Replica.Of)
	if err != nil {
		m.emit("cluster.door_failed", "warn", domain, "the AI door through the primary cannot listen here: "+err.Error(), nil)
		return nil
	}
	return map[string]string{"MIDDLEWARE_API": "http://" + addr, "MIDDLEWARE_TOKEN": DoorToken}
}

func (m *Manager) doorAddr(domain, of string) (string, error) {
	d := &m.doors
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.open == nil {
		d.open = map[string]*gw{}
		d.ports = m.loadDoorPorts()
	}
	if g := d.open[domain]; g != nil && g.of == of {
		return g.addr, nil
	} else if g != nil {
		_ = g.ln.Close()
		delete(d.open, domain)
	}
	var ln net.Listener
	var err error
	if p := d.ports[domain]; p != 0 {
		ln, err = net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
	}
	if ln == nil {
		// The saved port is taken (or there is none): a new one, saved.
		// A process still holding the old address loses the door until
		// it restarts, which a supervisor restart does not do.
		if ln, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			return "", err
		}
	}
	addr := ln.Addr().String()
	_, port, _ := net.SplitHostPort(addr)
	n, _ := strconv.Atoi(port)
	if d.ports[domain] != n {
		d.ports[domain] = n
		if raw, err := json.MarshalIndent(d.ports, "", "  "); err == nil {
			_ = os.MkdirAll(filepath.Dir(m.doorsFile()), 0o755)
			_ = fsx.WriteAtomic(m.doorsFile(), raw, 0o644)
		}
	}
	g := &gw{addr: addr, domain: domain, of: of, target: DoorTarget, ln: ln}
	d.open[domain] = g
	go m.serveDoorListener(g)
	return addr, nil
}

// serveDoorListener is serveGateway for a door, which can open before
// the mesh is wired (a copy's process starting at boot): the mesh is
// read at each connection.
func (m *Manager) serveDoorListener(g *gw) {
	for {
		c, err := g.ln.Accept()
		if err != nil {
			return
		}
		go func() {
			mesh := m.getMesh()
			var x nodesMember
			ok := false
			if mesh != nil {
				x, ok = mesh.Member(g.of)
			}
			if !ok {
				_ = c.Close()
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			t, err := mesh.Tunnel(ctx, x, g.domain, DoorTarget)
			cancel()
			if err != nil {
				_ = c.Close()
				m.emit("cluster.door_failed", "warn", g.domain, fmt.Sprintf("the AI door on %s could not be reached: %v", x.Name, err), nil)
				return
			}
			pipe(c, t)
		}()
	}
}

// reconcileDoors closes the doors of copies that are gone or no longer
// have one, and opens those that do and are not open yet.
func (m *Manager) reconcileDoors() {
	want := map[string]string{} // domain → of
	entries, _ := os.ReadDir(filepath.Join(m.opts.Root, "sites"))
	for _, e := range entries {
		man, err := manifest.Load(filepath.Join(m.opts.Root, "sites", e.Name()))
		if err == nil && man.IsReplica() && man.Replica.Door {
			want[e.Name()] = man.Replica.Of
		}
	}
	d := &m.doors
	d.mu.Lock()
	for domain, g := range d.open {
		if of, ok := want[domain]; !ok || of != g.of {
			_ = g.ln.Close()
			delete(d.open, domain)
		}
	}
	d.mu.Unlock()
	for domain, of := range want {
		if _, err := m.doorAddr(domain, of); err != nil {
			m.emit("cluster.door_failed", "warn", domain, "the AI door through the primary cannot listen here: "+err.Error(), nil)
		}
	}
}

func (m *Manager) closeDoors() {
	d := &m.doors
	d.mu.Lock()
	defer d.mu.Unlock()
	for domain, g := range d.open {
		_ = g.ln.Close()
		delete(d.open, domain)
	}
}

// SetDoor hands the manager the site door: door(site) serves one site's
// calls with its token on them.
func (m *Manager) SetDoor(door func(site string) http.Handler) {
	m.mu.Lock()
	m.door = door
	m.mu.Unlock()
}

// serveDoor is the primary's end of a copy's door tunnel: the connection
// is HTTP from the copy's process, served by the site door with the
// site's token put on each request, whatever the copy sent.
func (m *Manager) serveDoor(c net.Conn, br *bufio.Reader, site string) {
	m.mu.Lock()
	door := m.door
	m.mu.Unlock()
	if door == nil {
		_ = c.Close()
		return
	}
	h := door(site)
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 15 * time.Second}
	_ = srv.Serve(&oneConn{c: &bufConn{Conn: c, r: br}})
}

// bufConn reads what the tunnel's request had already read first.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// oneConn is a listener of one connection, for http.Server: Accept gives
// it once, then waits until the connection is closed and says so.
type oneConn struct {
	mu   sync.Mutex
	c    net.Conn
	done chan struct{}
}

func (l *oneConn) Accept() (net.Conn, error) {
	l.mu.Lock()
	if l.done == nil {
		l.done = make(chan struct{})
	}
	c := l.c
	l.c = nil
	done := l.done
	l.mu.Unlock()
	if c != nil {
		return &closeConn{Conn: c, done: done}, nil
	}
	<-done
	return nil, errors.New("tunnel closed")
}

func (l *oneConn) Close() error { return nil }

func (l *oneConn) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

type closeConn struct {
	net.Conn
	once sync.Once
	done chan struct{}
}

func (c *closeConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.done) })
	return err
}
