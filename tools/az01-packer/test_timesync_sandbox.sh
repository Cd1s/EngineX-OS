#!/bin/bash
# Sandbox boot test for NTP sync (with Google NTP unreachable), persistent clock
# across power loss, and Engine time zone survival across hard power-off and
# failed auto-detect. Runs inside unshare -m -n -p + chroot on an ARM build host;
# virtualizes CLOCK_REALTIME in user space so the host kernel clock is untouched.
set -euo pipefail
export LC_ALL=C

SELF=$(realpath "$0")
REPO=$(cd "$(dirname "$SELF")/../.." && pwd)
LAB=${SC5000_LAB_DIR:-/root/build/sc5000-lab}
IMG=${SC5000_530_IMAGE:-$LAB/fw-5.3.0/SC5000-5.3.0-Update.img}
TARGET_EPOCH=1805112000 # 2027-03-15T12:00:00Z (> 2026-10-30 build date & > 2025-05-29 TIME_EPOCH)
BUILD_EPOCH=1761824489  # 2026-10-30T11:41:29Z (/etc/timestamp in 5.3.0)

if [ "${1:-}" = "--inner" ]; then
    W=${2:?work dir}
    mount --make-rprivate /
    ip link set lo up
    ip addr add 127.0.0.10/8 dev lo 2>/dev/null || :
    # Blackhole 127.0.0.99 so Google NTP, the first candidate (testing failover), and
    # api.inmusicbrands.com fail immediately with ENETUNREACH / connection failure.
    ip route add blackhole 127.0.0.99/32

    R=$W/root
    DATA=$W/data
    mkdir -p "$R" "$DATA/system/var/overlay/lib" "$DATA/enginex/ui/packs"
    ln -sfn enginex "$DATA/sc5000-custom"
    mount -o loop,ro "$W/rootfs.ext2" "$R"

    # Overlay writable /etc (lower = stock + rootfs-overlay), /run, /tmp, /sys, /dev, /data, /var/lib
    mkdir -p "$W/etc-merged"
    cp -a "$R/etc/." "$W/etc-merged/"
    cp -a "$REPO/rootfs-overlay/etc/." "$W/etc-merged/"
    sh "$REPO/rootfs-overlay/usr/local/bin/sc5000-debloat" --root "$W/stage-debloat" 2>/dev/null || :
    mount --bind "$W/etc-merged" "$R/etc"
    mount --bind "$DATA" "$R/data"
    mount --bind "$DATA/system/var/overlay/lib" "$R/var/lib"
    for d in run tmp var/volatile dev sys; do mount -t tmpfs tmpfs "$R/$d"; done

    setup_volatile() {
        local reset_rtc=$1
        rm -rf "$R/run/"* "$R/tmp/"* "$R/var/volatile/"*
        mkdir -p -m 1777 "$R/var/volatile/tmp"
        mkdir -p "$R/var/volatile/log" "$R/var/volatile/cache" 
        mkdir -p "$R/dev/shm" "$R/dev/pts" "$R/sys/firmware/devicetree/base" "$R/sys/devices/system/cpu"
        for f in online possible present; do echo 0-3 > "$R/sys/devices/system/cpu/$f"; done
        for n in null zero full random urandom; do
            [ -e "$R/dev/$n" ] || { touch "$R/dev/$n" && mount --bind "/dev/$n" "$R/dev/$n"; }
        done
        printf 'JP07' > "$R/sys/firmware/devicetree/base/inmusic,product-code"
        printf 'inmusic,jp07\0inmusic,az01\0rockchip,rk3288\0' > "$R/sys/firmware/devicetree/base/compatible"
                mkdir -p "$R/run/dbus" "$R/run/systemd/timesync" "$R/tmp/sc5000-engine-home"
        chmod 0700 "$R/tmp/sc5000-engine-home"
        chown -R 994:992 "$R/run/systemd/timesync"
        mkdir -p "$R/var/lib/systemd/timesync"
        chown -R 994:992 "$R/var/lib/systemd/timesync"
        mkdir -p -m 0755 "$R/run/vclock"
        cp "$W/vclock_shim.so" "$R/run/vclock/vclock_shim.so"
        chmod 0755 "$R/run/vclock/vclock_shim.so"
        mkdir -p -m 0755 "$R/run/vclock"
        printf '%s 0\n' "$reset_rtc" > "$R/run/vclock/state"
        chmod 0666 "$R/run/vclock/state"
        printf '           CPU0\n 41:        100     GICv2  32 Level     ttyS0\n' > "$R/run/interrupts"
        if ! mountpoint -q "$R/proc"; then
            mount -t proc proc "$R/proc"
        fi
        mountpoint -q "$R/proc/interrupts" || mount --bind "$R/run/interrupts" "$R/proc/interrupts"
        if ! mountpoint -q "$R/proc/irq"; then
            mount -t tmpfs tmpfs "$R/proc/irq"
            mkdir -p "$R/proc/irq/41"
            printf 'f\n' > "$R/proc/irq/41/smp_affinity"
            printf '0-3\n' > "$R/proc/irq/41/smp_affinity_list"
        fi
    }

    read_vclock() {
        awk '{print $1}' "$R/run/vclock/state"
    }

    cat > "$R/etc/hosts" << 'HOSTS'
