#!/bin/bash
# Sandbox boot test for NTP sync (with Google NTP unreachable), persistent clock
# floor + daemon advancement outside Engine, and QML + stream-sync + prepare
# time zone capture/retention across both auto-detected and manual changes,
# immediate SIGKILL power loss, and offline reboot with failed auto-detect.
# Runs inside unshare -m -n -p + chroot on an ARM build host; virtualizes
# CLOCK_REALTIME in user space so the host kernel clock is untouched.
set -euo pipefail
export LC_ALL=C

SELF=$(realpath "$0")
REPO=$(cd "$(dirname "$SELF")/../.." && pwd)
LAB=${SC5000_LAB_DIR:-/root/build/sc5000-lab}
IMG=${SC5000_530_IMAGE:-$LAB/fw-5.3.0/SC5000-5.3.0-Update.img}
TARGET_EPOCH=1805112000 # 2027-03-15T12:00:00Z (> host clock & > 2026-10-30 build date)
BUILD_EPOCH=1793360489  # 2026-10-30T11:41:29Z (/etc/timestamp in SC5000 5.3.0)

if [ "${1:-}" = "--inner" ]; then
    W=${2:?work dir}
    python3 -I - << 'CAPPY'
import sys
CAP_SYS_TIME = 1 << 25
for line in open("/proc/self/status"):
    if line.startswith(("CapInh:", "CapPrm:", "CapEff:", "CapBnd:", "CapAmb:")):
        k, v = line.split(":", 1)
        mask = int(v.strip(), 16)
        if mask & CAP_SYS_TIME:
            sys.stderr.write(f"FAIL: sandbox still holds CAP_SYS_TIME in {k}: {v.strip()}\n")
            sys.exit(1)
CAPPY
    mount --make-rprivate /
    ip link set lo up
    ip addr add 127.0.0.10/8 dev lo 2>/dev/null || :
    # Blackhole 127.0.0.99 so Google NTP and the first CN candidate fail immediately.
    ip route add blackhole 127.0.0.99/32

    R=$W/root
    DATA=$W/data
    mkdir -p "$R" "$DATA/system/var/overlay/lib" "$DATA/enginex/ui/packs" "$DATA/enginex/state"
    ln -sfn enginex "$DATA/sc5000-custom"
    mount -o loop,ro "$W/rootfs.ext2" "$R"

    ACTUAL_TS=$(stat -c %Y "$R/etc/timestamp")
    [ "$ACTUAL_TS" -eq "$BUILD_EPOCH" ] || {
        echo "FAIL: unexpected /etc/timestamp mtime $ACTUAL_TS (expected $BUILD_EPOCH)" >&2
        exit 1
    }

    # Overlay writable /etc (lower = stock + rootfs-overlay + sandbox TLS CA), /usr/local/bin, /data, /var/lib
    mkdir -p "$W/etc-merged" "$DATA/enginex/bin"
    cp -a "$R/etc/." "$W/etc-merged/"
    cp -a "$REPO/rootfs-overlay/etc/." "$W/etc-merged/"
    cp "$W/ca.crt" "$W/etc-merged/ssl/certs/Sandbox_GeoIP_Test_CA.pem"
    ln -sfn Sandbox_GeoIP_Test_CA.pem "$W/etc-merged/ssl/certs/$(openssl x509 -subject_hash -noout -in "$W/ca.crt").0"
    cat "$W/ca.crt" >> "$W/etc-merged/ssl/certs/ca-certificates.crt"
    cp "$W/sc5000-suite" "$DATA/enginex/bin/sc5000-suite"
    chmod 0755 "$DATA/enginex/bin/sc5000-suite"
    ln -sfn sc5000-suite "$DATA/enginex/bin/stream-sync"

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
        mkdir -p -m 0755 "$R/run/vclock"
        cp "$W/vclock_shim.so" "$R/run/vclock/vclock_shim.so"
        chmod 0755 "$R/run/vclock/vclock_shim.so"
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

    start_stream_sync() {
        local log_file=$1
        chroot "$R" /usr/bin/env -i PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
            HOME=/root /data/enginex/bin/stream-sync \
            --listen 127.0.0.1:8080 --deemixd "" --relay-url "" --disable-guard > "$log_file" 2>&1 &
        SS_PID=$!
        for _ in $(seq 1 60); do
            if python3 -I -c 'import urllib.request,sys; r=urllib.request.urlopen("http://127.0.0.1:8080/v1/sc5000/ui",timeout=0.5); sys.exit(0 if r.status==200 else 1)' 2>/dev/null; then
                return 0
            fi
            sleep 0.1
        done
        cat "$log_file" >&2
        echo "FAIL: stream-sync did not start on 127.0.0.1:8080" >&2
        exit 1
    }

    cat > "$R/etc/hosts" << 'HOSTS'
