package chat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const uiLine = `{"type":"system","subtype":"ui_invalidate","event":"ui.render","uuid":"a28e6801-b66d-4d60-bf14-0ebb73db180f","session_id":"fe13f1ef-a90c-4210-a8e1-f5836af9ccae"}`

// Turns saved before the fix carry the CLI's own lines as paragraphs and
// steps of the answer; read back, they are gone, and words that merely
// look like JSON stay.
func TestASavedTurnIsReadWithoutTheCLIsOwnLines(t *testing.T) {
	saved := `{"id":"t2","role":"assistant","text":"` + jsonEsc(uiLine) + `\n\nSoy Claude.\n\n{\"type\":\"system\",\"note\":\"an example the agent wrote\"}",` +
		`"steps":[{"kind":"text","text":"` + jsonEsc(uiLine) + `"},{"kind":"text","text":"Soy Claude."},{"kind":"tool","tool":{"name":"Read"}}],` +
		`"threads":[{"id":"x","text":"` + jsonEsc(uiLine) + `\n\nhecho","steps":[{"kind":"text","text":"` + jsonEsc(uiLine) + `"}]}]}`
	var tr Turn
	if err := json.Unmarshal([]byte(saved), &tr); err != nil {
		t.Fatal(err)
	}
	if want := "Soy Claude.\n\n{\"type\":\"system\",\"note\":\"an example the agent wrote\"}"; tr.Text != want {
		t.Fatalf("text = %q", tr.Text)
	}
	if len(tr.Steps) != 2 || tr.Steps[0].Text != "Soy Claude." || tr.Steps[1].Kind != "tool" {
		t.Fatalf("steps = %+v", tr.Steps)
	}
	if tr.Threads[0].Text != "hecho" || len(tr.Threads[0].Steps) != 0 {
		t.Fatalf("thread = %+v", tr.Threads[0])
	}
	// What the person wrote is theirs, whatever it looks like.
	var u Turn
	json.Unmarshal([]byte(`{"id":"t1","role":"user","text":"`+jsonEsc(uiLine)+`"}`), &u)
	if u.Text != uiLine {
		t.Fatalf("user text = %q", u.Text)
	}
}

// Through the manager, as the panel and the site door read it.
func TestTheHistoryOfAConversationShowsNoCLILines(t *testing.T) {
	root := t.TempDir()
	m := &Manager{opts: Options{Root: root}}
	c := Conversation{Domain: "a.test", Turns: []Turn{{ID: "t", Role: "assistant", Text: uiLine + "\n\nhola", Steps: []Step{{Kind: "text", Text: uiLine}, {Kind: "text", Text: "hola"}}}}}
	data, _ := json.Marshal(c)
	os.MkdirAll(filepath.Dir(m.file("a.test")), 0o755)
	os.WriteFile(m.file("a.test"), data, 0o600)
	got := m.load("a.test")
	if len(got.Turns) != 1 || got.Turns[0].Text != "hola" || len(got.Turns[0].Steps) != 1 {
		t.Fatalf("turns = %+v", got.Turns)
	}
}

func jsonEsc(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}
