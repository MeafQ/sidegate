#!/usr/bin/env bash
# Runs as root inside the VM. Proves the daemon's controller against a real core and
# stand-in servers. Usage: control.sh STEP, one of
#   setup live [fresh] start kill blackhole degrade loss exitchange swapgap heldmove deadline stalltest wedge pause
#   passive wan blocked blip recover assigned quota restart cost contract
#   direct closeflows lowudp udpbroken udpcost
#   three storm allsilent allsilent64 badtarget jitter closed12 sweep64 memory acme teardown
#   soloblame (args: OCTET SECONDS)
#
# Each step asks for the world it needs and builds it if that isn't what's running, so any
# step runs alone. The root namespace plays the gateway, as in topology.sh. The base world
# has six servers off the internet router, in three stand-in providers:
#   good  srvA 198.51.100.10  srvC 198.51.100.40   DE
#   bad   srvB 198.51.100.20  srvD 198.51.100.50   DE
#   solo  srvE 198.51.100.60  srvF 198.51.100.70   NL
# Slot eu takes any DE server, over wgclt1. Slot solo has srvE as its only member, over
# wgclt2. Slot pin has srvF as its Exit, with no UniFi side: only its gate is checked.
# Timings are production's.
set -uo pipefail
source "$(dirname "$0")/names.sh"

LAB=/opt/unifi-lab
CTL=$LAB_HOME/current/controllab
STATE=/var/lib/unifi-lab
LIVE_KEPT=$STATE/live
CONF=/etc/unifi-lab
DAEMON=lab-control
UUID=6f1c1e52-0d3c-4b8e-9a51-2c1f7c0a5e11
LAN="ip netns exec lan"
EU=203.0.113.81
SOLO=203.0.113.82
EXTRA="srvC srvD srvE srvF srvG"
SHIM=$LAB/shim
SHIM_FLAGS=/run/lab-shim
API=127.0.0.1:10085
# A silent idle server's worst case is a 60 s heartbeat, its 4 s, and two 4 s rounds 1 s apart: 73 s.
LIMIT=80
WAN_IP=192.168.100.10
declare -A IP=([srvA]=198.51.100.10 [srvB]=198.51.100.20 [srvC]=198.51.100.40 [srvD]=198.51.100.50 [srvE]=198.51.100.60 [srvF]=198.51.100.70 [srvG]=198.51.100.80)
declare -A SRV=([198.51.100.10]=srvA [198.51.100.20]=srvB [198.51.100.40]=srvC [198.51.100.50]=srvD [198.51.100.60]=srvE [198.51.100.70]=srvF [198.51.100.80]=srvG)

STEP=${1:-}
fails=0
pass() { echo "PASS $STEP: $*"; }
fail() { echo "FAIL $STEP: $*"; fails=$((fails + 1)); }
note() { echo "       $*"; }
indent() { sed 's/^/       /'; }
expect() {
  local what=$1 got=$2 want=$3
  if [ "$got" = "$want" ]; then pass "$what = $got"; else fail "$what = '$got', want '$want'"; fi
}

now_iso() { date -u +%Y-%m-%dT%H:%M:%S.%3NZ; }
exit_ip() { $LAN curl -s -m "${2:-3}" "http://$1/ip"; }
exit_of() { local ip; ip=$(exit_ip "$1"); echo "${SRV[$ip]:-none}"; }
link_up() { [ $(( $(cat "/sys/class/net/$1/flags" 2>/dev/null || echo 0) & 1 )) -eq 1 ] && echo up || echo down; }
view() { jq -r "$1" "$RUN_DIR/view.json"; }
slot_field() { view ".slots[] | select(.id == \"$1\") | $2"; }
events_since() { "$CTL" switches --since "$1" | grep " slot=$2 "; }
count_since() { events_since "$1" "$2" | grep -c . ; }
elapsed() { awk -v a="$1" -v n="$(date +%s.%N)" 'BEGIN { printf "%.1f", n - a }'; }
past() { [ "$(awk -v a="$1" -v l="$2" -v n="$(date +%s.%N)" 'BEGIN { print (n - a >= l) ? 1 : 0 }')" = 1 ]; }
at_most() { [ "$(awk -v v="$1" -v l="$2" 'BEGIN { print (v < l) ? 1 : 0 }')" = 1 ]; }

