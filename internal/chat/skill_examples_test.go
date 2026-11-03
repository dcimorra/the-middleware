package chat

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/tasks"
)

// skillSource is the embedded skill as it sits in the tree. The test reads
// the file rather than an embed so that an edit to the skill is checked by
// the next `go test` without a rebuild step in between.
const skillSource = "skills/middleware/SKILL.md"

var fencedJSON = regexp.MustCompile("(?s)```json\\s*\n(.*?)\n```")

// skillExamples returns every ```json block of the skill that carries
// "version": 2, with the line it starts on for the failure message.
func skillExamples(t *testing.T) (blocks []string, lines []int) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("skills", "middleware", "SKILL.md"))
	if err != nil {
		t.Fatalf("read %s: %v", skillSource, err)
	}
	src := string(data)
	for _, m := range fencedJSON.FindAllStringSubmatchIndex(src, -1) {
		body := src[m[2]:m[3]]
		if !strings.Contains(body, `"version": 2`) {
			continue
		}
		blocks = append(blocks, body)
		lines = append(lines, strings.Count(src[:m[0]], "\n")+1)
	}
	if len(blocks) == 0 {
		t.Fatalf("%s has no ```json block with \"version\": 2; the skill is supposed to teach by example", skillSource)
	}
	return blocks, lines
}

// isTask tells a task file from a manifest: a task has a schedule at its
// top, and a manifest has one only inside its backups.
func isTask(body string) bool {
	return strings.Contains(body, `"schedule"`) && !strings.Contains(body, `"backups"`)
}

// TestEveryManifestExampleInTheSkillParses runs each manifest snippet of the
// skill through the real parser. A skill that teaches a field the schema
// rejects would have every agent writing manifests the engine refuses, and
// nothing but this test would say so.
func TestEveryManifestExampleInTheSkillParses(t *testing.T) {
	blocks, lines := skillExamples(t)
	manifests := 0
	for i, body := range blocks {
		if isTask(body) {
			continue
		}
		if _, err := manifest.Parse([]byte(body)); err != nil {
			t.Errorf("%s:%d: manifest example does not parse: %v\n%s", skillSource, lines[i], err, body)
		}
		manifests++
	}
	if manifests == 0 {
		t.Fatalf("%s: no manifest examples found", skillSource)
	}
}

// TestEveryTaskExampleInTheSkillParses does the same for the task files the
// skill shows: they share the "version": 2 marker and are told apart by
// their schedule, which a manifest never has.
func TestEveryTaskExampleInTheSkillParses(t *testing.T) {
	blocks, lines := skillExamples(t)
	found := 0
	for i, body := range blocks {
		if !isTask(body) {
			continue
		}
		if _, err := tasks.Parse([]byte(body)); err != nil {
			t.Errorf("%s:%d: task example does not parse: %v\n%s", skillSource, lines[i], err, body)
		}
		found++
	}
	if found == 0 {
		t.Fatalf("%s: no task examples found", skillSource)
	}
}

var savedTask = regexp.MustCompile("(?s)middleware site tasks \\S+ save \\S+ - <<'JSON'\n(.*?)\nJSON\n")

// TestEveryTaskSavedInTheSkillParses checks the tasks the skill has a site's
// agent save with `middleware site tasks … save` from a heredoc: they sit in
// a shell block, where the test above does not look, and they are the ones
// a site's agent copies first.
func TestEveryTaskSavedInTheSkillParses(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("skills", "middleware", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	found := savedTask.FindAllStringSubmatch(string(data), -1)
	if len(found) == 0 {
		t.Fatalf("%s: no `middleware site tasks … save` example", skillSource)
	}
	for _, m := range found {
		if _, err := tasks.Parse([]byte(m[1])); err != nil {
			t.Errorf("%s: a task saved with `site tasks save` does not parse: %v\n%s", skillSource, err, m[1])
		}
	}
}
