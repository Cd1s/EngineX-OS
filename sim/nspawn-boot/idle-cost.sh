# Sourced by boot.sh inside its private mount namespace when NSPAWN_IDLE_COST=<seconds>.
idle_cost_prepare() {
    mkdir -p "$W/data/enginex/ui"
    # A settings.conf to boot with, e.g. one that turns the four-deck layout on.
    [ -z "${NSPAWN_IDLE_SETTINGS:-}" ] || install -m 600 "$NSPAWN_IDLE_SETTINGS" "$W/data/enginex/ui/settings.conf"
    [ "${NSPAWN_IDLE_STOCK:-0}" = 1 ] || return 0
    # Baseline: the image's own Engine in safe mode (no hook, no UI pack) and none of our daemons.
    : > "$W/data/enginex/ui/safe-mode"
    # prepare clears safe-mode when it installs a pack version it has not seen.
    cp "$R/etc/sc5000/version.txt" "$W/data/enginex/ui/.pack-version"
    local unit
    for unit in stream-sync rb-bridge sc5000-deemixd engine-guardian sc5000-perf-tune sc5000-debloat sc5000-storage-discover; do
        ln -sf /dev/null "$R/etc/systemd/system/$unit.service"
    done
    rm -f "$R/etc/systemd/system/edisksd.service.d/50-sc5000-hook.conf"
}
idle_cost_observe() {
    local tool="$REPO/sim/nspawn-boot/idle_cost.py" lines
    # Boot work (thread sweeps at 20 and 40 s, first scans) is not idle cost.
    sleep "${NSPAWN_IDLE_SETTLE:-60}"
    lines=$(guest /bin/journalctl -b --no-pager | wc -l)
    python3 -I "$tool" snap "$init_pid" > "$W/idle-before.json"
    sleep "$NSPAWN_IDLE_COST"
    python3 -I "$tool" snap "$init_pid" > "$W/idle-after.json"
    guest /bin/journalctl -b --no-pager | tail -n "+$((lines + 1))" > "$W/idle-journal.log"
    python3 -I "$tool" diff "$W/idle-before.json" "$W/idle-after.json" | tee "$W/idle-cost.txt"
    echo "journal lines in the window: $(wc -l < "$W/idle-journal.log")"
}