# wait_applied prints the epoch time at which slot $2 applied a switch since $1 that matches $5,
# and fails $3 s after $4 (an epoch time).
wait_applied() {
  local since=$1 slot=$2 limit=$3 t0=$4 pattern=$5 line
  while :; do
    line=$(events_since "$since" "$slot" | grep -v 'applied=unapplied' | grep -- "$pattern" | head -1)
    if [ -n "$line" ]; then
      sed -E 's/.* applied=([0-9.]+) .*/\1/' <<<"$line"
      return 0
    fi
    past "$t0" "$limit" && return 1
    sleep 0.25
  done
}
# wait_move prints, to 0.1 s, how long after $4 slot $2 applied a switch, as wait_applied finds it.
wait_move() {
  local applied
  applied=$(wait_applied "$@") || return 1
  awk -v a="$4" -v b="$applied" 'BEGIN { printf "%.1f", b - a }'
}
# ns_of prints a decimal epoch time in whole nanoseconds, so durations compare exactly.
ns_of() {
  local i=${1%%.*} f=""
  [[ $1 == *.* ]] && f=${1#*.}
  f=${f}000000000
  echo $(( ${i:-0} * 1000000000 + 10#${f:0:9} ))
}

# wait_exit polls until slot traffic through $1 leaves via server $2, or $3 s pass.
wait_exit() {
  local target=$1 want=$2 limit=${3:-40} start got=""
  start=$(date +%s)
  while [ $(( $(date +%s) - start )) -lt "$limit" ]; do
    got=$(exit_of "$target")
    [ "$got" = "$want" ] && { echo "$(( $(date +%s) - start ))"; return 0; }
    sleep 0.5
  done
  echo "$got"
  return 1
}

# wait_link waits up to $3 s for link $1 to read $2, and prints how long that took in s.
wait_link() {
  local link=$1 want=$2 limit=$3 t0
  t0=$(date +%s.%N)
  while [ "$(link_up "$link")" != "$want" ]; do
    past "$t0" "$limit" && return 1
    sleep 0.1
  done
  elapsed "$t0"
}

# syn_rule counts new connections to a server's 443, in namespace $1, to address $2 if given.
syn_rule() {
  local d=(); [ -n "${2:-}" ] && d=(-d "$2")
  ip netns exec "$1" iptables -C INPUT "${d[@]}" -p tcp --syn --dport 443 2>/dev/null ||
    ip netns exec "$1" iptables -A INPUT "${d[@]}" -p tcp --syn --dport 443
}
syns() {
  ip netns exec "$1" iptables -vnxL INPUT | awk -v d="${2:-0.0.0.0/0}" '$3 == "tcp" && $8 == d && /dpt:443 flags:0x17\/0x02/ { print $1; exit }'
}

# after_beat returns just after a server's next new connection, the worst moment for it to die:
# its next heartbeat is a whole period away.
after_beat() {
  local before; syn_rule "$@"; before=$(syns "$@")
  for _ in $(seq 650); do
    [ "$(syns "$@")" != "$before" ] && { sleep 0.3; return 0; }
    sleep 0.1
  done
  note "no new connection reached $* in 65 s, so the fault's phase is whatever it is"
}

ticks() { awk '{print $14 + $15 + $16 + $17}' "/proc/$1/stat"; }
daemon_pid() { systemctl show -p MainPID --value $DAEMON; }
core_pid() { systemctl show -p MainPID --value $CORE_UNIT; }
now_ms() { date +%s%3N; }

# cpu_share prints a tick count as a percentage of one core over $2 s.
cpu_share() { awk -v t="$1" -v s="$2" -v hz="$(getconf CLK_TCK)" 'BEGIN { printf "%.1f", t * 100 / hz / s }'; }
rss_mb() { awk -v f="$2" '$1 == f ":" { printf "%d", $2 / 1024 }' "/proc/$1/status"; }

# metrics_sql runs one query on metrics.db, read only, and prints its rows.
metrics_sql() {
  python3 - "$LAB_HOME/$METRICS_DB" "$@" <<'PY'
import sqlite3, sys
db = sqlite3.connect(f"file:{sys.argv[1]}?mode=ro", uri=True)
for row in db.execute(sys.argv[2], sys.argv[3:]):
    print(" ".join("null" if v is None else str(v) for v in row))
PY
}

server_config() {
  jq -n --arg uuid "$UUID" --arg crt "$STATE/$1.crt" --arg key "$STATE/$1.pem" '{
    log: {loglevel: "warning"},
    inbounds: [{listen: "0.0.0.0", port: 443, protocol: "vless",
      settings: {clients: [{id: $uuid, flow: "xtls-rprx-vision"}], decryption: "none"},
      streamSettings: {network: "raw", security: "tls", tlsSettings: {certificates: [{certificateFile: $crt, keyFile: $key}]}}}],
    outbounds: [{protocol: "freedom", settings: {finalRules: [{action: "allow", ip: ["203.0.113.0/24", "8.8.8.8/32", "9.9.9.9/32"]}]}}]
  }'
}

ensure_cert() {
  [ -s "$STATE/$1.crt" ] || openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 3650 \
    -subj "/CN=$1.lab" -addext "subjectAltName=DNS:$1.lab" -keyout "$STATE/$1.pem" -out "$STATE/$1.crt" 2>/dev/null
}

start_xray() {
  systemd-run --unit="lab-xray-$1" --collect --quiet -p Restart=on-failure -p LimitNOFILE=65536 -p NetworkNamespacePath="/run/netns/$1" \
    /usr/local/bin/xray run -c "$CONF/$1.json"
}

# add_namespace hangs a namespace off the internet router's server network.
add_namespace() {
  ip netns add "$1"
  ip -n "$1" link set lo up
  ip -n inet link add "to-$1" type veth peer name eth0 netns "$1"
  ip -n inet link set "to-$1" master net up
  ip -n "$1" link set eth0 up
}

add_server() {
  local s=$1 cert=${2:-$1}
  add_namespace "$s"
  ip -n "$s" addr add "${IP[$s]}/24" dev eth0
  ip -n "$s" route add default via 198.51.100.1
  ensure_cert "$cert"
  server_config "$cert" >"$CONF/$s.json"
  start_xray "$s"
  syn_rule "$s"
}

pin_of() { openssl x509 -noout -fingerprint -sha256 -in "$STATE/$1.crt" | cut -d= -f2 | tr -d : | tr A-F a-f; }

feed_line() {
  local s=$1 label=$2 address=${3:-${IP[$1]:-}}
  echo "vless://$UUID@$address:443?security=tls&sni=$s.lab&allowInsecure=1&pcs=$(pin_of "$s")&flow=xtls-rprx-vision&type=tcp#$label"
}

write_feeds() {
  mkdir -p "$CONF/feeds"
  rm -f /etc/unifi-lab/feeds/*
  { feed_line srvA "DE good-A"; feed_line srvC "DE good-C"; } >"$CONF/feeds/good"
  { feed_line srvB "DE bad-B"; feed_line srvD "DE bad-D"; } >"$CONF/feeds/bad"
  { feed_line srvE "NL solo-E"; feed_line srvF "NL pin-F"; } >"$CONF/feeds/solo"
  echo "upload=1073741824; download=5368709120; total=107374182400; expire=$(( $(date +%s) + 2592000 ))" >"$CONF/feeds/good.info"
  echo "upload=96636764160; download=5368709120; total=107374182400; expire=$(( $(date +%s) + 86400 ))" >"$CONF/feeds/bad.info"
}

drop_base_feeds() { rm -f /etc/unifi-lab/feeds/good* /etc/unifi-lab/feeds/bad* /etc/unifi-lab/feeds/solo*; }

# write_bootstrap takes a JSON object to merge over the base. Failure timings stay production's.
write_bootstrap() {
  local more=${1:-'{}'}
  jq -n --arg data "$LAB_HOME" --arg run "$RUN_DIR" --argjson more "$more" '{
    dataDir: $data, runDir: $run,
    probeTargets: [{url: "http://203.0.113.80/ip", status: 200}, {url: "http://203.0.113.81/ip", status: 200},
      {url: "http://203.0.113.82/ip", status: 200}, {url: "http://203.0.113.83/ip", status: 200}],
    baselineTargets: [{url: "http://203.0.113.80/ip"}, {url: "http://11.0.0.80/ip"}],
    countryRules: [{regex: "^DE ", country: "DE"}, {regex: "^NL ", country: "NL"}]
  } * $more' >"$LAB_HOME/$BOOTSTRAP"
}

# The farm is one namespace with many server addresses and one core, so a world can hold many
# servers. A silent server is a DROP on its address. Farm servers are 198.51.100.101 up.
farm_ip() { echo "198.51.100.$(( 100 + $1 ))"; }
farm_label() { echo "farm-$(( 100 + $1 ))"; }
farm_up() {
  local n=$1 i
  add_namespace farm
  # The core exits from the first address, which stays out of the feed, so silencing a server cuts only that server.
  ip -n farm addr add 198.51.100.100/24 dev eth0
  for i in $(seq "$n"); do ip -n farm addr add "$(farm_ip "$i")/24" dev eth0; done
  ip -n farm route add default via 198.51.100.1
  ensure_cert farm
  server_config farm >"$CONF/farm.json"
  start_xray farm
  for i in $(seq "$n"); do feed_line farm "DE $(farm_label "$i")" "$(farm_ip "$i")"; done >"$CONF/feeds/farm"
}
silence() { ip netns exec farm iptables -I INPUT -d "$1" -p tcp --dport 443 -j DROP; }
unsilence() { ip netns exec farm iptables -D INPUT -d "$1" -p tcp --dport 443 -j DROP; }
label_ip() { echo "198.51.100.${1##*farm-}"; }

# The running daemon overlays supported and health from its world, which a read of state.db can't.
servers_json() { "$CTL" servers; }
label_of_lineage() { servers_json | jq -r --arg id "$1" '.items[] | select(.id == $id) | .label'; }
lineage_of_label() { servers_json | jq -r --arg l "$1" '.items[] | select(.label | contains($l)) | .id' | head -1; }
slot_label() { local id; id=$(slot_field "$1" '.server // empty'); [ -n "$id" ] && label_of_lineage "$id"; }

teardown_extras() {
  local s
  for s in $EXTRA; do
    systemctl stop "lab-xray-$s" 2>/dev/null
    ip netns del "$s" 2>/dev/null
  done
  systemctl stop lab-xray-farm lab-xray-lew lab-unifi lab-dns 2>/dev/null
  ip netns del farm 2>/dev/null
  ip netns del lew 2>/dev/null
}

# dns_up stands in for the resolvers a UDP check asks. They stay in the VM: only the web namespace
# holds these addresses, the internet router routes them there, and nothing else asks them.
dns_up() {
  local a
  for a in 8.8.8.8 9.9.9.9; do
    ip -n web addr add "$a/32" dev eth0
    ip -n inet route add "$a/32" dev net
  done
  systemctl stop lab-dns 2>/dev/null
  systemctl reset-failed lab-dns 2>/dev/null
  systemd-run --unit=lab-dns --collect --quiet -p NetworkNamespacePath=/run/netns/web python3 -c '
import select, socket
socks = [socket.socket(socket.AF_INET, socket.SOCK_DGRAM) for _ in range(2)]
for s, a in zip(socks, ["8.8.8.8", "9.9.9.9"]):
    s.bind((a, 53))
while True:
    for s in select.select(socks, [], [])[0]:
        d, a = s.recvfrom(512)
        s.sendto(d[:2] + bytes([d[2] | 0x80]) + d[3:], a)
'
}

# keep_live moves the live page's state aside, for its next start, so a suite run between two of its
# runs leaves the owner's sources where they were. The daemon has stopped by then.
keep_live() {
  if [ -e "$LAB_HOME/live" ] && [ -e "$LAB_HOME/$STATE_DB" ]; then
    rm -rf "$LIVE_KEPT" && mkdir -p "$LIVE_KEPT" && mv "$LAB_HOME/$STATE_DB"* "$LAB_HOME/$METRICS_DB"* "$LIVE_KEPT"/
  fi
  rm -f "$LAB_HOME/live"
}

world_reset() {
  local n s
  systemctl stop "$DAEMON" 2>/dev/null
  systemctl stop "$CORE_UNIT" 2>/dev/null
  systemctl reset-failed "$DAEMON" "$CORE_UNIT" 2>/dev/null
  teardown_extras
  for n in 1 2 3 4 5 6 7 8; do ip link del "pxa$n" 2>/dev/null; done
  modprobe sch_netem
  $LAB/topology.sh --no-slots | tail -1 | indent
  syn_rule srvA; syn_rule srvB
  for s in srvC srvD srvE srvF; do add_server "$s"; done
  # A feed needs an address the fetcher's SSRF guard calls public. Lab routes keep it in the VM.
  ip -n web addr add 11.0.0.80/32 dev eth0
  ip -n inet route add 11.0.0.80/32 dev net
  ip route add 11.0.0.80/32 via 192.168.100.1 dev eth4 table 201
  dns_up
  write_feeds
  write_bootstrap
  # state.db and metrics.db start fresh, whatever schema an earlier run left, once the live page's are kept.
  mkdir -p "$RUN_DIR"
  keep_live
  rm -f "$LAB_HOME/$STATE_DB"* "$LAB_HOME/$METRICS_DB"* "$LAB_HOME/daemon.lock"
  rm -f "$RUN_DIR/view.json" "$RUN_DIR/clients.json" "$RUN_DIR/controllab.sock" "$RUN_DIR/core.log"
  rm -rf /run/lab-shim
  "$CTL" unit >/etc/systemd/system/$CORE_UNIT
  systemctl daemon-reload
}

world_base() {
  jq -n '{
    sources: [{id: "good"}, {id: "bad"}, {id: "solo"}],
    groups: [{id: "lab-eu", countries: ["DE"]}, {id: "lab-solo", include: "solo-E"}],
    slots: [{id: "eu", exit: "group:lab-eu"}, {id: "solo", exit: "group:lab-solo"}, {id: "pin", exit: "server:pin-F"}]
  }' >"$CONF/world.json"
}

# The base world behind a stand-in Network Application, which counts what it is sent.
world_direct() {
  world_base
  systemctl stop lab-unifi 2>/dev/null
  systemctl reset-failed lab-unifi 2>/dev/null
  (umask 077; echo lab-stand-in-key >"$LAB_HOME/$NETWORK_KEY")
  systemd-run --unit=lab-unifi --collect --quiet "$CTL" unifi
  until curl -sk -m 2 -o /dev/null https://127.0.0.1:8443/; do sleep 0.2; done
  write_bootstrap "$(jq -n --arg key "$LAB_HOME/$NETWORK_KEY" '{networkCredentialFile: $key, networkEndpoint: "https://127.0.0.1:8443/proxy/network"}')"
}

world_closeflows() { world_direct; }

world_three() {
  farm_up 5
  drop_base_feeds
  jq -n '{sources: [{id: "farm"}], slots: [{id: "a", exit: "server:farm-101"}, {id: "b", exit: "server:farm-101"}, {id: "c", exit: "server:farm-101"}]}' >"$CONF/world.json"
}

# Eight slots with groups that share nothing, so a round asks about 32 servers.
world_storm() {
  local i j labels groups="" slots=""
  farm_up 32
  drop_base_feeds
  for i in 1 2 3 4 5 6 7 8; do
    labels=""
    for j in 1 2 3 4; do labels="$labels,\"$(farm_label $(( (i - 1) * 4 + j )))\""; done
    groups="$groups,{\"id\":\"g$i\",\"labels\":[${labels#,}]}"
    slots="$slots,{\"id\":\"s$i\",\"exit\":\"group:g$i\"}"
  done
  echo "{\"sources\":[{\"id\":\"farm\"}],\"groups\":[${groups#,}],\"slots\":[${slots#,}]}" >"$CONF/world.json"
}

world_kept() {
  farm_up "$1"
  drop_base_feeds
  jq -n '{sources: [{id: "farm"}], groups: [{id: "all", include: "farm-"}], slots: [{id: "c", exit: "group:all"}]}' >"$CONF/world.json"
}
world_kept12() { world_kept 12; }
world_kept64() { world_kept 64; }

# Every root reaches the world through seven more hops, each its own proxy outbound, in a feed of
# 64 configs. Nothing holds these addresses: this world is about what the core holds.
world_memory() {
  drop_base_feeds
  python3 - "$UUID" >"$CONF/feeds/chains" <<'PY'
import json, sys
uuid = sys.argv[1]
def hop(tag, host, onward):
    stream = {"network": "tcp", "security": "tls", "tlsSettings": {"serverName": "chain.lab"}}
    if onward:
        stream["sockopt"] = {"dialerProxy": onward}
    return {"tag": tag, "protocol": "vless", "settings": {"vnext": [{"address": host, "port": 443, "users": [{"id": uuid, "encryption": "none"}]}]}, "streamSettings": stream}
configs = []
for root in range(64):
    tags = ["proxy"] + [f"h{i}" for i in range(1, 8)]
    hosts = [f"198.51.100.{100 + root}"] + [f"198.51.100.{170 + i}" for i in range(1, 8)]
    outbounds = [hop(tags[i], hosts[i], tags[i + 1] if i < 7 else None) for i in range(8)]
    configs.append({"remarks": f"DE chain-{root:02d}", "outbounds": outbounds})
print(json.dumps(configs))
PY
  jq -n '{sources: [{id: "chains"}], groups: [{id: "all", include: "chain-"}], slots: [{id: "c", exit: "group:all"}]}' >"$CONF/world.json"
}

# The feed is the shape of internal/catalogue/testdata/acme-shape.json, whose credentials are
# fake, on lab addresses with a Reality key made here, so its servers really serve.
acme_up() {
  local keys private public address clients names shorts
  keys=$(/usr/local/bin/xray x25519)
  private=$(awk -F': *' 'tolower($1) ~ /private/ {print $2; exit}' <<<"$keys")
  public=$(awk -F': *' 'tolower($1) ~ /public|password/ {print $2; exit}' <<<"$keys")
  python3 - "$LAB/acme-shape.json" "$public" >"$CONF/feeds/acme" <<'PY'
import json, re, sys
text = open(sys.argv[1], encoding="utf-8").read()
text = re.sub(r"node(\d+)\.fixture\.example\.net", lambda m: f"198.51.100.{200 + int(m.group(1))}", text)
text = re.sub(r"fake-reality-key-\d+", sys.argv[2], text)
print(json.dumps(json.loads(text), ensure_ascii=False))
PY
  add_namespace lew
  for address in $(jq -r '[.. | .address? // empty] | unique | .[]' "$CONF/feeds/acme") 198.51.100.240; do ip -n lew addr add "$address/24" dev eth0; done
  ip -n lew route add default via 198.51.100.1
  clients=$(jq -c '[.. | .users? // empty | .[].id] | unique | map({id: .})' "$CONF/feeds/acme")
  names=$(jq -c '[.. | .serverName? // empty] | unique' "$CONF/feeds/acme")
  shorts=$(jq -c '[.. | .shortId? // empty] | unique' "$CONF/feeds/acme")
  jq -n --argjson clients "$clients" --arg key "$private" --argjson names "$names" --argjson shorts "$shorts" '{
    log: {loglevel: "warning"},
    inbounds: [{listen: "0.0.0.0", port: 443, protocol: "vless", settings: {clients: $clients, decryption: "none"},
      streamSettings: {network: "raw", security: "reality", realitySettings: {target: "203.0.113.80:443", serverNames: $names, privateKey: $key, shortIds: $shorts}}}],
    outbounds: [{protocol: "freedom", settings: {finalRules: [{action: "allow", ip: ["203.0.113.0/24"]}]}}]
  }' >"$CONF/lew.json"
  start_xray lew
}

world_acme() {
  drop_base_feeds
  acme_up
  jq -n '{sources: [{id: "acme"}], groups: [{id: "lab-lew", include: "."}], slots: [{id: "lew", exit: "group:lab-lew"}]}' >"$CONF/world.json"
  jq 'del(.countryRules)' "$LAB_HOME/$BOOTSTRAP" >"$LAB_HOME/$BOOTSTRAP.new" && mv "$LAB_HOME/$BOOTSTRAP.new" "$LAB_HOME/$BOOTSTRAP"
}

daemon_start() {
  local path=()
  [ "${1:-}" = shimmed ] && path=(--setenv=PATH="$SHIM:/usr/sbin:/usr/bin:/sbin:/bin")
  systemd-run --unit=$DAEMON --collect --quiet -p KillMode=process "${path[@]}" "$CTL" serve
}

# The shim stands in front of coreutils' timeout, which runs every tool, to wedge one on request
# within the same bound. A flag in $SHIM_FLAGS asks for it, and the daemon must have started shimmed.
shim_install() {
  mkdir -p "$SHIM" "$SHIM_FLAGS"
  cat >"$SHIM/timeout" <<SHIMMED
#!/bin/sh
case "\$*" in
  *"run -test"*) [ -e $SHIM_FLAGS/stall-test ] && exec /usr/bin/timeout "\$1" "\$2" "\$3" sleep 300 ;;
  *"systemctl show"*)
    [ -e $SHIM_FLAGS/wedge ] && exec /usr/bin/timeout "\$1" "\$2" "\$3" sleep 300
    if [ -e $SHIM_FLAGS/wedge-once ]; then rm -f $SHIM_FLAGS/wedge-once; exec /usr/bin/timeout "\$1" "\$2" "\$3" sleep 300; fi ;;
esac
exec /usr/bin/timeout "\$@"
SHIMMED
  chmod +x "$SHIM/timeout"
}

daemon_restart() {
  systemctl kill -s KILL $DAEMON 2>/dev/null
  until [ "$(systemctl is-active $DAEMON)" != active ]; do sleep 0.1; done
  systemctl reset-failed $DAEMON 2>/dev/null
  daemon_start "${1:-}"
  # An empty change proves the new process listens, and it says so by refusing it.
  until [[ "$("$CTL" apply /dev/null 2>&1)" == *"Bad Request"* ]]; do sleep 0.2; done
}

# apply_world hands a JSON change of world to the running daemon, as the page's edits would.
apply_world() {
  local file rc; file=$(mktemp)
  echo "$1" >"$file"
  "$CTL" apply "$file"; rc=$?
  rm -f "$file"
  return $rc
}

# client_sync plays UniFi's part: its VPN Client takes the keys the daemon generated.
client_sync() {
  local slot=$1 n=$2 old private hostPublic endpoint
  private=$(jq -r --arg s "$slot" '.[$s].private' "$RUN_DIR/clients.json")
  hostPublic=$(jq -r --arg s "$slot" '.[$s].hostPublic' "$RUN_DIR/clients.json")
  endpoint=$(jq -r --arg s "$slot" '.[$s].endpoint' "$RUN_DIR/clients.json")
  for old in $(wg show "wgclt$n" peers); do wg set "wgclt$n" peer "$old" remove; done
  wg set "wgclt$n" private-key <(echo "$private") peer "$hostPublic" endpoint "$endpoint" allowed-ips 0.0.0.0/0 persistent-keepalive 25
}

client_sum() { jq -c --arg s "$1" '.[$s]' "$RUN_DIR/clients.json" | sha256sum | cut -c1-16; }

slots_serving() {
  [ -s "$RUN_DIR/view.json" ] || return 1
  [ "$(jq '[.slots[] | select(.status == "serving" and .runtime.phase == "ready")] | length' "$RUN_DIR/view.json" 2>/dev/null)" = "$1" ]
}

# use_world builds the named world unless it's already running. $2 is its slot count, $3 how long
# heartbeats get to build each server's own median afterwards, and $4 how many slots must serve.
use_world() {
  local name=$1 want=$2 warm=${3:-35} serving=${4:-$2} start
  if [ "$(cat "$STATE/world" 2>/dev/null)" = "$name" ] && [ "$(systemctl is-active $DAEMON)" = active ] && slots_serving "$serving"; then
    return 0
  fi
  note "building world $name"
  rm -f "$STATE/world"
  world_reset
  shim_install
  "world_$name"
  daemon_start
  start=$(date +%s)
  until [ -s "$RUN_DIR/clients.json" ] && [ "$(jq 'length' "$RUN_DIR/clients.json" 2>/dev/null)" = "$want" ]; do
    [ $(( $(date +%s) - start )) -gt 120 ] && { fail "slots never appeared"; journalctl -u $DAEMON -n 30 -o cat | indent; return 1; }
    sleep 0.5
  done
  if [ "$name" = base ] || [ "$name" = direct ] || [ "$name" = closeflows ] || [ "$name" = udpbroken ]; then client_sync eu 1; client_sync solo 2; fi
  if [ "$name" = oneslot ]; then client_sync eu 1; fi
  until slots_serving "$serving"; do
    [ $(( $(date +%s) - start )) -gt 120 ] && { fail "slots never opened"; jq -c '.slots[] | [.id, .status, .downReason, .runtime.phase, .runtime.errorCode]' "$RUN_DIR/view.json" | indent; journalctl -u $DAEMON -n 30 -o cat | indent; return 1; }
    sleep 0.5
  done
  note "$serving of $want slots serving $(( $(date +%s) - start )) s after an empty start, warming for $warm s"
  sleep "$warm"
  echo "$name" >"$STATE/world"
}

# step_setup leaves the base world's servers and the Acme feed ready with nothing running, for page.sh.
step_setup() {
  rm -f "$STATE/world"
  world_reset
  world_base
  acme_up
  expect "server count" "$(ip netns list | grep -c '^srv')" 6
}

# step_live, after setup, gives the live page the state its last run kept, or none with fresh, and
# marks what the daemon writes from then on as the live page's, for the next world reset to keep.
step_live() {
  if [ "${1:-}" = fresh ]; then
    rm -rf "$LIVE_KEPT"
  elif [ -e "$LIVE_KEPT/$STATE_DB" ]; then
    mv "$LIVE_KEPT"/* "$LAB_HOME"/ && rmdir "$LIVE_KEPT"
  fi
  touch "$LAB_HOME/live"
  if [ -e "$LAB_HOME/$STATE_DB" ]; then pass "the live page starts on the state its last run left"; else pass "the live page starts with empty state"; fi
}

step_start() {
  use_world base 3 0 || return
  [ "$(exit_of $SOLO)" = srvE ] && pass "solo exits via its only member" || fail "solo exits via '$(exit_of $SOLO)'"
  case "$(exit_of $EU)" in srvA|srvB|srvC|srvD) pass "eu exits via a DE server, $(exit_of $EU)";; *) fail "eu exits via '$(exit_of $EU)'";; esac
  "$CTL" switches | indent
}

# failover runs one case on slot eu. It faults whichever server serves, just after a heartbeat, and
# reports how long the slot took, with new traffic checked on the far side. $1 is the case's words,
# $2 the reason the event must carry, $3 what it does to the server, $4 what undoes it. $5, if given,
# is the case's bound in s, in place of the 150 s wait and the $LIMIT.
failover() {
  local what=$1 reason=$2 inject=$3 undo=$4 window=${5:-150} limit=${5:-$LIMIT} srv since t0 secs after
  srv=$(exit_of $EU); [ "$srv" = none ] && { sleep 2; srv=$(exit_of $EU); }
  [ "$srv" = none ] && { fail "$what: eu serves nothing to begin with"; return; }
  after_beat "$srv"
  since=$(now_iso); t0=$(date +%s.%N)
  note "eu on $srv: $what"
  "$inject" "$srv"
  if secs=$(wait_move "$since" eu "$window" "$t0" "reason=$reason"); then
    if at_most "$secs" "$limit"; then pass "$what: replaced in $secs s"; else fail "$what: replaced in $secs s, past the $limit s"; fi
    after=$(exit_of $EU)
    [ "$after" != "$srv" ] && [ "$after" != none ] && pass "$what: new traffic exits via $after" || fail "$what: new traffic exits via '$after'"
  else
    fail "$what: no $reason within $window s of the fault on $srv"
    journalctl -u $DAEMON --since "@${t0%.*}" -o cat | grep -E ' slot=eu |runtime' | cut -c21- | tail -n 12 | indent
  fi
  events_since "$since" eu | indent
  "$undo" "$srv"
  sleep 3
}

kill_server() { systemctl stop "lab-xray-$1"; }
revive_server() { start_xray "$1"; }
mute_server() { ip -n "$1" link set eth0 down; }
unmute_server() { ip -n "$1" link set eth0 up; ip -n "$1" route add default via 198.51.100.1 2>/dev/null; }
delay_server() { ip netns exec "$1" tc qdisc add dev eth0 root netem delay 300ms; }
lose_server() { ip netns exec "$1" tc qdisc add dev eth0 root netem loss 25%; }
clean_server() { ip netns exec "$1" tc qdisc del dev eth0 root; }
slowed='\(slownessMove\|failureMove\)'

# step_lowudp proves a slot carries UDP on a port below 1024, as DNS and NTP are. The core answers a
# TPROXY flow from the destination's own port, and binding that port takes CAP_NET_BIND_SERVICE.
step_lowudp() {
  local got
  use_world base 3 || return
  systemctl stop lab-udp-echo 2>/dev/null
  systemd-run --unit=lab-udp-echo --collect --quiet -p NetworkNamespacePath=/run/netns/web python3 -c '
import socket
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.bind(("203.0.113.81", 53))
while True:
    d, a = s.recvfrom(100)
    s.sendto(d, a)
'
  sleep 0.5
  got=$($LAN python3 -c '
import socket, sys
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.settimeout(3)
s.sendto(b"low-port", (sys.argv[1], 53))
print(s.recv(100).decode())
' "$EU" 2>&1 | tail -1)
  expect "a UDP reply from port 53 through the slot" "$got" "low-port"
  systemctl stop lab-udp-echo 2>/dev/null
}

step_kill() { use_world base 3 && failover "a killed server" failureMove kill_server revive_server; }
step_blackhole() { use_world base 3 && failover "a silent server" failureMove mute_server unmute_server; }
# Slowness reads a server against its own heartbeats, so the one serving needs three first, a minute apart.
step_degrade() { use_world base 3 && sleep 200 && failover "300 ms added" "$slowed" delay_server clean_server; }
# The probing plan's open point 2 gives slowness three heartbeats at 60 s to be noticed, and two rounds
# then move it: 200 s with the lab's overhead. 22% of heartbeats through 25% loss lose nothing, so two
# heartbeats can both look clean (TestTimelineLossWhoseFirstTwoHeartbeatsShowNothingMovesWithinTheLabWindow).
step_loss() { use_world base 3 && sleep 200 && failover "25% loss" "$slowed" lose_server clean_server 200; }

# traffic_start asks slot traffic for its exit several times a second, over new connections, into $2,
# one line per request: its start, its answer or none, and curl's exit status. A request waits 1 s
# for its answer.
traffic_start() {
  local target=$1 out=$2
  : >"$out"
  ( while :; do
      t=$(date +%s.%3N); r=$($LAN curl -s -m 1 "http://$target/ip"); echo "$t ${r:-none} $?" >>"$out"
      sleep 0.1
    done ) &
  TRAFFIC=$!
}
traffic_stop() { kill "$TRAFFIC" 2>/dev/null; wait "$TRAFFIC" 2>/dev/null; }
# traffic_report prints: longest gap between answers in s, requests, failures, those the close at
# epoch $2 reset, and the exits in order. A request begun before the close that ended without an
# answer and without timing out had its connection open when the close destroyed it.
traffic_report() {
  awk -v closed="${2:-0}" '{ n++
      if ($2 == "none") { if ($1 < closed && $3 != 28) c++; else f++ }
      else { if (last && $1 - last > gap) gap = $1 - last; last = $1; if ($2 != prev) { exits = exits " " $2; prev = $2 } } }
    END { printf "%.2f %d %d %d%s\n", gap, n, f, c, exits }' "$1"
}

step_exitchange() {
  local srv gate peers routes keys since t0 secs out dl download turned gap total failures cut exits closed
  use_world base 3 || return
  srv=$(exit_of $EU); gate=$(cat /sys/class/net/pxa1/carrier_changes)
  peers=$(wg show wgclt1 peers); routes=$(ip route show table 181); keys=$(client_sum eu)
  out=$(mktemp); dl=$(mktemp)
  note "eu on $srv. Exit changes to solo's server, which is ready, with traffic flowing and a 20 MB download open"
  $LAN curl -s -m 60 --limit-rate 2M -o /dev/null -w '%{size_download} %{http_code}' "http://$EU/bytes?n=20000000" >"$dl" &
  download=$!
  traffic_start $EU "$out"
  sleep 3
  since=$(now_iso); t0=$(date +%s.%N)
  apply_world '{"slots":[{"id":"eu","exit":"group:lab-solo"}]}' || fail "the Exit change was refused"
  if secs=$(wait_move "$since" eu 10 "$t0" "reason=exitChanged"); then pass "moved $secs s after the edit"; else fail "no exitChanged within 10 s"; fi
  closed=$(wait_applied "$since" eu 0 "$t0" "reason=exitChanged")
  turned=$(wait_exit $EU srvE 10) && pass "new connections exit via srvE ${turned} s later" || fail "eu exits via '$turned'"
  sleep 3
  traffic_stop
  read -r gap total failures cut exits <<<"$(traffic_report "$out" "$closed")"
  note "$total requests, $cut open at the close and reset by it, $failures failed, longest gap $gap s, exits: $exits"
  expect "requests unanswered within 1 s, but those the close reset" "$failures" 0
  at_most "$gap" 1.5 && pass "no gap in traffic past one SYN retry: longest $gap s between answers" || fail "a gap of $gap s in traffic through the slot"
  expect "pxa1 carrier changes" "$(cat /sys/class/net/pxa1/carrier_changes)" "$gate"
  expect "the VPN Client's peer" "$(wg show wgclt1 peers)" "$peers"
  expect "the routes in table 181" "$(ip route show table 181)" "$routes"
  expect "the sum of the slot's client keys and endpoint" "$(client_sum eu)" "$keys"
  wait "$download"
  read -r got _ <"$dl"
  [ "$got" -lt 20000000 ] && pass "the download open across the change was closed at $got of 20000000 bytes" || fail "the download survived the change: $(cat "$dl")"
  events_since "$since" eu | indent
  apply_world '{"slots":[{"id":"eu","exit":"group:lab-eu"}]}'
  rm -f "$out" "$dl"
  sleep 3
}

# swap_probe runs in the LAN until it is stopped. Every 1 ms it begins a TCP connect to $1:80, given
# 500 ms and closed with a reset, and sends a datagram carrying its number to $1:$2. Stopped, it
# writes to $3 "tcp START SECONDS ERRNO" per connect, SECONDS -1 for one that timed out, and
# "udp START ANSWERED" per datagram.
swap_probe() {
  exec $LAN python3 -c '
import collections, select, signal, socket, struct, sys, time
target, port, path = sys.argv[1], int(sys.argv[2]), sys.argv[3]
reset = struct.pack("ii", 1, 0)
ep = select.epoll()
u = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
u.connect((target, port))
u.setblocking(False)
ep.register(u.fileno(), select.EPOLLIN)
pending, order, tcp, sent, answered = {}, collections.deque(), [], [], set()
running = True
def stop(*_):
    global running
    running = False
signal.signal(signal.SIGTERM, stop)
due = time.monotonic()
while running:
    now = time.monotonic()
    if now >= due:
        due = max(due + 0.001, now)
        wall = time.time()
        s = socket.socket()
        s.setblocking(False)
        s.setsockopt(socket.SOL_SOCKET, socket.SO_LINGER, reset)
        s.connect_ex((target, 80))
        pending[s.fileno()] = (s, wall, now)
        order.append((s.fileno(), s))
        ep.register(s.fileno(), select.EPOLLOUT)
        try:
            u.send(struct.pack("!I", len(sent)))
        except OSError:
            pass
        sent.append(wall)
    while order:
        fd, s = order[0]
        mine = pending.get(fd, (None,))[0] is s
        if mine and time.monotonic() - pending[fd][2] <= 0.5:
            break
        order.popleft()
        if mine:
            tcp.append((pending.pop(fd)[1], -1, 0))
            ep.unregister(fd)
            s.close()
    for fd, _ in ep.poll(max(0, due - time.monotonic())):
        if fd == u.fileno():
            while True:
                try:
                    answered.add(struct.unpack("!I", u.recv(16)[:4])[0])
                except OSError:
                    break
            continue
        s, wall, began = pending.pop(fd)
        ep.unregister(fd)
        tcp.append((wall, time.monotonic() - began, s.getsockopt(socket.SOL_SOCKET, socket.SO_ERROR)))
        s.close()
with open(path, "w") as out:
    for wall, secs, err in tcp:
        print("tcp %.4f %.4f %d" % (wall, secs, err), file=out)
    for n, wall in enumerate(sent):
        print("udp %.4f %d" % (wall, n in answered), file=out)
' "$@"
}

# step_swapgap proves a move costs a connection opened during it no SYN retry. Twenty Exit changes
# while connects begin every 1 ms: none may take past 200 ms, where one retry takes a second. The
# prober must have run throughout: at least one connect begun per 2 ms over its life, and one per 4 ms
# over each move, from the edit to the flow close the journal logs after the pass, since the prober
# skips the beats it misses while the move's pass keeps the VM busy. Those floors can't show that a
# connect began inside a gap as short as the old listener's, a few ms, so each move's largest gap
# between connects begun is recorded and not judged: a healthy prober stalls past 20 ms between moves
# too. The datagrams a move loses, while the slot's UDP listener is replaced, are counted and not judged.
step_swapgap() {
  local dir probe status moves exits began since t0 starts to closes total slow timeouts worst lost i probing sparse per gaps
  use_world base 3 || return
  dir=$(mktemp -d)
  systemctl stop lab-udp-seq 2>/dev/null
  systemd-run --unit=lab-udp-seq --collect --quiet -p NetworkNamespacePath=/run/netns/web python3 -c '
import socket
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.bind(("203.0.113.81", 5354))
while True:
    d, a = s.recvfrom(100)
    s.sendto(d, a)
'
  sleep 0.5
  began=$(date +%s)
  probing=$(date +%s.%N)
  swap_probe $EU 5354 "$dir/probe" & probe=$!
  sleep 2
  moves=0; exits=""; starts=""
  for i in $(seq 20); do
    [ $(( i % 2 )) = 1 ] && to=lab-solo || to=lab-eu
    since=$(now_iso); t0=$(date +%s.%N); starts="$starts $t0"
    apply_world "{\"slots\":[{\"id\":\"eu\",\"exit\":\"group:$to\"}]}" || fail "Exit change $i was refused"
    wait_applied "$since" eu 10 "$t0" "reason=exitChanged" >/dev/null && moves=$((moves + 1))
    sleep 2
    exits="$exits $(exit_of $EU)"
  done
  sleep 1
  kill -TERM "$probe" 2>/dev/null; wait "$probe"; status=$?
  probing=$(awk -v a="$probing" -v n="$(date +%s.%N)" 'BEGIN { printf "%d", (n - a) * 1000 }')
  systemctl stop lab-udp-seq 2>/dev/null
  closes=$(journalctl -u $DAEMON --since "@$began" -o short-unix | awk '/runtime step: closed flows slot=eu / { printf "%s ", $1 }')
  expect "Exit changes applied" "$moves" 20
  note "exits after each:$exits"
  expect "flow closes on eu, one per move" "$(wc -w <<<"$closes")" 20
  expect "the prober's exit status" "$status" 0
  [ -s "$dir/probe" ] || fail "the prober left no record"
  read -r total slow timeouts worst <<<"$(awk '$1 == "tcp" { n++; if ($3 < 0) f++; else { if ($3 > 0.2) s++; if ($3 > w) w = $3 } } END { printf "%d %d %d %.4f\n", n, s, f, w }' "$dir/probe" 2>/dev/null)"
  note "TCP connects begun: ${total:-0} in $probing ms, over 200 ms: $slow, timed out at 500 ms: $timeouts, slowest answered: $worst s"
  if [ "${total:-0}" -ge $(( probing / 2 )) ]; then pass "connects begun, one per 2 ms of the prober's life = ${total:-0}"; else fail "connects begun = ${total:-0}, want at least $(( probing / 2 )), one per 2 ms of the prober's life"; fi
  read -r sparse per <<<"$(awk -v starts="$starts" -v closes="$closes" 'BEGIN { m = split(starts, a, " "); split(closes, c, " ") }
    $1 == "tcp" { for (j = 1; j <= m; j++) if ($2 >= a[j] && $2 <= c[j]) n[j]++ }
    END { short = ""; per = ""; for (j = 1; j <= m; j++) { ms = (c[j] - a[j]) * 1000; per = per sprintf("%d/%.0f,", n[j], ms); if (ms <= 0 || n[j] < ms / 4) short = short j "," } print (short == "" ? "none" : short), per }' "$dir/probe" 2>/dev/null)"
  note "connects begun per move, from the edit to the close, per ms: ${per:-none}"
  expect "moves with fewer than one connect begun per 4 ms" "${sparse:-unread}" none
  gaps=$(awk '$1 == "tcp" { print $2 }' "$dir/probe" | sort -n | awk -v starts="$starts" -v closes="$closes" 'BEGIN { m = split(starts, a, " "); split(closes, c, " ") }
    { for (j = 1; j <= m; j++) if ($1 >= a[j] && $1 <= c[j]) { if (j in last && $1 - last[j] > g[j]) g[j] = $1 - last[j]; last[j] = $1 } }
    END { line = ""; for (j = 1; j <= m; j++) line = line sprintf("%.1f ", g[j] * 1000); print line }')
  note "largest gap between connects begun, per move, in ms: ${gaps:-unread}"
  awk '$1 == "tcp" && ($3 < 0 || $3 > 0.2) { print "slow connect at", $2, $3 }' "$dir/probe" | head -20 | indent
  expect "connects past 200 ms or unanswered in 500 ms" "$(( ${slow:-0} + ${timeouts:-0} ))" 0
  expect "connects refused or reset" "$(awk '$1 == "tcp" && $4 != 0' "$dir/probe" | grep -c .)" 0
  lost=$(awk -v closes="$closes" 'BEGIN { m = split(closes, c, " ") }
    $1 == "udp" && $3 == 0 { k = 0; for (j = 1; j <= m; j++) if (c[j] <= $2 + 1) k = j; per[k]++ }
    $1 == "udp" { n++ }
    END { line = "sent " n ", lost before any close " per[0] + 0 ", per move:"; for (j = 1; j <= m; j++) line = line " " per[j] + 0; print line }' "$dir/probe")
  note "UDP datagrams, one per ms: $lost"
  rm -rf "$dir"
}

# step_heldmove is a move whose UDP relisten loses the port: a plain socket, no SO_REUSEPORT, binds in a
# tight loop and wins the gap between the old UDP inbound's removal and the new one's add. Xray lists
# the new tag though its bind failed, and the daemon removes it again, so the slot reads as not
# listening and its gate closes within a pass. A datagram the LAN sends through eu goes unanswered only
# on a link that is down, what a closed VPN Client gives, never on one that is up. Freed, the port binds
# on the next pass's add and the gate reopens.
step_heldmove() {
  local port dir hold since t0 won down up_none=0 sent=0 l r ms j
  rm -f "$STATE/world"
  use_world base 3 || return
  port=$(jq -r '.inbounds[] | select(.tag == "s.eu.udp") | .port' "$RUN_DIR/xray.json")
  [ -n "$port" ] && [ "$port" != null ] || { fail "eu has no UDP inbound in the core's config"; return; }
  systemctl stop lab-udp-echo 2>/dev/null
  systemd-run --unit=lab-udp-echo --collect --quiet -p NetworkNamespacePath=/run/netns/web python3 -c '
import socket
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.bind(("203.0.113.81", 5353))
while True:
    d, a = s.recvfrom(100)
    s.sendto(a[0].encode(), a)
'
  sleep 0.5
  expect "a datagram through eu before the move" "$(udp_exit $EU)" "$(exit_ip $EU)"
  dir=$(mktemp -d)
  python3 -c '
import socket, sys, time
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
while True:
    try:
        s.bind(("127.0.0.1", int(sys.argv[1])))
        break
    except OSError:
        pass
open(sys.argv[2], "w").write("%.3f" % time.time())
time.sleep(600)
' "$port" "$dir/won" & hold=$!
  sleep 1
  [ -e "$dir/won" ] && fail "the holder bound UDP $port before any move"
  note "eu on $(exit_of $EU), UDP $port contested by pid $hold"
  since=$(now_iso); t0=$(date +%s.%N)
  apply_world '{"slots":[{"id":"eu","exit":"group:lab-solo"}]}' || fail "the Exit change was refused"
  for _ in $(seq 150); do [ -e "$dir/won" ] && break; sleep 0.1; done
  if [ ! -e "$dir/won" ]; then
    fail "not exercised: the holder never won UDP $port"
    kill $hold; wait $hold 2>/dev/null; systemctl stop lab-udp-echo; rm -rf "$dir"; return
  fi
  won=$(cat "$dir/won")
  pass "the holder won UDP $port $(awk -v a="$t0" -v b="$won" 'BEGIN { printf "%.2f", b - a }') s after the edit"
  ss -Hlunp "sport = :$port" | grep -q "pid=$(core_pid)," && fail "the core holds UDP $port beside the holder" || pass "UDP $port is the holder's alone"
  j=""
  for _ in $(seq 50); do
    journalctl -u $DAEMON --since "@${t0%.*}" -o cat | grep -q "listen udp 127.0.0.1:$port: bind: address already in use" && { j=1; break; }
    sleep 0.2
  done
  [ -n "$j" ] && pass "the relisten's bind error is in the journal" || fail "the relisten's bind error went unreported for 10 s"
  if down=$(wait_link pxa1 down 12); then pass "pxa1 down $down s after the holder won"; else fail "pxa1 still up 12 s after the holder won"; fi
  eventually "eu's runtime phase" eu '.runtime.phase' closed
  for _ in $(seq 10); do
    l=$(link_up pxa1); r=$(udp_exit $EU); sent=$((sent + 1))
    [ "$r" = none ] && [ "$l" = up ] && [ "$(link_up pxa1)" = up ] && up_none=$((up_none + 1))
    sleep 0.5
  done
  expect "datagrams of $sent through eu unanswered while pxa1 was up" "$up_none" 0
  kill $hold; wait $hold 2>/dev/null
  if ms=$(wait_link pxa1 up 25); then pass "pxa1 up $ms s after the port was freed"; else fail "pxa1 still down 25 s after the port was freed"; fi
  ss -Hlunp "sport = :$port" | grep -q "pid=$(core_pid)," && pass "UDP $port is the core's" || fail "the core holds no UDP socket on $port"
  r=""; for _ in $(seq 20); do r=$(udp_exit $EU); [ "$r" = "${IP[srvE]}" ] && break; sleep 0.5; done
  expect "a datagram through eu once the port was free" "$r" "${IP[srvE]}"
  since=$(now_iso); t0=$(date +%s.%N)
  apply_world '{"slots":[{"id":"eu","exit":"group:lab-eu"}]}' || fail "the Exit change back was refused"
  wait_applied "$since" eu 10 "$t0" "reason=exitChanged" >/dev/null || fail "eu did not move back"
  systemctl stop lab-udp-echo 2>/dev/null
  rm -rf "$dir"
}

# wait_exit_ip polls until slot traffic through $1 answers with an exit address $2 accepts, as a
# grep pattern, and prints how long that took in s.
wait_exit_ip() {
  local target=$1 pattern=$2 limit=${3:-10} t0 got
  t0=$(date +%s.%N)
  while :; do
    got=$(exit_ip "$target" 1)
    grep -qE "$pattern" <<<"$got" && { elapsed "$t0"; return 0; }
    past "$t0" "$limit" && { echo "$got"; return 1; }
    sleep 0.05
  done
}
# eventually is expect on a slot's field in the view, which is published every few seconds.
eventually() {
  local what=$1 slot=$2 field=$3 want=$4 t0 got
  t0=$(date +%s.%N)
  while got=$(slot_field "$slot" "$field"); [ "$got" != "$want" ] && ! past "$t0" 8; do sleep 0.2; done
  expect "$what" "$got" "$want"
}
unifi_writes() { curl -sk https://127.0.0.1:8443/lab/writes | jq -r 'length'; }
set_exit() { apply_world "{\"slots\":[{\"id\":\"eu\",\"exit\":\"$1\"}]}"; }

# An open connection, kept alive, asks where it exits before and after the trigger file appears.
open_flow() {
  $LAN python3 -c '
import http.client, os, sys, time
c = http.client.HTTPConnection(sys.argv[1], timeout=20)
def ask():
    c.request("GET", "/ip")
    return c.getresponse().read().decode()
print(ask(), flush=True)
while not os.path.exists(sys.argv[2]):
    time.sleep(0.05)
try:
    print(ask(), flush=True)
except Exception as e:
    print("closed:", e, flush=True)
' "$1" "$2"
}

# A datagram through the slot, answered with the address the target saw.
udp_exit() {
  $LAN python3 -c '
import socket, sys
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.settimeout(2)
s.sendto(b"x", (sys.argv[1], 5353))
try:
    print(s.recv(100).decode())
except OSError:
    print("none")
' "$1"
}

# flow_tool runs in the LAN and logs when a flow's exit changes and how the flow ends, to $3:
# tcp: one connection asking /ip. dl: one big download, with a small receive buffer so the end
# shows when it happens rather than when a buffer drains. udp: one socket, one 5-tuple, 10 datagrams
# a second, the shape QUIC has. udpidle: the same, silent from when $4 appears until $5 s later.
flow_tool() {
  exec $LAN python3 -c '
import os, signal, socket, sys, time
mode, target, path = sys.argv[1:4]
out = open(path, "w", buffering=1)
def log(*a): print("%.3f" % time.time(), *a, file=out)
def stop(*_):
    log("stopped")
    os._exit(0)
signal.signal(signal.SIGTERM, stop)
if mode == "dl":
    s = socket.socket()
    s.setsockopt(socket.SOL_SOCKET, socket.SO_RCVBUF, 131072)
    s.connect((target, 80))
    s.sendall(b"GET /bytes?n=2000000000 HTTP/1.1\r\nHost: lab\r\n\r\n")
    got = 0
    try:
        while True:
            b = s.recv(65536)
            if not b:
                log("closed eof", got); break
            got += len(b)
            time.sleep(0.01)
    except OSError as e:
        log("closed", type(e).__name__, got)
elif mode == "tcp":
    s = socket.create_connection((target, 80), timeout=5)
    prev = None
    try:
        while True:
            s.sendall(b"GET /ip HTTP/1.1\r\nHost: lab\r\n\r\n")
            r = s.recv(4096)
            if not r:
                log("closed eof"); break
            ip = r.decode().rsplit("\r\n\r\n", 1)[-1].strip()
            if ip != prev:
                log("exit", ip); prev = ip
            time.sleep(0.1)
    except OSError as e:
        log("closed", type(e).__name__)
else:
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.settimeout(0.5)
    s.connect((target, 5353))
    prev, sent, lost, quiet = None, 0, 0, False
    while True:
        if mode == "udpidle" and not quiet and os.path.exists(sys.argv[4]):
            quiet = True
            log("silent")
            time.sleep(float(sys.argv[5]))
            log("speaking")
        try:
            s.send(b"x"); sent += 1
            ip = s.recv(100).decode()
            if ip != prev:
                log("exit", ip, "sent", sent, "lost", lost); prev = ip
        except OSError:
            lost += 1
            log("lost", lost, "of", sent)
        time.sleep(0.1)
' "$@"
}
flow_first() { awk -v w="$2" '$2 == w { print $1; exit }' "$1"; }
flow_lost() { awk '$2 == "lost" { n++ } END { print n + 0 }' "$1"; }
flow_last_exit() { awk '$2 == "exit" { e = $3 } END { print e }' "$1"; }
# upstreams lists the core's connections to $1, by local port.
upstreams() { ss -tnp state established dst "$1" | grep -F "pid=$(core_pid)," | awk '{ n = split($3, a, ":"); print a[n] }' | sort; }
marked() { ss -tnpe state established | grep -F "pid=$(core_pid)," | grep -c "fwmark:$1"; }
secs_between() { awk -v a="$1" -v b="$2" 'BEGIN { if (a == "" || b == "") print "-"; else printf "%.2f", b - a }'; }
flows_stop() { local p; for p in "$@"; do kill "$p" 2>/dev/null; wait "$p" 2>/dev/null; done; }

# close_row runs one trigger against an open TCP flow, a big download and a UDP flow through eu,
# and prints one table row. $1 names it, $2 is the event it must log, $3 whether flows must close,
# $4 the address new connections should see (a pattern), then the trigger and its arguments.
close_row() {
  local name=$1 reason=$2 want=$3 pattern=$4; shift 4
  local dir tcp dl udp since t0 secs applied old_ip old_up gate shake tend dend uat recon rat exit_new after released carrier handshake moves got
  dir=$(mktemp -d); old_ip=$(exit_ip $EU)
  flow_tool tcp $EU "$dir/tcp" & tcp=$!
  flow_tool dl $EU "$dir/dl" & dl=$!
  flow_tool udp $EU "$dir/udp" & udp=$!
  sleep 2
  note "--- $name: open flows exit via $old_ip, eu carries $(marked 0x16a) TCP sockets with its mark"
  old_up=$(upstreams "$ROW_OLD_DST")
  gate=$(cat /sys/class/net/pxa1/carrier_changes); shake=$(wg show pxa1 latest-handshakes)
  since=$(now_iso); t0=$(date +%s.%N)
  note "trigger at $t0"
  "$@"
  secs=$(wait_move "$since" eu 900 "$t0" "reason=$reason") || { fail "$name: no $reason within 900 s"; flows_stop $tcp $dl $udp; return; }
  applied=$(events_since "$since" eu | grep -v 'applied=unapplied' | grep -- "reason=$reason" | head -1 | sed -E 's/.* applied=([0-9.]+) .*/\1/')
  local until; until=$(date +%s.%N)
  while [ "$want" = close ] && [ -z "$(flow_first "$dir/tcp" closed)" ] && ! past "$until" 5; do sleep 0.01; done
  rat=$(date +%s.%N); got=$(exit_ip $EU 3); recon=$(secs_between "$rat" "$(date +%s.%N)")
  exit_new=${SRV[$got]:-$got}
  sleep 2
  tend=$(flow_first "$dir/tcp" closed); dend=$(flow_first "$dir/dl" closed)
  uat=$(awk -v ip="$(flow_last_exit "$dir/udp")" -v o="$old_ip" '$2 == "exit" && $3 == ip && ip != o { print $1; exit }' "$dir/udp")
  after=$(upstreams "$ROW_OLD_DST"); released=$(comm -23 <(echo "$old_up") <(echo "$after") | grep -c .)
  carrier=$(cat /sys/class/net/pxa1/carrier_changes); handshake=$(wg show pxa1 latest-handshakes)
  [[ "$got" =~ $pattern ]] && pass "$name: a reconnect at once exits via $exit_new, $recon s" || fail "$name: a reconnect exits via '$got', want $pattern"
  if [ "$want" = close ]; then
    [ -n "$tend" ] && pass "$name: the TCP flow closed $(secs_between "$t0" "$tend") s after the trigger ($(awk '$2 == "closed" { print $3 }' "$dir/tcp"))" || fail "$name: the TCP flow is still open"
    [ -n "$dend" ] && pass "$name: the download ended $(secs_between "$t0" "$dend") s after the trigger, $(awk '$2 == "closed" { print $NF }' "$dir/dl") bytes in" || fail "$name: the download is still open"
    [ "$(flow_last_exit "$dir/udp")" != "$old_ip" ] && pass "$name: the UDP flow took $(flow_last_exit "$dir/udp") $(secs_between "$t0" "$uat") s after the trigger" || fail "$name: the UDP flow stayed on $old_ip"
  else
    [ -z "$tend" ] && [ -z "$dend" ] && pass "$name: the TCP flow and the download stay open" || fail "$name: a flow closed: tcp '$tend' dl '$dend'"
    expect "$name: the TCP flow's exit" "$(flow_last_exit "$dir/tcp")" "$old_ip"
    expect "$name: the UDP flow's exit" "$(flow_last_exit "$dir/udp")" "$old_ip"
  fi
  expect "$name: pxa1 carrier changes" "$carrier" "$gate"
  [ "$handshake" = "$shake" ] && pass "$name: pxa1's handshake is the one from before" || at_most "$(( $(date +%s) - $(awk '{ print $2 }' <<<"$handshake") ))" 180 && pass "$name: pxa1 keeps a live handshake, rekeyed on its own clock" || fail "$name: pxa1's handshake went stale: $handshake"
  sleep 8
  moves=$(events_since "$since" eu | grep -cE 'reason=(failureMove|slownessMove)')
  [ "$reason" = failureMove ] && moves=$(( moves - 1 ))
  expect "$name: false failure or slowness moves on eu" "$moves" 0
  expect "$name: moves on solo" "$(count_since "$since" solo)" 0
  ROW_TABLE+=$'\n'"$name | $want | $(secs_between "$t0" "$applied") | tcp $(secs_between "$t0" "$tend") | dl $(secs_between "$t0" "$dend") | $exit_new in $recon | udp $(flow_last_exit "$dir/udp") at $(secs_between "$t0" "$uat"), lost $(flow_lost "$dir/udp") | $released"
  flows_stop $tcp $dl $udp
  rm -rf "$dir"
}

control_still_open() {
  local what=$1 file=$2 want=$3
  grep -q ' closed' "$file" && fail "$what closed: $(grep ' closed' "$file")" || pass "$what stayed open"
  expect "$what, exit" "$(flow_last_exit "$file")" "$want"
}

# step_closeflows proves a slot's move closes its own open connections, or leaves them, and touches
# nothing else: not the other slot, not a flow outside slots, not the VPN Client outside the product.
step_closeflows() {
  local dir c_tcp c_udp c_out c_vpn c_ip began srv lineage killed t0 gone udpidle before old stale tcp dl tend dend
  use_world closeflows 3 35 || return
  systemctl stop lab-udp-echo 2>/dev/null
  systemd-run --unit=lab-udp-echo --collect --quiet -p NetworkNamespacePath=/run/netns/web python3 -c '
import select, socket
socks = [socket.socket(socket.AF_INET, socket.SOCK_DGRAM) for _ in range(3)]
for s, a in zip(socks, ["203.0.113.81", "203.0.113.82", "203.0.113.83"]):
    s.bind((a, 5353))
while True:
    for s in select.select(socks, [], [])[0]:
        d, a = s.recvfrom(100)
        s.sendto(a[0].encode(), a)
'
  sleep 0.5
  dir=$(mktemp -d); began=$(now_ms); ROW_TABLE=""; ROW_OLD_DST=""
  c_ip=$(exit_ip $SOLO)
  flow_tool tcp $SOLO "$dir/solo-tcp" & c_tcp=$!
  flow_tool udp $SOLO "$dir/solo-udp" & c_udp=$!
  flow_tool tcp 203.0.113.80 "$dir/outside-tcp" & c_out=$!
  flow_tool tcp 203.0.113.83 "$dir/client3-tcp" & c_vpn=$!
  sleep 1
  note "controls: slot solo on $c_ip, one flow to the WAN outside slots, one through wgclt3, which no slot owns"

  srv=$(exit_of $EU); ROW_OLD_DST=${IP[$srv]}
  close_row "Exit change" exitChanged close '^198\.51\.100\.60$' apply_world '{"slots":[{"id":"eu","exit":"group:lab-solo"}]}'
  apply_world '{"slots":[{"id":"eu","exit":"group:lab-eu"}]}'; sleep 3

  srv=$(exit_of $EU); ROW_OLD_DST=${IP[$srv]}
  close_row "Direct on" exitChanged close "^$WAN_IP\$" set_exit direct
  ROW_OLD_DST=$EU
  close_row "Direct off" exitChanged close '^198\.51\.100\.' set_exit group:lab-eu
  sleep 3

  srv=$(exit_of $EU); ROW_OLD_DST=${IP[$srv]}; killed=$srv
  lineage=$(slot_field eu .server)
  after_beat "$srv"
  close_row "Killed server" failureMove close '^198\.51\.100\.' kill_server "$srv"
  revive_server "$srv"
  sleep 3

  srv=$(exit_of $EU); old=${IP[$srv]}
  flow_tool tcp $EU "$dir/pause-tcp" & tcp=$!
  flow_tool dl $EU "$dir/pause-dl" & dl=$!
  sleep 2
  t0=$(date +%s.%N)
  set_exit block
  sleep 3
  tend=$(flow_first "$dir/pause-tcp" closed); dend=$(flow_first "$dir/pause-dl" closed)
  [ -n "$tend" ] && [ -n "$dend" ] && pass "Block closed both flows: tcp $(secs_between "$t0" "$tend") s, download $(secs_between "$t0" "$dend") s" || fail "Block left a flow open: tcp '$tend' download '$dend'"
  expect "sockets with eu's mark while blocked" "$(marked 0x16a)" 0
  ROW_TABLE+=$'\n'"Block | close | - | tcp $(secs_between "$t0" "$tend") | dl $(secs_between "$t0" "$dend") | dropped | -"
  flows_stop $tcp $dl
  set_exit group:lab-eu
  wait_exit_ip $EU '^198\.51\.100\.' 30 >/dev/null || fail "eu never came back from Block"
  sleep 2

  srv=$(exit_of $EU); old=${IP[$srv]}; before=$(upstreams "$old")
  flow_tool udp $EU "$dir/udp-gone" & udpidle=$!
  sleep 2
  stale=$(comm -13 <(echo "$before") <(upstreams "$old"))
  note "one UDP flow on $srv holds upstream port $(echo $stale)"
  apply_world '{"slots":[{"id":"eu","exit":"group:lab-solo"}]}'
  sleep 1
  expect "a moved UDP flow" "$(flow_last_exit "$dir/udp-gone")" 198.51.100.60
  flows_stop $udpidle
  t0=$(date +%s.%N); gone=""
  while [ -z "$gone" ] && ! past "$t0" 640; do
    [ -z "$(comm -12 <(echo "$stale") <(upstreams "$old"))" ] && gone=$(elapsed "$t0")
    sleep 5
  done
  [ -n "$gone" ] && pass "the core dropped the moved UDP session's upstream socket $gone s after its last datagram" || fail "the moved UDP session's upstream socket outlived 640 s"
  ROW_TABLE+=$'\n'"UDP upstream of a moved session | released ${gone:-never} s after its last datagram"
  apply_world '{"slots":[{"id":"eu","exit":"group:lab-eu"}]}'; sleep 3

  control_still_open "slot solo's TCP flow" "$dir/solo-tcp" "$c_ip"
  control_still_open "slot solo's UDP flow" "$dir/solo-udp" "$c_ip"
  expect "slot solo's UDP datagrams lost" "$(flow_lost "$dir/solo-udp")" 0
  control_still_open "the flow outside slots" "$dir/outside-tcp" "$WAN_IP"
  control_still_open "the flow through wgclt3" "$dir/client3-tcp" "$(awk '$2 == "exit" { print $3; exit }' "$dir/client3-tcp")"
  flows_stop $c_tcp $c_udp $c_out $c_vpn
  note "probes in the step, excluding the killed $killed: $(metrics_sql 'SELECT count(*), coalesce(sum(verdict = 2 AND wan_excluded = 0), 0) FROM probe WHERE ts >= ? AND lineage != ?' "$began" "$lineage")"
  expect "probes that blamed a server other than the killed one" "$(metrics_sql 'SELECT count(*) FROM probe WHERE ts >= ? AND lineage != ? AND verdict = 2 AND wan_excluded = 0' "$began" "$lineage")" 0
  note "ROW name | wanted | applied | tcp ends | download ends, each s after the trigger | reconnect exit in s | udp exit at s, datagrams lost | old upstream sockets released"
  echo "$ROW_TABLE" | indent
  systemctl stop lab-udp-echo 2>/dev/null
  rm -rf "$dir"
}

# step_direct flips slot eu to the WAN and back with a real core, and through every failure that
# must not close it. UniFi's side stays as it was, and the stand-in counts what it is sent.
step_direct() {
  local srv ip pid gate peers routes keys writes since t0 secs out flow trigger echoer gap total failures exits moved dl download
  use_world direct 3 || return
  [ "$(slot_field eu .runtime.networkId)" != null ] && pass "eu has a VPN Client in the stand-in" || fail "eu has no VPN Client"
  srv=$(exit_of $EU); ip=${IP[$srv]:-none}
  expect "the slot on VPN exits at its server's address" "$(exit_ip $EU)" "$ip"
  note "eu on $srv at $ip"
  pid=$(core_pid); gate=$(cat /sys/class/net/pxa1/carrier_changes); peers=$(wg show wgclt1 peers); routes=$(ip route show table 181); keys=$(client_sum eu); writes=$(unifi_writes)
  note "the stand-in has taken $writes writes, all of them creating the clients"
  out=$(mktemp); dl=$(mktemp); flow=$(mktemp); trigger=$(mktemp -u)
  systemd-run --unit=lab-udp-echo --collect --quiet -p NetworkNamespacePath=/run/netns/web python3 -c '
import socket
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.bind(("203.0.113.81", 5353))
while True:
    d, a = s.recvfrom(100)
    s.sendto(a[0].encode(), a)
'
  sleep 0.5
  expect "a datagram on VPN exits at the server's address" "$(udp_exit $EU)" "$ip"
  open_flow $EU "$trigger" >"$flow" &
  $LAN curl -s -m 60 --limit-rate 2M -o /dev/null -w '%{size_download} %{http_code}' "http://$EU/bytes?n=20000000" >"$dl" &
  download=$!
  traffic_start $EU "$out"
  sleep 2

  since=$(now_iso); t0=$(date +%s.%N)
  set_exit direct || fail "the flip to Direct was refused"
  if secs=$(wait_exit_ip $EU "^$WAN_IP\$" 5); then
    at_most "$(elapsed "$t0")" 2 && pass "new connections see the WAN address $WAN_IP, $(elapsed "$t0") s after the flip" || fail "new connections saw the WAN only after $(elapsed "$t0") s"
  else fail "new connections never reached the WAN, last '$secs'"; fi
  sleep 1
  expect "a datagram on Direct exits at the WAN address" "$(udp_exit $EU)" "$WAN_IP"
  eventually "the slot's status on Direct" eu .status direct
  eventually "the slot's Exit" eu .exit.kind direct
  eventually "what the runtime reports applied" eu .runtime.applied.kind direct
  expect "the event a flip leaves" "$(events_since "$since" eu | grep -c 'reason=exitChanged')" 1
  touch "$trigger"; sleep 1
  note "an open connection asked its exit before and after the flip: $(tr '\n' ' ' <"$flow")"
  grep -q '^closed:' "$flow" && pass "the flip closed an open connection" || fail "the open connection's exit after the flip: $(cat "$flow")"
  wait "$download"
  read -r got _ <"$dl"
  [ "$got" -lt 20000000 ] && pass "the flip closed a download at $got of 20000000 bytes" || fail "a download survived the flip: $(cat "$dl")"
  expect "the core's PID across the flip" "$(core_pid)" "$pid"
  expect "pxa1 carrier changes across the flip" "$(cat /sys/class/net/pxa1/carrier_changes)" "$gate"
  expect "pxa1" "$(link_up pxa1)" up
  expect "the VPN Client's peer" "$(wg show wgclt1 peers)" "$peers"
  expect "the routes in table 181" "$(ip route show table 181)" "$routes"
  expect "the sum of the slot's client keys and endpoint" "$(client_sum eu)" "$keys"
  expect "writes the stand-in took for the flip" "$(unifi_writes)" "$writes"

  sleep 1
  traffic_stop
  read -r gap total failures _ exits <<<"$(traffic_report "$out")"
  note "from before the flip: $total requests, $failures failed, longest gap $gap s, exits: $exits"
  expect "requests that failed through the flip" "$failures" 0

  t0=$(date +%s.%N)
  set_exit group:lab-eu || fail "the flip back was refused"
  if secs=$(wait_exit_ip $EU '^198\.51\.100\.' 5); then
    at_most "$(elapsed "$t0")" 2 && pass "back on VPN, new connections reach a server $(elapsed "$t0") s after the flip" || fail "the return took $(elapsed "$t0") s"
  else fail "new connections never left the WAN, last '$secs'"; fi
  expect "the server the slot returned to, by the view" "$(exit_of $EU)" "${SRV[$(exit_ip $EU)]}"
  eventually "the slot's status back on VPN" eu .status serving
  expect "the core's PID through the return" "$(core_pid)" "$pid"
  expect "pxa1 carrier changes through the return" "$(cat /sys/class/net/pxa1/carrier_changes)" "$gate"
  set_exit direct || fail "couldn't go back to Direct"
  sleep 3

  since=$(now_iso); gate=$(cat /sys/class/net/pxa1/carrier_changes)
  note "a daemon killed and restarted while on Direct"
  traffic_start $EU "$out"
  daemon_restart
  sleep 25
  traffic_stop
  read -r gap total failures _ exits <<<"$(traffic_report "$out")"
  note "across the restart: $total requests, $failures failed, longest gap $gap s, exits: $exits"
  expect "requests that failed across the restart" "$failures" 0
  expect "the exits across the restart" "$exits" "$WAN_IP"
  eventually "the status after the restart" eu .status direct
  expect "the core's PID across the restart" "$(core_pid)" "$pid"
  expect "pxa1 carrier changes across the restart" "$(cat /sys/class/net/pxa1/carrier_changes)" "$gate"

  set_exit block || fail "Block was refused"
  sleep 2
  got=$(exit_ip $EU 2)
  [ -z "$got" ] && pass "no traffic through a blocked slot" || fail "blocked traffic reached the web as '$got'"
  expect "pxa1 on Block" "$(link_up pxa1)" up
  eventually "the status on Block" eu .status block
  expect "the VPN Client's peer on Block" "$(wg show wgclt1 peers)" "$peers"
  set_exit direct || fail "the way back to Direct was refused"
  secs=$(wait_exit_ip $EU "^$WAN_IP\$" 15) && pass "Block back to Direct reaches the WAN, $secs s on" || fail "after Block: '$secs'"

  # A private address the main table doesn't hold, so UniFi's mark carries it into the slot as it would a route's match.
  ip -n web addr add 10.77.0.1/32 dev eth0
  ip -n inet route add 10.77.0.1/32 dev net
  ip route add 10.77.0.1/32 via 192.168.100.1 dev eth4 table 201
  systemd-run --unit=lab-private-web --collect --quiet -p NetworkNamespacePath=/run/netns/web python3 -m http.server 8099 --bind 10.77.0.1
  sleep 1
  iptables -t mangle -I UBIOS_PREROUTING_USER_HOOK -d 10.77.0.1/32 -j MARK --set-xmark 0x6a0000/0x7f0000
  expect "the gateway's own socket reaching a private address, as a control" "$(curl -s -m 3 -o /dev/null -w '%{http_code}' http://10.77.0.1:8099/)" 200
  got=$($LAN curl -s -m 3 -o /dev/null -w '%{http_code}' http://10.77.0.1:8099/)
  [ "$got" != 200 ] && pass "a client through a slot on Direct doesn't reach it (answer '$got')" || fail "a client reached a private address through Direct"
  iptables -t mangle -D UBIOS_PREROUTING_USER_HOOK -d 10.77.0.1/32 -j MARK --set-xmark 0x6a0000/0x7f0000
  systemctl stop lab-private-web lab-udp-echo 2>/dev/null
  ip route del 10.77.0.1/32 via 192.168.100.1 dev eth4 table 201
  ip -n inet route del 10.77.0.1/32 dev net
  ip -n web addr del 10.77.0.1/32 dev eth0

  set_exit group:lab-eu || fail "couldn't go back to VPN"
  sleep 3
  expect "writes the stand-in took through every flip, block and restart" "$(unifi_writes)" "$writes"
  curl -sk https://127.0.0.1:8443/proxy/network/v2/api/site/default/trafficroutes | jq -c '[.[] | .kill_switch_enabled]' | sed 's/^/       the routes still hold /'
  rm -f "$out" "$dl" "$flow" "$trigger"
}

# A server that will never answer, since nothing holds its address.
add_dead_server() {
  feed_line srvF "NL dead-G" 198.51.100.99 >>"$CONF/feeds/solo"
  touch "$RUN_DIR/refresh"
  apply_world '{"groups":[{"id":"lab-dead","labels":["dead-G"]}]}'
}
remove_dead_server() {
  apply_world '{"deleteGroups":["lab-dead"]}'
  sed -i '/dead-G/d' "$CONF/feeds/solo"
  touch "$RUN_DIR/refresh"
}

# The change has nothing ready to move to, so the slot bridges on its old server and closes at the
# deadline. A further edit and a daemon restart inside the window renew nothing.
step_deadline() {
  local srv since t0 out closed answers back
  use_world base 3 || return
  add_dead_server || { fail "the dead server's group was refused"; return; }
  sleep 5
  srv=$(exit_of $EU); out=$(mktemp)
  note "eu on $srv. Exit changes to a group with only a dead server, with traffic flowing"
  traffic_start $EU "$out"
  sleep 2
  since=$(now_iso); t0=$(date +%s.%N)
  apply_world '{"slots":[{"id":"eu","exit":"group:lab-dead"}]}' || fail "the Exit change was refused"
  sleep 10
  [ -n "$(slot_field eu '.leavingBy // empty')" ] && pass "the slot says it's leaving, by $(slot_field eu .leavingBy)" || fail "no leavingBy 10 s in"
  apply_world '{"groups":[{"id":"lab-dead","labels":["dead-G"]}]}' || fail "the further edit was refused"
  note "a further edit 10 s in"
  sleep 5
  daemon_restart
  note "a daemon killed and restarted 15 s in"
  if wait_link pxa1 down 25 >/dev/null; then
    closed=$(elapsed "$t0")
    if at_most "$closed" 33 && ! at_most "$closed" 27; then pass "the gate closed $closed s after the edit, with nothing ready"; else fail "the gate closed $closed s after the edit, want 30 s"; fi
  else
    fail "the gate never closed: leavingBy $(slot_field eu .leavingBy)"
  fi
  answers=$(awk -v a="$t0" '$1 >= a + 1 && $1 < a + 25 && $2 != "none" { n++ } END { print n + 0 }' "$out")
  [ "$answers" -gt 20 ] && pass "$answers answers through the bridge before the deadline" || fail "only $answers answers through the bridge"
  sleep 3
  traffic_stop
  expect "answers begun over a second after the close" "$(awk -v a="$t0" -v c="$closed" '$1 > a + c + 1 && $2 != "none"' "$out" | wc -l)" 0
  events_since "$since" eu | indent
  apply_world '{"slots":[{"id":"eu","exit":"group:lab-eu"}]}'
  back=$(wait_link pxa1 up 60) && pass "the gate reopened $back s after the Exit came back" || fail "the gate stayed closed after the Exit came back"
  remove_dead_server
  rm -f "$out"
  sleep 3
}

# The core's config test stalls, every run, for its whole bound, while a server dies and a feed
# moves another. Validation holds up neither, and a server moved past its deadline has only the
# replacement to open.
step_stalltest() {
  local srv since t0 secs moved closed out back label requested before
  use_world base 3 || return
  daemon_restart shimmed
  sleep 30
  touch "$SHIM_FLAGS/stall-test"
  srv=$(exit_of $EU)
  note "xray run -test stalls. A new server arrives and stays unjudged, and eu's server $srv dies"
  feed_line srvF "NL new-H" 198.51.100.98 >>"$CONF/feeds/solo"
  touch "$RUN_DIR/refresh"
  sleep 3
  after_beat "$srv"
  since=$(now_iso); t0=$(date +%s.%N)
  kill_server "$srv"
  if secs=$(wait_move "$since" eu 120 "$t0" "reason=failureMove"); then
    at_most "$secs" $LIMIT && pass "the failover landed in $secs s with validation stalled" || fail "the failover took $secs s with validation stalled"
  else
    fail "no failover within 120 s with validation stalled"
  fi
  revive_server "$srv"
  sleep 2
  moved=$(exit_of $EU)
  label=$(slot_label eu)
  [ -n "$label" ] && [ "$moved" != none ] || { fail "eu serves '$moved', '$label', after the failover"; return; }
  note "eu on $moved, '$label'. The feed moves that server to srvG's address, so its new revision can't be judged"
  add_server srvG "$moved"
  sed -i "/${label#* }/d" "$CONF/feeds/good" "$CONF/feeds/bad"
  case "$label" in
    *good*) feed_line "$moved" "$label" "${IP[srvG]}" >>"$CONF/feeds/good" ;;
    *) feed_line "$moved" "$label" "${IP[srvG]}" >>"$CONF/feeds/bad" ;;
  esac
  out=$(mktemp)
  traffic_start $EU "$out"
  since=$(now_iso)
  touch "$RUN_DIR/refresh"
  requested=""
  for _ in $(seq 200); do
    requested=$(events_since "$since" eu | grep 'reason=revisionChanged' | head -1)
    [ -n "$requested" ] && break
    sleep 0.1
  done
  t0=$(date +%s.%N)
  [ -n "$requested" ] && pass "the new revision was asked for at once" || fail "no revisionChanged within 20 s of the refresh"
  if wait_link pxa1 down 40 >/dev/null; then
    closed=$(elapsed "$t0")
    if at_most "$closed" 33 && ! at_most "$closed" 27; then pass "the gate closed $closed s after the revision, at the deadline"; else fail "the gate closed $closed s after the revision, want 30 s"; fi
  else
    fail "the gate never closed with the new revision unjudged"
  fi
  before=$(awk -v a="$t0" '$1 >= a && $1 < a + 25 && $2 != "none" { print $2 }' "$out" | sort -u | xargs)
  expect "the exit before the deadline, the prior" "$before" "${IP[$moved]}"
  sleep 4
  expect "the gate while validation still stalls" "$(link_up pxa1)" down
  rm -f "$SHIM_FLAGS/stall-test"
  note "validation works again"
  back=$(wait_link pxa1 up 30) && pass "the gate reopened $back s after validation came back" || fail "the gate stayed closed after validation came back"
  sleep 3
  traffic_stop
  expect "the exit that opened" "$(exit_ip $EU 3)" "${IP[srvG]}"
  expect "answers from the old server after the deadline" "$(awk -v a="$t0" -v old="${IP[$moved]}" '$1 >= a + 33 && $2 == old' "$out" | wc -l)" 0
  events_since "$since" eu | indent
  rm -f "$out" "$STATE/world"
}

# One stall of a read costs nothing that serves. A stall past readBound closes every gate and takes
# the routes in the same pass, so heartbeats stop as the gates close.
step_wedge() {
  local srv gate since before closed beats back t0
  use_world base 3 || return
  daemon_restart shimmed
  sleep 30
  srv=$(exit_of $EU); gate=$(cat /sys/class/net/pxa1/carrier_changes); since=$(now_iso)
  syn_rule "$srv"; before=$(syns "$srv")
  touch "$SHIM_FLAGS/wedge-once"
  note "one systemctl read wedges for its whole bound, with eu on $srv, watched for a heartbeat period and 5 s"
  sleep 65
  [ -e "$SHIM_FLAGS/wedge-once" ] && fail "the wedge never happened"
  expect "pxa1 carrier changes through one wedged systemctl read" "$(cat /sys/class/net/pxa1/carrier_changes)" "$gate"
  expect "switch events through it" "$("$CTL" switches --since "$since" | grep -c .)" 0
  beats=$(( $(syns "$srv") - before ))
  [ "$beats" -ge 1 ] && pass "routes stayed: $beats new connections to $srv through the wedge" || fail "no new connection to $srv in 65 s through the wedge, a 60 s heartbeat's period and 5 s"
  expect "eu's exit after it" "$(exit_of $EU)" "$srv"
  since=$(now_iso)
  note "the core freezes for 12 s, once"
  systemctl kill -s STOP $CORE_UNIT
  sleep 12
  systemctl kill -s CONT $CORE_UNIT
  sleep 20
  expect "pxa1 carrier changes through one frozen core" "$(cat /sys/class/net/pxa1/carrier_changes)" "$gate"
  expect "switch events through the freeze" "$("$CTL" switches --since "$since" | grep -c .)" 0
  expect "eu's exit after the freeze" "$(exit_of $EU)" "$srv"
  since=$(date '+%F %T')
  note "systemctl reads wedge for good"
  touch "$SHIM_FLAGS/wedge"; t0=$(date +%s.%N)
  if closed=$(wait_link pxa1 down 70); then
    at_most "$closed" 20 && fail "gates closed after only $closed s" || pass "gates closed $closed s after reads stopped, past readBound"
  else
    fail "gates still up 70 s after reads stopped"
  fi
  # Each gate closes in the same pass, one link at a time, so pxa2 is read until the same 70 s.
  if closed=$(wait_link pxa2 down "$(awk -v a="$t0" -v n="$(date +%s.%N)" 'BEGIN { l = 70 - (n - a); printf "%.1f", (l > 0 ? l : 0) }')"); then
    pass "pxa2 closed too, $(elapsed "$t0") s after reads stopped"
  else
    fail "pxa2 still up 70 s after reads stopped"
  fi
  sleep 2
  before=$(syns "$srv")
  sleep 15
  expect "new connections to $srv in the 15 s after the gates closed, the routes having gone" "$(( $(syns "$srv") - before ))" 0
  [ "$(journalctl -u $DAEMON --since "$since" -o cat | grep -c 'every gate closed: state unread')" -gt 0 ] && pass "the journal says why" || fail "the closure went unexplained"
  rm -f "$SHIM_FLAGS/wedge"
  back=$(wait_link pxa1 up 60) && pass "gates reopened $back s after reads came back" || fail "gates stayed closed after reads came back"
  rm -f "$STATE/world"
}

step_pause() {
  local solo_keys peers routes since t0 secs before pin_before got gate
  use_world base 3 || return
  before=$(syns srvE); pin_before=$(syns srvF)
  sleep 25
  note "before the pause, in 25 s: $(( $(syns srvE) - before )) new connections to srvE, $(( $(syns srvF) - pin_before )) to srvF"
  solo_keys=$(client_sum solo); peers=$(wg show wgclt2 peers); routes=$(ip route show table 182)
  apply_world '{"slots":[{"id":"solo","exit":"block"},{"id":"pin","exit":"block"}]}' || fail "Block was refused"
  sleep 2
  expect "pxa2 on Block" "$(link_up pxa2)" up
  expect "solo's status" "$(slot_field solo .status)" block
  expect "solo's reason" "$(slot_field solo .downReason)" null
  expect "the sum of solo's client keys and endpoint" "$(client_sum solo)" "$solo_keys"
  expect "the VPN Client's peer" "$(wg show wgclt2 peers)" "$peers"
  expect "the routes in table 182" "$(ip route show table 182)" "$routes"
  got=$(exit_ip $SOLO 2)
  [ -z "$got" ] && pass "no traffic through a blocked slot" || fail "blocked solo traffic reached the web as '$got'"
  sleep 12
  before=$(syns srvF)
  sleep 40
  expect "new connections in 40 s blocked to srvF, a server nothing else asks for" "$(( $(syns srvF) - before ))" 0
  before=$(syns srvE)
  sleep 25
  note "new connections in 25 s blocked to srvE, kept: $(( $(syns srvE) - before ))"
  since=$(now_iso); t0=$(date +%s.%N)
  apply_world '{"slots":[{"id":"solo","exit":"group:lab-solo"},{"id":"pin","exit":"server:pin-F"}]}' || fail "the way back was refused"
  if secs=$(wait_move "$since" solo 60 "$t0" "reason=exitChanged"); then pass "back from Block, solo reopened after $secs s"; else fail "no exitChanged within 60 s"; fi
  secs=$(wait_exit $SOLO srvE 20) && pass "solo exits via srvE again, $secs s on" || fail "solo exits via '$secs'"
  sleep 5
  gate=$(cat /sys/class/net/pxa2/carrier_changes); since=$(now_iso)
  note "Block, back, then a daemon killed and restarted"
  daemon_restart
  sleep 25
  expect "solo's status after the restart" "$(slot_field solo .status)" serving
  expect "pxa2 carrier changes across the restart" "$(cat /sys/class/net/pxa2/carrier_changes)" "$gate"
  expect "solo's events across the restart" "$(count_since "$since" solo)" 0
  expect "solo's exit after the restart" "$(exit_of $SOLO)" srvE
}

# step_passive loses packets on the way to the active server under a real upload, so the
# gateway's own socket retransmits. tcp_info should raise suspicion, and suspicion alone
# only starts a confirmation: whether the slot then moves is the rounds' call.
step_passive() {
  local srv upload started found=0
  use_world base 3 || return
  srv=$(exit_of $EU); started=$(date '+%F %T')
  note "eu on $srv, uploading through it, losing 15% of the packets toward it"
  head -c 60000000 /dev/zero >/tmp/control-upload
  $LAN curl -s -m 100 --limit-rate 2M -T /tmp/control-upload -X POST -o /dev/null "http://$EU/upload" &
  upload=$!
  sleep 3
  ip netns exec inet tc qdisc add dev "to-$srv" root netem loss 15%
  for _ in $(seq 60); do
    journalctl -u $DAEMON --since "$started" -o cat | grep -q 'confirm .* owner=group/lab-eu .*cause=passiveSuspicion' && { found=1; break; }
    sleep 1
  done
  [ "$found" = 1 ] && pass "tcp_info's suspicion started a confirmation for eu's group" || fail "tcp_info raised no suspicion within 60 s"
  journalctl -u $DAEMON --since "$started" -o cat | grep -E ' (owner=group/lab-eu|slot=eu) ' | cut -c21- | indent
  sleep 20
  ip netns exec inet tc qdisc del dev "to-$srv" root
  kill "$upload" 2>/dev/null
  wait "$upload" 2>/dev/null
  rm -f /tmp/control-upload
}

step_wan() {
  local srv since start_iso seen_down=0 gate_before after t_before t_cut t_back rows late
  use_world base 3 || return
  srv=$(exit_of $EU); since=$(now_iso); start_iso=$since
  gate_before=$(cat /sys/class/net/pxa1/carrier_changes)
  t_before=$(( $(now_ms) - 60000 )); t_cut=$(now_ms)
  note "cutting the WAN at the ISP, for 60 s, with eu on $srv"
  ip netns exec inet iptables -I FORWARD -i isp0 -j DROP
  ip netns exec inet iptables -I INPUT -i isp0 -j DROP
  sleep 5
  touch "$RUN_DIR/refresh"
  for _ in $(seq 55); do
    [ "$(view .wan)" = down ] && seen_down=1
    sleep 1
  done
  t_back=$(now_ms)
  [ "$seen_down" = 1 ] && pass "the daemon saw the WAN down" || fail "the daemon never saw the WAN down"
  expect "pxa1 during the outage" "$(link_up pxa1)" up
  ip netns exec inet iptables -D FORWARD -i isp0 -j DROP
  ip netns exec inet iptables -D INPUT -i isp0 -j DROP
  sleep 20
  expect "switch events of any slot during and after the outage" "$("$CTL" switches --since "$since" | grep -c .)" 0
  after=$(exit_of $EU)
  expect "eu's exit after the outage" "$after" "$srv"
  expect "pxa1 carrier changes" "$(cat /sys/class/net/pxa1/carrier_changes)" "$gate_before"
  expect "wan after the outage" "$(view .wan)" up
  sleep 35
  "$CTL" summary --from "$start_iso" --to "$(now_iso)" | tee "$RUN_DIR/wan.summary" | indent
  # Fields are read by name. A provider line missing one, or holding no number, leaves its total empty,
  # so no check below can pass on nothing.
  awk '/^provider=/ {
    delete v
    for (i = 1; i <= NF; i++) { split($i, kv, "="); v[kv[1]] = kv[2] }
    for (k in want) if (v[k] !~ /^[0-9]+$/) bad = 1
    lines++; excl += v["wanExcluded"]; forced += v["forcedAways"]; lost += v["attempts"] - v["successes"]
  } BEGIN { want["attempts"]; want["successes"]; want["wanExcluded"]; want["forcedAways"] }
  END { if (lines && !bad) print excl, lost, forced; else print "", "", "" }' "$RUN_DIR/wan.summary" >"$RUN_DIR/wan.totals"
  read -r excluded lost forced <"$RUN_DIR/wan.totals"
  [ -n "$excluded" ] && pass "the summary holds attempts, successes, wanExcluded and forcedAways for every provider" ||
    fail "the summary lacks a provider line, or one of attempts, successes, wanExcluded or forcedAways"
  [ "${excluded:-0}" -gt 0 ] && pass "$excluded probes during the outage were excluded, not blamed" || fail "no probe was excluded by the outage"
  # The accepted gap: a probe begun in the outage's last seconds and failing after the return is
  # charged once the baselines read blocked. Those rows are the only provider failures the step allows.
  late=$(metrics_sql 'SELECT count(*) FROM probe WHERE ts >= ? AND ts < ? AND verdict = 2 AND wan_excluded = 0' "$(( t_back - 5000 ))" "$t_back")
  expect "provider failures counted across the outage, against the failed rows begun in its last 5 s" "${lost:-}" "$late"
  expect "forced switch-aways across the outage" "${forced:-}" 0
  rows=$(metrics_sql 'SELECT count(*), coalesce(sum(wan_excluded), 0) FROM probe WHERE ts >= ? AND ts < ?' "$t_cut" "$t_back")
  read -r total excl <<<"$rows"
  awk -v t="$total" -v e="$excl" -v b="$(metrics_sql 'SELECT count(*) FROM probe WHERE ts >= ? AND ts < ?' "$t_before" "$t_cut")" -v s="$(( (t_back - t_cut) / 1000 ))" \
    'BEGIN { printf "metric rows: %.0f a minute during the outage, %d of them excluded, against %d in the minute before\n", t * 60 / s, e, b }' | indent
  expect "probe rows that blamed a server, of those over before the WAN came back" "$(metrics_sql 'SELECT count(*) FROM probe WHERE ts >= ? AND ts < ? AND verdict = 2 AND wan_excluded = 0' "$t_cut" "$(( t_back - 5000 ))")" 0
  note "rows that read failed among probes begun in its last 5 s: $late"
}

