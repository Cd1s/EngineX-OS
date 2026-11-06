#!/usr/bin/env python3
"""Inject EngineX OS data bundle into an ext4 userdata image using debugfs (#61).

Method A (Host-side non-destructive installation):
Injects the bundle and ALSA boot hook into an existing ext4 userdata partition
image without wiping user files, music, settings, or database.

Usage:
    host-install-userdata.py <userdata.img> --bundle <bundle.tar.gz>
"""
import argparse
import hashlib
import io
import os
import re
import subprocess
import sys
import tarfile
import tempfile
from pathlib import Path

# On macOS, Homebrew installs e2fsprogs keg-only (not symlinked to PATH).
if sys.platform == 'darwin':
    extra_paths = [
        '/opt/homebrew/opt/e2fsprogs/sbin', '/opt/homebrew/opt/e2fsprogs/bin',
        '/usr/local/opt/e2fsprogs/sbin', '/usr/local/opt/e2fsprogs/bin',
    ]
    cur_path = os.environ.get('PATH', '')
    to_add = [p for p in extra_paths if os.path.isdir(p) and p not in cur_path]
    if to_add:
        os.environ['PATH'] = ':'.join(to_add) + ':' + cur_path


ALSA_DIRS = ('system/var/overlay/lib/alsa/conf.d', 'system/var-lib/alsa/conf.d')


def fail(msg):
    raise SystemExit(f'host-install-userdata: {msg}')


def image_kind(img, rel):
    """'directory', 'symlink', 'regular' or None for /rel inside the image."""
    out = subprocess.run(['debugfs', '-R', f'stat /{rel}', str(img)], capture_output=True, text=True).stdout
    m = re.search(r'Type: (\w+)', out)
    return m.group(1) if m else None


def generate_debugfs_script(files_to_write, dirs_to_create, links=()):
    lines = []
    # Create directories in hierarchical order
    for d in sorted(dirs_to_create, key=lambda x: (len(x.parts), x)):
        target = '/' + str(d).replace('\\', '/').strip('/')
        lines.append(f'mkdir {target}')
    # Write files
    for src_path, target_rel, mode in files_to_write:
        target = '/' + str(target_rel).replace('\\', '/').strip('/')
        lines.append(f'rm {target}')
        lines.append(f'write {src_path} {target}')
        lines.append(f'set_inode_field {target} mode 0{mode:o}')
    for path, target in links:
        lines.append(f'symlink {path} {target}')
    return '\n'.join(lines) + '\n'


