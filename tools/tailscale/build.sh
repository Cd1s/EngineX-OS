#!/bin/sh
# Builds the trimmed Tailscale client the deck downloads on demand (#130).
# Run on the build host (the build host), never on the laptop or the deck:
#   sh tools/tailscale/build.sh <tailscale-tag, e.g. v1.104.5> <out-dir>
# Output: <out-dir>/tailscaled-armhf, tailscaled-arm64 (UPX-packed box binary, ~4 MB each) and SHA256SUMS.
# Upload them to the GitHub release "tailscale-<tag>" that pkg/webui/tailscale.go points at.
#
# Not --extra-small: that profile drops netstack (userspace networking) and serve, which the deck needs.
set -eu
TAG=${1:?tailscale tag, e.g. v1.104.5}
OUT=${2:?new output directory}
[ ! -e "$OUT" ] || { echo "$OUT exists; refusing to overwrite" >&2; exit 2; }
command -v upx >/dev/null || { echo "install upx first (apt-get install upx-ucl)" >&2; exit 2; }
SRC=$(mktemp -d)
trap 'rm -rf "$SRC"' EXIT
git clone --depth 1 --branch "$TAG" https://github.com/tailscale/tailscale.git "$SRC/ts"
mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)
cd "$SRC/ts"
VER=${TAG#v}
TAGS=$(GOOS= GOARCH= go run ./cmd/featuretags --min --add=netstack,serve,nattraversal,udptransport,ipnbus,bakedroots,health),ts_include_cli
LD="-X tailscale.com/version.longStamp=$VER-ee -X tailscale.com/version.shortStamp=$VER -w -s"
for t in arm:7:armhf arm64::arm64; do
    a=${t%%:*}; r=${t#*:}; m=${r%%:*}; n=${r#*:}
    CGO_ENABLED=0 GOOS=linux GOARCH=$a GOARM=$m go build -tags="$TAGS" -trimpath -ldflags "$LD" -o "$OUT/tailscaled-$n" tailscale.com/cmd/tailscaled
    upx --lzma --best -q "$OUT/tailscaled-$n" >/dev/null
done
cd "$OUT" && sha256sum tailscaled-armhf tailscaled-arm64 > SHA256SUMS && ls -l
