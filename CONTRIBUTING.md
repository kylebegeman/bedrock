# Contributing To Bedrock

Thanks for helping improve Bedrock. This project is contract-first, fixture-first,
and safety-first: public examples should use synthetic apps, mutating flows
should be previewable, and JSON surfaces should stay stable for agents.

## Development Setup

```bash
python3 -m venv .venv
.venv/bin/python -m ensurepip --upgrade
.venv/bin/python -m pip install -e ".[test]"

make validate-examples
make validate-manifests
make validate-fixtures
make validate-adoption-fixtures
make validate-fixture-plugins
make render-examples
make render-manifests
make test
make compile
make docs-check
make open-source-audit-strict
```

Use the narrowest meaningful command while working, then run the relevant
bundle above before opening a pull request.

## Contribution Rules

- Keep app examples synthetic. Use `example.com`, fixture apps, or local demo
  names. Do not add real hostnames, personal paths, private IPs, `.env` values,
  production data, provider tokens, or customer data.
- Prefer fixture coverage before live-provider coverage. Real deployments,
  host registries, and product migrations belong in private operator material
  until they are intentionally adopted.
- Preserve dry-run-first behavior for risky operations. Mutating commands need
  clear plans, confirmation tokens, receipts, and redacted outputs.
- Keep JSON envelopes schema-versioned and agent-friendly. Avoid changing
  output contracts without tests and docs.
- Follow the existing Python style: type hints on function signatures,
  direct functions over clever abstraction, and explicit boundary validation.

## Developer Certificate Of Origin

Bedrock uses the Developer Certificate of Origin instead of a CLA. Every commit
must include a DCO sign-off:

```bash
git commit -s -m "feat: describe the change"
```

That adds a line like:

```text
Signed-off-by: Your Name <you@example.com>
```

By signing off, you certify that you have the right to submit the contribution
under the project license. See <https://developercertificate.org/> for the full
DCO text.

## Pull Request Checklist

- Tests or docs were updated for behavior changes.
- `make open-source-audit-strict` passes.
- No secrets, private hostnames, personal paths, or production values were added.
- Any CLI, manifest, API, JSON, receipt, or runtime contract change has a
  changelog entry under `docs/changelog/`.
- The pull request explains user impact, safety impact, and verification.
