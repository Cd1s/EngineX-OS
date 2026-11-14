#!/bin/sh
# Checks the output directory of a release build before SHA256SUMS.txt is signed (#239). Run by
# sign-release.sh; usable on its own:  sh tools/az01-packer/check-release-manifest.sh <out-dir>
#
# A signature makes the list authoritative, so a list that leaves an artifact out would be signed
# as complete and the deck or installer would refuse that artifact. Names are compared whole
# (an entry for X-Update.img.bak is not an entry for X-Update.img), every file of an artifact
# class in the directory must be listed, every entry must be an artifact the directory has, and the
# counts must be those of the release. 5.3.0.4/5.3.0.5 have 16 images, 8 legacy data bundles and
# 4 installers; later releases retire the bundles: ENGINEX_EXPECT_BUNDLES=0.
set -eu
OUT=$(cd "${1:?release output directory}" && pwd)
fail() { echo "check-release-manifest: $*" >&2; exit 1; }
SUMS=$OUT/SHA256SUMS.txt
[ -f "$SUMS" ] || fail "SHA256SUMS.txt is missing"
[ -f "$OUT/RELEASE-TAG.txt" ] || fail "RELEASE-TAG.txt is missing (build-release.sh writes it)"
TAG=$(cat "$OUT/RELEASE-TAG.txt")
printf '%s\n' "$TAG" | grep -Eq '^v[0-9]+(\.[0-9]+)+(-r[1-9][0-9]*)?$' || fail "RELEASE-TAG.txt does not hold a release tag (v<version> or v<version>-rN)"
[ "$(wc -l < "$OUT/RELEASE-TAG.txt")" = 1 ] || fail "RELEASE-TAG.txt must be one line"

# Every line is "<64 hex>  <name>": the names are what is compared below.
LINES=$(wc -l < "$SUMS")
NAMES=$(sed -n 's/^[0-9a-fA-F]\{64\}  \([^ ]\{1,\}\)$/\1/p' "$SUMS")
[ "$(printf '%s\n' "$NAMES" | grep -c .)" = "$LINES" ] || fail "SHA256SUMS.txt has a line that is not '<sha256>  <name>' with one plain name"
# The listed files are the files, byte for byte.
(cd "$OUT" && sha256sum -c --quiet SHA256SUMS.txt) || fail "a listed file does not match SHA256SUMS.txt"
[ -z "$(printf '%s\n' "$NAMES" | sort | uniq -d)" ] || fail "SHA256SUMS.txt lists a name twice"

VERSION=${TAG#v}
VERSION=${VERSION%-r[0-9]*}
for n in $NAMES; do
    case $n in
    *-Update.img|*-Data-Bundle.tar.gz)
        case $n in *-"$VERSION"-EngineX-OS-*) ;; *) fail "$n is not a file of version $VERSION" ;; esac ;;
    EngineX-Installer-*)
        case $n in EngineX-Installer-"$VERSION"-*) ;; *) fail "$n is not an installer of version $VERSION" ;; esac ;;
    RELEASE-TAG.txt) ;;
    *) fail "SHA256SUMS.txt lists $n, which is not a release artifact" ;;
    esac
done
# Every artifact in the directory is listed, by its whole name.
for f in "$OUT"/*-Update.img "$OUT"/*-Data-Bundle.tar.gz "$OUT"/EngineX-Installer-* "$OUT"/RELEASE-TAG.txt; do
    [ -e "$f" ] || continue
    printf '%s\n' "$NAMES" | grep -Fxq -- "$(basename "$f")" || fail "$(basename "$f") is in the directory but not in SHA256SUMS.txt"
done
count() { printf '%s\n' "$NAMES" | grep -c -- "$1" || true; }
[ "$(count '-Update\.img$')" = "${ENGINEX_EXPECT_UPDATE_IMG:-16}" ] || fail "expected ${ENGINEX_EXPECT_UPDATE_IMG:-16} *-Update.img in SHA256SUMS.txt, found $(count '-Update\.img$')"
[ "$(count '-Data-Bundle\.tar\.gz$')" = "${ENGINEX_EXPECT_BUNDLES:-8}" ] || fail "expected ${ENGINEX_EXPECT_BUNDLES:-8} *-Data-Bundle.tar.gz in SHA256SUMS.txt, found $(count '-Data-Bundle\.tar\.gz$')"
[ "$(count '^EngineX-Installer-')" = "${ENGINEX_EXPECT_INSTALLERS:-4}" ] || fail "expected ${ENGINEX_EXPECT_INSTALLERS:-4} EngineX-Installer-* in SHA256SUMS.txt, found $(count '^EngineX-Installer-')"
echo "check-release-manifest: $(printf '%s\n' "$NAMES" | grep -c .) entries for $TAG, complete"
