# Named targets and portable bootstrap

Named targets let the same Bedrock CLI plan, inspect and operate a remote
machine through SSH. Select the machine explicitly on every invocation.
Connection failures never fall back to the local machine.

## Configure and verify a target

First establish the host's SSH key through your normal trusted channel. Bedrock
uses strict host-key checking and noninteractive SSH, including for bootstrap.
An existing SSH configuration alias can supply connection settings.

```sh
bedrock target set staging --ssh bedrock-staging --sudo
bedrock target verify staging
bedrock target list
bedrock --target staging status --json
bedrock --target staging check . --host
bedrock --target staging deploy . --plan --json
bedrock --target staging deploy . --digest <target-plan-digest>
```

`target verify` pins the reported machine identity. An unexpected identity is
refused. Inspect a replaced machine before deliberately changing its target
configuration. `--targets-file` selects a separate local configuration; the
default is `~/.bedrock/targets.json`. `--state-dir` and `--socket` overrides
apply consistently to target capability queries, uploads and operations.

A target plan's digest binds its connection settings, observed machine identity
and uploaded source or restore inputs. Changing any of these requires a fresh
plan. `--yes` applies without interactive confirmation. Target announcements
use stderr so command JSON, encrypted exports and migration handoffs retain
their normal stdout contract. SSH exit codes are preserved.

Local sources for `deploy`, `check`, `rehearse`, `move stage` and `move test`
are uploaded as bounded checksum-addressed archives. Git trees follow Git's
tracked and unignored file selection; non-Git trees exclude `.git` and
`node_modules`. Named-target archives omit `.env` and `.env.*` files, including
nested ones. Supply runtime values through Bedrock's secret store. Alternate
manifest names and deploy restore files are supported. Source archives retain
internal symlinks and reject paths that escape the extraction root.

Artifacts remain privately cached on the target for retries and operation
recovery. Keep the state directory on a filesystem with enough room for the
source and restore inputs. `run --stdin` and recovery identities stream stdin;
planning does not consume it.

## Write a portable profile

A profile contains a host configuration, an exact Bedrock version, reviewed
Linux binary checksums and integration names. It contains no private identity
or integration values.

```sh
bedrock host profile write staging.json \
  --hostname staging --version 0.7.5 \
  --checksum linux_arm64=<reviewed-sha256> \
  --integration storage,email
bedrock host profile check staging.json
bedrock --target staging host bootstrap --profile staging.json --plan
bedrock --target staging host bootstrap --profile staging.json --yes
```

Use the checksum published with the selected release. Bootstrap probes the
machine architecture and downloads the matching Linux build. For a local
Linux build, use `--binary ./bin/bedrock-linux-arm64`; its version and bytes must
match the profile. A development build may pin its reported development
version and a locally reviewed checksum.

Bootstrap journals the entire sequence on the client: install the pinned
binary, verify contracts and machine identity, create the target's own secret
identity, transfer selected integrations, apply the host profile, install the
daemon, then verify the daemon and doctor. A different existing target binary
is refused; use compatibility-aware `upgrade` to replace it.

```sh
bedrock host bootstrap --resume
bedrock history --state-dir ~/.bedrock/bootstrap --json
```

`--resume` replays unfinished receipts after their lease expires, using the
recorded target/profile and cached binary. Failed completed receipts can be
retried by submitting the profile again; steps adopt matching installed state.
`--journal-dir` or `BEDROCK_BOOTSTRAP_DIR` selects another local journal. Keep
that journal and the client secret store until bootstrap finishes.

## Transfer integrations separately

Public transfer includes only explicitly selected integrations. It preserves
storage's repository password, which is required to read existing backups.
The target keeps its own age identity.

```sh
bedrock --target staging secret recipient
bedrock integration export storage email --to <target-age-recipient> \
  | bedrock --target staging integration import storage email
```

The recipient-bound ciphertext expires after ten minutes. Import validates
its version, kind, expiry, recipient and requested integration scope before
one atomic secret-store update. Matching imports are idempotent; conflicting
values require explicit `--replace`. Deploy keys and hook keys are excluded
from this public integration transfer. Application secrets use `move secrets`.
