# Bedrock to Helios migration plan

Status: initial source rename implemented locally on `feat/helios-rename`; source inventory and isolated binary compatibility proofs complete; full host rehearsal and operational migration remain pending. Inspected 2026-10-15. No live runtime changes, releases, repository rename, consumer edits or production writes have been performed.

## Recommendation

Make Helios the product name, command, repository and manifest convention through a compatibility release first. Migrate existing machines and consumers individually, then complete the physical runtime-name migration in separate maintenance windows. Reliability comes from old and new callers operating the same machine state under one writer authority throughout the transition.

The final scope includes the software, Go module, GitHub repository, releases, local tooling, app YAML files, running daemons, host configuration, Docker resources, external automation and recovery workflows. Historical releases, encrypted recovery material and approved receipts retain their original identities; compatible readers make them usable after the rename.

Kyle authorized publication, repository rename, live rollout and consumer updates on 2026-10-15. Execute the verified compatibility release and live cutover now. The phase estimates below are superseded; retain the technical checks as execution criteria, not separate approval gates. Physical storage identifiers can remain stable compatibility identities while Helios becomes the live product and command.

## Current source implementation

Kyle authorized starting with the rename itself after the assessment. This first implementation prepares the public software name while preserving the installed runtime identities. It does not apply the baseline upgrade or physical migration ahead of their gates.

Implemented locally:

- The module is `github.com/kylebegeman/helios`, with primary `cmd/helios`, `cmd/helios-schema`, `bin/helios` and Helios version/help output. `cmd/bedrock` and the native build's legacy binary remain compatibility entrypoints.
- Scaffolding writes `helios.yaml`. Loading and offline checks discover a sole legacy `bedrock.yaml`; two defaults require explicit selection. Launch refuses either existing manifest. Init detects the legacy file, and forced writes cannot follow a symlink outside the source.
- Source capture and recovery use the selected manifest. Renaming the file changes the deploy approval. Named-target deploy/check forward the client's selection explicitly, including to supported older daemons. The older `--to` transport keeps its legacy wire request; a new filename through that transport needs a daemon that understands it.
- `HELIOS_*` configuration aliases work alongside the old variables. Conflicts fail without printing values, unless the corresponding explicit flag overrides that setting. Workloads receive both metadata prefixes. Machine backup commands accept `helios` while preserving the existing backup namespace.
- Release packaging builds four platforms and emits identical `helios_*` and `bedrock_*` assets with checksums for all eight files. Downloads support historical filenames and new filenames. A pinned download falls back only on a missing new asset, with the same pin; checksum failures and other HTTP errors cannot bypass verification.
- Both editor-schema URLs are generated from the same validator. Historical JSON keys, machine identity, paths, units, Docker labels/names, secret identities, DNS ownership markers and backup tags remain compatible.
- Current documentation, asset wiring, fixture manifests, lane linker stamps and generated help/scaffold snapshots use Helios. Lane safety refuses either production marker. Historical records retain their original naming.

Verification record:

- Final `make check` passed, covering gofmt, vet, Go tests and 80 lane safety/unit tests, including the named-target forwarding correction.
- The independent schema suite passed all eight tests. Both generated schema artifacts match their generators.
- A clean synthetic packaging checkout built Linux/macOS AMD64/ARM64, verified all eight checksums and four identical legacy copies, and reported clean VCS stamps. Its version `0.7.0` was a test fixture, not a release. Native CLI smoke checks validated both manifest names. Final source cross-builds also passed for all four targets with CGO disabled and output discarded after the build.
- Mocked installer boundaries passed new-name and historical-name downloads, wrong-pin refusal and conflicting-pin refusal. The generated remote script passed shell syntax validation; no remote script executed. Shellcheck passed for both release scripts.
- All 718 existing JSON/YAML struct tags were checked for preservation, along with the persistent identifiers in nine runtime contract files. The report passed wide/narrow layout checks and all 18 source links resolve.
- Generated native binaries and packaging/installer fixtures were removed after verification in response to the machine's storage reserve notice. The lane cleanup tool was used. The pre-existing Linux ARM64 artifact and checkout were preserved.

