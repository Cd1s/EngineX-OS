#!/bin/bash
# #336 E4 sandbox test: boots each board's stock 5.3.0 Engine with the built UI pack and a
# deterministic loopback HTTP server, and verifies that Settings switches, streaming switches,
# ListEntry selectors, and Remote access take effect on a single click without poll/POST clobber.
#
# Run under enginex-run sandbox on an ARM build host:
#   enginex-run sandbox -- bash tools/az01-packer/test_settings_click_sandbox.sh <new-private-dir> [BOARD ...]
# E4_BUILDER=<path/to/build_packs.py> lets you prove fail-before against another revision.
set -euo pipefail
REPO=$(cd "$(dirname "$0")/../.." && pwd)
. "${ENGINEX_MIGRATION:-$REPO/rootfs-overlay/usr/libexec/enginex-migration}"
ENGINEX_LAB=${ENGINEX_LAB:-$legacy_lab}
W=$(realpath -m "${1:?new private sandbox directory required}")
LAB=${ENGINEX_LAB}
BUILDER=$(realpath -m "${E4_BUILDER:-$REPO/tools/ui-mod/build_packs.py}")

image_of() {
    case "$1" in
        JP07|JP08|JP11) echo gen1 fw-5.3.0/SC5000 ;;
        JP13|JP14) echo gen1 fw-5.3.0/SC6000 ;;
        JC11|JC16|NH08) echo gen1 fw-5.3.0/PRIME4 ;;
        JP21|JP11S) echo gen2 fw-5.3.0/SCLIVE4 ;;
        *) echo "unsupported board $1" >&2; return 1 ;;
    esac
}

if [ "${2:-}" != --inner ]; then
    [ "$(uname -m)" = aarch64 ] || { echo 'native ARM build host (aarch64) required' >&2; exit 1; }
    [ ! -e "$W" ] || { echo "refusing existing sandbox $W" >&2; exit 1; }
    boards=("${@:2}")
    [ ${#boards[@]} -gt 0 ] || boards=(JP14 JP07 JC11)
    for board in "${boards[@]}"; do image_of "$board" > /dev/null; done
    mkdir -p "$W"
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
    python3 "$BUILDER" --qrc "$LAB/5.3.0/qrc" --out "$W/packs" > "$W/packs.log"
    arm-linux-gnueabihf-gcc -O2 -Wall -Wextra -Werror -shared -fPIC -o "$W/identity.so" "$REPO/tools/az01-packer/cc-fit-identity.c"
    fail=0
    for board in "${boards[@]}"; do
        unshare -m -n -p -f --kill-child bash "$0" "$W" --inner "$board" || fail=1
    done
    [ "$fail" = 0 ] && echo "E4SBX RESULT|PASS|${boards[*]}" || echo "E4SBX RESULT|FAIL|${boards[*]}"
    exit $fail
fi

board=${3:?board required}
read -r kind image < <(image_of "$board")
R=$W/$board-root
lower=$(echo "$board" | tr A-Z a-z)
mount --make-rprivate /
ip link set lo up
mkdir -p "$R"
mount -o loop "$W/$(basename "$image")/rootfs.ext2" "$R"
for d in tmp run dev data sys; do mount -t tmpfs tmpfs "$R/$d"; done
chroot "$R" /bin/mkdir -p /var/volatile/tmp /var/volatile/log /var/volatile/cache
mkdir -p "$R/dev/shm" "$R/dev/pts" "$R/sys/firmware/devicetree/base" "$R/sys/devices/system/cpu" "$R/run/dbus" "$R/tmp/home" "$R/data/enginex/ui/packs"
for f in online possible present; do echo 0-3 > "$R/sys/devices/system/cpu/$f"; done
for n in null zero full random urandom; do touch "$R/dev/$n"; mount --bind "/dev/$n" "$R/dev/$n"; done
printf '%s\0' "$board" > "$R/sys/firmware/devicetree/base/inmusic,product-code"
case "$kind" in
    gen1) compat="inmusic,$lower\0rockchip,rk3288" ;;
    gen2) compat="inmusic,$lower\0inmusic,az05\0rockchip,rk3288" ;;
esac
printf "$compat\0" > "$R/sys/firmware/devicetree/base/compatible"
mount -t proc -o ro proc "$R/proc"
printf '           CPU0\n 41:        100     GICv2  32 Level     ttyS0\n' > "$W/$board-interrupts"
mount --bind "$W/$board-interrupts" "$R/proc/interrupts"
mount -t tmpfs tmpfs "$R/proc/irq"
mkdir -p "$R/proc/irq/41"
printf 'f\n' > "$R/proc/irq/41/smp_affinity"
printf '0-3\n' > "$R/proc/irq/41/smp_affinity_list"
cp "$REPO/rootfs-overlay/etc/enginex/libengine-ui-hook.so" "$R/tmp/libengine-ui-hook.so"
preload=/tmp/libengine-ui-hook.so
[ "$kind" = gen2 ] && { cp "$W/identity.so" "$R/tmp/identity.so"; preload=/tmp/identity.so:/tmp/libengine-ui-hook.so; }
cp -a "$W/packs/en" "$R/data/enginex/ui/packs/en"
mkdir -p "$R/data/enginex/ui/packs/en/Skins/Common/Items"
sed "s/@BOARD@/$board/" "$REPO/tools/az01-packer/settings-click-probe.qml" > "$R/data/enginex/ui/packs/en/Skins/Common/Items/QmlWarnings.qml"
printf 'lang=en\n' > "$R/data/enginex/ui/settings.conf"

