#!/bin/bash
# Pad LED Brightness reaches the pads at once (#325), in stock Engine 5.3.0 with the real UI hook, natively on the
# build host: the real settings picker action, the real patched controller-side QML (GlobalAssignmentConfig,
# ColorOutputAssignment, instantiated with a stub MIDI device because the sandbox has none) and the real hook,
# with a driver standing in for Engine's sequencer writes (led_brightness_driver.c). After the pick, the first RGB
# LED SysEx at the new level must leave the hook within LEDB_MAX_IDLE_MS on a deck with no LED activity, and within
# LEDB_MAX_PLAY_MS on a deck whose LEDs keep changing.
#   bash tools/az01-packer/test_led_brightness_sandbox.sh [PRODUCT ...]     (default: JP07 JC11)
# Environment: ENGINEX_LAB (as test_fx_selector_sandbox.sh), LEDB_PICKS="60 100 40 80 20", LEDB_MAX_IDLE_MS=500, LEDB_MAX_PLAY_MS=400,
# LEDB_POST_DELAY_MS=0 (artificial delay on ?ledBrightness POST), LEDB_SLOW_DELAY_MS=350 (slow-save idle pass; set LEDB_SKIP_SLOW=1 to omit).
. "${ENGINEX_MIGRATION:-$(git rev-parse --show-toplevel)/rootfs-overlay/usr/libexec/enginex-migration}"
ENGINEX_LAB=${ENGINEX_LAB:-$legacy_lab}
set -uo pipefail
REPO=$(cd "$(dirname "$0")/../.." && pwd)
PICKS=${LEDB_PICKS:-60 100 40 80 20}
MAX_IDLE=${LEDB_MAX_IDLE_MS:-500}
MAX_PLAY=${LEDB_MAX_PLAY_MS:-400}
SLOW_DELAY=${LEDB_SLOW_DELAY_MS:-350}
products=("$@"); [ ${#products[@]} -gt 0 ] || products=(JP07 JC11)
W0=${LEDB_SANDBOX_DIR:-/var/tmp/led-brightness-sandbox}
rc=0

run_case() {
    local product=$1 suffix=$2 picks=$3 max_idle=$4 max_play=$5 post_delay=$6
    local W=$W0-$product$suffix
    [ ! -e "$W" ] || { echo "refusing existing sandbox $W" >&2; return 1; }
    mkdir -p "$W"
    local picks_js="[$(echo "$picks" | sed 's/ /, /g')]"
    sed -e "s/LEDB_PICKS/$picks_js/" -e "s/LEDB_MAX_IDLE_MS/$max_idle/" -e "s/LEDB_MAX_PLAY_MS/$max_play/" \
        "$REPO/tools/az01-packer/led-brightness-probe.qml" > "$W/probe.qml"
    echo "== $product$suffix (picks: $picks; idle limit $max_idle ms, playing limit $max_play ms, post delay $post_delay ms)"
    env ENGINEX_LAB="$ENGINEX_LAB" ENGINEX_MIGRATION="${ENGINEX_MIGRATION:-}" FX_SANDBOX_DIR="$W/fx" FX_PRODUCT="$product" \
        FX_LED_POST_DELAY_MS="$post_delay" \
        FX_DRIVER="$REPO/tools/az01-packer/led_brightness_driver.c" FX_DRIVER_TAG=LEDB FX_QML_PROBE="$W/probe.qml" \
        FX_TIMEOUT=$(( 40 + 15 * $(echo "$picks" | wc -w) )) \
        setpriv --inh-caps=-sys_time --bounding-set=-sys_time bash "$REPO/tools/az01-packer/test_fx_selector_sandbox.sh" > "$W/run.log" 2>&1
    local run_rc=$?
    grep -a -E "^LEDB |LEDB_QML" "$W/fx/engine.log" | cut -c1-230
    [ $run_rc -eq 0 ] || { echo "LEDB $product$suffix|FAIL: sandbox run rc=$run_rc (see $W/run.log)"; }
    if [ "${LEDB_KEEP:-0}" != 1 ]; then rm -rf "$W"; fi
    return $run_rc
}

for product in "${products[@]}"; do
    run_case "$product" "" "$PICKS" "$MAX_IDLE" "$MAX_PLAY" "${LEDB_POST_DELAY_MS:-0}" || rc=1
    if [ -z "${LEDB_POST_DELAY_MS:-}" ] && [ "${LEDB_SKIP_SLOW:-0}" != 1 ] && [ "$SLOW_DELAY" -gt 0 ]; then
        run_case "$product" "-slow" "60" "$(( MAX_IDLE > 1500 ? MAX_IDLE : 1500 ))" "$(( MAX_PLAY > 1500 ? MAX_PLAY : 1500 ))" "$SLOW_DELAY" || rc=1
    fi
done
exit $rc
