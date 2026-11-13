#!/bin/bash
# Private, native Engine/eglfs smoke test on the build host. Never a deck.
set -euo pipefail
REPO=$(cd "$(dirname "$0")/../.." && pwd)
W=$REPO/out/shadow-native
if [ "${1:-}" != --inner ]; then
    mkdir -p "$W"
    if [ ! -e "$W/image/rootfs.ext2" ]; then
        python3 "$REPO/tools/az01-packer/az01_packer.py" unpack /root/build/sc5000-lab/fw-5.3.0/SC5000-5.3.0-Update.img --output "$W/image" > "$W/unpack.json"
    fi
    python3 "$REPO/tools/ui-mod/build_packs.py" --qrc /root/build/sc5000-lab/5.3.0/qrc --out "$W/packs" > "$W/packs.log"
    python3 "$REPO/tools/az01-packer/shadow_render_probe.py" --out "$W/probe.qml"
    unshare -m -n -p -f --kill-child "$0" --inner
    exit
fi
mount --make-rprivate /
ip link set lo up
R=$W/root
mkdir -p "$R"
# The image is this task's private copy. The lab and its images remain read-only.
mount -o loop "$W/image/rootfs.ext2" "$R"
for d in tmp run dev data sys; do mount -t tmpfs tmpfs "$R/$d"; done
mkdir -p "$R/dev/dri" "$R/dev/shm" "$R/dev/pts"
for n in null zero full random urandom; do touch "$R/dev/$n"; mount --bind "/dev/$n" "$R/dev/$n"; done
mount --bind /dev/dri "$R/dev/dri"
for d in class devices bus; do
    mkdir -p "$R/sys/$d"
    mount --bind "/sys/$d" "$R/sys/$d"
    mount -o remount,bind,ro "$R/sys/$d"
done
mkdir -p "$R/sys/firmware/devicetree/base"
printf JP07 > "$R/sys/firmware/devicetree/base/inmusic,product-code"
printf 'inmusic,jp07\0rockchip,rk3288\0' > "$R/sys/firmware/devicetree/base/compatible"
mount -t proc -o ro proc "$R/proc"
printf '           CPU0\n 41:        100     GICv2  32 Level     ttyS0\n' > "$W/interrupts"
mount --bind "$W/interrupts" "$R/proc/interrupts"
mount -t tmpfs tmpfs "$R/proc/irq"
mkdir -p "$R/proc/irq/41"
printf 'f\n' > "$R/proc/irq/41/smp_affinity"
printf '0-3\n' > "$R/proc/irq/41/smp_affinity_list"
mkdir -p "$R/etc/sc5000" "$R/tmp/home" "$R/run/dbus" "$R/data/sc5000-custom/ui/packs"
cp "$REPO/rootfs-overlay/etc/enginex/libengine-ui-hook.so" "$R/etc/sc5000/"
cp -a "$W/packs/en" "$R/data/sc5000-custom/ui/packs/"
mkdir -p "$R/data/sc5000-custom/ui/packs/en/Skins/Common/Items"
cp "$W/probe.qml" "$R/data/sc5000-custom/ui/packs/en/Skins/Common/Items/QmlWarnings.qml"
printf 'lang=en\nfour_deck_waveform=1\nfour_deck_role=13\n' > "$R/data/sc5000-custom/ui/settings.conf"
chroot "$R" /usr/bin/dbus-daemon --system --fork --nopidfile > "$W/dbus.log" 2>&1 || true
# A fixed PNG fixture, and 404 for all real service calls. No suite or library is loaded.
python3 - "$W" > "$W/http.log" 2>&1 <<'PY' &
import http.server, struct, zlib
def chunk(k,b): return struct.pack('>I',len(b))+k+b+struct.pack('>I',zlib.crc32(k+b))
w,h=2048,128
rows=[]
for y in range(h):
    row=bytearray([0])
    for x in range(w):
        height=8+(x//13)%50
        row.extend((74,120,255,255) if abs(y-64)<height else (0,0,0,0))
    rows.append(row)
png=b'\x89PNG\r\n\x1a\n'+chunk(b'IHDR',struct.pack('>IIBBBBB',w,h,8,6,0,0,0))+chunk(b'IDAT',zlib.compress(b''.join(rows)))+chunk(b'IEND',b'')
class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path.split('?',1)[0]!='/tile.png': self.send_error(404);return
        self.send_response(200);self.send_header('Content-Type','image/png');self.end_headers();self.wfile.write(png)
http.server.HTTPServer(('127.0.0.1',8080),Handler).serve_forever()
PY
mock=$!
trap 'kill "$mock" 2>/dev/null || true' EXIT
timeout --kill-after=5s 35s chroot "$R" /usr/bin/env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin HOME=/tmp/home LD_LIBRARY_PATH=/usr/qt/lib \
    LD_PRELOAD=/etc/sc5000/libengine-ui-hook.so QT_QPA_PLATFORM=eglfs QT_QPA_EGLFS_INTEGRATION=eglfs_kms \
    QML_DISABLE_DISK_CACHE=1 MALLOC_ARENA_MAX=1 /usr/Engine/Engine -d0 > "$W/engine.log" 2>&1 || true
test -s "$R/tmp/shadow-render.png"
cp "$R/tmp/shadow-render.png" "$W/render.png"
grep -q 'SHADOWPROBE ancestor-hidden|true' "$W/engine.log"
grep -q 'SHADOWPROBE screenshot|true' "$W/engine.log"
if grep -aE 'Expected token|Syntax error|is not a type|Cannot assign to non-existent|ReferenceError|TypeError' "$W/engine.log" > "$W/qml-errors.log"; then
    cat "$W/qml-errors.log"
    exit 1
fi
echo SHADOW_NATIVE_RENDER_PASS