Remaining gates include additional external-controller confirmation, full disposable-host rehearsal, supported live baseline upgrades, operational old/new-client proof, GitHub and workspace directory cutover, publishing, consumer updates and physical runtime migration. The isolated binary proofs below do not establish full host behavior. No production rollout was performed.

## Starting point and evidence

- The starting checkout was clean on `next` at `16c2ba4`. A successful fetch confirmed zero commits ahead or behind `origin/next`; only one worktree exists.
- The initial assessment used `plan/helios-rename`. Implementation now uses `feat/helios-rename` from the same tip. Neither branch has been pushed.
- The published release is `v0.7.10`, dated 2026-10-11. Its production rollout was explicitly excluded from the release record. Two earlier disposable provider containers were confirmed stopped. Their saved ownership/SSH configuration and prior release evidence are unavailable in the expected location; they have not been adopted or restarted.
- This repository has 400 tracked files; 327 contain the name, totaling 3,694 case-insensitive occurrences. Many are Go imports, fixtures and generated help. These are scope indicators, not 3,694 independent changes.
- History has 297 commits from April through October. Current runtime seams deserve more attention than older Python hotspots. The earlier rename at `df92076` changed 122 files but explicitly left host migration outside that commit.
- Known-host inspections used existing pinned SSH trust and read-only binary, systemd and Docker metadata queries. Follow-up inventory projected operational identity, backup/drill receipts, source, job and operation metadata through read-only SQL with query-only enabled. Names-only secret listings were collected. No secret values, key material or application records were retrieved. No backups, drills, reconciliation or upgrade commands were run on live hosts.
- There is no local `bedrock` or `helios` executable on PATH and no default `~/.bedrock` target store. `/Users/kyle/.local/bin/personal-vps` resolves to the wrapper in `/Users/kyle/dotfiles/bin/personal-vps`.
- GitHub returned 404 for `kylebegeman/helios`. This does not reserve the name or guarantee availability when the rename occurs.

### Live hosts verified

Names here are existing SSH aliases. Addresses, credentials and private network configuration are intentionally absent.

| Host alias | Observed runtime | Observed footprint |
| --- | --- | --- |
| personal-vps | Bedrock 0.7.9, `cf19f4b8a14c`, Linux AMD64; daemon active | 10 app identities, 18 running containers including edge/registry, 18 named Bedrock volumes |
| braintreelabs | Bedrock 0.7.7, `39692069d9d8`, Linux AMD64; daemon active | One app, three running containers including edge/registry, four named Bedrock volumes; one exited restic helper |

The personal host runs Office, Loom Identity, Loom 3D, the personal site, protected Corbelo staging, Packrat, Switchboard TLS, Headscale, Begamin and Dragon Writer. The other host runs its namesake site. Container inventory is a point-in-time observation, not proof of health, registry completeness or successful backups.

The personal host also retains `bedrock-corbelo-product-data` while Corbelo production is offline. A registry-only inventory would miss this data. Preserve its sealed bindings, backups and production hold; never recreate its routes or activate its live webhook during this migration.

Both hosts reported mode 644 for the existing state database and 600 for the secrets key. The 0.7.10 release records stronger database/WAL/checkpoint permissions. Track permission convergence in the baseline upgrade and verify actual files afterward; no live permission change was made here.

### Consumer inventory

These are local working copies, not a complete inventory of remote branches or all hosted source. The original 11 local manifests plus the resolved sources below account for at least 17 relevant manifests, including named and test variants. These filenames need review even where their contents contain no occurrence of the old name.

