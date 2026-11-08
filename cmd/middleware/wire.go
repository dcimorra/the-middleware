package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dcimorra/the-middleware/internal/acme"
	"github.com/dcimorra/the-middleware/internal/api"
	"github.com/dcimorra/the-middleware/internal/app"
	"github.com/dcimorra/the-middleware/internal/audit"
	"github.com/dcimorra/the-middleware/internal/auth"
	"github.com/dcimorra/the-middleware/internal/autofix"
	"github.com/dcimorra/the-middleware/internal/autopilot"
	"github.com/dcimorra/the-middleware/internal/backups"
	"github.com/dcimorra/the-middleware/internal/cgroups"
	"github.com/dcimorra/the-middleware/internal/chat"
	"github.com/dcimorra/the-middleware/internal/deps"
	"github.com/dcimorra/the-middleware/internal/domains"
	"github.com/dcimorra/the-middleware/internal/engine"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/firewall"
	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/helper"
	"github.com/dcimorra/the-middleware/internal/inbound"
	"github.com/dcimorra/the-middleware/internal/installer"
	"github.com/dcimorra/the-middleware/internal/logrotate"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/modules"
	"github.com/dcimorra/the-middleware/internal/modules/agents"
	"github.com/dcimorra/the-middleware/internal/modules/cloudflare"
	"github.com/dcimorra/the-middleware/internal/modules/github"
	"github.com/dcimorra/the-middleware/internal/modules/openspec"
	"github.com/dcimorra/the-middleware/internal/modules/vpn"
	"github.com/dcimorra/the-middleware/internal/nodeenv"
	"github.com/dcimorra/the-middleware/internal/nodes"
	"github.com/dcimorra/the-middleware/internal/pages"
	"github.com/dcimorra/the-middleware/internal/proxy"
	"github.com/dcimorra/the-middleware/internal/publish"
	"github.com/dcimorra/the-middleware/internal/push"
	"github.com/dcimorra/the-middleware/internal/relocate"
	"github.com/dcimorra/the-middleware/internal/repos"
	"github.com/dcimorra/the-middleware/internal/runtime"
	"github.com/dcimorra/the-middleware/internal/settings"
	"github.com/dcimorra/the-middleware/internal/shots"
	"github.com/dcimorra/the-middleware/internal/sitegroups"
	"github.com/dcimorra/the-middleware/internal/sites"
	"github.com/dcimorra/the-middleware/internal/sitetoken"
	"github.com/dcimorra/the-middleware/internal/update"
)

// The order of the hooks is the shape of the process, so it is written
// here once and not left to the order in which the files' inits ran.
//
// Request chain, outermost first: inbound (the ACME challenge and the
// doors, before anything else sees the request), waf (refuse the abusive
// before anything is spent on them), security (probe paths, robots,
// browsers and countries: two lists are cheaper than a password), attack
// (the proof of work: outside the password, so a flood never reaches a
// key derivation without paying first), access
// (the door the operator closed), traffic (count whatever came out, error pages included),
// cache, cluster (a clustered site's request handed to a replica on
// another node, after the cache so a kept page costs no hop), then the
// proxy. Everything after traffic wraps nothing and only runs: the
// api hangs from the proxy's panel slot, the chat, the tasks and the
// modules are services, the guard is a watchdog. The wire hook goes last
// because it reaches into every instance the others created.
const (
	orderInbound  = 10
	orderWAF      = 20
	orderSecurity = 25
	orderAttack   = 27
	orderAccess   = 30
	orderTraffic  = 40
	orderCache    = 45
	orderCluster  = 47
	orderAPI      = 50
	orderChat     = 60
	orderTasks    = 70
	orderModules  = 80
	orderGuard    = 90
	orderWire     = 100
)

func init() {
	hookBuilders = append(hookBuilders, hookBuilder{Name: "wire", Order: orderWire, Build: buildWire})
}

// The agent modules are kept so the wiring can re-apply them at start.
var (
	agentClaude *agents.Module
	agentAgy    *agents.Module
	agentOpen   *agents.OpenCode
	// agentVendors are the vendors' own modules (DeepSeek, Z.AI…), each
	// an agent of the chat, run by OpenCode.
	agentVendors []*agents.VendorModule
)

// theVPN is the VPN module, for the inbound hook's VPN door.
var theVPN *vpn.VPN

// registerExtraModules adds what buildModules cannot build on its own:
// the VPN, which needs the helper and the secrets box, and the two agent
// modules, which replace providers in the registry the chat uses. It runs
// in the service and in the CLI alike, so `middleware modules` lists them.
func registerExtraModules(mgr *modules.Manager, root string, bus *events.Bus, dm *deps.Manager) error {
	box, err := modules.OpenBoxSecrets(root)
	if err != nil {
		return err
	}
	v, err := vpn.New(vpn.Options{Root: root, Helper: helper.Dial(""), Secrets: box, Bus: bus,
		// In a network, closing this panel to all but the VPN closes every
		// node's, and opening it opens them.
		OnOnly: func(enabled bool) {
			if theMesh != nil && theMesh.InNetwork() {
				_ = theMesh.SetSettings(nodes.NetSettings{PanelVPNOnly: enabled})
			}
		},
		// The address the ports door publishes, when the operator set
		// one; otherwise found the same way the publisher finds it.
		PublicIP: func(ctx context.Context) (string, error) {
			if st, err := inbound.LoadState(root); err == nil && st.PublicIP != "" {
				return st.PublicIP, nil
			}
			return publish.DetectPublicIP(ctx)
		}})
	if err != nil {
		return fmt.Errorf("vpn: %w", err)
	}
	mgr.Register(modules.VPNModule(v))
	theVPN = v
	reg := providers()
	agentClaude = agents.Claude(reg)
	agentAgy = agents.Agy(reg, filepath.Join(root, "data", "agents", "agy"))
	agentOpen = agents.NewOpenCode(reg, filepath.Join(root, "data", "agents", "opencode"))
	agentVendors = nil
	for _, v := range agents.Vendors {
		agentVendors = append(agentVendors, agents.NewVendor(reg, filepath.Join(root, "data", "agents"), v))
	}
	for _, m := range []*agents.Module{agentClaude, agentAgy, agentOpen.Module} {
		m.Deps = dm
		m.Store = mgr
		m.Usage = usageBook(root)
	}
	mgr.Register(agentClaude)
	mgr.Register(agentAgy)
	mgr.Register(agentOpen)
	for _, v := range agentVendors {
		v.Deps, v.Store, v.Usage = dm, mgr, usageBook(root)
		mgr.Register(v)
	}
	return nil
}

