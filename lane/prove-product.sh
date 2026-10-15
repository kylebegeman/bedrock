#!/usr/bin/env bash
# Real product workflows on a guarded disposable guest, without public DNS.
set -euo pipefail
umask 077
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lane/common.sh
. "$here/common.sh"
unset BEDROCK_STATE_DIR BEDROCK_SOCKET
work=$(mktemp -d "${TMPDIR:-/tmp}/bedrock-product.XXXXXX")
remote_work="/var/lib/bedrock/lane-product/$LANE_ID"
version="0.7.0-product"
fixture_started=false
cleanup() {
  if [[ "$fixture_started" == true ]]; then
    lane_run 'systemctl stop bedrock-lane-auth.service && rm -f /etc/systemd/system/bedrock-lane-auth.service /usr/local/share/ca-certificates/bedrock-lane-auth.crt && systemctl daemon-reload && update-ca-certificates >/dev/null' || true
  fi
  rm -rf "$work"
}
trap cleanup EXIT
br() { "$work/bedrock" --targets-file "$work/targets.json" --target lane "$@"; }
client() { "$work/bedrock" --targets-file "$work/targets.json" "$@"; }
field() { python3 -c 'import json,sys; print(json.load(sys.stdin)[sys.argv[1]])' "$1"; }
last_operation() { br history --json --limit 1 | python3 -c 'import json,sys; rows=json.load(sys.stdin); print(rows[0]["id"] if rows else "none")'; }
expect_refusal() {
  local message=$1
  shift
  if "$@" >"$work/refusal.log" 2>&1; then
    echo "Product NOT proven: expected refusal ($message)." >&2
    return 1
  fi
  if ! rg -q "$message" "$work/refusal.log"; then
    cat "$work/refusal.log" >&2
    echo "Product NOT proven: refusal did not explain $message." >&2
    return 1
  fi
}
active_revision() {
  br ps --json | python3 -c 'import json,sys; rows=json.load(sys.stdin); active={r["revision"] for r in rows if r["app"]==sys.argv[1]}; assert len(active)==1; print(next(iter(active)))' "$1"
}
notes_marker() {
  lane_run 'cid=$(docker ps -q --filter label=bedrock.app=notes --filter label=bedrock.workload=worker); test -n "$cid"; docker exec "$cid" cat /files/proof'
}

printf '== build exact product binaries\n'
(cd "$here/.." && go build -trimpath -ldflags "-X github.com/kylebegeman/helios/internal/version.number=$version" -o "$work/bedrock" ./cmd/helios)
(cd "$here/.." && CGO_ENABLED=0 GOOS=linux GOARCH="$LANE_ARCH" go build -trimpath -ldflags "-X github.com/kylebegeman/helios/internal/version.number=$version" -o "$work/bedrock-linux" ./cmd/helios)
checksum=$(shasum -a 256 "$work/bedrock-linux" | awk '{print $1}')
client target set lane --ssh "$box" --port "$PORT" --identity "$KEY" --known-hosts "$KNOWN_HOSTS"
lane_run "install -d -m 700 '$remote_work'"
scp -q "${scp_opts[@]}" "$work/bedrock-linux" "$box:$remote_work/bedrock-candidate"

