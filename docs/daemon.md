# Ophelia Host Daemon

Status: landed on `next`, released in 0.6.0 and hardened through 0.6.17

`opheliad` is Ophelia's durable single-host authority. It owns the operation
journal, runtime reconciliation, manifest v2 execution, scheduled workloads,
one-shot tasks, host controls, and the authenticated local API. Applications
continue serving and accepted operations remain recoverable when Lumen or the
operator CLI is unavailable.

The daemon is the default transport for manifest v2 planning and apply. Direct
CLI execution remains available only as an explicit local recovery path.

## Install And Start

On a Linux host with Python, Docker, and systemd, calculate the exact install
plan from a trusted Ophelia checkout:

```bash
ship daemon install --source-root /srv/ophelia --json
```

Review the source, unit, configuration, dependency observations, and
confirmation token. Apply the same plan as root:

```bash
sudo ship daemon install \
  --source-root /srv/ophelia \
  --apply \
  --confirm <confirmation-token> \
  --json
```

The installer creates a dedicated `ophelia` identity, installs an immutable
versioned Python environment beneath `/opt/ophelia/releases`, writes the
systemd unit and initial strict configuration, promotes the `current` link,
enables and starts or restarts the service, and requires a successful systemd
health check against the promoted release. An
incomplete version directory is quarantined before a replacement is staged and
atomically published.

Package installation builds from an isolated temporary copy beneath the
release tree. Build-backend output never mutates the trusted source checkout,
and generated `build`, cache, and package metadata directories do not affect
source identity.

The stable launcher treats a systemd-forwarded SIGTERM or SIGINT as an expected
service stop. It exits cleanly without invoking failed-start rollback, while
unexpected child exits still retain their failure status and rollback behavior.

The generated local socket is mode `0600` and owned by the daemon identity.
The initial configuration authorizes UID 0, so host operator commands use
`sudo`. Add another trusted UID explicitly only when that account should hold
root-equivalent Ophelia runtime authority.

## Configuration

The default file is `/etc/ophelia/agent.toml`:

```toml
runtime_root = "/var/lib/ophelia"
socket_path = "/run/ophelia/opheliad.sock"
install_root = "/opt/ophelia"
allowed_uids = [0]
allowed_manifest_roots = ["/srv"]
max_request_bytes = 1048576
max_workers = 4
operation_poll_seconds = 0.5
reconciliation_seconds = 15
integrity_check_seconds = 300
scheduler_seconds = 15
require_edge_runtime = true
local_planning_enabled = true
local_apply_enabled = true
agent_enabled = false
agent_poll_seconds = 5
agent_exchange_bytes = 8388608
agent_event_batch = 250
agent_command_batch = 100
recovery_backup_roots = []
# recovery_age_recipient = "age1..."
recovery_max_bytes = 68719476736
recovery_freshness_seconds = 86400
observation_seconds = 15
observation_retention = 1000
disk_warning_percent = 85
disk_critical_percent = 95
```

Encrypted host recovery is disabled until `recovery_backup_roots` contains one
or more existing trusted directories and `recovery_age_recipient` is set to a
public age, age-plugin, or SSH recipient. The recipient is public configuration;
the matching private identity must remain off-host or in a separately protected
recovery store.

Unknown fields, unsafe ownership, group or world writable configuration,
invalid paths, invalid types, and out-of-range limits fail closed. Inspect the
effective redacted configuration without starting the service:

```bash
opheliad --config /etc/ophelia/agent.toml --check-config
```

## Enroll With Lumen

Enrollment is a confirmation-bound root operation. Place a short-lived,
one-time Lumen enrollment token in a root-owned mode `0600` file, inspect the
exact plan, then apply the same plan:

```bash
sudo ship daemon enroll plan \
  --control-plane https://control.example.com \
  --token-file /root/ophelia-enrollment.token \
  --json

sudo ship daemon enroll apply \
  --control-plane https://control.example.com \
  --token-file /root/ophelia-enrollment.token \
  --confirm <confirmation-token> \
  --json
```

The exchange generates the host key locally, sends only a CSR, verifies the
returned host certificate, CA, and Lumen signing key, and enables the outbound
agent. Host key and certificate material live under the independent
`/var/lib/ophelia-identity` state directory; root-managed CA and decision keys live under
`/etc/ophelia/trust`. Planning reads the configured operation journal without
mutating it and blocks before exchange when that journal is bound to another
host identity. After a successful exchange, the remote identity and one-time
credential are already committed. Ophelia therefore deletes the consumed token
and retains the published identity and agent configuration even if the daemon
health check fails, returning an explicit service-repair error instead of
claiming that the remote enrollment was rolled back.

Normal fleet traffic is outbound HTTPS on port 443 with mutual TLS. No public
Ophelia listener is required. Lumen derives host identity from the client
certificate and must reject expired or revoked identities.

The host transport preserves normal system certificate authorities and adds
the Lumen enrollment CA. This lets the public control-plane edge use an
ordinary publicly trusted server certificate while the independent Lumen CA
continues to authenticate host client certificates. Private deployments can
still use a server certificate signed by the Lumen CA.

