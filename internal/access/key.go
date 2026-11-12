package access

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/edge"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/pages"
)

// A site's key hides the site the way the panel's key hides the panel
// (internal/inbound/panelkey.go): without it, the site's names answer the
// page of a name the house does not serve, before any other rule of this
// door, so not even "forbidden" or a password box says something is
// there. With it once in the address (?access-token=<key>), the browser
// gets a pass and is sent to the same address without the key.
//
// The manifest keeps the key's SHA-256 alone ("sha256:<hex>"), which is
// enough: the key is 32 random bytes, not a password to guess. The pass
// is signed over the domain and that hash like a password's pass, so a
// new key ends every pass the old one gave, with no list kept.

// KeyParam is the query parameter that carries a site's key, the same
// one the panel's key uses.
const KeyParam = "access-token"

// keyPassTTL is the pass's life: what a browser keeps a cookie at most.
// A key is a link that was handed out on purpose; asking for it again
// every week, like a password's pass, would only make people paste it
// into more places.
const keyPassTTL = 400 * 24 * time.Hour

const keyCookie = "mw_key"

// NewKey makes a site key and the value its manifest keeps.
func NewKey() (key, stored string) {
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	key = base64.RawURLEncoding.EncodeToString(raw)
	return key, HashKey(key)
}

// HashKey is the manifest's form of a key.
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return manifest.KeyPrefix + hex.EncodeToString(sum[:])
}

func keyMatches(stored, key string) bool {
	if stored == "" || key == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(HashKey(key)), []byte(stored)) == 1
}

// keyCookieName is one per level: a service's key and its site's are two
// passes, and the service's must not overwrite the site's.
func keyCookieName(level int) string {
	if level == 0 {
		return keyCookie
	}
	return keyCookie + "_" + strconv.Itoa(level)
}

func (a *Access) signKey(domain, stored string, exp int64) string {
	mac := hmac.New(sha256.New, a.key)
	for _, part := range []string{"key", domain, stored, strconv.FormatInt(exp, 10)} {
		mac.Write([]byte(part))
		mac.Write([]byte{0})
	}
	return strconv.FormatInt(exp, 10) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (a *Access) hasKeyPass(r *http.Request, domain, stored string, level int) bool {
	c, err := r.Cookie(keyCookieName(level))
	if err != nil {
		return false
	}
	expStr, _, ok := strings.Cut(c.Value, ".")
	if !ok {
		return false
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || exp < a.now().Unix() {
		return false
	}
	return hmac.Equal([]byte(a.signKey(domain, stored, exp)), []byte(c.Value))
}

// keyGate answers a request for a site hidden behind a key that does not
// carry it, and says whether it did. levels are the site's access and its
// service's, in that order; each key is asked for on its own.
func (a *Access) keyGate(w http.ResponseWriter, r *http.Request, site Site, levels []*manifest.Access) bool {
	var missing []int
	for i, ac := range levels {
		if ac == nil || ac.Key == "" || a.hasKeyPass(r, site.Domain, ac.Key, i) {
			continue
		}
		missing = append(missing, i)
	}
	if len(missing) == 0 {
		return false
	}
	key := r.URL.Query().Get(KeyParam)
	for _, i := range missing {
		if !keyMatches(levels[i].Key, key) {
			hidden(w, r)
			return true
		}
	}
	exp := a.now().Add(keyPassTTL).Unix()
	for _, i := range missing {
		http.SetCookie(w, &http.Cookie{
			Name: keyCookieName(i), Value: a.signKey(site.Domain, levels[i].Key, exp), Path: "/",
			MaxAge: int(keyPassTTL.Seconds()), HttpOnly: true,
			// Lax: a link to the site from a mail or a chat is a
			// navigation from another site, and Strict would leave the
			// page after it without the pass.
			SameSite: http.SameSiteLaxMode, Secure: edge.HTTPS(r),
		})
	}
	q := r.URL.Query()
	q.Del(KeyParam)
	to := r.URL.Path
	if to == "" {
		to = "/"
	}
	if enc := q.Encode(); enc != "" {
		to += "?" + enc
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, to, http.StatusSeeOther)
	return true
}

// hidden is the page of a name nobody serves, byte for byte: what the
// house answers for a domain it does not have.
func hidden(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	pages.Write(w, r, "unknown", http.StatusNotFound)
}
