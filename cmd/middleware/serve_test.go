package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/access"
	"github.com/dcimorra/the-middleware/internal/api"
	"github.com/dcimorra/the-middleware/internal/app"
	"github.com/dcimorra/the-middleware/internal/auth"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/settings"
	"github.com/dcimorra/the-middleware/internal/tasks"
)

// TestServeAnswersEverySectionOfTheAPI builds the process the way `serve`
// does, hooks and all, and asks every section of docs/api.md for its
// listing over the unix socket. It is the one place where the wiring is
// tested by running it: a hook built in the wrong order or a route
// registered on the wrong server shows up here as a 404 or a 500, not in
// a unit test that mounts the section by hand.
func TestServeAnswersEverySectionOfTheAPI(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "data"), 0o755)
	s := settings.Defaults()
	// Port 0, and the real address read back from middleware.started: a
	// port picked free and handed over later can be taken in between by
	// anything else on the machine, and this test then failed now and
	// then with "connection refused".
	s.Listen = "127.0.0.1:0"
	if err := settings.Save(root, s); err != nil {
		t.Fatal(err)
	}
	// A static site with a security section, asked through the listener
	// further down: the door is a hook, and only the wired chain proves it
	// sits where it should.
	shop := filepath.Join(root, "sites", "shop.example")
	os.MkdirAll(shop, 0o755)
	os.WriteFile(filepath.Join(shop, "index.html"), []byte("<p>shop</p>"), 0o644)
	os.WriteFile(filepath.Join(shop, "site.json"), []byte(`{"version":2,"security":{"bots":{"deny":["ai"]},"paths":["/wp-login.php"]}}`), 0o644)
	// And two under attack: one charging, one whose deadline has passed
	// and that the sweep must write down as off.
	for name, attack := range map[string]string{
		"busy.example": `{"on":true,"bits":8,"skip":["/hooks"]}`,
		"past.example": `{"on":true,"bits":10,"until":"2020-01-01T00:00:00Z"}`,
	} {
		dir := filepath.Join(root, "sites", name)
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "index.html"), []byte("<p>"+name+"</p>"), 0o644)
		os.WriteFile(filepath.Join(dir, "site.json"), []byte(`{"version":2,"attack":`+attack+`}`), 0o644)
	}
	// A private site with its pages kept by the house cache: the second
	// visit with the password is a hit, and one without it stays at the
	// door, which is the whole point of the cache being inside it.
	hash, _ := access.HashPassword("secret")
	cached := filepath.Join(root, "sites", "cached.example")
	os.MkdirAll(cached, 0o755)
	os.WriteFile(filepath.Join(cached, "index.html"), []byte("<p>members only</p>"), 0o644)
	os.WriteFile(filepath.Join(cached, "site.json"), []byte(`{"version":2,"access":{"password":"`+hash+`"},"cache":{"rules":[{"path":"/","mode":"force","ttl":60}]}}`), 0o644)
	// A site with the AI gateway open and a task of its own that writes
	// down what it was given: the task must reach the door as the site,
	// whatever its file in tasks/ says.
	// A private site whose name is kept to https: plain http is sent on
	// before the password door is shown, which is the whole point of the
	// redirect sitting in the outermost hook and not in the proxy.
	secure := filepath.Join(root, "sites", "secure.example")
	os.MkdirAll(secure, 0o755)
	os.WriteFile(filepath.Join(secure, "index.html"), []byte("<p>secure</p>"), 0o644)
	os.WriteFile(filepath.Join(secure, "site.json"), []byte(`{"version":2,"access":{"password":"`+hash+`"},"force_https":["secure.example"]}`), 0o644)
	ai := filepath.Join(root, "sites", "ai.example")
	os.MkdirAll(ai, 0o755)
	os.WriteFile(filepath.Join(ai, "index.html"), []byte("<p>ai</p>"), 0o644)
	os.WriteFile(filepath.Join(ai, "site.json"), []byte(`{"version":2,"claude_api":{"tools":"read","daily_usd":2}}`), 0o644)
	os.MkdirAll(filepath.Join(root, "tasks"), 0o755)
	os.WriteFile(filepath.Join(root, "tasks", "ai-writer.json"), []byte(`{"version":2,"schedule":"0 0 1 1 *","site":"ai.example","command":["/bin/sh","-c","echo \"$MIDDLEWARE_API $MIDDLEWARE_TOKEN\" > door.txt"],"env":{"MIDDLEWARE_TOKEN":"forged"}}`), 0o644)
	a, err := app.New(root)
	if err != nil {
		t.Fatal(err)
	}
	a.Hooks = hooks(a)
	if len(a.Hooks) < 9 {
		t.Fatalf("only %d hooks were built", len(a.Hooks))
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("serve did not stop")
		}
	})

	sock := api.SocketPath(root)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the api socket never appeared")
		}
		time.Sleep(50 * time.Millisecond)
	}
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}}
	get := func(path string) (int, string) {
		t.Helper()
		resp, err := client.Get("http://unix" + api.Prefix + path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	sections := []string{"/sites", "/events", "/system", "/modules", "/inbound", "/inbound/certificates", "/waf", "/waf/traffic", "/security/catalog", "/repos", "/push", "/agents", "/agents/capacity", "/chat/middleware", "/tasks", "/guard", "/system/doctor", "/openapi.json"}
	for _, path := range sections {
		code, body := get(path)
		if code != 200 {
			t.Errorf("GET %s: %d %s", path, code, body)
			continue
		}
		var v any
		if err := json.Unmarshal([]byte(body), &v); err != nil {
			t.Errorf("GET %s: not JSON: %v\n%s", path, err, body)
		}
	}

	// The modules the wiring adds are listed, and the module routes
	// resolve through the mount rather than through the catch-all.
	_, body := get("/modules")
	for _, key := range []string{"cloudflare", "github", "vpn", "agent-claude", "agent-agy"} {
		if !strings.Contains(body, `"key": "`+key+`"`) {
			t.Errorf("module %s is not listed:\n%s", key, body)
		}
	}
	if code, _ := get("/modules/vpn/peers"); code != 404 {
		t.Errorf("a disabled module's route must be 404, got %d", code)
	}

	// The request chain: a host nobody serves gets the proxy's answer
	// through inbound → waf → access → traffic, not a crash in one of them.
	// The API socket opens before the loopback listener, so the listener's
	// address is waited for rather than assumed.
	for wait := time.Now().Add(10 * time.Second); ; {
		_, body := get("/events?type=middleware.started")
		var evs []struct {
			Data struct {
				Listen string `json:"listen"`
			} `json:"data"`
		}
		if json.Unmarshal([]byte(body), &evs) == nil && len(evs) > 0 && evs[len(evs)-1].Data.Listen != "" {
			s.Listen = evs[len(evs)-1].Data.Listen
			break
		}
		if time.Now().After(wait) {
			t.Fatalf("middleware.started never said where it listens: %s", body)
		}
		time.Sleep(50 * time.Millisecond)
	}
	resp, err := http.Get("http://" + s.Listen + "/")
	if err != nil {
		t.Fatal(err)
	}
	// The panel answers its shell on this machine's name, not the API's
	// placeholder: a hook that stops installing the UI left every API
	// route working and the console blank, and nothing else here noticed.
	shell, err := http.Get("http://" + s.Listen + "/sites")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(shell.Body)
	shell.Body.Close()
	if !strings.Contains(string(page), `src="/assets/app.js"`) {
		t.Errorf("the panel's shell is not served:\n%.200s", page)
	}
	resp.Body.Close()
	if resp.StatusCode == 0 || resp.StatusCode >= 500 {
		t.Fatalf("unknown host through the chain: %d", resp.StatusCode)
	}

	// A name kept to https, asked over http through the tunnel's door, is
	// sent to https before the door asks for a password; over https it
	// is the door.
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, proto := range []string{"http", "https"} {
		req, _ := http.NewRequest("GET", "http://"+s.Listen+"/a?b=1", nil)
		req.Host = "secure.example"
		req.Header.Set("X-Forwarded-Proto", proto)
		up, err := noFollow.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		up.Body.Close()
		moved := up.StatusCode == 301 && up.Header.Get("Location") == "https://secure.example/a?b=1"
		if moved != (proto == "http") {
			t.Fatalf("force_https over %s: %d %q", proto, up.StatusCode, up.Header.Get("Location"))
		}
	}

	// The site's task gets the site's token at launch, the door answers
	// it as the site, and nothing of it is written to tasks/. Without the
	// block the next run gets nothing.
	runTask := func() []string {
		t.Helper()
		if err := taskScheduler.RunNow("ai-writer"); err != nil {
			log, _ := os.ReadFile(tasks.LogPath(root, "ai-writer"))
			t.Fatalf("task: %v\n%s", err, log)
		}
		data, err := os.ReadFile(filepath.Join(ai, "door.txt"))
		if err != nil {
			t.Fatal(err)
		}
		return strings.Fields(string(data))
	}
	door := runTask()
	if len(door) != 2 || !strings.HasPrefix(door[0], "http://127.0.0.1:") || !strings.HasPrefix(door[1], "ai.example.") {
		t.Fatalf("the task was given %q, not the site's door", door)
	}
	req, _ := http.NewRequest("GET", "http://"+s.Listen+"/api/site", nil)
	req.Header.Set("Authorization", "Bearer "+door[1])
	who, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var site struct {
		Domain string `json:"domain"`
		Claude struct {
			Limits manifest.ClaudeAPI `json:"limits"`
		} `json:"claude"`
	}
	json.NewDecoder(who.Body).Decode(&site)
	who.Body.Close()
	if who.StatusCode != 200 || site.Domain != "ai.example" || site.Claude.Limits.DailyUSD != 2 {
		t.Fatalf("the door answered the task's token with %d %+v", who.StatusCode, site)
	}
	if saved, _ := os.ReadFile(filepath.Join(root, "tasks", "ai-writer.json")); strings.Contains(string(saved), door[1]) {
		t.Fatal("the token was written to tasks/")
	}
	os.WriteFile(filepath.Join(ai, "site.json"), []byte(`{"version":2}`), 0o644)
	if door := runTask(); len(door) != 1 || door[0] != "forged" {
		t.Fatalf("with the block gone the task was still given %q", door)
	}

	// The security door, through the whole chain.
	ask := func(path, ua string) int {
		t.Helper()
		req, _ := http.NewRequest("GET", "http://"+s.Listen+path, nil)
		req.Host = "shop.example"
		req.Header.Set("User-Agent", ua)
		for wait := time.Now().Add(10 * time.Second); ; {
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			// The engine finds the site within a scan; until then the
			// host is unknown.
			if resp.StatusCode != http.StatusNotFound || path != "/" || time.Now().After(wait) {
				return resp.StatusCode
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	browser := "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36"
	if code := ask("/", browser); code != 200 {
		t.Fatalf("a browser on the shop got %d", code)
	}
	if code := ask("/", "Mozilla/5.0 (compatible; GPTBot/1.2; +https://openai.com/gptbot)"); code != 403 {
		t.Errorf("an AI crawler got %d", code)
	}
	for _, p := range []string{"/.git/config", "/a/../.env", "/wp-login.php"} {
		if code := ask(p, browser); code != 404 {
			t.Errorf("%s got %d", p, code)
		}
	}
	// Under attack, after the security door (a probe is still a plain 404
	// and costs nothing) and before the site.
	challenged := func(host, path string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest("GET", "http://"+s.Listen+path, nil)
		req.Host = host
		req.Header.Set("User-Agent", browser)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode, resp.Header.Get("X-Middleware-Challenge")
	}
	if code, mark := challenged("busy.example", "/"); code != 503 || mark != "1" {
		t.Errorf("a site under attack answered %d without the challenge", code)
	}
	if code, _ := challenged("busy.example", "/.env"); code != 404 {
		t.Errorf("a probe on a site under attack got %d", code)
	}
	if code, _ := challenged("busy.example", "/hooks/x"); code == 503 {
		t.Errorf("a skipped path was challenged")
	}
	if code, _ := challenged("past.example", "/"); code != 200 {
		t.Errorf("a passed deadline still charges: %d", code)
	}
	sweepOnce(a, time.Now())
	if m, err := manifest.Load(filepath.Join(root, "sites", "past.example")); err != nil || m.Attack == nil || m.Attack.On || m.Attack.Until != "" || m.Attack.Bits != 10 {
		t.Errorf("the sweep did not write the passed deadline down as off, keeping the rest: %+v %v", m.Attack, err)
	}
	// In through the form, as a visitor does: Basic auth carries
	// Authorization, which the cache never answers.
	form, _ := http.NewRequest("POST", "http://"+s.Listen+access.LoginPath, strings.NewReader("password=secret&next=/"))
	form.Host = "cached.example"
	form.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	lresp, err := noRedirect.Do(form)
	if err != nil {
		t.Fatal(err)
	}
	lresp.Body.Close()
	pass := lresp.Cookies()
	if len(pass) == 0 {
		t.Fatalf("no pass from the login form: %d", lresp.StatusCode)
	}
	visit := func(withPassword bool) (int, string) {
		t.Helper()
		req, _ := http.NewRequest("GET", "http://"+s.Listen+"/", nil)
		req.Host = "cached.example"
		if withPassword {
			for _, c := range pass {
				req.AddCookie(c)
			}
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 200 && !strings.Contains(string(body), "members only") {
			t.Fatalf("200 without the page: %s", body)
		}
		return resp.StatusCode, resp.Header.Get("X-Middleware-Cache")
	}
	visit(true)
	if code, mark := visit(true); code != 200 || mark != "hit" {
		t.Errorf("the second visit with the password: %d %q", code, mark)
	}
	if code, _ := visit(false); code != 401 {
		t.Errorf("a visit without the password got %d: the cache served past the door", code)
	}
	if code, body := get("/sites/shop.example/security/blocks"); code != 200 || !strings.Contains(body, "GPTBot") || !strings.Contains(body, `".git/"`) {
		t.Errorf("the refusals are not listed: %d %s", code, body)
	}

	// The ready event carries what was wired.
	code, body := get("/events?type=middleware.ready")
	if code != 200 || !strings.Contains(body, `"middleware.ready"`) {
		t.Fatalf("middleware.ready was not emitted: %d %s", code, body)
	}

	// `middleware agents` reads the same answer over the socket, and the
	// capacity it prints is the settings' default.
	code, body = get("/agents")
	if code != 200 || !strings.Contains(body, `"max": 8`) || !strings.Contains(body, `"provider": "claude"`) {
		t.Fatalf("agents: %d %s", code, body)
	}
	if err := agentsCmd([]string{"--root", root}); err != nil {
		t.Fatalf("middleware agents: %v", err)
	}

	// docs/api.md and the routes of the whole wired process say the same
	// thing, both ways: a route added without its line breaks this, and so
	// does a line left for a route that is gone. V1 learned that a
	// hand-written reference goes stale within months otherwise.
	_, body = get("/openapi.json")
	var spec struct {
		Paths map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal([]byte(body), &spec); err != nil {
		t.Fatal(err)
	}
	live := map[string]bool{}
	for path, methods := range spec.Paths {
		for m := range methods {
			switch strings.ToUpper(m) {
			case "GET", "POST", "PUT", "PATCH", "DELETE":
				live[strings.ToUpper(m)+" "+strings.TrimPrefix(path, api.Prefix)] = true
			}
		}
	}
	// Every route a token scope opens is still a route: renamed, the scope
	// would quietly stop opening it and a service door would just fail.
	for _, r := range auth.ScopeRoutes() {
		method, path, _ := strings.Cut(r, " ")
		if !live[method+" "+strings.TrimPrefix(path, api.Prefix)] {
			t.Errorf("token scope route %s is not registered", r)
		}
	}
	// Every route of a site is decided for the delegates: opened by a
	// permission or kept for the owner. One added without being placed
	// fails here, rather than being the operator's by accident or open
	// to every delegate by another.
	for r := range live {
		method, path, _ := strings.Cut(r, " ")
		if strings.HasPrefix(path, "/sites/{domain}") && !auth.SiteRoute(method+" "+api.Prefix+path) {
			t.Errorf("site route %s is in no permission and not the owner's (internal/auth/perms.go)", r)
		}
	}
	for _, r := range auth.PermRoutes() {
		method, path, _ := strings.Cut(r, " ")
		if !live[method+" "+strings.TrimPrefix(path, api.Prefix)] {
			t.Errorf("delegate route %s is not registered", r)
		}
	}
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "api.md"))
	if err != nil {
		t.Fatal(err)
	}
	documented := map[string]bool{}
	row := regexp.MustCompile("(?m)^\\| (GET|POST|PUT|PATCH|DELETE) \\| `([^`]+)` \\|")
	for _, m := range row.FindAllStringSubmatch(string(doc), -1) {
		documented[m[1]+" "+m[2]] = true
	}
	var missing, stale []string
	for r := range live {
		if !documented[r] {
			missing = append(missing, r)
		}
	}
	for r := range documented {
		// A module's own routes are served through its mount, which the
		// registry lists as one entry; they are checked by the module.
		if _, path, _ := strings.Cut(r, " "); strings.HasPrefix(path, "/modules/vpn") || strings.HasPrefix(path, "/modules/{agent}") {
			continue
		}
		if !live[r] {
			stale = append(stale, r)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing) > 0 || len(stale) > 0 {
		t.Errorf("docs/api.md is out of step with the routes.\nnot documented:\n  %s\ndocumented but gone:\n  %s", strings.Join(missing, "\n  "), strings.Join(stale, "\n  "))
	}
}

// TestMiddlewareListenOverridesTheSettings is the docker contract: the
// image sets MIDDLEWARE_LISTEN to 0.0.0.0 because a published port never
// reaches the container's own loopback, and the settings file must not
// have to change for that.
func TestMiddlewareListenOverridesTheSettings(t *testing.T) {
	t.Setenv("MIDDLEWARE_LISTEN", "")
	if got := app.ListenAddr("127.0.0.1:46100"); got != "127.0.0.1:46100" {
		t.Fatalf("unset variable must keep the settings, got %q", got)
	}
	t.Setenv("MIDDLEWARE_LISTEN", "0.0.0.0:46100")
	if got := app.ListenAddr("127.0.0.1:46100"); got != "0.0.0.0:46100" {
		t.Fatalf("the variable must win, got %q", got)
	}
}
