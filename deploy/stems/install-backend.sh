#!/bin/sh
# Run only after the pre-change backup ID has been recorded.
set -eu
: "${ENGINEX_BACKUP_ID:?provide verified pre-change backup id}"
staging=${1:?path to staged release}
test -f "$staging/enginex-stemsd"
test -f "$staging/worker.py"
test -f "$staging/enginex-stemsd.service"
/usr/bin/python3 -c 'import numpy, scipy; assert numpy.__version__; assert scipy.__version__'
# Fresh install only. Updates require backing up the existing release and unit.
test ! -e /opt/enginex-stems
test ! -e /etc/enginex-stems
test ! -e /etc/systemd/system/enginex-stemsd.service
install -d -m 755 /opt/enginex-stems
install -m 755 "$staging/enginex-stemsd" /opt/enginex-stems/enginex-stemsd
install -m 644 "$staging/worker.py" /opt/enginex-stems/worker.py
install -d -m 700 /etc/enginex-stems
/usr/bin/python3 - <<'PY'
import os,secrets
fd=os.open('/etc/enginex-stems/token',os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
with os.fdopen(fd,'w') as f:f.write(secrets.token_hex(32)+'\n')
PY
install -m 644 "$staging/enginex-stemsd.service" /etc/systemd/system/enginex-stemsd.service
systemd-analyze verify /etc/systemd/system/enginex-stemsd.service
systemctl daemon-reload
systemctl enable --now enginex-stemsd.service
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
p=Path('/root/automation_list.md')
entry='\n\n## EngineX Stems remote renderer\n\n- Unit: `enginex-stemsd.service` (enabled), listener `127.0.0.1:6596` (public entry: enginex-relay `/v1/engine`).\n- Code: `/opt/enginex-stems/`; credential: `/etc/enginex-stems/token` (0600, systemd LoadCredential).\n- Spectral HPSS approximation; one render session; no original audio persistence. Engine performs encrypted AAC/MP4 writing.\n- Stop: `systemctl disable --now enginex-stemsd.service`.\n'
if p.exists() and '## EngineX Stems remote renderer' not in p.read_text():
 with p.open('a') as f:f.write(entry)
PY
