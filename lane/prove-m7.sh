#!/usr/bin/env bash
# M7 on the lane, quark's half: an app shaped like Loom's Core. Secrets the
# manifest asks quark to make and derive; a database from another image
# with its own superuser, first-run scripts and a password on every local
# connection; an object store; release workloads in order; one image for
# two workloads; a singleton worker whose health goes through an alias; a
# route to a second port. A failing singleton brings its predecessor back,
# a failing release starts nothing, one-off commands get more secrets and a
# private standard input, and the backup, drill, removal and restore carry
# the object store and the first-run scripts.
#
# Needs the lane after prove-m5.sh: the storage integration is used for the
# backup. Loom's Core is the other half of M7: `pnpm loom core create`
# against core.lane.begam.in, from the Loom repository.
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
quiet() { grep -v '^  \.\.\.  '; }
fail() { echo "M7 NOT proven: $*" >&2; exit 1; }
sql() { run "quark psql notes -- -tAc \"$1\"" | tr -d '\r'; }
# edit <sed expression> changes the fixture's manifest on the box.
edit() { run "sed -i '$1' /srv/lane/notes/quark.yaml"; }
# worker prints the running worker container's name.
worker() { run "docker ps --filter label=quark.app=notes --filter label=quark.workload=worker --format '{{.Names}}'"; }
active() { run 'quark ps --json' | python3 -c 'import json,sys; print(next(r["Revision"] for r in json.load(sys.stdin) if r["App"]=="notes" and r["Workload"]=="api"))'; }

echo "== build and install"
(cd "$here/.." && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o bin/quark-linux-amd64 ./cmd/quark && go build -trimpath -o bin/quark ./cmd/quark)
put "$here/../bin/quark-linux-amd64" /usr/local/bin/quark.new
run 'chmod 755 /usr/local/bin/quark.new && mv -f /usr/local/bin/quark.new /usr/local/bin/quark && quark daemon install >/dev/null && quark version'
run 'quark integration list' | grep -q '^storage' || fail "run prove-m5.sh first: the backup needs the storage integration"

echo "== the first deploy makes the secrets, the data and the bucket, and runs the releases in order"
# From nothing: a notes app an earlier run left goes first, data included.
run 'quark remove notes --data --yes >/dev/null 2>&1 || true'
run 'rm -rf /srv/lane/notes'
(cd "$here/fixtures" && COPYFILE_DISABLE=1 tar czf - notes) | run 'tar xzf - -C /srv/lane 2>/dev/null'
if ! out=$(run 'quark deploy /srv/lane/notes --yes' 2>&1 | quiet); then echo "$out"; fail "the first deploy failed"; fi
echo "$out" | grep -E "made |derived |credentials made|postgres .* ready|object store ready|build api's image|buckets:|migrate:|Bucket created|INSERT|stopped first|running as|ready$|succeeded|failed"
names=$(run 'quark secret list notes --json')
for n in QUARK_POSTGRES_PASSWORD MINIO_ROOT_USER MINIO_ROOT_PASSWORD NOTES_OWNER_PASSWORD NOTES_APP_PASSWORD NOTES_DATABASE_URL PGPASSWORD MC_HOST_LOCAL; do
  echo "$names" | grep -q "\"$n\"" || fail "$n wasn't made"
done
echo "$names" | grep -q '"DATABASE_URL"' && fail "database_url: false still made DATABASE_URL"
echo "-- every secret made or derived; no DATABASE_URL, as the manifest says"

