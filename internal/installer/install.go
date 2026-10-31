package installer

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"

	"github.com/dcimorra/the-middleware/internal/fsx"
)

// Paths are the system directories the installer writes to. Variables so
// tests can point them at t.TempDir() and the real machine is never touched.
type Paths struct {
	SystemdDir  string
	TmpfilesDir string
	SysctlDir   string
	// Fail2banDir is fail2ban's configuration folder; empty is
	// /etc/fail2ban.
	Fail2banDir string
	// AppArmorDir is AppArmor's profiles folder; empty is /etc/apparmor.d.
	AppArmorDir string
}

// SystemPaths are where a Linux distribution expects these files.
func SystemPaths() Paths {
	return Paths{SystemdDir: "/etc/systemd/system", TmpfilesDir: "/etc/tmpfiles.d", SysctlDir: "/etc/sysctl.d", Fail2banDir: "/etc/fail2ban"}
}

// Installer runs the steps. Zero values are filled by New.
type Installer struct {
	Exec   Exec
	Params Params
	Paths  Paths
	// Out receives one line per step.
	Out io.Writer
	// Source is the executable to copy into Params.Binary; defaults to the
	// running one.
	Source string
	// NoStart renders and enables but never starts or restarts a unit.
	NoStart bool
	// KeepBinary leaves Params.Binary in place on uninstall.
	KeepBinary bool
	// OperatorExists says whether `middleware setup` already ran. The auth
	// package owns the file; the installer only needs a yes or no.
	OperatorExists func(root string) bool
	// Live says Out is a terminal, where progress rewrites one line.
	Live bool
	// FindBrowser looks for a headless Chromium; nil is shots.FindIn.
	FindBrowser func(root string) string
	// JailCheck says whether this process can build a site's jail; nil is
	// jail.Available. Tests set it: the answer is the machine's, and a CI
	// runner may say either.
	JailCheck func() error
	// BrowserPlatform overrides the Chrome for Testing platform (tests).
	BrowserPlatform string
	// Node installs nvm under the root and the current LTS with it; nil
	// skips the step (tests). New sets it.
	// user is who runs it: empty for whoever this process is, the
	// operator's user when this runs as root.
	Node func(root, user string) (bool, string, error)
	// AsRoot is a run as root for Params.User (Deps from the panel,
	// through the helper): what it creates under the root as root is
	// handed to that user at the end.
	AsRoot bool

	// setupOutput is what `middleware setup` printed, shown after the step
	// lines so the one-time code is the last thing on screen.
	setupOutput string
}

// New returns an installer against the real machine for the current user.
func New(p Params) *Installer {
	if p.User == "" {
		if u, err := user.Current(); err == nil {
			p.User = u.Username
		}
	}
	return &Installer{Exec: System{}, Params: p, Paths: SystemPaths(), Out: os.Stdout, Node: prepareNode, AsRoot: os.Geteuid() == 0 && p.User != "root"}
}

// Step is one re-runnable unit of work. changed is false when the machine
// was already in the wanted state, so a re-run prints "unchanged" lines and
// touches nothing.
type Step struct {
	Name string
	Run  func() (changed bool, detail string, err error)
}

// optional marks a step whose failure warns and lets the rest go on: a
// machine without the pictures of its sites is a whole machine, one
// without its units is not. cost says what is missing while it fails, in
// the warning: every optional step once said the browser's ("the panel
// works without the pictures"), fail2ban's too.
func optional(cost string, run func() (bool, string, error)) func() (bool, string, error) {
	return func() (bool, string, error) {
		c, d, err := run()
		if err != nil {
			err = optionalError{err, cost}
		}
		return c, d, err
	}
}

type optionalError struct {
	error
	cost string
}

func (e optionalError) Unwrap() error { return e.error }

// Report is what a run produced, for callers that want more than the lines.
type Report struct {
	Changed []string
	Failed  []string
}

func (in *Installer) defaults() error {
	if in.Exec == nil {
		in.Exec = System{}
	}
	if in.Out == nil {
		in.Out = io.Discard
	}
	if in.Paths == (Paths{}) {
		in.Paths = SystemPaths()
	}
	if in.Source == "" {
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("cannot find own executable: %w", err)
		}
		in.Source = exe
	}
	if in.OperatorExists == nil {
		in.OperatorExists = defaultOperatorExists
	}
	return in.Params.Validate()
}