// wireHook is the integration layer: it gives the API the routes of every
// other package and does the housekeeping that needs more than one of
// them. It wraps nothing.
type wireHook struct {
	set       *moduleSet
	publisher *publish.Publisher
	shots     *shots.Shotter
	repos     *repos.Manager
	notifier  *push.Notifier
	auditor   *audit.Auditor
	fixer     *autofix.Fixer
	pilot     *autopilot.Pilot
	updater   *update.Updater
	mesh      *nodes.Mesh
	fw        *firewall.Firewall
	backups   *backups.Keeper
}

func buildWire(a *app.App) app.Hook {
	if apiHook == nil {
		a.Bus.Emit(events.Event{Type: "wire.error", Actor: "engine", Level: "error", Message: "the api hook was not built; nothing else is reachable"})
		return nil
	}
	srv := apiHook.Server
	if chatManager != nil {
		// The socket confines a site's agent turn to its site: the chat
		// manager knows which processes are turns right now.
		apiHook.SiteOf = chatManager.SiteOf
	}
	w := &wireHook{}
	// A site whose command wants Node gets one: the version it asks for,
	// installed with nvm under modules/nvm when the machine has none or
	// another, and something that runs when that cannot be had. Set here,
	// before the engine's first pass starts anything.
	nodes := &nodeenv.Manager{Root: a.Root, Note: func(siteDir, msg string) {
		a.Bus.Emit(events.Event{Type: "node.chosen", Subject: filepath.Base(siteDir), Actor: "engine", Message: msg})
	}}
	wireJails(a)
	runtime.NodeDir = func(ctx context.Context, siteDir string, argv []string) string {
		if !nodeenv.Needs(siteDir, argv) {
			return ""
		}
		return nodes.For(ctx, siteDir)
	}
	set, err := modulesFor(a)
	if err != nil {
		// modulesHook.Run fails the start with the same error; the
		// routes below are registered anyway so a partial process is at
		// least an honest one.
		a.Bus.Emit(events.Event{Type: "modules.error", Actor: "engine", Level: "error", Message: err.Error()})
	} else {
		w.set = set
		srv.RegisterModules(api.ModulesOptions{Manager: set.Manager, Public: []string{"POST " + github.Key + "/hook"}})
		if err := agents.Restore(context.Background(), set.Manager, restorable()...); err != nil {
			a.Bus.Emit(events.Event{Type: "module.restore_failed", Actor: "engine", Level: "error", Message: err.Error()})
		}
		// One deployment runner for the installation: the sites' own,
		// shared with the GitHub module, so two deploys of one site never
		// overlap whichever button or push started them.
		if mgr := srv.Sites(); mgr != nil && mgr.Deploys != nil && set.GitHub != nil {
			set.GitHub.UseRunner(mgr.Deploys)
		}
		srv.SetDeployer(githubDeployer(a, set))
		if set.GitHub != nil {
			srv.SetRepoSource(githubRepos{set: set})
		}
		srv.RegisterDeps(api.DepsOptions{Manager: set.Deps})
		// The CLIs a turn runs come first in its PATH, with their Node.
		if chatManager != nil {
			chatManager.SetPath(set.Deps.Path)
		}
		go set.Deps.Run(context.Background())
		initSpecs := specsInit(a, set)
		srv.RegisterSpecs(api.SpecsOptions{Enabled: func() bool { return set.Manager.Enabled(openspec.Key) }, Init: initSpecs})
		go specsOnCreate(a, set, initSpecs)
	}
	if acmeIssuer != nil {
		acmeIssuer.SetDNS01(dns01For(cloudflareDNS{set: set}))
	}
	if inboundHook != nil {
		w.publisher = buildPublisher(a, set)
	}
	if set != nil {
		wirePreviews(a, set.GitHub, srv.Sites(), w.publisher)
	}
	if inboundHook != nil {
		srv.RegisterInbound(api.InboundOptions{Hook: inboundHook, DNS: cloudflareDNS{set: set}, Publisher: w.publisher})
	}
	srv.RegisterWAF(api.WAFOptions{WAF: theWAF, Traffic: theTraffic, Attack: theAttack})
	srv.RegisterSecurity(api.SecurityOptions{Door: theSecurity, Country: edgeCountry})
	// Whichever of this and the inbound hook is built first makes it.
	if a.Proxy.Unrouted == nil {
		a.Proxy.Unrouted = proxy.NewUnrouted()
	}
	srv.RegisterUnrouted(a.Proxy.Unrouted)
	if theVitals != nil {
		srv.RegisterVitals(theVitals)
	}
	if theSeen != nil {
		srv.RegisterSeen(theSeen)
	}
	if theVisits != nil {
		srv.RegisterVisits(theVisits)
	}
	if theWebVitals != nil {
		srv.RegisterWebVitals(theWebVitals)
	}
	if theStatus != nil {
		srv.RegisterStatus(theStatus)
	}
	// An automatic update waits while a site moves between nodes: the
	// restart that ends it would cut the move.
	w.updater = buildUpdater(a, srv.MovesRunning)
	srv.RegisterUpdate(w.updater)
	w.mesh = buildMesh(a, srv)
	if w.mesh != nil {
		srv.RegisterNodes(w.mesh, func() error {
			if theVPN == nil || !theVPN.Ready() {
				return errors.New("this node needs its VPN on, with a device, before every panel is closed to everything else: it is the way in that is known to work")
			}
			return nil
		})
		w.mesh.Serve(srv)
		if theCluster != nil {
			theCluster.SetMesh(w.mesh)
			// The closed door: what a primary hands a replica here goes
			// straight to the router, past this node's own chain (the
			// primary's already ran on it).
			w.mesh.ServeSites(theCluster.ReplicaHandler(a.Proxy))
			w.mesh.ServeTunnels(theCluster.TunnelHandler())
		}
	}
	srv.RegisterCluster(theCluster)
	w.shots = shots.New(shots.Options{
		Root:    a.Root,
		DataDir: dataDir(a),
		Bus:     a.Bus,
		Bare:    a.Proxy,
		Shootable: func(domain string) bool {
			sn, ok := a.Engine.Lookup(domain)
			if !ok || sn.Domain != domain || sn.Health == engine.HealthDown {
				return false
			}
			return sn.Mode == manifest.ModeStatic || sn.Mode == manifest.ModeUpstream || sn.Mode == manifest.ModeRun && sn.Alive
		},
		Domains: func() []string {
			var out []string
			for _, sn := range a.Engine.Sites() {
				if !manifest.IsProject(sn.Domain) {
					out = append(out, sn.Domain)
				}
			}
			return out
		},
	})
	srv.RegisterShots(w.shots)
	pages.SetHouseTheme(a.Settings.VisitorTheme)
	srv.OnSettingsSaved(func(st settings.Settings) { pages.SetHouseTheme(st.VisitorTheme) })
	srv.RegisterAgents(api.AgentsOptions{Registry: providers(), Chat: chatManager, Usage: func(ctx context.Context, refresh bool, provider string) any {
		mods := []*agents.Module{agentClaude, agentAgy}
		for _, v := range agentVendors {
			mods = append(mods, v.Module)
		}
		return agents.UsageOf(ctx, refresh, provider, mods...)
	}})
	if chatManager != nil {
		wireSiteDoor(a, srv, set)
	}
	srv.RegisterContainerize(api.ContainOptions{Chat: chatManager})
	srv.RegisterRepair(api.RepairOptions{Chat: chatManager})
	srv.RegisterDoctor(func() ([]installer.Check, bool) {
		p, err := installParams(flag.NewFlagSet("doctor", flag.ContinueOnError), []string{"--root", a.Root})
		if err != nil {
			return []installer.Check{{Name: "parameters", Detail: err.Error()}}, false
		}
		in := installer.New(p)
		in.Out = io.Discard
		return in.Doctor()
	})
	srv.RegisterSiteResources(cgroups.ThisMachine(), func() bool { return sitegroups.Live.Load() != nil }, func(domain string) (cgroups.SiteMemory, bool) {
		if t := sitegroups.Live.Load(); t != nil {
			return t.Memory(domain)
		}
		return cgroups.SiteMemory{}, false
	})
	srv.RegisterRequirements(helper.InstallLog(helper.DefaultRunDir), func(ctx context.Context, browser bool) error {
		_, err := helper.Dial("").InstallRun(ctx, browser)
		return err
	})
	if taskScheduler != nil {
		srv.RegisterTasks(api.TasksOptions{Scheduler: taskScheduler, Root: a.Root})
	}
	srv.RegisterGuard(api.GuardOptions{Guard: theGuard})
	// The sites' database copies (the manifests' backups).
	w.backups = backups.New(backups.Options{
		Root: a.Root, Bus: a.Bus,
		ServiceSpec: func(domain, service string) (*manifest.Spec, error) {
			return a.Engine.ServiceSpec(domain, service)
		},
		Restart: a.Engine.Restart,
	})
	srv.RegisterBackups(w.backups)
	// The repositories inside the sites, and the watcher that keeps the
	// marked ones pulled (internal/repos).
	rm := repos.New(repos.Options{DataDir: dataDir(a), Bus: a.Bus, Sites: func() []repos.Site {
		var out []repos.Site
		for _, sn := range a.Engine.Sites() {
			if sn.Manifest != nil {
				out = append(out, repos.Site{Domain: sn.Domain, Dir: sn.Dir, Repositories: sn.Manifest.GitRepositories()})
			}
		}
		return out
	}, Token: func() string {
		// The GitHub module's token, while it is enabled: the same one the
		// git proxy pushes a conversation's work with.
		if set := w.set; set != nil && set.GitHub != nil {
			return set.GitHub.PushToken()
		}
		return ""
	}})
	srv.RegisterRepos(rm)
	w.repos = rm
	// Web push to the operator's devices, and the notifier that decides
	// which events become one (internal/push).
	subject := "mailto:operator@localhost"
	if a.Settings.PanelDomain != "" {
		subject = "https://" + a.Settings.PanelDomain
	}
	ps, err := push.New(push.Options{DataDir: dataDir(a), Subject: subject})
	if err != nil {
		a.Bus.Emit(events.Event{Type: "push.error", Actor: "engine", Level: "error", Message: "push notifications are off: " + err.Error()})
		ps = nil
	}
	srv.RegisterPush(ps)
	// npm audit over each site once a day: critical and new is an event
	// (and a push); never a fix (internal/audit).
	auditSites := func() []audit.Site {
		var out []audit.Site
		for _, sn := range a.Engine.Sites() {
			if sn.Manifest != nil {
				out = append(out, audit.Site{Domain: sn.Domain, Dir: sn.Dir})
			}
		}
		return out
	}
	w.auditor = audit.New(audit.Options{DataDir: dataDir(a), Bus: a.Bus, Sites: auditSites, NPM: func() string {
		// The house's own Node first (modules/node, kept up to date by
		// deps), then whatever npm the machine has.
		if w.set != nil && w.set.Deps != nil {
			if p := filepath.Join(w.set.Deps.Prefix(), "bin", "npm"); fsx.IsFile(p) {
				return p
			}
		}
		p, _ := exec.LookPath("npm")
		return p
	}})
	// Automatic repair of a site that went down (internal/autofix): off
	// unless Settings turns it on, with its brakes.
	if chatManager != nil {
		sm := srv.Sites()
		w.fixer = autofix.New(autofix.Options{
			DataDir: dataDir(a),
			Bus:     a.Bus,
			Settings: func() settings.Autofix {
				s, _ := settings.Load(a.Root)
				return s.Autofix
			},
			Targets: func() []autofix.Target {
				var out []autofix.Target
				for _, sn := range a.Engine.Sites() {
					if sn.Manifest == nil {
						continue
					}
					t := autofix.Target{Domain: sn.Domain, Mode: string(sn.Mode), Failures: sn.Failures, Error: sn.Error,
						OptOut: sn.Manifest.Autofix != nil && !*sn.Manifest.Autofix}
					for _, c := range sn.Checks {
						t.Checks = append(t.Checks, c.OK)
					}
					if sn.Manifest.Run != nil {
						t.Command = strings.Join(sn.Manifest.Run.Command, " ")
					}
					t.Arrived = relocate.ArrivedAt(a.Root, sn.Domain)
					out = append(out, t)
				}
				return out
			},
			LogTail: func(domain string, n int) string {
				if sm == nil {
					return ""
				}
				s, _ := sm.Logs(domain, "", n)
				return s
			},
			// The house's AI for automatic work when there is one; the
			// site's own agent otherwise, as before it existed. In the
			// site's repair conversation, not the operator's: the prompt
			// quotes the site's log (chat.RepairConversation).
			Send: func(domain, prompt string) (string, error) {
				s, _ := settings.Load(a.Root)
				name, err := chatManager.RepairConversation(domain)
				if err != nil {
					return "", err
				}
				return chatManager.SendWith(name, prompt, chat.SendOptions{Provider: s.Automation.Provider, Model: s.Automation.Model})
			},
			Automatic: func() bool {
				s, _ := settings.Load(a.Root)
				return s.Automation.On()
			},
			Restart: func(domain string) error {
				if sn, ok := a.Engine.Lookup(domain); ok && sn.Mode != "run" {
					return nil // a static site has nothing to restart
				}
				return a.Engine.Restart(context.Background(), domain)
			},
		})
		srv.Handle("GET", api.Prefix+"/autofix", "The automatic repairs, oldest first (data/repairs.jsonl)", func(_ http.ResponseWriter, _ *http.Request) (any, error) {
			return w.fixer.Repairs(), nil
		})
	}
	// The autonomous agent (internal/autopilot): off until the operator
	// switches it on, and held to its brakes here and in the API.
	if chatManager != nil {
		w.pilot = buildPilot(a, w)
		srv.RegisterAutopilot(w.pilot, chatManager)
	}
	srv.RegisterDomains(domains.New(domains.Options{DataDir: dataDir(a)}))
	if theCache != nil {
		srv.RegisterCache(theCache)
		srv.OnSettingsSaved(func(s settings.Settings) { theCache.SetBudget(s.Cache.Bytes()) })
	}
	srv.RegisterAudit(w.auditor, func(domain string) (audit.Site, bool) {
		sn, ok := a.Engine.Lookup(domain)
		return audit.Site{Domain: sn.Domain, Dir: sn.Dir}, ok && sn.Manifest != nil
	})
	if ps != nil {
		w.notifier = &push.Notifier{Bus: a.Bus, Sender: ps, OnError: func(err error) {
			a.Bus.Emit(events.Event{Type: "push.failed", Actor: "engine", Level: "warn", Message: err.Error()})
		}}
	}
	// A block at layer 7 is pushed to the kernel through the helper, so
	// a flood stops before it reaches Go. Without the helper the firewall
	// says so once and the block stays at layer 7.
	fw := firewall.New(helper.Dial(""), a.Bus)
	if theWAF != nil && srv != nil {
		// The operator's own session on the panel is the panel's traffic,
		// not a visitor's: not rate-limited (waf.SetOperator).
		store := srv.Auth()
		theWAF.SetOperator(func(r *http.Request) bool {
			panel := a.Proxy.PanelDomain()
			host := strings.ToLower(strings.Split(r.Host, ":")[0])
			if panel == "" || host != panel || store == nil {
				return false
			}
			c, err := r.Cookie(auth.CookieName)
			return err == nil && store.OperatorSession(c.Value)
		})
	}
	if theWAF != nil {
		theWAF.OnBlock = func(ip net.IP, d time.Duration) { _ = fw.OnBlock(ip.String(), d) }
		theWAF.OnUnblock = func(ip net.IP) { _ = fw.Unblock(ip.String()) }
		w.fw = fw
		// The panel's logins feed the bans: a wrong password is a strike,
		// a right one makes its address trusted for a week, so the
		// operator's typos never lock them out.
		go func() {
			ch, stop := a.Bus.Subscribe()
			defer stop()
			for e := range ch {
				if e.Type != "auth.login_failed" && e.Type != "auth.login" {
					continue
				}
				ipStr, _ := e.Data["ip"].(string)
				ip := net.ParseIP(ipStr)
				if ip == nil {
					continue
				}
				if e.Type == "auth.login" {
					theWAF.Trust(ip, 7*24*time.Hour)
				} else {
					theWAF.Strike(ip, 1, "panel login failed", a.Proxy.PanelDomain(), "/login")
				}
			}
		}()
	}
	return w
}

