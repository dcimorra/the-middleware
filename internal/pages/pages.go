// Package pages renders what a visitor sees when the site itself cannot
// answer: starting, not answering, stopped, held, protected, unknown.
//
// These pages are the only thing some people ever see of a site, so they
// come in several designs (themes), and a site picks one in its manifest
// (`notice.theme`). What does not change between designs is what they say:
// the state (Label), which site this is (Title, the site's name), what to
// do (Line), and the domain and code at the foot. A design that drops one
// of those is not another design, it is a worse page.
//
// Every design is one file embedded in the binary and asks nothing of the
// network: they are served exactly when something is broken, so they
// cannot depend on a font or a stylesheet loading. Each tells the five
// signals apart without animation too, and each styles the same few class
// names that Extra uses (.password, .wrong, .when), because a design that
// does not style .password is one where nobody can type the password.
//
// Whoever asked for JSON gets JSON: an API client behind this proxy getting
// an HTML page where it expected data is a bug report that costs an
// afternoon.
package pages

import (
	"bytes"
	"embed"
	"encoding/json"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/dcimorra/the-middleware/internal/edge"
)

//go:embed themes/*.html
var themeFS embed.FS

// Theme is one design as the panel offers it.
type Theme struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	Note string `json:"note"`
}

// DefaultTheme is what a site gets when nobody chose: the design every
// page had before there were others.
const DefaultTheme = "sober"

var themes = []Theme{
	{"sober", "Sober", "The name large, a status band across the page, warm paper. The one that has always been served; fits almost anything."},
	{"card", "Card", "A centred card on a tinted field, closer to an app than a notice, the state in a pill on top. For what customers see."},
	{"console", "Console", "Monospaced and dark, the state and code on top. For internal things and work in progress: the machine talking."},
	{"note", "Note", "Almost nothing: a line, a rule and the words. For when the notice must not look designed: a parked domain."},
	{"poster", "Poster", "Flat ink, square edges, the label as a block and the HTTP code huge and faint behind everything. For a brand that speaks in posters."},
	{"blueprint", "Blueprint", "Dark blue grid paper, framing corners instead of a border, the explanation hanging from a leader line. A diagnosis, not an apology."},
	{"glitch", "Glitch", "Black screen, scan lines, the title split in two offset copies like a badly tuned signal. For a game or a hardware project."},
}

var templates = func() map[string]*template.Template {
	out := map[string]*template.Template{}
	for _, t := range themes {
		data, err := themeFS.ReadFile("themes/" + t.Key + ".html")
		if err != nil {
			panic("pages: theme " + t.Key + " has no file: " + err.Error())
		}
		out[t.Key] = template.Must(template.New(t.Key).Parse(string(data)))
	}
	return out
}()

// Themes is the catalogue, so nobody else carries a copy of it.
func Themes() []Theme { return append([]Theme(nil), themes...) }

// IsTheme says whether key is a theme. Empty is one: it means "whatever
// the house chose", which is what almost every site says.
func IsTheme(key string) bool {
	if key == "" {
		return true
	}
	_, ok := templates[key]
	return ok
}

var houseTheme atomic.Value // string

// SetHouseTheme is the design of the pages that belong to no site (an
// unknown host, a closed door) and of sites that did not choose.
func SetHouseTheme(key string) {
	if key == "" || !IsTheme(key) {
		key = DefaultTheme
	}
	houseTheme.Store(key)
}

// HouseTheme is the one in force.
func HouseTheme() string {
	if v, ok := houseTheme.Load().(string); ok && v != "" {
		return v
	}
	return DefaultTheme
}

// Signal is how a page shows the state, in every design.
type Signal string

// The five signals.
const (
	// Waiting: something is already happening; reloading is worth it.
	Waiting Signal = "waiting"
	// Paused: alive but still (held for maintenance).
	Paused Signal = "paused"
	// Broken: it should answer and does not.
	Broken Signal = "broken"
	// Nothing: nobody home (stopped, unknown, gone).
	Nothing Signal = "nothing"
	// Locked: there is a key in the way (a password, an allow list).
	Locked Signal = "locked"
)

