package cli

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/dcimorra/the-middleware/internal/manifest"
)

const siteGuestsUsage = `usage: middleware site guests <domain> [command]

Which other sites of this node <domain> takes in. The site that is
entered decides: nothing a site writes in its own manifest lets it into
another.

  (none)                    who it takes in, and which sites take it in
  add <site> [read|write]   take <site> in (default read); again to change it
  rm <site>                 stop taking <site> in

read:  <domain>'s folder, read-only, in every jail of the guest (its
       conversation, its process, its tasks), and for its conversation
       how <domain> is, its logs, its events and the copies of its database.
write: the folder writable, and every command <domain>'s own conversation
       has on it (restart, deploy, exec, tasks, backups).

A conversation sees a change at its next turn; a process or a container
at its next start.
`

// siteGuests is `middleware site guests`: the manifest's guests of one
// site, through the API so the event says who changed it.
func (e *Env) siteGuests(c common, cl *Client, domain string, args []string) error {
	base := "/api/v1/sites/" + url.PathEscape(domain) + "/guests"
	verb := "list"
	if len(args) > 0 {
		verb, args = args[0], args[1:]
	}
	var raw []byte
	switch verb {
	case "list", "ls":
		if err := cl.Do("GET", base, nil, &raw); err != nil {
			return err
		}
	case "add", "set":
		level := manifest.GuestRead
		if len(args) == 2 {
			level = args[1]
		}
		if len(args) < 1 || len(args) > 2 || (level != manifest.GuestRead && level != manifest.GuestWrite) {
			fmt.Fprint(e.Stderr, siteGuestsUsage)
			return fmt.Errorf("site guests add: a site and, after it, read or write")
		}
		if err := cl.Do("PUT", base+"/"+url.PathEscape(strings.ToLower(args[0])), map[string]any{"level": level}, &raw); err != nil {
			return err
		}
		if !c.json {
			fmt.Fprintf(e.Stdout, "%s now takes %s in, to %s.\n\n", domain, strings.ToLower(args[0]), level)
		}
	case "rm", "remove", "delete":
		if len(args) != 1 {
			fmt.Fprint(e.Stderr, siteGuestsUsage)
			return fmt.Errorf("site guests rm: a site is required")
		}
		if err := cl.Do("DELETE", base+"/"+url.PathEscape(strings.ToLower(args[0])), nil, &raw); err != nil {
			return err
		}
		if !c.json {
			fmt.Fprintf(e.Stdout, "%s no longer takes %s in.\n\n", domain, strings.ToLower(args[0]))
		}
	case "help":
		fmt.Fprint(e.Stdout, siteGuestsUsage)
		return nil
	default:
		fmt.Fprint(e.Stderr, siteGuestsUsage)
		return fmt.Errorf("site guests: unknown command %q", verb)
	}
	if c.json {
		e.printJSON(raw)
		return nil
	}
	var out struct {
		Guests []struct {
			Site  string `json:"site"`
			Level string `json:"level"`
			Here  bool   `json:"here"`
		} `json:"guests"`
		Hosts []struct {
			Site  string `json:"site"`
			Write bool   `json:"write"`
		} `json:"hosts"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return err
	}
	if len(out.Guests) == 0 {
		fmt.Fprintf(e.Stdout, "%s takes no other site in.\n", domain)
	} else {
		rows := [][]string{}
		for _, g := range out.Guests {
			state := "a site of this node"
			if !g.Here {
				state = "no site of this node has this name: it lends nothing until one does"
			}
			rows = append(rows, []string{g.Site, g.Level, state})
		}
		Table(e.Stdout, []string{"GUEST", "MAY", ""}, rows)
	}
	if len(out.Hosts) > 0 {
		fmt.Fprintf(e.Stdout, "\n%s is taken in by:\n", domain)
		rows := [][]string{}
		for _, h := range out.Hosts {
			level := manifest.GuestRead
			if h.Write {
				level = manifest.GuestWrite
			}
			rows = append(rows, []string{h.Site, level})
		}
		Table(e.Stdout, []string{"SITE", "MAY"}, rows)
	}
	return nil
}
