package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/dcimorra/the-middleware/internal/access"
	"github.com/dcimorra/the-middleware/internal/agent"
	"github.com/dcimorra/the-middleware/internal/app"
	"github.com/dcimorra/the-middleware/internal/chat"
	"github.com/dcimorra/the-middleware/internal/edge"
	"github.com/dcimorra/the-middleware/internal/engine"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/guard"
	"github.com/dcimorra/the-middleware/internal/helper"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/proxy"
	"github.com/dcimorra/the-middleware/internal/security"
	"github.com/dcimorra/the-middleware/internal/settings"
	"github.com/dcimorra/the-middleware/internal/traffic"
	"github.com/dcimorra/the-middleware/internal/waf"
)

// The request chain (wire.go has the order) is inbound → waf → security →
// access → traffic → proxy: the WAF refuses the abusive before anything
// else spends on them, security cuts probe paths and what the site does
// not want by what it claims to be, access closes the door the operator
// closed, and traffic counts
// whatever came out, error pages included. The guard wraps nothing; it
// only runs. These are the instances the API routes talk to (block,
// policy, incidents), set when the app starts.
var (
	theWAF      *waf.WAF
	theSecurity *security.Door
	theTraffic  *traffic.Recorder
	theGuard    *guard.Guard
)

func init() {
	hookBuilders = append(hookBuilders,
		hookBuilder{Name: "waf", Order: orderWAF, Build: buildWAF},
		hookBuilder{Name: "security", Order: orderSecurity, Build: buildSecurity},
		hookBuilder{Name: "access", Order: orderAccess, Build: buildAccess},
		hookBuilder{Name: "traffic", Order: orderTraffic, Build: buildTraffic},
		hookBuilder{Name: "guard", Order: orderGuard, Build: buildGuard},
	)
	subcommands["waf"] = wafCommand
	subcommands["guard"] = guardCommand
	extraUsage += `  waf block <ip> [duration] [reason]   block an address (default one day)
  waf unblock <ip>                     remove an address from the block list
  waf list                             show the block list
  guard incidents                      show what the guard found
`
}

// hook adapts a package with Wrap and Run(ctx) to app.Hook.
type hook struct {
	wrap func(http.Handler) http.Handler
	run  func(ctx context.Context)
}

func (h hook) Wrap(next http.Handler) http.Handler {
	if h.wrap == nil {
		return next
	}
	return h.wrap(next)
}

func (h hook) Run(ctx context.Context, _ *app.App) error {
	if h.run != nil {
		h.run(ctx)
	}
	return nil
}

// edgeCountry is the country the edge says, which is all the house knows
// without a GeoIP database. It was never wired before, so every
// deny_countries rule in the WAF and in manifests was dead configuration.
func edgeCountry(r *http.Request, _ net.IP) string { return edge.Country(r) }

func dataDir(a *app.App) string { return filepath.Join(a.Root, "data") }

func buildWAF(a *app.App) app.Hook {
	w, err := waf.New(waf.Options{
		DataDir: dataDir(a),
		Bus:     a.Bus,
		Country: edgeCountry,
		Lookup:  func(host string) (waf.Site, bool) { return wafSite(a.Engine.Lookup(host)) },
		LookupRequest: func(r *http.Request) (waf.Site, bool) {
			return wafSite(a.Engine.LookupPath(proxy.Host(r), r.URL.Path))
		},
	})
	var pe *waf.PolicyError
	switch {
	case errors.As(err, &pe):
		// A typo in policy.json must not take every site down with it:
		// the WAF runs on defaults and the event says what to fix.
		a.Bus.Emit(events.Event{Type: "waf.error", Actor: "engine", Level: "error", Message: "running with the default policy: " + err.Error()})
	case err != nil:
		a.Bus.Emit(events.Event{Type: "waf.error", Actor: "engine", Level: "error", Message: "waf not started: " + err.Error()})
		return nil
	}
	theWAF = w
	return hook{wrap: w.Wrap, run: w.Run}
}

