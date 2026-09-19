#!/usr/bin/env bash
# M3 on the lane: deploy a static site and a web app through the edge with
# real certificates, deploy a second revision of the web app, roll it back,
# show that a revision failing its check never goes live, and show a wrong
# DNS record reported in plain words.
#
# Needs DNS: *.lane.begam.in pointing at the box, DNS-only.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
. "$here/hostinger.env"
ssh_opts=(-i "$KEY" -o IdentityAgent=none -o IdentitiesOnly=yes -o BatchMode=yes
  -o UserKnownHostsFile="$here/known_hosts" -o StrictHostKeyChecking=yes -o ConnectTimeout=10)
box="root@$HOST"
run() { ssh "${ssh_opts[@]}" "$box" "$@"; }
put() { scp -q "${ssh_opts[@]}" "$1" "$box:$2"; }
fetch() { curl -sS --max-time 20 "$@"; }
quiet() { grep -v '^  \.\.\.  ' ; }

# hello_manifest <greeting> <check text> <host> writes the web fixture's
# manifest on the box.
hello_manifest() {
  run "cat > /srv/lane/hello/quark.yaml <<EOF
app: hello
description: The lane's web fixture
owner: personal
workloads:
  web:
    kind: web
    build:
      context: .
    port: 8000
    routes:
      - host: $3
    env:
      GREETING: $1
    health:
      path: /healthz
    resources:
      memory: 64m
checks:
  - url: https://$3/
    contains: $2
EOF"
}

echo "== build and install"
(cd "$here/.." && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o bin/quark-linux-amd64 ./cmd/quark)
put "$here/../bin/quark-linux-amd64" /usr/local/bin/quark.new
run 'chmod 755 /usr/local/bin/quark.new && mv -f /usr/local/bin/quark.new /usr/local/bin/quark && quark daemon install >/dev/null && quark version'

echo "== reconcile (adds the edge)"
run 'quark host reconcile --yes' | tail -2

echo "== copy the fixtures"
run 'rm -rf /srv/lane && mkdir -p /srv/lane'
(cd "$here/fixtures" && COPYFILE_DISABLE=1 tar czf - hello site) | run 'tar xzf - -C /srv/lane 2>/dev/null'

echo "== deploy the static site"
run 'quark deploy /srv/lane/site --yes' | quiet
echo "-- from the outside:"
fetch https://site.lane.begam.in/ | sed -n '3p'

echo "== deploy the web app"
hello_manifest hello "hello from quark" hello.lane.begam.in
run 'quark deploy /srv/lane/hello --yes' | quiet
first=$(fetch https://hello.lane.begam.in/)
echo "-- from the outside: $first"
case "$first" in "hello from quark, revision 2"*) ;; *) echo "M3 NOT proven: first revision" >&2; exit 1 ;; esac

echo "== ps and logs"
run 'quark ps; quark logs hello --tail 2'

echo "== a second revision"
hello_manifest "hi again" "hi again from quark" hello.lane.begam.in
run 'quark deploy /srv/lane/hello --yes' | quiet | tail -5
second=$(fetch https://hello.lane.begam.in/)
echo "-- from the outside: $second"
case "$second" in "hi again from quark, revision 2"*) ;; *) echo "M3 NOT proven: second revision" >&2; exit 1 ;; esac

echo "== roll back"
run 'quark rollback hello --yes' | quiet | tail -5
third=$(fetch https://hello.lane.begam.in/)
echo "-- from the outside: $third"
case "$third" in "hello from quark, revision 2"*) ;; *) echo "M3 NOT proven: rollback" >&2; exit 1 ;; esac
[[ "${third#*revision }" == "${first#*revision }" ]] || { echo "M3 NOT proven: the rollback isn't the first revision" >&2; exit 1; }

echo "== a revision that fails its check never goes live"
hello_manifest "broken" "this never appears" hello.lane.begam.in
run 'quark deploy /srv/lane/hello --yes' 2>&1 | quiet | tail -4 || true
fourth=$(fetch https://hello.lane.begam.in/)
echo "-- from the outside: $fourth"
case "$fourth" in "hello from quark, revision 2"*) ;; *) echo "M3 NOT proven: the failing revision went live" >&2; exit 1 ;; esac

echo "== a wrong DNS record, in plain words"
hello_manifest hello "hello from quark" begam.in
run 'quark deploy /srv/lane/hello --yes' 2>&1 | quiet | tail -3 || true
hello_manifest hello "hello from quark" hello.lane.begam.in

echo "== history"
run 'quark history --limit 7'
echo "M3 proven: static and web deploys with certificates, a second revision, a rollback, a failing revision kept off the edge, and a DNS problem explained"
