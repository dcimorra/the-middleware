package cgroups

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// DefaultSitePids is a site group's process cap when nothing sets one,
// the number runtime.Exec already gives a build step.
const DefaultSitePids = 4096

// SiteLimits are one site's group. Zero is the default: weight 100 (an
// equal share against every other site, whatever the number of processes
// each runs), DefaultSitePids, and no CPU or memory ceiling.
type SiteLimits struct {
	MemoryMax  int64
	MemoryHigh int64
	// CPUs is a hard cap in cores (1.5 is a core and a half).
	CPUs float64
	// CPUWeight is the cgroup scale, 1..10000 with 100 an equal share;
	// not docker's 1024.
	CPUWeight int
	Pids      int
}

// Sites says whether sites get groups of their own.
func (t *Tree) Sites() bool { return t != nil && t.sites != "" }

// Notes are what Setup could not do in full, to be said once at start.
func (t *Tree) Notes() []string {
	if t == nil {
		return nil
	}
	return t.notes
}

// siteName is the group's name for a spec's domain: a worker's or a
// service's ("example.com/queue") counts against its site.
func siteName(domain string) (string, error) {
	site, _, _ := strings.Cut(domain, "/")
	if site == "" || site == "." || site == ".." || strings.ContainsAny(site, "\x00\\") {
		return "", fmt.Errorf("cgroups: %q is not a site", domain)
	}
	return site, nil
}

// group makes the site's group if it is not there, with its limits.
func (t *Tree) group(site string) (string, error) {
	dir := filepath.Join(t.sites, site)
	if _, err := os.Stat(dir); err == nil {
		return dir, nil
	}
	if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("cgroups: %w", err)
	}
	t.mu.Lock()
	l := t.desired[site]
	t.mu.Unlock()
	if err := t.write(dir, l); err != nil {
		return "", err
	}
	return dir, nil
}

// SetSite writes a site's limits, live: a cgroup takes new numbers while
// its processes run. Its group is made if it is not there yet, so a site
// starts inside its limits rather than under the defaults for a moment.
func (t *Tree) SetSite(domain string, l SiteLimits) error {
	if !t.Sites() {
		return nil
	}
	site, err := siteName(domain)
	if err != nil {
		return err
	}
	t.mu.Lock()
	t.desired[site] = l
	t.mu.Unlock()
	dir, err := t.group(site)
	if err != nil {
		return err
	}
	return t.write(dir, l)
}

func (t *Tree) write(dir string, l SiteLimits) error {
	weight := l.CPUWeight
	if weight <= 0 {
		weight = 100
	}
	cpuMax := "max 100000"
	if l.CPUs > 0 {
		q := int64(l.CPUs * 100000)
		if q < 1000 {
			q = 1000 // the kernel's floor is 1 ms a period
		}
		cpuMax = strconv.FormatInt(q, 10) + " 100000"
	}
	high, max := "max", "max"
	if l.MemoryHigh > 0 {
		high = strconv.FormatInt(l.MemoryHigh, 10)
	}
	if l.MemoryMax > 0 {
		max = strconv.FormatInt(l.MemoryMax, 10)
	}
	writes := [][2]string{{"cpu.weight", strconv.Itoa(weight)}, {"cpu.max", cpuMax}, {"memory.high", high}, {"memory.max", max}}
	if t.pids {
		p := l.Pids
		if p <= 0 {
			p = DefaultSitePids
		}
		writes = append(writes, [2]string{"pids.max", strconv.Itoa(p)})
	}
	for _, w := range writes {
		if err := os.WriteFile(filepath.Join(dir, w[0]), []byte(w[1]), 0o644); err != nil {
			return fmt.Errorf("cgroups: %s/%s: %w", filepath.Base(dir), w[0], err)
		}
	}
	// A ceiling is a ceiling: with swap the kernel would page the site
	// out at memory.max instead of stopping it, and a site past its
	// budget would crawl, slowing the disk for everyone, rather than be
	// killed and said (site.oom). Without a ceiling the site swaps as
	// the machine does. A kernel without swap accounting has no file.
	swap := "max"
	if l.MemoryMax > 0 {
		swap = "0"
	}
	if f := filepath.Join(dir, "memory.swap.max"); t.real(dir) {
		if _, err := os.Stat(f); err == nil {
			if err := os.WriteFile(f, []byte(swap), 0o644); err != nil {
				return fmt.Errorf("cgroups: %s/memory.swap.max: %w", filepath.Base(dir), err)
			}
		}
	} else {
		_ = os.WriteFile(f, []byte(swap), 0o644)
	}
	return nil
}

// real says dir is a cgroup the kernel made (it has cgroup.procs), not a
// test's folder, where files are written as they come.
func (t *Tree) real(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "cgroup.procs"))
	return err == nil
}

// SiteAttr is a SysProcAttr that starts a process inside its site's
// group (clone into the cgroup, as agent turns start in agents/), or nil
// without a sites' branch. The caller owns the descriptor until Start
// returns.
func (t *Tree) SiteAttr(domain string) (*syscall.SysProcAttr, func(), error) {
	if !t.Sites() {
		return nil, func() {}, nil
	}
	site, err := siteName(domain)
	if err != nil {
		return nil, func() {}, err
	}
	dir, err := t.group(site)
	if err != nil {
		return nil, func() {}, err
	}
	fd, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, func() {}, err
	}
	return &syscall.SysProcAttr{Setpgid: true, UseCgroupFD: true, CgroupFD: fd}, func() { syscall.Close(fd) }, nil
}

