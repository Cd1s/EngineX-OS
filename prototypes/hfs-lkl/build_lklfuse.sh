#!/bin/bash
# Cross-build armhf lklfuse (LKL 6.12 + patched hfsplus + static libfuse3).
# Builds inside BUILD_ROOT and writes artifacts to OUTPUT_ROOT; never writes /etc or /usr.
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
BUILD=${BUILD_ROOT:-$HERE/.build}
OUT=${OUTPUT_ROOT:-$HERE/out}
SRC=$BUILD/src
SYS=$BUILD/sysroot-armhf
CROSS_COMPILE=${CROSS_COMPILE:-arm-linux-gnueabihf-}
JOBS=${JOBS:-$(getconf _NPROCESSORS_ONLN)}

LKL_URL=${LKL_URL:-https://github.com/lkl/linux.git}
LKL_REF=${LKL_REF:-d0f76a77e8c2ca68bd7d96190b8da09040eab072}
LIBFUSE_URL=${LIBFUSE_URL:-https://github.com/libfuse/libfuse.git}
LIBFUSE_REF=${LIBFUSE_REF:-fuse-3.17.2}
MESON_URL=${MESON_URL:-https://github.com/mesonbuild/meson.git}
MESON_REF=${MESON_REF:-1.7.0}
SAMURAI_URL=${SAMURAI_URL:-https://github.com/michaelforney/samurai.git}
SAMURAI_REF=${SAMURAI_REF:-b37524dbb762adfd140e5c306fc4b1aa258971bf}

die() { printf 'build_lklfuse: %s\n' "$*" >&2; exit 1; }
usage() {
    cat <<'EOF'
Usage: build_lklfuse.sh [all|fetch|libfuse|lklfuse|--help]
  fetch    Clone pinned lkl/linux, libfuse, meson, and samurai into BUILD_ROOT.
  libfuse  Cross-build static libfuse3.a for armhf into BUILD_ROOT/sysroot-armhf.
  lklfuse  Patch LKL 6.12 hfsplus and cross-build stripped armhf lklfuse.
  all      Run fetch, libfuse, and lklfuse (default).
EOF
}

fetch_repo() {
    local url=$1 ref=$2 dest=$3
    if [[ -d $dest/.git ]]; then
        return 0
    fi
    mkdir -p "$(dirname "$dest")"
    if ! git clone --depth=1 --branch "$ref" "$url" "$dest" 2>/dev/null; then
        git clone --depth=1 "$url" "$dest"
        git -C "$dest" fetch --depth=1 origin "$ref" || true
        git -C "$dest" checkout -q "$ref"
    fi
}

do_fetch() {
    fetch_repo "$LKL_URL" "$LKL_REF" "$SRC/lkl-linux"
    fetch_repo "$LIBFUSE_URL" "$LIBFUSE_REF" "$SRC/libfuse"
    if ! command -v meson >/dev/null 2>&1 && [[ ! -f $BUILD/tools/meson/meson.py ]]; then
        fetch_repo "$MESON_URL" "$MESON_REF" "$BUILD/tools/meson"
    fi
    if ! command -v ninja >/dev/null 2>&1 && [[ ! -x $BUILD/tools/samurai/samu ]]; then
        fetch_repo "$SAMURAI_URL" "$SAMURAI_REF" "$BUILD/tools/samurai"
        make -C "$BUILD/tools/samurai" -j"$JOBS"
    fi
}

do_libfuse() {
    command -v "${CROSS_COMPILE}gcc" >/dev/null 2>&1 || die "missing ${CROSS_COMPILE}gcc"
    local meson_bin ninja_bin
    if command -v meson >/dev/null 2>&1; then
        meson_bin=(meson)
    else
        meson_bin=(python3 "$BUILD/tools/meson/meson.py")
    fi
    if command -v ninja >/dev/null 2>&1; then
        ninja_bin=ninja
    else
        ninja_bin=$BUILD/tools/samurai/samu
    fi

    local cross=$BUILD/armhf-cross.ini bld=$BUILD/build-libfuse3
    cat > "$cross" <<EOF
[binaries]
c = '${CROSS_COMPILE}gcc'
ar = '${CROSS_COMPILE}ar'
strip = '${CROSS_COMPILE}strip'
pkg-config = 'false'

[host_machine]
system = 'linux'
cpu_family = 'arm'
cpu = 'armv7l'
endian = 'little'

[built-in options]
c_args = ['-Os', '-D_FILE_OFFSET_BITS=64']
EOF
    rm -rf "$bld" "$SYS"
    NINJA=$ninja_bin "${meson_bin[@]}" setup "$bld" "$SRC/libfuse" \
        --cross-file "$cross" \
        --prefix="$SYS" \
        --libdir=lib \
        -Ddefault_library=static \
        -Dutils=false -Dtests=false -Dexamples=false -Duseroot=false
    "$ninja_bin" -C "$bld" -j"$JOBS"
    DESTDIR="" "$ninja_bin" -C "$bld" install
    [[ -f $SYS/lib/libfuse3.a && -f $SYS/include/fuse3/fuse.h ]] || die "static libfuse3 install failed"
}

do_lklfuse() {
    command -v "${CROSS_COMPILE}gcc" >/dev/null 2>&1 || die "missing ${CROSS_COMPILE}gcc"
    [[ -f $SYS/lib/libfuse3.a ]] || die "missing $SYS/lib/libfuse3.a (run libfuse first)"
    local lkl_src=$BUILD/lkl-src lkl_out=$BUILD/out-lkl
    rm -rf "$lkl_src" "$lkl_out"
    cp -a "$SRC/lkl-linux" "$lkl_src"
    for p in "$HERE/patches"/[0-9]*.patch; do
        git -C "$lkl_src" apply "$p"
    done

    mkdir -p "$lkl_out/tools/lkl/include" "$lkl_out/tools/lkl/tests"
    SYSROOT_FLAGS="-isystem $SYS/include" \
        make -C "$lkl_src/tools/lkl" O="$lkl_out" CROSS_COMPILE="$CROSS_COMPILE" conf
    cat "$HERE/lkl-hfsplus.config" >> "$lkl_out/tools/lkl/kernel.config"
    cat >> "$lkl_out/tools/lkl/Makefile.conf" <<EOF
CFLAGS += -isystem $SYS/include
LDFLAGS += -L$SYS/lib
EOF
    make -C "$lkl_src/tools/lkl" O="$lkl_out" CROSS_COMPILE="$CROSS_COMPILE" -j"$JOBS" \
        "$lkl_out//tools/lkl/lklfuse"

    mkdir -p "$OUT/bin"
    cp -f "$lkl_out/tools/lkl/lklfuse" "$OUT/bin/lklfuse.unstripped"
    cp -f "$lkl_out/tools/lkl/lklfuse" "$OUT/bin/lklfuse"
    "${CROSS_COMPILE}strip" "$OUT/bin/lklfuse"

    "${CROSS_COMPILE}readelf" -h "$OUT/bin/lklfuse" | grep -q 'Machine:.*ARM' || die "not an ARM binary"
    if "${CROSS_COMPILE}readelf" -d "$OUT/bin/lklfuse" | grep -q 'libfuse3'; then
        die "libfuse3 must be statically linked"
    fi
    ls -lh "$OUT/bin/lklfuse" "$OUT/bin/lklfuse.unstripped"
}

cmd=${1:-all}
case "$cmd" in
    -h|--help) usage ;;
    fetch) do_fetch ;;
    libfuse) do_libfuse ;;
    lklfuse) do_lklfuse ;;
    all) do_fetch; do_libfuse; do_lklfuse ;;
    *) usage >&2; exit 2 ;;
esac
