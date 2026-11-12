package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/manifest"
)

// PublicHost is what the engine asks of the house about a service's
// public ports (services.<name>.ports, network: host): the things a
// manifest cannot say of itself. Without one, such a service does not
// start: a port open to the internet with nobody having been asked is
// the one way this could fail that is worse than not working.
type PublicHost interface {
	// PublicIP is the address this node is reached at from outside, ""
	// while it is not known. Asked on every pass, under the engine's
	// lock: it must answer from memory.
	PublicIP() string
	// Pending are the lines of m's PublicGrant the owner has not
	// approved (internal/pubports).
	Pending(domain string, m *manifest.Manifest) []string
	// Reserved are the ports the house itself holds in this
	// installation, each with what it is.
	Reserved() []manifest.HeldPort
	// Open is told every public port that should be open on the machine
	// whenever the set changes, and once at start. An error is tried
	// again later.
	Open(ctx context.Context, ports []manifest.PublicPort) error
}

// SetPublic installs the PublicHost after the engine was built, as
// SetSiteEnv does for the same reason.
func (e *Engine) SetPublic(h PublicHost) { e.public.Store(&h) }

func (e *Engine) publicHost() PublicHost {
	if p := e.public.Load(); p != nil {
		return *p
	}
	return nil
}

// stoppedApproval is a service's Stopped while its public ports wait for
// the owner. Stopped and not an error: the rest of the site is up, and a
// site counted as down for it would have the automatic repair called to
// a thing only the owner can settle.
const stoppedApproval = "approval"

// publicIP is the node's public address, or "".
func (e *Engine) publicIP() string {
	if h := e.publicHost(); h != nil {
		return h.PublicIP()
	}
	return ""
}

// publicPending is what of a service's public ports waits for the owner:
// the lines, joined, or "". A house with no PublicHost approves nothing.
func (e *Engine) publicPending(domain string, m *manifest.Manifest, name string) string {
	sv := m.Services[name]
	if !sv.Public() {
		return ""
	}
	h := e.publicHost()
	if h == nil {
		return "public ports are not available in this house"
	}
	var mine []string
	for _, line := range h.Pending(domain, m) {
		if strings.HasPrefix(line, name+" ") {
			mine = append(mine, strings.TrimPrefix(line, name+" "))
		}
	}
	return strings.Join(mine, ", ")
}

// publicConflictLocked is why a service's public ports cannot be bound
// now: one the house holds, or one a service of another site is running
// on. Asked before a start, not of what already runs: the first to hold
// a port keeps it, and the one that came second says so.
func (e *Engine) publicConflictLocked(s *Site, m *manifest.Manifest, name string) string {
	sv := m.Services[name]
	ports := sv.PublicPorts()
	if len(ports) == 0 {
		return ""
	}
	if h := e.publicHost(); h != nil {
		reserved := h.Reserved()
		for _, p := range ports {
			if held, ok := manifest.Holds(reserved, p); ok {
				return fmt.Sprintf("port %s is taken: %s is %s", p, held, held.What)
			}
		}
	}
	for _, domain := range sortedKeys(e.sites) {
		other := e.sites[domain]
		if other == s || other.Manifest == nil {
			continue
		}
		for oname, osvc := range other.Services {
			if osvc.inst == nil && !osvc.starting {
				continue
			}
			for _, op := range other.Manifest.Services[oname].PublicPorts() {
				for _, p := range ports {
					if p.Overlaps(op) {
						return fmt.Sprintf("port %s is taken: %s has %s on this node (its service %s)", p, domain, op, oname)
					}
				}
			}
		}
	}
	return ""
}

// publicWanted are the public ports that should be open on the machine
// now: those of every service that may run. Called under e.mu.
func (e *Engine) publicWantedLocked() []manifest.PublicPort {
	var out []manifest.PublicPort
	seen := map[string]bool{}
	for _, domain := range sortedKeys(e.sites) {
		s := e.sites[domain]
		m := s.Manifest
		if m == nil || len(m.Services) == 0 {
			continue
		}
		for _, name := range m.ServiceNames() {
			sv := m.Services[name]
			if !sv.Public() || serviceStopped(m, sv) != "" || e.publicPending(domain, m, name) != "" {
				continue
			}
			for _, p := range sv.PublicPorts() {
				if !seen[p.String()] {
					seen[p.String()] = true
					out = append(out, p)
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Proto != out[j].Proto {
			return out[i].Proto < out[j].Proto
		}
		return out[i].From < out[j].From
	})
	return out
}

// publicRetry is how long a failed Open waits before it is asked again:
// a helper that is not there will not be there two seconds later either.
const publicRetry = time.Minute

// syncPublic tells the house which ports to open when the set changed,
// and once at start even if it is empty of changes: the rules do not
// survive a reboot, the manifests do. A house that never had a public
// port is never asked anything.
func (e *Engine) syncPublic(ctx context.Context) {
	h := e.publicHost()
	if h == nil {
		return
	}
	e.mu.Lock()
	want := e.publicWantedLocked()
	key := fmt.Sprint(want)
	due := (!e.publicSynced || key != e.publicKey) && !time.Now().Before(e.publicNext)
	e.mu.Unlock()
	if !due {
		return
	}
	octx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err := h.Open(octx, want)
	cancel()
	e.mu.Lock()
	defer e.mu.Unlock()
	if err != nil {
		e.publicNext = time.Now().Add(publicRetry)
		if e.publicErr != err.Error() {
			e.publicErr = err.Error()
			e.emit("ports.failed", "", "warn", "the sites' public ports could not be opened in this machine's firewall: "+err.Error(), nil)
		}
		return
	}
	e.publicSynced, e.publicKey, e.publicErr = true, key, ""
	if len(want) > 0 {
		list := make([]string, len(want))
		for i, p := range want {
			list[i] = p.String()
		}
		e.emit("ports.opened", "", "", "public ports open on this node: "+strings.Join(list, ", "), map[string]any{"ports": list})
	}
}
