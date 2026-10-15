# Helios 0.7.11

Bedrock is now Helios. This patch introduces the Helios command, module, release
assets, documentation and default `helios.yaml` manifest while preserving the
existing installation, data and recovery identities.

Existing `bedrock` commands, `bedrock.yaml` manifests, `BEDROCK_*` environment
variables and published asset names remain supported. Both commands operate the
same daemon and state. If both default manifest files exist, select one explicitly
with `--manifest`. Renaming a manifest requires a fresh deployment plan/digest.

The GitHub repository is `kylebegeman/helios`; historical repository URLs redirect.
Legacy schema identifiers and historical release artifacts remain available.
Physical state paths, Docker resource names, backup namespaces and secrets retain
their existing identities so this release does not relocate data or redeploy apps.

## Verification

The source rename passed `make check`, eight schema tests, four platform builds,
installer/checksum boundary checks, and release packaging identity checks.
Twelve isolated real-binary CLI/state checks passed. Interrupted upgrades from
0.7.7 and 0.7.9 to 0.7.10, and from 0.7.10 to the Helios candidate, resumed to
successful receipts with staged markers cleared. These container proofs used a
simulated restart boundary; real Linux daemon recovery is additionally required
by release CI. Live results are recorded after rollout.