state_file="$W/$board-server-state.json"
python3 -u - "$state_file" <<'PY' > "$W/$board-http.log" 2>&1 &
import hashlib, http.server, json, socketserver, sys, threading, time, urllib.parse

state_file = sys.argv[1]
lock = threading.Lock()
configurable_sources = ["tidal", "netease", "spotify", "deezer", "soundcloud", "beatport", "applemusic"]
state = {
    "language": "en", "waveform": "denon", "quality": "flac", "neteaseMode": "official",
    "sources": list(configurable_sources),
    "loggedIn": [], "scope": "", "cueCountdown": True, "tracklistWaveform": True,
    "masterLimiter": False, "autoGain": False, "bpmDecimals": 2, "ledBrightness": 100,
    "uiScale": 100, "player": True, "rekordboxName": "Studio-Mac",
    "tunnel": {"enabled": False, "status": "stopped", "publicUrl": "", "error": ""},
}
get_seq = [0]
rev = [1]
epoch = "sbx-epoch-1"

def view_snap():
    snap = json.loads(json.dumps(state))
    # Mirror pkg/webui/ui_settings.go:999: discovered rekordbox is appended at read time, never saved.
    snap["sources"] = list(snap["sources"]) + ["rekordbox"]
    snap["loggedIn"] = list(snap["loggedIn"]) + ["rekordbox"]
    snap["rev"] = rev[0]
    snap["epoch"] = epoch
    return snap

def persist():
    with open(state_file, "w", encoding="utf-8") as f:
        json.dump(state, f, sort_keys=True)

persist()

class ThreadedHTTP(socketserver.ThreadingMixIn, http.server.HTTPServer):
    daemon_threads = True

class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def send_json(self, obj, code=200, with_digest=True):
        body = json.dumps(obj, sort_keys=True).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        if with_digest:
            self.send_header("X-UI-Digest", hashlib.md5(body).hexdigest()[:12])
            if isinstance(obj, dict) and "rev" in obj:
                self.send_header("X-UI-Rev", str(obj["rev"]))
            if isinstance(obj, dict) and "epoch" in obj:
                self.send_header("X-UI-Epoch", str(obj["epoch"]))
        self.end_headers()
        self.wfile.write(body)
    def do_GET(self):
        if self.path.startswith("/v1/enginex/views"):
            self.send_json({"active": 1, "views": {}}, with_digest=False)
            return
        with lock:
            get_seq[0] += 1
            n = get_seq[0]
            snap = view_snap()
        # Poll #2 runs right before Master Soft Clipper click; Poll #3 runs right before Remote access click.
        if n == 2:
            time.sleep(0.40)
        elif n == 3:
            time.sleep(0.20)
        self.send_json(snap)
    def do_POST(self):
        if self.path.startswith("/v1/enginex/views"):
            n = int(self.headers.get("Content-Length", "0") or 0)
            if n:
                self.rfile.read(n)
            self.send_json({"ok": True}, with_digest=False)
            return
        if self.path.startswith("/v1/enginex/tunnel"):
            n = int(self.headers.get("Content-Length", "0") or 0)
            req = json.loads(self.rfile.read(n) or b"{}")
            time.sleep(0.25)
            with lock:
                en = bool(req.get("enabled"))
                state["tunnel"] = {"enabled": en, "status": "running" if en else "stopped", "publicUrl": "https://example.invalid" if en else "", "error": ""}
                persist()
                snap = dict(state["tunnel"])
            self.send_json(snap, with_digest=False)
            return
        q = urllib.parse.parse_qs(urllib.parse.urlparse(self.path).query, keep_blank_values=True)
        if "sources" in q:
            raw = [s for s in q["sources"][0].split(",") if s]
            bad = [s for s in raw if s not in configurable_sources]
            if bad:
                self.send_json({"error": "unknown source: " + bad[0]}, code=400, with_digest=False)
                return
            if "soundcloud" not in raw and "deezer" in raw:
                time.sleep(0.20)
        delta_keys = sorted(k for k in q if k.startswith("source_"))
        for k in delta_keys:
            sid = k[len("source_"):]
            if sid not in configurable_sources:
                self.send_json({"error": "unknown source: " + sid}, code=400, with_digest=False)
                return
            if q[k][0] not in ("0", "1", "true", "false"):
                self.send_json({"error": "invalid setting"}, code=400, with_digest=False)
                return
            if sid == "soundcloud" and q[k][0] in ("0", "false"):
                time.sleep(0.20)
        if "ledBrightness" in q and q["ledBrightness"][0] == "80":
            time.sleep(0.25)
        with lock:
            if "autoGain" in q: state["autoGain"] = q["autoGain"][0] == "1"
            if "masterLimiter" in q: state["masterLimiter"] = q["masterLimiter"][0] == "1"
            if "sources" in q:
                raw = [s for s in q["sources"][0].split(",") if s]
                state["sources"] = [s for s in configurable_sources if s in raw]
            for k in delta_keys:
                sid = k[len("source_"):]
                on = q[k][0] in ("1", "true")
                cur = [s for s in state["sources"] if s != sid]
                if on:
                    cur.append(sid)
                state["sources"] = [s for s in configurable_sources if s in cur]
            if "ledBrightness" in q: state["ledBrightness"] = int(q["ledBrightness"][0])
            rev[0] += 1
            persist()
            snap = view_snap()
        self.send_json(snap)

