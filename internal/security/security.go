// Package security is a site's softest door: it decides by what a caller
// claims to be and where it is. The country is the one the edge writes
// (believed only from a trusted hop, see internal/edge); the User-Agent is
// written by whoever calls and can say anything. So this door guards
// nothing: an address list and a password do that. It clears out what
// identifies itself because it wants to (crawlers eating the bandwidth,
// AI crawlers taking the text), the country that only sends spam, and the
// probes every domain gets for `/.env` and `/.git`.
//
// The four lists (agents, robots, browsers, countries) are independent and
// a request must pass all of them. Inside each, the rule is a firewall's:
// deny first, and an allow list with anything in it means "only these".
// The paths come before all of them and answer 404, not 403: a scanner
// asking for `/.ssh/id_rsa` is told there is nothing, not that there is a
// door.
package security

import (
	"fmt"
	"strings"
	"time"
)

// Rules is the manifest's `security` section.
type Rules struct {
	// Countries are two-letter codes as the edge writes them, plus XX
	// (unknown) and T1 (Tor).
	Countries *List `json:"countries,omitempty"`
	// Browsers are keys of Browsers().
	Browsers *List `json:"browsers,omitempty"`
	// Bots closes robot families (Families()), with exceptions by name.
	Bots *Bots `json:"bots,omitempty"`
	// Agents are raw User-Agent fragments, for whatever is in no table.
	Agents *List `json:"agents,omitempty"`
	// Paths are prefixes this site does not serve, on top of the house
	// list every site gets (BuiltinPaths).
	Paths []string `json:"paths,omitempty"`
	// Message replaces the line a refused visitor reads.
	Message string `json:"message,omitempty"`
}

// List is an allow and a deny list.
type List struct {
	Allow []string `json:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty"`
}

// Bots is what to do with whatever declares itself a robot.
type Bots struct {
	// Deny are the families kept out.
	Deny []string `json:"deny,omitempty"`
	// Except are robots let in anyway, by name ("Googlebot"): closing the
	// search engines but one is an ordinary decision.
	Except []string `json:"except,omitempty"`
	// NoUA closes the door on anything that will not say what it is. A
	// browser always says; what does not is nearly always a hurried script.
	NoUA bool `json:"no_ua,omitempty"`
	// Window is a daily range in which Deny does not apply. An AI crawler
	// is not a scanner: owners want it out by day, when it competes with
	// visitors for the same machine, and do not mind it at 3 am.
	Window *Window `json:"window,omitempty"`
}

// Window is "HH:MM" to "HH:MM" in Zone (an IANA name; empty is the
// machine's time zone, never the visitor's clock). From after To crosses
// midnight; From equal to To is the whole day.
type Window struct {
	From string `json:"from"`
	To   string `json:"to"`
	Zone string `json:"zone,omitempty"`
}

// The reasons a request is turned away. They travel as data; whoever
// shows them to a person words them.
const (
	ByPath    = "path"
	ByAgent   = "agent"
	ByBot     = "bot"
	ByBrowser = "browser"
	ByCountry = "country"
)

// Verdict is what looking at a request gives.
type Verdict struct {
	Pass bool
	// Reason is which list refused it.
	Reason string
	// What is the value that refused it ("RU", "GPTBot", "chrome", a
	// path rule): for the panel only, a visitor is never told the rule.
	What string
}

// Filter is a set of rules read once, consulted on every request.
type Filter struct {
	countryAllow, countryDeny map[string]bool
	browserAllow, browserDeny map[string]bool
	agentAllow, agentDeny     []string
	botDeny, botExcept        map[string]bool
	window                    *window
	noUA                      bool
	paths                     []string
	message                   string
}

// Compile reads a set of rules or says which entry is wrong. It runs when
// the manifest is validated, not when a request comes: a misspelled
// family must be a manifest the engine refuses and says so, not a rule
// that quietly skips itself on the night it was needed. nil rules give a
// filter that still cuts the house's probe paths.
func Compile(r *Rules) (*Filter, error) {
	f := &Filter{}
	if r == nil {
		return f, nil
	}
	f.message = strings.TrimSpace(r.Message)
	var err error
	if r.Countries != nil {
		if f.countryAllow, err = readKeys(r.Countries.Allow, strings.ToUpper, isCountry, "a two-letter country code"); err != nil {
			return nil, fmt.Errorf("countries.allow: %w", err)
		}
		if f.countryDeny, err = readKeys(r.Countries.Deny, strings.ToUpper, isCountry, "a two-letter country code"); err != nil {
			return nil, fmt.Errorf("countries.deny: %w", err)
		}
	}
	if r.Browsers != nil {
		if f.browserAllow, err = readKeys(r.Browsers.Allow, strings.ToLower, isBrowser, "a browser of the catalogue"); err != nil {
			return nil, fmt.Errorf("browsers.allow: %w", err)
		}
		if f.browserDeny, err = readKeys(r.Browsers.Deny, strings.ToLower, isBrowser, "a browser of the catalogue"); err != nil {
			return nil, fmt.Errorf("browsers.deny: %w", err)
		}
	}
	if r.Agents != nil {
		if f.agentAllow, err = readFragments(r.Agents.Allow); err != nil {
			return nil, fmt.Errorf("agents.allow: %w", err)
		}
		if f.agentDeny, err = readFragments(r.Agents.Deny); err != nil {
			return nil, fmt.Errorf("agents.deny: %w", err)
		}
	}
	if f.paths, err = readPaths(r.Paths); err != nil {
		return nil, fmt.Errorf("paths: %w", err)
	}
	if b := r.Bots; b != nil {
		if f.botDeny, err = readKeys(b.Deny, strings.ToLower, isFamily, "a robot family"); err != nil {
			return nil, fmt.Errorf("bots.deny: %w", err)
		}
		f.botExcept = map[string]bool{}
		for _, v := range b.Except {
			if v = strings.ToLower(strings.TrimSpace(v)); v != "" {
				f.botExcept[v] = true
			}
		}
		f.noUA = b.NoUA
		if b.Window != nil {
			if f.window, err = readWindow(b.Window); err != nil {
				return nil, fmt.Errorf("bots.window: %w", err)
			}
		}
	}
	return f, nil
}

