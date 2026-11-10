package tasks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dcimorra/the-middleware/internal/cron"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/runtime"
)

// Options configure a Scheduler.
type Options struct {
	Root string
	Bus  *events.Bus
	// Tick is how often the folder is re-read; a minute by default.
	Tick time.Duration
	// Now is the clock; tests move it.
	Now func() time.Time
	// SiteEnv is what the house adds to the environment of a task that
	// runs in a site's folder, read at every launch from the manifest as
	// it is then: the site door's token for a site with claude_api. It
	// is given here and never written to tasks/, which is in the house's
	// git history, so a token copied into a task's env would be one
	// commit away from leaking and would outlive the block it came from.
	SiteEnv SiteEnvFunc
}

// SiteEnvFunc returns the variables the house gives a site's processes.
type SiteEnvFunc func(site string, m *manifest.Manifest) map[string]string

// Scheduler re-reads tasks/ every tick and runs what is due.
type Scheduler struct {
	opts Options
	mu   sync.Mutex
	// lastCheck is the minute the last tick covered: a task is due when
	// its next run after lastCheck is not after now. Starting it at boot
	// means a task missed while the supervisor was down is not replayed,
	// which is the crontab rule and the safe one for a backup.
	lastCheck time.Time
	states    map[string]*state
	wg        sync.WaitGroup
	siteEnv   atomic.Pointer[SiteEnvFunc]
}

type state struct {
	running bool
	queued  bool
	lastErr string
	inst    runtime.Instance
}

// running is data/tasks/running/<name>.json: a run in progress, so a
// supervisor that restarts mid-run adopts it instead of forgetting it.
// Before, a backup that outlived a restart kept running, was never
// recorded when it ended, and its task looked stuck forever.
type runningFile struct {
	State   runtime.State     `json:"state"`
	Start   time.Time         `json:"start"`
	Trigger string            `json:"trigger"`
	Timeout manifest.Duration `json:"timeout,omitempty"`
}

func (s *Scheduler) runningPath(name string) string {
	return filepath.Join(s.opts.Root, "data", "tasks", "running", name+".json")
}

// ErrRunning says the task is already running.
var ErrRunning = errors.New("task is already running")

// NewScheduler builds a scheduler. Nothing runs until Run or RunNow.
func NewScheduler(opts Options) *Scheduler {
	if opts.Bus == nil {
		opts.Bus = events.New("")
	}
	if opts.Tick == 0 {
		opts.Tick = time.Minute
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	// Absolute so a task's cwd and MIDDLEWARE_SITE_DIR do not depend on
	// where the operator typed the command.
	if abs, err := filepath.Abs(opts.Root); err == nil {
		opts.Root = abs
	}
	return &Scheduler{opts: opts, states: map[string]*state{}, lastCheck: opts.Now()}
}

// SetSiteEnv installs Options.SiteEnv after the scheduler was built: the
// site door is wired after the tasks hook, and a run already going keeps
// the environment it started with.
func (s *Scheduler) SetSiteEnv(f SiteEnvFunc) { s.siteEnv.Store(&f) }

// Run ticks until ctx ends. Runs in progress are left to finish: a backup
// halfway through must not die because the supervisor restarted, and the
// runtime gives each one its own process group so it is not our child's
// problem either.
func (s *Scheduler) Run(ctx context.Context) {
	s.adoptAll()
	s.tick()
	t := time.NewTicker(s.opts.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.tick()
		}
	}
}

// Status is one task as the panel lists it.
type Status struct {
	Name     string    `json:"name"`
	Schedule string    `json:"schedule"`
	Site     string    `json:"site,omitempty"`
	Title    string    `json:"title,omitempty"`
	Timezone string    `json:"timezone,omitempty"`
	Disabled bool      `json:"disabled,omitempty"`
	Next     time.Time `json:"next"`
	Last     *Run      `json:"last,omitempty"`
	Running  bool      `json:"running"`
	Error    string    `json:"error,omitempty"`
}

