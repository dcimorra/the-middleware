# The manifest: `site.json`

One file per site, at `sites/<domain>/site.json`. A folder whose name has no
dot (`sites/tools/`) is a **project**: its workers, its tasks and its
conversation, and nothing that answers a hostname, so `static`, `run`,
`upstream` and `aliases` are refused there. Every field is optional except
`version`. Durations are Go durations (`8s`, `2m`). Commands are arrays,
never shell strings. Paths are relative to the site folder.

```json
{
  "version": 2,
  "title": "Shop",
  "aliases": ["shop.example.org"],

  "static": { "root": "public", "spa": false },

  "run": {
    "command": ["./server", "--port", "$PORT"],
    "cwd": ".",
    "env": { "NODE_ENV": "production" },
    "env_files": [".env", ".env.local"],
    "env_from_file": { "NEXT_DIST_DIR": ".next-current" },
    "port": 0,
    "health": { "path": "/", "timeout": "60s", "interval": "30s" },
    "drain": "8s",
    "restart": { "max_backoff": "60s" },
    "runtime": "process",
    "docker": {
      "image": "ghcr.io/acme/app:latest",
      "build": "Dockerfile",
      "volumes": ["data:/app/data"],
      "memory": "512m",
      "pids": 1024,
      "cpus": 1.5,
      "cpu_weight": 1024,
      "user": ""
    }
  },

  "upstream": "http://127.0.0.1:8090",

  "workers": {
    "api": { "command": ["./bin/api"], "cwd": "backend", "port": 8090, "env": {},
             "env_files": [".env"], "env_from_file": {}, "disabled": false, "note": "" }
  },

  "disabled": false,
  "hold": { "enabled": false, "title": "", "message": "", "mode": "maintenance", "until": "" },
  "notice": { "theme": "card" },
  "claude_api": { "provider": "claude", "permissions": ["read"], "model": "", "daily_usd": 0, "daily_turns": 0, "max_chats": 20, "hide_chats": false },
  "claude": false,
  "chat": { "reads": ["other.example.com"] },
  "needs": ["api.example.com", "worker@shop.example.com"],

  "access": {
    "password": "",
    "message": "Ask Ana for the password",
    "allow": ["10.0.0.0/8"],
    "deny": ["10.9.9.9"],
    "deny_countries": ["XX"],
    "routes": {
      "/admin": { "password": "", "message": "", "allow": ["10.1.0.0/16"], "deny": [], "note": "back office" }
    },
    "key": ""
  },

  "security": {
    "countries": { "deny": ["RU"] },
    "bots": { "deny": ["ai", "seo"], "except": ["Googlebot"], "no_ua": false,
              "window": { "from": "00:00", "to": "07:00", "zone": "Europe/Madrid" } },
    "browsers": { "allow": [] },
    "agents": { "deny": ["scrapy"] },
    "paths": ["/wp-login.php", "/xmlrpc.php"],
    "message": ""
  },

  "waf": {
    "off": false,
    "rate": { "requests": 600, "window": "1m", "burst": 300 },
    "max_body": "20m"
  },

  "attack": { "on": false, "bits": 16, "life": "30m", "until": "", "skip": ["/hooks/stripe"], "message": "" },

  "cache": {
    "static_max_age": "1h",
    "rules": [
      { "path": "/product", "mode": "origin" },
      { "path": "/img", "mode": "force", "ttl": 3600, "ignore_cookies": true, "note": "logos, never change" },
      { "path": "/product/admin", "mode": "never" }
    ],
    "max_mb": 64
  },
  "headers": { "X-Frame-Options": "DENY" },

  "deploy": {
    "github": {
      "repository": "acme/app",
      "branch": "main",
      "build": [["npm", "ci"], ["npm", "run", "build"]],
      "pointer": ".next-current"
    }
  },
  "repositories": [{ "repository": "acme/app-oss", "path": "build/oss" }],

  "agent": { "provider": "claude", "model": "", "monthly_cap_usd": 0 }
}
```

## What answers a request

Exactly one of these decides, checked in this order:

1. `disabled: true` → the "stopped" page. No process runs.
2. `hold.enabled: true` → the holding page. No process runs.
3. `run` → the supervised instance. Started, health-checked, handed over.
   With `services`, each of the site's names is its service's (below).
4. `upstream` → a process the middleware does not supervise (a fixed port
   someone else owns). Proxied as is.
5. `static` → files from `static.root` (default `public`, then the folder
   itself). `spa: true` serves `index.html` for unknown paths.

A folder with no `site.json` is **not served** and the panel lists it as
"unmanaged", because a folder that serves silently and a folder that is
ignored silently look the same from outside.

## Variables

In `run.command`, `run.env` values, `upstream` and `workers.*.command`:

| variable | value |
| --- | --- |
| `$PORT` | the port the instance must listen on (assigned by the engine) |
| `$DOMAIN` | the site's domain |
| `$SITE_DIR` | absolute path of the site folder |

The process also receives `PORT`, `HOST=127.0.0.1`, `MIDDLEWARE_DOMAIN`,
`MIDDLEWARE_SITE_DIR` and `HOME` (a per-site home under `data/homes/`).

## Visitor pages, hold and needs

When the site cannot answer, the visitor gets a page in the site's design
(`notice.theme`: sober, card, console, note, poster, blueprint, glitch; empty
is the house's, set in settings as `visitor_theme`). The page names the site
(`title`, else the domain), says the state, and tells **starting** (reloads
itself in five seconds) from **not answering** (a crash or a failing health
check). An API client that asked for JSON gets JSON. Upstream errors are 503,
never 502: Cloudflare replaces an origin's 502 with its own page.

`hold.mode` is what a held site answers: `maintenance` (503, the default),
`404`, `410` (gone; search engines drop it) or `500`. `hold.until` (RFC 3339)
is shown and sent as `Retry-After`.

`needs` names what the site needs up: another site, or `worker@site`. A first
start waits up to a minute for them, then starts anyway and says so; the
panel names whatever is missing now.

## The site door (the AI gateway): `claude_api` and `claude`

`claude_api` lets the site's own code ask an AI through the house (see
api.md, "The site door"). The name is from when Claude was its only agent:

