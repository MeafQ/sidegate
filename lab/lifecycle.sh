#!/usr/bin/env bash
# Runs as root inside the VM, one lifecycle step per call. The bundle's own sidegated runs
# with no lab flags, on the gateway's real paths: /data/sidegate, /data/unifi-core/config/http
# and the system's nginx, whose config here is a stand-in for UniFi's. Usage:
# lifecycle.sh <prepare|net|watch|snapshot NAME|job|launcher|apply RUN|plan|command unarrived|unchecked|install|again|other|together|refused|interrupted install|uninstall|first-daemon|daemon|install|page|forged|contract|configure|connect|traffic|status|booted|break|broken|repair|heal|upgrade|rollback|update install|plan|refused|busy|moved|nostart|hung|unsaid|land|core|uninstall|paths|offer|cut kill|missing|gone|prior|core|recovery|reboot|back|deadcore|downcore|deliberate|busy|key|keep|reinstall|purge|paths|teardown>
set -uo pipefail

# This script uses the gateway's real paths, so on the gateway it would do real damage.
if [ "$(hostname)" != lima-unifi-lab ] || [ ! -d /mnt/lima-cidata ]; then
  echo "lifecycle.sh runs only in the unifi-lab VM, and this is $(hostname)." >&2
  exit 3
fi
source "$(dirname "$0")/names.sh"
source "$(dirname "$0")/unifi-login.sh"

WORK=$LIFECYCLE_WORK
OWNS_DATA=$WORK/lab-created-data
DATA=$DATA_DIR
HOOKS=/data/unifi-core/config/http
HOOK=$HOOKS/$HOOK_NAME
SOCKET=$HTTP_SOCKET
LOGIN_SOCKET=/run/sglc-login.sock
AUTH_CACHE=/var/cache/sglc-auth
LOGIN="Cookie: UNIFI_TOKEN=lab-login"
URL=http://127.0.0.1:8080
CORE=${CORE_UNIT%.service}
DAEMON=$DAEMON_BIN
UUID=6f1c1e52-0d3c-4b8e-9a51-2c1f7c0a5e11
STATE=/var/lib/unifi-lab
CONF=/etc/unifi-lab
FEED="${FEEDS}good?token=lab-s3cret"
TARGET=203.0.113.81
ONE=$WORK/one/$DAEMON_BIN
TWO=$WORK/two/$DAEMON_BIN
THREE=$WORK/three/$DAEMON_BIN
FOUR=$WORK/four/$DAEMON_BIN
OUTCOME=$WORK/outcome.json
JOURNAL=$DATA/rollback
JOB=sidegate-update
LAUNCHER=sglc-launcher
JOBS=$WORK/job
RELEASE_DIR=$CONF/releases

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
status_of() { curl -s -m 10 -o /dev/null -w '%{http_code}' "$@"; }
page_status() { status_of -H "$LOGIN" "$URL$BASE_PATH"; }
link_up() { [ $(( $(cat "/sys/class/net/$1/flags" 2>/dev/null || echo 0) & 1 )) -eq 1 ] && echo up || echo down; }
active() { systemctl is-active "$1" 2>/dev/null; }
pid_of() { systemctl show -p MainPID --value "$1"; }
state() { curl -s -m 10 -H "$LOGIN" "$URL${BASE_PATH}api/state"; }
state_field() { state | jq -r "$1" 2>/dev/null; }

wait_for() {
  local what=$1 limit=$2 start=$SECONDS; shift 2
  until "$@"; do
    if [ $((SECONDS - start)) -ge "$limit" ]; then fail "$what: not within $limit s"; return 1; fi
    sleep 0.5
  done
  pass "$what, after $((SECONDS - start)) s"
}
page_up() { [ "$(page_status)" = 200 ]; }
slot_ready() { [ "$(state_field '.slots[]? | select(.id == "eu") | .runtime.phase')" = ready ]; }
through_slot() { case "$(ip netns exec lan curl -s -m 3 "http://$TARGET/ip")" in 198.51.100.*) return 0;; esac; return 1; }

firewall() {
  local table
  for table in raw mangle nat filter; do
    "$1" -t "$table" | grep -v '^#' | sed -E 's/\[[0-9]+:[0-9]+\]//'
  done
}

# A new link's IPv6 routes arrive seconds after the link does.
settle_routes() {
  local last="" now stable=0
  while [ "$stable" -lt 6 ]; do
    now=$(ip -6 route show table all | wc -l)
    if [ "$now" = "$last" ]; then stable=$((stable + 1)); else stable=0; fi
    last=$now
    sleep 1
  done
}

# Everything a gateway owner could see of us, with counters taken out.
snapshot() {
  local d=$WORK/snap/$1; mkdir -p "$d"
  { ip rule; ip -6 rule; } >"$d/rules"
  ip -o link show | awk '{print $2, $3}' | sed 's/@[^:]*//' | sort >"$d/links"
  # A rebuilt link gets a random MAC, and so a random link-local address. Its device name stays.
  { ip route show table all; ip -6 route show table all; } | sed -E 's/fe80::[0-9a-f:]+ dev/fe80:: dev/' | sort >"$d/routes"
  # LIMADNS is Lima's own, and its guest agent adds it some seconds after boot.
  # One table at a time, since a whole save prints them in the order their modules loaded.
  firewall iptables-save | grep -v LIMADNS >"$d/iptables"
  firewall ip6tables-save >"$d/ip6tables"
  sysctl -a 2>/dev/null | grep -E '^net\.ipv4\.(ip_forward|conf\.(all|default)\.)' >"$d/sysctl"
  nginx -T >"$d/nginx" 2>&1
  { echo "page $(page_status)"; echo "network $(status_of -H "$LOGIN" "$URL/proxy/labnet/")"; } >"$d/nginx-live"
  { find "$HOOKS" -printf '%p %m %u:%g\n' | sort; find "$HOOKS" -type f -exec sha256sum {} + | sort -k2; } >"$d/hooks"
  ls -1 /etc/systemd/system /etc/systemd/system/multi-user.target.wants >"$d/units"
  systemctl list-units --all --no-legend 'sidegate*' >"$d/units-loaded"
  ls -d /run/sidegate* >"$d/run" 2>/dev/null
  find /data -printf '%p %m %u:%g\n' 2>/dev/null | sort >"$d/data"
}

step_snapshot() { settle_routes; snapshot "$2"; pass "snapshot $2 taken"; }

compare_with_pre() {
  local name=$1 files=${2:-rules links routes iptables ip6tables sysctl nginx nginx-live hooks units units-loaded run data} f
  snapshot "$name"
  for f in $files; do
    if diff "$WORK/snap/pre/$f" "$WORK/snap/$name/$f" >"$WORK/diff"; then
      pass "$f matches the pre-install snapshot ($(wc -l <"$WORK/snap/$name/$f") lines)"
    else
      fail "$f differs from the pre-install snapshot"; cut -c1-160 "$WORK/diff" | head -30 | indent
    fi
  done
}

write_bootstrap() {
  mkdir -p "$DATA"; chmod 700 "$DATA"
  jq -n '{
    probeTargets: [{url: "http://203.0.113.80/ip", status: 200}, {url: "http://203.0.113.83/ip", status: 200}],
    baselineTargets: [{url: "http://203.0.113.80/ip"}, {url: "http://11.0.0.80/ip"}],
    countryRules: [{regex: "^DE ", country: "DE"}]
  }' >"$DATA/$BOOTSTRAP"
  chmod 600 "$DATA/$BOOTSTRAP"
}

feed_line() {
  local s=$1 ip=$2 label=$3 pin
  pin=$(openssl x509 -noout -fingerprint -sha256 -in "$STATE/$s.crt" | cut -d= -f2 | tr -d : | tr A-F a-f)
  echo "vless://$UUID@$ip:443?security=tls&sni=$s.lab&allowInsecure=1&pcs=$pin&flow=xtls-rprx-vision&type=tcp#$label"
}

# The lab's stand-in for the network a booted gateway already has.
step_net() {
  /opt/unifi-lab/topology.sh --no-slots | indent
  ip -n web addr add 11.0.0.80/32 dev eth0
  ip -n inet route add 11.0.0.80/32 dev net
  ip route add 11.0.0.80/32 via 192.168.100.1 dev eth4 table 201
  mkdir -p "$CONF/feeds"
  find "$CONF/feeds" -maxdepth 1 -type f -delete
  { feed_line srvA 198.51.100.10 "DE good-A"; feed_line srvB 198.51.100.20 "DE good-B"; } >"$CONF/feeds/good"
}

# Runs at boot and proves the order from outside: it reads the links first, then the core.
step_watch() {
  local line f up
  while [ "$SECONDS" -lt 300 ]; do
    up=""
    for f in /sys/class/net/pxa*/flags; do
      [ -e "$f" ] && [ $(( $(cat "$f") & 1 )) -eq 1 ] && up="$up $(basename "$(dirname "$f")")"
    done
    line="$(cut -d' ' -f1 /proc/uptime) core=$(systemctl is-active $CORE) up=[$up ]"
    echo "$line" >>/run/sglc-watch.log
    sleep 0.2
  done
}

unit() {
  cat >"/etc/systemd/system/$1.service"
  systemctl enable "$1" >/dev/null 2>&1
}

# Removes only what this script made, by exact name. rmdir takes a directory only if empty.
remove_stand_ins() {
  rm -f "$HOOKS/auth.conf" "$HOOKS/shared-runnable-network.conf" "$HOOKS/shared-runnable-zz-conflict.conf" "$HOOKS/shared-runnable-zz-noauth.conf" "$HOOK"
  rm -rf -- "$DATA" "$AUTH_CACHE"
  rmdir "$HOOKS" /data/unifi-core/config /data/unifi-core /data 2>/dev/null
}

step_prepare() {
  mkdir -p "$WORK"
  if [ -e /data ] && [ ! -e "$OWNS_DATA" ]; then
    fail "/data exists and this script didn't make it, so it stays untouched"
    return
  fi
  systemctl stop lab-control lab-nginx lab-echo $CORE $DAEMON $LAUNCHER $JOB 2>/dev/null
  # A run killed after install leaves the daemon enabled, and its link would sit in the pre-install snapshot.
  rm -f /etc/systemd/system/$CORE.service /etc/systemd/system/$DAEMON.service /etc/systemd/system/multi-user.target.wants/$DAEMON.service /etc/systemd/system/$LAUNCHER.service
  systemctl daemon-reload
  systemctl reset-failed 2>/dev/null
  remove_stand_ins
  find "$WORK/snap" -mindepth 2 -maxdepth 2 -type f -delete 2>/dev/null
  : >"$OWNS_DATA"
  mkdir -p "$HOOKS"
  command -v nginx >/dev/null || DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends nginx-light >/dev/null 2>&1
  [ -f "$WORK/nginx.conf.orig" ] || cp /etc/nginx/nginx.conf "$WORK/nginx.conf.orig"
  cat >/etc/nginx/nginx.conf <<CONF
user www-data;
pid /run/nginx.pid;
error_log /var/log/nginx/error.log warn;
events { worker_connections 64; }
http {
  access_log off;
  proxy_cache_path $AUTH_CACHE keys_zone=auth_responses:1m max_size=1m;
$(unifi_csrf_map)
  server {
    listen 127.0.0.1:8080;
$(unifi_auth_location /__auth "$LOGIN_SOCKET")
    include $HOOKS/shared-runnable-*.conf;
  }
}
CONF
  unifi_auth_conf /__auth >"$HOOKS/auth.conf"
  cat >"$HOOKS/shared-runnable-network.conf" <<CONF
location ^~ /proxy/labnet/ {
    include $HOOKS/auth.conf;
    proxy_pass http://unix:$LOGIN_SOCKET:/app;
}
CONF
  { unifi_login_py; cat <<'PY'; } >"$WORK/login.py"
import http.server, os, socketserver, sys
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/auth": return unifi_auth(self)
        self.send_response(200 if "UNIFI_TOKEN=lab-login" in self.headers.get("Cookie", "") else 401)
        self.send_header("Content-Length", "0"); self.end_headers()
    def log_message(self, *a): pass
class S(socketserver.UnixStreamServer):
    def get_request(self):
        request, _ = super().get_request()
        return request, ("local", 0)
if os.path.exists(sys.argv[1]): os.remove(sys.argv[1])
s = S(sys.argv[1], H); os.chmod(sys.argv[1], 0o666); s.serve_forever()
PY
  unit sglc-login <<UNIT
[Unit]
After=network.target
[Service]
ExecStart=/usr/bin/python3 $WORK/login.py $LOGIN_SOCKET
[Install]
WantedBy=multi-user.target
UNIT
  # Named apart from lab-*, which topology.sh stops.
  unit sglc-net <<UNIT
[Unit]
After=network.target
Before=$DAEMON.service
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/opt/unifi-lab/lifecycle.sh net
[Install]
WantedBy=multi-user.target
UNIT
  unit sglc-watch <<UNIT
[Unit]
DefaultDependencies=no
After=local-fs.target
Before=sglc-net.service $DAEMON.service
[Service]
ExecStart=/opt/unifi-lab/lifecycle.sh watch
[Install]
WantedBy=multi-user.target
UNIT
  systemctl daemon-reload
  step_net
  systemctl enable nginx >/dev/null 2>&1
  systemctl restart nginx sglc-login
  sleep 1
  expect "the stand-in login guards UniFi's own app" "$(status_of "$URL/proxy/labnet/")" 401
  expect "and lets a login through" "$(status_of -H "$LOGIN" "$URL/proxy/labnet/")" 200
  expect "the page before install" "$(page_status)" 404
  ip netns exec lan curl -s -m 5 http://203.0.113.80/ip >/dev/null && pass "the LAN reaches the internet directly" || fail "no direct path"
}