// defaultOperatorExists says the operator's password is set.
func defaultOperatorExists(root string) bool {
	// Where the auth package keeps the operator's password. The installer
	// looked for data/auth.json, which nothing writes: every re-run took
	// the house for new and printed a setup code, which can set a new
	// password.
	b, err := os.ReadFile(filepath.Join(root, "data", "auth", "password"))
	return err == nil && len(bytes.TrimSpace(b)) > 0
}

func (in *Installer) runSteps(steps []Step) (Report, error) {
	var rep Report
	for _, s := range steps {
		changed, detail, err := s.Run()
		switch {
		case err != nil && errors.As(err, new(optionalError)):
			rep.Failed = append(rep.Failed, s.Name)
			var oe optionalError
			errors.As(err, &oe)
			fmt.Fprintf(in.Out, "! %-26s %s\n", s.Name, shortErr(err, oe.cost))
		case err != nil:
			rep.Failed = append(rep.Failed, s.Name)
			fmt.Fprintf(in.Out, "✗ %-26s %v\n", s.Name, err)
			return rep, fmt.Errorf("%s: %w", s.Name, err)
		case changed:
			rep.Changed = append(rep.Changed, s.Name)
			fmt.Fprintf(in.Out, "✓ %-26s %s\n", s.Name, detail)
		default:
			if detail == "" {
				detail = "unchanged"
			}
			fmt.Fprintf(in.Out, "✓ %-26s %s\n", s.Name, detail)
		}
	}
	return rep, nil
}

// shortErr is an optional step's failure on one line: apt's whole output
// was the installer's last screen, and the line that matters is its "E:".
func shortErr(err error, cost string) string {
	tail := " (the rest goes on; run middleware install again to retry)"
	if cost != "" {
		tail = " (the rest goes on: " + cost + "; run middleware install again to retry)"
	}
	msg := err.Error()
	for _, line := range strings.Split(msg, "\n") {
		if l := strings.TrimSpace(line); strings.HasPrefix(l, "E: ") {
			return l + tail
		}
	}
	if i := strings.IndexByte(msg, '\n'); i > 0 {
		msg = msg[:i]
	}
	return msg + tail
}

// Requirements runs only the requirement steps of Install: the packages, docker,
// nvm, the browser when Params.Browser is "yes", fail2ban, and the
// ownership of what it made as root. No unit, no binary, no restart: what
// the panel may start (the helper's requirements.install). It says whether the
// supervisor must restart to see what changed: the operator added to
// docker's group, or what the sites' jails need (stepJail).
func (in *Installer) Requirements() (Report, bool, error) {
	if err := in.defaults(); err != nil {
		return Report{}, false, err
	}
	if os.Geteuid() == 0 && in.Params.User == "root" {
		return Report{}, false, errors.New("refusing to install for root")
	}
	dockerChanged, jailChanged := false, false
	rep, err := in.runSteps([]Step{
		{"package manager", in.stepPackageManager},
		{"git, curl, wireguard, nftables, libatomic", in.stepPackages},
		{"docker", optional("sites in containers will not start here", func() (bool, string, error) {
			c, d, err := in.stepDocker()
			dockerChanged = c
			return c, d, err
		})},
		{"node (nvm)", optional("the first site that asks for a Node installs nvm itself, and waits for it", in.stepNode)},
		{"headless browser", optional("the panel works without the pictures", in.stepBrowser)},
		{"fail2ban", optional("SSH has no bans", in.stepFail2ban)},
		{"site jails", optional("sites run without a jail of their own", func() (bool, string, error) {
			c, d, err := in.stepJail()
			jailChanged = c
			return c, d, err
		})},
		{"ownership", in.stepOwnership},
	})
	return rep, dockerChanged || jailChanged, err
}

