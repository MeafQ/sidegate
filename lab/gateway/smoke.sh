#!/bin/bash
# Design B on the real gateway with no UniFi change and no device's traffic: a netns
# holds the stand-in servers and the target, and local WireGuard links play UniFi's
# VPN Clients. Everything is in memory. cleanup.sh removes it, on a timer if need be.
B=/tmp/sgtest; S=$B/state; mkdir -p $S; RL=/opt/sidegate-lab/current/runtimelab
PASS=0; FAIL=0
pass() { echo "PASS $*"; PASS=$((PASS+1)); }
fail() { echo "FAIL $*"; FAIL=$((FAIL+1)); }
expect() { [ "$2" = "$3" ] && pass "$1 = $2" || fail "$1: got '$2', want '$3'"; }
snapshot() { ip rule; ip -o link show | awk '{print $2}' | sed 's/@.*//' | sort; for t in raw mangle nat filter; do iptables-save -t "$t" | grep -vE '^#|^:' ; done; ip6tables-save -t filter | grep -vE '^#|^:'; ip route show table main; ls /etc/systemd/system | sort; }
exit_ip() { ip netns exec sglan curl -s -m "${2:-5}" "http://203.0.113.8$1/ip"; }
wait_exit() { local want=$2 got= start=$SECONDS; while [ $((SECONDS-start)) -lt 40 ]; do got=$(exit_ip "$1" 2); [ "$got" = "$want" ] && break; sleep 0.5; done; expect "slot$1 exit after $((SECONDS-start)) s" "$got" "$want"; }
health() { ping -c1 -W2 1.1.1.1 >/dev/null && pass "$1: WAN answers" || fail "$1: WAN silent"; [ -n "$(wg show wgclt6 latest-handshakes 2>/dev/null)" ] && pass "$1: wgclt6 present" || fail "$1: wgclt6 missing"; ip link show tailscale0 >/dev/null 2>&1 && pass "$1: tailscale0 present" || fail "$1: tailscale0 missing"; }
core() { systemctl show -p MainPID --value sidegate-core; }
usage() { local p=$(core); [ "$p" -gt 0 ] 2>/dev/null && awk '/VmRSS/{print $2" kB core RSS"}' /proc/$p/status; }
cpu() { awk '{print $14+$15}' /proc/$1/stat; }

systemd-run --quiet --unit=sgt-deadman --on-active="${DEADMAN:-15min}" $B/cleanup.sh
snapshot >$S/pre
mkdir -p /opt/sidegate-lab /var/lib/unifi-lab
mount -t tmpfs -o size=128m,mode=755 tmpfs /opt/sidegate-lab; mount -t tmpfs -o size=4m,mode=700 tmpfs /var/lib/unifi-lab
mkdir -p /opt/sidegate-lab/current; cp $B/sidegated $B/runtimelab $B/xray /opt/sidegate-lab/current/; tar -C /var/lib/unifi-lab -xf $B/keys.tar
modprobe -a wireguard xt_TPROXY xt_CT 2>/dev/null

