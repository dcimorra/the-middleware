// Package backups copies the sites' databases, as each site's manifest
// describes them (manifest.Backup), compressed, into
// data/backups/<domain>/<name>/, on their schedule or when asked, keeps
// the newest few, and puts one back when asked.
//
// The dump runs where the database is: inside the container of the
// service or worker that runs it, as that container's own user, or as the
// site's processes run (its folder, its environment, its jail). Its
// standard output comes back to the house, which compresses it on the way
// to the file: nothing needs gzip, or a folder it may write, where the
// dump runs. A copy is written beside its final name and renamed once
// whole, so a dump cut halfway is never listed as a copy.
//
// The copies are outside the site's folder on purpose: a database's dump
// is its people's data, and the folder is what the github module pushes,
// what a move carries and what the site's own code serves.
package backups

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/cron"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/runtime"
)

// Options configure a Keeper.
type Options struct {
	Root string
	Bus  *events.Bus
	// Now is the clock; tests move it.
	Now func() time.Time
	// Tick is how often the schedules are looked at; a minute by default.
	Tick time.Duration
	// ServiceSpec is a service's running spec (the engine's), for a
	// database in a service that runs as a process.
	ServiceSpec func(domain, service string) (*manifest.Spec, error)
	// Restart starts a site again after its SQLite file was replaced:
	// the process still holds the old one open.
	Restart func(ctx context.Context, domain string) error
	// Docker runs the commands that go inside a container.
	Docker runtime.Docker
	// Parallel is how many copies are taken at once (2 when 0): every
	// site's nightly copy at 03:00 is every database read at once.
	Parallel int
}

// Kinds of copy, by what made it.
const (
	KindScheduled  = "scheduled"
	KindManual     = "manual"
	KindPreRestore = "pre-restore"
)

// preRestoreKept is how many of the copies a restore takes first are
// kept, apart from the backup's own keep: a keep of 1 would otherwise
// remove the copy just restored from.
const preRestoreKept = 3

// Errors the API turns into answers.
var (
	ErrNotFound = errors.New("not found")
	ErrBusy     = errors.New("a copy or a restore of this backup is running")
	ErrInvalid  = errors.New("invalid")
)

// Copy is one file of a backup.
type Copy struct {
	ID    string    `json:"id"`
	At    time.Time `json:"at"`
	Bytes int64     `json:"bytes"`
	Kind  string    `json:"kind"`
}

// Result is how a copy or a restore ended.
type Result struct {
	At      time.Time `json:"at"`
	Trigger string    `json:"trigger"`
	OK      bool      `json:"ok"`
	Error   string    `json:"error,omitempty"`
	Seconds float64   `json:"seconds"`
	Bytes   int64     `json:"bytes,omitempty"`
	Copy    string    `json:"copy,omitempty"`
}

// record is data/backups/<domain>/<name>/state.json.
type record struct {
	Last        *Result `json:"last,omitempty"`
	LastRestore *Result `json:"last_restore,omitempty"`
}

// Status is one backup as the panel and the CLI list it.
type Status struct {
	Name        string           `json:"name"`
	Config      *manifest.Backup `json:"config"`
	Next        *time.Time       `json:"next,omitempty"`
	Running     string           `json:"running,omitempty"` // "backup" | "restore"
	Last        *Result          `json:"last,omitempty"`
	LastRestore *Result          `json:"last_restore,omitempty"`
	Copies      []Copy           `json:"copies"`
	Bytes       int64            `json:"bytes"`
	Error       string           `json:"error,omitempty"`
}

// Keeper takes, keeps and restores the copies.
type Keeper struct {
	opts      Options
	mu        sync.Mutex
	running   map[string]string
	lastCheck time.Time
	slots     chan struct{}
}

