#!/bin/bash
# Re-render documentation screenshots for EngineX OS using eglfs KMS on the build host.
#
# Usage:
#   bash tools/az01-packer/doc-shots.sh [UPDATE_IMG] [NEXT_VERSION] [OUT_DIR]
#
# Arguments:
#   UPDATE_IMG    Path to SC5000-*-Update.img (default: /var/tmp/enginex-release-5.3.0.4/SC5000-5.3.0.4-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img)
#   NEXT_VERSION  Mock newer version for OTA screenshot (default: v5.3.0.5)
#   OUT_DIR       Repository root directory (default: repo root containing this script)
#
# Replaces:
#   - docs/site/assets/shots/shot-about-version.png (1280x800 RGBA, English, up to date)
#   - docs/site/assets/shots/shot-about-ota.png (1280x800 RGBA, English, new version available)
#   - docs/assets/screenshots/en/06-preferences-enginex-github-update.png (900x562 RGB)
#   - docs/assets/screenshots/zh-CN/06-preferences-enginex-github-update.png (900x562 RGB, Simplified Chinese)
#   - docs/assets/screenshots/zh-TW/06-preferences-enginex-github-update.png (900x562 RGB, Traditional Chinese)
#
# Requirements:
#   - Runs on the build host (aarch64 with 32-bit EL0) as root
#   - /dev/dri/card0 available (virtio-gpu)
#   - Memory limit per process <= 4G

set -euo pipefail

REPO=$(cd "$(dirname "$0")/../.." && pwd)
IMG=${1:-/var/tmp/enginex-release-5.3.0.4/SC5000-5.3.0.4-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img}
NEXTV=${2:-v5.3.0.5}
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

    # D-Bus
    mkdir -p "$R/run/dbus"
    chroot "$R" /usr/bin/dbus-daemon --system --fork --nopidfile

    # /data setup
    mkdir -p "$R/data/enginex/ui/packs"
    ln -sf enginex "$R/data/sc5000-custom"
    cp -a "$W/packs/$LANG_PACK" "$R/data/enginex/ui/packs/"
    mkdir -p "$R/data/enginex/ui/packs/$LANG_PACK/Skins/Common/Items"
    cp "$W/probe.qml" "$R/data/enginex/ui/packs/$LANG_PACK/Skins/Common/Items/QmlWarnings.qml"

    printf 'lang=%s\n' "$LANG_SETTING" > "$R/data/enginex/ui/settings.conf"
    printf '{\n    "TIDAL": "{\\"provider\\":\\"TIDAL\\",\\"providerType\\":\\"TIDAL\\",\\"accessToken\\":\\"enginex\\",\\"refreshToken\\":\\"enginex\\"}"\n}\n' > "$R/data/StreamingAccounts.json"

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

    # Run Engine
    timeout --kill-after=5s 35s chroot "$R" /usr/bin/env -i \
        PATH=/usr/bin:/bin:/usr/sbin:/sbin \
        HOME=/tmp/home \
        LD_LIBRARY_PATH=/usr/lib:/usr/qt/lib \
        LD_PRELOAD=/etc/sc5000/libengine-ui-hook.so \
        QT_QPA_PLATFORM=eglfs \
        QT_QPA_EGLFS_INTEGRATION=eglfs_kms \
        LIBGL_ALWAYS_SOFTWARE=1 \
        QML_DISABLE_DISK_CACHE=1 \
        MALLOC_ARENA_MAX=1 \
        /usr/Engine/Engine -d0 > "$W/engine-$LANG_SETTING.log" 2>&1 || true

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

CURR_VERSION=$(debugfs -R "cat /etc/sc5000/version.txt" "$IMG_EXT2" 2>/dev/null | grep -E '^[0-9]' | head -1)
echo "Detected current version in image: $CURR_VERSION (mock next: $NEXTV)"

echo "==> Building language packs..."
QRC_SRC=/root/build/sc5000-lab/5.3.0/qrc
python3 "$REPO/tools/ui-mod/build_packs.py" --qrc "$QRC_SRC" --out "$W/packs" > "$W/packs.log"

# Setup shared fixtures
(cd "$W/tls" &&
 openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj /CN=mock-ca -keyout ca.key -out ca.pem \
     -addext basicConstraints=critical,CA:true -addext keyUsage=critical,keyCertSign,cRLSign 2>/dev/null &&
 openssl req -newkey rsa:2048 -nodes -subj /CN=github.com -keyout leaf.key -out leaf.csr 2>/dev/null &&
 printf 'subjectAltName=DNS:github.com,DNS:api.github.com\nextendedKeyUsage=serverAuth\n' > leaf.ext &&
 openssl x509 -req -in leaf.csr -CA ca.pem -CAkey ca.key -CAcreateserial -days 2 -out leaf.pem -extfile leaf.ext 2>/dev/null)

