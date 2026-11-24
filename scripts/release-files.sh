#!/bin/sh
# Writes a release's files from its bundle, and checks each before the release workflow uploads
# them. Usage: scripts/release-files.sh VERSION RELEASES BUNDLE OUT
# VERSION is the tag, RELEASES the releases page the bundle was built to trust, and BUNDLE what
# scripts/build-bundle.sh printed. OUT must not exist yet. It gets NOTES.md and files/: the bundle,
# sidegate.json, SHA256SUMS, install.sh and XRAY-LICENSE. The bundle's own sidegated must say
# VERSION and RELEASES, so this runs where that binary runs: on linux/arm64.
set -eu
[ $# = 4 ] || { echo "usage: scripts/release-files.sh VERSION RELEASES BUNDLE OUT" >&2; exit 2; }
version=$1 releases=$2 bundle=$3 out=$4
root=$(cd "$(dirname -- "$0")/.." && pwd)
. "$root/deploy/xray.pin"
fail() { echo "release-files: $*" >&2; exit 1; }
call="{ main </dev/null; }"

sh "$root/scripts/release-version.sh" "$version" || exit 1
echo "$releases" | grep -Eqx 'https://[A-Za-z0-9.:_-]+(/[A-Za-z0-9._-]+)+' || fail "$releases isn't an https releases address"
name=sidegate-$version-arm64.tar.gz
[ "$(basename -- "$bundle")" = "$name" ] || fail "the bundle for $version is named $name, not $(basename -- "$bundle")"
[ -s "$bundle" ] || fail "$bundle is missing or empty"
[ ! -e "$out" ] || fail "$out already exists"

if command -v sha256sum >/dev/null; then
	sha() { sha256sum "$1" | cut -d' ' -f1; }
	check() { sha256sum -c --status "$1"; }
else
	sha() { shasum -a 256 "$1" | cut -d' ' -f1; }
	check() { shasum -a 256 -c --status "$1"; }
fi

unpacked=$(mktemp -d)
trap 'rm -rf "$unpacked"' EXIT
tar -xzf "$bundle" -C "$unpacked" sidegated xray || fail "the bundle lacks sidegated or xray"
[ "$(sha "$unpacked/xray")" = "$XRAY_SHA256" ] || fail "the bundle's xray isn't Xray $XRAY_VERSION as deploy/xray.pin has it"
said=$("$unpacked/sidegated" version) || fail "the bundle's sidegated doesn't run here; run this on linux/arm64"
[ "$said" = "$version
$releases" ] || fail "the bundle's sidegated says $(echo "$said" | tr '\n' ' '), not $version $releases"

files=$out/files
mkdir -p "$files"
cp "$bundle" "$files/$name"
sed -e "s|@VERSION@|$version|g" -e "s|@RELEASES@|$releases|g" "$root/deploy/install.sh" >"$files/install.sh"
[ -s "$files/install.sh" ] || fail "install.sh is empty"
! grep -q '@[A-Z]*@' "$files/install.sh" || fail "install.sh has a blank left: $(grep -o '@[A-Z]*@' "$files/install.sh" | head -n 1)"
sh -n "$files/install.sh" || fail "install.sh doesn't parse"
[ "$(tail -n 1 "$files/install.sh")" = "$call" ] || fail "install.sh doesn't end with $call, so a cut download could run part of it"
cp "$root/deploy/XRAY-LICENSE" "$files/XRAY-LICENSE"
[ -s "$files/XRAY-LICENSE" ] || fail "deploy/XRAY-LICENSE is empty"
(cd "$files" && echo "$(sha "$name")  $name" >SHA256SUMS && [ "$(grep -c '' SHA256SUMS)" = 1 ] && check SHA256SUMS) ||
	fail "SHA256SUMS isn't the bundle's one line"
printf '{"version":"%s","bundle":"%s","size":%s,"sha256":"%s","core":"%s"}\n' \
	"$version" "$name" "$(wc -c <"$bundle" | tr -d ' ')" "$(sha "$bundle")" "$XRAY_SHA256" >"$files/sidegate.json"

cat >"$out/NOTES.md" <<EOF
Install it: SSH into the gateway as root and run

\`\`\`sh
curl --proto '=https' --tlsv1.2 -fsSL $releases/latest/download/install.sh | sh
\`\`\`

Update an installed sidegate in Settings, About.

The bundle carries [Xray $XRAY_VERSION](https://github.com/XTLS/Xray-core/tree/$XRAY_VERSION), under the Mozilla Public License 2.0 in XRAY-LICENSE.
EOF

for f in "$name" sidegate.json SHA256SUMS install.sh XRAY-LICENSE; do
	[ -s "$files/$f" ] || fail "$f is missing"
done
echo "$out"
