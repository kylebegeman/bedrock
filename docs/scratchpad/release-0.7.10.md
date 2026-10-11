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
