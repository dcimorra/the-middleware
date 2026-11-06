// Package hostgit is the one hardened way the supervisor runs git on the
// host inside a site's folder.
//
// Why it exists: a docker site's folder, .git included, is mounted
// read-write into its container, and the repos watcher runs git in folders
// a project (or a conversation) works in. git reads configuration and
// hooks from inside the repository it is pointed at, and several of those
// keys name a program git then executes: core.fsmonitor, core.sshCommand,
// core.pager, a filter driver's clean/smudge/process, a diff driver's
// textconv, credential.helper, gpg.program, uploadpack.packObjectsHook,
// and the hooks in .git/hooks. The supervisor's user is in the docker
// group, so a program it runs is effectively root. A compromised container
// or a pushed repository that can drop one of these into .git would own the
// host on the next deploy or the next repos refresh.
//
// Two defences, because neither alone is watertight:
//
//   - A minimal, secret-free environment and a set of -c overrides that
//     disable the single-valued command-bearing keys and the hooks, and
//     limit the transport protocols to the ones the house uses. -c on the
//     command line beats repository config, and GIT_CONFIG_NOSYSTEM with an
//     empty GIT_CONFIG_GLOBAL keeps system and global config out.
//   - -c cannot blanket-disable a key whose middle name is attacker-chosen
//     (filter.<anything>.clean, credential.<url>.helper, diff.<x>.textconv):
//     there is no wildcard. So Guard reads the repository's own config (and
//     any file it includes from within .git) and refuses to run when it
//     finds a command-bearing key, naming it. A legitimate site repository
//     has none of these in .git/config; git-lfs and the like are refused by
//     name rather than run on the host, which is the point.
//
// The environment carries PATH, HOME and a C locale and nothing else, so
// the supervisor's secrets (MIDDLEWARE_SECRET_KEY and friends) never reach
// git or anything it spawns.
package hostgit

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// hardening are the -c overrides every host git invocation carries. They
// win over the repository's config, so a planted core.fsmonitor or a
// post-checkout hook does not run. protocol.allow defaults to never and
// then the transports the house actually uses are turned back on, which
// shuts out ext:: (it runs a command) and other exotic transports a
// submodule or remote URL might smuggle in.
var hardening = []string{
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.fsmonitor=false",
	"-c", "core.sshCommand=",
	"-c", "core.pager=cat",
	"-c", "core.editor=false",
	"-c", "credential.helper=",
	"-c", "protocol.allow=never",
	"-c", "protocol.file.allow=always",
	"-c", "protocol.http.allow=always",
	"-c", "protocol.https.allow=always",
	"-c", "protocol.ssh.allow=always",
	"-c", "protocol.git.allow=always",
}

// Options parametrise one invocation.
type Options struct {
	// Dir is the repository folder git runs in; its config is what Guard
	// scans.
	Dir string
	// Home is the HOME git runs with. For a deploy it is the site's own,
	// isolated home; for the repos watcher it is the supervisor's, so the
	// operator's ssh keys still work. Either way global config is kept out
	// (GIT_CONFIG_GLOBAL below), so a home does not smuggle config in.
	Home string
	// Config are extra -c values appended after the hardening set, for the
	// one thing a call legitimately adds: the deploy's auth header. They
	// come last so a later call cannot be overridden by repository config
	// either.
	Config []string
	// Passthrough names environment variables copied from the supervisor's
	// environment when set, for the few git needs that carry no secret:
	// SSH_AUTH_SOCK so an ssh-agent key reaches a pull.
	Passthrough []string
	// Secret are config pairs (key, value) given through GIT_CONFIG_COUNT
	// and GIT_CONFIG_KEY_n/VALUE_n instead of -c, for a value that must
	// not be read off the command line: a -c is in /proc/<pid>/cmdline,
	// which every user of the machine (a jailed site included) can list,
	// while the environment is only readable by the same user.
	Secret [][2]string
}

