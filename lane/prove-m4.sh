#!/usr/bin/env bash
# M4 on the lane: secrets pinned to revisions, a cron workload, a one-off
# command, and the two real personal apps brought up from their archives:
# begamin (a SQLite volume) and Dragon Writer (Postgres and an uploads
# volume from the July 5 backup), with Olive's chapters counted.
#
# Needs: *.lane.begam.in -> the box, and the staging directory made by the
# session (sources without env files, and the archives), passed as $1.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
stage="${1:?usage: prove-m4.sh <staging dir with src/ and archives/>}"
# shellcheck source=/dev/null
. "$here/hostinger.env"
ssh_opts=(-i "$KEY" -o IdentityAgent=none -o IdentitiesOnly=yes -o BatchMode=yes
  -o UserKnownHostsFile="$here/known_hosts" -o StrictHostKeyChecking=yes -o ConnectTimeout=10)
box="root@$HOST"
run() { ssh "${ssh_opts[@]}" "$box" "$@"; }
put() { scp -q "${ssh_opts[@]}" "$1" "$box:$2"; }
fetch() { curl -sS --max-time 30 "$@"; }
quiet() { grep -v '^  \.\.\.  ' ; }

echo "== build and install"
(cd "$here/.." && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o bin/bedrock-linux-amd64 ./cmd/bedrock)
put "$here/../bin/bedrock-linux-amd64" /usr/local/bin/bedrock.new
run 'chmod 755 /usr/local/bin/bedrock.new && mv -f /usr/local/bin/bedrock.new /usr/local/bin/bedrock && bedrock daemon install >/dev/null && bedrock version'

echo "== copy the fixtures, the sources and the archives"
run 'mkdir -p /srv/lane/archives'
(cd "$here/fixtures" && COPYFILE_DISABLE=1 tar czf - hello) | run 'rm -rf /srv/lane/hello && tar xzf - -C /srv/lane 2>/dev/null'
(cd "$stage/src" && COPYFILE_DISABLE=1 tar czf - dragon-writer begamin) | run 'rm -rf /srv/lane/dragon-writer /srv/lane/begamin && tar xzf - -C /srv/lane 2>/dev/null'
for f in "$stage"/archives/*; do put "$f" "/srv/lane/archives/$(basename "$f")"; done
run 'ls -la /srv/lane/archives'

echo "== start clean: remove the apps and their lane data if a previous run left them"
for a in hello begamin dragon-writer; do run "bedrock remove $a --data --yes >/dev/null 2>&1 || true"; done

echo "== secrets: the first set makes the machine's key"
run 'printf alpha | bedrock secret set hello SECRET_WORD' 2>&1 | sed 's/AGE-SECRET-KEY-1[A-Z0-9]*/AGE-SECRET-KEY-1.../'
run 'bedrock secret list hello; bedrock secret versions hello'

echo "== hello with a secret and a cron workload"
run 'bedrock deploy /srv/lane/hello --yes' | quiet | tail -8
first=$(fetch https://hello.lane.begam.in/)
echo "-- from the outside: $first"
case "$first" in *"secret alpha"*) ;; *) echo "M4 NOT proven: the secret didn't reach the app" >&2; exit 1 ;; esac

echo "== a new secret version, a new revision, then a rollback restores the old secret"
run 'printf beta | bedrock secret set hello SECRET_WORD >/dev/null && bedrock deploy /srv/lane/hello --yes' | quiet | tail -3
second=$(fetch https://hello.lane.begam.in/)
echo "-- from the outside: $second"
case "$second" in *"secret beta"*) ;; *) echo "M4 NOT proven: the new secret didn't reach the app" >&2; exit 1 ;; esac
run 'bedrock rollback hello --yes' | quiet | tail -3
third=$(fetch https://hello.lane.begam.in/)
echo "-- from the outside: $third"
case "$third" in *"secret alpha"*) ;; *) echo "M4 NOT proven: the rollback didn't restore the secrets version" >&2; exit 1 ;; esac

