package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// Jail is what a run may and may not touch. The core builds it; adapters
// translate it; the audit checks it. Deny holds absolute paths: every
// other site folder, the installation's data (except this site's own
// home), tasks, modules and whatever else sits at the root. The root itself
// is not listed because a deny rule on the root would also cover the site
// folder inside it: "everything but here" is expressed by listing the
// siblings, which is why the list is rebuilt for every turn.
type Jail struct {
	// SiteDir is the one folder the agent works in.
	SiteDir string
	// Home is the HOME the child gets, under data/homes/<domain>.
	Home string
	// Config is where the CLIs keep their own state for this site, under
	// data/agents/config/<domain>: not in Home, which the site can write
	// (ConfigHome says why).
	Config string
	// Deny is the list of absolute paths the agent must not touch.
	Deny []string
	// Commands are command prefixes the agent must not run: what reaches
	// past the site to the machine (sudo, systemctl, the tunnel, docker,
	// which on a rootless daemon mounts the supervisor's whole HOME) and
	// what cannot be undone on a shared remote (a forced push).
	Commands []string
	// Secrets are path patterns that may not be read even inside the
	// site's own folder (its .env, for a conversation that cannot write).
	Secrets []string
	// Lent are other folders the agent may read and not write.
	Lent []string
}

// Lend opens dirs for reading: they leave Deny (by both their names, the
// link and the target) and are listed in Lent, which adapters close to
// writing. A turn runs with permissions skipped, so a folder merely left
// out of Deny would be a folder the agent can edit.
func (j *Jail) Lend(dirs ...string) {
	drop := map[string]bool{}
	for _, d := range dirs {
		d = absolute(d)
		drop[d], drop[resolved(d)] = true, true
		j.Lent = append(j.Lent, d)
	}
	kept := j.Deny[:0]
	for _, d := range j.Deny {
		if !drop[d] {
			kept = append(kept, d)
		}
	}
	j.Deny = kept
}

// machineSecrets are the supervisor's own keys, under its real HOME (not
// the jail's): denied to every conversation, the house's included. The
// agent works with permissions skipped, so what is not denied is allowed.
var machineSecrets = []string{".ssh", ".aws", ".cloudflare", ".claude", ".claude.json", ".config/gcloud", ".docker", ".kube", ".gnupg", ".netrc", ".git-credentials", ".npmrc"}

// deniedCommands for every conversation; the house's may run docker.
var deniedCommands = []string{"sudo", "su", "systemctl", "service", "shutdown", "reboot", "cloudflared",
	"git push --force", "git push -f", "git push --delete", "git push --mirror"}

var siteOnlyCommands = []string{"docker", "podman", "nsenter", "unshare"}

// realHome is the supervisor's HOME; a variable so tests use their own.
var realHome = func() string { h, _ := os.UserHomeDir(); return h }

// NewJail builds the jail for workdir inside root. Siblings are listed at
// call time, so a site added after this installation started is denied
// the next turn without a restart. For the installation's own conversation
// (workdir == root) every site that exists when the turn starts is denied
// one by one, never sites/ itself: that conversation may create a site (a
// new folder with a site.json, which the engine picks up on its next
// tick) but never edit a tenant that was already there. Its data (the
// credentials box, the other homes, the conversations) and the modules
// are denied too; the rest of the root, tasks included, is its own.
func NewJail(root, workdir, home string) Jail {
	root = absolute(root)
	workdir = absolute(workdir)
	if home != "" {
		home = absolute(home)
	}
	j := Jail{SiteDir: workdir, Home: home, Config: configBase(root, home), Commands: append([]string(nil), deniedCommands...)}
	if workdir != root {
		j.Commands = append(j.Commands, siteOnlyCommands...)
	}
	seen := map[string]bool{}
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			j.Deny = append(j.Deny, p)
		}
	}
	realWork := resolved(workdir)
	var deny func(p string)
	deny = func(p string) {
		p = filepath.Clean(p)
		if p == workdir || p == home || isUnder(home, p) {
			return
		}
		if workdir != root && (isUnder(p, workdir) || isUnder(workdir, p)) {
			return
		}
		// A sibling reached through a link is denied by both names: the
		// agent can open either. When the sibling (by its real name) is an
		// ancestor of this site's own folder (a project kept inside another
		// site and linked into sites/), denying it whole would deny the
		// site its own code, since a deny beats everything; so each level
		// on the way down is denied entry by entry except the way down.
		rp := resolved(p)
		if workdir != root && rp != realWork && isUnder(realWork, rp) {
			for _, q := range walkAround(rp, realWork) {
				add(q)
			}
			return
		}
		if workdir != root && rp == realWork {
			return
		}
		add(p)
		if rp != p {
			add(rp)
		}
	}
	if h := realHome(); h != "" {
		for _, s := range machineSecrets {
			add(filepath.Join(h, s))
		}
	}
	if j.Config != "" {
		// Denied even before the folder exists (the siblings below are
		// listed as they are now): it holds the CLI's sessions and login.
		add(filepath.Dir(filepath.Dir(j.Config)))
	}
	if workdir == root {
		for _, ent := range readDir(root) {
			p := filepath.Join(root, ent)
			switch ent {
			case "sites", "data":
				for _, sub := range readDir(p) {
					q := filepath.Join(p, sub)
					if ent == "data" && sub == "homes" {
						for _, h := range readDir(q) {
							deny(filepath.Join(q, h))
						}
						continue
					}
					deny(q)
				}
			case "modules":
				deny(p)
			}
		}
		sort.Strings(j.Deny)
		return j
	}
	own := filepath.Base(workdir)
	for _, ent := range readDir(root) {
		p := filepath.Join(root, ent)
		switch ent {
		case "sites", "data":
			for _, sub := range readDir(p) {
				q := filepath.Join(p, sub)
				if ent == "data" && sub == "homes" {
					for _, h := range readDir(q) {
						deny(filepath.Join(q, h))
					}
					continue
				}
				// The logs one by one: the site's own are lent to it to
				// read (OwnLogs), the others' and the tasks' stay denied.
				// Denied whole, a site's conversation could not read what
				// its own process said, and asked to look at its errors it
				// could only ask for them to be pasted.
				if ent == "data" && sub == "logs" {
					for _, l := range readDir(q) {
						if l != own {
							deny(filepath.Join(q, l))
						}
					}
					continue
				}
				deny(q)
			}
		default:
			deny(p)
		}
	}
	sort.Strings(j.Deny)
	if logs := OwnLogs(root, own); logs != "" {
		j.Lend(logs)
	}
	return j
}

