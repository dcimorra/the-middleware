package autopilot

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Entry is one line of data/autopilot/journal.jsonl: everything the agent
// did and everything the house did to it, readable with cat on the day the
// panel is what broke. It is never rewritten, only appended to.
type Entry struct {
	At time.Time `json:"at"`
	// Kind is one of the Kind* constants.
	Kind    string  `json:"kind"`
	Message string  `json:"message,omitempty"`
	Domain  string  `json:"domain,omitempty"`
	TurnID  string  `json:"turn_id,omitempty"`
	CostUSD float64 `json:"cost_usd,omitempty"`
	// Commands are the shell commands a turn ran, clipped.
	Commands []string `json:"commands,omitempty"`
	// Next is when the agent asked to be woken, on a turn's entry.
	Next *time.Time `json:"next,omitempty"`
}

// What a journal line is about.
const (
	KindOn      = "on"      // the operator switched it on
	KindOff     = "off"     // the operator switched it off
	KindStopped = "stopped" // the kill switch: turns cut, switched off
	KindWake    = "wake"    // a turn of its own started
	KindTurn    = "turn"    // a turn of its own ended
	KindSkip    = "skip"    // it was due and was not woken, and why
	KindSession = "session" // a new session began
	KindAsk     = "ask"     // it handed work to a site
	KindAnswer  = "answer"  // a site's turn it started ended
	KindSite    = "site"    // it created a site
	KindCall    = "call"    // it called the house's API
	KindRefused = "refused" // the house refused it something
	KindPublish = "publish" // one of its sites went public
)

type journal struct {
	mu   sync.Mutex
	path string
}

func (j *journal) add(e Entry) {
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	_ = os.MkdirAll(filepath.Dir(j.path), 0o755)
	if fh, err := os.OpenFile(j.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		_, _ = fh.Write(append(line, '\n'))
		fh.Close()
	}
}

// tail is the last n entries, oldest first; n <= 0 is all of them.
func (j *journal) tail(n int) []Entry {
	j.mu.Lock()
	defer j.mu.Unlock()
	fh, err := os.Open(j.path)
	if err != nil {
		return []Entry{}
	}
	defer fh.Close()
	out := []Entry{}
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		out = append(out, e)
		if n > 0 && len(out) > 2*n {
			out = append([]Entry(nil), out[len(out)-n:]...)
		}
	}
	if n > 0 && len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}
