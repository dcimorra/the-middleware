package autopilot

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// A Proposal is something it found that is not its to change (a site it
// did not create, the house itself) and asks the operator to decide.
// They lived in its notebook before, where the operator only met them by
// reading the whole notebook; the night it found why a site ran out of
// memory every hour, the finding sat there unread. Now each one is a card
// with two buttons: send it to be fixed, or dismiss it.
type Proposal struct {
	ID string    `json:"id"`
	At time.Time `json:"at"`
	// Domain is the site it is about; empty is the house itself.
	Domain string `json:"domain,omitempty"`
	Title  string `json:"title"`
	Text   string `json:"text"`
	// Status is open, sent (to be fixed), fixed, failed or dismissed.
	Status  string    `json:"status"`
	Decided time.Time `json:"decided,omitzero"`
	// Conversation and TurnID are where it was sent to be fixed.
	Conversation string    `json:"conversation,omitempty"`
	TurnID       string    `json:"turn_id,omitempty"`
	Ended        time.Time `json:"ended,omitzero"`
	// Outcome is the last line of what the fixing turn answered.
	Outcome string `json:"outcome,omitempty"`
}

// Proposal states.
const (
	ProposalOpen      = "open"
	ProposalSent      = "sent"
	ProposalFixed     = "fixed"
	ProposalFailed    = "failed"
	ProposalDismissed = "dismissed"
)

const (
	// maxOpenProposals bounds what waits for the operator: past it, the
	// operator has more to read than they will, and it is told so.
	maxOpenProposals = 20
	// keepProposals is how many decided ones are kept, newest first.
	keepProposals = 60
)

// ErrNoProposal is an id it does not have.
var ErrNoProposal = errors.New("no such proposal")

// Propose files a proposal. One still open for the same site and title is
// rewritten instead of repeated: it is the same finding, better known.
func (p *Pilot) Propose(domain, title, text string) (Proposal, error) {
	title, text = strings.TrimSpace(oneLine(title)), strings.TrimSpace(text)
	switch {
	case title == "":
		return Proposal{}, errors.New("a proposal needs a title")
	case len(title) > 200:
		return Proposal{}, errors.New("the title is longer than 200 characters")
	case text == "":
		return Proposal{}, errors.New("a proposal needs a text: what is wrong, why, and what to change")
	case len(text) > 16<<10:
		return Proposal{}, errors.New("the text is longer than 16 KB")
	}
	if domain != "" {
		found := false
		for _, s := range p.opts.Sites() {
			if s.Domain == domain {
				if s.OptOut {
					return Proposal{}, fmt.Errorf("%s: its manifest keeps the autonomous agent out (\"autopilot\": false)", domain)
				}
				found = true
				break
			}
		}
		if !found {
			return Proposal{}, fmt.Errorf("%s: no such site on this node", domain)
		}
	}
	now := p.opts.Now().UTC()
	p.mu.Lock()
	var out Proposal
	updated := false
	open := 0
	for i := range p.st.Proposals {
		x := &p.st.Proposals[i]
		if x.Status != ProposalOpen {
			continue
		}
		open++
		if x.Domain == domain && strings.EqualFold(x.Title, title) {
			x.Text, x.At = text, now
			out, updated = *x, true
		}
	}
	if !updated {
		if open >= maxOpenProposals {
			p.mu.Unlock()
			return Proposal{}, fmt.Errorf("%d proposals already wait for the operator; do not add more until they decide", open)
		}
		out = Proposal{ID: proposalID(), At: now, Domain: domain, Title: title, Text: text, Status: ProposalOpen}
		p.st.Proposals = append([]Proposal{out}, p.st.Proposals...)
		p.trimProposalsLocked()
	}
	p.saveLocked()
	p.mu.Unlock()
	verb := "proposed"
	if updated {
		verb = "rewrote its proposal"
	}
	p.Note(Entry{Kind: KindPropose, Domain: domain, Message: verb + ": " + title})
	if !updated {
		p.emit("autopilot.proposal", domain, "", title)
	}
	return out, nil
}

// Proposals are all it keeps, newest first.
func (p *Pilot) Proposals() []Proposal {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Proposal(nil), p.st.Proposals...)
}

// Send hands an open proposal to be fixed, in the operator's name: a new
// conversation of its site (or of the house) with the proposal as its
// first message. The operator's click is the decision; the turn that
// fixes it is an ordinary one, with the site's own jail and rights, not
// the agent's narrower ones.
func (p *Pilot) Send(id, note string) (Proposal, error) {
	if p.opts.Fix == nil {
		return Proposal{}, errors.New("nothing here can start a conversation")
	}
	p.mu.Lock()
	i := p.proposalLocked(id)
	if i < 0 {
		p.mu.Unlock()
		return Proposal{}, ErrNoProposal
	}
	x := p.st.Proposals[i]
	if x.Status != ProposalOpen && x.Status != ProposalFailed {
		p.mu.Unlock()
		return Proposal{}, fmt.Errorf("this proposal is %s already", x.Status)
	}
	p.mu.Unlock()
	name, turn, err := p.opts.Fix(x.Domain, x.Title, FixPrompt(x, note))
	if err != nil {
		return Proposal{}, err
	}
	now := p.opts.Now().UTC()
	p.mu.Lock()
	if i = p.proposalLocked(id); i >= 0 {
		y := &p.st.Proposals[i]
		y.Status, y.Decided, y.Conversation, y.TurnID, y.Ended, y.Outcome = ProposalSent, now, name, turn, time.Time{}, ""
		x = *y
	}
	p.saveLocked()
	p.mu.Unlock()
	p.Note(Entry{Kind: KindDecide, Domain: x.Domain, TurnID: turn, Message: "sent to be fixed: " + x.Title})
	return x, nil
}

