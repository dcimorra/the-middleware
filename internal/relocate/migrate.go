package relocate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/runtime"
)

// A worker that keeps its data outside the folder (a database in a named
// volume) says in its manifest how that data moves: move.export where the
// site is, with the site stopped, leaving a dump inside the folder that
// the last copy carries; and on the target, before the commit, the worker
// brought up on its own, move.ready until it answers, move.import, and the
// worker stopped again. The site is then put in place and starts on a
// database that already has its data. Where it was, the database has only
// been read, so undoing the move is starting the site there again.

// Exporter is a Source that runs its site's workers' move.export.
// workers are the ones to export now: a move takes the live ones while the
// site serves and the others once it is held.
type Exporter interface {
	Export(ctx context.Context, domain string, manifestData []byte, workers []string) (string, error)
}

// Migrator is a target that brings the staged site's workers up and runs
// their move.ready and move.import before the commit.
// workers are the ones to import now, as Exporter's.
type Migrator interface {
	Migrate(ctx context.Context, domain string, manifestData []byte, workers []string) (string, error)
}

// Planner is a Source that says which manifest the move will commit
// before it stops the site (a V1 one, converted or its site.v2.json).
type Planner interface {
	Planned(domain string) []byte
}

// moveTime bounds each move command when the manifest says nothing.
const moveTime = 30 * time.Minute

// readyTime bounds the wait for a worker brought up for its import.
const readyTime = 3 * time.Minute

// MovingWorkers are the names of the workers whose data moves by command,
// live or not.
func MovingWorkers(m *manifest.Manifest) []string {
	out := append(m.MovingWorkers(true), m.MovingWorkers(false)...)
	sort.Strings(out)
	return out
}

// UsesDocker says the site, or one of its workers, runs in a container.
func UsesDocker(m *manifest.Manifest) bool {
	if m == nil {
		return false
	}
	if m.Run != nil && m.Run.Runtime == "docker" {
		return true
	}
	for _, w := range m.Workers {
		if w != nil && !w.Disabled && w.Runtime == "docker" {
			return true
		}
	}
	// A service's container too: a site with services has no run, and
	// was moved to a node without docker as if it needed none.
	for _, sv := range m.Services {
		if sv != nil && !sv.Disabled && sv.Run != nil && sv.Run.Runtime == "docker" {
			return true
		}
	}
	return false
}

func moveTimeout(mv *manifest.WorkerMove) time.Duration {
	if mv.Timeout > 0 {
		return time.Duration(mv.Timeout)
	}
	return moveTime
}

// commandSpec is a move command as the house runs any command of a site:
// in its folder, with its environment (the .env that says where its
// database is) and the worker's on top, and $PORT the worker's.
func commandSpec(domain, dir string, m *manifest.Manifest, name string, cmd []string) *manifest.Spec {
	spec := &manifest.Spec{Domain: domain + "/" + name + "/move", SiteDir: dir, Cwd: dir, Env: map[string]string{}, Runtime: "process"}
	if m.Run != nil {
		if site, err := manifest.Resolve(domain, dir, m); err == nil {
			for k, v := range site.Env {
				spec.Env[k] = v
			}
		}
	}
	for k, v := range manifest.WorkerSpec(domain, dir, name, m.Workers[name]).Env {
		spec.Env[k] = v
	}
	vars := map[string]string{"DOMAIN": domain, "SITE_DIR": dir}
	spec.Command = make([]string, len(cmd))
	for i, a := range cmd {
		spec.Command[i] = manifest.Expand(a, vars)
	}
	spec.Rehash()
	return spec
}

