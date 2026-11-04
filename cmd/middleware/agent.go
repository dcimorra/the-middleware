package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dcimorra/the-middleware/internal/agent"
	"github.com/dcimorra/the-middleware/internal/agent/agy"
	"github.com/dcimorra/the-middleware/internal/agent/claude"
	"github.com/dcimorra/the-middleware/internal/agent/opencode"
	"github.com/dcimorra/the-middleware/internal/api"
	"github.com/dcimorra/the-middleware/internal/app"
	"github.com/dcimorra/the-middleware/internal/cgroups"
	"github.com/dcimorra/the-middleware/internal/chat"
	"github.com/dcimorra/the-middleware/internal/cli"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/settings"
	"github.com/dcimorra/the-middleware/internal/tasks"
)

// chatManager and taskScheduler are what the api hook reaches for; they
// exist once the hooks are built, before any hook runs.
var (
	chatManager   *chat.Manager
	taskScheduler *tasks.Scheduler
)

func init() {
	hookBuilders = append(hookBuilders,
		hookBuilder{Name: "chat", Order: orderChat, Build: newChatHook},
		hookBuilder{Name: "tasks", Order: orderTasks, Build: newTasksHook},
	)
	subcommands["tasks"] = tasksCmd
	subcommands["chat"] = chatCmd
	subcommands["skill"] = skillCmd
	subcommands["agents"] = agentsCmd
	extraUsage += `  agents       the agent providers and how many turns run of how many
  tasks        list the scheduled tasks with their next run
  tasks run <name>
               run a task now and wait for it
  chat <domain> <text>
               send one message to a site's conversation ("middleware" for the installation's own)
  skill install [dir]
               write the embedded middleware skill (default: <root>/.claude/skills/middleware/)
`
}

// skillCmd installs the skill the conversations run with where an
// operator running Claude Code by hand on the server will load it: the
// root's own .claude/skills, or wherever they say.
func skillCmd(args []string) error {
	fs := flag.NewFlagSet("skill", flag.ContinueOnError)
	root := rootFlag(fs)
	rest, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(rest) == 0 || rest[0] != "install" || len(rest) > 2 {
		return errors.New("usage: middleware skill install [dir]")
	}
	dir := filepath.Join(settings.Root(*root), ".claude", "skills", chat.SkillName)
	if len(rest) == 2 {
		dir = rest[1]
	}
	if err := chat.InstallSkill(dir); err != nil {
		return err
	}
	fmt.Printf("%s\n", filepath.Join(dir, "SKILL.md"))
	return nil
}

// providers is the registry every conversation picks from. Credentials
// and binaries come from the agent modules when they are enabled; without
// them the adapters rely on the CLI's own login under the site's HOME.
// There is one registry per process so the agent modules, which replace a
// provider when enabled, and the chat manager, which picks one per turn,
// are looking at the same map.
func providers() *agent.Registry {
	providersOnce.Do(func() {
		providersCur = agent.NewRegistry()
		providersCur.Register(claude.New(claude.Config{}))
		providersCur.Register(agy.New(agy.Config{}))
		// Without its module there are no keys, and it says so.
		providersCur.Register(opencode.New(opencode.Config{}))
	})
	return providersCur
}

var (
	providersOnce sync.Once
	providersCur  *agent.Registry
)

// usageBook is the one book of subscription limits: the chat writes what
// the turns say into it and the agent modules what the vendors answer, so
// both have to hold the same one.
func usageBook(root string) *agent.UsageBook {
	usageOnce.Do(func() { usageCur = agent.OpenUsageBook(filepath.Join(root, "data", "agents", "usage.json")) })
	return usageCur
}

var (
	usageOnce sync.Once
	usageCur  *agent.UsageBook
)

type chatHook struct{}

