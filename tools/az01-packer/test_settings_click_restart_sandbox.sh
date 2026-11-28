#!/bin/bash
# #336 E4 Rule 0.1 end-to-end sandbox proof: boots real stock 5.3.0 Engine with the built
# UI pack and a freshly compiled ARMv7 enginex-suite stream-sync binary on 127.0.0.1:8080,
# verifies single-tap setting writes before restart, restarts the real enginex-suite process
# twice while Engine stays open (1: persisted ui.rev, 2: ui.rev removed -> ephemeral rev=1),
# and verifies immediate epoch recovery + post-restart single-tap persistence on disk.
#
# Run under enginex-run sandbox on an ARM build host:
#   enginex-run sandbox -- bash tools/az01-packer/test_settings_click_restart_sandbox.sh <new-private-dir> [BOARD]
set -euo pipefail
REPO=$(cd "$(dirname "$0")/../.." && pwd)
. "${ENGINEX_MIGRATION:-$REPO/rootfs-overlay/usr/libexec/enginex-migration}"
ENGINEX_LAB=${ENGINEX_LAB:-$legacy_lab}
W=$(realpath -m "${1:?new private sandbox directory required}")
LAB=${ENGINEX_LAB}
BUILDER=$(realpath -m "${E4_BUILDER:-$REPO/tools/ui-mod/build_packs.py}")
SUITE_BIN=${E4_SUITE_BIN:-}

image_of() {
    case "$1" in
        JP07|JP08|JP11) echo gen1 fw-5.3.0/SC5000 ;;
        JP13|JP14) echo gen1 fw-5.3.0/SC6000 ;;
        JC11|JC16|NH08) echo gen1 fw-5.3.0/PRIME4 ;;
        *) echo "unsupported board $1" >&2; return 1 ;;
    esac
}

if [ "${2:-}" != --inner ]; then
    [ "$(uname -m)" = aarch64 ] || { echo 'native ARM build host (aarch64) required' >&2; exit 1; }
    [ ! -e "$W" ] || { echo "refusing existing sandbox $W" >&2; exit 1; }
    board=${2:-JP14}
    read -r kind image < <(image_of "$board")
    mkdir -p "$W"
    dir=$W/$(basename "$image")
    python3 "$REPO/tools/az01-packer/az01_packer.py" unpack "$LAB/$image-5.3.0-Update.img" --output "$dir" > "$dir.log"
    python3 "$BUILDER" --qrc "$LAB/5.3.0/qrc" --out "$W/packs" > "$W/packs.log"
    if [ -z "$SUITE_BIN" ]; then
        SUITE_BIN="$W/enginex-suite"
        (cd "$REPO" && GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -o "$SUITE_BIN" ./cmd/enginex-suite)
    else
        SUITE_BIN=$(realpath -m "$SUITE_BIN")
        cp "$SUITE_BIN" "$W/enginex-suite"
        SUITE_BIN="$W/enginex-suite"
    fi
    chmod +x "$SUITE_BIN"
    fail=0
    unshare -m -n -p -f --kill-child bash "$0" "$W" --inner "$board" || fail=1
    [ "$fail" = 0 ] && echo "E4REAL RESULT|PASS|$board" || echo "E4REAL RESULT|FAIL|$board"
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
mkdir -p "$R/dev/shm" "$R/dev/pts" "$R/sys/firmware/devicetree/base" "$R/sys/devices/system/cpu" \
    "$R/run/dbus" "$R/tmp/home" "$R/data/enginex/ui/packs" "$R/data/enginex/state" "$R/data/enginex/Stream"