// wafSite is the policy a request for a site is judged under: for a
// site with services, the waf of the service that answers it, with a
// rate bucket of its own when it has one.
func wafSite(sn engine.Snapshot, ok bool) (waf.Site, bool) {
	if !ok {
		return waf.Site{}, false
	}
	s := waf.Site{Domain: sn.Domain, Dir: sn.Dir}
	if sn.Manifest != nil {
		s.WAF = sn.Manifest.WAF
		if sv := sn.Manifest.Services[sn.Service]; sv != nil && sv.WAF != nil {
			s.Bucket = sn.Domain + "#" + sn.Service
		}
	}
	return s, true
}

// serviceOf is the service of a site with services a snapshot is for, or
// nil.
func serviceOf(sn engine.Snapshot) *manifest.Service {
	if sn.Manifest == nil || sn.Service == "" {
		return nil
	}
	return sn.Manifest.Services[sn.Service]
}

func buildSecurity(a *app.App) app.Hook {
	d := security.New(security.Options{
		Host:     proxy.Host,
		ClientIP: edge.ClientIP,
		Country:  edgeCountry,
		Lookup:   func(host string) (security.Site, bool) { return securitySite(a.Engine.Lookup(host)) },
		LookupRequest: func(r *http.Request) (security.Site, bool) {
			return securitySite(a.Engine.LookupPath(proxy.Host(r), r.URL.Path))
		},
	})
	theSecurity = d
	return hook{wrap: d.Wrap}
}

// securitySite is the site's rules, and for a site with services the
// rules of the service that answers the request, judged after them.
func securitySite(sn engine.Snapshot, ok bool) (security.Site, bool) {
	if !ok || sn.Manifest == nil {
		return security.Site{}, false
	}
	site := security.Site{Domain: sn.Domain, Rules: sn.Manifest.Security, Title: sn.Manifest.Title}
	if sv := serviceOf(sn); sv != nil && sv.Security != nil {
		site.More, site.Service = sv.Security, sn.Service
	}
	if sn.Manifest.Notice != nil {
		site.Theme = sn.Manifest.Notice.Theme
	}
	return site, true
}

// accessSite is the site's access, and for a site with services the
// access of the service that answers the request, judged after it.
func accessSite(sn engine.Snapshot, ok bool) (access.Site, bool) {
	if !ok || sn.Manifest == nil {
		return access.Site{}, false
	}
	site := access.Site{Domain: sn.Domain, Access: sn.Manifest.Access, Title: sn.Manifest.Title}
	if sv := serviceOf(sn); sv != nil {
		site.More = sv.Access
	}
	if sn.Manifest.Notice != nil {
		site.Theme = sn.Manifest.Notice.Theme
	}
	return site, true
}

func buildAccess(a *app.App) app.Hook {
	lookup := func(host string) (access.Site, bool) { return accessSite(a.Engine.Lookup(host)) }
	byRequest := func(r *http.Request, urlPath string) (access.Site, bool) {
		return accessSite(a.Engine.LookupPath(proxy.Host(r), urlPath))
	}
	ac, err := access.New(access.Options{
		DataDir:       dataDir(a),
		ClientIP:      edge.ClientIP,
		Country:       edgeCountry,
		Lookup:        lookup,
		LookupRequest: byRequest,
	})
	if err != nil {
		a.Bus.Emit(events.Event{Type: "access.error", Actor: "engine", Level: "error", Message: "session key not available, protected sites are closed: " + err.Error()})
		return hook{wrap: access.ClosedRequest(byRequest)}
	}
	return hook{wrap: ac.Wrap}
}

func buildTraffic(a *app.App) app.Hook {
	r := traffic.New(traffic.Options{
		LogDir:   filepath.Join(dataDir(a), "logs"),
		ClientIP: edge.ClientIP,
		Host:     proxy.Host,
		// A site's own name, which the engine resolves; the rest share one
		// log, so a client making up names makes no folders.
		Site: func(r *http.Request) (string, bool) {
			return siteOf(a, proxy.Host(r))
		},
		Service: func(r *http.Request) string {
			sn, _ := a.Engine.LookupPath(proxy.Host(r), r.URL.Path)
			return sn.Service
		},
	})
	theTraffic = r
	return hook{wrap: r.Wrap, run: r.Run}
}

