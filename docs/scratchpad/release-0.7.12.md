# Helios 0.7.12

This patch release stops one stuck backup from holding a machine's operations.
On 2026-10-15 a scheduled backup on a 0.7.9 host spent about three hours in
`restic forget --prune`. Retention ran with no deadline while the backup held
the daemon's operation lock and the machine-wide `locks/operations.lock`, so
every later backup, drill and deploy waited behind it.

## Operator changes

- Every restic repository call a backup makes now has a limit: opening or
  creating the repository 5 minutes, clearing stale locks 5 minutes, the
  snapshot upload 2 hours, forgetting old snapshots 15 minutes and the prune
  that follows 1 hour. A machine backup's copy of each data app's pinned
  snapshot has a 2 hour limit per app, and its workload image archive 1 hour.
  An app backup's capture and upload stay inside the existing 30 minute
  maintenance window. A call that runs out of time is stopped with a clear
  error. The restic helper container then gets a one minute stop grace,
  matching restic's own lock-release delay, and is removed. When the daemon
  itself is stopping, the grace stays 5 seconds so cleanup, including removal
  of the helper's credentials file, fits systemd's 60 second stop budget.
- Retention is now two calls: `restic forget`, then `restic prune
  --max-repack-size 2G`. Every backup prunes, even when it forgot nothing this
  time, so space left behind by a prune that was stopped is still reclaimed.
  One prune rewrites at most 2 GB, so a repository with a lot to reclaim
  catches up over several backups instead of starting over each time.
- When retention fails or runs out of time after a snapshot was taken, the
  backup is recorded as successful and keeps its snapshot. `helios backups`
  and the operation output show "retention deferred" with the reason, and so
  does `helios status <app>` for an app's last good backup. The overview of
  `helios status` does not show it, and a machine backup's deferral appears
  only in `helios backups`. The operation finishes and releases the lock, and
  the next backup applies retention again.
- When every backup of an app, or of the machine, has deferred retention for
  over 3 days, the watcher raises a warning (in the app's alert, or as
  "machine backups") with the last reason. It clears once retention completes.
- Before each capture on an existing repository, Helios runs plain
  `restic unlock`. It removes only locks restic itself considers stale, which
  includes a lock left by a prune that was stopped by hand. It never uses
  `--remove-all`, so a lock held by a live restic process is never forced.
- The daemon's watcher raises a "machine operations" warning when one
  operation has held the machine's operation lock for over 4 hours. The
  threshold is a heuristic: an app backup's limits add up to about 3 hours 25
  minutes plus cleanup, but a slow machine backup on a host with several data
  apps can legitimately cross it. The alert names the operations still
  running and resolves after the lock is released, through the usual alert
  rounds and email notices.

## Upgrade compatibility

No state migration and no schema change. A deferred retention is an ordinary
successful backup run with a note in its existing detail column. Backup plan
step names and descriptions are unchanged, so a 0.7.11 backup
interrupted by the upgrade still resumes under 0.7.12 instead of failing its
plan check. The lock holder now writes when it took the lock into
`locks/operations.lock`. Earlier releases only lock that file and ignore its
contents, and a lock taken by an earlier release reads as undated and is not
alerted on.

## Validation before release preparation

- `gofmt -l` over every tracked Go file reported nothing, and `go vet ./...`
  passed.
- `go test ./...` passed. `go test -race` passed for `internal/restic`,
  `internal/app`, `internal/api`, `internal/watch` and `internal/daemon`.
- New tests cover each repository call stopping at an injected deadline
  against a helper that never exits: init, unlock, backup, forget, prune and
  a bounded restore, each under a safety context so a missing deadline fails
  instead of hanging the suite. They also show that a caller's cancellation
  or its own deadline (such as the maintenance window) is not reported as the
  call's deadline, that only a call's own deadline gets the long stop grace,
  that the prune runs even when nothing was forgotten, and that unlock never
  forces live locks. App and machine backups whose forget hangs or fails, or
  whose prune hangs, finish as successful runs with the snapshot recorded and
  the deferral explained. The next backup runs retention again and clears the
  note. A daemon shutdown during retention is still an interruption to
  resume, not a deferral. A machine backup stops a hung copy of an app's
  pinned data at its deadline. The operation lock reports its holder and
  acquisition time, reads as free once released, and treats an earlier
  release's undated lock as held; the watcher never alerts on an undated
  lock. A watcher test opens a "machine operations" incident past the
  threshold, mails it, and resolves it after the lock is released, and
  another reports retention deferred on every backup for over 3 days and
  clears it once retention completes.
- The real-Docker restic test (`BEDROCK_DOCKER_TESTS=1`) passed against the
  pinned restic 0.19.1 image, including `forget` followed by
  `prune --max-repack-size 2G`.
- Static Linux AMD64 and ARM64 builds passed with `CGO_ENABLED=0` and
  `-trimpath`.
- `scripts/build-release.sh` built all four platform artifacts and
  `SHA256SUMS` into a temporary directory from the committed tree. It
  publishes nothing.

The lane tests (`make check`, `lane-up`, `lane-proof`) were not run for this
patch. CI and the release process own them.

## Known boundaries

- A lock left by a restic call that couldn't release it in time is not stale
  for 30 minutes, because restic runs as PID 1 in every helper container.
  A manual backup in that window can fail on the lock; the next scheduled run
  clears it. Helios does not unlock right after a stopped call, because a
  fresh lock would not yet be removed.
- A prune whose fixed work (reading the index and every snapshot's trees)
  takes longer than an hour still never finishes, whatever it would repack.
  The repository then grows, and the retention alert says so after 3 days;
  an operator has to run the prune by hand or move the repository.
- Waiting for the operation lock is still unbounded. An operation queued
  behind a stuck one waits. The new alert makes the stuck holder visible.
- Drill and restore repository calls have no new limits. A stuck drill or
  restore is reported by the operation lock alert. A machine backup's local
  work other than the image archive, such as copying the state database, has
  no new limit.
- Rollout to personal-vps and braintreelabs (both on 0.7.11) is recorded
  below once it happens.
