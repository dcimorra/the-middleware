package guard

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/runtime"
)

// A site's resources over time: CPU (percent of one core) and resident
// memory, summed over every process attributed to it, one sample a minute
// for a day. An episode is a stretch of five minutes or more above a
// threshold; it is an event when it starts and when it ends, so "the
// machine was slow last night" has a site and a time attached.

// Sample is one minute of a site.
type Sample struct {
	At  time.Time `json:"at"`
	CPU float64   `json:"cpu"` // percent of one core, averaged over the minute
	RSS int64     `json:"rss"` // bytes, at the sample
}

// Episode is a stretch above a threshold.
type Episode struct {
	Kind  string    `json:"kind"` // "cpu" or "memory"
	Start time.Time `json:"start"`
	End   time.Time `json:"end,omitzero"`
	Peak  float64   `json:"peak"` // percent for cpu, bytes for memory
}

// Resources is what is kept per site, and per worker.
type Resources struct {
	Samples  []Sample  `json:"samples"`
	Episodes []Episode `json:"episodes"`
	// Workers is each worker's last sample, in a site's history (whose
	// samples are the site and its workers together): what each one takes
	// now, without asking for each history.
	Workers map[string]Sample `json:"workers,omitempty"`
}

const (
	sampleEvery    = time.Minute
	keepSamples    = 24 * 60
	keepEpisodes   = 50
	episodeAfter   = 5 // minutes above the line before it is an episode
	cpuLine        = 90.0
	memoryLine     = int64(2 << 30)
	persistEvery   = 5 * time.Minute
	ticksPerSecond = 100
	// liveEvery is the window of what a site takes now. The minute samples
	// are the history; on the dashboard, a site that started burning a
	// core half a minute ago is the answer to "why is the node at 84%"
	// and a minute late it is not. Shorter than ten seconds and one
	// request finishing inside the window reads as a spike: the guard
	// sweeps every three, so it is about four sweeps.
	liveEvery = 10 * time.Second
	// liveStale is when a reading no longer says "now": a site that left
	// the house is no longer swept, and its last one must not stay on the
	// dashboard as if it were still burning.
	liveStale = time.Minute
)

type siteMeter struct {
	res       Resources
	lastTicks int64
	lastAt    time.Time
	above     map[string]int // consecutive minutes above, by kind
	below     map[string]int
	open      map[string]*Episode
	persisted time.Time
	// The reading of now, over liveEvery (see Usage).
	live      Sample
	liveAt    time.Time
	liveTicks int64
}

type meters struct {
	mu    sync.Mutex
	sites map[string]*siteMeter
}

// resourcesPath is a site's history, or a worker's ("shop.example/db",
// in a folder of the site's).
func (g *Guard) resourcesPath(key string) string {
	return filepath.Join(g.opts.DataDir, "resources", key+".json")
}

// unit is one thing the engine runs for a site: its instance (name "") or
// one of its workers, as its state file records it.
type unit struct {
	name      string
	pid       int
	container string // the container's id
}

// units reads a site's state files: data/state/<domain>.json for its
// instance and data/state/<domain>@<worker>.json for each worker (the
// engine's statePath).
func (g *Guard) units(domain string) []unit {
	var out []unit
	read := func(name, path string) {
		var st runtime.State
		if fsx.ReadJSON(path, &st) == nil && (st.PID > 0 || st.ContainerID != "") {
			out = append(out, unit{name: name, pid: st.PID, container: st.ContainerID})
		}
	}
	read("", runtime.StatePath(g.opts.DataDir, domain))
	ents, _ := os.ReadDir(filepath.Join(g.opts.DataDir, "state"))
	for _, e := range ents {
		rest, ok := strings.CutPrefix(e.Name(), domain+"@")
		if name, isJSON := strings.CutSuffix(rest, ".json"); ok && isJSON && name != "" && !e.IsDir() {
			read(name, runtime.StatePath(g.opts.DataDir, domain+"/"+name))
		}
	}
	return out
}