def inject(img_path, bundle_tar, check=True):
    img = Path(img_path)
    bundle = Path(bundle_tar)
    if not img.is_file():
        fail(f'image not found: {img}')
    if not bundle.is_file():
        fail(f'bundle not found: {bundle}')

    with tempfile.TemporaryDirectory(prefix='host-install-') as tmpdir:
        tmp = Path(tmpdir)
        filter_kw = {'filter': 'data'} if hasattr(tarfile, 'data_filter') else {}
        with tarfile.open(bundle) as tar:
            tar.extractall(tmp, **filter_kw)

        bundle_roots = [p for p in tmp.iterdir() if p.is_dir() and 'EngineX-OS-Data-Bundle' in p.name]
        if not bundle_roots:
            fail(f'{bundle} does not contain an EngineX-OS-Data-Bundle directory')
        broot = bundle_roots[0]

        # #103: runtime dir is enginex, with sc5000-custom -> enginex for older code. A deck
        # that still has the real legacy dir keeps being written there: the first boot renames it.
        legacy, new = image_kind(img, 'sc5000-custom'), image_kind(img, 'enginex')
        root = 'sc5000-custom' if legacy == 'directory' and new is None else 'enginex'
        links = [('/sc5000-custom', 'enginex')] if legacy is None and root == 'enginex' else []

        sums_file = broot / 'SHA256SUMS'
        if not sums_file.is_file():
            fail('SHA256SUMS missing in bundle')
        for line in sums_file.read_text().splitlines():
            line = line.strip()
            if not line:
                continue
            parts = line.split(maxsplit=1)
            if len(parts) != 2:
                continue
            sha, rel = parts
            target_f = broot / rel
            if not target_f.is_file():
                fail(f'corrupted bundle: {rel} missing')
            h = hashlib.sha256(target_f.read_bytes()).hexdigest()
            if h != sha:
                fail(f'corrupted bundle: {rel} SHA256 mismatch')

        ui_packs_tar = broot / 'ui-packs.tar'
        ui_stage = tmp / 'ui_stage'
        ui_stage.mkdir()
        with tarfile.open(ui_packs_tar) as tar:
            tar.extractall(ui_stage, **filter_kw)

        version = (broot / 'version.txt').read_text().strip()
        pack_id = hashlib.sha256(ui_packs_tar.read_bytes()).hexdigest()

        dirs = {
            Path(root),
            Path(root) / 'bundle',
            Path(root) / 'bundle/bin',
            Path(root) / 'bundle/lib',
            Path(root) / 'ui',
        }
        # Bundles without alsa-dir predate AZ04 (#76) and all used the overlay path.
        alsa_file = broot / 'alsa-dir'
        alsa_rel = Path(alsa_file.read_text().strip() if alsa_file.is_file() else 'system/var/overlay/lib/alsa/conf.d')
        if alsa_rel.is_absolute() or '..' in alsa_rel.parts:
            fail(f'unsafe alsa-dir {alsa_rel}')
        # The image is for a deck we may not be able to name (fastboot gives a board code,
        # not which var-lib it mounts), so the hook goes where either firmware reads it.
        alsa_rels = [alsa_rel] + [Path(d) for d in ALSA_DIRS if Path(d) != alsa_rel]
        dirs.update(alsa_rels)

        files = []

        # 1. Bundle contents
        if (broot / 'bin').is_dir():
            for p in (broot / 'bin').iterdir():
                if p.is_file():
                    files.append((p, Path(root) / 'bundle/bin' / p.name, 0o100755))

        if (broot / 'lib').is_dir():
            for p in (broot / 'lib').iterdir():
                if p.is_file():
                    files.append((p, Path(root) / 'bundle/lib' / p.name, 0o100755))

        for name, mode in [
            ('boot-activate.sh', 0o100755),
            ('99-sc5000-custom.conf', 0o100644),
            ('uninstall.sh', 0o100755),
            ('version.txt', 0o100644),
            ('alsa-dir', 0o100644),
            ('SHA256SUMS', 0o100644),
        ]:
            p = broot / name
            if p.is_file():
                files.append((p, Path(root) / 'bundle' / name, mode))

        # 2. UI packs and icons
        for root_item in ui_stage.rglob('*'):
            rel = root_item.relative_to(ui_stage)
            target_p = Path(root) / 'ui' / rel
            if root_item.is_dir():
                dirs.add(target_p)
            elif root_item.is_file():
                dirs.add(target_p.parent)
                files.append((root_item, target_p, 0o100644))

        pack_ver_f = tmp / '.pack-version'
        pack_ver_f.write_text(f'{version}\n')
        files.append((pack_ver_f, Path(root) / 'ui/.pack-version', 0o100644))

        pack_id_f = tmp / '.pack-id'
        pack_id_f.write_text(f'{pack_id}\n')
        files.append((pack_id_f, Path(root) / 'ui/.pack-id', 0o100644))

        ver_top_f = tmp / 'ver_top.txt'
        ver_top_f.write_text(f'{version}\n')
        files.append((ver_top_f, Path(root) / 'version.txt', 0o100644))

        # 3. ALSA hook configuration
        alsa_conf_f = broot / '99-sc5000-custom.conf'
        if alsa_conf_f.is_file():
            files.extend((alsa_conf_f, d / '99-sc5000-custom.conf', 0o100644) for d in alsa_rels)

        all_dirs = set(dirs)
        for d in dirs:
            for parent in d.parents:
                if str(parent) != '.':
                    all_dirs.add(parent)

        script = generate_debugfs_script(files, all_dirs, links)
        script_file = tmp / 'debugfs_script.txt'
        script_file.write_text(script)

        subprocess.run(['debugfs', '-w', '-f', str(script_file), str(img)],
                       capture_output=True, text=True)
        # debugfs exits 0 even when a write failed (full image, bad path): read back.
        for rel in (f'{root}/version.txt', f'{root}/bundle/bin/sc5000-suite'):
            st = subprocess.run(['debugfs', '-R', f'stat /{rel}', str(img)], capture_output=True, text=True).stdout
            if 'Type: regular' not in st:
                fail(f'/{rel} is not in {img} after writing (image full?)')
        if check:
            fsck = subprocess.run(['e2fsck', '-f', '-y', str(img)],
                                  capture_output=True, text=True)
            if fsck.returncode not in (0, 1, 2):
                fail(f'e2fsck failed with code {fsck.returncode}:\n{fsck.stdout}\n{fsck.stderr}')

        return {'status': 'ok', 'version': version, 'files_injected': len(files)}


def main(argv=None):
    p = argparse.ArgumentParser(description=__doc__.split('\n\n')[0])
    p.add_argument('image', help='ext4 userdata image to inject into')
    p.add_argument('--bundle', required=True, help='SCLIVE4 data bundle tar.gz')
    p.add_argument('--no-fsck', action='store_true', help='skip e2fsck check')
    a = p.parse_args(argv)
    r = inject(a.image, a.bundle, check=not a.no_fsck)
    print(f"Successfully injected EngineX OS {r['version']} ({r['files_injected']} files) into {a.image}")


if __name__ == '__main__':
    main()
