// Package browsers is the node's pool of browsers that the sites take in
// turn: a fixed number of headless Chromiums, each held by one site at a
// time, that its conversation (or its code) drives over the Chrome
// DevTools Protocol to see what the site does.
//
// Before, an agent that wanted to look at its site started its own
// browser, or several, and left them running: V1 found headless browsers
// days old on luma32.com, one of them on more than a core (chat.MarkVar).
// A browser is the most expensive thing a turn can start, so the house
// keeps a number of them the machine can hold (settings.Browsers, with a
// recommended number), and a site says when it takes one and when it gives
// it back, which is what the panel shows.
//
// A lease is written to disk: a restart of the supervisor does not take a
// browser away from the site that holds it. The browser itself does not
// survive one (it talks to the house over a pipe, which closes), and the
// next connection starts a fresh one in its place.
//
// Each browser listens on nothing. It is started with
// --remote-debugging-pipe and the house relays a websocket on a loopback
// port to that pipe, at a path that carries the lease's token: a debugging
// port of its own would be open to every process on the machine, which is
// every site, and whoever reaches a browser's port owns the browser and
// every page it has open. Through the relay only the holder gets in, and
// giving it back ends the browser and its profile, so the next site finds
// none of the last one's cookies.
package browsers

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/fsx"
)

// Dir is the pool's folder under the data directory.
const Dir = "browsers"

// MarkVar is in every browser's environment, with the pool's folder as
// its value: what a supervisor that starts again finds and ends, since a
// browser whose pipe closed with the old process may not have noticed.
const MarkVar = "MIDDLEWARE_BROWSER_POOL"

// MaxFor is the length of what a site says it takes a browser for.
const MaxFor = 200

// Options configure a Pool.
type Options struct {
	// Dir holds the leases and each browser's profile and output.
	Dir string
	Bus *events.Bus
	// Binary is the browser to start; "" is none on this machine.
	Binary func() string
	// Count is the pool's size now (settings.Browsers.In).
	Count func() int
	// Idle is how long a lease may go without a command from its holder.
	Idle func() time.Duration
	// Listen is where the relay answers; empty is a loopback port, the
	// one the last process had when it is free.
	Listen string
	// Now is the clock; tests replace it.
	Now func() time.Time
}

// Lease is a browser held by a site.
type Lease struct {
	Slot int    `json:"slot"`
	Site string `json:"site"`
	// For is what the site said it takes it for.
	For   string    `json:"for,omitempty"`
	Token string    `json:"token"`
	Since time.Time `json:"since"`
	// Used is the last command its holder sent.
	Used time.Time `json:"used"`
}

// Slot is one browser of the pool as a caller sees it.
type Slot struct {
	Slot  int        `json:"slot"`
	Free  bool       `json:"free"`
	Site  string     `json:"site,omitempty"`
	For   string     `json:"for,omitempty"`
	Since *time.Time `json:"since,omitempty"`
	Used  *time.Time `json:"used,omitempty"`
	// Running is a browser process alive behind the lease; a lease
	// without one starts it at the next connection.
	Running bool `json:"running"`
	// Connected is a client on the relay now.
	Connected bool `json:"connected"`
	// Leaving is a taken browser above the pool's size, after the
	// operator made it smaller: it goes when it is given back.
	Leaving bool `json:"leaving,omitempty"`
	// Endpoint is the DevTools websocket, for its holder only.
	Endpoint string `json:"endpoint,omitempty"`
	// Expires is when the house takes it back if nothing is sent.
	Expires *time.Time `json:"expires,omitempty"`
}

// The errors a take or a give-back can end in.
var (
	ErrNone      = errors.New("this node keeps no browsers for its sites: the operator sets how many in Settings, Browsers")
	ErrNoBinary  = errors.New("no headless Chromium on this machine: run the installer again and answer yes to the browser (middleware install --browser yes), or set MIDDLEWARE_CHROME")
	ErrNotTaken  = errors.New("that browser is not taken")
	ErrNotYours  = errors.New("that browser is held by another site: only its holder or the operator gives it back")
	ErrNoSuchOne = errors.New("this node has no browser with that number")
)

