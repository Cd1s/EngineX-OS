#!/usr/bin/env bash
# Real-systemd regression test for Gen2 boot-activate.sh running inside engine.service's cgroup.
# Proves:
#   1) Old 'systemctl stop <unit> && restore && systemctl start' inside <unit>'s cgroup kills the
#      activator script mid-flight and leaves <unit> dead (regression canary that fails on 03edab3).
#   2) New engine.service drop-in 'ExecStartPre=-.../boot-activate.sh --restore-tz' + 'systemctl
#      restart --no-block <unit>' survives cgroup teardown, runs AFTER the unhooked Engine's
#      SIGTERM destructor flushed stale Engine.conf, writes TZEnvVar=Asia/Shanghai before the
#      hooked Engine starts, and brings the hooked Engine up active.
set -euo pipefail
export LC_ALL=C

if [ "$(id -u)" != 0 ] || [ "$(cat /proc/1/comm 2>/dev/null || :)" != systemd ]; then
    echo "test_gen2_systemd_activate: requires root on a live systemd host" >&2
    exit 2
fi

REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
TMP=$(mktemp -d /var/tmp/gen2-sysd-test.XXXXXX)
UNIT="enginex-gen2-cgtest-$$.service"
UNIT_FILE="/run/systemd/system/$UNIT"
DROPIN_DIR="/run/systemd/system/${UNIT}.d"

cleanup() {
    systemctl stop "$UNIT" >/dev/null 2>&1 || :
    systemctl reset-failed "$UNIT" >/dev/null 2>&1 || :
    rm -rf "$UNIT_FILE" "$DROPIN_DIR" "$TMP"
    systemctl daemon-reload >/dev/null 2>&1 || :
}
trap cleanup EXIT

# Extract real BOOT_ACTIVATE from build-data-bundle.py
DEST="$TMP/data/enginex"
mkdir -p "$DEST/bundle" "$DEST/state" "$TMP/zoneinfo/Asia" "$TMP/zoneinfo/Europe"
printf 'TZif2' > "$TMP/zoneinfo/Asia/Shanghai"
printf 'TZif2' > "$TMP/zoneinfo/Europe/London"
printf 'JP21\n' > "$TMP/product-code"

python3 -I - "$REPO" "$DEST/bundle/boot-activate.sh" << 'PY'
import importlib.util, pathlib, sys
repo, out = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2])
spec = importlib.util.spec_from_file_location("bdb", repo / "tools/az01-packer/build-data-bundle.py")
bdb = importlib.util.module_from_spec(spec); spec.loader.exec_module(bdb)
ntp = (repo / "rootfs-overlay/etc/systemd/timesyncd.conf.d/10-enginex-ntp.conf").read_text()
text = bdb.BOOT_ACTIVATE.replace("@MIGRATE_IF_DEFAULT@", "").replace("@TIMESYNCD_DROPIN@", ntp)
out.write_text(text)
PY
chmod 0755 "$DEST/bundle/boot-activate.sh"

# Mock Engine (Type=forking like stock /usr/Engine/Scripts/runengine):
# - When unhooked (ENGINEX_TEST_HOOKED!=1): spawns $1 activator inside its own cgroup, and on SIGTERM
#   simulates QSettings::~QSettings() flushing stale TZEnvVar=Europe/London to Engine.conf!
# - When hooked (ENGINEX_TEST_HOOKED=1): records the TZEnvVar present in Engine.conf at startup and sleeps.
cat > "$TMP/mock-runengine.sh" << 'EOF'
#!/bin/sh
WORK=$1
ACTIVATOR=$2
CONF="$WORK/data/.config/jp21.user.settings/AIR Music Technology/Engine.conf"
(
    if [ "${ENGINEX_TEST_HOOKED:-0}" = 1 ]; then
        cp "$CONF" "$WORK/hooked-startup-Engine.conf" 2>/dev/null || :
        echo "HOOKED_RUNNING" > "$WORK/hooked-ready"
        trap 'exit 0' TERM INT
        while :; do sleep 1 & wait $! || :; done
    else
        trap 'mkdir -p "$(dirname "$CONF")"; printf "[TimeDateSettings]\nTZEnvVar=Europe/London\n" > "$CONF"; echo DESTRUCTOR_RAN > "$WORK/destructor-ran"; exit 0' TERM
        "$ACTIVATOR" >"$WORK/activator.log" 2>&1 &
        while :; do sleep 1 & wait $! || :; done
    fi
) </dev/null >/dev/null 2>&1 &
echo $! > "$WORK/engine.pid"
EOF
chmod 0755 "$TMP/mock-runengine.sh"

# --- Phase 1: Prove the OLD 'stop -> restore -> start' inside the unit's cgroup FAILS (leaves unit dead) ---
cat > "$TMP/old-activator.sh" << EOF
#!/bin/sh
sleep 0.2
systemctl stop "$UNIT"
echo "SHOULD_NEVER_REACH" > "$TMP/old-survived"
systemctl start --no-block "$UNIT"
EOF
chmod 0755 "$TMP/old-activator.sh"

cat > "$UNIT_FILE" << EOF
[Unit]
Description=EngineX Gen2 cgroup self-stop regression canary ($$)

[Service]
Type=forking
PIDFile=$TMP/engine.pid
ExecStart=$TMP/mock-runengine.sh $TMP $TMP/old-activator.sh
Restart=on-failure
EOF

systemctl daemon-reload
systemctl start "$UNIT"
for _ in $(seq 1 30); do
    ! systemctl is-active --quiet "$UNIT" && break
    sleep 0.1
