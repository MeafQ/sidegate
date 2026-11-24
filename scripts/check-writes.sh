#!/bin/sh
# Fails when a write crosses the page's one write path. Components press writes through the hooks of
# web/src/lib/use-write.ts and name them by `actions`; only use-write.ts runs the machine or sends an attempt, and
# only lib/api.ts sends a request: a fetch, an XMLHttpRequest or a beacon anywhere else could write around the path.
set -eu
cd "$(dirname -- "$0")/../web/src"

failed=0
for file in $(find . -name '*.ts' -o -name '*.tsx' | sed 's|^\./||' | grep -v '\.test\.ts$' | sort); do
	[ "$file" = lib/api.ts ] && continue
	if perl -0777 -ne 'exit(/(?<![\w.\$])(?:(?:window|globalThis|self)\.)?fetch\s*\(|\bXMLHttpRequest\b|\bsendBeacon\s*\(/ ? 0 : 1)' "$file"; then
		echo "$file: sends a request itself; only lib/api.ts does, and writes are pressed through use-write.ts" >&2
		failed=1
	fi
	[ "$file" = lib/use-write.ts ] && continue
	# Each import or re-export as "module|type|names": the module resolved from `@/` or from the file's own folder, with
	# no extension, in either quote, and a namespace or dynamic import's names as "*".
	imports=$(perl -0777 -e '
		my ($file) = @ARGV; local $/; open my $in, "<", $file or die; my $text = <$in>;
		(my $dir = $file) =~ s{[^/]*$}{};
		my @found;
		while ($text =~ /\b(?:import|export)\s+(type\s+)?((?:\w+\s*,\s*)?(?:\*(?:\s+as\s+\w+)?|\{[^}]*\}))\s*from\s*(["\x27])([^"\x27]+)\3/g) {
			my ($type, $what, $from) = ($1 // "", $2, $4);
			my $names = $what =~ /\*/ ? "*" : ($what =~ /\{([^}]*)\}/)[0];
			push @found, [$from, $type, $names];
		}
		while ($text =~ /\bimport\s*\(\s*(["\x27`])([^"\x27`]+)\1\s*\)/g) { push @found, [$2, "", "*"] }
		for my $f (@found) {
			my ($from, $type, $names) = @$f;
			$names =~ s/\s+/ /g;
			if ($from =~ s{^@/}{}) {} elsif ($from =~ m{^\.}) { $from = "$dir$from"; 1 while $from =~ s{(^|/)\./}{$1}; 1 while $from =~ s{[^/]+/\.\./}{} } else { next }
			$from =~ s/\.tsx?$//;
			print "$from|$type|$names\n";
		}' "$file")
	if printf '%s\n' "$imports" | grep -E '^lib/api\|\|' | grep -qE '(^|[ ,|])(attempt|session|\*)([ ,]|$)'; then
		echo "$file: sends attempts itself; press through use-write.ts" >&2
		failed=1
	fi
	if printf '%s\n' "$imports" | grep -qE '^lib/write\|\|'; then
		echo "$file: runs the write machine; only use-write.ts does, and others import its types" >&2
		failed=1
	fi
done
exit $failed