for f in online possible present; do echo 0-3 > "$R/sys/devices/system/cpu/$f"; done
for n in null zero full random urandom; do touch "$R/dev/$n"; mount --bind "/dev/$n" "$R/dev/$n"; done
printf '%s\0' "$board" > "$R/sys/firmware/devicetree/base/inmusic,product-code"
printf "inmusic,%s\0rockchip,rk3288\0" "$lower" > "$R/sys/firmware/devicetree/base/compatible"
mount -t proc -o ro proc "$R/proc"
printf '           CPU0\n 41:        100     GICv2  32 Level     ttyS0\n' > "$W/$board-interrupts"
mount --bind "$W/$board-interrupts" "$R/proc/interrupts"
mount -t tmpfs tmpfs "$R/proc/irq"
mkdir -p "$R/proc/irq/41"
printf 'f\n' > "$R/proc/irq/41/smp_affinity"
printf '0-3\n' > "$R/proc/irq/41/smp_affinity_list"
cp "$REPO/rootfs-overlay/etc/enginex/libengine-ui-hook.so" "$R/tmp/libengine-ui-hook.so"
cp "$W/enginex-suite" "$R/tmp/enginex-suite"
chmod +x "$R/tmp/enginex-suite"
cp -a "$W/packs/en" "$R/data/enginex/ui/packs/en"
mkdir -p "$R/data/enginex/ui/packs/en/Skins/Common/Items"
sed "s/@BOARD@/$board/" "$REPO/tools/az01-packer/settings-click-restart-probe.qml" > "$R/data/enginex/ui/packs/en/Skins/Common/Items/QmlWarnings.qml"
printf 'lang=en\n' > "$R/data/enginex/ui/settings.conf"

suite_pid_file="$W/$board-suite.pid"
watcher_pid=""
dbus_started=0
dbus_pid=""

stop_suite() {
    local spid=""
    [ -f "$suite_pid_file" ] && spid=$(cat "$suite_pid_file" 2>/dev/null || true)
    if [ -n "$spid" ]; then
        kill "$spid" 2>/dev/null || true
        for _ in $(seq 1 40); do
            kill -0 "$spid" 2>/dev/null || break
            sleep 0.05
        done
        kill -9 "$spid" 2>/dev/null || true
        wait "$spid" 2>/dev/null || true
    fi
    rm -f "$suite_pid_file"
}

start_suite() {
    local run_id=$1
    chroot "$R" /usr/bin/env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin HOME=/tmp/home \
        /tmp/enginex-suite stream-sync \
        --listen 127.0.0.1:8080 \
        --https-listen "" \
        --state-dir /data/enginex/state \
        --downloads /data/enginex/Stream \
        --deemixd "" \
        --disable-guard > "$W/$board-suite-$run_id.log" 2>&1 &
    local spid=$!
    echo "$spid" > "$suite_pid_file"
    for _ in $(seq 1 80); do
        if python3 -c "import urllib.request, json; r=urllib.request.urlopen('http://127.0.0.1:8080/v1/enginex/ui', timeout=1); d=json.loads(r.read()); assert d.get('rev',0)>0 and d.get('epoch')" 2>/dev/null; then
            return 0
        fi
        sleep 0.05
    done
    echo "E4REAL FAIL|$board|suite_${run_id}_not_ready|" >&2
    return 1
}

