#!/bin/sh
# Installs this bundle under /data/sc5000-custom. Checks SHA256SUMS first and
# replaces each part in one rename, so a failed install leaves the old one.
set -eu
src=$(cd "$(dirname "$0")" && pwd)
dest=${SC5000_CUSTOM:-/data/sc5000-custom}
# Where /var/lib lives on /data differs by firmware: see alsa-dir.
alsa_rel=$(cat "$src/alsa-dir")
if [ -n "${SC5000_ALSA_OVERLAY:-}" ]; then
    alsa_dest="$SC5000_ALSA_OVERLAY"
elif [ "$dest" != "/data/sc5000-custom" ] && [ ! -d "/data" ]; then
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
cp -R bin lib SHA256SUMS version.txt alsa-dir boot-activate.sh 99-sc5000-custom.conf uninstall.sh "$stage/bundle/"
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
# Same markers as sc5000-custom-prepare: new packs get a fresh try after safe mode.
rm -f "$dest/ui/safe-mode" "$dest/ui/boot-count" "$dest/ui/boot-token"
cp version.txt "$dest/ui/.pack-version"
sha256sum < ui-packs.tar | cut -d' ' -f1 > "$dest/ui/.pack-id"
cp version.txt "$dest/version.txt"
echo "EngineX OS $(cat version.txt) installed in $dest"
