# Bedrock implementation handoff

## Final status

All 12 selected items are implemented in local milestone commits on `next`.
Final executable source is `7f17df1`; its required full checks and both static
Linux builds passed. The live product proof is bound to `39e93a5` and the
live two-machine transfer/recovery proof to `b9c31bf`.
Both exact disposable providers are stopped with disks and evidence preserved.
Final verification, bounded corrections and limitations are recorded below.

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

## Corrected product rerun on `26f465b`

The clean rerun passed the earlier operational assertions plus authenticated
HTTPS deployment and cross-origin credential redirect refusal. It stopped
before any notes/data/release work when the independent storage fixture could
not pull its pinned MinIO tag: the registry returned HTTP 401. Investigation
also found missing `server /data` arguments in that fixture command. Preserve
the report as a failed proof, not evidence of backup or recovery success:
`/Users/kyle/Developer/lanes/bedrock/main/space/tmp/bedrock-product-final-26f465b/report.md`.
A reproducible, independently ready storage fixture is required before rerun.

All 26 local README and feature-document links and anchors resolve after the
setup documentation corrections. Production source remains unchanged by the
proof-fixture corrections.

Both official historical MinIO release binaries were independently downloaded
and verified against their published ARM64/AMD64 SHA-256 values. Their static
ELF architectures were checked. A scratch fixture image using the ARM64
artifact passed an actual local HTTP health check with explicit server/data
arguments. The corrected fixture is being proven independently on the guarded
guest before another complete product run.

The existing production `ObjectsImage` digest also returned HTTP 401; an
identical official Docker Hub mirror could not be verified. Its exact release
provenance is absent from the introducing commit. Preserve the production pin
and track the compatibility-dependent resolution in
`docs/technical/minio-image-availability.md`, linked from README. The disposable
fixture's historical release is not evidence of equivalence to that digest.

The exact corrected storage setup now passes on the guarded Linux guest,
including artifact checksum, running release version, real HTTP health,
repeat setup and credential-output privacy. All 28 lane safeguard tests,
shellcheck and whitespace checks pass. The fixture is ready before the full
product rerun, so registry availability is no longer a hidden prerequisite.

The frozen storage-fixture/documentation snapshot passed `make check`:
formatting, vet, the complete Go suite and 28 lane safeguards. All 30 local
README, lane, feature and technical-document links/anchors resolve. Only this
verification note was added afterward; production source remains unchanged.

## Provider OOM and restart-safe fixtures

The first clean `a399d47` product run failed before app stages because the
source QEMU provider was killed by a confirmed Docker backend OOM (exit 137).
The proof's SSH child exited 255, and the runner preserved a failed report:
`/Users/kyle/Developer/lanes/bedrock/main/space/tmp/bedrock-product-final-a399d47/report.md`.
Exact provider events/state and repair notes are in its `provider-diagnostics`
directory. No product assertion failed in that run.

Both owned providers reserve 3 GiB guest RAM against a Docker backend with
about 7.75 GiB total memory. The idle destination was stopped after its guard
and ownership label passed, preserving its disk and identity. Only the owned
source provider was restarted; no global Docker setting or unrelated process
was changed. After full guard recovery, interrupted upgrade receipt
`op-20261011-000600-nimfh` resumed and succeeded with one interruption.
Its receipt is preserved in `provider-diagnostics/after-restart-readiness.txt`.

The restart exposed a fixture lifecycle defect: retained `lane-minio` had no
restart policy, and create-only setup skipped it, leaving HTTP health unavailable.
The exact storage setup reproduced that failure. Storage and email fixtures
need to start retained containers when setup runs again. The next frozen
milestone will include that correction and focused lifecycle regressions.

For the later two-machine proof, a target-only boot-memory override to 2048
MiB is authorized if measured headroom supports it. Preserve the exact target
container/disk/identity and save original/override bytes and hashes in the
report. This is disposable provider configuration, not a production change.