// OwnLogs is a site's logs folder (data/logs/<domain>), what its process,
// its workers and its moves write and its conversation may read; "" when
// there is none yet.
func OwnLogs(root, domain string) string {
	p := filepath.Join(root, "data", "logs", domain)
	if st, err := os.Stat(p); err != nil || !st.IsDir() {
		return ""
	}
	return p
}

// Violations returns the denied paths that transcript mentions. It is a
// substring search on purpose: an agent that wrote "../other.site/.env"
// would slip past a path-aware parser and a false positive costs one event
// to read, while a false negative costs a tenant's secrets.
func (j Jail) Violations(transcript string) []string {
	var out []string
	for _, d := range j.Deny {
		if strings.Contains(transcript, d) {
			out = append(out, d)
		}
	}
	return out
}

// Env is the environment base every adapter starts from: a copy of the
// supervisor's PATH and locale, HOME inside the jail, and the request's own
// variables on top. No duplicate keys, sorted, so two runs with the same
// request build byte-identical environments.
func (j Jail) Env(extra map[string]string) []string {
	env := map[string]string{}
	for _, k := range []string{"PATH", "LANG", "LC_ALL", "TZ", "TERM"} {
		if v, ok := os.LookupEnv(k); ok {
			env[k] = v
		}
	}
	for k, v := range extra {
		env[k] = v
	}
	if j.Home != "" {
		env["HOME"] = j.Home
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

// Prepare creates the home so a CLI that writes its config under HOME
// does not fail its first run, and refuses a jail without a working
// directory: a run with nowhere to stand would inherit the supervisor's.
func (j Jail) Prepare() error {
	if j.SiteDir == "" {
		return fmt.Errorf("jail: no working directory")
	}
	if st, err := os.Stat(j.SiteDir); err != nil || !st.IsDir() {
		return fmt.Errorf("jail: %s is not a directory", j.SiteDir)
	}
	if j.Home != "" {
		if err := os.MkdirAll(j.Home, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// walkAround lists, at every level from ancestor down to target, the
// entries that are not on the way down.
func walkAround(ancestor, target string) []string {
	rel, err := filepath.Rel(ancestor, target)
	if err != nil {
		return nil
	}
	var out []string
	step := ancestor
	for _, name := range strings.Split(rel, string(filepath.Separator)) {
		next := filepath.Join(step, name)
		for _, e := range readDir(step) {
			if p := filepath.Join(step, e); p != next {
				out = append(out, p)
			}
		}
		step = next
	}
	return out
}

// resolved is a path by its real name, links followed when they can be.
func resolved(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// absolute cleans p and makes it absolute; a deny rule with a relative
// path would match nothing the CLI sees.
func absolute(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return filepath.Clean(p)
}

func isUnder(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	if err != nil {
		return false
	}
	return rel != "." && !strings.HasPrefix(rel, "..")
}

func readDir(p string) []string {
	ents, err := os.ReadDir(p)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

// Unattended narrows a jail of the installation's root (NewJail with
// workdir == root) for an agent that works with nobody watching (the
// autonomous agent, internal/autopilot): what the house's own
// conversation may do because the operator is reading it, it may not.
// Docker is root on the machine; `git push` is how anything a prompt
// injected would leave it; and every entry of the root but sites/ and
// data/ (whose entries NewJail already denies one by one) is denied,
// tasks/ above all: a task runs outside any jail, so writing one would be
// a way around every rule here.
func (j *Jail) Unattended(root string) {
	root = absolute(root)
	j.Commands = append(j.Commands, siteOnlyCommands...)
	j.Commands = append(j.Commands, "git push")
	for _, ent := range readDir(root) {
		if ent == "sites" || ent == "data" {
			continue
		}
		p := filepath.Join(root, ent)
		if !slices.Contains(j.Deny, p) {
			j.Deny = append(j.Deny, p)
		}
	}
	sort.Strings(j.Deny)
}
