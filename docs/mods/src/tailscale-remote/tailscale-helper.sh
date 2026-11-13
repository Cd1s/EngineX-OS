#!/bin/sh
set -eu
client=/data/enginex/tailscale/bin/tailscale
case "${1:-status}" in
    start|stop|install|uninstall|enable|disable) exit 0 ;;
esac
if [ ! -x "$client" ]; then
    echo 'Tailscale is not installed. Open Web Console > Remote access to install and sign in.' >&2
    exit 3
fi
# The daemon's socket: /run/enginex-tailscaled.sock since 5.3.0.5 (#234), the older name before.
socket=/run/enginex-tailscaled.sock
[ -S "$socket" ] || [ ! -S /run/sc5000-tailscaled.sock ] || socket=/run/sc5000-tailscaled.sock
case "${1:-status}" in
    status) exec "$client" --socket="$socket" status ;;
    json) exec "$client" --socket="$socket" status --json ;;
    *) echo 'Usage: tailscale-helper.sh [status|json]' >&2; exit 2 ;;
esac
