// Package manifest defines site.json, the only state a site has. Everything
// the engine, the proxy and the panel know about a site comes from here, and
// everything they decide about it is written back here. The schema is
// described for humans in docs/manifest.md; this file is the schema for the
// compiler.
package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/attack"
	"github.com/dcimorra/the-middleware/internal/httpcache"
	"github.com/dcimorra/the-middleware/internal/pages"
	"github.com/dcimorra/the-middleware/internal/security"
)

// FileName is the manifest's name inside a site folder.
const FileName = "site.json"

// Version is the only supported schema version.
const Version = 2

// Manifest is site.json.
type Manifest struct {
	Version int    `json:"version"`
	Title   string `json:"title,omitempty"`
	// Group puts the site under a heading in the panel's list (a client,
	// a family of projects). Nothing else reads it.
	Group string `json:"group,omitempty"`
	// Aliases are more names this site answers to, served the same way
	// and published with it.
	Aliases []string `json:"aliases,omitempty"`
	// WWW false drops the www. name an apex domain carries without writing
	// it (see Hosts).
	WWW      *bool              `json:"www,omitempty"`
	Static   *Static            `json:"static,omitempty"`
	Run      *Run               `json:"run,omitempty"`
	Upstream string             `json:"upstream,omitempty"`
	Workers  map[string]*Worker `json:"workers,omitempty"`
	// Services are the site's processes when it has more than one, each
	// with its own domains, lifecycle and rules (services.go). A site has
	// run or services, never both.
	Services map[string]*Service `json:"services,omitempty"`
	// Move says what a move to another node leaves behind (see SiteMove).
	Move     *SiteMove `json:"move,omitempty"`
	Disabled bool      `json:"disabled,omitempty"`
	Hold     *Hold     `json:"hold,omitempty"`
	Access   *Access   `json:"access,omitempty"`
	WAF      *WAF      `json:"waf,omitempty"`
	// Security decides by what a caller claims to be and where it is
	// (robots, browsers, countries), and which paths are never served.
	Security *security.Rules `json:"security,omitempty"`
	// VPN makes the site exist only inside the VPN: served through the VPN
	// door and nowhere else, never published, resolved by the VPN's own
	// DNS. Devices, when listed, narrows it to those peers. With Paths,
	// only those paths are: the site stays public and published, and
	// /admin (say) answers only through the VPN.
	VPN *VPNOnly `json:"vpn,omitempty"`
	// Autofix false keeps the automatic repair away from this site even
	// when the installation turns it on: some sites are not touched
	// without someone looking. The panel cannot override it.
	Autofix *bool `json:"autofix,omitempty"`
	// Attack is under-attack mode: a proof of work before anything is
	// served (internal/attack).
	Attack *attack.Rules `json:"attack,omitempty"`
	// Needs is what this site needs up to do its job: another site's
	// domain, or one of a site's workers as "name@domain". A first start
	// waits for them (a minute at most) and the panel names what is
	// missing.
	Needs []string `json:"needs,omitempty"`
	// Node is the Node major the site runs on ("24", or "lts"), over what
	// its own files ask (.nvmrc, engines.node): installed with nvm when the
	// machine does not have it. A site that asks nothing runs on the
	// machine's own node, which is a different one on each machine; a move
	// writes here the one it ran on, so the target runs it on the same.
	Node  string `json:"node,omitempty"`
	Cache *Cache `json:"cache,omitempty"`
	// Redirects send a request that matches one to another address with
	// a 3xx, before the site is served (redirect.go).
	Redirects []Redirect `json:"redirects,omitempty"`
	// ForceHTTPS are the site's names that answer only over https, one by
	// one because a site with several names may keep one on plain http
	// (an old device, a webhook that cannot follow a redirect). The rest
	// of the house serves http as it comes (https.go).
	ForceHTTPS []string          `json:"force_https,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	Notice     *Notice           `json:"notice,omitempty"`
	// ClaudeAPI opens the site door: the site's own code may talk to Claude
	// through the house (see docs/api.md, "The site door").
	ClaudeAPI *ClaudeAPI `json:"claude_api,omitempty"`
	// Claude passes the house's Claude subscription token to the site's
	// process (CLAUDE_CODE_OAUTH_TOKEN), for a site that runs the CLI
	// itself. Prefer claude_api: the token reaches as far as the
	// subscription does, the site door only as far as its level.
	Claude bool `json:"claude,omitempty"`
	// Chat is what this site's conversation may do beyond its folder.
	Chat   *ChatRules `json:"chat,omitempty"`
	Deploy *Deploy    `json:"deploy,omitempty"`
	Agent  *Agent     `json:"agent,omitempty"`
}

// Static serves files.
type Static struct {
	Root string `json:"root,omitempty"`
	SPA  bool   `json:"spa,omitempty"`
}

// Run describes the supervised instance.
type Run struct {
	Command []string          `json:"command"`
	Cwd     string            `json:"cwd,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	// EnvFiles are read in order, a later file overriding an earlier one,
	// and `env` over all of them. A missing file is not an error: `.env`
	// is usually not committed.
	EnvFiles    []string          `json:"env_files,omitempty"`
	EnvFromFile map[string]string `json:"env_from_file,omitempty"`
	// Port is written only when something outside the house already knows
	// it. A site with a written port cannot hand over (two generations
	// cannot share it), so a change restarts it instead.
	Port    int      `json:"port,omitempty"`
	Health  *Health  `json:"health,omitempty"`
	Drain   Duration `json:"drain,omitempty"`
	Restart *Restart `json:"restart,omitempty"`
	Runtime string   `json:"runtime,omitempty"` // "process" (default) | "docker"
	Docker  *Docker  `json:"docker,omitempty"`
}

