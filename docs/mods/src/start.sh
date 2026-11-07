#!/bin/sh
set -eu
cd -- "$(dirname -- "$0")"
mod_id=@MOD_ID@
state=${ENGINEX_MOD_STATE:-/data/enginex/mods-state}/$mod_id
case "${1:-serve}" in
    serve) exec python3 -B runtime.py "$mod_id" serve ;;
    start|enable|install)
        command -v python3 >/dev/null || { echo 'Python >=3.9 is required; no dependencies are installed automatically.' >&2; exit 1; }
        if ! python3 -B "$PWD/runtime.py" "$mod_id" health-once >/dev/null 2>&1; then
            mkdir -p "$state"
            nohup python3 -B "$PWD/runtime.py" "$mod_id" serve >/dev/null 2>&1 &
            echo "$!" >"$state/helper.pid"
        fi
        # Confirm readiness before telling the loader that startup succeeded.
        python3 -B "$PWD/runtime.py" "$mod_id" health || {
            python3 -B "$PWD/runtime.py" "$mod_id" stop || true
            exit 1
        }
        ;;
    stop|disable|uninstall)
        python3 -B "$PWD/runtime.py" "$mod_id" stop
        # A disabled/uninstalled club Mod restores only its recorded optional units.
        if [ "$mod_id" = minimal-club-mode ]; then
            python3 -B runtime.py "$mod_id" restore
        fi
        ;;
    *) echo 'Usage: start.sh [serve|start|stop]' >&2; exit 2 ;;
esac
