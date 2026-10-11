#!/usr/bin/env bash
# Two guarded disposable guests: real staged migration and clean machine recovery.
set -euo pipefail
umask 077
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
: "${LANE_TARGET_CONFIG:?set LANE_TARGET_CONFIG to a different disposable guest lane.env}"
# shellcheck source=lane/common.sh
. "$here/common.sh"
source_config=$LANE_CONFIG
source_id=$LANE_ID
source_box=$box
source_port=$PORT
source_key=$KEY
source_hosts=$KNOWN_HOSTS
source_ssh=("${ssh_opts[@]}")
source_scp=("${scp_opts[@]}")
source_arch=$LANE_ARCH
LANE_CONFIG=$LANE_TARGET_CONFIG
# shellcheck source=lane/common.sh
. "$here/common.sh"
[[ "$LANE_ID" != "$source_id" ]] || { echo 'Transfer needs two different disposable machine identities.' >&2; exit 2; }
[[ "$LANE_ARCH" == "$source_arch" ]] || { echo 'Transfer image fixtures require matching target architectures.' >&2; exit 2; }
target_box=$box
target_ssh=("${ssh_opts[@]}")
target_scp=("${scp_opts[@]}")
target_arch=$LANE_ARCH
# The arguments here are already quoted remote shell programs.
# shellcheck disable=SC2029
source_run() { ssh "${source_ssh[@]}" "$source_box" "$@"; }
# shellcheck disable=SC2029
target_run() { ssh "${target_ssh[@]}" "$target_box" "$@"; }
unset BEDROCK_STATE_DIR BEDROCK_SOCKET
work=$(mktemp -d "${TMPDIR:-/tmp}/bedrock-transfer.XXXXXX")
source_tunnel_pid=''
target_tunnel_pid=''
cleanup() {
  for owned_pid in "$target_tunnel_pid" "$source_tunnel_pid"; do
    if [[ -n "$owned_pid" ]] && jobs -pr | rg -q "^${owned_pid}$"; then kill "$owned_pid"; wait "$owned_pid" || true; fi
  done
  rm -rf "$work"
}
trap cleanup EXIT
client() { "$work/bedrock" --targets-file "$work/targets.json" "$@"; }
src() { client --target source "$@"; }
dst() { client --target destination "$@"; }
field() { python3 -c 'import json,sys; print(json.load(sys.stdin)[sys.argv[1]])' "$1"; }
expect_refusal() {
  local message=$1
  shift
  if "$@" >"$work/refusal.log" 2>&1; then echo "Transfer NOT proven: expected refusal ($message)." >&2; return 1; fi
  if ! rg -q "$message" "$work/refusal.log"; then cat "$work/refusal.log" >&2; return 1; fi
}
notes_marker() {
  "$1" 'cid=$(docker ps -q --filter label=bedrock.app=notes --filter label=bedrock.workload=worker); test -n "$cid"; docker exec "$cid" cat /files/proof'
}

printf '== build reviewed transfer binaries and retain exact source checkpoint\n'
version=0.7.0-transfer
(cd "$here/.." && go build -trimpath -ldflags "-X github.com/kylebegeman/bedrock/internal/version.number=$version" -o "$work/bedrock" ./cmd/bedrock)
(cd "$here/.." && CGO_ENABLED=0 GOOS=linux GOARCH="$source_arch" go build -trimpath -ldflags "-X github.com/kylebegeman/bedrock/internal/version.number=$version" -o "$work/bedrock-linux" ./cmd/bedrock)
checksum=$(shasum -a 256 "$work/bedrock-linux" | awk '{print $1}')
remote_work="/var/lib/bedrock/lane-transfer/$source_id"
source_run "install -d -m 700 '$remote_work'"
scp -q "${source_scp[@]}" "$work/bedrock-linux" "$source_box:$remote_work/bedrock-candidate"
prior=$(source_run 'bedrock history --json --limit 1' | python3 -c 'import json,sys; r=json.load(sys.stdin); print(r[0]["id"] if r else "none")')
source_run "chmod 755 '$remote_work/bedrock-candidate'; nohup bedrock upgrade '$remote_work/bedrock-candidate' --yes >'$remote_work/upgrade.log' 2>&1 </dev/null &"
ready=false
for _ in $(seq 1 90); do
  if [[ "$(source_run 'sha256sum /usr/local/bin/bedrock' | awk '{print $1}')" == "$checksum" ]] && source_run 'bedrock daemon status' 2>/dev/null | rg -q "daemon $version answering"; then
    if source_run 'bedrock history --json --limit 1' | python3 -c 'import json,sys; r=json.load(sys.stdin); sys.exit(0 if r and r[0]["id"]!=sys.argv[1] and r[0]["kind"]=="host.upgrade" and r[0]["status"]=="succeeded" else 1)' "$prior"; then ready=true; break; fi
  fi
  sleep 2
 done
