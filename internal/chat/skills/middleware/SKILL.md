---
name: middleware
description: How to build, run, deploy, publish and check a site, worker or task on a machine served by middleware; what the house already does for a site (blocking robots, User-Agents, browsers and countries, passwords, IP lists, rate limits, redirects, https, headers, cache, backups, an AI) so it is not rebuilt in the app; and how to see from a site's conversation why its tasks failed, its events, logs, OOM kills, limits and backups. Read before creating or changing anything, before writing a restriction or a feature the house may already have, and before saying you cannot see or do something.
---

# middleware

One Go process sits in front of every site on this machine. It reads the
`Host` header and either serves `sites/<domain>/` or forwards to the process
that site's `site.json` describes. There is no pm2, no nginx config, no
systemd unit per site: **the manifest is the only state a site has**, and the
engine re-reads it every two seconds. Anything you want the house to do, you
do by writing a file; anything you want to know, you ask the house.

**Before you say you cannot see or do something, ask the house.** From a
site's conversation, files outside your folder are denied, but what they
hold about *your* site is not: why a task failed (`middleware site tasks
<domain> log <task>`), why the process was killed or restarted
(`middleware events --subject <domain>`), its backups, its limits, a
restart, a deploy. The full list is in § 2, "What a site conversation can
ask the house". Say you cannot only after the command for it has refused.

## 1. First: what kind of thing is being asked for

Four kinds, and choosing wrong shows at once.

| They ask for | It is | It goes in |
| --- | --- | --- |
| something that answers a domain | a **site** | `sites/<domain>/` with a `site.json` |
| a process that must always be up and answers no domain (an API behind a frontend, a queue consumer) | a **worker** | `workers` inside the site it belongs to |
| workers, tasks and a conversation with no domain at all (a bot, a pipeline) | a **project** | `sites/<name>/`, a name without a dot, with only `workers` in its `site.json` |
| a copy of a site's **database** (MySQL/MariaDB, PostgreSQL, SQLite, MongoDB), now or every night | a **backup** | `backups` in the site's `site.json`, never a task with a dump script (§ 9, "Database backups") |
| something else that happens because it is time (purge, sync, prune old builds, a backup of files) | a **task** | `tasks/<name>.json`; from a site's conversation, `middleware site tasks <domain> save` (§ 9) |
| something not every installation wants (DNS at a provider, deploy on push, a VPN, an agent) | a **module** | already shipped: enable it, never reimplement it in a site |

Rules of thumb:

- A backend that must not drop connections on deploy is **a site with `run`**
  fronted by the one that calls it, not a worker: workers restart, sites hand
  over.
- A thing that runs once and takes long (a build, a migration) is neither: it
  is a detached job from this turn (§ 5). If it repeats, it is a task.
- A domain is lower-case ASCII, `a-z0-9-` and dots. A folder is a route and a
  `Host` header arrives lower-cased; a capital letter never matches.

### Before writing it in the app: what the house already does

Much of what a site is asked for is a line in `site.json`, enforced by the
house before the request reaches the site's process. Look here **before**
writing a middleware, a filter, a cron or a script in the app. A site's
agent once kept a crawler out with a User-Agent check in the app's code
because it did not know the house had it: the rule was invisible in the
panel, cost the app a request per hit, was gone on the next framework
upgrade, and the Security tab, which shows who calls and what was refused,
knew nothing of it. When the house has it, use the house's; when the app
already does it on its own and the person asks to tidy it, move it to the
manifest and take the code out.

| Asked for | The house's way | Where |
| --- | --- | --- |
| keep out a crawler, AI crawlers, SEO bots, `curl`/scripts, a User-Agent, a browser, a country | `security` (`bots`, `agents`, `browsers`, `countries`) | § 8, "Who gets in by what it says it is"; panel: site → Security |
| AI crawlers only by day, in at night | `security.bots.window` | § 8 |
| never serve `/wp-login.php`, `/xmlrpc.php`, an old `/admin` | `security.paths` (404 before the site); `.env`, `.git/`, `.ssh/`, keys and `/etc/passwd` are already cut on every site | § 8 |
| a password for the site or for `/admin` | `access.password`, `access.routes` (set from the panel: it hashes) | § 8 |
| only some networks, or ban some addresses | `access.allow`, `access.deny`, per path in `access.routes` | § 8 |
| a part of the site reachable only through the VPN | `vpn.paths` (operator only, needs the VPN module) | docs/manifest.md |
| too many requests, uploads too large | `waf.rate`, `waf.max_body` | § 8 |
| a flood, "we are under attack" | `attack` (proof of work, with an `until`) | § 8 |
| exploit probes, scanners, brute force on a login | already done for every site: the bans (an address is banned for an hour and longer at each repeat) | nothing to write |
| send an old URL or a whole name elsewhere | `redirects` | § 8 |
| http → https | `force_https`, name by name | § 8 |
| HSTS, `X-Frame-Options`, CORS, any header on every answer | `headers` | § 8 |
| cache pages or assets | `cache` | § 8 |
| gzip the CSS, JS, HTML, JSON and SVG | already done for the files the house serves (a static site, `static`): compressed once per change, `Vary: Accept-Encoding`; a site with `run`/`upstream` compresses in its own server | nothing to write for static files |
| long browser cache for the built JS/CSS | already done for a static site's hashed files (`index-4f2a9c1e.js`): `immutable` for a year; keep the bundler's hashed names | nothing to write; `cache.static_max_age` to override |
| a maintenance page, a site gone (410) | `hold` | § 3 |
| the page a visitor sees while the site starts or is down | `notice.theme` | docs/manifest.md |
| another name, its `www.` | `aliases`, `www` (operator only) | § 3 |
| a copy of the database every night | `backups` | § 9 |
| look at what is in the database, from the panel | site → Database: the databases `backups` names and the SQLite files of the folder, read-only | nothing to write; name a PostgreSQL/MySQL one in `backups` |
| something every so often | a task | § 9 |
| a process always up, or several processes | `workers`, `services` | § 3 |
| an AI in the site's code | `claude_api` (the AI gateway) | § 8 |
| deploy on push | `deploy.github` | § 4 |
| memory and CPU budget | `limits` (operator only) | § 3 |
| how many people visit, from where | the house already counts it: site → Visits | nothing to write |
| a preview per branch / per pull request | `previews: {branches: ["feature/*"]}` beside `deploy.github` (operator only): each pushed branch gets `<branch>-preview.<domain>`, a password-protected copy deploying that branch, removed with the branch or after `keep` (7 days) | docs/manifest.md, "Previews" |
| canary / progressive deploys, "try the new version on some visitors first", automatic rollback | `run.canary: {percent, for}` (not with `run.port` or services): a new generation gets that share of visitors, is rolled back on errors and takes over otherwise; `GET /api/v1/sites/<domain>/canary`, promote/rollback buttons on the site page | docs/manifest.md, "Canary deploys" |
| a public status page, uptime, "is it down?" | `"status_page": {"path": "/status"}` (the house serves it in front of the site, with 90 days of uptime and outages; JSON with `?format=json`); the uptime of any site is at `GET /api/v1/sites/<domain>/uptime` | docs/manifest.md, "Status page" |
| Core Web Vitals, "is the site fast for real people", LCP/INP/CLS | `"web_vitals": true` in the manifest (off by default; turn it on only when asked): the house adds a same-origin script to every page and reports the 75th percentile per day, device and page in site → Visits, and at `GET /api/v1/sites/<domain>/webvitals`; never add web-vitals or an analytics script to the code for it | docs/manifest.md, "Web Vitals" |
| an access log of every request | the house keeps one per site, turned over weekly, readable as your own logs | nothing to write |
| copies on other nodes | `cluster` | § 3 |
| a staging / test environment, "try it before it goes live", promote it after | a test copy: site page → Test copies, or `middleware site staging <domain> create` (operator only); the copy's code sees `MIDDLEWARE_STAGING`, `staging.keep` says what of production a promotion keeps | docs/manifest.md, "Staging" |

"Operator only" keys are refused when a delegated user's account writes
them; tell the person and leave them for the operator. Everything else in
this table a site's conversation writes itself, in `site.json`.

