#!/bin/sh
# Removes only what install.sh put in /data/enginex. SSH keys, settings,
# caches and downloads stay; UI packs stay if something else replaced them.
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
rm -rf "$dest/bundle"
if command -v systemctl >/dev/null 2>&1 && systemctl is-active basic.target >/dev/null 2>&1; then
    for applet in stream-sync rb-bridge sc5000-deemixd; do
        systemctl stop "enginex-$applet.service" 2>/dev/null || :
        rm -f "/run/systemd/system/enginex-$applet.service"
    done
    rm -f /run/systemd/system/engine.service.d/99-sc5000.conf /run/systemd/system/edisksd.service.d/99-sc5000.conf
    systemctl daemon-reload 2>/dev/null || :
    systemctl restart --no-block edisksd.service 2>/dev/null || :
fi
pkill -x sc5000-suite 2>/dev/null || :
rm -f /run/systemd/system/enginex-stream-sync.service \
      /run/systemd/system/enginex-rb-bridge.service \
      /run/systemd/system/enginex-sc5000-deemixd.service \
      /run/systemd/system/engine.service.d/99-sc5000.conf \
      /run/systemd/system/edisksd.service.d/99-sc5000.conf
echo "EngineX OS $version removed from $dest"
