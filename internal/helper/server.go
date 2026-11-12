package helper

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Peer is who opened the socket, as the kernel reports it.
type Peer struct {
	UID uint32
	GID uint32
	PID int32
}

// Server is the helper service. Zero values take the production defaults;
// tests set every path to a temp dir and stub Credentials.
type Server struct {
	// Socket path. Its directory is created; an old socket file is removed.
	Socket string
	// SocketGroup is the gid the socket file is chowned to (0660), so the
	// supervisor's group can open it. -1 leaves it as created.
	SocketGroup int
	// AllowUID and AllowGID: a peer must match one of them. -1 disables
	// that check; both at -1 means only root may call, which is what a
	// misconfigured unit should fall back to.
	AllowUID int
	AllowGID int
	// Exec runs the binaries. nil means the real system.
	Exec Exec
	// Credentials reads the peer's identity from a connection. nil means
	// SO_PEERCRED; tests substitute a stub.
	Credentials func(net.Conn) (Peer, error)
	// Build is reported by ping alongside the protocol version.
	Build string
	// WireguardDir, SysctlDir and SysNetDir are the system paths, overridable
	// so a test can watch what gets written without touching /etc.
	WireguardDir string
	SysctlDir    string
	SysNetDir    string
	// GroupsOf lists a user's groups; nil reads the system's (tests set it).
	GroupsOf func(uid uint32) []int
	// Binary is the executable update.install replaces; empty is this
	// one. Arch overrides the release's architecture, and Exit how the
	// helper ends after an update (tests).
	Binary string
	Arch   string
	Exit   func()
	// UpdateGrace is how long a new supervisor has to confirm an update
	// before it is rolled back, and WatchEvery how often the trial looks
	// at the supervisor; zero is the default (tests shorten both).
	UpdateGrace time.Duration
	WatchEvery  time.Duration
	// SystemdDir is where the installed units are (install.run reads the
	// service's); empty is /etc/systemd/system.
	SystemdDir string
	// Log receives one line per call. nil means stderr, which under
	// systemd is the journal.
	Log *log.Logger

	ops  map[string]func(context.Context, json.RawMessage) (any, error)
	once sync.Once
	// updMu orders the update record's readers and writers: an install,
	// a confirmation and a rollback each read it and write it back.
	updMu sync.Mutex
}

var (
	errBadIP       = errors.New("ip must be a plain IPv4 or IPv6 address")
	errUnknownOp   = errors.New("unknown operation")
	errNotAllowed  = errors.New("peer is not allowed to use the helper")
	errBadArgument = errors.New("invalid arguments")
)

func (s *Server) init() {
	s.once.Do(func() {
		if s.Socket == "" {
			s.Socket = DefaultSocket
		}
		if s.Exec == nil {
			s.Exec = &SystemExec{LockDir: filepath.Dir(s.Socket)}
		}
		if s.Credentials == nil {
			s.Credentials = peerCredentials
		}
		if s.WireguardDir == "" {
			s.WireguardDir = "/etc/wireguard"
		}
		if s.SysctlDir == "" {
			s.SysctlDir = "/etc/sysctl.d"
		}
		if s.SysNetDir == "" {
			s.SysNetDir = "/sys/class/net"
		}
		if s.Log == nil {
			s.Log = log.New(os.Stderr, "helper: ", 0)
		}
		s.ops = map[string]func(context.Context, json.RawMessage) (any, error){
			OpPing:            s.ping,
			OpFirewallApply:   s.firewallApply,
			OpFirewallBlock:   s.firewallBlock,
			OpFirewallUnblock: s.firewallUnblock,
			OpFirewallStatus:  s.firewallStatus,
			OpVPNApply:        s.vpnApply,
			OpVPNUp:           s.vpnUp,
			OpVPNDown:         s.vpnDown,
			OpVPNStatus:       s.vpnStatus,
			OpSysctlReserve:   s.sysctlReservePorts,
			OpUpdateInstall:   s.updateInstall,
			OpUpdateConfirm:   s.updateConfirm,
			OpMeshFirewall:    s.meshFirewall,
			OpInstallRun:      s.installRun,
			OpPortsOpen:       s.portsOpen,
		}
	})
}