| Consumer | Concrete work | Coordination |
| --- | --- | --- |
| Office | `bedrock.yaml`; `scripts/release/bedrock.py`, run/verify/smoke helpers; deploy preparation scripts; `BEDROCK_APP` in the entrypoint; database TLS SAN and network name `bedrock-office-postgres` | Dirty checkout has unrelated work. Use its release harness and preserve existing edits. Deploying application code is a separate step from changing runtime names. |
| Packrat | `bedrock.yaml`; deployment docs, Docker exclusions, webhook-driven deploy and backup references | A push to `main` deploys production. Prepare a branch without triggering it. |
| Homelab | `headscale/bedrock.yaml`; `loom-identity/bedrock.yaml`, `bedrock.bootstrap.yaml`, `bedrock.build.yaml`; runbooks and host commands | Identity and network access are dependencies of other apps. Preserve encryption keys, passkeys, sessions and the root SSH break-glass path. |
| Corbelo site | `bedrock.yaml`, `bedrock.staging.yaml`; release workflow comments, runbook and agent guidance | Local `next` is ahead 23 commits. Production is explicitly held offline; staging stays protected. Retained Ophelia release fields are an existing API contract. |
| Personal site | `bedrock.yaml`, `bedrock.test.yaml`; Docker exclusions, build/deploy docs, project route data, static links and live-verification tests | Add redirects from `/projects/bedrock/` and retain the older Ophelia redirect chain. Preserve content checks while the canonical route changes. |
| Loom 3D reference | `bedrock.yaml`; README, deployment instructions and current implementation handoff | Live plugin remains in this reference checkout while new product work moves elsewhere. Coordinate the migration with its owner. |
| Switchboard | `deployment/certificate-site/bedrock.yaml`; `deployment/certificate-export.py`, `controller/src/network-main.ts`, related tests and routing docs | Reads Caddy certificates directly from `bedrock-edge-data`. Change the reader before relocating certificate storage. Avoid interrupting the proxy that may carry active agent streams. |
| Dotfiles | `bin/personal-vps`, server shell helpers, SSH references, agent allowlists and infrastructure/project docs | Dirty repository, ahead one commit. Preserve unrelated edits. Inspect settings through targeted reads; never dump credential-bearing configuration. |
| Loom Platform, Atlas, Corbelo engine, Iris roadmap | Hosting guidance, architecture references, project links and deployment records as appropriate | Update current guidance. Keep dated evidence intact. Corbelo has unrelated local work; Loom Platform is ahead two commits. |

Amazon Bedrock provider references in app-kit, Loom and generated provider contracts are unrelated and must retain that name. Vendored/reference trees, caches and archived worktrees are excluded from implementation scope. Historical release logs and ledger receipts are evidence, not files to mass-rewrite.

### Resolved deployment sources and controllers

| Consumer | Authoritative source and observed deployment |
| --- | --- |
| Begamin | `kylebegeman/begam.in`, default `main`, with `bedrock.yaml` and `bedrock.test.yaml`. Live source is `/srv/begam.in`, recorded deployed commit `ff6f4fc8d4d2`. The separate older `begamin` repository is not this deployment's source. |
| Dragon Writer | `kylebegeman/dragon-writer`, branch `bedrock-manifest`, with `bedrock.yaml` and `quark.test.yaml`. The live `/srv/dragon-writer/bedrock.yaml` matches that branch by SHA-256. Default `next` has legacy Ophelia manifests, so inspecting only the default branch misses this consumer. |
| Brain Tree Labs | `braintreelabs/braintreelabs.io`, branch `bedrock-manifest`. Its `bedrock.yaml` matches `/srv/braintreelabs/bedrock.yaml` by SHA-256. The default branch lacks this manifest. |
| Switchboard TLS | Local `switchboard/deployment/certificate-site/bedrock.yaml`; deployed source `/srv/switchboard-certificate-site`. Keep certificate lookup changes separate from relocating edge storage. |

Two active GitHub push webhooks were confirmed: the personal site on `master` and Packrat on `main`. Both retain `/_bedrock/hook` without query parameters. GitHub and deployment metadata use different historical personal-site repository spellings; record the API's resolved repository before consumer work. No Git push keys are configured on either host: the names-only CLI encodes its empty key list as JSON null. Root crontabs had no matching Bedrock, Quark or Helios lines. The matching top-level systemd files are the existing daemon and rollback units. These bounded checks do not cover every external automation account.

