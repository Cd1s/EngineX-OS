#!/usr/bin/env bash
# tier2.sh — QEngine emulation test tier for EngineX OS and stock firmware images.
#
# Usage:
#   sim/qengine/tier2.sh <path/to/update.img> [--timeout <seconds>] [--keep]
#
# Builds the emulator instance with pinned upstream QEngine, boots it headless under
# QEMU TCG, verifies boot timing and systemd unit behavior, and prints PASS/FAIL results.
set -euo pipefail
export LC_ALL=C

UPSTREAM_REPO="https://github.com/Peyton-C/QEngine.git"
PINNED_COMMIT="9c2dfd6f206c2e2c3d77cfd1ac483350c68a77c8"
DEFAULT_TIMEOUT=450
KEEP_INSTANCE=0

usage() {
    cat <<EOF
Usage: $0 <path/to/update.img> [options]

Arguments:
  <update.img>       Path to firmware image (*.img)

Options:
  --timeout <sec>    Maximum boot wait time in seconds (default: $DEFAULT_TIMEOUT)
  --keep             Keep the built instance after execution (default: false)
  -h, --help         Show this help message
EOF
    exit 1
}

# Tier-2 emulation requires an x86 host (e.g. the AMD build host). Refuse native ARM hosts
# to prevent registering qemu-arm binfmt, which breaks the native armhf release gate.
case "$(uname -m)" in
    aarch64*|arm*)
        echo "ERROR: tier2.sh must run on an x86 host (such as the AMD build host)." >&2
        echo "Running QEMU arm binfmt on ARM hosts breaks the native armhf release gate." >&2
        exit 1
        ;;
esac

[ $# -ge 1 ] || usage

FIRMWARE=""
TIMEOUT="$DEFAULT_TIMEOUT"

while [ $# -gt 0 ]; do
    case "$1" in
        --timeout)
            TIMEOUT="$2"; shift 2 ;;
        --keep)
            KEEP_INSTANCE=1; shift ;;
        -h|--help)
            usage ;;
        -*)
            echo "ERROR: unknown option: $1" >&2; exit 1 ;;
        *)
            if [ -z "$FIRMWARE" ]; then
                FIRMWARE="$1"; shift
            else
                echo "ERROR: unexpected argument: $1" >&2; exit 1
            fi
            ;;
    esac
done

[ -n "$FIRMWARE" ] || usage
[ -f "$FIRMWARE" ] || { echo "ERROR: firmware image not found: $FIRMWARE" >&2; exit 1; }

FIRMWARE="$(cd "$(dirname "$FIRMWARE")" && pwd)/$(basename "$FIRMWARE")"
WORK_DIR="${QENGINE_WORK_DIR:-/root/build/qengine}"
mkdir -p "$WORK_DIR"

TASK_TMP_DIR="$WORK_DIR/tmp"
mkdir -p "$TASK_TMP_DIR"
export TMPDIR="$TASK_TMP_DIR"

QENGINE_DIR="$WORK_DIR/QEngine"

# Resource tracking for cleanup
TRACKED_CONTAINERS=()
TRACKED_MOUNTS=()
TRACKED_LOOPS=()
QEMU_PID=""
INSTANCE_DIR=""
SERIAL_LOG=""
QEMU_LOG=""

