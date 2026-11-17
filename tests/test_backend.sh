#!/usr/bin/env bash
# Deployment tests run with private mounts; no host service is changed.
set -euo pipefail
if [[ ${1:-} != --private ]]; then
  exec unshare -m -n --propagation private bash "$0" --private
fi
repo=$(cd "$(dirname "$0")/.." && pwd)
ip link set lo up
mount -t tmpfs tmpfs /data
cp -a /etc/alternatives /data/alternatives
for directory in /etc /var/lib /usr/local/bin /opt; do
  mount -t tmpfs tmpfs "$directory"
done
mv /data/alternatives /etc/alternatives
mkdir -p /etc/systemd/system /data/bin
export PATH="/data/bin:/usr/bin:/bin"
cat > /data/bin/systemctl <<'STUB'
#!/bin/sh
printf '%s\n' "$*" >> /data/service-calls
STUB
cat > /data/bin/curl <<'STUB'
#!/bin/sh
printf '{"ok":true}\n'
STUB
chmod +x /data/bin/*
/usr/bin/python3 - <<'HEALTH' &
from http.server import BaseHTTPRequestHandler,HTTPServer
class Health(BaseHTTPRequestHandler):
 def do_GET(self):
  self.send_response(200); self.end_headers(); self.wfile.write(b'{"ok":true}')
 def log_message(self,*args): pass
HTTPServer(('127.0.0.1',6596),Health).serve_forever()
HEALTH
health_pid=$!
trap 'kill "$health_pid" 2>/dev/null || :' EXIT
printf '#!/bin/sh\nexit 0\n' > /data/relay
chmod +x /data/relay
. "$repo/rootfs-overlay/usr/libexec/enginex-migration"
. "$repo/deploy/relay-host/migrate.sh"

sh "$repo/deploy/relay-host/install.sh" /data/relay
grep -q -- '-public auto' /etc/systemd/system/enginex-relay.service
for name in enginex-relay.service $relay_previous_units; do
  rm "/etc/systemd/system/$name"
done

mkdir -p "$legacy_relay_state"
printf '{"passwordHash":"keep","cookieSecret":"keep"}\n' > "$legacy_relay_state/config.json"
printf 'saved sessions\n' > "$legacy_relay_state/sessions.json"
cat > "/etc/systemd/system/$legacy_relay_unit" <<UNIT
[Service]
ExecStart=/usr/local/bin/$legacy_unit-relay -public https://keep.example -state $legacy_relay_state
StateDirectory=$legacy_unit-relay
Environment=KEEP_UNIT_VALUE=yes
UNIT
mkdir -p "/etc/systemd/system/$legacy_relay_unit.d"
printf '[Service]\nEnvironment=KEEP_CUSTOM=yes\n' > "/etc/systemd/system/$legacy_relay_unit.d/override.conf"
sh "$repo/deploy/relay-host/install.sh" /data/relay
test -f /etc/systemd/system/enginex-relay.service
grep -q -- '-public https://keep.example' /etc/systemd/system/enginex-relay.service
grep -q 'StateDirectory=enginex-relay' /etc/systemd/system/enginex-relay.service
grep -q '"passwordHash":"keep"' /var/lib/enginex-relay/config.json
grep -q 'saved sessions' "$legacy_relay_state/sessions.json"
test -L "$legacy_relay_state"
test -L "/usr/local/bin/$legacy_unit-relay"
test -L "/etc/systemd/system/$legacy_relay_unit"
grep -q KEEP_CUSTOM /etc/systemd/system/enginex-relay.service.d/override.conf
grep -q "disable --now $legacy_relay_unit" /data/service-calls
sha256sum /var/lib/enginex-relay/config.json /etc/systemd/system/enginex-relay.service > /data/after-migration
sh "$repo/deploy/relay-host/install.sh" /data/relay
sha256sum -c /data/after-migration

# DynamicUser's original state is often a symlink into /var/lib/private.
rm /var/lib/enginex-relay/config.json /var/lib/enginex-relay/sessions.json "$legacy_relay_state"
rmdir /var/lib/enginex-relay
mkdir -p "/var/lib/private/$legacy_unit-relay"
printf 'private state\n' > "/var/lib/private/$legacy_unit-relay/config.json"
ln -s "private/$legacy_unit-relay" "$legacy_relay_state"
sh "$repo/deploy/relay-host/install.sh" /data/relay
grep -q 'private state' /var/lib/enginex-relay/config.json
rm "$legacy_relay_state"
mkdir "$legacy_relay_state"
printf 'other state\n' > "$legacy_relay_state/config.json"
if sh "$repo/deploy/relay-host/install.sh" /data/relay > /data/conflict 2>&1; then
  echo 'Conflicting state was overwritten' >&2; exit 1
fi
grep -q 'private state' /var/lib/enginex-relay/config.json
grep -q 'other state' "$legacy_relay_state/config.json"
rm "$legacy_relay_state/config.json"
rmdir "$legacy_relay_state"
ln -s enginex-relay "$legacy_relay_state"

# A beginner's fresh install and rerun must use the current binary/flags and
# actually start Stems, while retaining its token and the relay public URL.
cat > /data/bin/apt-get <<'STUB'
#!/bin/sh
exit 0
STUB
cat > /data/bin/go <<'STUB'
#!/bin/sh
if [ "$1" = version ]; then echo 'go version go1.27.1 linux/amd64'; exit; fi
while [ "$#" -gt 0 ]; do
  if [ "$1" = -o ]; then cp /data/relay "$2"; chmod +x "$2"; exit; fi
  shift
done
STUB
cat > /data/bin/systemd-analyze <<'STUB'
#!/bin/sh
exit 0
STUB
chmod +x /data/bin/*
mkdir -p /etc/"$legacy_unit-stems" /opt/"$legacy_unit-stems"
printf 'existing-renderer-token\n' > /etc/"$legacy_unit-stems/token"
printf '[Service]\n' > "/etc/systemd/system/$legacy_stemsd.service"
bash "$repo/deploy/one-click-backend.sh" --relay-bin /data/relay > /data/summary
grep -q 'enginex-relay' /data/summary
grep -q 'enable --now enginex-stemsd.service' /data/service-calls
test -f /etc/enginex-stems/token
grep -q existing-renderer-token /etc/enginex-stems/token
sha256sum /etc/enginex-stems/token > /data/token-before
mkdir -p /opt/enginex-amd /opt/wrapper-lite
touch /opt/enginex-amd/enginex-amd.py /opt/enginex-amd/enginex-am-login.py
cp /data/relay /opt/wrapper-lite/amdl
cp /data/relay /opt/wrapper-lite/wrapper-lite-rootless
bash "$repo/deploy/one-click-backend.sh" --relay-bin /data/relay > /data/summary
sha256sum -c /data/token-before
test "$(stat -c %a /etc/enginex-amd.env)" = 600
grep -q 'EnvironmentFile=/etc/enginex-amd.env' /etc/systemd/system/enginex-relay.service.d/60-applemusic.conf
grep -q 'enable --now enginex-am-login.service enginex-amd.service' /data/service-calls
grep -q -- '-public https://keep.example' /etc/systemd/system/enginex-relay.service
if grep -q -- '-quick-tunnel' /etc/systemd/system/enginex-relay.service || grep -q 'Token:' /data/summary; then
  echo 'Unsupported relay flag or leaked helper token in summary' >&2; exit 1
fi
bash "$repo/deploy/one-click-backend.sh" --relay-bin /data/relay --public https://new.example > /data/summary
grep -q -- '-public https://new.example' /etc/systemd/system/enginex-relay.service
grep -q 'KEEP_UNIT_VALUE=yes' /etc/systemd/system/enginex-relay.service
systemctl_fake=/data/bin/systemctl
printf '#!/bin/sh\nexit 1\n' > "$systemctl_fake"
if bash "$repo/deploy/one-click-backend.sh" --relay-bin /data/relay > /data/failed-summary 2>&1; then
  echo 'A failed service was reported as a successful deployment' >&2; exit 1
fi
if grep -q 'backend is running' /data/failed-summary; then exit 1; fi
printf 'Backend migration and idempotent one-click setup: PASS\n'
