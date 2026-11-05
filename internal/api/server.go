// Package api is /api/v1: everything the panel and the CLI do, as JSON
// over HTTP, on the panel domain, on the loopback listener and on the
// unix socket. docs/api.md is the contract. Other packages add their own
// routes through Handle and Mount; this one owns authentication, the
// sites, the events and the system.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/auth"
	"github.com/dcimorra/the-middleware/internal/engine"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/nodes"
	"github.com/dcimorra/the-middleware/internal/publish"
	"github.com/dcimorra/the-middleware/internal/settings"
	"github.com/dcimorra/the-middleware/internal/sites"
)

// Prefix is where the API lives.
const Prefix = "/api/v1"

// CSRFHeader must be sent, with CSRFValue, on every non-GET request that
// authenticates with the cookie. A browser on another origin can make the
// cookie travel but cannot set a custom header without a CORS preflight,
// which this API never answers. Tokens and the socket do not need it.
const (
	CSRFHeader = "X-Requested-With"
	CSRFValue  = "middleware"
)

// HandlerFunc answers a route: it returns what to encode as JSON, or an
// error. An *Error carries its own status; any other error is a 500 with
// a generic message, the details going to the log and the event bus.
// Return Handled when the handler wrote the response itself.
type HandlerFunc func(w http.ResponseWriter, r *http.Request) (any, error)

// Handled is what a HandlerFunc returns when it has already written the
// response (a stream, a file, text).
var Handled = &handled{}

type handled struct{}

// Error is an HTTP error with a status.
type Error struct {
	Status  int
	Message string
}

// Error implements error.
func (e *Error) Error() string { return e.Message }

// Errorf builds an *Error.
func Errorf(status int, format string, args ...any) *Error {
	return &Error{Status: status, Message: fmt.Sprintf(format, args...)}
}

// OK is the `{ok: true}` answer.
var OK = map[string]bool{"ok": true}

// Options wire a Server to the rest of the process.
type Options struct {
	Root     string
	Version  string
	Listen   string
	Auth     *auth.Store
	Sites    *sites.Manager
	Engine   *engine.Engine
	Bus      *events.Bus
	Settings func() settings.Settings
	// SaveSettings persists and applies new settings; nil makes PUT
	// /settings a 501.
	SaveSettings func(settings.Settings) error
	// UI answers everything on the panel host that is not the API.
	UI http.Handler
	// Restart is what POST /system/restart does; nil sends SIGTERM to
	// this process.
	Restart func()
	Started time.Time
	// Isolation says why this node cannot put sites in jails, or nil when
	// it can. While it cannot, a delegate's code and conversations are
	// refused here (auth.NeedsJail). nil is a node with no such check.
	Isolation func() error
}

// Route is one registered endpoint, for the OpenAPI document.
type Route struct {
	Method  string `json:"method"`
	Path    string `json:"path"`
	Summary string `json:"summary"`
	Mounted bool   `json:"mounted,omitempty"`
}

// Server is the API.
type Server struct {
	opts Options
	mux  *http.ServeMux
	// UI answers non-API paths on the panel host; the lead sets the panel.
	UI http.Handler
	// Heartbeat is the SSE keep-alive interval.
	Heartbeat time.Duration

	mu     sync.Mutex
	routes []Route
	public map[string]bool
	// deployer, when set, is asked first by POST /sites/{domain}/deploy.
	deployer Deployer
	// repos, when set, is where a site made from a repository is read
	// (SetRepoSource).
	repos RepoSource
	// publisher, when set, publishes sites on creation and unpublishes
	// them on deletion (RegisterInbound sets it).
	publisher *publish.Publisher
	// moves are the sites moving to another node, by name.
	moves moves
	// repairs are the house's AI looking at arrived sites that do not start.
	repairs repairJobs
	// prepares are the installs of sites received from another node.
	prepares prepareJobs
	migrates prepareJobs
	// contains are the sites being put in a container, by name.
	contains containJobs
	// dnsEnabled says whether a DNS provider is on, which decides the
	// default of "publish" on creation.
	dnsEnabled func() bool
	// vulnerable, when set, says what is critical in a site's
	// dependencies; publishing reports it (RegisterAudit sets it).
	vulnerable func(domain string) string
	// onSettings runs after PUT /settings saved, with what was saved.
	onSettings []func(settings.Settings)
	// nodes, when set, are the installations linked to this one
	// (RegisterNodes sets it); summaries are the parts of /node/summary.
	mesh *nodes.Mesh
	// meshCanClose says whether the network's panels may be closed to
	// all but a VPN from here (this node has one, with a device).
	meshCanClose func() error
	summaries    map[string]Summarizer
	// loc is where the delegates' sites on other nodes were found
	// (delegate.go).
	loc locator
}

