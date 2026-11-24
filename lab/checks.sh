#!/usr/bin/env bash
# Runs as root inside the VM after topology.sh. Usage: checks.sh <a|b|c|d|e|f|g|h|i>
# The environment can point the checks at plumbing someone else built, as
# runtime.sh does for the daemon's: core unit, API address, slot1 balancer,
# srvA and srvB outbound tags, and the loopback guard's port match.
set -uo pipefail
GW_UNIT=${GW_UNIT:-lab-xray-gw}
XRAY_API=${XRAY_API:-127.0.0.1:10085}
BAL1=${BAL1:-slot1}
OUT_A=${OUT_A:-srvA}
OUT_B=${OUT_B:-srvB}
GUARD_MATCH=${GUARD_MATCH:-dpts:51831:51838}

STATE=/var/lib/unifi-lab
LAN="ip netns exec lan"
LABWEB=/opt/unifi-lab/labweb
SRV_A=198.51.100.10
SRV_B=198.51.100.20
DIRECT=203.0.113.80
SLOT1=203.0.113.81
SLOT2=203.0.113.82
WGPLAIN=203.0.113.83
WAN_IP=192.168.100.10
UPLINK=$(ip -o route show default table main | awk '{print $5; exit}')

CHECK=$1
fails=0
pass() { echo "PASS $CHECK: $*"; }
fail() { echo "FAIL $CHECK: $*"; fails=$((fails + 1)); }
note() { echo "       $*"; }
expect() {
  local what=$1 got=$2 want=$3
  if [ "$got" = "$want" ]; then pass "$what = $got"; else fail "$what = '$got', want '$want'"; fi
}

exit_ip() { $LAN curl -s -m "${2:-5}" "http://$1/ip"; }
xapi() { /usr/local/bin/xray api "$1" --server="$XRAY_API" "${@:2}"; }
counter() { cat "/sys/class/net/$1/statistics/$2"; }
rule_pkts() { iptables -t "$1" -nvxL "$2" | awk -v m="$3" 'index($0, m) {print $1; exit}'; }
payload_sum() { tcpdump -r "$1" -nn "$2" 2>/dev/null | awk '{for (i = 1; i < NF; i++) if ($i == "length") s += $(i + 1)} END {print s + 0}'; }
pkts() { tcpdump -r "$1" -nn "$2" 2>/dev/null | wc -l; }

check_a() {
  local k; k=$(uname -r)
  case "$k" in 5.4.*) pass "kernel $k" ;; *) fail "kernel $k is not 5.4" ;; esac
  local v; v=$(iptables --version)
  case "$v" in *legacy*) pass "$v" ;; *) fail "$v is not legacy" ;; esac
  expect "iptables alternative" "$(readlink -f /etc/alternatives/iptables)" /usr/sbin/xtables-legacy-multi
}

check_b() {
  expect "direct (no route) exit" "$(exit_ip $DIRECT)" $WAN_IP
  expect "route to wgclt1 (slot1) TCP exit" "$(exit_ip $SLOT1)" $SRV_A
  expect "route to wgclt2 (slot2) TCP exit" "$(exit_ip $SLOT2)" $SRV_B
  expect "route to wgclt1 (slot1) UDP exit" "$($LAN $LABWEB udp $SLOT1:9000)" $SRV_A
  expect "route to wgclt2 (slot2) UDP exit" "$($LAN $LABWEB udp $SLOT2:9000)" $SRV_B
  expect "route to wgclt3 (plain WG) exit" "$(exit_ip $WGPLAIN)" 198.51.100.30
  if $LAN $LABWEB udp $SLOT1:443 >/dev/null 2>&1; then fail "UDP 443 through slot1 got a reply"; else pass "UDP 443 through slot1 dropped"; fi
}

