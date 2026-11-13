#!/bin/bash
# One end-to-end run of deck-push against tools/dev/fake-deck.sh, building every
# component from this working tree with the repo's build steps (build host, root):
#   bash tools/dev/test_deck_push_e2e.sh [evidence dir]
# lock -> push all (real UI packs, hook, edisksd hook, suite) -> smoke PASS ->
# rollback -> smoke PASS -> push hook -> reboot -> original back -> unlock.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
EVIDENCE=${1:-}
W=$(mktemp -d /var/tmp/deck-push-e2e.XXXXXX)
trap 'bash "$HERE/fake-deck.sh" down "$W/deck" >/dev/null 2>&1 || :; rm -rf "$W"' EXIT
export DECK_PUSH_CONFIG DECK_PUSH_OWNER=deck-push-e2e
DECK_PUSH_CONFIG=$(bash "$HERE/fake-deck.sh" up "$W/deck" | tail -1)
export PATH="$W/deck/test-bin:$PATH"
dp() { python3 "$HERE/deck-push" "$@"; }
n=0
step() { # <name> <expected rc> <command...>
    local name=$1 want=$2 rc=0
    shift 2
    "$@" > "$W/step.log" 2>&1 || rc=$?
    sed 's/^/    /' "$W/step.log"
    [ -z "$EVIDENCE" ] || { n=$((n + 1)); mkdir -p "$EVIDENCE"; cp "$W/step.log" "$EVIDENCE/$(printf %02d "$n")-$name.log"; }
    if [ "$rc" = "$want" ]; then echo "E2E PASS $name (rc $rc)"; else echo "E2E FAIL $name (rc $rc, want $want)"; exit 1; fi
}
orig=$(sha256sum < "$W/deck/persist/etc-sc5000/libengine-ui-hook.so" | cut -d' ' -f1)
step lock 0 dp lock fake --purpose "deck-push end-to-end run"
step push-all 0 dp fake all --no-sandbox "e2e: the fake deck has no twin sandbox"
grep -q '^RESULT PASS fake' "$W/step.log"
step status 0 dp status fake
step rollback 0 dp fake --rollback
[ "$(grep -c 'PASS .* back to its original' "$W/step.log")" = 5 ] || { echo "E2E FAIL rollback did not restore all 5 targets"; exit 1; }
step push-hook 0 dp fake hook
bash "$HERE/fake-deck.sh" reboot "$W/deck" > /dev/null
now=$(python3 - "$DECK_PUSH_CONFIG" <<'PY'
import configparser, shlex, subprocess, sys
c = configparser.ConfigParser(); c.read(sys.argv[1]); d = c["fake"]
print(subprocess.run(["ssh", *shlex.split(d["ssh_options"]), d["ssh"], "sha256sum < /etc/sc5000/libengine-ui-hook.so"],
                     capture_output=True, text=True, check=True).stdout.split()[0])
PY
)
if [ "$now" = "$orig" ]; then echo "E2E PASS reboot restored the original hook ($orig)"; else echo "E2E FAIL reboot left $now, want $orig"; exit 1; fi
step reacquire-after-reboot 0 dp lock fake --purpose "after sandbox reboot"
step smoke-after-reboot 0 dp smoke fake
step unlock 0 dp unlock fake
step backups 0 dp backups fake
u=$(cat "$W/deck/unshare.pid")
bash "$HERE/fake-deck.sh" down "$W/deck" > /dev/null
if [ -e "/proc/$u" ]; then echo "E2E FAIL fake deck still running after down"; exit 1; fi
echo "E2E PASS fake deck stopped"
echo "E2E PASS all"
