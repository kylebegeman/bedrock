#!/usr/bin/env bash
# Bedrock lane: wipe the Hostinger test bed and wait until it answers over SSH.
#
#   reset.sh            recreate the VM, wait, print "ready"
#   reset.sh --status   show the VM's state and stop
#
# Needs: the hostinger CLI with ~/.hostinger.yaml, and hostinger.env beside this
# script with VM_ID, TEMPLATE_ID, SCRIPT_ID, KEY_ID, HOST and KEY.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
. "$here/hostinger.env"
known_hosts="$here/known_hosts"
ssh_opts=(-i "$KEY" -o IdentityAgent=none -o IdentitiesOnly=yes -o BatchMode=yes
  -o UserKnownHostsFile="$known_hosts" -o StrictHostKeyChecking=accept-new -o ConnectTimeout=10)

vm_json() { hostinger vps virtual-machines get "$VM_ID" --format json 2>/dev/null | python3 -c 'import json,sys; r=sys.stdin.read(); print(json.dumps(json.loads(r[r.find("{"):])))'; }
vm_state() { vm_json | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get("state"), d.get("actions_lock"))'; }

if [[ "${1:-}" == "--status" ]]; then
  echo "vm $VM_ID: $(vm_state)"
  exit 0
fi

# Refuse to recreate a machine that is carrying apps.
#
# The lane is meant to be a machine nobody minds losing, and it stops being
# that the moment something is deployed to it. This has already happened
# once: the lane box became the production host for the personal sites and
# Loom Core while hostinger.env still pointed at it, which left an unguarded
# `make lane-reset` one command away from reinstalling the OS under five
# live apps and deleting their snapshots with it.
#
# A machine that does not answer, or that has no bedrock on it, is a wiped
# or fresh machine and is fine to take. Only a clear yes, naming this exact
# host, gets past a machine that answers with apps.
carried=$(ssh "${ssh_opts[@]}" "root@$HOST" 'bedrock ls --json 2>/dev/null' 2>/dev/null \
  | python3 -c 'import json,sys
try:
    print(" ".join(a["name"] for a in json.load(sys.stdin).get("apps") or []))
except Exception:
    print("")' 2>/dev/null || true)
if [[ -n "${carried// /}" ]]; then
  if [[ "${BEDROCK_LANE_DESTROY:-}" != "$HOST" ]]; then
    cat >&2 <<EOF
$HOST is carrying apps, so it is not a test bed:

  $carried

Recreating it reinstalls the OS and deletes every snapshot, and those apps
go with it. Point hostinger.env at a machine you can lose, or if you really
mean this one, name it:

  BEDROCK_LANE_DESTROY=$HOST $0
EOF
    exit 1
  fi
  echo "BEDROCK_LANE_DESTROY names $HOST; destroying it with $carried on it" >&2
fi

echo "recreating vm $VM_ID with template $TEMPLATE_ID and post-install script $SCRIPT_ID"
started=$(date +%s)
action=$(hostinger vps virtual-machines recreate "$VM_ID" --template-id "$TEMPLATE_ID" --post-install-script-id "$SCRIPT_ID" --format json 2>/dev/null \
  | python3 -c 'import json,sys; r=sys.stdin.read(); d=json.loads(r[r.find("{"):]); print(d.get("id"))')
echo "action $action started"
action_state() { hostinger vps actions get "$VM_ID" "$action" --format json 2>/dev/null | python3 -c 'import json,sys; r=sys.stdin.read(); print(json.loads(r[r.find("{"):]).get("state"))'; }

# The old host key is gone with the OS; forget it so accept-new can pin the new one.
rm -f "$known_hosts"

# Wait for the recreate action to finish, then for the VM to run unlocked.
state=""
while :; do
  state="$(action_state)"
  case "$state" in success|completed|done|finished) break ;; failed|error|cancelled) echo "recreate $state" >&2; exit 1 ;; esac
  sleep 10
done
echo "recreate action $state after $(( $(date +%s) - started ))s"
until [[ "$(vm_state)" == "running unlocked" ]]; do sleep 10; done
echo "vm running after $(( $(date +%s) - started ))s; waiting for ssh"
# The fresh OS has no keys; ask Hostinger to install the account's deploy key on it.
for attempt in 1 2 3 4 5 6; do
  if hostinger vps public-keys attach "$VM_ID" --ids "$KEY_ID" --format json >/dev/null 2>&1; then echo "deploy key attached"; break; fi
  [[ $attempt -eq 6 ]] && { echo "could not attach the deploy key" >&2; exit 1; }
  sleep 10
done
until ssh "${ssh_opts[@]}" "root@$HOST" true 2>/dev/null; do sleep 5; done
echo "ready after $(( $(date +%s) - started ))s: $(ssh "${ssh_opts[@]}" "root@$HOST" 'hostname; . /etc/os-release; echo $PRETTY_NAME; uptime -p')"
echo "host key pinned in $known_hosts"
