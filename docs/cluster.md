# Cluster mode

A site in cluster mode has copies (replicas) on other nodes of the network,
and the node that holds it hands them part of its requests when it is
saturated or its own instance is down. Its names keep pointing at that node,
the **primary**: it is still the only way in, and nothing changes in DNS.

```json
"cluster": {
  "replicas": 2,
  "nodes": ["oracle"],
  "mode": "overflow",
  "spill": 70,
  "gateway": ["127.0.0.1:3306"]
}
```

| key | meaning |
|---|---|
| `replicas` | on how many other nodes; 0 is one, or as many as `nodes` names |
| `nodes` | the nodes to use first, by name or id; the rest are chosen by load |
| `mode` | `overflow` (default): the primary serves everything until saturated; `spread`: always shared by free capacity |
| `spill` | the primary's load per core, in percent (one-minute average), from which it shares; default 70 |
| `gateway` | the primary's database addresses the replicas reach through the mesh (below) |

The panel's site page has a **Cluster** card that writes this block and
shows each replica's state and load.

## What happens

1. **Placement.** Every few seconds the primary asks every live node for its
   load and the replicas it keeps. A site's replicas go on the nodes it
   names, then on the ones it already has, then on the least loaded of the
   rest. A choice stays while its node stays in the network: a replica is
   not copied elsewhere because another node is a little quieter. The
   choice is kept in `data/cluster/placement.json`.
2. **Copy.** The primary takes a fingerprint of the folder (every name with
   its size and time) and, when a replica is at another one, streams the
   folder to it through the mesh. Left out: `.git`, the dependencies the
   other machine installs for itself (a package's `node_modules`, as a move
   leaves them), and what `move.exclude` names. The receiving node installs
   the dependencies **for its own machine** (an aarch64 node gets aarch64
   native modules); when the lockfiles did not change it links the ones the
   running copy has instead of installing. Only then does the new copy take
   the old one's place, in one step (`renameat2` exchange), and the engine
   hands over to it as for any revision.
3. **Routing.** On the primary, after the cache, a request for a clustered
   site is served here unless:
   - its load per core is at or above `spill` (it stops sharing below 60 %
     of it, so it does not flap), or `mode` is `spread`; then it is shared
     by weighted rendezvous hashing of the visitor's address over this node
     and the replicas, each weighed by its free cores, so **one visitor
     always reaches the same node** and a node that comes or goes moves only
     its own share;
   - its own instance is down: then everything goes to the replicas.

   Only a replica at the primary's exact fingerprint, alive, and heard from
   in the last rounds is used: during a deploy the replicas are left out
   until they have the new version. A replica that cannot be reached is left
   out for 30 s and a GET is served here instead.

The WAF, the security lists, the password door, the cache, the access log
and the visits all happen on the primary, once. The replica gets the
visitor's address, country and scheme as the tunnel's connector would give
them.

## The closed door

A replica is a site folder with `"replica": {"of": …, "revision": …}` in its
manifest, written by the house. It:

- is not among the names its node publishes, certifies or opens a door for,
  and its node's own doors answer its names as a name nobody serves;
- answers only requests its primary sends through the mesh door, marked as a
  site's request and carrying the network's secret in a header of its own
  (the visitor's `Authorization` goes through untouched);
- is not listed among its node's sites, has no deploy, no agent, no
  conversation, no automatic repair and no backups (all the primary's);
- is replaced whole at every new version, and removed when its primary stops
  naming it (after two minutes' grace) or leaves the network (after 30 min).

## Data: what cannot be copied

A replica runs the site's **code, not its data**. Two copies of a database
that both write are two databases from the first write on. So a site cannot
be clustered when (`manifest.Blockers`):

- its database is SQLite (a `backups` entry with `engine: sqlite`);
- it has `workers` or `services` (a database or a queue beside it would run
  once per copy);
- its container has `volumes`;
- it forwards to an `upstream`, lives in the VPN, `needs` other sites of its
  node, or talks to an AI through the house (`claude_api`, `claude`).

What a site writes into its folder on a replica (an upload) stays there and
is lost at the next copy. Uploads belong in object storage or a database.

## The database gateway

With `gateway`, every node reads and writes **the primary's database**. On
each replica the house listens at the same address (`127.0.0.1:3306`) and
passes each connection through the mesh, as an upgrade on the mesh door, to
the primary, which connects it to that address. The site's configuration
does not change.

- Only server databases: MariaDB/MySQL, PostgreSQL, Redis, anything on TCP.
  Not SQLite.
- Only loopback addresses, only the ones the manifest names, only for the
  nodes that keep a replica of that site.
- A site that runs as a process; a container's `127.0.0.1` is its own.
- Each query from a replica pays a round trip between the nodes (tens of
  milliseconds between providers): fine for a page with a few queries, slow
  for one with fifty in a row.
- If the address is taken on the replica's machine (it has its own MariaDB
  on 3306), the replica reports it and is not used.
- When the primary is down, the replicas have no database; but then no
  request reaches them either, since everything enters by the primary.

## Events

`cluster.placed`, `cluster.sync_failed`, `cluster.overflow`,
`cluster.overflow_end`, `cluster.unreachable`, `cluster.gateway_failed` on
the primary; `cluster.replica_placed`, `cluster.replica_failed`,
`cluster.replica_removed`, `cluster.replica_retired` on the replica's node.

## API

| route | who | what |
|---|---|---|
| `GET /sites/{domain}/cluster` | operator | the site's cluster: blockers, placement, each replica's state and load, whether this node is sharing |
| `GET /cluster` | operator | this node's load and the replicas it keeps for others |
| `DELETE /cluster/replicas/{domain}` | operator, its primary | remove a replica kept here |
| `POST /cluster/status` | nodes | a primary's round: load and replicas, the ones not named removed |
| `PUT /cluster/replicas/{domain}` | nodes | a copy's folder, `.tar.gz` |
| `POST /cluster/replicas/{domain}/commit` | nodes | put it in place (install, swap); answers at once |
