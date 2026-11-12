# The command line

One binary, many subcommands. `middleware help` lists what the build you have
registers; this page describes the intended set. Everything that is not the
supervisor itself talks to it through the unix socket `data/api.sock` (no
credentials: whoever can open it owns the files) and so needs the service
running, or `--root` pointing at the installation when it is not the
default.

Common flags: `--root <dir>` (default `$MIDDLEWARE_ROOT`, then the current
directory if it holds `sites/`, then `/srv/middleware`), accepted before or
after the verb. The commands that go through the API also take `--json`.

Two kinds of command: the ones that talk to the running supervisor over the
socket (`site`, `events`, `system`, `token`, `setup`, `agents`), and the ones that
work on the files under the root and need no running process (`modules`,
`inbound`, `waf`, `guard`, `tasks`, `chat`, `skill`, `vpn`); the supervisor notices
what the second kind wrote on its next look. What the CLI does not cover is
in the API ([api.md](api.md)) and the panel.

## The process

| command | what |
| --- | --- |
| `serve [--root]` | run the supervisor. What `middleware.service` runs; from a shell it serves until Ctrl-C. Takes the listener from systemd when `LISTEN_FDS` is set. |
| `helper --socket <path> --allow-uid <user> --allow-gid <group> [--socket-group]` | the privileged helper. Runs as root under `middleware-helper.service`; answers a closed list of operations on the socket. Not meant to be run by hand. |
| `helper ping [--socket]` | check the helper answers and print its protocol and build |
| `version` | print the version the binary was built with |
| `help` | list the commands |

## Installation

| command | what |
| --- | --- |
| `install [--root] [--user] [--no-helper] [--no-start] [--yes]` | put middleware on this machine; one line per step, re-runnable. [install.md](install.md) |
| `install --requirements [--browser yes]` | only the house's requirements: packages, docker and its group, nvm with the LTS, fail2ban, a headless Chromium with `--browser yes`; no unit, binary or restart. What Sistema → "The house's requirements" runs through the helper |
| `uninstall [--yes] [--keep-binary]` | stop and remove the units. Keeps the root directory and the sites. |
| `doctor` | binary, units, loopback socket, helper, root directory, reserved ports, docker, wg, disk: one line each, exit 1 if any fails |
| `setup [--password]` | create the operator: prints a one-time code to enter in the panel, or asks for the password on the terminal with `--password`. Refuses once an operator exists. |
| `setup --join <code> [--endpoint ip:port]` | join the network another node's invitation was made in, through the running service: no domain and no operator of its own, managed from the network's panels. What `install --join` runs. |

## Sites

`middleware site …` mirrors `/api/v1/sites` ([api.md](api.md)).

