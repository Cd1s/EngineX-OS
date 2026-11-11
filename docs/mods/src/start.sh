#!/bin/sh
# Official EngineX Mod lifecycle (busybox sh), no interpreter needed: the helper
# is one static binary per architecture, picked by `uname -m`.
set -eu
cd -- "$(dirname -- "$0")"
mod_id=@MOD_ID@
state=${ENGINEX_MOD_STATE:-/data/enginex/mods-state}/$mod_id
machine=$(uname -m)
case "$machine" in
    aarch64|arm64) helper=bin/enginex-mod-helper-arm64 ;;
    armv7*|armv8l|armhf) helper=bin/enginex-mod-helper-armv7 ;;
    *) echo "No $mod_id helper for this CPU ($machine); expected armv7l or aarch64 in $PWD/bin." >&2; exit 1 ;;
esac
[ -x "$helper" ] || { echo "Missing or non-executable helper: $PWD/$helper" >&2; exit 1; }
helper=$PWD/$helper
case "${1:-serve}" in
    serve) exec "$helper" "$mod_id" serve "$PWD" ;;
    start|enable|install)
        if ! "$helper" "$mod_id" health-once "$PWD" >/dev/null 2>&1; then
            mkdir -p "$state"
            nohup "$helper" "$mod_id" serve "$PWD" >/dev/null 2>&1 &
            echo "$!" >"$state/helper.pid"
        fi
        # Confirm readiness before telling the loader that startup succeeded.
        "$helper" "$mod_id" health "$PWD" || {
            "$helper" "$mod_id" stop "$PWD" || true
            exit 1
        }
        ;;
    stop|disable|uninstall)
        "$helper" "$mod_id" stop "$PWD"
        # A disabled/uninstalled club Mod restores only its recorded optional units.
        if [ "$mod_id" = minimal-club-mode ]; then
            "$helper" "$mod_id" restore "$PWD"
        fi
        ;;
    *) echo 'Usage: start.sh [serve|start|stop]' >&2; exit 2 ;;
esac
