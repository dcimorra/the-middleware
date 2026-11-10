package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/dcimorra/the-middleware/internal/events"
)

// chat sends one message to a conversation through the running
// supervisor and prints the turn as it goes.
//
// It used to run the turn inside the CLI's own process, with a registry of
// providers built from nothing: no module configuration, so no
// subscription token and no API key. A Claude turn started that way had
// only a login under its own folder, which a site's folder never has, and
// answered "Not logged in · Please run /login" while the panel, whose turns
// the supervisor starts with the Claude module's login, worked on the very
// same site (2026-11-10, the house conversation sending a site its brief).
// The turn is the supervisor's now, as the panel's are: same login, same
// queue, same jail. Ctrl-C stops it, as it did before.
func (e *Env) chat(args []string) error {
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	var c common
	c.bind(fs)
	model := fs.String("model", "", "model name or alias (default: the conversation's)")
	rest, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(rest) < 2 {
		return errors.New("usage: middleware chat <domain> <text>")
	}
	domain, text := rest[0], strings.Join(rest[1:], " ")
	if strings.TrimSpace(text) == "" {
		return errors.New("usage: middleware chat <domain> <text>")
	}
	cl, err := e.client(c)
	if err != nil {
		return err
	}
	path := "/api/v1/sites/" + url.PathEscape(domain) + "/chat"
	if domain == "middleware" {
		path = "/api/v1/chat/middleware"
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The stream is open before the message goes: the server subscribes
	// before it answers the headers, so no event of the turn is missed,
	// however fast it ends.
	q := url.Values{"type": {"agent."}, "subject": {domain}}
	body, err := cl.Stream(ctx, "/api/v1/events/stream?"+q.Encode())
	if err != nil {
		return err
	}
	defer body.Close()
	var sent struct {
		TurnID string `json:"turn_id"`
		Queued bool   `json:"queued"`
	}
	if err := cl.Do("POST", path, map[string]string{"text": text, "model": *model}, &sent); err != nil {
		return err
	}
	if sent.Queued {
		fmt.Fprintln(e.Stderr, "queued: a turn is at work in "+domain+"; this message goes when it ends")
		return nil
	}
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var ev events.Event
		if json.Unmarshal([]byte(data), &ev) != nil || ev.Subject != domain {
			continue
		}
		if id, _ := ev.Data["turn_id"].(string); id != sent.TurnID {
			continue
		}
		switch ev.Type {
		case "agent.text":
			fmt.Fprintln(e.Stdout, ev.Data["text"])
		case "agent.tool":
			fmt.Fprintf(e.Stderr, "[%v]\n", ev.Data["tool"])
		case "agent.jail_violation":
			fmt.Fprintf(e.Stderr, "jail violation: %v\n", ev.Data["paths"])
		case "agent.done":
			if cost, ok := ev.Data["cost"].(float64); ok && cost > 0 {
				fmt.Fprintf(e.Stderr, "cost: $%.4f\n", cost)
			}
			return nil
		case "agent.error":
			return errors.New(ev.Message)
		}
	}
	if ctx.Err() != nil {
		// Interrupted by the person: the turn is stopped, as it was when
		// it ran in this process.
		_ = cl.Do("POST", path+"/stop", nil, nil)
		return errors.New("interrupted")
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return errors.New("the supervisor closed the stream before the turn ended; it goes on, follow it in the panel")
}