block_baselines() {
  ip netns exec inet iptables -I FORWARD -s 192.168.100.10 -d 203.0.113.80 -j DROP
  ip netns exec inet iptables -I FORWARD -s 192.168.100.10 -d 11.0.0.80 -j DROP
}
open_baselines() {
  ip netns exec inet iptables -D FORWARD -s 192.168.100.10 -d 203.0.113.80 -j DROP
  ip netns exec inet iptables -D FORWARD -s 192.168.100.10 -d 11.0.0.80 -j DROP
}

step_blocked() {
  local since
  use_world base 3 || return
  since=$(now_iso)
  note "every baseline target is blocked, with rivals still passing through the same WAN"
  block_baselines
  sleep 25
  note "the WAN reads $(view .wan) with only the baselines blocked"
  expect "switch events while every baseline is blocked and nothing's dead" "$("$CTL" switches --since "$since" | grep -c .)" 0
  failover "a silent server, every baseline blocked" failureMove mute_server unmute_server
  open_baselines
  sleep 20
  expect "wan after the baselines answer" "$(view .wan)" up
}

mute_and_blip() {
  mute_server "$1"
  ( sleep 10; block_baselines; sleep 5; open_baselines ) &
}

step_blip() {
  local since
  use_world base 3 || return
  since=$(now_iso)
  note "a 5 s baseline blip with nothing wrong"
  block_baselines
  sleep 5
  open_baselines
  sleep 25
  expect "switch events after a 5 s blip" "$("$CTL" switches --since "$since" | grep -c .)" 0
  failover "a silent server with a 5 s blip 10 s in" failureMove mute_and_blip unmute_server
  wait
}

