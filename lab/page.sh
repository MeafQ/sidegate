#!/usr/bin/env bash
# Runs as root inside the VM, after control.sh setup. The real daemon behind the real nginx
# hook, with stand-ins for UniFi's login and its Network Application. Usage:
# page.sh <nginx|unifi|daemon|auth|cache|csrf|paste|forged SLOT|connect SLOT|link SLOT|client SLOT|routes|move-acme|status|kill SERVER|start-server SERVER|delay SERVER DELAY|undelay SERVER|secrets|teardown>
set -uo pipefail
source "$(dirname "$0")/names.sh"
source "$(dirname "$0")/unifi-login.sh"

BIN=$LAB_HOME/current/$DAEMON_BIN
HTTP=/etc/unifi-lab/http
# The stand-in Network Application takes any key. The browser saves this one again through Settings.
UNIFI_KEY=lab-stand-in-key
HOOK=/opt/unifi-lab/$HOOK_NAME
SOCKET=$HTTP_SOCKET
PROOF_FILE=$LAB_HOME/$NGINX_PROOF
ECHO=/run/lab-echo.sock
AUTH_CACHE=/var/cache/lab-nginx-auth
DAEMON=lab-control
NGINX=lab-nginx
ECHOER=lab-echo
LOGIN="Cookie: UNIFI_TOKEN=lab-login"
URL=http://127.0.0.1:8080

STEP=${1:-}
fails=0
pass() { echo "PASS $STEP: $*"; }
fail() { echo "FAIL $STEP: $*"; fails=$((fails + 1)); }
note() { echo "       $*"; }
expect() {
  local what=$1 got=$2 want=$3
  if [ "$got" = "$want" ]; then pass "$what = $got"; else fail "$what = '$got', want '$want'"; fi
}
status_of() { curl -s -o /dev/null -w '%{http_code}' "$@"; }
# tokens sets TOKENS to a write's two CSRF headers for $LOGIN, as the page sends them: UniFi's,
# from a read's answer, and the daemon's own, from api/session.
tokens() {
  TOKENS=(-H "X-Csrf-Token: $(unifi_token "$LOGIN" "$URL${BASE_PATH}api/state")"
    -H "$CSRF_HEADER: $(curl -s -H "$LOGIN" "$URL${BASE_PATH}api/session" | jq -r .csrf)")
}

