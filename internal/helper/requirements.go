package helper

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// requirements.install: the house's dependencies, installed from the panel, for an
// operator without SSH. A closed list: the packages the house runs on
// (git, curl, wireguard-tools,
// nftables, and the libatomic Node needs on arm64), docker with the operator in its
// group, fail2ban with the house's jail, nvm with the current LTS and, when
// asked, a headless Chromium: the dependency steps of `middleware install`
// and nothing else, plus what the sites' jails need (an AppArmor profile
// for the binary, a drop-in for a unit rendered before the jail). No unit
// is rewritten, the binary is not replaced and the house is not restarted. The full install from the panel was asked
// for and then turned down (2026-10-24): it would give the panel the
// installer's whole power over the machine's configuration.
//
// Nothing of what runs comes from the caller: the command is this
// binary's own `install --requirements`; the user, the root and the binary are read
// from the installed middleware.service, which only root writes; the one
// argument is whether to add the browser. A supervisor that was broken
// into can have those packages installed, not another binary run or
// another user made the owner.
//
// It runs in a transient unit of its own (systemd-run), not in the helper:
// the helper is sandboxed (ProtectSystem=strict, a few capabilities), and
// apt is out of its reach on purpose. Its output goes to a file in the
// helper's runtime directory, which the panel reads; the last line says
// how it ended.
//
// Not in the root's data/, where it was: that folder is the supervisor's
// user's, and systemd opens StandardOutput=truncate: as root, following
// links. A requirements.log made a link to /etc/shadow had root empty it
// and write the installer's output in its place. The runtime directory is
// root's and only crossable by the supervisor's group.

// InstallUnit is the transient unit's name: one at a time.
const InstallUnit = "middleware-requirements"

// InstallArgs is the input of requirements.install.
type InstallArgs struct {
	// Browser adds a headless Chromium when there is none.
	Browser bool `json:"browser"`
}

// InstallResult says where it runs and where it writes.
type InstallResult struct {
	Unit string `json:"unit"`
	Log  string `json:"log"`
}

// InstallLog is the installer's output in the helper's runtime directory
// (the socket's): DefaultRunDir in production.
func InstallLog(runDir string) string { return filepath.Join(runDir, "requirements.log") }

// DefaultRunDir is the helper's runtime directory, root's.
var DefaultRunDir = filepath.Dir(DefaultSocket)

// installed is what the installed service unit says.
type installed struct {
	User, Root, Binary string
}

var unitUser = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// readServiceUnit reads User=, MIDDLEWARE_ROOT and ExecStart's binary.
func readServiceUnit(path string) (installed, error) {
	f, err := os.Open(path)
	if err != nil {
		return installed{}, fmt.Errorf("the installed service unit: %w", err)
	}
	defer f.Close()
	var in installed
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "User="):
			in.User = strings.TrimPrefix(line, "User=")
		case strings.HasPrefix(line, "Environment=MIDDLEWARE_ROOT="):
			in.Root = strings.TrimPrefix(line, "Environment=MIDDLEWARE_ROOT=")
		case strings.HasPrefix(line, "ExecStart="):
			if f := strings.Fields(strings.TrimPrefix(line, "ExecStart=")); len(f) > 0 {
				in.Binary = f[0]
			}
		}
	}
	if !unitUser.MatchString(in.User) || in.User == "root" {
		return installed{}, fmt.Errorf("%s: User=%q is not an operator's user", path, in.User)
	}
	if !filepath.IsAbs(in.Root) || strings.ContainsAny(in.Root, " \t\"'") {
		return installed{}, fmt.Errorf("%s: MIDDLEWARE_ROOT=%q is not a plain absolute path", path, in.Root)
	}
	if !filepath.IsAbs(in.Binary) {
		return installed{}, fmt.Errorf("%s: ExecStart does not name an absolute binary", path)
	}
	return in, nil
}

func (s *Server) systemdDir() string {
	if s.SystemdDir != "" {
		return s.SystemdDir
	}
	return "/etc/systemd/system"
}

func (s *Server) installRun(ctx context.Context, raw json.RawMessage) (any, error) {
	var a InstallArgs
	if err := decode(raw, &a); err != nil {
		return nil, err
	}
	in, err := readServiceUnit(filepath.Join(s.systemdDir(), "middleware.service"))
	if err != nil {
		return nil, err
	}
	// The unit's binary is this one: what runs as root is the helper's own
	// code, never a file the unit was pointed at afterwards.
	self, err := s.binaryPath()
	if err != nil {
		return nil, err
	}
	if real, err := filepath.EvalSymlinks(in.Binary); err != nil || real != self {
		return nil, fmt.Errorf("the service runs %s and the helper is %s: run middleware install on the machine once", in.Binary, self)
	}
	if out, _ := s.Exec.Run(ctx, "systemctl", []string{"is-active", InstallUnit}, ""); strings.TrimSpace(out) == "active" || strings.TrimSpace(out) == "activating" {
		return nil, errors.New("the requirements are already being installed")
	}
	log := InstallLog(filepath.Dir(s.Socket))
	// The last run's goes first, so what the panel reads from now on is
	// this run's and never says the last one's ending; the supervisor
	// cannot remove it itself.
	if err := os.Remove(log); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	args := []string{
		"--unit=" + InstallUnit, "--collect", "--no-block",
		"--description=middleware requirements, from the panel",
		"--property=StandardOutput=truncate:" + log,
		"--property=StandardError=inherit",
		"--setenv=MIDDLEWARE_ROOT=" + in.Root,
		in.Binary, "install", "--requirements", "--yes", "--panel", "--root", in.Root, "--user", in.User, "--binary", in.Binary,
	}
	if a.Browser {
		args = append(args, "--browser", "yes")
	}
	if out, err := s.Exec.Run(ctx, "systemd-run", args, ""); err != nil {
		return nil, fmt.Errorf("systemd-run: %v: %s", err, strings.TrimSpace(out))
	}
	return InstallResult{Unit: InstallUnit, Log: log}, nil
}

// InstallRun asks the helper to install the house's dependencies in a unit of its own;
// it returns once started, the output going to InstallLog.
func (c *Client) InstallRun(ctx context.Context, browser bool) (InstallResult, error) {
	var r InstallResult
	err := c.Call(ctx, OpInstallRun, InstallArgs{Browser: browser}, &r)
	return r, err
}