cleanup() {
    local rc=$?
    trap - EXIT INT TERM

    if [ -n "$QEMU_PID" ] && kill -0 "$QEMU_PID" 2>/dev/null; then
        echo "--- Stopping QEMU PID $QEMU_PID ---"
        kill -TERM "$QEMU_PID" 2>/dev/null || true
        sleep 1
        kill -KILL "$QEMU_PID" 2>/dev/null || true
        wait "$QEMU_PID" 2>/dev/null || true
    fi

    for cid in "${TRACKED_CONTAINERS[@]}"; do
        if [ -n "$cid" ]; then
            docker rm -f "$cid" >/dev/null 2>&1 || true
        fi
    done

    for mnt in "${TRACKED_MOUNTS[@]}"; do
        if [ -n "$mnt" ] && mountpoint -q "$mnt" 2>/dev/null; then
            umount "$mnt" 2>/dev/null || umount -l "$mnt" 2>/dev/null || true
        fi
        [ -d "$mnt" ] && rm -rf "$mnt" 2>/dev/null || true
    done

    for loop in "${TRACKED_LOOPS[@]}"; do
        if [ -n "$loop" ]; then
            losetup -d "$loop" 2>/dev/null || true
        fi
    done

    if [ "$KEEP_INSTANCE" -ne 1 ]; then
        [ -n "$INSTANCE_DIR" ] && [ -d "$INSTANCE_DIR" ] && rm -rf "$INSTANCE_DIR"
        [ -n "$SERIAL_LOG" ] && [ -f "$SERIAL_LOG" ] && rm -f "$SERIAL_LOG"
        [ -n "$QEMU_LOG" ] && [ -f "$QEMU_LOG" ] && rm -f "$QEMU_LOG"
        [ -d "$TASK_TMP_DIR" ] && rm -rf "$TASK_TMP_DIR"
    fi

    exit "$rc"
}
trap cleanup EXIT INT TERM

echo "=== EngineX OS QEngine Tier 2 Test Runner ==="
echo "Firmware image : $FIRMWARE"
echo "Timeout limit  : ${TIMEOUT}s"
echo "Working dir    : $WORK_DIR"

# 1. Ensure upstream QEngine is cloned at pinned commit
if [ ! -d "$QENGINE_DIR/.git" ]; then
    echo "--- Cloning upstream QEngine ---"
    git clone "$UPSTREAM_REPO" "$QENGINE_DIR"
fi

CURRENT_REV="$(git -C "$QENGINE_DIR" rev-parse HEAD 2>/dev/null || true)"
if [ "$CURRENT_REV" != "$PINNED_COMMIT" ]; then
    echo "--- Checking out pinned commit $PINNED_COMMIT ---"
    git -C "$QENGINE_DIR" checkout "$PINNED_COMMIT"
fi

# Override hard-coded /tmp paths in upstream scripts to isolate within TASK_TMP_DIR
find "$QENGINE_DIR/scripts/build_scripts" -type f -exec sed -i \
    -e 's|mktemp /tmp/|mktemp "${TMPDIR:-/tmp}"/|g' \
    -e 's|mktemp -d /tmp/|mktemp -d "${TMPDIR:-/tmp}"/|g' \
    -e 's|/tmp/qengine-|"${TMPDIR:-/tmp}"/qengine-|g' \
    -e 's|/tmp/build-|"${TMPDIR:-/tmp}"/build-|g' {} +

# Direct QEngine's drop-in to 99-qengine.conf so EngineX's override.conf remains untouched
if [ -f "$QENGINE_DIR/scripts/build_scripts/build_armv7_engine_rootfs.sh" ]; then
    sed -i 's|/mnt/rootfs/etc/systemd/system/\$APP_UNIT.d/override.conf|/mnt/rootfs/etc/systemd/system/\$APP_UNIT.d/99-qengine.conf|' "$QENGINE_DIR/scripts/build_scripts/build_armv7_engine_rootfs.sh"
fi

# Fix upstream launcher regex for EngineX engine invocations
if [ -f "$QENGINE_DIR/scripts/build_scripts/rootfs_steps/skip_firmware_update.sh" ]; then
    sed -i 's|sed -i .*-skipFirmwareUpdate.*|sed -i "s\\|/usr/Engine/Engine \\|/usr/Engine/Engine -skipFirmwareUpdate \\|\" \"\$_launcher\"|' "$QENGINE_DIR/scripts/build_scripts/rootfs_steps/skip_firmware_update.sh"
fi

# 2. Check prerequisites on the host
for tool in qemu-system-aarch64 qemu-img docker file dumpe2fs debugfs losetup; do
    command -v "$tool" >/dev/null 2>&1 || {
        echo "ERROR: required host tool '$tool' not found." >&2
        exit 1
    }
