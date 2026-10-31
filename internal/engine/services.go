package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/runtime"
)

// A site with services is several units, each with what a site's single
// instance has: a revision, a start that waits for its health, a handover
// (or a restart in place for one without names), a backoff of its own and
// a state file. One that fails to build or to get healthy keeps its
// previous generation and backs off; the others never hear of it. That is
// the whole point: before, a project with a mock, an app and a CMS was one
// image, and a typo in the part nobody touched kept every part from
// deploying.
type service struct {
	name string
	inst runtime.Instance
	spec *manifest.Spec
	// port is the one a service that restarts in place keeps across its
	// generations, so what reaches it ($SERVICE_<name>_PORT in a process)
	// never has to restart because it moved. Zero for one that hands
	// over: every generation gets a fresh port.
	port int
	// starting is set while a start runs outside the lock.
	starting  bool
	failures  int
	nextStart time.Time
	lastErr   string
	// waiting is what a first start waits for (depends_on).
	waiting string
	health  healthState
}

// ServiceSnapshot is a service's state for readers.
type ServiceSnapshot struct {
	Name      string   `json:"name"`
	Domains   []string `json:"domains,omitempty"`
	Runtime   string   `json:"runtime"`
	Port      int      `json:"port,omitempty"`
	Published bool     `json:"published"`
	Revision  string   `json:"revision,omitempty"`
	Alive     bool     `json:"alive"`
	Starting  bool     `json:"starting,omitempty"`
	Stopped   string   `json:"stopped,omitempty"` // disabled or hold, when it is
	Health    Health   `json:"health,omitempty"`
	Error     string   `json:"error,omitempty"`
	Failures  int      `json:"failures,omitempty"`
	Waiting   string   `json:"waiting,omitempty"`
	Optional  bool     `json:"optional,omitempty"`
	Checks    []Check  `json:"checks,omitempty"`
}

func serviceKey(domain, name string) string { return domain + "/" + name }

// serviceSubject is a service's events' subject: the site, then the
// service, as the panel and the delegates' filter read subjects.
func serviceSubject(domain, name string) string { return domain + "#" + name }

func (e *Engine) emitService(typ, domain, name, level, msg string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	data["service"] = name
	e.opts.Bus.Emit(events.Event{Type: typ, Subject: serviceSubject(domain, name), Actor: "engine", Level: level, Message: msg, Data: data})
}

// serviceStopped says why a service must not run, or "".
func serviceStopped(m *manifest.Manifest, sv *manifest.Service) string {
	switch {
	case m.Disabled || sv.Disabled:
		return "disabled"
	case (m.Hold != nil && m.Hold.Enabled) || (sv.Hold != nil && sv.Hold.Enabled):
		return "hold"
	}
	return ""
}

// reconcileServices makes what runs equal to the services the manifest
// declares. Called outside the lock; takes it.
func (e *Engine) reconcileServices(ctx context.Context, s *Site, m *manifest.Manifest) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if s.Services == nil {
		s.Services = map[string]*service{}
	}
	for name, svc := range s.Services {
		if _, still := m.Services[name]; still {
			continue
		}
		delete(s.Services, name)
		e.retireServiceLocked(ctx, s.Domain, svc, 8*time.Second, true)
		e.emitService("service.removed", s.Domain, name, "", name+": no longer in the manifest; stopped", nil)
	}
	for _, name := range m.ServiceNames() {
		sv := m.Services[name]
		svc, ok := s.Services[name]
		if !ok {
			svc = &service{name: name}
			s.Services[name] = svc
		}
		if svc.starting {
			continue
		}
		if why := serviceStopped(m, sv); why != "" {
			if svc.inst != nil {
				e.emitService("service.stopping", s.Domain, name, "", name+": "+why, nil)
				e.retireServiceLocked(ctx, s.Domain, svc, drainOf(svc.spec), false)
			}
			svc.waiting = ""
			continue
		}
		if svc.inst != nil {
			select {
			case err := <-svc.inst.Done():
				svc.failures++
				wait := backoff(svc.failures, restartMax(svc.spec, sv))
				svc.nextStart = time.Now().Add(wait)
				svc.lastErr = fmt.Sprintf("%v; restart in %s", err, wait)
				e.emitService("service.exited", s.Domain, name, "error", name+": "+svc.lastErr, map[string]any{"failures": svc.failures})
				e.retireServiceLocked(ctx, s.Domain, svc, 2*time.Second, false)
				continue
			default:
			}
		}
		spec, err := manifest.ServiceSpec(s.Domain, s.Dir, name, m, e.serviceAddrLocked(s, m))
		if err != nil {
			svc.lastErr = err.Error()
			continue
		}
		e.addSiteEnv(s.Domain, m, spec)
		if svc.inst != nil && svc.inst.Revision() == spec.Revision {
			svc.spec = spec
			continue
		}
		if time.Now().Before(svc.nextStart) {
			continue
		}
		if svc.inst == nil {
			if wait := e.dependsWaitLocked(s, sv); wait != "" {
				if svc.waiting != wait {
					svc.waiting = wait
					e.emitService("service.waiting", s.Domain, name, "", name+": waiting for "+wait, nil)
				}
				continue
			}
		}
		svc.waiting = ""
		e.startServiceLocked(ctx, s, svc, spec, sv.HandsOver(), "engine")
	}
}

