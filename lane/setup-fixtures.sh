#!/usr/bin/env bash
# Independent local substitutes; no private apps or external credentials.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lane/common.sh
. "$here/common.sh"
for fixture in "$@"; do
  case "$fixture" in
    storage)
      lane_run 'set -eu
[ -s /root/.lane-minio-key ] || { umask 077; head -c 18 /dev/urandom | base64 > /root/.lane-minio-key; }
if ! docker inspect lane-minio >/dev/null 2>&1; then
 docker run -d --name lane-minio -p 127.0.0.1:9000:9000 -e MINIO_ROOT_USER=lane -e MINIO_ROOT_PASSWORD="$(cat /root/.lane-minio-key)" -v lane-minio:/data quay.io/minio/minio:RELEASE.2025-04-22T22-12-26Z >/dev/null
fi
for attempt in $(seq 1 30); do curl -sf http://127.0.0.1:9000/minio/health/live >/dev/null && break; sleep 2; done
curl -sf http://127.0.0.1:9000/minio/health/live >/dev/null
printf "kind=s3\nendpoint=http://127.0.0.1:9000\nkey_id=lane\nkey=%s\nbucket_prefix=lane\n" "$(cat /root/.lane-minio-key)" | bedrock integration set storage >/dev/null'
      ;;
    email)
      lane_run 'set -eu
if ! docker inspect lane-mailpit >/dev/null 2>&1; then
 docker run -d --name lane-mailpit -p 127.0.0.1:1025:1025 -p 127.0.0.1:8025:8025 axllent/mailpit:v1.31.2@sha256:74d609a42ec279aa63c6b4622a6fa9b5408d1ad5b1d76a1c4be40a265ce0863d >/dev/null
fi
printf "smtp_host=127.0.0.1\nsmtp_port=1025\nfrom=bedrock@lane.invalid\nto=fixture@lane.invalid\n" | bedrock integration set email >/dev/null'
      ;;
    *) echo "Unknown fixture: $fixture. Choose storage or email." >&2; exit 2 ;;
  esac
  echo "Fixture ready: $fixture"
done