# The daemon's line for the update job.
launch_line() {
  local limit=$1; shift
  systemd-run --unit="$JOB" --collect --no-block --quiet -p RuntimeMaxSec="$limit" -p TimeoutStopSec=300s -- "$@"
}

# Stands in for the daemon: runs the line from inside its own unit, once per request.
step_launcher() {
  local limit cmd rc
  while :; do
    if [ -e "$JOBS/request" ]; then
      read -r limit cmd <"$JOBS/request"
      rm -f "$JOBS/request"
      # shellcheck disable=SC2086
      launch_line "$limit" $cmd >"$JOBS/said" 2>&1
      rc=$?
      echo "$rc" >"$JOBS/answer.tmp" && mv "$JOBS/answer.tmp" "$JOBS/answer"
    fi
    sleep 0.1
  done
}

# Plays apply-update once told to go: reloads systemd and restarts the launcher, as an upgrade
# restarts the daemon, then writes what it saw.
step_apply() {
  local run=$2 before after reload restart
  until [ -e "$JOBS/go" ]; do sleep 0.1; done
  before=$(pid_of $LAUNCHER)
  systemctl daemon-reload; reload=$?
  systemctl restart $LAUNCHER; restart=$?
  after=$(pid_of $LAUNCHER)
  sleep 3
  echo "$run $before $after $reload $restart $(sed -n 's/^0:://p' /proc/self/cgroup)" >"$JOBS/marker.tmp"
  mv "$JOBS/marker.tmp" "$JOBS/marker"
}

launch() {
  local start=$SECONDS
  rm -f "$JOBS/answer"
  echo "$*" >"$JOBS/request.tmp" && mv "$JOBS/request.tmp" "$JOBS/request"
  until [ -e "$JOBS/answer" ]; do
    [ $((SECONDS - start)) -ge 10 ] && { echo "the launcher didn't answer"; return 99; }
    sleep 0.1
  done
  return "$(cat "$JOBS/answer")"
}
job_active() { [ "$(active $JOB)" = active ]; }
job_gone() { [ "$(systemctl show -p LoadState --value $JOB)" = not-found ]; }
job_log() { journalctl -o cat -u $JOB --since "@$1"; }
# MainPID is 0 for a unit that isn't running, so a PID counts only above 1.
live_pid() { [ "${1:-0}" -gt 1 ] 2>/dev/null; }
job_cleanup() {
  systemctl stop $LAUNCHER $JOB
  systemctl reset-failed $LAUNCHER $JOB
  rm -f /etc/systemd/system/$LAUNCHER.service
  systemctl daemon-reload
  rm -rf "$JOBS"
}

# The update job, launched by a service that the job itself restarts, as an update restarts the daemon.
step_job() {
  local run got='' before='' after='' reload='' restart='' cgroup='' launcher now bin failing since pid start
  note "$(systemctl --version | head -1)"
  # An interrupted step would leave the launcher running and the job waiting out its 20 minutes.
  trap 'job_cleanup >/dev/null 2>&1' EXIT
  trap 'exit 130' INT TERM HUP PIPE
  rm -rf "$JOBS"; mkdir -p "$JOBS"
  printf '[Service]\nExecStart=%s launcher\n' "$(readlink -f "$0")" >/etc/systemd/system/$LAUNCHER.service
  systemctl daemon-reload
  systemctl start $LAUNCHER; expect "the launcher's start" "$?" 0
  launcher=$(pid_of $LAUNCHER)
  live_pid "$launcher" && pass "the launcher runs as PID $launcher" || fail "the launcher's PID = '$launcher'"
  run=$(date +%s%N)

  launch 20min "$(readlink -f "$0")" apply "$run"; expect "the launcher's line" "$?" 0
  wait_for "the job runs" 10 job_active
  expect "the job's type, which RuntimeMaxSec acts on" "$(systemctl show -p Type --value $JOB)" simple
  expect "the job's limit" "$(systemctl show -p RuntimeMaxUSec --value $JOB)" 20min
  launch 20min /bin/true; expect "a second launch while one runs" "$?" 1
  grep -q "Unit $JOB.service already exists" "$JOBS/said" && pass "systemd-run says the unit exists" || fail "systemd-run said: $(cat "$JOBS/said")"
  touch "$JOBS/go"
  wait_for "the job finishes its work" 30 test -e "$JOBS/marker"
  read -r got before after reload restart cgroup <"$JOBS/marker"
  expect "the marker is this run's" "$got" "$run"
  expect "the launcher's restart inside the job" "$restart" 0
  now=$(pid_of $LAUNCHER)
  live_pid "$launcher" && [ "$before" = "$launcher" ] && live_pid "$after" && [ "$after" != "$before" ] && [ "$now" = "$after" ] &&
    pass "the job restarted its launcher, PID $before to $after, and lived on" ||
    fail "launcher PIDs: started $launcher, the job saw $before then $after, now $now"
  expect "daemon-reload inside the job" "$reload" 0
  expect "the job's cgroup" "${cgroup##*/}" "$JOB.service"
  wait_for "--collect removes the finished unit" 10 job_gone

  launch 20min "$DATA/releases/v0.0.0/$DAEMON_BIN" apply-update; expect "a launch of a binary that isn't there" "$?" 1
  grep -q 'Failed to find executable' "$JOBS/said" && pass "systemd-run refuses it before making a unit" || fail "systemd-run said: $(cat "$JOBS/said")"
  job_gone && pass "and no unit is left" || fail "a unit is loaded: $(active $JOB)"

  bin=$JOBS/broken
  printf 'not a program\n' >"$bin"; chmod 755 "$bin"
  for failing in "$bin:203/EXEC" "/bin/false:1/FAILURE"; do
    since=$(date +%s)
    launch 20min "${failing%%:*}"; expect "the launch of ${failing%%:*}" "$?" 0
    wait_for "--collect removes the failed unit" 10 job_gone
    job_log "$since" | grep -q "status=${failing#*:}" && pass "the journal holds its status=${failing#*:}" || { fail "no status=${failing#*:} in the journal"; job_log "$since" | indent; }
  done
  launch 20min /bin/touch "$JOBS/after-failure"; expect "a launch after failures" "$?" 0
  wait_for "and it runs" 10 test -e "$JOBS/after-failure"
  wait_for "and is collected" 10 job_gone

  since=$(date +%s)
  launch 3s /bin/sleep 300; expect "the launch of a sleeper with a 3 s limit" "$?" 0
  wait_for "the sleeper runs" 10 job_active
  pid=$(pid_of $JOB); start=$SECONDS
  wait_for "RuntimeMaxSec ends it and the unit is collected" 15 job_gone
  [ $((SECONDS - start)) -le 5 ] || fail "it took $((SECONDS - start)) s from its start"
  if ! live_pid "$pid"; then fail "the sleeper's PID = '$pid'"
  elif kill -0 "$pid" 2>/dev/null; then fail "the sleeper, PID $pid, is still alive"
  else pass "the sleeper, PID $pid, is gone"; fi
  job_log "$since" | grep -q 'runtime time limit' && pass "the journal says it hit its runtime limit" || { fail "no runtime limit in the journal"; job_log "$since" | indent; }

  trap - EXIT INT TERM HUP PIPE
  job_cleanup 2>/dev/null
  [ "$(systemctl show -p LoadState --value $LAUNCHER)" = not-found ] && job_gone && [ ! -e "$JOBS" ] &&
    pass "the launcher, the job and their files are gone" ||
    fail "left behind: launcher $(systemctl show -p LoadState --value $LAUNCHER), job $(active $JOB), $(ls -d "$JOBS" 2>/dev/null)"
}

step_plan() {
  local out; out=$("$ONE" install --bundle "$WORK/one.tar.gz" 2>&1); expect "install without --yes exits" "$?" 0
  echo "$out" | indent
  out=$("$ONE" uninstall 2>&1); expect "uninstall without --yes exits" "$?" 0
  echo "$out" | indent
  compare_with_pre plan
}

# The one command, pasted into a terminal as root, against the stand-in release server, whose files
# release-files.sh wrote from real bundles.

# publish serves these releases and no other, each file as it is.
publish() {
  local tag
  rm -rf -- "$RELEASE_DIR"
  for tag; do
    mkdir -p "$RELEASE_DIR/$tag"
    cp "$WORK/release/$tag/files/"* "$RELEASE_DIR/$tag/"
  done
}

mode() { rm -f "$RELEASE_DIR/served"; /opt/unifi-lab/labweb release-mode "$@" || fail "the stand-in refused the mode $*"; }
# What the stand-in answered since the mode was set, as it recorded it: each file, how, and the status.
served() { cat "$RELEASE_DIR/served" 2>/dev/null; }

one_command() { grep '^curl ' "$WORK/release/v0.0.1/NOTES.md"; }

# paste_command types the one command into root's login shell on a terminal, and keeps what the
# terminal showed in $TERMINAL. PASTE_ENV is what else that shell's environment holds.
TERMINAL=$WORK/terminal
PASTE_ENV=()
paste_command() {
  printf '%s\nexit\n' "$(one_command)" |
    env -i HOME=/root USER=root LOGNAME=root SHELL=/bin/bash TERM=vt100 HISTFILE= PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
      "${PASTE_ENV[@]}" script -qec 'bash -il' /dev/null | tr -d '\r' >"$TERMINAL"
  return "${PIPESTATUS[1]}"
}
showed() { grep -qF -- "$1" "$TERMINAL"; }
expect_showed() { showed "$1" && pass "the terminal shows: $1" || { fail "the terminal doesn't show: $1"; tail -5 "$TERMINAL" | indent; }; }
# The script speaks first to say what it adds, and starts each failure with its name.
script_spoke() { grep -qE '^sidegate(:| v)|^Installed\.' "$TERMINAL"; }

# Everywhere the script or the installer writes, and what they start.
traces() {
  find /data /etc/systemd/system /root /tmp -name 'systemd-private-*' -prune -o -type f -printf '%p %m %s\n' -o -printf '%p %m\n' | LC_ALL=C sort
  ls -d /run/sidegate* 2>/dev/null
  pgrep -a "^$DAEMON"
  nginx -T 2>/dev/null | sha256sum
}
expect_nothing_left() {
  traces >"$WORK/traces"
  if diff "$WORK/traces-before" "$WORK/traces" >"$WORK/diff"; then pass "$1: nothing is installed and nothing is left behind"
  else fail "$1: something is left behind"; head -10 "$WORK/diff" | indent; fi
}

staging() { find /data -mindepth 1 -maxdepth 1 -name 'sidegate-install.*'; }
expect_no_staging() { [ -z "$(staging)" ] && pass "no staging is left in /data" || fail "staging is left: $(staging | xargs)"; }
expect_kept_log() {
  local dir; dir=$(staging)
  expect "what a failed install keeps" "$(ls -A $dir | xargs)" install.log
  [ "$(grep -c . <<<"$dir")" = 1 ] && grep -qF "$1" "$dir/install.log" && showed "The whole log: $dir/install.log" &&
    pass "its log says so, and stays where the terminal says" || fail "staging '$dir', log: $(tail -2 "$dir/install.log" 2>&1)"
}
expect_version() { expect "the installed sidegated's version" "$("$DATA/current/$DAEMON_BIN" version | xargs)" "$1 $RELEASES"; }
session_version() { curl -s -m 10 -H "$LOGIN" "$URL${BASE_PATH}api/session" | jq -r .version 2>/dev/null; }
serves_version() { [ "$(session_version)" = "$1" ]; }
command_uninstall() {
  local out; out=$("$DATA/current/$DAEMON_BIN" uninstall --purge --yes 2>&1); expect "uninstall --purge exits" "$?" 0
  [ -e "$DATA" ] && { fail "$DATA is left"; echo "$out" | tail -3 | indent; }
}