step_recover() {
  local since t0 secs got marker dwell
  use_world base 3 || return
  since=$(now_iso); t0=$(date +%s.%N)
  note "solo's only member srvE dies"
  systemctl stop lab-xray-srvE
  if secs=$(wait_move "$since" solo 90 "$t0" 'down(confirmedDead)'); then
    pass "solo closed $secs s after its only member died, so nothing leaks"
  else
    fail "solo did not close within 90 s"
  fi
  expect "pxa2 while its member is dead" "$(link_up pxa2)" down
  got=$(exit_ip $SOLO 2)
  [ -z "$got" ] && pass "no traffic escapes through the closed slot" || fail "solo traffic reached the web as '$got'"
  note "srvE flickers: up 8 s, down 8 s, three times"
  marker=$(now_iso)
  for _ in 1 2 3; do
    start_xray srvE
    sleep 8
    systemctl stop lab-xray-srvE
    sleep 8
  done
  expect "reopenings while it flickered" "$(events_since "$marker" solo | grep -c 'reason=recovery')" 0
  expect "pxa2 while it flickers" "$(link_up pxa2)" down
  note "srvE comes back and stays"
  marker=$(now_iso); t0=$(date +%s.%N)
  start_xray srvE
  if secs=$(wait_move "$marker" solo 90 "$t0" 'reason=recovery'); then
    pass "solo reopened $secs s after its member came back"
  else
    fail "solo did not reopen within 90 s"
  fi
  dwell=$(wait_exit $SOLO srvE 30) && pass "solo exits via srvE again ${dwell} s later" || fail "solo exits via '$dwell'"
  note "watching for a flap, 60 s"
  sleep 60
  events_since "$since" solo | indent
  expect "solo's switch events from the death on" "$(count_since "$since" solo)" 2
  expect "pxa2 after recovery" "$(link_up pxa2)" up
}

