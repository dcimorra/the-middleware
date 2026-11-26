package guard

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/runtime"
)

func TestASiteBusyForMinutesIsAnEpisodeWithAStartAndAnEnd(t *testing.T) {
	fp := newFakeProc(t)
	h := newHouse(t)
	a := h.site("a.example", 100, 33001)
	spec := procSpec{pid: 100, ppid: 1, comm: "server", cmdline: []string{"./server"}, cwd: a.Dir, startSec: 10, listen: []int{33001}}
	fp.add(spec)
	now := time.Unix(fp.bootUnix+600, 0)
	kills := &killLog{}
	g, bus := newGuard(t, fp, h, kills, now)
	g.now = func() time.Time { return now }
	ticks := int64(0)
	minute := func(cpuPercent int64) {
		ticks += cpuPercent * 60 // 100 ticks a second is one core
		fp.setUsage(spec, ticks, 1000)
		now = now.Add(time.Minute)
		g.Sweep()
	}
	g.Sweep()
	for i := 0; i < 7; i++ {
		minute(100)
	}
	r := g.Resources("a.example")
	if len(r.Samples) != 7 || r.Samples[0].CPU != 100 || r.Samples[0].RSS == 0 {
		t.Fatalf("samples %+v", r.Samples)
	}
	if len(r.Episodes) != 1 || !r.Episodes[0].End.IsZero() || r.Episodes[0].Kind != "cpu" {
		t.Fatalf("an open episode: %+v", r.Episodes)
	}
	for i := 0; i < 6; i++ {
		minute(5)
	}
	r = g.Resources("a.example")
	if len(r.Episodes) != 1 || r.Episodes[0].End.IsZero() || r.Episodes[0].End.Sub(r.Episodes[0].Start) < 5*time.Minute {
		t.Fatalf("a closed episode: %+v", r.Episodes)
	}
	starts, ends := 0, 0
	for _, e := range bus.Recent() {
		switch e.Type {
		case "resources.episode":
			starts++
		case "resources.episode_end":
			ends++
		}
	}
	if starts != 1 || ends != 1 {
		t.Fatalf("events: %d starts, %d ends", starts, ends)
	}
	// It survives a restart of the guard.
	g2, _ := newGuard(t, fp, h, kills, now)
	if len(g2.Resources("a.example").Episodes) != 1 {
		t.Fatal("the history was not kept on disk")
	}
}

// A worker in a container is measured, and apart: its processes are found
// by the container's id in their cgroup, since where a database works is
// its data, not the site's folder, and a rootful daemon's processes do not
// even let their working directory be read. The site's history is the site
// and its workers together, with each worker's last sample beside it.
func TestAContainerWorkerIsMeasuredByItsCgroupAndApart(t *testing.T) {
	fp := newFakeProc(t)
	h := newHouse(t)
	a := h.site("a.example", 100, 33001)
	const id = "4f1c0e9a7b2d"
	if err := fsx.WriteJSON(filepath.Join(h.root, "data", "state", "a.example@db.json"),
		runtime.State{Domain: "a.example/db", Runtime: "docker", Port: 5432, Container: "mw-a-example-db-5432", ContainerID: id}, 0o644); err != nil {
		t.Fatal(err)
	}
	web := procSpec{pid: 100, ppid: 1, comm: "server", cmdline: []string{"./server"}, cwd: a.Dir, startSec: 10, listen: []int{33001}}
	db := procSpec{pid: 200, ppid: 1, comm: "postgres", cmdline: []string{"postgres"}, startSec: 10,
		cgroup: "0::/system.slice/docker-" + id + "aaaa.scope\n"}
	other := procSpec{pid: 300, ppid: 1, comm: "postgres", cmdline: []string{"postgres"}, startSec: 10,
		cgroup: "0::/system.slice/docker-ffffffffffff.scope\n"}
	fp.add(web)
	fp.add(db)
	fp.add(other)
	now := time.Unix(fp.bootUnix+600, 0)
	g, _ := newGuard(t, fp, h, &killLog{}, now)
	g.now = func() time.Time { return now }
	g.Sweep()
	fp.setUsage(web, 60*10, 1000) // 10% of a core over the minute
	fp.setUsage(db, 60*30, 3000)  // 30%
	fp.setUsage(other, 60*90, 9000)
	now = now.Add(time.Minute)
	g.Sweep()

	w := g.Resources("a.example/db")
	if len(w.Samples) != 1 || w.Samples[0].CPU != 30 {
		t.Fatalf("the worker: %+v", w.Samples)
	}
	all := g.Resources("a.example")
	if len(all.Samples) != 1 || all.Samples[0].CPU != 40 {
		t.Fatalf("the site and its worker together: %+v (another container's process counted, or the worker not)", all.Samples)
	}
	if all.Workers["db"].CPU != 30 {
		t.Fatalf("the worker's last sample beside the site's: %+v", all.Workers)
	}
}

// The dashboard asks which site is behind a busy node: what every site
// takes now, over seconds rather than the minute's sample, hungriest
// first; a site that takes nothing is left out, and one that stopped
// being swept (it left the house) goes once its reading is stale.
func TestUsageIsWhatEachSiteTakesNowHungriestFirst(t *testing.T) {
	fp := newFakeProc(t)
	h := newHouse(t)
	a := h.site("a.example", 100, 33001)
	b := h.site("b.example", 200, 33002)
	h.site("idle.example", 300, 33003)
	pa := procSpec{pid: 100, ppid: 1, comm: "server", cmdline: []string{"./server"}, cwd: a.Dir, startSec: 10, listen: []int{33001}}
	pb := procSpec{pid: 200, ppid: 1, comm: "server", cmdline: []string{"./server"}, cwd: b.Dir, startSec: 10, listen: []int{33002}}
	fp.add(pa)
	fp.add(pb)
	now := time.Unix(fp.bootUnix+600, 0)
	g, _ := newGuard(t, fp, h, &killLog{}, now)
	g.now = func() time.Time { return now }
	g.Sweep()
	if u := g.Usage(); len(u) != 0 {
		t.Fatalf("a reading before any window has passed: %+v", u)
	}
	fp.setUsage(pa, 10*25, 1000)  // a quarter of a core over ten seconds
	fp.setUsage(pb, 10*150, 2000) // a core and a half
	now = now.Add(10 * time.Second)
	g.Sweep()
	u := g.Usage()
	if len(u) != 2 || u[0].Domain != "b.example" || u[0].CPU != 150 || u[1].Domain != "a.example" || u[1].CPU != 25 {
		t.Fatalf("usage: %+v", u)
	}
	if u[0].RSS == 0 {
		t.Fatalf("no memory: %+v", u[0])
	}
	if r := g.Resources("b.example"); len(r.Samples) != 0 {
		t.Fatalf("the reading of now wrote a minute sample: %+v", r.Samples)
	}
	// Nothing swept for more than a minute: nothing is "now" any more.
	now = now.Add(2 * time.Minute)
	if u := g.Usage(); len(u) != 0 {
		t.Fatalf("stale readings: %+v", u)
	}
}
