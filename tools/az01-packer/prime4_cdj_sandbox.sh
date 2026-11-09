#!/bin/bash
# Batam only. Private JC11 rootfs, native armhf; no hardware or shared lab writes.
set -euo pipefail
REPO=$(cd "$(dirname "$0")/../.." && pwd)
W=$REPO/out/prime4-cdj-sandbox
if [ "${1:-}" != --inner ]; then
    mkdir -p "$W"
    if [ ! -e "$W/image/rootfs.ext2" ]; then
        python3 "$REPO/tools/az01-packer/az01_packer.py" unpack /root/build/sc5000-lab/fw-5.3.0/PRIME4-5.3.0-Update.img --output "$W/image" > "$W/unpack.json"
    fi
    python3 "$REPO/tools/ui-mod/build_packs.py" --qrc /root/build/sc5000-lab/5.3.0/qrc --out "$W/packs" > "$W/packs.log"
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
printf JC11 > "$R/sys/firmware/devicetree/base/inmusic,product-code"
printf 'inmusic,jc11\0rockchip,rk3288\0' > "$R/sys/firmware/devicetree/base/compatible"
mount -t proc -o ro proc "$R/proc"
printf '           CPU0\n 41:        100     GICv2  32 Level     ttyS0\n' > "$W/interrupts"
mount --bind "$W/interrupts" "$R/proc/interrupts"
mount -t tmpfs tmpfs "$R/proc/irq"
mkdir -p "$R/proc/irq/41"
printf 'f\n' > "$R/proc/irq/41/smp_affinity"
printf '0-3\n' > "$R/proc/irq/41/smp_affinity_list"
mkdir -p "$R/etc/sc5000" "$R/tmp/home" "$R/run/dbus" "$R/data/sc5000-custom/ui/packs"
cp "$REPO/rootfs-overlay/etc/sc5000/libengine-ui-hook.so" "$R/etc/sc5000/"
cp -a "$W/packs/en" "$R/data/sc5000-custom/ui/packs/"
mkdir -p "$R/data/sc5000-custom/ui/packs/en/Skins/Common/Items"
cp "$REPO/tools/az01-packer/prime4_cdj_probe.qml" "$R/data/sc5000-custom/ui/packs/en/Skins/Common/Items/QmlWarnings.qml"
printf 'lang=en\nfour_deck_waveform=1\nfour_deck_role=12\n' > "$R/data/sc5000-custom/ui/settings.conf"
chroot "$R" /usr/bin/dbus-daemon --system --fork --nopidfile > "$W/dbus.log" 2>&1 || true

python3 - "$W" > "$W/http.log" 2>&1 <<'PY' &
import http.server, json, struct, time, zlib, urllib.parse

def chunk(k, b):
    return struct.pack('>I', len(b)) + k + b + struct.pack('>I', zlib.crc32(k + b))

w, h = 2048, 128
rows = []
for y in range(h):
    row = bytearray([0])
    for x in range(w):
        height = 8 + (x // 13) % 50
        row.extend((74, 120, 255, 255) if abs(y - 64) < height else (0, 0, 0, 0))
    rows.append(row)
png = b'\x89PNG\r\n\x1a\n' + chunk(b'IHDR', struct.pack('>IIBBBBB', w, h, 8, 6, 0, 0, 0)) + chunk(b'IDAT', zlib.compress(b''.join(rows))) + chunk(b'IEND', b'')

class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        u = urllib.parse.urlsplit(self.path)
        q = urllib.parse.parse_qs(u.query)
        if u.path == '/tile.png':
            self.send_response(200)
            self.send_header('Content-Type', 'image/png')
            self.end_headers()
            self.wfile.write(png)
            return
        if u.path == '/v1/sc5000/ui':
            res = {
                "version": "5.3.0.4",
                "fourDeckWaveform": True,
                "fourDeckRole": "12",
                "fourDeckRoleActive": "12",
                "prolinkShare": True,
                "prolinkWaveform": True
            }
            body = json.dumps(res).encode()
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.end_headers()
            self.wfile.write(body)
            return
        if u.path == '/v1/sc5000/shadow':
            now_ms = int(time.time() * 1000)
            lines_param = q.get('line', [''])[0]
            decks = []
            if '3' in lines_param.split(','):
                decks.append({
                    "deck": 3,
                    "key": "cdj:fixture",
                    "title": "Rekordbox Track",
                    "artist": "DJ User",
                    "bpm": 175.0,
                    "lineIn": True,
                    "metadataOnly": False,
                    "displayDetail": True,
                    "playing": True,
                    "positionMs": 2000,
                    "at": now_ms
                })
            res = {
                "role": "12",
                "decks": decks,
                "ver": 1,
                "displayOnly": True
            }
            body = json.dumps(res).encode()
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.end_headers()
            self.wfile.write(body)
            return
        if u.path == '/v1/sc5000/shadow/display':
            asset = {
                "version": 1,
                "key": "cdj:fixture",
                "title": "Rekordbox Track",
                "artist": "DJ User",
                "bpm": 175.0,
                "durationMs": 10000,
                "sampleRate": 44100,
                "detail": True,
                "detailPending": False,
                "waveformWidth": 2048,
                "waveformUrl": "/tile.png",
                "overviewUrl": "/tile.png",
                "waveformTiles": ["/tile.png"],
                "beats": [{"ms": 0, "number": 0}, {"ms": 342.8, "number": 1}],
                "cues": [{"i": 1, "ms": 1000, "n": "INTRO", "c": "#ffb400"}],
                "loops": []
            }
            body = json.dumps(asset).encode()
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.end_headers()
            self.wfile.write(body)
            return
        self.send_error(404)

    def do_POST(self):
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b'{"status":"ok"}')

http.server.HTTPServer(('127.0.0.1', 8080), Handler).serve_forever()
PY
mock=$!
trap 'kill "$mock" 2>/dev/null || true' EXIT

timeout --kill-after=5s 35s chroot "$R" /usr/bin/env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin HOME=/tmp/home LD_LIBRARY_PATH=/usr/qt/lib \
    LD_PRELOAD=/etc/sc5000/libengine-ui-hook.so QT_QPA_PLATFORM=eglfs QT_QPA_EGLFS_INTEGRATION=eglfs_kms \
    QML_DISABLE_DISK_CACHE=1 MALLOC_ARENA_MAX=1 /usr/Engine/Engine -d0 > "$W/engine.log" 2>&1 || true

if [ -s "$R/tmp/prime4-cdj-4deck.png" ]; then
    cp "$R/tmp/prime4-cdj-4deck.png" "$W/prime4-cdj-channel3.png"
    echo "PRIME4_CDJ_RENDER_PASS"
else
    echo "PRIME4_CDJ_RENDER_NO_SCREENSHOT"
fi
cat "$W/engine.log" | grep "PRIME4_PROBE" || true
