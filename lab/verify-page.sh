#!/usr/bin/env bash
# Builds the daemon with the page in it, runs it in the VM on the controller lab's stand-in
# servers behind the real nginx hook, a stand-in login and a stand-in Network Application, and
# drives the page in a real browser: a source that answers 404 until Retry sends another User-Agent,
# a source's settings saved, the Acme-shaped feed, a slot added and named on the board beside a
# foreign client, the Exit menu, Acme's configs and where its servers' countries came from, a
# filter preview, a group edit, a group's delete waiting on the slot that uses it, Block, Direct,
# the switch menu on a group and a source, a forged login refused, Settings with UniFi, Retention
# and the Xray the core runs, a switch that appears live over SSE, Server replaced, and a
# provider's analytics. Then, on the lifecycle suite's gateway, v0.0.1 installed by the one command, the page
# checks, offers v0.0.2 from the stand-in release server, is pressed and confirmed, goes away with the daemon
# and reloads itself onto the new release. Writes lab/results/page-<date>.md and its screenshots to
# lab/.state/page, and exits non-zero if any check fails. Takes about 15 minutes.
source "$(dirname "$0")/lib.sh"

vm_running || { echo "VM $VM is not running. Run lab/up.sh first." >&2; exit 1; }
if pgrep -f 'lab/verify-(runtim[e]|contro[l]|datapat[h]|lifecycl[e])' >/dev/null; then
  echo "Another lab suite is using VM $VM. Its setup and this script's teardown would break each other." >&2
  exit 1
fi
log=$(mktemp)
rc=0
page_lab "$log" ""

mkdir -p "$LAB_DIR/results"
out="$LAB_DIR/results/page-$(date +%F).md"
{
  echo "# The page on the real daemon in the lab, $(date '+%F %T %Z')"
  echo
  echo "Produced by \`lab/verify-page.sh\` at $(git -C "$LAB_DIR" rev-parse --short HEAD), Lima $(limactl --version | awk '{print $3}') vz VM."
  echo "The login and the Network Application are stand-ins."
  echo
  echo '```'
  cat "$log"
  echo '```'
} >"$out"
rm -f "$log"
echo "results: $out"
[ "$rc" -eq 0 ] && echo "ALL PASS" || echo "SOME CHECKS FAILED"
exit "$rc"
