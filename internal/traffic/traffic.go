// Package traffic is what the panel's live view and /waf/traffic read: the
// last sixty seconds of requests per domain, counted on the way out of the
// request chain. It costs one lock and a few increments per request; the
// latency percentiles come from a logarithmic histogram, not a list of
// samples, so a flood cannot grow memory. It also writes the per-site
// access log, which is a file a person tails, not an event.
package traffic

import (
	"bufio"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// window is how far back a snapshot looks.
const window = 60

// nbuckets covers 0.5ms to about 5 minutes doubling each step.
const nbuckets = 20

// slot is one second of one domain.
type slot struct {
	sec     int64
	count   int
	bytes   int64
	status  [6]int // index = status/100; 0 = unknown
	latency [nbuckets]int
}

type ring struct {
	slots [window]slot
}

func bucketOf(d time.Duration) int {
	us := d.Microseconds()
	b := 0
	for lim := int64(500); us > lim && b < nbuckets-1; lim *= 2 {
		b++
	}
	return b
}

// bucketUpper is the upper edge of bucket b in milliseconds.
func bucketUpper(b int) float64 {
	return 0.5 * float64(int64(1)<<uint(b))
}

// Options configure a Recorder.
type Options struct {
	// LogDir is data/logs; each domain gets <LogDir>/<domain>/access.log.
	// Empty disables the access log.
	LogDir string
	// ClientIP resolves the visitor; nil uses RemoteAddr.
	ClientIP func(r *http.Request) net.IP
	// Host resolves the domain of a request; nil lower-cases r.Host.
	Host func(r *http.Request) string
	// Site resolves a request to the site that serves it, by the site's
	// own domain (an alias and the www. count as the site). A name it
	// does not resolve writes to the one shared <LogDir>/access.log, with
	// the name in the line. The Host header is the client's to write:
	// keyed on it, every new name was a folder, an open file and a 16 KB
	// buffer kept for good, and a scanner rotating it ran the house out of
	// descriptors. Nil takes every name as a site of its own.
	Site func(r *http.Request) (string, bool)
	// Service is the service of a site with services that answered the
	// request, or "": written at the end of its line, so the lines of a
	// site without services stay what they were.
	Service func(r *http.Request) string
	Now     func() time.Time
	// MaxLogSize splits a week's access log when it grows past it, 64 MB
	// by default. The log turns over every week either way and nothing is
	// ever deleted: see archive.
	MaxLogSize int64
}

// Recorder counts requests and writes access logs.
type Recorder struct {
	opts Options
	now  func() time.Time

	mu    sync.Mutex
	rings map[string]*ring
	logs  map[string]*logFile
	// packing is the archives being compressed, waited for by Close.
	packing sync.WaitGroup
}

// New builds a recorder.
func New(opts Options) *Recorder {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.MaxLogSize <= 0 {
		opts.MaxLogSize = 64 << 20
	}
	return &Recorder{opts: opts, now: opts.Now, rings: map[string]*ring{}, logs: map[string]*logFile{}}
}

// Record adds one finished request. It is what Wrap calls; tests and other
// packages (an internal handler that bypasses the chain) may call it too.
func (r *Recorder) Record(domain string, status int, bytes int64, dur time.Duration) {
	if domain == "" {
		domain = "-"
	}
	now := r.now()
	sec := now.Unix()
	class := status / 100
	if class < 0 || class > 5 {
		class = 0
	}
	b := bucketOf(dur)
	r.mu.Lock()
	rg := r.rings[domain]
	if rg == nil {
		rg = &ring{}
		r.rings[domain] = rg
	}
	s := &rg.slots[sec%window]
	if s.sec != sec {
		*s = slot{sec: sec}
	}
	s.count++
	s.bytes += bytes
	s.status[class]++
	s.latency[b]++
	r.mu.Unlock()
}

// Domain is one domain's share of a snapshot.
type Domain struct {
	Requests  int            `json:"requests"`
	PerSecond float64        `json:"per_second"`
	Bytes     int64          `json:"bytes"`
	ByStatus  map[string]int `json:"by_status"`
	P50Ms     float64        `json:"p50_ms"`
	P95Ms     float64        `json:"p95_ms"`
	// Seconds is this domain's requests per second for the last minute,
	// oldest first: the site's own sparkline.
	Seconds []int `json:"seconds"`
}

// Snapshot is the last minute, what GET /waf/traffic returns.
type Snapshot struct {
	PerSecond float64           `json:"per_second"`
	Requests  int               `json:"requests"`
	Bytes     int64             `json:"bytes"`
	ByDomain  map[string]Domain `json:"by_domain"`
	ByStatus  map[string]int    `json:"by_status"`
	// Seconds is requests per second for the last minute, oldest first,
	// for a sparkline.
	Seconds []int `json:"seconds"`
}

// Snapshot sums the last sixty seconds.
func (r *Recorder) Snapshot() Snapshot {
	now := r.now().Unix()
	oldest := now - window + 1
	out := Snapshot{ByDomain: map[string]Domain{}, ByStatus: map[string]int{}, Seconds: make([]int, window)}
	r.mu.Lock()
	defer r.mu.Unlock()
	for domain, rg := range r.rings {
		d := Domain{ByStatus: map[string]int{}, Seconds: make([]int, window)}
		var lat [nbuckets]int
		for i := range rg.slots {
			s := &rg.slots[i]
			if s.sec < oldest || s.sec > now {
				continue
			}
			d.Requests += s.count
			d.Bytes += s.bytes
			out.Seconds[s.sec-oldest] += s.count
			d.Seconds[s.sec-oldest] += s.count
			for c, n := range s.status {
				if n > 0 {
					d.ByStatus[className(c)] += n
					out.ByStatus[className(c)] += n
				}
			}
			for b, n := range s.latency {
				lat[b] += n
			}
		}
		if d.Requests == 0 {
			continue
		}
		d.PerSecond = float64(d.Requests) / window
		d.P50Ms = percentile(lat, d.Requests, 0.50)
		d.P95Ms = percentile(lat, d.Requests, 0.95)
		out.ByDomain[domain] = d
		out.Requests += d.Requests
		out.Bytes += d.Bytes
	}
	out.PerSecond = float64(out.Requests) / window
	return out
}

func className(c int) string {
	if c == 0 {
		return "other"
	}
	return strconv.Itoa(c) + "xx"
}

// percentile walks the histogram to the bucket holding the p-th sample and
// returns that bucket's upper edge: an estimate biased upwards, which for
// an operator deciding whether a site is slow is the honest direction.
func percentile(lat [nbuckets]int, total int, p float64) float64 {
	if total == 0 {
		return 0
	}
	target := int(float64(total)*p + 0.999999)
	if target < 1 {
		target = 1
	}
	seen := 0
	for b, n := range lat {
		seen += n
		if seen >= target {
			return bucketUpper(b)
		}
	}
	return bucketUpper(nbuckets - 1)
}

// Prune drops rings that saw nothing for a minute, and closes the access
// logs nothing was written to for logIdle. Run calls it; without it a
// scan of ten thousand hostnames would leave ten thousand rings, and a
// site removed long ago would keep its log open.
func (r *Recorder) Prune() {
	now := r.now()
	oldest := now.Unix() - window
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, lf := range r.logs {
		if now.Sub(lf.used) > logIdle {
			lf.close()
			delete(r.logs, key)
		}
	}
	for domain, rg := range r.rings {
		live := false
		for i := range rg.slots {
			if rg.slots[i].sec >= oldest {
				live = true
				break
			}
		}
		if !live {
			delete(r.rings, domain)
		}
	}
}

// Wrap implements the hook: it records every response that passes through
// it, the proxy's error pages included. What the hooks outside it refused
// (the WAF's 429s and 403s, a closed door) never gets here; the WAF keeps
// its own count of those in Recent. Placing this hook first in the chain
// would log them too, at the cost of a log line per refused flood request.
func (r *Recorder) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		start := r.now()
		rw := &recordingWriter{ResponseWriter: w}
		defer func() {
			dur := r.now().Sub(start)
			domain, site := r.resolve(req)
			status := rw.status
			if status == 0 {
				// Nothing was written: the handler hijacked (WebSocket)
				// or the client left. Count it as a 2xx: the request
				// was served, just not as an HTTP response.
				status = http.StatusOK
				if rw.hijacked {
					status = http.StatusSwitchingProtocols
				}
			}
			if site {
				r.Record(domain, status, rw.bytes, dur)
			} else {
				r.Record(r.strangerRing(domain), status, rw.bytes, dur)
			}
			r.log(domain, site, req, status, rw.bytes, dur)
		}()
		next.ServeHTTP(rw, req)
	})
}

