#!/usr/bin/env bash
# M5 on the lane: backups that run, drills that prove them, a restore for
# real, health watching with one email per outage, the app registry,
# integration credentials, and traffic and resource signals.
#
# Needs the apps prove-m4.sh leaves behind (hello, site, begamin and
# Dragon Writer). The mail sink (mailpit) and the S3 store (MinIO) are lane
# fixtures on the box, not managed by bedrock; on the real machines the
# integrations point at a mail provider and Backblaze B2 instead.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
. "$here/hostinger.env"
: "${LANE_DOMAIN:?set LANE_DOMAIN in lane/hostinger.env; see hostinger.env.example}"
: "${LANE_DW_CHAPTERS:?set LANE_DW_CHAPTERS in lane/hostinger.env; see hostinger.env.example}"
export LANE_DOMAIN LANE_DW_CHAPTERS
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
(cd "$here/.." && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o bin/bedrock-linux-amd64 ./cmd/bedrock)
put "$here/../bin/bedrock-linux-amd64" /usr/local/bin/bedrock.new
run 'chmod 755 /usr/local/bin/bedrock.new && mv -f /usr/local/bin/bedrock.new /usr/local/bin/bedrock && bedrock daemon install >/dev/null && bedrock version'
run 'bedrock ps' | grep -q dragon-writer || fail "run prove-m4.sh first; Dragon Writer isn't deployed"

echo "== lane fixtures: a mail sink and an S3 store, neither managed by bedrock"
run "docker rm -f lane-mailpit lane-minio >/dev/null 2>&1 || true; docker volume rm -f lane-minio >/dev/null 2>&1 || true
[ -s /root/.lane-minio-key ] || { umask 077; head -c 18 /dev/urandom | base64 > /root/.lane-minio-key; }
docker run -d --name lane-mailpit -p 127.0.0.1:1025:1025 -p 127.0.0.1:8025:8025 $MAILPIT >/dev/null
docker run -d --name lane-minio -p 127.0.0.1:9000:9000 -e MINIO_ROOT_USER=lane -e MINIO_ROOT_PASSWORD=\"\$(cat /root/.lane-minio-key)\" -v lane-minio:/data $MINIO server /data >/dev/null
sleep 3; curl -sf http://127.0.0.1:9000/minio/health/live >/dev/null && echo 'mail sink and S3 store up'"

echo "== integrations: email to the sink, storage to the S3 store (values never leave the box)"
run "printf 'smtp_host=127.0.0.1\nsmtp_port=1025\nfrom=bedrock@$LANE_DOMAIN\nto=kyle@$LANE_DOMAIN\n' | bedrock integration set email"
run "printf 'kind=s3\nendpoint=http://127.0.0.1:9000\nkey_id=lane\nkey=%s\nbucket_prefix=lane\n' \"\$(cat /root/.lane-minio-key)\" | bedrock integration set storage" 2>&1 | mask
run 'bedrock integration list'

echo "== the registry"
run 'bedrock ls'

echo "== a test alert reaches the mail sink"
run 'bedrock alerts test'
mail_subjects | grep -q "test alert" || fail "the test alert didn't arrive"

echo "== Dragon Writer gets a verify query for its drills (a redeploy; the build is cached)"
run "cat > /srv/lane/dragon-writer/bedrock.yaml <<'EOF'
app: dragon-writer
description: A writing app
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
      - host: dragonwriter.$LANE_DOMAIN
    env:
      NODE_ENV: production
      UPLOAD_DIR: /app/public/uploads
      MAX_UPLOAD_SIZE_MB: \"20\"
      EMAIL_DELIVERY_MODE: log
      ALLOW_PUBLIC_SIGNUP: \"false\"
      NEXT_PUBLIC_APP_URL: https://dragonwriter.$LANE_DOMAIN
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
      description: The drawings people upload
backup:
  verify:
    sql: select count(*) from story_chapters
    at_least: $LANE_DW_CHAPTERS
EOF"
run 'bedrock deploy /srv/lane/dragon-writer --yes' | quiet | grep -v '^       | \(#\|=>\|npm\|added\|found\|>\|▲\|  \)' | tail -4

echo "== backups: Dragon Writer, begamin, hello, and the machine itself; the site has no data"
run 'bedrock backup dragon-writer --yes' | quiet
run 'bedrock backup begamin --yes' | quiet | tail -4
run 'bedrock backup hello --yes' | quiet | tail -4
run 'bedrock backup site --yes' 2>&1 | tail -1 || true
run 'bedrock backup bedrock --yes' | quiet | tail -4
run 'bedrock backups'
run 'bedrock backups --json' | python3 -c '
import json,sys
runs=[r for r in json.load(sys.stdin) if r["kind"]=="backup" and r["ok"]]
apps={r["app"] for r in runs}
assert {"dragon-writer","begamin","hello","bedrock"} <= apps, apps
print("good backups:", ", ".join(sorted(apps)))'

