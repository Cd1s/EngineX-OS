#!/bin/bash
# The repository from this script's own path: remote-build copies have no .git.
. "${ENGINEX_MIGRATION:-$(cd "$(dirname "$0")/../.." && pwd)/rootfs-overlay/usr/libexec/enginex-migration}"
ENGINEX_LAB=${ENGINEX_LAB:-$legacy_lab}
# Share Decks (#329): a SC5000 (JP07) in the Stock layout with the row on, shown by a console's Line In view. Natively on
# an ARM build host, nothing on a real deck or the host's network:
#   the player   its own Engine 5.3.0 with the UI hook and the pack built from this tree (eglfs, llvmpipe, the shadow-twin's
#                ALSA null card), share-decks-probe.qml in place of QmlWarnings.qml, and the real stream-sync of this tree,
#                in the sandbox's network namespace (10.79.0.2);
#   the console  the real stream-sync of this tree as a PRIME 4 (JC11) with Linked View -> Line In, in a second network
#                namespace on a veth (10.79.0.1), share_decks_sandbox_driver.py in place of its QML (a channel on Line or not).
# The probe loads a track on Deck 1, plays it and says READY; the driver then runs its phases (no ask, a channel on Line,
# silence, the row switched off on the player) while a sampler records the player's stream-sync and the Engine's CPU time.
# share_decks_sandbox_check.py judges the logs.
#   bash tools/az01-packer/share-decks-sandbox.sh <new-private-dir>
# Run it under `enginex-run exclusive` (CPU time is measured, one GPU); it takes no lock itself.
set -euo pipefail
REPO=$(cd "$(dirname "$0")/../.." && pwd)
HERE=$REPO/tools/az01-packer
W=$(realpath -m "${1:?private sandbox directory required}")
LAB=${ENGINEX_LAB}
if [ "${2:-}" != --inner ]; then
    [ "$(uname -m)" = aarch64 ] || { echo 'native ARM build host (aarch64) required' >&2; exit 1; }
    [ ! -e "$W" ] || { echo "refusing existing sandbox $W" >&2; exit 1; }
    mkdir -p "$W"
    python3 "$HERE/az01_packer.py" unpack "$LAB/fw-5.3.0/SC5000-5.3.0-Update.img" --output "$W/SC5000" > "$W/unpack.json"
    python3 "$REPO/tools/ui-mod/build_packs.py" --qrc "$LAB/5.3.0/qrc" --out "$W/packs" > "$W/packs.log"
    ( . /etc/profile.d/buildtools.sh
      cd "$REPO" && GOCACHE="$W/gocache" GOFLAGS=-buildvcs=false GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -o "$W/suite" ./cmd/enginex-suite )
    # Engine only loads and plays tracks with an audio card: the shadow-twin's sandbox-only ALSA null card on a monotonic clock.
    arm-linux-gnueabihf-gcc -O2 -shared -fPIC -Wall -Wextra -o "$W/alsa-null-fixture.so" "$REPO/tools/shadow-twin/alsa_null_fixture.c" -ldl
    # Non-copyright test track: 120 s of a 128 BPM kick.
    python3 -I - "$W/track.wav" <<'PY'
