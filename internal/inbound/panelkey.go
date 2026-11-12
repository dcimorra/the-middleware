package inbound

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"net"
	"net/http"
	"net/netip"
	"time"

	"github.com/dcimorra/the-middleware/internal/edge"
	"github.com/dcimorra/the-middleware/internal/pages"
)

// The panel's key hides the panel's name from whoever does not hold it:
// without it, middleware.example.com answers the page of a name nobody
// serves, the same page the panel gives outside the VPN when it is kept
// inside one. With it, once, as ?access-token=<key>, the browser gets a
// cookie and the address is sent on without the key, so it does not stay
// in the bar, the history or a Referer.
//
// It is not the panel's login, which still follows: it is that the login
// is not there to be found. So the way back is the shell's, like the
// panel's other fire exits: `middleware panel key off` writes the file
// and the running process reads it on its next request (stateFile).
//
// Only the key's hash is in data/inbound.json, a file other tools read;
// the key itself is shown once, when it is made.

// PanelKeyParam is the query parameter that carries the key.
const PanelKeyParam = "access-token"

// PanelKeyCookie holds the key in the browser that presented it.
const PanelKeyCookie = "mw_panel_key"

// PanelKey is the panel key's part of the state file.
type PanelKey struct {
	// Hash is the hex SHA-256 of the key; empty is no key, the panel
	// answers whoever asks (and then its own rules).
	Hash string `json:"hash,omitempty"`
	// Since is when the key was made.
	Since time.Time `json:"since,omitzero"`
}

// On says a key is set.
func (k PanelKey) On() bool { return k.Hash != "" }

// matches compares in constant time: a key guessed a byte at a time from
// how long a refusal took would be no key.
func (k PanelKey) matches(key string) bool {
	if k.Hash == "" || key == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(hashKey(key)), []byte(k.Hash)) == 1
}

func hashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// NewPanelKey makes a key: 32 random bytes, base64url, which fits in an
// address without escaping.
func NewPanelKey() (key string, k PanelKey) {
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	key = base64.RawURLEncoding.EncodeToString(raw)
	return key, PanelKey{Hash: hashKey(key), Since: time.Now().UTC().Truncate(time.Second)}
}

// SetPanelKey writes a new key in the file, or none with on false, and
// returns the key to show (empty when turned off). For the CLI, which
// works on the file alone.
func SetPanelKey(root string, on bool) (string, error) {
	s, err := LoadState(root)
	if err != nil {
		return "", err
	}
	key := ""
	s.PanelKey = PanelKey{}
	if on {
		key, s.PanelKey = NewPanelKey()
	}
	return key, SaveState(root, s)
}

// PanelKeyState is the running hook's key.
func (h *Hook) PanelKeyState() PanelKey { return h.file.get().PanelKey }

// SetPanelKey is SetPanelKey for the running process: the panel's switch.
func (h *Hook) SetPanelKey(on bool) (string, error) {
	s := h.file.get()
	key := ""
	s.PanelKey = PanelKey{}
	if on {
		key, s.PanelKey = NewPanelKey()
	}
	if err := h.file.set(s); err != nil {
		return "", err
	}
	if on {
		h.emit("operator", "inbound.panel_key", "", "", "the panel now answers only with its key", nil)
	} else {
		h.emit("operator", "inbound.panel_key", "", "", "the panel no longer asks for its key", nil)
	}
	return key, nil
}

// panelKeyGate answers a panel request that does not carry the key and
// says whether it did. A request that brings it in the address is sent
// to the same address without it, with the cookie.
func (h *Hook) panelKeyGate(w http.ResponseWriter, r *http.Request) bool {
	k := h.file.get().PanelKey
	if !k.On() || directFromMachine(r) {
		return false
	}
	if c, err := r.Cookie(PanelKeyCookie); err == nil && k.matches(c.Value) {
		return false
	}
	q := r.URL.Query()
	if key := q.Get(PanelKeyParam); key != "" && k.matches(key) {
		KeyCookie(w, r, key)
		q.Del(PanelKeyParam)
		u := r.URL.Path
		if u == "" {
			u = "/"
		}
		if enc := q.Encode(); enc != "" {
			u += "?" + enc
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		http.Redirect(w, r, u, http.StatusSeeOther)
		return true
	}
	pages.Write(w, r, "unknown", http.StatusNotFound)
	return true
}

// KeyCookie gives the browser the key, so the panel stays open to it.
func KeyCookie(w http.ResponseWriter, r *http.Request, key string) {
	http.SetCookie(w, &http.Cookie{
		Name:  PanelKeyCookie,
		Value: key,
		Path:  "/",
		// The most a browser keeps a cookie (400 days); a new key, or
		// none, is what ends it before.
		MaxAge:   400 * 24 * 3600,
		HttpOnly: true,
		// Lax and not Strict: a link to the panel from a mail or a chat
		// is a top-level navigation from another site, and with Strict
		// the first page after it would not carry the cookie.
		SameSite: http.SameSiteLaxMode,
		Secure:   edge.HTTPS(r),
	})
}

// directFromMachine is a request no edge relayed: the house's unix
// socket, or TCP from loopback with none of the headers a proxy writes.
// The CLI, a site calling its door and an SSH tunnel come this way; the
// key hides the panel from the internet, and what reaches it from this
// machine meets the panel's own rules (internal/auth) as before. A
// tunnel's connector also connects from loopback, but it always says
// whom it relays for.
func directFromMachine(r *http.Request) bool {
	if a, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok && a.Network() == "unix" {
		return true
	}
	if doorOf(r) != Tunnel {
		return false
	}
	ip, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil || !ip.Addr().Unmap().IsLoopback() {
		return false
	}
	for _, hdr := range []string{"Cf-Connecting-Ip", "Cf-Connecting-Ipv6", "X-Forwarded-For", "X-Real-Ip", "True-Client-Ip", "Forwarded"} {
		if r.Header.Get(hdr) != "" {
			return false
		}
	}
	return true
}