# The script reaches sh cut, empty or not at all: every byte of its last line, the braces it opens
# with and some way into it, with its length declared and without, over HTTP/2 and HTTP/1.1.
command_unarrived() {
  local script=$RELEASE_DIR/v0.0.1/install.sh size last opening at kind http runs spoke left named asked status c
  local -A statuses=()
  size=$(stat -c %s "$script"); last=$(tail -n 1 "$script" | wc -c); opening=$(grep -b -m1 '^{ main() {' "$script" | cut -d: -f1)
  expect "the script's last line" "$(tail -n 1 "$script")" '{ main </dev/null; }'
  mkdir -p "$WORK/http1.1"; echo http1.1 >"$WORK/http1.1/.curlrc"
  for http in 2 1.1; do
    PASTE_ENV=(); [ "$http" = 1.1 ] && PASTE_ENV=(CURL_HOME="$WORK/http1.1")
    for kind in cut short; do
      runs=0 spoke=0 left=0 named=0 asked=0 statuses=()
      for at in 0 1 $(seq "$opening" $((opening + 10))) $((size / 4)) $((size / 2)) $((size * 3 / 4)) $(seq $((size - last)) $((size - 2))); do
        mode install.sh $kind "$at"
        paste_command; status=$?
        runs=$((runs + 1)); statuses[$status]=1
        if [ "$(served)" = "install.sh $kind $at 200" ]; then asked=$((asked + 1)); else note "at byte $at the stand-in answered: $(served | xargs)"; fi
        script_spoke && { spoke=$((spoke + 1)); note "at byte $at: $(tail -3 "$TERMINAL" | xargs)"; }
        grep -qE '^curl: \((18|92)\)' "$TERMINAL" && named=$((named + 1))
        diff "$WORK/traces-before" <(traces) >/dev/null || { left=$((left + 1)); note "at byte $at something is left"; }
      done
      note "HTTP/$http, $kind: the pipe's statuses were ${!statuses[*]}, and curl named the cut in $named of $runs runs"
      expect "runs where the stand-in answered $kind, as asked" "$asked" "$runs"
      expect "the script's own words in $runs runs of it $kind over HTTP/$http, the last line at each byte" "$spoke" 0
      expect "runs that left anything behind" "$left" 0
      [ $kind = short ] || expect "runs where curl names the cut" "$named" "$runs"
    done
  done
  PASTE_ENV=()
  for c in 'empty 200|' '404 404|curl: (22) The requested URL returned error: 404' 'http 302|curl: (1) Protocol "http" not supported or disabled'; do
    kind=${c%% *}
    mode install.sh "$kind"
    paste_command; note "install.sh $kind: the pipe's status is $?"
    expect "what the stand-in answered" "$(served)" "install.sh ${c%%|*}"
    script_spoke && fail "the script ran: $(tail -3 "$TERMINAL")" || pass "install.sh $kind: the script never ran"
    [ -z "${c#*|}" ] || expect_showed "${c#*|}"
    expect_nothing_left "install.sh $kind"
  done
}

# The script arrives whole and refuses the bundle it was told to check.
command_unchecked() {
  local file kind says curl bundle=sidegate-v0.0.1-arm64.tar.gz
  while IFS='|' read -r file kind says curl; do
    mode "$file" "$kind"
    paste_command; expect "$file $kind: the command exits" "$?" 1
    expect_showed "sidegate: $says"
    [ -z "$curl" ] || expect_showed "curl: $curl"
    expect_nothing_left "$file $kind"
  done <<CASES
$bundle|changed|the bundle doesn't match its checksum|
$bundle|short|the bundle doesn't match its checksum|
$bundle|cut|the download of $bundle failed|(18) transfer closed with
$bundle|404|the download of $bundle failed|(22) The requested URL returned error: 404
$bundle|http|the download of $bundle failed|(1) Protocol "http" not supported or disabled
SHA256SUMS|empty|SHA256SUMS isn't the bundle's one line|
SHA256SUMS|404|the download of SHA256SUMS failed|(22) The requested URL returned error: 404
CASES
}

command_install() {
  [ ! -e "$DATA" ] && pass "no sidegate is on this gateway" || fail "$DATA is there, so this proves nothing"
  paste_command; expect "the command exits" "$?" 0
  grep '^[A-Z]\|^sidegate' "$TERMINAL" | cut -c1-160 | indent
  expect_showed "Installed. Open https://<this gateway>/sidegate/"
  expect_no_staging
  expect_version v0.0.1
  expect "the daemon is the served bundle's" "$(sha256sum <"$DATA/current/$DAEMON_BIN")" "$(tar -xzOf "$RELEASE_DIR/v0.0.1/sidegate-v0.0.1-arm64.tar.gz" $DAEMON_BIN | sha256sum)"
  expect "$DAEMON" "$(active $DAEMON)" active
  expect "$DAEMON at boot" "$(systemctl is-enabled $DAEMON)" enabled
  wait_for "the signed-in page answers through nginx" 30 page_up
  expect "the version it says" "$(session_version)" v0.0.1
  expect "the page with no login" "$(status_of "$URL$BASE_PATH")" 401
}

command_again() {
  local release; release=$(release_now)
  paste_command; expect "the command again exits" "$?" 0
  expect_showed "Installed. Open https://<this gateway>/sidegate/"
  expect_no_staging
  expect "the release" "$(release_now)" "$release"
  expect "the releases left" "$(ls "$DATA/releases" | wc -l)" 1
  expect_version v0.0.1
  expect "$DAEMON" "$(active $DAEMON)" active
  expect "$DAEMON at boot" "$(systemctl is-enabled $DAEMON)" enabled
  wait_for "the signed-in page answers" 30 page_up
}

# Over another release the command refuses in both directions, and names the command that moves a newer one.
command_other() {
  local pid out says="sidegate v0.0.1 is installed, and this is v0.0.2. Update it: $DATA/current/$DAEMON_BIN update --yes"
  pid=$(pid_of $DAEMON)
  publish v0.0.1 v0.0.2
  paste_command; expect "the command over v0.0.1, with v0.0.2 the latest, exits" "$?" 1
  expect_showed "$says"
  expect_kept_log "$says"
  rm -rf -- "$(staging)"
  expect_version v0.0.1
  expect "the daemon's PID" "$(pid_of $DAEMON)" "$pid"
  expect "the version the page says" "$(session_version)" v0.0.1
  out=$(${says##*: } 2>&1); expect "the command it names exits" "$?" 0
  echo "$out" | cut -c1-160 | indent
  expect_version v0.0.2
  wait_for "the signed-in page says v0.0.2" 30 serves_version v0.0.2
  paste_command; expect "the command over v0.0.2, now the latest, exits" "$?" 0
  expect_no_staging
  publish v0.0.1
  says="sidegate v0.0.2 is installed, and this is v0.0.1. To change to it, run upgrade --bundle with the bundle's own sidegated"
  paste_command; expect "the command over v0.0.2, with v0.0.1 the latest, exits" "$?" 1
  expect_showed "$says"
  expect_kept_log "$says"
  rm -rf -- "$(staging)"
  expect_version v0.0.2
  wait_for "the signed-in page still says v0.0.2" 30 serves_version v0.0.2
  command_uninstall
  compare_with_pre command
}

# Two at once, each in its own terminal. The one that holds the lock waits in nginx -t until the
# other has ended.
command_together() {
  local slow=$WORK/slow-nginx go=$WORK/go a b first second start=$SECONDS
  mkdir -p "$slow"; rm -f "$go"
  printf '#!/bin/sh\n[ "$1" != -t ] || until [ -e %s ]; do sleep 0.1; done\nexec %s "$@"\n' "$go" "$(command -v nginx)" >"$slow/nginx"
  chmod 755 "$slow/nginx"
  PASTE_ENV=(PATH="$slow:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
  (TERMINAL=$WORK/terminal-a; paste_command) & a=$!
  (TERMINAL=$WORK/terminal-b; paste_command) & b=$!
  while kill -0 $a 2>/dev/null && kill -0 $b 2>/dev/null && [ $((SECONDS - start)) -lt 50 ]; do sleep 0.1; done
  if kill -0 $a 2>/dev/null; then first=b second=a; else first=a second=b; fi
  kill -0 "${!second}" 2>/dev/null && pass "one ended while the other waited in nginx -t" || fail "neither or both waited"
  touch "$go"
  wait "${!first}"; expect "the first to end exits" "$?" 1
  TERMINAL=$WORK/terminal-$first
  expect_showed "another install, upgrade or uninstall holds"
  wait "${!second}"; expect "the other exits" "$?" 0
  expect_kept_log "another install, upgrade or uninstall holds"
  TERMINAL=$WORK/terminal-$second
  expect_showed "Installed. Open https://<this gateway>/sidegate/"
  rm -rf -- "$(staging)" "$slow" "$go"
  expect_version v0.0.1
  wait_for "the signed-in page answers" 30 page_up
  expect "the version it says" "$(session_version)" v0.0.1
  command_uninstall
  compare_with_pre command
}

step_command() {
  local how=$2
  publish v0.0.1
  expect "the command the release's notes give" "$(one_command)" "curl --proto '=https' --tlsv1.2 -fsSL $RELEASES/latest/download/install.sh | sh"
  traces >"$WORK/traces-before"
  "command_$how"
  rm -rf -- "$RELEASE_DIR"
}

# The updater, on a gateway whose release is v0.0.1 with a slot that serves, against the stand-in release
# server: update over SSH, and the page's routes through nginx with the UniFi login and both CSRF tokens.
UPDATE_OUT=$WORK/update.out
updates() { curl -s -m 20 -H "$LOGIN" "$URL${BASE_PATH}api/update"; }
update_field() { updates | jq -r "$1" 2>/dev/null; }
last_update() { update_field '"\(.last.outcome) \(.last.reason)"'; }
last_is() { [ "$(last_update)" = "$1" ]; }
# post_update ROUTE BODY leaves the answer's body in $WORK/answer and prints its status.
post_update() {
  curl -s -m 60 -o "${3:-$WORK/answer}" -w '%{http_code}' -X POST -H "$LOGIN" -H "X-Csrf-Token: $(unifi_token "$LOGIN" "$URL${BASE_PATH}api/state")" \
    -H "$CSRF_HEADER: $(curl -s -H "$LOGIN" "$URL${BASE_PATH}api/session" | jq -r .csrf)" -H 'Content-Type: application/json' -d "$2" "$URL${BASE_PATH}api/$1"
}
answered() { jq -r "$1" "$WORK/answer" 2>/dev/null; }
update_ssh() { "$DATA/current/$DAEMON_BIN" update --data-dir "$DATA" "$@" >"$UPDATE_OUT" 2>&1; }
downloading() { job_active && ls "$DATA"/update/*/staged/*.part >/dev/null 2>&1; }
BUNDLE=sidegate-v0.0.2-arm64.tar.gz

# untouched marks what a failed update must leave, and expect_untouched checks it.
untouched() { BEFORE=$(update_marks); }
update_marks() { echo "$(release_now) $(pid_of $DAEMON) $(pid_of $CORE) $(ls "$DATA/releases" "$DATA/cores" | xargs)"; }
expect_untouched() {
  expect "the release, the daemon's and the core's PIDs, and the releases" "$(update_marks)" "$BEFORE"
  journal_gone && pass "no journal" || fail "a journal is there"
  [ -z "$(ls -d "$DATA"/update/*/staged 2>/dev/null)" ] && pass "nothing staged is left" || fail "staged: $(ls -d "$DATA"/update/*/staged)"
  [ ! -e /run/sidegate-lifecycle.lock ] && pass "the lock's file is gone" || fail "the lock's file stayed"
  through_slot && pass "a LAN client's traffic exits through the slot" || fail "no traffic through the slot"
}
# $1 is the version the gateway must be back on.
expect_back() {
  wait_for "the job is gone, collected" 120 job_gone
  wait_for "the signed-in page says $1" 60 serves_version "$1"
  expect_version "$1"
  wait_for "the update says rolledBack" 30 last_is "rolledBack null"
  update_field .last.message | cut -c1-200 | indent
  wait_for "the journal is gone" 30 journal_gone
  wait_for "the slot serves" 120 slot_ready
  wait_for "a LAN client's traffic exits through the slot" 40 through_slot
}

update_install() {
  write_bootstrap
  publish v0.0.1
  paste_command; expect "the one command exits" "$?" 0
  expect_version v0.0.1
  wait_for "the signed-in page says v0.0.1" 30 serves_version v0.0.1
  expect "a check through the page, with nothing newer" "$(post_update update/check '{"auto": true}') $(answered '"\(.releases) \(.check.failure) \(.offered)"')" "200 true null null"
}

update_plan() {
  local tree
  publish v0.0.1 v0.0.2
  untouched
  tree=$(find "$DATA" | sort | sha256sum)
  update_ssh; expect "update exits" "$?" 0
  indent <"$UPDATE_OUT"
  grep -q '^update v0.0.1 to v0.0.2' "$UPDATE_OUT" && grep -q 'no flow drops' "$UPDATE_OUT" && grep -q '^Nothing changed' "$UPDATE_OUT" && pass "it shows the plan" || fail "it doesn't"
  expect "the data dir" "$(find "$DATA" | sort | sha256sum)" "$tree"
  expect_untouched
  publish v0.0.1
  update_ssh --yes; expect "update --yes with nothing newer exits" "$?" 0
  grep -q 'v0.0.1 is the newest release' "$UPDATE_OUT" && pass "it says so" || fail "it says $(cat "$UPDATE_OUT")"
  "$ONE" update --yes >"$UPDATE_OUT" 2>&1; expect "a development build's update exits" "$?" 1
  grep -q 'takes no updates' "$UPDATE_OUT" && pass "it says so" || fail "it says $(cat "$UPDATE_OUT")"
  "$WORK/repo/$DAEMON_BIN" update --yes >"$UPDATE_OUT" 2>&1; expect "a bundle's own update exits" "$?" 1
  grep -q "run the installed sidegated" "$UPDATE_OUT" && pass "it says so" || fail "it says $(cat "$UPDATE_OUT")"
}

# Every refusal of the job leaves the gateway as it was, and the next launch runs at once.
update_refused() {
  local file kind reason free
  publish v0.0.1 v0.0.2
  untouched
  while IFS='|' read -r file kind reason; do
    mode "$file" $kind
    update_ssh --yes; expect "update --yes with $file $kind exits" "$?" 1
    tail -1 "$UPDATE_OUT" | cut -c1-200 | indent
    expect "the page reads" "$(last_update)" "failed $reason"
    expect_untouched
  done <<CASES
$BUNDLE|changed|checksum
$BUNDLE|404|downloadFailed
$BUNDLE|cut|downloadFailed
$BUNDLE|short|checksum
$BUNDLE|endless|checksum
$BUNDLE|http|downloadFailed
CASES
  mode
  mode sidegate.json 404
  update_ssh --yes; expect "update --yes with no manifest exits" "$?" 1
  grep -q 'the latest release isn.t known, and nothing changed' "$UPDATE_OUT" && pass "it says so" || fail "it says $(cat "$UPDATE_OUT")"
  expect "a check through the page" "$(post_update update/check '{"auto": false}') $(answered .check.failure)" "200 filesUnavailable"
  mode
  # The files are another release's: v0.0.3's name over v0.0.2's bundle.
  cp "$WORK/release/v0.0.2/files/$BUNDLE" "$RELEASE_DIR/v0.0.2/sidegate-v0.0.3-arm64.tar.gz"
  mv "$RELEASE_DIR/v0.0.2" "$RELEASE_DIR/v0.0.3"
  sed -i 's/v0\.0\.2/v0.0.3/g' "$RELEASE_DIR/v0.0.3/sidegate.json"
  update_ssh --yes; expect "update --yes to a tag that isn't its bundle's exits" "$?" 1
  tail -1 "$UPDATE_OUT" | cut -c1-200 | indent
  expect "the page reads" "$(last_update)" "failed wrongRelease"
  expect_untouched
  publish v0.0.1 v0.0.2
  # The filesystem the job asks about is /data's.
  free=$(df --output=avail -B1 "$DATA" | tail -1 | xargs)
  fallocate -l $((free - 100000000)) "$WORK/filler"; expect "the disk is filled to 100 MB, so the filler's exit" "$?" 0
  update_ssh --yes; expect "update --yes with no room exits" "$?" 1
  rm -f "$WORK/filler"
  tail -1 "$UPDATE_OUT" | cut -c1-200 | indent
  expect "the page reads" "$(last_update)" "failed noRoom"
  expect_untouched
}

# While the job downloads, every other lifecycle command is busy and every press is answered with it, also
# after the daemon restarted. Then every process of it is killed.
update_busy() {
  local out follower attempt since=$SECONDS
  publish v0.0.1 v0.0.2
  untouched
  mode "$BUNDLE" slow 200000
  update_ssh --yes & follower=$!
  wait_for "the job downloads" 20 downloading
  out=$("$WORK/repo/v0.0.2/$DAEMON_BIN" upgrade --bundle "$WORK/repo/$BUNDLE" --yes 2>&1); expect "an SSH upgrade during it exits" "$?" 1
  grep -q 'another install, upgrade or uninstall holds' <<<"$out" && pass "as busy" || fail "it says $out"
  out=$("$DATA/current/$DAEMON_BIN" uninstall --yes 2>&1); expect "an uninstall during it exits" "$?" 1
  grep -q 'another install, upgrade or uninstall holds' <<<"$out" && pass "as busy" || fail "it says $out"
  "$DATA/current/$DAEMON_BIN" update --yes >"$WORK/update-2.out" 2>&1; expect "another update --yes exits" "$?" 1
  attempt=$(update_field .running.attempt)
  expect "the page reads the update that runs" "$(update_field '"\(.running.version) \(.last)"') ${#attempt}" "v0.0.2 null 16"
  expect "a press is answered with it" "$(post_update update '{"version": "v0.0.2", "restartsCore": false}') $(answered .running.attempt)" "202 $attempt"
  systemctl restart $DAEMON
  wait_for "the daemon is back" 30 page_up
  expect "and reads the same update" "$(update_field .running.attempt)" "$attempt"
  systemctl kill -s KILL $JOB
  wait "$follower"; expect "update --yes, following it, exits" "$?" 1
  tail -1 "$UPDATE_OUT" | cut -c1-200 | indent
  wait_for "the job is gone, collected" 20 job_gone
  expect "the page reads" "$(last_update)" "failed stopped"
  BEFORE="$(release_now) $(pid_of $DAEMON) ${BEFORE#* * }"
  wait_for "the slot serves" 60 slot_ready
  # A killed job leaves its lock's file, which the next command takes.
  [ -e /run/sidegate-lifecycle.lock ] && rm -f /run/sidegate-lifecycle.lock
  expect_untouched
  mode
}

# An SSH upgrade lands after the job is running and before it takes the lock.
update_moved() {
  local slow=$WORK/slow-run go=$WORK/go pid follower
  publish v0.0.1 v0.0.2
  mkdir -p "$slow"; rm -f "$go"
  printf '#!/bin/sh\n%s "$@"; rc=$?\nuntil [ -e %s ]; do sleep 0.1; done\nexit $rc\n' "$(command -v systemd-run)" "$go" >"$slow/systemd-run"
  chmod 755 "$slow/systemd-run"
  PATH="$slow:$PATH" update_ssh --yes & follower=$!
  wait_for "the job runs" 20 job_active
  pid=$(pid_of $JOB)
  wait_for "as sidegated" 10 grep -q apply-update "/proc/$pid/cmdline"
  kill -STOP "$pid"
  [ ! -e "$DATA"/update/*/staged/"$BUNDLE".part ] && pass "it waits for the lock its launcher holds" || fail "it went on"
  touch "$go"
  sleep 2
  "$WORK/repo/v0.0.2/$DAEMON_BIN" upgrade --bundle "$WORK/repo/$BUNDLE" --yes >"$WORK/upgrade.out" 2>&1; expect "an SSH upgrade to v0.0.2 exits" "$?" 0
  kill -CONT "$pid"
  wait "$follower"; expect "update --yes exits" "$?" 1
  tail -1 "$UPDATE_OUT" | cut -c1-200 | indent
  wait_for "the signed-in page says v0.0.2" 30 serves_version v0.0.2
  wait_for "and that the update moved" 20 last_is "failed moved"
  untouched
  expect_untouched
  "$WORK/repo/v0.0.1/$DAEMON_BIN" upgrade --bundle "$WORK/repo/sidegate-v0.0.1-arm64.tar.gz" --yes >"$WORK/upgrade.out" 2>&1; expect "the downgrade over SSH, by its own bundle, exits" "$?" 0
  wait_for "the signed-in page says v0.0.1" 30 serves_version v0.0.1
  wait_for "the slot serves" 60 slot_ready
  rm -rf "$slow" "$go"
}

# The job's binary is gone, which systemd-run refuses, or is no program, which the unit fails on.
update_nostart() {
  local bin how
  publish v0.0.1 v0.0.2
  bin=$(readlink -f "$DATA/current/$DAEMON_BIN")
  expect "a check through the page" "$(post_update update/check '{"auto": false}') $(answered .offered.version)" "200 v0.0.2"
  untouched
  for how in gone broken; do
    mv "$bin" "$WORK/daemon.real"
    [ "$how" = broken ] && { echo 'not a program' >"$bin"; chmod 755 "$bin"; }
    expect "a press with the job's binary $how" "$(post_update update '{"version": "v0.0.2", "restartsCore": false}')" 202
    wait_for "reads as never started" 20 last_is "failed noStart"
    update_field .last.message | cut -c1-200 | indent
    wait_for "and no unit is left" 10 job_gone
    mv -f "$WORK/daemon.real" "$bin"
    expect_untouched
  done
}

# v0.0.3's daemon runs and never answers. Its update is cut while it waits for it, and the gateway ends on
# v0.0.1, which answers: the upgrade killed alone, the job killed alone, every process killed, and a stop. An
# upgrade that lives gets a SIGTERM and rolls back itself, and one that doesn't is the stop-post's.
hung() { [ "$(release_now)" != "$ONE_RELEASE" ] && job_log "$SINCE" | grep -q 'wait for release' && pgrep -f 'sleep 1000' >/dev/null; }
update_hung() {
  local how follower core by
  publish v0.0.1 v0.0.3
  for how in upgrade job all stop; do
    ONE_RELEASE=$(release_now) SINCE=$(date +%s); core=$(pid_of $CORE)
    note "--- $how"
    update_ssh --yes & follower=$!
    wait_for "the daemon that hangs is what runs" 60 hung || return
    case $how in
      upgrade) pkill -KILL -f "$DAEMON_BIN upgrade --data-dir" ;;
      job) kill -9 "$(pid_of $JOB)" ;;
      all) systemctl kill -s KILL $JOB ;;
      stop) systemctl stop $JOB ;;
    esac
    wait "$follower"; expect "update --yes, following it, exits" "$?" 1
    tail -1 "$UPDATE_OUT" | cut -c1-200 | indent
    expect_back v0.0.1
    case $how in job | stop) by="context canceled. Release $ONE_RELEASE is back and answers" ;; *) by="didn't finish. Release $ONE_RELEASE is back and answers" ;; esac
    grep -qF "$by" <<<"$(update_field .last.message)" && pass "what put it back says: $by" || fail "the message isn't: $by"
    expect "the release" "$(release_now)" "$ONE_RELEASE"
    expect "the core's PID, since its binary never changed" "$(pid_of $CORE)" "$core"
    pgrep -f 'sleep 1000' >/dev/null && fail "the daemon that hung is still there" || pass "the daemon that hung is gone"
  done
}