// siteOf is the domain of the site that answers host, as the proxy
// decides it: a project has no hostname, so a Host that spells its
// folder's name is no site.
func siteOf(a *app.App, host string) (string, bool) {
	sn, ok := a.Engine.Lookup(host)
	if !ok || sn.Mode == manifest.ModeProject || manifest.IsProject(sn.Domain) {
		return "", false
	}
	return sn.Domain, true
}

// guardData is data/, for guardSite; set when the guard is built.
var guardData string

func buildGuard(a *app.App) app.Hook {
	guardData = dataDir(a)
	g := guard.New(guard.Options{
		ProcRoot:  "/proc",
		DataDir:   dataDir(a),
		SitesDir:  filepath.Join(a.Root, "sites"),
		PortRange: [2]int{a.Settings.Ports.From, a.Settings.Ports.To},
		Bus:       a.Bus,
		Contained: investigate(a),
		Agents: func() []int {
			if chatManager == nil {
				return nil
			}
			return chatManager.AgentPIDs()
		},
		Sites: func() []guard.SiteInfo {
			snaps := a.Engine.Sites()
			out := make([]guard.SiteInfo, 0, len(snaps))
			for _, sn := range snaps {
				out = append(out, guardSite(sn))
			}
			return out
		},
	})
	theGuard = g
	return hook{run: g.Run}
}

// investigate asks the house's conversation to find out how an intrusion
// got in, once the guard has cut it. What the guard saw was written by
// whoever broke in, so it goes between markers with a sentence saying it
// is evidence and not instructions: a model that reads the intruder's
// words and may run commands is a model the intruder can talk to. The
// house's conversation, not the site's: it reads the logs and incidents
// under data/, and the site's own agent is the one inside the walls that
// were just climbed.
func investigate(a *app.App) func(guard.Incident) {
	return func(inc guard.Incident) {
		if chatManager == nil {
			return
		}
		// A fixed marker could be closed by the intruder's own command
		// line, in the one conversation that is the operator's: the block
		// gets a marker drawn for this prompt that nothing in it can hold.
		prompt := fmt.Sprintf(`The guard found an intrusion on %s and has already acted: %s. Rule %q, at %s. Find out how it got in, what it reached, and what to change so it cannot happen again; do not start the site again, the operator decides that. The incident is data/guard/incidents/%s.json and the site's logs are under data/logs/%s/.

%s`, inc.Domain, inc.Action, inc.Rule, inc.At.Format(time.RFC3339), inc.ID, inc.Domain, agent.Untrusted("The guard's observations, written by whoever broke in,", inc.Where+"\n"+inc.Cmdline))
		if _, err := chatManager.SendWith(chat.Middleware, prompt, chat.SendOptions{Queue: true}); err != nil && !errors.Is(err, chat.ErrQueued) {
			a.Bus.Emit(events.Event{Type: "guard.error", Subject: inc.Domain, Actor: "guard", Level: "warn", Message: "the agent was not asked to investigate: " + err.Error()})
		}
	}
}

func guardSite(sn engine.Snapshot) guard.SiteInfo {
	s := guard.SiteInfo{Domain: sn.Domain, Dir: sn.Dir, Home: filepath.Join(guardData, "homes", sn.Domain)}
	logs, _ := filepath.Glob(filepath.Join(guardData, "logs", sn.Domain, "*.log"))
	for _, l := range logs {
		// access.log is the house's record of requests, not something the
		// site wrote: every line in it is a client's words.
		if filepath.Base(l) != "access.log" {
			s.Logs = append(s.Logs, l)
		}
	}
	if sn.Port > 0 {
		s.Ports = append(s.Ports, sn.Port)
	}
	for _, w := range sn.Workers {
		if w.Port > 0 {
			s.Ports = append(s.Ports, w.Port)
		}
	}
	return s
}

