#!/usr/bin/env bash
# Runs as root inside the VM. Builds, once, the iproute2 versions step named reads
# under: the gateway's 5.9 and a newer 6.1. The VM's own ip is 5.5.
set -euo pipefail

build() {
  local version=$1 digest=$2 dir=/opt/iproute2/$1 tmp
  [ -x "$dir/ip" ] && return
  if ! dpkg-query -W -f='${db:Status-Status}' build-essential 2>/dev/null | grep -qx installed; then
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -q
    apt-get install -y -q --no-install-recommends build-essential bison flex pkg-config libmnl-dev libelf-dev
  fi
  tmp=$(mktemp -d)
  curl -fsSL -o "$tmp/src.tar.xz" "https://www.kernel.org/pub/linux/utils/net/iproute2/iproute2-$version.tar.xz"
  echo "$digest  $tmp/src.tar.xz" | sha256sum -c --quiet
  tar -C "$tmp" -xf "$tmp/src.tar.xz"
  (cd "$tmp/iproute2-$version" && ./configure >/dev/null && make -j4 SUBDIRS="lib ip" >/dev/null)
  install -D -m 0755 "$tmp/iproute2-$version/ip/ip" "$dir/ip"
  rm -rf "$tmp"
}

build 5.9.0 a25dac94bcdcf2f73316c7f812115ea7a5710580bad892b08a83d00c6b33dacf
build 6.1.0 5ce12a0fec6b212725ef218735941b2dab76244db7e72646a76021b0537b43ab
