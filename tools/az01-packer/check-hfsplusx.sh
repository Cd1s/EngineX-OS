#!/bin/sh
# Gen-1 LoadPin loads kernel modules only from the root file system: with no hfsplusx.ko there, or
# one built for another kernel, every HFS+ stick quietly stays read-only (hfs-rw-deck.md).
# Usage: check-hfsplusx.sh <rootfs.ext2>. Exit 0 when /usr/lib/modules/<kernel>/extra/hfsplusx.ko
# is there and carries the vermagic of the image's own modules. build-release.sh and the release
# gate run it on every 1st-gen rootfs.
set -eu
fs=${1:?rootfs.ext2}
t=$(mktemp -d)
trap 'rm -rf "$t"' EXIT
PATH="$PATH:/usr/sbin:/sbin"
kernels=$(debugfs -R "ls -p /usr/lib/modules" "$fs" 2>/dev/null | awk -F/ '$6 ~ /^[0-9]/ {print $6}')
[ "$(printf '%s\n' "$kernels" | grep -c .)" = 1 ] || { echo "$fs: want one kernel in /usr/lib/modules, found: $kernels" >&2; exit 1; }
dir=/usr/lib/modules/$kernels
stock=$(debugfs -R "cat $dir/modules.dep" "$fs" 2>/dev/null | sed -n '1s/:.*//p')
[ -n "$stock" ] || { echo "$fs: no module of the image's own in $dir/modules.dep" >&2; exit 1; }
debugfs -R "dump $dir/$stock $t/stock.ko" "$fs" >/dev/null 2>&1
debugfs -R "dump $dir/extra/hfsplusx.ko $t/hfsplusx.ko" "$fs" >/dev/null 2>&1
[ -s "$t/hfsplusx.ko" ] || { echo "$fs: no $dir/extra/hfsplusx.ko" >&2; exit 1; }
want=$(modinfo -F vermagic "$t/stock.ko")
got=$(modinfo -F vermagic "$t/hfsplusx.ko")
[ -n "$want" ] && [ "$got" = "$want" ] || { echo "$fs: hfsplusx.ko vermagic '$got', the kernel's modules '$want'" >&2; exit 1; }
