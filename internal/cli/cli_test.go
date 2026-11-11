package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/dcimorra/the-middleware/internal/inbound"
	"github.com/dcimorra/the-middleware/internal/settings"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/api"
	"github.com/dcimorra/the-middleware/internal/auth"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/sites"
	"github.com/dcimorra/the-middleware/internal/tasks"
)

func TestMain(m *testing.M) {
	auth.SetTestCost(1000)
	os.Exit(m.Run())
}

func TestTheTableAlignsColumnsWithoutTrailingSpaces(t *testing.T) {
	var b bytes.Buffer
	Table(&b, []string{"DOMAIN", "MODE", "ALIVE"}, [][]string{
		{"a.test", "run", "yes"},
		{"a-much-longer.example.com", "static", ""},
		{"ü.test", "hold", "no"},
	})
	want := "" +
		"DOMAIN                     MODE    ALIVE\n" +
		"a.test                     run     yes\n" +
		"a-much-longer.example.com  static\n" +
		"ü.test                     hold    no\n"
	if b.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", b.String(), want)
	}
	b.Reset()
	KeyValues(&b, [][2]string{{"domain", "a.test"}, {"handing over", "yes"}})
	if b.String() != "domain        a.test\nhanding over  yes\n" {
		t.Fatalf("key values:\n%s", b.String())
	}
}

func TestHumanHelpersReadWell(t *testing.T) {
	if bytesHuman(512) != "512 B" || bytesHuman(1536) != "1.5 KB" || bytesHuman(3<<30) != "3.0 GB" {
		t.Fatal(bytesHuman(512), bytesHuman(1536), bytesHuman(3<<30))
	}
	if ago(time.Time{}) != "never" || ago(time.Now().Add(-90*time.Minute)) != "1h ago" {
		t.Fatal(ago(time.Now().Add(-90 * time.Minute)))
	}
}

// realServer runs the api on a unix socket in a temp root, with the
// operator set up, so the commands below talk to the real thing.
func realServer(t *testing.T) (root string, cl *Client) {
	t.Helper()
	root = t.TempDir()
	os.MkdirAll(filepath.Join(root, "sites"), 0o755)
	bus := events.New("")
	cur := settings.Defaults()
	srv := api.New(api.Options{Root: root, Version: "test", Auth: auth.New(root, bus), Sites: sites.New(root, nil, bus), Bus: bus,
		Settings: func() settings.Settings { return cur },
		SaveSettings: func(s settings.Settings) error {
			cur = s
			return settings.Save(root, s)
		}})
	srv.Heartbeat = 50 * time.Millisecond
	srv.RegisterTasks(api.TasksOptions{Scheduler: tasks.NewScheduler(tasks.Options{Root: root, Bus: bus}), Root: root})
	sock := api.SocketPath(root)
	ln, err := api.ListenSocket(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	hs := &http.Server{Handler: srv.Panel(), BaseContext: func(net.Listener) context.Context { return auth.WithSocket(ctx) }}
	go hs.Serve(ln)
	t.Cleanup(func() { cancel(); hs.Close() })
	go func() {
		time.Sleep(100 * time.Millisecond)
		bus.Emit(events.Event{Type: "test.tick", Subject: "a.test", Message: "tick"})
	}()
	return root, SocketClient(sock)
}

func run(t *testing.T, cl *Client, args ...string) (string, error) {
	t.Helper()
	return runIn(t, cl, "", args...)
}

// runIn is run with stdin.
func runIn(t *testing.T, cl *Client, stdin string, args ...string) (string, error) {
	t.Helper()
	var out, errw bytes.Buffer
	e := &Env{Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errw, Connect: func(string) (*Client, error) { return cl, nil }}
	err := e.Run(args[0], args[1:])
	return out.String() + errw.String(), err
}

func TestTheSocketClientTalksToARealServer(t *testing.T) {
	_, cl := realServer(t)
	var me map[string]any
	if err := cl.Do("GET", "/api/v1/auth/me", nil, &me); err != nil {
		t.Fatal(err)
	}
	if me["via"] != "socket" {
		t.Fatalf("me: %v", me)
	}
	err := cl.Do("GET", "/api/v1/sites/nope.test", nil, nil)
	var apiErr *APIError
	if !errorsAs(err, &apiErr) || apiErr.Status != 404 {
		t.Fatalf("missing site: %v", err)
	}
	if err := SocketClient(filepath.Join(t.TempDir(), "none.sock")).Do("GET", "/api/v1/system", nil, nil); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("dead socket: %v", err)
	}
}

