#!/bin/sh
# Signs the final SHA256SUMS.txt of a release (#239). Run on the build host, last, after
# build-release.sh AND build-installer.sh (the installer step rewrites SHA256SUMS.txt, so a
# signature made earlier would not match the file that is published):
#   sh tools/az01-packer/sign-release.sh <out-dir>
# The key stays at /root/.config/enginex/release.key (root-only, 0600, PKCS#8 PEM Ed25519).
# The hook pins that path for the signer. It writes <out-dir>/SHA256SUMS.txt.sig, which is
# uploaded to the Release with the other files.
set -eu
OUT=$(cd "${1:?release output directory}" && pwd)
export ENGINEX_RELEASE_KEY=/root/.config/enginex/release.key
cd "$(dirname "$0")/../.."
# The list must describe the files that are about to be uploaded, all of them, and nothing else.
sh tools/az01-packer/check-release-manifest.sh "$OUT"
W=$(mktemp -d)
trap 'rm -rf "$W"' EXIT
CGO_ENABLED=0 go build -trimpath -o "$W/enginex-sign" ./cmd/enginex-sign
TAG=$(cat "$OUT/RELEASE-TAG.txt")
rm -f "$OUT/SHA256SUMS.txt.sig"
# Refuses a key that is not embedded in this checkout: decks would reject the release.
"$W/enginex-sign" "$OUT/SHA256SUMS.txt"
# What a deck does: verify with the embedded keys only, and for the release tag it will be shown under.
"$W/enginex-sign" --verify --tag "$TAG" "$OUT/SHA256SUMS.txt"
ls -l "$OUT/SHA256SUMS.txt" "$OUT/SHA256SUMS.txt.sig"