// List returns every task file, including the ones that do not parse, so
// a broken task is a line with an error and not an absence.
func (s *Scheduler) List() []Status {
	now := s.opts.Now()
	var out []Status
	for _, name := range Names(s.opts.Root) {
		st := Status{Name: name}
		t, err := Load(s.opts.Root, name)
		if err != nil {
			st.Error = err.Error()
		} else {
			st.Schedule, st.Site, st.Disabled = t.Schedule, t.Site, t.Disabled
			st.Title, st.Timezone = t.Title, t.Timezone
			if t.Site != "" && !fsx.IsDir(filepath.Join(s.opts.Root, "sites", t.Site)) {
				// No next run to promise: tick runs nothing until the
				// folder is back.
				st.Error = fmt.Sprintf("site %s: no such folder", t.Site)
			} else if sched, err := cron.Parse(t.Schedule); err == nil && !t.Disabled {
				st.Next = sched.Next(now.In(t.Location()))
			}
		}
		if r := LoadRecord(s.opts.Root, name); len(r.Runs) > 0 {
			last := r.Runs[len(r.Runs)-1]
			st.Last = &last
		}
		s.mu.Lock()
		if x := s.states[name]; x != nil {
			st.Running = x.running
		}
		s.mu.Unlock()
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// RunNow runs a task outside its schedule and waits for it to end. It
// refuses while a scheduled run is in progress rather than queueing: the
// operator pressing the button wants to see it run, and "it will run
// later" is what the schedule already promises.
func (s *Scheduler) RunNow(name string) error {
	t, err := Load(s.opts.Root, name)
	if err != nil {
		return err
	}
	if !s.claim(name) {
		return ErrRunning
	}
	return s.execute(name, t, "operator")
}

func (s *Scheduler) tick() {
	now := s.opts.Now()
	s.mu.Lock()
	since := s.lastCheck
	s.lastCheck = now
	s.mu.Unlock()
	for _, name := range Names(s.opts.Root) {
		t, err := Load(s.opts.Root, name)
		if err != nil {
			s.invalid(name, err)
			continue
		}
		if t.Site != "" && !t.Disabled && !fsx.IsDir(filepath.Join(s.opts.Root, "sites", t.Site)) {
			// Said once, not at every run: the task of a site that was
			// archived or moved away failed every minute of its schedule,
			// each one an error in the journal and a run in its record,
			// until someone went looking for it. It starts again by
			// itself the minute the folder is back.
			s.invalid(name, fmt.Errorf("site %s: no such folder", t.Site))
			continue
		}
		s.valid(name)
		if t.Disabled {
			continue
		}
		sched, err := cron.Parse(t.Schedule)
		if err != nil {
			s.invalid(name, err)
			continue
		}
		// Read in the task's zone: cron.Next matches the fields in the
		// location of the time it is given.
		due := sched.Next(since.In(t.Location()))
		if due.IsZero() || due.After(now) {
			continue
		}
		s.trigger(name, t)
	}
}

func (s *Scheduler) invalid(name string, err error) {
	s.mu.Lock()
	st := s.state(name)
	changed := st.lastErr != err.Error()
	st.lastErr = err.Error()
	s.mu.Unlock()
	if changed {
		s.emit("task.invalid", name, "error", err.Error(), nil)
	}
}

func (s *Scheduler) valid(name string) {
	s.mu.Lock()
	st := s.state(name)
	was := st.lastErr
	st.lastErr = ""
	s.mu.Unlock()
	if was != "" {
		s.emit("task.valid", name, "", "task file readable again", nil)
	}
}

// state must be called with the lock held.
func (s *Scheduler) state(name string) *state {
	st, ok := s.states[name]
	if !ok {
		st = &state{}
		s.states[name] = st
	}
	return st
}

func (s *Scheduler) claim(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state(name)
	if st.running {
		return false
	}
	st.running = true
	return true
}

// trigger starts a scheduled run, or applies the overlap rule when one is
// already running. A skip is recorded as a run with status "skipped": a
// task that silently never runs because the previous one is stuck is the
// failure that looks like success.
func (s *Scheduler) trigger(name string, t *Task) {
	if !s.claim(name) {
		if t.Overlap == "queue" {
			s.mu.Lock()
			already := s.state(name).queued
			s.state(name).queued = true
			s.mu.Unlock()
			if !already {
				s.emit("task.queued", name, "", "previous run still going; will run again when it ends", nil)
			}
			return
		}
		_ = appendRun(s.opts.Root, name, Run{At: s.opts.Now(), Status: StatusSkipped, Trigger: "schedule", Error: "previous run still going"})
		s.emit("task.skipped", name, "warn", "previous run still going", nil)
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		_ = s.execute(name, t, "schedule")
	}()
}

// execute runs one claimed task to its end, records it, and releases the
// claim, running once more if a run was queued meanwhile.
func (s *Scheduler) execute(name string, t *Task, trigger string) error {
	err := s.runOnce(name, t, trigger)
	s.mu.Lock()
	st := s.state(name)
	again := st.queued
	st.queued = false
	st.running = false
	s.mu.Unlock()
	if again {
		if fresh, lerr := Load(s.opts.Root, name); lerr == nil && s.claim(name) {
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				_ = s.execute(name, fresh, "schedule")
			}()
		}
	}
	return err
}

// Wait blocks until every run the scheduler started has ended. Tests use
// it; the supervisor does not, on purpose (see Run).
func (s *Scheduler) Wait() { s.wg.Wait() }

func (s *Scheduler) spec(name string, t *Task) (*manifest.Spec, string, error) {
	root := s.opts.Root
	spec := &manifest.Spec{Domain: t.Site, SiteDir: root, Cwd: root, Env: map[string]string{}, Revision: "task:" + name}
	home := filepath.Join(root, "data", "homes", "tasks")
	var m *manifest.Manifest
	if t.Site != "" {
		dir := filepath.Join(root, "sites", t.Site)
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			return nil, "", fmt.Errorf("site %s: no such folder", t.Site)
		}
		var err error
		m, err = manifest.Load(dir)
		if err != nil {
			return nil, "", fmt.Errorf("site %s: %w", t.Site, err)
		}
		spec.SiteDir, spec.Cwd = dir, dir
		home = filepath.Join(root, "data", "homes", t.Site)
		if m.Run != nil {
			resolved, err := manifest.Resolve(t.Site, dir, m)
			if err != nil {
				return nil, "", err
			}
			for k, v := range resolved.Env {
				spec.Env[k] = v
			}
		}
	}
	vars := map[string]string{"DOMAIN": t.Site, "SITE_DIR": spec.SiteDir}
	for k, v := range t.Env {
		spec.Env[k] = manifest.Expand(v, vars)
	}
	// The house's variables go after the task's own, as the engine puts
	// them after the site's: what the door is told a process is comes
	// from the house, not from a file in tasks/.
	if m != nil {
		f := s.opts.SiteEnv
		if p := s.siteEnv.Load(); p != nil {
			f = *p
		}
		if f != nil {
			for k, v := range f(t.Site, m) {
				spec.Env[k] = v
			}
		}
	}
	spec.Command = make([]string, len(t.Command))
	for i, a := range t.Command {
		spec.Command[i] = manifest.Expand(a, vars)
	}
	return spec, home, nil
}