// SitePath is a site's group, "" without one.
func (t *Tree) SitePath(domain string) string {
	if !t.Sites() {
		return ""
	}
	site, err := siteName(domain)
	if err != nil {
		return ""
	}
	return filepath.Join(t.sites, site)
}

// procLinks is a process's parent and process group; tests replace it.
var procLinks = func(pid int) (ppid, pgid int, ok bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, false
	}
	i := strings.LastIndexByte(string(data), ')')
	if i < 0 {
		return 0, 0, false
	}
	f := strings.Fields(string(data[i+1:]))
	if len(f) < 3 {
		return 0, 0, false
	}
	ppid, err1 := strconv.Atoi(f[1])
	pgid, err2 := strconv.Atoi(f[2])
	return ppid, pgid, err1 == nil && err2 == nil
}

// AdoptSite moves a re-adopted instance and everything it started from
// house/ into its site's group: a supervisor from before sites had groups
// started them as its children, in house/, and KillMode=process kept them
// there across the restart. What moves is the leader, its descendants and
// its process group (a jail's command makes a group of its own under the
// jail's init, a descendant). Passes until nothing new is found, since a
// process may fork while the others move, as moveIntoHouse does. A process
// already elsewhere (its site's group, from this version) is not looked at.
func (t *Tree) AdoptSite(domain string, leader int) error {
	if !t.Sites() || leader <= 0 {
		return nil
	}
	site, err := siteName(domain)
	if err != nil {
		return err
	}
	dir, err := t.group(site)
	if err != nil {
		return err
	}
	ours := map[int]bool{leader: true}
	for pass := 0; pass < 5; pass++ {
		raw, err := os.ReadFile(filepath.Join(t.house, "cgroup.procs"))
		if err != nil {
			return fmt.Errorf("cgroups: %w", err)
		}
		links := map[int][2]int{}
		var pids []int
		for _, f := range strings.Fields(string(raw)) {
			pid, err := strconv.Atoi(f)
			if err != nil {
				continue
			}
			if ppid, pgid, ok := procLinks(pid); ok {
				links[pid] = [2]int{ppid, pgid}
				pids = append(pids, pid)
			}
		}
		// The closure over parents: a child listed before its parent is
		// found on the next round of this loop, not the next pass.
		for grew := true; grew; {
			grew = false
			for _, pid := range pids {
				l := links[pid]
				if !ours[pid] && (ours[l[0]] || l[1] == leader) {
					ours[pid], grew = true, true
				}
			}
		}
		moved := 0
		for _, pid := range pids {
			if !ours[pid] {
				continue
			}
			if err := movePID(dir, strconv.Itoa(pid)); err != nil && !errors.Is(err, syscall.ESRCH) {
				return fmt.Errorf("cgroups: moving %d into %s: %w", pid, dir, err)
			}
			moved++
		}
		if moved == 0 {
			return nil
		}
	}
	return fmt.Errorf("cgroups: %s's processes kept appearing in house/ after five passes", site)
}

// RemoveSite takes an empty site group away: the site was deleted or
// moved to another node. A group that still holds a process is left (the
// kernel refuses with EBUSY) and is not an error the caller can act on
// beyond trying again later.
func (t *Tree) RemoveSite(domain string) error {
	if !t.Sites() {
		return nil
	}
	site, err := siteName(domain)
	if err != nil {
		return err
	}
	t.mu.Lock()
	delete(t.desired, site)
	t.mu.Unlock()
	return removeGroup(filepath.Join(t.sites, site))
}

// removeGroup is rmdir on a cgroup, which the kernel allows with the
// interface files inside and refuses while a process is left; tests,
// whose folder holds those files as plain ones, replace it.
var removeGroup = func(dir string) error {
	err := os.Remove(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// SweepSites removes the groups of sites that no longer exist here, left
// by a deletion while the supervisor was down or a removal that found the
// group still busy.
func (t *Tree) SweepSites(exists func(site string) bool) {
	if !t.Sites() {
		return
	}
	entries, err := os.ReadDir(t.sites)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() && !exists(e.Name()) {
			_ = t.RemoveSite(e.Name())
		}
	}
}

// SiteMemory is what a site's group says of its memory: oom_kill from
// memory.events (how many processes the kernel killed inside it for
// going over memory.max, ever), the ceiling, and the most it held
// (memory.peak, or memory.current on a kernel without it).
type SiteMemory struct {
	OOMKills int64
	Max      int64 // 0 is no ceiling
	Held     int64
}

// Memory reads a site's group; false when it has none.
func (t *Tree) Memory(domain string) (SiteMemory, bool) {
	dir := t.SitePath(domain)
	if dir == "" {
		return SiteMemory{}, false
	}
	raw, err := os.ReadFile(filepath.Join(dir, "memory.events"))
	if err != nil {
		return SiteMemory{}, false
	}
	var m SiteMemory
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(line, "oom_kill "); ok {
			m.OOMKills, _ = strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		}
	}
	num := func(f string) int64 {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			return -1
		}
		n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		if err != nil {
			return 0 // "max"
		}
		return n
	}
	m.Max = max(num("memory.max"), 0)
	if m.Held = num("memory.peak"); m.Held < 0 {
		m.Held = max(num("memory.current"), 0)
	}
	return m, true
}

// SiteNames lists the groups there are, by site.
func (t *Tree) SiteNames() []string {
	if !t.Sites() {
		return nil
	}
	entries, err := os.ReadDir(t.sites)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}
