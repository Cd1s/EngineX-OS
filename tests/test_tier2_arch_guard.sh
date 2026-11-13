#!/bin/bash
# test_tier2_arch_guard.sh — verify sim/qengine/tier2.sh refuses ARM hosts
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TIER2="$ROOT/sim/qengine/tier2.sh"
TMP_DIR="$(mktemp -d /tmp/arch_guard_test.XXXXXX)"
trap 'rm -rf "$TMP_DIR"' EXIT

# Test 1: aarch64 must be rejected
cat > "$TMP_DIR/uname" <<'EOF'
#!/bin/sh
if [ "$1" = "-m" ]; then
    echo "aarch64"
else
    /bin/uname "$@"
fi
EOF
chmod +x "$TMP_DIR/uname"

OUTPUT=$(PATH="$TMP_DIR:$PATH" "$TIER2" --help 2>&1 || true)
if ! echo "$OUTPUT" | grep -q "ERROR: tier2.sh must run on an x86 host"; then
    echo "FAIL: tier2.sh did not reject aarch64 host" >&2
    echo "Output was: $OUTPUT" >&2
    exit 1
fi

# Test 2: armv7l must be rejected
cat > "$TMP_DIR/uname" <<'EOF'
#!/bin/sh
if [ "$1" = "-m" ]; then
    echo "armv7l"
else
    /bin/uname "$@"
fi
EOF
chmod +x "$TMP_DIR/uname"

OUTPUT=$(PATH="$TMP_DIR:$PATH" "$TIER2" --help 2>&1 || true)
if ! echo "$OUTPUT" | grep -q "ERROR: tier2.sh must run on an x86 host"; then
    echo "FAIL: tier2.sh did not reject armv7l host" >&2
    echo "Output was: $OUTPUT" >&2
    exit 1
fi

# Test 3: x86_64 must pass architecture guard
cat > "$TMP_DIR/uname" <<'EOF'
#!/bin/sh
if [ "$1" = "-m" ]; then
    echo "x86_64"
else
    /bin/uname "$@"
fi
EOF
chmod +x "$TMP_DIR/uname"

OUTPUT=$(PATH="$TMP_DIR:$PATH" "$TIER2" --help 2>&1 || true)
if echo "$OUTPUT" | grep -q "ERROR: tier2.sh must run on an x86 host"; then
    echo "FAIL: tier2.sh falsely rejected x86_64 host" >&2
    exit 1
fi
if ! echo "$OUTPUT" | grep -q "Usage: "; then
    echo "FAIL: tier2.sh did not display usage on x86_64" >&2
    exit 1
fi

echo "PASS: tier2 arch guard correctly enforces x86_64 and rejects aarch64/armv7l"
