# Quark

One small program that runs your machines. Quark installs and operates
everything a server needs for the apps it hosts: builds, certificates,
secrets, backups, health checks and alerts. Loom is where you see and approve
it all.

Quark is the Go rewrite of Ophelia. The Python 0.6 line is archived on the
`ophelia-0.6` branch and the `ophelia-0.6-final` tag, and stays there.

## Status

0.7 is being built. The plan, the ideas it was chosen from, and the blueprint
behind it live in [docs/](docs/):

- [docs/plan.html](docs/plan.html): the 0.7 plan, milestone by milestone.
- [docs/ideas.html](docs/ideas.html): the 37 ideas, with the 34 that were chosen.
- [docs/blueprint.html](docs/blueprint.html): why the rewrite, and what 0.7 to 1.0 are.

Every milestone is proven on a real machine that is wiped and rebuilt for the
purpose. See [lane/README.md](lane/README.md).

## An app's manifest

An app describes itself in `quark.yaml` at the root of its source:

```yaml
app: hello
description: What this is for
owner: personal
workloads:
  web:
    kind: web              # web, static, worker, cron or release
    build: {context: .}    # or image: ghcr.io/you/hello:1.2
    port: 8000
    routes:
      - host: hello.example.com
        dns: direct        # quark keeps the Cloudflare record (or proxied)
      - host: example.com
        path: /hello       # a prefix; longest wins on a host
    env: {GREETING: hello}
    secrets: [API_KEY]     # names only; values live in quark's store
    health: {path: /healthz}
    resources: {memory: 256m, cpus: 0.5}   # memory defaults to 2g
    tmpfs: [/app/.cache]   # writable in memory; the root filesystem is read-only
    # user: "1000:1000"    # default: the image's user, or nobody for a root image
    # capabilities: [NET_BIND_SERVICE]     # default: none at all
    # writable_root: true  # the explicit way out of a read-only root
    # privileged: true     # for the rare workload that runs containers
  site:
    kind: static
    dir: public            # served by a file server
    routes: [{host: www.example.com}]
  nightly:
    kind: cron
    image: alpine:3.21
    schedule: "0 2 * * *"  # five fields, UTC
    command: [sh, -c, "echo tidy"]
    mounts: [{volume: files, path: /files}]
data:
  postgres: {version: "16"}  # DATABASE_URL reaches every workload
  volumes:
    files: {description: What people upload}
backup:                      # optional; an app with data is backed up
  schedule: "0 3 * * *"      #   nightly at 03:00 UTC (the default)
  drill: "0 4 * * 0"         #   restored and verified on Sundays (the default)
  keep: {daily: 7, weekly: 4, monthly: 6}
  verify: {sql: select count(*) from posts, at_least: 1}
checks:
  - url: https://hello.example.com/
    contains: hello
```

`quark deploy <dir>` confirms DNS, makes the secrets the manifest asks for,
readies the data, builds the images on the machine, runs the release
workloads, starts the new revision beside the old one, runs the checks
against it, switches the edge, waits for certificates, checks again through
the edge and retires the replaced revision, which `quark rollback` can bring
back. Every container is told its `QUARK_APP`, `QUARK_WORKLOAD` and
`QUARK_REVISION`.

An app with several parts, such as Loom's Core, uses a few more fields:

```yaml
app: core
workloads:
  api:
    kind: web
    build: {target: server}      # workloads built alike share one image
    port: 4773
    routes:
      - host: core.example.com
        path: /api
      - host: runners.example.com
        port: 4774               # a route may reach another port
    aliases: [control]           # more names on the app's network; "api" is one
    singleton: true              # never two at once: the old one stops first
    grace: 60s                   # time to stop before it is killed (10s)
    secrets: [API_DATABASE_URL]
  worker:
    kind: worker
    build: {target: server}
    health:
      command: [node, bin/ready.js]   # run inside; exit 0 means ready
      timeout: 5m
    resources: {pids: 2048}
  migrate:
    kind: release                # runs once per deploy, before anything starts
    order: 1                     # lower first, then by name
    build: {target: server}
    command: [node, bin/migrate.js]
    secrets: [MIGRATION_DATABASE_URL]
    timeout: 10m
data:
  postgres:
    version: "17"
    image: pgvector/pgvector:0.8.0-pg17   # default postgres:<version>-alpine
    user: core_admin             # the superuser; default the app's name
    database: core
    init: deploy/postgres        # first-run scripts, from the source
    secrets: [API_PASSWORD]      # given to the first-run scripts
    database_url: false          # no automatic DATABASE_URL
  objects: {}                    # a MinIO of the app's own at {objects}:9000
secrets:
  generate:                      # made once, when missing
    API_PASSWORD: hex:32         # hex, base64 or base64url of N bytes; value:TEXT
  derive:                        # made again whenever a source changes
    API_DATABASE_URL: "postgres://core_api:{API_PASSWORD}@{postgres}:5432/core"
    MIGRATION_DATABASE_URL: "postgres://core_admin:{QUARK_POSTGRES_PASSWORD}@{postgres}:5432/core"
```

