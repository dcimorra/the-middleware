package relocate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/archive"
	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/manifest"
)

// Target is the node a site moves to, as the mover talks to it: over the
// mesh in the house, over plain HTTP between two servers in the tests.
type Target interface {
	Name() string
	// Send and SendDelta stream a bundle into the target's staging.
	Send(ctx context.Context, domain string, body io.Reader) error
	SendDelta(ctx context.Context, domain string, body io.Reader) error
	// Commit puts the staged site in place with its manifest and its
	// tasks (task files by name).
	Commit(ctx context.Context, domain string, manifestData []byte, tasks map[string][]byte) error
	// Discard throws the staging away; Remove deletes a committed site
	// that did not come up (it is a copy: the source still has it).
	Discard(ctx context.Context, domain string) error
	Remove(ctx context.Context, domain string) error
	// Check says, before anything moves, that the target can take the
	// site: the name is free there and it can publish names (all of them).
	Check(ctx context.Context, domain string, need Needs) error
	// Ready says the site answers there, or why not.
	Ready(ctx context.Context, domain string) (bool, string, error)
	// Publish puts its names on door there, over what points here.
	Publish(ctx context.Context, domain, door string) error
}

// Preparer is a target that installs a site's dependencies in its staging,
// for its own machine, before the site is stopped here.
type Preparer interface {
	// Prepare says what it installed, which the move's step shows: an
	// install that leaves out what the start needs looks done otherwise.
	// dirs are the site's folders whose dependencies were left behind
	// (LeftBehind), relative to it.
	Prepare(ctx context.Context, domain string, dirs []string) (string, error)
}

// Repairer is a target whose house's AI can look at a site that arrived
// and does not start, before the move is undone. ErrNoRepair is a target
// with none configured: the move is undone as before.
type Repairer interface {
	Repair(ctx context.Context, domain, why string) (string, error)
}

// ErrNoRepair is a target without an AI for automatic work.
var ErrNoRepair = errors.New("the target has no AI for automatic work")

// Finisher is a target told that the move ended well, so it forgets what
// it would have taken out had the site not come up.
type Finisher interface {
	Finish(ctx context.Context, domain string) error
}

// Source is this node's side of a move.
type Source interface {
	// Stop stops the site and returns the manifest it had.
	Stop(ctx context.Context, domain string) ([]byte, error)
	// Start puts that manifest back, which starts it again.
	Start(domain string, manifestData []byte) error
	// Published says on which door its names are, if they are.
	Published(domain string) (string, bool)
	// Names are every name the site answers to.
	Names(domain string) []string
	// Unpublish takes this node's routes and records for the site down;
	// records already pointing elsewhere are left alone.
	Unpublish(ctx context.Context, domain string)
	// Archive keeps the stopped site in this node's archive and removes
	// its folder.
	Archive(ctx context.Context, domain string, manifestData []byte) (string, error)
}

// Tasker is a Source whose site has tasks, which go with it: a site that
// arrives without its nightly jobs serves, and nothing says what stopped.
type Tasker interface {
	// Tasks are the site's tasks as V2 task files, by name, as they are
	// before the move turns them off.
	Tasks(domain string) (map[string][]byte, error)
	// HoldTasks turns off here the ones that are on, so none runs while
	// the site moves nor after (two nodes running the same job against
	// the same database), and says which it turned off.
	HoldTasks(domain string) ([]string, error)
	// ReleaseTasks turns those on again: the move was undone.
	ReleaseTasks(names []string) error
}

