# API v1

Everything the panel and the CLI do goes through `/api/v1`. JSON in, JSON out.
Errors are `{"error": "message"}` with a 4xx/5xx status. The API answers on
the panel domain and on the loopback listener; it is never served on a site's
domain.

## Authentication

- `Authorization: Bearer <token>` — API tokens, created in Settings.
- Session cookie `mw_session` — set by `POST /api/v1/auth/login`.
- The unix socket `data/api.sock` needs no credentials: whoever can open it
  already owns the files. The CLI uses it. One exception: a process started
  by a site conversation's agent turn (one of its parents is the turn) is
  that site's, with the routes a token scoped `read`, `restart`,
  `deploy`, `exec` and `tasks` for it opens, and `GET /events?subject=<its site>`;
  anything else is a 403 and an `api.turn_refused` event. `/auth/me` says
  `turn`.

**Who reaches the panel.** The address lists of `/auth/panel-access` go before
everything the panel answers (403). The unix socket always gets in; a TCP
connection from loopback does not (a site's process, or a site made to
fetch a URL, connects from there too) and is `127.0.0.1` to the lists. The
site door (`/api/v1/site/…`) is let past the lists on the containers'
socket and from loopback without a proxy header, since it answers a site's
token and nothing else. A list that would shut out the caller saving it is
refused. With `passkey_only`, the password login answers 403 without
comparing anything, from loopback too. `middleware panel open [30m]` on
the server lifts both for a while, without a restart; the panel shows a
red strip meanwhile.

**Passkeys** are WebAuthn credentials bound to the name the panel was opened
by (ES256, Ed25519, RS256; attestation not asked). They work on a secure
origin with a name, never on a bare IP, and survive a password change.

A cookie request that is not a GET must also come from the panel's own
origin (`Sec-Fetch-Site: same-origin`, or a matching `Origin`): every
subdomain of a domain is one site to the browser, so a page on a sibling
subdomain sends the panel's cookie.

**Service doors.** A token created with `scopes` opens only the routes those
scopes list, and with `:site` only for that site: `read` (the site list, one
site, its logs, the events, who calls at it and what its `security` refused), `restart` (a site's handover and a worker's
restart), `deploy` (a site's deploy), `exec` (a one-off command streamed
into the site's container or a service's, never on the machine), `tasks` (a site's own tasks, the six
`/sites/{domain}/tasks` routes below; never `/tasks`), `backups` (a site's
database copies, the `/sites/{domain}/backups` routes below). Anything else answers
403 and names the scopes. For a program that acts on the house from outside, an app that builds
sites and restarts them, it is the narrowest door that works. A token
without scopes is the operator's and opens everything. An `import:<site>` token
opens only that site's import (below, "Moving a site to another node") and
seeing or removing that one site; it is made by `POST /relocate/import-token`
and stops working after two hours (`expires`).