The existing pinned `aspectavy-vps` alias has no standard Bedrock, Quark, Helios or Ophelia binary or matching unit. Its project has separate `ophelia-edge` staging Compose references. Treat it as an independent stack; do not migrate that network as an incidental runtime rename.

### Recovery and retained-state inventory

At collection, neither live host had unfinished operations or jobs. All eight active stateful apps had recent successful backup receipts, approximately 3 to 15 hours old, and successful drill receipts, approximately 22 to 110 hours old. Machine-backup receipts were approximately 11 to 14 hours old. These are recorded results, not a new proof of offsite recoverability.

Corbelo production remains offline. Its retained volume, backup/drill records and ten sealed bindings remain present. Names-only inventories include the offline namespace; the migration must preserve the production hold. Protected lane evidence retains the host projections, source hashes, GitHub metadata and exact release pins outside public source. No secret values or production datasets were copied into fixtures.

### Isolated real-binary compatibility proof

Published Linux ARM64 binaries 0.7.7, 0.7.9 and 0.7.10 were verified against GitHub release asset SHA-256 metadata. The current Helios source was built as `0.7.11-dev` solely for testing; this does not reserve or release that version. Reports retain the candidate binary hash, Go source fingerprint, dirty-tree status and starting commit. The 0.7.10 Linux AMD64 release asset digest and URL are also recorded for later rollout preparation; no AMD64 rollout occurred.

The 12-check CLI/state proof passed. Actual old binaries and the Helios candidate wrote synthetic operations to the same default `/var/lib/bedrock`. The 0.7.10 state-version migration preserved earlier history; all three older binaries could still read history after a Helios write. Stable machine identity, canonical/legacy manifest discovery, ambiguous-default rejection, explicit selection and environment aliases/conflict rejection passed.

| Interrupted upgrade | Observed recovery |
| --- | --- |
| 0.7.7 to 0.7.10 | Binary switched; real new daemon resumed the interrupted operation to a successful receipt and cleared the staged marker. |
| 0.7.9 to 0.7.10 | Same successful journal recovery with one recorded interruption. |
| 0.7.10 to Helios candidate | Same successful recovery; state checkpoint metadata remained present. |

All three plans used `stage`, `switch`, `restart`, `verify`. Checkpoint creation is inside 0.7.10's `stage` step. Each operation was approved with its exact plan digest; no digest or journal safeguard was bypassed. A fixture `systemctl` shim killed the upgrading process at restart, then the actual new daemon recovered it. An initial fixture failure was Docker's non-executable tmpfs at the installed binary path; explicitly allowing execution in that fixture resolved it without a product-code change.

Tests ran in capped, network-disabled Linux ARM64 containers with a read-only root, synthetic machine identity and tmpfs state. Each exact owned test container was removed. This proves binary switching and journal recovery across the simulated interruption. It does not prove real systemd restart, reboot, rollback, live AMD64 behavior, application containers, backup restore, or old/new HTTP clients. Prior green Go/schema/build checks remain applicable; no product source changed during this inventory/proof packet.

### Remaining prerequisites before a live baseline upgrade

- Re-establish a positively identified disposable Linux/systemd guest with retained ownership metadata, pinned SSH trust and sufficient storage headroom. Host free space has fluctuated around the 40 GB reserve, so do not provision a large guest at this margin. Temporary compatibility binaries have been freed through the lane lifecycle tool; small reports/scripts remain in protected lane data.
- Run the observed-version-to-baseline matrix and baseline-to-Helios forward/reverse paths on that guest, including actual restart, reboot, rollback, single-writer ownership and permission convergence. Rehearse data services and backup/restore with synthetic data and available pinned images. The earlier release proof is not a substitute for a retained report of this candidate.
- Confirm any additional external controllers, private installer pins and users, then verify the exact selected manifests and GitHub webhook settings immediately before consumer changes. Do not push webhook deployment branches while preparing rename-only changes.
- Establish the host-specific recovery bundle, verified offsite snapshot/restore path, exact AMD64 binary/checksum and maintenance/observation window before proposing a live plan. Refresh backup/job/operation metadata at that time. Roll out the site-only host first if its recovery gate passes.

