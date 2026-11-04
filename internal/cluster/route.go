package cluster

import (
	"context"
	"hash/fnv"
	"math"
	"net/http"
	"time"

	"github.com/dcimorra/the-middleware/internal/edge"
	"github.com/dcimorra/the-middleware/internal/engine"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/nodes"
	"github.com/dcimorra/the-middleware/internal/pages"
	"github.com/dcimorra/the-middleware/internal/proxy"
)

// downFor is how long a node a request could not reach is left out, before
// the rounds' word on it is believed again.
const downFor = 30 * time.Second

// overflowExit is how far under its spill a primary's load must fall
// before it stops sharing: leaving at the spill itself sent everything
// back the moment sharing worked, and the load climbed over it again.
const overflowExit = 0.6

// option is one node a request may go to; the zero member is this one.
type option struct {
	member nodes.Member
	local  bool
	weight float64
}

// weight is a node's share: its cores times what is free of them, never
// nothing, so a busy node still takes a little and is seen to recover.
func weight(l Load) float64 {
	cores := l.Cores
	if cores < 1 {
		cores = 1
	}
	return float64(cores) * math.Max(0.05, 1-l.PerCore)
}

// Pick says where a request for domain goes: false is here, as always;
// a member is the replica there. key keeps one visitor on one node (their
// address), so a session the site keeps in its memory is not lost from
// one page to the next.
func (m *Manager) Pick(domain string, sn engine.Snapshot, key string) (nodes.Member, bool) {
	c := sn.Manifest.Cluster
	if c == nil || sn.Manifest.Replica != nil {
		return nodes.Member{}, false
	}
	if sn.Mode != manifest.ModeRun && sn.Mode != manifest.ModeStatic {
		return nodes.Member{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.prims[domain]
	if p == nil || p.fingerprint == "" {
		return nodes.Member{}, false
	}
	now := m.now()
	var opts []option
	for _, id := range m.placement[domain] {
		v := m.views[id]
		if !m.freshLocked(v) || (!v.down.IsZero() && now.Sub(v.down) < downFor) {
			continue
		}
		rs, ok := v.status.Replicas[domain]
		if !ok || rs.Revision != p.fingerprint || !rs.Alive {
			continue
		}
		opts = append(opts, option{member: v.member, weight: weight(v.status.Load)})
	}
	if len(opts) == 0 {
		return nodes.Member{}, false
	}
	localDown := sn.Mode == manifest.ModeRun && (!sn.Alive || sn.Port == 0)
	if !localDown {
		util, spill := m.local.PerCore, c.SpillAt()
		switch {
		case util >= spill:
			if !m.over[domain] {
				m.over[domain] = true
				m.emit("cluster.overflow", "", domain, "this node is saturated: requests are shared with the replicas", map[string]any{"load": util})
			}
		case util < spill*overflowExit && m.over[domain]:
			m.over[domain] = false
			m.emit("cluster.overflow_end", "", domain, "this node is comfortable again: it serves every request", map[string]any{"load": util})
		}
		if !c.Spreads() && !m.over[domain] {
			return nodes.Member{}, false
		}
		// Sharing because it is saturated, this node keeps the share its
		// spill says it is good for: weighed by its free capacity it
		// would take almost nothing, empty itself, stop sharing, and
		// fill up again.
		w := weight(m.local)
		if !c.Spreads() {
			cores := m.local.Cores
			if cores < 1 {
				cores = 1
			}
			w = float64(cores) * math.Max(0.05, 1-spill*overflowExit)
		}
		opts = append(opts, option{local: true, weight: w})
	}
	best, bestScore := -1, -1.0
	for i, o := range opts {
		id := "self"
		if !o.local {
			id = o.member.ID
		}
		// Weighted rendezvous hashing: each visitor ranks the nodes the
		// same way every time, and a node that joins or leaves moves only
		// its own share of visitors.
		h := fnv.New64a()
		_, _ = h.Write([]byte(key + "|" + id))
		u := (float64(h.Sum64()>>11) + 0.5) / float64(uint64(1)<<53)
		score := o.weight / -math.Log(u)
		if score > bestScore {
			best, bestScore = i, score
		}
	}
	if opts[best].local {
		return nodes.Member{}, false
	}
	return opts[best].member, true
}

func (m *Manager) markDown(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v := m.views[id]; v != nil {
		v.down = m.now()
	}
}

// who writes the visitor, as this node believes it, the way the tunnel's
// connector writes it: the replica takes it from there. What the visitor
// wrote in those headers was already weighed by edge here and is
// replaced.
func who(in *http.Request, out http.Header) {
	for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Real-Ip", "Forwarded", "True-Client-Ip", "Cf-Connecting-Ipv6", "X-Client-Ip", "Cf-Ipcountry", "Cf-Visitor"} {
		out.Del(h)
	}
	if ip := edge.ClientIP(in); ip != nil {
		out.Set("CF-Connecting-IP", ip.String())
	} else {
		out.Del("CF-Connecting-IP")
	}
	if c := edge.Country(in); c != "" {
		out.Set("CF-IPCountry", c)
	}
	if edge.HTTPS(in) {
		out.Set("X-Forwarded-Proto", "https")
	} else {
		out.Set("X-Forwarded-Proto", "http")
	}
}

// replayable says a request that could not reach a replica can be served
// here instead: nothing of it was read but its headers.
func replayable(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0
	}
	return false
}

