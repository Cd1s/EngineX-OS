#!/usr/bin/env bash
# Deployment tests run with private mounts; no host service is changed.
set -euo pipefail
if [[ ${1:-} != --private ]]; then
  ENGINEX_OUTER_MNTNS=$(readlink /proc/self/ns/mnt) exec unshare -m -n --propagation private bash "$0" --private
fi
# --private is the re-exec above. Passed by hand it skips the unshare, and the mounts below would cover
# /etc, /var/lib and /opt of the host itself: refuse unless the mount namespace really changed.
if [[ -z ${ENGINEX_OUTER_MNTNS:-} || $(readlink /proc/self/ns/mnt) == "$ENGINEX_OUTER_MNTNS" ]]; then
  echo "test_backend.sh: --private is this script's own re-exec flag; run the script without arguments" >&2
  exit 2
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
if [ "$1" = show ]; then printf '%032d\n' 1; fi
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
  from pathlib import Path
  backend = b'htdemucs' if Path('/etc/systemd/system/enginex-stemsd.service.d/50-demucs.conf').exists() else b'hpss'
  self.send_response(200); self.end_headers(); self.wfile.write(b'{"ok":true,"backend":"'+backend+b'"}')
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
reject_stems() {
  if bash "$repo/deploy/one-click-backend.sh" --stems "$@" > /data/invalid 2>&1; then
    echo "Accepted invalid --stems arguments: $*" >&2; exit 1
  fi
  grep -q 'expected hpss, demucs or auto' /data/invalid
}
reject_stems wrong
reject_stems
reject_stems --public
bash "$repo/deploy/one-click-backend.sh" --help > /data/help
grep -Fq -- '--stems hpss|demucs|auto' /data/help
mkdir -p /etc/"$legacy_unit-stems" /opt/"$legacy_unit-stems"
printf 'existing-renderer-token\n' > /etc/"$legacy_unit-stems/token"
printf '[Service]\n' > "/etc/systemd/system/$legacy_stemsd.service"
# A baseline deploy/ copy can be supplied when remote-build omits Git history.
if [[ -n ${BASELINE_BACKEND:-} ]]; then
  bash "$BASELINE_BACKEND/deploy/one-click-backend.sh" --relay-bin /data/relay > /data/baseline-summary
  mkdir /data/baseline
  cp -a /etc /opt /usr/local/bin /var/lib /data/baseline
fi
bash "$repo/deploy/one-click-backend.sh" --relay-bin /data/relay > /data/summary
grep -q 'enginex-relay' /data/summary
grep -q 'enable --now enginex-stemsd.service' /data/service-calls
test -f /etc/enginex-stems/token
grep -q existing-renderer-token /etc/enginex-stems/token
sha256sum /etc/enginex-stems/token > /data/token-before
dropin=/etc/systemd/system/enginex-stemsd.service.d/50-demucs.conf
test ! -e "$dropin"
test ! -e /opt/enginex-stems/venv
grep -qx 'Stems backend: hpss' /data/summary

if [[ -n ${BASELINE_BACKEND:-} ]]; then
  for path in /etc /opt /usr/local/bin /var/lib; do
    diff --no-dereference -r "/data/baseline/${path##*/}" "$path"
  done
  echo 'Default deployment files match baseline byte for byte'
fi

cat > /data/bin/python3 <<'STUB'
#!/bin/sh
set -eu
[ "$1 $2" = '-m venv' ] || exec /usr/bin/python3 "$@"
mkdir -p "$3/bin"
cat > "$3/bin/python3" <<'PYTHON'
#!/bin/sh
[ "$1 $2" = '-m pip' ] || exit 2
shift 2
exec pip "$@"
PYTHON
chmod +x "$3/bin/python3"
STUB
cat > /data/bin/pip <<'STUB'
#!/bin/sh
printf '%s\n' "$*" >> /data/pip-calls
case "$*" in
  *' torch torchaudio') [ ! -f /data/reject-torch ] || exit 1 ;;
esac
case "$*" in
  *whl/cu*) [ ! -f /data/reject-cuda ] || exit 1 ;;
esac
case "$*" in
  *whl/cu126*) [ ! -f /data/reject-cu126 ] ;;
  'install demucs') [ ! -f /data/reject-demucs ] ;;
esac
STUB
cat > /data/bin/nvidia-smi <<'STUB'
#!/bin/sh
[ -s /data/compute-cap ] || exit 1
case "$1" in
  -L) echo 'GPU 0: NVIDIA test device' ;;
  --query-gpu=compute_cap) cat /data/compute-cap ;;
  *) exit 2 ;;
esac
STUB
cat > /data/bin/journalctl <<'STUB'
#!/bin/sh
if grep -q '^cu' /opt/enginex-stems/venv/.torch-index; then
  echo 'Stems CUDA: ok'
else
  echo 'Stems CUDA: PyTorch is a CPU-only build'
