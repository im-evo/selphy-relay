#!/bin/sh
# Builds static binaries for common router CPUs and the release tarball in dist/.
set -e
cd "$(dirname "$0")/.."
V=${1:-$(sed -n 's/^const version = "\(.*\)"/\1/p' main.go)}
OUT=dist/selphy-relay
rm -rf dist && mkdir -p "$OUT/bin" "$OUT/src"
build() { CGO_ENABLED=0 GOOS=linux "$@" go build -trimpath -ldflags="-s -w" -o "$OUT/bin/selphy-relay-$T" .; }
T=arm64  build env GOARCH=arm64
T=armv7  build env GOARCH=arm GOARM=7
T=mipsle build env GOARCH=mipsle GOMIPS=softfloat
T=mips   build env GOARCH=mips GOMIPS=softfloat
T=amd64  build env GOARCH=amd64
(cd "$OUT/bin" && sha256sum selphy-relay-* > SHA256SUMS)
cp openwrt/install.sh openwrt/selphy-relay.init openwrt/selphy-relay.config README.md LICENSE "$OUT/"
cp *.go go.mod "$OUT/src/"
echo "$V" > "$OUT/VERSION"
tar -C dist -czf "dist/selphy-relay.tar.gz" selphy-relay
echo "dist/selphy-relay.tar.gz ($V)"
