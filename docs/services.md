# Services: one site, several processes and names

A site whose project is more than one process (a mock in Node, a Next.js
app, a Drupal and its database) declares them in `services`, each with its
own run section, names, build, health, log and lifecycle. The fields are in
[manifest.md](manifest.md#services-several-processes-and-several-names);
this page is about why it is shaped that way and how to move a site to it.

## What it replaces

Before services, such a project was one image with a router and a
supervisor written in bash inside it. Three things went wrong there, every
time:

- **One failure was everybody's.** A typo in one line of the supervisor,
  or one part that failed to build, kept every part from deploying.
- **A part that was starting answered a raw 502** from the router inside,
  which the house could not tell from a site that was down.
- **Every change rebuilt everything**, so a change to the mock waited for
  Drupal's image.

With services, the house is the router and the supervisor: a service that
fails keeps its previous generation and the others carry on, its names get
the house's page (503, `Retry-After`) while it starts, and a change to
`deploy/next/` rebuilds `next` and nothing else.

## Compatibility

Nothing changes for a site that does not write `services`:

- `run` and `aliases` keep working exactly as before; such a site is one
  implicit service. Its revision is computed as before, so no site
  restarts on the upgrade.
- Its container keeps docker's default bridge, its port inside equal to
  the port outside, as it always had. The per-site network is created only
  for sites with services.
- Volume names are the same for a site, a worker and a service
  (`mw-<domain>-<name>`), so a volume moves between them without a copy.
- The API and CLI only gained routes and flags (`site routes`,
  `site validate`, `--service`); none changed meaning. `site show <name>`
  now also takes a name a site serves.
- The access log gains ` service=<name>` at the end of a line, only for a
  request a service answered.

## Moving a site, step by step

The plan for a site that is today one image with a router inside
(`bakery.example`: a mock, Next.js, Drupal and MariaDB):

1. **Split the image.** One Dockerfile per part under `deploy/<service>/`
   (`deploy/next/Dockerfile`, `deploy/drupal/Dockerfile`), each building
   one thing, with the site's folder as the build context and its
   `.dockerignore` read. A binary the bash script downloaded becomes a
   `COPY --from=<image>:<tag>`: it is right on arm64 too.
2. **Write the draft** with `services` next to the current manifest
   (`site.new.json`): the site's own name on the service that answers it,
   each other name on its service, the database `internal`, `depends_on`
   where one needs another healthy first, `$SERVICE_db_HOST` and
   `$SERVICE_db_PORT` where the bash script had hard-coded addresses.
3. **Check it**: `middleware site validate bakery.example --file
   site.new.json`. It writes nothing and says what would be refused (a
   name another site serves, a missing Dockerfile or env file, a cycle in
   `depends_on`) and where each name would go.
4. **Keep the data.** A database whose files were in a named volume of the
   old container keeps them if the service mounts the same name (the
   volume is `mw-bakery-example-<name>` either way). Files in a bind mount
   under the site's folder stay where they are. Files that only lived in
   the old container's own layer are copied out first, into the site's
   folder, which every container of the site sees at its own path and is
   the working directory of a command: `middleware exec bakery.example --
   tar -C /var/lib/mysql -cf db.tar .` before the switch, and after it,
   with the service stopped (`site stop bakery.example --service db`),
   `middleware exec bakery.example --service db -- tar -C /var/lib/mysql
   -xf db.tar`, then `site start … --service db`. A dump and a restore is
   what a database prefers, when it has one.
5. **Switch** by replacing `run` and `aliases` with `services` in one write
   of `site.json` (the panel's manifest tab, or `PUT /manifest`). The old
   instance keeps answering every name until each service with names that
   is not optional is up, and only then is it stopped: nothing goes dark
   while Drupal installs. `middleware site routes bakery.example` shows
   each name moving to its service.
6. **Going back** is restoring the previous manifest from its history
   (`site.json` history in the panel, or
   `POST /manifest/history/{id}/restore`): the same handover, the other
   way.

## A service's data when the site moves

A service's named volume is the site's data, and it moves with the site
to another node ([api.md](api.md), "Moving a site to another node"):
copied file for file once the site is stopped, read back on the other
node, and compared, before anything starts there. A move that cannot
carry a volume whole is undone; the volume where the site was is never
removed by the move. verdelaurel.es is why: its `db` service kept
Postgres in `mw-verdelaurel-es-db`, the move took the folder, and the
service ran `initdb` on an empty volume on the other node while the web
seeded a demo catalogue over a shop with orders.

A database prefers a dump to its files between machines of different
processors: declare it in `backups` ([manifest.md](manifest.md)), whose
copies go with the site too.

## A site needs a port that is not HTTP

A TURN relay beside a WebRTC app, a game server, a broker: something that
answers the internet on a port of its own, often UDP. The way is a
service with `ports` ([manifest.md](manifest.md), "Public ports"), never
a container started by hand next to the site. p2p-webrtc had its coturn
in an `infra/docker-compose.yml` brought up from a shell: the house did
not know it existed, the site moved to another node without it, its
`--external-ip` still named the node it had left, and the first to find
out was a visitor behind a symmetric NAT.

1. Add the service to the manifest's `services`, with its `ports` and,
   for a relay with a range of ports, `network: "host"`. Write the
   address as `$MIDDLEWARE_PUBLIC_IP`, never as a number, and keep
   secrets in an env file (`run.env_files`), not in the command.
2. Give it a `health.command` that asks the service itself
   (`turnutils_stunclient -p 3478 127.0.0.1` for coturn).
3. The owner approves the ports: saving the manifest from the panel or
   with their own token does it; a manifest written by the site's agent
   waits for `middleware site ports <domain> approve` or the button in the
   site's services.
4. `middleware site show <domain>` says how the service is and
   `middleware site logs <domain> --service <name>` is its output; `site
   ports <domain>` says what is asked, what is approved, the node's public
   address and whether the firewall took it.
5. If the machine sits behind a provider's firewall, open the same ports
   there. That is the one step the house cannot do.
6. Stop what was started by hand (`docker compose down` where it was
   brought up): it holds the ports, and the house says so
   (`ports.conflict`, or "address already in use" in the service's log).

After that the relay is the site's: it starts with it, is watched and
restarted, is in its logs and limits, and moves with it to another node
with nothing to edit there.

**What a move says about the rest.** A compose file in a site's folder
is not read by the house, and containers started from it stay behind. The
plan of a move lists both as warnings (the containers by name, image and
ports), the move records them (`relocate.incomplete`), and the node the
site arrives at says it again in its own events.

## Security

Services are as strict as a site, and in places stricter:

- Public ports and the host's network are the owner's to give, by
  account and by file: a delegate's save that changes them is refused,
  and one written into `site.json` by the site's agent does not start
  until the owner approves it. A container on the host's network sees
  the machine's loopback, where every site's process listens: approve it
  for an image you would run on the machine yourself.

- A service is a run section with the same rules: a delegate cannot change
  a service's names, runtime, docker section or env files, as for a site's
  run.
- An internal container that nothing on the machine needs is not
  published on the machine's loopback, which every site shares: only its
  site's network reaches it.
- Each site with services has its own docker network; no other site's
  containers are on it.
- An image in a service names its version (a tag other than `latest`, or a
  digest), so its revision says what runs.
- A service's `access` and `security` only add to the site's; a service's
  allow list or password can never open what the site closed.
- A site's agent never gets the docker socket (it is root on the machine).
  `middleware exec --service` from its turn goes through the house, scope
  `exec:<site>`, into the site's containers only; a command that would run
  on the machine is refused there, since it would run as the supervisor,
  outside the turn's jail.
- There is no import of a compose file. If one is added later it will be
  declarative only, and `privileged`, `cap_add`, `network_mode` (the
  host's network is `services.<name>.network`, with its approval), mounts
  outside the site's folder, `devices`, `pid` and `ipc` will be errors.

## Limits, for now

- A service's public port is the same number inside and outside, on every
  address of the machine; there is no mapping and no choice of address.
- A firewall outside the machine is opened by hand.

- A service's container cannot reach a process service on the machine;
  run both in containers, or make the process the one that calls.
- A process service may name only services that restart in place (those
  without names), whose port does not move with each generation.
- `middleware exec` through the house has no terminal and no stdin; a
  shell to type into is the operator's, on the machine.