## Compatibility contracts to design first

| Surface | Required transition behavior |
| --- | --- |
| Product, Go module and binaries | Use Helios and `github.com/kylebegeman/helios` for current source, commands, linker stamps and new documentation. Provide a temporary `bedrock` command entrypoint to the same implementation. Test scripts that parse plain version output separately from JSON. Old source tags retain their original module paths. |
| Manifest discovery | New scaffolds write `helios.yaml`; load an existing `bedrock.yaml` when it is the sole default. If both exist, fail with an explicit-file instruction rather than guessing. Preserve `--manifest` for named variants. `init` and `launch` must detect either existing default before creating a file. |
| Source capture and approvals | Resolve the selected filename consistently across check, deploy, Git push, webhook, launch, preview, remote upload/cache, rehearsal and recovery. Bind that filename and its bytes into approvals. Existing `--to` handling rejects alternate manifests, so changing the filename constant alone is insufficient. Re-plan after any filename or source change; never reuse a prior digest. |
| Daemon, state and lock | Both command names must reach the same daemon, database and machine lock. On existing installations, retain the legacy layout initially. Never fall back into creating a second empty state directory. If both layouts exist, refuse ambiguous ownership and require explicit migration. Compatibility checks must cover old CLI/new daemon and new CLI/old daemon, including unsupported commands. |
| Host paths and services | Inventory `/var/lib/bedrock`, `/etc/bedrock`, `/run/bedrock`, `/usr/local/lib/bedrock`, daemon/rollback/web-guard units, systemd directories, Docker drop-ins, SSH settings, unattended updates, journald, fail2ban and the deploy user/group/home. Stage a single-authority service handover with a matched binary/unit/rollback bundle. Do not enable two schedulers. |
| Environment | Add `HELIOS_*` configuration names with documented legacy support. Reject conflicting explicit settings. Retain `BEDROCK_APP`, workload and revision variables for old consumers during the transition. Keep the stored `BEDROCK_POSTGRES_PASSWORD` identity readable; a rename must not generate a new database password. |
| Docker and application DNS | Discover existing labels, names, volumes, networks, database/object services, registry images and edge layout. Use explicit recorded identity for existing installations. Do not globally match/delete either prefix. Reserve both product names. Office TLS and any app-supplied URLs require stable DNS aliases until clients and certificates migrate. |
| Edge, certificates and firewall | Preserve Caddy certificates/configuration and registry contents. Support old and new hook paths during transition. Keep Cloudflare ownership comments recognizable. A firewall migration must preserve the effective guard in both address families and across Docker/host restarts; change chains without opening an access gap. |
| Git and SSH | Support old forced commands, `bedrock receive`, existing `bedrock.branch`, repository hooks and remote-user paths until converted. Preserve deploy-key restrictions and permissions. A new user must not widen app authorization. Local Git remote names can be changed after transport verification. |
| Backups, secrets and recovery | Preserve the age key, recipient, integration namespace, storage password, configured bucket prefix and existing repositories. Read old restic tags/checkpoint tags and machine catalogs. The reserved `bedrock` machine-backup target must remain addressable through the new command. Prove old snapshots can restore; do not create a new empty backup namespace. |
| Machine identity and JSON | Preserve stable machine IDs, including the historical fallback hash domain in `compat.go`. JSON keys named `bedrock` appear in receipts, bootstrap profiles and recovery catalogs. Use compatible readers or a deliberate versioned format, never a silent key replacement. Preserve stored receipts, digests and captured historical manifests byte-for-byte. |
| Releases, GitHub and schema | Old upgraders request `bedrock_VERSION_PLATFORM` from the old repository URL. Publish a bridge asset under the legacy filename before relying on a Helios-only release. New clients must resolve appropriate old/new assets without weakening checksum verification. Keep old schema paths available through generated compatibility artifacts; verify raw URLs, tags, badges, release downloads and Go resolution after the GitHub rename. Do not assume redirects cover every endpoint. |
| Lane safety | Migrate VM/provider ownership labels, paths, fixtures, scripts and environment names deliberately. Continue refusing `/etc/bedrock-production` and support the new production marker. A rename must never make a production target pass a disposable-target guard. |

