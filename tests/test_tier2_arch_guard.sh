#!/bin/bash
# test_tier2_arch_guard.sh — verify sim/qengine/tier2.sh refuses ARM hosts without side effects
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TIER2="$ROOT/sim/qengine/tier2.sh"
TMP_DIR="$(mktemp -d /tmp/arch_guard_test.XXXXXX)"
trap 'rm -rf "$TMP_DIR"' EXIT

SHIM_DIR="$TMP_DIR/bin"
mkdir -p "$SHIM_DIR"
COMMANDS_LOG="$TMP_DIR/commands.log"
: > "$COMMANDS_LOG"

# Create recording shims for commands that must NEVER be called when the guard trips
for cmd in qemu-system-aarch64 qemu-system-arm qemu-img docker losetup systemctl binfmt-support; do
    cat > "$SHIM_DIR/$cmd" <<EOF
#!/bin/sh
printf '%s\n' "\$0 \$*" >> "$COMMANDS_LOG"
exit 0
EOF
    chmod +x "$SHIM_DIR/$cmd"
done

# Fake firmware image
DUMMY_FW="$TMP_DIR/dummy-Update.img"
touch "$DUMMY_FW"

# Test 1: aarch64 with normal arguments must be rejected immediately
cat > "$SHIM_DIR/uname" <<'EOF'
#!/bin/sh
if [ "$1" = "-m" ]; then
    echo "aarch64"
else
    /bin/uname "$@"
fi
EOF
chmod +x "$SHIM_DIR/uname"

TEST_WORK_DIR="$TMP_DIR/work_aarch64"
set +e
OUTPUT=$(PATH="$SHIM_DIR:$PATH" QENGINE_WORK_DIR="$TEST_WORK_DIR" "$TIER2" "$DUMMY_FW" 2>&1)
EXIT_CODE=$?
set -e

# Assertions for aarch64
if [ "$EXIT_CODE" -eq 0 ]; then
    echo "FAIL: tier2.sh exited 0 on aarch64 host (expected non-zero)" >&2
    exit 1
fi

if ! echo "$OUTPUT" | grep -q "ERROR: tier2.sh must run on an x86 host"; then
    echo "FAIL: tier2.sh did not print expected architecture error message" >&2
    echo "Output was: $OUTPUT" >&2
    exit 1
fi

if [ -e "$TEST_WORK_DIR" ]; then
    echo "FAIL: work directory was created despite architecture rejection: $TEST_WORK_DIR" >&2
    exit 1
fi

if [ -s "$COMMANDS_LOG" ]; then
    echo "FAIL: prohibited host commands were called before/during architecture check:" >&2
    cat "$COMMANDS_LOG" >&2
    exit 1
fi

# Test 2: armv7l with normal arguments must also be rejected immediately
cat > "$SHIM_DIR/uname" <<'EOF'
#!/bin/sh
if [ "$1" = "-m" ]; then
    echo "armv7l"
else
    /bin/uname "$@"
fi
EOF
chmod +x "$SHIM_DIR/uname"

TEST_WORK_DIR="$TMP_DIR/work_armv7l"
set +e
OUTPUT=$(PATH="$SHIM_DIR:$PATH" QENGINE_WORK_DIR="$TEST_WORK_DIR" "$TIER2" "$DUMMY_FW" 2>&1)
EXIT_CODE=$?
set -e

if [ "$EXIT_CODE" -eq 0 ]; then
    echo "FAIL: tier2.sh exited 0 on armv7l host (expected non-zero)" >&2
    exit 1
fi

if ! echo "$OUTPUT" | grep -q "ERROR: tier2.sh must run on an x86 host"; then
    echo "FAIL: tier2.sh did not reject armv7l host" >&2
    exit 1
fi

if [ -e "$TEST_WORK_DIR" ]; then
    echo "FAIL: work directory was created on armv7l host" >&2
    exit 1
fi

if [ -s "$COMMANDS_LOG" ]; then
    echo "FAIL: prohibited host commands were called on armv7l host:" >&2
    cat "$COMMANDS_LOG" >&2
    exit 1
fi

# Test 3: x86_64 host passes architecture guard
cat > "$SHIM_DIR/uname" <<'EOF'
#!/bin/sh
if [ "$1" = "-m" ]; then
    echo "x86_64"
else
    /bin/uname "$@"
fi
EOF
chmod +x "$SHIM_DIR/uname"

set +e
OUTPUT=$(PATH="$SHIM_DIR:$PATH" "$TIER2" --help 2>&1)
EXIT_CODE=$?
set -e

if echo "$OUTPUT" | grep -q "ERROR: tier2.sh must run on an x86 host"; then
    echo "FAIL: tier2.sh falsely rejected x86_64 host" >&2
    exit 1
fi

if ! echo "$OUTPUT" | grep -q "Usage:"; then
    echo "FAIL: tier2.sh did not print usage on --help" >&2
    exit 1
fi

echo "PASS: tier2 arch guard correctly enforces x86_64 and rejects aarch64/armv7l without side effects"
