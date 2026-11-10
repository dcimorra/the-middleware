package api

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dcimorra/the-middleware/internal/auth"
)

// A long list goes out compressed to a caller that takes gzip and as
// written to one that does not; a short answer is not worth it; and a
// stream still flushes line by line through the marked writer.
func TestTheAPICompressesItsListsForWhoeverTakesGzip(t *testing.T) {
	s := New(Options{Version: "test"})
	list := make([]map[string]string, 300)
	for i := range list {
		list[i] = map[string]string{"domain": "site.example", "state": "running"}
	}
	s.Handle("GET", Prefix+"/long", "", func(w http.ResponseWriter, r *http.Request) (any, error) { return list, nil })
	s.Handle("GET", Prefix+"/short", "", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return map[string]bool{"ok": true}, nil
	})
	flushed := false
	s.Handle("GET", Prefix+"/stream", "", func(w http.ResponseWriter, r *http.Request) (any, error) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Error("the stream's writer cannot flush")
			return nil, nil
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: one\n\n")
		f.Flush()
		flushed = true
		return Handled, nil
	})
	ask := func(path, accept string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", Prefix+path, nil)
		r = r.WithContext(auth.WithSocket(r.Context()))
		if accept != "" {
			r.Header.Set("Accept-Encoding", accept)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	plain := ask("/long", "")
	if plain.Header().Get("Content-Encoding") != "" {
		t.Fatal("compressed for a caller that did not ask")
	}
	packed := ask("/long", "gzip")
	if packed.Header().Get("Content-Encoding") != "gzip" || packed.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatalf("headers: %v", packed.Header())
	}
	if packed.Body.Len() > plain.Body.Len()/5 {
		t.Errorf("%d bytes compressed of %d", packed.Body.Len(), plain.Body.Len())
	}
	zr, err := gzip.NewReader(packed.Body)
	if err != nil {
		t.Fatal(err)
	}
	var got []map[string]string
	if err := json.NewDecoder(zr).Decode(&got); err != nil || len(got) != len(list) {
		t.Fatalf("%d rows back, %v", len(got), err)
	}
	short := ask("/short", "gzip")
	if short.Header().Get("Content-Encoding") != "" || !strings.Contains(short.Body.String(), `"ok": true`) {
		t.Errorf("a short answer: %v %q", short.Header(), short.Body.String())
	}
	stream := ask("/stream", "gzip")
	if !flushed || stream.Header().Get("Content-Encoding") != "" || stream.Body.String() != "data: one\n\n" {
		t.Errorf("the stream: flushed %v, %v, %q", flushed, stream.Header(), stream.Body.String())
	}
}