// Notice is one page.
type Notice struct {
	Status int
	Signal Signal
	// Label is the state tag, two words.
	Label string
	// Title is the big line: the site's name when it is known, so the
	// visitor recognises the site they meant; otherwise the shortest
	// sentence that says what is going on.
	Title string
	// Line says what is going on and what to do.
	Line string
	// Foot is the domain, the other half of recognising the site.
	Foot string
	// Extra is more HTML this house builds (a password form, the time a
	// site is due back). Never anything from a request or a manifest.
	Extra template.HTML
	// LinkHref and LinkText are the only way out: the site's front page.
	LinkHref, LinkText string
	// Reload makes the page reload itself after so many seconds: only
	// for waits measured in seconds, like a site starting.
	Reload int
	// RetryAfter, in seconds, is the header; 0 sends none.
	RetryAfter int
	// Robots is the X-Robots-Tag; empty sends noindex, since none of
	// these pages is the site's content.
	Robots string
	// Theme is the design; empty is the house's.
	Theme string
	Lang  string
}

// Render writes n.
func Render(w http.ResponseWriter, r *http.Request, n Notice) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	if n.Robots == "" {
		n.Robots = "noindex"
	}
	h.Set("X-Robots-Tag", n.Robots)
	if n.RetryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(n.RetryAfter))
	}
	if n.Signal == "" {
		n.Signal = Broken
	}
	if wantsJSON(r) {
		h.Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(n.Status)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": n.Label, "detail": n.Line, "status": n.Status})
		return
	}
	h.Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(n.Status)
	if r != nil && r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(HTML(n))
}

// HTML renders n without writing headers: for previews in the panel.
func HTML(n Notice) []byte {
	if n.Lang == "" {
		n.Lang = "en"
	}
	if n.Signal == "" {
		n.Signal = Broken
	}
	theme := n.Theme
	if _, ok := templates[theme]; !ok {
		theme = HouseTheme()
	}
	var buf bytes.Buffer
	_ = templates[theme].Execute(&buf, n)
	return buf.Bytes()
}

// wantsJSON is an API client: it asked for JSON and not for HTML.
func wantsJSON(r *http.Request) bool {
	if r == nil {
		return false
	}
	accept := r.Header.Get("Accept")
	return strings.Contains(accept, "application/json") && !strings.Contains(accept, "text/html")
}

// entry is a house page that belongs to no site in particular.
type entry struct {
	signal             Signal
	label, title, line string
	retry              int
}

// Catalog holds the house's pages by key. The words are English and kept
// here, not in the handlers, so a translation touches one place.
var Catalog = map[string]entry{
	"unknown":     {Nothing, "Nothing here", "No site here", "No site answers to this name on this server.", 0},
	"forbidden":   {Locked, "Restricted", "Access restricted", "This address is not allowed to see this site.", 0},
	"ratelimited": {Paused, "Slow down", "Too many requests", "Too many requests from your address. Please wait a moment.", 30},
	"underattack": {Waiting, "Checking", "Checking your browser", "This site is under heavy load. This page reloads by itself in a few seconds.", 5},
	"doorclosed":  {Nothing, "Closed entrance", "This entrance is closed", "The site is served through another door. If you are its operator, open this one in Inbound.", 0},
	"toolarge":    {Broken, "Too large", "Request too large", "The request body exceeds what this site accepts.", 0},
	"nocert":      {Waiting, "No certificate yet", "No certificate yet", "A certificate for this name is being requested. Please retry shortly.", 30},
	"unavailable": {Broken, "Unavailable", "Temporarily unavailable", "Please try again shortly.", 30},
}

// Write renders a house page by key, in the house's design.
func Write(w http.ResponseWriter, r *http.Request, key string, status int) {
	e, ok := Catalog[key]
	if !ok {
		e = Catalog["unavailable"]
	}
	n := Notice{Status: status, Signal: e.signal, Label: e.label, Title: e.title, Line: e.line, RetryAfter: e.retry}
	if r != nil {
		n.Foot = edge.Host(r)
	}
	if e.signal == Waiting && e.retry > 0 && e.retry <= 10 {
		n.Reload = e.retry
	}
	Render(w, r, n)
}

