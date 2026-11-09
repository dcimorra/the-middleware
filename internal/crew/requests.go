package crew

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/autopilot"
)

// What became of a request.
const (
	StatusOpen      = "open"
	StatusAnswered  = "answered"
	StatusDismissed = "dismissed"
)

// Field is a credential or a piece of data a request asks for: what the
// operator types goes to the agent's vault under Key, not into the
// request, so a password never sits in requests.json or a prompt.
type Field struct {
	Key string `json:"key"`
	// Secret is a value the panel hides as it is typed (a password).
	Secret bool `json:"secret,omitempty"`
}

// Request is something an agent asked the operator.
type Request struct {
	ID    string    `json:"id"`
	At    time.Time `json:"at"`
	Title string    `json:"title"`
	Text  string    `json:"text,omitempty"`
	// Fields are what it wants in its vault, Options choices it offers.
	Fields  []Field  `json:"fields,omitempty"`
	Options []string `json:"options,omitempty"`
	// Urgent is a check only a person can pass: pushed as critical, and
	// pushed again while it stays open.
	Urgent bool   `json:"urgent,omitempty"`
	Status string `json:"status"`
	// What the operator answered: words, a choice, the keys they filled.
	Answer   string    `json:"answer,omitempty"`
	Choice   string    `json:"choice,omitempty"`
	Filled   []string  `json:"filled,omitempty"`
	Answered time.Time `json:"answered,omitzero"`
	// Reminded is the second push of an urgent request.
	Reminded bool `json:"reminded,omitempty"`
	// Told is an answer its turn has been given.
	Told bool `json:"told,omitempty"`
}

// Bounds on requests.
const (
	maxOpen     = 20
	maxKept     = 200
	maxTitle    = 200
	maxText     = 16 << 10
	maxFields   = 20
	maxOptions  = 6
	maxAnswer   = 16 << 10
	maxFieldVal = 16 << 10
)

func newRequestID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "r" + hex.EncodeToString(b[:])
}

// Ask records a request from the agent and tells the operator.
func (c *Crew) Ask(id string, r Request) (Request, error) {
	r.Title = strings.TrimSpace(r.Title)
	r.Text = strings.TrimSpace(r.Text)
	if r.Title == "" {
		return Request{}, errors.New("a request needs a title")
	}
	if len(r.Title) > maxTitle || len(r.Text) > maxText {
		return Request{}, fmt.Errorf("the title is %d bytes at most and the text %d", maxTitle, maxText)
	}
	if len(r.Fields) > maxFields || len(r.Options) > maxOptions {
		return Request{}, fmt.Errorf("%d fields and %d options at most", maxFields, maxOptions)
	}
	seen := map[string]bool{}
	for i, f := range r.Fields {
		f.Key = strings.ToUpper(strings.TrimSpace(f.Key))
		if err := validKey(f.Key); err != nil {
			return Request{}, err
		}
		if seen[f.Key] {
			return Request{}, fmt.Errorf("%s is asked for twice", f.Key)
		}
		seen[f.Key] = true
		r.Fields[i] = f
	}
	opts := r.Options[:0]
	for _, o := range r.Options {
		if o = strings.TrimSpace(o); o != "" {
			opts = append(opts, autopilot.Clip(o, 200))
		}
	}
	r.Options = opts
	c.mu.Lock()
	mb := c.get(id)
	if mb == nil {
		c.mu.Unlock()
		return Request{}, ErrNoAgent
	}
	open := 0
	for _, x := range mb.reqs {
		if x.Status == StatusOpen {
			open++
			if strings.EqualFold(x.Title, r.Title) {
				c.mu.Unlock()
				return x, fmt.Errorf("you asked that already (%s) and it is still open", x.ID)
			}
		}
	}
	if open >= maxOpen {
		c.mu.Unlock()
		return Request{}, fmt.Errorf("%d of your requests are open already; wait for the operator", open)
	}
	r.ID, r.At, r.Status = newRequestID(), c.now(), StatusOpen
	r.Answer, r.Choice, r.Filled, r.Answered, r.Reminded, r.Told = "", "", nil, time.Time{}, false, false
	mb.reqs = append(mb.reqs, r)
	c.trimLocked(mb)
	c.saveRequestsLocked(mb)
	c.note(mb, Entry{Kind: KindRequest, Message: autopilot.Clip(r.Title, 300)})
	name := mb.a.Name
	c.mu.Unlock()
	c.tell(id, name, r, false)
	return r, nil
}

