#!/bin/sh
# Installs sidegate @VERSION@ on a UniFi Cloud Gateway Ultra, as `curl ... | sh`. scripts/release-files.sh
# fills in the version and the releases address. Everything is inside main, called on the last line, so a
# download cut short fails to parse or never calls it, and runs nothing. The braces around the definition
# and the call make every cut of either line an unfinished group, which sh refuses: a cut just after
# `main` would otherwise run whatever `main`, or `m`, is on PATH.

{ main() {
	set -eu
	version=@VERSION@
	from=@RELEASES@/download/$version
	bundle=sidegate-$version-arm64.tar.gz
	work=
	fail() {
		[ -z "$work" ] || rm -rf "$work"
		echo "sidegate: $*" >&2
		exit 1
	}
	get() { curl -fsSL --proto '=https' -o "$1" "$from/$1" || fail "the download of $1 failed"; }
	trap 'fail "stopped"' HUP INT TERM

	[ "$(id -u)" = 0 ] || fail "run it as root on the gateway"
	[ "$(uname -m)" = aarch64 ] || fail "this isn't an arm64 gateway"
	[ -d /data/unifi-core ] || fail "this isn't a UniFi OS console"

	echo "sidegate $version adds /data/sidegate, two systemd units and the sidegate page, and reloads nginx."
	work=$(mktemp -d /data/sidegate-install.XXXXXX) || fail "can't make a work directory in /data"
	cd "$work" || fail "can't enter $work"
	get "$bundle"
	get SHA256SUMS
	[ "$(grep -c '' SHA256SUMS)" = 1 ] && grep -qx "[0-9a-f]\{64\}  $bundle" SHA256SUMS ||
		fail "SHA256SUMS isn't the bundle's one line"
	sha256sum -c --status SHA256SUMS || fail "the bundle doesn't match its checksum"
	tar -xzf "$bundle" sidegated || fail "the bundle doesn't unpack"
	if ./sidegated install --bundle "$bundle" --yes >install.log 2>&1; then
		cd /
		rm -rf "$work"
		echo "Installed. Open https://<this gateway>/sidegate/ and paste a UniFi API key in Settings."
	else
		rm -f "$bundle" SHA256SUMS sidegated
		tail -n 5 install.log >&2
		echo "sidegate: the install stopped. The whole log: $work/install.log" >&2
		exit 1
	fi
}; }

# Stdin is the pipe the script arrives on, so nothing main runs may read it.
{ main </dev/null; }
