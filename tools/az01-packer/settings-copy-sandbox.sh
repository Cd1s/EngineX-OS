#!/bin/bash
# Native profile experiment in the nspawn full-boot sandbox. ARM build host only.
set -euo pipefail
REPO=$(cd "$(dirname "$0")/../.." && pwd)
export NSPAWN_SETTINGS_COPY=1
bash "$REPO/sim/nspawn-boot/boot.sh" "${1:?release image required}" --keep