// Install runs every step in order and stops at the first failure, which is
// the safest thing an installer can do: a later step that assumes an
// earlier one would leave a half machine that looks whole.
func (in *Installer) Install() (Report, error) {
	if err := in.defaults(); err != nil {
		return Report{}, err
	}
	if os.Geteuid() == 0 && in.Params.User == "root" {
		return Report{}, errors.New("refusing to install for root: run as the user that will operate the panel (sudo is used where needed)")
	}
	var unitsChanged, binaryChanged, dockerChanged bool
	steps := []Step{
		{"package manager", in.stepPackageManager},
		{"git, curl, wireguard, nftables, libatomic", in.stepPackages},
		{"docker", optional("sites in containers will not start here", func() (bool, string, error) {
			c, d, err := in.stepDocker()
			dockerChanged = c
			return c, d, err
		})},
		{"system group", in.stepGroup},
		{"user in group", in.stepUserInGroup},
		{"root directory", in.stepRootDir},
		{"node (nvm)", optional("the first site that asks for a Node installs nvm itself, and waits for it", in.stepNode)},
		{"headless browser", optional("the panel works without the pictures", in.stepBrowser)},
		{"binary", func() (bool, string, error) {
			c, d, err := in.stepBinary()
			binaryChanged = c
			return c, d, err
		}},
		{"units and config", func() (bool, string, error) {
			c, d, err := in.stepFiles()
			unitsChanged = c
			return c, d, err
		}},
		{"site jails", optional("sites run without a jail of their own", in.stepJail)},
		{"sysctl", in.stepSysctl},
		{"tmpfiles", in.stepTmpfiles},
		{"daemon-reload", func() (bool, string, error) {
			if !unitsChanged {
				return false, "", nil
			}
			_, err := in.Exec.Sudo("systemctl", "daemon-reload")
			return true, "reloaded", err
		}},
		// After the binary: the jail's action runs it.
		{"fail2ban", optional("SSH has no bans", in.stepFail2ban)},
		{"enable units", in.stepEnable},
		// A group the user was just added to reaches the service only when
		// it starts again: docker's, without that, answers "permission
		// denied" to a supervisor that was told it may use it.
		{"start units", func() (bool, string, error) { return in.stepStart(unitsChanged || binaryChanged || dockerChanged) }},
		{"operator", in.stepSetup},
	}
	rep, err := in.runSteps(steps)
	if in.setupOutput != "" {
		// setup's own lines say where the panel is: https://<domain>, or
		// the loopback address and the SSH tunnel to reach it.
		fmt.Fprintf(in.Out, "\n%s", in.setupOutput)
	}
	return rep, err
}

// Uninstall stops and removes the units and the system files. It never
// touches the root directory: the sites are the operator's work and a
// command that deletes them by default is a command that will be run once
// by mistake.
func (in *Installer) Uninstall() (Report, error) {
	if err := in.defaults(); err != nil {
		return Report{}, err
	}
	units := in.unitNames()
	steps := []Step{
		{"stop units", func() (bool, string, error) {
			changed := false
			for _, u := range units {
				if !in.isActive(u) {
					continue
				}
				if _, err := in.Exec.Sudo("systemctl", "stop", u); err != nil {
					return changed, "", err
				}
				changed = true
			}
			return changed, "stopped", nil
		}},
		{"disable units", func() (bool, string, error) {
			changed := false
			for _, u := range units {
				if !in.isEnabled(u) {
					continue
				}
				if _, err := in.Exec.Sudo("systemctl", "disable", u); err != nil {
					return changed, "", err
				}
				changed = true
			}
			return changed, "disabled", nil
		}},
		{"fail2ban jail", func() (bool, string, error) {
			// The house's jail and action go; fail2ban stays, it is the
			// machine's.
			in.removeFail2ban()
			return false, "removed", nil
		}},
		{"remove files", func() (bool, string, error) {
			changed := false
			for _, p := range in.filePaths() {
				if !fsx.Exists(p) {
					continue
				}
				if err := in.Exec.RemoveRoot(p); err != nil {
					return changed, "", err
				}
				changed = true
			}
			if changed {
				if _, err := in.Exec.Sudo("systemctl", "daemon-reload"); err != nil {
					return true, "", err
				}
			}
			return changed, "removed", nil
		}},
		{"binary", func() (bool, string, error) {
			if in.KeepBinary || !fsx.Exists(in.Params.Binary) {
				return false, "kept", nil
			}
			// What update.install keeps next to it goes with it: the binary
			// it replaced and the record of the update on trial
			// (helper.PrevSuffix, helper.StateSuffix).
			for _, extra := range []string{in.Params.Binary + ".prev", in.Params.Binary + ".update.json"} {
				if fsx.Exists(extra) {
					if err := in.Exec.RemoveRoot(extra); err != nil {
						return true, "", err
					}
				}
			}
			return true, "removed " + in.Params.Binary, in.Exec.RemoveRoot(in.Params.Binary)
		}},
	}
	rep, err := in.runSteps(steps)
	fmt.Fprintf(in.Out, "\nKept %s with every site, task and data file in it. Remove it yourself if that is what you want.\n", in.Params.Root)
	return rep, err
}