Persist only the identity/layout metadata actually needed by these contracts, following the existing architecture. Whether that requires a state-schema change is an implementation decision to review explicitly before making one. Existing upgrade reader ranges prohibit pretending an incompatible state migration is an ordinary binary replacement.

## Delivery sequence and gates

### 1. Complete inventory and recovery preparation

Complete the missing source/automation inventory, resolve ownership and baseline versions, and define the exact compatibility matrix. Prepare a names-only migration record per host, including offline retained data. Establish a verified recovery path with existing identities and pinned images before proposing a live window.

Exit gate: every known app and retained resource has an owner, backup/recovery plan and consumer list. Any additional external destination becomes a tracked row. No production changes occur in this phase.

### 2. Establish a separate Bedrock baseline

Rehearse 0.7.7 to the current supported Bedrock patch and 0.7.9 to that patch on disposable machines. At inspection, the candidate baseline is 0.7.10. Older production versions lack newer capabilities and recovery commands; do not assume those commands work before the upgrade.

Prepare exact pinned binaries, plans, fallback units and recovery evidence. After rollout authorization, upgrade one host at a time in a separate change from the rename. Verify state permissions, history, data services, jobs, routes and backup behavior. A newer baseline introduces behavior changes of its own, so investigate regressions before proceeding.

Exit gate: both hosts run a verified baseline, or a specifically tested older-host path is documented. Existing source and resource names still work.

### 3. Implement and release the compatibility bridge

Add dual manifest discovery, command/environment compatibility and explicit handling of the durable contracts above. Keep existing runtime identities on existing hosts. Prepare the renamed module, build targets, generated schema/help and release tooling without moving live data.

Publish the bridge through the existing Bedrock release channel under legacy asset names once authorized. Use the next unused 0.7.x patch; 0.7.11 is only a candidate, not a reserved version. Keep application version, API version, state version and recovery format separate. Later releases also default to patch bumps.

Exit gate: supported old clients can fetch/install the bridge; both command names and both manifest conventions operate the same state. Unsupported combinations fail clearly. Immutable historical releases and checksums remain unchanged.

### 4. Prove upgrades and bridge rollback on disposable hosts

Extend the existing lane with interrupted-upgrade, conflicting-layout, old/new manifest, dual-command, lock-authority, backup/restore and rollback scenarios. Run the observed-version-to-baseline and baseline-to-bridge matrix before either reaches production. Prototype the physical layout transition here to refine the estimate; its final workflow and full forward/reverse proof are a gate inside phase 7.

Use old encrypted fixture state, existing data-volume markers, pinned images, fake credentials and retained certificates. Prove jobs/backup pauses resume safely after a crash and old saved recovery artifacts are readable. Test failed bridge installation and interrupted upgrade recovery as well as success. Do not copy production datasets into fixtures.

Exit gate: a retained report demonstrates the exact candidate commit, migration path and reverse path. Fresh provisioning has also been checked. Missing external prerequisites are recorded as blocked proof scopes, never passing evidence.

### 5. Rename the repository and roll out Helios compatibly

Prepare the GitHub rename, remotes, release URLs, schema compatibility files and project directory move as separate reviewable actions. Publish required files before switching consumers to their URLs. Recheck repository-name availability immediately before renaming. Preserve old tags and old download usability.

Choose the first live host by dependency and recovery readiness after inventory. The site-only host is a likely candidate because the personal host carries identity, networking and many apps. Rehearse its older-version path first. Use pinned checksums and approved plans, verify the daemon build and state continuity, then observe it before the personal host. Initially preserve existing resource names and application revisions; a branding release should not force every app to redeploy.