func (w *wireHook) Wrap(next http.Handler) http.Handler { return next }

// Run reserves the site port range and says the process is ready. Neither
// can fail the start: an installation without the helper runs with the
// kernel's ephemeral range overlapping ours, which is a warning, not an
// outage.
func (w *wireHook) Run(ctx context.Context, a *app.App) error {
	hc := helper.Dial("")
	st := a.CurrentSettings()
	from, to := st.Ports.From, st.Ports.To
	if hc.Available() {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := hc.ReservePorts(rctx, from, to)
		cancel()
		if err != nil {
			a.Bus.Emit(events.Event{Type: "ports.reserve_failed", Actor: "engine", Level: "warn", Message: err.Error(), Data: map[string]any{"from": from, "to": to}})
		} else {
			a.Bus.Emit(events.Event{Type: "ports.reserved", Actor: "engine", Data: map[string]any{"from": from, "to": to}})
		}
	} else {
		a.Bus.Emit(events.Event{Type: "ports.not_reserved", Actor: "engine", Level: "warn", Message: "the helper is not reachable on " + hc.Path() + "; the site port range is not reserved from the kernel's ephemeral range"})
	}
	if w.publisher != nil {
		go w.publisher.Run(ctx)
	}
	// The kernel forgets its blocks on a reboot; the list does not. What
	// is still blocked goes back into the kernel at start.
	if w.fw != nil && theWAF != nil && hc.Available() {
		go func() {
			for _, e := range theWAF.Blocked() {
				if left := time.Until(e.Until); left > time.Second {
					_ = w.fw.Block(e.IP, left, e.Reason)
				}
			}
		}()
	}
	if w.mesh != nil {
		go w.mesh.Run(ctx)
	}
	if w.shots != nil {
		go w.shots.Run(ctx)
	}
	if w.repos != nil {
		go w.repos.Run(ctx)
	}
	if w.notifier != nil {
		go w.notifier.Run(ctx)
	}
	if w.auditor != nil {
		go w.auditor.Run(ctx)
	}
	if w.fixer != nil {
		go w.fixer.Run(ctx)
	}
	if w.pilot != nil {
		go w.pilot.Run(ctx)
	}
	if w.backups != nil {
		go w.backups.Run(ctx)
	}
	if w.updater != nil {
		// Here, before app.Run opens the listener, so the confirmation
		// is waiting for it to be open and cannot miss it.
		w.updater.ConfirmWhenServing(ctx)
		go w.updater.Run(ctx)
	}
	// The VPN's interface, held up while the module is enabled (the CLI
	// builds the module too, but only the service keeps it).
	if theVPN != nil {
		go theVPN.Keep(ctx, 30*time.Second)
	}
	// The logs sites and tasks write, rotated at 10 MB with three kept.
	go logrotate.Run(ctx, []string{filepath.Join(dataDir(a), "logs", "*", "*.log")}, func(path string, err error) {
		if err != nil {
			a.Bus.Emit(events.Event{Type: "logs.rotate_failed", Actor: "engine", Level: "warn", Message: path + ": " + err.Error()})
			return
		}
		a.Bus.Emit(events.Event{Type: "logs.rotated", Actor: "engine", Message: path})
	})
	var enabled []string
	if w.set != nil {
		for _, info := range w.set.Manager.List() {
			if info.Enabled {
				enabled = append(enabled, info.Key)
			}
		}
	}
	var doors []string
	if inboundHook != nil {
		st := inboundHook.Status().Doors
		if st.Tunnel.Open {
			doors = append(doors, "tunnel")
		}
		if st.Ports.Open {
			doors = append(doors, "ports")
		}
	}
	if enabled == nil {
		enabled = []string{}
	}
	if doors == nil {
		doors = []string{}
	}
	a.Bus.Emit(events.Event{Type: "middleware.ready", Actor: "engine", Message: fmt.Sprintf("modules: %s; doors: %s", orNone(enabled), orNone(doors)), Data: map[string]any{"modules": enabled, "doors": doors, "version": Version}})
	return nil
}

