# Upgrading

The binary is swapped, the supervisor restarts, the sites keep running.
That is the whole procedure; what follows is why it is safe and what to do
after.

## From the panel

System → Updates says which release runs and which is the newest, and
**Update now** installs it: the privileged helper downloads it from this
repository, checks its sha256, replaces `/usr/local/bin/middleware` and
both services restart; the sites keep running. **Update automatically**
(off by default; `settings.json` `update.auto`) does the same once a day.
An installation running a pre-release is offered pre-releases; one on a
stable release, stable ones only. Without the helper (`--no-helper`, a
container) the panel says to use the procedure below.

### An update is on trial until it serves

The helper keeps the binary it replaced as `/usr/local/bin/middleware.prev`
and writes a record, `/usr/local/bin/middleware.update.json`, with the
release it came from, the one it installed and a deadline five minutes
away. The new supervisor confirms (`update.confirm`) once its listener is
open, it has run for half a minute, and a request through that listener
came back. If the deadline passes without it, if the supervisor restarted
three times meanwhile, or if systemd gave up restarting it, the helper
puts `middleware.prev` back by itself, restarts `middleware.service` and
ends so systemd starts it on the old binary too. Event `update.rolled_back`
says so at the first start after.

The release rolled back from is **skipped**: the panel shows it
(`skipped` in `GET /system/update`), it is not offered, and the helper
refuses to install it, by hand as well, until a newer one is out. That
keeps automatic updates from installing it again at the next check. Both
files are root's, written with `fsync` of the files and of the folder,
and the supervisor cannot write either.

A machine rebooted while an update is on trial and back after the
deadline rolls back at once: a good release skipped costs a release
cycle, a bad one kept costs every site. The install script below goes
around all of this; an operator who installed a binary by hand is not
second-guessed.

### Not in the middle of a move

An automatic install ends in a restart of the supervisor, and a site
moving between nodes is the supervisor copying it: restarted then, the
move is cut. So an automatic install waits while a move runs (event
`update.deferred`, once), and is looked at again a minute later rather
than at the next interval. **Update now** does not wait: the operator
chose.

## The procedure

```sh
curl -fsSL https://raw.githubusercontent.com/dcimorra/the-middleware/main/scripts/install.sh | sh
```

or, with a binary you built or downloaded yourself:

```sh
./middleware install
```

The installer compares the binary's hash with the installed one, copies the
new one into `/usr/local/bin/middleware`, rewrites the units if the
templates changed (then `daemon-reload`), and restarts
`middleware-helper.service` and `middleware.service`. It does **not**
restart `middleware.socket`: systemd keeps holding `127.0.0.1:46100`, and
connections that arrive during the restart wait in the accept queue
instead of being refused.

By hand, the same thing is:

```sh
sudo install -m 0755 middleware /usr/local/bin/middleware
sudo systemctl restart middleware-helper middleware
middleware doctor
```

## Why the sites keep running

`middleware.service` has `KillMode=process`. On restart systemd signals the
supervisor only, not its control group; the sites, which are its children,
go on serving. When the new supervisor starts it reads
`data/state/<domain>.json`, checks each pid is alive and its port listening,
and **re-adopts** the instance: the router points at it again without a
restart. Requests to a site during the few hundred milliseconds of the
supervisor's restart queue on the socket; nothing is refused.

Two things do not survive a supervisor restart:

