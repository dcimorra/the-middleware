package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/autopilot"
	"github.com/dcimorra/the-middleware/internal/visits"
)

const autopilotUsage = `usage: middleware autopilot <command>

  status              on or off, its limits, where it stands, its next wake
  journal [--limit N] what it did: wakes, turns, commands, sites, refusals
  sites               the sites it created and whether each is published
  on | off            switch it on (it looks at once) or off (a turn at work ends)
  stop                the kill switch: off, and every turn of it cut now
  wake                have it look now
  move <node>         hand it to another node of the network (one runs per network)
  ask <domain> -      (the agent) hand work to a site's conversation, the text on stdin
  publish <domain>    (the agent) one of its sites is finished: publish it
`

// autopilot is `middleware autopilot`: the operator's switch and the
// agent's own commands (chat.AutopilotCommands), the same API for both;
// the house decides which caller may do what.
func (e *Env) autopilot(args []string) error {
	if len(args) == 0 || args[0] == "help" {
		fmt.Fprint(e.Stderr, autopilotUsage)
		return nil
	}
	sub, args := args[0], args[1:]
	fs := flag.NewFlagSet("autopilot "+sub, flag.ContinueOnError)
	var c common
	c.bind(fs)
	limit := fs.Int("limit", 50, "journal: how many lines")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	cl, err := e.client(c)
	if err != nil {
		return err
	}
	base := "/api/v1/autopilot"
	switch sub {
	case "status":
		var raw []byte
		if err := cl.Do("GET", base, nil, &raw); err != nil {
			return err
		}
		if c.json {
			e.printJSON(raw)
			return nil
		}
		var st autopilot.Status
		if err := json.Unmarshal(raw, &st); err != nil {
			return err
		}
		return e.printPilot(st)
	case "sites":
		var st autopilot.Status
		if err := cl.Do("GET", base, nil, &st); err != nil {
			return err
		}
		if len(st.State.Created) == 0 {
			fmt.Fprintln(e.Stdout, "it has created no site")
			return nil
		}
		rows := [][]string{}
		for _, s := range st.State.Created {
			state := "being built, off the internet"
			switch {
			case s.Published:
				state = "published"
			case s.Waiting:
				state = "finished, waiting for the operator"
			}
			rows = append(rows, []string{s.Domain, s.At.Local().Format("2006-01-02 15:04"), state})
		}
		Table(e.Stdout, []string{"SITE", "CREATED", "STATE"}, rows)
		return nil
	case "journal", "log":
		var raw []byte
		if err := cl.Do("GET", base+"/journal?limit="+strconv.Itoa(*limit), nil, &raw); err != nil {
			return err
		}
		if c.json {
			e.printJSON(raw)
			return nil
		}
		var list []autopilot.Entry
		if err := json.Unmarshal(raw, &list); err != nil {
			return err
		}
		for _, en := range list {
			line := en.At.Local().Format("01-02 15:04") + "  " + fmt.Sprintf("%-8s", en.Kind)
			if en.Domain != "" {
				line += " " + en.Domain + ":"
			}
			line += " " + en.Message
			if en.Next != nil {
				line += " (next " + en.Next.Local().Format("15:04") + ")"
			}
			fmt.Fprintln(e.Stdout, line)
			for _, cmd := range en.Commands {
				fmt.Fprintln(e.Stdout, "              $ "+cmd)
			}
		}
		return nil
	case "on", "off", "stop", "wake":
		done := map[string]string{"on": "switched on: it looks now", "off": "switched off: a turn at work ends on its own", "stop": "stopped: switched off and its turns cut", "wake": "it looks on its next pass"}[sub]
		return e.simple(c, cl, "POST", base+"/"+sub, nil, done)
	case "move":
		if len(pos) != 1 {
			return fmt.Errorf("usage: middleware autopilot move <node>")
		}
		return e.simple(c, cl, "POST", base+"/move", map[string]string{"node": pos[0]}, "moved to "+pos[0]+": it starts there with its notebook, and is off here")
	case "ask":
		if len(pos) != 2 || pos[1] != "-" {
			return fmt.Errorf("usage: middleware autopilot ask <domain> - (the message on stdin)")
		}
		text, err := io.ReadAll(e.Stdin)
		if err != nil {
			return err
		}
		var out struct {
			TurnID string `json:"turn_id"`
		}
		if err := cl.Do("POST", base+"/ask", map[string]string{"domain": strings.ToLower(pos[0]), "text": string(text)}, &out); err != nil {
			return err
		}
		fmt.Fprintf(e.Stdout, "handed to %s (turn %s): its answer reaches you on your next wake, and you are woken when it ends\n", pos[0], out.TurnID)
		return nil
	case "publish", "ready":
		if len(pos) != 1 {
			return fmt.Errorf("usage: middleware autopilot publish <domain>")
		}
		var out struct {
			Published bool `json:"published"`
		}
		if err := cl.Do("POST", base+"/sites/"+url.PathEscape(strings.ToLower(pos[0]))+"/ready", nil, &out); err != nil {
			return err
		}
		if out.Published {
			fmt.Fprintf(e.Stdout, "%s is being published: `middleware site published` shows how it went\n", pos[0])
		} else {
			fmt.Fprintf(e.Stdout, "%s is finished and waits for the operator to publish it\n", pos[0])
		}
		return nil
	}
	fmt.Fprint(e.Stderr, autopilotUsage)
	return fmt.Errorf("autopilot: unknown command %q", sub)
}