// Deployer is an alternative way to deploy a site, set with SetDeployer.
// It returns handled false to leave the request to the sites manager.
type Deployer func(domain string) (result any, handled bool, err error)

// Auth is the operator's credentials store the server answers with.
func (s *Server) Auth() *auth.Store { return s.opts.Auth }

// Sites is the sites manager the server works on.
func (s *Server) Sites() *sites.Manager { return s.opts.Sites }

// RepoSource reads a GitHub repository for a site made from it: the
// GitHub module, when it is on.
type RepoSource interface {
	Enabled() bool
	Inspect(ctx context.Context, ref string) (sites.Repo, error)
}

// SetRepoSource installs the RepoSource.
func (s *Server) SetRepoSource(r RepoSource) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repos = r
}

// SetDeployer installs a Deployer. The github module, when enabled,
// deploys by fetching the repository first; without it the manager's
// in-place build is what the route does.
func (s *Server) SetDeployer(d Deployer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deployer = d
}

// OnSettingsSaved registers fn to run after every successful PUT
// /settings, so a section another package owns is applied live by that
// package rather than by the one that wrote the file.
func (s *Server) OnSettingsSaved(fn func(settings.Settings)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onSettings = append(s.onSettings, fn)
}

// New builds the server with every route of this package registered.
func New(opts Options) *Server {
	if opts.Started.IsZero() {
		opts.Started = time.Now()
	}
	if opts.Bus == nil {
		opts.Bus = events.New("")
	}
	if opts.Settings == nil {
		d := settings.Defaults()
		opts.Settings = func() settings.Settings { return d }
	}
	s := &Server{opts: opts, mux: http.NewServeMux(), UI: opts.UI, Heartbeat: 15 * time.Second, public: map[string]bool{}}
	if s.UI == nil {
		s.UI = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprintf(w, "middleware api %s\n\nThe API is under %s; the OpenAPI document at %s/openapi.json.\n", opts.Version, Prefix, Prefix)
		})
	}
	s.registerAuth()
	s.registerUsers()
	s.registerSites()
	s.registerRelocateTarget()
	s.registerImportToken()
	s.registerEvents()
	s.registerSystem()
	s.registerPages()
	s.registerDocs()
	s.Handle("GET", Prefix+"/openapi.json", "The OpenAPI description of every registered route", s.openAPI)
	s.Public("GET", Prefix+"/openapi.json")
	return s
}

// Handle registers a JSON route. pattern is a Go 1.22 ServeMux pattern
// without the method, e.g. "/api/v1/sites/{domain}"; summary is one line
// for the OpenAPI document.
func (s *Server) Handle(method, pattern, summary string, h HandlerFunc) {
	method = strings.ToUpper(method)
	s.mu.Lock()
	s.routes = append(s.routes, Route{Method: method, Path: pattern, Summary: summary})
	s.mu.Unlock()
	s.mux.HandleFunc(method+" "+pattern, func(w http.ResponseWriter, r *http.Request) {
		v, err := h(w, r)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		if v == Handled {
			return
		}
		WriteJSON(w, http.StatusOK, v)
	})
}

// Mount hands a whole subtree to another handler: prefix and everything
// under it. The handler still sits behind authentication and the CSRF
// check; it does its own JSON. Modules and the other parts of the house
// plug in here.
func (s *Server) Mount(prefix string, h http.Handler) {
	prefix = strings.TrimSuffix(prefix, "/")
	s.mu.Lock()
	s.routes = append(s.routes, Route{Method: "ANY", Path: prefix + "/…", Mounted: true})
	s.mu.Unlock()
	s.mux.Handle(prefix, h)
	s.mux.Handle(prefix+"/", h)
}

// Public exempts a route from authentication: the setup, the login, the
// "who am I" and the OpenAPI document. Everything else needs a caller.
func (s *Server) Public(method, pattern string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.public[strings.ToUpper(method)+" "+pattern] = true
}

// Routes lists what is registered.
func (s *Server) Routes() []Route {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Route, len(s.routes))
	copy(out, s.routes)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Method < out[j].Method
	})
	return out
}