// Needs are what a site asks of the node it moves to.
type Needs struct {
	// Names are the ones published where it is: the target must be able
	// to publish them all.
	Names []string `json:"names,omitempty"`
	// Docker: it runs in a container, and the target must have a docker
	// that answers. A container site moved to a machine without docker
	// is a site that does not start there.
	Docker bool `json:"docker,omitempty"`
	// Tasks are the names of its task files: none may be taken there.
	Tasks []string `json:"tasks,omitempty"`
	// Volumes are the docker volumes its move fills there (MoveVolumes):
	// none may be there already.
	Volumes []string `json:"volumes,omitempty"`
	// Carried are the docker volumes the move copies there as files, with
	// the site stopped (Mover.carried): none may be there already, and
	// the target must know how to take one. A node from before volumes
	// were carried refuses the field, which is the answer wanted: it
	// would have started the site on empty ones.
	Carried []string `json:"carried,omitempty"`
	// Data are the parts under data/ that go with it and are not in the
	// first list a move ever had (ExtraParts), when the site has them: a
	// target that does not place them all would drop them in silence.
	Data []string `json:"data,omitempty"`
	// Ports are the ones its workers have written (FixedPorts): each must
	// be free there.
	Ports []int `json:"ports,omitempty"`
	// Public are its services' public ports, as its manifest writes them
	// ("3478/udp", "49160-49400/udp"): none may be the target's own or
	// another site's there, nor bound by something else on that machine.
	Public []string `json:"public,omitempty"`
}

// Steps of a move, in order.
const (
	StepCheck   = "check"
	StepCopy    = "copy"
	StepInstall = "install"
	StepStop    = "stop"
	StepFinal   = "final"
	StepCommit  = "commit"
	StepStart   = "start"
	StepDNS     = "dns"
	StepCleanup = "cleanup"
	StepDone    = "done"
	// StepRestore is a failure's way back: the site started here again.
	StepRestore = "restore"
	// StepRepair is the target's AI looking at a site that does not start
	// there, before the move is undone.
	StepRepair = "repair"
	// StepExport and StepImport are the workers' move commands: the dump
	// where it was, with the site stopped, and its import there before
	// the commit.
	StepExport = "export"
	StepImport = "import"
)

// Mover moves a site off this node.
type Mover struct {
	Root   string
	Source Source
	// ReadyWait is how long the target gets to answer healthy.
	ReadyWait time.Duration
	// Poll is how often it is asked.
	Poll time.Duration
	// Parts, when set, replaces Parts: what goes with the site. An import
	// from another installation takes the folder and its HOME, not the
	// house's records of it, which that installation writes its own way.
	Parts func(root, domain string) []archive.Part
	// Dir, when set, is the site's folder: an app imported from a machine
	// that is not a house lives anywhere, not under Root/sites.
	Dir string
	// Install has a target that is a Preparer install the site's
	// dependencies there after the first copy; Parts must then leave
	// node_modules behind (MoveParts).
	Install bool
	// Journal, when set, has the move keep a journal from just before it
	// stops the site until it ends (JournalPath), for Recover after a
	// restart; its Target and ID are the caller's to fill.
	Journal *Journal
	// NoTrial has Check look for each export's program without running
	// the export (Rehearser): a check answered inside a request (the
	// panel's plan, the answer to the move's POST) has seconds, and a
	// database's dump takes minutes. The move's own check runs it.
	NoTrial bool
	// Note, when set, hears what Check found of the exports.
	Note func(string)
}

// Releaser is a Source whose house keeps counts of a site in memory
// (its visits, who has seen it): Release has them written, so the copy
// carries them.
type Releaser interface {
	Release(domain string)
}

// Needer is a Source that says itself what its site needs of the target,
// for a site whose manifest this package cannot read (a V1 one).
type Needer interface {
	Needs(domain string) Needs
}

// Report says what a move did.
type Report struct {
	Archive string        `json:"archive,omitempty"`
	Down    time.Duration `json:"down"`
	First   int64         `json:"first_bytes"`
	Last    int64         `json:"last_bytes"`
	DNS     string        `json:"dns,omitempty"`
}

