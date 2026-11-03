package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/manifest"
)

// Docker runs an instance as a container through the docker CLI. The CLI
// rather than the engine API keeps the SDK (and its hundred transitive
// modules) out of the core; the few commands used here have been stable for
// a decade.
type Docker struct {
	// Bin is the docker binary; empty means "docker" from PATH.
	Bin string
	// House labels the containers this installation starts (the sites
	// folder), for Reap.
	House string
	// Data is the house's data folder, where the owner of a named volume
	// made before volumes carried one is written down (VolumeClaimsFile).
	// Empty, there is nowhere to write it, and such a volume is used as
	// before.
	Data string
}

// Name implements Runtime.
func (Docker) Name() string { return "docker" }

func (d Docker) bin() string {
	if d.Bin != "" {
		return d.Bin
	}
	return "docker"
}

type dockerInstance struct {
	d        Docker
	name     string
	port     int
	revision string
	domain   string
	logFile  string
	started  time.Time
	done     chan error
	id       string
}

// HouseLabel marks every container this installation starts, with the
// sites folder as its value, so the reaper removes only its own leftovers
// and never another installation's on the same daemon.
const HouseLabel = "middleware.house"

// DefaultPids caps the processes in a site's container when the manifest
// does not: a fork bomb inside stops there instead of exhausting the
// machine's pid space. Generous for any web process and its children.
const DefaultPids = 1024

// Start implements Runtime.
//
// What makes the container transparent rather than a second way of
// writing a manifest is mounting each folder at the same absolute path it
// has outside: $SITE_DIR, a relative ./server, and the site's own HOME mean
// exactly the same inside and out, and the command is not touched. V2's
// first draft mounted the folder at /site while $SITE_DIR still said the
// host path, so any command that used it pointed at nothing.
//
// The walls are not the manifest's to choose: no capabilities, no new
// privileges, no restart policy (the engine decides restarts, with its
// backoff; two supervisors over one container make a dead site look
// alive), and the port published on loopback only.
func (d Docker) Start(ctx context.Context, spec *manifest.Spec, port int, logFile, home string) (Instance, error) {
	if spec.Docker == nil {
		return nil, errors.New("docker runtime needs a docker section")
	}
	image := spec.Docker.Image
	if spec.Docker.Build != "" {
		image = "middleware/" + dockerName(spec.Domain) + ":" + spec.Revision
		if spec.Service != "" {
			if err := d.buildService(ctx, spec, image, logFile); err != nil {
				return nil, err
			}
		} else {
			build := exec.CommandContext(ctx, d.bin(), "build", "-q", "-t", image, "-f", filepath.Join(spec.SiteDir, spec.Docker.Build), spec.SiteDir)
			if out, err := build.CombinedOutput(); err != nil {
				return nil, fmt.Errorf("docker build: %w: %s", err, bytes.TrimSpace(out))
			}
		}
	}
	if err := d.checkArch(ctx, image); err != nil {
		return nil, err
	}
	// Inside a service's container the port is its own (docker.port, or
	// the house's default): the published one moves with every
	// generation, and the site's other containers reach this one by name
	// on the port inside.
	inside := port
	if spec.ContainerPort != 0 {
		inside = spec.ContainerPort
	}
	argv, env := spec.WithPort(inside)
	name := ContainerName(spec.Domain, port)
	// A leftover with this name from an earlier life would make the run
	// fail, and the site would not come back over a scrap. But a name is
	// not unique per site (dockerName folds dots and slashes into one
	// dash), and a worker's port is written in its manifest: the
	// container under this name can be another site's, running, and
	// removing it would be stopping a neighbour's database.
	if owner := d.containerSite(ctx, name); owner != "" && owner != spec.Domain {
		return nil, fmt.Errorf("the container %s is %s's: its name and port collide with this one's; change the port", name, owner)
	}
	_ = exec.CommandContext(ctx, d.bin(), "rm", "-f", name).Run()
	args := []string{"run", "-d", "--name", name,
		"--label", domainLabel + "=" + spec.Domain,
		"--label", "middleware.revision=" + spec.Revision}
	if !spec.Unpublished {
		args = append(args, "-p", fmt.Sprintf("127.0.0.1:%d:%d", port, inside))
	}
	if spec.Network != "" {
		if err := d.ensureNetwork(ctx, spec.Network, siteOf(spec.Domain)); err != nil {
			return nil, err
		}
		args = append(args, "--network", spec.Network, "--network-alias", spec.Service)
	}
	if d.House != "" {
		args = append(args, "--label", HouseLabel+"="+d.House)
	}
	walls, err := d.walls(spec)
	if err != nil {
		return nil, err
	}
	args = append(args, walls...)
	if err := d.claimVolumes(ctx, spec); err != nil {
		return nil, err
	}
	mounts, err := siteMounts(spec, home)
	if err != nil {
		return nil, err
	}
	args = append(args, mounts...)
	args = append(args, "-w", spec.Cwd)
	for _, kv := range containerEnv(env, inside, spec.Domain, spec.SiteDir, home) {
		args = append(args, "-e", kv)
	}
	args = append(args, image)
	args = append(args, argv...)
	// Output and errors apart, and the id is the last line of the output:
	// together, the first run of an image the machine did not have yet put
	// the pull's progress in front of the id. V1 learned it with an
	// orphan container that held its port.
	var stdout, stderr bytes.Buffer
	run := exec.CommandContext(ctx, d.bin(), args...)
	run.Stdout, run.Stderr = &stdout, &stderr
	if err := run.Run(); err != nil {
		return nil, fmt.Errorf("docker run: %w: %s", err, bytes.TrimSpace(append(stderr.Bytes(), stdout.Bytes()...)))
	}
	inst := &dockerInstance{d: d, name: name, port: port, revision: spec.Revision, domain: spec.Domain, logFile: logFile, started: time.Now(), done: make(chan error, 1), id: lastLine(stdout.String())}
	go inst.watch()
	go inst.tailLogs("")
	return inst, nil
}

