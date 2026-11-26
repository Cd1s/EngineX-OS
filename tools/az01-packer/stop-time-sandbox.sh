#!/bin/bash
# The repository from this script's own path: remote-build copies have no .git.
. "${ENGINEX_MIGRATION:-$(cd "$(dirname "$0")/../.." && pwd)/rootfs-overlay/usr/libexec/enginex-migration}"
ENGINEX_LAB=${ENGINEX_LAB:-$legacy_lab}
# Stop Time knob (#329): a real track on Deck 1 of a player's own Engine 5.3.0 and UI hook, natively on the build host (no
# qemu, host services or deck connections). stop-time-probe.qml builds the knob as the board's assignment file does (the
# stock StopTime module; deckConfig as the stock file has it, or, in the 4-deck arm, as engine-ui-guard's own rewrite of
# that file names it). On the motorized boards (their assignment has a Motorized module: SC5000M, SC6000M) it checks that
# the motor's stop time, bound the way the Motorized module Engine serves binds it, follows the knob; on the others it
# measures Engine's brake after PLAY with the knob at the top and at the bottom (a motorized board loads no track here: its
# platter is missing). Each board runs the stock 2-deck layout (arm 2deck) and the hook's 4-deck layout (arm 4deck:
# four_deck_waveform=1, four_deck_role=13).
#   bash tools/az01-packer/stop-time-sandbox.sh <new-private-dir> [BOARD ...]      (default: JP07 JP08; JP14 also runs)
#   STOP_TIME_ARMS="2deck 4deck" picks the arms. Run it under `enginex-run exclusive` (timing, one GPU); it takes no lock.
set -euo pipefail
REPO=$(cd "$(dirname "$0")/../.." && pwd)
W=$(realpath -m "${1:?private sandbox directory required}")
LAB=${ENGINEX_LAB}
image_of() {
    case "$1" in
        JP07|JP08) echo fw-5.3.0/SC5000 ;;
        JP14) echo fw-5.3.0/SC6000 ;;
        # A track never loads on JP13 in this sandbox (as on the motorized boards), and JP13 has no motor, so neither
        # check could run; its knob and brake are the stock modules JP07 runs.
        JP13) echo "JP13: no track loads in this sandbox and it has no motor: nothing to check (JP07 covers its knob and brake)" >&2; return 1 ;;
        *) echo "not a player: $1" >&2; return 1 ;;
    esac
}
if [ "${2:-}" != --inner ]; then
    [ "$(uname -m)" = aarch64 ] || { echo 'native ARM build host (aarch64) required' >&2; exit 1; }
    [ ! -e "$W" ] || { echo "refusing existing sandbox $W" >&2; exit 1; }
    # The arms, checked before anything is built or started: a selector that names no arm (empty or blanks only) or
    # an unknown one is an error, never a run of zero arms that reports success.
    # The whole value, every line (read stops at a newline without -d ''; at the end of input it returns 1).
    IFS=$' \t\n' read -r -d '' -a arms <<< "${STOP_TIME_ARMS-2deck 4deck}" || true
    [ ${#arms[@]} -gt 0 ] || { echo "STOP_TIME_ARMS names no arm (use 2deck and/or 4deck)" >&2; exit 1; }
    for arm in "${arms[@]}"; do
        case $arm in 2deck|4deck) ;; *) echo "unknown arm $arm (use 2deck and/or 4deck)" >&2; exit 1 ;; esac
    done
    boards=("${@:2}"); [ ${#boards[@]} -gt 0 ] || boards=(JP07 JP08)
    for board in "${boards[@]}"; do image_of "$board" > /dev/null; done
    mkdir -p "$W"
    for board in "${boards[@]}"; do
        image=$(image_of "$board")
        dir=$W/$(basename "$image")
        [ -e "$dir" ] || python3 "$REPO/tools/az01-packer/az01_packer.py" unpack "$LAB/$image-5.3.0-Update.img" --output "$dir" > "$dir.log"
    done
    python3 "$REPO/tools/ui-mod/build_packs.py" --qrc "$LAB/5.3.0/qrc" --out "$W/packs" > "$W/packs.log"
    # Engine only loads and plays tracks with an audio card: the shadow-twin's sandbox-only ALSA null card on a monotonic clock.
    arm-linux-gnueabihf-gcc -O2 -shared -fPIC -Wall -Wextra -o "$W/alsa-null-fixture.so" "$REPO/tools/shadow-twin/alsa_null_fixture.c" -ldl
    # Non-copyright test track: 60 s of a 128 BPM kick, so the deck's position moves at every sample.
    python3 -I - "$W/track.wav" <<'PY'
import array, math, sys, wave
rate, seconds, bpm = 44100, 60, 128.0
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
    # The 4-deck arm's deckConfig comes from the guard's own assignment rewrite, cut out of engine-ui-guard.
    sed -n "/cat > \"\$patched\/role.awk\" <<'AWK'/,/^AWK\$/p" "$REPO/rootfs-overlay/etc/enginex/engine-ui-guard" | sed '1d;$d' > "$W/role.awk"
    grep -q 'id: deckConfig' "$W/role.awk" || { echo "engine-ui-guard no longer rewrites deckConfig: update this sandbox" >&2; exit 1; }
    fail=0
    # The child checks that it really runs in its own mount and network namespaces before its first mount.
    export ENGINEX_OUTER_MNTNS ENGINEX_OUTER_NETNS
    ENGINEX_OUTER_MNTNS=$(readlink /proc/self/ns/mnt)
    ENGINEX_OUTER_NETNS=$(readlink /proc/self/ns/net)
    for board in "${boards[@]}"; do
        for arm in "${arms[@]}"; do
            unshare -m -n -p -f --kill-child --propagation private bash "$0" "$W" --inner "$board" "$arm" || fail=1
        done
    done
    [ "$fail" = 0 ] && echo "STOP_TIME RESULT|PASS|${boards[*]}" || echo "STOP_TIME RESULT|FAIL"
    exit $fail
fi
# --inner is this script's own re-exec. Passed by hand it would skip the unshare above, and the mounts below would land
# in the caller's namespaces (the build host's): refuse unless both namespaces really changed.
if [ -z "${ENGINEX_OUTER_MNTNS:-}" ] || [ -z "${ENGINEX_OUTER_NETNS:-}" ] ||
    [ "$(readlink /proc/self/ns/mnt)" = "$ENGINEX_OUTER_MNTNS" ] || [ "$(readlink /proc/self/ns/net)" = "$ENGINEX_OUTER_NETNS" ]; then
    echo "stop-time-sandbox.sh: --inner is this script's own re-exec flag; run it as: bash $0 <new-private-dir> [BOARD ...]" >&2
    exit 2
fi
board=${3:?board required}
arm=${4:?arm required}
image=$(image_of "$board")
R=$W/$board-$arm-root
lower=$(echo "$board" | tr A-Z a-z)
mount --make-rprivate /
ip link set lo up
mkdir -p "$R"
mount -o loop "$W/$(basename "$image")/rootfs.ext2" "$R"
for d in tmp run dev data sys; do mount -t tmpfs tmpfs "$R/$d"; done
mkdir -p "$R/dev/shm" "$R/dev/pts" "$R/sys/firmware/devicetree/base" "$R/sys/devices/system/cpu" "$R/run/dbus" "$R/tmp/home" \
    "$R/data/enginex/ui/packs" "$R/data/music" "$R/data/inMusicTempFiles"
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
printf '{"device": "/dev/dri/card0", "headless": "1280x800", "hwcursor": false}\n' > "$W/$board-$arm-screen.json"
for c in Default "$board"; do
    [ -f "$R/usr/Engine/ScreenConfiguration/$c/ScreenConfiguration.json" ] &&
        mount --bind "$W/$board-$arm-screen.json" "$R/usr/Engine/ScreenConfiguration/$c/ScreenConfiguration.json"
done
for n in null zero full random urandom; do touch "$R/dev/$n"; mount --bind "/dev/$n" "$R/dev/$n"; done
printf '%s\0' "$board" > "$R/sys/firmware/devicetree/base/inmusic,product-code"
printf "inmusic,$lower\0rockchip,rk3288\0" > "$R/sys/firmware/devicetree/base/compatible"
mount -t proc -o ro proc "$R/proc"
printf '           CPU0\n 41:        100     GICv2  32 Level     ttyS0\n' > "$W/$board-$arm-interrupts"
mount --bind "$W/$board-$arm-interrupts" "$R/proc/interrupts"
mount -t tmpfs tmpfs "$R/proc/irq"
mkdir -p "$R/proc/irq/41"
printf 'f\n' > "$R/proc/irq/41/smp_affinity"
printf '0-3\n' > "$R/proc/irq/41/smp_affinity_list"
# The hook only starts when LD_PRELOAD names libengine-ui-hook.so.
cp "$REPO/rootfs-overlay/etc/enginex/libengine-ui-hook.so" "$R/tmp/libengine-ui-hook.so"
cp "$W/alsa-null-fixture.so" "$R/tmp/alsa-null-fixture.so"
cp "$W/track.wav" "$R/data/music/stop-test.wav"
cp -a "$W/packs/en" "$R/data/enginex/ui/packs/"
mkdir -p "$R/data/enginex/ui/packs/en/Skins/Common/Items"
# deckConfig of the knob: the stock file leaves the name empty; the 4-deck rewrite sets what the guard prints after
# "id: deckConfig" (a binding on the stock name property, here a property of the stand-in object).
stock=$R/usr/Engine/AssignmentFiles/PresetAssignmentFiles/$board/${board}_Controller_Assignments.qml
grep -q '^    StopTime {' "$stock" || { echo "$board: the stock assignment has no StopTime knob" >&2; exit 1; }
printf '        property string name\n' > "$W/$board-$arm-deckconfig.txt"
if [ "$arm" = 4deck ]; then
    awk -v role=13 -f "$W/role.awk" "$stock" > "$W/$board-$arm-assignments.qml"
    awk '/^        id: deckConfig$/ { on = 1; next } on && /^        \/\/ EngineX/ { next } on && /^        (name|readonly property var scDecks):/ { print; next } { on = 0 }' \
        "$W/$board-$arm-assignments.qml" | sed 's/^        name: /        property string name: /' > "$W/$board-$arm-deckconfig.txt"
    grep -q 'property string name: ' "$W/$board-$arm-deckconfig.txt" || { echo "$board: no deckConfig name in the guard's rewrite" >&2; exit 1; }
    printf 'four_deck_waveform=1\nfour_deck_role=13\n' > "$W/$board-$arm-fourdeck.conf"
fi
sed 's/^/  deckConfig| /' "$W/$board-$arm-deckconfig.txt"
# What this arm must show before any trial counts: the layout (Config.device.isPlayer), the knob's deck name and its
# exact target. A 4-deck arm whose layout switch failed would otherwise pass on the stock property.
case $arm in
    2deck) expect_player=true; expect_name=; expect_path=/Client/Preferences/RampDownTime ;;
    4deck) expect_player=false; expect_name=Left; expect_path=/Client/Preferences/RampDownTime/DeckLeft ;;
    *) echo "unknown arm $arm" >&2; exit 1 ;;