Manifest v2 secrets resolve locally from private runtime env files. `opheliad`
passes the resolver into both planning and recovered execution, so missing
bindings block the exact plan instead of failing after approval. File-mode
secrets are decoded and mounted only into their targeted workloads; values do
not enter daemon state, agent payloads, plans, or receipts.

Command sequence acknowledgement is also the authenticated gap-closing
mechanism. Lumen may advance the acknowledged sequence across commands it
withdrew or allowed to expire, but never across a command already accepted and
still running on the host. The host then accepts only the immediate sequence
after the greater of its durable command history and Lumen acknowledgement.
This keeps later fleet work usable after an offline host misses a short-lived
command without weakening signed-command ordering.

## Remote Command And Replay Contract

Each exchange binds a unique exchange ID and reports host capabilities,
identity posture, health, ordered events, and unacknowledged terminal command
results. Lumen returns the same exchange ID, monotonic acknowledgements, and a
strictly ordered command batch.

Remote commands are canonical signed envelopes with a host audience,
short-lived timestamps, explicit scopes, an idempotency key, and one global
per-host sequence. The daemon verifies the Lumen signing key before accepting
the command, stores only a signed-envelope digest and operation-specific
sanitized payload, and journals acceptance before execution. Raw approval
nonces, signatures, manifest contents, source archives, and caller
idempotency keys do not enter the operation database.

Delivery is at least once. Results and host events remain replayable until
Lumen advances their respective cursors. A malformed authorized payload is
recorded as a terminal rejected result so one bad command cannot wedge the
sequence. A missing scope, wrong audience, invalid signature, expired command,
or noncontiguous sequence fails closed.

Supported remote operations are:

- manifest v2 plan and Decision-bound deploy apply
- operation cancellation and one-shot workload runs
- host drain and maintenance controls
- host certificate rotation over the authenticated connection
- staged agent upgrade with a verified source digest, restart confirmation,
  and launcher rollback when the new daemon cannot start
- encrypted host-control backup into a configured destination while the host
  is drained and in maintenance

Certificate rotation reuses the existing protected host key, atomically
publishes a CA-verified replacement certificate, and reloads it through a
clean systemd restart. HTTP 401 or 403 changes local identity posture to
`revoked`; the agent keeps accepted work running locally while refusing to
invent connectivity.

## Normal Operator Flow

Plan through the daemon:

```bash
sudo ship manifest plan /srv/example/.ophelia.yml --json
```

Apply the exact reviewed plan. Acceptance is journaled before the API returns;
execution continues asynchronously:

```bash
sudo ship manifest apply <plan-id> --confirm <confirmation-token> --json
```

Inspect host state and events:

```bash
sudo ship daemon status --json
sudo ship daemon capabilities --json
sudo ship daemon events --cursor 0 --json
```

Run a task declared by the active revision:

```bash
sudo ship daemon run-task example production maintenance --json
```

Drain or place the host in maintenance mode:

```bash
sudo ship daemon drain --enable --json
sudo ship daemon maintenance --enable --json
```

Drain and maintenance refuse new deployments and task runs. Existing accepted
work remains durable and recoverable.

## Encrypted Host Backup

A host backup captures the authoritative operation database and managed
runtime tree, excluding volatile run/inbox state and host identity. Symlinks are
recorded as bounded manifest evidence and reconstructed only when their target
is also present. The payload is archived, encrypted to the configured age
recipient, and published atomically with a cleartext non-secret object manifest
and digests.

First drain the host and enter maintenance. Then review and apply the exact
backup plan:

```bash
sudo ship daemon drain --enable --json
sudo ship daemon maintenance --enable --json

sudo ship daemon backup plan \
  --backup-id backup_2026-07-19T2200Z \
  --destination /mnt/ophelia-offsite \
  --json

sudo ship daemon backup apply \
  --backup-id backup_2026-07-19T2200Z \
  --destination /mnt/ophelia-offsite \
  --confirm <confirmation-token> \
  --json
```

Lumen can request the same operation with the signed
`ophelia:backup:create` scope. The command is idempotent and returns the same
verified object receipt on replay.

This is a host-control backup. Application databases and external volumes must
first produce their declared product backup/export artifacts; Ophelia then
preserves that recovery evidence with the runtime. The host certificate,
private key, Lumen trust keys, and age private identity are intentionally not
inside the backup.

## Clean-Host Recovery

On the replacement Linux host, install Ophelia without starting the daemon so
the runtime root remains empty:

```bash
sudo ship daemon install \
  --source-root /srv/ophelia \
  --defer-start \
  --apply \
  --confirm <install-confirmation-token> \
  --json
```

Review and apply recovery using the protected age identity. The target runtime
must be absent or truly empty; recovery never overwrites a populated host.

```bash
sudo ship daemon recover plan \
  --backup-root /mnt/ophelia-offsite/host_example-1/backup_2026-07-19T2200Z \
  --identity-file /root/ophelia-recovery.agekey \
  --target-runtime-root /var/lib/ophelia \
  --json

sudo ship daemon recover apply \
  --backup-root /mnt/ophelia-offsite/host_example-1/backup_2026-07-19T2200Z \
  --identity-file /root/ophelia-recovery.agekey \
  --target-runtime-root /var/lib/ophelia \
  --confirm <recovery-confirmation-token> \
  --json
```

