#!/bin/bash
# Batam, native armhf, isolated official SCLIVE4 rootfs; never connects to a deck.
set -euo pipefail
REPO=$(cd "$(dirname "$0")/../.." && pwd)
W=$(realpath -m "${1:?new private sandbox directory required}")
LAB=/root/build/sc5000-lab
if [ "${2:-}" != --inner ]; then
    [ "$(uname -m)" = aarch64 ]
    [ ! -e "$W" ] || { echo "refusing existing sandbox $W" >&2; exit 1; }
    mkdir -p "$W"
    # This official image is in fw-5.3.0, not fw-5.3.0-gen2 in the lab inventory.
    python3 - "$REPO" "$LAB/fw-5.3.0/SCLIVE4-5.3.0-Update.img" "$W" <<'PY'
import gzip, hashlib, importlib.util, lzma, sys
from pathlib import Path
repo, image, work = map(Path, sys.argv[1:])
spec = importlib.util.spec_from_file_location('az0x', repo/'tools/az01-packer/az0x_packer.py')
az0x = importlib.util.module_from_spec(spec); spec.loader.exec_module(az0x)
b = image.read_bytes(); r = next(r for r in az0x.parse_header(b)['records'] if r['name'] == 'rootfs')
payload = b[r['offset']:r['offset']+r['size']]
inflate = lzma.decompress if payload.startswith(b'\xfd7zXZ\0') else gzip.decompress
(work/'rootfs.ext2').write_bytes(inflate(payload))
(work/'fixture.txt').write_text(f'official image SHA256 {hashlib.sha256(b).hexdigest()}\nrootfs payload SHA256 {hashlib.sha256(payload).hexdigest()}\n')
PY
    python3 "$REPO/tools/ui-mod/build_packs.py" --qrc "$LAB/5.3.0/qrc" --out "$W/packs" > "$W/packs.log"
    fixture=sclive-aux-hardware-fixture.c
    if [ "${X171_LIVE_AUDIO:-0}" = 1 ]; then
        fixture=sclive-aux-audio-fixture.c
        arm-linux-gnueabihf-gcc -O2 -Wall -Wextra -Werror -shared -fPIC \
            -U_FORTIFY_SOURCE -D_FORTIFY_SOURCE=0 -DENGINE_UI_HOOK_AUX_TRACE \
            -ffunction-sections -fdata-sections -Wl,--gc-sections -Wl,-z,max-page-size=4096 \
            -o "$W/hook-trace.so" "$REPO/tools/ui-mod/engine_ui_hook.c" -ldl -lpthread
        export X171_HOOK="${X171_HOOK:-$W/hook-trace.so}"
    fi
    arm-linux-gnueabihf-gcc -O2 -Wall -Wextra -Werror -shared -fPIC -o "$W/hardware-fixture.so" "$REPO/tools/az01-packer/$fixture" -ldl -lpthread
    arm-linux-gnueabihf-gcc -O2 -shared -fPIC -o "$W/qrc_dump.so" "$REPO/tools/ui-mod/qrc_dump.c" -ldl
    for lang in ${X171_LANGS:-en zh-Hans zh-Hant}; do
        unshare -m -n -p -f --kill-child bash "$0" "$W" --inner "$lang"
    done
    exit
fi
lang=${3:?language required}
mount --make-rprivate /
ip link set lo up
R=$W/root-$lang
mkdir -p "$R"
mount -o loop "$W/rootfs.ext2" "$R"
for d in tmp run dev data sys; do mount -t tmpfs tmpfs "$R/$d"; done
chroot "$R" /bin/mkdir -p /var/volatile/tmp /var/volatile/log /var/volatile/cache
mkdir -p "$R/dev/shm" "$R/dev/pts" "$R/sys/firmware/devicetree/base" "$R/sys/devices/system/cpu"
for f in online possible present; do echo 0-3 > "$R/sys/devices/system/cpu/$f"; done
for n in null zero full random urandom; do touch "$R/dev/$n"; mount --bind "/dev/$n" "$R/dev/$n"; done
printf 'JP21' > "$R/sys/firmware/devicetree/base/inmusic,product-code"
printf 'inmusic,jp21\0inmusic,az01\0rockchip,rk3288\0' > "$R/sys/firmware/devicetree/base/compatible"
mount -t proc -o ro proc "$R/proc"
printf '           CPU0\n 41:        100     GICv2  32 Level     ttyS0\n' > "$W/interrupts-$lang"
mount --bind "$W/interrupts-$lang" "$R/proc/interrupts"
mount -t tmpfs tmpfs "$R/proc/irq"
mkdir -p "$R/proc/irq/41"
printf 'f\n' > "$R/proc/irq/41/smp_affinity"
printf '0-3\n' > "$R/proc/irq/41/smp_affinity_list"
mkdir -p "$R/etc/sc5000" "$R/tmp/home" "$R/tmp/qrc-dump" "$R/run/dbus" "$R/data/sc5000-custom/ui/packs"
cp "${X171_HOOK:-$REPO/rootfs-overlay/etc/sc5000/libengine-ui-hook.so}" "$R/etc/sc5000/libengine-ui-hook.so"
cp "$W/qrc_dump.so" "$W/hardware-fixture.so" "$R/tmp/"
cp -a "$W/packs/$lang" "$R/data/sc5000-custom/ui/packs/"
mkdir -p "$R/data/sc5000-custom/ui/packs/$lang/Skins/Common/Items"
cp "$REPO/tools/az01-packer/sclive-aux-probe.qml" "$R/data/sc5000-custom/ui/packs/$lang/Skins/Common/Items/QmlWarnings.qml"
printf 'lang=%s\n' "$lang" > "$R/data/sc5000-custom/ui/settings.conf"
if [ "${X171_LIVE_AUDIO:-0}" = 1 ]; then
    python3 - "$R/data/sc5000-custom/ui/packs/$lang/Skins/Common/Items/QmlWarnings.qml" <<'PY'
