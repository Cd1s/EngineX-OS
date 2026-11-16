#!/bin/bash
# Re-runnable ARMv7 HIGHMEM QEMU stress harness for hfsplusx.ko vs built-in hfsplus.
set -euo pipefail
HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
CROSS_COMPILE=${CROSS_COMPILE:-arm-linux-gnueabihf-}
ROUNDS=${ROUNDS:-6}
JOBS=${JOBS:-$(getconf _NPROCESSORS_ONLN)}
FSCK_HFSPLUS=${FSCK_HFSPLUS:-$HERE/out/sbin/fsck.hfsplus}
SKIP_STOCK_TESTS=${SKIP_STOCK_TESTS:-0}
KEEP_WORKDIR=${KEEP_WORKDIR:-0}
QEMU_TIMEOUT=${QEMU_TIMEOUT:-240}

die() { printf 'test_hfsplusx_qemu: %s\n' "$*" >&2; exit 1; }
usage() {
    cat <<'EOF'
Usage: test_hfsplusx_qemu.sh [--help]

Reproduces the 32-bit ARM HIGHMEM kmap_local / same-page hfs_bnode_move bugs on
stock built-in hfsplus, and verifies the patched hfsplusx.ko under multi-round
8 KiB Catalog B-tree stress + in-guest and host fsck.hfsplus -n.

Inputs (use one mode):
  1. Prebuilt artifacts:
     QEMU_ZIMAGE=/path/to/zImage HFSPLUSX_KO=/path/to/hfsplusx.ko
  2. Kernel build dir (rebuilds hfsplusx.ko from KERNEL_SRC + patches/):
     KERNEL_SRC=/path/to/linux-6.18.57-rt QEMU_KERNEL_BUILD=/path/to/out-qemu
  3. Full source build (builds QEMU virt HIGHMEM zImage + hfsplusx.ko):
     KERNEL_SRC=/path/to/linux-6.18.57-rt TARGET_CONFIG=/path/to/engine.config

Optional: FSCK_HFSPLUS (static ARMv7 fsck.hfsplus, default out/sbin/fsck.hfsplus),
CROSS_COMPILE (default arm-linux-gnueabihf-), ROUNDS (default 6), WORKDIR,
KEEP_WORKDIR=1, SKIP_STOCK_TESTS=1, QEMU_TIMEOUT (default 240).
EOF
}

case ${1:-} in
    --help|-h) usage; exit 0 ;;
    "") ;;
    *) usage >&2; exit 2 ;;
esac

[[ $ROUNDS =~ ^[1-9][0-9]*$ ]] || die 'invalid ROUNDS'
[[ $JOBS =~ ^[1-9][0-9]*$ ]] || die 'invalid JOBS'
[[ $QEMU_TIMEOUT =~ ^[1-9][0-9]*$ ]] || die 'invalid QEMU_TIMEOUT'

for cmd in qemu-system-arm "${CROSS_COMPILE}gcc" "${CROSS_COMPILE}readelf" mkfs.hfsplus fsck.hfsplus cpio gzip truncate timeout; do
    command -v "$cmd" >/dev/null || die "missing host tool: $cmd"
done
[[ -x $FSCK_HFSPLUS ]] || die "missing executable static ARM fsck.hfsplus: $FSCK_HFSPLUS (run build_modules.sh tools or set FSCK_HFSPLUS)"
"${CROSS_COMPILE}readelf" -h "$FSCK_HFSPLUS" | grep -q 'Machine:.*ARM' || die "not ARM ELF: $FSCK_HFSPLUS"
! "${CROSS_COMPILE}readelf" -l "$FSCK_HFSPLUS" | grep -q 'INTERP' || die "fsck.hfsplus in initramfs must be statically linked: $FSCK_HFSPLUS"

CLEAN_WORKDIR=0
if [[ -z ${WORKDIR:-} ]]; then
    WORKDIR=$(mktemp -d "${TMPDIR:-/tmp}/hfsplusx-qemu.XXXXXX")
    [[ $KEEP_WORKDIR == 1 ]] || CLEAN_WORKDIR=1
else
    mkdir -p "$WORKDIR"
fi
WORKDIR=$(cd "$WORKDIR" && pwd)
cleanup() {
    if [[ $CLEAN_WORKDIR == 1 ]]; then
        rm -rf -- "$WORKDIR"
    fi
}
trap cleanup EXIT