echo "== a drill: restore the latest snapshot beside Dragon Writer, start it on that data, verify, clean up"
run 'bedrock drill dragon-writer --yes' | quiet
run 'bedrock backups dragon-writer --limit 2'
run 'bedrock backups dragon-writer --json' | python3 -c '
import json,os,sys
runs=json.load(sys.stdin)
drill=next(r for r in runs if r["kind"]=="drill")
assert drill["ok"], drill
assert "verify query "+os.environ["LANE_DW_CHAPTERS"] in drill["detail"], drill["detail"]
assert "web answered" in drill["detail"], drill["detail"]
print("drill:", drill["detail"])'
run 'docker ps -a --format "{{.Names}}" | grep -c drill || true' | grep -qx 0 || fail "the drill left containers behind"

echo "== the real thing: hello is removed with its data, restored from its bucket, and deployed again"
before=$(run 'bedrock run hello tick -- sh -c "head -1 /ticks/log"' | quiet | grep -E '^ +\| ' | head -1 | sed 's/^ *| //')
echo "-- first tick before the loss: $before"
run 'bedrock remove hello --data --yes' | quiet | tail -2
run 'bedrock restore hello --yes' | quiet
run 'bedrock deploy /srv/lane/hello --yes' | quiet | tail -3
after=$(run 'bedrock run hello tick -- sh -c "head -1 /ticks/log"' | quiet | grep -E '^ +\| ' | head -1 | sed 's/^ *| //')
echo "-- first tick after the restore: $after"
[[ -n "$before" && "$before" == "$after" ]] || fail "the restored volume doesn't hold the old ticks"

echo "== a scheduled backup: hello backs up every two minutes for a while"
run "sed -i 's/^checks:/backup:\n  schedule: \"*\/2 * * * *\"\nchecks:/' /srv/lane/hello/bedrock.yaml && grep -A1 '^backup:' /srv/lane/hello/bedrock.yaml"
run 'bedrock deploy /srv/lane/hello --yes' | quiet | tail -2
deployed=$(date -u +%s)
sleep 150
run 'bedrock backups hello --limit 3'
run 'bedrock backups hello --json' | python3 -c "
import json,sys,datetime
runs=[r for r in json.load(sys.stdin) if r['kind']=='backup']
newest=runs[0]
started=datetime.datetime.fromisoformat(newest['started_at'].replace('Z','+00:00')).timestamp()
assert started > $deployed - 5 and newest['ok'], newest
print('scheduled backup ran at', newest['started_at'])"

echo "== signals: traffic to hello shows up in status"
for _ in $(seq 40); do fetch -o /dev/null https://hello.$LANE_DOMAIN/; done
sleep 75
run 'bedrock status'
run 'bedrock status --json' | python3 -c '
import json,sys
st=json.load(sys.stdin)
hello=next(a for a in st["apps"] if a["name"]=="hello")
s=hello["signals"]
assert s["requests"] >= 40, s
assert s["memory_bytes"] > 0, s
print("hello: %d requests, p95 %.0f ms, cpu %.1f%%, memory %d bytes" % (s["requests"], s["p95_ms"], s["cpu_percent"], s["memory_bytes"]))'
run 'bedrock status hello'

echo "== an outage: hello is stopped; one email says so within minutes, and the external watch reports it too"
run "bedrock watch add https://hello.$LANE_DOMAIN/"
run 'docker stop $(docker ps -q -f name=bedrock-hello-web) >/dev/null && echo stopped'
wait_for_mail "hello is down" 420 || fail "no outage email within 7 minutes"
wait_for_mail "https://hello.$LANE_DOMAIN/ is down" 120 || fail "the external watch didn't report the outage"
mail_subjects
run 'bedrock alerts'
echo "-- recovery"
run 'docker start $(docker ps -aq -f name=bedrock-hello-web) >/dev/null && echo started'
wait_for_mail "hello recovered" 300 || fail "no recovery email within 5 minutes"
wait_for_mail "https://hello.$LANE_DOMAIN/ recovered" 120 || fail "the external watch didn't recover"
mail_subjects
run 'bedrock alerts'
run "bedrock watch remove https://hello.$LANE_DOMAIN/"
total=$(mail_subjects | wc -l | tr -d ' ')
[[ "$total" == "5" ]] || fail "expected exactly 5 emails (test, 2 down, 2 recovered), got $total"

echo "== back to hello's stock manifest"
run "sed -i '/^backup:/,/^checks:/{/^checks:/!d}' /srv/lane/hello/bedrock.yaml && bedrock deploy /srv/lane/hello --yes" | quiet | tail -1

echo "== history"
run 'bedrock history --limit 12'
echo "M5 proven: backups to per-app buckets, a drill that restores and verifies, a restore for real, a scheduled backup, traffic and resource signals, and one email per outage with recovery"