The corrected storage setup starts the retained container, passes real guest
HTTP health, and succeeds on repeat invocation without printing its password.
Four lifecycle regressions cover retained storage/email startup and explicit
start failures before health or integration registration. All 32 lane tests
pass; isolated copies with the restart fix removed fail all four regressions.
Shellcheck and whitespace checks pass. The target-memory override is prepared
and hashed outside the checkout but has not yet been applied.

The frozen restart-safe fixture snapshot passed `make check`: all 32 lane
safeguards, formatting, vet and the complete Go suite. Only this evidence note
was added afterward. Production Go source is unchanged by this milestone.


## Proof worker shutdown contract

The clean `c33aeac` run passed checkpointed upgrade and the full pinned profile
bootstrap, including identity and encrypted integration transfer. It was
intentionally interrupted after bootstrap completed, before deploying further
fixtures, when an independent shutdown preflight confirmed a fixture defect.
The interrupted report and `interruption.md` are retained at
`/Users/kyle/Developer/lanes/bedrock/main/space/tmp/bedrock-product-final-c33aeac`.
This partial run is not a passing product proof.

Both original BusyBox PID 1 worker loops exited 137 after the actual default
60-second backup grace, without an OOM kill. Bedrock correctly refuses to
capture volume data after that forced shutdown. Trap/background-wait variants
exited zero in 0.060 seconds with a 15-second stop grace. Exact commands,
image digest, container state and ownership-checked cleanup of all four local
containers are preserved in `worker-shutdown-preflight.json` in that report.
All repeating product/transfer fixture workers will use clean signal handlers;
notes retain their marker initialization. Production quiescing is unchanged.


The exact corrected commands extracted from the two frozen scripts passed
real Docker stop probes with the full 60-second grace: notes exited zero in
0.063 seconds, and the simple worker in 0.057 seconds. Notes retained its
original marker and both owned probes were removed. Source-file hashes and
results are in `corrected-worker-shutdown.json`. All eight assembled inline
manifests passed the current CLI, including the appended HTTPS check, with
results in `corrected-inline-manifests.json` in the same report directory.

The corrected frozen scripts passed `make check`: all 32 lane safeguards,
formatting, vet and the full Go suite. Shellcheck and whitespace checks pass.
Only this verification note was added after the check. Production Go source
remains unchanged; full product and two-machine proofs will run from the next
clean committed snapshot.


## First real data proof and restore-counter defect

The clean `6d10cd1` product run passed upgrade/bootstrap, remote approvals,
source preflight, deploy/previews, authenticated HTTPS and cross-origin refusal.
Its real app backup passed. Scratch release rehearsal changed restored data,
left live data and active revision unchanged, and removed scratch resources.
The live incompatible candidate changed the marker; ordinary rollback was
refused and explicit checkpoint rollback restored the original marker.
The fresh app backup and machine backup then succeeded.

Machine backup receipt `op-20261011-004423-eh4ku` captured snapshot
`f195c346b5d09037bd59ef76f4fc9cb1b99a62524aa09b005bfa051f0aee8965`.
Recovery rehearsal receipt `op-20261011-004526-q3dqw` passed fetch, resealing
and recorded-image loading, then failed with `notes restored 8 of 6 files`.
Retain the failed report and transcript at
`/Users/kyle/Developer/lanes/bedrock/main/space/tmp/bedrock-product-final-6d10cd1`.
This is not a passing recovery or full product proof.

Pinned restic v0.19.1 source explains the exact discrepancy: a filtered restore
selects `/data/app-data/notes`, but completes metadata for its two unselected
ancestor directories. Its total counter increments only selected entries;
its completed counter also increments those ancestors. File-count equality
is therefore not a valid integrity gate. The bounded correction will require
restic content verification on restore, retain raw counters for diagnostics,
and rely on verified successful completion plus existing manifest/provenance
checks rather than counter equality. Partial restore and verification errors
must continue to fail, with meaningful regression coverage.

