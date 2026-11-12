#!/usr/bin/env python3
"""Pack the EngineX OS user-space suite into a /data bundle (#61).

Devices whose root partition stays read-only (SC LIVE 4, inmusic,jp21) cannot
take an EngineX OS .img; they install this tarball under /data/enginex:

    tar -xzf SCLIVE4-<V>-EngineX-OS-Data-Bundle.tar.gz -C /tmp
    sh /tmp/SCLIVE4-<V>-EngineX-OS-Data-Bundle/install.sh

Run on the build host after build-release.sh's suite and UI-pack steps:

    build-data-bundle.py <version> --suite sc5000-suite --hook libengine-ui-hook.so \
        --ui-packs packs.tar.gz --out <new dir>
"""
import argparse
import gzip
import hashlib
import io
import json
import re
import sys
import tarfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from az01_packer import MIGRATE  # one migration snippet for the image and the bundle

# SC5000_CUSTOM is only set by tests: then dest is not the deck's /data, nothing to migrate.
MIGRATE_IF_DEFAULT = 'if [ -z "${SC5000_CUSTOM:-}" ]; then\n' + MIGRATE + '\nfi'

VERSION_RE = re.compile(r'^\d+(\.\d+){2,4}$')  # same range as az01_packer.py
APPLETS = ('stream-sync', 'rb-bridge', 'sc5000-deemixd', 'stems-processor', 'engine-guardian')

WRAPPER = '#!/bin/sh\nexec "$(dirname "$0")/sc5000-suite" {} "$@"\n'

