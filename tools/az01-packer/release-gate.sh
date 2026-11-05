#!/bin/bash
# EngineX OS release gate. Build host only, as root, never on a deck:
#   bash tools/az01-packer/release-gate.sh <previous release .img> <new .img> <new version> [work dir]
# e.g. SC5000-5.0.4.2-...-JP07-Update.img SC5000-5.3.0.1-...-JP07-Update.img 5.3.0.1
#
# On the real unpacked rootfs.ext2 of both images (unshare -m -n -p + chroot; armhf runs
# natively on an ARM host with 32-bit EL0, elsewhere through qemu-arm binfmt):
#  1. static audit of the new rootfs: unit dependencies, Exec* commands, tool
#     paths, shared libraries of every shipped ELF (rootfs_audit.py, ld.so --list);
#  2. the previous release boots (prepare, stream-sync, Engine), finds the new
#     release on a local TLS GitHub mock and runs the deck's online update:
#     /v1/sc5000/update?apply=1 -> Engine quits with UpdateFromUrl -> the stock
#     engine script -> az01-update downloads and verifies the new image;
#  3. the new rootfs boots on the same /data: etc-reset, prepare, debloat,
#     perf-tune, sshd -t, a stock Engine run that leaves a stale QML cache, then
#     stream-sync, deemixd, rb-bridge, guardian, storage, edisksd + hook, Engine +
#     UI hook (patch/DSP/QML counts, and gate-ui-probe.qml reading the rendered
#     Settings -> Services -> Streaming rows, About and Control Center), streaming
#     gateway, Stems handshake, Rekordbox conversion, fsck.hfsplus, and the
#     next online update again.
# Exit 0 only when every check passed. Evidence: <work dir>/results.txt and logs.
# Destructive deck paths are shimmed and logged, never run: kexec, systemctl,
# reboot, encrypt-fs.sh (fscrypt on /data), az01-update --erase-data. The
# GUI updater planck-remote-screen has no display here; its shim runs
# az01-update --net, which links the same libaz01-prog download/verify code.
set -euo pipefail
export LC_ALL=C
SELF=$(realpath "$0")
REPO=$(cd "$(dirname "$SELF")/../.." && pwd)
PATH_DECK=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

pass() { echo "PASS $*" | tee -a "$W/results.txt"; }
note() { echo "INFO $*" | tee -a "$W/results.txt"; }
fail() { echo "FAIL $*" | tee -a "$W/results.txt"; }
check() { # <name> <command...>: PASS when the command succeeds
    local name=$1; shift
    if "$@" >> "$W/checks.log" 2>&1; then pass "$name"; else fail "$name"; fi
}
json() { # <file> <python expression over j>: e.g. j["currentVersion"] == "5.3.0.1"
    python3 -I -c 'import json,sys; j=json.load(open(sys.argv[1])); sys.exit(0 if eval(sys.argv[2]) else 1)' "$1" "$2"
}
wait_for() { # <seconds> <command...>
    local end=$((SECONDS + $1)); shift
    until "$@" 2>/dev/null; do [ "$SECONDS" -lt "$end" ] || return 1; sleep 2; done
}
in_root() { chroot "$R" /usr/bin/env -i PATH=$PATH_DECK "$@"; }
unit_exec() { # ExecStart of a unit as the image ships it (last one wins)
    sed -n 's/^ExecStart=//p' "$R/etc/systemd/system/$1" "$R/usr/lib/systemd/system/$1" 2>/dev/null | tail -1
}

# --- phase: one boot of one rootfs, PID 1 of its own pid + mount namespace ---
phase() {
    local name=$1 R=$W/$1 v
    mount --make-rprivate /
    for d in tmp run dev sys; do mount -t tmpfs tmpfs "$R/$d"; done
    for n in null zero full urandom random; do touch "$R/dev/$n"; mount --bind "/dev/$n" "$R/dev/$n"; done
    mkdir -p "$R/dev/pts" "$R/dev/shm" "$R/run/gate" "$R/sys/firmware/devicetree/base" "$R/sys/devices/system/cpu"
    printf '%s' "$PRODUCT" > "$R/sys/firmware/devicetree/base/inmusic,product-code"
    printf '%s\0rockchip,rk3288\0' "$COMPAT" > "$R/sys/firmware/devicetree/base/compatible"
    for f in online possible present; do echo 0-3 > "$R/sys/devices/system/cpu/$f"; done
    mount --bind "$W/data" "$R/data"
    mount --bind "$W/hosts" "$R$(chroot "$R" /bin/readlink -f /etc/hosts)"
    local bundle; bundle=$R$(chroot "$R" /bin/readlink -f /etc/ssl/certs/ca-certificates.crt)
    cat "$bundle" "$W/tls/ca.pem" > "$W/$name-ca.crt"
    mount --bind "$W/$name-ca.crt" "$bundle"
    # Deck-destructive commands: record and succeed.
    cp "$R$(chroot "$R" /bin/readlink -f /usr/sbin/az01-update)" "$R/run/gate/az01-update.real"
    printf '#!/bin/sh\necho "SHIM $0 $*" >> /tmp/gate-shim.log\n' > "$R/run/gate/record"
    printf '#!/bin/sh\necho "SHIM $0 $*" >> /tmp/gate-shim.log\n[ "$1" = --update ] && exec /usr/sbin/az01-update --net "$2"\n' > "$R/run/gate/planck"
    cat > "$R/run/gate/az01-update" <<'SH'
#!/bin/sh
case " $* " in *" --erase-data "*) echo "SHIM refused $0 $*" >> /tmp/gate-shim.log; exit 1 ;; esac
{ /run/gate/az01-update.real "$@"; echo "rc=$?"; } 2>&1 | tee -a /tmp/gate-az01.log
rc=$(sed -n 's/^rc=//p' /tmp/gate-az01.log | tail -1); exit "${rc:-1}"
SH
    # qemu-user shows an emulated process as "<qemu> <path> <argv0> <args>" in
    # /proc/PID/cmdline, so the suite's "pkill -f ^/usr/Engine/Engine" would miss
    # Engine. This pkill, first in the deck's PATH, drops that prefix (none when native).
    mkdir -p "$R/usr/local/sbin"
    mount -t tmpfs tmpfs "$R/usr/local/sbin"
    cat > "$R/usr/local/sbin/pkill" <<SH
