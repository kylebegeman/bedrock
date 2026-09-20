#!/usr/bin/env bash
# M2 on the lane: set the box up, pass the doctor, reconcile as a no-op,
# maintain with a reboot, survive a broken upgrade (systemd rolls it back),
# then take a good one.
#
# Runs against the box as prove-m1 left it (daemon installed) or fresh.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
. "$here/hostinger.env"
ssh_opts=(-i "$KEY" -o IdentityAgent=none -o IdentitiesOnly=yes -o BatchMode=yes
  -o UserKnownHostsFile="$here/known_hosts" -o StrictHostKeyChecking=yes -o ConnectTimeout=10)
box="root@$HOST"
run() { ssh "${ssh_opts[@]}" "$box" "$@"; }
put() { scp -q "${ssh_opts[@]}" "$1" "$box:$2"; }
build() { # build <output> <version>
  (cd "$here/.." && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-X github.com/kylebegeman/bedrock/internal/version.number=$2" -o "$1" ./cmd/bedrock)
}
wait_ssh() { until run true 2>/dev/null; do sleep 5; done; }
last_op() { run 'bedrock history --json --limit 1' | python3 -c "import json,sys; rows=json.load(sys.stdin); print(rows[0]['$1'] if rows else 'none')"; }
wait_op() { # wait_op <seconds>
  for _ in $(seq 1 "$(( $1 / 5 ))"); do
    s=$(last_op status 2>/dev/null || echo waiting)
    case "$s" in succeeded|failed|compensated) echo "$s"; return 0 ;; esac
    sleep 5
  done
  echo "still $s"; return 1
}

echo "== build and install"
build "$here/../bin/bedrock-linux-amd64" "0.7.0-dev"
put "$here/../bin/bedrock-linux-amd64" /usr/local/bin/bedrock.new
run 'chmod 755 /usr/local/bin/bedrock.new && mv -f /usr/local/bin/bedrock.new /usr/local/bin/bedrock && bedrock daemon install'

echo "== doctor before setup (failures expected)"
run 'bedrock doctor' || true

echo "== host setup"
run 'bedrock host setup --hostname bedrock-lane --swap 4 --yes'

echo "== doctor after setup (must pass)"
run 'bedrock doctor'

echo "== reconcile plan (everything already fine)"
run 'bedrock host reconcile --plan'

echo "== maintain with reboot"
run 'nohup bedrock host maintain --reboot --yes > /var/lib/bedrock/maintain.log 2>&1 &'
sleep 20
echo "waiting for the box to come back"
wait_ssh
echo "back; uptime: $(run 'uptime -p')"
echo "maintain ended: $(wait_op 180)"
run 'bedrock history "$(bedrock history --json --limit 1 | python3 -c "import json,sys; print(json.load(sys.stdin)[0][\"id\"])")"'

echo "== broken upgrade (must roll back)"
build "$here/../bin/bedrock-broken" "0.7.0-broken"
put "$here/../bin/bedrock-broken" /tmp/bedrock-broken
run 'nohup bedrock upgrade /tmp/bedrock-broken --yes > /var/lib/bedrock/upgrade-broken.log 2>&1 &'
sleep 45
echo "upgrade ended: $(wait_op 120)"
run 'bedrock version; bedrock daemon status; tail -n 3 /var/lib/bedrock/upgrade-broken.log; journalctl -u bedrock-rollback --no-pager -n 3 -o cat'
if ! run 'bedrock daemon status' | grep -q 'daemon 0.7.0-dev answering'; then echo "M2 NOT proven: the daemon isn't back on the previous build" >&2; exit 1; fi
if run 'bedrock version' | grep -q broken; then echo "M2 NOT proven: the broken binary is still installed" >&2; exit 1; fi

echo "== good upgrade (must succeed)"
build "$here/../bin/bedrock-lane" "0.7.0-lane"
put "$here/../bin/bedrock-lane" /tmp/bedrock-lane
run 'nohup bedrock upgrade /tmp/bedrock-lane --yes > /var/lib/bedrock/upgrade-good.log 2>&1 &'
sleep 15
good=$(wait_op 120); echo "upgrade ended: $good"
run 'bedrock version; bedrock daemon status'
if [[ "$good" == "succeeded" ]] && run 'bedrock daemon status' | grep -q 'daemon 0.7.0-lane answering'; then
  echo "M2 proven: setup, doctor, reconcile, maintain with reboot, a rolled-back upgrade and a good one"
else
  echo "M2 NOT proven: the good build isn't running" >&2; exit 1
fi
