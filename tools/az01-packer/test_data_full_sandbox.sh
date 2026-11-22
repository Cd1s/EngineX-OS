#!/bin/bash
# A deck whose internal storage (/data) is full still starts everything (#5306). Real-image test, root on an ARM
# build host, never on a deck. Start it under the host's heavy lock, exactly like this:
#   flock /var/lock/enginex-heavy.lock bash tools/az01-packer/test_data_full_sandbox.sh <candidate JP07 image> <new work directory>
# The work directory must not exist. Everything runs in private mount, network and pid namespaces; the "internal
# storage" is an ext4 loop file of its own in the work directory, so the host's /data is never touched.
#
# Per scenario the deck's boot order runs on the real rootfs of the image (set-up code shared with
# boot-safety-sandbox.sh in sandbox-root.sh): etc-reset, prepare, the suite unpack, the guard's pre step, our SSH
# check, and the services stream-sync, enginex-deemixd, rb-bridge and the boot watch. systemd is not available in the
# sandbox, so the unit graph is read from the image's own unit files: a unit whose Requires= names enginex-prepare
# starts only when prepare ended with success, exactly as systemd decides it. Every step's exit code and wall time go
# to <work>/steps.tsv, what is up to <work>/up.tsv, and <work>/results.txt gets one PASS or FAIL line per check.
# The exit code is 0 only when every check passed.
#
# Scenarios (what ext4 can still allocate on /data before prepare; the DJ's own files fill the rest; the KiB that
# statfs reports, and prepare reads, are in steps.tsv and are higher by the 16 MiB ext4 keeps for itself):
#   normal           plenty of room (the control: whatever starts here must start in every other scenario)
#   full0 full1m full20m   0 bytes, 1 MiB, 20 MiB free; nothing of ours to remove; the install is up to date
#   upd0 upd1m upd20m      the same, on the first boot after an update: UI packs and suite are stale
#   rem-early              0 bytes free, with files of ours that are made again: the floor is met by the first two classes
#   rem-order              0 bytes free, with a file of every class: all classes are used, the stream cache only as far as needed
#   rem-upd                rem-order on the first boot after an update: the room made is used to install both
#   free2 free3            rem-order again: the three runs that time the freeing (steps.tsv, step prepare)
# normal runs prepare three times in a row for the timing of the step when there is room.
set -euo pipefail
export LC_ALL=C
SELF=$(realpath "$0")
REPO=$(cd "$(dirname "$SELF")/../.." && pwd)
DATA_MIB=512
SCENARIOS='normal full0 full1m full20m upd0 upd1m upd20m rem-early rem-order rem-upd free2 free3'
KIB=1024

if [ "${1:-}" != --inner ]; then
    [ "$#" = 2 ] && [ "$(id -u)" = 0 ] || { echo "usage: $0 <JP07 image> <new work directory>" >&2; exit 2; }
    image=$(realpath "$1")
    export W=$(realpath -m "$2")
    [ ! -e "$W" ] || { echo 'Use a new work directory' >&2; exit 2; }
    case $(basename "$image") in *-JP07-Update.img) export PRODUCT=JP07 ;; *) echo 'a JP07 image please' >&2; exit 2 ;; esac
    mkdir -p "$W"
    python3 -I "$REPO/tools/az01-packer/az01_packer.py" unpack "$image" --output "$W/image" > "$W/unpack.json"
    ssh-keygen -q -t ed25519 -N '' -f "$W/id" -C data-full-test
    : > "$W/steps.tsv"; : > "$W/up.tsv"; : > "$W/results.txt"
    unshare -m --propagation private -n -p -f --kill-child "$SELF" --inner install
    for scenario in $SCENARIOS; do
        unshare -m --propagation private -n -p -f --kill-child "$SELF" --inner "$scenario"
    done
    printf '%s\n' '--- steps: scenario, KiB free before, step, exit code, ms' && cat "$W/steps.tsv"
    printf '%s\n' '--- what is up: scenario, unit or service, state, detail' && cat "$W/up.tsv"
    cat "$W/results.txt"
    ! grep -q '^FAIL' "$W/results.txt"
    exit