// Health is how the engine knows a new instance is ready.
type Health struct {
	Path    string   `json:"path,omitempty"`
	Timeout Duration `json:"timeout,omitempty"`
	// Interval is how often a running instance is asked, once it is up.
	Interval Duration `json:"interval,omitempty"`
	// ExpectStatus, when set, is the only answer that counts as healthy;
	// otherwise anything below 500 does (a 404 on / still means the
	// process answers).
	ExpectStatus int `json:"expect_status,omitempty"`
	// Command, for a service that answers no HTTP (a database), is run
	// inside it and passes when it exits 0. Services only.
	Command []string `json:"command,omitempty"`
}

// Healthy says whether an HTTP status passes the check.
func (h Health) Healthy(status int) bool {
	if h.ExpectStatus != 0 {
		return status == h.ExpectStatus
	}
	return status < 500
}

// Restart bounds the backoff after a crash.
type Restart struct {
	MaxBackoff Duration `json:"max_backoff,omitempty"`
}

// Docker is the container runtime's share of the manifest.
type Docker struct {
	Image   string   `json:"image,omitempty"`
	Build   string   `json:"build,omitempty"` // Dockerfile path, relative to the site
	Volumes []string `json:"volumes,omitempty"`
	Memory  string   `json:"memory,omitempty"`
	// Pids caps the processes inside (default 1024). CPUs is a hard cap
	// in cores; CPUWeight is the relative share under contention (docker's
	// cpu-shares, 1024 is the default share).
	Pids      int     `json:"pids,omitempty"`
	CPUs      float64 `json:"cpus,omitempty"`
	CPUWeight int     `json:"cpu_weight,omitempty"`
	// User is uid:gid inside; empty is the right default for the daemon
	// (see runtime.Docker).
	User string `json:"user,omitempty"`
	// Port is the port the process listens on inside the container, for
	// an image that listens on one of its own (mariadb on 3306) and is
	// not told $PORT. Zero is $PORT, the same number inside and out.
	// Services only.
	Port int `json:"port,omitempty"`
	// Watch are folders and files of the site whose contents are part of
	// a built image's revision, so a change there rebuilds this service
	// and no other. Empty is the Dockerfile's folder. Services only.
	Watch []string `json:"watch,omitempty"`
}

