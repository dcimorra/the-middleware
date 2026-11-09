package autopilot

import (
	"strings"
	"testing"

	"github.com/dcimorra/the-middleware/internal/events"
)

// What it finds and may not change is a proposal the operator sees, sends
// to be fixed in their own name or dismisses; it reads every decision
// and every fix's outcome on its next wakes, and all of it survives a
// restart.
func TestAProposalIsSentToBeFixedAndItHearsHowItWent(t *testing.T) {
	r := newRig(t)
	x, err := r.p.Propose("shop.test", "Bound the database pool", "The pool has no limit; requests pile up in memory.")
	if err != nil {
		t.Fatal(err)
	}
	// The same finding again rewrites the open one.
	if y, err := r.p.Propose("shop.test", "bound the database pool", "Better known now."); err != nil || y.ID != x.ID {
		t.Fatalf("the same proposal again: %+v %v", y, err)
	}
	if ps := r.p.Proposals(); len(ps) != 1 || ps[0].Text != "Better known now." {
		t.Fatalf("proposals: %+v", ps)
	}
	for _, bad := range [][3]string{{"private.test", "t", "x"}, {"nowhere.test", "t", "x"}, {"shop.test", "", "x"}, {"shop.test", "t", ""}} {
		if _, err := r.p.Propose(bad[0], bad[1], bad[2]); err == nil {
			t.Errorf("filed: %v", bad)
		}
	}

	sent, err := r.p.Send(x.ID, "Keep it small.")
	if err != nil || sent.Status != ProposalSent || sent.Conversation != "shop.test#fx" {
		t.Fatalf("sent: %+v %v", sent, err)
	}
	if !strings.Contains(r.fixes[1], "I, the operator, approved it") || !strings.Contains(r.fixes[1], "Keep it small.") || !strings.Contains(r.fixes[1], "Better known now.") {
		t.Errorf("the fix's message: %s", r.fixes[1])
	}
	if _, err := r.p.Send(x.ID, ""); err == nil {
		t.Error("a proposal was sent twice")
	}

	r.p.Pass()
	if len(r.sent) != 1 || !strings.Contains(r.sent[0], "Sent to be fixed in shop.test#fx") {
		t.Fatalf("its prompt does not say the proposal is being fixed: %v", r.sent)
	}
	r.p.Finished(events.Event{Type: "agent.done", Subject: "shop.test#fx", Data: map[string]any{"turn_id": "fix1", "text": "Done.\nThe pool is capped at 10 and deployed."}})
	r.end("Summary: looked\nNext: 1h, later", 1000, 200000)

	// A restart keeps it.
	r.p = r.build()
	ps := r.p.Proposals()
	if len(ps) != 1 || ps[0].Status != ProposalFixed || ps[0].Outcome != "The pool is capped at 10 and deployed." {
		t.Fatalf("after the fix: %+v", ps)
	}
	z, _ := r.p.Propose("", "Rotate the access logs daily", "They are weekly.")
	if _, err := r.p.Dismiss(z.ID, "the law says weekly"); err != nil {
		t.Fatal(err)
	}
	r.advance(2 * 60 * 60 * 1e9)
	r.p.Wake()
	r.p.Pass()
	last := r.sent[len(r.sent)-1]
	for _, want := range []string{`fixed: "Bound the database pool" (shop.test): The pool is capped at 10`, `dismissed: "Rotate the access logs daily" (the house): the law says weekly`} {
		if !strings.Contains(last, want) {
			t.Errorf("its prompt lacks %q:\n%s", want, last)
		}
	}
	kinds := strings.Join(r.kinds(), " ")
	if !strings.Contains(kinds, KindPropose) || !strings.Contains(kinds, KindDecide) {
		t.Errorf("journal: %s", kinds)
	}
}