[[ "$ready" == true ]] || { source_run "cat '$remote_work/upgrade.log'" >&2; exit 1; }
client target set source --ssh "$source_box" --port "$source_port" --identity "$source_key" --known-hosts "$source_hosts"
client target set destination --ssh "$target_box" --port "$PORT" --identity "$KEY" --known-hosts "$KNOWN_HOSTS"
# A retry may find a previous reviewed build. Upgrade through its journal before
# the bootstrap installer verifies the exact requested checksum.
if target_run 'test -x /usr/local/bin/bedrock'; then
  installed=$(target_run 'sha256sum /usr/local/bin/bedrock' | awk '{print $1}')
  if [[ "$installed" != "$checksum" ]]; then
    target_work="/var/lib/bedrock/lane-transfer/$LANE_ID"
    target_run "install -d -m 700 '$target_work'"
    scp -q "${target_scp[@]}" "$work/bedrock-linux" "$target_box:$target_work/bedrock-candidate"
    prior=$(target_run 'bedrock history --json --limit 1' | python3 -c 'import json,sys; r=json.load(sys.stdin); print(r[0]["id"] if r else "none")')
    target_run "chmod 755 '$target_work/bedrock-candidate'; nohup bedrock upgrade '$target_work/bedrock-candidate' --yes >'$target_work/upgrade.log' 2>&1 </dev/null &"
    ready=false
    for _ in $(seq 1 90); do
      if [[ "$(target_run 'sha256sum /usr/local/bin/bedrock' | awk '{print $1}')" == "$checksum" ]] && target_run 'bedrock daemon status' 2>/dev/null | rg -q "daemon $version answering"; then
        if target_run 'bedrock history --json --limit 1' | python3 -c 'import json,sys; r=json.load(sys.stdin); sys.exit(0 if r and r[0]["id"]!=sys.argv[1] and r[0]["kind"]=="host.upgrade" and r[0]["status"]=="succeeded" else 1)' "$prior"; then ready=true; break; fi
      fi
      sleep 2
    done
    [[ "$ready" == true ]] || { target_run "cat '$target_work/upgrade.log'" >&2; exit 1; }
  fi
fi
client host profile write "$work/target-profile.json" --hostname bedrock-lane-target --swap 1 --timezone UTC --version "$version" --checksum "linux_$target_arch=$checksum"
client --target destination host bootstrap --profile "$work/target-profile.json" --binary "$work/bedrock-linux" --journal-dir "$work/bootstrap" --yes
target_run 'test "$(hostname)" = bedrock-lane-target'
source_recipient=$(src secret recipient)
target_recipient=$(dst secret recipient)
[[ "$source_recipient" != "$target_recipient" ]]
source_machine=$(src capabilities --json | field machine_id)
target_machine=$(dst capabilities --json | field machine_id)
[[ "$source_machine" != "$target_machine" ]]
client target set source --ssh "$source_box" --port "$source_port" --identity "$source_key" --known-hosts "$source_hosts" --machine-id "$source_machine"
client target set destination --ssh "$target_box" --port "$PORT" --identity "$KEY" --known-hosts "$KNOWN_HOSTS" --machine-id "$target_machine"

