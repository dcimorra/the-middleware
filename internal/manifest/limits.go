package manifest

import (
	"fmt"

	"github.com/dcimorra/the-middleware/internal/cgroups"
)

// Limits are a site's budget: what all its process-runtime pieces (its
// instances, process workers and services, its tasks) may hold together,
// since they share one cgroup (internal/cgroups). A docker piece keeps
// docker.*: its container lives in the daemon's cgroup, not in the site's.
//
// During a handover the old and new instance run at once in that group,
// so Memory covers both: it is what the site may hold, whatever it holds
// it with. A new instance that cannot start inside it fails its health
// check and the old one keeps serving.
type Limits struct {
	// Memory is the hard ceiling (memory.max): past it the kernel kills
	// inside the site. MemoryHigh is where it starts squeezing without
	// killing. Sizes as docker.memory writes them ("1g", "768m").
	Memory     string `json:"memory,omitempty"`
	MemoryHigh string `json:"memory_high,omitempty"`
	// CPUs caps the site in cores (1.5).
	CPUs float64 `json:"cpus,omitempty"`
	// CPUWeight is the cgroup scale: 1..10000, 100 an equal share with
	// every other site. Not docker.cpu_weight's scale, where 1024 is.
	CPUWeight int `json:"cpu_weight,omitempty"`
	// Pids caps the processes (4096 when unset).
	Pids int `json:"pids,omitempty"`
	// Network is the site's transfer budget: not a cgroup's, the house
	// counts the bytes itself (internal/netuse).
	Network *NetworkLimits `json:"network,omitempty"`
}

// NetworkLimits are how much a site may move over the network, bytes in
// plus bytes out, and what the house does about it. It lives in limits
// because it is the same kind of promise as memory: what one site may
// take of something every site shares, the owner's alone to raise.
type NetworkLimits struct {
	// Monthly and Daily are budgets for the calendar month and the day,
	// sizes as memory writes them ("500g", "2t").
	Monthly string `json:"monthly,omitempty"`
	Daily   string `json:"daily,omitempty"`
	// WarnAt is the percent of a budget at which the house warns, 80
	// when unset.
	WarnAt int `json:"warn_at,omitempty"`
	// Cut closes the site when a budget is reached, until the day or the
	// month ends or the budget is raised. Without it the house only
	// says so: a site that goes dark is a decision, not a default.
	Cut bool `json:"cut,omitempty"`
}

// Bytes are the two budgets in bytes, zero where there is none. Sizes
// that do not parse (Validate refuses them first) are no budget.
func (n *NetworkLimits) Bytes() (monthly, daily int64) {
	if n == nil {
		return 0, 0
	}
	if n.Monthly != "" {
		monthly, _ = cgroups.ParseBytes(n.Monthly)
	}
	if n.Daily != "" {
		daily, _ = cgroups.ParseBytes(n.Daily)
	}
	return monthly, daily
}

func (n *NetworkLimits) validate(field string) error {
	if n == nil {
		return nil
	}
	var monthly, daily int64
	var err error
	if n.Monthly != "" {
		if monthly, err = cgroups.ParseBytes(n.Monthly); err != nil {
			return fmt.Errorf("%s.monthly: %v", field, err)
		}
	}
	if n.Daily != "" {
		if daily, err = cgroups.ParseBytes(n.Daily); err != nil {
			return fmt.Errorf("%s.daily: %v", field, err)
		}
	}
	if monthly > 0 && daily > monthly {
		return fmt.Errorf("%s.daily: %s is above monthly %s", field, n.Daily, n.Monthly)
	}
	if n.WarnAt < 0 || n.WarnAt > 99 {
		return fmt.Errorf("%s.warn_at: want a percent from 1 to 99 (80 when left out)", field)
	}
	if n.Cut && monthly == 0 && daily == 0 {
		return fmt.Errorf("%s.cut: there is no monthly or daily budget to cut at", field)
	}
	return nil
}

// Validate refuses what the kernel would refuse or what makes no sense:
// negatives, a size that does not parse, a weight off the scale, and a
// squeeze point above the ceiling. A nil block is no limits.
func (l *Limits) Validate(field string) error {
	if l == nil {
		return nil
	}
	if l.CPUs < 0 || l.CPUWeight < 0 || l.Pids < 0 {
		return fmt.Errorf("%s: cpus, cpu_weight and pids cannot be negative", field)
	}
	if l.CPUWeight > 10000 {
		return fmt.Errorf("%s.cpu_weight: want 1..10000 (100 is an equal share; docker's 1024 scale is docker.cpu_weight)", field)
	}
	var mem, high int64
	var err error
	if l.Memory != "" {
		if mem, err = cgroups.ParseBytes(l.Memory); err != nil {
			return fmt.Errorf("%s.memory: %v", field, err)
		}
	}
	if l.MemoryHigh != "" {
		if high, err = cgroups.ParseBytes(l.MemoryHigh); err != nil {
			return fmt.Errorf("%s.memory_high: %v", field, err)
		}
	}
	if mem > 0 && high > mem {
		return fmt.Errorf("%s.memory_high: %s is above memory %s", field, l.MemoryHigh, l.Memory)
	}
	return l.Network.validate(field + ".network")
}

// Over lays l over the house's defaults field by field: what the
// manifest says wins, what it leaves out comes from the settings.
func (l *Limits) Over(def *Limits) Limits {
	var out Limits
	if def != nil {
		out = *def
	}
	if l == nil {
		return out
	}
	if l.Memory != "" {
		out.Memory = l.Memory
		// A site's own ceiling under the house's squeeze point would
		// put high above max; the site's ceiling wins and the house's
		// squeeze point goes unless the site gave its own.
		if l.MemoryHigh == "" {
			out.MemoryHigh = ""
		}
	}
	if l.MemoryHigh != "" {
		out.MemoryHigh = l.MemoryHigh
	}
	if l.CPUs > 0 {
		out.CPUs = l.CPUs
	}
	if l.CPUWeight > 0 {
		out.CPUWeight = l.CPUWeight
	}
	if l.Pids > 0 {
		out.Pids = l.Pids
	}
	// The block goes whole: a site's own budget with the house's cut, or
	// the house's warning share over a budget it was not written for,
	// would be a rule nobody wrote.
	if l.Network != nil {
		out.Network = l.Network
	}
	return out
}

// Group is the block as the cgroup takes it. Sizes that do not parse
// (Validate refuses them first) are no limit.
func (l Limits) Group() cgroups.SiteLimits {
	g := cgroups.SiteLimits{CPUs: l.CPUs, CPUWeight: l.CPUWeight, Pids: l.Pids}
	if l.Memory != "" {
		g.MemoryMax, _ = cgroups.ParseBytes(l.Memory)
	}
	if l.MemoryHigh != "" {
		g.MemoryHigh, _ = cgroups.ParseBytes(l.MemoryHigh)
	}
	if g.MemoryMax > 0 && g.MemoryHigh > g.MemoryMax {
		g.MemoryHigh = 0
	}
	return g
}