// unitNames lists the units in the order they are started.
func (in *Installer) unitNames() []string {
	units := []string{SocketUnit}
	if in.Params.Helper {
		units = append(units, HelperUnit)
	}
	return append(units, ServiceUnit)
}

// filePaths is every file the installer owns outside the root.
func (in *Installer) filePaths() []string {
	return append([]string{
		filepath.Join(in.Paths.SystemdDir, ServiceUnit),
		filepath.Join(in.Paths.SystemdDir, SocketUnit),
		filepath.Join(in.Paths.SystemdDir, HelperUnit),
		filepath.Join(in.Paths.TmpfilesDir, TmpfilesIn),
		filepath.Join(in.Paths.SysctlDir, SysctlIn),
	}, in.jailFiles()...)
}

func (in *Installer) pathFor(name string) string {
	switch name {
	case TmpfilesIn:
		return filepath.Join(in.Paths.TmpfilesDir, name)
	case SysctlIn:
		return filepath.Join(in.Paths.SysctlDir, name)
	default:
		return filepath.Join(in.Paths.SystemdDir, name)
	}
}

// packageManagers in the order they are looked for. apt-get before apt
// because apt warns about its CLI being unstable when scripted.
var packageManagers = []struct {
	bin     string
	install []string
}{
	// apt waits for the dpkg lock instead of failing at once: right after
	// installing docker.io, or with the daily upgrades running, the next
	// install found it taken and gave up (fail2ban did, the first time it
	// was installed for real).
	{"apt-get", []string{"apt-get", "install", "-y", "-q", "-o", "DPkg::Lock::Timeout=300"}},
	{"dnf", []string{"dnf", "install", "-y", "-q"}},
	{"apk", []string{"apk", "add", "--no-cache"}},
	{"pacman", []string{"pacman", "-S", "--noconfirm", "--needed"}},
	{"zypper", []string{"zypper", "--non-interactive", "install"}},
}

func (in *Installer) packageManager() ([]string, string) {
	for _, pm := range packageManagers {
		if _, err := in.Exec.LookPath(pm.bin); err == nil {
			return pm.install, pm.bin
		}
	}
	return nil, ""
}

func (in *Installer) stepPackageManager() (bool, string, error) {
	_, name := in.packageManager()
	if name == "" {
		return false, "none found (apt, dnf, apk, pacman, zypper); packages will not be installed", nil
	}
	return false, name, nil
}

func (in *Installer) stepPackages() (bool, string, error) {
	var missing []string
	// Binary → package: wg comes from wireguard-tools, so the vpn module
	// finds it there the day it is enabled instead of asking the operator
	// to apt-get something by hand.
	for _, p := range []struct{ bin, pkg string }{{"git", "git"}, {"curl", "curl"}, {"wg", "wireguard-tools"}, {"nft", "nftables"}} {
		if _, err := in.Exec.LookPath(p.bin); err != nil {
			missing = append(missing, p.pkg)
		}
	}
	cmd, name := in.packageManager()
	// Libraries → package, per package manager: libatomic is what Node's
	// official arm64 builds (the ones nvm downloads) link against, and a
	// minimal Ubuntu on an Oracle aarch64 machine does not have it. Every
	// Node site there died at once with "node: error while loading shared
	// libraries: libatomic.so.1" (exit 127), the template's too. Arch has
	// it in gcc-libs, which is always there.
	for _, l := range []struct {
		so   string
		pkgs map[string]string
	}{{"libatomic.so.1", map[string]string{"apt-get": "libatomic1", "dnf": "libatomic", "apk": "libatomic", "zypper": "libatomic1"}}} {
		if pkg := l.pkgs[name]; pkg != "" && !in.Exec.HasLib(l.so) {
			missing = append(missing, pkg)
		}
	}
	if len(missing) == 0 {
		return false, "present", nil
	}
	if cmd == nil {
		return false, "", fmt.Errorf("missing %s and no package manager to install them", strings.Join(missing, ", "))
	}
	args := append(append([]string{}, cmd[1:]...), missing...)
	if _, err := in.Exec.Sudo(cmd[0], args...); err != nil {
		return false, "", err
	}
	return true, fmt.Sprintf("installed %s with %s", strings.Join(missing, ", "), name), nil
}