// --- CLI ---

// cliRoot parses the shared -root flag, wherever it sits (`waf list
// --root /x` reads as naturally as the flag first), and returns the
// remaining args. The first smoke run of the binary had every `--root`
// after the verb silently ignored and the commands answering about
// /srv/middleware.
func cliRoot(name string, args []string) (string, []string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	root := fs.String("root", "", "installation root (default $MIDDLEWARE_ROOT or /srv/middleware)")
	rest, err := parseInterleaved(fs, args)
	if err != nil {
		return "", nil, err
	}
	return settings.Root(*root), rest, nil
}

// openWAF opens the block list from disk. The running supervisor notices
// the file change within seconds, so the CLI needs no socket to the API.
func openWAF(root string) (*waf.WAF, error) {
	return waf.New(waf.Options{DataDir: filepath.Join(root, "data"), Bus: events.New(filepath.Join(root, "data", "events"))})
}

func wafCommand(args []string) error {
	root, rest, err := cliRoot("waf", args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return errors.New("usage: middleware waf block|unblock|list")
	}
	w, err := openWAF(root)
	if err != nil {
		return err
	}
	switch rest[0] {
	case "block":
		if len(rest) < 2 {
			return errors.New("usage: middleware waf block <ip> [duration] [reason]")
		}
		ip := net.ParseIP(rest[1])
		if ip == nil {
			return fmt.Errorf("%q is not an address", rest[1])
		}
		dur := 24 * time.Hour
		reason := "blocked from the command line"
		if len(rest) > 2 {
			d, err := time.ParseDuration(rest[2])
			if err != nil {
				return fmt.Errorf("duration %q: %w", rest[2], err)
			}
			dur = d
		}
		if len(rest) > 3 {
			reason = strings.Join(rest[3:], " ")
		}
		if err := w.Block(ip, dur, reason); err != nil {
			return err
		}
		fmt.Printf("blocked %s for %s\n", ip, dur)
		return nil
	case "unblock":
		if len(rest) < 2 {
			return errors.New("usage: middleware waf unblock <ip>")
		}
		ip := net.ParseIP(rest[1])
		if ip == nil {
			return fmt.Errorf("%q is not an address", rest[1])
		}
		if err := w.Unblock(ip); err != nil {
			return err
		}
		// The kernel's copy too: the service only rereads the file, and
		// this is the way out of a ban for whoever is behind it.
		hctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := helper.Dial("").FirewallUnblock(hctx, ip.String()); err != nil && !errors.Is(err, helper.ErrUnavailable) {
			fmt.Printf("the kernel's block could not be lifted: %v\n", err)
		}
		cancel()
		fmt.Printf("unblocked %s\n", ip)
		return nil
	case "list":
		entries := w.Blocked()
		if len(entries) == 0 {
			fmt.Println("nothing blocked")
			return nil
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 8, 2, ' ', 0)
		fmt.Fprintln(tw, "ADDRESS\tUNTIL\tSTRIKES\tREASON")
		for _, e := range entries {
			fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", e.IP, e.Until.Local().Format(time.RFC3339), e.Strikes, e.Reason)
		}
		return tw.Flush()
	default:
		return fmt.Errorf("unknown waf command %q", rest[0])
	}
}

func guardCommand(args []string) error {
	root, rest, err := cliRoot("guard", args)
	if err != nil {
		return err
	}
	if len(rest) == 0 || rest[0] != "incidents" {
		return errors.New("usage: middleware guard incidents")
	}
	incidents, err := guard.ReadIncidents(filepath.Join(root, "data"))
	if err != nil {
		return err
	}
	if len(incidents) == 0 {
		fmt.Println("no incidents")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "AT\tDOMAIN\tLEVEL\tRULE\tWHERE\tACTION\tID")
	for _, inc := range incidents {
		if inc.Dismissed {
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", inc.At.Local().Format(time.RFC3339), inc.Domain, inc.Level, inc.Rule, inc.Where, inc.Action, inc.ID)
	}
	return tw.Flush()
}