printf '== upgrade an existing build through its journaled workflow\n'
if lane_run 'test -x /usr/local/bin/bedrock'; then
  prior_upgrade=$(lane_run 'bedrock history --limit 1 --json' | python3 -c 'import json,sys; r=json.load(sys.stdin); print(r[0]["id"] if r else "none")')
  lane_run "chmod 755 '$remote_work/bedrock-candidate'; nohup bedrock upgrade '$remote_work/bedrock-candidate' --yes >'$remote_work/upgrade.log' 2>&1 </dev/null &"
  ready=false
  for _ in $(seq 1 90); do
    if [[ "$(lane_run 'sha256sum /usr/local/bin/bedrock' | awk '{print $1}')" == "$checksum" ]] && lane_run 'bedrock daemon status' 2>/dev/null | rg -q "daemon $version answering"; then
      if lane_run 'bedrock history --limit 1 --json' | python3 -c 'import json,sys; r=json.load(sys.stdin); sys.exit(0 if r and r[0]["id"]!=sys.argv[1] and r[0]["kind"]=="host.upgrade" and r[0]["status"]=="succeeded" else 1)' "$prior_upgrade"; then
        ready=true
        break
      fi
    fi
    sleep 2
  done
  if [[ "$ready" != true ]]; then
    lane_run "cat '$remote_work/upgrade.log'; bedrock history --limit 1 --json" >&2
    echo 'Product NOT proven: the checkpointed upgrade did not complete.' >&2
    exit 1
  fi
  lane_run 'python3 - <<'"'"'PY'"'"'
import json,os,stat
m="/usr/local/lib/bedrock/state-checkpoint.json"
r=json.load(open(m))
assert r["operation_id"] and r["state_version"]==1 and os.path.isfile(r["path"])
assert stat.S_IMODE(os.stat(m).st_mode)==0o600
assert stat.S_IMODE(os.stat(r["path"]).st_mode)==0o600
PY'
fi

printf '== portable profile bootstrap and recipient-bound integration transfer\n'
client --state-dir "$work/client-state" secret recipient >/dev/null
printf 'smtp_host=127.0.0.1\nsmtp_port=1025\nfrom=bedrock@lane.invalid\nto=fixture@lane.invalid\n' | client --state-dir "$work/client-state" integration set email >/dev/null
client host profile write "$work/profile.json" --hostname bedrock-lane --swap 1 --timezone UTC --version "$version" --checksum "linux_$LANE_ARCH=$checksum" --integration email
client host profile check "$work/profile.json" >/dev/null
client --state-dir "$work/client-state" --target lane host bootstrap --profile "$work/profile.json" --binary "$work/bedrock-linux" --journal-dir "$work/bootstrap" --plan --json >"$work/bootstrap-plan.json"
bootstrap_digest=$(field digest <"$work/bootstrap-plan.json")
client --state-dir "$work/client-state" --target lane host bootstrap --profile "$work/profile.json" --binary "$work/bedrock-linux" --journal-dir "$work/bootstrap" --digest "$bootstrap_digest"
br integration list --json | python3 -c 'import json,sys; rows=json.load(sys.stdin)["integrations"]; assert any(r["name"]=="email" and r["set"] for r in rows)'
lane_run 'test "$(hostname)" = bedrock-lane && test -f /etc/bedrock/host.json && docker info >/dev/null && systemctl is-active bedrock >/dev/null'

printf '== remote machine and connection binding\n'
machine=$(br capabilities --json | field machine_id)
client target set lane --ssh "$box" --port "$PORT" --identity "$KEY" --known-hosts "$KNOWN_HOSTS" --machine-id "$machine"
br kernel exercise --dir "$remote_work/markers" --steps 2 --plan --json >"$work/remote-plan.json"
digest=$(field digest <"$work/remote-plan.json")
cp "$KNOWN_HOSTS" "$work/other-known-hosts"
client target set other --ssh "$box" --port "$PORT" --identity "$KEY" --known-hosts "$work/other-known-hosts" --machine-id "$machine"
before=$(last_operation)
expect_refusal 'another target|connection|bound' client --target other kernel exercise --dir "$remote_work/markers" --steps 2 --digest "$digest"
[[ "$(last_operation)" == "$before" ]]
client target set wrong --ssh "$box" --port "$PORT" --identity "$KEY" --known-hosts "$KNOWN_HOSTS" --machine-id linux:00000000000000000000000000000000
expect_refusal 'identity|machine' client --target wrong history --json
[[ "$(last_operation)" == "$before" ]]
br kernel exercise --dir "$remote_work/markers" --steps 2 --digest "$digest"
lane_run "test -f '$remote_work/markers/step-1' && test -f '$remote_work/markers/step-2'"