127.0.0.1 localhost
127.0.0.99 time.google.com time1.google.com time2.google.com time3.google.com time4.google.com
127.0.0.99 ntp.aliyun.com
127.0.0.10 ntp.tencent.com time.apple.com time.cloudflare.com pool.ntp.org
127.0.0.10 api.inmusicbrands.com
HOSTS

    # Start local RFC 5905 NTPv4 responder on 127.0.0.10:123 serving TARGET_EPOCH
    python3 -I "$W/ntp_responder.py" 127.0.0.10 123 "$TARGET_EPOCH" "$W/ntp-hits.log" &
    NTP_PID=$!
    # Start local HTTPS GeoIP timezone responder on 127.0.0.10:443 for api.inmusicbrands.com/v1/timezone
    printf 'America/New_York' > "$W/tz-api-response.txt"
    python3 -I "$W/tz_https_server.py" 127.0.0.10 443 "$W/server.crt" "$W/server.key" "$W/tz-api-response.txt" "$W/tz-api-hits.log" &
    HTTPS_PID=$!
    SS_PID=
    trap 'kill "$NTP_PID" "$HTTPS_PID" ${SS_PID:-} 2>/dev/null || :' EXIT
    for _ in $(seq 1 30); do [ -f "$W/ntp-ready" ] && [ -f "$W/https-ready" ] && break; sleep 0.1; done
    [ -f "$W/ntp-ready" ] && [ -f "$W/https-ready" ] || { echo "FAIL: local NTP/HTTPS responders did not start" >&2; exit 1; }

    # --- Phase 0: Control check (stock Google-only config + no clock file fails sync & starts before BUILD_EPOCH) ---
    rm -f "$R/etc/systemd/timesyncd.conf.d/10-enginex-ntp.conf"
    rm -f "$R/var/lib/systemd/timesync/clock"
    setup_volatile 5
    mkdir -p "$R/var/lib/systemd/timesync"
    chown -R 994:992 "$R/var/lib/systemd/timesync"
    timeout 3 chroot "$R" /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin \
        LD_PRELOAD=/run/vclock/vclock_shim.so VCLOCK_FILE=/run/vclock/state SYSTEMD_LOG_LEVEL=debug \
        /usr/lib/systemd/systemd-timesyncd > "$W/timesyncd-stock.log" 2>&1 || :
    STOCK_CLK=$(read_vclock)
    if [ -e "$R/run/systemd/timesync/synchronized" ] || [ "$STOCK_CLK" -ge "$BUILD_EPOCH" ]; then
        echo "FAIL: stock Google-only timesyncd unexpectedly synced or reached build epoch ($STOCK_CLK)" >&2
        exit 1
    fi
    echo "PASS 0: control confirmed stock Google-only timesyncd fails when Google NTP is unreachable and starts at $STOCK_CLK (< build $BUILD_EPOCH)"

    # --- Phase 1a: Install drop-in + run sc5000-custom-etc-reset -> verify drop-in survival + build-time clock floor + daemon monotonic advance ---
    mkdir -p "$R/etc/systemd/timesyncd.conf.d"
    cp "$REPO/rootfs-overlay/etc/systemd/timesyncd.conf.d/10-enginex-ntp.conf" "$R/etc/systemd/timesyncd.conf.d/10-enginex-ntp.conf"
    mkdir -p "$DATA/system/etc/overlay/systemd/timesyncd.conf.d"
    printf '[Time]\nFallbackNTP=time1.google.com\n' > "$DATA/system/etc/overlay/systemd/timesyncd.conf"
    printf '[Time]\nFallbackNTP=time1.google.com\n' > "$DATA/system/etc/overlay/systemd/timesyncd.conf.d/10-enginex-ntp.conf"
    rm -f "$DATA/system/var/overlay/lib/systemd/timesync/clock"

    chroot "$R" /bin/sh -s < "$W/etc-reset.sh"
    [ ! -e "$DATA/system/etc/overlay/systemd/timesyncd.conf" ]
    [ ! -e "$DATA/system/etc/overlay/systemd/timesyncd.conf.d" ]
    cmp -s "$REPO/rootfs-overlay/etc/systemd/timesyncd.conf.d/10-enginex-ntp.conf" "$R/etc/systemd/timesyncd.conf.d/10-enginex-ntp.conf"
    # Verify etc-reset did NOT create or seed /var/lib/systemd/timesync/clock
    [ ! -e "$DATA/system/var/overlay/lib/systemd/timesync/clock" ]

    # Verify sc5000-timesync-seed skips missing lib_dir without creating it, then seeds /var/lib/systemd/timesync/clock
    SC5000_TIMESYNC_LIB_DIR=/tmp/nonexistent-var-lib chroot "$R" /bin/sh -s < "$REPO/rootfs-overlay/usr/libexec/enginex-timesync-seed"
    [ ! -e "$R/tmp/nonexistent-var-lib" ]
    chroot "$R" /bin/sh -s < "$REPO/rootfs-overlay/usr/libexec/enginex-timesync-seed"

    FLOOR_MTIME=$(stat -c %Y "$DATA/system/var/overlay/lib/systemd/timesync/clock")
    [ "$FLOOR_MTIME" -eq "$BUILD_EPOCH" ] || {
        echo "FAIL: sc5000-timesync-seed seeded clock mtime=$FLOOR_MTIME, expected BUILD_EPOCH=$BUILD_EPOCH" >&2
        exit 1
    }

    # Verify offline first boot in China (with 127.0.0.10 temporarily blackholed) starts at BUILD_EPOCH (2026-10-30), NOT 2025 TIME_EPOCH
    ip addr del 127.0.0.10/8 dev lo 2>/dev/null || :
    ip route add blackhole 127.0.0.10/32
    setup_volatile 5
    timeout 2 chroot "$R" /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin \
        LD_PRELOAD=/run/vclock/vclock_shim.so VCLOCK_FILE=/run/vclock/state SYSTEMD_LOG_LEVEL=debug \
        /usr/lib/systemd/systemd-timesyncd > "$W/timesyncd-floor.log" 2>&1 || :
    FLOOR_VCLK=$(read_vclock)
    [ "$FLOOR_VCLK" -ge "$BUILD_EPOCH" ] || {
        cat "$W/timesyncd-floor.log" >&2
        echo "FAIL: first offline boot started at $FLOOR_VCLK (< BUILD_EPOCH $BUILD_EPOCH)" >&2
        exit 1
    }

    # Verify real ARM stream-sync advances persistent clock mtime monotonically without Engine running
    start_stream_sync "$W/streamsync-floor.log"
    DAEMON_MTIME=$(stat -c %Y "$DATA/system/var/overlay/lib/systemd/timesync/clock")
    kill "$SS_PID" 2>/dev/null || :
    wait "$SS_PID" 2>/dev/null || :
    SS_PID=
    [ "$DAEMON_MTIME" -gt "$BUILD_EPOCH" ] || {
        echo "FAIL: stream-sync did not advance clock mtime past BUILD_EPOCH ($DAEMON_MTIME <= $BUILD_EPOCH)" >&2
        exit 1
    }
    ip route del blackhole 127.0.0.10/32 2>/dev/null || :
    ip addr add 127.0.0.10/8 dev lo 2>/dev/null || :
    echo "PASS 1a: 10-enginex-ntp.conf survived etc-reset, clock floor seeded to build time ($FLOOR_MTIME -> offline boot vclock=$FLOOR_VCLK), and stream-sync advanced clock monotonically ($DAEMON_MTIME)"

    # --- Phase 1b: Sync clock from non-Google NTP (ntp.tencent.com failover past unreachable ntp.aliyun.com & Google) ---
    setup_volatile 5
    chroot "$R" /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin \
        LD_PRELOAD=/run/vclock/vclock_shim.so VCLOCK_FILE=/run/vclock/state SYSTEMD_LOG_LEVEL=debug \
        /usr/lib/systemd/systemd-timesyncd > "$W/timesyncd-boot1.log" 2>&1 &
    TS_PID=$!
    for _ in $(seq 1 80); do
        [ -e "$R/run/systemd/timesync/synchronized" ] && break
        sleep 0.1
    done
    if [ ! -e "$R/run/systemd/timesync/synchronized" ] || ! grep -q "Initial clock synchronization to" "$W/timesyncd-boot1.log"; then
        cat "$W/timesyncd-boot1.log" >&2
        echo "FAIL: timesyncd did not synchronize from non-Google server" >&2
        exit 1
    fi
    BOOT1_CLK=$(read_vclock)
    [ "$BOOT1_CLK" -ge "$TARGET_EPOCH" ] || { echo "FAIL: virtual clock $BOOT1_CLK < $TARGET_EPOCH" >&2; exit 1; }
    [ -s "$W/ntp-hits.log" ] || { echo "FAIL: local NTP responder received no packets" >&2; exit 1; }
    CLOCK_MTIME=$(stat -c %Y "$DATA/system/var/overlay/lib/systemd/timesync/clock")
    [ "$CLOCK_MTIME" -ge "$TARGET_EPOCH" ] || { echo "FAIL: persistent clock mtime $CLOCK_MTIME < $TARGET_EPOCH" >&2; exit 1; }

    # Start stream-sync and verify it NEVER moves a newer persistent clock mtime backwards
    start_stream_sync "$W/streamsync-boot1.log"
    AFTER_SS_MTIME=$(stat -c %Y "$DATA/system/var/overlay/lib/systemd/timesync/clock")
    [ "$AFTER_SS_MTIME" -ge "$TARGET_EPOCH" ] || {
        echo "FAIL: stream-sync moved clock mtime backwards ($AFTER_SS_MTIME < $TARGET_EPOCH)" >&2
        exit 1
    }
    echo "PASS 1b: clock synced from non-Google server (ntp.tencent.com failover; vclock=$BOOT1_CLK, clock_mtime=$AFTER_SS_MTIME, never regressed by stream-sync)"

    # --- Phase 1c & 1d: Real Engine + UI pack + stream-sync (no C timezone hook):
    #     1c: Auto-detected zone via HTTPS https://api.inmusicbrands.com/v1/timezone ("America/New_York")
    #     1d: Manual zone change ("Asia/Shanghai") followed IMMEDIATELY by SIGKILL ("power cut") ---
    cp -a "$W/packs/en" "$DATA/enginex/ui/packs/"
    mkdir -p "$DATA/enginex/ui/packs/en/Skins/Common/Items"
    cp "$W/probe-boot1.qml" "$DATA/enginex/ui/packs/en/Skins/Common/Items/QmlWarnings.qml"
    printf 'lang=en\n' > "$DATA/enginex/ui/settings.conf"
    printf '{"TIDAL": "{\\"provider\\":\\"TIDAL\\",\\"providerType\\":\\"TIDAL\\",\\"accessToken\\":\\"enginex\\",\\"refreshToken\\":\\"enginex\\"}"}\n' > "$DATA/StreamingAccounts.json"
    rm -f "$DATA/enginex/state/timezone"

    chroot "$R" /usr/bin/dbus-daemon --system --fork --nopidfile > "$W/dbus1.log" 2>&1 || chroot "$R" /usr/bin/dbus-daemon --system --fork
    chroot "$R" /usr/bin/env -i PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
        HOME=/tmp/sc5000-engine-home MESA_SHADER_CACHE_MAX_SIZE=32M QML_DISABLE_DISK_CACHE=1 \
        QT_QPA_PLATFORM=offscreen MALLOC_ARENA_MAX=1 LD_LIBRARY_PATH=/usr/qt/lib \
        VCLOCK_FILE=/run/vclock/state LD_PRELOAD=/run/vclock/vclock_shim.so:/etc/sc5000/libengine-ui-hook.so \
        /usr/Engine/Engine -d0 > "$W/engine-boot1.log" 2>&1 &
    ENG1_PID=$!

    # Wait for auto-detected zone ("America/New_York") to be captured by Main.qml -> stream-sync -> /data/enginex/state/timezone
    AUTODETECT_OK=0
    for _ in $(seq 1 300); do
        if [ "$(cat "$DATA/enginex/state/timezone" 2>/dev/null || :)" = "America/New_York" ]; then
            AUTODETECT_OK=1
            break
        fi
        if ! kill -0 "$ENG1_PID" 2>/dev/null; then break; fi
        sleep 0.1
    done
    if [ "$AUTODETECT_OK" != 1 ] || ! grep -q "GET /v1/timezone" "$W/tz-api-hits.log"; then
        tail -n 60 "$W/engine-boot1.log" >&2
        echo "FAIL: auto-detected timezone America/New_York was not captured to /data/enginex/state/timezone" >&2
        exit 1
    fi
    AUTO_LOG=$(grep -a "TimeZone detected:" "$W/engine-boot1.log" | tail -n 1 | tr -d "\r" || :)
    echo "PASS 1c: auto-detected timezone America/New_York (from HTTPS api.inmusicbrands.com/v1/timezone, display 'America/New York') captured via QML + stream-sync into /data/enginex/state/timezone [${AUTO_LOG:-ok}]"

    # Now wait for manual setSystemTimeZone("Asia/Shanghai") to hit /data/enginex/state/timezone and SIGKILL Engine IMMEDIATELY
    MANUAL_OK=0
    for _ in $(seq 1 400); do
        if [ "$(cat "$DATA/enginex/state/timezone" 2>/dev/null || :)" = "Asia/Shanghai" ]; then
            # Immediate hard power cut (SIGKILL) right after the zone change!
            kill -9 "$ENG1_PID" "$TS_PID" "$SS_PID" 2>/dev/null || :
            chroot "$R" /bin/sh -c 'kill -9 $(pidof Engine systemd-timesyncd stream-sync sc5000-suite dbus-daemon) 2>/dev/null || :' || :
            MANUAL_OK=1
            break
        fi
        if ! kill -0 "$ENG1_PID" 2>/dev/null; then break; fi
        sleep 0.02
    done
    wait "$ENG1_PID" 2>/dev/null || :
    wait "$TS_PID" 2>/dev/null || :
    wait "$SS_PID" 2>/dev/null || :
    SS_PID=

    if [ "$MANUAL_OK" != 1 ]; then
        tail -n 60 "$W/engine-boot1.log" >&2
        echo "FAIL: manual timezone Asia/Shanghai was not captured before SIGKILL" >&2
        exit 1
    fi
    [ "$(cat "$DATA/enginex/state/timezone")" = "Asia/Shanghai" ]
    QCONF="$DATA/.config/jp07.user.settings/AIR Music Technology/Engine.conf"
    ENGINE_CONF_STATE="missing/unflushed (as expected on SIGKILL)"
    if [ -f "$QCONF" ] && grep -Fqx "TZEnvVar=Asia/Shanghai" "$QCONF"; then
        ENGINE_CONF_STATE="already flushed"
    fi
    echo "PASS 1d: manual timezone Asia/Shanghai survived immediate SIGKILL right after zone change (/data/enginex/state/timezone=Asia/Shanghai; Engine.conf before reboot: $ENGINE_CONF_STATE)"

    # --- Phase 2: Simulated power cycle -> offline boot (no network, RTC reset to 1970, auto-detect failing + hostile UTC fallback) ---
    kill "$NTP_PID" "$HTTPS_PID" 2>/dev/null || :
    wait "$NTP_PID" 2>/dev/null || :
    wait "$HTTPS_PID" 2>/dev/null || :
    ip addr del 127.0.0.10/8 dev lo 2>/dev/null || :
    ip route add blackhole 127.0.0.10/32

    setup_volatile 5
    [ "$(read_vclock)" -eq 5 ]

    # 2a. Offline timesyncd restores clock from /var/lib/systemd/timesync/clock (>= TARGET_EPOCH)
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

    # 2b. Run sc5000-custom-prepare before Engine starts -> restores [TimeDateSettings] TZEnvVar=Asia/Shanghai into Engine.conf!
    chroot "$R" /bin/sh -s < "$W/prepare.sh"
    [ -f "$QCONF" ] || { echo "FAIL: sc5000-custom-prepare did not create $QCONF" >&2; exit 1; }
    grep -Fqx "[TimeDateSettings]" "$QCONF"
    grep -Fqx "TZEnvVar=Asia/Shanghai" "$QCONF"

    cp "$W/probe-boot2.qml" "$DATA/enginex/ui/packs/en/Skins/Common/Items/QmlWarnings.qml"
    rm -f "$DATA/enginex/ui/safe-mode" "$DATA/enginex/ui/boot-count" "$DATA/enginex/ui/boot-token"
    start_stream_sync "$W/streamsync-boot2.log"
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
    kill -9 "$ENG2_PID" "$SS_PID" 2>/dev/null || :
    chroot "$R" /bin/sh -c 'kill -9 $(pidof Engine stream-sync sc5000-suite dbus-daemon) 2>/dev/null || :' || :
    wait "$ENG2_PID" 2>/dev/null || :
    wait "$SS_PID" 2>/dev/null || :
    SS_PID=

    if ! grep -aq 'TSPROBE BOOT2_FALLBACK_GUARDED|tz=Asia/Shanghai|utc=false|offset=-480|' "$W/engine-boot2.log"; then
        tail -n 60 "$W/engine-boot2.log" >&2
        echo "FAIL: Asia/Shanghai did not survive offline boot / failed auto-detect / boot-window UTC fallback" >&2
        exit 1
    fi
    if ! grep -aq 'TSPROBE BOOT2_PASS|tz=UTC|utc=true|offset=0|' "$W/engine-boot2.log"; then
        tail -n 60 "$W/engine-boot2.log" >&2
        echo "FAIL: user-chosen UTC via picker was not respected" >&2
        exit 1
    fi
    [ "$(cat "$DATA/enginex/state/timezone")" = "UTC" ]

    # Also prove restore_timezone does NOT overwrite a newer non-UTC TZEnvVar written by Engine on clean shutdown
    printf '[TimeDateSettings]\nTZEnvVar=Europe/London\n' > "$QCONF"
    touch -d '+10 seconds' "$QCONF" 2>/dev/null || sleep 1 && touch "$QCONF"
    chroot "$R" /bin/sh -s < "$W/prepare.sh"
    grep -Fqx "TZEnvVar=Europe/London" "$QCONF"

    # Re-seed Asia/Shanghai with newer mtime for Phase 3 Gen2 test
    printf 'Asia/Shanghai\n' > "$DATA/enginex/state/timezone"
    echo "PASS 2b: boot prepare restored TZEnvVar=Asia/Shanghai; offline boot guarded against unsolicited boot UTC fallback, respected explicit picker-chosen UTC (/data/enginex/state/timezone=UTC), and preserved newer Engine.conf TZEnvVar=Europe/London"

    # --- Phase 3: Gen2 /data bundle mechanism (/run/systemd/timesyncd.conf.d/10-enginex-ntp.conf + clock floor + Engine.conf restore) ---
    rm -f "$R/etc/systemd/timesyncd.conf.d/10-enginex-ntp.conf"
    rm -f "$R/var/lib/systemd/timesync/clock" "$QCONF" "$W/ntp-ready"
    ip route del blackhole 127.0.0.10/32 2>/dev/null || :
    ip addr add 127.0.0.10/8 dev lo 2>/dev/null || :
    setup_volatile 5
    chroot "$R" /bin/sh -s < "$W/gen2-boot-activate.sh"
    [ -f "$R/run/systemd/timesyncd.conf.d/10-enginex-ntp.conf" ]
    [ "$(stat -c %Y "$R/var/lib/systemd/timesync/clock")" -eq "$BUILD_EPOCH" ]
    grep -Fqx "TZEnvVar=Asia/Shanghai" "$QCONF"

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
    { [ -e "$R/run/systemd/timesync/synchronized" ] && grep -q "Initial clock synchronization to" "$W/timesyncd-gen2.log"; } || { cat "$W/timesyncd-gen2.log" >&2; echo "FAIL: Gen2 /run drop-in did not sync" >&2; exit 1; }
    [ "$(read_vclock)" -ge "$TARGET_EPOCH" ]
    echo "PASS 3: Gen2 boot-activate.sh seeded clock floor, restored TZEnvVar=Asia/Shanghai to Engine.conf, and synced via /run/systemd/timesyncd.conf.d/10-enginex-ntp.conf"
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