BOOT_ACTIVATE = r'''#!/bin/sh
# One-shot boot activator for SC LIVE 4 (inmusic,jp21) and read-only root decks (#61).
set -eu

@MIGRATE_IF_DEFAULT@
dest="${SC5000_CUSTOM:-/data/enginex}"
bundle_dir="$dest/bundle"
bin_dir="$bundle_dir/bin"
lib_hook="$bundle_dir/lib/libengine-ui-hook.so"
lib_edisksd_hook="$bundle_dir/lib/libedisksd-hook.so"

check_gen2_tz_newer() {
    tz_file="$dest/state/timezone"
    [ -f "$tz_file" ] || return 1
    cfg_root=${SC5000_CONFIG_DIR:-"$(dirname "$dest")/.config"}
    for d in "$cfg_root"/*.user.settings; do
        [ -d "$d" ] || continue
        conf="$d/AIR Music Technology/Engine.conf"
        if [ ! -f "$conf" ] || [ "$tz_file" -nt "$conf" ]; then
            return 0
        fi
    done
    return 1
}

restore_gen2_timezone() {
    force_newer=${1:-0}
    tz_file="$dest/state/timezone"
    [ ! -L "$dest/state" ] && [ ! -L "$tz_file" ] && [ -f "$tz_file" ] || return 0
    tz=$(awk '{gsub(/\r/, ""); gsub(/^[[:space:]]+|[[:space:]]+$/, ""); gsub(/ /, "_"); print; exit}' "$tz_file")
    zdir=${SC5000_ZONEINFO_DIR:-/usr/share/zoneinfo}
    if [ "$tz" = UTC ]; then
        [ -f "$zdir/UTC" ] || return 0
    else
        case $tz in
            ''|/*|*/|*//*|*..*|*[![:alnum:]_+/-]*) return 0 ;;
            */*) ;;
            *) return 0 ;;
        esac
        [ "${#tz}" -le 63 ] || return 0
        old_ifs=$IFS; IFS=/
        for seg in $tz; do
            IFS=$old_ifs
            [ -n "$seg" ] && [ "$seg" != "." ] && [ "${#seg}" -le 32 ] || return 0
        done
        IFS=$old_ifs
        [ -f "$zdir/$tz" ] || return 0
    fi
    cfg_root=${SC5000_CONFIG_DIR:-"$(dirname "$dest")/.config"}
    [ ! -L "$cfg_root" ] || return 0
    read_conf_tz() {
        [ -f "$1" ] || return 0
        awk '
            /^\[TimeDateSettings\][[:space:]]*$/ { in_sec = 1; next }
            /^\[.*\]/ { in_sec = 0; next }
            in_sec && /^TZEnvVar[[:space:]]*=/ {
                sub(/^TZEnvVar[[:space:]]*=[[:space:]]*/, "")
                sub(/[[:space:]]*$/, "")
                print
                exit
            }
        ' "$1"
    }
    should_write_conf() {
        conf=$1
        [ -f "$conf" ] || return 0
        [ "$force_newer" != 1 ] || return 0
        cur=$(read_conf_tz "$conf")
        case $cur in
            ''|Universal) return 0 ;;
            UTC|Etc/UTC) [ ! "$conf" -nt "$tz_file" ] && return 0 ;;
        esac
        [ "$tz_file" -nt "$conf" ]
    }
    write_conf() {
        air_dir="$1/AIR Music Technology"
        conf="$air_dir/Engine.conf"
        [ ! -L "$1" ] && [ ! -L "$air_dir" ] && [ ! -L "$conf" ] || return 0
        should_write_conf "$conf" || return 0
        mkdir -p "$air_dir"
        tmp="$conf.tmp.$$"
        if [ -f "$conf" ]; then
            awk -v tz="$tz" '
                BEGIN { in_sec = 0; seen_sec = 0; wrote = 0 }
                /^\[TimeDateSettings\][[:space:]]*$/ { in_sec = 1; seen_sec = 1; print; next }
                /^\[.*\]/ {
                    if (in_sec && !wrote) { print "TZEnvVar=" tz; wrote = 1 }
                    in_sec = 0; print; next
                }
                in_sec && /^TZEnvVar[[:space:]]*=/ {
                    if (!wrote) { print "TZEnvVar=" tz; wrote = 1 }
                    next
                }
                { print }
                END {
                    if (!seen_sec) {
                        if (NR > 0) print ""
                        print "[TimeDateSettings]"
                        print "TZEnvVar=" tz
                    } else if (!wrote) {
                        print "TZEnvVar=" tz
                    }
                }
            ' "$conf" > "$tmp"
        else
            printf '[TimeDateSettings]\nTZEnvVar=%s\n' "$tz" > "$tmp"
        fi
        if cmp -s "$tmp" "$conf" 2>/dev/null; then
            rm -f "$tmp"
        else
            chmod 644 "$tmp"
            mv "$tmp" "$conf"
        fi
    }
    found=0
    for d in "$cfg_root"/*.user.settings; do
        [ -d "$d" ] || continue
        found=1
        write_conf "$d"
    done
    if [ "$found" = 0 ]; then
        pfile=${SC5000_PRODUCT_CODE_FILE:-/sys/firmware/devicetree/base/inmusic,product-code}
        if [ -f "$pfile" ]; then
            prod=$(tr -d '\000\r\n ' < "$pfile" | tr '[:upper:]' '[:lower:]')
            case $prod in
                ''|*[!a-z0-9_-]*) ;;
                *) write_conf "$cfg_root/$prod.user.settings" ;;
            esac
        fi
    fi
}

# Invoked by engine.service's ExecStartPre=- after the unhooked Engine stops and before
# the hooked Engine starts, so Engine.conf is updated without stopping our own cgroup.
if [ "${1:-}" = "--restore-tz" ]; then
    force_newer=0
    tz_marker=${SC5000_TZ_NEWER_MARKER:-/run/sc5000-tz-newer}
    if [ -e "$tz_marker" ]; then
        force_newer=1
        rm -f "$tz_marker" 2>/dev/null || :
    fi
    restore_gen2_timezone "$force_newer" || :
    exit 0
fi

if [ -d "$dest" ] && [ ! -L "$dest/gen2-unlocked" ]; then
    [ -e "$dest/gen2-locked" ] || [ -e "$dest/gen2-unlocked" ] || printf '1\n' > "$dest/gen2-unlocked" 2>/dev/null || :
fi

mkdir -p /run/systemd/system/engine.service.d

# No KillMode=process: engine.service is Type=forking with the Scripts/engine shell as main PID, so it would
# leave the hook-less /usr/Engine/Engine running and nobody to run poweroff (#116). The daemons have their own units.
cat <<EOF > /run/systemd/system/engine.service.d/99-sc5000.conf
[Unit]
Wants=enginex-stream-sync.service enginex-rb-bridge.service enginex-sc5000-deemixd.service engine-guardian.service

[Service]
UnsetEnvironment=LD_PRELOAD
Environment="SC5000_UI_HOOK=$lib_hook"
Environment="SC5000_ENGINE_GUARDIAN=$bin_dir/engine-guardian"
ExecStart=
ExecStart=$bin_dir/engine-ui-guard service /usr/Engine/Scripts/runengine
Environment="QT_LOGGING_CONF=/data/logging.ini"
Environment=HOME=/tmp/sc5000-engine-home MESA_SHADER_CACHE_MAX_SIZE=32M
Environment=QML_DISABLE_DISK_CACHE=1
EnvironmentFile=-/data/sc5000-custom/ui-scale.env
ExecStartPre=/bin/mkdir -p -m 0700 /tmp/sc5000-engine-home
ExecStartPre=-$bundle_dir/boot-activate.sh --restore-tz
ExecStartPre=-$bin_dir/engine-ui-guard pre
ExecStartPost=-$bin_dir/engine-ui-guard post
ExecStopPost=-$bin_dir/engine-ui-guard stop
EOF

# The bootstrap library is dlopened locally by ALSA without a preload, so its
# Engine patch constructor stays inert. Keep that minimal activator for network
# diagnostics on subsequent fallback boots; inject nothing into stock Engine.
# Wrap the actual Engine invocation, including retries inside the vendor loop.
if [ -x "$bin_dir/engine-ui-guard" ] && [ -f /usr/Engine/Scripts/engine ]; then
    script_stage=$(mktemp /run/sc5000-engine-script.XXXXXX)
    sed -E \
        -e "s|^([[:space:]]*)/usr/Engine/Engine([[:space:]])|\\1$bin_dir/engine-ui-guard launch /usr/Engine/Engine\\2|" \
        -e "s|^([[:space:]]*(exec[[:space:]]+)?)/usr/sbin/az01-update([[:space:]].*)?\$|\\1env LD_PRELOAD=\"$lib_hook\" /usr/sbin/az01-update\\3|" \
        -e "s|^([[:space:]]*(exec[[:space:]]+)?)az01-update([[:space:]]+--erase-data([[:space:]].*)?)\$|\\1env LD_PRELOAD=\"$lib_hook\" /usr/sbin/az01-update\\3|" \
        -e "s|^([[:space:]]*(exec[[:space:]]+)?)/usr/bin/planck-remote-screen([[:space:]].*)?\$|\\1env LD_PRELOAD=\"$lib_hook\" /usr/bin/planck-remote-screen\\3|" \
        /usr/Engine/Scripts/engine > "$script_stage"
    chmod 755 "$script_stage"
    if grep -Fq "$bin_dir/engine-ui-guard launch /usr/Engine/Engine" "$script_stage"; then
        if ! grep -Fq "LD_PRELOAD=\"$lib_hook\" /usr/sbin/az01-update" "$script_stage"; then
            echo "boot-activate: warning: /usr/sbin/az01-update preload hook missing in /usr/Engine/Scripts/engine" >&2
            ! command -v logger >/dev/null 2>&1 || logger -p user.warning -t enginex-boot-activate "az01-update preload hook missing in /usr/Engine/Scripts/engine" 2>/dev/null || :
        fi
        if cmp -s "$script_stage" /usr/Engine/Scripts/engine; then
            rm -f "$script_stage"
        else
            mv "$script_stage" /run/sc5000-engine-script
            mount --bind /run/sc5000-engine-script /usr/Engine/Scripts/engine
        fi
    else
        rm -f "$script_stage"
        "$bin_dir/engine-ui-guard" fallback previous-fallback
    fi
fi

cat <<EOF > /run/systemd/system/engine-guardian.service
[Unit]
Description=EngineX boot safety and diagnostic report export
Before=engine.service

[Service]
ExecStart=$bin_dir/engine-ui-guard watch
Environment="SC5000_ENGINE_GUARDIAN=$bin_dir/engine-guardian"
Restart=on-failure
RestartSec=1
Nice=10
IOSchedulingClass=best-effort
IOSchedulingPriority=7
EOF

# Converts a rekordbox stick inside the mount, before Engine sees it (as on the 1st-gen images).
edisksd_changed=0
if [ -f "$lib_edisksd_hook" ]; then
    dropin=/run/systemd/system/edisksd.service.d/99-sc5000.conf
    mkdir -p /run/systemd/system/edisksd.service.d
    cat <<EOF > "$dropin.new"
[Service]
Environment="LD_PRELOAD=$lib_edisksd_hook"
EOF
    # Restart edisksd only when the drop-in is new or changed (the .new name is not read by systemd).
    if cmp -s "$dropin.new" "$dropin"; then
        rm -f "$dropin.new"
    else
        mv -f "$dropin.new" "$dropin"
        edisksd_changed=1
    fi
fi

timesyncd_changed=0
if [ -d /run/systemd ]; then
    ntp_dropin=/run/systemd/timesyncd.conf.d/10-enginex-ntp.conf
    mkdir -p /run/systemd/timesyncd.conf.d
    cat <<'EOF' > "$ntp_dropin.new"
@TIMESYNCD_DROPIN@EOF
    if cmp -s "$ntp_dropin.new" "$ntp_dropin"; then
        rm -f "$ntp_dropin.new"
    else
        mv -f "$ntp_dropin.new" "$ntp_dropin"
        timesyncd_changed=1
    fi
fi

ts_file=${SC5000_TIMESTAMP_FILE:-/etc/timestamp}
if [ -f "$ts_file" ] && [ ! -L "$ts_file" ]; then
    data_root=$(dirname "$dest")
    if [ -n "${SC5000_TIMESYNC_LIB_DIR:-}" ]; then
        lib_dir=$SC5000_TIMESYNC_LIB_DIR
    elif [ -d "$data_root/system/var-lib" ]; then
        lib_dir="$data_root/system/var-lib"
    elif [ -d "$data_root/system/var/overlay/lib" ]; then
        lib_dir="$data_root/system/var/overlay/lib"
    else
        lib_dir=/var/lib
    fi
    # Never create $lib_dir itself; skip if the bind source / mount is absent.
    if [ -d "$lib_dir" ] && [ ! -L "$lib_dir" ] && [ ! -L "$lib_dir/systemd" ] && [ ! -L "$lib_dir/systemd/timesync" ] && [ ! -L "$lib_dir/systemd/timesync/clock" ]; then
        clock="$lib_dir/systemd/timesync/clock"
        if [ ! -e "$clock" ] || [ "$clock" -ot "$ts_file" ]; then
            (
                umask 022
                mkdir -p "$lib_dir/systemd/timesync" &&
                : >> "$clock" &&
                touch -r "$ts_file" "$clock" &&
                chmod 644 "$clock"
            ) 2>/dev/null && {
                chown systemd-timesync:systemd-timesync "$lib_dir/systemd/timesync" "$clock" 2>/dev/null ||
                    chown 994:992 "$lib_dir/systemd/timesync" "$clock" 2>/dev/null || :
                timesyncd_changed=1
            } || :
        fi
    fi
fi

if [ -f "$bundle_dir/version.txt" ]; then
    mkdir -p /run/sc5000
    if [ ! -f /etc/sc5000/version.txt ] || ! cmp -s "$bundle_dir/version.txt" /etc/sc5000/version.txt; then
        cp "$bundle_dir/version.txt" /run/sc5000/version.txt
        if [ -f /etc/sc5000/version.txt ]; then
            mount --bind /run/sc5000/version.txt /etc/sc5000/version.txt 2>/dev/null || :
        fi
    fi
fi

if [ -x "$bin_dir/stream-sync" ]; then
    cat <<EOF > /run/systemd/system/enginex-stream-sync.service
[Unit]
Description=EngineX Stream Sync & Audio Analyzer Console
After=network.target enginex-sc5000-deemixd.service

[Service]
Type=simple
ExecStart=$bin_dir/stream-sync --listen :8080 --cpu 3
Restart=always
RestartSec=3
Nice=19
IOSchedulingClass=idle

[Install]
WantedBy=multi-user.target
EOF
fi

if [ -x "$bin_dir/rb-bridge" ]; then
    cat <<EOF > /run/systemd/system/enginex-rb-bridge.service
[Unit]
Description=EngineX Rekordbox Zero-Scan & Pro DJ Link Bridge
After=network.target

[Service]
Type=simple
ExecStart=$bin_dir/rb-bridge --prolink
Restart=always
RestartSec=3
Nice=19
IOSchedulingClass=idle

[Install]
WantedBy=multi-user.target
EOF
fi

if [ -x "$bin_dir/sc5000-deemixd" ]; then
    cat <<EOF > /run/systemd/system/enginex-sc5000-deemixd.service
[Unit]
Description=EngineX Deezer Download Queue
After=network.target

[Service]
Type=simple
ExecStart=$bin_dir/sc5000-deemixd -state-dir $dest/state
Restart=always
RestartSec=3
Nice=19
IOSchedulingClass=idle

[Install]
WantedBy=multi-user.target
EOF
fi

# host-install-userdata.py --ssh-key (#116). The root fs is read-only on these decks, so when the
# copy fails the key is bind-mounted over the stock /root/.ssh/authorized_keys.
ssh_key=
if [ -s "$dest/authorized_keys" ] && [ -x /usr/sbin/sshd ]; then
    ssh_key=1
    chown 0:0 "$dest/authorized_keys" 2>/dev/null || :
    chmod 600 "$dest/authorized_keys"
    if ! cmp -s "$dest/authorized_keys" /root/.ssh/authorized_keys 2>/dev/null; then
        { mkdir -p -m 700 /root/.ssh && cp "$dest/authorized_keys" /root/.ssh/authorized_keys && chmod 600 /root/.ssh/authorized_keys; } 2>/dev/null ||
            mount --bind "$dest/authorized_keys" /root/.ssh/authorized_keys 2>/dev/null || :
    fi
fi

# Seed /data/StreamingAccounts.json with a stored-credential TIDAL entry (#56, #159)
# so Engine 5.3.0 logs in to the local gateway at boot instead of waiting for Profile SSO.
accounts=/data/StreamingAccounts.json
if [ ! -e "$dest/ui/safe-mode" ] && [ -d /data ] && ! grep -qx 'scope=tidal' "$dest/ui/settings.conf" 2>/dev/null; then
    export T='"TIDAL": "{\"provider\":\"TIDAL\",\"providerType\":\"TIDAL\",\"accessToken\":\"enginex\",\"refreshToken\":\"enginex\"}"'
    if grep -Eq '"TIDAL"[[:space:]]*:' "$accounts" 2>/dev/null && ! grep -E '"TIDAL"[[:space:]]*:' "$accounts" 2>/dev/null | grep -q 'accessToken'; then
        awk '{
            if ($0 ~ /"TIDAL"[[:space:]]*:/ && $0 !~ /accessToken/) {
                comma = ($0 ~ /,[[:space:]]*$/) ? "," : ""
                print "    " ENVIRON["T"] comma
            } else {
                print
            }
        }' "$accounts" > "$accounts.tmp" && mv "$accounts.tmp" "$accounts"
    elif ! grep -q '"TIDAL"' "$accounts" 2>/dev/null; then
        if grep -q '"' "$accounts" 2>/dev/null; then
            awk '!d && (i = index($0, "{")) { $0 = substr($0, 1, i) "\n    " ENVIRON["T"] "," substr($0, i + 1); d = 1 } 1' "$accounts" > "$accounts.tmp"
        else
            printf '{\n    %s\n}\n' "$T" > "$accounts.tmp"
        fi
        mv "$accounts.tmp" "$accounts"
    fi
fi

# #160: record the 4-deck satellite link role on Gen2/AZ04 consoles when four_deck_waveform=1.
if [ ! -e "$dest/ui/safe-mode" ] && grep -qx 'four_deck_waveform=1' "$dest/ui/settings.conf" 2>/dev/null; then
    role=$(sed -n 's/^four_deck_role=//p' "$dest/ui/settings.conf" 2>/dev/null | tail -n 1)
    case $role in 12|13|24|34|1|2) ;; *) role=12 ;; esac
    mkdir -p /run/sc5000-assignments && echo "$role" > /run/sc5000-assignments/role
fi

# Sticks mounted before this ran never went through the edisksd hook.
convert_mounted() {
    [ ! -e "$dest/ui/safe-mode" ] || return 0
    [ -x "$bin_dir/rb-bridge" ] || return 0
    for m in /media/*; do
        if [ -d "$m/PIONEER" ] || [ -d "$m/.PIONEER" ]; then
            "$bin_dir/rb-bridge" --mount-path "$m" >/dev/null 2>&1 || :
        fi
    done
}

# In a chroot systemctl only prints "Running in chroot, ignoring" and exits 0, so require systemd as PID 1.
if [ "$(cat /proc/1/comm 2>/dev/null)" = systemd ] && systemctl daemon-reload >/dev/null 2>&1; then
    systemctl restart --no-block enginex-stream-sync.service enginex-rb-bridge.service enginex-sc5000-deemixd.service engine-guardian.service || :
    [ -z "$ssh_key" ] || systemctl start --no-block sshd.service || :
    # Blocking, so edisksd is up again before convert_mounted looks at /media.
    [ "$edisksd_changed" = 0 ] || systemctl restart edisksd.service || :
    [ "$timesyncd_changed" = 0 ] || systemctl restart --no-block systemd-timesyncd.service || :
    convert_mounted
    if check_gen2_tz_newer; then
        : > "${SC5000_TZ_NEWER_MARKER:-/run/sc5000-tz-newer}" 2>/dev/null || :
    fi
    if [ ! -e "$dest/ui/safe-mode" ]; then
        systemctl restart --no-block engine.service || systemctl restart engine.service || :
    else
        "$bin_dir/engine-ui-guard" pre
    fi
else
    restore_gen2_timezone 0 || :
    convert_mounted
    if [ -n "$ssh_key" ] && ! pgrep -x sshd >/dev/null 2>&1; then
        mkdir -p /var/run/sshd && /usr/sbin/sshd || :
    fi
    pkill -x sc5000-suite 2>/dev/null || :
    [ -x "$bin_dir/stream-sync" ] && "$bin_dir/stream-sync" --listen :8080 --cpu 3 >/dev/null 2>&1 &
    [ -x "$bin_dir/rb-bridge" ] && "$bin_dir/rb-bridge" --prolink >/dev/null 2>&1 &
    [ -x "$bin_dir/sc5000-deemixd" ] && "$bin_dir/sc5000-deemixd" -state-dir "$dest/state" >/dev/null 2>&1 &
fi
'''