func drainOf(spec *manifest.Spec) time.Duration {
	if spec == nil {
		return 8 * time.Second
	}
	return spec.Drain.Or(8 * time.Second)
}

func restartMax(spec *manifest.Spec, sv *manifest.Service) time.Duration {
	if spec != nil {
		return spec.Restart.MaxBackoff.Or(60 * time.Second)
	}
	if sv != nil && sv.Run != nil && sv.Run.Restart != nil {
		return sv.Run.Restart.MaxBackoff.Or(60 * time.Second)
	}
	return 60 * time.Second
}

// dependsWaitLocked is what of sv's depends_on is not healthy yet, or "".
// It waits for as long as it takes: a CMS started before its database
// only fails its health and backs off, and an order nobody follows is no
// order. What it waits for is said, on the service and in an event.
func (e *Engine) dependsWaitLocked(s *Site, sv *manifest.Service) string {
	var missing []string
	for _, dep := range sv.DependsOn {
		d := s.Services[dep]
		if d == nil || d.inst == nil || d.starting || d.health.status == HealthDown {
			missing = append(missing, dep)
		}
	}
	sort.Strings(missing)
	return strings.Join(missing, ", ")
}

// serviceAddrLocked answers ServiceSpec's question: where target is
// reached from a container (its name, its port inside) or from a process
// (loopback, the port it keeps on the machine, claimed now if it has
// none yet so its consumers can be born with it).
func (e *Engine) serviceAddrLocked(s *Site, m *manifest.Manifest) manifest.ServiceAddr {
	return func(target string, fromContainer bool) (string, int, bool) {
		sv := m.Services[target]
		if sv == nil || sv.Run == nil {
			return "", 0, false
		}
		if fromContainer {
			p := manifest.DefaultServicePort
			if sv.Run.Docker != nil && sv.Run.Docker.Port != 0 {
				p = sv.Run.Docker.Port
			}
			return target, p, true
		}
		t := s.Services[target]
		if t == nil {
			t = &service{name: target}
			s.Services[target] = t
		}
		if sv.Run.Port != 0 {
			return "127.0.0.1", sv.Run.Port, true
		}
		if t.port == 0 {
			p, err := e.opts.Ports.Claim(serviceKey(s.Domain, target))
			if err != nil {
				return "", 0, false
			}
			t.port = p
		}
		return "127.0.0.1", t.port, true
	}
}

// startServiceLocked starts a new generation outside the lock.
func (e *Engine) startServiceLocked(ctx context.Context, s *Site, svc *service, spec *manifest.Spec, handover bool, actor string) {
	if e.closing {
		return
	}
	svc.starting = true
	e.wg.Add(1)
	go e.startService(ctx, s, svc, spec, handover, actor)
}

