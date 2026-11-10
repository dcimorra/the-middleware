package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dcimorra/the-middleware/internal/auth"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/inbound"
	"github.com/dcimorra/the-middleware/internal/settings"
	"github.com/dcimorra/the-middleware/internal/sites"
)

// Usage is what `middleware help` adds for these commands.
const Usage = `  setup        print a one-time code to create the operator in the panel
               (--domain panel.example.com [--door ports|tunnel]: the panel's name and door)
               (--password: set the password from the terminal instead)
               (--reset: with an operator already set, a code to set a new one)
  site         list | show | add | rm | restart | stop | start | hold | unhold | logs | deploy | tasks | backups | security | visits
  site tasks <domain> [list | save <name> <file|-> | rm <name> | run <name> | stop <name> | log <name>]
               the tasks that run in that site's folder, through the API (what a site's agent may manage)
  site backups <domain> [list | run <name> | restore <name> <copy> | download <name> <copy> [file] | rm <name> <copy> | log <name>]
               the site's database copies, as its manifest's backups describe them
  site security <domain> [seen | blocks | catalog]
               who calls at the site, what its security rules refused, and what the rules may name
  site staging <domain> [status | create [--name n] | promote | undo | refresh]
               test copies: make one with the site's data, promote it back, undo, copy again
  autopilot    status | journal | sites | on | off | stop | wake | move <node> | ask <domain> - | publish <domain>
               the network's autonomous agent: its switch, the kill switch, where it runs, what it did
  crew         list | status | journal | on | off | stop | wake | request | requests | answer | vault | mail
               the operator's own autonomous agents, made in the panel (Agents)
  events       recent events (-f to follow)
  system       version, uptime, load, memory, disk
  token        add | list | rm — API tokens for scripts
  nodes        the network of nodes and its members
  nodes panel open|close
               every node's panel open to the internet again, or only through
               a VPN (the way out when no VPN of the network answers)

  Every command takes --root <dir> and --json (machine-readable output).
  The CLI talks to the running supervisor over data/api.sock; without the
  socket it uses the loopback listener with $MIDDLEWARE_TOKEN.
`

// Env is what a command runs with. Tests replace Connect and the streams.
type Env struct {
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	Connect func(root string) (*Client, error)

	in *bufio.Reader
}

// Command returns the function cmd/middleware registers for name.
func Command(name string) func(args []string) error {
	return func(args []string) error {
		e := &Env{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Connect: Connect}
		return e.Run(name, args)
	}
}

// Run dispatches one top-level command.
func (e *Env) Run(name string, args []string) error {
	switch name {
	case "setup":
		return e.setup(args)
	case "site":
		return e.site(args)
	case "events":
		return e.events(args)
	case "system":
		return e.system(args)
	case "token":
		return e.token(args)
	case "nodes":
		return e.nodes(args)
	case "autopilot":
		return e.autopilot(args)
	case "crew":
		return e.crew(args)
	case "chat":
		return e.chat(args)
	}
	return fmt.Errorf("unknown command %q", name)
}

// common holds the flags every command takes.
type common struct {
	root string
	json bool
}

func (c *common) bind(fs *flag.FlagSet) {
	fs.StringVar(&c.root, "root", "", "installation root (default $MIDDLEWARE_ROOT or /srv/middleware)")
	fs.BoolVar(&c.json, "json", false, "print the API's answer as JSON")
}

// parse accepts flags before and after positional arguments, so both
// `site logs -f example.com` and `site logs example.com -f` work. The
// flag package stops at the first positional; this resumes after it.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
		if len(args) == 0 {
			return pos, nil
		}
	}
}

func (e *Env) client(c common) (*Client, error) {
	return e.Connect(settings.Root(c.root))
}

// printJSON writes raw API bytes as they came, indented by the server.
func (e *Env) printJSON(raw []byte) {
	raw = []byte(strings.TrimRight(string(raw), "\n"))
	fmt.Fprintln(e.Stdout, string(raw))
}

// --- setup ---------------------------------------------------------------

