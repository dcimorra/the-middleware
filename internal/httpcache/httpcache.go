// Package httpcache is the house's response cache: it keeps what a site
// marks as public (or what the manifest forces) and answers it without
// asking the site again. It exists for what the edge cannot do: an edge
// cache rule reaches only the names proxied by the edge, and a house that
// also serves through its own 80 and 443 has no cache there otherwise.
//
// What is kept is kept for everyone, so there are walls a manifest cannot
// remove: nothing with Set-Cookie; nothing private, no-store or no-cache;
// nothing with Vary: * or Vary: Cookie; nothing but a 200 to a GET or
// HEAD; nothing for a request with Authorization; no event streams and no
// WebSockets, which do not even wait behind one another. Nothing for a
// request with a cookie either, unless the rule says its pages read none
// (ignore_cookies). What a manifest chooses is otherwise only which paths
// and for how long. A site that gets
// its cache section wrong can be slow or serve something old; it cannot
// serve someone what was someone else's.
//
// It sits inside the door (access) and behind it, never in front: V1
// had it the other way round first, and the first page someone who had
// logged in loaded was then served to whoever had not.
package httpcache

import (
	"bytes"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/edge"
)

// Rule is one entry of the manifest's cache.rules.
type Rule struct {
	// Path is a prefix, by whole segments; the longest that matches wins.
	Path string `json:"path"`
	// Mode is origin (keep what the site marks, for as long as it says),
	// force (keep for TTL seconds whatever the site says, for what is
	// known to be public and still) or never.
	Mode string `json:"mode"`
	// TTL is for force, in seconds.
	TTL int `json:"ttl,omitempty"`
	// IgnoreCookies keeps and answers requests that carry cookies as if
	// they carried none, for paths whose pages read none (an analytics
	// cookie on every visitor otherwise leaves nothing to keep). Without
	// it a request with a cookie of the site's passes the cache by.
	IgnoreCookies bool `json:"ignore_cookies,omitempty"`
	// Note is what the rule is for, for whoever reads the manifest.
	Note string `json:"note,omitempty"`
}

// The modes.
const (
	ModeOrigin = "origin"
	ModeForce  = "force"
	ModeNever  = "never"
)

// maxEntry is the largest response kept: a video is not what this is for.
const maxEntry = 8 << 20

type entry struct {
	vary    map[string]string // the request headers it varies on, and their values
	status  int
	header  http.Header
	body    []byte
	stored  time.Time
	expires time.Time
	size    int64
	used    time.Time
}

// Stats is a site's cache as listed.
type Stats struct {
	Entries int   `json:"entries"`
	Bytes   int64 `json:"bytes"`
	Hits    int64 `json:"hits"`
	Misses  int64 `json:"misses"`
	// Limit is the site's own cap in bytes (0: only the house's budget).
	Limit int64 `json:"limit,omitempty"`
}

type store struct {
	// entries are by host and URI; each holds the variants Vary asked for.
	entries map[string][]*entry
	bytes   int64
	hits    int64
	misses  int64
	limit   int64
	// generation is the instance the entries came from; a handover
	// empties the store, because what is kept was answered by the
	// version that just left.
	generation string
}

// Cache holds every site's store under one budget.
type Cache struct {
	mu     sync.Mutex
	sites  map[string]*store
	budget int64
	now    func() time.Time
	// flights coalesces simultaneous misses on one key.
	flights map[string]*flight
}

type flight struct {
	done chan struct{}
}

// New returns a cache with a house budget in bytes.
func New(budget int64) *Cache {
	return &Cache{sites: map[string]*store{}, budget: budget, now: time.Now, flights: map[string]*flight{}}
}

// SetBudget changes the house budget at once, without a restart: lowering
// it because the machine is short of memory must not require restarting
// the house, which is exactly what is not wanted then.
func (c *Cache) SetBudget(b int64) {
	c.mu.Lock()
	c.budget = b
	c.trimLocked()
	c.mu.Unlock()
}

