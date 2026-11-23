#!/bin/bash
# Every controller-side QML file the UI pack replaces must compile in the Engine that loads it. A file that does not
# compile kills that module on every board using it (the stock pads, buttons or LEDs stop), and handler tests that lift
# functions out of the file cannot see it. This runs each board's own stock Engine natively on the build host (5.3.0 for
# the first-generation boards, the SC LIVE 4 Engine with its own qrc for the second-generation boards), with the real UI
# hook and the pack built by build_packs.py. A probe loaded as the root QML (so it runs even when the main UI cannot
# load) compiles with Qt.createComponent, the Engine's QML compiler and type registrations (offscreen, no deck, no MIDI):
#   - the main UI root EmbeddedMain.qml from the pack,
#   - every QML file the pack replaces under ControlSurfaceModules/ and airAssignments/ (from the Engine's qrc, so the
#     pack version is what compiles), and
#   - every stock controller assignment and device file of every board in that rootfs (and the AZ04 rootfs when the lab
#     has it), which import the replaced modules the way the controller engine does at start-up.
# The stock arm runs the same list with the controller-side files left out of the pack; the pack must compile
# everything the stock files compile, and every file it replaces.
#   bash tools/az01-packer/test_controller_qml_compile.sh <new-private-dir> [BOARD ...]   (default: all 14 boards)
# CQC_BUILDER=<build_packs.py> builds the pack with another revision's builder (copy its tools/ui-mod next to it).
# CQC_PROBE=<file.qml> CQC_TAG=<tag> [CQC_ARMS=pack] runs another root probe in the same sandbox instead (for example
# test_beatjump_pads_sandbox.sh): it passes when the log has "<tag> DONE|PASS|<board>-<arm>|". Arm "stock" runs
# without the pack's controller files; arm "fourdeck" (players only) turns on the hook's 4-deck layout (decks 1 and 3,
# Config.device.isPlayer cleared, as the 4 Decks and Paired Player layouts run).
# Run it inside your own mount namespace (unshare -m --propagation private) under the build host's heavy-job lock.
. "${ENGINEX_MIGRATION:-$(git rev-parse --show-toplevel)/rootfs-overlay/usr/libexec/enginex-migration}"
ENGINEX_LAB=${ENGINEX_LAB:-$legacy_lab}
set -euo pipefail
REPO=$(cd "$(dirname "$0")/../.." && pwd)
W=$(realpath -m "${1:?new private sandbox directory required}")
LAB=${ENGINEX_LAB}
BUILDER=$(realpath -m "${CQC_BUILDER:-$REPO/tools/ui-mod/build_packs.py}")
image_of() {
    case "$1" in
        JP07|JP08|JP11) echo gen1 fw-5.3.0/SC5000 ;;
        JP13|JP14) echo gen1 fw-5.3.0/SC6000 ;;
        JC11|JC16|NH08) echo gen1 fw-5.3.0/PRIME4 ;;
        JP21|JP11S) echo gen2 fw-5.3.0/SCLIVE4 ;;
        JP20) echo gen2 fw-5.3.0/SCLIVE2 ;;
        JC11S) echo gen2 fw-5.3.0-gen2/PRIME4PLUS ;;
        NH08S) echo gen2 fw-5.3.0-gen2/MIXSTREAMPROPLUS ;;
        NH10) echo gen2 fw-5.3.0-gen2/MIXSTREAMPROGO ;;
        *) echo "unknown board $1 (the AZ04 Engine does not start in this sandbox; its files are compiled by the others)" >&2; return 1 ;;
    esac
}
if [ "${2:-}" != --inner ]; then
    [ "$(uname -m)" = aarch64 ] || { echo 'native ARM build host (aarch64) required' >&2; exit 1; }
    [ ! -e "$W" ] || { echo "refusing existing sandbox $W" >&2; exit 1; }
    boards=("${@:2}")
    [ ${#boards[@]} -gt 0 ] || boards=(JP07 JP08 JP11 JP13 JP14 JC11 JC16 NH08 JP20 JP21 JC11S JP11S NH08S NH10)
    for board in "${boards[@]}"; do image_of "$board" > /dev/null; done
    mkdir -p "$W"
    for board in "${boards[@]}"; do
        read -r kind image < <(image_of "$board")
        dir=$W/$(basename "$image")
        [ ! -e "$dir" ] || continue
        case "$kind" in
            gen1) python3 "$REPO/tools/az01-packer/az01_packer.py" unpack "$LAB/$image-5.3.0-Update.img" --output "$dir" > "$dir.log" ;;
            gen2) python3 - "$REPO" "$LAB/$image-5.3.0-Update.img" "$dir" <<'PY'
