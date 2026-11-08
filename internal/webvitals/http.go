package webvitals

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Script measures the page and sends what it measured once, when the
// page is hidden (the moment web.dev's own library reports at: LCP and
// CLS are only final then). INP is the longest interaction seen, which is
// the definition's answer for any page with fewer than fifty of them, and
// that is nearly every page. CLS starts at 0 where the browser measures
// it, so a page that never shifts counts as the good page it is instead
// of leaving the shifting ones alone in the percentile. It is served from
// the site's own origin, so a Content-Security-Policy that allows 'self'
// allows it.
const Script = `(()=>{if(!("PerformanceObserver"in window)||!navigator.sendBeacon)return;
var v={},sent=false;
function o(t,f,x){try{var p=new PerformanceObserver(function(l){l.getEntries().forEach(f)});p.observe(Object.assign({type:t,buffered:true},x||{}))}catch(e){}}
o("largest-contentful-paint",function(e){v.lcp=Math.round(e.startTime)});
var s=0,first=0,last=0,cls=0;
if((PerformanceObserver.supportedEntryTypes||[]).indexOf("layout-shift")>=0)v.cls=0;
o("layout-shift",function(e){if(e.hadRecentInput)return;if(s&&e.startTime-last<1000&&e.startTime-first<5000){s+=e.value}else{s=e.value;first=e.startTime}last=e.startTime;if(s>cls){cls=s;v.cls=Math.round(cls*1000)/1000}});
o("paint",function(e){if(e.name==="first-contentful-paint")v.fcp=Math.round(e.startTime)});
o("event",function(e){if(e.interactionId&&(!v.inp||e.duration>v.inp))v.inp=Math.round(e.duration)},{durationThreshold:16});
var n=performance.getEntriesByType&&performance.getEntriesByType("navigation")[0];if(n&&n.responseStart>0)v.ttfb=Math.round(n.responseStart);
function send(){if(sent)return;sent=true;v.p=location.pathname;navigator.sendBeacon("` + BeaconPath + `",JSON.stringify(v))}
addEventListener("visibilitychange",function(){if(document.visibilityState==="hidden")send()});
addEventListener("pagehide",send)})();
`

// Tag is what goes into the page.
const Tag = `<script src="` + ScriptPath + `" defer></script>`

// ServeScript answers ScriptPath. A day of cache: the script changes with
// a release, not with a page.
func ServeScript(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = io.WriteString(w, Script)
}

// ErrForeign is a beacon that did not come from a page of the site.
var ErrForeign = errors.New("webvitals: not sent by a page of this site")

// maxBeacon is far over what the script sends (under 200 bytes).
const maxBeacon = 2048