# Every process of the update is killed once its candidate is current, and something else holds the lock from
# the old daemon's pre-start, which has put its release back, until that daemon has answered for seconds. It
# says nothing under the lock, as when the stop-post holds it for a look, and rolledBack once it is free.
restored() { [ "$(jq -r .phase "$JOURNAL/record.json" 2>/dev/null)" = restored ]; }
update_unsaid() {
  local follower dropin=/run/systemd/system/$DAEMON.service.d holder=sglc-holder
  publish v0.0.1 v0.0.3
  ONE_RELEASE=$(release_now) SINCE=$(date +%s)
  # After the unit's own, which is the recovery.
  mkdir -p "$dropin"
  cat >"$WORK/hold" <<HOLD
#!/bin/sh
grep -q '"phase":"restored"' $JOURNAL/record.json 2>/dev/null || exit 0
systemd-run --unit=$holder --collect --quiet flock /run/sidegate-lifecycle.lock sh -c 'touch $WORK/held; while [ -e $WORK/held ]; do sleep 0.1; done'
until [ -e $WORK/held ]; do sleep 0.1; done
HOLD
  chmod 755 "$WORK/hold"
  printf '[Service]\nExecStartPre=%s\n' "$WORK/hold" >"$dropin/hold.conf"
  systemctl daemon-reload
  update_ssh --yes & follower=$!
  wait_for "the daemon that hangs is what runs" 60 hung || return
  expect "the release the kill leaves current" "$([ "$(release_now)" != "$ONE_RELEASE" ] && echo another)" another
  systemctl kill -s KILL $JOB
  wait_for "the old release's pre-start put it back, and the lock is held" 60 test -e "$WORK/held"
  wait_for "the signed-in page says v0.0.1" 60 serves_version v0.0.1
  sleep 3
  restored && pass "it has answered for 3 s and not said so" || fail "the record is $(jq -r .phase "$JOURNAL/record.json" 2>&1)"
  expect "what the outcome says" "$(cat "$DATA"/update/*/outcome.json 2>/dev/null)" ""
  rm -f "$WORK/held"
  wait "$follower"; expect "update --yes, following it, exits" "$?" 1
  expect_back v0.0.1
  expect "the release" "$(release_now)" "$ONE_RELEASE"
  [ ! -e /run/sidegate-lifecycle.lock ] && pass "the lock's file is gone" || fail "the lock's file stayed"
  rm -rf -- "$dropin" "$WORK/hold"
  systemctl daemon-reload
}

# Two presses at once through the page, with a download open through the slot.
update_land() {
  local core_pid core_id daemon_pid flag=$WORK/watching size=80000000 watcher download a b attempt start=$SECONDS
  publish v0.0.1 v0.0.2
  core_pid=$(pid_of $CORE); core_id=$(systemctl show -p InvocationID --value $CORE); daemon_pid=$(pid_of $DAEMON)
  expect "a check through the page" "$(post_update update/check '{"auto": false}') $(answered '"\(.offered.version) \(.offered.restartsCore) \(.offered.notes)"')" "200 v0.0.2 false $RELEASES/tag/v0.0.2"
  expect "a press for another version" "$(post_update update '{"version": "v0.0.9", "restartsCore": false}') $(answered .reason)" "422 offerChanged"
  expect "a press through nginx with no UniFi token" "$(status_of -X POST -H "$LOGIN" -H 'Content-Type: application/json' -d '{"version": "v0.0.2", "restartsCore": false}' "$URL${BASE_PATH}api/update")" 403
  expect "a press with UniFi's token and not the daemon's" "$(status_of -X POST -H "$LOGIN" -H "X-Csrf-Token: $(unifi_token "$LOGIN" "$URL${BASE_PATH}api/state")" -H 'Content-Type: application/json' -d '{"version": "v0.0.2", "restartsCore": false}' "$URL${BASE_PATH}api/update")" 403
  : >"$flag"
  gate_watch "$flag" >"$WORK/downs" & watcher=$!
  ip netns exec lan curl -s -m 120 -o /dev/null --limit-rate 4M -w '%{http_code} %{size_download}' "http://$TARGET/bytes?n=$size" >"$WORK/download" & download=$!
  sleep 2
  post_update update '{"version": "v0.0.2", "restartsCore": false}' "$WORK/answer-a" >"$WORK/status-a" & a=$!
  post_update update '{"version": "v0.0.2", "restartsCore": false}' "$WORK/answer-b" >"$WORK/status-b" & b=$!
  wait $a $b
  attempt=$(jq -r .running.attempt "$WORK/answer-a")
  expect "two presses at once" "$(cat "$WORK/status-a" "$WORK/status-b")" 202202
  expect "are one update" "$(jq -r .running.attempt "$WORK/answer-b") ${#attempt}" "$attempt 16"
  wait_for "a page that reconnects reads it landed" 90 last_is "landed null"
  expect "as the attempt the press named" "$(update_field .last.attempt)" "$attempt"
  note "it took $((SECONDS - start)) s"
  expect "the version the session says" "$(session_version)" v0.0.2
  kill -0 "$download" 2>/dev/null && pass "the download was still running when the update landed" || fail "the download ended before the update did, so it proves nothing"
  wait "$download"; expect "the download's exit" "$?" 0
  rm -f "$flag"; wait "$watcher"
  expect "the whole download arrived" "$(cat "$WORK/download")" "200 $size"
  expect "samples with pxa1 down during the update" "$(cat "$WORK/downs")" 0
  expect "the core's PID" "$(pid_of $CORE)" "$core_pid"
  expect "the core's invocation" "$(systemctl show -p InvocationID --value $CORE)" "$core_id"
  [ "$(pid_of $DAEMON)" != "$daemon_pid" ] && pass "the daemon restarted" || fail "the daemon kept its PID"
  expect_version v0.0.2
  expect "the releases left" "$(ls "$DATA/releases" | wc -l)" 1
  expect "a press now" "$(post_update update '{"version": "v0.0.2", "restartsCore": false}') $(answered .reason)" "422 nothingNewer"
  untouched
  expect_untouched
}