import gzip, importlib.util, lzma, sys
from pathlib import Path
repo, image, out = map(Path, sys.argv[1:])
spec = importlib.util.spec_from_file_location('az0x', repo/'tools/az01-packer/az0x_packer.py')
az0x = importlib.util.module_from_spec(spec); spec.loader.exec_module(az0x)
b = image.read_bytes(); r = next(r for r in az0x.parse_header(b)['records'] if r['name'] == 'rootfs')
payload = b[r['offset']:r['offset']+r['size']]
out.mkdir()
(out/'rootfs.ext2').write_bytes((lzma.decompress if payload.startswith(b'\xfd7zXZ\0') else gzip.decompress)(payload))
PY
            ;;
        esac
    done
    python3 "$BUILDER" --qrc "$LAB/5.3.0/qrc" --out "$W/packs" > "$W/packs.log"
    (cd "$W/packs/en" && find ControlSurfaceModules airAssignments -name '*.qml' 2>/dev/null | LC_ALL=C sort) > "$W/controller-files.txt"
    [ -s "$W/controller-files.txt" ] || { echo "the pack replaces no controller-side QML file" >&2; exit 1; }
    # Engine loads one language's pack. The controller side must not differ between languages, or each needs a run.
    for lang in es zh-Hans zh-Hant; do
        while read -r rel; do
            cmp -s "$W/packs/en/$rel" "$W/packs/$lang/$rel" || { echo "$lang/$rel differs from en/$rel" >&2; exit 1; }
        done < "$W/controller-files.txt"
    done
    sed 's/^/  pack replaces /' "$W/controller-files.txt"
    # The stock arm: the same pack without the controller-side files, so Engine serves its own for those.
    cp -a "$W/packs/en" "$W/stock-en"
    while read -r rel; do rm -f "$W/stock-en/$rel"; done < "$W/controller-files.txt"
    arm-linux-gnueabihf-gcc -O2 -Wall -Wextra -Werror -shared -fPIC -o "$W/identity.so" "$REPO/tools/az01-packer/cc-fit-identity.c"
    fail=0
    for board in "${boards[@]}"; do
        for arm in ${CQC_ARMS:-stock pack}; do
            unshare -m -n -p -f --kill-child bash "$0" "$W" --inner "$board" "$arm" || fail=1
        done
    done
    [ "$fail" = 0 ] && echo "CQC RESULT|PASS|${boards[*]}" || echo "CQC RESULT|FAIL"
    exit $fail
fi
board=${3:?board required}
arm=${4:?arm required}
read -r kind image < <(image_of "$board")
if [ "$arm" = fourdeck ] && ! [[ " JP07 JP08 JP13 JP14 " == *" $board "* ]]; then echo "$board fourdeck: not a player, skipped"; exit 0; fi
R=$W/$board-$arm-root
lower=$(echo "$board" | tr A-Z a-z)
mount --make-rprivate /
ip link set lo up
mkdir -p "$R"
mount -o loop "$W/$(basename "$image")/rootfs.ext2" "$R"
for d in tmp run dev data sys; do mount -t tmpfs tmpfs "$R/$d"; done
chroot "$R" /bin/mkdir -p /var/volatile/tmp /var/volatile/log /var/volatile/cache
mkdir -p "$R/dev/shm" "$R/dev/pts" "$R/sys/firmware/devicetree/base" "$R/sys/devices/system/cpu" "$R/run/dbus" "$R/tmp/home" "$R/data/enginex/ui/packs"
for f in online possible present; do echo 0-3 > "$R/sys/devices/system/cpu/$f"; done
for n in null zero full random urandom; do touch "$R/dev/$n"; mount --bind "/dev/$n" "$R/dev/$n"; done
printf '%s\0' "$board" > "$R/sys/firmware/devicetree/base/inmusic,product-code"
case "$kind" in
    gen1) compat="inmusic,$lower\0rockchip,rk3288" ;;
    gen2) second=az05; [ "$board" = JC11S ] && second=az01; compat="inmusic,$lower\0inmusic,$second\0rockchip,rk3288" ;;