step_assigned() {
  local since t0 secs
  use_world base 3 || return
  since=$(now_iso); t0=$(date +%s.%N)
  note "the provider withdraws the server slot pin has as its Exit"
  sed -i '/pin-F/d' "$CONF/feeds/solo"
  touch "$RUN_DIR/refresh"
  if secs=$(wait_move "$since" pin 30 "$t0" 'down(serverMissing)'); then
    pass "the slot closed $secs s after the feed dropped its server, and took no other"
  else
    fail "the slot did not close"
  fi
  expect "pxa3 while its server is missing" "$(link_up pxa3)" down
  feed_line srvF "NL pin-F" >>"$CONF/feeds/solo"
  t0=$(date +%s.%N)
  touch "$RUN_DIR/refresh"
  if secs=$(wait_move "$since" pin 60 "$t0" 'reason=recovery'); then
    pass "the slot reopened $secs s after its server returned"
  else
    fail "the slot did not reopen"
  fi
  events_since "$since" pin | indent
  sleep 2
  expect "its Exit, still that one server" "$(slot_field pin '.exit.kind')" server
}

step_quota() {
  use_world base 3 || return
  jq -c '.sources[] | {id, quotaUsed: ((.origin.provider.quota.upload // 0) + (.origin.provider.quota.download // 0)), limit: .origin.provider.quota.limit, expires: .origin.provider.quota.expires, failures: .origin.fetch.failures}' "$RUN_DIR/view.json" | indent
  [ "$(view '.sources[] | select(.id=="bad") | .origin.provider.quota.limit')" = 107374182400 ] && pass "quota and expiry reach the view" || fail "no quota in the view"
}

