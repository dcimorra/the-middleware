package cluster

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/archive"
	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/nodes"
)

// The keeper's side: what a primary sends lands in data/cluster/incoming
// until it is whole and installed, and only then takes the place of the
// copy that serves, in one step. A copy that fails to arrive or install
// leaves the one that serves as it was.

// Errors the API maps to statuses.
var (
	ErrBadName = errors.New("not a site's name")
	// ErrTaken is a site of this node with that name, or another node's
	// replica: answered 409, and that primary places it elsewhere.
	ErrTaken      = errors.New("that name is a site of this node, or another node's replica")
	ErrBusy       = errors.New("a copy of that site is being installed here")
	ErrNoIncoming = errors.New("nothing was received for that site")
	ErrNotReplica = errors.New("no replica of that site here")
)

// installTime bounds a copy's install, as a move's does.
const installTime = 30 * time.Minute

func (m *Manager) incoming(domain string) string {
	return filepath.Join(m.dir(), "incoming", domain)
}

// replicaOf is the replica's mark in the manifest on disk, nil when the
// folder is not a replica (or is not there).
func (m *Manager) replicaOf(domain string) (*manifest.Replica, bool, error) {
	dir := m.siteDir(domain)
	if _, err := os.Lstat(dir); err != nil {
		return nil, false, nil
	}
	man, err := manifest.Load(dir)
	if err != nil || man.Replica == nil {
		return nil, true, ErrTaken
	}
	return man.Replica, true, nil
}

// mayKeep says from may send domain here: the name is free, or it is
// already from's replica.
func (m *Manager) mayKeep(from nodes.Member, domain string) error {
	if !manifest.ValidName(domain) || manifest.IsProject(domain) {
		return ErrBadName
	}
	r, _, err := m.replicaOf(domain)
	if err != nil {
		return err
	}
	if r != nil && r.Of != from.ID {
		return ErrTaken
	}
	return nil
}

// Receive takes a copy of domain from its primary into the incoming
// folder, replacing what an earlier attempt left there.
func (m *Manager) Receive(ctx context.Context, from nodes.Member, domain string, body io.Reader) (archive.Report, error) {
	if err := m.mayKeep(from, domain); err != nil {
		return archive.Report{}, err
	}
	m.mu.Lock()
	if j := m.jobs[domain]; j != nil && j.running {
		m.mu.Unlock()
		return archive.Report{}, ErrBusy
	}
	m.mu.Unlock()
	dir := m.incoming(domain)
	if err := os.RemoveAll(dir); err != nil {
		return archive.Report{}, err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return archive.Report{}, err
	}
	return archive.UnpackFrom(ctx, body, dir)
}

// replicaManifest is the primary's manifest as a replica keeps it: marked,
// and without what only the primary does (it deploys, its agent works on
// the site, the automatic repair looks at it, it asks for replicas).
func replicaManifest(raw []byte, from nodes.Member, req CommitRequest) (*manifest.Manifest, error) {
	man, err := manifest.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("the primary's manifest: %w", err)
	}
	if b := man.Blockers(); len(b) > 0 {
		return nil, fmt.Errorf("this site cannot have replicas: %s", strings.Join(b, "; "))
	}
	var gw []string
	if man.Cluster != nil {
		for _, g := range man.Cluster.Gateway {
			if a, err := manifest.GatewayAddr(g); err == nil {
				gw = append(gw, a)
			}
		}
	}
	man.Cluster = nil
	// The databases' copies are the primary's to take: here they would
	// copy the same database again, through the gateway.
	man.Backups = nil
	man.Deploy, man.Agent, man.Chat = nil, nil, nil
	no := false
	man.Autofix = &no
	primary := req.Primary
	if primary == "" {
		primary = from.Name
	}
	man.Replica = &manifest.Replica{Of: from.ID, Primary: primary, Revision: req.Revision, Gateway: gw}
	return man, man.Validate()
}