// runCommand runs one move command to its end, its output in logFile:
// inside container when it is set (a docker worker's), otherwise as a
// process of the site, as a task runs.
//
// output, when set, is a file inside the site's folder that gets the
// command's standard output (move.output), written by the house.
func runCommand(ctx context.Context, d runtime.Docker, container string, spec *manifest.Spec, port int, logFile, home string, timeout time.Duration, output string) error {
	if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	argv, env := spec.WithPort(port)
	what := strings.Join(argv, " ")
	if container != "" || output != "" {
		out, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		defer out.Close()
		dump, finish, err := openOutput(spec.SiteDir, output)
		if err != nil {
			return err
		}
		cctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		if container != "" {
			err = d.ExecIn(cctx, container, spec, port, home, out, dump)
		} else {
			// A process worker's export with an output: run here, its
			// standard output to the file, as the site's processes run.
			e := []string{"HOME=" + home, "PORT=" + strconv.Itoa(port)}
			for _, k := range []string{"PATH", "LANG", "LC_ALL", "TZ"} {
				if v, ok := os.LookupEnv(k); ok {
					e = append(e, k+"="+v)
				}
			}
			for k, v := range env {
				e = append(e, k+"="+v)
			}
			err = runExec(cctx, argv, spec.Cwd, e, out, dump)
		}
		if err == nil {
			err = finish()
		} else {
			abandon(dump)
		}
		if err != nil {
			if cctx.Err() == context.DeadlineExceeded {
				return fmt.Errorf("%s: cut after %s (its output is in %s)", what, timeout, logFile)
			}
			return fmt.Errorf("%s: %w (its output is in %s)%s", what, err, logFile, logTail(logFile))
		}
		return nil
	}
	inst, err := runtime.Process{}.Start(ctx, spec, port, logFile, home)
	if err != nil {
		return err
	}
	select {
	case err := <-inst.Done():
		if err != nil {
			return fmt.Errorf("%s: %w (its output is in %s)%s", what, err, logFile, logTail(logFile))
		}
		return nil
	case <-time.After(timeout):
		_ = inst.Stop(context.Background(), 5*time.Second)
		return fmt.Errorf("%s: cut after %s (its output is in %s)", what, timeout, logFile)
	case <-ctx.Done():
		_ = inst.Stop(context.Background(), 5*time.Second)
		return ctx.Err()
	}
}

// runAsOperator runs a source's move.export as the person who ran the
// import, in their environment with the site's on top: the database it
// reads is one that person's machine runs (a V1 site's container, which no
// house manages), reached however they reach it.
func runAsOperator(ctx context.Context, spec *manifest.Spec, port int, logFile string, timeout time.Duration, output string) error {
	if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	argv, _ := spec.WithPort(port)
	env := os.Environ()
	for k, v := range spec.Env {
		env = append(env, k+"="+v)
	}
	if port != 0 {
		env = append(env, "PORT="+strconv.Itoa(port))
	}
	dump, finish, err := openOutput(spec.SiteDir, output)
	if err != nil {
		return err
	}
	err = runExec(cctx, argv, spec.Cwd, env, out, dump)
	if err == nil {
		err = finish()
	} else {
		abandon(dump)
	}
	if err != nil {
		what := strings.Join(argv, " ")
		if cctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("%s: cut after %s (its output is in %s)", what, timeout, logFile)
		}
		return fmt.Errorf("%s: %w (its output is in %s)%s", what, err, logFile, logTail(logFile))
	}
	return nil
}

// runExec runs argv in dir with env to its end, its errors (and its output
// when stdout is nil) in log. Its own group, killed whole when it is cut:
// pg_dump under a shell outlives a kill of the shell alone.
func runExec(ctx context.Context, argv []string, dir string, env []string, log, stdout *os.File) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = log, log
	if stdout != nil {
		cmd.Stdout = stdout
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	return cmd.Run()
}