check_external_http_clock() {
    local phase=$1
    python3 -I - "$phase" << 'HTTPPY'
import email.utils, ssl, sys, time, urllib.request

phase = sys.argv[1]
urls = [
    "https://1.1.1.1",
    "https://www.apple.com",
    "https://api.inmusicbrands.com/v1/timezone",
]
last_err = None
for u in urls:
    try:
        req = urllib.request.Request(u, method="HEAD", headers={"User-Agent": "timesync-check/1.0"})
        t0 = time.time()
        with urllib.request.urlopen(req, timeout=5, context=ssl.create_default_context()) as resp:
            hdr = resp.headers.get("Date")
        t1 = time.time()
        if not hdr:
            continue
        ext_ts = email.utils.parsedate_to_datetime(hdr).timestamp()
        host_mid = (t0 + t1) / 2.0
        skew = host_mid - ext_ts
        if abs(skew) > 2.0:
            sys.stderr.write(
                f"FAIL [{phase}]: host clock ({host_mid:.3f}) differs from {u} HTTP Date ({hdr} -> {ext_ts:.3f}) by {skew:+.3f}s (limit +-2.0s)\n"
            )
            sys.exit(1)
        print(f"{phase}: host={host_mid:.3f} ext={ext_ts:.0f} ({u}) skew={skew:+.2f}s (within +-2s)")
        sys.exit(0)
    except Exception as exc:
        last_err = exc
sys.stderr.write(f"FAIL [{phase}]: could not verify host clock against external HTTP Date: {last_err}\n")
sys.exit(1)
HTTPPY
}

