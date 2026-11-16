#!/bin/sh
# Install/upgrade enginex-relay on <relay-host>. Run as root:
#   sh install.sh /path/to/enginex-relay /path/to/enginex-deemix-relay.service
# Re-running is safe; state in /var/lib/enginex-relay (config.json) is kept, and
# so is an installed unit while the given one still has the placeholder -public.
set -eu
BIN=${1:?binary}; UNIT=${2:?unit file}
INSTALLED=/etc/systemd/system/enginex-deemix-relay.service

if grep -q 'relay\.example\.com' "$UNIT"; then
  # The placeholder must never replace a working -public (OAuth callbacks, #28).
  if [ ! -f "$INSTALLED" ]; then
    echo "install.sh: set -public in $UNIT to this relay's public URL first" >&2
    exit 1
  fi
  echo "install.sh: $UNIT still has the placeholder -public; keeping $INSTALLED"
  UNIT=
fi
install -m 0755 "$BIN" /usr/local/bin/enginex-relay
[ -z "$UNIT" ] || install -m 0644 "$UNIT" "$INSTALLED"
systemctl daemon-reload
systemctl enable enginex-deemix-relay.service >/dev/null 2>&1
systemctl restart enginex-deemix-relay.service
for i in 1 2 3 4 5 6 7 8 9 10; do
  curl -fsS http://127.0.0.1:6595/healthz && exit 0
  sleep 1
done
journalctl -u enginex-deemix-relay -n 30 --no-pager; exit 1