printf '== share only disposable loopback backup storage through pinned SSH\n'
LANE_CONFIG="$source_config" "$here/setup-fixtures.sh" storage
bridge=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')
ssh "${source_ssh[@]}" -N -o ExitOnForwardFailure=yes -L "127.0.0.1:$bridge:127.0.0.1:9000" "$source_box" >"$work/source-tunnel.log" 2>&1 &
source_tunnel_pid=$!
ssh "${target_ssh[@]}" -N -o ExitOnForwardFailure=yes -R "127.0.0.1:9000:127.0.0.1:$bridge" "$target_box" >"$work/target-tunnel.log" 2>&1 &
target_tunnel_pid=$!
ready=false
for _ in $(seq 1 20); do
  if target_run 'curl -sf http://127.0.0.1:9000/minio/health/live >/dev/null'; then ready=true; break; fi
  sleep 1
 done
[[ "$ready" == true ]] || { cat "$work/source-tunnel.log" "$work/target-tunnel.log" >&2; exit 1; }
src integration export storage --to "$target_recipient" | dst integration import storage

printf '== baseline fixture apps without external routes\n'
mkdir "$work/hello" "$work/notes"
cat >"$work/hello/bedrock.yaml" <<'YAML'
app: hello
workloads:
  worker:
    kind: worker
    image: busybox:1.37
    command: [sh, -c, 'trap "exit 0" TERM INT; while :; do sleep 10 & wait "$!"; done']
YAML
cat >"$work/notes/bedrock.yaml" <<'YAML'
app: notes
workloads:
  worker:
    kind: worker
    image: busybox:1.37
    user: "0"
    command: [sh, -c, 'test -f /files/proof || printf original >/files/proof; trap "exit 0" TERM INT; while :; do sleep 10 & wait "$!"; done']
    mounts: [{volume: files, path: /files}]
    health: {command: [sh, -c, 'test "$(cat /files/proof)" = original'], timeout: 5s}
data:
  volumes: {files: {}}
YAML
printf 'retained transfer fixture\n' | src secret set hello RECOVERY_PROOF >/dev/null
src deploy "$work/hello" --yes
src deploy "$work/notes" --yes
[[ "$(notes_marker source_run)" == original ]]

printf '== real destination rehearsal and exact final staging\n'
src move rehearse notes --to bedrock-lane-target --yes >"$work/rehearsal.json"
dst move test "$work/notes" --handoff "$work/rehearsal.json" --yes
[[ "$(notes_marker source_run)" == original ]]
target_run 'test -z "$(docker ps -aq --filter name=bedrock.notes.drill.)" && test -z "$(docker volume ls -q --filter name=bedrock.notes.drill.)"'
src move final notes --to bedrock-lane-target --yes >"$work/final.json"
source_run 'test -f /var/lib/bedrock/locks/notes.migration.json && test -z "$(docker ps -q --filter label=bedrock.app=notes --filter label=bedrock.workload=worker)"; systemctl restart bedrock'
expect_refusal 'fenced|migration|move resume' src deploy "$work/notes" --yes
dst move stage "$work/notes" --handoff "$work/final.json" --yes
target_run 'test -f /var/lib/bedrock/locks/notes.migration.json && test -z "$(docker ps -q --filter label=bedrock.app=notes --filter label=bedrock.workload=worker)"'
expect_refusal 'fenced|migration|move cutover' dst deploy "$work/notes" --yes
dst move cutover notes --yes
[[ "$(notes_marker target_run)" == original ]]
source_run 'test -z "$(docker ps -q --filter label=bedrock.app=notes --filter label=bedrock.workload=worker)"'
dst remove notes --data --yes
src move resume notes --target-stopped --yes
[[ "$(notes_marker source_run)" == original ]]