// maxRings bounds the names counted one by one. A site always has its
// ring; past this many, the names no site serves are counted together as
// "-": each ring is some fourteen kilobytes, Prune only runs once a
// minute, and a flood of made-up names would otherwise fill memory within
// that minute.
const maxRings = 1024

func (r *Recorder) strangerRing(name string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.rings[name]; !ok && len(r.rings) >= maxRings {
		return "-"
	}
	return name
}

// resolve is the name a request is counted and logged under, and whether
// a site serves it.
func (r *Recorder) resolve(req *http.Request) (string, bool) {
	if r.opts.Site == nil {
		return r.host(req), true
	}
	if d, ok := r.opts.Site(req); ok && d != "" {
		return d, true
	}
	return r.host(req), false
}

func (r *Recorder) host(req *http.Request) string {
	if r.opts.Host != nil {
		return r.opts.Host(req)
	}
	h := req.Host
	if i := strings.LastIndexByte(h, ':'); i > 0 && !strings.Contains(h[i:], "]") {
		h = h[:i]
	}
	return strings.ToLower(h)
}

// Run flushes the access logs every second and prunes idle rings, until
// ctx ends. It returns at once.
func (r *Recorder) Run(ctx context.Context) {
	r.packLeftovers()
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		n := 0
		for {
			select {
			case <-ctx.Done():
				r.Flush()
				r.Close()
				return
			case <-t.C:
				r.Flush()
				if n++; n%60 == 0 {
					r.Prune()
				}
			}
		}
	}()
}

