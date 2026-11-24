#!/bin/sh
# Builds the one tarball a gateway needs: the daemon with the page in it, and the pinned
# Xray by digest. Usage: scripts/build-bundle.sh [VERSION [RELEASES]]. Prints the tarball's path.
# VERSION is a tag such as v0.1.0, or dev-<git describe> by default. RELEASES is the releases
# page the build trusts for updates, such as https://github.com/MeafQ/sidegate/releases.
# On the gateway: tar -xzf BUNDLE sidegated && ./sidegated install --bundle BUNDLE
set -eu
cd "$(dirname -- "$0")/.."
. deploy/xray.pin
version=${1:-dev-$(git describe --always --dirty)}
releases=${2:-}
out=$PWD/dist
stage=$out/stage-$version
cache=$out/xray-$XRAY_VERSION
tarball=$out/sidegate-$version-arm64.tar.gz
mkdir -p "$out" "$cache"
rm -rf "$stage"
mkdir -p "$stage"

if command -v sha256sum >/dev/null; then
	sha() { sha256sum "$1" | cut -d' ' -f1; }
else
	sha() { shasum -a 256 "$1" | cut -d' ' -f1; }
fi

if [ ! -f "$cache/xray" ] || [ "$(sha "$cache/xray")" != "$XRAY_SHA256" ]; then
	curl -fsSL -o "$cache/xray.zip" "https://github.com/XTLS/Xray-core/releases/download/$XRAY_VERSION/Xray-linux-arm64-v8a.zip"
	[ "$(sha "$cache/xray.zip")" = "$XRAY_ZIP_SHA256" ] || { echo "the Xray download doesn't match its pin" >&2; exit 1; }
	unzip -o -q "$cache/xray.zip" xray -d "$cache"
	[ "$(sha "$cache/xray")" = "$XRAY_SHA256" ] || { echo "the unpacked Xray doesn't match its pin" >&2; exit 1; }
fi
cp "$cache/xray" "$stage/xray"
chmod 755 "$stage/xray"

[ -d web/node_modules ] || (cd web && npm ci --silent)
scripts/build-page.sh >&2
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=$version -X main.releases=$releases" -o "$stage/sidegated" ./cmd/sidegated

COPYFILE_DISABLE=1 tar --no-xattrs -czf "$tarball" -C "$stage" sidegated xray 2>/dev/null ||
	COPYFILE_DISABLE=1 tar -czf "$tarball" -C "$stage" sidegated xray
rm -rf "$stage"
echo "$(sha "$tarball")  $(basename "$tarball")" >&2
echo "$tarball"
