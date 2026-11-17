#!/bin/sh
# Builds on the armhf cross host; the target has glibc 2.39, so reject newer symbols.
# The deck kernel uses 4 KiB pages; ld pads ARM segments to 64 KiB by default.
set -eu
# AArch64 hook (AZ04: PRIME 4 G2, SYSTEM ONE) on an arm64 host: CROSS= OUT=/abs/path.so sh build.sh
# ENGINEX_HOOK_DISABLE="stems,zoom30" leaves those features out of this build (names: hook_features.h).
cd "$(dirname "$0")"
cross=${CROSS-arm-linux-gnueabihf-}
out=${OUT:-../../rootfs-overlay/etc/enginex/libengine-ui-hook.so}
disable=$(printf '%s' "${ENGINEX_HOOK_DISABLE-}" | tr ',\t\r\n' '    ')
set --
if [ -n "$(printf '%s' "$disable" | tr -d ' ')" ]; then
    known=$(sed -n 's/^ *X([A-Z0-9_]*, "\([a-z0-9-]*\)").*$/\1/p' hook_features.h)
    list=
    for name in $disable; do
        printf '%s\n' "$known" | grep -qx -- "$name" || {
            echo "unknown hook feature: $name (known: $(printf '%s ' $known))" >&2
            exit 2
        }
        list=${list:+$list,}$name
    done
    set -- "-DENGINEX_HOOK_DISABLE=\"$list\""
    echo "hook features off in this build: $list" >&2
fi
"${cross}gcc" -O2 -Wall -Wextra -shared -fPIC -U_FORTIFY_SOURCE -D_FORTIFY_SOURCE=0 "$@" \
    -ffunction-sections -fdata-sections -Wl,--gc-sections -Wl,-z,max-page-size=4096 -o "$out" engine_ui_hook.c -ldl -lpthread
"${cross}strip" "$out"
if "${cross}readelf" -V "$out" | grep -E 'GLIBC_2\.(39\.|[4-9][0-9])'; then
    echo "needs glibc newer than the target's 2.39" >&2
    exit 1
fi
