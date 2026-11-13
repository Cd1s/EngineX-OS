#!/bin/bash
# Stock Engine + the image's own edisksd against loop-image USB sticks, in a private
# mount/net/pid namespace with its own D-Bus and its own udev database. Build host only
# (aarch64 with 32-bit EL0 and armhf Mesa), as root; never on a deck. See README.md.
#   STICKS='<label> <vfat|exfat> <content dir>[\n...]' run.sh <work dir> <rootfs.ext2> [seconds]
# Leaves <work dir>/out/: engine.log (probe lines "FSP ..."), edisksd.log, edisksd-objects.txt
# (edisksd's D-Bus objects and properties), mounts.txt (mount options as edisksd chose them),
# sticks.txt and the udev records. Never touches the host's /tmp, system bus, udev or systemd.
set -euo pipefail
SELF=$(realpath "$0"); HERE=$(dirname "$SELF"); REPO=$(cd "$HERE/../.." && pwd)
W=$(realpath -m "${1:?work dir}"); ROOTFS=$(realpath "${2:?rootfs.ext2 of an unpacked official image}"); SECS=${3:-240}
PRODUCT=${PRODUCT:-JC11}; COMPAT=${COMPAT:-inmusic,jc11}; CARD=${SBX_CARD:-PRIME 4 Audio}
PROBE=${PROBE:-$HERE/probe.qml}; O=$W/out
STICKS=${STICKS:?set STICKS}
if [ "${USB_SANDBOX_INNER:-}" != 1 ]; then
    [ "$(id -u)" = 0 ] && [ "$(uname -m)" = aarch64 ] || { echo "run as root on an aarch64 build host" >&2; exit 2; }
    for t in unshare chroot losetup blkid mkfs.vfat mkfs.exfat arm-linux-gnueabihf-gcc python3; do command -v "$t" >/dev/null || { echo "missing $t" >&2; exit 2; }; done
    [ -d /usr/lib/arm-linux-gnueabihf ] || { echo "needs the host's armhf Mesa in /usr/lib/arm-linux-gnueabihf" >&2; exit 2; }
    mkdir -p "$W/bin"; rm -rf "$O"; mkdir -p "$O"
    exec 9> "$W/.lock"; flock -n 9 || { echo "$W is in use" >&2; exit 1; }
    [ -e "$W/bin/alsa-null-fixture.so" ] || arm-linux-gnueabihf-gcc -O2 -shared -fPIC -x c -o "$W/bin/alsa-null-fixture.so" "$REPO/tools/shadow-twin/alsa_null_fixture.c" -ldl
    if [ ! -e "$W/bin/libengine-ui-hook.so" ]; then
        # The UI hook as a loader only: it serves the probe from the pack dir and changes nothing in Engine.
        rm -rf "$W/loader-src"; mkdir -p "$W/loader-src"; cp "$REPO"/tools/ui-mod/*.h "$REPO/tools/ui-mod/engine_ui_hook.c" "$W/loader-src/"
        python3 -I - "$W/loader-src/engine_ui_hook.c" <<'PY'
import sys
p = sys.argv[1]; s = open(p).read()
a = '    fprintf(stderr, "engine-ui-hook: Engine %s\\n", build->name);\n'
assert s.count(a) == 1, "engine_ui_hook.c changed: update the loader-only patch"
open(p, "w").write(s.replace(a, a + '    if (getenv("SC_LOADER_ONLY")) { fprintf(stderr, "engine-ui-hook: LOADER ONLY\\n"); return; }\n'))
PY
        (cd "$W/loader-src" && arm-linux-gnueabihf-gcc -O2 -shared -fPIC -U_FORTIFY_SOURCE -D_FORTIFY_SOURCE=0 -o "$W/bin/libengine-ui-hook.so" engine_ui_hook.c -ldl -lpthread)
    fi
    export W ROOTFS SECS PRODUCT COMPAT CARD PROBE STICKS
    USB_SANDBOX_INNER=1 exec unshare -m -n -p -f --kill-child bash "$SELF" "$@"
fi

mount --make-rprivate /
ip link set lo up 2>/dev/null || :
LOOPS=()
cleanup() { for d in "${LOOPS[@]}"; do losetup -d "$d" 2>/dev/null || :; done; }
trap cleanup EXIT
R=$W/root; mkdir -p "$R" "$W/ro" "$W/ovl" "$W/stick"
mount -o loop,ro "$ROOTFS" "$W/ro"; LOOPS+=("$(findmnt -n -o SOURCE "$W/ro")")
mount -t tmpfs tmpfs "$W/ovl"; mkdir -p "$W/ovl/upper" "$W/ovl/work"
mount -t overlay overlay -o "lowerdir=$W/ro,upperdir=$W/ovl/upper,workdir=$W/ovl/work" "$R"
for d in tmp run dev data sys media; do mount -t tmpfs tmpfs "$R/$d"; done
chmod 1777 "$R/tmp"
# Deck-destructive commands refuse inside the chroot.
printf '#!/bin/sh\necho "SHIM $0 $*" >> /tmp/shim.log\nexit 1\n' > "$R/usr/bin/sbx-refuse"; chmod 755 "$R/usr/bin/sbx-refuse"
for c in /usr/bin/systemctl /sbin/reboot /usr/sbin/kexec /sbin/poweroff /sbin/shutdown /usr/sbin/az01-update /usr/Engine/Scripts/encrypt-fs.sh; do
    t=$(chroot "$R" /bin/readlink -f "$c" 2>/dev/null) || continue
    [ -e "$R$t" ] && mount --bind "$R/usr/bin/sbx-refuse" "$R$t"
done
# Software GL (as tools/shadow-twin): the host's armhf Mesa instead of the deck's GPU blobs.
mkdir -p "$R/dev/shm" "$R/dev/pts" "$R/dev/dri" "$R/usr/lib/arm-linux-gnueabihf" "$R/usr/share/glvnd" "$R/usr/share/drirc.d"
mount --bind /dev/dri "$R/dev/dri"
mount --bind /usr/lib/arm-linux-gnueabihf "$R/usr/lib/arm-linux-gnueabihf"; mount -o remount,bind,ro "$R/usr/lib/arm-linux-gnueabihf"
[ -d /usr/share/glvnd ] && mount --bind /usr/share/glvnd "$R/usr/share/glvnd"
[ -d /usr/share/drirc.d ] && mount --bind /usr/share/drirc.d "$R/usr/share/drirc.d"
mount --bind /usr/lib/arm-linux-gnueabihf/ld-linux-armhf.so.3 "$R/lib/ld-linux-armhf.so.3"
printf '{"device": "/dev/dri/card0", "headless": "1280x800", "hwcursor": false}\n' > "$O/ScreenConfiguration.json"
for c in Default "$PRODUCT"; do f="$R/usr/Engine/ScreenConfiguration/$c/ScreenConfiguration.json"; [ -f "$f" ] && mount --bind "$O/ScreenConfiguration.json" "$f"; done
for n in null zero full random urandom; do touch "$R/dev/$n"; mount --bind "/dev/$n" "$R/dev/$n"; done
for d in class devices bus block dev; do mkdir -p "$R/sys/$d"; mount --bind "/sys/$d" "$R/sys/$d"; mount -o remount,bind,ro "$R/sys/$d"; done
mkdir -p "$R/sys/firmware/devicetree/base"
printf '%s' "$PRODUCT" > "$R/sys/firmware/devicetree/base/inmusic,product-code"
printf '%s\0rockchip,rk3288\0' "$COMPAT" > "$R/sys/firmware/devicetree/base/compatible"
mount -t proc proc "$R/proc"
# Engine aborts without the deck's serial IRQ and then pins it: give it a private /proc/irq.
printf '           CPU0\n 41:        100     GICv2  32 Level     ttyS0\n' > "$O/interrupts"
mount --bind "$O/interrupts" "$R/proc/interrupts"
mount -t tmpfs tmpfs "$R/proc/irq"; mkdir -p "$R/proc/irq/41"
printf 'f\n' > "$R/proc/irq/41/smp_affinity"; printf '0-3\n' > "$R/proc/irq/41/smp_affinity_list"

# Sticks: one loop image each. edisksd learns them from the private udev database below:
# blkid's ID_FS_* (what udev's blkid builtin sets) plus the removable flag a USB stick gets.
mkdir -p "$R/run/udev/data" "$O/img"
while read -r label fs src; do
    [ -n "$label" ] || continue
    case $fs in vfat|exfat) ;; *) echo "unsupported fs $fs" >&2; exit 2 ;; esac
    img=$O/img/$label.img; truncate -s "${STICK_SIZE:-512M}" "$img"
    if [ "$fs" = vfat ]; then mkfs.vfat -F 32 -n "$label" "$img" >/dev/null; else mkfs.exfat -L "$label" "$img" >/dev/null; fi
    dev=$(losetup -f --show "$img"); LOOPS+=("$dev")
    mount -t "$fs" "$dev" "$W/stick"; cp -r "$src/." "$W/stick/"; umount "$W/stick"
    mm=$(stat -L -c '%t:%T' "$dev"); mm=$((16#${mm%:*})):$((16#${mm#*:}))
    { echo "I:$(date +%s)000000"; blkid -p -o udev "$dev" | sed 's/^/E:/'; echo "E:EDISKSD_REMOVABLE=1"; echo "E:ID_DRIVE_THUMB=1"; echo "E:ID_BUS=usb"; echo "G:systemd"; echo "V:1"; } > "$R/run/udev/data/b$mm"
    cp "$R/run/udev/data/b$mm" "$O/udev-$label"
    touch "$R$dev"; mount --bind "$dev" "$R$dev"
    echo "$label $fs $dev b$mm" >> "$O/sticks.txt"
done <<< "$STICKS"

mkdir -p "$R/data/inMusicTempFiles" "$R/data/AppDataUser/AIR Music Technology/Engine/State" "$R/run/sc5000" "$R/tmp/home" "$R/run/dbus" \
    "$R/data/sc5000-custom/ui/packs/en/Skins/Common/Items"
cp "$W/bin/alsa-null-fixture.so" "$R/tmp/"; cp "$W/bin/libengine-ui-hook.so" "$R/run/sc5000/"
printf 'lang=en\n' > "$R/data/sc5000-custom/ui/settings.conf"
labels=$(cut -d' ' -f1 "$O/sticks.txt" | paste -sd'|')
sed -e "s/@NSTICKS@/$(wc -l < "$O/sticks.txt")/" -e "s/@LABELS@/$labels/" "$PROBE" > "$R/data/sc5000-custom/ui/packs/en/Skins/Common/Items/QmlWarnings.qml"
chroot "$R" /usr/bin/dbus-daemon --system --fork --nopidfile > "$O/dbus.log" 2>&1
chroot "$R" /usr/bin/env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin /usr/libexec/edisksd > "$O/edisksd.log" 2>&1 &
sleep 4
chroot "$R" dbus-send --system --print-reply --dest=io.github.inmusicdev.edisksd / org.freedesktop.DBus.ObjectManager.GetManagedObjects > "$O/edisksd-objects.txt" 2>&1 || :
set +e
timeout --kill-after=5s "${SECS}s" chroot "$R" /usr/bin/env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin HOME=/tmp/home \
    LD_LIBRARY_PATH=/usr/lib/arm-linux-gnueabihf:/usr/lib:/usr/qt/lib \
    LD_PRELOAD=/tmp/alsa-null-fixture.so:/run/sc5000/libengine-ui-hook.so SC_LOADER_ONLY=1 \
    SBX_CARD="$CARD" SBX_OUT=10 SBX_IN=10 SBX_CLOCK_HZ=48000 SBX_HEADLESS_VSYNC=1 \
    QT_QPA_PLATFORM=eglfs QT_QPA_EGLFS_INTEGRATION=eglfs_kms LIBGL_ALWAYS_SOFTWARE=1 GALLIUM_DRIVER=llvmpipe LP_NUM_THREADS=2 \
    QML_DISABLE_DISK_CACHE=1 MALLOC_ARENA_MAX=1 \
    /usr/Engine/Engine -d0 > "$O/engine.log" 2>&1
echo "$?" > "$O/engine-exit.txt"
set -e
grep " $R/media/" /proc/self/mountinfo | sed "s| $R| |" > "$O/mounts.txt" || :
echo "done: $O (Engine exit $(cat "$O/engine-exit.txt"), $(grep -ac '^\[W\] FSP ' "$O/engine.log" || :) probe lines)"