// Dismiss closes a proposal unsent. Its next prompts say so, with the
// operator's reason when they give one, so it does not propose it again.
func (p *Pilot) Dismiss(id, note string) (Proposal, error) {
	note = clip(oneLine(note), 500)
	now := p.opts.Now().UTC()
	p.mu.Lock()
	i := p.proposalLocked(id)
	if i < 0 {
		p.mu.Unlock()
		return Proposal{}, ErrNoProposal
	}
	x := &p.st.Proposals[i]
	if x.Status != ProposalOpen && x.Status != ProposalFailed {
		p.mu.Unlock()
		return Proposal{}, fmt.Errorf("this proposal is %s already", x.Status)
	}
	x.Status, x.Decided, x.Outcome = ProposalDismissed, now, note
	out := *x
	p.saveLocked()
	p.mu.Unlock()
	p.Note(Entry{Kind: KindDecide, Domain: out.Domain, Message: "dismissed: " + out.Title})
	return out, nil
}

// fixEndedLocked is the end of a turn that fixes a proposal; called from
// Finished with mu held. It says whether the turn was one.
func (p *Pilot) fixEndedLocked(name, id, text string, failed bool) bool {
	for i := range p.st.Proposals {
		x := &p.st.Proposals[i]
		if x.Status != ProposalSent || x.Conversation != name || x.TurnID != id {
			continue
		}
		x.Status, x.Ended, x.Outcome = ProposalFixed, p.opts.Now().UTC(), clip(oneLine(lastLine(text)), 500)
		if failed {
			x.Status = ProposalFailed
		}
		p.saveLocked()
		p.j.add(Entry{At: x.Ended, Kind: KindDecide, Domain: x.Domain, TurnID: id, Message: x.Status + ": " + x.Title})
		return true
	}
	return false
}

func (p *Pilot) proposalLocked(id string) int {
	for i, x := range p.st.Proposals {
		if x.ID == id {
			return i
		}
	}
	return -1
}

// trimProposalsLocked keeps every open one and the newest decided ones.
func (p *Pilot) trimProposalsLocked() {
	kept, decided := p.st.Proposals[:0], 0
	for _, x := range p.st.Proposals {
		if x.Status != ProposalOpen && x.Status != ProposalSent {
			if decided >= keepProposals {
				continue
			}
			decided++
		}
		kept = append(kept, x)
	}
	p.st.Proposals = kept
}

// proposalsTextLocked is the prompt's part about them: what waits, and
// what the operator decided lately, so it neither repeats a dismissed one
// nor forgets to look at what a fix did.
func (p *Pilot) proposalsTextLocked() string {
	var b strings.Builder
	since := p.opts.Now().Add(-14 * 24 * time.Hour)
	for _, x := range p.st.Proposals {
		where := x.Domain
		if where == "" {
			where = "the house"
		}
		switch {
		case x.Status == ProposalOpen:
			fmt.Fprintf(&b, "- Waiting for the operator since %s: %q (%s).\n", x.At.Format("Jan 2 15:04"), x.Title, where)
		case x.Status == ProposalSent:
			fmt.Fprintf(&b, "- Sent to be fixed in %s, at work: %q.\n", x.Conversation, x.Title)
		case x.Decided.After(since):
			s := fmt.Sprintf("- %s: %q (%s)", x.Status, x.Title, where)
			if x.Outcome != "" {
				s += ": " + x.Outcome
			}
			b.WriteString(s + ".\n")
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return "- Your proposals (`middleware autopilot proposals` for the texts):\n" + indent(b.String())
}

// FixPrompt is the first message of the conversation that fixes x.
func FixPrompt(x Proposal, note string) string {
	var b strings.Builder
	b.WriteString("The autonomous agent proposed the change below and I, the operator, approved it. Fix it: check first that what it says is still true, make the change, test it the way a visitor would meet it, deploy it, and tell me what you changed and how you checked it. If you find the proposal is wrong, change nothing and tell me why.\n")
	if note = strings.TrimSpace(note); note != "" {
		b.WriteString("\nMy note: " + note + "\n")
	}
	b.WriteString("\n# " + x.Title + "\n\n" + x.Text + "\n")
	return b.String()
}

func indent(s string) string {
	return "  " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n  ") + "\n"
}

func proposalID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