// BusyError is a take with every browser held.
type BusyError struct{ Slots []Slot }

func (e *BusyError) Error() string {
	var b strings.Builder
	b.WriteString("every browser of this node is taken: ")
	for i, s := range e.Slots {
		if i > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "%d by %s", s.Slot, s.Site)
		if s.Since != nil {
			b.WriteString(" since " + s.Since.Format("15:04"))
		}
		if s.For != "" {
			b.WriteString(" (" + s.For + ")")
		}
	}
	b.WriteString(". Try again when one is given back")
	return b.String()
}

// Pool is the node's browsers.
type Pool struct {
	opts   Options
	ln     net.Listener
	mu     sync.Mutex
	leases map[int]*Lease
	live   map[int]*browser
	dirty  bool
}

type state struct {
	Port   int     `json:"port,omitempty"`
	Leases []Lease `json:"leases"`
}

// New reads the leases and opens the relay's port. Browsers left by a
// supervisor that went away are ended here.
func New(opts Options) (*Pool, error) {
	if opts.Bus == nil {
		opts.Bus = events.New("")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Binary == nil {
		opts.Binary = func() string { return "" }
	}
	if opts.Count == nil {
		opts.Count = func() int { return 0 }
	}
	if opts.Idle == nil {
		opts.Idle = func() time.Duration { return 15 * time.Minute }
	}
	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		return nil, err
	}
	p := &Pool{opts: opts, leases: map[int]*Lease{}, live: map[int]*browser{}}
	var st state
	if err := fsx.ReadJSON(p.statePath(), &st); err != nil && !os.IsNotExist(err) {
		opts.Bus.Emit(events.Event{Type: "browsers.error", Actor: "engine", Level: "warn", Message: "the browsers' leases could not be read, every browser is free: " + err.Error()})
	}
	for _, l := range st.Leases {
		l := l
		if l.Slot > 0 && l.Site != "" && l.Token != "" {
			p.leases[l.Slot] = &l
		}
	}
	sweep(opts.Dir)
	listen := opts.Listen
	if listen == "" {
		listen = "127.0.0.1:0"
		if st.Port > 0 {
			// The same port as before, so an endpoint a site wrote down
			// before the restart still answers; another when it is taken.
			listen = "127.0.0.1:" + strconv.Itoa(st.Port)
		}
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil && opts.Listen == "" {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		return nil, err
	}
	p.ln = ln
	p.mu.Lock()
	err = p.saveLocked()
	p.mu.Unlock()
	if err != nil {
		ln.Close()
		return nil, err
	}
	return p, nil
}

func (p *Pool) statePath() string { return filepath.Join(p.opts.Dir, "leases.json") }

// Port is the relay's loopback port.
func (p *Pool) Port() int { return p.ln.Addr().(*net.TCPAddr).Port }

func (p *Pool) endpoint(l *Lease) string {
	return "ws://127.0.0.1:" + strconv.Itoa(p.Port()) + "/browsers/" + strconv.Itoa(l.Slot) + "/" + l.Token
}

func (p *Pool) saveLocked() error {
	st := state{Port: p.Port(), Leases: []Lease{}}
	for _, l := range p.leases {
		st.Leases = append(st.Leases, *l)
	}
	sort.Slice(st.Leases, func(i, j int) bool { return st.Leases[i].Slot < st.Leases[j].Slot })
	p.dirty = false
	return fsx.WriteJSON(p.statePath(), st, 0o600)
}

// Available says whether there is a browser to start.
func (p *Pool) Available() bool { return p.opts.Binary() != "" }

// Size is the pool's size now.
func (p *Pool) Size() int { return max(p.opts.Count(), 0) }

// List is every browser of the pool, and each taken one above its size.
// site is the caller: its own lease comes with its endpoint; "" (the
// operator) sees no endpoint at all, since the token is the holder's.
func (p *Pool) List(site string) []Slot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.listLocked(site)
}