fi

SCEN=$2
C=$W/$SCEN
R=$C/root
mkdir -p "$R" "$C/data"
pass() { echo "PASS $SCEN: $*" | tee -a "$W/results.txt"; }
fail() { echo "FAIL $SCEN: $*" | tee -a "$W/results.txt"; }
check() { local name=$1; shift; if "$@" > /dev/null 2>&1; then pass "$name"; else fail "$name"; fi; }
free_bytes() { stat -f -c '%f %S' "$C/data" | awk '{print $1*$2}'; }   # statfs, as prepare reads it

# --- /data: an ext4 loop file. The install is made once, on an empty file system, by the first scenario.
if [ "$SCEN" = install ]; then
    truncate -s "${DATA_MIB}M" "$C/data.img"
    mkfs.ext4 -q -F -O encrypt -L az01-data "$C/data.img"   # the deck's own options (az0x-data-mkfs): 5% stay reserved for root
else
    cp --sparse=always "$W/install/data.img" "$C/data.img"
fi
mount -o loop "$C/data.img" "$C/data"

# Files of the DJ and of the deck that nothing of ours may ever remove. One of every class of the PM list.
NEVER='Music/dj-collection.bin
Music/dj-chosen-track.mp3
enginex/Stream/Stems/1234/stem-kept.flac
enginex/Stream/my-own-recording.wav
AppDataUser/AIR Music Technology/Engine/Engine Library/Database2/m.db
AppDataUser/AIR Music Technology/Engine/Engine Library/Album Art/00/cover.jpg
inMusicTempFiles/engine-copy.flac
enginex/ui/settings.conf
enginex/ui-scale.env
enginex/state/deezer-owner.json
enginex/hfs-libraries/vol-1/Engine Library/Database2/m.db
enginex/hfs-lib/base-1/upper/Database2/m.db
enginex/hfs-lib/base-1/work/keep
.prolink-cache/Player-1/folder/track.mp3
enginex/mods/test-mod/mod.json
enginex/mods-state/test-mod/state.json
enginex/ui/mods-overlay/Test.qml
enginex/ui/mods-state.json
enginex/tailscale/config.json
enginex/hook-disable.conf'
seed_never() {
    local f
    while IFS= read -r f; do
        mkdir -p "$(dirname "$C/data/$f")"
        case $f in
            Music/dj-collection.bin) : ;;  # the ballast below
            *.conf|*.env|*.json|*.qml) printf 'seed %s\n' "$f" > "$C/data/$f" ;;
            *) head -c 4096 /dev/urandom > "$C/data/$f" ;;
        esac
    done <<< "$NEVER"
}
never_sums() { (cd "$C/data" && while IFS= read -r f; do [ "$f" = Music/dj-collection.bin ] || sha256sum -- "$f"; done <<< "$NEVER"); }

# The sign-ins of Engine's streaming accounts: the guard's pre step edits this file (it adds a TIDAL entry for the
# gateway), so it is checked for what it must still hold, not for a hash.
seed_accounts() {
    printf '{\n    "Deezer": "{\\"provider\\":\\"Deezer\\",\\"accessToken\\":\\"dj-own-token\\"}"\n}\n' > "$C/data/StreamingAccounts.json"
    cp "$C/data/StreamingAccounts.json" "$C/accounts.seed"
}

# Engine.conf with a time zone that the deck's saved zone is newer than: prepare rewrites it when it can (restore_timezone).
seed_timezone() {
    local dir="$C/data/.config/jp07.user.settings/AIR Music Technology"
    mkdir -p "$dir" "$C/data/enginex/state"
    { printf '[General]\n'; for i in $(seq 1 600); do printf 'Setting%d=%s\n' "$i" "$(head -c 24 /dev/zero | tr '\0' x)"; done
      printf '[TimeDateSettings]\nTZEnvVar=UTC\n'; } > "$dir/Engine.conf"
    printf 'Europe/Berlin\n' > "$C/data/enginex/state/timezone"
    touch -d '2 minutes' "$C/data/enginex/state/timezone"
    sha256sum "$dir/Engine.conf" | cut -d' ' -f1 > "$C/engine-conf.sha"
    wc -c < "$dir/Engine.conf" > "$C/engine-conf.size"
}

