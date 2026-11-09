package autopilot

import (
	"strings"
	"testing"
	"time"
)

// The operator asks it for a site from its page: it is woken at once, the
// order is in its prompt in the operator's words, the next site it makes
// is taken as the order and not from the week's allowance, and an order
// taken back is told and leaves the prompt. All of it survives a restart.
func TestASiteTheOperatorAsksForIsOnTopOfTheWeek(t *testing.T) {
	r := newRig(t)
	r.cfg.SitesPerWeek = 1
	r.p.Pass()
	r.end("Summary: looked\nNext: 6h, nothing to do", 1000, 200000)
	r.advance(time.Minute)
	// Its own site uses the whole week.
	r.p.Created("own.test")
	if err := r.p.MayCreate("more.test"); err == nil {
		t.Fatal("a second site of its own in a week of one")
	}

	o, err := r.p.Order("A guide to electric bikes, in Spanish")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.p.MayCreate("bikes.test"); err != nil {
		t.Fatalf("the order cannot be made: %v", err)
	}
	r.p.Pass()
	if len(r.sent) != 2 {
		t.Fatalf("an order does not wake it: %d turns", len(r.sent))
	}
	if p := r.sent[1]; !strings.Contains(p, "Sites the operator asked you for") || !strings.Contains(p, "A guide to electric bikes, in Spanish") || !strings.Contains(p, o.ID) {
		t.Errorf("the order is not in its prompt:\n%s", p)
	}
	r.p.Created("bikes.test")
	r.end("Summary: started the bikes guide\nNext: 1h, keep building", 1000, 200000)

	r.p = r.build()
	st := r.p.Status()
	if len(st.State.Orders) != 1 || st.State.Orders[0].Domain != "bikes.test" || st.State.Orders[0].Done.IsZero() {
		t.Fatalf("the order after a restart: %+v", st.State.Orders)
	}
	if c := st.State.Created[1]; c.Domain != "bikes.test" || c.Order != o.ID {
		t.Errorf("the site does not name its order: %+v", c)
	}
	if st.SitesLeft != 0 {
		t.Errorf("sites left of its own: %d", st.SitesLeft)
	}
	if err := r.p.MayCreate("third.test"); err == nil {
		t.Error("a made order still opens the door")
	}

	// One taken back is told, and is no longer asked of it.
	o2, _ := r.p.Order("")
	if err := r.p.CancelOrder(o2.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.p.CancelOrder(o2.ID); err == nil {
		t.Error("taken back twice")
	}
	st = r.p.Status()
	if len(st.State.Alerts) == 0 || !strings.Contains(st.State.Alerts[len(st.State.Alerts)-1], "took back") {
		t.Errorf("not told: %v", st.State.Alerts)
	}
	if err := r.p.MayCreate("third.test"); err == nil {
		t.Error("a cancelled order opens the door")
	}

	r.cfg.Enabled = false
	if _, err := r.p.Order("x"); err == nil {
		t.Error("an order to an agent switched off")
	}
}
