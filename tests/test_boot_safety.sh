#!/bin/bash
# Boot state machine with fake Engine/systemd; no host service or media changes.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
GUARD=${BOOT_GUARD_TEST_TARGET:-$ROOT/rootfs-overlay/etc/enginex/engine-ui-guard}
W=$(mktemp -d "$ROOT/tests/.tmp.boot-safety.XXXXXX")
pids=()
cleanup() { for p in "${pids[@]}"; do kill "$p" 2>/dev/null || :; done; wait 2>/dev/null || :; rm -rf "$W"; }
trap cleanup EXIT
mkdir -p "$W/bin"
export BOOT_TEST_LOG=$W/systemctl.log SC5000_ENGINE_GUARDIAN=$W/bin/guardian
export SC5000_PERF_TUNE=/bin/true
export SC5000_BOOT_TIMEOUT=2 SC5000_BOOT_POLL_INTERVAL=.1 SC5000_UI_HOOK=/nonexistent/test-only-hook.so
cat > "$W/bin/systemctl" <<'SH'
#!/bin/sh
echo "$*" >> "$BOOT_TEST_LOG"
[ "$1" != is-system-running ] || echo running
SH
cat > "$W/bin/guardian" <<'SH'
#!/bin/sh
exit 0
SH
cat > "$W/bin/engine" <<'SH'
#!/bin/sh
printf '%s\n' "${LD_PRELOAD-unset}" > "$SC5000_UI_DIR/observed-preload"
case ${BOOT_TEST_MODE:-hang} in
    fail) exit 17 ;;
    mux) exit 7 ;;
    intentional) echo UpdateFromUrl > "$SC5000_QUIT_REASON_FILE"; exit 143 ;;
    stock) exit 0 ;;
    ready) cp "$SC5000_UI_DIR/boot-token" "$SC5000_UI_DIR/boot-ready" ;;
esac
sleep 30 &
child=$!
trap 'kill "$child" 2>/dev/null; exit 0' TERM INT HUP
wait "$child"
SH
chmod +x "$W/bin/"*
cat > "$W/bin/forbidden" <<'SH'
#!/bin/sh
printf '%s\n' "$0 $*" >> "$BOOT_TEST_FORBIDDEN_LOG"
exit 99
SH
chmod +x "$W/bin/forbidden"
for tool in pkill killall reboot az01-update mount umount; do ln -s forbidden "$W/bin/$tool"; done
export BOOT_TEST_FORBIDDEN_LOG=$W/forbidden.log
export PATH=$W/bin:$PATH
fail() { echo "FAIL $*" >&2; exit 1; }
has_file() { for _ in {1..70}; do [ ! -e "$1" ] || return 0; sleep .1; done; fail "missing $1"; }
has_text() { for _ in {1..70}; do grep -q -- "$1" "$2" 2>/dev/null && return 0; sleep .1; done; fail "missing $1 in $2"; }
fixture() {
    export SC5000_UI_DIR=$W/$1 SC5000_ENGINE_DATA_DIR=$W/$1-data SC5000_STREAMING_ACCOUNTS=$W/$1-accounts.json
    export SC5000_QUIT_REASON_FILE=$W/$1-reason
    export SC5000_PRIMARY_UI_DIR=$SC5000_UI_DIR SC5000_LEGACY_UI_DIR=$W/$1-legacy
    export SC5000_ASSIGNMENT_DIR=$W/$1-assignments SC5000_ASSIGNMENT_PATCHED=$W/$1-patched
    export SC5000_STATE_DIR=$W/$1-state SC5000_PLANCK_STATE=$W/$1-planck.json
    mkdir -p "$SC5000_UI_DIR"
    : > "$BOOT_TEST_LOG"
}
launch() { sh "$GUARD" launch "$W/bin/engine" > "$W/engine.log" 2>&1 & pids+=("$!"); }
watch() { sh "$GUARD" watch > "$W/watch.log" 2>&1 & pids+=("$!"); }

fixture hung
sh "$GUARD" pre
launch; has_file "$SC5000_UI_DIR/boot-token"; watch
has_file "$SC5000_UI_DIR/safe-mode"
grep -q '^ui-ready-timeout$' "$SC5000_UI_DIR/boot-stage" || fail 'hang stage missing'
has_text 'restart --no-block engine.service' "$BOOT_TEST_LOG"
BOOT_TEST_MODE=stock sh "$GUARD" launch "$W/bin/engine"
[ "$(cat "$SC5000_UI_DIR/observed-preload")" = unset ] || fail 'fallback inherited LD_PRELOAD'
[ -e "$SC5000_UI_DIR/mods-quarantined" ] || fail 'mods not quarantined'
SERVICE_RESULT=success sh "$GUARD" stop; sh "$GUARD" pre; sh "$GUARD" post
[ -e "$SC5000_UI_DIR/safe-mode" ] || fail 'clean stop/pre/post cleared persistent fallback'
echo 'PASS hang timeout, stock environment, quarantine and fallback persistence'

fixture healthy
sh "$GUARD" pre
BOOT_TEST_MODE=ready launch
has_file "$SC5000_UI_DIR/boot-ready"; watch
sleep 3
[ ! -e "$SC5000_UI_DIR/safe-mode" ] || fail 'normal UI-ready boot fell back'
echo 'PASS normal boot never falls back'