// New builds a keeper. Nothing is scheduled until Run.
func New(opts Options) *Keeper {
	if opts.Bus == nil {
		opts.Bus = events.New("")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Tick == 0 {
		opts.Tick = time.Minute
	}
	if opts.Parallel <= 0 {
		opts.Parallel = 2
	}
	if abs, err := filepath.Abs(opts.Root); err == nil {
		opts.Root = abs
	}
	return &Keeper{opts: opts, running: map[string]string{}, slots: make(chan struct{}, opts.Parallel)}
}

func (k *Keeper) data() string                 { return filepath.Join(k.opts.Root, "data") }
func (k *Keeper) siteDir(domain string) string { return filepath.Join(k.opts.Root, "sites", domain) }

// Dir is where a backup's copies are.
func (k *Keeper) Dir(domain, name string) string {
	return filepath.Join(k.data(), "backups", domain, name)
}

// LogPath is a backup's log: what its dumps and restores said.
func (k *Keeper) LogPath(domain, name string) string {
	return filepath.Join(k.data(), "logs", domain, "backup-"+name+".log")
}

func (k *Keeper) statePath(domain, name string) string {
	return filepath.Join(k.Dir(domain, name), "state.json")
}

// load reads the site's manifest and the backup in it.
func (k *Keeper) load(domain, name string) (*manifest.Manifest, *manifest.Backup, error) {
	if !manifest.ValidName(domain) || !manifest.ValidBackupName(name) {
		return nil, nil, ErrNotFound
	}
	m, err := manifest.Load(k.siteDir(domain))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, fmt.Errorf("%w: %s is not a site here", ErrNotFound, domain)
		}
		return nil, nil, fmt.Errorf("%s: %w", domain, err)
	}
	b := m.Backups[name]
	if b == nil {
		return m, nil, fmt.Errorf("%w: %s has no backup %q", ErrNotFound, domain, name)
	}
	return m, b, nil
}

// copyRe is a copy's file name: when (UTC), what made it, and what it is.
var copyRe = regexp.MustCompile(`^(\d{8}-\d{6})-(scheduled|manual|pre-restore)\.(sql|archive|dump)\.gz$`)

// ValidCopy says id names a copy (and nothing else in the folder).
func ValidCopy(id string) bool { return copyRe.MatchString(id) }