// Move takes domain to t. progress hears each step as it starts.
func (m *Mover) Move(ctx context.Context, domain string, t Target, progress func(step, msg string)) (Report, error) {
	var rep Report
	if progress == nil {
		progress = func(string, string) {}
	}
	if !manifest.ValidName(domain) {
		return rep, ErrBadName
	}
	dir := filepath.Join(m.Root, "sites", domain)
	if m.Dir != "" {
		dir = m.Dir
	}
	if info, err := os.Lstat(dir); err != nil {
		return rep, fmt.Errorf("%s: no such site here", domain)
	} else if !info.IsDir() {
		return rep, ErrNotAFolder
	}
	parts := m.PartsFor(domain)

	ask := "asking " + t.Name() + " whether it can take " + domain
	if held := m.rehearsed(domain); len(held) > 0 && !m.NoTrial {
		ask += ", and trying the export of " + strings.Join(held, ", ") + " here while it serves"
	}
	progress(StepCheck, ask)
	if _, err := m.check(ctx, domain, t, func(s string) { progress(StepCheck, s) }); err != nil {
		return rep, err
	}
	// Whoever keeps counts of it in memory writes them, so the first copy
	// carries them as they are and not as they were a minute ago.
	if rl, ok := m.Source.(Releaser); ok {
		rl.Release(domain)
	}
	exporter, _ := m.Source.(Exporter)
	migrator, _ := t.(Migrator)
	tasker, _ := m.Source.(Tasker)
	// The workers whose data moves while the site serves (move.live), and
	// the ones that wait for it to stop. The live ones are exported before
	// the first copy, which carries their dump, and imported there before
	// the site stops here: it is down only for the last copy and the start.
	planned := m.plannedManifest(domain, dir)
	plannedRaw, _ := json.Marshal(planned)
	liveWorkers, stopWorkers := planned.MovingWorkers(true), planned.MovingWorkers(false)
	// However it ends, the dumps the exports leave here go: undone, the
	// site runs here on the database they were taken from; done, the
	// archive has them (it is packed before this runs), or nothing that
	// keeps the folder here needs them.
	defer DropOutputs(dir, planned, append(append([]string(nil), liveWorkers...), stopWorkers...))
	if len(liveWorkers) > 0 {
		progress(StepExport, "exporting the data of "+strings.Join(liveWorkers, ", ")+" here, while it serves")
		said, err := exporter.Export(ctx, domain, plannedRaw, liveWorkers)
		if err != nil {
			return rep, fmt.Errorf("export: %w", err)
		}
		if said != "" {
			progress(StepExport, said)
		}
		// The last copy takes what changed since two seconds before the
		// first began: a dump finished within those two seconds was sent
		// twice. comparaloo's 3.66 GB went again in its last copy, and that
		// was 2m40s of the 2m52s it was down.
		time.Sleep(3 * time.Second)
	}

	// The first copy, with the site serving. What is written while it
	// goes is caught by the second, which carries everything changed
	// since this moment.
	progress(StepCopy, "copying "+domain+" to "+t.Name()+" while it serves")
	since := time.Now().Add(-2 * time.Second)
	n, err := m.stream(ctx, parts, time.Time{}, func(r io.Reader) error { return t.Send(ctx, domain, r) })
	if err != nil {
		return rep, undone(fmt.Errorf("first copy: %w", err), m.undo(t, domain, false))
	}
	rep.First = n

	// node_modules built here is built for this machine's Node: a native
	// module (better-sqlite3, sharp) compiled for Node 24 does not load on
	// a target whose site runs on 18, and the first moves carried it and
	// failed at the start with ERR_DLOPEN_FAILED. Installed there instead,
	// with the Node the site gets there, while it still serves here, so
	// the install is not part of the time it is down.
	if pr, ok := t.(Preparer); ok && m.Install {
		progress(StepInstall, "installing its dependencies on "+t.Name()+", for that machine, while it serves here")
		said, err := pr.Prepare(ctx, domain, LeftBehind(dir, siteSkip(parts)))
		if err != nil {
			return rep, undone(fmt.Errorf("installing its dependencies there: %w", err), m.undo(t, domain, false))
		}
		if said != "" {
			// The same step again: its message becomes what was done.
			progress(StepInstall, said)
		}
	}

	if len(liveWorkers) > 0 {
		progress(StepImport, "importing the data of "+strings.Join(liveWorkers, ", ")+" on "+t.Name()+", while it serves here")
		said, err := migrator.Migrate(ctx, domain, plannedRaw, liveWorkers)
		if err != nil {
			return rep, undone(fmt.Errorf("import: %w", err), m.undo(t, domain, false))
		}
		if said != "" {
			progress(StepImport, said)
		}
	}

	progress(StepStop, "stopping "+domain+" here for the last copy")
	// Its tasks first, read as they are and then turned off: one starting
	// between the stop and the commit would write what the last copy
	// leaves behind.
	var taskFiles map[string][]byte
	var held []string
	if tasker != nil {
		if taskFiles, err = tasker.Tasks(domain); err != nil {
			return rep, undone(fmt.Errorf("its tasks here: %w", err), m.undo(t, domain, false))
		}
	}
	// The journal before anything stops: a restart from here on finds it,
	// and the site back as it was (Recover).
	var jr *Journal
	if m.Journal != nil {
		j := *m.Journal
		j.Domain, j.Step, j.Started, j.Held = domain, JournalStop, time.Now().UTC(), enabledTasks(taskFiles)
		j.Manifest, err = os.ReadFile(filepath.Join(dir, manifest.FileName))
		if err == nil {
			err = j.write(m.Root)
		}
		if err != nil {
			return rep, undone(fmt.Errorf("writing down the move before stopping the site: %w", err), m.undo(t, domain, false))
		}
		jr = &j
		defer removeJournal(m.Root, domain)
	}
	if tasker != nil {
		if held, err = tasker.HoldTasks(domain); err != nil {
			_ = tasker.ReleaseTasks(held)
			return rep, undone(fmt.Errorf("turning its tasks off here: %w", err), m.undo(t, domain, false))
		}
	}
	original, err := m.Source.Stop(ctx, domain)
	if err != nil {
		if jr != nil {
			// A stop that failed halfway has written hold or disabled: the
			// manifest it had goes back, or the site stays down.
			_ = m.Source.Start(domain, jr.Manifest)
		}
		if tasker != nil {
			_ = tasker.ReleaseTasks(held)
		}
		return rep, undone(fmt.Errorf("stop: %w", err), m.undo(t, domain, false))
	}
	if jr != nil {
		jr.Manifest = original
		_ = jr.write(m.Root)
	}
	stopped := time.Now()
	// From here every failure before the target answers puts the source
	// back as it was.
	restore := func(cause error) (Report, error) {
		progress(StepRestore, "starting it here again, as it was")
		if err := m.Source.Start(domain, original); err != nil {
			return rep, fmt.Errorf("%w; and starting it here again failed too: %v", cause, err)
		}
		if tasker != nil {
			if err := tasker.ReleaseTasks(held); err != nil {
				return rep, fmt.Errorf("%w; it runs here again, but its tasks are still off (%s): %v", cause, strings.Join(held, ", "), err)
			}
		}
		return rep, fmt.Errorf("%w; it runs here again, as it was", cause)
	}

	if len(stopWorkers) > 0 {
		progress(StepExport, "exporting the data of "+strings.Join(stopWorkers, ", ")+" here, with the site stopped")
		said, err := exporter.Export(ctx, domain, original, stopWorkers)
		if err != nil {
			return restore(undone(fmt.Errorf("export: %w", err), m.undo(t, domain, false)))
		}
		if said != "" {
			progress(StepExport, said)
		}
	}

	// Its docker volumes, asked for again now that it is stopped: one made
	// since the check goes too, and the target refuses one it already
	// has. Before the commit and with nothing running on either side, so
	// a copy that fails, or that is not there what it is here, undoes the
	// move with the site never having started on it.
	vols, err := m.carried(ctx, domain, planned, append(append([]string(nil), liveWorkers...), stopWorkers...))
	if err != nil {
		return restore(undone(err, m.undo(t, domain, false)))
	}
	if len(vols) > 0 {
		if _, ok := t.(VolumeTaker); !ok {
			return restore(undone(fmt.Errorf("%s cannot take the site's docker volumes (%s)", t.Name(), strings.Join(vols, ", ")), m.undo(t, domain, false)))
		}
		progress(StepVolumes, "copying its docker volumes ("+strings.Join(vols, ", ")+") to "+t.Name()+", with the site stopped")
		if jr != nil {
			jr.Carried = vols
			_ = jr.write(m.Root)
		}
		if err := m.carry(ctx, domain, t, vols, progress); err != nil {
			return restore(undone(fmt.Errorf("its docker volumes: %w", err), m.undo(t, domain, false)))
		}
	}

	if rl, ok := m.Source.(Releaser); ok {
		rl.Release(domain)
	}
	progress(StepFinal, "sending what changed")
	n, err = m.stream(ctx, parts, since, func(r io.Reader) error { return t.SendDelta(ctx, domain, r) })
	if err != nil {
		return restore(undone(fmt.Errorf("last copy: %w", err), m.undo(t, domain, false)))
	}
	rep.Last = n

	if len(stopWorkers) > 0 {
		progress(StepImport, "importing the data of "+strings.Join(stopWorkers, ", ")+" on "+t.Name()+", before the site starts there")
		said, err := migrator.Migrate(ctx, domain, original, stopWorkers)
		if err != nil {
			return restore(undone(fmt.Errorf("import: %w", err), m.undo(t, domain, false)))
		}
		if said != "" {
			progress(StepImport, said)
		}
	}

	progress(StepCommit, "putting it in place on "+t.Name())
	if jr != nil {
		// Before the commit is asked for, not after it answered: a restart
		// in between must ask the target, not start the site here again
		// while it may run there.
		jr.Step = JournalCommit
		if err := jr.write(m.Root); err != nil {
			return restore(undone(fmt.Errorf("writing down the move before its commit: %w", err), m.undo(t, domain, false)))
		}
	}
	if err := t.Commit(ctx, domain, original, taskFiles); err != nil {
		return restore(undone(fmt.Errorf("commit: %w", err), m.undo(t, domain, false)))
	}

	progress(StepStart, "waiting for it to answer on "+t.Name())
	err = m.waitReady(ctx, domain, t)
	if r, ok := t.(Repairer); ok && err != nil {
		// Before undoing it: the target's AI looks at why, with the site
		// stopped here (the price of it is minutes more down). What fails
		// at this point is mostly what the move did not carry, and the
		// operator had to find it by hand, every time a different thing.
		progress(StepRepair, "the house's AI on "+t.Name()+" looks at why it does not start there")
		said, rerr := r.Repair(ctx, domain, err.Error())
		switch {
		case errors.Is(rerr, ErrNoRepair):
		case rerr != nil:
			err = fmt.Errorf("%w; the house's AI there could not repair it: %v", err, rerr)
		default:
			progress(StepRepair, said)
			progress(StepStart, "waiting again for it to answer on "+t.Name()+", after the repair")
			if again := m.waitReady(ctx, domain, t); again != nil {
				err = fmt.Errorf("%w; and after the house's AI (%s): %v", err, said, again)
			} else {
				err = nil
			}
		}
	}
	if err != nil {
		return restore(undone(err, m.undo(t, domain, true)))
	}
	rep.Down = time.Since(stopped).Round(time.Second)
	if jr != nil {
		jr.Step = JournalReady
		_ = jr.write(m.Root)
	}

	if err := m.settle(ctx, domain, t, original, vols, &rep, progress); err != nil {
		return rep, err
	}
	progress(StepDone, fmt.Sprintf("%s runs on %s; it was down %s", domain, t.Name(), rep.Down))
	return rep, nil
}

