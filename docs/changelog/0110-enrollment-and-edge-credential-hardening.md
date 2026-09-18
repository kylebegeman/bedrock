---
id: 0110
title: Enrollment journal observation and in-place edge credential refresh
date: 2026-08-28
status: landed
areas: [daemon, enrollment, execution, edge, credentials, reliability, docs, tests]
change_type: fix
commits: []
---

## Summary

Record the enrollment and edge credential work that landed after 0.6.17.
Enrollment now observes the operation journal before it accepts an identity,
and a failed health check no longer unwinds the enrolled identity. Edge
credential refresh writes a bind-mounted env file in place instead of replacing
its inode.

## Why

Two failure modes were reachable in host operations.

Enrollment validated remote identity without inspecting the local operation
journal, so a host whose journal was invalid or carried another host's identity
could enroll and only fail later. Its rollback was also the wrong shape: a
health check that failed after a successful remote enrollment restored the old
config, removed the written targets and restarted the service, which discarded
a good identity because of a service problem and left the consumed token spent.

Edge credential refresh wrote env files through an atomic rename. A rename
replaces the inode, and a container bind-mounting that file keeps the old one,
so refreshed credentials never reached the running edge.

## Changed Behavior

- Enrollment reports an `operation_journal` observation and blocks on
  `operation_journal_invalid` or `operation_journal_host_identity_mismatch`.
- A failed post-enrollment health check deletes the consumed token, retains the
  enrolled identity and agent configuration, and says so, so the operator
  repairs the service rather than repeating enrollment.
- `_write_private_env` truncates and rewrites an existing env file in place,
  with `O_NOFOLLOW` and an explicit symlink refusal, and only creates a new file
  atomically. Restores go through the same path.

## Changed Areas

- `src/ophelia/daemon/enrollment.py`: journal observation, blockers, retained
  identity on health-check failure.
- `src/ophelia/execution/compose_backend.py`: `_write_private_env`,
  `_restore_private_env`.
- `docs/daemon.md`: enrollment contract.
- `tests/test_daemon.py`, `tests/test_compose_backend.py`.

## Verification

Covered by the suite that ships with release 0.6.18 (record 0111).
