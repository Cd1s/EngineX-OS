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
case "${1:-status}" in
    status) exec "$client" --socket=/run/sc5000-tailscaled.sock status ;;
    json) exec "$client" --socket=/run/sc5000-tailscaled.sock status --json ;;
    *) echo 'Usage: tailscale-helper.sh [status|json]' >&2; exit 2 ;;
esac
