#!/bin/bash
set -euo pipefail
WORK=/root/Batam-ARM-Paseo/re-shadow-pos
REPO="$WORK/repo"
TREE=/root/build/sc5000-lab/5.3.0/tree
SBX=/var/tmp/shadow-probe-sbx
LOG="$WORK/probe.log"

if [ "${1:-}" != "--inner" ]; then
    rm -rf "$SBX"
    mkdir -p "$SBX"
    arm-linux-gnueabihf-gcc -O2 -Wall -Wextra -Werror -shared -fPIC \
        -o "$SBX/alsa_null_fixture.so" "$REPO/tools/shadow-twin/alsa_null_fixture.c" -ldl -lpthread
    arm-linux-gnueabihf-gcc -O2 -Wall -Wextra -Werror -shared -fPIC \
        -o "$SBX/layer-switch-fixture.so" "$REPO/tools/az01-packer/layer-switch-fixture.c" -ldl -lpthread
    python3 -c '
import wave, struct, math
with wave.open("'"$SBX/t2.wav"'", "wb") as w:
    w.setnchannels(2); w.setsampwidth(2); w.setframerate(44100)
    frames = b"".join(struct.pack("<hh", int(8000*math.sin(2*math.pi*440*i/44100)), int(8000*math.sin(2*math.pi*440*i/44100))) for i in range(44100*2))
    w.writeframes(frames * 15) # 30s audio
'
    exec flock /var/lock/enginex-heavy.lock unshare -m -n -p -f --kill-child bash "$0" --inner
fi

cleanup() {
    umount -R "$SBX/root" 2>/dev/null || umount -lR "$SBX/root" 2>/dev/null || true
    umount "$SBX/lower" 2>/dev/null || umount -l "$SBX/lower" 2>/dev/null || true
    rm -rf "$SBX" 2>/dev/null || true
}
trap cleanup EXIT

mount --make-rprivate /
ip link set lo up
python3 "$REPO/tools/az01-packer/az01_packer.py" unpack /root/build/sc5000-lab/fw-5.3.0/SC5000-5.3.0-Update.img --output "$SBX/image" > "$SBX/unpack.json"
mv "$SBX/image/rootfs.ext2" "$SBX/rootfs.ext2"
rm -rf "$SBX/image"
L="$SBX/lower"
R="$SBX/root"
U="$SBX/upper"
mkdir -p "$L" "$R" "$U/up" "$U/work"
mount -o loop,ro "$SBX/rootfs.ext2" "$L"
mount -t overlay overlay -o "lowerdir=$L,upperdir=$U/up,workdir=$U/work" "$R"
for d in tmp run dev data sys; do [ -L "$R/$d" ] && rm -f "$R/$d"; mkdir -p "$R/$d"; mount -t tmpfs tmpfs "$R/$d"; done
chroot "$R" /bin/mkdir -p /var/volatile/tmp /var/volatile/log /var/volatile/cache
mkdir -p "$R/dev/shm" "$R/dev/pts" "$R/dev/dri" "$R/sys/firmware/devicetree/base"
for n in null zero full random urandom; do touch "$R/dev/$n"; mount --bind "/dev/$n" "$R/dev/$n"; done
mount --bind /dev/dri "$R/dev/dri"
mkdir -p "$R/usr/lib/arm-linux-gnueabihf"
mount --bind -o ro /usr/lib/arm-linux-gnueabihf "$R/usr/lib/arm-linux-gnueabihf"
for d in /usr/share/glvnd /usr/share/drirc.d; do
    [ ! -d "$d" ] || { mkdir -p "$R$d"; mount --bind -o ro "$d" "$R$d"; }
done
mount --bind /usr/lib/arm-linux-gnueabihf/ld-linux-armhf.so.3 "$R/lib/ld-linux-armhf.so.3"
for d in class devices bus; do mkdir -p "$R/sys/$d"; mount --bind -o ro "/sys/$d" "$R/sys/$d"; done
printf %s "JP07" > "$R/sys/firmware/devicetree/base/inmusic,product-code"
printf 'inmusic,jp07\0rockchip,rk3288\0' > "$R/sys/firmware/devicetree/base/compatible"
mount -t proc proc "$R/proc"
printf '           CPU0\n 41:        100     GICv2  32 Level     ttyS0\n' > "$U/interrupts"
mount --bind "$U/interrupts" "$R/proc/interrupts"
mount -t tmpfs tmpfs "$R/proc/irq"
mkdir -p "$R/proc/irq/41"; printf 'f\n' > "$R/proc/irq/41/smp_affinity"; printf '0-3\n' > "$R/proc/irq/41/smp_affinity_list"
printf '{"device": "/dev/dri/card0", "headless": "1280x800", "hwcursor": false}\n' > "$U/screen.json"
for c in "$R"/usr/Engine/ScreenConfiguration/{Default,JP07}/ScreenConfiguration.json; do
    [ ! -f "$c" ] || mount --bind "$U/screen.json" "$c"
done

mkdir -p "$R/etc/enginex" "$R/tmp/home" "$R/run/dbus" "$R/data/music" "$R/data/enginex/ui/packs/en/Skins/Common/Items"
cp "$REPO/rootfs-overlay/etc/enginex/libengine-ui-hook.so" "$R/etc/enginex/"
cp "$WORK/shadow_probe.so" "$SBX/layer-switch-fixture.so" "$SBX/alsa_null_fixture.so" "$R/tmp/"
cp "$SBX/t2.wav" "$R/data/music/t2.wav"
cp "$WORK/probe.qml" "$R/data/enginex/ui/packs/en/Skins/Common/Items/QmlWarnings.qml"
printf 'lang=en\n' > "$R/data/enginex/ui/settings.conf"

chroot "$R" /usr/bin/dbus-daemon --system --fork --nopidfile > "$U/dbus.log" 2>&1 || true

chroot "$R" /usr/bin/env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin HOME=/tmp/home \
    LD_LIBRARY_PATH=/usr/lib/arm-linux-gnueabihf:/usr/lib:/usr/qt/lib \
    LD_PRELOAD=/tmp/shadow_probe.so:/tmp/layer-switch-fixture.so:/tmp/alsa_null_fixture.so:/etc/enginex/libengine-ui-hook.so \
    SBX_CARD="JP07" SBX_OUT="4" SBX_IN="2" SBX_CLOCK_HZ=48000 SBX_HEADLESS_VSYNC=1 \
    QT_QPA_PLATFORM=offscreen LIBGL_ALWAYS_SOFTWARE=1 GALLIUM_DRIVER=llvmpipe LP_NUM_THREADS=2 \
    QML_DISABLE_DISK_CACHE=1 QML_XHR_ALLOW_FILE_READ=1 MALLOC_ARENA_MAX=1 \
    /usr/Engine/Engine -d0 > "$LOG" 2>&1 &
engine=$!

for _ in $(seq 75); do
    grep -aqE 'LSW DONE|PROBE_QML\|ERROR' "$LOG" || ! kill -0 "$engine" 2>/dev/null && break
    sleep 1
done
kill -TERM "$engine" 2>/dev/null || true; sleep 2; kill -KILL "$engine" 2>/dev/null || true; wait "$engine" 2>/dev/null || true
echo "PROBE_FINISHED"
