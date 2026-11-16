#!/bin/bash
# hfsplusx.ko for the kernel of a stock Gen-1 rootfs, from pinned sources, for build-release.sh: the
# module proven on a deck was built this way (Linux 6.18.57 + RT patch, the stock kernel's own config
# with its release suffix, GCC 13, Module.symvers from the full vmlinux build; MODVERSIONS is off).
#   build-release-module.sh <stock Gen-1 rootfs.ext2> <output hfsplusx.ko>
# A cold build downloads ~150 MB and compiles the kernel (~10 min on 4 cores). The result is cached
# by the sha256 of every input in $HFSPLUSX_CACHE (default ~/.cache/enginex-hfsplusx), next to the
# downloads, so a later release build copies it. Needs debugfs, curl, flex, bison, bc, libssl-dev,
# libelf-dev and gcc-13-arm-linux-gnueabihf. check-hfsplusx.sh checks the result in each rootfs.
set -euo pipefail
HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
FS=${1:?stock Gen-1 rootfs.ext2}
DEST=${2:?output hfsplusx.ko}
CACHE=${HFSPLUSX_CACHE:-$HOME/.cache/enginex-hfsplusx}
KVER=6.18.57
KERNEL_URL=https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-$KVER.tar.xz
KERNEL_SHA=5bd9acdc82ec5efc3790ae840e5aaa40dfde7efff261cf6dde902dc570acf98c
RT_URL=https://cdn.kernel.org/pub/linux/kernel/projects/rt/6.18/older/patch-$KVER-rt8.patch.xz
RT_SHA=bced14f6fa5d8c386296792a294d90372c95d3ab1719be1bdfe2cba6169c0c5f
CROSS=arm-linux-gnueabihf-
PATH=$PATH:/usr/sbin:/sbin
die() { printf 'build-release-module: %s\n' "$*" >&2; exit 1; }
command -v debugfs >/dev/null || die 'missing debugfs (e2fsprogs)'
krel=$(debugfs -R 'ls -p /usr/lib/modules' "$FS" 2>/dev/null | awk -F/ '$6 ~ /^[0-9]/ {print $6}')
[[ $krel == "$KVER"-* && $krel != *$'\n'* ]] || die "the rootfs kernel '$krel' is not $KVER-*: pin its sources here first"
for t in curl xz patch make flex bison bc modinfo "${CROSS}gcc-13" "${CROSS}ld"; do
    command -v "$t" >/dev/null || die "missing $t (apt install curl xz-utils patch make flex bison bc kmod libssl-dev libelf-dev gcc-13-arm-linux-gnueabihf)"
done
mkdir -p "$CACHE"
W=$(mktemp -d "${TMPDIR:-/var/tmp}/hfsplusx.XXXXXX")
trap 'rm -rf "$W"' EXIT
debugfs -R "dump /boot/zImage-$krel $W/zImage" "$FS" >/dev/null 2>&1
[ -s "$W/zImage" ] || die "no /boot/zImage-$krel in $FS"
key=$({ sha256sum < "$W/zImage"; echo "$krel $KERNEL_SHA $RT_SHA"; "${CROSS}gcc-13" --version | head -n 1
        cat "$HERE"/patches/hfsplusx-*.patch "$HERE/build_modules.sh" "$HERE/build-release-module.sh"; } | sha256sum | cut -c1-16)
if [ ! -s "$CACHE/$key/hfsplusx.ko" ]; then
    fetch() { # <url> <sha256>
        local f=$CACHE/${1##*/}
        [ -s "$f" ] || { curl -fsSL --retry 3 -o "$f.part" "$1" && mv "$f.part" "$f"; }
        printf '%s  %s\n' "$2" "$f" | sha256sum -c --quiet - || { rm -f "$f"; die "checksum mismatch: $1"; }
    }
    fetch "$KERNEL_URL" "$KERNEL_SHA"
    fetch "$RT_URL" "$RT_SHA"
    tar -xJf "$CACHE/linux-$KVER.tar.xz" -C "$W"
    src=$W/linux-$KVER
    xz -dc "$CACHE/${RT_URL##*/}" | patch -s -d "$src" -p1
    : > "$src/localversion-rt" # the stock kernel carries its whole suffix in CONFIG_LOCALVERSION
    "$src/scripts/extract-ikconfig" "$W/zImage" > "$W/stock.config"
    sed -i "s/^CONFIG_LOCALVERSION=.*/CONFIG_LOCALVERSION=\"${krel#"$KVER"}\"/" "$W/stock.config"
    # GCC 13 as for the stock kernel (13.4) and the module proven on a deck: CROSS_COMPILE's gcc is 13 here.
    mkdir "$W/bin" "$W/kernel"
    ln -s "$(command -v "${CROSS}gcc-13")" "$W/bin/${CROSS}gcc"
    export PATH=$W/bin:$PATH
    cp "$W/stock.config" "$W/kernel/.config"
    kmake=(make -s -C "$src" "O=$W/kernel" ARCH=arm "CROSS_COMPILE=$CROSS")
    "${kmake[@]}" olddefconfig
    "${kmake[@]}" -j"$(getconf _NPROCESSORS_ONLN)" vmlinux
    TARGET_CONFIG=$W/stock.config TARGET_SYMVERS=$W/kernel/vmlinux.symvers KERNEL_SRC=$src KERNEL_BUILD=$W/kernel \
        EXPECTED_RELEASE=$krel CROSS_COMPILE=$CROSS BUILD_ROOT=$W/build OUTPUT_ROOT=$W/out \
        bash "$HERE/build_modules.sh" modules >&2
    mkdir -p "$CACHE/$key.new"
    cp "$W/out/lib/modules/$krel/extra/hfsplusx.ko" "$W/out/modules-build-manifest.txt" "$CACHE/$key.new/"
    rm -rf "$CACHE/$key" && mv "$CACHE/$key.new" "$CACHE/$key"
fi
install -m 0644 "$CACHE/$key/hfsplusx.ko" "$DEST"
echo "hfsplusx.ko for $krel: $(sha256sum < "$DEST" | cut -c1-64) (cache $CACHE/$key)" >&2