# v0.0.4 has another core, which the plan says.
update_core() {
  local core; core=$(core_digest)
  publish v0.0.2 v0.0.4
  expect "a check through the page" "$(post_update update/check '{"auto": false}') $(answered '"\(.offered.version) \(.offered.restartsCore)"')" "200 v0.0.4 true"
  expect "a press that keeps the core" "$(post_update update '{"version": "v0.0.4", "restartsCore": false}') $(answered .reason)" "422 offerChanged"
  update_ssh; grep -q "drops every slot's flows" "$UPDATE_OUT" && pass "the plan says its core's restart drops flows" || fail "the plan: $(cat "$UPDATE_OUT")"
  update_ssh --yes; expect "update --yes exits" "$?" 0
  tail -2 "$UPDATE_OUT" | indent
  expect_version v0.0.4
  expect "the core it runs" "$(core_digest)" "$(bundle_core "$WORK/three.tar.gz")"
  [ "$(core_digest)" != "$core" ] && pass "is another than before" || fail "is the one from before"
  wait_for "the signed-in page says v0.0.4" 30 serves_version v0.0.4
  wait_for "that it landed" 20 last_is "landed null"
  wait_for "the slot serves" 120 slot_ready
  wait_for "a LAN client's traffic exits through the slot" 60 through_slot
}

update_uninstall() {
  command_uninstall
  compare_with_pre update
}

# The job's line and its stop-post in a data dir with every character a unit carries.
update_paths() {
  local DATA='/data/a b;$HOME${HOME}&%n 100%`id`*[1]/sidegate' follower out
  local JOURNAL=$DATA/rollback
  write_bootstrap
  out=$("$WORK/repo/v0.0.1/$DAEMON_BIN" install --data-dir "$DATA" --bundle "$WORK/repo/sidegate-v0.0.1-arm64.tar.gz" --yes 2>&1); expect "install of v0.0.1 into $DATA exits" "$?" 0
  wait_for "the signed-in page says v0.0.1" 30 serves_version v0.0.1
  publish v0.0.1 v0.0.3
  ONE_RELEASE=$(release_now) SINCE=$(date +%s)
  update_ssh --yes & follower=$!
  wait_for "the daemon that hangs is what runs" 60 hung || return
  systemctl kill -s KILL $JOB
  wait "$follower"; expect "update --yes, following it, exits" "$?" 1
  wait_for "the job is gone, collected" 60 job_gone
  wait_for "the signed-in page says v0.0.1 again" 60 serves_version v0.0.1
  wait_for "the update says rolledBack" 30 last_is "rolledBack null"
  expect "the release" "$(release_now)" "$ONE_RELEASE"
  [ -z "$(ls -d "$DATA"/update/*/staged 2>/dev/null)" ] && pass "nothing staged is left" || fail "staged: $(ls -d "$DATA"/update/*/staged)"
  publish v0.0.1 v0.0.2
  update_ssh --yes; expect "update --yes to v0.0.2 exits" "$?" 0
  wait_for "the signed-in page says v0.0.2" 30 serves_version v0.0.2
  out=$("$WORK/repo/v0.0.2/$DAEMON_BIN" uninstall --data-dir "$DATA" --purge --yes 2>&1); expect "uninstall there exits" "$?" 0
  rmdir "${DATA%/sidegate}" && pass "the data dir is gone" || fail "left: $(ls -A "$DATA")"
  compare_with_pre update-paths
}

step_update() {
  local how=$2 v
  for v in v0.0.1 v0.0.2; do
    mkdir -p "$WORK/repo/$v"
    [ -x "$WORK/repo/$v/$DAEMON_BIN" ] || tar -C "$WORK/repo/$v" -xzf "$WORK/repo/sidegate-$v-arm64.tar.gz" $DAEMON_BIN
  done
  cp "$WORK/repo/v0.0.1/$DAEMON_BIN" "$WORK/repo/$DAEMON_BIN"
  "update_$how"
  rm -rf -- "$RELEASE_DIR"
}

# v0.0.2 becomes the latest release, and stays, for the page lab's browser to update to.
step_offer() { publish v0.0.1 v0.0.2; expect "the latest release" "$(curl -s -L -m 10 "$RELEASES/latest/download/sidegate.json" | jq -r .version)" v0.0.2; }

# A sibling that takes the hook's location makes nginx refuse the hook and nothing else.
step_refused() {
  local conflict=$HOOKS/shared-runnable-zz-conflict.conf out
  echo "location ^~ $BASE_PATH { return 418; }" >"$conflict"
  nginx -s reload; sleep 1
  out=$("$ONE" install --bundle "$WORK/one.tar.gz" --yes 2>&1)
  [ $? -ne 0 ] && pass "install stops when nginx refuses the hook" || fail "install passed: $out"
  echo "$out" | tail -2 | cut -c1-160 | indent
  [ ! -e "$HOOK" ] && pass "the hook was removed" || fail "the refused hook stayed"
  expect "nginx still validates" "$(nginx -t >/dev/null 2>&1; echo $?)" 0
  expect "nginx kept serving" "$(status_of -H "$LOGIN" "$URL$BASE_PATH")" 418
  [ ! -e /etc/systemd/system/$DAEMON.service ] && pass "no unit was written" || fail "a unit was written before nginx validated"
  expect "nothing was enabled" "$(systemctl is-enabled $DAEMON 2>/dev/null || echo none)" none
  grep -qF "$(cat "$DATA/$NGINX_PROOF")" /var/log/nginx/error.log && fail "the proof is in nginx's log" || pass "the proof isn't in nginx's log"
  rm -f "$conflict"
  nginx -s reload; sleep 1
  out=$("$ONE" uninstall --purge --yes 2>&1); expect "uninstall after a failed install exits" "$?" 0
  write_bootstrap
}

# kill_at runs a command with a tool that waits when its first argument is the one given, and kills
# the command there: "nginx -t" after it wrote the hook and before nginx tested it, and "systemctl
# daemon-reload" in a recovery after it put the files back and before systemd read them.
kill_at() {
  local tool=$1 first=$2 slow=$WORK/slow-$1 at=$WORK/at-$1 pid start=$SECONDS
  shift 2
  mkdir -p "$slow"
  printf '#!/bin/sh\n[ "$1" = %s ] && { echo $$ >%s; exec sleep 300; }\nexec %s "$@"\n' "$first" "$at" "$(command -v "$tool")" >"$slow/$tool"
  chmod 755 "$slow/$tool"
  rm -f "$at"
  PATH="$slow:$PATH" "$@" >/dev/null 2>&1 &
  pid=$!
  until [ -s "$at" ]; do
    if [ $((SECONDS - start)) -gt 60 ]; then fail "the command never reached $tool $first"; kill -9 "$pid"; return 1; fi
    sleep 0.2
  done
  kill -9 "$pid" "$(cat "$at")"
  wait "$pid" 2>/dev/null
  rm -f "$at"
}

hook_gone() { [ ! -e "$HOOK" ]; }
holds_back_refused_hook() {
  journalctl -o cat _SYSTEMD_INVOCATION_ID="$(systemctl show -p InvocationID --value $DAEMON)" | grep -q 'nginx refused this hook'
}
slot_ready_by_status() { "$DATA/current/$DAEMON_BIN" status 2>/dev/null | grep -q '^slot eu "EU" [a-z]* ready '; }

# An install killed between writing a hook nginx refuses and testing it, then the next run of
# install, uninstall or the daemon's start, with no step by a person in between. first-daemon
# kills a first install, which never wrote bootstrap.json's hook dir nor made its release current,
# and starts by hand the daemon it unpacked.
# daemon then fixes nginx and restarts the daemon, which waits out the refusal's hour, so the
# upgrade after it is what brings the page back.
step_interrupted() {
  local how=$2 conflict=$HOOKS/shared-runnable-zz-conflict.conf
  [ "$how" = daemon ] && systemctl stop $DAEMON
  rm -f "$HOOK"
  echo "location ^~ $BASE_PATH { return 418; }" >"$conflict"
  nginx -s reload; sleep 1
  if [ "$how" = daemon ]; then kill_at nginx -t "$DATA/current/$DAEMON_BIN" install --yes; else kill_at nginx -t "$ONE" install --bundle "$WORK/one.tar.gz" --yes; fi
  [ -e "$HOOK" ] && pass "the killed install left the hook" || fail "the killed install left no hook, so this proves nothing"
  expect "nginx -t with the hook it left" "$(nginx -t >/dev/null 2>&1; echo $?)" 1
  case $how in
    install) "$ONE" install --bundle "$WORK/one.tar.gz" --yes >/dev/null 2>&1 ;;
    uninstall) "$ONE" uninstall --purge --yes >/dev/null 2>&1 ;;
    daemon) systemctl start $DAEMON; wait_for "the daemon's start removes the refused hook" 30 hook_gone ;;
    first-daemon)
      [ ! -e "$DATA/current" ] && pass "the killed first install made nothing current" || fail "the killed first install made $(readlink "$DATA/current") current"
      local unpacked; unpacked=$(find "$DATA/releases" -mindepth 2 -maxdepth 2 -name "$DAEMON_BIN" -type f 2>/dev/null)
      expect "the daemons it unpacked" "$(grep -c . <<<"$unpacked")" 1
      "$unpacked" serve --data-dir "$DATA" >/dev/null 2>&1 &
      local daemon=$!
      wait_for "a daemon started after a killed first install removes the refused hook" 30 hook_gone
      kill "$daemon"; wait "$daemon" 2>/dev/null ;;
  esac
  expect "nginx -t after the next $how" "$(nginx -t >/dev/null 2>&1; echo $?)" 0
  expect "UniFi's own app through nginx" "$(status_of -H "$LOGIN" "$URL/proxy/labnet/")" 200
  rm -f "$conflict"
  nginx -s reload; sleep 1
  if [ "$how" = daemon ]; then
    systemctl restart $DAEMON
    wait_for "the restarted daemon holds back the hook nginx refused" 30 holds_back_refused_hook
    hook_gone && pass "and doesn't write it again, so the flash doesn't wear" || fail "the restarted daemon wrote the refused hook again"
    wait_for "the slot serves, as sidegated status reads it" 60 slot_ready_by_status
  else
    "$ONE" uninstall --purge --yes >/dev/null 2>&1
    write_bootstrap
  fi
}

step_install() {
  local out; out=$("$ONE" install --bundle "$WORK/one.tar.gz" --yes 2>&1); expect "install exits" "$?" 0
  echo "$out" | indent
  expect "the hook's mode and owner" "$(stat -c '%a %U' "$HOOK")" "600 root"
  expect "the proof's mode and owner" "$(stat -c '%a %U' "$DATA/$NGINX_PROOF")" "600 root"
  expect "the data dir's mode" "$(stat -c '%a' "$DATA")" 700
  expect "the core binary's digest" "$(sha256sum "$DATA/current/xray" | cut -d' ' -f1)" "$(sed -n 's/^XRAY_SHA256=//p' "$WORK/xray.pin")"
  expect "$DAEMON at boot" "$(systemctl is-enabled $DAEMON)" enabled
  expect "$DAEMON" "$(active $DAEMON)" active
  expect "the core at boot, since the daemon starts it" "$(systemctl is-enabled $CORE)" static
  expect "socket owner and mode" "$(stat -c '%U:%G %a' "$SOCKET")" "root:www-data 660"
}

# A login of its own, so the page is its first auth subrequest and the rest are answered from
# the 10 s cache.
step_page() {
  local login body asset
  login="Cookie: UNIFI_TOKEN=lab-login-$(date +%s%N)"
  body=$(curl -s -m 10 -H "$login" "$URL$BASE_PATH")
  grep -qi '<html' <<<"$body" && pass "the built page comes through the real hook with a login" || fail "page body: ${body:0:120}"
  asset=$(grep -o 'assets/[^"]*\.js' <<<"$body" | head -1)
  expect "its script, on the cached login" "$(status_of -H "$login" "$URL$BASE_PATH${asset:-no-script}")" 200
  expect "the API through the hook, on the cached login" "$(status_of -H "$login" "$URL${BASE_PATH}api/state")" 200
  expect "a deep link" "$(status_of -H "$login" "$URL${BASE_PATH}slots/eu")" 200
  expect "the page with no login, inside that login's cache window" "$(status_of "$URL$BASE_PATH")" 401
}

