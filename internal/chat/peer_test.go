package chat

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// SiteOfProcess finds a site's turn above a process, by the running turns
// or by the mark, and a forged mark can neither lift it nor point it at
// another site.
func TestASiteTurnIsFoundAboveAProcess(t *testing.T) {
	start := func(env ...string) int {
		t.Helper()
		cmd := exec.Command("sleep", "60")
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, MarkVar+"=") {
				cmd.Env = append(cmd.Env, kv)
			}
		}
		cmd.Env = append(cmd.Env, env...)
		cmd.Env = append(cmd.Env, "MW_EXECED=1")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
		// Start returns as the exec begins, and for a moment after it /proc
		// still shows the environment from before it.
		pid := cmd.Process.Pid
		for i := 0; i < 400; i++ {
			if env, _ := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid)); strings.Contains(string(env), "MW_EXECED=1") {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		return pid
	}
	self := os.Getpid()
	cases := []struct {
		name  string
		mark  string
		turns map[int]string
		want  string
		fails bool
	}{
		{name: "nothing", want: ""},
		{name: "the turn above", turns: map[int]string{self: "a.test"}, want: "a.test"},
		{name: "a door conversation's turn", turns: map[int]string{self: "a.test#00ff"}, want: "a.test"},
		{name: "the mark alone", mark: "a.test", want: "a.test"},
		{name: "a house mark under a site's turn", mark: Middleware, turns: map[int]string{self: "a.test"}, want: "a.test"},
		{name: "another site's mark under a site's turn", mark: "b.test", turns: map[int]string{self: "a.test"}, fails: true},
		{name: "the house's turn", turns: map[int]string{self: Middleware}, want: ""},
		{name: "a mark that names no site", mark: "../etc", want: ""},
		// The autonomous agent is a caller of its own, never the operator's,
		// and a site's turn cannot pass itself off as it, nor it as one.
		{name: "the autonomous agent's turn", turns: map[int]string{self: Autopilot + "#00ff"}, want: Autopilot},
		{name: "the autonomous agent's mark alone", mark: Autopilot + "#00ff", want: Autopilot},
		{name: "its mark under a site's turn", mark: Autopilot, turns: map[int]string{self: "a.test"}, fails: true},
		{name: "a house mark under its turn", mark: Middleware, turns: map[int]string{self: Autopilot + "#00ff"}, want: Autopilot},
	}
	for _, c := range cases {
		var env []string
		if c.mark != "" {
			env = append(env, MarkVar+"="+c.mark)
		}
		pid := start(env...)
		got, err := SiteOfProcess("/proc", pid, c.turns)
		if c.fails {
			if err == nil {
				t.Errorf("%s: %q, want an error", c.name, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s: %q %v, want %q", c.name, got, err, c.want)
		}
	}
	// A process gone before it was looked at cannot be told.
	gone := exec.Command("true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := SiteOfProcess("/proc", gone.Process.Pid, nil); err == nil {
		t.Fatal("a process that is gone was told apart")
	}
}
