#!/bin/sh
# Install or upgrade the renderer, preserving its existing token.
set -eu
staging=${1:?path to staged release}
mode=${2:-hpss}
case "$mode" in
 hpss|demucs|auto) ;;
 *) echo "Invalid Stems backend '$mode'; expected hpss, demucs or auto" >&2; exit 2 ;;
esac
HERE=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=deploy/relay-host/migrate.sh
. "$HERE/../relay-host/migrate.sh"
test -f "$staging/enginex-stemsd"
test -f "$staging/worker.py"
test -f "$staging/enginex-stemsd.service"
/usr/bin/python3 -c 'import numpy, scipy; assert numpy.__version__; assert scipy.__version__'
migrate_stems
install -d -m 755 /opt/enginex-stems
install -m 755 "$staging/enginex-stemsd" /opt/enginex-stems/enginex-stemsd
install -m 644 "$staging/worker.py" /opt/enginex-stems/worker.py
install -d -m 700 /etc/enginex-stems
/usr/bin/python3 - <<'PY'
import os,secrets
path='/etc/enginex-stems/token'
if not os.path.exists(path):
 fd=os.open(path,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
 with os.fdopen(fd,'w') as f:f.write(secrets.token_hex(32)+'\n')
PY
install -m 644 "$staging/enginex-stemsd.service" /etc/systemd/system/enginex-stemsd.service

# The base unit and system Python are sufficient for HPSS on small hosts.
dropin=/etc/systemd/system/enginex-stemsd.service.d/50-demucs.conf
venv=/opt/enginex-stems/venv
worker=/opt/enginex-stems/worker.py
gpu=no
if [ "$mode" != hpss ]; then
 if command -v nvidia-smi >/dev/null && devices=$(nvidia-smi -L 2>/dev/null) &&
    printf '%s\n' "$devices" | grep -q '^GPU [0-9]'; then
  gpu=yes
 fi
 if [ "$mode" = auto ]; then
  mode=hpss
  # MemTotal is in KiB; allow space for the OS and one Demucs session.
  if [ "$gpu" = yes ] || awk '/^MemTotal:/ {ok = ($2 >= 8*1024*1024)} END {exit !ok}' /proc/meminfo; then
   mode=demucs
  fi
 fi
fi
if [ "$mode" = demucs ]; then
 apt-get install -y -qq python3-venv
 [ -x "$venv/bin/python3" ] || python3 -m venv "$venv"
 py=$venv/bin/python3
 indexes=cpu
 if [ "$gpu" = yes ]; then
  caps=$(nvidia-smi --query-gpu=compute_cap --format=csv,noheader 2>/dev/null) || caps=
  indexes=$(/usr/bin/python3 "$worker" --torch-index "$caps")
 fi
 # Prefer an already installed compatible wheel set, including a cu124 fallback.
 installed=$(cat "$venv/.torch-index" 2>/dev/null || :)
 case " $indexes " in
  *" $installed "*) indexes="$installed $indexes" ;;
 esac
 for index in $indexes; do
  set --
  [ "$index" = "$installed" ] || set -- --force-reinstall
  if "$py" -m pip install "$@" --index-url "https://download.pytorch.org/whl/$index" torch torchaudio; then
   printf '%s\n' "$index" > "$venv/.torch-index"
   break
  fi
  index=
 done
 [ -n "${index:-}" ] || { echo 'Could not install PyTorch wheels for Stems' >&2; exit 1; }
 "$py" -m pip install demucs
 mkdir -p "$(dirname "$dropin")"
 {
  cat <<'UNIT'
[Service]
ExecStart=
ExecStart=/opt/enginex-stems/enginex-stemsd -listen 127.0.0.1:6596 -token-file %d/token -worker /opt/enginex-stems/worker.py -python /opt/enginex-stems/venv/bin/python3 -backend demucs
StateDirectory=enginex-stems
Environment=TORCH_HOME=/var/lib/enginex-stems/cache
ReadWritePaths=-/var/lib/enginex-stems/cache
ExecStartPre=/opt/enginex-stems/venv/bin/python3 -c 'from demucs.pretrained import get_model; get_model("htdemucs")'
ExecStartPre=-/bin/sh -c 'status=$$(/opt/enginex-stems/venv/bin/python3 /opt/enginex-stems/worker.py --cuda-check) || status="the CUDA check failed"; echo "Stems CUDA: $$status"'
TimeoutStartSec=5min
MemoryMax=8G
TasksMax=128
CPUQuota=800%
UNIT
  if [ "$gpu" = yes ]; then
   # DeviceAllow accepts globs in character device classes, not /dev paths.
   cat <<'UNIT'
PrivateDevices=no
DevicePolicy=closed
DeviceAllow=char-nvidia* rw
SupplementaryGroups=video render
UNIT
  fi
 } > "$staging/50-demucs.conf"
 install -m 644 "$staging/50-demucs.conf" "$dropin"
elif [ -e "$dropin" ] || [ -L "$dropin" ]; then
 rm "$dropin"
 rmdir "$(dirname "$dropin")" 2>/dev/null || :
fi

systemd-analyze verify /etc/systemd/system/enginex-stemsd.service
systemctl daemon-reload
systemctl enable --now enginex-stemsd.service
systemctl restart enginex-stemsd.service
systemctl is-enabled enginex-stemsd.service
systemctl is-active enginex-stemsd.service
/usr/bin/python3 - "$mode" <<'PY'
import json, sys
from pathlib import Path
import urllib.request
p=Path('/etc/enginex-stems/token')
r=urllib.request.Request('http://127.0.0.1:6596/healthz',headers={'Authorization':'Bearer '+p.read_text().strip()})
import time
for attempt in range(20):
 try:
  body=urllib.request.urlopen(r,timeout=10).read().decode(); break
 except OSError:
  if attempt == 19: raise
  time.sleep(0.1)
print(body)
backend=json.loads(body).get('backend', 'unknown')
if sys.argv[1] == 'demucs' and backend != 'htdemucs':
 raise SystemExit('Expected the htdemucs backend, but /healthz reported '+backend)
print('Stems backend: '+backend)
PY
if [ "$mode" = demucs ]; then
 invocation=$(systemctl show -p InvocationID --value enginex-stemsd.service)
 cuda=$(journalctl --no-pager -o cat "_SYSTEMD_INVOCATION_ID=$invocation" | awk '/^Stems CUDA: / {line=$0} END {print line}')
 echo "${cuda:-Stems CUDA: service did not report a result}"
else
 echo 'Stems CUDA: not used by HPSS'
fi
