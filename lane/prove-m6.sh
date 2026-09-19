#!/usr/bin/env bash
# M6 on the lane: git push deploys begamin, and its DNS records appear and
# disappear with it; no app can reach another's network, nor the edge's
# admin API; a name that points at another machine stops a deploy until it
# is pointed here; a GitHub-style webhook deploys a push; quark deploy --to
# sends a working tree from this Mac.
#
# Needs the apps prove-m4.sh and prove-m5.sh leave behind, and begamin's
# repository on this Mac ($1, default ~/Developer/active/begamin). It is
# cloned into a temporary directory and a lane manifest committed there;
# nothing is pushed anywhere but the box. Cloudflare's API is a lane
# fixture (fakeflare) the proof starts on the box; on the real machines the
# cloudflare integration holds a real token.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
begamin_repo="${1:-$HOME/Developer/active/begamin}"
# shellcheck source=/dev/null
. "$here/hostinger.env"
ssh_opts=(-i "$KEY" -o IdentityAgent=none -o IdentitiesOnly=yes -o BatchMode=yes
  -o UserKnownHostsFile="$here/known_hosts" -o StrictHostKeyChecking=yes -o ConnectTimeout=10)
box="root@$HOST"
run() { ssh "${ssh_opts[@]}" "$box" "$@"; }
put() { scp -q "${ssh_opts[@]}" "$1" "$box:$2"; }
fetch() { curl -sS --max-time 30 "$@"; }
quiet() { grep -v '^  \.\.\.  ' | grep -v '^remote:   \.\.\.  ' ; }
fail() { echo "M6 NOT proven: $*" >&2; exit 1; }
# cf <method> <path> [json] talks to the Cloudflare stand-in on the box.
cf() { run "curl -s -X $1 -H \"Authorization: Bearer \$(cat /root/.lane-cloudflare-token)\" -H 'Content-Type: application/json' http://127.0.0.1:8788/client/v4$2 ${3:+-d '$3'}"; }
# records prints the stand-in's A records as name content comment.
records() { cf GET "/zones/zone-begam-in/dns_records?per_page=100" | python3 -c 'import json,sys; [print(r["name"], r["content"], r.get("comment","")) for r in json.load(sys.stdin)["result"]]'; }

work="$(mktemp -d)"
push_key="$work/push-key"
ssh-keygen -q -t ed25519 -N "" -C "lane push key" -f "$push_key"
# The throwaway key stops working when the proof ends, however it ends.
trap 'run "quark git deny begamin \"$(cat "$push_key.pub")\"" >/dev/null 2>&1 || true; rm -rf "$work"' EXIT
push_ssh="ssh -i $push_key -o IdentityAgent=none -o IdentitiesOnly=yes -o BatchMode=yes -o UserKnownHostsFile=$here/known_hosts -o StrictHostKeyChecking=yes"

echo "== build and install; reconcile keeps the quark user that receives pushes"
(cd "$here/.." && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o bin/quark-linux-amd64 ./cmd/quark \
  && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o bin/fakeflare-linux-amd64 ./lane/fixtures/fakeflare \
  && go build -trimpath -o bin/quark ./cmd/quark)
put "$here/../bin/quark-linux-amd64" /usr/local/bin/quark.new
run 'mkdir -p /srv/lane/bin'
put "$here/../bin/fakeflare-linux-amd64" /srv/lane/bin/fakeflare.new
run 'chmod 755 /usr/local/bin/quark.new /srv/lane/bin/fakeflare.new && mv -f /usr/local/bin/quark.new /usr/local/bin/quark && mv -f /srv/lane/bin/fakeflare.new /srv/lane/bin/fakeflare && quark daemon install >/dev/null && quark version'
run 'quark host reconcile --yes' | quiet | grep -E "pushes|succeeded|failed"
run 'quark ps' | grep -q dragon-writer || fail "run prove-m4.sh and prove-m5.sh first"