check_c() {
  local n=100000000 dir got
  dir=$(mktemp -d)
  local rx_wg0 tx_px0 tx_br0 rx_px0
  rx_px0=$(counter pxa1 rx_bytes); tx_px0=$(counter pxa1 tx_bytes)
  rx_wg0=$(counter wgclt1 rx_bytes); tx_br0=$(counter br0 tx_bytes)
  local i pids=()
  for i in pxa1 wgclt1 br0 lo; do
    local filter="host $SLOT1"
    [ "$i" = lo ] && filter="udp port 51831"
    tcpdump -i "$i" -nn -s 96 -B 65536 -w "$dir/$i.pcap" "$filter" 2>"$dir/$i.err" & pids+=($!)
  done
  sleep 1
  got=$($LAN curl -s -m 60 -o /dev/null -w '%{size_download}' "http://$SLOT1/bytes?n=$n")
  # tcpdump hands ring blocks over on a 1 s timeout, so a kill sooner loses the last block.
  sleep 3
  kill "${pids[@]}"; wait "${pids[@]}" 2>/dev/null

  local px_up px_reply wg_up wg_reply br_reply_bytes lo_in lo_out
  px_up=$(pkts "$dir/pxa1.pcap" "dst host $SLOT1")
  px_reply=$(pkts "$dir/pxa1.pcap" "src host $SLOT1")
  wg_up=$(pkts "$dir/wgclt1.pcap" "dst host $SLOT1")
  wg_reply=$(pkts "$dir/wgclt1.pcap" "src host $SLOT1")
  br_reply_bytes=$(payload_sum "$dir/br0.pcap" "src host $SLOT1 and dst host 192.168.1.10")
  lo_in=$(payload_sum "$dir/lo.pcap" "udp dst port 51831")
  lo_out=$(payload_sum "$dir/lo.pcap" "udp src port 51831")
  local d_rx_px=$(( $(counter pxa1 rx_bytes) - rx_px0 )) d_tx_px=$(( $(counter pxa1 tx_bytes) - tx_px0 ))
  local d_rx_wg=$(( $(counter wgclt1 rx_bytes) - rx_wg0 )) d_tx_br=$(( $(counter br0 tx_bytes) - tx_br0 ))

  expect "download bytes via slot1" "$got" "$n"
  note "tcpdump pxa1:   $px_up packets toward $SLOT1 (upload), $px_reply packets from $SLOT1 (reply)"
  note "tcpdump wgclt1: $wg_up packets toward $SLOT1 (upload), $wg_reply packets from $SLOT1 (reply)"
  note "tcpdump br0:    $br_reply_bytes TCP payload bytes $SLOT1 -> 192.168.1.10"
  note "tcpdump lo:     WG UDP to :51831 $lo_in bytes (wgclt1->pxa1), from :51831 $lo_out bytes (pxa1->wgclt1)"
  note "counters:       pxa1 rx +$d_rx_px tx +$d_tx_px, wgclt1 rx +$d_rx_wg, br0 tx +$d_tx_br"
  local lossless=1
  for i in pxa1 wgclt1 br0 lo; do
    local cap rcv drop
    cap=$(awk '/packets captured/ {print $1}' "$dir/$i.err"); rcv=$(awk '/received by filter/ {print $1}' "$dir/$i.err")
    drop=$(awk '/dropped by kernel/ {print $1}' "$dir/$i.err")
    note "tcpdump $i: $cap captured, $rcv received by filter, $drop dropped by kernel"
    # On lo the filter sees each packet twice (out and in), and libpcap keeps one copy.
    local want=$cap; [ "$i" = lo ] && want=$((cap * 2))
    [ "$rcv" = "$want" ] && [ "$drop" = 0 ] || lossless=0
  done
  [ "$lossless" = 1 ] && pass "captures are complete (every packet the filter matched was written, 0 dropped)" || fail "a capture lost packets, so its counts are a lower bound"
  [ "$px_up" -gt 0 ] && [ "$wg_up" -gt 0 ] && pass "uploads (requests and ACKs) traverse wgclt1 and pxa1" || fail "no upload packets on wgclt1/pxa1"
  # A WireGuard keepalive is 32 B on the counters and carries no inner packet.
  if [ "$px_reply" -eq 0 ] && [ "$wg_reply" -eq 0 ] && [ $((d_tx_px % 32)) -eq 0 ] && [ "$d_tx_px" -le 320 ]; then
    pass "no reply packets on pxa1 or wgclt1 (tcpdump 0, pxa1 tx +$d_tx_px B = $((d_tx_px / 32)) keepalives)"
  else
    fail "reply traffic traverses the tunnel (tcpdump $px_reply/$wg_reply, pxa1 tx +$d_tx_px B)"
  fi
  [ "$br_reply_bytes" -ge "$n" ] && [ "$d_tx_br" -ge "$n" ] && pass "reply payload reaches the lan client on br0 (tcpdump $br_reply_bytes, br0 tx +$d_tx_br, both >= $n)" || fail "br0 carried tcpdump $br_reply_bytes, counter $d_tx_br bytes"
  [ "$lo_out" -le 1024 ] && [ "$d_rx_wg" -le 320 ] && pass "tunnel return direction carried only keepalives or handshakes (UDP from :51831 $lo_out B, wgclt1 rx +$d_rx_wg B)" || fail "tunnel return direction carried $lo_out B, wgclt1 rx +$d_rx_wg"
  rm -rf "$dir"
}

