#!/bin/bash
. "${ENGINEX_MIGRATION:-$(git rev-parse --show-toplevel)/rootfs-overlay/usr/libexec/enginex-migration}"
ENGINEX_LAB=${ENGINEX_LAB:-$legacy_lab}
# Build host only. Times a deck layer switch with every deck loaded and playing, stock QML vs the
# UI pack, on one model's official 5.3.0 rootfs: native armhf Engine, eglfs on llvmpipe at 60 Hz,
# null audio clock. Both arms run the same hook; the stock arm's pack holds only the probe.
# Usage: LSW_PRODUCT=JP21|JC11|JP07 bash layer-switch-sandbox.sh <new work dir> [rounds=3]
# LSW_ARMS="pack drop" LSW_DROP="<globs in the pack>" times the pack without those files (stock ones load instead).
# LSW_LOAD=false keeps the decks empty (JC11: its device manager rejects the null audio card).
# LSW_CENSUS=true logs the objects each toggle creates and destroys (LSW NEW/GONE), LSW_PROFILE=1 samples EMain.
set -euo pipefail
REPO=$(cd "$(dirname "$0")/../.." && pwd)
W=$(realpath -m "${1:?new work directory required}")
PRODUCT=${LSW_PRODUCT:-JP21}
case $PRODUCT in
    JP21) IMAGE=fw-5.3.0/SCLIVE4-5.3.0-Update.img CARD=${LSW_CARD:-JP21} OUT=${LSW_OUT:-8} IN=${LSW_IN:-2} ;;
    JC11) IMAGE=fw-5.3.0/PRIME4-5.3.0-Update.img CARD=${LSW_CARD:-JC11} OUT=${LSW_OUT:-8} IN=${LSW_IN:-2} ;;
    JP07) IMAGE=fw-5.3.0/SC5000-5.3.0-Update.img CARD=${LSW_CARD:-JP07} OUT=${LSW_OUT:-4} IN=${LSW_IN:-2} ;;
    *) echo "unsupported LSW_PRODUCT $PRODUCT" >&2; exit 2 ;;
esac
if [ "${3:-}" != --inner ]; then
    [ "$(uname -m)" = aarch64 ]
    [ ! -e "$W" ] || { echo "refusing existing work directory $W" >&2; exit 1; }
    mkdir -p "$W/music"
    case $PRODUCT in
        JP21) python3 - "$REPO" "$ENGINEX_LAB/$IMAGE" "$W/rootfs.ext2" <<'PY' ;; # signed AZ0x container: its rootfs record
import gzip, importlib.util, lzma, sys
from pathlib import Path
repo, image, out = map(Path, sys.argv[1:])
spec = importlib.util.spec_from_file_location('az0x', repo/'tools/az01-packer/az0x_packer.py')
az0x = importlib.util.module_from_spec(spec); spec.loader.exec_module(az0x)
b = image.read_bytes(); r = next(r for r in az0x.parse_header(b)['records'] if r['name'] == 'rootfs')
payload = b[r['offset']:r['offset'] + r['size']]
out.write_bytes((lzma.decompress if payload.startswith(b'\xfd7zXZ\0') else gzip.decompress)(payload))
PY
        *) python3 "$REPO/tools/az01-packer/az01_packer.py" unpack "$ENGINEX_LAB/$IMAGE" --output "$W/image" > "$W/unpack.json"
           mv "$W/image/rootfs.ext2" "$W/rootfs.ext2"; rm -rf "$W/image" ;;
    esac
    python3 "$REPO/tools/ui-mod/build_packs.py" --qrc "$ENGINEX_LAB/5.3.0/qrc" --out "$W/packs" > "$W/packs.log"
    for c in layer-switch-fixture.c ../../tools/shadow-twin/alsa_null_fixture.c; do
        arm-linux-gnueabihf-gcc -O2 -Wall -Wextra -Werror -shared -fPIC -o "$W/$(basename "$c" .c).so" "$REPO/tools/az01-packer/$c" -ldl -lpthread
    done
    (cd "$REPO" && go run ./tools/shadow-twin/gen_testaudio.go --out "$W/music" --seconds 400 &&
        GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -o "$W/stream-sync" ./cmd/enginex-suite)
    for d in 1 3; do cp "$W/music/track-a.wav" "$W/music/t$d.wav"; done
    for d in 2 4; do cp "$W/music/track-b.wav" "$W/music/t$d.wav"; done
    rm "$W/music/track-"?.wav
    # Arms alternate their order each round, so drift on the host hits both alike.
    rounds=${2:-3}
    for round in $(seq "$rounds"); do
        set -- ${LSW_ARMS:-stock pack}
        arms="$1 $2"; [ $((round % 2)) = 1 ] || arms="$2 $1"
        for arm in $arms; do unshare -m -n -p -f --kill-child bash "$0" "$W" "$arm-$round" --inner; done
    done
    python3 "$REPO/tools/az01-packer/layer_switch_summary.py" "$W" | tee "$W/summary.txt"
    exit
