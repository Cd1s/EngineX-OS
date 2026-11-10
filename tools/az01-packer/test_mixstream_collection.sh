#!/bin/bash
# Batam only, after mixstream-sandbox.sh: undo ONLY the collection branches.
# The live RTTI gate must reject NhSweepFxCollection despite unlocked flags.
set -euo pipefail
REPO=$(cd "$(dirname "$0")/../.." && pwd)
cd "$REPO"
mkdir -p out/issue166
python3 - <<'PY'
from pathlib import Path
p=Path('tools/ui-mod/engine_ui_hook.c')
s=p.read_text()
for site in ('d1aaf4', 'd27e14'):
    old='{0x'+site+'u, "\\xbb\\x03\\x00\\x0a", "\\xbb\\x03\\x00\\xea", 4, EDIT_STEMS}'
    new=old.replace('"\\xbb\\x03\\x00\\xea"', '"\\xbb\\x03\\x00\\x0a"')
    assert old in s
    s=s.replace(old,new)
Path('out/issue166/stock-sweep.c').write_text(s)
PY
arm-linux-gnueabihf-gcc -O2 -shared -fPIC -U_FORTIFY_SOURCE -D_FORTIFY_SOURCE=0 -ffunction-sections -fdata-sections -Wl,--gc-sections -Wl,-z,max-page-size=4096 -Itools/ui-mod -o out/issue166/stock-sweep.so out/issue166/stock-sweep.c -ldl -lpthread
W=${1:?existing Mixstream sandbox directory required}
if X166_HOOK="$PWD/out/issue166/stock-sweep.so" unshare -m -n -p -f --kill-child bash tools/az01-packer/mixstream-sandbox.sh "$W" --inner NH08 > out/issue166/negative.log 2>&1; then
    echo 'ERROR: stock collection incorrectly passed'; exit 1
fi
# The negative control keeps all Config flags unlocked, but undoes only the
# collection branch. The independent live RTTI check must detect native NH.
grep -Eq 'SweepFxCollection=0\|NhSweepFxCollection=([2-9]|[1-9][0-9]+)\|expected=full' "$W/NH08-collection.log"
cp "$W/NH08-collection.log" out/issue166/negative-collection.log
cp "$W/NH08-engine.log" out/issue166/negative-engine.log
cat out/issue166/negative-collection.log
echo 'X166 NEGATIVE|stock branch rejected despite unlocked Config|PASS|'
