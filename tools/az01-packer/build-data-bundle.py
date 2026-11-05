#!/usr/bin/env python3
"""Pack the EngineX OS user-space suite into a /data bundle (#61).

Devices whose root partition stays read-only (SC LIVE 4, inmusic,jp21) cannot
take an EngineX OS .img; they install this tarball under /data/sc5000-custom:

    tar -xzf SCLIVE4-<V>-EngineX-OS-Data-Bundle.tar.gz -C /tmp
    sh /tmp/SCLIVE4-<V>-EngineX-OS-Data-Bundle/install.sh

Run on the build host after build-release.sh's suite and UI-pack steps:

    build-data-bundle.py <version> --suite sc5000-suite --hook libengine-ui-hook.so \
        --ui-packs packs.tar.gz --out <new dir>
"""
import argparse
import gzip
import hashlib
import io
import json
import re
import sys
import tarfile
from pathlib import Path

VERSION_RE = re.compile(r'^\d+(\.\d+){2,4}$')  # same range as az01_packer.py
APPLETS = ('stream-sync', 'rb-bridge', 'sc5000-deemixd', 'stems-processor')

WRAPPER = '#!/bin/sh\nexec "$(dirname "$0")/sc5000-suite" {} "$@"\n'

INSTALL = r'''#!/bin/sh
# Installs this bundle under /data/sc5000-custom. Checks SHA256SUMS first and
# replaces each part in one rename, so a failed install leaves the old one.
set -eu
src=$(cd "$(dirname "$0")" && pwd)
dest=${SC5000_CUSTOM:-/data/sc5000-custom}
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
cp -R bin lib SHA256SUMS version.txt uninstall.sh "$stage/bundle/"
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
# Same markers as sc5000-custom-prepare: new packs get a fresh try after safe mode.
rm -f "$dest/ui/safe-mode" "$dest/ui/boot-count" "$dest/ui/boot-token"
cp version.txt "$dest/ui/.pack-version"
sha256sum < ui-packs.tar | cut -d' ' -f1 > "$dest/ui/.pack-id"
cp version.txt "$dest/version.txt"
echo "EngineX OS $(cat version.txt) installed in $dest"
'''

UNINSTALL = r'''#!/bin/sh
# Removes only what install.sh put in /data/sc5000-custom. SSH keys, settings,
# caches and downloads stay; UI packs stay if something else replaced them.
set -eu
dest=${SC5000_CUSTOM:-/data/sc5000-custom}
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
rm -rf "$dest/bundle"
echo "EngineX OS $version removed from $dest"
'''


def fail(msg):
    raise SystemExit(f'build-data-bundle: {msg}')


def ui_packs_tar(path):
    """The UI packs as a plain tar (BusyBox tar on the device), checked for packs/."""
    with tarfile.open(path) as src:
        members = src.getmembers()
        for m in members:
            parts = [p for p in m.name.split('/') if p not in ('', '.')]
            if m.name.startswith('/') or '..' in parts or not (m.isfile() or m.isdir()):
                fail(f'{path}: unsafe member {m.name!r}')
        if not any([p for p in m.name.split('/') if p not in ('', '.')][:1] == ['packs'] for m in members):
            fail(f'{path} has no packs/ directory')
        out = io.BytesIO()
        with tarfile.open(fileobj=out, mode='w', format=tarfile.PAX_FORMAT) as dst:
            for m in members:
                dst.addfile(m, src.extractfile(m) if m.isfile() else None)
    return out.getvalue()


def build(version, suite, hook, ui_packs, out, prefix='SCLIVE4'):
    if not VERSION_RE.match(version):
        fail(f'version must be 3 to 5 dot-separated numbers, got {version!r}')
    if out.exists():
        fail(f'{out} exists; refusing to overwrite')
    files = {}  # path in bundle -> (bytes, mode)
    for path, name in ((suite, 'bin/sc5000-suite'), (hook, 'lib/libengine-ui-hook.so')):
        data = Path(path).read_bytes()
        if data[:4] != b'\x7fELF':
            fail(f'{path} is not an ELF file')
        files[name] = (data, 0o755)
    for applet in APPLETS:
        files[f'bin/{applet}'] = (WRAPPER.format(applet).encode(), 0o755)
    files['ui-packs.tar'] = (ui_packs_tar(ui_packs), 0o644)
    files['version.txt'] = (f'{version}\n'.encode(), 0o644)
    files['install.sh'] = (INSTALL.encode(), 0o755)
    files['uninstall.sh'] = (UNINSTALL.encode(), 0o755)
    sums = ''.join(f'{hashlib.sha256(data).hexdigest()}  {name}\n' for name, (data, _) in sorted(files.items()))
    files['SHA256SUMS'] = (sums.encode(), 0o644)

    top = f'{prefix}-{version}-EngineX-OS-Data-Bundle'
    out.mkdir(parents=True)
    bundle = out / f'{top}.tar.gz'
    with open(bundle, 'wb') as raw, gzip.GzipFile(filename='', fileobj=raw, mode='wb', mtime=0) as gz, \
            tarfile.open(fileobj=gz, mode='w', format=tarfile.PAX_FORMAT) as tar:
        dirs = sorted({f'{top}/{n}'.rsplit('/', 1)[0] for n in files} | {top})
        for name in dirs:
            info = tarfile.TarInfo(name)
            info.type, info.mode = tarfile.DIRTYPE, 0o755
            tar.addfile(info)
        for name, (data, mode) in sorted(files.items()):
            info = tarfile.TarInfo(f'{top}/{name}')
            info.size, info.mode = len(data), mode
            tar.addfile(info, io.BytesIO(data))
    digest = hashlib.sha256(bundle.read_bytes()).hexdigest()
    (out / 'SHA256SUMS.txt').write_text(f'{digest}  {bundle.name}\n')
    return {'bundle': str(bundle), 'sha256': digest, 'files': len(files)}


def main(argv=None):
    p = argparse.ArgumentParser(description=__doc__.split('\n\n')[0])
    p.add_argument('version')
    p.add_argument('--suite', required=True, type=Path, help='static ARMv7 sc5000-suite')
    p.add_argument('--hook', required=True, type=Path, help='libengine-ui-hook.so')
    p.add_argument('--ui-packs', required=True, type=Path, help='build_packs.py packs.tar(.gz)')
    p.add_argument('--out', required=True, type=Path, help='new output directory')
    p.add_argument('--prefix', default='SCLIVE4', help='model prefix in the file name')
    a = p.parse_args(argv)
    json.dump(build(a.version, a.suite, a.hook, a.ui_packs, a.out, a.prefix), sys.stdout, indent=2)
    print()


if __name__ == '__main__':
    main()