Exit gate: Helios operates both known hosts, old callers still work, no duplicate daemon/scheduler exists, and a full scheduled monitoring/backup cycle has been assessed. Perform any explicit backup/drill within its approved maintenance window.

### 6. Migrate consumers in independent changes

Change manifest filenames, explicit `--manifest` callers, Docker exclusions, schema annotations, CI/release tools, wrappers, current docs and project links per consumer. Keep app identity, image pins, credentials, environment policy and data declarations stable. Validate actual selected files before any push or deployment.

Update Switchboard certificate lookup and Office environment/TLS compatibility before changing their underlying resources. Add the personal-site redirect before changing canonical project links. Avoid side effects from push-triggered deployments. Respect Corbelo's production hold and protected staging. Coordinate dirty/ahead checkouts; never reset, stash or commit unrelated edits.

Exit gate: every known active consumer uses the new convention, release tooling can plan/apply the intended digest, webhook and SSH deployments work, and outstanding external consumers remain covered by compatibility support.

### 7. Complete physical runtime naming per host

Build one resumable, reviewed migration workflow rather than distributing manual rename commands. Inventory old/new resource identity and checkpoints before applying it.

Stop new operations, drain managed writers, complete or safely recover in-flight work, stop the old scheduler, and create a consistent checkpoint. Move host state/configuration and update service/rollback paths under one authority. Preserve ownership, permissions and key material. Do not move a live SQLite database while ignoring its WAL.

Docker volumes have no ordinary rename operation. Moving to Helios volume names requires an explicit offline copy or restore into new volumes, with ownership, file metadata and data checks. Container labels and network identity require recreation. Keep certificates, registry content, app DNS/TLS aliases and backup repository mappings available throughout. Re-plan deployment/recovery operations against their changed resource identities.

Migrate app groups in dependency order established by the inventory. Avoid coupling identity/network changes to database moves and app releases. Retain the original volumes until validated copies and the rollback window are accepted. Do not copy active PostgreSQL files or start two servers on a shared volume. Any database schema change is separate scope.

Exit gate: active host paths/services/resources use Helios names; remaining old names are documented historical identities or compatibility interfaces. Restart/reboot, restore, drill, deploy, rollback, garbage collection, authorization and public-route checks pass against the new layout. Corbelo's offline data remains offline and recoverable.

### 8. Retire temporary compatibility deliberately

Observe usage and verify that no active callers require old command, manifest or webhook names. Remove temporary aliases only after known consumers, retained branches and recovery runbooks have been checked. Keep historical backup/catalog readers, stable machine IDs, original release artifacts and useful URL redirects for as long as they are needed.

Exit gate: an explicit compatibility-retention list explains every remaining Bedrock reference. No automated zero-string-count criterion overrides recoverability.

## Rollback and operational limits

- Before physical migration, roll back with the tested matching binary, service and layout bundle. Retain both published asset naming paths. Never restore a stale database automatically merely because a newer binary failed.
- After physical migration, use the migration journal to reverse the layout under a stopped writer authority. A binary downgrade alone cannot reverse moved volumes, changed labels, paths or schemas.
- Once new application writes have reached migrated storage, switching to the old copy would discard data. Fence writers and reconcile or restore using an explicitly reviewed data-loss boundary. Never silently choose the old volume because it still exists.
- A control-daemon restart can leave app containers serving, but edge replacement, database moves, certificate storage moves and consistent backups may interrupt service. Measure copy times in rehearsal and schedule the actual maintenance window; promise no zero-downtime physical rename.
- Preserve root SSH access throughout. Do not rotate keys, change public hostnames, open Corbelo production, alter database schemas, or replace infrastructure as incidental rename work.

## Verification plan

For documentation/evidence updates: verify source references, document links, whitespace, the rendered HTML at wide/narrow widths and final worktree scope. The source implementation and isolated proof results are recorded above. No application suite repeat is needed when only documentation and protected evidence change.

