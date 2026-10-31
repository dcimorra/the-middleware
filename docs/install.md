# Installing

`middleware` is one static binary. It runs as an unprivileged user under
systemd and needs root for exactly five things, which the installer does
through `sudo` and prints as it goes. Requirements: Linux with systemd,
`sudo`, and a user account that is not `root`.

## One-liner

```sh
curl -fsSL https://raw.githubusercontent.com/dcimorra/the-middleware/main/scripts/install.sh | sh
```

The script refuses to run as root, detects the architecture (amd64, arm64),
downloads the latest release binary, verifies it against the `.sha256`
published next to it, and runs `middleware install`. Arguments after `sh -s --`
reach the installer:

```sh
curl -fsSL …/install.sh | sh -s -- --root /opt/middleware --no-helper
```

"Latest" means the newest release, prereleases included; `MIDDLEWARE_VERSION`
pins one:

```sh
curl -fsSL …/install.sh | MIDDLEWARE_VERSION=v2.0.0-alpha.1 sh
```

## From source

Needs Go (version in `go.mod`), nothing else: the core has no dependencies.

```sh
git clone https://github.com/dcimorra/the-middleware
cd the-middleware
scripts/build-from-source.sh            # builds ./middleware and runs install
scripts/build-from-source.sh --no-install
```

or `make build && ./middleware install`.

## Docker

For trying it out, for a development machine, or for hosts without
systemd. The image ships Node, Claude Code and wireguard-tools, fetches
the other agent CLIs when their module is enabled, and keeps them current
on its own. The privileged
helper runs inside the container when `docker-compose.yml` says so
(`MIDDLEWARE_HELPER: "1"` with `cap_add: [NET_ADMIN]`), which brings the
layer-3 firewall and the WireGuard vpn; the port reservation sysctl is the
one thing a container never gets.

```sh
echo "MIDDLEWARE_SECRET_KEY=$(openssl rand -hex 32)" > .env
docker compose up -d
docker compose exec middleware middleware setup     # prints the one-time code
open http://127.0.0.1:46100
```

`docker-compose.yml` keeps everything in one named volume and maps the panel
to loopback only. The container runs as an unprivileged system user, so a
host directory bind-mounted in place of the volume must be writable by it:
set `user:` in the compose file to the directory's owner (`id -u`:`id -g`),
or `chown` the directory to the container's user. Keep `MIDDLEWARE_ROOT`
pointing at the path *inside* the container. The 80/443 lines are commented until the ports door is
enabled; the comments say what to do.

Inside the container the supervisor binds `0.0.0.0:46100` (the image sets
`MIDDLEWARE_LISTEN`): a published port comes in by the container's `eth0`,
and a listener on the container's own loopback would reset it. The host
side of the mapping is `127.0.0.1`, so the panel is still reachable from
that machine only. `MIDDLEWARE_LISTEN` overrides `listen` in
`data/settings.json` wherever it is set; leave it unset under systemd.

## Local development bench

The **local door** is for a laptop or a workstation: sites named
`something.middleware` (any suffix you like) resolve to this machine and
answer over plain HTTP, with no internet, no hosts file and no certificate.
It is two listeners: an HTTP one on `127.0.0.1:80` that serves the chain
straight (no redirect to 443), and a name server on `127.0.0.1:5353` that
answers loopback for every site here and NXDOMAIN for everything else.

```sh
middleware inbound local on            # or the Local card on the Inbound screen
```

The command prints how to tell your resolver to send the suffix there; the
same text is under "How to point this machine's resolver here" on the
panel. For Linux with systemd-resolved:

```sh
sudo mkdir -p /etc/systemd/resolved.conf.d
printf '[Resolve]\nDNS=127.0.0.1:5353\nDomains=~middleware\n' | sudo tee /etc/systemd/resolved.conf.d/middleware.conf
sudo systemctl restart systemd-resolved
```

macOS takes a file in `/etc/resolver/middleware` with `nameserver` and
`port` lines; Windows takes an NRPT rule (no port there, so run the door's
DNS on `127.0.0.1:53`). Then create a site called `misite.middleware` and
open `http://misite.middleware/`. Browsers resolve `*.localhost` to the
machine on their own, so `misite.localhost` needs no resolver change.

