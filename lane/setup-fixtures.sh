#!/usr/bin/env bash
# Independent local substitutes; no private apps or external credentials.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lane/common.sh
. "$here/common.sh"
for fixture in "$@"; do
  case "$fixture" in
    storage)
      lane_run 'sh -s' <<'STORAGE'
set -eu
[ -s /root/.lane-minio-key ] || { umask 077; head -c 18 /dev/urandom | base64 > /root/.lane-minio-key; }
if ! docker inspect lane-minio >/dev/null 2>&1; then
 # The pinned registry artifact is unavailable. Preserve this fixture release
 # using the official historical static binary, verified before building.
 release=RELEASE.2025-04-22T22-12-26Z
 case "$(uname -m)" in
  aarch64|arm64) arch=arm64; expected=6c2f3142c94240206123177f4ba1e360daa5d1e0a4962e90757ef4f92c3ab57c ;;
  x86_64) arch=amd64; expected=53e2a2cb16c5366ea6fbbc479c19ddb4c6a0948273e752f740fb1fbf27bb817c ;;
  *) echo "Unsupported MinIO fixture architecture." >&2; exit 1 ;;
 esac
 image="bedrock-lane-minio:$release-$arch"
 if ! docker image inspect "$image" >/dev/null 2>&1; then
  work=$(mktemp -d /tmp/bedrock-lane-minio.XXXXXX)
  trap 'rm -rf "$work"' EXIT
  curl --fail --location --retry 3 "https://github.com/minio/minio/releases/download/$release/minio.linux-$arch.$release" -o "$work/minio"
  printf "%s  %s\n" "$expected" "$work/minio" | sha256sum --check -
  chmod 755 "$work/minio"
  printf 'FROM scratch\nCOPY minio /minio\nENTRYPOINT ["/minio"]\nCMD ["server", "/data", "--address", "0.0.0.0:9000"]\n' >"$work/Dockerfile"
  docker build --network none --label "io.bedrock.lane.minio.binary-sha256=$expected" --tag "$image" "$work"
  rm -rf "$work"
  trap - EXIT
 fi
 test "$(docker image inspect --format '{{index .Config.Labels "io.bedrock.lane.minio.binary-sha256"}}' "$image")" = "$expected" || { echo "MinIO fixture image differs from its pinned artifact." >&2; exit 1; }
 docker run -d --name lane-minio -p 127.0.0.1:9000:9000 -e MINIO_ROOT_USER=lane -e MINIO_ROOT_PASSWORD="$(cat /root/.lane-minio-key)" -v lane-minio:/data "$image" server /data --address 0.0.0.0:9000 >/dev/null
fi
for attempt in $(seq 1 30); do curl -sf http://127.0.0.1:9000/minio/health/live >/dev/null && break; sleep 2; done
curl -sf http://127.0.0.1:9000/minio/health/live >/dev/null
# The recovery password is disposable fixture state; retain it only in the
# guest sealed store, without printing it into a caller transcript.
if ! integration_error=$(printf "kind=s3\nendpoint=http://127.0.0.1:9000\nkey_id=lane\nkey=%s\nbucket_prefix=lane\n" "$(cat /root/.lane-minio-key)" | bedrock integration set storage 2>&1 >/dev/null); then
 printf "%s\n" "$integration_error" | sed -E "s/[a-f0-9]{48}/[redacted fixture password]/g" >&2
 exit 1
fi
STORAGE
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