// settle ends a move whose site answers there: its names follow, the
// target forgets how to take it out, and the copy here is archived. Each
// on its own time, not the move's: the site already runs there, and a
// move whose time ran out in these last steps left its names pointing
// here and its folder half packed.
func (m *Mover) settle(ctx context.Context, domain string, t Target, original []byte, carried []string, rep *Report, progress func(step, msg string)) error {
	own := context.WithoutCancel(ctx)
	// Its names move now, and only then does this node take its own route
	// down: the other order would leave the names pointing at nothing.
	if door, ok := m.Source.Published(domain); ok {
		progress(StepDNS, "pointing its names at "+t.Name())
		pctx, cancel := context.WithTimeout(own, 5*time.Minute)
		if err := t.Publish(pctx, domain, door); err != nil {
			// It runs there; the names did not follow. Said, not undone:
			// the target's own loop keeps trying, and this node keeps its
			// records until it does.
			rep.DNS = "the names were not moved: " + err.Error()
		} else {
			m.Source.Unpublish(pctx, domain)
			rep.DNS = "moved"
		}
		cancel()
	}

	if f, ok := t.(Finisher); ok {
		// It stays there: the target forgets how to take it out. A target
		// that does not answer keeps the record, which takes nothing out
		// while the site is there.
		fctx, cancel := context.WithTimeout(own, time.Minute)
		_ = f.Finish(fctx, domain)
		cancel()
	}
	progress(StepCleanup, "archiving the copy that stays here")
	mf, _ := manifest.Parse(original)
	dumped := dumpsThere(m.dir(domain), mf)
	// Unbounded: packing a site of gigabytes takes what it takes, and
	// cutting it leaves the folder here instead of in the archive.
	path, err := m.Source.Archive(own, domain, original)
	if err != nil {
		path = ""
	}
	m.sourceVolumes(own, domain, mf, path, dumped)
	if len(carried) > 0 {
		// Copied and read back there, and still never removed here by a
		// move: they are listed, and removing them is the operator's, once
		// they have seen the site with its data where it now runs.
		_ = LeaveVolumes(m.Root, domain, carried)
	}
	if err != nil {
		return fmt.Errorf("moved and running on %s, but the copy here could not be archived: %w", t.Name(), err)
	}
	rep.Archive = path
	return nil
}

