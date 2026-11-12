package api

import (
	"errors"
	"testing"

	"github.com/dcimorra/the-middleware/internal/relocate"
)

// A move is read afterwards as its operations: which went, which failed
// and why, and the way back, kept across a restart of the house.
func TestAMoveIsReadAsItsOperationsAndSurvivesARestart(t *testing.T) {
	f := newFixture(t).withToken()
	j, busy := f.srv.newMove("m.test", "b", movePlan(true, false, false))
	if busy != nil {
		t.Fatal(busy)
	}
	if _, busy := f.srv.newMove("m.test", "b", movePlan(true, false, false)); busy == nil {
		t.Fatal("a second move of the same site while one runs")
	}
	for _, st := range []string{relocate.StepCheck, relocate.StepCopy, relocate.StepStop, relocate.StepFinal, relocate.StepCommit, relocate.StepStart, relocate.StepRestore} {
		f.srv.advance(j, st, st+"…")
	}
	f.srv.finish(j, relocate.Report{}, errors.New("not ready after 1m0s; it runs here again, as it was"))
	list := f.srv.listMoves("m.test")
	if len(list) != 1 {
		t.Fatalf("moves: %+v", list)
	}
	got := list[0]
	states := map[string]string{}
	for _, st := range got.Steps {
		states[st.Step] = st.State
	}
	if !got.Done || states[relocate.StepCopy] != stepOK || states[relocate.StepStart] != stepFailed || states[relocate.StepRestore] != stepOK || got.Steps[5].Error != "not ready after 1m0s" {
		t.Fatalf("steps: %+v", got.Steps)
	}

	// One cut halfway by a restart: a new server over the same folder.
	j2, _ := f.srv.newMove("n.test", "b", nil)
	f.srv.advance(j2, relocate.StepCopy, "copying")
	again := New(Options{Root: f.root})
	cut := again.listMoves("n.test")
	if len(cut) != 1 || !cut[0].Done || cut[0].Steps[0].State != stepFailed || cut[0].Error == "" {
		t.Fatalf("a move cut by a restart: %+v", cut)
	}
	if all := again.listMoves(""); len(all) != 2 || all[0].Domain != "n.test" {
		t.Fatalf("all moves, the newest first: %+v", all)
	}
}