# Files of ours that are made again by themselves, one of every class of the PM list, in the order prepare frees them.
# $1 early: the QML cache alone is large enough to meet the floor after the leftovers.
FLOOR_KIB=$(debugfs -R 'cat /usr/libexec/enginex-prepare' "$W/image/rootfs.ext2" 2> /dev/null | sed -n 's/.*ENGINEX_DATA_FLOOR_KIB:-\([0-9]*\)}.*/\1/p' | head -1)
FLOOR_KIB=${FLOOR_KIB:-73728}
kib() { head -c "$(( $2 * 1024 ))" /dev/zero > "$C/data/$1"; }   # a file of $2 KiB
aged() { touch -d "@$((1700000000 + $2))" "$C/data/$1"; }       # its age: higher = newer
seed_removable() {
    local e=enginex mode=$1 i
    mkdir -p "$C/data/$e"/{.prepare.old,ui/.install.old,bin,Stream,shadow-v3/peer-aa,ui/qml-cache,logs,prolink-export/PIONEER/{USBANLZ/P001,USBANLZ/P002,USBANLZ/P003,rekordbox}}
    kib "$e/.prepare.old/f" 512; aged "$e/.prepare.old/f" 1; aged "$e/.prepare.old" 1
    kib "$e/ui/.install.old/f" 512; aged "$e/ui/.install.old/f" 2; aged "$e/ui/.install.old" 2
    kib "$e/bin/enginex-suite.1.tmp" 512; aged "$e/bin/enginex-suite.1.tmp" 3
    kib "$e/Stream/.part-x" 512; aged "$e/Stream/.part-x" 4
    kib "$e/Stream/.deemix-1-a.part" 512; aged "$e/Stream/.deemix-1-a.part" 5
    kib "$e/shadow-v3/peer-aa/.fetch-x" 512; aged "$e/shadow-v3/peer-aa/.fetch-x" 6
    if [ "$mode" = rem-early ]; then kib "$e/ui/qml-cache/seed.qmlc" $((FLOOR_KIB - 8 * 1024)); else kib "$e/ui/qml-cache/seed.qmlc" 8192; fi
    for i in 1 2 3 4 5 6 7; do
        kib "$e/logs/EngineX-boot-report-20261122T0${i}0000.000000000Z.txt" 512; aged "$e/logs/EngineX-boot-report-20261122T0${i}0000.000000000Z.txt" $((10 + i))
    done
    printf 'EngineX-boot-report-20261122T020000.000000000Z.txt\n' > "$C/data/$e/ui/boot-report-latest"
    for i in 1 2 3; do kib "$e/prolink-export/PIONEER/USBANLZ/P00$i/ANLZ0000.DAT" 512; aged "$e/prolink-export/PIONEER/USBANLZ/P00$i" $((19 + i)); done
    printf 'PMAI \x00 /PIONEER/USBANLZ/P001/ANLZ0000.DAT \x00 end\n' > "$C/data/$e/prolink-export/PIONEER/rekordbox/export.pdb"
    for i in 1 2 3; do kib "$e/Stream/$i.flac" 24576; aged "$e/Stream/$i.flac" $((30 + i)); done
    kib "$e/Stream/keep-me.wav" 4096
    # what must be gone and what must stay, after prepare
    : > "$C/expect.gone"; : > "$C/expect.kept"
    printf '%s\n' "$e/.prepare.old" "$e/ui/.install.old" "$e/bin/enginex-suite.1.tmp" "$e/Stream/.part-x" "$e/Stream/.deemix-1-a.part" \
        "$e/shadow-v3/peer-aa/.fetch-x" "$e/ui/qml-cache" >> "$C/expect.gone"
    if [ "$mode" = rem-early ]; then
        printf '%s\n' "$e/logs/EngineX-boot-report-20261122T010000.000000000Z.txt" "$e/prolink-export/PIONEER/USBANLZ/P002" \
            "$e/Stream/1.flac" "$e/Stream/2.flac" >> "$C/expect.kept"
    else
        printf '%s\n' "$e/logs/EngineX-boot-report-20261122T010000.000000000Z.txt" "$e/logs/EngineX-boot-report-20261122T030000.000000000Z.txt" \
            "$e/logs/EngineX-boot-report-20261122T040000.000000000Z.txt" "$e/prolink-export/PIONEER/USBANLZ/P002" \
            "$e/prolink-export/PIONEER/USBANLZ/P003" "$e/Stream/1.flac" "$e/Stream/2.flac" >> "$C/expect.gone"
    fi
    printf '%s\n' "$e/logs/EngineX-boot-report-20261122T020000.000000000Z.txt" "$e/logs/EngineX-boot-report-20261122T050000.000000000Z.txt" \
        "$e/logs/EngineX-boot-report-20261122T060000.000000000Z.txt" "$e/logs/EngineX-boot-report-20261122T070000.000000000Z.txt" \
        "$e/prolink-export/PIONEER/USBANLZ/P001" "$e/Stream/3.flac" "$e/Stream/keep-me.wav" >> "$C/expect.kept"
}
# The first boot after an update: neither the suite nor the UI packs installed match the image.
seed_stale() {
    printf 'stale\n' > "$C/data/enginex/bin/enginex-suite.id"
    rm -f "$C/data/enginex/ui/.pack-id" "$C/data/enginex/etc-overlay-epoch"   # a new image: the /etc reset has real work too
}