done

# 3. Ensure generic kernel and initrd exist
if [ ! -s "$QENGINE_DIR/build/vmlinuz-generic-armhf" ] || [ ! -s "$QENGINE_DIR/build/initrd-generic-armhf" ]; then
    echo "--- Building armhf generic kernel and initrd ---"
    (cd "$QENGINE_DIR" && ./scripts/build_scripts/get_kernel.sh --arch armhf)
fi

# 4. Create QEngine instance
IMG_BASE="$(basename "$FIRMWARE" .img | tr '[:upper:]' '[:lower:]' | tr -c 'a-z0-9_' '_')"
INSTANCE_NAME="t2_${IMG_BASE}_$$"
INSTANCE_DIR="$QENGINE_DIR/build/instances/$INSTANCE_NAME"

echo "--- Building instance $INSTANCE_NAME ---"
(cd "$QENGINE_DIR" && ./scripts/build_scripts/new_instance.sh --name "$INSTANCE_NAME" --device engine --firmware "$FIRMWARE")

ENV_FILE="$INSTANCE_DIR/instance.env"
[ -f "$ENV_FILE" ] || { echo "ERROR: $ENV_FILE missing." >&2; exit 1; }

# Cap SMP to 4 (avoids lock contention and soft lockups under ARMv7 TCG)
echo "SMP=4" >> "$ENV_FILE"

# Extract instance settings
SSH_PORT="$(sed -n 's/^SSH_PORT=//p' "$ENV_FILE")"
ROOTFS_IMG="$(sed -n 's/^ROOTFS_IMG=//p' "$ENV_FILE")"

# 5. Fix test-side rootfs conflicts and setup automated probe access
echo "--- Configuring emulated rootfs for headless probing ---"
SSH_KEY_PRIV="$WORK_DIR/id_tier2_ed25519"
if [ ! -f "$SSH_KEY_PRIV" ]; then
    ssh-keygen -t ed25519 -N "" -f "$SSH_KEY_PRIV" -C "tier2-test" >/dev/null 2>&1
fi
SSH_PUB_CONTENT="$(cat "${SSH_KEY_PRIV}.pub")"

MNT_DIR="$(mktemp -d "$TASK_TMP_DIR/mnt.XXXXXX")"
TRACKED_MOUNTS+=("$MNT_DIR")

LOOP_DEV="$(losetup -f --show "$ROOTFS_IMG")"
TRACKED_LOOPS+=("$LOOP_DEV")

if ! mount "$LOOP_DEV" "$MNT_DIR"; then
    echo "ERROR: failed to mount $LOOP_DEV on $MNT_DIR" >&2
    exit 1
fi

IS_ENGINEX=0
if [ -f "$MNT_DIR/etc/sc5000/engine-ui-guard" ] || [ -f "$MNT_DIR/etc/systemd/system/engine.service.d/60-ui-hook.conf" ]; then
    IS_ENGINEX=1
    echo "--- Detected EngineX OS image ---"
else
    echo "--- Detected stock inMusic image ---"
fi