// dockerPackages is the docker engine's package in each distribution's
// own repositories: what the distribution supports, not a vendor script
// piped to a shell.
// dockerCLIPackages is the package with the `docker` command where the
// distribution ships it apart from the daemon's.
var dockerCLIPackages = map[string]string{"apt-get": "docker-cli"}

// buildxPackages is docker's BuildKit front end in each distribution.
var buildxPackages = map[string]string{"apt-get": "docker-buildx", "dnf": "docker-buildx", "apk": "docker-cli-buildx", "pacman": "docker-buildx", "zypper": "docker-buildx"}

var dockerPackages = map[string]string{"apt-get": "docker.io", "dnf": "moby-engine", "apk": "docker", "pacman": "docker", "zypper": "docker"}

// stepDocker makes the docker runtime work on every machine: installed
// when missing, its daemon enabled and started, and the operator's user
// in the docker group so the supervisor can use it. A site in a container
// that arrives on a machine without docker (a move, a restore) is a site
// that does not start, and nothing in the panel can fix that. The docker
// group is root on the host through the daemon; docs/security.md says so.
func (in *Installer) stepDocker() (bool, string, error) {
	changed := false
	var did []string
	if _, err := in.Exec.LookPath("docker"); err != nil {
		cmd, name := in.packageManager()
		pkg := dockerPackages[name]
		if cmd == nil || pkg == "" {
			return false, "", errors.New("docker is not installed and there is no package manager here to install it with")
		}
		args := append(append([]string{}, cmd[1:]...), pkg)
		if _, err := in.Exec.Sudo(cmd[0], args...); err != nil {
			return false, "", err
		}
		changed = true
		did = append(did, "installed "+pkg+" with "+name)
		// The daemon's package is not always the command's: Debian 12
		// ships `docker` in docker-cli, apart from docker.io, and an
		// install that put only the daemon said "✓ docker installed"
		// while every site in a container failed with "docker is not
		// installed". The command is what the house runs, so it is what
		// is checked.
		if _, err := in.Exec.LookPath("docker"); err != nil {
			if cli := dockerCLIPackages[name]; cli != "" {
				args := append(append([]string{}, cmd[1:]...), cli)
				if _, err := in.Exec.Sudo(cmd[0], args...); err != nil {
					return changed, "", fmt.Errorf("the docker daemon is installed, but not its command (%s): %w", cli, err)
				}
				did = append(did, "installed "+cli)
			}
		}
		if _, err := in.Exec.LookPath("docker"); err != nil {
			return changed, "", fmt.Errorf("%s was installed but there is no `docker` command on the PATH", pkg)
		}
	}
	if _, err := in.Exec.Run("docker", "version", "--format", "{{.Server.Version}}"); err != nil || changed {
		if _, err := in.Exec.Sudo("systemctl", "enable", "--now", "docker"); err != nil {
			return changed, "", fmt.Errorf("the docker daemon could not be started: %w", err)
		}
		did = append(did, "daemon enabled and started")
		changed = true
	}
	// BuildKit's front end: a site's services build with it, and without
	// it docker falls back to the legacy builder, deprecated and slower.
	// Not every release of every distribution packages it, so a missing
	// one is said and stops nothing.
	if _, err := in.Exec.Run("docker", "buildx", "version"); err != nil {
		if cmd, name := in.packageManager(); cmd != nil && buildxPackages[name] != "" {
			args := append(append([]string{}, cmd[1:]...), buildxPackages[name])
			if _, err := in.Exec.Sudo(cmd[0], args...); err == nil {
				did = append(did, "installed "+buildxPackages[name])
				changed = true
			} else {
				did = append(did, "buildx not installed ("+buildxPackages[name]+": "+strings.TrimSpace(strings.SplitN(err.Error(), "\n", 2)[0])+"); services build with the legacy builder")
			}
		}
	}
	out, err := in.Exec.Run("id", "-nG", in.Params.User)
	if err != nil {
		return changed, "", fmt.Errorf("user %s: %w", in.Params.User, err)
	}
	if !slices.Contains(strings.Fields(out), "docker") {
		if _, err := in.Exec.Sudo("usermod", "-aG", "docker", in.Params.User); err != nil {
			return changed, "", err
		}
		did = append(did, in.Params.User+" added to docker")
		changed = true
	}
	if !changed {
		return false, "present", nil
	}
	return true, strings.Join(did, "; "), nil
}