printf '== source upload, read-only host preflight and stale-source refusal\n'
mkdir "$work/hello" "$work/preview" "$work/notes" "$work/candidate"
cat >"$work/hello/helios.yaml" <<'YAML'
app: hello
workloads:
  worker:
    kind: worker
    image: busybox:1.37
    command: [sh, -c, 'trap "exit 0" TERM INT; while :; do sleep 10 & wait "$!"; done']
YAML
before=$(last_operation)
br check "$work/hello" --host --json | python3 -c 'import json,sys; r=json.load(sys.stdin); assert r["valid"] and r["host_preflight"]["valid"], r.get("diagnostics")'
[[ "$(last_operation)" == "$before" ]]
br deploy "$work/hello" --plan --json >"$work/source-plan.json"
source_digest=$(field digest <"$work/source-plan.json")
printf '\n# changed source after approval\n' >>"$work/hello/helios.yaml"
expect_refusal 'source|digest|bound' br deploy "$work/hello" --digest "$source_digest"
[[ "$(last_operation)" == "$before" ]]
br deploy "$work/hello" --yes
hello_revision=$(active_revision hello)

printf '== preview retention and reviewable cleanup\n'
cat >"$work/preview/helios.yaml" <<'YAML'
app: lane-preview
preview: {of: hello, branch: fixture}
workloads:
  worker:
    kind: worker
    image: busybox:1.37
    command: [sh, -c, 'trap "exit 0" TERM INT; while :; do sleep 10 & wait "$!"; done']
YAML
br deploy "$work/preview" --yes
br preview pin lane-preview --yes
br preview prune hello --older-than 1ns --plan --json | python3 -c 'import json,sys; assert json.load(sys.stdin)==[]'
br preview ls --json | python3 -c 'import json,sys; rows=json.load(sys.stdin); assert len(rows)==1 and rows[0]["preview"]=="lane-preview" and rows[0]["pinned"]'
br preview unpin lane-preview --yes
br preview prune hello --older-than 1ns --plan --json >"$work/prune-plan.json"
prune_digest=$(field digest <"$work/prune-plan.json")
br preview prune hello --older-than 1ns --digest "$prune_digest"
br preview ls --json | python3 -c 'import json,sys; assert json.load(sys.stdin)==[]'
[[ "$(active_revision hello)" == "$hello_revision" ]]

printf '== authenticated HTTPS checks and cross-origin redirect refusal\n'
fixture_started=true
lane_run "sh -s -- '$remote_work'" <<'REMOTE'
set -eu
work=$1
openssl req -x509 -newkey rsa:2048 -nodes -keyout "$work/auth.key" -out "$work/auth.crt" -days 1 -subj /CN=bedrock-lane-auth -addext subjectAltName=IP:127.0.0.1 >/dev/null 2>&1
chmod 600 "$work/auth.key"
install -m 644 "$work/auth.crt" /usr/local/share/ca-certificates/bedrock-lane-auth.crt
update-ca-certificates >/dev/null
cat > "$work/auth.py" <<'PY'
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
import ssl,sys
work=Path(sys.argv[1])
class Handler(BaseHTTPRequestHandler):
 def log_message(self,*args): pass
 def do_GET(self):
  if self.path == '/redirect':
   self.send_response(302); self.send_header('Location','https://127.0.0.1:18444/leak'); self.end_headers(); return
  allowed=self.headers.get('Authorization')=='Bearer lane-fixture-check'
  self.send_response(200 if allowed else 403); self.end_headers()
  self.wfile.write(b'authenticated fixture' if allowed else b'denied')
