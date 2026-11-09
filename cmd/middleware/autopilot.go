package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/agent"
	"github.com/dcimorra/the-middleware/internal/app"
	"github.com/dcimorra/the-middleware/internal/autopilot"
	"github.com/dcimorra/the-middleware/internal/chat"
	"github.com/dcimorra/the-middleware/internal/crew"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/nodes"
	"github.com/dcimorra/the-middleware/internal/settings"
	"github.com/dcimorra/the-middleware/internal/sites"
)

// buildPilot gives the autonomous agent the house's parts: the chat for
// its turns and the ones it hands to sites, the engine and the visits for
// what it sees, the usage book for the subscription's windows, and the
// settings for its switch.
func buildPilot(a *app.App, w *wireHook) *autopilot.Pilot {
	node, _ := os.Hostname()
	return autopilot.New(autopilot.Options{
		DataDir:  dataDir(a),
		Bus:      a.Bus,
		Node:     node,
		Home:     filepath.Join(a.Root, "data", "homes", chat.Autopilot),
		Settings: func() settings.Autopilot { return a.CurrentSettings().Autopilot },
		// Through the app's copy, which GET /settings answers from: a
		// switch written to the file alone would come back on with the
		// next card the panel saves.
		SetEnabled: func(on bool) error {
			cur := a.CurrentSettings()
			cur.Autopilot.Enabled = on
			if err := settings.Save(a.Root, cur); err != nil {
				return err
			}
			a.SetSettings(cur)
			return nil
		},
		Sites: func() []autopilot.Site {
			var out []autopilot.Site
			now := time.Now()
			for _, sn := range a.Engine.Sites() {
				if sn.Manifest == nil {
					continue
				}
				s := autopilot.Site{Domain: sn.Domain, Mode: string(sn.Mode), Alive: sn.Alive, Failures: sn.Failures, Error: sn.Error,
					OptOut: sn.Manifest.Autopilot != nil && !*sn.Manifest.Autopilot}
				if n := len(sn.Checks); n >= 3 && !sn.Checks[n-1].OK && !sn.Checks[n-2].OK && !sn.Checks[n-3].OK {
					s.Down = true
				}
				if w.publisher != nil {
					_, s.Published = w.publisher.Published(sn.Domain)
				}
				if theVisits != nil {
					s.Visits = int(theVisits.Report(sn.Domain, now.AddDate(0, 0, -7), now).Visits)
				}
				out = append(out, s)
			}
			return out
		},
		NewSession: func() (string, error) {
			chatManager.Keep(chat.Autopilot, keptSessions)
			return chatManager.Create(chat.Autopilot, chat.CreateOptions{Origin: chat.OriginAutopilot, Title: "Session of " + time.Now().UTC().Format("2006-01-02 15:04")})
		},
		Send: func(name, prompt, provider, model string) (string, error) {
			return chatManager.SendWith(name, prompt, chat.SendOptions{Provider: provider, Model: model})
		},
		AskSite: func(domain, prompt, provider, model string) (string, string, error) {
			name, err := chatManager.AutopilotConversation(domain)
			if err != nil {
				return "", "", err
			}
			id, err := chatManager.SendWith(name, prompt, chat.SendOptions{Provider: provider, Model: model})
			return name, id, err
		},
		Fix: func(domain, title, prompt string) (string, string, error) {
			// The operator's own conversation, as if they had opened it
			// and written the proposal themselves: it runs with the
			// site's (or the house's) agent and rights, not the agent's.
			if domain == "" {
				domain = chat.Middleware
			}
			name, err := chatManager.Create(domain, chat.CreateOptions{Origin: chat.OriginPanel, Title: title})
			if err != nil {
				return "", "", err
			}
			id, err := chatManager.SendWith(name, prompt, chat.SendOptions{})
			return name, id, err
		},
		Stop:    chatManager.Stop,
		Running: chatManager.Running,
		Commands: func(name, turnID string) []string {
			c, err := chatManager.Conversation(name)
			if err != nil {
				return nil
			}
			for _, t := range c.Turns {
				if t.ID == turnID && t.Role != "user" {
					return commandsOf(t.Steps)
				}
			}
			return nil
		},
		Self: func() (string, string) {
			if w.mesh == nil || !w.mesh.InNetwork() {
				return "", ""
			}
			me := w.mesh.Self()
			return me.ID, me.Name
		},
		Network: func(ctx context.Context) []autopilot.Node {
			if w.mesh == nil || !w.mesh.InNetwork() {
				return nil
			}
			return readNetwork(ctx, w.mesh)
		},
		Hand: func(ctx context.Context, node string, b autopilot.Bundle) error {
			if w.mesh == nil || !w.mesh.InNetwork() {
				return errors.New("this node is in no network")
			}
			for _, x := range w.mesh.Members() {
				if x.ID == node || strings.EqualFold(x.Name, node) {
					return w.mesh.Call(ctx, x, "POST", "/autopilot/adopt", b, nil)
				}
			}
			return fmt.Errorf("%s is not a node of this network", node)
		},
		SetLimits: func(l settings.Autopilot) error {
			cur := a.CurrentSettings()
			cur.Autopilot = l
			if err := settings.Save(a.Root, cur); err != nil {
				return err
			}
			a.SetSettings(cur)
			return nil
		},
		Asks: func() string {
			if w.crew == nil {
				return ""
			}
			return w.crew.AskText(crew.Network)
		},
		Usage: func(provider string) []agent.Window {
			u, ok := usageBook(a.Root).Get(provider)
			if !ok {
				return nil
			}
			return u.Windows
		},
	})
}

