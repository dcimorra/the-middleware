// Package publish makes a site reachable from outside and takes it back.
//
// Publishing and moving a name between doors are the same operation seen
// from two places, and V1 learned it the hard way: while they were two
// pieces of code, publishing with the ports door open wrote the A record
// and *left* the tunnel rule, so the name was published by both doors and
// served by whichever won. Here there is one Publisher, and the API's move,
// a site's creation and its deletion all go through it, under one lock,
// because the tunnel's ingress is one object that every write replaces.
//
// It is a reconcile loop like the engine. A site is published by writing
// the intent (data/publish.json: this site, on this door); every name the
// site answers to (manifest.Hosts) is then brought to that door and kept
// there: an alias added later is published on the next pass, one removed is
// unpublished. A name that fails is retried with backoff and its error is
// shown, never swallowed. The file also records what the house itself
// wrote, so unpublishing only ever removes the house's own records.
//
// What is never done on a guess: a site folder that disappears without
// being deleted through the house keeps its records (a folder renamed by
// hand would otherwise lose its DNS), and an event says so once.
package publish

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/inbound"
)

// PanelOwner is the owner the panel's own name is published under: not a
// domain and not a project name (neither starts with "@"), so it cannot
// be taken by a site.
const PanelOwner = "@panel"

// DNS is the provider of records and tunnel routes: the cloudflare module.
type DNS interface {
	// Covers says whether the provider manages the zone a name is in.
	Covers(ctx context.Context, name string) (bool, error)
	// Publish points name at this house through door ("tunnel"/"ports").
	Publish(ctx context.Context, name, door, ip string) error
	// Unpublish removes the route and the records that point at this
	// house (the tunnel, or ip).
	Unpublish(ctx context.Context, name, ip string) error
}

// Inspector is a provider that can say what a name already points at
// that is not this house; empty is nothing, or only this house.
type Inspector interface {
	Foreign(ctx context.Context, name string, ips ...string) (string, error)
}

// Doors is what the inbound hook offers: certificates and recorded doors.
type Doors interface {
	Move(ctx context.Context, domains []string, to inbound.Door) []inbound.Line
	State() inbound.State
	Forget(domain string) error
}

// Options configure a Publisher.
type Options struct {
	Root  string
	Bus   *events.Bus
	Doors Doors
	// DNS returns the provider, or nil when none is enabled. It is a
	// function because the module can be enabled while the process runs.
	DNS func() DNS
	// Hosts returns every name → the site that owns it.
	Hosts func() map[string]string
	// PublicIP detects the address the ports door is reached at; the
	// inbound state's public_ip, when set, wins over it.
	PublicIP func(ctx context.Context) (string, error)
	// VPNOnly says whether a site exists only inside the VPN. Such a site
	// is never published, and one that becomes VPN-only is taken down: a
	// public record would point the world at a door that answers it 404,
	// and tell it the name exists.
	VPNOnly func(site string) bool
	// Settle is how long Publish waits for the engine to see a site that
	// answers no name yet (10s when zero).
	Settle time.Duration
	Tick   time.Duration
	Now    func() time.Time
}

// Status of one name.
const (
	StatePublished = "published"
	StatePending   = "pending"
	StateFailed    = "failed"
	// StateManual is a name the house cannot point here by itself: no
	// DNS provider, or a zone the provider does not manage.
	StateManual = "manual"
	// StateRetiring is a name being unpublished that has not finished.
	StateRetiring = "retiring"
	// StateConflict is a name whose zone already points it somewhere
	// else (Record.Found): nothing is written until the operator says
	// to take it (Takeover).
	StateConflict = "conflict"
)

// Record is what the house knows about one published name.
type Record struct {
	Site     string       `json:"site"`
	Door     inbound.Door `json:"door"`
	State    string       `json:"state"`
	Error    string       `json:"error,omitempty"`
	Attempts int          `json:"attempts,omitempty"`
	Next     time.Time    `json:"next,omitzero"`
	At       time.Time    `json:"at,omitzero"`
	// IP is the A record's address when published on the ports door, so
	// unpublishing knows which record is the house's.
	IP string `json:"ip,omitempty"`
	// Found is what the zone had on the name, pointing elsewhere, when it
	// was found in conflict; Takeover is the operator's yes to replacing
	// it.
	Found    string `json:"found,omitempty"`
	Takeover bool   `json:"takeover,omitempty"`
}

