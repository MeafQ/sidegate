#!/usr/bin/env bash
# Rebuilds the lab topology in the VM and runs the design B data path checks.
source "$(dirname "$0")/lib.sh"

vm_running || { echo "VM $VM is not running. Run lab/up.sh first." >&2; exit 1; }
sync_guest
vm_root "$GUEST_DIR/topology.sh"

log=$(mktemp)
rc=0
for c in a b c d e f g h i; do
  vm_root "$GUEST_DIR/checks.sh" "$c" 2>&1 | tee -a "$log" || rc=1
done

mkdir -p "$LAB_DIR/results"
out="$LAB_DIR/results/datapath-$(date +%F).md"
{
  echo "# Design B data path, $(date '+%F %T %Z')"
  echo
  echo "Produced by \`lab/up.sh && lab/verify-datapath.sh\` on $(sysctl -n machdep.cpu.brand_string), Lima $(limactl --version | awk '{print $3}') vz VM with $(vm nproc) vCPUs."
  echo "Guest: $(vm uname -r), $(vm_root iptables --version), $(vm /usr/local/bin/xray version | head -1 | cut -d' ' -f1-2)."
  echo
  echo "## Throughput and CPU (check g)"
  echo
  echo "VM numbers on an Apple M4. They are not the gateway's Cortex-A53 numbers and only compare paths with each other."
  echo "CPU columns are percent of one core over the run. softirq and busy add up all 4 vCPUs (400% = all busy)."
  echo "Everything shares the VM: srvA, labweb and iperf3 run on the same vCPUs as the gateway."
  echo
  vm_root cat /var/lib/unifi-lab/throughput.md
  echo
  echo "## Verify output"
  echo
  echo '```'
  cat "$log"
  echo '```'
} >"$out"
rm -f "$log"
echo "results: $out"
[ "$rc" -eq 0 ] && echo "ALL PASS" || echo "SOME CHECKS FAILED"
exit "$rc"
