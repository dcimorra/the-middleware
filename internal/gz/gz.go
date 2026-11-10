// Package gz compresses what the house answers itself: a site's static
// files, the panel, the API. What a site's own process answers is never
// touched: it compresses or not as it sees fit, and the proxy passes on
// what it sent.
//
// Through the tunnel Cloudflare compresses for the visitor, but only on
// its side of the tunnel; through the 80/443 door, the VPN and the local
// door nobody did, and the panel alone was a megabyte and a half of plain
// text on a first load.
package gz

import (
	"bytes"
	"compress/gzip"
	"container/list"
	"net/http"
	"strings"
	"sync"
)

// MinSize is the smallest body worth it: under it the gzip header and
// the time cost more than the bytes saved.
const MinSize = 1024

// Accepts says whether the client takes gzip. "gzip;q=0" is a refusal.
func Accepts(r *http.Request) bool {
	for _, v := range r.Header.Values("Accept-Encoding") {
		for v != "" {
			var part string
			part, v, _ = strings.Cut(v, ",")
			name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
			if name = strings.TrimSpace(name); !strings.EqualFold(name, "gzip") && name != "*" {
				continue
			}
			q := strings.TrimSpace(params)
			if k, val, ok := strings.Cut(q, "="); ok && strings.EqualFold(strings.TrimSpace(k), "q") {
				val = strings.TrimSpace(val)
				return val != "0" && val != "0." && strings.Trim(val, "0.") != ""
			}
			return true
		}
	}
	return false
}

// Compressible says whether a content type is text that gzip shrinks.
// Pictures, video, woff2 and archives are compressed already, and gzip
// over them costs time to save nothing.
func Compressible(contentType string) bool {
	ct, _, _ := strings.Cut(contentType, ";")
	ct = strings.ToLower(strings.TrimSpace(ct))
	if strings.HasPrefix(ct, "text/") {
		return true
	}
	switch ct {
	case "application/javascript", "application/x-javascript", "application/json",
		"application/xml", "application/wasm", "image/svg+xml", "image/x-icon",
		"image/vnd.microsoft.icon", "font/ttf", "font/otf", "application/vnd.ms-fontobject":
		return true
	}
	return strings.HasSuffix(ct, "+json") || strings.HasSuffix(ct, "+xml")
}

var writers = sync.Pool{New: func() any { return gzip.NewWriter(nil) }}

// Bytes is data compressed. A gzip.Writer is some 800 KiB of state, so
// they are lent from a pool instead of made per answer.
func Bytes(data []byte) []byte {
	var buf bytes.Buffer
	buf.Grow(len(data)/3 + 64)
	zw := writers.Get().(*gzip.Writer)
	zw.Reset(&buf)
	_, _ = zw.Write(data)
	_ = zw.Close()
	writers.Put(zw)
	return buf.Bytes()
}

// Smaller is data compressed when that saves at least a tenth of it, and
// nil when it does not: the caller then sends data as it is.
func Smaller(data []byte) []byte {
	if len(data) < MinSize {
		return nil
	}
	if out := Bytes(data); len(out) <= len(data)*9/10 {
		return out
	}
	return nil
}

// Cache keeps compressed bodies by a key of the caller's, at most max
// bytes of them, dropping the least recently asked for first. A body
// that did not shrink is kept as nil, so it is not tried again on every
// request.
type Cache struct {
	mu    sync.Mutex
	max   int
	size  int
	order *list.List
	items map[string]*list.Element
}

type item struct {
	key  string
	data []byte
}

// itemCost is what an entry weighs besides its bytes, so that a cache of
// nothing but nils is bounded too.
const itemCost = 256

// NewCache makes a cache of at most max bytes.
func NewCache(max int) *Cache {
	return &Cache{max: max, order: list.New(), items: map[string]*list.Element{}}
}

// Get is what was put under key, and whether anything was.
func (c *Cache) Get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*item).data, true
}

// Put keeps data under key. nil is kept too: "this one does not shrink".
func (c *Cache) Put(key string, data []byte) {
	cost := len(data) + len(key) + itemCost
	if cost > c.max {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		old := el.Value.(*item)
		c.size -= len(old.data) + len(old.key) + itemCost
		c.order.Remove(el)
	}
	c.items[key] = c.order.PushFront(&item{key: key, data: data})
	c.size += cost
	for c.size > c.max {
		last := c.order.Back()
		it := last.Value.(*item)
		c.order.Remove(last)
		delete(c.items, it.key)
		c.size -= len(it.data) + len(it.key) + itemCost
	}
}
