#!/bin/bash
. "${ENGINEX_MIGRATION:-$(git rev-parse --show-toplevel)/rootfs-overlay/usr/libexec/enginex-migration}"
ENGINEX_LAB=${ENGINEX_LAB:-$legacy_lab}
# Hot cue countdown (#151): a real track with hot cues loaded on Deck 1 of a first-generation model's own Engine and UI hook,
# natively on the build host (no qemu, host services or deck connections). The probe moves the pitch fader and edits the beat
# grid through Engine's own controls and checks what the countdown shows.
# bash tools/az01-packer/cue-countdown-sandbox.sh <new-private-dir> [BOARD ...]     (default: JP07; JC11 loads the track but its analysis never completes in this sandbox)
set -euo pipefail
REPO=$(cd "$(dirname "$0")/../.." && pwd)
W=$(realpath -m "${1:?private sandbox directory required}")
LAB=${ENGINEX_LAB}
image_of() {
    case "$1" in
        JC11) echo fw-5.3.0/MIXSTREAMPRO ;;
        JP07) echo fw-5.3.0/SC5000 ;;
        *) echo "unsupported board $1" >&2; return 1 ;;
    esac
}
if [ "${2:-}" != --inner ]; then
    [ "$(uname -m)" = aarch64 ] || { echo 'native ARM build host (aarch64) required' >&2; exit 1; }
    [ ! -e "$W" ] || { echo "refusing existing sandbox $W" >&2; exit 1; }
    mkdir -p "$W"
    boards=("${@:2}"); [ ${#boards[@]} -gt 0 ] || boards=(JP07)
    for board in "${boards[@]}"; do
        image=$(image_of "$board")
        dir=$W/$(basename "$image")
        [ -e "$dir" ] || python3 "$REPO/tools/az01-packer/az01_packer.py" unpack "$LAB/$image-5.3.0-Update.img" --output "$dir" > "$dir.log"
    done
    python3 "$REPO/tools/ui-mod/build_packs.py" --qrc "$LAB/5.3.0/qrc" --out "$W/packs" > "$W/packs.log"
    # Engine only loads and plays tracks with an audio card: the shadow-twin's sandbox-only ALSA null card on a monotonic clock.
    arm-linux-gnueabihf-gcc -O2 -shared -fPIC -Wall -Wextra -o "$W/alsa-null-fixture.so" "$REPO/tools/shadow-twin/alsa_null_fixture.c" -ldl
    # Non-copyright test track: 128 BPM kick every beat (stronger on the bar), hat on the off-beats, first beat at 0.30 s.
    python3 - "$W/track.wav" <<'PY'
import array, math, random, sys, wave
rate, seconds, bpm, first = 44100, 90, 128.0, 0.30
buf = array.array('h', bytes(rate * seconds * 2 * 2))
random.seed(7)
beat = 0
while True:
    start = int(round((first + beat * 60.0 / bpm) * rate))
    if start + rate // 4 >= rate * seconds: break
    amp = 30000 if beat % 4 == 0 else 24000
    for n in range(rate // 5):
        v = int(amp * math.exp(-n / 2200.0) * math.sin(2 * math.pi * (55 + 90 * math.exp(-n / 600.0)) * n / rate))
        buf[(start + n) * 2] += v; buf[(start + n) * 2 + 1] += v
    off = int(round((first + (beat + 0.5) * 60.0 / bpm) * rate))
    for n in range(rate // 30):
        v = int(9000 * math.exp(-n / 300.0) * (random.random() * 2 - 1))
        if (off + n) * 2 + 1 < len(buf): buf[(off + n) * 2] += v; buf[(off + n) * 2 + 1] += v
    beat += 1
with wave.open(sys.argv[1], 'wb') as w:
    w.setnchannels(2); w.setsampwidth(2); w.setframerate(rate); w.writeframes(buf.tobytes())
PY
    fail=0
    for board in "${boards[@]}"; do
        unshare -m -n -p -f --kill-child bash "$0" "$W" --inner "$board" || fail=1
    done
    exit $fail
fi
board=${3:?board required}
image=$(image_of "$board")
R=$W/$board-root
lower=$(echo "$board" | tr A-Z a-z)
mount --make-rprivate /
ip link set lo up
mkdir -p "$R"
mount -o loop "$W/$(basename "$image")/rootfs.ext2" "$R"
for d in tmp run dev data sys; do mount -t tmpfs tmpfs "$R/$d"; done
mkdir -p "$R/dev/shm" "$R/dev/pts" "$R/sys/firmware/devicetree/base" "$R/sys/devices/system/cpu" "$R/run/dbus" "$R/tmp/home" "$R/data/enginex/ui/packs" "$R/data/music" "$R/data/inMusicTempFiles" "$R/data/AppDataUser/AIR Music Technology/Engine/State"
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
printf '{"device": "/dev/dri/card0", "headless": "1280x800", "hwcursor": false}\n' > "$W/$board-screen.json"
for c in Default "$board"; do [ -f "$R/usr/Engine/ScreenConfiguration/$c/ScreenConfiguration.json" ] && mount --bind "$W/$board-screen.json" "$R/usr/Engine/ScreenConfiguration/$c/ScreenConfiguration.json"; done
for n in null zero full random urandom; do touch "$R/dev/$n"; mount --bind "/dev/$n" "$R/dev/$n"; done
printf '%s\0' "$board" > "$R/sys/firmware/devicetree/base/inmusic,product-code"
printf "inmusic,$lower\0rockchip,rk3288\0" > "$R/sys/firmware/devicetree/base/compatible"
mount -t proc -o ro proc "$R/proc"
printf '           CPU0\n 41:        100     GICv2  32 Level     ttyS0\n' > "$W/$board-interrupts"
mount --bind "$W/$board-interrupts" "$R/proc/interrupts"
mount -t tmpfs tmpfs "$R/proc/irq"
mkdir -p "$R/proc/irq/41"
printf 'f\n' > "$R/proc/irq/41/smp_affinity"
printf '0-3\n' > "$R/proc/irq/41/smp_affinity_list"
# The hook only starts when LD_PRELOAD names libengine-ui-hook.so.
cp "$REPO/rootfs-overlay/etc/enginex/libengine-ui-hook.so" "$R/tmp/libengine-ui-hook.so"
cp "$W/alsa-null-fixture.so" "$R/tmp/alsa-null-fixture.so"
cp "$W/track.wav" "$R/data/music/cd-test.wav"
cp -a "$W/packs/en" "$R/data/enginex/ui/packs/"
mkdir -p "$R/data/enginex/ui/packs/en/Skins/Common/Items"
sed "s/@BOARD@/$board/" "$REPO/tools/az01-packer/cue-countdown-probe.qml" > "$R/data/enginex/ui/packs/en/Skins/Common/Items/QmlWarnings.qml"
printf 'lang=en\n' > "$R/data/enginex/ui/settings.conf"
chroot "$R" /usr/bin/dbus-daemon --system --fork --nopidfile > "$W/$board-dbus.log" 2>&1 || true
engine_rc=0
timeout --kill-after=5s "${CUE_COUNTDOWN_TIMEOUT:-420}s" chroot "$R" /usr/bin/env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin HOME=/tmp/home LD_LIBRARY_PATH=/usr/lib/arm-linux-gnueabihf:/usr/lib:/usr/qt/lib \
    LD_PRELOAD=/tmp/alsa-null-fixture.so:/tmp/libengine-ui-hook.so SBX_CARD=$board SBX_OUT=4 SBX_IN=2 SBX_CLOCK_HZ=48000 SBX_HEADLESS_VSYNC=1 \
    QT_QPA_PLATFORM=eglfs QT_QPA_EGLFS_INTEGRATION=eglfs_kms LIBGL_ALWAYS_SOFTWARE=1 GALLIUM_DRIVER=llvmpipe LP_NUM_THREADS=2 \
    QML_DISABLE_DISK_CACHE=1 QML_XHR_ALLOW_FILE_READ=1 MALLOC_ARENA_MAX=1 /usr/Engine/Engine -d0 > "$W/$board-engine.log" 2>&1 || engine_rc=$?
echo "X166 ENGINE_EXIT|$engine_rc|" >> "$W/$board-engine.log"
cp "$R"/tmp/cd-*.png "$W/" 2>/dev/null || true
grep -aq "engine-ui-hook: Engine " "$W/$board-engine.log" || { echo "$board: the hook did not start" >&2; exit 1; }
grep -aE 'X166 (CD|ERROR|DONE)' "$W/$board-engine.log" | sed 's/ *\[20[0-9T:.Z-]*\].*//; s/^\[W\] //' | cut -c1-200
if grep -aq 'X166 ERROR|' "$W/$board-engine.log" || ! grep -aq 'X166 DONE|PASS|' "$W/$board-engine.log"; then
    echo "$board CUE_COUNTDOWN_FAIL"
    exit 1
fi
echo "$board CUE_COUNTDOWN_PASS"