echo "== from the outside: both ports, through the edge"
api=$(fetch https://notes.lane.begam.in/); control=$(fetch https://notes-control.lane.begam.in/)
echo "-- $api"; echo "-- $control"
[[ "$api" == "notes api, revision"* && "$control" == "notes control, revision"* ]] || fail "a route doesn't reach its port"

echo "== one image serves both workloads"
run 'quark ps --json' | python3 -c '
import json,sys
rows={r["Workload"]:r for r in json.load(sys.stdin) if r["App"]=="notes"}
assert rows["api"]["Image"]==rows["worker"]["Image"], rows
print("-- api and worker run", rows["api"]["Image"][:60])'

echo "== the database: another image, its own superuser, roles from the first-run scripts, a password on every connection"
roles=$(sql "select string_agg(rolname, ',' order by rolname) from pg_roles where rolname like 'notes%'")
echo "-- roles: $roles"; [[ "$roles" == "notes_admin,notes_app,notes_owner" ]] || fail "the first-run scripts didn't make the roles"
[[ "$(sql "select extname from pg_extension where extname = 'vector'")" == vector ]] || fail "pgvector's image isn't the one running"
[[ "$(sql "select tableowner from pg_tables where tablename = 'notes'")" == notes_owner ]] || fail "the release didn't run as the owner"
[[ "$(sql "select count(*) from notes")" == 1 ]] || fail "the migrate release didn't insert its row"
run "docker exec quark-notes-postgres grep -c scram-sha-256 /var/lib/postgresql/data/pgdata/pg_hba.conf" | grep -qv '^0$' || fail "local connections don't ask for a password"
echo "-- pgvector, notes_owner's table with 1 row, scram on every connection"

echo "== the object store has the bucket the first release made"
run 'quark run notes buckets -- ls LOCAL' | quiet | grep -q 'notes/' || fail "no notes bucket"
echo "-- LOCAL/notes exists"

echo "== a second deploy stops the singleton worker first and runs the releases again"
edit 's/NOTE: first/NOTE: second/'
before=$(worker)
if ! out=$(run 'quark deploy /srv/lane/notes --yes' 2>&1 | quiet); then echo "$out"; fail "the second deploy failed"; fi
echo "$out" | grep -E "stopped first|worker ready|succeeded"
echo "$out" | grep -q "$before stopped first" || fail "the old worker wasn't stopped before the new one started"
[[ $(worker | wc -l | tr -d ' ') == 1 ]] || fail "more than one worker runs"
[[ "$(fetch https://notes.lane.begam.in/)" == *"note second"* ]] || fail "the second revision doesn't serve"
[[ "$(sql "select count(*) from notes")" == 2 ]] || fail "the releases didn't run again"
good=$(active); good_worker=$(worker)
echo "-- $good serves, one worker ($good_worker), 2 rows"

echo "== a singleton that never becomes ready: its predecessor starts again"
edit 's|    command: \[/notes, worker\]|    command: [/notes, worker]\n    env: {FAIL: "1"}|'
if out=$(run 'quark deploy /srv/lane/notes --yes' 2>&1 | quiet); then echo "$out"; fail "a worker that never becomes ready was deployed"; fi
echo "$out" | grep -E "stopped first|started again|told to fail|failed" | head -5
edit '/    env: {FAIL: "1"}/d'
[[ "$(worker)" == "$good_worker" ]] || fail "the previous worker isn't running again: $(worker)"
[[ "$(active)" == "$good" ]] || fail "the active revision changed"
[[ "$(fetch https://notes.lane.begam.in/)" == *"note second"* ]] || fail "the site stopped serving the good revision"
echo "-- $good_worker runs again; $good still serves (its releases ran: the data moves forward, as a migration does)"

echo "== a release that fails starts nothing new"
run "sed -i \"s/insert into notes (body) values ('deployed')/select 1 \\/ 0/\" /srv/lane/notes/quark.yaml"
if out=$(run 'quark deploy /srv/lane/notes --yes' 2>&1 | quiet); then echo "$out"; fail "a failing release was deployed"; fi
echo "$out" | grep -E "division by zero|nothing new started|failed" | head -3
run "sed -i \"s/select 1 \\/ 0/insert into notes (body) values ('deployed')/\" /srv/lane/notes/quark.yaml"
[[ "$(worker)" == "$good_worker" && "$(active)" == "$good" ]] || fail "a failing release touched the running revision"
[[ $(run "docker ps -a --filter label=quark.app=notes --format '{{.Names}}'" | grep -vc "$good\|postgres\|objects" || true) == 0 ]] || fail "a failing release left containers behind"
echo "-- the worker was never stopped, and no new container exists"

echo "== one-off commands: another secret by name, and a standard input nobody records"
got=$(run "quark run notes api --secret PGPASSWORD -- sh -c 'test -n \"\$PGPASSWORD\" && echo has-the-secret'" | quiet)
[[ "$got" == *has-the-secret* ]] || fail "--secret didn't reach the command: $got"
word="lane-owner-code-$RANDOM$RANDOM"
want=$(printf %s "$word" | shasum -a 256 | cut -d' ' -f1)
got=$(printf %s "$word" | run "quark run notes api --stdin -- sh -c 'read -r v; printf %s \"\$v\" | sha256sum'")
[[ "$got" == "$want"* ]] || fail "--stdin didn't reach the command: $got"
run "quark jobs notes api --json" | grep -q "$word" && fail "the standard input reached the job record"
code=0; run "quark run notes api --stdin -- sh -c 'exit 7'" </dev/null || code=$?
[[ $code == 7 ]] || fail "quark run --stdin exited $code, not the command's 7"
echo "-- the secret arrived; the input arrived and was recorded nowhere; the command's exit code came back"

echo "== backup and drill: the database with its first-run scripts, and the object store"
if ! out=$(run 'quark backup notes --yes' 2>&1 | quiet); then echo "$out"; fail "the backup failed"; fi
echo "$out" | grep -E "database dumped|snapshot|succeeded"
if ! out=$(run 'quark drill notes --yes' 2>&1 | quiet); then echo "$out"; fail "the drill failed"; fi
echo "$out" | grep -E "restored|tables|verify query|object store answered|api answered|succeeded"
echo "$out" | grep -q "object store answered" || fail "the drill didn't bring the object store back"
echo "$out" | grep -q "api answered" || fail "the drill's api didn't reach its restored database"
echo "$out" | grep -q "worker answered" || fail "the drill omitted the API's worker dependency"
[[ "$(run "docker ps -a --format '{{.Names}}' | grep -c 'notes.drill' || true")" == 0 ]] || fail "the drill left containers behind"

echo "== exposure names the object store"
run 'quark exposure notes' | grep -E "notes (database|object store)" | head -4
run 'quark exposure notes' | grep -q "notes object store" || fail "quark exposure doesn't know the object store"

echo "== remove with its data, then restore from the bucket and deploy again"
run 'quark remove notes --data --yes' 2>&1 | quiet | grep -E "removed|stopped|forgotten" | head -12
[[ "$(run "docker volume ls -q | grep -c '^quark-notes-' || true")" == 0 ]] || fail "volumes outlived quark remove --data"
run 'test ! -e /var/lib/quark/apps/notes' || fail "the first-run scripts' copy outlived the app"
if ! out=$(run 'quark restore notes --yes' 2>&1 | quiet); then echo "$out"; fail "the restore failed"; fi
echo "$out" | grep -E "snapshot|volume|postgres .* ready|database restored|succeeded"
if ! out=$(run 'quark deploy /srv/lane/notes --yes' 2>&1 | quiet); then echo "$out"; fail "the deploy after the restore failed"; fi
[[ "$(sql "select count(*) from notes")" == 4 ]] || fail "the restored database didn't keep its rows: $(sql "select count(*) from notes")"
[[ "$(sql "select tableowner from pg_tables where tablename = 'notes'")" == notes_owner ]] || fail "the restore lost the table's owner"
run 'quark run notes buckets -- ls LOCAL' | quiet | grep -q 'notes/' || fail "the restored object store lost its bucket"
echo "-- 3 restored rows and the new deploy's, still notes_owner's, and the bucket is back"

echo "M7 proven on the lane (quark's half): a Core-shaped app deploys, fails safely, and comes back from its bucket"
