#!/bin/bash
# Re-render documentation screenshots for EngineX OS using eglfs KMS on the build host.
#
# Usage:
#   bash tools/az01-packer/doc-shots.sh [UPDATE_IMG] [NEXT_VERSION] [OUT_DIR]
#
# Modes:
#   DOC_SHOT_ALL=1             Render all documentation screenshots (Console, Settings pages, Copy, About)
#   DOC_SHOT_CONSOLE=1         Render Web Console screenshots (14, 15) in 3 languages
#   DOC_SHOT_SETTINGS_PAGES=1  Render Settings -> Device (06b) and Services (13) in 3 languages
#   DOC_SHOT_SETTINGS_COPY=1   Render Settings -> Copy (21b) in 3 languages
#   DOC_SHOT_PROFILES=1        Render Profile -> Playback (16) and Display (17) in 3 languages
#   DOC_SHOT_USB_PROFILE=1     Render the Load drive profile prompt (21a) in 3 languages
#   (default)                  Render About & Update screenshots (06, 06c, shot-about-*)
#
# Arguments:
#   UPDATE_IMG    Path to SC5000-*-Update.img
#   NEXT_VERSION  Mock newer version for OTA screenshot (default: v5.3.0.6)
#   OUT_DIR       Repository root directory (default: repo root containing this script)
#
# Requirements:
#   - Runs on the build host (aarch64 with 32-bit EL0) as root
#   - /dev/dri/card0 available (virtio-gpu), or set DOC_SHOT_DRM_CARD (for example card1 on Batam)
#   - Memory limit per process <= 4G

set -euo pipefail

REPO=$(cd "$(dirname "$0")/../.." && pwd)
mapfile -t LEGACY_INPUTS < <(PYTHONPATH="$REPO/tools/az01-packer" python3 -c \
    'import legacy_names as legacy; print(legacy.DATA_DIR); print(legacy.IMAGE_DIR); print(legacy.LAB)')
DEFAULT_IMG="/var/tmp/enginex-rc-5305-3e4c7a3/SC5000-5.3.0.5-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img"
[ -f "$DEFAULT_IMG" ] || DEFAULT_IMG="/var/tmp/enginex-dryrun-5305/SC5000-5.3.0.5-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img"
IMG=${1:-$DEFAULT_IMG}
NEXTV=${2:-v5.3.0.6}
OUT_REPO=${3:-$REPO}

if [ "${1:-}" = "--render-one" ]; then
    # Internal worker in private mount namespace
    W=$2
    IMG_EXT2=$3
    LANG_PACK=$4
    LANG_SETTING=$5
    MOCK_TAG=$6
    OUT_PNG=$7

    mount --make-rprivate /
    ip link set lo up

    # 1. Start mock GitHub release server
    cat > "$W/mock/release.json" <<JSON
{
  "tag_name": "$MOCK_TAG",
  "html_url": "https://github.com/Cd1s/EngineX-OS/releases/tag/$MOCK_TAG",
  "assets": [
    {
      "name": "SC5000-5.3.0.5-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img",
      "browser_download_url": "https://github.com/Cd1s/EngineX-OS/releases/download/$MOCK_TAG/SC5000-5.3.0.5-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img"
    }
  ]
}
JSON

    python3 -I - <<'PY' "$W/mock" "$W/tls/leaf.pem" "$W/tls/leaf.key" 2> "$W/mock.log" &
import http.server, os, ssl, sys
root = sys.argv[1]
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path.endswith('/releases/latest'):
            data = open(os.path.join(root, 'release.json'), 'rb').read()
            self.send_response(200); self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(data))); self.end_headers()
            self.wfile.write(data)
            return
        self.send_error(404)
    def log_message(self, fmt, *args): pass
