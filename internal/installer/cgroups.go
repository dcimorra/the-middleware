package installer

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/dcimorra/the-middleware/internal/cgroups"
	"github.com/dcimorra/the-middleware/internal/fsx"
)

// The supervisor splits its unit's cgroup into the house, the agents and
// one group per site (internal/cgroups). Units rendered before that work
// was finished stand in its way in two ways: ProtectControlGroups=yes
// mounts /sys/fs/cgroup read-only for the whole unit, the delegated part
// included, and Delegate= does not name pids. An update replaces the
// binary and never the units, so, as for the jail, a drop-in is how an
// installed machine gets there from the panel's «Instalar requisitos»
// without a reinstall.

// CgroupsDropIn is the service's drop-in that hands it its cgroup.
const CgroupsDropIn = "middleware.service.d/cgroups.conf"

const cgroupsDropInContent = `# Rendered by middleware install. The supervisor splits its cgroup into
# the house, the agents and one group per site: it needs the cgroup
# delegated, with pids, and /sys/fs/cgroup writable, which
# ProtectControlGroups takes away.
[Service]
Delegate=cpu memory pids
DelegateSubgroup=house
ProtectControlGroups=no
`

// unitNeedsCgroups says whether an installed unit keeps the supervisor
// from its cgroup.
func unitNeedsCgroups(unit string) bool {
	return strings.Contains(unit, "\nProtectControlGroups=yes") || !strings.Contains(unit, "\nDelegate=cpu memory pids")
}

// stepCgroups writes the drop-in for an old unit. It says it changed
// something when the supervisor must restart to see it.
func (in *Installer) stepCgroups() (bool, string, error) {
	unit, err := os.ReadFile(in.pathFor(ServiceUnit))
	if err != nil || !unitNeedsCgroups(string(unit)) {
		return false, "ready", nil
	}
	p := filepath.Join(in.Paths.SystemdDir, CgroupsDropIn)
	if cur, err := os.ReadFile(p); err == nil && string(cur) == cgroupsDropInContent {
		return false, "ready", nil
	}
	if err := in.Exec.WriteRoot(p, []byte(cgroupsDropInContent), 0o644); err != nil {
		return false, "", err
	}
	if _, err := in.Exec.Sudo("systemctl", "daemon-reload"); err != nil {
		return true, "", err
	}
	return true, "service may split its cgroup", nil
}

// checkCgroups says whether the running supervisor did split its cgroup,
// which is what the installer's step cannot: the step writes the unit,
// and only the supervisor started after it makes sites/. A machine where
// «Instalar requisitos» ended well and nobody restarted the house looks
// ready to every other check and still runs every site in house/.
func (in *Installer) checkCgroups() Check {
	const name = "cgroups"
	if _, err := os.Stat(filepath.Join(cgroups.Root, "cgroup.controllers")); err != nil {
		return Check{name, true, "no unified cgroup hierarchy here: sites share the machine"}
	}
	cg, err := in.Exec.Run("systemctl", "show", "-p", "ControlGroup", "--value", ServiceUnit)
	cg = strings.TrimSpace(cg)
	if err != nil || cg == "" {
		return Check{name, true, "the service is not running under systemd: nothing to split"}
	}
	if _, err := os.Stat(filepath.Join(cgroups.Root, cg, cgroups.SitesName)); err == nil {
		return Check{name, true, "agents and each site have their own group"}
	}
	if unit, err := os.ReadFile(in.pathFor(ServiceUnit)); err == nil && unitNeedsCgroups(string(unit)) && !fsx.Exists(filepath.Join(in.Paths.SystemdDir, CgroupsDropIn)) {
		return Check{name, false, "the service unit keeps the supervisor from its cgroup: run `middleware install` again (or Install requirements on the panel), then restart the house"}
	}
	return Check{name, false, "the unit allows it but the supervisor has not split its cgroup: restart the house"}
}
