package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/dcimorra/the-middleware/internal/edge"
)

// CookieName is the session cookie.
const CookieName = "mw_session"

// Via says how a caller was recognised.
type Via string

// The ways in.
const (
	ViaNone   Via = ""
	ViaCookie Via = "cookie"
	ViaToken  Via = "token"
	ViaSocket Via = "socket"
	// ViaNode is another node of the mesh, recognised by its tunnel address
	// and the network's secret on the mesh door; Token carries its name.
	ViaNode Via = "node"
)

// Caller is who is making the request, as resolved by Middleware.
type Caller struct {
	Via     Via
	TokenID string // when Via is token
	Token   string // the token's name, for the audit trail
	// Scopes are the token's; empty is full access.
	Scopes []string
	// Turn is the site whose agent turn made this socket request; its
	// Scopes are then TurnScopes(Turn).
	Turn string
	// Autopilot is a socket request made from a turn of the house's
	// autonomous agent; its Scopes are then AutopilotScopes().
	Autopilot bool
	// Crew is the agent of the operator's crew (internal/crew) whose turn
	// made this socket request; its Scopes are then CrewScopes(), and the
	// API holds it to its own agent's routes.
	Crew string
	// User is a delegate's name (users.go), UserID their id where they
	// signed in, and Grants what they may do by site (perms.go). On the
	// node a delegate's request was relayed to, User and Grants come from
	// the node that relayed it, for the one site, and UserID is empty.
	User   string
	UserID string
	Grants map[string][]string
	// Agents are the delegate's limits on agents (agents.go), nil for
	// none; they travel with the user to the node that holds the site.
	Agents map[string][]string
}

// Delegate reports whether the caller is a delegated user, whose every
// route is decided by UserAllows.
func (c Caller) Delegate() bool { return c.User != "" }

// Authenticated reports whether the caller may use the API.
func (c Caller) Authenticated() bool { return c.Via != ViaNone }

type ctxKey int

const (
	callerKey ctxKey = iota
	socketKey
	turnKey
	autopilotKey
	crewKey
)

// WithSocket marks a context as belonging to a connection that arrived
// over the unix socket. The socket listener sets it per connection, once
// it has looked at who connected; nothing a client sends can set it.
func WithSocket(ctx context.Context) context.Context {
	return context.WithValue(ctx, socketKey, true)
}

// WithSocketTurn marks a socket connection made from inside a site's
// agent turn: the caller is that site's, not the operator.
func WithSocketTurn(ctx context.Context, site string) context.Context {
	return context.WithValue(WithSocket(ctx), turnKey, site)
}

// WithSocketAutopilot marks a socket connection made from inside a turn
// of the autonomous agent: neither the operator nor a site.
func WithSocketAutopilot(ctx context.Context) context.Context {
	return context.WithValue(WithSocket(ctx), autopilotKey, true)
}

// WithSocketCrew marks a socket connection made from inside a turn of
// the crew agent id.
func WithSocketCrew(ctx context.Context, id string) context.Context {
	return context.WithValue(WithSocket(ctx), crewKey, id)
}

// IsSocket reports whether ctx came through WithSocket.
func IsSocket(ctx context.Context) bool {
	v, _ := ctx.Value(socketKey).(bool)
	return v
}

// SocketCaller is the caller of a socket connection: the operator, or a
// site's turn confined to its site.
func SocketCaller(ctx context.Context) Caller {
	if site, _ := ctx.Value(turnKey).(string); site != "" {
		return Caller{Via: ViaSocket, Turn: site, Scopes: TurnScopes(site)}
	}
	if id, _ := ctx.Value(crewKey).(string); id != "" {
		return Caller{Via: ViaSocket, Crew: id, Scopes: CrewScopes()}
	}
	if on, _ := ctx.Value(autopilotKey).(bool); on {
		return Caller{Via: ViaSocket, Autopilot: true, Scopes: AutopilotScopes()}
	}
	return Caller{Via: ViaSocket}
}

// FromContext returns the caller Middleware resolved, or the zero Caller.
func FromContext(ctx context.Context) Caller {
	c, _ := ctx.Value(callerKey).(Caller)
	return c
}

// WithCaller attaches a caller for tests and for handlers mounted
// elsewhere.
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey, c)
}

// Resolve identifies the caller of r without rejecting anything: the
// socket first (it cannot be forged), then a bearer token, then the
// cookie. Deciding what an unauthenticated caller may do is the API's.
func (s *Store) Resolve(r *http.Request) Caller {
	if IsSocket(r.Context()) {
		return SocketCaller(r.Context())
	}
	if h := r.Header.Get("Authorization"); h != "" {
		if secret, ok := strings.CutPrefix(h, "Bearer "); ok {
			if t, ok := s.LookupToken(strings.TrimSpace(secret)); ok {
				return Caller{Via: ViaToken, TokenID: t.ID, Token: t.Name, Scopes: t.Scopes}
			}
		}
		return Caller{}
	}
	if c, err := r.Cookie(CookieName); err == nil {
		if u, ok := s.sessionUser(c.Value); ok {
			if u.ID == "" {
				return Caller{Via: ViaCookie}
			}
			return Caller{Via: ViaCookie, User: u.Name, UserID: u.ID, Grants: u.GrantMap(), Agents: u.Agents}
		}
	}
	return Caller{}
}

// Middleware resolves the caller into the request context.
func (s *Store) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(WithCaller(r.Context(), s.Resolve(r))))
	})
}

// SetCookie writes the session cookie for id. Secure is set when the
// request came over TLS, directly or through a proxy that says so: a
// Secure cookie on plain http://localhost would never come back.
func SetCookie(w http.ResponseWriter, r *http.Request, id string) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    id,
		Path:     "/",
		MaxAge:   int(SessionTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   IsHTTPS(r),
	})
}

// ClearCookie tells the browser to forget the session.
func ClearCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   IsHTTPS(r),
	})
}

// IsHTTPS reports whether the client's hop was TLS.
func IsHTTPS(r *http.Request) bool { return edge.HTTPS(r) }

// ClientIP is the address login failures are counted against: the one the
// edge package believes. It used to prefer CF-Connecting-IP from anyone,
// on the theory that a forger could only spread its own failures across
// addresses; that is exactly what unlimited guessing needs, and V1 closed
// the same hole on 7 September 2026.
func ClientIP(r *http.Request) string {
	if ip := edge.ClientIP(r); ip != nil {
		return ip.String()
	}
	return r.RemoteAddr
}
