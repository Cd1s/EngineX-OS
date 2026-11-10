#!/bin/sh
# Builds every model's EngineX OS upgrade image from one set of official images.
# Run from the repository root on the build host (never on the deck):
#   sh tools/az01-packer/build-release.sh <version> <fw-dir> <public-key> <fsck.hfsplus> <out-dir>
# Each official AZ01 group is patched once and copied under each model's name:
# Engine finds an image on USB by <PREFIX>-<digits and dots>-...Update.img.
set -eu
VERSION=${1:?version, e.g. 5.0.4.1}
FW=${2:?directory with the official <PREFIX>-<base>-Update.img files, one base version}
KEY=${3:?one SSH public key for a private build, or "none" for public releases (#46)}
FSCK=${4:?static ARMv7 fsck.hfsplus}
OUT=${5:?new output directory outside any git worktree}
case $VERSION in *[!0-9.]*|'') echo "version must be digits and dots" >&2; exit 2 ;; esac
[ ! -e "$OUT" ] || { echo "$OUT exists; refusing to overwrite" >&2; exit 2; }
ROOT=$(pwd)
KEYARG=$KEY; [ "$KEY" != none ] || KEYARG=
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
# The official base, e.g. 5.3.0: every image in $FW must carry the same one.
BASE=$(cd "$FW" && ls *-Update.img | sed -n 's/^SC5000-\([0-9.]*\)-Update\.img$/\1/p')
[ -n "$BASE" ] && [ "$(echo "$BASE" | wc -l)" = 1 ] || { echo "need exactly one SC5000-<base>-Update.img in $FW" >&2; exit 2; }

# 1. One static ARMv7 binary for all five tools, and the UI packs.
GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$WORK/sc5000-suite" ./cmd/sc5000-suite
# QML comes straight from the base image's Engine, so a new base needs no hand-made qrc tree.
if [ -z "${QRC:-}" ]; then
    arm-linux-gnueabihf-gcc -O2 -shared -fPIC -o "$WORK/qrc_dump.so" tools/ui-mod/qrc_dump.c -ldl
    python3 -I tools/ui-mod/extract_qrc.py --image "$FW/SC5000-$BASE-Update.img" --preload "$WORK/qrc_dump.so" --out "$WORK/qrc" >/dev/null
    QRC=$WORK/qrc
fi
python3 tools/ui-mod/build_packs.py --qrc "$QRC" --out "$WORK/packs" >/dev/null
STAGE=$WORK/overlay
cp -a rootfs-overlay "$STAGE"
rm -f "$STAGE/README.md"
mkdir -p "$STAGE/usr/share/sc5000-custom" "$STAGE/usr/sbin"
# BusyBox xzcat on the deck rejects BCJ filters; lc/lp/pb suit 32-bit ARM code.
# The 8 MiB dictionary keeps the deck's decoder memory as it was with -6.
xz -T0 --check=crc32 --lzma2=preset=9e,dict=8MiB,lc=0,lp=2,pb=2 -c "$WORK/sc5000-suite" > "$STAGE/usr/share/sc5000-custom/sc5000-suite.xz"
gzip -dc "$WORK/packs.tar.gz" | xz -T0 --check=crc32 --lzma2=preset=9e,dict=8MiB -c > "$STAGE/usr/share/sc5000-custom/ui-packs.tar.xz"
install -m 0755 "$FSCK" "$STAGE/usr/sbin/fsck.hfsplus"
chmod 0644 "$STAGE/usr/share/sc5000-custom/"*

# 1b. Second generation: /data bundles. 6 armhf models share the 32-bit suite and hooks, AZ04 (PRIME 4 G2, SYSTEM ONE) needs arm64 ones.
mkdir -p "$WORK/hooks32" "$WORK/hooks64"
OUT=$WORK/hooks32/libengine-ui-hook.so sh tools/ui-mod/build.sh
OUT=$WORK/hooks32/libedisksd-hook.so sh tools/edisksd-hook/build.sh
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$WORK/sc5000-suite-arm64" ./cmd/sc5000-suite
CROSS64=aarch64-linux-gnu-; [ "$(uname -m)" != aarch64 ] || CROSS64=
CROSS=$CROSS64 OUT=$WORK/hooks64/libengine-ui-hook.so sh tools/ui-mod/build.sh
CROSS=$CROSS64 OUT=$WORK/hooks64/libedisksd-hook.so sh tools/edisksd-hook/build.sh

# 1c. EngineX boot splash (#118). The firmware splash partition shows before Linux starts, and every
# update rewrites it, so an image carrying the stock one puts the stock picture back on the deck.
python3 -I tools/splash/splash_tool.py encode --input docs/assets/boot-splash-enginex.png --output "$WORK/splash.bin" >/dev/null

# 2. One image per official group, copied to every model name of the group.
mkdir -p "$OUT"
build() { # <official image prefix> <model prefix> <release name>...
    base=$1; shift
    python3 tools/az01-packer/az01_packer.py patch-rootfs "$FW/$base-$BASE-Update.img" \
        ${KEYARG:+--ssh-public-key "$KEYARG"} --overlay "$STAGE" --set-version "$VERSION" --output "$WORK/$base.ext2" >/dev/null
    python3 tools/az01-packer/az01_packer.py repack "$FW/$base-$BASE-Update.img" \
        --rootfs "$WORK/$base.ext2" --splash "$WORK/splash.bin" --output "$WORK/$base.img" >/dev/null
    rm -f "$WORK/$base.ext2"
    while [ "$#" -ge 2 ]; do
        cmp -s "$FW/$base-$BASE-Update.img" "$FW/$1-$BASE-Update.img" ||
            { echo "$1 is not the same official image as $base" >&2; exit 1; }
        cp "$WORK/$base.img" "$OUT/$1-$VERSION-EngineX-OS-$2-Update.img"
        shift 2
    done
    rm -f "$WORK/$base.img"
}
build SC5000 SC5000 Denon-SC5000-Prime-JP07 SC5000M Denon-SC5000M-Prime-JP08 PRIMEGO Denon-PRIME-GO-JP11 &
p1=$!
build SC6000 SC6000 Denon-SC6000-Prime-JP13 SC6000M Denon-SC6000M-Prime-JP14 &
p2=$!
build PRIME4 PRIME4 Denon-PRIME-4-JC11 PRIME2 Denon-PRIME-2-JC16 MIXSTREAMPRO Numark-Mixstream-Pro-NH08 &
p3=$!
# One wait per job: "wait a b c" only reports c, so set -e would miss a failed group.
wait "$p1"
wait "$p2"
wait "$p3"

# 3. Verify every 1st-gen AZ01 image as a container again, then copy 2nd-gen images and record checksums.
mkdir "$OUT/verify"
for f in "$OUT/"*-Update.img; do
    b=$(basename "$f")
    python3 "$ROOT/tools/az01-packer/az01_packer.py" info "$f" > "$OUT/verify/${b%.img}.info.json"
done

for m in SCLIVE4 SCLIVE2 PRIME4PLUS PRIMEGOPLUS MIXSTREAMPROPLUS MIXSTREAMPROGO; do
    python3 tools/az01-packer/build-data-bundle.py "$VERSION" --prefix "$m" --suite "$WORK/sc5000-suite" \
        --hook "$WORK/hooks32/libengine-ui-hook.so" --ui-packs "$WORK/packs.tar.gz" --out "$WORK/bundle-$m" >/dev/null
    cp "$WORK/bundle-$m/$m-$VERSION-EngineX-OS-Data-Bundle.tar.gz" "$WORK/bundle-$m/$m-$VERSION-EngineX-OS-"*-Update.img "$OUT/"
done
for m in PRIME4G2 SYSTEMONE; do
    python3 tools/az01-packer/build-data-bundle.py "$VERSION" --prefix "$m" --suite "$WORK/sc5000-suite-arm64" \
        --hook "$WORK/hooks64/libengine-ui-hook.so" --ui-packs "$WORK/packs.tar.gz" --out "$WORK/bundle-$m" >/dev/null
    cp "$WORK/bundle-$m/$m-$VERSION-EngineX-OS-Data-Bundle.tar.gz" "$WORK/bundle-$m/$m-$VERSION-EngineX-OS-"*-Update.img "$OUT/"
done

cd "$OUT"
sha256sum *-Update.img *-Data-Bundle.tar.gz > SHA256SUMS.txt
ls -l