// openOutput makes the file a command's standard output goes to, rel
// inside dir; nil when rel is "". It is written beside, as .part, and
// finish puts it in place: a dump cut halfway is never taken for one.
// A command that fails has its .part removed (dropOutput): it was left in
// the folder, and went with the site on its next move.
//
// rel absolute is a file of the house's, outside any site (a rehearsal's).
// Inside a site whose folder is a git repository, the dump is kept out of
// it first: the github module commits and pushes the folder, and a
// database's dump on GitHub is its people's data in public.
func openOutput(dir, rel string) (*os.File, func() error, error) {
	if rel == "" {
		return nil, func() error { return nil }, nil
	}
	final := filepath.Join(dir, filepath.FromSlash(rel))
	if filepath.IsAbs(rel) {
		final = rel
	} else if err := ExcludeFromGit(dir, rel); err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return nil, nil, err
	}
	f, err := os.OpenFile(final+".part", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, nil, err
	}
	return f, func() error {
		if err := f.Close(); err != nil {
			return err
		}
		return os.Rename(final+".part", final)
	}, nil
}

// abandon closes a dump whose command failed and removes it.
func abandon(dump *os.File) {
	if dump != nil {
		dump.Close()
		_ = os.Remove(dump.Name())
	}
}

// DropOutputs removes the dumps the workers' exports wrote in dir
// (move.output, and its .part), for a move that is over: done there, the
// dump is the database's people's data lying in a folder that travels on
// every later move and that the github module may push; undone, it is
// that and nothing else. An export that writes its own files (no output)
// is the site's to clean, as the house does not know their names.
func DropOutputs(dir string, m *manifest.Manifest, workers []string) {
	if m == nil {
		return
	}
	for _, name := range workers {
		w := m.Workers[name]
		if w == nil || w.Move == nil || w.Move.Output == "" {
			continue
		}
		p := filepath.Join(dir, filepath.FromSlash(w.Move.Output))
		_ = os.Remove(p)
		_ = os.Remove(p + ".part")
	}
}

// ExcludeFromGit adds rel, and its .part, to the git repository's own
// exclude file when dir is one (.git/info/exclude, never the site's
// .gitignore, which is the site's and would travel with it).
func ExcludeFromGit(dir, rel string) error {
	info, err := os.Stat(filepath.Join(dir, ".git"))
	if err != nil || !info.IsDir() {
		return nil
	}
	path := filepath.Join(dir, ".git", "info", "exclude")
	raw, _ := os.ReadFile(path)
	have := map[string]bool{}
	for _, l := range strings.Split(string(raw), "\n") {
		have[strings.TrimSpace(l)] = true
	}
	var add []string
	for _, p := range []string{"/" + rel, "/" + rel + ".part"} {
		if !have[p] {
			add = append(add, p)
		}
	}
	if len(add) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	text := string(raw)
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	text += "# a move's database dump: never committed\n" + strings.Join(add, "\n") + "\n"
	return fsx.WriteAtomic(path, []byte(text), 0o644)
}

// logTail is the end of a failed command's log, for its error: the log is
// on the target, and the person running an import on the source only sees
// the error. hogarify's import failed with "exit status 1" and the reason,
// a "Permission denied" two lines up, was on another machine.
func logTail(logFile string) string {
	raw, err := os.ReadFile(logFile)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) > 5 {
		lines = lines[len(lines)-5:]
	}
	return "; it ended with: " + strings.Join(lines, " | ")
}

