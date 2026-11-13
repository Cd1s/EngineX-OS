#!/bin/bash
# No root, real block device, mount, sysctl, or host service changes required.
set -euo pipefail
ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
TMP=$(mktemp -d "$ROOT/tests/.tmp.XXXXXX")
trap 'rm -rf -- "$TMP"' EXIT
STORAGE=$ROOT/rootfs-overlay/usr/local/bin/sc5000-storage-mount
PERF=$ROOT/rootfs-overlay/usr/local/bin/sc5000-perf-tune
mkdir -p "$TMP/bin"
cat > "$TMP/bin/mock" <<'MOCK'
#!/bin/bash
set -eu
tool=${0##*/}
printf '%s' "$tool" >> "$MOCK_ROOT/log"
printf ' <%s>' "$@" >> "$MOCK_ROOT/log"
printf '\n' >> "$MOCK_ROOT/log"
case $tool in
    blkid)
        dev=${@: -1}
        if [[ $* == *' UUID '* ]]; then
            cat "$MOCK_ROOT/uuid/${dev##*/}"
        else
            cat "$MOCK_ROOT/types/${dev##*/}" 2>/dev/null || exit 2
        fi ;;
    findmnt)
        if [[ $* == *' -S '* ]]; then
            dev=$3 col=3
            [[ $* != *OPTIONS* ]] || col=4
            awk -F '|' -v dev="$dev" -v col="$col" '$1==dev {print $col; found=1} END {exit !found}' "$MOCK_ROOT/mounts"
        else
            path=$5
            if [[ $path == "$SC5000_MEDIA_ROOT/sc5000-sd-cache" ]]; then
                cat "$MOCK_ROOT/sd-type"
            else
                cat "$MOCK_ROOT/internal-type"
            fi
        fi ;;
    mountpoint)
        target=${@: -1}
        awk -F '|' -v target="$target" '$3==target {found=1} END {exit !found}' "$MOCK_ROOT/mounts" ;;
    mount)
        type= opts= dev= target=
        while (($#)); do
            case $1 in
                -t) type=$2; shift 2 ;;
                -o) opts=$2; shift 2 ;;
                *) if [[ -z $dev ]]; then dev=$1; else target=$1; fi; shift ;;
            esac
        done
        [[ ${MOCK_MOUNT_FAIL:-} != "$type" ]] || exit 32
        [[ ${MOCK_MOUNT_FAIL_TARGET:-} != "$target" ]] || exit 32
        printf '%s|%s|%s|%s\n' "$dev" "$type" "$target" "$opts" >> "$MOCK_ROOT/mounts"
        if [[ ${MOCK_APFS_LIBRARY:-0} == 1 && $type == apfs ]]; then
            mkdir -p "$target/Engine Library"
        fi
        if [[ ${MOCK_PDB:-0} == 1 && $type != apfs ]]; then
            export_dir=${MOCK_EXPORT_DIR:-PIONEER/rekordbox}
            mkdir -p "$target/$export_dir"
            touch "$target/$export_dir/${MOCK_EXPORT_NAME:-export.pdb}"
        fi
        if [[ ${MOCK_FAKE_EXPORT:-0} == 1 ]]; then
            mkdir -p "$target/other/data"
            touch "$target/other/data/export.pdb"
        fi
        if [[ ${MOCK_LIB_SYMLINK:-0} == 1 ]]; then
            ln -s "$MOCK_ROOT/outside" "$target/Engine Library"
        fi ;;
    umount)
        target=${@: -1}
        [[ ${MOCK_BUSY_TARGET:-} != "$target" ]] || exit 32
        awk -F '|' -v target="$target" '$3!=target' "$MOCK_ROOT/mounts" > "$MOCK_ROOT/mounts.new"
        mv "$MOCK_ROOT/mounts.new" "$MOCK_ROOT/mounts" ;;
    fsck.hfsplus) exit "${MOCK_FSCK_RC:-0}" ;;
    modprobe) [[ ${MOCK_MOD_FAIL:-} != "$1" ]] ;;
    taskset)
        [[ ${MOCK_TASKSET_FAIL:-0} == 0 ]] || exit 1
        if [[ $1 == -c ]]; then shift 2; exec "$@"; fi ;;
    rb-bridge)
        [[ -z ${MOCK_BRIDGE_SLEEP:-} ]] || sleep "$MOCK_BRIDGE_SLEEP"
        if [[ ${MOCK_ENGINE_DB:-0} == 1 ]]; then
            mkdir -p "$2/Engine Library/Database2"
            touch "$2/Engine Library/Database2/m.db"
        fi
        exit "${MOCK_BRIDGE_RC:-0}" ;;
    engine-guardian)
        [[ -z ${MOCK_GUARDIAN_SLEEP:-} ]] || sleep "$MOCK_GUARDIAN_SLEEP"
        exit "${MOCK_GUARDIAN_RC:-0}" ;;
    notify|chown|chrt) exit 0 ;;
    *) exit 127 ;;
esac
MOCK
chmod +x "$TMP/bin/mock"
for tool in blkid findmnt mountpoint mount umount fsck.hfsplus modprobe taskset rb-bridge engine-guardian notify chown chrt; do
    ln -s mock "$TMP/bin/$tool"
done
cat > "$TMP/bin/blocked-host-command" <<'MOCK'
#!/bin/sh
printf '%s\n' "$0 $*" >> "$HOST_COMMAND_LOG"
exit 1
MOCK
chmod +x "$TMP/bin/blocked-host-command"
for tool in systemctl pkill killall reboot ip; do
    ln -s blocked-host-command "$TMP/bin/$tool"