func newChatHook(a *app.App) app.Hook {
	// Agent turns start in a cgroup of their own with a low CPU weight and
	// a ceiling, so a conversation's browsers cannot starve the sites. The
	// ceiling is the machine's unless the settings say otherwise, and
	// follows them live: the chat manager hands every new section on.
	machine := cgroups.ThisMachine()
	tree, err := cgroups.Setup(cgroups.SelfPath(), a.Settings.Agents.Limits(machine))
	if err != nil {
		a.Bus.Emit(events.Event{Type: "cgroups.off", Actor: "engine", Level: "warn", Message: err.Error()})
	} else {
		agent.SpawnAttr = tree.AgentsAttr
		a.Bus.Emit(events.Event{Type: "cgroups.on", Actor: "engine", Message: "agent turns run in " + tree.Agents()})
		// Each site gets a group of its own beside house/ and agents/:
		// CPU shared per site, not per process.
		wireSiteGroups(a, tree)
	}
	onLimits := func(ag settings.Agents) {
		if err := tree.SetAgents(ag.Limits(machine)); err != nil {
			a.Bus.Emit(events.Event{Type: "cgroups.limits", Actor: "engine", Level: "warn", Message: err.Error()})
		}
	}
	chatManager = chat.New(chat.Options{Root: a.Root, Bus: a.Bus, Registry: providers(), Limits: a.Settings.Agents, Usage: usageBook(a.Root), OnLimits: onLimits})
	// A figure that moved reaches the chats open on the panel at once:
	// a long turn moves the window while nobody types.
	usageBook(a.Root).OnChange(func(provider string, u agent.Usage) {
		a.Bus.Emit(events.Event{Type: "agent.usage", Subject: provider, Actor: "agent", Data: map[string]any{"provider": provider, "usage": u}})
	})
	return chatHook{}
}

// agentsCmd prints the providers and the live capacity, through the
// running supervisor: the numbers live in its chat manager, not on disk.
func agentsCmd(args []string) error {
	fs := flag.NewFlagSet("agents", flag.ContinueOnError)
	root := rootFlag(fs)
	asJSON := fs.Bool("json", false, "print the API's answer as JSON")
	if _, err := parseInterleaved(fs, args); err != nil {
		return err
	}
	cl, err := cli.Connect(settings.Root(*root))
	if err != nil {
		return err
	}
	if *asJSON {
		var raw []byte
		if err := cl.Do("GET", api.Prefix+"/agents", nil, &raw); err != nil {
			return err
		}
		fmt.Println(strings.TrimRight(string(raw), "\n"))
		return nil
	}
	var out api.Agents
	if err := cl.Do("GET", api.Prefix+"/agents", nil, &out); err != nil {
		return err
	}
	rows := [][]string{}
	for _, p := range out.Providers {
		state := "not available"
		if p.Available {
			state = "available"
		}
		used, max := "-", "-"
		if out.Capacity != nil {
			if c, ok := out.Capacity.Providers[p.Provider]; ok {
				used = fmt.Sprint(c.Used)
				max = "no cap"
				if c.Max > 0 {
					max = fmt.Sprint(c.Max)
				}
			}
		}
		rows = append(rows, []string{p.Provider, state, p.Where, used, max, strings.Join(p.Models, ", ")})
	}
	cli.Table(os.Stdout, []string{"PROVIDER", "STATE", "WHERE", "RUNNING", "CAP", "MODELS"}, rows)
	if out.Capacity == nil {
		return nil
	}
	c := out.Capacity
	fmt.Printf("\ncapacity: %d of %d running, %d reserved for the house\n", c.Used, c.Max, c.Reserved)
	for _, r := range c.Running {
		fmt.Printf("  %-32s %-8s since %s (turn %s)\n", r.Domain, r.Provider, r.Started.Format("15:04:05"), r.TurnID)
	}
	return nil
}

func (chatHook) Wrap(next http.Handler) http.Handler { return next }

// Run picks up the turns a previous supervisor left in flight.
func (chatHook) Run(ctx context.Context, a *app.App) error {
	if chatManager != nil {
		chatManager.Recover(ctx)
	}
	return nil
}

type tasksHook struct{}

func newTasksHook(a *app.App) app.Hook {
	taskScheduler = tasks.NewScheduler(tasks.Options{Root: a.Root, Bus: a.Bus})
	return tasksHook{}
}

