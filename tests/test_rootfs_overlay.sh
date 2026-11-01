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
            dev=$3
            awk -F '|' -v dev="$dev" '$1==dev {print $3; found=1} END {exit !found}' "$MOCK_ROOT/mounts"
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
export PATH="$TMP/bin:$PATH"
count=0
fail() { printf 'FAIL: %s\n' "$*" >&2; tail -n 8 "$MOCK_ROOT/log" >&2; exit 1; }
ok() { count=$((count + 1)); printf 'ok %02d - %s\n' "$count" "$*"; }
has() { grep -Fq -- "$1" "$2" || fail "missing $1 in $2"; }
lacks() { if grep -Fq -- "$1" "$2"; then fail "unexpected $1 in $2"; fi; }
expect_fail() { if "$@" > "$MOCK_ROOT/output" 2>&1; then fail "unexpected success: $*"; fi; }
run_storage() { bash "$STORAGE" "$@" > "$MOCK_ROOT/output" 2>&1; }
fixture() {
    export MOCK_ROOT=$TMP/case-$count
    mkdir -p "$MOCK_ROOT"/{types,uuid,sys/class/block,proc,media,internal,outside}
    export SC5000_SYS_ROOT=$MOCK_ROOT/sys SC5000_PROC_ROOT=$MOCK_ROOT/proc
    export SC5000_RUN_ROOT=$MOCK_ROOT/run SC5000_MEDIA_ROOT=$MOCK_ROOT/media
    export SC5000_INTERNAL_CACHE=$MOCK_ROOT/internal
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
ACTION=add DEVNAME=/dev/sda1 run_storage
has 'fsck.hfsplus <-f> </dev/sda1>' "$MOCK_ROOT/log"
has '<force,rw,uid=1000,gid=1000,umask=0000,nodev,nosuid>' "$MOCK_ROOT/log"
fsck_line=$(grep -n '^fsck.hfsplus ' "$MOCK_ROOT/log" | cut -d: -f1)
mount_line=$(grep -n '^mount ' "$MOCK_ROOT/log" | cut -d: -f1)
[[ $fsck_line -lt $mount_line ]] || fail 'HFS check must precede mount'
[[ -d $SC5000_MEDIA_ROOT/sc5000-sda1/Engine\ Library ]] || fail 'missing Engine Library'
ok 'udev HFS+ detection, fsck before forced RW, library creation'

fixture
device sda1 hfsplus
run_storage add /dev/sda1
run_storage change /dev/sda1
[[ $(grep -c '^fsck.hfsplus ' "$MOCK_ROOT/log") == 1 ]] || fail 'duplicate event repeated fsck'
ok 'repeated udev events do not recheck a mounted HFS+ volume'

fixture
device sda1 hfsplus
for rc in 1 2 3 4 8 47 127; do
    MOCK_FSCK_RC=$rc expect_fail bash "$STORAGE" add /dev/sda1
done
lacks 'mount ' "$MOCK_ROOT/log"
ok 'uncorrected HFS+ errors and missing checker block RW'

fixture
device sda1 hfsplus
printf '/dev/sda1|hfsplus|/elsewhere|ro\n' > "$MOCK_ROOT/mounts"
expect_fail bash "$STORAGE" add /dev/sda1
lacks 'fsck.hfsplus ' "$MOCK_ROOT/log"
ok 'already mounted volume is never checked or claimed'

fixture
sd_device
run_storage add /dev/mmcblk1p1
for dir in 'Engine Library' Downloads apfs-overlays; do
    [[ -d $SC5000_MEDIA_ROOT/sc5000-sd-cache/$dir ]] || fail "missing $dir"
done
device mmcblk0p1 ext4
mkdir -p "$SC5000_SYS_ROOT/class/block/mmcblk0/device"
printf 'MMC\n' > "$SC5000_SYS_ROOT/class/block/mmcblk0/device/type"
run_storage add /dev/mmcblk0p1
lacks '</dev/mmcblk0p1>' "$MOCK_ROOT/log"
ok 'front SD cache structure; internal eMMC is excluded'

fixture
sd_device
run_storage add /dev/mmcblk1p1
device sdb1 apfs
MOCK_PDB=1 run_storage add /dev/sdb1
has 'mount <-t> <apfs> <-o> <ro,nodev,nosuid> </dev/sdb1>' "$MOCK_ROOT/log"
has "upperdir=$SC5000_MEDIA_ROOT/sc5000-sd-cache/apfs-overlays/AABB-1234/upper" "$MOCK_ROOT/log"
has "workdir=$SC5000_MEDIA_ROOT/sc5000-sd-cache/apfs-overlays/AABB-1234/work" "$MOCK_ROOT/log"
has 'modprobe <apfs>' "$MOCK_ROOT/log"
has 'rb-bridge <--mount-path>' "$MOCK_ROOT/log"
bridge_line=$(grep -n '^rb-bridge ' "$MOCK_ROOT/log" | cut -d: -f1)
notify_line=$(grep -n '^notify ' "$MOCK_ROOT/log" | tail -n1 | cut -d: -f1)
[[ $bridge_line -lt $notify_line ]] || fail 'bridge must precede Engine notification'
has "<$SC5000_MEDIA_ROOT/sc5000-sdb1/Engine Library>" "$MOCK_ROOT/log"
touch "$SC5000_MEDIA_ROOT/sc5000-sd-cache/apfs-overlays/AABB-1234/upper/cues.db"
before=$(grep -c '^mount ' "$MOCK_ROOT/log")
run_storage add /dev/sdb1
[[ $(grep -c '^mount ' "$MOCK_ROOT/log") == "$before" ]] || fail 'duplicate mount'
expect_fail bash "$STORAGE" remove /dev/mmcblk1p1
device sdc1 apfs
expect_fail bash "$STORAGE" add /dev/sdc1
MOCK_BUSY_TARGET=$SC5000_MEDIA_ROOT/sc5000-sdb1 expect_fail bash "$STORAGE" remove /dev/sdb1
[[ -f $SC5000_RUN_ROOT/sdb1.state ]] || fail 'busy mount lost state'
has 'overlay' "$MOCK_ROOT/mounts"
has "$SC5000_MEDIA_ROOT/sc5000-sdb1/Engine Library" "$MOCK_ROOT/mounts"
MOCK_BUSY_TARGET=$SC5000_MEDIA_ROOT/sc5000-sdb1/Engine\ Library expect_fail bash "$STORAGE" remove /dev/sdb1
[[ -f $SC5000_RUN_ROOT/sdb1.state ]] || fail 'busy library lost state'
run_storage remove /dev/sdb1
run_storage add /dev/sdb1
[[ -f $SC5000_MEDIA_ROOT/sc5000-sd-cache/apfs-overlays/AABB-1234/upper/cues.db ]] || fail 'cache not persistent'
run_storage remove /dev/sdb1
run_storage remove /dev/mmcblk1p1
[[ ! -s $MOCK_ROOT/mounts ]] || fail 'unmount leaked mounts'
ok 'APFS RO + SD overlay, bridge ordering, duplicate/busy safety and persistence'

fixture
device sda1 apfs
run_storage add /dev/sda1
has "upperdir=$SC5000_INTERNAL_CACHE/apfs-overlays/AABB-1234/upper" "$MOCK_ROOT/log"
lacks 'fsck.hfsplus ' "$MOCK_ROOT/log"
ok 'APFS without SD uses internal cache and never invokes HFS tools'

fixture
device sda1 apfs
MOCK_APFS_LIBRARY=1 run_storage add /dev/sda1
has "lowerdir=$SC5000_RUN_ROOT/lower/sda1/Engine Library,upperdir=" "$MOCK_ROOT/log"
umount -- "$SC5000_MEDIA_ROOT/sc5000-sda1/Engine Library"
run_storage add /dev/sda1
has "$SC5000_MEDIA_ROOT/sc5000-sda1/Engine Library" "$MOCK_ROOT/mounts"
: > "$MOCK_ROOT/log"
run_storage remove /dev/sda1
expected=$(printf 'umount <--> <%s>\numount <--> <%s>\numount <--> <%s>' \
    "$SC5000_MEDIA_ROOT/sc5000-sda1/Engine Library" "$SC5000_MEDIA_ROOT/sc5000-sda1" "$SC5000_RUN_ROOT/lower/sda1")
[[ $(grep '^umount ' "$MOCK_ROOT/log") == "$expected" ]] || fail 'APFS unmount order'
ok 'existing APFS library is the dedicated lower; missing overlay recovers; ordered unmount'

fixture
sd_device vfat
printf 'vfat\n' > "$MOCK_ROOT/sd-type"
run_storage add /dev/mmcblk1p1
device sda1 apfs
run_storage add /dev/sda1
has "upperdir=$SC5000_INTERNAL_CACHE/apfs-overlays/AABB-1234/upper" "$MOCK_ROOT/log"
ok 'FAT SD remains usable for downloads; APFS upper falls back to Unix filesystem'

fixture
device sda1 apfs
run_storage add /dev/sda1
touch "$SC5000_INTERNAL_CACHE/apfs-overlays/AABB-1234/upper/cues.db"
run_storage remove /dev/sda1
sd_device
run_storage add /dev/mmcblk1p1
run_storage add /dev/sda1
[[ $(sed -n '3p' "$SC5000_RUN_ROOT/sda1.state") == "$SC5000_INTERNAL_CACHE/apfs-overlays/AABB-1234" ]] || fail 'new SD hid existing internal cues'
ok 'later SD insertion retains an existing internal overlay library'

fixture
device sda1 apfs
printf 'vfat\n' > "$MOCK_ROOT/internal-type"
expect_fail bash "$STORAGE" add /dev/sda1
lacks 'mount ' "$MOCK_ROOT/log"
ok 'APFS fails closed without a compatible writable upper filesystem'

fixture
device sda1 apfs
MOCK_MOUNT_FAIL=overlay expect_fail bash "$STORAGE" add /dev/sda1
[[ ! -s $MOCK_ROOT/mounts && ! -f $SC5000_RUN_ROOT/sda1.state ]] || fail 'failed overlay leaked lower mount/state'
MOCK_MOD_FAIL=apfs expect_fail bash "$STORAGE" add /dev/sda1
[[ ! -s $MOCK_ROOT/mounts ]] || fail 'failed module load leaked mount'
MOCK_MOUNT_FAIL_TARGET=$SC5000_MEDIA_ROOT/sc5000-sda1/Engine\ Library expect_fail bash "$STORAGE" add /dev/sda1
[[ ! -s $MOCK_ROOT/mounts && ! -f $SC5000_RUN_ROOT/sda1.state ]] || fail 'failed library overlay leaked root/lower mounts'
ok 'overlay failure rolls back RO lower; module failure exposes no mount'

fixture
device sda1 apfs '../bad'
expect_fail bash "$STORAGE" add /dev/sda1
lacks 'mount ' "$MOCK_ROOT/log"
printf 'AABB-1234\n' > "$MOCK_ROOT/uuid/sda1"
mkdir -p "$SC5000_INTERNAL_CACHE/apfs-overlays"
ln -s "$MOCK_ROOT/outside" "$SC5000_INTERNAL_CACHE/apfs-overlays/AABB-1234"
expect_fail bash "$STORAGE" add /dev/sda1
[[ ! -d $MOCK_ROOT/outside/upper ]] || fail 'cache symlink followed'
ok 'UUID traversal and removable-cache symlinks rejected'

fixture
device sda1 hfsplus
MOCK_LIB_SYMLINK=1 expect_fail bash "$STORAGE" add /dev/sda1
[[ ! -s $MOCK_ROOT/mounts ]] || fail 'unsafe library mount leaked'
ok 'library symlink fails safely and unmounts'

fixture
device sda1 hfsplus
MOCK_PDB=1 MOCK_BRIDGE_RC=1 run_storage add /dev/sda1
has 'notify <--mount-path>' "$MOCK_ROOT/log"
[[ -f $SC5000_RUN_ROOT/sda1.state ]] || fail 'bridge failure removed readable files'
ok 'bridge failure preserves original files and still notifies Engine'

fixture
device sda1 hfsplus
MOCK_PDB=1 MOCK_BRIDGE_SLEEP=2 SC5000_BRIDGE_TIMEOUT=0.05s run_storage add /dev/sda1
has 'timed out' "$MOCK_ROOT/output"
has 'notify <--mount-path>' "$MOCK_ROOT/log"
ok 'hung bridge is bounded and does not block original-file notification'

fixture
mkdir -p "$SC5000_SYS_ROOT/class/block/"{mmcblk0boot0,mmcblk0boot1,mmcblk0rpmb}
run_storage --discover
lacks 'blkid ' "$MOCK_ROOT/log"
ok 'coldplug ignores internal eMMC boot and RPMB auxiliary nodes'

fixture
sd_device
device sda1 apfs
bash "$STORAGE" --discover > "$MOCK_ROOT/output" 2>&1
has "upperdir=$SC5000_MEDIA_ROOT/sc5000-sd-cache/apfs-overlays/AABB-1234/upper" "$MOCK_ROOT/log"
ok 'coldplug discovery mounts SD before APFS'

fixture
device sda1 crypto_LUKS
run_storage add /dev/sda1
lacks 'mount ' "$MOCK_ROOT/log"
expect_fail bash "$STORAGE" add /dev/../etc/passwd
expect_fail bash "$STORAGE" add /dev/sdz1
ok 'unsupported, malformed and disappeared devices are handled safely'

fixture
for type in vfat exfat ntfs; do
    device sda1 "$type"
    run_storage add /dev/sda1
    has 'uid=1000,gid=1000,umask=0000' "$MOCK_ROOT/log"
    run_storage remove /dev/sda1
done
has 'mount <-t> <ntfs3>' "$MOCK_ROOT/log"
ok 'FAT/exFAT are writable; NTFS explicitly selects the NTFS3 kernel driver'

fixture
sd_device apfs
run_storage add /dev/mmcblk1p1
has "<$SC5000_MEDIA_ROOT/sc5000-mmcblk1p1/Engine Library>" "$MOCK_ROOT/log"
has "upperdir=$SC5000_INTERNAL_CACHE/apfs-overlays/SD-1234/upper" "$MOCK_ROOT/log"
ok 'APFS SD is protected as an external volume rather than a writable cache'

for export_name in EXPORT.PDB exportLibrary.db exportOneLibrary.db; do
    fixture
    device sda1 exfat
    MOCK_PDB=1 MOCK_ENGINE_DB=1 MOCK_EXPORT_DIR=pIoNeEr/ReKoRdBoX MOCK_EXPORT_NAME=$export_name run_storage add /dev/sda1
    has 'rb-bridge <--mount-path>' "$MOCK_ROOT/log"
    has 'engine-guardian <--heal-mount>' "$MOCK_ROOT/log"
    has "taskset <-c> <3> <$TMP/bin/engine-guardian>" "$MOCK_ROOT/log"
    bridge_line=$(grep -n '^rb-bridge ' "$MOCK_ROOT/log" | cut -d: -f1)
    heal_line=$(grep -n '^engine-guardian ' "$MOCK_ROOT/log" | cut -d: -f1)
    notify_line=$(grep -n '^notify ' "$MOCK_ROOT/log" | cut -d: -f1)
    ((bridge_line < heal_line && heal_line < notify_line)) || fail 'bridge/healer/notify ordering'
    ok "$export_name converted and healed before notification"
done
fixture
device sda1 exfat
MOCK_PDB=1 MOCK_ENGINE_DB=1 MOCK_GUARDIAN_SLEEP=2 SC5000_GUARDIAN_TIMEOUT=0.05s run_storage add /dev/sda1
has 'notify <--mount-path>' "$MOCK_ROOT/log"
ok 'guardian timeout never blocks notification'

fixture
export SC5000_MEDIA_ROOT="$MOCK_ROOT/media[1]"
device sda1 exfat
MOCK_PDB=1 run_storage add /dev/sda1
has 'rb-bridge <--mount-path>' "$MOCK_ROOT/log"
ok 'export detection treats media root glob characters as literal path bytes'

fixture
export SC5000_MEDIA_ROOT="$MOCK_ROOT/pioneer/rekordbox/export-root"
device sda1 exfat
MOCK_FAKE_EXPORT=1 run_storage add /dev/sda1
lacks 'rb-bridge ' "$MOCK_ROOT/log"
ok 'parent directory names cannot make unrelated files look like a Rekordbox export'



fixture
mkdir -p "$SC5000_PROC_ROOT/sys/"{vm,fs}
for knob in vm/swappiness vm/dirty_background_ratio vm/dirty_ratio fs/file-max; do
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
printf 'simple_ondemand\n' > "$gpu/governor"
printf '600000000\n' > "$gpu/max_freq"
printf '100000000\n' > "$gpu/min_freq"
for dev in sda mmcblk1; do mkdir -p "$SC5000_SYS_ROOT/block/$dev/queue"; done
printf '[none] mq-deadline kyber\n' > "$SC5000_SYS_ROOT/block/sda/queue/scheduler"
printf '[none]\n' > "$SC5000_SYS_ROOT/block/mmcblk1/queue/scheduler"
printf ' CPU0 CPU1 CPU2 CPU3\n 42: 1 0 0 0 GIC i2s-audio\n 43: 0 1 0 0 GIC ehci\n' > "$SC5000_PROC_ROOT/interrupts"
printf ' 44: 0 1 0 0 GIC ffa30000.gpu\n 45: 0 1 0 0 GIC ehci_hcd\n 46: 0 1 0 0 GIC ffb20000\n' >> "$SC5000_PROC_ROOT/interrupts"
printf ' 144: 0 1 0 0 GIC ffa30000.gpu\n 145: 0 1 0 0 GIC ff540000\n 146: 0 1 0 0 GIC ffb20000\n' >> "$SC5000_PROC_ROOT/interrupts"
for irq in 42 43 44 45 46 144 145 146; do mkdir -p "$SC5000_PROC_ROOT/irq/$irq"; printf 'f\n' > "$SC5000_PROC_ROOT/irq/$irq/smp_affinity"; done
mkdir -p "$SC5000_PROC_ROOT/100/task/"{100,101,102} "$SC5000_PROC_ROOT/200"
ln -s /usr/Engine/Engine "$SC5000_PROC_ROOT/100/exe"
printf 'Engine\n' > "$SC5000_PROC_ROOT/100/comm"
printf 'irq/42-i2s\n' > "$SC5000_PROC_ROOT/200/comm"
for tid in 100 101 102; do
    policy=0; name=GUI
    if [[ $tid == 101 ]]; then policy=1; name=AudioRT; fi
    if [[ $tid == 102 ]]; then name=AudioHelper; fi
    printf '%s\n' "$name" > "$SC5000_PROC_ROOT/100/task/$tid/comm"
    {
        printf '%s (name with spaces) S' "$tid"
        for ((field=4; field<=40; field++)); do printf ' 0'; done
        printf ' %s\n' "$policy"
    } > "$SC5000_PROC_ROOT/100/task/$tid/stat"
done
for entry in '201 144-ffa3000' '202 145-ff54000' '203 99-eth0' '204 99-other' '205 146-ffb2000'; do
    read -r pid name <<< "$entry"
    mkdir -p "$SC5000_PROC_ROOT/$pid"
    printf 'irq/%s\n' "$name" > "$SC5000_PROC_ROOT/$pid/comm"
done
bash "$PERF" > "$MOCK_ROOT/output" 2>&1
for cpu in 0 1 2 3; do
    has performance "$SC5000_SYS_ROOT/devices/system/cpu/cpu$cpu/cpufreq/scaling_governor"
    has 1800000 "$SC5000_SYS_ROOT/devices/system/cpu/cpu$cpu/cpufreq/scaling_max_freq"
done
has performance "$gpu/governor"
has 600000000 "$gpu/min_freq"
has 10 "$SC5000_PROC_ROOT/sys/vm/swappiness"
has 5 "$SC5000_PROC_ROOT/sys/vm/dirty_background_ratio"
has 1048576 "$SC5000_PROC_ROOT/sys/fs/file-max"
has mq-deadline "$SC5000_SYS_ROOT/block/sda/queue/scheduler"
has none "$SC5000_SYS_ROOT/block/mmcblk1/queue/scheduler"
has 1 "$SC5000_PROC_ROOT/irq/42/smp_affinity"
has 6 "$SC5000_PROC_ROOT/irq/43/smp_affinity"
has 'taskset <-pc> <0> <101>' "$MOCK_ROOT/log"
has 'taskset <-pc> <1-2> <100>' "$MOCK_ROOT/log"
has 'taskset <-pc> <1-2> <102>' "$MOCK_ROOT/log"
has 'taskset <-pc> <0> <200>' "$MOCK_ROOT/log"
has 'chrt <-f> <-p> <99> <200>' "$MOCK_ROOT/log"
has 2 "$SC5000_PROC_ROOT/irq/44/smp_affinity"
has 4 "$SC5000_PROC_ROOT/irq/45/smp_affinity"
has 1 "$SC5000_PROC_ROOT/irq/46/smp_affinity"
has 'taskset <-pc> <1> <201>' "$MOCK_ROOT/log"
has 'taskset <-pc> <2> <202>' "$MOCK_ROOT/log"
has 'taskset <-pc> <2> <203>' "$MOCK_ROOT/log"
has 'taskset <-pc> <1-2> <204>' "$MOCK_ROOT/log"
has 'taskset <-pc> <0> <205>' "$MOCK_ROOT/log"
has 'chrt <-f> <-p> <99> <205>' "$MOCK_ROOT/log"
has 1 "$SC5000_PROC_ROOT/irq/146/smp_affinity"
has 2 "$SC5000_PROC_ROOT/irq/144/smp_affinity"
has 4 "$SC5000_PROC_ROOT/irq/145/smp_affinity"
lacks 'taskset <-pc> <3>' "$MOCK_ROOT/log"
find "$SC5000_SYS_ROOT" "$SC5000_PROC_ROOT" -type f -exec sha256sum {} + | sort > "$MOCK_ROOT/before"
bash "$PERF" > "$MOCK_ROOT/output" 2>&1
find "$SC5000_SYS_ROOT" "$SC5000_PROC_ROOT" -type f -exec sha256sum {} + | sort > "$MOCK_ROOT/after"
cmp -s "$MOCK_ROOT/before" "$MOCK_ROOT/after" || fail 'performance tuning is not idempotent'
ok 'CPU/GPU, sysctl, schedulers, IRQs and RT-vs-GUI affinity are idempotent'

MOCK_TASKSET_FAIL=1 bash "$PERF" > "$MOCK_ROOT/output" 2>&1
rm "$gpu/governor"
ln -s /dev/full "$gpu/governor"
bash "$PERF" > "$MOCK_ROOT/output" 2>&1
ok 'thread exit/affinity failure and disappearing sysfs knobs are nonfatal'

fixture
bash "$PERF" > "$MOCK_ROOT/output" 2>&1
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
lacks ', RUN+=' "$ROOT/rootfs-overlay/etc/udev/rules.d/60-sc5000-storage.rules"
ok 'background service isolation and systemd udev handoff are installed'

fixture
expect_fail env TARGET_CONFIG= TARGET_SYMVERS= bash "$ROOT/kernel-modules/build_modules.sh" modules
has 'TARGET_CONFIG' "$MOCK_ROOT/output"
ok 'module build refuses missing target ABI inputs before building'
python3 -m unittest discover -s "$ROOT/sim/qengine" -p test_smoke.py >/dev/null
printf 'PASS: %d tests (mocked storage and hardware; no privileged operations)\n' "$count"