// Commit starts putting the received copy in place: its dependencies
// installed for this machine (or carried over from the copy that serves
// when its lockfiles did not change), its manifest marked, then swapped
// in for the copy that serves. It answers at once; Status says how it
// goes.
func (m *Manager) Commit(from nodes.Member, domain string, req CommitRequest) error {
	if err := m.mayKeep(from, domain); err != nil {
		return err
	}
	if req.Revision == "" {
		return errors.New("revision: empty")
	}
	raw, err := base64.StdEncoding.DecodeString(req.Manifest)
	if err != nil {
		return errors.New("manifest: not base64")
	}
	man, err := replicaManifest(raw, from, req)
	if err != nil {
		return err
	}
	site := filepath.Join(m.incoming(domain), "site")
	if !fsx.Exists(site) {
		return ErrNoIncoming
	}
	var dirs []string
	for _, d := range req.Dirs {
		c := filepath.Clean(filepath.FromSlash(d))
		if filepath.IsAbs(c) || c == ".." || strings.HasPrefix(c, ".."+string(filepath.Separator)) {
			return fmt.Errorf("dirs: %q is not inside the site", d)
		}
		dirs = append(dirs, c)
	}
	m.mu.Lock()
	if j := m.jobs[domain]; j != nil && j.running {
		m.mu.Unlock()
		return ErrBusy
	}
	j := &job{revision: req.Revision, running: true}
	m.jobs[domain] = j
	m.mu.Unlock()
	go func() {
		err := m.place1(domain, site, man, dirs)
		m.mu.Lock()
		j.running = false
		if err != nil {
			j.err = err.Error()
		} else {
			j.placed = m.now()
		}
		m.mu.Unlock()
		if err != nil {
			m.emit("cluster.replica_failed", "warn", domain, fmt.Sprintf("the copy from %s was not put in place: %v", man.Replica.Primary, err), nil)
			return
		}
		if mesh := m.getMesh(); mesh != nil {
			m.reconcileGateways(mesh)
		}
		m.emit("cluster.replica_placed", "", domain, fmt.Sprintf("replica of %s at revision %s", man.Replica.Primary, req.Revision), map[string]any{"revision": req.Revision, "primary": man.Replica.Primary})
	}()
	return nil
}

func (m *Manager) place1(domain, site string, man *manifest.Manifest, dirs []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), installTime)
	defer cancel()
	cur := m.siteDir(domain)
	for _, d := range dirs {
		dst := filepath.Join(site, d)
		old := filepath.Join(cur, d)
		if fsx.Exists(filepath.Join(old, "node_modules")) && sameDeps(old, dst) {
			if err := linkTree(filepath.Join(old, "node_modules"), filepath.Join(dst, "node_modules")); err == nil {
				continue
			}
			_ = os.RemoveAll(filepath.Join(dst, "node_modules"))
		}
		m.mu.Lock()
		install := m.opts.Install
		m.mu.Unlock()
		if install == nil {
			continue
		}
		if _, err := install(ctx, dst); err != nil {
			return fmt.Errorf("installing its dependencies in %s: %w", filepath.ToSlash(d), err)
		}
	}
	// Not manifest.Save: it reads the folder's name as the site's, and
	// the incoming folder is named "site".
	if err := man.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return err
	}
	if err := fsx.WriteAtomic(filepath.Join(site, manifest.FileName), append(data, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(m.opts.Root, "sites"), 0o755); err != nil {
		return err
	}
	// The copy that served is set aside, not deleted: its process keeps
	// running from it until the new one has taken over.
	old := filepath.Join(m.dir(), "old")
	if err := os.MkdirAll(old, 0o700); err != nil {
		return err
	}
	m.clearOld(domain)
	if _, err := os.Lstat(cur); err == nil {
		if exchange(site, cur) == nil {
			aside := filepath.Join(old, domain+"-"+m.now().UTC().Format("20060102T150405.000"))
			_ = os.Rename(site, aside)
			_ = os.RemoveAll(m.incoming(domain))
			return nil
		}
		aside := filepath.Join(old, domain+"-"+m.now().UTC().Format("20060102T150405.000"))
		if err := os.Rename(cur, aside); err != nil {
			return err
		}
	}
	if err := os.Rename(site, cur); err != nil {
		return err
	}
	return os.RemoveAll(m.incoming(domain))
}

// clearOld removes the copies of domain set aside before the last one:
// the last one's process has long been replaced by the time another
// copy arrives.
func (m *Manager) clearOld(domain string) {
	list, _ := filepath.Glob(filepath.Join(m.dir(), "old", domain+"-*"))
	for _, p := range list {
		_ = os.RemoveAll(p)
	}
}

// Remove takes a replica out: its folder goes, which stops it.
func (m *Manager) Remove(from *nodes.Member, domain string) error {
	r, _, err := m.replicaOf(domain)
	if err != nil {
		return ErrNotReplica
	}
	if r == nil {
		return ErrNotReplica
	}
	if from != nil && r.Of != from.ID {
		return ErrNotReplica
	}
	m.mu.Lock()
	if j := m.jobs[domain]; j != nil && j.running {
		m.mu.Unlock()
		return ErrBusy
	}
	delete(m.jobs, domain)
	m.mu.Unlock()
	old := filepath.Join(m.dir(), "old")
	if err := os.MkdirAll(old, 0o700); err != nil {
		return err
	}
	m.clearOld(domain)
	aside := filepath.Join(old, domain+"-"+m.now().UTC().Format("20060102T150405.000"))
	if err := os.Rename(m.siteDir(domain), aside); err != nil {
		return err
	}
	_ = os.RemoveAll(m.incoming(domain))
	m.emit("cluster.replica_removed", "", domain, "replica of "+r.Primary+" removed", nil)
	// The engine stops it within a tick; its folder goes after.
	go func() {
		time.Sleep(time.Minute)
		_ = os.RemoveAll(aside)
	}()
	return nil
}

