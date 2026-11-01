#!/bin/bash
# Engine OS 5.0.4 / RK3288. This builds artifacts; it never installs on the host.
set -euo pipefail
HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
BUILD=${BUILD_ROOT:-$HERE/.build}
OUT=${OUTPUT_ROOT:-$HERE/out}
KERNEL_SRC=${KERNEL_SRC:-$BUILD/linux}
KERNEL_BUILD=${KERNEL_BUILD:-$BUILD/kernel}
APFS_SRC=${APFS_SRC:-$BUILD/linux-apfs-rw}
HFS_TOOLS_SRC=${HFS_TOOLS_SRC:-$BUILD/hfsprogs}
EXPECTED_RELEASE=${EXPECTED_RELEASE:-6.6.119-rt}
CROSS_COMPILE=${CROSS_COMPILE:-arm-linux-gnueabihf-}
JOBS=${JOBS:-$(getconf _NPROCESSORS_ONLN)}
APFS_REF=${APFS_REF:-cb9c0a0681923eaa090026b4c9357e139dfb355b}
KERNEL_REF=${KERNEL_REF:-v6.6.119}
HFS_URL=https://deb.debian.org/debian/pool/non-free/h/hfsprogs
HFS_SHA=b01b203a97f9a3bf36a027c13ddfc59292730552e62722d690d33bd5c24f5497
HFS_PATCH_SHA=a2752fc536c0b982f0f7b3cf24808a4e7b0f599ed0763f30e0913e015b75c46a
die() { printf 'build_modules: %s\n' "$*" >&2; exit 1; }
usage() {
    cat <<'EOF'
Usage: build_modules.sh [all|fetch|modules|tools|--help]
  fetch    Fetch pinned Linux 6.6.119, linux-apfs-rw and patched hfsprogs.
  modules  Require target kernel source/config/Module.symvers; build ARM .ko.
  tools    Cross-build fsck.hfsplus and stage mount.hfsplus helper.
  all      Fetch missing sources and build both (default).

Required for modules: TARGET_CONFIG, TARGET_SYMVERS, an exact vendor/RT kernel
tree (KERNEL_SRC), or KERNEL_PATCH_DIR to apply vendor/RT patches to fetched Linux.
Optional: CROSS_COMPILE, HFS_SYSROOT, HFS_CFLAGS, HFS_LDFLAGS, EXPECTED_RELEASE,
BUILD_ROOT, OUTPUT_ROOT, JOBS, APFS_SRC, APFS_REF, HFS_TOOLS_SRC.
Do not use vanilla upstream headers as a substitute for the firmware build ABI.
EOF
}
mode=${1:-all}
case $mode in --help|-h) usage; exit 0 ;; all|fetch|modules|tools) ;; *) usage >&2; exit 2 ;; esac
[[ $EXPECTED_RELEASE == 6.6.119-rt* && $EXPECTED_RELEASE != */* ]] || die 'expected a 6.6.119-rt release'
[[ $JOBS =~ ^[1-9][0-9]*$ ]] || die 'invalid JOBS'
mkdir -p "$BUILD" "$OUT"
BUILD=$(cd "$BUILD" && pwd)
OUT=$(cd "$OUT" && pwd)

fetch_git() {
    local url=$1 ref=$2 dest=$3
    if [[ -d $dest ]]; then
        [[ -f $dest/Makefile ]] || die "incomplete source directory: $dest"
        return 0
    fi
    mkdir -p "$dest"
    git -C "$dest" init -q
    git -C "$dest" remote add origin "$url"
    git -C "$dest" fetch --depth=1 origin "$ref"
    git -C "$dest" checkout --detach FETCH_HEAD
}
download() {
    local url=$1 dest=$2 sha=$3
    if [[ ! -f $dest ]]; then
        curl --fail --location --retry 3 --output "$dest.part" "$url"
        mv "$dest.part" "$dest"
    fi
    printf '%s  %s\n' "$sha" "$dest" | sha256sum -c -
}
fetch_tools() {
    [[ -d $HFS_TOOLS_SRC ]] && return 0
    download "$HFS_URL/hfsprogs_540.1.linux3.orig.tar.gz" "$BUILD/hfsprogs.tar.gz" "$HFS_SHA"
    download "$HFS_URL/hfsprogs_540.1.linux3-6.debian.tar.xz" "$BUILD/hfsprogs-debian.tar.xz" "$HFS_PATCH_SHA"
    local staging=$BUILD/hfsprogs-unpack patch
    [[ ! -e $staging ]] || die "remove incomplete unpack directory: $staging"
    mkdir -p "$staging/src"
    tar -xzf "$BUILD/hfsprogs.tar.gz" -C "$staging/src" --strip-components=1
    tar -xJf "$BUILD/hfsprogs-debian.tar.xz" -C "$staging"
    while IFS= read -r patch; do
        [[ -n $patch && $patch != \#* ]] || continue
        patch -d "$staging/src" -p1 < "$staging/debian/patches/$patch"
    done < "$staging/debian/patches/series"
    patch -d "$staging/src" -p1 < "$HERE/patches/hfsprogs-cross-ar.patch"
    mv "$staging/src" "$HFS_TOOLS_SRC"
}
fetch_modules() {
    local patch
    if [[ ! -d $KERNEL_SRC ]]; then
        fetch_git https://git.kernel.org/pub/scm/linux/kernel/git/stable/linux.git "$KERNEL_REF" "$KERNEL_SRC"
        if [[ -n ${KERNEL_PATCH_DIR:-} ]]; then
            shopt -s nullglob
            patches=("$KERNEL_PATCH_DIR"/*.patch)
            ((${#patches[@]})) || die 'KERNEL_PATCH_DIR has no patches'
            for patch in "${patches[@]}"; do
                git -C "$KERNEL_SRC" apply "$patch"
            done
        fi
    fi
    fetch_git https://github.com/linux-apfs/linux-apfs-rw.git "$APFS_REF" "$APFS_SRC"
}
check_compiler() {
    command -v "${CROSS_COMPILE}gcc" >/dev/null || die "missing compiler: ${CROSS_COMPILE}gcc"
    local triple
    triple=$("${CROSS_COMPILE}gcc" -dumpmachine)
    [[ $triple == arm*-linux-* ]] || die "expected ARM Linux compiler, got $triple"
}
build_modules() {
    [[ -f ${TARGET_CONFIG:-} ]] || die 'TARGET_CONFIG must be the firmware kernel .config'
    [[ -s ${TARGET_SYMVERS:-} ]] || die 'TARGET_SYMVERS must be the firmware Module.symvers (modules_prepare cannot generate it)'
    [[ -f $KERNEL_SRC/Makefile && -f $APFS_SRC/Makefile ]] || die 'missing sources; use fetch first'
    check_compiler
    command -v modinfo >/dev/null || die 'install host kmod (modinfo)'
    mkdir -p "$KERNEL_BUILD"
    KERNEL_SRC=$(cd "$KERNEL_SRC" && pwd)
    KERNEL_BUILD=$(cd "$KERNEL_BUILD" && pwd)
    cp "$TARGET_CONFIG" "$KERNEL_BUILD/.config"
    # Change only the filesystem module selection, preserving the target ABI.
    "$KERNEL_SRC/scripts/config" --file "$KERNEL_BUILD/.config" --module HFSPLUS_FS
    local -a kmake=(make -C "$KERNEL_SRC" "O=$KERNEL_BUILD" ARCH=arm "CROSS_COMPILE=$CROSS_COMPILE")
    "${kmake[@]}" olddefconfig
    grep -qx 'CONFIG_ARM=y' "$KERNEL_BUILD/.config" || die 'target config is not ARM'
    grep -qx 'CONFIG_CPU_V7=y' "$KERNEL_BUILD/.config" || die 'target config is not ARMv7'
    grep -qx 'CONFIG_PREEMPT_RT=y' "$KERNEL_BUILD/.config" || die 'missing RT patch/config; supply exact vendor/RT sources'
    grep -qx 'CONFIG_MODULES=y' "$KERNEL_BUILD/.config" || die 'target lacks module support'
    grep -Eq '^CONFIG_NLS=[ym]$' "$KERNEL_BUILD/.config" || die 'target must provide NLS support'
    grep -Eq '^CONFIG_OVERLAY_FS=[ym]$' "$KERNEL_BUILD/.config" || die 'target must provide OverlayFS'
    local release
    release=$("${kmake[@]}" -s kernelrelease)
    [[ $release == "$EXPECTED_RELEASE" ]] || die "ABI release mismatch: $release != $EXPECTED_RELEASE"
    "${kmake[@]}" -j"$JOBS" modules_prepare
    cp "$TARGET_SYMVERS" "$KERNEL_BUILD/Module.symvers"
    local hfs=$BUILD/hfsplus-module apfs=$BUILD/apfs-module
    mkdir -p "$hfs" "$apfs"
    cp -a "$KERNEL_SRC/fs/hfsplus/." "$hfs/"
    tar -C "$APFS_SRC" --exclude=.git -cf - . | tar -C "$apfs" -xf -
    (cd "$apfs" && sh ./genver.sh)
    # APFS_CONFIG is explicitly empty: never compile in default-write mode.
    "${kmake[@]}" -j"$JOBS" "M=$hfs" modules
    "${kmake[@]}" -j"$JOBS" "M=$apfs" APFS_CONFIG= modules
    local dest=$OUT/lib/modules/$release/extra module vermagic
    mkdir -p "$dest"
    for module in "$hfs/hfsplus.ko" "$apfs/apfs.ko"; do
        "${CROSS_COMPILE}readelf" -h "$module" | grep -q 'Machine:.*ARM' || die "not ARM: $module"
        vermagic=$(modinfo -F vermagic "$module")
        [[ $vermagic == "$release "* ]] || die "bad vermagic: $vermagic"
        install -m 0644 "$module" "$dest/"
    done
    {
        printf 'release=%s\narch=arm\ncross_compile=%s\n' "$release" "$CROSS_COMPILE"
        if [[ -e $APFS_SRC/.git ]]; then
            printf 'apfs_commit=%s\n' "$(git -C "$APFS_SRC" rev-parse HEAD)"
        else
            printf 'apfs_commit=supplied-source-without-git\n'
        fi
        sha256sum "$TARGET_CONFIG" "$TARGET_SYMVERS" "$KERNEL_BUILD/.config" "$dest/"*.ko
        if [[ -e $KERNEL_SRC/.git ]]; then
            git -C "$KERNEL_SRC" rev-parse HEAD
            git -C "$KERNEL_SRC" diff --binary | sha256sum
        else
            printf 'kernel_commit=supplied-source-without-git\n'
            sha256sum "$KERNEL_SRC/Makefile"
        fi
    } > "$OUT/modules-build-manifest.txt"
}
build_tools() {
    [[ -f $HFS_TOOLS_SRC/Makefile ]] || die 'missing hfsprogs; use fetch first'
    check_compiler
    local -a cc=("${CROSS_COMPILE}gcc")
    [[ -z ${HFS_SYSROOT:-} ]] || cc+=("--sysroot=$HFS_SYSROOT")
    # Debian patches remove clang Blocks and fix ARM stdarg/glibc compatibility.
    # Set CFLAGS in the environment so upstream's include/defines are appended.
    make -C "$HFS_TOOLS_SRC" clean
    CFLAGS="${HFS_CFLAGS:--O2 -march=armv7-a -mfpu=neon}" \
        make -C "$HFS_TOOLS_SRC" -j"$JOBS" "CC=${cc[*]}" "AR=${CROSS_COMPILE}ar" \
        "LDFLAGS=${HFS_LDFLAGS:--Wl,--build-id}"
    local fsck=$HFS_TOOLS_SRC/fsck_hfs.tproj/fsck_hfs
    "${CROSS_COMPILE}readelf" -h "$fsck" | grep -q 'Machine:.*ARM' || die 'fsck is not ARM'
    mkdir -p "$OUT/sbin"
    install -m 0755 "$fsck" "$OUT/sbin/fsck.hfsplus"
    install -m 0755 "$HERE/mount.hfsplus" "$OUT/sbin/mount.hfsplus"
    sha256sum "$OUT/sbin/"* > "$OUT/tools-build-manifest.txt"
}
case $mode in
    fetch) fetch_modules; fetch_tools ;;
    modules) build_modules ;;
    tools) fetch_tools; build_tools ;;
    all) fetch_modules; fetch_tools; build_modules; build_tools ;;
esac
printf 'Artifacts staged under %s; nothing installed on this host.\n' "$OUT"