Under Docker the listeners must bind every interface of the container
and be published on the host's loopback. The image defaults them to
`0.0.0.0:8080` and `0.0.0.0:5353` (`MIDDLEWARE_LOCAL_HTTP` and
`MIDDLEWARE_LOCAL_DNS`, honoured until the file names other addresses);
open the door and uncomment the local-door lines in `docker-compose.yml`.
An installation whose file already says `127.0.0.1:80` fixes it with
`middleware inbound local on --http 0.0.0.0:8080 --dns 0.0.0.0:5353`. The local and ports doors cannot share port 80;
on a development machine leave the ports door closed.

## What `middleware install` does

Each step prints one line, `✓` or `✗`, and the command stops at the first
`✗`. Every step compares before it writes, so running it again is quiet and
changes nothing; running it with a newer binary updates the binary and
restarts the service (the sites keep running, see [upgrade.md](upgrade.md)).

| step | as | what |
| --- | --- | --- |
| package manager | you | finds apt, dnf, apk, pacman or zypper |
| git and curl | sudo | installs whichever is missing (and wireguard-tools, nftables, and libatomic, which the arm64 builds of Node load) |
| fail2ban | sudo | installs fail2ban when missing and writes `/etc/fail2ban/jail.d/middleware.conf` and `action.d/middleware.conf`: an SSH jail with the house's numbers (5 failures in 10 minutes, an hour, four times longer at every repeat, 30 days at most) and exceptions (loopback, private and CGNAT ranges), whose every ban is also written into the house's block list (`middleware waf block`), shown in the panel and closed in the kernel for every port; fail2ban's own nftables ban stays too. Uninstall removes the two files and leaves the package. A failure here warns and the install goes on |
| docker | sudo | installs the distribution's docker engine when missing (`docker.io`, `moby-engine`, `docker`), enables and starts its daemon, and adds you to the `docker` group so the supervisor can run sites in containers; a failure here warns and the install goes on. The `docker` group is root on the host through the daemon ([security.md](security.md)) |
| system group | sudo | `groupadd --system middleware` |
| user in group | sudo | `usermod -aG middleware <you>` — takes effect at your next login |
| root directory | you (sudo if the parent is root's) | `/srv/middleware` with `sites/ tasks/ data/ modules/`, owned by you |
| binary | sudo | copies itself to `/usr/local/bin/middleware` |
| units and config | sudo | `middleware.service`, `middleware.socket`, `middleware-helper.service` in `/etc/systemd/system`; `/etc/tmpfiles.d/middleware.conf`; `/etc/sysctl.d/60-middleware-reserved-ports.conf` |
| sysctl | sudo | reserves ports 33000–47000 from the kernel's ephemeral range |
| tmpfiles | sudo | creates `/run/middleware` (the helper socket lives there) |
| daemon-reload, enable, start | sudo | socket, helper, service |
| operator | you | runs `middleware setup` if no operator exists and prints the one-time code and the panel URL |

The rendered units are also checked in under [`systemd/`](../systemd/) so you
can read them before installing. The comments in them say why each line is
what it is; the one that matters most is `KillMode=process`, which is what
lets the sites outlive a restart of the supervisor.

Flags: `--root <dir>` (default `/srv/middleware`, or `$MIDDLEWARE_ROOT`),
`--user <name>` (default: you), `--no-helper` (skip the privileged helper;
firewall, VPN and sysctl operations are then unavailable), `--no-start`
(write and enable the units but do not start them), `--yes` (no prompt).

## Where things end up

```
/usr/local/bin/middleware                      the binary
/etc/systemd/system/middleware.service         supervisor, runs as you
/etc/systemd/system/middleware.socket          holds 127.0.0.1:46100
/etc/systemd/system/middleware-helper.service  root; closed list of operations
/etc/tmpfiles.d/middleware.conf                creates /run/middleware at boot
/etc/sysctl.d/60-middleware-reserved-ports.conf
/run/middleware/helper.sock                    root:middleware 0660
/srv/middleware/                               the root: yours
  sites/<domain>/                              one folder per site, site.json inside
  tasks/<name>.json
  data/                                        settings, modules, state, logs, events
  modules/                                     binaries modules installed
```

## After installing

```sh
middleware doctor      # one line per check; exit 1 if any fails
middleware setup       # if the installer did not run it (prints the one-time code)
```

The installer asks for the panel's domain (or takes `--domain`, or
`MIDDLEWARE_DOMAIN`) and then how it is reached (`--door ports|tunnel`, or
`MIDDLEWARE_DOOR`; ports by default). Neither door is required, and only
the chosen one is opened; new sites go through it too.

