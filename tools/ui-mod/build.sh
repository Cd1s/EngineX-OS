#!/bin/sh
# Builds on the armhf cross host; the target has glibc 2.39, so reject newer symbols.
# The deck kernel uses 4 KiB pages; ld pads ARM segments to 64 KiB by default.
# -z now: every libc symbol is bound at load, none by the audio thread on its first call (and a missing one fails at load, where the gate sees it).
# Native JSON decoder exceptions must cross the C metadata wrapper.
# -z defs: a symbol none of the linked libraries has fails the build. The hook is preloaded into every command Engine starts; one that
# does not itself load the missing library (libm for lscpu) would not start, and Engine aborts without its CPU count.
set -eu
# AArch64 hook (AZ04: PRIME 4 G2, SYSTEM ONE) on an arm64 host: CROSS= OUT=/abs/path.so sh build.sh
# ENGINEX_HOOK_DISABLE="stems,zoom30" leaves those features out of this build (names: hook_features.h).
cd "$(dirname "$0")"
python3 check_hook_features.py
cross=${CROSS-arm-linux-gnueabihf-}
out=${OUT:-../../rootfs-overlay/etc/enginex/libengine-ui-hook.so}
disable=$(printf '%s' "${ENGINEX_HOOK_DISABLE-}" | tr ',\t\r\n' '    ')
set --
if [ -n "$(printf '%s' "$disable" | tr -d ' ')" ]; then
    known=$(sed -n 's/^ *X([A-Z0-9_]*, "\([a-z0-9-]*\)").*$/\1/p' hook_features.h)
    list=
    for name in $disable; do
        printf '%s\n' "$known" | grep -Fqx -- "$name" || {
            echo "unknown hook feature: $name (known: $(printf '%s ' $known))" >&2
            exit 2
        }
        list=${list:+$list,}$name
    done
    set -- "-DENGINEX_HOOK_DISABLE=\"$list\""
    echo "hook features off in this build: $list" >&2
fi
"${cross}gcc" -O2 -Wall -Wextra -Werror=missing-field-initializers -shared -fPIC -funwind-tables -U_FORTIFY_SOURCE -D_FORTIFY_SOURCE=0 "$@" \
    -ffunction-sections -fdata-sections -Wl,--gc-sections -Wl,-z,now -Wl,-z,defs -Wl,-z,max-page-size=4096 -o "$out" engine_ui_hook.c -ldl -lpthread
"${cross}strip" "$out"
# One large initialised array turns a zero-filled .bss into file bytes (the Color FX delay banks once made the hook 16 MB, 116 KB before).
size=$(wc -c < "$out")
[ "$size" -le 2097152 ] || { echo "hook is $size bytes (> 2 MiB): a big static array was given a non-zero initialiser" >&2; exit 1; }
if "${cross}readelf" -V "$out" | grep -E 'GLIBC_2\.(39\.|[4-9][0-9])'; then
    echo "needs glibc newer than the target's 2.39" >&2
    exit 1
fi
