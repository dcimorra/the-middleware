package cli

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"sort"
	"strconv"

	"github.com/dcimorra/the-middleware/internal/chat"
	"github.com/dcimorra/the-middleware/internal/sites"
)

// ownerOf is the site a name belongs to, and the service that answers it
// when the site has services: `site show onion-nest-bakery-next.ddev.es`
// is the bakery's next. A name that is a site's folder is that site. A
// site's own conversation can only look among its own names: the house
// tells it nothing of the others.
func (e *Env) ownerOf(cl *Client, name string) (site, service string, err error) {
	var s sites.Site
	err = cl.Do("GET", "/api/v1/sites/"+name, nil, &s)
	var ae *APIError
	if err == nil || !errors.As(err, &ae) || (ae.Status != http.StatusNotFound && ae.Status != http.StatusForbidden) {
		return name, "", err
	}
	first := err
	var candidates []string
	if turn, terr := chat.SiteOfProcess("/proc", os.Getpid(), nil); terr == nil && turn != "" && turn != chat.Autopilot {
		candidates = []string{turn}
	} else {
		var list []sites.Site
		if lerr := cl.Do("GET", "/api/v1/sites", nil, &list); lerr != nil {
			return "", "", first
		}
		for _, x := range list {
			candidates = append(candidates, x.Domain)
		}
	}
	for _, c := range candidates {
		var x sites.Site
		if cl.Do("GET", "/api/v1/sites/"+c, nil, &x) != nil || x.Manifest == nil {
			continue
		}
		if svc, ok := x.Manifest.ServiceFor(name, "/"); ok {
			return c, svc, nil
		}
		for _, h := range x.Manifest.Hosts(c) {
			if h == name {
				return c, "", nil
			}
		}
	}
	return "", "", first
}

func (e *Env) siteRoutes(c common, cl *Client, site string) error {
	var raw []byte
	if err := cl.Do("GET", "/api/v1/sites/"+site+"/routes", nil, &raw); err != nil {
		return err
	}
	if c.json {
		e.printJSON(raw)
		return nil
	}
	var list []sites.RouteState
	if err := json.Unmarshal(raw, &list); err != nil {
		return err
	}
	rows := make([][]string, 0, len(list))
	for _, r := range list {
		svc := r.Service
		if svc == "" {
			svc = "-"
		}
		port := "-"
		if r.Port != 0 {
			port = strconv.Itoa(r.Port)
		}
		rows = append(rows, []string{r.Host + r.Prefix, svc, port, r.State, oneLine(r.Error)})
	}
	Table(e.Stdout, []string{"HOST", "SERVICE", "PORT", "STATE", "ERROR"}, rows)
	return nil
}

func serviceState(alive, starting bool, stopped, waiting, errText string) string {
	switch {
	case stopped != "":
		return stopped
	case starting:
		if alive {
			return "up, new generation starting"
		}
		return "starting"
	case waiting != "":
		return "waiting for " + waiting
	case alive:
		return "up"
	case errText != "":
		return "down"
	}
	return "starting"
}

func sortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