func (c *Cache) siteLocked(domain string) *store {
	s := c.sites[domain]
	if s == nil {
		s = &store{entries: map[string][]*entry{}}
		c.sites[domain] = s
	}
	return s
}

// Purge empties a site, and each of its services' ("<domain>#<name>").
func (c *Cache) Purge(domain string) {
	c.mu.Lock()
	for d := range c.sites {
		if d == domain || strings.HasPrefix(d, domain+"#") {
			delete(c.sites, d)
		}
	}
	c.mu.Unlock()
}

// Stats is one site's numbers.
func (c *Cache) Stats(domain string) Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out Stats
	for d, s := range c.sites {
		if d != domain && !strings.HasPrefix(d, domain+"#") {
			continue
		}
		out.Entries += s.count()
		out.Bytes += s.bytes
		out.Hits += s.hits
		out.Misses += s.misses
		out.Limit = max(out.Limit, s.limit)
	}
	return out
}

// Total is the house's numbers and budget.
func (c *Cache) Total() (bytes, budget int64, entries int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.sites {
		bytes += s.bytes
		entries += s.count()
	}
	return bytes, c.budget, entries
}

// trimLocked releases entries while the house is over its budget, from
// the site that holds the most, one entry at a time and asking again who
// is largest: the big ones come down together instead of one emptying
// while another almost as big keeps everything, and the site that behaves
// does not pay for the one that does not.
func (c *Cache) trimLocked() {
	for c.budget > 0 {
		var total int64
		var largest *store
		for _, s := range c.sites {
			total += s.bytes
			if largest == nil || s.bytes > largest.bytes {
				largest = s
			}
		}
		if total <= c.budget || largest == nil || largest.bytes == 0 {
			return
		}
		largest.evictOne()
	}
}

func (s *store) count() int {
	n := 0
	for _, list := range s.entries {
		n += len(list)
	}
	return n
}

// evictOne drops the least recently used entry.
func (s *store) evictOne() {
	var oldKey string
	oldIdx := -1
	var oldest time.Time
	for k, list := range s.entries {
		for i, e := range list {
			if oldIdx < 0 || e.used.Before(oldest) {
				oldKey, oldIdx, oldest = k, i, e.used
			}
		}
	}
	if oldIdx < 0 {
		return
	}
	s.remove(oldKey, oldIdx)
}

func (s *store) remove(k string, i int) {
	list := s.entries[k]
	s.bytes -= list[i].size
	list = append(list[:i], list[i+1:]...)
	if len(list) == 0 {
		delete(s.entries, k)
	} else {
		s.entries[k] = list
	}
}

// Match finds the rule for a cleaned path: the longest prefix by segments.
func Match(rules []Rule, path string) (Rule, bool) {
	best, found := Rule{}, false
	for _, r := range rules {
		p := strings.TrimSuffix(r.Path, "/")
		if p == "" || path == p || strings.HasPrefix(path, p+"/") {
			if !found || len(p) > len(strings.TrimSuffix(best.Path, "/")) {
				best, found = r, true
			}
		}
	}
	return best, found
}

// ttlFor says how long a response may be kept under a rule, or 0.
func ttlFor(rule Rule, h http.Header, status int) time.Duration {
	if status != http.StatusOK || h.Get("Set-Cookie") != "" {
		return 0
	}
	if strings.HasPrefix(h.Get("Content-Type"), "text/event-stream") {
		return 0
	}
	for _, v := range h.Values("Vary") {
		for _, f := range strings.Split(v, ",") {
			f = strings.TrimSpace(strings.ToLower(f))
			if f == "*" || f == "cookie" || f == "authorization" {
				return 0
			}
		}
	}
	// Every Cache-Control line, not the first: with two, Get read the one
	// the manifest had put first and kept a page the site marked private.
	cc := parseCC(strings.Join(h.Values("Cache-Control"), ","))
	if _, ok := cc["private"]; ok {
		return 0
	}
	if _, ok := cc["no-store"]; ok {
		return 0
	}
	if _, ok := cc["no-cache"]; ok {
		return 0
	}
	switch rule.Mode {
	case ModeForce:
		return time.Duration(rule.TTL) * time.Second
	case ModeOrigin:
		// s-maxage is for shared caches like this one; max-age is the
		// browser's, and the fallback.
		for _, k := range []string{"s-maxage", "max-age"} {
			if v, ok := cc[k]; ok {
				if n, err := strconv.Atoi(v); err == nil && n > 0 {
					return time.Duration(n) * time.Second
				}
			}
		}
	}
	return 0
}

