package cli

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const siteStagingUsage = `usage: middleware site staging <domain> [command]

  status      the site's test copies, or the production it is a copy of,
              and what a promotion keeps (the default)
  create      make a test copy of <domain> (--name, default
              staging.<domain>): files and databases as they are now,
              behind a password printed once
  promote     on a test copy: put its code into its production; the
              production's manifest and data stay
  undo        on production: put back what the last promotion replaced
  refresh     on a test copy: copy production into it again, data too
`

// siteStaging is `middleware site staging`: the panel's test copies.
func (e *Env) siteStaging(c common, cl *Client, domain string, args []string, name, publish, door string) error {
	verb := "status"
	if len(args) > 0 {
		verb = args[0]
	}
	if len(args) > 1 {
		fmt.Fprint(e.Stderr, siteStagingUsage)
		return fmt.Errorf("site staging %s: takes no arguments", verb)
	}
	base := "/api/v1/sites/" + url.PathEscape(domain) + "/staging"
	var raw json.RawMessage
	var err error
	switch verb {
	case "status":
		err = cl.Do("GET", base, nil, &raw)
	case "create":
		body := map[string]any{}
		if name != "" {
			body["name"] = name
		}
		switch strings.ToLower(publish) {
		case "true", "1", "yes":
			body["publish"] = true
		case "false", "0", "no":
			body["publish"] = false
		}
		if door != "" {
			body["door"] = door
		}
		err = cl.Do("POST", base, body, &raw)
	case "promote", "undo", "refresh":
		err = cl.Do("POST", base+"/"+verb, nil, &raw)
	default:
		fmt.Fprint(e.Stderr, siteStagingUsage)
		return fmt.Errorf("site staging: unknown command %q", verb)
	}
	if err != nil {
		return err
	}
	if c.json {
		e.printJSON(raw)
		return nil
	}
	var out struct {
		Site struct {
			Domain string `json:"domain"`
		} `json:"site"`
		Password string    `json:"password"`
		Notes    []string  `json:"notes"`
		Of       string    `json:"of"`
		Copies   []string  `json:"copies"`
		Keep     []string  `json:"keep"`
		Before   time.Time `json:"before"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return err
	}
	switch verb {
	case "status":
		if out.Of != "" {
			fmt.Fprintf(e.Stdout, "%s is a test copy of %s\n", domain, out.Of)
		} else if len(out.Copies) == 0 {
			fmt.Fprintf(e.Stdout, "%s has no test copy\n", domain)
		} else {
			fmt.Fprintf(e.Stdout, "test copies of %s: %s\n", domain, strings.Join(out.Copies, ", "))
		}
		fmt.Fprintf(e.Stdout, "a promotion keeps: %s\n", strings.Join(out.Keep, " "))
		if !out.Before.IsZero() {
			fmt.Fprintf(e.Stdout, "the last promotion can be undone (folder set aside %s)\n", out.Before.Local().Format("2006-01-02 15:04"))
		}
	case "create":
		fmt.Fprintf(e.Stdout, "%s is a test copy of %s\n", out.Site.Domain, domain)
		if out.Password != "" {
			fmt.Fprintf(e.Stdout, "password (shown once): %s\n", out.Password)
		}
	case "promote":
		fmt.Fprintf(e.Stdout, "promoted to %s; `middleware site staging %s undo` puts it back\n", out.Site.Domain, out.Site.Domain)
	case "undo":
		fmt.Fprintf(e.Stdout, "%s is back as it was before the last promotion\n", domain)
	case "refresh":
		fmt.Fprintf(e.Stdout, "%s is a fresh copy of production again\n", domain)
	}
	for _, n := range out.Notes {
		fmt.Fprintf(e.Stdout, "  note: %s\n", n)
	}
	return nil
}
