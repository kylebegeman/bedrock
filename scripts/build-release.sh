#!/bin/sh
# Build reviewed, clean source. Upload these exact bytes and pin SHA256SUMS.
set -eu
version=${1:?usage: build-release.sh VERSION OUTPUT_DIRECTORY}
out=${2:?usage: build-release.sh VERSION OUTPUT_DIRECTORY}
case "$version" in *[!0-9.]*|'') echo 'A numeric release version is required' >&2; exit 1;; esac
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
(cd "$out" && shasum -a 256 bedrock_* > SHA256SUMS)
printf 'Built %s at %s into %s\n' "$version" "$(git rev-parse HEAD)" "$out"
