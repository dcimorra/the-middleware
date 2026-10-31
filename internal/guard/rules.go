package guard

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Levels a rule can report at.
const (
	LevelIntrusion  = "intrusion"
	LevelSuspicious = "suspicious"
)

// View is what a rule sees: one process, the site it belongs to and the
// rest of that site's processes.
type View struct {
	Site SiteInfo
	// SitesDir is the folder every site lives under.
	SitesDir string
	Proc     Process
	// Root is true for the instance the engine started; rules about the
	// whole site match on it so they fire once, not once per process.
	Root bool
	// Tree is every process attributed to the site, the root included.
	Tree []Process
	// PortRange is the range the engine assigns from; a port inside it
	// is the engine's business, a port outside it is the rule's.
	PortRange [2]int
}

// Rule is one thing the guard looks for. Rules are code, not config: an
// operator reads them in the panel and cannot be talked into disabling one
// by a prompt, which is the point of a watchdog.
type Rule struct {
	Key         string `json:"key"`
	Level       string `json:"level"`
	Description string `json:"description"`
	// Match reports where the rule matched, in words an operator can act on.
	Match func(v View) (where string, ok bool) `json:"-"`
}

// maxProcesses is where a site stops looking like a build and starts
// looking like a fork bomb.
const maxProcesses = 200

var rules = []Rule{
	{
		Key: "foreign-site", Level: LevelIntrusion,
		Description: "a site process works inside another site's folder",
		Match: func(v View) (string, bool) {
			if other := foreignSite(v, v.Proc.Cwd); other != "" {
				return "cwd " + v.Proc.Cwd + " belongs to " + other, true
			}
			for _, arg := range v.Proc.Cmdline {
				if other := foreignSite(v, arg); other != "" {
					return "argument " + arg + " names " + other, true
				}
			}
			return "", false
		},
	},
	{
		Key: "foreign-port", Level: LevelSuspicious,
		Description: "a site process listens on a port that was not assigned to it",
		Match: func(v View) (string, bool) {
			assigned := map[int]bool{}
			for _, p := range v.Site.Ports {
				assigned[p] = true
			}
			for _, port := range v.Proc.Listening {
				if assigned[port] || (port >= v.PortRange[0] && port <= v.PortRange[1] && v.PortRange[1] > 0) {
					continue
				}
				return "listening on " + strconv.Itoa(port), true
			}
			return "", false
		},
	},
	{
		Key: "tmp-binary", Level: LevelIntrusion,
		Description: "a site process runs a binary from a temporary folder",
		Match: func(v View) (string, bool) {
			own := filepath.Clean(v.Site.Dir) + string(filepath.Separator)
			for _, p := range []string{v.Proc.Exe, firstArg(v.Proc.Cmdline)} {
				// A binary inside the site folder is the site's own, wherever
				// the folder happens to be (tests build houses under /tmp).
				if v.Site.Dir != "" && strings.HasPrefix(p, own) {
					continue
				}
				// So is the house's own binary: a jail's init is it, until it
				// execs the command. From a house under /tmp (a test binary,
				// a build tried out) every jailed process was an intrusion
				// for its first instant, and the guard's tick that landed
				// there killed it: a site's task in the wired test, once in
				// a few CI runs.
				if p != "" && p == houseBinary {
					continue
				}
				for _, tmp := range []string{"/tmp/", "/var/tmp/", "/dev/shm/"} {
					if strings.HasPrefix(p, tmp) {
						return "binary " + p, true
					}
				}
			}
			return "", false
		},
	},
	{
		Key: "reverse-shell", Level: LevelIntrusion,
		Description: "a shell with a reverse-shell signature in its command line",
		Match: func(v View) (string, bool) {
			cmd := strings.Join(v.Proc.Cmdline, " ")
			for _, sig := range []string{"/dev/tcp/", "/dev/udp/", "nc -e", "ncat -e", "nc.traditional -e", "bash -i >&", "sh -i >&", "exec:/bin/sh", "exec:/bin/bash", "mkfifo /tmp"} {
				if strings.Contains(cmd, sig) {
					return "command line contains " + strconv.Quote(sig), true
				}
			}
			return "", false
		},
	},
	{
		Key: "miner", Level: LevelIntrusion,
		Description: "a process with a crypto-miner signature",
		Match: func(v View) (string, bool) {
			hay := strings.ToLower(v.Proc.Comm + " " + v.Proc.Exe + " " + strings.Join(v.Proc.Cmdline, " "))
			for _, sig := range []string{"xmrig", "minerd", "cpuminer", "kdevtmpfsi", "kinsing", "stratum+tcp://", "stratum+ssl://", "cryptonight", "randomx", "nicehash"} {
				if strings.Contains(hay, sig) {
					return "signature " + strconv.Quote(sig), true
				}
			}
			return "", false
		},
	},
	{
		Key: "fork-bomb", Level: LevelIntrusion,
		Description: "more than " + strconv.Itoa(maxProcesses) + " processes under one site",
		Match: func(v View) (string, bool) {
			if v.Root && len(v.Tree) > maxProcesses {
				return strconv.Itoa(len(v.Tree)) + " processes", true
			}
			return "", false
		},
	},
}

func init() {
	// The text signatures judge each process's command line too, with the
	// site's own HOME and folder taken out first.
	for _, tr := range textRules {
		tr := tr
		rules = append(rules, Rule{Key: tr.Key, Level: tr.Level, Description: tr.Description,
			Match: func(v View) (string, bool) {
				if len(v.Proc.Cmdline) == 0 {
					return "", false
				}
				r, where, ok := MatchText(strings.Join(v.Proc.Cmdline, " "), v.Site.Home, v.Site.Dir)
				return where, ok && r.Key == tr.Key
			}})
	}
}

// Rules lists what the guard looks for, in the order it checks.
func Rules() []Rule {
	out := make([]Rule, len(rules))
	copy(out, rules)
	return out
}

// foreignSite returns the domain of another site that path points into,
// or "" when it is the process's own site or not a site at all.
func foreignSite(v View, path string) string {
	if v.SitesDir == "" || path == "" {
		return ""
	}
	prefix := filepath.Clean(v.SitesDir) + string(filepath.Separator)
	i := strings.Index(path, prefix)
	if i < 0 {
		return ""
	}
	rest := path[i+len(prefix):]
	domain := rest
	if j := strings.IndexAny(rest, "/ :\n\t'\"`),;"); j >= 0 {
		domain = rest[:j]
	}
	// A path at the end of a sentence carries its full stop: "the site's
	// folder: /srv/middleware/sites/a.example. The whole…" named
	// "a.example." and killed a.example's own agent as an intruder.
	domain = strings.TrimRight(domain, ".")
	if domain == "" || domain == v.Site.Domain || !strings.Contains(domain, ".") {
		return ""
	}
	return domain
}

// houseBinary is the running middleware's own executable, resolved.
var houseBinary = func() string {
	p, err := os.Executable()
	if err != nil {
		return ""
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}()

func firstArg(cmdline []string) string {
	if len(cmdline) == 0 {
		return ""
	}
	return cmdline[0]
}