server=HTTPServer(('127.0.0.1',18443),Handler)
ctx=ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER); ctx.load_cert_chain(work/'auth.crt',work/'auth.key')
server.socket=ctx.wrap_socket(server.socket,server_side=True)
server.serve_forever()
PY
cat > /etc/systemd/system/bedrock-lane-auth.service <<EOF
[Unit]
Description=Disposable Bedrock product proof HTTPS fixture
[Service]
ExecStart=/usr/bin/python3 $work/auth.py $work
Restart=on-failure
EOF
systemctl daemon-reload
systemctl start bedrock-lane-auth.service
for n in $(seq 1 15); do curl -s -o /dev/null https://127.0.0.1:18443/guard && break; sleep 1; done
test "$(curl -s -o /dev/null -w '%{http_code}' https://127.0.0.1:18443/guard)" = 403
printf 'Bearer lane-fixture-check\n' | bedrock secret set hello CHECK_TOKEN >/dev/null
systemctl restart bedrock
REMOTE
cat >>"$work/hello/helios.yaml" <<'YAML'
checks:
  - url: https://127.0.0.1:18443/guard
    context: edge
    contains: authenticated fixture
    headers_from_secrets: {Authorization: CHECK_TOKEN}
    within: 2s
YAML
br check "$work/hello" --host --json | python3 -c 'import json,sys; r=json.load(sys.stdin); assert r["valid"], r.get("diagnostics")'
sleep 1
br deploy "$work/hello" --yes
hello_revision=$(active_revision hello)
sed 's@18443/guard@18443/redirect@' "$work/hello/helios.yaml" >"$work/hello/bedrock.changed"
mv "$work/hello/bedrock.changed" "$work/hello/helios.yaml"
sleep 1
expect_refusal 'another origin' br deploy "$work/hello" --yes
[[ "$(active_revision hello)" == "$hello_revision" ]]
lane_run 'test "$(curl -s -H "Authorization: Bearer lane-fixture-check" https://127.0.0.1:18443/guard)" = "authenticated fixture"'
sed 's@18443/redirect@18443/guard@' "$work/hello/helios.yaml" >"$work/hello/bedrock.changed"
mv "$work/hello/bedrock.changed" "$work/hello/helios.yaml"
# Leave a usable app after the temporary authenticated-check fixture is removed.
cat >"$work/hello/helios.yaml" <<'YAML'
app: hello
workloads:
  worker:
    kind: worker
    image: busybox:1.37
    command: [sh, -c, 'trap "exit 0" TERM INT; while :; do sleep 10 & wait "$!"; done']
YAML
sleep 1
br deploy "$work/hello" --yes

printf '== real backup, scratch release rehearsal, and data-aware rollback\n'
"$here/setup-fixtures.sh" storage
cat >"$work/notes/helios.yaml" <<'YAML'
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
br deploy "$work/notes" --yes
[[ "$(notes_marker)" == original ]]
notes_revision=$(active_revision notes)
br backup notes --yes
cat >"$work/candidate/helios.yaml" <<'YAML'
app: notes
release: {data_compatibility: restore-required}
workloads:
  worker:
    kind: worker
    image: busybox:1.37
    user: "0"
    command: [sh, -c, 'trap "exit 0" TERM INT; while :; do sleep 10 & wait "$!"; done']
    mounts: [{volume: files, path: /files}]
    health: {command: [sh, -c, 'test "$(cat /files/proof)" = candidate'], timeout: 5s}
  migrate:
    kind: release
    image: busybox:1.37
    user: "0"
    command: [sh, -c, 'printf candidate >/files/proof']
    mounts: [{volume: files, path: /files}]
data:
  volumes: {files: {}}
