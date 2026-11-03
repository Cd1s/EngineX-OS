#!/bin/sh
# Run only after the pre-change backup ID has been recorded.
set -eu
: "${SC5000_BACKUP_ID:?provide verified pre-change backup id}"
staging=${1:?path to staged release}
test -f "$staging/sc5000-stemsd"
test -f "$staging/worker.py"
test -f "$staging/sc5000-stemsd.service"
/usr/bin/python3 -c 'import numpy, scipy; assert numpy.__version__; assert scipy.__version__'
# Fresh install only. Updates require backing up the existing release and unit.
test ! -e /opt/sc5000-stems
test ! -e /etc/sc5000-stems
test ! -e /etc/systemd/system/sc5000-stemsd.service
install -d -m 755 /opt/sc5000-stems
install -m 755 "$staging/sc5000-stemsd" /opt/sc5000-stems/sc5000-stemsd
install -m 644 "$staging/worker.py" /opt/sc5000-stems/worker.py
install -d -m 700 /etc/sc5000-stems
/usr/bin/python3 - <<'PY'
import os,secrets
fd=os.open('/etc/sc5000-stems/token',os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
with os.fdopen(fd,'w') as f:f.write(secrets.token_hex(32)+'\n')
PY
install -m 644 "$staging/sc5000-stemsd.service" /etc/systemd/system/sc5000-stemsd.service
systemd-analyze verify /etc/systemd/system/sc5000-stemsd.service
systemctl daemon-reload
systemctl enable --now sc5000-stemsd.service
systemctl is-enabled sc5000-stemsd.service
systemctl is-active sc5000-stemsd.service
/usr/bin/python3 - <<'PY'
from pathlib import Path
import urllib.request
p=Path('/etc/sc5000-stems/token')
r=urllib.request.Request('http://127.0.0.1:6596/healthz',headers={'Authorization':'Bearer '+p.read_text().strip()})
import time
for attempt in range(20):
 try:
  print(urllib.request.urlopen(r,timeout=10).read().decode()); break
 except OSError:
  if attempt == 19: raise
  time.sleep(0.1)
p=Path('/root/automation_list.md')
entry='\n\n## SC5000 Stems remote renderer\n\n- Unit: `sc5000-stemsd.service` (enabled), listener `127.0.0.1:6596` (public entry: sc5000-relay `/v1/engine`).\n- Code: `/opt/sc5000-stems/`; credential: `/etc/sc5000-stems/token` (0600, systemd LoadCredential).\n- Spectral HPSS approximation; one render session; no original audio persistence. Engine performs encrypted AAC/MP4 writing.\n- Stop: `systemctl disable --now sc5000-stemsd.service`.\n'
if p.exists() and '## SC5000 Stems remote renderer' not in p.read_text():
 with p.open('a') as f:f.write(entry)
PY
