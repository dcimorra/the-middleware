// Package sites is the write side of sites/: creating, editing, stopping,
// archiving and inspecting a site folder on behalf of the API. The engine
// only reads manifests; everything the panel and the CLI change goes
// through here so that every change is validated, written atomically and
// announced on the bus.
package sites

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/access"
	"github.com/dcimorra/the-middleware/internal/archive"
	"github.com/dcimorra/the-middleware/internal/deploy"
	"github.com/dcimorra/the-middleware/internal/engine"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/manifest"
)

// Errors callers branch on.
var (
	ErrNotFound    = errors.New("no such site")
	ErrExists      = errors.New("a folder with that name already exists")
	ErrBadDomain   = errors.New("not a valid domain name (lower-case ASCII, at least one dot)")
	ErrUnmanaged   = errors.New("the folder has no site.json")
	ErrOutside     = errors.New("path leaves the site folder")
	ErrBadTemplate = errors.New("unknown template: want static, node, go or docker")
	ErrInvalid     = errors.New("invalid manifest")
	ErrForbidden   = errors.New("not for this caller")
)

// Manager operates on the sites of one installation.
type Manager struct {
	Root   string
	Engine *engine.Engine // may be nil: then nothing is alive and restarts fail
	Bus    *events.Bus
	// Ctx bounds background work; nil means Background.
	Ctx context.Context
	// Deploys is the one deployment runner of the installation: the
	// panel's Deploy button and GitHub's pushes share its lock per site.
	Deploys *deploy.Runner
}

// ErrDeploying says a deployment is already running for the site.
var ErrDeploying = deploy.ErrDeploying

// Deploy builds what is in the site folder (deploy.github.build) and
// writes the pointer, or restarts the site when there is none. It returns
// the build id at once; progress is on the bus as deploy.* and the output
// in data/logs/<domain>/deploy.log.
func (m *Manager) Deploy(domain string) (string, error) {
	dir, err := m.managedDir(domain)
	if err != nil {
		return "", err
	}
	mf, err := manifest.Load(dir)
	if err != nil {
		return "", fmt.Errorf("manifest: %w", err)
	}
	g := mf.Deploy != nil && mf.Deploy.GitHub != nil
	if !g && mf.Run == nil {
		return "", errors.New("nothing to deploy: no deploy.github.build, no pointer and no run section")
	}
	return m.Deploys.Start(domain, dir, mf, "operator", deploy.InPlace)
}

// New builds a manager over root. bus may be nil for tests.
func New(root string, eng *engine.Engine, bus *events.Bus) *Manager {
	if bus == nil {
		bus = events.New("")
	}
	// Absolute, because the root reaches a site's environment as
	// MIDDLEWARE_SITE_DIR and a build step may chdir before using it.
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	m := &Manager{Root: root, Engine: eng, Bus: bus}
	var restart func(context.Context, string) error
	if eng != nil {
		restart = eng.Restart
	}
	m.Deploys = deploy.New(deploy.Options{DataDir: filepath.Join(root, "data"), Bus: bus, Restart: restart})
	return m
}

// Site is the API's view of one folder, as docs/api.md describes it.
type Site struct {
	Domain   string                  `json:"domain"`
	Mode     manifest.Mode           `json:"mode"`
	Alive    bool                    `json:"alive"`
	Port     int                     `json:"port"`
	Revision string                  `json:"revision"`
	Handing  bool                    `json:"handing"`
	Error    string                  `json:"error"`
	Health   engine.Health           `json:"health"`
	Checks   []engine.Check          `json:"checks,omitempty"`
	Missing  []string                `json:"missing,omitempty"`
	Managed  bool                    `json:"managed"`
	Manifest *manifest.Manifest      `json:"manifest,omitempty"`
	Workers  map[string]WorkerStatus `json:"workers,omitempty"`
	// Services are a site's services with their state, when it has them.
	Services map[string]engine.ServiceSnapshot `json:"services,omitempty"`
}

