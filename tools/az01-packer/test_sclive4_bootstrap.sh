#!/usr/bin/env bash
# End-to-end sandbox verification for SC LIVE 4 (inmusic,jp21) (#61).
# Verifies Method A (host-side ext4 debugfs injection), Method B (on-device install.sh),
# ALSA-to-LD_PRELOAD one-shot boot hook, and Engine 5.3.0-sclive4 full hook application.
set -euo pipefail

# AZ04=1: PRIME 4 G2 / SYSTEM ONE (#76), 64-bit Engine, /var/lib bound from /data/system/var-lib.
AZ04="${AZ04:-0}"
if [ "$AZ04" = 1 ]; then
    ROOTFS="${ROOTFS:-/root/build/sc5000-lab/systemone-analysis/rootfs}"
    PREFIX=PRIME4G2 VARLIB=data/system/var-lib PROFILE=5.3.0-az04 EDITS=6
else
    ROOTFS="${ROOTFS:-/root/build/sc5000-lab/sclive4-rea61-followup/rootfs}"  # or another unpacked gen2 rootfs
    PREFIX=SCLIVE4 VARLIB=data/system/var/overlay/lib PROFILE=5.3.0-sclive4 EDITS=16
fi
QRC="${QRC:-/root/build/sc5000-lab/5.3.0/qrc}"  # AZ04's QML is byte-identical to it
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$SCRIPT_DIR/../.." && pwd)"

if [ "$(id -u)" != 0 ]; then
    echo "test_sclive4_bootstrap: must run as root on Oracle-Batam-arm" >&2
    exit 1
fi

if [ ! -d "$ROOTFS" ]; then
    echo "test_sclive4_bootstrap: SC LIVE 4 rootfs not found at $ROOTFS" >&2
    exit 1
fi

TMP=$(mktemp -d /tmp/sclive4-sbx-XXXXXX)
trap 'rm -rf "$TMP"' EXIT

echo "=== $PREFIX Bootstrap & Installer Verification ==="
echo "Workdir: $TMP"

