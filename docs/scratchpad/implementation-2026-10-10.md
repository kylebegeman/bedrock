# Bedrock implementation handoff

## Session intent and authorization

Kyle authorized end-to-end implementation with milestone commits of the union
of the review's highest ROI, strategic, and quick-win lists, plus item 9.
Selected items: **1, 3, 4, 6, 7, 9, 10, 12, 13, 14, 17, 18**.
Items 2, 5, 8, 11, 15, 16 remain outside this selection.
No publishing, production deployment, or paid VPS provisioning is implied.
Local reversible implementation, schema evolution needed for these systems,
disposable local proofs, and explicit commits are authorized.

## Starting state

- Branch: `next`; HEAD: `cf19f4b`; checkout initially clean.
- `make check` passed before feature changes.
- Go: 1.27.1 darwin/arm64; project targets Go 1.26+.
- Docker Desktop has Linux/aarch64; other VM CLIs absent from PATH.
- Historical lane target became production. Never use its env or reset it.

## Milestones

1. Alerts, diagnostics, offline schema/check, preview cleanup and guarded checks.
2. Disposable real-machine lane and compatibility foundations.
3. Shared named SSH targets, profiles, portable integrations, bootstrap.
4. Whole-machine recovery, data-safe staged moves, and release/checkpoint safety.
5. Integrated real-machine proofs, final hardening and verification documentation.

## Ownership and in-progress files

- `ergonomics_review`: CLI alerts/history, API, kernel output, state operation queries.
- `platform_review`: new schema/check files, generator, generated schema.
- `operations_review`: preview CLI/app, check contexts, manifest, rollout.
- `lane_implementation`: lane, Makefile, CI, CONTRIBUTING.
- Root: remote transport/target config, root CLI registration, bootstrap,
  integration transfer, documentation and milestone commits.
- Only root stages or commits, with explicit file lists. Preserve concurrent edits.

## Decisions and contracts

- Existing receipt JSON remains unchanged; saved output is opt-in.
- Alert history limits never hide an open incident.
- Schema is generated from Go types; no unsupported manifest version field.
- Explicit check contexts separate app behavior, auth guard and authenticated edge checks.
- Named remote targets use pinned SSH host keys, explicit target selection and scoped digests.
- Keep the one-binary architecture and existing serialized operation authority.
- All major workflows must have actual implementations, not placeholder commands.

## Verification and remaining work

Baseline `make check` passed. Workers run focused contracts before integration.
Remote/lane verification must use disposable fixtures, never production.
Real DNS/TLS proof needs dedicated public infrastructure and will be identified
separately if unavailable. Record each milestone hash, tests, and limitations here.

## Milestone 1 verification

Staged snapshot independently materialized with checkout-index.
`make check` passed on that exact snapshot: formatting, vet and all unit tests.
Includes alerts, opt-in diagnostic output and filters, generated schema/offline
check and real deploy-plan host preflight, preview down/prune/pins, credentialed
and anonymous guarded checks, daemon registry and watcher integration.
Named-target work remains outside this first snapshot.

## Milestone 2 verification

Commit `a5a5132`: compatibility/state/upgrade foundations and disposable Linux
lane. Exact staged snapshot passed `make check`, including 26 lane safeguards,
formatting, vet and the full Go suite. Snapshot needed its own Git metadata
because proof-report tests intentionally record repository provenance.

Real m1 proof passed: daemon interruption resumed, six markers, exactly one
interruption and two attempts for the interrupted step. Report:
`/Users/kyle/Developer/lanes/bedrock/main/space/tmp/bedrock-proof-t7qeg71y/report.md`.

Current ownership: ergonomics owns machine recovery; operations owns safe moves;
lane owns release rehearsal/rollback; platform owns product proof; root owns
named targets/bootstrap/integration transfer and central startup/access wiring.
Only root stages and commits.


## Milestone 3 verification

Commit `a409cba`: named SSH targets, portable profiles, encrypted integration
transfer, source/restore-file transport and journaled bootstrap. The exact
staged snapshot passed `make check`, including formatting, vet, full Go tests
and lane safeguards. Snapshot:
`/Users/kyle/Developer/lanes/bedrock/main/space/tmp/bedrock-milestone3-x6hpgq8h`.

## Milestone 4 integration decisions

Recovery, staged moves and release safety are committed together because their
writer reservations, data fences and startup recovery share authority. The
implementation includes self-contained machine recovery with separate private
readiness and cutover; source fencing and exact move handoffs; scratch release
rehearsal; data compatibility boundaries and explicit checkpoint rollback.