// Wrap is the request chain's part (after the cache, so a page the cache
// keeps is served here without a hop): a replica's name through any door
// of this node answers as a name nobody serves, and a clustered site's
// request goes where Pick says.
func (m *Manager) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		domain, ok := m.opts.Sites.Owner(proxy.Host(r))
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		sn, ok := m.opts.Sites.Site(domain)
		if !ok || sn.Manifest == nil {
			next.ServeHTTP(w, r)
			return
		}
		if sn.Manifest.IsReplica() {
			// The closed door: a replica exists only for its primary.
			pages.Write(w, r, "unknown", http.StatusNotFound)
			return
		}
		if !sn.Manifest.Clustered() {
			next.ServeHTTP(w, r)
			return
		}
		mesh := m.getMesh()
		if mesh == nil {
			next.ServeHTTP(w, r)
			return
		}
		key := ""
		if ip := edge.ClientIP(r); ip != nil {
			key = ip.String()
		}
		x, ok := m.Pick(domain, sn, key)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		mesh.SiteProxy(x, who, func(w http.ResponseWriter, r *http.Request, err error) {
			m.markDown(x.ID)
			m.emit("cluster.unreachable", "warn", domain, "the replica on "+x.Name+" could not be reached: "+err.Error(), map[string]any{"node": x.Name})
			if replayable(r) {
				next.ServeHTTP(w, r)
				return
			}
			pages.Render(w, r, proxy.SiteNotice(sn, "down"))
		}).ServeHTTP(w, r)
	})
}

// ReplicaHandler serves what a primary hands this node through the mesh:
// a request for one of its replicas, and nothing else (a name of this
// node's own sites is not another node's to ask for).
func (m *Manager) ReplicaHandler(site http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		from, _ := nodes.MemberFrom(r.Context())
		domain, ok := m.opts.Sites.Owner(proxy.Host(r))
		var sn engine.Snapshot
		if ok {
			sn, ok = m.opts.Sites.Site(domain)
		}
		if !ok || !sn.Manifest.IsReplica() || sn.Manifest.Replica.Of != from.ID {
			pages.Write(w, r, "unknown", http.StatusNotFound)
			return
		}
		site.ServeHTTP(w, r)
	})
}

// RunHook lets the manager be an app hook: Run starts the rounds.
func (m *Manager) RunHook(ctx context.Context) { go m.Run(ctx) }