func orNone(s []string) string {
	if len(s) == 0 {
		return "none"
	}
	return strings.Join(s, ", ")
}

// githubDeployer sends POST /sites/{domain}/deploy through the github
// module's runner when the module is enabled and the manifest has a
// deploy.github section: that path fetches the repository before it
// builds, which the manager's in-place build does not. Anything else is
// left to the manager.
func githubDeployer(a *app.App, set *moduleSet) api.Deployer {
	return func(domain string) (any, bool, error) {
		if set == nil || set.GitHub == nil || !set.Manager.Enabled(github.Key) {
			return nil, false, nil
		}
		dir := filepath.Join(a.Root, "sites", domain)
		mf, err := manifest.Load(dir)
		if err != nil || mf.Deploy == nil || mf.Deploy.GitHub == nil {
			return nil, false, nil
		}
		set.GitHub.Runner().Trigger(domain, dir, mf, "operator")
		return map[string]any{"ok": true, "via": github.Key}, true, nil
	}
}

// cloudflareDNS is the cloudflare module as the inbound section sees it.
type cloudflareDNS struct {
	set *moduleSet
}

func (d cloudflareDNS) Enabled() bool {
	return d.set != nil && d.set.Cloudflare != nil && d.set.Manager.Enabled(cloudflare.Key)
}