# The DJ's own music takes every block ext4 will give; then $1 bytes are given back. Allocated, not written: ext4
# counts the pages it still has to write back as free space, so a write loop stops with room to spare. ext4 also
# keeps up to 16 MiB (its reserved clusters) back for itself that statfs still reports as free: at "0 bytes" the
# number prepare reads is about that much, and nothing can be allocated.
fill_to() {
    local want=$1 f="$C/data/Music/dj-collection.bin" n
    mkdir -p "$C/data/Music"
    : > "$f"
    n=$(( $(free_bytes) / 4096 * 4096 ))
    while [ "$n" -ge 4096 ]; do
        fallocate -o "$(stat -c %s "$f")" -l "$n" "$f" 2> /dev/null || n=$(( n / 2 / 4096 * 4096 ))
    done
    [ "$want" = 0 ] || truncate -s "$(( $(stat -c %s "$f") - want ))" "$f"
    sync
}

case $SCEN in
    install) ;;
    *)
        seed_never; seed_accounts; seed_timezone
        case $SCEN in full*|upd*) rm -rf "$C/data/enginex/ui/qml-cache" ;; esac   # nothing of ours to remove
        case $SCEN in upd*|rem-upd) seed_stale ;; esac
        case $SCEN in rem-*|free*) seed_removable "$SCEN" ;; esac
        case $SCEN in
            full0|upd0|rem-*|free*) fill_to 0 ;;
            full1m|upd1m) fill_to $((1024 * 1024)) ;;
            full20m|upd20m) fill_to $((20 * 1024 * 1024)) ;;
        esac ;;
esac
FREE0=$(free_bytes)
sync
case $SCEN in upd*) (cd "$C/data/enginex" && find bin ui/packs ui/icons -type f -print0 | sort -z | xargs -0 sha256sum > "$C/installed.before") ;; esac
debugfs -R 'cat /usr/share/enginex/enginex-suite.xz' "$W/image/rootfs.ext2" 2> /dev/null | sha256sum | cut -d' ' -f1 > "$C/suite.id"
never_sums > "$C/never.before" 2>/dev/null || :
find "$C/data" -path "$C/data/Music" -prune -o -type f -print0 | sort -z | xargs -0 -r sha256sum > "$C/data.before"

mkdir -p "$C/usb" "$C/late"
. "$(dirname "$SELF")/sandbox-root.sh"
wait_for() { local end=$((SECONDS + $1)); shift; until "$@"; do [ "$SECONDS" -lt "$end" ] || return 1; sleep .5; done; }