| command | what |
| --- | --- |
| `site list` | every folder under `sites/`, managed or not, with mode, port and state |
| `site show <domain\|name> [--service s]` | the site object: manifest, instance, workers, and its services with their names, port, revision, state and error; a name a site serves shows that site (and that service) |
| `site routes <domain>` | every name the site answers: host (and prefix) → service → port → state |
| `site add <domain> [--template static\|node\|go\|docker\|fastapi\|django\|php\|astro\|project] [--manifest file.json\|-]` | a folder with a `site.json` |
| `site start <domain> [--service s]` / `site stop <domain> [--service s]` | clear or set `disabled` in the manifest (`services.<s>.disabled` for one service, the others untouched) |
| `site restart <domain> [--service s]` / `site restart --all` | force a handover (of one service with `--service`, the others untouched); `--all` after an upgrade, see [upgrade.md](upgrade.md) |
| `site hold <domain> [--service s] [--title t] [--message m]` / `site unhold <domain> [--service s]` | the holding page instead of the process (on one service's names with `--service`) |
| `exec <domain> [--service s] [-t] [--] <command…>` | a one-off command where the site runs: in its container, or a service's (its running image, on the site's network). From a site's turn, whose jail has no docker, a container's command goes through the house (`exec:<domain>`), output as it comes and the command's exit code as its own |
| `site logs <domain> [-f] [--lines 200] [--worker name \| --service name [--build]]` | the instance's output, a worker's, a service's, or a service's image build |
| `site deploy <domain>` | run `deploy.github.build` and write the pointer, [deploy.md](deploy.md); through the github module's runner when it is enabled |
| `site validate <domain> [--file site.json\|-]` | check a manifest without writing or deploying it: what the house would refuse, the names it would answer and whether another site has them, Dockerfiles, build contexts and folders that are not there. Exits 1 when it would not work. A site's own conversation may run it for its site |
| `site rm <domain> [--archive]` | remove (with its tasks and what the house kept by its name; its access logs and database copies stay), or move to `data/archive/` |
| `site tasks <domain>` (or `… list`) | the site's own tasks (the ones whose `site` is that domain) with their schedule, next and last run; `--json` prints the API's rows |
| `site tasks <domain> save <name> <file.json\|->` | write a task from a file or stdin; with no `site` it is this one's. Refused when the name is another site's or the root's |
| `site tasks <domain> rm <name>` / `run <name>` / `stop <name>` / `log <name> [--lines n]` | delete it (history and log stay), start a run now without waiting, stop a run, its output. Through `/sites/{domain}/tasks` ([api.md](api.md#tasks)), so it is what a site's agent turn and a `tasks:<domain>` token may do; `middleware tasks` is the house's, over every file |
| `site security <domain> [seen \| blocks \| catalog]` | who calls at the site (countries, robots with their family, browsers, User-Agents no table knows; heaviest first), the last 50 requests its `security` refused with which list and entry did it, and the families, robots, browsers and probe paths a rule may name (compiled in, nothing asked). The panel's Security tab, for a site's agent turn and a `read:<domain>` token; `--json` prints the API's answer |
| `site staging <domain> [status \| create [--name n] [--publish] [--door d] \| promote \| undo \| refresh]` | test copies ([manifest.md](manifest.md#staging-test-copies)): what the site has (`status`), a copy of it with its data behind a password printed once (`create`, default `staging.<domain>`), the copy's code put into its production with production's data kept (`promote`, run on the copy), the last promotion put back (`undo`, on production), production copied into the copy again (`refresh`). Owner only |
| `site network [<domain>] [--days 7]` | what a site moves over the network: the rate in and out now, today, this month (and the part that came by its public ports), day by day, its budget and state; with no domain, every site of the node, the heaviest of the month first (`--json` prints the API's answer) |
| `site visits <domain> [--days 7]` | people and pages by day, the top pages, links in, countries and devices: the panel's Visits tab in a few lines (`--json` prints the API's answer) |

## Everything else

| command | what |
| --- | --- |
| `events [-f] [--since] [--type prefix] [--subject domain]` | the audit trail; `-f` streams |
| `system` | version, root, uptime, listeners, load, memory, disk |
| `system restart` | SIGTERM to the supervisor; systemd restarts it, sites keep running |
| `token list` / `token add <name>` / `token rm <id>` | API tokens; the token is shown once |
| `modules` (or `modules list`) / `modules verify <key> k=v…` / `modules enable <key> k=v…` / `modules disable <key>` / `modules forget <key>` | [modules.md](modules.md); `enable` verifies before it saves and prints one line per check; a value of `@file` or `-` reads the value from a file or stdin so a token never sits in the shell history. `forget` disables and deletes the credentials |
| `inbound status` / `inbound open <door>` / `inbound close <door>` | the three doors (`tunnel`, `ports`, `local`) and the certificates on disk. Moving domains and renewing is `POST /inbound/move` and `POST /inbound/certificates/{domain}/renew` in the API |
| `site publish <domain> [--door d]` / `site unpublish <domain>` / `site published` | put every name of a site on the internet, take them back, and list what the house published with each name's state |
| `site ports <domain> [approve\|revoke]` | the public ports the site's services ask for (`services.<name>.ports`, `network: host`): which are approved and which wait, the node's public address, whether the firewall has them, and what else holds one. `approve` is the owner's yes to what the manifest asks now; `revoke` withdraws it and stops those services |
| `site key <domain> on` / `site key <domain> off` | hide a site behind a key in its address (`access.key`, [manifest.md](manifest.md)): `on` makes a new key and prints the address with it, the only time it is shown; `off` removes it |
| `migrate [--sites dir] [--apply]` | convert V1 manifests; see [upgrade.md](upgrade.md) |
| `import --to <url> --token <token> [--domain d] [--command …] [--stop …] [--start …] [--tasks-off] [--dry-run] <folder>` | run where the site is now: move one site into a V2 house with the token its panel made (Sites → Import a site), from a V1 house (`<V1 root>/sites/<domain>`) or any app folder on any server. It says first what it will do (what is copied, where dependencies are installed and with what, links that leave the folder); `--tasks-off` has a V1 site's tasks arrive turned off (a stay on a machine too small for them); `--dry-run` asks the target whether it would take it (the name, every name the site answers to in its DNS, the tasks, docker) and stops there. See [upgrade.md](upgrade.md) |
| `inbound local [on\|off] [--http addr] [--dns addr]` | the development bench: plain HTTP and a name server for the sites on this machine; prints the resolver snippet. See [install.md](install.md#local-development-bench) |
| `waf list` / `waf block <ip> [duration] [reason]` / `waf unblock <ip>` | the block list, [security.md](security.md); the duration defaults to one day. Under-attack mode (`attack` in the manifest, [manifest.md](manifest.md)) and the traffic are `/waf/under-attack` and `/waf/traffic` in the API |
| `guard incidents` | what the guard found and has not been dismissed; dismissing is `POST /guard/incidents/{id}/dismiss` |
| `tasks` (or `tasks list`) / `tasks run <name>` | scheduled work, [manifest.md](manifest.md#tasks-tasksnamejson); `run` waits and prints the log's path. The log itself is `GET /tasks/{name}/log`. One site's tasks through the API: `site tasks <domain>` above |
| `autopilot status` / `journal [--limit n]` / `sites` | the network's autonomous agent ([api.md](api.md#the-autonomous-agent)): on or off and on which node, the session's context, the subscription's windows, its next wake; what it did; the sites it made and whether each is published |
| `autopilot on` / `off` / `stop` / `wake` / `move <node>` | the switch; `stop` is the kill switch (its turn and every site turn it started cut, and off after a restart); look now; hand it to another node of the network |
| `autopilot ask <domain> -` / `autopilot publish <domain>` | the agent's own: hand work to a site's conversation (the message on stdin), say one of its sites is finished |
| `autopilot propose <domain\|house> --title T -` | the agent's own: propose a change it may not make (the text on stdin); the operator sends it to be fixed or dismisses it |
| `autopilot proposals [<id>]` / `autopilot proposals send\|dismiss <id> [--note N]` | its proposals and what was decided (an id prints its text); the operator sends one to be fixed in their name, or dismisses it |
| `agents [--json]` | the providers, whether each can run, and the capacity: how many turns run of the cap, per provider, and which conversations hold them (`GET /agents`) |
| `chat [--model m] <domain> <text…>` / `chat middleware <text…>` | a turn with the site's agent; progress is printed from the event stream |
| `skill install [dir]` | write the skill the conversations run with (embedded in the binary, installed into every conversation's HOME before each turn) to `<root>/.claude/skills/middleware/`, or to `dir`, so Claude Code run by hand on the server loads it too. Needs no running process |
| `deps status` / `deps check` / `deps install <claude\|agy>` | the agent CLIs under `modules/node`: what runs, the latest published, install or update now. The daily check does this on its own ([modules.md](modules.md)) |
| `panel open [duration]` / `panel close` / `panel status` | the fire exit: lift the panel's address lists and closed password for a while (30 minutes unless told) |
| `panel key on` / `panel key off` / `panel key status` | hide the panel behind a key: without `?access-token=<key>` once in the address its name answers like a name nobody serves. `on` makes a new key and prints the address with it (the only time it is shown; an earlier key stops working); `off` is the way back when it is lost. Works on `data/inbound.json` alone, the running process reads it on its next request |
| `vpn status [--json]` / `vpn peer add <name>` / `vpn peer remove <name>` | the WireGuard door for the panel; `--socket` names the helper's socket. "VPN only" is `POST /modules/vpn/only` in the API |
| `install` / `uninstall` / `doctor` | above |

## Exit codes

`0` done, `1` the command failed and said why on stderr, `2` bad usage.
`doctor` returns `1` when any check fails so it can sit in a cron line.

## Environment

| variable | effect |
| --- | --- |
| `MIDDLEWARE_ROOT` | the root when `--root` is not given |
| `MIDDLEWARE_SECRET_KEY` | key for the credentials store; otherwise `data/modules-key` |
| `LISTEN_FDS` | set by systemd socket activation; `serve` takes fd 3 as its listener |