YAML
br rehearse "$work/candidate" --yes
[[ "$(notes_marker)" == original ]]
[[ "$(active_revision notes)" == "$notes_revision" ]]
lane_run 'python3 - <<'"'"'PY'"'"'
import json,glob
proofs=glob.glob("/var/lib/bedrock/rehearsals/notes/*.json")
assert proofs
r=json.load(open(sorted(proofs)[-1]))
assert r["status"]=="passed" and r["snapshot"] and r["releases"]==["migrate"]
PY'
lane_run 'test -z "$(docker ps -aq --filter name=bedrock.notes.rehearse.)" && test -z "$(docker volume ls -q --filter name=bedrock.notes.rehearse.)" && test -z "$(docker network ls -q --filter name=bedrock.notes.rehearse.)"'
sleep 1
br deploy "$work/candidate" --yes
[[ "$(notes_marker)" == candidate ]]
expect_refusal 'restore-data|restore-required|checkpoint' br rollback notes --yes
checkpoint=$(lane_run 'python3 - <<'"'"'PY'"'"'
import sqlite3
c=sqlite3.connect("file:/var/lib/bedrock/state.db?mode=ro",uri=True)
r=c.execute("select snapshot from release_boundaries where app=? and status=? order by started_at desc limit 1",("notes","completed")).fetchone()
assert r and len(r[0])==64
print(r[0])
PY')
br rollback notes --restore-data "$checkpoint" --yes
[[ "$(notes_marker)" == original ]]

printf '== exact machine recovery rehearsal, including recorded images and volume data\n'
# The rollback changed the active revision; bind a fresh data snapshot to it.
br backup notes --yes
br backup bedrock --yes
snapshot=$(br backups bedrock --json | python3 -c 'import json,sys; rows=json.load(sys.stdin); r=next(r for r in rows if r["kind"]=="backup" and r["ok"]); assert len(r["snapshot"])==64; print(r["snapshot"])')
recovery_id="lane-product-${snapshot:0:16}"
before_hello=$(active_revision hello)
before_notes=$(active_revision notes)
lane_run "bedrock recover bedrock-lane --snapshot '$snapshot' --identity-file /etc/bedrock/secrets.key --rehearsal --id '$recovery_id' --yes"
br recover report "$recovery_id" --json | python3 -c 'import json,sys; r=json.load(sys.stdin); assert r["status"]=="rehearsed" and r["private_ready"] and all(r["private_ready"].values()) and not r["cutover"]'
[[ "$(active_revision hello)" == "$before_hello" && "$(active_revision notes)" == "$before_notes" && "$(notes_marker)" == original ]]
lane_run 'test -z "$(docker ps -aq --filter name=bedrock.recovery.)" && test -z "$(docker volume ls -q --filter name=bedrock.recovery.)" && test -z "$(docker network ls -q --filter name=bedrock.recovery.)"'

printf '== source handoff rehearsal, persistent final fence, and deliberate resume\n'
br move rehearse notes --to bedrock-lane-target --yes >"$work/rehearsal-handoff.json"
python3 -c 'import json,sys; r=json.load(open(sys.argv[1])); assert r["snapshot"] and r["phase"]=="rehearsal"' "$work/rehearsal-handoff.json"
[[ "$(notes_marker)" == original ]]
br move final notes --to bedrock-lane-target --yes >"$work/final-handoff.json"
lane_run 'test -f /var/lib/bedrock/locks/notes.migration.json && test -z "$(docker ps -q --filter label=bedrock.app=notes --filter label=bedrock.workload=worker)"'
expect_refusal 'fenced|migration|move resume' br deploy "$work/notes" --yes
expect_refusal 'fenced|migration|move resume' br rollback notes --yes
expect_refusal 'target-stopped' br move resume notes --yes
br move resume notes --target-stopped --yes
[[ "$(notes_marker)" == original ]]
lane_run 'test ! -f /var/lib/bedrock/locks/notes.migration.json'
printf 'Product proven: checkpointed upgrade; pinned bootstrap/integration; remote identity, connection and source approval; retained previews; authenticated HTTPS checks; isolated release rehearsal and explicit data rollback; exact machine recovery rehearsal; source migration fence and deliberate resume.\n'
printf 'Public DNS/certificates and cross-host recovery/cutover were not exercised by this proof.\n'