// measureUnits measures a site and each of its workers apart. tree is what
// attribute found (the instance's tree and whatever works in the folder);
// each unit adds its own processes: a process runtime's by its pid, a
// container's by the id in its processes' cgroup. Before, a container
// counted only when its processes' working directory could be read and was
// the folder: a rootful daemon's could not be read, and a database's
// working directory is its data, so hogarify's PostgreSQL and every docker
// site of a rootful machine measured nothing.
func (g *Guard) measureUnits(domain string, tree []*Process, procs []Process, byPID map[int]*Process) {
	all := map[int]*Process{}
	for _, p := range tree {
		all[p.PID] = p
	}
	for _, u := range g.units(domain) {
		var mine []*Process
		for i := range procs {
			p := &procs[i]
			switch {
			case u.container != "" && strings.Contains(p.Cgroup, u.container):
			case u.pid > 0 && (p.PID == u.pid || p.PGID == u.pid || descends(p, u.pid, byPID)):
			default:
				continue
			}
			mine = append(mine, p)
			all[p.PID] = p
		}
		if u.name != "" {
			g.measure(domain+"/"+u.name, domain, mine)
		}
	}
	total := make([]*Process, 0, len(all))
	for _, p := range all {
		total = append(total, p)
	}
	g.measure(domain, domain, total)
}

func (g *Guard) meter(domain string) *siteMeter {
	g.res.mu.Lock()
	defer g.res.mu.Unlock()
	if g.res.sites == nil {
		g.res.sites = map[string]*siteMeter{}
	}
	m := g.res.sites[domain]
	if m == nil {
		m = &siteMeter{above: map[string]int{}, below: map[string]int{}, open: map[string]*Episode{}}
		_ = fsx.ReadJSON(g.resourcesPath(domain), &m.res)
		g.res.sites[domain] = m
	}
	return m
}

// measure adds a site's processes, or a worker's, to its meter (key is the
// domain, or domain/worker; site is the domain its events name); a sample
// is taken once a minute from the CPU ticks consumed since the last one.
func (g *Guard) measure(key, site string, tree []*Process) {
	var ticks, rss int64
	for _, p := range tree {
		ticks += p.CPUTicks
		rss += p.RSS
	}
	now := g.now()
	m := g.meter(key)
	g.res.mu.Lock()
	defer g.res.mu.Unlock()
	if m.liveAt.IsZero() {
		m.liveAt, m.liveTicks = now, ticks
	} else if el := now.Sub(m.liveAt); el >= liveEvery {
		m.live = Sample{At: now.UTC(), CPU: rate(ticks-m.liveTicks, el), RSS: rss}
		m.liveAt, m.liveTicks = now, ticks
	}
	if m.lastAt.IsZero() {
		m.lastAt, m.lastTicks = now, ticks
		return
	}
	elapsed := now.Sub(m.lastAt)
	if elapsed < sampleEvery {
		return
	}
	s := Sample{At: now.UTC(), CPU: rate(ticks-m.lastTicks, elapsed), RSS: rss}
	m.lastAt, m.lastTicks = now, ticks
	m.res.Samples = append(m.res.Samples, s)
	if len(m.res.Samples) > keepSamples {
		m.res.Samples = m.res.Samples[len(m.res.Samples)-keepSamples:]
	}
	g.episode(key, site, m, "cpu", s.CPU >= cpuLine, s.CPU, now)
	g.episode(key, site, m, "memory", s.RSS >= memoryLine, float64(s.RSS), now)
	if now.Sub(m.persisted) >= persistEvery {
		m.persisted = now
		g.persist(key, m.res)
	}
}

// rate is CPU ticks over a stretch as percent of one core, to a tenth. A
// negative delta is processes that ended: their ticks left the sum.
func rate(delta int64, over time.Duration) float64 {
	if delta < 0 || over <= 0 {
		return 0
	}
	cpu := float64(delta) / ticksPerSecond / over.Seconds() * 100
	return float64(int(cpu*10)) / 10
}

// persist writes a meter's history; a worker's goes in its site's folder.
func (g *Guard) persist(key string, res Resources) {
	path := g.resourcesPath(key)
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = fsx.WriteJSON(path, res, 0o644)
}

