#!/usr/bin/env bash
# Stops the VM and keeps its disk, so the next up.sh is quick.
source "$(dirname "$0")/lib.sh"
vm_running && limactl stop "$VM"
vm_running && { echo "FAIL: $VM still running" >&2; exit 1; }
echo "$VM stopped"