// Env is the minimal, secret-free environment git runs with. GIT_CONFIG_
// GLOBAL points at /dev/null (an empty config file) and GIT_CONFIG_NOSYSTEM
// keeps /etc/gitconfig out, so only the repository's own config can speak,
// and Guard has already vetted that. No terminal and no askpass means a
// remote that wants a password fails instead of hanging or prompting.
func Env(o Options) []string {
	env := []string{
		"HOME=" + o.Home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=true",
		"LC_ALL=C",
	}
	if p, ok := os.LookupEnv("PATH"); ok {
		env = append(env, "PATH="+p)
	}
	for _, name := range o.Passthrough {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	if len(o.Secret) > 0 {
		env = append(env, "GIT_CONFIG_COUNT="+strconv.Itoa(len(o.Secret)))
		for i, kv := range o.Secret {
			n := strconv.Itoa(i)
			env = append(env, "GIT_CONFIG_KEY_"+n+"="+kv[0], "GIT_CONFIG_VALUE_"+n+"="+kv[1])
		}
	}
	return env
}

// Command builds the hardened *exec.Cmd after Guard has vetted the
// repository, or returns Guard's error. The caller sets Stdout, Stderr and
// WaitDelay and runs it.
func Command(ctx context.Context, o Options, args ...string) (*exec.Cmd, error) {
	if err := Guard(o.Dir); err != nil {
		return nil, err
	}
	full := make([]string, 0, len(hardening)+len(o.Config)+len(args))
	full = append(full, hardening...)
	full = append(full, o.Config...)
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = o.Dir
	cmd.Env = Env(o)
	return cmd, nil
}

// ErrUnsafeConfig is returned by Guard when a repository's config names a
// program git would run. Its text names the key so an operator reading a
// failed deploy or a stuck repo knows exactly what to remove.
type ErrUnsafeConfig struct {
	Key  string
	File string
}

func (e *ErrUnsafeConfig) Error() string {
	return fmt.Sprintf("refusing to run git: %s sets %q, which runs a program on the host; remove it", e.File, e.Key)
}

// Guard reads dir's .git config and every file it includes from within
// .git, and returns an *ErrUnsafeConfig naming the first command-bearing
// key it finds. A missing .git (a folder not yet init'd) is no error.
// Nothing here runs git, so Guard is safe to call before every invocation.
func Guard(dir string) error {
	gitDir, err := resolveGitDir(dir)
	if err != nil || gitDir == "" {
		return nil
	}
	return scanConfig(filepath.Join(gitDir, "config"), gitDir, map[string]bool{}, 0)
}

// resolveGitDir finds the real .git directory. It is usually dir/.git, but
// a worktree or a submodule leaves a "gitdir: <path>" pointer file there
// instead; following it means Guard scans the config that actually applies.
func resolveGitDir(dir string) (string, error) {
	p := filepath.Join(dir, ".git")
	fi, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if fi.IsDir() {
		return p, nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(data))
	rest, ok := strings.CutPrefix(line, "gitdir:")
	if !ok {
		return "", nil
	}
	target := strings.TrimSpace(rest)
	if !filepath.IsAbs(target) {
		target = filepath.Join(dir, target)
	}
	return target, nil
}

// scanConfig walks one config file for command-bearing keys. An include
// that stays inside gitDir is followed (git honours it, so Guard must too);
// one that points outside is refused, because its file is not git's own and
// cannot be vetted cheaply. The visited set and depth bound stop an include
// cycle from looping.
func scanConfig(path, gitDir string, visited map[string]bool, depth int) error {
	if depth > 10 {
		return &ErrUnsafeConfig{Key: "include", File: path}
	}
	abs, err := filepath.Abs(path)
	if err == nil {
		if visited[abs] {
			return nil
		}
		visited[abs] = true
	}
	f, err := os.Open(path)
	if err != nil {
		// A config that is not there says nothing; an unreadable one is
		// refused so a deploy does not proceed on a config it could not
		// check.
		if os.IsNotExist(err) {
			return nil
		}
		return &ErrUnsafeConfig{Key: "config", File: path}
	}
	defer f.Close()

	section, subsection := "", ""
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := stripComment(strings.TrimSpace(sc.Text()))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			section, subsection = parseSection(line)
			continue
		}
		key, value := line, ""
		if i := strings.IndexByte(line, '='); i >= 0 {
			key, value = strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:])
		}
		key = strings.ToLower(key)
		if commandBearing(section, key) {
			return &ErrUnsafeConfig{Key: joinKey(section, subsection, key), File: path}
		}
		if (section == "include" || section == "includeif") && key == "path" {
			inc, err := resolveInclude(unquote(value), path, gitDir)
			if err != nil {
				return err
			}
			if err := scanConfig(inc, gitDir, visited, depth+1); err != nil {
				return err
			}
		}
	}
	if err := sc.Err(); err != nil {
		return &ErrUnsafeConfig{Key: "config", File: path}
	}
	return nil
}

