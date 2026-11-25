package cli

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/browsers"
)

const siteBrowsersUsage = `usage: middleware site browsers <domain> [command]

The browsers this node keeps for its sites: headless Chromiums that a
site holds one at a time and drives over the Chrome DevTools Protocol.

  (none)                 which are free, who holds the rest, and yours
  take [--for <what>]    take a free one, saying what for: prints its address.
                         Taking again renews the one you hold.
  release                give yours back: its browser and its profile end

Say what for, and give it back as soon as you are done: other sites wait
for it. One nothing is sent to for a while is taken back by the house.
`

// siteBrowsers is `middleware site browsers`: the pool as one site sees
// it, through the API so the events say who took and who gave back.
func (e *Env) siteBrowsers(c common, cl *Client, domain string, args []string, what string) error {
	base := "/api/v1/sites/" + url.PathEscape(domain) + "/browsers"
	verb := "list"
	if len(args) > 0 {
		verb, args = args[0], args[1:]
	}
	if len(args) > 0 && verb == "take" && what == "" {
		// `take checking the form` reads as well as --for.
		what, args = strings.Join(args, " "), nil
	}
	if len(args) > 0 {
		fmt.Fprint(e.Stderr, siteBrowsersUsage)
		return fmt.Errorf("site browsers %s: unexpected %q", verb, strings.Join(args, " "))
	}
	var raw []byte
	switch verb {
	case "list", "ls":
		if err := cl.Do("GET", base, nil, &raw); err != nil {
			return err
		}
		if c.json {
			e.printJSON(raw)
			return nil
		}
		return e.printBrowsers(domain, raw)
	case "take":
		if err := cl.Do("POST", base, map[string]any{"for": what}, &raw); err != nil {
			return err
		}
		if c.json {
			e.printJSON(raw)
			return nil
		}
		var out struct {
			Browser browsers.Slot `json:"browser"`
			Again   bool          `json:"again"`
			Idle    int           `json:"idle_minutes"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return err
		}
		b := out.Browser
		if out.Again {
			fmt.Fprintf(e.Stdout, "%s already holds browser %d: it is renewed.\n", domain, b.Slot)
		} else {
			fmt.Fprintf(e.Stdout, "Browser %d is %s's now.\n", b.Slot, domain)
		}
		fmt.Fprintf(e.Stdout, "\nIts DevTools address (yours alone, do not write it into the site):\n\n  %s\n\n", b.Endpoint)
		fmt.Fprintf(e.Stdout, "  Playwright: chromium.connectOverCDP(%q)\n", b.Endpoint)
		fmt.Fprintf(e.Stdout, "  Puppeteer:  puppeteer.connect({browserWSEndpoint: %q})\n", b.Endpoint)
		fmt.Fprintf(e.Stdout, "  Node 22+:   new WebSocket(%q), then the protocol's JSON\n\n", b.Endpoint)
		fmt.Fprintf(e.Stdout, "Disconnecting keeps the browser and its pages for your next script. Give it back when you are done:\n\n  middleware site browsers %s release\n\nNothing sent to it for %d minutes, the house takes it back.\n", domain, out.Idle)
		return nil
	case "release", "give", "drop":
		if err := cl.Do("DELETE", base, nil, &raw); err != nil {
			return err
		}
		if c.json {
			e.printJSON(raw)
			return nil
		}
		var out struct {
			Slot int `json:"slot"`
		}
		_ = json.Unmarshal(raw, &out)
		fmt.Fprintf(e.Stdout, "%s gave browser %d back.\n", domain, out.Slot)
		return nil
	case "help":
		fmt.Fprint(e.Stdout, siteBrowsersUsage)
		return nil
	}
	fmt.Fprint(e.Stderr, siteBrowsersUsage)
	return fmt.Errorf("site browsers: unknown command %q", verb)
}

func (e *Env) printBrowsers(domain string, raw []byte) error {
	var v struct {
		Count     int             `json:"count"`
		Free      int             `json:"free"`
		Available bool            `json:"available"`
		Idle      int             `json:"idle_minutes"`
		Slots     []browsers.Slot `json:"slots"`
		Mine      *browsers.Slot  `json:"mine"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	if v.Count == 0 && len(v.Slots) == 0 {
		fmt.Fprintln(e.Stdout, "This node keeps no browsers for its sites: the operator sets how many in Settings, Browsers.")
		return nil
	}
	if !v.Available {
		fmt.Fprintln(e.Stdout, "There is no headless Chromium on this machine yet: no browser can be started until the operator installs one.")
	}
	rows := [][]string{}
	for _, s := range v.Slots {
		state, holder, since, what := "free", "", "", ""
		if !s.Free {
			state, holder, what = "taken", s.Site, s.For
			if s.Site == domain {
				state = "yours"
			}
			if s.Leaving {
				state += " (leaves when given back)"
			}
			if s.Since != nil {
				since = s.Since.Local().Format("15:04") + " (" + time.Since(*s.Since).Round(time.Minute).String() + ")"
			}
		}
		rows = append(rows, []string{strconv.Itoa(s.Slot), state, holder, since, what})
	}
	Table(e.Stdout, []string{"BROWSER", "STATE", "HELD BY", "SINCE", "FOR"}, rows)
	fmt.Fprintf(e.Stdout, "\n%d of %d free.", v.Free, v.Count)
	if v.Mine != nil {
		fmt.Fprintf(e.Stdout, " You hold browser %d: %s\nGive it back with `middleware site browsers %s release`; unused for %d minutes, the house takes it back.\n", v.Mine.Slot, v.Mine.Endpoint, domain, v.Idle)
	} else if v.Free > 0 {
		fmt.Fprintf(e.Stdout, " Take one with `middleware site browsers %s take --for \"what you will look at\"`.\n", domain)
	} else {
		fmt.Fprintln(e.Stdout, " Every one is held: try again when one is given back.")
	}
	return nil
}