// Intent is a site the operator published.
type Intent struct {
	Door  inbound.Door `json:"door"`
	Since time.Time    `json:"since"`
	// Suspended: the site went inside the VPN. Its public names are taken
	// down but the intent and its door are kept, so that leaving the VPN
	// publishes it again where it was. Forgetting the intent (as an
	// unpublish does) left a site that came out of the VPN with no record
	// anywhere, and nothing that would ever write one.
	Suspended bool `json:"suspended,omitempty"`
}

type file struct {
	Sites map[string]Intent  `json:"sites"`
	Names map[string]*Record `json:"names"`
}

// Publisher is the one place names are published and unpublished.
type Publisher struct {
	opts Options
	// work serializes every operation that writes DNS or tunnel routes:
	// the ingress is one object that each write replaces. It is held
	// across network calls (a certificate can take a minute).
	work sync.Mutex
	// mu guards f and is only held for moments, so reading the state
	// never waits for a certificate.
	mu      sync.Mutex
	f       file
	orphans map[string]bool // said once, guarded by work

	ipMu   sync.Mutex
	ip     string
	ipTime time.Time

	kick chan struct{}
}

// New loads data/publish.json.
func New(opts Options) (*Publisher, error) {
	if opts.Settle == 0 {
		opts.Settle = 10 * time.Second
	}
	if opts.Tick == 0 {
		opts.Tick = time.Minute
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Bus == nil {
		opts.Bus = events.New("")
	}
	if opts.PublicIP == nil {
		opts.PublicIP = DetectPublicIP
	}
	p := &Publisher{opts: opts, orphans: map[string]bool{}, kick: make(chan struct{}, 1)}
	p.f = file{Sites: map[string]Intent{}, Names: map[string]*Record{}}
	if err := fsx.ReadJSON(p.path(), &p.f); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if p.f.Sites == nil {
		p.f.Sites = map[string]Intent{}
	}
	if p.f.Names == nil {
		p.f.Names = map[string]*Record{}
	}
	return p, nil
}

func (p *Publisher) path() string { return filepath.Join(p.opts.Root, "data", "publish.json") }

func (p *Publisher) saveLocked() error { return fsx.WriteJSON(p.path(), p.f, 0o644) }

func (p *Publisher) emit(typ, subject, level, msg string, data map[string]any) {
	p.opts.Bus.Emit(events.Event{Type: typ, Subject: subject, Actor: "engine", Level: level, Message: msg, Data: data})
}

// Run reconciles until ctx ends.
func (p *Publisher) Run(ctx context.Context) {
	t := time.NewTicker(p.opts.Tick)
	defer t.Stop()
	for {
		p.Reconcile(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-p.kick:
		}
	}
}

func (p *Publisher) wake() {
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

// Publish records that site is published on door ("" is the door for new
// sites) and brings its names there now. The lines say what happened to
// each name; a failure is retried by the loop.
func (p *Publisher) Publish(ctx context.Context, site string, door inbound.Door) ([]inbound.Line, error) {
	if door == "" {
		door = p.opts.Doors.State().DoorForNew()
	}
	if _, err := inbound.ParsePublicDoor(string(door)); err != nil {
		return nil, err
	}
	if p.opts.VPNOnly != nil && p.opts.VPNOnly(site) {
		return nil, fmt.Errorf("%s exists only inside the VPN (its manifest's vpn): it is never published", site)
	}
	hosts := p.settledHostsOf(ctx, site)
	if len(hosts) == 0 {
		return nil, fmt.Errorf("%s answers no name: a project, or not a site of this installation", site)
	}
	p.work.Lock()
	defer p.work.Unlock()
	p.mu.Lock()
	p.f.Sites[site] = Intent{Door: door, Since: p.opts.Now()}
	for _, h := range hosts {
		r := p.f.Names[h]
		if r == nil || r.Door != door || r.State == StateRetiring {
			takeover := r != nil && r.Takeover
			p.f.Names[h] = &Record{Site: site, Door: door, State: StatePending, Takeover: takeover}
		} else {
			r.Next = time.Time{}
		}
	}
	if err := p.saveLocked(); err != nil {
		p.mu.Unlock()
		return nil, err
	}
	p.mu.Unlock()
	p.emit("publish.requested", site, "", "on the "+string(door)+" door", map[string]any{"names": hosts})
	return p.reconcileSiteLocked(ctx, site), nil
}

// PublishOver is Publish for a site that arrives from another node of the
// network: its names point at that node, and taking them over is the
// point of the move, so no name of it stops in conflict.
func (p *Publisher) PublishOver(ctx context.Context, site string, door inbound.Door) ([]inbound.Line, error) {
	p.mu.Lock()
	for _, h := range p.hostsOf(site) {
		if r := p.f.Names[h]; r != nil {
			r.Takeover = true
		} else {
			p.f.Names[h] = &Record{Site: site, Takeover: true}
		}
	}
	p.mu.Unlock()
	return p.Publish(ctx, site, door)
}

// Unpublish takes every name of site off the internet and forgets the
// intent. A name that cannot be removed now stays as retiring and the
// loop keeps trying: a deleted site's record must not linger silently.
func (p *Publisher) Unpublish(ctx context.Context, site string) []inbound.Line {
	p.work.Lock()
	defer p.work.Unlock()
	p.mu.Lock()
	delete(p.f.Sites, site)
	for _, r := range p.f.Names {
		if r.Site == site {
			r.State, r.Next = StateRetiring, time.Time{}
		}
	}
	_ = p.saveLocked()
	p.mu.Unlock()
	p.emit("publish.withdrawn", site, "", "unpublishing every name", nil)
	return p.reconcileSiteLocked(ctx, site)
}

// Renamed hands what the house published for site from to site to: its
// intent and its names' records, without touching a record or a route.
// A site renamed to one of its own aliases answers the same names after
// as before; unpublishing and publishing again took every one of them
// off the internet for as long as DNS took to come back, for a change
// that does not change where any of them points.
func (p *Publisher) Renamed(from, to string) {
	p.work.Lock()
	defer p.work.Unlock()
	p.mu.Lock()
	moved := false
	if in, ok := p.f.Sites[from]; ok {
		p.f.Sites[to] = in
		delete(p.f.Sites, from)
		moved = true
	}
	for _, r := range p.f.Names {
		if r.Site == from {
			r.Site = to
			moved = true
		}
	}
	if moved {
		_ = p.saveLocked()
	}
	p.mu.Unlock()
	delete(p.orphans, from)
	if moved {
		// No wake: the engine sees the renamed folder on its next scan,
		// and a pass before that would call the site an orphan.
		p.emit("publish.renamed", to, "", from+" is now "+to+"; its names stay where they are", map[string]any{"from": from})
	}
}

// Move puts names on a door: the API's move, which is publishing names
// one by one. The names' site is published on that door from now on, so
// the loop keeps them there.
func (p *Publisher) Move(ctx context.Context, names []string, door inbound.Door) []inbound.Line {
	if _, err := inbound.ParsePublicDoor(string(door)); err != nil {
		return []inbound.Line{{Status: "fail", Text: err.Error()}}
	}
	owners := p.opts.Hosts()
	var lines []inbound.Line
	sites := map[string]bool{}
	p.work.Lock()
	defer p.work.Unlock()
	p.mu.Lock()
	for _, n := range names {
		n = strings.ToLower(strings.TrimSpace(n))
		site, ok := owners[n]
		if !ok {
			lines = append(lines, inbound.Line{Status: "fail", Text: n + ": not a name of any site here"})
			continue
		}
		p.f.Names[n] = &Record{Site: site, Door: door, State: StatePending}
		sites[site] = true
		// Moving a name is publishing it: a site with no intent yet gets
		// one on this door, or the next pass would see a name nobody
		// asked for and take it down again. A site already published
		// keeps its door for its other names; this one keeps its own.
		if _, ok := p.f.Sites[site]; !ok {
			p.f.Sites[site] = Intent{Door: door, Since: p.opts.Now()}
		}
	}
	_ = p.saveLocked()
	p.mu.Unlock()
	for site := range sites {
		lines = append(lines, p.reconcileSiteLocked(ctx, site)...)
	}
	return lines
}

// Records returns what the house published, by name.
func (p *Publisher) Records() map[string]Record {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]Record, len(p.f.Names))
	for n, r := range p.f.Names {
		out[n] = *r
	}
	return out
}