The transfer preflight also found a stale scratch cleanup namespace. MoveTest
uses `bedrock.notes.rehearse.move-test-`, while the proof checked an obsolete
drill prefix. Correct container and volume filters and an added network
assertion will be tested against deliberately present owned scratch resources.

For the two-host proof, the post-product capacity measurement will decide
whether to apply a target-only 1536 MiB boot override. A sample during product
showed about 2.5 GiB available backend RAM, insufficient reserve for the earlier
2048 MiB proposal. Original/replacement bytes, hashes and measured rationale
will remain in external artifacts. Source memory and global settings stay as-is.


Exact fresh-target `--verify` restore of the failed machine snapshot succeeded
with the same eight-versus-six counts and all 671 bytes restored. A repeated
verified restore also succeeded with three skipped files, proving that retry
progress counters are unsuitable for integrity comparisons. The metadata-only
inventory, raw restore summaries and probe scripts are saved at
`/Users/kyle/Developer/lanes/bedrock/main/space/tmp/bedrock-recovery-f195c346-evidence`.
Owned probe resources were cleaned; the original failed workspace is retained.

Shared restores now require `--verify`; recovery and drills retain raw counters
but accept verified successful completion. Regressions reproduce the live
counter discrepancy, reject partial restore/verification errors despite a
summary, preserve credential/container cleanup, enforce recovery metadata and
provenance, and check drill failure receipts and scratch cleanup. The focused
Restic and app suites passed before integration. The corrected snapshot passed
`make check`: 32 lane safeguards, formatting, vet and the full Go suite.

The corrected transfer cleanup assertion passed seven actual local checks:
clean baseline, deliberate stopped-container/volume/network leaks each fail,
and exact ownership-checked cleanup returns to success after each. Evidence:
`/Users/kyle/Developer/lanes/bedrock/main/space/tmp/bedrock-transfer-provider-resources/cleanup-filter-probe-eb7d2810d231/report.md`.
Only this evidence note was added after the full check. Another clean frozen
product proof and the two-machine proof remain required.


## Full product success on `39e93a5`

The full operational product proof passed on clean commit
`39e93a56222dfa3dd7ffe598996a50c1ed7bf1b1`, source tree SHA-256
`3b350ba4afaa3deaa1f86b89ecbabb1015c42b1f36b8e42c1c61ef4627a7ee76`.
Report and independent transcript/receipt review:
`/Users/kyle/Developer/lanes/bedrock/main/space/tmp/bedrock-product-final-39e93a5`.
It proves checkpointed upgrade and pinned bootstrap, identity/connection/source
approval binding, source preflight, preview retention/cleanup, authenticated
HTTPS and cross-origin refusal, real volume backups, scratch release rehearsal,
explicit checkpoint data rollback, exact machine recovery rehearsal, source
fencing and deliberate resume. The independent review found no contradictory
result. Public DNS/CA issuance and cross-host cutover are outside that proof.

The previously failed exact machine snapshot also recovered successfully with
the same recovery ID, retaining its failed receipt and validating scratch
cleanup. Static Linux AMD64/ARM64 builds and Linux ARM64 CLI test compilation
passed. All 32 local documentation links and anchors resolved. Production Go
source has not changed since this passing product snapshot.

## Transfer infrastructure failure and bounded resource repair

The first clean two-machine transfer attempt on `39e93a5` failed during target
host setup, before migration, storage transfer or recovery. The source provider
suffered a confirmed backend OOM at `2026-10-11T01:35:04Z`, exit 137. After the
target transport failed, provider-local inspection established no active host,
apt or dpkg writers and an expired operation lease. Only the positively owned
proof group was stopped at that safe boundary. Preserve the failed report,
client bootstrap journal, bindings and guest receipts:
`/Users/kyle/Developer/lanes/bedrock/main/space/tmp/bedrock-transfer-final-39e93a5`.
This is infrastructure evidence, not a passing transfer proof.