// SiteEntry is a page that belongs to one site: its name in the title,
// its domain at the foot, its design.
type SiteEntry struct {
	Status int
	Signal Signal
	Label  string
	Line   string
	Retry  int
	Reload int
	Robots string
}

// SiteCatalog holds a site's pages by key.
var SiteCatalog = map[string]SiteEntry{
	// Starting and crashed are told apart: "starting" is worth a reload
	// in five seconds and says so; "down" is not, and says that instead.
	"starting":  {503, Waiting, "Starting", "The site is starting. This page reloads by itself.", 5, 5, ""},
	"down":      {503, Broken, "Not answering", "The site is not answering right now. Please try again in a few minutes.", 30, 0, ""},
	"stopped":   {503, Nothing, "Switched off", "Its owner has switched it off for now.", 0, 0, "noindex, nofollow"},
	"hold":      {503, Paused, "Maintenance", "It is being worked on and will be back soon.", 300, 0, ""},
	"hold-404":  {404, Nothing, "Not found", "There is nothing at this address.", 0, 0, "noindex, nofollow"},
	"hold-410":  {410, Nothing, "Gone", "This site is no longer here.", 0, 0, "noindex, nofollow"},
	"hold-500":  {500, Broken, "Error", "Something went wrong on this site.", 0, 0, ""},
	"forbidden": {403, Locked, "Restricted", "This address is not allowed to see this site.", 0, 0, ""},
	// The site reached its transfer budget and its owner chose to close
	// it there (limits.network.cut). 509 is nobody's standard and every
	// host's habit for exactly this; it is a 5xx, so a crawler keeps
	// what it had and comes back.
	"overbudget": {509, Paused, "Paused", "This site has used up its data transfer for now. It will be back later.", 3600, 0, ""},
	"password":   {401, Locked, "Private", "This site is private. Enter its password to continue.", 0, 0, "noindex, nofollow"},
	// The security door's. A probe for /.env is told there is nothing,
	// the same page a static site gives for a missing file. The refusals
	// say why in a way the visitor can act on ("not from your country"
	// means reloading will not help) and never what the rule holds; and
	// a refused robot must not carry an index away from here.
	"challenge":       {503, Waiting, "Checking", "Checking your browser before letting you in. It takes a moment and there is nothing to do.", 0, 0, "noindex, nofollow"},
	"notfound":        {404, Nothing, "Not found", "There is nothing at this address.", 0, 0, "noindex, nofollow"},
	"refused-country": {403, Locked, "Closed", "This site cannot be opened from the country you are connecting from.", 0, 0, "noindex, nofollow"},
	"refused-bot":     {403, Locked, "Closed", "This site is closed to robots. If you are a person, something in your browser says you are not.", 0, 0, "noindex, nofollow"},
	"refused-browser": {403, Locked, "Closed", "This site cannot be opened with this browser. Please try another one.", 0, 0, "noindex, nofollow"},
	"refused-agent":   {403, Locked, "Closed", "This site is closed to this program.", 0, 0, "noindex, nofollow"},
}

// ChallengeText is what the under-attack challenge says while it works.
var ChallengeText = map[string]string{
	"working":  "Checking…",
	"done":     "Done, going in…",
	"again":    "That check is no longer valid. Here is another one.",
	"noscript": "This check needs JavaScript. Turn it on and reload the page.",
	"nocookie": "Your browser is not keeping this site's cookie, and without it you cannot get past this page. Allow cookies for this site or leave private mode.",
}

// RobotWindow is the line a refused robot reads when its family is let in
// at certain hours.
func RobotWindow(from, to, zone string) string {
	return "Robots are let in between " + from + " and " + to + " (" + zone + " time)."
}

// ForSite builds a site's page by key. title is the site's name (its
// manifest title, or the domain).
func ForSite(key, title, domain, theme string) Notice {
	e, ok := SiteCatalog[key]
	if !ok {
		e = SiteCatalog["down"]
	}
	if title == "" {
		title = domain
	}
	return Notice{Status: e.Status, Signal: e.Signal, Label: e.Label, Title: title, Line: e.Line, Foot: domain,
		RetryAfter: e.Retry, Reload: e.Reload, Robots: e.Robots, Theme: theme}
}