// Published reports whether site is published and on which door.
func (p *Publisher) Published(site string) (inbound.Door, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	in, ok := p.f.Sites[site]
	return in.Door, ok && !in.Suspended
}

// Covers says which of names this house can publish by itself (its DNS
// provider manages their zone) and which it cannot. With no provider, it
// can publish none.
func (p *Publisher) Covers(ctx context.Context, names []string) (yes, no []string, err error) {
	dns := p.dns()
	for _, n := range names {
		if dns == nil {
			no = append(no, n)
			continue
		}
		ok, err := dns.Covers(ctx, n)
		if err != nil {
			return nil, nil, err
		}
		if ok {
			yes = append(yes, n)
		} else {
			no = append(no, n)
		}
	}
	return yes, no, nil
}

// HostsOf is every name site answers to now.
func (p *Publisher) HostsOf(site string) []string { return p.hostsOf(site) }

// settledHostsOf is hostsOf, waiting up to Settle for a site whose folder
// was just put in place: the engine sees it on its next scan, and the
// publish that follows a commit or a create arrives before that. An import
// of a site that serves files is ready the moment its folder is there, so
// its names were refused with "answers no name" and left pointing at the
// node it had just been stopped on (ddev.es and dcimorra.es, October
// 2026). Each caller had its own wait loop before; the import had none.
func (p *Publisher) settledHostsOf(ctx context.Context, site string) []string {
	deadline := time.Now().Add(p.opts.Settle)
	for {
		hosts := p.hostsOf(site)
		if len(hosts) > 0 || !time.Now().Before(deadline) {
			return hosts
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (p *Publisher) hostsOf(site string) []string {
	var out []string
	for h, owner := range p.opts.Hosts() {
		if owner == site {
			out = append(out, h)
		}
	}
	sort.Strings(out)
	return out
}

// Reconcile is one pass over every published site.
func (p *Publisher) Reconcile(ctx context.Context) {
	p.mu.Lock()
	sites := map[string]bool{}
	for s := range p.f.Sites {
		sites[s] = true
	}
	for _, r := range p.f.Names {
		sites[r.Site] = true
	}
	p.mu.Unlock()
	names := make([]string, 0, len(sites))
	for s := range sites {
		names = append(names, s)
	}
	sort.Strings(names)
	for _, s := range names {
		if ctx.Err() != nil {
			return
		}
		p.mu.Lock()
		in, intends := p.f.Sites[s]
		vpn := p.opts.VPNOnly != nil && p.opts.VPNOnly(s)
		switch {
		case intends && vpn && !in.Suspended:
			in.Suspended = true
			p.f.Sites[s] = in
			_ = p.saveLocked()
			p.mu.Unlock()
			p.emit("publish.vpn_only", s, "warn", "the site is VPN-only now: its public names are taken down until it leaves the VPN", nil)
		case intends && !vpn && in.Suspended:
			// Out of the VPN: back on the door it was on.
			in.Suspended = false
			p.f.Sites[s] = in
			for _, h := range p.hostsOf(s) {
				p.f.Names[h] = &Record{Site: s, Door: in.Door, State: StatePending}
			}
			_ = p.saveLocked()
			p.mu.Unlock()
			p.emit("publish.requested", s, "", "out of the VPN: published again on the "+string(in.Door)+" door", nil)
		default:
			p.mu.Unlock()
		}
		p.work.Lock()
		p.reconcileSiteLocked(ctx, s)
		p.work.Unlock()
	}
}

// reconcileSiteLocked brings one site's names to where its intent says.
// The caller holds p.work; p.mu is taken only around the file.
func (p *Publisher) reconcileSiteLocked(ctx context.Context, site string) []inbound.Line {
	owners := p.opts.Hosts()
	exists := false
	current := map[string]bool{}
	for h, owner := range owners {
		if owner == site {
			exists = true
			current[h] = true
		}
	}
	p.mu.Lock()
	intent, published := p.f.Sites[site]
	if intent.Suspended {
		// Kept, not published: its names are retired below.
		published = false
		exists = true
	}
	if published && !exists {
		p.mu.Unlock()
		if !p.orphans[site] {
			p.orphans[site] = true
			p.emit("publish.orphan", site, "warn", "published, but no site answers to it any more; its records are kept until it is deleted through the house or unpublished", nil)
		}
		return nil
	}
	delete(p.orphans, site)
	if published {
		for h := range current {
			r := p.f.Names[h]
			if r == nil {
				p.f.Names[h] = &Record{Site: site, Door: intent.Door, State: StatePending}
			} else if r.Site != site {
				// A name that moved from one site to another (an alias
				// passed on) is the new owner's from now on.
				r.Site = site
			}
		}
	}
	now := p.opts.Now()
	var todo []string
	for h, r := range p.f.Names {
		if r.Site != site {
			continue
		}
		if (!published || !current[h]) && r.State != StateRetiring {
			r.State, r.Next = StateRetiring, time.Time{}
		}
		if r.State == StatePublished || r.State == StateManual || r.State == StateConflict || now.Before(r.Next) {
			continue
		}
		todo = append(todo, h)
	}
	sort.Strings(todo)
	work := make(map[string]Record, len(todo))
	for _, h := range todo {
		work[h] = *p.f.Names[h]
	}
	p.mu.Unlock()

	var lines []inbound.Line
	for _, h := range todo {
		rec := work[h]
		var line inbound.Line
		retired := false
		if rec.State == StateRetiring {
			line, retired = p.retire(ctx, h, &rec)
		} else {
			line = p.publish(ctx, h, &rec)
		}
		lines = append(lines, line)
		p.mu.Lock()
		if retired {
			delete(p.f.Names, h)
		} else {
			p.f.Names[h] = &rec
		}
		p.mu.Unlock()
	}
	p.mu.Lock()
	_ = p.saveLocked()
	p.mu.Unlock()
	return lines
}

// publish puts one name on its door: certificate first for the ports
// door (a name pointed at a door that cannot serve it is an outage), then
// DNS. The tunnel door needs no certificate of ours.
func (p *Publisher) publish(ctx context.Context, name string, r *Record) inbound.Line {
	dns := p.dns()
	if dns == nil {
		return p.manual(name, r, "no DNS provider is enabled: point "+name+" at this house by hand, or enable the cloudflare module")
	}
	covered, err := dns.Covers(ctx, name)
	if err != nil {
		return p.failed(name, r, err)
	}
	if !covered {
		return p.manual(name, r, name+" is in a zone the DNS provider does not manage: point it at this house by hand")
	}
	ip := ""
	if r.Door == inbound.Ports {
		if ip, err = p.publicIP(ctx); err != nil {
			return p.failed(name, r, err)
		}
	}
	if in, ok := dns.(Inspector); ok && !r.Takeover {
		// What points at this house is the house's to move (another door,
		// an address it published before): only what points elsewhere
		// stops here. Before the certificate, which would be asked for a
		// name the operator may not give.
		now := ip
		if now == "" {
			now, _ = p.publicIP(ctx)
		}
		found, err := in.Foreign(ctx, name, now, r.IP)
		if err != nil {
			return p.failed(name, r, err)
		}
		if found != "" {
			return p.conflict(name, r, found)
		}
	}
	for _, l := range p.opts.Doors.Move(ctx, []string{name}, r.Door) {
		if l.Status == "fail" {
			return p.failed(name, r, errors.New(strings.TrimPrefix(l.Text, name+": ")))
		}
	}
	if err := dns.Publish(ctx, name, string(r.Door), ip); err != nil {
		return p.failed(name, r, err)
	}
	r.State, r.Error, r.Attempts, r.Next, r.At, r.IP = StatePublished, "", 0, time.Time{}, p.opts.Now(), ip
	r.Found, r.Takeover = "", false
	p.emit("publish.published", name, "", "on the "+string(r.Door)+" door", map[string]any{"site": r.Site})
	return inbound.Line{Status: "ok", Text: name + ": published on the " + string(r.Door) + " door"}
}

func (p *Publisher) retire(ctx context.Context, name string, r *Record) (inbound.Line, bool) {
	if dns := p.dns(); dns != nil {
		ip := r.IP
		if ip == "" && r.Door == inbound.Ports {
			ip, _ = p.publicIP(ctx)
		}
		covered, err := dns.Covers(ctx, name)
		if err != nil {
			return p.failed(name, r, err), false
		}
		if covered {
			if err := dns.Unpublish(ctx, name, ip); err != nil {
				return p.failed(name, r, err), false
			}
		}
	}
	_ = p.opts.Doors.Forget(name)
	p.emit("publish.unpublished", name, "", "route and records removed", map[string]any{"site": r.Site})
	return inbound.Line{Status: "ok", Text: name + ": unpublished"}, true
}

func (p *Publisher) conflict(name string, r *Record, found string) inbound.Line {
	why := name + " already points elsewhere (" + found + "): nothing was changed; take it to point it at this site"
	if r.State != StateConflict || r.Found != found {
		p.emit("publish.conflict", name, "warn", why, map[string]any{"site": r.Site, "found": found})
	}
	r.State, r.Error, r.Found, r.Next = StateConflict, why, found, time.Time{}
	return inbound.Line{Status: "skip", Text: why}
}

// Takeover is the operator's yes to replacing what the zone has on names
// found in conflict: they are published on their site's door now, over
// whatever they pointed at. A name not in conflict is left as it is.
func (p *Publisher) Takeover(ctx context.Context, names []string) []inbound.Line {
	p.work.Lock()
	defer p.work.Unlock()
	p.mu.Lock()
	sites := map[string]bool{}
	var lines []inbound.Line
	for _, n := range names {
		n = strings.ToLower(strings.TrimSpace(n))
		r := p.f.Names[n]
		if r == nil || r.State != StateConflict {
			lines = append(lines, inbound.Line{Status: "skip", Text: n + ": not in conflict"})
			continue
		}
		r.Takeover, r.State, r.Next = true, StatePending, time.Time{}
		sites[r.Site] = true
		p.emit("publish.takeover", n, "warn", "the operator took "+n+" from "+r.Found, map[string]any{"site": r.Site, "found": r.Found})
	}
	_ = p.saveLocked()
	p.mu.Unlock()
	for s := range sites {
		lines = append(lines, p.reconcileSiteLocked(ctx, s)...)
	}
	return lines
}

func (p *Publisher) manual(name string, r *Record, why string) inbound.Line {
	if r.State != StateManual || r.Error != why {
		p.emit("publish.manual", name, "warn", why, map[string]any{"site": r.Site})
	}
	r.State, r.Error, r.Next = StateManual, why, time.Time{}
	return inbound.Line{Status: "skip", Text: name + ": " + why}
}

// failed records an error with backoff: 1, 2, 4… minutes up to an hour.
func (p *Publisher) failed(name string, r *Record, err error) inbound.Line {
	r.Attempts++
	wait := time.Minute
	for i := 1; i < r.Attempts && wait < time.Hour; i++ {
		wait *= 2
	}
	if wait > time.Hour {
		wait = time.Hour
	}
	if r.State != StateRetiring {
		r.State = StateFailed
	}
	r.Error, r.Next = err.Error(), p.opts.Now().Add(wait)
	p.emit("publish.failed", name, "error", fmt.Sprintf("%v; retry in %s", err, wait), map[string]any{"site": r.Site, "attempts": r.Attempts})
	return inbound.Line{Status: "fail", Text: fmt.Sprintf("%s: %v (retry in %s)", name, err, wait)}
}

func (p *Publisher) dns() DNS {
	if p.opts.DNS == nil {
		return nil
	}
	return p.opts.DNS()
}

// Retry clears the backoff of every name that is not published, and of
// the names in manual state, so the next pass tries them again: what the
// operator presses after fixing a token or a zone.
func (p *Publisher) Retry() {
	p.work.Lock()
	defer p.work.Unlock()
	p.mu.Lock()
	for _, r := range p.f.Names {
		if r.State != StatePublished {
			if r.State == StateManual || r.State == StateConflict {
				// A conflict is looked at again, not taken: if the zone
				// still points elsewhere it comes back as one.
				r.State = StatePending
			}
			r.Next = time.Time{}
		}
	}
	_ = p.saveLocked()
	p.mu.Unlock()
	p.wake()
}

// publicIP is the override from the inbound state, or the detected one,
// kept ten minutes.
func (p *Publisher) publicIP(ctx context.Context) (string, error) {
	if ip := p.opts.Doors.State().PublicIP; ip != "" {
		return ip, nil
	}
	p.ipMu.Lock()
	defer p.ipMu.Unlock()
	if p.ip != "" && p.opts.Now().Sub(p.ipTime) < 10*time.Minute {
		return p.ip, nil
	}
	ip, err := p.opts.PublicIP(ctx)
	if err != nil {
		return "", fmt.Errorf("the public address of this machine is not known (%v): set it with PUT /inbound/publishing", err)
	}
	if parsed := net.ParseIP(ip); parsed == nil || parsed.To4() == nil {
		return "", fmt.Errorf("the detected public address %q is not IPv4: set it with PUT /inbound/publishing", ip)
	}
	p.ip, p.ipTime = ip, p.opts.Now()
	return ip, nil
}