check_d() {
  local n=60000000 dir; dir=$(mktemp -d)
  $LAN curl -s -m 60 --limit-rate 10M -D "$dir/headers" -o /dev/null -w '%{size_download} %{http_code}' \
    "http://$SLOT1/bytes?n=$n" >"$dir/result" &
  local dl=$!
  sleep 1.5
  local first; first=$(awk -F'[ :]+' 'tolower($1) == "x-client-addr" {print $2}' "$dir/headers")
  expect "long download started via" "$first" $SRV_A
  xapi bo -b "$BAL1" "$OUT_B" >/dev/null && pass "override slot1 -> srvB through the API (RoutingService)" || fail "xray api bo failed"
  note "$(xapi bi "$BAL1" 2>&1 | tr -s ' \n' ' ' | cut -c1-160)"
  expect "new connection on slot1 exits via" "$(exit_ip $SLOT1)" $SRV_B
  local still; still=$(ss -tnH state established "dst $SRV_A:443" | wc -l)
  [ "$still" -gt 0 ] && pass "old upstream to srvA still established after the swap ($still sockets)" || fail "no upstream to srvA during the download"
  if kill -0 $dl 2>/dev/null; then pass "download still running after the swap"; else fail "download ended before the swap took effect"; fi
  wait $dl
  expect "long download result (bytes code)" "$(cat "$dir/result")" "$n 200"
  xapi bo -b "$BAL1" "$OUT_A" >/dev/null
  expect "slot1 back on srvA" "$(exit_ip $SLOT1)" $SRV_A
  rm -rf "$dir"
}

leak_watch() {
  local dir=$1 filter=${2:-dst host $SLOT1}
  tcpdump -i eth4 -nn -U -w "$dir/eth4.pcap" "$filter" 2>/dev/null & echo $! >"$dir/pids"
  tcpdump -i "$UPLINK" -nn -U -w "$dir/uplink.pcap" "$filter" 2>/dev/null & echo $! >>"$dir/pids"
  sleep 1
}
leak_count() {
  local dir=$1 f
  sleep 0.5; kill $(cat "$dir/pids"); wait $(cat "$dir/pids") 2>/dev/null
  for f in "$dir/eth4.pcap" "$dir/uplink.pcap"; do
    tcpdump -r "$f" -nn -e -v 2>/dev/null | sed "s|^|       leaked on $(basename "$f" .pcap): |" >&2
  done
  echo $(( $(pkts "$dir/eth4.pcap" "") + $(pkts "$dir/uplink.pcap" "") ))
}