printf '127.0.0.1 localhost\n127.0.0.1 api.github.com github.com\n' > "$W/hosts"
printf '           CPU0\n 41:        100     GICv2  32 Level     ttyS0\n' > "$W/interrupts"

cat > "$W/ScreenConfiguration.json" <<'JSON'
{
	"device": "/dev/dri/card0",
	"outputs": [
		{
			"name": "Virtual-1",
			"mode": "1280x800",
			"primary": true,
			"virtualIndex": 0
		}
	]
}
JSON

# Prepare probe.qml
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
                                if (d.indexOf("About") >= 0 || d.indexOf("关于") >= 0 || d.indexOf("關於") >= 0) {
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

if [ "${DOC_SHOT_SETTINGS_COPY:-0}" = 1 ]; then
    cp "$REPO/tools/az01-packer/settings-copy-shot.qml" "$W/probe.qml"
    for spec in en:en zh-Hans:zh-CN zh-Hant:zh-TW; do
        pack=${spec%%:*}; language=${spec#*:}
        unshare -m -n -p -f --kill-child bash "$0" --render-one "$W" "$IMG_EXT2" "$pack" "$pack" "v$CURR_VERSION" "$W/raw/$language.png"
    done
    python3 - "$W/raw" "$OUT_REPO" <<'PY'
from pathlib import Path
from PIL import Image
import sys
raw, repo = map(Path, sys.argv[1:])
for language in ('en', 'zh-CN', 'zh-TW'):
    out = repo/'docs/assets/screenshots'/language/'21b-settings-copy.png'
    out.parent.mkdir(parents=True, exist_ok=True)
    Image.open(raw/(language+'.png')).convert('RGB').resize((900,562), Image.Resampling.LANCZOS).save(out)
PY
    exit
fi

echo "==> Rendering 1/4: English (up-to-date)..."
unshare -m -n -p -f --kill-child bash "$0" --render-one "$W" "$IMG_EXT2" en en "v$CURR_VERSION" "$W/raw/shot-about-version.png"

echo "==> Rendering 2/4: English (OTA available)..."
unshare -m -n -p -f --kill-child bash "$0" --render-one "$W" "$IMG_EXT2" en en "$NEXTV" "$W/raw/shot-about-ota.png"

echo "==> Rendering 3/4: Simplified Chinese (zh-Hans OTA)..."
unshare -m -n -p -f --kill-child bash "$0" --render-one "$W" "$IMG_EXT2" zh-Hans zh-Hans "$NEXTV" "$W/raw/shot-zh-cn.png"

echo "==> Rendering 4/4: Traditional Chinese (zh-Hant OTA)..."
unshare -m -n -p -f --kill-child bash "$0" --render-one "$W" "$IMG_EXT2" zh-Hant zh-Hant "$NEXTV" "$W/raw/shot-zh-tw.png"

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
save_image(os.path.join(raw_dir, 'shot-about-version.png'),
           os.path.join(repo_root, 'docs/site/assets/shots/shot-about-version.png'),
           size=(1280, 800), mode='RGBA')

# 2. shot-about-ota.png (1280x800 RGBA)
save_image(os.path.join(raw_dir, 'shot-about-ota.png'),
           os.path.join(repo_root, 'docs/site/assets/shots/shot-about-ota.png'),
           size=(1280, 800), mode='RGBA')

# 3. 06-preferences-enginex-github-update.png (en, 900x562 RGB)
save_image(os.path.join(raw_dir, 'shot-about-ota.png'),
           os.path.join(repo_root, 'docs/assets/screenshots/en/06-preferences-enginex-github-update.png'),
           size=(900, 562), mode='RGB')

# 4. 06-preferences-enginex-github-update.png (zh-CN, 900x562 RGB)
save_image(os.path.join(raw_dir, 'shot-zh-cn.png'),
           os.path.join(repo_root, 'docs/assets/screenshots/zh-CN/06-preferences-enginex-github-update.png'),
           size=(900, 562), mode='RGB')

# 5. 06-preferences-enginex-github-update.png (zh-TW, 900x562 RGB)
save_image(os.path.join(raw_dir, 'shot-zh-tw.png'),
           os.path.join(repo_root, 'docs/assets/screenshots/zh-TW/06-preferences-enginex-github-update.png'),
           size=(900, 562), mode='RGB')
PY

echo "==> All 5 screenshots successfully rendered and saved!"
