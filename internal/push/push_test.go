package push

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/events"
)

func mustB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestTheRFC8291ExampleComesOutByteForByte runs Appendix A of RFC 8291:
// its keys, its salt, its message, and the body it says must come out.
func TestTheRFC8291ExampleComesOutByteForByte(t *testing.T) {
	as, err := ecdh.P256().NewPrivateKey(mustB64(t, "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"))
	if err != nil {
		t.Fatal(err)
	}
	ua, err := ecdh.P256().NewPublicKey(mustB64(t, "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := encrypt([]byte("When I grow up, I want to be a watermelon"), ua, as, mustB64(t, "BTBZMqHH6r4Tts7J_aSIgg"), mustB64(t, "DGv6ra1nlYgDCS1FRnbzlw"))
	if err != nil {
		t.Fatal(err)
	}
	want := "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN"
	if g := base64.RawURLEncoding.EncodeToString(got); g != want {
		t.Fatalf("got  %s\nwant %s", g, want)
	}
}

// decrypt is the browser's side, for the round trip.
func decrypt(t *testing.T, body []byte, ua *ecdh.PrivateKey, auth []byte) []byte {
	t.Helper()
	salt, idlen := body[:16], int(body[20])
	asPub, ct := body[21:21+idlen], body[21+idlen:]
	as, err := ecdh.P256().NewPublicKey(asPub)
	if err != nil {
		t.Fatal(err)
	}
	shared, _ := ua.ECDH(as)
	prkKey, _ := hkdf.Extract(sha256.New, shared, auth)
	ikm, _ := hkdf.Expand(sha256.New, prkKey, "WebPush: info\x00"+string(ua.PublicKey().Bytes())+string(asPub), 32)
	prk, _ := hkdf.Extract(sha256.New, ikm, salt)
	cek, _ := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pt[len(pt)-1] != 0x02 {
		t.Fatal("no last-record delimiter")
	}
	return pt[:len(pt)-1]
}

// device is a push service and one browser subscribed to it.
type device struct {
	key  *ecdh.PrivateKey
	auth []byte
	got  chan *http.Request
	body chan []byte
	srv  *httptest.Server
	code int
}

func newDevice(t *testing.T) *device {
	d := &device{got: make(chan *http.Request, 8), body: make(chan []byte, 8), code: 201}
	d.key, _ = ecdh.P256().GenerateKey(rand.Reader)
	d.auth = make([]byte, 16)
	rand.Read(d.auth)
	d.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		d.got <- r
		d.body <- b
		w.WriteHeader(d.code)
	}))
	t.Cleanup(d.srv.Close)
	return d
}

func (d *device) sub(kinds ...string) Subscription {
	s := Subscription{Endpoint: d.srv.URL + "/push/abc", Kinds: kinds}
	s.Keys.P256dh = base64.RawURLEncoding.EncodeToString(d.key.PublicKey().Bytes())
	s.Keys.Auth = base64.RawURLEncoding.EncodeToString(d.auth)
	return s
}