done
export HOST_COMMAND_LOG=$TMP/host-commands.log
: > "$HOST_COMMAND_LOG"
export PATH="$TMP/bin:$PATH"
count=0
fail() { printf 'FAIL: %s\n' "$*" >&2; tail -n 8 "$MOCK_ROOT/log" >&2; exit 1; }
ok() { count=$((count + 1)); printf 'ok %02d - %s\n' "$count" "$*"; }
has() { grep -Fq -- "$1" "$2" || fail "missing $1 in $2"; }
lacks() { if grep -Fq -- "$1" "$2"; then fail "unexpected $1 in $2"; fi; }
expect_fail() { if "$@" > "$MOCK_ROOT/output" 2>&1; then fail "unexpected success: $*"; fi; }
run_storage() { sh "$STORAGE" "$@" > "$MOCK_ROOT/output" 2>&1; }
fixture() {
    export MOCK_ROOT=$TMP/case-$count
    mkdir -p "$MOCK_ROOT"/{types,uuid,sys/class/block,proc,media,internal,outside}
    export SC5000_SYS_ROOT=$MOCK_ROOT/sys SC5000_PROC_ROOT=$MOCK_ROOT/proc
    export SC5000_THREAD_DELAYS= # no delayed Engine thread passes outliving a case
    export SC5000_RUN_ROOT=$MOCK_ROOT/run SC5000_MEDIA_ROOT=$MOCK_ROOT/media
    export SC5000_INTERNAL_CACHE=$MOCK_ROOT/internal
    export SC5000_UI_DIR=$MOCK_ROOT/ui SC5000_PRIMARY_UI_DIR=$MOCK_ROOT/ui SC5000_LEGACY_UI_DIR=$MOCK_ROOT/legacy-ui
    export SC5000_ASSIGNMENT_DIR=$MOCK_ROOT/assignments SC5000_ASSIGNMENT_PATCHED=$MOCK_ROOT/patched
    export SC5000_STATE_DIR=$MOCK_ROOT/state SC5000_PLANCK_STATE=$MOCK_ROOT/planck.json
    export SC5000_ENGINE_GUARDIAN=$TMP/bin/engine-guardian
    export SC5000_RB_BRIDGE=$TMP/bin/rb-bridge SC5000_STORAGE_NOTIFY=$TMP/bin/notify
    : > "$MOCK_ROOT/log"
    : > "$MOCK_ROOT/mounts"
    printf 'ext4\n' > "$MOCK_ROOT/sd-type"
    printf 'ext4\n' > "$MOCK_ROOT/internal-type"
    unset MOCK_FSCK_RC MOCK_MOUNT_FAIL MOCK_MOD_FAIL MOCK_BUSY_TARGET MOCK_PDB MOCK_FAKE_EXPORT
    unset MOCK_MOUNT_FAIL_TARGET MOCK_APFS_LIBRARY
    unset MOCK_LIB_SYMLINK MOCK_BRIDGE_RC MOCK_BRIDGE_SLEEP MOCK_TASKSET_FAIL SC5000_FRONT_SD_DEVICE
    unset SC5000_BRIDGE_TIMEOUT SC5000_GUARDIAN_TIMEOUT MOCK_ENGINE_DB MOCK_EXPORT_NAME MOCK_EXPORT_DIR MOCK_GUARDIAN_SLEEP MOCK_GUARDIAN_RC
}
device() {
    mkdir -p "$SC5000_SYS_ROOT/class/block/$1"
    printf '%s\n' "$2" > "$MOCK_ROOT/types/$1"
    printf '%s\n' "${3:-AABB-1234}" > "$MOCK_ROOT/uuid/$1"
}
sd_device() {
    device mmcblk1p1 "${1:-ext4}" SD-1234
    mkdir -p "$SC5000_SYS_ROOT/class/block/mmcblk1/device"
    printf 'SD\n' > "$SC5000_SYS_ROOT/class/block/mmcblk1/device/type"
}

fixture
device sda1 hfsplus
printf '/dev/sda1|hfsplus|%s/vivi|ro,nosuid\n' "$SC5000_MEDIA_ROOT" > "$MOCK_ROOT/mounts"
mkdir -p "$SC5000_MEDIA_ROOT/vivi/.PIONEER/rekordbox"; touch "$SC5000_MEDIA_ROOT/vivi/.PIONEER/rekordbox/export.pdb"
ACTION=add DEVNAME=/dev/sda1 run_storage
lacks 'mount ' "$MOCK_ROOT/log"
lacks 'fsck.hfsplus' "$MOCK_ROOT/log"
lacks 'rb-bridge ' "$MOCK_ROOT/log"
has 'read-only' "$MOCK_ROOT/output"
ok 'read-only HFS+ from edisksd is never remounted, checked or written'

fixture
device sda1 exfat
printf '/dev/sda1|exfat|%s/STICK|rw,nosuid\n' "$SC5000_MEDIA_ROOT" > "$MOCK_ROOT/mounts"
mkdir -p "$SC5000_MEDIA_ROOT/STICK/PIONEER/rekordbox"; touch "$SC5000_MEDIA_ROOT/STICK/PIONEER/rekordbox/export.pdb"
MOCK_ENGINE_DB=1 run_storage add /dev/sda1
lacks 'rb-bridge ' "$MOCK_ROOT/log"
lacks 'engine-guardian ' "$MOCK_ROOT/log"
lacks 'mount ' "$MOCK_ROOT/log"
has 'ready: /dev/sda1' "$MOCK_ROOT/output"
ok 'writable export is left to edisksd-hook; the worker never writes databases'

fixture
device sda1 vfat
SC5000_MOUNT_WAIT=0 run_storage add /dev/sda1
has 'not mounted by edisksd' "$MOCK_ROOT/output"
lacks 'rb-bridge ' "$MOCK_ROOT/log"
ok 'device not mounted by edisksd is left alone'

fixture
mkdir -p "$SC5000_SYS_ROOT/class/block/mmcblk0p7" "$SC5000_SYS_ROOT/class/block/mmcblk0/device"
printf 'MMC\n' > "$SC5000_SYS_ROOT/class/block/mmcblk0/device/type"
run_storage add /dev/mmcblk0p7
lacks 'findmnt' "$MOCK_ROOT/log"
expect_fail sh "$STORAGE" add /dev/../etc/passwd
expect_fail sh "$STORAGE" add /dev/sdz1x
run_storage --discover
ok 'internal eMMC, malformed devices and coldplug discovery are handled safely'

fixture
mkdir -p "$SC5000_PROC_ROOT/sys/"{vm,fs}
for knob in vm/swappiness vm/vfs_cache_pressure vm/dirty_background_ratio vm/dirty_ratio fs/file-max; do
    printf '99\n' > "$SC5000_PROC_ROOT/sys/$knob"
done
for cpu in 0 1 2 3; do
    dir=$SC5000_SYS_ROOT/devices/system/cpu/cpu$cpu/cpufreq
    mkdir -p "$dir"
    printf 'ondemand\n' > "$dir/scaling_governor"
    printf '1800000\n' > "$dir/cpuinfo_max_freq"
    printf '600000\n' > "$dir/scaling_min_freq"
    printf '1200000\n' > "$dir/scaling_max_freq"
