#!/usr/bin/env bash
# Regression for #311: intentional first-boot restarts and normal shutdowns
# must not turn the boot guard's evidence into a crash streak.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
ASSETS=${ISSUE311_RELEASE_ASSETS:?set ISSUE311_RELEASE_ASSETS to the v5.3.0.5-r1 asset directory}
MANIFEST=$ASSETS/SHA256SUMS.txt
GUARD=$ROOT/rootfs-overlay/etc/enginex/engine-ui-guard
MIGRATION=$ROOT/rootfs-overlay/usr/libexec/enginex-migration
WORK=$(mktemp -d /root/build/issue311-firstboot-run.XXXXXX)
trap 'find "$WORK" -depth -delete' EXIT

[ -r "$MANIFEST" ] && [ -x "$GUARD" ]

check_asset() {
    local name=$1
    (cd "$ASSETS" && awk -v want="$name" '$2 == want { print; found=1 } END { exit !found }' "$MANIFEST" | sha256sum -c)
}

check_gen2_image() {
    local image=$1
    tail -c +129 "$image" | tar -tzf - > "$WORK/$(basename "$image").list"
    grep -q '/bin/engine-ui-guard$' "$WORK/$(basename "$image").list"
}

check_gen1_image() {
    python3 "$ROOT/tools/az01-packer/az01_packer.py" info "$1" >/dev/null
}

cat > "$WORK/systemctl" <<'SH'
#!/bin/sh
printf 'systemctl %s\n' "$*" >> "$ISSUE311_JOURNAL"
case $1 in
    is-system-running) printf '%s\n' running ;;
esac
SH
chmod +x "$WORK/systemctl"
printf '#!/bin/sh\nexit 0\n' > "$WORK/true-helper"
chmod +x "$WORK/true-helper"

snapshot() {
    local label=$1 ui=$2 journal=$3
    {
        printf '[%s]\n' "$label"
        for name in boot-count boot-failures boot-window boot-token boot-ready last-crash running-sentinel safe-mode; do
            if [ -e "$ui/$name" ]; then
                printf '%s=%s\n' "$name" "$(tr '\n' ' ' < "$ui/$name")"
            else
                printf '%s=<absent>\n' "$name"
            fi
        done
    } >> "$journal"
}

run_profile() {
    local profile=$1 image=$2 gen2=$3
    local base=$WORK/$profile ui=$WORK/$profile-ui journal=$WORK/$profile-journal
    mkdir -p "$ui" "$base/engine" "$base/state" "$base/assignments" "$base/custom"
    : > "$journal"
    export ISSUE311_JOURNAL=$journal
    export PATH="$WORK:$PATH"
    export ENGINEX_MIGRATION=$MIGRATION
    export ENGINEX_UI_DIR=$ui ENGINEX_PRIMARY_UI_DIR=$ui ENGINEX_LEGACY_UI_DIR=$base/legacy
    export ENGINEX_ENGINE_DATA_DIR=$base/engine ENGINEX_GUARD_PRE_MARK=$base/pre-mark
    export ENGINEX_ENGINE_GUARDIAN=$WORK/true-helper ENGINEX_RB_BRIDGE=$WORK/true-helper
    export ENGINEX_STREAM_SYNC=$WORK/true-helper ENGINEX_PERF_TUNE=$WORK/true-helper
    export ENGINEX_ASSIGNMENT_DIR=$base/assignments ENGINEX_ASSIGNMENT_PATCHED=$base/patched
    export ENGINEX_CUSTOM=$base/custom ENGINEX_STATE_DIR=$base/state
    export ENGINEX_QUIT_REASON_FILE=$base/quit-reason
    : > "$base/engine/placeholder"

    printf 'asset=%s gen2=%s\n' "$image" "$gen2" >> "$journal"
    if [ "$gen2" = 1 ]; then
        check_gen2_image "$image"
        # Stock Engine starts first on Gen-2. Its activation restart has no
        # guard pre marker, so it must leave the previous boot evidence alone.
        rm -f "$ENGINEX_GUARD_PRE_MARK"
        printf 'stock activation restart\n' >> "$journal"
        SERVICE_RESULT=success sh "$GUARD" stop
    else
        check_gen1_image "$image"
    fi

    sh "$GUARD" pre
    snapshot 'first hooked boot' "$ui" "$journal"

    # EngineX's language/layout/settings-copy restart reaches systemd as a
    # requested stop, while the child may already have been TERM'd.
    echo AnalyticsChange > "$ENGINEX_QUIT_REASON_FILE"
    printf 'language/layout restart SERVICE_RESULT=signal EXIT_CODE=killed EXIT_STATUS=TERM reason=AnalyticsChange\n' >> "$journal"
    SERVICE_RESULT=signal EXIT_CODE=killed EXIT_STATUS=TERM sh "$GUARD" stop
    sh "$GUARD" pre
    snapshot 'after intentional restart' "$ui" "$journal"

    # The Engine's own Reboot/Poweroff path can race ExecStopPost during
    # shutdown. The reason file is the stock Engine's existing handoff.
    for reason in Reboot Poweroff Reboot; do
        echo "$reason" > "$ENGINEX_QUIT_REASON_FILE"
        printf 'normal %s SERVICE_RESULT=signal EXIT_CODE=killed EXIT_STATUS=TERM reason=%s\n' "$reason" "$reason" >> "$journal"
        SERVICE_RESULT=signal EXIT_CODE=killed EXIT_STATUS=TERM sh "$GUARD" stop
        sh "$GUARD" pre
        snapshot "after $reason" "$ui" "$journal"
    done

    if [ -e "$ui/safe-mode" ] || [ "$(cat "$ui/boot-failures" 2>/dev/null || echo 0)" != 0 ]; then
        cat "$journal"
        echo "FAIL $profile: intentional first-boot sequence entered fallback" >&2
        return 1
    fi
    printf 'PASS %s: no fallback after intentional restart/reboot sequence\n' "$profile"
    cat "$journal"
}