// tell is the event the push notifier turns into a notification.
func (c *Crew) tell(id, name string, r Request, again bool) {
	level := ""
	if r.Urgent {
		level = "warn"
	}
	c.emit("crew.request", id, level, r.Title, map[string]any{"request": r.ID, "urgent": r.Urgent, "name": name, "again": again})
}

// remindLocked pushes an urgent request again, once, when it has waited
// an hour: one push is easy to miss, and the agent cannot move on that
// path without it.
func (c *Crew) remindLocked(mb *member, now time.Time) {
	for i := range mb.reqs {
		r := &mb.reqs[i]
		if r.Status == StatusOpen && r.Urgent && !r.Reminded && now.Sub(r.At) >= remindOpen {
			r.Reminded = true
			c.saveRequestsLocked(mb)
			go c.tell(mb.a.ID, mb.a.Name, *r, true)
		}
	}
}

// trimLocked keeps the open requests and the last answered ones.
func (c *Crew) trimLocked(mb *member) {
	if len(mb.reqs) <= maxKept {
		return
	}
	var kept []Request
	drop := len(mb.reqs) - maxKept
	for _, r := range mb.reqs {
		if drop > 0 && r.Status != StatusOpen && r.Told {
			drop--
			continue
		}
		kept = append(kept, r)
	}
	mb.reqs = kept
}

// Reply is the operator's answer to a request.
type Reply struct {
	Answer string `json:"answer,omitempty"`
	Choice string `json:"choice,omitempty"`
	// Values fill the request's fields, by key, into the vault.
	Values  map[string]string `json:"values,omitempty"`
	Dismiss bool              `json:"dismiss,omitempty"`
}

// Answer closes a request: what was typed into its fields goes to the
// vault, the words and the choice to the agent's next wake, which comes
// within a minute.
func (c *Crew) Answer(id, rid string, rep Reply) (Request, error) {
	if len(rep.Answer) > maxAnswer {
		return Request{}, fmt.Errorf("the answer is %d bytes at most", maxAnswer)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	mb := c.get(id)
	if mb == nil {
		return Request{}, ErrNoAgent
	}
	i := slices.IndexFunc(mb.reqs, func(r Request) bool { return r.ID == rid })
	if i < 0 {
		return Request{}, fmt.Errorf("%s: no such request", rid)
	}
	r := &mb.reqs[i]
	if r.Status != StatusOpen {
		return *r, fmt.Errorf("%s was %s already", rid, r.Status)
	}
	if rep.Choice != "" && !slices.Contains(r.Options, rep.Choice) {
		return Request{}, fmt.Errorf("%q is not one of the options", rep.Choice)
	}
	var filled []string
	if !rep.Dismiss {
		vals := map[string]string{}
		for _, f := range r.Fields {
			if v, ok := rep.Values[f.Key]; ok && v != "" {
				if len(v) > maxFieldVal {
					return Request{}, fmt.Errorf("%s is longer than %d bytes", f.Key, maxFieldVal)
				}
				vals[f.Key] = v
			}
		}
		for k := range rep.Values {
			if !slices.ContainsFunc(r.Fields, func(f Field) bool { return f.Key == k }) {
				return Request{}, fmt.Errorf("%s is not a field of this request", k)
			}
		}
		if len(vals) > 0 {
			if err := c.setVaultLocked(id, vals); err != nil {
				return Request{}, err
			}
			for k := range vals {
				filled = append(filled, k)
			}
			slices.Sort(filled)
		}
		if len(filled) == 0 && strings.TrimSpace(rep.Answer) == "" && rep.Choice == "" {
			return Request{}, errors.New("answer something, fill a field, pick an option or dismiss it")
		}
	}
	r.Status = StatusAnswered
	if rep.Dismiss {
		r.Status = StatusDismissed
	}
	r.Answer, r.Choice, r.Filled, r.Answered, r.Told = strings.TrimSpace(rep.Answer), rep.Choice, filled, c.now(), false
	c.saveRequestsLocked(mb)
	msg := r.Status + ": " + r.Title
	if len(filled) > 0 {
		msg += " (" + strings.Join(filled, ", ") + " into the vault)"
	}
	c.note(mb, Entry{Kind: KindAnswer, Message: autopilot.Clip(msg, 300)})
	go c.emit("crew.answered", id, "", msg, map[string]any{"request": rid})
	return *r, nil
}

// Requests is an agent's requests, the open ones first, then the newest.
func (c *Crew) Requests(id string) ([]Request, error) {
	st, err := c.Get(id)
	if err != nil {
		return nil, err
	}
	return st.Requests, nil
}
