#!/usr/bin/env bash
# Deletes the VM. Touches nothing of the unifi-lab VM.
source "$(dirname "$0")/lib.sh"
vm_exists && limactl delete --force "$VM"