Installed QEMU 8.2.2 reserved approximately 1 GiB of executable translation
cache per provider in addition to guest RAM. Installed help and official
versioned source confirm the `tb-size` property. Exact provider boot overrides
now assign 1536 MiB to source, 1024 MiB to destination, and cap each cache at
128 MiB. They preserve the original boot mode, containers, disks, keys,
identities and provider images. No global Docker setting or unrelated process
changed. Initial live measurements showed 127.9 MiB translation mappings per
provider and 3.79 GiB backend memory available; these are boot measurements,
not peak workload claims. Original/override bytes, hashes, metadata and
capacity evidence are retained under the failed transfer report.

Both refreshed endpoints require byte-only provider-local SSH bridges after
Docker published forwarding failed. The bridges retain pinned host keys,
private client keys, exact endpoints, provider ownership and disposable guards.
Source readiness and supported expired target-operation recovery are in
progress. A new clean-commit transfer run remains required. The old client
bootstrap receipt will remain tied to its original endpoint; a fresh reviewed
bootstrap will be used for the changed endpoint.

The reproducible lane repair adds canonical ASCII integer RAM configuration,
a fixed cache cap, recorded resource metadata, safe reuse of existing settings,
and immutable retained-image inspection. Unknown legacy resource keys are
omitted from generated configs, preserving export-and-reuse behavior. Validation
must precede destructive reset, including retained boot-policy compatibility.
Final review found that reset initially validated retained-image compatibility
after deletion. It now prepares that policy before deletion and reuses the
inspected immutable image ID. Three regressions reproduced the failure first,
then passed with the correction, including a mutable tag changing at deletion.
The final snapshot passed `make check`: 49 lane safeguards, shell syntax and
shellcheck, Python AST, formatting, vet and the full Go suite. Only this
evidence note was added after those checks; production Go source is unchanged.


## Transfer capacity-policy failure on `d104afc`

The fresh two-machine run failed at destination bootstrap verification before
storage transfer, migration or recovery. The retained 1024 MiB destination
reported 0.9 GiB `MemTotal`; doctor correctly refuses less than 2 GiB. Host
setup and both reviewed-binary upgrades succeeded. Preserve the failed report:
`/Users/kyle/Developer/lanes/bedrock/main/space/tmp/bedrock-transfer-final-d104afc`.
The first target setup receipt also recovered successfully under its original
ID, with one interruption, before canonical daemon restoration and this run.

The earlier fresh 1536/1024 MiB recipe is invalid: source `m2` also requires
doctor. This session's retained source was set up and passed product verification
at 3072 MiB before its bounded memory reduction. Legal provisioning RAM is not
proof capacity. Doctor reads guest `MemTotal`, excluding reserved RAM, and swap
cannot satisfy the minimum. The documentation now uses the 3072 MiB defaults
for fresh guests and budgets backend/cache/other-work headroom explicitly.
Production memory policy and runtime are unchanged.

The failed run's 20 resource samples showed no provider OOM, source maximum
RSS 1316.56 MiB, destination maximum 1218.80 MiB, and minimum backend available
memory 2075.10 MiB. A larger destination requires a measured capacity plan,
verification of its actual `MemTotal` and doctor, then a fresh clean-commit run.
The read-only memory preflight now refuses insufficient `m2`/product sources
and transfer destinations before fixture or proof mutation. It records actual
guest memory, allows a smaller already-prepared transfer source, and leaves the
lightweight `m1` proof unrestricted. Malformed or failed probes block safely
without serializing raw output. Seven test-first regressions reproduced the
boundary failures and pass with the correction. The snapshot passed
`make check`: 56 lane tests, shell syntax/shellcheck, Python AST, formatting,
vet and the full Go suite. Only this evidence note was added afterward.

