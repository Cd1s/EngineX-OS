#!/bin/sh
# Install/upgrade enginex-relay on <relay-host>. Run as root:
#   sh install.sh /path/to/enginex-relay [service unit]
# Re-running is safe; state in /var/lib/enginex-relay (config.json) is kept, and
# so is an installed unit while the given one still has the placeholder -public.
set -eu
BIN=${1:?binary}; UNIT=${2:-}
HERE=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=deploy/relay-host/migrate.sh
. "$HERE/migrate.sh"
INSTALLED=/etc/systemd/system/enginex-relay.service
PREVIOUS=$(installed_relay_unit)
[ -n "$UNIT" ] || UNIT=${PREVIOUS:-$HERE/enginex-relay.service}

if grep -q 'relay\.example\.com' "$UNIT"; then
  # The placeholder must never replace a working -public (OAuth callbacks, #28).
  if [ -z "$PREVIOUS" ]; then
    echo "install.sh: set -public in $UNIT to this relay's public URL first" >&2
    exit 1
  fi
  echo "install.sh: $UNIT still has the placeholder -public; keeping $PREVIOUS"
  UNIT=$PREVIOUS
fi
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
rewrite_relay_unit "$UNIT" > "$WORK/enginex-relay.service"
migrate_relay
if ! cmp -s "$BIN" /usr/local/bin/enginex-relay; then
  install -m 0755 "$BIN" /usr/local/bin/enginex-relay
fi
install -m 0644 "$WORK/enginex-relay.service" "$INSTALLED"
relay_compatibility_aliases
systemctl daemon-reload
systemctl enable enginex-relay.service
systemctl restart enginex-relay.service
for _ in 1 2 3 4 5 6 7 8 9 10; do
  curl -fsS http://127.0.0.1:6595/healthz && exit 0
  sleep 1
done
journalctl -u enginex-relay -n 30 --no-pager; exit 1
