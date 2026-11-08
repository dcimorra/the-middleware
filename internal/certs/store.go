// Package certs keeps the certificates the ports door serves: one folder per
// domain under data/certs, an in-memory cache in front, on-demand issuance
// the first time a name is asked for, and a renewal loop. Only names the
// installation serves are ever requested, because a stranger pointing DNS
// at this server must not be able to spend the CA's quota.
package certs

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/acme"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/manifest"
)

// Issuer obtains a chain for one domain with the given key. The ACME
// issuer is the one that ships; tests plug a fake.
type Issuer interface {
	Issue(ctx context.Context, domain string, key crypto.Signer) (chainPEM []byte, err error)
}

// Errors callers branch on.
var (
	ErrNotAllowed = errors.New("certs: domain is not served by this installation")
	ErrBackoff    = errors.New("certs: last attempt failed recently, not retrying yet")
	ErrNoIssuer   = errors.New("certs: no issuer configured")
)

// Info describes one stored certificate.
type Info struct {
	Domain  string    `json:"domain"`
	Issued  time.Time `json:"issued"`
	Expires time.Time `json:"expires"`
	Issuer  string    `json:"issuer"`
}

// Store is the certificate store. Fields are set before Run or the first
// GetCertificate and not changed after.
type Store struct {
	// Dir is data/certs.
	Dir string
	// Issuer obtains certificates. Nil means the store only serves what is
	// on disk (the CLI uses it that way).
	Issuer Issuer
	// Allowed says which names may be requested. Nil allows none.
	Allowed func(domain string) bool
	// Bus receives cert.issued, cert.renewed and cert.failed. May be nil.
	Bus *events.Bus
	// ALPN, when set, answers a CA probing TLS-ALPN-01 before anything
	// else is looked at.
	ALPN *acme.TLSALPN01
	// RenewBefore is how long before expiry renewal starts. Default 30 days.
	RenewBefore time.Duration
	// Backoff is how long a failed domain is left alone. Default 1 hour.
	Backoff time.Duration
	// IssueTimeout bounds how long a handshake waits for on-demand
	// issuance before getting the fallback. Default 30 seconds. The order
	// itself goes on in the background.
	IssueTimeout time.Duration
	// CheckEvery is the renewal loop's period. Default 1 hour.
	CheckEvery time.Duration

	mu       sync.Mutex
	cache    map[string]*tls.Certificate
	inflight map[string]*call
	failed   map[string]time.Time
	fallback *tls.Certificate
}

// call is one issuance in flight; waiters share its result.
type call struct {
	done chan struct{}
	cert *tls.Certificate
	err  error
}

// New returns a store over dir.
func New(dir string, issuer Issuer) *Store {
	return &Store{
		Dir:          dir,
		Issuer:       issuer,
		RenewBefore:  30 * 24 * time.Hour,
		Backoff:      time.Hour,
		IssueTimeout: 30 * time.Second,
		CheckEvery:   time.Hour,
		cache:        map[string]*tls.Certificate{},
		inflight:     map[string]*call{},
		failed:       map[string]time.Time{},
	}
}

func (s *Store) domainDir(domain string) string { return filepath.Join(s.Dir, domain) }
func (s *Store) keyPath(domain string) string   { return filepath.Join(s.domainDir(domain), "key.pem") }
func (s *Store) chainPath(domain string) string {
	return filepath.Join(s.domainDir(domain), "chain.pem")
}

func (s *Store) allowed(domain string) bool {
	return manifest.ValidDomain(domain) && s.Allowed != nil && s.Allowed(domain)
}

// GetCertificate is for tls.Config. It never returns an error: a handshake
// that fails shows the visitor nothing, while the fallback certificate
// lets the "no certificate yet" page through, which says what is going
// on. Unknown names get the fallback without any issuance.
func (s *Store) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if s.ALPN != nil {
		if c := s.ALPN.GetCertificate(hello); c != nil {
			return c, nil
		}
	}
	domain := strings.ToLower(strings.TrimSuffix(hello.ServerName, "."))
	if domain == "" || !s.allowed(domain) {
		if c := s.cached(domain); c != nil {
			return c, nil
		}
		return s.selfSigned()
	}
	ctx := hello.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, s.IssueTimeout)
	defer cancel()
	c, err := s.Obtain(ctx, domain)
	if err != nil {
		return s.selfSigned()
	}
	return c, nil
}

