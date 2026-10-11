# Compatibility and upgrades

`bedrock capabilities --json` describes the invoked binary's API, manifest and
state contracts, supported state reader range, optional capability names,
build and stable machine identity. It reads existing identity/state metadata
without creating a secret key or database. The daemon's
`GET /v1/capabilities` describes the running daemon and its persisted schema.

Historical unversioned state is migrated transactionally. The journal,
saved output and app history survive. A binary refuses a newer unsupported
schema through a read-only inspection before opening the file for writes or
WAL changes. Schema changes live in the migration sequence; generated manifest
schema stays aligned with the Go validator.

An upgrade probes the actual candidate binary before switching. It checks
contract versions, required capabilities and the state reader range, then
writes a private consistent SQLite checkpoint before staging the binary.
Checkpoint metadata is bound to the kernel operation ID so an interrupted
upgrade cannot adopt a checkpoint from a different attempt.

Automatic binary rollback requires both binaries to read the resulting state.
A candidate that migrates beyond the running binary's reader is refused with
instructions for a coordinated migration. A state checkpoint is evidence and
a recovery artifact; it is not an automatic destructive database replacement.

API errors retain the existing human message and add stable `code`,
`retryable` and `api_version` fields. Existing receipt and history JSON stay
compatible. Daemon and direct local fallback execution share the same machine
operation lock, so choosing another socket cannot create a second concurrent
writer authority.