srv = http.server.ThreadingHTTPServer(('127.0.0.1', 443), H)
ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
ctx.load_cert_chain(sys.argv[2], sys.argv[3])
srv.socket = ctx.wrap_socket(srv.socket, server_side=True)
srv.serve_forever()
PY
    MOCK_PID=$!
    trap 'kill "$MOCK_PID" 2>/dev/null || true' EXIT

    # 2. Mount rootfs
    R=$W/root
    rm -rf "$R"
    mkdir -p "$R"
    mount -o loop,ro "$IMG_EXT2" "$R"

    for d in tmp run dev data sys; do mount -t tmpfs tmpfs "$R/$d"; done
    mount -t tmpfs tmpfs "$R/var"
    mkdir -p "$R/var/volatile/tmp" "$R/var/volatile/log" "$R/var/volatile/cache" "$R/var/lib/dbus"
    ln -sf volatile/tmp "$R/var/tmp"
    ln -sf volatile/log "$R/var/log"
    ln -sf ../run "$R/var/run"
    ln -sf ../run/lock "$R/var/lock"
    printf "1234567890abcdef1234567890abcdef\n" > "$R/var/lib/dbus/machine-id"

    # Dev nodes & DRI
    mkdir -p "$R/dev/dri" "$R/dev/shm" "$R/dev/pts"
    for n in null zero full random urandom; do touch "$R/dev/$n"; mount --bind "/dev/$n" "$R/dev/$n"; done
    mount --bind /dev/dri "$R/dev/dri"

    # Sysfs
    for d in class devices bus; do
        mkdir -p "$R/sys/$d"
        mount --bind "/sys/$d" "$R/sys/$d"
        mount -o remount,bind,ro "$R/sys/$d"
    done
    mkdir -p "$R/sys/firmware/devicetree/base"
    printf JP07 > "$R/sys/firmware/devicetree/base/inmusic,product-code"
    printf 'inmusic,jp07\0rockchip,rk3288\0' > "$R/sys/firmware/devicetree/base/compatible"

    # Procfs & interrupts
    mount -t proc -o ro proc "$R/proc"
    mount --bind "$W/interrupts" "$R/proc/interrupts"
    mount -t tmpfs tmpfs "$R/proc/irq"
    mkdir -p "$R/proc/irq/41"
    printf 'f\n' > "$R/proc/irq/41/smp_affinity"
    printf '0-3\n' > "$R/proc/irq/41/smp_affinity_list"

    # Network / hosts & TLS trust
    mount --bind "$W/hosts" "$R/etc/hosts"
    bundle=$R$(chroot "$R" /bin/readlink -f /etc/ssl/certs/ca-certificates.crt)
    cat "$bundle" "$W/tls/ca.pem" > "$W/ca.crt"
    mount --bind "$W/ca.crt" "$bundle"

    # ScreenConfiguration.json binding
    mount --bind "$W/ScreenConfiguration.json" "$R/usr/Engine/ScreenConfiguration/Default/ScreenConfiguration.json"
    if [ -d "$R/usr/Engine/ScreenConfiguration/JP07" ]; then
        touch "$R/usr/Engine/ScreenConfiguration/JP07/ScreenConfiguration.json" 2>/dev/null || true
        mount --bind "$W/ScreenConfiguration.json" "$R/usr/Engine/ScreenConfiguration/JP07/ScreenConfiguration.json" || true
    fi

    # D-Bus
    mkdir -p "$R/run/dbus"
    chroot "$R" /usr/bin/dbus-daemon --system --fork --nopidfile

    # /data setup
    mkdir -p "$R/data/enginex/ui/packs"
    ln -sf enginex "$R${LEGACY_INPUTS[0]}"
    cp -a "$W/packs/$LANG_PACK" "$R/data/enginex/ui/packs/"
    if [ -f "$W/probe.qml" ]; then
        mkdir -p "$R/data/enginex/ui/packs/$LANG_PACK/Skins/Common/Items"
        cp "$W/probe.qml" "$R/data/enginex/ui/packs/$LANG_PACK/Skins/Common/Items/QmlWarnings.qml"
    fi

    printf 'lang=%s\n' "$LANG_SETTING" > "$R/data/enginex/ui/settings.conf"
    printf '{\n    "TIDAL": "{\\"provider\\":\\"TIDAL\\",\\"providerType\\":\\"TIDAL\\",\\"accessToken\\":\\"enginex\\",\\"refreshToken\\":\\"enginex\\"}"\n}\n' > "$R/data/StreamingAccounts.json"

    # Console shots: serve the console of the suite built from this checkout.
    if [ -n "${DOC_SHOT_SUITE:-}" ]; then
        mount --bind "$DOC_SHOT_SUITE" "$R/usr/local/bin/stream-sync"
    fi

    # Start stream-sync
    mkdir -p "$R/tmp/home"
    chroot "$R" /usr/local/bin/stream-sync --listen :8080 --cpu 3 > "$W/stream-sync.log" 2>&1 &
    SYNC_PID=$!

    # Wait for stream-sync to be ready
    for i in $(seq 1 40); do
        if curl -sf http://127.0.0.1:8080/v1/enginex/ui >/dev/null 2>&1; then
            break
        fi
        sleep 0.1
    done

    if [ -n "${DOC_SHOT_CONSOLE_LANG:-}" ]; then
        # A tiny proxy adds the console access code (read from the sandbox, never printed);
        # headless Chromium shares this network namespace and shoots a tall window.
        TOKEN_FILE=$(find "$R/tmp/home" "$R/root" "$R/data" -maxdepth 4 -name access-token 2>/dev/null | head -1)
        [ -n "$TOKEN_FILE" ] || { echo "FAIL: no console access code in the sandbox" >&2; exit 1; }
        python3 -I - "$TOKEN_FILE" <<'PY' 2> "$W/console-proxy.log" &
