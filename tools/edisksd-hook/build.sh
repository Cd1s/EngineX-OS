#!/bin/sh
# Builds on the armhf cross host; the target has glibc 2.39, so reject newer symbols.
# The deck kernel uses 4 KiB pages; ld pads ARM segments to 64 KiB by default.
set -eu
# AArch64 (AZ04: PRIME 4 G2, SYSTEM ONE) on an arm64 host: CROSS= OUT=/abs/path.so sh build.sh
cd "$(dirname "$0")"
cross=${CROSS-arm-linux-gnueabihf-}
out=${OUT:-../../rootfs-overlay/etc/sc5000/libedisksd-hook.so}
mkdir -p "$(dirname "$out")"
"${cross}gcc" -O2 -Wall -Wextra -shared -fPIC -U_FORTIFY_SOURCE -D_FORTIFY_SOURCE=0 \
    -ffunction-sections -fdata-sections -Wl,--gc-sections -Wl,-z,max-page-size=4096 -o "$out" edisksd_hook.c -ldl
"${cross}strip" "$out"
if "${cross}readelf" -V "$out" | grep -E 'GLIBC_2\.(39\.|[4-9][0-9])'; then
    echo "needs glibc newer than the target's 2.39" >&2
    exit 1
fi
