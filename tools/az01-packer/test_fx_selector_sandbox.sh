#!/bin/bash
# Batam only. Private JC11 rootfs, native armhf; no hardware or shared lab writes.
set -euo pipefail
REPO=$(cd "$(dirname "$0")/../.." && pwd)
W=$REPO/out/fx-selector-native
if [ "${1:-}" != --inner ]; then
    test "$(uname -m)" = aarch64
    mkdir -p "$W"
    exec 9> "$W/sandbox.lock"
    flock -n 9 || { echo "FX sandbox already in use" >&2; exit 1; }
    CGO_ENABLED=0 go build -o "$W/ui-server" "$REPO/tools/az01-packer/fx_selector_server.go"
    arm-linux-gnueabihf-gcc -O2 -shared -fPIC -Wall -Wextra -Werror -Wl,-z,max-page-size=4096 \
        "$REPO/tools/az01-packer/test_fx_selector_native.c" -ldl -lpthread -lm -o "$W/libfx-native-test.so"
    if [ ! -e "$W/image/rootfs.ext2" ]; then
        python3 tools/az01-packer/az01_packer.py unpack /root/build/sc5000-lab/fw-5.3.0/PRIME4-5.3.0-Update.img --output "$W/image" > "$W/unpack.json"
    fi
    python3 tools/ui-mod/build_packs.py --qrc /root/build/sc5000-lab/5.3.0/qrc --out "$W/packs" > "$W/packs.log"
    unshare -m -n -p -f --kill-child bash "$0" --inner
    exit
fi
mount --make-rprivate /
ip link set lo up
R=$W/root
mkdir -p "$R"
mount -o loop "$W/image/rootfs.ext2" "$R"
for d in tmp run dev data sys; do mount -t tmpfs tmpfs "$R/$d"; done
mkdir -p "$R/dev/shm" "$R/dev/pts"
for n in null zero full random urandom; do touch "$R/dev/$n"; mount --bind "/dev/$n" "$R/dev/$n"; done
for d in class devices bus; do
    mkdir -p "$R/sys/$d"
    mount --bind "/sys/$d" "$R/sys/$d"
    mount -o remount,bind,ro "$R/sys/$d"
done
mkdir -p "$R/sys/firmware/devicetree/base"
printf JC11 > "$R/sys/firmware/devicetree/base/inmusic,product-code"
printf 'inmusic,jc11\0rockchip,rk3288\0' > "$R/sys/firmware/devicetree/base/compatible"
mount -t proc proc "$R/proc"
printf '           CPU0\n 41:        100     GICv2  32 Level     ttyS0\n' > "$W/interrupts"
mount --bind "$W/interrupts" "$R/proc/interrupts"
mount -t tmpfs tmpfs "$R/proc/irq"
mkdir -p "$R/proc/irq/41"
printf 'f\n' > "$R/proc/irq/41/smp_affinity"
printf '0-3\n' > "$R/proc/irq/41/smp_affinity_list"
mkdir -p "$R/etc/sc5000" "$R/tmp/home" "$R/run/dbus" "$R/data/sc5000-custom/ui/packs"
mkdir -p "$R/tmp/fx-qml-imports"
cp -a /root/build/sc5000-lab/5.3.0/qrc/airAssignments/InputAssignment /root/build/sc5000-lab/5.3.0/qrc/airAssignments/OutputAssignment "$R/tmp/fx-qml-imports/"
cp "${FX_HOOK:-$REPO/rootfs-overlay/etc/sc5000/libengine-ui-hook.so}" "$R/etc/sc5000/libengine-ui-hook.so"
cp "$W/libfx-native-test.so" "$R/tmp/libfx-native-test.so"
cp -a "$W/packs/en" "$R/data/sc5000-custom/ui/packs/"
mkdir -p "$R/data/sc5000-custom/ui/packs/en/Skins/Common/Items"
cp "$REPO/tools/az01-packer/fx-selector-probe.qml" "$R/data/sc5000-custom/ui/packs/en/Skins/Common/Items/QmlWarnings.qml"
printf 'lang=en\n' > "$R/data/sc5000-custom/ui/settings.conf"
chroot "$R" /usr/bin/dbus-daemon --system --fork --nopidfile > "$W/dbus.log" 2>&1 || true
"$W/ui-server" "$R/data/sc5000-custom/ui" "$R/tmp/fx-state" > "$W/http.log" 2>&1 &
server=$!
trap 'kill "$server" 2>/dev/null || true' EXIT
set +e
timeout --kill-after=5s 40s chroot "$R" /usr/bin/env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin HOME=/tmp/home LD_LIBRARY_PATH=/usr/qt/lib \
    LD_PRELOAD=/etc/sc5000/libengine-ui-hook.so:/tmp/libfx-native-test.so QT_QPA_PLATFORM=offscreen \
    QML_IMPORT_PATH=/tmp/fx-qml-imports QML2_IMPORT_PATH=/tmp/fx-qml-imports \
    QML_XHR_ALLOW_FILE_READ=1 \
    QML_DISABLE_DISK_CACHE=1 MALLOC_ARENA_MAX=1 /usr/Engine/Engine -d0 > "$W/engine.log" 2>&1
engine_rc=$?
set -e
printf '%s\n' "$engine_rc" > "$W/engine-exit.txt"
case "$engine_rc" in 0|124|137) ;; *) echo "Engine exited $engine_rc" >&2; exit 1 ;; esac
grep -a -m 40 -E 'FXPROBE|FXNATIVE|engine-ui-hook: PX' "$W/engine.log" | cut -c1-200 | head -n 40
grep -aq 'FXPROBE done|PASS' "$W/engine.log"
grep -aq 'FXNATIVE offline-driver|PASS' "$W/engine.log"
! grep -aEq 'FXPROBE .*\|FAIL|FXNATIVE .*FAIL|Type [A-Za-z_]+ unavailable|is not a type|ReferenceError|TypeError|Cannot assign to non-existent' "$W/engine.log"