func (e *Env) setup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	var c common
	c.bind(fs)
	direct := fs.Bool("password", false, "set the operator password now, from the terminal")
	reset := fs.Bool("reset", false, "with an operator already set, print a code that sets a new password from the panel")
	domain := fs.String("domain", "", "the panel's domain, so the first sign-in is at https://<domain>")
	door := fs.String("door", "ports", "how the panel's domain is reached: ports (80/443) or tunnel (cloudflare); new sites follow it")
	join := fs.String("join", "", "an invitation code from another node's panel: join its network and be managed from it, with no domain")
	endpoint := fs.String("endpoint", "", "with --join: this machine's public ip:port for the network (default: the one the house works out)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	root := settings.Root(c.root)
	if _, err := os.Stat(root); err != nil {
		return fmt.Errorf("root %s: %w", root, err)
	}
	if *join != "" {
		return e.joinNetwork(root, strings.TrimSpace(*join), strings.TrimSpace(*endpoint))
	}
	store := auth.New(root, events.New(""))
	if *domain != "" {
		if err := e.setPanelDomain(root, *domain, inbound.Door(*door)); err != nil {
			return fmt.Errorf("domain: %w", err)
		}
		if store.HasOperator() && !*direct {
			// The operator exists: the name was all that was asked for,
			// and a new setup code would be one more secret lying around.
			fmt.Fprintf(e.Stdout, "Open https://%s/ and sign in.\n", strings.ToLower(strings.TrimSpace(*domain)))
			return nil
		}
		if !c.json {
			fmt.Fprintln(e.Stdout)
		}
	}
	if *direct {
		pw, err := e.readPassword("New operator password: ")
		if err != nil {
			return err
		}
		again, err := e.readPassword("Again: ")
		if err != nil {
			return err
		}
		if pw != again {
			return errors.New("the passwords do not match")
		}
		if err := store.SetPassword(pw); err != nil {
			return err
		}
		if c.json {
			e.printJSON([]byte(`{"ok": true}`))
		} else {
			fmt.Fprintln(e.Stdout, "Password set. Every session is logged out.")
		}
		return nil
	}
	// A code sets the operator's password: with one already set, it is a
	// reset, and has to be asked for by name. The installer re-run on a
	// working house printed one every time.
	if store.HasOperator() && !*reset {
		fmt.Fprintln(e.Stdout, "The operator is already set. `middleware setup --password` sets a new password here; `middleware setup --reset` prints a code to set it from the panel.")
		return nil
	}
	code, err := store.CreateSetupCode()
	if err != nil {
		return err
	}
	if c.json {
		out, _ := json.Marshal(map[string]string{"code": code})
		e.printJSON(out)
		return nil
	}
	st, _ := settings.Load(root)
	where := "http://" + st.Listen + "/"
	if st.PanelDomain != "" {
		where = "https://" + st.PanelDomain + "/"
	}
	fmt.Fprintf(e.Stdout, "Setup code: %s\n\nOpen %s and enter it with the password you choose.\nThe code works once; run `middleware setup` again for a new one,\nor `middleware setup --password` to set the password here.\n", code, where)
	if st.PanelDomain == "" {
		fmt.Fprintf(e.Stdout, "\nThe panel has no domain yet, so it listens on this machine only. From your\ncomputer: ssh -L %s:%s <this-server>, then open the URL above. Or give it\none now: middleware setup --domain panel.example.com\n", portOf(st.Listen), st.Listen)
	}
	return nil
}

func portOf(listen string) string {
	if _, p, err := net.SplitHostPort(listen); err == nil {
		return p
	}
	return "46100"
}