esac
# The motor's ramp-down lines from the Motorized module Engine serves: the pack's copy when the pack replaces it.
: > "$W/$board-$arm-motor.txt"
brake=${STOP_TIME_BRAKE:-1}
case $board in JP08|JP14)
    grep -q '^    Motorized {' "$stock" || { echo "$board: the stock assignment has no Motorized module" >&2; exit 1; } ;;
esac
if grep -q '^    Motorized {' "$stock"; then
    brake=${STOP_TIME_BRAKE:-0}
    rel=ControlSurfaceModules/ControlSurfaceModules/Motorized.qml
    module=$W/packs/en/$rel; [ -f "$module" ] || module=$LAB/5.3.0/qrc/$rel
    # The GUI engine cannot hold the controller engine's property type (QObjProperty there is a QThreadAwareProperty:
    # "Unable to assign QObjProperty to QThreadAwareProperty"), so the stand-in declares those properties as var: the
    # binding expressions themselves run unchanged.
    grep -E 'RampDownTime|rampDownTime' "$module" | sed 's/readonly property QObjProperty /readonly property var /' > "$W/$board-$arm-motor.txt"
    [ -s "$W/$board-$arm-motor.txt" ] || { echo "$board: no ramp-down lines in $module" >&2; exit 1; }
    sed 's/^/  motor| /' "$W/$board-$arm-motor.txt"
