#!/usr/bin/env bash
# M5 on the lane: backups that run, drills that prove them, a restore for
# real, health watching with one email per outage, the app registry,
# integration credentials, and traffic and resource signals.
#
# Needs the apps prove-m4.sh leaves behind (hello, site, begamin and
# Dragon Writer). The mail sink (mailpit) and the S3 store (MinIO) are lane
# fixtures on the box, not managed by quark; on the real machines the
# integrations point at a mail provider and Backblaze B2 instead.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
. "$here/hostinger.env"
ssh_opts=(-i "$KEY" -o IdentityAgent=none -o IdentitiesOnly=yes -o BatchMode=yes
  -o UserKnownHostsFile="$here/known_hosts" -o StrictHostKeyChecking=yes -o ConnectTimeout=10)
box="root@$HOST"
run() { ssh "${ssh_opts[@]}" "$box" "$@"; }
put() { scp -q "${ssh_opts[@]}" "$1" "$box:$2"; }
fetch() { curl -sS --max-time 30 "$@"; }
quiet() { grep -v '^  \.\.\.  ' ; }
mask() { sed -E 's/^  [0-9a-f]{48}$/  (48-character password shown once; masked in this transcript)/'; }
fail() { echo "M5 NOT proven: $*" >&2; exit 1; }
# mail_subjects prints every subject the sink has received, one per line.
mail_subjects() { run "curl -s 'http://127.0.0.1:8025/api/v1/messages?limit=50' | python3 -c 'import json,sys; [print(m[\"Subject\"]) for m in json.load(sys.stdin)[\"messages\"]]'"; }
# wait_for_mail <pattern> <seconds> waits until a subject matches.
wait_for_mail() {
  local deadline=$((SECONDS + $2))
  while (( SECONDS < deadline )); do
    if mail_subjects | grep -q -- "$1"; then return 0; fi
    sleep 15
  done
  return 1
}

MAILPIT="axllent/mailpit:v1.31.2@sha256:74d609a42ec279aa63c6b4622a6fa9b5408d1ad5b1d76a1c4be40a265ce0863d"
MINIO="quay.io/minio/minio:latest"

echo "== build and install"
(cd "$here/.." && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o bin/quark-linux-amd64 ./cmd/quark)
put "$here/../bin/quark-linux-amd64" /usr/local/bin/quark.new
run 'chmod 755 /usr/local/bin/quark.new && mv -f /usr/local/bin/quark.new /usr/local/bin/quark && quark daemon install >/dev/null && quark version'
run 'quark ps' | grep -q dragon-writer || fail "run prove-m4.sh first; Dragon Writer isn't deployed"

echo "== lane fixtures: a mail sink and an S3 store, neither managed by quark"
run "docker rm -f lane-mailpit lane-minio >/dev/null 2>&1 || true; docker volume rm -f lane-minio >/dev/null 2>&1 || true
[ -s /root/.lane-minio-key ] || { umask 077; head -c 18 /dev/urandom | base64 > /root/.lane-minio-key; }
docker run -d --name lane-mailpit -p 127.0.0.1:1025:1025 -p 127.0.0.1:8025:8025 $MAILPIT >/dev/null
docker run -d --name lane-minio -p 127.0.0.1:9000:9000 -e MINIO_ROOT_USER=lane -e MINIO_ROOT_PASSWORD=\"\$(cat /root/.lane-minio-key)\" -v lane-minio:/data $MINIO server /data >/dev/null
sleep 3; curl -sf http://127.0.0.1:9000/minio/health/live >/dev/null && echo 'mail sink and S3 store up'"

echo "== integrations: email to the sink, storage to the S3 store (values never leave the box)"
run "printf 'smtp_host=127.0.0.1\nsmtp_port=1025\nfrom=quark@lane.begam.in\nto=kyle@lane.begam.in\n' | quark integration set email"
run "printf 'kind=s3\nendpoint=http://127.0.0.1:9000\nkey_id=lane\nkey=%s\nbucket_prefix=lane\n' \"\$(cat /root/.lane-minio-key)\" | quark integration set storage" 2>&1 | mask
run 'quark integration list'

echo "== the registry"
run 'quark ls'

echo "== a test alert reaches the mail sink"
run 'quark alerts test'
mail_subjects | grep -q "test alert" || fail "the test alert didn't arrive"

echo "== Dragon Writer gets a verify query for its drills (a redeploy; the build is cached)"
run "cat > /srv/lane/dragon-writer/quark.yaml <<'EOF'
app: dragon-writer
description: Olive's writing app
owner: personal
workloads:
  web:
    kind: web
    build:
      context: .
      args:
        DATABASE_URL: postgres://build:build@localhost:5432/build
    port: 3000
    routes:
      - host: dragonwriter.lane.begam.in
    env:
      NODE_ENV: production
      UPLOAD_DIR: /app/public/uploads
      MAX_UPLOAD_SIZE_MB: \"20\"
      EMAIL_DELIVERY_MODE: log
      ALLOW_PUBLIC_SIGNUP: \"false\"
      NEXT_PUBLIC_APP_URL: https://dragonwriter.lane.begam.in
    health:
      path: /
      timeout: 120s
    mounts:
      - volume: uploads
        path: /app/public/uploads
    resources:
      memory: 1g
data:
  postgres:
    version: \"16\"
  volumes:
    uploads:
      description: Her drawings
backup:
  verify:
    sql: select count(*) from story_chapters
    at_least: 36
