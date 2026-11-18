#!/bin/bash
. "${ENGINEX_MIGRATION:-$(git rev-parse --show-toplevel)/rootfs-overlay/usr/libexec/enginex-migration}"
ENGINEX_LAB=${ENGINEX_LAB:-$legacy_lab}
# Control Center fit: the shipped 5.3.0 ControlCenterTab.qml laid out at each model's real panel size, on that model's own
# Engine and Config (product code), natively on the build host. No qemu, host services or deck connections.
# bash tools/az01-packer/cc-fit-sandbox.sh <new-private-dir> [BOARD ...]     (default: all 16 models)
# CROSS_FADER_TEST=1 bash tools/az01-packer/cc-fit-sandbox.sh <new-private-dir> JP11S [other software-switch boards...]
# Panel: every model's device tree (Gen-2, AZ04) or stock splash (Gen-1) is an 800 x 1280 portrait panel shown rotated, 1280 x 800.
set -euo pipefail
REPO=$(cd "$(dirname "$0")/../.." && pwd)
W=$(realpath -m "${1:?private sandbox directory required}")
LAB=${ENGINEX_LAB}
PANEL=${CC_FIT_PANEL:-1280x800}
# board -> kind and lab image (AZ04 uses the lab's unpacked rootfs)
image_of() {
    case "$1" in
        JC11|JC16|NH08) echo gen1 fw-5.3.0/MIXSTREAMPRO ;;
        JP07|JP08|JP11) echo gen1 fw-5.3.0/SC5000 ;;
        JP13|JP14) echo gen1 fw-5.3.0/SC6000 ;;
        JP20|JP21|JC11S|JP11S) echo gen2 fw-5.3.0/SCLIVE4 ;;
        NH08S|NH10) echo gen2 fw-5.3.0-gen2/MIXSTREAMPROGO ;;
        JP24|RMZ2) echo az04 systemone-analysis ;;
        *) echo "unknown board $1" >&2; return 1 ;;
    esac
}
if [ "${2:-}" != --inner ]; then
    [ "$(uname -m)" = aarch64 ] || { echo 'native ARM build host (aarch64) required' >&2; exit 1; }
    [ ! -e "$W" ] || { echo "refusing existing sandbox $W" >&2; exit 1; }
    mkdir -p "$W"
    boards=("${@:2}")
    if [ "${CROSS_FADER_TEST:-0}" = 1 ]; then
        [ ${#boards[@]} -gt 0 ] || boards=(JC11 JC16 NH08 JP11 JP20 JP21 JC11S JP11S NH08S NH10)
    fi
    [ ${#boards[@]} -gt 0 ] || boards=(JC11 JC16 NH08 JP07 JP08 JP11 JP13 JP14 JP20 JP21 JC11S JP11S NH08S NH10 JP24 RMZ2)
    for board in "${boards[@]}"; do
        read -r kind image < <(image_of "$board")
        dir=$W/$(basename "$image")
        [ ! -e "$dir" ] || continue
        case "$kind" in
            gen1) python3 "$REPO/tools/az01-packer/az01_packer.py" unpack "$LAB/$image-5.3.0-Update.img" --output "$dir" > "$dir.log" ;;
            gen2) python3 - "$REPO" "$LAB/$image-5.3.0-Update.img" "$dir" <<'PY'
import gzip, importlib.util, lzma, sys
from pathlib import Path
repo, image, out = map(Path, sys.argv[1:])
spec = importlib.util.spec_from_file_location('az0x', repo/'tools/az01-packer/az0x_packer.py')
az0x = importlib.util.module_from_spec(spec); spec.loader.exec_module(az0x)
b = image.read_bytes(); r = next(r for r in az0x.parse_header(b)['records'] if r['name'] == 'rootfs')
payload = b[r['offset']:r['offset']+r['size']]
out.mkdir()
(out/'rootfs.ext2').write_bytes((lzma.decompress if payload.startswith(b'\xfd7zXZ\0') else gzip.decompress)(payload))
PY
            ;;
        esac
    done
    python3 "$REPO/tools/ui-mod/build_packs.py" --qrc "$LAB/5.3.0/qrc" --out "$W/packs" > "$W/packs.log"
    # The 32-bit Gen-2 Engine exits with "TestApp" without a deck's SoC identity: a sandbox-only stub passes that one check.
    arm-linux-gnueabihf-gcc -O2 -Wall -Wextra -Werror -shared -fPIC -o "$W/identity.so" "$REPO/tools/az01-packer/cc-fit-identity.c"
    if [ "${CROSS_FADER_TEST:-0}" != 1 ]; then
        (cd "$REPO/tools/ui-mod" && CROSS= OUT="$W/hook-az04.so" sh build.sh)
    fi
    fail=0
    for board in "${boards[@]}"; do
        unshare -m -n -p -f --kill-child bash "$0" "$W" --inner "$board" || fail=1
    done
    exit $fail
fi
board=${3:?board required}
read -r kind image < <(image_of "$board")
R=$W/$board-root
lower=$(echo "$board" | tr A-Z a-z)
mount --make-rprivate /
ip link set lo up
mkdir -p "$R"
if [ "$kind" = az04 ]; then
    mount --bind "$LAB/$image/rootfs" "$R"; mount -o remount,ro,bind "$R"
else
    mount -o loop "$W/$(basename "$image")/rootfs.ext2" "$R"
fi
for d in tmp run dev data sys; do mount -t tmpfs tmpfs "$R/$d"; done
mkdir -p "$R/dev/shm" "$R/dev/pts" "$R/sys/firmware/devicetree/base" "$R/sys/devices/system/cpu" "$R/run/dbus" "$R/tmp/home" "$R/data/enginex/ui/packs"
[ "$kind" = az04 ] || chroot "$R" /bin/mkdir -p /var/volatile/tmp /var/volatile/log /var/volatile/cache
for f in online possible present; do echo 0-3 > "$R/sys/devices/system/cpu/$f"; done
for n in null zero full random urandom; do touch "$R/dev/$n"; mount --bind "/dev/$n" "$R/dev/$n"; done
printf '%s\0' "$board" > "$R/sys/firmware/devicetree/base/inmusic,product-code"
case "$kind" in
    gen1) compat="inmusic,$lower\0rockchip,rk3288" ;;
    gen2) second=az05; [ "$board" = JC11S ] && second=az01; compat="inmusic,$lower\0inmusic,$second\0rockchip,rk3288" ;;
    az04) compat="inmusic,$lower\0rockchip,rk3588" ;;