# Stand-in servers and target, all inside one namespace.
ip netns add sgtest; ip link add sgts0 type veth peer name eth0 netns sgtest
ip addr add 198.51.100.1/24 dev sgts0; ip link set sgts0 up
ip -n sgtest link set lo up; ip -n sgtest link set eth0 up
for a in 198.51.100.10 198.51.100.20; do ip -n sgtest addr add $a/24 dev eth0; done
ip -n sgtest route add default via 198.51.100.1
# The target has its own namespace because it also serves 443. It sees each server's address.
ip netns add sgweb; ip -n sgtest link add eth1 type veth peer name eth0 netns sgweb
ip -n sgtest link set eth1 up; ip -n sgtest route add 203.0.113.0/24 dev eth1
ip -n sgweb link set lo up; ip -n sgweb link set eth0 up
for a in 203.0.113.81 203.0.113.82; do ip -n sgweb addr add $a/24 dev eth0; done
ip -n sgweb route add 198.51.100.0/24 dev eth0
# This kernel's ARP defaults answer for one server's address only, so name both.
m=$(ip netns exec sgtest cat /sys/class/net/eth1/address); for a in 198.51.100.10 198.51.100.20; do ip -n sgweb neigh replace $a lladdr $m dev eth0 nud permanent; done
run() { local u=$1 ns=$2; shift 2; systemd-run --quiet --unit=$u -p NetworkNamespacePath=/run/netns/$ns -p LimitNOFILE=4096 "$@"; }
run sgt-web sgweb $B/labweb
for s in srvA:198.51.100.10 srvB:198.51.100.20; do n=${s%%:*}; a=${s##*:}
  jq -n --arg crt /var/lib/unifi-lab/$n.crt --arg key /var/lib/unifi-lab/$n.pem --arg a $a --arg id 6f1c1e52-0d3c-4b8e-9a51-2c1f7c0a5e11 '{log:{loglevel:"warning"},inbounds:[{listen:$a,port:443,protocol:"vless",settings:{clients:[{id:$id,flow:"xtls-rprx-vision"}],decryption:"none"},streamSettings:{network:"raw",security:"tls",tlsSettings:{certificates:[{certificateFile:$crt,keyFile:$key}]}}}],outbounds:[{protocol:"freedom",sendThrough:$a,settings:{finalRules:[{action:"allow",ip:["203.0.113.0/24"]}]}}]}' >$S/$n.json
  run sgt-$n sgtest $B/xray run -c $S/$n.json
done
sleep 1
$RL unit >/run/systemd/system/sidegate-core.service; systemctl daemon-reload

echo "== install"
out=$($RL once 2>&1); rc=$?; echo "$out" | sed 's/^/   /'; expect "first reconcile exit" $rc 0
expect "pxa1" "$(ip -br link show pxa1 | awk '{print $2}' | grep -q DOWN && echo down || echo up)" up
expect "pxa2" "$(ip -br link show pxa2 | awk '{print $2}' | grep -q DOWN && echo down || echo up)" up
out=$($RL once 2>&1); expect "second reconcile changes" "$(awk '/^changes: /{n=$2} END{print n}' <<<"$out")" 0
health "installed"

echo "== stand-ins for UniFi's VPN Clients"
for n in 1 2; do
  ip link add sgclt$n type wireguard
  wg set sgclt$n private-key /var/lib/unifi-lab/wgclt$n.key peer "$(cat /var/lib/unifi-lab/pxa$n.pub)" endpoint 127.0.0.1:5183$n allowed-ips 0.0.0.0/0 persistent-keepalive 5
  ip addr add 10.70.$n.2/30 dev sgclt$n; ip link set sgclt$n up
  ip route add 203.0.113.8$n/32 dev sgclt$n
done
# A LAN device's traffic reaches a VPN Client forwarded and NATed. Our egress guard rightly
# drops the gateway's own traffic with a tunnel source. These rules stand in for what UniFi
# adds for a real VPN Client, and only for this test's addresses.
ip netns add sglan; ip link add sglan0 type veth peer name eth0 netns sglan
ip addr add 10.71.0.1/24 dev sglan0; ip link set sglan0 up
ip -n sglan link set lo up; ip -n sglan link set eth0 up; ip -n sglan addr add 10.71.0.2/24 dev eth0; ip -n sglan route add default via 10.71.0.1
iptables -I FORWARD -i sglan0 -j ACCEPT -m comment --comment sgtest
iptables -I FORWARD -o sglan0 -j ACCEPT -m comment --comment sgtest
for n in 1 2; do iptables -t nat -I POSTROUTING -s 10.71.0.0/24 -o sgclt$n -j MASQUERADE -m comment --comment sgtest; done
[ -n "${HOLD:-}" ] && { echo "holding; cleanup.sh runs on the timer"; exit 0; }
wait_exit 1 198.51.100.10
wait_exit 2 198.51.100.20
wg show pxa1 latest-handshakes | awk '{print "pxa1 handshake at "$2}'

echo "== switch"
$RL once --select slot1=srvB >/dev/null 2>&1
wait_exit 1 198.51.100.20
wait_exit 2 198.51.100.20
$RL once --from slot1=srvB >/dev/null 2>&1
wait_exit 1 198.51.100.10

echo "== core down closes the gates, and nothing leaves another way"
systemctl stop sidegate-core
sleep 1
expect "pxa1 after the core stopped" "$(cat /sys/class/net/pxa1/flags | python3 -c 'import sys;print("up" if int(sys.stdin.read(),16)&1 else "down")')" down
g=$(exit_ip 1 3); [ -z "$g" ] && pass "no answer through slot1 with the core stopped" || fail "slot1 answered '$g' with the core stopped"
health "core stopped"
$RL once >/dev/null 2>&1
wait_exit 1 198.51.100.10

echo "== cost"
usage
p=$(core); c0=$(cpu $p); t0=$SECONDS
ip netns exec sglan curl -s -m 60 -o /dev/null -w 'download through slot1: %{speed_download} bytes/s, %{size_download} bytes\n' "http://203.0.113.81/bytes?n=${BYTES:-104857600}"
d=$((SECONDS-t0)); c1=$(cpu $p); echo "core CPU during download: $((c1-c0)) ticks over $d s (100 ticks = one core for 1 s, 4 cores)"; usage
c0=$(cpu $p); sleep 20; c1=$(cpu $p); echo "core CPU idle: $((c1-c0)) ticks over 20 s"
timeout -s TERM 21 $RL run >$S/run.log 2>&1 & rp=$!; sleep 1; d0=$(pgrep -n -f 'runtimelab run'); c0=$(awk '{print $14+$15+$16+$17}' /proc/$d0/stat); sleep 18; c1=$(awk '{print $14+$15+$16+$17}' /proc/$d0/stat); echo "runtime CPU: $((c1-c0)) ticks over 18 s at a 1 s poll, with its tool children"; wait $rp
grep -cE '^.*pass [0-9]+: 0 steps' $S/run.log | sed 's/^/idle passes: /'; grep -E 'error|slot .*:' $S/run.log | sort | uniq -c | head -5

echo "== did UniFi leave our state alone"
expect "jump in mangle PREROUTING" "$(iptables -t mangle -S PREROUTING | grep -c sidegate-prerouting)" 1
expect "rule pref 99" "$(ip rule | grep -c '^99:')" 1

echo "== teardown"
iptables -D FORWARD -i sglan0 -j ACCEPT -m comment --comment sgtest
iptables -D FORWARD -o sglan0 -j ACCEPT -m comment --comment sgtest
for n in 1 2; do iptables -t nat -D POSTROUTING -s 10.71.0.0/24 -o sgclt$n -j MASQUERADE -m comment --comment sgtest; done
ip link del sglan0; ip netns del sglan
for n in 1 2; do ip route del 203.0.113.8$n/32 dev sgclt$n; ip link del sgclt$n; done
$RL teardown 2>&1 | tail -2 | sed 's/^/   /'
systemctl stop sgt-web sgt-srvA sgt-srvB
ip link del sgts0 2>/dev/null; ip netns del sgtest; ip netns del sgweb
rm -f /run/systemd/system/sidegate-core.service; systemctl daemon-reload
umount /opt/sidegate-lab /var/lib/unifi-lab; rmdir /opt/sidegate-lab /var/lib/unifi-lab
rm -rf /run/sidegate
snapshot >$S/post
diff $S/pre $S/post >$S/diff && pass "rules, links, firewall, main routes and units match the pre-install snapshot" || { fail "snapshot differs"; head -30 $S/diff | cut -c1-140; }
health "torn down"
echo "RESULT $PASS passed, $FAIL failed"
