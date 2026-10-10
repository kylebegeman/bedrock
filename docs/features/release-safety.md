# Release rehearsal and data-safe rollback

A release workload can write to a database or volume before a deployment fails.
Stopping its container does not undo those writes. Bedrock records that boundary,
tests candidates against isolated restored data, and refuses to run older code
against data whose compatibility is unknown.

## Rehearse a candidate

```sh
bedrock rehearse ./candidate --snapshot <snapshot-id> --plan
bedrock rehearse ./candidate --snapshot <snapshot-id> --yes
```

The app must already be deployed and have backed-up data. Omitting `--snapshot`
selects the latest backup when the rehearsal begins. An explicit ID can be an
unambiguous hexadecimal prefix of at least eight characters. The proof records
the full identity actually restored.

Bedrock captures the candidate's source bytes and permission modes. The source
hash is part of the approved plan digest, and a source change after planning
refuses execution. Source trees must contain plain files and directories;
symlinks and devices are refused.

The rehearsal uses a separate encrypted secret store with the active revision's
pinned credentials. Generated and derived candidate secrets stay in that store.
It restores data to uniquely named scratch volumes, builds or pulls the candidate
images, pins image digests, runs release workloads in declared order, starts
long-running workloads and runs direct application checks. The scratch network
is internal, direct checks refuse redirects outside the container's origin, and
no host ports, public routes or cron schedules are activated. Privileged workloads
are refused because they could escape those constraints.

All scratch containers, volumes, networks, copied source, keys and secrets are
removed on success or failure. Interrupted operations compensate by removing the
same resources. A cleanup failure makes the proof fail.

A credential-free JSON proof lives at
`<state-dir>/rehearsals/<app>/<candidate-revision>.json`. It records source and
manifest hashes, the active revision, exact snapshot, workload image digests,
completed release jobs, outcome and times. Read it locally or through a named target with
`bedrock rehearse report <app> <candidate-revision>`; `--json` returns the full proof.
A successful result proves that
combination. Public edge checks are deferred until real deployment, and a later
snapshot or rebuild can differ from what was tested.

## Declare the data compatibility contract

```yaml
release:
  data_compatibility: backward-compatible
workloads:
  migrate:
    kind: release
    image: ghcr.io/example/app:2
    command: [app, migrate]
```

Use `backward-compatible` only when the release's data changes allow the older
revision to keep reading and writing safely. An additive migration that retains
the old columns is one common example.

Use `restore-required` when running older code requires reverting the app's data.
Omitting the policy means `unknown`, which has the same conservative rollback
behavior. Bedrock does not infer reverse SQL or assume failed release jobs made
no changes.

Before an unknown or restore-required release against an existing data app,
Bedrock captures a quiesced checkpoint of the active revision. Storage must be
configured and capture must succeed before any release job runs. The checkpoint
is tagged `bedrock-release-checkpoint` and ordinary backup retention keeps it.
Checkpoint storage therefore grows until checkpoints are deliberately retired
outside automatic retention. Garbage collection also keeps the prior revision
record and its workload images while a checkpoint has not been restored, even
beyond the ordinary five retired revisions. This preserves the code needed for
an explicit data rollback after later deployments. Once restoration completes,
ordinary active and previous revision retention applies again.

The exact checkpoint and prior revision are persisted before release execution.
The release boundary then transitions from prepared to started to completed or
failed. Failed candidates are included even when they never became active. If
execution is interrupted, jobs are not replayed automatically: inspect the
recorded jobs and create a new candidate or restore the retained checkpoint.
A completed release step is safely skipped on operation recovery.

## Roll back with the matching data

Declared backward-compatible transitions permit ordinary code rollback:

```sh
bedrock rollback my-app --to <older-revision> --plan
bedrock rollback my-app --to <older-revision> --yes
```

Unknown or restore-required transitions refuse code-only rollback. The error
names the attempted release and its exact checkpoint. This applies to automatic
failure recovery too: Bedrock does not restart stopped singletons or switch the
edge back to incompatible older code.

To explicitly replace live data with the retained pre-release checkpoint:

```sh
bedrock rollback my-app --restore-data <full-64-character-checkpoint> --plan
bedrock rollback my-app --restore-data <full-64-character-checkpoint> --yes
```

The checkpoint pins its previous revision, so `--to` is optional. If
supplied, it must name that same revision. The snapshot's manifest is checked
against the target before live data is touched.

**Restoring discards database and volume writes made since that checkpoint.**
Bedrock persists a writer fence, removes the app's containers, replaces its data,
restores its original current secrets as a new secret version, and only then
starts and checks the older code. Revision history, retained images and all
prior secret versions survive. Older release workloads are never rerun.

Jobs and interactive commands for the app remain blocked while restoration is
incomplete. Integration changes also wait, preserving shared recovery credentials.
Read-only reports remain available.
After a daemon or host restart, surviving container restart policies cannot
revive the app. Retrying the same rollback restores the same checkpoint and
clears any partially restored data before retrying. A completed restoration is
idempotent while no later unsafe data release has occurred. If a later release
changes data incompatibly, the previously restored checkpoint cannot be reused
to skip restoration; Bedrock names the newer checkpoint to review. Other apps
remain active.
Releases predating boundary recording with unknown data compatibility are also
refused for code-only rollback; recover through a separately verified backup
when no retained pre-release checkpoint exists.
