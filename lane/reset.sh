#!/usr/bin/env bash
# Reset only the VM this lane created. Never infer an empty VPS from SSH failure.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
case "${1:-}" in
  --status) exec "$here/vm.sh" status ;;
  '') exec "$here/vm.sh" reset ;;
  *) echo 'Usage: lane/reset.sh [--status]. Dedicated SSH machines are reprovisioned by their owner, separately from proofs.' >&2; exit 2 ;;
esac