printf '== exact clean-host recovery with source writers fenced\n'
dst kernel exercise --dir /var/lib/bedrock/lane-transfer/journal --steps 1 --yes
target_journal=$(dst history --json --limit 1 | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["id"])')
# Bind the resumed notes revision to a fresh provenance-bearing snapshot.
src backup notes --yes
src backup bedrock --yes
snapshot=$(src backups bedrock --json | python3 -c 'import json,sys; r=next(r for r in json.load(sys.stdin) if r["kind"]=="backup" and r["ok"]); assert len(r["snapshot"])==64; print(r["snapshot"])')
recovery_id="lane-transfer-${snapshot:0:16}"
src move final notes --to bedrock-lane-target --yes >"$work/recovery-notes-final.json"
src move final hello --to bedrock-lane-target --yes >"$work/recovery-hello-final.json"
source_run 'test -z "$(docker ps -q --filter label=bedrock.app=notes --filter label=bedrock.workload=worker)" && test -z "$(docker ps -q --filter label=bedrock.app=hello --filter label=bedrock.workload=worker)"'
# The source identity is piped directly between two pinned SSH transports.
# It is never printed, stored on the client, or substituted for the target key.
source_run 'cat /etc/bedrock/secrets.key' | dst recover bedrock-lane --snapshot "$snapshot" --identity-stdin --id "$recovery_id" --yes
dst recover report "$recovery_id" --json | python3 -c 'import json,sys; r=json.load(sys.stdin); assert r["status"]=="private-ready" and not r["cutover"] and all(r["private_ready"].values()) and {"hello","notes"}.issubset(r["private_ready"])'
dst ps --json | python3 -c 'import json,sys; assert not json.load(sys.stdin)'
[[ "$(dst secret recipient)" == "$target_recipient" && "$(dst capabilities --json | field machine_id)" == "$target_machine" ]]
dst history --json --limit 200 | python3 -c 'import json,sys; assert any(r["id"]==sys.argv[1] for r in json.load(sys.stdin))' "$target_journal"
dst secret list hello --json | python3 -c 'import json,sys; r=json.load(sys.stdin); assert "RECOVERY_PROOF" in r["names"]'
expect_refusal 'reserved by recovery|recovery' dst deploy "$work/hello" --yes
target_run "python3 - '$recovery_id'" <<'REMOTE'
import json,subprocess,sys
ids=subprocess.check_output(["docker","ps","-q","--filter","label=bedrock.recovery="+sys.argv[1]],text=True).split()
assert ids
for cid in ids:
 r=json.loads(subprocess.check_output(["docker","inspect",cid],text=True))[0]
 assert not r["HostConfig"]["PortBindings"]
REMOTE

printf '== explicit recovered-machine cutover and retained target identity\n'
dst recover cutover "$recovery_id" --yes
dst recover report "$recovery_id" --json | python3 -c 'import json,sys; r=json.load(sys.stdin); assert r["status"]=="live" and r["cutover"]'
[[ "$(notes_marker target_run)" == original && "$(dst secret recipient)" == "$target_recipient" ]]
dst ps --json | python3 -c 'import json,sys; rows=json.load(sys.stdin); assert {"hello","notes"}.issubset({r["app"] for r in rows})'
source_run 'test -f /var/lib/bedrock/locks/hello.migration.json && test -f /var/lib/bedrock/locks/notes.migration.json && test -z "$(docker ps -q --filter label=bedrock.app=notes --filter label=bedrock.workload=worker)"'

printf '== remove disposable destination apps before deliberate source resume\n'
dst remove hello --data --yes
dst remove notes --data --yes
src move resume hello --target-stopped --yes
src move resume notes --target-stopped --yes
[[ "$(notes_marker source_run)" == original ]]
printf 'Transfer proven: distinct host identities; recipient-bound shared storage; destination scratch rehearsal; persistent source final fence after daemon restart; exact destination staging and explicit writer cutover; clean encrypted machine recovery; inactive imported state and retained target journal/key; explicit recovered-machine cutover; deliberate source resume after removing destination writers.\n'
printf 'No public DNS, traffic routing, certificate issuance, external provider or production account was used.\n'
