#!/bin/sh
# The quick check: `scripts/quick.sh` runs both halves, `scripts/quick.sh go` or `page` one of them. It stops at the
# first failure. E2E_PORT moves the page checks' ports. The slow checks are listed in lab/README.md.
set -eu
cd "$(dirname -- "$0")/.."
half=${1:-both}
case $half in both | go | page) ;; *) echo "usage: scripts/quick.sh [go|page]" >&2; exit 2 ;; esac

if [ "$half" != page ]; then
	start=$(date +%s)
	scripts/check-skeleton.sh
	scripts/check-writes.sh
	go test -short -count=1 ./...
	go test -short -count=1 -tags lab -run '^TestTheLab' ./internal/runtime
	(cd lab/labweb && go vet ./... && go test -short -count=1 ./...)
	printf 'PASS: the Go half in %s s\n' $(($(date +%s) - start))
fi

if [ "$half" != go ]; then
	start=$(date +%s)
	(
		cd web
		npm run -s typecheck
		npm run -s lint
		npm test
		node tests/e2e/fuzz.mjs --smoke
		npm run -s test:contract
		npm run -s test:e2e
	)
	printf 'PASS: the page half in %s s\n' $(($(date +%s) - start))
fi
