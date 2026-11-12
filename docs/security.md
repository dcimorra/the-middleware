# Security

What protects what, and what does not. Report vulnerabilities as
[SECURITY.md](../SECURITY.md) says.

## Threat model

The machine runs sites you do not fully trust (a dependency you did not
read, a build script from a repository, an agent that writes code) behind a
supervisor that you do trust, operated through a panel that must not be a
way in. The attacker is: anyone on the internet, a compromised site, or an
agent doing something it should not.

What is defended: the supervisor's privileges, the other sites, the
operator's credentials, the panel. What is not: a site's own data from that
site's own code. A site that leaks its own database leaks it; middleware is
not a sandbox for the site's logic.

## The pieces

**Unprivileged supervisor.** `middleware.service` runs as your user. It
binds 80/443 with `CAP_NET_BIND_SERVICE` and that is the only capability it
has. If the supervisor is compromised, the attacker is your user, not root.

**Closed helper.** `middleware-helper.service` is the only root process.
It listens on `/run/middleware/helper.sock` (`root:middleware`, `0660`) and
answers a **closed list of operations**: `firewall.apply`, `firewall.block`,
`firewall.unblock`, `vpn.apply`, `vpn.up`, `vpn.down`, `sysctl.reserve_ports`,
`update.install`, `update.confirm`, `requirements.install`, `ports.open`,
`ports.meter`. `ports.open`
takes the whole list of the sites' approved public ports (numbers and
`tcp`/`udp`, nothing else) and makes a chain of its own in iptables say
exactly that. `ports.meter` takes the same list with one boolean per port
and keeps two chains of its own that count those ports' bytes and drop a
port whose site spent its transfer budget; it answers numbers. `requirements.install` takes one
boolean (add a headless Chromium) and nothing else: what runs is the
helper's own binary's `install --requirements`, for the user, root and
binary the installed `middleware.service` names (written by root; a unit
pointing at another binary is refused), in a transient unit, and it only
installs the house's own list of packages; it writes no unit, replaces no
binary and restarts nothing. `update.install` takes a release's name and nothing else: it
downloads that release from this repository only, checks the published
sha256, runs the binary to see it is that version, and refuses anything
not newer than what runs, so a supervisor that was broken into can neither
install a binary of its own as root nor go back to an older one.
`update.confirm` takes the version the supervisor runs and only ends the
trial of that version; the rollback to `middleware.prev` when no
confirmation comes is the helper's own decision from its own record
(root's, next to the binary), so the most a broken-into supervisor gets
by withholding it is the release that ran minutes before.
Each validates its input (addresses parse, keys are base64 of the right
length, names match `^[a-z0-9-]+$`) and none takes a free-form string that
reaches a shell. Its unit has `ProtectSystem=strict` with exactly three
writable paths. A feature that needs root needs a new operation with its own
test, not a wider one. Inside Docker the helper is optional: the image's
entrypoint starts as root, runs the helper only when `MIDDLEWARE_HELPER=1`
(with `NET_ADMIN` granted by compose), and drops to the `middleware` user
for the supervisor either way; without it those features are absent rather
than running as root.

**Subscription sessions.** Signing in with a Claude subscription or a
Google account runs the vendor's own interactive flow on a pty with a
temporary HOME; what comes out (a long-lived token, a token file) is
stored in the encrypted credentials store like any other secret and never
shown again: the panel's "what the program printed" view blanks it. A
turn receives it in its environment (`CLAUDE_CODE_OAUTH_TOKEN`) or under
its HOME (`.gemini/antigravity-cli/antigravity-oauth-token`, mode 0600), which an agent running
there could read; a site's own process never does.

**Per-site HOME and environment.** Each site runs with `HOME` under
`data/homes/<domain>/`, an environment built from its manifest only (no
inherited variables from the supervisor, no duplicate keys), and its own
process group. A site's `~/.ssh`, `~/.npmrc` or agent login is not another
site's. Sites run as the same Unix user as the supervisor, though: see
"what is not protected".

**Host git in a site folder.** The supervisor runs git in a site's folder
to deploy from GitHub and to keep marked repositories pulled. That folder,
`.git` included, is a docker site's own writable mount, and git reads from
inside the repository several keys that name a program it then runs
(`core.fsmonitor`, `core.sshCommand`, a filter driver's clean/smudge, a
diff textconv, `credential.helper`, `gpg.program`,
`uploadpack.packObjectsHook`) and the hooks in `.git/hooks`. The
supervisor's user is in the
docker group, so that program would run as effectively root. One hardened
path (`internal/hostgit`) is the only way the house runs git in such a
folder: it strips the environment to `PATH`, `HOME` and a locale (no
`MIDDLEWARE_SECRET_KEY`, no inherited secret), keeps system and global
config out, passes `-c` overrides that disable the single-valued keys and
the hooks and limit transports to the ones the house uses, and — because
`-c` cannot blanket-disable a key whose middle name the attacker chose
(`filter.<x>.clean`, `credential.<url>.helper`) — reads the repository's own
config first and refuses to run, naming the key, when it finds one that runs
a program. A deploy then fails with that key in the event and the running
build is untouched; a marked repository shows the refusal instead of pulling.
The single-valued keys a repository may set for itself (husky writes
`core.hooksPath=.husky` on every install) are overridden, not refused, so
such a site still deploys and its hooks still do not run on the host.

**The jail for agents.** A conversation's agent may read and write the
site's folder and nothing above it, and no other site. Two things outside
it are its own to read and not write: the house's guide in its HOME, named
to it by its full path, and its logs (`data/logs/<domain>/`: the site's
process, its workers, its moves, its requests); the other sites' logs and
the tasks' stay denied. The jail is enforced
by the core before anything reaches the CLI: paths are resolved and checked
against the site folder, symlinks included. The adapter is a subprocess with
no say in it.

**The agent CLI's own folder.** A turn's HOME is the site's (a docker site
mounts it), and its folder comes from its repository, so neither may hold
anything the agent CLI runs by itself. Claude Code, Codex and Kimi Code
keep their settings, sessions and login in `data/agents/config/<domain>/
<agent>/` (`CLAUDE_CONFIG_DIR`, `CODEX_HOME`, `KIMI_CODE_HOME`), which no
container mounts and the jail denies to the agent's tools; what an earlier
release kept for them under HOME (sessions, a login) is copied in once,
plain files only. Claude Code runs with hooks off (`disableAllHooks` in the
`--settings` file, which outranks every file but managed policy), with the
user's settings only (`--setting-sources user`: a project's
`.claude/settings.json` could name an `apiKeyHelper` or MCP servers, which
ran on the host in print mode) and no MCP server (`--strict-mcp-config`);
the project's `CLAUDE.md` is still read, from `--add-dir`. Qwen Code, Vibe,
OpenCode, Antigravity and Grok still read their configuration from HOME or
from the project as their CLIs do.

