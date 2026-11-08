// Package edge decides which hop in front of the house is believed about the
// client: who it is (CF-Connecting-IP, X-Forwarded-For), where it is
// (CF-IPCountry) and how it came (X-Forwarded-Proto). There is one answer
// for the whole house, here, because three copies of it (the WAF, the
// login brake, the proxy) said three different things in V2's first draft:
// the WAF believed loopback only, the login believed anyone, and the proxy
// copied whatever the client wrote through to the sites.
package edge

import (
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync/atomic"
)

// cloudflareRanges are the addresses Cloudflare's proxy connects from, as
// published at cloudflare.com/ips-v4 and /ips-v6 (copied 7 September 2026
// by V1, unchanged since). Written here, not fetched: this is asked on
// every request and must hold with the network down and before any module
// starts. They change rarely; data/edge/cloudflare-ips.txt (one range per
// line) replaces them without rebuilding, see Load.
var cloudflareRanges = []string{
	"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22",
	"141.101.64.0/18", "108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20",
	"197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
	"104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
	"2400:cb00::/32", "2606:4700::/32", "2803:f800::/32", "2405:b500::/32",
	"2405:8100::/32", "2a06:98c0::/29", "2c0f:f248::/32",
}

// ranges is read by every request and written once at start, hence an
// atomic pointer and no lock.
var ranges atomic.Pointer[[]netip.Prefix]

func init() { ranges.Store(ptr(parseRanges(cloudflareRanges))) }

func ptr[T any](v T) *T { return &v }

func parseRanges(lines []string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(lines))
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if p, err := netip.ParsePrefix(l); err == nil {
			out = append(out, p.Masked())
		}
	}
	return out
}

// Load replaces the edge's ranges with a file's, one per line, when the
// file exists and holds at least one range. A file that does not parse
// keeps the built-in list: a typo there must not mean "trust nobody",
// which with the orange cloud in front means knowing nobody's address.
func Load(path string) (n int, used bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return len(*ranges.Load()), false
	}
	list := parseRanges(strings.Split(string(data), "\n"))
	if len(list) == 0 {
		return len(*ranges.Load()), false
	}
	ranges.Store(&list)
	return len(list), true
}

// fromEdge reports whether ip is loopback (the tunnel, and this machine)
// or one of the edge's ranges (the orange cloud in front of 443).
//
// Loopback is believed because the tunnel's connector is there, and its
// header is the only word on who the visitor is: not believing it would
// make every visitor through the tunnel one address. Every site's process
// is on loopback too and can write the same header, and nothing at the
// TCP level tells the two apart. So what an address decides must not be
// worth more than that: the panel's "this machine" is the unix socket,
// not loopback (auth.FromMachine), and the login has a budget for the
// whole house beside each address's (auth's houseLoginPolicy).
func fromEdge(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	a = a.Unmap()
	for _, p := range *ranges.Load() {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Remote is the address of the hop that opened the connection.
func Remote(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return net.ParseIP(strings.Trim(host, "[]"))
}

// Trusted reports whether the hop that opened the connection may speak for
// the client: loopback (the tunnel's connector, a proxy on this machine)
// or the edge's own ranges. A client reaching the 80/443 door directly is
// neither, so whatever it writes in those headers is ignored and removed.
func Trusted(r *http.Request) bool { return fromEdge(Remote(r)) }

// ClientIP is the address a request really came from. From a trusted hop
// the edge's header is believed: CF-Connecting-IP first, then the last
// non-loopback hop of X-Forwarded-For, which is the one the trusted proxy
// itself appended. From anywhere else the headers are ignored, because a
// client can write them.
func ClientIP(r *http.Request) net.IP {
	remote := Remote(r)
	if !fromEdge(remote) {
		return remote
	}
	if cf := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); cf != "" {
		if ip := net.ParseIP(cf); ip != nil {
			return ip
		}
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			if ip := net.ParseIP(strings.TrimSpace(parts[i])); ip != nil && !fromEdge(ip) {
				return ip
			}
		}
	}
	return remote
}

// Country is the edge's country code for the client, or "" when the hop
// is not trusted to say.
func Country(r *http.Request) string {
	if !Trusted(r) {
		return ""
	}
	return strings.ToUpper(strings.TrimSpace(r.Header.Get("CF-IPCountry")))
}

// HTTPS reports whether the client's hop was TLS: this connection, or the
// trusted edge saying so.
func HTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return Trusted(r) && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// spoofable are the headers a site may read as "who is the client". From
// an untrusted hop they are the client's own words and are removed before
// the request reaches a site; X-Forwarded-For is rebuilt by the proxy.
var spoofable = []string{
	"CF-Connecting-IP", "CF-Connecting-IPv6", "CF-IPCountry", "CF-Ray", "CF-Visitor",
	"CF-Worker", "True-Client-IP", "X-Real-IP", "X-Forwarded-For", "X-Forwarded-Host",
	"X-Forwarded-Proto", "X-Client-IP", "Forwarded",
}

// Scrub prepares the headers a site will see. From an untrusted hop the
// edge headers are dropped; in every case CF-Connecting-IP and X-Real-IP
// are rewritten to the address this package believes, so a site that
// reads either gets the same answer the house acted on.
func Scrub(in *http.Request, out http.Header) {
	if !Trusted(in) {
		for _, h := range spoofable {
			out.Del(h)
		}
	}
	if ip := ClientIP(in); ip != nil {
		out.Set("CF-Connecting-IP", ip.String())
		out.Set("X-Real-IP", ip.String())
	}
}

// Host is the name a request is for: the Host header without its port,
// lower-case, without the trailing dot of a fully qualified name. Every
// door looks a site up by it, so they must all spell it the same way: a
// door that kept "Example.com." would miss the site the router served.
func Host(r *http.Request) string {
	h := r.Host
	if i := strings.LastIndexByte(h, ':'); i > 0 && !strings.Contains(h[i:], "]") {
		h = h[:i]
	}
	return strings.ToLower(strings.TrimSuffix(h, "."))
}