- **Open WebSockets** through the proxy. They are raw sockets the process
  owns ([deploy.md](deploy.md#what-breaks-a-websocket)). Clients reconnect.
- **A handover in progress.** The new supervisor finds two instances for
  the site and finishes the handover: the healthy newer one wins, the other
  is drained.

### The unit and the sites' cgroups

Since v2.0.0-alpha.109 every site runs in a cgroup of its own
([architecture.md](architecture.md#resources)) and the unit says
`Delegate=cpu memory pids` with `ProtectControlGroups=no`. The binary
updates itself and the unit does not: «Instalar requisitos» in Sistema
writes a drop-in (`middleware.service.d/cgroups.conf`) that gives an older
unit both lines and restarts the supervisor, and `middleware install` (or
the one-liner) rewrites the unit. Units up to alpha.109 had
`ProtectControlGroups=yes`, which mounts the cgroups read-only for the
whole unit: on those the split never happened, the agents' included, and
the start says `cgroups.off` ("read-only"). Until then:

- A unit with `Delegate=cpu memory` (from before): sites get their groups,
  CPU shares and memory limits; the process cap is off, and the start says
  so once (`cgroups.sites`).
- A unit without delegation: everything runs as before (`cgroups.off`).

Sites already running when the new version starts are in `house/`; at
re-adoption each one is moved into its group with everything it started,
without a restart. Afterwards `house/` holds only the supervisor and the
house's own tasks. No site gets a memory ceiling it did not ask for: the
manifest's `limits` and the settings' `sites.default_limits` are empty
unless someone writes them.

## Roll the sites over

A re-adopted instance still carries the environment it was born with and
the descriptors it inherited. If the upgrade changed how instances are
started (a new variable, a different `HOME` layout, a fix in the
environment builder), running instances do not have it until they are
handed over. The release notes say when that is the case. To roll them all
over, one at a time, each through a normal zero-downtime handover:

```sh
middleware site restart --all
```

The panel shows the same as a banner after an upgrade until it is done.

## Rolling back

The release before the last update is at `/usr/local/bin/middleware.prev`
until the next one:

```sh
sudo install -m 0755 /usr/local/bin/middleware.prev /usr/local/bin/middleware
sudo systemctl restart middleware-helper middleware
```

Any other release: install the previous binary the same way; the state files are forward and
backward compatible within a major version. Releases are at
`https://github.com/dcimorra/the-middleware/releases`; each has its
`.sha256`.

```sh
curl -fsSL -o middleware https://github.com/dcimorra/the-middleware/releases/download/v2.0.0/middleware-linux-amd64
curl -fsSL https://github.com/dcimorra/the-middleware/releases/download/v2.0.0/middleware-linux-amd64.sha256 | sed 's/ .*//' > want
sha256sum middleware | sed 's/ .*//' | diff - want && chmod +x middleware && ./middleware install
```

## Checking

```sh
middleware version
middleware doctor
middleware events --since 10m
```

`doctor` fails the `binary` line when the installed binary and the one
running the command differ in version, which is what you see when the copy
step did not happen.

## From v1

The directory layout (`sites/<domain>/` as the whole project) is the same, so
the folders stay where they are; only the manifests change shape.

```
middleware migrate --sites /path/to/sites          # report, writes nothing
middleware migrate --sites /path/to/sites --apply  # convert the ready ones
```

Each site is reported as **ready** (with notes on what changed and why
nothing is lost) or **blocked** by the V1 settings it uses that V2 cannot
honour yet. A blocked site is not touched: converting it without the setting
would publish something that looks migrated and has quietly lost a door, a
VPN rule or its container. `--apply` keeps the original as `site.v1.json`
next to the new one and never overwrites that backup, so it can be run again
as V2 gains features, and going back is renaming a file. V1 password hashes
are carried as they are (same algorithm); passwords written in the clear are
hashed. Folders V1 ignored (`_retirado-…`) are reported and skipped.

### One site at a time, into another machine

To move a V1 house's sites into a V2 house on another machine, one by one
and each reversible, the V2 panel's **Sites → Import from V1** makes a
token for that one site (two hours) and the command to paste on the V1
machine:

```sh
/tmp/middleware-v2 import --to https://panel.example.com --token mw_… /srv/v1/sites/shop.example.com
```

Before anything moves, the V2 house is asked whether it can take the site:
its name, docker if it needs it, its tasks' names, and every name it answers
to (aliases and their `www` included), which its DNS provider must be able
to point; a name in a zone it does not have stops the import there, with
the V1 site untouched. `--dry-run` asks the same and stops.

It converts the manifest (refusing a site V2 cannot honour yet), copies the
folder while the site serves (without `node_modules`, which is installed on
the V2 side for its machine), stops the site in V1 by writing `disabled` in
its manifest, sends what changed, and starts it in V2. A database that lives
in the folder (SQLite, with its `-wal` and `-shm`) travels with it, copied
with the site stopped. If the V2 copy does not come up, V1 is started again
as it was. Once it answers in V2 its names are pointed there by
themselves, over what pointed at V1 (with V2's DNS provider on; without
one they wait in Publish); archive it in the V1 panel once it has served a
while. What V1 keeps of the site under its own `data/` (conversations,
cost) stays in V1.

Its tasks (the V1 `tasks/*.json` whose `site` is it) go with it: converted
(`timezone` kept, or the V1 machine's zone written down when the task had
none; V1's unstated 30-minute timeout written; `group` dropped; a bare
program such as `tsx` pointed at the site's own `node_modules/.bin` when it
has one, otherwise said in the plan, since V1 found it in its own `PATH`),
placed in V2 with the site, and turned off in V1 by writing `disabled` in
each, so the same job never runs on both machines. An import that is
undone puts each V1 task back word for word.

A database outside the folder (PostgreSQL in a container, say) is something
V1 has no words for. A `site.v2.json` next to the V1 `site.json` is the
manifest the site will have in V2, written by a person, and the import takes
it as it is instead of converting: there the database is a worker with
`runtime: docker` and a `move` that says how its data goes ([the
manifest](manifest.md#workers)). Its `export` runs on the V1 machine as the
person running the import, against the database the V1 site used (a
`docker exec` into its container, for one), with the site stopped; the dump
travels in the last copy and is imported in V2 before the site starts.
What the site makes again on demand need not travel: `move.exclude` in its
`site.v2.json` ([the manifest](manifest.md#move)) leaves it behind.

### From any server

The same command takes an app from a machine that is not a V1 house: a
folder anywhere, run by pm2, systemd, a container or a screen. Its manifest
is its own `site.json` when it has one (V2, or V1 converted), or worked out
from what it holds: a `Dockerfile` (in a container, its exec-form `CMD`),
`package.json`'s `start`, an `index.html` at the top or in `public/`;
otherwise `--command` says it. `--stop` and `--start` say how the app is
stopped there for the last copy and started again if the import fails;
without them it is copied running, and a database written meanwhile may
arrive torn (the command says so first).

```sh
/tmp/middleware-v2 import --to https://panel.example.com --token mw_… --domain shop.example.com \
  --stop "pm2 stop shop" --start "pm2 start shop" /var/www/shop
```

