#!/usr/bin/env bash
# The release gate's loader audit (tools/az01-packer/elf-loader-audit.sh) on the real stock rootfs of
# the three bases (review of the SHOUTcast mode, finding 2). Build host only, as root, in a private
# mount namespace under the heavy lock; ARM host (armhf and arm64 programs run natively):
#   GEN1_ROOTFS=<rootfs.ext2 of the stock SC5000 5.3.0 image, from az01_packer.py unpack> \
#     flock /var/lock/enginex-heavy.lock unshare -m --propagation private bash tools/az01-packer/test_elf_loader_audit.sh
# GEN2_ROOTFS / AZ04_ROOTFS default to the gate's (the lab's SC LIVE 4 and SYSTEM ONE rootfs); both
# are only ever mounted read-only or as an overlay's lower layer.
#  1. On each base, enginex-aacenc as tools/aacenc/build.sh makes it loads and opens the stock AAC
#     encoder. The same program with a loader the base lacks (soft-float /lib/ld-linux.so.3 on armhf,
#     musl on AZ04) fails: `ld.so --list` alone, the gate's old check, passed it. A program that needs
#     a library the base lacks fails (as it did before); a file that is no dynamic ELF is skipped.
#  2. gen2_aacenc_audit, the gate's step for the second-generation images of a build, run on images
#     made by build-data-bundle.py: the good encoders pass, a bundle whose encoder has a bad
#     interpreter fails, a bundle without the encoder fails, first-generation images are skipped.
set -euo pipefail
REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
. "$REPO/rootfs-overlay/usr/libexec/enginex-migration"
. "$REPO/tools/az01-packer/elf-loader-audit.sh"
ENGINEX_LAB=${ENGINEX_LAB:-$legacy_lab}
GEN1_ROOTFS=${GEN1_ROOTFS:?set GEN1_ROOTFS to the rootfs.ext2 of the unpacked stock SC5000 5.3.0 image}
GEN2_ROOTFS=${GEN2_ROOTFS:-$ENGINEX_LAB/sclive4-rea61-followup/rootfs}
AZ04_ROOTFS=${AZ04_ROOTFS:-$ENGINEX_LAB/systemone-analysis/rootfs}
[ "$(id -u)" = 0 ] || { echo "run as root (chroot, mounts)" >&2; exit 2; }
[ "$(readlink /proc/self/ns/mnt)" != "$(readlink /proc/1/ns/mnt)" ] ||
    { echo "run inside: unshare -m --propagation private" >&2; exit 2; }
# /tmp must be a real directory: a symlink would be followed on the host.
for d in "$GEN2_ROOTFS" "$AZ04_ROOTFS"; do [ -d "$d/tmp" ] && [ ! -L "$d/tmp" ] || { echo "missing stock rootfs (or its /tmp) $d" >&2; exit 2; }; done
[ -f "$GEN1_ROOTFS" ] || { echo "missing $GEN1_ROOTFS" >&2; exit 2; }

T=$(mktemp -d /var/tmp/enginex-elf-audit.XXXXXX)
cleanup() {
    local m
    for m in $(findmnt -rn -o TARGET | grep "^$T/" | sort -r); do umount "$m" 2>/dev/null || umount -l "$m" || :; done
    # Never delete through a mount: anything still mounted under $T stays, and so does $T.
    if findmnt -rn -o TARGET | grep -q "^$T/"; then echo "leaving $T: still mounted" >&2; else rm -rf "$T"; fi
}
trap cleanup EXIT
FAILS=0
ok() { echo "ok   $*"; }
no() { echo "FAIL $*"; FAILS=$((FAILS + 1)); }

# --- programs -------------------------------------------------------------------------------
mkdir -p "$T/bin"
CROSS64=aarch64-linux-gnu-; [ "$(uname -m)" != aarch64 ] || CROSS64=
OUT=$T/bin/aacenc-armhf sh "$REPO/tools/aacenc/build.sh"
CROSS=$CROSS64 OUT=$T/bin/aacenc-arm64 sh "$REPO/tools/aacenc/build.sh"
# The same programs with their interpreter swapped for one the bases do not have (same file
# otherwise): a soft-float armhf loader, a musl arm64 loader.
python3 -I - "$T/bin" <<'PY'
import sys
from pathlib import Path
d = Path(sys.argv[1])
for src, old, new in (('aacenc-armhf', b'/lib/ld-linux-armhf.so.3', b'/lib/ld-linux.so.3'),
                      ('aacenc-arm64', b'/lib/ld-linux-aarch64.so.1', b'/lib/ld-musl-aarch64.so.1')):
    data = (d / src).read_bytes()
    assert data.count(old + b'\0') == 1, src
    out = d / (src + '-badinterp')
    out.write_bytes(data.replace(old + b'\0', new + b'\0' * (len(old) - len(new) + 1)))
    out.chmod(0o755)