// dir is the site's folder here.
func (m *Mover) dir(domain string) string {
	if m.Dir != "" {
		return m.Dir
	}
	return filepath.Join(m.Root, "sites", domain)
}

// Budget is the time a move of domain is given in all, for a caller that
// runs it in the background: what its steps are each bounded by, added
// up, and an hour for the copies, which nothing else bounds. A fixed hour
// for all of it was less than what a site with a database may take by
// its own manifest (an export of 30 minutes, then an import of as much
// there, the worker's start and the wait for the site), and a move cut
// there is undone after the site was stopped for nothing.
func (m *Mover) Budget(domain string) time.Duration {
	ready := m.ReadyWait
	if ready == 0 {
		ready = 3 * time.Minute
	}
	// The copies and the questions; the wait for the site, before and
	// after the target's AI looks at it (ten minutes at most).
	b := time.Hour + 2*ready + 10*time.Minute
	if m.Install {
		// The install there (30 minutes at most), and the one at the
		// commit an older house does instead (15).
		b += 45 * time.Minute
	}
	dir := filepath.Join(m.Root, "sites", domain)
	if m.Dir != "" {
		dir = m.Dir
	}
	planned := m.plannedManifest(domain, dir)
	if planned == nil {
		return b
	}
	for _, name := range MovingWorkers(planned) {
		t := moveTimeout(planned.Workers[name].Move)
		// Its export tried before the stop and run for the move, its
		// import there, the wait for it to be ready, and its start.
		b += 3*t + readyTime + 5*time.Minute
	}
	return b
}

