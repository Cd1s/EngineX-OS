#!/bin/sh
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
# Same markers as sc5000-custom-prepare: new packs get a fresh try after safe mode.
rm -f "$dest/ui/safe-mode" "$dest/ui/mods-quarantined" "$dest/ui/boot-count" "$dest/ui/boot-token" "$dest/ui/last-crash"
if [ -z "${SC5000_CUSTOM:-}" ]; then
    rm -rf "/data/AppDataUser/AIR Music Technology/Engine/.cache/qml"
fi
cp version.txt "$dest/ui/.pack-version"
sha256sum < ui-packs.tar | cut -d' ' -f1 > "$dest/ui/.pack-id"
cp version.txt "$dest/version.txt"
# Opens the *-Update.img upgrade path (screen OTA / USB stick); gen2-locked still wins.
printf '1\n' > "$dest/gen2-unlocked"
echo "EngineX OS $(cat version.txt) installed in $dest"
