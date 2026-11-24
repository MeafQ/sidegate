#!/bin/sh
# Rerunnable gate for the daemon skeleton: formatting, vet, and the gateway build target.
set -eu
cd "$(dirname -- "$0")/.."

unformatted=$(gofmt -l .)
if [ -n "$unformatted" ]; then
	printf 'FAIL: gofmt reports unformatted files:\n%s\n' "$unformatted"
	exit 1
fi
printf 'PASS: gofmt -l .\n'

go vet ./...
printf 'PASS: go vet ./...\n'

CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./...
printf 'PASS: CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./...\n'

CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go vet -tags lab ./internal/runtime ./lab/runtimelab
printf 'PASS: the lab build of runtimelab vets\n'