The proof's EXIT cleanup removed the failed fresh client bootstrap journal,
profile and target bindings before they could be copied. Applying-step public
projections, the failed verify receipt ID/transcript, exact binaries and all
guest setup/upgrade receipts remain. This is a retention limitation, not evidence
of client-journal resume. The next artifact collector will preserve applying
public state promptly. Original failed 39e93a5 bootstrap state remains retained.


## Guarded memory-probe timing correction

The new preflight initially blocked both guests because its separate 30-second
probe budget included the entire disposable-machine guard. Other guarded
calls used 60 seconds. With no other change, identical guarded memory probes
succeeded in 34.500 seconds on source and 30.037 seconds on destination. Their
parsed `MemTotal` values were 1450.71 MiB and 950.23 MiB respectively. Evidence:
`bedrock-transfer-final-d104afc/target-memory-preparation/preflight-probe-timing/report.json`
under the retained external artifact root.

The probe now reuses the established remote budget. A modeled 45-second guard
failed before the fix and succeeds afterward; a 65-second probe still blocks
before fixture setup or proof launch. The snapshot passed `make check`: all
58 lane tests, shell syntax/shellcheck, Python AST, formatting, vet and the full
Go suite. Only this evidence note was added after verification. Product runtime
and the inner product/transfer proof scripts remain unchanged from `39e93a5`.

A target-only 2304 MiB boot repair is approved after measured reservation and
capacity analysis. It changes only the reviewed target boot RAM value, retaining
its 0644 mode, disks, container, keys and identity; source remains 1536 MiB and
both translation caches stay capped at 128 MiB. Current measured target
reservation projects 2230.23 MiB usable RAM. Projected backend reserve is
1610.06 MiB at the latest pre-change sample and 795.10 MiB at the earlier sampled
minimum. These estimates require actual post-boot memory, doctor and resource
verification before another full proof. Fresh guests should follow the documented
3072 MiB defaults, rather than this retained-source fixture configuration.


## Retained target RAM repair and guard diagnostics

A subsequent clean `8cfd743` preflight timed out inside the existing source
configuration guard at 60 seconds, before evaluating guest memory. The isolated
unchanged guard later passed in 13.91 seconds; direct local Docker/app inventory
passed in 3.67 seconds. No deadline was extended or unrelated process stopped.
Backend paging had increased, but the measurements do not establish the cause
of the earlier transient delay. Current source guest memory remained healthy
and guest swap had not been used.

The already-approved target-only 2304 MiB repair was applied at
`2026-10-11T02:35:58Z` after a fresh safe-boundary and capacity check. The exact
boot bytes have SHA-256
`77261b4cbcb29e6817c1fd40e305fbe4dc02b4439d47df95cf921fc5bb90794d`
and retain mode 0644. Container, image, disk, keys and identity were preserved;
source binding and 1536 MiB RAM were unchanged. Pre-stop backend available
memory was 3275.17 MiB, with projected post-change reserve of 1995.17 MiB.
The target stopped cleanly and restarted without OOM. Application provenance:
`bedrock-transfer-final-d104afc/target-memory-preparation/target2304-application/application.json`
under the retained external artifact root. Actual usable memory, host doctor
and a fresh clean-commit transfer preflight must pass before full verification.

Read-only guard diagnosis also found a concrete safety defect: `ls --json`
suppressed Docker connection/list failures and omitted unmanaged inventory,
while the disposable guard treated that omission as an empty list. A focused
explicit-evidence correction and failure regressions are in progress. The
next full transfer must use the clean committed correction, with a reviewed
transition for the retained binaries rather than a weakened guard.


## Explicit disposable inventory evidence

The bounded correction adds `unmanaged_known` to `ls --json`, true only after
a successful all-container Docker inventory. Existing app/backup fields and
successful unmanaged-list omission remain compatible. Unavailable Docker
keeps the app listing usable and reports false; human output identifies
unknown inventory without printing raw Docker endpoint or error details.

