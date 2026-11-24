#!/usr/bin/env bash
# Rebuilds the app from an empty database, runs every lab step through internal/unifi,
# rewrites the contract go test replays, and writes lab/results/net-<date>.md.
source "$(dirname "$0")/lib.sh"
ROOT=$(cd "$NET_DIR/../.." && pwd)
STATE="$ROOT/lab/.state/net"
mkdir -p "$STATE"
cd "$ROOT"

"$NET_DIR/up.sh"

if [ -n "$LAB_BLOCK_SUBNET" ]; then
  vm_root iptables -C OUTPUT -d "$LAB_BLOCK_SUBNET" -j REJECT || { echo "FAIL: the LAN guard is missing" >&2; exit 1; }
fi

GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o "$STATE/informsim" ./lab/net/informsim
go build -trimpath -o "$STATE/netlab" ./lab/net/netlab
COPYFILE_DISABLE=1 tar --no-xattrs -C "$STATE" -cf - informsim |
  vm_root sh -c "tar -C $GUEST_DIR -xf - && chmod +x $GUEST_DIR/informsim"

vm_root bash -c 'systemctl stop unifi; rm -rf /usr/lib/unifi/data/* /usr/lib/unifi/logs/*; systemctl start unifi'
rm -f "$STATE/admin"
for _ in $(seq 1 90); do
  curl -fsSk -m 5 "$BASE_URL/status" 2>/dev/null | grep -q '"up":true' && break
  sleep 5
done
# The first answers can come before the setup API is ready.
sleep 15

version=$(curl -fsSk -m 5 "$BASE_URL/status" | python3 -c 'import json,sys; print(json.load(sys.stdin)["meta"]["server_version"])')
result="$ROOT/lab/results/net-$(date +%F).md"
{
  echo "# Network Application lab, $(date '+%F %T %Z')"
  echo
  echo "Produced by \`lab/net/verify.sh\` at $(git rev-parse --short HEAD) on Lima $(limactl --version | awk '{print $3}'), VM \`$VM\`."
  echo "Network Application $version from an empty database, no real gateway."
  echo
  echo '```'
} >"$result"
set +e
"$STATE/netlab" -base "$BASE_URL" -vm "$VM" -state "$STATE" 2>&1 | tee -a "$result"
status=${PIPESTATUS[0]}
set -e
echo '```' >>"$result"
echo "wrote $result"
exit "$status"