Recovery validates the outer object digest, archive safety and quotas, every
inner file digest, symlink containment, and SQLite integrity before one atomic
runtime promotion. It writes a restore receipt before promotion and does not
restore host identity. Enroll the replacement host with Lumen, then start or
restart `opheliad`; running host-agent commands and accepted operations remain
replayable from the restored journal.

For recovery of the same logical host, pass the `host_id` from the backup
manifest explicitly to `ship daemon enroll`; do not accept the replacement
machine's newly derived default ID. Lumen should revoke the lost certificate
and issue a fresh certificate for that recovered logical host before normal
exchange resumes.

## Continuous Observations

`opheliad` records a bounded durable observation at startup and on the
configured interval. Each sample includes disk pressure, CPU/load and memory
availability, operation/workload counts, active app count, certificate posture,
backup freshness, and control-plane connection state. Samples are canonical,
digest-verified, retention-limited, exposed at
`GET /v1/observations/latest`, and attached to every outbound Lumen exchange.
Critical disk, expired identity, or revoked control-plane identity degrades
daemon health. Stale backups, expiring certificates, and ordinary disconnects
remain explicit warning posture without stopping accepted local work.

## Local Recovery

When the daemon is unavailable, an operator may use the same kernel directly:

```bash
sudo ship manifest plan /srv/example/.ophelia.yml \
  --direct \
  --runtime-root /var/lib/ophelia \
  --json

sudo ship manifest apply <plan-id> \
  --direct \
  --runtime-root /var/lib/ophelia \
  --confirm <confirmation-token> \
  --json
```

This is a recovery and compatibility lane, not a second executor. It uses the
same plan contract, operation journal, leases, backend, compensation, and
receipt logic as the daemon.

## Local API Contract

The daemon serves HTTP/JSON over its Unix socket. Every request requires:

- `X-Ophelia-Protocol-Version: 1`
- a bounded `X-Request-ID`
- authenticated Unix peer credentials
- an `Idempotency-Key` for every mutation

Primary resources are:

| Method | Resource | Purpose |
| --- | --- | --- |
| `GET` | `/v1/health` | Loop health and daemon identity |
| `GET` | `/v1/capabilities` | Protocol, workload, runtime, and limit negotiation |
| `GET` | `/v1/hosts/self` | Durable host state and controls |
| `GET` | `/v1/observations/latest` | Latest digest-verified host observation |
| `GET` | `/v1/apps` | Active application revisions |
| `GET` | `/v1/apps/{app}/{environment}` | Active revision and recent task runs |
| `POST` | `/v1/plans` | Create an idempotent manifest v2 plan |
| `POST` | `/v1/operations` | Durably accept an approved plan |
| `GET` | `/v1/operations` | List journaled operations |
| `GET` | `/v1/operations/{id}` | Operation, events, and terminal receipt |
| `POST` | `/v1/operations/{id}/cancel` | Request durable cancellation |
| `GET` | `/v1/events` | Read the globally ordered host event stream |
| `POST` | `/v1/events/acknowledge` | Advance a consumer cursor monotonically |
| `POST` | `/v1/workload-runs` | Accept an active-revision task run |
| `GET` | `/v1/workload-runs` | List task and cron runs |
| `POST` | `/v1/workload-runs/{id}/cancel` | Request task cancellation |
| `POST` | `/v1/host/drain` | Change drain state |
| `POST` | `/v1/host/maintenance` | Change maintenance state |

Actor identity is always derived from the Unix peer UID. Caller-supplied actor
labels are not accepted. Events have one monotonic host cursor and retain their
operation or workload event digest for replay and integrity verification.

## Recovery And Service Health

At startup, daemon workers acquire fresh monotonic fences and resume every
nonterminal operation through the canonical executor. Workload runs are bound
to the exact retained active revision. Long-running runs renew their lease;
workers that lose a fence stop participating without overwriting the new
owner's result.

The reconciliation loop heartbeats host state, checks journal and event-chain
integrity, and reports `STATUS=` to systemd. Loop failures make `/v1/health`
degraded while retaining only the error type and observation time. Raw process
output and exception text are not persisted in host health.

A separate watchdog loop owns `WATCHDOG=1`, at half the `WATCHDOG_USEC` systemd
supplies. Reconciliation must not own the ping: a pass can block past
`WatchdogSec` while the host is under deployment load, and a pass that raises
would skip it, so systemd would kill a daemon that was merely slow or reporting
an error. The watchdog keeps pinging while reconciliation still completes
passes, and withholds the ping once none has completed for
`watchdog_stall_seconds`, so a genuinely wedged daemon is still restarted.

## Current Boundary

The host side of the authenticated fleet transport is implemented on `next`.
Lumen still needs to provide the matching enrollment, certificate authority,
exchange, signing, acknowledgement, command queue, and DeployProvider surfaces.
Encrypted off-site backups, clean-host restore, richer runtime observations,
placement, and fleet rollout orchestration remain separate milestones.
