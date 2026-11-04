package installer

import (
	"os"
	"path/filepath"
	"strings"
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