echo "0/4 Verifying host clock against external HTTP Date before sandbox..."
check_external_http_clock "BEFORE"
HOST_BEFORE=$(date +%s)

echo "1/4 Unpacking SC5000 5.3.0 rootfs, building UI pack and ARM stream-sync..."
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

(cd "$REPO" && GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -o "$W/sc5000-suite" ./cmd/enginex-suite)

python3 -I - "$REPO" "$W" << 'PY'
import importlib.util, pathlib, sys
repo, w = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2])
spec = importlib.util.spec_from_file_location("az01_packer", repo / "tools/az01-packer/az01_packer.py")
az = importlib.util.module_from_spec(spec); spec.loader.exec_module(az)
managed = sorted(p.relative_to(repo / "rootfs-overlay/etc").as_posix()
                 for p in (repo / "rootfs-overlay/etc").rglob("*") if p.is_file())
(w / "etc-reset.sh").write_bytes(az.etc_reset("5.3.0.4-sandbox", managed))
(w / "prepare.sh").write_bytes(az.PREPARE)

bspec = importlib.util.spec_from_file_location("build_data_bundle", repo / "tools/az01-packer/build-data-bundle.py")
bdb = importlib.util.module_from_spec(bspec); bspec.loader.exec_module(bdb)
ntp = (repo / "rootfs-overlay/etc/systemd/timesyncd.conf.d/10-enginex-ntp.conf").read_text()
ba = bdb.BOOT_ACTIVATE.replace("@MIGRATE_IF_DEFAULT@", bdb.MIGRATE_IF_DEFAULT).replace("@TIMESYNCD_DROPIN@", ntp)
# Stop before daemon startup at the bottom of boot-activate.sh; we start timesyncd under vclock_shim below.
ba = ba.split("# Sticks mounted before this ran")[0] + "\nrestore_gen2_timezone 0 || :\n"
(w / "gen2-boot-activate.sh").write_text(ba)
PY