// ContainerName is unique per domain and port, so two generations of a
// handover never collide.
func ContainerName(domain string, port int) string {
	return fmt.Sprintf("mw-%s-%d", dockerName(domain), port)
}

// dockerName is a site's name, or a worker's ("shop.example/db"), as
// docker accepts it in a container's or a volume's name: no dots, no
// slash. A worker's slash made docker refuse the run.
func dockerName(domain string) string {
	return strings.NewReplacer(".", "-", "/", "-").Replace(domain)
}

// walls are the flags every container of a site gets, from Start and from
// Exec alike.
func (d Docker) walls(spec *manifest.Spec) ([]string, error) {
	dk := spec.Docker
	args := []string{"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--restart", "no"}
	user := dk.User
	if user == "" {
		user = d.defaultUser()
	}
	if user != "" {
		args = append(args, "--user", user)
	}
	if dk.Memory != "" {
		args = append(args, "--memory", dk.Memory)
	}
	pids := dk.Pids
	if pids == 0 {
		pids = DefaultPids
	}
	args = append(args, "--pids-limit", strconv.Itoa(pids))
	if dk.CPUs > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(dk.CPUs, 'f', -1, 64))
	}
	if dk.CPUWeight > 0 {
		args = append(args, "--cpu-shares", strconv.Itoa(dk.CPUWeight))
	}
	for _, v := range dk.Volumes {
		mount, err := volumeArg(spec, v)
		if err != nil {
			return nil, err
		}
		args = append(args, "-v", mount)
	}
	return args, nil
}

// defaultUser is who the process is inside, which depends on the daemon,
// and is the opposite of what it looks like (V1 measured it): with a
// rootful daemon the uid inside is the uid outside, so the supervisor's
// uid:gid is passed or what the site writes would belong to root; with a
// rootless daemon uid 0 inside is the supervisor's user outside, and the
// supervisor's uid inside lands on a subuid that cannot write its own
// folder. So rootless gets no --user at all.
func (d Docker) defaultUser() string {
	out, err := exec.Command(d.bin(), "info", "--format", "{{.SecurityOptions}}").Output()
	if err == nil && strings.Contains(string(out), "name=rootless") {
		return ""
	}
	return fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
}

