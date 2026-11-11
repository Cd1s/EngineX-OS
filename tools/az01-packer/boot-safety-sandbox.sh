#!/bin/bash
# Real-image boot recovery test. Root on an ARM build host; never on a deck.
# Usage: boot-safety-sandbox.sh <candidate image> <new work directory> [ext4|vfat|exfat|ntfs3] [hang]
set -euo pipefail
SELF=$(realpath "$0")
REPO=$(cd "$(dirname "$SELF")/../.." && pwd)
if [ "${1:-}" != --inner ]; then
    [ "$#" -ge 2 ] && [ "$#" -le 4 ] && [ "$(id -u)" = 0 ] || exit 2
    REPORT_FS=ext4
    [ "$#" -lt 3 ] || REPORT_FS=$3
    case $REPORT_FS in ext4|vfat|exfat|ntfs3) ;; *) exit 2 ;; esac
    export REPORT_FS
    scenarios='hang normal'
    if [ "$#" = 4 ]; then [ "$4" = hang ] || exit 2; scenarios=hang; fi
    image=$(realpath "$1")
    export W=$(realpath -m "$2")
    [ ! -e "$W" ] || { echo 'Use a new work directory' >&2; exit 2; }
    case $(basename "$image") in *-JP07-Update.img) export PRODUCT=JP07 ;; *-JC11-Update.img) export PRODUCT=JC11 ;; *) exit 2 ;; esac
    mkdir -p "$W"
    python3 "$REPO/tools/az01-packer/az01_packer.py" unpack "$image" --output "$W/image" > "$W/unpack.json"
    # Deliberately block the real Engine before main(), after loading its normal
    # libraries. This test-only library never goes into an image or bundle.
    cat > "$W/hang.c" <<'C'
#include <unistd.h>
__attribute__((constructor)) static void before_ready(void) { for (;;) sleep(60); }
C
    arm-linux-gnueabihf-gcc -shared -fPIC -o "$W/hang.so" "$W/hang.c"
    owned_loops=()
    cleanup_loops() { for device in "${owned_loops[@]}"; do losetup -d "$device" 2>/dev/null || :; done; }
    trap cleanup_loops EXIT
    for scenario in $scenarios; do
        mkdir -p "$W/$scenario"
        for disk in usb late; do
            truncate -s 64M "$W/$scenario/$disk.img"
            case $REPORT_FS in
                ext4) mkfs.ext4 -q -F "$W/$scenario/$disk.img" ;;
                vfat) mkfs.vfat -F 32 "$W/$scenario/$disk.img" ;;
                exfat) mkfs.exfat "$W/$scenario/$disk.img" ;;
                ntfs3) mkntfs -F -Q "$W/$scenario/$disk.img" ;;
            esac
            device=$(losetup --find --show "$W/$scenario/$disk.img")
            owned_loops+=("$device")
            stat -c '%t %T' "$device" > "$W/$scenario/$disk.dev"
        done
        unshare -m -n -p -f --kill-child "$SELF" --inner "$scenario"
        cleanup_loops
        owned_loops=()
    done
    echo 'PASS real-image hang -> stock, USB and hot-insertion report, public notice, normal UI-ready boot' | tee "$W/result.txt"
    exit
fi
C=$W/$2
R=$C/root
mkdir -p "$R" "$C/data" "$C/usb" "$C/late"
mount --make-rprivate /
ip link set lo up
mount -o loop "$W/image/rootfs.ext2" "$R"
for d in tmp run dev sys proc; do mount -t tmpfs tmpfs "$R/$d"; done
mount --bind "$C/data" "$R/data"
mkdir -p "$R/dev/shm" "$R/dev/pts" "$R/run/gate" "$R/run/dbus" "$R/tmp/home" \
    "$R/sys/firmware/devicetree/base" "$R/sys/devices/system/cpu"
for n in null zero full random urandom; do touch "$R/dev/$n"; mount --bind "/dev/$n" "$R/dev/$n"; done
printf '%s' "$PRODUCT" > "$R/sys/firmware/devicetree/base/inmusic,product-code"
printf 'inmusic,%s\0rockchip,rk3288\0' "${PRODUCT,,}" > "$R/sys/firmware/devicetree/base/compatible"
for n in online possible present; do echo 0-3 > "$R/sys/devices/system/cpu/$n"; done
cat > "$R/run/gate/systemctl" <<'SH'
#!/bin/sh
echo "$*" >> /tmp/systemctl.log
case $1 in
    is-system-running) echo running ;;
    restart) touch /tmp/fallback-restart ;;