// Worker is an always-on process that belongs to a site and answers no
// hostname of its own. With no port, one is assigned and passed in $PORT;
// a written port is for when whoever calls the worker already knows it.
type Worker struct {
	Command     []string          `json:"command"`
	Cwd         string            `json:"cwd,omitempty"`
	Port        int               `json:"port,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	EnvFiles    []string          `json:"env_files,omitempty"`
	EnvFromFile map[string]string `json:"env_from_file,omitempty"`
	Disabled    bool              `json:"disabled,omitempty"`
	Note        string            `json:"note,omitempty"`
	// Runtime and Docker run the worker in a container, behind the same
	// walls as a site's: the database a site needs, declared where the
	// site is, so whoever moves the site knows it is there.
	Runtime string  `json:"runtime,omitempty"` // "process" (default) | "docker"
	Docker  *Docker `json:"docker,omitempty"`
	// Move is what a move does with what the worker keeps outside the
	// folder (a database in a named volume): copying its files while it
	// runs would carry a torn copy, and not copying them carries nothing.
	Move *WorkerMove `json:"move,omitempty"`
}

// SiteMove is what a move does with the site's folder.
type SiteMove struct {
	// Exclude are globs relative to the folder ("storage/products/*",
	// ".next-*/cache") whose folders and files do not travel: what the
	// site makes again on demand. comparaloo's 7.9 GB of image cache, in
	// 692,042 files, was most of what a move of it would have copied. A
	// folder's own pattern leaves out the folder; "folder/*" its contents,
	// so the folder is there for the site to write into.
	Exclude []string `json:"exclude,omitempty"`
}

// Excluded says whether rel, a slash path inside the site's folder, is
// one the manifest's move.exclude leaves out.
func (m *Manifest) Excluded(rel string) bool {
	if m == nil || m.Move == nil {
		return false
	}
	for _, pat := range m.Move.Exclude {
		if ok, _ := path.Match(pat, rel); ok {
			return true
		}
	}
	return false
}

// WorkerMove are the commands a move runs for a worker's data. Arrays, as
// every command of a manifest: a script of the site's ("./scripts/x.sh")
// or the program itself.
type WorkerMove struct {
	// Export runs where the site is, with the site stopped and the worker
	// still up, and leaves what it takes inside the folder, which the last
	// copy carries.
	Export []string `json:"export"`
	// Ready runs on the target, again and again, until it succeeds: the
	// container up is not the database answering.
	Ready []string `json:"ready,omitempty"`
	// Import runs on the target with the worker up and the site not yet
	// started, so the site never comes up on an empty database.
	Import []string `json:"import"`
	// Timeout bounds each of them (30m when empty).
	Timeout Duration `json:"timeout,omitempty"`
	// Output, a path inside the folder, is where the house writes the
	// export's standard output ("pg_dump -Fc" with no file of its own).
	// The export inside a database's container runs as the database's user,
	// who cannot write in the site's folder: the house can, and the file it
	// makes is readable by the import there.
	Output string `json:"output,omitempty"`
	// Live takes the data while the site serves: export, and import on the
	// target, run before the site stops, so it is down only for the last
	// copy and the start. What the site writes after the export does not
	// travel. Without it, the site is put on hold (its workers up) for the
	// export and the import, which is the whole of the down time.
	Live bool `json:"live,omitempty"`
}

// MovingWorkers are the workers whose data moves by command, by name;
// live picks the ones that move while the site serves, or the others.
func (m *Manifest) MovingWorkers(live bool) []string {
	if m == nil {
		return nil
	}
	var out []string
	for name, w := range m.Workers {
		if w != nil && w.Move != nil && !w.Disabled && w.Move.Live == live {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Hold shows a page instead of the site, with the process stopped.
type Hold struct {
	Enabled bool   `json:"enabled"`
	Title   string `json:"title,omitempty"`
	Message string `json:"message,omitempty"`
	// Mode is what the page answers: "maintenance" (503, the default),
	// "404", "410" (gone, and search engines drop it) or "500".
	Mode string `json:"mode,omitempty"`
	// Until is when the site is due back (RFC 3339): shown on the page
	// and sent as Retry-After.
	Until string `json:"until,omitempty"`
}

// ChatRules widen a site's conversation.
type ChatRules struct {
	// Reads are other sites of this house whose folder the conversation
	// may read, never write: what is lent is understanding, not the
	// ability to change someone else's project. The folder is lent whole,
	// its .env included, and that is a decision: closing the secrets and
	// leaving the rest would promise a wall with a hole in it. Which is why
	// each one is written by hand.
	Reads []string `json:"reads,omitempty"`
}

// ClaudeAPI is the site door's rules for this site: the LLM gateway its
// own code calls. The name is the one it had when Claude was its only
// agent; any enabled agent may answer it now. An empty block is a site
// that may ask Claude and nothing else: no tools, no files, no web.
type ClaudeAPI struct {
	// Provider is the agent that answers (claude, openai, agy…); empty is
	// claude.
	Provider string `json:"provider,omitempty"`
	// Permissions are granted one by one: read, edit, web, shell. When
	// the block lists none, Tools says what the door may do, as it did
	// before permissions came apart.
	Permissions []string `json:"permissions,omitempty"`
	// Tools is none (default), read, web or write. Raising it is a serious
	// decision: the site's process answers the internet, and what an
	// injected prompt can reach depends on this word.
	Tools string `json:"tools,omitempty"`
	// DailyTurns caps a day's turns through the door, for an agent whose
	// turns report no cost (a subscription): a dollar cap never trips on
	// those. 0 is the house's default for such an agent.
	DailyTurns int `json:"daily_turns,omitempty"`
	// DailyUSD caps a day's spending through the door; 0 is the house's
	// (agents.site_daily_usd). Traffic decides this spending, not a person.
	DailyUSD float64 `json:"daily_usd,omitempty"`
	// MaxChats caps the conversations the site keeps open; 0 is 20.
	MaxChats int `json:"max_chats,omitempty"`
	// Model is the default model of the site's conversations.
	Model string `json:"model,omitempty"`
	// Models are the models the site's code may choose among, call by
	// call (a cheap one to classify, a strong one to write); empty is any
	// of its agent's. Model, when set, is one of them.
	Models []string `json:"models,omitempty"`
	// HideChats keeps the site's conversations out of the panel's list;
	// they are still kept, costed and readable.
	HideChats bool `json:"hide_chats,omitempty"`
}

// VPNOnly is the manifest's vpn section.
type VPNOnly struct {
	// Devices are VPN peer names; empty is every device.
	Devices []string `json:"devices,omitempty"`
	// Paths, when listed, are the only paths kept inside the VPN: a path
	// and everything under it ("/admin" covers "/admin" and "/admin/x",
	// not "/administrator"). Empty is the whole site.
	Paths []string `json:"paths,omitempty"`
}

// Whole says the whole site is inside the VPN: never published, served
// through no other door.
func (v *VPNOnly) Whole() bool { return v != nil && len(v.Paths) == 0 }

// Covers says the request path is kept inside the VPN. The path is
// cleaned first, so //admin and /x/../admin are /admin.
func (v *VPNOnly) Covers(p string) bool {
	if v == nil {
		return false
	}
	if len(v.Paths) == 0 {
		return true
	}
	p = path.Clean("/" + p)
	for _, pre := range v.Paths {
		pre = strings.TrimSuffix(path.Clean(pre), "/")
		if pre == "" || p == pre || strings.HasPrefix(p, pre+"/") {
			return true
		}
	}
	return false
}

// Notice is how the visitor pages of this site look.
type Notice struct {
	// Theme is one of pages.Themes(); empty is the house's.
	Theme string `json:"theme,omitempty"`
}

// Access decides who gets in. Passwords are stored hashed (see
// internal/access); the panel and `middleware site password` write them.
type Access struct {
	Password      string                  `json:"password,omitempty"`
	Message       string                  `json:"message,omitempty"` // shown above the password box
	Allow         []string                `json:"allow,omitempty"`   // CIDRs or addresses
	Deny          []string                `json:"deny,omitempty"`
	DenyCountries []string                `json:"deny_countries,omitempty"`
	Routes        map[string]*RouteAccess `json:"routes,omitempty"`
}

// RouteAccess is a zone: a path prefix with rules added to the site's.
type RouteAccess struct {
	Password string   `json:"password,omitempty"` // the zone's own, asked after the site's
	Message  string   `json:"message,omitempty"`
	Allow    []string `json:"allow,omitempty"`
	Deny     []string `json:"deny,omitempty"`
	Note     string   `json:"note,omitempty"` // for whoever reads the manifest
}

// WAF is the per-site layer-7 policy.
type WAF struct {
	// Off takes this site out of the rate limit and the body limit. What
	// protects the machine stays: the block list, and the bans for
	// exploits, scanners and failed logins, which close every site.
	Off     bool   `json:"off,omitempty"`
	Rate    *Rate  `json:"rate,omitempty"`
	MaxBody string `json:"max_body,omitempty"`
	// UnderAttack is the switch from before `attack` existed, read as
	// `attack.on` with its defaults; switching from the panel or the API
	// moves it there.
	UnderAttack bool `json:"under_attack,omitempty"`
}

// Rate is a token bucket per client address.
type Rate struct {
	Requests int      `json:"requests"`
	Window   Duration `json:"window"`
	Burst    int      `json:"burst,omitempty"`
}

// Cache is what the house cache may keep.
type Cache struct {
	StaticMaxAge Duration `json:"static_max_age,omitempty"`
	// Rules are the house's response cache (internal/httpcache): which
	// paths are kept and how. None keeps nothing.
	Rules []httpcache.Rule `json:"rules,omitempty"`
	// MaxMB caps this site's share; it can only ask for less than the
	// house's budget (Settings), never reserve more.
	MaxMB int `json:"max_mb,omitempty"`
}

// Deploy is how new code gets in.
type Deploy struct {
	GitHub *GitHubDeploy `json:"github,omitempty"`
}

// GitHubDeploy deploys on push.
type GitHubDeploy struct {
	Repository string     `json:"repository"`
	Branch     string     `json:"branch,omitempty"`
	Build      [][]string `json:"build,omitempty"`
	Pointer    string     `json:"pointer,omitempty"`
}

// Agent chooses the conversation provider for this site.
type Agent struct {
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	// MonthlyCapUSD stops the conversation once the ledger passes it. Zero
	// means no cap.
	MonthlyCapUSD float64 `json:"monthly_cap_usd,omitempty"`
}

// Duration marshals as a Go duration string ("8s"), because "8000000000" in
// a file a person edits is a number nobody can read.
type Duration time.Duration

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON accepts "8s" or a bare number of seconds.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		v, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("duration %q: %w", s, err)
		}
		*d = Duration(v)
		return nil
	}
	var n float64
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("duration %s: want \"8s\" or seconds", b)
	}
	*d = Duration(time.Duration(n * float64(time.Second)))
	return nil
}

// Or returns d or fallback when d is zero.
func (d Duration) Or(fallback time.Duration) time.Duration {
	if d == 0 {
		return fallback
	}
	return time.Duration(d)
}

// Load reads and validates a site folder's manifest.
func Load(siteDir string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(siteDir, FileName))
	if err != nil {
		return nil, err
	}
	m, err := Parse(data)
	if err != nil {
		return nil, err
	}
	if err := m.CheckInside(siteDir); err != nil {
		return nil, err
	}
	return m, nil
}

// Parse decodes and validates manifest bytes. Unknown fields are an error:
// a typo in "comand" silently serving a folder as static files is exactly
// the kind of failure that looks like success.
func Parse(data []byte) (*Manifest, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Save writes m to the site folder atomically.
func Save(siteDir string, m *Manifest) error {
	if err := m.Validate(); err != nil {
		return err
	}
	if err := m.CheckInside(siteDir); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(siteDir, FileName), append(data, '\n'))
}

var domainRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9-]{2,63}$`)