## 2. Where you are

There are two conversations and the house tells you which one you are in.

**The site conversation.** `cwd` is `sites/<domain>/`, the whole project
including `site.json`. You may read, edit and commit anything inside it. You
must not read, write or run anything outside it: other sites, `data/`,
`tasks/`, the root. Every tool call is audited, and a denied path that merely
appears in your transcript is reported as a violation. Your `HOME` is private
to this site (`data/homes/<domain>`); the site's process gets the same one.
Denied as files is not out of reach: your site's tasks, their logs, its
events and its backups are read through the house's CLI (below).

**The house conversation** (named `middleware`). `cwd` is the installation
root (default `/srv/middleware`). You may edit the installation's own files,
its settings and its tasks, and **create new sites**. You must not touch a
project under `sites/`: each site has its own conversation for that.

### Creating a site from the house conversation

Two ways, both end with the engine picking the folder up within two seconds.

By hand, which shows what happens:

```sh
mkdir -p sites/shop.example.com
cat > sites/shop.example.com/site.json <<'JSON'
{ "version": 2, "static": { "root": "public" } }
JSON
mkdir -p sites/shop.example.com/public
echo '<!doctype html><title>shop</title><h1>shop.example.com</h1>' > sites/shop.example.com/public/index.html
```

Or through the house, which also drops a working hello page:

```sh
middleware site add shop.example.com --template node     # static | node | go | docker | fastapi | django | php | astro
middleware site add shop.example.com --manifest -  <<'JSON'
{ "version": 2, "run": { "command": ["npm", "start"], "env": { "NODE_ENV": "production" } } }
JSON
```

(`POST /api/v1/sites` with `{domain, template?, manifest?}` is the same call.)

`fastapi`, `django` and `php` run in a container built from the `Dockerfile`
they bring (packages are installed into the image, the folder is mounted
over it, so code edits need a restart and a `requirements.txt` change
rebuilds). Django keeps its database in `data/db.sqlite3` and its key in
`.env`; PHP uses its own server with `PHP_CLI_SERVER_WORKERS`, because a
site's container has no capabilities and Apache cannot drop to www-data in
it. `astro` serves `dist/`, which starts with a placeholder: run
`npm install && npm run build` after editing `src/`.

A folder **without** `site.json` is not served and the panel lists it as
"unmanaged". That is on purpose: a folder served silently and a folder
ignored silently look the same from outside.

Then **hand the detailed work to that site's own conversation**: say what you
created, where, and that the project work (code, dependencies, deploy
recipe) continues from the site's chat. Do not build the project from the
house conversation.

### What a site conversation can ask the house

The CLI talks to the supervisor over its socket and is how you look at the
house without opening `data/` or `tasks/` by path. From a site's
conversation it needs no token and is confined to that site: another
site's name, the house's settings, tokens and the list of every site are
refused. Everything below works on your own site:

```sh
middleware site show shop.example.com     # mode, port, alive, revision, last error, workers, services, manifest
middleware site logs shop.example.com --lines 200 [--worker api | --service web]
middleware events --subject shop.example.com [--type site.] [--since 24h] [--limit 500]
                                          # starts, crashes, handovers, OOM kills (site.oom), limits, manifest changes
                                          # --subject <your domain> is required
middleware site tasks shop.example.com    # its tasks: schedule, next run, last run ok/failed and how long (§ 9)
middleware site tasks shop.example.com log <task> [--lines 200]   # why a task failed
middleware site tasks shop.example.com save <task> - <<'JSON' … JSON   # create or change one
middleware site tasks shop.example.com run <task>      # now; then read its log and the list
middleware site tasks shop.example.com stop <task>
middleware site tasks shop.example.com rm <task>
middleware site backups shop.example.com [log|run <name> | restore <name> <copy>]   # § 9
middleware site security shop.example.com [seen|blocks|catalog]   # who calls, what security refused, what it may name (§ 8)
middleware site restart shop.example.com [--service web]
middleware site deploy shop.example.com
middleware site validate shop.example.com [--file draft.json]
middleware site routes shop.example.com
middleware exec shop.example.com [--service db] -- <command>
curl -s -o /dev/null -w '%{http_code}\n' -H "Host: shop.example.com" http://127.0.0.1:46100/
```

