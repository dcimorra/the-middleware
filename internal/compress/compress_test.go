package compress

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestAccepts(t *testing.T) {
	for header, want := range map[string]bool{
		"":                  false,
		"gzip":              true,
		"br, gzip, deflate": true,
		"gzip;q=0":          false,
		"gzip; q=0, br":     false,
		"deflate, *":        false,
		"identity":          false,
		"GZIP;q=0.5":        true,
	} {
		r, _ := http.NewRequest("GET", "/", nil)
		r.Header.Set("Accept-Encoding", header)
		if got := Accepts(r); got != want {
			t.Errorf("%q: %v, want %v", header, got, want)
		}
	}
}

func TestCompressible(t *testing.T) {
	for ct, want := range map[string]bool{
		"text/html; charset=utf-8": true,
		"application/json":         true,
		"image/svg+xml":            true,
		"application/vnd.api+json": true,
		"image/png":                false,
		"font/woff2":               false,
		"application/zip":          false,
		"":                         false,
	} {
		if got := Compressible(ct); got != want {
			t.Errorf("%q: %v, want %v", ct, got, want)
		}
	}
}

func TestCacheCompressesOnceAndAgainWhenTheFileChanges(t *testing.T) {
	c := NewCache(1 << 20)
	text := strings.Repeat("hello middleware ", 200)
	at := time.Now()
	reads := 0
	get := func(body string, mtime time.Time) []byte {
		t.Helper()
		data, err := c.Get("/a.css", int64(len(body)), mtime, &counting{r: strings.NewReader(body), n: &reads})
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	first := get(text, at)
	before := reads
	if again := get(text, at); !bytes.Equal(again, first) || reads != before {
		t.Fatal("an unchanged file was read and compressed again")
	}
	if unpack(t, first) != text {
		t.Fatal("the gzip does not unpack to the file")
	}
	if unpack(t, get(text+"more", at.Add(time.Second))) != text+"more" {
		t.Fatal("a changed file was answered from the cache")
	}
}

func unpack(t *testing.T, data []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	back, _ := io.ReadAll(zr)
	return string(back)
}

type counting struct {
	r io.Reader
	n *int
}

func (c *counting) Read(p []byte) (int, error) { *c.n++; return c.r.Read(p) }

func TestCacheStaysUnderItsSize(t *testing.T) {
	c := NewCache(4096)
	for i := 0; i < 100; i++ {
		body := strings.Repeat(string(rune('a'+i%26)), 50000) + strings.Repeat("x", i)
		if _, err := c.Get("/f"+string(rune(i)), int64(len(body)), time.Unix(int64(i), 0), strings.NewReader(body)); err != nil {
			t.Fatal(err)
		}
	}
	if c.size > 4096 {
		t.Fatalf("the cache holds %d bytes", c.size)
	}
}

func TestAFileReadShortIsNotCompressed(t *testing.T) {
	c := NewCache(1 << 20)
	got, err := c.Get("/r.txt", 4096, time.Now(), strings.NewReader("short"))
	if err != nil || got != nil {
		t.Fatalf("a file changing under the read was compressed: %v", err)
	}
}