// Copies lists a backup's copies, newest first. The files are the list:
// a copy put there or removed by hand is one more or one less.
func (k *Keeper) Copies(domain, name string) []Copy {
	ents, err := os.ReadDir(k.Dir(domain, name))
	if err != nil {
		return []Copy{}
	}
	out := []Copy{}
	for _, e := range ents {
		m := copyRe.FindStringSubmatch(e.Name())
		if m == nil || e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		at, err := time.ParseInLocation("20060102-150405", m[1], time.UTC)
		if err != nil {
			continue
		}
		out = append(out, Copy{ID: e.Name(), At: at, Bytes: info.Size(), Kind: m[2]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}

// Path is a copy's file, checked to be one.
func (k *Keeper) Path(domain, name, id string) (string, error) {
	if !manifest.ValidName(domain) || !manifest.ValidBackupName(name) || !ValidCopy(id) {
		return "", ErrNotFound
	}
	p := filepath.Join(k.Dir(domain, name), id)
	if !fsx.Exists(p) {
		return "", fmt.Errorf("%w: no copy %s", ErrNotFound, id)
	}
	return p, nil
}

// Delete removes a copy.
func (k *Keeper) Delete(domain, name, id string) error {
	p, err := k.Path(domain, name, id)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil {
		return err
	}
	k.emit("backup.deleted", domain, "operator", "", name+": "+id+" deleted", map[string]any{"backup": name, "copy": id})
	return nil
}

// List is every backup of a site with its copies, and the folders of
// backups no longer in the manifest that still hold copies: removing a
// backup from site.json stops it, and does not throw its copies away.
func (k *Keeper) List(domain string) ([]Status, error) {
	if !manifest.ValidName(domain) {
		return nil, ErrNotFound
	}
	m, err := manifest.Load(k.siteDir(domain))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s is not a site here", ErrNotFound, domain)
		}
		return nil, err
	}
	out := []Status{}
	seen := map[string]bool{}
	for _, name := range m.BackupNames() {
		seen[name] = true
		out = append(out, k.status(domain, name, m.Backups[name], m.Disabled))
	}
	ents, _ := os.ReadDir(filepath.Join(k.data(), "backups", domain))
	for _, e := range ents {
		if !e.IsDir() || seen[e.Name()] || !manifest.ValidBackupName(e.Name()) {
			continue
		}
		if st := k.status(domain, e.Name(), nil, false); len(st.Copies) > 0 {
			st.Error = "not in the manifest any more: its copies are kept until they are deleted"
			out = append(out, st)
		}
	}
	return out, nil
}

func (k *Keeper) status(domain, name string, b *manifest.Backup, siteOff bool) Status {
	st := Status{Name: name, Config: b, Copies: k.Copies(domain, name)}
	for _, c := range st.Copies {
		st.Bytes += c.Bytes
	}
	var rec record
	_ = fsx.ReadJSON(k.statePath(domain, name), &rec)
	st.Last, st.LastRestore = rec.Last, rec.LastRestore
	k.mu.Lock()
	st.Running = k.running[domain+"/"+name]
	k.mu.Unlock()
	if b != nil && b.Schedule != "" && !b.Disabled && !siteOff {
		if s, err := cron.Parse(b.Schedule); err == nil {
			if n := s.Next(k.opts.Now().In(b.Location())); !n.IsZero() {
				st.Next = &n
			}
		}
	}
	return st
}

// begin marks the backup busy, or says it already is.
func (k *Keeper) begin(domain, name, what string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	key := domain + "/" + name
	if k.running[key] != "" {
		return false
	}
	k.running[key] = what
	return true
}

func (k *Keeper) end(domain, name string) {
	k.mu.Lock()
	delete(k.running, domain+"/"+name)
	k.mu.Unlock()
}

// Running says whether a copy or a restore of the backup is under way.
func (k *Keeper) Running(domain, name string) string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.running[domain+"/"+name]
}

// Backup takes a copy now. trigger is who asked ("manual", "schedule").
func (k *Keeper) Backup(ctx context.Context, domain, name, trigger string) (Copy, error) {
	if _, _, err := k.load(domain, name); err != nil {
		return Copy{}, err
	}
	if !k.begin(domain, name, "backup") {
		return Copy{}, ErrBusy
	}
	defer k.end(domain, name)
	kind := KindManual
	if trigger == "schedule" {
		kind = KindScheduled
	}
	return k.backup(ctx, domain, name, trigger, kind)
}

// Check says a copy or a restore could start now, without starting it:
// what the API answers before it runs one in the background.
func (k *Keeper) Check(domain, name, id string) error {
	_, b, err := k.load(domain, name)
	if err != nil {
		return err
	}
	if id != "" {
		if _, err := k.Path(domain, name, id); err != nil {
			return err
		}
		if b.Engine == "command" && len(b.Restore) == 0 {
			return fmt.Errorf("%w: %v", ErrInvalid, ErrNoRestore)
		}
	}
	if k.Running(domain, name) != "" {
		return ErrBusy
	}
	return nil
}

func (k *Keeper) backup(ctx context.Context, domain, name, trigger, kind string) (Copy, error) {
	start := k.opts.Now()
	res := &Result{At: start, Trigger: trigger}
	k.emit("backup.started", domain, actorOf(trigger), "", name+": copying ("+kind+")", map[string]any{"backup": name, "kind": kind})
	c, err := k.dump(ctx, domain, name, kind, start)
	res.Seconds = time.Since(start).Seconds()
	if err != nil {
		res.Error = err.Error()
		k.save(domain, name, func(r *record) { r.Last = res })
		k.emit("backup.failed", domain, actorOf(trigger), "error", name+": "+err.Error(), map[string]any{"backup": name, "trigger": trigger})
		return Copy{}, err
	}
	res.OK, res.Bytes, res.Copy = true, c.Bytes, c.ID
	k.save(domain, name, func(r *record) { r.Last = res })
	k.emit("backup.done", domain, actorOf(trigger), "", fmt.Sprintf("%s: %s (%s)", name, c.ID, sizeWords(c.Bytes)), map[string]any{"backup": name, "copy": c.ID, "bytes": c.Bytes, "trigger": trigger})
	return c, nil
}