QEMU_ZIMAGE=${QEMU_ZIMAGE:-}
HFSPLUSX_KO=${HFSPLUSX_KO:-}
QEMU_KERNEL_BUILD=${QEMU_KERNEL_BUILD:-}
KERNEL_SRC=${KERNEL_SRC:-}
TARGET_CONFIG=${TARGET_CONFIG:-}

if [[ -z $QEMU_ZIMAGE ]]; then
    if [[ -n $QEMU_KERNEL_BUILD && -f $QEMU_KERNEL_BUILD/arch/arm/boot/zImage ]]; then
        QEMU_ZIMAGE=$QEMU_KERNEL_BUILD/arch/arm/boot/zImage
    else
        [[ -f ${KERNEL_SRC:-}/Makefile ]] || die 'set QEMU_ZIMAGE or KERNEL_SRC'
        [[ -f ${TARGET_CONFIG:-} ]] || die 'set QEMU_ZIMAGE (or QEMU_KERNEL_BUILD) or TARGET_CONFIG'
        QEMU_KERNEL_BUILD=${QEMU_KERNEL_BUILD:-$WORKDIR/qemu-kernel}
        mkdir -p "$QEMU_KERNEL_BUILD"
        cp "$TARGET_CONFIG" "$QEMU_KERNEL_BUILD/.config"
        "$KERNEL_SRC/scripts/config" --file "$QEMU_KERNEL_BUILD/.config" \
            --enable ARCH_VIRT \
            --enable ARM_GIC_V3 \
            --enable ARM_GIC_V3_ITS \
            --enable VIRTIO \
            --enable VIRTIO_MMIO \
            --enable VIRTIO_MMIO_CMDLINE_DEVICES \
            --enable VIRTIO_BLK \
            --enable SERIAL_AMBA_PL011 \
            --enable SERIAL_AMBA_PL011_CONSOLE \
            --enable HIGHMEM \
            --enable KMAP_LOCAL \
            --enable KMAP_LOCAL_NON_LINEAR_PTE_ARRAY \
            --enable HFSPLUS_FS \
            --enable PANIC_ON_OOPS \
            --enable BLK_DEV_INITRD \
            --enable DEVTMPFS \
            --enable RD_GZIP \
            --enable MODULES
        make -C "$KERNEL_SRC" O="$QEMU_KERNEL_BUILD" ARCH=arm "CROSS_COMPILE=$CROSS_COMPILE" olddefconfig
        for opt in CONFIG_HIGHMEM CONFIG_KMAP_LOCAL CONFIG_HFSPLUS_FS CONFIG_PANIC_ON_OOPS CONFIG_VIRTIO_BLK CONFIG_SERIAL_AMBA_PL011; do
            grep -qx "$opt=y" "$QEMU_KERNEL_BUILD/.config" || die "QEMU config missing $opt=y"
        done
        make -C "$KERNEL_SRC" O="$QEMU_KERNEL_BUILD" ARCH=arm "CROSS_COMPILE=$CROSS_COMPILE" -j"$JOBS" zImage modules
        QEMU_ZIMAGE=$QEMU_KERNEL_BUILD/arch/arm/boot/zImage
    fi
fi

if [[ -z $HFSPLUSX_KO ]]; then
    [[ -f ${KERNEL_SRC:-}/Makefile ]] || die 'set HFSPLUSX_KO or KERNEL_SRC'
    [[ -s ${QEMU_KERNEL_BUILD:-}/Module.symvers ]] || die 'set HFSPLUSX_KO or QEMU_KERNEL_BUILD with Module.symvers'
    hfs_mod=$WORKDIR/hfsplusx-mod
    rm -rf "$hfs_mod"
    mkdir -p "$hfs_mod"
    cp -a "$KERNEL_SRC/fs/hfsplus/." "$hfs_mod/"
    hfs_patches=("$HERE/patches"/hfsplusx-*.patch)
    [[ -f ${hfs_patches[0]:-} ]] || die 'missing hfsplusx patches'
    for patch in "${hfs_patches[@]}"; do
        patch -d "$hfs_mod" -p1 < "$patch"
    done
    make -C "$KERNEL_SRC" O="$QEMU_KERNEL_BUILD" ARCH=arm "CROSS_COMPILE=$CROSS_COMPILE" -j"$JOBS" "M=$hfs_mod" modules
    HFSPLUSX_KO=$hfs_mod/hfsplusx.ko
fi

[[ -f $QEMU_ZIMAGE ]] || die "missing QEMU_ZIMAGE: $QEMU_ZIMAGE"
[[ -f $HFSPLUSX_KO ]] || die "missing HFSPLUSX_KO: $HFSPLUSX_KO"

