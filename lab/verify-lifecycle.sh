#!/usr/bin/env bash
# Launches the update job from a stand-in daemon that the job restarts, pastes the one command
# against the stand-in release server with each download cut, missing, changed or redirected, installs
# with it, again, over another release and twice at once, then takes one bundle through install,
# installs killed between writing a hook nginx refuses and
# testing it, a reboot, the page through the real hook, forged headers,
# a reboot with a daemon that can't start, an upgrade with traffic flowing that tries again a hook nginx
# refused, an upgrade whose daemon
# doesn't answer and is rolled back, upgrades killed or rebooted mid-switch that go back at the next
# start, a recovery killed half way, a new core that exits at once, a new core where none ran, over an
# upgrade killed before its switch, two upgrades at once, uninstall with an empty diff against the
# pre-install snapshot, data and run dirs with every character a unit can carry, and updates over SSH and
# through the page, landing, refused, cut, hung and put back under a held lock. Writes lab/results/lifecycle-<date>.md and
# exits non-zero if any check fails. Takes about 15 minutes. Refuses to run beside another suite.
source "$(dirname "$0")/lib.sh"

vm_running || { echo "VM $VM is not running. Run lab/up.sh first." >&2; exit 1; }
if pgrep -f 'lab/verify-(runtime|control|page|datapath)' >/dev/null; then
  echo "Another lab suite is using VM $VM. This script reboots it and would break that run." >&2
  exit 1
fi
ROOT=$(cd "$LAB_DIR/.." && pwd)
mkdir -p "$STATE"

lifecycle_bundles

log=$(mktemp)
rc=0
reboot_vm() {
  echo "--- reboot $(date +%T)" | tee -a "$log"
  limactl stop "$VM" >/dev/null 2>&1 && limactl start --tty=false "$VM" >/dev/null 2>&1 || { echo "FAIL reboot: the VM didn't come back" | tee -a "$log"; rc=1; }
  vm_root systemctl is-system-running --wait >/dev/null 2>&1 || true
}
step() {
  echo "--- $1 $(date +%T)" | tee -a "$log"
  vm_root "$GUEST_DIR/lifecycle.sh" "$@" 2>&1 | tee -a "$log" || { echo "FAIL $1: the step exited non-zero" | tee -a "$log"; rc=1; }
}

reboot_vm
lifecycle_copy

step prepare
[ "$rc" -eq 0 ] || { echo "prepare failed, so nothing else runs" >&2; exit 1; }
step snapshot pre
step job
step plan
step command unarrived
step command unchecked
step command install
step page
step command again
step command other
step command together
step refused
step interrupted install
step interrupted uninstall
step interrupted first-daemon
step install
step page
step forged
step contract
step configure
step connect
step status
reboot_vm
step booted
step page
step forged
step connect
step break
reboot_vm
step broken
step repair
step connect
step heal
step interrupted daemon
step upgrade
step rollback
step cut kill
step cut missing
step cut gone
step cut prior
step cut core
step cut recovery
step deadcore
step downcore
step deliberate
step busy
step cut reboot
reboot_vm
step back
step connect
step status
step key
step keep
step reinstall
step purge
step paths
step update install
step page
step configure
step connect
step update plan
step update refused
step update busy
step update moved
step update nostart
step update hung
step update unsaid
step update land
step update core
step update uninstall
step update paths
step teardown

mkdir -p "$LAB_DIR/results"
out="$LAB_DIR/results/lifecycle-$(date +%F).md"
{
  echo "# Install, reboot, upgrade and uninstall in the lab, $(date '+%F %T %Z')"
  echo
  echo "Produced by \`lab/verify-lifecycle.sh\` at $(git -C "$ROOT" rev-parse --short HEAD), Lima $(limactl --version | awk '{print $3}') vz VM."
  echo "The login and UniFi's nginx config are stand-ins, on the gateway's real paths. No Network Application runs here."
  echo
  echo "$(grep -c '^PASS' "$log") passed, $(grep -c '^FAIL' "$log") failed."
  echo
  echo '```'
  cat "$log"
  echo '```'
} >"$out"
rm -f "$log"
echo "results: $out"
[ "$rc" -eq 0 ] && echo "ALL PASS" || echo "SOME CHECKS FAILED"
exit "$rc"
