#!/bin/sh
# Mod lifecycle for the deck's busybox sh. The Mod loader runs `start.sh start`
# when the Mod is enabled (and at boot) and `start.sh stop` when it is disabled.
set -eu
cd -- "$(dirname -- "$0")"
id=my-mod   # your mod.json "id"
state=${ENGINEX_MOD_STATE:-/data/enginex/mods-state}/$id
case "$(uname -m)" in
    aarch64|arm64) helper=$PWD/bin/helper-arm64 ;;
    armv7*|armv8l) helper=$PWD/bin/helper-armv7 ;;
    *) echo "no helper for $(uname -m)" >&2; exit 1 ;;
esac
case "${1:-}" in
    start)
        mkdir -p "$state"
        nohup "$helper" serve >/dev/null 2>&1 &
        echo "$!" >"$state/helper.pid"
        "$helper" health
        ;;
    stop)
        [ -f "$state/helper.pid" ] && kill "$(cat "$state/helper.pid")" 2>/dev/null || true
        rm -f "$state/helper.pid"
        ;;
    *) echo 'Usage: start.sh start|stop' >&2; exit 2 ;;
esac
