package migrate

import (
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/access"
	"github.com/dcimorra/the-middleware/internal/manifest"
)

// v1Hash writes a password the way V1's panel did.
func v1Hash(t *testing.T, pw string) string {
	salt := []byte("0123456789abcdef")
	key, err := pbkdf2.Key(sha256.New, pw, salt, 1000, 32)
	if err != nil {
		t.Fatal(err)
	}
	return "pbkdf2$1000$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(key)
}

func TestAProxySiteKeepsEverythingItHad(t *testing.T) {
	in := `{
	  "title": "Shop", "kind": "proxy", "aliases": ["shop.example.org"],
	  "command": ["node", "server.js"], "dir": "app", "port": 36666,
	  "env": {"X_PORT": "$PORT"}, "env_files": [".env", ".env.local"],
	  "env_from_file": {"NEXT_DIST_DIR": ".next-current"},
	  "health": {"path": "/up", "interval": "30s", "timeout": "8s"},
	  "auth": {"hash": "` + v1Hash(t, "site pw") + `", "message": "ask Ana"},
	  "ip": {"deny": ["203.0.113.0/24"]},
	  "routes": [{"path": "/admin", "note": "back office", "auth": {"password": "zone pw"}, "ip": {"allow": ["10.0.0.0/8"]}}],
	  "workers": [{"name": "api", "command": ["./api"], "dir": "backend", "port": 8090, "note": "fixed"},
	              {"name": "queue", "command": ["./q"], "env_files": [".env"], "disabled": true}],
	  "attack": {"bits": 20}, "needs": ["z4.example.com", "api@shop.example.com"]
	}`
	r := Convert("shop.example.com", []byte(in))
	if !r.Ready() {
		t.Fatalf("blocked: %v", r.Blockers)
	}
	m := r.Manifest
	if m.Title != "Shop" || len(m.Aliases) != 1 || m.Run == nil || m.Run.Cwd != "app" || m.Run.Port != 36666 {
		t.Fatalf("run: %+v", m.Run)
	}
	if strings.Join(m.Run.EnvFiles, ",") != ".env,.env.local" || m.Run.EnvFromFile["NEXT_DIST_DIR"] != ".next-current" || m.Run.Env["X_PORT"] != "$PORT" {
		t.Fatalf("env: %+v", m.Run)
	}
	if m.Run.Health.Path != "/up" || m.Run.Health.Interval.Or(0).String() != "30s" || m.Run.Health.Timeout != 0 {
		t.Fatalf("health: %+v", m.Run.Health)
	}
	if !access.CheckPassword(m.Access.Password, "site pw") || m.Access.Message != "ask Ana" {
		t.Fatal("the V1 site password does not open the V2 site")
	}
	zone := m.Access.Routes["/admin"]
	if zone == nil || !access.CheckPassword(zone.Password, "zone pw") || zone.Note != "back office" || zone.Allow[0] != "10.0.0.0/8" {
		t.Fatalf("zone: %+v", zone)
	}
	if strings.Contains(zone.Password, "zone pw") {
		t.Fatal("a clear password was carried in the clear")
	}
	if m.Access.Deny[0] != "203.0.113.0/24" {
		t.Fatalf("deny: %+v", m.Access)
	}
	if w := m.Workers["api"]; w == nil || w.Port != 8090 || w.Cwd != "backend" || w.Note != "fixed" {
		t.Fatalf("worker api: %+v", w)
	}
	if w := m.Workers["queue"]; w == nil || !w.Disabled || w.Port != 0 {
		t.Fatalf("worker queue: %+v", w)
	}
	if strings.Join(m.Needs, ",") != "z4.example.com,api@shop.example.com" {
		t.Fatalf("needs: %v", m.Needs)
	}
	notes := strings.Join(r.Notes, "\n")
	if a := r.Manifest.Attack; a == nil || a.On || a.Bits != 20 {
		t.Errorf("the attack block written and off was not carried: %+v", a)
	}
	for _, want := range []string{"health.timeout", "in the clear"} {
		if !strings.Contains(notes, want) {
			t.Errorf("no note about %q in:\n%s", want, notes)
		}
	}
}

func TestWhatV2CannotDoYetBlocksTheSite(t *testing.T) {
	cases := map[string]string{
		`{"kind":"static","vpn":{"devices":["phone"],"typo":1}}`:              "vpn",
		`{"kind":"static","attack":{"on":true,"bits":40}}`:                    "attack.bits",
		`{"kind":"static","cache":{"rules":[{"path":"/","mode":"siempre"}]}}`: "not a V1 mode",
		`{"kind":"static","hold":{"mode":"302"}}`:                             "hold mode",
		`{"kind":"static","notice":{"theme":"neon"}}`:                         "notice.theme",
		`{"kind":"static","comand":["typo"]}`:                                 "does not read",
		`{"kind":"cgi"}`:                                                      "kind",
	}
	for in, want := range cases {
		r := Convert("a.example", []byte(in))
		if r.Ready() || r.Manifest != nil {
			t.Errorf("%s converted", in)
			continue
		}
		if !strings.Contains(strings.Join(r.Blockers, "\n"), want) {
			t.Errorf("%s: blockers %v do not mention %q", in, r.Blockers, want)
		}
	}
}