// startService is a service's handover, or its restart in place when it
// does not hand over: the old generation stops first and the new one
// takes the same port.
func (e *Engine) startService(ctx context.Context, s *Site, svc *service, spec *manifest.Spec, handover bool, actor string) {
	defer e.wg.Done()
	domain, name := s.Domain, svc.name
	stopCtx := context.WithoutCancel(ctx)
	failed := func(err error) {
		e.mu.Lock()
		svc.starting = false
		svc.failures++
		wait := backoff(svc.failures, spec.Restart.MaxBackoff.Or(60*time.Second))
		svc.nextStart = time.Now().Add(wait)
		svc.lastErr = err.Error()
		kept := svc.inst != nil
		e.mu.Unlock()
		msg := fmt.Sprintf("%s: %v; retry in %s", name, err, wait)
		if kept {
			msg += "; the previous generation keeps serving"
		}
		e.emitService("instance.failed", domain, name, "error", msg, map[string]any{"failures": svc.failures})
	}
	rt, ok := e.opts.Runtimes[spec.Runtime]
	if !ok {
		failed(fmt.Errorf("runtime %q is not available", spec.Runtime))
		return
	}
	key := serviceKey(domain, name)
	var port int
	fresh := false
	switch {
	case spec.Port != 0:
		port = spec.Port
		e.opts.Ports.Reserve(port, key)
	case !handover:
		e.mu.Lock()
		if svc.port == 0 {
			p, err := e.opts.Ports.Claim(key)
			if err != nil {
				e.mu.Unlock()
				failed(err)
				return
			}
			svc.port = p
		}
		port = svc.port
		e.mu.Unlock()
	default:
		p, err := e.opts.Ports.Claim(key)
		if err != nil {
			failed(err)
			return
		}
		port, fresh = p, true
	}
	if !handover || spec.Port != 0 {
		e.mu.Lock()
		old := svc.inst
		svc.inst = nil
		e.mu.Unlock()
		if old != nil {
			e.emitService("instance.stopping", domain, name, "", name+": restarting in place (it does not hand over)", nil)
			_ = old.Stop(stopCtx, drainOf(svc.spec))
			e.clearState(key)
		}
	}
	logFile := filepath.Join(e.opts.DataDir, "logs", domain, name+".log")
	home := filepath.Join(e.opts.DataDir, "homes", domain)
	e.emitService("instance.starting", domain, name, "", name, map[string]any{"port": port, "revision": spec.Revision, "actor": actor})
	inst, err := rt.Start(ctx, spec, port, logFile, home)
	if err != nil {
		if fresh {
			e.opts.Ports.Release(port)
		}
		failed(err)
		return
	}
	timeout := spec.Health.Timeout.Or(60 * time.Second)
	if err := waitServiceReady(ctx, inst, spec, timeout); err != nil {
		_ = inst.Stop(stopCtx, 2*time.Second)
		if fresh {
			e.opts.Ports.Release(port)
		}
		if ctx.Err() != nil {
			e.mu.Lock()
			svc.starting = false
			e.mu.Unlock()
			return
		}
		failed(fmt.Errorf("%w (last lines of the log: %s)%s", err, tail(logFile, 5), archHint(logFile)))
		return
	}
	e.mu.Lock()
	if e.sites[domain] != s || s.Services[name] != svc || s.Manifest == nil || s.Manifest.Services[name] == nil {
		svc.starting = false
		e.mu.Unlock()
		_ = inst.Stop(stopCtx, 5*time.Second)
		if fresh {
			e.opts.Ports.Release(port)
		}
		e.emitService("instance.discarded", domain, name, "", name+": the site changed while it started; stopped it", nil)
		return
	}
	old := svc.inst
	svc.inst, svc.spec = inst, spec
	svc.starting, svc.failures, svc.lastErr = false, 0, ""
	// It just passed its check: what waits for it may start now.
	svc.health.status, svc.health.next = HealthOK, time.Now().Add(spec.Health.Interval.Or(defaultHealthInterval))
	e.saveState(inst.State())
	e.mu.Unlock()
	if old == nil {
		e.emitService("instance.started", domain, name, "", name, map[string]any{"port": port, "revision": spec.Revision})
		return
	}
	e.emitService("handover.switched", domain, name, "", name, map[string]any{"from": old.Port(), "to": port, "revision": spec.Revision})
	select {
	case <-time.After(spec.Drain.Or(8 * time.Second)):
	case <-ctx.Done():
	}
	_ = old.Stop(stopCtx, 5*time.Second)
	e.opts.Ports.Release(old.Port())
	e.emitService("handover.done", domain, name, "", name, nil)
}