func (in *Installer) stepGroup() (bool, string, error) {
	if _, err := in.Exec.Run("getent", "group", in.Params.Group); err == nil {
		return false, in.Params.Group + " exists", nil
	}
	if _, err := in.Exec.Sudo("groupadd", "--system", in.Params.Group); err != nil {
		return false, "", err
	}
	return true, "created " + in.Params.Group, nil
}

func (in *Installer) stepUserInGroup() (bool, string, error) {
	out, err := in.Exec.Run("id", "-nG", in.Params.User)
	if err != nil {
		return false, "", fmt.Errorf("user %s: %w", in.Params.User, err)
	}
	for _, g := range strings.Fields(out) {
		if g == in.Params.Group {
			return false, fmt.Sprintf("%s is in %s", in.Params.User, in.Params.Group), nil
		}
	}
	if _, err := in.Exec.Sudo("usermod", "-aG", in.Params.Group, in.Params.User); err != nil {
		return false, "", err
	}
	return true, fmt.Sprintf("added %s to %s (takes effect at next login)", in.Params.User, in.Params.Group), nil
}

var rootSubdirs = []string{"sites", "tasks", "data", "modules"}

func (in *Installer) stepRootDir() (bool, string, error) {
	root := in.Params.Root
	changed := false
	if !fsx.Exists(root) {
		// The parent (/srv) is usually root's. Try as the user first so a
		// root under $HOME needs no sudo at all.
		if err := os.MkdirAll(root, 0o755); err != nil {
			if _, err := in.Exec.Sudo("install", "-d", "-m", "0755", "-o", in.Params.User, "-g", in.Params.Group, root); err != nil {
				return false, "", err
			}
		}
		changed = true
	}
	for _, d := range rootSubdirs {
		p := filepath.Join(root, d)
		if fsx.Exists(p) {
			continue
		}
		if err := os.MkdirAll(p, 0o755); err != nil {
			return changed, "", fmt.Errorf("%s: %w (is %s owned by %s?)", p, err, root, in.Params.User)
		}
		changed = true
	}
	if changed {
		return true, "created " + root, nil
	}
	return false, root, nil
}