done
gpu=$SC5000_SYS_ROOT/class/devfreq/ffa30000.gpu
mkdir -p "$gpu"
printf 'performance\n' > "$gpu/governor"
printf '100000000 200000000 300000000 400000000 600000000\n' > "$gpu/available_frequencies"
printf '600000000\n' > "$gpu/max_freq"
printf '600000000\n' > "$gpu/min_freq"
for dev in sda sdb mmcblk1; do mkdir -p "$SC5000_SYS_ROOT/block/$dev/queue"; done
printf '[none] mq-deadline kyber\n' > "$SC5000_SYS_ROOT/block/sda/queue/scheduler"
printf '128\n' > "$SC5000_SYS_ROOT/block/sda/queue/read_ahead_kb"
printf '7812500000\n' > "$SC5000_SYS_ROOT/block/sdb/size"
printf '128\n' > "$SC5000_SYS_ROOT/block/sdb/queue/read_ahead_kb"
printf '[none]\n' > "$SC5000_SYS_ROOT/block/mmcblk1/queue/scheduler"
mkdir -p "$SC5000_SYS_ROOT/block/mmcblk0/queue" "$SC5000_SYS_ROOT/devices/system/cpu/cpufreq/ondemand"
printf 'none mq-deadline kyber [bfq]\n' > "$SC5000_SYS_ROOT/block/mmcblk0/queue/scheduler"
for knob in up_threshold sampling_down_factor ignore_nice_load; do printf '0\n' > "$SC5000_SYS_ROOT/devices/system/cpu/cpufreq/ondemand/$knob"; done
mkdir -p "$SC5000_SYS_ROOT/devices/virtual/workqueue"
printf '6\n' > "$SC5000_SYS_ROOT/devices/virtual/workqueue/cpumask"
printf ' CPU0 CPU1 CPU2 CPU3\n 42: 1 0 0 0 GIC i2s-audio\n 43: 0 1 0 0 GIC ehci\n' > "$SC5000_PROC_ROOT/interrupts"
printf ' 44: 0 1 0 0 GIC ffa30000.gpu\n 45: 0 1 0 0 GIC ehci_hcd\n 46: 0 1 0 0 GIC ffb20000\n' >> "$SC5000_PROC_ROOT/interrupts"
printf ' 144: 0 1 0 0 GIC ffa30000.gpu\n 145: 0 1 0 0 GIC ff540000\n 146: 0 1 0 0 GIC ffb20000\n' >> "$SC5000_PROC_ROOT/interrupts"
printf ' 51: 0 0 1 0 GIC ff650000.i2c\n 52: 0 1 0 0 GIC panfrost-gpu\n 53: 0 1 0 0 GIC ff940000.vop\n 54: 0 1 0 0 GIC panfrost-job\n 55: 0 0 1 0 GIC eth0\n 65: 0 0 1 0 GIC dw-mci\n' >> "$SC5000_PROC_ROOT/interrupts"
for irq in 42 43 44 45 46 51 52 53 54 55 65 144 145 146; do mkdir -p "$SC5000_PROC_ROOT/irq/$irq"; printf 'f\n' > "$SC5000_PROC_ROOT/irq/$irq/smp_affinity"; done
mkdir -p "$SC5000_PROC_ROOT/100/task/"{100,101,102} "$SC5000_PROC_ROOT/200"
ln -s /usr/Engine/Engine "$SC5000_PROC_ROOT/100/exe"
printf 'EMain\n' > "$SC5000_PROC_ROOT/100/comm"
printf '/usr/Engine/Engine\n' > "$SC5000_PROC_ROOT/100/cmdline"
printf 'irq/42-i2s\n' > "$SC5000_PROC_ROOT/200/comm"
mkdir -p "$SC5000_PROC_ROOT/100/task/"{103,104,105,106,107,108,110}
for tid in 100 101 102 103 104 105 106 107 108 110; do
    policy=0; name=GUI
    if [[ $tid == 101 ]]; then policy=1; name=AudioRT; fi
    if [[ $tid == 102 ]]; then name=AudioHelper; fi
    if [[ $tid == 103 ]]; then name=SceneGraph; fi
    if [[ $tid == 104 ]]; then name=Streamer2; fi
    if [[ $tid == 105 ]]; then name=EMain; fi
    if [[ $tid == 106 ]]; then policy=2; name=AudioRenderer3; fi
    if [[ $tid == 107 ]]; then name=J:I:Takingfreez; fi
    if [[ $tid == 108 ]]; then name=QQmlThread; fi
    if [[ $tid == 110 ]]; then name=AudioOut; fi # named before its RT policy is set
    if (( tid >= 103 )); then printf 'Cpus_allowed_list:\t1\n' > "$SC5000_PROC_ROOT/100/task/$tid/status"; fi
    if [[ $tid == 107 ]]; then printf 'Cpus_allowed_list:\t0-3\n' > "$SC5000_PROC_ROOT/100/task/$tid/status"; fi
    if [[ $tid == 108 ]]; then printf 'Cpus_allowed_list:\t2\n' > "$SC5000_PROC_ROOT/100/task/$tid/status"; fi
    printf '%s\n' "$name" > "$SC5000_PROC_ROOT/100/task/$tid/comm"
    {
        printf '%s (name with spaces) S' "$tid"
        for ((field=4; field<=40; field++)); do printf ' 0'; done
        printf ' %s\n' "$policy"
    } > "$SC5000_PROC_ROOT/100/task/$tid/stat"
done
# OfflineAnalyzer can inherit the GUI's single-CPU mask; include its child too.
mkdir -p "$SC5000_PROC_ROOT/300/task"
ln -s /usr/Engine/OfflineAnalyzer "$SC5000_PROC_ROOT/300/exe"
printf '/usr/Engine/OfflineAnalyzer\n' > "$SC5000_PROC_ROOT/300/cmdline"
cp -r "$SC5000_PROC_ROOT/100/task/105" "$SC5000_PROC_ROOT/300/task/300"
cp -r "$SC5000_PROC_ROOT/100/task/105" "$SC5000_PROC_ROOT/300/task/301"
for entry in '201 144-ffa3000' '202 145-ff54000' '203 99-eth0' '204 99-other' '205 146-ffb2000' '206 54-panfrost'; do
    read -r pid name <<< "$entry"
    mkdir -p "$SC5000_PROC_ROOT/$pid"
    printf 'irq/%s\n' "$name" > "$SC5000_PROC_ROOT/$pid/comm"
done
sh "$PERF" > "$MOCK_ROOT/output" 2>&1
for cpu in 0 1 2 3; do
    has ondemand "$SC5000_SYS_ROOT/devices/system/cpu/cpu$cpu/cpufreq/scaling_governor"
    has 1200000 "$SC5000_SYS_ROOT/devices/system/cpu/cpu$cpu/cpufreq/scaling_min_freq"
done
has simple_ondemand "$gpu/governor"
has 300000000 "$gpu/min_freq"
has 50 "$SC5000_SYS_ROOT/devices/system/cpu/cpufreq/ondemand/up_threshold"
has 10 "$SC5000_SYS_ROOT/devices/system/cpu/cpufreq/ondemand/sampling_down_factor"
has '[bfq]' "$SC5000_SYS_ROOT/block/mmcblk0/queue/scheduler"
has 10 "$SC5000_PROC_ROOT/sys/vm/swappiness"
has 50 "$SC5000_PROC_ROOT/sys/vm/vfs_cache_pressure"
has 5 "$SC5000_PROC_ROOT/sys/vm/dirty_background_ratio"
has 1048576 "$SC5000_PROC_ROOT/sys/fs/file-max"
has mq-deadline "$SC5000_SYS_ROOT/block/sda/queue/scheduler"
has 1024 "$SC5000_SYS_ROOT/block/sda/queue/read_ahead_kb"
has 2048 "$SC5000_SYS_ROOT/block/sdb/queue/read_ahead_kb"
has none "$SC5000_SYS_ROOT/block/mmcblk1/queue/scheduler"
has 9 "$SC5000_SYS_ROOT/devices/virtual/workqueue/cpumask"
has 'EnvironmentFile=-/etc/default/sc5000-perf' "$ROOT/rootfs-overlay/etc/systemd/system/engine.service.d/60-ui-hook.conf"
has 'Environment=QT_LOGGING_CONF=/data/logging.ini' "$ROOT/rootfs-overlay/etc/systemd/system/engine.service.d/60-ui-hook.conf"
lacks 'taskset <-pc> <0> <101>' "$MOCK_ROOT/log"
lacks 'taskset <-pc> <1-2> <100>' "$MOCK_ROOT/log"
has 'taskset <-pc> <1> <100>' "$MOCK_ROOT/log"
has 'taskset <-pc> <2> <103>' "$MOCK_ROOT/log"
has 'taskset <-pc> <0,3> <104>' "$MOCK_ROOT/log"
has 'taskset <-pc> <0,3> <105>' "$MOCK_ROOT/log"
has 'taskset <-pc> <0,3> <107>' "$MOCK_ROOT/log"
has 'taskset <-pc> <0,3> <108>' "$MOCK_ROOT/log"
has 'taskset <-pc> <0,3> <300>' "$MOCK_ROOT/log"
has 'taskset <-pc> <0,3> <301>' "$MOCK_ROOT/log"
lacks '> <106>' "$MOCK_ROOT/log"
lacks '> <110>' "$MOCK_ROOT/log"
lacks 'chrt <-f> <-p> <40> <106>' "$MOCK_ROOT/log"
has 1 "$SC5000_PROC_ROOT/irq/42/smp_affinity"
has 8 "$SC5000_PROC_ROOT/irq/43/smp_affinity"
has 'taskset <-pc> <0> <200>' "$MOCK_ROOT/log"
has 'chrt <-f> <-p> <99> <200>' "$MOCK_ROOT/log"
has 4 "$SC5000_PROC_ROOT/irq/44/smp_affinity"
has 8 "$SC5000_PROC_ROOT/irq/45/smp_affinity"
has 1 "$SC5000_PROC_ROOT/irq/46/smp_affinity"
has 'taskset <-pc> <2> <201>' "$MOCK_ROOT/log"
has 'taskset <-pc> <3> <202>' "$MOCK_ROOT/log"
has 'taskset <-pc> <3> <203>' "$MOCK_ROOT/log"
has 'taskset <-pc> <3> <204>' "$MOCK_ROOT/log"
has 'taskset <-pc> <2> <206>' "$MOCK_ROOT/log"
has 'chrt <-f> <-p> <40> <201>' "$MOCK_ROOT/log"
has 'chrt <-f> <-p> <40> <202>' "$MOCK_ROOT/log"
has 'chrt <-f> <-p> <40> <206>' "$MOCK_ROOT/log"
has 'taskset <-pc> <0> <205>' "$MOCK_ROOT/log"
has 'chrt <-f> <-p> <99> <205>' "$MOCK_ROOT/log"
has 1 "$SC5000_PROC_ROOT/irq/146/smp_affinity"
has 4 "$SC5000_PROC_ROOT/irq/144/smp_affinity"
has 8 "$SC5000_PROC_ROOT/irq/145/smp_affinity"
for irq in 52 53 54; do has 4 "$SC5000_PROC_ROOT/irq/$irq/smp_affinity"; done
for irq in 51 55 65; do has 8 "$SC5000_PROC_ROOT/irq/$irq/smp_affinity"; done
find "$SC5000_SYS_ROOT" "$SC5000_PROC_ROOT" -type f -exec sha256sum {} + | sort > "$MOCK_ROOT/before"
sh "$PERF" > "$MOCK_ROOT/output" 2>&1
find "$SC5000_SYS_ROOT" "$SC5000_PROC_ROOT" -type f -exec sha256sum {} + | sort > "$MOCK_ROOT/after"
cmp -s "$MOCK_ROOT/before" "$MOCK_ROOT/after" || fail 'performance tuning is not idempotent'
ok 'CPU/GPU, sysctl, schedulers, IRQs and RT-vs-GUI affinity are idempotent'