// dump runs the backup's dump and writes its compressed output.
func (k *Keeper) dump(ctx context.Context, domain, name, kind string, start time.Time) (Copy, error) {
	m, b, err := k.load(domain, name)
	if err != nil {
		return Copy{}, err
	}
	dir := k.Dir(domain, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Copy{}, err
	}
	id := start.UTC().Format("20060102-150405") + "-" + kind + "." + Ext(b.Engine) + ".gz"
	final := filepath.Join(dir, id)
	if fsx.Exists(final) {
		return Copy{}, fmt.Errorf("%w: a copy was taken this very second; try again", ErrBusy)
	}
	log, err := k.openLog(domain, name, "backup ("+kind+")")
	if err != nil {
		return Copy{}, err
	}
	defer log.Close()
	// 0600: a database's whole content, the house's to read and no one
	// else's on the machine.
	f, err := os.OpenFile(final+".part", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return Copy{}, err
	}
	fail := func(err error) (Copy, error) {
		f.Close()
		_ = os.Remove(final + ".part")
		fmt.Fprintf(log, "failed: %v\n", err)
		return Copy{}, err
	}
	gz, _ := gzip.NewWriterLevel(f, gzip.BestCompression)
	gz.Name = strings.TrimSuffix(id, ".gz")
	gz.ModTime = start
	pr, pw, err := os.Pipe()
	if err != nil {
		return fail(err)
	}
	var n int64
	copied := make(chan error, 1)
	go func() {
		var err error
		n, err = io.Copy(gz, pr)
		copied <- err
	}()
	ctx, cancel := context.WithTimeout(ctx, b.TimeoutOrDefault())
	defer cancel()
	runErr := k.exec(ctx, domain, m, b, false, nil, pw, log)
	pw.Close()
	// A program the dump left behind holding the pipe would keep the
	// copy from ending: the dump's own end is the end.
	var copyErr error
	select {
	case copyErr = <-copied:
	case <-time.After(10 * time.Second):
		pr.Close()
		copyErr = <-copied
	}
	pr.Close()
	if runErr != nil {
		if ctx.Err() == context.DeadlineExceeded {
			runErr = fmt.Errorf("cut after %s", b.TimeoutOrDefault())
		}
		return fail(fmt.Errorf("%w%s", runErr, tail(k.LogPath(domain, name))))
	}
	if copyErr != nil {
		return fail(copyErr)
	}
	if n == 0 {
		// A dump that ended well and wrote nothing is a client that did
		// not reach its database the way it reports errors: a copy of
		// nothing kept as a copy is the worst of the two.
		return fail(errors.New("the dump ended without writing anything" + tail(k.LogPath(domain, name))))
	}
	if err := gz.Close(); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		return fail(err)
	}
	if err := os.Rename(final+".part", final); err != nil {
		_ = os.Remove(final + ".part")
		return Copy{}, err
	}
	info, err := os.Stat(final)
	if err != nil {
		return Copy{}, err
	}
	fmt.Fprintf(log, "ok: %s, %s of dump, %s compressed\n", id, sizeWords(n), sizeWords(info.Size()))
	k.prune(domain, name, b.KeepCopies())
	return Copy{ID: id, At: start.UTC().Truncate(time.Second), Bytes: info.Size(), Kind: kind}, nil
}