// readPassword reads a line without echo when stdin is a terminal. The
// echo is switched off with stty, which every Linux has, because reading
// the terminal's mode without x/term is a hundred lines of ioctl for one
// prompt.
func (e *Env) readPassword(prompt string) (string, error) {
	fmt.Fprint(e.Stderr, prompt)
	f, isTerm := e.Stdin.(*os.File)
	if isTerm {
		if st, err := f.Stat(); err != nil || st.Mode()&os.ModeCharDevice == 0 {
			isTerm = false
		}
	}
	if isTerm {
		stty := func(arg string) { cmd := exec.Command("stty", arg); cmd.Stdin = f; _ = cmd.Run() }
		stty("-echo")
		defer func() { stty("echo"); fmt.Fprintln(e.Stderr) }()
	}
	// One buffered reader for the whole run: a fresh one per prompt
	// would swallow the second line while reading the first.
	if e.in == nil {
		e.in = bufio.NewReader(e.Stdin)
	}
	line, err := e.in.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// --- site ----------------------------------------------------------------

const siteUsage = `usage: middleware site <command> [flags]

  list                        every site folder
  show <domain|name> [--service s]  one site, with its manifest and its services
  add <domain> [--template static|node|go|docker|fastapi|django|php|astro|project] [--manifest file.json|-]
              [--publish=false] [--door tunnel|ports]
                              published by default when a DNS module is on
  rm <domain> [--archive]     unpublish, then delete or move to data/archive/
  publish <domain> [--door tunnel|ports]
                              put every name of the site on the internet
  unpublish <domain>          take its tunnel rules and records away
  published                   every name the house published, with its state
  restart <domain> [--service s] | --all    force a handover (of one service only with --service)
  stop | start <domain>       write disabled into the manifest
  hold <domain> [--title t] [--message m]
  unhold <domain>
  logs <domain> [-f] [--lines 200] [--worker name | --service name [--build]]
  routes <domain>             every name of the site: host → service → port → state
  deploy <domain>             run deploy.github.build and write the pointer
  validate <domain> [--file site.json]
                              check a manifest (the file, or the one on disk) without
                              writing or deploying it: schema, paths, names, Dockerfiles
  tasks <domain> [list | save <name> <file|-> | rm <name> | run <name> | stop <name> | log <name>]
                              the site's own scheduled tasks
  backups <domain> [list | run <name> | restore <name> <copy> [--no-safety-copy] |
          download <name> <copy> [file] | rm <name> <copy> | log <name>]
                              the site's database copies (the manifest's backups)
  security <domain> [seen | blocks | catalog]
                              who calls (countries, robots, browsers, User-Agents), the last
                              requests the manifest's security refused, and what it may name
  visits <domain> [--days 7]  people and pages by day, the top pages, links and countries
`

func (e *Env) site(args []string) error {
	if len(args) == 0 || args[0] == "help" {
		fmt.Fprint(e.Stderr, siteUsage)
		return nil
	}
	sub, args := args[0], args[1:]
	fs := flag.NewFlagSet("site "+sub, flag.ContinueOnError)
	var c common
	c.bind(fs)
	var (
		template = fs.String("template", "static", "template for add")
		manifest = fs.String("manifest", "", "manifest file for add (- for stdin)")
		archive  = fs.Bool("archive", false, "move to data/archive/ instead of deleting")
		all      = fs.Bool("all", false, "every running site (restart)")
		title    = fs.String("title", "", "holding page title")
		message  = fs.String("message", "", "holding page message")
		follow   = fs.Bool("f", false, "follow the log")
		lines    = fs.Int("lines", 200, "lines of log")
		worker   = fs.String("worker", "", "a worker's log instead of the site's")
		publish  = fs.String("publish", "", "add: true or false (default: true when a DNS module is on)")
		door     = fs.String("door", "", "add, publish: tunnel or ports (default: the door for new sites)")
		file     = fs.String("file", "", "validate: the manifest to check (- for stdin; default: the one on disk)")
		service  = fs.String("service", "", "restart, logs, show: one of the site's services")
		build    = fs.Bool("build", false, "logs: the service's image build instead of its output")
		noSafety = fs.Bool("no-safety-copy", false, "backups restore: skip the copy of the database as it is now")
		name     = fs.String("name", "", "staging create: the copy's domain (default staging.<domain>)")
		days     = fs.Int("days", 7, "visits: how many days back")
	)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	cl, err := e.client(c)
	if err != nil {
		return err
	}
	domain := ""
	if len(pos) > 0 {
		domain = strings.ToLower(pos[0])
	}
	need := func() error {
		if domain == "" {
			return fmt.Errorf("site %s: a domain is required", sub)
		}
		return nil
	}
	switch sub {
	case "list", "ls":
		var raw []byte
		if err := cl.Do("GET", "/api/v1/sites", nil, &raw); err != nil {
			return err
		}
		if c.json {
			e.printJSON(raw)
			return nil
		}
		var list []sites.Site
		if err := json.Unmarshal(raw, &list); err != nil {
			return err
		}
		rows := make([][]string, 0, len(list))
		for _, s := range list {
			mode := string(s.Mode)
			if !s.Managed {
				mode = "unmanaged"
			}
			port := ""
			if s.Port != 0 {
				port = strconv.Itoa(s.Port)
			}
			rows = append(rows, []string{s.Domain, mode, yesNo(s.Alive), port, s.Revision, oneLine(s.Error)})
		}
		Table(e.Stdout, []string{"DOMAIN", "MODE", "ALIVE", "PORT", "REVISION", "ERROR"}, rows)
		return nil
	case "show":
		if err := need(); err != nil {
			return err
		}
		site, svc, err := e.ownerOf(cl, domain)
		if err != nil {
			return err
		}
		if *service != "" {
			svc = *service
		}
		var raw []byte
		if err := cl.Do("GET", "/api/v1/sites/"+site, nil, &raw); err != nil {
			return err
		}
		return e.showSite(c, raw, svc)
	case "routes":
		if err := need(); err != nil {
			return err
		}
		site, _, err := e.ownerOf(cl, domain)
		if err != nil {
			return err
		}
		return e.siteRoutes(c, cl, site)
	case "add":
		if err := need(); err != nil {
			return err
		}
		body := map[string]any{"domain": domain}
		if *manifest != "" {
			var data []byte
			if *manifest == "-" {
				data, err = io.ReadAll(e.Stdin)
			} else {
				data, err = os.ReadFile(*manifest)
			}
			if err != nil {
				return err
			}
			body["manifest"] = json.RawMessage(data)
		} else {
			body["template"] = *template
		}
		switch *publish {
		case "":
		case "true", "1", "yes":
			body["publish"] = true
		case "false", "0", "no":
			body["publish"] = false
		default:
			return fmt.Errorf("site add: --publish is true or false, not %q", *publish)
		}
		if *door != "" {
			body["door"] = *door
		}
		var raw []byte
		if err := cl.Do("POST", "/api/v1/sites", body, &raw); err != nil {
			return err
		}
		if c.json {
			e.printJSON(raw)
			return nil
		}
		var out struct {
			Publishing bool `json:"publishing"`
		}
		_ = json.Unmarshal(raw, &out)
		fmt.Fprintf(e.Stdout, "Created %s. The engine picks it up on its next tick.\n", domain)
		if out.Publishing {
			fmt.Fprintf(e.Stdout, "Publishing it in the background: `middleware site published` shows how it went.\n")
		}
		return nil
	case "publish":
		if err := need(); err != nil {
			return err
		}
		body := map[string]any{}
		if *door != "" {
			body["door"] = *door
		}
		var out struct {
			Lines []struct{ Status, Text string } `json:"lines"`
		}
		if err := cl.Do("POST", "/api/v1/sites/"+domain+"/publish", body, &out); err != nil {
			return err
		}
		return e.printLines(out.Lines)
	case "unpublish":
		if err := need(); err != nil {
			return err
		}
		var out struct {
			Lines []struct{ Status, Text string } `json:"lines"`
		}
		if err := cl.Do("POST", "/api/v1/sites/"+domain+"/unpublish", map[string]any{}, &out); err != nil {
			return err
		}
		return e.printLines(out.Lines)
	case "published":
		var raw []byte
		if err := cl.Do("GET", "/api/v1/publish", nil, &raw); err != nil {
			return err
		}
		if c.json {
			e.printJSON(raw)
			return nil
		}
		var recs map[string]struct {
			Site, Door, State, Error string
		}
		if err := json.Unmarshal(raw, &recs); err != nil {
			return err
		}
		names := make([]string, 0, len(recs))
		for n := range recs {
			names = append(names, n)
		}
		sort.Strings(names)
		rows := make([][]string, 0, len(names))
		for _, n := range names {
			r := recs[n]
			rows = append(rows, []string{n, r.Site, r.Door, r.State, oneLine(r.Error)})
		}
		Table(e.Stdout, []string{"NAME", "SITE", "DOOR", "STATE", "ERROR"}, rows)
		return nil
	case "rm", "remove", "delete":
		if err := need(); err != nil {
			return err
		}
		var raw []byte
		q := ""
		if *archive {
			q = "?archive=true"
		}
		if err := cl.Do("DELETE", "/api/v1/sites/"+domain+q, nil, &raw); err != nil {
			return err
		}
		if c.json {
			e.printJSON(raw)
			return nil
		}
		var out struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(raw, &out)
		if out.Path != "" {
			fmt.Fprintf(e.Stdout, "Archived %s to %s\n", domain, out.Path)
		} else {
			fmt.Fprintf(e.Stdout, "Deleted %s\n", domain)
		}
		return nil
	case "restart":
		if *all {
			var list []sites.Site
			if err := cl.Do("GET", "/api/v1/sites", nil, &list); err != nil {
				return err
			}
			for _, s := range list {
				if !s.Alive {
					continue
				}
				if err := cl.Do("POST", "/api/v1/sites/"+s.Domain+"/restart", nil, nil); err != nil {
					fmt.Fprintf(e.Stderr, "%s: %v\n", s.Domain, err)
					continue
				}
				fmt.Fprintf(e.Stdout, "%s: handover started\n", s.Domain)
			}
			return nil
		}
		if err := need(); err != nil {
			return err
		}
		if *service != "" {
			return e.simple(c, cl, "POST", "/api/v1/sites/"+domain+"/services/"+url.PathEscape(*service)+"/restart", nil, "A new generation of "+*service+" is starting; the other services are not touched")
		}
		return e.simple(c, cl, "POST", "/api/v1/sites/"+domain+"/restart", nil, "Handover started for "+domain)
	case "stop":
		if err := need(); err != nil {
			return err
		}
		if *service != "" {
			return e.simple(c, cl, "POST", "/api/v1/sites/"+domain+"/services/"+url.PathEscape(*service)+"/stop", nil, "Stopped "+*service+" of "+domain+" (services."+*service+".disabled in site.json)")
		}
		return e.simple(c, cl, "POST", "/api/v1/sites/"+domain+"/stop", nil, "Stopped "+domain+" (disabled in site.json)")
	case "start":
		if err := need(); err != nil {
			return err
		}
		if *service != "" {
			return e.simple(c, cl, "POST", "/api/v1/sites/"+domain+"/services/"+url.PathEscape(*service)+"/start", nil, "Started "+*service+" of "+domain)
		}
		return e.simple(c, cl, "POST", "/api/v1/sites/"+domain+"/start", nil, "Started "+domain)
	case "hold":
		if err := need(); err != nil {
			return err
		}
		if *service != "" {
			return e.simple(c, cl, "POST", "/api/v1/sites/"+domain+"/services/"+url.PathEscape(*service)+"/hold", map[string]any{"enabled": true, "title": *title, "message": *message}, "The names of "+*service+" are behind the holding page")
		}
		return e.simple(c, cl, "POST", "/api/v1/sites/"+domain+"/hold", map[string]any{"enabled": true, "title": *title, "message": *message}, domain+" is behind the holding page")
	case "unhold":
		if err := need(); err != nil {
			return err
		}
		if *service != "" {
			return e.simple(c, cl, "POST", "/api/v1/sites/"+domain+"/services/"+url.PathEscape(*service)+"/hold", map[string]any{"enabled": false}, *service+" is back")
		}
		return e.simple(c, cl, "POST", "/api/v1/sites/"+domain+"/hold", map[string]any{"enabled": false}, domain+" is back")
	case "validate", "check":
		if err := need(); err != nil {
			return err
		}
		return e.validateSite(c, cl, domain, *file)
	case "deploy":
		if err := need(); err != nil {
			return err
		}
		return e.simple(c, cl, "POST", "/api/v1/sites/"+domain+"/deploy", nil, "Deployment started for "+domain+"; follow it with `middleware events -f`")
	case "tasks":
		if err := need(); err != nil {
			return err
		}
		// The server's default is longer than the site log's; only a
		// --lines given on the command line is passed on.
		n := 0
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "lines" {
				n = *lines
			}
		})
		return e.siteTasks(c, cl, domain, pos[1:], n)
	case "visits":
		if err := need(); err != nil {
			return err
		}
		return e.siteVisits(c, cl, domain, *days)
	case "backups", "backup":
		if err := need(); err != nil {
			return err
		}
		n := 0
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "lines" {
				n = *lines
			}
		})
		return e.siteBackups(c, cl, domain, pos[1:], n, *noSafety)
	case "security":
		if err := need(); err != nil {
			return err
		}
		return e.siteSecurity(c, cl, domain, pos[1:])
	case "staging":
		if err := need(); err != nil {
			return err
		}
		return e.siteStaging(c, cl, domain, pos[1:], *name, *publish, *door)
	case "logs", "log":
		if err := need(); err != nil {
			return err
		}
		q := url.Values{}
		q.Set("lines", strconv.Itoa(*lines))
		if *worker != "" {
			q.Set("worker", *worker)
		}
		if *service != "" {
			q.Set("service", *service)
			if *build {
				q.Set("build", "1")
			}
		}
		if *follow {
			q.Set("follow", "1")
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		body, err := cl.Stream(ctx, "/api/v1/sites/"+domain+"/logs?"+q.Encode())
		if err != nil {
			return err
		}
		defer body.Close()
		_, err = io.Copy(e.Stdout, body)
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	fmt.Fprint(e.Stderr, siteUsage)
	return fmt.Errorf("site: unknown command %q", sub)
}