// Panel returns the handler for the panel host: the API under Prefix,
// the UI for everything else. It is what the hook puts on proxy.Panel
// and what the socket serves.
func (s *Server) Panel() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The address lists go before everything the panel answers, API
		// and pages alike: whoever cannot get in is not shown the box.
		// Except the site door, called by a site's own process: over the
		// containers' socket, which has no address for a list to read, or
		// over loopback, which a list need not name. The lists are about
		// people reaching the panel; the door answers a site's token and
		// nothing else, and a list used to shut every site out of it.
		path, door := SiteDoorPath(r.URL.Path)
		if s.opts.Auth != nil && !(door && auth.Direct(r)) && !s.opts.Auth.PanelAdmits(r) {
			s.writeError(w, r, Errorf(http.StatusForbidden, "this address may not reach the panel"))
			return
		}
		if door && path != r.URL.Path {
			r2 := r.Clone(r.Context())
			r2.URL.Path, r2.URL.RawPath = path, ""
			s.ServeHTTP(w, r2)
			return
		}
		if r.URL.Path == Prefix || strings.HasPrefix(r.URL.Path, Prefix+"/") {
			s.ServeHTTP(w, r)
			return
		}
		s.UI.ServeHTTP(w, r)
	})
}

// ServeHTTP implements http.Handler: resolve the caller, gate, dispatch.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	caller := auth.Caller{}
	if m, ok := nodes.MemberFrom(r.Context()); ok {
		// Set only by the mesh door, after it checked the tunnel address
		// and the secret; nothing a browser or the panel sends makes it.
		caller = auth.Caller{Via: auth.ViaNode, Token: m.Name}
		// A delegate the node relays is that delegate here too, for the
		// one site, and is held to the same lists as where they signed
		// in: this node is the one that knows the site's manifest, and
		// a node of an older release that never sends the header gets
		// nothing it did not have.
		if d, ok := delegateOf(r); ok {
			caller.User, caller.Grants, caller.Agents = d.User, map[string][]string{}, d.Agents
			if d.Site != "" {
				caller.Grants[d.Site] = d.Perms
			}
		}
	} else if s.opts.Auth != nil {
		caller = s.opts.Auth.Resolve(r)
	} else if auth.IsSocket(r.Context()) {
		caller = auth.SocketCaller(r.Context())
	}
	r = r.WithContext(auth.WithCaller(r.Context(), caller))
	_, pattern := s.mux.Handler(r)
	if pattern == "" {
		s.writeError(w, r, Errorf(http.StatusNotFound, "no such route"))
		return
	}
	s.mu.Lock()
	public := s.public[pattern]
	s.mu.Unlock()
	if !public && !caller.Authenticated() {
		s.writeError(w, r, Errorf(http.StatusUnauthorized, "authentication required"))
		return
	}
	// A scoped token is a service door: the route it asks for must be one
	// its scopes open, for the site in the path. Public routes are public
	// to it as to anyone.
	if !public && len(caller.Scopes) > 0 && !auth.Allows(caller.Scopes, pattern, siteIn(pattern, r.URL.Path)) && !turnReadsOwnEvents(caller, pattern, r) {
		if caller.Turn != "" {
			// An agent reaching for the operator's routes is worth knowing
			// about: it is what a prompt injected into a site's turn does.
			if s.opts.Bus != nil {
				s.opts.Bus.Emit(events.Event{Type: "api.turn_refused", Subject: caller.Turn, Actor: "agent", Level: "warn", Message: "a turn of this site's conversation asked the socket for " + pattern, Data: map[string]any{"method": r.Method, "path": r.URL.Path}})
			}
			s.writeError(w, r, Errorf(http.StatusForbidden, "a turn of %s's conversation may read, restart, deploy and exec in that site and manage its tasks and backups, and nothing else; %s is the operator's", caller.Turn, pattern))
			return
		}
		s.writeError(w, r, Errorf(http.StatusForbidden, "this token's scopes (%s) do not open %s", strings.Join(caller.Scopes, ", "), pattern))
		return
	}
	if !public && caller.Delegate() && !s.gateDelegate(w, r, caller, pattern) {
		return
	}
	if name, ok := s.fromNode(caller); ok && !nodeMay(pattern) {
		s.writeError(w, r, Errorf(http.StatusForbidden, "node %s may not use %s: the network and the credentials are decided at each machine's own panel", name, pattern))
		return
	}
	if caller.Via == auth.ViaCookie && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
		if err := sameSite(r); err != nil {
			s.writeError(w, r, Errorf(http.StatusForbidden, "%v", err))
			return
		}
	}
	if caller.Via == auth.ViaCookie && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions && r.Header.Get(CSRFHeader) != CSRFValue {
		s.writeError(w, r, Errorf(http.StatusForbidden, "cookie requests must send %s: %s", CSRFHeader, CSRFValue))
		return
	}
	// A delegate's site that is not here is answered by the node that
	// holds it. Only where they signed in: a relayed request is not
	// relayed again (nodeMay), and the node at the end is the one with
	// the site.
	if caller.Delegate() && caller.Via != auth.ViaNode {
		if site := relaySite(caller, pattern, r); site != "" && !s.localSite(site) && s.relay(w, r, caller, site) {
			return
		}
	}
	// Here, on the node that holds the site: the one whose processes
	// would run what is asked.
	if caller.Delegate() && auth.NeedsJail(pattern) && s.opts.Isolation != nil {
		if err := s.opts.Isolation(); err != nil {
			s.writeError(w, r, Errorf(http.StatusServiceUnavailable, "this server cannot isolate sites yet, so code and conversations are closed to delegated accounts; the owner sees why on the panel"))
			return
		}
	}
	s.mux.ServeHTTP(w, r)
}