// ValidDomain reports whether s can name a site folder. Lower-case ASCII
// only: a folder is a route, and a route with a capital letter never matches
// a Host header, which arrives lower-cased.
func ValidDomain(s string) bool { return domainRe.MatchString(s) }

var projectRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Reserved names cannot be projects: "middleware" is the house's own
// conversation, and the rest are what a Host header without a dot says.
var reserved = map[string]bool{"middleware": true, "localhost": true}

// IsProject reports whether a site folder's name is a project: a name with
// no dot, which answers no hostname. A project has its workers, its tasks
// and its conversation, and nothing that serves a request.
func IsProject(s string) bool { return projectRe.MatchString(s) && !reserved[s] }

// ValidName reports whether s can name a folder under sites/: a domain or
// a project. What deals with hostnames (certificates, doors, aliases)
// asks ValidDomain instead.
func ValidName(s string) bool { return ValidDomain(s) || IsProject(s) }

// nodeRe is a Node major or "lts".
var nodeRe = regexp.MustCompile(`^([1-9][0-9]?|lts)$`)

// agentName is what an agent's key looks like (claude, openai, agy).
var agentName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// Validate checks the manifest is coherent. It rejects what the engine could
// not act on rather than what merely looks odd.
// validRuntime checks a runtime and its docker section, a site's run or a
// worker's alike.
func validRuntime(field, rt string, d *Docker) error {
	switch rt {
	case "", "process", "docker":
	default:
		return fmt.Errorf("%s.runtime: %q is not process or docker", field, rt)
	}
	if rt == "docker" && (d == nil || (d.Image == "" && d.Build == "")) {
		return fmt.Errorf("%s.docker: needs image or build", field)
	}
	if d != nil {
		if d.Pids < 0 || d.CPUs < 0 || d.CPUWeight < 0 {
			return fmt.Errorf("%s.docker: pids, cpus and cpu_weight cannot be negative", field)
		}
		if d.User != "" && !userRe.MatchString(d.User) {
			return fmt.Errorf("%s.docker.user: %q is not uid[:gid]", field, d.User)
		}
	}
	return nil
}

