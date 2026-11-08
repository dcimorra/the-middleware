package autopilot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/settings"
)

// One agent for the whole network of nodes, not one per node: two would
// each decide on their own, create the same idea twice and work against
// each other. It runs on one node; moving it is the operator's act
// (Move on the node it is on, Adopt on the one it goes to), and it knows
// every node and what each holds (Options.Network), read over the mesh.
//
// Being alone is held twice. Switching it on asks every node first and is
// refused while another has it on (Alone). And because two nodes can be
// switched on while the mesh is cut between them, the one that was
// switched on later stands down on its own when it sees the other
// (standDown), so a partition that heals leaves one.

// Node is another node of the network as the agent is shown it.
type Node struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	// OK is the node answering over the mesh; Error says why not.
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	// Sites are its sites; Trouble its count of sites in trouble.
	Sites   []Site `json:"sites,omitempty"`
	Trouble int    `json:"trouble,omitempty"`
	// Pilot is the agent there, when it is on.
	Pilot *Remote `json:"pilot,omitempty"`
}

// Remote is what another node says of its agent.
type Remote struct {
	Enabled   bool      `json:"enabled"`
	EnabledAt time.Time `json:"enabled_at,omitzero"`
}

// Bundle is what moves with the agent from one node to another: what it
// knows and what it was told, not its conversations, which stay where
// they were held (its journal there says it left). It starts a new
// session on arrival, from the notebook.
type Bundle struct {
	From       string             `json:"from"`
	Guidelines string             `json:"guidelines,omitempty"`
	Notebook   string             `json:"notebook,omitempty"`
	Created    []Created          `json:"created,omitempty"`
	Limits     settings.Autopilot `json:"limits"`
}

// ErrElsewhere is switching it on where it is not: it runs on another node.
type ErrElsewhere struct{ Node string }

func (e *ErrElsewhere) Error() string {
	return fmt.Sprintf("the autonomous agent runs on %s: there is one for the whole network; move it from there instead", e.Node)
}

// netEvery is how often the other nodes are read.
const netEvery = 5 * time.Minute

// Alone says no other node of the network has the agent on.
func (p *Pilot) Alone(ctx context.Context) error {
	if p.opts.Network == nil {
		return nil
	}
	for _, n := range p.opts.Network(ctx) {
		if n.Pilot != nil && n.Pilot.Enabled {
			return &ErrElsewhere{Node: n.Name}
		}
	}
	return nil
}

// refresh reads the other nodes and stands down if another has it on
// since before this one.
func (p *Pilot) refresh(ctx context.Context) {
	if p.opts.Network == nil {
		return
	}
	nodes := p.opts.Network(ctx)
	p.mu.Lock()
	p.net, p.netAt = nodes, p.opts.Now()
	mine := p.st.EnabledAt
	p.mu.Unlock()
	if !p.opts.Settings().Enabled {
		return
	}
	self := ""
	if p.opts.Self != nil {
		self, _ = p.opts.Self()
	}
	for _, n := range nodes {
		if n.Pilot == nil || !n.Pilot.Enabled {
			continue
		}
		if n.Pilot.EnabledAt.Before(mine) || (n.Pilot.EnabledAt.Equal(mine) && n.ID < self) {
			p.standDown(n.Name)
			return
		}
	}
}

func (p *Pilot) standDown(other string) {
	_ = p.Halt()
	p.Note(Entry{Kind: KindStopped, Message: "stood down: " + other + " had the agent on first, and there is one for the whole network"})
	p.emit("autopilot.stood_down", "", "warn", "switched off here: it runs on "+other)
}

// Network is what it knows of the other nodes, as last read.
func (p *Pilot) Network() []Node {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Node(nil), p.net...)
}

