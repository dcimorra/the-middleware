package push

import (
	"context"
	"net/url"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/events"
)

// The kinds of notification, a closed list: what a device can ask for,
// and the only things this house pushes.
const (
	// KindIntrusion is a guard rule of the intrusion level firing (the
	// site is stopped by then: the rule acts first, then tells).
	KindIntrusion = "intrusion"
	// KindCritical is something that needs looking at today: a panic in
	// the supervisor, a critical vulnerability in a site's dependencies.
	KindCritical = "critical"
	// KindRepo is a repository that stopped pulling (the first failure of
	// a streak, not every two minutes).
	KindRepo = "repo"
	// KindTurn is a conversation's turn ending, for whoever left the
	// panel while the agent worked.
	KindTurn = "turn"
	// KindTest is the panel's "send a test", delivered whatever a device
	// chose.
	KindTest = "test"
)

// Kinds lists what a device can choose.
func Kinds() []string { return []string{KindIntrusion, KindCritical, KindRepo, KindTurn} }

func validKind(k string) bool {
	for _, x := range Kinds() {
		if x == k {
			return true
		}
	}
	return false
}

// For maps an event to the notification it means, or ok false. Every
// push this house sends comes from here.
func For(e events.Event) (Message, bool) {
	site := e.Subject
	link := func(tab string) string {
		if site == "" {
			return "/#/events"
		}
		return "/#/sites/" + url.PathEscape(site) + "/" + tab
	}
	switch e.Type {
	case "guard.intrusion":
		return Message{Kind: KindIntrusion, Title: "Intrusion on " + orHouse(site), Body: e.Message, URL: link("activity"), Tag: "intrusion-" + site}, true
	case "middleware.panic":
		return Message{Kind: KindCritical, Title: "The supervisor panicked", Body: e.Message, URL: "/#/events", Tag: "panic"}, true
	case "deps.critical":
		return Message{Kind: KindCritical, Title: "Critical vulnerability in " + orHouse(site), Body: e.Message, URL: link("overview"), Tag: "audit-" + site}, true
	case "repo.failed":
		return Message{Kind: KindRepo, Title: "A repository stopped pulling", Body: e.Message, URL: "/#/repos", Tag: "repo-" + site}, true
	case "autopilot.ready":
		// A site it finished waits for the operator (publish: ask).
		return Message{Kind: KindTurn, Title: "A site is ready to publish", Body: site + ": the autonomous agent finished it and asks you to publish it", URL: "/#/agents/network", Tag: "autopilot-" + site}, true
	case "autopilot.proposal":
		// Something it found and may not change itself: the operator
		// decides, and only knows to if told.
		return Message{Kind: KindTurn, Title: "The autonomous agent proposes a fix", Body: orHouse(site) + ": " + e.Message, URL: "/#/agents/network", Tag: "autopilot-proposal"}, true
	case "autopilot.stood_down":
		return Message{Kind: KindCritical, Title: "The autonomous agent stood down here", Body: e.Message, URL: "/#/agents/network", Tag: "autopilot-down"}, true
	case "crew.request":
		// An agent of the crew asks the operator: urgent is a check only
		// a person can pass (an SMS code, a captcha), and it waits on it.
		name, _ := e.Data["name"].(string)
		if name == "" {
			name = site
		}
		rid, _ := e.Data["request"].(string)
		where := "/#/agents/" + url.PathEscape(site)
		if urgent, _ := e.Data["urgent"].(bool); urgent {
			title := name + " needs you now"
			if again, _ := e.Data["again"].(bool); again {
				title = name + " is still waiting for you"
			}
			return Message{Kind: KindCritical, Title: title, Body: e.Message, URL: where, Tag: "crew-" + site + "-" + rid}, true
		}
		return Message{Kind: KindTurn, Title: name + " asks you something", Body: e.Message, URL: where, Tag: "crew-" + site + "-" + rid}, true
	case "agent.done", "agent.error":
		// The site door's conversations have a program on the other side,
		// not a person waiting.
		if o, _ := e.Data["origin"].(string); o == "site" {
			return Message{}, false
		}
		domain, id, _ := strings.Cut(site, "#")
		// Nor the autonomous agent's turns, or the ones it hands to sites:
		// it works all day with nobody waiting, and a push a turn would
		// bury the ones that matter. Its journal says what it did.
		if o, _ := e.Data["origin"].(string); o == "autopilot" || domain == "autopilot" || strings.HasPrefix(domain, "crew~") {
			return Message{}, false
		}
		where := "/#/sites/" + url.PathEscape(domain) + "/chat"
		if domain == "middleware" {
			where = "/#/house"
		}
		if id != "" {
			where += "?c=" + url.QueryEscape(id)
		}
		title, body := "The agent finished in "+domain, firstLine(e.Data["text"])
		if e.Type == "agent.error" {
			title, body = "The agent stopped with an error in "+domain, e.Message
		}
		return Message{Kind: KindTurn, Title: title, Body: body, URL: where, Tag: "turn-" + site}, true
	}
	return Message{}, false
}

func orHouse(site string) string {
	if site == "" {
		return "the house"
	}
	return site
}

func firstLine(v any) string {
	s, _ := v.(string)
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:157] + "…"
	}
	return s
}

// Notifier pushes what For maps, as the events happen.
type Notifier struct {
	Bus    *events.Bus
	Sender *Sender
	// OnError reports a failed delivery (an event, by the caller).
	OnError func(error)
}

// Run follows the bus until ctx ends. Deliveries run one at a time in
// their own goroutine, so a slow push service never holds the bus.
func (n *Notifier) Run(ctx context.Context) {
	ch, stop := n.Bus.Subscribe()
	defer stop()
	queue := make(chan Message, 64)
	go func() {
		for m := range queue {
			c, cancel := context.WithTimeout(ctx, 30*time.Second)
			if _, err := n.Sender.Send(c, m); err != nil && n.OnError != nil {
				n.OnError(err)
			}
			cancel()
		}
	}()
	defer close(queue)
	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-ch:
			if !ok {
				return
			}
			if m, ok := For(e); ok {
				select {
				case queue <- m:
				default: // a burst beyond 64 waiting pushes drops, not blocks
				}
			}
		}
	}
}
