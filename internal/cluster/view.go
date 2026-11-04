package cluster

import (
	"sort"
	"time"

	"github.com/dcimorra/the-middleware/internal/manifest"
)

// ReplicaView is one of a site's replicas as its primary sees it.
type ReplicaView struct {
	Node   string `json:"node"`
	NodeID string `json:"node_id"`
	// State is ready (serving this revision), behind (an older one, a
	// copy on its way), copying (being sent), syncing (installing
	// there), failed, or unreachable.
	State   string    `json:"state"`
	Alive   bool      `json:"alive"`
	Load    *Load     `json:"load,omitempty"`
	Error   string    `json:"error,omitempty"`
	Synced  time.Time `json:"synced,omitzero"`
	Refused string    `json:"refused,omitempty"`
}

// SiteView is a site's cluster, for the panel.
type SiteView struct {
	Domain    string            `json:"domain"`
	InNetwork bool              `json:"in_network"`
	Clustered bool              `json:"clustered"`
	Blockers  []string          `json:"blockers"`
	Cluster   *manifest.Cluster `json:"cluster,omitempty"`
	Want      int               `json:"want"`
	SpillAt   float64           `json:"spill_at"`
	Gateway   []string          `json:"gateway"`
	// Services are the site's services, Kept the ones that run only here.
	Services []string      `json:"services"`
	Kept     []string      `json:"kept"`
	Revision string        `json:"revision,omitempty"`
	Error    string        `json:"error,omitempty"`
	Load     Load          `json:"load"`
	Sharing  bool          `json:"sharing"`
	Replicas []ReplicaView `json:"replicas"`
	// Replica is set when the site here is another node's copy.
	Replica *manifest.Replica `json:"replica,omitempty"`
}

// View is domain's cluster as this node sees it.
func (m *Manager) View(domain string) (SiteView, bool) {
	sn, ok := m.opts.Sites.Site(domain)
	if !ok || sn.Manifest == nil {
		return SiteView{}, false
	}
	man := sn.Manifest
	v := SiteView{Domain: domain, Clustered: man.Clustered(), Blockers: man.Blockers(), Cluster: man.Cluster,
		Want: man.Cluster.Want(), SpillAt: man.Cluster.SpillAt(), Gateway: []string{}, Replicas: []ReplicaView{}, Replica: man.Replica}
	v.Services, v.Kept = man.ServiceNames(), man.KeptServices()
	if v.Services == nil {
		v.Services = []string{}
	}
	if v.Kept == nil {
		v.Kept = []string{}
	}
	if v.Blockers == nil {
		v.Blockers = []string{}
	}
	if man.Cluster != nil {
		for _, g := range man.Cluster.Gateway {
			if a, err := manifest.GatewayAddr(g); err == nil {
				v.Gateway = append(v.Gateway, a)
			}
		}
	}
	mesh := m.getMesh()
	v.InNetwork = mesh != nil && mesh.InNetwork()
	m.mu.Lock()
	defer m.mu.Unlock()
	v.Load, v.Sharing = m.local, m.over[domain]
	p := m.prims[domain]
	if p == nil {
		return v, true
	}
	v.Revision, v.Error = p.fingerprint, p.fpErr
	for _, id := range m.placement[domain] {
		nv := m.views[id]
		r := ReplicaView{NodeID: id, Error: p.errs[id], Synced: p.synced[id], Refused: p.refused[id]}
		if nv == nil {
			r.State = "unreachable"
			v.Replicas = append(v.Replicas, r)
			continue
		}
		r.Node = nv.member.Name
		l := nv.status.Load
		r.Load = &l
		rs, has := nv.status.Replicas[domain]
		switch {
		case !m.freshLocked(nv):
			r.State = "unreachable"
			if r.Error == "" {
				r.Error = nv.err
			}
		case p.syncing[id]:
			r.State = "copying"
		case has && rs.State == StateSyncing:
			r.State = StateSyncing
		case has && rs.State == StateFailed && rs.Pending == p.fingerprint:
			r.State, r.Error = StateFailed, rs.Error
		case has && rs.Revision == p.fingerprint:
			r.State = StateReady
		default:
			r.State = "behind"
		}
		if has {
			r.Alive = rs.Alive
			if rs.Error != "" && r.Error == "" {
				r.Error = rs.Error
			}
		}
		v.Replicas = append(v.Replicas, r)
	}
	return v, true
}

// Kept is a replica this node keeps for another.
type Kept struct {
	Domain  string `json:"domain"`
	Primary string `json:"primary"`
	ReplicaStatus
}

// Kept are the replicas this node keeps, by domain.
func (m *Manager) Kept() []Kept {
	var out []Kept
	for domain, st := range m.Replicas("") {
		k := Kept{Domain: domain, ReplicaStatus: st}
		if sn, ok := m.opts.Sites.Site(domain); ok && sn.Manifest.IsReplica() {
			k.Primary = sn.Manifest.Replica.Primary
		}
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Domain < out[j].Domain })
	if out == nil {
		out = []Kept{}
	}
	return out
}

// LocalLoad is this node's load as the last round read it.
func (m *Manager) LocalLoad() Load {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.local
}