// prune keeps the newest keep copies, and the newest few a restore took
// first.
func (k *Keeper) prune(domain, name string, keep int) {
	kept, pre := 0, 0
	for _, c := range k.Copies(domain, name) {
		if c.Kind == KindPreRestore {
			pre++
			if pre <= preRestoreKept {
				continue
			}
		} else {
			kept++
			if kept <= keep {
				continue
			}
		}
		_ = os.Remove(filepath.Join(k.Dir(domain, name), c.ID))
	}
}

// RestoreOptions say how a restore goes.
type RestoreOptions struct {
	Trigger string
	// NoSafetyCopy skips the copy of the database as it is now that a
	// restore takes first: for a database too broken to be copied.
	NoSafetyCopy bool
}

// Restore puts a copy back: first a copy of the database as it is now
// (pre-restore), then the copy given, read into the restore command.
func (k *Keeper) Restore(ctx context.Context, domain, name, id string, o RestoreOptions) (Result, error) {
	m, b, err := k.load(domain, name)
	if err != nil {
		return Result{}, err
	}
	p, err := k.Path(domain, name, id)
	if err != nil {
		return Result{}, err
	}
	if !k.begin(domain, name, "restore") {
		return Result{}, ErrBusy
	}
	defer k.end(domain, name)
	if o.Trigger == "" {
		o.Trigger = "manual"
	}
	start := k.opts.Now()
	res := Result{At: start, Trigger: o.Trigger, Copy: id}
	finish := func(err error) (Result, error) {
		res.Seconds = time.Since(start).Seconds()
		if err != nil {
			res.Error = err.Error()
			k.save(domain, name, func(r *record) { r.LastRestore = &res })
			k.emit("backup.restore_failed", domain, actorOf(o.Trigger), "error", name+": "+id+": "+err.Error(), map[string]any{"backup": name, "copy": id})
			return res, err
		}
		res.OK = true
		k.save(domain, name, func(r *record) { r.LastRestore = &res })
		k.emit("backup.restored", domain, actorOf(o.Trigger), "warn", name+": "+id+" restored", map[string]any{"backup": name, "copy": id})
		return res, nil
	}
	if b.Engine == "command" && len(b.Restore) == 0 {
		return finish(ErrNoRestore)
	}
	k.emit("backup.restoring", domain, actorOf(o.Trigger), "warn", name+": restoring "+id, map[string]any{"backup": name, "copy": id})
	safety := !o.NoSafetyCopy
	if b.Engine == "sqlite" && !fsx.Exists(filepath.Join(k.siteDir(domain), filepath.FromSlash(b.File))) {
		safety = false // nothing there to keep
	}
	if safety {
		if _, err := k.backup(ctx, domain, name, o.Trigger, KindPreRestore); err != nil {
			return finish(fmt.Errorf("nothing was restored: the copy of the database as it is now failed first (%v); restore without it only if it cannot be copied", err))
		}
	}
	in, err := os.Open(p)
	if err != nil {
		return finish(err)
	}
	defer in.Close()
	zr, err := gzip.NewReader(in)
	if err != nil {
		return finish(fmt.Errorf("%s is not a gzip file: %w", id, err))
	}
	log, err := k.openLog(domain, name, "restore of "+id)
	if err != nil {
		return finish(err)
	}
	defer log.Close()
	cctx, cancel := context.WithTimeout(ctx, b.TimeoutOrDefault())
	defer cancel()
	if err := k.exec(cctx, domain, m, b, true, zr, log, log); err != nil {
		if cctx.Err() == context.DeadlineExceeded {
			err = fmt.Errorf("cut after %s", b.TimeoutOrDefault())
		}
		said := tail(k.LogPath(domain, name))
		fmt.Fprintf(log, "failed: %v\n", err)
		return finish(fmt.Errorf("%w%s", err, said))
	}
	fmt.Fprintf(log, "ok: %s restored\n", id)
	if b.Engine == "sqlite" && k.opts.Restart != nil && (m.Run != nil || len(m.Services) > 0) && !m.Disabled {
		// The process keeps the old file open, and would go on reading
		// and writing the database the restore replaced.
		if err := k.opts.Restart(context.Background(), domain); err != nil {
			fmt.Fprintf(log, "restored, but the site did not restart: %v\n", err)
		}
	}
	return finish(nil)
}