// workerPort is the worker's written port, or a free one for the time of
// the move when it has none.
func workerPort(w *manifest.Worker) (int, error) {
	if w.Port != 0 {
		return w.Port, nil
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// Export runs each moving worker's move.export in dir, as the person who
// runs the import. root is where its log goes (data/logs).
func Export(ctx context.Context, root, dir, domain string, m *manifest.Manifest, workers []string) (string, error) {
	var said []string
	for _, name := range workers {
		w := m.Workers[name]
		port := w.Port
		logFile := filepath.Join(root, "data", "logs", domain, "move-"+name+".log")
		spec := commandSpec(domain, dir, m, name, w.Move.Export)
		if err := runAsOperator(ctx, spec, port, logFile, moveTimeout(w.Move), w.Move.Output); err != nil {
			return "", fmt.Errorf("%s: export: %w", name, err)
		}
		said = append(said, name+": exported")
	}
	return strings.Join(said, " · "), nil
}

// ExportHere runs workers' move.export where the site is a site of this
// house (root): inside each worker's running container, as the house runs
// a target's imports, or as the site's processes run for a process worker.
// The worker must be up: StopForMove holds the site and keeps it.
func ExportHere(ctx context.Context, root, siteDir, domain string, m *manifest.Manifest, workers []string) (string, error) {
	var said []string
	data := filepath.Join(root, "data")
	for _, name := range workers {
		w := m.Workers[name]
		if w == nil || w.Move == nil {
			return "", fmt.Errorf("%s: not a worker whose data moves by command", name)
		}
		var st runtime.State
		if err := fsx.ReadJSON(runtime.StatePath(data, domain+"/"+name), &st); err != nil || (st.Container == "" && st.PID == 0) {
			return "", fmt.Errorf("%s: not running here, so its data cannot be exported", name)
		}
		logFile := filepath.Join(data, "logs", domain, "move-"+name+".log")
		spec := commandSpec(domain, siteDir, m, name, w.Move.Export)
		if err := runCommand(ctx, runtime.Docker{}, st.Container, spec, st.Port, logFile, filepath.Join(data, "homes", domain), moveTimeout(w.Move), w.Move.Output); err != nil {
			return "", fmt.Errorf("%s: export: %w", name, err)
		}
		said = append(said, name+": exported")
	}
	return strings.Join(said, " · "), nil
}

// Planned implements Planner.
func (s *V1Source) Planned(string) []byte { return s.converted }

// Export implements Exporter: with the V1 site stopped, against the
// database it was using.
func (s *V1Source) Export(ctx context.Context, domain string, manifestData []byte, workers []string) (string, error) {
	m, err := manifest.Parse(manifestData)
	if err != nil {
		return "", err
	}
	return Export(ctx, s.Root, filepath.Join(s.Root, "sites", domain), domain, m, workers)
}

// volumesFile lists the volumes a migration created in a staging, so a
// move that is undone takes them out with the rest.
func (r Receiver) volumesFile(domain string) string {
	return filepath.Join(r.staging(domain), volumesName)
}

const volumesName = "volumes.json"

func (r Receiver) docker() runtime.Docker {
	if r.Docker != nil {
		return *r.Docker
	}
	return runtime.Docker{}
}

// Migrate brings each moving worker of the staged site up on its own, waits
// for it (its port, then move.ready), runs move.import, and stops it. The
// named volumes it creates are written down first: a migration that fails
// halfway leaves nothing behind once the move is undone.
// workers nil is every moving worker.
func (r Receiver) Migrate(ctx context.Context, domain string, manifestData []byte, workers []string) (string, error) {
	if !manifest.ValidName(domain) {
		return "", ErrBadName
	}
	dir := r.StagingSite(domain)
	if !fsx.Exists(dir) {
		return "", ErrNoStaging
	}
	m, err := manifest.Parse(manifestData)
	if err != nil {
		return "", fmt.Errorf("the manifest sent with the move: %w", err)
	}
	if workers == nil {
		workers = MovingWorkers(m)
	}
	for _, name := range workers {
		if w := m.Workers[name]; w == nil || w.Move == nil {
			return "", fmt.Errorf("%s: not a worker whose data moves by command", name)
		}
	}
	// The dumps go once their import ends, whichever way: left in the
	// folder, a database's whole dump was put in place with the site,
	// travelled with it on every later move, and sat in its git working
	// tree for the github module to push. Kept out of git meanwhile.
	defer DropOutputs(dir, m, workers)
	for _, name := range workers {
		if out := m.Workers[name].Move.Output; out != "" {
			if err := ExcludeFromGit(dir, out); err != nil {
				return "", err
			}
		}
	}
	d := r.docker()
	// Added to what an earlier call of this move recorded (the live
	// workers', imported while the site still served): written over, those
	// volumes would stay behind a move that is undone.
	var volumes []string
	if raw, err := os.ReadFile(r.volumesFile(domain)); err == nil {
		_ = json.Unmarshal(raw, &volumes)
	}
	for _, name := range workers {
		w := m.Workers[name]
		if w.Docker == nil {
			continue
		}
		for _, v := range w.Docker.Volumes {
			src, err := manifest.VolumeSource(v)
			if err != nil || !src.Named || src.Name == "" {
				continue
			}
			vol := runtime.NamedVolume(domain, src.Name)
			// One already here is data of an earlier life of the name, or
			// of an attempt that was not cleaned: importing over it would
			// mix the two, and removing it is not a move's decision.
			if d.VolumeExists(ctx, vol) {
				return "", fmt.Errorf("%s: the volume %s is already on this node (an earlier attempt, or a site that had this name): remove it with docker volume rm %s and move again", name, vol, vol)
			}
			volumes = append(volumes, vol)
		}
	}
	if err := fsx.WriteJSON(r.volumesFile(domain), volumes, 0o644); err != nil {
		return "", err
	}
	home := filepath.Join(r.Root, "data", "homes", domain)
	var said []string
	for _, name := range workers {
		w := m.Workers[name]
		port, err := workerPort(w)
		if err != nil {
			return "", err
		}
		logFile := filepath.Join(r.Root, "data", "logs", domain, "move-"+name+".log")
		spec := manifest.WorkerSpec(domain, dir, name, w)
		var rt runtime.Runtime = runtime.Process{}
		if spec.Runtime == "docker" {
			rt = d
		}
		if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
			return "", err
		}
		if err := os.MkdirAll(home, 0o700); err != nil {
			return "", err
		}
		inst, err := rt.Start(ctx, spec, port, logFile, home)
		if err != nil {
			return "", fmt.Errorf("%s: bringing it up for its import: %w", name, err)
		}
		// A docker worker's commands run inside its container: the machine
		// need not have the database's tools (the image has them), and the
		// command never holds the docker socket.
		container := ""
		if spec.Runtime == "docker" {
			container = runtime.ContainerName(spec.Domain, port)
		}
		cmdSpec := func(cmd []string) *manifest.Spec { return commandSpec(domain, dir, m, name, cmd) }
		err = func() error {
			if err := runtime.WaitReady(ctx, inst, manifest.Health{}, readyTime); err != nil {
				return fmt.Errorf("it did not answer on its port: %w (its output is in %s)", err, logFile)
			}
			if len(w.Move.Ready) > 0 {
				if err := r.untilReady(ctx, d, container, cmdSpec(w.Move.Ready), port, logFile, home); err != nil {
					return err
				}
			}
			if err := runCommand(ctx, d, container, cmdSpec(w.Move.Import), port, logFile, home, moveTimeout(w.Move), ""); err != nil {
				return fmt.Errorf("import: %w", err)
			}
			return nil
		}()
		// Stopped with time to close its files: a database killed after its
		// import comes up again replaying what it had not written.
		stopErr := inst.Stop(context.Background(), 60*time.Second)
		if err != nil {
			return "", fmt.Errorf("%s: %w", name, err)
		}
		if stopErr != nil {
			return "", fmt.Errorf("%s: stopping it after its import: %w", name, stopErr)
		}
		said = append(said, name+": imported")
	}
	return strings.Join(said, " · "), nil
}

// untilReady runs move.ready until it succeeds, for readyTime at most.
func (r Receiver) untilReady(ctx context.Context, d runtime.Docker, container string, spec *manifest.Spec, port int, logFile, home string) error {
	deadline := time.Now().Add(readyTime)
	for {
		err := runCommand(ctx, d, container, spec, port, logFile, home, time.Minute, "")
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ready: still failing after %s: %w", readyTime, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// removeVolumes takes out the volumes listed in path, if it is there.
func (r Receiver) removeVolumes(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var vols []string
	if err := json.Unmarshal(raw, &vols); err != nil {
		return err
	}
	return r.removeVolumeList(vols)
}

func (r Receiver) removeVolumeList(vols []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var errs []string
	for _, v := range vols {
		if err := r.docker().RemoveVolume(ctx, v); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}
