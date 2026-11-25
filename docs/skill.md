# The embedded skill

`internal/chat/skills/middleware/SKILL.md` is what an agent knows about this
house before it touches anything: what kind of thing a request is (site,
worker, task, module), where each one goes, the manifest field by field with
recipes that parse, how a deployment by pointer works, how to run long work
from a turn without losing it, how to check a site, how a domain is
published, and the lessons that were paid for more than once.

It is written as a Claude Code / agent skill: YAML front matter with `name`
and a one-line `description`, then Markdown. The conversation manager
installs it into every conversation's `HOME` before each turn, for the
per-site conversation (working directory: the site folder) and for the house
conversation (working directory: the installation root), so the agent reads
it the way it reads any skill, and an installation never has to copy it by
hand or keep it current: it ships inside the binary and changes with it.

Every manifest and task example in the skill goes through the real parsers
in `internal/chat/skill_examples_test.go`: a skill that taught a field the
schema rejects would have every agent writing manifests the engine refuses,
and the test is what says so before a release does. That includes the task
a site's agent saves from a heredoc with `middleware site tasks <domain>
save <name> -`, which is how the skill tells an agent working in a site to
manage that site's tasks (§ 9): it cannot write `tasks/`, and the house
keeps the task to its own site.

To change what agents are told, edit the skill, run `go test ./internal/chat/`,
and ship the binary. A site's own `AGENTS.md` (or `CLAUDE.md`) adds what is
specific to that project; the skill is what is true for every site on every installation.

## The operator's own rules

The skill says what is true of the house in every installation. What one
operator wants of every agent (no emoji as icons, a stack, a tone) is a
setting, `agents.rules` in `data/settings.json`, written in Settings →
Agents → "Rules for every agent" or with `PUT /settings`. It is free text,
up to 8000 bytes, and `chat.OperatorRules` says it at the end of every
preamble, inside an `<operator-rules>` block: a site's conversation, the
house's, the network agent, each crew agent, and a site's own conversation
when it can change files. It does not depend on the provider: the
preamble is what every agent CLI is given as its system text.

The rules take effect on the next turn of every conversation, with no
restart. Settings are per node: the card offers to save the same rules on
the other nodes of the network.
