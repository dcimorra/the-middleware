package chat

import (
	"encoding/json"
	"strings"
)

// streamTypes are the "type"s of the lines an agent CLI writes to its own
// interface (Claude Code's stream-json, which Qwen Code shares).
var streamTypes = map[string]bool{"system": true, "stream_event": true, "assistant": true, "user": true, "result": true, "rate_limit_event": true}

// streamLine reports whether a piece of a reply is one of those lines
// rather than words. Until alpha.136 the Claude adapter showed a line it
// could not decode as text, and newer CLIs write system lines it could not
// ({"type":"system","subtype":"ui_invalidate","event":"ui.render",…}), so
// turns saved since then carry them as paragraphs of the answer. A line
// counts only when it is a whole JSON object of a stream type that names
// its session or its uuid, which an answer someone wrote does not.
func streamLine(s string) bool {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return false
	}
	var l struct {
		Type      string `json:"type"`
		SessionID string `json:"session_id"`
		UUID      string `json:"uuid"`
	}
	if json.Unmarshal([]byte(s), &l) != nil {
		return false
	}
	return streamTypes[l.Type] && (l.SessionID != "" || l.UUID != "")
}

// withoutStreamLines takes those lines out of a text whose paragraphs are
// the agent's messages, joined by a blank line as a turn joins them.
func withoutStreamLines(text string) string {
	if !strings.Contains(text, "{") {
		return text
	}
	parts := strings.Split(text, "\n\n")
	kept := parts[:0]
	for _, p := range parts {
		if !streamLine(p) {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "\n\n")
}

func withoutStreamSteps(steps []Step) []Step {
	if len(steps) == 0 {
		return steps
	}
	kept := steps[:0]
	for _, s := range steps {
		if s.Kind == "text" && streamLine(s.Text) {
			continue
		}
		kept = append(kept, s)
	}
	return kept
}

// UnmarshalJSON reads a saved turn without the stream lines it may carry,
// so every reader of a conversation (panel, site door, export, the CLI)
// gets the answer alone, and the next save of it writes it clean.
func (t *Turn) UnmarshalJSON(data []byte) error {
	type plain Turn
	var p plain
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	*t = Turn(p)
	if t.Role != "assistant" {
		return nil
	}
	t.Text = withoutStreamLines(t.Text)
	t.Steps = withoutStreamSteps(t.Steps)
	for i := range t.Threads {
		t.Threads[i].Text = withoutStreamLines(t.Threads[i].Text)
		t.Threads[i].Steps = withoutStreamSteps(t.Threads[i].Steps)
	}
	return nil
}