ALSA_CONF = r'''hook_func.sc5000_alsa_boot_hook {
	lib "/data/sc5000-custom/bundle/lib/libengine-ui-hook.so"
	func sc5000_alsa_boot_hook
}

@hooks [
	{
		func sc5000_alsa_boot_hook
		errors false
	}
]
'''

INSTALL = r'''#!/bin/sh
# Installs this bundle under /data/enginex (/data/sc5000-custom stays as a symlink). Checks SHA256SUMS first and
# replaces each part in one rename, so a failed install leaves the old one.
set -eu
# <MODEL>-<VERSION>-EngineX-OS-<Name>-Update.img (128-byte header + this bundle's .tar.gz): check the SHA-256
# in the header, unpack the payload and run its own install.sh.
if [ "$#" -gt 0 ] && [ -f "$1" ] && [ "$(dd if="$1" bs=27 count=1 2>/dev/null)" = ENGINEX_DATA_BUNDLE_IMG_V1 ]; then
    want=$(dd if="$1" bs=128 count=1 2>/dev/null | tr -d '\000' | sed -n 2p)
    got=$(tail -c +129 "$1" | sha256sum | cut -d' ' -f1)
    [ -n "$want" ] && [ "$want" = "$got" ] || { echo "image damaged: SHA-256 mismatch" >&2; exit 1; }
    mkdir -p "${SC5000_CUSTOM:-/data/enginex}"
    work=$(mktemp -d "${SC5000_CUSTOM:-/data/enginex}/.img-install.XXXXXX")
    trap 'rm -rf "$work"' 0
    tail -c +129 "$1" | tar -xzf - -C "$work"
    sh "$work"/*/install.sh
    exit 0
fi
src=$(cd "$(dirname "$0")" && pwd)
@MIGRATE_IF_DEFAULT@
dest=${SC5000_CUSTOM:-/data/enginex}
if [ -L "$dest" ]; then
    dest=$(readlink -f "$dest" 2>/dev/null || readlink "$dest")
fi
# Where /var/lib lives on /data differs by firmware: see alsa-dir.
alsa_rel=$(cat "$src/alsa-dir")
if [ -n "${SC5000_ALSA_OVERLAY:-}" ]; then
    alsa_dest="$SC5000_ALSA_OVERLAY"
elif [ "$dest" != "/data/enginex" ] && [ ! -d "/data" ]; then
    alsa_dest="$(dirname "$dest")/$alsa_rel"
else
    alsa_dest="/data/$alsa_rel"
fi
cd "$src"
sha256sum -c SHA256SUMS >/dev/null || { echo "bundle damaged: SHA256SUMS mismatch" >&2; exit 1; }
for path in "$dest" "$dest/bundle" "$dest/ui" "$dest/ui/packs" "$dest/ui/icons"; do
    [ ! -L "$path" ] || { echo "refusing symlink: $path" >&2; exit 1; }
done
mkdir -p "$dest/ui"
chmod 700 "$dest"
umask 022
stage=$(mktemp -d "$dest/.bundle.XXXXXX")
trap 'rm -rf "$stage"' 0
mkdir "$stage/bundle" "$stage/ui"
cp -R bin lib SHA256SUMS version.txt alsa-dir boot-activate.sh 99-sc5000-custom.conf install.sh uninstall.sh "$stage/bundle/"
tar -C "$stage/ui" -xf ui-packs.tar
[ -d "$stage/ui/packs" ] || { echo "ui-packs.tar has no packs/" >&2; exit 1; }
rm -rf "$dest/bundle.replaced"
[ ! -e "$dest/bundle" ] || mv "$dest/bundle" "$dest/bundle.replaced"
mv "$stage/bundle" "$dest/bundle"
rm -rf "$dest/bundle.replaced"
for name in packs icons; do
    [ -d "$stage/ui/$name" ] || continue
    rm -rf "$dest/ui/$name.replaced"
    [ ! -e "$dest/ui/$name" ] || mv "$dest/ui/$name" "$dest/ui/$name.replaced"
    mv "$stage/ui/$name" "$dest/ui/$name"
    rm -rf "$dest/ui/$name.replaced"
done
mkdir -p "$alsa_dest"
cp 99-sc5000-custom.conf "$alsa_dest/99-sc5000-custom.conf"
# Same-version repairs preserve fallback. A newly installed version gets one try.
if [ "$(cat "$dest/ui/.pack-version" 2>/dev/null)" != "$(cat version.txt)" ]; then
    rm -f "$dest/ui/safe-mode" "$dest/ui/mods-quarantined" "$dest/ui/boot-count" "$dest/ui/boot-token" "$dest/ui/boot-ready" \
        "$dest/ui/last-crash" "$dest/ui/boot-failures" "$dest/ui/boot-window" "$dest/ui/boot-progress" "$dest/ui/boot-stage" "$dest/ui/boot-report-latest"
fi
if [ -z "${SC5000_CUSTOM:-}" ]; then
    rm -rf "/data/AppDataUser/AIR Music Technology/Engine/.cache/qml"
fi
cp version.txt "$dest/ui/.pack-version"
sha256sum < ui-packs.tar | cut -d' ' -f1 > "$dest/ui/.pack-id"
cp version.txt "$dest/version.txt"
# Opens the *-Update.img upgrade path (screen OTA / USB stick); gen2-locked still wins.
printf '1\n' > "$dest/gen2-unlocked"
echo "EngineX OS $(cat version.txt) installed in $dest"
'''

