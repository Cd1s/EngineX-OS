#!/bin/bash
# Native armhf Engine on the build host, private copies of the actual NH08 / NH10 rootfs.
# Run after tools/ui-mod/build.sh. No qemu, host services or deck connections.
# bash tools/az01-packer/mixstream-sandbox.sh /root/workspace/sc5000-m1-20261109
# Optional gen-2 boot: X166_BOARDS="NH08 NH10" bash tools/az01-packer/mixstream-sandbox.sh <new-dir>
set -euo pipefail
REPO=$(cd "$(dirname "$0")/../.." && pwd)
W=$(realpath -m "${1:?private sandbox directory required}")
LAB=/root/build/sc5000-lab
if [ "${2:-}" != --inner ]; then
    [ "$(uname -m)" = aarch64 ] || { echo 'the build host native ARM host required' >&2; exit 1; }
    [ ! -e "$W" ] || { echo "refusing existing sandbox $W" >&2; exit 1; }
    mkdir -p "$W"
    python3 "$REPO/tools/az01-packer/az01_packer.py" unpack "$LAB/fw-5.3.0/MIXSTREAMPRO-5.3.0-Update.img" --output "$W/NH08-image" > "$W/unpack.log"
    python3 - "$REPO" "$LAB" "$W" <<'PY'
import gzip, hashlib, importlib.util, lzma, sys
from pathlib import Path
repo, lab, work = map(Path, sys.argv[1:])
spec = importlib.util.spec_from_file_location('az0x', repo/'tools/az01-packer/az0x_packer.py')
az0x = importlib.util.module_from_spec(spec); spec.loader.exec_module(az0x)
image = (lab/'fw-5.3.0-gen2/MIXSTREAMPROGO-5.3.0-Update.img').read_bytes()
r = next(r for r in az0x.parse_header(image)['records'] if r['name'] == 'rootfs')
payload = image[r['offset']:r['offset']+r['size']]
(work/'NH10-image').mkdir()
inflate = lzma.decompress if payload.startswith(b'\xfd7zXZ\0') else gzip.decompress
(work/'NH10-image/rootfs.ext2').write_bytes(inflate(payload))
print('NH10 rootfs payload', hex(r['offset']), hex(r['size']), hashlib.sha256(payload).hexdigest())
PY
    python3 "$REPO/tools/ui-mod/build_packs.py" --qrc "$LAB/5.3.0/qrc" --out "$W/packs" > "$W/packs.log"
    arm-linux-gnueabihf-gcc -O2 -shared -fPIC -o "$W/qrc_dump.so" "$REPO/tools/ui-mod/qrc_dump.c" -ldl
    for board in ${X166_BOARDS:-NH08}; do
        unshare -m -n -p -f --kill-child bash "$0" "$W" --inner "$board"
    done
    exit