The guard captures the POSIX producer exit before JSON parsing, validates
shapes and the exact completion boolean with checks unaffected by Python
optimization, and refuses remote Docker connection overrides. Unknown
inventory can pass only for the empty pre-Docker `m1` to `m2` machine, with
explicit absence of the documented commands, sockets, data and service-unit
footprints. Existing files and broken symlinks count as installation evidence;
missing Bedrock cannot bypass an existing Docker installation. Older binaries
without completion evidence require a separately verified disposable transition.

Failure regressions reproduced the old fail-open behavior before the fix.
Six focused CLI tests cover complete and unavailable inventories, retained
app/backup metadata and error privacy; 34 guard/fixture tests cover the POSIX
producer exit, malformed evidence under optimization, fresh provisioning,
installation footprints and connection overrides. `make check` passed on the
frozen corrected source: all 71 lane tests, shell syntax/shellcheck, Python AST,
formatting, vet and the complete Go suite. Independent read-only contract
review found no blocking fresh-setup or inventory regression.

The production delta since the passing `39e93a5` product proof is limited to
`ls` evidence and its human unknown-state message. Deployment, backup, release
and recovery paths are unchanged. The final two-machine run must exercise the
new live CLI evidence and strict guard on both guests after the reviewed
legacy transition. Repeating unrelated product fixtures is not required by
this bounded inventory change. Only this evidence note was added after the
checks; executable source remained frozen.


## Final checksum-bound transitions and transfer preflight

Both retained disposable guests completed separate journaled upgrades to
clean commit `b9c31bf7e08ae58f448ba93df613ceb35c829ba5`. Source receipt
`op-20261011-025630-q25dn` and destination receipt
`op-20261011-025826-37r2z` each retain four successful steps and one
interruption. Installed and canonical daemon bytes match the reviewed Linux
ARM64 SHA-256
`31c640d308f70dd6310f17bc86242deaecd569ef7dc190c4f9582a177488da67`.
An independent read-only review verified the receipts, hashes, public
identities and strict frozen inventory guards. Native input digests are
24 hexadecimal characters; an external verifier's initial 64-character
assumption was corrected by read-only review without replaying an upgrade.
Transition artifacts:
`/Users/kyle/Developer/lanes/bedrock/main/space/tmp/bedrock-inventory-transition-b9c31bf`.

The retained destination's actual `MemTotal` is 2,307,063,808 bytes, above
doctor's 2 GiB minimum. Native doctor passed after the RAM repair and again
after the binary transition. Source remains at 1536 MiB configured RAM and
1,521,180,672 bytes actual `MemTotal`; it was already prepared before this
transfer. Both executable translation caches remain capped at 128 MiB.
These retained configurations do not replace the documented fresh-guest
3072 MiB defaults.

The final transfer preflight passed on the same clean commit, source tree
SHA-256
`f1dad22e795db684593ee9630acc2bfb17897ae73823186cb86f4f50278ba47f`.
The final transfer's retained proof-built Linux and macOS binaries match the
reviewed checksums.
Preflight report:
`/Users/kyle/Developer/lanes/bedrock/main/space/tmp/bedrock-transfer-preflight-b9c31bf/report.json`.
The checkout stayed unchanged throughout the final proof.

## Full two-machine transfer and recovery success

The final runner passed with exit code 0 on the exact clean `b9c31bf`
commit and preflight tree digest above, finishing at
`2026-10-11T03:30:52.279268Z`. The transfer script ran for 1582.597 seconds.
Both installed guest binaries match the reviewed ARM64 checksum.
Report:
`/Users/kyle/Developer/lanes/bedrock/main/space/tmp/bedrock-transfer-final-b9c31bf/report.md`.
An independent read-only review verified the exact binaries, four public
handoffs, native receipt digests, public readiness observations and final
removal/resume ordering. It found no contradictory selected-flow result.

