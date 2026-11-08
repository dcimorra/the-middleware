package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/runtime"
)

// canaryRun is a new generation serving part of the traffic while the
// one before serves the rest (manifest.Canary). The counts are the
// proxy's (NoteCanary), by side.
type canaryRun struct {
	inst     runtime.Instance
	cfg      manifest.Canary
	started  time.Time
	until    time.Time
	revision string

	reqs, errs             atomic.Int64
	stableReqs, stableErrs atomic.Int64
	// decide carries the operator's word: true promotes now, false rolls
	// back now.
	decide chan bool
}

// CanarySnapshot is a canary in progress, for the proxy and the panel.
type CanarySnapshot struct {
	Port           int       `json:"port"`
	Percent        int       `json:"percent"`
	Revision       string    `json:"revision"`
	Started        time.Time `json:"started"`
	Until          time.Time `json:"until"`
	Requests       int64     `json:"requests"`
	Errors         int64     `json:"errors"`
	StableRequests int64     `json:"stable_requests"`
	StableErrors   int64     `json:"stable_errors"`
}

func (c *canaryRun) snapshot() *CanarySnapshot {
	return &CanarySnapshot{Port: c.inst.Port(), Percent: c.cfg.Share(), Revision: c.revision, Started: c.started, Until: c.until,
		Requests: c.reqs.Load(), Errors: c.errs.Load(), StableRequests: c.stableReqs.Load(), StableErrors: c.stableErrs.Load()}
}

// CanaryFailure is the last generation a canary turned away. It is
// written down, because a decision that does not survive a restart is
// not a decision: without it the next tick, or the next supervisor,
// would start the same generation again and send visitors to it.
type CanaryFailure struct {
	Revision string    `json:"revision"`
	At       time.Time `json:"at"`
	Reason   string    `json:"reason"`
}

func (e *Engine) canaryPath(domain string) string {
	return filepath.Join(e.opts.DataDir, "canary", domain+".json")
}

// CanaryFailed says which generation of domain was rolled back, if any.
func (e *Engine) CanaryFailed(domain string) (CanaryFailure, bool) {
	var f CanaryFailure
	if err := fsx.ReadJSON(e.canaryPath(domain), &f); err != nil || f.Revision == "" {
		return CanaryFailure{}, false
	}
	return f, true
}

func (e *Engine) clearCanaryFailure(domain string) { _ = os.Remove(e.canaryPath(domain)) }

// NoteCanary counts one answer of a site in canary: on the canary's side
// or the serving one's, and whether it was an error. Called by the proxy
// for every request of such a site, so it takes the read lock only.
func (e *Engine) NoteCanary(domain string, canary bool, status int) {
	e.mu.RLock()
	s, ok := e.sites[domain]
	var c *canaryRun
	if ok {
		c = s.canary
	}
	e.mu.RUnlock()
	if c == nil {
		return
	}
	failed := status >= 500 || status == 0
	if canary {
		c.reqs.Add(1)
		if failed {
			c.errs.Add(1)
		}
		return
	}
	c.stableReqs.Add(1)
	if failed {
		c.stableErrs.Add(1)
	}
}

// ErrNoCanary is a decision asked of a site with no canary running.
var ErrNoCanary = errors.New("no canary is running for this site")

// DecideCanary promotes the running canary of domain now, or rolls it
// back now.
func (e *Engine) DecideCanary(domain string, promote bool) error {
	e.mu.RLock()
	s, ok := e.sites[domain]
	var c *canaryRun
	if ok {
		c = s.canary
	}
	e.mu.RUnlock()
	if c == nil {
		return ErrNoCanary
	}
	select {
	case c.decide <- promote:
	default:
		// A decision is already on its way.
	}
	return nil
}

// canaryEvery is how often a running canary is judged.
var canaryEvery = 2 * time.Second

// runCanary serves inst beside the site's current generation and says
// whether it is to take over. cancelled is the supervisor stopping: the
// canary is then stopped like any generation that had not switched yet,
// and the next supervisor starts again from the generation in the state
// file.
func (e *Engine) runCanary(ctx context.Context, s *Site, inst runtime.Instance, spec *manifest.Spec) (promote, cancelled bool, reason string) {
	cfg := *spec.Canary
	now := time.Now()
	c := &canaryRun{inst: inst, cfg: cfg, started: now, until: now.Add(cfg.Window()), revision: spec.Revision, decide: make(chan bool, 1)}
	e.mu.Lock()
	s.canary = c
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		s.canary = nil
		e.mu.Unlock()
	}()
	e.emit("canary.started", s.Domain, "", fmt.Sprintf("the new generation gets %d%% of the visitors for %s", cfg.Share(), cfg.Window()), map[string]any{"revision": spec.Revision, "percent": cfg.Share()})
	t := time.NewTicker(canaryEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return false, true, "the supervisor is stopping"
		case err := <-inst.Done():
			return false, false, fmt.Sprintf("the new generation exited: %v", err)
		case p := <-c.decide:
			if p {
				return true, false, "promoted by the operator"
			}
			return false, false, "rolled back by the operator"
		case <-t.C:
			if bad, why := c.failing(); bad {
				return false, false, why
			}
			if time.Now().After(c.until) {
				return true, false, c.verdict()
			}
		}
	}
}

func rate(errs, reqs int64) float64 {
	if reqs == 0 {
		return 0
	}
	return float64(errs) / float64(reqs)
}

// failing is the canary answering worse than allowed, and worse than the
// generation beside it: when both fail alike the fault is not the new
// code's (a database down), and keeping the old one would fix nothing.
func (c *canaryRun) failing() (bool, string) {
	reqs := c.reqs.Load()
	if reqs < c.cfg.Enough() {
		return false, ""
	}
	r := rate(c.errs.Load(), reqs)
	stable := rate(c.stableErrs.Load(), c.stableReqs.Load())
	if r > c.cfg.MaxErrors() && r > stable {
		return true, fmt.Sprintf("%.1f%% errors on %d requests (the serving generation: %.1f%%)", 100*r, reqs, 100*stable)
	}
	return false, ""
}

func (c *canaryRun) verdict() string {
	reqs := c.reqs.Load()
	if reqs < c.cfg.Enough() {
		return fmt.Sprintf("only %d requests in %s, too few to judge; it takes over", reqs, c.cfg.Window())
	}
	return fmt.Sprintf("%.1f%% errors on %d requests", 100*rate(c.errs.Load(), reqs), reqs)
}