from pathlib import Path
import sys
p = Path(sys.argv[1]); text = p.read_text()
assert text.count('input.index = 1') == 1
p.write_text(text.replace('input.index = 1', 'probe.check(input.index === 1, "boot AUX without touching selector")'))
PY
    cat > "$R/etc/asound.conf" <<'EOF'
pcm.x171_null {
    type null
    hint { show on description "X171 Engine native audio callback test" }
}
pcm.!default { type null }
EOF
fi
chroot "$R" /usr/bin/dbus-daemon --system --fork --nopidfile > "$W/dbus-$lang.log" 2>&1 || true
rc=0
trace=()
if [ "${X171_STRACE:-0}" = 1 ]; then trace=(strace -f -o "$W/trace-$lang.log" -e trace=file,process,connect,sendto,write); fi
"${trace[@]}" timeout --kill-after=5s 25s chroot "$R" /usr/bin/env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin HOME=/tmp/home LD_LIBRARY_PATH=/usr/qt/lib \
    LD_PRELOAD=/tmp/qrc_dump.so:/tmp/hardware-fixture.so:/etc/sc5000/libengine-ui-hook.so QRC_DUMP_DIR=/tmp/qrc-dump QT_QPA_PLATFORM=offscreen \
    QML_DISABLE_DISK_CACHE=1 MALLOC_ARENA_MAX=1 X171_ALSA_NULL="${X171_LIVE_AUDIO:-0}" /usr/Engine/Engine "${X171_ENGINE_ARG:--d0}" > "$W/engine-$lang.log" 2>&1 || rc=$?
printf 'X171 ENGINE_EXIT|%s|\n' "$rc" >> "$W/engine-$lang.log"
case "$rc" in 0|124|137) ;; *) echo "Engine failed: $rc" >&2; exit 1 ;; esac
find "$R/tmp" -maxdepth 3 -type f -printf '%P\n' > "$W/guest-files-$lang.txt"
if [ -f "$R/tmp/engine-quit-reason" ]; then cp "$R/tmp/engine-quit-reason" "$W/quit-reason-$lang.txt"; fi
mkdir -p "$W/qrc-$lang"
cp -a "$R/tmp/qrc-dump/." "$W/qrc-$lang/"
grep -aE 'X171 (CONFIG|PROPERTY|PAGE|INPUTS|SELECTOR|PASS|ERROR|DONE)' "$W/engine-$lang.log" | cut -c1-200 | head -n 40
! grep -aq 'X171 ERROR|' "$W/engine-$lang.log"
grep -aq 'X171 DONE|PASS|' "$W/engine-$lang.log"
if [ "${X171_LIVE_AUDIO:-0}" = 1 ]; then
    grep -aE 'X171 (ALSA_OPEN|ALSA_START|AUDIO|BOOT|LIVE)' "$W/engine-$lang.log" | cut -c1-200 | head -n 20
    grep -aq 'X171 AUDIO_DONE|PASS|' "$W/engine-$lang.log"
    ! grep -aq 'X171 AUDIO_DONE|FAIL|' "$W/engine-$lang.log"
    printf '%s SCLIVE_AUX_LIVE_AUDIO_PASS\n' "$lang"
    exit
fi
grep -aq 'X171 BUFFER_DONE|PASS|' "$W/engine-$lang.log"
! grep -aq 'X171 BUFFER_DONE|FAIL|' "$W/engine-$lang.log"
grep -aE 'X171 (BUFFER|LANGUAGE)' "$W/engine-$lang.log" | cut -c1-200 | head -n 6
printf '%s SCLIVE_AUX_NATIVE_PASS\n' "$lang"
