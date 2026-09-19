#!/usr/bin/env bash
# Cloudflare itself, on the lane. The box's cloudflare integration holds a
# real token (Zone read and DNS edit on begam.in, usable only from the box's
# two addresses), which its owner typed in on the box. Deploys make and
# remove real records, the audit reads the real zone, and nothing outside
# lane.begam.in is written. The token never leaves the box.
#
# Needs the lane after prove-m6.sh, with the stand-in gone.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
. "$here/hostinger.env"
ssh_opts=(-i "$KEY" -o IdentityAgent=none -o IdentitiesOnly=yes -o BatchMode=yes
  -o UserKnownHostsFile="$here/known_hosts" -o StrictHostKeyChecking=yes -o ConnectTimeout=10)
box="root@$HOST"
run() { ssh "${ssh_opts[@]}" "$box" "$@"; }
fetch() { curl -sS --max-time 30 "$@"; }
quiet() { grep -v '^  \.\.\.  '; }
fail() { echo "Cloudflare NOT proven: $*" >&2; exit 1; }
# verdict <host> prints what quark dns says about a routed host.
verdict() { run 'quark dns --json' | python3 -c "import json,sys; print(next((s['verdict'] for s in json.load(sys.stdin) if s['host']=='$1'), 'not routed'))"; }
# audit_mentions <host> succeeds when the audit reports the host.
audit_mentions() { run 'quark dns audit --json' | python3 -c "import json,sys; sys.exit(0 if any(f['host']=='$1' for f in (json.load(sys.stdin) or [])) else 1)"; }

echo "== the integration is Cloudflare's own API"
run 'quark integration list --json' | python3 -c '
import json,sys
cf=[i for i in json.load(sys.stdin)["integrations"] if i["name"]=="cloudflare"][0]
assert cf["set"] and "api" not in cf["fields"], cf
print("cloudflare set", cf["at"][:16], "with no API override")'

echo "== what the real zone says about every routed host"
run 'quark dns'

echo "== begamin deploys and keeps its two records in the real zone"
run 'quark deploy /srv/lane/begamin --yes' | quiet | grep -E "api.lane|go.lane|succeeded|failed"
for h in api.lane.begam.in go.lane.begam.in; do
  v=$(verdict $h); echo "-- $h: $v"; [[ "$v" == ok ]] || fail "$h's record isn't quark's in the real zone"
done

echo "== a route's record follows it: made with the route, removed when the route goes"
run "grep -q dnstest /srv/lane/hello/quark.yaml || sed -i 's|      - host: hello.lane.begam.in|      - host: hello.lane.begam.in\n      - host: dnstest.lane.begam.in\n        dns: direct|' /srv/lane/hello/quark.yaml"
run 'quark deploy /srv/lane/hello --yes' | quiet | grep -E "dnstest|succeeded|failed"
[[ "$(verdict dnstest.lane.begam.in)" == ok ]] || fail "dnstest.lane.begam.in has no record of quark's"
echo "-- from the outside: $(fetch https://dnstest.lane.begam.in/)"
run "sed -i '/dnstest.lane.begam.in/,+1d' /srv/lane/hello/quark.yaml && quark deploy /srv/lane/hello --yes" | quiet | grep -E "record removed|succeeded|failed"
audit_mentions dnstest.lane.begam.in && fail "dnstest.lane.begam.in's record outlived its route"
echo "-- the audit finds nothing left of dnstest.lane.begam.in"

echo "== removing begamin removes its records; deploying it again brings them back"
run 'quark remove begamin --yes' | quiet | grep -E "record|forgotten"
for h in api.lane.begam.in go.lane.begam.in; do audit_mentions $h && fail "$h's record outlived begamin"; done
echo "-- the audit finds neither record"
run 'quark deploy /srv/lane/begamin --yes' | quiet | grep -E "made|succeeded|failed"
[[ "$(verdict api.lane.begam.in)" == ok ]] || fail "api.lane.begam.in's record didn't come back"
echo "-- from the outside: $(fetch -o /dev/null -w '%{http_code}' https://api.lane.begam.in/healthz) at https://api.lane.begam.in/healthz"

echo "== exploratory: a proxied route, reached through Cloudflare's own addresses"
# Two levels below begam.in, so Cloudflare's free certificate doesn't cover
# it: plain HTTP goes through the proxy, HTTPS fails at Cloudflare's edge,
# and quark says so. A name one level below the zone is what proxied suits.
run "grep -q proxytest /srv/lane/hello/quark.yaml || sed -i 's|      - host: hello.lane.begam.in|      - host: hello.lane.begam.in\n      - host: proxytest.lane.begam.in\n        dns: proxied|' /srv/lane/hello/quark.yaml"
run 'quark deploy /srv/lane/hello --yes' 2>&1 | quiet | grep -E "proxytest|note:|succeeded|failed" || true
proxy=""
for _ in $(seq 1 30); do
  proxy=$(dig +short @1.1.1.1 proxytest.lane.begam.in A | head -1)
  [[ -n "$proxy" && "$proxy" != "$HOST" ]] && break
  sleep 5
done
[[ -n "$proxy" && "$proxy" != "$HOST" ]] || fail "public DNS never answered proxytest.lane.begam.in with Cloudflare's addresses"
echo "-- public DNS answers with Cloudflare's $proxy"
echo "-- plain HTTP through Cloudflare: $(fetch --resolve "proxytest.lane.begam.in:80:$proxy" -o /dev/null -w '%{http_code}' http://proxytest.lane.begam.in/ 2>&1 || true)"
echo "-- HTTPS through Cloudflare: $(fetch --resolve "proxytest.lane.begam.in:443:$proxy" -o /dev/null -w '%{http_code}' https://proxytest.lane.begam.in/ 2>&1 | tail -1 || true)"
run "sed -i '/proxytest.lane.begam.in/,+1d' /srv/lane/hello/quark.yaml && quark deploy /srv/lane/hello --yes" | quiet | grep -E "record removed|succeeded|failed"
audit_mentions proxytest.lane.begam.in && fail "proxytest.lane.begam.in's record outlived its route"
echo "-- proxytest.lane.begam.in's record is gone again"

echo "== the audit of the real zone"
run 'quark dns audit'
echo "Cloudflare proven: records made, kept, pruned and removed in the real begam.in zone from the box"
