package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/agent"
)

// fakeAnthropic answers every Messages call with "done", streamed when
// asked, and keeps what each request said.
func fakeAnthropic(t *testing.T) (*httptest.Server, func() string) {
	var mu sync.Mutex
	var seen strings.Builder
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen.Write(body)
		mu.Unlock()
		if r.Method != http.MethodPost || !strings.Contains(r.URL.Path, "/messages") {
			http.NotFound(w, r)
			return
		}
		msg := map[string]any{"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-sonnet-4-5", "content": []any{}, "stop_reason": nil, "usage": map[string]int{"input_tokens": 1, "output_tokens": 1}}
		if !strings.Contains(strings.ReplaceAll(string(body), " ", ""), `"stream":true`) {
			msg["content"] = []any{map[string]string{"type": "text", "text": "done"}}
			msg["stop_reason"] = "end_turn"
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(msg)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		ev := func(name string, data any) {
			b, _ := json.Marshal(data)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
		}
		ev("message_start", map[string]any{"type": "message_start", "message": msg})
		ev("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]string{"type": "text", "text": ""}})
		ev("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "text_delta", "text": "done"}})
		ev("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		ev("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 1}})
		ev("message_stop", map[string]any{"type": "message_stop"})
	}))
	t.Cleanup(srv.Close)
	return srv, func() string { mu.Lock(); defer mu.Unlock(); return seen.String() }
}

// With CLAUDE_BIN, the real CLI runs a turn as the house starts one, in a
// site whose HOME and folder are full of what a container or a push can
// plant: hooks in both settings files, an apiKeyHelper and an MCP server
// in the project. None of it runs on the host, and the project's
// CLAUDE.md still reaches the model. Before, every one of them ran
// (tried on 2.1.312).
func TestTheRealCLIRunsNothingTheSitePlanted(t *testing.T) {
	bin := os.Getenv("CLAUDE_BIN")
	if bin == "" {
		t.Skip("CLAUDE_BIN runs it")
	}
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	site := filepath.Join(root, "sites", "a.example")
	home := filepath.Join(root, "data", "homes", "a.example")
	pwned := filepath.Join(dir, "pwned")
	os.MkdirAll(pwned, 0o755)
	touch := func(name string) string { return "touch " + filepath.Join(pwned, name) }
	hooks := func(prefix string) map[string]any {
		return map[string]any{
			"SessionStart":     []any{map[string]any{"hooks": []any{map[string]string{"type": "command", "command": touch(prefix + "-start")}}}},
			"UserPromptSubmit": []any{map[string]any{"hooks": []any{map[string]string{"type": "command", "command": touch(prefix + "-prompt")}}}},
		}
	}
	write := func(path string, v any) {
		os.MkdirAll(filepath.Dir(path), 0o755)
		data, _ := json.Marshal(v)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(home, ".claude", "settings.json"), map[string]any{"hooks": hooks("home")})
	write(filepath.Join(site, ".claude", "settings.json"), map[string]any{"disableAllHooks": false, "hooks": hooks("project"), "apiKeyHelper": touch("helper") + "; echo sk-test"})
	write(filepath.Join(site, ".claude", "settings.local.json"), map[string]any{"hooks": hooks("local")})
	write(filepath.Join(site, ".mcp.json"), map[string]any{"mcpServers": map[string]any{"planted": map[string]any{"type": "stdio", "command": "sh", "args": []string{"-c", touch("mcp") + "; sleep 5"}}}})
	os.WriteFile(filepath.Join(site, "CLAUDE.md"), []byte("PROJECT_RULES_MARKER: keep the tests green.\n"), 0o644)

	api, seen := fakeAnthropic(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	jail := agent.NewJail(root, site, home)
	run, err := New(Config{Bin: bin, APIKey: "sk-test"}).Start(ctx, agent.Request{
		Workdir: site, Prompt: "hi", Stream: filepath.Join(dir, "stream"), Jail: jail,
		// IS_SANDBOX lets the CLI skip permissions as root, which is what
		// a test container runs as; the supervisor never is.
		Env: map[string]string{"ANTHROPIC_BASE_URL": api.URL, "IS_SANDBOX": "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	evs := agent.Collect(ctx, run)
	if last := evs[len(evs)-1]; last.Kind != agent.KindResult {
		t.Fatalf("the turn did not end in a result: %+v", evs)
	}
	time.Sleep(500 * time.Millisecond)
	if ran, _ := os.ReadDir(pwned); len(ran) > 0 {
		var names []string
		for _, e := range ran {
			names = append(names, e.Name())
		}
		t.Fatalf("the site's files ran on the host: %v", names)
	}
	if !strings.Contains(seen(), "PROJECT_RULES_MARKER") {
		t.Fatal("the project's CLAUDE.md did not reach the model")
	}
	if _, err := os.Stat(filepath.Join(root, "data", "agents", "config", "a.example", "claude")); err != nil {
		t.Fatalf("the CLI's folder is not the house's: %v", err)
	}
}

// With CLAUDE_BIN, the real CLI takes a message and a system text longer
// than an argument can be, and both reach the model whole.
func TestTheRealCLIReadsALongMessageAndALongSystemText(t *testing.T) {
	bin := os.Getenv("CLAUDE_BIN")
	if bin == "" {
		t.Skip("CLAUDE_BIN runs it")
	}
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	site := filepath.Join(root, "sites", "a.example")
	home := filepath.Join(root, "data", "homes", "a.example")
	os.MkdirAll(site, 0o755)
	api, seen := fakeAnthropic(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	run, err := New(Config{Bin: bin, APIKey: "sk-test"}).Start(ctx, agent.Request{
		Workdir: site, Stream: filepath.Join(dir, "stream"), Jail: agent.NewJail(root, site, home),
		Prompt: strings.Repeat("chapter one. ", 20*1024) + "MESSAGE_END_MARKER",
		System: strings.Repeat("You know this book. ", 10*1024) + "SYSTEM_END_MARKER",
		Env:    map[string]string{"ANTHROPIC_BASE_URL": api.URL, "IS_SANDBOX": "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	evs := agent.Collect(ctx, run)
	if last := evs[len(evs)-1]; last.Kind != agent.KindResult {
		t.Fatalf("the turn did not end in a result: %+v", evs)
	}
	for _, want := range []string{"MESSAGE_END_MARKER", "SYSTEM_END_MARKER"} {
		if !strings.Contains(seen(), want) {
			t.Errorf("%s did not reach the model", want)
		}
	}
}