// PartsFor is what a move of domain copies: the source's parts, less what
// the manifest it will have leaves out (move.exclude).
func (m *Mover) PartsFor(domain string) []archive.Part {
	dir := filepath.Join(m.Root, "sites", domain)
	if m.Dir != "" {
		dir = m.Dir
	}
	parts := Parts(m.Root, domain)
	if m.Parts != nil {
		parts = m.Parts(m.Root, domain)
	}
	return Excluding(parts, dir, m.plannedManifest(domain, dir))
}

// Check asks the target, before anything is touched, whether it can take
// the site: its name, its tasks' names, docker when it needs it, and every
// name it answers to when they are published here (a target that cannot
// point them would leave them on a node that no longer serves the site).
// Nothing changes on either side, so an import's --dry-run asks it too,
// and so does the panel's plan. The exports that wait for the stop are
// tried too, where they will run (Rehearser). It returns the workers whose
// data moves by command.
func (m *Mover) Check(ctx context.Context, domain string, t Target) ([]string, error) {
	return m.check(ctx, domain, t, m.Note)
}

func (m *Mover) check(ctx context.Context, domain string, t Target, note func(string)) ([]string, error) {
	need, moving, err := m.NeedsFor(domain, t)
	if err != nil {
		return nil, err
	}
	need.Carried, err = m.carried(ctx, domain, m.plannedManifest(domain, m.dir(domain)), moving)
	if err != nil {
		return nil, err
	}
	if _, ok := t.(VolumeTaker); !ok && len(need.Carried) > 0 {
		return nil, fmt.Errorf("the site keeps data in docker volumes (%s), which this move cannot take to %s: it would start there on empty ones", strings.Join(need.Carried, ", "), t.Name())
	}
	if err := t.Check(ctx, domain, need); err != nil {
		return nil, err
	}
	// The live exports need no trial: they run before the stop anyway,
	// and their failure stops nothing.
	if held := m.rehearsed(domain); len(held) > 0 {
		raw, _ := json.Marshal(m.plannedManifest(domain, m.dir(domain)))
		said, err := m.Source.(Rehearser).Rehearse(ctx, domain, raw, held, !m.NoTrial)
		if err != nil {
			return nil, err
		}
		if said != "" && note != nil {
			note(said)
		}
	}
	return moving, nil
}