#!/bin/sh
[ "\$2" = -f ] || exec /usr/bin/pkill "\$@"
rc=1
for c in /proc/[0-9]*/cmdline; do
    pid=\${c#/proc/}; pid=\${pid%/cmdline}
    cmd=\$(tr '\\0' ' ' < "\$c" 2>/dev/null) || continue
    [ -z "$QEMU_INTERP" ] || case \$cmd in "$QEMU_INTERP "*) cmd=\${cmd#* }; cmd=\${cmd#* } ;; esac
    [ "\$pid" = \$\$ ] || ! printf '%s\\n' "\${cmd% }" | grep -Eq -- "\$3" || { kill "\$1" "\$pid" && rc=0; }
done
exit \$rc
SH
    chmod 755 "$R/run/gate/"* "$R/usr/local/sbin/pkill"
    local p
    for p in /usr/sbin/kexec /usr/bin/systemctl /sbin/reboot /usr/Engine/Scripts/encrypt-fs.sh; do
        p=$(chroot "$R" /bin/readlink -f "$p") && [ -e "$R$p" ] && mount --bind "$R/run/gate/record" "$R$p"
    done
    mount --bind "$R/run/gate/planck" "$R/usr/bin/planck-remote-screen"
    mount --bind "$R/run/gate/az01-update" "$R/usr/sbin/az01-update"

    # Boot scripts see a fake /proc and /sys: they tune IRQs and sysctls of the host otherwise.
    mount -t tmpfs tmpfs "$R/proc"
    mkdir -p "$R/proc/sys/net/core" "$R/proc/sys/net/ipv4" "$R/proc/sys/vm" "$R/proc/sys/fs" "$R/proc/irq"
    for k in net/core/rmem_max net/core/wmem_max net/ipv4/tcp_rmem net/ipv4/tcp_wmem vm/swappiness fs/file-max; do
        echo 0 > "$R/proc/sys/$k"
    done
    printf '           CPU0\n' > "$R/proc/interrupts"
    in_root /usr/libexec/sc5000-custom-etc-reset > "$W/$name-boot.log" 2>&1 || fail "$name: sc5000-custom-etc-reset"
    if in_root /usr/libexec/sc5000-custom-prepare >> "$W/$name-boot.log" 2>&1; then pass "$name: sc5000-custom-prepare"
    else fail "$name: sc5000-custom-prepare"; fi
    if [ "$name" = old ]; then
        printf 'lang=%s\n' "$LANG_PACK" > "$W/data/sc5000-custom/ui/settings.conf"
        check "old: version $OLDV installed" grep -qx "$OLDV" "$W/data/sc5000-custom/version.txt"
    else
        check "new: version.txt is $NEWV" grep -qx "$NEWV" "$W/data/sc5000-custom/version.txt"
        check "new: .pack-version is $NEWV" grep -qx "$NEWV" "$W/data/sc5000-custom/ui/.pack-version"
        check "new: SSH host key kept across the update" cmp -s "$W/old-hostkey.pub" "$W/data/sc5000-custom/ssh_host_ed25519_key.pub"
        check "new: no SSH key in a public image" test ! -s "$R/usr/share/sc5000-custom/authorized_keys"
        check "new: sc5000-debloat --apply-runtime" in_root /usr/local/bin/sc5000-debloat --apply-runtime
        check "new: sc5000-perf-tune" in_root SC5000_PROC_ROOT=/proc SC5000_SYS_ROOT=/sys /usr/local/bin/sc5000-perf-tune
        check "new: 8 MiB TCP buffers" grep -qx '4096 131072 8388608' "$R/proc/sys/net/ipv4/tcp_rmem"
        mkdir -p "$R/run/sshd"
        check "new: sshd -t" in_root /usr/sbin/sshd -t -f /usr/share/sc5000-custom/sshd_config
    fi
    umount "$R/proc"
    mount -t proc proc "$R/proc"
    # Engine aborts without the deck's serial IRQ ("No IRQ matching 'ttyS0'").
    printf '           CPU0\n 41:        100     GICv2  32 Level     ttyS0\n' > "$R/run/gate/interrupts"
    mount --bind "$R/run/gate/interrupts" "$R/proc/interrupts"
    # Engine then pins that IRQ to CPU 0. Without this it wrote the build host's own
    # IRQ 41, and aborts where the host has no such IRQ (native ARM hosts).
    mount -t tmpfs tmpfs "$R/proc/irq"
    mkdir "$R/proc/irq/41"
    printf 'f\n' > "$R/proc/irq/41/smp_affinity"
    printf '0-3\n' > "$R/proc/irq/41/smp_affinity_list"

    mkdir -p "$R/run/dbus"
    chroot "$R" /usr/bin/dbus-daemon --system --fork --nopidfile >> "$W/$name-boot.log" 2>&1 || chroot "$R" /usr/bin/dbus-daemon --system --fork
    declare -A pid
    # shellcheck disable=SC2046
    in_root $(unit_exec stream-sync.service) > "$W/$name-stream-sync.log" 2>&1 & pid[stream-sync]=$!
    if [ "$name" = new ]; then
        in_root $(unit_exec sc5000-deemixd.service) > "$W/new-deemixd.log" 2>&1 & pid[deemixd]=$!
        in_root $(unit_exec rb-bridge.service) > "$W/new-rb-bridge.log" 2>&1 & pid[rb-bridge]=$!
        # No udev here: edisksd quits after "Failed to get initial devices". Binding every
        # symbol of the preloaded hook at start is the check; the mount path itself is
        # tools/edisksd-hook/test_edisksd_hook.c --mounts (step 2).
        in_root LD_BIND_NOW=1 LD_PRELOAD=/etc/sc5000/libedisksd-hook.so $(unit_exec edisksd.service) > "$W/new-edisksd.log" 2>&1 &
        check "new: engine-guardian --watch (retired) exits 0" in_root $(unit_exec engine-guardian.service)
        check "new: sc5000-storage-mount --discover" in_root $(unit_exec sc5000-storage-discover.service)
    fi
    # engine.service: ExecStartPre (guard), runengine's environment, the stock engine script.
    mkdir -p -m 0700 "$R/tmp/sc5000-engine-home"
    local ui=$W/data/sc5000-custom/ui want=0
    local engine_env=(HOME=/tmp/sc5000-engine-home MESA_SHADER_CACHE_MAX_SIZE=32M QML_DISABLE_DISK_CACHE=1
        LD_PRELOAD=/etc/sc5000/libengine-ui-hook.so QT_QPA_PLATFORM=offscreen MALLOC_ARENA_MAX=1 LD_LIBRARY_PATH=/usr/qt/lib)
    if [ "$name" = new ]; then
        # The 5.3.0.1 deck kept the stock UI (2026-11-05): Engine 5.3.0 once ran without the
        # pack, and its forced QML disk cache in /data outlived every later boot with it.
        # Seed that cache with one stock run (safe-mode), then boot as a deck would.
        : > "$ui/safe-mode"
        in_root "${engine_env[@]}" /usr/Engine/Engine -d0 > "$W/new-stock-engine.log" 2>&1 &
        local stock=$! qmlc
        qmlc=$(printf '%s' :/Skins/Embedded/Views/ControlCenter/Tabs/Preferences/Entries/StreamingServices.qml | sha1sum | cut -c1-40).qmlc
        check "new: a stock Engine run cached StreamingServices.qml in /data (stale-cache seed)" \
            wait_for "$ENGINE_START" bash -c "find '$W/data' -name '$qmlc' | grep -q ."
        sleep 20  # let it finish writing the rest of the cache
        in_root /usr/local/sbin/pkill -TERM -f '^/usr/Engine/Engine( |$)' || :
        (sleep 60; in_root /usr/local/sbin/pkill -KILL -f '^/usr/Engine/Engine( |$)') & local reaper=$!
        wait "$stock" 2>/dev/null || :
        kill "$reaper" 2>/dev/null || :
        note "new: stale QML cache seeded: $(find "$W/data" -name '*.qmlc' | wc -l) .qmlc files"
        rm -f "$ui/safe-mode" "$ui/boot-count" "$ui/boot-token" "$R/tmp/engine_runguard.lock"
        # The probe stands in for a stock file Engine always creates; it is not in the image.
        want=$(find "$ui/packs/$LANG_PACK" -type f | wc -l)
        mkdir -p "$ui/packs/$LANG_PACK/Skins/Common/Items"
        cp "$REPO/tools/az01-packer/gate-ui-probe.qml" "$ui/packs/$LANG_PACK/Skins/Common/Items/QmlWarnings.qml"
    fi
    in_root /etc/sc5000/engine-ui-guard pre >> "$W/$name-boot.log" 2>&1 || :
    # #56: without a TIDAL account Engine 5.3.0 opens the Engine DJ Profile login
    # (device.enginedj.com) instead of logging in to the local gateway.
    [ "$name" = old ] || check "new: engine-ui-guard pre left Engine a TIDAL account on a fresh /data (#56)" \
        python3 -I -c 'import json,sys; a=json.load(open(sys.argv[1])); sys.exit(json.loads(a["TIDAL"])["providerType"] != "TIDAL")' "$W/data/StreamingAccounts.json"
    in_root "${engine_env[@]}" /usr/Engine/Scripts/engine "$PRODUCT" > "$W/$name-engine.log" 2>&1 & pid[engine]=$!
    check "$name: stream-sync listens on :8080" wait_for 120 curl -sf -o /dev/null http://127.0.0.1:8080/v1/sc5000/ui
    check "$name: UI hook loaded the $LANG_PACK pack" wait_for "$ENGINE_START" grep -aq "packs/$LANG_PACK: [0-9]* files" "$W/$name-engine.log"
    in_root /etc/sc5000/engine-ui-guard post >> "$W/$name-boot.log" 2>&1 || :

    # Engine removes a stale /tmp/update-file-url while it starts: a deck owner can only
    # press Update once it is up, so the gate waits the same way before ?apply=1.
    sleep "$ENGINE_SECONDS"
    if [ "$name" = new ]; then
        local log=$W/new-engine.log files label
        files=$(sed -n "s|.*packs/$LANG_PACK: \([0-9]*\) files.*|\1|p" "$log" | awk '{s += $1} END {print s + 0}')
        check "new: UI hook replaced all $want pack files + the gate probe (got ${files:-0})" test "${files:-0}" -eq $((want + 1)) -a "$want" -gt 0
        # What Engine rendered, not what the hook counted: a stale .qmlc showed the stock UI
        # while the hook still logged every file.
        check "new: render probe reported (Settings -> Services, About)" wait_for "$ENGINE_START" grep -aq 'SCPROBE done|' "$log"
        check "new: render probe ran without errors" bash -c "! grep -aq 'SCPROBE error|' '$log'"
        label=$(grep -ao 'SCPROBE entry|[^|]*|Deezer|1|' "$log" | head -1 | cut -d'|' -f2) || :
        check "new: Settings -> Services -> Streaming shows the pack's Deezer row" test -n "$label"
        check "new: Streaming shows SoundCloud once (stock row hidden)" test "$(grep -acF "SCPROBE entry|$label|SoundCloud|1|" "$log")" -eq 1
        check "new: Streaming shows Beatport once (stock row hidden)" test "$(grep -acF "SCPROBE entry|$label|Beatport|1|" "$log")" -eq 1
        check "new: Streaming hides stock Tidal" bash -c "! grep -aqF 'SCPROBE entry|$label|Tidal|1|' '$log'"
        check "new: About shows EngineX OS $NEWV" grep -aqF "SCPROBE version|EngineX OS $NEWV " "$log"
        check "new: Control Center has the waveform tile" grep -aqF 'SCPROBE tile|ControlCenter_WaveformStyle|' "$log"
        check "new: Control Center has the 4-Deck Waveform tile (#49)" grep -aqF 'SCPROBE tile|ControlCenter_FourDeckWaveform|' "$log"
        check "new: Control Center has the Live Stream tile (#50)" grep -aqF 'SCPROBE tile|ControlCenter_LiveStream|' "$log"
        check "new: Control Center has the upfader echo tile (#52)" grep -aqF 'SCPROBE tile|ControlCenter_UpfaderEcho|' "$log"
        check "new: 4-Deck remote waveform strip is loaded (#49)" grep -aqF 'SCPROBE tile|FourDeckRemoteStrip|' "$log"
        check "new: auto-analyze toggle and Engine's AnalyzeUnanalyzed action exist (#55)" grep -aqF 'SCPROBE analyze|boolean|1|' "$log"
        note "new: render probe: $(grep -ao 'SCPROBE [a-z]*|.*|' "$log" | grep -v 'SCPROBE done' | tr '\n' ' ')"
        check "new: Engine build matched (Engine $BASE)" grep -aq "engine-ui-hook: Engine $BASE" "$log"
        check "new: $EXPECT_APPLIED Engine patches applied (TidalAPI gateway, Stems, getConfig...)" \
            test "$(grep -ac 'engine-ui-hook: applied 0x' "$log")" -eq "$EXPECT_APPLIED"
        check "new: $EXPECT_DSP DSP guards installed" grep -aq "engine-ui-hook: $EXPECT_DSP DSP guards/transitions installed" "$log"
        check "new: SQLite hooks installed" grep -aq 'engine-ui-hook: SQLite open/open_v2 hooks installed' "$log"
        check "new: no QML load errors" bash -c "! grep -aE 'is not a type|is not installed|Cannot assign to non-existent|Expected token|Syntax error|QQmlComponent: Component is not ready|failed to load component' '$log'"
        check "new: no Engine crash" bash -c "! grep -aE 'Crash with|Unknown exit code' '$log'"
        for v in "${!pid[@]}"; do check "new: $v still running after ${ENGINE_SECONDS}s" kill -0 "${pid[$v]}"; done
        # Field 41 of /proc/PID/stat is the policy; 5 is SCHED_IDLE.
        check "new: OfflineAnalyzer runs at idle priority (#55)" bash -c "chroot '$R' /bin/sh -c 'cat /proc/[0-9]*/stat 2>/dev/null' | awk '\$2 == \"(OfflineAnalyzer)\" { n++; if (\$41 != 5) bad = 1 } END { exit !(n && !bad) }'"
        check "new: edisksd starts with the hook preloaded (LD_BIND_NOW)" bash -c "grep -aq 'Loaded configuration' '$W/new-edisksd.log' && ! grep -aiE 'symbol lookup error|undefined symbol|cannot open shared object|cannot be preloaded' '$W/new-edisksd.log'"
        curl -sf http://127.0.0.1:8080/v1/sc5000/ui > "$W/new-ui.json" || :
        check "new: GET /v1/sc5000/ui reports $NEWV" json "$W/new-ui.json" "j.get('version') == '$NEWV'"
        curl -sf -X POST "http://127.0.0.1:8080/v1/sc5000/ui?upfaderEcho=1" > "$W/new-ui-echo.json" || :
        check "new: POST /v1/sc5000/ui?upfaderEcho=1 returns upfaderEcho: true" json "$W/new-ui-echo.json" "j.get('upfaderEcho') is True"
        curl -sf -X POST http://127.0.0.1:8080/au/oauth2/token > "$W/new-token.json" || :
        check "new: POST /au/oauth2/token (TIDAL gateway)" json "$W/new-token.json" "j.get('access_token')"
        check "new: Beatport is one of the deck's sources" json "$W/new-ui.json" "'beatport' in j.get('sources', [])"
        curl -sf -X POST "http://127.0.0.1:8080/v1/sc5000/login?provider=beatport" > "$W/new-beatport-login.json" || :
        check "new: Beatport login flow starts" json "$W/new-beatport-login.json" "j.get('provider') == 'beatport' and '/m/beatport?t=' in j.get('target', '')"
        local phone
        phone=$(python3 -I -c 'import json,sys,urllib.parse as u; p=u.urlsplit(json.load(open(sys.argv[1]))["target"]); print(p.path + "?" + p.query)' "$W/new-beatport-login.json" 2>/dev/null || :)
        check "new: Beatport phone login page" bash -c "curl -sf 'http://127.0.0.1:8080$phone' | grep -q 'Beatport'"
        check "new: UI pack carries the Beatport source" grep -rqs 'id: "beatport"' "$W/data/sc5000-custom/ui/packs/$LANG_PACK"
        curl -sf http://127.0.0.1:6595/healthz > "$W/new-deemixd.json" || :
        check "new: sc5000-deemixd /healthz" json "$W/new-deemixd.json" "j.get('ok') is True"
        curl -sf -X POST -d '{"number":1,"name":"CDJ-3000","track_id":77,"bpm":128.0,"playing":true,"title":"Prolink Gate"}' http://127.0.0.1:8080/v1/sc5000/prolink > "$W/new-prolink.json" || :
        check "new: Pro DJ Link loopback API syncs CDJ-3000 to Deck 3 (#51)" json "$W/new-prolink.json" "j.get('enabled') is True and j.get('decks', {}).get('3', {}).get('title') == 'Prolink Gate'"
        curl -sf -X POST "http://127.0.0.1:8080/v1/sc5000/broadcast/metadata?artist=Denon&title=Prime" > "$W/new-broadcast.json" || :
        check "new: Icecast/Shoutcast broadcast API updates live track metadata (#50)" json "$W/new-broadcast.json" "j.get('currentTrack') == 'Denon - Prime'"
        cp "$W/bin/stems-probe" "$R/tmp/stems-probe"
        check "new: Stems handshake and render (stems-probe -> stems-processor)" \
            timeout 600 chroot "$R" /usr/bin/env -i PATH=$PATH_DECK HOME=/tmp /tmp/stems-probe -seconds 3 /usr/local/bin/stems-processor
        cp -a "$W/usb" "$R/tmp/usb"
        check "new: rb-bridge converts a Rekordbox export" in_root /usr/local/bin/rb-bridge --mount-path /tmp/usb
        check "new: Engine Library written with tracks" python3 -I -c 'import sqlite3,sys; sys.exit(0 if sqlite3.connect(sys.argv[1]).execute("select count(*) from Track").fetchone()[0] > 0 else 1)' "$R/tmp/usb/Engine Library/Database2/m.db"
        check "new: fsck.hfsplus runs" bash -c "chroot '$R' /usr/sbin/fsck.hfsplus 2>&1 | grep -qi usage"
    fi
    # The online update, exactly as Settings -> Update does it.
    local cur=$OLDV tag=v$NEWV asset
    [ "$name" = old ] || { cur=$NEWV; tag=v$NEXTV; }
    asset=$(cat "$W/mock/asset-$name")
    curl -sf http://127.0.0.1:8080/v1/sc5000/update > "$W/$name-update.json" || :
    check "$name: /v1/sc5000/update offers $tag over $cur" json "$W/$name-update.json" \
        "j.get('currentVersion') == '$cur' and j.get('latestVersion') == '$tag' and j.get('updateAvailable') is True and j.get('downloadUrl') == 'https://github.com/Cd1s/EngineX-OS/releases/download/$tag/$asset'"
    check "$name: Engine still running before the update" bash -c "! grep -aqE 'Crash with|SOFT RESET' '$W/$name-engine.log'"
    : > "$R/tmp/gate-shim.log"
    curl -sf "http://127.0.0.1:8080/v1/sc5000/update?apply=1" > "$W/$name-apply.json" || :
    check "$name: Engine quit with UpdateFromUrl" wait_for 120 grep -aq "Starting update from url: https://github.com/Cd1s/EngineX-OS/releases/download/$tag/" "$W/$name-engine.log"
    check "$name: the engine script handed the URL to the updater" wait_for 60 grep -qF "SHIM /usr/bin/planck-remote-screen --update https://github.com/Cd1s/EngineX-OS/releases/download/$tag/$asset" "$R/tmp/gate-shim.log"
    # libaz01-prog only starts "Applying" after the image verified; flashing itself
    # needs the deck's eMMC (the sandbox has none) and is checked on a real deck.
    wait_for "$UPDATE_TIMEOUT" grep -q '^rc=' "$R/tmp/gate-az01.log" || :
    check "$name: az01-update downloaded and verified the image" \
        grep -aq "Applying https://github.com/Cd1s/EngineX-OS/releases/download/$tag/$asset" "$R/tmp/gate-az01.log"
    check "$name: no download, signature or compatibility error" \
        bash -c "! grep -aiE 'verification failed|not compatible|no valid signature|download error|failed to download' '$R/tmp/gate-az01.log'"
    note "$name: sandbox apply step: $(grep -a 'failed to update\|^rc=' "$R/tmp/gate-az01.log" | tr '\n' ' ')"
    if [ "$name" = old ]; then
        # Control: the same download + verify path must reject the image with one byte flipped.
        cp "$R/tmp/gate-az01.log" "$W/old-az01.log"
        : > "$R/tmp/gate-az01.log"
        python3 -I -c 'import shutil,sys; shutil.copyfile(sys.argv[1], sys.argv[2]); f=open(sys.argv[2], "r+b"); n=f.seek(0, 2)//2; f.seek(n); b=f.read(1); f.seek(n); f.write(bytes([b[0]^0xff]))' \
            "$NEW_IMG" "$W/mock/assets/gate-corrupt-Update.img"
        in_root /usr/sbin/az01-update --net "https://github.com/Cd1s/EngineX-OS/releases/download/$tag/gate-corrupt-Update.img" > /dev/null 2>&1 || :
        check "old: a one-byte corrupted image is rejected before Applying" bash -c "grep -aq Verifying '$R/tmp/gate-az01.log' && ! grep -aq Applying '$R/tmp/gate-az01.log'"
        note "old: corrupted image: $(tr '\r' '\n' < "$R/tmp/gate-az01.log" | grep -av '^Downloading' | tr '\n' ' ')"
        rm -f "$W/mock/assets/gate-corrupt-Update.img"
        cp "$R/tmp/gate-az01.log" "$W/old-corrupt-az01.log"
        cp "$W/old-az01.log" "$R/tmp/gate-az01.log"
    fi
    cp "$R/tmp/gate-shim.log" "$W/$name-shim.log" 2>/dev/null || :
    cp "$R/tmp/gate-az01.log" "$W/$name-az01.log" 2>/dev/null || :
    [ "$name" = new ] || cp "$W/data/sc5000-custom/ssh_host_ed25519_key.pub" "$W/old-hostkey.pub"
    # The update ramdisk consumes the staged image; the next boot never sees it.
    rm -f "$W/data/az01-update.img" "$W/data/az01-update.img.url"
}

