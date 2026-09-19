# The lane

The lane is a real machine that gets wiped and rebuilt for every milestone,
so a green check means "this works on a real server", not "the unit tests
pass". Today it is the Hostinger box (`hostinger.env`). Later it can be a
Linux VM on the Mac.

## What you need

- The [Hostinger CLI](https://github.com/hostinger/api-cli) on your PATH,
  with an API token in `~/.hostinger.yaml` (`api_token: ...`, mode 600).
  Create the token in hPanel under Profile, then API.
- The deploy key that Hostinger has on file for the machine. `hostinger.env`
  expects it at `~/.ssh/bagel-box-deploy`; set `QUARK_LANE_KEY` to use another.

## Commands

```sh
lane/reset.sh --status   # the machine's state, nothing changes
lane/reset.sh            # wipe it, wait, print "ready"
```

`reset.sh` reinstalls Ubuntu 24.04 through Hostinger's API, waits for the
recreate action to succeed, asks Hostinger to install the deploy key on the
fresh OS, waits for SSH, and pins the new host key in `lane/known_hosts`
(ignored by git). Everything after that is quark's job.

Recreating the machine deletes everything on it, including snapshots. The
script never asks; the caller decides.

## Proofs

Each milestone has a script that runs quark on the machine and checks the
result from the outside. They build the binary, install it, and print what
they see; a failure says `NOT proven` and stops.

```sh
lane/prove-m1.sh              # the kernel: journaling and recovery after a kill
lane/prove-m2.sh              # the host: setup, a maintenance reboot, an upgrade rolled back
lane/prove-m3.sh              # deploys: certificates, a second revision, a rollback, a failing check
lane/prove-m4.sh <staging>    # secrets, cron, one-off commands, begamin and Dragon Writer from their archives
lane/prove-m5.sh              # backups to per-app buckets, a drill, a restore, signals, one email per outage
lane/prove-m6.sh [begamin]    # git push deploys begamin with its records, no app reaches another, dns point, a webhook
```

`prove-m5.sh` needs the apps `prove-m4.sh` leaves behind. It runs two
fixtures on the box that quark doesn't manage: a mail sink (mailpit on
127.0.0.1:1025 and :8025) and an S3 store (MinIO on 127.0.0.1:9000), so the
email and storage integrations have something to talk to without any real
credential leaving the machine. On the real machines the same integrations
point at a mail provider and Backblaze B2.

`prove-m6.sh` clones begamin's repository from this Mac (its argument, by
default `~/Developer/active/begamin`) into a temporary directory and commits
a lane manifest there; nothing is pushed anywhere but the box. It runs a
stand-in for Cloudflare's API on the box (`lane/fixtures/fakeflare`, on
127.0.0.1:8788), and makes a throwaway SSH key for the pushes.
