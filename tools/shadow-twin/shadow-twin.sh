#!/bin/bash
# EngineX OS dual-deck SC5000 shadow-track sandbox entrypoint.
# Usage:
#   tools/shadow-twin/shadow-twin.sh <up|down|update|deploy|load|play|sync|nudge|view|shot|perf|ping|status|eval> [args...]
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
exec python3 -I "$HERE/twin.py" "$@"