The run proves distinct pinned host identities, recipient-bound shared storage,
destination scratch rehearsal and cleanup, the final source fence surviving
an explicit daemon restart, exact snapshot staging and explicit app cutover.
Final notes move `move-5f0ea009f3f46903` staged under
`op-20261011-031611-zssh5` (12 successful steps), then cut over under
`op-20261011-031711-vkobq` (5 successful steps).
The first destination removal finished before deliberate source resumption.

Fresh notes snapshot
`c77248b42096e8b3f2a518be5bb677cac80b04a36422dc78e02863e5558c49f1`
was embedded in machine snapshot
`1396e54a04fa568024bfab4433a032cb26faa508f276e3bd5c7515affc2d1a49`.
Recovery `lane-transfer-1396e54a04fa5680` completed under
`op-20261011-032216-exvw7` with all 8 steps successful.
Later source fencing captures legitimately have different app snapshot IDs;
they do not replace the earlier machine snapshot's embedded recovery point.

Before cutover, both restored apps were privately ready, imported app history
had no active revision, source container references were cleared, and restored
workloads published no ports. The destination kept its original machine ID,
encryption recipient and existing journal, including primer
`op-20261011-031830-s7d4u` and four earlier anchors.
Applying the source host profile changed its hostname as expected while
preserving the destination machine identity.
Explicit machine cutover `op-20261011-032737-ka2n3` succeeded in all 7 steps
and produced a live recovery report.

Final destination hello removal finished at `03:28:58.626Z` and notes removal
at `03:29:11.487Z`, before source hello resume began at `03:29:21.589Z`
and notes resume at `03:29:33.561Z`.
Both final resume receipts succeeded, the original marker was verified,
destination apps and workers were empty, and source writers were running
with their normal restart policies and no migration fences.

The retained client bootstrap receipt `op-20261011-030501-jmrpv` succeeded
in all 6 steps with zero interruptions. Its terminal private SQLite journal,
exact binaries, target/profile inputs, four public handoffs and refusal logs
were copied before workdir cleanup. Final journal capture
`state-00009-succeeded.db` and its public read-only projection were
independently verified. Early worker projections include timeout gaps; later
relevant captures completed. Sampled observations are non-atomic and do not
provide continuous monitoring. The verdict combines script assertions,
native receipts and independent observations.

The preserver, observer and resource monitor each ended normally with exit 0
after the terminal report. Resource coverage retained 57 samples and did not
hit its 30-minute bound. Sampled provider OOM flags were false; this does not
establish historical or guest OOM absence. Both translation-cache caps and
the reviewed destination RAM boot configuration remained intact.

## Bounded final corrections on `7f17df1`

Independent review found a real recovery-history unit mismatch:
`ImportMachineRecovery` synthesized seconds while the store writes and reads
milliseconds. The regression reproduced January 1970 dates on seven
synthesized fields: interrupted jobs and backups, both interrupted release
states, unresolved incidents, fallback app timestamps and the import marker.
The single assignment now uses the existing `unix()` helper.
The persistent-state test verifies actual public dates, completed-record
preservation, source immutability and unchanged timestamps on same-ID retry.

The source-fence assertion before daemon restart also allowed the restart's
success to mask a failed fence or writer check. The corrected POSIX command
uses errexit and a separately checked Docker producer before inspecting its
output or restarting. Three negative regressions failed on the old command:
missing fence, active writer and failed Docker inventory. All now refuse
before restart, while the accepted fenced/no-writer case succeeds.
Independent observations positively corroborate this completed run's fence
and stopped source writer, so the assertion weakness does not contradict its
factual result.

The four-file correction was independently reviewed without blockers and
committed as `7f17df1ba1555d64387cda7918c79f9e109ec1a7`.
Focused recovery state tests and all four POSIX shell regressions passed.
Final `make check` passed: **75 lane tests**, shell syntax/shellcheck,
Python AST, formatting, vet and the full Go suite.
Static Linux AMD64 and ARM64 builds passed from the clean correction commit:
`/Users/kyle/Developer/lanes/bedrock/main/space/tmp/bedrock-final-build-7f17df1/report.json`.

