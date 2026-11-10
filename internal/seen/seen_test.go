package seen

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestASitesCallersAreTalliedByKindAndSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	b := New(dir)
	for i := 0; i < 3; i++ {
		b.Note("a.example", "es", "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)")
	}
	b.Note("a.example", "XX", "Mozilla/5.0 (Windows NT 10.0) AppleWebKit/537.36 Chrome/120 Safari/537.36")
	b.Note("a.example", "", "")
	b.Note("a.example", "", "weird-fetcher/1.0")
	tb := b.Table("a.example")
	if tb.Total != 6 || len(tb.Countries) != 1 || tb.Countries[0].Key != "ES" || tb.Countries[0].Count != 3 {
		t.Fatalf("countries %+v", tb)
	}
	if len(tb.Bots) == 0 || tb.Bots[0].Count != 3 {
		t.Fatalf("bots %+v", tb.Bots)
	}
	if len(tb.Agents) != 1 || tb.Agents[0].Key != "weird-fetcher/1.0" {
		t.Fatalf("raw agents %+v", tb.Agents)
	}
	b.Flush()
	again := New(dir).Table("a.example")
	if again.Total != 6 || len(again.Bots) == 0 {
		t.Fatalf("after a restart %+v", again)
	}
	b.Forget("a.example")
	if New(dir).Table("a.example").Total != 0 {
		t.Fatal("not forgotten")
	}
	if b.Table("../x").Total != 0 {
		t.Fatal("a path in the name")
	}
}

func TestRawAgentsAreBounded(t *testing.T) {
	b := New(t.TempDir())
	for i := 0; i < agentsKept+50; i++ {
		b.Note("a.example", "", fmt.Sprintf("fetcher/%d", i))
	}
	if n := len(b.Table("a.example").Agents); n > agentsKept {
		t.Fatalf("%d kept", n)
	}
}

// A renamed site's tally follows its file: released, nothing in memory
// writes the old name again a minute later, and the new one reads what
// was counted.
func TestAReleasedTallyIsWrittenAndNotBroughtBack(t *testing.T) {
	dir := t.TempDir()
	b := New(dir)
	b.Note("old.example", "es", "weird-fetcher/1.0")
	b.Release("old.example")
	if err := os.Rename(filepath.Join(dir, "old.example.json"), filepath.Join(dir, "new.example.json")); err != nil {
		t.Fatalf("released without writing: %v", err)
	}
	b.Flush()
	if _, err := os.Stat(filepath.Join(dir, "old.example.json")); !os.IsNotExist(err) {
		t.Fatal("the old name was written again")
	}
	if tb := b.Table("new.example"); tb.Total != 1 {
		t.Fatalf("the new name does not have what was counted: %+v", tb)
	}
}