import http.client, http.server, socketserver, sys
code = open(sys.argv[1], encoding="utf-8").read().strip()
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        c = http.client.HTTPConnection("127.0.0.1", 8080, timeout=20)
        c.request("GET", self.path, headers={"Cookie": "stream_sync=" + code})
        r = c.getresponse(); body = r.read()
        self.send_response(r.status)
        for k, v in r.getheaders():
            if k.lower() not in ("transfer-encoding", "connection", "content-length", "set-cookie"):
                self.send_header(k, v)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers(); self.wfile.write(body)
    do_HEAD = do_GET
    def log_message(self, *a): pass
class S(socketserver.ThreadingMixIn, http.server.HTTPServer):
    daemon_threads = True
S(("127.0.0.1", 18080), H).serve_forever()
PY
        PROXY_PID=$!
        sleep 1
        timeout 90 chromium-headless-shell --no-sandbox --disable-gpu --hide-scrollbars             --user-data-dir="$W/chrome-$DOC_SHOT_CONSOLE_LANG" --force-device-scale-factor=2             --window-size=1280,3600 --virtual-time-budget=12000             --screenshot="$OUT_PNG" "http://127.0.0.1:18080/?lang=$DOC_SHOT_CONSOLE_LANG" > "$W/chromium.log" 2>&1 || true
        kill "$PROXY_PID" "$SYNC_PID" "$MOCK_PID" 2>/dev/null || true
        trap - EXIT
        [ -s "$OUT_PNG" ] || { echo "FAIL: $OUT_PNG not generated" >&2; exit 1; }
        echo "PASS: $OUT_PNG rendered successfully"
        exit 0
    fi

    # Run Engine
    timeout --kill-after=5s 35s chroot "$R" /usr/bin/env -i         PATH=/usr/bin:/bin:/usr/sbin:/sbin         HOME=/tmp/home         LD_LIBRARY_PATH=/usr/lib:/usr/qt/lib         LD_PRELOAD=/etc/enginex/libengine-ui-hook.so         QT_QPA_PLATFORM=eglfs         QT_QPA_EGLFS_INTEGRATION=eglfs_kms         LIBGL_ALWAYS_SOFTWARE=1         QML_DISABLE_DISK_CACHE=1         MALLOC_ARENA_MAX=1         /usr/Engine/Engine -d0 > "$W/engine-$LANG_SETTING.log" 2>&1 || true

    kill "$SYNC_PID" 2>/dev/null || true
    kill "$MOCK_PID" 2>/dev/null || true
    trap - EXIT

    if [ -f "$R/tmp/shot.png" ]; then
        cp "$R/tmp/shot.png" "$OUT_PNG"
        echo "PASS: $OUT_PNG rendered successfully"
    else
        echo "FAIL: $OUT_PNG not generated" >&2
        exit 1
    fi
    exit 0
fi

# Main host entrypoint
[ "$(uname -m)" = "aarch64" ] || { echo "Error: an aarch64 build host is required" >&2; exit 1; }
[ -e "$IMG" ] || { echo "Error: image file $IMG not found" >&2; exit 1; }

W=/var/tmp/doc-shots-work-$$
mkdir -p "$W/tls" "$W/mock" "$W/packs" "$W/raw"
trap 'rm -rf "$W" 2>/dev/null || true' EXIT

echo "==> Unpacking image $IMG..."
python3 "$REPO/tools/az01-packer/az01_packer.py" unpack "$IMG" --output "$W/image" > "$W/unpack.json"
IMG_EXT2=$W/image/rootfs.ext2
[ -f "$IMG_EXT2" ] || { echo "Error: unpacked rootfs.ext2 not found" >&2; exit 1; }

