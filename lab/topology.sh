#!/usr/bin/env bash
# Runs as root inside the VM. Tears down and rebuilds the whole lab, so every
# run converges to the same state. With --no-slots it leaves out the slot
# plumbing (pxaN, NOTRACK, TPROXY, guard, pref 99, table 100, gateway Xray), so
# the daemon's runtime can build it: see runtime.sh.
#
# Root namespace = the gateway (UCG). Namespaces:
#   lan    client 192.168.1.10 behind br0
#   inet   the internet router: gateway WAN, servers and the target hang off it
#   srvA   VLESS server 198.51.100.10     srvB  VLESS server 198.51.100.20
#   wgsrv  plain WireGuard server 198.51.100.30 (baseline for a normal VPN Client)
#   web    labweb + iperf3 on 203.0.113.80 (direct), .81 (wgclt1), .82 (wgclt2), .83 (wgclt3)
set -euo pipefail
source "$(dirname "$0")/names.sh"

STATE=/var/lib/unifi-lab
CONF=/etc/unifi-lab
GUEST_DIR=/opt/unifi-lab
UUID=6f1c1e52-0d3c-4b8e-9a51-2c1f7c0a5e11
NAMESPACES="lan inet srvA srvB wgsrv web"
SLOTS="1 2"
[ "${1:-}" = --no-slots ] && SLOTS=""

declare -A SERVER_IP=([srvA]=198.51.100.10 [srvB]=198.51.100.20)
declare -A SLOT_SERVER=([1]=srvA [2]=srvB)
# wgcltN: VPN subnet address, UniFi route-table, traffic-route mark, target routed to it.
declare -A CLT_ADDR=([1]=10.70.1.2 [2]=10.70.2.2 [3]=10.99.0.2)
declare -A CLT_TABLE=([1]=181 [2]=182 [3]=183)
declare -A CLT_MARK=([1]=0x6a0000 [2]=0x6c0000 [3]=0x6e0000)
declare -A CLT_TARGET=([1]=203.0.113.81 [2]=203.0.113.82 [3]=203.0.113.83)
declare -A CLT_PEER=([1]=pxa1 [2]=pxa2 [3]=wgsrv)
declare -A CLT_ENDPOINT=([1]=127.0.0.1:51831 [2]=127.0.0.1:51832 [3]=198.51.100.30:51820)

teardown() {
  systemctl stop 'lab-*' "$CORE_UNIT" 2>/dev/null || true
  systemctl reset-failed 'lab-*' "$CORE_UNIT" 2>/dev/null || true
  for ns in $NAMESPACES; do
    { ip netns pids "$ns" 2>/dev/null || true; } | xargs -r kill -9
    ip netns del "$ns" 2>/dev/null || true
  done
  for l in br0 lan-br eth4 wgclt1 wgclt2 wgclt3 pxa1 pxa2; do
    ip link del "$l" 2>/dev/null || true
  done
  # The VM reaches Lima only through main, so its catch-all goes in before UniFi's 32766 replaces the stock rule.
  ip rule show pref 40000 | grep -q . || ip rule add pref 40000 lookup main
  for pref in 99 100 5270 32000 32500 32502 32504 32506 32508 32765 32766; do
    while ip rule del pref "$pref" 2>/dev/null; do :; done
  done
  for t in 52 100 181 182 183 201; do ip route flush table "$t" 2>/dev/null || true; done
}

keypair() {
  local name=$1
  [ -s "$STATE/$name.key" ] || { umask 077; wg genkey >"$STATE/$name.key"; }
  wg pubkey <"$STATE/$name.key" >"$STATE/$name.pub"
}

certs() {
  for s in srvA srvB; do
    [ -s "$STATE/$s.crt" ] && continue
    openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 3650 \
      -subj "/CN=$s.lab" -addext "subjectAltName=DNS:$s.lab" \
      -keyout "$STATE/$s.pem" -out "$STATE/$s.crt" 2>/dev/null
  done
  # The daemon fetches sources over HTTPS only, and trusts the system's roots, so the stand-in
  # feeds' certificate is one of them.
  [ -s "$STATE/labweb.crt" ] || openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 3650 \
    -subj "/CN=unifi-lab feeds" -addext "subjectAltName=IP:11.0.0.80" \
    -keyout "$STATE/labweb.pem" -out "$STATE/labweb.crt" 2>/dev/null
  if ! cmp -s "$STATE/labweb.crt" /usr/local/share/ca-certificates/unifi-lab-feeds.crt; then
    cp "$STATE/labweb.crt" /usr/local/share/ca-certificates/unifi-lab-feeds.crt
    update-ca-certificates >/dev/null
  fi
}