func (e *Env) printPilot(st autopilot.Status) error {
	on := "off"
	if st.Enabled {
		on = "on"
	}
	l := st.Limits
	fmt.Fprintf(e.Stdout, "autonomous agent: %s (%s %s)", on, l.Provider, l.Model)
	if st.Node != "" {
		fmt.Fprintf(e.Stdout, " on %s", st.Node)
	}
	fmt.Fprintln(e.Stdout)
	for _, n := range st.Network {
		state := fmt.Sprintf("%d sites, %d in trouble", len(n.Sites), n.Trouble)
		if !n.OK {
			state = "not answering"
		}
		fmt.Fprintf(e.Stdout, "node %s: %s\n", n.Name, state)
	}
	if st.Running {
		fmt.Fprintf(e.Stdout, "working now, since %s\n", st.State.TurnStarted.Local().Format("15:04"))
	} else if !st.State.NextWake.IsZero() && st.Enabled {
		fmt.Fprintf(e.Stdout, "next wake: %s (%s)\n", st.State.NextWake.Local().Format("2006-01-02 15:04"), orDash(st.State.NextReason))
	}
	if st.State.Skipped != "" {
		fmt.Fprintf(e.Stdout, "held back: %s\n", st.State.Skipped)
	}
	if st.ContextPercent >= 0 {
		fmt.Fprintf(e.Stdout, "context: %d of %d tokens (%d%%), a new session at %d%%\n", st.State.Context, st.State.Window, st.ContextPercent, l.ContextPercent)
	}
	for _, w := range st.Quota {
		reset := ""
		if !w.ResetsAt.IsZero() {
			reset = ", resets " + w.ResetsAt.Local().Format("Jan 2 15:04")
		}
		fmt.Fprintf(e.Stdout, "subscription %s: %.0f%% used%s (it stops at %.0f%%)\n", w.Name, w.Used, reset, l.QuotaPercent)
	}
	zones := "any name"
	if len(l.Zones) > 0 {
		zones = strings.Join(l.Zones, ", ")
	}
	fmt.Fprintf(e.Stdout, "new sites: %d more this week (of %d), under %s, publish: %s\n", st.SitesLeft, l.SitesPerWeek, zones, l.Publish)
	fmt.Fprintf(e.Stdout, "wakes between %s and %s apart\n", time.Duration(l.MinMinutes)*time.Minute, time.Duration(l.MaxMinutes)*time.Minute)
	for _, d := range st.State.Delegated {
		fmt.Fprintf(e.Stdout, "at work in %s since %s\n", d.Domain, d.At.Local().Format("15:04"))
	}
	if st.State.LastResult != "" {
		fmt.Fprintf(e.Stdout, "last turn: %s\n", st.State.LastResult)
	}
	return nil
}

// siteVisits is `middleware site visits`: the panel's Visits tab in a few
// lines, for an agent that wants to know whether a site brings anyone.
func (e *Env) siteVisits(c common, cl *Client, domain string, days int) error {
	var raw []byte
	if err := cl.Do("GET", "/api/v1/sites/"+url.PathEscape(domain)+"/visits?days="+strconv.Itoa(max(1, days)), nil, &raw); err != nil {
		return err
	}
	if c.json {
		e.printJSON(raw)
		return nil
	}
	var r visits.Report
	if err := json.Unmarshal(raw, &r); err != nil {
		return err
	}
	fmt.Fprintf(e.Stdout, "%s, %s to %s: %d visits, %d pages read\n", domain, r.From, r.To, r.Visits, r.Pageviews)
	for _, d := range r.Days {
		fmt.Fprintf(e.Stdout, "  %s  %d people, %d pages\n", d.Date, d.Visitors, d.Pageviews)
	}
	for _, part := range []struct {
		title string
		rows  []visits.Row
	}{{"pages", r.Pages}, {"links in", r.Referrers}, {"countries", r.Countries}, {"devices", r.Devices}} {
		if len(part.rows) == 0 {
			continue
		}
		var items []string
		for i, x := range part.rows {
			if i == 10 {
				break
			}
			items = append(items, fmt.Sprintf("%s %d", x.Key, x.Count))
		}
		fmt.Fprintf(e.Stdout, "%s: %s\n", part.title, strings.Join(items, ", "))
	}
	return nil
}
