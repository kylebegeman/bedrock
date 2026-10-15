# Bedrock 0.7.10

This release adds safer machine operations, named SSH targets, portable bootstrap,
whole-machine recovery, staged app moves and data-aware release workflows. It
includes the implementation and the subsequent comprehensive audit.

## Operator workflows

- Run context-aware manifest checks, inspect operational diagnostics and history,
  and create, pin and prune previews without bypassing plan approval.
- Save named SSH targets, upload approved source captures, and resume bootstrap
  from a reviewed portable profile with encrypted integration transfers.
- Recover a machine from recorded images and backups, rehearse recovery on
  isolated data, and cut over only after destination readiness checks.
- Stage an app move before cutover, persist the source write fence across
  restarts, and deliberately resume a source after destination writers stop.
- Rehearse candidate releases on scratch-restored data, retain checkpoints for
  incompatible data changes, and refuse an ordinary rollback when it would
  expose incompatible data.

## Audit improvements

Approved deployments use captured source and immutable image identity. Build
paths and Dockerfiles stay inside the captured context. Jobs and backups have
owned identities, bounded deadlines and recovery receipts. Daemon shutdown,
SQLite durability, private sidecar permissions, Git and credential workflows,
DNS reconciliation and IPv4/IPv6 firewall reconciliation are hardened.

Correction: the deadlines covered jobs and an app backup's maintenance window.
A backup's other restic repository calls, including retention, had no limit
until [0.7.11](release-0.7.11.md).

## Validation before release preparation

The complete `make check` suite passed, including 78 lane tests. Race checks
passed across the app and affected packages. Linux AMD64 and ARM64 static
builds passed, and the native ARM64 netfilter proof passed for both address
families. The final audit's source is `bc8f111`; later commits document the work.
See [the audit record](audit-2026-10-11.md) for commands and evidence.

The release process additionally requires the pull request's check and
fresh disposable Linux daemon-recovery jobs, then checksum verification of
all four version-stamped release artifacts built from the merged commit.

## Known boundaries

Fresh object-service provisioning remains blocked where the pinned MinIO image
cannot be pulled. The pin remains unchanged because its provenance and a
replacement's existing-data compatibility are not established. See
[the dependency record](../technical/minio-image-availability.md).

Earlier full product and two-host recovery proofs are historical evidence;
they were not rerun for the audit. Public DNS/CA issuance, the real Cloudflare
lifecycle and historical private fixture proofs remain unverified. No rollout
of this release to the historical production host is included.

## Completed merge, publication and disposable rollout

[Pull request #6](https://github.com/kylebegeman/bedrock/pull/6) merged into
`next` at `09fc9e63c1e3bddc9e5b5f52d642e7e3780ea698`, preserving the
implementation and audit milestone commits. Release preparation added a
separate notes commit and fixed the CI workflow's invalid job-level
`runner.temp` reference by initializing lane state through `$GITHUB_ENV` in a
step. Actionlint reproduced the original failure and passed the corrected
workflow.

[The PR CI run](https://github.com/kylebegeman/bedrock/actions/runs/38139402575)
passed both jobs: formatting, vet, tests and cross-builds, plus a fresh AMD64
Ubuntu daemon-recovery proof. The proof's source-tree SHA-256 exactly matches
the merged release checkout:

```text
6eb0e5fda3d1517f630bc322e9ecb688a73e2a7e8d5dc3be0e7de928ff174b14
```

[Bedrock 0.7.10](https://github.com/kylebegeman/bedrock/releases/tag/v0.7.10)
was published with four Linux/macOS AMD64/ARM64 artifacts and `SHA256SUMS`.
They were built from the clean merge commit with Go 1.27.1. Every artifact's
platform and clean VCS revision was inspected; both Linux builds are static.
The native macOS build reported 0.7.10 and passed a manifest-check smoke test.
All five draft assets were downloaded and matched byte-for-byte to the local
reviewed files and GitHub's SHA-256 metadata before publication. Existing
published releases and assets were not overwritten.

The retained ARM64 source guest was verified against its exact saved provider
ID and ownership label, pinned SSH host key and private disposable marker.
A provider-local byte relay worked around the established broken published
SSH forwarding while preserving SSH host verification. Its Docker inventory
was readable, held only lane fixtures, and had no applying operations.

The guest fetched the public 0.7.10 ARM64 asset with the reviewed SHA-256 pin:

```text
1a93168b2b3650c5044b71ae8ae55bca8e2fc8bc311b6ad07e4cdc65b8eb9b62
```

The reviewed upgrade plan was applied using its exact digest. Operation
`op-20261011-123838-a7ml7` succeeded with one interruption across
the daemon restart. Both the CLI and the daemon API reported version 0.7.10
and commit `09fc9e63c1e3`. The upgrade retained a state checkpoint and cleared
its staged marker. The live database and checkpoint passed SQLite
`quick_check`; the database, WAL, shared-memory file and checkpoint were
root-owned with mode 600. Both fixture apps retained their revisions,
container names and running state, and no applying operations remained.

After verification, the daemon was stopped, the absence of applying operations
was checked directly in the state database, filesystems were synced and the
guest powered off cleanly. Its exact provider exited with code 0. Both retained
disposable providers are stopped; their disks, keys and evidence are preserved.
The target provider was not started or upgraded. No historical production host
was contacted, and no public DNS, paid provider or deployment infrastructure
was changed.

Rollout evidence, release artifacts, downloaded copies, CI proof reports and
upgrade receipts are retained outside the repository at:
`/Users/kyle/Developer/lanes/bedrock/main/space/tmp/bedrock-release-20261011`.
This is a verified disposable binary upgrade and state-preservation proof;
the full product and two-host proof boundaries described above still apply.