TDIR=$WORKDIR/run
RDIR=$TDIR/rootfs
LOG=$TDIR/prep.log
rm -rf "$TDIR"
mkdir -p "$RDIR/sbin" "$RDIR/dev" "$RDIR/proc" "$RDIR/sys" "$RDIR/mnt"
: > "$LOG"

cat > "$TDIR/init.c" <<'EOF'
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mount.h>
#include <sys/reboot.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <sys/wait.h>
#include <unistd.h>

static void die(const char *msg) {
    printf("FAIL: %s (errno=%d %s)\n", msg, errno, strerror(errno));
    fflush(stdout);
    sync();
    reboot(RB_POWER_OFF);
    for (;;) pause();
}

static void show_zones(void) {
    FILE *f = fopen("/proc/zoneinfo", "r");
    if (!f) return;
    char line[256];
    int in_zone = 0;
    printf("--- /proc/zoneinfo summary ---\n");
    while (fgets(line, sizeof(line), f)) {
        if (strncmp(line, "Node ", 5) == 0) {
            printf("%s", line);
            in_zone = 1;
        } else if (in_zone && (strstr(line, "pages free") || strstr(line, "managed") || strstr(line, "spanned"))) {
            printf("%s", line);
            if (strstr(line, "managed")) in_zone = 0;
        }
    }
    fclose(f);
    fflush(stdout);
}

static int parse_rounds(const char *cmdline) {
    const char *p = strstr(cmdline, "hfs_rounds=");
    if (!p) return 2;
    int v = atoi(p + 11);
    return (v > 0 && v <= 50) ? v : 2;
}

static int run_cmd(char *const argv[]) {
    fflush(stdout);
    pid_t pid = fork();
    if (pid == 0) { execv(argv[0], argv); _exit(127); }
    int st = 0;
    if (waitpid(pid, &st, 0) < 0) return -1;
    return WIFEXITED(st) ? WEXITSTATUS(st) : -1;
}

