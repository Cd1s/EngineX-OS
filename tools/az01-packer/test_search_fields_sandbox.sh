#!/bin/bash
# #336 S23 Rule 0.1 end-to-end real-Engine + real-ARMv7-enginex-suite sandbox proof:
# 1. Stock 5.3.0 control (Boot 1 -> Boot 2 on persistent /data): verifies unmodified stock
#    Engine drops custom search fields back to song,artist on restart while keeping SearchAll.
# 2. S23+E4 pack + real ARMv7 enginex-suite stream-sync (Boot 1 -> Boot 2 -> Boot 3):
#    - Boot 1: opens real SearchMenuDialog, turns SearchAll off, picks BPM/Key/Comment,
#      turns SearchAll on, persists search_fields=song,artist,key,comment,bpm in settings.conf.
#    - Boot 2: restarts both Engine and enginex-suite, confirms all 5 fields restored in
#      Planck.librarianController and SearchMenuDialog (SearchAll preserved independently),
#      triggers stock /Client/Preferences/ResetAll, confirms song,artist restored and
#      search_fields= omitted from settings.conf.
#    - Boot 3: restarts both Engine and enginex-suite, confirms clean default song,artist.
#
# Run under enginex-run sandbox on an ARM build host:
#   enginex-run sandbox -- bash tools/az01-packer/test_search_fields_sandbox.sh <new-private-dir> [BOARD...]
set -euo pipefail
REPO=$(cd "$(dirname "$0")/../.." && pwd)
. "${ENGINEX_MIGRATION:-$REPO/rootfs-overlay/usr/libexec/enginex-migration}"
ENGINEX_LAB=${ENGINEX_LAB:-$legacy_lab}
W=$(realpath -m "${1:?new private sandbox directory required}")
LAB=${ENGINEX_LAB}
BUILDER=$(realpath -m "${S23_BUILDER:-$REPO/tools/ui-mod/build_packs.py}")
SUITE_BIN=${S23_SUITE_BIN:-}

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
    shift
    boards=("$@")
    [ "${#boards[@]}" -gt 0 ] || boards=(JP11)
    mkdir -p "$W"
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
    for board in "${boards[@]}"; do
        read -r kind image < <(image_of "$board")
        dir=$W/$(basename "$image")
        if [ ! -f "$dir/rootfs.ext2" ]; then
            python3 "$REPO/tools/az01-packer/az01_packer.py" unpack "$LAB/$image-5.3.0-Update.img" --output "$dir" > "$dir.log"
        fi
        unshare -m -n -p -f --kill-child bash "$0" "$W" --inner "$board" || fail=1
    done
    [ "$fail" = 0 ] && echo "S23REAL RESULT|PASS|${boards[*]}" || echo "S23REAL RESULT|FAIL|${boards[*]}"
    exit $fail
fi

board=${3:?board required}
read -r kind image < <(image_of "$board")
R=$W/$board-root
lower=$(echo "$board" | tr A-Z a-z)
mount --make-rprivate /
ip link set lo up
mkdir -p "$R" "$W/$board-data-stock" "$W/$board-data-s23"
mount -o loop "$W/$(basename "$image")/rootfs.ext2" "$R"
for d in tmp run dev sys; do mount -t tmpfs tmpfs "$R/$d"; done
chroot "$R" /bin/mkdir -p /var/volatile/tmp /var/volatile/log /var/volatile/cache
mkdir -p "$R/dev/shm" "$R/dev/pts" "$R/sys/firmware/devicetree/base" "$R/sys/devices/system/cpu" \
    "$R/run/dbus" "$R/tmp/home"
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

suite_pid_file="$W/$board-suite.pid"
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
        if python3 -c "import urllib.request, json; r=urllib.request.urlopen('http://127.0.0.1:8080/v1/enginex/ui', timeout=1); d=json.loads(r.read()); assert d.get('rev',0)>0 and d.get('epoch') and isinstance(d.get('searchFields'), list)" 2>/dev/null; then
            return 0
        fi
        sleep 0.05
    done
    echo "S23REAL FAIL|$board|suite_${run_id}_not_ready|" >&2
    return 1
}

cleanup() {
    local rc=$?
    stop_suite
    if [ -n "$dbus_pid" ]; then
        kill "$dbus_pid" 2>/dev/null || true
    fi
    return "$rc"
}
trap cleanup EXIT

dbus_pid=$(chroot "$R" /usr/bin/dbus-daemon --system --fork --nopidfile --print-pid=1 3>&- 4>&- 2>"$W/$board-dbus.log" || true)
dbus_pid=${dbus_pid%%[!0-9]*}
if [ -z "$dbus_pid" ] || ! kill -0 "$dbus_pid" 2>/dev/null; then
    echo "S23REAL FAIL|$board|dbus_pid_unusable=$dbus_pid|" >&2
    exit 1
