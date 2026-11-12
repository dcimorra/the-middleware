package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dcimorra/the-middleware/internal/agent"
)

func parse(text string) []agent.Event {
	p := &parser{}
	var out []agent.Event
	for _, l := range strings.Split(text, "\n") {
		evs, done := p.Line(l)
		out = append(out, evs...)
		if done {
			return out
		}
	}
	return append(out, p.Flush()...)
}

// What Codex 0.163 printed for a wrong key: retries, then turn.failed.
func TestAWrongKeyIsOneErrorNotFiveRetries(t *testing.T) {
	raw, err := os.ReadFile("testdata/bad_key.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	evs := parse(string(raw))
	if len(evs) != 1 || evs[0].Kind != agent.KindError || !strings.Contains(evs[0].Text, "401") || evs[0].SessionID == "" {
		t.Fatalf("%+v", evs)
	}
}

// The shape OpenAI documents for a turn that ran a command and answered.
func TestACommandAndAnAnswer(t *testing.T) {
	evs := parse(`{"type":"thread.started","thread_id":"t1"}
{"type":"turn.started"}
{"type":"item.started","item":{"id":"item_1","type":"command_execution","command":"bash -lc ls","status":"in_progress"}}
{"type":"item.completed","item":{"id":"item_1","type":"command_execution","command":"bash -lc ls","aggregated_output":"docs\nsdk\n","exit_code":0,"status":"completed"}}
{"type":"item.completed","item":{"id":"item_2","type":"file_change","changes":[{"path":"/s/a.txt","kind":"update"}],"status":"completed"}}
{"type":"item.completed","item":{"id":"item_3","type":"agent_message","text":"Repo contains docs and sdk."}}
{"type":"turn.completed","usage":{"input_tokens":2476,"output_tokens":122}}`)
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
	}
	if strings.Join(kinds, ",") != "tool,tool_result,tool,tool_result,text,cost,result" {
		t.Fatalf("%v", kinds)
	}
	if evs[0].Tool != "Bash" || evs[1].Output != "docs\nsdk\n" || evs[2].Input["file_path"] != "/s/a.txt" {
		t.Fatalf("%+v", evs[:3])
	}
	if last := evs[len(evs)-1]; last.Text != "Repo contains docs and sdk." || last.SessionID != "t1" {
		t.Fatalf("%+v", last)
	}
}

func TestAReadOnlyLevelRunsReadOnlyAndAResumeGoesByThread(t *testing.T) {
	p := New(Config{APIKey: "k", Models: []string{"gpt-5.5"}})
	args := strings.Join(p.Args(agent.Request{Prompt: "hola", Tools: []string{"Read"}, SessionID: "t1"}), " ")
	for _, want := range []string{"exec resume --json", `sandbox_mode="read-only"`, "--model gpt-5.5", "t1 hola"} {
		if !strings.Contains(args, want) {
			t.Fatalf("missing %q in %s", want, args)
		}
	}
	if !strings.Contains(strings.Join(p.Args(agent.Request{Prompt: "x"}), " "), `sandbox_mode="workspace-write"`) {
		t.Fatal("a full turn is not workspace-write")
	}
}

func TestASubscriptionRunsThePlansDefaultUnlessAModelIsChosen(t *testing.T) {
	p := New(Config{Login: "{}", Models: []string{"gpt-5.5"}})
	if args := strings.Join(p.Args(agent.Request{Prompt: "x"}), " "); strings.Contains(args, "--model") {
		t.Fatalf("a subscription was given a model nobody chose: %s", args)
	}
	if args := strings.Join(p.Args(agent.Request{Prompt: "x", Model: "gpt-6-sol"}), " "); !strings.Contains(args, "--model gpt-6-sol") {
		t.Fatalf("the chosen model is not passed: %s", args)
	}
}

func TestThePlansModelsAreTheListedOnesInCodexsOrder(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex")
	script := `#!/bin/sh
[ "$1 $2" = "debug models" ] || exit 2
[ -f "$CODEX_HOME/auth.json" ] || exit 3
echo '{"models":[{"slug":"b","visibility":"list","priority":5},{"slug":"hidden","visibility":"hide","priority":0},{"slug":"a","visibility":"list","priority":1}]}'
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ids, err := PlanModels(context.Background(), bin, "", filepath.Join(dir, "home"), `{"tokens":{}}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(ids, ",") != "a,b" {
		t.Fatalf("%v", ids)
	}
}

func TestTheDoorsPermissionsTurnCodexsShellAndWebOff(t *testing.T) {
	p := New(Config{APIKey: "k"})
	none := strings.Join(p.Args(agent.Request{Prompt: "x", Tools: []string{}}), " ")
	for _, want := range []string{"--disable shell_tool", "--disable view_image", `web_search="disabled"`, `sandbox_mode="read-only"`} {
		if !strings.Contains(none, want) {
			t.Fatalf("no tools: missing %q in %s", want, none)
		}
	}
	read := strings.Join(p.Args(agent.Request{Prompt: "x", Tools: []string{"Read", "Glob", "Grep", "WebSearch", "WebFetch"}}), " ")
	if strings.Contains(read, "shell_tool") || strings.Contains(read, "web_search") || !strings.Contains(read, `sandbox_mode="read-only"`) {
		t.Fatalf("reading with the web: %s", read)
	}
	all := strings.Join(p.Args(agent.Request{Prompt: "x"}), " ")
	if strings.Contains(all, "--disable") || strings.Contains(all, "web_search") {
		t.Fatalf("every tool: %s", all)
	}
	if got := strings.Join(p.Permissions(), ","); got != "read,edit,web" {
		t.Fatalf("permissions: %s", got)
	}
}

// A message longer than an argument can be is read by Codex on its
// standard input ("-"), whole and with the house's text first; a short
// one stays the argument it always was.
func TestALongMessageIsReadFromStandardInput(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex")
	os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+filepath.Join(dir, "args")+"\ncat > "+filepath.Join(dir, "stdin")+"\n"), 0o755)
	site := filepath.Join(dir, "site")
	os.MkdirAll(site, 0o755)
	p := New(Config{Bin: bin, APIKey: "sk-test"})
	if args := p.Args(agent.Request{Prompt: "hola"}); args[len(args)-1] != "hola" {
		t.Fatalf("a short message: args = %v", args)
	}
	message := strings.Repeat("chapter one. ", 40*1024)
	run, err := p.Start(context.Background(), agent.Request{Workdir: site, Prompt: message, System: "The house's rules.", SessionID: "t1", Jail: agent.Jail{SiteDir: site}})
	if err != nil {
		t.Fatal(err)
	}
	agent.Collect(context.Background(), run)
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	if !strings.HasSuffix(string(args), "\nt1\n-\n") || len(args) > 4096 {
		t.Fatalf("args (%d bytes) do not end in the session and \"-\"", len(args))
	}
	if in, _ := os.ReadFile(filepath.Join(dir, "stdin")); string(in) != "The house's rules.\n\n---\n\n"+message {
		t.Fatalf("Codex read %d bytes", len(in))
	}
}