// commandsOf are the shell commands of a turn's steps, each on one line
// and clipped: the journal says what ran, the conversation how it went.
func commandsOf(steps []chat.Step) []string {
	var out []string
	for _, st := range steps {
		if st.Tool == nil || st.Tool.Name != "Bash" {
			continue
		}
		cmd, _ := st.Tool.Input["command"].(string)
		cmd = strings.Join(strings.Fields(cmd), " ")
		if cmd == "" {
			continue
		}
		if r := []rune(cmd); len(r) > 300 {
			cmd = string(r[:300]) + "…"
		}
		out = append(out, cmd)
		if len(out) >= 100 {
			break
		}
	}
	return out
}

// readNetwork asks every other node, at once, for its sites and its
// agent: what the agent knows of the network, and how it knows it is
// alone. A node of an older release has no /autopilot and is read
// without it.
func readNetwork(ctx context.Context, m *nodes.Mesh) []autopilot.Node {
	members := m.Members()
	out := make([]autopilot.Node, len(members))
	var wg sync.WaitGroup
	for i, x := range members {
		wg.Add(1)
		go func(i int, x nodes.Member) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			n := autopilot.Node{ID: x.ID, Name: x.Name, Version: x.Version}
			var list []sites.Site
			if err := m.Call(cctx, x, "GET", "/sites", nil, &list); err != nil {
				n.Error = err.Error()
				out[i] = n
				return
			}
			n.OK = true
			for _, s := range list {
				if s.Mode == manifest.ModeProject {
					continue
				}
				site := autopilot.Site{Domain: s.Domain, Mode: string(s.Mode), Alive: s.Alive, Error: s.Error, Down: s.Health == "down", Published: true, Visits: -1}
				if s.Manifest != nil && s.Manifest.Autopilot != nil && !*s.Manifest.Autopilot {
					site.OptOut = true
				}
				if site.Down || (s.Mode == manifest.ModeRun && !s.Alive) {
					n.Trouble++
				}
				n.Sites = append(n.Sites, site)
			}
			var st autopilot.Status
			if err := m.Call(cctx, x, "GET", "/autopilot", nil, &st); err == nil {
				n.Pilot = &autopilot.Remote{Enabled: st.Enabled, EnabledAt: st.EnabledAt}
			}
			out[i] = n
		}(i, x)
	}
	wg.Wait()
	return out
}
