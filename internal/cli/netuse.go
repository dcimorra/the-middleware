package cli

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"

	"github.com/dcimorra/the-middleware/internal/netuse"
)

// siteNetwork is `middleware site network [<domain>]`: what a site moves
// over the network, or with no domain what every site does, the heaviest
// of the month first.
func (e *Env) siteNetwork(c common, cl *Client, domain string, days int) error {
	path := "/api/v1/network"
	if domain != "" {
		path = "/api/v1/sites/" + url.PathEscape(domain) + "/network?days=" + strconv.Itoa(max(1, days))
	}
	var raw []byte
	if err := cl.Do("GET", path, nil, &raw); err != nil {
		return err
	}
	if c.json {
		e.printJSON(raw)
		return nil
	}
	if domain == "" {
		var all struct {
			Sites []netuse.Row `json:"sites"`
		}
		if err := json.Unmarshal(raw, &all); err != nil {
			return err
		}
		rows := [][]string{}
		for _, r := range all.Sites {
			rows = append(rows, []string{r.Domain, netRate(r.RateIn), netRate(r.RateOut), netSize(r.Today.In + r.Today.Out), netSize(r.Month.In), netSize(r.Month.Out), netBudget(r.Limits), r.State})
		}
		Table(e.Stdout, []string{"SITE", "IN NOW", "OUT NOW", "TODAY", "MONTH IN", "MONTH OUT", "BUDGET", "STATE"}, rows)
		return nil
	}
	var r netuse.Report
	if err := json.Unmarshal(raw, &r); err != nil {
		return err
	}
	fmt.Fprintf(e.Stdout, "%s: %s in, %s out now; state %s\n", domain, netRate(r.RateIn), netRate(r.RateOut), r.State)
	for _, part := range []struct {
		name string
		t    netuse.Totals
	}{{"today", r.Today}, {"this month", r.Month}} {
		fmt.Fprintf(e.Stdout, "%s: %s in, %s out, %s in all", part.name, netSize(part.t.In), netSize(part.t.Out), netSize(part.t.In+part.t.Out))
		if part.t.PortsIn+part.t.PortsOut > 0 {
			fmt.Fprintf(e.Stdout, " (%s of it by its public ports)", netSize(part.t.PortsIn+part.t.PortsOut))
		}
		fmt.Fprintln(e.Stdout)
	}
	if r.Limits != nil {
		fmt.Fprintf(e.Stdout, "budget: %s, warns at %d%%\n", netBudget(r.Limits), r.Limits.WarnAt)
	}
	for _, d := range r.Days {
		fmt.Fprintf(e.Stdout, "  %s  %s in, %s out\n", d.Date, netSize(d.In), netSize(d.Out))
	}
	return nil
}

func netBudget(b *netuse.Budget) string {
	if b == nil {
		return "-"
	}
	out := ""
	if b.Monthly > 0 {
		out = netSize(b.Monthly) + " a month"
	}
	if b.Daily > 0 {
		if out != "" {
			out += ", "
		}
		out += netSize(b.Daily) + " a day"
	}
	if b.Cut {
		return out + ", then closed"
	}
	return out + ", then a warning"
}

// netSize writes bytes in the units the manifest's sizes and the panel
// use (1 GB is 1024 MB).
func netSize(n int64) string {
	f, u := float64(n), 0
	units := []string{"B", "KB", "MB", "GB", "TB"}
	for f >= 1024 && u < len(units)-1 {
		f /= 1024
		u++
	}
	if u == 0 {
		return strconv.FormatInt(n, 10) + " B"
	}
	return strconv.FormatFloat(f, 'f', 1, 64) + " " + units[u]
}

func netRate(perSecond float64) string { return netSize(int64(perSecond)) + "/s" }
