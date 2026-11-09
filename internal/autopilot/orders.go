package autopilot

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// An Order is a site the operator asked it for, from the Sites card of
// its page. Until then a new site was only ever its own idea, at the
// pace its weekly allowance let it; the operator wanted to ask for one
// when they had an idea of their own, or simply wanted another. An order
// is not taken from the allowance: the operator asking is the operator's
// decision, and a full week would leave the order waiting for nothing.
type Order struct {
	ID string    `json:"id"`
	At time.Time `json:"at"`
	// Text is what the operator asked for; empty leaves the idea to it.
	Text string `json:"text,omitempty"`
	// Domain is the site created for it, and Done when.
	Domain string    `json:"domain,omitempty"`
	Done   time.Time `json:"done,omitzero"`
	// Cancelled is the operator taking it back before it was made.
	Cancelled time.Time `json:"cancelled,omitzero"`
}

func (o Order) open() bool { return o.Done.IsZero() && o.Cancelled.IsZero() }

const (
	// maxOpenOrders bounds what it is asked for at once.
	maxOpenOrders = 10
	// keepOrders is how many ended ones are kept.
	keepOrders = 40
)

// ErrNoOrder is an order it does not have, or one already made.
var ErrNoOrder = errors.New("no such open order")

// Order asks it for a new site and has it look now.
func (p *Pilot) Order(text string) (Order, error) {
	if !p.opts.Settings().Limits().Enabled {
		return Order{}, ErrOff
	}
	text = strings.TrimSpace(text)
	if len(text) > 16<<10 {
		return Order{}, errors.New("the order is longer than 16 KB")
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	o := Order{ID: hex.EncodeToString(b), At: p.opts.Now().UTC(), Text: text}
	p.mu.Lock()
	n := 0
	for _, x := range p.st.Orders {
		if x.open() {
			n++
		}
	}
	if n >= maxOpenOrders {
		p.mu.Unlock()
		return Order{}, fmt.Errorf("%d sites asked for are still to be made", n)
	}
	p.st.Orders = append(p.st.Orders, o)
	p.trimOrdersLocked()
	p.st.NextWake, p.st.NextReason, p.st.Skipped = time.Time{}, "the operator asked for a new site", ""
	p.saveLocked()
	p.mu.Unlock()
	msg := "the operator asked for a new site"
	if text != "" {
		msg += ": " + clip(oneLine(text), 300)
	}
	p.Note(Entry{Kind: KindOrder, Message: msg})
	p.emit("autopilot.order", o.ID, "", msg)
	return o, nil
}

// CancelOrder takes back an order not made yet.
func (p *Pilot) CancelOrder(id string) error {
	p.mu.Lock()
	found := false
	for i := range p.st.Orders {
		if p.st.Orders[i].ID == id && p.st.Orders[i].open() {
			p.st.Orders[i].Cancelled = p.opts.Now().UTC()
			found = true
		}
	}
	if found {
		p.saveLocked()
	}
	p.mu.Unlock()
	if !found {
		return ErrNoOrder
	}
	p.Note(Entry{Kind: KindOrder, Message: "the operator took back a site they had asked for"})
	p.alert("The operator took back the site they had asked for (order " + id + "); do not make it, and drop what you started for it unless it is a site of yours anyway.")
	return nil
}

// orderForLocked gives the oldest open order to the site just created;
// called with mu held. It returns the order's id, or "" when none was open.
func (p *Pilot) orderForLocked(domain string, now time.Time) string {
	for i := range p.st.Orders {
		if p.st.Orders[i].open() {
			p.st.Orders[i].Domain, p.st.Orders[i].Done = domain, now
			return p.st.Orders[i].ID
		}
	}
	return ""
}

func (p *Pilot) openOrdersLocked() []Order {
	var out []Order
	for _, o := range p.st.Orders {
		if o.open() {
			out = append(out, o)
		}
	}
	return out
}

// trimOrdersLocked keeps every open order and the last ended ones.
func (p *Pilot) trimOrdersLocked() {
	ended := 0
	for _, o := range p.st.Orders {
		if !o.open() {
			ended++
		}
	}
	if ended <= keepOrders {
		return
	}
	kept := p.st.Orders[:0]
	for _, o := range p.st.Orders {
		if !o.open() && ended > keepOrders {
			ended--
			continue
		}
		kept = append(kept, o)
	}
	p.st.Orders = kept
}

// ordersTextLocked is the prompt's part on what the operator asked for.
// Their words are the operator's own, like the guidelines, so they are
// not fenced as untrusted.
func (p *Pilot) ordersTextLocked() string {
	open := p.openOrdersLocked()
	if len(open) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n## Sites the operator asked you for\nThe operator asked for these from your page. They come before your own ideas and do not count against your weekly allowance: the next site you create is taken as the oldest one here. Build each as you would one of yours, a finished product before it is public.\n")
	for _, o := range open {
		what := o.Text
		if what == "" {
			what = "(no details: a new site of your choosing, the best idea you have)"
		}
		fmt.Fprintf(&b, "\n### Asked %s (order %s)\n%s\n", o.At.UTC().Format("2006-01-02 15:04"), o.ID, what)
	}
	return b.String()
}
