#!/bin/sh
# Install or upgrade the renderer, preserving its existing token.
set -eu
staging=${1:?path to staged release}
HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
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
systemd-analyze verify /etc/systemd/system/enginex-stemsd.service
systemctl daemon-reload
systemctl enable --now enginex-stemsd.service
systemctl restart enginex-stemsd.service
systemctl is-enabled enginex-stemsd.service
systemctl is-active enginex-stemsd.service
/usr/bin/python3 - <<'PY'
from pathlib import Path
import urllib.request
p=Path('/etc/enginex-stems/token')
r=urllib.request.Request('http://127.0.0.1:6596/healthz',headers={'Authorization':'Bearer '+p.read_text().strip()})
import time
for attempt in range(20):
 try:
  print(urllib.request.urlopen(r,timeout=10).read().decode()); break
 except OSError:
  if attempt == 19: raise
  time.sleep(0.1)
PY