esac
SH
cat > "$R/run/gate/blocked" <<'SH'
#!/bin/sh
echo "BLOCKED $0 $*" >> /tmp/blocked.log
exit 0
SH
cat > "$R/run/gate/journalctl" <<'SH'
#!/bin/sh
echo '{"MESSAGE":"fixture accessToken=test-secret","_SYSTEMD_UNIT":"engine.service","__REALTIME_TIMESTAMP":"1794390000000000"}'
echo '{"MESSAGE":"Engine boot test: waiting for UI ready","_SYSTEMD_UNIT":"engine.service","__REALTIME_TIMESTAMP":"1794390000000001"}'
SH
cat > "$R/run/gate/dmesg" <<'SH'
#!/bin/sh
echo '[ 1.000] sandbox kernel: no physical devices exposed; peer 198.51.100.2'
SH
chmod +x "$R/run/gate/"*
# Block the vendor's absolute systemctl path as well as PATH lookups.
if [ -f "$R/usr/bin/systemctl" ]; then mount --bind "$R/run/gate/systemctl" "$R/usr/bin/systemctl"; fi
for tool in systemctl journalctl dmesg; do
    mkdir -p "$R/usr/local/sbin"
    cp "$R/run/gate/$tool" "$R/usr/local/sbin/$tool"
    target=$(chroot "$R" /bin/sh -c 'command -v "$1"' sh "$tool")
    target=$(chroot "$R" /bin/readlink -f "$target")
    # BusyBox applets share an inode: replace the applet path in our private
    # image instead of bind-mounting over the shared BusyBox executable.
    if [ -L "$R$target" ] || [ "${target##*/}" = busybox ]; then
        mkdir -p "$R/usr/local/sbin"
        cp "$R/run/gate/$tool" "$R/usr/local/sbin/$tool"
    else
        mount --bind "$R/run/gate/$tool" "$R$target"
    fi
done
for f in /sbin/reboot /usr/sbin/kexec /usr/sbin/az01-update /usr/Engine/Scripts/encrypt-fs.sh /usr/Engine/Reporter; do
    if [ -L "$R$f" ]; then
        rm "$R$f"
        cp "$R/run/gate/blocked" "$R$f"
    elif [ -f "$R$f" ]; then
        mount --bind "$R/run/gate/blocked" "$R$f"
    fi