step_nginx() {
  command -v nginx >/dev/null || {
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends nginx-light >/dev/null 2>&1
    systemctl disable --now nginx 2>/dev/null
  }
  mkdir -p "$HTTP"
  rm -f "$HTTP"/shared-runnable-*.conf
  rm -rf "$AUTH_CACHE"
  unifi_auth_conf /__auth >"$HTTP/auth.conf"
  [ -s "$PROOF_FILE" ] || (umask 077; head -c 32 /dev/urandom | base64 | tr '+/' '-_' | tr -d '=\n' >"$PROOF_FILE")
  render() { sed "s|@AUTH_INCLUDE@|$1|; s|@PROOF@|$(cat "$PROOF_FILE")|; s|@AUTH_HEADER@|$AUTH_HEADER|; s|@PROOF_HEADER@|$PROOF_HEADER|; s|@SOCKET@|$SOCKET|; s|@PAGE@|$2|" "$HOOK"; }
  render "include $HTTP/auth.conf;" "${BASE_PATH%/}" >"$HTTP/$HOOK_NAME"
  # The same hook, aimed at a server that echoes what it was sent: once as it is, and once
  # with the login include gone, as a changed UniFi could leave it.
  render "include $HTTP/auth.conf;" /echo | sed "s|$SOCKET|$ECHO|" >"$HTTP/shared-runnable-echo.conf"
  render "" /noauth | sed "s|$SOCKET|$ECHO|" >"$HTTP/shared-runnable-noauth.conf"
  render "" /noauth-daemon | sed "s|http://unix:$SOCKET|http://unix:$SOCKET:$BASE_PATH|" >"$HTTP/shared-runnable-noauth-daemon.conf"
  cat >"$HTTP/nginx.conf" <<CONF
user www-data;
pid /run/lab-nginx.pid;
error_log /var/log/lab-nginx.log warn;
events { worker_connections 64; }
http {
  access_log off;
  proxy_cache_path $AUTH_CACHE keys_zone=auth_responses:1m max_size=1m;
$(unifi_csrf_map)
  server {
    listen 127.0.0.1:8080;
$(unifi_auth_location /__auth "$ECHO")
    include $HTTP/shared-runnable-*.conf;
  }
}
CONF
  chmod 600 "$HTTP"/shared-runnable-*.conf
  if nginx -t -c "$HTTP/nginx.conf" 2>&1 | grep -q successful; then pass "nginx takes the hook"; else fail "nginx -t: $(nginx -t -c "$HTTP/nginx.conf" 2>&1)"; fi
  { unifi_login_py; cat <<'PY'; } >/opt/unifi-lab/echo.py
import http.server, json, os, socketserver, sys
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/auth": return unifi_auth(self)
        body = json.dumps({"path": self.path, "headers": {k.lower(): v for k, v in self.headers.items()}}).encode()
        self.send_response(200); self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def log_message(self, *a): pass
class S(socketserver.UnixStreamServer):
    def get_request(self):
        request, _ = super().get_request()
        return request, ("local", 0)
if os.path.exists(sys.argv[1]): os.remove(sys.argv[1])
s = S(sys.argv[1], H); os.chmod(sys.argv[1], 0o666); s.serve_forever()
PY
  systemctl stop $ECHOER $NGINX 2>/dev/null
  systemctl reset-failed $ECHOER $NGINX 2>/dev/null
  systemd-run --unit=$ECHOER --collect --quiet python3 /opt/unifi-lab/echo.py "$ECHO"
  systemd-run --unit=$NGINX --collect --quiet nginx -g 'daemon off;' -c "$HTTP/nginx.conf"
  sleep 1
  holds "127.0.0.1:8080" "$(ss -Hltnp 'sport = :8080')" $NGINX "nginx is up" "lab/step.sh lifecycle prepare teardown clears a lifecycle run's nginx. $(tail -1 /var/log/lab-nginx.log)"
}

# holds WHAT LISTENERS UNIT PASS HINT passes when UNIT's main process is among the listeners
# on WHAT. Another suite's leftover listener would answer in its place, as a killed lifecycle
# run's nginx and daemon once did, and every check after it would test that world instead.
holds() {
  local main who; main=$(systemctl show -p MainPID --value "$3")
  who=$(grep -o 'users:.*' <<<"$2")
  if [ "$main" != 0 ] && grep -q "pid=$main," <<<"$2"; then pass "$4"; else fail "$3 doesn't hold $1, which ${who:-nothing} holds. $5"; fi
}

# step_unifi stands in for the Network Application on loopback, holding one VPN Client that isn't
# ours. Our first client gets one route that blocks when down and one that goes direct.
step_unifi() {
  systemctl stop lab-unifi 2>/dev/null
  systemctl reset-failed lab-unifi 2>/dev/null
  (umask 077; echo "$UNIFI_KEY" >"$LAB_HOME/$NETWORK_KEY")
  systemd-run --unit=lab-unifi --collect --quiet "$LAB_HOME/current/controllab" unifi
  local start; start=$(date +%s)
  until curl -sk -m 2 -o /dev/null https://127.0.0.1:8443/; do
    [ $(( $(date +%s) - start )) -gt 10 ] && { fail "the stand-in never answered"; return; }
    sleep 0.2
  done
  pass "a stand-in Network Application answers on loopback"
}