done
sleep 0.3
if systemctl is-active --quiet "$UNIT" || [ -e "$TMP/old-survived" ]; then
    echo "FAIL: old stop->write->start unexpectedly survived cgroup teardown" >&2
    exit 1
fi
echo "PASS 1 (canary): confirmed old 'systemctl stop' inside engine.service cgroup kills activator and leaves service dead"

# --- Phase 2: Prove NEW ExecStartPre=-.../boot-activate.sh --restore-tz + 'systemctl restart --no-block' SUCCEEDS ---
rm -f "$TMP/destructor-ran" "$TMP/hooked-ready" "$TMP/hooked-startup-Engine.conf"
QCONF="$TMP/data/.config/jp21.user.settings/AIR Music Technology/Engine.conf"
mkdir -p "$(dirname "$QCONF")"
printf '[TimeDateSettings]\nTZEnvVar=Europe/London\n' > "$QCONF"
touch -d '2026-11-10T00:00:00Z' "$QCONF"
printf 'Asia/Shanghai\n' > "$DEST/state/timezone"
touch -d '2026-11-11T00:00:00Z' "$DEST/state/timezone"

# Build an isolated copy of the real boot-activate.sh targeting ONLY our transient $UNIT and $TMP paths
python3 -I - "$DEST/bundle/boot-activate.sh" "$TMP/isolated-boot-activate.sh" "$UNIT" "$DROPIN_DIR" "$TMP" << 'PY'
import pathlib, sys
src, dst, unit, dropin_dir, tmp = sys.argv[1:]
s = pathlib.Path(src).read_text()
# Redirect engine.service drop-in and restart calls strictly to our transient test unit, and skip real deck daemons
s = s.replace("/run/systemd/system/engine.service.d", dropin_dir)
s = s.replace("Wants=enginex-stream-sync.service enginex-rb-bridge.service enginex-sc5000-deemixd.service\n", "")
s = s.replace('Environment="LD_PRELOAD=$lib_hook"\n', 'Environment=ENGINEX_TEST_HOOKED=1\n')
s = s.replace("ExecStartPre=/bin/mkdir -p -m 0700 /tmp/sc5000-engine-home\n",
              f"Environment=SC5000_CUSTOM={tmp}/data/enginex SC5000_CONFIG_DIR={tmp}/data/.config SC5000_ZONEINFO_DIR={tmp}/zoneinfo SC5000_PRODUCT_CODE_FILE={tmp}/product-code SC5000_TZ_NEWER_MARKER={tmp}/tz-newer\n")
s = s.replace("$bundle_dir/boot-activate.sh --restore-tz", f"{dst} --restore-tz")
s = s.replace("/run/systemd/timesyncd.conf.d/10-enginex-ntp.conf", f"{tmp}/ntp.conf")
s = s.replace("/run/systemd/timesyncd.conf.d", f"{tmp}/ntp.d")
s = s.replace("systemctl restart --no-block enginex-stream-sync.service enginex-rb-bridge.service enginex-sc5000-deemixd.service || :", ":")
s = s.replace('[ "$timesyncd_changed" = 0 ] || systemctl restart --no-block systemd-timesyncd.service || :', ":")
s = s.replace("engine.service", unit)
pathlib.Path(dst).write_text(s)
PY
chmod 0755 "$TMP/isolated-boot-activate.sh"

cat > "$TMP/new-activator-wrapper.sh" << EOF
#!/bin/sh
sleep 0.2
export SC5000_CUSTOM="$DEST"
export SC5000_CONFIG_DIR="$TMP/data/.config"
export SC5000_ZONEINFO_DIR="$TMP/zoneinfo"
export SC5000_PRODUCT_CODE_FILE="$TMP/product-code"
export SC5000_TZ_NEWER_MARKER="$TMP/tz-newer"
export SC5000_TIMESYNC_LIB_DIR="$TMP/missing-var-lib"
exec "$TMP/isolated-boot-activate.sh"
EOF
chmod 0755 "$TMP/new-activator-wrapper.sh"

cat > "$UNIT_FILE" << EOF
[Unit]
Description=EngineX Gen2 ExecStartPre restore test ($$)

[Service]
Type=forking
PIDFile=$TMP/engine.pid
ExecStart=$TMP/mock-runengine.sh $TMP $TMP/new-activator-wrapper.sh
Restart=on-failure
EOF

systemctl daemon-reload
systemctl start "$UNIT"

for _ in $(seq 1 50); do
    [ -f "$TMP/hooked-ready" ] && break
    sleep 0.1
done

[ -f "$TMP/destructor-ran" ] || { cat "$TMP/activator.log" >&2 || :; systemctl status "$UNIT" --no-pager >&2 || :; echo "FAIL: unhooked Engine SIGTERM destructor did not run" >&2; exit 1; }
[ -f "$TMP/hooked-ready" ] || { echo "FAIL: hooked Engine never started after boot-activate.sh" >&2; exit 1; }
systemctl is-active --quiet "$UNIT" || { echo "FAIL: $UNIT is not active after restart" >&2; exit 1; }
grep -Fqx "TZEnvVar=Asia/Shanghai" "$TMP/hooked-startup-Engine.conf" || {
    cat "$TMP/hooked-startup-Engine.conf" >&2 || :
    echo "FAIL: hooked Engine did not see TZEnvVar=Asia/Shanghai at startup (destructor race or ExecStartPre missed)" >&2
    exit 1
}
echo "PASS 2: real systemd restarted $UNIT from inside its own cgroup; ExecStartPre=--restore-tz ran after old destructor and hooked Engine booted active with TZEnvVar=Asia/Shanghai"
