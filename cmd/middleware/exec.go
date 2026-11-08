package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/dcimorra/the-middleware/internal/api"
	"github.com/dcimorra/the-middleware/internal/chat"
	"github.com/dcimorra/the-middleware/internal/cli"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/runtime"
	"github.com/dcimorra/the-middleware/internal/settings"
)

func init() {
	subcommands["exec"] = execCommand
	extraUsage += `  exec <domain> [--service s] [--] <command…>
               run a command where the site runs (inside its container if it has one,
               or one of its services')
`
}

// execCommand needs no running supervisor for a site it can run itself:
// it reads the manifest and runs the command, so it works while the site
// is down, which is when an install or a migration is usually needed.
// What it cannot run itself goes through the house: a container from a
// site's turn, whose jail has no docker socket ("failed to connect to the
// docker API" was all it said), and a service's container, whose image
// and network only the running supervisor knows.
func execCommand(args []string) error {
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	root := fs.String("root", "", "installation root (default $MIDDLEWARE_ROOT or /srv/middleware)")
	tty := fs.Bool("t", false, "allocate a terminal (an interactive shell)")
	service := fs.String("service", "", "for a site with services, the one to run in (default: the one that answers the site's name)")
	// Flags before the domain, and before the command: everything from the
	// command's first word on is its own (npm install --omit=dev).
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) < 2 {
		return fmt.Errorf("usage: middleware exec <domain> [--service s] [--] <command…>")
	}
	domain := strings.ToLower(rest[0])
	// And between the domain and the command, which is where --service
	// reads naturally (`exec shop.example --service db -- mysql`). The
	// command's own flags are past its first word, or past "--", where
	// the parse stops.
	if err := fs.Parse(rest[1:]); err != nil {
		return err
	}
	cmdArgs := fs.Args()
	if len(cmdArgs) > 0 && cmdArgs[0] == "--" {
		cmdArgs = cmdArgs[1:]
	}
	if len(cmdArgs) == 0 {
		return fmt.Errorf("usage: middleware exec <domain> [--service s] [--] <command…>")
	}
	if !manifest.ValidName(domain) {
		return fmt.Errorf("%q is not a site or project", domain)
	}
	// The skill tells a docker site's conversation to install with
	// `middleware exec <its domain>`, and the command needs no supervisor:
	// from a site's turn, `middleware exec other.site -- env` printed the
	// other site's secrets, the very folder its jail denies. A turn may
	// exec in its own site and nowhere else; one that cannot be told
	// (two sites' marks in its chain) nowhere.
	turn, err := chat.SiteOfProcess("/proc", os.Getpid(), nil)
	if err != nil {
		return fmt.Errorf("exec refused: %w", err)
	}
	if turn == chat.Autopilot {
		// Run here, it would skip the API, and with it the manifest that
		// keeps the agent out of a site: its work in a site goes through
		// that site's conversation.
		return fmt.Errorf("exec refused: the autonomous agent hands work in a site to that site's conversation (middleware autopilot ask %s -)", domain)
	}
	if turn != "" && turn != domain {
		return fmt.Errorf("exec refused: this runs under a turn of %s's conversation, which may exec in %s only", turn, turn)
	}
	dir := settings.Root(*root)
	siteDir := filepath.Join(dir, "sites", domain)
	m, err := manifest.Load(siteDir)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var spec *manifest.Spec
	switch {
	case len(m.Services) > 0:
		name := *service
		if name == "" {
			n, ok := m.ServiceFor(domain, "/")
			if !ok {
				return fmt.Errorf("no service answers %s: name one with --service (%s)", domain, strings.Join(m.ServiceNames(), ", "))
			}
			name = n
		}
		sv := m.Services[name]
		if sv == nil {
			return fmt.Errorf("%s has no service %q (services: %s)", domain, name, strings.Join(m.ServiceNames(), ", "))
		}
		if isDocker(sv.Run) {
			return execThroughHouse(ctx, dir, domain, name, cmdArgs, *tty)
		}
		// A process service runs here, in this jail if a turn's, with
		// the ports of the services it names as the house has them.
		ports, err := servicePorts(dir, domain)
		if err != nil {
			return err
		}
		spec, err = manifest.ServiceSpec(domain, siteDir, name, m, func(target string, fromContainer bool) (string, int, bool) {
			if t := m.Services[target]; t != nil && t.Run != nil && t.Run.Port != 0 {
				return "127.0.0.1", t.Run.Port, true
			}
			p, ok := ports[target]
			return "127.0.0.1", p, ok && p != 0
		})
		if err != nil {
			return err
		}
	case *service != "":
		return fmt.Errorf("%s has no services", domain)
	default:
		if m.Run == nil {
			// A static site or a project still gets its folder and HOME.
			m = &manifest.Manifest{Version: manifest.Version, Run: &manifest.Run{Command: []string{"true"}}}
		}
		if turn != "" && isDocker(m.Run) {
			return execThroughHouse(ctx, dir, domain, "", cmdArgs, *tty)
		}
		spec, err = manifest.Resolve(domain, siteDir, m)
		if err != nil {
			return err
		}
	}
	code, err := runtime.Exec(ctx, spec, filepath.Join(dir, "data", "homes", domain), runtime.ExecOptions{
		Args: cmdArgs, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, TTY: *tty,
	})
	if err != nil {
		return err
	}
	if code != 0 {
		os.Exit(code)
	}
	return nil
}

func isDocker(r *manifest.Run) bool { return r != nil && r.Runtime == "docker" }

// execThroughHouse asks the supervisor to run the command in the site's or
// the service's container, and prints its output as it comes. It has no
// terminal and no stdin: a shell to type into is the operator's, on the
// machine.
func execThroughHouse(ctx context.Context, root, domain, service string, argv []string, tty bool) error {
	if tty {
		return fmt.Errorf("-t: a terminal in a container is the operator's, from the machine; run the command itself")
	}
	c, err := cli.Connect(root)
	if err != nil {
		return err
	}
	resp, err := c.PostStream(ctx, "/api/v1/sites/"+domain+"/exec", map[string]any{"args": argv, "service": service, "stream": true})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if _, err := io.Copy(os.Stdout, resp.Body); err != nil {
		return err
	}
	if msg := resp.Trailer.Get(api.ExecErrorTrailer); msg != "" {
		fmt.Fprintln(os.Stderr, "middleware exec: "+msg)
	}
	code, err := strconv.Atoi(resp.Trailer.Get(api.ExitCodeTrailer))
	if err != nil {
		return fmt.Errorf("the house did not say how the command ended")
	}
	if code != 0 {
		os.Exit(code)
	}
	return nil
}

// servicePorts are the machine's ports of a site's services, as the
// running supervisor has them; nothing when it is not running.
func servicePorts(root, domain string) (map[string]int, error) {
	out := map[string]int{}
	c, err := cli.Connect(root)
	if err != nil {
		return out, nil
	}
	var site struct {
		Services map[string]struct {
			Port int `json:"port"`
		} `json:"services"`
	}
	if err := c.Do("GET", "/api/v1/sites/"+domain, nil, &site); err != nil {
		return out, nil
	}
	for name, s := range site.Services {
		out[name] = s.Port
	}
	return out, nil
}