// WorkerStatus is a worker's state.
type WorkerStatus struct {
	Port  int    `json:"port"`
	Alive bool   `json:"alive"`
	Error string `json:"error"`
}

// SitesDir is sites/ under the root.
func (m *Manager) SitesDir() string { return filepath.Join(m.Root, "sites") }

// Dir is a site's folder.
func (m *Manager) Dir(domain string) string { return filepath.Join(m.SitesDir(), domain) }

func (m *Manager) logsDir(domain string) string {
	return filepath.Join(m.Root, "data", "logs", domain)
}

func (m *Manager) emit(typ, domain, actor, msg string, data map[string]any) {
	if actor == "" {
		actor = "operator"
	}
	m.Bus.Emit(events.Event{Type: typ, Subject: domain, Actor: actor, Message: msg, Data: data})
}

func (m *Manager) lookup(domain string) (engine.Snapshot, bool) {
	if m.Engine == nil {
		return engine.Snapshot{}, false
	}
	// The site's own snapshot: Lookup is a request's view, which for a
	// site with services is one service's.
	return m.Engine.Site(domain)
}

// List returns every folder under sites/ whose name is a domain: what the
// engine runs, what it has not reconciled yet, and folders without a
// manifest, which are listed as unmanaged so a folder that is ignored
// never looks the same as one that is served.
func (m *Manager) List() ([]Site, error) {
	entries, err := os.ReadDir(m.SitesDir())
	if err != nil {
		if os.IsNotExist(err) {
			return []Site{}, nil
		}
		return nil, err
	}
	out := make([]Site, 0, len(entries))
	for _, ent := range entries {
		if !ent.IsDir() && ent.Type()&os.ModeSymlink == 0 {
			continue
		}
		if !manifest.ValidName(ent.Name()) {
			continue
		}
		out = append(out, m.describe(ent.Name()))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Domain < out[j].Domain })
	return out, nil
}

// Get returns one site.
func (m *Manager) Get(domain string) (Site, error) {
	if !manifest.ValidName(domain) {
		return Site{}, ErrNotFound
	}
	st, err := os.Stat(m.Dir(domain))
	if err != nil || !st.IsDir() {
		return Site{}, ErrNotFound
	}
	return m.describe(domain), nil
}

// describe merges the engine's snapshot with what is on disk. The disk is
// read even when the engine knows the site: a manifest written a second
// ago is what the operator expects to see back, not the one from the
// last tick.
func (m *Manager) describe(domain string) Site {
	s := Site{Domain: domain, Mode: manifest.ModeStatic}
	dir := m.Dir(domain)
	if !fsx.Exists(filepath.Join(dir, manifest.FileName)) {
		s.Managed = false
		s.Mode = ""
		return s
	}
	s.Managed = true
	mf, err := manifest.Load(dir)
	if err != nil {
		s.Error = err.Error()
	} else {
		s.Manifest = mf
		s.Mode = mf.ModeFor(domain)
	}
	sn, ok := m.lookup(domain)
	if !ok {
		return s
	}
	if s.Manifest == nil && sn.Manifest != nil {
		s.Manifest = sn.Manifest // the last valid one, still in force
		s.Mode = sn.Mode
	}
	s.Alive, s.Port, s.Revision, s.Handing = sn.Alive, sn.Port, sn.Revision, sn.Handing
	s.Health, s.Checks, s.Missing = sn.Health, sn.Checks, sn.Missing
	if s.Error == "" {
		s.Error = sn.Error
	}
	s.Services = sn.Services
	if len(sn.Workers) > 0 {
		s.Workers = map[string]WorkerStatus{}
		for name, w := range sn.Workers {
			s.Workers[name] = WorkerStatus{Port: w.Port, Alive: w.Alive, Error: w.Error}
		}
	}
	return s
}