run_crash_loop() {
    local base=$WORK/crash-loop ui=$WORK/crash-loop-ui
    mkdir -p "$ui" "$base/engine" "$base/state" "$base/assignments" "$base/custom"
    export ISSUE311_JOURNAL=$WORK/crash-loop-journal
    export ENGINEX_UI_DIR=$ui ENGINEX_PRIMARY_UI_DIR=$ui ENGINEX_LEGACY_UI_DIR=$base/legacy
    export ENGINEX_ENGINE_DATA_DIR=$base/engine ENGINEX_GUARD_PRE_MARK=$base/pre-mark
    export ENGINEX_ENGINE_GUARDIAN=$WORK/true-helper ENGINEX_RB_BRIDGE=$WORK/true-helper
    export ENGINEX_STREAM_SYNC=$WORK/true-helper ENGINEX_PERF_TUNE=$WORK/true-helper
    export ENGINEX_ASSIGNMENT_DIR=$base/assignments ENGINEX_ASSIGNMENT_PATCHED=$base/patched
    export ENGINEX_CUSTOM=$base/custom ENGINEX_STATE_DIR=$base/state
    export ENGINEX_QUIT_REASON_FILE=$base/quit-reason
    : > "$ISSUE311_JOURNAL"
    for _ in 1 2 3; do
        rm -f "$ENGINEX_QUIT_REASON_FILE"
        sh "$GUARD" pre
        SERVICE_RESULT=signal EXIT_CODE=exited EXIT_STATUS=17 sh "$GUARD" stop
    done
    sh "$GUARD" pre
    grep -qx crash-loop "$ui/safe-mode"
    printf 'PASS real crash loop still enters crash-loop fallback\n'
}

check_asset PRIME4G2-5.3.0.5-EngineX-OS-Denon-PRIME-4-G2-JP24-Update.img
check_asset SCLIVE4-5.3.0.5-EngineX-OS-Denon-SC-LIVE-4-JP21-Update.img
check_asset SC5000-5.3.0.5-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img

run_profile az04 "$ASSETS/PRIME4G2-5.3.0.5-EngineX-OS-Denon-PRIME-4-G2-JP24-Update.img" 1
run_profile gen2-armhf "$ASSETS/SCLIVE4-5.3.0.5-EngineX-OS-Denon-SC-LIVE-4-JP21-Update.img" 1
run_profile gen1-sc5000 "$ASSETS/SC5000-5.3.0.5-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img" 0
run_crash_loop
