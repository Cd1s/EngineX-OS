#!/bin/sh
# Builds every model's EngineX OS upgrade image from the official 5.0.4 images.
# Run from the repository root on the build host (never on the deck):
#   sh tools/az01-packer/build-release.sh <version> <fw-dir> <public-key> <fsck.hfsplus> <out-dir>
# Each official AZ01 group is patched once and copied under each model's name:
# Engine finds an image on USB by <PREFIX>-<digits and dots>-...Update.img.
set -eu
VERSION=${1:?version, e.g. 5.0.4.1}
FW=${2:?directory with the official <PREFIX>-5.0.4-Update.img files}
KEY=${3:?one SSH public key}
FSCK=${4:?static ARMv7 fsck.hfsplus}
OUT=${5:?new output directory outside any git worktree}
case $VERSION in *[!0-9.]*|'') echo "version must be digits and dots" >&2; exit 2 ;; esac
[ ! -e "$OUT" ] || { echo "$OUT exists; refusing to overwrite" >&2; exit 2; }
ROOT=$(pwd)
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

# 1. One static ARMv7 binary for all five tools, and the UI packs.
GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$WORK/sc5000-suite" ./cmd/sc5000-suite
python3 tools/ui-mod/build_packs.py --qrc "${QRC:-/root/build/sc5000-lab/5.0.4/qrc}" --out "$WORK/packs" >/dev/null
STAGE=$WORK/overlay
cp -a rootfs-overlay "$STAGE"
rm -f "$STAGE/README.md"
mkdir -p "$STAGE/usr/share/sc5000-custom" "$STAGE/usr/sbin"
xz --check=crc32 -6 -c "$WORK/sc5000-suite" > "$STAGE/usr/share/sc5000-custom/sc5000-suite.xz"
gzip -dc "$WORK/packs.tar.gz" | xz --check=crc32 -6 -c > "$STAGE/usr/share/sc5000-custom/ui-packs.tar.xz"
install -m 0755 "$FSCK" "$STAGE/usr/sbin/fsck.hfsplus"
chmod 0644 "$STAGE/usr/share/sc5000-custom/"*

# 2. One image per official group, copied to every model name of the group.
mkdir -p "$OUT"
build() { # <official image prefix> <model prefix> <release name>...
    base=$1; shift
    python3 tools/az01-packer/az01_packer.py patch-rootfs "$FW/$base-5.0.4-Update.img" \
        --ssh-public-key "$KEY" --overlay "$STAGE" --set-version "$VERSION" --output "$WORK/$base.ext2" >/dev/null
    python3 tools/az01-packer/az01_packer.py repack "$FW/$base-5.0.4-Update.img" \
        --rootfs "$WORK/$base.ext2" --output "$WORK/$base.img" >/dev/null
    rm -f "$WORK/$base.ext2"
    while [ "$#" -ge 2 ]; do
        cmp -s "$FW/$base-5.0.4-Update.img" "$FW/$1-5.0.4-Update.img" ||
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
wait "$p1" "$p2" "$p3"

# 3. Verify every image as a container again and record checksums.
cd "$OUT"
mkdir verify
for f in *-Update.img; do
    python3 "$ROOT/tools/az01-packer/az01_packer.py" info "$f" > "verify/${f%.img}.info.json"
done
sha256sum *-Update.img > SHA256SUMS.txt
ls -l
