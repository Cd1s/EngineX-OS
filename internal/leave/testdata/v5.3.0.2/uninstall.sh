#!/bin/sh
# Removes only what install.sh put in /data/sc5000-custom. SSH keys, settings,
# caches and downloads stay; UI packs stay if something else replaced them.
set -eu
dest=${SC5000_CUSTOM:-/data/sc5000-custom}
# Bundles before 5.3.0.2's AZ04 support had no alsa-dir; they all used the overlay path.
alsa_rel=$(cat "$dest/bundle/alsa-dir" 2>/dev/null || echo system/var/overlay/lib/alsa/conf.d)
if [ -n "${SC5000_ALSA_OVERLAY:-}" ]; then
    alsa_dest="$SC5000_ALSA_OVERLAY"
elif [ "$dest" != "/data/sc5000-custom" ] && [ ! -d "/data" ]; then
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
rm -f "$alsa_dest/99-sc5000-custom.conf"
rmdir "$alsa_dest" 2>/dev/null || :
rm -rf "$dest/bundle"
echo "EngineX OS $version removed from $dest"
