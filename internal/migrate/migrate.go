// Package migrate turns a V1 site.json into a V2 one.
//
// It converts what V2 can do and refuses the rest: a V1 setting in use with
// no V2 equivalent yet is a blocker, and a site with a blocker is not
// converted at all. Converting it without the setting would publish a site
// that looks migrated and has quietly lost something (a VPN-only site served
// to everyone, a password zone gone), which is the failure that looks
// exactly like success. As V2 gains each feature, its blocker becomes a
// conversion here; ROADMAP.md is the list.
//
// Settings that change nothing V1 would have done (an attack mode written
// but off, a health timeout V2 measures differently) are converted or
// dropped with a note, so the report says everything that changed.
package migrate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/access"
	"github.com/dcimorra/the-middleware/internal/attack"
	"github.com/dcimorra/the-middleware/internal/httpcache"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/security"
)

// Result is one site's conversion.
type Result struct {
	Name string `json:"name"`
	// Manifest is the V2 manifest, nil when there are blockers.
	Manifest *manifest.Manifest `json:"manifest,omitempty"`
	// Blockers are V1 settings in use that V2 cannot honour yet.
	Blockers []string `json:"blockers,omitempty"`
	// Notes are changes worth knowing that lose nothing.
	Notes []string `json:"notes,omitempty"`
	// Skipped says why a folder is not a site to convert.
	Skipped string `json:"skipped,omitempty"`
}

// Ready reports whether the site can be converted.
func (r Result) Ready() bool { return r.Manifest != nil && len(r.Blockers) == 0 }

// v1 is the V1 manifest as far as this package needs it. Fields whose
// contents are never converted are kept raw, only to know they are there.
type v1 struct {
	Title       string            `json:"title"`
	Kind        string            `json:"kind"`
	Aliases     []string          `json:"aliases"`
	WWW         *bool             `json:"www"`
	Group       string            `json:"group"`
	App         string            `json:"app"`
	Disabled    bool              `json:"disabled"`
	AutoFix     *bool             `json:"autofix"`
	Hold        *v1Hold           `json:"hold"`
	Auth        *v1Auth           `json:"auth"`
	IP          *v1IP             `json:"ip"`
	VPN         json.RawMessage   `json:"vpn"`
	Security    json.RawMessage   `json:"security"`
	Attack      json.RawMessage   `json:"attack"`
	Notice      json.RawMessage   `json:"notice"`
	Root        string            `json:"root"`
	SPA         bool              `json:"spa"`
	Command     []string          `json:"command"`
	Dir         string            `json:"dir"`
	Env         map[string]string `json:"env"`
	EnvFiles    []string          `json:"env_files"`
	EnvFromFile map[string]string `json:"env_from_file"`
	Port        int               `json:"port"`
	Workers     []v1Worker        `json:"workers"`
	Routes      []v1Route         `json:"routes"`
	Cache       json.RawMessage   `json:"cache"`
	Needs       []string          `json:"needs"`
	Chat        json.RawMessage   `json:"chat"`
	Claude      bool              `json:"claude"`
	ClaudeAPI   json.RawMessage   `json:"claude_api"`
	Container   json.RawMessage   `json:"container"`
	Health      v1Health          `json:"health"`
}

type v1Hold struct {
	Mode    string `json:"mode"`
	Message string `json:"message"`
	Until   string `json:"until"`
}

type v1Auth struct {
	Password string `json:"password"`
	Hash     string `json:"hash"`
	Message  string `json:"message"`
}

type v1IP struct {
	Allow []string `json:"allow"`
	Deny  []string `json:"deny"`
}

// v1Cache is read leniently, only for whether it is on: its other fields
// are never converted.
type v1Cache struct {
	Enabled *bool             `json:"enabled"`
	MaxMB   int               `json:"max_mb"`
	Rules   []json.RawMessage `json:"rules"`
}

// v1Modes are V1's cache modes, in Spanish, and V2's names for them.
var v1Modes = map[string]string{"": httpcache.ModeOrigin, "origen": httpcache.ModeOrigin, "forzar": httpcache.ModeForce, "nunca": httpcache.ModeNever}