echo "== lane fixture: a stand-in for Cloudflare's API; the token stays on the box"
run "set -e
[ -s /root/.lane-cloudflare-token ] || { umask 077; head -c 24 /dev/urandom | base64 | tr -d '/+=' > /root/.lane-cloudflare-token; }
docker rm -f lane-cloudflare >/dev/null 2>&1 || true
docker run -d --name lane-cloudflare -p 127.0.0.1:8788:8788 -e FAKEFLARE_TOKEN=\"\$(cat /root/.lane-cloudflare-token)\" -e FAKEFLARE_ZONES=begam.in -v /srv/lane/bin/fakeflare:/fakeflare:ro alpine:3.21 /fakeflare >/dev/null
sleep 1
printf 'token=%s\napi=http://127.0.0.1:8788/client/v4\n' \"\$(cat /root/.lane-cloudflare-token)\" | quark integration set cloudflare"
cf POST /zones/zone-begam-in/dns_records "{\"type\":\"A\",\"name\":\"*.lane.begam.in\",\"content\":\"$HOST\"}" >/dev/null
cf POST /zones/zone-begam-in/dns_records "{\"type\":\"A\",\"name\":\"stale.lane.begam.in\",\"content\":\"$HOST\"}" >/dev/null
cf POST /zones/zone-begam-in/dns_records '{"type":"A","name":"moved.lane.begam.in","content":"198.51.100.7","comment":"quark: hello on old-box"}' >/dev/null
echo "-- the zone the stand-in serves:"; records

echo "== every app redeployed under isolation"
run 'rm -rf /srv/lane/hello /srv/lane/site'
(cd "$here/fixtures" && COPYFILE_DISABLE=1 tar czf - hello site) | run 'tar xzf - -C /srv/lane 2>/dev/null'
run "sed -i 's|    resources:\n      memory: 1g|&|' /srv/lane/dragon-writer/quark.yaml; grep -q 'tmpfs:' /srv/lane/dragon-writer/quark.yaml || sed -i 's|^    mounts:|    tmpfs: [/app/.next/cache]\n    mounts:|' /srv/lane/dragon-writer/quark.yaml; grep -B1 -A1 tmpfs /srv/lane/dragon-writer/quark.yaml"
for a in site hello dragon-writer; do
  run "quark deploy /srv/lane/$a --yes" | quiet | grep -E "running as|volume .* belongs|succeeded|failed"
done

echo "== git push deploys begamin, and its records appear"
# A fresh repository on the box, so the proof can run again.
run 'rm -rf /var/lib/quark-git/begamin.git'
run "quark git allow begamin '$(cat "$push_key.pub")'" | head -1
git clone -q --no-hardlinks "$begamin_repo" "$work/begamin"
cat > "$work/begamin/quark.yaml" <<'EOF'
app: begamin
description: Kyle's private API
owner: personal
repo: github.com/kylebegeman/begamin
workloads:
  web:
    kind: web
    build:
      context: .
    port: 8484
    routes:
      - host: api.lane.begam.in
        dns: direct
      - host: go.lane.begam.in
        dns: direct
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
EOF
g() { git -C "$work/begamin" -c user.name=lane -c user.email=lane@quark "$@"; }
g add quark.yaml && g commit -q -m "lane: deploy with quark"
g remote add quark "quark@$HOST:begamin.git"
GIT_SSH_COMMAND="$push_ssh" g push quark HEAD:main 2>&1 | quiet | tail -8
commit=$(g rev-parse --short=12 HEAD)
echo "-- from the outside: $(fetch -o /dev/null -w '%{http_code}' https://api.lane.begam.in/healthz) at https://api.lane.begam.in/healthz"
running_commit() { run 'quark ls --json' | python3 -c 'import json,sys; print(next(a.get("commit","") for a in json.load(sys.stdin)["apps"] if a["name"]=="begamin"))'; }
[[ "$(running_commit)" == "$commit" ]] || fail "the running revision doesn't record the pushed commit $commit"
echo "-- begamin runs commit $commit"
echo "-- the records:"; records | grep -E "^(api|go)\.lane" || fail "no records for begamin"
records | grep -q "^api.lane.begam.in $HOST quark: begamin on" || fail "api's record isn't quark's"

echo "== a push whose deploy fails is refused, and the running revision stays"
before=$(run 'quark ps' | awk '$1=="begamin" {print $3}')
sed -i.bak 's|  - url: https://api.lane.begam.in/healthz|  - url: https://api.lane.begam.in/not-a-page|' "$work/begamin/quark.yaml" && rm -f "$work/begamin/quark.yaml.bak"
g commit -q -am "lane: a check that fails"
if GIT_SSH_COMMAND="$push_ssh" g push quark HEAD:main >"$work/refused.log" 2>&1; then fail "a failing deploy was accepted"; fi
grep -E "fail |the push is refused|declined" "$work/refused.log" | head -4
after=$(run 'quark ps' | awk '$1=="begamin" {print $3}')
[[ "$before" == "$after" ]] || fail "the running revision changed from $before to $after"
echo "-- still running $after"
g reset -q --hard HEAD~1

