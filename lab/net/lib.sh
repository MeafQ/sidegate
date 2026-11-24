# Sourced by the Mac-side scripts of the Network Application lab.
set -euo pipefail
NET_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
VM=unifi-lab-net
GUEST_DIR=/opt/unifi-lab-net
BASE_URL=https://127.0.0.1:18443
# The IPv4 subnet the VM must not reach, such as the Mac's LAN. Empty blocks nothing.
LAB_BLOCK_SUBNET=${LAB_BLOCK_SUBNET:-}

vm() { limactl shell --workdir / "$VM" "$@"; }
vm_root() { vm sudo "$@"; }
vm_exists() { limactl list -q 2>/dev/null | grep -qx "$VM"; }
vm_running() { [ "$(limactl list --format '{{.Status}}' "$VM" 2>/dev/null)" = Running ]; }