// Has reports whether a certificate for domain is loaded or on disk.
func (s *Store) Has(domain string) bool { return s.cached(domain) != nil }

// cached returns the certificate from memory or disk, nil when absent. An
// expired certificate is still returned: it is better than the fallback
// while renewal catches up.
func (s *Store) cached(domain string) *tls.Certificate {
	s.mu.Lock()
	c := s.cache[domain]
	s.mu.Unlock()
	if c != nil {
		return c
	}
	c, err := s.load(domain)
	if err != nil {
		return nil
	}
	s.mu.Lock()
	s.cache[domain] = c
	s.mu.Unlock()
	return c
}

func (s *Store) load(domain string) (*tls.Certificate, error) {
	if !manifest.ValidDomain(domain) {
		return nil, os.ErrNotExist
	}
	chain, err := os.ReadFile(s.chainPath(domain))
	if err != nil {
		return nil, err
	}
	key, err := os.ReadFile(s.keyPath(domain))
	if err != nil {
		return nil, err
	}
	c, err := tls.X509KeyPair(chain, key)
	if err != nil {
		return nil, fmt.Errorf("certs: %s: %w", domain, err)
	}
	c.Leaf, err = x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// Obtain returns the certificate for domain, issuing one when there is
// none. Concurrent callers for the same domain share one order; a domain
// that failed within Backoff is refused without touching the CA. When ctx
// ends before the order does, the order carries on and the next caller
// finds the result.
func (s *Store) Obtain(ctx context.Context, domain string) (*tls.Certificate, error) {
	domain = strings.ToLower(domain)
	if c := s.cached(domain); c != nil {
		return c, nil
	}
	if !s.allowed(domain) {
		return nil, ErrNotAllowed
	}
	return s.issue(ctx, domain, false)
}

// Renew obtains a new certificate for domain regardless of what is stored.
func (s *Store) Renew(ctx context.Context, domain string) error {
	domain = strings.ToLower(domain)
	if !s.allowed(domain) {
		return ErrNotAllowed
	}
	_, err := s.issue(ctx, domain, true)
	return err
}

func (s *Store) issue(ctx context.Context, domain string, renew bool) (*tls.Certificate, error) {
	if s.Issuer == nil {
		return nil, ErrNoIssuer
	}
	s.mu.Lock()
	if c, ok := s.inflight[domain]; ok {
		s.mu.Unlock()
		return s.wait(ctx, c)
	}
	if t, ok := s.failed[domain]; ok && time.Since(t) < s.Backoff {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w (%s ago)", ErrBackoff, time.Since(t).Round(time.Second))
	}
	c := &call{done: make(chan struct{})}
	s.inflight[domain] = c
	s.mu.Unlock()
	// The order is detached from the caller: a handshake that gives up
	// after IssueTimeout must not cancel a validation the CA is halfway
	// through, or every first visit would start one and finish none.
	go s.run(context.WithoutCancel(ctx), domain, renew, c)
	return s.wait(ctx, c)
}

func (s *Store) wait(ctx context.Context, c *call) (*tls.Certificate, error) {
	select {
	case <-c.done:
		return c.cert, c.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Store) run(ctx context.Context, domain string, renew bool, c *call) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cert, err := s.order(ctx, domain)
	s.mu.Lock()
	delete(s.inflight, domain)
	if err != nil {
		s.failed[domain] = time.Now()
	} else {
		delete(s.failed, domain)
		s.cache[domain] = cert
	}
	s.mu.Unlock()
	// The event goes out before the waiters are released: whoever asked
	// for the certificate must find the journal already saying what
	// happened, or an API that reads both would see a result with no trace.
	typ := "cert.issued"
	if renew {
		typ = "cert.renewed"
	}
	if err != nil {
		s.emit("cert.failed", domain, "error", err.Error(), map[string]any{"renew": renew})
	} else {
		s.emit(typ, domain, "", "", map[string]any{"expires": cert.Leaf.NotAfter, "issuer": cert.Leaf.Issuer.CommonName})
	}
	c.cert, c.err = cert, err
	close(c.done)
}

// order is one issuance from key to files. The key is fresh each time: a
// renewal that reuses the key would keep an old compromise alive.
func (s *Store) order(ctx context.Context, domain string) (*tls.Certificate, error) {
	key, err := acme.GenerateKey()
	if err != nil {
		return nil, err
	}
	chain, err := s.Issuer.Issue(ctx, domain, key)
	if err != nil {
		return nil, err
	}
	keyPEM, err := acme.MarshalKeyPEM(key)
	if err != nil {
		return nil, err
	}
	cert, err := tls.X509KeyPair(chain, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("certs: issuer returned a chain that does not match the key: %w", err)
	}
	cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, err
	}
	if err := cert.Leaf.VerifyHostname(domain); err != nil {
		return nil, fmt.Errorf("certs: issuer returned a certificate for the wrong name: %w", err)
	}
	// The key is written first and the chain last, both private: a reader
	// that finds a chain knows its key is already there.
	if err := os.MkdirAll(s.domainDir(domain), 0o700); err != nil {
		return nil, err
	}
	if err := fsx.WriteAtomic(s.keyPath(domain), keyPEM, 0o600); err != nil {
		return nil, err
	}
	if err := fsx.WriteAtomic(s.chainPath(domain), chain, 0o600); err != nil {
		return nil, err
	}
	return &cert, nil
}