ThreadedHTTP(("127.0.0.1", 8080), Handler).serve_forever()
PY
http_pid=$!
dbus_started=0
dbus_pid=""
cleanup() {
    local rc=$?
    local dbus_err=0
    if [ "$dbus_started" = 1 ]; then
        if [ -z "$dbus_pid" ]; then
            echo "E4SBX FAIL|$board|dbus_cleanup_pid_lost|" >&2
            dbus_err=1
        else
            kill "$dbus_pid" 2>/dev/null || true
            for _ in 1 2 3 4 5 6 7 8 9 10; do
                if [ ! -e "$R/proc/$dbus_pid/stat" ]; then break; fi
                local st
                st=$(awk '{print $3}' "$R/proc/$dbus_pid/stat" 2>/dev/null || true)
                if [ "$st" = Z ]; then break; fi
                sleep 0.05
            done
            if [ -e "$R/proc/$dbus_pid/stat" ]; then
                local st
                st=$(awk '{print $3}' "$R/proc/$dbus_pid/stat" 2>/dev/null || true)
                if [ -n "$st" ] && [ "$st" != Z ]; then
                    kill -9 "$dbus_pid" 2>/dev/null || true
                    echo "E4SBX FAIL|$board|dbus_cleanup_still_live=$dbus_pid|" >&2
                    dbus_err=1
                fi
            fi
        fi
    fi
    kill "$http_pid" 2>/dev/null || true
    wait "$http_pid" 2>/dev/null || true
    if [ "$dbus_err" != 0 ]; then
        [ "$rc" -ne 0 ] && exit "$rc"
        exit 1
    fi
    return "$rc"
}
trap cleanup EXIT

dbus_started=1
dbus_pid=$(chroot "$R" /usr/bin/dbus-daemon --system --fork --nopidfile --print-pid=1 3>&- 4>&- 2>"$W/$board-dbus.log" || true)
dbus_pid=${dbus_pid%%[!0-9]*}
if [ -z "$dbus_pid" ] || ! kill -0 "$dbus_pid" 2>/dev/null; then
    echo "E4SBX FAIL|$board|dbus_pid_unusable=$dbus_pid|" >&2
    exit 1
fi

log="$W/$board-engine.log"
timeout --kill-after=5s 45s chroot "$R" /usr/bin/env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin HOME=/tmp/home \
    LD_LIBRARY_PATH=/usr/qt/lib LD_PRELOAD="$preload" QT_QPA_PLATFORM=offscreen \
    QML_DISABLE_DISK_CACHE=1 MALLOC_ARENA_MAX=1 \
    /usr/Engine/Engine -d0 > "$log" 2>&1 || true

grep -a -o -E "E4SBX (PASS|FAIL|SUMMARY|DONE|EXC)\|.*" "$log" | sed 's/ *\[20[0-9T:.Z-]*\].*//' || true
python3 - "$state_file" "$board" <<'PY'
import json, sys
st = json.load(open(sys.argv[1], encoding="utf-8"))
board = sys.argv[2]
checks = [
    (st["autoGain"] is True, "server_autogain_true"),
    (st["masterLimiter"] is True, "server_master_limiter_true"),
    ("soundcloud" not in st["sources"] and "deezer" not in st["sources"] and "rekordbox" not in st["sources"], f"server_sources_both_off={st['sources']}"),
    (st["ledBrightness"] == 60, f"server_led_60={st['ledBrightness']}"),
    (st["tunnel"]["enabled"] is True, f"server_tunnel_on={st['tunnel']['enabled']}"),
]
bad = [name for ok, name in checks if not ok]
for ok, name in checks:
    print(f"E4SBX {'PASS' if ok else 'FAIL'}|{board}|{name}|")
if bad:
    sys.exit(1)
PY
grep -aq "E4SBX DONE|PASS|$board|" "$log"
