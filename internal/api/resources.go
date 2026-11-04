package api

import (
	"net/http"

	"github.com/dcimorra/the-middleware/internal/cgroups"
	"github.com/dcimorra/the-middleware/internal/manifest"
)

// A site's resources as the panel's «Recursos» card draws them: the
// machine it can share (the scale of each bar), what its manifest asks,
// what the house gives a site that asks nothing, and what its group holds
// now, so a ceiling is never set blind under what the site already uses.

// SiteResources is what GET /sites/{domain}/limits answers.
type SiteResources struct {
	// Applied is false when this node does not split its cgroup: the
	// limits are kept in the manifest and wait for it.
	Applied  bool             `json:"applied"`
	Machine  ResourcesMachine `json:"machine"`
	Limits   *manifest.Limits `json:"limits"`
	Defaults *manifest.Limits `json:"defaults"`
	// Use is the group's: bytes now, the most it held, kills at the
	// ceiling, and CPU time in microseconds (a rate between two reads).
	Use *ResourcesUse `json:"use,omitempty"`
}

type ResourcesMachine struct {
	CPUs   int   `json:"cpus"`
	Memory int64 `json:"memory"`
}

type ResourcesUse struct {
	Memory   int64 `json:"memory"`
	Peak     int64 `json:"peak"`
	OOMKills int64 `json:"oom_kills"`
	CPUUsec  int64 `json:"cpu_usec"`
}

// RegisterSiteResources mounts it; applied says whether this node splits
// its cgroup now, and use reads a site's group (false when it has none).
func (s *Server) RegisterSiteResources(machine cgroups.Machine, applied func() bool, use func(domain string) (cgroups.SiteMemory, bool)) {
	s.Handle("GET", Prefix+"/sites/{domain}/limits", "The site's resource limits on one page: the machine's cores and memory, the manifest's limits, the house's defaults, and what the site's group holds and has used now", func(w http.ResponseWriter, r *http.Request) (any, error) {
		m, err := s.sitesManager()
		if err != nil {
			return nil, err
		}
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		site, err := m.Get(domain)
		if err != nil {
			return nil, Errorf(http.StatusNotFound, "%v", err)
		}
		out := SiteResources{Machine: ResourcesMachine{CPUs: machine.CPUs, Memory: machine.Memory}}
		if site.Manifest != nil {
			out.Limits = site.Manifest.Limits
		}
		if st := s.opts.Settings; st != nil {
			out.Defaults = st().Sites.DefaultLimits
		}
		if applied() {
			out.Applied = true
			if u, ok := use(domain); ok {
				out.Use = &ResourcesUse{Memory: u.Current, Peak: u.Held, OOMKills: u.OOMKills, CPUUsec: u.CPUUsec}
			}
		}
		return out, nil
	})
}
