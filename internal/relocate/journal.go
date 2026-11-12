package relocate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/tasks"
)

// A move that the process running it does not outlive (an automatic
// update, the kernel's out-of-memory killer, a crash) used to leave the
// site down here for good: the manifest it had and the tasks it turned
// off lived only in the move's goroutine, while site.json already said
// hold or disabled and its workers disabled, and a restart obeys
// site.json. So from just before the stop until it ends, a move keeps a
// journal on disk with what undoing it or finishing it needs, and the
// house reads it when it starts again (Recover).

// Steps of a move's journal.
const (
	// JournalStop: the site is being stopped here, or is stopped; nothing
	// of it is in place there.
	JournalStop = "stop"
	// JournalCommit: it may be in place there; only the target can say.
	JournalCommit = "commit"
	// JournalReady: it answered there; its names, the target's record and
	// the archive here remain.
	JournalReady = "ready"
)

// StepRecover is the step a move cut by a restart is put right in.
const StepRecover = "recover"

// Journal is what a move in progress keeps on disk.
type Journal struct {
	Domain string `json:"domain"`
	// Target is the caller's name for the target, by which it finds it
	// again after a restart (a node's id), and ID the caller's own for the
	// move; both are filled by the caller.
	Target string `json:"target"`
	ID     string `json:"id,omitempty"`
	Step   string `json:"step"`
	// Manifest is the one the site had before the move stopped it.
	Manifest []byte `json:"manifest"`
	// Held are its tasks the move turned off here.
	Held []string `json:"held,omitempty"`
	// Carried are the docker volumes it copied to the target, which stay
	// here listed once the move ends well.
	Carried []string  `json:"carried,omitempty"`
	Started time.Time `json:"started"`
}

func journalDir(root string) string { return filepath.Join(root, "data", "relocate", ".moving") }

// JournalPath is where the journal of domain's move is.
func JournalPath(root, domain string) string {
	return filepath.Join(journalDir(root), domain+".json")
}

func (j *Journal) write(root string) error {
	if err := os.MkdirAll(journalDir(root), 0o755); err != nil {
		return err
	}
	return fsx.WriteJSON(JournalPath(root, j.Domain), j, 0o644)
}

func removeJournal(root, domain string) { _ = os.Remove(JournalPath(root, domain)) }

// Journals are the moves a restart cut, as their journals say.
func Journals(root string) []Journal {
	names, _ := filepath.Glob(filepath.Join(journalDir(root), "*.json"))
	sort.Strings(names)
	var out []Journal
	for _, n := range names {
		var j Journal
		if fsx.ReadJSON(n, &j) == nil && manifest.ValidName(j.Domain) && len(j.Manifest) > 0 {
			out = append(out, j)
		}
	}
	return out
}

// enabledTasks are the names of the task files that are on: what
// HoldTasks will turn off. Written before it runs, so a restart halfway
// through it turns on again all it may have turned off, and only those.
func enabledTasks(files map[string][]byte) []string {
	var out []string
	for _, name := range SortedNames(files) {
		if t, err := tasks.Parse(files[name]); err == nil && !t.Disabled {
			out = append(out, name)
		}
	}
	return out
}

// ErrNotThere is a target saying the site is not one of its own: never
// committed there, or taken out since.
var ErrNotThere = errors.New("the site is not there")

// ErrUnreachable is a target Recover could not ask whether the site is up
// there: nothing is done until it answers, for either answer could be
// the true one, and guessing wrong runs the site on both nodes or on none.
var ErrUnreachable = errors.New("the target did not answer")

// Recover puts right a move a restart cut, from its journal: before the
// commit the site starts here again as it was, its tasks turned on and
// what reached the target thrown away; after it the target is asked
// whether the site answers there, and the move is finished (its names,
// the archive) or undone accordingly. The journal goes once it is done,
// either way; a target that does not answer keeps it (ErrUnreachable).
func (m *Mover) Recover(ctx context.Context, j Journal, t Target, progress func(step, msg string)) (Report, error) {
	var rep Report
	if progress == nil {
		progress = func(string, string) {}
	}
	if !manifest.ValidName(j.Domain) {
		return rep, ErrBadName
	}
	domain := j.Domain
	mf, _ := manifest.Parse(j.Manifest)
	restore := func(cause string, undoErr error) (Report, error) {
		progress(StepRestore, "starting it here again, as it was")
		if err := m.Source.Start(domain, j.Manifest); err != nil {
			return rep, fmt.Errorf("%s; and starting it here again failed: %v (the journal stays, for the next start)", cause, err)
		}
		removeJournal(m.Root, domain)
		if mf != nil {
			DropOutputs(m.dir(domain), mf, MovingWorkers(mf))
		}
		if tasker, ok := m.Source.(Tasker); ok && len(j.Held) > 0 {
			if err := tasker.ReleaseTasks(j.Held); err != nil {
				return rep, undone(fmt.Errorf("%s; it runs here again, but its tasks are still off (%s): %v", cause, strings.Join(j.Held, ", "), err), undoErr)
			}
		}
		return rep, undone(fmt.Errorf("%s; it runs here again, as it was", cause), undoErr)
	}
	if j.Step == JournalStop {
		return restore("the house restarted before "+domain+" was put in place on "+t.Name(), m.undo(t, domain, false))
	}

	wait, poll := m.ReadyWait, m.Poll
	if wait == 0 {
		wait = 3 * time.Minute
	}
	if poll == 0 {
		poll = 2 * time.Second
	}
	progress(StepStart, "asking "+t.Name()+" whether "+domain+" answers there")
	deadline := time.Now().Add(wait)
	var last error
	state := ""
	for {
		ok, why, err := t.Ready(ctx, domain)
		switch {
		case err == nil && ok:
			if j.Step != JournalReady {
				j.Step = JournalReady
				_ = j.write(m.Root)
			}
			if err := m.settle(ctx, domain, t, j.Manifest, j.Carried, &rep, progress); err != nil {
				removeJournal(m.Root, domain)
				return rep, err
			}
			removeJournal(m.Root, domain)
			if mf != nil {
				DropOutputs(m.dir(domain), mf, MovingWorkers(mf))
			}
			progress(StepDone, domain+" runs on "+t.Name())
			return rep, nil
		case errors.Is(err, ErrNotThere):
			state, last = "gone", err
		case err != nil:
			state, last = "unreachable", err
		default:
			state, last = "down", errors.New(why)
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return rep, fmt.Errorf("%w: %v", ErrUnreachable, ctx.Err())
		case <-time.After(poll):
		}
	}
	switch state {
	case "gone":
		return restore("the house restarted during the move, and "+domain+" is not in place on "+t.Name(), m.undo(t, domain, false))
	case "down":
		return restore(fmt.Sprintf("the house restarted during the move, and %s does not come up on %s (%v)", domain, t.Name(), last), m.undo(t, domain, true))
	}
	return rep, fmt.Errorf("%w: %s, asked whether %s is up there: %v; it stays stopped here until it answers", ErrUnreachable, t.Name(), domain, last)
}