// Move hands it to another node: switched off here first (so the other
// does not see two and stand down), handed over, and switched back on
// here if the other could not take it.
func (p *Pilot) Move(ctx context.Context, node string) error {
	if p.opts.Hand == nil {
		return errors.New("this node is in no network")
	}
	cfg := p.opts.Settings()
	p.mu.Lock()
	busy := p.st.Turn != "" || len(p.st.Delegated) > 0
	created := append([]Created(nil), p.st.Created...)
	p.mu.Unlock()
	if busy {
		return errors.New("it is at work: wait for its turn and the site turns it started to end, or stop it first")
	}
	b := Bundle{Created: created, Limits: cfg}
	if p.opts.Self != nil {
		_, b.From = p.opts.Self()
	}
	if raw, err := os.ReadFile(p.GuidelinesPath()); err == nil {
		b.Guidelines = string(raw)
	}
	if p.opts.Home != "" {
		if raw, err := os.ReadFile(filepath.Join(p.opts.Home, "notebook.md")); err == nil {
			b.Notebook = string(raw)
		}
	}
	was := cfg.Enabled
	if was {
		if err := p.opts.SetEnabled(false); err != nil {
			return err
		}
	}
	if err := p.opts.Hand(ctx, node, b); err != nil {
		if was {
			_ = p.opts.SetEnabled(true)
		}
		return fmt.Errorf("%s did not take it: %w", node, err)
	}
	p.Note(Entry{Kind: KindOff, Message: "moved to " + node + " by the operator: its notebook, guidelines and limits went with it"})
	p.emit("autopilot.moved", "", "", "moved to "+node)
	return nil
}

// Adopt takes it from another node: what it knew, what it was told and
// its limits, switched on here, with a new session.
func (p *Pilot) Adopt(b Bundle) error {
	if b.Guidelines != "" {
		if err := p.SetGuidelines(b.Guidelines); err != nil {
			return err
		}
	}
	if b.Notebook != "" && p.opts.Home != "" {
		if err := os.MkdirAll(p.opts.Home, 0o700); err != nil {
			return err
		}
		if err := fsx.WriteAtomic(filepath.Join(p.opts.Home, "notebook.md"), []byte(b.Notebook), 0o600); err != nil {
			return err
		}
	}
	if p.opts.SetLimits != nil {
		if err := p.opts.SetLimits(b.Limits); err != nil {
			return err
		}
	}
	now := p.opts.Now().UTC()
	p.mu.Lock()
	have := map[string]bool{}
	for _, c := range p.st.Created {
		have[c.Domain] = true
	}
	for _, c := range b.Created {
		if !have[c.Domain] {
			p.st.Created = append(p.st.Created, c)
		}
	}
	p.st.Session, p.st.Context, p.st.Window = "", 0, 0
	if b.Limits.Enabled {
		p.st.EnabledAt = now
	}
	p.st.NextWake, p.st.NextReason, p.st.Skipped = time.Time{}, "it arrived from "+b.From, ""
	p.saveLocked()
	p.mu.Unlock()
	p.Note(Entry{Kind: KindOn, Message: "arrived from " + b.From + ": a new session from its notebook"})
	p.emit("autopilot.on", "", "", "arrived from "+b.From)
	return nil
}

// networkText is the prompt's section on the other nodes.
func networkText(self string, nodes []Node, at time.Time) (head, sites string) {
	if len(nodes) == 0 {
		return "", ""
	}
	var h, s strings.Builder
	fmt.Fprintf(&h, "You run on %s, the one agent of this network of %d nodes; the others, as read over the mesh at %s UTC:\n", orName(self), len(nodes)+1, at.UTC().Format("15:04"))
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	for _, n := range nodes {
		state := "answering"
		if !n.OK {
			state = "not answering (" + clip(oneLine(n.Error), 120) + ")"
		}
		fmt.Fprintf(&h, "- %s: %s, version %s, %d sites, %d in trouble\n", n.Name, state, orName(n.Version), len(n.Sites), n.Trouble)
		if len(n.Sites) > 0 {
			fmt.Fprintf(&s, "## %s\n%s\n", n.Name, sitesTable(n.Sites))
		}
	}
	h.WriteString("What you do happens on this node: sites you create are created here, and the house commands reach this node's sites. Another node's site you can see, and say in your notebook what it needs, until the operator moves you there.\n")
	return h.String(), s.String()
}

func orName(s string) string {
	if s == "" {
		return "?"
	}
	return s
}