- **ports**: 80 and 443. The first sign-in is at `https://<domain>` once
  its DNS points at the server; the certificate is obtained on its own.
- **tunnel**: no port is opened. The name is reached through a Cloudflare
  tunnel, which needs the cloudflare module with `tunnel_id` set to this
  server's tunnel (one is never picked by itself: an account can hold
  other servers' tunnels). Until then, sign in through SSH as below.

The site pictures in the panel need a headless Chromium. When the
machine has none (the system's, Playwright's or an earlier install), the
installer says so and asks whether to install one (`--browser yes`, or
`MIDDLEWARE_BROWSER=yes`): Chrome for Testing's headless shell from
Google, amd64 or arm64, unpacked under `modules/chromium/` of the root,
plus the system libraries its own `deb.deps`/`rpm.deps` lists (Alpine and
Arch get their `chromium` package). One already there is never installed
again, and the running service picks a new one up without a restart.

With the cloudflare module on, the panel's name is published like a
site's: its DNS record, and its tunnel route on the tunnel door. Without
it, the name is marked to point by hand. `middleware setup --domain <name>
--door <door>` does the same later, and the Inbound screen moves it.

Without a domain the panel is at `http://127.0.0.1:46100`, loopback only;
from your machine, `ssh -L 46100:127.0.0.1:46100 host` and open the same URL.

If `doctor` says the helper socket is not reachable with a permission error,
you have not logged in again since the installer added you to the
`middleware` group.

## Several machines

Installations join one network from the panel's Dashboard: one makes an
invitation code, the other pastes it, and from then on every node sees
and manages every other (see [architecture](architecture.md#linked-nodes)).
They talk only through a WireGuard mesh, so each machine needs:

- **UDP 51821 open** to the other machines, in its own firewall and in the
  provider's (an IONOS, Hetzner or AWS firewall rule, as UDP, not TCP). A
  machine behind NAT can leave it closed and join with an empty address:
  it then always calls out first.
- The helper service (`middleware install` sets it up): the interface and
  its firewall are root's business.

Nothing else of the machine's traffic goes through the mesh, and through
it each node reaches only the others' middleware (TCP 46101 on the mesh
address). To try several installations on one machine without root, start
them with `MIDDLEWARE_MESH_LOOPBACK=1`: no tunnel, addresses on loopback.

A new machine can join at install instead of getting a domain: where the
installer asks how the panel is reached, the second answer takes an
invitation code (or `middleware install --join <code>`, or
`MIDDLEWARE_JOIN`). The node joins that network once its service is up and
has no domain and no operator of its own: it is managed from the network's
panels, and nothing of it is public but its sites. `--endpoint ip:port` is
its address for the mesh; without it, the house's guess (its public address
and UDP 51821), and behind NAT it only calls out. A node that already has a
domain can drop it from the Dashboard (its card, **Network only**): its
record and tunnel route are retired. It is refused outside a network, or
with no other node answering, because the panel would then be reached only
through an SSH tunnel. Giving it a domain again in its Settings undoes it.

## Updating

```sh
curl -fsSL …/install.sh | sh        # same command; or: middleware install with a new binary
```

The installer replaces the binary and restarts the service. Sites keep
running and are re-adopted; see [upgrade.md](upgrade.md) for what that means.

## Removing

```sh
middleware uninstall          # or scripts/uninstall.sh
```

Stops and removes the units, the tmpfiles and sysctl entries and the binary
(`--keep-binary` keeps it). **It never removes the root directory or the
sites.** Delete `/srv/middleware` yourself when you mean it.