# A network helper born after the startup passes must leave CPU1 on a periodic
# sweep. Fake sleep advances those phases deterministically, then ends Engine.
cat > "$TMP/bin/sleep" <<'SLEEP'
#!/bin/bash
set -eu
n=$(cat "$MOCK_ROOT/sleep-count" 2>/dev/null || printf 0)
n=$((n + 1))
printf '%s\n' "$n" > "$MOCK_ROOT/sleep-count"
if [[ $n == 2 ]]; then
    cp -r "$SC5000_PROC_ROOT/100/task/105" "$SC5000_PROC_ROOT/100/task/109"
    printf 'event_engine\n' > "$SC5000_PROC_ROOT/100/task/109/comm"
elif [[ $n == 3 ]]; then
    printf '%s\n' "$PPID" > "$MOCK_ROOT/sweeper-pid"
    unlink "$SC5000_PROC_ROOT/100/exe"
    touch "$MOCK_ROOT/sweeper-done"
fi
SLEEP
chmod +x "$TMP/bin/sleep"
SC5000_THREAD_DELAYS=0 SC5000_THREAD_INTERVAL=30 sh "$PERF" > "$MOCK_ROOT/output" 2>&1
for ((attempt=0; attempt<100; attempt++)); do
    [[ ! -e $MOCK_ROOT/sweeper-done ]] || break
    /bin/sleep 0.05
done
[[ -e $MOCK_ROOT/sweeper-done ]] || fail 'periodic sweeper did not stop with Engine'
sweeper_pid=$(cat "$MOCK_ROOT/sweeper-pid")
for ((attempt=0; attempt<100; attempt++)); do
    sweeper_state=$(awk '{print $3}' "/proc/$sweeper_pid/stat" 2>/dev/null || true)
    [[ -n $sweeper_state && $sweeper_state != Z ]] || break
    /bin/sleep 0.05
done
[[ -z $sweeper_state || $sweeper_state == Z ]] || fail 'sweeper survived Engine exit'
has 'taskset <-pc> <0,3> <109>' "$MOCK_ROOT/log"
unlink "$TMP/bin/sleep"
ln -s /usr/Engine/Engine "$SC5000_PROC_ROOT/100/exe"
ok 'late helpers leave the GUI core and the sweeper stops with Engine'

# The sweep runs beside playback every 30 s: no process per Engine thread.
for tool in cat sed awk; do
    real=$(command -v "$tool")
    printf '#!/bin/sh\nprintf "%%s\\n" "%s $*" >> "$MOCK_ROOT/forks"\nexec %s "$@"\n' "$tool" "$real" > "$TMP/bin/$tool"
    chmod +x "$TMP/bin/$tool"
done
: > "$MOCK_ROOT/forks"; : > "$MOCK_ROOT/log"
sh "$PERF" > "$MOCK_ROOT/output" 2>&1
rm "$TMP/bin/cat" "$TMP/bin/sed" "$TMP/bin/awk"
lacks '/task/' "$MOCK_ROOT/forks"
has 'taskset <-pc> <2> <103>' "$MOCK_ROOT/log"
ok 'the Engine thread sweep reads /proc with shell builtins'

# Hot at Engine start: the GPU idles at its lowest clock.
mkdir -p "$SC5000_SYS_ROOT/class/thermal/thermal_zone0"
printf '90000\n' > "$SC5000_SYS_ROOT/class/thermal/thermal_zone0/temp"
sh "$PERF" > "$MOCK_ROOT/output" 2>&1
has 100000000 "$gpu/min_freq"
ok 'a hot deck keeps the lowest GPU clock'

MOCK_TASKSET_FAIL=1 sh "$PERF" > "$MOCK_ROOT/output" 2>&1
rm "$gpu/governor"
ln -s /dev/full "$gpu/governor"
sh "$PERF" > "$MOCK_ROOT/output" 2>&1
ok 'thread exit/affinity failure and disappearing sysfs knobs are nonfatal'

fixture
sh "$PERF" > "$MOCK_ROOT/output" 2>&1
ok 'missing cpufreq, GPU, IRQ and process facilities are nonfatal'

for service in sc5000-storage@.service rb-bridge.service.d/50-sc5000-background.conf \
    stream-sync.service.d/50-sc5000-background.conf ssh.service.d/50-sc5000-background.conf \
    sshd.service.d/50-sc5000-background.conf; do
    file=$ROOT/rootfs-overlay/etc/systemd/system/$service
    has 'CPUAffinity=3' "$file"
    has 'CPUWeight=10' "$file"
    has 'IOWeight=10' "$file"
done
has 'IMPORT{builtin}="blkid"' "$ROOT/rootfs-overlay/etc/udev/rules.d/60-sc5000-storage.rules"
lacks 'UDISKS_IGNORE' "$ROOT/rootfs-overlay/etc/udev/rules.d/60-sc5000-storage.rules"
lacks ', RUN+=' "$ROOT/rootfs-overlay/etc/udev/rules.d/60-sc5000-storage.rules"
has 'Restart=always' "$ROOT/rootfs-overlay/etc/systemd/system/rb-bridge.service"
ok 'background service isolation and systemd udev handoff are installed'