step_forged() {
  local proof; proof=$(cat "$DATA/$NGINX_PROOF")
  expect "the page with no login" "$(status_of "$URL$BASE_PATH")" 401
  expect "the API with a bad login" "$(status_of -H 'Cookie: UNIFI_TOKEN=wrong' "$URL${BASE_PATH}api/state")" 401
  expect "no login, forged auth and the real proof, at nginx" "$(status_of -H "$AUTH_HEADER: 200" -H "$PROOF_HEADER: $proof" "$URL${BASE_PATH}api/state")" 401
  expect "a login and a forged proof, which nginx overwrites" "$(status_of -H "$LOGIN" -H "$PROOF_HEADER: forged" "$URL${BASE_PATH}api/state")" 200
  expect "nginx's user on the socket with a forged header" "$(sudo -u www-data curl -s -o /dev/null -w '%{http_code}' -H "$AUTH_HEADER: 200" --unix-socket "$SOCKET" http://x${BASE_PATH}api/session)" 401
  expect "nginx's user on the socket with a guessed proof" "$(sudo -u www-data curl -s -o /dev/null -w '%{http_code}' -H "$AUTH_HEADER: 200" -H "$PROOF_HEADER: guess" --unix-socket "$SOCKET" http://x${BASE_PATH}api/session)" 401
  expect "nobody else can connect" "$(sudo -u nobody curl -s -o /dev/null -w '%{http_code}' --unix-socket "$SOCKET" http://x${BASE_PATH}api/session)" 000
  expect "nginx's user reads the proof file" "$(sudo -u www-data cat "$DATA/$NGINX_PROOF" >/dev/null 2>&1; echo $?)" 1
  expect "nginx's user reads the hook" "$(sudo -u www-data cat "$HOOK" >/dev/null 2>&1; echo $?)" 1
  expect "root with the real proof, so a 401 above is the proof's doing" "$(curl -s -o /dev/null -w '%{http_code}' -H "$AUTH_HEADER: local" -H "$PROOF_HEADER: $proof" --unix-socket "$SOCKET" http://x${BASE_PATH}api/session)" 200
}

# The hook with its login include gone, as a changed UniFi could leave it.
step_contract() {
  local probe=$HOOKS/shared-runnable-zz-noauth.conf
  grep -v 'auth\.conf' "$HOOK" | sed "s| ${BASE_PATH%/}\\([ /]\\)| /noauth-daemon\\1|g; s|http://unix:$SOCKET|http://unix:$SOCKET:$BASE_PATH|" >"$probe"
  chmod 600 "$probe"
  nginx -t >/dev/null 2>&1 && nginx -s reload; sleep 1
  expect "with no auth subrequest the daemon closes the page" "$(status_of -H "$AUTH_HEADER: forged" "$URL/noauth-daemon/api/state")" 401
  expect "even to a login it just passed" "$(status_of -H "$LOGIN" "$URL${BASE_PATH}api/state")/$(status_of -H "$LOGIN" "$URL/noauth-daemon/api/state")" 200/401
  rm -f "$probe"
  nginx -s reload; sleep 1
}

mutate() {
  local method=$1 path=$2 key=$3 precondition=$4 body=${5:-}
  local args=(-s -m 20 -o /dev/null -w '%{http_code}' -X "$method" -H "$LOGIN" -H "X-Csrf-Token: $(unifi_token "$LOGIN" "$URL${BASE_PATH}api/state")"
    -H "$CSRF_HEADER: $(curl -s -H "$LOGIN" "$URL${BASE_PATH}api/session" | jq -r .csrf)" -H "Idempotency-Key: $key")
  [ -n "$precondition" ] && args+=(-H "$precondition")
  [ -n "$body" ] && args+=(-H 'Content-Type: application/json' -d "$body")
  curl "${args[@]}" "$URL${BASE_PATH}api/$path"
}

servers_present() { [ "$(state_field '.sources[]? | select(.id == "good") | .stored')" = 2 ]; }