UNINSTALL = r'''#!/bin/sh
# Removes only what install.sh put in /data/enginex. SSH keys, settings,
# caches and downloads stay; UI packs stay if something else replaced them.
set -eu
@MIGRATE_IF_DEFAULT@
dest=${SC5000_CUSTOM:-/data/enginex}
if [ -L "$dest" ]; then
    dest=$(readlink -f "$dest" 2>/dev/null || readlink "$dest")
fi
# Bundles before 5.3.0.2's AZ04 support had no alsa-dir; they all used the overlay path.
alsa_rel=$(cat "$dest/bundle/alsa-dir" 2>/dev/null || echo system/var/overlay/lib/alsa/conf.d)
if [ -n "${SC5000_ALSA_OVERLAY:-}" ]; then
    alsa_dest="$SC5000_ALSA_OVERLAY"
elif [ "$dest" != "/data/enginex" ] && [ ! -d "/data" ]; then
    alsa_dest="$(dirname "$dest")/$alsa_rel"
else
    alsa_dest="/data/$alsa_rel"
fi
for path in "$dest" "$dest/bundle" "$dest/ui"; do
    [ ! -L "$path" ] || { echo "refusing symlink: $path" >&2; exit 1; }
done
[ -f "$dest/bundle/version.txt" ] || { echo "no bundle installed in $dest" >&2; exit 1; }
version=$(cat "$dest/bundle/version.txt")
ui_id=$(awk '$2 == "ui-packs.tar" {print $1}' "$dest/bundle/SHA256SUMS")
if [ "$(cat "$dest/ui/.pack-id" 2>/dev/null)" = "$ui_id" ]; then
    rm -rf "$dest/ui/packs" "$dest/ui/icons" "$dest/ui/.pack-version" "$dest/ui/.pack-id"
fi
[ "$(cat "$dest/version.txt" 2>/dev/null)" != "$version" ] || rm -f "$dest/version.txt"
rm -f "$dest/gen2-unlocked"
rm -f "$alsa_dest/99-sc5000-custom.conf"
rmdir "$alsa_dest" 2>/dev/null || :
if [ -z "${SC5000_CUSTOM:-}" ] && [ -e /run/sc5000-engine-script ]; then
    ! mountpoint -q /usr/Engine/Scripts/engine || umount /usr/Engine/Scripts/engine || :
    rm -f /run/sc5000-engine-script
fi
rm -rf "$dest/bundle"
if [ -z "${SC5000_CUSTOM:-}" ]; then
    if command -v systemctl >/dev/null 2>&1 && systemctl is-active basic.target >/dev/null 2>&1; then
        for applet in stream-sync rb-bridge sc5000-deemixd; do
            systemctl stop "enginex-$applet.service" 2>/dev/null || :
            rm -f "/run/systemd/system/enginex-$applet.service"
        done
        systemctl stop engine-guardian.service 2>/dev/null || :
        rm -f /run/systemd/system/engine-guardian.service
        rm -f /run/systemd/system/engine.service.d/99-sc5000.conf /run/systemd/system/edisksd.service.d/99-sc5000.conf /run/systemd/timesyncd.conf.d/10-enginex-ntp.conf /run/sc5000-tz-newer
        systemctl daemon-reload 2>/dev/null || :
        systemctl restart --no-block edisksd.service systemd-timesyncd.service 2>/dev/null || :
    fi
    pkill -x sc5000-suite 2>/dev/null || :
    rm -f /run/systemd/system/enginex-stream-sync.service \
          /run/systemd/system/engine-guardian.service \
          /run/systemd/system/enginex-rb-bridge.service \
          /run/systemd/system/enginex-sc5000-deemixd.service \
          /run/systemd/system/engine.service.d/99-sc5000.conf \
          /run/systemd/system/edisksd.service.d/99-sc5000.conf \
          /run/systemd/timesyncd.conf.d/10-enginex-ntp.conf /run/sc5000-tz-newer
fi
echo "EngineX OS $version removed from $dest"
'''


