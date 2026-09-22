#!/bin/sh
# Put a published release on one or more machines.
#
# The bytes come from the GitHub release and are checked against the
# SHA256SUMS published beside them, once here and again on the machine after
# transfer. A machine is only ever given a binary whose checksum matched.
#
# Usage: install-release.sh VERSION HOST [HOST...]
set -eu

version=${1:?usage: install-release.sh VERSION HOST [HOST...]}
shift
[ $# -gt 0 ] || { echo 'At least one host is required' >&2; exit 1; }
case "$version" in *[!0-9.]*|'') echo 'A numeric release version is required' >&2; exit 1;; esac

base="https://github.com/kylebegeman/bedrock/releases/download/v$version"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

curl -fsSL "$base/SHA256SUMS" -o "$work/SHA256SUMS"

# Fetch one asset and verify it against the published sums. Downloading the
# sums from the same release proves only that the two agree; pin the sums in
# reviewed source when that matters.
fetch() {
  asset="bedrock_${version}_linux_$1"
  [ -f "$work/$asset" ] && return 0
  curl -fsSL "$base/$asset" -o "$work/$asset"
  want=$(grep " $asset\$" "$work/SHA256SUMS" | awk '{print $1}')
  [ -n "$want" ] || { echo "No published checksum for $asset" >&2; exit 1; }
  got=$(shasum -a 256 "$work/$asset" | awk '{print $1}')
  [ "$want" = "$got" ] || { echo "$asset does not match its published checksum" >&2; exit 1; }
}

for host in "$@"; do
  machine=$(ssh "$host" 'uname -m')
  case "$machine" in
    x86_64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) echo "$host: unsupported architecture $machine" >&2; exit 1 ;;
  esac
  fetch "$arch"
  asset="bedrock_${version}_linux_$arch"
  want=$(grep " $asset\$" "$work/SHA256SUMS" | awk '{print $1}')

  scp -q "$work/$asset" "$host:/tmp/$asset"
  # Rename rather than write in place: overwriting a running executable is
  # ETXTBSY, and rename is atomic, so no one ever sees a partial binary.
  ssh "$host" "set -eu
    got=\$(sha256sum '/tmp/$asset' | awk '{print \$1}')
    [ \"\$got\" = '$want' ] || { rm -f '/tmp/$asset'; echo 'transfer corrupted' >&2; exit 1; }
    chmod 0755 '/tmp/$asset'
    mv '/tmp/$asset' /usr/local/bin/bedrock
    systemctl restart bedrock.service"
  printf '%s: %s\n' "$host" "$(ssh "$host" 'bedrock version')"
done
