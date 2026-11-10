# Architecture

`middleware` is one Go binary with no third-party dependencies. It runs as an
unprivileged user under systemd, listens on a loopback port (and optionally on
80/443), and is the only thing in front of every site on the machine.

```
                 ┌────────────────────────────────────────────────────────┐
  internet ──►   │  inbound: tunnel port · 80/443 (ACME)                  │
                 │     │                                                  │
                 │     ▼                                                  │
                 │  waf ──► router (Host) ──► proxy ──► site instance     │
                 │                    │                                   │
                 │                    └──► panel / api (panel domain)     │
                 │                                                        │
                 │  engine: reconcile manifests ⇄ running instances       │
                 │     runtime: process | docker                          │
                 │  modules: cloudflare · github · agents · vpn · …       │
                 │  tasks · guard · events                                │
                 └──────────────┬─────────────────────────────────────────┘
                                │ unix socket, closed op list
                 ┌──────────────▼─────────────────────────────────────────┐
                 │  middleware-helper (root): firewall · vpn · sysctl     │
                 └────────────────────────────────────────────────────────┘
```

## The rule that governs everything

**A site lives inside its folder.** `sites/<domain>/` is the whole project,
`.git` included, plus a `site.json` manifest. The manifest is the only state
that matters: stopping a site, holding it behind a page, jailing it in a
container, closing its door — all of it is a field in `site.json`, written
atomically, and all of it survives a restart because the restart re-reads it.

The root directory (`MIDDLEWARE_ROOT`, default `/srv/middleware`) holds:

```
sites/<domain>/site.json     one folder per domain
tasks/<name>.json            scheduled work
data/                        what this installation decided
  settings.json              panel domain, ports, listeners
  modules.json               which modules are on; credentials encrypted inside
  state/<domain>.json        running instance: pid, port, revision (for re-adoption)
  docker-volumes.json        which site owns each docker volume made before volumes carried a label
  logs/<domain>/             site output (rotated by size) and its access.log
  logs/access.log            requests for the names no site serves, one file
                             every access.log (client address, time, request,
                             User-Agent) turns over each Monday into
                             access.log.<YYYY-Www>[.N].gz, never deleted
  visits/<domain>/           the site's audience per month, no addresses
  events/                    audit trail, one file per day
modules/                     binaries a module installed (cloudflared, wg, agent CLIs)
```

## One process per site, always

The engine runs a reconcile loop. Every tick it loads each manifest, resolves
everything that can change underneath it (`env_from_file` pointers, the file's
own content) into a **revision hash**, and compares it with the revision of the
running instance. When they differ, it performs a **handover**:

1. start the new instance on a fresh port, output straight to its log file
   (never a pipe: a grandchild that inherits the pipe would hang `Wait`);
2. wait until the port accepts connections and the health path answers 2xx;
3. switch the router to the new port;
4. keep the old instance draining for `drain` (default 8s), then SIGTERM, then
   SIGKILL.

