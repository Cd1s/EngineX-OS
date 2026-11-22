#!/bin/bash
. "${ENGINEX_MIGRATION:-$(git rev-parse --show-toplevel)/rootfs-overlay/usr/libexec/enginex-migration}"
ENGINEX_LAB=${ENGINEX_LAB:-$legacy_lab}
# Streaming provider mark in the browse view (#305, 5.3.0.6): the shipped track-row mark (BrowseView/Components/Components/ProviderIcon.qml)
# (and the rekordbox import banner's counts, scFormatCount) on a model's own Engine and UI hook, natively on the build host (no qemu, host
# services or deck connections), with real OpenGL (eglfs on the host's DRM device and Mesa: offscreen leaves ColoredImage icons blank).
# This Engine has no streaming row, so the probe models rows of Engine's TIDAL slot (providerIconUri, and the gateway's provider label);
# the file, its context and its code are the shipped ones. Gen-1 (armhf), Gen-2 (armhf, SC LIVE 4's Engine) and AZ04 (64-bit) boards.
# bash tools/az01-packer/browse-icon-sandbox.sh <new-private-dir> after|before [BOARD ...]     (default: JP07 JP13 JC11 NH08 JP21 NH08S JP24)
# "before" builds the pack with the script named by BROWSE_ICON_BUILDER (the base commit's build_packs.py, copied next to the current one).
set -euo pipefail
REPO=$(cd "$(dirname "$0")/../.." && pwd)
W=$(realpath -m "${1:?private sandbox directory required}")
LAB=${ENGINEX_LAB}
# board -> kind and lab image (AZ04 uses the lab's unpacked rootfs)
image_of() {
    case "$1" in
        JC11|JC16|NH08) echo gen1 fw-5.3.0/MIXSTREAMPRO ;;
        JP07|JP08|JP11) echo gen1 fw-5.3.0/SC5000 ;;
        JP13|JP14) echo gen1 fw-5.3.0/SC6000 ;;
        JP20|JP21|JC11S|JP11S) echo gen2 fw-5.3.0/SCLIVE4 ;;
        NH08S|NH10) echo gen2 fw-5.3.0-gen2/MIXSTREAMPROGO ;;
        JP24|RMZ2) echo az04 systemone-analysis ;;
        *) echo "unsupported board $1" >&2; return 1 ;;
    esac
}
expect=${2:?after or before required}
case "$expect" in after|before) ;; *) echo "second argument must be after or before" >&2; exit 2 ;; esac
if [ "${3:-}" != --inner ]; then
    [ "$(uname -m)" = aarch64 ] || { echo 'native ARM build host (aarch64) required' >&2; exit 1; }
    [ ! -e "$W" ] || { echo "refusing existing sandbox $W" >&2; exit 1; }
    mkdir -p "$W"
    boards=("${@:3}"); [ ${#boards[@]} -gt 0 ] || boards=(JP07 JP13 JC11 NH08 JP21 NH08S JP24)
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
    python3 "${BROWSE_ICON_BUILDER:-$REPO/tools/ui-mod/build_packs.py}" --qrc "$LAB/5.3.0/qrc" --out "$W/packs" > "$W/packs.log"
    # Engine only loads and plays tracks with an audio card: the shadow-twin's sandbox-only ALSA null card on a monotonic clock.
    arm-linux-gnueabihf-gcc -O2 -shared -fPIC -Wall -Wextra -o "$W/alsa-null-fixture.so" "$REPO/tools/shadow-twin/alsa_null_fixture.c" -ldl
    # The 32-bit Gen-2 Engine exits with "TestApp" without a deck's SoC identity: a sandbox-only stub passes that one check.
    arm-linux-gnueabihf-gcc -O2 -Wall -Wextra -Werror -shared -fPIC -o "$W/identity.so" "$REPO/tools/az01-packer/cc-fit-identity.c"
    (cd "$REPO/tools/ui-mod" && CROSS= OUT="$W/hook-az04.so" sh build.sh)
    fail=0
    for board in "${boards[@]}"; do
        unshare -m -n -p -f --kill-child bash "$0" "$W" "$expect" --inner "$board" || fail=1
    done
    exit $fail
fi
board=${4:?board required}
read -r kind image < <(image_of "$board")
R=$W/$board-root
triplet=arm-linux-gnueabihf; loader=ld-linux-armhf.so.3
[ "$kind" != az04 ] || { triplet=aarch64-linux-gnu; loader=ld-linux-aarch64.so.1; }
lower=$(echo "$board" | tr A-Z a-z)
mount --make-rprivate /
ip link set lo up
mkdir -p "$R"
if [ "$kind" = az04 ]; then
    # The lab's rootfs is read-only: an overlay with a private upper layer takes the mount points the host's Mesa needs.
    mkdir -p "$W/$board-upper" "$W/$board-work"
    mount -t overlay overlay -o "lowerdir=$LAB/$image/rootfs,upperdir=$W/$board-upper,workdir=$W/$board-work" "$R"
else
    mount -o loop "$W/$(basename "$image")/rootfs.ext2" "$R"
fi
for d in tmp run dev data sys; do mount -t tmpfs tmpfs "$R/$d"; done
mkdir -p "$R/dev/shm" "$R/dev/pts" "$R/sys/firmware/devicetree/base" "$R/sys/devices/system/cpu" "$R/run/dbus" "$R/tmp/home" "$R/data/enginex/ui/packs" "$R/data/inMusicTempFiles" "$R/data/AppDataUser/AIR Music Technology/Engine/State"
[ "$kind" = az04 ] || chroot "$R" /bin/mkdir -p /var/volatile/tmp /var/volatile/log /var/volatile/cache
for f in online possible present; do echo 0-3 > "$R/sys/devices/system/cpu/$f"; done
# Real OpenGL like the shadow-twin: the host's DRM device with a headless 1280 x 800 screen and the host's Mesa (llvmpipe) of the model's architecture.
mkdir -p "$R/dev/dri"
mount --bind /dev/dri "$R/dev/dri"
for d in class devices bus; do
    mkdir -p "$R/sys/$d"
    mount --bind "/sys/$d" "$R/sys/$d"
    mount -o remount,bind,ro "$R/sys/$d"
done
mkdir -p "$R/usr/lib/$triplet"
mount --bind "/usr/lib/$triplet" "$R/usr/lib/$triplet"
mount -o remount,bind,ro "$R/usr/lib/$triplet"
mkdir -p "$R/usr/share/glvnd" "$R/usr/share/drirc.d"
for d in glvnd drirc.d; do [ -d /usr/share/$d ] && mount --bind /usr/share/$d "$R/usr/share/$d" && mount -o remount,bind,ro "$R/usr/share/$d"; done
mount --bind "/usr/lib/$triplet/$loader" "$R/lib/$loader"
printf '{"device": "/dev/dri/card0", "headless": "1280x800", "hwcursor": false}\n' > "$W/$board-screen.json"
for c in Default "$board"; do [ -f "$R/usr/Engine/ScreenConfiguration/$c/ScreenConfiguration.json" ] && mount --bind "$W/$board-screen.json" "$R/usr/Engine/ScreenConfiguration/$c/ScreenConfiguration.json"; done
for n in null zero full random urandom; do touch "$R/dev/$n"; mount --bind "/dev/$n" "$R/dev/$n"; done
printf '%s\0' "$board" > "$R/sys/firmware/devicetree/base/inmusic,product-code"
case "$kind" in
    gen1) compat="inmusic,$lower\0rockchip,rk3288" ;;
    gen2) second=az05; [ "$board" = JC11S ] && second=az01; compat="inmusic,$lower\0inmusic,$second\0rockchip,rk3288" ;;
    az04) compat="inmusic,$lower\0rockchip,rk3588" ;;
