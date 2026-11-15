#!/bin/sh
# One-shot boot activator for SC LIVE 4 (inmusic,jp21) and read-only root decks (#61).
set -eu

if [ -z "${SC5000_CUSTOM:-}" ]; then
# /data/sc5000-custom became /data/enginex (#103): one rename keeps every file, and a
# symlink keeps the old path working for older scripts and for a rollback.
old=/data/sc5000-custom
new=/data/enginex
if [ -d "$old" ] && [ ! -L "$old" ]; then
    if [ ! -e "$new" ] && [ ! -L "$new" ]; then
        mv "$old" "$new"
    elif [ -d "$new" ] && [ ! -L "$new" ]; then
        # Both are real: move over what enginex lacks, set the remainder aside.
        for e in "$old"/* "$old"/.[!.]*; do
            [ -e "$e" ] || [ -L "$e" ] || continue
            [ -e "$new/${e##*/}" ] || [ -L "$new/${e##*/}" ] || mv "$e" "$new/"
        done
        rmdir "$old" 2>/dev/null || mv "$old" "$new/legacy-sc5000-custom.$$"
    fi
fi
[ -e "$new" ] || [ -L "$new" ] || mkdir -p "$new"
{ [ -d "$old" ] && [ ! -L "$old" ]; } || ln -sfn enginex "$old"
fi
dest="${SC5000_CUSTOM:-/data/enginex}"
bundle_dir="$dest/bundle"
bin_dir="$bundle_dir/bin"
lib_hook="$bundle_dir/lib/libengine-ui-hook.so"
lib_edisksd_hook="$bundle_dir/lib/libedisksd-hook.so"

mkdir -p /run/systemd/system/engine.service.d

# No KillMode=process: engine.service is Type=forking with the Scripts/engine shell as main PID, so it would
# leave the hook-less /usr/Engine/Engine running and nobody to run poweroff (#116). The daemons have their own units.
cat <<EOF > /run/systemd/system/engine.service.d/99-sc5000.conf
[Unit]
Wants=enginex-stream-sync.service enginex-rb-bridge.service enginex-sc5000-deemixd.service

[Service]
Environment="LD_PRELOAD=$lib_hook"
EnvironmentFile=-/data/sc5000-custom/ui-scale.env
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
if [ -d /data ] && ! grep -qx 'scope=tidal' "$dest/ui/settings.conf" 2>/dev/null; then
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
if grep -qx 'four_deck_waveform=1' "$dest/ui/settings.conf" 2>/dev/null; then
    role=$(sed -n 's/^four_deck_role=//p' "$dest/ui/settings.conf" 2>/dev/null | tail -n 1)
    case $role in 12|13|24|34|1|2) ;; *) role=12 ;; esac
    mkdir -p /run/sc5000-assignments && echo "$role" > /run/sc5000-assignments/role
fi

# Sticks mounted before this ran never went through the edisksd hook.
convert_mounted() {
    [ -x "$bin_dir/rb-bridge" ] || return 0
    for m in /media/*; do
        if [ -d "$m/PIONEER" ] || [ -d "$m/.PIONEER" ]; then
            "$bin_dir/rb-bridge" --mount-path "$m" >/dev/null 2>&1 || :
        fi
    done
}

# In a chroot systemctl only prints "Running in chroot, ignoring" and exits 0, so require systemd as PID 1.
if [ "$(cat /proc/1/comm 2>/dev/null)" = systemd ] && systemctl daemon-reload >/dev/null 2>&1; then
    systemctl restart --no-block enginex-stream-sync.service enginex-rb-bridge.service enginex-sc5000-deemixd.service || :
    [ -z "$ssh_key" ] || systemctl start --no-block sshd.service || :
    # Blocking, so edisksd is up again before convert_mounted looks at /media.
    [ "$edisksd_changed" = 0 ] || systemctl restart edisksd.service || :
    convert_mounted
    systemctl restart --no-block engine.service || systemctl restart engine.service || :
else
    convert_mounted
    if [ -n "$ssh_key" ] && ! pgrep -x sshd >/dev/null 2>&1; then
        mkdir -p /var/run/sshd && /usr/sbin/sshd || :
    fi
    pkill -x sc5000-suite 2>/dev/null || :
    [ -x "$bin_dir/stream-sync" ] && "$bin_dir/stream-sync" --listen :8080 --cpu 3 >/dev/null 2>&1 &
    [ -x "$bin_dir/rb-bridge" ] && "$bin_dir/rb-bridge" --prolink >/dev/null 2>&1 &
    [ -x "$bin_dir/sc5000-deemixd" ] && "$bin_dir/sc5000-deemixd" -state-dir "$dest/state" >/dev/null 2>&1 &
fi
