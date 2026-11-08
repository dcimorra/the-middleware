package claude

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dcimorra/the-middleware/internal/agent"
)

// transcript is what a real `claude -p --output-format stream-json
// --verbose` printed for a short turn, trimmed to the fields that matter.
const transcript = `{"type":"system","subtype":"init","cwd":"/x","session_id":"sess-123","tools":["Read","Edit"],"model":"claude-sonnet"}
{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"Let me read the file."},{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/x/README.md"}}]},"session_id":"sess-123"}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"hello"}]},"session_id":"sess-123"}
a stray non-json warning
{"type":"assistant","message":{"id":"m2","role":"assistant","content":[{"type":"text","text":"Done."}]},"session_id":"sess-123"}
{"type":"result","subtype":"success","is_error":false,"duration_ms":1200,"num_turns":2,"result":"Done.","session_id":"sess-123","total_cost_usd":0.0421}
`

// fakeBin writes a script that records its arguments and environment and
// prints the canned transcript, so the adapter is tested end to end
// without the real CLI, a login or a network.
func fakeBin(t *testing.T, dir string) (bin, argsFile, envFile string) {
	t.Helper()
	argsFile = filepath.Join(dir, "args")
	envFile = filepath.Join(dir, "env")
	os.WriteFile(filepath.Join(dir, "transcript.jsonl"), []byte(transcript), 0o644)
	bin = filepath.Join(dir, "claude")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\nenv > " + envFile + "\ncat " + filepath.Join(dir, "transcript.jsonl") + "\n"
	os.WriteFile(bin, []byte(script), 0o755)
	return bin, argsFile, envFile
}