# /etc/enginex since 5.3.0.5 (#234); use the migration reader for older images.
CURR_VERSION=$(debugfs -R "cat /etc/enginex/version.txt" "$IMG_EXT2" 2>/dev/null | grep -E '^[0-9]' | head -1 || true)
[ -n "$CURR_VERSION" ] || CURR_VERSION=$(debugfs -R "cat ${LEGACY_INPUTS[1]}/version.txt" "$IMG_EXT2" 2>/dev/null | grep -E '^[0-9]' | head -1 || true)
echo "Detected current version in image: $CURR_VERSION (mock next: $NEXTV)"

echo "==> Building language packs..."
QRC_SRC="${LEGACY_INPUTS[2]}/5.3.0/qrc"
python3 "$REPO/tools/ui-mod/build_packs.py" --release --qrc "$QRC_SRC" --out "$W/packs" > "$W/packs.log"

# Setup shared fixtures
(cd "$W/tls" &&
 openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj /CN=mock-ca -keyout ca.key -out ca.pem      -addext basicConstraints=critical,CA:true -addext keyUsage=critical,keyCertSign,cRLSign 2>/dev/null &&
 openssl req -newkey rsa:2048 -nodes -subj /CN=github.com -keyout leaf.key -out leaf.csr 2>/dev/null &&
 printf 'subjectAltName=DNS:github.com,DNS:api.github.com
extendedKeyUsage=serverAuth
' > leaf.ext &&
 openssl x509 -req -in leaf.csr -CA ca.pem -CAkey ca.key -CAcreateserial -days 2 -out leaf.pem -extfile leaf.ext 2>/dev/null)

printf '127.0.0.1 localhost
127.0.0.1 api.github.com github.com
' > "$W/hosts"
printf '           CPU0
 41:        100     GICv2  32 Level     ttyS0
' > "$W/interrupts"

DRM_CARD=${DOC_SHOT_DRM_CARD:-card0}
SCREEN_WIDTH=${DOC_SHOT_SCREEN_WIDTH:-1280}
SCREEN_HEIGHT=${DOC_SHOT_SCREEN_HEIGHT:-800}
cat > "$W/ScreenConfiguration.json" <<JSON
{
	"device": "/dev/dri/$DRM_CARD",
	"headless": "${SCREEN_WIDTH}x${SCREEN_HEIGHT}",
	"hwcursor": false
}
JSON

if [ -z "${DOC_SHOT_SUITE:-}" ] && [ -x /var/tmp/enginex-suite-5305 ]; then
    export DOC_SHOT_SUITE=/var/tmp/enginex-suite-5305
fi