// siteMounts are the folders the site sees, and only those: its own,
// at the path its link has (the path $SITE_DIR expands to) with the
// target resolved, and its HOME.
func siteMounts(spec *manifest.Spec, home string) ([]string, error) {
	real, err := filepath.EvalSymlinks(spec.SiteDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	out := []string{"-v", real + ":" + spec.SiteDir, "-v", home + ":" + home}
	// The manifest goes in read-only, over the folder's own mount. The
	// folder is the container's to write, and the manifest is what the
	// house reads on the machine at its next tick: a container that could
	// rewrite it would set `runtime: "process"` and a command of its own,
	// and the house would run that outside the walls, as its own user.
	// Bound as a file, it is a mount point inside: neither written,
	// removed nor renamed from there.
	mf := filepath.Join(real, manifest.FileName)
	switch info, err := os.Lstat(mf); {
	case err == nil && info.Mode().IsRegular():
		out = append(out, "-v", mf+":"+filepath.Join(spec.SiteDir, manifest.FileName)+":ro")
	case err == nil:
		// A link would stay the container's to replace, whatever is
		// mounted where it points.
		return nil, fmt.Errorf("%s is not a plain file: a site in a container needs its manifest to be one", manifest.FileName)
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	for _, m := range spec.Mounts {
		out = append(out, "-v", m+":"+m)
	}
	return out, nil
}

// containerEnv is buildEnv for inside a container: the image's PATH, not
// the host's, and HOST=0.0.0.0, because inside its own network namespace
// a process bound to 127.0.0.1 is unreachable from the published port.
// What is reachable from outside is still loopback only: -p 127.0.0.1.
func containerEnv(env map[string]string, port int, domain, siteDir, home string) []string {
	var out []string
	for _, kv := range buildEnv(env, port, domain, siteDir, home) {
		switch {
		case strings.HasPrefix(kv, "PATH="):
			continue
		case strings.HasPrefix(kv, "HOST="):
			out = append(out, "HOST=0.0.0.0")
		default:
			out = append(out, kv)
		}
	}
	return out
}

// Adopt implements Runtime.
func (d Docker) Adopt(ctx context.Context, st State) (Instance, error) {
	if st.Container == "" {
		return nil, ErrGone
	}
	out, err := exec.CommandContext(ctx, d.bin(), "inspect", "-f", "{{.State.Running}} {{.Id}} {{index .Config.Labels \""+domainLabel+"\"}}", st.Container).Output()
	f := strings.Fields(string(out))
	if err != nil || len(f) < 2 || f[0] != "true" {
		return nil, ErrGone
	}
	id := f[1]
	// The name in the state file is found by name, and another site's can
	// have it (see Start): adopting it would route this site's traffic to
	// the neighbour's container and stop it on the next handover.
	if len(f) > 2 && f[2] != st.Domain {
		return nil, ErrGone
	}
	inst := &dockerInstance{d: d, name: st.Container, port: st.Port, revision: st.Revision, domain: st.Domain, logFile: st.LogFile, started: st.StartedAt, done: make(chan error, 1), id: id}
	go inst.watch()
	// From now on only: the log file already has what came before, and
	// replaying it would double every line. Without this, an adopted
	// container's log simply stopped.
	go inst.tailLogs("1s")
	return inst, nil
}

func (c *dockerInstance) Port() int          { return c.port }
func (c *dockerInstance) Revision() string   { return c.revision }
func (c *dockerInstance) Done() <-chan error { return c.done }

func (c *dockerInstance) Alive() bool {
	out, err := exec.Command(c.d.bin(), "inspect", "-f", "{{.State.Running}}", c.name).Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

func (c *dockerInstance) State() State {
	return State{Domain: c.domain, Runtime: "docker", Port: c.port, Revision: c.revision, Container: c.name, ContainerID: c.id, LogFile: c.logFile, StartedAt: c.started}
}

// Stop does `docker stop` with the grace as its timeout, then removes the
// container so the name is free for the next generation.
func (c *dockerInstance) Stop(ctx context.Context, grace time.Duration) error {
	secs := int(grace.Seconds())
	if secs < 1 {
		secs = 1
	}
	_ = exec.CommandContext(ctx, c.d.bin(), "stop", "-t", fmt.Sprint(secs), c.name).Run()
	_ = exec.CommandContext(ctx, c.d.bin(), "rm", "-f", c.name).Run()
	return nil
}

// watch blocks on `docker wait`, which returns when the container exits.
func (c *dockerInstance) watch() {
	err := exec.Command(c.d.bin(), "wait", c.name).Run()
	if err == nil {
		err = errors.New("container exited")
	}
	c.done <- err
	close(c.done)
}

// tailLogs copies the container's output into the site's log file so the
// panel shows one log per site whatever the runtime. since, when set, is
// passed to docker logs --since.
func (c *dockerInstance) tailLogs(since string) {
	if c.logFile == "" {
		return
	}
	// Its folder made first, as the process runtime does: a site whose only
	// process is a container worker (a static site and its database) had
	// no logs folder yet, the open failed in silence, and the worker's log
	// stayed empty however much the container said.
	_ = os.MkdirAll(filepath.Dir(c.logFile), 0o755)
	f, err := os.OpenFile(c.logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	args := []string{"logs", "-f"}
	if since != "" {
		args = append(args, "--since", since)
	}
	cmd := exec.Command(c.d.bin(), append(args, c.name)...)
	cmd.Stdout = f
	cmd.Stderr = f
	_ = cmd.Run()
}

// volumeArg rewrites a manifest volume for docker run. Host paths are made
// absolute against the site folder (the manifest check has already kept
// them inside it), and named volumes get the site in front, because docker
// volume names are global: two sites writing "data" would otherwise share
// one volume and read each other's files.
func volumeArg(spec *manifest.Spec, v string) (string, error) {
	src, err := manifest.VolumeSource(v)
	if err != nil {
		return "", err
	}
	switch {
	case src.Named && src.Name == "":
		return src.Rest, nil
	case src.Named:
		// The site's, a worker's included: the volume is the site's data,
		// whichever of its processes mounts it.
		site, _, _ := strings.Cut(spec.Domain, "/")
		return NamedVolume(site, src.Name) + src.Rest, nil
	}
	host := src.Path
	if !filepath.IsAbs(host) {
		host = filepath.Join(spec.SiteDir, host)
	}
	return filepath.Clean(host) + src.Rest, nil
}

// lastLine is the last non-empty line of out: the id docker run -d prints
// after whatever pull progress came first.
func lastLine(out string) string {
	lines := strings.Fields(out)
	if len(lines) == 0 {
		return ""
	}
	return lines[len(lines)-1]
}

// NamedVolume is the docker volume a site's named volume is.
//
// It is not unique per site: my-shop.com and my.shop.com are both
// mw-my-shop-com-data. Renaming would orphan the data every installation
// already keeps under these names, so the name stays and the volume says
// whose it is instead (ClaimVolume): a second site that maps to it is
// refused, not handed the first one's database.
func NamedVolume(site, name string) string { return "mw-" + dockerName(site) + "-" + name }

// domainLabel names the site, or the worker, a container runs.
const domainLabel = "middleware.domain"

// SiteLabel names the site a named volume belongs to.
const SiteLabel = "middleware.site"

// VolumeClaimsFile, under the house's data folder, names the owner of
// each volume that was made before volumes carried SiteLabel: docker
// cannot add a label to a volume that exists, and the data in it is not
// the house's to move to a new one.
const VolumeClaimsFile = "docker-volumes.json"

// claimsMu keeps two sites starting at once from both reading the claims
// file before either wrote it. Package-wide because Docker is a value
// and every copy writes the same file.
var claimsMu sync.Mutex

// containerSite is the domain the container under name was started for,
// empty when there is none or it carries no label.
func (d Docker) containerSite(ctx context.Context, name string) string {
	out, err := exec.CommandContext(ctx, d.bin(), "inspect", "-f", "{{index .Config.Labels \""+domainLabel+"\"}}", name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// claimVolumes claims, before the run, every named volume the spec
// mounts: left to docker run, a volume that is not there is made without
// a word about whose it is, and one that is there is mounted whoever's
// it is.
func (d Docker) claimVolumes(ctx context.Context, spec *manifest.Spec) error {
	site, _, _ := strings.Cut(spec.Domain, "/")
	for _, v := range spec.Docker.Volumes {
		src, err := manifest.VolumeSource(v)
		if err != nil {
			return err
		}
		if !src.Named || src.Name == "" {
			continue
		}
		if err := d.ClaimVolume(ctx, site, NamedVolume(site, src.Name)); err != nil {
			return err
		}
	}
	return nil
}

// ClaimVolume makes sure the volume is site's before a container of it
// mounts it. One that is not there is made labelled with the site; one
// labelled with another site is refused; one with no label, from before
// labels, goes to the first site that claims it, written down in the
// claims file, and is refused to any other after that.
func (d Docker) ClaimVolume(ctx context.Context, site, volume string) error {
	owner, exists := d.volumeOwner(ctx, volume)
	if !exists {
		// Read again after the create rather than trusting it: docker
		// answers a create of a name that is there with success and leaves
		// that volume as it was, labels and all, so one made in between
		// by another site must still be seen as the other site's.
		out, err := exec.CommandContext(ctx, d.bin(), "volume", "create", "--label", SiteLabel+"="+site, volume).CombinedOutput()
		if owner, exists = d.volumeOwner(ctx, volume); !exists {
			return fmt.Errorf("docker volume create %s: %v: %s", volume, err, bytes.TrimSpace(out))
		}
	}
	if owner == site {
		return nil
	}
	if owner != "" {
		return volumeTaken(volume, site, owner)
	}
	if d.Data == "" {
		return nil
	}
	claimsMu.Lock()
	defer claimsMu.Unlock()
	path := filepath.Join(d.Data, VolumeClaimsFile)
	claims := map[string]string{}
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &claims); err != nil {
			// Unreadable, every unlabelled volume would be anybody's.
			return fmt.Errorf("%s: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	switch claims[volume] {
	case site:
		return nil
	case "":
		claims[volume] = site
		return fsx.WriteJSON(path, claims, 0o600)
	default:
		return volumeTaken(volume, site, claims[volume])
	}
}

func volumeTaken(volume, site, owner string) error {
	return fmt.Errorf("the docker volume %s is %s's, not %s's: both names map to it, and a site is never handed another's data; rename the volume in %s's manifest", volume, owner, site, site)
}

// volumeOwner is the site a volume's label names, and whether the volume
// is there at all. A daemon that does not answer reads as "not there",
// and the create that follows says what is wrong.
func (d Docker) volumeOwner(ctx context.Context, volume string) (string, bool) {
	out, err := exec.CommandContext(ctx, d.bin(), "volume", "inspect", "--format", "{{json .Labels}}", volume).Output()
	if err != nil {
		return "", false
	}
	var labels map[string]string
	_ = json.Unmarshal(bytes.TrimSpace(out), &labels)
	return labels[SiteLabel], true
}

// Reap removes the containers of this house that nobody adopted: a
// generation whose supervisor died between "run" and "saved", a handover
// cut halfway. They hold a port and memory and serve nothing. keep is the
// names the engine adopted.
func (d Docker) Reap(ctx context.Context, keep map[string]bool) (removed []string) {
	if d.House == "" {
		return nil
	}
	out, err := exec.CommandContext(ctx, d.bin(), "ps", "-a", "--filter", "label="+HouseLabel+"="+d.House, "--format", "{{.Names}}").Output()
	if err != nil {
		return nil
	}
	for _, name := range strings.Fields(string(out)) {
		if keep[name] {
			continue
		}
		if exec.CommandContext(ctx, d.bin(), "rm", "-f", name).Run() == nil {
			removed = append(removed, name)
		}
	}
	return removed
}

// VolumeExists says whether docker has a volume by that name.
func (d Docker) VolumeExists(ctx context.Context, name string) bool {
	return exec.CommandContext(ctx, d.bin(), "volume", "inspect", name).Run() == nil
}

// RemoveVolume removes a volume; one that is not there is not an error.
func (d Docker) RemoveVolume(ctx context.Context, name string) error {
	if !d.VolumeExists(ctx, name) {
		return nil
	}
	out, err := exec.CommandContext(ctx, d.bin(), "volume", "rm", "-f", name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker volume rm %s: %w: %s", name, err, bytes.TrimSpace(out))
	}
	return nil
}

// ExecIn runs a command inside a running container of this house, as the
// container's user and behind its walls (docker exec keeps the dropped
// capabilities and no-new-privileges), in cwd, with spec's environment and
// PORT. It is how a move reaches a worker's database without giving the
// command the docker socket: a command written in a site's manifest with
// docker in its hands would own the machine.
// stdout nil is the log (out), with its errors.
func (d Docker) ExecIn(ctx context.Context, container string, spec *manifest.Spec, port int, home string, out, stdout *os.File) error {
	// The command with $PORT in it expanded too, not only the environment:
	// ["pg_isready", "-p", "$PORT"] got "$PORT" as its port, and a move
	// waited three minutes for a database that was up.
	argv, env := spec.WithPort(port)
	args := []string{"exec", "-i", "-w", spec.Cwd}
	for _, kv := range containerEnv(env, port, spec.Domain, spec.SiteDir, home) {
		args = append(args, "-e", kv)
	}
	args = append(args, container)
	args = append(args, argv...)
	cmd := exec.CommandContext(ctx, d.bin(), args...)
	cmd.Stdout, cmd.Stderr = out, out
	if stdout != nil {
		cmd.Stdout = stdout
	}
	cmd.WaitDelay = 5 * time.Second
	return cmd.Run()
}

// ExecStdio runs argv inside a running container as its own user, with
// env added to the container's own environment, stdin read and stdout
// written as they are: a backup's dump, whose output the house compresses
// itself, and its restore. No -w: a database's container does not have
// the site's folder, and docker exec refuses a directory that is not
// there.
//
// A cancel kills the docker client, not what it started inside the
// container, which docker exec has no way to reach; the dump inside then
// ends on the closed pipe it writes to.
func (d Docker) ExecStdio(ctx context.Context, container string, env, argv []string, stdin io.Reader, stdout, stderr io.Writer) error {
	args := []string{"exec", "-i"}
	for _, kv := range env {
		args = append(args, "-e", kv)
	}
	args = append(args, container)
	args = append(args, argv...)
	cmd := exec.CommandContext(ctx, d.bin(), args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	cmd.WaitDelay = 5 * time.Second
	return cmd.Run()
}