// turnReadsOwnEvents lets a site's turn list the events about its own
// site, which the skill tells it to read (`middleware events --subject`):
// the list is filtered by that subject, so nothing of another site's
// comes back. The stream is not filtered and stays closed.
func turnReadsOwnEvents(c auth.Caller, pattern string, r *http.Request) bool {
	return c.Turn != "" && pattern == "GET "+Prefix+"/events" && r.URL.Query().Get("subject") == c.Turn
}

// siteIn is the {domain} of path under pattern, or "". The mux does not
// fill PathValue until it dispatches, and the scope check runs before.
func siteIn(pattern, path string) string {
	_, p, _ := strings.Cut(pattern, " ")
	ps, xs := strings.Split(p, "/"), strings.Split(path, "/")
	for i, seg := range ps {
		if seg == "{domain}" && i < len(xs) {
			return strings.ToLower(xs[i])
		}
	}
	return ""
}

// WriteJSON encodes v with the status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// ReadJSON decodes the request body into v, refusing unknown fields and
// bodies over a megabyte: a manifest is kilobytes and a token name is
// a line.
func ReadJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return Errorf(http.StatusBadRequest, "body: %v", err)
	}
	return nil
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	status, msg := statusOf(err)
	if status >= 500 {
		s.opts.Bus.Emit(events.Event{Type: "api.error", Actor: "api", Level: "error", Message: err.Error(), Data: map[string]any{"method": r.Method, "path": r.URL.Path}})
	}
	if status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "60")
	}
	WriteJSON(w, status, map[string]string{"error": msg})
}

// statusOf maps the errors of the packages below to statuses. Unknown
// errors are 500 with the message kept: this API talks to the operator,
// not to the public, and "internal error" is the one answer that helps
// nobody.
func statusOf(err error) (int, string) {
	var e *Error
	if errors.As(err, &e) {
		return e.Status, e.Message
	}
	switch {
	case errors.Is(err, sites.ErrNotFound), errors.Is(err, auth.ErrNotFound), errors.Is(err, os.ErrNotExist):
		return http.StatusNotFound, err.Error()
	case errors.Is(err, sites.ErrExists):
		return http.StatusConflict, err.Error()
	case errors.Is(err, sites.ErrDeploying):
		return http.StatusConflict, err.Error()
	case errors.Is(err, sites.ErrOutside), errors.Is(err, sites.ErrForbidden):
		return http.StatusForbidden, err.Error()
	case errors.Is(err, sites.ErrBadDomain), errors.Is(err, sites.ErrBadTemplate), errors.Is(err, sites.ErrInvalid), errors.Is(err, sites.ErrUnmanaged), errors.Is(err, sites.ErrTooLarge), errors.Is(err, auth.ErrWeakPassword), errors.Is(err, auth.ErrTokenNameNeeded):
		return http.StatusBadRequest, err.Error()
	case errors.Is(err, auth.ErrNoPasskey):
		return http.StatusNotFound, err.Error()
	case errors.Is(err, auth.ErrLastPasskey):
		return http.StatusConflict, err.Error()
	case errors.Is(err, auth.ErrBadPasskeyName):
		return http.StatusBadRequest, err.Error()
	case errors.Is(err, auth.ErrBadCredentials), errors.Is(err, auth.ErrBadSetupCode), errors.Is(err, auth.ErrPasskeyFailed):
		return http.StatusUnauthorized, err.Error()
	case errors.Is(err, auth.ErrNoOperator):
		return http.StatusForbidden, err.Error()
	case errors.Is(err, auth.ErrRateLimited):
		return http.StatusTooManyRequests, err.Error()
	case errors.Is(err, context.Canceled):
		return 499, "client went away"
	}
	msg := err.Error()
	// A body cut by the WAF's max_body halfway through (it streams, so
	// Content-Length did not say it first) came out of whatever was reading
	// it as a 500 "request body too large". It is the sender's size against
	// a limit the operator set, and the answer says which and where.
	var mb *http.MaxBytesError
	if errors.As(err, &mb) || strings.Contains(msg, "http: request body too large") {
		limit := "its limit"
		if mb != nil {
			limit = fmt.Sprintf("%d MB", mb.Limit>>20)
		}
		return http.StatusRequestEntityTooLarge, "the request body passed the WAF's max_body (" + limit + "): raise it in the panel, Security → WAF (max_body, for every site and the panel), or in the site's manifest (waf.max_body)"
	}
	if strings.HasPrefix(msg, "manifest:") || strings.HasPrefix(msg, "body:") {
		return http.StatusBadRequest, msg
	}
	return http.StatusInternalServerError, msg
}