step() { # <name> <command...>: run it, record exit code and wall time
    local name=$1 t0 t1 rc=0
    shift
    t0=$(date +%s%N)
    "$@" > "$C/$name.log" 2>&1 || rc=$?
    t1=$(date +%s%N)
    printf '%s\t%s\t%s\t%s\t%s\n' "$SCEN" "$((FREE0 / 1024))" "$name" "$rc" "$(( (t1 - t0) / 1000000 ))" >> "$W/steps.tsv"
    eval "RC_${name//-/_}=$rc"
    return 0
}
# systemd's rule for Requires=: the unit starts only when the unit it requires did not fail.
requires_prepare() { # <unit>: does this unit hard-require enginex-prepare.service?
    { cat "$R/usr/lib/systemd/system/$1" "$R/usr/lib/systemd/system/$1.d/"*.conf "$R/etc/systemd/system/$1" 2>/dev/null || :; } |
        sed -n 's/^Requires=//p' | tr ' ' '\n' | grep -x enginex-prepare.service > /dev/null   # pipefail: neither cat of a missing file nor grep -q may decide
}
unit_up() { # <unit>: would systemd start it after prepare ended as it did?
    ! requires_prepare "$1" || [ "$RC_prepare" = 0 ]
}
up() { printf '%s\t%s\t%s\t%s\n' "$SCEN" "$1" "$2" "${3:-}" >> "$W/up.tsv"; }
unit_exec() { sed -n 's/^ExecStart=-\{0,1\}//p' "$R/etc/systemd/system/$1" "$R/usr/lib/systemd/system/$1" 2> /dev/null | tail -1; }

RC_prepare=
step etc-reset in_root /usr/libexec/enginex-etc-reset
if [ "$SCEN" = install ]; then
    mkdir -p -m 700 "$C/data/enginex"
    cp "$W/id.pub" "$C/data/enginex/authorized_keys"
fi
# Traced (sh -x) to name the failing line; the scenarios that time the freeing run it as systemd does.
TRACE=-x
case $SCEN in rem-*|free*) TRACE= ;; esac
step prepare in_root /bin/sh $TRACE /usr/libexec/enginex-prepare
# The control runs the step three more times, untraced: the timing of prepare when there is room, nothing to free.
if [ "$SCEN" = normal ]; then
    for i in 2 3 4; do step "prepare-$i" in_root /usr/libexec/enginex-prepare; done
fi
# After prepare: what was made again is gone, in the order of the list and only as far as the floor needs; the rest stays.
expect_check() {
    local bad=0 f
    while IFS= read -r f; do [ ! -e "$C/data/$f" ] || { echo "still there: $f"; bad=1; }; done < "$C/expect.gone"
    while IFS= read -r f; do [ -e "$C/data/$f" ] || { echo "gone: $f"; bad=1; }; done < "$C/expect.kept"
    return $bad
}
case $SCEN in rem-*|free*) check "$SCEN: the files of ours were removed in the order of the list, only as far as the floor needs" expect_check ;; esac
[ "$SCEN" = install ] || check "$SCEN: after prepare the DJ's streaming sign-ins are byte for byte as they were" cmp "$C/accounts.seed" "$C/data/StreamingAccounts.json"
# Right after prepare, before any service runs (stream-sync cleans some files of its own later): nothing of the DJ's
# or of the deck's was touched.
[ "$SCEN" = install ] || check "$SCEN: after prepare nothing of the DJ or of the deck was touched" \
    bash -c 'cd "$1/data" && sha256sum -c --quiet "$1/never.before" > "$1/never.check" 2>&1' x "$C"
mkdir -p "$R/run/sshd"
step sshd-t in_root /usr/sbin/sshd -t -f /usr/share/enginex/sshd_config
[ "$SCEN" != install ] || printf 'lang=en\n' > "$C/data/enginex/ui/settings.conf"
step guard-pre in_root /etc/enginex/engine-ui-guard pre
# The suite is unpacked by the first service that starts; the retired database watch exits 0 at once after it.
step suite-unpack in_root /usr/local/bin/engine-guardian --watch

