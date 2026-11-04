// Package settings is data/settings.json: what this installation decided
// that is not about one site. It is small on purpose. Anything about a site
// goes in its manifest; anything about a module goes in the module store.
package settings

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/dcimorra/the-middleware/internal/cgroups"
	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/manifest"
)

// Format is the file's shape. A file without it was written before
// max_concurrent could mean "computed": there 8 was the default written on
// every save, and is read as computed (see Load).
const Format = 2

// Settings is the file.
type Settings struct {
	// Format is the shape the file was last written in.
	Format int `json:"format,omitempty"`
	// PanelDomain answers the panel. Empty means loopback only.
	PanelDomain string `json:"panel_domain,omitempty"`
	// Listen is the loopback address the tunnel door pushes to.
	Listen string `json:"listen,omitempty"`
	// Ports is the site port range.
	Ports struct {
		From int `json:"from,omitempty"`
		To   int `json:"to,omitempty"`
	} `json:"ports"`
	// VisitorTheme is the design of the visitor pages that belong to no
	// site, and of sites that did not choose one (see internal/pages).
	// It replaces a "pages" text override that nothing ever read.
	VisitorTheme string `json:"visitor_theme,omitempty"`
	// Agents caps how many conversations run at once.
	Agents Agents `json:"agents"`
	// Deps is how the external CLIs are kept.
	Deps Deps `json:"deps"`
	// Autofix hands a site that went down to its agent (internal/autofix).
	Autofix Autofix `json:"autofix"`
	// Automation is the house's AI for what nobody asked for at that
	// moment: repairing a site that arrived from a move and does not start,
	// the automatic repairs. Apart from the house's conversation, whose own
	// picker says whom the operator talks to there.
	Automation Automation `json:"automation"`
	// Cache is the house's response cache budget.
	Cache Cache `json:"cache"`
	// Update is whether this program updates itself (internal/update).
	Update Update `json:"update"`
	// Sites is what every site gets that its manifest does not say.
	Sites Sites `json:"sites"`
}

// Sites holds the house-wide defaults of the sites' budgets.
type Sites struct {
	// DefaultLimits is every site's cgroup budget where its manifest's
	// limits block leaves a field out. Empty by default: a memory ceiling
	// guessed for everyone would kill some working site on the upgrade
	// that brought it, so the operator sets one knowing the sites.
	DefaultLimits *manifest.Limits `json:"default_limits,omitempty"`
}

// Validate holds the defaults to a manifest's rules.
func (s Sites) Validate() error { return s.DefaultLimits.Validate("sites.default_limits") }

// Update is the self-update. The newest release is looked for every
// EveryMinutes (an hour unless set); one found is installed by itself only
// with Auto on, off by default, and otherwise waits for the button.
type Update struct {
	Auto         bool `json:"auto,omitempty"`
	EveryMinutes int  `json:"every_minutes,omitempty"`
}

// DefaultUpdateMinutes is how often a new release is looked for.
const DefaultUpdateMinutes = 60

// Every is the interval between checks, with its default.
func (u Update) Every() time.Duration {
	if u.EveryMinutes <= 0 {
		return DefaultUpdateMinutes * time.Minute
	}
	return time.Duration(u.EveryMinutes) * time.Minute
}

// Validate refuses an interval that is not a whole number of minutes from
// one minute to a week.
func (u Update) Validate() error {
	if u.EveryMinutes < 0 || u.EveryMinutes > 7*24*60 {
		return fmt.Errorf("update.every_minutes: want 1 to %d minutes", 7*24*60)
	}
	return nil
}

// Cache is the memory every site's response cache shares. The sum is what
// leaves a machine without memory, not one site, so the house holds the
// budget and takes from the largest when it is over.
type Cache struct {
	MaxMB int `json:"max_mb,omitempty"`
}

// Bytes is the budget with its default, 256 MB.
func (c Cache) Bytes() int64 {
	if c.MaxMB <= 0 {
		return 256 << 20
	}
	return int64(c.MaxMB) << 20
}

// Autofix is the automatic repair's switch and brakes. Off by default:
// it acts while the operator sleeps, so the brakes are the important part.
type Autofix struct {
	Enabled bool `json:"enabled,omitempty"`
	// Attempts per fault, then it stops and leaves it to the operator.
	Attempts int `json:"attempts,omitempty"`
	// WaitMinutes between attempts on one site.
	WaitMinutes int `json:"wait_minutes,omitempty"`
	// DailyUSD caps what all repairs together may spend in a day.
	DailyUSD float64 `json:"daily_usd,omitempty"`
}

// Automation is the house's AI for automatic work: an agent provider
// ("claude") and its model ("opus"). Empty is none, and what would use it
// does without.
type Automation struct {
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
}

// On says one is configured.
func (a Automation) On() bool { return a.Provider != "" }

// Limits fills the defaults: 2 attempts, 30 minutes, 2 USD a day.
func (a Autofix) Limits() Autofix {
	if a.Attempts <= 0 {
		a.Attempts = 2
	}
	if a.WaitMinutes <= 0 {
		a.WaitMinutes = 30
	}
	if a.DailyUSD <= 0 {
		a.DailyUSD = 2
	}
	return a
}