// waitServiceReady is WaitReady for a service: one on the site's network
// only, or with a health command, is asked from inside its container.
func waitServiceReady(ctx context.Context, inst runtime.Instance, spec *manifest.Spec, timeout time.Duration) error {
	if !spec.Unpublished && len(spec.Health.Command) == 0 {
		return runtime.WaitReady(ctx, inst, spec.Health, timeout)
	}
	ck, ok := inst.(runtime.Checker)
	deadline := time.Now().Add(timeout)
	settled := time.Now().Add(2 * time.Second)
	var last error
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-inst.Done():
			if err == nil {
				err = errors.New("exited")
			}
			return fmt.Errorf("instance %s before it was ready", err)
		case <-time.After(300 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			if last != nil {
				return fmt.Errorf("not ready after %s: %v", timeout, last)
			}
			return fmt.Errorf("not ready after %s", timeout)
		}
		if len(spec.Health.Command) == 0 || !ok {
			// No way to ask: a container that stays up is all there is
			// to know.
			if time.Now().After(settled) {
				return nil
			}
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		last = ck.Check(cctx, spec.Health.Command)
		cancel()
		if last == nil {
			return nil
		}
	}
}

// archHint names the likely cause when a log says a binary was built for
// another machine.
func archHint(logFile string) string {
	last := strings.ToLower(tail(logFile, 20))
	if strings.Contains(last, "exec format error") {
		return "; \"exec format error\" is a binary built for another architecture: use a multi-arch base image and copy binaries from an image (COPY --from=<image>)"
	}
	// Node's arm64 builds need libatomic and a minimal Ubuntu lacks it:
	// the log said only "libatomic.so.1: cannot open shared object file",
	// and nothing about what installs it.
	if strings.Contains(last, "error while loading shared libraries") {
		if strings.Contains(last, "libatomic") {
			return "; this machine lacks libatomic, which Node needs here: System → the house's requirements installs it"
		}
		return "; a shared library the program needs is not installed on this machine"
	}
	return ""
}

// retireServiceLocked stops what svc runs, outside the lock; gone says
// the service left the manifest, and its kept port goes back too.
func (e *Engine) retireServiceLocked(ctx context.Context, domain string, svc *service, grace time.Duration, gone bool) {
	inst := svc.inst
	svc.inst = nil
	svc.health = healthState{}
	key := serviceKey(domain, svc.name)
	keep := svc.port
	if gone {
		svc.port = 0
	}
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		if inst != nil {
			_ = inst.Stop(context.WithoutCancel(ctx), grace)
			if inst.Port() != keep {
				e.opts.Ports.Release(inst.Port())
			}
		}
		if gone && keep != 0 {
			e.opts.Ports.Release(keep)
		}
		e.clearState(key)
	}()
}

// stopServicesLocked stops every service of a site that no longer has
// them (it went back to run, or its folder is gone).
func (e *Engine) stopServicesLocked(ctx context.Context, s *Site) {
	for name, svc := range s.Services {
		if !svc.starting {
			e.retireServiceLocked(ctx, s.Domain, svc, 8*time.Second, true)
		}
		delete(s.Services, name)
	}
}

// RestartService forces a new generation of one service, even if its
// revision has not changed: a rebuild, or a .env the service reads
// itself.
func (e *Engine) RestartService(ctx context.Context, domain, name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.sites[domain]
	if !ok || s.Manifest == nil || len(s.Manifest.Services) == 0 {
		return fmt.Errorf("%s: not a site with services", domain)
	}
	sv := s.Manifest.Services[name]
	svc := s.Services[name]
	if sv == nil || svc == nil {
		return fmt.Errorf("%s: no service %q", domain, name)
	}
	if svc.starting {
		return fmt.Errorf("%s: %s is already starting", domain, name)
	}
	if why := serviceStopped(s.Manifest, sv); why != "" {
		return fmt.Errorf("%s: %s is stopped (%s)", domain, name, why)
	}
	if e.closing {
		return fmt.Errorf("%s: the supervisor is shutting down", domain)
	}
	spec := svc.spec
	if spec == nil {
		var err error
		spec, err = manifest.ServiceSpec(domain, s.Dir, name, s.Manifest, e.serviceAddrLocked(s, s.Manifest))
		if err != nil {
			return err
		}
		e.addSiteEnv(domain, s.Manifest, spec)
	}
	run := e.runCtx
	if run == nil {
		run = context.Background()
	}
	svc.failures, svc.nextStart = 0, time.Time{}
	e.startServiceLocked(run, s, svc, spec, sv.HandsOver(), "operator")
	return nil
}