func (m *Manifest) Validate() error {
	if m.Version != Version {
		return fmt.Errorf("version: want %d, got %d", Version, m.Version)
	}
	if m.Node != "" && !nodeRe.MatchString(m.Node) {
		return fmt.Errorf("node: %q is not a major version (24) or lts", m.Node)
	}
	if m.Run != nil {
		if err := validRun("run", m.Run, false); err != nil {
			return err
		}
	}
	if err := m.validServices(); err != nil {
		return err
	}
	for _, a := range m.Aliases {
		if !ValidDomain(a) {
			return fmt.Errorf("aliases: %q is not a domain", a)
		}
	}
	if err := validHold("hold", m.Hold); err != nil {
		return err
	}
	if c := m.ClaudeAPI; c != nil {
		switch strings.ToLower(c.Tools) {
		case "", "none", "read", "web", "write":
		default:
			return fmt.Errorf("claude_api.tools: %q is not none, read, web or write", c.Tools)
		}
		if c.DailyUSD < 0 || c.MaxChats < 0 || c.DailyTurns < 0 {
			return errors.New("claude_api: daily_usd, daily_turns and max_chats cannot be negative")
		}
		for _, p := range c.Permissions {
			switch p {
			case "read", "edit", "web", "shell":
			default:
				return fmt.Errorf("claude_api.permissions: %q is not read, edit, web or shell", p)
			}
		}
		if c.Provider != "" && !agentName.MatchString(c.Provider) {
			return fmt.Errorf("claude_api.provider: %q is not an agent's name", c.Provider)
		}
		for _, m := range c.Models {
			if strings.TrimSpace(m) == "" {
				return errors.New("claude_api.models: an empty name")
			}
		}
		if c.Model != "" && len(c.Models) > 0 && !slices.Contains(c.Models, c.Model) {
			return fmt.Errorf("claude_api.model: %q is not one of claude_api.models (%s)", c.Model, strings.Join(c.Models, ", "))
		}
	}
	for i, rd := range m.Redirects {
		if err := rd.Check(); err != nil {
			return fmt.Errorf("redirects[%d]: %w", i, err)
		}
	}
	if err := validForceHTTPS(m.ForceHTTPS); err != nil {
		return err
	}
	if m.Chat != nil {
		for _, r := range m.Chat.Reads {
			if !ValidName(r) {
				return fmt.Errorf("chat.reads: %q is not a site", r)
			}
		}
	}
	if m.Notice != nil && !pages.IsTheme(m.Notice.Theme) {
		return fmt.Errorf("notice.theme: %q is not one of the designs", m.Notice.Theme)
	}
	for _, n := range m.Needs {
		ok := ValidName(n)
		if name, dom, isWorker := strings.Cut(n, "@"); isWorker {
			ok = validName(name) && ValidName(dom)
		}
		if !ok {
			return fmt.Errorf("needs: %q is not a site nor worker@site", n)
		}
	}
	if m.Upstream != "" && !strings.HasPrefix(m.Upstream, "http://") && !strings.HasPrefix(m.Upstream, "https://") {
		return fmt.Errorf("upstream: %q must start with http:// or https://", m.Upstream)
	}
	if m.Move != nil {
		for _, pat := range m.Move.Exclude {
			clean := path.Clean(pat)
			if pat == "" || clean != pat || path.IsAbs(pat) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
				return fmt.Errorf("move.exclude: %q is not a path inside the folder (relative, clean, no ..)", pat)
			}
			if _, err := path.Match(pat, ""); err != nil {
				return fmt.Errorf("move.exclude: %q: %v", pat, err)
			}
			// The manifest is what makes the site there: leaving it out
			// would move a folder that is not a site.
			if ok, _ := path.Match(pat, FileName); ok {
				return fmt.Errorf("move.exclude: %q leaves out %s", pat, FileName)
			}
		}
	}
	for name, w := range m.Workers {
		if !validName(name) {
			return fmt.Errorf("workers: %q is not a name", name)
		}
		if len(w.Command) == 0 {
			return fmt.Errorf("workers.%s.command: empty", name)
		}
		if w.Port < 0 || w.Port > 65535 {
			return fmt.Errorf("workers.%s.port: %d is not a port", name, w.Port)
		}
		if err := validRuntime("workers."+name, w.Runtime, w.Docker); err != nil {
			return err
		}
		if mv := w.Move; mv != nil {
			// Both or none: an export nobody imports is a move that
			// carries the data and leaves the site on an empty database.
			if len(mv.Export) == 0 || len(mv.Import) == 0 {
				return fmt.Errorf("workers.%s.move: needs export and import", name)
			}
			if mv.Timeout < 0 {
				return fmt.Errorf("workers.%s.move.timeout: negative", name)
			}
			if o := mv.Output; o != "" {
				if c := path.Clean(o); c != o || path.IsAbs(o) || c == "." || c == ".." || strings.HasPrefix(c, "../") || c == FileName {
					return fmt.Errorf("workers.%s.move.output: %q is not a file inside the folder", name, o)
				}
			}
		}
		for k := range w.EnvFromFile {
			if !validEnvKey(k) {
				return fmt.Errorf("workers.%s.env_from_file: %q is not a variable name", name, k)
			}
		}
	}
	if err := validAccess("access", m.Access); err != nil {
		return err
	}
	if m.VPN != nil {
		for _, d := range m.VPN.Devices {
			if !validName(d) {
				return fmt.Errorf("vpn.devices: %q is not a device name", d)
			}
		}
		for _, p := range m.VPN.Paths {
			if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "?#*") {
				return fmt.Errorf("vpn.paths: %q is not a path (it starts with /, no wildcards)", p)
			}
		}
	}
	if err := validLive("", m.Cache, m.Attack, m.Security, m.WAF); err != nil {
		return err
	}
	if m.Deploy != nil && m.Deploy.GitHub != nil && !strings.Contains(m.Deploy.GitHub.Repository, "/") {
		return errors.New("deploy.github.repository: want owner/name")
	}
	return nil
}