step_restart() {
  local srv solo core gate since download out result
  use_world base 3 || return
  srv=$(exit_of $EU); solo=$(exit_of $SOLO)
  core=$(core_pid)
  gate=$(cat /sys/class/net/pxa1/carrier_changes)
  since=$(now_iso)
  out=$(mktemp)
  note "eu on $srv, a 20 MB download running, killing the daemon with SIGKILL"
  $LAN curl -s -m 60 --limit-rate 1M -o /dev/null -w '%{size_download} %{http_code}' "http://$EU/bytes?n=20000000" >"$out" &
  download=$!
  sleep 2
  systemctl kill -s KILL $DAEMON
  sleep 1
  expect "daemon" "$(systemctl is-active $DAEMON)" inactive
  expect "core while the daemon is dead" "$(systemctl is-active $CORE_UNIT)" active
  expect "eu's exit while the daemon is dead" "$(exit_of $EU)" "$srv"
  sleep 5
  note "starting it again"
  daemon_start
  sleep 25
  expect "daemon" "$(systemctl is-active $DAEMON)" active
  expect "core PID" "$(core_pid)" "$core"
  expect "eu's exit after the restart" "$(exit_of $EU)" "$srv"
  expect "solo's exit after the restart" "$(exit_of $SOLO)" "$solo"
  expect "pxa1 carrier changes" "$(cat /sys/class/net/pxa1/carrier_changes)" "$gate"
  expect "switch events since the kill" "$("$CTL" switches --since "$since" | grep -c .)" 0
  wait "$download"
  result=$(cat "$out"); rm -f "$out"
  expect "the download across the restart" "$result" "20000000 200"
}

step_cost() {
  local d c hz own0 kids0 c0 w0 own1 kids1 c1 w1 secs=180
  use_world base 3 || return
  d=$(daemon_pid); c=$(core_pid); hz=$(getconf CLK_TCK)
  own() { awk '{print $14 + $15}' "/proc/$1/stat"; }
  kids() { awk '{print $16 + $17}' "/proc/$1/stat"; }
  written() { awk '/^write_bytes/ {print $2}' "/proc/$1/io"; }
  own0=$(own "$d"); kids0=$(kids "$d"); c0=$(own "$c"); w0=$(written "$d")
  sleep "$secs"
  own1=$(own "$d"); kids1=$(kids "$d"); c1=$(own "$c"); w1=$(written "$d")
  awk -v o="$((own1 - own0))" -v k="$((kids1 - kids0))" -v c="$((c1 - c0))" -v w="$((w1 - w0))" -v hz="$hz" -v s="$secs" 'BEGIN {
    printf "daemon, its own work (probes, signals, decisions, SQLite): %.0f ms CPU in %d s, %.2f%% of one core\n", o * 1000 / hz, s, o * 100 / hz / s
    printf "daemon, the tools its runtime runs each poll:              %.0f ms CPU in %d s, %.2f%% of one core\n", k * 1000 / hz, s, k * 100 / hz / s
    printf "core, all probe traffic and no user traffic:               %.0f ms CPU in %d s, %.2f%% of one core\n", c * 1000 / hz, s, c * 100 / hz / s
    printf "daemon disk writes: %.0f KB in %d s, about %.0f MB a day\n", w / 1024, s, w / 1024 / 1024 * 86400 / s
  }' | indent
  local d0 d1; d0=$((own0 + kids0)); d1=$((own1 + kids1))
  note "daemon memory: $(awk '/VmRSS|VmHWM/ {printf "%s %d MB  ", $1, $2 / 1024}' /proc/"$d"/status)"
  note "core memory:   $(awk '/VmRSS|VmHWM/ {printf "%s %d MB  ", $1, $2 / 1024}' /proc/"$c"/status)"
  note "binary sizes:  $(du -h "$CTL" | cut -f1) controllab, $(du -h "$LAB_HOME/current/$DAEMON_BIN" | cut -f1) $DAEMON_BIN"
  note "databases:     $(du -h "$LAB_HOME/$STATE_DB"* "$LAB_HOME/$METRICS_DB"* | awk '{printf "%s %s  ", $2, $1}' | sed "s#$LAB_HOME/##g")"
  note "probing:       a heartbeat each 60 s on the 3 served servers, over HTTP targets"
  [ "$(awk -v d="$((d1 - d0))" -v hz="$hz" -v s="$secs" 'BEGIN { print (d * 100 / hz / s < 5) ? 1 : 0 }')" = 1 ] && pass "daemon under 5% of one VM core" || fail "daemon CPU is too high"
}

step_contract() {
  use_world base 3 || return
  mkdir -p /tmp/control-contract
  cp "$RUN_DIR/view.json" /tmp/control-contract/view.json
  "$CTL" json switches >/tmp/control-contract/switches.json
  "$CTL" servers >/tmp/control-contract/servers.json
  "$CTL" json replaced >/tmp/control-contract/replaced.json
  "$CTL" json --from "$(date -u -d '-20 minutes' +%Y-%m-%dT%H:%M:%SZ)" --to "$(now_iso)" analytics >/tmp/control-contract/analytics.json || fail "no analytics"
  chmod -R a+rX /tmp/control-contract
  pass "query results saved for the page's schema check"
}

# Three slots with different groups, all on one server that dies. Each moves on its own campaign,
# and none waits for another.
step_three() {
  local since t0 secs s ip applied ns
  use_world three 3 0 || return
  apply_world '{"groups":[{"id":"g1","labels":["farm-101","farm-102","farm-103","farm-104"]},{"id":"g2","labels":["farm-101","farm-102","farm-103","farm-105"]},{"id":"g3","labels":["farm-101","farm-102","farm-104","farm-105"]}],
    "slots":[{"id":"a","exit":"group:g1"},{"id":"b","exit":"group:g2"},{"id":"c","exit":"group:g3"}]}' || { fail "the groups were refused"; return; }
  sleep 35
  for s in a b c; do expect "slot $s's server before the death" "$(slot_label "$s")" "DE farm-101"; done
  ip=$(farm_ip 1)
  after_beat farm "$ip"
  since=$(now_iso); t0=$(date +%s.%N)
  silence "$ip"
  for s in a b c; do
    # Each slot's bound is on its own switch's applied time. The wait runs 5 s past it, so a switch
    # applied in time but read late is still judged by when it applied.
    # The bound compares the exact duration; only the printed one is rounded, to 1 ms.
    if applied=$(wait_applied "$since" "$s" $(( LIMIT + 5 )) "$t0" 'reason=failureMove'); then
      ns=$(( $(ns_of "$applied") - $(ns_of "$t0") ))
      secs=$(awk -v n="$ns" 'BEGIN { printf "%.3f", n / 1e9 }')
      if [ "$ns" -gt $(( LIMIT * 1000000000 )) ]; then fail "slot $s moved in $secs s, past the $LIMIT s"; else pass "slot $s moved in $secs s"; fi
    else
      fail "slot $s did not move within $(( LIMIT + 5 )) s"
    fi
  done
  "$CTL" switches --since "$since" | indent
  unsilence "$ip"
  rm -f "$STATE/world"
}