echo "2/4 Building user-space virtual RTC fixture and local TLS GeoIP fixture..."
cat > "$W/vclock_shim.c" << 'CEOF'
#define _GNU_SOURCE
#include <dlfcn.h>
#include <errno.h>
#include <fcntl.h>
#include <linux/capability.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <sys/time.h>
#include <sys/timex.h>
#include <time.h>
#include <unistd.h>
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

# Generate ephemeral test CA + TLS cert for api.inmusicbrands.com (valid 2026-10-01 .. 2028-01-01)
cat > "$W/openssl.cnf" << 'CNF'
[req]
distinguished_name = dn
x509_extensions = v3_ca
prompt = no
[dn]
CN = Sandbox GeoIP Test CA
[v3_ca]
basicConstraints = critical, CA:true
keyUsage = critical, keyCertSign, cRLSign
[v3_srv]
basicConstraints = CA:false
keyUsage = critical, digitalSignature, keyEncipherment
extendedKeyUsage = serverAuth
subjectAltName = DNS:api.inmusicbrands.com, IP:127.0.0.10
CNF
openssl genrsa -out "$W/ca.key" 2048 2>/dev/null
openssl req -new -x509 -key "$W/ca.key" -out "$W/ca.crt" -days 900 -config "$W/openssl.cnf" -extensions v3_ca 2>/dev/null
openssl genrsa -out "$W/server.key" 2048 2>/dev/null
openssl req -new -key "$W/server.key" -subj "/CN=api.inmusicbrands.com" -out "$W/server.csr" 2>/dev/null
openssl x509 -req -in "$W/server.csr" -CA "$W/ca.crt" -CAkey "$W/ca.key" -CAcreateserial \
    -out "$W/server.crt" -days 800 -extfile "$W/openssl.cnf" -extensions v3_srv 2>/dev/null