PY
# A program that needs a library no base has.
printf 'int enginex_gone(void) { return 0; }\n' > "$T/bin/gone.c"
printf 'int enginex_gone(void);\nint main(void) { return enginex_gone(); }\n' > "$T/bin/needs.c"
mkdir -p "$T/bin/armhf" "$T/bin/arm64"
arm-linux-gnueabihf-gcc -shared -fPIC -o "$T/bin/armhf/libenginex-gone.so" "$T/bin/gone.c"
arm-linux-gnueabihf-gcc -o "$T/bin/needs-armhf" "$T/bin/needs.c" -L"$T/bin/armhf" -lenginex-gone
"${CROSS64}gcc" -shared -fPIC -o "$T/bin/arm64/libenginex-gone.so" "$T/bin/gone.c"
"${CROSS64}gcc" -o "$T/bin/needs-arm64" "$T/bin/needs.c" -L"$T/bin/arm64" -lenginex-gone
printf '#!/bin/sh\necho not an ELF\n' > "$T/bin/script"
chmod 0755 "$T/bin/script"

# --- 1. the audit on each base --------------------------------------------------------------
base() { # <name> <lower dir> <arch> <loader>
    local name=$1 lower=$2 arch=$3 loader=$4 root=$T/root-$1 rc out
    mkdir -p "$root" "$T/upper-$name" "$T/work-$name"
    mount -t overlay overlay -o "lowerdir=$lower,upperdir=$T/upper-$name,workdir=$T/work-$name" "$root"
    [ -d "$root/tmp" ] && [ ! -L "$root/tmp" ] || { no "$name: /tmp of the rootfs is not a directory"; umount "$root"; return; }
    mkdir -p "$root/tmp/t"
    install -m 0755 "$T/bin/aacenc-$arch" "$root/tmp/t/enginex-aacenc"
    install -m 0755 "$T/bin/aacenc-$arch-badinterp" "$root/tmp/t/enginex-aacenc-badinterp"
    install -m 0755 "$T/bin/needs-$arch" "$root/tmp/t/needs-gone"
    install -m 0755 "$T/bin/script" "$root/tmp/t/script"

    rc=0; out=$(elf_loader_audit "$root" "$loader" /tmp/t/enginex-aacenc) || rc=$?
    [ "$rc" = 0 ] && ok "$name: enginex-aacenc loads" || no "$name: enginex-aacenc: rc $rc: $out"
    if chroot "$root" /tmp/t/enginex-aacenc --check < /dev/null; then ok "$name: enginex-aacenc opens the stock AAC encoder"
    else no "$name: enginex-aacenc --check"; fi

    rc=0; out=$(elf_loader_audit "$root" "$loader" /tmp/t/enginex-aacenc-badinterp) || rc=$?
    if [ "$rc" = 1 ] && grep -q 'program interpreter .* is not in the rootfs' <<< "$out"; then ok "$name: bad interpreter fails: $out"
    else no "$name: bad interpreter: rc $rc: $out"; fi
    # The gate's old check, ld.so --list with the base's loader, did not see it:
    if out=$(chroot "$root" "$loader" --list /tmp/t/enginex-aacenc-badinterp 2>&1) && ! grep -q 'not found' <<< "$out"; then
        ok "$name: (the old check passed the bad interpreter)"
    else no "$name: the old check unexpectedly failed the bad interpreter: $out"; fi
    if chroot "$root" /tmp/t/enginex-aacenc-badinterp --check < /dev/null 2>/dev/null; then no "$name: the bad interpreter ran"
    else ok "$name: (and the program indeed does not start)"; fi

    rc=0; out=$(elf_loader_audit "$root" "$loader" /tmp/t/needs-gone) || rc=$?
    # ld.so --list stops at the first missing library of the program itself (ldd says "not found").
    if [ "$rc" = 1 ] && grep -Eq 'libenginex-gone\.so( => not found|: cannot open shared object file)' <<< "$out"; then ok "$name: missing library fails"
    else no "$name: missing library: rc $rc: $out"; fi

    rc=0; elf_loader_audit "$root" "$loader" /tmp/t/script > /dev/null || rc=$?
    [ "$rc" = 2 ] && ok "$name: a script is skipped" || no "$name: script: rc $rc"
    umount "$root"
}
mkdir -p "$T/gen1-lower"
mount -o loop,ro "$GEN1_ROOTFS" "$T/gen1-lower"
base gen1 "$T/gen1-lower" armhf /lib/ld-linux-armhf.so.3
base gen2 "$GEN2_ROOTFS" armhf /lib/ld-linux-armhf.so.3
base az04 "$AZ04_ROOTFS" arm64 /lib/ld-linux-aarch64.so.1