// exec runs the backup's dump (restore false: its output to stdout) or
// its restore (stdin the copy), where the database is.
func (k *Keeper) exec(ctx context.Context, domain string, m *manifest.Manifest, b *manifest.Backup, restore bool, stdin io.Reader, stdout io.Writer, log *os.File) error {
	dir := k.siteDir(domain)
	spec, container, err := k.target(domain, dir, m, b)
	if err != nil {
		return err
	}
	argv, env, err := command(b, restore, container == "", domain, dir)
	if err != nil {
		return err
	}
	if container != "" {
		var kv []string
		for _, key := range sortedKeys(env) {
			kv = append(kv, key+"="+env[key])
		}
		return k.opts.Docker.ExecStdio(ctx, container, kv, argv, stdin, stdout, log)
	}
	code, err := runtime.Exec(ctx, spec, filepath.Join(k.data(), "homes", domain), runtime.ExecOptions{Args: argv, Stdin: stdin, Stdout: stdout, Stderr: log, Env: env})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("exit status %d", code)
	}
	return nil
}

// target is where the backup's commands run: a container that is up
// (its name), or a spec to run as the site's processes run.
func (k *Keeper) target(domain, dir string, m *manifest.Manifest, b *manifest.Backup) (*manifest.Spec, string, error) {
	// SQLite is a file in the folder, read where the folder is, whatever
	// runs the site: an image seldom carries sqlite3.
	if b.Engine == "sqlite" {
		return siteSpec(domain, dir, m)
	}
	key, runtimeName := domain, ""
	switch {
	case b.Service == "":
		if m.Run != nil {
			runtimeName = m.Run.Runtime
		}
	case m.Services[b.Service] != nil:
		key = domain + "/" + b.Service
		if r := m.Services[b.Service].Run; r != nil {
			runtimeName = r.Runtime
		}
	case m.Workers[b.Service] != nil:
		key = domain + "/" + b.Service
		runtimeName = m.Workers[b.Service].Runtime
	default:
		return nil, "", fmt.Errorf("%w: %s has no service nor worker %q", ErrNotFound, domain, b.Service)
	}
	if runtimeName == "docker" {
		var st runtime.State
		what := domain
		if b.Service != "" {
			what = b.Service
		}
		if err := fsx.ReadJSON(runtime.StatePath(k.data(), key), &st); err != nil || st.Container == "" {
			return nil, "", fmt.Errorf("%s is not running here, and the copy is taken from the running database: start it first", what)
		}
		return nil, st.Container, nil
	}
	switch {
	case b.Service == "":
		return siteSpec(domain, dir, m)
	case m.Workers[b.Service] != nil:
		return manifest.WorkerSpec(domain, dir, b.Service, m.Workers[b.Service]), "", nil
	}
	if k.opts.ServiceSpec == nil {
		return nil, "", errors.New("no engine to ask for the service's environment")
	}
	spec, err := k.opts.ServiceSpec(domain, b.Service)
	return spec, "", err
}

// siteSpec is the site's run as a command of its own runs, or, for a
// site with none (static, services), its folder and HOME.
func siteSpec(domain, dir string, m *manifest.Manifest) (*manifest.Spec, string, error) {
	run := m.Run
	if run == nil {
		run = &manifest.Run{Command: []string{"true"}}
	}
	// As a process, whatever the site runs in: a copy in the site's
	// image would be a container of its own, with the site's limits and
	// none of its tools.
	r := *run
	r.Runtime, r.Docker = "process", nil
	spec, err := manifest.Resolve(domain, dir, &manifest.Manifest{Version: manifest.Version, Run: &r})
	return spec, "", err
}