**The CLI from inside a turn.** A turn runs as the supervisor's user with
the `middleware` binary in its PATH, so whatever its Bash calls start can
open `data/api.sock`. The socket reads who connected (`SO_PEERCRED`) and
walks that process's parents: when one of them is a site conversation's
turn (the chat manager's running turns, or the `MIDDLEWARE_CHAT_ID` mark
every turn's environment carries, its ancestors' included), the request is
that site's and opens what a token scoped `read`, `restart`, `deploy` and
`tasks` for it opens, plus the events about it; no tokens, no settings, no
other site, not the house's conversation. `tasks` lets the turn manage its
own site's scheduled tasks (`middleware site tasks <domain>`): the ones
whose `site` is that site, which run in its folder with its environment,
where its deployed code already runs. It cannot see, run or replace a task
of another site or of the root (404, and 409 to write over one), cannot
write a task for another site (400), and never reaches `/tasks`, which is
every task of the house. Refusals emit `api.turn_refused`. A
connection whose process cannot be looked at is not trusted. `middleware
exec` reads the same chain and runs only in the turn's own site. The
house's own conversation stays the operator, by design. This is a fence
against an agent (or a prompt injected into it) using the obvious door,
not a user boundary: see "what is not protected".

**Service doors.** A token with scopes opens a closed list of routes,
optionally for one site (`internal/auth/scopes.go`, the one place that
says what each opens): `read`, `restart`, `deploy`, and `tasks`, the six
`/sites/{domain}/tasks` routes, for a program that manages a site's
scheduled work from outside. `tasks:<domain>` gives what a site's turn has
over its tasks and no more: the routes check the task is that site's, and
`/tasks` stays the operator's. A task written through it is signed
`token:<name>` in the audit trail. A `tasks` scope with no site opens every
site's tasks, never the root's.