func (p *Pool) listLocked(site string) []Slot {
	n := p.Size()
	out := []Slot{}
	for i := 1; i <= n; i++ {
		out = append(out, p.slotLocked(i, site, n))
	}
	var above []int
	for i := range p.leases {
		if i > n {
			above = append(above, i)
		}
	}
	sort.Ints(above)
	for _, i := range above {
		out = append(out, p.slotLocked(i, site, n))
	}
	return out
}

func (p *Pool) slotLocked(i int, site string, n int) Slot {
	s := Slot{Slot: i, Free: true}
	b := p.live[i]
	s.Running = b != nil
	s.Connected = b != nil && b.client != nil
	l := p.leases[i]
	if l == nil {
		return s
	}
	since, used := l.Since, l.Used
	exp := used.Add(p.opts.Idle())
	s.Free, s.Site, s.For, s.Since, s.Used, s.Leaving = false, l.Site, l.For, &since, &used, i > n
	if site != "" && site == l.Site {
		s.Endpoint = p.endpoint(l)
		s.Expires = &exp
	}
	if site == "" {
		s.Expires = &exp
	}
	return s
}

// Holding is the browser site holds, if any.
func (p *Pool) Holding(site string) (Slot, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, l := range p.leases {
		if l.Site == site {
			return p.slotLocked(l.Slot, site, p.Size()), true
		}
	}
	return Slot{}, false
}

// Take gives site a free browser, saying what for. A site holds one at a
// time: a second take is the one it holds, renewed (an agent that forgot
// it had one is not handed another while others wait). again says so.
func (p *Pool) Take(site, what, actor string) (s Slot, again bool, err error) {
	what = cleanFor(what)
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.opts.Now()
	for _, l := range p.leases {
		if l.Site == site {
			l.Used = now
			if what != "" {
				l.For = what
			}
			_ = p.saveLocked()
			return p.slotLocked(l.Slot, site, p.Size()), true, nil
		}
	}
	n := p.Size()
	if n == 0 {
		return Slot{}, false, ErrNone
	}
	if p.opts.Binary() == "" {
		return Slot{}, false, ErrNoBinary
	}
	for i := 1; i <= n; i++ {
		if p.leases[i] != nil {
			continue
		}
		tok := make([]byte, 24)
		if _, err := rand.Read(tok); err != nil {
			return Slot{}, false, err
		}
		l := &Lease{Slot: i, Site: site, For: what, Token: hex.EncodeToString(tok), Since: now, Used: now}
		p.leases[i] = l
		if err := p.saveLocked(); err != nil {
			delete(p.leases, i)
			return Slot{}, false, err
		}
		msg := fmt.Sprintf("took browser %d", i)
		if what != "" {
			msg += ": " + what
		}
		p.opts.Bus.Emit(events.Event{Type: "browser.taken", Subject: site, Actor: actor, Message: msg, Data: map[string]any{"slot": i, "for": what}})
		return p.slotLocked(i, site, n), false, nil
	}
	return Slot{}, false, &BusyError{Slots: p.listLocked("")}
}