func TestAPushIsEncryptedSignedAndForgottenWhenGone(t *testing.T) {
	dir := t.TempDir()
	d := newDevice(t)
	s, err := New(Options{DataDir: dir, Client: d.srv.Client(), Subject: "mailto:ops@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Subscribe(d.sub()); err != nil {
		t.Fatal(err)
	}
	n, err := s.Send(context.Background(), Message{Kind: KindIntrusion, Title: "Intrusion on a.example", URL: "/#/x"})
	if err != nil || n != 1 {
		t.Fatalf("sent %d: %v", n, err)
	}
	r, body := <-d.got, <-d.body
	var m Message
	if err := json.Unmarshal(decrypt(t, body, d.key, d.auth), &m); err != nil || m.Title != "Intrusion on a.example" {
		t.Fatalf("the device read %+v %v", m, err)
	}
	if r.Header.Get("Content-Encoding") != "aes128gcm" || r.Header.Get("Urgency") != "high" || r.Header.Get("TTL") == "" {
		t.Fatalf("headers: %v", r.Header)
	}
	// The VAPID token: for this push service, signed by the key the
	// browser was given.
	auth := r.Header.Get("Authorization")
	tok := strings.TrimSuffix(strings.TrimPrefix(strings.Split(auth, ", k=")[0], "vapid t="), ",")
	k := strings.Split(auth, ", k=")[1]
	if k != s.PublicKey() {
		t.Fatalf("k=%s, the key is %s", k, s.PublicKey())
	}
	parts := strings.Split(tok, ".")
	var claims struct {
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
		Sub string `json:"sub"`
	}
	json.Unmarshal(mustB64(t, parts[1]), &claims)
	if claims.Aud != d.srv.URL || claims.Sub != "mailto:ops@example.com" || claims.Exp < time.Now().Unix() {
		t.Fatalf("claims %+v", claims)
	}
	pub := mustB64(t, k)
	x, y := elliptic.Unmarshal(elliptic.P256(), pub) //nolint:staticcheck
	sig := mustB64(t, parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(&ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("the VAPID signature does not verify")
	}
	// The key survives a restart: the browsers tie the subscription to it.
	s2, _ := New(Options{DataDir: dir, Client: d.srv.Client()})
	if s2.PublicKey() != s.PublicKey() {
		t.Fatal("a restart made a new VAPID key")
	}
	// A device that chose other kinds does not get this one.
	s.Subscribe(d.sub(KindRepo))
	if n, _ := s.Send(context.Background(), Message{Kind: KindTurn, Title: "x"}); n != 0 {
		t.Fatalf("a turn went to a device that wants repos only")
	}
	// Gone (410): forgotten.
	d.code = http.StatusGone
	s.Send(context.Background(), Message{Kind: KindRepo, Title: "x"})
	<-d.got
	if len(s.Subscriptions()) != 0 {
		t.Fatal("a device the push service says is gone was kept")
	}
}

func TestOnlyTheListedEventsArePushed(t *testing.T) {
	cases := map[string]bool{"guard.intrusion": true, "guard.suspicious": false, "repo.failed": true, "repo.pulled": false, "middleware.panic": true, "instance.failed": false}
	for typ, want := range cases {
		if _, ok := For(events.Event{Type: typ, Subject: "a.example"}); ok != want {
			t.Errorf("%s: pushed %v", typ, ok)
		}
	}
	if _, ok := For(events.Event{Type: "agent.done", Subject: "a.example#12ab", Data: map[string]any{"origin": "site"}}); ok {
		t.Error("a site door's turn was pushed")
	}
	// The autonomous agent works with nobody waiting: neither its turns
	// nor the ones it hands to sites push; a site waiting to go public does.
	if _, ok := For(events.Event{Type: "agent.done", Subject: "autopilot#12ab", Data: map[string]any{"origin": "panel"}}); ok {
		t.Error("an autonomous agent's turn was pushed")
	}
	if _, ok := For(events.Event{Type: "agent.done", Subject: "a.example#12ab", Data: map[string]any{"origin": "autopilot"}}); ok {
		t.Error("a site turn the autonomous agent handed out was pushed")
	}
	if m, ok := For(events.Event{Type: "autopilot.ready", Subject: "a.example"}); !ok || m.URL != "/#/agents/network" {
		t.Errorf("a finished site waiting: %+v %v", m, ok)
	}
	m, ok := For(events.Event{Type: "agent.done", Subject: "a.example#12ab", Data: map[string]any{"origin": "panel", "text": "Done.\nMore"}})
	if !ok || m.Body != "Done." || m.URL != "/#/sites/a.example/chat?c=12ab" {
		t.Errorf("a panel turn: %+v %v", m, ok)
	}
	bad := Subscription{Endpoint: "http://insecure.example/x"}
	if (&Sender{}).Subscribe(bad) == nil {
		t.Error("an http endpoint was accepted")
	}
}
