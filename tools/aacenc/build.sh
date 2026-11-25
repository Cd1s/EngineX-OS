#!/bin/sh
# Builds enginex-aacenc, the SHOUTcast mode's AAC encoder front end (it carries no codec: it opens the
# stock libavcodec.so.58 that every base image ships). Run on the build host:
#   OUT=/abs/enginex-aacenc sh tools/aacenc/build.sh                       # armhf (Gen-1, Gen-2 armhf)
#   CROSS= OUT=/abs/enginex-aacenc-arm64 sh tools/aacenc/build.sh          # AZ04 on an arm64 host
# It needs FFmpeg 4.4.3's headers for the struct layouts (the version on the decks): the release tarball
# is fetched once, checked against its sha256 (the release signature by the FFmpeg release key
# FCF986EA15E6E293A5644F10B4322F04D67658D8 was checked when this pin was written) and cached in
# $FFMPEG_HEADERS_CACHE (default ~/.cache/enginex-ffmpeg-headers). FFMPEG_HEADERS=<dir> uses an
# unpacked include tree instead (offline builds).
set -eu
cd "$(dirname "$0")"
cross=${CROSS-arm-linux-gnueabihf-}
out=${OUT:?OUT=/absolute/path/of/the/binary}
VER=4.4.3
SHA=6c5b6c195e61534766a0b5fe16acc919170c883362612816d0a1c7f4f947006e
inc=${FFMPEG_HEADERS:-}
if [ -z "$inc" ]; then
    cache=${FFMPEG_HEADERS_CACHE:-$HOME/.cache/enginex-ffmpeg-headers}
    inc=$cache/ffmpeg-$VER-include
    if [ ! -f "$inc/libavcodec/avcodec.h" ]; then
        mkdir -p "$cache"
        tarball=$cache/ffmpeg-$VER.tar.xz
        [ -s "$tarball" ] || { curl -fsSL --retry 3 -o "$tarball.part" "https://ffmpeg.org/releases/ffmpeg-$VER.tar.xz" && mv "$tarball.part" "$tarball"; }
        printf '%s  %s\n' "$SHA" "$tarball" | sha256sum -c --quiet - || { rm -f "$tarball"; echo "checksum mismatch: ffmpeg-$VER.tar.xz" >&2; exit 1; }
        tmp=$(mktemp -d "$cache/unpack.XXXXXX")
        tar -xJf "$tarball" -C "$tmp" --wildcards "ffmpeg-$VER/libavcodec/*.h" "ffmpeg-$VER/libavutil/*.h"
        # configure writes avconfig.h; both deck CPUs are little-endian with fast unaligned access.
        printf '%s\n' '#ifndef AVUTIL_AVCONFIG_H' '#define AVUTIL_AVCONFIG_H' '#define AV_HAVE_BIGENDIAN 0' \
            '#define AV_HAVE_FAST_UNALIGNED 1' '#endif' > "$tmp/ffmpeg-$VER/libavutil/avconfig.h"
        rm -rf "$inc"
        mv "$tmp/ffmpeg-$VER" "$inc"
        rmdir "$tmp"
    fi
fi
# -isystem: FFmpeg's own headers are not held to this file's warnings.
"${cross}gcc" -O2 -Wall -Wextra -Werror -std=gnu11 -isystem "$inc" -Wl,-z,now -Wl,-z,max-page-size=4096 \
    -o "$out" enginex-aacenc.c -ldl
"${cross}strip" "$out"
# The oldest deck glibc is 2.39 (Gen-1 5.3.0): reject newer symbol versions.
if "${cross}readelf" -V "$out" | grep -E 'GLIBC_2\.(39\.|[4-9][0-9])'; then
    echo "enginex-aacenc needs glibc newer than the target's 2.39" >&2
    exit 1
fi
# Only libc and libdl: libavcodec is opened at run time, so a base without it still starts the binary.
needed=$("${cross}readelf" -d "$out" | sed -n 's/.*(NEEDED).*\[\(.*\)\]/\1/p' | sort | tr '\n' ' ')
case $needed in
    "libc.so.6 "|"libc.so.6 libdl.so.2 ") ;;
    *) echo "enginex-aacenc links unexpected libraries: $needed" >&2; exit 1 ;;
esac