if [ "$IS_ENGINEX" -eq 1 ]; then
    # Do NOT write /etc/ld.so.preload: OpenSSH unprivileged privsep child seccomp sandbox
    # triggers SIGSYS on constructor syscalls (eventfd2, pthread_create) when preloaded.
    # Instead, patch engine-ui-guard so LD_PRELOAD includes QEngine shims for Engine only.
    if [ -f "$MNT_DIR/etc/sc5000/engine-ui-guard" ]; then
        sed -i 's|\${SC5000_UI_HOOK:-/etc/sc5000/libengine-ui-hook.so}|\${SC5000_UI_HOOK:-/etc/sc5000/libengine-ui-hook.so}:/root/dtshim.so:/root/drmatomic.so:/root/alsashim.so:/root/quitshim.so|g' "$MNT_DIR/etc/sc5000/engine-ui-guard"
    fi

    # Strip UsePAM from sshd configs: az0x OpenSSH is built without PAM and fails rexec if UsePAM is present
    for cfg in "$MNT_DIR/usr/share/sc5000-custom/sshd_config" "$MNT_DIR/etc/ssh/sshd_config"; do
        if [ -f "$cfg" ]; then
            sed -i '/UsePAM/d' "$cfg"
        fi
    done

    # Seed automated public key into sc5000-custom seed location so prepare copies it to /data
    if [ -d "$MNT_DIR/usr/share/sc5000-custom" ]; then
        echo "$SSH_PUB_CONTENT" > "$MNT_DIR/usr/share/sc5000-custom/authorized_keys"
        chmod 600 "$MNT_DIR/usr/share/sc5000-custom/authorized_keys"
    fi

    # Mask hardware-only services that cannot succeed in QEMU (avoids systemctl --failed noise)
    ln -sf /dev/null "$MNT_DIR/etc/systemd/system/az0x-info.service"

    # Clean 0-byte RSA key stub that causes OpenSSH daemon to terminate KEXINIT
    rm -f "$MNT_DIR/etc/ssh/ssh_host_rsa_key"
    rm -f "$MNT_DIR/etc/systemd/system/multi-user.target.wants/sshd.service"
    mkdir -p "$MNT_DIR/etc/systemd/system/multi-user.target.wants"
    ln -sf /usr/lib/systemd/system/sc5000-custom-sshd.service "$MNT_DIR/etc/systemd/system/multi-user.target.wants/sc5000-custom-sshd.service"
else
    # Stock inMusic firmware uses stock sshd.service
    mkdir -p "$MNT_DIR/etc/systemd/system/multi-user.target.wants"
    ln -sf /lib/systemd/system/sshd.service "$MNT_DIR/etc/systemd/system/multi-user.target.wants/sshd.service"
    if [ -f "$MNT_DIR/etc/ssh/sshd_config" ]; then
        echo "PermitRootLogin yes" >> "$MNT_DIR/etc/ssh/sshd_config"
    fi
fi

# Setup SSH test key in /root/.ssh as well
mkdir -p "$MNT_DIR/root/.ssh"
chmod 700 "$MNT_DIR/root/.ssh"
echo "$SSH_PUB_CONTENT" > "$MNT_DIR/root/.ssh/authorized_keys"
chmod 600 "$MNT_DIR/root/.ssh/authorized_keys"

# Configure network interface DHCP for systemd-networkd
mkdir -p "$MNT_DIR/etc/systemd/network"
cat > "$MNT_DIR/etc/systemd/network/20-wired.network" <<'CFG'
[Match]
Name=eth* en*

[Network]
DHCP=yes
CFG

# Mask hardware-bound verity / hostname units that fail under generic emulator
ln -sf /dev/null "$MNT_DIR/etc/systemd/system/az0x-loadpin-trustpoints.service"
ln -sf /dev/null "$MNT_DIR/etc/systemd/system/az0x-setup-hostname.service"

# Setup serial autologin on ttyAMA0
mkdir -p "$MNT_DIR/etc/systemd/system/serial-getty@ttyAMA0.service.d"
cat > "$MNT_DIR/etc/systemd/system/serial-getty@ttyAMA0.service.d/autologin.conf" <<'CFG'
[Service]
ExecStart=
ExecStart=-/sbin/agetty --autologin root --keep-baud 115200,38400,9600 %I \$TERM
CFG

umount "$MNT_DIR"
losetup -d "$LOOP_DEV"
rm -rf "$MNT_DIR"

# Clean tracked mount and loop
TRACKED_MOUNTS=()
TRACKED_LOOPS=()

# 6. Launch QEMU in headless TCG mode with tuned performance options
SERIAL_LOG="$WORK_DIR/${INSTANCE_NAME}.serial.log"
QEMU_LOG="$WORK_DIR/${INSTANCE_NAME}.qemu.log"
rm -f "$SERIAL_LOG" "$QEMU_LOG"

