package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"syscall"
	"time"

	"github.com/dcimorra/the-middleware/internal/auth"
	"github.com/dcimorra/the-middleware/internal/chat"
)

// SocketServer is the HTTP server of the unix socket. Every connection
// used to be the operator's (BaseContext marked them all), and a site's
// agent turn, which runs as the supervisor's user with the middleware
// binary in its PATH, could open it: `middleware token add` gave it a
// full token, `middleware chat middleware …` the house's own
// conversation. Now each connection is looked at before its first
// request: the kernel says which process connected (SO_PEERCRED, which
// the client cannot choose), siteOf says whether a site's turn started
// it, and such a caller is confined to that site (auth.TurnScopes). A
// connection whose process cannot be told is not trusted at all: it gets
// what an anonymous one gets, and a token still works on it.
//
// siteOf nil reads the marks in the processes' environments alone.
func SocketServer(ctx context.Context, handler http.Handler, siteOf func(pid int) (string, error)) *http.Server {
	if siteOf == nil {
		siteOf = func(pid int) (string, error) { return chat.SiteOfProcess("/proc", pid, nil) }
	}
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ConnContext: func(c context.Context, conn net.Conn) context.Context {
			return socketContext(c, conn, siteOf)
		},
	}
}

func socketContext(ctx context.Context, conn net.Conn, siteOf func(int) (string, error)) context.Context {
	pid, err := peerPID(conn)
	if err != nil {
		return ctx
	}
	site, err := siteOf(pid)
	if err != nil {
		return ctx
	}
	if id, ok := chat.CrewID(site); ok {
		return auth.WithSocketCrew(ctx, id)
	}
	if site == chat.Autopilot {
		return auth.WithSocketAutopilot(ctx)
	}
	if site != "" {
		return auth.WithSocketTurn(ctx, site)
	}
	return auth.WithSocket(ctx)
}

// peerPID is the process that connected, as the kernel recorded it at
// connect(2). 0 is a peer in another pid namespace (a container), which
// is no process of this machine's that can be looked at.
func peerPID(conn net.Conn) (int, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, errors.New("not a unix connection")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *syscall.Ucred
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if cerr != nil {
		return 0, cerr
	}
	if cred.Pid <= 0 {
		return 0, errors.New("the peer has no pid here")
	}
	return int(cred.Pid), nil
}
