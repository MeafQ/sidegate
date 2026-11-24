#!/usr/bin/env bash
# Builds the daemon for the gateway target and proves its controller against a real core and
# stand-in servers, with production's failure timings: killed, silent and slow servers replaced in
# 80 s, an Exit change that keeps the gate up and one
# that closes at the 30 s deadline, a slot's move closing its flows and no one else's, broken UDP moving a slot only when a rival's works, stalled validation and wedged reads, Pause, tcp_info under a
# lossy upload, a WAN outage that switches nothing and blames nobody, blocked and blipping
# baselines, a sole member's recovery, a server Exit leaving the feed, a provider's quota in the view,
# a killed daemon, three slots on one dead server, eight dying at once, a
# whole group going silent, a closed slot's sweep at 12 and 64 kept, core memory at 64 roots by 8 hops,
# and a Acme-shaped feed. Writes lab/results/control-<date>.md and exits non-zero if any
# check fails. Takes about 60 minutes. lab/step.sh control STEP runs one step alone.
source "$(dirname "$0")/lib.sh"

vm_running || { echo "VM $VM is not running. Run lab/up.sh first." >&2; exit 1; }
ROOT=$(cd "$LAB_DIR/.." && pwd)
mkdir -p "$STATE"
for target in $DAEMON_BIN:./cmd/$DAEMON_BIN controllab:./lab/controllab; do
  (cd "$ROOT" && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o "$STATE/${target%%:*}" "${target#*:}")
done
sync_guest
COPYFILE_DISABLE=1 tar --no-xattrs -C "$STATE" -cf - "$DAEMON_BIN" controllab |
  vm_root sh -c "mkdir -p $LAB_HOME/current && tar -C $LAB_HOME/current -xf - &&
    ln -sfn /usr/local/bin/xray $LAB_HOME/current/xray"

log=$(mktemp)
rc=0
step() { echo "--- $1 $(date +%T)" | tee -a "$log"; vm_root "$GUEST_DIR/control.sh" "$1" 2>&1 | tee -a "$log" || rc=1; }
for s in start kill blackhole degrade loss exitchange swapgap heldmove closeflows lowudp udpbroken deadline stalltest wedge pause passive wan blocked blip recover assigned quota restart cost contract; do step "$s"; done

contract=$(mktemp -d)
vm_root tar -C /tmp/control-contract -cf - . | tar -C "$contract" -xf -
if [ -d "$ROOT/web/node_modules" ]; then
  (cd "$ROOT/web" && node --experimental-strip-types scripts/check-daemon-json.mts "$contract") 2>&1 | tee -a "$log" || rc=1
else
  echo "SKIP contract: web/node_modules is absent, so the schema check did not run" | tee -a "$log"
fi
rm -rf "$contract"
for s in three storm allsilent closed12 sweep64 memory acme teardown; do step "$s"; done

mkdir -p "$LAB_DIR/results"
out="$LAB_DIR/results/control-$(date +%F).md"
{
  echo "# Daemon controller in the lab, $(date '+%F %T %Z')"
  echo
  echo "Produced by \`lab/verify-control.sh\` at $(git -C "$ROOT" rev-parse --short HEAD) on $(sysctl -n machdep.cpu.brand_string), Lima $(limactl --version | awk '{print $3}') vz VM with $(vm nproc) vCPUs."
  echo "Guest: $(vm uname -r), $(vm /usr/local/bin/xray version | head -1 | cut -d' ' -f1-2). Failure timings are production's."
  echo
  echo '```'
  cat "$log"
  echo '```'
} >"$out"
rm -f "$log"
echo "results: $out"
[ "$rc" -eq 0 ] && echo "ALL PASS" || echo "SOME CHECKS FAILED"
exit "$rc"
