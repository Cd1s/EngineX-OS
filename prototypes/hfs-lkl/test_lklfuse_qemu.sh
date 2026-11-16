#!/bin/bash
# Re-runnable ARMv7 HIGHMEM + FUSE QEMU test and benchmark harness for lklfuse (patched hfsplus).
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
CROSS_COMPILE=${CROSS_COMPILE:-arm-linux-gnueabihf-}
LKLFUSE=${LKLFUSE:-$HERE/out/bin/lklfuse}
QEMU_ZIMAGE=${QEMU_ZIMAGE:-/var/tmp/hfs-rw-gen1/out-qemu/arch/arm/boot/zImage}
FSCK_HFSPLUS=${FSCK_HFSPLUS:-/var/tmp/hfs-rw-gen1/qemu-test/rootfs/sbin/fsck.hfsplus}
WORKDIR=${WORKDIR:-/var/tmp/hfs-lkl-qemu}
ROUNDS=${ROUNDS:-6}
QEMU_TIMEOUT=${QEMU_TIMEOUT:-360}
KEEP_WORKDIR=${KEEP_WORKDIR:-1}

die() { printf 'test_lklfuse_qemu: %s\n' "$*" >&2; exit 1; }

[[ -f $LKLFUSE ]] || die "missing LKLFUSE: $LKLFUSE"
[[ -f $QEMU_ZIMAGE ]] || die "missing QEMU_ZIMAGE: $QEMU_ZIMAGE"
[[ -f $FSCK_HFSPLUS ]] || die "missing FSCK_HFSPLUS: $FSCK_HFSPLUS"
command -v qemu-system-arm >/dev/null 2>&1 || die "missing qemu-system-arm"
command -v "${CROSS_COMPILE}gcc" >/dev/null 2>&1 || die "missing ${CROSS_COMPILE}gcc"
command -v mkfs.hfsplus >/dev/null 2>&1 || die "missing mkfs.hfsplus"
command -v fsck.hfsplus >/dev/null 2>&1 || die "missing host fsck.hfsplus"

SYSROOT_LIB=${SYSROOT_LIB:-/usr/arm-linux-gnueabihf/lib}
for lib in ld-linux-armhf.so.3 libc.so.6 libgcc_s.so.1; do
    [[ -e $SYSROOT_LIB/$lib ]] || die "missing $SYSROOT_LIB/$lib"
done

TDIR=$WORKDIR/run
RDIR=$TDIR/rootfs
LOG=$TDIR/prep.log
rm -rf "$TDIR"
mkdir -p "$RDIR/sbin" "$RDIR/usr/bin" "$RDIR/lib" "$RDIR/dev" "$RDIR/proc" "$RDIR/sys" "$RDIR/mnt" "$RDIR/tmp"
: > "$LOG"

cat > "$TDIR/init.c" <<'INIT_C'
#define _GNU_SOURCE
#include <dirent.h>
#include <errno.h>
#include <fcntl.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mount.h>
#include <sys/reboot.h>
#include <sys/stat.h>
#include <sys/utsname.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

static void die(const char *msg) {
    printf("FAIL: %s (errno=%d %s)\n", msg, errno, strerror(errno));
    fflush(stdout);
    sync();
    reboot(RB_POWER_OFF);
    for (;;) pause();
}

static uint64_t mono_ns(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (uint64_t)ts.tv_sec * 1000000000ULL + (uint64_t)ts.tv_nsec;
}

static void drop_caches(void) {
    sync();
    int fd = open("/proc/sys/vm/drop_caches", O_WRONLY);
    if (fd < 0) die("open drop_caches");
    if (write(fd, "3\n", 2) != 2) die("write drop_caches");
    close(fd);
}

static uint64_t fnv1a_update(uint64_t h, const uint8_t *buf, size_t len) {
    for (size_t i = 0; i < len; i++) {
        h ^= buf[i];
        h *= 1099511628211ULL;
    }
    return h;
}

