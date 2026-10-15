# Helios rename and live rollout

Status: **live and verified**, 2026-10-15. Kyle authorized repository rename, publication, production rollout and consumer updates. The earlier engineering-day estimates and proposed approval sequence are superseded by the completed execution below.

## Live result

- Repository: [kylebegeman/helios](https://github.com/kylebegeman/helios). Historical Bedrock repository URLs redirect correctly.
- Release: [Helios 0.7.11](https://github.com/kylebegeman/helios/releases/tag/v0.7.11), built from clean merge `a00813be058fc03b16e09933cd34301370746f1b` through [PR #7](https://github.com/kylebegeman/helios/pull/7).
- Both known live hosts run Helios 0.7.11 in the CLI and daemon API. `helios` and `helios.service` aliases reach the same existing daemon and state as their legacy names.
- All 11 active apps report healthy; both hosts report edge health OK and no open alerts. Corbelo production remains offline with its retained data and sealed bindings.
- Local operator commands are installed at `~/.local/bin/helios`, with a compatible `bedrock` alias. The workspace project is titled Helios.

| Host | Upgrade path | Successful Helios receipt | Final checks |
| --- | --- | --- | --- |
| braintreelabs | 0.7.7 to 0.7.10 to 0.7.11 | `op-20261015-182619-xiw2k` | One active app healthy; four volumes retained; one daemon authority. |
| personal-vps | 0.7.9 to 0.7.10 to 0.7.11 | `op-20261015-182716-dprvr` | Ten active apps healthy; 18 volumes retained; one daemon authority. |

Every upgrade used its freshly inspected exact plan digest and the published Linux AMD64 asset's SHA-256. Each host retained a private recovery bundle with a consistent SQLite backup, previous binary, service/rollback files and local key material before each upgrade. No production dataset or private key was exported. Actual systemd restarts recovered all four upgrade operations to successful receipts and cleared staged markers. Database, WAL, shared-memory and secrets-key modes are now 600.

## Consumers updated

| Project | Completed change |
| --- | --- |
| Office | `helios.yaml`, Helios release helper/commands, preparation scripts and current runbooks; `HELIOS_APP` with legacy fallback. Published to `next`; unrelated frontend and agent-access edits preserved. |
| Packrat | `helios.yaml`, Docker exclusion, CI and current deployment guidance. Published only the naming change to `main`; webhook deployment succeeded. Its existing unpublished local commit remains separate. |
| Homelab | Headscale and Loom Identity default/bootstrap/build manifests and current guidance use Helios. Published to `main`; identity keys, sessions and data unchanged. |
| Corbelo site | Default/staging manifests, release workflow references and guarded runbook use Helios. Published the isolated naming change to `next`; the existing unpublished local work and production hold are preserved. |
| Personal site | Default/test manifests, project data, links, current checks and route tests use Helios. Published to `master`; webhook deployment succeeded. `/projects/helios/` serves the new title and heading. Both `/projects/bedrock/` and `/projects/ophelia/` return 308 redirects to the new route. |
| Switchboard | Certificate-site manifest is `helios.yaml`, published to `master`. Certificate readers keep the existing edge-volume identity; proxy/network services were not restarted. |
| Begamin | `kylebegeman/begam.in` on `main` now has Helios default/test manifests and Docker exclusion. The separate older `begamin` repository is not the live source. |
| Dragon Writer | `kylebegeman/dragon-writer` on its deployment branch `bedrock-manifest` now has `helios.yaml` and `helios.test.yaml`; legacy test filename replaced. |
| Brain Tree Labs | `braintreelabs/braintreelabs.io` on its deployment branch `bedrock-manifest` now has `helios.yaml`. |
| Loom 3D reference | Local manifest and current deployment guidance use Helios; the implementation record notes adoption. This reference folder has no Git metadata, so no repository push is claimed. |
| Dotfiles | The personal VPS wrapper calls Helios; current infrastructure/project guidance is updated and published independently from earlier local work. |
| Atlas, Loom Platform, Iris | Relevant current hosting/project guidance is updated and pushed. Loom Platform's HTML view was regenerated from its source. |
| Corbelo engine | Current hosting-plan product references and the completed Ledger receipt are published independently from existing unpublished implementation work. |

At least 17 consumer manifests are migrated across local and remote sources. Fourteen files in the maintained live source directories were renamed without changing their bytes and passed the actual host's offline checks. Historical captured build trees, saved manifests and receipts remain intact. The personal site's and Packrat's deployment revisions advanced through their existing GitHub webhooks; other active app revisions were preserved during the binary upgrades.

Deployment branches still named `bedrock-manifest` remain valid source-history references. Both webhook routes retain `/_bedrock/hook`; changing a working deployment route is unnecessary for using Helios. AspectAvy's independent `ophelia-edge` stack is outside this software's runtime cutover. Amazon Bedrock provider names are unrelated.

## Compatibility identities retained

Helios continues to use the installed `/var/lib/bedrock`, `/etc/bedrock`, `/run/bedrock` and `/usr/local/lib/bedrock` identities, the existing systemd unit, Docker labels/names/volumes, deployment user/SSH transport, Caddy certificate volume, backup repositories/tags, sealed namespaces, stable machine IDs and historical JSON keys. These are supported compatibility identities. Renaming them would be a distinct storage/transport migration; this release leaves data in place.

New scaffolds write `helios.yaml`; a sole existing `bedrock.yaml` loads. If both defaults exist, select one explicitly with `--manifest`. Selected filenames and source bytes remain bound to deployment approvals. Both environment prefixes work, with conflicting values rejected without exposing them. Workloads receive both metadata prefixes. `backup helios` addresses the existing machine-backup namespace.

The release publishes identical `helios_*` and `bedrock_*` assets for Linux/macOS AMD64/ARM64. Old upgraders can use legacy filenames and repository redirects. New downloads retain their checksum pin when falling back to a historical filename. Both public schema filenames resolve and match generated artifacts; the legacy schema ID remains stable.

## Verification

- Local `make check` passed: gofmt, vet, all Go packages and 80 lane safety/unit tests. Eight independent schema tests passed with the documented uv environment.
- Both PR and merged-commit CI runs passed, including fresh Linux AMD64 daemon-recovery proofs. The merged release run is [38785340853](https://github.com/kylebegeman/helios/actions/runs/38785340853).
- Four release builds have clean VCS stamps, the exact merge revision and CGO disabled. All eight binary checksums passed, four legacy copies are byte-identical, and all nine uploaded assets match GitHub SHA-256 metadata before publication.
- Twelve real-binary CLI/state checks passed against released 0.7.7, 0.7.9, 0.7.10 and the Helios candidate. Three isolated interrupted-upgrade paths passed before the actual four live upgrades.
- Office's 13 release authorization/staging tests and four preparation tests passed; shell syntax and 391-document/6,445-link checks passed. Its Ledger receipt is landed.
- The personal site's suite passed 121 tests, with one existing independent clip-catalog check skipped because its external fixture was not configured. Production build and HTML validation passed. Personal-site and Packrat CI passed after their naming changes.
- Local consumer manifests passed Helios offline checks; the five remote deployment-branch manifests passed the generated schema; all fourteen renamed live source files passed host checks.
- Live HTTP checks verified the Helios page title, heading and canonical URL, plus both 308 redirects. The collaborative browser opened the page with the correct title; its snapshot tool timed out twice, so no successful screenshot inspection is claimed.
- Runtime status reports all active apps healthy, both edges OK, no open alerts and no unfinished migration operations. Sealed namespaces and existing volume identities were retained, including offline Corbelo production.

## Operational record and limits

Protected evidence and repeatable scripts remain in this thread's lane data under `helios-migration`, with private file modes. They include release asset pins, source/consumer commits, host recovery paths, reviewed plans, upgrade receipts, activation metadata, final status and source-rename checks. Temporary binaries and downloaded verification copies are removed after completion; installed operator binaries and host recovery bundles remain.

No physical data-volume migration, credential rotation, public-hostname change or Corbelo production launch occurred. Existing recent backup/drill receipts were inventoried; a new offsite restore and host reboot were not performed during this rename. Both hosts report an OS reboot requirement independently of the successful daemon restarts. Historical release artifacts, immutable recovery material and earlier deployment evidence retain their original names.

The workspace's local directory move preserves an old-path compatibility symlink so existing worktrees and running tools can still resolve their checkout. Existing unrelated edits and unpublished commits in consumer repositories remain intact.

## Source references

- [Compatibility contracts](../features/compatibility.md)
- [Recovery contracts](../features/machine-recovery.md)
- [Source capture](../features/deployment-sources.md)
- [Disposable lane](../../lane/README.md)
- [Manifest discovery](../../internal/manifest/manifest.go)
- [Upgrade and binary rollback](../../internal/host/upgrade.go)
- [Release naming](../../internal/release/release.go)
- [Release record](../scratchpad/release-0.7.11.md)
