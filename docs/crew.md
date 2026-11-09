# The crew: autonomous agents you create

The network's agent (`internal/autopilot`) looks after the sites. The crew
(`internal/crew`) is for whatever else the operator wants done: each agent
is made at the panel (**Agents**) with a name and a goal in the operator's
own words, starts off, and once switched on works by itself toward it.

## What an agent is

`data/crew/<id>/`:

| File | What it holds |
|---|---|
| `agent.json` | name, goal, switch, agent and model, wake bounds, quota and context shares |
| `state.json` | its session, its running turn, its next wake, why it was held back |
| `requests.json` | what it asked the operator and what they answered |
| `vault.json` | credentials and data, each value encrypted with the house's key |
| `journal.jsonl` | every wake, turn, request, answer, mail and refusal |
| `desk/` | its working folder, the only one it may write; `notebook.md` is its memory |

Its conversations are `crew~<id>#<hex>`, one per session. The tilde is in
no site or project name, so a crew turn is never taken for a site, nor for
the operator.

## How it works

- **Wakes**: as the autopilot's. Every turn ends with `Summary:` and
  `Next:` lines; the house wakes it then, within its bounds, and earlier
  when the operator answers one of its requests (within a minute).
- **Brake**: no turn while a subscription window is above its
  `quota_percent` (80 % by default). No dollar caps.
- **Context**: at `context_percent` (60 %) the next wake opens a new
  session, and it picks up from its notebook.
- **Jail**: its desk. Other agents, its own settings, requests and vault
  on disk, the sites and the house's data are denied (`agent.Jail.Crew`).
- **Socket**: a caller of its own (`auth.CrewScopes`), held to its own
  `/api/v1/crew/{agent}` routes: status, journal, requests, vault names,
  mail. Never a token's scope.

## Requests

`middleware crew request --title T [--field KEY] [--secret KEY] [--option O] [--urgent] -`

What the operator types into a request's fields goes to the vault, not to
the request, and from the next turn is in the agent's environment as
`$KEY`. An urgent request (a check only a person can pass) is pushed as
critical, and again an hour later if still open. The operator answers on
the agent's page, or talks to it in its **Sessions** tab.

## What the house gives every agent

The shell and the internet in its desk; email from its own account over
SMTP and IMAP (`middleware crew mail send|list|read`, with `SMTP_*` and
`IMAP_*` in its vault; IMAP falls back to the SMTP details). A capability
it lacks the names for is listed as not ready, and the agent asks for them.

Its preamble holds it to the services' rules: official APIs before
automating pages, true details, no faked engagement, and no getting round
checks meant for people; those become urgent requests.