static void fill_pattern(uint8_t *buf, size_t len, uint64_t seed) {
    uint64_t x = seed;
    for (size_t i = 0; i < len; i++) {
        x = x * 6364136223846793005ULL + 1442695040888963407ULL;
        buf[i] = (uint8_t)(x >> 56);
    }
}

static int run_cmd(char *const argv[]) {
    fflush(stdout);
    pid_t pid = fork();
    if (pid == 0) { execv(argv[0], argv); _exit(127); }
    int st = 0;
    if (waitpid(pid, &st, 0) < 0) return -1;
    return WIFEXITED(st) ? WEXITSTATUS(st) : -1;
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
}

static void report_rss(pid_t pid, const char *tag) {
    char path[64], line[256];
    long vmsize = -1, vmrss = -1, vmhwm = -1, rssanon = -1, rssfile = -1, pss = -1;
    snprintf(path, sizeof(path), "/proc/%d/status", (int)pid);
    FILE *f = fopen(path, "r");
    if (f) {
        while (fgets(line, sizeof(line), f)) {
            sscanf(line, "VmSize: %ld kB", &vmsize);
            sscanf(line, "VmRSS: %ld kB", &vmrss);
            sscanf(line, "VmHWM: %ld kB", &vmhwm);
            sscanf(line, "RssAnon: %ld kB", &rssanon);
            sscanf(line, "RssFile: %ld kB", &rssfile);
        }
        fclose(f);
    }
    snprintf(path, sizeof(path), "/proc/%d/smaps_rollup", (int)pid);
    f = fopen(path, "r");
    if (f) {
        while (fgets(line, sizeof(line), f)) {
            sscanf(line, "Pss: %ld kB", &pss);
        }
        fclose(f);
    }
    printf("RSS[%s] pid=%d VmSize=%ldkB VmRSS=%ldkB VmHWM=%ldkB RssAnon=%ldkB RssFile=%ldkB Pss=%ldkB\n",
           tag, (int)pid, vmsize, vmrss, vmhwm, rssanon, rssfile, pss);
    fflush(stdout);
}

static pid_t mount_lklfuse(const char *opts) {
    struct stat st_before, st_now;
    if (stat("/mnt", &st_before) != 0) die("stat /mnt before");
    pid_t pid = fork();
    if (pid < 0) die("fork lklfuse");
    if (pid == 0) {
        char *argv[] = { "/usr/bin/lklfuse", "/dev/vda", "/mnt", "-f", "-s", "-o", (char *)opts, NULL };
        execv(argv[0], argv);
        _exit(127);
    }
    for (int i = 0; i < 400; i++) {
        if (stat("/mnt", &st_now) == 0 && st_now.st_dev != st_before.st_dev) {
            return pid;
        }
        int st = 0;
        if (waitpid(pid, &st, WNOHANG) > 0) {
            printf("lklfuse exited early st=%d\n", st);
            die("lklfuse mount failed");
        }
        usleep(20000);
    }
    die("lklfuse mount timeout");
    return -1;
}

static void clean_eject_lklfuse(pid_t pid, const char *tag) {
    uint64_t t0 = mono_ns();
    if (umount("/mnt") != 0) die("umount /mnt");
    uint64_t t1 = mono_ns();
    int st = 0;
    pid_t w_now = waitpid(pid, &st, WNOHANG);
    int alive_after_umount = (w_now == 0) ? 1 : 0;
    if (w_now == 0) {
        if (waitpid(pid, &st, 0) < 0) die("waitpid lklfuse");
    } else if (w_now < 0) {
        die("waitpid WNOHANG");
    }
    uint64_t t2 = mono_ns();
    if (!WIFEXITED(st) || WEXITSTATUS(st) != 0) {
        printf("FAIL: lklfuse exit abnormal st=0x%x\n", st);
        die("lklfuse exit status");
    }
    printf("EJECT[%s] umount_ms=%.2f block_wait_ms=%.2f total_ms=%.2f alive_after_umount=%d exit=%d\n",
           tag, (t1 - t0) / 1e6, (t2 - t1) / 1e6, (t2 - t0) / 1e6,
           alive_after_umount, WEXITSTATUS(st));
    fflush(stdout);
}