fi
STUB
chmod +x /data/bin/python3 /data/bin/pip /data/bin/nvidia-smi /data/bin/journalctl

one_click_stems() {
  bash "$repo/deploy/one-click-backend.sh" --relay-bin /data/relay --stems "$1" > /data/summary
}
printf 'MemTotal: 8388607 kB\n' > /data/meminfo
mount --bind /data/meminfo /proc/meminfo
one_click_stems auto
test ! -e "$dropin"
test ! -e /data/pip-calls
printf 'MemTotal: 8388608 kB\n' > /data/meminfo
one_click_stems auto
grep -Fq 'whl/cpu torch torchaudio' /data/pip-calls
grep -qx 'install demucs' /data/pip-calls
grep -q -- '-python /opt/enginex-stems/venv/bin/python3 -backend demucs$' "$dropin"
grep -qx 'Environment=TORCH_HOME=/var/lib/enginex-stems/cache' "$dropin"
cpu_threads=$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 1)
case "$cpu_threads" in ''|*[!0-9]*) cpu_threads=1 ;; esac
[ "$cpu_threads" -lt 1 ] && cpu_threads=1
[ "$cpu_threads" -gt 8 ] && cpu_threads=8
grep -qx "Environment=STEMS_TORCH_THREADS=$cpu_threads" "$dropin"
grep -q 'get_model("htdemucs")' "$dropin"
if grep -Eq '^(PrivateDevices|DeviceAllow|SupplementaryGroups)=' "$dropin"; then
  echo 'CPU drop-in changed GPU sandbox settings' >&2; exit 1
fi
grep -qx 'Stems backend: htdemucs' /data/summary
grep -qx 'Stems CUDA: PyTorch is a CPU-only build' /data/summary
cp "$dropin" /data/cpu-dropin
one_click_stems demucs
cmp "$dropin" /data/cpu-dropin
grep -qx 'install --index-url https://download.pytorch.org/whl/cpu torch torchaudio' /data/pip-calls

printf '12.0\n' > /data/compute-cap
printf 'MemTotal: 8388607 kB\n' > /data/meminfo
one_click_stems auto
grep -Fq 'whl/cu128 torch torchaudio' /data/pip-calls
grep -qx 'PrivateDevices=no' "$dropin"
grep -Fxq 'DeviceAllow=char-nvidia* rw' "$dropin"
grep -qx 'SupplementaryGroups=video render' "$dropin"
grep -qx 'Stems CUDA: ok' /data/summary
/usr/bin/systemd-analyze --recursive-errors=no verify /etc/systemd/system/enginex-stemsd.service
printf '8.6\n' > /data/compute-cap
touch /data/reject-cu126
one_click_stems demucs
grep -Fq 'whl/cu126 torch torchaudio' /data/pip-calls
grep -Fq 'whl/cu124 torch torchaudio' /data/pip-calls
grep -qx cu124 /opt/enginex-stems/venv/.torch-index

# With CUDA wheels unavailable, retry CPU before giving up on Demucs.
touch /data/reject-cuda
: > /data/pip-calls
one_click_stems auto
grep -Fq 'whl/cu124 torch torchaudio' /data/pip-calls
grep -Fq 'whl/cpu torch torchaudio' /data/pip-calls
grep -qx cpu /opt/enginex-stems/venv/.torch-index
cmp "$dropin" /data/cpu-dropin
grep -qx 'Stems backend: htdemucs' /data/summary

# Both optional dependency failures fall back in auto and remove a stale drop-in.
touch /data/reject-torch
one_click_stems auto 2> /data/stems-diagnostics
grep -Fxq 'Could not install PyTorch wheels for Stems; falling back to HPSS' /data/stems-diagnostics
test ! -e "$dropin"
grep -qx 'Stems backend: hpss' /data/summary
rm /data/reject-torch
one_click_stems auto
touch /data/reject-demucs
one_click_stems auto 2> /data/stems-diagnostics
grep -Fxq 'Could not install Demucs for Stems; falling back to HPSS' /data/stems-diagnostics
test ! -e "$dropin"
grep -qx 'Stems backend: hpss' /data/summary
if one_click_stems demucs 2> /data/stems-diagnostics; then
  echo 'Explicit Demucs install reported success after pip failed' >&2; exit 1
fi
grep -Fxq 'Could not install Demucs for Stems' /data/stems-diagnostics
rm /data/reject-demucs /data/reject-cuda
one_click_stems demucs

# Switch back without removing operator files or the renderer token.
printf '[Service]\nNice=11\n' > "${dropin%/*}/90-local.conf"
one_click_stems hpss
test ! -e "$dropin"
test -f "${dropin%/*}/90-local.conf"
test -x /opt/enginex-stems/venv/bin/python3
sha256sum -c /data/token-before
grep -qx 'Stems backend: hpss' /data/summary

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