done
mkdir -p "$R/proc/sys/net/core" "$R/proc/sys/net/ipv4" "$R/proc/sys/vm" "$R/proc/sys/fs"
for k in net/core/rmem_max net/core/wmem_max net/ipv4/tcp_rmem net/ipv4/tcp_wmem vm/swappiness fs/file-max; do echo 0 > "$R/proc/sys/$k"; done
in_root() { chroot "$R" /usr/bin/env -i PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin "$@"; }
in_root /usr/libexec/sc5000-custom-etc-reset > "$C/prepare.log" 2>&1
in_root /usr/libexec/sc5000-custom-prepare >> "$C/prepare.log" 2>&1
printf 'lang=en\n' > "$C/data/enginex/ui/settings.conf"
in_root /etc/sc5000/engine-ui-guard pre >> "$C/prepare.log" 2>&1
umount "$R/proc"
mount -t proc proc "$R/proc"
printf '           CPU0\n 41:        100     GICv2  32 Level     ttyS0\n' > "$R/run/gate/interrupts"
mount --bind "$R/run/gate/interrupts" "$R/proc/interrupts"
mount -t tmpfs tmpfs "$R/proc/irq"
mkdir -p "$R/proc/irq/41"
echo f > "$R/proc/irq/41/smp_affinity"; echo 0-3 > "$R/proc/irq/41/smp_affinity_list"
mkdir -p "$R/media/USB-Test"
mkdir -p "$R/sys/class/block"
# Real disposable filesystems, with fake USB identities in this namespace.
# Mount through the chroot so mountinfo names /dev/sdz1 rather than a host path.
for spec in usb:sdz late:sdy; do
    disk=${spec%:*}; name=${spec#*:}
    read -r major minor < "$C/$disk.dev"
    mknod "$R/dev/${name}1" b "$((16#$major))" "$((16#$minor))"
    mkdir -p "$R/sys/devices/platform/usb-test/block/$name"
    echo 1 > "$R/sys/devices/platform/usb-test/block/$name/removable"
    ln -s "../../devices/platform/usb-test/block/$name" "$R/sys/class/block/$name"
done
in_root /bin/mount -t "$REPORT_FS" /dev/sdz1 /media/USB-Test
cp "$W/hang.so" "$R/run/gate/hang.so"
in_root /usr/bin/dbus-daemon --system --fork --nopidfile > "$C/dbus.log" 2>&1
in_root /usr/local/bin/stream-sync --listen :8080 --cpu 3 --deemixd '' --relay-url '' > "$C/stream-sync.log" 2>&1 &
stream=$!
wait_for() { local end=$((SECONDS + $1)); shift; until "$@"; do [ "$SECONDS" -lt "$end" ] || return 1; sleep .1; done; }
wait_for 20 curl -sf -o /dev/null http://127.0.0.1:8080/boot-status
timeout=45; hook=/etc/sc5000/libengine-ui-hook.so
if [ "$2" = hang ]; then timeout=2; hook=/run/gate/hang.so:$hook; fi
in_root SC5000_BOOT_TIMEOUT=$timeout SC5000_BOOT_POLL_INTERVAL=.1 /etc/sc5000/engine-ui-guard watch > "$C/guardian.log" 2>&1 &
watcher=$!
start=$SECONDS
chroot "$R" /usr/bin/env -i PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    HOME=/tmp/home QT_QPA_PLATFORM=offscreen LD_LIBRARY_PATH=/usr/qt/lib MALLOC_ARENA_MAX=1 \
    SC5000_UI_HOOK=$hook /etc/sc5000/engine-ui-guard launch /usr/Engine/Engine -d0 > "$C/engine.log" 2>&1 &
engine=$!
if [ "$2" = hang ]; then
    wait_for 8 test -e "$R/tmp/fallback-restart"
    kill -TERM "$engine" 2>/dev/null || :
    sleep .5
    kill -KILL "$engine" 2>/dev/null || :
    wait "$engine" 2>/dev/null || :
    rm -f "$R/tmp/engine_runguard.lock"
    in_root /etc/sc5000/engine-ui-guard pre >> "$C/prepare.log" 2>&1
    chroot "$R" /usr/bin/env -i PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
        HOME=/tmp/home QT_QPA_PLATFORM=offscreen LD_LIBRARY_PATH=/usr/qt/lib MALLOC_ARENA_MAX=1 \
        /etc/sc5000/engine-ui-guard launch /usr/Engine/Engine -d0 > "$C/stock-engine.log" 2>&1 &
    engine=$!
    elapsed=$((SECONDS - start))
    [ "$elapsed" -le 10 ]
    echo "stock-start-within=${elapsed}s; test-T=${timeout}s" > "$C/timing.txt"
    sleep 2
    kill -0 "$engine"
    test -r "$R/proc/$engine/environ"
    readlink "$R/proc/$engine/exe" | grep -q '/usr/Engine/Engine$'
    python3 - "$R/proc/$engine" > "$C/stock-proof.json" <<'PY'
import json,pathlib,sys
p=pathlib.Path(sys.argv[1])
assert not any(v.startswith(b'LD_PRELOAD=') for v in (p/'environ').read_bytes().split(b'\0'))
assert b'libengine-ui-hook' not in (p/'maps').read_bytes()
print(json.dumps({'ldPreload':False,'uiHookMapped':False}))
PY
    awk '$5 ~ "^/media/" || $5 == "/" { print }' "$R/proc/self/mountinfo" > "$C/media-mountinfo.txt"
    wait_for 20 bash -c 'compgen -G "$1/EngineX-boot-report-*.txt" >/dev/null' sh "$R/media/USB-Test"
    mkdir -p "$R/media/USB-Late"
    in_root /bin/mount -t "$REPORT_FS" /dev/sdy1 /media/USB-Late
    wait_for 10 bash -c 'compgen -G "$1/EngineX-boot-report-*.txt" >/dev/null' sh "$R/media/USB-Late"
    python3 - "$R/media/USB-Test" > "$C/report-proof.json" <<'PY'
import json,pathlib,sys
reports=list(pathlib.Path(sys.argv[1]).glob('EngineX-boot-report-*.txt'))
assert reports
for report in reports:
    body=report.read_bytes()
    assert b'test-secret' not in body and b'198.51.100.2' not in body
    assert b'\nStock Engine: 5.3.0\n' in body
print(json.dumps({'redacted':True,'stockEngine':'5.3.0','reports':len(reports)}))
PY
    cp "$R/media/USB-Test/"*.txt "$C/usb/"
    cp "$R/media/USB-Late/"*.txt "$C/late/"
    curl -sf http://127.0.0.1:8080/boot-status > "$C/public-status.json"
    python3 - "$C/public-status.json" <<'PY'
import json,sys
j=json.load(open(sys.argv[1])); assert j.get('fallback') is True, j
assert not any(k in j for k in ('token','logs','accessCode'))
PY
    test -e "$C/data/enginex/ui/mods-quarantined"
else
    wait_for 40 bash -c 'test -s "$1/boot-ready" && cmp -s "$1/boot-token" "$1/boot-ready"' sh "$C/data/enginex/ui"
    echo "ui-ready-within=$((SECONDS - start))s; test-T=${timeout}s" > "$C/timing.txt"
    sleep "$timeout"
    test ! -e "$C/data/enginex/ui/safe-mode"
    kill -0 "$engine"
fi
kill -TERM "$engine" "$watcher" "$stream" 2>/dev/null || :
echo "PASS $PRODUCT $2"