func (s *Scheduler) runOnce(name string, t *Task, trigger string) error {
	start := s.opts.Now()
	record := func(status string, code int, err error) error {
		run := Run{At: start, Status: status, Duration: manifest.Duration(s.opts.Now().Sub(start)), ExitCode: code, Trigger: trigger}
		if err != nil {
			run.Error = err.Error()
		}
		if werr := appendRun(s.opts.Root, name, run); werr != nil {
			s.emit("task.failed", name, "error", "run not recorded: "+werr.Error(), nil)
		}
		data := map[string]any{"status": status, "exit_code": code, "duration": time.Duration(run.Duration).String(), "trigger": trigger}
		if status == StatusOK {
			s.emit("task.done", name, "", "", data)
			return nil
		}
		msg := status
		if err != nil {
			msg = err.Error()
		}
		s.emit("task.failed", name, "error", msg, data)
		if err == nil {
			err = fmt.Errorf("%s: %s (exit %d)", name, status, code)
		}
		return err
	}
	spec, home, err := s.spec(name, t)
	if err != nil {
		return record(StatusError, -1, err)
	}
	// Port 0: a task has no port, and the runtime puts PORT last anyway.
	// What the runtime gives in return is everything a site gets: its own
	// process group, output to a file nobody pipes, SIGTERM then SIGKILL.
	inst, err := runtime.Process{}.Start(context.Background(), spec, 0, LogPath(s.opts.Root, name), home)
	if err != nil {
		return record(StatusError, -1, err)
	}
	s.emit("task.started", name, "", "", map[string]any{"trigger": trigger, "pid": inst.State().PID, "site": t.Site})
	_ = fsx.WriteJSON(s.runningPath(name), runningFile{State: inst.State(), Start: start, Trigger: trigger, Timeout: t.Timeout}, 0o644)
	return s.follow(name, inst, time.Duration(t.Timeout), start, record)
}

