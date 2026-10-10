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
4. Whole-machine recovery and data-safe staged moves.
5. Release rehearsal/data compatibility, integrated proofs and final docs.

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
