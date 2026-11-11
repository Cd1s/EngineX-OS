#!/bin/sh
# Official Mods on the real deck rootfs, without Python (#208).
#
# Run as root on an arm64 build host (32-bit EL0 for the armhf case), never on a deck:
#   sh tools/mods/test_mods_chroot.sh <rootfs-dir> <armv7|arm64> [packages-dir]
# <rootfs-dir> is an unpacked Engine OS rootfs (gen-1 5.3.0 rootfs.ext2 mounted
# read-only, gen-2 SC LIVE 4 or AZ04 rootfs directory). For each official Mod the
# package is copied to a tmpfs /data/mods/<id>, then `start.sh start` -> health (GET
# /status from inside the chroot) -> `start.sh stop` run in `unshare -m -n -p` + chroot,
# so nothing touches the host. armv7 runs under `setarch linux32` (uname -m: armv8l).
set -eu
ROOTFS=$(cd "$1" && pwd)
ARCH=$2
REPO=$(cd "$(dirname "$0")/../.." && pwd)
PKGS=$(cd "${3:-$REPO/docs/mods/packages}" && pwd)
case "$ARCH" in armv7) personality="setarch linux32" ;; arm64) personality= ;; *) echo "arch must be armv7 or arm64" >&2; exit 2 ;; esac
[ "$(id -u)" = 0 ] || { echo "needs root (unshare/chroot)" >&2; exit 2; }
WORK=$(mktemp -d /var/tmp/mods-chroot.XXXXXX)
trap 'rm -rf "$WORK"' EXIT
fail=0
for pkg in "$PKGS"/*.tar.gz; do
    id=$(basename "$pkg" | sed 's/-[0-9][0-9.]*\.tar\.gz$//')
    [ "$id" = tailscale-remote ] && continue
    mkdir -p "$WORK/$id"
    tar -xzf "$pkg" -C "$WORK/$id"
    out=$WORK/$id.log
    if unshare -m -n -p -f --kill-child sh -s "$ROOTFS" "$WORK/$id" "$id" "$personality" >"$out" 2>&1 <<'EOS'
set -eu
ROOTFS=$1 PKG=$2 ID=$3 PERS=$4
R=$PKG.root && mkdir "$R" # inside the caller's work dir, removed with it
mount --make-rprivate /
mount --bind "$ROOTFS" "$R"
mount -o remount,bind,ro "$R"
for d in tmp run data var/volatile; do [ -d "$R/$d" ] && mount -t tmpfs tmpfs "$R/$d"; done
mount -t proc proc "$R/proc"
mount -t tmpfs tmpfs "$R/dev" && for n in null zero urandom random; do touch "$R/dev/$n"; mount --bind "/dev/$n" "$R/dev/$n"; done
ip link set lo up 2>/dev/null || ifconfig lo 127.0.0.1 up
mkdir -p "$R/data/mods/$ID" && cp -a "$PKG/." "$R/data/mods/$ID/"
run() { $PERS chroot "$R" /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin ENGINEX_MOD_STATE=/data/enginex/mods-state "$@"; }
if run sh -c 'command -v python3 || command -v python'; then echo "FAIL python present in the rootfs PATH"; exit 1; fi
echo "uname -m: $(run uname -m)"
run sh /data/mods/$ID/start.sh start
helper=$(run sh -c 'case $(uname -m) in aarch64|arm64) echo arm64;; *) echo armv7;; esac')
pid=$(cat "$R/data/enginex/mods-state/$ID/helper.pid")
grep -q "enginex-mod-helper-$helper" "$R/proc/$pid/cmdline" 2>/dev/null || tr '\0' ' ' <"$R/proc/$pid/cmdline"
run /data/mods/$ID/bin/enginex-mod-helper-$helper "$ID" health-once /data/mods/$ID && echo "health OK ($helper, pid $pid)"
run sh /data/mods/$ID/start.sh stop
[ ! -e "$R/data/enginex/mods-state/$ID/helper.pid" ] || { echo "FAIL pid file left"; exit 1; }
[ ! -d "$R/proc/$pid" ] || grep -q '^State:.*Z' "$R/proc/$pid/status" || { echo "FAIL helper still running"; exit 1; }
echo "stop OK"
EOS
    then
        echo "PASS $id ($ARCH): $(grep -h 'uname -m\|health OK' "$out" | tr '\n' ' ')"
    else
        echo "FAIL $id ($ARCH):"; sed 's/^/    /' "$out"; fail=1
    fi
done
exit $fail
