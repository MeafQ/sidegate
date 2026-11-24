#!/usr/bin/env bash
# Builds the daemon for the gateway target and lets its runtime build design B's
# data path in the VM from a two-slot plan. Then proves it: the datapath checks
# b to i against the daemon-made plumbing, idempotency, the tunnel egress guard,
# repair after a foreign flush, gates closed on a conflict, the probe routes,
# telemetry, a hot switch, adoption across a daemon restart, plans that change only
# probe roots, rewrites backed off under a flapping check, gates that follow the
# check when a rewrite can't land or state can't be read, one link's failure kept to
# that link, a slot whose UDP port another process holds kept closed, refused
# servers, the renderer against the pinned core, recovery after a core outage,
# gates closed by a stopped or crashed core, orphaned and wedged tools, a
# reinstall after /data loss, foreign links and marked foreign state left alone,
# markers read under named values on three iproute2 versions, and owned-only teardown.
# Writes lab/results/runtime-<date>.md and exits non-zero if any check fails.
source "$(dirname "$0")/lib.sh"

vm_running || { echo "VM $VM is not running. Run lab/up.sh first." >&2; exit 1; }
echo "restarting $VM, so the runtime installs from a fresh boot"
limactl --log-level=error stop "$VM" >/dev/null
limactl --log-level=error start --tty=false "$VM" >/dev/null
ROOT=$(cd "$LAB_DIR/.." && pwd)
mkdir -p "$STATE"
# Only runtimelab carries the lab build's hold on a link up; the daemon is built as the gateway's is.
(cd "$ROOT" && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o "$STATE/$DAEMON_BIN" ./cmd/$DAEMON_BIN)
(cd "$ROOT" && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -tags lab -o "$STATE/runtimelab" ./lab/runtimelab)
sync_guest
vm_root "$GUEST_DIR/iproute2.sh"
(cd "$ROOT" && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go test -c -trimpath -o "$STATE/runtime.test" ./internal/runtime)
COPYFILE_DISABLE=1 tar --no-xattrs -C "$STATE" -cf - "$DAEMON_BIN" runtimelab runtime.test |
  vm_root sh -c "mkdir -p $LAB_HOME/current && tar -C $LAB_HOME/current -xf - &&
    ln -sfn /usr/local/bin/xray $LAB_HOME/current/xray"

log=$(mktemp)
rc=0
step() { vm_root "$GUEST_DIR/runtime.sh" "$1" 2>&1 | tee -a "$log" || rc=1; }
check() { vm_root bash -c "eval \"\$($GUEST_DIR/runtime.sh env)\" && $GUEST_DIR/checks.sh $1" 2>&1 | tee -a "$log" || rc=1; }
for s in setup fresh install idempotent; do step "$s"; done
for c in b c d e f g h i; do check "$c"; done
for s in egress localguard established ipv6guard flush conflict foreignrule probe telemetry switch routing crossslot network balancer premise restart idle churn flapping noland nobinary blips gatelock apistall slowread portheld heldadd outage corestop coredeath orphan hung linkcrash reinstall absentlinks foreignlink collision foreign orphanrule mixed renderers named teardown; do step "$s"; done

mkdir -p "$LAB_DIR/results"
out="$LAB_DIR/results/runtime-$(date +%F).md"
{
  echo "# Daemon runtime in the lab, $(date '+%F %T %Z')"
  echo
  echo "Produced by \`lab/verify-runtime.sh\` at $(git -C "$ROOT" rev-parse --short HEAD) on $(sysctl -n machdep.cpu.brand_string), Lima $(limactl --version | awk '{print $3}') vz VM with $(vm nproc) vCPUs."
  echo "Guest: $(vm uname -r), $(vm_root iptables --version), $(vm /usr/local/bin/xray version | head -1 | cut -d' ' -f1-2)."
  echo
  echo '```'
  cat "$log"
  echo '```'
} >"$out"
rm -f "$log"
echo "results: $out"
[ "$rc" -eq 0 ] && echo "ALL PASS" || echo "SOME CHECKS FAILED"
exit "$rc"