// Replicas are the replicas kept here, by domain; of, when not empty,
// only that primary's.
func (m *Manager) Replicas(of string) map[string]ReplicaStatus {
	out := map[string]ReplicaStatus{}
	for _, sn := range m.opts.Sites.Sites() {
		r := sn.Manifest
		if !r.IsReplica() || (of != "" && r.Replica.Of != of) {
			continue
		}
		st := ReplicaStatus{Of: r.Replica.Of, Revision: r.Replica.Revision, State: StateReady}
		switch sn.Mode {
		case manifest.ModeRun:
			st.Alive = sn.Alive && sn.Port != 0 && !sn.Handing
		case manifest.ModeStatic:
			st.Alive = true
		}
		m.mu.Lock()
		if e := m.gwErrs[sn.Domain]; e != "" {
			st.Alive, st.Error = false, e
		}
		if j := m.jobs[sn.Domain]; j != nil {
			if j.running {
				st.State, st.Pending = StateSyncing, j.revision
			} else if j.err != "" {
				st.State, st.Pending, st.Error = StateFailed, j.revision, j.err
			}
			st.Synced = j.placed
		}
		m.mu.Unlock()
		out[sn.Domain] = st
	}
	// A first copy being installed has no site yet.
	m.mu.Lock()
	for domain, j := range m.jobs {
		if _, ok := out[domain]; ok {
			continue
		}
		st := ReplicaStatus{Pending: j.revision, State: StateSyncing}
		if !j.running {
			if j.err == "" {
				continue // placed: the engine shows it next tick
			}
			st.State, st.Error = StateFailed, j.err
		}
		out[domain] = st
	}
	m.mu.Unlock()
	return out
}

// keepGrace is how long a replica is kept after it was placed although
// its primary does not name it: the primary's placement and this copy can
// cross in one round.
const keepGrace = 2 * time.Minute

// Status answers a primary's round: this node's load and that primary's
// replicas here, after removing the ones it no longer names.
func (m *Manager) Status(from nodes.Member, req StatusRequest) Status {
	want := map[string]bool{}
	for _, d := range req.Expect {
		want[d] = true
	}
	for domain, st := range m.Replicas(from.ID) {
		if want[domain] || st.State == StateSyncing {
			continue
		}
		if m.placedRecently(domain) {
			continue
		}
		if err := m.Remove(&from, domain); err == nil {
			m.emit("cluster.replica_retired", "", domain, from.Name+" no longer keeps a replica of it here", nil)
		}
	}
	m.mu.Lock()
	l := m.local
	m.mu.Unlock()
	if l.Cores == 0 {
		l = m.opts.Load()
	}
	return Status{Load: l, Replicas: m.Replicas(from.ID)}
}

func (m *Manager) placedRecently(domain string) bool {
	m.mu.Lock()
	j := m.jobs[domain]
	m.mu.Unlock()
	if j != nil && !j.placed.IsZero() {
		return m.now().Sub(j.placed) < keepGrace
	}
	info, err := os.Stat(filepath.Join(m.siteDir(domain), manifest.FileName))
	return err == nil && m.now().Sub(info.ModTime()) < keepGrace
}

// orphanAfter is how long a replica outlives its primary leaving the
// network: long enough for a node that restarts or changes address, short
// enough that a node removed for good does not leave sites running.
const orphanAfter = 30 * time.Minute

// sweepOrphans removes the replicas whose primary is no longer a node of
// the network.
func (m *Manager) sweepOrphans(members []nodes.Member) {
	live := map[string]bool{}
	for _, x := range members {
		live[x.ID] = true
	}
	for domain, st := range m.Replicas("") {
		if st.Of == "" || live[st.Of] {
			m.mu.Lock()
			delete(m.orphans, domain)
			m.mu.Unlock()
			continue
		}
		m.mu.Lock()
		since, ok := m.orphans[domain]
		if !ok {
			m.orphans[domain] = m.now()
		}
		m.mu.Unlock()
		if ok && m.now().Sub(since) >= orphanAfter {
			if m.Remove(nil, domain) == nil {
				m.emit("cluster.replica_retired", "", domain, "its primary is no longer a node of the network", nil)
			}
			m.mu.Lock()
			delete(m.orphans, domain)
			m.mu.Unlock()
		}
	}
}
