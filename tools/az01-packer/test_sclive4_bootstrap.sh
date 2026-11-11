#!/usr/bin/env bash
# End-to-end sandbox verification for SC LIVE 4 (inmusic,jp21) (#61).
# Verifies Method A (host-side ext4 debugfs injection), Method B (on-device install.sh),
# ALSA-to-LD_PRELOAD one-shot boot hook, and Engine 5.3.0-sclive4 full hook application.
set -euo pipefail

# AZ04=1: PRIME 4 G2 / SYSTEM ONE (#76), 64-bit Engine, /var/lib bound from /data/system/var-lib.
AZ04="${AZ04:-0}"
BOOTSTRAP_MODEL="${BOOTSTRAP_MODEL:-SCLIVE4}"
JC11S_ROOTFS="${JC11S_ROOTFS:-}"
if [ "$AZ04" != 1 ] && [ -n "$JC11S_ROOTFS" ] && [ ! -d "$JC11S_ROOTFS" ]; then
    echo "test_sclive4_bootstrap: explicit JC11S_ROOTFS not found: $JC11S_ROOTFS" >&2
    exit 1
fi
if [ "$BOOTSTRAP_MODEL" = PRIME4PLUS ]; then
    ROOTFS="${JC11S_ROOTFS:?set JC11S_ROOTFS to the unpacked official PRIME4PLUS rootfs}"
    # JC11S uses the same Engine/edit table as SC LIVE 4, including #166 SweepFX.
    PREFIX=PRIME4PLUS VARLIB=data/system/var/overlay/lib PROFILE=5.3.0-sclive4 EDITS=28
elif [ "$AZ04" = 1 ]; then
    ROOTFS="${ROOTFS:-/root/build/sc5000-lab/systemone-analysis/rootfs}"
    PREFIX=PRIME4G2 VARLIB=data/system/var-lib PROFILE=5.3.0-az04 EDITS=26
else
    ROOTFS="${ROOTFS:-/root/build/sc5000-lab/sclive4-rea61-followup/rootfs}"  # or another unpacked gen2 rootfs
    PREFIX=SCLIVE4 VARLIB=data/system/var/overlay/lib PROFILE=5.3.0-sclive4 EDITS=28
fi
QRC="${QRC:-/root/build/sc5000-lab/5.3.0/qrc}"  # AZ04's QML is byte-identical to it
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$SCRIPT_DIR/../.." && pwd)"

if [ "$(id -u)" != 0 ]; then
    echo "test_sclive4_bootstrap: must run as root on Oracle-Batam-arm" >&2
    exit 1
fi

if [ ! -d "$ROOTFS" ]; then
    echo "test_sclive4_bootstrap: $PREFIX rootfs not found at $ROOTFS" >&2
    exit 1
fi

TMP=$(mktemp -d "${BOOTSTRAP_TMP_PARENT:-/tmp}/sclive4-sbx-XXXXXX")
trap 'rm -rf "$TMP"' EXIT

echo "=== $PREFIX Bootstrap & Installer Verification ==="
echo "Workdir: $TMP"

