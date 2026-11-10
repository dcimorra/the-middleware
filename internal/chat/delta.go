package chat

import (
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/events"
)

// Delta is a piece of a conversation's answer while it is being written
// (agent.KindDelta): what a site's code shows letter by letter before the
// whole message comes.
type Delta struct {
	TurnID string `json:"turn_id"`
	Text   string `json:"text"`
}

// Watched is one thing a watcher of a conversation receives, in the order
// it happened: an event the conversation emitted (agent.*), or a piece of
// the answer being written.
type Watched struct {
	Event *events.Event
	Delta *Delta
}

// watchers hand a conversation's events and pieces to whoever watches it,
// on one channel, so their order is the turn's. The pieces do not go
// through the event bus: a chapter is thousands of them, which would push
// every other event out of the bus's memory, write them all to the
// journal and fill the panel's streams, whose buffers drop what does not
// fit, real events included. Tried first: the pieces on a channel of
// their own beside the bus's; a reader's select picked either, and a
// message's last pieces could come after it.
type watchers struct {
	mu   sync.Mutex
	next int
	subs map[string]map[int]chan Watched
}

// Watch returns what happens in the conversation name from now on, and
// the function that stops it. A reader that does not keep up loses what
// does not fit, as on the bus.
func (m *Manager) Watch(name string) (<-chan Watched, func()) {
	w := &m.watchers
	ch := make(chan Watched, 4096)
	w.mu.Lock()
	if w.subs == nil {
		w.subs = map[string]map[int]chan Watched{}
	}
	if w.subs[name] == nil {
		w.subs[name] = map[int]chan Watched{}
	}
	id := w.next
	w.next++
	w.subs[name][id] = ch
	w.mu.Unlock()
	return ch, func() {
		w.mu.Lock()
		delete(w.subs[name], id)
		if len(w.subs[name]) == 0 {
			delete(w.subs, name)
		}
		w.mu.Unlock()
	}
}

func (w *watchers) send(name string, item Watched) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, ch := range w.subs[name] {
		select {
		case ch <- item:
		default:
		}
	}
}

func (w *watchers) event(e events.Event) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	w.send(e.Subject, Watched{Event: &e})
}
