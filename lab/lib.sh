# Sourced by the Mac-side scripts.
set -euo pipefail
LAB_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
source "$LAB_DIR/names.sh"
VM=unifi-lab
STATE="$LAB_DIR/.state"
GUEST_DIR=/opt/unifi-lab

vm() { limactl shell --workdir / "$VM" "$@"; }
vm_root() { vm sudo "$@"; }

vm_exists() { limactl list -q 2>/dev/null | grep -qx "$VM"; }
vm_running() { [ "$(limactl list --format '{{.Status}}' "$VM" 2>/dev/null)" = Running ]; }

build_labweb() {
  mkdir -p "$STATE"
  (cd "$LAB_DIR/labweb" && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o "$STATE/labweb" .)
}

sync_guest() {
  build_labweb
  COPYFILE_DISABLE=1 tar --no-xattrs -C "$LAB_DIR" -cf - names.sh unifi-login.sh provision.sh iproute2.sh topology.sh checks.sh runtime.sh control.sh page.sh -C "$STATE" labweb -C "$LAB_DIR/../internal/catalogue/testdata" acme-shape.json -C "$LAB_DIR/../deploy" xray.pin |
    vm_root sh -c "mkdir -p $GUEST_DIR && tar -C $GUEST_DIR -xf - && chmod +x $GUEST_DIR/*.sh $GUEST_DIR/labweb"
}

# page_install builds the page, the daemon and controllab, and copies them, the guest scripts and the nginx hook into the VM.
page_install() {
  local root target
  root=$(cd "$LAB_DIR/.." && pwd)
  mkdir -p "$STATE"
  "$root/scripts/build-page.sh"
  for target in $DAEMON_BIN:./cmd/$DAEMON_BIN controllab:./lab/controllab; do
    (cd "$root" && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o "$STATE/${target%%:*}" "${target#*:}")
  done
  sync_guest
  COPYFILE_DISABLE=1 tar --no-xattrs -C "$root/deploy/nginx" -cf - "$HOOK_NAME" | vm_root tar -C "$GUEST_DIR" -xf -
  COPYFILE_DISABLE=1 tar --no-xattrs -C "$STATE" -cf - "$DAEMON_BIN" controllab |
    vm_root sh -c "mkdir -p $LAB_HOME/current && tar -C $LAB_HOME/current -xf - &&
      ln -sfn /usr/local/bin/xray $LAB_HOME/current/xray"
}

