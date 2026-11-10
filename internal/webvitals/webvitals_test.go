package webvitals

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestThePercentileIsTheOneGoogleRanksBy(t *testing.T) {
	b := New(t.TempDir())
	day := time.Date(2026, 11, 8, 12, 0, 0, 0, time.Local)
	b.now = func() time.Time { return day }
	// Three good pages and one poor one: the 75th percentile is the
	// third, good; the share of poor is a quarter.
	for _, v := range []int{1000, 1200, 2000, 6000} {
		if err := b.Note("a.test", Sample{Path: "/p?token=secret", Values: map[string]int{"lcp": v, "cls": 50}}); err != nil {
			t.Fatal(err)
		}
	}
	// Out of range is dropped, an unknown metric too: nothing left.
	if err := b.Note("a.test", Sample{Values: map[string]int{"lcp": -1, "fid": 10}}); err != ErrEmpty {
		t.Fatalf("an empty sample was kept: %v", err)
	}
	b.Flush()
	again := New(b.dir)
	again.now = b.now
	rep := again.Report("a.test", day, day)
	lcp := rep.Metrics[0]
	if lcp.Metric != "lcp" || lcp.N != 4 || lcp.Poor != 0.25 || lcp.P75 < 1800 || lcp.P75 > 2000 || lcp.Rating != "good" {
		t.Fatalf("lcp after a restart: %+v", lcp)
	}
	if cls := rep.Metrics[2]; cls.N != 4 || cls.Rating != "good" {
		t.Fatalf("cls: %+v", cls)
	}
	if len(rep.Paths) != 1 || rep.Paths[0].Path != "/p" {
		t.Fatalf("the query was kept, or the page lost: %+v", rep.Paths)
	}
	if len(rep.Months) != 1 || rep.Months[0] != "2026-11" || len(rep.Days) != 1 || rep.Days[0].P75["lcp"] != lcp.P75 {
		t.Fatalf("months/days: %+v %+v", rep.Months, rep.Days)
	}
	if rep.Devices["desktop"] == nil || rep.Devices["mobile"] != nil {
		t.Fatalf("devices: %+v", rep.Devices)
	}
}

func TestOnlyAPageOfTheSiteIsBelieved(t *testing.T) {
	send := func(headers map[string]string) error {
		r := httptest.NewRequest("POST", BeaconPath, strings.NewReader(`{"p":"/","lcp":1234.4,"cls":0.123,"inp":80}`))
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		_, err := ParseBeacon(r, "a.test")
		return err
	}
	if err := send(map[string]string{"Sec-Fetch-Site": "cross-site"}); err != ErrForeign {
		t.Fatalf("another site's page was believed: %v", err)
	}
	if err := send(map[string]string{"Origin": "https://evil.test"}); err != ErrForeign {
		t.Fatalf("another origin was believed: %v", err)
	}
	if err := send(nil); err != ErrForeign {
		t.Fatalf("a beacon saying nothing of where it came from was believed: %v", err)
	}
	if err := send(map[string]string{"Origin": "https://A.test"}); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", BeaconPath, strings.NewReader(`{"p":"/","lcp":1234.4,"cls":0.123}`))
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set("Sec-CH-UA-Mobile", "?1")
	s, err := ParseBeacon(r, "a.test")
	if err != nil || s.Values["lcp"] != 1234 || s.Values["cls"] != 123 || !s.Mobile {
		t.Fatalf("%+v %v", s, err)
	}
}

func serve(t *testing.T, h http.HandlerFunc, ae string) (*http.Response, string, *http.Request) {
	t.Helper()
	var seen *http.Request
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Accept-Encoding", ae)
	rec := httptest.NewRecorder()
	iw, r2, done := Inject(rec, r)
	h(iw, r2)
	seen = r2
	done()
	resp := rec.Result()
	body := io.Reader(resp.Body)
	if resp.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		body = zr
	}
	raw, _ := io.ReadAll(body)
	return resp, string(raw), seen
}

func TestTheScriptGoesIntoTheHeadOfAPageAndNowhereElse(t *testing.T) {
	page := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Length", "999")
		w.Header().Set("ETag", `"abc"`)
		// Written in pieces that cut </head> in two.
		io.WriteString(w, "<!doctype html><html><HEAD><title>x</title></HE")
		io.WriteString(w, "AD><body>hello</body></html>")
	}
	resp, body, seen := serve(t, page, "br, gzip")
	if seen.Header.Get("Accept-Encoding") != "identity" {
		t.Fatalf("the site was asked for a compressed page: %q", seen.Header.Get("Accept-Encoding"))
	}
	want := "<title>x</title>" + Tag + "</HEAD><body>hello"
	if !strings.Contains(body, want) || resp.Header.Get("Content-Encoding") != "gzip" || resp.Header.Get("Content-Length") != "" || resp.Header.Get("ETag") != `W/"abc"` {
		t.Fatalf("%v %q", resp.Header, body)
	}
	if _, body, _ := serve(t, page, ""); !strings.Contains(body, want) {
		t.Fatalf("without gzip: %q", body)
	}
	for name, h := range map[string]http.HandlerFunc{
		"json": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"a":"</head>"}`)
		},
		"404": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(404)
			io.WriteString(w, "<head></head>")
		},
		"compressed anyway": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Content-Encoding", "br")
			io.WriteString(w, "<head></head>")
		},
	} {
		if _, body, _ := serve(t, h, "gzip"); strings.Contains(body, Tag) {
			t.Fatalf("%s: the script went in: %q", name, body)
		}
	}
	// A page with no head at all is passed whole, untouched.
	if _, body, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<p>bare</p>")
	}, ""); body != "<p>bare</p>" {
		t.Fatalf("bare page: %q", body)
	}
}

// A renamed site's measures follow its folder: released, the old name is
// not written again and the new one has what was measured.
func TestReleasedMeasuresAreWrittenAndNotBroughtBack(t *testing.T) {
	dir := t.TempDir()
	b := New(dir)
	day := time.Date(2026, 11, 8, 12, 0, 0, 0, time.Local)
	b.now = func() time.Time { return day }
	if err := b.Note("old.test", Sample{Path: "/", Values: map[string]int{"lcp": 1000}}); err != nil {
		t.Fatal(err)
	}
	b.Release("old.test")
	if err := os.Rename(filepath.Join(dir, "old.test"), filepath.Join(dir, "new.test")); err != nil {
		t.Fatalf("released without writing: %v", err)
	}
	b.Flush()
	if _, err := os.Stat(filepath.Join(dir, "old.test")); !os.IsNotExist(err) {
		t.Fatal("the old name was written again")
	}
	if rep := b.Report("new.test", day, day); len(rep.Metrics) == 0 || rep.Metrics[0].N != 1 {
		t.Fatalf("the new name does not have what was measured: %+v", rep.Metrics)
	}
}