fi
board=${3:?board required}
case "$board" in NH08) compat=nh08 ;; NH10) compat=nh10 ;; *) exit 2 ;; esac
mount --make-rprivate /
ip link set lo up
R=$W/$board-root
mkdir -p "$R"
mount -o loop "$W/$board-image/rootfs.ext2" "$R"
for d in tmp run dev data sys; do mount -t tmpfs tmpfs "$R/$d"; done
# Gen-2's /var/tmp points to volatile/tmp, normally populated at boot.
# Resolve guest paths inside chroot, including any absolute guest symlinks.
chroot "$R" /bin/mkdir -p /var/volatile/tmp /var/volatile/log /var/volatile/cache
mkdir -p "$R/dev/shm" "$R/dev/pts" "$R/sys/firmware/devicetree/base" "$R/sys/devices/system/cpu"
for f in online possible present; do echo 0-3 > "$R/sys/devices/system/cpu/$f"; done
for n in null zero full random urandom; do touch "$R/dev/$n"; mount --bind "/dev/$n" "$R/dev/$n"; done
printf '%s\0' "$board" > "$R/sys/firmware/devicetree/base/inmusic,product-code"
printf 'inmusic,%s\0inmusic,az05\0rockchip,rk3288\0' "$compat" > "$R/sys/firmware/devicetree/base/compatible"
mount -t proc -o ro proc "$R/proc"
# Engine requires the deck's serial IRQ. Its affinity writes stay in tmpfs.
printf '           CPU0\n 41:        100     GICv2  32 Level     ttyS0\n' > "$W/$board-interrupts"
mount --bind "$W/$board-interrupts" "$R/proc/interrupts"
mount -t tmpfs tmpfs "$R/proc/irq"
mkdir -p "$R/proc/irq/41"
printf 'f\n' > "$R/proc/irq/41/smp_affinity"
printf '0-3\n' > "$R/proc/irq/41/smp_affinity_list"
mkdir -p "$R/etc/enginex" "$R/tmp/home" "$R/tmp/qrc-dump" "$R/run/dbus" "$R/data/enginex/ui/packs"
cp "${X166_HOOK:-$REPO/rootfs-overlay/etc/enginex/libengine-ui-hook.so}" "$R/etc/enginex/libengine-ui-hook.so"
cp "$W/qrc_dump.so" "$R/tmp/"
cp -a "$W/packs/en" "$R/data/enginex/ui/packs/"
mkdir -p "$R/data/enginex/ui/packs/en/Skins/Common/Items"
cp "$REPO/tools/az01-packer/mixstream-probe.qml" "$R/data/enginex/ui/packs/en/Skins/Common/Items/QmlWarnings.qml"
printf 'lang=en\n' > "$R/data/enginex/ui/settings.conf"
chroot "$R" /usr/bin/dbus-daemon --system --fork --nopidfile > "$W/$board-dbus.log" 2>&1 || true
python3 "$REPO/tools/az01-packer/mixstream-collection-probe.py" "$R" "${X166_COLLECTION:-full}" > "$W/$board-collection.log" 2>&1 &
collection_pid=$!
trace=()
if [ "${X166_STRACE:-0}" = 1 ]; then trace=(strace -f -o "$W/$board-strace.log" -e trace=file,process); fi
engine_rc=0
"${trace[@]}" timeout --kill-after=5s 40s chroot "$R" /usr/bin/env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin HOME=/tmp/home LD_LIBRARY_PATH=/usr/qt/lib \
    LD_PRELOAD=/tmp/qrc_dump.so:/etc/enginex/libengine-ui-hook.so QRC_DUMP_DIR=/tmp/qrc-dump QT_QPA_PLATFORM=offscreen \
    QML_DISABLE_DISK_CACHE=1 MALLOC_ARENA_MAX=1 /usr/Engine/Engine -d0 > "$W/$board-engine.log" 2>&1 || engine_rc=$?
echo "X166 ENGINE_EXIT|$engine_rc|" >> "$W/$board-engine.log"
if [ -f "$R/tmp/engine-quit-reason" ]; then cp "$R/tmp/engine-quit-reason" "$W/$board-quit-reason"; fi
mkdir -p "$W/$board-qrc-dump"
cp -a "$R/tmp/qrc-dump/." "$W/$board-qrc-dump/"
wait "$collection_pid" || { tail -n 8 "$W/$board-collection.log"; tail -n 8 "$W/$board-engine.log"; cat "$W/$board-quit-reason" 2>/dev/null || true; exit 1; }
cat "$W/$board-collection.log"
grep -aE 'X166 (CONFIG|TOUCH|CAROUSEL|SWEEP|ERROR|DONE)|X166 PASS\|(native Sweep|Sweep Type|Beat Jump preference)' "$W/$board-engine.log" | cut -c1-200 | head -n 40
! grep -aq 'X166 ERROR|' "$W/$board-engine.log"
! grep -aEq '(Pads|SweepFxSelectCarousel)\.qml:[0-9]+: (TypeError|ReferenceError|Error:|Unable to assign)' "$W/$board-engine.log"
grep -aq 'X166 DONE|PASS|' "$W/$board-engine.log"
echo "$board MIXSTREAM_NATIVE_PASS"
