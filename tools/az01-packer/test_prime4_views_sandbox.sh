#!/bin/bash
. "${ENGINEX_MIGRATION:-$(git rev-parse --show-toplevel)/rootfs-overlay/usr/libexec/enginex-migration}"
ENGINEX_LAB=${ENGINEX_LAB:-$legacy_lab}
# Build host only. Private JC11 rootfs, native armhf; no hardware or shared lab writes.
set -euo pipefail
REPO=$(cd "$(dirname "$0")/../.." && pwd)
W=${VIEWS_SANDBOX_DIR:-/var/tmp/prime4-views-native-JC11}
if [ "${1:-}" != --inner ]; then
    test "$(uname -m)" = aarch64
    mkdir -p "$W"
    exec 9> "$W/sandbox.lock"
    flock -n 9 || { echo "Views sandbox already in use" >&2; exit 1; }
    if [ ! -e "$W/image/rootfs.ext2" ]; then
        python3 "$REPO/tools/az01-packer/az01_packer.py" unpack ${ENGINEX_LAB}/fw-5.3.0/PRIME4-5.3.0-Update.img --output "$W/image" > "$W/unpack.json"
    fi
    python3 "$REPO/tools/ui-mod/build_packs.py" --qrc ${ENGINEX_LAB}/5.3.0/qrc --out "$W/packs" > "$W/packs.log"
    unshare -m -n -p -f --kill-child bash "$0" --inner
    exit
fi
mount --make-rprivate /
ip link set lo up
R=$W/root
mkdir -p "$R"
mount -o loop,ro "$W/image/rootfs.ext2" "$R"
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
mkdir -p "$R/tmp/home" "$R/run/dbus" "$R/data/enginex/ui/packs" "$R/tmp/fx-qml-imports"
cp -a ${ENGINEX_LAB}/5.3.0/qrc/airAssignments/InputAssignment ${ENGINEX_LAB}/5.3.0/qrc/airAssignments/OutputAssignment "$R/tmp/fx-qml-imports/"
cp "$REPO/rootfs-overlay/etc/enginex/libengine-ui-hook.so" "$R/tmp/libengine-ui-hook.so"
cp -a "$W/packs/en" "$R/data/enginex/ui/packs/"
mkdir -p "$R/data/enginex/ui/packs/en/Skins/Common/Items"
cp "$REPO/tools/az01-packer/prime4-views-probe.qml" "$R/data/enginex/ui/packs/en/Skins/Common/Items/QmlWarnings.qml"
printf 'lang=en\nfour_deck_waveform=1\nfour_deck_role=12\n' > "$R/data/enginex/ui/settings.conf"
chroot "$R" /usr/bin/dbus-daemon --system --fork --nopidfile > "$W/dbus.log" 2>&1 || true

python3 - "$W/views-state.json" > "$W/http.log" 2>&1 <<'PY' &
import http.server, json, sys

state_file = sys.argv[1]
state = {"active": 1, "views": {}}
with open(state_file, "w") as f:
    json.dump(state, f)

class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args): pass
    def do_GET(self):
        path = self.path.split("?", 1)[0]
        if path == "/v1/enginex/ui":
            body = json.dumps({
                "version": "5.3.0.5",
                "fourDeckWaveform": True,
                "fourDeckRole": "12",
                "fourDeckRoleActive": "12",
                "pairedView": False,
                "sources": ["tidal"],
                "rev": 1,
            }).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("X-UI-Rev", "1")
            self.end_headers()
            self.wfile.write(body)
            return
        if self.path == "/v1/enginex/views":
            body = json.dumps(state).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(body)
            return
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(b"{}")

    def do_POST(self):
        n = int(self.headers.get("Content-Length", "0") or "0")
        raw = self.rfile.read(n) if n > 0 else b"{}"
        if self.path == "/v1/enginex/views":
            data = json.loads(raw.decode() or "{}")
            state["active"] = data.get("active", state["active"])
            state["views"] = data.get("views", state["views"])
            with open(state_file, "w") as f:
                json.dump(state, f)
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(b'{"ok":true}')

http.server.HTTPServer(("127.0.0.1", 8080), Handler).serve_forever()
PY
server=$!
trap 'kill "$server" 2>/dev/null || true' EXIT
python3 -c 'import socket, time
for _ in range(50):
    try:
        with socket.create_connection(("127.0.0.1", 8080), 0.1): break
    except OSError:
        time.sleep(0.05)
'

set +e
timeout --kill-after=5s 25s chroot "$R" /usr/bin/env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin HOME=/tmp/home LD_LIBRARY_PATH=/usr/qt/lib \
    LD_PRELOAD=/tmp/libengine-ui-hook.so QT_QPA_PLATFORM=offscreen \
    QML_IMPORT_PATH=/tmp/fx-qml-imports QML2_IMPORT_PATH=/tmp/fx-qml-imports \
    QML_DISABLE_DISK_CACHE=1 MALLOC_ARENA_MAX=1 /usr/Engine/Engine -d0 > "$W/engine.log" 2>&1
engine_rc=$?
set -e
case "$engine_rc" in 0|124|137) ;; *) echo "Engine exited $engine_rc" >&2; tail -n 30 "$W/engine.log" >&2; exit 1 ;; esac
grep -a "VIEWSPROBE " "$W/engine.log" | cut -c1-220 || true
for tag in linked-view-ignored linked-view-absent view1-stock shift-view-2 shift-view-3 shift-view-back-1 load-horiz-lib load-horiz-nolib saved-view-priority select-view-2 done; do
    grep -aq "VIEWSPROBE $tag|PASS" "$W/engine.log" || { echo "MISSING PASS: $tag" >&2; exit 1; }
done
! grep -aEq "VIEWSPROBE .*\|FAIL" "$W/engine.log"
