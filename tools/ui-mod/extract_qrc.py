#!/usr/bin/env python3
"""Extract the QML/asset tree Engine embeds with rcc, straight from an official image.

Build host only (needs debugfs, qemu-arm and the armhf preload built from qrc_dump.c;
for a 64-bit AZ04 Engine, qemu-aarch64 and qrc_dump.c built for aarch64):
    python3 -I extract_qrc.py --image <PREFIX>-<ver>-Update.img --preload qrc_dump.so --out <new dir>
    python3 -I extract_qrc.py --tree <unpacked rootfs> --preload qrc_dump.so --out <new dir>

Engine runs its static initialisers under qemu-arm as an unprivileged user, so every
qRegisterResourceData call is captured exactly as Engine sees it; it then aborts on
the missing hardware config. Nothing from the image runs natively on the host.
"""
import argparse
import json
import os
import shutil
import struct
import subprocess
import sys
import tempfile
import zlib
from pathlib import Path

FLAG_COMPRESSED, FLAG_DIRECTORY, FLAG_ZSTD = 1, 2, 4


def be32(b, o):
    return struct.unpack_from('>I', b, o)[0]


def be16(b, o):
    return struct.unpack_from('>H', b, o)[0]


def decode(blob):
    """Files from one res-<n>.bin as {path: bytes}."""
    version, tree_len, names_len, data_len = struct.unpack_from('<4I', blob)
    tree = blob[16:16 + tree_len]
    names = blob[16 + tree_len:16 + tree_len + names_len]
    data = blob[16 + tree_len + names_len:16 + tree_len + names_len + data_len]
    if len(data) != data_len:
        raise ValueError('truncated dump')
    node = 22 if version >= 2 else 14
    files = {}

    def name(off):
        n = be16(names, off)
        return names[off + 6:off + 6 + 2 * n].decode('utf-16-be')

    def walk(n, prefix, depth):
        e = n * node
        flags = be16(tree, e + 4)
        path = prefix if n == 0 else prefix + '/' + name(be32(tree, e))
        if flags & FLAG_DIRECTORY:
            if depth > 64:
                raise ValueError('resource tree too deep')
            for i in range(be32(tree, e + 6)):
                walk(be32(tree, e + 10) + i, path, depth + 1)
            return
        off = be32(tree, e + 10)
        raw = data[off + 4:off + 4 + be32(data, off)]
        if flags & FLAG_COMPRESSED:
            raw = zlib.decompress(raw[4:])  # qCompress: be32 length, then zlib
        elif flags & FLAG_ZSTD:
            raw = subprocess.run(['zstd', '-dcq'], input=raw, capture_output=True, check=True).stdout
        files[path.lstrip('/')] = raw

    walk(0, '', 0)
    return files


def safe_rel(path):
    p = Path(path)
    if p.is_absolute() or not path or any(part in ('', '.', '..') for part in p.parts):
        raise ValueError('unsafe resource path: %r' % path)
    return p


def run_engine(tree, preload, dump):
    engine = tree / 'usr/Engine/Engine'
    if not engine.is_file():
        raise SystemExit('no usr/Engine/Engine in ' + str(tree))
    os.chmod(dump, 0o777)
    env = {'PATH': '/usr/bin:/bin', 'HOME': str(dump), 'QT_QPA_PLATFORM': 'offscreen'}
    with open(engine, 'rb') as f:
        aarch64 = f.read(20)[18:20] == b'\xb7\x00'  # EM_AARCH64: AZ04 (PRIME 4 G2, SYSTEM ONE)
    cmd = ['qemu-aarch64' if aarch64 else 'qemu-arm', '-L', str(tree), '-E', 'LD_PRELOAD=' + str(preload),
           '-E', 'QRC_DUMP_DIR=' + str(dump), '-E', 'QT_QPA_PLATFORM=offscreen', str(engine), '--help']
    if os.geteuid() == 0:
        cmd = ['setpriv', '--reuid=nobody', '--regid=nogroup', '--clear-groups', '--'] + cmd
    # Engine aborts once it finds no product config; resources are registered before that.
    r = subprocess.run(cmd, cwd=dump, env=env, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, timeout=120)
    return r.stderr.decode(errors='replace')[-2000:]


def unpack_tree(image, work):
    packer = Path(__file__).resolve().parent.parent / 'az01-packer/az01_packer.py'
    out = work / 'unpacked'
    subprocess.run([sys.executable, '-I', str(packer), 'unpack', str(image), '--output', str(out)],
                   check=True, stdout=subprocess.DEVNULL)
    tree = work / 'tree'
    tree.mkdir()
    # The whole root: 5.3.0 Engine stops before registering its QML without /etc and /usr/share.
    subprocess.run(['debugfs', '-R', 'rdump / %s' % tree, str(out / 'rootfs.ext2')],
                   check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    (out / 'rootfs.ext2').unlink()
    return tree


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    src = ap.add_mutually_exclusive_group(required=True)
    src.add_argument('--image', type=Path, help='official AZ01 update image')
    src.add_argument('--tree', type=Path, help='already unpacked rootfs tree')
    ap.add_argument('--preload', type=Path, required=True, help='qrc_dump.so for the Engine\'s architecture')
    ap.add_argument('--out', type=Path, required=True, help='new output directory')
    a = ap.parse_args(argv)
    if a.out.exists():
        raise SystemExit('%s exists; refusing to overwrite' % a.out)
    with tempfile.TemporaryDirectory() as tmp:
        work = Path(tmp)
        os.chmod(work, 0o755)
        tree = unpack_tree(a.image.resolve(), work) if a.image else a.tree.resolve()
        preload = work / 'qrc_dump.so'
        shutil.copy(a.preload, preload)
        os.chmod(preload, 0o755)
        dump = work / 'dump'
        dump.mkdir()
        log = run_engine(tree, preload, dump)
        files = {}
        for res in sorted(dump.glob('res-*.bin')):
            for path, body in decode(res.read_bytes()).items():
                if path in files and files[path] != body:
                    raise SystemExit('conflicting resource ' + path)
                files[path] = body
        # 5.0.4 names the deck skin Skins/EmbeddedCommon, 5.3.0 Skins/Embedded.
        if not any(p.startswith('Skins/Embedded') and p.endswith('.qml') for p in files):
            raise SystemExit('no Skins/Embedded* QML captured; Engine did not register its QML:\n' + log)
        for path, body in files.items():
            dest = a.out / safe_rel(path)
            dest.parent.mkdir(parents=True, exist_ok=True)
            dest.write_bytes(body)
    print(json.dumps({'out': str(a.out), 'files': len(files)}))


if __name__ == '__main__':
    main()