func parseCC(v string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(v, ",") {
		k, val, _ := strings.Cut(strings.TrimSpace(part), "=")
		if k != "" {
			out[strings.ToLower(k)] = strings.Trim(val, `"`)
		}
	}
	return out
}

// key is the scheme, the host and the URI: a HEAD is answered from a
// GET's entry. Variants by Vary live under it. The scheme is the one the
// house believes (edge.HTTPS) and tells the site: a page rendered for
// http, with its http:// links and assets, was kept and given to https
// visitors as mixed content, and whoever asked first over plain http
// chose what everyone on https got.
func key(r *http.Request) string {
	scheme := "http"
	if edge.HTTPS(r) {
		scheme = "https"
	}
	return scheme + " " + strings.ToLower(r.Host) + " " + r.URL.RequestURI()
}

func (e *entry) fits(r *http.Request) bool {
	for h, v := range e.vary {
		if r.Header.Get(h) != v {
			return false
		}
	}
	return true
}

// varyOf is the request headers a response varies on, sorted.
func varyOf(h http.Header) []string {
	var out []string
	for _, v := range h.Values("Vary") {
		for _, f := range strings.Split(v, ",") {
			if f = strings.TrimSpace(f); f != "" {
				out = append(out, http.CanonicalHeaderKey(f))
			}
		}
	}
	sort.Strings(out)
	return out
}

// recorder writes through to the client and keeps a copy while it fits.
// keepable says, once the status and headers are known, whether the answer
// could be kept at all; when it cannot, or once the body outgrows
// maxEntry, release lets the requests waiting on the same key go.
type recorder struct {
	http.ResponseWriter
	status  int
	buf     bytes.Buffer
	tooBig  bool
	unkept  bool
	written bool
	// header is the answer's headers as the site sent them, taken when
	// the status is known and before the writers outside this one see
	// it. They share the map and change it as the answer passes: the
	// Web Vitals hook marks the page gzip as it compresses it on the way
	// out, and read after the handler returned, that mark was kept with
	// the page the site sent, which is not compressed. Every later
	// visitor got a plain page announced as gzip, which no browser
	// shows.
	header   http.Header
	keepable func(status int) bool
	release  func()
}

func (r *recorder) settle(code int) {
	if r.written {
		return
	}
	r.status, r.written = code, true
	r.header = r.ResponseWriter.Header().Clone()
	if r.keepable != nil && !r.keepable(code) {
		r.unkept = true
		r.letGo()
	}
}

func (r *recorder) letGo() {
	if r.release != nil {
		r.release()
	}
}

func (r *recorder) WriteHeader(code int) {
	// An informational answer (103 Early Hints) comes before the real
	// one; taken as the status, it made every such page one never kept.
	if code >= 200 {
		r.settle(code)
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(p []byte) (int, error) {
	r.settle(http.StatusOK)
	// What will not be kept is not copied either: an hour of an event
	// stream held in memory only to be thrown away.
	if !r.tooBig && !r.unkept {
		if r.buf.Len()+len(p) > maxEntry {
			r.tooBig = true
			r.buf.Reset()
			r.letGo()
		} else {
			r.buf.Write(p)
		}
	}
	return r.ResponseWriter.Write(p)
}

func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