check_e() {
  local dir; dir=$(mktemp -d)
  ip link set pxa1 down
  leak_watch "$dir"
  if exit_ip $SLOT1 4 >/dev/null; then fail "slot1 still answers with pxa1 down"; else pass "pxa1 down: slot1 traffic stops"; fi
  $LAN $LABWEB udp $SLOT1:9000 >/dev/null 2>&1 && fail "UDP still answers with pxa1 down"
  local leaks; leaks=$(leak_count "$dir")
  expect "pxa1 down: packets to $SLOT1 leaving eth4 or $UPLINK" "$leaks" 0

  local route_get="ip route get $SLOT1 from 192.168.1.10 iif br0 mark 0x6b0000"
  expect "route for mark 0x6b0000 with table 181 populated" "$($route_get 2>&1 | grep -o 'dev wgclt1')" "dev wgclt1"
  ip route del default dev wgclt1 table 181
  note "UniFi stand-in: removed 'default dev wgclt1' from table 181 (VPN Client marked down)"
  expect "route for mark 0x6b0000 with table 181 empty (EINVAL = pref 32765 blackhole rule)" "$($route_get 2>&1)" "RTNETLINK answers: Invalid argument"
  leak_watch "$dir"
  if exit_ip $SLOT1 4 >/dev/null; then fail "slot1 answers with its route table empty"; else pass "kill switch: traffic stops with table 181 empty"; fi
  leaks=$(leak_count "$dir")
  expect "kill switch: packets to $SLOT1 leaving eth4 or $UPLINK" "$leaks" 0

  ip route add default dev wgclt1 table 181
  ip link set pxa1 up
  local start; start=$(date +%s)
  local got=""
  while [ $(( $(date +%s) - start )) -lt 40 ]; do
    got=$(exit_ip $SLOT1 2) && break
  done
  expect "pxa1 back up: slot1 exit after $(( $(date +%s) - start )) s" "$got" $SRV_A
  rm -rf "$dir"
}

check_f() {
  local dir; dir=$(mktemp -d)
  $LAN curl -s -m 30 --limit-rate 30M -o /dev/null "http://$SLOT1/bytes?n=150000000" & local d1=$!
  $LAN curl -s -m 30 --limit-rate 30M -o /dev/null "http://$SLOT2/bytes?n=150000000" & local d2=$!
  sleep 2.5
  local t0 t1
  t0=$(date +%s%N)
  ss -tniH -p state established '( dport = :443 )' >"$dir/ss"
  t1=$(date +%s%N)
  kill $d1 $d2 2>/dev/null; wait $d1 $d2 2>/dev/null
  note "ss -tni (inet_diag) over $(grep -c xray "$dir/ss") Xray upstream sockets took $(( (t1 - t0) / 1000 )) us"
  local -A server=([$SRV_A:443]=srvA [$SRV_B:443]=srvB)
  local -A busy=()
  while read -r head && read -r info; do
    local dst; dst=$(awk '{print $4}' <<<"$head")
    local name=${server[$dst]:-}
    [ -n "$name" ] && [[ "$head" == *xray* ]] || continue
    local rtt retr rate recv
    rtt=$(grep -oE '(^|[[:space:]])rtt:[^ ]*' <<<"$info" | tr -d '[:space:]' | sed 's/^/s/'); retr=$(grep -o 'retrans:[^ ]*' <<<"$info" || echo retrans:0)
    rate=$(grep -o 'delivery_rate [^ ]*' <<<"$info"); recv=$(grep -o 'bytes_received:[0-9]*' <<<"$info" | cut -d: -f2)
    note "$name $dst $rtt $retr $rate bytes_received:${recv:-0}"
    [ -n "$rtt" ] && [ -n "$rate" ] && [ "${recv:-0}" -gt 1000000 ] && busy[$name]=1
  done <"$dir/ss"
  [ -n "${busy[srvA]:-}" ] && pass "srvA upstream mapped by destination with srtt and delivery_rate" || fail "no busy srvA socket in inet_diag"
  [ -n "${busy[srvB]:-}" ] && pass "srvB upstream mapped by destination with srtt and delivery_rate" || fail "no busy srvB socket in inet_diag"
  rm -rf "$dir"
}

pid_of() { systemctl show -p MainPID --value "$1"; }
ticks() { awk '{print $14 + $15}' "/proc/$1/stat"; }
ksoftirqd_ticks() { for p in $(pgrep ksoftirqd); do ticks "$p"; done | awk '{s += $1} END {print s + 0}'; }
cpu_line() { awk '/^cpu /{busy = $2 + $3 + $4 + $7 + $8; print busy, $8, busy + $5 + $6}' /proc/stat; }