// Deps is the section about the programs the modules run (the agent
// CLIs). They come from npm and have release channels of their own, so
// the installation checks for a newer one daily and, by default,
// installs it: a CLI a year behind is a chat that stops working one day
// with nobody having changed anything here.
type Deps struct {
	// AutoUpdate installs a newer release when the daily check finds
	// one. Off, the check only reports it in the panel.
	AutoUpdate *bool `json:"auto_update,omitempty"`
}

// AutoUpdateOn reports the setting with its default (on).
func (d Deps) AutoUpdateOn() bool { return d.AutoUpdate == nil || *d.AutoUpdate }

// Agents is the concurrency section. Every agent turn is a CLI process
// with a model behind it, and a machine that serves forty sites cannot
// run forty of them at once; the cap is house-wide, with a few slots kept
// for the installation's own conversation so the operator can still ask
// the house what is going on when every site is busy.
type Agents struct {
	// MaxConcurrent is the number of turns running at once, all
	// conversations together. 1..64, or 0 for computed from the machine
	// (MaxConcurrentFor).
	MaxConcurrent int `json:"max_concurrent"`
	// ReservedForHouse is how many of those only the middleware
	// conversation may take. Less than MaxConcurrent.
	ReservedForHouse int `json:"reserved_for_house"`
	// PerProvider caps one provider's share; 0 means no cap of its own.
	PerProvider map[string]int `json:"per_provider"`
	// DefaultProvider runs a conversation whose manifest names none.
	DefaultProvider string `json:"default_provider"`
	// SiteDailyUSD is what a site may spend through the site door in a
	// day when its manifest says nothing (claude_api.daily_usd).
	SiteDailyUSD float64 `json:"site_daily_usd,omitempty"`
	// CPUCores and MemoryMax replace the agents' cgroup ceiling computed
	// from the machine (cgroups.Machine.DefaultLimits); zero and empty
	// are computed. MemoryMax is a size as docker writes it ("8g").
	CPUCores  int    `json:"cpu_cores,omitempty"`
	MemoryMax string `json:"memory_max,omitempty"`
}

// SiteDaily is SiteDailyUSD with its default.
func (a Agents) SiteDaily() float64 {
	if a.SiteDailyUSD > 0 {
		return a.SiteDailyUSD
	}
	return 5
}

// MaxAgents is the ceiling of MaxConcurrent.
const MaxAgents = 64

// TurnMemory is what one agent turn is counted as holding when the number
// of turns is computed: a CLI, its model's context and, often, a browser.
const TurnMemory = 3 << 29 // 1.5 GiB

// MaxConcurrentFor is the computed cap: the agents' memory ceiling over
// TurnMemory, from 2 to 64. It was 8 everywhere: too many turns for a
// 4 GB VPS, whose agents' ceiling then killed them one after another,
// and too few for 64 GB. Unknown memory keeps the old 8.
func MaxConcurrentFor(agentsMemory int64) int {
	if agentsMemory <= 0 {
		return 8
	}
	n := int(agentsMemory / TurnMemory)
	return min(max(n, 2), MaxAgents)
}

// Machine is the machine the computed values are sized from; tests
// replace it.
var Machine = cgroups.ThisMachine

// Limits is the agents' cgroup budget on m: the computed one with the
// operator's numbers in its place. A lower memory ceiling brings the
// point where the kernel starts squeezing down with it (three quarters,
// the computed quarter-to-third ratio), or high would sit above max.
func (a Agents) Limits(m cgroups.Machine) cgroups.Limits {
	l := m.DefaultLimits()
	if a.CPUCores > 0 {
		l.CPUCores = a.CPUCores
		if m.CPUs > 0 && l.CPUCores > m.CPUs {
			l.CPUCores = m.CPUs
		}
	}
	if a.MemoryMax != "" {
		if v, err := cgroups.ParseBytes(a.MemoryMax); err == nil && v > 0 {
			l.MemoryMax = v
			l.MemoryHigh = v / 4 * 3
		}
	}
	return l
}

// Concurrency is the cap in force on m and whether it was computed.
func (a Agents) Concurrency(m cgroups.Machine) (int, bool) {
	if a.MaxConcurrent > 0 {
		return a.MaxConcurrent, false
	}
	return MaxConcurrentFor(a.Limits(m).MemoryMax), true
}

// Resolved is the section with the computed cap in place of a zero and
// the reserved slots kept under it: an operator's 4 reserved on a machine
// that computes 2 leaves one slot for everyone else rather than none.
func (a Agents) Resolved(m cgroups.Machine) Agents {
	a.MaxConcurrent, _ = a.Concurrency(m)
	if a.ReservedForHouse >= a.MaxConcurrent {
		a.ReservedForHouse = a.MaxConcurrent - 1
	}
	if a.ReservedForHouse < 0 {
		a.ReservedForHouse = 0
	}
	return a
}