echo "== the same key can't deploy another app, open a shell or run anything else"
as_key() { ssh -i "$push_key" -o IdentityAgent=none -o IdentitiesOnly=yes -o BatchMode=yes -o UserKnownHostsFile="$here/known_hosts" "quark@$HOST" "$@" 2>&1 </dev/null | tail -1 || true; }
refused=$(as_key "git-receive-pack 'site.git'"); echo "$refused"; [[ "$refused" == *"deploys begamin, not site"* ]] || fail "the key reached another app"
refused=$(as_key); echo "$refused"; [[ "$refused" == *"opens no shell"* ]] || fail "the key opened a shell"
refused=$(as_key "cat /etc/passwd"); echo "$refused"; [[ "$refused" == *"isn't something it runs"* ]] || fail "the key ran a command"

echo "== quark deploy --to sends this Mac's working tree, uncommitted change included"
echo "lane: sent without a commit" > "$work/begamin/LANE.md"
QUARK_SSH="$push_ssh" "$here/../bin/quark" deploy "$work/begamin" --to "quark@$HOST" 2>&1 | quiet | tail -3
rm -f "$work/begamin/LANE.md"

echo "== no app reaches another's network, nor the edge's admin API"
run 'bash -s' <<'PROBE'
set -u
apps="hello site begamin dragon-writer"
declare -A port=([hello]=8000 [site]=8080 [begamin]=8484 [dragon-writer]=3000 [postgres]=5432)
probe() { docker run --rm --network "container:$1" alpine:3.21 nc -z -w 2 "$2" "$3" >/dev/null 2>&1 && echo open || echo closed; }
serving() { docker ps --filter "label=quark.app=$1" --format '{{.Names}} {{.Label "quark.workload"}}' | awk '$2!="postgres" {print $1}' | grep -v -- '-job-' | head -1; }
containers() { docker ps --filter "label=quark.app=$1" --format '{{.Names}}' | grep -v -- '-job-'; }
ips() { docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}' "$1"; }
bad=0; tried=0
for a in $apps; do
  from=$(serving "$a")
  [ "$(probe "$from" 127.0.0.1 "${port[$a]}")" = open ] || { echo "control: $a can't reach itself"; bad=1; }
  edge_ok=closed
  for ip in $(ips quark-edge); do [ "$(probe "$from" "$ip" 443)" = open ] && edge_ok=open; done
  [ "$edge_ok" = open ] || { echo "control: $a can't reach the edge on 443"; bad=1; }
  for b in $apps; do
    [ "$a" = "$b" ] && continue
    for c in $(containers "$b"); do
      p=${port[$b]}; case "$c" in *-postgres) p=5432 ;; esac
      for target in "$c" $(ips "$c"); do
        tried=$((tried + 1))
        if [ "$(probe "$from" "$target" "$p")" = open ]; then echo "REACHED: $a -> $c at $target:$p"; bad=1; fi
      done
    done
  done
  for ip in $(ips quark-edge); do
    tried=$((tried + 1))
    if [ "$(probe "$from" "$ip" 2019)" = open ]; then echo "REACHED: $a -> the edge's admin at $ip:2019"; bad=1; fi
  done
done
dw=$(serving dragon-writer)
[ "$(probe "$dw" quark-dragon-writer-postgres 5432)" = open ] || { echo "control: dragon-writer can't reach its own database"; bad=1; }
echo "$tried attempts from every app at every other app's containers and the edge's admin API; $([ $bad = 0 ] && echo none got through || echo SOME GOT THROUGH)"
echo "each app reached itself, its own database and the edge's web ports"
exit $bad
PROBE
run 'quark exposure' | tail -8

echo "== begamin removed: its records go with it"
run 'quark remove begamin --yes' | quiet | grep -E "record|forgotten"
records | grep -E "^(api|go)\.lane" && fail "begamin's records survived its removal"
echo "-- no records for api or go.lane.begam.in; https://api.lane.begam.in/healthz answers $(fetch -o /dev/null -w '%{http_code}' https://api.lane.begam.in/healthz 2>/dev/null || true)"
run 'docker network ls --format "{{.Name}}"' | grep -q '^quark.edge.begamin$' && fail "begamin's edge network survived"

echo "== pushed again, begamin comes back with its records and its data"
g commit -q --allow-empty -m "lane: deploy again"
GIT_SSH_COMMAND="$push_ssh" g push quark HEAD:main 2>&1 | quiet | grep -E "made|succeeded|live"
records | grep -E "^(api|go)\.lane"
echo "-- from the outside: $(fetch -o /dev/null -w '%{http_code}' https://api.lane.begam.in/healthz) at https://api.lane.begam.in/healthz"
run "quark run begamin web -- sh -c 'ls /var/lib/begamin'" | quiet | grep -E '^ +\| ' | head -3