fi
run=$2 arm=${2%-*}
mount --make-rprivate /
ip link set lo up
L=$W/lower R=$W/root-$run U=$W/upper-$run
mkdir -p "$L" "$R" "$U/up" "$U/work"
mount -o loop,ro "$W/rootfs.ext2" "$L"
mount -t overlay overlay -o "lowerdir=$L,upperdir=$U/up,workdir=$U/work" "$R"
for d in tmp run dev data sys; do mount -t tmpfs tmpfs "$R/$d"; done
chroot "$R" /bin/mkdir -p /var/volatile/tmp /var/volatile/log /var/volatile/cache
mkdir -p "$R/dev/shm" "$R/dev/pts" "$R/dev/dri" "$R/sys/firmware/devicetree/base"
for n in null zero full random urandom; do touch "$R/dev/$n"; mount --bind "/dev/$n" "$R/dev/$n"; done
mount --bind /dev/dri "$R/dev/dri"
# Host Mesa (llvmpipe) and its loader, as tools/shadow-twin does.
mkdir -p "$R/usr/lib/arm-linux-gnueabihf"
mount --bind -o ro /usr/lib/arm-linux-gnueabihf "$R/usr/lib/arm-linux-gnueabihf"
for d in /usr/share/glvnd /usr/share/drirc.d; do [ ! -d "$d" ] || { mkdir -p "$R$d"; mount --bind -o ro "$d" "$R$d"; }; done
mount --bind /usr/lib/arm-linux-gnueabihf/ld-linux-armhf.so.3 "$R/lib/ld-linux-armhf.so.3"
for d in class devices bus; do mkdir -p "$R/sys/$d"; mount --bind -o ro "/sys/$d" "$R/sys/$d"; done
printf %s "$PRODUCT" > "$R/sys/firmware/devicetree/base/inmusic,product-code"
printf 'inmusic,%s\0rockchip,rk3288\0' "${PRODUCT,,}" > "$R/sys/firmware/devicetree/base/compatible"
mount -t proc proc "$R/proc"
printf '           CPU0\n 41:        100     GICv2  32 Level     ttyS0\n' > "$U/interrupts"
mount --bind "$U/interrupts" "$R/proc/interrupts"
mount -t tmpfs tmpfs "$R/proc/irq"
mkdir -p "$R/proc/irq/41"; printf 'f\n' > "$R/proc/irq/41/smp_affinity"; printf '0-3\n' > "$R/proc/irq/41/smp_affinity_list"
printf '{"device": "/dev/dri/card0", "headless": "1280x800", "hwcursor": false}\n' > "$U/screen.json"
for c in "$R"/usr/Engine/ScreenConfiguration/{Default,"$PRODUCT"}/ScreenConfiguration.json; do [ ! -f "$c" ] || mount --bind "$U/screen.json" "$c"; done
mkdir -p "$R/etc/enginex" "$R/tmp/home" "$R/run/dbus" "$R/data/music" "$R/data/enginex/ui/packs/en/Skins/Common/Items"
cp "$REPO/rootfs-overlay/etc/enginex/libengine-ui-hook.so" "$R/etc/enginex/"
cp "$W/layer-switch-fixture.so" "$W/alsa_null_fixture.so" "$W/stream-sync" "$R/tmp/"
cp "$W"/music/t?.wav "$R/data/music/"
if [ "$arm" != stock ]; then cp -a "$W/packs/en/." "$R/data/enginex/ui/packs/en/"; cp -a "$W/packs/icons" "$R/data/enginex/ui/"; fi
if [ "$arm" = drop ]; then (cd "$R/data/enginex/ui/packs/en" && shopt -s globstar nullglob && for g in $LSW_DROP; do rm -rfv -- $g; done) | wc -l; fi
sed "s/@RUNS@/${LSW_RUNS:-12}/; s/@CENSUS@/${LSW_CENSUS:-false}/; s/@LOAD@/${LSW_LOAD:-true}/" "$REPO/tools/az01-packer/layer-switch-probe.qml" > "$R/data/enginex/ui/packs/en/Skins/Common/Items/QmlWarnings.qml"
printf 'lang=en\n' > "$R/data/enginex/ui/settings.conf"
chroot "$R" /usr/bin/dbus-daemon --system --fork --nopidfile > "$U/dbus.log" 2>&1 || true
# The pack polls stream-sync for its settings, as on a deck (both arms run it).
chroot "$R" /usr/bin/env -i PATH=/usr/bin:/bin HOME=/tmp/home /tmp/stream-sync --deemixd= --disable-guard --listen :8080 > "$U/sync.log" 2>&1 &
for _ in $(seq 50); do curl -sf -o /dev/null http://127.0.0.1:8080/v1/enginex/ui && break; sleep 0.2; done || :
curl -sf -o /dev/null http://127.0.0.1:8080/v1/enginex/ui || echo "LSW stream-sync not answering ($run)"
log=$W/engine-$run.log
chroot "$R" /usr/bin/env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin HOME=/tmp/home \
    LD_LIBRARY_PATH=/usr/lib/arm-linux-gnueabihf:/usr/lib:/usr/qt/lib \
    LD_PRELOAD=/tmp/layer-switch-fixture.so:/tmp/alsa_null_fixture.so:/etc/enginex/libengine-ui-hook.so \
    SBX_CARD="$CARD" SBX_OUT="$OUT" SBX_IN="$IN" SBX_CLOCK_HZ=48000 SBX_HEADLESS_VSYNC=1 LSW_PROFILE="${LSW_PROFILE:-0}" \
    QT_QPA_PLATFORM=eglfs QT_QPA_EGLFS_INTEGRATION=eglfs_kms LIBGL_ALWAYS_SOFTWARE=1 GALLIUM_DRIVER=llvmpipe LP_NUM_THREADS=2 \
    QML_DISABLE_DISK_CACHE=1 QML_XHR_ALLOW_FILE_READ=1 MALLOC_ARENA_MAX=1 /usr/Engine/Engine -d0 > "$log" 2>&1 &
engine=$!
for _ in $(seq "${LSW_TIMEOUT:-600}"); do
    grep -aqE 'LSW (DONE|ERROR)' "$log" || ! kill -0 "$engine" 2>/dev/null && break
    sleep 1
done
kill -TERM "$engine" 2>/dev/null || true; sleep 3; kill -KILL "$engine" 2>/dev/null || true; wait "$engine" 2>/dev/null || true
cp "$R/tmp/swaps.log" "$W/swaps-$run.log" 2>/dev/null || : > "$W/swaps-$run.log"
if [ "${LSW_PROFILE:-0}" = 1 ]; then
    cp "$R/tmp/samples.txt" "$W/samples-$run.txt"; cp "$R/tmp/maps.txt" "$W/maps-$run.txt"
    mkdir -p "$W/libs"; cp -n "$R"/usr/lib/libQt6*.so.* "$R"/usr/Engine/Engine /usr/lib/arm-linux-gnueabihf/lib{c,stdc++,m}.so.* "$W/libs/" 2>/dev/null || :
fi
grep -aE 'LSW (CONFIG|LOADED|PLAY|ERROR|DONE)|engine-ui-hook: .*files' "$log" | cut -c1-200
grep -aq 'LSW DONE|PASS' "$log"