step_daemon() {
  jq --arg key "$LAB_HOME/$NETWORK_KEY" '. + {listenGroup: "www-data", networkCredentialFile: $key, networkEndpoint: "https://127.0.0.1:8443/proxy/network"}' "$LAB_HOME/$BOOTSTRAP" >"$LAB_HOME/$BOOTSTRAP.new" && mv "$LAB_HOME/$BOOTSTRAP.new" "$LAB_HOME/$BOOTSTRAP"
  systemd-run --unit=$DAEMON --collect --quiet -p KillMode=process "$BIN" serve --data-dir "$LAB_HOME"
  local start; start=$(date +%s)
  until [ -S "$SOCKET" ]; do
    [ $(( $(date +%s) - start )) -gt 30 ] && { fail "the socket never appeared"; journalctl -u $DAEMON -n 30 -o cat | sed 's/^/       /'; return; }
    sleep 0.2
  done
  holds "$SOCKET" "$(ss -Hlxp "src $SOCKET")" $DAEMON "$DAEMON_BIN serve listens on $SOCKET" "lab/step.sh lifecycle prepare teardown clears a lifecycle run's daemon."
  expect "socket owner and mode" "$(stat -c '%U:%G %a' "$SOCKET")" "root:www-data 660"
  expect "the socket's own directory can be entered" "$(stat -c '%a' "$(dirname "$SOCKET")")" 755
  ss -ltnp 2>/dev/null | grep -q "$DAEMON_BIN" && fail "the daemon listens on a TCP port" || pass "the daemon has no TCP listener"
}

step_auth() {
  expect "the page with no login" "$(status_of "$URL$BASE_PATH")" 401
  expect "the API with no login" "$(status_of "$URL${BASE_PATH}api/state")" 401
  expect "the API with no login and forged headers" "$(status_of -H "$AUTH_HEADER: 200" -H "$PROOF_HEADER: $(cat "$PROOF_FILE")" "$URL${BASE_PATH}api/state")" 401
  expect "a login with a forged proof, which nginx replaces" "$(status_of -H "$LOGIN" -H "$PROOF_HEADER: forged" "$URL${BASE_PATH}api/state")" 200
  expect "the API with a bad login" "$(status_of -H 'Cookie: UNIFI_TOKEN=wrong' "$URL${BASE_PATH}api/state")" 401
  expect "the API with a login" "$(status_of -H "$LOGIN" "$URL${BASE_PATH}api/state")" 200
  expect "the page with a login" "$(status_of -H "$LOGIN" "$URL$BASE_PATH")" 200
  expect "a deep link with a login" "$(status_of -H "$LOGIN" "$URL${BASE_PATH}slots/eu")" 200
  local sent; sent=$(curl -s -H "$LOGIN" -H "$AUTH_HEADER: forged" -H "$PROOF_HEADER: forged" -H 'Authorization: Basic Zm9v' "$URL/echo/x")
  expect "the hook's proof replaces the browser's" "$(jq -r --arg h "${PROOF_HEADER,,}" '.headers[$h] == $proof' --arg proof "$(cat "$PROOF_FILE")" <<<"$sent")" true
  expect "the login cookie goes no further" "$(jq -r '.headers.cookie // "absent"' <<<"$sent")" absent
  expect "Authorization goes no further" "$(jq -r '.headers.authorization // "absent"' <<<"$sent")" absent
  sent=$(curl -s -H "$AUTH_HEADER: forged" "$URL/noauth/x")
  expect "with the login include gone, no header reaches the daemon" "$(jq -r --arg h "${AUTH_HEADER,,}" '.headers[$h] // "absent"' <<<"$sent")" absent
  expect "and the daemon closes the page" "$(status_of -H "$AUTH_HEADER: forged" "$URL/noauth-daemon/api/state")" 401
  expect "the socket itself refuses a request with no header" "$(status_of --unix-socket "$SOCKET" http://x${BASE_PATH}api/state)" 401
  expect "www-data can connect, as nginx's workers do, but a forged login gets nothing" "$(sudo -u www-data curl -s -o /dev/null -w '%{http_code}' -H "$AUTH_HEADER: 200" -H "$PROOF_HEADER: forged" --unix-socket "$SOCKET" http://x${BASE_PATH}api/state)" 401
  expect "www-data can't read the proof" "$(sudo -u www-data cat "$HTTP/$HOOK_NAME" >/dev/null 2>&1; echo $?)" 1
  expect "nobody else can" "$(sudo -u nobody curl -s -o /dev/null -w '%{http_code}' --unix-socket "$SOCKET" http://x${BASE_PATH}api/state)" 000
}

