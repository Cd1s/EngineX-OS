#!/usr/bin/env bash
# tests/test_backend.sh refuses its own re-exec flag when it is passed by hand. Without the script's
# unshare its mounts would cover /etc, /var/lib and /opt of the host (a build host lost SSH that way on
# 2026-11-22). Both calls below are made inside a throwaway mount and network namespace, so a script
# without the check cannot reach the host either.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
log=$(mktemp)
trap 'rm -f "$log"' EXIT
result=$(unshare -m -n --propagation private bash -c '
  a=0; env -u ENGINEX_OUTER_MNTNS bash "$1/test_backend.sh" --private >"$2" 2>&1 || a=$?
  b=0; ENGINEX_OUTER_MNTNS=$(readlink /proc/self/ns/mnt) bash "$1/test_backend.sh" --private >>"$2" 2>&1 || b=$?
  echo "flag alone rc=$a, flag in the caller namespace rc=$b, tmpfs over /etc: $(findmnt -rn -t tmpfs --mountpoint /etc | wc -l)"' _ "$here" "$log")
want="flag alone rc=2, flag in the caller namespace rc=2, tmpfs over /etc: 0"
if [[ $result != "$want" ]]; then
  echo "FAIL: $result" >&2
  echo "want: $want" >&2
  tail -n 5 "$log" >&2
  exit 1
fi
grep -q 're-exec flag' "$log" || { echo "FAIL: the refusal does not say why" >&2; cat "$log" >&2; exit 1; }
printf 'test_backend.sh refuses its internal --private flag: PASS\n'