esac
printf "$compat\0" > "$R/sys/firmware/devicetree/base/compatible"
mount -t proc -o ro proc "$R/proc"
# Engine requires the deck's serial IRQ. Its affinity writes stay in tmpfs.
printf '           CPU0\n 41:        100     GICv2  32 Level     ttyS0\n' > "$W/$board-interrupts"
mount --bind "$W/$board-interrupts" "$R/proc/interrupts"
mount -t tmpfs tmpfs "$R/proc/irq"
mkdir -p "$R/proc/irq/41"
printf 'f\n' > "$R/proc/irq/41/smp_affinity"
printf '0-3\n' > "$R/proc/irq/41/smp_affinity_list"
# The hook only starts when LD_PRELOAD names libengine-ui-hook.so: without it Config is stock, not what a deck runs.
hook=$REPO/rootfs-overlay/etc/enginex/libengine-ui-hook.so; preload=/tmp/libengine-ui-hook.so
[ "$kind" = az04 ] && hook=$W/hook-az04.so
cp "$hook" "$R/tmp/libengine-ui-hook.so"
[ "$kind" = gen2 ] && { cp "$W/identity.so" "$R/tmp/identity.so"; preload=/tmp/identity.so:/tmp/libengine-ui-hook.so; }
cp -a "$W/packs/en" "$R/data/enginex/ui/packs/"
mkdir -p "$R/data/enginex/ui/packs/en/Skins/Common/Items"
sed "s/@BOARD@/$board/; s/@WIDTH@/${PANEL%x*}/; s/@HEIGHT@/${PANEL#*x}/" "$REPO/tools/az01-packer/cc-fit-probe.qml" > "$R/data/enginex/ui/packs/en/Skins/Common/Items/QmlWarnings.qml"
printf 'lang=en\n' > "$R/data/enginex/ui/settings.conf"
if [ "${CROSS_FADER_TEST:-0}" = 1 ]; then
    cp "$REPO/tools/az01-packer/crossfader-reverse-probe.qml" "$R/data/enginex/ui/packs/en/Skins/Common/Items/QmlWarnings.qml"
    triplet=arm-linux-gnueabihf; loader=ld-linux-armhf.so.3
    for path in /dev/dri /sys/class /sys/devices /sys/bus /usr/lib/"$triplet" /usr/share/glvnd /usr/share/drirc.d; do
        mkdir -p "$R$path"
        mount --bind "$path" "$R$path"; mount -o remount,bind,ro "$R$path"
    done
    printf '{"device":"/dev/dri/card0","headless":"1280x800","hwcursor":false}\n' > "$W/$board-screen.json"
    for c in Default "$board"; do
        cfg="$R/usr/Engine/ScreenConfiguration/$c/ScreenConfiguration.json"
        if [ -f "$cfg" ]; then mount --bind "$W/$board-screen.json" "$cfg"; fi
    done
    chroot "$R" /usr/bin/dbus-daemon --system --fork --nopidfile > "$W/$board-dbus.log" 2>&1 || true
    # Engine locates QML plugins relative to /proc/self/exe. Exec it normally with Mesa's matching loader.
    mount --bind "/usr/lib/$triplet/$loader" "$R/lib/$loader"
    engine_rc=0
    timeout --kill-after=5s 60s chroot "$R" "/usr/lib/$triplet/$loader" --library-path "/usr/lib/$triplet" \
        /usr/bin/env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin HOME=/tmp/home \
        QT_QPA_PLATFORM=eglfs QT_QPA_EGLFS_INTEGRATION=eglfs_kms LIBGL_ALWAYS_SOFTWARE=1 GALLIUM_DRIVER=llvmpipe \
        LP_NUM_THREADS=2 QML_DISABLE_DISK_CACHE=1 MALLOC_ARENA_MAX=1 LD_LIBRARY_PATH="/usr/lib/$triplet:/usr/lib:/usr/qt/lib" \
        LD_PRELOAD="$preload" /usr/Engine/Engine -d0 > "$W/$board-engine.log" 2>&1 || engine_rc=$?
    if ! grep -aq "X303 PASS|$board" "$W/$board-engine.log"; then
        cp "$R/tmp/engine-quit-reason" "$W/$board-quit-reason" 2>/dev/null || true
        echo "$board: crossfader test failed (Engine exit $engine_rc)" >&2
        exit 1
    fi
    grep -a "X303 " "$W/$board-engine.log" | cut -c1-150
    cp "$R/tmp/crossfader-reverse.png" "$W/$board-reverse.png"
    exit
