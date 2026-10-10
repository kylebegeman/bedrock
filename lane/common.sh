#!/usr/bin/env bash
# Shared, provider-independent transport. Configuration is local trusted shell.
set -euo pipefail

LANE_CONFIG="${LANE_CONFIG:-${TMPDIR:-/tmp}/bedrock-lane-${UID}/lane.env}"
[[ -f "$LANE_CONFIG" ]] || { echo "Lane configuration missing: $LANE_CONFIG. Run lane/vm.sh up, or see lane/README.md for a dedicated SSH target." >&2; exit 2; }
# shellcheck source=/dev/null
. "$LANE_CONFIG"
: "${HOST:?set HOST in the lane configuration}"
: "${KEY:?set KEY in the lane configuration}"
: "${LANE_ID:?set LANE_ID to the dedicated machine identity}"
: "${KNOWN_HOSTS:?set KNOWN_HOSTS to the pinned SSH host-key file}"
[[ "${LANE_DISPOSABLE:-}" == true ]] || { echo 'Refusing target: LANE_DISPOSABLE=true is required.' >&2; exit 2; }
[[ "$LANE_ID" =~ ^[a-f0-9]{32}$ ]] || { echo 'Refusing target: LANE_ID must be 32 lowercase hexadecimal characters.' >&2; exit 2; }
[[ "$HOST" =~ ^[a-zA-Z0-9][a-zA-Z0-9.-]*$ ]] || { echo 'Refusing target: HOST must be an address or hostname, without shell syntax.' >&2; exit 2; }
PORT="${PORT:-22}"
[[ "$PORT" =~ ^[0-9]+$ && "$PORT" -ge 1 && "$PORT" -le 65535 ]] || { echo 'Refusing target: PORT must be 1 through 65535.' >&2; exit 2; }
[[ -f "$KEY" && -s "$KNOWN_HOSTS" ]] || { echo 'Lane needs its SSH key and pinned KNOWN_HOSTS file.' >&2; exit 2; }
for lane_name in LANE_DOMAIN LANE_ZONE; do
  lane_value="${!lane_name:-}"
  [[ -z "$lane_value" || "$lane_value" =~ ^[a-zA-Z0-9][a-zA-Z0-9.-]*$ ]] || { echo "$lane_name contains unsupported characters." >&2; exit 2; }
done
ssh_opts=(-i "$KEY" -p "$PORT" -o IdentityAgent=none -o IdentitiesOnly=yes -o BatchMode=yes
  -o UserKnownHostsFile="$KNOWN_HOSTS" -o StrictHostKeyChecking=yes -o ConnectTimeout=10)
# shellcheck disable=SC2034 # consumed by proof scripts sourcing this file
scp_opts=(-i "$KEY" -P "$PORT" -o IdentityAgent=none -o IdentitiesOnly=yes -o BatchMode=yes
  -o UserKnownHostsFile="$KNOWN_HOSTS" -o StrictHostKeyChecking=yes -o ConnectTimeout=10)
box="root@$HOST"
lane_run() { ssh "${ssh_opts[@]}" "$box" "$@"; }

# Nothing can override this refusal. Neither an unreachable machine nor an
# unreadable app registry can be mistaken for an empty disposable machine.
lane_require_disposable() {
  local observed
  observed=$(lane_run "sh -s -- '$LANE_ID'" <<'GUARD'
set -eu
[ "$(id -u)" = 0 ] || { echo 'Lane must connect as root.' >&2; exit 1; }
[ ! -e /etc/bedrock-production ] && [ ! -L /etc/bedrock-production ] || { echo 'Refusing a production-marked machine.' >&2; exit 1; }
[ -f /etc/bedrock-lane-id ] && [ ! -L /etc/bedrock-lane-id ] || { echo 'Remote disposable identity is missing.' >&2; exit 1; }
[ "$(stat -c %u /etc/bedrock-lane-id)" = 0 ] && [ "$(stat -c %a /etc/bedrock-lane-id)" = 600 ] || { echo 'Remote identity must be owned by root with mode 600.' >&2; exit 1; }
[ "$(cat /etc/bedrock-lane-id)" = "$1" ] || { echo 'Remote disposable identity differs from LANE_ID.' >&2; exit 1; }
if command -v bedrock >/dev/null 2>&1; then
  bedrock ls --json | python3 -c 'import json,sys
registry=json.load(sys.stdin)
assert isinstance(registry, dict) and "apps" in registry and (registry["apps"] is None or isinstance(registry["apps"], list)), "unreadable app registry"
allowed={"hello", "site", "notes", "begamin", "dragon-writer", "lane-preview"}
foreign=[a["name"] for a in (registry["apps"] or []) if a["name"] not in allowed]
if foreign: raise SystemExit("Refusing machine with non-lane apps: " + ", ".join(foreign))
unmanaged=registry.get("unmanaged") or []
assert isinstance(unmanaged, list), "unreadable unmanaged container registry"
foreign=[name for name in unmanaged if name not in {"lane-mailpit", "lane-minio", "lane-cloudflare"}]
if foreign: raise SystemExit("Refusing machine with unmanaged non-lane containers: " + ", ".join(foreign))'
fi
printf 'DISPOSABLE:%s\n' "$1"
GUARD
  ) || { echo 'Lane target safety check failed; no proof was started.' >&2; return 2; }
  [[ "$observed" == "DISPOSABLE:$LANE_ID" ]] || { echo 'Lane target returned an unexpected identity; refusing it.' >&2; return 2; }
}

lane_architecture() {
  local machine
  machine=$(lane_run uname -m)
  case "$machine" in x86_64) LANE_ARCH=amd64 ;; aarch64|arm64) LANE_ARCH=arm64 ;; *) echo "Unsupported lane architecture: $machine" >&2; return 2 ;; esac
  export LANE_ARCH
}

lane_require_disposable
lane_architecture
export LANE_CONFIG LANE_ID HOST PORT LANE_DISPOSABLE
for lane_name in LANE_PROVIDER LANE_DOMAIN LANE_ZONE LANE_SITE_URL LANE_BEGAMIN_REPO LANE_DW_CHAPTERS LANE_VM_IMAGE LANE_OS_IMAGE_SHA256; do
  export "${lane_name?}"
done