// Create makes sites/<domain>/ with the given manifest, or from a template
// when mf is nil. The manifest is written last: the engine ticks at any
// moment, and a manifest pointing at files that are not there yet would
// start a crash loop before the folder is complete.
func (m *Manager) Create(domain string, mf *manifest.Manifest, template string) (Site, error) {
	if !manifest.ValidName(domain) {
		return Site{}, ErrBadDomain
	}
	dir := m.Dir(domain)
	if fsx.Exists(dir) {
		return Site{}, ErrExists
	}
	var files map[string]string
	if mf == nil {
		// "auto" is "what the repository says"; with no repository there
		// is nothing to detect, and the default is the answer. An older
		// panel sent it for a site from a template and got "unknown
		// template" for a choice it had made itself.
		if template == "" || template == "auto" {
			template = "static"
			if manifest.IsProject(domain) {
				template = "project"
			}
		}
		t, ok := templates[template]
		if !ok {
			return Site{}, ErrBadTemplate
		}
		mf, files = t(domain)
	}
	if mf.Version == 0 {
		mf.Version = manifest.Version
	}
	if err := mf.Validate(); err != nil {
		return Site{}, fmt.Errorf("manifest: %w", err)
	}
	if err := mf.CheckInside(dir); err != nil {
		return Site{}, fmt.Errorf("manifest: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Site{}, err
	}
	for name, content := range files {
		if err := fsx.WriteAtomic(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			_ = os.RemoveAll(dir)
			return Site{}, err
		}
	}
	if err := sealPassword(mf); err != nil {
		_ = os.RemoveAll(dir)
		return Site{}, err
	}
	if err := manifest.Save(dir, mf); err != nil {
		_ = os.RemoveAll(dir)
		return Site{}, err
	}
	m.emit("site.created", domain, "", "site created", map[string]any{"template": template, "mode": string(mf.ModeFor(domain))})
	return m.describe(domain), nil
}

// sealPassword hashes a plain access password before it reaches disk. The
// panel and the API send what the operator typed; access.CheckPassword
// only ever accepts a hash, so a clear value saved as is would never match
// and the site would look protected while refusing everyone.
func sealPassword(mf *manifest.Manifest) error {
	if mf == nil || mf.Access == nil {
		return nil
	}
	seal := func(p *string) error {
		if *p == "" || access.IsHash(*p) {
			return nil
		}
		h, err := access.HashPassword(*p)
		if err != nil {
			return err
		}
		*p = h
		return nil
	}
	if err := seal(&mf.Access.Password); err != nil {
		return err
	}
	for _, ra := range mf.Access.Routes {
		if ra == nil {
			continue
		}
		if err := seal(&ra.Password); err != nil {
			return err
		}
	}
	return nil
}

// Update replaces the manifest.
func (m *Manager) Update(domain string, mf *manifest.Manifest) (Site, error) {
	dir, err := m.managedDir(domain)
	if err != nil {
		return Site{}, err
	}
	if mf.Version == 0 {
		mf.Version = manifest.Version
	}
	if err := sealPassword(mf); err != nil {
		return Site{}, err
	}
	if err := manifest.Save(dir, mf); err != nil {
		return Site{}, fmt.Errorf("manifest: %w", err)
	}
	m.emit("manifest.updated", domain, "", "manifest replaced", nil)
	return m.describe(domain), nil
}

// Patch merges top-level keys into the manifest; a null value removes the
// key. The result goes through the same parser as a file edit, so an
// unknown key is rejected rather than silently dropped.
func (m *Manager) Patch(domain string, partial map[string]json.RawMessage) (Site, error) {
	dir, err := m.managedDir(domain)
	if err != nil {
		return Site{}, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, manifest.FileName))
	if err != nil {
		return Site{}, err
	}
	current := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &current); err != nil {
		return Site{}, fmt.Errorf("current manifest: %w", err)
	}
	keys := make([]string, 0, len(partial))
	for k, v := range partial {
		keys = append(keys, k)
		if string(v) == "null" {
			delete(current, k)
		} else {
			current[k] = v
		}
	}
	sort.Strings(keys)
	merged, err := json.Marshal(current)
	if err != nil {
		return Site{}, err
	}
	mf, err := manifest.Parse(merged)
	if err != nil {
		return Site{}, fmt.Errorf("manifest: %w", err)
	}
	if err := sealPassword(mf); err != nil {
		return Site{}, err
	}
	if err := manifest.Save(dir, mf); err != nil {
		return Site{}, err
	}
	m.emit("manifest.updated", domain, "", "manifest patched: "+strings.Join(keys, ", "), map[string]any{"keys": keys})
	return m.describe(domain), nil
}

