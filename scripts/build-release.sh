#!/bin/sh
# Build reviewed, clean source. Upload these exact bytes and pin SHA256SUMS.
set -eu
version=${1:?usage: build-release.sh VERSION OUTPUT_DIRECTORY}
out=${2:?usage: build-release.sh VERSION OUTPUT_DIRECTORY}
# The same rule bedrock upgrade --version applies.
printf '%s' "$version" | grep -Eq '^[0-9]+(\.[0-9]+)*$' || { echo "A release version such as 0.7.8 is required, not '$version'" >&2; exit 1; }
if [ -n "$(git status --porcelain)" ]; then
  echo 'Commit the reviewed changes before building a release.' >&2
  exit 1
fi
mkdir -p "$out"
out=$(cd "$out" && pwd)
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  os=${target%/*}
  arch=${target#*/}
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -buildvcs=true \
    -ldflags "-s -w -X github.com/kylebegeman/bedrock/internal/version.number=$version" \
    -o "$out/bedrock_${version}_${os}_${arch}" ./cmd/bedrock
done
# Whichever the machine has; the two print the same first field.
if command -v sha256sum >/dev/null 2>&1; then
  (cd "$out" && sha256sum bedrock_* > SHA256SUMS)
else
  (cd "$out" && shasum -a 256 bedrock_* > SHA256SUMS)
fi
printf 'Built %s at %s into %s\n' "$version" "$(git rev-parse HEAD)" "$out"
