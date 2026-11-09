# The crew: autonomous agents you create

The network's agent (`internal/autopilot`) looks after the sites. The crew
(`internal/crew`) is for whatever else the operator wants done: each agent
is made at the panel (**Agents**) with a name and a goal in the operator's
own words, starts off, and once switched on works by itself toward it.

## The network's agent is one of them

The crew has a member from the start, `network`, that is the network's
agent. Its engine stays `internal/autopilot` (one per network, movable
between nodes, watching every site, its guidelines and proposals), so the
crew never sends it a turn and refuses to switch, wake, stop, edit or
delete it (`crew.ErrNetwork`): those are `/autopilot`'s. What it shares
with the rest of the crew:

- **Requests and urgent pushes**: `middleware crew request` from its turns
  lands in `data/crew/network/requests.json`; each wake's prompt carries
  its open and newly answered requests (`Crew.AskText`), and an answer
  wakes the Pilot at once (`Options.OnAnswer`).
- **Vault**: its turns get the vault in their environment, and
  `crew vault [set|rm]` works as for any agent. The autopilot's socket
  caller may reach `/api/v1/crew/network/...` and no other agent's.
- **The panel**: it is first in **Agents**, and its page is
  `#/agents/network` (the old `#/autopilot` links land there); the menu
  no longer has an entry of its own.

## What an agent is

`data/crew/<id>/`:

| File | What it holds |
|---|---|
| `agent.json` | name, goal, switch, agent and model, wake bounds, quota share, its room (`disk_mb`) |
| `state.json` | its session, its running turn, its next wake, why it was held back |
| `requests.json` | what it asked the operator and what they answered |
| `vault.json` | credentials and data, each value encrypted with the house's key |
| `journal.jsonl` | every wake, turn, request, answer, vault change and refusal |
| `desk/` | its working folder, with its HOME (`data/homes/crew~<id>`) the only place it may write; `notebook.md` is its memory, `tools/` what it built |

Its conversations are `crew~<id>#<hex>`, one per session. The tilde is in
no site or project name, so a crew turn is never taken for a site, nor for
the operator.

## How it works

- **Wakes**: as the autopilot's. Every turn ends with `Summary:` and
  `Next:` lines; the house wakes it then, within its bounds, and earlier
  when the operator answers one of its requests (within a minute).
- **Brake**: no turn while a subscription window is above its
  `quota_percent` (80 % by default). No dollar caps.
- **Memory**: every wake is a new conversation, with nothing of the ones
  before. What it needs from them it wrote in its notebook, and it decides
  what to write there and when to read it. The last 30 conversations are
  kept for the operator to read; older ones are forgotten.
- **Jail**: its desk and its HOME. Other agents, its own settings, requests and vault
  on disk, the sites and the house's data are denied (`agent.Jail.Crew`).
- **Socket**: a caller of its own (`auth.CrewScopes`), held to its own
  `/api/v1/crew/{agent}` routes: status, journal, requests, and its vault
  (it may keep and remove values of its own). Never a token's scope.

## Requests

`middleware crew request --title T [--field KEY] [--secret KEY] [--option O] [--urgent] -`

What the operator types into a request's fields goes to the vault, not to
the request, and from the next turn is in the agent's environment as
`$KEY`. An urgent request (a check only a person can pass) is pushed as
critical, and again an hour later if still open. The operator answers on
the agent's page, or talks to it in its **Sessions** tab.

## A room, not a toolbox

The house gives an agent nothing ready-made for its goal: no mail client,
no API wrappers. It gets a room (its desk and HOME), a shell with the
machine's tools and the internet, its vault and the operator, and builds
what its goal needs there: an agent that needs email writes its own SMTP
and IMAP client in `desk/tools`, installs what it needs inside its room
(a virtualenv, a tool's own `node_modules`), notes in its notebook how to
run it, and keeps the account's credentials in its vault
(`middleware crew vault set KEY -`, the value on stdin; never in a file).
What it cannot get or make itself it asks for with a request.

The room has a size, `disk_mb` (2 GiB by default), set on its page. The
house weighs the desk and HOME every ten minutes (`Status.disk_used`, in
MiB); over the size, the journal notes it and every wake tells the agent
to make room before anything else. Nothing is deleted for it.

Its preamble holds it to the services' rules: official APIs before
automating pages, true details, no faked engagement, and no getting round
checks meant for people; those become urgent requests.