def fail(msg):
    raise SystemExit(f'build-data-bundle: {msg}')


def ui_packs_tar(path):
    """The UI packs as a plain tar (BusyBox tar on the device), checked for packs/."""
    with tarfile.open(path) as src:
        members = src.getmembers()
        for m in members:
            parts = [p for p in m.name.split('/') if p not in ('', '.')]
            if m.name.startswith('/') or '..' in parts or not (m.isfile() or m.isdir()):
                fail(f'{path}: unsafe member {m.name!r}')
        if not any([p for p in m.name.split('/') if p not in ('', '.')][:1] == ['packs'] for m in members):
            fail(f'{path} has no packs/ directory')
        out = io.BytesIO()
        with tarfile.open(fileobj=out, mode='w', format=tarfile.PAX_FORMAT) as dst:
            for m in members:
                dst.addfile(m, src.extractfile(m) if m.isfile() else None)
    return out.getvalue()


# The second-generation models: signed AZ0x firmware with SC LIVE 4's armhf Engine
# (same BuildID; #61, #68, #69), and the AZ04 pair with a 64-bit Engine (#76).
MODELS = {
    'SCLIVE4': 'Denon DJ SC LIVE 4', 'SCLIVE2': 'Denon DJ SC LIVE 2',
    'PRIME4PLUS': 'Denon DJ PRIME 4+', 'PRIMEGOPLUS': 'Denon DJ PRIME GO+',
    'MIXSTREAMPROPLUS': 'Numark Mixstream Pro+', 'MIXSTREAMPROGO': 'Numark Mixstream Pro Go',
    'PRIME4G2': 'Denon DJ PRIME 4 G2', 'SYSTEMONE': 'Rane SYSTEM ONE',
}
PREFIXES = tuple(MODELS)
# File-name model part of <PREFIX>-<VERSION>-EngineX-OS-<name>-Update.img
MODEL_NAMES = {
    'SCLIVE4': 'Denon-SC-LIVE-4-JP21', 'SCLIVE2': 'Denon-SC-LIVE-2-JP20',
    'PRIME4PLUS': 'Denon-PRIME-4-Plus-JC11S', 'PRIMEGOPLUS': 'Denon-PRIME-GO-Plus-JP11S',
    'MIXSTREAMPROPLUS': 'Numark-Mixstream-Pro-Plus-NH08S', 'MIXSTREAMPROGO': 'Numark-Mixstream-Pro-Go-NH10',
    'PRIME4G2': 'Denon-PRIME-4-G2-JP24', 'SYSTEMONE': 'Rane-SYSTEM-ONE-RMZ2',
}
IMG_MAGIC = b'ENGINEX_DATA_BUNDLE_IMG_V1\n'
IMG_HEADER = 128
AZ04 = ('PRIME4G2', 'SYSTEMONE')
EM_ARM, EM_AARCH64 = 40, 183