// recordingWriter remembers the status and byte count. It forwards Flush
// and Hijack and exposes Unwrap so http.ResponseController reaches the
// real writer: the reverse proxy hijacks for WebSockets through it, and a
// wrapper that hid the hijacker would break every socket on the machine.
type recordingWriter struct {
	http.ResponseWriter
	status   int
	bytes    int64
	hijacked bool
}

func (w *recordingWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *recordingWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *recordingWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *recordingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	c, rw, err := h.Hijack()
	if err == nil {
		w.hijacked = true
	}
	return c, rw, err
}

func (w *recordingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// --- access log ---

type logFile struct {
	path string
	f    *os.File
	w    *bufio.Writer
	size int64
	used time.Time
	// week is the ISO week the open file's lines belong to.
	week string
}

// logIdle is how long an access log stays open with nothing to write.
const logIdle = 5 * time.Minute

// log appends one line:
//
//	2026-10-19T12:00:00Z 203.0.113.5 GET /path?q=1 200 1234 12ms "Mozilla/5.0 …"
//
// Compact on purpose: it is grepped and tailed, not parsed by a product.
// A name no site serves goes to the shared <LogDir>/access.log, the name
// after the time.
func (r *Recorder) log(domain string, site bool, req *http.Request, status int, bytes int64, dur time.Duration) {
	if r.opts.LogDir == "" {
		return
	}
	key, path, prefix := domain, filepath.Join(r.opts.LogDir, domain, "access.log"), ""
	if !site {
		key, path, prefix = "", filepath.Join(r.opts.LogDir, "access.log"), logName(domain)+" "
	} else if domain == "" || strings.ContainsAny(domain, "/\\") || domain == "." || domain == ".." {
		return
	}
	ip := "-"
	if r.opts.ClientIP != nil {
		if v := r.opts.ClientIP(req); v != nil {
			ip = v.String()
		}
	} else if host, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
		ip = host
	}
	ua := strings.ReplaceAll(req.UserAgent(), `"`, `'`)
	if len(ua) > 200 {
		ua = ua[:200]
	}
	uri := req.URL.RequestURI()
	if len(uri) > 1000 {
		uri = uri[:1000]
	}
	now := r.now()
	line := now.UTC().Format(time.RFC3339) + " " + prefix + ip + " " + req.Method + " " + uri + " " +
		strconv.Itoa(status) + " " + strconv.FormatInt(bytes, 10) + " " + dur.Round(time.Millisecond).String() + " \"" + ua + "\""
	if site && r.opts.Service != nil {
		if svc := r.opts.Service(req); svc != "" {
			line += " service=" + svc
		}
	}
	line += "\n"
	r.mu.Lock()
	defer r.mu.Unlock()
	lf := r.logs[key]
	if lf == nil {
		lf = &logFile{path: path}
		r.logs[key] = lf
	}
	lf.used = now
	if lf.f == nil {
		if err := lf.open(now); err != nil {
			return
		}
	}
	if week := weekOf(now); week != lf.week || lf.size+int64(len(line)) > r.opts.MaxLogSize {
		r.archive(lf, now)
		if lf.f == nil {
			return
		}
	}
	n, _ := lf.w.WriteString(line)
	lf.size += int64(n)
}

// logName is a name nobody serves as it goes in the shared log: the
// client wrote it, so it is cut short and keeps no space or quote that
// would shift the fields of the line.
func logName(name string) string {
	if name == "" {
		return "-"
	}
	if len(name) > 100 {
		name = name[:100]
	}
	return strings.Map(func(c rune) rune {
		if c <= ' ' || c == '"' || c == 0x7f {
			return '_'
		}
		return c
	}, name)
}