fi
python3 -I - "$REPO/tools/az01-packer/stop-time-probe.qml" "$R/data/enginex/ui/packs/en/Skins/Common/Items/QmlWarnings.qml" \
    "$board" "$arm" "$W/$board-$arm-deckconfig.txt" "$W/$board-$arm-motor.txt" "$brake" \
    "$expect_player" "$expect_name" "$expect_path" <<'PY'
import sys
src, out, board, arm, deck, motor, brake, player, name, path = sys.argv[1:]
def js(path):  # the lines, as the inside of a JavaScript string literal
    lines = open(path, encoding="utf-8").read().rstrip("\n")
    return lines.replace("\\", "\\\\").replace('"', '\\"').replace("\n", "\\n")
text = open(src, encoding="utf-8").read()
for key, value in (("@BOARD@", board), ("@ARM@", arm), ("@DECKCONFIG@", js(deck)), ("@MOTOR@", js(motor)), ("@BRAKE@", brake),
                   ("@EXPECT_PLAYER@", player), ("@EXPECT_NAME@", name), ("@EXPECT_PATH@", path)):
    text = text.replace(key, value)
open(out, "w", encoding="utf-8").write(text)
PY
printf 'lang=en\n' > "$R/data/enginex/ui/settings.conf"
[ "$arm" != 4deck ] || cat "$W/$board-$arm-fourdeck.conf" >> "$R/data/enginex/ui/settings.conf"
chroot "$R" /usr/bin/dbus-daemon --system --fork --nopidfile > "$W/$board-$arm-dbus.log" 2>&1 || true
log=$W/$board-$arm-engine.log
engine_rc=0
# The probe imports ControlSurfaceModules from Engine's qrc, as the controller engine does.
timeout --kill-after=5s "${STOP_TIME_TIMEOUT:-400}s" chroot "$R" /usr/bin/env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin HOME=/tmp/home \
    LD_LIBRARY_PATH=/usr/lib/arm-linux-gnueabihf:/usr/lib:/usr/qt/lib \
    LD_PRELOAD=/tmp/alsa-null-fixture.so:/tmp/libengine-ui-hook.so SBX_CARD=$board SBX_OUT=4 SBX_IN=2 SBX_CLOCK_HZ=48000 SBX_HEADLESS_VSYNC=1 \
    QT_QPA_PLATFORM=eglfs QT_QPA_EGLFS_INTEGRATION=eglfs_kms LIBGL_ALWAYS_SOFTWARE=1 GALLIUM_DRIVER=llvmpipe LP_NUM_THREADS=2 \
    QML_IMPORT_PATH=:/airAssignments::/ControlSurfaceModules QML_DISABLE_DISK_CACHE=1 MALLOC_ARENA_MAX=1 \
    /usr/Engine/Engine -d0 > "$log" 2>&1 || engine_rc=$?
grep -aq "engine-ui-hook: Engine " "$log" || { echo "$board $arm: the hook did not start (Engine exit $engine_rc)" >&2; exit 1; }
grep -aE 'X329 (ST|ERROR|DONE)' "$log" | sed 's/ *\[20[0-9T:.Z-]*\].*//; s/^\[W\] //' | cut -c1-230
grep -a -E '^\[W\] (qrc|file):/.*(stop-time|StopTime)' "$log" | sed 's/ *\[20[0-9T:.Z-]*\].*//' | sort -u | head -n 6 || true
if grep -aq "X329 DONE|PASS|$board-$arm|" "$log"; then echo "$board $arm STOP_TIME_PASS"; exit 0; fi
echo "$board $arm STOP_TIME_FAIL (Engine exit $engine_rc)"
exit 1