measure() {
  local label=$1; shift
  local gw srv hz; gw=$(pid_of "$GW_UNIT"); srv=$(pid_of lab-xray-srvA); hz=$(getconf CLK_TCK)
  local g0 s0 k0 c0 t0; g0=$(ticks "$gw"); s0=$(ticks "$srv"); k0=$(ksoftirqd_ticks); c0=$(cpu_line); t0=$(date +%s.%N)
  local mbps; mbps=$("$@")
  local g1 s1 k1 c1 t1; g1=$(ticks "$gw"); s1=$(ticks "$srv"); k1=$(ksoftirqd_ticks); c1=$(cpu_line); t1=$(date +%s.%N)
  awk -v label="$label" -v mbps="$mbps" -v hz="$hz" -v t0="$t0" -v t1="$t1" \
      -v g="$((g1 - g0))" -v s="$((s1 - s0))" -v k="$((k1 - k0))" -v c0="$c0" -v c1="$c1" 'BEGIN {
    split(c0, a, " "); split(c1, b, " "); el = t1 - t0
    tot = b[3] - a[3]; busy = (b[1] - a[1]) / tot * 400; soft = (b[2] - a[2]) / tot * 400
    gw = g / hz / el * 100
    printf "| %s | %.0f | %.0f%% | %.1f%% | %.0f%% | %.0f%% | %.0f%% | %.0f%% |\n", label, mbps, gw, gw / (mbps / 1000), s / hz / el * 100, k / hz / el * 100, soft, busy
  }'
}
iperf_mbps() { $LAN iperf3 -J -t 8 -c "$@" | jq '.end.sum_received.bits_per_second / 1e6'; }
curl_mbps() { $LAN curl -sk -m 60 -o /dev/null -w '%{speed_download}' "$1" | awk '{print $1 * 8 / 1e6}'; }

check_g() {
  local out=$STATE/throughput.md
  {
    echo "| path | Mbps | gateway xray CPU | gateway xray CPU per Gbps | srvA xray CPU | ksoftirqd | softirq (all CPUs) | busy (all CPUs) |"
    echo "|---|---|---|---|---|---|---|---|"
    measure "direct upload (iperf3 -c $DIRECT)" iperf_mbps $DIRECT
    measure "direct download (iperf3 -R -c $DIRECT)" iperf_mbps $DIRECT -R
    measure "plain WireGuard wgclt3 upload (iperf3 -c $WGPLAIN)" iperf_mbps $WGPLAIN
    measure "plain WireGuard wgclt3 download (iperf3 -R -c $WGPLAIN)" iperf_mbps $WGPLAIN -R
    measure "slot1 upload (iperf3 -c $SLOT1)" iperf_mbps $SLOT1
    measure "slot1 download (iperf3 -R -c $SLOT1)" iperf_mbps $SLOT1 -R
    measure "slot1 download, plain HTTP (curl 1 GB)" curl_mbps "http://$SLOT1/bytes?n=1000000000"
    measure "slot1 download, inner TLS (curl -k https 1 GB)" curl_mbps "https://$SLOT1/bytes?n=1000000000"
  } >"$out"
  cat "$out"
  local rows; rows=$(awk 'NR > 2' "$out" | wc -l)
  local zero; zero=$(awk -F'|' 'NR > 2 && $3 + 0 <= 0' "$out" | wc -l)
  [ "$rows" -eq 8 ] && [ "$zero" -eq 0 ] && pass "8 throughput rows recorded in $out" || fail "throughput rows: $rows, zero-rate rows: $zero"
}