func errorsAs(err error, target **APIError) bool {
	for err != nil {
		if e, ok := err.(*APIError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func TestSiteCommandsRoundTripThroughTheSocket(t *testing.T) {
	_, cl := realServer(t)
	out, err := run(t, cl, "site", "add", "hello.test", "--template", "node")
	if err != nil || !strings.Contains(out, "Created hello.test") {
		t.Fatalf("add: %q %v", out, err)
	}
	out, err = run(t, cl, "site", "list")
	if err != nil || !strings.HasPrefix(out, "DOMAIN") || !strings.Contains(out, "hello.test  run   no") {
		t.Fatalf("list:\n%s%v", out, err)
	}
	out, err = run(t, cl, "site", "list", "--json")
	var list []sites.Site
	if err != nil || json.Unmarshal([]byte(out), &list) != nil || len(list) != 1 {
		t.Fatalf("list --json: %q %v", out, err)
	}
	out, err = run(t, cl, "site", "show", "hello.test")
	if err != nil || !strings.Contains(out, "mode     run") || !strings.Contains(out, `"npm"`) {
		t.Fatalf("show:\n%s%v", out, err)
	}
	out, err = run(t, cl, "site", "stop", "hello.test")
	if err != nil || !strings.Contains(out, "Stopped") {
		t.Fatalf("stop: %q %v", out, err)
	}
	out, _ = run(t, cl, "site", "list")
	if !strings.Contains(out, "hello.test  disabled") {
		t.Fatalf("after stop:\n%s", out)
	}
	if out, err = run(t, cl, "site", "hold", "hello.test", "--title", "Soon"); err != nil {
		t.Fatalf("hold: %q %v", out, err)
	}
	if out, err = run(t, cl, "site", "logs", "hello.test", "--lines", "3"); err != nil {
		t.Fatalf("logs: %q %v", out, err)
	}
	out, err = run(t, cl, "site", "rm", "hello.test", "--archive")
	if err != nil || !strings.Contains(out, "Archived hello.test to") {
		t.Fatalf("rm: %q %v", out, err)
	}
	out, err = run(t, cl, "site", "show", "hello.test")
	if err == nil || !strings.Contains(err.Error(), "no such site") {
		t.Fatalf("show after rm: %q %v", out, err)
	}
	if _, err := run(t, cl, "site", "stop"); err == nil {
		t.Fatal("stop without a domain succeeded")
	}
	if _, err := run(t, cl, "site", "bogus"); err == nil {
		t.Fatal("unknown subcommand succeeded")
	}
}

func TestTokenEventsAndSystemCommands(t *testing.T) {
	_, cl := realServer(t)
	out, err := run(t, cl, "token", "add", "ci", "script")
	if err != nil || !strings.Contains(out, "mw_") {
		t.Fatalf("token add: %q %v", out, err)
	}
	out, err = run(t, cl, "token", "list")
	if err != nil || !strings.Contains(out, "ci script") || !strings.Contains(out, "never") {
		t.Fatalf("token list:\n%s%v", out, err)
	}
	id := strings.Fields(strings.Split(out, "\n")[1])[0]
	if out, err = run(t, cl, "token", "rm", id); err != nil || !strings.Contains(out, "revoked") {
		t.Fatalf("token rm: %q %v", out, err)
	}
	out, err = run(t, cl, "token", "add", "mask", "--scope", "restart:a.example", "--scope", "read")
	if err != nil || !strings.Contains(out, "mw_") {
		t.Fatalf("token add --scope: %q %v", out, err)
	}
	out, err = run(t, cl, "token", "list")
	if err != nil || !strings.Contains(out, "restart:a.example,read") {
		t.Fatalf("token list does not show the scopes:\n%s%v", out, err)
	}
	if _, err = run(t, cl, "token", "add", "bad", "--scope", "reboot"); err == nil {
		t.Fatal("a scope outside the list was accepted")
	}
	out, err = run(t, cl, "system")
	if err != nil || !strings.Contains(out, "version  test") || !strings.Contains(out, "via      socket") {
		t.Fatalf("system:\n%s%v", out, err)
	}
	// The tick is emitted in the background a moment after the server
	// starts; on a loaded machine it can come after these commands.
	for wait := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		out, err = run(t, cl, "events", "--type", "test.")
		if err == nil && strings.Contains(out, "test.tick  a.test") {
			break
		}
		if time.Now().After(wait) {
			t.Fatalf("events:\n%s%v", out, err)
		}
	}
	out, err = run(t, cl, "events", "--since", "1h", "--json")
	if err != nil || !strings.Contains(out, `"test.tick"`) {
		t.Fatalf("events --json: %q %v", out, err)
	}
}

func TestSetupPrintsACodeThatTheStoreAccepts(t *testing.T) {
	root := t.TempDir()
	var out bytes.Buffer
	e := &Env{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &out, Connect: Connect}
	if err := e.Run("setup", []string{"--root", root, "--json"}); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || got.Code == "" {
		t.Fatalf("setup output %q: %v", out.String(), err)
	}
	store := auth.New(root, nil)
	if err := store.Setup(got.Code, "password-1"); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	e.Stdin = strings.NewReader("password-2\npassword-2\n")
	if err := e.Run("setup", []string{"--password", "--root", root}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Login("password-2", "127.0.0.1"); err != nil {
		t.Fatalf("the password set from the terminal does not log in: %v", err)
	}
	e.Stdin = strings.NewReader("one-password\nother-password\n")
	if err := e.Run("setup", []string{"--password", "--root", root}); err == nil {
		t.Fatal("mismatched passwords accepted")
	}
}

func TestFlagsMayFollowPositionalArguments(t *testing.T) {
	_, cl := realServer(t)
	out, err := run(t, cl, "site", "add", "x.test", "--template", "static")
	if err != nil {
		t.Fatalf("%q %v", out, err)
	}
	out, err = run(t, cl, "site", "show", "x.test", "--json")
	if err != nil || !strings.HasPrefix(out, "{") {
		t.Fatalf("show --json after the domain: %q %v", out, err)
	}
}

func TestSetupWithADomainGivesThePanelItsNameAndOpensTheDoor(t *testing.T) {
	// Through a running supervisor: the settings change where it reads
	// them, not only on disk.
	root, cl := realServer(t)
	out, err := run(t, cl, "setup", "--root", root, "--domain", "Panel.Example.com")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var st map[string]any
	if err := cl.Do("GET", "/api/v1/settings", nil, &st); err != nil || st["panel_domain"] != "panel.example.com" {
		t.Fatalf("settings: %v %v", st, err)
	}
	doors, _ := inbound.LoadState(root)
	if !doors.Ports.Open {
		t.Fatal("the ports door was not opened")
	}
	if !strings.Contains(out, "Setup code:") || !strings.Contains(out, "https://panel.example.com/") || !strings.Contains(out, "DNS:") {
		t.Fatalf("output:\n%s", out)
	}
	// With the operator there, only the name: no new code lying around.
	auth.New(root, nil).SetPassword("password-1")
	out, _ = run(t, cl, "setup", "--root", root, "--domain", "panel2.example.com")
	if strings.Contains(out, "Setup code:") || !strings.Contains(out, "https://panel2.example.com/") {
		t.Fatalf("with an operator:\n%s", out)
	}
	if _, err := run(t, cl, "setup", "--root", root, "--domain", "not a domain"); err == nil {
		t.Fatal("a bad name was accepted")
	}
}

func TestSetupWithADomainAndNoSupervisorWritesTheFiles(t *testing.T) {
	root := t.TempDir()
	var out bytes.Buffer
	e := &Env{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &out, Connect: func(string) (*Client, error) { return nil, errors.New("no socket") }}
	if err := e.Run("setup", []string{"--root", root, "--domain", "panel.example.com"}); err != nil {
		t.Fatal(err)
	}
	st, _ := settings.Load(root)
	doors, _ := inbound.LoadState(root)
	if st.PanelDomain != "panel.example.com" || !doors.Ports.Open || doors.NewSites != inbound.Ports {
		t.Fatalf("settings %q, ports open %v, new sites %q", st.PanelDomain, doors.Ports.Open, doors.NewSites)
	}
}

func TestThePanelOnTheTunnelDoorOpensNoPort(t *testing.T) {
	root := t.TempDir()
	var out bytes.Buffer
	e := &Env{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &out, Connect: func(string) (*Client, error) { return nil, errors.New("no socket") }}
	if err := e.Run("setup", []string{"--root", root, "--domain", "panel.example.com", "--door", "tunnel"}); err != nil {
		t.Fatal(err)
	}
	doors, _ := inbound.LoadState(root)
	if doors.Ports.Open || !doors.Tunnel.Open || doors.NewSites != inbound.Tunnel {
		t.Fatalf("doors: ports %v, tunnel %v, new sites %q", doors.Ports.Open, doors.Tunnel.Open, doors.NewSites)
	}
	if !strings.Contains(out.String(), "ssh -L") || strings.Contains(out.String(), "DNS:") {
		t.Fatalf("output:\n%s", out.String())
	}
	if err := e.Run("setup", []string{"--root", root, "--domain", "panel.example.com", "--door", "local"}); err == nil {
		t.Fatal("the local door was accepted for the panel")
	}
}

// setup --join enters the network through the running service, with the
// endpoint the house suggests when none is given, and says where the node
// is managed from.
func TestSetupJoinEntersTheNetworkThroughTheService(t *testing.T) {
	var got map[string]string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/nodes", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"suggested_endpoint":"203.0.113.9:51821"}`))
	})
	mux.HandleFunc("POST /api/v1/nodes/join", func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"ok":true,"inviter":"alpha","members":2}`))
	})
	var asked []string
	mux.HandleFunc("POST /api/v1/inbound/doors/{door}", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Open bool `json:"open"`
		}
		json.NewDecoder(r.Body).Decode(&b)
		asked = append(asked, fmt.Sprintf("door %s %v", r.PathValue("door"), b.Open))
		w.Write([]byte(`{}`))
	})
	mux.HandleFunc("PUT /api/v1/inbound/publishing", func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		asked = append(asked, fmt.Sprintf("new sites %v", b["new_sites"]))
		w.Write([]byte(`{}`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()
	cl := &Client{Base: ts.URL, HTTP: ts.Client(), Via: ts.URL}
	root := t.TempDir()
	out, err := run(t, cl, "setup", "--root", root, "--join", "CODE123")
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if got["code"] != "CODE123" || got["endpoint"] != "203.0.113.9:51821" {
		t.Fatalf("join asked with %v", got)
	}
	if !strings.Contains(out, "network of alpha") || !strings.Contains(out, "no operator of its own") {
		t.Fatalf("output: %s", out)
	}
	// A node with a public address and no door chosen gets 80 and 443,
	// and its new sites go there: it has no tunnel to be published on.
	if want := []string{"door ports true", "new sites ports"}; !slices.Equal(asked, want) {
		t.Fatalf("after joining, asked %v, want %v", asked, want)
	}
	if !strings.Contains(out, "Ports 80 and 443 are open") {
		t.Fatalf("output: %s", out)
	}
}

// A node that joins keeps its doors as they are when 80 and 443 would
// reach nothing (behind a NAT, no endpoint) or when a door was already
// chosen on it: joining is not a second opinion on the Inbound screen.
func TestSetupJoinLeavesTheDoorsWhenThereIsNothingToDecide(t *testing.T) {
	for name, tc := range map[string]struct {
		suggested string
		prepare   func(t *testing.T, root string)
	}{
		"behind a nat": {"", func(*testing.T, string) {}},
		"door already chosen": {"203.0.113.9:51821", func(t *testing.T, root string) {
			st := inbound.DefaultState()
			st.NewSites = inbound.Tunnel
			if err := os.MkdirAll(filepath.Join(root, "data"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := inbound.SaveState(root, st); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			var asked []string
			mux := http.NewServeMux()
			mux.HandleFunc("GET /api/v1/nodes", func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"suggested_endpoint":%q}`, tc.suggested)
			})
			mux.HandleFunc("POST /api/v1/nodes/join", func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(`{"ok":true,"inviter":"alpha","members":2}`))
			})
			mux.HandleFunc("/api/v1/inbound/", func(w http.ResponseWriter, r *http.Request) {
				asked = append(asked, r.Method+" "+r.URL.Path)
				w.Write([]byte(`{}`))
			})
			ts := httptest.NewServer(mux)
			defer ts.Close()
			cl := &Client{Base: ts.URL, HTTP: ts.Client(), Via: ts.URL}
			root := t.TempDir()
			tc.prepare(t, root)
			out, err := run(t, cl, "setup", "--root", root, "--join", "CODE123")
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			if len(asked) != 0 || strings.Contains(out, "Ports 80") {
				t.Fatalf("the doors were touched: %v\n%s", asked, out)
			}
		})
	}
}
