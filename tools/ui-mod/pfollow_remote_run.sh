#!/bin/bash
set -euo pipefail
base=/root/Batam-ARM-Paseo/re-shadow-pos
backup=$(mktemp -d)
cp "$base/shadow_probe.c" "$backup/shadow_probe.c"
cp "$base/shadow_probe.so" "$backup/shadow_probe.so"
cp "$base/probe.qml" "$backup/probe.qml"
cp "$base/repo/rootfs-overlay/etc/enginex/libengine-ui-hook.so" "$backup/hook.so"
restore() {
    cp "$backup/shadow_probe.c" "$base/shadow_probe.c"
    cp "$backup/shadow_probe.so" "$base/shadow_probe.so"
    cp "$backup/probe.qml" "$base/probe.qml"
    cp "$backup/hook.so" "$base/repo/rootfs-overlay/etc/enginex/libengine-ui-hook.so"
    rm -rf "$backup"
}
trap restore EXIT
cp tools/ui-mod/pfollow_feed_probe.c "$base/shadow_probe.c"
arm-linux-gnueabihf-gcc -O2 -Wall -Wextra -Werror -shared -fPIC -o "$base/shadow_probe.so" "$base/shadow_probe.c" -ldl -lpthread
cp tools/ui-mod/pfollow_probe.qml "$base/probe.qml"
cp tools/ui-mod/pfollow_hook.so "$base/repo/rootfs-overlay/etc/enginex/libengine-ui-hook.so"
bash tools/ui-mod/pfollow_run_probe.sh
grep -aE "PFOLOW_QML|PROBE_FEED|engine-ui-hook: paused|PROBE_FINISHED" "$base/probe.log" > "$base/pfollow-result.log"
