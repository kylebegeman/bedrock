# Approved deployment sources

A deployment plan binds the manifest and the source tree used by database
initialization, image builds and static workloads. Its source digest covers
file bytes, permission modes, directories and relative links. Local captures
include the supplied checkout and Git metadata. Named-target uploads retain
their existing source selection and environment-file exclusions.

```sh
bedrock deploy ./app --plan --json
bedrock deploy ./app --digest <approved-plan-digest>
```

Interactive confirmation applies the digest of the displayed plan too. A
changed source requires reviewing a fresh plan. `--yes` approves the current
plan without an interactive prompt.

## Capture before execution

The first execution step copies the approved tree into a private,
operation-bound directory under `<state-dir>/builds/<app>`. The copied tree is
hashed again and checked against the approved digest before later steps run.
Initialization and builds read that capture, so changing or removing the
original checkout after capture cannot change the deployed bytes.

Sources must be plain directories containing regular files, directories and
safe relative links. Links whose resolved ancestors escape the source,
absolute links, cycles and special files are refused. Safe dangling relative
links may remain for build-generated files. A Dockerfile must stay inside its
build context; its actual file is checked before the builder starts.

Capturing a tree that contains Bedrock's own capture directory is refused to
avoid recursive copies. Before copying, Bedrock accounts for logical file
sizes, including sparse files, and entry overhead while preserving at least
5 GiB of free space. Free space or run `bedrock gc` if this check fails.

## Retry and recovery

The reservation records the operation, input, manifest, commit and exact source
digest. A recovered operation adopts only its own matching capture. Reusing a
revision belonging to another deployment is refused, and retrying a recorded
step preserves its images and pinned secret version.

An interrupted arbitrary one-off command is not replayed automatically because
its external effects cannot be determined. Inspect the recorded run before
starting another command. Cron jobs surviving a daemon restart are adopted by
container identity, retain their original execution deadline and prevent another
run of that workload until they finish or are safely stopped.

## Cleanup and retention

Successful deployments remove the source copy while retaining the reservation
needed to complete their durable receipt. Failed and interrupted operations
keep the evidence necessary for recovery. Garbage collection removes captures
and reservations only after the owning operation is terminal. It retains
captures whose ownership is missing or mismatched and revisions and images
needed by unfinished jobs.

Explicit app removal clears its source reservations. With `--data`, the plan
names the exact volumes gathered from all recorded revisions, including volumes
removed from the latest manifest. Malformed or mismatched history refuses that
destructive plan rather than guessing which data belongs to the app.

Backup recovery similarly retains an operation-bound receipt with the exact
full snapshot ID and summary. Retention adopts that identity rather than the
repository's latest snapshot. An older interrupted operation without enough
durable identity fails safely and requires a fresh reviewed backup.
