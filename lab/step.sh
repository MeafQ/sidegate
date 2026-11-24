#!/usr/bin/env bash
# Runs named lab steps alone on the Mac's VM, without a whole verify script. Each control and
# runtime step builds what it needs. Usage:
#   lab/step.sh control kill deadline    # controller steps, in order, then the world's teardown
#   lab/step.sh runtime setup install    # runtime steps, in order
#   lab/step.sh checks b                 # a datapath check, on whatever the VM holds now
#   lab/step.sh page board block         # the page lab with those browser scenarios, after its sources and slot
#   lab/step.sh lifecycle prepare install # lifecycle steps, in order, with a built bundle. A reboot is yours to do.
# Add --keep before the suite to leave the world running for the next call. Prints each step's
# time and the total. Exits non-zero if any check fails.
source "$(dirname "$0")/lib.sh"

keep=0
[ "${1:-}" = --keep ] && { keep=1; shift; }
suite=${1:-}; shift || true
[ -n "$suite" ] && [ "$#" -gt 0 ] || { sed -n '2,10p' "$0" | cut -c3-; exit 2; }
vm_running || { echo "VM $VM is not running. Run lab/up.sh first." >&2; exit 1; }
ROOT=$(cd "$LAB_DIR/.." && pwd)
mkdir -p "$STATE"
if [ "$suite" = page ]; then
  began=$(date +%s); log=$(mktemp)
  only=$(IFS=,; echo "$*")
  rc=0
  page_lab "$log" "$only"
  rm -f "$log"
  echo "VM time: $(( $(date +%s) - began )) s"
  exit "$rc"
fi
built=$(date +%s)
case "$suite" in
  control) bins="$DAEMON_BIN:./cmd/$DAEMON_BIN controllab:./lab/controllab" ;;
  runtime) bins="$DAEMON_BIN:./cmd/$DAEMON_BIN runtimelab:./lab/runtimelab" ;;
  checks) bins="" ;;
  lifecycle)
    bins=""
    lifecycle_bundles
    ;;
  *) echo "unknown suite $suite" >&2; exit 2 ;;
esac
names=""
for target in $bins; do
  # Only runtimelab carries the lab build's hold on a link up; the daemon is built as the gateway's is.
  tags=""; [ "${target%%:*}" = runtimelab ] && tags="-tags lab"
  (cd "$ROOT" && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath $tags -o "$STATE/${target%%:*}" "${target#*:}")
  names="$names ${target%%:*}"
done
sync_guest
[ "$suite" = lifecycle ] && lifecycle_copy
if [ "$suite" = runtime ]; then
  vm_root "$GUEST_DIR/iproute2.sh"
  (cd "$ROOT" && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go test -c -trimpath -o "$STATE/runtime.test" ./internal/runtime)
  names="$names runtime.test"
fi
if [ -n "$names" ]; then
  # shellcheck disable=SC2086
  COPYFILE_DISABLE=1 tar --no-xattrs -C "$STATE" -cf - $names |
    vm_root sh -c "mkdir -p $LAB_HOME/current && tar -C $LAB_HOME/current -xf - &&
      ln -sfn /usr/local/bin/xray $LAB_HOME/current/xray"
fi
echo "built and copied in $(( $(date +%s) - built )) s"

rc=0
total=0
for s in "$@"; do
  began=$(date +%s)
  echo "--- $s $(date +%T)"
  case "$suite" in
    checks) vm_root bash -c "eval \"\$($GUEST_DIR/runtime.sh env)\" && $GUEST_DIR/checks.sh $s" || rc=1 ;;
    # A step may take an argument, as in "snapshot pre".
    # shellcheck disable=SC2086
    *) vm_root "$GUEST_DIR/$suite.sh" $s || rc=1 ;;
  esac
  took=$(( $(date +%s) - began )); total=$(( total + took ))
  echo "--- $s took $took s"
done
if [ "$suite" = control ] && [ "$keep" = 0 ]; then vm_root "$GUEST_DIR/control.sh" teardown >/dev/null; fi
echo "VM time: $total s"
exit "$rc"