echo "== a name at another machine stops a deploy until it is pointed here"
run "sed -i 's|      - host: hello.lane.begam.in|      - host: hello.lane.begam.in\n      - host: moved.lane.begam.in\n        dns: direct|' /srv/lane/hello/quark.yaml"
run 'quark deploy /srv/lane/hello --yes' 2>&1 | quiet | grep -E "fail dns|points elsewhere" | head -2 || true
run 'quark ps' | grep -q "hello  *web" || fail "hello stopped running"
run 'quark dns point moved.lane.begam.in --yes' | quiet | grep -E "updated|points here"
run 'quark deploy /srv/lane/hello --yes' | quiet | grep -E "moved|succeeded|failed"
echo "-- from the outside: $(fetch https://moved.lane.begam.in/)"
echo "-- and when hello stops routing it, the record goes at retire:"
run "sed -i '/moved.lane.begam.in/,+1d' /srv/lane/hello/quark.yaml && quark deploy /srv/lane/hello --yes" | quiet | grep -E "record removed|succeeded|failed"
records | grep -q "^moved.lane.begam.in" && fail "moved.lane.begam.in's record survived hello dropping it"

echo "== quark dns, and its audit"
run 'quark dns'
run 'quark dns audit'
run 'quark dns audit --json' | python3 -c 'import json,sys; f=json.load(sys.stdin); assert any(x["host"]=="stale.lane.begam.in" for x in f), f; print("the audit found stale.lane.begam.in, which points here with nothing routed")'

echo "== a GitHub-style webhook deploys a push (the secret stays on the box)"
run 'rm -rf /srv/lane/upstream && mkdir -p /srv/lane/upstream && git init -q --bare --initial-branch=main /srv/lane/upstream/begamin.git'
GIT_SSH_COMMAND="ssh ${ssh_opts[*]}" g push -q "root@$HOST:/srv/lane/upstream/begamin.git" HEAD:main
run 'umask 077; quark git webhook begamin --repo file:///srv/lane/upstream/begamin.git 2>/root/.lane-webhook.out; awk "/Secret/ {print \$2}" /root/.lane-webhook.out > /root/.lane-webhook-secret; grep -v Secret /root/.lane-webhook.out; rm -f /root/.lane-webhook.out'
g commit -q --allow-empty -m "lane: deployed by a webhook"
GIT_SSH_COMMAND="ssh ${ssh_opts[*]}" g push -q "root@$HOST:/srv/lane/upstream/begamin.git" HEAD:main
hooked=$(g rev-parse HEAD)
run "python3 - <<'PY'
import hmac, hashlib, json, urllib.request
secret = open('/root/.lane-webhook-secret').read().strip()
body = json.dumps({'ref': 'refs/heads/main', 'after': '$hooked'}).encode()
def post(sig):
    req = urllib.request.Request('https://api.lane.begam.in/_quark/hook', data=body, method='POST', headers={'Content-Type': 'application/json', 'X-GitHub-Event': 'push', 'X-Hub-Signature-256': sig})
    try:
        with urllib.request.urlopen(req, timeout=20) as r:
            return r.status, r.read().decode().strip()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode().strip()
print('-- a wrong signature:', *post('sha256=' + '0' * 64))
print('-- the right one:', *post('sha256=' + hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()))
PY"
deadline=$((SECONDS + 300))
until run 'quark git webhooks --json' | python3 -c "import json,sys; h=json.load(sys.stdin)[0]; sys.exit(0 if h.get('last_commit')=='${hooked:0:12}' and h.get('last_result') else 1)"; do
  (( SECONDS < deadline )) || fail "the webhook's deploy didn't finish in 5 minutes"
  sleep 5
done
run 'quark git webhooks'
run 'quark git webhooks --json' | python3 -c "import json,sys; h=json.load(sys.stdin)[0]; assert h['last_result']=='deployed', h" || fail "the webhook's deploy failed"
[[ "$(running_commit)" == "${hooked:0:12}" ]] || fail "the webhook's commit isn't running"
echo "-- begamin runs commit ${hooked:0:12}, which only the webhook announced"
run 'quark git webhook begamin --off; rm -f /root/.lane-webhook-secret'

echo "== history"
run 'quark history --limit 14'
echo "M6 proven: git push deploys begamin and a failing push is refused; its records appear and disappear with it; no app reaches another's network or the edge's admin; a name at another machine waits for quark dns point; a webhook deploys a push; quark deploy --to sends a working tree"
