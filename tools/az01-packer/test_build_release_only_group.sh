#!/usr/bin/env bash
set -euo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin" "$TMP/fw" "$TMP/qrc"
printf 'stock\n' > "$TMP/fw/SC5000-5.3.0-Update.img"
cp "$TMP/fw/SC5000-5.3.0-Update.img" "$TMP/fw/SC5000M-5.3.0-Update.img"
cp "$TMP/fw/SC5000-5.3.0-Update.img" "$TMP/fw/PRIMEGO-5.3.0-Update.img"
printf 'fsck\n' > "$TMP/fsck.hfsplus"
chmod +x "$TMP/fsck.hfsplus"
cat > "$TMP/bin/go" <<'MOCK'
#!/usr/bin/env bash
set -eu
if [[ $* == *'build '* ]]; then
    out=
    while (($#)); do [[ $1 == -o ]] && { out=$2; shift 2; continue; }; shift; done
    : > "$out"
fi
MOCK
cat > "$TMP/bin/python3" <<'MOCK'
#!/usr/bin/env bash
set -eu
args=("$@")
last=${args[${#args[@]}-1]}
case " $* " in
    *build_packs.py*)
        for ((i=0; i<${#args[@]}; i++)); do if [[ ${args[i]} == --out ]]; then out=${args[i+1]}; fi; done
        mkdir -p "$out"
        printf 'packs\n' | gzip -c > "$out.tar.gz" ;;
    *' splash_tool.py '*|*'az01_packer.py '*)
        case " $* " in
            *' --png '*) for ((i=0; i<${#args[@]}; i++)); do if [[ ${args[i]} == --png ]]; then : > "${args[i+1]}"; fi; done ;;
            *' --bin '*) for ((i=0; i<${#args[@]}; i++)); do if [[ ${args[i]} == --bin ]]; then : > "${args[i+1]}"; fi; done ;;
            *' --output '*) for ((i=0; i<${#args[@]}; i++)); do if [[ ${args[i]} == --output ]]; then : > "${args[i+1]}"; fi; done ;;
            *) [[ $last == *.bin ]] && printf 'splash\n' > "$last" || printf '{}\n' ;;
        esac ;;
esac
MOCK
cat > "$TMP/bin/readelf" <<'MOCK'
#!/bin/sh
printf '  Machine: ARM\n'
MOCK
cat > "$TMP/bin/modinfo" <<'MOCK'
#!/bin/sh
if [ "${1:-}" = -F ] && [ "${2:-}" = name ]; then printf 'hfsplusx\n'; else printf '6.18.57-az01 SMP\n'; fi
MOCK
cat > "$TMP/bin/sh" <<'MOCK'
#!/usr/bin/env bash
set -eu
if [[ ${1:-} == tools/az01-packer/check-hfsplusx.sh ]]; then exit 0; fi
if [[ ${1:-} == tools/aacenc/build.sh ]]; then : > "$OUT"; exit 0; fi  # the SHOUTcast encoder front end
exec /bin/sh "$@"
MOCK
chmod +x "$TMP/bin"/*
OUT="$TMP/out"
(
    cd "$ROOT"
    PATH="$TMP/bin:$PATH" QRC="$TMP/qrc" HFSPLUSX_KO="$TMP/fsck.hfsplus" ONLY_GROUP=SC5000 \
        /bin/sh tools/az01-packer/build-release.sh 5.3.0.5 "$TMP/fw" none "$TMP/fsck.hfsplus" "$OUT"
)
mapfile -t images < <(find "$OUT" -maxdepth 1 -type f -name '*-Update.img' -printf '%f\n' | sort)
[[ ${#images[@]} -eq 3 ]] || { printf 'expected three SC5000 images, found %s\n' "${#images[@]}" >&2; exit 1; }
for image in "${images[@]}"; do [[ $image == SC5000-* || $image == Denon-SC5000-* || $image == SC5000M-* || $image == Denon-SC5000M-* || $image == PRIMEGO-* || $image == Denon-PRIME-GO-* ]] || { echo "unexpected image: $image" >&2; exit 1; }; done
! find "$OUT" -maxdepth 1 -type f -name '*SC6000*' | grep -q .
! find "$OUT" -maxdepth 1 -type f -name '*PRIME4*' | grep -q .
! find "$OUT" -maxdepth 1 -type f -name '*Data-Bundle.tar.gz' | grep -q .
printf 'ONLY_GROUP=SC5000 emits only the SC5000 group images\n'
