#!/bin/sh
# Builds the page for the daemon to embed, under BASE_PATH (default in web/vite.config.ts).
# The next go build of cmd/sidegated carries it.
set -eu
cd "$(dirname -- "$0")/.."
out=$PWD/cmd/sidegated/page
find "$out" -mindepth 1 ! -name .gitignore -delete
cd web
npx vite build --logLevel warn --outDir "$out" --emptyOutDir false
