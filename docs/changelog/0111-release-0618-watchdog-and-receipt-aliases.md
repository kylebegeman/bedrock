---
id: 0111
title: Release 0.6.18 watchdog independence and receipt alias resolution
date: 2026-09-18
status: landed
areas: [release, daemon, systemd, reliability, client, docs, tests]
change_type: fix
commits: []
---

## Summary

Publish Ophelia 0.6.18. The systemd watchdog heartbeat moves to its own loop:
the daemon previously sent `WATCHDOG=1` at the end of each reconciliation pass,
so a pass that ran longer than `WatchdogSec`, or one that raised, starved the
heartbeat and systemd killed the process with SIGABRT. A read through
`DaemonClient` now also waits out the restart that follows. Receipt resolution
gained the alias lookup its discovery layer was already building, so a legacy
identifier resolves.

This release also publishes the work recorded in 0109 and 0110, which had
landed but not shipped.

## Why

A host under deployment load reconciles while an external caller drives long
synchronous work through it. Three watchdog kills were recorded on a production
host, each during deployment activity: 2026-09-10 05:57 and 07:50, and
2026-09-17 22:32. Memory was stable near 80-105 MB each time, so this was
starvation rather than a leak, and systemd restarted the daemon about five
seconds later.

The 2026-09-17 kill broke a caller. A Loom production deploy preflight ran
22:30:56 to 22:32:46 UTC and its closing `ship daemon status --json` landed
inside the 22:32:18 to 22:32:44 restart window. It returned
`daemon_unavailable`, which failed the whole preflight under `set -e` even
though every manifest plan was clean and the host was serving normally.

## Changed Behavior

- A dedicated `watchdog` loop sends `WATCHDOG=1`. Its cadence is half the
  `WATCHDOG_USEC` systemd supplies, so one delayed ping is not fatal, and it
  starts only when systemd asked for a watchdog and scoped it to this process.
- The reconciliation loop still reports `STATUS=`, which is informational, and
  records a monotonic stamp after every pass, including a pass that raised. A
  loop that keeps failing is still a live loop and is reported through
  `loop_errors` rather than by being killed.
- The watchdog withholds its ping once reconciliation has not completed a pass
  for `watchdog_stall_seconds` (default 900). A wedged daemon is still restarted
  by systemd; only a slow one is spared. `WatchdogSec` itself is unchanged,
  because the limit was never the problem.
- `DaemonClient` retries a read for up to `reconnect_seconds` (default 8) while
  the socket is missing or refusing, which covers a systemd restart. A write is
  never replayed: a connection lost mid-request gives no proof the write did not
  land.
- Receipt discovery already recorded the identifiers earlier releases used for
  the same evidence, as `aliases`, but nothing read them. Reference resolution
  now matches an exact alias, after an exact id and before a prefix guess, and
  reports an ambiguous alias the same way it reports an ambiguous prefix. A
  legacy `verify_id` therefore resolves.
- Every drill and verification lookup reports one name for a receipt: its
  `verify_id` or `restore_drill_id` when it has one, and the resolved id
  otherwise. The by-path and by-record lookups previously disagreed.

## Changed Areas

- `src/ophelia/daemon/service.py`: `_watchdog_loop`, reconciliation records
  progress and no longer owns the ping.
- `src/ophelia/daemon/systemd.py`: `watchdog_ping_seconds`.
- `src/ophelia/daemon/config.py`: `watchdog_stall_seconds`.
- `src/ophelia/daemon/client.py`: bounded reconnect for reads.
- `src/ophelia/operation_refs.py`: `_record_aliases`, alias resolution strategy.
- `src/ophelia/restore_verification.py`: `_restore_drill_display_id`, alias
  matching in the record lookup.
- `tests/test_daemon.py`: watchdog interval, watchdog loop, and client
  reconnect coverage.

## Verification

- `tests/test_daemon.py` and `tests/test_daemon_recovery.py`: 47 passed.
- New coverage: pings continue through a pass blocked for 30 seconds, well past
  a one minute `WatchdogSec`; pings stop once stalled past the limit so systemd
  can act; a pass that raises still records progress; a read waits for a socket
  that appears mid-restart; a read still fails once its budget is spent; a write
  is never replayed.
- Full suite: 887 tests, all passing. The two failures present on the branch
  before this release, `test_restore_drills_cli_resolves_legacy_verification_id`
  and `test_list_and_show_surface_receipts_without_secrets`, were the unwired
  alias lookup and are fixed here rather than deferred.

## Adopter Notes

Loom pins Ophelia by exact version and commit through `OPHELIA_INSTALL_SPEC`.
Adopting this release needs a new tag and a matching pin bump in that
repository's deploy workflow and manifests.

## Known Issues

`test_operation_store.SQLiteOperationJournalTests.test_concurrent_first_open_migrates_once_across_processes`
is flaky on macOS at roughly two runs in ten. Four processes first-open the WAL
journal at once and two are killed by SIGBUS (exit -10) with no output. It is
not introduced here: it reproduces on v0.6.17 with no local changes, and
`mmap_size` is already 0, so the mapping involved is WAL's `-shm` file, which
SQLite maps regardless. Deployment hosts are Linux and it has not been observed
there. It is recorded rather than fixed because the cause is below this
codebase, in SQLite's shared-memory handling on this platform, and diagnosing
it is its own piece of work.