// SetDisabled stops or starts a site by writing the decision to its
// manifest, where it survives a restart.
func (m *Manager) SetDisabled(domain string, disabled bool) (Site, error) {
	dir, err := m.managedDir(domain)
	if err != nil {
		return Site{}, err
	}
	mf, err := manifest.Load(dir)
	if err != nil {
		return Site{}, fmt.Errorf("manifest: %w", err)
	}
	mf.Disabled = disabled
	if err := manifest.Save(dir, mf); err != nil {
		return Site{}, err
	}
	if disabled {
		m.emit("site.stopped", domain, "", "stopped by the operator", nil)
	} else {
		m.emit("site.started", domain, "", "started by the operator", nil)
	}
	return m.describe(domain), nil
}

// SetHold puts a site behind the holding page, or takes it out.
func (m *Manager) SetHold(domain string, hold manifest.Hold) (Site, error) {
	dir, err := m.managedDir(domain)
	if err != nil {
		return Site{}, err
	}
	mf, err := manifest.Load(dir)
	if err != nil {
		return Site{}, fmt.Errorf("manifest: %w", err)
	}
	if hold.Enabled {
		mf.Hold = &hold
	} else {
		mf.Hold = nil
	}
	if err := manifest.Save(dir, mf); err != nil {
		return Site{}, err
	}
	m.emit("site.hold", domain, "", fmt.Sprintf("hold %v", hold.Enabled), map[string]any{"enabled": hold.Enabled, "title": hold.Title})
	return m.describe(domain), nil
}

// Restart forces a handover.
func (m *Manager) Restart(ctx context.Context, domain string) error {
	if _, err := m.managedDir(domain); err != nil {
		return err
	}
	if m.Engine == nil {
		return errors.New("no engine")
	}
	return m.Engine.Restart(ctx, domain)
}

// RestartService forces a new generation of one of a site's services.
func (m *Manager) RestartService(ctx context.Context, domain, name string) error {
	if _, err := m.managedDir(domain); err != nil {
		return err
	}
	if m.Engine == nil {
		return errors.New("no engine")
	}
	return m.Engine.RestartService(ctx, domain, name)
}

// RouteState is one name of a site and what answers it now.
type RouteState struct {
	Host    string `json:"host"`
	Prefix  string `json:"prefix,omitempty"`
	Service string `json:"service,omitempty"`
	Port    int    `json:"port,omitempty"`
	// State is up, starting, down, stopped, hold or disabled, or why the
	// name is not served (another site claims it).
	State string `json:"state"`
	Error string `json:"error,omitempty"`
}

