#!/usr/bin/env bash
# Runs as root inside the VM. Installs the gateway's tool set and the Xray deploy/xray.pin pins.
set -euo pipefail
source "$(dirname "$0")/xray.pin"

PACKAGES="wireguard-tools iptables iproute2 tcpdump iperf3 curl conntrack sysstat unzip openssl jq"

missing=""
for p in $PACKAGES; do
  [ "$(dpkg-query -W -f='${db:Status-Status}' "$p" 2>/dev/null)" = installed ] || missing="$missing $p"
done
if [ -n "$missing" ]; then
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -q
  apt-get install -y -q --no-install-recommends $PACKAGES
fi

update-alternatives --set iptables /usr/sbin/iptables-legacy >/dev/null
update-alternatives --set ip6tables /usr/sbin/ip6tables-legacy >/dev/null

if [ "$(/usr/local/bin/xray version 2>/dev/null | awk 'NR==1{print "v"$2}')" != "$XRAY_VERSION" ]; then
  tmp=$(mktemp -d)
  curl -fsSL -o "$tmp/xray.zip" "https://github.com/XTLS/Xray-core/releases/download/$XRAY_VERSION/Xray-linux-arm64-v8a.zip"
  echo "$XRAY_ZIP_SHA256  $tmp/xray.zip" | sha256sum -c --quiet
  unzip -q -o "$tmp/xray.zip" xray -d "$tmp"
  install -m 0755 "$tmp/xray" /usr/local/bin/xray
  rm -rf "$tmp"
fi

echo "iptables: $(iptables --version)"
echo "xray: $(/usr/local/bin/xray version | head -1)"