// restartServicesLocked is Restart for a site with services: every one
// that runs gets a new generation.
func (e *Engine) restartServicesLocked(s *Site) error {
	run := e.runCtx
	if run == nil {
		run = context.Background()
	}
	n := 0
	for _, name := range s.Manifest.ServiceNames() {
		svc := s.Services[name]
		if svc == nil || svc.starting || svc.spec == nil || svc.inst == nil {
			continue
		}
		e.startServiceLocked(run, s, svc, svc.spec, s.Manifest.Services[name].HandsOver(), "operator")
		n++
	}
	if n == 0 {
		return fmt.Errorf("%s: no service is running to restart", s.Domain)
	}
	return nil
}

// adoptService attaches a service a previous supervisor left running.
func (e *Engine) adoptService(st runtime.State, inst runtime.Instance, handover bool) {
	domain, name, _ := strings.Cut(st.Domain, "/")
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.sites[domain]
	if !ok {
		s = &Site{Domain: domain, Dir: filepath.Join(e.opts.SitesDir, domain), Workers: map[string]*worker{}}
		e.sites[domain] = s
	}
	if s.Services == nil {
		s.Services = map[string]*service{}
	}
	svc := &service{name: name, inst: inst}
	if !handover {
		svc.port = st.Port
	}
	svc.health.status = HealthOK
	s.Services[name] = svc
	s.Mode = manifest.ModeServices
}

// serviceSnapshotsLocked are the services of s for readers.
func (e *Engine) serviceSnapshotsLocked(s *Site) map[string]ServiceSnapshot {
	if s.Manifest == nil || len(s.Manifest.Services) == 0 {
		return nil
	}
	out := map[string]ServiceSnapshot{}
	for _, name := range s.Manifest.ServiceNames() {
		sv := s.Manifest.Services[name]
		sn := ServiceSnapshot{Name: name, Domains: sv.Domains, Runtime: "process", Published: s.Manifest.Published(name), Optional: sv.Optional, Stopped: serviceStopped(s.Manifest, sv)}
		if sv.Run != nil && sv.Run.Runtime != "" {
			sn.Runtime = sv.Run.Runtime
		}
		if svc := s.Services[name]; svc != nil {
			sn.Starting, sn.Error, sn.Failures, sn.Waiting = svc.starting, svc.lastErr, svc.failures, svc.waiting
			sn.Health, sn.Checks = svc.health.status, append([]Check(nil), svc.health.checks...)
			if svc.inst != nil {
				sn.Alive, sn.Port, sn.Revision = true, svc.inst.Port(), svc.inst.Revision()
			} else if svc.port != 0 {
				sn.Port = svc.port
			}
		}
		out[name] = sn
	}
	return out
}

// serviceView turns a site's snapshot into the one a request for host
// and path sees: the service that answers it, its port, its state.
// The legacy instance of a site that just moved from run to services
// keeps answering a name whose service is not up yet, so the move costs
// no downtime.
func (e *Engine) serviceView(s *Site, sn Snapshot, host, urlPath string) (Snapshot, bool) {
	name, ok := s.Manifest.ServiceFor(host, urlPath)
	if !ok {
		return Snapshot{}, false
	}
	sv := s.Manifest.Services[name]
	svc := s.Services[name]
	sn.Service = name
	sn.Manifest = s.Manifest.ForService(name)
	sn.Mode = manifest.ModeRun
	sn.Port, sn.Alive, sn.Revision, sn.Handing, sn.Error, sn.Failures, sn.Health = 0, false, "", false, "", 0, HealthUnknown
	sn.Checks = nil
	switch serviceStopped(s.Manifest, sv) {
	case "disabled":
		sn.Mode = manifest.ModeDisabled
		return sn, true
	case "hold":
		sn.Mode = manifest.ModeHold
		return sn, true
	}
	if svc != nil {
		sn.Handing, sn.Error, sn.Failures = svc.starting, svc.lastErr, svc.failures
		sn.Health, sn.Checks = svc.health.status, append([]Check(nil), svc.health.checks...)
		if svc.inst != nil && (svc.spec == nil || !svc.spec.Unpublished) {
			sn.Port, sn.Alive, sn.Revision = svc.inst.Port(), true, svc.inst.Revision()
			return sn, true
		}
	}
	if s.Instance != nil {
		sn.Port, sn.Alive, sn.Revision = s.Instance.Port(), true, s.Instance.Revision()
	} else if svc != nil && svc.waiting != "" && sn.Error == "" {
		sn.Handing = true // waiting for what it depends on is starting
	}
	return sn, true
}