// Routes are a site's names as the house routes them now: for a site with
// services, each host and prefix with its service, port and state; for
// any other, its names and its one instance.
func (m *Manager) Routes(domain string) ([]RouteState, error) {
	s, err := m.Get(domain)
	if err != nil {
		return nil, err
	}
	if s.Manifest == nil {
		return []RouteState{}, nil
	}
	owner := func(h string) string {
		if m.Engine == nil {
			return domain
		}
		if o, ok := m.Engine.Owner(h); ok {
			return o
		}
		return ""
	}
	state := func(alive, starting bool, stopped, errText string) string {
		switch {
		case stopped != "":
			return stopped
		case alive:
			return "up"
		case starting:
			return "starting"
		case errText != "":
			return "down"
		}
		return "starting"
	}
	out := []RouteState{}
	if len(s.Manifest.Services) > 0 {
		for _, r := range s.Manifest.Routes() {
			rs := RouteState{Host: r.Host, Prefix: r.Prefix, Service: r.Service}
			v := s.Services[r.Service]
			rs.Port, rs.Error = v.Port, v.Error
			rs.State = state(v.Alive, v.Starting || v.Waiting != "", v.Stopped, v.Error)
			if s.Mode == manifest.ModeDisabled || s.Mode == manifest.ModeHold {
				rs.State = string(s.Mode)
			}
			if o := owner(r.Host); o != domain && !strings.HasPrefix(r.Host, "www.") {
				rs.State, rs.Port = "not served: another site claims it too", 0
			}
			out = append(out, rs)
		}
		return out, nil
	}
	for _, h := range s.Manifest.Hosts(domain) {
		rs := RouteState{Host: h, Port: s.Port, Error: s.Error, State: state(s.Alive, s.Handing, "", s.Error)}
		switch s.Mode {
		case manifest.ModeStatic, manifest.ModeUpstream, manifest.ModeDisabled, manifest.ModeHold:
			rs.State, rs.Port = string(s.Mode), 0
		}
		if o := owner(h); o != domain && !strings.HasPrefix(h, "www.") {
			rs.State, rs.Port = "not served: another site claims it too", 0
		}
		out = append(out, rs)
	}
	return out, nil
}

// ArchiveDir is where Delete with archive=true moves folders.
func (m *Manager) ArchiveDir() string { return filepath.Join(m.Root, "data", "archive") }

// Delete removes a site folder, or moves it under data/archive/ with a
// timestamp. The instance is stopped first by writing disabled into the
// manifest and waiting for the engine: removing the folder under a running
// process would work on Linux, but a worker on a written port would keep
// that port until the engine noticed the folder was gone, and a site
// created again with the same name would then fail to bind.
func (m *Manager) Delete(ctx context.Context, domain string, keep bool) (archived string, err error) {
	return m.deleteWith(ctx, domain, keep, nil)
}

// StopForMove stops a site the way a delete does (disabled written, the
// engine waited for) and returns the manifest it had, for a move to take
// to the other node and to put back here if the move fails.
func (m *Manager) StopForMove(ctx context.Context, domain string) ([]byte, error) {
	dir, err := m.managedDir(domain)
	if err != nil {
		return nil, err
	}
	original, err := os.ReadFile(filepath.Join(dir, manifest.FileName))
	if err != nil {
		return nil, err
	}
	mf, err := manifest.Parse(original)
	if err != nil {
		return nil, fmt.Errorf("cannot stop %s for a move: %w", domain, err)
	}
	if keep := mf.MovingWorkers(false); len(keep) > 0 {
		return original, m.holdForMove(ctx, domain, dir, mf, keep)
	}
	return original, m.stopAndWait(ctx, domain, dir, original)
}