The full live product proof remains bound to `39e93a5`, and the full live
two-machine proof to `b9c31bf`. The final timestamp/assertion correction was
verified by its focused red/green regressions, the required full check suite
and fresh cross-builds. Those bounded corrections did not require replaying
the unrelated live workflows; they were not installed on the retained guests.

## Separately classified disposable backup history

A bounded read-only after-run probe classified inherited source notes backup
row 15 as terminal unsuccessful at `03:00:20.812Z`, before final proof launch.
Its unique nearby operation `op-20261011-030014-woebg` failed at snapshot
initialization with matching catalog/step errors. Fixed error categories were
restic initialization failure, connection refused and local storage endpoint.
Four subsequent notes backups in the final proof succeeded.

The destination's scheduled machine backup
`op-20261011-033003-sx3jj` also failed at storage initialization after the
proof script removed its temporary shared-storage tunnel. Its fixed error
categories match the inherited source failure. This is separate from the
completed transfer/recovery operations. The later source scheduled machine
backup `op-20261011-033015-r6pi6` succeeded in all 3 steps at
`03:31:34.229Z`, establishing its terminal boundary before provider cleanup.

Classification artifacts:
`/Users/kyle/Developer/lanes/bedrock/main/space/tmp/bedrock-transfer-final-b9c31bf-after-run`.
Catalog associations are supported by target, time and matching error
metadata; there is no direct backup/operation foreign key.
Empty snapshot fields alone do not prove that no snapshot was captured.
Raw errors, operation inputs, step output and private identities stayed
inside the guests; only reviewed metadata and fixed categories were retained.

## Owned provider cleanup and final scope

After independent proof review and confirmation that both guests had no
applying operations, only the two exact owned providers were stopped with
`docker stop`:

- Source `bedrock-lane-c6b1271d5ffbf6fa366973497d676e9b` stopped at
  `03:35:10.512Z`.
- Target `bedrock-lane-94a23d6c5023e8917a2c70168b9cc88a` stopped at
  `03:35:52.273Z`.

Both retained provider postconditions show stopped, PID 0, exit code 0 and
OOMKilled false. Container IDs, images, configurations and guest disk files
were preserved. No down, reset, remove or broad process kill was used.
All task-owned observation/preservation/resource sessions have ended; no
live guest work remains.
The external cleanup helper initially assumed Docker stdout would equal
the full container ID. Its assertion failed after the source had stopped;
a read-only postcondition confirmed completion, and source stop was not
replayed. That capture limitation is preserved in the cleanup evidence.

Cleanup evidence:
`/Users/kyle/Developer/lanes/bedrock/main/space/tmp/bedrock-transfer-final-b9c31bf-after-run/provider-stop-evidence.json`.
Final independent retained-evidence audit verified 22 successful in-run
receipts and 112 successful steps:
`/Users/kyle/Developer/lanes/bedrock/main/space/tmp/bedrock-transfer-final-b9c31bf-after-run/retained-evidence-review.md`.
The separate later metadata classification establishes the source scheduled
backup's terminal success, beyond that audit's earlier sampled observation.
Final platform summary:
`/Users/kyle/Developer/lanes/bedrock/main/space/tmp/bedrock-transfer-final-b9c31bf-after-run/platform-proof-summary.json`.
The external operational handoff is updated; provider disks and evidence
remain available for deliberate reuse.

All selected implementation work is complete. Public DNS/CA issuance, real
Cloudflare lifecycle and historical private fixture proofs remain unverified.
Fresh production MinIO object-image pulls still return HTTP 401; the exact
pin is unchanged because replacement provenance and data compatibility are
unestablished. Its owned follow-up is
[the tracked availability issue](../technical/minio-image-availability.md).
The disposable backup repository used separately checksum-pinned official
binaries and does not establish production object-service equivalence.
No production target was contacted, and no production deployment, publishing,
paid provisioning or external messages occurred.