Asked "why did the tasks fail?": `site tasks <domain>` for which ones and
when, `site tasks <domain> log <task>` for each, and when a log just stops
with no error, `events --subject <domain>` around that time (a `site.oom`
there means the site's memory limit killed it; § 3, `limits`). Never answer
that the logs are out of reach and ask for them to be pasted.

Do not write a denied path (`data/…`, `tasks/…` under the root) in a reply
or a command, not even to say it is denied: any mention is reported as a
violation. Say "the task's log" and read it with its command.

## 3. The manifest, field by field

`sites/<domain>/site.json`. Every field is optional except `version`.
**Unknown fields are an error**: a typo in `"comand"` would otherwise serve
the folder as static files and look like success. Durations are Go strings
(`8s`, `2m`). Commands are arrays, never shell strings; there is no shell, so
no pipes, no globs, no `&&`. Paths are relative to the site folder.

### What answers a request

Exactly one, checked in this order:

1. `disabled: true` → the "stopped" page. No process.
2. `hold.enabled: true` → the holding page. No process.
3. `run` → the supervised instance.
4. `upstream` → a process the house does not supervise, proxied as is.
5. `static` → files from `static.root` (default `public`, then the folder).

Writing `run` into a static site turns it into a process; removing `run`
turns it back. Nothing else needs to change.

### Variables

In `run.command`, `run.env` values, `env_from_file` file names, `upstream`
and `workers.*.command`: `$PORT`, `$DOMAIN`, `$SITE_DIR`. Unknown `$NAMES` are
left untouched. The process also receives `PORT`, `HOST=127.0.0.1`,
`MIDDLEWARE_DOMAIN`, `MIDDLEWARE_SITE_DIR` and a private `HOME`. The base
environment is only `PATH`, `LANG`, `LC_ALL`, `TZ`: nothing else of the
supervisor's is inherited.

**Environment order**: `env_files` first, then `env` on top, then
`env_from_file` on top of that, and `PORT` always last. No duplicate keys:
glibc reads the first occurrence and Node the last, so a duplicate would mean
two runtimes read two values. A site's `.env` can never override the port.

### `run`

```json
{
  "version": 2,
  "run": {
    "command": ["./server", "--port", "$PORT"],
    "cwd": ".",
    "env": { "NODE_ENV": "production" },
    "env_files": [".env"],
    "env_from_file": { "APP_DIST": ".dist-current" },
    "health": { "path": "/", "timeout": "60s" },
    "drain": "8s",
    "restart": { "max_backoff": "60s" },
    "runtime": "process"
  }
}
```

- `command`: resolved in the site folder, so `./server` needs no absolute
  path. Never write an absolute path of this machine (`/home/x/.nvm/...`):
  it breaks the day the site moves to the docker runtime or another box.
- `cwd`: a subfolder to run from (`frontend`).
- `env`: plain variables. **Secrets never go here**: the manifest is in git.
- `env_files`: files of `KEY=VALUE` lines (`#` comments, optional `export`)
  or `.json` objects of strings, read in order, a later file overriding an
  earlier one. Secrets go here, and the files go in `.gitignore`. A missing
  file is not an error.
- `env_from_file`: the variable is set to the **trimmed contents** of the
  file, re-read every tick. This is the deploy pointer (§ 4). A missing
  pointer file sets the variable empty and says so in the events; it does
  not refuse to start.
- `health`: the engine waits until the port accepts and this path answers
  2xx before switching traffic. Default `/`, 60s. A `go run .` or a cold
  `next start` needs more: write `"timeout": "5m"`.
- `drain`: how long the old instance keeps serving in-flight requests after
  the switch. Raise it for long downloads or WebSockets that say goodbye.
- `restart.max_backoff`: a crashing instance is retried with growing waits
  up to this.
- `runtime`: `process` (default) or `docker`.

### Canonical recipes

Static folder:

```json
{ "version": 2, "static": { "root": "public" }, "cache": { "static_max_age": "1h" } }
```

SPA (unknown paths get `index.html`):

```json
{ "version": 2, "static": { "root": "dist", "spa": true } }
```

Node app (`npm start` must honour `PORT` and bind `127.0.0.1`):

```json
{
  "version": 2,
  "run": {
    "command": ["npm", "start"],
    "env": { "NODE_ENV": "production" },
    "env_files": [".env"],
    "health": { "path": "/", "timeout": "2m" }
  }
}
```

Next.js by pointer (`next.config.js`: `distDir: process.env.NEXT_DIST_DIR || ".next"`):

```json
{
  "version": 2,
  "run": {
    "command": ["./node_modules/.bin/next", "start", "-p", "$PORT", "-H", "127.0.0.1"],
    "env": { "NODE_ENV": "production" },
    "env_from_file": { "NEXT_DIST_DIR": ".next-current" },
    "health": { "path": "/", "timeout": "2m" },
    "drain": "15s"
  },
  "deploy": {
    "github": {
      "repository": "acme/shop",
      "branch": "main",
      "build": [["npm", "ci"], ["npm", "run", "build"]],
      "pointer": ".next-current"
    }
  }
}
```

Go binary, built in place:

```json
{
  "version": 2,
  "run": {
    "command": ["./bin/server"],
    "env_files": [".env"],
    "health": { "path": "/healthz", "timeout": "30s" }
  }
}
```

Docker image. The site folder and its HOME are mounted **at their own
paths**, so `$SITE_DIR`, relative paths and the command mean the same inside
and out. `HOST` is `0.0.0.0` inside (the published port is loopback-only on
the host). The container has no capabilities, cannot gain privileges, and
at most `pids` processes (default 1024):

```json
{
  "version": 2,
  "run": {
    "command": ["node", "server.js"],
    "runtime": "docker",
    "docker": { "image": "node:22-slim", "memory": "512m", "pids": 512, "cpus": 1.5, "volumes": ["data:/var/lib/app"] },
    "health": { "path": "/" }
  }
}
```

Or built from the site: `"docker": { "build": "Dockerfile" }`. Pin a tag;
`latest` makes the revision lie. `memory` is a hard limit: the kernel kills
what exceeds it. A named volume is the site's own (`mw-<domain>-data`).

**A docker site's dependencies are installed inside its image, never on the
host**: `middleware exec <domain> -- npm install` runs the command where the
site runs (its container, its folder, its HOME). Run on the host, native
modules are built for the wrong system and `node_modules` belongs to the
host's Node; the site then fails at start with errors that point nowhere
near the cause.

Upstream on a fixed port someone else owns (nothing is supervised; the house
only proxies):

```json
{ "version": 2, "upstream": "http://127.0.0.1:8090" }
```

A worker behind a frontend (the port **is** written because the frontend
already knows it; the worker restarts, it is not handed over):

```json
{
  "version": 2,
  "run": {
    "command": ["npm", "start"],
    "env": { "API_URL": "http://127.0.0.1:8090" },
    "health": { "path": "/" }
  },
  "workers": {
    "api": { "command": ["./bin/api"], "cwd": "backend", "port": 8090, "env_files": ["backend/.env"] }
  }
}
```

Worker names match `^[a-z0-9][a-z0-9-]{0,62}$`. Leave a worker's `port`
out unless something already has it written: the engine then assigns one and
passes `$PORT`, as for a site. A written port must be free of the engine's
own range (ask `GET /api/v1/system/ports`). **Never pick a port for a site**;
`run.port` exists only for a port someone outside the house already knows,
and it costs the handover. A project that reads its port from another name
maps it: `"env": { "APP_PORT": "$PORT" }`.

### Several services and several names: `services`

When a project is more than one process with names of its own (a frontend,
an API on a path, a CMS on a subdomain, a database), write `services`
instead of `run`, and **never** a router and a supervisor in bash inside
one image: there, one typo kept every part from deploying, and a CMS that
was installing answered a raw 502. Each service is a run section with the
names it answers:

```json
{
  "version": 2,
  "services": {
    "web": {
      "run": { "command": ["node", "server.js"], "health": { "path": "/healthz" } },
      "domains": ["bakery.example"]
    },
    "api": {
      "run": { "command": ["node", "api.js"], "env": { "DB_HOST": "127.0.0.1", "DB_PORT": "$SERVICE_db_PORT" } },
      "domains": ["bakery.example/api"],
      "depends_on": ["db"],
      "waf": { "max_body": "50m" }
    },
    "drupal": {
      "run": {
        "runtime": "docker",
        "docker": { "build": "deploy/drupal/Dockerfile", "memory": "1g", "volumes": ["drupal:/var/www/html/sites/default/files"] },
        "env": { "DB_HOST": "$SERVICE_db_HOST", "DB_PORT": "$SERVICE_db_PORT" },
        "env_files": ["deploy/drupal/.env"],
        "health": { "path": "/user/login", "timeout": "10m" }
      },
      "domains": ["cms.bakery.example"],
      "depends_on": ["db"],
      "access": { "allow": ["203.0.113.0/24"] }
    },
    "db": {
      "run": {
        "runtime": "docker",
        "docker": { "image": "mariadb:11.4", "port": 3306, "volumes": ["db:/var/lib/mysql"] },
        "env_files": ["deploy/db/.env"],
        "health": { "command": ["healthcheck.sh", "--connect"] }
      },
      "internal": true
    }
  }
}
```

What to know, in the order it bites:

- **The site's own name must be one service's** (`web` here). `domains`
  are `"host"` or `"host/prefix"` (longest prefix wins, the path reaches the
  service as it came). They are published like aliases; a name in two
  services, or another site's, is refused. `services` and `run`, `aliases`,
  `static` or `upstream` are exclusive.
- **Each service has its own life**: its revision, build, health, log and
  backoff. Changing `deploy/drupal/` rebuilds `drupal` and nothing else
  (`docker.watch` lists other folders to watch). One that fails keeps its
  previous generation serving; while one starts, its names get the house's
  503 page with `Retry-After`. `depends_on` holds a first start until those
  are up and healthy; `optional: true` keeps the site up without it.
- **Reaching each other**: containers share the site's network and reach
  each other by service name (`db:3306`); `$SERVICE_<name>_HOST` and
  `$SERVICE_<name>_PORT` (dash as underscore) give the right address from
  wherever the service runs. A container listens inside on `docker.port`
  or 8080 (`$PORT` says which). A process may only name a service that
  restarts in place (one without names); a container cannot reach a
  process. An internal container nothing on the machine needs is not on
  the machine's loopback at all; its health is `health.command`.
- **Images** in services carry a version (`mariadb:11.4`, never `latest`).
  A `build` uses the site's folder as its context and its `.dockerignore`;
  its output is `middleware site logs <domain> --service <name> --build`.
  Prefer `COPY --from=<image>` for a binary over downloading one in the
  Dockerfile: an arm64 machine gets the right one, and an image built for
  another architecture is refused before it runs.
- **Volumes** keep their names (`mw-<domain>-db`), so moving a database
  from a worker or a run into a service keeps its data.
- **Rules per service**: `waf`, `attack`, `headers`, `cache` and `hold`
  replace the site's for that service's names; `access` and `security` add
  to the site's (judged after it, never instead). `disabled` and `hold`
  stop one service and leave the others serving.

Working with one from a conversation:

- Check before writing: `middleware site validate <domain> --file draft.json`
  (it writes nothing, and lists every name with where it would go).
- `middleware site show <domain>` and `middleware site routes <domain>`:
  each service's state, port and error; each name → service → port → state.
- `middleware site logs <domain> --service <name>` (and `--build`).
- `middleware site restart <domain> --service <name>`; `site stop|start|hold
  <domain> --service <name>` write that service's section.
- `middleware exec <domain> --service <name> -- <command>`: an install or a
  migration in that service's container, on the site's network. From a
  site's turn it goes through the house (the jail has no docker): output as
  it comes and the command's exit code.

**Moving a site from `run` to `services`**: write the new manifest to a
draft, `site validate` it, then replace `run` and `aliases` with `services`
in one write. The old instance keeps answering until every service with
names is up, so nothing goes dark; the previous manifest is in the history
to go back to.

### Copies on other nodes: `cluster`

```json
"cluster": { "replicas": 1, "spill": 70, "gateway": ["127.0.0.1:3306"] }
```

Asked for "replicas", "spread the load", "cluster mode": this block, on the
node the names point at. The house copies the folder to the least loaded
other nodes (dependencies installed there for that machine) at every
change, and hands them requests through the mesh only when this node's load
per core passes `spill` % (70 by default) or its instance is down; the same
visitor always lands on the same node. `gateway` lists the database
addresses on this node (loopback only) the copies reach through the mesh,
so every node uses the one database. With `services`, the ones with data
stay on this node (`keep: ["db"]`; any with volumes stays anyway) and the
copies run the rest, reaching the kept ones through the mesh by the same
name and `$SERVICE_<name>_…`: nothing in the site changes. It is refused
for SQLite, workers, volumes on the site's own run, an upstream, VPN-only
sites, `needs`, `claude` (the subscription) and `claude_api` from a
container the copies would run. `claude_api` from a process is fine (a
database in docker kept on this node does not change that): the copies
call the door through the primary, with its token and its spending, and
the code changes nothing. A service that drains a queue or runs on a
timer (an outbox, a mailer) would run once per copy: put it in `keep`
too. When it is refused, say why rather than working around it. Files a copy writes into its folder
(uploads) are lost at the next copy; put what the site writes as it runs in
`move.exclude`, or it is copied again and again. Never write `replica`: the
house writes it on the copies. `GET /api/v1/sites/<domain>/cluster` says
where the copies are and how they are. Details: docs/cluster.md.

### A site's budget: `limits`

`"limits": { "memory": "1g", "memory_high": "768m", "cpus": 1.5, "cpu_weight": 100, "pids": 2048 }`
is what every process-runtime piece of the site (instances, process
workers and services, tasks) may hold together, in the site's own cgroup;
a docker piece keeps `docker.*`. `cpu_weight` is 1..10000 with 100 an equal
share (not docker's 1024). `cpus` is a hard cap that holds even on an idle
machine; `cpu_weight` only counts when sites compete, so a site asked to
"use less CPU" gets a lower weight first, and a cap only when it must never
go past a number. No `memory` means no ceiling (the house's
`sites.default_limits` aside). During a deploy both generations run inside the
same `memory`, so leave room for two: a new one that does not fit is killed
and fails its health check while the old one serves. A kill at the ceiling
is a `site.oom` event: read it before raising the limit. Applied live.

### Live versus restart

The **revision** hashes what the instance was born from: resolved command,
resolved environment (`env`, `env_files` contents, `env_from_file` contents),
runtime and docker settings. Changing any of those hands the site over.
`access`, `security`, `waf`, `attack`, `redirects`, `headers`, `cache`, `hold`, `disabled`,
`force_https`, `limits` are applied live and touch no process.

### `disabled` and `hold`

```json
{ "version": 2, "run": { "command": ["npm", "start"] }, "disabled": true }
```

```json
{
  "version": 2,
  "run": { "command": ["npm", "start"] },
  "hold": { "enabled": true, "title": "Back in a moment", "message": "We are moving the database." }
}
```

Both stop the process and show a page. `middleware site stop|start|hold|unhold`
write exactly these fields. They live in the manifest because a decision
kept in memory does not survive a restart: a stopped site would come back
the next time someone deployed, and nobody asked for that.

## 4. Deploy by pointer

There is one way to deploy without a cut and it is `env_from_file`. Build
somewhere new while the current build keeps serving, then write the new
name into the pointer **last**:

```sh
cd sites/shop.example.com
git pull --ff-only
npm ci
NEXT_DIST_DIR=.next-2026-10-20T10-00 npm run build
echo .next-2026-10-20T10-00 > .next-current
```

The engine sees the pointer change, starts a new instance on a fresh port
with output straight to its log, waits for the port and the health path,
switches the router, drains the old one for `drain`, then SIGTERM, SIGKILL.
One instance receives traffic at any moment; two exist for a few seconds.

**If the new instance never becomes healthy nothing switches.** The old one
keeps serving, `handover.failed` says why, the panel shows the error with
the log tail. Fix, write the pointer again.

**Rollback** is writing the previous name:

```sh
echo .next-2026-10-19T16-30 > .next-current
```

Keep a few builds; a task prunes the rest (§ 9). Never `rm -rf` the build
the pointer currently names.

### `deploy.github` and the build contract

With the `github` module enabled (`middleware modules`), a push to `branch`
pulls, runs each `build` command in the site folder with the site's
resolved environment plus `MIDDLEWARE_BUILD` (the build id, like
`20261020T100000Z`), and on success writes that id into `pointer`. So the
whole recipe for Next.js is `distDir: process.env.MIDDLEWARE_BUILD` in
`next.config.js` and the manifest above. The build's output goes to
`data/logs/<domain>/deploy.log`; a failed build writes nothing and emits
`deploy.failed`; the running instance is untouched. A second deploy while one
runs answers 409.

`middleware site deploy <domain>` runs the same by hand, and so does
`POST /api/v1/sites/{domain}/deploy`. Without a `pointer` the build runs in
place and the instance is restarted: both generations share files, so a
build that deletes what the running instance serves will show as a 503 for
the length of the build. Prefer a pointer.

A site that keeps another GitHub repository in its folder (a public copy
in `build/oss`, a vendored library) lists it in the manifest as
`"repositories": [{"repository": "owner/name", "path": "build/oss"}]`.
The GitHub module's token then reaches it as it reaches the deploy one:
`git push` from the conversation and «keep up to date» both work, and no
repository outside that list ever gets the token. Only the owner may add
an entry; a turn's git settings are set when it starts, so push from the
next turn.

### What breaks a WebSocket

A WebSocket is a TCP connection to one process. On a site handover the old
instance keeps its sockets until the drain ends; then they close with the
process and clients reconnect to the new one. On a restart of **the
middleware itself** every WebSocket dies at once: the proxy owns the raw
socket. Sites keep running through that restart (systemd's `KillMode=process`;
they are re-adopted), so the reconnect is fast. Do not restart the supervisor
to fix a site; it does nothing for the site and cuts everyone's sockets.

### What a few seconds of two instances means

A migration that runs on start, an in-process singleton cron, a file lock:
all of these will meet a second copy of themselves during a handover. Guard
them (an advisory lock, an idempotent migration) or move them to a task or a
worker. The house will not serialize them for you.

## 5. Long work from a turn

When this turn ends, **everything it spawned is killed as a process group**,
and the operator may stop a turn at any moment. A build, an `npm ci`, a
migration, a long import: none of it may hang from the turn.

```sh
setsid nohup npm run build >/tmp/shop-build.log 2>&1 </dev/null &
```

- `setsid` is what detaches: it starts a new session and process group.
- `&` alone leaves the job in this turn's group and it dies with the turn.
- `nohup` alone only ignores SIGHUP; the SIGKILL to the group still lands.
- Output **always** to a file; an inherited pipe goes away with the turn and
  then nobody can say what happened.
- **Say the log path** in your reply, and in the next message read it to see
  how it ended. A deploy launched and never checked is one nobody knows
  anything about, which is worse than one that was cut.
- A detached job survives the turn, so what it starts and does not close
  (a headless browser, a watcher) keeps eating CPU until someone looks. Make
  the job finish.
- If it repeats or must stay up, it is not a `setsid`: it is a task or a
  worker.
- Handing a site over is not long work and is not done this way: write the
  manifest or the pointer and let the engine do it. Do not wait in the turn
  for a site to come up; check it in the next message.

## 6. Checking it worked

In order:

```sh
curl -s -o /dev/null -w '%{http_code}\n' -H "Host: shop.example.com" http://127.0.0.1:46100/
middleware site show shop.example.com
middleware site logs shop.example.com --lines 100
middleware events --subject shop.example.com
curl -s -o /dev/null -w '%{http_code}\n' https://shop.example.com/
```

The first tests the house; the last tests the whole path. If the first
answers and the last does not, the site is not published (§ 7). Read
`site show` for `mode`, `alive`, `port`, `revision`, `handing` and `error`.

What a visitor page means (the title is what a visitor reads):

| page | what it means | what to do |
| --- | --- | --- |
| "This site is stopped" | `disabled: true` | `middleware site start`, or remove the field |
| "Back in a moment" | `hold.enabled: true` | unhold when done |
| "Nothing here" | no folder, no `site.json`, or a different `Host` | check the folder name and the manifest |
| "Temporarily unavailable" | starting, or crashed and retrying | read the log; `instance.failed` has the reason |
| "This entrance is closed" | the door the request came by is closed | the Inbound screen; open the door or move the domain |
| "No certificate yet" | ports door, certificate being requested | wait; `cert.failed` says why if it does not come |
| "Access restricted" | `access` denied this client | the allow list, the country list, the password |
| "Closed" (403: "closed to robots", "to this program", "with this browser", "from the country") | `security` refused it | `middleware site security <domain> blocks` says which list and which entry |
| "Not found" for a path the site has | `security.paths`, or a house probe path (`.env`, `.git/`…) | `site security <domain> blocks`; rename the path, never serve those |
| "Slow down" / "Request too large" | `waf.rate`, `waf.max_body` | raise the limit or exempt the client |
| "Checking your browser" | `attack.on` (or the WAF policy's `attack`) | `attack.skip` for webhooks, or switch it off |

Events to know: `instance.starting`, `instance.started`, `instance.failed`
(carries the error and the retry wait; the panel shows it with the log
tail), `instance.exited`, `handover.switched`, `handover.done`,
`handover.failed`, `worker.started`, `worker.failed`, `deploy.started`,
`deploy.done`, `deploy.failed`, `task.started`, `task.done`, `task.failed`,
`task.skipped`, `cert.renewed`, `cert.failed`, `inbound.moved`,
`waf.blocked`. `GET /api/v1/events/stream` streams them;
`middleware events -f` follows.

A site that starts, serves and is wrong is the case to design for: before
saying "done", load a page that proves the new code is the one serving
(a version string, a changed title), not just a 200.

## 7. Publishing

A site that answers by `Host` is not a site anyone can reach. Three things
decide that, and the house handles them:

**Doors.** Two, either or both open (`middleware inbound status`):
- `tunnel`: a reverse tunnel (Cloudflare or any) delivers to the loopback
  listener with `Host` intact. No public IP, no certificates here.
- `ports`: the house listens on 80/443 and gets certificates itself (ACME)
  for the domains it serves and no other.

**DNS.** Which door a domain uses is decided by its DNS, not by the house: a
CNAME to the tunnel, or an A/AAAA to this machine. With the `cloudflare`
module on, the house writes the records; without it, say exactly which
record the person must create and at what value.

**Publishing is the house's, not yours.** A site created through the house
is published on the door for new sites; every name it answers to (its
domain, `aliases`, and `www.` in front of an apex unless `"www": false`)
follows: an alias you add to the manifest is published on the next pass,
one you remove is unpublished. `middleware site published` shows each name
and its state; `manual` means no DNS provider covers it and a person must
point it here. Never write DNS records or tunnel rules yourself.

**Certificates.** Only on the ports door, only for domains this installation
serves. A domain moved to the ports door before its certificate exists
answers "No certificate yet".

The Inbound screen shows every domain with its door, DNS and certificate,
and moves them. The same call from here:

```sh
curl -s --unix-socket data/api.sock -X POST http://x/api/v1/inbound/move \
  -H 'Content-Type: application/json' \
  -d '{"domains": ["shop.example.com"], "to": "ports"}'
```

(From a site conversation you cannot open `data/api.sock`; ask the person to
publish from the Inbound screen, or use a token against the panel domain.)

The order of a move is the only part that must not change:
- **to ports**: certificate first (by a TXT record when the DNS provider
  manages the zone, otherwise through the door the domain currently uses),
  then DNS to this machine, then the tunnel route removed. If the
  certificate does not come, the domain is not moved.
- **to tunnel**: the tunnel route first, then the CNAME. The other way round
  gives a window of provider errors.

Keep **both doors open during a move**: resolvers keep the old answer for a
while, and with the old door closed that is a "This entrance is closed".

Check from outside when done: `curl -sI https://shop.example.com/`. If the
house answers by `Host` and the public name does not, the DNS or the door is
what is missing, not the site.

## 8. Access, security, WAF, hold, cache, headers, redirects

All of these are applied live; none restarts the process. Use the narrowest
one that solves the case.

**A site not yet shown to anyone** → a password. The manifest stores a hash,
not the password: set it from the panel's Access screen (which hashes it),
and never paste a plain password into `site.json`. A program gets in with
`Authorization: Basic` and the same password. One password for the site, no
accounts, no roles.

**Who gets in by what it says it is: robots, programs, browsers, countries**
→ `security`. This is the panel's **Security** tab on the site's page, and
it is the manifest's `security` block: the tab writes nothing else. Never
write a User-Agent check, a list of bots, a GeoIP lookup or a "block
crawlers" middleware in the app, and never rely on `robots.txt` alone (it
asks; this refuses). Serving a `robots.txt` as well is fine.

```json
{
  "version": 2,
  "static": { "root": "public" },
  "security": {
    "bots": {
      "deny": ["ai", "seo"],
      "except": ["Googlebot"],
      "no_ua": true,
      "window": { "from": "01:00", "to": "07:00", "zone": "Europe/Madrid" }
    },
    "agents": { "deny": ["scrapy", "python-requests"] },
    "browsers": { "deny": ["ie"] },
    "countries": { "deny": ["T1"] },
    "paths": ["/wp-login.php", "/xmlrpc.php"],
    "message": ""
  }
}
```

- Four lists, and a request must pass all of them. In each, `deny` comes
  first, and an `allow` with anything in it means **only** those: an
  `agents.allow` or a `browsers.allow` shuts out everything else, robots,
  monitors and webhooks included. Prefer `deny`.
- `bots.deny` closes **families**, not names, so next month's crawler is
  already in: `search` (Google, Bing: closing it takes the site out of
  search results), `ai` (GPTBot, ClaudeBot, CCBot, PerplexityBot,
  Bytespider…), `social` (the link previews of WhatsApp, Telegram, X,
  LinkedIn: closing it leaves pasted links without a card), `seo` (Ahrefs,
  Semrush, MJ12bot…), `tools` (curl, wget, python-requests, headless
  browsers: deploy scripts and webhooks use them too), `monitors`
  (UptimeRobot, Pingdom: whoever watches the site from outside), `other`
  (anything that says bot, crawler or spider and is in no table).
  `except` lets robots in by name. `no_ua` closes requests with no
  User-Agent. `window` lets the closed families in between two times
  (`zone` is an IANA name; empty is the machine's clock), and a crawler
  refused outside it is told the hours.
  `middleware site security <domain> catalog` lists every family with its
  robots, the browsers and the paths every site already cuts.
- `agents` are User-Agent fragments, case-insensitive, two characters at
  least: for what is in no table. A single broad fragment (`"mozilla"`)
  closes the site to every browser.
- `browsers`: `edge`, `opera`, `samsung`, `ie`, `firefox`, `chrome`,
  `safari`, `other` (anything not a known browser, robots included),
  `none` (no User-Agent).
- `countries`: two letters as the edge writes them, `XX` unknown, `T1`
  Tor. The country comes from Cloudflare; a request that arrives without
  one (the ports door, the bench) is `XX`, so an **allow list of countries
  shuts out everything that has no country**. Say so before writing one.
- `paths`: prefixes the site never serves, by whole segments, answered
  404 before the site sees them. `.env*`, `.git/`, `.ssh/`, private keys,
  `/etc/passwd` and `/proc/self/` are already cut on every site; do not
  list them, and do not write code in the app to refuse them. `/` and
  `/.well-known` cannot be listed.
- `message` replaces the line a refused visitor reads (403, the site's
  design). A visitor is never told which rule.
- In a site with `services`, a service's `security` adds to the site's,
  never opens what the site closed.

What it is not: a User-Agent is written by whoever calls and can say
anything, so this clears out what identifies itself, and guards nothing.
To keep a person or a determined script out, `access` (below) is the door.

How to do it: look at who really calls (`middleware site security <domain>`,
heaviest first: countries, robots with their family, browsers, and the
User-Agents no table knows), write the rule, `middleware site validate
<domain>` (an unknown family, browser or country, or a bad time, is refused
there and the manifest is not taken), save `site.json`, which is applied
live, and check it: `curl -s -o /dev/null -w '%{http_code}\n' -A 'GPTBot/1.2'
-H 'Host: <domain>' http://127.0.0.1:46100/` is a 403, and `middleware site
security <domain> blocks` shows the last 50 refusals with which list and
which entry did it. When something wanted is refused (a payment webhook
sent by a library, an uptime monitor, link previews), that list is where
to find it: add an `except`, or take the family out.

Who can change it: the operator, a delegated user with `code`, and the
site's own conversation (it writes `site.json`).

**Only some people, by network** → `access.allow`, CIDRs. Who is not in the
list sees "Access restricted" with their own address, which is what they
need to ask for access.

**Only part of the site** → `access.routes`: zones by path prefix, the
longest matching one wins, matched by path segment (`/admin` covers
`/admin/x`, not `/administration`) on the cleaned path, so `/x/../admin`
and `/ADMIN` are covered too:

```json
{
  "version": 2,
  "run": { "command": ["npm", "start"] },
  "access": {
    "allow": [],
    "deny": ["203.0.113.0/24"],
    "deny_countries": ["XX"],
    "routes": {
      "/admin": { "password": "", "note": "the back office" },
      "/webhooks": { "allow": ["192.0.2.0/24"] }
    }
  }
}
```

A zone **adds** to the site, never loosens it: its `allow` narrows the
site's, its `deny` adds to it, and its own `password` (a hash, set from the
panel like the site's) is asked for after the site's, if the site has one.
Each password gives its own pass, valid only for its zone. The client
address is the real one: edge headers are believed only from the door they
belong to, so a hand-written `X-Forwarded-For` opens nothing.

**The site's code needs an AI** (draft, classify, answer with JSON) → the
site door, which the panel calls the **AI gateway**. The site's own code
asks the house, and the house runs the agent: same engine, same jail, same
bill, visible in the panel. Never install or call an agent's CLI from the
site, and never put an API key in the project.

*Configuring it.* When the person asks for it in the conversation ("make
the site summarise with AI", "set up the gateway"), you open it yourself by
writing the block in `site.json`; nobody has to go to the panel. The house
sees the change, gives the process `MIDDLEWARE_TOKEN` and `MIDDLEWARE_API`
and restarts it (the block is part of the revision). It needs a process
(`run` or `workers`): a site of static files has nobody to call it.

```json
{
  "version": 2,
  "run": { "command": ["node", "server.js"] },
  "claude_api": {
    "models": ["haiku", "sonnet", "opus"],
    "model": "sonnet",
    "daily_usd": 3
  }
}
```

(Only the `claude_api` key is new: add it to the `site.json` that is
there, leaving the rest as it is.)

- `models`: the ones the code may pick from, **several at once**. The code
  chooses per call with `"model"`; one not on the list is a 400, so an
  injected prompt cannot move every call to the dearest model. Empty: any
  of the agent's. `model` is the default when a call names none, and must
  be on the list.
- `provider`: which agent answers (`claude` when absent; `openai` for
  Codex, `agy`, a vendor's…). Only one whose module is enabled answers; if
  the gateway says `available: false`, tell the person to enable it and
  sign in under **Modules**.
- `permissions`: absent is **no tools at all** (the model talks and
  nothing else), which is what generating text needs. `read` lets it read
  the site's folder. `edit`, `web` and `shell` reach as far as an injected
  prompt can, since the site answers the internet: never write them on
  your own; ask (§ 12) and write them only on a yes.
- `daily_usd` (and `daily_turns` for agents on a subscription, whose turns
  report no cost): pick a cap that fits the feature; absent is the house's
  ($5 by default). `max_chats` (20) is how many conversations it keeps, and each is
  forgotten three days after its last turn: code that keeps a conversation's
  id longer must open another on a 404.

Say what you wrote and why each value, and that the site restarts.

*Choosing the model in the code.* Route by task, and keep it in one place
(a small map in the code, not a model name in every call): the cheapest
that does the job well, a stronger one only where it shows.

| task (Claude's names; another agent's are in `claude.models`) | model |
| --- | --- |
| classify, extract fields, tag, short JSON, moderate | `haiku` |
| write and rewrite, summarise, answer a visitor, most work | `sonnet` |
| long reasoning, hard code, plans, what failed on `sonnet` | `opus` |
| the hardest and longest work, only where `opus` falls short (dearest) | `fable` |

A failed or poor answer from a cheap model can be retried once on the next
one up; never loop. `GET $MIDDLEWARE_API/api/site` says what the code has:
`claude.models` (what it may name), `claude.model` (the default),
`left_usd`, `available`. Read it at start, not on every call.

*Calling it.*

```sh
curl -s "$MIDDLEWARE_API/api/site/claude/ask" -H "Authorization: Bearer $MIDDLEWARE_TOKEN" \
  -H 'Content-Type: application/json' -d '{"prompt": "Summarise: …", "model": "haiku", "format": "json"}'
```

The answer is `{chat, state, text, model, turns, cost_usd, json, question}`;
`files: ["path/in/the/site"]` sends files of the site's folder with the
message, `wait: "0"` answers 202 at once and the conversation is followed at
`/api/site/claude/chats/{chat}` or its `/stream`. The stream is
server-sent events: `event: delta` (`{turn_id, text}`) is the answer as it
is written, a few words at a time; `event: message` is each whole message
when it ends, which replaces the deltas received since the previous one;
`event: session` comes after the turn. For text that appears as it is
written, open the stream before sending with `wait: "0"`, and relay it to
the browser from the site's backend (the token never leaves the server).
A conversation (`POST
/api/site/claude/chats`, then `/messages`) keeps its context between calls;
each message may name its own `model`. A 429 says the daily cap was
reached: the code shows something sensible, it does not retry. In a docker
site, call it over `$MIDDLEWARE_API_SOCKET` (curl `--unix-socket`).

*Checking it.* After the restart (`middleware site show <domain>`: a new
revision, `alive`), exercise the feature through the site itself, the way
a visitor would, and read `middleware site logs` for the gateway's answer.
A 401 is a process started before the block was written; a 503 is an
agent without a session.

**Too much automated traffic** → `waf`:

```json
{
  "version": 2,
  "static": { "root": "public" },
  "waf": { "rate": { "requests": 600, "window": "1m", "burst": 100 }, "max_body": "20m" },
  "attack": { "on": false, "bits": 16, "life": "30m", "until": "", "skip": ["/hooks/stripe"] }
}
```

`rate` is a token bucket per client address; an address that trips it is
blocked for a while, and pushed to the kernel firewall when the helper runs.
`attack.on` makes every browser solve a proof of work before serving (a
fraction of a second at 16 bits; each bit doubles it). While it is on,
**anything that is not a browser does not get in**: webhooks, your own API
clients, a payment gateway's callback. Put the narrowest path that works in
`skip` (each line is an open door), give it an `until` (switch it on with
`POST /api/v1/waf/under-attack {"enabled":true,"domain":…,"for":"2h"}`), and
say so before turning it on; it protects nothing, it only makes the first
request cost. A POST interrupted by the challenge is lost.

**An old address, or a whole name, sent elsewhere** → `redirects`, applied
before the site is served (also while it is stopped or held):

```json
{
  "version": 2,
  "static": { "root": "public" },
  "redirects": [
    { "from": "/old", "to": "/new" },
    { "from": "/blog/*", "to": "https://news.example.com/articles/*", "code": 308 }
  ]
}
```

`code` is 301 by default (302, 303, 307, 308). `/*` at the end of both
carries the rest of the path; the query string follows unless `to` has its
own. The first rule that matches wins. Do not write these as routes in the
app.

**Maintenance** → `hold` (§ 3), not `disabled`: hold keeps the domain, the
DNS and the door, and shows a page you wrote.

**Static assets cached by the house** → `cache.static_max_age`. The house
never caches a response with `Set-Cookie`, `private`, `no-store`, anything
but a 200 to GET/HEAD, or a request with `Authorization`; what you choose is
only how long.

**Headers on every response** → `headers`:

```json
{
  "version": 2,
  "static": { "root": "public" },
  "headers": { "X-Frame-Options": "DENY", "Referrer-Policy": "strict-origin-when-cross-origin" }
}
```

Do not write `Content-Security-Policy` here for an app that sets its own: two
copies of the same header end up saying two things.

**A name that must never answer over http** → `force_https`, name by name:

```json
{ "version": 2, "aliases": ["shop.example"], "force_https": ["example.com", "shop.example"] }
```

Plain http for a listed name is sent to https before anything else answers,
the password door included. It matters through the Cloudflare tunnel, which
serves `http://` unless told otherwise; the ports door already sends every
name to https. Do not write it in your app as well: a redirect your app makes
from what it believes the scheme is can loop behind the tunnel.

## 9. Tasks and workers

**A task** is a file. No registry, no database; the scheduler re-reads
`tasks/` and what is there is what runs. Names match
`^[a-z0-9][a-z0-9-]{0,62}$`.

```json
{
  "version": 2,
  "schedule": "30 4 * * *",
  "site": "shop.example.com",
  "command": ["./scripts/backup.sh"],
  "timeout": "1h",
  "overlap": "skip",
  "env": { "KEEP_DAYS": "30" },
  "disabled": false
}
```

- `schedule`: five-field cron (`*`, lists, ranges, steps). **The clock is the
  machine's**, usually UTC: "at 8:00" may be 10:00 where the person lives.
  Say which you mean.
- `site`: the command runs **inside that site's folder with that site's
  resolved environment and HOME**: same `.env`, same database. Use it
  whenever the project is here. Without it the task runs from the root.
- `timeout`: when exceeded the whole process group is killed. Without it
  the task may run forever; write one.
- `overlap`: `skip` (default; if the previous run is still going, this one
  is skipped and `task.skipped` says so) or `queue`.
- `env`: the task's own variables. No secrets: `tasks/` is the house's git
  history, and the day someone asks why the sync runs twice a day the answer
  is there.
- Unknown fields are an error, like in a manifest.

A task that prunes old builds, the companion of § 4:

```json
{
  "version": 2,
  "schedule": "15 3 * * *",
  "site": "shop.example.com",
  "command": ["./scripts/prune-builds.sh", "3"],
  "timeout": "10m"
}
```

where the script lists `.next-*` sorted by name, keeps the newest N **and
the one `.next-current` names**, and removes the rest.

**From a site conversation, manage the site's tasks with `middleware site
tasks <your domain> …`**, never by writing `tasks/` (outside your folder:
the jail denies it). Write the script in the project, then save the task
through the house, which reads it from stdin and makes it your site's:

```sh
middleware site tasks shop.example.com save prune-builds - <<'JSON'
{ "version": 2, "schedule": "15 3 * * *", "command": ["./scripts/prune-builds.sh", "3"], "timeout": "10m" }
JSON
middleware site tasks shop.example.com              # list: schedule, next, last run
middleware site tasks shop.example.com run prune-builds
middleware site tasks shop.example.com log prune-builds
middleware site tasks shop.example.com rm prune-builds
```

`site` may be left out (it becomes yours) and cannot name another site.
Names are one space for the whole house: a name another site or the root
already holds is refused (409); choose another, do not ask for it to be
removed. You only see and touch your own site's tasks; the house's are the
person's. The site's **Tasks** tab in the panel shows the same list.

From the house conversation write the file under `tasks/`; `middleware
tasks` lists every task of the house.

**Never leave a new task to its first scheduled hour unseen.**
`middleware tasks run <name>` runs it now, re-reading the file first, waits
and prints the log path; from a site's conversation `middleware site tasks
<domain> run <name>` starts it and returns at once, so read `… log <name>`
and `…` (the list, with the last run's status) until it has ended. The
panel shows the last runs with exit code and output. The log is
`data/logs/tasks/<name>.log`, read through `log`, not by path.

**A worker** (§ 3) is supervised like a site: same backoff, same re-adoption
when the supervisor restarts, its own log (`site logs --worker <name>`). It
restarts on any change; it is never handed over. If you need a worker to
deploy without a cut, it is a site.

### Database backups: `backups`

When the person asks for copies of the site's database ("haz backups de la
base de datos cada noche", "keep a week of copies", "restore yesterday's"),
write `backups` in `site.json`. **Never** a task with a dump script: the
house already runs the dump where the database is (inside its container,
or in the site's jail for a process), compresses it (gzip), keeps the
newest N outside the folder (`data/backups/<domain>/<name>/`, never pushed,
served or moved), lists them in the site's **Backups** tab and puts one
back, taking a copy of what is there first.

Find the database first, from what the site already says: the `services`
or `workers` entry with a database image (`mariadb:11.4`, `mysql:8`,
`postgres:17`, `mongo:7`), a SQLite file in the folder (`*.db`, `*.sqlite`,
`DATABASE_URL=file:...`), or a `.env` that points at a server
(`DB_HOST`, `DATABASE_URL`). Then one entry per database:

```json
{
  "version": 2,
  "workers": {
    "db": {
      "command": ["mariadbd"],
      "runtime": "docker",
      "docker": { "image": "mariadb:11.4", "volumes": ["db:/var/lib/mysql"] },
      "env_files": [".env.db"]
    }
  },
  "backups": {
    "db": { "engine": "mysql", "service": "db", "schedule": "30 3 * * *", "timezone": "Europe/Madrid", "keep": 14 },
    "app": { "engine": "sqlite", "file": "data/app.db", "schedule": "0 */6 * * *", "keep": 28 }
  }
}
```

| field | write it |
| --- | --- |
| `engine` | `mysql` (MariaDB too), `postgres`, `sqlite`, `mongodb`, or `command` for anything else |
| `service` | the service or worker that runs the database (`db` above). Empty is the site's own `run`. **A site with `services` must name one**; sqlite never does |
| `database` | leave it out when the container was made with `MARIADB_DATABASE`/`MYSQL_DATABASE`/`POSTGRES_DB` (that one is copied); `"*"` copies every database; otherwise its name |
| `file` | sqlite: the file, a path inside the folder |
| `user`, `password_env` | leave both out for an official image (its root user and `MARIADB_ROOT_PASSWORD`/`POSTGRES_USER` are used). Otherwise `user` and the **name** of the variable with the password (`"password_env": "DB_PASSWORD"`), which must be in the container's or the site's environment. **Never write a password in the manifest** |
| `host`, `port` | only for a server the dump does not find: a process reaches `127.0.0.1` by default, a container its own socket |
| `schedule`, `timezone` | cron as a task's; write `timezone` with the person's zone, the machine's clock is usually UTC. No schedule: by hand only |
| `keep` | copies kept, newest first (7 when left out) |
| `dump`, `restore` | `command` only: argv, the dump writes the copy to stdout, the restore reads it from stdin. E.g. Redis: `"dump": ["sh", "-c", "redis-cli --rdb /tmp/d.rdb >&2 && cat /tmp/d.rdb"]` |

Then, every time, **take the first copy now and read how it went**; a
backup nobody saw work is a backup that does not exist:

```sh
middleware site validate shop.example.com            # the manifest with the new block
middleware site backups shop.example.com run db      # waits, prints the copy or the client's error
middleware site backups shop.example.com             # each backup: schedule, next, last result, copies
middleware site backups shop.example.com log db      # what the dump said
```

The usual failures and what they mean: `none of mariadb-dump mysqldump is
installed where the database is` (the dump ran on the machine or in the
wrong container: set `service`, or install the client), `Access denied`
(set `user` and `password_env`, or the variable it names is not in that
environment), `is not running here` (start the service first: a copy is
taken from the running database), `no such file` (the sqlite `file` path).

**Restoring replaces the database.** Do it only when the person asks for
it, and say which copy (its date) you are putting back before you do:

```sh
middleware site backups shop.example.com restore db 20261103-013000-scheduled.sql.gz
```

It first takes a `pre-restore` copy of the database as it is now, and
restores nothing if that copy fails; `--no-safety-copy` skips it, only
for a database too broken to be copied, and only when the person agrees.
A SQLite restore restarts the site. `download <name> <copy> [file]` saves
a copy (gzip) where you are: never commit it, it is the people's data.
`rm <name> <copy>` deletes one.

Removing an entry from `backups` stops it and keeps its copies.

## 10. Before saying "done": the checklist

Every line here was paid for at least once. Run through it.

- **Test by doing, not by reading.** You wrote a manifest: `curl -H Host`
  it. You wrote a deploy script: run it once with the site serving and
  watch `handover.done`. You wrote a task: run it (`site tasks <domain> run`). Paths that only
  exist in a file have taken whole machines down the first time someone
  used them.
- **The worst failures give no error anywhere.** A site that serves and
  leaks; a folder without a manifest the engine ignores; a manifest that
  says one thing and a process born with another; a pointer written to a
  folder the build never finished; a cron that fires at the wrong hour
  because of the clock. For anything you add, ask how it would look if it
  failed. If the answer is "the same as if it worked", make the failure
  visible first (an event, a log line, a health path that checks the real
  thing).
- **A fix in the wrong place is not a fix.** A rate limit in the client
  code and not where the cost is born; a `HOME` fixed after the process
  started; a retry around a build instead of in the pointer. Put the change
  where the thing actually happens, and prove it from outside.
- **A decision that does not survive a restart is not a decision.** Stopped,
  held, jailed, password, deploy pointer: all in `site.json` or in a file
  the manifest names. Nothing you keep only in a running process, a shell
  variable or this conversation counts.
- **A change of environment needs a handover to exist.** The manifest says
  so, the panel says so, and the process still has the environment it was
  born with until the revision changes. `env`, `env_files` contents and
  `env_from_file` contents are in the revision; a file the app reads itself
  at start is not. After a middleware upgrade, re-adopted instances carry
  the old environment too: `middleware site restart --all` rolls them.
- **Never hand a pipe to a supervised process.** Output goes to a file. A
  grandchild that inherits a pipe hangs the parent's `Wait`; a pipe with no
  reader kills the child with SIGPIPE the moment the reader leaves. The same
  is true of your detached jobs (§ 5).
- **No duplicate environment keys; `PORT` last.** If you build an
  environment yourself (a Dockerfile `ENV`, a wrapper script), keep it.
- **Secrets are not in the manifest, not in `tasks/`, not in `env`.** They
  are in `env_files`, ignored by git. The manifest is in git; a hashed
  password is fine there, a plain one is not.
- **Absolute paths of this machine do not go in `command`.** Relative to
  the site folder, or `$SITE_DIR`.
- **Frameworks age.** Check the **installed** version
  (`node_modules/<pkg>/package.json`, not the `^` in `package.json`) against
  the line's patched minimum before publishing. A site with a known
  critical hole is not done, whatever the page shows.
- **Two copies of the same thing end up saying two things.** One build
  script, one pointer, one place that knows the port (the manifest), one
  catalogue of visitor strings.
- **A model decides what to do; the house presses the button.** Do not
  write code that restarts sites, kills processes or blocks addresses on a
  model's say-so. Emit the recommendation; the person or a written rule
  acts.
- **Nothing is a 502 from here.** The house answers its own pages with
  503 and `Retry-After` so an edge that swallows 502 still shows the words.
  If your app returns its own errors, do not use 502/504 for anything you
  want read from outside.

## 11. Conventions for a site's project

- **English in the code**: identifiers, comments, README, commit messages,
  the `AGENTS.md`. A comment says **why** (what was tried, what breaks if
  it is changed back), not what.
- **Visitor strings in a locale catalogue**, never inside a component or a
  template: `locales/es.json` (or whatever i18n the framework already has),
  keys in English by meaning (`checkout.empty_cart`), the default language
  filled, other languages as files added later with no code change. A site
  already written the other way is not rewritten on your initiative; new
  code goes this way, the migration is a person's decision.
- **A `README.md`** that says what the site is, how it runs here (`site.json`
  is the truth: name the command, the pointer, the health path), how to
  deploy and how to roll back.
- **An `AGENTS.md` in the project**, and a `CLAUDE.md` that is only the line
  `@AGENTS.md`: Codex, Antigravity and OpenCode read the first, Claude Code
  the second, and one text serves them all. In it, what is not obvious: why this
  runtime, why this port for the worker, why this drain, what the deploy
  pointer is, which task prunes builds, what must not be changed and what
  happened when it was.
- **Commits**: small, conventional, lower-case imperative subject
  (`feat: add checkout page`, `fix(deploy): write pointer after build`).
  Commit `site.json` with the project; it is the project's runtime
  contract. Do not commit `.env`, `node_modules`, build folders, or the
  pointer target folders; do commit the pointer file if the team agrees it
  should travel (it usually should not: it names a folder on this machine).
- **Dependencies**: `npm ci` with a lockfile; run it in the site folder (or
  in `deploy.github.build`), never globally. Choose versions by checking,
  not by memory.
- **Bind `127.0.0.1`** on `$PORT` (`0.0.0.0` only inside the docker
  runtime). A site listening on all interfaces is a site reachable around
  the house.

### Spec-driven work: OpenSpec

When the **OpenSpec** module is on, `openspec` is on your PATH and every new
site is born with an `openspec/` folder and OpenSpec's own skills
(`openspec-propose`, `openspec-apply-change`, `openspec-archive-change`…, and
`/opsx:propose`, `/opsx:apply`, `/opsx:archive` in Claude Code). The project
then says how it behaves in `openspec/specs/` (the truth today) and what is
being changed in `openspec/changes/<id>/` (`proposal.md`, `design.md`,
`tasks.md`, and the spec deltas). The panel shows both on the site's
**Specs** tab, with the tasks ticked off as you go.

- **A change that adds or alters behaviour** (a feature, a flow, an API, a
  data model, anything a person would want to read before it is built)
  goes through it: propose with OpenSpec's propose workflow, run `openspec
  validate <id>`, then **stop** and ask (§ 12) whether to build it.
  Planning is not permission to implement.
- On a yes (in the chat, or the **Approve** button of the Specs tab, which
  sends "apply the change `<id>`" into this conversation), implement with
  its apply workflow and tick `tasks.md` as each task is done and checked,
  not at the end. When every task is ticked and the site works (§ 6),
  archive it (`openspec archive <id> --yes`) so its deltas become the specs.
- **Not worth a proposal**: a typo, a margin, a dependency bump, a fix
  that restores what the specs already say. Do it directly; if it changes
  behaviour the specs describe, update the spec in the same commit.
- Specs, proposals and tasks are **in English**, as the rest of the
  project. Commit `openspec/` and OpenSpec's skill folders (`.claude/`,
  `.agents/`) with the project.
- A project without `openspec/` is set up only when the person asks (the
  Specs tab has a button, or run `openspec init --tools claude --no-animation .`
  in the folder). Never in a site whose `site.json` serves its whole
  folder (no `static.root`): `openspec/` would be public.
- Without the module (no `openspec` on PATH), do not install it: say that
  it is enabled under **Modules**.

## 12. How to ask the person

This is a narrow chat beside a panel. Answer short, in the person's
language, and prefer doing over asking. When a real decision is theirs, ask
with a fenced block the panel renders as buttons:

```question
{"question": "Which database?", "options": ["SQLite", "Postgres"], "recommended": "SQLite"}
```

- Two to four options; `"recommended"` marks the one you would pick.
- `"options": []` for free text (a domain name, a repository URL).
- One question per turn, and only when the answer changes what you will
  build: not "shall I proceed?", not something the manifest or the project
  already says.
- Do not ask what you can check: which door is open, whether a module is
  enabled, what the current version is, whether the site answers.

When you finish, say in two or three lines what changed (files, manifest
fields), what the person should see (the URL, the page), and anything left
to them (publish from the Inbound screen, add a task, set a password).
