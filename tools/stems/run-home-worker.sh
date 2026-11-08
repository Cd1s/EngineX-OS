#!/bin/sh
# Turn this computer (macOS or Linux; Windows: WSL with mirrored networking)
# into the Stems renderer for EngineX decks on the same LAN. Decks find it by
# themselves (UDP 17891) and render on the deck again once it is switched off.
#
#   tools/stems/run-home-worker.sh            # auto: htdemucs if installed, else hpss
#   tools/stems/run-home-worker.sh demucs     # install demucs (torch) and use it
#   tools/stems/run-home-worker.sh hpss       # fast spectral split, numpy + scipy only
#
# Needs python3 and the enginex-stemsd binary (in PATH, next to this script, or
# SC5000_STEMSD=/path). Python packages go into ~/.enginex-stems/venv.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
backend=${1:-${ENGINEX_STEMS_BACKEND:-auto}}
venv=${ENGINEX_STEMS_HOME:-$HOME/.enginex-stems}/venv
bin=${SC5000_STEMSD:-$(command -v enginex-stemsd || command -v sc5000-stemsd || echo "$here/enginex-stemsd")}
if [ ! -x "$bin" ]; then
  echo "enginex-stemsd not found: put the release binary next to this script or build it with 'go build ./cmd/enginex-stemsd'" >&2
  exit 1
fi
[ -x "$venv/bin/python3" ] || python3 -m venv "$venv"
py=$venv/bin/python3
"$py" -c 'import numpy, scipy' 2>/dev/null || "$py" -m pip install -q numpy scipy
case $backend in
  demucs|htdemucs) "$py" -c 'import demucs' 2>/dev/null || "$py" -m pip install -q demucs ;;
esac
if [ "$("$py" "$here/worker.py" --backend "$backend" --print-backend)" = htdemucs ]; then
  # Fetch the model weights now, not during a deck's first session.
  "$py" -c 'from demucs.pretrained import get_model; get_model("htdemucs")'
fi
exec "$bin" -listen 0.0.0.0:17890 -token-file "" -python "$py" -worker "$here/worker.py" -backend "$backend" -sessions 2