fi
chroot "$R" /usr/bin/dbus-daemon --system --fork --nopidfile > "$W/$board-dbus.log" 2>&1 || true
engine_rc=0
timeout --kill-after=5s 60s chroot "$R" /usr/bin/env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin HOME=/tmp/home LD_LIBRARY_PATH=/usr/qt/lib \
    LD_PRELOAD=$preload QT_QPA_PLATFORM=offscreen QML_DISABLE_DISK_CACHE=1 MALLOC_ARENA_MAX=1 /usr/Engine/Engine -d0 > "$W/$board-engine.log" 2>&1 || engine_rc=$?
echo "X166 ENGINE_EXIT|$engine_rc|" >> "$W/$board-engine.log"
cp "$R/tmp/mx-cc-fit.png" "$W/$board-cc.png" 2>/dev/null || true
cp "$R/tmp/engine-quit-reason" "$W/$board-quit-reason" 2>/dev/null || true
grep -aq "engine-ui-hook: Engine " "$W/$board-engine.log" || { echo "$board: the hook did not start" >&2; exit 1; }
grep -aE 'X166 (CCFIT|CCGRID|CCOVER|ERROR|DONE)' "$W/$board-engine.log" | sed 's/ *\[20[0-9T:.Z-]*\].*//' | cut -c1-260 | head -n 8
if grep -aq 'X166 ERROR|' "$W/$board-engine.log" || ! grep -aq 'X166 DONE|PASS|' "$W/$board-engine.log"; then
    echo "$board CC_FIT_FAIL"
    exit 1
fi
echo "$board CC_FIT_PASS"