pin() { openssl x509 -noout -fingerprint -sha256 -in "$STATE/$1.crt" | cut -d= -f2; }

veth_into() {
  local ns=$1 here=$2 there=$3
  ip link add "$here" type veth peer name "$there" netns "$ns"
}

ns_up() {
  local ns=$1
  ip netns add "$ns"
  ip -n "$ns" link set lo up
}

run_unit() {
  local unit=$1 ns=$2; shift 2
  local opts=(--unit="$unit" --collect --quiet -p Restart=on-failure)
  [ "$ns" = root ] || opts+=(-p NetworkNamespacePath=/run/netns/"$ns")
  systemd-run "${opts[@]}" "$@"
}

build_world() {
  for ns in $NAMESPACES; do ns_up "$ns"; done

  ip -n inet link add net type bridge
  ip -n inet addr add 198.51.100.1/24 dev net
  ip -n inet addr add 203.0.113.1/24 dev net
  ip -n inet link set net up
  ip netns exec inet sysctl -qw net.ipv4.ip_forward=1
  for ns in srvA srvB wgsrv web; do
    ip -n inet link add "to-$ns" type veth peer name eth0 netns "$ns"
    ip -n inet link set "to-$ns" master net up
    ip -n "$ns" link set eth0 up
  done
  ip -n srvA addr add 198.51.100.10/24 dev eth0
  ip -n srvB addr add 198.51.100.20/24 dev eth0
  ip -n wgsrv addr add 198.51.100.30/24 dev eth0
  for h in srvA srvB wgsrv; do ip -n "$h" route add default via 198.51.100.1; done
  for a in 80 81 82 83; do ip -n web addr add "203.0.113.$a/24" dev eth0; done
  ip -n web route add default via 203.0.113.1

  veth_into inet eth4 isp0
  ip -n inet addr add 192.168.100.1/24 dev isp0
  ip -n inet link set isp0 up

  ip netns exec wgsrv sysctl -qw net.ipv4.ip_forward=1
  ip -n wgsrv link add wg0 type wireguard
  ip netns exec wgsrv wg set wg0 listen-port 51820 private-key "$STATE/wgsrv.key" \
    peer "$(cat "$STATE/wgclt3.pub")" allowed-ips 10.99.0.2/32
  ip -n wgsrv addr add 10.99.0.1/24 dev wg0
  ip -n wgsrv link set wg0 up
  ip netns exec wgsrv iptables-restore <<'EOF'
*nat
:PREROUTING ACCEPT [0:0]
:INPUT ACCEPT [0:0]
:OUTPUT ACCEPT [0:0]
:POSTROUTING ACCEPT [0:0]
-A POSTROUTING -o eth0 -j MASQUERADE
COMMIT
EOF

  run_unit lab-web web "$GUEST_DIR/labweb"
  run_unit lab-iperf web iperf3 -s
  for s in srvA srvB; do
    server_config "$s" >"$CONF/$s.json"
    run_unit "lab-xray-$s" "$s" /usr/local/bin/xray run -c "$CONF/$s.json"
  done
}

server_config() {
  local s=$1
  jq -n --arg uuid "$UUID" --arg crt "$STATE/$s.crt" --arg key "$STATE/$s.pem" '{
    log: {loglevel: "warning"},
    inbounds: [{
      listen: "0.0.0.0", port: 443, protocol: "vless",
      settings: {clients: [{id: $uuid, flow: "xtls-rprx-vision"}], decryption: "none"},
      streamSettings: {network: "raw", security: "tls",
        tlsSettings: {certificates: [{certificateFile: $crt, keyFile: $key}]}}
    }],
    outbounds: [{protocol: "freedom", settings: {finalRules: [{action: "allow", ip: ["203.0.113.0/24"]}]}}]
  }'
}

