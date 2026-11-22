#!/bin/sh
# Builds one release tarball per router CPU architecture in dist/:
#   selphy-relay-<arch>.tar.gz  containing  selphy-relay/{bin/selphy-relay-<arch>, install.sh, ...}
set -e
cd "$(dirname "$0")/.."
V=${1:-$(sed -n 's/^const version = "\(.*\)"/\1/p' main.go)}
rm -rf dist && mkdir -p dist
for spec in arm64:GOARCH=arm64 armv7:GOARCH=arm,GOARM=7 mipsle:GOARCH=mipsle,GOMIPS=softfloat \
            mips:GOARCH=mips,GOMIPS=softfloat amd64:GOARCH=amd64; do
	arch=${spec%%:*}
	OUT=dist/$arch/selphy-relay
	mkdir -p "$OUT/bin" "$OUT/src"
	env CGO_ENABLED=0 GOOS=linux $(echo "${spec#*:}" | tr ',' ' ') \
		go build -trimpath -ldflags="-s -w" -o "$OUT/bin/selphy-relay-$arch" .
	cp openwrt/install.sh openwrt/selphy-relay.init openwrt/selphy-relay.config README.md LICENSE "$OUT/"
	cp *.go go.mod "$OUT/src/"
	echo "$V" > "$OUT/VERSION"
	tar -C "dist/$arch" -czf "dist/selphy-relay-$arch.tar.gz" selphy-relay
done
(cd dist && sha256sum selphy-relay-*.tar.gz > SHA256SUMS)
ls -l dist/*.tar.gz dist/SHA256SUMS | awk '{printf "%-36s %6.1f MB\n", $9, $5/1048576}'
echo "version $V"