# --- 2. gen2_aacenc_audit on second-generation images ---------------------------------------
V=5.3.0.6
REL=$T/release
mkdir -p "$REL" "$T/fake32" "$T/fake64"
python3 -I - "$REPO" "$T" "$V" <<'PY'
import importlib.util, io, shutil, sys, tarfile
from pathlib import Path
repo, t, v = Path(sys.argv[1]), Path(sys.argv[2]), sys.argv[3]
spec = importlib.util.spec_from_file_location('bdb', repo / 'tools/az01-packer/build-data-bundle.py')
bdb = importlib.util.module_from_spec(spec); spec.loader.exec_module(bdb)
fake = lambda m, tag: b'\x7fELF' + bytes(14) + m.to_bytes(2, 'little') + tag
for arch, m in (('32', bdb.EM_ARM), ('64', bdb.EM_AARCH64)):
    (t / f'fake{arch}/suite').write_bytes(fake(m, b'suite'))
    (t / f'fake{arch}/libengine-ui-hook.so').write_bytes(fake(m, b'hook'))
ui = t / 'packs.tar.gz'
with tarfile.open(ui, 'w:gz') as tar:
    info = tarfile.TarInfo('packs/a/Main.qml'); info.size = 7; tar.addfile(info, io.BytesIO(b'Item {}'))
cases = {  # prefix: (arch, encoder or None)
    'SCLIVE4': ('32', 'aacenc-armhf'), 'PRIME4G2': ('64', 'aacenc-arm64'),
    'MIXSTREAMPROGO': ('32', 'aacenc-armhf-badinterp'), 'SYSTEMONE': ('64', 'aacenc-arm64-badinterp'),
    'SCLIVE2': ('32', None),
}
for prefix, (arch, enc) in cases.items():
    r = bdb.build(v, t / f'fake{arch}/suite', t / f'fake{arch}/libengine-ui-hook.so', ui, t / f'out-{prefix}',
                  prefix=prefix, aacenc=(t / 'bin' / enc) if enc else None)
    shutil.copy(r['image'], t / 'release')
# A first-generation image of the same version: an AZ01 container, not a bundle.
(t / 'release' / f'SC5000-{v}-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img').write_bytes(b'AZ01' + bytes(4096))
PY
GATE_RESULTS=()
pass() { GATE_RESULTS+=("PASS $*"); }
fail() { GATE_RESULTS+=("FAIL $*"); }
note() { GATE_RESULTS+=("INFO $*"); }
check() { local name=$1; shift; if "$@" > /dev/null 2>&1; then pass "$name"; else fail "$name"; fi; }
W=$T/gate NEW_IMG=$REL/SC5000-$V-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img NEWV=$V
GATE_GEN2_ROOTFS=$GEN2_ROOTFS GATE_AZ04_ROOTFS=$AZ04_ROOTFS
mkdir -p "$W"
gen2_aacenc_audit > "$T/gate-output.txt"
printf '%s\n' "${GATE_RESULTS[@]}" | sed 's/^/  gate: /'
want=(
    "PASS gen2 SCLIVE4: libraries of bin/enginex-aacenc"
    "PASS gen2 SCLIVE4: bin/enginex-aacenc opens the stock AAC encoder"
    "PASS gen2 PRIME4G2: libraries of bin/enginex-aacenc"
    "PASS gen2 PRIME4G2: bin/enginex-aacenc opens the stock AAC encoder"
    "FAIL gen2 MIXSTREAMPROGO: libraries of bin/enginex-aacenc"
    "FAIL gen2 MIXSTREAMPROGO: bin/enginex-aacenc opens the stock AAC encoder"
    "FAIL gen2 SYSTEMONE: libraries of bin/enginex-aacenc"
    "FAIL gen2 SYSTEMONE: bin/enginex-aacenc opens the stock AAC encoder"
    "FAIL gen2 SCLIVE2: the bundle has no bin/enginex-aacenc"
)
got=$(printf '%s\n' "${GATE_RESULTS[@]}" | sort)
if [ "$got" = "$(printf '%s\n' "${want[@]}" | sort)" ]; then ok "gate: gen2_aacenc_audit gives exactly the expected PASS / FAIL lines"
else no "gate: gen2_aacenc_audit lines differ from the expected ones"; fi
grep -q 'program interpreter /lib/ld-musl-aarch64.so.1 is not in the rootfs' "$T/gate-output.txt" &&
    ok "gate: the bad AZ04 interpreter is named in the gate output" || no "gate: no interpreter reason in the output"
findmnt -rn -o TARGET | grep -q "^$W/" && no "gate: left a mount behind" || ok "gate: unmounted the stock rootfs again"
for d in "$GEN2_ROOTFS" "$AZ04_ROOTFS"; do [ ! -e "$d/tmp/gate" ] && [ ! -e "$d/tmp/t" ] || no "wrote into $d"; done

echo "test_elf_loader_audit: $FAILS failure(s)"
[ "$FAILS" = 0 ]
