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
    kind: web              # web, static or worker
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

`quark deploy <dir>` builds the images on the machine, starts the new
revision beside the old one, runs the checks against it, confirms DNS,
switches the edge, waits for certificates, checks again through the edge
and retires the replaced revision, which `quark rollback` can bring back.
Every container is told its `QUARK_APP`, `QUARK_WORKLOAD` and
`QUARK_REVISION`.

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