// validRun checks a run section: a site's, or a service's (inService),
// which may also say what only a service can (docker.port, docker.watch,
// health.command).
func validRun(field string, r *Run, inService bool) error {
	if len(r.Command) == 0 && !(inService && r.Runtime == "docker") {
		return fmt.Errorf("%s.command: empty", field)
	}
	if err := validRuntime(field, r.Runtime, r.Docker); err != nil {
		return err
	}
	for k := range r.Env {
		if !validEnvKey(k) {
			return fmt.Errorf("%s.env: %q is not a variable name", field, k)
		}
	}
	for k := range r.EnvFromFile {
		if !validEnvKey(k) {
			return fmt.Errorf("%s.env_from_file: %q is not a variable name", field, k)
		}
	}
	if r.Port < 0 || r.Port > 65535 {
		return fmt.Errorf("%s.port: %d is not a port", field, r.Port)
	}
	if d := r.Docker; d != nil {
		if d.Port < 0 || d.Port > 65535 {
			return fmt.Errorf("%s.docker.port: %d is not a port", field, d.Port)
		}
		// Said rather than ignored: a site's run that wrote them would
		// look as if they worked.
		if !inService && (d.Port != 0 || len(d.Watch) > 0) {
			return fmt.Errorf("%s.docker: port and watch belong to a service (services.<name>.run)", field)
		}
	}
	if r.Health != nil && len(r.Health.Command) > 0 && !inService {
		return fmt.Errorf("%s.health.command belongs to a service (services.<name>.run)", field)
	}
	return nil
}