func TestAFakeClaudeTranscriptBecomesEventsWithSessionAndCost(t *testing.T) {
	dir := t.TempDir()
	bin, argsFile, envFile := fakeBin(t, dir)
	site := filepath.Join(dir, "site")
	home := filepath.Join(dir, "home")
	os.MkdirAll(site, 0o755)
	p := New(Config{Bin: bin, APIKey: "sk-test", ExtraArgs: []string{"--extra"}})
	req := agent.Request{
		Workdir: site, Prompt: "hello", Model: "sonnet", SessionID: "prev", System: "You are in a jail.",
		Jail: agent.Jail{SiteDir: site, Home: home, Deny: []string{"/other"}},
	}
	run, err := p.Start(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	evs := agent.Collect(context.Background(), run)
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
	}
	want := "text tool tool_result text text cost result"
	if got := strings.Join(kinds, " "); got != want {
		t.Fatalf("kinds = %q, want %q\n%+v", got, want, evs)
	}
	if evs[3].Text != "a stray non-json warning" {
		t.Fatalf("the non-json line was not passed as text: %+v", evs[2])
	}
	last := evs[len(evs)-1]
	if last.SessionID != "sess-123" || last.Cost != 0.0421 || last.Text != "Done." {
		t.Fatalf("result = %+v", last)
	}
	if evs[1].Tool != "Read" || evs[1].Input["file_path"] != "/x/README.md" || evs[1].SessionID != "sess-123" {
		t.Fatalf("tool = %+v", evs[1])
	}

	args, _ := os.ReadFile(argsFile)
	for _, want := range []string{"-p\nhello\n", "--output-format\nstream-json\n", "--verbose\n", "--permission-mode\nbypassPermissions\n", "--model\nsonnet\n", "--resume\nprev\n", "--add-dir\n" + site + "\n", "--append-system-prompt\nYou are in a jail.\n", "--settings\n", "--extra\n"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("args missing %q:\n%s", want, args)
		}
	}
	env, _ := os.ReadFile(envFile)
	if !strings.Contains(string(env), "ANTHROPIC_API_KEY=sk-test\n") || !strings.Contains(string(env), "HOME="+home+"\n") {
		t.Fatalf("env:\n%s", env)
	}

	// The settings file carries a deny rule for the jailed path.
	var settings map[string]any
	data, err := os.ReadFile(filepath.Join(home, ".middleware", "claude-settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"Read(//other/**)"`) || !strings.Contains(string(data), `"Bash(*/other*)"`) {
		t.Fatalf("settings:\n%s", data)
	}
}

func TestTheAPIKeyIsNotPassedUnlessConfigured(t *testing.T) {
	dir := t.TempDir()
	bin, _, envFile := fakeBin(t, dir)
	t.Setenv("ANTHROPIC_API_KEY", "from-the-supervisor")
	site := filepath.Join(dir, "site")
	os.MkdirAll(site, 0o755)
	p := New(Config{Bin: bin})
	run, err := p.Start(context.Background(), agent.Request{Workdir: site, Prompt: "hi", Jail: agent.Jail{SiteDir: site, Home: filepath.Join(dir, "home")}})
	if err != nil {
		t.Fatal(err)
	}
	agent.Collect(context.Background(), run)
	env, _ := os.ReadFile(envFile)
	if strings.Contains(string(env), "ANTHROPIC_API_KEY") {
		t.Fatalf("the supervisor's key leaked:\n%s", env)
	}
}

func TestAnErrorResultBecomesAnErrorEvent(t *testing.T) {
	p := &parser{}
	evs, done := p.Line(`{"type":"result","subtype":"error_max_turns","is_error":true,"result":"too many turns","session_id":"s","total_cost_usd":0.5}`)
	if !done || len(evs) != 2 || evs[1].Kind != agent.KindError || evs[1].Text != "too many turns" || evs[0].Cost != 0.5 {
		t.Fatalf("events = %+v done=%v", evs, done)
	}
}

func TestAMissingBinaryIsReportedAsUnavailable(t *testing.T) {
	p := New(Config{Bin: "claude-does-not-exist-" + t.Name()})
	if ok, why := p.Available(); ok || !strings.Contains(why, "not found") {
		t.Fatalf("ok=%v why=%q", ok, why)
	}
	if _, err := p.Start(context.Background(), agent.Request{Workdir: t.TempDir(), Prompt: "x"}); err == nil {
		t.Fatal("start succeeded without a binary")
	}
}

// The CLI's own folder is the house's: settings, hooks off, no project
// settings and no MCP server; a login and the sessions a site's HOME held
// come over once, plain files only, and a link planted there is not
// followed out of it.
func TestTheCLIRunsFromTheHousesFolderWithHooksOff(t *testing.T) {
	dir := t.TempDir()
	bin, argsFile, envFile := fakeBin(t, dir)
	root := filepath.Join(dir, "root")
	site := filepath.Join(root, "sites", "a.example")
	home := filepath.Join(root, "data", "homes", "a.example")
	other := filepath.Join(root, "sites", "b.example")
	os.MkdirAll(site, 0o755)
	os.MkdirAll(other, 0o755)
	os.WriteFile(filepath.Join(other, ".env"), []byte("SECRET=b"), 0o600)
	os.MkdirAll(filepath.Join(home, ".claude", "projects", "p"), 0o700)
	os.WriteFile(filepath.Join(home, ".claude", ".credentials.json"), []byte(`{"login":1}`), 0o600)
	os.WriteFile(filepath.Join(home, ".claude", "projects", "p", "s1.jsonl"), []byte("session"), 0o600)
	os.Symlink(other, filepath.Join(home, ".claude", "projects", "other"))
	os.Symlink(filepath.Join(other, ".env"), filepath.Join(home, ".claude", "projects", "p", "leak.jsonl"))
	os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(`{"hooks":{}}`), 0o644)

	jail := agent.NewJail(root, site, home)
	run, err := New(Config{Bin: bin}).Start(context.Background(), agent.Request{Workdir: site, Prompt: "hi", Jail: jail})
	if err != nil {
		t.Fatal(err)
	}
	agent.Collect(context.Background(), run)
	config := filepath.Join(root, "data", "agents", "config", "a.example", "claude")
	env, _ := os.ReadFile(envFile)
	for _, want := range []string{"CLAUDE_CONFIG_DIR=" + config + "\n", "CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD=1\n"} {
		if !strings.Contains(string(env), want) {
			t.Errorf("env missing %q:\n%s", want, env)
		}
	}
	args, _ := os.ReadFile(argsFile)
	settingsFile := filepath.Join(config, "claude-settings.json")
	for _, want := range []string{"--settings\n" + settingsFile + "\n", "--setting-sources\nuser\n", "--strict-mcp-config\n", "--add-dir\n" + site + "\n"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("args missing %q:\n%s", want, args)
		}
	}
	var s map[string]any
	data, _ := os.ReadFile(settingsFile)
	if json.Unmarshal(data, &s) != nil || s["disableAllHooks"] != true {
		t.Fatalf("settings:\n%s", data)
	}
	if got, _ := os.ReadFile(filepath.Join(config, ".credentials.json")); string(got) != `{"login":1}` {
		t.Fatalf("the login did not come over: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(config, "projects", "p", "s1.jsonl")); string(got) != "session" {
		t.Fatalf("the session did not come over: %q", got)
	}
	for _, p := range []string{"projects/other", "projects/p/leak.jsonl", "settings.json"} {
		if _, err := os.Lstat(filepath.Join(config, p)); err == nil {
			t.Errorf("%s came over from the site's HOME", p)
		}
	}
	// The agent's own tools may not reach the folder.
	if !slices.Contains(jail.Deny, filepath.Join(root, "data", "agents")) {
		t.Fatalf("the CLI's folder is not denied: %v", jail.Deny)
	}

	// Once: a login the site writes into its HOME later is not taken.
	os.WriteFile(filepath.Join(home, ".claude", ".credentials.json"), []byte(`{"login":2}`), 0o600)
	os.Remove(filepath.Join(config, ".credentials.json"))
	run, _ = New(Config{Bin: bin}).Start(context.Background(), agent.Request{Workdir: site, Prompt: "hi", Jail: jail})
	agent.Collect(context.Background(), run)
	if _, err := os.Stat(filepath.Join(config, ".credentials.json")); err == nil {
		t.Fatal("the site's HOME was carried over twice")
	}

	// A vendor run through Claude Code gets its own folder and no login.
	run, _ = New(Config{Bin: bin, Key: "deepseek", Env: map[string]string{"ANTHROPIC_BASE_URL": "http://x"}}).Start(context.Background(), agent.Request{Workdir: site, Prompt: "hi", Jail: jail})
	agent.Collect(context.Background(), run)
	vendor := filepath.Join(root, "data", "agents", "config", "a.example", "deepseek")
	if _, err := os.Stat(filepath.Join(vendor, ".credentials.json")); err == nil {
		t.Fatal("a vendor's turn was given the Claude login")
	}
	if _, err := os.Stat(filepath.Join(vendor, "projects", "p", "s1.jsonl")); err != nil {
		t.Fatal("a vendor's sessions did not come over")
	}
}