# 1. Build Suite, Hook and UI Packs
echo "[1/5] Building components and packaging data bundle..."
BUILD_TAG=$(python3 "$REPO/tools/ui-mod/build_packs.py" --print-build-tag)
if [ "$AZ04" = 1 ]; then
    HOOK="$REPO/out/aarch64/libengine-ui-hook.so"
    SUITE="$REPO/out/aarch64/sc5000-suite"
    mkdir -p "$REPO/out/aarch64"
    (cd "$REPO/tools/ui-mod" && CROSS= OUT="$HOOK" sh build.sh)
    (cd "$REPO/tools/edisksd-hook" && CROSS= OUT="$REPO/out/aarch64/libedisksd-hook.so" sh build.sh)
    (cd "$REPO" && . /etc/profile.d/buildtools.sh && \
     GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X 'github.com/Cd1s/EngineX-OS/pkg/webui.BuildTag=$BUILD_TAG'" -o "$SUITE" ./cmd/sc5000-suite)
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
    (cd "$REPO/tools/edisksd-hook" && sh build.sh)
    HOOK="$REPO/rootfs-overlay/etc/sc5000/libengine-ui-hook.so"
    SUITE="$REPO/out/sc5000-suite"
fi

if [ ! -f "$SUITE" ]; then
    echo "Building sc5000-suite..."
    (cd "$REPO" && . /etc/profile.d/buildtools.sh && \
     GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X 'github.com/Cd1s/EngineX-OS/pkg/webui.BuildTag=$BUILD_TAG'" -o "$SUITE" ./cmd/sc5000-suite)
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
mkfs.ext4 -F -q -O encrypt -L az01-data "$IMG"
debugfs -w -R "mkdir /Music" "$IMG" >/dev/null 2>&1
debugfs -w -R "write /etc/hosts /Music/user_track.flac" "$IMG" >/dev/null 2>&1

echo 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEngineXSandboxTestKeyOnlyNotARealKey00000 enginex-sandbox' > "$TMP/id_test.pub"
python3 "$REPO/tools/az01-packer/host-install-userdata.py" "$IMG" --bundle "$BUNDLE" --ssh-key "$TMP/id_test.pub"

# Verify filesystem integrity
e2fsck -f -y "$IMG" >/dev/null
debugfs -R "stat /Music/user_track.flac" "$IMG" 2>&1 | grep -q "regular"
debugfs -R "stat /${VARLIB#data/}/alsa/conf.d/99-sc5000-custom.conf" "$IMG" 2>&1 | grep -q "regular"
debugfs -R "stat /enginex/bundle/lib/libengine-ui-hook.so" "$IMG" 2>&1 | grep -q "regular"
debugfs -R "stat /enginex/bundle/lib/libedisksd-hook.so" "$IMG" 2>&1 | grep -q "regular"
debugfs -R "stat /enginex/bundle/boot-activate.sh" "$IMG" 2>&1 | grep -q "regular"
test "$(debugfs -R "cat /enginex/version.txt" "$IMG" 2>/dev/null | tr -d '\r\n')" = "5.3.0.1.1"
# #103: the old path stays reachable (the ALSA hook and older scripts use it)
debugfs -R "stat /sc5000-custom" "$IMG" 2>&1 | grep -q "symlink"
debugfs -R "stat /sc5000-custom/bundle/boot-activate.sh" "$IMG" 2>&1 | grep -q "regular"
debugfs -R "stat /enginex/authorized_keys" "$IMG" 2>&1 | grep -q "Mode:  0600"
echo "  Method A ext4 userdata injection: PASS"

if [ "$PREFIX" = PRIME4PLUS ]; then
    # Verify the actual JC11S unit chain, not SC LIVE 4's similar layout.
    grep -q 'What=PARTUUID=931ad49d-ad59-0849-833a-9bf00af5b60e' "$ROOTFS/lib/systemd/system/data.mount"
    grep -q 'Requires=az0x-data-mkfs.service' "$ROOTFS/lib/systemd/system/data.mount"
    grep -q 'What=/data/system/var/overlay/lib' "$ROOTFS/lib/systemd/system/var-lib.mount"
    test -L "$ROOTFS/etc/systemd/system/local-fs.target.wants/data.mount"
    test -L "$ROOTFS/etc/systemd/system/local-fs.target.wants/var-lib.mount"
    test -L "$ROOTFS/etc/systemd/system/multi-user.target.wants/engine.service"
    unshare -m -n -p -f bash "$SCRIPT_DIR/test_jc11s_firstboot.sh" "$ROOTFS" "$IMG" "$TMP"
fi

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
    if [ -f '$TARGET/run/systemd/system/enginex-sc5000-deemixd.service' ]; then
        break
    fi
    sleep 0.1
done

test -f '$TARGET/run/sc5000-alsa-reexec.once'
grep -q 'SC5000_UI_HOOK=/data/enginex/bundle/lib/libengine-ui-hook.so' \
    '$TARGET/run/systemd/system/engine.service.d/99-sc5000.conf'
grep -q 'UnsetEnvironment=LD_PRELOAD' '$TARGET/run/systemd/system/engine.service.d/99-sc5000.conf'
grep -q 'engine-ui-guard launch /usr/Engine/Engine' '$TARGET/usr/Engine/Scripts/engine'
grep -q 'engine-ui-guard service /usr/Engine/Scripts/runengine' '$TARGET/run/systemd/system/engine.service.d/99-sc5000.conf'
# Repeated activation in one boot must not truncate its own bound input.
before=\$(sha256sum '$TARGET/usr/Engine/Scripts/engine' | cut -d' ' -f1)
chroot '$TARGET' /bin/sh /data/enginex/bundle/boot-activate.sh >/dev/null 2>&1
test -s '$TARGET/usr/Engine/Scripts/engine'
test \"\$before\" = \"\$(sha256sum '$TARGET/usr/Engine/Scripts/engine' | cut -d' ' -f1)\"
grep -q 'Environment=HOME=/tmp/sc5000-engine-home MESA_SHADER_CACHE_MAX_SIZE=32M' \
    '$TARGET/run/systemd/system/engine.service.d/99-sc5000.conf'
grep -q 'Environment=QML_DISABLE_DISK_CACHE=1' \
    '$TARGET/run/systemd/system/engine.service.d/99-sc5000.conf'
grep -q 'ExecStartPre=/bin/mkdir -p -m 0700 /tmp/sc5000-engine-home' \
    '$TARGET/run/systemd/system/engine.service.d/99-sc5000.conf'
grep -q 'EnvironmentFile=-/data/sc5000-custom/ui-scale.env' \
    '$TARGET/run/systemd/system/engine.service.d/99-sc5000.conf'
grep -q 'Wants=enginex-stream-sync.service' \
    '$TARGET/run/systemd/system/engine.service.d/99-sc5000.conf'
test -f '$TARGET/run/systemd/system/enginex-stream-sync.service'
grep -q 'ExecStart=/data/enginex/bundle/bin/stream-sync' \
    '$TARGET/run/systemd/system/enginex-stream-sync.service'
test -f '$TARGET/run/systemd/system/enginex-rb-bridge.service'
# rekordbox sticks are converted inside the mount, as on the 1st-gen images.
grep -q 'LD_PRELOAD=/data/enginex/bundle/lib/libedisksd-hook.so' '$TARGET/run/systemd/system/edisksd.service.d/99-sc5000.conf'
test -f '$TARGET/run/systemd/system/enginex-sc5000-deemixd.service'
# #116: KillMode=process left the hook-less Engine running after the restart.
if grep -q KillMode '$TARGET/run/systemd/system/engine.service.d/99-sc5000.conf'; then exit 1; fi
# --ssh-key: the read-only stock authorized_keys is replaced by a bind mount, and sshd starts.
for i in \$(seq 1 50); do
    chroot '$TARGET' pgrep -x sshd >/dev/null 2>&1 && break
    sleep 0.1
done
cmp '$TMP/id_test.pub' '$TARGET/root/.ssh/authorized_keys'
chroot '$TARGET' pgrep -x sshd >/dev/null
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
    # AArch64 DSP guards: 14 of the 21 sites (the 7 event guards need an unverified event layout).
    grep -q 'engine-ui-hook: 14 DSP guards/transitions installed' '$TMP/stage2.log'
    grep -q 'engine-ui-hook: SQLite open/open_v2 hooks installed' '$TMP/stage2.log'
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
test -f '$TARGET/data/enginex/version.txt'
test -L '$TARGET/data/sc5000-custom'

# Run boot-activate.sh to verify service unit creation and subsequent cleanup
chroot '$TARGET' /bin/sh /data/enginex/bundle/boot-activate.sh >/dev/null 2>&1 || true
test -f '$TARGET/run/systemd/system/enginex-stream-sync.service'
test -f '$TARGET/run/systemd/system/enginex-rb-bridge.service'
test -f '$TARGET/run/systemd/system/enginex-sc5000-deemixd.service'
test -f '$TARGET/run/systemd/system/engine.service.d/99-sc5000.conf'
for i in \$(seq 1 20); do
    chroot '$TARGET' pgrep -x sc5000-suite >/dev/null 2>&1 && break
    sleep 0.1
done
chroot '$TARGET' pgrep -x sc5000-suite >/dev/null 2>&1

chroot '$TARGET' /bin/sh /data/enginex/bundle/uninstall.sh >/dev/null

test ! -f '$TARGET/$VARLIB/alsa/conf.d/99-sc5000-custom.conf'
test ! -d '$TARGET/data/enginex/bundle'
test -f '$TARGET/data/Music/user_track.flac'
test ! -f '$TARGET/run/systemd/system/enginex-stream-sync.service'
test ! -f '$TARGET/run/systemd/system/enginex-rb-bridge.service'
test ! -f '$TARGET/run/systemd/system/enginex-sc5000-deemixd.service'
test ! -f '$TARGET/run/systemd/system/engine.service.d/99-sc5000.conf'
test ! -f '$TARGET/run/systemd/system/edisksd.service.d/99-sc5000.conf'
for i in \$(seq 1 20); do
    chroot '$TARGET' pgrep -x sc5000-suite >/dev/null 2>&1 || break
    sleep 0.1
done
if chroot '$TARGET' pgrep -x sc5000-suite >/dev/null 2>&1; then exit 1; fi
"
echo "  Method B on-device install & uninstall: PASS"

# 5b. #103 upgrade: a deck still on the real /data/sc5000-custom keeps every file when install.sh runs
echo "[5b] Testing upgrade from the legacy /data/sc5000-custom directory..."
unshare -m -n -p -f bash -c "
set -euo pipefail
mount --bind '$ROOTFS' '$TARGET'
mount -o remount,ro,bind '$TARGET'
mount -o loop '$IMG' '$TARGET/data'
mount --bind '$TARGET/$VARLIB' '$TARGET/var/lib'
mount -t tmpfs tmpfs '$TARGET/run'
mount -t tmpfs tmpfs '$TARGET/tmp'
mount --bind /dev '$TARGET/dev'

rm -f '$TARGET/data/sc5000-custom'
rm -rf '$TARGET/data/enginex/bundle'
mv '$TARGET/data/enginex' '$TARGET/data/sc5000-custom'
mkdir -p '$TARGET/data/sc5000-custom/Stream'
echo keep > '$TARGET/data/sc5000-custom/Stream/song.flac'
echo ssh-key > '$TARGET/data/sc5000-custom/authorized_keys'
mkdir -p '$TARGET/tmp/installer'
tar -xzf '$BUNDLE' -C '$TARGET/tmp/installer'
chroot '$TARGET' /bin/sh /tmp/installer/$PREFIX-5.3.0.1.1-EngineX-OS-Data-Bundle/install.sh >/dev/null

test -L '$TARGET/data/sc5000-custom'
test \"\$(readlink '$TARGET/data/sc5000-custom')\" = enginex
test \"\$(cat '$TARGET/data/enginex/Stream/song.flac')\" = keep
test \"\$(cat '$TARGET/data/sc5000-custom/authorized_keys')\" = ssh-key
test -f '$TARGET/data/sc5000-custom/bundle/boot-activate.sh'
"
echo "  Legacy directory upgrade: PASS"

echo ""
echo "=== All $PREFIX verification checks PASSED ==="

# Include the separate official JC11S rootfs when its fixture is supplied. Set
# JC11S_ROOTFS to an unpacked PRIME4PLUS-5.3.0 image (never a live deck).
if [ "$AZ04" != 1 ] && [ "$BOOTSTRAP_MODEL" = SCLIVE4 ]; then
    if [ -n "$JC11S_ROOTFS" ]; then
        BOOTSTRAP_MODEL=PRIME4PLUS JC11S_ROOTFS="$JC11S_ROOTFS" bash "$0"
    else
        echo 'WARN: JC11S not tested; set JC11S_ROOTFS to include the official PRIME4PLUS first-boot case (required for release).'
    fi
fi