esac
printf "$compat\0" > "$R/sys/firmware/devicetree/base/compatible"
mount -t proc -o ro proc "$R/proc"
printf '           CPU0\n 41:        100     GICv2  32 Level     ttyS0\n' > "$W/$board-interrupts"
mount --bind "$W/$board-interrupts" "$R/proc/interrupts"
mount -t tmpfs tmpfs "$R/proc/irq"
mkdir -p "$R/proc/irq/41"
printf 'f\n' > "$R/proc/irq/41/smp_affinity"
printf '0-3\n' > "$R/proc/irq/41/smp_affinity_list"
# The hook only starts when LD_PRELOAD names libengine-ui-hook.so.
hook=$REPO/rootfs-overlay/etc/enginex/libengine-ui-hook.so; preload=/tmp/alsa-null-fixture.so:/tmp/libengine-ui-hook.so
[ "$kind" != az04 ] || { hook=$W/hook-az04.so; preload=/tmp/libengine-ui-hook.so; } # the null ALSA card is 32-bit
cp "$hook" "$R/tmp/libengine-ui-hook.so"
cp "$W/alsa-null-fixture.so" "$R/tmp/alsa-null-fixture.so"
[ "$kind" != gen2 ] || { cp "$W/identity.so" "$R/tmp/identity.so"; preload=/tmp/identity.so:$preload; }
mkdir -p "$R/data/enginex/ui/icons"
cp "$REPO"/tools/ui-mod/icons/*.png "$R/data/enginex/ui/icons/"
cp -a "$W/packs/en" "$R/data/enginex/ui/packs/"
mkdir -p "$R/data/enginex/ui/packs/en/Skins/Common/Items"
sed "s/@BOARD@/$board/; s/@TAG@/$expect/; s/@EXPECT@/$expect/" "$REPO/tools/az01-packer/browse-icon-probe.qml" > "$R/data/enginex/ui/packs/en/Skins/Common/Items/QmlWarnings.qml"
printf 'lang=en\n' > "$R/data/enginex/ui/settings.conf"
# The host's loader is bound over the rootfs's: its libc goes first, or a rootfs with an older glibc (AZ04) cannot start even env.
host_libs=LD_LIBRARY_PATH=/usr/lib/$triplet
env "$host_libs" chroot "$R" /usr/bin/dbus-daemon --system --fork --nopidfile > "$W/$board-dbus.log" 2>&1 || true
# The QML engine that evaluates the probe is the one in this rootfs.
echo "$board Qt library: $(ls "$R/usr/qt/lib" "$R/usr/lib" 2>/dev/null | grep -m1 -E '^libQt6Core\.so\.[0-9]+\.[0-9]+\.[0-9]+$')"
engine_rc=0
timeout --kill-after=5s "${BROWSE_ICON_TIMEOUT:-180}s" env "$host_libs" chroot "$R" /usr/bin/env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin HOME=/tmp/home LD_LIBRARY_PATH=/usr/lib/$triplet:/usr/lib:/usr/qt/lib \
    LD_PRELOAD=$preload SBX_CARD=$board SBX_OUT=4 SBX_IN=2 SBX_CLOCK_HZ=48000 SBX_HEADLESS_VSYNC=1 \
    QT_QPA_PLATFORM=eglfs QT_QPA_EGLFS_INTEGRATION=eglfs_kms LIBGL_ALWAYS_SOFTWARE=1 GALLIUM_DRIVER=llvmpipe LP_NUM_THREADS=2 \
    QML_DISABLE_DISK_CACHE=1 QML_XHR_ALLOW_FILE_READ=1 MALLOC_ARENA_MAX=1 /usr/Engine/Engine -d0 > "$W/$board-engine.log" 2>&1 || engine_rc=$?
echo "X166 ENGINE_EXIT|$engine_rc|" >> "$W/$board-engine.log"
cp "$R"/tmp/browse-*.png "$W/" 2>/dev/null || true
grep -aq "engine-ui-hook: Engine " "$W/$board-engine.log" || { echo "$board: the hook did not start" >&2; exit 1; }
grep -aE 'X166 (BROWSE|ERROR|DONE)' "$W/$board-engine.log" | sed 's/ *\[20[0-9T:.Z-]*\].*//; s/^\[W\] //' | cut -c1-200
if grep -aq 'X166 ERROR|' "$W/$board-engine.log" || ! grep -aq 'X166 DONE|PASS|' "$W/$board-engine.log"; then
    echo "$board BROWSE_ICON_FAIL"
    exit 1
fi
echo "$board BROWSE_ICON_PASS"
