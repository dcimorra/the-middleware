package runtime

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// ReapFollowers kills the `docker logs -f` and `docker wait` an earlier
// supervisor left behind, and returns how many it killed.
//
// Every container instance has two followers: watch (docker wait) and
// tailLogs (docker logs -f). They are plain children of the supervisor and
// the unit has KillMode=process, so when the supervisor stops they are not
// killed with it: the containers outlive it on purpose, and so did they.
// The next supervisor adopts the container and starts two new followers,
// and the old pair kept running, about 14 MB each, and the old logs -f
// kept appending to the site's log as well, so every line was written once
// per supervisor that had ever adopted it. On a machine that updated itself
// every day they reached hundreds of processes and gigabytes of memory.
//
// They are not killed with the supervisor instead (Pdeathsig) because that
// signal follows the thread that started them, not the process: a locked
// thread that ends would kill a follower of a healthy container, and a
// dead docker wait reads as a dead site, which the engine restarts.
//
// What is ours to kill is narrow: a process in this supervisor's cgroup
// (the unit's, or house/ inside it), whose command is the docker CLI
// following a container this house names (mw-...), and whose parent is
// not this process. A docker logs someone runs in a terminal is in their
// session's cgroup, and another installation's followers are in its own
// unit, so neither is touched.
func (d Docker) ReapFollowers() int {
	mine := cgroupOf("/proc/self")
	if mine == "" || mine == "/" {
		// Not knowing our own cgroup (or sharing the root one with
		// everything), there is no telling ours from anyone else's:
		// better a leak than killing a stranger's.
		return 0
	}
	return reapFollowers("/proc", os.Getpid(), filepath.Base(d.bin()), mine, func(pid int) error {
		return syscall.Kill(pid, syscall.SIGKILL)
	})
}

func reapFollowers(proc string, self int, bin, mine string, kill func(int) error) int {
	ours := map[string]bool{mine: true}
	if filepath.Base(mine) == "house" {
		// Before the cgroups setup moves everything into house/, an
		// earlier supervisor's followers can still be in the unit's own.
		ours[filepath.Dir(mine)] = true
	}
	entries, err := os.ReadDir(proc)
	if err != nil {
		return 0
	}
	killed := 0
	for _, ent := range entries {
		pid, err := strconv.Atoi(ent.Name())
		if err != nil || pid == self {
			continue
		}
		dir := filepath.Join(proc, ent.Name())
		if !isFollower(dir, bin) || parentOf(dir) == self || !ours[cgroupOf(dir)] {
			continue
		}
		if kill(pid) == nil {
			killed++
		}
	}
	return killed
}

// isFollower says whether the process in dir is the docker CLI running
// `wait` or `logs -f` on a container of this house. The binary may be
// argv[0] or, for a script, argv[1] after its interpreter.
func isFollower(dir, bin string) bool {
	raw, err := os.ReadFile(filepath.Join(dir, "cmdline"))
	if err != nil {
		return false
	}
	argv := strings.Split(string(bytes.TrimRight(raw, "\x00")), "\x00")
	for i := 0; i < len(argv) && i < 2; i++ {
		if filepath.Base(argv[i]) != bin {
			continue
		}
		rest := argv[i+1:]
		if len(rest) < 2 || !strings.HasPrefix(rest[len(rest)-1], "mw-") {
			return false
		}
		switch rest[0] {
		case "wait":
			return len(rest) == 2
		case "logs":
			return len(rest) >= 3 && rest[1] == "-f"
		}
		return false
	}
	return false
}

// cgroupOf is the process's cgroup v2 path, empty when it cannot be read.
func cgroupOf(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, "cgroup"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if p, ok := strings.CutPrefix(line, "0::"); ok {
			return strings.TrimSpace(p)
		}
	}
	return ""
}

// parentOf is the process's parent pid, -1 when it cannot be read.
func parentOf(dir string) int {
	raw, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return -1
	}
	// The command name in parentheses may hold spaces and parentheses
	// itself; what follows the last ")" is " state ppid ...".
	s := string(raw)
	f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
	if len(f) < 2 {
		return -1
	}
	ppid, err := strconv.Atoi(f[1])
	if err != nil {
		return -1
	}
	return ppid
}
