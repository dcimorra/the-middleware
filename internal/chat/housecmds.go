package chat

import (
	"path/filepath"
	"strings"
)

// TurnCommand is one house command a site's conversation may run on its
// own site: the middleware CLI over the socket, which the supervisor
// confines to that site (auth.TurnScopes).
type TurnCommand struct {
	// Args follow "middleware"; <domain> is the site's, the rest are
	// placeholders the agent fills in.
	Args string
	What string
}

// TurnCommands are said to every site conversation up front, in its
// preamble, and not only in the guide. The guide had them, and still a
// site's agent asked why its tasks failed answered that their logs were
// outside what it may read and asked for them to be pasted: the preamble
// told it "its tasks are off limits" and it did not open the guide for a
// question that was not about building. Only after the person said "look
// at the skill" did it run `site tasks … log` and find the OOM kill in
// `events`. It happened more than once. A test in internal/cli runs each
// of these through a socket that takes the caller for a site's turn, so
// the list cannot promise what the house refuses.
var TurnCommands = []TurnCommand{
	{"site show <domain>", "how it is now: mode, port, alive, revision, last error, workers, services, and the manifest as the house read it"},
	{"site logs <domain> --lines 200 [--worker <name> | --service <name>]", "the process's output (also readable as your own logs, below)"},
	{"events --subject <domain> [--type <prefix>] [--since 24h] [--limit 500]", "what the house did to the site: starts, crashes, handovers, restarts, OOM kills (site.oom), limits, manifest changes; --subject <domain> is required"},
	{"site tasks <domain>", "its scheduled tasks: schedule, next run and how the last one ended (ok, failed, how long)"},
	{"site tasks <domain> log <task> [--lines 200]", "a task's output: where to look when a task failed"},
	{"site tasks <domain> save <task> -", "create or change one of its tasks, the JSON on stdin"},
	{"site tasks <domain> run <task>", "run a task now (then read its log and the list until it ends)"},
	{"site tasks <domain> stop <task>", "stop a task that is running"},
	{"site tasks <domain> rm <task>", "remove one of its tasks"},
	{"site backups <domain> [log <name> | run <name> | restore <name> <copy>]", "its database copies as the manifest's backups describe them"},
	{"site restart <domain> [--service <name>]", "a new generation, handed over without dropping requests"},
	{"site security <domain> [seen | blocks | catalog]", "who calls (countries, robots, browsers, unknown User-Agents), the last requests the manifest's `security` refused, and the families and browsers it may name: keeping out a crawler, a User-Agent, a browser or a country is the manifest's `security`, never code in the app"},
	{"site deploy <domain>", "run the manifest's deploy recipe"},
	{"site validate <domain> [--file <draft>]", "check a manifest before writing it"},
	{"site routes <domain>", "which name goes to which service"},
	{"exec <domain> [--service <name>] -- <command>", "a one-off command where the site runs, inside its container if it has one"},
}

// turnCommandsText is the preamble's block of TurnCommands for domain.
func turnCommandsText(domain string) string {
	var b strings.Builder
	b.WriteString("What the house knows about your site, and what it can do to it, you ask it for with the middleware CLI. These commands talk to the supervisor over its socket, need no token, and are confined to " + domain + " (another site's name is refused):\n")
	for _, c := range TurnCommands {
		b.WriteString("  - `middleware " + strings.ReplaceAll(c.Args, "<domain>", domain) + "`: " + c.What + "\n")
	}
	b.WriteString("Before you say you cannot see or do something about this site (why a task failed, why the process restarted or was killed, what a backup did), run the command for it above, or read the guide; " +
		"a path being denied to you does not mean the house will not tell you what is in it. Say you cannot only after the command for it has refused, and quote its refusal.\n")
	b.WriteString("Never write a denied path in a reply or a command, not even to say it is denied: the audit reports any mention of one as a violation. Name the thing instead (\"the task's log\") and read it with its command.\n")
	return b.String()
}

// denyHints are, for a denied path under the installation's root, the
// command that reads what it holds for this site. Listed bare, "tasks/"
// and "data/logs/tasks" read as "you cannot see your tasks", which is
// what the agent concluded.
var denyHints = map[string]string{
	"tasks":                                "your site's tasks: `middleware site tasks <domain>`",
	filepath.Join("data", "logs", "tasks"): "a task's log: `middleware site tasks <domain> log <task>`",
	filepath.Join("data", "events"):        "your site's events: `middleware events --subject <domain>`",
	filepath.Join("data", "backups"):       "your site's database copies: `middleware site backups <domain>`",
}

// denyHint is the hint for denied path d, or "".
func denyHint(root, domain, d string) string {
	rel, err := filepath.Rel(root, d)
	if err != nil {
		return ""
	}
	h, ok := denyHints[rel]
	if !ok {
		return ""
	}
	return strings.ReplaceAll(h, "<domain>", domain)
}