// domainOf reads the {domain} path value and validates it, so no handler
// below ever builds a path from an unchecked name.
func domainOf(r *http.Request) (string, error) {
	d := strings.ToLower(strings.TrimSpace(r.PathValue("domain")))
	if !manifest.ValidName(d) {
		return "", Errorf(http.StatusBadRequest, "%q is not a site or project", d)
	}
	return d, nil
}

// openAPI describes the registered routes. It is generated from the
// registrations rather than written by hand so it cannot drift from what
// the server answers.
func (s *Server) openAPI(w http.ResponseWriter, r *http.Request) (any, error) {
	paths := map[string]map[string]any{}
	for _, rt := range s.Routes() {
		if rt.Mounted {
			paths[rt.Path] = map[string]any{"x-mounted": true, "description": "a subtree served by another component"}
			continue
		}
		ops, ok := paths[rt.Path]
		if !ok {
			ops = map[string]any{}
			paths[rt.Path] = ops
		}
		op := map[string]any{
			"summary":     rt.Summary,
			"operationId": operationID(rt.Method, rt.Path),
			"responses":   map[string]any{"200": map[string]any{"description": "OK"}, "default": map[string]any{"description": "{\"error\": message}"}},
		}
		if params := pathParams(rt.Path); len(params) > 0 {
			op["parameters"] = params
		}
		ops[strings.ToLower(rt.Method)] = op
	}
	return map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{
			"title":       "middleware",
			"version":     s.opts.Version,
			"description": "The API of one middleware installation. Authenticate with `Authorization: Bearer <token>`, the `mw_session` cookie (plus `" + CSRFHeader + ": " + CSRFValue + "` on non-GET requests), or the unix socket data/api.sock.",
		},
		"servers": []map[string]any{{"url": "/"}},
		"paths":   paths,
		"components": map[string]any{"securitySchemes": map[string]any{
			"bearer": map[string]any{"type": "http", "scheme": "bearer"},
			"cookie": map[string]any{"type": "apiKey", "in": "cookie", "name": auth.CookieName},
		}},
		"security": []map[string]any{{"bearer": []string{}}, {"cookie": []string{}}},
	}, nil
}

func pathParams(p string) []map[string]any {
	var out []map[string]any
	for _, seg := range strings.Split(p, "/") {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			name := strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(seg, "{"), "}"), "...")
			out = append(out, map[string]any{"name": name, "in": "path", "required": true, "schema": map[string]string{"type": "string"}})
		}
	}
	return out
}

func operationID(method, p string) string {
	p = strings.TrimPrefix(p, Prefix)
	var b strings.Builder
	b.WriteString(strings.ToLower(method))
	for _, seg := range strings.Split(p, "/") {
		seg = strings.Trim(seg, "{}.")
		if seg == "" {
			continue
		}
		b.WriteString(strings.ToUpper(seg[:1]))
		b.WriteString(strings.ReplaceAll(seg[1:], "-", ""))
	}
	return b.String()
}