# 1. Build Suite, Hook and UI Packs
echo "[1/5] Building components and packaging data bundle..."
if [ "$AZ04" = 1 ]; then
    HOOK="$REPO/out/aarch64/libengine-ui-hook.so"
    SUITE="$REPO/out/aarch64/sc5000-suite"
    mkdir -p "$REPO/out/aarch64"
    (cd "$REPO/tools/ui-mod" && CROSS= OUT="$HOOK" sh build.sh)
    (cd "$REPO" && . /etc/profile.d/buildtools.sh && \
     GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$SUITE" ./cmd/sc5000-suite)
    # Stage 1 stand-in for the stock Engine: loads alsa.conf (and so conf.d) and opens a PCM, as Engine does.
    PROBE="$TMP/alsa-probe"
    printf '%s\n' '#include <dlfcn.h>' \
        'int main(void) {' \
        '    void *h = dlopen("libasound.so.2", RTLD_NOW), *pcm = 0;' \
        '    if (!h) return 1;' \
        '    int (*update)(void) = (int (*)(void))dlsym(h, "snd_config_update");' \
        '    int (*open)(void **, const char *, int, int) = (int (*)(void **, const char *, int, int))dlsym(h, "snd_pcm_open");' \
        '    int (*close)(void *) = (int (*)(void *))dlsym(h, "snd_pcm_close");' \
        '    if (!update || !open || !close || update() < 0) return 1;' \
        '    if (open(&pcm, "null", 0, 0) == 0) close(pcm);' \
        '    return 0;' \
        '}' \
        > "$TMP/alsa-probe.c"
    gcc -O2 -o "$PROBE" "$TMP/alsa-probe.c" -ldl
else
    PROBE=/root/build/sc5000-lab/sclive4-rea61-followup/stock-alsa-probe
    (cd "$REPO/tools/ui-mod" && sh build.sh)
    HOOK="$REPO/rootfs-overlay/etc/sc5000/libengine-ui-hook.so"
    SUITE="$REPO/out/sc5000-suite"
fi

if [ ! -f "$SUITE" ]; then
    echo "Building sc5000-suite..."
    (cd "$REPO" && . /etc/profile.d/buildtools.sh && \
     GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$SUITE" ./cmd/sc5000-suite)
fi

mkdir -p "$TMP/packs_raw" "$TMP/ui_bundle/packs"
python3 "$REPO/tools/ui-mod/build_packs.py" --qrc "$QRC" --out "$TMP/packs_raw" >/dev/null
cp -a "$TMP/packs_raw/en" "$TMP/packs_raw/zh-Hans" "$TMP/packs_raw/zh-Hant" "$TMP/ui_bundle/packs/"
cp -a "$TMP/packs_raw/icons" "$TMP/ui_bundle/"
tar -C "$TMP/ui_bundle" -czf "$TMP/ui-packs.tar.gz" packs icons

python3 "$REPO/tools/az01-packer/build-data-bundle.py" 5.3.0.1.1 \
    --suite "$SUITE" \
    --hook "$HOOK" \
    --ui-packs "$TMP/ui-packs.tar.gz" \
    --prefix "$PREFIX" \
    --out "$TMP/bundle_out" >/dev/null

BUNDLE="$TMP/bundle_out/$PREFIX-5.3.0.1.1-EngineX-OS-Data-Bundle.tar.gz"
test -f "$BUNDLE"
echo "  Data bundle: $BUNDLE"

# 2. Method A: Host-side userdata ext4 injection
echo "[2/5] Testing Method A (host-install-userdata.py ext4 injection)..."
IMG="$TMP/userdata.img"
dd if=/dev/zero of="$IMG" bs=1M count=64 status=none
mkfs.ext4 -F -q "$IMG"
debugfs -w -R "mkdir /Music" "$IMG" >/dev/null 2>&1
debugfs -w -R "write /etc/hosts /Music/user_track.flac" "$IMG" >/dev/null 2>&1

python3 "$REPO/tools/az01-packer/host-install-userdata.py" "$IMG" --bundle "$BUNDLE"

# Verify filesystem integrity
e2fsck -f -y "$IMG" >/dev/null
debugfs -R "stat /Music/user_track.flac" "$IMG" 2>&1 | grep -q "regular"
debugfs -R "stat /${VARLIB#data/}/alsa/conf.d/99-sc5000-custom.conf" "$IMG" 2>&1 | grep -q "regular"
debugfs -R "stat /sc5000-custom/bundle/lib/libengine-ui-hook.so" "$IMG" 2>&1 | grep -q "regular"
debugfs -R "stat /sc5000-custom/bundle/boot-activate.sh" "$IMG" 2>&1 | grep -q "regular"
test "$(debugfs -R "cat /sc5000-custom/version.txt" "$IMG" 2>/dev/null | tr -d '\r\n')" = "5.3.0.1.1"
echo "  Method A ext4 userdata injection: PASS"

# 3. Stage 1: Cold Boot ALSA One-Shot Activation
echo "[3/5] Testing Stage 1 (Cold boot ALSA hook trigger & activation)..."
TARGET="$TMP/root"
mkdir -p "$TARGET"

unshare -m -n -p -f bash -c "
set -euo pipefail
mount --bind '$ROOTFS' '$TARGET'
mount -o remount,ro,bind '$TARGET'
mount -o loop '$IMG' '$TARGET/data'
mount --bind '$TARGET/$VARLIB' '$TARGET/var/lib'
mount -t tmpfs tmpfs '$TARGET/run'
mount -t tmpfs tmpfs '$TARGET/tmp'
mount --bind /dev '$TARGET/dev'
mount -t proc proc '$TARGET/proc'
mount -t sysfs sysfs '$TARGET/sys'

# Assert rootfs is read-only
if touch '$TARGET/etc/ro_test' 2>/dev/null; then
    echo 'FAIL: rootfs not read-only' >&2
    exit 1
fi

# Verify ALSA overlay conf exists
test -f '$TARGET/var/lib/alsa/conf.d/99-sc5000-custom.conf'

# Stage 1 trigger: run probe named Engine (simulating stock un-preloaded Engine audio open)
cp '$PROBE' '$TARGET/tmp/Engine'
chmod +x '$TARGET/tmp/Engine'

chroot '$TARGET' /tmp/Engine / > '$TMP/stage1.log' 2>&1 || true

# Wait briefly for background boot-activate.sh
for i in \$(seq 1 30); do
    if [ -f '$TARGET/run/systemd/system/engine.service.d/99-sc5000.conf' ]; then
        break
    fi
    sleep 0.1
done

test -f '$TARGET/run/sc5000-alsa-reexec.once'
grep -q 'LD_PRELOAD=/data/sc5000-custom/bundle/lib/libengine-ui-hook.so' \
    '$TARGET/run/systemd/system/engine.service.d/99-sc5000.conf'
"
echo "  Stage 1 ALSA boot activation: PASS"

# 4. Stage 2: Second Boot with LD_PRELOAD
echo "[4/5] Testing Stage 2 ($PREFIX Engine running with LD_PRELOAD)..."
unshare -m -n -p -f bash -c "
set -euo pipefail
mount --bind '$ROOTFS' '$TARGET'
mount -o remount,ro,bind '$TARGET'
mount -o loop '$IMG' '$TARGET/data'
mount --bind '$TARGET/$VARLIB' '$TARGET/var/lib'
mount -t tmpfs tmpfs '$TARGET/run'
mount -t tmpfs tmpfs '$TARGET/tmp'
mount --bind /dev '$TARGET/dev'
mount -t proc proc '$TARGET/proc'
mount -t sysfs sysfs '$TARGET/sys'

mkdir -p -m 0700 '$TARGET/tmp/sc5000-engine-home'

# Launch real SC LIVE 4 Engine with LD_PRELOAD
timeout 8 chroot '$TARGET' /usr/bin/env -i \
    HOME=/tmp/sc5000-engine-home \
    LD_PRELOAD=/data/sc5000-custom/bundle/lib/libengine-ui-hook.so \
    QT_QPA_PLATFORM=offscreen \
    LD_LIBRARY_PATH=/usr/qt/lib \
    /usr/Engine/Engine -d0 > '$TMP/stage2.log' 2>&1 || true

grep -q 'engine-ui-hook: Engine $PROFILE' '$TMP/stage2.log'
test \$(grep -c 'engine-ui-hook: applied 0x' '$TMP/stage2.log') -eq $EDITS
grep -Eq 'engine-ui-hook: .*packs/en: [1-9][0-9]* files' '$TMP/stage2.log'
if [ '$AZ04' = 1 ]; then
    # The A32 trampolines must never run in the 64-bit Engine.
    ! grep -Eq 'DSP guards|SQLite open' '$TMP/stage2.log'
else
    grep -q 'engine-ui-hook: 21 DSP guards/transitions installed' '$TMP/stage2.log'
    grep -q 'engine-ui-hook: SQLite open/open_v2 hooks installed' '$TMP/stage2.log'
fi
"
echo "  Stage 2 $PREFIX Engine hook: PASS ($EDITS memory edits, UI pack loaded)"

# 5. Method B: On-Device install.sh / uninstall.sh
echo "[5/5] Testing Method B (on-device install.sh / uninstall.sh)..."
unshare -m -n -p -f bash -c "
set -euo pipefail
mount --bind '$ROOTFS' '$TARGET'
mount -o remount,ro,bind '$TARGET'
mount -o loop '$IMG' '$TARGET/data'
mount --bind '$TARGET/$VARLIB' '$TARGET/var/lib'
mount -t tmpfs tmpfs '$TARGET/run'
mount -t tmpfs tmpfs '$TARGET/tmp'
mount --bind /dev '$TARGET/dev'
mount -t proc proc '$TARGET/proc'
mount -t sysfs sysfs '$TARGET/sys'

mkdir -p '$TARGET/tmp/installer'
tar -xzf '$BUNDLE' -C '$TARGET/tmp/installer'
chroot '$TARGET' /bin/sh /tmp/installer/$PREFIX-5.3.0.1.1-EngineX-OS-Data-Bundle/install.sh >/dev/null

test -f '$TARGET/$VARLIB/alsa/conf.d/99-sc5000-custom.conf'
test -f '$TARGET/data/sc5000-custom/version.txt'

chroot '$TARGET' /bin/sh /data/sc5000-custom/bundle/uninstall.sh >/dev/null

test ! -f '$TARGET/$VARLIB/alsa/conf.d/99-sc5000-custom.conf'
test ! -d '$TARGET/data/sc5000-custom/bundle'
test -f '$TARGET/data/Music/user_track.flac'
"
echo "  Method B on-device install & uninstall: PASS"

echo ""
echo "=== All $PREFIX verification checks PASSED ==="
