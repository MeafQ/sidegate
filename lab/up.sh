#!/usr/bin/env bash
# Creates or starts the unifi-lab VM (Ubuntu 20.04 arm64, kernel 5.4) and provisions it.
source "$(dirname "$0")/lib.sh"

if ! vm_exists; then
  limactl create --name="$VM" --tty=false "$LAB_DIR/lima.yaml"
fi
vm_running || limactl start --tty=false "$VM"

kernel=$(vm uname -r)
case "$kernel" in
  5.4.*) echo "kernel $kernel" ;;
  *) echo "FAIL: kernel $kernel is not 5.4" >&2; exit 1 ;;
esac

sync_guest
vm_root "$GUEST_DIR/provision.sh"
