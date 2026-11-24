#!/bin/sh
# Says whether TAG's release becomes Latest: true when TAG is above every published release, false
# when one above it is published. Usage: scripts/release-latest.sh TAG, with gh's GH_TOKEN and GH_REPO.
# It refuses TAG unless release-version.sh takes it, and refuses it when a published release
# has its three numbers, TAG itself included, so a tag is released once. Drafts and prereleases
# don't count, so a higher tag whose run failed holds nothing back, and an older tag that finishes
# last doesn't take Latest from a newer one.
set -eu
[ $# = 1 ] || { echo "usage: scripts/release-latest.sh TAG" >&2; exit 2; }
tag=$1
limit=10000
fail() { echo "release-latest: $*" >&2; exit 1; }
sh "$(dirname -- "$0")/release-version.sh" "$tag" || exit 1

# The first line is how many releases gh read, so an empty answer is never a list. gh stops at
# limit, so fewer than limit is every release there is.
answer=$(gh release list --limit "$limit" --json tagName,isDraft,isPrerelease \
	-q 'length, (.[] | select((.isDraft or .isPrerelease) | not) | .tagName)') ||
	fail "can't read the published releases, so can't say whether $tag is Latest"
count=$(echo "$answer" | head -n 1)
echo "$count" | grep -Eqx '[0-9]+' || fail "gh's list of releases has no count: $count"
[ "$count" -lt "$limit" ] || fail "there are $limit releases or more, past what this reads; raise limit"

# Each published vX.Y.Z as its numbers without leading zeros, then the tag as published.
published=$(echo "$answer" | tail -n +2 | grep -Ex 'v[0-9]+\.[0-9]+\.[0-9]+' |
	sed -E 'h; s/^v//; s/(^|\.)0+([0-9])/\1\2/g; G; s/\n/ /')
same=$(echo "$published" | awk -v t="${tag#v}" '$1 == t { print $2; exit }')
[ "$same" != "$tag" ] || fail "$tag is already published; push a new tag for a new release"
[ -z "$same" ] || fail "$tag has the numbers of the published $same; push a new tag for a new release"
highest=$(echo "$published" | cut -d' ' -f1 | sort -t. -k1,1n -k2,2n -k3,3n | tail -n 1)
newest=$(printf '%s\n%s\n' "$highest" "${tag#v}" | sort -t. -k1,1n -k2,2n -k3,3n | tail -n 1)
if [ -z "$highest" ] || [ "$newest" = "${tag#v}" ]; then echo true; else echo false; fi