// ParseBeacon reads what a page sent. Only a page of the site itself is
// believed: the browser says so in Sec-Fetch-Site (or, older ones, in
// Origin), which a page of another site cannot make it say. Anyone can
// still send numbers by hand, as with any analytics; the range checks in
// Note keep one from moving a percentile far.
func ParseBeacon(r *http.Request, host string) (Sample, error) {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin":
	case "":
		o, err := url.Parse(r.Header.Get("Origin"))
		if err != nil || o.Host == "" || !strings.EqualFold(o.Hostname(), host) {
			return Sample{}, ErrForeign
		}
	default:
		return Sample{}, ErrForeign
	}
	var in struct {
		P    string   `json:"p"`
		LCP  *float64 `json:"lcp"`
		INP  *float64 `json:"inp"`
		CLS  *float64 `json:"cls"`
		FCP  *float64 `json:"fcp"`
		TTFB *float64 `json:"ttfb"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBeacon)).Decode(&in); err != nil {
		return Sample{}, err
	}
	s := Sample{Path: in.P, Values: map[string]int{}, Mobile: mobile(r)}
	put := func(k string, v *float64, scale float64) {
		if v != nil && !math.IsNaN(*v) && !math.IsInf(*v, 0) {
			s.Values[k] = int(math.Round(*v * scale))
		}
	}
	put("lcp", in.LCP, 1)
	put("inp", in.INP, 1)
	put("cls", in.CLS, 1000)
	put("fcp", in.FCP, 1)
	put("ttfb", in.TTFB, 1)
	return s, nil
}

func mobile(r *http.Request) bool {
	if v := r.Header.Get("Sec-CH-UA-Mobile"); v != "" {
		return v == "?1"
	}
	ua := r.UserAgent()
	return strings.Contains(ua, "Mobi") || strings.Contains(ua, "Android")
}

// Wants says whether the answer to r is a page the script goes into: a
// browser opening a document, the whole of it.
func Wants(r *http.Request) bool {
	if r.Method != http.MethodGet || r.Header.Get("Range") != "" {
		return false
	}
	return r.Header.Get("Sec-Fetch-Dest") == "document"
}

// maxScan is how far into a page </head> is looked for. A head longer
// than this is a page streamed in pieces, and holding it back longer
// would delay its first paint for a measure of it.
const maxScan = 256 << 10

// Inject returns the writer the page is written to and a function to call
// when the handler is done. The page is asked for without compression
// (the house would have to undo it to find </head>) and compressed again
// here with gzip when the browser takes it, so what reaches the browser
// is as small as it would have been, minus brotli.
func Inject(w http.ResponseWriter, r *http.Request) (http.ResponseWriter, *http.Request, func()) {
	gz := acceptsGzip(r.Header.Get("Accept-Encoding"))
	r2 := r.Clone(r.Context())
	r2.Header.Set("Accept-Encoding", "identity")
	iw := &injector{w: w, gzip: gz}
	return iw, r2, iw.finish
}

func acceptsGzip(ae string) bool {
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

// injector holds the start of an HTML page until </head> is in it, puts
// the tag before it, and passes the rest through. Anything that is not a
// whole HTML page answered 200 passes through untouched, from the first
// byte.
type injector struct {
	w       http.ResponseWriter
	gzip    bool
	status  int
	decided bool
	inject  bool // the page is HTML and the tag still has to go in
	buf     bytes.Buffer
	out     io.Writer
	zw      *gzip.Writer
}

func (i *injector) Header() http.Header { return i.w.Header() }

func (i *injector) WriteHeader(code int) {
	if i.decided {
		return
	}
	i.status = code
	if code >= 100 && code < 200 {
		i.w.WriteHeader(code)
		return
	}
	i.decide()
}

func (i *injector) decide() {
	i.decided = true
	if i.status == 0 {
		i.status = http.StatusOK
	}
	h := i.w.Header()
	ct := strings.ToLower(h.Get("Content-Type"))
	i.inject = i.status == http.StatusOK && strings.HasPrefix(ct, "text/html") && h.Get("Content-Encoding") == ""
	i.out = i.w
	if !i.inject {
		i.w.WriteHeader(i.status)
		return
	}
	h.Del("Content-Length")
	// The page is not the one the site sent any more: a strong validator
	// would claim byte-for-byte sameness that is not true.
	if et := h.Get("ETag"); et != "" && !strings.HasPrefix(et, "W/") {
		h.Set("ETag", "W/"+et)
	}
	h.Add("Vary", "Accept-Encoding")
	if i.gzip {
		h.Set("Content-Encoding", "gzip")
		i.zw = gzip.NewWriter(i.w)
		i.out = i.zw
	}
	i.w.WriteHeader(i.status)
}

func (i *injector) Write(b []byte) (int, error) {
	if !i.decided {
		i.decide()
	}
	if !i.inject {
		return i.out.Write(b)
	}
	i.buf.Write(b)
	if idx := headEnd(i.buf.Bytes()); idx >= 0 {
		return len(b), i.release(idx)
	}
	if i.buf.Len() > maxScan {
		return len(b), i.release(-1)
	}
	return len(b), nil
}

// release writes what was held, with the tag at idx (-1: without it), and
// leaves the writer passing through.
func (i *injector) release(idx int) error {
	i.inject = false
	held := i.buf.Bytes()
	var err error
	if idx >= 0 {
		if _, err = i.out.Write(held[:idx]); err == nil {
			if _, err = io.WriteString(i.out, Tag); err == nil {
				_, err = i.out.Write(held[idx:])
			}
		}
	} else {
		_, err = i.out.Write(held)
	}
	i.buf = bytes.Buffer{}
	return err
}

// headEnd is where </head> starts, in any case.
func headEnd(b []byte) int {
	for off := 0; ; {
		j := bytes.Index(b[off:], []byte("</"))
		if j < 0 {
			return -1
		}
		k := off + j
		if len(b) >= k+7 && bytes.EqualFold(b[k:k+7], []byte("</head>")) {
			return k
		}
		off = k + 2
	}
}

// Flush gives up on the tag when the page asks for what it wrote to go
// out: a page that streams its head wants it painted, not measured.
func (i *injector) Flush() {
	if !i.decided {
		i.decide()
	}
	if i.inject {
		_ = i.release(-1)
	}
	if i.zw != nil {
		_ = i.zw.Flush()
	}
	if f, ok := i.w.(http.Flusher); ok {
		f.Flush()
	}
}

func (i *injector) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := i.w.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return h.Hijack()
}

func (i *injector) Unwrap() http.ResponseWriter { return i.w }

func (i *injector) finish() {
	if !i.decided {
		// The handler wrote nothing at all: its answer is still its own.
		if i.status == 0 {
			return
		}
		i.decide()
	}
	if i.inject {
		_ = i.release(-1)
	}
	if i.zw != nil {
		_ = i.zw.Close()
	}
}