# Web console (14, 15) in the three languages. DOC_SHOT_SUITE is the enginex-suite built from this
# checkout (GOARCH=arm), so the page is the one this release ships.
if [ "${DOC_SHOT_ALL:-0}" = 1 ] || [ "${DOC_SHOT_CONSOLE:-0}" = 1 ]; then
    : "${DOC_SHOT_SUITE:?set DOC_SHOT_SUITE to the enginex-suite binary built from this checkout}"
    export DOC_SHOT_SUITE
    echo "==> Rendering Web Console screenshots (14, 15)..."
    for spec in en:en zh-Hans:zh-CN zh-Hant:zh-TW; do
        pack=${spec%%:*}; language=${spec#*:}
        DOC_SHOT_CONSOLE_LANG=$pack unshare -m -n -p -f --kill-child bash "$0" --render-one "$W" "$IMG_EXT2" "$pack" "$pack" "v$CURR_VERSION" "$W/raw/$language-console.png"
    done
    [ -z "${DOC_SHOT_RAW_DIR:-}" ] || { mkdir -p "$DOC_SHOT_RAW_DIR"; cp "$W"/raw/*-console.png "$DOC_SHOT_RAW_DIR"/; }
    python3 - "$W/raw" "$OUT_REPO" <<'PY'
from pathlib import Path
from PIL import Image
import sys
raw, repo = map(Path, sys.argv[1:])
for language in ('en', 'zh-CN', 'zh-TW'):
    img = Image.open(raw/f'{language}-console.png').convert('RGB')
    out = repo/'docs/assets/screenshots'/language
    out.mkdir(parents=True, exist_ok=True)
    img.crop((0, 0, 2560, 2700)).save(out/'14-webui-console.png')
    img.crop((0, 1780, 2560, 5060)).save(out/'15-webui-wifi-drop.png')
    print(f"Updated: {out}/14-webui-console.png and 15-webui-wifi-drop.png")
PY
    [ "${DOC_SHOT_ALL:-0}" = 1 ] || exit 0
fi

# Settings -> Device and Settings -> Services in the three languages (06b, 13).
if [ "${DOC_SHOT_ALL:-0}" = 1 ] || [ "${DOC_SHOT_SETTINGS_PAGES:-0}" = 1 ]; then
    echo "==> Rendering Settings pages screenshots (06b, 13)..."
    for spec in en:en zh-Hans:zh-CN zh-Hant:zh-TW; do
        pack=${spec%%:*}; language=${spec#*:}
        for page in device services; do
            if [ "$page" = device ]; then
                tabs='["Device", "设备", "裝置"]'; rows='["High-speed SD card", "高速 SD 卡模式"]'
            else
                tabs='["Services", "服务", "服務"]'; rows='["Remote Load (LAN API)", "远程加载（局域网 API）", "遠端載入（區域網路 API）"]'
            fi
            python3 -I - "$REPO/tools/az01-packer/settings-pages-shot.qml" "$W/probe.qml" "$tabs" "$rows" <<'PY'
import sys
src, dst, tabs, rows = sys.argv[1:]
open(dst, "w", encoding="utf-8").write(open(src, encoding="utf-8").read().replace("@TABS@", tabs).replace("@ROWS@", rows))
PY
            unshare -m -n -p -f --kill-child bash "$0" --render-one "$W" "$IMG_EXT2" "$pack" "$pack" "v$CURR_VERSION" "$W/raw/$language-$page.png"
        done
    done
    python3 - "$W/raw" "$OUT_REPO" <<'PY'
from pathlib import Path
from PIL import Image
import sys
raw, repo = map(Path, sys.argv[1:])
for language in ('en', 'zh-CN', 'zh-TW'):
    for page, name in (('device', '06b-preferences-device.png'), ('services', '13-preferences-services.png')):
        out = repo/'docs/assets/screenshots'/language/name
        out.parent.mkdir(parents=True, exist_ok=True)
        Image.open(raw/f'{language}-{page}.png').convert('RGB').save(out)
        print(f"Updated: {out}")
PY
    [ "${DOC_SHOT_ALL:-0}" = 1 ] || exit 0
fi

if [ "${DOC_SHOT_ALL:-0}" = 1 ] || [ "${DOC_SHOT_SETTINGS_COPY:-0}" = 1 ]; then
    echo "==> Rendering Settings Copy screenshots (21b)..."
    cp "$REPO/tools/az01-packer/settings-copy-shot.qml" "$W/probe.qml"
    for spec in en:en zh-Hans:zh-CN zh-Hant:zh-TW; do
        pack=${spec%%:*}; language=${spec#*:}
        unshare -m -n -p -f --kill-child bash "$0" --render-one "$W" "$IMG_EXT2" "$pack" "$pack" "v$CURR_VERSION" "$W/raw/$language-copy.png"
    done
    python3 - "$W/raw" "$OUT_REPO" <<'PY'
from pathlib import Path
from PIL import Image
import sys
raw, repo = map(Path, sys.argv[1:])
for language in ('en', 'zh-CN', 'zh-TW'):
    out = repo/'docs/assets/screenshots'/language/'21b-settings-copy.png'
    out.parent.mkdir(parents=True, exist_ok=True)
    Image.open(raw/(language+'-copy.png')).convert('RGB').resize((900,562), Image.Resampling.LANCZOS).save(out)
    print(f"Updated: {out}")
PY
    [ "${DOC_SHOT_ALL:-0}" = 1 ] || exit 0
fi

if [ "${DOC_SHOT_PROFILES:-0}" = 1 ]; then
    echo "==> Rendering Profile pages screenshots (16, 17)..."
    for page in playback display; do
        index=0
        [ "$page" = display ] && index=2
        cat > "$W/probe.qml" <<QML
import QtQuick 2.12
import QtQuick.Window 2.2
import Planck 1.0
Item {
    id: probe
    visible: false
    property int tick: 0
    property int opened: 0
    property int selected: 0
    function walk(o, f) {
        if (!o) return
        f(o)
        if (o.children) for (let i = 0; i < o.children.length; i++) walk(o.children[i], f)
    }
    Timer {
        interval: 1000; repeat: true; running: true
        onTriggered: {
            probe.tick++
            const win = probe.Window.window ? probe.Window.window.contentItem : null
            if (!win) return
            if (PlanckGUI.views.outOfBoxUpdateScreenShowing) PlanckGUI.views.outOfBoxUpdateScreenShowing = false
            if (PlanckGUI.views.onBoardingController) PlanckGUI.views.onBoardingController.close()
            PlanckGUI.views.prompts.drop("StemsDownload")
            Planck.getProperty("/GUI/CloseDialogs").translator.state = true
            probe.walk(win, function(o) {
                if (o.objectName === "DismissButton" && typeof o.clicked === "function") o.clicked()
                if (o.objectName === "NextButton" && typeof o.clicked === "function") o.clicked()
            })
            if (!probe.opened) {
                try { PlanckGUI.views.controlCenter.openTab("Profile") } catch (e) {}
            }
            if (!probe.selected && probe.tick >= 2) {
                probe.walk(win, function(o) {
                    if (o.objectName === "ControlCenter_SubTabBar" && o.visible) {
                        o.currentIndex = $index
                        probe.opened = probe.tick
                        probe.selected = probe.tick
                    }
                })
            }
            if (probe.selected && probe.tick >= probe.selected + 3) {
                probe.walk(win, function(o) {
                    if (typeof o.contentY === "number" && typeof o.originY === "number" && o.visible && o.height > 300) {
                        o.contentY = o.originY
                    }
                })
                win.grabToImage(function(result) { result.saveToFile("/tmp/shot.png"); Qt.quit() })
            }
        }
    }
}
QML
        for spec in en:en zh-Hans:zh-CN zh-Hant:zh-TW; do
            pack=${spec%%:*}; language=${spec#*:}
            unshare -m -n -p -f --kill-child bash "$0" --render-one "$W" "$IMG_EXT2" "$pack" "$pack" "v$CURR_VERSION" "$W/raw/$language-profile-$page.png"
        done
    done
    python3 - "$W/raw" "$OUT_REPO" <<'PY'
from pathlib import Path
from PIL import Image
import sys
raw, repo = map(Path, sys.argv[1:])
for language in ('en', 'zh-CN', 'zh-TW'):
    for page, name in (('playback', '16-profile-playback.png'), ('display', '17-profile-display.png')):
        out = repo/'docs/assets/screenshots'/language/name
        out.parent.mkdir(parents=True, exist_ok=True)
        Image.open(raw/f'{language}-profile-{page}.png').convert('RGB').save(out)
        print(f"Updated: {out}")
PY
    exit 0
fi

if [ "${DOC_SHOT_USB_PROFILE:-0}" = 1 ]; then
    echo "==> Rendering USB profile import prompt (21a)..."
    cat > "$W/probe.qml" <<'QML'
import QtQuick 2.12
import QtQuick.Window 2.2
import Planck 1.0
Item {
    id: probe
    visible: false
    property int tick: 0
    property bool posted: false
    function walk(o, f) {
        if (!o) return
        f(o)
        if (o.children) for (let i = 0; i < o.children.length; i++) walk(o.children[i], f)
    }
    Timer {
        interval: 1000; repeat: true; running: true
        onTriggered: {
            probe.tick++
            const win = probe.Window.window ? probe.Window.window.contentItem : null
            if (!win) return
            if (PlanckGUI.views.outOfBoxUpdateScreenShowing) PlanckGUI.views.outOfBoxUpdateScreenShowing = false
            if (PlanckGUI.views.onBoardingController) PlanckGUI.views.onBoardingController.close()
            PlanckGUI.views.prompts.drop("StemsDownload")
            Planck.getProperty("/GUI/CloseDialogs").translator.state = true
            if (!probe.posted && probe.tick >= 4) {
                const p = Planck.getProperty("/Client/Preferences/Profile/ShowLoadDialog")
                if (p && p.translator) {
                    p.translator.state = true
                    probe.posted = true
                }
            }
            if (probe.posted && probe.tick >= 8)
                win.grabToImage(function(result) { result.saveToFile("/tmp/shot.png"); Qt.quit() })
        }
    }
}
QML
    for spec in en:en zh-Hans:zh-CN zh-Hant:zh-TW; do
        pack=${spec%%:*}; language=${spec#*:}
        DOC_SHOT_SCREEN_WIDTH=800 DOC_SHOT_SCREEN_HEIGHT=800 unshare -m -n -p -f --kill-child bash "$0" --render-one "$W" "$IMG_EXT2" "$pack" "$pack" "v$CURR_VERSION" "$W/raw/$language-usb-profile.png"
    done
    python3 - "$W/raw" "$OUT_REPO" <<'PY'
from pathlib import Path
from PIL import Image
import sys
raw, repo = map(Path, sys.argv[1:])
for language in ('en', 'zh-CN', 'zh-TW'):
    out = repo/'docs/assets/screenshots'/language/'21a-usb-profile-import.png'
    out.parent.mkdir(parents=True, exist_ok=True)
    Image.open(raw/f'{language}-usb-profile.png').convert('RGB').save(out)
    print(f"Updated: {out}")
PY
    exit 0
fi

if [ "${DOC_SHOT_ALL:-0}" = 1 ] || [ "${DOC_SHOT_CONSOLE:-0}" = 0 -a "${DOC_SHOT_SETTINGS_PAGES:-0}" = 0 -a "${DOC_SHOT_SETTINGS_COPY:-0}" = 0 ]; then
    echo "==> Rendering About & Update screenshots (06, 06c, shot-about-*)..."
    cat > "$W/probe.qml" <<'QML'
import QtQuick 2.12
import QtQuick.Window 2.2
import Planck 1.0

Item {
    id: probe
    visible: false

    Timer {
        id: runTimer
        interval: 1000
        repeat: true
        running: true
        property int tick: 0
        property int switchedAtTick: 0
        property bool shotTaken: false

        onTriggered: {
            tick++
            try {
                if (PlanckGUI.views.outOfBoxUpdateScreenShowing) PlanckGUI.views.outOfBoxUpdateScreenShowing = false
                if (PlanckGUI.views.onBoardingController) {
                    PlanckGUI.views.onBoardingController.close()
                    if (typeof PlanckGUI.views.onBoardingController.onIgnoreOnBoarding === "function") {
                        PlanckGUI.views.onBoardingController.onIgnoreOnBoarding()
                    }
                }
                if (PlanckGUI.views.prompts && typeof PlanckGUI.views.prompts.drop === "function") {
                    PlanckGUI.views.prompts.drop("StemsDownload")
                }
                if (PlanckGUI.views.sourceScreenView && typeof PlanckGUI.views.sourceScreenView.close === "function") {
                    PlanckGUI.views.sourceScreenView.close()
                }
                let pClose = Planck.getProperty("/GUI/CloseDialogs")
                if (pClose && pClose.translator) pClose.translator.state = true
                let pWifi = Planck.getProperty("/GUI/ShowWifiView")
                if (pWifi && pWifi.translator) pWifi.translator.state = false

                let win = probe.Window.window ? probe.Window.window.contentItem : null
                if (!win) return

                function dismissPopups(o) {
                    if (!o) return
                    if (o.objectName === "DismissButton" && typeof o.clicked === "function") o.clicked()
                    if (o.objectName === "NextButton" && typeof o.clicked === "function") o.clicked()
                    if (o.view && typeof o.view.onDismissed === "function") {
                        if (o.notifier && o.notifier.close) o.notifier.close()
                        o.view.onDismissed()
                    }
                    if (o.children) {
                        for (let i = 0; i < o.children.length; i++) dismissPopups(o.children[i])
                    }
                }
                dismissPopups(win)

                try {
                    PlanckGUI.views.controlCenter.openTab(ControlCenterTabNameSettings)
                } catch (e) {}

                function switchVisibleSubTab(o) {
                    if (!o || switchedAtTick > 0) return
                    if (o.objectName === "ControlCenter_SubTabBar" && o.visible) {
                        let tv = o.parent
                        if (tv && tv.tabs && tv.tabs.count > 0) {
                            let targetIdx = tv.tabs.count - 1
                            for (let i = 0; i < tv.tabs.count; i++) {
                                let t = tv.tabs.get(i)
                                let d = t ? t.display : ""
                                if (d.indexOf("About") >= 0 || d.indexOf("关于") >= 0 || d.indexOf("關於") >= 0 || d.indexOf("Acerca") >= 0) {
                                    targetIdx = i
                                    break
                                }
                            }
                            o.currentIndex = targetIdx
                            switchedAtTick = tick
                            return
                        }
                    }
                    if (o.children) {
                        for (let i = 0; i < o.children.length; i++) switchVisibleSubTab(o.children[i])
                    }
                }
                if (switchedAtTick === 0) switchVisibleSubTab(win)

                function triggerUpdate(o) {
                    if (!o) return
                    if (typeof o.scCheckUpdate === "function") o.scCheckUpdate()
                    if (o.children) {
                        for (let i = 0; i < o.children.length; i++) triggerUpdate(o.children[i])
                    }
                }
                if (switchedAtTick > 0 && tick === switchedAtTick + 1) triggerUpdate(win)

                // Wait 4 seconds after tab switch for update check & UI settling
                if (switchedAtTick > 0 && (tick - switchedAtTick >= 4) && !shotTaken) {
                    shotTaken = true
                    runTimer.running = false
                    win.grabToImage(function(result) {
                        result.saveToFile("/tmp/shot.png")
                        Qt.quit()
                    })
                }
            } catch (err) {}
        }
    }
}
QML

    echo "==> Rendering English (up-to-date & OTA)..."
    unshare -m -n -p -f --kill-child bash "$0" --render-one "$W" "$IMG_EXT2" en en "v$CURR_VERSION" "$W/raw/en-about-version.png"
    unshare -m -n -p -f --kill-child bash "$0" --render-one "$W" "$IMG_EXT2" en en "$NEXTV" "$W/raw/en-about-ota.png"

    echo "==> Rendering Simplified Chinese (up-to-date & OTA)..."
    unshare -m -n -p -f --kill-child bash "$0" --render-one "$W" "$IMG_EXT2" zh-Hans zh-Hans "v$CURR_VERSION" "$W/raw/zh-CN-about-version.png"
    unshare -m -n -p -f --kill-child bash "$0" --render-one "$W" "$IMG_EXT2" zh-Hans zh-Hans "$NEXTV" "$W/raw/zh-CN-about-ota.png"

    echo "==> Rendering Traditional Chinese (up-to-date & OTA)..."
    unshare -m -n -p -f --kill-child bash "$0" --render-one "$W" "$IMG_EXT2" zh-Hant zh-Hant "v$CURR_VERSION" "$W/raw/zh-TW-about-version.png"
    unshare -m -n -p -f --kill-child bash "$0" --render-one "$W" "$IMG_EXT2" zh-Hant zh-Hant "$NEXTV" "$W/raw/zh-TW-about-ota.png"

    echo "==> Resizing and saving screenshots to repository..."
    python3 - <<PY "$W/raw" "$OUT_REPO"
import os, sys
from PIL import Image

raw_dir = sys.argv[1]
repo_root = sys.argv[2]

def save_image(src, dst, size=None, mode='RGB'):
    im = Image.open(src)
    if mode == 'RGB' and im.mode == 'RGBA':
        bg = Image.new('RGB', im.size, (0, 0, 0))
        bg.paste(im, mask=im.split()[3])
        im = bg
    elif mode == 'RGBA' and im.mode != 'RGBA':
        im = im.convert('RGBA')
    elif mode == 'RGB' and im.mode != 'RGB':
        im = im.convert('RGB')
    if size is not None and im.size != size:
        im = im.resize(size, Image.Resampling.LANCZOS)
    os.makedirs(os.path.dirname(dst), exist_ok=True)
    im.save(dst, format='PNG', optimize=True)
    print(f"Updated: {dst} ({im.size[0]}x{im.size[1]} {im.mode})")

# 1. shot-about-version.png (1280x800 RGBA)
save_image(os.path.join(raw_dir, 'en-about-version.png'),
           os.path.join(repo_root, 'docs/site/assets/shots/shot-about-version.png'),
           size=(1280, 800), mode='RGBA')

# 2. shot-about-ota.png (1280x800 RGBA)
save_image(os.path.join(raw_dir, 'en-about-ota.png'),
           os.path.join(repo_root, 'docs/site/assets/shots/shot-about-ota.png'),
           size=(1280, 800), mode='RGBA')

# 3. 06c-about-version.png (en, zh-CN, zh-TW, 1280x800 RGB)
for lang, fname in (('en', 'en-about-version.png'),
                    ('zh-CN', 'zh-CN-about-version.png'),
                    ('zh-TW', 'zh-TW-about-version.png')):
    save_image(os.path.join(raw_dir, fname),
               os.path.join(repo_root, f'docs/assets/screenshots/{lang}/06c-about-version.png'),
               size=(1280, 800), mode='RGB')

# 4. 06-preferences-enginex-github-update.png (en, zh-CN, zh-TW, 1280x800 RGB)
for lang, fname in (('en', 'en-about-ota.png'),
                    ('zh-CN', 'zh-CN-about-ota.png'),
                    ('zh-TW', 'zh-TW-about-ota.png')):
    save_image(os.path.join(raw_dir, fname),
               os.path.join(repo_root, f'docs/assets/screenshots/{lang}/06-preferences-enginex-github-update.png'),
               size=(1280, 800), mode='RGB')
PY
    echo "==> All About & Update screenshots successfully rendered and saved!"
fi