func (d cloudflareDNS) client() (*cloudflare.Client, error) {
	if !d.Enabled() {
		return nil, errors.New("cloudflare: not enabled")
	}
	return d.set.Cloudflare.Client()
}

// Describe lists the zones once, reads each zone that holds one of the
// domains once, and the tunnel's ingress once.
func (d cloudflareDNS) Describe(ctx context.Context, domains []string) (map[string]api.DNSInfo, error) {
	c, err := d.client()
	if err != nil {
		return nil, err
	}
	zones, err := c.Zones(ctx)
	if err != nil {
		return nil, err
	}
	byZone := map[string][]string{}
	for _, dom := range domains {
		z, err := cloudflare.MatchZone(zones, dom)
		if err != nil {
			continue
		}
		byZone[z.ID] = append(byZone[z.ID], strings.ToLower(dom))
	}
	out := map[string]api.DNSInfo{}
	for zoneID, names := range byZone {
		recs, err := c.DNSRecords(ctx, zoneID)
		if err != nil {
			return out, err
		}
		for _, name := range names {
			info := api.DNSInfo{}
			for _, r := range recs {
				if strings.EqualFold(r.Name, name) && (r.Type == "A" || r.Type == "AAAA" || r.Type == "CNAME") {
					info.DNS = r.Type + " " + r.Content
					break
				}
			}
			out[name] = info
		}
	}
	if c.TunnelID != "" {
		rules, err := c.TunnelIngress(ctx, "")
		if err != nil {
			return out, err
		}
		for _, r := range rules {
			name := strings.ToLower(r.Hostname)
			if info, ok := out[name]; ok {
				info.TunnelRoute = true
				out[name] = info
			}
		}
	}
	return out, nil
}