EOF"
run 'quark deploy /srv/lane/dragon-writer --yes' | quiet | grep -v '^       | \(#\|=>\|npm\|added\|found\|>\|▲\|  \)' | tail -4

echo "== backups: Dragon Writer, begamin, hello, and the machine itself; the site has no data"
run 'quark backup dragon-writer --yes' | quiet
run 'quark backup begamin --yes' | quiet | tail -4
run 'quark backup hello --yes' | quiet | tail -4
run 'quark backup site --yes' 2>&1 | tail -1 || true
run 'quark backup quark --yes' | quiet | tail -4
run 'quark backups'
run 'quark backups --json' | python3 -c '
import json,sys
runs=[r for r in json.load(sys.stdin) if r["kind"]=="backup" and r["ok"]]
apps={r["app"] for r in runs}
assert {"dragon-writer","begamin","hello","quark"} <= apps, apps
print("good backups:", ", ".join(sorted(apps)))'

echo "== a drill: restore the latest snapshot beside Dragon Writer, start it on that data, verify, clean up"
run 'quark drill dragon-writer --yes' | quiet
run 'quark backups dragon-writer --limit 2'
run 'quark backups dragon-writer --json' | python3 -c '
import json,sys
runs=json.load(sys.stdin)
drill=next(r for r in runs if r["kind"]=="drill")
assert drill["ok"], drill
assert "verify query 36" in drill["detail"], drill["detail"]
assert "web answered" in drill["detail"], drill["detail"]
print("drill:", drill["detail"])'
run 'docker ps -a --format "{{.Names}}" | grep -c drill || true' | grep -qx 0 || fail "the drill left containers behind"

echo "== the real thing: hello is removed with its data, restored from its bucket, and deployed again"
before=$(run 'quark run hello tick -- sh -c "head -1 /ticks/log"' | quiet | grep -E '^ +\| ' | head -1 | sed 's/^ *| //')
echo "-- first tick before the loss: $before"
run 'quark remove hello --data --yes' | quiet | tail -2
run 'quark restore hello --yes' | quiet
run 'quark deploy /srv/lane/hello --yes' | quiet | tail -3
after=$(run 'quark run hello tick -- sh -c "head -1 /ticks/log"' | quiet | grep -E '^ +\| ' | head -1 | sed 's/^ *| //')
echo "-- first tick after the restore: $after"
[[ -n "$before" && "$before" == "$after" ]] || fail "the restored volume doesn't hold the old ticks"

echo "== a scheduled backup: hello backs up every two minutes for a while"
run "sed -i 's/^checks:/backup:\n  schedule: \"*\/2 * * * *\"\nchecks:/' /srv/lane/hello/quark.yaml && grep -A1 '^backup:' /srv/lane/hello/quark.yaml"
run 'quark deploy /srv/lane/hello --yes' | quiet | tail -2
deployed=$(date -u +%s)
sleep 150
run 'quark backups hello --limit 3'
run 'quark backups hello --json' | python3 -c "
import json,sys,datetime
runs=[r for r in json.load(sys.stdin) if r['kind']=='backup']
newest=runs[0]
started=datetime.datetime.fromisoformat(newest['started_at'].replace('Z','+00:00')).timestamp()
assert started > $deployed - 5 and newest['ok'], newest
print('scheduled backup ran at', newest['started_at'])"

echo "== signals: traffic to hello shows up in status"
for _ in $(seq 40); do fetch -o /dev/null https://hello.lane.begam.in/; done
sleep 75
run 'quark status'
run 'quark status --json' | python3 -c '
import json,sys
st=json.load(sys.stdin)
hello=next(a for a in st["apps"] if a["name"]=="hello")
s=hello["signals"]
assert s["requests"] >= 40, s
assert s["memory_bytes"] > 0, s
print("hello: %d requests, p95 %.0f ms, cpu %.1f%%, memory %d bytes" % (s["requests"], s["p95_ms"], s["cpu_percent"], s["memory_bytes"]))'
run 'quark status hello'

echo "== an outage: hello is stopped; one email says so within minutes, and the external watch reports it too"
run 'quark watch add https://hello.lane.begam.in/'
run 'docker stop $(docker ps -q -f name=quark-hello-web) >/dev/null && echo stopped'
wait_for_mail "hello is down" 420 || fail "no outage email within 7 minutes"
wait_for_mail "https://hello.lane.begam.in/ is down" 120 || fail "the external watch didn't report the outage"
mail_subjects
run 'quark alerts'
echo "-- recovery"
run 'docker start $(docker ps -aq -f name=quark-hello-web) >/dev/null && echo started'
wait_for_mail "hello recovered" 300 || fail "no recovery email within 5 minutes"
wait_for_mail "https://hello.lane.begam.in/ recovered" 120 || fail "the external watch didn't recover"
mail_subjects
run 'quark alerts'
run 'quark watch remove https://hello.lane.begam.in/'
total=$(mail_subjects | wc -l | tr -d ' ')
[[ "$total" == "5" ]] || fail "expected exactly 5 emails (test, 2 down, 2 recovered), got $total"

echo "== back to hello's stock manifest"
run "sed -i '/^backup:/,/^checks:/{/^checks:/!d}' /srv/lane/hello/quark.yaml && quark deploy /srv/lane/hello --yes" | quiet | tail -1

echo "== history"
run 'quark history --limit 12'
echo "M5 proven: backups to per-app buckets, a drill that restores and verifies, a restore for real, a scheduled backup, traffic and resource signals, and one email per outage with recovery"