def build_gen2_img(bundle_bytes, prefix, version):
    """A 128-byte header (magic, payload SHA-256, prefix, version) followed by the .tar.gz.
    The deck's updater hook checks the SHA-256, then installs the payload under /data only."""
    head = (IMG_MAGIC.decode() + f'{hashlib.sha256(bundle_bytes).hexdigest()}\n{prefix}\n{version}\n').encode('ascii')
    if len(head) > IMG_HEADER:
        fail('image header does not fit in 128 bytes')
    return head.ljust(IMG_HEADER, b'\x00') + bundle_bytes


def alsa_dir(prefix):
    """/var/lib's home on /data: AZ04's var-lib.mount binds /data/system/var-lib,
    the armhf AZ0x builds /data/system/var/overlay/lib."""
    return 'system/var-lib/alsa/conf.d' if prefix in AZ04 else 'system/var/overlay/lib/alsa/conf.d'


AZ04_NOTE = """
64-bit (AZ04) build: the UI packs, the streaming gateway (Deezer, SoundCloud,
Beatport) and the EngineX services are in, with 16 Engine patches (including
the stock Apple Music unlock), 14 DSP guards, the 0 dB master soft clipper,
auto gain, Echo Out, SQLite tuning and the OfflineAnalyzer .perf injection
(#149). The 4-deck unlock is for the stand-alone players only; Stems stay as
the official firmware has them. The 64-bit Engine
needs the device's secure element (OP-TEE) to start, so in the sandbox it
stops before its UI: the hook, edits and pack loading were checked, the
screens themselves were not.
"""