// simple does a call whose answer is either printed as JSON or summed
// up in one line.
func (e *Env) simple(c common, cl *Client, method, path string, body any, done string) error {
	var raw []byte
	if err := cl.Do(method, path, body, &raw); err != nil {
		return err
	}
	if c.json {
		e.printJSON(raw)
		return nil
	}
	fmt.Fprintln(e.Stdout, done)
	return nil
}

func (e *Env) showSite(c common, raw []byte, only string) error {
	if c.json {
		e.printJSON(raw)
		return nil
	}
	var s sites.Site
	if err := json.Unmarshal(raw, &s); err != nil {
		return err
	}
	pairs := [][2]string{
		{"domain", s.Domain},
		{"mode", string(s.Mode)},
		{"managed", yesNo(s.Managed)},
		{"alive", yesNo(s.Alive)},
	}
	if s.Port != 0 {
		pairs = append(pairs, [2]string{"port", strconv.Itoa(s.Port)})
	}
	if s.Revision != "" {
		pairs = append(pairs, [2]string{"revision", s.Revision})
	}
	if s.Handing {
		pairs = append(pairs, [2]string{"handing over", "yes"})
	}
	if s.Error != "" {
		pairs = append(pairs, [2]string{"error", s.Error})
	}
	for name, w := range s.Workers {
		pairs = append(pairs, [2]string{"worker " + name, fmt.Sprintf("port %d, alive %s %s", w.Port, yesNo(w.Alive), w.Error)})
	}
	KeyValues(e.Stdout, pairs)
	if len(s.Services) > 0 {
		if only != "" {
			if _, ok := s.Services[only]; !ok {
				return fmt.Errorf("%s has no service %q", s.Domain, only)
			}
		}
		fmt.Fprintln(e.Stdout)
		rows := [][]string{}
		for _, name := range sortedNames(s.Services) {
			v := s.Services[name]
			if only != "" && name != only {
				continue
			}
			port := ""
			if v.Port != 0 {
				port = strconv.Itoa(v.Port)
			}
			if !v.Published {
				port += " (site network only)"
			}
			state := serviceState(v.Alive, v.Starting, v.Stopped, v.Waiting, v.Error)
			rows = append(rows, []string{name, strings.Join(v.Domains, " "), v.Runtime, yesNo(v.Alive), strings.TrimSpace(port), v.Revision, state, oneLine(v.Error)})
		}
		Table(e.Stdout, []string{"SERVICE", "DOMAINS", "RUNTIME", "ALIVE", "PORT", "REVISION", "STATE", "ERROR"}, rows)
		if only != "" {
			return nil
		}
	}
	if s.Manifest != nil {
		data, _ := json.MarshalIndent(s.Manifest, "", "  ")
		fmt.Fprintf(e.Stdout, "\nsite.json\n%s\n", data)
	}
	return nil
}

