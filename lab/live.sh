#!/usr/bin/env bash
# Starts the real daemon with the page in the VM, on the controller lab's stand-in servers as the
# page lab has it, and serves it on a LAN address of the Mac with the stand-in login until Ctrl-C.
# Each start begins with the state the last one left, kept through any lab suite run in between,
# and --fresh begins with none. --real-targets leaves the lab's probe and baseline targets out of
# the bootstrap, so the daemon checks servers against its built-in public test pages and a real
# subscription can be tried. Usage:
#   lab/live.sh [--fresh] [--real-targets] LAN_IP [PORT]   # PORT defaults to 5465, and PORT+1 is the tunnel's
source "$(dirname "$0")/lib.sh"

real=0 fresh=
while :; do
  case "${1:-}" in
    --real-targets) real=1; shift ;;
    --fresh) fresh=fresh; shift ;;
    *) break ;;
  esac
done
lan=${1:-}
port=${2:-5465}
[ -n "$lan" ] || { sed -n '2,8p' "$0" | cut -c3-; exit 2; }
tunnel_port=$((port + 1))
vm_running || { echo "VM $VM is not running. Run lab/up.sh first." >&2; exit 1; }
if pgrep -f 'lab/verify-(runtim[e]|contro[l]|datapat[h]|lifecycl[e]|pag[e])' >/dev/null; then
  echo "A lab suite is using VM $VM. Its setup and this script's would break each other." >&2
  exit 1
fi

page_install
vm_root "$GUEST_DIR/control.sh" setup
vm_root "$GUEST_DIR/control.sh" live $fresh
feed_titles
if [ "$real" = 1 ]; then
  vm_root sh -c "b=$LAB_HOME/$BOOTSTRAP"'; jq "del(.probeTargets, .baselineTargets)" $b >$b.new && mv $b.new $b'
fi
for s in nginx unifi daemon auth; do
  echo "--- $s"
  vm_root "$GUEST_DIR/page.sh" "$s"
done

tunnel= proxy=
trap 'kill $tunnel $proxy 2>/dev/null || true; wait 2>/dev/null || true' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP
ssh -F "$HOME/.lima/$VM/ssh.config" -N -o ControlMaster=no -o ControlPath=none -o ServerAliveInterval=30 \
  -o ExitOnForwardFailure=yes -L "127.0.0.1:$tunnel_port:127.0.0.1:8080" "lima-$VM" &
tunnel=$!
until curl -s -o /dev/null -m 1 "http://127.0.0.1:$tunnel_port/"; do
  kill -0 "$tunnel" 2>/dev/null || { echo "the tunnel to the VM's nginx didn't come up" >&2; exit 1; }
  sleep 0.2
done
targets=lab; [ "$real" = 1 ] && targets="built-in public"
echo "serving http://$lan:$port$BASE_PATH with $targets test pages until Ctrl-C"
# In the background, so a signal to this script reaches its traps at once and they stop both.
python3 -I "$LAB_DIR/live-proxy.py" "$lan" "$port" "$tunnel_port" "${BASE_PATH%/}" &
proxy=$!
wait "$proxy"