func fileSum(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

func (in *Installer) stepBinary() (bool, string, error) {
	src, err := fileSum(in.Source)
	if err != nil {
		return false, "", err
	}
	if dst, err := fileSum(in.Params.Binary); err == nil && bytes.Equal(src, dst) {
		return false, in.Params.Binary + " is current", nil
	}
	data, err := os.ReadFile(in.Source)
	if err != nil {
		return false, "", err
	}
	if err := in.Exec.WriteRoot(in.Params.Binary, data, 0o755); err != nil {
		return false, "", err
	}
	return true, "installed " + in.Params.Binary, nil
}

// stepFiles writes every rendered file whose content differs. Comparing
// first is what makes a re-run quiet, and what keeps daemon-reload from
// running when nothing changed.
func (in *Installer) stepFiles() (bool, string, error) {
	files, err := Render(in.Params)
	if err != nil {
		return false, "", err
	}
	var written []string
	for _, name := range []string{ServiceUnit, SocketUnit, HelperUnit, TmpfilesIn, SysctlIn} {
		content, ok := files[name]
		if !ok {
			continue
		}
		p := in.pathFor(name)
		if cur, err := os.ReadFile(p); err == nil && string(cur) == content {
			continue
		}
		if err := in.Exec.WriteRoot(p, []byte(content), 0o644); err != nil {
			return len(written) > 0, "", err
		}
		written = append(written, name)
	}
	if len(written) == 0 {
		return false, "current", nil
	}
	return true, "wrote " + strings.Join(written, ", "), nil
}

func (in *Installer) stepSysctl() (bool, string, error) {
	want := fmt.Sprintf("%d-%d", in.Params.PortsFrom, in.Params.PortsTo)
	if out, err := in.Exec.Run("sysctl", "-n", "net.ipv4.ip_local_reserved_ports"); err == nil && strings.TrimSpace(out) == want {
		return false, "ports " + want + " reserved", nil
	}
	if _, err := in.Exec.Sudo("sysctl", "-q", "-p", in.pathFor(SysctlIn)); err != nil {
		return false, "", err
	}
	return true, "reserved ports " + want, nil
}

func (in *Installer) stepTmpfiles() (bool, string, error) {
	if fsx.Exists(in.Params.RunDir) {
		return false, in.Params.RunDir + " exists", nil
	}
	if _, err := in.Exec.Sudo("systemd-tmpfiles", "--create", in.pathFor(TmpfilesIn)); err != nil {
		return false, "", err
	}
	return true, "created " + in.Params.RunDir, nil
}

func (in *Installer) isEnabled(unit string) bool {
	out, err := in.Exec.Run("systemctl", "is-enabled", unit)
	return err == nil && strings.TrimSpace(out) == "enabled"
}

func (in *Installer) isActive(unit string) bool {
	out, err := in.Exec.Run("systemctl", "is-active", unit)
	return err == nil && strings.TrimSpace(out) == "active"
}

func (in *Installer) stepEnable() (bool, string, error) {
	var enabled []string
	for _, u := range in.unitNames() {
		if in.isEnabled(u) {
			continue
		}
		if _, err := in.Exec.Sudo("systemctl", "enable", u); err != nil {
			return len(enabled) > 0, "", err
		}
		enabled = append(enabled, u)
	}
	if len(enabled) == 0 {
		return false, "enabled", nil
	}
	return true, "enabled " + strings.Join(enabled, ", "), nil
}

// stepStart starts what is down and restarts what changed underneath a
// running unit. The socket is never restarted: it holds the loopback port
// and the service picks up the new binary on its own restart.
func (in *Installer) stepStart(restart bool) (bool, string, error) {
	if in.NoStart {
		return false, "skipped (--no-start)", nil
	}
	var done []string
	for _, u := range in.unitNames() {
		switch {
		case !in.isActive(u):
			if _, err := in.Exec.Sudo("systemctl", "start", u); err != nil {
				return len(done) > 0, "", err
			}
			done = append(done, "started "+u)
		case restart && u != SocketUnit:
			if _, err := in.Exec.Sudo("systemctl", "restart", u); err != nil {
				return len(done) > 0, "", err
			}
			done = append(done, "restarted "+u)
		}
	}
	if len(done) == 0 {
		return false, "running", nil
	}
	return true, strings.Join(done, ", "), nil
}

// stepSetup runs `middleware setup` once, so the install ends with the
// one-time code on screen instead of a second command to remember.
func (in *Installer) stepSetup() (bool, string, error) {
	if in.Params.Join != "" {
		return in.stepJoin()
	}
	exists := in.OperatorExists(in.Params.Root)
	if exists && in.Params.Domain == "" {
		return false, "exists", nil
	}
	if in.NoStart {
		return false, fmt.Sprintf("none yet: run `%s setup --root %s` after starting", in.Params.Binary, in.Params.Root), nil
	}
	args := []string{"setup", "--root", in.Params.Root}
	if in.Params.Domain != "" {
		args = append(args, "--domain", in.Params.Domain)
		if in.Params.Door != "" {
			args = append(args, "--door", in.Params.Door)
		}
	}
	out, err := in.Exec.Run(in.Params.Binary, args...)
	if err != nil {
		return false, fmt.Sprintf("none yet: run `%s setup --root %s` to create one (%v)", in.Params.Binary, in.Params.Root, err), nil
	}
	in.setupOutput = out
	if exists {
		return true, "the panel's domain is " + in.Params.Domain, nil
	}
	return true, "created; the one-time code is below", nil
}

// stepJoin enters the network an invitation code was made in, through the
// running supervisor: the node has no domain and no operator of its own,
// and is managed from the network's panels. The code is single-use and
// lasts fifteen minutes: a failure says to make another.
func (in *Installer) stepJoin() (bool, string, error) {
	if in.NoStart {
		return false, fmt.Sprintf("not joined: run `%s setup --join <code> --root %s` after starting", in.Params.Binary, in.Params.Root), nil
	}
	args := []string{"setup", "--root", in.Params.Root, "--join", in.Params.Join}
	if in.Params.MeshEndpoint != "" {
		args = append(args, "--endpoint", in.Params.MeshEndpoint)
	}
	out, err := in.Exec.Run(in.Params.Binary, args...)
	if err != nil {
		return false, "", fmt.Errorf("joining the network: %v: %s (an invitation lasts fifteen minutes and opens once: make another in a node's panel, Dashboard → Invite, and run %s setup --join <code>)", err, strings.TrimSpace(out), in.Params.Binary)
	}
	in.setupOutput = out
	return true, "joined the network; managed from its panels", nil
}