check_h() {
  local d0; d0=$(rule_pkts filter sidegate-input "$GUARD_MATCH")
  $LAN $LABWEB udp 192.168.1.1:51831 >/dev/null 2>&1
  local d1; d1=$(rule_pkts filter sidegate-input "$GUARD_MATCH")
  [ $((d1 - d0)) -gt 0 ] && pass "WG port 51831 from lan dropped by the loopback-only guard (+$((d1 - d0)))" || fail "guard counter did not move"
  local dir; dir=$(mktemp -d)
  local i0; i0=$(iptables -t mangle -nvxL sidegate-prerouting | awk '$3 == "DROP" && $4 == "all" && $6 == "pxa1" {print $1}')
  tcpdump -i eth4 -nn -U -w "$dir/eth4.pcap" "icmp and dst host $SLOT1" 2>/dev/null & local p=$!
  sleep 1
  $LAN ping -c 2 -W 1 $SLOT1 >/dev/null 2>&1 && fail "ping through slot1 answered"
  sleep 0.5; kill $p; wait $p 2>/dev/null
  local i1; i1=$(iptables -t mangle -nvxL sidegate-prerouting | awk '$3 == "DROP" && $4 == "all" && $6 == "pxa1" {print $1}')
  [ $((i1 - i0)) -ge 2 ] && pass "ICMP through slot1 dropped on pxa1 (+$((i1 - i0)))" || fail "pxa1 catch-all drop counter +$((i1 - i0))"
  expect "ICMP to $SLOT1 leaking out eth4" "$(pkts "$dir/eth4.pcap" "")" 0
  rm -rf "$dir"
}

late_udp_server() {
  ip netns exec web python3 - "$@" <<'EOF'
import socket, sys, time
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind((sys.argv[1], 9100))
_, a = s.recvfrom(64)
t0 = time.time()
for d in map(float, sys.argv[2:]):
    time.sleep(max(0, t0 + d - time.time()))
    s.sendto(b"late\n", a)
EOF
}
held_udp_client() {
  $LAN python3 - "$@" <<'EOF'
import socket, sys, time
c = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
c.connect((sys.argv[1], 9100))
c.send(b"hi\n")
time.sleep(float(sys.argv[2]))
EOF
}
guard_drops() { iptables -nvxL sidegate-output 2>/dev/null | awk '$3 == "DROP" && $7 == "!lo" && $8 == "10.70.1.0/30" {print $1}'; }

# The core outlives UniFi's masquerade entry twice: a TCP flow whose client reset
# a downed pxa1 kept from the core, and a UDP answer later than the entry's 30 s.
# The core's next packet then reaches wgclt1's own address, and the kernel's
# answer from it must not leave by the WAN.
check_i() {
  local dir; dir=$(mktemp -d)
  local d0; d0=$(guard_drops)
  leak_watch "$dir" "src host 10.70.1.2 or dst host $SLOT1"
  tcpdump -i lo -nn -U -w "$dir/lo.pcap" "dst host 10.70.1.2" 2>/dev/null & echo $! >>"$dir/pids"
  sleep 1
  late_udp_server $SLOT1 0 36 & local srv=$!
  sleep 0.5
  held_udp_client $SLOT1 40 & local cli=$!
  $LAN curl -s --limit-rate 200k -o /dev/null "http://$SLOT1/bytes?n=50000000" & local dl=$!
  sleep 2
  ip link set pxa1 down
  sleep 1
  kill -9 $dl; wait $dl 2>/dev/null
  wait $srv $cli 2>/dev/null
  local leaks; leaks=$(leak_count "$dir")
  local tcp udp; tcp=$(pkts "$dir/lo.pcap" tcp); udp=$(pkts "$dir/lo.pcap" udp)
  [ "$tcp" -gt 0 ] && pass "the core's TCP for a reset flow reached wgclt1's own address ($tcp)" || fail "no core TCP reached wgclt1's address, so the reset path went unexercised"
  [ "$udp" -gt 0 ] && pass "the core's late UDP answer reached wgclt1's own address ($udp)" || fail "no core UDP reached wgclt1's address, so the ICMP path went unexercised"
  expect "packets from 10.70.1.2 or to $SLOT1 leaving eth4 or $UPLINK" "$leaks" 0
  local moved=$(( $(guard_drops) - ${d0:-0} ))
  [ "$moved" -ge 2 ] && pass "the kernel's answers from slot1's tunnel dropped by the egress guard (+$moved)" || fail "egress guard counter +$moved"
  ip link set pxa1 up
  local start got=""; start=$(date +%s)
  while [ $(( $(date +%s) - start )) -lt 40 ]; do got=$(exit_ip $SLOT1 2) && break; done
  expect "pxa1 back up: slot1 exit" "$got" $SRV_A
  rm -rf "$dir"
}

"check_$CHECK"
exit $((fails > 0))