int main(void) {
    mount("devtmpfs", "/dev", "devtmpfs", 0, NULL);
    mount("proc", "/proc", "proc", 0, NULL);
    mount("sysfs", "/sys", "sysfs", 0, NULL);

    char cmdline[1024] = {0};
    int cfd = open("/proc/cmdline", O_RDONLY);
    if (cfd >= 0) { ssize_t r = read(cfd, cmdline, sizeof(cmdline) - 1); (void)r; close(cfd); }
    int rounds = parse_rounds(cmdline);
    int mode_fix = strstr(cmdline, "hfs_mode=fix") != NULL;
    int mode_prepop = strstr(cmdline, "hfs_mode=prepop ") != NULL || strstr(cmdline, "hfs_mode=prepop\n") != NULL;
    int mode_trigger = strstr(cmdline, "hfs_mode=trigger") != NULL;
    const char *fstype = (mode_fix || mode_prepop) ? "hfsplusx" : "hfsplus";
    printf("QEMU_BOOT fstype=%s rounds=%d cmdline=%s\n", fstype, rounds, cmdline);
    show_zones();

    if (mode_fix || mode_prepop) {
        int mfd = open("/hfsplusx.ko", O_RDONLY);
        if (mfd < 0) die("open /hfsplusx.ko");
        if (syscall(SYS_finit_module, mfd, "", 0) != 0) die("finit_module hfsplusx.ko");
        close(mfd);
        printf("LOADED_HFSPLUSX_KO\n");
    }

    if (mount("/dev/vda", "/mnt", fstype, 0, "rw") != 0) die("mount /dev/vda");
    printf("MOUNTED_OK fstype=%s\n", fstype);
    fflush(stdout);

    char path[256], path2[512];
    long total_ops = 0;
    if (mode_prepop) {
        if (mkdir("/mnt/prepop", 0755) != 0) die("mkdir prepop");
        for (int i = 260; i >= 251; i--) {
            snprintf(path, sizeof(path), "/mnt/prepop/catalog_entry_reverse_%04d_xxxxxxxx", i);
            if (mkdir(path, 0755) != 0) die("mkdir prepop child");
            snprintf(path2, sizeof(path2), "%s/file_%04d.bin", path, i);
            int fd = open(path2, O_CREAT | O_WRONLY | O_TRUNC, 0644);
            if (fd < 0) die("create prepop file");
            if (write(fd, "enginex-hfs-prepop", 18) != 18) die("write prepop file");
            close(fd);
            total_ops += 2;
        }
        printf("PREPOP_DONE ops=%ld\n", total_ops);
    } else if (mode_trigger) {
        printf("TRIGGER_START\n");
        fflush(stdout);
        for (int i = 250; i >= 230; i--) {
            snprintf(path, sizeof(path), "/mnt/prepop/catalog_entry_reverse_%04d_xxxxxxxx", i);
            printf("TRIGGER_MKDIR i=%d\n", i);
            fflush(stdout);
            if (mkdir(path, 0755) != 0) die("mkdir trigger child");
            snprintf(path2, sizeof(path2), "%s/file_%04d.bin", path, i);
            int fd = open(path2, O_CREAT | O_WRONLY | O_TRUNC, 0644);
            if (fd < 0) die("create trigger file");
            if (write(fd, "enginex-hfs-trigger", 19) != 19) die("write trigger file");
            close(fd);
            total_ops += 2;
        }
        printf("TRIGGER_NO_CRASH ops=%ld\n", total_ops);
    } else {
        for (int r = 0; r < rounds; r++) {
            snprintf(path, sizeof(path), "/mnt/top_%03d", 999 - r);
            if (mkdir(path, 0755) != 0) die("mkdir top");
            total_ops++;
            for (int i = 260; i >= 0; i--) {
                snprintf(path, sizeof(path), "/mnt/top_%03d/catalog_entry_reverse_%04d_xxxxxxxx", 999 - r, i);
                if (mkdir(path, 0755) != 0) die("mkdir child");
                total_ops++;
                snprintf(path2, sizeof(path2), "%s/file_%04d.bin", path, i);
                int fd = open(path2, O_CREAT | O_WRONLY | O_TRUNC, 0644);
                if (fd < 0) die("create file");
                if (write(fd, "enginex-hfs-stress", 18) != 18) die("write file");
                close(fd);
                total_ops++;
                if (i == 260 || i == 200 || i == 100 || i == 0) {
                    printf("PROGRESS r=%d i=%d ops=%ld\n", r, i, total_ops);
                    fflush(stdout);
                }
            }
            if (r >= 1) {
                for (int j = 0; j < 120; j++) {
                    snprintf(path, sizeof(path), "/mnt/top_%03d/catalog_entry_reverse_%04d_xxxxxxxx",
                             999 - (r - 1), j * 2);
                    snprintf(path2, sizeof(path2), "/mnt/top_%03d/moved_entry_%04d_yyyyyyyy",
                             999 - r, 500 - j);
                    if (rename(path, path2) != 0) die("rename");
                    total_ops++;
                }
                for (int j = 0; j < 60; j++) {
                    snprintf(path, sizeof(path), "/mnt/top_%03d/moved_entry_%04d_yyyyyyyy/file_%04d.bin",
                             999 - r, 500 - j, j * 2);
                    if (unlink(path) != 0) die("unlink");
                    total_ops++;
                    snprintf(path, sizeof(path), "/mnt/top_%03d/moved_entry_%04d_yyyyyyyy",
                             999 - r, 500 - j);
                    if (rmdir(path) != 0) die("rmdir");
                    total_ops++;
                }
            }
            printf("ROUND_DONE r=%d/%d ops=%ld\n", r + 1, rounds, total_ops);
            fflush(stdout);
        }
    }

    sync();
    if (umount("/mnt") != 0) die("umount /mnt");
    printf("UMOUNT_OK fstype=%s total_ops=%ld\n", fstype, total_ops);
    fflush(stdout);

    char *fsck_argv[] = { "/sbin/fsck.hfsplus", "-n", "/dev/vda", NULL };
    int frc = run_cmd(fsck_argv);
    printf("FSCK_RC=%d\n", frc);
    if (frc != 0) die("fsck.hfsplus -n failed");

    printf("QEMU_HFS_TEST_PASS fstype=%s rounds=%d total_ops=%ld\n", fstype, rounds, total_ops);
    fflush(stdout);
    sync();
    reboot(RB_POWER_OFF);
    for (;;) pause();
    return 0;
}
EOF

"${CROSS_COMPILE}gcc" -static -O2 -Wall -Wextra -o "$RDIR/init" "$TDIR/init.c"
install -m 0644 "$HFSPLUSX_KO" "$RDIR/hfsplusx.ko"
install -m 0755 "$FSCK_HFSPLUS" "$RDIR/sbin/fsck.hfsplus"
(cd "$RDIR" && find . -print0 | cpio --null -o --format=newc --quiet | gzip -9 > "$TDIR/initramfs.cpio.gz")