// convertCache carries V1's house cache rules. A rule that also cached
// other methods than GET is carried for GET only and said: V2 keeps GET
// and HEAD and nothing else.
func (c *converter) convertCache(cr v1Cache) {
	out := &manifest.Cache{MaxMB: cr.MaxMB}
	for i, raw := range cr.Rules {
		var r struct {
			Path    string   `json:"path"`
			Note    string   `json:"note"`
			Mode    string   `json:"mode"`
			TTL     int      `json:"ttl"`
			Methods []string `json:"methods"`
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&r); err != nil {
			c.block("cache.rules[%d]: %v", i, err)
			return
		}
		mode, ok := v1Modes[r.Mode]
		if !ok {
			c.block("cache.rules[%d].mode %q is not a V1 mode", i, r.Mode)
			return
		}
		for _, m := range r.Methods {
			if !strings.EqualFold(m, "GET") && !strings.EqualFold(m, "HEAD") {
				c.note("cache.rules[%d]: %s is not cached in V2, only GET and HEAD", i, m)
			}
		}
		out.Rules = append(out.Rules, httpcache.Rule{Path: r.Path, Mode: mode, TTL: r.TTL, Note: r.Note})
	}
	c.cache = out
}

type v1Worker struct {
	Name        string            `json:"name"`
	Command     []string          `json:"command"`
	Dir         string            `json:"dir"`
	Env         map[string]string `json:"env"`
	EnvFiles    []string          `json:"env_files"`
	EnvFromFile map[string]string `json:"env_from_file"`
	Port        int               `json:"port"`
	Disabled    bool              `json:"disabled"`
	Note        string            `json:"note"`
}

type v1Route struct {
	Path string  `json:"path"`
	Note string  `json:"note"`
	Auth *v1Auth `json:"auth"`
	IP   *v1IP   `json:"ip"`
}

type v1Health struct {
	Path     string `json:"path"`
	Interval string `json:"interval"`
	Timeout  string `json:"timeout"`
	Expect   int    `json:"expect_status"`
}

// Convert converts the V1 manifest of the folder called name.
func Convert(name string, data []byte) Result {
	r := Result{Name: name}
	var in v1
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		r.Blockers = append(r.Blockers, "the V1 manifest does not read: "+err.Error())
		return r
	}
	c := &converter{r: &r}
	m := &manifest.Manifest{Version: manifest.Version, Title: in.Title, Group: in.Group, Disabled: in.Disabled, WWW: in.WWW}
	c.blockers(&in)
	c.serve(&in, m, name)
	c.access(&in, m)
	c.workers(&in, m)
	c.hold(&in, m)
	if len(in.Aliases) > 0 {
		m.Aliases = append([]string(nil), in.Aliases...)
	}
	m.Needs = in.Needs
	m.Notice = c.notice
	m.ClaudeAPI = c.claudeAPI
	m.Security = c.security
	m.Attack = c.attack
	m.VPN = c.vpn
	if c.cache != nil {
		if m.Cache == nil {
			m.Cache = c.cache
		} else {
			m.Cache.Rules, m.Cache.MaxMB = c.cache.Rules, c.cache.MaxMB
		}
	}
	if c.autofixOff {
		off := false
		m.Autofix = &off
	}
	m.Claude = in.Claude
	if len(r.Blockers) > 0 {
		return r
	}
	if err := m.Validate(); err != nil {
		r.Blockers = append(r.Blockers, "the converted manifest is not valid: "+err.Error())
		return r
	}
	r.Manifest = m
	return r
}

type converter struct {
	r         *Result
	notice    *manifest.Notice
	claudeAPI *manifest.ClaudeAPI
	security  *security.Rules
	attack    *attack.Rules
	// autofixOff carries V1's "autofix": false.
	autofixOff bool
	cache      *manifest.Cache
	vpn        *manifest.VPNOnly
}

// themeKeys maps V1's design names to V2's: the same seven designs.
var themeKeys = map[string]string{
	"": "", "sobrio": "sober", "ficha": "card", "consola": "console", "nota": "note",
	"cartel": "poster", "plano": "blueprint", "glitch": "glitch",
}

func (c *converter) block(format string, args ...any) {
	c.r.Blockers = append(c.r.Blockers, fmt.Sprintf(format, args...))
}

func (c *converter) note(format string, args ...any) {
	c.r.Notes = append(c.r.Notes, fmt.Sprintf(format, args...))
}

func present(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null" && s != "{}" && s != "[]" && s != "false"
}

