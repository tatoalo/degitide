#!/bin/sh
set -eu

version=${1:?usage: build-release.sh vX.Y.Z owner}
owner=${2:?usage: build-release.sh vX.Y.Z owner}
python3 scripts/package-release.py "$version" "$owner" --validate
python3 scripts/build_changelog.py "$version"
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
    os=${target%/*}
    arch=${target#*/}
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -buildvcs=false \
        -ldflags="-s -w -X main.version=$version" \
        -o "dist/bin/${os}_${arch}/degitide" ./cmd/degitide
done
python3 scripts/package-release.py "$version" "$owner"