for script in "$STORAGE" "$PERF" "$ROOT/rootfs-overlay/usr/local/bin/sc5000-suite-exec" "$ROOT/rootfs-overlay/etc/sc5000/engine-ui-guard"; do
    [[ $(head -n 1 "$script") == '#!/bin/sh' ]] || fail "$script must use /bin/sh (no bash on Engine OS)"
    if command -v busybox >/dev/null; then busybox sh -n "$script" || fail "busybox sh syntax: $script"; else sh -n "$script"; fi
done
ok 'device scripts run under /bin/sh'

suite_share=$TMP/suite-share suite_dir=$TMP/suite-bin
mkdir -p "$suite_share"
printf '#!/bin/sh\necho image-1 "$@"\n' | gzip -c > "$suite_share/sc5000-suite.gz"
run_suite() { SC5000_SUITE_SHARE=$suite_share SC5000_SUITE_DIR=$suite_dir sh "$ROOT/rootfs-overlay/usr/local/bin/sc5000-suite-exec" x; }
[[ $(run_suite) == 'image-1 x' ]] || fail 'suite archive is unpacked on first start'
printf '#!/bin/sh\necho hand "$@"\n' > "$suite_dir/sc5000-suite"
touch -t 200001010000 "$suite_dir/sc5000-suite"
[[ $(run_suite) == 'hand x' ]] || fail 'a hand-deployed suite survives restarts of the same image'
printf '#!/bin/sh\necho image-2 "$@"\n' | gzip -c > "$suite_share/sc5000-suite.gz"
touch -t 200001010000 "$suite_share/sc5000-suite.gz"
touch "$suite_dir/sc5000-deemixd" "$suite_dir/stems-processor" "$suite_dir/sc5000-suite.prev" "$suite_dir/sc5000-suite.pre-x" "$suite_dir/drmgrab"
[[ $(run_suite) == 'image-2 x' ]] || fail 'a new image archive replaces the suite even with an older mtime'
[[ ! -e $suite_dir/sc5000-deemixd && ! -e $suite_dir/stems-processor && ! -e $suite_dir/sc5000-suite.prev && ! -e $suite_dir/sc5000-suite.pre-x && -e $suite_dir/drmgrab ]] ||
    fail 'a new image removes the replaced standalone binaries and backups only'
ok 'suite binary follows the image archive content, not mtime'

fixture
expect_fail env TARGET_CONFIG= TARGET_SYMVERS= bash "$ROOT/kernel-modules/build_modules.sh" modules
has 'TARGET_CONFIG' "$MOCK_ROOT/output"
ok 'module build refuses missing target ABI inputs before building'
debloat_root=$TMP/debloat-root
mkdir -p "$debloat_root/etc"
printf '127.0.0.1 localhost\n0.0.0.0 inmusicbrands.com\n' > "$debloat_root/etc/hosts"
sh "$ROOT/rootfs-overlay/usr/local/bin/sc5000-debloat" --root "$debloat_root"
sh "$ROOT/rootfs-overlay/usr/local/bin/sc5000-debloat" --root "$debloat_root"
lacks '0.0.0.0 inmusicbrands.com' "$debloat_root/etc/hosts"
lacks 'address=/inmusicbrands.com/' "$debloat_root/etc/dnsmasq.d/sc5000-telemetry.conf"
has '0.0.0.0 telemetry.inmusicbrands.com' "$debloat_root/etc/hosts"
has 'address=/logstash.inmusicbrands.com/0.0.0.0' "$debloat_root/etc/dnsmasq.d/sc5000-telemetry.conf"
[[ $(grep -c 'logstash.inmusicbrands.com' "$debloat_root/etc/hosts") == 1 ]] || fail 'hosts entries duplicated'
has 'EnableOnlineCheck = true' "$debloat_root/etc/connman/main.conf"
has 'OnlineCheckIPv4URL = http://ipv4.connman.inmusicbrands.com/' "$debloat_root/etc/connman/main.conf"
has 'PreferredTechnologies=wifi,ethernet' "$debloat_root/etc/connman/main.conf"
has 'CONNMAN_DISCOVER_RETRIES=5' "$debloat_root/etc/systemd/system/connman.service.d/disable_dhcp_retries.conf"
has '[ntfs3]' "$debloat_root/etc/edisksd/main.conf"
lacks '[hfsplus]' "$debloat_root/etc/edisksd/main.conf"
# #60: Bluetooth stays on for every board (JC11, JC16, JP07, JP13); nothing masks it.
grep -Eq '^ +if command -v rfkill .*; then rfkill unblock bluetooth' "$ROOT/rootfs-overlay/usr/local/bin/sc5000-debloat" || fail 'debloat no longer unblocks Bluetooth'
if grep -rEil 'bluetooth' "$debloat_root/etc/systemd" | grep -q .; then fail 'debloat touches a Bluetooth unit'; fi
for module in btbcm hci_uart; do
    if grep -qx "$module" "$ROOT/rootfs-overlay/etc/modules-load.d/sc5000-filesystems.conf"; then fail "$module must not be force-loaded"; fi
done
has 'feature_disable=0x82004' "$ROOT/rootfs-overlay/usr/lib/modprobe.d/brcmfmac.conf"
ok 'stock connman online check, read-only HFS+, rootfs brcmfmac options'