echo "--- Booting $INSTANCE_NAME (TCG, headless, -display none) ---"
T0=$(date +%s%N)

export AUDIODEV_BACKEND=none
export ACCEL="${ACCEL:-tcg,thread=multi,tb-size=1024}"
export CPU="${CPU:-cortex-a15}"
export SMP="${SMP:-4}"
export DISPLAY_MODE=none
export SERIAL="file:$SERIAL_LOG"
export KERNEL_EXTRA_ARGS="systemd.show_status=1 systemd.log_color=0"

"$QENGINE_DIR/scripts/qemu/run_instance.sh" --name "$INSTANCE_NAME" --display none --no-gl > "$QEMU_LOG" 2>&1 &
QEMU_PID=$!
echo "QEMU PID: $QEMU_PID (SSH port: $SSH_PORT)"

# 7. Probing
echo "--- Probing guest boot status (max ${TIMEOUT}s) ---"
SSH_OK=0
ENGINE_OK=0
ENGINE_T=0

guest_exec() {
    ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 -o BatchMode=yes \
        -o HostKeyAlgorithms=ssh-ed25519 -i "$SSH_KEY_PRIV" -p "$SSH_PORT" root@127.0.0.1 "$@"
}

deadline=$(( $(date +%s) + TIMEOUT ))
while [ "$(date +%s)" -lt "$deadline" ]; do
    # Check serial log for Engine start
    if [ "$ENGINE_OK" -eq 0 ] && [ -f "$SERIAL_LOG" ]; then
        if grep -q "Starting Engine" "$SERIAL_LOG" 2>/dev/null || grep -q "Started Engine" "$SERIAL_LOG" 2>/dev/null; then
            ENGINE_OK=1
            ENGINE_T=$(date +%s%N)
            echo "-> Engine start detected in serial console."
        fi
    fi

    # Check SSH probe
    if guest_exec 'echo READY' 2>/dev/null | grep -q READY; then
        SSH_OK=1
        if [ "$ENGINE_OK" -eq 0 ]; then
            st="$(guest_exec 'systemctl is-active engine.service 2>/dev/null || pgrep -f /usr/Engine || true' 2>/dev/null | tr -d '\r\n')"
            if [ "$st" = "active" ] || echo "$st" | grep -qE '^[0-9]+$'; then
                ENGINE_OK=1
                ENGINE_T=$(date +%s%N)
            fi
        fi
        if [ "$ENGINE_OK" -eq 1 ]; then
            break
        fi
    fi
    sleep 2
done

if [ "$ENGINE_OK" -eq 0 ]; then
    echo "FAIL: Engine did not start within ${TIMEOUT}s timeout."
    tail -n 40 "$SERIAL_LOG" 2>/dev/null || true
    exit 1
fi

TOTAL_SECS=$(awk -v t0="$T0" -v t1="$ENGINE_T" 'BEGIN { printf "%.2f", (t1-t0)/1000000000 }')
echo "=== Boot-to-Engine Time: ${TOTAL_SECS}s ==="

ALL_PASS=1
report() {
    local status="$1"; shift
    local desc="$*"
    if [ "$status" = "PASS" ]; then
        printf '  [PASS] %s\n' "$desc"
    else
        printf '  [FAIL] %s\n' "$desc"
        ALL_PASS=0
    fi
}

echo ""
echo "=== Test Results ==="

if [ "$IS_ENGINEX" -eq 0 ]; then
    report PASS "Stock boot to Engine in ${TOTAL_SECS}s"