README_INSTALL = '''EngineX OS {version} for {model} (data bundle, beta)
=================================================================

This bundle adds EngineX OS to /data. The system partition and the signed
official firmware are not touched; Engine OS 5.3.0 must already be installed.
Status: beta. Built from the same files as the released .img; tested in a
sandbox on the official 5.3.0 root file system, not yet on a real {model}.
Please report results in the GitHub issues (#61, #68, #69, #76).
{note}
Do not flash an .img meant for a first-generation model (PRIME4, PRIMEGO,
MIXSTREAMPRO, ...) on this device. Second-generation units have no hardware
eFuse lock, and their stock updater checks the AZ0x container signature in
software before writing, so a modified or first-generation .img is rejected at
the Verifying step without writing anything. Use this data bundle or
EngineX-Installer instead; it only writes /data/enginex.

Update.img: {prefix}-{version}-EngineX-OS-*-Update.img is this same bundle with a 128-byte header. It is for
the deck's own screen update or a USB stick; the hook checks the header's SHA-256 and writes only /data/enginex.
It is NOT an AZ0x firmware image and never touches the system partition. "sh install.sh <that .img>" works too.

Method B (recommended): install on the device
---------------------------------------------
Needs a root shell on the device. Copy this .tar.gz to the device, then:

    sha256sum -c SHA256SUMS.txt          # on the computer, before copying
    tar -xzf {top}.tar.gz -C /tmp
    sh /tmp/{top}/install.sh
    reboot

install.sh only writes /data/enginex (the old /data/sc5000-custom becomes a symlink to it) and one ALSA file
(/data/{alsa}/99-sc5000-custom.conf). Music,
settings and the Engine library are left alone.

Method A: inject into a userdata image (fastboot)
-------------------------------------------------
Only if you have no shell on the device. "fastboot flash userdata" REPLACES
THE WHOLE /data PARTITION: music, settings, the Engine library and logins.
Back up /data first and use an image made from your own backup:

    python3 host-install-userdata.py <your-userdata-backup.img> --bundle {top}.tar.gz
    sh host-install-fastboot.sh <your-userdata-backup.img>

Never flash an empty or someone else's userdata image.

Decks without fastboot (SYSTEM ONE, PRIME 4 G2: Firmware Update Mode shows up
as USB 1cc5:2027) take a NEW, EMPTY 64 MiB /data image; music, settings and the
library on the internal /data are erased. EngineX-Installer does all of this;
by hand (needs pyusb, see host-install-usbupdate.py):

    mke2fs -t ext4 -O encrypt -L data -b 4096 -F data.img 64M
    python3 host-install-userdata.py data.img --bundle {top}.tar.gz [--ssh-key ~/.ssh/id_ed25519.pub]
    python3 host-install-usbupdate.py data.img     # then power-cycle the deck

Uninstall
---------
    sh /data/enginex/bundle/uninstall.sh && reboot

Removes only what install.sh added; Engine starts stock again.
'''


