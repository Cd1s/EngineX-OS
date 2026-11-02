#!/bin/sh
# Builds on the armhf cross host; the target has glibc 2.39, so reject newer symbols.
set -eu
cd "$(dirname "$0")"
out=../../rootfs-overlay/etc/sc5000/libengine-ui-hook.so
arm-linux-gnueabihf-gcc -O2 -Wall -Wextra -shared -fPIC -U_FORTIFY_SOURCE -D_FORTIFY_SOURCE=0 \
    -o "$out" engine_ui_hook.c -ldl
arm-linux-gnueabihf-strip "$out"
if arm-linux-gnueabihf-readelf -V "$out" | grep -E 'GLIBC_2\.(39\.|[4-9][0-9])'; then
    echo "needs glibc newer than the target's 2.39" >&2
    exit 1
fi