Final integration hardening preserves staged/checkpoint images through GC,
refuses consumed checkpoints after later unsafe releases, binds app backups
to revision/manifest/secret provenance, protects direct credential writes,
verifies remote artifact cache integrity, and pins bootstrap integration
ciphertext and SSH settings. Named-target approvals also bind state/socket
overrides. Interactive remote credentials are collected privately on the client.

Only disposable loopback guests are used for live proofs. The first guest's
real host bootstrap became unresponsive during package installation. This is
recorded as infrastructure evidence, never a successful product proof. The
proof worker owns recovery of the specifically identified guest and will test
a final frozen source snapshot. Public DNS/TLS issuance and external/private
fixture proofs remain dependent on explicit unavailable prerequisites.


## Milestone 4 verification

Commit `7ae0003` contains the combined safety workflows and product/transfer
proof runners. The checkout was clean after commit. The exact staged snapshot
passed `make check`: 28 lane tests, formatting, vet and the complete Go suite,
including the regenerated command-help and plan goldens. Snapshot:
`/Users/kyle/Developer/lanes/bedrock/main/space/tmp/bedrock-milestone4-3arxooys`.

Both static Linux builds (`amd64`, `arm64`) and the Linux ARM64 CLI test binary
compile. The independent draft-2020-12 schema checks passed all 8 tests; README
and feature-document local links resolve. Darwin terminal regressions passed.
Final product and two-machine proofs will rebuild from a frozen commit after
disposable guest recovery. No public infrastructure proof is implied by the
local and emulated-guest checks.


## Linux terminal verification

The Linux CLI test binary was run as PID 1 in an isolated, read-only,
networkless container made from the existing lane provider image. This exposed
a test-helper controlling-terminal bug: closing the PTY sent SIGHUP to the test
process. The Linux helper now opens its slave with `O_NOCTTY`. Production
credential code is unchanged. The rebuilt Linux binary passed the three real
terminal regressions plus malformed-input privacy, unchanged credential pipes
and state/socket approval binding. Matching Darwin regressions also pass.

## Milestone 5 verification

Commit `0642a68` contains Linux terminal test hardening and the integrated
verification record. Its exact staged snapshot passed `make check`, including
28 lane safeguard tests, formatting, vet and the complete Go suite. Snapshot:
`/Users/kyle/Developer/lanes/bedrock/main/space/tmp/bedrock-milestone5-jmpu5q62`.

## Final proof investigation and hardening

The original guest's host setup receipt `op-20261010-231314-josu6` succeeded;
the apparent outage was isolated to Docker's published SSH forwarding. Guest
SSH responded immediately inside its positively identified provider container.
A task-specific byte-only SSH ProxyCommand retained the original endpoint,
private client key, pinned host key and all remote disposable guards. No
production lane was contacted. The second guest retains ordinary pinned SSH.
The final report directory includes `transport.md` with the exact limitation.

The clean `0642a68` product run passed checkpointed upgrade, pinned bootstrap,
recipient-bound integration transfer, connection/machine/source approval
refusals, read-only host preflight, deploy and preview retention/cleanup.
It then failed while validating the authenticated fixture because the proof
used `checks[].timeout` instead of the supported `checks[].within` field.
The CLI correctly refused the malformed manifest. Preserve that failed report:
`/Users/kyle/Developer/lanes/bedrock/main/space/tmp/bedrock-product-final-0642a68/report.md`.
The corrected proof must run again from a clean committed source.

The final bounded documentation audit found three corrections: verify fresh
targets after bootstrap, explicitly disable the default daemon socket when
reading the client bootstrap journal, and describe recovered cron admission
only after cutover releases its reservation. These documentation fixes do not
change runtime behavior.

The corrected authenticated fixture validates through the current CLI. All six
product and both transfer inline manifests validate with zero diagnostics:
`/Users/kyle/Developer/lanes/bedrock/main/space/tmp/bedrock-proof-manifest-validation/results.json`.
Lane safeguard checks pass all 28 tests; shellcheck passes with the existing
intentional `SC2016` exclusion. Live product and transfer verification remain
pending for the next frozen milestone.

The frozen corrected proof and documentation snapshot passed `make check`:
formatting, vet, the complete Go suite and all 28 lane safeguard tests. Only
this verification note was added afterward; runtime source is unchanged.