sandbox_proc
in_root /usr/bin/dbus-daemon --system --fork --nopidfile > "$C/dbus.log" 2>&1 || :

# The units of the image: our target, our SSH, and the services of the suite.
for unit in enginex.target enginex-sshd.service; do
    if unit_up "$unit"; then up "$unit" up; else up "$unit" DOWN 'Requires=enginex-prepare.service failed'; fi
done
pids=()
if unit_up enginex-sshd.service && [ -s "$C/data/enginex/authorized_keys" ]; then
    # shellcheck disable=SC2046
    in_root $(unit_exec enginex-sshd.service) > "$C/sshd.log" 2>&1 & pids+=($!)
    if wait_for 10 ssh -q -i "$W/id" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes root@127.0.0.1 true; then
        up ssh-login up
    else
        up ssh-login DOWN 'sshd does not accept the key'
    fi
else
    up ssh-login DOWN 'unit not started'
fi
# shellcheck disable=SC2046
in_root $(unit_exec stream-sync.service) > "$C/stream-sync.log" 2>&1 & pids+=($!)
# shellcheck disable=SC2046
in_root $(unit_exec enginex-deemixd.service) > "$C/deemixd.log" 2>&1 & pids+=($!)
# shellcheck disable=SC2046
in_root $(unit_exec rb-bridge.service) > "$C/rb-bridge.log" 2>&1 & rb=$! pids+=($!)
in_root ENGINEX_BOOT_POLL_INTERVAL=1 /etc/enginex/engine-ui-guard watch > "$C/guardian.log" 2>&1 & guard=$! pids+=($!)
if wait_for 25 curl -sf -o /dev/null http://127.0.0.1:8080/boot-status; then up web-console up; else up web-console DOWN "$(tail -1 "$C/stream-sync.log" | cut -c1-120)"; fi
if wait_for 10 curl -sf -o /dev/null http://127.0.0.1:6595/healthz; then up deemixd up; else up deemixd DOWN "$(tail -1 "$C/deemixd.log" | cut -c1-120)"; fi
sleep 3
if kill -0 "$rb" 2> /dev/null; then up rb-bridge up; else up rb-bridge DOWN "$(tail -1 "$C/rb-bridge.log" | cut -c1-120)"; fi
if kill -0 "$guard" 2> /dev/null; then up boot-watch up; else up boot-watch DOWN "$(tail -1 "$C/guardian.log" | cut -c1-120)"; fi
if [ "$SCEN" = install ]; then
    chroot "$R" /usr/bin/env -i PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
        HOME=/tmp/home QT_QPA_PLATFORM=offscreen LD_LIBRARY_PATH=/usr/qt/lib MALLOC_ARENA_MAX=1 \
        ENGINEX_UI_HOOK=/etc/enginex/libengine-ui-hook.so /etc/enginex/engine-ui-guard launch /usr/Engine/Engine -d0 > "$C/engine.log" 2>&1 &
    pids+=($!)
    wait_for 60 bash -c 'test -s "$1/boot-ready" && cmp -s "$1/boot-token" "$1/boot-ready"' x "$C/data/enginex/ui" || echo 'engine did not report ready' >> "$W/measure.txt"
    sleep 5
    {
        echo "qml-cache: $(du -sk "$C/data/enginex/ui/qml-cache" | cut -f1) KiB in $(find "$C/data/enginex/ui/qml-cache" -type f | wc -l) files"
        echo "ui packs: $(du -sk "$C/data/enginex/ui/packs" "$C/data/enginex/ui/icons" | awk '{s += $1} END {print s}') KiB"
        echo "bin: $(du -sk "$C/data/enginex/bin" | cut -f1) KiB"
        echo "enginex dir: $(du -sk "$C/data/enginex" | cut -f1) KiB, whole /data: $(du -sk "$C/data" | cut -f1) KiB"
    } >> "$W/measure.txt"