func (k *Keeper) openLog(domain, name, what string) (*os.File, error) {
	p := k.LogPath(domain, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(f, "== %s %s\n", k.opts.Now().Format(time.RFC3339), what)
	return f, nil
}

// Log is the end of a backup's log.
func (k *Keeper) Log(domain, name string, lines int) (string, error) {
	if !manifest.ValidName(domain) || !manifest.ValidBackupName(name) {
		return "", ErrNotFound
	}
	raw, err := os.ReadFile(k.LogPath(domain, name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	all := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if lines > 0 && len(all) > lines {
		all = all[len(all)-lines:]
	}
	return strings.Join(all, "\n") + "\n", nil
}

func (k *Keeper) save(domain, name string, f func(*record)) {
	var rec record
	p := k.statePath(domain, name)
	_ = fsx.ReadJSON(p, &rec)
	f(&rec)
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	_ = fsx.WriteJSON(p, rec, 0o600)
}

func (k *Keeper) emit(typ, domain, actor, level, msg string, data map[string]any) {
	k.opts.Bus.Emit(events.Event{Type: typ, Subject: domain, Actor: actor, Level: level, Message: msg, Data: data})
}

func actorOf(trigger string) string {
	if trigger == "schedule" {
		return "schedule"
	}
	return "operator"
}

// Run looks at the schedules every tick until ctx ends. A backup is due
// when its next time after the last look is not after now; the first look
// is at start, so a copy missed while the supervisor was down is not
// taken at once on its return (the next one is hours away at most, and a
// restart at 03:00 is not a reason for two copies).
func (k *Keeper) Run(ctx context.Context) {
	k.mu.Lock()
	k.lastCheck = k.opts.Now()
	k.mu.Unlock()
	t := time.NewTicker(k.opts.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			k.Tick(ctx)
		}
	}
}

// Tick starts every backup due since the last tick.
func (k *Keeper) Tick(ctx context.Context) {
	now := k.opts.Now()
	k.mu.Lock()
	since := k.lastCheck
	if since.IsZero() {
		since = now.Add(-time.Minute)
	}
	k.lastCheck = now
	k.mu.Unlock()
	for _, d := range k.due(since, now) {
		go func(domain, name string) {
			select {
			case k.slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-k.slots }()
			_, _ = k.Backup(ctx, domain, name, "schedule")
		}(d[0], d[1])
	}
}

// due are the [domain, backup] pairs whose time came between since and
// now.
func (k *Keeper) due(since, now time.Time) [][2]string {
	ents, err := os.ReadDir(filepath.Join(k.opts.Root, "sites"))
	if err != nil {
		return nil
	}
	var out [][2]string
	for _, e := range ents {
		domain := e.Name()
		if !manifest.ValidName(domain) {
			continue
		}
		m, err := manifest.Load(filepath.Join(k.opts.Root, "sites", domain))
		if err != nil || m.Disabled || len(m.Backups) == 0 {
			continue
		}
		for _, name := range m.BackupNames() {
			b := m.Backups[name]
			if b.Disabled || b.Schedule == "" {
				continue
			}
			s, err := cron.Parse(b.Schedule)
			if err != nil {
				continue
			}
			if next := s.Next(since.In(b.Location())); !next.IsZero() && !next.After(now) {
				out = append(out, [2]string{domain, name})
			}
		}
	}
	return out
}

// tail is the end of the log, for an error: the reason is usually the
// client's last line ("Access denied for user").
func tail(logFile string) string {
	raw, err := os.ReadFile(logFile)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	var keep []string
	for i := len(lines) - 1; i >= 0 && len(keep) < 3; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" || strings.HasPrefix(l, "== ") {
			if strings.HasPrefix(l, "== ") {
				break
			}
			continue
		}
		keep = append([]string{l}, keep...)
	}
	if len(keep) == 0 {
		return ""
	}
	return "; it said: " + strings.Join(keep, " | ")
}

func sizeWords(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