// Release gives slot back. site is the holder giving it back; "" is the
// operator, who may free any. slot 0 is the one site holds. actor is who
// asked, for the event.
func (p *Pool) Release(site string, slot int, actor string) (int, error) {
	p.mu.Lock()
	if slot == 0 && site != "" {
		for _, l := range p.leases {
			if l.Site == site {
				slot = l.Slot
			}
		}
		if slot == 0 {
			p.mu.Unlock()
			return 0, ErrNotTaken
		}
	}
	l := p.leases[slot]
	if l == nil {
		p.mu.Unlock()
		if slot < 1 || slot > p.Size() {
			return 0, ErrNoSuchOne
		}
		return 0, ErrNotTaken
	}
	if site != "" && l.Site != site {
		p.mu.Unlock()
		return 0, ErrNotYours
	}
	p.mu.Unlock()
	if site == "" {
		p.end(slot, l, "browser.freed", actor, "", "the operator freed browser "+strconv.Itoa(slot)+", held by "+l.Site)
	} else {
		p.end(slot, l, "browser.released", actor, "", "gave browser "+strconv.Itoa(slot)+" back after "+p.opts.Now().Sub(l.Since).Round(time.Second).String())
	}
	return slot, nil
}

// end takes lease l off slot (if it is still the one there), ends its
// browser and its profile, and says so.
func (p *Pool) end(slot int, l *Lease, typ, actor, level, msg string) {
	p.mu.Lock()
	if p.leases[slot] != l {
		p.mu.Unlock()
		return
	}
	delete(p.leases, slot)
	b := p.live[slot]
	delete(p.live, slot)
	_ = p.saveLocked()
	p.mu.Unlock()
	if b != nil {
		b.stop("the browser was given back")
	}
	_ = os.RemoveAll(p.profile(slot))
	p.opts.Bus.Emit(events.Event{Type: typ, Subject: l.Site, Actor: actor, Level: level, Message: msg, Data: map[string]any{"slot": slot}})
}

// Expire takes back each lease unused for longer than Idle.
func (p *Pool) Expire() {
	idle := p.opts.Idle()
	now := p.opts.Now()
	p.mu.Lock()
	var gone []*Lease
	for _, l := range p.leases {
		if now.Sub(l.Used) > idle {
			gone = append(gone, l)
		}
	}
	if p.dirty {
		_ = p.saveLocked()
	}
	p.mu.Unlock()
	for _, l := range gone {
		p.end(l.Slot, l, "browser.expired", "engine", "warn", fmt.Sprintf("took browser %d at %s and did not give it back: nothing was sent to it for %s, so the house took it back", l.Slot, l.Since.Format("15:04"), idle.Round(time.Minute)))
	}
}

// Run serves the relay and takes back forgotten leases until ctx ends,
// and then ends every browser (the leases stay on disk).
func (p *Pool) Run(ctx context.Context) {
	srv := &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(p.ln)
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = srv.Close()
			p.Close()
			return
		case <-t.C:
			p.Expire()
		}
	}
}

// Close ends every browser and the relay; the leases stay written.
func (p *Pool) Close() {
	_ = p.ln.Close()
	p.mu.Lock()
	live := p.live
	p.live = map[int]*browser{}
	if p.dirty {
		_ = p.saveLocked()
	}
	p.mu.Unlock()
	for _, b := range live {
		b.stop("the house is stopping")
	}
}

func (p *Pool) slotDir(slot int) string { return filepath.Join(p.opts.Dir, strconv.Itoa(slot)) }
func (p *Pool) profile(slot int) string { return filepath.Join(p.slotDir(slot), "profile") }

// cleanFor keeps what a site says on one line and within MaxFor.
func cleanFor(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > MaxFor {
		s = string(r[:MaxFor])
	}
	return s
}