During implementation:

- Use focused Go behavior tests for ambiguity, source binding, legacy formats, discovery, authority/locks and failure recovery. Regenerate schema with `go generate ./internal/manifest` and goldens through the packages' existing update flags, then review diffs.
- Run `make check` as required by repository guidance: lane safety checks, gofmt, vet and Go tests. Cross-build Linux and macOS AMD64/ARM64 release artifacts. Run schema validation as documented in `schema/README.md`.
- Extend and run the relevant `m1`, `m2`, `product` and two-host `transfer` proofs. Use the DNS/Cloudflare/backup scopes when their actual prerequisites are available. Do not require unrelated scopes merely to repeat a prior green result.
- Verify old/new downloader and checksum combinations, actual GitHub redirects/raw schema URLs, clean-host bootstrap and upgrade from every observed production version. A renamed binary that compiles does not establish any of these contracts.
- Per consumer, run its focused release/deploy checks. Office needs release-harness and TLS/DNS checks; Switchboard needs certificate lookup checks; the personal site needs route/redirect checks. Validate every renamed manifest with the supported binary and editor schema.
- After each authorized host change, compare app/revision/data identity, daemon/API version, single-writer ownership, scheduled work, key recipient, permissions, routing/authentication, firewall persistence and backup/restore results. Retain sanitized receipts and exact candidate checksums outside public source.

## Tracked findings and handoff

| Finding | Owner and next action |
| --- | --- |
| Production runtimes lag the published patch; observed database mode is 644 | Bedrock/Helios baseline work: prepare and prove the upgrade; verify database, WAL, shared-memory and checkpoint permissions after an authorized rollout. Do not treat this report as rollout approval. |
| Current direction guidance needed reconciliation with delivered workflows | Resolved in the source packet in `docs/direction.html`; preserve older blueprint and dated audit history. |
| Known object-service image has a documented fresh-pull availability problem | Existing issue in `../technical/minio-image-availability.md`: establish a reproducible image source for affected proofs. Do not substitute a database/object-service image during the rename without its own compatibility assessment. |
| Previously missing live app sources resolved; external-controller coverage remains bounded | Source branches and file hashes are recorded above. Confirm additional external users/controllers before scheduling physical migration or removing compatibility. |
| Several consumer repositories have uncommitted or unpublished work | Consumer owners: isolate rename changes on their correct base, inspect remote freshness at implementation time and preserve all unrelated work. |

Next implementation packet: restore safe disposable-host prerequisites and run full systemd/reboot/rollback and restore rehearsals for the pinned baseline and Helios candidate. Isolated CLI/state and interrupted-upgrade proofs are complete; real-host and external recovery gates remain open. Keep baseline upgrade, public naming changes, consumer changes and physical data migration in separately reviewable changes. Each phase supplies the evidence and concrete plan needed for the next live action; this planning request does not authorize production writes or publication.

## Source references

- [Release 0.7.10 record](../scratchpad/release-0.7.10.md)
- [Current compatibility contracts](../features/compatibility.md)
- [Recovery contracts](../features/machine-recovery.md)
- [Safe moves](../features/safe-moves.md)
- [Source capture](../features/deployment-sources.md)
- [Disposable lane](../../lane/README.md)
- [Manifest discovery and reserved names](../../internal/manifest/manifest.go)
- [CLI state fallback](../../internal/cli/root.go)
- [Remote deploy manifest restriction](../../internal/cli/app.go)
- [Systemd units](../../internal/daemon/systemd.go)
- [Upgrade and binary rollback](../../internal/host/upgrade.go)
- [Machine identity](../../internal/compat/compat.go)
- [Docker identity](../../internal/docker/docker.go)
- [Edge identity and hooks](../../internal/edge/ensure.go)
- [Data-service identity](../../internal/app/data.go)
- [Release naming](../../internal/release/release.go)
- [Bootstrap profile format](../../internal/bootstrap/profile.go)
- [Machine recovery format](../../internal/app/machine_recovery_catalog.go)