// validHold checks a hold section, the site's or a service's.
func validHold(field string, h *Hold) error {
	if h == nil {
		return nil
	}
	switch h.Mode {
	case "", "maintenance", "404", "410", "500":
	default:
		return fmt.Errorf("%s.mode: %q is not maintenance, 404, 410 or 500", field, h.Mode)
	}
	if h.Until != "" {
		if _, err := time.Parse(time.RFC3339, h.Until); err != nil {
			return fmt.Errorf("%s.until: %q is not an RFC 3339 time", field, h.Until)
		}
	}
	return nil
}

// validAccess checks every address of an access section.
func validAccess(field string, a *Access) error {
	if a == nil {
		return nil
	}
	if err := validAddrs(field+".allow", a.Allow); err != nil {
		return err
	}
	if err := validAddrs(field+".deny", a.Deny); err != nil {
		return err
	}
	for prefix, r := range a.Routes {
		if r == nil {
			continue
		}
		if err := validAddrs(field+".routes."+prefix+".allow", r.Allow); err != nil {
			return err
		}
		if err := validAddrs(field+".routes."+prefix+".deny", r.Deny); err != nil {
			return err
		}
	}
	return nil
}

// validLive checks the rules applied live (cache, attack, security, waf);
// prefix is "" for the site's and "services.<name>." for a service's.
func validLive(prefix string, c *Cache, at *attack.Rules, sec *security.Rules, w *WAF) error {
	if c != nil {
		if c.MaxMB < 0 {
			return fmt.Errorf("%scache.max_mb cannot be negative", prefix)
		}
		for i, r := range c.Rules {
			switch {
			case !strings.HasPrefix(r.Path, "/"):
				return fmt.Errorf("%scache.rules[%d].path: %q must start with /", prefix, i, r.Path)
			case r.Mode != httpcache.ModeOrigin && r.Mode != httpcache.ModeForce && r.Mode != httpcache.ModeNever:
				return fmt.Errorf("%scache.rules[%d].mode: %q is not origin, force or never", prefix, i, r.Mode)
			case r.Mode == httpcache.ModeForce && r.TTL <= 0:
				return fmt.Errorf("%scache.rules[%d].ttl: force needs the seconds to keep", prefix, i)
			case r.Mode == httpcache.ModeNever && r.IgnoreCookies:
				// Said of a rule that keeps nothing, it is a rule written
				// for another path: refused, so it is not read as working.
				return fmt.Errorf("%scache.rules[%d].ignore_cookies: never keeps nothing, with cookies or without", prefix, i)
			}
		}
	}
	if err := at.Validate(); err != nil {
		return fmt.Errorf("%sattack.%w", prefix, err)
	}
	if _, err := security.Compile(sec); err != nil {
		return fmt.Errorf("%ssecurity.%w", prefix, err)
	}
	if w != nil && w.Rate != nil {
		if w.Rate.Requests <= 0 || w.Rate.Window <= 0 {
			return fmt.Errorf("%swaf.rate: requests and window must be positive", prefix)
		}
	}
	return nil
}