static void do_stress(int rounds) {
    char path[512], path2[600];
    long total_ops = 0;
    pid_t pid = mount_lklfuse("type=hfsplus,mb=64");
    report_rss(pid, "stress_idle");
    uint64_t t0 = mono_ns();
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
    uint64_t t1 = mono_ns();
    report_rss(pid, "stress_after");
    clean_eject_lklfuse(pid, "stress");
    char *fsck_argv[] = { "/sbin/fsck.hfsplus", "-n", "-f", "/dev/vda", NULL };
    int frc = run_cmd(fsck_argv);
    printf("STRESS_FSCK_RC=%d elapsed_ms=%.1f total_ops=%ld\n", frc, (t1 - t0) / 1e6, total_ops);
    if (frc != 0) die("fsck.hfsplus -n -f failed after stress");
    printf("QEMU_LKL_STRESS_PASS ops=%ld\n", total_ops);
}

static void do_prepop(void) {
    pid_t pid = mount_lklfuse("type=hfsplus,mb=64");
    uint8_t *buf = malloc(65536);
    if (!buf) die("malloc");
    uint64_t hash = 14695981039346656037ULL;
    int fd = open("/mnt/audio_10mb.bin", O_CREAT | O_WRONLY | O_TRUNC, 0644);
    if (fd < 0) die("create audio_10mb.bin");
    for (int i = 0; i < 160; i++) { /* 160 * 64 KiB = 10 MiB */
        fill_pattern(buf, 65536, 0x12345678ULL + (uint64_t)i);
        hash = fnv1a_update(hash, buf, 65536);
        if (write(fd, buf, 65536) != 65536) die("write audio_10mb.bin");
    }
    close(fd);
    free(buf);
    printf("PREPOP_AUDIO_HASH=0x%016llx\n", (unsigned long long)hash);

    uint8_t small[256];
    memset(small, 'A', sizeof(small));
    char path[256];
    int count = 0;
    for (int d = 0; d < 10; d++) {
        snprintf(path, sizeof(path), "/mnt/dir_%02d", d);
        if (mkdir(path, 0755) != 0) die("mkdir dir_XX");
        for (int f = 0; f < 500; f++) {
            snprintf(path, sizeof(path), "/mnt/dir_%02d/track_%04d.flac", d, f);
            int ffd = open(path, O_CREAT | O_WRONLY | O_TRUNC, 0644);
            if (ffd < 0) die("create track");
            if (write(ffd, small, sizeof(small)) != (ssize_t)sizeof(small)) die("write track");
            close(ffd);
            count++;
        }
        printf("PREPOP_FILES=%d/5000\n", count);
        fflush(stdout);
    }
    clean_eject_lklfuse(pid, "prepop");
    char *fsck_argv[] = { "/sbin/fsck.hfsplus", "-n", "-f", "/dev/vda", NULL };
    int frc = run_cmd(fsck_argv);
    printf("PREPOP_FSCK_RC=%d\n", frc);
    if (frc != 0) die("fsck prepop failed");
    printf("QEMU_LKL_PREPOP_PASS\n");
}

