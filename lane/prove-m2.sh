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
  (cd "$here/.." && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-X github.com/kylebegeman/quark/internal/version.number=$2" -o "$1" ./cmd/quark)
}
wait_ssh() { until run true 2>/dev/null; do sleep 5; done; }
last_op() { run 'quark history --json --limit 1' | python3 -c "import json,sys; rows=json.load(sys.stdin); print(rows[0]['$1'] if rows else 'none')"; }
wait_op() { # wait_op <seconds>
  for _ in $(seq 1 "$(( $1 / 5 ))"); do
    s=$(last_op status 2>/dev/null || echo waiting)
    case "$s" in succeeded|failed|compensated) echo "$s"; return 0 ;; esac
    sleep 5
  done
  echo "still $s"; return 1
}

echo "== build and install"
build "$here/../bin/quark-linux-amd64" "0.7.0-dev"
put "$here/../bin/quark-linux-amd64" /usr/local/bin/quark.new
run 'chmod 755 /usr/local/bin/quark.new && mv -f /usr/local/bin/quark.new /usr/local/bin/quark && quark daemon install'

echo "== doctor before setup (failures expected)"
run 'quark doctor' || true

echo "== host setup"
run 'quark host setup --hostname quark-lane --swap 4 --yes'

echo "== doctor after setup (must pass)"
run 'quark doctor'

echo "== reconcile plan (everything already fine)"
run 'quark host reconcile --plan'

echo "== maintain with reboot"
run 'nohup quark host maintain --reboot --yes > /var/lib/quark/maintain.log 2>&1 &'
sleep 20
echo "waiting for the box to come back"
wait_ssh
echo "back; uptime: $(run 'uptime -p')"
echo "maintain ended: $(wait_op 180)"
run 'quark history "$(quark history --json --limit 1 | python3 -c "import json,sys; print(json.load(sys.stdin)[0][\"id\"])")"'

echo "== broken upgrade (must roll back)"
build "$here/../bin/quark-broken" "0.7.0-broken"
put "$here/../bin/quark-broken" /tmp/quark-broken
run 'nohup quark upgrade /tmp/quark-broken --yes > /var/lib/quark/upgrade-broken.log 2>&1 &'
sleep 45
echo "upgrade ended: $(wait_op 120)"
run 'quark version; quark daemon status; tail -n 3 /var/lib/quark/upgrade-broken.log; journalctl -u quark-rollback --no-pager -n 3 -o cat'
if ! run 'quark daemon status' | grep -q 'daemon 0.7.0-dev answering'; then echo "M2 NOT proven: the daemon isn't back on the previous build" >&2; exit 1; fi
if run 'quark version' | grep -q broken; then echo "M2 NOT proven: the broken binary is still installed" >&2; exit 1; fi

echo "== good upgrade (must succeed)"
build "$here/../bin/quark-lane" "0.7.0-lane"
put "$here/../bin/quark-lane" /tmp/quark-lane
run 'nohup quark upgrade /tmp/quark-lane --yes > /var/lib/quark/upgrade-good.log 2>&1 &'
sleep 15
good=$(wait_op 120); echo "upgrade ended: $good"
run 'quark version; quark daemon status'
if [[ "$good" == "succeeded" ]] && run 'quark daemon status' | grep -q 'daemon 0.7.0-lane answering'; then
  echo "M2 proven: setup, doctor, reconcile, maintain with reboot, a rolled-back upgrade and a good one"
else
  echo "M2 NOT proven: the good build isn't running" >&2; exit 1
fi
