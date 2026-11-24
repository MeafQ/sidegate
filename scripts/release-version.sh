#!/bin/sh
# Says whether TAG is a release's version, vX.Y.Z, and refuses it otherwise. Usage: scripts/release-version.sh TAG
# This is the release scripts' one rule, and parseReleaseVersion in internal/daemon/releases.go is the updater's.
# Both are tested against internal/daemon/testdata/release-tags.json, so whatever is published can be installed.
# Each number has no leading zeros, so one release has one tag, and at most nine digits, so it fits in 32 bits
# and sort, awk and Go all order it exactly.
set -eu
LC_ALL=C
export LC_ALL
[ $# = 1 ] || { echo "usage: scripts/release-version.sh TAG" >&2; exit 2; }
refuse() { echo "$1 isn't a release version: vX.Y.Z, each number with no leading zeros and at most nine digits" >&2; exit 1; }
# First, since grep reads a tag with a line break in it as two.
case $1 in *[!v.0-9]*) refuse "$1" ;; esac
n='(0|[1-9][0-9]{0,8})'
printf '%s\n' "$1" | grep -Eqx "v$n\.$n\.$n" || refuse "$1"