// siteServicesSummaryLocked fills a site's snapshot from its services:
// alive when every service that is not optional is, its health the
// worst of theirs, its error the first.
func siteServicesSummaryLocked(sn *Snapshot) {
	if len(sn.Services) == 0 {
		return
	}
	sn.Alive, sn.Health = true, HealthOK
	for _, name := range sortedKeys(sn.Services) {
		v := sn.Services[name]
		if v.Stopped != "" {
			continue
		}
		if v.Error != "" && sn.Error == "" {
			sn.Error = name + ": " + v.Error
		}
		if v.Optional {
			continue
		}
		if !v.Alive {
			sn.Alive = false
		}
		if v.Starting {
			sn.Handing = true
		}
		switch {
		case v.Health == HealthDown:
			sn.Health = HealthDown
		case v.Health == HealthUnknown && sn.Health == HealthOK:
			sn.Health = HealthUnknown
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// servicesHealthDue starts the checks that are due for s's services.
// Called under e.mu.
func (e *Engine) servicesHealthDue(ctx context.Context, s *Site) {
	for _, svc := range s.Services {
		h := &svc.health
		if svc.inst == nil || svc.starting || svc.spec == nil || h.checking || time.Now().Before(h.next) {
			continue
		}
		inst, spec := svc.inst, svc.spec
		interval := spec.Health.Interval.Or(defaultHealthInterval)
		h.checking = true
		svc := svc
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			var c Check
			if spec.Unpublished || len(spec.Health.Command) > 0 {
				c = probeInside(ctx, inst, spec)
			} else {
				c = probeHTTP(ctx, inst.Port(), spec.Health)
			}
			e.mu.Lock()
			defer e.mu.Unlock()
			h.checking = false
			h.checks = append(h.checks, c)
			if len(h.checks) > maxChecks {
				h.checks = h.checks[len(h.checks)-maxChecks:]
			}
			status := HealthDown
			if c.OK {
				h.hits++
				status = HealthOK
			} else {
				h.hits = 0
			}
			if h.status != status {
				h.status = status
				if status == HealthOK {
					e.emitService("health.ok", s.Domain, svc.name, "", svc.name+": answering", nil)
				} else {
					e.emitService("health.down", s.Domain, svc.name, "error", svc.name+": not answering: "+c.Error, nil)
				}
			}
			h.next = time.Now().Add(h.delay(interval))
		}()
	}
}

func probeInside(ctx context.Context, inst runtime.Instance, spec *manifest.Spec) Check {
	start := time.Now()
	c := Check{At: start}
	ck, ok := inst.(runtime.Checker)
	switch {
	case len(spec.Health.Command) == 0 || !ok:
		c.OK = inst.Alive()
		if !c.OK {
			c.Error = "the container is not running"
		}
	default:
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := ck.Check(cctx, spec.Health.Command)
		cancel()
		c.OK = err == nil
		if err != nil {
			c.Error = err.Error()
		}
	}
	c.Millis = time.Since(start).Milliseconds()
	return c
}

// ServiceSpec is what one of a site's services runs now: the running
// generation's spec, or, for one that is not running, the spec it would
// start with. A one-off command (`middleware exec --service`) runs in
// it, so it sees the image, env and network the service has.
func (e *Engine) ServiceSpec(domain, name string) (*manifest.Spec, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.sites[domain]
	if !ok || s.Manifest == nil {
		return nil, fmt.Errorf("%s: no such site", domain)
	}
	if _, ok := s.Manifest.Services[name]; !ok {
		return nil, fmt.Errorf("%s has no service %q", domain, name)
	}
	if svc := s.Services[name]; svc != nil && svc.spec != nil && svc.inst != nil {
		cp := *svc.spec
		return &cp, nil
	}
	if s.Services == nil {
		s.Services = map[string]*service{}
	}
	return manifest.ServiceSpec(domain, s.Dir, name, s.Manifest, e.serviceAddrLocked(s, s.Manifest))
}
