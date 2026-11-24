#!/usr/bin/env bash
# Runs in the VM. Installs MongoDB and Network Application 10.6.109 by digest.
# $1 is the subnet the VM must not reach, or empty.
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive

VERSION=10.6.109
DEB_URL=https://dl.ui.com/unifi/$VERSION/unifi_sysvinit_all.deb
DEB_SHA256=bbbd514b786c27bfc13c2e075e0ecd3b279dab12aa80077b4b351c3501662bf2

if [ -n "${1:-}" ] && ! iptables -C OUTPUT -d "$1" -j REJECT 2>/dev/null; then
  echo "FAIL: the guard that keeps this VM off $1 is missing; lab/net/down.sh, then run again" >&2
  exit 1
fi

if dpkg-query -W -f='${Version}' unifi 2>/dev/null | grep -q "^$VERSION-"; then
  echo "unifi $VERSION already installed"
else
  apt-get update -qq
  apt-get install -y -qq ca-certificates curl gnupg logrotate binutils libcap2 openjdk-25-jre-headless
  if [ ! -f /etc/apt/sources.list.d/mongodb-org-8.0.list ]; then
    curl -fsSL https://www.mongodb.org/static/pgp/server-8.0.asc | gpg --dearmor --yes -o /usr/share/keyrings/mongodb-8.0.gpg
    echo "deb [arch=arm64 signed-by=/usr/share/keyrings/mongodb-8.0.gpg] https://repo.mongodb.org/apt/ubuntu noble/mongodb-org/8.0 multiverse" \
      >/etc/apt/sources.list.d/mongodb-org-8.0.list
    apt-get update -qq
  fi
  apt-get install -y -qq mongodb-org-server
  curl -fsSL -o /tmp/unifi.deb "$DEB_URL"
  echo "$DEB_SHA256  /tmp/unifi.deb" | sha256sum -c -
  apt-get install -y -qq /tmp/unifi.deb
  rm -f /tmp/unifi.deb
fi

systemctl start unifi
for _ in $(seq 1 90); do
  if curl -fsSk -m 5 https://127.0.0.1:8443/status | grep -q '"up":true'; then
    curl -fsSk https://127.0.0.1:8443/status
    echo
    exit 0
  fi
  sleep 5
done
echo "FAIL: the Network Application did not come up" >&2
systemctl status unifi --no-pager >&2 || true
exit 1
