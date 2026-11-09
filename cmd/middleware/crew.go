package main

import (
	"os"
	"path/filepath"
	"time"

	"github.com/dcimorra/the-middleware/internal/agent"
	"github.com/dcimorra/the-middleware/internal/app"
	"github.com/dcimorra/the-middleware/internal/chat"
	"github.com/dcimorra/the-middleware/internal/crew"
	"github.com/dcimorra/the-middleware/internal/secrets"
)

// keptSessions is how many of an agent's conversations are kept: every
// wake opens one, and what it must remember is in its notebook, not in
// them; the operator reads the recent ones.
const keptSessions = 30

// buildCrew gives the operator's crew of agents the house's parts: the
// chat for their sessions, the usage book for the subscription's windows,
// and the house's key for their vaults. The chat is told back where an
// agent's turn gets its vault and its agent from.
func buildCrew(a *app.App, w *wireHook) *crew.Crew {
	box, err := secrets.Open(a.Root)
	if err != nil {
		box = nil
	}
	c := crew.New(crew.Options{
		Root: a.Root,
		Bus:  a.Bus,
		Box:  box,
		NewSession: func(id string) (string, error) {
			chatManager.Keep(chat.CrewDomain(id), keptSessions)
			return chatManager.Create(chat.CrewDomain(id), chat.CreateOptions{Origin: chat.OriginPanel, Title: "Session of " + time.Now().UTC().Format("2006-01-02 15:04")})
		},
		Send: func(name, prompt, provider, model string) (string, error) {
			return chatManager.SendWith(name, prompt, chat.SendOptions{Provider: provider, Model: model})
		},
		Stop:    chatManager.Stop,
		Running: chatManager.Running,
		Commands: func(name, turnID string) []string {
			conv, err := chatManager.Conversation(name)
			if err != nil {
				return nil
			}
			for _, t := range conv.Turns {
				if t.ID == turnID && t.Role != "user" {
					return commandsOf(t.Steps)
				}
			}
			return nil
		},
		Usage: func(provider string) []agent.Window {
			u, ok := usageBook(a.Root).Get(provider)
			if !ok {
				return nil
			}
			return u.Windows
		},
		// The network's agent is woken by its own engine: an answer to one
		// of its requests is a reason to look now.
		OnAnswer: func(id string) {
			if id == crew.Network && w.pilot != nil {
				w.pilot.Wake()
			}
		},
		Forget: func(domain string) {
			_, _ = chatManager.Purge(domain)
			// What Purge left (a turn still stopping) goes with the agent.
			_ = os.RemoveAll(filepath.Join(a.Root, "data", "chat", domain))
			_ = os.Remove(filepath.Join(a.Root, "data", "chat", domain+".json"))
		},
	})
	chatManager.SetCrew(c.Env, c.Settings)
	return c
}