On a machine there is exactly one instance of a site receiving traffic at
any moment: a WebSocket or an in-memory session needs one process, and a
deployment that wants zero dropped requests gets it from the handover, not
from replicas. `TestHandoverDropsNoRequests` is what keeps it honest. A site
that asks for it in its manifest (`cluster`) also has copies on other nodes
of the network, each of them one process there, which take what the primary
cannot ([Cluster](#cluster) below); a site that says nothing never has one.

### Re-adoption

Sites outlive the supervisor. The unit has `KillMode=process`, so a restart of
`middleware` leaves every site running. On boot the engine reads
`data/state/<domain>.json`, checks the pid is alive and is the process that
was started (the same boot id and the same start tick in `/proc`, which a step
of the wall clock does not move), checks the port is listening, and adopts the
instance without restarting it. A pid that is someone else now is forgotten;
one that is ours but no longer listens is stopped, so the fresh start the next
tick makes is never a second copy. After a binary upgrade the
operator is told that adopted instances still carry the environment they were
born with, and can roll them over with `middleware site restart --all`.

What the supervisor leaves behind when it stops is always in a state file.
On SIGTERM it stops accepting, gives the requests in flight up to 10s to
finish (handlers that never end by themselves, such as the panel's event
stream, are cut after 5s), and ends every handover in flight before it
exits: one that has not switched yet stops its new generation, so the old one
is what the next supervisor adopts; one that has switched cuts its drain short
and stops the old generation.

## Runtimes

`runtime.Runtime` is the interface; two implementations ship.

- **process** (default): `exec.Cmd` with a clean environment (no duplicate
  keys, `PORT` last), a per-site `HOME` under `data/homes/`, output to file,
  process group of its own so a stop reaches grandchildren, also those a
  leader that already exited left behind (the group is signalled when the
  instance exits, before its port is handed out again).
- **docker**: the same manifest, run as a container. The image is built from
  `docker.build` or pulled from `docker.image`; the site folder is bind-mounted;
  the port is published on loopback only. Handover works the same way: two
  containers for a few seconds, then one.

Switching a site between runtimes is a manifest edit.

## Privilege

The supervisor is unprivileged. Everything that needs root goes through
`middleware-helper`, a second systemd service running the same binary with a
different subcommand, listening on `/run/middleware/helper.sock` (owner root,
group `middleware`, mode 0660). It accepts a **closed list of operations**:

| op | what it does |
| --- | --- |
| `firewall.apply` | replace the `middleware` nftables table from a validated rule set |
| `firewall.block` / `firewall.unblock` | add/remove an address from the block set, with a TTL |
| `vpn.apply` | write `/etc/wireguard/mw0.conf` from a validated config and reload |
| `vpn.up` / `vpn.down` | bring the interface up or down |
| `sysctl.reserve_ports` | reserve the site port range from the ephemeral range |
| `update.install` | replace the binary with a newer release of this repository, sha256 checked and synced to disk, keeping the old one as `middleware.prev` and a record of the update on trial next to it; then restart itself. Refuses a release it rolled back from, and anything while an update is on trial |
| `update.confirm` | the supervisor running the release on trial says it serves (listener open, half a minute up, a request answered through it); the trial ends. Answers the release skipped, if any. Without it in five minutes, or with the supervisor in a crash loop, the helper restarted on the new binary rolls back by itself: no operation asks for an older version |
| `requirements.install` | install the house's requirements (packages, docker and its group, nvm with the LTS, fail2ban, optionally a headless Chromium): this binary's own `install --requirements`, for the user and root the installed `middleware.service` names, in a transient unit of its own (`systemd-run`), its output in `/run/middleware/requirements.log`, root's (in the supervisor's `data/` a link there made root write over any file); also what the sites' jails need (an AppArmor profile for the binary, a drop-in turning `ProtectKernelTunables` off for a unit rendered before); no unit rewritten, no binary, no restart |

Each operation validates its input (addresses parse, keys are base64 of the
right length, names match `^[a-z0-9-]+$`), logs what it did, and never takes a
free-form string that reaches a shell. Ports 80 and 443 do not need the helper:
the unit grants `CAP_NET_BIND_SERVICE`.

## Inbound

Two doors, either or both open:

- **tunnel**: a Cloudflare tunnel (or any reverse tunnel) pushes to the
  loopback port. No public IP, no certificates here.
- **ports**: the process listens on 80 and 443 and obtains certificates by
  itself with an ACME client written against RFC 8555 on the standard library.
  Only domains this installation serves get certificates.

The panel answers through any door, always, so closing the one you came in by
cannot lock you out.

## WAF

Layer 7 lives in the core and is always on, configured per site and globally:
rate limits per route, country and user-agent policies, request body limits,
an "under attack" switch (the proof of work itself is `internal/attack`, a
hook after `security` and before `access`), and automatic blocking of
addresses that trip the limits. Blocking also goes to layer 3 through
`firewall.block` when the helper is present, so a flood never reaches Go.

## Modules

A module is something not every installation wants: it declares what it
needs, how to verify credentials, and what it installs. Enabling a module
**verifies before it saves**: every permission is probed and reported as a
line, and what cannot be probed without side effects is said so.

Shipped modules: `inbound-tunnel`, `inbound-ports`, `cloudflare` (DNS and
tunnel routes), `github` (deploy on push), `agent-claude`, `agent-agy`,
`vpn` (WireGuard for the panel and service door), `docker` (runtime).

Credentials are encrypted at rest with AES-256-GCM; the key comes from
`MIDDLEWARE_SECRET_KEY` or from `data/modules-key` (owner-only).

## Agents

`agent.Provider` is the interface a conversation runs on. An adapter turns a
prompt and a working directory into a stream of events (text, tool use, cost,
end). Every adapter drives the vendor's CLI as a subprocess so no SDK
enters the core: `claude`, `agy` (Antigravity), `openai` (Codex) and the
rest. What an agent may touch is decided
by the **jail** in the core (the site folder, nothing above, no other site),
not by the adapter. The CLIs that can be pointed elsewhere keep their own
settings and sessions in `data/agents/config/<domain>/`, not in the site's
HOME, which a container can write.

The **autonomous agent** (`internal/autopilot`) is one for the whole network
of nodes, running on one of them and off until the operator switches it on.
It wakes by itself, looks at every site of every node and decides what to do,
working in conversations of its own (`autopilot#<id>`) and handing work in a
site to that site's conversation, which stays jailed in the site. The agent
decides; the house holds what it may do, never the prompt alone: its socket
caller has its own scopes (no settings, tokens, deletion or publishing of
what it did not make), a manifest's `"autopilot": false` keeps it out of a
site, sites it creates are counted and built off the internet until it says
they are finished, the subscription's windows bound when it works, and every
wake, turn, command and call is a line of its journal. Every wake is a new
conversation; what it chooses to remember is in a notebook it keeps in its HOME.

## Resources

The unit's cgroup is the supervisor's to split (`Delegate=cpu memory pids`,
`DelegateSubgroup=house`), into three branches beside each other:

```
middleware.service/
├── house/            the supervisor and the house's own tasks
├── agents/           every agent turn and whatever it starts
└── sites/
    ├── example.com/  the site's instances, process workers and services, its tasks
    └── api.example.com/
```

Sites cannot go under `house/`: cgroup v2 gives no controllers to the
children of a group that holds processes, and `house/` holds the
supervisor. Processes start inside their group directly (clone into the
cgroup), so nothing a site starts is ever briefly outside it, jailed or
not. `agents/` has a CPU weight of 10 and a ceiling sized from the machine
(every core but a quarter, a third of the memory; `agents.cpu_cores` and
`agents.memory_max` replace them). Each site's group has weight 100, 4096
processes and the manifest's `limits` (see [manifest.md](manifest.md#limits-limits)).
What stays out: commands run for a site (`runtime.Exec`: installs and
migrations run uncapped), docker containers (the daemon's cgroup, with
`docker.*`), and the house's own tasks. A site re-adopted from a version
that started it in `house/` is moved, with everything it started, into its
group. Without delegation everything runs in the unit's cgroup as before,
said once at start (`cgroups.off`).

## API and panel

Everything the panel does goes through `/api/v1`, versioned JSON, with a
bearer token or a session cookie. `/api/v1/events` is a server-sent stream of
everything that happens (instance started, handover done, certificate
renewed, address blocked). The panel is static HTML/CSS/JS embedded in the
binary, no build step; the CLI uses the same API over the unix socket.
The socket is the operator's except to a site's agent turn: it reads which
process connected, walks its parents, and a request from under a site
conversation's turn is confined to that site (`auth.TurnScopes`).

## Linked nodes

Several installations join one network (`internal/nodes`): a WireGuard
mesh with no master, in which every node is a peer of every other on its
own interface (`mwmesh`) and address. Only traffic to another node's /32
goes through the tunnel; the machines' own traffic does not change, and
the helper's `mesh.firewall` table admits on that interface the nodes' API
and nothing else. Membership is a roster each node pulls from the others
every round: a node is learnt from whoever names it, but once known its
entry changes only when read from itself (dated at most a minute ahead),
a removal is final and only a new join undoes it, and an address outside
the subnet or already another node's is dropped, so a compromised node
can neither vote itself back in nor take another's address. Joining one
node is joining all of them. An invitation carries a key pair
made for the newcomer so the first contact already goes through the
tunnel; the newcomer replaces it with its own on joining. The panel keeps
per tab the node it works on and sends every call through
`/api/v1/nodes/{id}/api/…`, a streaming reverse proxy to that node's mesh
door: a build, a chat turn, an agent run on the machine that owns the
site. The dashboard is one request, `/nodes/overview`, which asks every
node's `/node/summary` in parallel.

## Cluster

A site with `cluster` in its manifest has replicas on other nodes
(`internal/cluster`). Its primary keeps where they are, copies its folder
to them at every change (dependencies installed there for that machine),
and, after the cache in the request chain, hands a request to one of them
through the mesh door when it is saturated or its instance is down, by
weighted rendezvous hashing on the visitor's address. A replica is behind a
closed door: not published, not served by its node's own doors, reached
only by its primary with the network's secret. The database gateway passes
a replica's connections to `127.0.0.1:<port>` through the mesh to the
primary's database. See [cluster.md](cluster.md).

## Jails

Every process a site has runs in a jail of the site's own
(`internal/jail`): its process, workers, tasks, a command from the
panel, a deploy's build, its conversations' turns. The runtimes build
the command as before and hand it to `jail.Wrap`, which re-executes this
binary as the jail's init (`middleware __jail <plan> -- command`) in new
user, mount, pid, ipc and uts namespaces, the supervisor's own uid inside.
The init builds a root out of an empty tmpfs and the plan's mounts, each
at its real path (so `HOME`, `MIDDLEWARE_SITE_DIR` and the socket keep
their names), pivots into it, drops its capabilities and starts the
command as pid 2, passing signals on and reaping orphans; when the
command ends the init exits with its status and the kernel ends what is
left in the namespace. From outside nothing changes: the recorded pid is
the init's, its process group is the one Stop signals, and a restarted
supervisor adopts it the same way. `jail.Available` builds one jail at
start; where it fails the sites run as before and delegates lose `code`
and `agents` on that node. The house's own tasks and conversation are the
operator's and run outside.

## Delegated users

The operator is the house's one full account; below it are delegated
users, each allowed some sites and, on each, some of `view`, `operate`,
`code` and `agents` (`internal/auth/perms.go`). A user signs in at the
node they were made on and the panel shows them their sites as if they
were all there: a request for a site this node does not hold is relayed
to the node that does (found by asking the live nodes, kept for a
minute), with the user and their grant for that site in
`X-Middleware-Delegate`, believed only on the mesh door. That node holds
the request to the same lists, and keeps the manifest keys that reach
past the site out of the user's hands, since only it has the manifest.
`GET /sites` and the event stream for a user are this node's and the
other nodes' put together, narrowed to their sites.

## Events, not logs

Anything that changes state emits an event with a type, the subject and who
caused it (operator, agent, guard, schedule). Events go to the SSE stream, to
`data/events/`, and to notifiers. If something can fail and look the same as
if it worked, it emits an event either way.
