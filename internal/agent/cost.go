package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/fsx"
)

// Ledger is the per-site cost record at data/agents/cost/<domain>.json: a
// sum per day, so a month's total is a sum over keys and a cap is checked
// before a turn starts rather than discovered on the invoice.
type Ledger struct {
	dir string
	mu  sync.Mutex
}

// LedgerFile is the on-disk shape.
type LedgerFile struct {
	Domain string             `json:"domain"`
	Days   map[string]float64 `json:"days"` // "2006-01-02" → USD
	Total  float64            `json:"total"`
	// Turns and Models keep, per day, how many turns cost something and
	// what each model cost: the days are kept, not only today, so what a
	// project spent in a month can be read back (V1 reset at midnight and
	// lost it).
	Turns  map[string]int                `json:"turns,omitempty"`
	Models map[string]map[string]float64 `json:"models,omitempty"`
	// Unpriced counts, per day, the turns that reported no cost (an agent
	// on a subscription): kept apart from Turns, which are the ones that
	// cost something, so the sums above stay what they say.
	Unpriced map[string]int `json:"unpriced,omitempty"`
}

// NewLedger returns a ledger under dataDir.
func NewLedger(dataDir string) *Ledger {
	return &Ledger{dir: filepath.Join(dataDir, "agents", "cost")}
}

func (l *Ledger) path(domain string) string {
	return filepath.Join(l.dir, domain+".json")
}

func (l *Ledger) read(domain string) LedgerFile {
	var f LedgerFile
	if err := fsx.ReadJSON(l.path(domain), &f); err != nil || f.Days == nil {
		f = LedgerFile{Domain: domain, Days: map[string]float64{}}
	}
	return f
}

// Add records usd for domain on the day of at. Zero and negative amounts
// are ignored: a provider that reports no cost must not create a file that
// reads as "free".
func (l *Ledger) Add(domain string, usd float64, at time.Time) error {
	return l.AddTurn(domain, "", usd, at)
}

// AddTurn records one turn's cost with its model.
func (l *Ledger) AddTurn(domain, model string, usd float64, at time.Time) error {
	if usd <= 0 {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.read(domain)
	day := at.UTC().Format("2006-01-02")
	f.Days[day] += usd
	f.Total += usd
	if f.Turns == nil {
		f.Turns = map[string]int{}
	}
	f.Turns[day]++
	if model != "" {
		if f.Models == nil {
			f.Models = map[string]map[string]float64{}
		}
		if f.Models[day] == nil {
			f.Models[day] = map[string]float64{}
		}
		f.Models[day][model] += usd
	}
	return fsx.WriteJSON(l.path(domain), f, 0o644)
}

// CountUnpriced records a turn that reported no cost, for a cap that
// counts turns where dollars cannot be counted.
func (l *Ledger) CountUnpriced(domain string, at time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.read(domain)
	if f.Unpriced == nil {
		f.Unpriced = map[string]int{}
	}
	f.Unpriced[at.UTC().Format("2006-01-02")]++
	return fsx.WriteJSON(l.path(domain), f, 0o644)
}

// DayTurns is how many turns domain ran on the day of at, priced or not.
func (l *Ledger) DayTurns(domain string, at time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.read(domain)
	day := at.UTC().Format("2006-01-02")
	return f.Turns[day] + f.Unpriced[day]
}

// Books lists every ledger kept, by its key (a domain, or a domain's
// site door book).
func (l *Ledger) Books() []string {
	ents, _ := os.ReadDir(l.dir)
	var out []string
	for _, e := range ents {
		if name, ok := strings.CutSuffix(e.Name(), ".json"); ok {
			out = append(out, name)
		}
	}
	return out
}

// Day is what domain spent on the day of at.
func (l *Ledger) Day(domain string, at time.Time) float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.read(domain).Days[at.UTC().Format("2006-01-02")]
}

// File is domain's whole record, for the panel.
func (l *Ledger) File(domain string) LedgerFile {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.read(domain)
}

// Month sums the domain's cost for the month containing at.
func (l *Ledger) Month(domain string, at time.Time) float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.read(domain)
	prefix := at.UTC().Format("2006-01")
	sum := 0.0
	for day, v := range f.Days {
		if len(day) >= 7 && day[:7] == prefix {
			sum += v
		}
	}
	return sum
}

// Total returns everything ever spent by domain.
func (l *Ledger) Total(domain string) float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.read(domain).Total
}

// ErrOverCap says the month's spending reached the manifest's cap.
type ErrOverCap struct {
	Domain string
	Spent  float64
	Cap    float64
}

func (e *ErrOverCap) Error() string {
	return fmt.Sprintf("%s: agent spending this month is $%.2f, cap is $%.2f", e.Domain, e.Spent, e.Cap)
}

// Allow fails with *ErrOverCap when cap is positive and the month is spent.
func (l *Ledger) Allow(domain string, cap float64, at time.Time) error {
	if cap <= 0 {
		return nil
	}
	if spent := l.Month(domain, at); spent >= cap {
		return &ErrOverCap{Domain: domain, Spent: spent, Cap: cap}
	}
	return nil
}

// SiteSettings is the manifest's `agent` section as this package reads it.
type SiteSettings struct {
	Provider      string  `json:"provider"`
	Model         string  `json:"model"`
	MonthlyCapUSD float64 `json:"monthly_cap_usd"`
}

// ReadSiteSettings reads the `agent` section straight from the site
// folder's site.json, without the manifest package: that package rejects
// unknown fields on purpose and `monthly_cap_usd` is not in its schema, and
// a manifest it refuses (a typo elsewhere) must still let the conversation
// open, because the conversation is how the typo gets fixed. A missing
// file or section is all defaults.
func ReadSiteSettings(siteDir string) SiteSettings {
	data, err := os.ReadFile(filepath.Join(siteDir, "site.json"))
	if err != nil {
		return SiteSettings{}
	}
	var raw struct {
		Agent SiteSettings `json:"agent"`
	}
	if json.Unmarshal(data, &raw) != nil {
		return SiteSettings{}
	}
	return raw.Agent
}