def build(version, suite, hook, ui_packs, out, prefix='SCLIVE4', edisksd_hook=None):
    if prefix not in PREFIXES:
        fail(f'prefix must be one of {PREFIXES}, got {prefix!r}')
    if not VERSION_RE.match(version):
        fail(f'version must be 3 to 5 dot-separated numbers, got {version!r}')
    if out.exists():
        fail(f'{out} exists; refusing to overwrite')
    files = {}  # path in bundle -> (bytes, mode)
    machine = EM_AARCH64 if prefix in AZ04 else EM_ARM
    if edisksd_hook is None and (Path(hook).parent / 'libedisksd-hook.so').is_file():
        edisksd_hook = Path(hook).parent / 'libedisksd-hook.so'
    parts = [(suite, 'bin/sc5000-suite'), (hook, 'lib/libengine-ui-hook.so')]
    if edisksd_hook:
        parts.append((edisksd_hook, 'lib/libedisksd-hook.so'))
    for path, name in parts:
        data = Path(path).read_bytes()
        if data[:4] != b'\x7fELF':
            fail(f'{path} is not an ELF file')
        # An armhf hook in a 64-bit Engine (or the reverse) is never loaded: refuse it here.
        if int.from_bytes(data[18:20], 'little') != machine:
            fail(f'{path} is not a {"AArch64" if machine == EM_AARCH64 else "32-bit ARM"} ELF, needed for {prefix}')
        files[name] = (data, 0o755)
    for applet in APPLETS:
        files[f'bin/{applet}'] = (WRAPPER.format(applet).encode(), 0o755)
    files['bin/engine-ui-guard'] = ((Path(__file__).resolve().parents[2] /
                                  'rootfs-overlay/etc/sc5000/engine-ui-guard').read_bytes(), 0o755)
    files['ui-packs.tar'] = (ui_packs_tar(ui_packs), 0o644)
    files['version.txt'] = (f'{version}\n'.encode(), 0o644)
    ntp_dropin = (Path(__file__).resolve().parents[2] / 'rootfs-overlay/etc/systemd/timesyncd.conf.d/10-enginex-ntp.conf').read_text()
    files['boot-activate.sh'] = (BOOT_ACTIVATE.replace('@MIGRATE_IF_DEFAULT@', MIGRATE_IF_DEFAULT).replace('@TIMESYNCD_DROPIN@', ntp_dropin).encode(), 0o755)
    files['99-sc5000-custom.conf'] = (ALSA_CONF.encode(), 0o644)
    files['alsa-dir'] = (f'{alsa_dir(prefix)}\n'.encode(), 0o644)
    files['install.sh'] = (INSTALL.replace('@MIGRATE_IF_DEFAULT@', MIGRATE_IF_DEFAULT).encode(), 0o755)
    files['uninstall.sh'] = (UNINSTALL.replace('@MIGRATE_IF_DEFAULT@', MIGRATE_IF_DEFAULT).encode(), 0o755)
    top = f'{prefix}-{version}-EngineX-OS-Data-Bundle'
    files['README-INSTALL.txt'] = (README_INSTALL.format(version=version, model=MODELS[prefix], top=top, alsa=alsa_dir(prefix),
                                                        note=AZ04_NOTE if prefix in AZ04 else '', prefix=prefix).encode(), 0o644)
    here = Path(__file__).parent
    files['host-install-userdata.py'] = ((here / 'host-install-userdata.py').read_bytes(), 0o755)
    files['host-install-fastboot.sh'] = ((here / 'host-install-fastboot.sh').read_bytes(), 0o755)
    files['host-install-usbupdate.py'] = ((here / 'host-install-usbupdate.py').read_bytes(), 0o755)
    sums = ''.join(f'{hashlib.sha256(data).hexdigest()}  {name}\n' for name, (data, _) in sorted(files.items()))
    files['SHA256SUMS'] = (sums.encode(), 0o644)

    out.mkdir(parents=True)
    bundle = out / f'{top}.tar.gz'
    with open(bundle, 'wb') as raw, gzip.GzipFile(filename='', fileobj=raw, mode='wb', mtime=0) as gz, \
            tarfile.open(fileobj=gz, mode='w', format=tarfile.PAX_FORMAT) as tar:
        dirs = sorted({f'{top}/{n}'.rsplit('/', 1)[0] for n in files} | {top})
        for name in dirs:
            info = tarfile.TarInfo(name)
            info.type, info.mode = tarfile.DIRTYPE, 0o755
            tar.addfile(info)
        for name, (data, mode) in sorted(files.items()):
            info = tarfile.TarInfo(f'{top}/{name}')
            info.size, info.mode = len(data), mode
            tar.addfile(info, io.BytesIO(data))
    bundle_bytes = bundle.read_bytes()
    digest = hashlib.sha256(bundle_bytes).hexdigest()
    img_path = out / f'{prefix}-{version}-EngineX-OS-{MODEL_NAMES[prefix]}-Update.img'
    img_path.write_bytes(build_gen2_img(bundle_bytes, prefix, version))
    (out / 'SHA256SUMS.txt').write_text(f'{digest}  {bundle.name}\n')
    (out / 'host-install-userdata.py').write_bytes(files['host-install-userdata.py'][0])
    (out / 'host-install-userdata.py').chmod(0o755)
    (out / 'host-install-fastboot.sh').write_bytes(files['host-install-fastboot.sh'][0])
    (out / 'host-install-fastboot.sh').chmod(0o755)
    (out / 'host-install-usbupdate.py').write_bytes(files['host-install-usbupdate.py'][0])
    (out / 'host-install-usbupdate.py').chmod(0o755)
    return {'bundle': str(bundle), 'image': str(img_path), 'sha256': digest, 'files': len(files)}


def main(argv=None):
    p = argparse.ArgumentParser(description=__doc__.split('\n\n')[0])
    p.add_argument('version')
    p.add_argument('--suite', required=True, type=Path, help='static sc5000-suite (ARMv7, arm64 for AZ04)')
    p.add_argument('--hook', required=True, type=Path, help='libengine-ui-hook.so')
    p.add_argument('--edisksd-hook', type=Path, help='libedisksd-hook.so (default: next to --hook, if present)')
    p.add_argument('--ui-packs', required=True, type=Path, help='build_packs.py packs.tar(.gz)')
    p.add_argument('--out', required=True, type=Path, help='new output directory')
    p.add_argument('--prefix', default='SCLIVE4', choices=PREFIXES, help='model prefix in the file name')
    a = p.parse_args(argv)
    json.dump(build(a.version, a.suite, a.hook, a.ui_packs, a.out, a.prefix, a.edisksd_hook), sys.stdout, indent=2)
    print()


if __name__ == '__main__':
    main()
