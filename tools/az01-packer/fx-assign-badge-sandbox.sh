#!/bin/bash
. "${ENGINEX_MIGRATION:-$(git rev-parse --show-toplevel)/rootfs-overlay/usr/libexec/enginex-migration}"
ENGINEX_LAB=${ENGINEX_LAB:-$legacy_lab}
# FX assign badge (#276): the pack's FX bar (MainView/Fx/DJFx.qml) on a first-generation model's own Engine and UI hook, natively on
# the build host (no qemu, host services or deck connections). The probe creates the shipped file from the pack (badge hidden off the
# SC LIVE boards) and a copy whose board table maps this board to the SC LIVE 2 targets, then sets the same
# /Engine/Mixer/<target>/DJFx/Assigned properties the stock JP20/JP21 FX selector writes and checks the badge text and screenshots.
# The SC LIVE boards themselves run the second-generation Engine, which needs a hardware-identity fixture; the QML is the same qrc.
# Offscreen, so it runs beside other sandboxes: enginex-run sandbox -- unshare -m --propagation private \
#     bash tools/az01-packer/fx-assign-badge-sandbox.sh <new-private-dir> [BOARD ...]   (default: JP11, the PRIME GO, one FX unit)
set -euo pipefail
REPO=$(cd "$(dirname "$0")/../.." && pwd)
W=$(realpath -m "${1:?private sandbox directory required}")
LAB=${ENGINEX_LAB}
image_of() {
    case "$1" in
        JP11) echo fw-5.3.0/PRIMEGO ;;
        *) echo "unsupported board $1" >&2; return 1 ;;
    esac
}
if [ "${2:-}" != --inner ]; then
    [ "$(uname -m)" = aarch64 ] || { echo 'native ARM build host (aarch64) required' >&2; exit 1; }
    [ ! -e "$W" ] || { echo "refusing existing sandbox $W" >&2; exit 1; }
    mkdir -p "$W"
    boards=("${@:2}"); [ ${#boards[@]} -gt 0 ] || boards=(JP11)
    for board in "${boards[@]}"; do
        image=$(image_of "$board")
        dir=$W/$(basename "$image")
        [ -e "$dir" ] || python3 "$REPO/tools/az01-packer/az01_packer.py" unpack "$LAB/$image-5.3.0-Update.img" --output "$dir" > "$dir.log"
    done
    python3 "$REPO/tools/ui-mod/build_packs.py" --qrc "$LAB/5.3.0/qrc" --out "$W/packs" > "$W/packs.log"
    # Engine only starts its audio side with a card: the shadow-twin's sandbox-only ALSA null card on a monotonic clock.
    arm-linux-gnueabihf-gcc -O2 -shared -fPIC -Wall -Wextra -o "$W/alsa-null-fixture.so" "$REPO/tools/shadow-twin/alsa_null_fixture.c" -ldl
    fail=0
    for board in "${boards[@]}"; do
        unshare -m -n -p -f --kill-child bash "$0" "$W" --inner "$board" || fail=1
    done
    exit $fail
fi
board=${3:?board required}
image=$(image_of "$board")
R=$W/$board-root
lower=$(echo "$board" | tr A-Z a-z)
mount --make-rprivate /
ip link set lo up
mkdir -p "$R"
mount -o loop "$W/$(basename "$image")/rootfs.ext2" "$R"
for d in tmp run dev data sys; do mount -t tmpfs tmpfs "$R/$d"; done
mkdir -p "$R/dev/shm" "$R/dev/pts" "$R/sys/firmware/devicetree/base" "$R/sys/devices/system/cpu" "$R/run/dbus" "$R/tmp/home" "$R/data/enginex/ui/packs" "$R/data/inMusicTempFiles" \
    "$R/data/AppDataUser/AIR Music Technology/Engine/State"
chroot "$R" /bin/mkdir -p /var/volatile/tmp /var/volatile/log /var/volatile/cache
for f in online possible present; do echo 0-3 > "$R/sys/devices/system/cpu/$f"; done
# Offscreen is enough: the badge is text and rectangles (no ColoredImage icons), and it needs no GPU slot.
platform=offscreen
# The PRIME GO's Engine reads its mains and battery state and powers off ("Poweroff" quit reason) when the files are
# missing: give it mains power and a full battery inside the sandbox's own /sys.
mkdir -p "$R/sys/class/power_supply/az01-ac-power" "$R/sys/class/power_supply/sbs-3-000b"
echo 1 > "$R/sys/class/power_supply/az01-ac-power/online"
echo 1 > "$R/sys/class/power_supply/sbs-3-000b/present"
echo 100 > "$R/sys/class/power_supply/sbs-3-000b/capacity"
for n in null zero full random urandom; do touch "$R/dev/$n"; mount --bind "/dev/$n" "$R/dev/$n"; done
printf '%s\0' "$board" > "$R/sys/firmware/devicetree/base/inmusic,product-code"
printf "inmusic,$lower\0rockchip,rk3288\0" > "$R/sys/firmware/devicetree/base/compatible"
mount -t proc -o ro proc "$R/proc"
printf '           CPU0\n 41:        100     GICv2  32 Level     ttyS0\n' > "$W/$board-interrupts"
mount --bind "$W/$board-interrupts" "$R/proc/interrupts"
mount -t tmpfs tmpfs "$R/proc/irq"
mkdir -p "$R/proc/irq/41"
printf 'f\n' > "$R/proc/irq/41/smp_affinity"
printf '0-3\n' > "$R/proc/irq/41/smp_affinity_list"
# The hook only starts when LD_PRELOAD names libengine-ui-hook.so.
cp "$REPO/rootfs-overlay/etc/enginex/libengine-ui-hook.so" "$R/tmp/libengine-ui-hook.so"
cp "$W/alsa-null-fixture.so" "$R/tmp/alsa-null-fixture.so"
cp -a "$W/packs/en" "$R/data/enginex/ui/packs/"
mkdir -p "$R/data/enginex/ui/packs/en/Skins/Common/Items"
sed "s/@BOARD@/$board/" "$REPO/tools/az01-packer/fx-assign-badge-probe.qml" > "$R/data/enginex/ui/packs/en/Skins/Common/Items/QmlWarnings.qml"
printf 'lang=en\n' > "$R/data/enginex/ui/settings.conf"
chroot "$R" /usr/bin/dbus-daemon --system --fork --nopidfile > "$W/$board-dbus.log" 2>&1 || true
engine_rc=0
# The probe is done about 25 s after start; on the PRIME GO /Client/Quit becomes a power-off that never returns in the
# sandbox, so the timeout ends the run (exit 124 with quit reason Poweroff is the normal end here).
timeout --kill-after=5s "${FX_ASSIGN_TIMEOUT:-75}s" chroot "$R" /usr/bin/env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin HOME=/tmp/home LD_LIBRARY_PATH=/usr/qt/lib \
    LD_PRELOAD=/tmp/alsa-null-fixture.so:/tmp/libengine-ui-hook.so SBX_CARD=$board SBX_OUT=4 SBX_IN=2 SBX_CLOCK_HZ=48000 \
    QT_QPA_PLATFORM=$platform \
    QML_DISABLE_DISK_CACHE=1 QML_XHR_ALLOW_FILE_READ=1 MALLOC_ARENA_MAX=1 /usr/Engine/Engine -d0 > "$W/$board-engine.log" 2>&1 || engine_rc=$?
echo "X276 ENGINE_EXIT|$engine_rc|$platform|" >> "$W/$board-engine.log"
cp "$R"/tmp/fx-assign-*.png "$W/" 2>/dev/null || true
cp "$R/tmp/engine-quit-reason" "$W/$board-quit-reason" 2>/dev/null && echo "X276 QUIT_REASON|$(head -c 200 "$W/$board-quit-reason" | tr -c '[:print:]' ' ')|" >> "$W/$board-engine.log" || true
grep -aq "engine-ui-hook: Engine " "$W/$board-engine.log" || { echo "$board: the hook did not start" >&2; exit 1; }
grep -aE 'X276 (BADGE|ERROR|DONE|ENGINE_EXIT|QUIT_REASON)' "$W/$board-engine.log" | sed 's/ *\[20[0-9T:.Z-]*\].*//; s/^\[W\] //' | cut -c1-200
# Any warning the Engine prints against the FX bar file fails the run, whatever the probe saw.
if grep -aE 'MainView/Fx/DJFx\.qml:[0-9]+' "$W/$board-engine.log" | grep -av 'X276' | sed -n '1,10p' | grep -q .; then
    grep -aE 'MainView/Fx/DJFx\.qml:[0-9]+' "$W/$board-engine.log" | cut -c1-200 | sed -n '1,10p'
    echo "$board FX_ASSIGN_FAIL (QML warnings in DJFx.qml)"
    exit 1
fi
if grep -aq 'X276 ERROR|' "$W/$board-engine.log" || ! grep -aq 'X276 DONE|PASS|' "$W/$board-engine.log"; then
    echo "$board FX_ASSIGN_FAIL"
    exit 1
fi
echo "$board FX_ASSIGN_PASS"