**Delegated users.** The operator can make users who manage some sites and
nothing else (`internal/auth/users.go`, `perms.go`): each signs in with a
name and a password at the node they were made on, through the same brakes
as the operator's password, and every request of theirs is decided by a
closed list of routes per permission (`view`, `operate`, `code`,
`agents`), for the sites granted. Every route under `/sites/{domain}` is
in one permission's list or kept for the operator, and the wired process's
test fails for one that is in neither. A site not theirs is a 404, so a
user cannot learn which other sites exist. Grants and the user are read
on every request: a grant taken away or a user deleted is gone on the
next click. A user's site on another node is answered by that node: this
one relays the request through the mesh with who the user is and what
they may do there, and that node holds them to the same lists before
answering; refusals and failures come back without the node's name or
address, and the events about moves, nodes and the house are left out of
what a user is shown. What a permission means for the machine is the
honest part:

- `view` and `operate` read the site and change its state; they run
  nothing of the user's choosing.
- `code` decides what the site runs: its files, its manifest, its build,
  its tasks and a command where it runs. All of it runs in the site's
  jail (below), so what it reaches is the site's folder and its HOME. The
  manifest's keys that reach past the site (`aliases`, `upstream`,
  `run.runtime`, `run.docker`, the files read into the environment, the
  move, the VPN, the agents' monthly cap and the gateway, the folders a
  conversation may read) are refused from a user's account; which agent
  and model answer is theirs, within the limits below.
- `agents` is a conversation, whose turns run in the site's jail too, and
  spends the operator's subscriptions.

The operator can also limit which agents a user's conversations use and,
per agent, which models (`agents` on the user, `internal/auth/agents.go`).
The limits name providers and models whether or not the machine has them:
one turned on later is already limited. They travel with the user to the
node that holds the site, which shows the user only what the limits leave
and refuses a turn with anything else, the provider's default model
included (another allowed one is taken). The refusal comes before anything
is said of the provider, so a user cannot learn which agents the house has
outside their list. No limits is every agent; an empty list is none.

A node that cannot build jails (below) refuses `code` and `agents` to
delegates with a 503, on the node that holds the site: there their code
would run with the run of the house.

