#!/bin/sh
# Signs the final SHA256SUMS.txt of a release (#239). Run on the build host, last, after
# build-release.sh AND build-installer.sh (the installer step rewrites SHA256SUMS.txt, so a
# signature made earlier would not match the file that is published):
#   ENGINEX_RELEASE_KEY=<release key file> sh tools/az01-packer/sign-release.sh <out-dir>
# The key is a root-only (0600) PKCS#8 PEM Ed25519 key; the tool reads its path from
# ENGINEX_RELEASE_KEY and never prints it. It writes <out-dir>/SHA256SUMS.txt.sig, which is
# uploaded to the Release with the other files.
set -eu
OUT=$(cd "${1:?release output directory}" && pwd)
: "${ENGINEX_RELEASE_KEY:?set ENGINEX_RELEASE_KEY to the release key file (public releases are signed, #239)}"
cd "$(dirname "$0")/../.."
[ -f "$OUT/SHA256SUMS.txt" ] || { echo "$OUT/SHA256SUMS.txt is missing" >&2; exit 2; }
W=$(mktemp -d)
trap 'rm -rf "$W"' EXIT
CGO_ENABLED=0 go build -trimpath -o "$W/enginex-sign" ./cmd/enginex-sign
# The list must describe the files that are about to be uploaded, all of them.
(cd "$OUT" && sha256sum -c --quiet SHA256SUMS.txt)
for f in "$OUT"/*-Update.img "$OUT"/*-Data-Bundle.tar.gz "$OUT"/EngineX-Installer-*; do
    [ -e "$f" ] || continue
    grep -qF "  $(basename "$f")" "$OUT/SHA256SUMS.txt" || { echo "$(basename "$f") is not in SHA256SUMS.txt" >&2; exit 1; }
done
rm -f "$OUT/SHA256SUMS.txt.sig"
# Refuses a key that is not embedded in this checkout: decks would reject the release.
"$W/enginex-sign" "$OUT/SHA256SUMS.txt"
# What a deck does: verify with the embedded keys only.
"$W/enginex-sign" --verify "$OUT/SHA256SUMS.txt"
ls -l "$OUT/SHA256SUMS.txt" "$OUT/SHA256SUMS.txt.sig"
