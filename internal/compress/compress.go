// Package compress gzips what the house serves itself: a site's static
// files and the panel. A site that runs its own server compresses (or
// not) on its own; the house never touches what an instance answers.
//
// It exists because nothing else did it. Through the tunnel Cloudflare
// compresses for the visitor, but the leg from cloudflared to the edge
// carried every stylesheet and script at full size, and through the
// 80/443 door, the VPN and the local bench nobody compressed at all: the
// panel alone was 1.4 MB of text on every cold load.
package compress

import (
	"bytes"
	"compress/gzip"
	"container/list"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MinSize is the smallest body worth compressing: below it the gzip
// header and the CPU cost more than the bytes saved.
const MinSize = 1024

// MaxSize is the largest file compressed: past it a file is most likely
// a media or a download that is already compressed or that someone asks
// for by ranges, and holding it in memory is not worth it.
const MaxSize = 8 << 20

// Compressible says whether a content type is text that gzip shrinks.
// Images, fonts in woff2, video and archives are compressed already.
func Compressible(contentType string) bool {
	ct, _, _ := strings.Cut(contentType, ";")
	ct = strings.TrimSpace(strings.ToLower(ct))
	if strings.HasPrefix(ct, "text/") {
		return true
	}
	switch ct {
	case "application/javascript", "application/json", "application/manifest+json",
		"application/xml", "application/rss+xml", "application/atom+xml",
		"application/ld+json", "application/wasm", "image/svg+xml",
		"application/x-javascript", "font/ttf", "font/otf", "application/vnd.ms-fontobject":
		return true
	}
	return strings.HasSuffix(ct, "+json") || strings.HasSuffix(ct, "+xml")
}

// TypeByName is the content type a file name promises, or "".
func TypeByName(name string) string {
	switch path.Ext(name) {
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	case ".json":
		return "application/json"
	case ".svg":
		return "image/svg+xml"
	case ".webmanifest":
		return "application/manifest+json"
	}
	return mime.TypeByExtension(path.Ext(name))
}

// Accepts says whether the client takes gzip. "gzip;q=0" is a refusal.
func Accepts(r *http.Request) bool { return AcceptsHeader(r.Header.Get("Accept-Encoding")) }

// AcceptsHeader is Accepts for the value of an Accept-Encoding header.
func AcceptsHeader(ae string) bool {
	for _, part := range strings.Split(ae, ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
			continue
		}
		for _, p := range strings.Split(params, ";") {
			k, v, _ := strings.Cut(strings.TrimSpace(p), "=")
			if strings.EqualFold(k, "q") {
				q, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
				return err == nil && q > 0
			}
		}
		return true
	}
	return false
}

// Bytes gzips data at the default level.
func Bytes(data []byte) []byte {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.DefaultCompression)
	zw.Write(data)
	zw.Close()
	return buf.Bytes()
}

// Cache keeps gzipped files by path, size and modification time, so a
// file changed on disk is compressed again and an unchanged one is
// compressed once. It holds at most max bytes of compressed data, the
// least recently used going first.
type Cache struct {
	mu    sync.Mutex
	max   int
	size  int
	order *list.List
	items map[key]*list.Element
}

type key struct {
	path  string
	size  int64
	mtime int64
}

type item struct {
	k    key
	data []byte
}

// NewCache makes a cache of at most max bytes.
func NewCache(max int) *Cache {
	return &Cache{max: max, order: list.New(), items: map[key]*list.Element{}}
}

// Get returns the gzipped content of the file at path, of that size and
// modification time, reading it from r when it is not kept. A file that
// gzip does not shrink by a tenth is kept as nil, and nil is returned:
// the caller serves it as it is.
func (c *Cache) Get(path string, size int64, mtime time.Time, r io.Reader) ([]byte, error) {
	k := key{path, size, mtime.UnixNano()}
	c.mu.Lock()
	if el, ok := c.items[k]; ok {
		c.order.MoveToFront(el)
		data := el.Value.(*item).data
		c.mu.Unlock()
		return data, nil
	}
	c.mu.Unlock()
	raw, err := io.ReadAll(io.LimitReader(r, size+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) != size {
		// Written to while read: serve it whole and keep nothing.
		return nil, nil
	}
	data := Bytes(raw)
	if len(data) > len(raw)*9/10 {
		data = nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[k]; !ok && len(data) <= c.max {
		c.items[k] = c.order.PushFront(&item{k: k, data: data})
		c.size += len(data)
		for c.size > c.max {
			last := c.order.Back()
			it := last.Value.(*item)
			c.order.Remove(last)
			delete(c.items, it.k)
			c.size -= len(it.data)
		}
	}
	return data, nil
}