// Mode is what answers a request for this site, in the order docs/manifest.md
// lists.
type Mode string

// The modes.
const (
	ModeDisabled Mode = "disabled"
	ModeHold     Mode = "hold"
	ModeRun      Mode = "run"
	// ModeServices is a site whose names are served by its services, each
	// its own process (services.go).
	ModeServices Mode = "services"
	ModeUpstream Mode = "upstream"
	ModeStatic   Mode = "static"
	// ModeProject is a folder without a domain: workers, tasks and a
	// conversation, nothing that answers a request. The engine sets it
	// from the folder's name; a manifest cannot ask for it.
	ModeProject Mode = "project"
)

// Hosts is every name the site in the folder called name answers to and
// is published, certified and unpublished under: the folder's name, its
// aliases, and www. in front of an apex domain unless `www` is false.
// Everything that touches names asks this, so none of them can forget one:
// V1 published luma32.com and left www.luma32.com with no record and no
// certificate because nobody had written it as an alias. A project has no
// names at all.
func (m *Manifest) Hosts(name string) []string {
	if IsProject(name) || !ValidDomain(name) {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(h string) {
		h = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), "."))
		if h != "" && !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	// The www of every apex the site answers to, its own name's and an
	// extra domain's alike: whoever registers patata.com means
	// www.patata.com too. It is published and certified, and the proxy
	// sends it to the site's name.
	www := m.WWW == nil || *m.WWW
	apex := func(h string) {
		if www && strings.Count(h, ".") == 1 && !strings.HasPrefix(h, "www.") {
			add("www." + strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), ".")))
		}
	}
	// A service's domains are the site's names as aliases are: published,
	// certified, refused when another site claims them.
	extra := append([]string(nil), m.Aliases...)
	for _, n := range m.ServiceNames() {
		for _, d := range m.Services[n].Domains {
			host, _ := SplitDomain(d)
			extra = append(extra, host)
		}
	}
	add(name)
	for _, a := range extra {
		add(a)
	}
	apex(name)
	for _, a := range extra {
		apex(a)
	}
	return out
}

// ModeFor is Mode for the folder called name: a project is a project
// unless it is disabled.
func (m *Manifest) ModeFor(name string) Mode {
	mode := m.Mode()
	if mode != ModeDisabled && IsProject(name) {
		return ModeProject
	}
	return mode
}

// Mode resolves the precedence rules.
func (m *Manifest) Mode() Mode {
	switch {
	case m.Disabled:
		return ModeDisabled
	case m.Hold != nil && m.Hold.Enabled:
		return ModeHold
	case len(m.Services) > 0:
		return ModeServices
	case m.Run != nil:
		return ModeRun
	case m.Upstream != "":
		return ModeUpstream
	default:
		return ModeStatic
	}
}

var userRe = regexp.MustCompile(`^[0-9]{1,10}(:[0-9]{1,10})?$`)
var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// validAddrs rejects a list with an entry the door could not read. The
// door used to skip such an entry, which fails open: "10.0.0/8" in an
// allow list that held nothing else left the list empty, and an empty
// allow list lets everyone in. A typo in a deny list just as quietly
// stopped denying. Refusing the manifest keeps the previous one in force
// and the engine says why.
func validAddrs(field string, list []string) error {
	for _, s := range list {
		if !ValidAddr(s) {
			return fmt.Errorf("%s: %q is not an address or CIDR", field, s)
		}
	}
	return nil
}

// ValidAddr is whether s is an address or a CIDR as the door reads them.
func ValidAddr(s string) bool {
	s = strings.TrimSpace(s)
	if _, err := netip.ParsePrefix(s); err == nil {
		return true
	}
	_, err := netip.ParseAddr(s)
	return err == nil
}

func validEnvKey(s string) bool { return envKeyRe.MatchString(s) }
func validName(s string) bool   { return nameRe.MatchString(s) }
