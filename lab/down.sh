#!/usr/bin/env bash
source "$(dirname "$0")/lib.sh"
vm_exists && limactl delete --force "$VM"
rm -rf "$STATE"