// rehearsed are the workers whose exports Check tries: the ones that
// wait for the stop, of a source that can try them.
func (m *Mover) rehearsed(domain string) []string {
	if _, ok := m.Source.(Rehearser); !ok {
		return nil
	}
	planned := m.plannedManifest(domain, m.dir(domain))
	if planned == nil {
		return nil
	}
	return planned.MovingWorkers(false)
}

// NeedsFor is what a move of domain to t asks of t (Needs), and the
// workers whose data moves by command; an error when this source or t
// cannot move that data.
func (m *Mover) NeedsFor(domain string, t Target) (Needs, []string, error) {
	dir := m.dir(domain)
	need := NeedsOf(m.Root, domain, m.Source)
	if n, ok := m.Source.(Needer); ok {
		need = n.Needs(domain)
	}
	// The manifest it will have there says which workers' data moves by
	// command; both sides must know how, or the site would arrive on an
	// empty database, which serves and says nothing.
	planned := m.plannedManifest(domain, dir)
	moving := MovingWorkers(planned)
	_, exports := m.Source.(Exporter)
	_, migrates := t.(Migrator)
	if len(moving) > 0 && (!exports || !migrates) {
		return need, nil, fmt.Errorf("the data of %s moves by command (move in its manifest), which this move cannot run on one side or the other: move it by hand", strings.Join(moving, ", "))
	}
	if UsesDocker(planned) {
		need.Docker = true
	}
	// Found at the check, not at the import: there, the site was already
	// stopped here and its last copy sent.
	need.Volumes, need.Ports = MoveVolumes(domain, planned, moving), FixedPorts(planned)
	for _, p := range planned.PublicPorts() {
		need.Public = append(need.Public, p.PublicPort.String())
	}
	if tasker, ok := m.Source.(Tasker); ok {
		ts, err := tasker.Tasks(domain)
		if err != nil {
			return need, nil, fmt.Errorf("its tasks: %w", err)
		}
		need.Tasks = SortedNames(ts)
	}
	extra := extraParts(m.Root, domain)
	for _, part := range m.PartsFor(domain) {
		if extra[part.Name] && fsx.Exists(part.Path) {
			need.Data = append(need.Data, part.Name)
		}
	}
	return need, moving, nil
}

