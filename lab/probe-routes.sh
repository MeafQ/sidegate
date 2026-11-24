#!/usr/bin/env bash
# Measures open point 1 of the probing plan: what one on-demand probe route costs on the pinned core,
# beside 12 and 64 routes already there. Runs internal/runtime's TestProbeRouteCost in the VM, in a
# network namespace of its own so the core's fixed API address meets nothing else. CYCLES sets the
# cycles per measurement, 200 by default. Prints the measured lines; the write-up goes in
# lab/results/probe-routes-<date>.md. Refuses to run beside another suite.
source "$(dirname "$0")/lib.sh"

if pgrep -f 'lab/verify-(runtim[e]|contro[l]|datapat[h]|lifecycl[e]|pag[e])' >/dev/null; then
  echo "another lab suite is running; the VM's CPU is not ours to measure" >&2
  exit 1
fi
vm_running || { echo "VM $VM is not running. Run lab/up.sh first." >&2; exit 1; }
ROOT=$(cd "$LAB_DIR/.." && pwd)
mkdir -p "$STATE"
(cd "$ROOT" && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go test -c -trimpath -o "$STATE/runtime.test" ./internal/runtime)
COPYFILE_DISABLE=1 tar --no-xattrs -C "$STATE" -cf - runtime.test | vm_root sh -c "mkdir -p $LAB_HOME/bench && tar -C $LAB_HOME/bench -xf -"
echo "load before: $(vm uptime)"
vm_root sh -c "ip netns del probecost 2>/dev/null; ip netns add probecost && ip -n probecost link set lo up &&
  cd /tmp && PROBE_COST_CYCLES=${CYCLES:-200} XRAY_BIN=/usr/local/bin/xray ip netns exec probecost \
    $LAB_HOME/bench/runtime.test -test.run '^TestProbeRouteCost$' -test.v -test.timeout 60m; rc=\$?; ip netns del probecost; exit \$rc" |
  grep -E 'measured:|^(--- |FAIL|ok|PASS)|_test.go'
echo "load after: $(vm uptime)"
