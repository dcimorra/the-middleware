package gz

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func TestAcceptsReadsTheHeaderAsAClientMeansIt(t *testing.T) {
	for header, want := range map[string]bool{
		"":                       false,
		"gzip":                   true,
		"br, gzip, deflate":      true,
		"GZIP":                   true,
		"deflate, br":            false,
		"gzip;q=0":               false,
		"gzip; q=0.0":            false,
		"gzip;q=0.5, identity":   true,
		"identity;q=1, gzip;q=1": true,
		"*":                      true,
		"x-gzip":                 false,
	} {
		r, _ := http.NewRequest("GET", "/", nil)
		if header != "" {
			r.Header.Set("Accept-Encoding", header)
		}
		if got := Accepts(r); got != want {
			t.Errorf("Accept-Encoding %q: accepts %v, want %v", header, got, want)
		}
	}
}

func TestCompressibleIsTextAndNotWhatIsAlreadyPacked(t *testing.T) {
	for ct, want := range map[string]bool{
		"text/html; charset=utf-8": true,
		"text/css":                 true,
		"application/json":         true,
		"application/ld+json":      true,
		"image/svg+xml":            true,
		"application/wasm":         true,
		"image/png":                false,
		"font/woff2":               false,
		"video/mp4":                false,
		"application/zip":          false,
		"":                         false,
	} {
		if got := Compressible(ct); got != want {
			t.Errorf("%q: compressible %v, want %v", ct, got, want)
		}
	}
}

func TestBytesComeBackAsTheyWent(t *testing.T) {
	in := []byte(strings.Repeat("the house answers this itself\n", 200))
	for i := 0; i < 3; i++ { // the pooled writer, used again
		zr, err := gzip.NewReader(bytes.NewReader(Bytes(in)))
		if err != nil {
			t.Fatal(err)
		}
		out, err := io.ReadAll(zr)
		if err != nil || !bytes.Equal(in, out) {
			t.Fatalf("round %d: %d bytes back, err %v", i, len(out), err)
		}
	}
}

func TestSmallerIsNilWhenNothingIsSaved(t *testing.T) {
	if Smaller([]byte("short")) != nil {
		t.Error("a body under MinSize was compressed")
	}
	packed := Bytes([]byte(strings.Repeat("abcdefghij", 5000)))
	noise := make([]byte, 0, 8192)
	for i := 0; len(noise) < 8192; i++ {
		noise = append(noise, packed[i%len(packed)]^byte(i*131>>3))
	}
	if out := Smaller(noise); out != nil && len(out) > len(noise)*9/10 {
		t.Errorf("kept a body that shrank from %d to %d", len(noise), len(out))
	}
	if Smaller([]byte(strings.Repeat("text shrinks. ", 500))) == nil {
		t.Error("plain text was not compressed")
	}
}

func TestCacheDropsTheLeastRecentlyAskedFor(t *testing.T) {
	c := NewCache(3 * (1000 + itemCost + 1))
	body := make([]byte, 1000)
	for i := 0; i < 3; i++ {
		c.Put(strconv.Itoa(i), body)
	}
	c.Get("0") // 1 is now the oldest
	c.Put("3", body)
	if _, ok := c.Get("1"); ok {
		t.Error("the least recently used entry was kept")
	}
	for _, k := range []string{"0", "2", "3"} {
		if _, ok := c.Get(k); !ok {
			t.Errorf("entry %s was dropped", k)
		}
	}
	c.Put("nil", nil)
	if data, ok := c.Get("nil"); !ok || data != nil {
		t.Error("a body that does not shrink was not remembered as such")
	}
	c.Put("huge", make([]byte, 1<<20))
	if _, ok := c.Get("huge"); ok {
		t.Error("an entry larger than the cache was kept")
	}
	c.Put("0", make([]byte, 10))
	if c.size > c.max || c.size < 0 {
		t.Errorf("size %d out of bounds %d", c.size, c.max)
	}
}