127.0.0.1 localhost
127.0.0.99 time.google.com time1.google.com time2.google.com time3.google.com time4.google.com
127.0.0.99 ntp.aliyun.com
127.0.0.10 ntp.tencent.com time.apple.com time.cloudflare.com pool.ntp.org
127.0.0.99 api.inmusicbrands.com
HOSTS

    # Start local RFC 5905 NTPv4 responder on 127.0.0.10:123 serving TARGET_EPOCH
    python3 -I "$W/ntp_responder.py" 127.0.0.10 123 "$TARGET_EPOCH" "$W/ntp-hits.log" &
    NTP_PID=$!
    trap 'kill "$NTP_PID" 2>/dev/null || :' EXIT
    for _ in $(seq 1 30); do [ -f "$W/ntp-ready" ] && break; sleep 0.1; done
    [ -f "$W/ntp-ready" ] || { echo "FAIL: local NTP responder did not start" >&2; exit 1; }

    # --- Phase 0: Control check (stock Google-only config cannot sync when Google is blackholed) ---
    rm -f "$R/etc/systemd/timesyncd.conf.d/10-enginex-ntp.conf"
    rm -f "$R/var/lib/systemd/timesync/clock"
    setup_volatile 5
    timeout 3 chroot "$R" /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin \
        LD_PRELOAD=/run/vclock/vclock_shim.so VCLOCK_FILE=/run/vclock/state SYSTEMD_LOG_LEVEL=debug \
        /usr/lib/systemd/systemd-timesyncd > "$W/timesyncd-stock.log" 2>&1 || :
    if [ -e "$R/run/systemd/timesync/synchronized" ] || [ "$(read_vclock)" -ge "$BUILD_EPOCH" ]; then
        echo "FAIL: stock Google-only timesyncd unexpectedly synced with Google blackholed" >&2
        exit 1
    fi
    echo "PASS 0: control confirmed stock Google-only timesyncd fails when Google NTP is unreachable (vclock=$(read_vclock))"

    # --- Phase 1: Install drop-in + prove etc-reset survival + sync clock from non-Google server ---
    mkdir -p "$R/etc/systemd/timesyncd.conf.d"
    cp "$REPO/rootfs-overlay/etc/systemd/timesyncd.conf.d/10-enginex-ntp.conf" "$R/etc/systemd/timesyncd.conf.d/10-enginex-ntp.conf"
    # Seed stale upper-layer timesyncd files and run sc5000-custom-etc-reset
    mkdir -p "$DATA/system/etc/overlay/systemd/timesyncd.conf.d"
    printf '[Time]\nFallbackNTP=time1.google.com\n' > "$DATA/system/etc/overlay/systemd/timesyncd.conf"
    printf '[Time]\nFallbackNTP=time1.google.com\n' > "$DATA/system/etc/overlay/systemd/timesyncd.conf.d/10-enginex-ntp.conf"
    python3 -I - "$REPO" > "$W/etc-reset.sh" << 'PY'
import importlib.util, sys
from pathlib import Path
repo = Path(sys.argv[1])
spec = importlib.util.spec_from_file_location("az01_packer", repo / "tools/az01-packer/az01_packer.py")
az = importlib.util.module_from_spec(spec); spec.loader.exec_module(az)
managed = sorted(p.relative_to(repo / "rootfs-overlay/etc").as_posix()
                 for p in (repo / "rootfs-overlay/etc").rglob("*") if p.is_file())