// --- events --------------------------------------------------------------

func (e *Env) events(args []string) error {
	fs := flag.NewFlagSet("events", flag.ContinueOnError)
	var c common
	c.bind(fs)
	follow := fs.Bool("f", false, "stream new events as they happen")
	since := fs.String("since", "", "RFC3339 time, or a duration back from now (1h)")
	typ := fs.String("type", "", "type prefix (instance., handover., auth.)")
	subject := fs.String("subject", "", "a domain")
	limit := fs.Int("limit", 50, "how many")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	cl, err := e.client(c)
	if err != nil {
		return err
	}
	q := url.Values{}
	if *since != "" {
		if d, err := time.ParseDuration(*since); err == nil {
			q.Set("since", time.Now().Add(-d).UTC().Format(time.RFC3339Nano))
		} else {
			q.Set("since", *since)
		}
	}
	if *typ != "" {
		q.Set("type", *typ)
	}
	if *subject != "" {
		q.Set("subject", *subject)
	}
	if !*follow {
		q.Set("limit", strconv.Itoa(*limit))
		var raw []byte
		if err := cl.Do("GET", "/api/v1/events?"+q.Encode(), nil, &raw); err != nil {
			return err
		}
		if c.json {
			e.printJSON(raw)
			return nil
		}
		var list []events.Event
		if err := json.Unmarshal(raw, &list); err != nil {
			return err
		}
		rows := make([][]string, 0, len(list))
		for _, ev := range list {
			rows = append(rows, eventRow(ev))
		}
		Table(e.Stdout, []string{"TIME", "TYPE", "SUBJECT", "ACTOR", "MESSAGE"}, rows)
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	body, err := cl.Stream(ctx, "/api/v1/events/stream?"+q.Encode())
	if err != nil {
		return err
	}
	defer body.Close()
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		if c.json {
			fmt.Fprintln(e.Stdout, data)
			continue
		}
		var ev events.Event
		if json.Unmarshal([]byte(data), &ev) != nil {
			continue
		}
		fmt.Fprintln(e.Stdout, strings.Join(eventRow(ev), "  "))
	}
	if ctx.Err() != nil {
		return nil
	}
	return sc.Err()
}

