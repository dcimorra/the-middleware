package cli

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/dcimorra/the-middleware/internal/security"
	"github.com/dcimorra/the-middleware/internal/seen"
)

const siteSecurityUsage = `usage: middleware site security <domain> [command]

  seen        who calls at the site: countries, robots, browsers and the
              User-Agents no table knows, heaviest first (the default)
  blocks      the last 50 requests the site's security rules refused
  catalog     the robot families, robots, browsers and probe paths the
              rules may name

The rules themselves are the manifest's "security" (docs/manifest.md):
write them in site.json and they apply live, with no restart.
`

// siteSecurity is `middleware site security`: what the panel's Security
// tab reads, for a site's agent turn. A site's agent asked to keep out a
// crawler wrote the User-Agent check into the app because nothing told
// it the house already had one; with the tally and the refusals it can
// write the rule from what really calls and see what the rule did.
func (e *Env) siteSecurity(c common, cl *Client, domain string, args []string) error {
	verb := "seen"
	if len(args) > 0 {
		verb = args[0]
	}
	if len(args) > 1 {
		fmt.Fprint(e.Stderr, siteSecurityUsage)
		return fmt.Errorf("site security %s: takes no arguments", verb)
	}
	base := "/api/v1/sites/" + url.PathEscape(domain) + "/security"
	switch verb {
	case "seen":
		var raw []byte
		if err := cl.Do("GET", base+"/seen", nil, &raw); err != nil {
			return err
		}
		if c.json {
			e.printJSON(raw)
			return nil
		}
		var tb seen.Table
		if err := json.Unmarshal(raw, &tb); err != nil {
			return err
		}
		if tb.Total == 0 {
			fmt.Fprintf(e.Stdout, "nobody counted at %s yet\n", domain)
			return nil
		}
		fmt.Fprintf(e.Stdout, "%d calls since %s\n", tb.Total, tb.Since.Local().Format("2006-01-02 15:04"))
		for _, s := range []struct {
			title string
			rows  []seen.Entry
		}{{"countries", tb.Countries}, {"robots", tb.Bots}, {"browsers", tb.Browsers}, {"unknown User-Agents", tb.Agents}} {
			if len(s.rows) == 0 {
				continue
			}
			fmt.Fprintf(e.Stdout, "\n%s\n", s.title)
			for i, r := range s.rows {
				if i == 15 {
					fmt.Fprintf(e.Stdout, "  … %d more (--json for all)\n", len(s.rows)-15)
					break
				}
				name := r.Name
				if name == "" {
					name = r.Key
				}
				if r.Extra != "" {
					name += " (" + r.Extra + ")"
				}
				fmt.Fprintf(e.Stdout, "  %8d  %s\n", r.Count, name)
			}
		}
		return nil
	case "blocks":
		var raw []byte
		if err := cl.Do("GET", base+"/blocks", nil, &raw); err != nil {
			return err
		}
		if c.json {
			e.printJSON(raw)
			return nil
		}
		var list []security.Block
		if err := json.Unmarshal(raw, &list); err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Fprintf(e.Stdout, "nothing refused at %s since the house started\n", domain)
			return nil
		}
		for _, b := range list {
			fmt.Fprintf(e.Stdout, "%s  %-7s %-24s %s", b.At.Local().Format("01-02 15:04:05"), b.Reason, b.What, b.Path)
			if b.Country != "" {
				fmt.Fprintf(e.Stdout, "  [%s]", b.Country)
			}
			if b.Agent != "" {
				fmt.Fprintf(e.Stdout, "  %q", b.Agent)
			}
			fmt.Fprintln(e.Stdout)
		}
		return nil
	case "catalog":
		// The catalogue is compiled in, the same one the manifest is
		// checked against: nothing to ask the supervisor for.
		cat := map[string]any{
			"families":      security.Families(),
			"robots":        security.Robots(),
			"browsers":      security.Browsers(),
			"builtin_paths": security.BuiltinPaths(),
		}
		if c.json {
			data, _ := json.Marshal(cat)
			e.printJSON(data)
			return nil
		}
		fmt.Fprintln(e.Stdout, "robot families (security.bots.deny):")
		robots := security.Robots()
		for _, f := range security.Families() {
			var names []string
			for _, r := range robots {
				if r.Family == f.Key {
					names = append(names, r.Name)
				}
			}
			fmt.Fprintf(e.Stdout, "  %-9s %s: %s\n", f.Key, f.Name, f.Note)
			if len(names) > 0 {
				fmt.Fprintf(e.Stdout, "            %s\n", strings.Join(names, ", "))
			}
		}
		fmt.Fprintln(e.Stdout, "\nbrowsers (security.browsers):")
		for _, b := range security.Browsers() {
			line := "  " + b.Key
			if b.Note != "" {
				line += ": " + b.Note
			}
			fmt.Fprintln(e.Stdout, line)
		}
		fmt.Fprintln(e.Stdout, "\npaths every site answers 404 to, before the site sees them:")
		fmt.Fprintln(e.Stdout, "  "+strings.Join(security.BuiltinPaths(), ", "))
		return nil
	case "help":
		fmt.Fprint(e.Stdout, siteSecurityUsage)
		return nil
	default:
		fmt.Fprint(e.Stderr, siteSecurityUsage)
		return fmt.Errorf("site security: unknown command %q", verb)
	}
}