make_img() {
    local img=$1 label=$2
    rm -f "$img"
    truncate -s 128M "$img"
    mkfs.hfsplus -v "$label" -n c=8192 "$img" >> "$LOG" 2>&1
    fsck.hfsplus -n "$img" >> "$LOG" 2>&1
}

run_qemu() {
    local img=$1 mode=$2 rounds=$3 out=$4 tmo=$5
    timeout "$tmo" qemu-system-arm \
        -M virt -cpu cortex-a15 -m 2048M -smp 2 \
        -nographic -no-reboot -monitor none \
        -kernel "$QEMU_ZIMAGE" \
        -initrd "$TDIR/initramfs.cpio.gz" \
        -drive "file=$img,if=none,format=raw,id=hd0" \
        -device virtio-blk-device,drive=hd0 \
        -append "console=ttyAMA0,115200 panic=1 hfs_mode=$mode hfs_rounds=$rounds" \
        > "$out" 2>&1 || true
}

if [[ $SKIP_STOCK_TESTS != 1 ]]; then
    make_img "$TDIR/disk-stock-prepop.img" QEMUPRE
    run_qemu "$TDIR/disk-stock-prepop.img" prepop 1 "$TDIR/prepop.log" 120
    grep -q 'HighMem' "$TDIR/prepop.log" || die 'HighMem zone missing in QEMU guest'
    grep -q 'QEMU_HFS_TEST_PASS fstype=hfsplusx' "$TDIR/prepop.log" || die "prepop failed (see $TDIR/prepop.log)"

    run_qemu "$TDIR/disk-stock-prepop.img" trigger 1 "$TDIR/stock-trigger.log" 120
    ! grep -q 'TRIGGER_NO_CRASH' "$TDIR/stock-trigger.log" || die 'stock hfsplus unexpectedly survived >4096B node write'
    grep -Eq 'kunmap_local_indexed|kernel BUG at mm/highmem\.c|Kernel panic' "$TDIR/stock-trigger.log" \
        || die "stock trigger did not produce expected HIGHMEM BUG/panic (see $TDIR/stock-trigger.log)"
    printf 'PASS: stock hfsplus reproduced HIGHMEM BUG on >4096B 8 KiB Catalog node\n'

    make_img "$TDIR/disk-stock.img" QEMUSTOCK
    run_qemu "$TDIR/disk-stock.img" stock "$ROUNDS" "$TDIR/stock.log" "$QEMU_TIMEOUT"
    stock_fsck_rc=0
    fsck.hfsplus -n "$TDIR/disk-stock.img" >> "$TDIR/stock-host-fsck.log" 2>&1 || stock_fsck_rc=$?
    if grep -q 'QEMU_HFS_TEST_PASS' "$TDIR/stock.log" && [[ $stock_fsck_rc -eq 0 ]]; then
        die 'stock hfsplus unexpectedly passed fresh-image stress'
    fi
    printf 'PASS: stock hfsplus failed fresh-image stress as expected (host fsck rc=%d)\n' "$stock_fsck_rc"
fi

make_img "$TDIR/disk-fix.img" QEMUFIX
run_qemu "$TDIR/disk-fix.img" fix "$ROUNDS" "$TDIR/fix.log" "$QEMU_TIMEOUT"
grep -q 'HighMem' "$TDIR/fix.log" || die 'HighMem zone missing in fix guest'
grep -q 'LOADED_HFSPLUSX_KO' "$TDIR/fix.log" || die "hfsplusx.ko failed to load (see $TDIR/fix.log)"
grep -q "ROUND_DONE r=$ROUNDS/$ROUNDS" "$TDIR/fix.log" || die "hfsplusx stress incomplete (see $TDIR/fix.log)"
grep -q 'FSCK_RC=0' "$TDIR/fix.log" || die "guest fsck failed (see $TDIR/fix.log)"
grep -q "QEMU_HFS_TEST_PASS fstype=hfsplusx rounds=$ROUNDS " "$TDIR/fix.log" || die "fix pass marker missing (see $TDIR/fix.log)"
if grep -E 'BUG:|WARNING:|Oops:|Kernel panic|keylen .* too large|FAIL:' "$TDIR/fix.log"; then
    die "kernel error/warning detected in $TDIR/fix.log"
fi
fsck.hfsplus -n "$TDIR/disk-fix.img" > "$TDIR/fix-host-fsck.log" 2>&1 || die "host fsck failed (see $TDIR/fix-host-fsck.log)"

pass_line=$(grep 'QEMU_HFS_TEST_PASS' "$TDIR/fix.log" | tail -n 1 | tr -d '\r')
printf 'PASS: %s (host fsck rc=0)\n' "$pass_line"
