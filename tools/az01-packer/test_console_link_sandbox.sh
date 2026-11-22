#!/bin/bash
. "${ENGINEX_MIGRATION:-$(git rev-parse --show-toplevel)/rootfs-overlay/usr/libexec/enginex-migration}"
ENGINEX_LAB=${ENGINEX_LAB:-$legacy_lab}
# Console linking, stage 1 (console_line_view): a linked player on a console channel that is on Line / AUX.
# Build host only (aarch64, native armhf), run under the host's heavy lock:
#     flock /var/lock/enginex-heavy.lock bash tools/az01-packer/test_console_link_sandbox.sh
# A private JC11 (PRIME 4) rootfs, the real 5.3.0 Engine with the real UI hook and the pack built from this tree. No real
# deck is touched; every mount is inside `unshare -m`, below this script's own directory, none under /dev, /run, /tmp or /etc.
#   stub  console_link_stub.py stands in for stream-sync and counts every request by kind: the zero-cost rows (offscreen)
#   pair  the real stream-sync (this tree, armhf) on the console and on a second unit, a linked player (board JP13, role 3+4) in
#         its own network namespace on a veth: the deck loop and UDP 50199 per phase, what the console's stream-sync would answer,
#         and that the player sees no console deck
#   shots the stub again, drawn by eglfs at 1280x800 on the host's DRM device (as tools/shadow-twin does): the three captures,
#         the lanes drawn on channels 2-4 with their labels, the reason line of a CDJ lane with Pro DJ Link off, and the totals of pictures fetched (software rendering is slow, so no
#         per-minute rate is taken from this run; the offscreen window is 800x800, a layout without a waveform area, so labels are
#         asserted only here); skipped without /dev/dri/card0
# All run console-link-probe.qml in the console's Engine, which walks nine phases (off, on, no Line channel, channel 3 on
# Line, channels 3 and 4, channel 4 released, channels 2-4 with three linked units, a CDJ on channel 3 with Pro DJ Link off, all
# released, off) and logs what the QML holds; console_link_sandbox_check.py
# asserts it. Captures of three states end up in $W/shots/shots. `--clean` removes this script's own directory (checked first).
# The Go suite and the stub need nothing from the host but python3; test_prime4_views_sandbox.sh covers the 4-deck gates on a console.
set -euo pipefail
REPO=$(cd "$(dirname "$0")/../.." && pwd)
HERE=$REPO/tools/az01-packer
W=${CL_SANDBOX_DIR:-/var/tmp/console-link-sandbox}
LAB=${ENGINEX_LAB}

if [ "${1:-}" = --clean ]; then
    python3 -I - "$W" <<'PY'
import os, shutil, sys
t = sys.argv[1]
mounts = [l.split()[1] for l in open("/proc/mounts") if l.split()[1].startswith(t)]
if mounts:
    sys.exit("refusing: mounted below %s: %s" % (t, mounts))
if os.path.isdir(t) and os.path.realpath(t) == t and os.path.basename(t).startswith("console-link-"):
    shutil.rmtree(t)
    print("removed", t)
else:
    print("nothing to remove at", t)
PY
    exit 0
fi

if [ "${1:-}" != --inner ]; then
    test "$(uname -m)" = aarch64
    mkdir -p "$W"
    exec 9> "$W/sandbox.lock"
    flock -n 9 || { echo "console link sandbox already in use" >&2; exit 1; }
    if [ ! -e "$W/image/rootfs.ext2" ]; then
        python3 "$HERE/az01_packer.py" unpack "$LAB/fw-5.3.0/PRIME4-5.3.0-Update.img" --output "$W/image" > "$W/unpack.json"
    fi
    python3 "$REPO/tools/ui-mod/build_packs.py" --qrc "$LAB/5.3.0/qrc" --out "$W/packs" > "$W/packs.log"
    case " ${CL_MODES:-stub pair shots} " in *" pair "*)
        ( . /etc/profile.d/buildtools.sh
          cd "$REPO" && GOCACHE="$W/gocache" GOFLAGS=-buildvcs=false GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -o "$W/suite" ./cmd/enginex-suite ) ;;
    esac
    status=0
    for mode in ${CL_MODES:-stub pair shots}; do
        if [ "$mode" = shots ] && [ ! -e /dev/dri/card0 ]; then echo "shots: no /dev/dri/card0, skipped"; continue; fi
        mkdir -p "$W/$mode/shots"
        : > "$W/$mode/engine.log"; : > "$W/$mode/stats.jsonl"; : > "$W/$mode/sample.log"; : > "$W/$mode/player.log"
        export CL_MODE=$mode
        unshare -m -n -p -f --kill-child bash "$0" --inner || true
        python3 -I "$HERE/console_link_sandbox_check.py" "$mode" "$W/$mode" || status=1
    done
    exit $status