sys.stdout.buffer.write(az.etc_reset("5.3.0.4-sandbox", managed))
PY
    chroot "$R" /bin/sh -s < "$W/etc-reset.sh"
    [ ! -e "$DATA/system/etc/overlay/systemd/timesyncd.conf" ]
    [ ! -e "$DATA/system/etc/overlay/systemd/timesyncd.conf.d" ]
    cmp -s "$REPO/rootfs-overlay/etc/systemd/timesyncd.conf.d/10-enginex-ntp.conf" "$R/etc/systemd/timesyncd.conf.d/10-enginex-ntp.conf"
    echo "PASS 1a: 10-enginex-ntp.conf survived sc5000-custom-etc-reset and purged stale upper-layer overrides"

    rm -f "$R/var/lib/systemd/timesync/clock"
    setup_volatile 5
    chroot "$R" /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin \
        LD_PRELOAD=/run/vclock/vclock_shim.so VCLOCK_FILE=/run/vclock/state SYSTEMD_LOG_LEVEL=debug \
        /usr/lib/systemd/systemd-timesyncd > "$W/timesyncd-boot1.log" 2>&1 &
    TS_PID=$!
    for _ in $(seq 1 80); do
        [ -e "$R/run/systemd/timesync/synchronized" ] && break
        sleep 0.1
    done
    if [ ! -e "$R/run/systemd/timesync/synchronized" ]; then
        cat "$W/timesyncd-boot1.log" >&2
        echo "FAIL: timesyncd did not synchronize from non-Google server" >&2
        exit 1
    fi
    BOOT1_CLK=$(read_vclock)
    [ "$BOOT1_CLK" -ge "$TARGET_EPOCH" ] || { echo "FAIL: virtual clock $BOOT1_CLK < $TARGET_EPOCH" >&2; exit 1; }
    [ -s "$W/ntp-hits.log" ] || { echo "FAIL: local NTP responder received no packets" >&2; exit 1; }
    CLOCK_MTIME=$(stat -c %Y "$DATA/system/var/overlay/lib/systemd/timesync/clock")
    [ "$CLOCK_MTIME" -ge "$TARGET_EPOCH" ] || { echo "FAIL: persistent clock mtime $CLOCK_MTIME < $TARGET_EPOCH" >&2; exit 1; }
    echo "PASS 1b: clock synced from non-Google server (ntp.tencent.com failover past unreachable ntp.aliyun.com & Google; vclock=$BOOT1_CLK, clock_mtime=$CLOCK_MTIME)"

    # Boot 1 of real SC5000 5.3.0 Engine: set timezone to Asia/Shanghai, then hard power-off (SIGKILL)
    cp -a "$W/packs/en" "$DATA/enginex/ui/packs/"
    mkdir -p "$DATA/enginex/ui/packs/en/Skins/Common/Items"
    cp "$W/probe-boot1.qml" "$DATA/enginex/ui/packs/en/Skins/Common/Items/QmlWarnings.qml"
    printf 'lang=en\n' > "$DATA/enginex/ui/settings.conf"
    printf '{"TIDAL": "{\\"provider\\":\\"TIDAL\\",\\"providerType\\":\\"TIDAL\\",\\"accessToken\\":\\"enginex\\",\\"refreshToken\\":\\"enginex\\"}"}\n' > "$DATA/StreamingAccounts.json"

    chroot "$R" /usr/bin/dbus-daemon --system --fork --nopidfile > "$W/dbus1.log" 2>&1 || chroot "$R" /usr/bin/dbus-daemon --system --fork
    chroot "$R" /usr/bin/env -i PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
        HOME=/tmp/sc5000-engine-home MESA_SHADER_CACHE_MAX_SIZE=32M QML_DISABLE_DISK_CACHE=1 \
        QT_QPA_PLATFORM=offscreen MALLOC_ARENA_MAX=1 LD_LIBRARY_PATH=/usr/qt/lib \
        VCLOCK_FILE=/run/vclock/state LD_PRELOAD=/run/vclock/vclock_shim.so:/etc/sc5000/libengine-ui-hook.so \
        /usr/Engine/Engine -d0 > "$W/engine-boot1.log" 2>&1 &
    ENG1_PID=$!
    for _ in $(seq 1 300); do
        grep -aq 'TSPROBE BOOT1_PASS|' "$W/engine-boot1.log" && break
        if ! kill -0 "$ENG1_PID" 2>/dev/null; then break; fi
        sleep 0.2
    done
    if ! grep -aq 'TSPROBE BOOT1_PASS|tz=Asia/Shanghai|utc=false|offset=-480|' "$W/engine-boot1.log"; then
        tail -n 60 "$W/engine-boot1.log" >&2
        echo "FAIL: Engine boot 1 did not apply Asia/Shanghai" >&2
        exit 1
    fi

    # Simulate abrupt AC power loss: SIGKILL Engine & timesyncd & dbus (destructors NEVER run!)
    kill -9 "$ENG1_PID" "$TS_PID" 2>/dev/null || :
    chroot "$R" /bin/sh -c 'kill -9 $(pidof Engine systemd-timesyncd dbus-daemon) 2>/dev/null || :' || :
    wait "$ENG1_PID" 2>/dev/null || :
    wait "$TS_PID" 2>/dev/null || :

    QCONF="$DATA/.config/jp07.user.settings/AIR Music Technology/Engine.conf"
    [ -f "$QCONF" ] || { echo "FAIL: QSettings Engine.conf was not persisted before power loss: $QCONF" >&2; exit 1; }
    grep -Fqx "[TimeDateSettings]" "$QCONF"
    grep -Fqx "TZEnvVar=Asia/Shanghai" "$QCONF"
    echo "PASS 1c: Asia/Shanghai persisted immediately to $QCONF before SIGKILL power loss"

    # --- Phase 2: Simulated power cycle -> offline boot (no network, RTC reset to 1970, auto-detect failing) ---
    kill "$NTP_PID" 2>/dev/null || :
    wait "$NTP_PID" 2>/dev/null || :
    ip route add blackhole 127.0.0.10/32

    # Drop all volatile state (/run, /tmp) and reset unbacked RTC back to 1970-01-01T00:00:05Z
    setup_volatile 5
    [ "$(read_vclock)" -eq 5 ]

    # Start timesyncd completely offline: clock must restore from /var/lib/systemd/timesync/clock on /data
    timeout 2 chroot "$R" /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin \
        LD_PRELOAD=/run/vclock/vclock_shim.so VCLOCK_FILE=/run/vclock/state SYSTEMD_LOG_LEVEL=debug \
        /usr/lib/systemd/systemd-timesyncd > "$W/timesyncd-boot2.log" 2>&1 || :
    BOOT2_CLK=$(read_vclock)
    if [ "$BOOT2_CLK" -lt "$TARGET_EPOCH" ]; then
        cat "$W/timesyncd-boot2.log" >&2
        echo "FAIL: after power cycle offline, clock regressed to $BOOT2_CLK (< $TARGET_EPOCH, build=$BUILD_EPOCH)" >&2
        exit 1
    fi
    echo "PASS 2a: after simulated power cycle with no network, clock restored to $BOOT2_CLK (>= $TARGET_EPOCH, never regressed to build date $BUILD_EPOCH)"

    # Boot Engine offline with auto-detect failing + hostile UTC fallback attempt
    cp "$W/probe-boot2.qml" "$DATA/enginex/ui/packs/en/Skins/Common/Items/QmlWarnings.qml"
    rm -f "$DATA/enginex/ui/safe-mode" "$DATA/enginex/ui/boot-count" "$DATA/enginex/ui/boot-token"
    chroot "$R" /usr/bin/dbus-daemon --system --fork --nopidfile > "$W/dbus2.log" 2>&1 || chroot "$R" /usr/bin/dbus-daemon --system --fork
    chroot "$R" /usr/bin/env -i PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
        HOME=/tmp/sc5000-engine-home MESA_SHADER_CACHE_MAX_SIZE=32M QML_DISABLE_DISK_CACHE=1 \
        QT_QPA_PLATFORM=offscreen MALLOC_ARENA_MAX=1 LD_LIBRARY_PATH=/usr/qt/lib \
        VCLOCK_FILE=/run/vclock/state LD_PRELOAD=/run/vclock/vclock_shim.so:/etc/sc5000/libengine-ui-hook.so \
        /usr/Engine/Engine -d0 > "$W/engine-boot2.log" 2>&1 &
    ENG2_PID=$!
    for _ in $(seq 1 300); do
        grep -aq 'TSPROBE BOOT2_PASS|' "$W/engine-boot2.log" && break
        if ! kill -0 "$ENG2_PID" 2>/dev/null; then break; fi
        sleep 0.2
    done
    kill -9 "$ENG2_PID" 2>/dev/null || :
    chroot "$R" /bin/sh -c 'kill -9 $(pidof Engine dbus-daemon) 2>/dev/null || :' || :
    wait "$ENG2_PID" 2>/dev/null || :

    if ! grep -aq 'TSPROBE BOOT2_PASS|tz=Asia/Shanghai|utc=false|offset=-480|' "$W/engine-boot2.log"; then
        tail -n 60 "$W/engine-boot2.log" >&2
        echo "FAIL: chosen timezone Asia/Shanghai did not survive offline boot / failed auto-detect" >&2
        exit 1
    fi
    grep -Fqx "TZEnvVar=Asia/Shanghai" "$QCONF"
    echo "PASS 2b: chosen timezone Asia/Shanghai survived hard power cycle, offline boot, and failed detectTimeZone()"

    # --- Phase 3: Gen2 /data bundle mechanism (/run/systemd/timesyncd.conf.d/10-enginex-ntp.conf) ---
    rm -f "$R/etc/systemd/timesyncd.conf.d/10-enginex-ntp.conf"
    ip route del blackhole 127.0.0.10/32
    setup_volatile 5
    mkdir -p "$R/run/systemd/timesyncd.conf.d"
    cp "$REPO/rootfs-overlay/etc/systemd/timesyncd.conf.d/10-enginex-ntp.conf" "$R/run/systemd/timesyncd.conf.d/10-enginex-ntp.conf"
    rm -f "$R/var/lib/systemd/timesync/clock" "$W/ntp-ready"
    python3 -I "$W/ntp_responder.py" 127.0.0.10 123 "$TARGET_EPOCH" "$W/ntp-hits-gen2.log" &
    NTP_PID=$!
    for _ in $(seq 1 30); do [ -f "$W/ntp-ready" ] && break; sleep 0.1; done
    chroot "$R" /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin \
        LD_PRELOAD=/run/vclock/vclock_shim.so VCLOCK_FILE=/run/vclock/state SYSTEMD_LOG_LEVEL=debug \
        /usr/lib/systemd/systemd-timesyncd > "$W/timesyncd-gen2.log" 2>&1 &
    TS2_PID=$!
    for _ in $(seq 1 80); do
        [ -e "$R/run/systemd/timesync/synchronized" ] && break
        sleep 0.1
    done
    kill -9 "$TS2_PID" "$NTP_PID" 2>/dev/null || :
    wait "$TS2_PID" 2>/dev/null || :
    wait "$NTP_PID" 2>/dev/null || :
    [ -e "$R/run/systemd/timesync/synchronized" ] || { cat "$W/timesyncd-gen2.log" >&2; echo "FAIL: Gen2 /run drop-in did not sync" >&2; exit 1; }
    [ "$(read_vclock)" -ge "$TARGET_EPOCH" ]
    echo "PASS 3: Gen2 /run/systemd/timesyncd.conf.d/10-enginex-ntp.conf synced via identical timesyncd mechanism"
    exit 0