// episode moves one kind's state by one sample. Called under the lock.
func (g *Guard) episode(key, domain string, m *siteMeter, kind string, high bool, value float64, now time.Time) {
	who := kind
	if _, worker, ok := strings.Cut(key, "/"); ok {
		who = "worker " + worker + ": " + kind
	}
	if high {
		m.above[kind]++
		m.below[kind] = 0
	} else {
		m.below[kind]++
		m.above[kind] = 0
	}
	ep := m.open[kind]
	switch {
	case ep == nil && m.above[kind] >= episodeAfter:
		ep = &Episode{Kind: kind, Start: now.Add(-episodeAfter * sampleEvery).UTC(), Peak: value}
		m.open[kind] = ep
		g.emit(events.Event{Type: "resources.episode", Subject: domain, Actor: "guard", Level: "warn",
			Message: fmt.Sprintf("%s above the line for %d minutes (%s)", who, episodeAfter, show(kind, value)), Data: map[string]any{"kind": kind}})
	case ep != nil && value > ep.Peak:
		ep.Peak = value
	}
	if ep != nil && m.below[kind] >= episodeAfter {
		ep.End = now.Add(-episodeAfter * sampleEvery).UTC()
		m.res.Episodes = append(m.res.Episodes, *ep)
		if len(m.res.Episodes) > keepEpisodes {
			m.res.Episodes = m.res.Episodes[len(m.res.Episodes)-keepEpisodes:]
		}
		delete(m.open, kind)
		g.emit(events.Event{Type: "resources.episode_end", Subject: domain, Actor: "guard",
			Message: fmt.Sprintf("%s back below the line after %s, peak %s", who, ep.End.Sub(ep.Start).Round(time.Minute), show(kind, ep.Peak)), Data: map[string]any{"kind": kind}})
		g.persist(key, m.res)
	}
}

func show(kind string, v float64) string {
	if kind == "cpu" {
		return fmt.Sprintf("%.0f%% of a core", v)
	}
	return fmt.Sprintf("%.1f GB", v/(1<<30))
}

// Resources is a site's history, with any episode still open at the end.
// A worker's is Resources("domain/worker").
func (g *Guard) Resources(domain string) Resources {
	m := g.meter(domain)
	g.res.mu.Lock()
	defer g.res.mu.Unlock()
	out := Resources{Samples: append([]Sample{}, m.res.Samples...), Episodes: append([]Episode{}, m.res.Episodes...)}
	for _, ep := range m.open {
		out.Episodes = append(out.Episodes, *ep)
	}
	if !strings.Contains(domain, "/") {
		for key, wm := range g.res.sites {
			worker, ok := strings.CutPrefix(key, domain+"/")
			if ok && len(wm.res.Samples) > 0 {
				if out.Workers == nil {
					out.Workers = map[string]Sample{}
				}
				out.Workers[worker] = wm.res.Samples[len(wm.res.Samples)-1]
			}
		}
	}
	return out
}

// SiteUse is what a site takes now: CPU as percent of one core (a site on
// two full cores is 200) and resident memory, its workers included.
type SiteUse struct {
	Domain string  `json:"domain"`
	CPU    float64 `json:"cpu"`
	RSS    int64   `json:"rss"`
}

// Usage is what every site takes now, the hungriest first, for the
// dashboard's "which site is the node's CPU": the reading over the last
// liveEvery, not the minute's sample. A site that takes nothing (a static
// one, a stopped one) is left out, so the list is the sites that weigh;
// one not swept for liveStale has left the house.
func (g *Guard) Usage() []SiteUse {
	now := g.now()
	g.res.mu.Lock()
	out := []SiteUse{}
	for key, m := range g.res.sites {
		if strings.Contains(key, "/") || m.live.At.IsZero() || now.Sub(m.live.At) > liveStale {
			continue
		}
		if m.live.CPU == 0 && m.live.RSS == 0 {
			continue
		}
		out = append(out, SiteUse{Domain: key, CPU: m.live.CPU, RSS: m.live.RSS})
	}
	g.res.mu.Unlock()
	slices.SortFunc(out, func(a, b SiteUse) int {
		return cmp.Or(cmp.Compare(b.CPU, a.CPU), cmp.Compare(b.RSS, a.RSS), strings.Compare(a.Domain, b.Domain))
	})
	return out
}