func readKeys(list []string, norm func(string) string, valid func(string) bool, what string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, raw := range list {
		v := norm(strings.TrimSpace(raw))
		if v == "" {
			continue
		}
		if !valid(v) {
			return nil, fmt.Errorf("%q is not %s", raw, what)
		}
		out[v] = true
	}
	return out, nil
}

func isCountry(s string) bool {
	if len(s) != 2 {
		return false
	}
	if s == "T1" {
		return true
	}
	return s[0] >= 'A' && s[0] <= 'Z' && s[1] >= 'A' && s[1] <= 'Z'
}

// readFragments reads User-Agent substrings. Two characters at least: one
// matches nearly everything, and a deny list that matches everything is a
// closed site nobody asked for.
func readFragments(list []string) ([]string, error) {
	var out []string
	for _, raw := range list {
		v := strings.ToLower(strings.TrimSpace(raw))
		if v == "" {
			continue
		}
		if len(v) < 2 {
			return nil, fmt.Errorf("%q is too short: it would match nearly everything", raw)
		}
		out = append(out, v)
	}
	return out, nil
}

type window struct {
	from, to         int // minutes of the day
	fromText, toText string
	loc              *time.Location
}

func readWindow(w *Window) (*window, error) {
	from, err := parseHour(w.From)
	if err != nil {
		return nil, err
	}
	to, err := parseHour(w.To)
	if err != nil {
		return nil, err
	}
	loc := time.Local
	if z := strings.TrimSpace(w.Zone); z != "" {
		if loc, err = time.LoadLocation(z); err != nil {
			return nil, fmt.Errorf("zone %q: %w", w.Zone, err)
		}
	}
	return &window{from: from, to: to, fromText: strings.TrimSpace(w.From), toText: strings.TrimSpace(w.To), loc: loc}, nil
}

func parseHour(raw string) (int, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("%q is not a time (HH:MM)", raw)
	}
	return t.Hour()*60 + t.Minute(), nil
}

func (w *window) open(now time.Time) bool {
	t := now.In(w.loc)
	m := t.Hour()*60 + t.Minute()
	switch {
	case w.from == w.to:
		return true
	case w.from < w.to:
		return m >= w.from && m < w.to
	default:
		return m >= w.from || m < w.to
	}
}

// Active says whether the rules do anything beyond the house's paths.
func (f *Filter) Active() bool {
	return f != nil && (len(f.countryAllow)+len(f.countryDeny)+len(f.browserAllow)+len(f.browserDeny)+
		len(f.agentAllow)+len(f.agentDeny)+len(f.botDeny)+len(f.paths) > 0 || f.noUA)
}

// Decide looks at who a request says it is. country is what the edge
// wrote ("" when nothing did); ua is the User-Agent as it came.
//
// The order runs from the most specific rule to the most general, because
// the reason the panel shows comes from it and the useful reason is the
// most specific one that applies.
func (f *Filter) Decide(country, ua string, now time.Time) Verdict {
	if f == nil {
		return Verdict{Pass: true}
	}
	lower := strings.ToLower(ua)
	for _, s := range f.agentDeny {
		if strings.Contains(lower, s) {
			return Verdict{Reason: ByAgent, What: s}
		}
	}
	if len(f.agentAllow) > 0 && !containsAny(lower, f.agentAllow) {
		return Verdict{Reason: ByAgent, What: "not on the allow list"}
	}
	if f.noUA && strings.TrimSpace(ua) == "" {
		return Verdict{Reason: ByAgent, What: "no User-Agent"}
	}
	if len(f.botDeny) > 0 {
		if r, ok := Bot(lower); ok && f.botDeny[r.Family] && !f.botExcept[strings.ToLower(r.Name)] {
			// The window lets the family in during its hours the way an
			// exception lets one name in: by clock instead of by name.
			if f.window == nil || !f.window.open(now) {
				return Verdict{Reason: ByBot, What: r.Name}
			}
		}
	}
	if len(f.browserAllow) > 0 || len(f.browserDeny) > 0 {
		b := Browser(lower)
		if f.browserDeny[b] || (len(f.browserAllow) > 0 && !f.browserAllow[b]) {
			return Verdict{Reason: ByBrowser, What: b}
		}
	}
	// An unknown country does not satisfy an allow list, the same as an
	// unreadable address: that is what makes it an allow list, and also
	// how to lock out everyone if the edge stops sending the header. The
	// panel shows the country of the request it is viewed with for that.
	if len(f.countryAllow) > 0 || len(f.countryDeny) > 0 {
		c := strings.ToUpper(strings.TrimSpace(country))
		if c == "" {
			c = "XX"
		}
		if f.countryDeny[c] || (len(f.countryAllow) > 0 && !f.countryAllow[c]) {
			return Verdict{Reason: ByCountry, What: c}
		}
	}
	return Verdict{Pass: true}
}

func containsAny(s string, subs []string) bool {
	for _, x := range subs {
		if strings.Contains(s, x) {
			return true
		}
	}
	return false
}