esac
printf "$compat\0" > "$R/sys/firmware/devicetree/base/compatible"
mount -t proc -o ro proc "$R/proc"
# Engine requires the deck's serial IRQ. Its affinity writes stay in tmpfs.
printf '           CPU0\n 41:        100     GICv2  32 Level     ttyS0\n' > "$W/$board-$arm-interrupts"
mount --bind "$W/$board-$arm-interrupts" "$R/proc/interrupts"
mount -t tmpfs tmpfs "$R/proc/irq"
mkdir -p "$R/proc/irq/41"
printf 'f\n' > "$R/proc/irq/41/smp_affinity"
printf '0-3\n' > "$R/proc/irq/41/smp_affinity_list"
cp "$REPO/rootfs-overlay/etc/enginex/libengine-ui-hook.so" "$R/tmp/libengine-ui-hook.so"
preload=/tmp/libengine-ui-hook.so
[ "$kind" = gen2 ] && { cp "$W/identity.so" "$R/tmp/identity.so"; preload=/tmp/identity.so:/tmp/libengine-ui-hook.so; }
if [ "$arm" = stock ]; then cp -a "$W/stock-en" "$R/data/enginex/ui/packs/en"; else cp -a "$W/packs/en" "$R/data/enginex/ui/packs/en"; fi
# The AZ04 boards' files (byte-identical QML base), compiled here because the AZ04 Engine does not start in a chroot.
az04=$LAB/systemone-analysis/rootfs/usr/Engine/AssignmentFiles
if [ -d "$az04" ]; then
    mkdir -p "$R/tmp/az04-assignments"
    mount --bind "$az04" "$R/tmp/az04-assignments"
    mount -o remount,ro,bind "$R/tmp/az04-assignments"
fi
urls=$W/$board-$arm-urls.txt
{ echo qrc:/Skins/Embedded/EmbeddedMain.qml; sed 's|^|qrc:/|' "$W/controller-files.txt"; } > "$urls"
(cd "$R" && find usr/Engine/AssignmentFiles tmp/az04-assignments -name '*.qml' 2>/dev/null | LC_ALL=C sort | sed 's|^|file:///|') >> "$urls"
probe="$R/data/enginex/ui/packs/en/Skins/Embedded/main.qml"
mkdir -p "$(dirname "$probe")"
python3 -I - "${CQC_PROBE:-$REPO/tools/az01-packer/controller-qml-compile-probe.qml}" "$probe" "$board-$arm" "$urls" <<'PY'
import json, sys
src, out, board, urls = sys.argv[1:]
text = open(src, encoding="utf-8").read()
text = text.replace("@BOARD@", board).replace("@URLS@", json.dumps(open(urls, encoding="utf-8").read().split()))
open(out, "w", encoding="utf-8").write(text)
PY
printf 'lang=en\n' > "$R/data/enginex/ui/settings.conf"
[ "$arm" != fourdeck ] || printf 'four_deck_waveform=1\nfour_deck_role=13\n' >> "$R/data/enginex/ui/settings.conf"
chroot "$R" /usr/bin/dbus-daemon --system --fork --nopidfile > "$W/$board-$arm-dbus.log" 2>&1 || true
log=$W/$board-$arm-engine.log
engine_rc=0
# The controller engine imports these two module roots from Engine's qrc; the GUI engine has no such import path.
timeout --kill-after=5s "${CQC_TIMEOUT:-90}s" chroot "$R" /usr/bin/env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin HOME=/tmp/home \
    LD_LIBRARY_PATH=/usr/qt/lib LD_PRELOAD=$preload QT_QPA_PLATFORM=offscreen QML_DISABLE_DISK_CACHE=1 MALLOC_ARENA_MAX=1 \
    QML_IMPORT_PATH="${CQC_IMPORT_PATH-:/airAssignments::/ControlSurfaceModules}" \
    /usr/Engine/Engine -d0 > "$log" 2>&1 || engine_rc=$?