func TestTheHouseJailDeniesEachSiteInTheSettingsAndNotTheSitesFolder(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"sites/old.test", "data/homes/middleware", "modules"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	j := agent.NewJail(root, root, filepath.Join(root, "data", "homes", "middleware"))
	deny, _ := json.Marshal(Settings(j)["permissions"].(map[string]any)["deny"])
	rules := string(deny)
	if !strings.Contains(rules, "Read(/"+filepath.Join(root, "sites", "old.test")+"/**)") || !strings.Contains(rules, "Bash(*"+filepath.Join(root, "modules")+"*)") {
		t.Fatalf("rules do not name the existing site and the modules:\n%s", rules)
	}
	if strings.Contains(rules, filepath.Join(root, "sites")+"/**") || strings.Contains(rules, filepath.Join(root, "sites")+")") {
		t.Fatalf("rules deny the sites folder itself, so a new site could not be created:\n%s", rules)
	}
}

func TestALentFolderIsClosedToWritingAndAdded(t *testing.T) {
	j := agent.Jail{SiteDir: "/s/a", Lent: []string{"/s/b"}}
	rules, _ := json.Marshal(Settings(j))
	for _, want := range []string{"Edit(//s/b/**)", "Write(//s/b/**)", "Bash(*/s/b*)"} {
		if !strings.Contains(string(rules), want) {
			t.Errorf("missing %s in %s", want, rules)
		}
	}
	if strings.Contains(string(rules), "Read(//s/b") {
		t.Fatal("the lent folder cannot be read")
	}
	args := strings.Join(New(Config{}).Args(agent.Request{Prompt: "x", Jail: j}, ""), " ")
	if !strings.Contains(args, "--add-dir /s/b") {
		t.Fatalf("args: %s", args)
	}
	// In a jail the folder is read-only by the mount: a command may read it.
	agent.Enclose = func(*exec.Cmd, agent.Jail, string) error { return nil }
	defer func() { agent.Enclose = nil }()
	rules, _ = json.Marshal(Settings(j))
	if strings.Contains(string(rules), "Bash(*/s/b*)") || !strings.Contains(string(rules), "Write(//s/b/**)") {
		t.Fatalf("jailed: %s", rules)
	}
}