// Listen creates the socket. It is separate from Serve so a caller can
// know the socket exists (and fail early on a permission problem) before
// the loop starts.
func (s *Server) Listen() (net.Listener, error) {
	s.init()
	if err := os.MkdirAll(filepath.Dir(s.Socket), 0o755); err != nil {
		return nil, err
	}
	// A stale socket from a previous run refuses the bind; removing it is
	// safe because nothing but this service ever listens there.
	if err := os.Remove(s.Socket); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	ln, err := net.Listen("unix", s.Socket)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(s.Socket, 0o660); err != nil {
		ln.Close()
		return nil, err
	}
	if s.SocketGroup > 0 {
		if err := os.Chown(s.Socket, -1, s.SocketGroup); err != nil {
			ln.Close()
			return nil, fmt.Errorf("chown socket to gid %d: %w", s.SocketGroup, err)
		}
	}
	return ln, nil
}

// Serve answers on ln until ctx ends.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	s.init()
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.serveConn(ctx, conn)
		}()
	}
}

func (s *Server) serveConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	peer, err := s.Credentials(conn)
	if err != nil {
		s.Log.Printf("refused connection: %v", err)
		return
	}
	r := bufio.NewReaderSize(conn, 1<<20)
	enc := json.NewEncoder(conn)
	if !s.allowed(peer) {
		s.Log.Printf("refused uid=%d gid=%d pid=%d: %v", peer.UID, peer.GID, peer.PID, errNotAllowed)
		// Read the request before answering: closing the socket while the
		// client is still writing turns the refusal into a broken pipe on
		// its side, and it never learns why. The request is not acted on.
		var id string
		if line, err := r.ReadBytes('\n'); err == nil {
			var req Request
			if json.Unmarshal(line, &req) == nil {
				id = req.ID
			}
		}
		_ = enc.Encode(Response{ID: id, Error: errNotAllowed.Error()})
		return
	}
	for {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Minute))
		line, err := r.ReadBytes('\n')
		if len(line) == 0 && err != nil {
			if !errors.Is(err, io.EOF) && ctx.Err() == nil {
				s.Log.Printf("uid=%d read: %v", peer.UID, err)
			}
			return
		}
		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			_ = enc.Encode(Response{Error: "malformed request: " + err.Error()})
			return
		}
		resp := s.Handle(ctx, peer, req)
		if err := enc.Encode(resp); err != nil {
			return
		}
	}
}

func (s *Server) allowed(p Peer) bool {
	if p.UID == 0 {
		return true
	}
	if s.AllowUID >= 0 && int(p.UID) == s.AllowUID {
		return true
	}
	if s.AllowGID >= 0 && int(p.GID) == s.AllowGID {
		return true
	}
	// The group as a secondary one: the operator's own shell (`middleware
	// vpn status`) has it that way, and SO_PEERCRED gives the primary only.
	// Only the user's membership is read, never anything the peer says.
	if s.AllowGID >= 0 {
		groupsOf := s.GroupsOf
		if groupsOf == nil {
			groupsOf = userGroups
		}
		for _, g := range groupsOf(p.UID) {
			if g == s.AllowGID {
				return true
			}
		}
	}
	return false
}

// userGroups are the groups the system lists the user in.
func userGroups(uid uint32) []int {
	u, err := user.LookupId(strconv.FormatUint(uint64(uid), 10))
	if err != nil {
		return nil
	}
	ids, err := u.GroupIds()
	if err != nil {
		return nil
	}
	var out []int
	for _, id := range ids {
		if n, err := strconv.Atoi(id); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// Handle runs one request for an already authenticated peer. It is the
// whole dispatch, exported so a test can call operations without a socket.
func (s *Server) Handle(ctx context.Context, peer Peer, req Request) Response {
	s.init()
	started := time.Now()
	op, ok := s.ops[req.Op]
	if !ok {
		s.Log.Printf("op=%s uid=%d refused: %v", req.Op, peer.UID, errUnknownOp)
		return Response{ID: req.ID, Error: errUnknownOp.Error()}
	}
	result, err := op(ctx, req.Args)
	if err != nil {
		s.Log.Printf("op=%s uid=%d failed in %s: %v", req.Op, peer.UID, time.Since(started).Round(time.Millisecond), err)
		return Response{ID: req.ID, Error: err.Error()}
	}
	data, err := json.Marshal(result)
	if err != nil {
		return Response{ID: req.ID, Error: "encode result: " + err.Error()}
	}
	s.Log.Printf("op=%s uid=%d ok in %s", req.Op, peer.UID, time.Since(started).Round(time.Millisecond))
	return Response{ID: req.ID, OK: true, Result: data}
}

// decode parses args strictly: an unknown field is a client built against
// a different protocol, and silently ignoring it is how a "vpn_only" flag
// would be dropped on the floor without anyone noticing.
func decode(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %v", errBadArgument, err)
	}
	return nil
}

func (s *Server) ping(context.Context, json.RawMessage) (any, error) {
	return PingResult{Version: Version, Build: s.Build}, nil
}