func TestAStaticSiteServesPublicLikeV1(t *testing.T) {
	r := Convert("a.example", []byte(`{"kind":"static","title":"T"}`))
	if !r.Ready() || r.Manifest.Static == nil || r.Manifest.Static.Root != "public" {
		t.Fatalf("%+v", r)
	}
}

func TestAProjectKeepsItsWorkersAndServesNothing(t *testing.T) {
	r := Convert("tools", []byte(`{"kind":"static","root":"public","workers":[{"name":"w","command":["./w"]}]}`))
	if !r.Ready() || r.Manifest.Static != nil || r.Manifest.Workers["w"] == nil {
		t.Fatalf("%+v", r)
	}
	if r := Convert("tools", []byte(`{"kind":"proxy","command":["x"]}`)); r.Ready() {
		t.Fatal("a project with a process of its own was converted")
	}
}

func TestApplyKeepsTheOriginalAndOnlyTouchesReadySites(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		os.MkdirAll(filepath.Join(dir, name, "public"), 0o755)
		os.WriteFile(filepath.Join(dir, name, manifest.FileName), []byte(body), 0o644)
	}
	write("ok.example", `{"kind":"static","title":"ok"}`)
	write("cgi.example", `{"kind":"cgi"}`)
	write("v2.example", `{"version":2}`)
	write("_retirado-x.example-20260101-0000", `{"kind":"static"}`)
	write("out.example", `{"kind":"static","root":"../ok.example"}`)
	results, err := Plan(dir)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Result{}
	for _, r := range results {
		byName[r.Name] = r
	}
	if _, listed := byName["v2.example"]; listed {
		t.Fatal("a site already on V2 was planned")
	}
	if r := byName["_retirado-x.example-20260101-0000"]; r.Skipped == "" {
		t.Fatalf("a retired folder was not reported as skipped: %+v", r)
	}
	if r := byName["out.example"]; r.Ready() {
		t.Fatal("a root outside the site was converted")
	}
	original, _ := os.ReadFile(filepath.Join(dir, "ok.example", manifest.FileName))
	for i := 0; i < 2; i++ {
		written, err := Apply(dir, results)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 && (len(written) != 1 || written[0] != "ok.example") {
			t.Fatalf("written %v", written)
		}
	}
	backup, _ := os.ReadFile(filepath.Join(dir, "ok.example", BackupName))
	if string(backup) != string(original) {
		t.Fatalf("the V1 manifest was not kept as it was: %s", backup)
	}
	m, err := manifest.Load(filepath.Join(dir, "ok.example"))
	if err != nil || m.Title != "ok" {
		t.Fatalf("the converted manifest does not load: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "vpn.example", BackupName)); err == nil {
		t.Fatal("a blocked site was touched")
	}
}

func TestWwwFalseIsCarried(t *testing.T) {
	r := Convert("a.example", []byte(`{"kind":"static","www":false}`))
	if !r.Ready() || r.Manifest.WWW == nil || *r.Manifest.WWW {
		t.Fatalf("%+v", r)
	}
	if h := r.Manifest.Hosts("a.example"); len(h) != 1 {
		t.Fatalf("hosts %v", h)
	}
}

func TestAContainedSiteRunsInDockerWithItsLimits(t *testing.T) {
	r := Convert("a.example", []byte(`{"kind":"proxy","command":["node","s.js"],"container":{"image":"node:22-slim","memory":"512m","pids":200,"cpus":1.5,"cpu_weight":512}}`))
	if !r.Ready() {
		t.Fatalf("%v", r.Blockers)
	}
	run := r.Manifest.Run
	if run.Runtime != "docker" || run.Docker == nil || run.Docker.Image != "node:22-slim" || run.Docker.Memory != "512m" || run.Docker.Pids != 200 || run.Docker.CPUs != 1.5 || run.Docker.CPUWeight != 512 {
		t.Fatalf("%+v %+v", run, run.Docker)
	}
	if r := Convert("a.example", []byte(`{"kind":"proxy","command":["x"],"container":{"image":"i","gpu":true}}`)); r.Ready() {
		t.Fatal("an unknown container setting was dropped silently")
	}
}

func TestTheDesignAndTheHoldModeAreCarried(t *testing.T) {
	r := Convert("a.example", []byte(`{"kind":"static","notice":{"theme":"ficha"},"hold":{"mode":"410","message":"Closed for good","until":"2026-12-01T10:00:00Z"}}`))
	if !r.Ready() {
		t.Fatalf("%v", r.Blockers)
	}
	m := r.Manifest
	if m.Notice == nil || m.Notice.Theme != "card" || m.Hold.Mode != "410" || m.Hold.Until != "2026-12-01T10:00:00Z" || m.Hold.Message != "Closed for good" {
		t.Fatalf("%+v %+v", m.Notice, m.Hold)
	}
}

