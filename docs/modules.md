# Modules

A module is something not every installation wants. Each one declares what
it needs (a token, a binary, a port), how to **verify** that before saving
anything, and what it **installs**. Enabling a module runs the verification
first and prints one line per check: `ok`, `skip` (cannot be probed without
a side effect; it says so) or `fail`. Nothing is saved on a `fail`.

Credentials are encrypted at rest (AES-256-GCM) in `data/modules.json`; the
key is `MIDDLEWARE_SECRET_KEY` or `data/modules-key` (owner-only, generated
on first use). Disabling a module keeps its credentials; "forget
credentials" deletes them.

Binaries a module installs go under `modules/` inside the root, never into
the system, so uninstalling middleware leaves no trace outside the root and
the system package manager never fights with them. The agent CLIs are kept
current on their own: once a day the installation asks npm for the latest
release and, unless "Install newer releases automatically" is off in
Settings, installs it (`middleware deps status|check|install`, `GET
/api/v1/deps`). The container image ships them already; the daily check
replaces the shipped copy in the volume when a newer one exists.

```sh
middleware modules list
middleware modules verify cloudflare      # same checks, saves nothing
middleware modules enable cloudflare      # asks for what it needs
```

### Propagating to other nodes

In a network, a module set up on one node can be carried to the others
from its card: **Propagate to nodes**, every node ticked by default. The
credentials travel only through the mesh (WireGuard, with the network's
secret), and each node takes them through its own `enable`: it verifies
them, installs what it needs for its own architecture, enables the module
and seals them with its own key. A node that refuses them keeps nothing
and says why on the card. What belongs to the machine stays behind and the
far node keeps its own: Cloudflare's tunnel id, an agent's binary path,
and the whole VPN (its keys and port are this server's), which is never
propagated. Values empty here are not sent, so they do not clear one set
there. Owner only; `POST /api/v1/modules/{key}/propagate`.

## inbound-tunnel

A reverse tunnel pushes traffic to the loopback listener. No public IP, no
certificates on this machine.

- **Needs**: nothing by itself; with the `cloudflare` module it creates the
  tunnel and its routes for you. Any other tunnel (an SSH `-R`, a tunnel from
  another provider) works if it delivers to `127.0.0.1:46100` with the
  `Host` header intact.
- **Verify**: the loopback listener answers; with cloudflare, the token can
  create and list tunnels.
- **Installs**: `cloudflared` under `modules/` when the cloudflare module is
  on. The supervisor runs it itself while the module is on and has a
  tunnel (no unit: a container has no systemd), restarts it with backoff,
  re-adopts it after a restart of its own like a site, and asks its
  `/ready` on `127.0.0.1:46101`. Its log is `data/logs/cloudflared.log`; its
  state, `GET /api/v1/modules/cloudflare/connector`.

## inbound-ports

The process listens on 80 and 443 and gets certificates itself (ACME, RFC
8555, written on the standard library). Only domains this installation
serves get certificates; the panel domain included.

- **Needs**: 80 and 443 reachable from the internet, DNS pointing here. No
  root: the unit grants `CAP_NET_BIND_SERVICE`.
- **Verify**: both ports can be bound (or are already ours); an HTTP-01
  challenge round trip against the ACME directory's staging endpoint
  (`skip` if the DNS does not point here yet, with the reason).
- **Installs**: nothing. Certificates go to `data/certs/`.

Moving domains between doors happens from the Inbound screen or with
`POST /api/v1/inbound/move`; the certificate is obtained before DNS is
touched, so a domain is never moved to a door that cannot answer it. With
the cloudflare module on, the certificate comes through dns-01 for the zones
it manages, so a brand new name gets one before its DNS points here.
The client address behind Cloudflare's proxy is believed from Cloudflare's
own ranges (built in; `data/edge/cloudflare-ips.txt` replaces them).

## cloudflare

DNS records and tunnel routes for the domains you serve.

- **Needs**: an API token. Create it at Cloudflare → My Profile → API Tokens
  → Create Token → "Create Custom Token" with exactly:
  - `Zone` · `DNS` · `Edit`
  - `Zone` · `Zone` · `Read`
  - `Account` · `Cloudflare Tunnel` · `Edit` (only for `inbound-tunnel`)
  - Zone Resources: the zones you will serve (not "All zones" unless you
    mean it).
- **Verify**: `GET /user/tokens/verify` says the token is active; each
  permission is probed by listing (zones, DNS records, tunnels). Writing is
  not probed (`skip`: "creating a record to test would leave one behind").
- **Installs**: nothing by itself.

## github

Deploy on push: a webhook calls the API, which pulls, builds and writes the
pointer ([deploy.md](deploy.md)).

- **Needs**: a webhook secret (generated for you), and for private
  repositories a way to pull: a deploy key the module generates per site
  (`site.json` → `deploy.github`), shown once so you can paste it under the
  repository's Settings → Deploy keys.
- **Webhook**: repository → Settings → Webhooks → Add webhook. Payload URL
  `https://<panel domain>/api/v1/modules/github/hook`, content type
  `application/json`, secret as shown by the module, event "Just the push
  event". The module answers 200 with what it did, or 202 "not this
  branch".
- **Verify**: the secret is set; for each site with `deploy.github`, `git
  ls-remote` works with its key (`skip` with the key to paste when it does
  not yet).
- **Installs**: nothing; `git` is a system package the installer put there.
- **Sites from a repository**: with the module on, a new site can be made
  from a repository (the panel's "New site" → Code → A GitHub repository).
  `GET repos` lists the ones the token can see (none without a token: a
  public one is still given by name or address); `GET repos/{owner}/{name}`
  says what its root holds and what site it would make: Node (`npm ci` or
  `npm install`, `npm run build` when there is one, `npm start`), a built
  front end (a `build` script and no `start`: served from `dist`, or
  `build` for Create React App), Go (`go run .`) or files (the root, or
  `public`). The site gets `deploy.github`, so every push deploys it.
- **Pushing from a conversation**: with a token that has Contents: Read and
  write, a site's own conversation (the panel's, not the ones its code
  opens) can `git push` to `deploy.github.repository`, and to each
  repository the manifest lists under `repositories`, as it is. The turn
  never holds the module's token: it carries a push token only the house
  accepts, and `GIT_CONFIG_*` variables rewrite those repositories' GitHub
  addresses to `/api/v1/site/git/<owner>/<name>`, where the house checks
  the repository against the manifest and adds the credential. Any other
  repository and any branch or tag deletion are refused (`git.refused` in
  the events); a push that goes through is `git.pushed`. The same
  repositories are the ones «keep up to date» fetches with the token.

## openspec

Spec-driven development with [OpenSpec](https://github.com/Fission-AI/OpenSpec):
before a change that alters behaviour is built, the site's agent writes
what and why (`openspec/changes/<id>/`: proposal, design, tasks, spec
deltas) and a person approves it; once built and checked it is archived
and becomes the project's specs (`openspec/specs/`).

- **Needs**: nothing.
- **Installs**: the `openspec` CLI (`@fission-ai/openspec`) under
  `modules/node`, with the Node it needs, kept current like the agents'.
- **New sites**: every site created while the module is on (panel, CLI,
  the house's conversation, Mask: all of them end in `site.created`) is set
  up with `openspec init`, which writes `openspec/` and OpenSpec's own skills
  for every agent the house can run (`.claude/skills`, `.agents/skills`…).
  Not a site that serves its whole folder: `openspec/` would be public.
  An existing site is set up from its tab, on request.
- **The Specs tab**: each change with its tasks ticked, to read and to
  approve or archive (a message into the site's conversation: the agent
  does the work where it can be read and stopped), and the specs in force.
  It reads the folder without the CLI, so it keeps working with the module
  off.
- **Telemetry**: off. `OPENSPEC_TELEMETRY=0` on every turn and on init.
- **The agents**: the house's guide (§ 11) says when a change goes through
  a proposal and when it does not, and never to build before a yes.

## agent-claude

Conversations in the panel run on Anthropic's Claude through Claude Code,
the official CLI, driven as a subprocess. No SDK in the core.

**Sign in with your subscription.** The card has a "Sign in with your
subscription" button. It runs `claude setup-token` on a terminal of the
server's, with a temporary HOME so a failed attempt touches nothing, shows
you the authorization link to open on any device, and takes the code the
browser gives back. The long-lived token that comes out is stored encrypted
with the module's other credentials and handed to every turn as
`CLAUDE_CODE_OAUTH_TOKEN`; the module is enabled at the same time. The
panel shows "subscription since <date>" from then on. Signing in needs no
SSH and no browser on the server. An API key is the alternative: typed in
the card, passed as `ANTHROPIC_API_KEY`, and it wins over a stored session
(that is the CLI's rule).

- **Needs**: all optional. `model` (the alias offered first in the panel),
  `api_key`, `bin` (an executable to use instead of the managed one).
  The session (`oauth_token`, `oauth_since`) is written by the sign-in,
  not typed.
- **Verify**: the binary answers `claude --version`, or is about to be
  installed. Which credential a turn would use is said (`skip`), never
  probed: a probe would spend, and the first turn says if it is wrong.
- **Installs**: Claude Code itself, under `modules/node`, when no `bin` is
  given and none is found: `npm install -g` with a Node of our own if the
  machine has none that is new enough (fetched from nodejs.org and
  verified against the published checksum). Kept current daily from then
  on. Signing in before enabling installs it too.
- **Enable** replaces the provider the conversations run with; **disable**
  puts the CLI's own login back. Enabled modules are re-applied when the
  service starts.

Without the module the conversations still run: the `claude` CLI is looked
up in PATH and uses its own login under each site's home.

## agent-agy

Same shape, Google's Antigravity CLI (`agy`), which took the Gemini CLI's
place when Google stopped serving that one to individual accounts. The
binary is not on npm: it is downloaded from the manifest Google's own
`install.sh` reads, checked against the sha512 the manifest gives, and
kept current by the daily check; turns run it with
`AGY_CLI_DISABLE_AUTO_UPDATE=1` so it never rewrites itself mid-turn.

"Sign in with your Google account" runs the CLI bare on a terminal of the
server's: it asks how to sign in (the Google account is taken), prints the
authorization link, takes the code, and writes
`~/.gemini/antigravity-cli/antigravity-oauth-token` in the temporary HOME.
That file (it holds the refresh token) is stored encrypted and written
under each run's HOME, unless the copy there is already of the same
sign-in: the CLI refreshes it every hour and the stale copy is not put
back. There is no API key.

The jail is written as Antigravity's own permission rules in
`~/.gemini/antigravity-cli/settings.json`, which it enforces even with
permissions skipped: `read_file(…)` and `write_file(…)` denied for every
path the jail denies, `write_file(…)` for what is lent, `command(…)` for
the jail's commands. The usage windows (one weekly limit per model group)
come from the CLI's own `/usage`, run with a HOME of the module's own; it
spends no tokens.

## agent-opencode

Conversations run by [OpenCode](https://opencode.ai), which talks to many
vendors: instead of one sign-in it keeps one API key per provider, chosen
from OpenCode's own catalog (OpenAI, OpenRouter, Anthropic, Groq, DeepSeek
and two hundred more). The models the chat offers are the ones the CLI
lists with those keys, read again whenever a provider is connected or
disconnected and every twelve hours; OpenCode's free tier, which it lists
too, is left out because it only answers its own app.

- Config: `model` (default `provider/model`), `bin`. The keys are kept
  encrypted in the module's config and written, before every turn, to
  the CLI's `auth.json` under the site's HOME.
- Routes (also before enabling): `GET providers` → `{catalog, connected:
  [{id, name, models}], models, reading, models_error}`, `PUT
  providers/{id}` `{key}`, `DELETE providers/{id}`, `POST models/refresh`.
- The jail becomes OpenCode's permission rules (`OPENCODE_CONFIG_CONTENT`):
  everything outside the folder denied, the jail's commands denied after a
  general allow (the last matching rule wins there), a lent folder open to
  reading and closed to edits, and a level without some tools denying
  them. `opencode run` refuses whatever would ask.
- Installed under modules/node from npm (`opencode-ai`) when there is no
  binary.

## agent-openai, agent-deepseek, agent-zai, agent-kimi, agent-qwen, agent-mistral, agent-xai

The best-known model makers, each a module of its own: a card, an API
key, its models, and an agent of the chat under the vendor's name. Each
runs the vendor's own agent when it has one that works detached, the way
`claude -p` does; DeepSeek and Z.AI have none and document Claude Code
against their Anthropic-compatible APIs, so that is what runs them.

| Module | Agent that runs the turns | How it is installed | Key |
| --- | --- | --- | --- |
| agent-openai | Codex CLI, `codex exec --json` | npm `@openai/codex` | `CODEX_API_KEY`, or a ChatGPT sign-in (`codex login --device-auth`: a link and a code, from the card) kept in one CODEX_HOME every turn shares, because Codex refreshes it and copies would drift |
| agent-qwen | Qwen Code, `qwen --output-format stream-json` | npm `@qwen-code/qwen-code` | `OPENAI_API_KEY` + `OPENAI_BASE_URL` (QwenCloud, Coding Plan, China) |
| agent-kimi | Kimi Code, `kimi -p --output-format stream-json` | npm `@moonshot-ai/kimi-code` (Node 22.19+) | `KIMI_API_KEY`, named in a per-turn `config.toml` (`api_key_env`) |
| agent-xai | Grok Build, `grok -p --output-format streaming-json` | binary from `x.ai/cli` by version (no checksum published; the sha256 is recorded in `deps.downloaded`) | `XAI_API_KEY` |
| agent-mistral | Vibe, `vibe -p --output streaming` | `uv tool install mistral-vibe` (uv pinned with its sha256; it brings Python 3.12 when the machine has none) | `MISTRAL_API_KEY` |
| agent-deepseek | Claude Code | npm `@anthropic-ai/claude-code` | `ANTHROPIC_BASE_URL=https://api.deepseek.com/anthropic`, `ANTHROPIC_AUTH_TOKEN` |
| agent-zai | Claude Code | npm `@anthropic-ai/claude-code` | `ANTHROPIC_BASE_URL=https://api.z.ai/api/anthropic`, `ANTHROPIC_AUTH_TOKEN` |

The key always travels in the environment of the turn, never in a file
under the site's HOME. DeepSeek and Z.AI never get the Claude module's
subscription or key: a turn for them cannot spend on Anthropic. The
models are read from the vendor's `/models` with the key when the module
is turned on, filtered to the ones that chat (Mistral's Vibe runs the
model it is built around; Z.AI has no such list). Turned off, the agent
leaves the chat.

The jail, per agent: Codex runs in its own sandbox (workspace-write, or
read-only for a level that cannot write); Kimi Code and Grok Build get it
as their permission rules (paths denied outside the folder, the jail's
commands denied); Qwen Code as Gemini CLI did (excluded shell commands in
its settings, its file tools confined to the folder); Vibe by the tools a
level allows. What none of them can express is caught by the audit after
the turn, as for every agent. Kimi Code prints no session id, so each
conversation has its own data directory and a turn resumes with
`--continue`.

## vpn

A WireGuard interface (`mw0`) so the panel and the service door are
reachable without exposing them. Peers are created in the panel; each gets
a client config shown once. "VPN only" makes the panel unreachable from
anywhere else.

- **Needs**: `endpoint` (host:port clients connect to), and optionally
  `listen_port` (51820) and `subnet` (10.77.0.0/24). Plus the helper (it
  writes `/etc/wireguard/mw0.conf` and brings the interface up) and
  `wireguard-tools`, which `middleware install` puts there with git and
  curl, and the container image ships.
- **Verify**: the values are well formed, `wg` and `wg-quick` are in PATH,
  the helper answers. Whether the endpoint reaches this machine is a
  `skip`: only a client connecting can tell.
- **Installs**: nothing; the helper's operation list has no package
  manager in it on purpose, so a missing `wireguard-tools` is reported,
  not fetched.
- The server key and the peers' preshared keys are kept in the same
  encrypted store as every other credential (`data/modules-key`), so
  `middleware vpn` and the panel read each other's state.

Inside Docker it works when the helper runs in the container: uncomment
`MIDDLEWARE_HELPER: "1"`, `cap_add: [NET_ADMIN]` and the `51820/udp` line
in `docker-compose.yml`. The entrypoint then starts the helper as root
and drops to the `middleware` user for the supervisor.

## docker

Run a site as a container instead of a process: `run.runtime: "docker"` in
its manifest. Same handover, same logs, same health check.

- **Needs**: a reachable Docker daemon the middleware user can talk to
  (member of the `docker` group, or a rootless daemon).
- **Verify**: `docker version` answers; a `hello-world` sized image can be
  run and removed (`skip` if the daemon is offline, with why).
- **Installs**: nothing; Docker is yours to install.

## Writing one

A module is a Go package implementing the module interface (key, name,
what it needs, `Verify`, `Enable`, `Disable`, routes). It may install an
external binary with `os/exec`; it may not import one. Look at the shipped
modules and at [CONTRIBUTING.md](../CONTRIBUTING.md).