func TestASubagentsResultAndStateAreRead(t *testing.T) {
	p := &parser{}
	evs, _ := p.Line(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tA","name":"Task","input":{"description":"look","run_in_background":true}}]}}`)
	if len(evs) != 1 || evs[0].ToolID != "tA" || evs[0].Tool != "Task" {
		t.Fatalf("%+v", evs)
	}
	evs, _ = p.Line(`{"type":"user","parent_tool_use_id":"tA","message":{"content":[{"type":"tool_result","tool_use_id":"t9","content":[{"type":"text","text":"line 1"},{"type":"text","text":"line 2"}],"is_error":true}]}}`)
	if len(evs) != 1 || evs[0].Kind != agent.KindToolResult || evs[0].Output != "line 1\nline 2" || !evs[0].IsError || evs[0].Thread != "tA" {
		t.Fatalf("%+v", evs)
	}
	evs, _ = p.Line(`{"type":"system","subtype":"task_notification","tool_use_id":"tA","status":"completed"}`)
	if len(evs) != 1 || evs[0].Kind != agent.KindAgentStatus || evs[0].Status != "completed" || evs[0].ToolID != "tA" {
		t.Fatalf("%+v", evs)
	}
}

func TestARateLimitEventBecomesTheWindowItNames(t *testing.T) {
	p := &parser{}
	evs, _ := p.Line(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":1790000000,"rateLimitType":"seven_day","utilization":0.81},"uuid":"u","session_id":"s1"}`)
	if len(evs) != 1 || evs[0].Kind != agent.KindLimits {
		t.Fatalf("%+v", evs)
	}
	w := evs[0].Limits[0]
	if w.Name != "7d" || w.Used < 80.99 || w.Used > 81.01 || w.Status != "allowed_warning" || w.ResetsAt.Unix() != 1790000000 {
		t.Fatalf("%+v", w)
	}
	evs, _ = p.Line(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","rateLimitType":"five_hour"}}`)
	if len(evs) != 1 || evs[0].Limits[0].Used != -1 {
		t.Fatalf("a window without utilization is not unknown: %+v", evs)
	}
	// The week binds, and the five hours come only in unifiedWindows.
	evs, _ = p.Line(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1790000000,"rateLimitType":"seven_day","unifiedWindows":{"five_hour":{"utilization":0.12,"resetsAt":1789000000},"seven_day":{"utilization":0.4,"resetsAt":1790000000}}}}`)
	ws := evs[0].Limits
	if len(ws) != 2 || ws[0].Name != "5h" || ws[0].Used < 11.99 || ws[0].Used > 12.01 || ws[0].ResetsAt.Unix() != 1789000000 || ws[1].Name != "7d" || ws[1].Used < 39.99 || ws[1].Status != "allowed" {
		t.Fatalf("%+v", ws)
	}
}

func TestClaudesAliasesSayWhatTheyRunAndAVendorsDoNot(t *testing.T) {
	l := New(Config{}).ModelLabels()
	if l["sonnet"] != "claude-sonnet-5-5" || l["opus"] != "claude-opus-5-5" || l["haiku"] == "" {
		t.Fatalf("labels %v", l)
	}
	// DeepSeek through Claude Code: its names are its own, and "sonnet"
	// is not one of them.
	if l := New(Config{Key: "deepseek"}).ModelLabels(); l != nil {
		t.Fatalf("a vendor got Claude's labels: %v", l)
	}
}

// A monitor's turn as print mode writes it: the agent arms it and stops
// talking (a result), the CLI waits, the monitor fires and the agent gets
// another turn (another result), and only then does the CLI exit.
const monitorTurn = `{"type":"system","subtype":"init","session_id":"s1"}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tM","name":"Monitor","input":{"command":"sleep 20; echo DONE"}}]},"session_id":"s1"}
{"type":"system","subtype":"task_started","task_id":"b1","tool_use_id":"tM","task_type":"local_bash","session_id":"s1"}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tM","content":"Monitor started"}]},"session_id":"s1"}
{"type":"assistant","message":{"content":[{"type":"text","text":"I'll tell you when it ends."}]},"session_id":"s1"}
{"type":"result","subtype":"success","result":"I'll tell you when it ends.","session_id":"s1","total_cost_usd":0.1}
`

const monitorFired = `{"type":"system","subtype":"task_notification","task_id":"b1","tool_use_id":"tM","status":"completed","session_id":"s1"}
{"type":"assistant","message":{"content":[{"type":"text","text":"It finished: DONE."}]},"session_id":"s1"}
{"type":"result","subtype":"success","result":"It finished: DONE.","session_id":"s1","total_cost_usd":0.2}
`

// monitorBin prints first, waits longer than the grace a finished run
// gets before its group is killed, then prints then (if any) and exits.
func monitorBin(t *testing.T, first, then string) string {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "first"), []byte(first), 0o644)
	os.WriteFile(filepath.Join(dir, "then"), []byte(then), 0o644)
	bin := filepath.Join(dir, "claude")
	os.WriteFile(bin, []byte("#!/bin/sh\ncat "+filepath.Join(dir, "first")+"\nsleep 4\ncat "+filepath.Join(dir, "then")+"\n"), 0o755)
	return bin
}

func runMonitorBin(t *testing.T, bin string) []agent.Event {
	t.Helper()
	site := t.TempDir()
	run, err := New(Config{Bin: bin}).Start(context.Background(), agent.Request{Workdir: site, Prompt: "watch it", Jail: agent.Jail{SiteDir: site, Home: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	return agent.Collect(context.Background(), run)
}

func TestARunWaitsForItsMonitorAndReadsTheTurnItGives(t *testing.T) {
	evs := runMonitorBin(t, monitorBin(t, monitorTurn, monitorFired))
	var texts []string
	ends := 0
	for _, e := range evs {
		switch e.Kind {
		case agent.KindText:
			texts = append(texts, e.Text)
		case agent.KindResult, agent.KindError:
			ends++
		}
	}
	if !slices.Contains(texts, "It finished: DONE.") {
		t.Fatalf("the turn the monitor gave was lost: %+v", evs)
	}
	last := evs[len(evs)-1]
	if ends != 1 || last.Kind != agent.KindResult || last.Text != "It finished: DONE." || last.Cost != 0.2 {
		t.Fatalf("want one result, the last one: %+v", evs)
	}
}

func TestAResultHeldForAMonitorIsGivenWhenTheCLIExitsWithoutAnother(t *testing.T) {
	evs := runMonitorBin(t, monitorBin(t, monitorTurn, ""))
	last := evs[len(evs)-1]
	if last.Kind != agent.KindResult || last.Text != "I'll tell you when it ends." {
		t.Fatalf("the held result was not given: %+v", evs)
	}
}

func TestABackgroundShellDoesNotHoldTheRun(t *testing.T) {
	p := &parser{}
	p.Line(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tB","name":"Bash","input":{"command":"npm run dev","run_in_background":true}}]}}`)
	p.Line(`{"type":"system","subtype":"task_started","task_id":"b2","tool_use_id":"tB","task_type":"local_bash"}`)
	if _, done := p.Line(`{"type":"result","subtype":"success","result":"Started."}`); !done {
		t.Fatal("a dev server left running would hold the turn for ever")
	}
	// A background subagent does.
	p = &parser{}
	p.Line(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tA","name":"Agent","input":{"run_in_background":true}}]}}`)
	p.Line(`{"type":"system","subtype":"task_started","task_id":"a1","tool_use_id":"tA","task_type":"local_agent"}`)
	if evs, done := p.Line(`{"type":"result","subtype":"success","result":"Launched."}`); done || len(evs) != 1 || evs[0].Kind != agent.KindCost {
		t.Fatalf("a result under a running subagent ended the run: %v %+v", done, evs)
	}
}

// The session's size is the main agent's last call (what it read, from
// the cache or not, plus what it wrote), not a subagent's, and the
// window is the result's: what the autonomous agent rotates its session
// by.
func TestTheResultSaysHowMuchOfTheContextTheSessionHolds(t *testing.T) {
	p := &parser{}
	for _, l := range []string{
		`{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":10,"cache_creation_input_tokens":1000,"cache_read_input_tokens":40000,"output_tokens":200}}}`,
		`{"type":"assistant","parent_tool_use_id":"t1","message":{"content":[{"type":"text","text":"sub"}],"usage":{"input_tokens":5,"cache_read_input_tokens":90000,"output_tokens":1}}}`,
	} {
		p.Line(l)
	}
	evs, done := p.Line(`{"type":"result","subtype":"success","result":"ok","total_cost_usd":0.1,"modelUsage":{"claude-opus-5-5":{"contextWindow":200000},"claude-haiku-4-5":{"contextWindow":100000}}}`)
	if !done || len(evs) != 2 || evs[1].Kind != agent.KindResult {
		t.Fatalf("%+v", evs)
	}
	if evs[1].Context != 41210 || evs[1].Window != 200000 {
		t.Fatalf("context %d of %d, want 41210 of 200000", evs[1].Context, evs[1].Window)
	}
}