// holdForMove stops a site whose data is exported with it stopped, but not
// the workers that hold that data (a database): its instance and every
// other worker stop, and the site answers its maintenance page meanwhile.
// Disabled, the engine stopped the workers too, and the export had no
// database to read.
func (m *Manager) holdForMove(ctx context.Context, domain, dir string, mf *manifest.Manifest, keep []string) error {
	kept := map[string]bool{}
	for _, k := range keep {
		kept[k] = true
	}
	mf.Hold = &manifest.Hold{Enabled: true, Mode: "maintenance"}
	for name, w := range mf.Workers {
		if w != nil && !kept[name] {
			w.Disabled = true
		}
	}
	if err := manifest.Save(dir, mf); err != nil {
		return err
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		sn, ok := m.lookup(domain)
		done := !ok || !sn.Alive
		for name, w := range sn.Workers {
			if kept[name] {
				done = done && w.Alive
			} else {
				done = done && !w.Alive
			}
		}
		if done {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("%s: the engine did not stop the instance and keep %s up in time", domain, strings.Join(keep, ", "))
}

// Resume writes back the manifest StopForMove returned, which starts the
// site again as it was.
func (m *Manager) Resume(domain string, original []byte) error {
	dir, err := m.managedDir(domain)
	if err != nil {
		return err
	}
	if err := fsx.WriteAtomic(filepath.Join(dir, manifest.FileName), original, 0o644); err != nil {
		return err
	}
	m.emit("site.resumed", domain, "", "started again with the manifest it had", nil)
	return nil
}

// ArchiveMoved archives a site that was stopped for a move with the
// manifest it had before the stop, and removes its folder. The site runs
// on the other node by now: the pack has its own time, not the move's,
// and a pack a restart cut short (its folder already out of sites/, under
// data/archive as .packing) is packed again.
func (m *Manager) ArchiveMoved(ctx context.Context, domain string, original []byte) (string, error) {
	return m.deleteWith(context.WithoutCancel(ctx), domain, true, original)
}

// packingSuffix names a site's folder taken out of sites/ to be packed.
const packingSuffix = ".packing"

// beforePack, when set, is called with the folder about to be packed;
// an error fails the pack. The tests look at the house in that moment.
var beforePack func(folder string) error

func (m *Manager) deleteWith(ctx context.Context, domain string, keep bool, before []byte) (archived string, err error) {
	if !manifest.ValidName(domain) {
		return "", ErrNotFound
	}
	dir := m.Dir(domain)
	if st, err := os.Lstat(dir); err != nil || !st.IsDir() {
		if keep && before != nil {
			if packing := m.packingOf(domain); packing != "" {
				return m.pack(ctx, domain, packing, before, nil)
			}
		}
		return "", ErrNotFound
	}
	original, _ := os.ReadFile(filepath.Join(dir, manifest.FileName))
	if err := m.stopAndWait(ctx, domain, dir, original); err != nil {
		return "", err
	}
	if before != nil {
		original = before
	}
	if !keep {
		if err := os.RemoveAll(dir); err != nil {
			return "", err
		}
		_ = os.RemoveAll(m.logsDir(domain))
		m.emit("site.deleted", domain, "", "folder deleted", nil)
		return "", nil
	}
	if err := os.MkdirAll(m.ArchiveDir(), 0o755); err != nil {
		return "", err
	}
	// Out of sites/ before the manifest it had goes back in. Written in
	// place, as it first was, the engine read it at its next tick and
	// started the site again, workers and database included, for the
	// minutes the pack takes; a pack that failed, or a move whose time ran
	// out during it, left the site running here and on the node it moved
	// to. Renamed, the engine sees the folder gone at once, and nothing
	// written into it after is anything the engine reads.
	packing := filepath.Join(m.ArchiveDir(), domain+"-"+time.Now().UTC().Format("20060102T150405Z")+packingSuffix)
	if err := os.Rename(dir, packing); err != nil {
		return "", fmt.Errorf("archive: taking the folder out of sites/: %w", err)
	}
	stopped, _ := os.ReadFile(filepath.Join(packing, manifest.FileName))
	return m.pack(ctx, domain, packing, original, stopped)
}

// packingOf is a folder of domain's left under data/archive by a pack
// that did not finish, the newest; "" when there is none.
func (m *Manager) packingOf(domain string) string {
	names, _ := filepath.Glob(filepath.Join(m.ArchiveDir(), domain+"-*"+packingSuffix))
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	return names[len(names)-1]
}

// pack archives the folder taken out of sites/ (packing) with the
// manifest the operator had, and removes it. A pack that fails puts the
// folder back where it was with stopped, the manifest that kept it
// stopped: nothing was deleted, and nothing started.
func (m *Manager) pack(ctx context.Context, domain, packing string, original, stopped []byte) (string, error) {
	mf := filepath.Join(packing, manifest.FileName)
	if original != nil {
		// The archive keeps the manifest the operator had, not the stop
		// written to get there.
		_ = fsx.WriteAtomic(mf, original, 0o644)
	}
	dest := strings.TrimSuffix(packing, packingSuffix) + ".tar.gz"
	var rep archive.Report
	var err error
	if beforePack != nil {
		err = beforePack(packing)
	}
	if err == nil {
		rep, err = archive.Pack(ctx, packing, dest)
	}
	if err != nil {
		if stopped != nil {
			_ = fsx.WriteAtomic(mf, stopped, 0o644)
		}
		if !fsx.Exists(m.Dir(domain)) && os.Rename(packing, m.Dir(domain)) == nil {
			return "", fmt.Errorf("archive: %w (the folder is back in sites/, stopped)", err)
		}
		return "", fmt.Errorf("archive: %w (the folder is in %s)", err, packing)
	}
	if err := os.RemoveAll(packing); err != nil {
		return dest, err
	}
	_ = os.RemoveAll(m.logsDir(domain))
	m.emit("site.archived", domain, "", "packed to "+dest, map[string]any{"path": dest, "files": rep.Files, "bytes": rep.Bytes, "skipped": rep.Skipped})
	return dest, nil
}

// Archived is one archived site.
type Archived struct {
	Name   string    `json:"name"`
	Domain string    `json:"domain"`
	At     time.Time `json:"at"`
	Bytes  int64     `json:"bytes"`
}

// Archives lists data/archive, newest first.
func (m *Manager) Archives() []Archived {
	ents, _ := os.ReadDir(m.ArchiveDir())
	var out []Archived
	for _, e := range ents {
		name := e.Name()
		base, ok := strings.CutSuffix(name, ".tar.gz")
		if !ok || len(base) < 17 {
			continue
		}
		at, err := time.Parse("20060102T150405Z", base[len(base)-16:])
		if err != nil {
			continue
		}
		info, _ := e.Info()
		a := Archived{Name: name, Domain: strings.TrimSuffix(base[:len(base)-16], "-"), At: at}
		if info != nil {
			a.Bytes = info.Size()
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out
}

// ArchivePath is the file of an archive by name, refusing anything that
// is not one of data/archive's entries.
func (m *Manager) ArchivePath(name string) (string, Archived, error) {
	for _, a := range m.Archives() {
		if a.Name == name {
			return filepath.Join(m.ArchiveDir(), name), a, nil
		}
	}
	return "", Archived{}, ErrNotFound
}

// Restore unpacks an archive back to sites/<domain>, which must be free.
// The engine picks it up on its next tick, as it was when archived.
func (m *Manager) Restore(ctx context.Context, name string) (Site, error) {
	path, a, err := m.ArchivePath(name)
	if err != nil {
		return Site{}, err
	}
	if fsx.Exists(m.Dir(a.Domain)) {
		return Site{}, ErrExists
	}
	rep, err := archive.Unpack(ctx, path, m.Dir(a.Domain))
	if err != nil {
		return Site{}, err
	}
	m.emit("site.restored", a.Domain, "", "restored from "+name, map[string]any{"files": rep.Files, "bytes": rep.Bytes})
	return m.describe(a.Domain), nil
}

// DeleteArchive removes an archive for good.
func (m *Manager) DeleteArchive(name string) error {
	path, a, err := m.ArchivePath(name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	m.emit("archive.deleted", a.Domain, "", name+" deleted", nil)
	return nil
}

// Rename moves a site to a new name: its folder, its HOME, its logs, its
// conversations and its cost record, so the site under its new name is
// the same site and not a stranger with the old one's files. It is
// stopped first and comes back as it was.
//
// A project (a name without a dot) may be given a domain this way, and a
// site may become a project when nothing in it answers a hostname: the
// manifest has to hold under the new name, and the reason is said when it
// does not.
func (m *Manager) Rename(ctx context.Context, from, to string) (Site, error) {
	if !manifest.ValidName(to) {
		return Site{}, ErrBadDomain
	}
	dir, err := m.managedDir(from)
	if err != nil {
		return Site{}, err
	}
	if manifest.IsProject(to) && !manifest.IsProject(from) {
		if mf, err := manifest.Load(dir); err == nil && (mf.Run != nil || mf.Upstream != "" || mf.Static != nil || len(mf.Aliases) > 0) {
			return Site{}, fmt.Errorf("%s serves a hostname (run, upstream, static or aliases): a project answers none, so it cannot become %s; take those out of its manifest first", from, to)
		}
	}
	if fsx.Exists(m.Dir(to)) {
		return Site{}, ErrExists
	}
	original, _ := os.ReadFile(filepath.Join(dir, manifest.FileName))
	if err := m.stopAndWait(ctx, from, dir, original); err != nil {
		return Site{}, err
	}
	if err := os.Rename(dir, m.Dir(to)); err != nil {
		return Site{}, err
	}
	data := filepath.Join(m.Root, "data")
	for _, pair := range [][2]string{
		{filepath.Join(data, "homes", from), filepath.Join(data, "homes", to)},
		{m.logsDir(from), m.logsDir(to)},
		{filepath.Join(data, "chat", from+".json"), filepath.Join(data, "chat", to+".json")},
		{filepath.Join(data, "chat", from), filepath.Join(data, "chat", to)},
		{filepath.Join(data, "agents", "cost", from+".json"), filepath.Join(data, "agents", "cost", to+".json")},
		{filepath.Join(data, "history", from), filepath.Join(data, "history", to)},
	} {
		if fsx.Exists(pair[0]) && !fsx.Exists(pair[1]) {
			_ = os.Rename(pair[0], pair[1])
		}
	}
	if original != nil {
		_ = fsx.WriteAtomic(filepath.Join(m.Dir(to), manifest.FileName), original, 0o644)
	}
	m.emit("site.renamed", to, "", from+" is now "+to, map[string]any{"from": from})
	return m.describe(to), nil
}

// stopAndWait writes disabled into the manifest and waits for the engine
// to report nothing alive. Without a manifest or an engine there is
// nothing to stop.
func (m *Manager) stopAndWait(ctx context.Context, domain, dir string, original []byte) error {
	sn, ok := m.lookup(domain)
	if !ok || original == nil {
		return nil
	}
	running := sn.Alive
	for _, w := range sn.Workers {
		running = running || w.Alive
	}
	for _, v := range sn.Services {
		running = running || v.Alive
	}
	if !running {
		return nil
	}
	mf, err := manifest.Parse(original)
	if err != nil {
		return fmt.Errorf("cannot stop %s before removing it: %w", domain, err)
	}
	mf.Disabled = true
	if err := manifest.Save(dir, mf); err != nil {
		return err
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		sn, ok := m.lookup(domain)
		stopped := !ok || !sn.Alive
		for _, w := range sn.Workers {
			stopped = stopped && !w.Alive
		}
		for _, v := range sn.Services {
			stopped = stopped && !v.Alive
		}
		if stopped {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("%s: the engine did not stop the instance in time", domain)
}

// managedDir returns the folder of a site that has a manifest.
func (m *Manager) managedDir(domain string) (string, error) {
	if !manifest.ValidName(domain) {
		return "", ErrNotFound
	}
	dir := m.Dir(domain)
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return "", ErrNotFound
	}
	if !fsx.Exists(filepath.Join(dir, manifest.FileName)) {
		return "", ErrUnmanaged
	}
	return dir, nil
}