fi

run_engine_phase() {
    local mode=$1
    local log="$W/$board-$mode.log"
    sed -e "s/@BOARD@/$board/g" -e "s/@MODE@/$mode/g" \
        "$REPO/tools/az01-packer/search-fields-probe.qml" \
        > "$R/data/enginex/ui/packs/en/Skins/Common/Items/QmlWarnings.qml"
    : > "$log"
    timeout --kill-after=5s 25s chroot "$R" /usr/bin/env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin HOME=/tmp/home \
        LD_LIBRARY_PATH=/usr/qt/lib LD_PRELOAD=/tmp/libengine-ui-hook.so QT_QPA_PLATFORM=offscreen \
        QML_DISABLE_DISK_CACHE=1 MALLOC_ARENA_MAX=1 \
        /usr/Engine/Engine -d0 > "$log" 2>&1 || true
    grep -a -o -E "S23REAL (PASS|FAIL|INFO|SUMMARY|DONE|EXC)\|.*" "$log" | sed 's/ *\[20[0-9T:.Z-]*\].*//' || true
    grep -aq "S23REAL DONE|PASS|$board|$mode|" "$log"
}

# --- Arm A: Stock 5.3.0 control (unmodified EmbeddedMain + Search + SearchMenuDialog, no enginex-suite) ---
mount --bind "$W/$board-data-stock" "$R/data"
mkdir -p "$R/data/enginex/ui/packs/en/Skins/Common/Items"
printf 'lang=en\n' > "$R/data/enginex/ui/settings.conf"
run_engine_phase stock_boot1
run_engine_phase stock_boot2
umount "$R/data"

# --- Arm B: S23+E4 pack + real compiled ARMv7 enginex-suite across Engine & suite restarts ---
mount --bind "$W/$board-data-s23" "$R/data"
mkdir -p "$R/data/enginex/ui/packs" "$R/data/enginex/state" "$R/data/enginex/Stream"
cp -a "$W/packs/en" "$R/data/enginex/ui/packs/en"
mkdir -p "$R/data/enginex/ui/packs/en/Skins/Common/Items"
printf 'lang=en\n' > "$R/data/enginex/ui/settings.conf"

# Boot 1: pick BPM, Key, Comment in real SearchMenuDialog -> persisted to settings.conf
start_suite 1
echo "S23REAL SUITE|$board|boot1|$(python3 -c "import urllib.request, json; d=json.loads(urllib.request.urlopen('http://127.0.0.1:8080/v1/enginex/ui').read()); print('epoch='+d['epoch']+' rev='+str(d['rev'])+' fields='+','.join(d['searchFields']))")"
run_engine_phase s23_boot1
stop_suite
if grep -qx 'search_fields=song,artist,key,comment,bpm' "$R/data/enginex/ui/settings.conf"; then
    echo "S23REAL PASS|$board|s23_boot1|disk_search_fields=song,artist,key,comment,bpm|"
else
    echo "S23REAL FAIL|$board|s23_boot1|disk_missing_search_fields=$(grep search_fields "$R/data/enginex/ui/settings.conf" || echo absent)|"
    exit 1
fi

# Boot 2: restart both enginex-suite & Engine -> verify 5 fields restored -> ResetAll -> verify omitted on disk
start_suite 2
echo "S23REAL SUITE|$board|boot2|$(python3 -c "import urllib.request, json; d=json.loads(urllib.request.urlopen('http://127.0.0.1:8080/v1/enginex/ui').read()); print('epoch='+d['epoch']+' rev='+str(d['rev'])+' fields='+','.join(d['searchFields']))")"
run_engine_phase s23_boot2
stop_suite
if grep -q '^search_fields=' "$R/data/enginex/ui/settings.conf"; then
    echo "S23REAL FAIL|$board|s23_boot2|disk_reset_still_has=$(grep '^search_fields=' "$R/data/enginex/ui/settings.conf")|"
    exit 1
else
    echo "S23REAL PASS|$board|s23_boot2|disk_reset_omits_search_fields|"
fi

# Boot 3: restart both enginex-suite & Engine -> verify clean default song,artist
start_suite 3
echo "S23REAL SUITE|$board|boot3|$(python3 -c "import urllib.request, json; d=json.loads(urllib.request.urlopen('http://127.0.0.1:8080/v1/enginex/ui').read()); print('epoch='+d['epoch']+' rev='+str(d['rev'])+' fields='+','.join(d['searchFields']))")"
run_engine_phase s23_boot3
stop_suite
umount "$R/data"