static void bench_audio_read(const char *label, uint64_t expected_hash) {
    uint8_t *buf = malloc(65536);
    if (!buf) die("malloc read");
    uint64_t t0 = mono_ns();
    int fd = open("/mnt/audio_10mb.bin", O_RDONLY);
    if (fd < 0) die("open audio_10mb.bin");
    size_t total = 0;
    uint64_t h = 14695981039346656037ULL;
    ssize_t n;
    while ((n = read(fd, buf, 65536)) > 0) {
        h = fnv1a_update(h, buf, (size_t)n);
        total += (size_t)n;
    }
    if (n < 0) die("read audio_10mb.bin");
    close(fd);
    uint64_t t1 = mono_ns();
    free(buf);
    if (total != 10485760ULL) die("audio size mismatch");
    if (expected_hash && h != expected_hash) die("audio hash mismatch");
    double sec = (t1 - t0) / 1e9;
    double mbps = (10.0) / sec;
    printf("BENCH_READ[%s] bytes=%zu ms=%.2f MBps=%.2f hash=0x%016llx\n",
           label, total, sec * 1000.0, mbps, (unsigned long long)h);
    fflush(stdout);
}

static void bench_dir_scan(const char *label) {
    uint64_t t0 = mono_ns();
    DIR *root = opendir("/mnt");
    if (!root) die("opendir /mnt");
    int dirs = 0, files = 0;
    struct dirent *de;
    char dpath[320];
    while ((de = readdir(root)) != NULL) {
        if (strncmp(de->d_name, "dir_", 4) != 0) continue;
        dirs++;
        snprintf(dpath, sizeof(dpath), "/mnt/%s", de->d_name);
        DIR *sub = opendir(dpath);
        if (!sub) die("opendir subdir");
        int dfd = dirfd(sub);
        struct dirent *sde;
        struct stat st;
        while ((sde = readdir(sub)) != NULL) {
            if (sde->d_name[0] == '.') continue;
            if (fstatat(dfd, sde->d_name, &st, AT_SYMLINK_NOFOLLOW) != 0) die("fstatat");
            if (S_ISREG(st.st_mode)) files++;
        }
        closedir(sub);
    }
    closedir(root);
    uint64_t t1 = mono_ns();
    if (dirs != 10 || files != 5000) {
        printf("FAIL: scan count dirs=%d files=%d\n", dirs, files);
        die("scan count mismatch");
    }
    double sec = (t1 - t0) / 1e9;
    printf("BENCH_SCAN[%s] dirs=%d files=%d ms=%.2f files_per_sec=%.1f\n",
           label, dirs, files, sec * 1000.0, files / sec);
    fflush(stdout);
}

static void do_bench(uint64_t expected_hash) {
    /* 1. In-kernel hfsplus -o ro baseline */
    drop_caches();
    if (mount("/dev/vda", "/mnt", "hfsplus", MS_RDONLY, NULL) != 0) die("mount kernel hfsplus ro");
    bench_audio_read("kernel_ro_cold", expected_hash);
    bench_audio_read("kernel_ro_warm", expected_hash);
    drop_caches();
    bench_dir_scan("kernel_ro_cold");
    bench_dir_scan("kernel_ro_warm");
    if (umount("/mnt") != 0) die("umount kernel ro");

    /* 2. lklfuse mb=64 and mb=32 */
    const char *cfgs[2][2] = {
        { "lkl_mb64", "type=hfsplus,mb=64" },
        { "lkl_mb32", "type=hfsplus,mb=32" }
    };
    for (int c = 0; c < 2; c++) {
        char tag[32];
        drop_caches();
        pid_t pid = mount_lklfuse(cfgs[c][1]);
        snprintf(tag, sizeof(tag), "%s_idle", cfgs[c][0]);
        report_rss(pid, tag);

        snprintf(tag, sizeof(tag), "%s_cold", cfgs[c][0]);
        bench_audio_read(tag, expected_hash);
        snprintf(tag, sizeof(tag), "%s_after_read", cfgs[c][0]);
        report_rss(pid, tag);
        snprintf(tag, sizeof(tag), "%s_warm", cfgs[c][0]);
        bench_audio_read(tag, expected_hash);
        clean_eject_lklfuse(pid, cfgs[c][0]);

        drop_caches();
        pid = mount_lklfuse(cfgs[c][1]);
        snprintf(tag, sizeof(tag), "%s_cold", cfgs[c][0]);
        bench_dir_scan(tag);
        snprintf(tag, sizeof(tag), "%s_after_scan", cfgs[c][0]);
        report_rss(pid, tag);
        snprintf(tag, sizeof(tag), "%s_warm", cfgs[c][0]);
        bench_dir_scan(tag);
        clean_eject_lklfuse(pid, cfgs[c][0]);
    }
    printf("QEMU_LKL_BENCH_PASS\n");
}

