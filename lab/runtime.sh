#!/usr/bin/env bash
# Runs as root inside the VM. Proves the daemon's runtime builds design B's data
# path itself. Usage: runtime.sh <setup|fresh|install|idempotent|env|egress|localguard|established|ipv6guard|flush|
# conflict|foreignrule|probe|telemetry|switch|routing|crossslot|network|balancer|premise|restart|idle|churn|flapping|noland|nobinary|blips|gatelock|apistall|slowread|portheld|heldadd|outage|corestop|coredeath|orphan|hung|linkcrash|reinstall|absentlinks|foreignlink|collision|foreign|orphanrule|mixed|renderers|named|teardown>
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/names.sh"

LAB=/opt/unifi-lab
RL=${RL:-$LAB_HOME/current/runtimelab}
SNAP=/var/lib/unifi-lab/snapshot
DAEMON=lab-runtime
LAN="ip netns exec lan"
SLOT1=203.0.113.81
SRV_A=198.51.100.10
SRV_B=198.51.100.20
UPLINK=$(ip -o route show default table main | awk '{print $5; exit}')

STEP=${1-}
fails=0
pass() { echo "PASS $STEP: $*"; }
fail() { echo "FAIL $STEP: $*"; fails=$((fails + 1)); }
note() { echo "       $*"; }
expect() {
  local what=$1 got=$2 want=$3
  if [ "$got" = "$want" ]; then pass "$what = $got"; else fail "$what = '$got', want '$want'"; fi
}

exit_ip() { $LAN curl -s -m "${2:-5}" "http://$1/ip"; }
changes() { awk '/^changes: / {n = $2} END {print n}' <<<"$1"; }
indent() { sed 's/^/       /'; }
link_up() { [ $(( $(cat "/sys/class/net/$1/flags" 2>/dev/null || echo 0) & 1 )) -eq 1 ] && echo up || echo down; }
carrier() { cat "/sys/class/net/$1/carrier_changes"; }
unit_prop() { systemctl show -p "$2" --value "$1"; }
pkts() { tcpdump -r "$1" -nn 2>/dev/null | wc -l; }
API=127.0.0.1:10085
tag_of() { /usr/local/bin/xray api lso --server=$API | jq -r --arg p "o.$1." '.outbounds[].tag | select(startswith($p))'; }
rule_target() { /usr/local/bin/xray api lsrules --server=$API | jq -r --arg r "$1" '.rules[] | select(.ruleTag == $r) | .tag // "balancer"'; }

snapshot() {
  iptables-save | grep -v '^#' | sed -E 's/\[[0-9]+:[0-9]+\]/[0:0]/'
  echo '-- ip6tables'; ip6tables-save | grep -E '^(-A |:[A-Z_]+ - )' | sed -E 's/\[[0-9]+:[0-9]+\]/[0:0]/'
  echo '-- ip rule'; ip rule show
  echo '-- table 100'; ip route show table 100 2>/dev/null
  echo '-- links'; ip -o link show | awk -F': ' '{print $2}' | cut -d@ -f1 | sort
}

# wait_exit polls a slot until it exits via want. A gate that just opened waits
# for the UniFi-side peer's next handshake retry, up to about 5 s.
wait_exit() {
  local want=$1 target=${2:-$SLOT1} got="" start; start=$(date +%s)
  while [ $(( $(date +%s) - start )) -lt 40 ]; do
    got=$(exit_ip "$target" 2) && [ "$got" = "$want" ] && break
    sleep 0.5
  done
  expect "$target exit after $(( $(date +%s) - start )) s" "$got" "$want"
}

leak_watch() {
  tcpdump -i eth4 -nn -U -w "$1/eth4.pcap" "dst host $SLOT1" 2>/dev/null & echo $! >"$1/pids"
  tcpdump -i "$UPLINK" -nn -U -w "$1/uplink.pcap" "dst host $SLOT1" 2>/dev/null & echo $! >>"$1/pids"
  sleep 1
}
leak_count() {
  sleep 0.5; kill $(cat "$1/pids"); wait $(cat "$1/pids") 2>/dev/null
  echo $(( $(pkts "$1/eth4.pcap") + $(pkts "$1/uplink.pcap") ))
}

# daemon_start runs the long-lived actor as a unit and waits for its first
# converged pass. It prints the journal lines of this run.
daemon_start() {
  local since; since=$(date '+%F %T')
  systemd-run --unit=$DAEMON --collect --quiet "$RL" run
  for _ in $(seq 150); do
    journalctl -u $DAEMON --since "$since" -o cat | grep -q ': 0 steps' && break
    sleep 0.2
  done
  journalctl -u $DAEMON --since "$since" -o cat
}
# A step's journal window starts on a whole second, so the next step must start in a later one.
daemon_stop() { systemctl stop $DAEMON 2>/dev/null; sleep 1; true; }

step_setup() {
  daemon_stop
  $LAB/topology.sh --no-slots | indent
  "$RL" unit >/etc/systemd/system/$CORE_UNIT
  systemctl daemon-reload
  snapshot >"$SNAP.pre"
  expect "pxa links before install" "$(ip -o link show | grep -c ' pxa')" 0
  pass "core unit installed, pre-install snapshot has $(wc -l <"$SNAP.pre") lines"
}

step_fresh() {
  if ip route show table 100 >/dev/null 2>&1; then
    fail "table 100 exists before install: the boot is not fresh"
  else
    pass "table 100 has never existed, as on a freshly booted gateway"
  fi
}

step_install() {
  local out; out=$("$RL" once 2>&1); echo "$out" | indent
  local n; n=$(changes "$out")
  [ "${n:-0}" -gt 0 ] && pass "first reconcile made $n changes" || fail "first reconcile made '${n}' changes"
  expect "core unit" "$(systemctl is-active $CORE_UNIT)" active
  expect "pxa1" "$(link_up pxa1)" up
  expect "pxa2" "$(link_up pxa2)" up
  local o; o=$(grep -n 'core override b.slot1' <<<"$out" | cut -d: -f1); local u; u=$(grep -n 'link pxa1 up' <<<"$out" | cut -d: -f1)
  [ -n "$o" ] && [ -n "$u" ] && [ "$o" -lt "$u" ] && pass "slot1 override applied (line $o) before pxa1 opened (line $u)" || fail "pxa1 opened without a prior override"
  wait_exit $SRV_A
  wait_exit $SRV_B 203.0.113.82
}

step_idempotent() {
  local out; out=$("$RL" once 2>&1); echo "$out" | indent
  expect "second reconcile changes" "$(changes "$out")" 0
}

step_env() {
  echo "export GW_UNIT=$CORE_UNIT XRAY_API=127.0.0.1:10085 BAL1=b.slot1 OUT_A=$(tag_of srvA) OUT_B=$(tag_of srvB) GUARD_MATCH=dpt:51831"
}

step_egress() {
  local dir; dir=$(mktemp -d)
  local d0; d0=$(iptables -nvxL sidegate-output | awk '$3 == "DROP" && $8 == "10.70.1.0/30" {print $1}')
  tcpdump -i eth4 -nn -U -w "$dir/eth4.pcap" "dst host $SLOT1" 2>/dev/null & local p=$!
  sleep 1
  curl -s -m 2 --interface 10.70.1.2 "http://$SLOT1/ip" >/dev/null && fail "a socket on wgclt1's address reached $SLOT1 outside the tunnel"
  sleep 0.5; kill $p; wait $p 2>/dev/null
  local d1; d1=$(iptables -nvxL sidegate-output | awk '$3 == "DROP" && $8 == "10.70.1.0/30" {print $1}')
  [ $((d1 - d0)) -gt 0 ] && pass "packets from slot1's tunnel toward eth4 dropped by the egress guard (+$((d1 - d0)))" || fail "egress guard counter did not move"
  expect "tunnel-sourced packets to $SLOT1 leaving eth4" "$(pkts "$dir/eth4.pcap")" 0
  rm -rf "$dir"
}

connects() { timeout 2 bash -c "exec 3<>/dev/tcp/127.0.0.1/$1" 2>/dev/null; }

step_localguard() {
  local port
  for port in 10085 10086 12341; do
    if runuser -u nobody -- bash -c "$(declare -f connects); connects $port"; then
      fail "uid nobody connects to the core's 127.0.0.1:$port"
    else
      pass "uid nobody cannot connect to the core's 127.0.0.1:$port"
    fi
    connects $port && pass "root connects to 127.0.0.1:$port" || fail "root cannot connect to 127.0.0.1:$port"
  done
  runuser -u nobody -- /usr/local/bin/xray api lso --server=$API >/dev/null 2>&1 &&
    fail "uid nobody reads the core API" || pass "uid nobody cannot read the core API"
  /usr/local/bin/xray api lso --server=$API >/dev/null 2>&1 && pass "root reads the core API" || fail "root cannot read the core API"
  local stuck; stuck=$(ss -Htan state last-ack '( sport = :10086 )' | wc -l)
  for _ in $(seq 20); do connects 10086; done
  sleep 1
  expect "core sockets newly stuck in LAST-ACK after 20 root connections closed" \
    "$(( $(ss -Htan state last-ack '( sport = :10086 )' | wc -l) - stuck ))" 0
}

# h2_client runs as the user it is started as. On one HTTP/2 connection to the
# core API it overrides slot1 to $2, writes how that went to $1/first, waits for
# $1/go, then overrides slot1 to $3 and writes $1/second.
h2_client() {
  python3 - "$@" <<'PY'
import os, socket, struct, sys, time
d, first, second = sys.argv[1:4]
def frame(kind, flags, stream, payload=b""):
    return struct.pack(">I", len(payload))[1:] + bytes([kind, flags]) + struct.pack(">I", stream) + payload
def literal(name, value):
    return b"\0" + bytes([len(name)]) + name + bytes([len(value)]) + value
def override(stream, target):
    head = b"".join(literal(k, v) for k, v in [(b":method", b"POST"), (b":scheme", b"http"),
        (b":path", b"/xray.app.router.command.RoutingService/OverrideBalancerTarget"),
        (b":authority", b"127.0.0.1:10085"), (b"content-type", b"application/grpc"), (b"te", b"trailers")])
    msg = b"\x0a\x07b.slot1\x12" + bytes([len(target)]) + target.encode()
    return frame(1, 4, stream, head) + frame(0, 1, stream, b"\0" + struct.pack(">I", len(msg)) + msg)
def call(stream, target, name):
    buf, result = b"", "answered"
    try:
        s.sendall(override(stream, target))
        while True:
            while len(buf) < 9 or len(buf) < 9 + int.from_bytes(buf[:3], "big"):
                chunk = s.recv(65536)
                if not chunk:
                    raise ConnectionResetError("closed by the core")
                buf += chunk
            n, kind, flags = int.from_bytes(buf[:3], "big"), buf[3], buf[4]
            sid = int.from_bytes(buf[5:9], "big") & 0x7fffffff
            buf = buf[9 + n:]
            if kind == 4 and not flags & 1:
                s.sendall(frame(4, 1, 0))
            if sid == stream and flags & 1:
                break
    except (ConnectionResetError, BrokenPipeError):
        result = "reset"
    except OSError as e:
        result = repr(e)
    open(d + "/" + name, "w").write(result)
s = socket.create_connection(("127.0.0.1", 10085), timeout=5)
open(d + "/port", "w").write(str(s.getsockname()[1]))
s.sendall(b"PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n" + frame(4, 0, 0))
call(1, first, "first")
while not os.path.exists(d + "/go"):
    time.sleep(0.05)
call(3, second, "second")
PY
}