import array, math, sys, wave
rate, seconds, bpm = 44100, 120, 128.0
buf = array.array('h', bytes(rate * seconds * 2 * 2))
beat = 0
while True:
    start = int(round((0.30 + beat * 60.0 / bpm) * rate))
    if start + rate // 4 >= rate * seconds: break
    for n in range(rate // 5):
        v = int(28000 * math.exp(-n / 2200.0) * math.sin(2 * math.pi * (55 + 90 * math.exp(-n / 600.0)) * n / rate))
        buf[(start + n) * 2] += v; buf[(start + n) * 2 + 1] += v
    beat += 1
with wave.open(sys.argv[1], 'wb') as w:
    w.setnchannels(2); w.setsampwidth(2); w.setframerate(rate); w.writeframes(buf.tobytes())
PY
    # The child checks that it really runs in its own mount, network and PID namespaces before its first mount.
    export ENGINEX_OUTER_MNTNS ENGINEX_OUTER_NETNS
    ENGINEX_OUTER_MNTNS=$(readlink /proc/self/ns/mnt)
    ENGINEX_OUTER_NETNS=$(readlink /proc/self/ns/net)
    rc=0
    unshare -m -n -p -f --kill-child --propagation private bash "$0" "$W" --inner || rc=$?
    echo "inner rc=$rc"
    python3 -I "$HERE/share_decks_sandbox_check.py" "$W" && exit 0
    exit 1
fi
# --inner is this script's own re-exec. Passed by hand it would skip the unshare above, and the mounts below would land
# in the caller's namespaces (the build host's): refuse unless both namespaces really changed.
if [ -z "${ENGINEX_OUTER_MNTNS:-}" ] || [ -z "${ENGINEX_OUTER_NETNS:-}" ] ||
    [ "$(readlink /proc/self/ns/mnt)" = "$ENGINEX_OUTER_MNTNS" ] || [ "$(readlink /proc/self/ns/net)" = "$ENGINEX_OUTER_NETNS" ]; then
    echo "share-decks-sandbox.sh: --inner is this script's own re-exec flag; run it as: bash $0 <new-private-dir>" >&2
    exit 2
fi
R=$W/root
mount --make-rprivate /
mount -t proc proc /proc # this PID namespace's own: the PIDs below (nsenter, the Engine's CPU time) are its own
ip link set lo up
mkdir -p "$R"
mount -o loop "$W/SC5000/rootfs.ext2" "$R"
for d in tmp run dev data sys; do mount -t tmpfs tmpfs "$R/$d"; done
mkdir -p "$R/dev/shm" "$R/dev/pts" "$R/sys/firmware/devicetree/base" "$R/sys/devices/system/cpu" "$R/run/dbus" "$R/tmp/home" \
    "$R/data/enginex/ui/packs" "$R/data/music"
chroot "$R" /bin/mkdir -p /var/volatile/tmp /var/volatile/log /var/volatile/cache
for f in online possible present; do echo 0-3 > "$R/sys/devices/system/cpu/$f"; done
# Real OpenGL like the shadow-twin: the host's DRM device with a headless 1280 x 800 screen and the host's armhf Mesa (llvmpipe).
mkdir -p "$R/dev/dri"
mount --bind /dev/dri "$R/dev/dri"
for d in class devices bus; do
    mkdir -p "$R/sys/$d"
    mount --bind "/sys/$d" "$R/sys/$d"
    mount -o remount,bind,ro "$R/sys/$d"
done
mkdir -p "$R/usr/lib/arm-linux-gnueabihf"
mount --bind /usr/lib/arm-linux-gnueabihf "$R/usr/lib/arm-linux-gnueabihf"
mount -o remount,bind,ro "$R/usr/lib/arm-linux-gnueabihf"
mkdir -p "$R/usr/share/glvnd" "$R/usr/share/drirc.d"
for d in glvnd drirc.d; do [ -d /usr/share/$d ] && mount --bind /usr/share/$d "$R/usr/share/$d" && mount -o remount,bind,ro "$R/usr/share/$d"; done
mount --bind /usr/lib/arm-linux-gnueabihf/ld-linux-armhf.so.3 "$R/lib/ld-linux-armhf.so.3"
printf '{"device": "/dev/dri/card0", "headless": "1280x800", "hwcursor": false}\n' > "$W/screen.json"
for c in Default JP07; do
    [ -f "$R/usr/Engine/ScreenConfiguration/$c/ScreenConfiguration.json" ] &&
        mount --bind "$W/screen.json" "$R/usr/Engine/ScreenConfiguration/$c/ScreenConfiguration.json"
done
for n in null zero full random urandom; do touch "$R/dev/$n"; mount --bind "/dev/$n" "$R/dev/$n"; done
printf 'JP07\0' > "$R/sys/firmware/devicetree/base/inmusic,product-code"
printf 'inmusic,jp07\0rockchip,rk3288\0' > "$R/sys/firmware/devicetree/base/compatible"
mount -t proc -o ro proc "$R/proc"
printf '           CPU0\n 41:        100     GICv2  32 Level     ttyS0\n' > "$W/interrupts"
mount --bind "$W/interrupts" "$R/proc/interrupts"
mount -t tmpfs tmpfs "$R/proc/irq"
mkdir -p "$R/proc/irq/41"
printf 'f\n' > "$R/proc/irq/41/smp_affinity"
printf '0-3\n' > "$R/proc/irq/41/smp_affinity_list"
# The hook only starts when LD_PRELOAD names libengine-ui-hook.so.
cp "$REPO/rootfs-overlay/etc/enginex/libengine-ui-hook.so" "$R/tmp/libengine-ui-hook.so"
cp "$W/alsa-null-fixture.so" "$R/tmp/alsa-null-fixture.so"
cp "$W/track.wav" "$R/data/music/share-test.wav"
cp "$W/suite" "$R/tmp/shadow-twin-sync"; ln -sf shadow-twin-sync "$R/tmp/stream-sync"
cp -a "$W/packs/en" "$R/data/enginex/ui/packs/"
mkdir -p "$R/data/enginex/ui/packs/en/Skins/Common/Items"
sed 's|@MARK@|http://10.79.0.1:8099|' "$HERE/share-decks-probe.qml" > "$R/data/enginex/ui/packs/en/Skins/Common/Items/QmlWarnings.qml"
printf 'lang=en\nshare_decks=1\n' > "$R/data/enginex/ui/settings.conf" # the Stock layout: no four_deck_waveform
chroot "$R" /usr/bin/dbus-daemon --system --fork --nopidfile > "$W/dbus.log" 2>&1 || true
pids=()
trap 'kill "${pids[@]}" 2>/dev/null || true' EXIT
# The console: a second network namespace on a veth, its own /data and /run and device tree (JC11), Line In on.
unshare -n sleep 900 & HOLD=$!
pids+=("$HOLD")
sleep 0.5
ip link add vp0 type veth peer name vc0
ip link set vc0 netns "$HOLD"
ip addr add 10.79.0.2/24 dev vp0; ip link set vp0 up
nsenter -t "$HOLD" -n ip addr add 10.79.0.1/24 dev vc0
nsenter -t "$HOLD" -n ip link set vc0 up
nsenter -t "$HOLD" -n ip link set lo up
printf 'JC11\0' > "$W/c-code"; printf 'inmusic,jc11\0rockchip,rk3288\0' > "$W/c-compat"
nsenter -t "$HOLD" -n -- unshare -m --propagation private bash -c '
    R=$1; W=$2
    mount -t tmpfs tmpfs "$R/data"; mount -t tmpfs tmpfs "$R/run"
    mount --bind "$W/c-code" "$R/sys/firmware/devicetree/base/inmusic,product-code"
    mount --bind "$W/c-compat" "$R/sys/firmware/devicetree/base/compatible"
    mkdir -p "$R/data/enginex/ui" "$R/run/enginex"
    printf "lang=en\nconsole_line_view=1\n" > "$R/data/enginex/ui/settings.conf"
    exec chroot "$R" /usr/bin/env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin HOME=/tmp/home /tmp/stream-sync --deemixd= --disable-guard --listen :8080
' _ "$R" "$W" > "$W/console-sync.log" 2>&1 &
pids+=($!)
# The player's own stream-sync, in this namespace, before its Engine (the QML reads its settings from it).
chroot "$R" /usr/bin/env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin HOME=/tmp/home /tmp/stream-sync --deemixd= --disable-guard --listen :8080 \
    > "$W/player-sync.log" 2>&1 &
pids+=($!)
python3 -I - <<'PY'
import socket, time
for _ in range(150):
    try:
        with socket.create_connection(("127.0.0.1", 8080), 0.1): break
    except OSError: time.sleep(0.1)
PY
nsenter -t "$HOLD" -n python3 -I "$HERE/share_decks_sandbox_driver.py" console 8099 > "$W/console.log" 2>&1 &
CONSOLE_DRIVER=$!
pids+=("$CONSOLE_DRIVER")
python3 -I "$HERE/share_decks_sandbox_driver.py" player > "$W/player.log" 2>&1 &
pids+=($!)
engine_rc=0
timeout --kill-after=5s 360s chroot "$R" /usr/bin/env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin HOME=/tmp/home \
    LD_LIBRARY_PATH=/usr/lib/arm-linux-gnueabihf:/usr/lib:/usr/qt/lib \
    LD_PRELOAD=/tmp/alsa-null-fixture.so:/tmp/libengine-ui-hook.so SBX_CARD=JP07 SBX_OUT=4 SBX_IN=2 SBX_CLOCK_HZ=48000 SBX_HEADLESS_VSYNC=1 \
    QT_QPA_PLATFORM=eglfs QT_QPA_EGLFS_INTEGRATION=eglfs_kms LIBGL_ALWAYS_SOFTWARE=1 GALLIUM_DRIVER=llvmpipe LP_NUM_THREADS=2 \
    QML_DISABLE_DISK_CACHE=1 MALLOC_ARENA_MAX=1 /usr/Engine/Engine -d0 > "$W/engine.log" 2>&1 || engine_rc=$?
echo "engine rc=$engine_rc"
# The console's driver ends on its own 89 s after READY.
for _ in $(seq 1 30); do kill -0 "$CONSOLE_DRIVER" 2>/dev/null || break; sleep 1; done
grep -aq "engine-ui-hook: Engine " "$W/engine.log" || echo "the hook did not start"
exit 0