static void do_eject(void) {
    pid_t pid = mount_lklfuse("type=hfsplus,mb=64");
    uint8_t *buf = malloc(65536);
    if (!buf) die("malloc eject");
    uint64_t hash = 14695981039346656037ULL;
    int fd = open("/mnt/eject_dirty.bin", O_CREAT | O_WRONLY | O_TRUNC, 0644);
    if (fd < 0) die("create eject_dirty.bin");
    for (int i = 0; i < 128; i++) { /* 8 MiB dirty without fsync */
        fill_pattern(buf, 65536, 0xdeadbeefULL + (uint64_t)i);
        hash = fnv1a_update(hash, buf, 65536);
        if (write(fd, buf, 65536) != 65536) die("write eject_dirty.bin");
    }
    close(fd);
    free(buf);
    if (mkdir("/mnt/eject_meta", 0755) != 0) die("mkdir eject_meta");
    char p[128];
    for (int i = 0; i < 200; i++) {
        snprintf(p, sizeof(p), "/mnt/eject_meta/item_%03d.dat", i);
        int mfd = open(p, O_CREAT | O_WRONLY | O_TRUNC, 0644);
        if (mfd < 0) die("create meta");
        if (write(mfd, "dirty", 5) != 5) die("write meta");
        close(mfd);
    }
    report_rss(pid, "eject_dirty");
    clean_eject_lklfuse(pid, "dirty_8mb_200files");

    char *fsck_argv[] = { "/sbin/fsck.hfsplus", "-n", "-f", "/dev/vda", NULL };
    int frc = run_cmd(fsck_argv);
    printf("EJECT_FSCK_RC=%d\n", frc);
    if (frc != 0) die("fsck failed after clean eject");

    /* Verify persisted content via kernel ro mount */
    drop_caches();
    if (mount("/dev/vda", "/mnt", "hfsplus", MS_RDONLY, NULL) != 0) die("remount kernel ro");
    uint8_t *rbuf = malloc(65536);
    int rfd = open("/mnt/eject_dirty.bin", O_RDONLY);
    if (rfd < 0) die("reopen eject_dirty.bin");
    uint64_t rhash = 14695981039346656037ULL;
    size_t rtot = 0;
    ssize_t rn;
    while ((rn = read(rfd, rbuf, 65536)) > 0) {
        rhash = fnv1a_update(rhash, rbuf, (size_t)rn);
        rtot += (size_t)rn;
    }
    close(rfd);
    free(rbuf);
    umount("/mnt");
    if (rtot != 8388608ULL || rhash != hash) die("eject data integrity mismatch");
    printf("EJECT_DATA_VERIFIED bytes=%zu hash=0x%016llx\n", rtot, (unsigned long long)rhash);

    /* Replug (2nd RW mount) must succeed RW when HFSPLUS_VOL_UNMNT was written on eject */
    pid_t pid2 = mount_lklfuse("type=hfsplus,mb=64");
    int rfd2 = open("/mnt/replug_rw.txt", O_CREAT | O_WRONLY | O_TRUNC, 0644);
    if (rfd2 < 0) die("replug 2nd mount RW open");
    if (write(rfd2, "replug-ok", 9) != 9) die("replug write");
    close(rfd2);
    clean_eject_lklfuse(pid2, "replug_rw");
    frc = run_cmd(fsck_argv);
    printf("REPLUG_FSCK_RC=%d\n", frc);
    if (frc != 0) die("fsck failed after replug eject");
    printf("QEMU_LKL_EJECT_PASS\n");
}