Generated and derived values stay on the machine; only their names appear
anywhere. `{postgres}` and `{objects}` are the data services' names on the
app's network, the object store's root user is `MINIO_ROOT_USER` and
`MINIO_ROOT_PASSWORD`, and `{sha256:NAME}` is the hex SHA-256 of a secret.
A release that fails stops the deploy before anything new starts. A
singleton that fails to come up is replaced by its predecessor again. A
backup holds the database, every volume, the object store's files and the
first-run scripts, so a restore on a new machine rebuilds the same roles
before it loads the dump. `quark run --secret NAME` gives a one-off command
one more secret, and `quark run --stdin` hands it this terminal's input
without recording it anywhere.

One source can hold several apps, such as a product and a worker that runs
other people's code apart from its data: `quark deploy <dir> --manifest
quark.worker.yaml` deploys the other one, and `quark secret copy <app> NAME
<other-app>` gives it a secret the first made, without showing it.

## How apps are kept apart

Each app runs as if it were alone on the machine. Its workloads share a
network with each other and its database; the ones that serve traffic also
share a network with the edge, and with nothing else. No app can reach
another app's containers or database, or the edge's admin API, which is a
socket only the machine reaches. By default a workload has no Linux
capabilities, can't gain privileges, runs as its image's user (or nobody,
when the image would run as root), has a read-only root filesystem with
/tmp in memory, and is bounded to 2 GiB and 4096 processes. The manifest
fields above are the explicit ways out, and `quark exposure` shows each
container as Docker runs it: its user, privileges, root filesystem,
networks, published ports and mounts, and anything that shares a network
it shouldn't.

## Deploying without a pipeline

A route with `dns: direct` or `dns: proxied` gets its Cloudflare record from
the deploy, which refuses a record that points at another machine; `quark
dns point <host>` moves one on purpose. Retiring a route, or the app,
removes its record. `quark dns` explains every host; `quark dns audit`
finds records that point here with nothing routed.

`quark git allow <app> <public key>` lets a key push that app and nothing
else. Then, from the app's checkout:

```sh
git remote add quark quark@<machine>:<app>.git
git push quark main        # deploys, and streams every step back
```

A push whose deploy fails is refused, so the branch on the machine is what
runs. `quark deploy . --to quark@<machine>` sends a working tree the same
way, commit or not. `quark git webhook <app> --repo git@github.com:you/app.git`
deploys when GitHub says the branch moved, with a deploy key and secret
quark makes and shows once.

## What quark keeps an eye on

The daemon watches every app and the machine once a minute: containers
running, health paths and checks answering through the edge, certificates
valid and renewing, disk and memory, backups fresh and drills passing, and
any URL added with `quark watch add` (the other machine's sites, say). A
problem has to hold for three rounds before it becomes an alert, one alert
per app, and you hear once when it starts and once when it recovers.
`quark alerts` shows what is wrong now and what was; `quark status` shows
health beside each app's requests, errors, p95 latency, CPU, memory and
disk from the last day; `quark ls` is the registry: owner, hosts,
repository, last deploy and last backup.

Backups go to one bucket per app with restic, encrypted with a password
made once per storage account. `quark backup <app>` runs one now,
Restore the app's original secrets from the sealed machine backup before
restoring data on a new machine. Quark refuses to generate replacement keys
for restored data, which could make encrypted records unreadable.

`quark drill <app>` uses an internal network with no outbound access, starts
all long-running workloads before probing readiness, and never runs release
or cron jobs. Privileged workloads cannot be safely drilled. Workloads that
require external services may fail their drill readiness check.

`quark drill <app>` restores the latest snapshot beside the app, starts the
app on it, runs the verify query and cleans up, and `quark restore <app>`
brings the data onto a machine that doesn't run the app yet, before
`quark deploy`. `quark backup quark` snapshots the machine's own state and
sealed secrets. `quark backups` lists what happened.

The credentials quark itself uses are integrations, kept sealed like any
secret: `quark integration set storage` (Backblaze B2 or any S3 store),
`quark integration set email` (SMTP, for alerts) and
`quark integration set cloudflare`. `quark integration list` shows which
are set and when each was last used, never the values.

## Build

```sh
make check   # gofmt, vet, test
make build   # bin/quark for this Mac
make linux   # bin/quark-linux-amd64 for the machines
```

One static binary, no runtime to install.

## License

Apache 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