# step_established opens a non-root API connection while the owner guard is
# missing, lets the runtime restore the guard, and checks the connection can do
# nothing more while root's connections still close cleanly.
step_established() {
  local dir a b; dir=$(mktemp -d); chmod 777 "$dir"; a=$(tag_of srvA); b=$(tag_of srvB)
  iptables -S sidegate-output | grep -e '-m owner' | sed 's/^-A /-D /' | while read -r r; do iptables $r; done
  note "owner guard removed; uid nobody connects to the core API and overrides slot1 to its planned srvA"
  runuser -u nobody -- bash -c "$(declare -f h2_client); h2_client $dir $a $b" &
  local client=$!
  for _ in $(seq 100); do [ -s "$dir/first" ] && break; sleep 0.05; done
  expect "uid nobody's call before the repair" "$(cat "$dir/first" 2>/dev/null)" answered
  local out; out=$("$RL" once 2>&1); echo "$out" | indent
  grep -q 'iptables filter sidegate-output write' <<<"$out" && pass "reconcile restored the owner guard" || fail "reconcile left the guard out"
  touch "$dir/go"; wait $client
  expect "uid nobody's override to srvB on the same connection after the repair" "$(cat "$dir/second")" reset
  expect "slot1 override after that call" "$(/usr/local/bin/xray api bi --server=$API b.slot1 | awk '/Override/ {getline; print $2}')" "$a"
  note "the core's half of that connection: $(ss -Htn state all "( sport = :10085 and dport = :$(cat "$dir/port") )" | awk '{print $1}' | grep . || echo gone); it closes on the core's next write or keepalive"
  local stuck; stuck=$(ss -Htan state last-ack '( sport = :10085 )' | wc -l)
  for _ in $(seq 20); do /usr/local/bin/xray api lso --server=$API >/dev/null; done
  sleep 1
  expect "core API sockets newly stuck in LAST-ACK after 20 root calls" "$(( $(ss -Htan state last-ack '( sport = :10085 )' | wc -l) - stuck ))" 0
  rm -rf "$dir"
}

# udp_delivered counts datagrams the kernel handed to a UDP socket. On 5.4 the
# IPv6 encapsulation path WireGuard uses counts them in the IPv4 Udp MIB.
udp_delivered() {
  echo $(( $(awk '/^Udp:/ {getline; print $2; exit}' /proc/net/snmp) + $(awk '$1 == "Udp6InDatagrams" {print $2}' /proc/net/snmp6) ))
}
v6_guard_drops() { ip6tables -nvxL sidegate-input 2>/dev/null | awk '$3 == "DROP" && /dpt:51831/ {print $1}' | grep . || echo 0; }

step_ipv6guard() {
  local gw d0 g0; gw=$(ip -6 addr show dev br0 scope link | awk '/inet6/ {print $2}' | cut -d/ -f1)
  d0=$(udp_delivered); g0=$(v6_guard_drops)
  $LAN python3 -c '
import socket, sys
s = socket.socket(socket.AF_INET6, socket.SOCK_DGRAM)
for _ in range(50):
    s.sendto(b"x" * 32, (sys.argv[1], 51831, 0, socket.if_nametoindex("eth0")))
' "$gw"
  sleep 0.5
  note "the LAN sent 50 UDP datagrams to pxa1's listen port at [$gw]:51831"
  expect "IPv6 datagrams to pxa1's port dropped by the guard" "$(( $(v6_guard_drops) - g0 ))" 50
  local delivered; delivered=$(( $(udp_delivered) - d0 ))
  [ "$delivered" -lt 50 ] && pass "datagrams delivered to any UDP socket meanwhile: $delivered of 50" ||
    fail "the LAN reached pxa1's WireGuard socket over IPv6: $delivered datagrams delivered"
}

step_flush() {
  local dir; dir=$(mktemp -d)
  iptables -t mangle -F PREROUTING
  iptables -t mangle -A PREROUTING -j UBIOS_PREROUTING_USER_HOOK
  ip rule del pref 99 fwmark 0x169 lookup 100
  note "foreign flush: mangle PREROUTING flushed, UniFi's hook re-added, pref 99 rule deleted"
  leak_watch "$dir"
  if exit_ip $SLOT1 4 >/dev/null; then fail "slot1 answers with interception flushed"; else pass "flushed: slot1 traffic stops"; fi
  expect "flushed: packets to $SLOT1 leaving eth4 or $UPLINK" "$(leak_count "$dir")" 0
  local out; out=$("$RL" once 2>&1); echo "$out" | indent
  local n; n=$(changes "$out")
  [ "${n:-0}" -gt 0 ] && pass "repair made $n changes" || fail "repair made '${n}' changes"
  grep -q 'iptables mangle sidegate-prerouting write' <<<"$out" && grep -q 'ip rule add 99:' <<<"$out" &&
    pass "repair rewrote our mangle chain and the pref 99 rule" || fail "repair missed the chain or the rule"
  grep -q 'link pxa. down' <<<"$out" && fail "repair closed a gate its unreachable rule kept fail-closed" ||
    pass "repair kept the gates open: the iif unreachable rule held the slot fail-closed"
  expect "first mangle PREROUTING rule" "$(iptables -t mangle -S PREROUTING | sed -n 2p)" "-A PREROUTING -j sidegate-prerouting"
  expect "UniFi's hook kept" "$(iptables -t mangle -S PREROUTING | grep -c UBIOS_PREROUTING_USER_HOOK)" 1
  wait_exit $SRV_A
  out=$("$RL" once 2>&1)
  expect "reconcile after repair changes" "$(changes "$out")" 0
  rm -rf "$dir"
}

step_conflict() {
  sysctl -qw net.ipv4.conf.all.rp_filter=1
  local out; out=$("$RL" once 2>&1); echo "$out" | indent
  grep -q 'kernel conflicts: net.ipv4.conf.all.rp_filter is 1' <<<"$out" && pass "conflict reported" || fail "conflict not reported"
  expect "conflict: pxa1" "$(link_up pxa1)" down
  expect "conflict: pxa2" "$(link_up pxa2)" down
  expect "conflict: all.rp_filter left as the other owner set it" "$(sysctl -n net.ipv4.conf.all.rp_filter)" 1
  sysctl -qw net.ipv4.conf.all.rp_filter=0
  out=$("$RL" once 2>&1); echo "$out" | indent
  expect "conflict cleared: pxa1" "$(link_up pxa1)" up
  wait_exit $SRV_A
}

step_foreignrule() {
  ip link add pxa9 type dummy
  ip rule add pref 100 iif pxa9 unreachable
  note "foreign software's pxa9 and its iif unreachable rule at our preference"
  local out; out=$("$RL" once 2>&1); local rc=$?; echo "$out" | indent
  expect "reconcile exit status beside a foreign rule" "$rc" 0
  expect "foreign pxa9 rule kept" "$(ip rule show pref 100 | grep -c 'iif pxa9')" 1
  grep -q 'link pxa. down' <<<"$out" && fail "a gate closed over a foreign rule" || pass "gates stayed open beside a foreign rule"
  ip rule del pref 100 iif pxa9 unreachable 2>/dev/null
  ip link del pxa9
}

