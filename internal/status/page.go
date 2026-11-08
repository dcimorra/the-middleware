package status

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"
)

// Public is the part of a report anyone may read: no reasons, which are
// the house's error messages and can name paths and ports.
type Public struct {
	Site      string             `json:"site"`
	State     State              `json:"state"`
	Since     time.Time          `json:"since,omitzero"`
	Uptime    map[string]float64 `json:"uptime"`
	Days      []DayRow           `json:"days"`
	Incidents []PublicIncident   `json:"incidents"`
	Updated   time.Time          `json:"updated"`
}

// PublicIncident is an outage without its reason.
type PublicIncident struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to,omitzero"`
}

// PublicOf strips what is not for visitors.
func PublicOf(site string, rep Report, now time.Time) Public {
	p := Public{Site: site, State: rep.State, Since: rep.Since, Uptime: rep.Uptime, Days: rep.Days, Updated: now}
	for _, in := range rep.Incidents {
		p.Incidents = append(p.Incidents, PublicIncident{From: in.From, To: in.To})
	}
	return p
}

// Serve answers the status page: JSON for ?format=json or a client that
// asks for it, the page otherwise. Not cached for long: a visitor who
// opens it is usually asking whether the site is down right now.
func Serve(w http.ResponseWriter, r *http.Request, title string, p Public) {
	h := w.Header()
	h.Set("Cache-Control", "public, max-age=30")
	h.Set("X-Content-Type-Options", "nosniff")
	if r.URL.Query().Get("format") == "json" || (strings.Contains(r.Header.Get("Accept"), "application/json") && !strings.Contains(r.Header.Get("Accept"), "text/html")) {
		h.Set("Content-Type", "application/json; charset=utf-8")
		h.Set("Access-Control-Allow-Origin", "*")
		_ = json.NewEncoder(w).Encode(p)
		return
	}
	h.Set("Content-Type", "text/html; charset=utf-8")
	// Nothing on the page runs or loads: styles are its own, inline.
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(HTML(title, p))
}

type bar struct {
	X     int
	Class string
	Title string
}

type view struct {
	Title, Site, State, StateClass, Since string
	Uptime                                []struct{ Label, Value string }
	Bars                                  []bar
	Incidents                             []struct {
		When, Duration string
		Open           bool
	}
	Updated, First, Last string
}

// HTML renders the page.
func HTML(title string, p Public) []byte {
	if title == "" {
		title = p.Site
	}
	v := view{Title: title, Site: p.Site, Updated: p.Updated.UTC().Format("2006-01-02 15:04 UTC")}
	switch p.State {
	case Up:
		v.State, v.StateClass = "All systems operational", "up"
	case Down:
		v.State, v.StateClass = "Down", "down"
	case Maintenance:
		v.State, v.StateClass = "Under maintenance", "maint"
	default:
		v.State, v.StateClass = "Not checked yet", "none"
	}
	if !p.Since.IsZero() {
		v.Since = "since " + p.Since.UTC().Format("2006-01-02 15:04 UTC")
	}
	for _, k := range []struct{ key, label string }{{"1", "24 hours"}, {"7", "7 days"}, {"30", "30 days"}, {"90", "90 days"}} {
		v.Uptime = append(v.Uptime, struct{ Label, Value string }{k.label, percent(p.Uptime[k.key])})
	}
	for i, d := range p.Days {
		b := bar{X: i * 6}
		switch {
		case d.Uptime < 0 && d.Maintenance > 0:
			b.Class, b.Title = "maint", d.Date+": maintenance"
		case d.Uptime < 0:
			b.Class, b.Title = "none", d.Date+": no data"
		case d.Uptime >= 0.999:
			b.Class = "up"
		case d.Uptime >= 0.99:
			b.Class = "warn"
		default:
			b.Class = "down"
		}
		if b.Title == "" {
			b.Title = fmt.Sprintf("%s: %s up", d.Date, percent(d.Uptime))
			if d.Down > 0 {
				b.Title += fmt.Sprintf(", %s down", minutes(d.Down))
			}
		}
		v.Bars = append(v.Bars, b)
	}
	if n := len(p.Days); n > 0 {
		v.First, v.Last = p.Days[0].Date, "today"
	}
	for i, in := range p.Incidents {
		if i == 10 {
			break
		}
		row := struct {
			When, Duration string
			Open           bool
		}{When: in.From.UTC().Format("2006-01-02 15:04 UTC")}
		if in.To.IsZero() {
			row.Open, row.Duration = true, "ongoing"
		} else {
			row.Duration = minutes(int(in.To.Sub(in.From).Round(time.Minute) / time.Minute))
		}
		v.Incidents = append(v.Incidents, row)
	}
	var buf bytes.Buffer
	_ = page.Execute(&buf, v)
	return buf.Bytes()
}