cp "$R/tmp/engine-quit-reason" "$W/$board-$arm-quit-reason" 2>/dev/null || true
grep -aq "engine-ui-hook: Engine " "$log" || { echo "$board $arm: the hook did not start (Engine exit $engine_rc)" >&2; exit 1; }
if [ -n "${CQC_PROBE:-}" ]; then
    tag=${CQC_TAG:?CQC_TAG names the log tag of the probe}
    grep -a -o -E "$tag (PASS|FAIL|DONE)\|.*" "$log" | sed 's/ *\[20[0-9T:.Z-]*\].*//' || true
    # A ReferenceError in a pack file is a misnamed id or property: print them all.
    grep -a -E '^\[W\] qrc:/.*(ReferenceError|is not a function)' "$log" | sed 's/ *\[20[0-9T:.Z-]*\].*//' | sort -u | head -n 20 || true
    grep -aq "$tag DONE|PASS|$board-$arm|" "$log" || { echo "$board $arm: $tag probe failed (Engine exit $engine_rc)" >&2; exit 1; }
    echo "$board $tag PASS"
    exit 0
fi
total=$(wc -l < "$urls")
grep -aq "CQC DONE|$board-$arm|" "$log" || {
    grep -a -A2 -E '^\[W\] (qrc|file):/' "$log" | sed 's/ *\[20[0-9T:.Z-]*\].*//' | head -n 12 >&2
    echo "$board $arm: the probe did not finish (Engine exit $engine_rc)" >&2; exit 1; }
# "<OK|FAIL> <url>" per listed file, by the line number the probe reports (Engine wraps long log lines).
grep -a -o -E "CQC (OK|FAIL)\|$board-$arm\|[0-9]+\|" "$log" | awk -F'|' -v list="$urls" '
    BEGIN { while ((getline u < list) > 0) url[++n] = u }
    { split($1, w, " "); print w[2], url[$3] }' | LC_ALL=C sort -k2 > "$W/$board-$arm-result.txt"
[ "$(wc -l < "$W/$board-$arm-result.txt")" = "$total" ] || { echo "$board $arm: $total files listed, $(wc -l < "$W/$board-$arm-result.txt") reported" >&2; exit 1; }
grep -a -o -E "CQC ERR\|$board-$arm\|[0-9]+\|.*" "$log" | sed 's/ *\[20[0-9T:.Z-]*\].*//' | awk -F'|' -v list="$urls" '
    BEGIN { while ((getline u < list) > 0) url[++n] = u }
    { e = $4; for (i = 5; i <= NF; i++) e = e "|" $i; f = url[$3]; sub(/^.*\//, "", f); print "CQC ERR|" $2 "|" f ": " e }' | sort -u || true
grep -a -o -E "CQC DONE\|$board-$arm\|[^|]*\|[^|]*\|" "$log"
if [ "$arm" = pack ]; then
    # Everything the stock files compile must compile with the pack, and every replaced file must compile.
    regressions=$(LC_ALL=C join -1 2 -2 2 -o 1.1,2.1,0 "$W/$board-stock-result.txt" "$W/$board-pack-result.txt" | awk '$1 == "OK" && $2 != "OK" {print $3}')
    replaced=$(grep -a '^FAIL qrc:/' "$W/$board-pack-result.txt" | sed 's/^FAIL //' || true)
    if [ -n "$regressions$replaced" ]; then
        printf '%s\n' $regressions $replaced | sort -u | sed "s/^/CQC REGRESSION|$board|/"
        echo "$board CQC_FAIL"
        exit 1
    fi
    echo "$board CQC_PASS ($(grep -c '^OK ' "$W/$board-pack-result.txt")/$total compile; stock $(grep -c '^OK ' "$W/$board-stock-result.txt")/$total)"
fi