step_configure() {
  expect "a source through the hook" "$(mutate PUT sources/good lab-source-1 'If-None-Match: *' "$(jq -n --arg url "$FEED" '{name: "good", origin: {kind: "remote", url: $url, userAgent: "Happ/3.0.0", refreshEverySeconds: 3600, paused: false}}')")" 200
  wait_for "its servers arrive" 40 servers_present
  expect "a group through the hook" "$(mutate PUT groups/lab-eu lab-group-1 'If-None-Match: *' '{"name":"EU","members":{"sources":{"not":false,"values":[]},"countries":{"not":false,"values":["DE"]},"protocols":{"not":false,"values":[]},"include":"","exclude":"","box":null},"ticked":[]}')" 200
  expect "a slot through the hook" "$(mutate PUT slots/eu lab-slot-1 'If-None-Match: *' '{"name":"EU","exit":{"kind":"group","group":"lab-eu"}}')" 200
  wait_for "the slot opens" 90 slot_ready
  local forged; forged=$(sudo -u www-data curl -s -o /dev/null -w '%{http_code}' -X DELETE -H "$AUTH_HEADER: 200" -H 'Idempotency-Key: forged' -H "$CSRF_HEADER: $(curl -s -H "$LOGIN" "$URL${BASE_PATH}api/session" | jq -r .csrf)" --unix-socket "$SOCKET" http://x${BASE_PATH}api/sources/good)
  expect "the reviewer's forged delete" "$forged" 401
  expect "the source is still there" "$(state | jq -r '[.sources[] | select(.id == "good")] | length')" 1
}

# Plays UniFi's part, as a real VPN Client would, with the keys the daemon stored.
step_connect() {
  local keys private public port old
  keys=$(python3 - "$DATA/$STATE_DB" eu <<'PY'
import json, sqlite3, sys
db = sqlite3.connect(f"file:{sys.argv[1]}?mode=ro", uri=True)
a = json.loads(db.execute("SELECT allocation FROM slots WHERE id=?", (sys.argv[2],)).fetchone()[0])
k = a["keys"]
secret = db.execute("SELECT value FROM secrets WHERE ref=?", (k["ClientPrivate"],)).fetchone()[0]
print(secret, k["HostPublic"], a["peerPort"])
PY
  ) || { fail "couldn't read the slot's keys"; return; }
  read -r private public port <<<"$keys"
  for old in $(wg show wgclt1 peers); do wg set wgclt1 peer "$old" remove; done
  wg set wgclt1 private-key <(echo "$private") peer "$public" endpoint "127.0.0.1:$port" allowed-ips 0.0.0.0/0 persistent-keepalive 25
  unset private keys
  wait_for "a LAN client's traffic exits through the slot" 40 through_slot
  ip netns exec lan curl -s -m 20 -o /dev/null "http://$TARGET/bytes?n=5000000" && pass "5 MB crossed the slot" || fail "the download through the slot failed"
}

step_status() {
  local out; out=$("$DATA/current/$DAEMON_BIN" status 2>&1); expect "status exits" "$?" 0
  echo "$out" | cut -c1-160 | indent
  grep -q '^release ' <<<"$out" && grep -q 'nginx hook: present' <<<"$out" && grep -q '^wan ' <<<"$out" && pass "status prints the install and the daemon's view" || fail "status: $out"
}

daemon_active() { [ "$(active $DAEMON)" = active ]; }

step_booted() {
  wait_for "the daemon came back with no command" 60 daemon_active
  wait_for "the page through the hook" 60 page_up
  wait_for "the slot opens" 120 slot_ready
  expect "the core at boot, started by the daemon" "$(active $CORE)" active
  local log=/run/sglc-watch.log first_up first_core early
  note "the watcher took $(wc -l <"$log") samples from $(head -1 "$log" | cut -d' ' -f1) s of uptime"
  note "first sample: $(head -1 "$log")"
  first_core=$(grep -m1 'core=active' "$log" | cut -d' ' -f1)
  first_up=$(grep -m1 'up=\[ pxa' "$log" | cut -d' ' -f1)
  [ -n "$first_up" ] && pass "pxa came up, $first_up s after boot, with the core active from $first_core s" || fail "the watcher never saw a gate open"
  early=$(grep 'up=\[ pxa' "$log" | grep -vc 'core=active')
  expect "samples with an open gate and no active core" "$early" 0
  grep -m1 -q 'up=\[ \]' "$log" && pass "the gates were closed when the watcher started" || fail "no sample shows closed gates"
}

step_break() {
  local bin; bin=$(readlink -f "$DATA/current/$DAEMON_BIN")
  mv "$bin" "$bin.real"
  printf '#!/bin/sh\nexit 3\n' >"$bin"; chmod 755 "$bin"
  pass "the daemon binary now exits 3"
}

step_broken() {
  expect "multi-user.target reached" "$(active multi-user.target)" active
  expect "network.target" "$(active network.target)" active
  local daemon_state; daemon_state=$(active $DAEMON)
  [ "$daemon_state" != active ] && pass "the daemon isn't running: $daemon_state, and systemd retries it" || fail "the broken daemon is active"
  expect "the last start's result" "$(systemctl show -p Result --value $DAEMON)" exit-code
  expect "the LAN still reaches the internet directly" "$(ip netns exec lan curl -s -m 5 -o /dev/null -w '%{http_code}' http://203.0.113.80/ip)" 200
  expect "UniFi's own app through nginx" "$(status_of -H "$LOGIN" "$URL/proxy/labnet/")" 200
  expect "our page, with no daemon behind it" "$(page_status)" 502
  expect "pxa links" "$(ls /sys/class/net | grep -c '^pxa')" 0
}

step_repair() {
  local bin; bin=$(readlink -f "$DATA/current/$DAEMON_BIN")
  mv "$bin.real" "$bin"
  systemctl restart $DAEMON
  wait_for "the slot opens once the binary is good" 120 slot_ready
}

step_heal() {
  rm -f "$HOOK"
  nginx -s reload; sleep 1
  expect "the page with its hook gone" "$(page_status)" 404
  wait_for "the daemon puts the hook back and reloads nginx" 75 page_up
  expect "the hook's mode" "$(stat -c '%a %U' "$HOOK")" "600 root"
}

gate_watch() {
  local downs=0 until=$1
  while [ -e "$until" ]; do
    [ "$(link_up pxa1)" = up ] || downs=$((downs + 1))
    sleep 0.2
  done
  echo "$downs"
}

step_upgrade() {
  local core_pid core_id daemon_pid out flag=$WORK/watching size=80000000
  core_pid=$(pid_of $CORE); core_id=$(systemctl show -p InvocationID --value $CORE); daemon_pid=$(pid_of $DAEMON)
  : >"$flag"
  gate_watch "$flag" >"$WORK/downs" &
  local watcher=$!
  ip netns exec lan curl -s -m 120 -o /dev/null --limit-rate 4M -w '%{http_code} %{size_download}' "http://$TARGET/bytes?n=$size" >"$WORK/download" &
  local download=$!
  sleep 3
  out=$("$TWO" upgrade --bundle "$WORK/two.tar.gz" 2>&1)
  grep -q 'no flow drops' <<<"$out" && pass "the plan says no flow drops" || fail "plan: $out"
  out=$("$TWO" upgrade --bundle "$WORK/two.tar.gz" --yes 2>&1); expect "upgrade exits" "$?" 0
  echo "$out" | indent
  kill -0 "$download" 2>/dev/null && pass "the download was still running when the upgrade finished" || fail "the download ended before the upgrade did, so it proves nothing"
  wait "$download"; expect "the download's exit" "$?" 0
  rm -f "$flag"; wait "$watcher"
  expect "the whole download arrived" "$(cat "$WORK/download")" "200 $size"
  expect "samples with pxa1 down during the upgrade" "$(cat "$WORK/downs")" 0
  expect "the core's PID" "$(pid_of $CORE)" "$core_pid"
  expect "the core's invocation" "$(systemctl show -p InvocationID --value $CORE)" "$core_id"
  [ "$(pid_of $DAEMON)" != "$daemon_pid" ] && pass "the daemon restarted" || fail "the daemon kept its PID"
  expect "the running daemon is the new release" "$("$DATA/current/$DAEMON_BIN" version)" two
  expect "the releases left" "$(ls "$DATA/releases" | wc -l)" 1
  wait_for "the page after the upgrade, which tries the hook nginx refused again" 30 page_up
  wait_for "the slot serves after the upgrade" 30 slot_ready
  expect "the page says the running core is the pinned one" "$(state | jq -r '.core.running != null and .core.running == .core.version')" true
  through_slot && pass "new traffic still exits through the slot" || fail "new traffic failed after the upgrade"
}

# An upgrade to a bundle whose daemon exits at once, so it never answers.
step_rollback() {
  local core_pid out rc bad=$WORK/bad
  core_pid=$(pid_of $CORE)
  rm -rf "$bad"; mkdir -p "$bad"
  tar -C "$bad" -xzf "$WORK/two.tar.gz"
  printf '#!/bin/sh\nexit 3\n' >"$bad/$DAEMON_BIN"; chmod 755 "$bad/$DAEMON_BIN"
  tar -C "$bad" -czf "$WORK/bad.tar.gz" "$DAEMON_BIN" xray
  out=$("$TWO" upgrade --bundle "$WORK/bad.tar.gz" --yes 2>&1); rc=$?
  echo "$out" | tail -7 | cut -c1-200 | indent
  [ "$rc" -ne 0 ] && pass "an upgrade whose daemon doesn't answer exits $rc" || fail "the upgrade passed"
  grep -q 'is back and answers' <<<"$out" && pass "it says the old release is back" || fail "it doesn't say what it put back"
  expect "the selected daemon" "$("$DATA/current/$DAEMON_BIN" version)" two
  expect "$DAEMON" "$(active $DAEMON)" active
  wait_for "the page after the rollback" 30 page_up
  expect "the core's PID" "$(pid_of $CORE)" "$core_pid"
  expect "the releases left" "$(ls "$DATA/releases" | wc -l)" 1
  [ ! -e "$DATA/rollback" ] && pass "no rollback record is left" || fail "the rollback record stayed"
  wait_for "the slot serves after the rollback" 30 slot_ready
  rm -rf "$bad" "$WORK/bad.tar.gz"
}

# An unfinished switch goes back by itself: each case below cuts an upgrade from two short, or
# lets it fail, and ends with release two answering the signed-in page, its slot carrying traffic,
# its source's servers, the journal gone and the outcome the switch's daemon wrote.
release_now() { basename "$(readlink "$DATA/current")"; }
current_is() { [ "$(release_now)" = "$1" ]; }
running_version() { "$DATA/current/$DAEMON_BIN" version 2>/dev/null | head -1; }
session_release() { curl -s -m 10 -H "$LOGIN" "$URL${BASE_PATH}api/session" | jq -r .release 2>/dev/null; }
answers_as() { [ "$(session_release)" = "$1" ]; }
outcome_is() { [ "$(jq -r .outcome "$OUTCOME" 2>/dev/null)" = "$1" ]; }
journal_gone() { [ ! -e "$JOURNAL" ]; }
core_digest() { basename "$(dirname "$(readlink "/proc/$(pid_of $CORE)/exe" 2>/dev/null)")"; }
core_on() { [ "$(core_digest)" = "$1" ]; }
said() { grep -q -- "^- $1" "$WORK/upgrade.out"; }
bundle_core() { tar -xzOf "$1" xray | sha256sum | cut -d' ' -f1; }

# A copy of two's bundle whose daemon is the script given.
bad_bundle() {
  local name=$1 dir=$WORK/$1
  rm -rf "$dir"; mkdir -p "$dir"
  tar -C "$dir" -xzf "$WORK/two.tar.gz"
  printf '#!/bin/sh\n%s\n' "$2" >"$dir/$DAEMON_BIN"; chmod 755 "$dir/$DAEMON_BIN"
  tar -C "$dir" -czf "$WORK/$name.tar.gz" "$DAEMON_BIN" xray
  rm -rf "$dir"
}

# Runs an upgrade with --outcome and kills it with SIGKILL once the test after the third word holds,
# as a power cut or the OOM killer would, so nothing of its own rollback runs.
kill_upgrade_when() {
  local bin=$1 bundle=$2 what=$3 pid start=$SECONDS; shift 3
  rm -f "$OUTCOME"
  "$bin" upgrade --data-dir "$DATA" --bundle "$bundle" --yes --outcome "$OUTCOME" >"$WORK/upgrade.out" 2>&1 &
  pid=$!
  until "$@"; do
    if ! kill -0 "$pid" 2>/dev/null || [ $((SECONDS - start)) -gt 120 ]; then
      fail "the upgrade ended before $what"; tail -5 "$WORK/upgrade.out" | indent; return 1
    fi
    sleep 0.1
  done
  kill -9 "$pid"; wait "$pid" 2>/dev/null
  pass "the upgrade was killed once $what"
  [ -d "$JOURNAL" ] && ! current_is "$TWO_RELEASE" && pass "it left its journal, with $(release_now) current" ||
    fail "it left current $(release_now) and journal $(ls -d "$JOURNAL" 2>/dev/null), so this proves nothing"
}

# Kills an upgrade while it flushes its record to disk, its last act before the switch. The unwritten file
# makes that flush take long enough to be seen.
kill_planned() {
  local pid
  dd if=/dev/zero of="$WORK/unwritten" bs=1M count=300 status=none
  "$1" upgrade --bundle "$2" --yes --outcome "$OUTCOME" >"$WORK/upgrade.out" 2>&1 &
  pid=$!
  until [ -e "$JOURNAL/record.json" ] || ! kill -0 "$pid" 2>/dev/null; do :; done
  kill -9 "$pid"; wait "$pid" 2>/dev/null
  rm -f "$WORK/unwritten"
  expect "the upgrade was killed before its switch, so its record" "$(jq -r '"\(.phase) \(.coreRequired)"' "$JOURNAL/record.json" 2>/dev/null)" "planned true"
}

# $1 is the outcome to see, or - for none to check; $2 is traffic, or none after a reboot, whose
# connect step brings it.
back_on_two() {
  local said_outcome=$1 traffic=${2:-traffic}
  wait_for "the signed-in page answers" 90 page_up
  wait_for "release two answers it" 30 answers_as "$TWO_RELEASE"
  expect "the selected daemon's version" "$(running_version)" two
  if [ "$said_outcome" != - ]; then
    wait_for "the outcome says $said_outcome" 30 outcome_is "$said_outcome"
    jq -r .message "$OUTCOME" 2>/dev/null | cut -c1-200 | indent
  fi
  wait_for "the journal is gone" 30 journal_gone
  wait_for "the slot serves" 120 slot_ready
  servers_present && pass "the source's servers are there" || fail "the source has $(state_field '.sources[]? | select(.id == "good") | .stored') servers"
  [ "$traffic" = none ] || wait_for "a LAN client's traffic exits through the slot" 40 through_slot
}

step_cut() {
  local how=$2 core core_pid three_core bin
  TWO_RELEASE=$(release_now)
  echo "$TWO_RELEASE" >"$WORK/two-release"
  core=$(core_digest); core_pid=$(pid_of $CORE)
  case $how in
    kill)
      bad_bundle bad-exit 'exit 3'
      kill_upgrade_when "$TWO" "$WORK/bad-exit.tar.gz" "it waits for the daemon that exits" said "wait for release" || return
      # Units another release wrote can differ from two's, and systemd holds what it wrote.
      sed -i 's/^Description=.*/& (a candidate)/' /etc/systemd/system/$DAEMON.service /etc/systemd/system/$CORE.service
      systemctl daemon-reload
      wait_for "systemd's own restart of that daemon puts two back" 60 current_is "$TWO_RELEASE"
      back_on_two rolledBack
      expect "systemd holds two's daemon unit" "$(systemctl show -p Description --value $DAEMON)" "sidegate gateway daemon"
      expect "and two's core unit" "$(systemctl show -p Description --value $CORE)" "sidegate proxy core (Xray)"
      expect "with nothing left to reload" "$(systemctl show -p NeedDaemonReload --value $DAEMON)/$(systemctl show -p NeedDaemonReload --value $CORE)" no/no
      expect "the core's PID, since its binary never changed" "$(pid_of $CORE)" "$core_pid"
      ;;
    missing)
      bad_bundle bad-hang 'exec sleep 1000'
      kill_upgrade_when "$TWO" "$WORK/bad-hang.tar.gz" "it waits for the daemon that hangs" said "wait for release" || return
      rm -f "$DATA/current/$DAEMON_BIN"
      pass "the candidate's daemon is gone from the release current names"
      systemctl restart $DAEMON
      back_on_two rolledBack
      ;;
    gone)
      bad_bundle bad-hang 'exec sleep 1000'
      kill_upgrade_when "$TWO" "$WORK/bad-hang.tar.gz" "it waits for the daemon that hangs" said "wait for release" || return
      rm -rf -- "$DATA/releases/$(release_now)"
      [ ! -e "$DATA/current/" ] && pass "the candidate's directory is gone, and current names it" || fail "current names $(release_now), which is there"
      systemctl restart $DAEMON
      back_on_two rolledBack
      expect "the core's PID, since its binary never changed" "$(pid_of $CORE)" "$core_pid"
      ;;
    prior)
      bad_bundle bad-hang 'exec sleep 1000'
      kill_upgrade_when "$TWO" "$WORK/bad-hang.tar.gz" "it waits for the daemon that hangs" said "wait for release" || return
      bin=$DATA/releases/$TWO_RELEASE/$DAEMON_BIN
      mv "$bin" "$WORK/two-daemon.real"
      printf '#!/bin/sh\nexit 3\n' >"$bin"; chmod 755 "$bin"
      pass "release two's own daemon now exits 3"
      systemctl restart $DAEMON
      wait_for "recover puts two back without it" 30 current_is "$TWO_RELEASE"
      wait_for "two starts again without having answered, and recover says so" 40 outcome_is putBackFailed
      jq -r .message "$OUTCOME" | cut -c1-200 | indent
      local said_at; said_at=$(stat -c %Y "$JOURNAL/record.json" "$OUTCOME" | xargs)
      sleep 25
      expect "two more starts write nothing" "$(stat -c %Y "$JOURNAL/record.json" "$OUTCOME" | xargs)" "$said_at"
      expect "systemd keeps retrying the daemon, which costs nothing else" "$(systemctl show -p Result --value $DAEMON)" exit-code
      expect "UniFi's own app through nginx" "$(status_of -H "$LOGIN" "$URL/proxy/labnet/")" 200
      local out; out=$("$TWO" upgrade --bundle "$WORK/two.tar.gz" --yes 2>&1)
      expect "the owner's one command: the release's own bundle, through its own sidegated" "$?" 0
      echo "$out" | tail -3 | cut -c1-200 | indent
      rm -f "$WORK/two-daemon.real"
      back_on_two -
      ;;
    core)
      three_core=$(bundle_core "$WORK/three.tar.gz")
      kill_upgrade_when "$THREE" "$WORK/three.tar.gz" "the core runs three's core" core_on "$three_core" || return
      note "the core ran $core, and runs $(core_digest) as PID $(pid_of $CORE)"
      systemctl restart $DAEMON
      back_on_two rolledBack
      expect "the core runs two's core again" "$(core_digest)" "$core"
      ;;
    recovery)
      three_core=$(bundle_core "$WORK/three.tar.gz")
      kill_upgrade_when "$THREE" "$WORK/three.tar.gz" "the core runs three's core" core_on "$three_core" || return
      sed -i 's/^Description=.*/& (a candidate)/' /etc/systemd/system/$DAEMON.service /etc/systemd/system/$CORE.service
      systemctl daemon-reload
      systemctl stop $DAEMON
      kill_at systemctl daemon-reload "$JOURNAL/$DAEMON_BIN" recover --yes || return
      expect "the recovery killed half way put back current" "$(release_now)" "$TWO_RELEASE"
      expect "and neither the units systemd holds" "$(systemctl show -p Description --value $DAEMON)" "sidegate gateway daemon (a candidate)"
      expect "nor the core" "$(core_digest)" "$three_core"
      [ -d "$JOURNAL" ] && ! outcome_is rolledBack && pass "it left its journal, and said no rolledBack" || fail "journal $(ls -d "$JOURNAL" 2>/dev/null), outcome $(cat "$OUTCOME" 2>/dev/null)"
      systemctl start $DAEMON
      back_on_two rolledBack
      expect "the core runs two's core again" "$(core_digest)" "$core"
      expect "systemd holds two's daemon unit" "$(systemctl show -p Description --value $DAEMON)" "sidegate gateway daemon"
      expect "and two's core unit" "$(systemctl show -p Description --value $CORE)" "sidegate proxy core (Xray)"
      ;;
    reboot)
      bad_bundle bad-hang 'exec sleep 1000'
      kill_upgrade_when "$TWO" "$WORK/bad-hang.tar.gz" "it waits for the daemon that hangs" said "wait for release" || return
      expect "the daemon that hangs is what runs" "$(active $DAEMON)" active
      ;;
    *) fail "no such cut: $how" ;;
  esac
}

# After the reboot step cut reboot leads to.
step_back() {
  TWO_RELEASE=$(cat "$WORK/two-release")
  back_on_two rolledBack none
}

# The new core exits at once, so the upgrade's own check rolls it back, core and all.
step_deadcore() {
  local core out rc
  TWO_RELEASE=$(release_now); core=$(core_digest)
  rm -f "$OUTCOME"
  out=$("$FOUR" upgrade --bundle "$WORK/four.tar.gz" --yes --outcome "$OUTCOME" 2>&1); rc=$?
  echo "$out" | tail -8 | cut -c1-200 | indent
  [ "$rc" -ne 0 ] && pass "an upgrade whose core exits at once exits $rc" || fail "the upgrade passed"
  back_on_two rolledBack
  expect "the core runs two's core" "$(core_digest)" "$core"
}

core_down() { [ "$(pid_of $CORE)" = 0 ]; }
slot_field() { state_field ".slots[]? | select(.id == \"eu\") | .$1"; }
put_slot() { expect "the slot's Exit becomes $1" "$(mutate PUT slots/eu "lab-slot-$(date +%s%N)" "If-Match: \"$(slot_field version)\"" "$(jq -n --argjson exit "$2" '{name: "EU", exit: $exit}')")" 200; }
refused_present() { [ "$(state_field '.sources[]? | select(.id == "refused") | .stored')" = 1 ]; }
# An upgrade, its outcome file and its exit 0.
land() {
  local out
  rm -f "$OUTCOME"
  out=$("$1" upgrade --bundle "$2" --yes --outcome "$OUTCOME" 2>&1); expect "$3 exits" "$?" 0
  echo "$out" | tail -5 | cut -c1-200 | indent
  expect "its outcome" "$(jq -r .outcome "$OUTCOME")" landed
  journal_gone && pass "it leaves no journal" || fail "the journal stayed"
}