func (d cloudflareDNS) Publish(ctx context.Context, domain, door, ip string) error {
	c, err := d.client()
	if err != nil {
		return err
	}
	return c.Publish(ctx, domain, door, ip)
}

func (d cloudflareDNS) Unpublish(ctx context.Context, domain, ip string) error {
	c, err := d.client()
	if err != nil {
		return err
	}
	return c.Unpublish(ctx, domain, ip)
}

func (d cloudflareDNS) Foreign(ctx context.Context, domain string, ips ...string) (string, error) {
	c, err := d.client()
	if err != nil {
		return "", err
	}
	return c.Foreign(ctx, domain, ips...)
}

func (d cloudflareDNS) Covers(ctx context.Context, domain string) (bool, error) {
	c, err := d.client()
	if err != nil {
		return false, err
	}
	return c.Covers(ctx, domain)
}

var (
	_ api.DNSProvider   = cloudflareDNS{}
	_ publish.DNS       = cloudflareDNS{}
	_ publish.Inspector = cloudflareDNS{}
)

// dns01For is the ACME dns-01 solver over the cloudflare module, asked
// each time whether the module is on and which zones it sees, so enabling
// it while the process runs is enough. The zone list is cached for a few
// minutes: Covers is asked once per name per order.
func dns01For(d cloudflareDNS) *acme.DNS01 {
	var mu sync.Mutex
	var zones []cloudflare.Zone
	var fetched time.Time
	return &acme.DNS01{
		Covers: func(domain string) bool {
			c, err := d.client()
			if err != nil {
				return false
			}
			mu.Lock()
			defer mu.Unlock()
			if time.Since(fetched) > 5*time.Minute {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				z, err := c.Zones(ctx)
				cancel()
				if err != nil {
					return false
				}
				zones, fetched = z, time.Now()
			}
			_, err = cloudflare.MatchZone(zones, domain)
			return err == nil
		},
		Set: func(ctx context.Context, fqdn, value string) (func(context.Context) error, error) {
			c, err := d.client()
			if err != nil {
				return nil, err
			}
			return c.SetTXT(ctx, fqdn, value)
		},
	}
}

// buildUpdater is the self-update: the helper installs, the supervisor
// restarts itself as the Restart button does. busy defers an automatic
// install (update.Options.Busy).
func buildUpdater(a *app.App, busy func() bool) *update.Updater {
	hc := helper.Dial("")
	return update.New(update.Options{
		Current: Version,
		Bus:     a.Bus,
		Auto: func() bool {
			s, err := settings.Load(a.Root)
			return err == nil && s.Update.Auto
		},
		Every: func() time.Duration {
			s, err := settings.Load(a.Root)
			if err != nil {
				return 0
			}
			return s.Update.Every()
		},
		Helper: hc.Available,
		Install: func(ctx context.Context, version string) error {
			_, err := hc.UpdateInstall(ctx, version)
			return err
		},
		Restart: func() { _ = syscall.Kill(os.Getpid(), syscall.SIGTERM) },
		Busy:    busy,
		Confirm: func(ctx context.Context, version string) (update.Confirmation, error) {
			r, err := hc.UpdateConfirm(ctx, version)
			c := update.Confirmation{Confirmed: r.Confirmed, Skipped: r.Skipped}
			if r.Rollback != nil {
				c.RolledBack, c.Why = r.Rollback.From, r.Rollback.Why
			}
			return c, err
		},
	})
}

// buildPublisher is the one place names are published. Its DNS is the
// cloudflare module while it is enabled, and nothing otherwise: names are
// then recorded as manual, with the reason, rather than silently skipped.
func buildPublisher(a *app.App, set *moduleSet) *publish.Publisher {
	if inboundHook == nil {
		return nil
	}
	dns := cloudflareDNS{set: set}
	p, err := publish.New(publish.Options{
		Root:  a.Root,
		Bus:   a.Bus,
		Doors: inboundHook,
		DNS: func() publish.DNS {
			if !dns.Enabled() {
				return nil
			}
			return dns
		},
		// The panel's name is published like a site's, so the operator
		// chooses its door (tunnel or 80/443) and neither is required.
		Hosts: func() map[string]string {
			hosts := a.Engine.Hosts()
			if d := a.Proxy.PanelDomain(); d != "" {
				if _, taken := hosts[d]; !taken {
					hosts[d] = publish.PanelOwner
				}
			}
			return hosts
		},
		VPNOnly: func(site string) bool {
			sn, ok := a.Engine.Lookup(site)
			// Only a site wholly inside the VPN is never published; one
			// with some paths in it is public everywhere else.
			return ok && sn.Manifest != nil && sn.Manifest.VPN.Whole()
		},
	})
	if err != nil {
		a.Bus.Emit(events.Event{Type: "publish.error", Actor: "engine", Level: "error", Message: "data/publish.json does not read, nothing is published: " + err.Error()})
		return nil
	}
	return p
}