# --- inner: private mount + network namespace around both phases ---
inner() {
    mount --make-rprivate /
    ip link set lo up
    mkdir -p "$W/old" "$W/new" "$W/data"
    mount -o loop "$W/old-img/rootfs.ext2" "$W/old"
    mount -o loop "$W/new-img/rootfs.ext2" "$W/new"
    python3 -I "$W/mock/server.py" "$W/mock" "$W/tls/leaf.pem" "$W/tls/leaf.key" 2> "$W/mock.log" &
    # shellcheck disable=SC2064
    trap "kill $! 2>/dev/null" EXIT  # the netns and loop mounts go when the last process does
    wait_for 30 curl -sf --cacert "$W/tls/ca.pem" --resolve api.github.com:443:127.0.0.1 \
        https://api.github.com/repos/Cd1s/EngineX-OS/releases/latest -o /dev/null || { fail "TLS GitHub mock"; return; }

    local R=$W/new interp f
    check "image: /etc/sc5000/version.txt is $NEWV" grep -qx "$NEWV" "$R/etc/sc5000/version.txt"
    if python3 -I "$REPO/tools/az01-packer/rootfs_audit.py" "$R" --overlay "$REPO/rootfs-overlay" \
        $(for c in $SUITE_CMDS; do printf -- '--require-cmd %s ' "$c"; done) > "$W/audit.txt" 2>&1
    then pass "image: unit/command/tool audit ($(tail -1 "$W/audit.txt"))"
    else fail "image: unit/command/tool audit, see audit.txt"; grep '^FAIL' "$W/audit.txt" || :; fi
    # The host's own systemd resolves the same transactions (it printed
    # "Unit var.mount not found" for the first 5.3.0.1 build).
    local units='' u
    for u in $(cd "$REPO/rootfs-overlay/etc/systemd/system" && ls -d *.service *.target *.d) $(cd "$R/usr/lib/systemd/system" && ls sc5000-custom*); do
        u=${u%.d}; u=${u/@./@gate.}
        [ -e "$R/usr/lib/systemd/system/${u/@gate./@.}" ] || [ -e "$R/etc/systemd/system/${u/@gate./@.}" ] && units="$units $u"
    done
    # shellcheck disable=SC2086
    systemd-analyze verify --root="$R" --man=no $units > "$W/systemd-verify.txt" 2>&1 || :
    check "image: systemd-analyze verify of $(echo $units | wc -w) units is clean" test ! -s "$W/systemd-verify.txt"
    interp=$(readelf -l "$R/usr/sbin/az01-update" | sed -n 's/.*interpreter: \(.*\)]/\1/p')
    while IFS= read -r f; do
        readelf -h "$R$f" >/dev/null 2>&1 || continue
        readelf -l "$R$f" 2>/dev/null | grep -q 'INTERP\|DYN' || continue
        if chroot "$R" "$interp" --list "$f" > "$W/ldd.txt" 2>&1 && ! grep -q 'not found' "$W/ldd.txt"; then pass "image: libraries of $f"
        else fail "image: libraries of $f"; cat "$W/ldd.txt"; fi
    done < <(cd "$REPO/rootfs-overlay" && find . -type f | sed 's|^\.||'; echo /usr/sbin/fsck.hfsplus)
    check "image: engine script no longer forces 100 Mbps" bash -c "! grep -Eq 'az01-ethernet-speed[[:space:]]+100([^0-9]|\$)' '$R/usr/Engine/Scripts/engine'"
    for f in "$R"/usr/Engine/Scripts/*.sh "$R/usr/Engine/Scripts/engine" "$R/usr/Engine/Scripts/runengine"; do
        check "image: sh -n ${f#"$R"}" chroot "$R" /bin/sh -n "${f#"$R"}"
    done

    unshare -m -p -f --kill-child "$SELF" --phase old || fail "old phase aborted"
    cp "$W/mock/release-new.json" "$W/mock/release.json"
    unshare -m -p -f --kill-child "$SELF" --phase new || fail "new phase aborted"
}

case ${1:-} in
--inner) inner; exit 0 ;;
--phase) phase "$2"; exit 0 ;;
esac

[ $# -ge 3 ] || { sed -n '2,4p' "$SELF" >&2; exit 2; }
[ "$(id -u)" = 0 ] || { echo "run as root on the build host" >&2; exit 2; }
# A registered qemu-arm handler wins over native execution, so only its absence means native.
QEMU_INTERP=
if grep -qs '^enabled' /proc/sys/fs/binfmt_misc/qemu-arm; then
    grep -qs '^flags:.*F' /proc/sys/fs/binfmt_misc/qemu-arm || { echo "qemu-arm binfmt needs the F flag" >&2; exit 2; }
    QEMU_INTERP=$(sed -n 's/^interpreter //p' /proc/sys/fs/binfmt_misc/qemu-arm)
else
    case $(uname -m) in aarch64|arm*) ;; *) echo "needs an ARM host or qemu-arm binfmt with the F flag" >&2; exit 2 ;; esac
fi
for t in python3 openssl curl unshare chroot ip readelf realpath systemd-analyze; do command -v $t >/dev/null || { echo "missing $t" >&2; exit 2; }; done
OLD_IMG=$(realpath "$1"); NEW_IMG=$(realpath "$2"); NEWV=$3
W=$(realpath -m "${4:-$(mktemp -d /var/tmp/sc5000-gate.XXXXXX)}")
[ ! -e "$W/results.txt" ] || { echo "$W was used before; give a new work dir" >&2; exit 2; }
NEW_NAME=$(basename "$NEW_IMG"); OLD_NAME=$(basename "$OLD_IMG")
[[ $NEW_NAME =~ ^([A-Z0-9]+)-$NEWV-EngineX-OS-(.*)-([A-Z]{2}[0-9]{2})-Update\.img$ ]] || { echo "unexpected new image name $NEW_NAME" >&2; exit 2; }
PREFIX=${BASH_REMATCH[1]} MODEL=${BASH_REMATCH[2]} PRODUCT=${BASH_REMATCH[3]}
[[ $OLD_NAME =~ ^$PREFIX-([0-9.]+)-EngineX-OS-.*-$PRODUCT-Update\.img$ ]] || { echo "old image must be the same model: $OLD_NAME" >&2; exit 2; }
OLDV=${BASH_REMATCH[1]}
NEXTV=${NEWV%.*}.$(( ${NEWV##*.} + 1 ))
COMPAT=inmusic,$(echo "$PRODUCT" | tr 'A-Z' 'a-z')
export W REPO OLDV NEWV NEXTV PREFIX PRODUCT COMPAT QEMU_INTERP NEW_IMG
export LANG_PACK=${GATE_LANG:-zh-Hans} ENGINE_START=${GATE_ENGINE_START:-300} ENGINE_SECONDS=${GATE_ENGINE_SECONDS:-90}
export UPDATE_TIMEOUT=${GATE_UPDATE_TIMEOUT:-900} EXPECT_APPLIED=${GATE_EXPECT_APPLIED:-16} EXPECT_DSP=${GATE_EXPECT_DSP:-21}
mkdir -p "$W/bin" "$W/tls" "$W/mock/assets"
: > "$W/results.txt"
echo "gate: $OLD_NAME -> $NEW_NAME (next $NEXTV), work $W"

for side in old new; do
    img=$OLD_IMG; [ $side = old ] || img=$NEW_IMG
    python3 -I "$REPO/tools/az01-packer/az01_packer.py" unpack "$img" --output "$W/$side-img" > "$W/$side-unpack.json"
    check "$side image unpacks and verifies" json "$W/$side-unpack.json" "j.get('verified') is True"
done
export BASE=$(cut -d. -f1-3 <<< "$NEWV")  # the official Engine the image is built on, as the UI hook names it (5.3.0 for 5.3.0.1.1)
# Tools the suite runs directly (exec.Command); a base without them breaks that feature.
SUITE_CMDS=$(cd "$REPO" && grep -rhoE 'exec\.Command(Context)?\((ctx, )?"[^"]+"' --include='*.go' --exclude='*_test.go' cmd internal pkg | sed -E 's/.*"([^"]+)"$/\1/' | sort -u | tr '\n' ' ')
export SUITE_CMDS

# Host-side helpers: an armhf stems-probe and the Rekordbox fixture.
[ -r /etc/profile.d/buildtools.sh ] && . /etc/profile.d/buildtools.sh
(cd "$REPO" && GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -trimpath -o "$W/bin/stems-probe" ./cmd/stems-probe)
(cd "$REPO" && SC5000_GATE_EXPORT=$W/usb go test -count=1 -run '^TestGateExport$' ./internal/testfixture >/dev/null)

# A throwaway CA for the GitHub mock; only the sandboxes trust it.
(cd "$W/tls" &&
 openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj /CN=sc5000-gate-ca -keyout ca.key -out ca.pem \
     -addext basicConstraints=critical,CA:true -addext keyUsage=critical,keyCertSign,cRLSign 2>/dev/null &&
 openssl req -newkey rsa:2048 -nodes -subj /CN=github.com -keyout leaf.key -out leaf.csr 2>/dev/null &&
 printf 'subjectAltName=DNS:github.com,DNS:api.github.com\nextendedKeyUsage=serverAuth\n' > leaf.ext &&
 openssl x509 -req -in leaf.csr -CA ca.pem -CAkey ca.key -CAcreateserial -days 2 -out leaf.pem -extfile leaf.ext 2>/dev/null)
printf '127.0.0.1 localhost\n127.0.0.1 api.github.com github.com\n' > "$W/hosts"
release() { # <file> <tag> <asset name> <image>: GitHub's releases/latest answer
    ln -sf "$4" "$W/mock/assets/$3"
    printf '{"tag_name":"%s","html_url":"https://github.com/Cd1s/EngineX-OS/releases/tag/%s","assets":[{"name":"%s","browser_download_url":"https://github.com/Cd1s/EngineX-OS/releases/download/%s/%s"}]}\n' \
        "$2" "$2" "$3" "$2" "$3" > "$1"
}
release "$W/mock/release-old.json" "v$NEWV" "$NEW_NAME" "$NEW_IMG"
echo "$NEW_NAME" > "$W/mock/asset-old"
cp "$W/mock/release-old.json" "$W/mock/release.json"
NEXT_NAME=$PREFIX-$NEXTV-EngineX-OS-$MODEL-$PRODUCT-Update.img
release "$W/mock/release-new.json" "v$NEXTV" "$NEXT_NAME" "$NEW_IMG"
echo "$NEXT_NAME" > "$W/mock/asset-new"
cat > "$W/mock/server.py" <<'PY'
import http.server, os, ssl, sys
root = sys.argv[1]
class H(http.server.BaseHTTPRequestHandler):
    def reply(self, body):
        if self.path.endswith('/releases/latest'):
            data = open(os.path.join(root, 'release.json'), 'rb').read()
            self.send_response(200); self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(data))); self.end_headers()
            if body: self.wfile.write(data)
            return
        path = os.path.join(root, 'assets', os.path.basename(self.path.split('?')[0]))
        if '/releases/download/' not in self.path or not os.path.isfile(path):
            return self.send_error(404)
        self.send_response(200); self.send_header('Content-Type', 'application/octet-stream')
        self.send_header('Content-Length', str(os.path.getsize(path))); self.end_headers()
        if body:
            with open(path, 'rb') as f:
                while chunk := f.read(1 << 20):
                    self.wfile.write(chunk)
    def do_GET(self): self.reply(True)
    def do_HEAD(self): self.reply(False)
    def log_message(self, fmt, *args): sys.stderr.write('mock: ' + fmt % args + '\n')
srv = http.server.ThreadingHTTPServer(('127.0.0.1', 443), H)
ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
ctx.load_cert_chain(sys.argv[2], sys.argv[3])
srv.socket = ctx.wrap_socket(srv.socket, server_side=True)
srv.serve_forever()
PY

unshare -m -n --kill-child "$SELF" --inner || fail "inner namespace aborted"
n=$(grep -c '^FAIL' "$W/results.txt" || :)
echo "gate: $(grep -c '^PASS' "$W/results.txt") PASS, $n FAIL ($W/results.txt)"
[ "$n" = 0 ]