# step_cache reads the page as a browser does, inside the 10 s UniFi keeps a passed login: the
# page on the login's first subrequest, then its assets and the API on the cached answer. Each
# run uses a login of its own, so its first request is the cache's first sight of it.
step_cache() {
  local page asset login vouch vouched
  page=$(curl -s --unix-socket "$SOCKET" -H "$AUTH_HEADER: lab" -H "$PROOF_HEADER: $(cat "$PROOF_FILE")" "http://x$BASE_PATH")
  asset=$(grep -o 'assets/[^"]*\.js' <<<"$page" | head -1)
  [ -n "$asset" ] || { fail "the page names no script: ${page:0:120}"; return; }
  vouch() { curl -s -H "$1" "$URL/echo/x" | jq -r --arg h "${AUTH_HEADER,,}" '.headers[$h] // ""'; }
  vouched() { local v; v=$(vouch "$1"); note "$2: $AUTH_HEADER: ${v:-absent}" >&2; [ -n "$v" ] && echo yes || echo no; }
  login="Cookie: UNIFI_TOKEN=lab-login-$(date +%s%N)"
  expect "the page, a new login's first request" "$(status_of -H "$login" "$URL$BASE_PATH")" 200
  expect "its script, on the cached login" "$(status_of -H "$login" "$URL$BASE_PATH$asset")" 200
  expect "its favicon, on the cached login" "$(status_of -H "$login" "$URL${BASE_PATH}favicon.svg")" 200
  expect "api/session, on the cached login" "$(status_of -H "$login" "$URL${BASE_PATH}api/session")" 200
  expect "api/state, on the cached login" "$(status_of -H "$login" "$URL${BASE_PATH}api/state")" 200
  expect "the hook vouches for a cached login" "$(vouched "$login" cached)" yes
  expect "the hook vouches for a login's first request" "$(vouched "Cookie: UNIFI_TOKEN=lab-login-$(date +%s%N)" first)" yes
  expect "the page with no login, inside a login's cache window" "$(status_of "$URL$BASE_PATH")" 401
  expect "api/state with no login, inside the window" "$(status_of "$URL${BASE_PATH}api/state")" 401
  expect "api/state with a bad login, inside the window" "$(status_of -H 'Cookie: UNIFI_TOKEN=wrong' "$URL${BASE_PATH}api/state")" 401
  expect "the cached login, with the login include gone" "$(status_of -H "$login" "$URL/noauth-daemon/api/state")" 401
}

# step_csrf writes under UniFi's CSRF check in front of the daemon and the daemon's own behind it.
# Each write deletes a source that isn't there, so nothing is stored, and only the daemon's 404
# says both checks let it through. A refusal says whose it is: the daemon's JSON names a kind.
step_csrf() {
  local unifi daemon other sent
  write() {
    local out; out=$(curl -s -m 10 -o /tmp/lab-csrf.json -w '%{http_code}' -X DELETE -H "$LOGIN" -H "Idempotency-Key: lab-csrf-$RANDOM$RANDOM" "$@" "$URL${BASE_PATH}api/sources/lab-none")
    echo "$out $(jq -r 'if .kind then "daemon:" + .kind elif .error then "unifi" else "other" end' /tmp/lab-csrf.json 2>/dev/null || echo other)"
  }
  unifi=$(unifi_token "$LOGIN" "$URL${BASE_PATH}api/state")
  expect "a read's answer hands out UniFi's token" "$([ -n "$unifi" ] && echo yes || echo no)" yes
  expect "a second read's, on the cached login, hands out the same" "$(unifi_token "$LOGIN" "$URL${BASE_PATH}api/state")" "$unifi"
  other=$(unifi_token "Cookie: UNIFI_TOKEN=lab-login-other" "$URL${BASE_PATH}api/state")
  daemon=$(curl -s -H "$LOGIN" "$URL${BASE_PATH}api/session" | jq -r .csrf)
  expect "a write with no UniFi token, at nginx" "$(write -H "$CSRF_HEADER: $daemon")" "403 unifi"
  expect "a write with the daemon's token where UniFi reads its own, at nginx" "$(write -H "X-Csrf-Token: $daemon")" "403 unifi"
  expect "a write with another login's UniFi token, at nginx" "$(write -H "X-Csrf-Token: $other" -H "$CSRF_HEADER: $daemon")" "403 unifi"
  expect "a write with UniFi's token and not the daemon's, at the daemon" "$(write -H "X-Csrf-Token: $unifi")" "403 daemon:csrf"
  expect "a write with both, past both checks" "$(write -H "X-Csrf-Token: $unifi" -H "$CSRF_HEADER: $daemon")" "404 daemon:validation"
  sent=$(curl -s -H "$LOGIN" -H "X-Csrf-Token: $unifi" "$URL/echo/x")
  expect "the hook passes no X-Csrf-Token on to the daemon" "$(jq -r '.headers["x-csrf-token"] // "absent"' <<<"$sent")" absent
  rm -f /tmp/lab-csrf.json
}