fi

mode=$CL_MODE
O=$W/$mode
R=$O/root
mount --make-rprivate /
mount -t proc proc /proc # the PIDs used below (nsenter, ip link set ... netns) are this namespace's, not the host's
ip link set lo up
mkdir -p "$R"
mount -o loop,ro "$W/image/rootfs.ext2" "$R"
for d in tmp run dev data sys; do mount -t tmpfs tmpfs "$R/$d"; done
mkdir -p "$R/dev/shm" "$R/dev/pts"
for n in null zero full random urandom; do touch "$R/dev/$n"; mount --bind "/dev/$n" "$R/dev/$n"; done
for d in class devices bus; do
    mkdir -p "$R/sys/$d"; mount --bind "/sys/$d" "$R/sys/$d"; mount -o remount,bind,ro "$R/sys/$d"
done
mkdir -p "$R/sys/firmware/devicetree/base"
printf JC11 > "$R/sys/firmware/devicetree/base/inmusic,product-code"
printf 'inmusic,jc11\0rockchip,rk3288\0' > "$R/sys/firmware/devicetree/base/compatible"
mount -t proc proc "$R/proc"
printf '           CPU0\n 41:        100     GICv2  32 Level     ttyS0\n' > "$O/interrupts"
mount --bind "$O/interrupts" "$R/proc/interrupts"
mount -t tmpfs tmpfs "$R/proc/irq"
mkdir -p "$R/proc/irq/41"; printf 'f\n' > "$R/proc/irq/41/smp_affinity"; printf '0-3\n' > "$R/proc/irq/41/smp_affinity_list"
UIROOT=$(sed -n 's/^#define UI_ROOT "\(.*\)"/\1/p' "$REPO/tools/ui-mod/engine_ui_hook.c" | head -1)
DATADIR=$(dirname "$UIROOT")
mkdir -p "$R/tmp/home" "$R/run/dbus" "$R$UIROOT/packs" "$R/tmp/fx-qml-imports"
cp -a "$LAB/5.3.0/qrc/airAssignments/InputAssignment" "$LAB/5.3.0/qrc/airAssignments/OutputAssignment" "$R/tmp/fx-qml-imports/"
cp "$REPO/rootfs-overlay/etc/enginex/libengine-ui-hook.so" "$R/tmp/libengine-ui-hook.so"
cp -a "$W/packs/en" "$R$UIROOT/packs/"
mkdir -p "$R$UIROOT/packs/en/Skins/Common/Items"
cp "$HERE/console-link-probe.qml" "$R$UIROOT/packs/en/Skins/Common/Items/QmlWarnings.qml"
printf 'lang=en\n' > "$R$UIROOT/settings.conf" # the Line In view is off: the first phase is the default
chroot "$R" /usr/bin/dbus-daemon --system --fork --nopidfile > "$O/dbus.log" 2>&1 || true
CHR=(chroot "$R" /usr/bin/env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin HOME=/tmp/home)
pids=()
trap 'kill "${pids[@]}" 2>/dev/null || true' EXIT

if [ "$mode" != pair ]; then
    python3 -I "$HERE/console_link_stub.py" "$O/stats.jsonl" > "$O/stub.log" 2>&1 &
    pids+=($!)
