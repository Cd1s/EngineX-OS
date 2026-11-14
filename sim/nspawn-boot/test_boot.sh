#!/usr/bin/env bash
# One real-image smoke test; execute only on the ARM build host.
set -euo pipefail
[ $# = 1 ] || { echo "usage: $0 <JP07-Update.img>" >&2; exit 2; }
[[ $1 = *-JP07-Update.img ]] || { echo 'JP07 fixture required' >&2; exit 2; }
log=$(mktemp /var/tmp/nspawn-boot-test.XXXXXX)
trap 'rm -f "$log"' EXIT
before=$(sha256sum "$1")
bash "$(dirname "$0")/boot.sh" "$1" | tee "$log"
grep -qx 'PASS nspawn-boot JP07' "$log"
[ "$(sha256sum "$1")" = "$before" ]
echo 'PASS self-test: JP07 boots and release image is unchanged'
