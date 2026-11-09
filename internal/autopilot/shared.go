package autopilot

import (
	"path/filepath"
	"time"

	"github.com/dcimorra/the-middleware/internal/agent"
)

// What the operator's crew of agents (internal/crew) shares with this
// one: they wake, end their turns and are held by the subscription the
// same way, and one reading of a "Next:" line is one less to drift.

// Journal is an append-only journal of Entry lines.
type Journal struct{ j journal }

// OpenJournal is the journal at path, created on its first line.
func OpenJournal(path string) *Journal { return &Journal{journal{path: filepath.Clean(path)}} }

// Add appends e.
func (x *Journal) Add(e Entry) { x.j.add(e) }

// Tail is the last n entries, oldest first; n <= 0 is all of them.
func (x *Journal) Tail(n int) []Entry { return x.j.tail(n) }

// ParseNext reads a turn's "Next:" line: how long to wait and why.
func ParseNext(text string) (time.Duration, string) { return parseNext(text) }

// SummaryOf is a turn's "Summary:" line, or its last line.
func SummaryOf(text string) string { return summaryOf(text) }

// OverQuota is the first subscription window over limit percent that has
// not reset yet.
func OverQuota(ws []agent.Window, limit float64, now time.Time) (agent.Window, bool) {
	return overQuota(ws, limit, now)
}

// Clip keeps s to n bytes, on a rune boundary.
func Clip(s string, n int) string { return clip(s, n) }

// OneLine folds s's whitespace into single spaces.
func OneLine(s string) string { return oneLine(s) }
