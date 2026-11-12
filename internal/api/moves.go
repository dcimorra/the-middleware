package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/relocate"
)

// A move is a list of operations, each with its state, kept on disk. The
// first version kept only the last step, in memory and in a notice that
// went away with the page: a move that failed said "it did not complete"
// and nothing else, and a restart forgot it had happened. What each step
// did, how long it took, which one failed and why, and what was done to
// put the site back is what the operator needs to read afterwards, and a
// moved site's own page is gone from this node by then; so the moves are
// listed by the house too (GET /moves).

// Step states.
const (
	stepRunning = "running"
	stepOK      = "ok"
	stepFailed  = "failed"
	stepWarn    = "warn"
)

// moveStep is one operation of a move.
type moveStep struct {
	Step    string     `json:"step"`
	Message string     `json:"message"`
	State   string     `json:"state"`
	Started time.Time  `json:"started"`
	Ended   *time.Time `json:"ended,omitempty"`
	Error   string     `json:"error,omitempty"`
}

// moveJob is one move in flight or finished.
type moveJob struct {
	ID      string     `json:"id"`
	Domain  string     `json:"domain"`
	Node    string     `json:"node"`
	Step    string     `json:"step"`
	Message string     `json:"message"`
	Error   string     `json:"error,omitempty"`
	Done    bool       `json:"done"`
	Started time.Time  `json:"started"`
	Ended   *time.Time `json:"ended,omitempty"`
	Steps   []moveStep `json:"steps"`
	// Plan is the steps it is expected to go through, in order, for a
	// progress bar: the ones this site will have (install, dns) or not.
	Plan   []string         `json:"plan"`
	Report *relocate.Report `json:"report,omitempty"`
}

type moves struct {
	mu   sync.Mutex
	jobs map[string]*moveJob
	// receiving counts the requests of a move into this node in progress
	// (a copy arriving, a commit): MovesRunning.
	receiving int
}

// movesKept is how many finished moves stay on disk.
const movesKept = 50

func (s *Server) movesDir() string { return filepath.Join(s.opts.Root, "data", "moves") }

// movePlan is the steps a move goes through.
func movePlan(install, published, volumes bool) []string {
	p := []string{relocate.StepCheck, relocate.StepCopy}
	if install {
		p = append(p, relocate.StepInstall)
	}
	p = append(p, relocate.StepStop)
	if volumes {
		p = append(p, relocate.StepVolumes)
	}
	p = append(p, relocate.StepFinal, relocate.StepCommit, relocate.StepStart)
	if published {
		p = append(p, relocate.StepDNS)
	}
	return append(p, relocate.StepCleanup, relocate.StepDone)
}

// newMove starts a job, or says the one in flight.
func (s *Server) newMove(domain, node string, plan []string) (*moveJob, *moveJob) {
	s.moves.mu.Lock()
	defer s.moves.mu.Unlock()
	if s.moves.jobs == nil {
		s.moves.jobs = map[string]*moveJob{}
	}
	if j := s.moves.jobs[domain]; j != nil && !j.Done {
		c := j.copy()
		return nil, &c
	}
	now := time.Now()
	j := &moveJob{ID: now.UTC().Format("20060102T150405.000") + "-" + domain, Domain: domain, Node: node, Started: now, Steps: []moveStep{}, Plan: plan}
	s.moves.jobs[domain] = j
	s.saveMoveLocked(j)
	return j, nil
}

// advance closes the step running and opens the next one.
func (s *Server) advance(j *moveJob, step, msg string) {
	s.moves.mu.Lock()
	defer s.moves.mu.Unlock()
	now := time.Now()
	if n := len(j.Steps); n > 0 && j.Steps[n-1].State == stepRunning && j.Steps[n-1].Step == step {
		// The same step again says what it did: one step, its message
		// updated, not a second line.
		j.Steps[n-1].Message, j.Message = msg, msg
		s.saveMoveLocked(j)
		return
	}
	if n := len(j.Steps); n > 0 && j.Steps[n-1].State == stepRunning {
		j.Steps[n-1].State, j.Steps[n-1].Ended = stepOK, &now
	}
	j.Step, j.Message = step, msg
	st := moveStep{Step: step, Message: msg, State: stepRunning, Started: now}
	if step == relocate.StepDone {
		st.State, st.Ended = stepOK, &now
	}
	j.Steps = append(j.Steps, st)
	s.saveMoveLocked(j)
}

