#!/usr/bin/env bash
# Native full-userspace boot. Mounts exist only in the private mount namespace.
set -euo pipefail
export LC_ALL=C
SELF=$(realpath "$0")
REPO=$(realpath "$(dirname "$SELF")/../..")

inner() {
    W=$1 R=$1/root
    runner=
    local init_pid='' failed=0
    mount --make-rprivate /
    mount -o loop "$W/boot.ext2" "$R"
    trap 'if [ -n "${runner:-}" ]; then kill -TERM "$runner" 2>/dev/null || :; wait "$runner" 2>/dev/null || :; fi; umount "$R"' EXIT
    mkdir -p "$R/opt/nspawn" "$R/etc/systemd/system/engine.service.d"
    # Same destructive-command recorder as release-gate.sh; keep the real
    # systemctl here because the image's PID 1 must manage its own services.
    cp "$REPO/tools/az01-packer/gate-record.sh" "$R/opt/nspawn/record"
    chmod 755 "$R/opt/nspawn/record"
    local p target
    for p in /usr/sbin/kexec /sbin/reboot /usr/Engine/Scripts/encrypt-fs.sh /usr/sbin/az01-update; do
        target=$(chroot "$R" /bin/readlink -f "$(dirname "$p")")/$(basename "$p")
        cp --remove-destination "$R/opt/nspawn/record" "$R$target"
    done
    # The replacement /data has no eMMC and hardware firmware is never updated.
    cat > "$W/masked-units.txt" <<'EOF'
az0x-data-mkfs.service
touch-fw-update.service
xmos-update.service
bluetooth.service
az0x-loadpin-trustpoints.service
az01-usbsata-fixer.service
az0x-info.service
systemd-networkd-wait-online.service
EOF
    while read -r p; do ln -sf /dev/null "$R/etc/systemd/system/$p"; done < "$W/masked-units.txt"
    # Describe the pre-mounted empty /data without the vendor eMMC mkfs dependency.
    rm -f "$R/etc/systemd/system/data.mount"
    cat > "$R/etc/systemd/system/data.mount" <<'EOF'
[Mount]
What=/opt/nspawn/data
Where=/data
Type=none
Options=bind
EOF
    cat > "$R/etc/systemd/system/engine.service.d/99-nspawn.conf" <<'EOF'
[Service]
Environment=QT_QPA_PLATFORM=offscreen
StandardOutput=journal
StandardError=journal
EOF
    # etc-reset was attached to etc.mount on older images. Enable it explicitly
    # before prepare when testing the 5.3 rootfs without that mount unit.
    mkdir -p "$R/etc/systemd/system/sc5000-custom-prepare.service.d"
    cat > "$R/etc/systemd/system/sc5000-custom-prepare.service.d/99-nspawn.conf" <<'EOF'
[Unit]
Wants=sc5000-custom-etc-reset.service
After=sc5000-custom-etc-reset.service
EOF
    printf '%s' "$PRODUCT" > "$W/dt/inmusic,product-code"
    printf 'inmusic,%s\0rockchip,rk3288\0' "${PRODUCT,,}" > "$W/dt/compatible"
    # Reuse the gate's serial IRQ cover. Never expose host IRQ affinity writes.
    printf '           CPU0\n 41:        100     GICv2  32 Level     ttyS0\n' > "$R/opt/nspawn/interrupts"
    mkdir -p "$R/opt/nspawn/irq/41"
    printf 'f\n' > "$R/opt/nspawn/irq/41/smp_affinity"
    printf '0-3\n' > "$R/opt/nspawn/irq/41/smp_affinity_list"
    # Keep the fake IRQ writable when the image remounts its root read-only.
    cp -a "$R/opt/nspawn/irq/." "$W/irq/"
    cat "$W/masked-units.txt"
    date +%s%N > "$W/start-ns"
    SYSTEMD_LOG_TARGET=console SYSTEMD_LOG_LEVEL=debug \
        timeout --signal=TERM --kill-after=5s 65s systemd-nspawn \
        --boot --directory="$R" --machine="nspawn-boot-$$" --register=no \
        --private-network --resolv-conf=off --link-journal=no --timezone=off \
        --drop-capability=CAP_SYS_TIME,CAP_SYS_MODULE,CAP_SYS_RAWIO,CAP_SYS_BOOT \
        --bind="$W/data:/data" --bind="$W/data:/opt/nspawn/data" \
        --bind="$W/irq:/opt/nspawn/irq" \
        --bind=/opt/nspawn/irq:/proc/irq \
        --bind-ro=/opt/nspawn/interrupts:/proc/interrupts \
        --bind-ro="$W/dt:/sys/firmware/devicetree/base" \
        --bind=/dev/null:/dev/ttyS0 --console=pipe \
        systemd.log_target=console systemd.log_color=0 systemd.show_status=1 \
        > "$W/console.log" 2>&1 &
    runner=$!
    local end=$((SECONDS + 60))
    until [ -n "$init_pid" ] || ! kill -0 "$runner" 2>/dev/null; do
        init_pid=$(sed -n 's/Init process invoked as PID //p' "$W/console.log" | tail -1)
        sleep 0.1
    done
    guest() { nsenter -t "$init_pid" -m -p -n -u -i --root --wd=/ "$@"; }
    until [ "$SECONDS" -ge "$end" ] || ! kill -0 "$runner" 2>/dev/null; do
        if [ -s "$W/data/enginex/ui/boot-ready" ] &&
            cmp -s "$W/data/enginex/ui/boot-token" "$W/data/enginex/ui/boot-ready" &&
            [ ! -e "$W/data/enginex/ui/safe-mode" ]; then
            date +%s%N > "$W/ready-ns"
            break
        fi
        sleep 0.2
    done
    # Keep observing startup until all boot jobs finish, including coldplug.
    # The wall time above is UI readiness, not this later verification point.
    while [ "$SECONDS" -lt "$end" ] && [ -e "/proc/$init_pid" ]; do
        guest /bin/systemctl is-system-running --quiet 2>/dev/null && break
        sleep 0.2
    done
    if [ -n "$init_pid" ] && [ -e "/proc/$init_pid" ]; then
        guest /bin/journalctl -b --no-pager > "$W/journal.log" || failed=1
        guest /bin/journalctl -u engine.service --no-pager > "$W/engine.log" || failed=1
        guest /bin/systemctl --failed --no-legend --plain --no-pager > "$W/failed-units.txt" || failed=1
        guest /bin/systemctl show sc5000-custom-prepare.service sc5000-custom-etc-reset.service -p ActiveState -p Result -p ExecStart > "$W/prepare.txt" || failed=1
    fi
    local applied expected seconds
    check() {
        local label=$1; shift
        if "$@" >> "$W/checks.log" 2>&1; then echo "PASS $label"; else echo "FAIL $label"; failed=1; fi
    }
    check 'UI ready' test -s "$W/ready-ns"
    check 'current boot token acknowledged' cmp -s "$W/data/enginex/ui/boot-token" "$W/data/enginex/ui/boot-ready"
    check 'no safe-mode fallback' test ! -e "$W/data/enginex/ui/safe-mode"
    check 'all systemd boot jobs completed' guest /bin/systemctl is-system-running --quiet
    local unit
    for unit in sc5000-custom-prepare.service stream-sync.service engine-guardian.service engine.service rb-bridge.service sc5000-deemixd.service; do
        check "$unit active" guest /bin/systemctl is-active --quiet "$unit"
    done
    check 'Engine process running' guest /usr/bin/pgrep -f '^/usr/Engine/Engine( |$)'
    # Evaluated by the guest shell, not this host shell.
    # shellcheck disable=SC2016
    check 'etc-reset executed successfully' guest /bin/sh -c 'test "$(systemctl show -p ExecMainStatus --value sc5000-custom-etc-reset.service)" = 0 && test "$(systemctl show -p ExecMainStartTimestampMonotonic --value sc5000-custom-etc-reset.service)" -gt 0'
    # Same model defaults and QML failure expressions as release-gate.sh.
    case $PRODUCT in JP07|JP08|JP13|JP14) expected=20 ;; *) expected=28 ;; esac
    applied=$(grep -ac 'engine-ui-hook: applied 0x' "$W/engine.log" || :)
    check "hook applied $applied/$expected (gate default)" test "$applied" -eq "$expected"
    check 'UI pack loaded' grep -aq 'engine-ui-hook: .*packs/.*: [0-9]* files' "$W/engine.log"
    local qml_errors='Type [A-Za-z_][A-Za-z0-9_]* unavailable|PropertyChanges does not support creating|state-specific objects|is not a type|is not installed|Cannot assign to non-existent|Expected token|Syntax error|QQmlComponent: Component is not ready|failed to load component|possible QQuickItem::polish\(\)|called polish\(\) inside updatePolish'
    local qml_status=0
    grep -aE "$qml_errors|Crash with|Unknown exit code" "$W/engine.log" > "$W/qml-errors.txt" || qml_status=$?
    check 'no QML load errors or Engine crashes' test "$qml_status" -eq 1
    check 'systemctl --failed empty' test ! -s "$W/failed-units.txt"
    if nsenter -t "$init_pid" -n curl --max-time 2 -sf http://127.0.0.1:8080/v1/sc5000/ui > "$W/ui.json"; then
        check '/v1/sc5000/ui answers JSON' python3 -I -c 'import json,sys; assert isinstance(json.load(open(sys.argv[1])),dict)' "$W/ui.json"
    else echo 'FAIL /v1/sc5000/ui'; failed=1; fi
    if [ -s "$W/ready-ns" ]; then
        seconds=$(python3 -I -c 'import sys; print("%.3f" % ((int(open(sys.argv[2]).read())-int(open(sys.argv[1]).read()))/1e9))' "$W/start-ns" "$W/ready-ns")
        echo "WALL TIME $PRODUCT ${seconds}s (nspawn start to Engine UI ready)"
        check 'UI ready under 60s' python3 -I -c 'import sys; assert float(sys.argv[1]) < 60' "$seconds"
    fi
    if [ "$failed" = 0 ]; then echo "PASS nspawn-boot $PRODUCT"; else
        tail -20 "$W/console.log"
        cat "$W/failed-units.txt" "$W/prepare.txt" "$W/checks.log" 2>/dev/null || :
    fi
    # Graceful container shutdown, bounded by the outer timeout.
    kill -TERM "$runner" 2>/dev/null || :
    wait "$runner" 2>/dev/null || :
    runner=
    return "$failed"
}