func (lf *logFile) open(now time.Time) error {
	if err := os.MkdirAll(filepath.Dir(lf.path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(lf.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	lf.f, lf.w, lf.size = f, bufio.NewWriterSize(f, 16<<10), st.Size()
	// A file left from before a restart, or from a site nobody visited
	// for a while, belongs to the week its last line was written in: the
	// next line, if that week is over, archives it under that week's
	// name. The line's own time, not the file's: a copy or a backup
	// restored changes the second and not the first.
	lf.week = weekOf(now)
	if lf.size > 0 {
		lf.week = weekOf(lastLineTime(lf.path, st))
	}
	return nil
}

// lastLineTime is the time the last line of an access log begins with,
// or the file's modification time when it does not read as one.
func lastLineTime(path string, st os.FileInfo) time.Time {
	f, err := os.Open(path)
	if err != nil {
		return st.ModTime()
	}
	defer f.Close()
	const tail = 4096
	off := st.Size() - tail
	if off < 0 {
		off = 0
	}
	buf := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return st.ModTime()
	}
	text := strings.TrimRight(string(buf), "\n")
	if i := strings.LastIndexByte(text, '\n'); i >= 0 {
		text = text[i+1:]
	}
	stamp, _, _ := strings.Cut(text, " ")
	if t, err := time.Parse(time.RFC3339, stamp); err == nil {
		return t
	}
	return st.ModTime()
}

// weekOf is t's ISO week, 2026-W45: the access log turns over on Monday.
func weekOf(t time.Time) string {
	y, w := t.Local().ISOWeek()
	return fmt.Sprintf("%d-W%02d", y, w)
}

// archive closes the week's access log as access.log.<week>, or
// access.log.<week>.2, .3… when a busy week was split by size, opens a
// fresh one, and compresses the archive in the background.
//
// Nothing is ever deleted. The log holds who called (address, time, path,
// User-Agent) and the operator may be bound by law to hand that over, so
// a rotation that dropped the oldest file, as this did by size until
// 2026-11, lost exactly the lines that might be asked for. What bounds
// the disk now is gzip, about a tenth of the text, and the operator.
// Renames, not copies, so the running writer never races a reader that
// tails the file. The archive's name does not end in .log on purpose:
// the guard and the site's own logrotate glob *.log, and an archive is
// neither.
func (r *Recorder) archive(lf *logFile, now time.Time) {
	_ = lf.w.Flush()
	_ = lf.f.Close()
	lf.f, lf.w = nil, nil
	base := lf.path + "." + lf.week
	name := base
	for i := 2; exists(name) || exists(name+".gz"); i++ {
		name = base + "." + strconv.Itoa(i)
	}
	if os.Rename(lf.path, name) == nil {
		r.packing.Add(1)
		go func() {
			defer r.packing.Done()
			_ = pack(name)
		}()
	}
	_ = lf.open(now)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// pack compresses an archive to <name>.gz and removes the text. The .gz
// is written aside and renamed into place, so a crash halfway leaves the
// text, which packLeftovers compresses at the next start, and never a
// truncated .gz next to nothing.
func pack(name string) error {
	in, err := os.Open(name)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := name + ".gz.tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(out)
	_, err = io.Copy(zw, in)
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if serr := out.Sync(); err == nil {
		err = serr
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, name+".gz")
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Remove(name)
}

// archived matches an archive still in text: access.log.2026-W45 or
// access.log.2026-W45.2.
var archived = regexp.MustCompile(`^access\.log\.\d{4}-W\d{2}(\.\d+)?$`)

// packLeftovers compresses the archives a stop or a crash left in text,
// before anything is written: nothing else is compressing yet.
func (r *Recorder) packLeftovers() {
	if r.opts.LogDir == "" {
		return
	}
	dirs, _ := filepath.Glob(filepath.Join(r.opts.LogDir, "*"))
	dirs = append(dirs, r.opts.LogDir)
	for _, d := range dirs {
		entries, _ := os.ReadDir(d)
		for _, e := range entries {
			if !e.IsDir() && archived.MatchString(e.Name()) {
				name := filepath.Join(d, e.Name())
				r.packing.Add(1)
				go func() {
					defer r.packing.Done()
					_ = pack(name)
				}()
			}
		}
	}
}

// Flush writes buffered log lines to disk.
func (r *Recorder) Flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, lf := range r.logs {
		if lf.w != nil {
			_ = lf.w.Flush()
		}
	}
}

// Close flushes and closes every log file, and waits for the archives
// being compressed.
func (r *Recorder) Close() {
	r.mu.Lock()
	for _, lf := range r.logs {
		lf.close()
	}
	r.mu.Unlock()
	r.packing.Wait()
}

func (lf *logFile) close() {
	if lf.w != nil {
		_ = lf.w.Flush()
	}
	if lf.f != nil {
		_ = lf.f.Close()
	}
	lf.f, lf.w = nil, nil
}

// Domains lists the domains with traffic in the last minute, sorted.
func (r *Recorder) Domains() []string {
	snap := r.Snapshot()
	out := make([]string, 0, len(snap.ByDomain))
	for d := range snap.ByDomain {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}