cat > "$W/tz_https_server.py" << 'PY'
import http.server, pathlib, ssl, sys
host, port, cert, key, resp_file, log_file = sys.argv[1], int(sys.argv[2]), sys.argv[3], sys.argv[4], sys.argv[5], sys.argv[6]

class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        with open(log_file, "a") as f:
            f.write(f"GET {self.path}\n")
        body = pathlib.Path(resp_file).read_bytes()
        self.send_response(200)
        self.send_header("Content-Type", "text/plain; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *args):
        pass

httpd = http.server.HTTPServer((host, port), Handler)
ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
ctx.load_cert_chain(certfile=cert, keyfile=key)
httpd.socket = ctx.wrap_socket(httpd.socket, server_side=True)
(pathlib.Path(log_file).parent / "https-ready").write_text("1\n")
httpd.serve_forever()
PY

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
    orig = pkt[40:48]
    resp = struct.pack("!BBBbIII", 0x24, 2, 6, -20, 16, 16, 0x434E5450)
    resp += struct.pack("!II", ntp_sec - 1, 0)
    resp += orig
    resp += struct.pack("!II", ntp_sec, 0)
    resp += struct.pack("!II", ntp_sec, 1000)
    sock.sendto(resp, addr)
    with open(log_path, "a") as f:
        f.write(f"{addr[0]}:{addr[1]}\n")
PY

# Boot 1 probe: ONLY triggers stock detectTimeZone() (auto-detect) and then stock
# setSystemTimeZone("Asia/Shanghai") (manual). It does NOT call stream-sync itself —
# Main.qml's SERVICES_MAIN Connections handler captures both signals and posts to stream-sync!
cat > "$W/probe-boot1.qml" << 'QML'
import QtQuick 2.12
import Planck 1.0
Item {
    visible: false
    property int phase: 0
    Timer {
        interval: 300; running: true; repeat: true
        onTriggered: {
            if (phase === 0) {
                phase = 1
                const autoset = Planck.getProperty("/Client/Preferences/TimeZoneAutoset")
                if (autoset && autoset.translator) {
                    autoset.translator.state = false
                    autoset.translator.state = true
                }
            } else if (phase === 1) {
                const tz = Planck.timeDateController.getSystemTimeZone()
                if (tz === "America/New York") {
                    console.warn("TSPROBE AUTODETECT_PASS|tz=" + tz + "|utc=" + Planck.timeDateController.isSystemTimeZoneUTC() + "|")
                    phase = 2
                }
            } else if (phase === 2) {
                phase = 3
                // Wait one 300ms tick so outer harness observes America/New_York in /data/enginex/state/timezone,
                // then perform manual zone selection; outer harness SIGKILLs Engine immediately!
            } else if (phase === 3) {
                phase = 4
                Planck.timeDateController.setSystemTimeZone("Asia/Shanghai")
                console.warn("TSPROBE MANUAL_TRIGGERED|tz=" + Planck.timeDateController.getSystemTimeZone() + "|")
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
        interval: 600; running: true; repeat: true
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
                // 1) Trigger failed network auto-detect AND unsolicited boot-window UTC fallback (picker NOT used)
                const autoset = Planck.getProperty("/Client/Preferences/TimeZoneAutoset")
                if (autoset && autoset.translator) {
                    autoset.translator.state = false
                    autoset.translator.state = true
                }
                Planck.timeDateController.setSystemTimeZone("UTC")
            } else if (step === 1) {
                step = 2
                const tz = Planck.timeDateController.getSystemTimeZone()
                const isUtc = Planck.timeDateController.isSystemTimeZoneUTC()
                const off = new Date(1805112000000).getTimezoneOffset()
                console.warn("TSPROBE BOOT2_FALLBACK_GUARDED|tz=" + tz + "|utc=" + isUtc + "|offset=" + off + "|")
                // 2) Now open the Time Zone picker (/GUI/ShowTimeZoneView) and explicitly choose UTC!
                const showPicker = Planck.getProperty("/GUI/ShowTimeZoneView")
                if (showPicker && showPicker.translator) showPicker.translator.state = true
                Planck.timeDateController.setSystemTimeZone("UTC")
                if (showPicker && showPicker.translator) showPicker.translator.state = false
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
setpriv --inh-caps=-sys_time --bounding-set=-sys_time unshare -m -n -p -f --kill-child "$SELF" --inner "$W"

echo "4/4 Verifying host clock against external HTTP Date after sandbox..."
check_external_http_clock "AFTER"
HOST_AFTER=$(date +%s)
DIFF=$((HOST_AFTER - HOST_BEFORE))
if [ "$DIFF" -lt 0 ] || [ "$DIFF" -gt 300 ]; then
    echo "FAIL: host clock drifted unexpectedly ($HOST_BEFORE -> $HOST_AFTER)" >&2
    exit 1
fi
echo "TIMESYNC_SANDBOX_PASS (CAP_SYS_TIME=0 enforced; host clock within +-2s of external HTTP Date before & after; elapsed ${DIFF}s)"
