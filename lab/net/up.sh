#!/usr/bin/env bash
# Creates or starts the unifi-lab-net VM and installs the pinned Network Application.
source "$(dirname "$0")/lib.sh"

vm_exists || limactl create --name="$VM" --tty=false --param "blockSubnet=$LAB_BLOCK_SUBNET" "$NET_DIR/lima.yaml"
vm_running || limactl start --tty=false "$VM"

COPYFILE_DISABLE=1 tar --no-xattrs -C "$NET_DIR" -cf - provision.sh |
  vm_root sh -c "mkdir -p $GUEST_DIR && tar -C $GUEST_DIR -xf - && chmod +x $GUEST_DIR/provision.sh"
vm_root "$GUEST_DIR/provision.sh" "$LAB_BLOCK_SUBNET"