else
    cp "$W/suite" "$R/tmp/shadow-twin-sync"; ln -sf shadow-twin-sync "$R/tmp/stream-sync"
    "${CHR[@]}" /tmp/stream-sync --deemixd= --disable-guard --listen :8080 > "$O/console-sync.log" 2>&1 &
    pids+=($!)
    # the linked player: its own network namespace on a veth, its own /data and /run, board JP13, role 3+4
    unshare -n sleep 600 & HOLD=$!
    pids+=("$HOLD")
    sleep 0.5
    ip link add vc0 type veth peer name vp0
    ip link set vp0 netns "$HOLD"
    ip addr add 10.77.0.1/24 dev vc0; ip link set vc0 up
    nsenter -t "$HOLD" -n ip addr add 10.77.0.2/24 dev vp0
    nsenter -t "$HOLD" -n ip link set vp0 up
    nsenter -t "$HOLD" -n ip link set lo up
    printf JP13 > "$O/p-code"; printf 'inmusic,jp13\0rockchip,rk3288\0' > "$O/p-compat"
    nsenter -t "$HOLD" -n -- unshare -m --propagation private bash -c '
        R=$1; O=$2; DATADIR=$3
        mount -t tmpfs tmpfs "$R/data"; mount -t tmpfs tmpfs "$R/run"
        mount --bind "$O/p-code" "$R/sys/firmware/devicetree/base/inmusic,product-code"
        mount --bind "$O/p-compat" "$R/sys/firmware/devicetree/base/compatible"
        mkdir -p "$R$DATADIR/ui" "$R$DATADIR/Stream" "$R/run/enginex-assignments" "$R/run/enginex"
        head -c 4096 /dev/zero > "$R$DATADIR/Stream/a.mp3" # a file for the decks to name: a deck without one has no Key and no lane
        echo 34 > "$R/run/enginex-assignments/role"
        printf "lang=en\nfour_deck_waveform=1\nfour_deck_role=34\nshadow_mode=engine\n" > "$R$DATADIR/ui/settings.conf"
        exec chroot "$R" /usr/bin/env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin HOME=/tmp/home /tmp/stream-sync --deemixd= --disable-guard --listen :8080
    ' _ "$R" "$O" "$DATADIR" > "$O/player-sync.log" 2>&1 &
    pids+=($!)
fi
python3 - <<'PY'
import socket, time
for _ in range(100):
    try:
        with socket.create_connection(("127.0.0.1", 8080), 0.1): break
    except OSError: time.sleep(0.1)
PY
if [ "$mode" = pair ]; then
    nsenter -t "$HOLD" -n python3 -I "$HERE/console_link_pair_driver.py" player 290 > "$O/player.log" 2>&1 &
    pids+=($!)
    python3 -I "$HERE/console_link_pair_driver.py" sample "$O/engine.log" > "$O/sample.log" 2>&1 &
    pids+=($!)
fi
set +e
run_engine() { # <platform>: eglfs draws at 1280x800 on the host's DRM device (as the twin does), offscreen needs none
    local env=(QT_QPA_PLATFORM=$1)
    [ "$1" = eglfs ] && env+=(QT_QPA_EGLFS_INTEGRATION=eglfs_kms)
    timeout --kill-after=5s 300s "${CHR[@]}" LD_LIBRARY_PATH=/usr/qt/lib LD_PRELOAD=/tmp/libengine-ui-hook.so "${env[@]}" \
        QML_IMPORT_PATH=/tmp/fx-qml-imports QML2_IMPORT_PATH=/tmp/fx-qml-imports QML_DISABLE_DISK_CACHE=1 MALLOC_ARENA_MAX=1 /usr/Engine/Engine -d0 > "$O/engine.log" 2>&1
    echo "engine rc=$? mode=$mode platform=$1 uiroot=$UIROOT"
}
platform=offscreen
if [ "$mode" = shots ]; then
    platform=eglfs
    mkdir -p "$R/dev/dri"; mount --bind /dev/dri "$R/dev/dri" # into this namespace's own tmpfs /dev, not the host's
    printf '{"device": "/dev/dri/card0", "headless": "1280x800", "hwcursor": false}\n' > "$O/ScreenConfiguration.json"
    mount --bind "$O/ScreenConfiguration.json" "$R/usr/Engine/ScreenConfiguration/Default/ScreenConfiguration.json"
fi
run_engine "$platform"
set -e
sleep 2
for f in "$R"/tmp/cl-shot-*.png; do [ -s "$f" ] && cp "$f" "$O/shots/"; done
ls "$O/shots" | tr '\n' ' '; echo