# Eight slots lose eight servers at once, with disjoint rivals, so one round asks about 32 servers.
# The design's bound is 44 probes at once: those 32, the 4 workers that checks and UDP checks share,
# and the 8 serving heartbeats, which wait for no worker and so can start once all 4 are taken.
# This world has no check due in the storm, and UDP checks leave one worker free, so it reaches 43
# when a dying server's UDP check and the rival checks another one's failure owes land in the rounds.
# The living rivals carry 200 ms of delay, so that each round's probes really overlap.
step_storm() {
  local since t0 i s label killed="" ids="" d0 c0 f0 d1 c1 f1 sampler peaks window=40 t0ms line secs slowest=0 moved=0 faraway peak outward bound=44
  use_world storm 8 || return
  for i in 1 2 3 4 5 6 7 8; do
    label=$(slot_label "s$i")
    [ -n "$label" ] || { fail "slot s$i serves nothing to begin with"; return; }
    killed="$killed $(label_ip "$label")"
    ids="$ids $(slot_field "s$i" .server)"
  done
  note "slots serve:$killed"
  peaks=$(mktemp)
  ( while :; do
      echo "$(ss -Htn state established '( dport = :10086 )' | wc -l) $(ss -Htn state established state syn-sent '( dport = :443 )' dst 198.51.100.0/24 | wc -l)" >>"$peaks"
      sleep 0.1
    done ) &
  sampler=$!
  d0=$(ticks "$(daemon_pid)"); c0=$(ticks "$(core_pid)"); f0=$(ticks "$(systemctl show -p MainPID --value lab-xray-farm)")
  since=$(now_iso); t0=$(date +%s.%N); t0ms=$(now_ms)
  for s in $killed; do silence "$s"; done
  # Living servers answer in tens of ms unslowed, and a probe that ends that soon never overlaps the rest.
  ip netns exec farm tc qdisc add dev eth0 root netem delay 200ms
  until past "$t0" 45; do
    moved=$("$CTL" switches --since "$since" | grep -v 'applied=unapplied' | grep -c 'reason=failureMove')
    [ "$moved" -ge 8 ] && break
    sleep 0.5
  done
  while ! past "$t0" "$window"; do sleep 0.5; done
  d1=$(ticks "$(daemon_pid)"); c1=$(ticks "$(core_pid)"); f1=$(ticks "$(systemctl show -p MainPID --value lab-xray-farm)")
  kill "$sampler" 2>/dev/null; wait "$sampler" 2>/dev/null
  peak=$(awk '$1 > p { p = $1 } END { print p + 0 }' "$peaks"); outward=$(awk '$2 > p { p = $2 } END { print p + 0 }' "$peaks"); rm -f "$peaks"
  for i in 1 2 3 4 5 6 7 8; do
    line=$(events_since "$since" "s$i" | grep -v 'applied=unapplied' | grep 'reason=failureMove' | head -1)
    if [ -n "$line" ]; then
      secs=$(awk -v a="$t0" -v b="$(sed -E 's/.* applied=([0-9.]+) .*/\1/' <<<"$line")" 'BEGIN { printf "%.1f", b - a }')
      note "s$i moved in $secs s"
      [ "$(awk -v a="$secs" -v b="$slowest" 'BEGIN { print (a > b) ? 1 : 0 }')" = 1 ] && slowest=$secs
    else
      fail "s$i did not move within $window s"
    fi
  done
  at_most "$slowest" $LIMIT && pass "the slowest of eight moved in $slowest s" || fail "the slowest of eight took $slowest s, past the $LIMIT s"
  "$CTL" switches --since "$since" | grep -v 'reason=failureMove' | indent
  expect "slots closed by the storm" "$("$CTL" switches --since "$since" | grep -c ' -> down(')" 0
  [ "$peak" -le "$bound" ] && pass "at most $peak probes at once, sampled at the core's probe inbound, against the bound of $bound" || fail "$peak probes at once, past the bound of $bound"
  note "the core's own sockets to servers peaked at $outward, since a dial to a silent server outlives the probe that asked for it"
  note "CPU over $window s, share of one VM core: daemon $(cpu_share $((d1 - d0)) "$window")%, core $(cpu_share $((c1 - c0)) "$window")%, the farm's stand-in core $(cpu_share $((f1 - f0)) "$window")%"
  note "waiting for the metrics flush"
  sleep 35
  faraway=$(metrics_sql 'SELECT DISTINCT lineage FROM probe WHERE ts >= ? AND verdict = 2 AND wan_excluded = 0' "$t0ms" | grep -vwF -f <(tr ' ' '\n' <<<"$ids" | grep .) | xargs)
  expect "healthy servers that any round called failed" "${faraway:-none}" none
  ip netns exec farm tc qdisc del dev eth0 root
  for s in $killed; do unsilence "$s"; done
  rm -f "$STATE/world"
}

# Every server of a slot's group goes silent while it serves, and one answers again later. The slot
# should close, stay closed, and find that server.
# A whole group goes silent together. $1 is the world, $2 its servers, and $3 how long after one answers
# again the slot may take to be on it.
all_silent() {
  local world=$1 n=$2 limit=$3 i since t0 secs back
  use_world "$world" 1 || return
  since=$(now_iso); t0=$(date +%s.%N)
  for i in $(seq "$n"); do silence "$(farm_ip "$i")"; done
  if secs=$(wait_move "$since" c 120 "$t0" ' -> down('); then
    if at_most "$secs" $LIMIT; then pass "the slot closed $secs s after all $n servers went silent"; else fail "the slot closed $secs s after all $n servers went silent, past the $LIMIT s"; fi
  else fail "the slot stayed open 120 s after all $n servers went silent"; fi
  sleep 90
  expect "reopenings on silent servers $(elapsed "$t0") s after the silence" "$("$CTL" switches --since "$since" | grep -c ' -> \[')" 0
  "$CTL" switches --since "$since" | cut -c1-150 | indent
  if [ "$(slot_field c .status)" = failed ]; then pass "the slot reads failed $(elapsed "$t0") s after the silence"; else fail "$(elapsed "$t0") s after the silence the slot reads $(slot_field c .status) on a silent server, $(slot_label c)"; fi
  note "round results since the silence, by target and verdict, 0 unknown and 2 failed: $(metrics_sql 'SELECT target, verdict, count(*) FROM probe WHERE purpose = 1 AND ts >= ? GROUP BY 1, 2' "${t0%.*}000" | tr '\n' ';')"
  expect "round results that blamed a target instead" "$(metrics_sql 'SELECT count(*) FROM probe WHERE purpose = 1 AND verdict = 0 AND ts >= ?' "${t0%.*}000")" 0
  back=$(farm_ip "$n")
  since=$(now_iso); t0=$(date +%s.%N)
  unsilence "$back"
  if secs=$(wait_move "$since" c "$limit" "$t0" "\[DE/$(farm_label "$n")\]"); then pass "it moved to the server that answered, $secs s after it did"; else fail "it isn't on the one server that answers, $limit s after it did, and reads $(slot_field c .status) on $(slot_label c)"; fi
  "$CTL" switches --since "$since" | cut -c1-150 | indent
  for i in $(seq "$n"); do [ "$(farm_ip "$i")" = "$back" ] || unsilence "$(farm_ip "$i")"; done
  rm -f "$STATE/world"
}

# The plan's bound: up to 20 s for the fast search (top 4 every 5 s, the rest every 20 s) to reach the
# one server that answers, then 3 passes 30 s apart before the slot reopens on it, about 81 s.
step_allsilent() { all_silent kept12 12 90; }
step_allsilent64() { all_silent kept64 64 240; }

# One probe target stops answering through every server, while the servers go on passing the others.
# The target is blamed and nothing moves. Then a server dies, and it still fails over.
step_badtarget() {
  local since t0
  use_world base 3 || return
  since=$(now_iso); t0=$(date +%s)
  note "probe target 203.0.113.82 censored for every server"
  ip netns exec inet iptables -I FORWARD -s 198.51.100.0/24 -d 203.0.113.82 -j DROP
  sleep 100
  expect "switch events of any slot with one target censored, nothing dead" "$("$CTL" switches --since "$since" | grep -c .)" 0
  note "results on the censored target by purpose and verdict: $(metrics_sql 'SELECT purpose, verdict, count(*) FROM probe WHERE target = ? AND ts >= ? GROUP BY 1, 2' t3 "${t0}000" | tr '\n' ';')"
  failover "a killed server with one target censored" failureMove kill_server revive_server
  ip netns exec inet iptables -D FORWARD -s 198.51.100.0/24 -d 203.0.113.82 -j DROP
}

# Latency that swings on the server a slot is on, and then on all of them. Its heartbeats read slow
# against themselves, its rounds show nothing severe, and nothing leaves for slowness.
step_jitter() {
  local srv s since t0 rounds
  use_world base 3 || return
  sleep 35
  srv=$(exit_of $EU); since=$(now_iso); t0=$(date +%s)
  note "eu on $srv: 60 ms swinging by 60 ms, on that server alone, for 120 s"
  ip netns exec "$srv" tc qdisc add dev eth0 root netem delay 60ms 60ms
  sleep 120
  ip netns exec "$srv" tc qdisc del dev eth0 root
  expect "slowness and failure moves off a jittery server" "$(events_since "$since" eu | grep -c 'reason=\(slownessMove\|failureMove\)')" 0
  note "campaigns a slow heartbeat started: $(journalctl -u $DAEMON --since "@$t0" -o cat | grep -c 'slot=eu .*cause=slowHeartbeat'), round probes: $(metrics_sql 'SELECT count(*) FROM probe WHERE purpose = 1 AND ts >= ?' "${t0}000")"
  events_since "$since" eu | cut -c1-120 | indent
  sleep 30
  since=$(now_iso); t0=$(date +%s)
  note "60 ms swinging by 60 ms on every DE server, for 90 s"
  for s in srvA srvB srvC srvD; do ip netns exec "$s" tc qdisc add dev eth0 root netem delay 60ms 60ms; done
  sleep 90
  for s in srvA srvB srvC srvD; do ip netns exec "$s" tc qdisc del dev eth0 root; done
  expect "slowness and failure moves with every server jittery" "$(events_since "$since" eu | grep -c 'reason=\(slownessMove\|failureMove\)')" 0
  note "campaigns a slow heartbeat started: $(journalctl -u $DAEMON --since "@$t0" -o cat | grep -c 'slot=eu .*cause=slowHeartbeat'), round probes: $(metrics_sql 'SELECT count(*) FROM probe WHERE purpose = 1 AND ts >= ?' "${t0}000")"
  events_since "$since" eu | cut -c1-120 | indent
}

# A slot closed on servers silent for longer than any success vouches for runs its sweep, and when
# one server answers it reopens. Block gets it there, since a blocked slot stays off every server while
# the old successes run out. $1 is the world, $2 its servers, and $3 the reopen limit.
closed_sweep() {
  local world=$1 n=$2 limit=$3 i since t0 secs window=60 d0 c0 d1 c1 back
  use_world "$world" 1 || return
  apply_world '{"slots":[{"id":"c","exit":"block"}]}' || { fail "Block was refused"; return; }
  for i in $(seq "$n"); do silence "$(farm_ip "$i")"; done
  note "blocked, with all $n servers silent, for longer than a success vouches"
  sleep 125
  apply_world '{"slots":[{"id":"c","exit":"group:all"}]}' || { fail "the way back was refused"; return; }
  sleep 15
  expect "the slot with nothing ready" "$(slot_field c .status)/$(link_up pxa1)" "failed/down"
  d0=$(ticks "$(daemon_pid)"); c0=$(ticks "$(core_pid)")
  sleep "$window"
  d1=$(ticks "$(daemon_pid)"); c1=$(ticks "$(core_pid)")
  note "sweep CPU at $n kept, $window s closed with every server silent, share of one VM core: daemon $(cpu_share $((d1 - d0)) "$window")%, core $(cpu_share $((c1 - c0)) "$window")%"
  note "daemon RSS $(rss_mb "$(daemon_pid)" VmRSS) MB, core RSS $(rss_mb "$(core_pid)" VmRSS) MB"
  back=$(farm_ip "$n")
  since=$(now_iso); t0=$(date +%s.%N)
  unsilence "$back"
  if secs=$(wait_move "$since" c "$limit" "$t0" ' -> \['); then pass "the slot reopened $secs s after one of $n answered again"; else fail "the slot stayed closed $limit s after one of $n answered again"; fi
  "$CTL" switches --since "$since" | cut -c1-150 | indent
  for i in $(seq "$n"); do [ "$(farm_ip "$i")" = "$back" ] || unsilence "$(farm_ip "$i")"; done
  rm -f "$STATE/world"
}

step_closed12() { closed_sweep kept12 12 75; }
step_sweep64() { closed_sweep kept64 64 240; }

# Each root renders its own hops, so 64 roots by 8 hops is 512 outbounds. The design proposes a
# limit of 512 MB of the gateway's 3 GB.
step_memory() {
  local t0 count=0 rss hwm d
  t0=$(date +%s.%N)
  use_world memory 1 0 0 || return
  until past "$t0" 240; do
    count=$(/usr/local/bin/xray api lso --server=$API 2>/dev/null | jq '[.outbounds[].tag] | length' 2>/dev/null || echo 0)
    [ "${count:-0}" -ge 512 ] && break
    sleep 2
  done
  [ "${count:-0}" -ge 512 ] && pass "the core holds $count outbounds, $(elapsed "$t0") s after an empty start" || fail "the core holds $count outbounds, want at least 64 by 8"
  note "servers: $(servers_json | jq '.items | length') stored, supported: $(servers_json | jq '[.items[] | select(.supported)] | length')"
  sleep 60
  rss=$(rss_mb "$(core_pid)" VmRSS); hwm=$(rss_mb "$(core_pid)" VmHWM); d=$(rss_mb "$(daemon_pid)" VmRSS)
  note "core RSS $rss MB, high water $hwm MB. Daemon RSS $d MB"
  [ "$hwm" -lt 512 ] && pass "core memory at 64 roots by 8 hops: $hwm MB high water, under the proposed 512 MB" || fail "core memory at 64 roots by 8 hops: $hwm MB high water, past the proposed 512 MB"
  rm -f "$STATE/world"
}

step_acme() {
  local items stored origins line want moved_label moved_id replaced got active
  use_world acme 1 0 || return
  items=$(servers_json)
  stored=$(jq '[.items[] | select(.presence == "present")] | length' <<<"$items")
  origins=$(jq '[.items[] | select(.label | endswith(" · EDGE-STANDBY"))] | length' <<<"$items")
  expect "present servers, every one offered" "$stored" 12
  expect "servers named for EDGE-STANDBY" "$origins" 6
  # Each config names the server it calls proxy, its tags carry the flag, and the label is sent without it.
  for line in "Germany=DE" "Netherlands=NL" "Sweden=SE" "Bulgaria=BG" "USA=US" "Romania=RO"; do
    want="${line%=*} · proxy"
    expect "$want's country, from its tags' flag" "$(jq -r --arg l "$want" '.items[] | select(.label == $l) | "\(.country.code) \(.country.source)"' <<<"$items")" "${line#*=} tagFlag"
  done
  active=$(slot_label lew)
  [ -n "$active" ] && pass "slot lew serves '$active' over Reality, through the lab's own core" || fail "slot lew serves nothing"
  moved_label="Sweden · proxy"
  moved_id=$(jq -r --arg l "$moved_label" '.items[] | select(.label == $l) | .id' <<<"$items")
  [ -n "$moved_id" ] || { fail "no server is labelled '$moved_label', so nothing can be moved"; rm -f "$STATE/world"; return; }
  note "the provider moves '$moved_label' to another address, keeping its name and credentials"
  # Every config lists every server, so a move changes each occurrence. Sweden · proxy is node 6.
  sed -i 's/198\.51\.100\.206/198.51.100.240/g' "$CONF/feeds/acme"
  touch "$RUN_DIR/refresh"
  replaced=""
  for _ in $(seq 60); do
    replaced=$("$CTL" json replaced | jq -r --arg id "$moved_id" '[.[] | select(.server == $id)] | length')
    [ "${replaced:-0}" -ge 1 ] && break
    sleep 0.5
  done
  [ "${replaced:-0}" -ge 1 ] && pass "'Server replaced' is on record for it" || fail "no 'Server replaced' event for it"
  "$CTL" json replaced | jq -c '.[]' | indent
  got=$(servers_json | jq -r --arg l "$moved_label" '.items[] | select(.label == $l) | .id')
  expect "its lineage after the move" "$got" "$moved_id"
  expect "present servers after the move" "$(servers_json | jq '[.items[] | select(.presence == "present")] | length')" 12
  rm -f "$STATE/world"
}

# One provider, with checks 20 s apart against production's 5 minutes.
world_udpbroken() {
  drop_base_feeds
  { feed_line srvA "DE good-A"; feed_line srvB "DE bad-B"; feed_line srvC "DE good-C"; feed_line srvD "DE bad-D"
    feed_line srvE "NL solo-E"; feed_line srvF "NL pin-F"; } >"$CONF/feeds/good"
  write_bootstrap "{\"timings\": {\"udpCheckEvery\": \"${UDP_EVERY}s\"}}"
  jq -n '{
    sources: [{id: "good"}],
    groups: [{id: "lab-eu", countries: ["DE"]}, {id: "lab-solo", include: "solo-E"}],
    slots: [{id: "eu", exit: "group:lab-eu"}, {id: "solo", exit: "group:lab-solo"}]
  }' >"$CONF/world.json"
}

UDP_EVERY=20
declare -A UDP_LABEL=([srvA]=good-A [srvB]=bad-B [srvC]=good-C [srvD]=bad-D [srvE]=solo-E [srvF]=pin-F)
udp_block() { ip netns exec inet iptables -I FORWARD -s "${IP[$1]}" -p udp -j DROP; }
udp_unblock() { ip netns exec inet iptables -D FORWARD -s "${IP[$1]}" -p udp -j DROP; }
udp_state() { servers_json | jq -r --arg l "${UDP_LABEL[$1]}" '.items[] | select(.label | contains($l)) | .udp'; }
udp_states() { servers_json | jq -r '[.items[].udp] | sort | group_by(.) | map("\(length) \(.[0])") | join(", ")'; }

# wait_udp waits up to $3 s for server $1 to read $2, and prints how long that took.
wait_udp() {
  local t0; t0=$(date +%s.%N)
  until [ "$(udp_state "$1")" = "$2" ]; do
    past "$t0" "$3" && return 1
    sleep 0.25
  done
  elapsed "$t0"
}