# The slot's only server is one both cores refuse, so the core is down before the upgrade, and an upgrade
# to a new core lands with it down, as does the downgrade. An upgrade planned while the core ran, and killed
# before its switch, changes nothing of that.
step_downcore() {
  local two
  two=$(release_now)
  core_down && fail "no core runs before the slot's Exit changes, so this proves nothing"
  kill_planned "$THREE" "$WORK/three.tar.gz"
  expect "the release still current" "$(release_now)" "$two"
  echo "ss://2022-blake3-aes-256-gcm:lab-bad-shadowsocks-key@198.51.100.10:8388#DE refused" >"$CONF/feeds/refused"
  expect "a source whose one server the core refuses" "$(mutate PUT sources/refused "lab-refused-$(date +%s%N)" 'If-None-Match: *' "$(jq -n --arg url "${FEEDS}refused?token=lab-s3cret" '{name: "refused", origin: {kind: "remote", url: $url, userAgent: "Happ/3.0.0", refreshEverySeconds: 3600, paused: false}}')")" 200
  wait_for "its server arrives" 40 refused_present
  put_slot "that source" '{"kind":"source","source":"refused"}'
  wait_for "the daemon stops the core, which has nothing to run" 120 core_down
  sleep 10
  expect "the core's PID before the upgrade" "$(pid_of $CORE)" 0
  land "$THREE" "$WORK/three.tar.gz" "the upgrade to a new core"
  expect "the selected daemon's version" "$(running_version)" three
  wait_for "release three answers the signed-in page" 30 answers_as "$(release_now)"
  sleep 20
  expect "the slot's server is still refused, so the core's PID" "$(pid_of $CORE)" 0
  systemctl restart $DAEMON
  wait_for "the next start keeps three" 60 answers_as "$(release_now)"
  expect "the version it keeps" "$(running_version)" three
  land "$TWO" "$WORK/two.tar.gz" "the downgrade to two"
  expect "the release" "$(release_now)" "$two"
  wait_for "release two answers" 30 answers_as "$two"
  put_slot "the group again" '{"kind":"group","group":"lab-eu"}'
  TWO_RELEASE=$two
  back_on_two -
  expect "the source goes" "$(mutate DELETE sources/refused "lab-refused-$(date +%s%N)" '')" 200
  rm -f "$CONF/feeds/refused"
}

# A deliberate upgrade over SSH after a switch cut short lands and stays, and so does a deliberate
# downgrade after it.
step_deliberate() {
  local out
  TWO_RELEASE=$(release_now)
  bad_bundle bad-hang 'exec sleep 1000'
  kill_upgrade_when "$TWO" "$WORK/bad-hang.tar.gz" "it waits for the daemon that hangs" said "wait for release" || return
  out=$("$THREE" upgrade --bundle "$WORK/three.tar.gz" --yes 2>&1); expect "the owner's upgrade to three over it" "$?" 0
  echo "$out" | tail -3 | cut -c1-200 | indent
  journal_gone && pass "it leaves no journal" || fail "the journal stayed"
  local three; three=$(release_now)
  systemctl restart $DAEMON
  wait_for "the next start keeps three" 60 answers_as "$three"
  expect "three's version" "$(running_version)" three
  out=$("$TWO" upgrade --bundle "$WORK/two.tar.gz" --yes 2>&1); expect "the owner's downgrade to two, by its own bundle" "$?" 0
  systemctl restart $DAEMON
  back_on_two -
}

# A journal a kill cut short while it was written is a staging directory nothing reads, and two
# upgrades at once give one busy.
step_busy() {
  local a b ra rb busy
  TWO_RELEASE=$(release_now)
  mkdir -p "$JOURNAL.new"
  head -c 4096 "$TWO" >"$JOURNAL.new/$DAEMON_BIN"
  printf '{"phase": "swi' >"$JOURNAL.new/record.json"
  systemctl restart $DAEMON
  wait_for "the daemon answers over the half-written journal" 60 answers_as "$TWO_RELEASE"
  [ -d "$JOURNAL.new" ] && journal_gone && pass "recover read nothing of it" || fail "journal: $(ls -d "$JOURNAL"* 2>/dev/null | xargs)"
  "$TWO" upgrade --bundle "$WORK/two.tar.gz" --yes >"$WORK/a.out" 2>&1 & a=$!
  "$TWO" upgrade --bundle "$WORK/two.tar.gz" --yes >"$WORK/b.out" 2>&1 & b=$!
  wait "$a"; ra=$?; wait "$b"; rb=$?
  busy=$(grep -l 'another install, upgrade or uninstall holds' "$WORK/a.out" "$WORK/b.out" | wc -l)
  expect "the exits of two upgrades at once" "$(echo "$ra $rb" | tr ' ' '\n' | sort | xargs)" "0 1"
  expect "upgrades that said busy" "$busy" 1
  [ ! -e "$JOURNAL.new" ] && pass "the upgrade that ran cleared the half-written journal" || fail "the half-written journal stayed"
  back_on_two -
}

step_key() {
  local canary=lab-api-key-canary-7f3
  local out; out=$(printf '%s\n' "$canary" | "$TWO" install --api-key-stdin --yes 2>&1); expect "install with a key on stdin exits" "$?" 0
  expect "the key file's mode and owner" "$(stat -c '%a %U' "$DATA/$NETWORK_KEY")" "600 root"
  [ "$(cat "$DATA/$NETWORK_KEY")" = "$canary" ] && pass "the key file holds what stdin gave" || fail "the key file holds something else"
  expect "the key in install's output" "$(grep -c "$canary" <<<"$out")" 0
  expect "the key in any log" "$(journalctl -b -o cat | grep -c "$canary")" 0
  expect "the key in bootstrap.json" "$(grep -c "$canary" "$DATA/$BOOTSTRAP")" 0
  wait_for "the page after the key" 30 page_up
}

step_keep() {
  local out; out=$("$TWO" uninstall --yes 2>&1); expect "uninstall exits" "$?" 0
  echo "$out" | cut -c1-200 | indent
  grep -q "aren't known" <<<"$out" && pass "with no app answering, it says routes aren't known" || fail "no word on routes"
  compare_with_pre keep "rules links routes iptables ip6tables sysctl nginx nginx-live hooks units units-loaded run"
  expect "what stays in the data dir" "$(ls "$DATA" | grep -vE "^$STATE_DB-(wal|shm)\$" | xargs)" "$NETWORK_KEY $STATE_DB"
  expect "the key stays root's" "$(stat -c '%a' "$DATA/$NETWORK_KEY")" 600
  expect "$DAEMON" "$(systemctl is-enabled $DAEMON 2>/dev/null || echo none)" none
  expect "the core" "$(active $CORE || true)" inactive
}

step_reinstall() {
  rm -f "$DATA/$NETWORK_KEY"
  write_bootstrap
  local out; out=$("$TWO" install --bundle "$WORK/two.tar.gz" --yes 2>&1); expect "install over kept state exits" "$?" 0
  wait_for "the kept slot opens with its keys" 120 slot_ready
  wait_for "a LAN client's traffic exits through it, with no reconnect by hand" 40 through_slot
}

step_purge() {
  local out; out=$("$TWO" uninstall --purge --yes 2>&1); expect "uninstall --purge exits" "$?" 0
  echo "$out" | cut -c1-200 | indent
  compare_with_pre purge
  out=$("$TWO" uninstall --purge --yes 2>&1); expect "uninstall again exits" "$?" 0
  compare_with_pre again
  [ ! -e "$DATA" ] && pass "the data dir is gone" || fail "the data dir stayed"
}

argv_of() { tr '\0' '\n' <"/proc/$1/cmdline" 2>/dev/null | paste -sd'|'; }
tree_of() { find /data /etc/systemd/system | LC_ALL=C sort | sha256sum; }

# The units carry every character a data dir and a run dir may have: each starts the daemon, its recovery
# and the core, and puts back an upgrade cut short. One they can't carry is refused, with nothing written.
step_paths() {
  local dir parent run out pid before name
  for parent in '/data/backup(1)' '/data/a b;$HOME${HOME}&%n 100%`id`*[1]'; do
    dir=$parent/sidegate run="/run/sidegate (${parent#/data/})"
    local DATA=$dir JOURNAL=$dir/rollback
    write_bootstrap
    jq --arg run "$run" '.runDir = $run' "$dir/$BOOTSTRAP" >"$WORK/bootstrap" && cp "$WORK/bootstrap" "$dir/$BOOTSTRAP"
    out=$("$ONE" install --data-dir "$dir" --bundle "$WORK/one.tar.gz" --yes 2>&1); expect "install into $dir exits" "$?" 0
    [ -d "$dir/current" ] || { echo "$out" | tail -3 | cut -c1-200 | indent; return; }
    out=$("$TWO" upgrade --data-dir "$dir" --bundle "$WORK/two.tar.gz" --yes 2>&1); expect "upgrade there exits" "$?" 0
    TWO_RELEASE=$(release_now)
    wait_for "release two answers the signed-in page" 30 answers_as "$TWO_RELEASE"
    expect "the daemon's command" "$(argv_of "$(pid_of $DAEMON)")" "$dir/current/$DAEMON_BIN|serve|--data-dir|$dir"
    bad_bundle bad-exit 'exit 3'
    kill_upgrade_when "$TWO" "$WORK/bad-exit.tar.gz" "it waits for the daemon that exits" said "wait for release" || return
    wait_for "systemd's own restart of that daemon puts two back" 60 current_is "$TWO_RELEASE"
    wait_for "release two answers" 60 answers_as "$TWO_RELEASE"
    wait_for "the outcome says rolledBack" 30 outcome_is rolledBack
    wait_for "the journal is gone" 30 journal_gone
    systemctl stop $DAEMON
    mkdir "$JOURNAL"
    printf '#!/bin/sh\nprintf "%%s\\n" "$@" >%s\n' "$WORK/recover-args" >"$JOURNAL/$DAEMON_BIN"; chmod 755 "$JOURNAL/$DAEMON_BIN"
    systemctl start $DAEMON
    wait_for "two starts over a journal" 30 answers_as "$TWO_RELEASE"
    expect "the recovery's arguments" "$(paste -sd'|' "$WORK/recover-args")" "recover|--data-dir|$dir|--yes"
    [ -e "$JOURNAL/started" ] && pass "the start marked the journal" || fail "no mark in the journal: $(ls "$JOURNAL" | xargs)"
    rm -rf -- "$JOURNAL" "$WORK/recover-args"
    systemctl stop $DAEMON
    mkdir -p "$run"
    echo '{"outbounds": [{"protocol": "blackhole"}]}' >"$run/xray.json"
    systemctl start $CORE; sleep 2
    pid=$(pid_of $CORE)
    expect "the core's command" "$(argv_of "$pid")" "$dir/current/xray|run|-c|$run/xray.json"
    [ -s "$run/core.log" ] && pass "its log is in the run dir" || fail "no core log in $run: $(ls "$run" | xargs)"
    systemctl stop $CORE
    expect "its stop and stop hook" "$(systemctl show -p Result --value $CORE) $(systemctl show -p ExecStopPost --value $CORE | grep -o 'status=[0-9]*')" "success status=0"
    out=$("$TWO" uninstall --data-dir "$dir" --purge --yes 2>&1); expect "uninstall there exits" "$?" 0
    [ ! -e "$dir" ] && [ ! -e "$run" ] && pass "the data and run dirs are gone" || fail "left: $(ls -d "$dir" "$run" 2>/dev/null)"
    rmdir "$parent"
  done
  before=$(tree_of)
  for name in "it's" 'a"b' 'a\b' $'a\nb' $'a\tb' $'a\x7fb' $'a\xffb'; do
    out=$("$ONE" install --data-dir "/data/$name/sidegate" --bundle "$WORK/one.tar.gz" --yes 2>&1)
    [ $? -ne 0 ] && grep -q "can't be in a systemd unit" <<<"$out" && pass "$(printf %q "$name") is refused: $(tail -1 <<<"$out" | cut -c1-160)" || fail "$(printf %q "$name"): $out"
  done
  expect "what the refusals wrote" "$(tree_of)" "$before"
  compare_with_pre paths
}

step_teardown() {
  systemctl disable --now nginx sglc-login sglc-net sglc-watch >/dev/null 2>&1
  rm -f /etc/systemd/system/sglc-login.service /etc/systemd/system/sglc-net.service /etc/systemd/system/sglc-watch.service
  [ -f "$WORK/nginx.conf.orig" ] && cp "$WORK/nginx.conf.orig" /etc/nginx/nginx.conf
  systemctl daemon-reload
  remove_stand_ins
  [ ! -e /data ] && pass "the lab's /data is gone" || fail "/data has something the lab didn't make: $(ls -A /data)"
  rm -rf -- "$WORK" "$RELEASE_DIR"
  rm -f /run/sglc-watch.log "$LOGIN_SOCKET"
}

declare -F "step_$STEP" >/dev/null || { echo "unknown step: $STEP" >&2; exit 2; }
"step_$STEP" "$@"
[ "$fails" -eq 0 ]