int main(void) {
    mount("devtmpfs", "/dev", "devtmpfs", 0, NULL);
    int cfd_con = open("/dev/console", O_RDWR);
    if (cfd_con >= 0) {
        dup2(cfd_con, 0);
        dup2(cfd_con, 1);
        dup2(cfd_con, 2);
        if (cfd_con > 2) close(cfd_con);
    }
    mount("proc", "/proc", "proc", 0, NULL);
    mount("sysfs", "/sys", "sysfs", 0, NULL);

    struct utsname u;
    uname(&u);
    char cmdline[1024] = {0};
    int cfd = open("/proc/cmdline", O_RDONLY);
    if (cfd >= 0) { ssize_t r = read(cfd, cmdline, sizeof(cmdline) - 1); (void)r; close(cfd); }

    printf("QEMU_BOOT release=%s arch=%s cmdline=%s\n", u.release, u.machine, cmdline);
    show_zones();

    int rounds = 6;
    const char *rp = strstr(cmdline, "hfs_rounds=");
    if (rp) { int v = atoi(rp + 11); if (v > 0 && v <= 50) rounds = v; }

    uint64_t hash = 0;
    const char *hp = strstr(cmdline, "audio_hash=");
    if (hp) hash = strtoull(hp + 11, NULL, 16);

    if (strstr(cmdline, "lkl_mode=stress")) do_stress(rounds);
    else if (strstr(cmdline, "lkl_mode=prepop")) do_prepop();
    else if (strstr(cmdline, "lkl_mode=bench")) do_bench(hash);
    else if (strstr(cmdline, "lkl_mode=eject")) do_eject();
    else die("unknown lkl_mode");

    fflush(stdout);
    sync();
    reboot(RB_POWER_OFF);
    for (;;) pause();
    return 0;
}
INIT_C

"${CROSS_COMPILE}gcc" -static -O2 -Wall -Wextra -o "$RDIR/init" "$TDIR/init.c"
install -m 0755 "$LKLFUSE" "$RDIR/usr/bin/lklfuse"
install -m 0755 "$FSCK_HFSPLUS" "$RDIR/sbin/fsck.hfsplus"
cp -L "$SYSROOT_LIB/ld-linux-armhf.so.3" "$RDIR/lib/"
cp -L "$SYSROOT_LIB/libc.so.6" "$RDIR/lib/"
cp -L "$SYSROOT_LIB/libgcc_s.so.1" "$RDIR/lib/"
(cd "$RDIR" && find . -print0 | cpio --null -o --format=newc --quiet | gzip -1 > "$TDIR/initramfs.cpio.gz")

make_img() {
    local img=$1 size=$2 label=$3
    rm -f "$img"
    truncate -s "$size" "$img"
    mkfs.hfsplus -v "$label" -n c=8192 "$img" >> "$LOG" 2>&1
    fsck.hfsplus -n -f "$img" >> "$LOG" 2>&1
}

run_qemu() {
    local img=$1 mode=$2 extra=$3 out=$4
    timeout "$QEMU_TIMEOUT" qemu-system-arm \
        -M virt -cpu cortex-a15 -m 2048M -smp 2 \
        -nographic -no-reboot -monitor none \
        -kernel "$QEMU_ZIMAGE" \
        -initrd "$TDIR/initramfs.cpio.gz" \
        -drive "file=$img,if=none,format=raw,id=hd0" \
        -device virtio-blk-device,drive=hd0 \
        -append "console=ttyAMA0,115200 panic=1 lkl_mode=$mode $extra" \
        > "$out" 2>&1 || true
}