// wireSiteDoor opens the site door: tokens derived from data/site-door.key,
// put in the environment of every site whose manifest has claude_api, and
// a socket of its own for sites in containers, where 127.0.0.1 is the
// container's and the loopback listener cannot be reached. The socket's
// folder is mounted, not the file: the socket is recreated at every start,
// and a container holding the file would keep the dead inode. Through that
// socket only the site door answers, never the panel: what calls it is
// what answers requests from the internet.
func wireSiteDoor(a *app.App, srv *api.Server, set *moduleSet) {
	tokens := sitetoken.New(dataDir(a))
	runDir := filepath.Join(dataDir(a), "run")
	sock := filepath.Join(runDir, "site.sock")
	// Where the loopback listener really is (MIDDLEWARE_LISTEN wins over
	// the settings), as an address a process can dial: the settings said
	// one port while a test instance listened on another, and the site's
	// calls went to whatever answered there.
	listen := app.ListenAddr(a.Settings.Listen)
	if host, port, err := net.SplitHostPort(listen); err == nil && (host == "" || host == "0.0.0.0" || host == "::") {
		listen = net.JoinHostPort("127.0.0.1", port)
	}
	a.Engine.SetSiteEnv(func(domain string, m *manifest.Manifest) (map[string]string, []string) {
		env := map[string]string{}
		var mounts []string
		if door := doorEnv(tokens, listen, domain, m); door != nil {
			for k, v := range door {
				env[k] = v
			}
			if m.Run != nil && m.Run.Runtime == "docker" {
				env["MIDDLEWARE_API"] = "http://localhost"
				env["MIDDLEWARE_API_SOCKET"] = sock
				mounts = append(mounts, runDir)
			}
		}
		// A copy of another node's site calls the site door through the
		// primary, with the primary's token and spending.
		if theCluster != nil {
			for k, v := range theCluster.DoorEnv(domain, m) {
				env[k] = v
			}
		}
		if m.Claude && set != nil {
			// The house's subscription token, for a site that runs the CLI
			// itself: it reaches as far as the subscription does.
			if cfg, err := set.Manager.Config(agents.ClaudeKey); err == nil && cfg["oauth_token"] != "" {
				env["CLAUDE_CODE_OAUTH_TOKEN"] = cfg["oauth_token"]
			}
		}
		return env, mounts
	})
	// A site's tasks get the same door as its process, with the same
	// token: the door reads the scope, the models and daily_usd from the
	// manifest at every call and counts the spend by domain, so a task
	// spends from the site's day and loses the door with the block. A
	// task is always a plain process on this machine, never in the
	// site's container, so the loopback listener is the address.
	if taskScheduler != nil {
		taskScheduler.SetSiteEnv(func(site string, m *manifest.Manifest) map[string]string {
			return doorEnv(tokens, listen, site, m)
		})
	}
	srv.RegisterSiteDoor(api.SiteDoorOptions{
		Chat:   chatManager,
		Tokens: tokens,
		Bus:    a.Bus,
		SiteDir: func(domain string) string {
			if sn, ok := a.Engine.Lookup(domain); ok {
				return sn.Dir
			}
			return filepath.Join(a.Root, "sites", domain)
		},
		API: "http://" + listen,
		// Claude's door has always started on the Claude module's model;
		// the other agents start on their own default.
		DefaultModel: func(provider string) string {
			if set == nil || provider != "claude" {
				return ""
			}
			cfg, _ := set.Manager.Config(agents.ClaudeKey)
			return cfg["model"]
		},
		Socket: sock,
		Domains: func() []string {
			var out []string
			for _, sn := range a.Engine.Sites() {
				out = append(out, sn.Domain)
			}
			return out
		},
		Models: func(provider string) []string {
			_, models := chatManager.ProviderInfo(provider)
			return doorModels(provider, models)
		},
		Lookup: func(domain string) (*manifest.Manifest, bool) {
			sn, ok := a.Engine.Lookup(domain)
			if !ok || sn.Domain != domain {
				return nil, false
			}
			return sn.Manifest, sn.Manifest != nil
		},
		HouseDaily: func() float64 { return a.CurrentSettings().Agents.SiteDaily() },
		Spent:      chatManager.Ledger().Day,
	})
	wireGitProxy(a, srv, set, tokens, "http://"+listen)
	ln, err := api.ListenSocket(sock)
	if err != nil {
		a.Bus.Emit(events.Event{Type: "sitedoor.socket_failed", Actor: "api", Level: "error", Message: err.Error()})
		return
	}
	_ = os.Chmod(sock, 0o666)
	h := srv.Panel()
	if theCluster != nil {
		// A copy of one of this node's sites calls its door through here.
		theCluster.SetDoor(func(site string) http.Handler { return doorForCopies(tokens, site, h) })
	}
	door := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := api.SiteDoorPath(r.URL.Path); !ok {
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})
	go func() { _ = (&http.Server{Handler: door, ReadHeaderTimeout: 15 * time.Second}).Serve(ln) }()
}

// doorForCopies is the primary's end of a copy's door: only the door's
// routes, with this site's token on them whatever the copy sent, so a
// copy reaches no other site's door and nothing of the panel.
func doorForCopies(tokens *sitetoken.Keys, site string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, ok := api.SiteDoorPath(r.URL.Path)
		tok, err := tokens.Token(site)
		if !ok || err != nil {
			http.NotFound(w, r)
			return
		}
		r.Header.Del("X-Middleware-Site")
		r.Header.Set("Authorization", "Bearer "+tok)
		// The call is a site process's, as direct as one on this
		// machine; it arrives from the other node's mesh address, which
		// the panel's address lists would refuse, as they would have
		// refused the site door before it was let past them on loopback.
		r.RemoteAddr = "127.0.0.1:0"
		h.ServeHTTP(w, r)
	})
}

// doorEnv is what a process of a site with claude_api needs to call the
// site door: its token and the bare address, as in V1, since the code
// calls $MIDDLEWARE_API/api/site/… (or /api/v1/site/…). Nil for a site
// without the block.
func doorEnv(tokens *sitetoken.Keys, listen, domain string, m *manifest.Manifest) map[string]string {
	if m == nil || m.ClaudeAPI == nil {
		return nil
	}
	tok, err := tokens.Token(domain)
	if err != nil {
		return nil
	}
	return map[string]string{"MIDDLEWARE_TOKEN": tok, "MIDDLEWARE_API": "http://" + listen}
}