func eventRow(ev events.Event) []string {
	msg := ev.Message
	if ev.Level != "" {
		msg = "[" + ev.Level + "] " + msg
	}
	return []string{ev.Time.Local().Format("2006-01-02 15:04:05"), ev.Type, ev.Subject, ev.Actor, oneLine(msg)}
}

// --- system --------------------------------------------------------------

func (e *Env) system(args []string) error {
	fs := flag.NewFlagSet("system", flag.ContinueOnError)
	var c common
	c.bind(fs)
	if _, err := parse(fs, args); err != nil {
		return err
	}
	cl, err := e.client(c)
	if err != nil {
		return err
	}
	var raw []byte
	if err := cl.Do("GET", "/api/v1/system", nil, &raw); err != nil {
		return err
	}
	if c.json {
		e.printJSON(raw)
		return nil
	}
	var sys struct {
		Version     string             `json:"version"`
		Root        string             `json:"root"`
		Uptime      string             `json:"uptime"`
		Listen      string             `json:"listen"`
		PanelDomain string             `json:"panel_domain"`
		Go          string             `json:"go"`
		Hostname    string             `json:"hostname"`
		Load        map[string]float64 `json:"load"`
		Memory      map[string]int64   `json:"memory"`
		Disk        map[string]int64   `json:"disk"`
	}
	if err := json.Unmarshal(raw, &sys); err != nil {
		return err
	}
	KeyValues(e.Stdout, [][2]string{
		{"version", sys.Version + " (" + sys.Go + ")"},
		{"host", sys.Hostname},
		{"root", sys.Root},
		{"uptime", sys.Uptime},
		{"listen", sys.Listen},
		{"panel", orDash(sys.PanelDomain)},
		{"via", cl.Via},
		{"load", fmt.Sprintf("%.2f %.2f %.2f", sys.Load["1m"], sys.Load["5m"], sys.Load["15m"])},
		{"memory", fmt.Sprintf("%s used of %s", bytesHuman(sys.Memory["used"]), bytesHuman(sys.Memory["total"]))},
		{"disk", fmt.Sprintf("%s free of %s", bytesHuman(sys.Disk["free"]), bytesHuman(sys.Disk["total"]))},
	})
	return nil
}

