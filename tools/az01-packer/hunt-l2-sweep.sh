#!/bin/bash
# L2 UI render sweep runner
set -euo pipefail

REPO=$(cd "$(dirname "$0")/../.." && pwd)
W=/var/tmp/hunt-L2-work
OUT_DIR=/var/tmp/hunt-L2
IMG_EXT2=$W/image/rootfs.ext2

if [ "${1:-}" = "--run-one" ]; then
    MODEL=$2
    LANG_SETTING=$3

    mount --make-rprivate /
    ip link set lo up

    R=$W/root-$MODEL-$LANG_SETTING
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
    printf "$MODEL" > "$R/sys/firmware/devicetree/base/inmusic,product-code"
    case "$MODEL" in
        JP07) printf 'inmusic,jp07\0rockchip,rk3288\0' > "$R/sys/firmware/devicetree/base/compatible" ;;
        JP13) printf 'inmusic,jp13\0rockchip,rk3288\0' > "$R/sys/firmware/devicetree/base/compatible" ;;
        JC11) printf 'inmusic,jc11\0rockchip,rk3288\0' > "$R/sys/firmware/devicetree/base/compatible" ;;
    esac

    mount -t proc -o ro proc "$R/proc"
    printf '           CPU0\n 41:        100     GICv2  32 Level     ttyS0\n' > "$W/interrupts"
    mount --bind "$W/interrupts" "$R/proc/interrupts"
    mount -t tmpfs tmpfs "$R/proc/irq"
    mkdir -p "$R/proc/irq/41"
    printf 'f\n' > "$R/proc/irq/41/smp_affinity"
    printf '0-3\n' > "$R/proc/irq/41/smp_affinity_list"

    # ScreenConfiguration.json
    mount --bind "$W/ScreenConfiguration.json" "$R/usr/Engine/ScreenConfiguration/Default/ScreenConfiguration.json"

    # D-Bus
    mkdir -p "$R/run/dbus"
    chroot "$R" /usr/bin/dbus-daemon --system --fork --nopidfile

    # Bind host OUT_DIR into chroot
    mkdir -p "$OUT_DIR/$MODEL/$LANG_SETTING"
    mkdir -p "$R/var/tmp/hunt-L2"
    mount --bind "$OUT_DIR" "$R/var/tmp/hunt-L2"

    # /data setup
    mkdir -p "$R/data/enginex/ui/packs"
    ln -sf enginex "$R/data/sc5000-custom"
    cp -a "$W/packs/$LANG_SETTING" "$R/data/enginex/ui/packs/"
    mkdir -p "$R/data/enginex/ui/packs/$LANG_SETTING/Skins/Common/Items"

    # Generate probe with substituted model and lang
    sed -e "s/@MODEL@/$MODEL/g" -e "s/@LANG@/$LANG_SETTING/g" \
        "$REPO/tools/az01-packer/hunt-l2-probe-template.qml" \
        > "$R/data/enginex/ui/packs/$LANG_SETTING/Skins/Common/Items/QmlWarnings.qml"

    # Copy hook
    mkdir -p "$R/tmp"
    cp "$REPO/rootfs-overlay/etc/enginex/libengine-ui-hook.so" "$R/tmp/libengine-ui-hook.so"

    # FX QML imports
    mkdir -p "$R/tmp/fx-qml-imports"
    cp -a /root/build/sc5000-lab/5.3.0/qrc/airAssignments/InputAssignment /root/build/sc5000-lab/5.3.0/qrc/airAssignments/OutputAssignment "$R/tmp/fx-qml-imports/" || true

    # Copy icons
    mkdir -p "$R/data/enginex/ui/icons"
    cp -a "$REPO/tools/ui-mod/icons/." "$R/data/enginex/ui/icons/"

    # Mod Sequencer
    cp "$REPO/docs/mods/src/sampler-sequencer/Panel.qml" "$R/tmp/Panel.qml"

    printf 'lang=%s\nfour_deck_waveform=1\nfour_deck_role=12\n' "$LANG_SETTING" > "$R/data/enginex/ui/settings.conf"

    # Start mock server
    python3 "$REPO/tools/az01-packer/hunt-l2-mock-server.py" 8080 > "$W/logs/mock-$MODEL-$LANG_SETTING.log" 2>&1 &
    MOCK_PID=$!
    trap 'kill "$MOCK_PID" 2>/dev/null || true' EXIT

    for i in $(seq 1 40); do
        if curl -sf http://127.0.0.1:8080/v1/enginex/ui >/dev/null 2>&1; then break; fi
        sleep 0.05
    done

    # Run Engine
    mkdir -p "$R/tmp/home"
    timeout --kill-after=5s 180s chroot "$R" /usr/bin/env -i \
        PATH=/usr/bin:/bin:/usr/sbin:/sbin \
        HOME=/tmp/home \
        LD_LIBRARY_PATH=/usr/lib:/usr/qt/lib \
        LD_PRELOAD=/tmp/libengine-ui-hook.so \
        QT_QPA_PLATFORM=eglfs \
        QT_QPA_EGLFS_INTEGRATION=eglfs_kms \
        QML_IMPORT_PATH=/tmp/fx-qml-imports QML2_IMPORT_PATH=/tmp/fx-qml-imports \
        LIBGL_ALWAYS_SOFTWARE=1 \
        LP_NUM_THREADS=4 \
        QML_DISABLE_DISK_CACHE=1 \
        MALLOC_ARENA_MAX=1 \
        /usr/Engine/Engine -d0 > "$W/logs/engine-$MODEL-$LANG_SETTING.log" 2>&1 || true

    kill "$MOCK_PID" 2>/dev/null || true
    trap - EXIT

    cp "$W/logs/engine-$MODEL-$LANG_SETTING.log" "$OUT_DIR/logs/" || true
    exit 0
fi

# Main host entrypoint
[ "$(uname -m)" = "aarch64" ] || { echo "Error: an aarch64 build host is required" >&2; exit 1; }
mkdir -p "$W" "$OUT_DIR" "$W/logs" "$OUT_DIR/logs"

# Unpack rootfs once
if [ ! -f "$IMG_EXT2" ]; then
    echo "==> Unpacking baseline image rootfs..."
    rm -rf "$W/image"
    python3 "$REPO/tools/az01-packer/az01_packer.py" unpack /root/build/sc5000-lab/fw-5.3.0/SC5000-5.3.0-Update.img --output "$W/image"
fi

# Build UI packs
echo "==> Building language packs..."
python3 "$REPO/tools/ui-mod/build_packs.py" --qrc /root/build/sc5000-lab/5.3.0/qrc --out "$W/packs"

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

MODELS=${SWEEP_MODELS:-"JP07 JP13 JC11"}
LANGS=${SWEEP_LANGS:-"en zh-Hans zh-Hant es"}

for model in $MODELS; do
    for lang in $LANGS; do
        echo "==> [$(date +%T)] Sweeping $model $lang..."
        unshare -m -n -p -f --kill-child bash "$0" --run-one "$model" "$lang"
    done
done

echo "==> Sweep finished!"
ls -la "$OUT_DIR"