// plannedManifest is the manifest the site will be committed with, read
// before it stops: the source's word for it, or its site.json.
func (m *Mover) plannedManifest(domain, dir string) *manifest.Manifest {
	if p, ok := m.Source.(Planner); ok {
		if raw := p.Planned(domain); raw != nil {
			if mf, err := manifest.Parse(raw); err == nil {
				return mf
			}
		}
	}
	if mf, err := manifest.Load(dir); err == nil {
		return mf
	}
	return nil
}

// SortedNames are the keys of a set of task files, in order.
func SortedNames(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// undo takes out of the target what reached it: the staging and, once
// committed, the site (placed) and what it put under data/. On its own
// time: the move's may be what ran out. Before, every one of these was
// "_ = t.Discard(...)", and a target that kept the copy said nothing.
func (m *Mover) undo(t Target, domain string, placed bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var errs []string
	if placed {
		if err := t.Remove(ctx, domain); err != nil {
			errs = append(errs, "the site: "+err.Error())
		}
	}
	if err := t.Discard(ctx, domain); err != nil {
		errs = append(errs, "what was received: "+err.Error())
	}
	if len(errs) > 0 {
		return fmt.Errorf("what reached %s could not all be removed (%s); it is listed there as a leftover", t.Name(), strings.Join(errs, "; "))
	}
	return nil
}

// undone is cause, with what undoing it could not do.
func undone(cause, undoErr error) error {
	if undoErr == nil {
		return cause
	}
	return fmt.Errorf("%w; and %v", cause, undoErr)
}

// stream packs parts (since, for the last copy) into a pipe that send
// reads, and returns the bytes that went.
func (m *Mover) stream(ctx context.Context, parts []archive.Part, since time.Time, send func(io.Reader) error) (int64, error) {
	pr, pw := io.Pipe()
	counted := &counter{r: pr}
	done := make(chan error, 1)
	go func() {
		_, err := archive.PackPartsSince(ctx, pw, parts, since)
		pw.CloseWithError(err)
		done <- err
	}()
	err := send(counted)
	// Whatever send left unread, so the packer is not stuck writing.
	_ = pr.CloseWithError(errors.New("the target stopped reading"))
	packErr := <-done
	if packErr != nil && err == nil {
		err = packErr
	}
	return counted.n, err
}

type counter struct {
	r io.Reader
	n int64
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (m *Mover) waitReady(ctx context.Context, domain string, t Target) error {
	wait, poll := m.ReadyWait, m.Poll
	if wait == 0 {
		wait = 3 * time.Minute
	}
	if poll == 0 {
		poll = 2 * time.Second
	}
	deadline := time.Now().Add(wait)
	why := ""
	for {
		ok, w, err := t.Ready(ctx, domain)
		if err == nil && ok {
			return nil
		}
		if err != nil {
			why = err.Error()
		} else if w != "" {
			why = w
		}
		if time.Now().After(deadline) {
			if why == "" {
				why = "it did not answer"
			}
			return fmt.Errorf("it did not come up on %s in %s (%s)", t.Name(), wait, why)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// NeedsOf is what domain asks of a target: its published names, and docker
// when it runs in a container.
func NeedsOf(root, domain string, src Source) Needs {
	var n Needs
	if _, ok := src.Published(domain); ok {
		n.Names = src.Names(domain)
	}
	if mf, err := manifest.Load(filepath.Join(root, "sites", domain)); err == nil && UsesDocker(mf) {
		n.Docker = true
	}
	return n
}
