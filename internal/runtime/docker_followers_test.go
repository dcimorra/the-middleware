package runtime

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestReapFollowersKillsOnlyAnEarlierSupervisorsFollowers runs real
// processes under a fake docker: orphans (as the followers of a supervisor
// that stopped are) and a child of this process (as the new supervisor's
// own are), and reaps them through the real /proc. Each restart of the
// house used to leave a docker logs -f and a docker wait per container
// running for good; hundreds of them were found holding gigabytes.
func TestReapFollowersKillsOnlyAnEarlierSupervisorsFollowers(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("needs /proc")
	}
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nwhile :; do sleep 1; done\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var all []int
	t.Cleanup(func() {
		for _, p := range all {
			_ = syscall.Kill(p, syscall.SIGKILL)
		}
	})
	// orphan starts the fake docker in the background of a shell that
	// then exits, so its parent is no longer this process.
	orphan := func(args ...string) int {
		t.Helper()
		out, err := exec.Command("sh", append([]string{"-c", `"$0" "$@" </dev/null >/dev/null 2>&1 & echo $!`, docker}, args...)...).Output()
		if err != nil {
			t.Fatal(err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, pid)
		return pid
	}
	waitOld := orphan("wait", "mw-shop-example-33005")
	logsOld := orphan("logs", "-f", "--since", "1s", "mw-shop-example-33005")
	logsOther := orphan("logs", "-f", "someone-elses")
	notFollower := orphan("ps", "mw-shop-example-33005")
	own := exec.Command(docker, "wait", "mw-shop-example-33005")
	if err := own.Start(); err != nil {
		t.Fatal(err)
	}
	all = append(all, own.Process.Pid)
	defer func() { _ = own.Process.Kill(); _ = own.Wait() }()
	// The shell needs a moment to exec the script before its cmdline
	// says what it is.
	time.Sleep(300 * time.Millisecond)

	kill := func(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) }
	self := os.Getpid()
	mine := cgroupOf("/proc/self")

	if n := reapFollowers("/proc", self, "docker", "/elsewhere.slice/other.service", kill); n != 0 {
		t.Fatalf("followers in another cgroup were killed: %d", n)
	}
	if n := reapFollowers("/proc", self, "docker", mine, kill); n != 2 {
		t.Fatalf("killed %d, want the two orphaned followers", n)
	}
	for _, p := range []int{waitOld, logsOld} {
		if !gone(p, 5*time.Second) {
			t.Errorf("orphaned follower %d still running", p)
		}
	}
	for _, p := range []int{logsOther, notFollower, own.Process.Pid} {
		if gone(p, 500*time.Millisecond) {
			t.Errorf("process %d was killed and is not an earlier supervisor's follower", p)
		}
	}
}

// gone waits up to within for pid to be dead (a zombie counts: its parent,
// init here, reaps it when it gets to it).
func gone(pid int, within time.Duration) bool {
	for end := time.Now().Add(within); time.Now().Before(end); {
		raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			return true
		}
		s := string(raw)
		if f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:]); len(f) > 0 && f[0] == "Z" {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

func TestIsFollower(t *testing.T) {
	dir := t.TempDir()
	n := 0
	write := func(argv ...string) string {
		n++
		d := filepath.Join(dir, strconv.Itoa(n))
		_ = os.MkdirAll(d, 0o755)
		_ = os.WriteFile(filepath.Join(d, "cmdline"), []byte(strings.Join(argv, "\x00")+"\x00"), 0o644)
		return d
	}
	for _, c := range []struct {
		argv []string
		want bool
	}{
		{[]string{"docker", "wait", "mw-a-1"}, true},
		{[]string{"docker", "logs", "-f", "mw-a-1"}, true},
		{[]string{"/usr/bin/docker", "logs", "-f", "--since", "1s", "mw-a-1"}, true},
		{[]string{"/bin/sh", "/x/docker", "wait", "mw-a-1"}, true},
		{[]string{"docker", "logs", "mw-a-1"}, false},
		{[]string{"docker", "wait", "web"}, false},
		{[]string{"docker", "rm", "-f", "mw-a-1"}, false},
		{[]string{"podman", "wait", "mw-a-1"}, false},
	} {
		if got := isFollower(write(c.argv...), "docker"); got != c.want {
			t.Errorf("%q: %v, want %v", c.argv, got, c.want)
		}
	}
}