gateway_config() {
  local outbounds="[]" inbounds rules balancers
  for s in srvA srvB; do
    outbounds=$(jq --arg tag "$s" --arg ip "${SERVER_IP[$s]}" --arg uuid "$UUID" --arg pin "$(pin "$s")" '. + [{
      tag: $tag, protocol: "vless",
      settings: {vnext: [{address: $ip, port: 443, users: [{id: $uuid, flow: "xtls-rprx-vision", encryption: "none"}]}]},
      streamSettings: {network: "raw", security: "tls",
        tlsSettings: {serverName: ($tag + ".lab"), pinnedPeerCertSha256: $pin}}
    }]' <<<"$outbounds")
  done
  inbounds=$(for n in $SLOTS; do
    jq -n --argjson n "$n" '{tag: "slot\($n)", listen: "127.0.0.1", port: (12340 + $n), protocol: "tunnel",
      settings: {network: "tcp,udp", followRedirect: true}, streamSettings: {sockopt: {tproxy: "tproxy"}}}'
  done | jq -s .)
  rules=$(for n in $SLOTS; do jq -n --argjson n "$n" '{inboundTag: ["slot\($n)"], balancerTag: "slot\($n)"}'; done | jq -s .)
  balancers=$(for n in $SLOTS; do jq -n --argjson n "$n" '{tag: "slot\($n)", selector: ["srvA", "srvB"], strategy: {type: "random"}}'; done | jq -s .)
  jq -n --argjson ib "$inbounds" --argjson ob "$outbounds" --argjson rules "$rules" --argjson bal "$balancers" '{
    log: {loglevel: "warning"},
    api: {tag: "api", listen: "127.0.0.1:10085", services: ["HandlerService", "StatsService", "RoutingService"]},
    stats: {},
    policy: {system: {statsInboundUplink: true, statsInboundDownlink: true, statsOutboundUplink: true, statsOutboundDownlink: true}},
    inbounds: $ib,
    outbounds: $ob,
    routing: {rules: $rules, balancers: $bal}
  }'
}

build_gateway() {
  sysctl -qw net.ipv4.ip_unprivileged_port_start=1024  # As the gateway: the VM ships 0, which hides a missing bind capability.
  sysctl -qw net.ipv4.ip_forward=1 net.ipv4.conf.all.rp_filter=0 net.ipv4.conf.default.rp_filter=2 \
    net.ipv4.conf.all.accept_local=0

  ip link add br0 type bridge
  ip addr add 192.168.1.1/24 dev br0
  ip link set br0 up
  veth_into lan lan-br eth0
  ip link set lan-br master br0 up
  ip -n lan addr add 192.168.1.10/24 dev eth0
  ip -n lan link set eth0 up
  ip -n lan route add default via 192.168.1.1

  ip addr add 192.168.100.10/24 dev eth4
  ip link set eth4 up

  for n in 1 2 3; do
    ip link add "wgclt$n" type wireguard
    wg set "wgclt$n" listen-port "4100$n" private-key "$STATE/wgclt$n.key" \
      peer "$(cat "$STATE/${CLT_PEER[$n]}.pub")" \
      endpoint "${CLT_ENDPOINT[$n]}" allowed-ips 0.0.0.0/0 persistent-keepalive 25
    ip addr add "${CLT_ADDR[$n]}/32" dev "wgclt$n"
    ip link set "wgclt$n" up
    ip route add default dev "wgclt$n" table "${CLT_TABLE[$n]}"
  done

  for n in $SLOTS; do
    ip link add "pxa$n" type wireguard
    wg set "pxa$n" listen-port "5183$n" private-key "$STATE/pxa$n.key" \
      peer "$(cat "$STATE/wgclt$n.pub")" allowed-ips "${CLT_ADDR[$n]}/32"
    # Loose rp_filter drops here: pxaN has no address, and the source is wgcltN's own local address.
    sysctl -qw "net.ipv4.conf.pxa$n.accept_local=1" "net.ipv4.conf.pxa$n.rp_filter=0"
    ip link set "pxa$n" up
  done

  # Lab internet prefixes only: the VM's main table keeps Lima's own default route.
  ip route add 198.51.100.0/24 via 192.168.100.1 dev eth4 table 201
  ip route add 203.0.113.0/24 via 192.168.100.1 dev eth4 table 201
  if [ -n "$SLOTS" ]; then
    ip route add local default dev lo table 100
    ip rule add pref 99 fwmark 0x169 lookup 100
  fi

  ip -batch - <<EOF
rule add pref 5270 not fwmark 0x80000/0xff0000 lookup 52
rule add pref 32000 lookup main suppress_prefixlength 0
rule add pref 32500 to 127.0.0.1 lookup 201
rule add pref 32500 to 198.51.100.30 lookup 201
rule add pref 32502 fwmark 0x1a0000/0x7e0000 lookup 201
rule add pref 32504 fwmark ${CLT_MARK[1]}/0x7e0000 lookup ${CLT_TABLE[1]}
rule add pref 32506 fwmark ${CLT_MARK[2]}/0x7e0000 lookup ${CLT_TABLE[2]}
rule add pref 32508 fwmark ${CLT_MARK[3]}/0x7e0000 lookup ${CLT_TABLE[3]}
rule add pref 32765 fwmark 0x10000/0x10000 blackhole
rule add pref 32766 lookup 201
EOF

  iptables_rules | iptables-restore
}