**A jail per site.** Every process of a site runs in a jail of the site's
own (`internal/jail`): its process, its workers, its tasks, a command run
from the panel, a deploy's build and every turn of its conversations,
whoever started them, the operator included. The jail is Linux
namespaces (user, mount, pid, ipc, uts) built by the supervisor without
root: the process's root is an empty tmpfs that holds only what the jail
names, each at its own path. The system (`/usr`, `/etc`, the libraries,
`/opt`, the folders of the supervisor's `PATH`) and the installation's
`modules/` are read-only; the site's folder and its HOME
(`data/homes/<domain>`) are writable; `/tmp` and `/dev/shm` are its own;
`/proc` is that of its own pid namespace, so no other process of the
machine exists for it; `/dev` holds a few devices. A conversation also
has its CLI's state (`data/agents/config/<domain>`), its own logs
(read-only) and `data/api.sock`, through which its `middleware` calls are
held to its site. A site's own process is not given the socket. Nothing
else of the installation is there: not the other sites, not their
homes, not `data/`. The command holds no capability and cannot gain one
(empty bounding set, `no_new_privs`). The network is the machine's: a
site still listens on its loopback port and reaches the internet.

What is shared on purpose: the CLI logins the operator chose to share
(a `claude` config folder set by hand, the Codex subscription's home) are
mounted writable into every site's turns, because the CLI cannot run
without them.

Building jails needs user namespaces. On Ubuntu 24.04 and later, AppArmor
allows them only to programs a profile names, and `middleware install`
(or Install requirements on the panel) writes one for the binary.
systemd's `ProtectKernelTunables` keeps a jail from mounting its `/proc`,
so units rendered now leave it off and older ones get a drop-in. The
supervisor checks at start: `jail.on` or `jail.off` on the panel, and
`middleware doctor` says what is missing. A process started before the
jail existed keeps running outside until it is restarted.

**Credentials encrypted at rest.** Module credentials live in
`data/modules.json`, AES-256-GCM, key from `MIDDLEWARE_SECRET_KEY` or
`data/modules-key` (mode `0600`). Backups of the root are safe to store
elsewhere only if the key is stored apart. The panel password is hashed;
API tokens are stored hashed and shown once.

**The panel.** Served on the loopback listener and on the panel domain
only, never on a site's domain. Login with a password, or passkeys;
sessions are cookies with `Secure`, `HttpOnly`, `SameSite=Strict`. The
unix socket `data/api.sock` needs no credentials because its file mode is
the credential. The panel answers through every door so closing the one you
came in by cannot lock you out, and the VPN module can make it reachable
through WireGuard only.

**Password guessing.** Every password door (the panel's login, a site's,
each zone's) goes through one brake (`internal/brake`): failures are counted
per address (per /64 for IPv6, per /48 at the panel's login), and five
inside a minute lock that address out of that door for a minute, doubling
with each lock (to a day for the panel, fifteen minutes for a site). A
success forgets the count. The panel's login also has a budget for the
whole house: thirty wrong passwords in ten minutes, from any number of
addresses, close the password for everyone for ten minutes, doubling to an
hour. From loopback the tunnel's `CF-Connecting-IP` is believed, so a
local process can name a new address on every try; this budget is what
stops it. A flood can keep the operator's password shut as well; the
passkey, the CLI and `middleware panel open` do not go through it. Locked
attempts get 429 with `Retry-After` and are not checked at all. On top of
that, at most two key derivations run at once in the whole installation, so
guesses spread over many addresses queue instead of taking every site's CPU.

**Probe paths.** Every site answers 404, before its process sees the
request, to what no website serves: `.git/`, `.ssh/`, `.env*`, private keys,
`/etc/passwd`, `/proc/self/`. A site adds its own prefixes in
`security.paths`, and can close robot families, browsers and countries there
too; that part trusts the User-Agent, so it filters what identifies itself
and guards nothing.

**Under attack.** `attack` in a manifest, or in the WAF policy for the whole
installation and the panel, puts a proof of work in front of the site: the
browser spends a fraction of a second, a flood pays it on every request,
and checking costs one hash. The pass is signed with a key on disk and bound
to the client. It filters floods, not people: whoever runs JavaScript gets
through.

**WAF, in layers.** Layer 7 in the core, always on: rate limits per route
with burst, body size limits, country and user-agent policies, a password or
an allow list per route, and an "under attack" mode that challenges or
rejects. The rate is per visitor, not per address: an address and its
browser (User-Agent and Accept-Language) are one visitor, so two phones on
one home connection have a burst each. What a page brings in with it (an
image, a script, a font, or a request that only asks whether it changed)
costs a quarter and is paid by the address alone, never the visitor: a
page of three hundred files reloaded ten times is neither refused nor
blocked. A visitor past its own rate gets 429s and nothing more. The
address as a whole carries eight visitors' worth (`NATShare`), and only a
flood past that, `trips` times in a minute, blocks it: for a while, and
when the helper is present also in nftables (`firewall.block`), so a flood
stops at layer 3 and never reaches Go. That block closes every site at
once, which is why volume alone from a person never earns it; exploits,
scanners and failed logins still do (the bans). The limit is the site's,
by its own domain, its `www.` and aliases sharing it, and one more for
every name no site serves together: a flood that names a new host on each
request is counted as one and blocked like any other. Blocks have a TTL and are
written down, so a restart neither forgets nor keeps them forever. A site
with `waf.off` in its manifest has no rate nor body limit; the block list
and the bans still apply to it.

The body limit (`max_body`, 20m by default) is for what visitors send. It
does not apply to a site's bundle coming into this house (`PUT
/api/v1/relocate/{domain}` and its `/delta`, on a host that is not a site):
a move or an import is hundreds of megabytes on purpose, and the API behind
authorizes it (the network's secret or a one-site import token). A body cut
by the limit halfway through, as a streamed one is, answers 413 naming the
limit and where to change it.

The operator's own session on the panel's domain is not rate-limited nor
counted toward a block: it is the panel's own traffic (the dashboard, the
bar asking every node, the stream), which on a phone passed the limit in a
minute and blocked the operator for ten. It is recognised by its cookie,
not its address, which on a phone changes; and the address it came from is
trusted for a week, as after a login.

**Guard.** Rules written in code (a site writing outside its folder, a
process spawning a browser, `npm audit` findings, a port that should not be
listening) produce incidents; the rule decides and acts, and only then may a
model be asked to explain. Who presses the button is never a model.

**Inbound.** The tunnel door exposes no port; the ports door exposes 80/443
with certificates obtained for the domains this installation serves and no
other. Headers from the edge (`X-Forwarded-For`, `CF-Connecting-IP`) are
trusted only from the door they belong to.

## Bans (fail2ban)

Inside the WAF, the first layer every request crosses. A request carrying
an exploit that is wrong on any site (Log4Shell's lookup in the target or
a header, a climb out of the folder to `/etc/passwd`, PHP's stream
wrappers, phpunit's `eval-stdin.php`, the routers' RCEs, command
injection, Shellshock) is refused and bans its address at once. What
scanners ask every server for (`/.env`, `/.git/`, `/wp-login.php`,
`/phpmyadmin`, a `.php` on a site without PHP…) counts as one attempt
only when the site answered with an error, so a WordPress's visitors are
never counted; a wrong panel password counts too. Five attempts in ten
minutes ban the address: in the WAF's block list (kept on disk) and in the
kernel through the helper's nftables set, for an hour the first time and
four times longer at every repeat, up to thirty days. Lifting a ban lifts
it in the kernel too, and what is still banned goes back into the kernel
when the supervisor starts (a reboot empties the kernel's set).

SSH is the one door the house does not see, so the installer gives it
fail2ban with the same numbers and exceptions; each of its bans is written
into the same block list (reason `fail2ban sshd`), so the panel lists it
with the others. fail2ban lifting it lifts it in the house; lifting it in
the panel lifts the house's block, while fail2ban keeps SSH closed to that
address until its own time is up (`fail2ban-client unban <ip>` ends it).

Never banned: loopback, the private and CGNAT ranges (the VPN's, the
mesh's), the allow list, and an address the operator logged in from in
the last week. Behind the tunnel the address is the edge's
`CF-Connecting-IP`; the kernel cannot cut those (they arrive from
cloudflared), so for them the ban is the WAF's 403.

## What is not protected

- **Sites share a Unix user.** Isolation between sites is HOME, environment,
  process group and the jail for agents, not a user boundary. A site that
  reads `../other-site/` can. For hard isolation, run that site with
  `run.runtime: "docker"`: it then sees only its own folder.
- **The supervisor can read everything in the root.** It has to. A bug in
  the supervisor is a bug with your user's rights.
- **Secrets in a site's `.env` are plain files** in the site folder, readable
  by anything running as your user. Encryption at rest covers module
  credentials, not site files.
- **Docker is root-equivalent.** A site in the docker runtime is isolated
  from the others, but the `docker` group grants the supervisor root on the
  host through the daemon. That is Docker's model, not ours.
- **The loopback listener does not trust the machine.** Anything that can
  reach `127.0.0.1:46100` can reach the panel's login page, held to the
  address lists (as `127.0.0.1`, or as the address it writes in
  `CF-Connecting-IP`, which loopback is believed about for the tunnel's
  sake), the closed password and the house's login budget; only the unix
  socket skips them. Use the VPN module or
  a tunnel that authenticates (Cloudflare Access) if other people have
  shells here.
- **A model may be wrong.** Agents act inside the jail with the site's
  rights; a conversation can still delete the site's files, commit nonsense
  or spend money. The git history is the undo.
- **No protection against the operator.** Tokens and the socket give full
  control. Delegated users are the one role below the operator, and their
  code and conversations run in their site's jail (above, "Delegated
  users" and "A jail per site").