# step_paste sends a 900 KiB pasted source through nginx. Go refuses its text with a 422, which
# only an answer from the daemon carries: a 413 would be nginx's limit.
step_paste() {
  local body=/tmp/lab-paste.json
  tokens
  python3 -c 'import json; print(json.dumps({"name": "Paste", "origin": {"kind": "manual", "text": ("not a server\n" * 80000)[:900 * 1024]}}))' >"$body"
  expect "a 900 KiB pasted source through nginx" "$(status_of -X PUT -H "$LOGIN" "${TOKENS[@]}" -H 'Idempotency-Key: lab-paste-1' -H 'If-None-Match: *' -H 'Content-Type: application/json' --data-binary @"$body" "$URL${BASE_PATH}api/sources/lab-paste")" 422
  rm -f "$body"
}

# step_forged tries to pause a slot with a forged login, at nginx and at the socket, and the slot stays as it was.
step_forged() {
  local slot=$2 before body='{"name":"forged","exit":{"kind":"group","group":"none"}}'
  before=$(curl -s -H "$LOGIN" "$URL${BASE_PATH}api/state" | jq -c --arg s "$slot" '.slots[] | select(.id == $s) | [.version, .exit.kind]')
  tokens
  expect "a write with no login, forged headers and the real proof, at nginx" "$(status_of -X PUT -H "$AUTH_HEADER: 200" -H "$PROOF_HEADER: $(cat "$PROOF_FILE")" "${TOKENS[@]}" -H 'Idempotency-Key: forged-1' -H 'Content-Type: application/json' -d "$body" "$URL${BASE_PATH}api/slots/$slot")" 401
  expect "a write with a bad login cookie, at nginx" "$(status_of -X PUT -H 'Cookie: UNIFI_TOKEN=wrong' "${TOKENS[@]}" -H 'Idempotency-Key: forged-2' -H 'Content-Type: application/json' -d "$body" "$URL${BASE_PATH}api/slots/$slot")" 401
  expect "a write at the socket as nginx's user, with a forged proof" "$(sudo -u www-data curl -s -o /dev/null -w '%{http_code}' -X PUT -H "$AUTH_HEADER: 200" -H "$PROOF_HEADER: forged" -H "$CSRF_HEADER: ${TOKENS[3]#*: }" -H 'Idempotency-Key: forged-3' -H 'Content-Type: application/json' -d "$body" --unix-socket "$SOCKET" "http://x${BASE_PATH}api/slots/$slot")" 401
  expect "the slot after all of them" "$(curl -s -H "$LOGIN" "$URL${BASE_PATH}api/state" | jq -c --arg s "$slot" '.slots[] | select(.id == $s) | [.version, .exit.kind]')" "$before"
}

# The kernel's and the stand-in's own word, for what the page has just said.
step_link() { echo "link=$([ $(( $(cat "/sys/class/net/$(curl -s -H "$LOGIN" "$URL${BASE_PATH}api/state" | jq -r --arg s "$2" '.slots[] | select(.id == $s) | .unifi.interface')/flags") & 1 )) -eq 1 ] && echo up || echo down)"; }
step_client() { echo "client=$(curl -sk https://127.0.0.1:8443/proxy/network/api/s/default/rest/networkconf | jq -r --arg n "$2" '[.data[] | select(.name == $n)] | length')"; }
step_routes() { echo "routes=$(curl -sk https://127.0.0.1:8443/proxy/network/v2/api/site/default/trafficroutes | jq -c '[.[] | .kill_switch_enabled]')"; }

# The provider moves one of the Acme feed's servers, keeping its name and credentials.
step_move-acme() {
  sed -i 's/198\.51\.100\.206/198.51.100.240/g' /etc/unifi-lab/feeds/acme
  pass "the feed moved a server"
}

step_status() {
  local out; out=$("$BIN" status --data-dir "$LAB_HOME" 2>&1)
  echo "$out" | sed 's/^/       /'
  grep -q '^wan ' <<<"$out" && pass "$DAEMON_BIN status prints the daemon's view" || fail "status: $out"
}

# step_connect plays UniFi's part with the lab's wgclt1, from the keys the daemon stored,
# as a real VPN Client would hold them. It proves the tunnel and what crosses it.
step_connect() {
  local keys private public port before after
  keys=$(python3 - "$LAB_HOME/$STATE_DB" "$2" <<'PY'
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
  local start got=none; start=$(date +%s)
  while [ $(( $(date +%s) - start )) -lt 40 ]; do
    got=$(ip netns exec lan curl -s -m 3 http://203.0.113.81/ip)
    [ -n "$got" ] && break
    sleep 1
  done
  case "$got" in
    198.51.100.*) pass "a LAN client reaches the internet through the slot, exiting from $got";;
    *) fail "a LAN client through the slot got '$got'";;
  esac
  ip netns exec lan curl -s -m 20 -o /dev/null 'http://203.0.113.81/bytes?n=20000000' && pass "20 MB crossed the slot" || fail "the download through the slot failed"
}

# The VM's clock at the stop, which the daemon's own switch times are read against.
step_kill() { systemctl stop "lab-xray-$2" && pass "stopped $2 at epoch $(date +%s.%3N)"; }

step_start-server() {
  systemd-run --unit="lab-xray-$2" --collect --quiet -p Restart=on-failure -p NetworkNamespacePath="/run/netns/$2" /usr/local/bin/xray run -c "/etc/unifi-lab/$2.json"
  pass "started $2"
}

step_delay() { ip netns exec "$2" tc qdisc replace dev eth0 root netem delay "$3" && pass "delayed what $2 sends by $3"; }
step_undelay() { ip netns exec "$2" tc qdisc del dev eth0 root && pass "took $2's delay off"; }

step_secrets() {
  local logs; logs=$(journalctl -u $DAEMON -o cat; cat /var/log/lab-nginx.log 2>/dev/null)
  expect "log lines" "$([ -n "$logs" ] && echo some)" some
  expect "the feed's token in any log" "$(grep -c lab-s3cret <<<"$logs")" 0
  expect "the feed's token in api/state" "$(curl -s -H "$LOGIN" "$URL${BASE_PATH}api/state" | grep -c lab-s3cret)" 0
  expect "the feed's token in api/servers" "$(curl -s -H "$LOGIN" "$URL${BASE_PATH}api/servers" | grep -c lab-s3cret)" 0
  expect "a private key in any log" "$(grep -cE 'x_wireguard|PrivateKey' <<<"$logs")" 0
  expect "the UniFi key in any log" "$(grep -c "$UNIFI_KEY" <<<"$logs")" 0
  expect "the UniFi key in api/state" "$(curl -s -H "$LOGIN" "$URL${BASE_PATH}api/state" | grep -c "$UNIFI_KEY")" 0
}

step_teardown() {
  systemctl stop $NGINX $ECHOER lab-unifi 2>/dev/null
  rm -f "$ECHO"
  pass "stopped nginx"
}

declare -F "step_$STEP" >/dev/null || { echo "unknown step: $STEP" >&2; exit 2; }
"step_$STEP" "$@"
[ "$fails" -eq 0 ]
