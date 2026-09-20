#!/usr/bin/env bash
# M1 on the lane: install the daemon, run an operation through it, kill the
# daemon halfway, and show that systemd brings it back and the operation
# finishes with the interruption on its receipt.
#
# Run after lane/reset.sh: the box is fresh, the binary goes over by scp.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
. "$here/hostinger.env"
ssh_opts=(-i "$KEY" -o IdentityAgent=none -o IdentitiesOnly=yes -o BatchMode=yes
  -o UserKnownHostsFile="$here/known_hosts" -o StrictHostKeyChecking=yes -o ConnectTimeout=10)
box="root@$HOST"
run() { ssh "${ssh_opts[@]}" "$box" "$@"; }

echo "== build and copy"
(cd "$here/.." && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o bin/bedrock-linux-amd64 ./cmd/bedrock)
# A running daemon keeps the old binary busy, so copy beside it and rename
# over it: the running process keeps its inode, the next start gets the new one.
scp -q "${ssh_opts[@]}" "$here/../bin/bedrock-linux-amd64" "$box:/usr/local/bin/bedrock.new"
run 'chmod 755 /usr/local/bin/bedrock.new && mv -f /usr/local/bin/bedrock.new /usr/local/bin/bedrock && bedrock version'

echo "== install the daemon"
run 'bedrock daemon install && bedrock daemon status'

echo "== start a slow operation through the daemon, in the background on the box"
run 'rm -rf /var/lib/bedrock/exercise; nohup bedrock kernel exercise --dir /var/lib/bedrock/exercise --steps 6 --step-duration 3s --yes > /var/lib/bedrock/exercise.log 2>&1 &'
sleep 8

echo "== kill the daemon with SIGKILL while step 3 runs"
run 'pid=$(systemctl show -p MainPID --value bedrock); echo "daemon pid $pid"; kill -9 "$pid"; sleep 1; systemctl show -p ActiveState,SubState,NRestarts bedrock | tr "\n" " "; echo'

echo "== wait for the operation to finish under the restarted daemon"
for _ in $(seq 1 30); do
  status=$(run 'bedrock history --json --limit 1' | python3 -c 'import json,sys; rows=json.load(sys.stdin); print(rows[0]["status"] if rows else "none")')
  [[ "$status" == "succeeded" || "$status" == "failed" || "$status" == "compensated" ]] && break
  sleep 3
done
echo "operation ended: $status"

echo "== the receipt"
run 'bedrock history "$(bedrock history --json --limit 1 | python3 -c "import json,sys; print(json.load(sys.stdin)[0][\"id\"])")"'
echo "== what the client saw before the daemon died"
run 'tail -n 5 /var/lib/bedrock/exercise.log'
echo "== daemon log"
run 'journalctl -u bedrock --no-pager -n 12 -o cat'
echo "== markers"
run 'ls /var/lib/bedrock/exercise'

interruptions=$(run 'bedrock history --json --limit 1' | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["interruptions"])')
if [[ "$status" == "succeeded" && "$interruptions" == "1" ]]; then
  echo "M1 proven: the killed daemon's operation was resumed and finished (interrupted once)"
else
  echo "M1 NOT proven: status=$status interruptions=$interruptions" >&2
  exit 1
fi