cleanup() {
    local rc=$?
    local dbus_err=0
    [ -n "$watcher_pid" ] && kill "$watcher_pid" 2>/dev/null || true
    stop_suite
    if [ "$dbus_started" = 1 ]; then
        if [ -z "$dbus_pid" ]; then
            echo "E4REAL FAIL|$board|dbus_cleanup_pid_lost|" >&2
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
                    echo "E4REAL FAIL|$board|dbus_cleanup_still_live=$dbus_pid|" >&2
                    dbus_err=1
                fi
            fi
        fi
    fi
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
    echo "E4REAL FAIL|$board|dbus_pid_unusable=$dbus_pid|" >&2
    exit 1
fi

start_suite 1
echo "E4REAL SUITE_START|1|$(python3 -c "import urllib.request, json; d=json.loads(urllib.request.urlopen('http://127.0.0.1:8080/v1/enginex/ui').read()); print('epoch='+d['epoch']+' rev='+str(d['rev']))")"

log="$W/$board-engine.log"
: > "$log"

# Background watcher orchestrates real enginex-suite restarts while real Engine stays open.
(
    stage=0
    for _ in $(seq 1 400); do
        if [ "$stage" = 0 ] && grep -aq "E4REAL TRIGGER_RESTART_1|$board|" "$log"; then
            stage=1
            echo "E4REAL HOST_RESTART|1|ui.rev=$(cat "$R/data/enginex/ui/ui.rev" 2>/dev/null || echo missing)"
            stop_suite
            start_suite 2
            echo "E4REAL SUITE_START|2|$(python3 -c "import urllib.request, json; d=json.loads(urllib.request.urlopen('http://127.0.0.1:8080/v1/enginex/ui').read()); print('epoch='+d['epoch']+' rev='+str(d['rev']))")"
        elif [ "$stage" = 1 ] && grep -aq "E4REAL TRIGGER_RESTART_2|$board|" "$log"; then
            stage=2
            echo "E4REAL HOST_RESTART|2|removing_ui_rev_was=$(cat "$R/data/enginex/ui/ui.rev" 2>/dev/null || echo missing)"
            stop_suite
            rm -f "$R/data/enginex/ui/ui.rev"
            start_suite 3
            echo "E4REAL SUITE_START|3|$(python3 -c "import urllib.request, json; d=json.loads(urllib.request.urlopen('http://127.0.0.1:8080/v1/enginex/ui').read()); print('epoch='+d['epoch']+' rev='+str(d['rev']))")"
            break
        fi
        sleep 0.1
    done
) &
watcher_pid=$!

timeout --kill-after=5s 60s chroot "$R" /usr/bin/env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin HOME=/tmp/home \
    LD_LIBRARY_PATH=/usr/qt/lib LD_PRELOAD=/tmp/libengine-ui-hook.so QT_QPA_PLATFORM=offscreen \
    QML_DISABLE_DISK_CACHE=1 MALLOC_ARENA_MAX=1 \
    /usr/Engine/Engine -d0 > "$log" 2>&1 || true

wait "$watcher_pid" 2>/dev/null || true
watcher_pid=""

grep -a -o -E "E4REAL (PASS|FAIL|TRIGGER_RESTART_[12]|SUMMARY|DONE|EXC)\|.*" "$log" | sed 's/ *\[20[0-9T:.Z-]*\].*//' || true

python3 - "$R/data/enginex/ui/settings.conf" "$R/data/enginex/ui/ui.rev" "$board" <<'PY'
import pathlib, sys
conf_text = pathlib.Path(sys.argv[1]).read_text(encoding="utf-8")
rev_text = pathlib.Path(sys.argv[2]).read_text(encoding="utf-8").strip()
board = sys.argv[3]
kv = {}
for line in conf_text.splitlines():
    if "=" in line:
        k, v = line.split("=", 1)
        kv[k.strip()] = v.strip()
sources = [s for s in kv.get("sources", "").split(",") if s]
rev = int(rev_text)
checks = [
    (kv.get("auto_gain") in (None, "0"), f"disk_auto_gain_off={kv.get('auto_gain')}"),
    (kv.get("master_limiter") == "1", f"disk_master_limiter_1={kv.get('master_limiter')}"),
    (kv.get("cue_countdown") == "0", f"disk_cue_countdown_0={kv.get('cue_countdown')}"),
    ("soundcloud" in sources, f"disk_sources_has_soundcloud={sources}"),
    (rev >= 3, f"disk_ui_rev_ge_3={rev}"),
]
bad = [name for ok, name in checks if not ok]
for ok, name in checks:
    print(f"E4REAL {'PASS' if ok else 'FAIL'}|{board}|{name}|")
if bad:
    sys.exit(1)
PY
grep -aq "E4REAL DONE|PASS|$board|" "$log"