// blockers lists the V1 settings in use that V2 cannot honour yet, each
// with the section of ROADMAP.md that brings it.
func (c *converter) blockers(in *v1) {
	if in.App != "" {
		// V1's in-process apps (Mask, the Browser) are not part of V2:
		// the site is converted and the line dropped, said here.
		c.note("app %q: apps are not part of V2, the site is converted without it", in.App)
	}
	if in.AutoFix != nil {
		if *in.AutoFix {
			c.note("autofix: true is the installation's switch in V2 (Settings), not the site's; the site follows it")
		} else {
			c.autofixOff = true
		}
	}
	if present(in.VPN) {
		var v manifest.VPNOnly
		dec := json.NewDecoder(bytes.NewReader(in.VPN))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&v); err != nil {
			c.block("vpn: %v", err)
		} else {
			c.vpn = &v
			c.note("vpn: VPN-only in V2 too; its devices are the VPN module's peer names, which a V1 device list may not match")
		}
	}
	if present(in.Security) {
		c.convertSecurity(in.Security)
	}
	if present(in.Attack) {
		c.convertAttack(in.Attack)
	}
	if present(in.Notice) {
		var nt struct {
			Theme string `json:"theme"`
		}
		dec := json.NewDecoder(bytes.NewReader(in.Notice))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&nt); err != nil {
			c.block("notice: %v", err)
		} else if key, ok := themeKeys[nt.Theme]; !ok {
			c.block("notice.theme %q is not a V1 design", nt.Theme)
		} else if key != "" {
			c.notice = &manifest.Notice{Theme: key}
		}
	}
	if present(in.Cache) {
		var cr v1Cache
		_ = json.Unmarshal(in.Cache, &cr)
		if cr.Enabled != nil && !*cr.Enabled {
			c.note("cache: written but off; its rules are not carried")
		} else if len(cr.Rules) > 0 {
			c.convertCache(cr)
		}
	}
	if present(in.Chat) {
		var cr struct {
			Reads []string `json:"reads"`
		}
		dec := json.NewDecoder(bytes.NewReader(in.Chat))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cr); err != nil {
			c.block("chat: %v", err)
		} else if len(cr.Reads) > 0 {
			// The site that is read decides in V2 (its manifest's
			// guests), so the loan cannot be carried in this one's.
			c.note("chat.reads: not carried; in V2 each of %s takes this site in from its own manifest (`middleware site guests <that site> add <this site>`)", strings.Join(cr.Reads, ", "))
		}
	}
	if present(in.ClaudeAPI) {
		var ca struct {
			Tools      string  `json:"tools"`
			DailyUSD   float64 `json:"daily_usd"`
			Concurrent int     `json:"concurrent"`
			MaxChats   int     `json:"max_chats"`
			Model      string  `json:"model"`
			HideChats  bool    `json:"hide_chats"`
		}
		dec := json.NewDecoder(bytes.NewReader(in.ClaudeAPI))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&ca); err != nil {
			c.block("claude_api: %v", err)
		} else {
			if ca.Concurrent != 0 {
				// V1 stopped honouring it on 29 September 2026; only the
				// house's capacity limits turns now, in both versions.
				c.note("claude_api.concurrent dropped: V1 no longer applied it either")
			}
			c.claudeAPI = &manifest.ClaudeAPI{Tools: ca.Tools, DailyUSD: ca.DailyUSD, MaxChats: ca.MaxChats, Model: ca.Model, HideChats: ca.HideChats}
		}
	}
}

// serve converts what answers a request.
func (c *converter) serve(in *v1, m *manifest.Manifest, name string) {
	switch in.Kind {
	case "static", "":
		if len(in.Command) > 0 {
			c.block("kind static with a command: V1 never ran it, and V2 would not either")
		}
		if manifest.IsProject(name) {
			// V1 projects are written as static sites with workers.
			if in.Root != "" || in.SPA {
				c.note("root/spa dropped: a project serves nothing")
			}
			return
		}
		root := in.Root
		if root == "" {
			// V1 served public/ by default; V2 would serve the folder
			// itself when public/ is missing, sources included. Written
			// out so the two agree.
			root = "public"
		}
		m.Static = &manifest.Static{Root: root, SPA: in.SPA}
	case "proxy":
		if manifest.IsProject(name) {
			c.block("a project with a process of its own: make it a worker")
			return
		}
		if len(in.Command) == 0 {
			c.block("kind proxy without a command")
			return
		}
		run := &manifest.Run{
			Command:     in.Command,
			Cwd:         in.Dir,
			Env:         in.Env,
			EnvFiles:    in.EnvFiles,
			EnvFromFile: in.EnvFromFile,
			Port:        in.Port,
		}
		h := &manifest.Health{Path: in.Health.Path, ExpectStatus: in.Health.Expect}
		if d, ok := c.duration("health.interval", in.Health.Interval); ok {
			h.Interval = manifest.Duration(d)
		}
		if in.Health.Timeout != "" {
			// V1's timeout is per check; V2's is how long a new
			// generation has to get ready, and a check is bounded on its
			// own. Carrying "8s" over would fail every cold start.
			c.note("health.timeout %s dropped: in V2 it is the start timeout (default 60s)", in.Health.Timeout)
		}
		if h.Path != "" || h.Interval != 0 || h.ExpectStatus != 0 {
			run.Health = h
		}
		if in.Root != "" {
			c.note("root %q dropped: a proxy site serves its process, not files", in.Root)
		}
		if present(in.Container) {
			// V1 ran the same command inside the image with the folder at
			// its own path; V2's docker runtime does exactly that now.
			var ct struct {
				Image     string  `json:"image"`
				Memory    string  `json:"memory"`
				Pids      int     `json:"pids"`
				CPUs      float64 `json:"cpus"`
				CPUWeight int     `json:"cpu_weight"`
				User      string  `json:"user"`
			}
			dec := json.NewDecoder(bytes.NewReader(in.Container))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&ct); err != nil {
				c.block("container: %v", err)
			} else if ct.Image == "" {
				c.block("container without an image")
			} else {
				run.Runtime = "docker"
				run.Docker = &manifest.Docker{Image: ct.Image, Memory: ct.Memory, Pids: ct.Pids, CPUs: ct.CPUs, CPUWeight: ct.CPUWeight, User: ct.User}
			}
		}
		m.Run = run
	default:
		c.block("kind %q is not static or proxy", in.Kind)
	}
}