fi

[ "$(id -u)" = 0 ] || { echo "run as root on the ARM build host" >&2; exit 2; }
[ -f "$IMG" ] || { echo "missing firmware image: $IMG" >&2; exit 2; }
[ -r /etc/profile.d/buildtools.sh ] && . /etc/profile.d/buildtools.sh

W=$(realpath -m "${1:-$(mktemp -d /var/tmp/timesync-sandbox.XXXXXX)}")
CLEAN_W=0
if [ $# -eq 0 ]; then CLEAN_W=1; fi
trap '[ "$CLEAN_W" = 0 ] || rm -rf "$W"' EXIT
mkdir -p "$W"

HOST_BEFORE=$(date +%s)

echo "1/4 Unpacking SC5000 5.3.0 rootfs and building UI pack..."
python3 -I "$REPO/tools/az01-packer/az01_packer.py" unpack "$IMG" --output "$W/unpacked" >/dev/null
mv "$W/unpacked/rootfs.ext2" "$W/rootfs.ext2"
rm -rf "$W/unpacked"

if [ -d "$LAB/5.3.0/qrc" ]; then
    QRC_DIR=$LAB/5.3.0/qrc
else
    arm-linux-gnueabihf-gcc -O2 -shared -fPIC -o "$W/qrc_dump.so" "$REPO/tools/ui-mod/qrc_dump.c" -ldl
    python3 -I "$REPO/tools/ui-mod/extract_qrc.py" --image "$IMG" --preload "$W/qrc_dump.so" --out "$W/qrc" >/dev/null
    QRC_DIR=$W/qrc
fi
python3 -I "$REPO/tools/ui-mod/build_packs.py" --qrc "$QRC_DIR" --out "$W/packs" >/dev/null

echo "2/4 Building user-space virtual RTC fixture..."
cat > "$W/vclock_shim.c" << 'CEOF'
#define _GNU_SOURCE
#include <dlfcn.h>
#include <errno.h>
#include <fcntl.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <sys/time.h>
#include <sys/timex.h>
#include <time.h>
#include <unistd.h>
#include <sys/socket.h>
#ifndef SCM_TIMESTAMPNS_NEW
#define SCM_TIMESTAMPNS_NEW 64
#endif
static void vclock_get(struct timespec *tp);
ssize_t recvmsg(int sockfd, struct msghdr *msg, int flags) {
    static ssize_t (*real_fn)(int, struct msghdr *, int) = NULL;
    if (!real_fn) real_fn = dlsym(RTLD_NEXT, "__recvmsg64");
    if (!real_fn) real_fn = dlsym(RTLD_NEXT, "recvmsg");
    ssize_t n = real_fn(sockfd, msg, flags);
    if (n >= 0 && msg && msg->msg_control && msg->msg_controllen > 0) {
        for (struct cmsghdr *cmsg = CMSG_FIRSTHDR(msg); cmsg; cmsg = CMSG_NXTHDR(msg, cmsg)) {
            if (cmsg->cmsg_level == SOL_SOCKET &&
                (cmsg->cmsg_type == SCM_TIMESTAMPNS || cmsg->cmsg_type == SCM_TIMESTAMPNS_NEW) &&
                cmsg->cmsg_len >= CMSG_LEN(sizeof(struct timespec))) {
                struct timespec now;
                vclock_get(&now);
                memcpy(CMSG_DATA(cmsg), &now, sizeof now);
            }
        }
    }
    return n;
}

#include <linux/capability.h>
int capset(cap_user_header_t hdrp, cap_user_data_t datap) {
    static int (*real_fn)(cap_user_header_t, const struct __user_cap_data_struct *) = NULL;
    if (!real_fn) real_fn = dlsym(RTLD_NEXT, "capset");
    if (hdrp && datap) {
        struct __user_cap_data_struct copy[2];
        memcpy(copy, datap, sizeof copy);
        copy[0].effective &= ~(1U << CAP_SYS_TIME);
        copy[0].permitted &= ~(1U << CAP_SYS_TIME);
        copy[0].inheritable &= ~(1U << CAP_SYS_TIME);
        return real_fn(hdrp, copy);
    }
    return real_fn(hdrp, datap);
}


static const char *vclock_path(void) {
    const char *p = getenv("VCLOCK_FILE");
    return (p && *p) ? p : "/run/vclock/state";
}

static int raw_openat_call(int dfd, const char *path, int flags, mode_t mode) {
    static int (*fn)(int, const char *, int, ...) = NULL;
    if (!fn) fn = dlsym(RTLD_NEXT, "openat");
    return fn ? fn(dfd, path, flags, mode) : (int)syscall(SYS_openat, dfd, path, flags, mode);
}

static int real_clock_gettime_call(clockid_t clk, struct timespec *tp) {
    static int (*fn)(clockid_t, struct timespec *) = NULL;
    if (!fn) fn = dlsym(RTLD_NEXT, "__clock_gettime64");
    if (!fn) fn = dlsym(RTLD_NEXT, "clock_gettime");
    return fn ? fn(clk, tp) : -1;
}

// /tmp/vclock stores "<base_real_sec> <mono_ref_sec>" so virtual CLOCK_REALTIME ticks
// naturally with CLOCK_MONOTONIC while isolated from the host's real wall clock.
static void vclock_get(struct timespec *tp) {
    struct timespec mono = {0, 0};
    real_clock_gettime_call(CLOCK_MONOTONIC, &mono);
    long long base = 5, ref = mono.tv_sec;
    int fd = raw_openat_call(AT_FDCWD, vclock_path(), O_RDONLY | O_CLOEXEC, 0);
    if (fd >= 0) {
        char buf[64];
        ssize_t n = read(fd, buf, sizeof buf - 1);
        close(fd);
        if (n > 0) {
            buf[n] = 0;
            long long b = 5, r = 0;
            int got = sscanf(buf, "%lld %lld", &b, &r);
            if (got >= 1) base = b;
            if (got >= 2 && r > 0) ref = r;
        }
    }
    tp->tv_sec = (time_t)(base + (mono.tv_sec - ref));
    tp->tv_nsec = mono.tv_nsec;
}

static void vclock_set(time_t sec) {
    struct timespec mono = {0, 0};
    real_clock_gettime_call(CLOCK_MONOTONIC, &mono);
    int fd = raw_openat_call(AT_FDCWD, vclock_path(), O_WRONLY | O_TRUNC | O_CLOEXEC, 0);
    if (fd >= 0) {
        char buf[64];
        int n = snprintf(buf, sizeof buf, "%lld %lld\n", (long long)sec, (long long)mono.tv_sec);
        if (n > 0) { if (write(fd, buf, (size_t)n) < 0) {} }
        fchmod(fd, 0666);
        close(fd);
    }
}

int clock_gettime(clockid_t clk, struct timespec *tp) {
    if (clk == CLOCK_REALTIME || clk == CLOCK_REALTIME_COARSE) {
        if (!tp) { errno = EFAULT; return -1; }
        vclock_get(tp);
        return 0;
    }
    return real_clock_gettime_call(clk, tp);
}

int clock_settime(clockid_t clk, const struct timespec *tp) {
    if (clk == CLOCK_REALTIME) {
        if (!tp) { errno = EFAULT; return -1; }
        vclock_set(tp->tv_sec);
        return 0;
    }
    errno = EPERM;
    return -1;
}

int clock_adjtime(clockid_t clk, struct timex *tx) {
    if (clk != CLOCK_REALTIME || !tx) { errno = EINVAL; return -1; }
    struct timespec cur;
    vclock_get(&cur);
    if (tx->modes & ADJ_SETOFFSET) {
        long long dsec = tx->time.tv_sec;
        long long dnsec = (tx->modes & ADJ_NANO) ? (long long)tx->time.tv_usec : (long long)tx->time.tv_usec * 1000LL;
        long long total_ns = (long long)cur.tv_sec * 1000000000LL + cur.tv_nsec + dsec * 1000000000LL + dnsec;
        if (total_ns < 0) total_ns = 0;
        cur.tv_sec = (time_t)(total_ns / 1000000000LL);
        cur.tv_nsec = (long)(total_ns % 1000000000LL);
        vclock_set(cur.tv_sec);
    }
    tx->time.tv_sec = cur.tv_sec;
    tx->time.tv_usec = (tx->modes & ADJ_NANO) ? cur.tv_nsec : (cur.tv_nsec / 1000);
    tx->maxerror = 0;
    tx->esterror = 0;
    return TIME_OK;
}

int gettimeofday(struct timeval *tv, void *tz) {
    (void)tz;
    if (tv) {
        struct timespec tp;
        vclock_get(&tp);
        tv->tv_sec = tp.tv_sec;
        tv->tv_usec = tp.tv_nsec / 1000;
    }
    return 0;
}

time_t time(time_t *tloc) {
    struct timespec tp;
    vclock_get(&tp);
    if (tloc) *tloc = tp.tv_sec;
    return tp.tv_sec;
}

int futimens(int fd, const struct timespec times[2]) {
    static int (*real_fn)(int, const struct timespec[2]) = NULL;
    if (!real_fn) real_fn = dlsym(RTLD_NEXT, "__futimens64");
    struct timespec sub[2];
    if (!times || times[0].tv_nsec == UTIME_NOW || times[1].tv_nsec == UTIME_NOW) {
        struct timespec now;
        vclock_get(&now);
        sub[0] = (!times || times[0].tv_nsec == UTIME_NOW) ? now : times[0];
        sub[1] = (!times || times[1].tv_nsec == UTIME_NOW) ? now : times[1];
        times = sub;
    }
    return real_fn(fd, times);
}

int utimensat(int dfd, const char *path, const struct timespec times[2], int flags) {
    static int (*real_fn)(int, const char *, const struct timespec[2], int) = NULL;
    if (!real_fn) real_fn = dlsym(RTLD_NEXT, "__utimensat64");
    struct timespec sub[2];
    if (!times || times[0].tv_nsec == UTIME_NOW || times[1].tv_nsec == UTIME_NOW) {
        struct timespec now;
        vclock_get(&now);
        sub[0] = (!times || times[0].tv_nsec == UTIME_NOW) ? now : times[0];
        sub[1] = (!times || times[1].tv_nsec == UTIME_NOW) ? now : times[1];
        times = sub;
    }
    return real_fn(dfd, path, times, flags);
}
CEOF

arm-linux-gnueabihf-gcc -O2 -Wall -Wextra -Werror -Wno-nonnull-compare -shared -fPIC \
    -U_FORTIFY_SOURCE -D_FORTIFY_SOURCE=0 -D_FILE_OFFSET_BITS=64 -D_TIME_BITS=64 -Wl,-z,max-page-size=4096 \
    -o "$W/vclock_shim.so" "$W/vclock_shim.c" -ldl

cat > "$W/ntp_responder.py" << 'PY'
import pathlib, socket, struct, sys
host, port, target_unix, log_path = sys.argv[1], int(sys.argv[2]), int(sys.argv[3]), sys.argv[4]
NTP_DELTA = 2208988800
ntp_sec = target_unix + NTP_DELTA
sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
sock.bind((host, port))
(pathlib.Path(log_path).parent / "ntp-ready").write_text("1\n")
while True:
    pkt, addr = sock.recvfrom(512)
    if len(pkt) < 48:
        continue
    # Copy client transmit timestamp (bytes 40..48) into origin timestamp (bytes 24..32)
    orig = pkt[40:48]
    # LI=0, VN=4, Mode=4 (server) -> 0x24; stratum=2, poll=6, precision=-20
    resp = struct.pack("!BBBbIII", 0x24, 2, 6, -20, 16, 16, 0x434E5450)
    resp += struct.pack("!II", ntp_sec - 1, 0)  # reference timestamp
    resp += orig                                # origin timestamp
    resp += struct.pack("!II", ntp_sec, 0)      # receive timestamp
    resp += struct.pack("!II", ntp_sec, 1000)   # transmit timestamp
    sock.sendto(resp, addr)
    with open(log_path, "a") as f:
        f.write(f"{addr[0]}:{addr[1]}\n")
PY

cat > "$W/probe-boot1.qml" << 'QML'
import QtQuick 2.12
import Planck 1.0
Item {
    visible: false
    property int step: 0
    Timer {
        interval: 800; running: true; repeat: true
        onTriggered: {
            if (step === 0) {
                step = 1
                Planck.timeDateController.setSystemTimeZone("Asia/Shanghai")
            } else if (step === 1) {
                step = 2
                const tz = Planck.timeDateController.getSystemTimeZone()
                const isUtc = Planck.timeDateController.isSystemTimeZoneUTC()
                const off = new Date(1805112000000).getTimezoneOffset()
                console.warn("TSPROBE BOOT1_PASS|tz=" + tz + "|utc=" + isUtc + "|offset=" + off + "|now=" + Date.now() + "|")
            }
        }
    }
}
QML

cat > "$W/probe-boot2.qml" << 'QML'
import QtQuick 2.12
import Planck 1.0
Item {
    visible: false
    property int step: 0
    Timer {
        interval: 800; running: true; repeat: true
        onTriggered: {
            if (step === 0) {
                step = 1
                const bootTz = Planck.timeDateController.getSystemTimeZone()
                const bootUtc = Planck.timeDateController.isSystemTimeZoneUTC()
                console.warn("TSPROBE BOOT2_INIT|tz=" + bootTz + "|utc=" + bootUtc + "|now=" + Date.now() + "|")
                if (bootTz !== "Asia/Shanghai" || bootUtc) {
                    console.warn("TSPROBE ERROR|boot2 lost timezone: " + bootTz + "|")
                    return
                }
                // Trigger failed network auto-detect AND hostile UTC fallback attempt
                const autoset = Planck.getProperty("/Client/Preferences/TimeZoneAutoset")
                if (autoset && autoset.translator) autoset.translator.state = true
                Planck.timeDateController.detectTimeZone()
                Planck.timeDateController.setSystemTimeZone("UTC")
            } else if (step === 1) {
                step = 2
            } else if (step === 2) {
                step = 3
                const tz = Planck.timeDateController.getSystemTimeZone()
                const isUtc = Planck.timeDateController.isSystemTimeZoneUTC()
                const off = new Date(1805112000000).getTimezoneOffset()
                console.warn("TSPROBE BOOT2_PASS|tz=" + tz + "|utc=" + isUtc + "|offset=" + off + "|now=" + Date.now() + "|")
            }
        }
    }
}
QML

echo "3/4 Running unshare -m -n -p + chroot sandbox boot test..."
setpriv --bounding-set=-sys_time unshare -m -n -p -f --kill-child "$SELF" --inner "$W"

echo "4/4 Verifying host clock was untouched..."
HOST_AFTER=$(date +%s)
DIFF=$((HOST_AFTER - HOST_BEFORE))
if [ "$DIFF" -lt -5 ] || [ "$DIFF" -gt 300 ]; then
    echo "FAIL: host clock drifted unexpectedly ($HOST_BEFORE -> $HOST_AFTER)" >&2
    exit 1
fi
echo "TIMESYNC_SANDBOX_PASS (host clock untouched, elapsed ${DIFF}s)"