# lifecycle_bundles builds the lifecycle suite's bundles on the Mac into $STATE/bundles: one and two by
# scripts/build-bundle.sh, and three and four with cores of their own, each this checkout's daemon
# built with a pin on its core. Three's core is the pinned Xray with bytes after its end, which still
# runs, and four's exits at once. v0.0.1 and v0.0.2 are releases as a tag builds them, at the stand-in's
# address. v0.0.3 is one whose daemon hangs: its sidegated is a script that serves nothing and gives every
# other command to the real one, hang/, beside it in the VM. v0.0.4 is one with three's core. Call it bare, as
# page_lab.
lifecycle_bundles() {
  local root dir name core
  root=$(cd "$LAB_DIR/.." && pwd)
  rm -rf "$STATE/bundles"; mkdir -p "$STATE/bundles"
  for name in one two; do
    cp "$("$root/scripts/build-bundle.sh" "$name")" "$STATE/bundles/$name.tar.gz"
  done
  for name in v0.0.1 v0.0.2; do
    cp "$("$root/scripts/build-bundle.sh" "$name" "$RELEASES")" "$STATE/bundles/"
  done
  # shellcheck source=/dev/null
  . "$root/deploy/xray.pin"
  for name in three four; do
    dir=$STATE/bundles/$name; core=$dir/xray
    mkdir -p "$dir"
    if [ "$name" = three ]; then
      { cat "$root/dist/xray-$XRAY_VERSION/xray"; printf '\nlab core three\n'; } >"$core"
    else
      printf '#!/bin/sh\nexit 1\n' >"$core"
    fi
    chmod 755 "$core"
    sed "s/^XRAY_SHA256=.*/XRAY_SHA256=$(shasum -a 256 "$core" | cut -d' ' -f1)/" "$root/deploy/xray.pin" >"$dir/xray.pin"
    printf '{"Replace":{"%s":"%s"}}' "$root/deploy/xray.pin" "$dir/xray.pin" >"$dir/overlay.json"
    (cd "$root" && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -overlay "$dir/overlay.json" -trimpath -ldflags "-X main.version=$name" -o "$dir/$DAEMON_BIN" ./cmd/$DAEMON_BIN)
    COPYFILE_DISABLE=1 tar --no-xattrs -czf "$STATE/bundles/$name.tar.gz" -C "$dir" "$DAEMON_BIN" xray
  done
  dir=$STATE/bundles/hang
  mkdir -p "$dir/bundle"
  (cd "$root" && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=v0.0.3 -X main.releases=$RELEASES" -o "$dir/$DAEMON_BIN" ./cmd/$DAEMON_BIN)
  printf '#!/bin/sh\n[ "$1" = serve ] && exec sleep 1000\nexec %s "$@"\n' "$LIFECYCLE_WORK/hang/$DAEMON_BIN" >"$dir/bundle/$DAEMON_BIN"
  cp "$root/dist/xray-$XRAY_VERSION/xray" "$dir/bundle/xray"
  chmod 755 "$dir/bundle/"*
  COPYFILE_DISABLE=1 tar --no-xattrs -czf "$STATE/bundles/sidegate-v0.0.3-arm64.tar.gz" -C "$dir/bundle" "$DAEMON_BIN" xray
  dir=$STATE/bundles/three
  (cd "$root" && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -overlay "$dir/overlay.json" -trimpath -ldflags "-X main.version=v0.0.4 -X main.releases=$RELEASES" -o "$dir/$DAEMON_BIN" ./cmd/$DAEMON_BIN)
  COPYFILE_DISABLE=1 tar --no-xattrs -czf "$STATE/bundles/sidegate-v0.0.4-arm64.tar.gz" -C "$dir" "$DAEMON_BIN" xray
}

# lifecycle_copy puts the guest scripts and the bundles lifecycle_bundles built in the VM, each
# bundle beside its own sidegated. release-files.sh writes the releases' files there, since it runs
# the bundle's own sidegated, and the Mac runs no linux/arm64. v0.0.4's takes three's pin.
lifecycle_copy() {
  local name
  sync_guest
  COPYFILE_DISABLE=1 tar --no-xattrs -C "$LAB_DIR" -cf - names.sh unifi-login.sh lifecycle.sh | vm_root tar -C "$GUEST_DIR" -xf -
  for name in one two three four; do
    vm_root sh -c "mkdir -p $LIFECYCLE_WORK/$name && cat >$LIFECYCLE_WORK/$name.tar.gz && tar -C $LIFECYCLE_WORK/$name -xzf $LIFECYCLE_WORK/$name.tar.gz $DAEMON_BIN" <"$STATE/bundles/$name.tar.gz"
  done
  vm_root sh -c "cat >$LIFECYCLE_WORK/xray.pin" <"$LAB_DIR/../deploy/xray.pin"
  vm_root rm -rf "$LIFECYCLE_WORK/repo" "$LIFECYCLE_WORK/repo4" "$LIFECYCLE_WORK/release"
  COPYFILE_DISABLE=1 tar --no-xattrs -C "$STATE/bundles" -cf - "hang/$DAEMON_BIN" | vm_root tar -C "$LIFECYCLE_WORK" -xf -
  COPYFILE_DISABLE=1 tar --no-xattrs -C "$LAB_DIR/.." -cf - scripts/release-files.sh scripts/release-version.sh deploy/xray.pin deploy/install.sh deploy/XRAY-LICENSE \
    -C "$STATE/bundles" sidegate-v0.0.1-arm64.tar.gz sidegate-v0.0.2-arm64.tar.gz sidegate-v0.0.3-arm64.tar.gz sidegate-v0.0.4-arm64.tar.gz |
    vm_root sh -c "mkdir -p $LIFECYCLE_WORK/repo $LIFECYCLE_WORK/release && tar -C $LIFECYCLE_WORK/repo -xf - && cp -R $LIFECYCLE_WORK/repo $LIFECYCLE_WORK/repo4"
  vm_root sh -c "cat >$LIFECYCLE_WORK/repo4/deploy/xray.pin" <"$STATE/bundles/three/xray.pin"
  for name in v0.0.1 v0.0.2 v0.0.3; do
    vm_root sh "$LIFECYCLE_WORK/repo/scripts/release-files.sh" "$name" "$RELEASES" "$LIFECYCLE_WORK/repo/sidegate-$name-arm64.tar.gz" "$LIFECYCLE_WORK/release/$name"
  done
  vm_root sh "$LIFECYCLE_WORK/repo4/scripts/release-files.sh" v0.0.4 "$RELEASES" "$LIFECYCLE_WORK/repo4/sidegate-v0.0.4-arm64.tar.gz" "$LIFECYCLE_WORK/release/v0.0.4"
}

# A subscription's name follows its provider's title, as the page adds it by URL alone.
feed_titles() {
  vm_root sh -c 'echo v2rayNG/ >/etc/unifi-lab/feeds/good.ua; echo Good >/etc/unifi-lab/feeds/good.title
    echo Bad >/etc/unifi-lab/feeds/bad.title; echo Acme >/etc/unifi-lab/feeds/acme.title'
}

# page_lab runs the page lab: the real daemon with the page in it, behind nginx, and the browser.
# $1 is the log, and $2 the browser's scenarios, comma-separated, with empty meaning all. The scenario
# update has a world of its own, after the others', and alone when it is the only one named.
# A failed check sets the caller's rc to 1, and a failed setup ends the run. Call it bare, since under
# || or if bash turns errexit off for the whole function.
page_lab() {
  local log=$1 only=$2 root port shots
  root=$(cd "$LAB_DIR/.." && pwd)
  port=${PAGE_PORT:-5465}
  shots=$STATE/page
  mkdir -p "$STATE" "$shots"
  inner() { echo "--- $1 $(date +%T)" | tee -a "$log"; vm_root "$GUEST_DIR/$2" "${@:3}" 2>&1 | tee -a "$log" || rc=1; }
  [ "$only" = update ] || page_world
  case ",$only," in ,, | *,update,*) page_update ;; esac
}