func (c *converter) duration(field, s string) (time.Duration, bool) {
	if s == "" {
		return 0, false
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		c.block("%s %q is not a duration", field, s)
		return 0, false
	}
	return d, true
}

// password turns a V1 auth into a V2 hash. The algorithm is the same
// (PBKDF2-SHA256, raw base64), only the framing differs; a plain password
// is hashed so it stops being written in the clear.
func (c *converter) password(where string, a *v1Auth) string {
	if a == nil {
		return ""
	}
	if a.Hash != "" {
		parts := strings.Split(a.Hash, "$")
		if len(parts) != 4 || parts[0] != "pbkdf2" {
			c.block("%s: hash in an unknown format", where)
			return ""
		}
		return "$pbkdf2-sha256$" + parts[1] + "$" + parts[2] + "$" + parts[3]
	}
	if a.Password != "" {
		h, err := access.HashPassword(a.Password)
		if err != nil {
			c.block("%s: %v", where, err)
			return ""
		}
		c.note("%s: the password was written in the clear; it is stored hashed now", where)
		return h
	}
	return ""
}

func (c *converter) access(in *v1, m *manifest.Manifest) {
	ac := &manifest.Access{}
	if in.Auth != nil {
		ac.Password = c.password("auth", in.Auth)
		ac.Message = in.Auth.Message
	}
	if in.IP != nil {
		ac.Allow, ac.Deny = in.IP.Allow, in.IP.Deny
	}
	for _, rt := range in.Routes {
		if rt.Path == "" || !strings.HasPrefix(rt.Path, "/") {
			c.block("routes: %q is not a path", rt.Path)
			continue
		}
		ra := &manifest.RouteAccess{Note: rt.Note}
		if rt.Auth != nil {
			ra.Password = c.password("routes "+rt.Path, rt.Auth)
			ra.Message = rt.Auth.Message
		}
		if rt.IP != nil {
			ra.Allow, ra.Deny = rt.IP.Allow, rt.IP.Deny
		}
		if ac.Routes == nil {
			ac.Routes = map[string]*manifest.RouteAccess{}
		}
		if _, dup := ac.Routes[rt.Path]; dup {
			c.block("routes: %s written twice", rt.Path)
		}
		ac.Routes[rt.Path] = ra
	}
	if ac.Password != "" || len(ac.Allow) > 0 || len(ac.Deny) > 0 || len(ac.Routes) > 0 {
		m.Access = ac
	}
}

func (c *converter) workers(in *v1, m *manifest.Manifest) {
	for _, w := range in.Workers {
		if m.Workers == nil {
			m.Workers = map[string]*manifest.Worker{}
		}
		if _, dup := m.Workers[w.Name]; dup {
			c.block("workers: %q written twice", w.Name)
			continue
		}
		m.Workers[w.Name] = &manifest.Worker{
			Command: w.Command, Cwd: w.Dir, Port: w.Port, Env: w.Env,
			EnvFiles: w.EnvFiles, EnvFromFile: w.EnvFromFile,
			Disabled: w.Disabled, Note: w.Note,
		}
	}
}