else
    if [ "$SSH_OK" -ne 1 ]; then
        report FAIL "SSH access unavailable to perform EngineX unit verification"
    else
        report PASS "EngineX boot to Engine in ${TOTAL_SECS}s"

        # Check 1: systemctl --failed is empty
        FAILED_UNITS="$(guest_exec 'systemctl --failed --no-legend 2>/dev/null || true' | tr -d '\r' | grep -v '^$' || true)"
        if [ -z "$FAILED_UNITS" ]; then
            report PASS "systemctl --failed is empty"
        else
            report FAIL "systemctl --failed has failures: $(echo "$FAILED_UNITS" | awk '{print $1}' | tr '\n' ' ')"
        fi

        # Check 2: sc5000-custom-prepare, stream-sync, rb-bridge, sc5000-deemixd, engine-guardian, engine active
        for unit in sc5000-custom-prepare.service stream-sync.service rb-bridge.service sc5000-deemixd.service engine-guardian.service engine.service; do
            u_state="$(guest_exec "systemctl is-active $unit 2>/dev/null || true" | tr -d '\r\n')"
            if [ "$u_state" = "active" ]; then
                report PASS "Unit $unit is active"
            else
                report FAIL "Unit $unit is $u_state (expected active)"
            fi
        done

        # Check 3: UI hook log reports QML replacement count
        HOOK_LOG="$(guest_exec 'journalctl -u engine.service 2>/dev/null | grep -E "engine-ui-hook:.*files" || cat /tmp/libengine-ui-hook.log 2>/dev/null || true')"
        if echo "$HOOK_LOG" | grep -qE '[0-9]+ files'; then
            COUNT="$(echo "$HOOK_LOG" | grep -oE '[0-9]+ files' | head -n1)"
            report PASS "UI hook reports QML replacement count ($COUNT)"
        else
            report FAIL "UI hook did not report QML replacement count"
        fi

        # Check 4: sc5000-custom-prepare before engine
        ORDER_CHECK="$(guest_exec 'systemd-analyze critical-chain engine.service 2>/dev/null || systemctl show engine.service -p After 2>/dev/null || true')"
        if echo "$ORDER_CHECK" | grep -q "sc5000-custom-prepare"; then
            report PASS "sc5000-custom-prepare is ordered before engine"
        else
            report FAIL "sc5000-custom-prepare is not ordered before engine"
        fi

        # Check 5: curl 127.0.0.1:8080/v1/sc5000/ui returns 200
        HTTP_CODE="$(guest_exec 'curl -s -o /dev/null -w "%{http_code}" http://127.0.0.1:8080/v1/sc5000/ui 2>/dev/null || true' | tr -d '\r\n')"
        if [ "$HTTP_CODE" = "200" ]; then
            report PASS "stream-sync API 127.0.0.1:8080/v1/sc5000/ui returned 200"
        else
            report FAIL "stream-sync API returned $HTTP_CODE (expected 200)"
        fi

        # Check 6: journal has no errors from our units
        if JOURNAL_OUT="$(guest_exec 'journalctl --no-pager -p err..emerg -u sc5000-custom-prepare -u stream-sync -u rb-bridge -u sc5000-deemixd -u engine-guardian -u engine 2>&1')"; then
            ERR_LINES="$(echo "$JOURNAL_OUT" | tr -d '\r' | grep -v '^\s*$' | grep -v -- '-- No entries --' || true)"
            if [ -z "$ERR_LINES" ]; then
                report PASS "Journal has no errors from EngineX units"
            else
                report FAIL "Journal errors detected from EngineX units: $(echo "$ERR_LINES" | head -n 3 | tr '\n' '; ')"
            fi
        else
            report FAIL "journalctl query failed: $(echo "$JOURNAL_OUT" | head -n 1)"
        fi
    fi
fi

# Graceful poweroff
if [ "$SSH_OK" -eq 1 ]; then
    echo "--- Powering off guest cleanly ---"
    guest_exec 'poweroff' 2>/dev/null || true
    wait "$QEMU_PID" 2>/dev/null || true
fi

echo ""
if [ "$ALL_PASS" -eq 1 ]; then
    echo "=== TIER 2 ALL CHECKS PASSED ==="
    exit 0
else
    echo "=== TIER 2 CHECKS FAILED ==="
    exit 1
fi
