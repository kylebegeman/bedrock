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