func (c *converter) hold(in *v1, m *manifest.Manifest) {
	if in.Hold == nil {
		return
	}
	mode := in.Hold.Mode
	switch mode {
	case "", "mantenimiento":
		mode = ""
	case "404", "410", "500":
	default:
		c.block("hold mode %q is not mantenimiento, 404, 410 or 500", mode)
		return
	}
	m.Hold = &manifest.Hold{Enabled: true, Message: in.Hold.Message, Mode: mode, Until: in.Hold.Until}
}

// Summary counts blockers across results, most common first: the order in
// which closing them migrates the most sites.
func Summary(results []Result) []string {
	count := map[string]int{}
	for _, r := range results {
		seen := map[string]bool{}
		for _, b := range r.Blockers {
			key := b
			if i := strings.IndexAny(b, " :("); i > 0 {
				key = b[:i]
			}
			if !seen[key] {
				seen[key] = true
				count[key]++
			}
		}
	}
	keys := make([]string, 0, len(count))
	for k := range count {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if count[keys[i]] != count[keys[j]] {
			return count[keys[i]] > count[keys[j]]
		}
		return keys[i] < keys[j]
	})
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = fmt.Sprintf("%-14s %d site(s)", k, count[k])
	}
	return out
}

// v1Security is V1's `security`, which V2 reads with the same lists and
// two differences: V1 wrote "" for a request without a User-Agent in the
// browser lists (V2 says "none"), and its robot window was always Madrid
// time (V2 names the zone).
type v1Security struct {
	Countries *security.List `json:"countries"`
	Browsers  *security.List `json:"browsers"`
	Bots      *struct {
		Deny   []string `json:"deny"`
		Except []string `json:"except"`
		NoUA   bool     `json:"no_ua"`
		Window *struct {
			From string `json:"from"`
			To   string `json:"to"`
		} `json:"window"`
	} `json:"bots"`
	Agents  *security.List `json:"agents"`
	Paths   []string       `json:"paths"`
	Message string         `json:"message"`
}

func (c *converter) convertSecurity(raw json.RawMessage) {
	var in v1Security
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		c.block("security: %v", err)
		return
	}
	out := &security.Rules{Countries: in.Countries, Agents: in.Agents, Paths: in.Paths, Message: in.Message}
	if b := in.Browsers; b != nil {
		none := func(list []string) []string {
			var o []string
			for _, k := range list {
				if strings.TrimSpace(k) == "" {
					k = security.BrowserNone
				}
				o = append(o, k)
			}
			return o
		}
		out.Browsers = &security.List{Allow: none(b.Allow), Deny: none(b.Deny)}
	}
	if b := in.Bots; b != nil {
		out.Bots = &security.Bots{Deny: b.Deny, Except: b.Except, NoUA: b.NoUA}
		if w := b.Window; w != nil && (strings.TrimSpace(w.From) != "" || strings.TrimSpace(w.To) != "") {
			out.Bots.Window = &security.Window{From: w.From, To: w.To, Zone: "Europe/Madrid"}
		}
	}
	if _, err := security.Compile(out); err != nil {
		c.block("security.%v", err)
		return
	}
	c.security = out
}

// convertAttack carries V1's `attack` whole, on or off: a block written
// and off is a difficulty and exemptions someone chose, ready for the day
// it is switched on. V1 also accepted a short "YYYY-MM-DD HH:MM" until in
// the machine's time; V2 stores RFC 3339. A deadline already gone is
// carried as off, which is what V1's engine would have written.
func (c *converter) convertAttack(raw json.RawMessage) {
	var in struct {
		On      bool     `json:"on"`
		Bits    int      `json:"bits"`
		Life    string   `json:"life"`
		Skip    []string `json:"skip"`
		Until   string   `json:"until"`
		Message string   `json:"message"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		c.block("attack: %v", err)
		return
	}
	out := &attack.Rules{On: in.On, Bits: in.Bits, Life: in.Life, Skip: in.Skip, Message: in.Message}
	if u := strings.TrimSpace(in.Until); u != "" {
		t, err := time.Parse(time.RFC3339, u)
		if err != nil {
			t, err = time.ParseInLocation("2006-01-02 15:04", u, time.Local)
		}
		if err != nil {
			c.block("attack.until: %q is not a date", in.Until)
			return
		}
		if t.Before(time.Now()) {
			out.On = false
			c.note("attack: its deadline %s has passed, carried as off", u)
		} else {
			out.Until = t.Format(time.RFC3339)
		}
	}
	if err := out.Validate(); err != nil {
		c.block("attack.%v", err)
		return
	}
	c.attack = out
}