# browser runs the page's scenarios against nginx in the VM, in $1's world.
browser() {
  local tunnel
  ssh -F "$HOME/.lima/$VM/ssh.config" -N -o ControlMaster=no -o ControlPath=none -o ExitOnForwardFailure=yes -L "$port:127.0.0.1:8080" "lima-$VM" &
  tunnel=$!
  sleep 2
  echo "--- browser, $1 $(date +%T)" | tee -a "$log"
  (cd "$root/web" && LAB_WORLD="$1" LAB_VM="$VM" LAB_FEEDS="$FEEDS" LAB_PAGE_URL="http://127.0.0.1:$port$BASE_PATH" LAB_SHOTS="$shots" LAB_ONLY="$only" GUEST_DIR="$GUEST_DIR" LAB_CORE_UNIT="$CORE_UNIT" node scripts/lab-page.mjs) 2>&1 | tee -a "$log" || rc=1
  kill "$tunnel" 2>/dev/null || true
}

page_world() {
  page_install
  inner setup control.sh setup
  feed_titles
  inner nginx page.sh nginx
  inner unifi page.sh unifi
  inner daemon page.sh daemon
  inner auth page.sh auth
  inner cache page.sh cache
  inner csrf page.sh csrf
  inner paste page.sh paste
  browser page
  inner status page.sh status
  inner secrets page.sh secrets
  inner teardown page.sh teardown
  inner control-teardown control.sh teardown
}

# page_update is the lifecycle suite's gateway, v0.0.1 installed by the one command and a slot that
# serves, with v0.0.2 published: the browser updates it through the page.
page_update() {
  lifecycle_bundles
  lifecycle_copy
  inner prepare lifecycle.sh prepare
  inner snapshot lifecycle.sh snapshot pre
  inner update-install lifecycle.sh update install
  inner configure lifecycle.sh configure
  inner connect lifecycle.sh connect
  inner offer lifecycle.sh offer
  browser update
  inner connect-again lifecycle.sh connect
  inner update-uninstall lifecycle.sh update uninstall
  inner lifecycle-teardown lifecycle.sh teardown
}
