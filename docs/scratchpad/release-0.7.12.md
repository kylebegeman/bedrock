# Bedrock 0.7.11

This patch release stops one stuck backup from holding a machine's operations.
On 2026-10-15 a scheduled backup on a 0.7.9 host spent about three hours in
`restic forget --prune`. Retention ran with no deadline while the backup held
the daemon's operation lock and the machine-wide `locks/operations.lock`, so
every later backup, drill and deploy waited behind it.

## Operator changes

- Every restic repository call a backup makes now has a limit: opening or
  creating the repository 5 minutes, clearing stale locks 5 minutes, the
  snapshot upload 2 hours and retention 1 hour. An app backup's capture and
  upload stay inside the existing 30 minute maintenance window. A call that
  runs out of time is stopped with a clear error. The restic helper container
  gets a one minute stop grace, matching restic's own lock-release delay, and
  is then removed.
- When retention fails or runs out of time after a snapshot was taken, the
  backup is recorded as successful and keeps its snapshot. `bedrock backups`,
  `bedrock status` and the operation output show "retention deferred" with
  the reason. The operation finishes and releases the lock. The next backup
  applies retention again, so a deferral catches up on its own.
- Before each capture on an existing repository, Bedrock runs plain
  `restic unlock`. It removes only locks restic itself considers stale, which
  includes a lock left by a prune that was stopped by hand. It never uses
  `--remove-all`, so a lock held by a live restic process is never forced.
- The daemon's watcher raises a "machine operations" warning when one
  operation has held the machine's operation lock for over 4 hours, longer
  than a backup's limits added together. The alert names the operations still
  running and resolves after the lock is released, through the usual alert
  rounds and email notices.

## Upgrade compatibility

No state migration and no schema change. A deferred retention is an ordinary
successful backup run with a note in its existing detail column. Backup plan
step names and descriptions are unchanged, so a 0.7.9 or 0.7.7 backup
interrupted by the upgrade still resumes under 0.7.11 instead of failing its
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
  against a helper that never exits: init, unlock, backup and forget. They
  also show that a caller's cancellation stays a cancellation, and that unlock
  never forces live locks. App and machine backups whose forget hangs or fails
  finish as successful runs with the snapshot recorded and the deferral
  explained. The next backup runs retention again and clears the note. A
  daemon shutdown during retention is still an interruption to resume, not a
  deferral. The operation lock reports its holder and acquisition time, reads
  as free once released, and treats an earlier release's undated lock as
  held. A watcher test opens a "machine operations" incident past the
  threshold, mails it, and resolves it after the lock is released.
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
  clears it. Bedrock does not unlock right after a stopped call, because a
  fresh lock would not yet be removed.
- Repeated retention deferrals are visible in `bedrock backups` and
  `bedrock status`, but they raise no alert of their own. The repository
  grows until retention succeeds.
- Waiting for the operation lock is still unbounded. An operation queued
  behind a stuck one waits. The new alert makes the stuck holder visible.
- Drill and restore repository calls have no new limits. A stuck drill or
  restore is reported by the operation lock alert.
- The fix has not been rolled out to the affected host. That host should
  upgrade from 0.7.9 through the reviewed upgrade plan.
