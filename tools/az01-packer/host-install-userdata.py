#!/usr/bin/env python3
"""Inject EngineX OS data bundle into an ext4 userdata image using debugfs (#61).

Method A (Host-side non-destructive installation):
Injects the bundle and ALSA boot hook into an existing ext4 userdata partition
image without wiping user files, music, settings, or database.

Usage:
    host-install-userdata.py <userdata.img> --bundle <bundle.tar.gz> [--ssh-key ~/.ssh/id_ed25519.pub]
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


def unwrap_bundle_or_img(bundle: Path) -> bytes:
    data = bundle.read_bytes()
    if data[:19] == b'ENGINEX_DATA_BUNDLE':
        magic = b'ENGINEX_DATA_BUNDLE_IMG_V1\n'
        if len(data) <= 128 or data[:len(magic)] != magic or data[len(magic) + 64:len(magic) + 65] != b'\n':
            fail(f'{bundle}: invalid or truncated ENGINEX_DATA_BUNDLE_IMG_V1 header')
        want = data[len(magic):len(magic) + 64].decode('ascii', errors='replace').lower()
        got = hashlib.sha256(data[128:]).hexdigest()
        if got != want:
            fail(f'{bundle}: payload SHA-256 mismatch (got {got}, header says {want})')
        return data[128:]
    return data


def generate_debugfs_script(files_to_write, dirs_to_create, links=(), removes=()):
    lines = []
    # Create directories in hierarchical order
    for d in sorted(dirs_to_create, key=lambda x: (len(x.parts), x)):
        target = '/' + str(d).replace('\\', '/').strip('/')
        lines.append(f'mkdir {target}')
    for rel in removes:
        target = '/' + str(rel).replace('\\', '/').strip('/')
        lines.append(f'rm {target}')
    # Write files
    for src_path, target_rel, mode in files_to_write:
        target = '/' + str(target_rel).replace('\\', '/').strip('/')
        lines.append(f'rm {target}')
        lines.append(f'write "{src_path}" "{target}"')
        lines.append(f'set_inode_field {target} mode 0{mode:o}')
    for path, target in links:
        lines.append(f'symlink {path} {target}')
    return '\n'.join(lines) + '\n'


def _sha256(path):
    h = hashlib.sha256()
    with open(path, 'rb') as f:
        for block in iter(lambda: f.read(1 << 20), b''):
            h.update(block)
    return h.hexdigest()


# debugfs noise of a normal run: the banner, rm of a file that is not there yet, mkdir of a dir that is.
_BENIGN = re.compile(r'^debugfs \d|File not found by ext2_lookup|already exists|^\s*$')


def read_back(img, files, links, tmp):
    """BH-G2-3: debugfs exits 0 when a write failed, so compare every installed file with its source
    (SHA-256 and mode) and every link with its target; return what is wrong."""
    out = tmp / 'readback'
    out.mkdir()
    script = tmp / 'readback.txt'
    lines = []
    for i, (_, target, _) in enumerate(files):
        lines.append(f'dump -p "/{str(target).replace(os.sep, "/").strip("/")}" "{out / str(i)}"\n')
    script.write_text(''.join(lines))
    subprocess.run(['debugfs', '-f', str(script), str(img)], capture_output=True, text=True)
    bad = []
    for i, (src, target, mode) in enumerate(files):
        got = out / str(i)
        if not got.is_file():
            bad.append(f'/{target} is missing')
        elif _sha256(got) != _sha256(src):
            bad.append(f'/{target} differs from the bundle ({got.stat().st_size} of {Path(src).stat().st_size} bytes)')
        elif got.stat().st_mode & 0o7777 != mode & 0o7777:
            bad.append(f'/{target} has mode {got.stat().st_mode & 0o7777:04o}, not {mode & 0o7777:04o}')
    for path, target in links:
        st = subprocess.run(['debugfs', '-R', f'stat {path}', str(img)], capture_output=True, text=True).stdout
        if 'Type: symlink' not in st or f'dest: "{target}"' not in st:
            bad.append(f'{path} is not a link to {target}')
    return bad


def inject(img_path, bundle_tar, check=True, ssh_key=None):
    img = Path(img_path)
    bundle = Path(bundle_tar)
    if ssh_key is not None and (not Path(ssh_key).is_file() or not Path(ssh_key).read_bytes().strip()):
        fail(f'SSH public key missing or empty: {ssh_key}')
    if not img.is_file():
        fail(f'image not found: {img}')
    if not bundle.is_file():
        fail(f'bundle not found: {bundle}')

    with tempfile.TemporaryDirectory(prefix='host-install-') as tmpdir:
        tmp = Path(tmpdir)
        filter_kw = {'filter': 'data'} if hasattr(tarfile, 'data_filter') else {}
        with tarfile.open(fileobj=io.BytesIO(unwrap_bundle_or_img(bundle))) as tar:
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
            ('install.sh', 0o100755),
            ('uninstall.sh', 0o100755),
            ('version.txt', 0o100644),
            ('release-tag', 0o100644),
            ('model', 0o100644),
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

        # Opens the *-Update.img upgrade path on the deck (#119).
        unlock_f = tmp / 'gen2-unlocked'
        unlock_f.write_text('1\n')
        files.append((unlock_f, Path(root) / 'gen2-unlocked', 0o100644))

        # boot-activate.sh hands this to root's sshd (#116).
        if ssh_key is not None:
            files.append((Path(ssh_key), Path(root) / 'authorized_keys', 0o100600))

        # 3. ALSA hook configuration
        alsa_conf_f = broot / '99-sc5000-custom.conf'
        if alsa_conf_f.is_file():
            files.extend((alsa_conf_f, d / '99-sc5000-custom.conf', 0o100644) for d in alsa_rels)

        all_dirs = set(dirs)
        for d in dirs:
            for parent in d.parents:
                if str(parent) != '.':
                    all_dirs.add(parent)

        script = generate_debugfs_script(files, all_dirs, links, removes=(Path(root) / 'gen2-locked',))
        script_file = tmp / 'debugfs_script.txt'
        script_file.write_text(script)

        wr = subprocess.run(['debugfs', '-w', '-f', str(script_file), str(img)],
                            capture_output=True, text=True)
        notes = [ln for ln in (wr.stdout + wr.stderr).splitlines() if not _BENIGN.search(ln)]
        if wr.returncode != 0:
            fail(f'debugfs failed with code {wr.returncode} on {img}:\n' + '\n'.join(notes[-20:]))
        # debugfs exits 0 even when a write failed (full image, bad path): read back.
        for rel in (f'{root}/version.txt', f'{root}/bundle/bin/sc5000-suite', f'{root}/gen2-unlocked'):
            st = subprocess.run(['debugfs', '-R', f'stat /{rel}', str(img)], capture_output=True, text=True).stdout
            if 'Type: regular' not in st:
                fail(f'/{rel} is not in {img} after writing (image full?)')
        unlocked_val = subprocess.run(['debugfs', '-R', f'cat /{root}/gen2-unlocked', str(img)], capture_output=True, text=True).stdout
        if unlocked_val != '1\n':
            fail(f'/{root}/gen2-unlocked did not read back as 1 in {img}')
        locked_st = subprocess.run(['debugfs', '-R', f'stat /{root}/gen2-locked', str(img)], capture_output=True, text=True).stdout
        if 'Type:' in locked_st:
            fail(f'/{root}/gen2-locked still present in {img} after unlock')
        if check:
            fsck = subprocess.run(['e2fsck', '-f', '-y', str(img)],
                                  capture_output=True, text=True)
            if fsck.returncode not in (0, 1, 2):
                fail(f'e2fsck failed with code {fsck.returncode}:\n{fsck.stdout}\n{fsck.stderr}')
        # After e2fsck, which may have dropped a damaged file: only a complete image is flashed.
        bad = read_back(img, files, links, tmp)
        if bad:
            fail(f'{img} is incomplete after writing (image full?): ' + '; '.join(bad[:10]) +
                 (f' (+{len(bad) - 10} more)' if len(bad) > 10 else '') +
                 ('\ndebugfs said:\n' + '\n'.join(notes[-20:]) if notes else ''))

        return {'status': 'ok', 'version': version, 'files_injected': len(files)}


def main(argv=None):
    p = argparse.ArgumentParser(description=__doc__.split('\n\n')[0])
    p.add_argument('image', help='ext4 userdata image to inject into')
    p.add_argument('--bundle', required=True, help='SCLIVE4 data bundle tar.gz')
    p.add_argument('--ssh-key', help='SSH public key (.pub) for root, started on boot')
    p.add_argument('--no-fsck', action='store_true', help='skip e2fsck check')
    a = p.parse_args(argv)
    r = inject(a.image, a.bundle, check=not a.no_fsck, ssh_key=a.ssh_key)
    print(f"Successfully injected EngineX OS {r['version']} ({r['files_injected']} files) into {a.image}")


if __name__ == '__main__':
    main()
