#!/bin/sh
# Install/upgrade sc5000-relay on <relay-host>. Run as root:
#   sh install.sh /path/to/sc5000-relay /path/to/sc5000-deemix-relay.service
# Re-running is safe; state in /var/lib/sc5000-relay (config.json) is kept.
set -eu
BIN=${1:?binary}; UNIT=${2:?unit file}

install -m 0755 "$BIN" /usr/local/bin/sc5000-relay
install -m 0644 "$UNIT" /etc/systemd/system/sc5000-deemix-relay.service
systemctl daemon-reload
systemctl enable sc5000-deemix-relay.service >/dev/null 2>&1
systemctl restart sc5000-deemix-relay.service
for i in 1 2 3 4 5 6 7 8 9 10; do
  curl -fsS http://127.0.0.1:6595/healthz && exit 0
  sleep 1
done
journalctl -u sc5000-deemix-relay -n 30 --no-pager; exit 1