GUARD=$ROOT/rootfs-overlay/etc/sc5000/engine-ui-guard
export SC5000_UI_DIR=$TMP/ui-guard SC5000_STREAMING_ACCOUNTS=$TMP/StreamingAccounts.json
export SC5000_ENGINE_DATA_DIR=$TMP/engine-data
# #56: every start leaves Engine a TIDAL account entry (valid JSON, other accounts
# kept) unless TIDAL itself is the selected service.
tidal_entry() { python3 -c 'import json,sys; a=json.load(open(sys.argv[1])); d=json.loads(a["TIDAL"]); assert d["providerType"] == "TIDAL" and d.get("accessToken") == "enginex" and d.get("refreshToken") == "enginex"; print(len(a))' "$SC5000_STREAMING_ACCOUNTS"; }
sh "$GUARD" pre; [[ $(tidal_entry) == 1 ]] || fail 'guard must create the TIDAL account entry with non-SSO tokens (#159)'
printf '{\n    "SoundCloud": "{\\"provider\\":\\"SoundCloud\\"}"\n}\n' > "$SC5000_STREAMING_ACCOUNTS"
sh "$GUARD" pre; [[ $(tidal_entry) == 2 ]] || fail 'guard must add TIDAL beside other accounts'
printf '{\n    "TIDAL": "{\\"provider\\":\\"TIDAL\\",\\"providerType\\":\\"TIDAL\\"}"\n}\n' > "$SC5000_STREAMING_ACCOUNTS"
sh "$GUARD" pre; [[ $(tidal_entry) == 1 ]] || fail 'guard must upgrade legacy tokenless TIDAL entry (#159)'
cp "$SC5000_STREAMING_ACCOUNTS" "$TMP/accounts.before"; sh "$GUARD" pre
cmp -s "$TMP/accounts.before" "$SC5000_STREAMING_ACCOUNTS" || fail 'guard must leave an existing TIDAL entry alone'
rm "$SC5000_STREAMING_ACCOUNTS"; mkdir -p "$SC5000_UI_DIR"; echo scope=tidal > "$SC5000_UI_DIR/settings.conf"
sh "$GUARD" pre; [[ ! -e $SC5000_STREAMING_ACCOUNTS ]] || fail 'guard must not touch accounts while TIDAL is selected'
# #224: disabled TIDAL cannot retain stale upstream routing across boot.
printf 'scope=tidal\nsources=spotify,netease\n' > "$SC5000_UI_DIR/settings.conf"
sh "$GUARD" pre
! grep -qx 'scope=tidal' "$SC5000_UI_DIR/settings.conf" || fail 'disabled TIDAL retained stale scope'
[[ $(tidal_entry) == 1 ]] || fail 'disabled TIDAL must seed the gateway account'
has 'Wants=stream-sync.service' "$ROOT/rootfs-overlay/etc/systemd/system/engine.service.d/60-ui-hook.conf"
has 'After=stream-sync.service' "$ROOT/rootfs-overlay/etc/systemd/system/engine.service.d/60-ui-hook.conf"
rm -rf "$SC5000_UI_DIR"
for _ in 1 2 3; do SERVICE_RESULT=success sh "$GUARD" stop; sh "$GUARD" pre; done
[[ ! -e $SC5000_UI_DIR/safe-mode && $(cat "$SC5000_UI_DIR/boot-count") == 1 ]] || fail 'clean restarts must not reach safe-mode'
for result in signal core-dump exit-code; do SERVICE_RESULT=$result sh "$GUARD" stop; sh "$GUARD" pre; done
[[ -e $SC5000_UI_DIR/safe-mode ]] || fail 'three crashed starts must enter safe-mode'
# #140: a corrupt Qt QML cache must not survive a crash, safe-mode or a hung/hard-powered-off start (boot-token left).
SERVICE_RESULT=signal sh "$GUARD" stop; mkdir -p "$SC5000_ENGINE_DATA_DIR/.cache/qml"; sh "$GUARD" pre
[[ ! -e $SC5000_ENGINE_DATA_DIR/.cache/qml ]] || fail 'a crashed start must clear the QML disk cache'
rm -f "$SC5000_UI_DIR"/*; mkdir -p "$SC5000_ENGINE_DATA_DIR/.cache/qml"; sh "$GUARD" pre
[[ -d $SC5000_ENGINE_DATA_DIR/.cache/qml ]] || fail 'a clean start must keep the QML disk cache'
sh "$GUARD" post; mkdir -p "$SC5000_ENGINE_DATA_DIR/.cache/qml"; sh "$GUARD" pre
[[ ! -e $SC5000_ENGINE_DATA_DIR/.cache/qml && $(cat "$SC5000_UI_DIR/boot-count") == 2 ]] || fail 'a leftover boot-token must count as a crash and clear the QML cache'
# #140: power cut after a healthy start (no stop hook, no boot-token) still drops the cache but is no crash.
rm -f "$SC5000_UI_DIR"/*; sh "$GUARD" pre; mkdir -p "$SC5000_ENGINE_DATA_DIR/.cache/qml"; sh "$GUARD" pre
[[ ! -e $SC5000_ENGINE_DATA_DIR/.cache/qml && $(cat "$SC5000_UI_DIR/boot-count") == 1 ]] || fail 'a start without a stop hook must clear the QML cache without counting a crash'
SERVICE_RESULT=success sh "$GUARD" stop; mkdir -p "$SC5000_ENGINE_DATA_DIR/.cache/qml"; sh "$GUARD" pre
[[ -d $SC5000_ENGINE_DATA_DIR/.cache/qml ]] || fail 'a clean stop must keep the QML disk cache'
# #140: the healthy timer leaves the token when Engine is gone, so the next pre counts a crash.
SC5000_UI_HEALTHY_AFTER=1 SC5000_UI_ALIVE_CMD=false sh "$GUARD" post; sleep 1.5
[[ -e $SC5000_UI_DIR/boot-token ]] || fail 'a dead Engine must not clear boot-token'
# #140: a hung engine-guardian must not block Engine start.
if command -v timeout >/dev/null; then
    printf '#!/bin/sh\nsleep 60\n' > "$TMP/slow-guardian"; chmod +x "$TMP/slow-guardian"
    start=$SECONDS; SC5000_ENGINE_GUARDIAN=$TMP/slow-guardian sh "$GUARD" pre
    (( SECONDS - start < 20 )) || fail 'guard pre must time out a hung engine-guardian'
fi
# #7: a crashed start's timer must not reset the streak of the starts after it.
rm -f "$SC5000_UI_DIR"/*
export SC5000_UI_HEALTHY_AFTER=1 SC5000_UI_ALIVE_CMD=true
sh "$GUARD" pre; sh "$GUARD" post
SERVICE_RESULT=signal sh "$GUARD" stop; sh "$GUARD" pre
sleep 1.5
[[ $(cat "$SC5000_UI_DIR/boot-count") == 2 ]] || fail 'stale timer reset the crash streak'
sh "$GUARD" post; sleep 1.5
[[ -e $SC5000_UI_DIR/boot-count ]] || fail 'uptime alone must not clear a boot without UI readiness'
token=$(cat "$SC5000_UI_DIR/boot-token")
sh "$GUARD" ready "$token"
SC5000_BOOT_POLL_INTERVAL=.1 sh "$GUARD" watch > "$TMP/ui-watch.log" 2>&1 & ui_watch=$!
sleep .3
kill "$ui_watch"; wait "$ui_watch" || :
[[ ! -e $SC5000_UI_DIR/boot-count ]] || fail 'only the current UI-ready attempt may clear boot-count'
# #140 (b82197b): a power cut after a UI-ready start leaves running-sentinel; the second boot
# purges the possibly truncated QML cache and is no crash.
[[ -e $SC5000_UI_DIR/running-sentinel ]] || fail 'healthy start before power cut must keep running-sentinel'
mkdir -p "$SC5000_ENGINE_DATA_DIR/.cache/qml"; printf truncated > "$SC5000_ENGINE_DATA_DIR/.cache/qml/powercut.qmlc"
sh "$GUARD" pre
[[ ! -e $SC5000_ENGINE_DATA_DIR/.cache/qml && ! -e $SC5000_UI_DIR/safe-mode && $(cat "$SC5000_UI_DIR/boot-count") == 1 && -e $SC5000_UI_DIR/running-sentinel ]] ||
    fail 'second boot after healthy power cut must purge .cache/qml without entering safe-mode'
has '--phase boot2' "$ROOT/tools/az01-packer/release-gate.sh"
has 'boot2: main UI acknowledged the current boot attempt' "$ROOT/tools/az01-packer/release-gate.sh"
unset SC5000_UI_HEALTHY_AFTER SC5000_UI_ALIVE_CMD
# Quit reasons the stock script exits 0 after: the guard must restart Engine unless
# the system is already rebooting (FactoryReset when az01-update --erase-data worked).
mkdir -p "$TMP/guard-bin"
cat > "$TMP/guard-bin/systemctl" <<'MOCK'
#!/bin/sh
echo "$*" >> "$GUARD_LOG"
[ "$1" != is-system-running ] || echo "$GUARD_STATE"
MOCK
chmod +x "$TMP/guard-bin/systemctl"
export SC5000_QUIT_REASON_FILE=$TMP/quit-reason GUARD_LOG=$TMP/guard-log
for case in 'FactoryReset running 1' 'FactoryReset degraded 1' 'FactoryReset stopping 0' 'AnalyticsChange stopping 1' 'Poweroff running 0' ' running 0'; do
    set -- $case; [[ $# == 3 ]] || set -- '' "$@"
    [[ -z $1 ]] && rm -f "$SC5000_QUIT_REASON_FILE" || echo "$1" > "$SC5000_QUIT_REASON_FILE"
    : > "$GUARD_LOG"
    GUARD_STATE=$2 SERVICE_RESULT=success PATH=$TMP/guard-bin:$PATH sh "$GUARD" stop
    started=$(grep -c '^start --no-block engine.service$' "$GUARD_LOG" || :)
    [[ $started == "$3" ]] || fail "guard stop reason='$1' state=$2 started=$started want $3"
    if [[ $3 == 1 && -e $SC5000_QUIT_REASON_FILE ]]; then fail "guard must consume quit reason $1"; fi
done
unset SC5000_QUIT_REASON_FILE GUARD_LOG
# #49: in the 4-deck layout the guard binds, per start, a player's assignment file for
# its role (bindings only: the assignment loader rejects handlers); off, the stock file.
assign=$TMP/assign; mkdir -p "$assign/JP07" "$assign/JP13" "$TMP/ui-roles"
printf 'import airAssignments 1.0\nMidiAssignment {\n    objectName: "SC5000 Controller Assignment"\n    Layer {\n        note: 11\n        midiChannel: globalConfig.midiChannel\n    }\n    DeckAssignmentConfig {\n        id: deckConfig\n        midiChannel: 1\n    }\n}\n' > "$assign/JP07/JP07_Controller_Assignments.qml"
printf 'Item {}\n' > "$assign/JP13/JP13_Controller_Assignments.qml"
cat > "$TMP/guard-bin/mount" <<'MOCK'
#!/bin/sh
echo "mount $*" >> "$GUARD_LOG"; echo "$3" >> "$GUARD_LOG.bound"
MOCK
cat > "$TMP/guard-bin/umount" <<'MOCK'
#!/bin/sh
echo "umount $*" >> "$GUARD_LOG"; grep -vxF "$1" "$GUARD_LOG.bound" > "$GUARD_LOG.tmp"; mv "$GUARD_LOG.tmp" "$GUARD_LOG.bound"
MOCK
cat > "$TMP/guard-bin/mountpoint" <<'MOCK'
#!/bin/sh
grep -qxF "$2" "$GUARD_LOG.bound" 2>/dev/null
MOCK
chmod +x "$TMP/guard-bin/mount" "$TMP/guard-bin/umount" "$TMP/guard-bin/mountpoint"
printf '{\n  "Player": {\n    "string": "2",\n    "type": 4\n  }\n}\n' > "$TMP/planck-state.json"
export GUARD_LOG=$TMP/guard-log SC5000_ASSIGNMENT_DIR=$assign SC5000_ASSIGNMENT_PATCHED=$TMP/assign-patched SC5000_PLANCK_STATE=$TMP/planck-state.json
j=$TMP/assign-patched/JP07_Controller_Assignments.qml
role_case() { # <four_deck_role or ""> <resolved role> <group in the name binding> <LAYER: stock|own|none>
    printf 'four_deck_waveform=1\n%s\n' "${1:+four_deck_role=$1}" > "$TMP/ui-roles/settings.conf"
    SC5000_UI_DIR=$TMP/ui-roles PATH=$TMP/guard-bin:$PATH sh "$GUARD" pre
    [[ $(cat "$TMP/assign-patched/role") == "$2" ]] || fail "role '$1' resolved to $(cat "$TMP/assign-patched/role"), want $2"
    [[ $(grep -c '^mount --bind' "$GUARD_LOG") == 1 ]] || fail "role '$1': want exactly one bind"
    grep -A2 '^        id: deckConfig$' "$j" | grep -q "name: Config.device.isPlayer ? \"\" : $3" || fail "role '$1': name binding"
    case $4 in
    stock) has '    Layer {' "$j"; lacks 'ValueNoteAssignment' "$j" ;;
    own) lacks '    Layer {' "$j"; has 'path: "/GUI/Decks/DeckLeft/Layer"' "$j"; has 'note: 11' "$j"; has 'import Planck 1.0' "$j" ;;
    none) lacks '    Layer {' "$j"; lacks 'ValueNoteAssignment' "$j" ;;
    esac
    if grep -q '^ *on[A-Z][A-Za-z]*:' "$j"; then fail "role '$1': handlers break the assignment loader"; fi
    : > "$GUARD_LOG"
}
: > "$GUARD_LOG"; rm -f "$GUARD_LOG.bound"
role_case 13 13 '"Left"' stock
role_case 24 24 '"Right"' stock
role_case 12 12 'Planck.getProperty' own
role_case 34 34 'Planck.getProperty' own
role_case 1 1 '"Left"' none
role_case 2 2 '"Right"' none
# FOOLPROOF (#49/#123): auto maps odd Player -> 13, even -> 24, with or without any paired-player file; never 12
player_state() { printf '{\n  "Player": {\n    "string": "%s",\n    "type": 4\n  }\n}\n' "$1" > "$TMP/planck-state.json"; }
export SC5000_PAIRED_PLAYER=$TMP/paired-player
for paired in absent present; do
    rm -f "$TMP/paired-player"; [[ $paired == absent ]] || touch "$TMP/paired-player"
    for mode in '' auto; do
        player_state 1; role_case "$mode" 13 '"Left"' stock
        player_state 2; role_case "$mode" 24 '"Right"' stock
    done
done
rm -f "$TMP/paired-player"
unset SC5000_PAIRED_PLAYER
[[ ! -e $TMP/assign-patched/JP13_Controller_Assignments.qml ]] || fail 'an unpatchable file must not be bound'
printf 'lang=en\n' > "$TMP/ui-roles/settings.conf"
SC5000_UI_DIR=$TMP/ui-roles PATH=$TMP/guard-bin:$PATH sh "$GUARD" pre
has "umount $assign/JP07/JP07_Controller_Assignments.qml" "$GUARD_LOG"
if grep -q '^mount --bind' "$GUARD_LOG"; then fail 'the 4-deck layout off must leave the stock assignment files'; fi
unset GUARD_LOG SC5000_ASSIGNMENT_DIR SC5000_ASSIGNMENT_PATCHED SC5000_PLANCK_STATE
has 'ExecStopPost=-/etc/sc5000/engine-ui-guard stop' "$ROOT/rootfs-overlay/etc/systemd/system/engine.service.d/override.conf"
has 'ExecStopPost=-/etc/sc5000/engine-ui-guard stop' "$debloat_root/etc/systemd/system/engine.service.d/override.conf"
for conf in "$ROOT/rootfs-overlay/etc/systemd/system/engine.service.d/override.conf" "$debloat_root/etc/systemd/system/engine.service.d/override.conf"; do
    has 'Environment=HOME=/tmp/sc5000-engine-home' "$conf"
    has "command -v connmanctl >/dev/null || exit 0" "$conf"
done
has 'SC5000_BOOT_TIMEOUT:-180' "$GUARD"
has 'SC5000_BOOT_CRASH_LIMIT:-3' "$GUARD"
has 'SC5000_BOOT_CRASH_WINDOW:-300' "$GUARD"
unset SC5000_UI_DIR
ok 'UI guard: deliberate restarts reset the boot count, crashes still reach safe-mode, failed FactoryReset restarts Engine'

lacks '"/GUI/Scripted/ZoomIn"' "$ROOT/tools/ui-mod/build_packs.py"
lacks 'BrowseEncoder.qml' "$ROOT/tools/ui-mod/build_packs.py"
ok 'browse encoder: stock zoom steps and stock hold actions'

# #148: About shows the LAN stems renderer.
has 'scLanStems' "$ROOT/tools/ui-mod/build_packs.py"
has 'lanStems' "$ROOT/pkg/webui/ui_settings.go"
ok 'About shows the LAN stems renderer'

# #146: on-screen previous/next track only on PRIME GO / Mixstream (no physical Track Skip keys).
has 'scNoSkipKeys' "$ROOT/tools/ui-mod/build_packs.py"
has 'NH08S' "$ROOT/tools/ui-mod/build_packs.py"
has '/Track/${modelData.n}' "$ROOT/tools/ui-mod/build_packs.py"
ok 'on-screen track skip buttons only on models without physical keys'

# Time sync & timezone persistence: mainland-China + global NTP drop-in, etc-reset survival, Gen1 & Gen2 parity.
NTP_CONF=$ROOT/rootfs-overlay/etc/systemd/timesyncd.conf.d/10-enginex-ntp.conf
[[ -f $NTP_CONF ]] || fail "missing $NTP_CONF"
has "[Time]" "$NTP_CONF"
has "FallbackNTP=" "$NTP_CONF"
has "FallbackNTP=ntp.aliyun.com ntp.tencent.com time.apple.com time.cloudflare.com pool.ntp.org time.google.com" "$NTP_CONF"
cmp -s "$NTP_CONF" "$debloat_root/etc/systemd/timesyncd.conf.d/10-enginex-ntp.conf" ||
    fail "sc5000-debloat --root timesyncd drop-in differs from rootfs-overlay"
python3 -I - "$ROOT" "$TMP" <<'PY'
import importlib.util, subprocess, sys
from pathlib import Path
repo, tmp = Path(sys.argv[1]), Path(sys.argv[2])
spec = importlib.util.spec_from_file_location("az01_packer", repo / "tools/az01-packer/az01_packer.py")
az = importlib.util.module_from_spec(spec); spec.loader.exec_module(az)
etc_files = sorted(p.relative_to(repo / "rootfs-overlay/etc").as_posix()
                   for p in (repo / "rootfs-overlay/etc").rglob("*") if p.is_file())
assert "systemd/timesyncd.conf.d/10-enginex-ntp.conf" in etc_files
data_dir = tmp / "etc-reset-ntp"
upper = data_dir / "system/etc/overlay"
lower = tmp / "lower-etc"
(lower / "systemd/timesyncd.conf.d").mkdir(parents=True, exist_ok=True)
want = (repo / "rootfs-overlay/etc/systemd/timesyncd.conf.d/10-enginex-ntp.conf").read_text()
(lower / "systemd/timesyncd.conf.d/10-enginex-ntp.conf").write_text(want)
for stale in ("systemd/timesyncd.conf", "systemd/timesyncd.conf.d/10-enginex-ntp.conf", "systemd/timesyncd.conf.d/99-stale.conf"):
    (upper / stale).parent.mkdir(parents=True, exist_ok=True)
    (upper / stale).write_text("[Time]\nFallbackNTP=time1.google.com\n")
script = az.etc_reset("5.3.0.4-test", etc_files).replace(b"/data", str(data_dir).encode())
r = subprocess.run(["sh", "-s"], input=script, capture_output=True)
assert r.returncode == 0, r.stderr.decode()
assert not (upper / "systemd/timesyncd.conf").exists()
assert not (upper / "systemd/timesyncd.conf.d").exists()
assert (lower / "systemd/timesyncd.conf.d/10-enginex-ntp.conf").read_text() == want

bspec = importlib.util.spec_from_file_location("build_data_bundle", repo / "tools/az01-packer/build-data-bundle.py")
bdb = importlib.util.module_from_spec(bspec); bspec.loader.exec_module(bdb)
ba = bdb.BOOT_ACTIVATE.replace("@MIGRATE_IF_DEFAULT@", bdb.MIGRATE_IF_DEFAULT).replace("@TIMESYNCD_DROPIN@", want)
assert "/run/systemd/timesyncd.conf.d/10-enginex-ntp.conf" in ba
assert want in ba
assert "systemctl restart --no-block systemd-timesyncd.service" in ba
assert "/run/systemd/timesyncd.conf.d/10-enginex-ntp.conf" in bdb.UNINSTALL
PY
SEED_CONF=$ROOT/rootfs-overlay/usr/lib/systemd/system/systemd-timesyncd.service.d/10-enginex-seed.conf
SEED_BIN=$ROOT/rootfs-overlay/usr/libexec/sc5000-timesync-seed
[[ -f $SEED_CONF ]] || fail "missing $SEED_CONF"
[[ -x $SEED_BIN ]] || fail "missing executable $SEED_BIN"
has "TimeoutStartSec=15s" "$SEED_CONF"
has "ExecStartPre=-+/usr/libexec/sc5000-timesync-seed" "$SEED_CONF"
lacks 'timesync_persist' "$ROOT/tools/ui-mod/engine_ui_hook.c"
lacks 'persist_monotonic_clock' "$ROOT/tools/ui-mod/engine_ui_hook.c"
has 'onSystemTimeZoneSetSucceeded' "$ROOT/tools/ui-mod/build_packs.py"
has 'watchPersistentClock' "$ROOT/internal/cli/streamsync.go"
ok "timesyncd drop-in, etc-reset survival, Gen1/Gen2 parity and TZ/clock persistence unit checks"

cmp -s "$ROOT/rootfs-overlay/etc/sc5000/libengine-ui-hook.so" "$ROOT/rootfs-overlay/usr/local/lib/sc5000/libengine-ui-hook.so" ||
    fail 'both libengine-ui-hook.so copies in rootfs-overlay must match'
if command -v arm-linux-gnueabihf-gcc >/dev/null 2>&1 && command -v arm-linux-gnueabihf-strip >/dev/null 2>&1 && command -v arm-linux-gnueabihf-readelf >/dev/null 2>&1; then
    OUT=$TMP/libengine-ui-hook.so sh "$ROOT/tools/ui-mod/build.sh"
    cmp -s "$TMP/libengine-ui-hook.so" "$ROOT/rootfs-overlay/etc/sc5000/libengine-ui-hook.so" ||
        fail 'rootfs-overlay libengine-ui-hook.so is out of sync with tools/ui-mod/engine_ui_hook.c'
    OUT=$TMP/libedisksd-hook.so sh "$ROOT/tools/edisksd-hook/build.sh"
    cmp -s "$TMP/libedisksd-hook.so" "$ROOT/rootfs-overlay/etc/sc5000/libedisksd-hook.so" ||
        fail 'rootfs-overlay libedisksd-hook.so is out of sync with tools/edisksd-hook/edisksd_hook.c'
    ok 'committed 32-bit hooks in rootfs-overlay match fresh source builds byte-for-byte'
fi

python3 -m unittest discover -s "$ROOT/sim/qengine" -p test_smoke.py >/dev/null
[[ ! -s $HOST_COMMAND_LOG ]] || fail "unexpected host command: $(cat "$HOST_COMMAND_LOG")"
ok 'host systemctl, pkill, killall, reboot and ip were never called'
printf 'PASS: %d tests (mocked storage and hardware; no privileged operations)\n' "$count"