// commandBearing reports whether section+key is one that makes git run a
// program. The section name is lower-cased (git folds it); the subsection
// in between is attacker-chosen and so cannot be matched, which is the
// whole reason -c cannot disable these and Guard must refuse them.
func commandBearing(section, key string) bool {
	switch section {
	case "filter":
		return key == "clean" || key == "smudge" || key == "process"
	case "diff":
		return key == "command" || key == "textconv"
	case "core":
		// Not refused: core.hooksPath, fsmonitor, sshCommand, pager and
		// editor are single-valued, so the -c overrides in hardening win
		// over whatever the repository says, and GIT_ASKPASS in Env beats
		// core.askPass. Refusing them broke every site using husky, whose
		// install writes core.hooksPath=.husky into .git/config: the deploy
		// failed on a key that could not have run anything.
		return false
	case "gpg":
		return key == "program" || key == "defaultkeycommand"
	case "credential":
		return key == "helper"
	case "uploadpack":
		return key == "packobjectshook"
	}
	return false
}

// resolveInclude turns an include path into a file inside gitDir, or
// refuses it. git resolves a relative include against the including file's
// directory; "~/" is a home path git expands and we do not, so a home
// include is treated as outside .git and refused.
func resolveInclude(value, from, gitDir string) (string, error) {
	if value == "" || strings.HasPrefix(value, "~") {
		return "", &ErrUnsafeConfig{Key: "include.path", File: from}
	}
	p := value
	if !filepath.IsAbs(p) {
		p = filepath.Join(filepath.Dir(from), p)
	}
	p = filepath.Clean(p)
	gitDir = filepath.Clean(gitDir)
	if p != gitDir && !strings.HasPrefix(p, gitDir+string(filepath.Separator)) {
		return "", &ErrUnsafeConfig{Key: "include.path (outside .git)", File: from}
	}
	return p, nil
}

// parseSection reads "[section]" or `[section "sub"]` or legacy
// "[section.sub]" and returns the lower-cased section and its subsection
// (kept as written; git does not fold it).
func parseSection(line string) (section, subsection string) {
	inner := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"))
	if i := strings.IndexByte(inner, '"'); i >= 0 {
		section = strings.ToLower(strings.TrimSpace(inner[:i]))
		if j := strings.LastIndexByte(inner, '"'); j > i {
			subsection = inner[i+1 : j]
		}
		return section, subsection
	}
	if i := strings.IndexByte(inner, '.'); i >= 0 {
		return strings.ToLower(inner[:i]), inner[i+1:]
	}
	return strings.ToLower(inner), ""
}

func joinKey(section, subsection, key string) string {
	if subsection != "" {
		return section + "." + subsection + "." + key
	}
	return section + "." + key
}

// stripComment drops a # or ; comment that is not inside quotes.
func stripComment(line string) string {
	inQuote := false
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '"':
			inQuote = !inQuote
		case '#', ';':
			if !inQuote {
				return strings.TrimSpace(line[:i])
			}
		}
	}
	return line
}

func unquote(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		return v[1 : len(v)-1]
	}
	return v
}