// DefaultAgents is the section a fresh installation runs with: the cap
// computed from the machine.
func DefaultAgents() Agents {
	return Agents{MaxConcurrent: 0, ReservedForHouse: 1, PerProvider: map[string]int{"claude": 0, "agy": 0, "opencode": 0}, DefaultProvider: "claude"}
}

// Validate says what is wrong with the section, if anything, on this
// machine.
func (a Agents) Validate() error { return a.ValidateOn(Machine()) }

// ValidateOn is Validate on m: with the cap computed, the reserved slots
// are held to the number it computes there.
func (a Agents) ValidateOn(m cgroups.Machine) error {
	if a.MaxConcurrent < 0 || a.MaxConcurrent > MaxAgents {
		return fmt.Errorf("agents.max_concurrent: want 1..%d, or 0 for computed", MaxAgents)
	}
	if n, computed := a.Concurrency(m); a.ReservedForHouse < 0 || a.ReservedForHouse >= n {
		if computed {
			return fmt.Errorf("agents.reserved_for_house: want 0 <= reserved < %d (max_concurrent computed on this machine)", n)
		}
		return errors.New("agents.reserved_for_house: want 0 <= reserved < max_concurrent")
	}
	for k, v := range a.PerProvider {
		if v < 0 || v > MaxAgents {
			return fmt.Errorf("agents.per_provider.%s: want 0..%d", k, MaxAgents)
		}
	}
	if a.DefaultProvider == "" {
		return errors.New("agents.default_provider: empty")
	}
	if a.CPUCores < 0 {
		return errors.New("agents.cpu_cores: cannot be negative (0 is computed)")
	}
	if a.MemoryMax != "" {
		if v, err := cgroups.ParseBytes(a.MemoryMax); err != nil {
			return fmt.Errorf("agents.memory_max: %v", err)
		} else if v < 256<<20 {
			return errors.New("agents.memory_max: want at least 256m (empty is computed)")
		}
	}
	return nil
}

// Defaults are what a fresh installation runs with.
func Defaults() Settings {
	var s Settings
	s.Listen = "127.0.0.1:46100"
	s.Ports.From, s.Ports.To = 33000, 47000
	s.Agents = DefaultAgents()
	return s
}

// Path is where the file lives under a root.
func Path(root string) string { return filepath.Join(root, "data", "settings.json") }

// Load reads the file, returning defaults when it does not exist yet.
func Load(root string) (Settings, error) {
	s := Defaults()
	err := fsx.ReadJSON(Path(root), &s)
	if err != nil && !os.IsNotExist(err) {
		return s, err
	}
	s.migrate()
	s.Fill()
	return s, nil
}

// migrate brings a file written before Format up to it. Until then every
// save wrote max_concurrent, so 8 there is the old default that nobody
// chose, and becomes computed; any other number was the operator's and
// stays. An operator who had typed 8 on purpose gets the computed number:
// the file cannot tell the two apart, and the panel shows which applies.
func (s *Settings) migrate() {
	if s.Format < 2 && s.Agents.MaxConcurrent == 8 {
		s.Agents.MaxConcurrent = 0
	}
	s.Format = Format
}

// Fill puts the defaults where a field was left empty: a file from before
// a section existed, or a PUT from a panel that does not know it yet,
// must not turn into "no agents may run".
func (s *Settings) Fill() {
	if s.Listen == "" {
		s.Listen = "127.0.0.1:46100"
	}
	if s.Ports.From == 0 || s.Ports.To == 0 {
		s.Ports.From, s.Ports.To = 33000, 47000
	}
	d := DefaultAgents()
	if s.Agents.MaxConcurrent == 0 && s.Agents.DefaultProvider == "" && s.Agents.PerProvider == nil {
		s.Agents = d
		return
	}
	if s.Agents.DefaultProvider == "" {
		s.Agents.DefaultProvider = d.DefaultProvider
	}
	if s.Agents.PerProvider == nil {
		s.Agents.PerProvider = d.PerProvider
	}
}

// ValidListen refuses a listen address the next start could not bind: it
// was any string, "127.0.0.1" without a port made net.Listen fail at the
// restart, and systemd brought the supervisor back to fail again, with
// every site behind it. The port must be a number: a service name would
// parse, and it would mean another port on another machine's
// /etc/services. Zero is refused too, since the kernel would pick one
// that nothing knows to dial.
func ValidListen(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("listen: %q is not host:port", addr)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("listen: want a port from 1 to 65535, not %q", port)
	}
	return nil
}

// Save writes the file atomically.
func Save(root string, s Settings) error {
	// Whatever is saved now was read or sent in this shape: an 8 from
	// here on is a choice.
	s.Format = Format
	return fsx.WriteJSON(Path(root), s, 0o644)
}

// Root resolves the installation root: the flag, then MIDDLEWARE_ROOT, then
// the current directory if it already holds a sites/ folder, then the
// system default.
func Root(flag string) string {
	if flag != "" {
		return flag
	}
	if v := os.Getenv("MIDDLEWARE_ROOT"); v != "" {
		return v
	}
	if wd, err := os.Getwd(); err == nil && fsx.Exists(filepath.Join(wd, "sites")) {
		return wd
	}
	return "/srv/middleware"
}