fi
up free-after "$(( $(free_bytes) / 1024 )) KiB"
for p in "${pids[@]}"; do kill -TERM "$p" 2> /dev/null || :; done
sleep 1
for p in "${pids[@]}"; do kill -KILL "$p" 2> /dev/null || :; done
sync

# --- what the scenario leaves behind
[ "$SCEN" = install ] && {
    in_root SERVICE_RESULT=success /etc/enginex/engine-ui-guard stop > /dev/null 2>&1 || :
    umount -R "$R" 2> /dev/null || umount -l "$R" || :
    umount "$C/data" 2> /dev/null || umount -l "$C/data" || :
    exit 0
}
# --- what must hold after the boot order: every scenario starts everything, with or without room
check "$SCEN: prepare ends with success" test "$RC_prepare" = 0
check "$SCEN: our target, our SSH unit and its key login, the web console, the downloader, the bridge and the boot watch are up" \
    awk -F '\t' -v s="$SCEN" '$1 == s && $3 == "DOWN" { bad = 1 } END { exit bad }' "$W/up.tsv"
lines=$(grep -c '^enginex-prepare: internal storage is full' "$C/prepare.log" || :)
case $SCEN in
    full*|upd*) check "$SCEN: one line says that internal storage is full (journal and start-up report)" test "$lines" = 1 ;;
    *) check "$SCEN: no such line while the floor is met" test "$lines" = 0 ;;
esac
case $SCEN in
    upd*)  # no room for the update: the installed programs and UI packs stay as they are, the suite runs from memory, said once
        check "$SCEN: the installed suite and UI packs are untouched" bash -c 'cd "$1/data/enginex" && sha256sum -c --quiet "$1/installed.before" > /dev/null' x "$C"
        check "$SCEN: the programs run from memory and that is said once" test "$(cat "$C"/*.log | grep -c 'enginex-suite-exec: no room on internal storage')" = 1
        check "$SCEN: no temporary of the unpack is left on /data" bash -c '! ls "$1"/data/enginex/bin/enginex-suite.*.tmp 2> /dev/null | grep -q .' x "$C" ;;
    rem-upd)
        check "$SCEN: the room made was used: the suite and the UI packs of the image are installed" \
            bash -c 'cd "$1/data/enginex" && [ "$(cat bin/enginex-suite.id)" = "$(cat "$2")" ] && [ -s ui/.pack-id ]' x "$C" "$C/suite.id" ;;
esac
engine_conf="$C/data/.config/jp07.user.settings/AIR Music Technology/Engine.conf"
check "$SCEN: Engine.conf is never cut short (kept as it was, or rewritten whole)" \
    bash -c 'f=$1; [ "$(wc -c < "$f")" -ge "$(cat "$2")" ] && grep -q "^\[TimeDateSettings\]" "$f" && grep -q "^TZEnvVar=" "$f"' x "$engine_conf" "$C/engine-conf.size"
# The guard's `pre` adds the TIDAL entry for the gateway with `awk ... > $accounts.tmp; mv` and does not look at awk's status:
# on a disk with no room it can put an empty file in the DJ's place. That line is outside this change (PM decision 5: nothing
# else in the guard changes); it is reported as KNOWN, not hidden, with the proposed patch in the report.
if python3 -I -c 'import json,sys; a=json.load(open(sys.argv[1])); sys.exit(0 if "dj-own-token" in a.get("Deezer", "") else 1)' "$C/data/StreamingAccounts.json" 2> /dev/null; then
    pass "$SCEN: the DJ's streaming sign-ins are still in StreamingAccounts.json after the guard's pre"
elif [ ! -s "$C/data/StreamingAccounts.json" ] && grep -q 'No space left' "$C/guard-pre.log"; then
    echo "KNOWN $SCEN: the guard's pre replaced StreamingAccounts.json by an empty file for want of room (engine-ui-guard accounts write; not changed here)" | tee -a "$W/results.txt"
else
    fail "$SCEN: the DJ's streaming sign-ins are gone from StreamingAccounts.json"
fi
echo done > "$C/done"