// wireGitProxy lets a site's conversation push to the repository its
// manifest deploys from, with the GitHub module's token, which the turn
// never sees: the turn gets a push token and the git settings that send
// that repository through the house (api.GitProxyOptions says why it is
// not a credential helper). The repository is the one in the manifest the
// engine accepted, read per request, so changing deploy.github moves it.
func wireGitProxy(a *app.App, srv *api.Server, set *moduleSet, tokens *sitetoken.Keys, listen string) {
	if set == nil || set.GitHub == nil {
		return
	}
	repositories := func(domain string) []string {
		sn, ok := a.Engine.Lookup(domain)
		if !ok || sn.Domain != domain || sn.Manifest == nil {
			return nil
		}
		return sn.Manifest.GitRepositoryNames()
	}
	srv.RegisterGitProxy(api.GitProxyOptions{
		Tokens:       tokens,
		Repositories: repositories,
		Token:        set.GitHub.PushToken,
		Bus:          a.Bus,
	})
	chatManager.SetTurnEnv(func(domain string) map[string]string {
		repos := repositories(domain)
		if len(repos) == 0 || set.GitHub.PushToken() == "" {
			return nil
		}
		tok, err := tokens.GitToken(domain)
		if err != nil {
			return nil
		}
		return api.GitEnv(listen, repos, tok)
	})
}

// buildMesh opens the node's membership in the mesh. The links of the
// first design (alpha.14: two tokens over the public panel) are undone
// here once: their tokens revoked and their file removed, so nothing keeps
// opening this machine to a node that is now expected to come through the
// tunnel.
// theMesh is the node's network, for the hooks built before it (the
// inbound's panel rule, the VPN's switch).
var theMesh *nodes.Mesh

func buildMesh(a *app.App, srv *api.Server) *nodes.Mesh {
	host, _ := os.Hostname()
	root := a.Root
	var network nodes.Network = nodes.HelperNetwork{Client: helper.Dial("")}
	var subnet netip.Prefix
	// For trying several installations on one machine without root: no
	// tunnel, addresses on loopback. Never on a real network.
	if os.Getenv("MIDDLEWARE_MESH_LOOPBACK") == "1" {
		network, subnet = nodes.LoopbackNetwork{}, netip.MustParsePrefix("127.77.0.0/16")
	}
	var m *nodes.Mesh
	m, err := nodes.Open(nodes.Options{
		// What this node answers for, for the other nodes' VPN doors: its
		// sites, their other names and its panel.
		Names: func() []string {
			var out []string
			for h := range a.Engine.Hosts() {
				out = append(out, h)
			}
			if d := a.Proxy.PanelDomain(); d != "" {
				out = append(out, d)
			}
			return out
		},
		OnRelay: func(c net.Conn, device string) {
			if inboundHook == nil {
				_ = c.Close()
				return
			}
			inboundHook.AcceptRelayed(c, device)
		},
		// The network's "panel only through a VPN" is also this node's
		// VPN rule when it has a VPN to close it to, so the kernel's wall
		// stands behind the house's here too.
		OnSettings: func(st nodes.NetSettings) {
			if m == nil || !m.InNetwork() || theVPN == nil || !theVPN.Ready() || theVPN.PanelOnly() == st.PanelVPNOnly {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = theVPN.SetOnly(ctx, st.PanelVPNOnly)
		},
		Dir:      filepath.Join(dataDir(a), "nodes"),
		Hostname: host,
		Version:  Version,
		Net:      network,
		Subnet:   subnet,
		Bus:      a.Bus,
		PublicIP: func(ctx context.Context) (string, error) {
			if st, err := inbound.LoadState(root); err == nil && st.PublicIP != "" {
				return st.PublicIP, nil
			}
			return publish.DetectPublicIP(ctx)
		},
	})
	if err != nil {
		a.Bus.Emit(events.Event{Type: "nodes.error", Actor: "engine", Level: "error", Message: "the nodes' mesh is unavailable: " + err.Error()})
		return nil
	}
	theMesh = m
	if inboundHook != nil {
		inboundHook.Elsewhere = func(host string) bool {
			_, ok := m.Owner(host)
			return ok
		}
		inboundHook.Relay = m.Relay
	}
	var legacy []struct {
		Name    string `json:"name"`
		TokenID string `json:"token_id"`
	}
	if raw, err := os.ReadFile(m.LegacyPath()); err == nil && json.Unmarshal(raw, &legacy) == nil {
		if store := srv.Auth(); store != nil {
			for _, l := range legacy {
				_ = store.DeleteToken(l.TokenID)
			}
		}
		_ = os.Remove(m.LegacyPath())
		a.Bus.Emit(events.Event{Type: "nodes.legacy_removed", Actor: "engine", Level: "warn", Message: fmt.Sprintf("%d node links over the public panel were undone: nodes now join through the WireGuard mesh", len(legacy))})
	}
	return m
}

// restorable is every agent module, for Restore.
func restorable() []agents.Restorable {
	out := []agents.Restorable{agentClaude, agentAgy, agentOpen}
	for _, v := range agentVendors {
		out = append(out, v)
	}
	return out
}

// githubRepos is the github module as the place sites are made from.
type githubRepos struct{ set *moduleSet }

func (g githubRepos) Enabled() bool { return g.set.Manager.Enabled(github.Key) }

func (g githubRepos) Inspect(ctx context.Context, ref string) (sites.Repo, error) {
	return g.set.GitHub.Inspect(ctx, ref)
}

// doorModels are the models a site door may name for an agent: its own,
// and for Claude the CLI's aliases as well, the ones the list does not
// already carry (the list is the aliases by default, and appending them
// again showed each one twice in the gateway's screen).
func doorModels(provider string, models []string) []string {
	out := append([]string(nil), models...)
	if provider == "claude" {
		for _, a := range []string{"opus", "sonnet", "haiku"} {
			if !slices.Contains(out, a) {
				out = append(out, a)
			}
		}
	}
	return out
}