// follow waits for a run to end, killing it at its timeout, and records
// it. An adopted run's exit code is not ours to read (it is not our
// child), so it ends as "ended" rather than guessing ok or failed.
func (s *Scheduler) follow(name string, inst runtime.Instance, timeout time.Duration, start time.Time, record func(string, int, error) error) error {
	s.mu.Lock()
	s.state(name).inst = inst
	s.mu.Unlock()
	defer func() {
		_ = os.Remove(s.runningPath(name))
		s.mu.Lock()
		s.state(name).inst = nil
		s.mu.Unlock()
	}()
	var timer <-chan time.Time
	if timeout > 0 {
		left := timeout - s.opts.Now().Sub(start)
		if left < time.Second {
			left = time.Second
		}
		timer = time.After(left)
	}
	select {
	case err := <-inst.Done():
		s.mu.Lock()
		stopped := s.state(name).lastErr == "stopped"
		s.state(name).lastErr = ""
		s.mu.Unlock()
		switch {
		case stopped:
			return record(StatusStopped, -1, errors.New("stopped by the operator"))
		case isAdopted(inst):
			return record(StatusEnded, -1, nil)
		case err == nil:
			return record(StatusOK, 0, nil)
		}
		return record(StatusFailed, exitCode(err), nil)
	case <-timer:
		_ = inst.Stop(context.Background(), 10*time.Second)
		return record(StatusTimeout, -1, fmt.Errorf("killed after %s", timeout))
	}
}

func isAdopted(inst runtime.Instance) bool {
	a, ok := inst.(interface{ Adopted() bool })
	return ok && a.Adopted()
}

// Stop ends a run in progress.
func (s *Scheduler) Stop(name string) error {
	s.mu.Lock()
	st := s.state(name)
	inst := st.inst
	if inst != nil {
		st.lastErr = "stopped"
	}
	s.mu.Unlock()
	if inst == nil {
		return fmt.Errorf("%s: not running", name)
	}
	return inst.Stop(context.Background(), 10*time.Second)
}

// adoptAll takes over the runs a previous supervisor left going. One that
// already ended is recorded as such, with its exit code unknown: a run
// that simply vanished from the record would be the failure that looks
// like nothing happened.
func (s *Scheduler) adoptAll() {
	ents, err := os.ReadDir(filepath.Dir(s.runningPath("x")))
	if err != nil {
		return
	}
	for _, e := range ents {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		var rf runningFile
		if fsx.ReadJSON(s.runningPath(name), &rf) != nil {
			_ = os.Remove(s.runningPath(name))
			continue
		}
		record := func(status string, code int, err error) error {
			run := Run{At: rf.Start, Status: status, Duration: manifest.Duration(s.opts.Now().Sub(rf.Start)), ExitCode: code, Trigger: rf.Trigger}
			if err != nil {
				run.Error = err.Error()
			} else if status == StatusEnded {
				run.Error = "adopted after a supervisor restart; the exit code is not known"
			}
			_ = appendRun(s.opts.Root, name, run)
			s.emit("task."+map[bool]string{true: "done", false: "failed"}[status == StatusEnded || status == StatusOK], name, "", status, map[string]any{"status": status, "adopted": true})
			return nil
		}
		inst, err := runtime.Process{}.Adopt(context.Background(), rf.State)
		if err != nil {
			_ = record(StatusEnded, -1, errors.New("ended while the supervisor was down; the exit code is not known"))
			_ = os.Remove(s.runningPath(name))
			continue
		}
		if !s.claim(name) {
			continue
		}
		s.emit("task.adopted", name, "", "still running from a previous supervisor", map[string]any{"pid": rf.State.PID})
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			_ = s.follow(name, inst, time.Duration(rf.Timeout), rf.Start, record)
			s.mu.Lock()
			s.state(name).running = false
			s.mu.Unlock()
		}()
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func (s *Scheduler) emit(typ, name, level, msg string, data map[string]any) {
	s.opts.Bus.Emit(events.Event{Type: typ, Subject: name, Actor: "schedule", Level: level, Message: msg, Data: data})
}
