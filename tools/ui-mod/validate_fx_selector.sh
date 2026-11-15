#!/bin/bash
# Full task gate; invoke through remote-build on the build host.
set -uo pipefail
repo=$(cd "$(dirname "$0")/../.." && pwd)
cd "$repo"
work=$repo/out/fx-validation
mkdir -p "$work"
failed=0
run() {
    local name=$1; shift
    "$@" > "$work/$name.log" 2>&1
    local rc=$?
    echo "$name exit=$rc"
    if [ "$rc" != 0 ]; then failed=1; tail -n 12 "$work/$name.log" | cut -c1-200 | head -n 40; fi
}
run hook-first sh tools/ui-mod/build.sh
cp rootfs-overlay/etc/enginex/libengine-ui-hook.so "$work/hook-first.so"
run hook-second sh tools/ui-mod/build.sh
run reproducible cmp "$work/hook-first.so" rootfs-overlay/etc/enginex/libengine-ui-hook.so
cp rootfs-overlay/etc/enginex/libengine-ui-hook.so rootfs-overlay/usr/local/lib/enginex/libengine-ui-hook.so
sha256sum rootfs-overlay/etc/enginex/libengine-ui-hook.so rootfs-overlay/usr/local/lib/enginex/libengine-ui-hook.so > "$work/hook-sha256.txt"
run hook-az04 env CROSS= OUT="$work/az04-hook.so" sh tools/ui-mod/build.sh
run gofmt bash -c 'gofmt -l . > out/fx-validation/gofmt-files.txt && test ! -s out/fx-validation/gofmt-files.txt'
run vet go vet ./...
run race go test -race ./...
run python python3 -m unittest discover -s tools/az01-packer
run overlay bash tests/test_rootfs_overlay.sh
run dsp bash tools/ui-mod/test_fx_selector.sh
run docs node --check docs/site/content.js
echo 'Go race filtered tail:'
grep -v '^ok' "$work/race.log" | tail -n 20 | cut -c1-200 | head -n 40 || true
echo 'Python summary:'
grep -E '^(Ran|OK|FAILED)' "$work/python.log" | cut -c1-200 | head -n 40 || true
echo 'Overlay tail:'
tail -n 3 "$work/overlay.log" | cut -c1-200 | head -n 40
echo 'Hook hashes:'
cat "$work/hook-sha256.txt" | cut -c1-200 | head -n 40
exit "$failed"