# The same world at two check periods, so what a check costs is the difference. Counted queries and the
# WAN's bytes say how many checks a window really held.
step_udpcost() {
  local secs=${1:-180} every d c hz
  own() { awk '{print $14 + $15}' "/proc/$1/stat"; }
  wan_bytes() { echo $(( $(cat /sys/class/net/eth4/statistics/rx_bytes) + $(cat /sys/class/net/eth4/statistics/tx_bytes) )); }
  queries() { ip netns exec web iptables -vnxL INPUT | awk '$3 == "udp" && /dpt:53/ { n += $1 } END { print n + 0 }'; }
  sample() {
    local d0 c0 w0 q0 d1 c1 w1 q1
    d=$(daemon_pid); c=$(core_pid); hz=$(getconf CLK_TCK)
    sleep 30
    d0=$(own "$d"); c0=$(own "$c"); w0=$(wan_bytes); q0=$(queries)
    sleep "$secs"
    d1=$(own "$d"); c1=$(own "$c"); w1=$(wan_bytes); q1=$(queries)
    note "every $1: $((q1 - q0)) questions in $secs s, daemon $(((d1 - d0) * 1000 / hz)) ms CPU, core $(((c1 - c0) * 1000 / hz)) ms CPU, WAN $((w1 - w0)) bytes"
    echo "$((q1 - q0)) $(((d1 - d0) * 1000 / hz)) $(((c1 - c0) * 1000 / hz)) $((w1 - w0))"
  }
  UDP_EVERY=86400
  rm -f "$STATE/world"
  use_world udpbroken 2 35 || return
  ip netns exec web iptables -A INPUT -p udp --dport 53
  local off on
  off=$(sample "24h" | tee /dev/stderr | tail -1)
  write_bootstrap "{\"timings\": {\"udpCheckEvery\": \"20s\"}}"
  daemon_restart
  until slots_serving 2; do sleep 0.5; done
  on=$(sample "20s" | tee /dev/stderr | tail -1)
  awk -v off="$off" -v on="$on" 'BEGIN {
    split(off, a, " "); split(on, b, " "); n = b[1] - a[1]
    if (n <= 0) { print "no checks ran"; exit 1 }
    printf "%d more questions: daemon %.2f ms, core %.2f ms, WAN %.0f bytes each\n", n, (b[2] - a[2]) / n, (b[3] - a[3]) / n, (b[4] - a[4]) / n
    printf "at 12 a server an hour: daemon %.0f ms, core %.0f ms CPU, WAN %.1f KB\n", 12 * (b[2] - a[2]) / n, 12 * (b[3] - a[3]) / n, 12 * (b[4] - a[4]) / n / 1024
  }' | indent
  rm -f "$STATE/world"
}

# Counters in the web namespace of each server's questions to the stand-in resolvers, so a window says
# which servers were checked.
udp_count_on() { local s; for s in "${!UDP_LABEL[@]}"; do ip netns exec web iptables -A INPUT -s "${IP[$s]}" -p udp --dport 53; done; }
udp_count_off() { local s; for s in "${!UDP_LABEL[@]}"; do ip netns exec web iptables -D INPUT -s "${IP[$s]}" -p udp --dport 53 2>/dev/null; done; }
udp_questions() { ip netns exec web iptables -vnxL INPUT | awk -v s="${IP[$1]}" '$3 == "udp" && $7 == s && /dpt:53/ { print $1; exit }'; }
# udp_asked prints, for the servers named, those whose question count moved since the snapshot in $1,
# or none. A counter it can't read fails the step rather than reading as no question.
udp_snapshot() { local s; for s in "${!UDP_LABEL[@]}"; do echo "$s $(udp_questions "$s")"; done; }
udp_asked() {
  local snap=$1 s now was out=""; shift
  for s in "$@"; do
    now=$(udp_questions "$s"); was=$(awk -v s="$s" '$1 == s { print $2 }' <<<"$snap")
    [ -n "$now" ] && [ -n "$was" ] || { echo "unread counter for $s"; return; }
    [ "$now" != "$was" ] && out="$out $s"
  done
  out=$(tr ' ' '\n' <<<"$out" | sed '/^$/d' | sort | tr '\n' ' ' | sed 's/ $//')
  echo "${out:-none}"
}
udp_others() { local s; for s in "${!UDP_LABEL[@]}"; do [[ " $* " == *" $s "* ]] || echo "$s"; done | sort; }
# udp_failed_of prints, of the servers named, those that read failed, or none. A state it can't read,
# or a call naming no server, prints what is missing instead, so no evidence never reads as none.
udp_failed_of() {
  local s st out=""
  [ $# -gt 0 ] || { echo "no servers named"; return; }
  for s in "$@"; do
    st=$(udp_state "$s") || { echo "unread state for $s"; return; }
    case $st in
      failed) out="$out $s";;
      passed|unknown) ;;
      *) echo "unread state for $s: '$st'"; return;;
    esac
  done
  if [ -n "$out" ]; then echo "${out# }"; else echo none; fi
}
# udp_states_of prints the servers' states, as udp_states does for all.
udp_states_of() { local s; for s in "$@"; do udp_state "$s"; done | sort | uniq -c | awk '{ printf "%s%d %s", (NR > 1 ? ", " : ""), $1, $2 } END { print "" }'; }

# Only a serving server is checked: eu's and solo's srvE. A failure on eu's asks its next two once,
# so the failure is blamed on the server only when a rival passes.
step_udpbroken() {
  local srv since t0 secs after solo_since flow trigger core all_since snap idle de rivals
  rm -f "$STATE/world"
  use_world udpbroken 2 5 || return
  systemctl stop lab-udp-echo 2>/dev/null
  systemd-run --unit=lab-udp-echo --collect --quiet -p NetworkNamespacePath=/run/netns/web python3 -c '
import socket
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.bind(("203.0.113.81", 5353))
while True:
    d, a = s.recvfrom(100)
    s.sendto(a[0].encode(), a)
'
  udp_count_on
  sleep 0.5
  srv=$(exit_of $EU); core=$(core_pid)
  [ "$srv" = none ] && { fail "eu serves nothing to begin with"; udp_count_off; return; }
  if secs=$(wait_udp "$srv" passed 90) && wait_udp srvE passed 90 >/dev/null; then
    pass "the serving servers, $srv and srvE, read passed, $secs s on"
  else
    fail "the serving servers read $srv $(udp_state "$srv"), srvE $(udp_state srvE) after 90 s"; udp_count_off; return
  fi
  idle=$(udp_others "$srv" srvE | tr '\n' ' ')
  snap=$(udp_snapshot)
  sleep "$(( 2 * UDP_EVERY ))"
  # shellcheck disable=SC2086
  expect "servers on no slot, after two periods" "$(udp_states_of $idle)" "4 unknown"
  # shellcheck disable=SC2086
  expect "servers on no slot asked a question in two periods" "$(udp_asked "$snap" $idle)" none
  expect "serving servers asked a question in two periods" "$(udp_asked "$snap" "$srv" srvE)" "$(printf '%s\n' "$srv" srvE | sort | tr '\n' ' ' | sed 's/ $//')"

  expect "a datagram through the slot before the fault" "$(udp_exit $EU)" "${IP[$srv]}"
  flow=$(mktemp); trigger=$(mktemp -u)
  open_flow $EU "$trigger" >"$flow" &
  sleep 1
  note "eu on $srv: blocking only its UDP, every $UDP_EVERY s a check"
  since=$(now_iso); t0=$(date +%s.%N); snap=$(udp_snapshot)
  udp_block "$srv"
  sleep 1
  expect "a datagram through the slot during the fault" "$(udp_exit $EU)" none
  expect "new TCP through the slot during the fault" "$(exit_of $EU)" "$srv"
  if secs=$(wait_move "$since" eu 100 "$t0" 'reason=udpMove'); then
    at_most "$(( 2 * UDP_EVERY ))" "$secs" && pass "three checks took the slot off it, $secs s on, no sooner than two periods" || fail "moved after $secs s, under two periods of $UDP_EVERY s, so fewer than three failures moved it"
    at_most "$secs" "$(( 3 * UDP_EVERY + 20 ))" && pass "moved within three periods and a round: $secs s" || fail "moved after $secs s, past three periods and a round"
  else
    fail "no udpMove within 100 s of blocking $srv's UDP"
    journalctl -u $DAEMON --since "@${t0%.*}" -o cat | grep -E ' (owner=group/lab-eu|slot=eu) ' | cut -c21- | tail -n 12 | indent
  fi
  events_since "$since" eu | cut -c1-170 | indent
  expect "moves off it, all for this reason" "$(events_since "$since" eu | grep -cv 'reason=udpMove')" 0
  expect "$srv reads" "$(udp_state "$srv")" failed
  after=$(exit_of $EU)
  [ "$after" != "$srv" ] && [ "$after" != none ] && pass "new TCP exits via $after" || fail "new TCP exits via '$after'"
  # The rival checks: each failure asks the next two by score, so the pair can change as the order
  # does. Only eu's group is ever asked, every one asked passed, and srvF, in no group of eu's, never is.
  de=$(for s in srvA srvB srvC srvD; do [ "$s" != "$srv" ] && echo "$s"; done | tr '\n' ' ')
  # shellcheck disable=SC2086
  rivals=$(udp_asked "$snap" $de)
  [ "$rivals" != none ] && [ "$(wc -w <<<"$rivals")" -ge 2 ] && pass "rivals of $srv checked: $rivals" || fail "rivals of $srv checked: '$rivals', want at least two of $de"
  [[ " $rivals " == *" $after "* ]] && pass "eu moved to one of them, $after" || fail "eu moved to $after, not a checked rival"
  # shellcheck disable=SC2086
  expect "the rivals read" "$(udp_states_of $rivals)" "$(wc -w <<<"$rivals" | tr -d ' ') passed"
  expect "srvF, on no slot and no rival, asked a question during the fault" "$(udp_asked "$snap" srvF)" none
  expect "a datagram through the slot after the move" "$(udp_exit $EU)" "${IP[$after]:-none}"
  touch "$trigger"; wait; sleep 1
  grep -q '^closed:' "$flow" && pass "the move closed an open connection" || fail "an open connection after the move: $(tr '
' ' ' <"$flow")"
  expect "the core across the move" "$(core_pid)" "$core"
  expect "slot solo's switches meanwhile" "$(events_since "$since" solo | grep -c .)" 0
  rm -f "$flow" "$trigger"
  udp_unblock "$srv"
  snap=$(udp_snapshot)
  sleep "$(( 2 * UDP_EVERY ))"
  expect "$srv, on no slot now, asked a question in two periods after its UDP came back" "$(udp_asked "$snap" "$srv")" none
  expect "$srv, unchecked, still reads" "$(udp_state "$srv")" failed

  # The other servers passing is what proves srvE's own fault, and it has nowhere to go.
  solo_since=$(now_iso)
  note "solo on srvE, its only member: blocking its UDP"
  udp_block srvE
  if secs=$(wait_udp srvE failed 100); then
    pass "srvE reads failed, $secs s on, with the others passing"
    sleep "$UDP_EVERY"
    expect "solo's switches with its only server broken" "$(events_since "$solo_since" solo | grep -c .)" 0
    expect "solo's status" "$(slot_field solo .status)" serving
    expect "new TCP through solo" "$(exit_of $SOLO)" srvE
  else
    fail "srvE reads $(udp_state srvE) after 100 s"
  fi
  udp_unblock srvE
  if ! secs=$(wait_udp srvE passed 45); then fail "srvE reads $(udp_state srvE) 45 s after its UDP came back"; udp_count_off; return; fi
  pass "one pass cleared srvE, $secs s after its UDP came back"

  note "one target blocked for every server: 8.8.8.8, for $(( 3 * UDP_EVERY )) s"
  all_since=$(now_iso)
  ip netns exec inet iptables -I FORWARD -d 8.8.8.8 -p udp -j DROP
  sleep "$(( 3 * UDP_EVERY ))"
  ip netns exec inet iptables -D FORWARD -d 8.8.8.8 -p udp -j DROP
  expect "serving servers with one provider blocked" "$(udp_states_of "$after" srvE)" "2 passed"
  expect "moves with one provider blocked" "$("$CTL" switches --since "$all_since" | grep -c .)" 0

  note "every server's UDP blocked, for $(( 6 * UDP_EVERY )) s"
  all_since=$(now_iso)
  for s in "${!UDP_LABEL[@]}"; do udp_block "$s"; done
  sleep "$(( 6 * UDP_EVERY ))"
  note "servers now read: $(udp_states)"
  # srvE has no rival to prove the path, so its failure stands alone, as a one-member group's does.
  expect "srvE, a one-member group's, with every server blocked" "$(udp_state srvE)" failed
  # shellcheck disable=SC2046
  expect "servers besides $srv and srvE that read failed with every server blocked" "$(udp_failed_of $(udp_others "$srv" srvE))" none
  expect "campaigns for broken UDP with every server blocked" "$(journalctl -u $DAEMON --since "$(date -d "$all_since" '+%F %T')" -o cat | grep -c 'cause=udpFailure')" 0
  expect "moves with every server blocked" "$("$CTL" switches --since "$all_since" | grep -c .)" 0
  expect "new TCP through eu" "$(exit_of $EU)" "$after"
  expect "eu's status" "$(slot_field eu .status)" serving
  for s in "${!UDP_LABEL[@]}"; do udp_unblock "$s"; done
  if secs=$(wait_udp "$after" passed 45) && wait_udp srvE passed 45 >/dev/null; then pass "the serving servers read passed again, $secs s on"; else fail "the serving servers read $after $(udp_state "$after"), srvE $(udp_state srvE) 45 s after the block ended"; fi
  udp_count_off
  systemctl stop lab-udp-echo 2>/dev/null
}

# One slot on four DE servers with production's clocks everywhere.
world_oneslot() {
  drop_base_feeds
  write_feeds
  rm -f "$CONF"/feeds/solo*
  write_bootstrap
  jq -n '{sources: [{id: "good"}, {id: "bad"}], groups: [{id: "lab-eu", countries: ["DE"]}], slots: [{id: "eu", exit: "group:lab-eu"}]}' >"$CONF/world.json"
}

# One slot, so nothing but its own server's heartbeats ever fails on the censored target. $1 is the
# target's last octet, $2 how long it stays censored first. Then the active server dies.
step_soloblame() {
  local host=${1:-82} settle=${2:-180} srv since t0 secs low t1 status closed=""
  rm -f "$STATE/world"
  use_world oneslot 1 35 || return
  since=$(now_iso); t1=$(date +%s)
  srv=$(exit_of $EU)
  note "one slot on $srv, target 203.0.113.$host censored for every server for $settle s"
  ip netns exec inet iptables -I FORWARD -s 198.51.100.0/24 -d "203.0.113.$host" -j DROP
  sleep "$settle"
  expect "switch events with one target censored, nothing dead" "$(events_since "$since" eu | grep -c .)" 0
  events_since "$since" eu | cut -c1-170 | indent
  note "campaigns meanwhile: failedProbe $(journalctl -u $DAEMON --since "@$t1" -o cat | grep -c 'slot=eu .*cause=failedProbe')"
  note "results on the censored target by purpose and verdict (1 is a round): $(metrics_sql 'SELECT purpose, verdict, count(*) FROM probe WHERE target = ? AND ts >= ? GROUP BY 1, 2' "t$(( host - 79 ))" "${t1}000" | tr '\n' ';')"
  sleep 35
  srv=$(exit_of $EU); [ "$srv" = none ] && { sleep 20; srv=$(exit_of $EU); }
  note "slot now on $srv and status $(slot_field eu .status)"
  after_beat "$srv"
  since=$(now_iso); t0=$(date +%s.%N)
  note "killing $srv"
  kill_server "$srv"
  ( while ! past "$t0" 150; do status=$(slot_field eu .status); [ "$status" = failed ] && { echo "$(elapsed "$t0") read Failed"; break; }; sleep 0.2; done ) >"$STATE/soloblame.status" &
  if secs=$(wait_move "$since" eu 150 "$t0" ' -> \['); then
    at_most "$secs" $LIMIT && pass "the killed server was replaced in $secs s" || fail "the killed server was replaced in $secs s, past the $LIMIT s"
  else
    fail "no server took over within 150 s"
  fi
  wait
  note "status: $(cat "$STATE/soloblame.status")"
  events_since "$since" eu | cut -c1-170 | indent
  note "traffic now exits via $(exit_of $EU)"
  revive_server "$srv"
  ip netns exec inet iptables -D FORWARD -s 198.51.100.0/24 -d "203.0.113.$host" -j DROP
  rm -f "$STATE/world"
}

# plumbing_down removes what the daemon installs beside its links, as internal/runtime/kernel.go
# owns it: rules 99 and 100 and table 100's route under protocol 85, and the sidegate chains. Stopping
# the daemon leaves them, so without this the next suite starts on them.
plumbing_down() {
  local pref i ref tool table builtin chain
  for pref in 99 100; do
    for i in $(seq 16); do ip rule del pref "$pref" proto 85 2>/dev/null || break; done
  done
  ip route flush table 100 proto 85 2>/dev/null
  for ref in iptables:raw:PREROUTING:sidegate-prerouting iptables:mangle:PREROUTING:sidegate-prerouting \
    iptables:filter:INPUT:sidegate-input iptables:filter:OUTPUT:sidegate-output ip6tables:filter:INPUT:sidegate-input; do
    IFS=: read -r tool table builtin chain <<<"$ref"
    while "$tool" -t "$table" -D "$builtin" -j "$chain" 2>/dev/null; do :; done
    "$tool" -t "$table" -F "$chain" 2>/dev/null
    "$tool" -t "$table" -X "$chain" 2>/dev/null
  done
}

step_teardown() {
  systemctl stop $DAEMON 2>/dev/null
  systemctl stop $CORE_UNIT 2>/dev/null
  teardown_extras
  for n in 1 2 3 4 5 6 7 8; do ip link del "pxa$n" 2>/dev/null; done
  plumbing_down
  rm -f "$STATE/world"
  expect "the daemon's rules and chains left" "$( { ip -N rule show; ip -N route show table 100; iptables-save; ip6tables-save; } 2>/dev/null | grep -cE 'proto 85|sidegate-')" 0
  pass "stopped the daemon and the core, removed the extra servers and the daemon's plumbing"
}

declare -F "step_$STEP" >/dev/null || { echo "unknown step: $STEP" >&2; exit 2; }
shift
"step_$STEP" "$@"
[ "$fails" -eq 0 ]