func (s *Store) emit(typ, domain, level, msg string, data map[string]any) {
	if s.Bus == nil {
		return
	}
	s.Bus.Emit(events.Event{Type: typ, Subject: domain, Actor: "engine", Level: level, Message: msg, Data: data})
}

// Info returns what is stored for domain.
func (s *Store) Info(domain string) (Info, bool) {
	c := s.cached(strings.ToLower(domain))
	if c == nil || c.Leaf == nil {
		return Info{}, false
	}
	return Info{Domain: domain, Issued: c.Leaf.NotBefore, Expires: c.Leaf.NotAfter, Issuer: c.Leaf.Issuer.CommonName}, true
}

// List returns every certificate on disk, sorted by domain.
func (s *Store) List() []Info {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return nil
	}
	var out []Info
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if info, ok := s.Info(e.Name()); ok {
			out = append(out, info)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Domain < out[j].Domain })
	return out
}

// Remove deletes a domain's files and forgets it. The next request for
// the name issues again, so this is for a domain that left the ports door
// for good, not a way to force renewal.
func (s *Store) Remove(domain string) error {
	domain = strings.ToLower(domain)
	if !manifest.ValidDomain(domain) {
		return fmt.Errorf("certs: invalid domain %q", domain)
	}
	s.mu.Lock()
	delete(s.cache, domain)
	delete(s.failed, domain)
	s.mu.Unlock()
	return os.RemoveAll(s.domainDir(domain))
}

// Due returns the stored domains whose certificate expires within
// RenewBefore and that are still allowed. A domain no longer served is
// left to expire: renewing it would be exactly the quota spend the
// Allowed check exists to prevent.
func (s *Store) Due(now time.Time) []string {
	var due []string
	for _, info := range s.List() {
		if !s.allowed(info.Domain) {
			continue
		}
		if now.Add(s.RenewBefore).After(info.Expires) {
			due = append(due, info.Domain)
		}
	}
	return due
}

// RenewDue renews everything Due, one domain at a time so a CA hiccup
// costs one order and not one per site. Failures are events, not errors.
func (s *Store) RenewDue(ctx context.Context) {
	for _, domain := range s.Due(time.Now()) {
		if ctx.Err() != nil {
			return
		}
		_ = s.Renew(ctx, domain)
	}
}

// Run is the renewal loop. It checks once at start, because a server that
// was off for a month must not wait another hour to notice.
func (s *Store) Run(ctx context.Context) {
	s.RenewDue(ctx)
	t := time.NewTicker(s.CheckEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.RenewDue(ctx)
		}
	}
}

// selfSigned is the certificate for names without one. It exists so the
// TLS handshake completes and the visitor reaches a page that explains,
// instead of a browser error that explains nothing. One shared
// certificate, generated once: a stranger scanning names must not be able
// to make this process mint keys on every hello.
func (s *Store) selfSigned() (*tls.Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fallback != nil {
		return s.fallback, nil
	}
	key, err := acme.GenerateKey()
	if err != nil {
		return nil, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 100))
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "middleware (no certificate yet)"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour * 3650),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	leaf, _ := x509.ParseCertificate(der)
	s.fallback = &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
	return s.fallback, nil
}

// IsFallback reports whether c is the shared self-signed certificate.
func (s *Store) IsFallback(c *tls.Certificate) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return c != nil && c == s.fallback
}