fixture progress
sh "$GUARD" pre; launch
has_file "$SC5000_UI_DIR/boot-token"; watch
for step in 1 2 3 4; do
    sh "$GUARD" progress media-heal "$step"
    sleep 1
    [ ! -e "$SC5000_UI_DIR/safe-mode" ] || fail 'genuine completed progress triggered fallback'
done
has_file "$SC5000_UI_DIR/safe-mode"
echo 'PASS completed work extends deadline; stalled progress times out'

fixture crashes
sh "$GUARD" pre
echo UpdateFromUrl > "$SC5000_QUIT_REASON_FILE"
for n in 1 2 3; do
    if BOOT_TEST_MODE=fail sh "$GUARD" launch "$W/bin/engine" > "$W/crash.log" 2>&1; then fail 'fake crash lost exit status'; fi
    [ ! -e "$SC5000_UI_DIR/safe-mode" ] || fail 'fallback before three failed attempts'
done
BOOT_TEST_MODE=stock sh "$GUARD" launch "$W/bin/engine"
[ -e "$SC5000_UI_DIR/safe-mode" ] || fail 'internal vendor retries escaped crash threshold'
[ "$(cat "$SC5000_UI_DIR/observed-preload")" = unset ] || fail 'crash fallback still preloaded hook'
echo 'PASS three crashes in five minutes force next attempt to stock'

fixture intentional
sh "$GUARD" pre
for mode in mux intentional; do
    if BOOT_TEST_MODE=$mode sh "$GUARD" launch "$W/bin/engine" > "$W/intentional.log" 2>&1; then fail 'intentional nonzero exit code was lost'; fi
    [ ! -e "$SC5000_UI_DIR/last-crash" ] && [ ! -e "$SC5000_UI_DIR/boot-token" ] || fail 'intentional handoff counted as crash or left watchdog armed'
    sh "$GUARD" pre
    [ "$(cat "$SC5000_UI_DIR/boot-count")" = 1 ] || fail 'intentional handoff raised boot count'
done
SC5000_BOOT_TIMEOUT=invalid SC5000_BOOT_CRASH_WINDOW=0 SC5000_BOOT_CRASH_LIMIT=999999999999999999999 BOOT_TEST_MODE=stock sh "$GUARD" launch "$W/bin/engine"
echo 'PASS intentional USB/update handoffs and invalid environment do not prevent boot'

fixture inherited
BOOT_TEST_MODE=stock sh "$GUARD" service "$W/bin/engine" > "$W/service.log" 2>&1
[ "$(cat "$SC5000_UI_DIR/observed-preload")" = "$SC5000_UI_HOOK" ] || fail 'normal vendor helpers lost the updater hook'
[ -s "$SC5000_UI_DIR/boot-token" ] || fail 'vendor launcher not protected before preload'
sh "$GUARD" fallback previous-fallback
BOOT_TEST_MODE=stock sh "$GUARD" service "$W/bin/engine"
[ "$(cat "$SC5000_UI_DIR/observed-preload")" = unset ] || fail 'stock vendor launcher inherited hook'
echo 'PASS vendor updater inheritance and hook-free fallback launcher'
export SC5000_DEBLOAT=$W/bin/forbidden-runtime
printf '#!/bin/sh\nexit 99\n' > "$SC5000_DEBLOAT"
chmod +x "$SC5000_DEBLOAT"
sh "$GUARD" runtime || fail 'fallback called EngineX runtime tuning'
rm "$SC5000_UI_DIR/safe-mode"
if sh "$GUARD" runtime; then fail 'normal start lost its runtime preparation'; fi
rm "$SC5000_DEBLOAT"
unset SC5000_DEBLOAT

fixture expired
echo 2 > "$SC5000_UI_DIR/boot-failures"
echo $(( $(date +%s) - 301 )) > "$SC5000_UI_DIR/boot-window"
touch "$SC5000_UI_DIR/last-crash"
sh "$GUARD" pre
[ ! -e "$SC5000_UI_DIR/safe-mode" ] && [ "$(cat "$SC5000_UI_DIR/boot-count")" = 2 ] || fail 'old crashes did not expire'
sh "$GUARD" post
token=$(cat "$SC5000_UI_DIR/boot-token")
if sh "$GUARD" ready stale-attempt; then fail 'stale ready acknowledgement accepted'; fi
[ ! -e "$SC5000_UI_DIR/boot-ready" ] || fail 'stale attempt became ready'
sh "$GUARD" ready "$token"
echo 'PASS crash-window expiry and stale readiness rejection'

fixture primary-alias
export SC5000_PRIMARY_UI_DIR=$W/primary-ui
mkdir -p "$SC5000_PRIMARY_UI_DIR/mods-overlay" "$SC5000_UI_DIR/mods-overlay"
sh "$GUARD" fallback previous-fallback
for ui in "$SC5000_PRIMARY_UI_DIR" "$SC5000_UI_DIR"; do
    [ -e "$ui/mods-quarantined" ] && [ ! -e "$ui/mods-overlay" ] || fail 'redirected compatibility UI was not quarantined'
done
echo 'PASS compatibility UI paths stay inside the test root'
if grep -Eq 'erase-data|FactoryReset|reboot' "$BOOT_TEST_LOG"; then fail 'destructive action'; fi
[ ! -e "$BOOT_TEST_FORBIDDEN_LOG" ] || fail 'test attempted host process or mount control'