func (tasksHook) Wrap(next http.Handler) http.Handler { return next }

func (tasksHook) Run(ctx context.Context, a *app.App) error {
	go taskScheduler.Run(ctx)
	return nil
}

func rootFlag(fs *flag.FlagSet) *string {
	return fs.String("root", "", "installation root (default $MIDDLEWARE_ROOT or /srv/middleware)")
}

func tasksCmd(args []string) error {
	fs := flag.NewFlagSet("tasks", flag.ContinueOnError)
	root := rootFlag(fs)
	rest, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	r := settings.Root(*root)
	bus := events.New(filepath.Join(r, "data", "events"))
	s := tasks.NewScheduler(tasks.Options{Root: r, Bus: bus})
	if len(rest) == 0 || rest[0] == "list" {
		list := s.List()
		if len(list) == 0 {
			fmt.Println("no tasks (tasks/<name>.json)")
			return nil
		}
		fmt.Printf("%-24s %-18s %-24s %-20s %s\n", "NAME", "SCHEDULE", "SITE", "NEXT", "LAST")
		for _, t := range list {
			if t.Error != "" {
				fmt.Printf("%-24s %s\n", t.Name, "invalid: "+t.Error)
				continue
			}
			next := "-"
			if t.Disabled {
				next = "disabled"
			} else if !t.Next.IsZero() {
				next = t.Next.Format("2006-01-02 15:04")
			}
			last := "never"
			if t.Last != nil {
				last = fmt.Sprintf("%s %s (%s)", t.Last.At.Format("2006-01-02 15:04"), t.Last.Status, time.Duration(t.Last.Duration).Round(time.Second))
			}
			if t.Running {
				last = "running"
			}
			fmt.Printf("%-24s %-18s %-24s %-20s %s\n", t.Name, t.Schedule, t.Site, next, last)
		}
		return nil
	}
	if rest[0] == "run" {
		if len(rest) != 2 {
			return errors.New("usage: middleware tasks run <name>")
		}
		name := rest[1]
		if err := s.RunNow(name); err != nil {
			fmt.Fprintf(os.Stderr, "see %s\n", tasks.LogPath(r, name))
			return err
		}
		fmt.Printf("%s: ok (log: %s)\n", name, tasks.LogPath(r, name))
		return nil
	}
	return fmt.Errorf("unknown tasks command %q (list, run <name>)", rest[0])
}

// chatCmd runs one turn through the same manager the panel uses, so the
// conversation file, the cost ledger and the jail audit are the same ones,
// and prints what the agent says as it says it.
func chatCmd(args []string) error {
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	root := rootFlag(fs)
	model := fs.String("model", "", "model name or alias (default: the site's)")
	rest, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(rest) < 2 {
		return errors.New("usage: middleware chat <domain> <text>")
	}
	domain, text := rest[0], strings.Join(rest[1:], " ")
	r := settings.Root(*root)
	bus := events.New(filepath.Join(r, "data", "events"))
	st, _ := settings.Load(r)
	m := chat.New(chat.Options{Root: r, Bus: bus, Registry: providers(), Limits: st.Agents, Usage: usageBook(r)})
	ch, unsub := bus.Subscribe()
	defer unsub()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	turnID, err := m.Send(domain, text, *model)
	if err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			_ = m.Stop(domain)
			return errors.New("interrupted")
		case ev := <-ch:
			if ev.Subject != domain || ev.Data["turn_id"] != turnID {
				continue
			}
			switch ev.Type {
			case "agent.text":
				fmt.Println(ev.Data["text"])
			case "agent.tool":
				fmt.Fprintf(os.Stderr, "[%s]\n", ev.Data["tool"])
			case "agent.jail_violation":
				fmt.Fprintf(os.Stderr, "jail violation: %v\n", ev.Data["paths"])
			case "agent.done":
				if c, ok := ev.Data["cost"].(float64); ok && c > 0 {
					fmt.Fprintf(os.Stderr, "cost: $%.4f\n", c)
				}
				return nil
			case "agent.error":
				return errors.New(ev.Message)
			}
		}
	}
}