if [ "${1:-}" = --inner ]; then inner "$2"; exit; fi
if [ $# -lt 1 ] || [ $# -gt 2 ] || { [ $# = 2 ] && [ "$2" != --keep ]; }; then
    echo "usage: $0 <*-Update.img> [--keep]" >&2; exit 2
fi
[ "$(id -u)" = 0 ] || { echo 'run as root on the ARM build host' >&2; exit 2; }
case $(uname -m) in aarch64|arm*) ;; *) echo 'native ARM host required' >&2; exit 2 ;; esac
if grep -qs '^enabled' /proc/sys/fs/binfmt_misc/qemu-arm; then
    echo 'refusing qemu-arm binfmt: native execution required' >&2; exit 2
fi
command -v systemd-nspawn >/dev/null || { echo 'install systemd-container on the ARM build host' >&2; exit 2; }
IMAGE=$(realpath "$1")
[[ $(basename "$IMAGE") =~ -([A-Z]{2}[0-9]{2})-Update\.img$ ]] || { echo 'expected an AZ01 Update.img' >&2; exit 2; }
export PRODUCT=${BASH_REMATCH[1]}
W=$(mktemp -d /var/tmp/nspawn-boot.XXXXXX)
cleanup() { if [ "${2:-}" != --keep ]; then rm -rf "$W"; else echo "work directory: $W"; fi; }
trap 'cleanup "$@"' EXIT
trap 'echo "FAIL nspawn-boot $PRODUCT" >&2' ERR
echo "work directory: $W"
python3 -I "$REPO/tools/az01-packer/az01_packer.py" unpack "$IMAGE" --output "$W/image" > "$W/unpack.json"
cat "$W/unpack.json"
python3 -I -c 'import json,sys; assert json.load(open(sys.argv[1])).get("verified") is True' "$W/unpack.json"
cp --reflink=auto "$W/image/rootfs.ext2" "$W/boot.ext2"
mkdir "$W/root" "$W/data" "$W/dt" "$W/irq"
unshare --mount --propagation private "$SELF" --inner "$W" | tee "$W/results.txt"