// finish ends the job: the step running failed with err, or all went.
func (s *Server) finish(j *moveJob, rep relocate.Report, err error) {
	s.moves.mu.Lock()
	defer s.moves.mu.Unlock()
	now := time.Now()
	j.Done, j.Ended, j.Report = true, &now, &rep
	n := len(j.Steps)
	if err != nil {
		j.Error = err.Error()
		if n > 0 && j.Steps[n-1].State == stepRunning {
			last := &j.Steps[n-1]
			last.Ended = &now
			if last.Step == relocate.StepRestore && strings.Contains(j.Error, "it runs here again") {
				// The way back worked; what failed was the step before it.
				last.State = stepOK
				for i := n - 2; i >= 0; i-- {
					if j.Steps[i].Step != relocate.StepRestore {
						j.Steps[i].State, j.Steps[i].Error = stepFailed, strings.TrimSuffix(j.Error, "; it runs here again, as it was")
						break
					}
				}
			} else {
				last.State, last.Error = stepFailed, j.Error
			}
		}
	} else if n > 0 && j.Steps[n-1].State == stepRunning {
		j.Steps[n-1].State, j.Steps[n-1].Ended = stepOK, &now
	}
	if strings.HasPrefix(rep.DNS, "the names were not moved") {
		for i := range j.Steps {
			if j.Steps[i].Step == relocate.StepDNS {
				j.Steps[i].State, j.Steps[i].Error = stepWarn, rep.DNS
			}
		}
	}
	s.saveMoveLocked(j)
	s.pruneMovesLocked()
}

func (j *moveJob) copy() moveJob {
	c := *j
	c.Steps = append([]moveStep(nil), j.Steps...)
	c.Plan = append([]string(nil), j.Plan...)
	return c
}

func (s *Server) saveMoveLocked(j *moveJob) {
	_ = os.MkdirAll(s.movesDir(), 0o755)
	_ = fsx.WriteJSON(filepath.Join(s.movesDir(), j.ID+".json"), j, 0o644)
}

func (s *Server) pruneMovesLocked() {
	names, _ := filepath.Glob(filepath.Join(s.movesDir(), "*.json"))
	sort.Strings(names)
	for len(names) > movesKept {
		_ = os.Remove(names[0])
		names = names[1:]
	}
}

// listMoves is every move kept, the newest first; domain, when set, only
// that site's. One found unfinished on disk and not running here was cut
// by a restart of the house: said as such, at the step it was in.
func (s *Server) listMoves(domain string) []moveJob {
	s.moves.mu.Lock()
	defer s.moves.mu.Unlock()
	names, _ := filepath.Glob(filepath.Join(s.movesDir(), "*.json"))
	out := []moveJob{}
	for _, n := range names {
		raw, err := os.ReadFile(n)
		if err != nil {
			continue
		}
		var j moveJob
		if json.Unmarshal(raw, &j) != nil || (domain != "" && j.Domain != domain) {
			continue
		}
		if live := s.moves.jobs[j.Domain]; live != nil && live.ID == j.ID {
			j = live.copy()
		} else if !j.Done {
			now := time.Now()
			j.Done, j.Ended = true, &now
			j.Error = "the house restarted during this step: the move did not go on; if the site was already stopped here, start it, or move it again"
			if k := len(j.Steps); k > 0 {
				j.Steps[k-1].State, j.Steps[k-1].Error, j.Steps[k-1].Ended = stepFailed, j.Error, &now
			}
			_ = fsx.WriteJSON(n, &j, 0o644)
		}
		out = append(out, j)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Started.After(out[b].Started) })
	return out
}