- `provider`: the agent that answers, any whose module is enabled
  (`claude`, `openai` for Codex, `agy` for Antigravity, the vendors'…);
  empty is `claude`.
- `model`: one of that agent's; empty is the first of `models`, or the
  Claude module's model for Claude and the agent's own default for the
  others. The default when a call names none.
- `models`: the models the site's code may choose among, call by call
  (`["haiku", "sonnet", "opus"]`: a cheap one to classify, a strong one to
  write); a call naming one off the list is refused (400) before anything
  is spent, and an alias counts as the model it runs. Empty is any of the
  agent's. `model`, when set, must be on it.
- `permissions`: granted one by one, `read` (the site's folder), `edit`
  (change files in it), `web` (search and read the web) and `shell` (run
  commands in it). All four is the panel conversation's own jail. An agent
  that cannot hold one apart is not given it apart: Codex's shell comes
  with `read` or `edit` (read-only when it only reads); Antigravity's web
  search cannot be taken away. Without `edit` or `shell` the site's own
  `.env` is not readable either.
- `tools`: the four levels a door had before permissions came apart
  (`none`, `read`, `web` = read and web, `write` = everything), read when
  `permissions` is absent. A block with neither is a door with no tools,
  as it always was; the panel writes `permissions` out, all four by
  default.
- `daily_usd` caps the dollars a day (0: `agents.site_daily_usd`);
  `daily_turns` caps the turns, for agents on a subscription whose turns
  report no cost (0: none for Claude, 200 for the others).

Every permission is a decision that matters: the site's process answers
the internet, and what an injected prompt can reach depends on them.

`claude: true` puts the house's Claude subscription token in the site's
environment (`CLAUDE_CODE_OAUTH_TOKEN`) for a site that runs the CLI itself.
It reaches as far as the subscription does; prefer `claude_api`.

## Loans: `chat.reads`

Other sites of this house whose folder this site's conversation may read,
never write. The folder is lent whole, its `.env` included: closing the
secrets and leaving the rest would be a wall with a hole in it, which is why
each one is written by hand.

## Docker

The folder and the site's HOME are mounted at their own paths, so nothing in
the command changes between runtimes. The folder is writable and `site.json`
inside it is not: it is mounted read-only over it, so the container cannot
rewrite the manifest the house reads on the machine (a `runtime: "process"`
of its own would run outside the walls). A `site.json` that is a link is
refused in a container. Every container has no capabilities,
`no-new-privileges`, no restart policy (the engine restarts), at most `pids`
processes (default 1024), the port on loopback only, and `HOST=0.0.0.0`
inside. `user` empty is right for the daemon: the supervisor's uid on a
rootful daemon, none on a rootless one (where root inside is the supervisor
outside). Containers carry the label `middleware.house`; leftovers no site
claims are removed when the engine starts. `middleware exec <domain> -- …`
runs a one-off command inside the site's image with the same mounts;
`--service <name>` in a service's, on the site's network.

Taking a site out of its container is `run.runtime: "process"` (the site's
page has a button for it, and one to go back): the same command in the same
folder, on the machine, without the container's walls. The `docker` section
is kept, so going back is the same switch the other way.

Putting a site in a container from its page (`POST /sites/{domain}/containerize`)
works the `docker` section out when the manifest has none: the site's own
`Dockerfile`; otherwise an image for what the command runs — `node:<major>-slim`
for node, npm and `node_modules/.bin`, at the major the site runs on now;
`python:<x.y>-slim` for python, at the machine's; for a compiled binary the
machine's own distribution (`ubuntu:24.04`), or a distroless image when it is
static. It is then started beside the generation that serves, on a spare port
(its log in `logs/<domain>/trial.log`), and written only once it answered, so
the switch is an ordinary handover. When nothing says which image, or the try
fails, the agent the operator picked is asked in a new conversation of the
site, with what was tried and the last lines of the log; it writes a
`Dockerfile` and `run.docker` (agents cannot run docker), and the button tries
that next. Going back into a kept container takes the same path.

## Limits: `limits`

A site's budget, for every process-runtime piece of it together:

```json
"limits": { "memory": "1g", "memory_high": "768m", "cpus": 1.5, "cpu_weight": 100, "pids": 2048 }
```

Each site has a cgroup of its own, `sites/<domain>/` beside the house's
(see [architecture.md](architecture.md#resources)): its instances, its
process workers and services, and its tasks run inside it, and the block
is what that group may hold. `memory` is the ceiling (past it the kernel
kills inside the site, a `site.oom` event says so, with the limit and what
the site held, and the site does not swap); `memory_high` is where the kernel
starts squeezing without killing, at most `memory`; `cpus` caps the site in
cores; `pids` caps its processes (4096 when unset). Sizes are written as
`docker.memory` is.

`cpu_weight` is the cgroup scale, **1 to 10000 with 100 an equal share**:
every site has 100 with no manifest at all, so CPU is shared per site, not
per process, and a site with thirty processes no longer has thirty shares.
`docker.cpu_weight` is docker's scale, where 1024 is the default share: the
two are different numbers on purpose, each the scale of what reads it.

The ceiling is the site's, whatever holds it. **During a handover** the old
and new generation run at once in the same group, so `memory` has to cover
both. The new generation starts in a leaf of the group (`trial-N`, beside
the `main` leaf the site otherwise runs in) whose own ceiling is what the
site has free at that moment less 8 MiB: a generation that does not fit is
killed inside that leaf, fails its health check, and the old one keeps
serving untouched. The leaf's ceiling is lifted at the switch, and empty
leaves are swept.

The block applies to process-runtime pieces: a docker piece lives in the
daemon's cgroup and keeps `docker.*`. The site's page has a «Recursos» tab
that sets `memory`, `cpus` and `cpu_weight` with a bar each, the machine as
its scale and what the site uses now marked on it, and keeps the rest of the
block; only the owner changes it. Changes take effect live, on the next
reconcile, without a restart. Whatever the block leaves out comes from the
house's `sites.default_limits` in the settings (empty by default: no memory
ceiling unless the operator sets one). Commands run for the site
(`npm install`, migrations) are not inside the budget. Without a delegated
cgroup (a unit from an old install, a container) the block is read and
validated and has no effect.

## Node

A command that wants Node (`node`, `npm`, `npx`, `pnpm`, `yarn`,
`corepack`, a shell line that runs one, or any command of a site with a
`package.json`) gets one, in the process runtime and in every one-off
command and build step. The version the site asks for is read from
`.nvmrc`, `.node-version` or `package.json`'s `engines.node`; the machine's
`node` is used when it is that one, or when nothing is asked. Otherwise nvm
is installed under `modules/nvm` (the latest release, the first time) and
the version installed with it, and its folder put first in the site's
`PATH`. The version is never what stops a site: when the one asked for
cannot be had, the current LTS is used, then the newest Node nvm already
has, then the house's own, then the machine's, and a `node.chosen` event
says which and why.

`node` in the manifest (`"24"`, or `"lts"`) says it over the files. A site
that asks for nothing runs on the machine's own `node`, which is a different
one on each machine; moving a site to another node writes here the major it
runs on, so the target installs that one with nvm (and its dependencies,
which a move does not carry: `node_modules` is built for one machine's
Node). A new `node` is a new generation. The installer puts nvm under
`modules/nvm` with the current LTS on every machine, and `middleware doctor`
says whether it is there.

## Paths stay inside the site

`static.root`, `cwd`, `env_files`, `env_from_file`, `docker.build` and the host
side of `docker.volumes` must resolve inside the site folder, links included;
a manifest that names anything outside is refused when it is read. Docker
volumes are either a path in the site (`"./uploads:/u"`) or a name
(`"data:/app/data"`); a name is per site (`mw-<domain>-data`, dots and
slashes as dashes), so two sites writing `data` do not share one. Two
domains can still map to one name (`my-shop.com` and `my.shop.com`), so the
house makes the volume itself before the first run, labelled
`middleware.site=<domain>`, and refuses to start a container of any other
site on it. A volume from before the label goes to the first site that
starts on it, written down in `data/docker-volumes.json` (docker cannot add
a label to a volume that exists), and is refused to the second. A container
whose name another site's running container already has (a worker's port is
written in its manifest) is neither removed to make room nor adopted.

## `env_from_file`: deploying by pointer

Each entry sets a variable to the **contents of a file**, trimmed. The file is
re-read on every tick, and a change in its contents changes the instance's
revision, which triggers a handover. A deployment is then:

```
build into .next-2026-10-19T10-00/   (while the current build keeps serving)
echo .next-2026-10-19T10-00 > .next-current
```

Rolling back is writing the previous name.

## Revision

The revision is a hash of everything the instance was born from: the resolved
command, the resolved environment (including `env_from_file` contents and
`env_files`), the runtime and docker settings. Anything else in the manifest
(`access`, `waf`, `headers`, `hold`) is applied live and does not restart the
process.

## Repositories: `repositories`

The GitHub module's token reaches the repository a site deploys from
(`deploy.github.repository`) and the ones listed here, and no other: a
fine-grained token usually opens every repository of its owner, so the
manifest says which of them are this site's. Each entry is `repository`
(owner/name) and `path`, the clone's folder inside the site (`.` or empty
for the site's own). For each of them, a site's conversation can `git push`
through the house ([modules.md](modules.md), github) and «keep up to date»
fetches with the token, from that path only. The key is the owner's: a
delegated user's change to it is refused. A turn's git settings are made
when it starts, so a repository added during one is reachable from the
next.

## Fields worth a line

- `env_files` are read in order, a later file overriding an earlier one, and
  `env` over all of them. `KEY=VALUE` lines, or a `.json` object of strings.
  A missing file is not an error.
- `run.port` is written only when something outside the house already knows
  it. Such a site cannot hand over (one port holds one generation), so a
  change stops the old process before starting the new one.
- `health.timeout` is how long a new generation has to get ready.
  `health.interval` is how often a running one is asked afterwards (default
  30s, stretched up to four times while it keeps answering, back to the
  interval on the first failure). Static sites are checked for their
  `index.html`. The panel shows the last checks; every change is an event.
- `access`: every rule narrows the one above it, never widens it. A zone's
  `allow` narrows the site's, `deny` adds to it, and a zone's own `password`
  is asked for after the site's. Each password gives a pass valid for its own
  zone only. Rules decide on the cleaned, case-folded path, which is what the
  site will serve. Every `allow` and `deny` entry is an address or a CIDR;
  one that is neither rejects the whole manifest rather than being skipped,
  because a skipped entry can leave an allow list empty, and an empty one
  admits everyone. Passwords are stored hashed; set them from the panel or
  the API, which hash what they are given.
  `key` hides the site: without `?access-token=<key>` once in the address,
  every name of the site answers the page of a name the house does not
  serve (404), ahead of the forbidden page and the password box, so
  nothing says a site is there. With it, the browser gets a pass for 400
  days (HttpOnly, SameSite=Lax) and is sent to the same address without
  the key; the rest of `access` applies after. It holds
  `sha256:<64 hex>`, the key's hash, never the key: the house makes the
  key (panel, Access tab; `POST /sites/{domain}/access/key`; `middleware
  site key <domain> on`) and shows it once. A new key ends every pass of
  the old one. A service's `access.key` is asked for after the site's.
- `security` decides by what a caller claims to be, and is the softest door
  on purpose: the country is the edge's and can be believed, the
  User-Agent is written by the caller and can say anything. It clears out
  what identifies itself (crawlers, AI crawlers, scripts), not anyone who
  insists; `access` is for that. Four lists, all of which must pass:
  `agents` (User-Agent fragments, two characters at least), `bots`
  (families: `search`, `ai`, `social`, `seo`, `tools`, `monitors`,
  `other` for anything that says bot, crawler or spider and is in no
  table; `except` by robot name; `no_ua` closes requests without a
  User-Agent; `window` lets the closed families in between two times in
  `zone`, the machine's if empty), `browsers` (`edge`, `opera`, `samsung`,
  `ie`, `firefox`, `chrome`, `safari`, `other`, `none`) and `countries`
  (two letters, `XX` unknown, `T1` Tor). In each, deny first, and an allow
  list with anything in it means only those. An allow list of countries
  shuts out a request that arrives without one. A refused visitor gets 403
  with a line that says why but not what the rule holds (`message`
  replaces it); a robot refused by family with a window is told the hours.
  Before all of that, every site answers 404 to what no website serves:
  `.git/`, `.ssh/`, `.env*`, private keys, `/etc/passwd` and the like
  (`GET /api/v1/security/catalog` lists them), plus the site's own
  `paths`, matched by whole segments. `/.well-known` is never cut and
  cannot be listed. An unknown family, browser, country or a malformed
  time rejects the manifest. The last 50 refusals per site are kept in
  memory for the panel.
- `attack` is under-attack mode: before anything is served, the browser
  finds a number whose SHA-256 with the challenge starts with `bits` zero
  bits (8 to 24, default 16; each bit doubles the work, checking it is one
  hash) and gets a pass for `life` (default 30m), signed with a key kept on
  disk, so a restart sends nobody back, and bound to the address and
  User-Agent that solved it. It guards nothing (whoever runs JavaScript gets
  through): `access` does that. While it is on, what is not a browser does
  not get in; `skip` lists the paths that never get the challenge, by whole
  segments, and each line is an open door. A POST interrupted by it is lost.
  `until` (RFC 3339) switches it off by itself; empty is permanent. The
  clock stops charging at once and the house writes it down as off within a
  minute, keeping `bits` and `skip`. It runs after `security` and before the
  password, so a flood never reaches a key derivation without paying first.
  The WAF policy's `attack` applies to every site whose own is off (with the
  site's `skip`) and to the panel, except requests that carry their own
  credentials (an API token, the site door). The old `waf.under_attack:
  true` is read as `attack.on` with the defaults.
- `waf.off` takes the site out of the rate limit and the body limit
  (`rate`, `max_body`, its own and the policy's). The block list and the
  bans for exploits, scanners and failed logins still apply: they protect
  the machine, and a block closes every site.
- `headers` go on every answer of the site, the house's own pages included,
  and replace the site's header of the same name rather than being sent
  next to it (two `Access-Control-Allow-Origin` break CORS). A
  `Cache-Control` here replaces the site's except for its `private`,
  `no-store` and `no-cache`, which stay: a line written for the public
  pages does not make the signed-in ones public. On static files
  `cache.static_max_age` sets `Cache-Control` over both.
- Static files need nothing written to be served well. Text (HTML, CSS,
  scripts, JSON, SVG, XML) of a kilobyte or more goes out gzipped to the
  clients that take it, compressed once per change of the file and not once
  per visit; pictures, video and `woff2`, compressed already, go as they
  are. A script, stylesheet or font named by a hash of its content, as
  bundlers write them (`index-Bx2Yz9Qa.js`, `main.8f3c2a1b.css`), answers
  `Cache-Control: public, max-age=31536000, immutable`, since its name
  changes when it does; pages and every other file are revalidated as
  before. A `Cache-Control` in `headers`, or `cache.static_max_age`, wins
  over that. What a site's own process answers is passed on as it sent it,
  compressed or not.
- `cache.rules` are the house's response cache, kept in memory and served
  without asking the site, behind the door so nothing passes a password.
  `origin` keeps what the site marks (`s-maxage`, else `max-age`) for as
  long as it says, so switching it on over an app that sends nothing changes
  nothing; `force` keeps `ttl` seconds whatever the site says, for what is
  known to be public and still; `never` keeps nothing, for `/admin`.
  Prefixes go by whole segments and the longest wins. An answer is kept by
  scheme, name and URI, so a page made for `http://` is never given to
  `https://`. What no rule can
  change: nothing with `Set-Cookie`, `private`, `no-store` or `no-cache`,
  `Vary: *`, `Cookie` or `Authorization`, nothing but a 200 to GET or HEAD,
  nothing for a request with `Authorization`, no event streams and no
  WebSockets (a request with `Upgrade` or `Accept: text/event-stream` never
  enters the cache). Simultaneous misses reach the site as one, until the
  first answer shows it will not be kept (its headers say so, or it passes
  8 MB): then the others go to the site too, so a second client of a stream
  does not wait for the first one's to end. A handover empties the site's
  cache. A request with a cookie passes the cache by, neither answered from
  it nor kept, because a signed-in user's page that says nothing about
  caching would otherwise be kept under `force` and served to everyone;
  the house's own cookies (the door's pass, the challenge's, the panel's
  session) do not count. `"ignore_cookies": true` on a rule says its pages
  read no cookie (images, an analytics cookie on every visitor) and keeps
  and answers them as if they had none; it is refused on `never`.
  `max_mb` asks for less than the house's budget, never for more.
- `vpn` makes the site exist only inside the VPN (the module of that name):
  it is served through the VPN door (the server's address in the tunnel, on
  80 and 443, open while the VPN is up) and through no other, where it gets
  the same page as a name nobody serves, whatever the headers say. It is
  never published, and a published site that becomes VPN-only is taken
  down; when it leaves the VPN it is published again on the door it was on. `vpn.devices` narrows it to those peers, by their address in the
  tunnel, which WireGuard makes trustworthy. The VPN's own resolver, which
  every device's config points at, answers the house's names with the
  server's address in the tunnel and no AAAA, and forwards every other
  name. Its certificate needs dns-01 (the Cloudflare module): no CA can
  reach it.
  `vpn.paths` keeps only those paths inside the VPN (`["/admin"]` covers
  `/admin` and everything under it, not `/administrator`; the path is
  cleaned first, so `//admin` is `/admin`): the rest of the site stays
  public and published, and those paths get, outside the VPN, the page of a
  name nobody serves. A device reaches them because the VPN's resolver
  sends it to the VPN door for every name of the house. The panel sets all
  of this from the site's Access tab. The panel itself can be kept inside
  the VPN from Inbound → VPN; the machine's own loopback (SSH, the CLI)
  still reaches it, which is the way back.
- `autofix: false` keeps the automatic repair (Settings) away from this
  site, whatever the installation says; the panel cannot override it.
- `autopilot: false` keeps the network's autonomous agent away from this
  site: it is not shown the site, and every call of its that names it is
  refused (api.md, The autonomous agent).
- `aliases` are served exactly like the site. A name that is already a site,
  or that two sites claim, is served by neither, and an event says so. An
  alias that is an apex (`patata.es`) brings its `www.` with it, as the
  site's own apex name does: published, certified and sent to the site
  (`"www": false` turns both off). A name the zone already points
  elsewhere is not taken without the operator's yes (api.md, Publishing).

## Services: several processes and several names

A site whose project is more than one process (a mock, a Next.js app, a
Drupal and its database) declares them in `services` instead of `run`.
Each service is a run section with the same fields and rules as a site's,
the names it answers, and the order it starts in:

```json
{
  "version": 2,
  "services": {
    "mock":   { "run": { "command": ["node", "server.js"], "health": { "path": "/healthz" } },
                "domains": ["bakery.example"] },
    "next":   { "run": { "runtime": "docker", "docker": { "build": "deploy/next/Dockerfile", "memory": "768m" },
                         "command": ["npm", "start"], "env_files": ["deploy/next/.env"], "health": { "path": "/api/health" } },
                "domains": ["next.bakery.example"] },
    "drupal": { "run": { "runtime": "docker",
                         "docker": { "build": "deploy/drupal/Dockerfile", "memory": "1g", "volumes": ["drupal:/var/lib/app"] },
                         "env": { "DB_HOST": "$SERVICE_db_HOST", "DB_PORT": "$SERVICE_db_PORT" },
                         "health": { "path": "/user/login", "timeout": "10m" } },
                "domains": ["drupal.bakery.example"], "depends_on": ["db"] },
    "db":     { "run": { "runtime": "docker", "docker": { "image": "mariadb:11.4", "port": 3306 },
                         "env_files": ["deploy/db/.env"], "health": { "command": ["healthcheck.sh", "--connect"] } },
                "internal": true }
  }
}
```

- `domains` are the names the service answers: `"host"`, or `"host/prefix"`
  for part of a name (`"shop.example/api"`: the longest prefix wins, by
  whole segments, and the path reaches the service as it came). They are
  published, certified and refused when another site claims them exactly
  as `aliases` are, and an apex brings its `www.`. The site's own name must
  be one service's. A name in two services is refused; a host the site
  does not serve, or a path no prefix covers, gets the house's page.
- `internal: true` (or no `domains`) is a service only the site's other
  services reach.
- `depends_on` holds a service's first start until those it names are up
  and passing their health, for as long as that takes; `site show` says
  what it waits for.
- `optional: true` keeps the site counted as up while that service is down.
- `handover` is `true` for a service with names (two generations for a
  few seconds, as a site's) and `false` for one without, which is stopped
  and started again on the same port: a database cannot run twice on one
  volume. A written `run.port` never hands over.

Each service has its own revision, build, health check, backoff, log
(`logs/<domain>/<service>.log`) and state file. One that fails to build or
to get healthy keeps its previous generation serving and backs off; the
others are not touched, and while a service starts or is down its names
get the house's page (503 with `Retry-After`), never a raw 502. A change
to one service's section, its env files or the files its image is built
from restarts that service only. The site is up when every service that
is not optional is. Events about a service have `<domain>#<service>` as
their subject.

**Reaching each other.** A site's containers share a network of their
own, `mw-<domain>-net`, in which each is reachable by its service's name:
`drupal` reaches `db:3306`. `$SERVICE_<name>_HOST` and `$SERVICE_<name>_PORT`
(a dash in the name as an underscore) are expanded in a service's command
and env: from a container, the service's name and its port inside; from a
process on the machine, `127.0.0.1` and its port there. A process may only
name a service that restarts in place (its port stays); a container
cannot reach a process on the machine. Inside a service's container its
port is `docker.port` (for an image that listens on its own, mariadb's
3306) or 8080, and `$PORT` says which.

**What is published.** A service's port goes on the machine's loopback
only, and only when something on the machine needs it: a service with
names, one a process service names or depends on. An internal container
that only containers reach is on the site's network and nowhere else
(the loopback is every site's). The house cannot dial such a service, so
its health is `health.command`, run inside it (exit 0 is healthy), or,
without one, that the container stays up; `health.path` there is refused.

**Images.** An `image` in a service names its version: a tag other than
`latest`, or a digest. A `build` is made with BuildKit (`docker buildx
build`) when the machine has it (the house's requirements install it),
the site's folder as its context, its `.dockerignore` read (BuildKit also
reads a `<Dockerfile>.dockerignore` next to the Dockerfile); its output
goes to `logs/<domain>/build-<service>.log` (`site logs <domain> --service
<name> --build`) and a failure says its last lines. `docker.watch` lists
the folders whose files are part of the image's revision, so changing them
rebuilds that service and no other; empty is the Dockerfile's folder, or
the Dockerfile alone when it sits at the root. An image built for another
architecture is refused before it runs, with what to do instead.

**Volumes.** A named volume is the site's, whichever of its services
mounts it: `"drupal:/var/lib/app"` is `mw-<domain>-drupal` (dots as
dashes), the same name it had as a worker's or a site's, so nothing is
renamed or migrated.

**Rules per service.** `disabled`, `hold`, `access`, `security`, `waf`,
`attack`, `headers` and `cache` may be written in a service, and apply to
the requests its names (and prefixes) answer. `waf`, `attack`, `headers`
and `cache` replace the site's for that service (a service with a `waf`
of its own has a rate bucket of its own; its cache is kept apart from the
other services'), so an upload endpoint can take a larger body or an API
be left out of the challenge. `access` and `security` only ever add: the
site's are judged first and the service's after them, never instead, so a
service's allow list, deny list, password, zones or closed countries close
more and can never open what the site closed. An allow list or a password
at both levels, or the same zone at both, is refused, since it could only
be read as one level replacing the other. A disabled or held service shows
its page on its names while the others serve; the site's `disabled` and
`hold` still stop and hold every service. `site stop|start|hold|unhold
<domain> --service <name>` write the service's section. In the access
log, a request a service answered ends in `service=<name>`.

**Moving a site from `run` to `services`** (the whole plan, with the data,
is [services.md](services.md)). Check the new manifest first
(`middleware site validate <domain> --file site.new.json`), then replace
`run` and `aliases` with `services`. The run the site had keeps answering
its names until every service with names that is not optional is up, and
only then is it stopped. Going back is restoring the previous manifest
from the site's history.

`services` and `run`, `upstream`, `static` or `aliases` are exclusive; a
site's `workers` stay, with names that are not its services'. A delegate
cannot change a service's `domains`, or its runtime, docker section and
env files, as for a site's run.

## Redirects

```json
"redirects": [
  { "from": "/old", "to": "/new" },
  { "from": "/blog/*", "to": "https://news.example.com/articles/*", "code": 308 },
  { "from": "alias.example.net/*", "to": "https://example.com/*" }
]
```

A request that matches `from` is sent to `to` with `code` (301 by default;
302, 303, 307, 308), before the site is served: an old address goes on even
while the site is stopped or behind its holding page. `from` is a path, a
path and everything under it (`/*`), or one of the site's names with either
(an alias sent elsewhere whole). When `from` and `to` both end in `/*`, the
rest of the path follows; the query string follows unless `to` has its own.
The first rule that matches wins; one that would send a request to itself
is not followed. The site's page edits them (Redirects).

## Forcing HTTPS

```json
"force_https": ["example.com", "www.example.com"]
```

Each name listed answers only over https: a request for it over plain
http is sent to the same address on https (301, or 308 for anything but
GET and HEAD, so a form keeps its method and body) before anything else
answers it, the password door, the attack page and the cache included.
It goes name by name because a site with several names may keep one on
http (an old device, a webhook that cannot follow a redirect). A name the
site does not answer is ignored, so an alias taken away leaves a harmless
line. The site's page edits it (Force HTTPS), one switch per name.

The ports door already sends every name to https on port 80, so this is
for the tunnel door, where Cloudflare serves `http://` unless told
otherwise and cloudflared says which it was in `X-Forwarded-Proto`. A
client on the machine that asks the loopback listener for the name with
no such header is sent to https as well. The local door (the bench) and
the VPN door are left alone: the first has no https to go to, and inside
the VPN plain http is already encrypted. The panel's name is never
affected. The CA's challenge is answered over http as always.

The redirect says nothing about later visits; for that, add
`Strict-Transport-Security` to `headers` once https works on every name
of the site.

## Web Vitals: `web_vitals`

```json
"web_vitals": true
```

Off unless turned on (the Visits tab of the site page has the switch).
On, the house adds `<script src="/.well-known/middleware/vitals.js" defer>`
before `</head>` of every HTML page the site answers with 200 to a
browser opening it, and answers that path and the beacon
(`POST /.well-known/middleware/vitals`) itself. The script measures LCP,
INP (the longest interaction), CLS, FCP and TTFB with the browser's own
`PerformanceObserver` and sends them once, when the page is hidden.

What is kept, in `data/webvitals/<domain>/<YYYY-MM>.json`, is a histogram
per metric per day and device (mobile or desktop) and, for the fifty pages
of the month measured first, per page: no address, no agent string, no
query string. The report is the 75th percentile (what Google ranks by)
with the share of good and poor against web.dev's thresholds.

What it changes in the page's answer:

- the site is asked for the page uncompressed (to find `</head>`) and the
  house compresses it with gzip when the browser takes it; a site that
  compresses anyway keeps its page as it is, without the script;
- `Content-Length` goes and a strong `ETag` becomes weak;
- a page that flushes before `</head>` (streaming SSR) is not held back:
  it goes out without the script;
- a `Content-Security-Policy` needs `'self'` in `script-src` and
  `connect-src` for the script to run and report.

Only a beacon the browser marks as sent by a page of the same site
(`Sec-Fetch-Site: same-origin`, or a matching `Origin`) is taken, at most
3000 a minute per site.

## Previews: `previews`

```json
"deploy": { "github": { "repository": "acme/shop", "branch": "main" } },
"previews": { "branches": ["feature/*"], "keep": "168h" }
```

Each branch pushed to the site's repository (other than the one it
deploys, and among `branches` when given: globs, empty is all) gets a
preview: a test copy of the site, made from production as the panel's
test copies are (files, databases, a password when production has none),
named `<branch>-preview.<domain>` (`feature/login` →
`feature-login-preview.shop.example`, one label so the domain's wildcard
certificate covers it) and published when the house publishes names. It
deploys its branch on every push to it, with production's build steps.
Its `site.json` is the copy's, with `staging: {of, branch}`; the code sees
`MIDDLEWARE_STAGING` and `MIDDLEWARE_PREVIEW=<branch>`. If the repository
ships its own `site.json`, the copy keeps its mark, its password, its
branch and its names over it.

A preview goes when its branch is deleted (GitHub's push for the
deletion), when nothing has been pushed to it for `keep` (a week by
default; checked every hour), or when production no longer has
`previews`. Removing it unpublishes its name. Owner only. Pull requests
get their previews through their branch; the house does not comment on
them.

## Canary deploys: `run.canary`

```json
"run": {
  "command": ["npm", "start"],
  "canary": { "percent": 10, "for": "10m", "max_error_rate": 0.05, "min_requests": 20 }
}
```

A new generation (a deploy, a restart, a changed manifest) starts and
passes its health check as always, and then, instead of taking every
request, takes `percent` of the visitors (10 by default) for `for` (10
minutes by default, 10s to 24h) while the generation that was serving
keeps the rest. Each visitor stays on the side they were drawn for (a
cookie, `__mw_canary`, for that revision), so a page and the files it
loads come from the same code.

Every answer on both sides is counted. Once the canary has answered
`min_requests` (20) and more than `max_error_rate` (5%) of them were
errors (a 5xx, or no answer at all), and that is worse than the serving
side, it is stopped and the serving generation keeps everything: when
both sides fail alike the fault is not the new code's (a database down),
and keeping the old one would fix nothing. If the canary process exits,
it is rolled back as well. Otherwise it takes over at the end, as a
normal handover does; with fewer than `min_requests` by then it takes
over too, since there was not enough traffic to judge it.

A rolled back revision is written down (`data/canary/<domain>.json`) and
not started again until the revision changes or someone restarts the
site; the site shows `canary rolled back: <why>`. The site page shows a
running canary with both sides' numbers and buttons to promote it or roll
it back now (`POST /api/v1/sites/<domain>/canary/promote|rollback`).

What it does not do:

- a site with `run.port` cannot have it (two generations cannot share a
  written port), and neither can a service;
- the files on disk are the new deploy's whatever happens: a rollback
  keeps the previous generation running, not its files, so the next
  restart runs the new code. With a deploy by pointer
  (`env_from_file`) the pointer says which build runs, and a rollback
  plus moving the pointer back is a full undo;
- a database migration the new generation ran is not undone;
- the house cache serves a kept page to both sides.

## Status page: `status_page`

```json
"status_page": { "path": "/status", "title": "Shop" }
```

A public page with the site's state now, its uptime over 24 hours and 7,
30 and 90 days, a bar per day and its outages. The house answers `path`
(`/status` when left out) itself, in front of the site, so it still
answers when the site does not, which is when people open it. It sits
inside the doors: a site behind a password has its page behind it too.
`?format=json` (or `Accept: application/json`) gives the same as JSON,
with `Access-Control-Allow-Origin: *` for a badge elsewhere.

The house counts every site, page or not, once a minute: up when its
health check passes (a static site whenever the house answers),
down when it fails or the site cannot start, maintenance when it is held,
stopped or disabled. Maintenance is shown and not counted against the
uptime. An outage is two minutes down in a row or more: one bad minute is
a restart nobody saw. The page shows no reasons (they are the house's
error messages); `GET /api/v1/sites/<domain>/uptime` has them.

It is the house's view, not a probe from outside: a whole machine down
is the one thing a page served by that machine cannot say, and its
footer says so.

## Move

```json
"move": { "exclude": ["storage/products/*", ".next-*/cache"] }
```

What a move to another node, or an import, leaves behind: globs relative to
the site's folder (`*` does not cross a `/`), for folders and files alike,
of what the site makes again on demand (an image cache, a build's cache). A
folder's own pattern leaves out the folder; `folder/*` leaves out what is in
it, so the folder arrives empty for the site to write into. Nothing is
installed inside what is left out, and the plan a move or an import shows
first counts without it. A pattern that leaves the folder, or that would
leave out `site.json`, is refused.

## Staging: test copies

A test copy is a second site made from a production one under a name of
its own (`staging.<domain>` unless another is given), with production's
files and databases as they are at that moment: the panel's site page
(Staging card), `middleware site staging <domain> create` or
`POST /sites/{domain}/staging`. The copy:

- has `staging.of` in its manifest naming production;
- leaves out what names production: its aliases, `force_https`, the
  `www.` name, fixed ports, `cluster` and `backups`. A service's names
  under the domain are renamed under the copy's (`api.example.com` →
  `api.staging.example.com`); other names are left out and said so;
- is behind a password when production has none, printed once;
- gets `MIDDLEWARE_STAGING=<production domain>` in its environment, so
  the code can tell (a payment in test mode, no mail to customers);
- copies SQLite files with `sqlite3 .backup` when sqlite3 is installed,
  since a file copied while the site writes it can be torn. Named docker
  volumes (a container's database) start empty;
- does not carry production's tasks (`tasks/`), which would run twice.

**Promoting** the copy (`… staging <copy> promote`) puts its files into
production and keeps of production its own `site.json` and every path
`staging.keep` names:

```json
"staging": { "of": "example.com", "keep": ["*.sqlite", ".env", "storage/app/*"] }
```

A glob without a slash matches a name at any depth, one with a slash the
path from the folder. Without `keep` the default keeps SQLite files and
their `-wal`/`-shm`/`-journal`, `.env` and `.env.*`, and folders named
`storage`, `uploads` or `data`. Kept paths are hard-linked, the very files
production is writing, so nothing written during the promotion is lost.
The new folder is built beside production and the two are exchanged in
one rename; production then hands over to a new generation the usual way.
A change to the copy's `site.json` is not carried: write it in
production's.

The folder that served until the promotion is kept under
`data/staging/<domain>/before`, and **undo** (`… staging <domain> undo`,
on production) puts it back with the kept paths as they are now: the
orders taken since the promotion stay. **Refresh** (on the copy) copies
production into it again, data too, keeping the copy's manifest and
password.

## Cluster: `cluster`

```json
"cluster": { "replicas": 2, "spill": 70, "gateway": ["127.0.0.1:3306"] }
```

Copies of the site on other nodes of the network, which this node hands
requests to when its load per core passes `spill` (70 % by default) or its
own instance is down. The names keep pointing here. `nodes` names the
nodes to use first, `mode: "spread"` shares always by free capacity, and
`gateway` lets the copies use this node's database through the mesh. In a
site with services, `keep` names the services that stay here (those with
volumes stay anyway); the copies run the others and reach the kept ones
through the mesh by the same names. Refused for a site whose data a copy
would fork (SQLite, workers, volumes on its own run). `replica` is what the house writes on a copy; never
write it by hand. See [cluster.md](cluster.md).

## Workers

A worker is a process that must always be up and answers no domain: a backend
behind a frontend, an internal API, a queue consumer. It is declared inside
the site (or project) it belongs to. With no `port`, one is assigned and
passed in `$PORT`; write it only when whoever calls the worker already knows
it. Workers are supervised like instances, saved to `data/state` and
re-adopted after a supervisor restart, but never handed over: they restart.
`disabled` stops one without deleting it.

A worker can run in a container (`"runtime": "docker"` and a `docker`
section, the same as a site's `run`): the database a site needs, declared in
the site, so whatever moves the site knows it is there. A named volume is the
site's (`pg:` is `mw-<site>-pg` whichever of its processes mounts it), and
that is where a database keeps its data, out of the folder.

```json
"workers": {
  "db": {
    "command": ["postgres", "-p", "$PORT"],
    "port": 5433,
    "runtime": "docker",
    "docker": { "image": "postgres:16-alpine", "user": "70:70",
                "volumes": ["pg:/var/lib/postgresql/data"] },
    "env": { "PGDATA": "/var/lib/postgresql/data/pgdata" },
    "move": {
      "export": ["pg_dump", "-Fc", "-U", "app", "-d", "app", "-p", "$PORT"],
      "output": ".move/app.dump",
      "ready":  ["pg_isready", "-h", "127.0.0.1", "-U", "app", "-p", "$PORT"],
      "import": ["pg_restore", "-U", "app", "-d", "app", "-p", "$PORT", ".move/app.dump"]
    }
  }
}
```

`move` says how its data moves with the site, since copying a database's
files while it runs carries a torn copy. `export` runs where the site is,
with the site stopped and the database still up, and leaves a dump inside the
folder, which the last copy carries. On the target, before the site is put
in place, the worker is brought up on its own, `ready` runs until it
succeeds, `import` runs, and the worker is stopped again: the site then
starts on a database that has its data. A docker worker's `ready` and
`import` run inside its container (the image has the database's tools, and
a command from a manifest never holds the docker socket); a process worker's
run as the site's processes do. All get the site's environment, the
worker's on top, and `$PORT`. `output`, a file inside the folder, gets the
export's standard output, written by the house: inside a database's
container the export runs as the database's user, who cannot write in the
site's folder. The house removes `output` (and its `.part`, all a failed
export leaves) once the import there has ended, and here once the move is
over (the archive of the copy here keeps it); in a folder that is a git
repository it is added to `.git/info/exclude` first, so a dump is never
committed nor pushed. What an export without `output` writes is the site's
to clean. `timeout` bounds each (30m). A move that fails
removes the volumes it created and starts the site again where it was, whose
database has only been read.

A target that already has one of the volumes the move would fill, or on
which a worker's written `port` is taken, refuses the move at its check,
before anything stops. Once the site runs there and the copy where it was
is archived with the dump inside, the volumes of the workers exported with
the site stopped are removed where it was: they hold nothing that is not
in the database there and in the archive. A live worker's volume stays,
listed among the leftovers of moves (what the site wrote after its export
is only there), and so do all of them when the archive could not be
written.

Where the site is a site of a V2 house, the move holds it (its maintenance
page, its other workers stopped) and keeps the workers whose data moves up
for their `export`, which runs inside the worker's container. `"live": true`
takes the data while the site serves instead: `export` runs before the first
copy and `import` on the target before the site stops, so it is down only for
the last copy and the start; what the site writes after the export does not
travel. A live import writes where the worker keeps its data (its volume):
what it writes in the folder is replaced by the last copy, which leaves the
folder as it is where the site was.

`export` must only read: it is run more than once. Before a move stops the
site, each `export` that waits for the stop is tried where it will run,
while the site serves: its program is looked for there (inside the
worker's running container for a docker worker, with the image's `sh`),
and unless that same command (its arguments as resolved, in the same
image) has already worked on this house, it is run once, its `output` to a
scratch file that is then deleted. One that fails refuses the move with
its error and the end of its output (`data/logs/<site>/move-<worker>-trial.log`),
and nothing has stopped. An export written for the machine (`docker exec`,
a client the machine has) is found there, not with the site down. The
plan a move shows first, and `middleware import --dry-run`, look for the
program and say whether the export will be tried. What an export without
`output` writes, the trial writes too, where the real one writes it again.

## Backups: `backups`

The site's databases, copied by the house on a schedule or by hand,
compressed (gzip), into `data/backups/<domain>/<name>/`: outside the
folder, so a copy is never pushed by the github module, served by the site
or carried by a move. Each entry is one database:

```json
{
  "backups": {
    "db": {
      "engine": "mysql",
      "service": "db",
      "database": "drupal",
      "schedule": "30 3 * * *",
      "timezone": "Europe/Madrid",
      "keep": 14
    },
    "app": { "engine": "sqlite", "file": "data/app.db", "schedule": "0 */6 * * *" },
    "pg": { "engine": "postgres", "service": "postgres", "user": "app", "password_env": "DB_PASSWORD", "schedule": "@daily" },
    "redis": { "engine": "command", "service": "cache", "dump": ["sh", "-c", "redis-cli --rdb /tmp/d.rdb >&2 && cat /tmp/d.rdb"] }
  }
}
```

| field | meaning |
| --- | --- |
| `engine` | `mysql` (MariaDB too: `mariadb-dump` or `mysqldump`), `postgres` (`pg_dump`, or `pg_dumpall` for every database), `sqlite` (`sqlite3 .dump`), `mongodb` (`mongodump --archive`) or `command` |
| `service` | the service or worker that runs the database. Its container when it runs in one (`docker exec`, as the container's own user, with the container's environment), or as the site's processes run when it is a process. Empty is the site's own `run`; a site with `services` must name one (sqlite excepted) |
| `database` | the database; empty is the one the container was made with (`MARIADB_DATABASE`, `MYSQL_DATABASE`, `POSTGRES_DB`, `MONGO_INITDB_DATABASE`) and, without one, every database; `"*"` is every database |
| `file` | sqlite only: the database file, inside the folder. It is read where the folder is, with the machine's `sqlite3`, whatever runs the site |
| `host`, `port` | where the server is when the dump does not find it on its own. Inside a container, empty is its local socket; from a process, `127.0.0.1` (a jailed process has no `/run`, where the socket lives) |
| `user` | who connects; empty is the image's administrator: `root` (with `MARIADB_ROOT_PASSWORD` or `MYSQL_ROOT_PASSWORD`), `POSTGRES_USER` or `postgres`, `MONGO_INITDB_ROOT_USERNAME`. A `user` without `password_env` uses `MARIADB_PASSWORD`/`MYSQL_PASSWORD` |
| `password_env` | the **name** of the variable that holds the password, in the container's environment or the site's (`.env`, `env`). The password itself never goes in the manifest; it reaches the client through its own variable (`MYSQL_PWD`, `PGPASSWORD`), not as an argument |
| `dump`, `restore` | `command` only: `dump` writes the copy to its standard output; `restore` reads one from its standard input (without it, a copy is restored by hand). `$DOMAIN` and `$SITE_DIR` are expanded |
| `schedule` | five-field cron, as a task's; empty is by hand only |
| `timezone` | reads `schedule` in that zone; empty is the machine's clock |
| `keep` | how many copies are kept, the newest (7 when empty, up to 1000) |
| `timeout` | bounds a copy and a restore (`1h` when empty) |
| `disabled` | stops the schedule; copies by hand still work |
| `note` | for people |

A copy is `<YYYYMMDD-HHMMSS>-<kind>.<sql|archive|dump>.gz` (UTC), kind
`scheduled`, `manual` or `pre-restore`, mode `0600`. The dump's standard
output comes back to the house, which compresses it: nothing needs gzip, or
a folder it may write, where the dump runs. It is written beside its name
and renamed once whole; a dump that fails, is cut by `timeout` or writes
nothing leaves no copy, and the reason (the end of what the client said) is
the backup's last result and in `data/logs/<domain>/backup-<name>.log`.

A **restore** first takes a copy of the database as it is now (kind
`pre-restore`, the newest three kept apart from `keep`) and restores
nothing if that fails, unless asked to go without it. Then the copy is read
into the client: MySQL dumps drop each table before making it again;
PostgreSQL ones are made with `--clean --if-exists` and one database is
restored in one transaction that stops at the first error; MongoDB's with
`--drop`; SQLite's are loaded into a new file that replaces the old one
whole (its `-wal` and `-shm` removed) and the site is restarted, since its
process holds the old file open.

The schedule is looked at every minute; a copy missed while the house was
down is not taken on its return. Two copies run at once at most across the
house, and one per backup. A stopped site (`disabled`) is not copied on
schedule. Removing a backup from the manifest stops it and keeps its
copies, listed until they are deleted. `middleware site backups <domain>`
and the site's **Backups** tab list, take, download, restore and delete
them (docs/api.md).

## Tasks: `tasks/<name>.json`

```json
{
  "version": 2,
  "schedule": "0 3 * * *",
  "site": "example.com",
  "command": ["./scripts/backup.sh"],
  "timeout": "1h",
  "overlap": "skip",
  "env": { "KEEP_DAYS": "30" },
  "timezone": "Europe/Madrid",
  "title": "Nightly backup",
  "note": "Why it runs at this hour.",
  "disabled": false
}
```

`site` makes the command run inside that site's folder with that site's
environment. A site with `claude_api` gives its tasks the same
`MIDDLEWARE_API` and `MIDDLEWARE_TOKEN` as its process: the same token,
so the same models, permissions and `daily_usd`, spent from the same day.
They are added at each launch from the manifest as it is then and never
written here: a task's `env` cannot set them, and a site whose block is
gone gives the next run nothing. Without `site`, the task runs from the root directory. `overlap` is
`skip` (default) or `queue`. `timezone` reads `schedule` in that zone, so a
daylight-saving change moves nothing; without it the schedule is in the
machine's clock. `title` and `note` are for people. A site's tasks go with
it when it moves to another node or is imported, and are turned off where
it was.