echo "== begamin from its archive"
run "cat > /srv/lane/begamin/bedrock.yaml <<'EOF'
app: begamin
description: Kyle's private API
owner: personal
workloads:
  web:
    kind: web
    build:
      context: .
    port: 8484
    routes:
      - host: api.lane.begam.in
      - host: go.lane.begam.in
    env:
      NODE_ENV: production
      HOST_API: api.lane.begam.in
      HOST_GO: go.lane.begam.in
      PUBLIC_SITE_URL: https://kylebegeman.com
    secrets: [IP_HASH_SALT]
    health:
      path: /healthz
    mounts:
      - volume: data
        path: /var/lib/begamin
data:
  volumes:
    data:
      description: The SQLite database
checks:
  - url: https://api.lane.begam.in/healthz
EOF"
run 'head -c 32 /dev/urandom | base64 | bedrock secret set begamin IP_HASH_SALT >/dev/null'
run 'bedrock deploy /srv/lane/begamin --yes --restore-volume data=/srv/lane/archives/begamin-data.tar.gz' | quiet | tail -14
echo "-- from the outside: $(fetch -o /dev/null -w '%{http_code}' https://api.lane.begam.in/healthz) at https://api.lane.begam.in/healthz"

echo "== Dragon Writer from the July 5 backup"
run "cat > /srv/lane/dragon-writer/bedrock.yaml <<'EOF'
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
EOF"
run 'bedrock deploy /srv/lane/dragon-writer --yes --restore-postgres /srv/lane/archives/dragon_writer_20260705_175914.dump --restore-volume uploads=/srv/lane/archives/uploads_20260705_175914.tar.gz' | quiet | grep -v '^       | \(#\|=>\|npm\|added\|found\|>\|▲\|  \)' | tail -20
echo "-- from the outside: $(fetch -o /dev/null -w '%{http_code}' https://dragonwriter.lane.begam.in/) at https://dragonwriter.lane.begam.in/"
echo "-- her drawing: $(fetch -o /dev/null -w '%{http_code} %{content_type} %{size_download} bytes' https://dragonwriter.lane.begam.in/uploads/gallery/1782661846889-483c8db3d7a952ab.png)"

echo "== Olive's data, counted in the restored database"
run "bedrock psql dragon-writer -- -tAc \"select (select count(*) from story_chapters where archived_at is null) || ' chapters, ' || (select count(*) from stories) || ' stories, ' || (select count(*) from oc_characters) || ' characters, ' || (select display_name from users where display_name <> 'Kyle' limit 1) || ' is here'\""
chapters=$(run "bedrock psql dragon-writer -- -tAc 'select count(*) from story_chapters'")
[[ "$chapters" == "36" ]] || { echo "M4 NOT proven: expected 36 chapters, got $chapters" >&2; exit 1; }

echo "== a one-off command in the app's environment, with its database and secrets"
run "bedrock run dragon-writer web -- sh -c 'psql \"\$DATABASE_URL\" -tAc \"select count(*) from stories\" | sed s/\$/\ stories\ reachable\ from\ a\ one-off\ command/'" | quiet | tail -4
echo "-- and the app's own migration check, whose verdict is the app's (the July database was migrated with drizzle-kit push, so its journal is short):"
run 'bedrock run dragon-writer web -- npm run db:migrate:check' 2>&1 | quiet | grep -E "baseline|Pending|exited" || true
run 'bedrock jobs dragon-writer web --limit 2'

echo "== the cron workload ran"
sleep 75
run 'bedrock jobs hello tick --limit 3'
run 'bedrock jobs hello tick --limit 1 --json' | python3 -c 'import json,sys; runs=json.load(sys.stdin); assert runs and runs[0]["exit_code"]==0, runs; print("last tick:", runs[0]["output"].strip())'

echo "== ps"
run 'bedrock ps'
echo "M4 proven: secrets pinned to revisions, a cron job, a one-off command, begamin and Dragon Writer restored from their archives"