**Delegated users.** The operator can let other people manage some sites
and nothing else (`/users`, below). A user signs in with
`POST /auth/login {user, password}` at the node they were made on and gets
a cookie session like the operator's, held to their grants: each grant is a
site and permissions out of `view` (the site, its logs, files, history,
tasks and their logs, the backups and their logs: implied by every grant),
`operate` (restart, stop, start, hold, workers, run and stop tasks, take a
database copy, empty the cache), `code` (the manifest, files, deploy, exec,
writing and deleting tasks, downloading, restoring and deleting database
copies) and `agents`
(the site's conversations, `GET /sites/{domain}/agents`). Outside those a
user reaches `GET /sites` and `GET /events` (both narrowed to their sites),
`/events/stream` (the same), `/auth/me`, `/auth/logout` and
`PUT /auth/password`. A site not theirs answers 404, a route their grant
does not open 403, and any other route 404. Deleting, renaming, moving,
publishing and containerizing a site and its LLM gateway stay the
operator's. With `code`, a manifest that changes `aliases`, `upstream`,
`move`, `vpn`, `claude_api`, `claude`, `agent`, `chat`, `needs`, or a
`runtime`, `docker`, `env_files`, `env_from_file` of `run` or of a worker,
is refused with 403 naming the key, whether it comes through
`/manifest`, a kept version or `PUT /file?path=site.json`. A user's site
may live on another node: the request goes to this node's own path all the
same and is answered by the node that holds the site, through the mesh,
which checks the grant again; nothing in the answer names that node. The
events about moves, nodes, publishing and the house are never shown to a
user. What the permissions mean for the machine: docs/security.md,
"Delegated users".

First run: `middleware setup` prints a one-time code; `POST /api/v1/auth/setup`
with `{"code": "...", "password": "..."}` creates the operator.

**CSRF.** A request authenticated by the cookie that is not a `GET`, `HEAD`
or `OPTIONS` must carry the header `X-Requested-With: middleware`, or it is
refused with 403. A page on another origin can make the browser send the
cookie but cannot add a custom header without a CORS preflight, which this
API never answers. Tokens and the socket need no header.

| method | path | body / result |
| --- | --- | --- |
| POST | `/auth/setup` | `{code, password}` → `{ok}` |
| POST | `/auth/login` | `{password}` (the operator) or `{user, password}` (a delegated user) → sets cookie, `{ok}`; a user's password is not closed by `passkey_only` |
| POST | `/auth/logout` | → `{ok}` |
| GET | `/auth/me` | → `{authenticated, via: "cookie"|"token"|"socket", turn?, setup_pending, passkeys, password_closed, open_until?}`; `turn` is the site whose agent turn called over the socket; a delegated user gets `{authenticated, via, user: {name, sites: [{site, perms}]}}` and nothing else |
| PUT | `/auth/password` | a delegated user's own: `{current, password}` → `{ok}`; ends their sessions, this one included; 403 for the operator |
| GET | `/users` | → `[{id, name, created, grants: [{site, perms}], agents, last_seen?}]` the delegated users |
| POST | `/users` | `{name, password, grants?: [{site, perms}], agents?: {provider: [models]}}` → the user (201); `name` is 2–32 of `a-z 0-9 . _ -`; 409 for a name taken, 400 for a permission outside `view, operate, code, agents`. `agents` limits their conversations: `null` (or absent) is every agent, `{}` none, a provider with `[]` every model of it; providers and models need not be on the machine |
| PUT | `/users/{id}` | `{grants?, agents?, password?}` → the user; a new password ends their sessions, new grants and limits hold from their next request; `agents` absent keeps the limits, `null` lifts them |
| DELETE | `/users/{id}` | → `{ok}`; their sessions end |
| GET | `/auth/tokens` | → `[{id, name, scopes?, created, last_used}]` |
| POST | `/auth/tokens` | `{name, scopes?: ["restart:shop.example", "read"]}` → `{id, token}` (shown once); 400 for a scope outside the list |
| DELETE | `/auth/tokens/{id}` | → `{ok}` |
| GET | `/auth/passkeys` | → `[{id, name, rp_id, created, last_used}]` |
| POST | `/auth/passkeys/register/options` | → `{challenge_id, publicKey}` for `navigator.credentials.create()`, bound to the name the panel is opened by |
| POST | `/auth/passkeys/register` | `{challenge_id, name, client_data_json, attestation_object}` (base64url) → the passkey; 401 when it does not verify |
| DELETE | `/auth/passkeys/{id}` | → `{ok}`; 409 for the last one while the password is closed |
| POST | `/auth/passkeys/login/options` | public → `{challenge_id, publicKey}` for `navigator.credentials.get()` |
| POST | `/auth/passkeys/login` | public, `{challenge_id, id, client_data_json, authenticator_data, signature}` → sets the cookie; 401 when refused, counted by the same brake as passwords |
| GET | `/auth/panel-access` | → `{rules: {allow, deny, passkey_only}, your_address, from_machine, open_until?}` |
| PUT | `/auth/panel-access` | `{allow, deny, passkey_only}` → rules; 400 for an unreadable entry, a list that would shut out the caller, or closing the password with no passkey |
| DELETE | `/auth/panel-open` | → `{ok}`; ends an emergency opening (`middleware panel open` starts one, from the shell only) |

## Sites

A site object:

```json
{
  "domain": "example.com",
  "mode": "run",
  "alive": true,
  "port": 33012,
  "revision": "9f2c…",
  "handing": false,
  "error": "",
  "managed": true,
  "manifest": { … site.json … },
  "workers": { "api": { "port": 8090, "alive": true, "error": "" } }
}
```

| method | path | body / result |
| --- | --- | --- |
| GET | `/sites` | → `[site]` (includes unmanaged folders with `managed: false`) |
| GET | `/sites/{domain}` | → `site` |
| POST | `/sites` | `{domain, manifest?, template?: "static"|"node"|"go"|"docker", repository?, branch?}` → `site`. With `repository` (owner/name, its page or its clone address; the GitHub module must be on, 409 otherwise) the manifest is made from what the repository's root holds (or `template`), with `deploy.github` naming it and its branch, and the first deploy (clone, build, start) starts at once |
| PUT | `/sites/{domain}/manifest` | full manifest → `site` (validated, written atomically) |
| PATCH | `/sites/{domain}/manifest` | partial, top-level keys merged → `site` |
| DELETE | `/sites/{domain}` | `?archive=true` moves to `data/archive/` instead of deleting |
| POST | `/sites/{domain}/validate` | body: a manifest, or nothing for the one on disk → `{ok, error?, problems?, warnings?, routes: [{host, prefix?, service?, conflict?}]}`. Writes nothing. `error` is what the house would refuse it with; `problems`, what would fail once written (a Dockerfile that is not there, a name another site serves: a delegate, a scoped token or a site's own turn are told it is taken, not by whom); `warnings` stop nothing. In the `read` scope and the `view` grant |
| GET | `/sites/{domain}/routes` | → `[{host, prefix?, service?, port?, state, error?}]`: every name the site answers and what answers it now (`up`, `starting`, `down`, `disabled`, `hold`, or why it is not served) |
| POST | `/sites/{domain}/services/{name}/restart` | → `{ok}`; a new generation of that service (rebuilt when it has a `build`), the others untouched (409 when it is starting or stopped) |
| POST | `/sites/{domain}/restart` | → `{ok}` (forces a handover) |
| POST | `/sites/{domain}/exec` | `{command, service?, timeout?: "60s"}` → `{exit_code, output, truncated?, seconds, timed_out?}`; `sh -c command` where the site runs (its folder, environment and HOME, inside its container if it has one, or in a service's: its running image, on the site's network), stdin empty, output capped at 1 MB, a minute by default and 10m at most. With `stream: true` (and `command`, or `args` run as they are) the output is sent as it comes, as `text/plain`, and the trailers `X-Exit-Code` and, when it did not run to its end, `X-Exec-Error` close it; 30m by default and an hour at most. A site's turn and a token with `exec:<domain>` stream only, and only into a container: on the machine the command would run as the supervisor, outside the turn's jail. Every run is a `site.exec` event with the command, its subject `<domain>#<service>` for a service |
| GET | `/sites/{domain}/manifest/history` | → `[{id, at, bytes}]`, newest first; the first is the manifest in force. The engine keeps every valid version it sees, whoever wrote it (50 per site, in `data/history/<domain>/`) |
| GET | `/sites/{domain}/manifest/history/{id}` | the version as it was written |
| POST | `/sites/{domain}/manifest/history/{id}/restore` | → `site`; written back through the same validation as a PUT (400 when it no longer validates), and kept as the newest version |
| POST | `/sites/{domain}/workers/{name}/restart` | → `{ok}`; stops it and the engine starts it again on its next tick, without a crash's backoff (409 when it is not running) |
| POST | `/sites/{domain}/workers/{name}/stop` | → `site`; writes `workers.<name>.disabled: true`, so it stays stopped across restarts |
| POST | `/sites/{domain}/workers/{name}/start` | → `site`; clears it |
| POST | `/sites/{domain}/stop` | → `site` (sets `disabled: true`) |
| POST | `/sites/{domain}/start` | → `site` (clears `disabled`) |
| POST | `/sites/{domain}/hold` | `{enabled, title?, message?}` → `site` |
| POST | `/sites/{domain}/services/{name}/stop` | → `site`; writes `services.<name>.disabled: true`; its names answer the stopped page, the other services are not touched |
| POST | `/sites/{domain}/services/{name}/start` | → `site`; clears `services.<name>.disabled` |
| POST | `/sites/{domain}/services/{name}/hold` | `{enabled, title?, message?}` → `site`; `services.<name>.hold`, the holding page on that service's names |
| GET | `/sites/{domain}/logs` | `?lines=200&worker=name` (or `&service=name`, with `&build=1` for its image build) → `text/plain`; `?follow=1` streams |
| GET | `/sites/{domain}/files` | `?path=/` → `[{name, dir, size, mtime}]` (inside the folder only) |
| GET | `/sites/{domain}/file` | `?path=…` → raw content |
| PUT | `/sites/{domain}/file` | `?path=…`, raw body → `{ok}` |
| GET | `/sites/{domain}/specs` | the project's OpenSpec folder → `{enabled, initialized, changes: [{id, title, why, done, total, files, updated}], archived: [{id, date, path}], specs: [{path, capability, title, requirements}]}` |
| POST | `/sites/{domain}/specs/init` | `openspec init` in the folder (409 with the module off; refused for a site that serves its whole folder) → `{ok, output}` |
| POST | `/sites/{domain}/deploy` | → `{ok, build}` (runs `deploy.github.build`, writes the pointer); `{ok, via: "github"}` when the github module is enabled and the manifest has `deploy.github`: the module's runner fetches the repository first and answers at once, progress on `/events/stream` as `deploy.*` |
| GET | `/sites/{domain}/env` | `?service=name` for one service's → `{resolved: {…}}` what the instance was born with (secrets masked) |
| POST | `/sites/{domain}/rename` | `{to, publish?}` → site; the folder, its HOME, logs, conversations and cost move with it; published names are taken down and the new ones published on the same door. A project (no dot) is given a domain this way (`publish: true` publishes it on the door for new sites); a site becomes a project only when nothing in it answers a hostname (no `run`, `upstream`, `static` or `aliases`) |
| GET | `/sites/{domain}/staging` | → `{of?, copies?, keep, before?}`: for a test copy, the production it is a copy of; for production, its copies and when the folder an undo would put back was set aside; `keep` is what a promotion leaves in production. Owner only |
| POST | `/sites/{domain}/staging` | `{name?, publish?, door?}` → `{site, password?, notes}` (201): a test copy of the site under `name` (default `staging.<domain>`) with its files and databases as they are now (SQLite copied with `sqlite3 .backup` when installed), without production's aliases, `force_https`, fixed ports, cluster and backups, a service's names under the domain renamed under the copy's, `MIDDLEWARE_STAGING=<domain>` in its environment and `staging.of` in its manifest. With no password on production, the copy gets one, returned once as `password`. `notes` say what was not carried (named docker volumes start empty). Published like a new site. Owner only |
| POST | `/sites/{domain}/staging/promote` | → `{site}`: on a test copy, put its files into its production: production's own `site.json` and the paths `staging.keep` names (default: SQLite files, `.env*`, `storage`, `uploads`, `data`) stay, hard-linked so nothing written meanwhile is lost; the two folders are exchanged in one rename and production hands over to a new generation. The folder replaced is kept for an undo. Owner only |
| POST | `/sites/{domain}/staging/undo` | → `{site}`: on production, put back the folder the last promotion replaced, with the kept paths as they are now; an undo of the undo is the promotion again. Owner only |
| POST | `/sites/{domain}/staging/refresh` | → `{site, notes}`: on a test copy, copy production into it again, data too; the copy keeps its manifest (name, password). Owner only |
| POST | `/sites/{domain}/names/{name}/main` | → site; makes one of the site's names its main one: the site is renamed to it as with `rename`, and the old name stays as an alias (for a site with services it is still in its service's domains, and the new one must be a service's domain with no path). Nothing is unpublished: the published names change owner, not place. Owner only |
| DELETE | `/sites/{domain}/names/{name}` | → site; takes a name away: an alias, or every domain of the site's services on that host, and its `force_https` line. The main name is refused, a `www.` that follows its apex too (that is `www: false`), and so is a service's last name. The publisher takes the name down on its next pass. Owner only |
| GET | `/sites/{domain}/download.zip` | the folder as a streamed zip without what an install or a build gives back (`node_modules`, builds, caches); `?all=1` for everything |
| GET | `/sites/{domain}/shot.png` | the site's picture, taken through a door that skips its password; 404 until there is one. Asking keeps pictures renewing for an hour |
| POST | `/sites/{domain}/shot` | take it now, whatever state the site is in; 501 without a headless Chromium |
| GET | `/archive` | archived sites, newest first → `[{name, domain, at, bytes}]` |
| POST | `/archive/{name}/restore` | → site; unpacks it back to `sites/<domain>`, 409 when that folder exists |
| GET | `/archive/{name}/download` | the archive file (`.tar.gz`) |
| DELETE | `/archive/{name}` | → `{ok}`; for good |

**The build contract.** Every command of `deploy.github.build` runs in the
site's folder with the site's resolved environment plus `MIDDLEWARE_BUILD`,
the build id (`20260131T120000Z`). A build writes into a folder named after
it and the pointer file then names that folder, which is what makes the
engine hand over without a restart: `next build` with
`distDir: process.env.MIDDLEWARE_BUILD` is the whole recipe. Without a
pointer the build runs in place and the instance is restarted. A second
deploy while one runs answers 409.

## Repositories

The git repositories inside the sites (the folder and up to three levels
below, not in dependencies, not through links). Marked ones are pulled every
two minutes: fetch, then merge the upstream, never reset or force; a merge
that collides is undone with `merge --abort` and recorded, and the first
failure of a streak is a `repo.failed` event. Pulling is not deploying.
A repository whose remote is one its site's manifest names is fetched with
the GitHub module's token while the module is enabled, so a private one
pulls: `deploy.github.repository` anywhere in the site's folder, each of
`repositories` only in its own `path`. The token goes as a header scoped to
that repository's URL, never on the command line, in the remote or in
`.git/config`, and any other remote gets no token.

| method | path | result |
| --- | --- | --- |
| GET | `/repos` | → `[{id, domain, path, branch, remote, upstream, head: {hash, subject, author, at}, ahead, behind, dirty, auto, last?: {at, ok, message, pulled, failures}, error?}]`; ahead and behind as of the last fetch, the remote without credentials |
| GET | `/repos/{id}/log` | → the last 30 commits |
| POST | `/repos/{id}/pull` | → the repository after a fetch and merge now |
| PUT | `/repos/{id}/auto` | `{enabled}` → the repository; marking pulls once at once. Kept in `data/repos.json` |

## Domains

| method | path | result |
| --- | --- | --- |
| GET | `/domains/search` | `?name=label&tlds=com,es` (default: com, net, org, es, eu, io, dev, app, co, me) → `{results: [{domain, status: free|taken|unknown, sure, price?, error?}], prices_from?, stale_prices?}`. RDAP by IANA's map (a 404 is free, a 200 taken); endings without RDAP fall back to DNS and answer `sure: false`. Prices come from `data/tld-prices.json` (`{"updated": "YYYY-MM-DD", "usd": {"com": 10.44}}`), written by hand; past six months they are `stale_prices`, and an ending not in it has none |

The Cloudflare module adds, under `/modules/cloudflare/`: `GET zones`,
`GET|POST zones/{zone}/records`, `PUT|DELETE zones/{zone}/records/{id}`,
`POST purge {domain, hosts?}` (the edge's cache for those hosts),
`GET cache-rules?domain=` (the zone's cache rules, each with its `scope`:
`site`, `zone` or `other`), `POST cache-rules {domain, path, ttl}` (born
scoped to the host; `ttl: 0` bypasses), `DELETE cache-rules/{zone}/{ruleset}/{rule}?domain=`
(409 for another site's rule unless `other=1`), `GET access` (Access
applications), `GET registrar` (the account's domains and when they expire),
`GET token` (its permissions, `extra` beyond what the house uses and
`missing`) and `POST token/narrow` (mints a token with exactly what the house
uses, proves it and stores it; the old one is named in `revoke` for the
operator to revoke).

## Response cache

The house's cache of responses, behind the door, by the manifest's
`cache.rules` (docs/manifest.md). Responses carry `X-Middleware-Cache:
hit|miss` and, on a hit, `Age`. The budget is `cache.max_mb` in the
settings (256 by default), changed at once on save.

| method | path | result |
| --- | --- | --- |
| GET | `/cache` | → `{bytes, budget, entries}` for the whole house |
| GET | `/sites/{domain}/cache` | → `{entries, bytes, hits, misses, limit}` |
| DELETE | `/sites/{domain}/cache` | → `{ok}`; empties it (a handover does that by itself) |

## Dependency audit

`npm audit --package-lock-only` over each site with a `package-lock.json`, a
few minutes after start and then daily; never `npm audit fix`. A critical
advisory not seen in the previous audit is a `deps.critical` event (and a
push); its key carries the range, so a widened range is news again.
Publishing a site with critical advisories answers `vulnerable` beside the
lines.

| method | path | result |
| --- | --- | --- |
| GET | `/audit` | → `[{domain, at, counts: {critical, high, moderate, low, info}, critical: [{name, severity, range, title, url, key}], high: […], skipped?, error?}]` |
| POST | `/audit/run` | → `{ok}`; every site in the background, `deps.audited` when done |
| POST | `/sites/{domain}/audit` | → the site's report, now |

## Automatic repair

Off until `autofix.enabled` in the settings (with `attempts`, 2; `wait_minutes`,
30; `daily_usd`, 2). A run site with five failed starts in a row, or any site
with three failed health checks in a row, is handed to its agent with the
reason, its command and the engine's error and the last 60 log lines, fenced
as untrusted data between markers drawn at random for each prompt (a fixed
marker could be closed by a request's path written to the log); the house
restarts it when the turn ends and writes the outcome to
`data/repairs.jsonl`. A manifest's `"autofix": false` keeps it away. The
repairs run in a conversation of the site's own for them (origin `repair`,
"Automatic repairs" in the panel), not in the operator's, and without the
credential that lets the panel's conversation `git push`.

The turn runs with the house's AI for automatic work, `automation: {provider,
model}` in the settings (Settings → "The house's AI for automatic work"),
and with the site's own agent when there is none. That AI also repairs a
site that arrived from a move or an import and does not start, before the
move is undone (`/relocate/{domain}/repair`), and for a day after a site
arrives the repair applies to it even with `autofix.enabled` off. It is not
the house's conversation, whose picker says whom the operator talks to.

| method | path | result |
| --- | --- | --- |
| GET | `/autofix` | → `[{at, domain, reason, attempt, turn_id, outcome, cost_usd, recovered, skipped?}]`, oldest first |

## Push notifications

Web push (VAPID, aes128gcm) to the operator's devices. What is pushed is a
closed list: an intrusion (`guard.intrusion`), a critical (`middleware.panic`,
`deps.critical`), a repository that stopped pulling (`repo.failed`, the first
of a streak) and a turn that ended in the panel's conversations (not the site
door's). A device chooses among those kinds; a push service that says a
device is gone (404, 410) gets it forgotten.

| method | path | result |
| --- | --- | --- |
| GET | `/push` | → `{public_key, kinds, devices: [{endpoint, service, name, kinds, created, last_sent, last_error}]}` |
| POST | `/push/subscriptions` | `{endpoint, keys: {p256dh, auth}, name?, kinds?}` (the browser's PushSubscription) → `{ok}`; the same endpoint updates |
| DELETE | `/push/subscriptions` | `?endpoint=<the device's endpoint>` → `{ok}` |
| POST | `/push/test` | → `{sent, error?}` to every device, whatever it chose |

## Events

| method | path | result |
| --- | --- | --- |
| GET | `/events` | `?since=RFC3339&type=prefix&subject=domain&limit=200` → `[event]` |
| GET | `/events/stream` | `text/event-stream`; each `data:` is one event JSON, its `id:` names the event; `Last-Event-ID` (or `?last_event_id=`) resumes after that event, from memory when this process still holds what followed and from the journal otherwise; `?since=RFC3339` catches up from memory first |

## System

| method | path | body / result |
| --- | --- | --- |
| GET | `/docs` | → `[{name, title}]`, the documents under docs/ as embedded in this binary |
| GET | `/docs/{name}` | `text/markdown`, one of them (`api`, `manifest`, `security`…) |
| GET | `/openapi.json` | public; the OpenAPI document of every registered route |
| GET | `/system` | → `{version, root, uptime, listen, panel_domain, load, memory, disk, go}` |
| GET | `/settings` | → settings |
| PUT | `/settings` | settings → settings |
| GET | `/pages/themes` | → `{themes: [{key, name, note}], house}`: the designs of the visitor pages |
| GET | `/pages/preview` | `?theme=&page=down` → the page as HTML, for the panel's preview |
| GET | `/domains/prices` | → `{updated, usd: {ending: price}}`, the hand-written yearly prices |
| PUT | `/domains/prices` | `{usd: {ending: price}}` → the table, dated today |
| GET | `/system/vitals` | → `{now, history: [{at, cpu, cores, iowait, load1, mem_used, mem_total, mem_pct, swap_used, swap_total, disk_used, disk_total, disk_pct}], episodes: [{from, to, reason, peak_cpu, avg_cpu, peak_mem, iowait_max, blamed: [{cmd, site, times, cpu}], shots}], lines}`; a sample every 5 s, an hour kept; `?history=0` leaves the hour out |
| POST | `/system/restart` | → `{ok}` (SIGTERM to self; systemd restarts) |
| GET | `/system/update` | → `{current, latest, available, checked_at, next_check, every_minutes, error, auto, installing, skipped, can_install, why}` (`why`: `not_release` or `no_helper`; `skipped`: a release this machine rolled back from, not offered until a newer one is out, see [upgrade.md](upgrade.md#an-update-is-on-trial-until-it-serves)); `?check=1` asks GitHub (an answer under an hour old is reused) |
| POST | `/system/update/check` | → the same, asking GitHub now |
| GET | `/system/doctor` | → `{ok, checks: [{name, ok, detail}]}`: what `middleware doctor` says of this machine (binary, units, loopback socket, helper, root, reserved ports, docker, fail2ban, Node, wireguard, disk); it only reads |
| GET | `/system/requirements` | → `{state, steps, log, error?, updated?}`: the house's requirements installed from the panel, read from `/run/middleware/requirements.log`; `steps` are the run's report lines as `{name, ok, optional?, detail}` (`optional` is a step that failed without stopping the run); `state` is `none`, `running`, `finished`, `restart` (finished, with something the supervisor only sees once restarted: docker's group, the jail or cgroups drop-in), `failed` or `interrupted` (the log went quiet for 30 minutes without its last line) |
| POST | `/system/requirements` | the operator only: `{browser?}` → `{state: "running"}`; the helper's `requirements.install` (packages, docker and its group, nvm with the LTS, fail2ban, and with `browser` a headless Chromium) in a unit of its own. 409 while one runs, 502 when the helper refused |
| POST | `/system/update/install` | → `{ok, version}` once the helper installed the newest release; the supervisor restarts a second later (sites keep running). 409 when there is nothing newer, 502 when the helper refused or the download failed |
| GET | `/system/ports` | → `{from, to, in_use: {port: domain}}` |
| GET | `/system/processes` | → `[{pid, domain, kind, rss, cpu, started}]` |

The settings object (`data/settings.json`):

```json
{
  "panel_domain": "panel.example.com",
  "listen": "127.0.0.1:46100",
  "ports": { "from": 33000, "to": 47000 },
  "pages": { "stopped": ["Title", "Message"] },
  "update": { "auto": false, "every_minutes": 60 },
  "sites": { "default_limits": { "memory": "2g", "pids": 2048 } },
  "agents": {
    "max_concurrent": 0,
    "reserved_for_house": 1,
    "per_provider": { "claude": 0, "agy": 0 },
    "default_provider": "claude",
    "cpu_cores": 0,
    "memory_max": ""
  }
}
```

`sites.default_limits` is every site's cgroup budget where its manifest's
`limits` leaves a field out (the same block and rules,
[manifest.md](manifest.md#limits-limits)); absent by default, so no site
has a memory ceiling it did not ask for. A change applies to every site
within seconds, without a restart. When the kernel kills inside a site's
group at its ceiling, a `site.oom` event (warn) carries `kills`, `limit`
and `held` (the most the site held, in bytes).

`update` is the self-update: `every_minutes` (1 to 10080, an hour when
absent) is how often a new release is looked for, and `auto` whether one
found installs itself; off, it is announced once (`update.available`) and
waits for `POST /system/update/install`. On, it waits while a site is
moving between nodes (`update.deferred`, once) and is tried again a
minute later; the `POST` does not wait.

`agents` caps the conversations: `max_concurrent` turns at once across
every conversation (1..64, or 0 for computed: the agents' memory ceiling
over 1.5 GiB a turn, from 2 to 64, so 4 GB gives 2, 24 GB 5, 64 GB 14 and
128 GB 28), of which `reserved_for_house` (fewer than the
cap in force) only the `middleware` conversation may take once the rest are busy;
`per_provider` caps one provider's share, 0 meaning no cap of its own;
`default_provider` runs a site whose manifest names none. A `PUT` without
the section keeps the defaults; one that breaks the rules is a 400 naming
the field. The new limits apply to the next message, not the next restart.
`cpu_cores` and `memory_max` (a size as docker writes it, `"8g"`, at least
`256m`) replace the agents' cgroup ceiling, which is otherwise computed:
every core but a quarter of them (at least one is always left to the
sites) and a third of the memory, squeezing from a quarter (three quarters
of an operator's `memory_max`). Zero and empty are computed, and a change
is written to the cgroup at once. `GET /agents` answers `computed` with
this machine's numbers (`cpus`, `memory`, `max_concurrent`, `cpu_cores`,
`memory_max`) and its `capacity` says `max_computed` when the cap in force
came from the machine. A file written before this (no `format`) that holds
the old default `max_concurrent: 8` is read as computed; any other number
stays the operator's.

## Modules

| method | path | body / result |
| --- | --- | --- |
| GET | `/modules` | → `[{key, name, description, enabled, installed, configured, shareable, needs: [{key, label, secret, optional, hidden, local, help}]}]` (a hidden need is filled by the module, not typed; a local one is the machine's own and never propagated) |
| POST | `/modules/{key}/verify` | `{config}` → `{ok, lines: [{status: ok|skip|fail, text}]}`; nothing is written |
| POST | `/modules/{key}/enable` | `{config}` → verifies, installs, enables, saves → `{ok, lines, progress: [text]}`; a failed probe answers 200 with `ok: false` and the lines, a failed install 502 |
| POST | `/modules/{key}/disable` | → `{ok}` (keeps credentials) |
| DELETE | `/modules/{key}/credentials` | → `{ok}` (disables and forgets) |
| POST | `/modules/{key}/propagate` | `{nodes: [id]}` (empty: every other node) → `{ok, results: [{id, name, ok, error, lines}]}`; sends the stored credentials through the mesh to each node's own `enable`, leaving out the machine's own values (`local` needs); 409 for a module never set up or bound to this machine (the VPN). Owner only |
| POST | `/modules/github/hook` | GitHub's push webhook: public, checked by its signature |
| GET | `/modules/{key}` | the module's own status when it is enabled and has one (the VPN), otherwise its entry of the list |

`config` names only the module's needs; an empty secret means "keep the one
stored", never "clear it". Modules mount their own routes under
`/api/v1/modules/{key}/…`, behind authentication, and they answer 404 while
the module is disabled. `POST /modules/github/hook` is public: GitHub
authenticates it with the HMAC signature of the webhook secret.

The modules an installation has: `cloudflare`, `github`, `vpn`,
`agent-claude` and `agent-agy` ([modules.md](modules.md)). The agent
modules take `model`, `api_key` and `bin`, all optional; enabling one
replaces the provider the conversations run with, disabling it puts the
CLI's own login back. Their sign-in with a subscription:

| method | path | body / result |
| --- | --- | --- |
| GET | `/modules/{agent}/session` | → `{provider, session: {kind: subscription|api_key|none, since?}, binary?: {name, version, latest, outdated, managed, path}}` |
| POST | `/modules/{agent}/login` | starts the vendor's sign-in on the server → `{state, url?, error?, tail?}`; `state` is `starting`, `authorize` (open `url`, paste the code), `checking`, `done` or `failed` |
| GET | `/modules/{agent}/login` | the poll; a `done` attempt stores the session, enables the module and answers `{state: "done", session}` |
| POST | `/modules/{agent}/login/code` | `{code}` → the state; 400 when it is not the moment |
| DELETE | `/modules/{agent}/login` | cancels the attempt |
| GET | `/modules/{agent}/usage` | the subscription's windows, one entry of `/agents/usage` (`?refresh=1` asks the vendor now) |
| POST | `/modules/{agent}/binary/sync` | installs or updates the CLI now → `{ok, progress}` |

## Deps

The external programs the modules run (the agent CLIs), installed under
`modules/node` and checked daily against npm.

| method | path | result |
| --- | --- | --- |
| GET | `/deps` | → `[{name, npm, path, version, latest, outdated, installed, managed, checked_at, error?}]` |
| POST | `/deps/check` | asks the registry now (and installs when auto-update is on) → `{deps, progress}` |
| POST | `/deps/{name}/install` | installs or updates one → `{ok, progress, deps}` |

## Inbound

| method | path | result |
| --- | --- | --- |
| GET | `/inbound` | → `{doors: {tunnel: {open}, ports: {open, http, https, listening, error}, local: {open, http, dns, listening, error, help}}, domains: [{domain, door, cert_expires, dns, tunnel_route, published, publish_error}], new_sites, public_ip, dns_error?}` |
| POST | `/inbound/move` | `{domains: [], to: "tunnel"|"ports"}` → `{ok, lines: [{status, text}]}` |
| PUT | `/inbound/publishing` | `{new_sites?: "tunnel"|"ports", public_ip?}` → `{new_sites, public_ip}`; an empty `public_ip` goes back to detecting it |
| POST | `/inbound/doors/{door}` | `{open}` → `doors` (door is `tunnel`, `ports` or `local`; written to `data/inbound.json` and acted on at once) |
| PUT | `/inbound/local` | `{open, http?, dns?}` → `doors`; the local door's listeners (plain HTTP and the name server), 400 when an address is malformed or collides with the ports door |
| GET | `/sites/{domain}/security/seen` | who called at the site → `{countries, bots, browsers, agents: [{key, name, extra, count, first, last}], since, total}`, heaviest first; counted before the security rules, so what they shut out is there too. In the `read` scope (`middleware site security <domain>`) |
| POST | `/sites/{domain}/security/seen/forget` | → `{ok}`; empties the tally |
| GET | `/sites/{domain}/visits` | the site's audience → `{from, to, days: [{date, visitors, pageviews}], visits, pageviews, unique?, countries, regions, devices, browsers, os, referrers, hosts, pages: [{key, count}], hours: [24], months: [{month, unique, visits, pageviews}]}`. `?month=YYYY-MM` for a calendar month (then `unique` is people once each that month) or `?days=N` (1–366, 30 by default). Only a browser's navigation answered with a page counts; robots, headless browsers and programs that wear a browser's name do not. Visitors are hashes of address, User-Agent and day or month with a key of this machine, thrown away when the day or month ends; tables count visitors, `pages` and `hours` count page views |
| POST | `/inbound/migration` | `{domains, to}` → 202 the migration: moves them in the background one at a time (certificate, door, DNS each); 409 while one runs. Progress on `inbound.migration.*` events |
| GET | `/inbound/migration` | → `{id, to, domains, done, current, lines, started, finished, running, cancelled, failed}`, the running one or the last |
| POST | `/inbound/migration/cancel` | → `{ok}`; stops after the domain it is on |
| GET | `/unrouted` | names that reached this house and belong to no site → `[{host, count, first, last, doors: {door: n}, samples: [{when, door, method, path, ip, agent, proxy, tls, sni}]}]`, most recent first; in memory, 200 names, 5 samples each |
| DELETE | `/unrouted/{host}` | → `{ok}`; forget it |
| GET | `/inbound/certificates` | → `[{domain, issued, expires, issuer}]` |
| POST | `/inbound/certificates/{domain}/renew` | → `{ok}`; 502 with the CA's reason when it fails |

`dns` (`"A 203.0.113.9"`, `"CNAME …cfargotunnel.com"`) and `tunnel_route`
(`"yes"`/`"no"`) are filled by the cloudflare module and empty when it is
off; when it is on but cannot be reached, `dns_error` says so and the rest
of the answer stands. A move is publishing names one by one, through the
same publisher as creating a site (below): to the ports door the certificate
comes first (dns-01 when the cloudflare module manages the zone, so the name
need not point here yet) and DNS only after it; the A record uses
`public_ip`, or the address the machine is seen from.

## Publishing

| method | path | result |
| --- | --- | --- |
| GET | `/publish` | → `{name: {site, door, state, error?, attempts?, next?, at?, ip?}}`; state is `published`, `pending`, `failed` (retried with backoff, 1 min doubling to 1 h), `manual` (no DNS provider, or a zone it does not manage: a person points the name), `conflict` (the zone already has an A, AAAA or CNAME on the name pointing somewhere that is not this house, said in `found`; nothing is written until `POST /publish/takeover`) or `retiring` |
| POST | `/publish/takeover` | `{names}` → `{lines}`; the operator's yes: names in `conflict` are published on their site's door over what the zone had (`publish.takeover` event) |
| POST | `/publish/retry` | → `{ok}`; every name not published is tried on the next pass, manual and conflict ones included (a conflict that is still there comes back as one) |
| POST | `/sites/{domain}/publish` | `{door?, over?}` → `{lines, vulnerable?}`; every name of the site (its domain, aliases and the implicit `www.` of every apex among them) on that door, kept there from now on. `over` takes names that point elsewhere: only for a node moving the site here, or the site's own import token |
| POST | `/sites/{domain}/unpublish` | → `{lines}`; tunnel rules and the records that point at this house are removed, nothing else |

`POST /sites` publishes the new site on the door for new sites when a DNS
provider is on (`"publish": false` to skip it, `"door"` to choose); the
answer carries `publishing: true` and the outcome arrives as `publish.*`
events. `DELETE /sites/{domain}` unpublishes first; a name that cannot be
removed then stays `retiring` and is retried. The intent and what the house
wrote live in `data/publish.json`, so an alias added later is published and
one removed is unpublished without anyone asking. A site folder removed by
hand keeps its records (`publish.orphan`, said once): only a deletion
through the house takes names off the internet.

## WAF

| method | path | result |
| --- | --- | --- |
| GET | `/waf` | → `{global: policy, blocked: [{ip, until, reason, strikes}], recent: [{ip, hits, status}], attack: {global: rules, counts: {domain or "panel": {served, solved}}}, ban: {off, max_retry, find_minutes, ban_minutes, factor, max_ban_days}, watched: [{ip, score, first, offenses: [{at, reason, host, path}]}]}`; `policy.ban` sets the bans (defaults 5 attempts in 10 minutes, 60 minutes ×4 per repeat, at most 30 days) |
| PUT | `/waf` | global policy → policy (400 with the field when it does not validate) |
| POST | `/waf/block` | `{ip, duration?: "1h", reason?}` → `{ok}` (default one day; pushed to the kernel through the helper when it runs) |
| DELETE | `/waf/block/{ip}` | → `{ok}` |
| POST | `/waf/under-attack` | `{enabled, domain?, for?: "2h", bits?, life?, skip?, message?}` → `{ok}`; without `domain` the global policy, with it the site's manifest. Absent fields keep their value; `for` becomes `until` on this machine's clock and `"for": ""` is permanent (400 when the rules do not validate) |
| GET | `/waf/traffic` | → `{per_second, requests, bytes, by_domain: {domain: {requests, per_second, bytes, by_status, p50_ms, p95_ms, seconds: []}}, by_status: {…}, seconds: []}` (last minute; `seconds` oldest first) |

The routes answer 503 when the WAF did not start (a broken block list); the
`waf.error` event says what to fix.

## Security

The rules are the manifest's `security` (docs/manifest.md) and are written
through the manifest; these routes are what the panel reads beside them.

| method | path | result |
| --- | --- | --- |
| GET | `/security/catalog` | → `{families: [{key, name, note}], robots: [{name, family}], browsers: [{key, name, note}], builtin_paths: [], your_country}` |
| GET | `/sites/{domain}/security/blocks` | → `[{at, ip, reason, what, path, agent, country}]`, the last 50 refusals, newest first (kept in memory). In the `read` scope, so a site's turn reads it (`middleware site security <domain> blocks`) |

## Agents

| method | path | body / result |
| --- | --- | --- |
| GET | `/agents` | → `{providers: [{provider, available, where, models: []}], capacity}` (`where` is the binary's path, or why it cannot run) |
| GET | `/sites/{domain}/agents` | → `{providers}` as `/agents`, without the capacity: what a conversation of the site can use, from the node that holds it; a delegated user sees only the agents and models their limits leave (a turn with another is 403) |
| GET | `/agents/waiting` | → `{asking: [{name, domain, id?, title?, updated}], working, running: [{name, domain, id?, title?, updated}]}`: the conversations waiting for the operator's answer (the newest first), how many turns are at work, and which (the oldest first, `updated` is when the turn started). The panel's bar asked every node for it before `/agents/recent`, and still asks a node that does not know that one |
| GET | `/agents/recent` | `?limit=15` (up to 200) → `[{name, domain, id?, title?, updated, turns, running?, asking?}]`: the conversations last used on this node, of every site and of the house, the last first (`updated` is the last message, or when the turn at work started); beyond the limit, every one at work or waiting for an answer. A site's own conversations are left out unless at work, and so is one with no message. The panel's bar asks every node of the network for it; the operator's only |
| DELETE | `/agents/chats` | forgets every conversation the panel holds on this node, of every site and of the house, with their attachments → `{purged, busy}`; the site's code's own included; one at work is left and counted in `busy`. The bar's "delete every conversation" asks every node; the operator's only |
| GET | `/agents/capacity` | → `{used, max, reserved_for_house, providers: {claude: {used, max}}, running: [{domain, provider, started, turn_id}]}`; a provider's `max` of 0 means no cap of its own |
| GET | `/sites/{domain}/chat` | → `{domain, provider, model, session_id, running, live, turns: [{id, role, text, cost, at, tools, questions: [{question, options, recommended}]}]}`; while a turn runs, `live: [{seq, type, data}]` is what it showed so far, the same events the stream sent (whose `data.seq` matches), so a client that opens it late draws it where it is. A turn does not depend on any client and has no time limit; it ends on its own or with `/stop` |
| POST | `/sites/{domain}/chat` | `{text, model?, provider?}` → `{turn_id}`; `provider` runs this message with that agent instead of the manifest's (each agent keeps its own session in the conversation, and the one that comes in is told what was said while it was away); progress on `/events/stream` as `agent.*`; 409 while a turn runs in this conversation; 429 when the house, or the provider, has no free slot (the message says how many of how many; `agent.capacity` is emitted with the numbers) |
| POST | `/sites/{domain}/chat/stop` | → `{ok}`; 409 when nothing runs |
| DELETE | `/sites/{domain}/chat` | → `{ok}` (new conversation) |
| POST | `/sites/{domain}/chat/reap` | → `{killed}`; stops the turn and kills everything the conversation left running (setsid escapes included) |
| GET | `/sites/{domain}/chat/export` | → a JSON file to download (`Content-Disposition: attachment`): `{exported, version, conversation}`, the conversation with every turn, its steps and its subagents' |
| GET | `/sites/{domain}/chat/processes` | → `[{pid, command, started}]` what the conversation has running now |
| POST | `/sites/{domain}/chat/processes/{pid}/kill` | → `{ok}`; 404 for a pid that is not this conversation's |
| GET | `/sites/{domain}/chat/files` | → `[{name, bytes}]` attachments that go with the next message |
| POST | `/sites/{domain}/chat/files` | `?name=file.ext`, raw body (25 MB) → `{name, bytes}` |
| DELETE | `/sites/{domain}/chat/files/{file}` | → `{ok}` |
| GET | `/sites/{domain}/chats` | the site's other conversations, the last used first → `[{name, id, origin: panel|site, title, tools, created, updated, turns, hidden, running}]`; `?all=1` includes those `claude_api.hide_chats` hides, `?main=1` the first conversation too (empty `id`). A conversation is titled by its first message; a panel one without messages is not listed |
| POST | `/sites/{domain}/chats` | `{title?}` → `{name, id}` (201); another conversation of the panel's |
| DELETE | `/sites/{domain}/chats` | forgets every conversation the panel holds, the first included, with their attachments → `{purged}`; the site's code's own included (one of those at work is passed over); 409 while one of the panel's is at work |
| GET | `/sites/{domain}/chats/{id}` | as `/sites/{domain}/chat` |
| POST | `/sites/{domain}/chats/{id}` | as `/sites/{domain}/chat`; 409 for a conversation the site's code opened (read and stop it, do not write into it) |
| DELETE | `/sites/{domain}/chats/{id}` | → `{ok}` |
| POST | `/sites/{domain}/chats/{id}/stop` | as above |
| POST | `/sites/{domain}/chats/{id}/reap` | as above |
| GET | `/sites/{domain}/chats/{id}/export` | as above |
| GET | `/sites/{domain}/chats/{id}/processes` | as above |
| POST | `/sites/{domain}/chats/{id}/processes/{pid}/kill` | as above |
| GET | `/sites/{domain}/chats/{id}/files` | as above |
| POST | `/sites/{domain}/chats/{id}/files` | as above |
| DELETE | `/sites/{domain}/chats/{id}/files/{file}` | as above |
| GET | `/chat/middleware/chats` | as for a site, for the house's conversations |
| POST | `/chat/middleware/chats` | as for a site, for the house's conversations |
| DELETE | `/chat/middleware/chats` | as for a site, for the house's conversations |
| GET | `/chat/middleware/chats/{id}` | as for a site, for the house's conversations |
| POST | `/chat/middleware/chats/{id}` | as for a site, for the house's conversations |
| DELETE | `/chat/middleware/chats/{id}` | as for a site, for the house's conversations |
| POST | `/chat/middleware/chats/{id}/stop` | as for a site, for the house's conversations |
| POST | `/chat/middleware/chats/{id}/reap` | as for a site, for the house's conversations |
| GET | `/chat/middleware/chats/{id}/export` | as for a site, for the house's conversations |
| GET | `/chat/middleware/chats/{id}/processes` | as for a site, for the house's conversations |
| POST | `/chat/middleware/chats/{id}/processes/{pid}/kill` | as for a site, for the house's conversations |
| GET | `/chat/middleware/chats/{id}/files` | as for a site, for the house's conversations |
| POST | `/chat/middleware/chats/{id}/files` | as for a site, for the house's conversations |
| DELETE | `/chat/middleware/chats/{id}/files/{file}` | as for a site, for the house's conversations |
| GET | `/chat/middleware` | the conversation about the installation itself, same shapes; it works in the root of the installation and may create new sites, but never edits one that already existed when its turn began |
| POST | `/chat/middleware` | as above |
| DELETE | `/chat/middleware` | as above |
| POST | `/chat/middleware/stop` | as above |
| POST | `/chat/middleware/reap` | as above |
| GET | `/chat/middleware/export` | as above |
| GET | `/chat/middleware/processes` | as above |
| POST | `/chat/middleware/processes/{pid}/kill` | as above |
| GET | `/chat/middleware/files` | as above |
| POST | `/chat/middleware/files` | as above |
| DELETE | `/chat/middleware/files/{file}` | as above |
| GET | `/agents/usage` | each subscription's windows → `[{provider, name, module, supported, usage: {plan?, windows: [{name, label?, used, resets_at, status?}], at, source}, error?}]`: asked of the vendor (Anthropic's `/api/oauth/usage`, ChatGPT's `/backend-api/wham/usage`, Antigravity's `agy -p /usage`), cached a minute, `?refresh=1` asks now, `?provider=agy` only that agent's; `source: "turn"` is what the last turn's stream said. `used` is a percentage, negative when unknown; `label` is the vendor's name for a window the house has no word for. A figure that moves is also sent on the event stream as `agent.usage` (`subject` the provider, `data: {provider, usage}`) |
| GET | `/agents/cost` | `?days=30` → `{days, books: [{book, total_usd, turns, days: {date: usd}, models: {model: usd}}]}`; a site door's spending is the book `<domain>#door` |

A message sent while a turn runs is queued (202 `{queued: true}`) and sent,
with any other queued one, when the turn ends. A turn in flight when the
supervisor restarts is picked up by the next one. A subagent's words are
kept in the turn's `threads`, apart from the answer.

The `agent.*` events on the stream, all with the domain (or `middleware`) as
subject and `turn_id` in `data`: `agent.started` (`provider`, `model`),
`agent.text` (`text`, one complete piece as the agent said it), `agent.tool`
(`tool`, `input`), `agent.done` (`cost`, `session_id`, `text`, `tools`),
`agent.error` (level `error`; the turn ended badly), `agent.jail_violation`
(`paths` the agent referred to and must not have), `agent.cost_cap` (the
site's monthly cap refused the turn), `agent.capacity` (level `warn`; a
send was refused for lack of a slot: `used`, `max`, `provider`, `providers`)
and `agent.question` (`question`, `options`, `recommended`: the agent asked
for a decision).

**Questions.** An agent that needs a decision writes a fenced block in its
text:

```question
{"question": "Which database?", "options": ["SQLite", "Postgres"], "recommended": "SQLite"}
```

The block is parsed as the text arrives, emitted as `agent.question` and
kept on the turn as `questions`; the panel shows it as buttons (plus
"Something else…" for a typed answer; an empty `options` list means only
the text box) and the chosen text is sent as the next message. A block that
is not valid JSON is left in the text and ignored.

## Tasks

| method | path | body / result |
| --- | --- | --- |
| GET | `/tasks` | → `[{name, schedule, site, disabled, next, running, error, last: {at, status, duration, exit_code, trigger}}]` (a file that does not parse is a row with `error`) |
| PUT | `/tasks/{name}` | task JSON ([manifest.md](manifest.md#tasks-tasksnamejson)) → the task's row; 400 with the field when it does not validate |
| DELETE | `/tasks/{name}` | → `{ok}`; the history and the log stay |
| POST | `/tasks/{name}/stop` | → `{ok}`; ends a run in progress, recorded as `stopped`; 409 when none runs |
| POST | `/tasks/{name}/disable` | → the task's row; writes `disabled: true` to the file and nothing else, so it stays off across restarts; a run in progress ends on its own. Emits `task.disabled`. 404 when there is no such task, 400 when its file does not parse |
| POST | `/tasks/{name}/enable` | → the task's row; clears `disabled`, as above. Emits `task.enabled` |
| POST | `/tasks/{name}/run` | → `{ok, log}` at once; the run is on `/events/stream` as `task.*`, and `task.skipped` when it was already running |
| GET | `/tasks/{name}/log` | `?lines=500` → `text/plain`, the last lines of `data/logs/tasks/{name}.log` |

A site's own tasks are the same files seen from the site: the ones whose
`site` is `{domain}`. These are the routes the `tasks:<domain>` scope opens
and a site's agent turn reaches over the socket (`middleware site tasks`).
`{domain}` is a site or project here (404 otherwise). A task that is not
there and one that is not this site's are the same 404; names stay one space
for the whole house, so a name the root or another site holds is 409 to PUT.

| method | path | body / result |
| --- | --- | --- |
| GET | `/sites/{domain}/tasks` | → the rows of `GET /tasks` whose `site` is `{domain}` (`[]` when none); a task of the site that does not parse is a row with `error` |
| PUT | `/sites/{domain}/tasks/{name}` | task JSON, validated as `PUT /tasks/{name}`; an empty `site` becomes `{domain}` → the task's row. 400 when `site` names another site; 409 when `{name}` is a task of another site or of the root, which is left as it was. Emits `task.saved` with `site` and the caller as actor (`operator`, `agent` for a turn, `token:<name>` for a scoped token) |
| DELETE | `/sites/{domain}/tasks/{name}` | → `{ok}`; the history and the log stay |
| POST | `/sites/{domain}/tasks/{name}/run` | → `{ok, log}` at once, as `POST /tasks/{name}/run` |
| POST | `/sites/{domain}/tasks/{name}/stop` | → `{ok}`; 409 when none runs |
| POST | `/sites/{domain}/tasks/{name}/disable` | → the task's row, as `POST /tasks/{name}/disable`; `task.disabled` carries `site` |
| POST | `/sites/{domain}/tasks/{name}/enable` | → the task's row, as `POST /tasks/{name}/enable` |
| GET | `/sites/{domain}/tasks/{name}/log` | `?lines=500` → `text/plain`, as `GET /tasks/{name}/log` |

A site's database copies. What is copied, when and how many are kept is
the manifest's `backups` (docs/manifest.md), written as any part of it;
these routes are what is done with it. They are what the `backups:<domain>`
scope opens and a site's agent turn reaches over the socket (`middleware
site backups`). A copy and a restore answer at once and run on their own,
with `backup.*` events (`backup.started`, `backup.done`, `backup.failed`,
`backup.restoring`, `backup.restored`, `backup.restore_failed`,
`backup.deleted`), unless the body says `wait`. 409 while a copy or a restore of the same backup runs.

| method | path | body / result |
| --- | --- | --- |
| GET | `/sites/{domain}/backups` | → `[{name, config, next?, running?, last?, last_restore?, copies: [{id, at, bytes, kind}], bytes, error?}]`: each backup of the manifest (`config` is its entry), newest copy first, `last` and `last_restore` `{at, trigger, ok, error?, seconds, bytes?, copy?}`. A folder of a backup no longer in the manifest that still holds copies is listed with `config: null` and `error` |
| POST | `/sites/{domain}/backups/{name}/run` | `{wait?}` → 202 `{started}`; with `wait`, the copy `{id, at, bytes, kind}` once written, 422 with the client's words when it failed |
| GET | `/sites/{domain}/backups/{name}/log` | `?lines=200` → `text/plain`, the end of `data/logs/{domain}/backup-{name}.log` |
| GET | `/sites/{domain}/backups/{name}/copies/{id}` | the copy as kept, `application/gzip`, as an attachment |
| DELETE | `/sites/{domain}/backups/{name}/copies/{id}` | → `{ok}` |
| POST | `/sites/{domain}/backups/{name}/copies/{id}/restore` | `{wait?, no_safety_copy?}` → 202 `{started}`; with `wait`, `{at, trigger, ok, seconds, copy}`, 422 when it failed. A copy of the database as it is now (`pre-restore`) is taken first and nothing is restored when it fails, unless `no_safety_copy`. 400 for a `command` backup without `restore` |

## Nodes

Installations joined in one network: a WireGuard mesh (interface
`mwmesh`, UDP 51821, addresses drawn from `10.66.0.0/16`) in which every
node sees and manages every other. Joining one node is joining all: each
node reads the others' rosters every twenty seconds. A node not known yet
is learnt from whoever names it; once known, its entry changes only when
read from that node itself, dated at most a minute ahead of the reader's
clock. A removal is an entry marked removed, taken from any member, and is
final: the node comes back only by joining again through an invitation.
An entry whose address is outside the mesh's subnet or another node's is
dropped. The nodes' API listens only on
the mesh address (TCP 46101), the helper closes the interface to any other
port, and a request there is accepted only from a member's tunnel address
with the network's secret. The panel works on another node through
`/nodes/{id}/api/…`, which forwards to that node's `/api/v1/…` through the
tunnel, streams included. A request from another node opens everything an
operator's does except `/nodes/…` and `/auth/…` (but `/auth/me`): the
network and the credentials are decided at each machine's own panel.

| method | path | body / result |
| --- | --- | --- |
| GET | `/nodes` | → `{self: {id, name, ip, public_key, endpoint, version}, network, port, api_port, up, error, invites, suggested_endpoint, settings: {panel_vpn_only, updated}, members: [{id, name, ip, endpoint, version, updated, handshake, online, last_read}]}` |
| PUT | `/nodes/self` | `{name}` → `{id, name}` |
| PUT | `/nodes/settings` | `{panel_vpn_only}` → the network's settings, for every node within a round. Closing is refused (409) from a node without its VPN on and a device in it |
| POST | `/nodes/invite` | `{endpoint}` (this node's public `ip:port`, needed only when the invitation creates the network) → `{code, expires}`; the code carries a key pair made for the newcomer, which replaces it on joining; single use, fifteen minutes. 503 without the helper |
| POST | `/nodes/join` | `{code, endpoint}` (this node's public `ip:port`; empty behind NAT) → `{ok, inviter, members}`; the first contact goes through the tunnel, retried for forty seconds. 409 when already in a network, 502 naming the cause when the inviter does not answer |
| POST | `/system/panel/network-only` | → `{ok, was, manage_from}`: the panel leaves its domain (its record and tunnel route retired) and the node is managed from the network alone; under `/system` so another node's panel can ask it through the mesh. 409 outside a network, with no other node answering, or with no domain |
| POST | `/nodes/leave` | → `{ok}`; tells the others, takes the interface down |
| DELETE | `/nodes/{id}` | → `{ok, name}`; removes the node for every node, and tells it |
| GET | `/nodes/overview` | → `[{id, name, ip, version, self, ok, error, latency_ms, summary}]`, this node first, every member asked in parallel (8 s each) |
| GET | `/node/summary` | → `{id, name, version, started, hostname, sites: {sites, projects, serving, stopped, trouble: [{domain, why}]}, events (last warnings and errors), vitals: {now, minutes: [[unix, cpu, mem]], open_episode}, chats: {asking, working}, update}` |
| GET | `/nodes/{id}/api/{path...}` | the member's `/api/v1/{path...}` through the mesh, streams included. Its 401 comes back as a 502: not being recognised over there is not this panel's session ending |
| POST | `/nodes/{id}/api/{path...}` | the same, for every method |
| PUT | `/nodes/{id}/api/{path...}` | 〃 |
| PATCH | `/nodes/{id}/api/{path...}` | 〃 |
| DELETE | `/nodes/{id}/api/{path...}` | 〃 |

On the mesh door only (not routes of this API): `POST /api/v1/mesh/join`
(the newcomer, from the invitation's address, with its secret),
`GET /api/v1/mesh/roster` (members and settings), `GET /api/v1/mesh/names`
(what the node answers for), `POST /api/v1/mesh/leave` and
`POST /api/v1/mesh/expelled`.

With `panel_vpn_only` on, no panel of the network answers the internet:
each answers through a VPN and through the mesh, and a node with no VPN
of its own is managed from the others. A device on any node's VPN reaches
every node: that node's resolver answers the others' names with its own
VPN address, and its VPN door passes a TLS connection for one of them,
undecrypted, to the node that serves it (TCP 46102 on the mesh address,
after a line with the network's secret and the device's name), which
serves it as one that came through its own VPN. `middleware nodes panel
open` on any machine reopens every panel.


### Moving a site to another node

A site moves with its folder, its HOME, its conversations, its cost, its
history and its deploy record. A first copy goes while it serves; it is
then stopped and a second copy carries only the files changed since the
first, with the index of every name, so what was deleted here is deleted
there: the stop lasts that second copy and the start on the other side.
The target keeps what arrives apart (`data/relocate/<site>`) until the
commit, and refuses a name it already has. A target that does not answer
healthy in three minutes is removed there and the site started here again
as it was. Only once it answers there are its names published on that
node over the records that pointed here (`over`, which only a node may
ask), and only then does this node take its own route down. The copy here
is archived, not deleted (`data/archive/`), so going back is a restore;
its folder leaves `sites/` before it is packed (as `data/archive/<site>-<time>.packing`),
so nothing of it starts here again meanwhile, and a pack that fails puts it
back stopped.
A move is given in all what its steps may take by the site's manifest
(each `move` command's `timeout`, the install there, the wait for the
site) and an hour for the copies; once the site answers there, pointing
its names and archiving the copy here have their own time.
Each step is a `relocate.*` event (`copy`, `stop`, `final`, `commit`,
`start`, `dns`, `cleanup`, `done`, or `failed` with the step it failed at).
What does not go: its logs (this machine's), the tasks under `tasks/` that
name it, and what a build left that only runs on this machine's processor
(a site moved between amd64 and arm64 needs its build run again there).
A target that cannot publish every name the site has published here (no
DNS provider for their zone there), or a site in a container and a target
without a docker that answers, is refused before anything moves.

From just before the stop until it ends, a move keeps a journal in
`data/relocate/.moving/<site>.json`: the manifest the site had, the tasks
it turned off, the target and how far it got. A restart of the house in
the middle (an automatic update, a crash) is put right when it starts
again, as a `recover` step of the same move: before the commit the site
starts here again as it was, its tasks on, and the target throws away what
it received; after it the target is asked whether the site answers there,
and the move is finished (names, archive) or undone. A target that does
not answer is asked again for an hour, the site staying stopped here
(starting it without the target's word could run it on both nodes), and
then the journal waits for the next start.

| method | path | body / result |
| --- | --- | --- |
| POST | `/relocate/import-token` | the operator only: `{domain, base?}` → `{token, expires, domain, command}`; `base` is the address the V1 machine reaches this node at, by default this node's panel domain (400 when it has none: a node opened through another node's panel is reached through the mesh, which the V1 machine is not in; import into a node that has a domain and move the site afterwards); a token for importing that one site from a V1 house, valid two hours, and the command to run on the V1 machine (`middleware import`, downloaded at this house's version). The import packs the folder without `node_modules` and the site's HOME, stops the site in V1 (writing `disabled` into its V1 manifest), converts the manifest, commits it here with `install: true` (dependencies installed for this machine, with the Node the site asks for), and leaves V1 stopped; if it does not come up here, V1 is started again. Once it answers here its names are published on this node's door for new sites over what pointed at the old server (`POST /sites/{domain}/publish` with `over`, which the import token may ask for its own site), as a move between nodes does; without a DNS provider they stay `manual` |
| POST | `/sites/{domain}/containerize` | `{provider?, model?}` → the job `{step, how?, error?, chat?, done, started}`; 409 when docker does not answer or the site already runs in one. The container is worked out (see [manifest.md](manifest.md#docker)), tried beside the site, and written only if it answered; when it cannot be, the agent named is asked in a new conversation (`chat`, its id), and without one the job ends `failed` with why. Events `containerize.trying`, `.done`, `.asked`, `.failed` |
| GET | `/sites/{domain}/containerize` | → the job as it is now |
| POST | `/sites/{domain}/move` | `{node}` → the move (as `GET /sites/{domain}/move` says it); it goes on in the background, each step an event `relocate.<step>` |
| GET | `/sites/{domain}/move/plan` | `?node=` → `{analysis: {bytes, files, installs: [{dir, command, lockfile}], node?, docker?, names?, outside_links?: [{path, target}], warnings}, node_pinned?, node?, check_error?, exports?}`: what moving the site would do, before it does it. Dependencies are found as `node_modules` beside a `package.json` in any folder; links that leave the site's folder are listed (they point at whatever is there on the target). `check_error` is the target saying it cannot take it, or an export that waits for the stop whose program is not where it runs; `exports` says of each such export whether it has worked here before or will be tried, at the move, while the site still serves (see the manifest's workers) |
| GET | `/sites/{domain}/move` | → the site's last move from this node: `{id, domain, node, step, done, started, ended?, error?, steps: [{step, message, state, started, ended?, error?}], report?: {archive, down, first_bytes, last_bytes, dns}}`; `state` is `running`, `ok`, `failed` or `warn` (the names did not follow), and a failure's way back is its own step, `restore` |
| GET | `/moves` | → every move from this node kept on disk (the last 50), the newest first; `?domain=` one site's. A move a restart cut is put right when the house starts again (its journal, a `recover` step); one without a journal is said as failed at the step it was in |
| POST | `/relocate/{domain}/check` | the receiving node, before anything moves: `{names, docker, tasks, volumes, ports}` (the names published where the site is; whether it runs in a container; its task files; the docker volumes its move fills here; its workers' written ports) → `{ok}`, or 409 when the name is a site here, this node cannot publish every one of those names, or the site runs in a container and this node has no docker that answers, a task name or one of those volumes is already here, or one of those ports is taken |
| PUT | `/relocate/{domain}` | the receiving node: a site's bundle (`.tar.gz`) as the raw body → `{files, dirs, links, bytes}`; 409 when the name is a site here |
| PUT | `/relocate/{domain}/delta` | the last copy: changed files and the index, laid over what was received |
| POST | `/relocate/{domain}/prepare` | the network, or an import token for that site: `{dirs?}` (the site's folders whose `node_modules` the source left behind, relative to it; none is its root): start installing a received site's dependencies in each, in its staging, for this machine (`npm ci --include=dev`, or pnpm or yarn by the lockfile, dev dependencies included, with the Node the site gets here) → `{started, id}` (`{started: false, running: true, id}` while an earlier one runs, which the moving node does not wait on). It runs on its own, not inside the request (a connection between nodes, or Cloudflare's 100 seconds, would cut it); a move calls it after the first copy, while the site still serves where it was, for a Node site on the machine, whose `node_modules` is left behind |
| GET | `/relocate/{domain}/prepare` | → `{id, done, output?, error?}`: what the install said, or why it failed; a process killed says by what (its 30 minutes, the kernel for lack of memory with the machine's memory, or another SIGKILL) |
| POST | `/relocate/{domain}/migrate` | the network, or an import token for that site: `{manifest, workers?}` (manifest base64, the one it will be committed with; workers the ones to import now, all of them when none: a move takes the live ones while the site serves): start, before the commit, bringing up on its own each worker whose `move` says its data moves by command, waiting for its port and its `move.ready`, running its `move.import` (inside its container for a docker worker) and stopping it → `{started, id}`. The named volumes it creates are recorded, and removed if the move is undone; one already here refuses it. It runs on its own, as `prepare` |
| GET | `/relocate/{domain}/migrate` | → `{id, done, output?, error?}` (404 once this node restarted: a job lives in its memory, and the moving node takes a 404, or another `id` than the one it started, as the job failed): what each import did, or why it failed (the commands' output is in `data/logs/<domain>/move-<worker>.log`) |
| POST | `/relocate/{domain}/repair` | the network, or the site's import token: `{why}` → `{started}`; a site that arrived and does not start is handed to this node's AI for automatic work (`settings.automation`), one turn in its conversation with why and its log, then restarted; the moving node waits for it (10 minutes at most) before undoing the move. 409 when no AI for automatic work is configured |
| GET | `/relocate/{domain}/repair` | → `{done, output?, error?}`: the AI's closing line ("Done: …"), or why it failed |
| POST | `/relocate/{domain}/placed` | the network, or an import token for that site: the move into this node ended well; the record of what to take out if it had not is dropped |
| GET | `/relocate/leftovers` | → `[{path, kind, domain, bytes, modified}]`: what moves left on this node, the largest first; `kind` is `staging` (a reception still for an hour), `aside` (an earlier copy a commit set aside, `.before-…`), `placed` (a site placed here that did not come up and was not taken out), `record` (a site that came up whose move never said it ended) or `volume` (a docker volume of a site that moved away, whose data may be only there; `path` is `docker-volume:<name>`, `bytes` 0) |
| POST | `/relocate/leftovers/clean` | the operator only: `{paths}` → `{results: {path: "removed" or why not}}`; only what `GET` lists now is touched, and a `placed` site is taken out as a failed move would (what arrived removed, what was there before put back), a `volume` removed from docker |
| POST | `/relocate/{domain}/commit` | `{manifest, install?}` (base64, the one the site had before it stopped; `install` installs a Node site's dependencies in the staging folder first, `npm ci` with a lockfile) → `{ok}`; the site is put in place and the engine starts it |
| DELETE | `/relocate/{domain}` | throw away what was received for a site that did not finish moving in: the staging and, once it was put in place and its folder removed, what it put under `data/` (HOME, conversations, history), with what was there before under that name put back; refused while the site's folder is here |

## Cluster

Replicas of a site on other nodes, handed requests when its node is
saturated (the manifest's `cluster`; see [cluster.md](cluster.md)). The
`/cluster/…` routes below the first two are what nodes call on each other
and refuse anyone else, except the operator's removal.

| method | path | body / result |
| --- | --- | --- |
| GET | `/sites/{domain}/limits` | → `{applied, machine: {cpus, memory}, limits, defaults, use?: {memory, peak, oom_kills, cpu_usec}}`: what the panel's «Recursos» tab draws; `applied` is false on a node that does not split its cgroup (the limits wait in the manifest), `use` is the site's group now (`cpu_usec` grows; two reads give a rate). `limits` is changed with PATCH `/manifest` and only by the owner: a delegate's change to it is refused |
| GET | `/sites/{domain}/cluster` | → `{domain, in_network, clustered, blockers, cluster, want, spill_at, gateway, revision, load: {per_core, cores}, sharing, replicas: [{node, node_id, state, alive, load, error, synced, refused}], replica}`; owner only |
| GET | `/cluster` | → `{load, kept: [{domain, primary, of, revision, alive, state, pending, error, synced}]}` |
| POST | `/cluster/status` | `{expect}` → `{load, replicas}`; nodes only. Replicas of the caller not in `expect` are removed after two minutes' grace |
| PUT | `/cluster/replicas/{domain}` | the site's folder as a `.tar.gz` (raw body); nodes only. 409 when the name is a site of this node or another node's replica |
| POST | `/cluster/replicas/{domain}/commit` | `{manifest (base64), revision, primary, dirs}` → `{started}`; nodes only. Installs dependencies, then swaps the copy in |
| DELETE | `/cluster/replicas/{domain}` | → `{ok}`; its primary or the operator |

## VPN

| method | path | result |
| --- | --- | --- |
| GET | `/modules/vpn` | → `{enabled, up, interface, address, endpoint, listen_port, public_key, vpn_only, helper, peers: [{name, address, public_key, created, last_handshake, online, endpoint, rx, tx}], error?}` |
| POST | `/modules/vpn/peers` | `{name}` → 201 `{name, config}` (a client config, shown once, never stored) |
| DELETE | `/modules/vpn/peers/{name}` | → `{ok}` |
| POST | `/modules/vpn/only` | `{enabled}` → `{ok, enabled}`; panel and service door reachable only through the VPN. Refused while the VPN is down or has no peer |

The module is enabled like any other (`POST /modules/vpn/enable` with
`endpoint`, and optionally `listen_port` and `subnet`); its routes answer
404 until then. It needs the privileged helper: `wg` and `nft` are its
business, never this process's.

## Guard

| method | path | result |
| --- | --- | --- |
| GET | `/guard` | `?all=1` → `{rules: [{key, level, description}], incidents: [{id, at, domain, rule, level, where, action, pid, cmdline}]}`; dismissed incidents are left out unless `all=1` |
| POST | `/guard/incidents/{id}/dismiss` | → `{ok}` |
| GET | `/sites/{domain}/resources` | `?worker=name` for one worker alone → `{samples: [{at, cpu, rss}], episodes: [{kind: cpu|memory, start, end?, peak}], workers?: {name: {at, cpu, rss}}}`; CPU in percent of one core and resident bytes, summed over every process of the site and of its workers (a container's found by its id in their cgroup, whatever their working directory), a sample a minute for a day; `workers` is each worker's last sample. An episode is five minutes or more over 90% of a core or 2 GB, and is a `resources.episode` event when it starts and `resources.episode_end` when it ends |

The rules are listed from the code that applies them: there is no way to
turn one off through the API, which is the point of a watchdog.

## The site door

The site's own code talks to an AI through the house (the panel calls it
the AI gateway): the agent its manifest names (`claude_api.provider`,
Claude by default, any enabled agent otherwise), same engine, same jail,
same bill, visible in the panel. Open it from the site's page in the
panel (or with `claude_api` in the manifest); the site's process then gets
`MIDDLEWARE_TOKEN` (its token: it names the site and opens only this door)
and `MIDDLEWARE_API`, the house's bare address (`http://127.0.0.1:46100`),
and so do its tasks, each run, with the same token and the same day's
spend ([manifest.md](manifest.md#tasks-tasksnamejson)). A
docker site gets `MIDDLEWARE_API_SOCKET` too, a unix socket mounted at its
own path: inside a container 127.0.0.1 is the container's. Through that
socket only the site door answers. Opening or closing the door restarts the
site.

The contract is V1's, path for path and field for field: the routes answer
at `$MIDDLEWARE_API/api/site/…` as in V1, and at `/api/v1/site/…` too, so a
site written for V1 moves without a line changed.

| method | path | result |
| --- | --- | --- |
| GET | `/site` | → `{domain, claude: {available, provider, model, models, tools, limits: {provider, tools, permissions, daily_usd, daily_turns, max_chats, model, models, hide_chats}, spent_usd, left_usd, turns, chats_open, busy}}`; the `claude` key keeps its V1 name whatever agent answers |
| POST | `/site/claude/ask` | `{prompt (or text), system?, model?, title?, format?: "json", schema?, wait?, files?: [paths relative to the site's folder: regular files only, a link that leaves the folder is refused with 400]}` → `{chat, state: "done", text, model, turns, cost_usd, json?, json_error?, question?, error?}`; one question, one call. Still running when `wait` runs out: 202 `{chat, state: "running", why}` |
| GET | `/site/claude/chats` | → `{chats: [{id, domain, title, model, created, updated, turns, cost_usd, busy, messages, origin, system, tools, asking}]}` |
| POST | `/site/claude/chats` | `{title?, system?, model?, format?, schema?, prompt?, files?}` → `{chat: {…}}`, or the answer when a prompt was sent |
| GET | `/site/claude/chats/{id}` | → `{chat, state, text, messages: [{seq, at, role: user\|assistant\|tool\|result, text, tool: {id, name, input, result, error, done, run, background}, agent, cost_usd, error}], more}`; `?limit=150` (the last ones) |
| POST | `/site/claude/chats/{id}/messages` | `{prompt, model?, format?, schema?, wait?, files?}` → as ask (409 `{error, chat}` while a turn runs) |
| GET | `/site/claude/chats/{id}/stream` | server-sent events: `event: message` with each message as it happens, `event: session` with the conversation after each turn |
| POST | `/site/claude/chats/{id}/files` | a file for the next message: multipart (field `file`) or the raw body with `?name=`; 25 MB → `{ok, file: {name, size, kind, at}}` |
| DELETE | `/site/claude/chats/{id}/files/{name}` | drop it before it is sent → `{ok}` |
| POST | `/site/claude/chats/{id}/stop` | stop the turn → `{ok}` |
| POST | `/site/claude/chats/{id}/reap` | stop it and kill what it left running → `{ok, killed}` |
| DELETE | `/site/claude/chats/{id}` | delete it → `{ok}` |

A site's own conversations push through the house (see the github module in
[modules.md](modules.md)). git calls these, not a person: `Authorization:
Bearer <push token>` (the turn's, never the door's), the repository must be
the manifest's `deploy.github.repository` or one of its `repositories`, and
the GitHub module's token is added on the way out. Refused: 403 and a `git.refused` event.

| method | path | result |
| --- | --- | --- |
| GET | `/site/git/{owner}/{repo}/info/refs` | `?service=git-upload-pack\|git-receive-pack` → GitHub's ref advertisement |
| POST | `/site/git/{owner}/{repo}/git-upload-pack` | a fetch, passed through |
| POST | `/site/git/{owner}/{repo}/git-receive-pack` | a push, passed through unless it deletes a ref; a `git.pushed` event with the refs |

The panel's side:

| method | path | result |
| --- | --- | --- |
| GET | `/sites/{domain}/claude-api` | → `{enabled, agents: [{provider, available, models, permissions}], models, defaults, limits?, token?, chats?, spent_usd, turns, use?, api, socket, process}`; `agents` are the ones that can run now (and the door's own), each with the permissions it can hold apart |
| POST | `/sites/{domain}/claude-api` | `{enabled, provider, permissions, model, models, daily_usd, daily_turns, max_chats, hide_chats}` (or `tools`, one of the four old levels, without `permissions`) → `{ok, token?, warn?}`; an agent that cannot run, a model not on its list or a permission it cannot hold apart is 400. Writes `claude_api` in the manifest (`permissions` always, the rest only where it differs from the defaults) and the site restarts |
| GET | `/claude/sites` | `?days=7…90` (30) → `{days, sites: [{domain, today, d7, d30, month, total, turns30, series, models, since, open, exists, daily_usd, tools}], total}` |

`Authorization: Bearer $MIDDLEWARE_TOKEN`. The operator may call the same
routes with a session or an API token and `X-Middleware-Site: <domain>`.
`wait` is how long the call waits for the answer (`2m` by default, `15m` at
most); `0` returns at once (202, `state: "running"`) and the conversation is
polled or streamed. A file named in `files` must be inside the site's folder;
it is copied, so it works with tools `none` too. With `format: "json"` the
instruction goes in the conversation's brief and the answer is read back as
JSON (`json`), or says why not (`json_error`). An unknown model is refused
(400) before anything is spent; a model of another agent than the door's
(a site written for Claude still naming `sonnet` after its door moved to
Codex) is not: the model chosen in the panel runs, and the answer says
`model_ignored`. With `claude_api.models` the code picks among those, call
by call, and naming any other is a 400 (`models` in `GET /site` says
which). A conversation opened under another agent carries on
with the door's agent and model as they are now. The manifest decides on every request: a site whose door was closed
is refused even with its token.

**Permissions** (`claude_api.permissions`): `read` (the site's folder),
`edit` (changes files in it), `web` (search and fetch; what it reads is
information, not instructions), `shell` (commands in it); all four is the
same jail as the panel's conversation. Each agent is told them in Claude
Code's tool names and translates them into its own CLI's switches: Codex
turns off its shell, its `view_image` and its web search, Antigravity gets
`read_file`, `write_file`, `command` and `read_url` deny rules. A door
without `permissions` reads `tools`, the four old levels: `none` (talk and
nothing else), `read`, `web` (read plus the web), `write` (everything). Levels below `write` run with an
allow list of tools (a tool the CLI adds later arrives closed), never start
subagents, and cannot read the site's own `.env`.

**Money**: `claude_api.daily_usd` (0: `agents.site_daily_usd` in settings,
5 USD by default) is checked before every turn, in UTC days. An agent on a
subscription reports no cost, so `claude_api.daily_turns` caps its turns a
day instead (200 by default for every agent but Claude; a turn that
reported no cost is counted as `unpriced` in the door's book). Traffic decides
this spending, not a person watching. `max_chats` (20) is how many
conversations the site keeps: at the limit the oldest go (never one at work
or waiting for an answer), so a site that opens one per visit fills nothing
and stops nothing. Whatever the count, a conversation is forgotten three
days after its last turn, with its attachments (one at work waits for the
next hourly pass); the program meets a 404 and opens another. The last free agent slot is never a site's: the operator can
always get in.