func TestTheSiteDoorAndTheClaudeTokenAreCarried(t *testing.T) {
	r := Convert("a.example", []byte(`{"kind":"proxy","command":["x"],"claude":true,"claude_api":{"tools":"read","daily_usd":1000,"concurrent":1,"max_chats":20,"model":"sonnet","hide_chats":true}}`))
	if !r.Ready() {
		t.Fatalf("%v", r.Blockers)
	}
	ca := r.Manifest.ClaudeAPI
	if !r.Manifest.Claude || ca == nil || ca.Tools != "read" || ca.DailyUSD != 1000 || ca.MaxChats != 20 || ca.Model != "sonnet" || !ca.HideChats {
		t.Fatalf("%+v %+v", r.Manifest, ca)
	}
	if !strings.Contains(strings.Join(r.Notes, "\n"), "concurrent") {
		t.Fatalf("notes: %v", r.Notes)
	}
}

// V1 lent a folder on the reader's word. V2 does not, so the loan is not
// written into the reader's manifest: a note says where it is given now.
func TestALoanIsNotCarriedInTheReadersManifest(t *testing.T) {
	r := Convert("a.example", []byte(`{"kind":"static","chat":{"reads":["b.example"]}}`))
	if !r.Ready() || r.Manifest.Chat != nil || len(r.Notes) != 1 || !strings.Contains(r.Notes[0], "b.example") || !strings.Contains(r.Notes[0], "site guests") {
		t.Fatalf("%+v", r)
	}
}

func TestTheSecuritySectionIsCarried(t *testing.T) {
	r := Convert("a.example", []byte(`{"kind":"static","security":{"countries":{"deny":["RU"]},"bots":{"deny":["ai"],"except":["Googlebot"],"window":{"from":"00:00","to":"07:00"}},"browsers":{"deny":[""]},"agents":{"deny":["scrapy"]},"paths":["/wp-login.php"],"message":"No."}}`))
	if !r.Ready() {
		t.Fatalf("%v", r.Blockers)
	}
	s := r.Manifest.Security
	if s == nil || s.Countries.Deny[0] != "RU" || s.Bots.Window.Zone != "Europe/Madrid" || s.Browsers.Deny[0] != "none" || s.Paths[0] != "/wp-login.php" || s.Message != "No." {
		t.Fatalf("%+v", s)
	}
	if r := Convert("a.example", []byte(`{"kind":"static","security":{"bots":{"deny":["ia"]}}}`)); r.Ready() {
		t.Fatal("a family V1 would not have accepted either converted")
	}
}

func TestTheAttackBlockIsCarriedWithItsShortDeadline(t *testing.T) {
	r := Convert("a.example", []byte(`{"kind":"static","attack":{"on":true,"bits":18,"life":"1h","skip":["/hooks"],"until":"2099-01-02 03:04"}}`))
	if !r.Ready() {
		t.Fatalf("%v", r.Blockers)
	}
	a := r.Manifest.Attack
	want, _ := time.ParseInLocation("2006-01-02 15:04", "2099-01-02 03:04", time.Local)
	if a == nil || !a.On || a.Bits != 18 || a.Life != "1h" || a.Skip[0] != "/hooks" || a.Until != want.Format(time.RFC3339) {
		t.Fatalf("%+v", a)
	}
	r = Convert("a.example", []byte(`{"kind":"static","attack":{"on":true,"until":"2020-01-01 00:00"}}`))
	if !r.Ready() || r.Manifest.Attack.On || r.Manifest.Attack.Until != "" {
		t.Fatalf("a passed deadline: %+v %v", r.Manifest.Attack, r.Blockers)
	}
}

func TestTheHouseCacheRulesAreCarried(t *testing.T) {
	r := Convert("a.example", []byte(`{"kind":"static","cache":{"max_mb":64,"rules":[{"path":"/product","mode":"origen"},{"path":"/img","mode":"forzar","ttl":3600,"note":"logos"},{"path":"/admin","mode":"nunca"}]}}`))
	if !r.Ready() {
		t.Fatalf("%v", r.Blockers)
	}
	c := r.Manifest.Cache
	if c == nil || c.MaxMB != 64 || len(c.Rules) != 3 || c.Rules[0].Mode != "origin" || c.Rules[1].Mode != "force" || c.Rules[1].TTL != 3600 || c.Rules[2].Mode != "never" {
		t.Fatalf("%+v", c)
	}
}

func TestAVPNOnlySiteIsCarried(t *testing.T) {
	r := Convert("a.example", []byte(`{"kind":"static","vpn":{"devices":["phone"]}}`))
	if !r.Ready() || r.Manifest.VPN == nil || r.Manifest.VPN.Devices[0] != "phone" {
		t.Fatalf("%+v %v", r.Manifest, r.Blockers)
	}
}

func TestAnAppIsDroppedAndSaid(t *testing.T) {
	r := Convert("a.example", []byte(`{"kind":"static","app":"mask"}`))
	if !r.Ready() || !strings.Contains(strings.Join(r.Notes, "\n"), "apps are not part of V2") {
		t.Fatalf("%+v", r)
	}
}