iptables_rules() {
  echo '*raw'
  echo ':PREROUTING ACCEPT [0:0]'
  echo ':OUTPUT ACCEPT [0:0]'
  for n in $SLOTS; do echo "-A PREROUTING -i pxa$n -j CT --notrack"; done
  echo 'COMMIT'

  echo '*mangle'
  for c in PREROUTING INPUT FORWARD OUTPUT POSTROUTING; do echo ":$c ACCEPT [0:0]"; done
  echo ':UBIOS_PREROUTING_USER_HOOK - [0:0]'
  [ -n "$SLOTS" ] && echo ':sidegate-prerouting - [0:0]'
  echo '-A PREROUTING -j UBIOS_PREROUTING_USER_HOOK'
  [ -n "$SLOTS" ] && echo '-A PREROUTING -j sidegate-prerouting'
  for n in 1 2 3; do
    echo "-A UBIOS_PREROUTING_USER_HOOK -d ${CLT_TARGET[$n]}/32 -j MARK --set-xmark $(printf '0x%x' $((CLT_MARK[$n] | 0x10000)))/0x7f0000"
  done
  for n in $SLOTS; do
    echo "-A sidegate-prerouting -i pxa$n -p udp --dport 443 -j DROP"
    echo "-A sidegate-prerouting -i pxa$n -p tcp -j TPROXY --on-ip 127.0.0.1 --on-port 1234$n --tproxy-mark 0x169/0xffffffff"
    echo "-A sidegate-prerouting -i pxa$n -p udp -j TPROXY --on-ip 127.0.0.1 --on-port 1234$n --tproxy-mark 0x169/0xffffffff"
    echo "-A sidegate-prerouting -i pxa$n -j DROP"
  done
  echo 'COMMIT'

  echo '*nat'
  for c in PREROUTING INPUT OUTPUT POSTROUTING; do echo ":$c ACCEPT [0:0]"; done
  echo ':UBIOS_POSTROUTING_USER_HOOK - [0:0]'
  echo '-A POSTROUTING -j UBIOS_POSTROUTING_USER_HOOK'
  echo '-A UBIOS_POSTROUTING_USER_HOOK -o wgclt+ -m comment --comment "UniFi VPN client masquerade" -j MASQUERADE'
  echo '-A UBIOS_POSTROUTING_USER_HOOK -o eth4 -j MASQUERADE'
  echo 'COMMIT'

  echo '*filter'
  for c in INPUT FORWARD OUTPUT; do echo ":$c ACCEPT [0:0]"; done
  if [ -n "$SLOTS" ]; then
    echo ':sidegate-input - [0:0]'
    echo '-A INPUT -j sidegate-input'
    echo '-A sidegate-input -p udp --dport 51831:51838 ! -i lo -j DROP'
    echo ':sidegate-output - [0:0]'
    echo '-A OUTPUT -j sidegate-output'
    for n in $SLOTS; do
      echo "-A sidegate-output -s 10.70.$n.0/30 -o wgclt+ -j RETURN"
      echo "-A sidegate-output -s 10.70.$n.0/30 ! -o lo -j DROP"
    done
  fi
  echo 'COMMIT'
}

start_gateway_xray() {
  gateway_config >"$CONF/gateway.json"
  /usr/local/bin/xray run -test -c "$CONF/gateway.json" >/dev/null
  run_unit lab-xray-gw root /usr/local/bin/xray run -c "$CONF/gateway.json"
  for _ in $(seq 50); do
    /usr/local/bin/xray api bi --server=127.0.0.1:10085 >/dev/null 2>&1 && break
    sleep 0.1
  done
  for n in $SLOTS; do
    /usr/local/bin/xray api bo --server=127.0.0.1:10085 -b "slot$n" "${SLOT_SERVER[$n]}" >/dev/null
  done
}

modprobe -a wireguard xt_TPROXY xt_CT nf_tproxy_ipv4
mkdir -p "$STATE" "$CONF"
for k in wgclt1 wgclt2 wgclt3 pxa1 pxa2 wgsrv; do keypair "$k"; done
certs
teardown
rm -f "$CONF/releases/mode"
build_world
build_gateway
if [ -n "$SLOTS" ]; then
  start_gateway_xray
  echo "topology up"
else
  echo "topology up without slot plumbing"
fi
