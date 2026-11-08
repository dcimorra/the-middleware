// Package agent is the contract a conversation runs on. A Provider turns a
// prompt and a working directory into a stream of events; the adapters in
// agent/claude, agent/agy and the rest drive the vendor's CLI as a subprocess so
// no SDK enters the core. What an agent may touch is decided here, by the
// Jail, and verified here, by the audit: the adapter only translates the
// jail into whatever flags its CLI understands, and a flag it does not
// understand is caught after the run rather than trusted.
package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Event kinds. "cost" carries the run's cost once it is known; "result"
// closes a successful run and "error" a failed one. Exactly one of the two
// ends every run, so a consumer can stop reading on either.
const (
	KindText   = "text"
	KindTool   = "tool"
	KindResult = "result"
	KindError  = "error"
	KindCost   = "cost"
	// KindToolResult is a tool's answer, matched to its call by ToolID.
	KindToolResult = "tool_result"
	// KindAgentStatus is a subagent changing state (Status), named by the
	// id of the call that launched it.
	KindAgentStatus = "agent_status"
	// KindLimits is the subscription's usage as the turn's stream told
	// it (Limits); it is not part of the conversation.
	KindLimits = "limits"
)

// Event is one thing the agent did or said.
type Event struct {
	Kind      string         `json:"kind"`
	Text      string         `json:"text,omitempty"`
	Tool      string         `json:"tool,omitempty"`
	Input     map[string]any `json:"input,omitempty"`
	Cost      float64        `json:"cost,omitempty"`
	SessionID string         `json:"session_id,omitempty"`
	// Thread is set on what a subagent said or did: the id of the tool
	// call that started it. Empty is the main agent.
	Thread string `json:"thread,omitempty"`
	// ToolID names a tool call, so its result (and, for a subagent, its
	// state) can be put on it when it comes back.
	ToolID string `json:"tool_id,omitempty"`
	// Output and IsError are a tool's answer (KindToolResult).
	Output  string `json:"output,omitempty"`
	IsError bool   `json:"is_error,omitempty"`
	// Status is a subagent's state (KindAgentStatus): what the provider
	// says, "completed", "failed", "killed"…
	Status string `json:"status,omitempty"`
	// Limits are the windows a KindLimits event reports.
	Limits []Window `json:"limits,omitempty"`
	// Context is how much of the model's context window the session
	// holds at the end of the run, in tokens, and Window how large that
	// window is; on a KindResult, zero when the CLI did not say. What
	// works on its own across many turns (internal/autopilot) starts a
	// fresh session before the CLI compacts this one behind its back.
	Context int `json:"context,omitempty"`
	Window  int `json:"window,omitempty"`
}

// Request is one turn.
type Request struct {
	// Workdir is where the agent runs: the site folder, or the root for the
	// conversation about the installation itself.
	Workdir string
	Prompt  string
	// Model is the provider's model name or alias; empty means its default.
	Model string
	// SessionID resumes a previous turn's session when the provider can.
	SessionID string
	// Env is added to the child's environment on top of the jail's base.
	Env map[string]string
	// System is a preamble the provider passes as system context when it
	// can; otherwise it is prepended to the prompt.
	System string
	Jail   Jail
	// Tools, when not nil, is every tool the turn has (empty: none);
	// nil is every tool, behind the jail. Disallowed are refused on top.
	Tools      []string
	Disallowed []string
	// Name is the conversation's name, for files that must be one per
	// conversation and not one per site.
	Name string
	// Stream is the file the CLI's output goes to; it outlives this
	// process so a restarted supervisor can adopt the turn.
	Stream string
}

// Adopter is a provider that can pick up a run a previous supervisor
// started, from its pid and its output file.
type Adopter interface {
	Adopt(ctx context.Context, pid int, stream string) Run
}

// Run is one turn in progress.
type Run interface {
	// Events delivers the stream and is closed when the run ends.
	Events() <-chan Event
	// Stop ends the run early. Safe to call more than once.
	Stop()
}

// Provider is one agent CLI.
type Provider interface {
	// Key names the provider in manifests ("claude", "agy").
	Key() string
	// Available reports whether a run could start, with a reason when not.
	Available() (bool, string)
	// Models the provider accepts; the first is its default.
	Models() []string
	Start(ctx context.Context, req Request) (Run, error)
}

// Permissioner is a provider that cannot hold every permission of a site
// door apart (chat.Permissions: read, edit, web, shell) and says which it
// can: the panel offers those alone. One that does not implement it holds
// all four, translating the request's Tools.
type Permissioner interface {
	Permissions() []string
}

// Labeler is a provider whose model names do not say what they are (an
// alias): ModelLabels says, by name, what each one runs, for the panel to
// show beside it.
type Labeler interface {
	ModelLabels() map[string]string
}

// ErrUnknownProvider is returned by a registry lookup that finds nothing.
var ErrUnknownProvider = errors.New("unknown agent provider")

// Registry holds the providers an installation enabled.
type Registry struct {
	mu    sync.RWMutex
	byKey map[string]Provider
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{byKey: map[string]Provider{}} }

// Register adds or replaces a provider under its key.
func (r *Registry) Register(p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byKey[p.Key()] = p
}

// Remove takes a provider out: a module that adds one when it is enabled
// takes it away when it is disabled, rather than leaving a provider the
// chat offers and nothing can run.
func (r *Registry) Remove(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byKey, key)
}

// Get looks a provider up by key.
func (r *Registry) Get(key string) (Provider, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.byKey[key]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownProvider, key)
	}
	return p, nil
}

// Keys lists the registered providers, sorted.
func (r *Registry) Keys() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.byKey))
	for k := range r.byKey {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Collect drains a run into a slice, for callers that do not stream. It
// returns when the channel closes or when ctx ends, stopping the run in
// the latter case.
func Collect(ctx context.Context, run Run) []Event {
	var out []Event
	for {
		select {
		case ev, ok := <-run.Events():
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-ctx.Done():
			run.Stop()
			for ev := range run.Events() {
				out = append(out, ev)
			}
			return out
		}
	}
}

// Transcript flattens events into the text the audit greps: what the agent
// said plus every tool call's name and input. It is built from events, not
// from the CLI's raw output, so a provider that reports tool calls is
// audited on what it actually did rather than on what it printed.
func Transcript(evs []Event) string {
	var b []byte
	for _, ev := range evs {
		switch ev.Kind {
		case KindText, KindError:
			b = append(b, ev.Text...)
			b = append(b, '\n')
		case KindTool:
			b = append(b, ev.Tool...)
			b = append(b, ' ')
			b = append(b, flatten(ev.Input)...)
			b = append(b, '\n')
		}
	}
	return string(b)
}

func flatten(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		s := ""
		for _, k := range keys {
			s += k + "=" + flatten(x[k]) + " "
		}
		return s
	case []any:
		s := ""
		for _, e := range x {
			s += flatten(e) + " "
		}
		return s
	default:
		return fmt.Sprint(x)
	}
}