func percent(f float64) string {
	if f < 0 {
		return "—"
	}
	if f >= 1 {
		return "100%"
	}
	// Never rounded up to 100: a site that was down for a minute was not
	// up all the time, and 99.99% says so.
	s := fmt.Sprintf("%.2f", f*100)
	if s == "100.00" {
		s = "99.99"
	}
	return strings.TrimRight(strings.TrimRight(s, "0"), ".") + "%"
}

func minutes(n int) string {
	switch {
	case n < 1:
		return "under a minute"
	case n < 60:
		return fmt.Sprintf("%d min", n)
	case n%60 == 0:
		return fmt.Sprintf("%d h", n/60)
	}
	return fmt.Sprintf("%d h %d min", n/60, n%60)
}

var page = template.Must(template.New("status").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}} status</title>
<style>
:root{color-scheme:light dark;--fg:#1b1b1f;--bg:#f6f6f8;--card:#fff;--mute:#6b6b75;--line:#e3e3e8;--up:#1f9d55;--warn:#d69e2e;--down:#d64545;--maint:#3b82f6;--none:#d5d5dc}
@media(prefers-color-scheme:dark){:root{--fg:#e8e8ec;--bg:#121216;--card:#1b1b21;--mute:#9a9aa6;--line:#2c2c34;--none:#3a3a44}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--fg);font:15px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif;padding:32px 16px}
main{max-width:44rem;margin:0 auto}h1{font-size:1.4rem;margin:0 0 16px;font-weight:650;overflow-wrap:anywhere}
.card{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:18px 20px;margin-bottom:16px}
.state{display:flex;align-items:center;gap:12px;font-weight:600;font-size:1.05rem}.state small{display:block;font-weight:400;color:var(--mute);font-size:.85rem}
.dot{width:14px;height:14px;border-radius:50%;flex:none}.dot.up{background:var(--up)}.dot.down{background:var(--down)}.dot.maint{background:var(--maint)}.dot.none{background:var(--none)}
.nums{display:grid;grid-template-columns:repeat(4,1fr);gap:8px;text-align:center}.nums b{display:block;font-size:1.1rem;font-variant-numeric:tabular-nums}.nums span{color:var(--mute);font-size:.8rem}
svg{width:100%;height:40px;display:block}rect.up{fill:var(--up)}rect.warn{fill:var(--warn)}rect.down{fill:var(--down)}rect.maint{fill:var(--maint)}rect.none{fill:var(--none)}
.ends{display:flex;justify-content:space-between;color:var(--mute);font-size:.8rem;margin-top:6px}
h2{font-size:1rem;margin:0 0 10px}ul{list-style:none;margin:0;padding:0}li{display:flex;justify-content:space-between;gap:12px;padding:8px 0;border-top:1px solid var(--line)}li:first-child{border-top:0}
.open{color:var(--down);font-weight:600}.mute{color:var(--mute)}footer{color:var(--mute);font-size:.8rem;text-align:center;margin-top:8px}
@media(max-width:480px){.nums{grid-template-columns:repeat(2,1fr)}}
</style></head>
<body><main>
<h1>{{.Title}}</h1>
<div class="card"><div class="state"><span class="dot {{.StateClass}}"></span><div>{{.State}}{{if .Since}}<small>{{.Since}}</small>{{end}}</div></div></div>
<div class="card"><div class="nums">{{range .Uptime}}<div><b>{{.Value}}</b><span>{{.Label}}</span></div>{{end}}</div></div>
<div class="card"><h2>Uptime, last 90 days</h2>
<svg viewBox="0 0 540 40" preserveAspectRatio="none" role="img" aria-label="Uptime per day">{{range .Bars}}<rect class="{{.Class}}" x="{{.X}}" y="0" width="4" height="40" rx="1"><title>{{.Title}}</title></rect>{{end}}</svg>
<div class="ends"><span>{{.First}}</span><span>{{.Last}}</span></div></div>
<div class="card"><h2>Outages</h2>{{if .Incidents}}<ul>{{range .Incidents}}<li><span>{{.When}}</span><span{{if .Open}} class="open"{{end}}>{{.Duration}}</span></li>{{end}}</ul>{{else}}<p class="mute">No outages in this period.</p>{{end}}</div>
<footer>Measured every minute by the server that serves {{.Site}}. If this page does not load, that server is down. Updated {{.Updated}}.</footer>
</main></body></html>
`))