// --- token ---------------------------------------------------------------

const tokenUsage = `usage: middleware token <command>

  add <name> [--scope s]...
                create a token; the secret is printed once. With --scope it
                is a service door: read, restart, deploy or tasks, optionally
                for one site (restart:shop.example), and nothing else
  list          tokens, with when each was last used
  rm <id>       revoke a token
`

func (e *Env) token(args []string) error {
	if len(args) == 0 || args[0] == "help" {
		fmt.Fprint(e.Stderr, tokenUsage)
		return nil
	}
	sub, args := args[0], args[1:]
	fs := flag.NewFlagSet("token "+sub, flag.ContinueOnError)
	var c common
	c.bind(fs)
	var scopes multiFlag
	fs.Var(&scopes, "scope", "what the token opens (read, restart, deploy, tasks; :site to narrow); repeatable")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	cl, err := e.client(c)
	if err != nil {
		return err
	}
	switch sub {
	case "add", "create":
		if len(pos) == 0 {
			return errors.New("token add: a name is required")
		}
		var raw []byte
		body := map[string]any{"name": strings.Join(pos, " ")}
		if len(scopes) > 0 {
			body["scopes"] = []string(scopes)
		}
		if err := cl.Do("POST", "/api/v1/auth/tokens", body, &raw); err != nil {
			return err
		}
		if c.json {
			e.printJSON(raw)
			return nil
		}
		var out struct {
			ID    string `json:"id"`
			Token string `json:"token"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return err
		}
		fmt.Fprintf(e.Stdout, "Token %s created. The secret is shown once:\n\n  %s\n\nUse it as `Authorization: Bearer …` or in $%s.\n", out.ID, out.Token, TokenEnv)
		return nil
	case "list", "ls":
		var raw []byte
		if err := cl.Do("GET", "/api/v1/auth/tokens", nil, &raw); err != nil {
			return err
		}
		if c.json {
			e.printJSON(raw)
			return nil
		}
		var list []auth.Public
		if err := json.Unmarshal(raw, &list); err != nil {
			return err
		}
		rows := make([][]string, 0, len(list))
		for _, t := range list {
			scope := "all"
			if len(t.Scopes) > 0 {
				scope = strings.Join(t.Scopes, ",")
			}
			rows = append(rows, []string{t.ID, t.Name, scope, t.Created.Local().Format("2006-01-02"), ago(t.LastUsed)})
		}
		Table(e.Stdout, []string{"ID", "NAME", "OPENS", "CREATED", "LAST USED"}, rows)
		return nil
	case "rm", "remove", "delete", "revoke":
		if len(pos) == 0 {
			return errors.New("token rm: an id is required")
		}
		return e.simple(c, cl, "DELETE", "/api/v1/auth/tokens/"+url.PathEscape(pos[0]), nil, "Token "+pos[0]+" revoked")
	}
	fmt.Fprint(e.Stderr, tokenUsage)
	return fmt.Errorf("token: unknown command %q", sub)
}

// multiFlag collects a repeated flag.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func oneLine(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) > 80 {
		return s[:77] + "…"
	}
	return s
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// printLines prints a publish or move result one line per name, and fails
// when any line failed, so a script notices.
func (e *Env) printLines(lines []struct{ Status, Text string }) error {
	failed := 0
	for _, l := range lines {
		mark := map[string]string{"ok": "✓", "skip": "·", "fail": "✗"}[l.Status]
		fmt.Fprintf(e.Stdout, "%s %s\n", mark, l.Text)
		if l.Status == "fail" {
			failed++
		}
	}
	if len(lines) == 0 {
		fmt.Fprintln(e.Stdout, "nothing to do: every name is already where it should be")
	}
	if failed > 0 {
		return fmt.Errorf("%d name(s) failed; they are retried with backoff", failed)
	}
	return nil
}

// --- nodes ---------------------------------------------------------------

func (e *Env) nodes(args []string) error {
	fs := flag.NewFlagSet("nodes", flag.ContinueOnError)
	var c common
	c.bind(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	cl, err := e.client(c)
	if err != nil {
		return err
	}
	if len(pos) >= 1 && pos[0] == "panel" {
		if len(pos) != 2 || (pos[1] != "open" && pos[1] != "close") {
			return fmt.Errorf("usage: middleware nodes panel open|close")
		}
		done := "every node's panel answers the internet again (within a minute on the others)"
		if pos[1] == "close" {
			done = "every node's panel answers only through a VPN (within a minute on the others)"
		}
		return e.simple(c, cl, "PUT", "/api/v1/nodes/settings", map[string]bool{"panel_vpn_only": pos[1] == "close"}, done)
	}
	var raw []byte
	if err := cl.Do("GET", "/api/v1/nodes", nil, &raw); err != nil {
		return err
	}
	if c.json {
		e.printJSON(raw)
		return nil
	}
	var in struct {
		Self struct {
			Name string `json:"name"`
			IP   string `json:"ip"`
		} `json:"self"`
		Network  string `json:"network"`
		Up       bool   `json:"up"`
		Error    string `json:"error"`
		Settings struct {
			PanelVPNOnly bool `json:"panel_vpn_only"`
		} `json:"settings"`
		Members []struct {
			Name     string `json:"name"`
			IP       string `json:"ip"`
			Endpoint string `json:"endpoint"`
			Version  string `json:"version"`
			Online   bool   `json:"online"`
		} `json:"members"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return err
	}
	if in.Network == "" {
		fmt.Fprintf(e.Stdout, "%s is in no network\n", in.Self.Name)
		return nil
	}
	KeyValues(e.Stdout, [][2]string{
		{"node", in.Self.Name + " " + in.Self.IP},
		{"tunnel", yesNo(in.Up) + " " + in.Error},
		{"panel only by vpn", yesNo(in.Settings.PanelVPNOnly)},
	})
	rows := [][]string{}
	for _, m := range in.Members {
		rows = append(rows, []string{m.Name, m.IP, orDash(m.Endpoint), m.Version, yesNo(m.Online)})
	}
	if len(rows) > 0 {
		fmt.Fprintln(e.Stdout)
		Table(e.Stdout, []string{"NODE", "IP", "ENDPOINT", "VERSION", "ONLINE"}, rows)
	}
	return nil
}

// joinNetwork enters the network an invitation was made in, through the
// running supervisor (it holds the mesh and asks the helper for the
// interface). The installer calls it right after starting the service,
// so the socket is given a moment. With no endpoint, the one the house
// suggests (its public address with the mesh port) is used: behind a NAT
// it is not reachable, and the node then only calls out, which works.
func (e *Env) joinNetwork(root, code, endpoint string) error {
	cl, err := e.Connect(root)
	for i := 0; err != nil && i < 30; i++ {
		time.Sleep(time.Second)
		cl, err = e.Connect(root)
	}
	if err != nil {
		return fmt.Errorf("the service is not answering: %w", err)
	}
	if endpoint == "" {
		var info struct {
			Suggested string `json:"suggested_endpoint"`
		}
		var raw []byte
		if cl.Do("GET", "/api/v1/nodes", nil, &raw) == nil && json.Unmarshal(raw, &info) == nil {
			endpoint = info.Suggested
		}
	}
	var raw []byte
	if err := cl.Do("POST", "/api/v1/nodes/join", map[string]string{"code": code, "endpoint": endpoint}, &raw); err != nil {
		return err
	}
	var out struct {
		Inviter string `json:"inviter"`
		Members int    `json:"members"`
	}
	_ = json.Unmarshal(raw, &out)
	where := endpoint
	if where == "" {
		where = "no public endpoint: it calls out"
	}
	fmt.Fprintf(e.Stdout, "Joined the network of %s (%d other nodes; this one at %s).\nManage it from their panels: Dashboard, this node's card. It has no domain and no operator of its own; `middleware setup --domain <name>` gives it one later.\n", out.Inviter, out.Members, where)
	return nil
}