step_probe() {
  local out
  out=$("$RL" probe srvA http://203.0.113.80/ip 2>&1); expect "probe route srvA exit" "$(awk '/^probe / {print $3}' <<<"$out")" $SRV_A
  out=$("$RL" probe srvB http://203.0.113.80/ip 2>&1); expect "probe route srvB exit" "$(awk '/^probe / {print $3}' <<<"$out")" $SRV_B
}

step_telemetry() {
  local out; out=$("$RL" telemetry 2>&1); grep -E '^(core|counter)' <<<"$out" | indent
  expect "telemetry core PID" "$(awk '/^core pid/ {print $3}' <<<"$out")" "$(unit_prop $CORE_UNIT MainPID)"
  expect "telemetry epoch" "$(awk '/^core pid/ {print $5}' <<<"$out")" "$(unit_prop $CORE_UNIT InvocationID)"
  local down; down=$(awk '/^counter srvA/ {print $6}' <<<"$out")
  [ "${down:-0}" -gt 1000000 ] && pass "srvA downlink counter $down bytes" || fail "srvA downlink counter '${down}'"
  awk '/^counter srvB/ {f = 1} END {exit !f}' <<<"$out" && pass "srvB counter present" || fail "no srvB counter"
}

step_switch() {
  local dir; dir=$(mktemp -d)
  local c0; c0=$(carrier pxa1)
  $LAN curl -s -m 60 --limit-rate 10M -o /dev/null -w '%{size_download} %{http_code}' "http://$SLOT1/bytes?n=60000000" >"$dir/r" &
  local dl=$!
  sleep 1.5
  local out; out=$("$RL" once --select slot1=srvB 2>&1); echo "$out" | indent
  expect "switch changes: a routing rewrite, which resets both overrides, and both overrides" "$(changes "$out")" 3
  grep -q "core replace routing" <<<"$out" && grep -q "core override b.slot1 -> o.srvB" <<<"$out" &&
    pass "switch rewrote routing and overrode slot1 to srvB" || fail "switch missed the rewrite or the override"
  grep -q '  link ' <<<"$out" && fail "switch touched a gate" || pass "switch touched no gate"
  expect "new connection on slot1 exits via" "$(exit_ip $SLOT1)" $SRV_B
  kill -0 $dl 2>/dev/null && pass "download still running after the switch" || fail "download ended before the switch"
  wait $dl
  expect "download across the switch (bytes code)" "$(cat "$dir/r")" "60000000 200"
  expect "pxa1 carrier changes during the switch" "$(carrier pxa1)" "$c0"
  out=$("$RL" once --from slot1=srvB 2>&1)
  expect "switch back changes" "$(changes "$out")" 3
  expect "slot1 back on" "$(exit_ip $SLOT1)" $SRV_A
  rm -rf "$dir"
}

udp_exit() { $LAN $LAB/labweb udp "$1:9000" 2>/dev/null; }
rules_read() { /usr/local/bin/xray api lsrules --server=$API | jq -c '[.rules[] | {ruleTag, tag}]'; }

# drift replaces the core's routing with the rendered routing passed through a
# jq map over its rules ($b is srvB's outbound tag), then sets both overrides as
# planned, so only the filter differs from the plan.
drift() {
  local f; f=$(mktemp)
  jq --arg b "$(tag_of srvB)" "{routing} | .routing.rules |= map($1)" $RUN_DIR/xray.json >"$f"
  /usr/local/bin/xray api adrules --server=$API "$f" >/dev/null
  /usr/local/bin/xray api bo --server=$API -b b.slot1 "$(tag_of srvA)" >/dev/null
  /usr/local/bin/xray api bo --server=$API -b b.slot2 "$(tag_of srvB)" >/dev/null
  rm -f "$f"
}

# repaired lets the runtime converge and checks it rewrote the core's routing
# and slot1's TCP and UDP exit through srvA again.
repaired() {
  local out; out=$("$RL" once 2>&1); echo "$out" | indent
  grep -q 'core replace routing' <<<"$out" && pass "$1: reconcile rewrote the drifted routing" || fail "$1: reconcile kept the drifted routing"
  expect "$1: pxa1" "$(link_up pxa1)" up
  wait_exit $SRV_A
  expect "$1: slot1 UDP exit" "$(udp_exit $SLOT1)" $SRV_A
}

step_routing() {
  drift 'if .ruleTag == "b.slot1" then del(.balancerTag) + {outboundTag: $b} else . end'
  ip link set pxa1 down
  note "drift: slot1's rule re-pointed to srvB under its own rule tag, both overrides as planned, pxa1 closed"
  expect "drifted slot1 rule target" "$(rule_target b.slot1)" "$(tag_of srvB)"
  repaired "re-pointed rule"
  expect "slot1 rule target after reconcile" "$(rule_target b.slot1)" balancer
}

step_crossslot() {
  local before; before=$(rules_read)
  drift 'if .ruleTag == "b.slot1" then .port = "0,443" elif (.ruleTag | startswith("p.srvB.")) then del(.user) + {inboundTag: ["s.slot1.tcp", "s.slot1.udp"]} else . end'
  ip link set pxa1 down
  note "drift: slot1's rule matches ports 0 and 443 only, srvB's probe rule takes slot1's inbounds, overrides as planned, pxa1 closed"
  [ "$(rules_read)" = "$before" ] && pass "rule tags and outbounds read back unchanged by the drift" || fail "the drift changed ListRule's readback"
  repaired "cross-slot"
}

step_network() {
  drift 'if .ruleTag == "b.slot1" then .network = "tcp" else . end'
  note "drift: slot1's rule matches TCP only, overrides as planned, pxa1 open"
  if udp_exit $SLOT1 >/dev/null; then fail "slot1 UDP answers under the drift"; else pass "slot1 UDP dead under the drift"; fi
  repaired "network matcher"
}

step_balancer() {
  drift 'if .ruleTag == "b.slot1" then .balancerTag = "b.slot2" else . end'
  note "drift: slot1's rule feeds slot2's balancer under its own rule tag, overrides as planned, pxa1 open"
  expect "slot1 exit under the drift" "$(exit_ip $SLOT1)" $SRV_B
  repaired "balancer target"
}

# step_premise tests the premise that readback can prove the core routes as
# planned. This drift reads back as planned at every port the runtime samples,
# while slot1's real traffic exits through srvB. Only a rewrite before the gate
# opens repairs it.
step_premise() {
  local before; before=$(rules_read)
  drift 'if .ruleTag == "b.slot1" then .port = "0,443,49152" elif (.ruleTag | startswith("p.srvB.")) then del(.user) + {inboundTag: ["s.slot1.tcp", "s.slot1.udp"]} else . end'
  note "drift: slot1's rule matches ports 0, 443 and 49152 only, srvB's probe rule takes slot1's inbounds, overrides as planned, pxa1 open"
  [ "$(rules_read)" = "$before" ] && pass "rule tags and outbounds read back unchanged by the drift" || fail "the drift changed ListRule's readback"
  expect "slot1 TCP exit under the drift" "$(exit_ip $SLOT1)" $SRV_B
  expect "slot1 UDP exit under the drift" "$(udp_exit $SLOT1)" $SRV_B
  local out; out=$("$RL" once 2>&1); echo "$out" | indent
  expect "premise refuted: changes from a reconcile whose readback agrees with the plan" "$(changes "$out")" 0
  expect "slot1 TCP exit after that reconcile" "$(exit_ip $SLOT1)" $SRV_B
  ip link set pxa1 down
  note "pxa1 closed, so the next pass opens it"
  repaired "premise drift"
}

step_restart() {
  daemon_start | indent
  local e0 p0 c0; e0=$(unit_prop $CORE_UNIT InvocationID); p0=$(unit_prop $CORE_UNIT MainPID); c0=$(carrier pxa1)
  local dir; dir=$(mktemp -d)
  $LAN curl -s -m 60 --limit-rate 10M -o /dev/null -w '%{size_download} %{http_code}' "http://$SLOT1/bytes?n=60000000" >"$dir/r" &
  local dl=$!
  sleep 1.5
  local since; since=$(date '+%F %T')
  systemctl restart $DAEMON
  local first=""
  for _ in $(seq 100); do
    first=$(journalctl -u $DAEMON --since "$since" -o cat | grep -m1 ' pass 1: ')
    [ -n "$first" ] && break
    sleep 0.1
  done
  note "restarted daemon: $first"
  [[ "$first" == *"pass 1: 0 steps"* ]] && pass "restarted daemon's first pass changed nothing" || fail "restarted daemon's first pass: '$first'"
  kill -0 $dl 2>/dev/null && pass "download still running after the daemon restart" || fail "download ended during the daemon restart"
  wait $dl
  expect "download across the daemon restart (bytes code)" "$(cat "$dir/r")" "60000000 200"
  expect "core epoch (InvocationID) across the daemon restart" "$(unit_prop $CORE_UNIT InvocationID)" "$e0"
  expect "core PID across the daemon restart" "$(unit_prop $CORE_UNIT MainPID)" "$p0"
  expect "pxa1 carrier changes across the daemon restart" "$(carrier pxa1)" "$c0"
  daemon_stop
  expect "core after the daemon stops" "$(systemctl is-active $CORE_UNIT)" active
  expect "pxa1 after the daemon stops" "$(link_up pxa1)" up
  rm -rf "$dir"
}

step_idle() {
  daemon_start >/dev/null
  local pid hz t0 t1; pid=$(unit_prop $DAEMON MainPID); hz=$(getconf CLK_TCK)
  local passes0; passes0=$(journalctl -u $DAEMON -o cat | grep -c ' pass ')
  t0=$(awk '{print $14 + $15 + $16 + $17}' /proc/$pid/stat)
  sleep 20
  t1=$(awk '{print $14 + $15 + $16 + $17}' /proc/$pid/stat)
  local passes; passes=$(( $(journalctl -u $DAEMON -o cat | grep -c ' pass ') - passes0 ))
  daemon_stop
  [ "$passes" -gt 0 ] && pass "$passes idle passes in 20 s, $(( (t1 - t0) * 1000 / hz )) ms CPU in total with children ($(( (t1 - t0) * 1000 / hz / passes )) ms per pass, VM on Apple M4)" ||
    fail "no passes while idle"
}

# step_churn sends plans that change only probe roots: eight adds, 3 s apart, then
# a drop. Each add changes the core's rule set and so rewrites routing, as does the
# end of the drop's drain 30 s later, and no slot's gate or flow may notice. The
# drop itself leaves the rules alone, since a draining root keeps its rule.
step_churn() {
  local dir since j down1=0 down2=0 samples=0 out held=""
  out=$("$RL" once 2>&1) || { fail "install did not converge"; echo "$out" | indent; }
  dir=$(mktemp -d); since=$(date '+%F %T')
  $LAN curl -s -m 90 --limit-rate 2M -o /dev/null -w '%{size_download} %{http_code}' "http://$SLOT1/bytes?n=150000000" >"$dir/r" &
  local dl=$!
  systemd-run --unit=$DAEMON --collect --quiet "$RL" churn
  until journalctl -u $DAEMON --since "$since" -o cat | grep -q 'churn done'; do
    [ "$(link_up pxa1)" = down ] && down1=$((down1 + 1))
    [ "$(link_up pxa2)" = down ] && down2=$((down2 + 1))
    samples=$((samples + 1))
    # Plan 10 comes 3 s after the eighth add and removes the rules at once, so the core is read between.
    [ -z "$held" ] && [ "$(journalctl -u $DAEMON --since "$since" -o cat | grep -c 'core add probe rule ')" -ge 8 ] &&
      held=$(/usr/local/bin/xray api lsrules --server=$API | jq -r '.rules[].ruleTag')
    sleep 0.25
  done
  j=$(journalctl -u $DAEMON --since "$since" -o cat)
  grep -E 'plan [0-9]+:|core (replace routing|add probe rule|remove probe rule)|link pxa|held' <<<"$j" | tail -40 | indent
  # A probe-route change adds or removes that route's own rule and never rewrites slot routing. Each
  # plan of one more root adds one rule, so the adds after each plan line are counted per plan.
  expect "routing rewrites across eight adds and a drain's end" "$(grep -c 'core replace routing' <<<"$j")" 0
  expect "probe rules added after each of the eight plans" "$(awk '
    / plan [0-9]+: [0-9]+ extra probe roots/ { if (n) printf "%d ", adds; n++; adds = 0; next }
    /core add probe rule / { adds++ }
    END { if (n) printf "%d", adds }' <<<"$j")" "1 1 1 1 1 1 1 1 0"
  expect "distinct probe rules added" "$(grep -o 'core add probe rule .*' <<<"$j" | sort -u | wc -l)" 8
  expect "probe rules removed by the drain, each one added" "$(comm -12 <(grep -o 'core add probe rule .*' <<<"$j" | sed 's/ add / /' | sort -u) \
    <(grep -o 'core remove probe rule .*' <<<"$j" | sed 's/ remove / /' | sort -u) | wc -l)" 8
  expect "added probe rules the core held after the eighth add" "$(comm -12 <(grep -o 'core add probe rule .*' <<<"$j" | sed 's/core add probe rule //' | sort -u) \
    <(sort -u <<<"$held") | wc -l)" 8
  expect "pxa1 down samples of $samples through the churn" "$down1" 0
  expect "pxa2 down samples of $samples through the churn" "$down2" 0
  expect "rewrites held through the churn" "$(grep -c 'routing rewrite held' <<<"$j")" 0
  expect "gate changes through the churn" "$(grep -c '  link pxa' <<<"$j")" 0
  kill -0 $dl 2>/dev/null && pass "download still running when the churn ended" || fail "download ended before the churn did"
  wait $dl
  expect "download across the churn (bytes code)" "$(cat "$dir/r")" "150000000 200"
  daemon_stop
  rm -rf "$dir"
  out=$("$RL" once 2>&1) || { fail "reconcile after the churn"; echo "$out" | indent; }
}

# route_watch samples pxa1's gate against slot1's override every quarter second
# until route_stop; route_table prints the counts of each pair.
route_watch() {
  local dir=$1 a; a=$(tag_of srvA)
  ( exec >"$dir/samples"
    while :; do
      echo "$(link_up pxa1)+$([ "$(override_of b.slot1)" = "$a" ] && echo planned || echo wrong)"
      sleep 0.25
    done ) &
  ROUTE_WATCH=$!
}
route_stop() { kill "$ROUTE_WATCH" 2>/dev/null; wait "$ROUTE_WATCH" 2>/dev/null; }
route_table() { sort "$1/samples" | uniq -c | awk '{printf "%s %s; ", $2, $1}'; }
route_count() { grep -c "^$2\$" "$1/samples"; }

# step_flapping re-points slot1's override to srvB, one of its candidates, every
# half second for 40 s under the long-running runtime, which polls every 2 s. Each
# failed check closes the gate and repairs the override, and asks for a routing
# rewrite that backs off toward a minute. A writer faster than the poll keeps the
# gate closed, whatever the backoff says, and slot2 never notices.
step_flapping() {
  daemon_start >/dev/null
  local b since end n held dir c2
  b=$(tag_of srvB); since=$(date '+%F %T'); end=$(( $(date +%s) + 40 )); dir=$(mktemp -d); c2=$(carrier pxa2)
  route_watch "$dir"
  while [ "$(date +%s)" -lt $end ]; do
    /usr/local/bin/xray api bo --server=$API -b b.slot1 "$b" >/dev/null
    sleep 0.5
  done
  route_stop
  note "measured, slot1's gate and override, a sample every 0.25 s: $(route_table "$dir")"
  n=$(journalctl -u $DAEMON --since "$since" -o cat | grep -c 'core replace routing')
  held=$(journalctl -u $DAEMON --since "$since" -o cat | grep -o 'routing rewrite held until .*' | tail -1)
  note "last hold: ${held:-none}"
  local wrong; wrong=$(route_count "$dir" up+wrong)
  [ "$wrong" -le 16 ] && pass "$wrong samples of pxa1 up on the wrong route in 40 s, of about 150" ||
    fail "$wrong samples of pxa1 up on the wrong route in 40 s, of about 150, want at most 16"
  [ "$n" -le 8 ] && pass "$n routing rewrites in 40 s of slot1's check failing every half second" ||
    fail "$n routing rewrites in 40 s of slot1's check failing every half second, want at most 8"
  expect "pxa2 carrier changes while slot1 flapped" "$(carrier pxa2)" "$c2"
  wait_exit $SRV_B 203.0.113.82
  local start got=""; start=$(date +%s)
  while [ $(( $(date +%s) - start )) -lt 15 ]; do
    got=$(exit_ip $SLOT1 2) && [ "$got" = $SRV_A ] && break
    sleep 0.5
  done
  expect "slot1 exit $(( $(date +%s) - start )) s after the flapping stopped" "$got" $SRV_A
  daemon_stop
  rm -rf "$dir"
}

# rewrite_blocked: slot1's override drifts to srvB while the pinned binary's
# adrules, set up by the caller, cannot land. The override must be repaired anyway,
# and no gate may stay up on the wrong route once a pass has run.
rewrite_blocked() {
  local since dir; since=$1; dir=$(mktemp -d)
  /usr/local/bin/xray api bo --server=$API -b b.slot1 "$(tag_of srvB)" >/dev/null
  sleep "$2"
  route_watch "$dir"
  sleep 12
  route_stop
  note "measured, slot1's gate and override, a sample every 0.25 s, after the first pass: $(route_table "$dir")"
  expect "$3: samples of pxa1 up on the wrong route" "$(route_count "$dir" up+wrong)" 0
  expect "$3: slot1 exit" "$(exit_ip $SLOT1 3)" $SRV_A
  [ "$(journalctl -u $DAEMON --since "$since" -o cat | grep -c 'api adrules')" -gt 0 ] &&
    pass "$3: the failed rewrite is reported" || fail "$3: the failed rewrite went unreported"
  rm -rf "$dir"
}

# step_noland makes every rewrite take longer than a tool may run. step_nobinary
# leaves the pinned binary's path dangling, so none can run at all.
step_noland() {
  local S since X=$LAB_HOME/current/xray out
  S=$(mktemp -d)
  cat >"$S/xray" <<SHIM
#!/bin/bash
[[ "\$*" == api\ adrules* ]] && sleep 11
exec /usr/local/bin/xray "\$@"
SHIM
  chmod +x "$S/xray"
  daemon_start >/dev/null
  since=$(date '+%F %T')
  ln -sfn "$S/xray" "$X"
  rewrite_blocked "$since" 14 "rewrite killed at its tool bound"
  ln -sfn /usr/local/bin/xray "$X"
  local t0; t0=$(date +%s)
  until journalctl -u $DAEMON --since "$since" -o cat | grep -q 'core replace routing' || [ $(( $(date +%s) - t0 )) -gt 40 ]; do sleep 0.5; done
  journalctl -u $DAEMON --since "$since" -o cat | grep -q 'core replace routing' &&
    pass "the owed rewrite landed $(( $(date +%s) - t0 )) s after adrules worked again" || fail "the owed rewrite never landed"
  daemon_stop
  rm -rf "$S"
  out=$("$RL" once 2>&1) || { fail "reconcile after noland"; echo "$out" | indent; }
}

step_nobinary() {
  local since X=$LAB_HOME/current/xray out
  daemon_start >/dev/null
  since=$(date '+%F %T')
  ln -sfn /nonexistent/xray "$X"
  rewrite_blocked "$since" 5 "pinned binary gone"
  ln -sfn /usr/local/bin/xray "$X"
  daemon_stop
  out=$("$RL" once 2>&1) || { fail "reconcile after nobinary"; echo "$out" | indent; }
}

# held_count prints how many passes since $1 reported a held rewrite.
held_count() { journalctl -u $DAEMON --since "$1" -o cat | grep -c 'routing rewrite held'; }

# await_link waits up to $3 s for link $1 to read $2 and prints the ms it took, or nothing.
await_link() {
  local t0; t0=$(date +%s%3N)
  while [ $(( $(date +%s%3N) - t0 )) -lt $(( $3 * 1000 )) ]; do
    [ "$(link_up "$1")" = "$2" ] && { echo $(( $(date +%s%3N) - t0 )); return; }
    sleep 0.1
  done
}

# step_blips repeats a one-pass foreign rule that closes the gates while the core
# keeps passing its check. No rewrite failed, so nothing may be held.
step_blips() {
  local since i ms worst=0 out
  out=$("$RL" once 2>&1) || { fail "install did not converge"; echo "$out" | indent; }
  since=$(date '+%F %T')
  systemd-run --unit=$DAEMON --collect --quiet "$RL" run
  sleep 3
  ip link set pxa1 down
  await_link pxa1 up 10 >/dev/null
  for i in 1 2 3 4 5 6; do
    sleep 6
    ip rule add pref 100 lookup 52
    await_link pxa1 down 8 >/dev/null
    ip rule del pref 100 lookup 52
    ms=$(await_link pxa1 up 80)
    note "round $i: gate back ${ms:-never} ms after the foreign rule left"
    [ "${ms:-999999}" -gt "$worst" ] && worst=${ms:-999999}
  done
  [ "$worst" -lt 5000 ] && pass "gate back within $worst ms of the foreign rule leaving, in every round" ||
    fail "gate back after $worst ms in the slowest round, want under 5000"
  expect "passes that held a rewrite through six blips" "$(held_count "$since")" 0
  daemon_stop
}

# step_gatelock holds the gate lock past a Worker's 10 s give-up while both gates
# are closed. A link up that gives up is not a failed check.
step_gatelock() {
  local since p ms
  since=$(date '+%F %T')
  systemd-run --unit=$DAEMON --collect --quiet "$RL" run
  sleep 3
  ip link set pxa1 down; ip link set pxa2 down
  flock $RUN_DIR/gates.lock sleep 12 & p=$!
  note "gates closed by hand, gate lock held 12 s"
  ms=$(await_link pxa1 up 40)
  [ -n "$ms" ] && pass "pxa1 up $ms ms after the lock was taken" || fail "pxa1 not up 40 s after the lock was taken"
  await_link pxa2 up 20 >/dev/null
  expect "pxa2 after the lock was freed" "$(link_up pxa2)" up
  wait $p
  expect "passes that held a rewrite after a lock give-up" "$(held_count "$since")" 0
  daemon_stop
}

# step_apistall freezes the core for 12 s, longer than its API's answers. Nothing
# failed a check, since nothing could be read, and that is a blip the gates ride
# through. Frozen past the read bound, they close.
step_apistall() {
  local since downs=0 end ms
  since=$(date '+%F %T')
  systemd-run --unit=$DAEMON --collect --quiet "$RL" run
  sleep 3
  ip link set pxa1 down
  await_link pxa1 up 10 >/dev/null
  sleep 3
  note "a rewrite landed and passed its check, so a failed check now would spend it"
  systemctl kill -s STOP $CORE_UNIT
  end=$(( $(date +%s) + 12 ))
  while [ "$(date +%s)" -lt "$end" ]; do
    [ "$(link_up pxa1)" = down ] && downs=$((downs + 1))
    sleep 0.25
  done
  systemctl kill -s CONT $CORE_UNIT
  note "core frozen 12 s, then thawed"
  expect "samples of pxa1 down through a 12 s freeze" "$downs" 0
  await_link pxa1 up 10 >/dev/null
  await_link pxa2 up 10 >/dev/null
  sleep 3
  expect "pxa1 3 s after the thaw" "$(link_up pxa1)" up
  expect "pxa2 3 s after the thaw" "$(link_up pxa2)" up
  expect "passes that held a rewrite after an API stall" "$(held_count "$since")" 0
  wait_exit $SRV_A
  systemctl kill -s STOP $CORE_UNIT
  ms=$(await_link pxa1 down 60)
  systemctl kill -s CONT $CORE_UNIT
  [ -n "$ms" ] && [ "$ms" -ge 20000 ] && pass "core frozen for good: gates closed $ms ms in" ||
    fail "core frozen for good: gates closed '${ms:-never}' ms in, want between 20 and 60 s"
  ms=$(await_link pxa1 up 15)
  [ -n "$ms" ] && pass "pxa1 up $ms ms after the thaw" || fail "pxa1 still down 15 s after the thaw"
  wait_exit $SRV_A
  daemon_stop
}

# step_slowread makes iptables-save outlast its tool bound on every pass, and drifts
# slot1's override. The Worker must still judge the gate from the core it can read,
# and past the read bound close every gate, since plumbing can't be proven.
step_slowread() {
  local S since dir ms out
  out=$("$RL" once 2>&1) || { fail "install did not converge"; echo "$out" | indent; }
  S=$(mktemp -d); dir=$(mktemp -d)
  cat >"$S/iptables-save" <<SHIM
#!/bin/bash
[ -e $S/slow ] && sleep 11
exec /sbin/iptables-save "\$@"
SHIM
  chmod +x "$S/iptables-save"
  since=$(date '+%F %T')
  systemd-run --unit=$DAEMON --collect --quiet --setenv=PATH="$S:/usr/sbin:/usr/bin:/sbin:/bin" "$RL" run
  sleep 3
  touch "$S/slow"
  local t0; t0=$(date +%s%3N)
  /usr/local/bin/xray api bo --server=$API -b b.slot1 "$(tag_of srvB)" >/dev/null
  note "iptables-save now takes 11 s; slot1's override drifted to srvB"
  sleep 14
  expect "14 s in: pxa2, one failed read being no blip's end" "$(link_up pxa2)" up
  route_watch "$dir"
  ms=$(await_link pxa2 down 60)
  route_stop
  note "measured, slot1's gate and override, a sample every 0.25 s, from 14 s in: $(route_table "$dir")"
  expect "samples of pxa1 up on the wrong route" "$(route_count "$dir" up+wrong)" 0
  [ -n "$ms" ] && pass "pxa2 closed $(( 14000 + ms )) ms after reads stopped" || fail "pxa2 still up 74 s after reads stopped"
  expect "pxa1 past the bound" "$(link_up pxa1)" down
  local t1; t1=$(date +%s)
  until journalctl -u $DAEMON --since "$since" -o cat | grep -q 'every gate closed: state unread' || [ $(( $(date +%s) - t1 )) -gt 30 ]; do sleep 0.5; done
  journalctl -u $DAEMON --since "$since" -o cat | grep -q 'every gate closed: state unread' &&
    pass "the closure says why, by the end of its pass" || fail "the closure went unexplained"
  rm "$S/slow"
  ms=$(await_link pxa1 up 30); await_link pxa2 up 10 >/dev/null
  [ -n "$ms" ] && pass "pxa1 up $ms ms after reads recovered" || fail "pxa1 still down 30 s after reads recovered"
  expect "pxa2 after reads recovered" "$(link_up pxa2)" up
  wait_exit $SRV_A
  daemon_stop
  rm -rf "$S" "$dir"
}

# refused_up succeeds when the log on stdin reports link $1's up refused because its port is held,
# whatever case the writer gives the errno's words.
refused_up() { grep -qi "link $1 up:.*address already in use"; }

# step_portheld: a foreign socket holds slot1's listen port, so the kernel refuses
# pxa1's link up. slot2 serves regardless, and slot1 opens once the port is free.
step_portheld() {
  local since p ms out n
  out=$("$RL" once 2>&1) || { fail "install did not converge"; echo "$out" | indent; }
  ip link set pxa1 down; ip link set pxa2 down
  python3 -c 'import socket,time; s=socket.socket(socket.AF_INET6,socket.SOCK_DGRAM); s.setsockopt(socket.IPPROTO_IPV6,socket.IPV6_V6ONLY,0); s.bind(("::",51831)); time.sleep(300)' & p=$!
  sleep 0.3
  note "both gates closed, UDP 51831 held by a foreign socket"
  since=$(date '+%F %T')
  systemd-run --unit=$DAEMON --collect --quiet "$RL" run
  ms=$(await_link pxa2 up 10)
  [ -n "$ms" ] && pass "pxa2 up after $ms ms beside pxa1's refused link up" || fail "pxa2 still down after 10 s"
  wait_exit $SRV_B 203.0.113.82
  sleep 12
  expect "pxa1 while its port is held" "$(link_up pxa1)" down
  expect "pxa2 12 s on" "$(link_up pxa2)" up
  journalctl -u $DAEMON --since "$since" -o cat | refused_up pxa1 &&
    pass "pxa1's refusal is reported" || fail "pxa1's refusal went unreported"
  n=$(journalctl -u $DAEMON --since "$since" -o cat | grep -c 'core replace routing')
  [ "$n" -le 2 ] && pass "$n routing rewrites while one link up kept failing" || fail "$n routing rewrites while one link up kept failing, want at most 2"
  expect "passes that held a rewrite while a link up kept failing" "$(held_count "$since")" 0
  kill $p; wait $p 2>/dev/null
  ms=$(await_link pxa1 up 10)
  [ -n "$ms" ] && pass "pxa1 up $ms ms after the port was freed" || fail "pxa1 still down 10 s after the port was freed"
  wait_exit $SRV_A
  daemon_stop
}

# step_heldadd is a hot add of slot2 while another process holds its UDP port. Xray lists slot2's UDP
# tag though its bind failed, and the daemon removes that tag in the same pass, so the core lists only
# what listens: pxa2 stays down, slot2's TCP listener stays, and pxa1 serves throughout. Each pass adds
# the UDP inbound again and removes it when the bind fails, so the port binds on the first pass after
# it is freed.
step_heldadd() {
  local p since t0 core n up1=up opened="" ms out j r listed=0 reads=0 tcp
  daemon_stop
  out=$("$RL" teardown 2>&1) || { fail "teardown"; echo "$out" | indent; }
  out=$("$RL" once --slots slot1 2>&1) || { fail "slot1 alone did not converge"; echo "$out" | indent; }
  expect "pxa1 with slot1 alone" "$(link_up pxa1)" up
  expect "pxa2 with slot1 alone" "$(link_up pxa2)" down
  python3 -c 'import socket,time; s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.bind(("127.0.0.1",12342)); time.sleep(600)' & p=$!
  sleep 0.3
  note "UDP 127.0.0.1:12342 held by a plain socket, pid $p"
  since=$(date '+%F %T'); t0=$(date +%s)
  systemd-run --unit=$DAEMON --collect --quiet "$RL" run
  core=$(unit_prop $CORE_UNIT MainPID)
  for _ in $(seq 100); do ss -Hltnp 'sport = :12342' | grep -q "pid=$core," && break; sleep 0.2; done
  ss -Hltnp 'sport = :12342' | grep -q "pid=$core," && pass "slot2's TCP listener is the core's, pid $core" || fail "the core holds no TCP listener on 12342"
  ss -Hlunp 'sport = :12342' | grep -q "pid=$core," && fail "the core holds UDP 12342 beside the holder" || pass "UDP 12342 is the holder's alone"
  tcp=$(ss -Hltne 'sport = :12342' | grep -o 'ino:[0-9]*')
  for _ in $(seq 40); do
    [ "$(link_up pxa2)" = up ] && opened=1
    [ "$(link_up pxa1)" = up ] || up1=down
    if out=$(/usr/local/bin/xray api lsi --server=$API 2>/dev/null); then
      reads=$((reads + 1))
      grep -q '"s.slot2.udp"' <<<"$out" && listed=$((listed + 1))
    fi
    sleep 0.5
  done
  expect "list reads of 40 that failed" "$((40 - reads))" 0
  [ "$listed" -le 8 ] && pass "s.slot2.udp listed in $listed of $reads list reads, at most 8 for the instants inside an add" || fail "s.slot2.udp listed in $listed of $reads list reads with nothing bound"
  expect "slot2's TCP listener after 20 s" "$(ss -Hltne 'sport = :12342' | grep -o 'ino:[0-9]*')" "$tcp"
  [ -z "$opened" ] && pass "pxa2 stayed down for 20 s while its UDP port was held" || fail "pxa2 opened with nothing of the core's on its UDP port"
  expect "pxa1 throughout" "$up1" up
  expect "slot1's exit" "$(exit_ip $SLOT1 4)" $SRV_A
  j=$(journalctl -u $DAEMON --since "$since" -o cat)
  n=$(grep -c 'listen udp 127.0.0.1:12342: bind: address already in use' <<<"$j")
  [ "$n" -ge 1 ] && pass "the bind error is in the journal, $n passes in $(( $(date +%s) - t0 )) s" || fail "the bind error went unreported"
  expect "removals of any inbound by a pass step" "$(grep -c 'core remove inbound' <<<"$j")" 0
  kill $p; wait $p 2>/dev/null
  ms=$(await_link pxa2 up 25)
  [ -n "$ms" ] && pass "pxa2 up $ms ms after the port was freed" || fail "pxa2 still down 25 s after the port was freed"
  ss -Hlunp 'sport = :12342' | grep -q "pid=$core," && pass "UDP 12342 is the core's" || fail "the core holds no UDP socket on 12342"
  expect "slot2's TCP listener once UDP bound" "$(ss -Hltne 'sport = :12342' | grep -o 'ino:[0-9]*')" "$tcp"
  wait_exit $SRV_B 203.0.113.82
  r=""; for _ in $(seq 10); do r=$(udp_exit 203.0.113.82) && break; sleep 1; done
  expect "UDP through slot2" "$r" $SRV_B
  daemon_stop
}

# step_outage keeps the core from starting for 35 s, with no flapping check: its
# unit fails before it runs, so it never reads as active. No rewrite can land
# meanwhile, so none may count against the slot.
step_outage() {
  daemon_stop
  local S D=/etc/systemd/system/$CORE_UNIT.d since healed up="" j out
  out=$("$RL" once 2>&1) || { fail "install did not converge"; echo "$out" | indent; }
  S=$(mktemp -d)
  mkdir -p "$D"
  printf '[Service]\nExecStartPre=/bin/sh -c "! test -e %s/broken"\n' "$S" >"$D/outage.conf"
  systemctl daemon-reload
  since=$(date '+%F %T')
  systemd-run --unit=$DAEMON --collect --quiet "$RL" run
  sleep 3
  touch "$S/broken"
  systemctl kill -s KILL $CORE_UNIT
  sleep 35
  note "after 35 s of a core that cannot start: core $(systemctl is-active $CORE_UNIT), pxa1 $(link_up pxa1)"
  expect "pxa1 with the core out" "$(link_up pxa1)" down
  rm "$S/broken"; healed=$(date +%s%3N)
  while [ $(( $(date +%s%3N) - healed )) -lt 90000 ]; do
    [ "$(link_up pxa1)" = up ] && { up=$(( $(date +%s%3N) - healed )); break; }
    sleep 0.2
  done
  j=$(journalctl -u $DAEMON --since "$since" -o cat)
  grep -E 'error: ' <<<"$j" | cut -c1-120 | sort | uniq -c | indent
  [ -n "$up" ] && [ "$up" -lt 15000 ] && pass "pxa1 up $up ms after the core could start again" || fail "pxa1 up '${up:-never}' ms after the core could start again, want under 15000"
  wait_exit $SRV_A
  daemon_stop
  rm -rf "$S" "$D"; systemctl daemon-reload
  out=$("$RL" once 2>&1) || { fail "reconcile after the outage"; echo "$out" | indent; }
}

step_corestop() {
  daemon_stop
  local dir; dir=$(mktemp -d)
  systemctl stop $CORE_UNIT
  expect "core stopped: pxa1" "$(link_up pxa1)" down
  expect "core stopped: pxa2" "$(link_up pxa2)" down
  leak_watch "$dir"
  if exit_ip $SLOT1 4 >/dev/null; then fail "slot1 answers with the core stopped"; else pass "core stopped: slot1 traffic stops"; fi
  expect "core stopped: packets to $SLOT1 leaving eth4 or $UPLINK" "$(leak_count "$dir")" 0

  local log; log=$(daemon_start); echo "$log" | indent
  local s o u
  s=$(grep -n 'core start' <<<"$log" | head -1 | cut -d: -f1)
  o=$(grep -n 'core override b.slot1' <<<"$log" | head -1 | cut -d: -f1)
  u=$(grep -n 'link pxa1 up' <<<"$log" | head -1 | cut -d: -f1)
  [ -n "$s" ] && [ -n "$o" ] && [ -n "$u" ] && [ "$s" -lt "$o" ] && [ "$o" -lt "$u" ] &&
    pass "daemon started the core (line $s), overrode slot1 (line $o), then opened pxa1 (line $u)" || fail "gate order in: start=$s override=$o up=$u"
  wait_exit $SRV_A
  daemon_stop

  local e0; e0=$(unit_prop $CORE_UNIT InvocationID)
  systemctl kill -s KILL $CORE_UNIT
  local e1=""
  for _ in $(seq 100); do
    e1=$(unit_prop $CORE_UNIT InvocationID)
    [ "$(systemctl is-active $CORE_UNIT)" = active ] && [ "$e1" != "$e0" ] && break
    sleep 0.1
  done
  [ "$e1" != "$e0" ] && pass "systemd restarted the killed core (new epoch)" || fail "core did not come back after SIGKILL"
  expect "core crashed: pxa1 closed by ExecStopPost" "$(link_up pxa1)" down
  local bi; bi=$(/usr/local/bin/xray api bi --server=127.0.0.1:10085 b.slot1)
  grep -q Selects <<<"$bi" && expect "restarted core: slot1 override" "$(awk '/Override/ {getline; print $2}' <<<"$bi")" "" ||
    fail "balancer readback failed: $bi"
  log=$(daemon_start); echo "$log" | indent
  o=$(grep -n 'core override b.slot1' <<<"$log" | head -1 | cut -d: -f1)
  u=$(grep -n 'link pxa1 up' <<<"$log" | head -1 | cut -d: -f1)
  grep -q 'core start' <<<"$log" && fail "daemon started a core systemd already restarted" || pass "daemon adopted the restarted core"
  [ -n "$o" ] && [ -n "$u" ] && [ "$o" -lt "$u" ] && pass "override re-set (line $o) before pxa1 reopened (line $u)" || fail "gate order: override=$o up=$u"
  wait_exit $SRV_A
  daemon_stop
  rm -rf "$dir"
}

# await_core_restart waits until systemd has restarted the core into a new epoch.
await_core_restart() {
  local e0=$1
  for _ in $(seq 100); do
    [ "$(systemctl is-active $CORE_UNIT)" = active ] && [ "$(unit_prop $CORE_UNIT InvocationID)" != "$e0" ] && return 0
    sleep 0.1
  done
  return 1
}

await_core_restart_within() {
  local t0; t0=$(date +%s)
  while [ $(( $(date +%s) - t0 )) -lt "$1" ]; do
    [ "$(systemctl is-active $CORE_UNIT)" = active ] && [ "$(unit_prop $CORE_UNIT InvocationID)" != "$2" ] && return 0
    sleep 0.2
  done
  return 1
}

# reopen_slot1 lets the runtime reopen pxa1 and checks it set the override first. The core may still
# be starting, so the runner polls through that rather than end on its first refused read.
reopen_slot1() {
  local out o u; out=$("$RL" once --through-restart 2>&1); echo "$out" | indent
  o=$(grep -n 'core override b.slot1' <<<"$out" | head -1 | cut -d: -f1)
  u=$(grep -n 'link pxa1 up' <<<"$out" | head -1 | cut -d: -f1)
  [ -n "$o" ] && [ -n "$u" ] && [ "$o" -lt "$u" ] && pass "$1: override re-set (line $o) before pxa1 reopened (line $u)" ||
    fail "$1: gate order: override=$o up=$u"
  wait_exit $SRV_A
}

# read_down succeeds only when a read of the link's flags succeeds and shows it down.
read_down() { local f; f=$(cat "/sys/class/net/$1/flags" 2>/dev/null) && [ $(( f & 1 )) -eq 0 ]; }

# coredeath_case holds pxa1's up inside its write, after the runtime confirmed the core and with the
# gate lock held, and kills the core. The stop hook's first sweep closes pxa1, still down, and then
# pxa2, so the write is released only once pxa2 reads down: the up lands after that sweep, as the
# link's read back inside the write shows, and only the hook's second close, once it has the gate
# lock, can close pxa1 again.
coredeath_case() {
  local G=$1 what=$2 e0 rl rc
  rm -f "$G/held" "$G/release" "$G/written"
  expect "$what: pxa2 before the kill" "$(link_up pxa2)" up
  ip link set pxa1 down
  echo pxa1 >"$G/hold"
  e0=$(unit_prop $CORE_UNIT InvocationID)
  "$RL" once >"$G/once.log" 2>&1 & rl=$!
  for _ in $(seq 500); do [ -e "$G/held" ] && break; sleep 0.02; done
  [ -e "$G/held" ] && pass "$what: pxa1's up held in its write, after the confirmation" || fail "$what: the runtime never reached pxa1's up"
  systemctl kill -s KILL $CORE_UNIT
  for _ in $(seq 250); do read_down pxa2 && break; sleep 0.02; done
  read_down pxa2 && pass "$what: the hook's first sweep closed pxa2, after pxa1" || fail "$what: pxa2 never read down after the kill"
  touch "$G/release"
  wait $rl; rc=$?
  indent <"$G/once.log"
  expect "$what: pxa1 read after its up, released after that sweep" "$(cat "$G/written" 2>/dev/null)" up
  [ -e "$G/kill" ] && expect "$what: runtime exit status" "$rc" 137
  await_core_restart "$e0" && pass "$what: systemd restarted the core on a new epoch" || fail "$what: the core did not come back"
  read_down pxa1 && pass "$what: pxa1 down once the hook has finished" || fail "$what: pxa1 not read down once the hook has finished"
  reopen_slot1 "$what"
}

# step_coredeath kills the core while the runtime sets pxa1 up, after it verified the core. The stop
# hook must close the gate after that link up, even when the runtime dies right after it too. It also
# times each up the runtime writes on the VM's WireGuard links.
step_coredeath() {
  daemon_stop
  local G; G=$(mktemp -d)
  export SIDEGATE_LAB_GATES=$G
  coredeath_case "$G" "core killed as pxa1 opened"
  touch "$G/kill"
  coredeath_case "$G" "core and runtime killed as pxa1 opened"
  unset SIDEGATE_LAB_GATES
  if [ -s "$G/ups" ]; then
    note "each link up by ioctl, in µs: $(awk '{print $2}' "$G/ups" | sort -n | tr '\n' ' ')"
    expect "up writes by ioctl that failed" "$(awk '$3 != "ok"' "$G/ups" | wc -l)" 0
  else
    fail "no link up was timed"
  fi
  rm -rf "$G"
}

override_of() { /usr/local/bin/xray api bi --server=$API "$1" | awk '/Override/ {getline; print $2}'; }

# slot1_serves_srvA checks pxa1, slot1's override and its exit, as the runtime
# that converged last planned them.
slot1_serves_srvA() {
  expect "$1: pxa1" "$(link_up pxa1)" up
  expect "$1: slot1 override" "$(override_of b.slot1)" "$(tag_of srvA)"
  expect "$1: slot1 exit" "$(exit_ip $SLOT1 3)" $SRV_A
}

# step_orphan kills a runtime while its `xray api adrules` child for a switch
# to srvB is still running, then runs the next runtime with slot1 on srvA. The
# orphan must land before that runtime observes, never after it converged.
step_orphan() {
  local S X=$LAB_HOME/current/xray out rc p t0
  S=$(mktemp -d)
  cat >"$S/xray" <<SHIM
#!/bin/bash
if [ "\$2" = adrules ] && [ -e $S/slow ]; then
  rm $S/slow; touch $S/inflight; sleep 2
  /usr/local/bin/xray "\$@"; rc=\$?; touch $S/landed; exit \$rc
fi
exec /usr/local/bin/xray "\$@"
SHIM
  chmod +x "$S/xray"; ln -sfn "$S/xray" "$X"
  touch "$S/slow"
  "$RL" once --select slot1=srvB >/dev/null 2>&1 & p=$!
  for _ in $(seq 300); do [ -e "$S/inflight" ] && break; sleep 0.01; done
  kill -9 $p; wait $p 2>/dev/null
  ln -sfn /usr/local/bin/xray "$X"
  note "runtime switching slot1 to srvB killed while its adrules child runs: $(pgrep -fc 'xray api adrules') orphan(s)"
  t0=$(date +%s.%N)
  out=$("$RL" once 2>&1); rc=$?
  note "next runtime, slot1 on srvA, finished after $(echo "$(date +%s.%N) - $t0" | bc | cut -c1-4) s: $(grep -E 'pass [0-9]+:' <<<"$out" | tail -1)"
  expect "switch orphan: next runtime exit status" "$rc" 0
  [ -e "$S/landed" ] && pass "switch orphan: the orphan landed before the next runtime converged" ||
    fail "switch orphan: the next runtime converged while the orphan was still running"
  slot1_serves_srvA "switch orphan, next runtime converged"
  sleep 2.5
  slot1_serves_srvA "switch orphan, 2.5 s later"
  rm -rf "$S"

  S=$(mktemp -d)
  cat >"$S/xray" <<SHIM
#!/bin/bash
if [ "\$1 \$2" = "api adrules" ]; then
  cat >$S/payload.json; touch $S/paused
  while [ ! -e $S/release ]; do sleep 0.02; done
  /usr/local/bin/xray "\$@" <$S/payload.json; rc=\$?; touch $S/landed; exit \$rc
fi
exec /usr/local/bin/xray "\$@"
SHIM
  printf '#!/bin/bash\n/sbin/iptables-restore "$@"; rc=$?\nln -sfn %s/xray %s\nexit $rc\n' "$S" "$X" >"$S/iptables-restore"
  chmod +x "$S/xray" "$S/iptables-restore"
  iptables -t filter -F sidegate-output
  note "sidegate-output flushed, so the switching runtime repairs plumbing first, then pauses in adrules"
  PATH=$S:$PATH "$RL" once --select slot1=srvB >/dev/null 2>&1 & p=$!
  for _ in $(seq 300); do [ -e "$S/paused" ] && break; sleep 0.02; done
  [ -e "$S/paused" ] && pass "repair orphan: the switching runtime is paused in its adrules child" || fail "repair orphan: no adrules child paused"
  kill -9 $p; wait $p 2>/dev/null
  ln -sfn /usr/local/bin/xray "$X"
  (sleep 2; touch "$S/release") &
  out=$("$RL" once 2>&1); rc=$?
  note "next runtime, slot1 on srvA: $(grep -E 'pass [0-9]+:' <<<"$out" | tail -1)"
  expect "repair orphan: next runtime exit status" "$rc" 0
  [ -e "$S/landed" ] && pass "repair orphan: the orphan landed before the next runtime converged" ||
    fail "repair orphan: the next runtime converged while the orphan was still paused"
  wait
  for _ in $(seq 100); do [ -e "$S/landed" ] && break; sleep 0.05; done
  slot1_serves_srvA "repair orphan, once the orphan is gone"
  expect "runtime lock holders left" "$(fuser $RUN_DIR/runtime.lock 2>/dev/null | wc -w)" 0
  rm -rf "$S"

  flock $RUN_DIR/runtime.lock sleep 12 & p=$!
  sleep 0.2
  out=$("$RL" once 2>&1); rc=$?
  [ "$rc" -ne 0 ] && grep -q "waiting for $RUN_DIR/runtime.lock" <<<"$out" &&
    pass "a holder that outlasts a tool's bound is reported, not waited on in silence" ||
    fail "a stalled runtime lock went unreported (exit status $rc)"
  expect "stalled lock: pxa1 left as it was" "$(link_up pxa1)" up
  wait $p
  slot1_serves_srvA "holder gone"
}

# wedged_tool writes a shim for tool on $1 whose call matching $3 ignores every
# signal but KILL and never finishes, as a tool wedged on a stuck resource would.
wedged_tool() {
  local dir=$1 tool=$2 call=$3 real=$4
  cat >"$dir/$tool" <<SHIM
#!/bin/bash
if [[ "\$*" == $call ]]; then
  trap '' HUP INT TERM
  touch $dir/inflight
  while :; do sleep 1; done
fi
exec $real "\$@"
SHIM
  chmod +x "$dir/$tool"
}

# await_gates polls until both gates are up, and prints the seconds it took or nothing.
await_gates() {
  local t0; t0=$(date +%s)
  while [ $(( $(date +%s) - t0 )) -lt "$1" ]; do
    [ "$(link_up pxa1)" = up ] && [ "$(link_up pxa2)" = up ] && { echo $(( $(date +%s) - t0 )); return; }
    sleep 0.2
  done
}

# hung_cleanup ends whatever a wedged case left behind and converges again.
hung_cleanup() {
  daemon_stop
  pkill -9 -f "$1/" 2>/dev/null
  fuser -k -9 $RUN_DIR/runtime.lock $RUN_DIR/gates.lock >/dev/null 2>&1
  ln -sfn /usr/local/bin/xray $LAB_HOME/current/xray
  rm -rf "$1"
  sleep 0.5
  systemctl reset-failed $CORE_UNIT 2>/dev/null
  local out; out=$("$RL" once 2>&1) || { fail "reconcile after a wedged tool"; echo "$out" | indent; }
}

# step_hung wedges tools past every bound the runtime has. A wedged tool a killed
# runtime left behind must end by itself, so its successor converges, and a gate
# must close when the core dies whatever is wedged.
step_hung() {
  daemon_stop
  local S X=$LAB_HOME/current/xray p took e0 t0 out
  out=$("$RL" once 2>&1) || { fail "install did not converge"; echo "$out" | indent; }

  S=$(mktemp -d)
  wedged_tool "$S" xray 'api\ adrules*' /usr/local/bin/xray
  ln -sfn "$S/xray" "$X"
  ip link set pxa1 down
  "$RL" once >/dev/null 2>&1 & p=$!
  for _ in $(seq 500); do [ -e "$S/inflight" ] && break; sleep 0.02; done
  kill -9 $p; wait $p 2>/dev/null
  ln -sfn /usr/local/bin/xray "$X"
  note "runtime killed; its adrules child is wedged holding the runtime lock"
  systemd-run --unit=$DAEMON --collect --quiet "$RL" run
  took=$(await_gates 20)
  [ -n "$took" ] && pass "wedged adrules orphan: the successor converged after $took s" || fail "wedged adrules orphan: the successor still waits after 20 s"
  expect "wedged adrules orphan: left running" "$(pgrep -fc 'current/xray api adrules')" 0
  hung_cleanup "$S"

  S=$(mktemp -d)
  systemd-run --unit=$DAEMON --collect --quiet "$RL" run
  sleep 3
  flock $RUN_DIR/gates.lock sleep 50 & p=$!
  sleep 0.2
  note "something holds the gate lock past every tool's bound, and the core dies"
  e0=$(unit_prop $CORE_UNIT InvocationID); t0=$(date +%s)
  systemctl kill -s KILL $CORE_UNIT
  sleep 2
  expect "gate lock wedged, core dead 2 s: pxa1" "$(link_up pxa1)" down
  expect "gate lock wedged, core dead 2 s: pxa2" "$(link_up pxa2)" down
  await_core_restart_within 40 "$e0" && pass "gate lock wedged: close-gates gave up and the core restarted after $(( $(date +%s) - t0 )) s" ||
    fail "gate lock wedged: close-gates still blocks the core's restart after 40 s"
  kill $p 2>/dev/null; pkill -f 'sleep 50'; wait $p 2>/dev/null
  took=$(await_gates 25)
  [ -n "$took" ] && pass "gate lock freed: both gates back $took s later" || fail "gate lock freed: gates not back after 25 s: pxa1 $(link_up pxa1) pxa2 $(link_up pxa2)"
  hung_cleanup "$S"
  wait_exit $SRV_A
}

# crash_after_add deletes pxa2 and runs the runtime with an ip that kills it the
# moment `ip link add` recreates pxa2, as a kill -9 or power loss would.
crash_after_add() {
  local shim; shim=$(mktemp -d)
  printf '#!/bin/sh\n/sbin/ip "$@"; rc=$?\ncase "$*" in "link add dev pxa2 "*) pkill -9 -x runtimelab ;; esac\nexit $rc\n' >"$shim/ip"
  chmod +x "$shim/ip"
  ip link del pxa2
  { PATH=$shim:$PATH "$RL" once >/dev/null 2>&1; } 2>/dev/null
  expect "$1: runtime killed right after it created pxa2 (exit status)" "$?" 137
  rm -rf "$shim"
}

step_linkcrash() {
  crash_after_add "crash then reconcile"
  local out; out=$("$RL" once 2>&1); local rc=$?; echo "$out" | indent
  expect "crash then reconcile: reconcile exit status" "$rc" 0
  grep -q 'link pxa1 down' <<<"$out" && fail "crash then reconcile: the healthy slot1 was closed" ||
    pass "crash then reconcile: slot1 stayed open"
  expect "crash then reconcile: pxa2" "$(link_up pxa2)" up
  wait_exit $SRV_B 203.0.113.82

  crash_after_add "crash then teardown"
  out=$("$RL" teardown 2>&1); rc=$?; echo "$out" | indent
  expect "crash then teardown: teardown exit status" "$rc" 0
  expect "crash then teardown: pxa links left" "$(ip -o link show | grep -c ' pxa')" 0
  out=$("$RL" once 2>&1) && pass "reinstalled after teardown" || { fail "reinstall after teardown"; echo "$out" | indent; }
}

# step_reinstall: what an install leaves behind when /data is lost, gates up,
# built here from the product's fixed identity, which a real install is first
# checked to use. Nothing on the gateway names an install, so the reinstall must
# adopt it all, serve from it, and tear it down.
step_reinstall() {
  local out rc n group=$((0x55565041))
  out=$("$RL" once 2>&1) || { fail "install did not converge"; echo "$out" | indent; }
  expect "a real install's pxa1 group" "$(cat /sys/class/net/pxa1/netdev_group)" $group
  "$RL" teardown >/dev/null 2>&1
  for n in 1 2; do
    ip link add pxa$n group $group type wireguard
    ip link set pxa$n up
    ip rule add pref 100 iif pxa$n unreachable proto 85
  done
  ip rule add pref 99 fwmark 0x169 lookup 100 proto 85
  ip route add local default dev lo proto 85 table 100
  note "an earlier install's pxa1 and pxa2 up, its rules and route; /data lost; the reinstall puts slot1 on srvB"
  out=$("$RL" once --select slot1=srvB 2>&1); rc=$?; echo "$out" | indent
  expect "reinstall exit status" "$rc" 0
  expect "reinstall: pxa1" "$(link_up pxa1)" up
  expect "reinstall: pxa2" "$(link_up pxa2)" up
  wait_exit $SRV_B
  out=$("$RL" teardown 2>&1); rc=$?; echo "$out" | indent
  expect "reinstall's teardown exit status" "$rc" 0
  expect "reinstall's teardown: pxa links left" "$(ip -o link show | grep -c ' pxa')" 0
  expect "reinstall's teardown: rules left at prefs 99 and 100" "$(ip rule show | grep -cE '^(99|100):')" 0
  expect "reinstall's teardown: routes left in table 100" "$(ip route show table 100 2>/dev/null | wc -l)" 0
  out=$("$RL" once 2>&1) && pass "installed again" || { fail "install again"; echo "$out" | indent; }
  wait_exit $SRV_A
}

prefs_rules() { ip rule show | grep -E '^(99|100):' | sed 's/ \[detached\]//' | tr -s '\t ' ' '; }

# step_absentlinks: someone else deletes both our links, and foreign software
# adds an unmarked rule in our shape at a name we retire. A teardown removes
# every rule of ours, link or no link, and never the unmarked one.
step_absentlinks() {
  local out rc
  out=$("$RL" once 2>&1) || { fail "install did not converge"; echo "$out" | indent; }
  ip link del pxa1
  ip link del pxa2
  ip rule add pref 100 iif pxa1 unreachable
  note "pxa1 and pxa2 deleted by someone else; foreign software adds an unmarked 'iif pxa1 unreachable' at pref 100"
  prefs_rules | indent
  out=$("$RL" teardown 2>&1); rc=$?; echo "$out" | indent
  expect "teardown exit status" "$rc" 0
  expect "rules left at prefs 99 and 100" "$(prefs_rules | tr '\n' ';')" "100: from all iif pxa1 unreachable;"
  ip rule del pref 100 iif pxa1 unreachable
  out=$("$RL" once 2>&1) && pass "installed again" || { fail "install again"; echo "$out" | indent; }
  wait_exit $SRV_A
}

# step_foreignlink: a pxa link outside our group is foreign software's. A
# teardown leaves it and its rule. At a name we want it is a conflict: our gates
# close, the conflict is reported, and the foreign link stays as it is. Our own
# running pxa1 moved to another group and set up, its index unchanged, is never
# read as our gate: the slot reads closed, from one message per link (W33).
step_foreignlink() {
  local out rc
  "$RL" teardown >/dev/null 2>&1
  ip link add pxa1 group 4242 type wireguard
  ip link set pxa1 up
  ip rule add pref 100 iif pxa1 unreachable
  note "foreign software's pxa1 (group 4242, up) with its iif unreachable rule; the runtime tears down"
  out=$("$RL" teardown 2>&1); rc=$?; echo "$out" | indent
  expect "teardown exit status beside a foreign pxa1" "$rc" 0
  expect "foreign pxa1's rule kept" "$(ip rule show pref 100 | grep -c 'iif pxa1')" 1
  expect "foreign pxa1" "$(link_up pxa1)" up
  ip rule del pref 100 iif pxa1 unreachable
  ip link del pxa1

  out=$("$RL" once 2>&1) || { fail "install did not converge"; echo "$out" | indent; }
  wait_exit $SRV_B 203.0.113.82
  ip link del pxa1
  ip link add pxa1 group 4242 type wireguard
  ip link set pxa1 up
  note "our pxa1 deleted and a foreign pxa1 (group 4242, up) put in its place"
  out=$("$RL" once 2>&1); rc=$?; echo "$out" | indent
  [ "$rc" -ne 0 ] && grep -q 'link pxa1 exists and is not ours' <<<"$out" &&
    pass "conflict at pxa1 reported (exit status $rc)" || fail "conflict at pxa1 not reported (exit status $rc)"
  expect "our pxa2 beside the conflict" "$(link_up pxa2)" down
  expect "foreign pxa1 left up" "$(link_up pxa1)" up
  expect "foreign pxa1 left in its group" "$(cat /sys/class/net/pxa1/netdev_group)" 4242
  ip link del pxa1
  out=$("$RL" once 2>&1) && pass "converged once the foreign pxa1 left" || { fail "no convergence once the foreign pxa1 left"; echo "$out" | indent; }
  wait_exit $SRV_A

  local index; index=$(cat /sys/class/net/pxa1/ifindex)
  ip link set dev pxa1 group 4242 up
  note "our running pxa1 moved to group 4242 and set up, its index $index unchanged"
  out=$("$RL" once 2>&1); echo "$out" | indent
  grep -q 'slot slot1 phase closed' <<<"$out" && pass "slot1 reads closed on a foreign pxa1 at its index" ||
    fail "slot1 doesn't read closed on a foreign pxa1 at its index"
  expect "pxa1's index" "$(cat /sys/class/net/pxa1/ifindex)" "$index"
  expect "foreign pxa1 left up" "$(link_up pxa1)" up
  expect "foreign pxa1 left in its group" "$(cat /sys/class/net/pxa1/netdev_group)" 4242
  ip link del pxa1
  out=$("$RL" once 2>&1) && pass "converged once the regrouped pxa1 left" || { fail "no convergence once the regrouped pxa1 left"; echo "$out" | indent; }
  wait_exit $SRV_A
}

# step_collision: state that merely carries our marker is not ours. Rules on links
# outside our group and names we want stay, and do not touch our gates. A marked
# rule that can match pxa traffic, and any other route in table 100, is a conflict
# and stays too. Only our exact route is ours.
step_collision() {
  local out rc
  out=$("$RL" once 2>&1) || { fail "install did not converge"; echo "$out" | indent; }
  ip link add foreign0 type dummy
  ip link add pxa9 group 4242 type dummy
  ip rule add pref 100 iif foreign0 unreachable proto 85
  ip rule add pref 100 iif pxa9 unreachable proto 85
  ip rule add pref 100 iif pxa7 unreachable proto 85
  note "marked rules at pref 100 on foreign0, on foreign pxa9, and on pxa7, which nothing wants"
  out=$("$RL" once 2>&1); rc=$?; echo "$out" | indent
  expect "reconcile exit status beside marked foreign rules" "$rc" 0
  expect "reconcile beside marked foreign rules changes" "$(changes "$out")" 0
  expect "pxa1 beside marked foreign rules" "$(link_up pxa1)" up
  expect "marked foreign rules kept" "$(prefs_rules | grep -cE 'iif (foreign0|pxa9|pxa7) unreachable proto 85')" 3
  out=$("$RL" teardown 2>&1); rc=$?; echo "$out" | indent
  expect "teardown exit status beside marked foreign rules" "$rc" 0
  expect "rules a teardown left at prefs 99 and 100" "$(prefs_rules | tr '\n' ';')" \
    "100: from all iif foreign0 unreachable proto 85;100: from all iif pxa9 unreachable proto 85;100: from all iif pxa7 unreachable proto 85;"
  expect "foreign0 after a teardown" "$(cat /sys/class/net/foreign0/operstate)" down
  ip rule del pref 100 iif foreign0 unreachable proto 85
  ip rule del pref 100 iif pxa9 unreachable proto 85
  ip rule del pref 100 iif pxa7 unreachable proto 85
  ip link del foreign0
  ip link del pxa9

  ip route add local default dev lo proto 85 table 100
  out=$("$RL" teardown 2>&1); rc=$?; echo "$out" | indent
  expect "teardown exit status beside our exact route" "$rc" 0
  expect "routes left in table 100 by a teardown of our exact route" "$(ip route show table 100 2>/dev/null | wc -l)" 0

  out=$("$RL" once 2>&1) || { fail "install did not converge"; echo "$out" | indent; }
  ip rule add pref 99 fwmark 0x169 lookup 52 proto 85
  ip route add local 192.0.2.1 dev lo proto 85 table 100
  note "a marked rule on our mark, and a marked route, that are not ours"
  out=$("$RL" once 2>&1); rc=$?; echo "$out" | indent
  [ "$rc" -ne 0 ] && grep -q 'kernel conflicts' <<<"$out" && pass "marked foreign state in our way is a conflict (exit status $rc)" || fail "marked foreign state not reported (exit status $rc)"
  expect "marked foreign fwmark rule kept" "$(ip rule show pref 99 | grep -c 'lookup 52')" 1
  expect "marked foreign route kept" "$(ip route show table 100 | grep -c '192.0.2.1')" 1
  ip rule del pref 99 fwmark 0x169 lookup 52 proto 85
  ip route del local 192.0.2.1 dev lo proto 85 table 100
  out=$("$RL" once 2>&1) || { fail "no convergence once the marked foreign state left"; echo "$out" | indent; }
  wait_exit $SRV_A
}

# step_foreign puts rules like Tailscale's beside ours, next to the one the topology
# copies from the real gateway at pref 5270. A rule
# that cannot match pxa traffic touches nothing; one that can is reported.
step_foreign() {
  local out rc before p
  out=$("$RL" once 2>&1) || { fail "install did not converge"; echo "$out" | indent; }
  ip rule add pref 5210 fwmark 0x80000/0xff0000 lookup main
  ip rule add pref 5250 fwmark 0x80000/0xff0000 unreachable
  ip rule add pref 5280 lookup main suppress_prefixlength 0
  ip rule add pref 5290 from 10.77.0.0/16 goto 32000
  before=$(ip rule show)
  out=$("$RL" once 2>&1); rc=$?
  expect "a real-style ruleset: exit status" "$rc" 0
  expect "a real-style ruleset: changes" "$(changes "$out")" 0
  expect "a real-style ruleset: untouched" "$(ip rule show)" "$before"
  harmless() {
    local what=$1; shift
    ip rule add "$@"
    out=$("$RL" once 2>&1); rc=$?
    expect "$what: exit status" "$rc" 0
    expect "$what: pxa1" "$(link_up pxa1)" up
    expect "$what: pxa2" "$(link_up pxa2)" up
    expect "$what: kept" "$(ip rule show | grep -c "^$p:")" "$KEPT"
    ip rule del "$@"
    "$RL" once >/dev/null 2>&1
  }
  conflicting() {
    local what=$1; shift
    ip rule add "$@"
    out=$("$RL" once 2>&1); rc=$?
    [ "$rc" -ne 0 ] && grep -q 'kernel conflicts' <<<"$out" && pass "$what: reported" || fail "$what: not reported (exit status $rc)"
    expect "$what: pxa1" "$(link_up pxa1)" down
    ip rule del "$@"
    out=$("$RL" once 2>&1) || { fail "$what: no recovery"; echo "$out" | indent; }
    expect "$what: pxa1 once it left" "$(link_up pxa1)" up
  }
  p=100; KEPT=3
  harmless "fwmark, then an iif outside our links" pref 100 fwmark 0x80000/0xff0000 iif wg9 lookup 52
  harmless "destination, then an iif outside our links" pref 100 to 10.0.0.0/8 iif wg9 lookup 52
  harmless "tos, then an iif outside our links" pref 100 tos 0x10 iif wg9 lookup 52
  harmless "a mark pxa traffic never carries" pref 100 fwmark 0x80000/0xff0000 lookup 52
  harmless "a source no tunnel holds" pref 100 from 10.99.0.1 lookup 52
  harmless "a goto from a source no tunnel holds" pref 100 from 10.77.0.0/16 goto 5290
  conflicting "a negated iif outside our links" pref 100 not iif wg9 lookup 52
  conflicting "a goto at our preference" pref 100 goto 5290
  conflicting "the negated mark at our preference" pref 100 not fwmark 0x80000/0xff0000 lookup 52
  conflicting "a source inside slot1's tunnel" pref 100 from 10.70.1.2 lookup 52
  conflicting "any rule before ours" pref 50 lookup 52
  for p in 5210 5250 5280 5290; do ip rule del pref $p; done
  out=$("$RL" once 2>&1) || { fail "reconcile after the foreign rules left"; echo "$out" | indent; }
  wait_exit $SRV_A
}

# step_orphanrule: an owned link nothing wants, whose rule delete fails once. The
# rule must go before the link, so the next pass can still tell the rule is ours.
step_orphanrule() {
  local S out rc
  out=$("$RL" once 2>&1) || { fail "install did not converge"; echo "$out" | indent; }
  ip link add dev pxa7 group $((0x55565041)) type wireguard
  ip rule add pref 100 iif pxa7 unreachable proto 85
  S=$(mktemp -d)
  cat >"$S/ip" <<SHIM
#!/bin/bash
if [[ "\$*" == "rule del pref 100 iif pxa7"* && ! -e $S/once ]]; then touch $S/once; echo "killed at 10 s" >&2; exit 137; fi
exec /sbin/ip "\$@"
SHIM
  chmod +x "$S/ip"
  out=$(PATH=$S:$PATH "$RL" once 2>&1); echo "$out" | grep -E 'pxa7|error' | cut -c1-140 | indent
  expect "pxa1 while a stale rule's delete fails" "$(link_up pxa1)" up
  out=$("$RL" once 2>&1); rc=$?; echo "$out" | grep -E 'pxa7|error' | cut -c1-140 | indent
  expect "next pass: exit status" "$rc" 0
  expect "next pass: pxa7 links left" "$(ip -o link show | grep -c ' pxa7')" 0
  expect "next pass: pxa7 rules left" "$(ip rule show | grep -c 'iif pxa7')" 0
  ip rule del pref 100 iif pxa7 unreachable proto 85 2>/dev/null
  rm -rf "$S"
}

# step_mixed plans a Hysteria2 root nothing selects, a server the core refuses and one
# the renderer can't express. Each bad one costs only itself, cold or hot.
step_mixed() {
  local out rc all=hy,badkey,unrendered
  out=$("$RL" once --with $all 2>&1); rc=$?; echo "$out" | grep -E 'refused|slot |error' | sort -u | cut -c1-150 | indent
  expect "beside refused probe roots: exit status" "$rc" 0
  grep -q 'refused badkey' <<<"$out" && pass "the core's refusal is reported" || fail "the core's refusal went unreported"
  grep -q 'refused unrendered: .*Path' <<<"$out" && pass "the renderer's refusal is reported" || fail "the renderer's refusal went unreported"
  grep -q 'lab-bad-shadowsocks-key' <<<"$out" && fail "a refusal quotes the credential" || pass "no refusal quotes the credential"
  expect "beside refused probe roots: pxa1" "$(link_up pxa1)" up
  expect "beside refused probe roots: pxa2" "$(link_up pxa2)" up
  [ -n "$(tag_of hy)" ] && pass "the Hysteria2 root is installed" || fail "the Hysteria2 root is not installed"
  expect "the refused roots' outbounds" "$(tag_of badkey)$(tag_of unrendered)" ""
  wait_exit $SRV_A
  ip link set pxa1 down
  out=$("$RL" once --with $all 2>&1); rc=$?
  expect "reopen beside refused probe roots: exit status" "$rc" 0
  expect "reopen beside refused probe roots: pxa1" "$(link_up pxa1)" up

  out=$("$RL" once --with $all --select slot2=badkey 2>&1); rc=$?; echo "$out" | grep -E 'slot ' | sort -u | indent
  expect "slot2 moves to a refused server: exit status" "$rc" 0
  grep -q 'slot slot2: server_refused' <<<"$out" && pass "slot2's refusal is reported" || fail "slot2's refusal went unreported"
  expect "slot2 moves to a refused server: pxa2 stays on its prior" "$(link_up pxa2)" up
  wait_exit $SRV_B 203.0.113.82

  out=$("$RL" once --with $all --select slot2=badkey --from slot2=badkey 2>&1); rc=$?; echo "$out" | grep -E 'slot ' | sort -u | indent
  expect "slot2 selects a refused server: exit status" "$rc" 0
  grep -q 'slot slot2: server_refused' <<<"$out" && pass "slot2's refusal is reported" || fail "slot2's refusal went unreported"
  expect "slot2 selects a refused server with nothing to fall back on: pxa2" "$(link_up pxa2)" down
  expect "slot2 selects a refused server: pxa1" "$(link_up pxa1)" up
  wait_exit $SRV_A

  systemctl stop $CORE_UNIT
  out=$("$RL" once --with $all 2>&1); rc=$?
  expect "cold start beside refused roots: exit status" "$rc" 0
  expect "cold start beside refused roots: core" "$(systemctl is-active $CORE_UNIT)" active
  expect "cold start beside refused roots: pxa1" "$(link_up pxa1)" up
  expect "cold start beside refused roots: pxa2" "$(link_up pxa2)" up
  grep -q 'lab-bad-shadowsocks-key' $RUN_DIR/xray.json && fail "the refused server reached the core's config" || pass "the refused server is not in the core's config"
  wait_exit $SRV_B 203.0.113.82

  out=$("$RL" once 2>&1); rc=$?
  expect "plan back to the fixture: exit status" "$rc" 0
  wait_exit $SRV_A
}

# step_renderers runs the renderer's tests against the pinned core: every protocol and
# transport the catalogue emits loads, and the ones with wire fields connect, over
# loopback, to a core configured the way a provider would be.
step_renderers() {
  local out rc; out=$(XRAY_BIN=/usr/local/bin/xray $LAB_HOME/current/runtime.test -test.run 'Xray|Live' -test.v 2>&1); rc=$?
  grep -E '^(=== RUN|--- |    --- |FAIL|ok|PASS)' <<<"$out" | grep -v '=== RUN' | indent
  [ "$rc" -ne 0 ] && echo "$out" | grep -E '_test.go:' | indent
  expect "renderer tests against the pinned core: exit status" "$rc" 0
}

# named_markers runs the runtime on one iproute2, whose dir is $2 or empty for the
# VM's own, while /etc/iproute2 names our protocol, table, scope and link group.
named_markers() {
  local v=$1 p=$PATH out rc
  [ -n "$2" ] && p=$2:$PATH
  note "$v: $(PATH=$p ip -V), which prints: $(PATH=$p ip rule show pref 99 | tr -s '\t ' ' ')"
  out=$(PATH=$p "$RL" once 2>&1); rc=$?; echo "$out" | indent
  expect "$v: reconcile exit status" "$rc" 0
  expect "$v: reconcile changes" "$(changes "$out")" 0
  expect "$v: pxa1" "$(link_up pxa1)" up
  expect "$v: pxa2" "$(link_up pxa2)" up
  ip link del pxa2
  out=$(PATH=$p "$RL" once 2>&1) || { fail "$v: pxa2 not rebuilt"; echo "$out" | indent; }
  expect "$v: rebuilt pxa2" "$(link_up pxa2)" up
  expect "$v: rebuilt pxa2's group" "$(cat /sys/class/net/pxa2/netdev_group)" $((0x55565041))
  $LAB_HOME/current/$DAEMON_BIN close-gates --run-dir $RUN_DIR
  expect "$v: close-gates: pxa1" "$(link_up pxa1)" down
  out=$(PATH=$p "$RL" once 2>&1) || { fail "$v: gates did not reopen"; echo "$out" | indent; }
  expect "$v: reopened pxa1" "$(link_up pxa1)" up
  out=$(PATH=$p "$RL" teardown 2>&1); rc=$?
  expect "$v: teardown exit status" "$rc" 0
  expect "$v: teardown: rules left at prefs 99 and 100" "$(ip rule show | grep -cE '^(99|100):')" 0
  expect "$v: teardown: routes left in table 100" "$(ip route show table 100 2>/dev/null | wc -l)" 0
  expect "$v: teardown: pxa links left" "$(ip -o link show | grep -c ' pxa')" 0
  out=$(PATH=$p "$RL" once 2>&1) || { fail "$v: install did not converge"; echo "$out" | indent; }
  wait_exit $SRV_A
}

step_named() {
  local out files="rt_protos rt_tables rt_scopes group" f saved; saved=$(mktemp -d)
  for f in $files; do cp "/etc/iproute2/$f" "$saved/$f"; done
  out=$("$RL" once 2>&1) || { fail "install did not converge"; echo "$out" | indent; }
  echo '85 sidegatelab' >>/etc/iproute2/rt_protos
  echo '100 sidegatelab' >>/etc/iproute2/rt_tables
  echo '254 sidegatelab' >>/etc/iproute2/rt_scopes
  echo '1431720001 sidegatelab' >>/etc/iproute2/group
  named_markers "iproute2 5.5" ""
  named_markers "iproute2 5.9" /opt/iproute2/5.9.0
  named_markers "iproute2 6.1" /opt/iproute2/6.1.0
  for f in $files; do cp "$saved/$f" "/etc/iproute2/$f"; done
  rm -rf "$saved"
  out=$("$RL" once 2>&1) || { fail "reconcile once the names left"; echo "$out" | indent; }
}

step_teardown() {
  daemon_stop
  local out; out=$("$RL" teardown 2>&1); echo "$out" | indent
  expect "core after teardown" "$(systemctl is-active $CORE_UNIT)" inactive
  snapshot >"$SNAP.post"
  if diff -u "$SNAP.pre" "$SNAP.post" >"$SNAP.diff"; then
    pass "ip rule, iptables-save, table 100 and links match the pre-install snapshot (diff empty)"
  else
    fail "state differs from the pre-install snapshot:"; indent <"$SNAP.diff"
  fi
  rm -f /etc/systemd/system/$CORE_UNIT
  systemctl daemon-reload
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  [ -n "$STEP" ] || { echo "usage: runtime.sh <step>, the steps named above" >&2; exit 2; }
  "step_$STEP"
  exit $((fails > 0))
fi
