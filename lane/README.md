# The lane

The lane is a real machine that gets wiped and rebuilt for every feature, so
a green check means "this works on a real server", not "the unit tests pass".

**The lane has no machine right now.** `hostinger.env` still points at the
Hostinger box, and that box became production: it runs the personal sites,
begamin, Dragon Writer and Loom Core. `reset.sh` refuses to touch a machine
that is carrying apps, so the lane cannot run until `hostinger.env` points
somewhere disposable. A second cheap VPS, or a Linux VM on the Mac, both
work; nothing else in the lane depends on which it is.

## What you need

- The [Hostinger CLI](https://github.com/hostinger/api-cli) on your PATH,
  with an API token in `~/.hostinger.yaml` (`api_token: ...`, mode 600).
  Create the token in hPanel under Profile, then API.
- The deploy key that Hostinger has on file for the machine. `hostinger.env`
  expects it at `~/.ssh/bagel-box-deploy`; set `BEDROCK_LANE_KEY` to use another.

## Commands

```sh
lane/reset.sh --status   # the machine's state, nothing changes
lane/reset.sh            # wipe it, wait, print "ready"
```

`reset.sh` reinstalls Ubuntu 24.04 through Hostinger's API, waits for the
recreate action to succeed, asks Hostinger to install the deploy key on the
fresh OS, waits for SSH, and pins the new host key in `lane/known_hosts`
(ignored by git). Everything after that is bedrock's job.

Recreating the machine deletes everything on it, including snapshots.

Before doing any of that, `reset.sh` asks the machine what it is carrying and
refuses to recreate one with apps on it, printing their names. A machine that
does not answer, or has no bedrock on it, is fine to take. To destroy one
that does answer, name it exactly:

```sh
BEDROCK_LANE_DESTROY=203.0.113.10 lane/reset.sh
```

Naming the host is deliberate. The lane box quietly became the production
host while `hostinger.env` still pointed at it, which left `make lane-reset`
one command away from reinstalling the OS under five live apps.

## Proofs

Each proof is a script that runs bedrock on the machine and checks the
result from the outside. They build the binary, install it, and print what
they see; a failure says `NOT proven` and stops.

```sh
lane/prove-m1.sh              # the kernel: journaling and recovery after a kill
lane/prove-m2.sh              # the host: setup, a maintenance reboot, an upgrade rolled back
lane/prove-m3.sh              # deploys: certificates, a second revision, a rollback, a failing check
lane/prove-m4.sh <staging>    # secrets, cron, one-off commands, begamin and Dragon Writer from their archives
lane/prove-m5.sh              # backups to per-app buckets, a drill, a restore, signals, one email per outage
lane/prove-m6.sh [begamin]    # git push deploys begamin with its records, no app reaches another, dns point, a webhook
lane/prove-m7.sh              # a Core-shaped app: made secrets, releases, singletons, an object store, back from its bucket
```

`prove-m5.sh` needs the apps `prove-m4.sh` leaves behind. It runs two
fixtures on the box that bedrock doesn't manage: a mail sink (mailpit on
127.0.0.1:1025 and :8025) and an S3 store (MinIO on 127.0.0.1:9000), so the
email and storage integrations have something to talk to without any real
credential leaving the machine. On the real machines the same integrations
point at a mail provider and Backblaze B2.

`prove-m6.sh` clones begamin's repository from this Mac (its argument, by
default `~/Developer/active/begamin`) into a temporary directory and commits
a lane manifest there; nothing is pushed anywhere but the box. It runs a
stand-in for Cloudflare's API on the box (`lane/fixtures/fakeflare`, on
127.0.0.1:8788), and makes a throwaway SSH key for the pushes.

`prove-m7.sh` deploys `lane/fixtures/notes`, an app shaped like Loom's
Core: pgvector's image with its own superuser and first-run scripts, an
object store, two release workloads, one image for two workloads, and a
singleton worker whose health goes through the API's alias. It breaks the
worker and a release on purpose, then backs the app up, drills it, removes
it with its data and restores it from the bucket. It needs the storage
integration `prove-m5.sh` sets up. Loom's own Core is M7's other half:
`pnpm loom core create` from the Loom repository, against
core.lane.begam.in.