// ServeHTTP is the relay: /browsers/{slot}/{token} is the websocket, and
// /browsers/{slot}/{token}/json/version what a client that is given an
// http address reads to find it (Playwright's connectOverCDP and
// Puppeteer's browserURL both start there).
func (p *Pool) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 3 || parts[0] != "browsers" {
		http.NotFound(w, r)
		return
	}
	slot, err := strconv.Atoi(parts[1])
	if err != nil {
		http.NotFound(w, r)
		return
	}
	p.mu.Lock()
	l := p.leases[slot]
	ok := l != nil && subtle.ConstantTimeCompare([]byte(l.Token), []byte(parts[2])) == 1
	p.mu.Unlock()
	if !ok {
		http.Error(w, "no browser is held with this address: it was given back, taken back after a while unused, or never taken. Take one again with `middleware site browsers <domain> take`", http.StatusNotFound)
		return
	}
	rest := strings.Join(parts[3:], "/")
	switch {
	case rest == "" && isUpgrade(r):
		p.connect(w, r, slot, l)
	case rest == "json/version" || rest == "json/version/":
		ws := p.endpoint(l)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"Browser": "HeadlessChrome", "Protocol-Version": "1.3", "webSocketDebuggerUrl": %q}`+"\n", ws)
	case rest == "":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "browser %d, held by %s. Connect a DevTools client to %s\n", slot, l.Site, p.endpoint(l))
	default:
		http.NotFound(w, r)
	}
}

// connect relays one client to the browser of slot, starting it if it is
// not running. A new client replaces the one before: an agent's script
// that died without closing would otherwise hold the browser from its own
// next script.
func (p *Pool) connect(w http.ResponseWriter, r *http.Request, slot int, l *Lease) {
	p.mu.Lock()
	if p.leases[slot] != l {
		p.mu.Unlock()
		http.Error(w, "the browser was given back", http.StatusGone)
		return
	}
	b := p.live[slot]
	if b == nil {
		var err error
		b, err = p.launch(slot)
		if err != nil {
			p.mu.Unlock()
			http.Error(w, "the browser could not be started: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		p.live[slot] = b
		go p.pump(slot, l, b)
	}
	p.mu.Unlock()
	ws, err := upgrade(w, r)
	if err != nil {
		return
	}
	p.mu.Lock()
	if p.live[slot] != b {
		p.mu.Unlock()
		ws.shut(1011, "the browser stopped")
		return
	}
	old := b.client
	b.client = ws
	l.Used = p.opts.Now()
	p.dirty = true
	p.mu.Unlock()
	if old != nil {
		old.shut(1000, "another connection took this browser")
	}
	for {
		msg, err := ws.read()
		if err != nil {
			break
		}
		if err := b.send(msg); err != nil {
			break
		}
		p.mu.Lock()
		l.Used = p.opts.Now()
		p.dirty = true
		p.mu.Unlock()
	}
	p.mu.Lock()
	if b.client == ws {
		b.client = nil
	}
	p.mu.Unlock()
	_ = ws.c.Close()
}

// browser is one running Chromium.
type browser struct {
	cmd *exec.Cmd
	// in is what the browser reads its commands from (its fd 3), out
	// what it answers on (its fd 4); each message ends with a NUL.
	in, out *os.File
	wmu     sync.Mutex
	client  *wsConn
	done    chan struct{}
	// stopping is set when the house ends it, so its end is not news.
	stopping bool
	smu      sync.Mutex
}

// launch starts the browser of slot. Called with p.mu held.
func (p *Pool) launch(slot int) (*browser, error) {
	bin := p.opts.Binary()
	if bin == "" {
		return nil, ErrNoBinary
	}
	prof := p.profile(slot)
	if err := os.MkdirAll(prof, 0o700); err != nil {
		return nil, err
	}
	// Output goes straight to a file (AGENTS.md: never an io.Writer to a
	// supervised process). It is cut at each start past a few megabytes:
	// a browser's own chatter is only worth the last run's.
	logPath := filepath.Join(p.slotDir(slot), "browser.log")
	if st, err := os.Stat(logPath); err == nil && st.Size() > 4<<20 {
		_ = os.Truncate(logPath, 0)
	}
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	defer logf.Close()
	// Commands: the house writes cw, the browser reads cr as fd 3.
	// Answers: the browser writes aw as fd 4, the house reads ar.
	cr, cw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	ar, aw, err := os.Pipe()
	if err != nil {
		cr.Close()
		cw.Close()
		return nil, err
	}
	cmd := exec.Command(bin,
		"--headless", "--no-sandbox", "--disable-gpu", "--disable-dev-shm-usage",
		"--no-first-run", "--no-default-browser-check", "--disable-background-networking",
		"--force-color-profile=srgb", "--window-size=1280,800",
		"--remote-debugging-pipe", "--user-data-dir="+prof, "about:blank")
	cmd.Env = []string{"HOME=" + prof, "PATH=/usr/bin:/bin", MarkVar + "=" + p.opts.Dir}
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.ExtraFiles = []*os.File{cr, aw}
	// Its own group, so its renderers and helpers end with it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err = cmd.Start()
	cr.Close()
	aw.Close()
	if err != nil {
		cw.Close()
		ar.Close()
		return nil, err
	}
	p.opts.Bus.Emit(events.Event{Type: "browser.started", Subject: p.leases[slot].Site, Actor: "engine", Message: "browser " + strconv.Itoa(slot) + " started", Data: map[string]any{"slot": slot, "pid": cmd.Process.Pid}})
	return &browser{cmd: cmd, in: cw, out: ar, done: make(chan struct{})}, nil
}

// send hands one message to the browser.
func (b *browser) send(msg []byte) error {
	b.wmu.Lock()
	defer b.wmu.Unlock()
	if _, err := b.in.Write(msg); err != nil {
		return err
	}
	_, err := b.in.Write([]byte{0})
	return err
}

// pump carries the browser's answers to whoever is connected, until it
// ends.
func (p *Pool) pump(slot int, l *Lease, b *browser) {
	r := bufio.NewReaderSize(b.out, 1<<20)
	for {
		msg, err := r.ReadBytes(0)
		if err != nil {
			break
		}
		p.mu.Lock()
		c := b.client
		p.mu.Unlock()
		if c != nil {
			if err := c.text(msg[:len(msg)-1]); err != nil {
				_ = c.c.Close()
			}
		}
	}
	b.out.Close()
	b.in.Close()
	_ = syscall.Kill(-b.cmd.Process.Pid, syscall.SIGKILL)
	_ = b.cmd.Wait()
	close(b.done)
	p.mu.Lock()
	if p.live[slot] == b {
		delete(p.live, slot)
	}
	c := b.client
	b.client = nil
	p.mu.Unlock()
	if c != nil {
		c.shut(1011, "the browser stopped")
	}
	b.smu.Lock()
	stopping := b.stopping
	b.smu.Unlock()
	if !stopping {
		p.opts.Bus.Emit(events.Event{Type: "browser.exited", Subject: l.Site, Actor: "engine", Level: "warn", Message: "browser " + strconv.Itoa(slot) + " stopped by itself (" + b.cmd.ProcessState.String() + "); it is still the site's, and the next connection starts a new one", Data: map[string]any{"slot": slot}})
	}
}

// stop ends the browser and waits for it.
func (b *browser) stop(why string) {
	b.smu.Lock()
	b.stopping = true
	b.smu.Unlock()
	_ = syscall.Kill(-b.cmd.Process.Pid, syscall.SIGKILL)
	b.in.Close()
	select {
	case <-b.done:
	case <-time.After(10 * time.Second):
	}
}

// sweep ends the browsers a supervisor before this one left: those whose
// environment names this pool.
func sweep(dir string) {
	mark := []byte(MarkVar + "=" + dir + "\x00")
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		env, err := os.ReadFile(filepath.Join("/proc", e.Name(), "environ"))
		if err != nil || !containsEnv(env, mark) {
			continue
		}
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

func containsEnv(env, mark []byte) bool {
	for len(env) > 0 {
		if len(env) >= len(mark) && string(env[:len(mark)]) == string(mark) {
			return true
		}
		i := 0
		for i < len(env) && env[i] != 0 {
			i++
		}
		if i >= len(env) {
			return false
		}
		env = env[i+1:]
	}
	return false
}