echo "=== 1. Gen-1 6-round write/rename/delete stress + fsck.hfsplus -n -f ==="
make_img "$TDIR/disk-stress.img" 128M LKLSTRESS
run_qemu "$TDIR/disk-stress.img" stress "hfs_rounds=$ROUNDS" "$TDIR/stress.log"
grep -q 'HighMem' "$TDIR/stress.log" || die "HighMem missing"
grep -q 'QEMU_LKL_STRESS_PASS' "$TDIR/stress.log" || die "stress failed (see $TDIR/stress.log)"
fsck.hfsplus -n -f "$TDIR/disk-stress.img" > "$TDIR/stress-host-fsck.log" 2>&1 || die "host fsck stress failed"

echo "=== 2. Prepopulate 256M benchmark image (10 MiB audio + 5,000 files) ==="
make_img "$TDIR/disk-bench.img" 256M LKLBENCH
run_qemu "$TDIR/disk-bench.img" prepop "" "$TDIR/prepop.log"
grep -q 'QEMU_LKL_PREPOP_PASS' "$TDIR/prepop.log" || die "prepop failed (see $TDIR/prepop.log)"
fsck.hfsplus -n -f "$TDIR/disk-bench.img" > "$TDIR/prepop-host-fsck.log" 2>&1 || die "host fsck prepop failed"
AUDIO_HASH=$(grep 'PREPOP_AUDIO_HASH=' "$TDIR/prepop.log" | sed 's/.*PREPOP_AUDIO_HASH=//' | tr -d '\r')
[[ -n $AUDIO_HASH ]] || die "missing AUDIO_HASH"

echo "=== 3. Benchmark 10 MiB read + 5,000-file scan + RSS (kernel_ro vs lkl mb=64 vs mb=32) ==="
run_qemu "$TDIR/disk-bench.img" bench "audio_hash=$AUDIO_HASH" "$TDIR/bench.log"
grep -q 'QEMU_LKL_BENCH_PASS' "$TDIR/bench.log" || die "bench failed (see $TDIR/bench.log)"

echo "=== 4. Clean eject (dirty 8 MiB + 200 files, block until lklfuse exits) ==="
make_img "$TDIR/disk-eject.img" 128M LKLEJECT
run_qemu "$TDIR/disk-eject.img" eject "" "$TDIR/eject.log"
grep -q 'QEMU_LKL_EJECT_PASS' "$TDIR/eject.log" || die "eject failed (see $TDIR/eject.log)"
fsck.hfsplus -n -f "$TDIR/disk-eject.img" > "$TDIR/eject-host-fsck.log" 2>&1 || die "host fsck eject failed"

if grep -q 'not cleanly unmounted' "$TDIR/stress.log" "$TDIR/prepop.log" "$TDIR/bench.log" "$TDIR/eject.log"; then
    die "unclean HFS+ unmount warning detected in QEMU logs"
fi
python3 - "$TDIR/disk-stress.img" "$TDIR/disk-bench.img" "$TDIR/disk-eject.img" <<'VHCHECK'
import struct, sys
for img in sys.argv[1:]:
    with open(img, "rb") as f:
        f.seek(1024)
        vh = f.read(512)
    sig, ver, attrs = struct.unpack(">HHI", vh[:8])
    unmnt = bool(attrs & (1 << 8))
    incnstnt = bool(attrs & (1 << 11))
    print(f"VHDR[{img.rsplit('/', 1)[-1]}] sig=0x{sig:04x} attrs=0x{attrs:08x} UNMNT={unmnt} INCNSTNT={incnstnt}")
    if sig != 0x482b or not unmnt or incnstnt:
        raise SystemExit(f"dirty volume header on {img}")
VHCHECK

echo "=== SUMMARY ==="
grep -E '^(QEMU_BOOT|Node| *managed|RSS|EJECT|STRESS_FSCK|QEMU_LKL_)' "$TDIR/stress.log" | tr -d '\r'
grep -E '^(BENCH_|RSS|EJECT|QEMU_LKL_)' "$TDIR/bench.log" | tr -d '\r'
grep -E '^(RSS|EJECT|EJECT_FSCK|EJECT_DATA|REPLUG_FSCK|QEMU_LKL_)' "$TDIR/eject.log" | tr -d '\r'
