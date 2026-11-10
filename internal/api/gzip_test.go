package api

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// A route's answer goes gzipped to a client that takes it and plain to
// one that does not, the same JSON either way.
func TestAnswersAreCompressedForAClientThatTakesGzip(t *testing.T) {
	f := newFixture(t).withToken()
	ask := func(enc string) (*http.Response, []byte) {
		req, _ := http.NewRequest("GET", f.http.URL+Prefix+"/openapi.json", nil)
		req.Header.Set("Authorization", "Bearer "+f.token)
		if enc != "" {
			req.Header.Set("Accept-Encoding", enc)
		}
		// A transport that leaves the encoding as it came.
		resp, err := (&http.Transport{DisableCompression: true}).RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp, body
	}
	plainResp, plain := ask("")
	zipResp, zipped := ask("gzip")
	if plainResp.Header.Get("Content-Encoding") != "" || zipResp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("encodings %q / %q", plainResp.Header.Get("Content-Encoding"), zipResp.Header.Get("Content-Encoding"))
	}
	if !strings.Contains(zipResp.Header.Get("Vary"), "Accept-Encoding") {
		t.Fatal("no Vary: Accept-Encoding")
	}
	zr, err := gzip.NewReader(strings.NewReader(string(zipped)))
	if err != nil {
		t.Fatal(err)
	}
	back, _ := io.ReadAll(zr)
	var a, b any
	if json.Unmarshal(plain, &a) != nil || json.Unmarshal(back, &b) != nil {
		t.Fatal("not JSON")
	}
	if string(back) != string(plain) {
		t.Fatal("the compressed answer is not the plain one")
	}
	if len(zipped) >= len(plain) {
		t.Fatalf("compressed %d bytes, plain %d", len(zipped), len(plain))
	}
}
