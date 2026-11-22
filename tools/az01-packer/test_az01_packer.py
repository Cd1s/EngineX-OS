"""Synthetic hostile containers plus real offline ext2 integration (no mounts)."""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parents[2] / 'tools/az01-packer'))
import legacy_names as legacy
import base64
import hashlib
import io
import lzma
import os
import re
from pathlib import Path
import shutil
import stat
import struct
import subprocess
import tempfile
import unittest
import unittest.mock

import az01_packer as az

# Firmware fixtures are read-only shared inputs. Keep disposable image writes
# in the caller's sandbox (or the ordinary system temporary directory).
TEST_TMPDIR = os.environ.get('ENGINEX_TEST_TMPDIR')
# Official 5.3.0 images of the three first-generation image groups, as kept in the build host's lab.
LAB = Path(os.environ.get('ENGINEX_LAB_DIR', legacy.LAB))
STOCK_530 = [LAB/'fw-5.3.0'/f'{name}-5.3.0-Update.img' for name in ('SC5000', 'SC6000', 'PRIME4')]
GUARDED_UPDATE_FILE = (b'UPDATE_FILE=$(/etc/enginex/engine-ui-guard update-file >/dev/null 2>&1; '
                       b'cat /tmp/update-file-path)\n')


def string(value):
    b = value.encode()
    return struct.pack('<I', len(b)) + b + bytes(1 + (-(len(b)+1)) % 4)


def fixture(payload=b'rootfs', version=1, name='rootfs'):
    header = (string('test-build') + struct.pack('<I', 1) + string('inmusic,jp07') +
              struct.pack('<II', 1, 0x15e4d006) + string('test'))
    data = b'AZ01' + struct.pack('<II', version, 12+len(header)) + header
    for tag, label, comp, blob in ((b'PARTP', 'splash', 'none', b'S'),
                                  (b'PARTX', 'recoverysplash', 'none', b'R'),
                                  (b'PARTL', name, 'xz', payload)):
        data += tag + bytes(3) + struct.pack('<Q', len(blob))
        data += string(label) + string(comp) + struct.pack('<I', 1)
        data += string('sha1') + struct.pack('<I', 20) + hashlib.sha1(blob).digest() + blob
    return data + az.Image.EOF


class ContainerTests(unittest.TestCase):
    def test_info_and_resize_preserve_every_nonrootfs_byte(self):
        im = az.Image(fixture())
        self.assertEqual(im.replace_rootfs(im.payload()), im.data)
        changed = az.Image(im.replace_rootfs(b'a different length'))
        self.assertEqual(changed.payload(), b'a different length')
        self.assertEqual(changed.data[:im.parts[-1]['entry']], im.data[:im.parts[-1]['entry']])
        self.assertEqual(changed.payload('splash'), b'S')
        self.assertEqual(changed.info()['compatible'], ['inmusic,jp07'])

    def test_replace_partition_splash_and_recoverysplash(self):
        im = az.Image(fixture())
        splash_payload = b'S' * 4096000
        rec_payload = b'R' * 4096000

        # Replace splash
        im_splash = az.Image(im.replace_partition('splash', splash_payload))
        self.assertEqual(im_splash.payload('splash'), splash_payload)
        self.assertEqual(im_splash.payload('recoverysplash'), b'R')
        self.assertEqual(im_splash.payload('rootfs'), b'rootfs')
        self.assertEqual(im_splash.parts[0]['sha1'], hashlib.sha1(splash_payload).hexdigest())

        # Replace recoverysplash
        im_rec = az.Image(im_splash.replace_partition('recoverysplash', rec_payload))
        self.assertEqual(im_rec.payload('splash'), splash_payload)
        self.assertEqual(im_rec.payload('recoverysplash'), rec_payload)
        self.assertEqual(im_rec.payload('rootfs'), b'rootfs')
        self.assertEqual(im_rec.parts[1]['sha1'], hashlib.sha1(rec_payload).hexdigest())

        # Bad size / compressed / invalid partition
        with self.assertRaises(az.SafetyError):
            im.replace_partition('splash', b'too-short')
        with self.assertRaises(az.SafetyError):
            im.replace_partition('recoverysplash', b'\xfd7zXZ\0' + bytes(4095994))
        with self.assertRaises(az.SafetyError):
            im.replace_partition('unknown_part', b'X' * 4096000)

    def test_models_and_aligned_trailer(self):
        im = az.Image(fixture())
        self.assertEqual(im.info()['models'], [{'code': 'JP07', 'name': 'SC5000'}])
        # SC6000 / PRIME 4 pad the last record to 8 bytes before EOF (#32).
        padded = fixture(payload=b'rootfs0')
        body = padded[:-16]
        pad = (-len(body)) % 8
        if pad:
            im = az.Image(body + bytes(pad) + az.Image.EOF)
            self.assertTrue(im.aligned)
            self.assertEqual(len(im.replace_rootfs(b'odd')) % 8, 0)
        with self.assertRaises(az.SafetyError):
            az.Image(fixture().replace(b'inmusic,jp07', b'inmusic,xx99'))
        with self.assertRaises(az.SafetyError):
            az.Image(b'AZ0x' + fixture()[4:])

    def test_truncation_every_boundary(self):
        data = fixture()
        for n in range(len(data)):
            with self.subTest(length=n), self.assertRaises(az.SafetyError):
                az.Image(data[:n])

    def test_corrupt_hash_header_trailer_unknown_version(self):
        good = fixture()
        im = az.Image(good)
        for index in (im.parts[-1]['offset'], im.parts[0]['digest_at'], len(good)-1, 8):
            bad = bytearray(good)
            bad[index] ^= 1
            with self.subTest(index=index), self.assertRaises(az.SafetyError):
                az.Image(bytes(bad))
        for bad in (good+b'extra signature', fixture(version=2), fixture(version=3),
                    b'\xd0\x0d\xfe\xed'+good[4:], fixture(name='uboot'), fixture(name='../rootfs')):
            with self.assertRaises(az.SafetyError):
                az.Image(bad)

    def test_paths_protected_and_noncanonical(self):
        for path in ('/usr/Engine/Firmware/JP08 Motor/mcu.bin', '/boot/zImage',
                     '/uboot', '/u-boot.bin', '/spl.bin', '/data/env', '/data/env.bin',
                     '/data/boot.env', '/root/idbloader.img', '/MLO', '/tmp/trust.img',
                     '/etc/../boot/x', '/etc//x', '/etc/x\nwrite bad', '/etc/"x', 'relative'):
            with self.subTest(path=path), self.assertRaises(az.SafetyError):
                az.writable(path)
        az.writable('/usr/libexec/enginex-prepare')

    def test_usb_id_header_and_invalid_counts(self):
        im = az.Image(fixture())
        self.assertEqual(im.usb_devices, [0x15e4d006])
        data = bytearray(fixture())
        # The USB count follows the build string, compatible count and string.
        r = az.Reader(data, 12)
        r.string()
        r.u32()
        r.string()
        struct.pack_into('<I', data, r.pos, 0xffffffff)
        with self.assertRaises(az.SafetyError):
            az.Image(data)

    def test_bounded_xz_rejects_trailing_truncated_and_nonxz(self):
        payload = lzma.compress(b'x' * (3*1024**2))
        out = io.BytesIO()
        az.inflate(payload, out)
        self.assertEqual(len(out.getvalue()), 3*1024**2)
        for bad in (payload+b'junk', payload+payload, payload[:-1], b'notxz'):
            with self.assertRaises((az.SafetyError, lzma.LZMAError)):
                az.inflate(bad, io.BytesIO())
        limit = az.MAX_ROOTFS
        try:
            az.MAX_ROOTFS = 100
            with self.assertRaises(az.SafetyError):
                az.inflate(payload, io.BytesIO())
        finally:
            az.MAX_ROOTFS = limit

    def test_recompression_preserves_original_xz_integrity_check(self):
        for check in (lzma.CHECK_CRC32, lzma.CHECK_CRC64):
            source = lzma.compress(b'original', check=check)
            changed = az.compress_rootfs(b'modified', source)
            self.assertEqual(changed[7] & 15, check)
            self.assertEqual(lzma.decompress(changed), b'modified')

    def test_no_overwrite_and_no_git_binary_output(self):
        with tempfile.TemporaryDirectory(dir=TEST_TMPDIR) as tmp:
            root = Path(tmp)
            (root/'existing').write_text('keep')
            with self.assertRaises(az.SafetyError):
                az.new_output(root/'existing')
            (root/'.git').write_text('gitdir: something')
            with self.assertRaises(az.SafetyError):
                az.new_output(root/'firmware.img')

    def test_parent_symlink_cannot_hide_git_worktree(self):
        with tempfile.TemporaryDirectory(dir=TEST_TMPDIR) as tmp:
            root = Path(tmp)
            tree = root/'repo'
            (tree/'nested').mkdir(parents=True)
            (tree/'.git').write_text('gitdir: elsewhere')
            (root/'alias').symlink_to(tree/'nested')
            with self.assertRaises(az.SafetyError):
                az.new_output(root/'alias'/'rootfs.ext2')
            with self.assertRaises(az.SafetyError):
                az.new_output(root/'bad"name.img')


@unittest.skipUnless(all(shutil.which(x) for x in ('debugfs', 'mke2fs', 'e2fsck',
                                                  'ssh-keygen', 'openssl')),
                     'offline ext2 integration requires e2fsprogs/OpenSSH/OpenSSL')
class FilesystemTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix='az01-tests-', dir=TEST_TMPDIR)
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.disk = self.root/'test.ext2'
        with self.disk.open('wb') as f:
            f.truncate(16*1024**2)
        subprocess.run(['mke2fs', '-q', '-t', 'ext2', '-F', str(self.disk)], check=True,
                       capture_output=True)
        self.fs = az.ExtFS(self.disk)
        self.fs.write('/etc/shadow', b'root:!:1:0:99999:7:::\n')
        # Test fixtures deliberately bypass the policy to seed protected files.
        self.fs.cmd('mkdir /usr', True)
        self.fs.cmd('mkdir /usr/Engine', True)
        self.fs.cmd('mkdir /usr/Engine/Firmware', True)
        seed = self.root/'mcu'
        seed.write_bytes(b'MCU golden bytes')
        self.fs.cmd(f'write "{seed}" /usr/Engine/Firmware/motor.bin', True)
        self.fs.cmd('mkdir /boot', True)
        self.fs.cmd(f'write "{seed}" /boot/uboot', True)
        self.key = self.root/'id_ed25519'
        subprocess.run(['ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-f', str(self.key)],
                       check=True, capture_output=True)

    def test_key_validation_rejects_options_private_and_truncated(self):
        key = self.key.with_suffix('.pub').read_text()
        self.assertTrue(az.public_key(key).startswith(b'ssh-ed25519 '))
        for bad in (self.key.read_text(), 'command="id" '+key, key+'\n'+key,
                    'ssh-ed25519 '+base64.b64encode(struct.pack('>I', 11)+b'ssh-ed25519').decode()):
            with self.assertRaises(az.SafetyError):
                az.public_key(bad)

    def test_inject_enablement_shadow_and_persistent_host_keys(self):
        before = self.fs.snapshot()
        az.inject(self.fs, az.public_key(self.key.with_suffix('.pub').read_text()))
        self.assertEqual(self.fs.snapshot(), before)
        self.assertIn(b'AuthorizedKeysFile /data/enginex/',
                      self.fs.read('/usr/share/enginex/sshd_config'))
        self.assertIn(b'root:$6$', self.fs.read('/etc/shadow'))
        prepare = self.fs.read('/usr/lib/systemd/system/enginex-prepare.service')
        self.assertIn(b'RequiresMountsFor=/data /etc /var\n', prepare)
        self.assertNotIn(b'Requires=etc.mount', prepare)  # 5.3.0 has no etc.mount
        # After a factory reset /data is blank: the UI packs must exist before Engine starts.
        self.assertIn(b'Before=enginex-sshd.service engine.service stream-sync.service', prepare)
        self.assertIn('enginex.target', self.fs.entries(
            '/usr/lib/systemd/system/multi-user.target.wants'))
        self.assertIn('enginex-etc-reset.service', self.fs.entries(
            '/usr/lib/systemd/system/etc.mount.wants'))
        self.assertIn(b'Before=etc.mount',
                      self.fs.read('/usr/lib/systemd/system/enginex-etc-reset.service'))
        az.check_fs(self.disk)

    def test_prepare_installs_ui_packs_once_per_version(self):
        import io, lzma, tarfile
        seed = self.root/'seed-ui'
        seed.mkdir()
        (seed/'authorized_keys').write_bytes(self.key.with_suffix('.pub').read_bytes())
        buf = io.BytesIO()
        with tarfile.open(fileobj=buf, mode='w') as t:
            for name, data in (('packs/en/Main.qml', b'new'), ('icons/deezer.png', b'png')):
                info = tarfile.TarInfo(name)
                info.size = len(data)
                t.addfile(info, io.BytesIO(data))
        (seed/'ui-packs.tar.xz').write_bytes(lzma.compress(buf.getvalue(), check=lzma.CHECK_CRC32))
        version = self.root/'version.txt'
        version.write_text('5.0.4.1\n')
        data = self.root/'data-ui'
        ui = data/'enginex/ui'
        (ui/'packs/en').mkdir(parents=True)
        (ui/'packs/en/Main.qml').write_text('old')
        (ui/'packs.dev').mkdir()
        (ui/'safe-mode').write_text('')
        (ui/'boot-count').write_text('3')
        script = az.PREPARE.replace(b'/data', str(data).encode()).replace(
            b'/usr/share/enginex', str(seed).encode()).replace(
            b'/etc/enginex/version.txt', str(version).encode())
        passed = subprocess.run(['sh', '-s'], input=script, capture_output=True)
        self.assertEqual(passed.returncode, 0, passed.stderr)
        self.assertEqual((ui/'packs/en/Main.qml').read_text(), 'new')
        self.assertTrue((ui/'icons/deezer.png').exists() and (ui/'packs.dev').is_dir())
        self.assertFalse((ui/'safe-mode').exists() or (ui/'boot-count').exists())
        self.assertEqual((ui/'.pack-version').read_text(), '5.0.4.1\n')
        self.assertEqual((data/'enginex/version.txt').read_text(), '5.0.4.1\n')
        self.assertEqual(list(ui.glob('.install.*')) + list(ui.glob('*.replaced')), [])
        self.assertFalse((data/'AppDataUser/AIR Music Technology/Engine/.cache/qml').exists())
        # Same version again: a hand-deployed pack is left alone, and so is the crash state
        # engine-ui-guard needs to see (#140).
        qml = data/'AppDataUser/AIR Music Technology/Engine/.cache/qml'
        qml.mkdir(parents=True)
        for name in ('safe-mode', 'mods-quarantined', 'boot-count', 'boot-token', 'last-crash'):
            (ui/name).write_text('1')
        (ui/'packs/en/Main.qml').write_text('dev')
        subprocess.run(['sh', '-s'], input=script, check=True, capture_output=True)
        self.assertEqual((ui/'packs/en/Main.qml').read_text(), 'dev')
        self.assertTrue(qml.is_dir())
        for name in ('safe-mode', 'mods-quarantined', 'boot-count', 'boot-token', 'last-crash'):
            self.assertTrue((ui/name).exists(), name)
        # A rebuilt release with the same version but new packs is installed.
        buf = io.BytesIO()
        with tarfile.open(fileobj=buf, mode='w') as t:
            info = tarfile.TarInfo('packs/en/Main.qml')
            info.size = 7
            t.addfile(info, io.BytesIO(b'rebuilt'))
        (seed/'ui-packs.tar.xz').write_bytes(lzma.compress(buf.getvalue(), check=lzma.CHECK_CRC32))
        (data/'enginex/version.txt').write_text('5.0.4\n')
        subprocess.run(['sh', '-s'], input=script, check=True, capture_output=True)
        self.assertEqual((ui/'packs/en/Main.qml').read_text(), 'rebuilt')
        self.assertFalse(qml.exists())
        for name in ('safe-mode', 'mods-quarantined', 'boot-count', 'boot-token', 'last-crash'):
            self.assertTrue((ui/name).exists(), name)
        self.assertEqual((data/'enginex/version.txt').read_text(), '5.0.4.1\n')
        version.write_text('5.0.4.2\n')
        subprocess.run(['sh', '-s'], input=script, check=True, capture_output=True)
        for name in ('safe-mode', 'mods-quarantined', 'boot-count', 'boot-token', 'last-crash'):
            self.assertFalse((ui/name).exists(), name)

    def test_prepare_atomic_failure_retry_identity_and_symlink_guard(self):
        # Relocate every guest state/seed path into this lab fixture. Only host
        # shell utilities run; no firmware executable or real /data is accessed.
        seed = self.root/'seed'
        seed.mkdir()
        key = self.key.with_suffix('.pub').read_bytes()
        (seed/'authorized_keys').write_bytes(key)
        data = self.root/'data'
        script = az.PREPARE.replace(b'/data', str(data).encode()).replace(
            b'/usr/share/enginex', str(seed).encode())
        failed = subprocess.run(['sh', '-s'], input=script.replace(
            b'/usr/bin/ssh-keygen', b'/usr/bin/false'), capture_output=True)
        self.assertNotEqual(failed.returncode, 0)
        state = data/'enginex'
        self.assertFalse((state/'ssh_host_ed25519_key').exists())
        self.assertEqual(list(state.glob('.prepare.*')), [])
        for _ in range(2):
            passed = subprocess.run(['sh', '-s'], input=script, capture_output=True)
            self.assertEqual(passed.returncode, 0, passed.stderr)
            identity = (state/'ssh_host_ed25519_key.pub').read_bytes()
            if _ == 0:
                original_identity = identity
                (seed/'authorized_keys').write_bytes(b'replacement seed\n')
            self.assertEqual(identity, original_identity)
            self.assertEqual((state/'authorized_keys').read_bytes(), key)
        (state/'authorized_keys').unlink()
        sentinel = self.root/'sentinel'
        sentinel.write_text('keep')
        (state/'authorized_keys').symlink_to(sentinel)
        refused = subprocess.run(['sh', '-s'], input=script, capture_output=True)
        self.assertNotEqual(refused.returncode, 0)
        self.assertEqual(sentinel.read_text(), 'keep')

    # --- a full /data must not stop prepare ------------------------------------------------------------
    def _prepare_lab(self, name):
        """A lab /data, a seed directory and the script with the deck's paths relocated into the lab (as above)."""
        seed, data, version = self.root/(name+'-seed'), self.root/(name+'-data'), self.root/(name+'-version')
        seed.mkdir()
        (seed/'authorized_keys').write_bytes(self.key.with_suffix('.pub').read_bytes())
        version.write_text('5.0.4.1\n')
        script = az.PREPARE.replace(b'/data', str(data).encode()).replace(
            b'/usr/share/enginex', str(seed).encode()).replace(b'/etc/enginex/version.txt', str(version).encode())
        return seed, data, script

    def _fake_stat(self, data, capacity_kib, log=None):
        """PATH entry with a `stat -f` of a disk of capacity_kib KiB that holds exactly what is under `data`."""
        shim = self.root/'shim'
        shim.mkdir(exist_ok=True)
        (shim/'stat').write_text(
            '#!/bin/sh\n'
            'case $1 in -f)\n'
            + (f'echo x >> "{log}"\n' if log else '') +
            f'used=$(du -sk "{data}" | cut -f1)\n'
            f'echo "$(( ({capacity_kib} - used) / 4 )) 4096" ;;\n'
            '*) exec /usr/bin/stat "$@" ;; esac\n')
        (shim/'stat').chmod(0o755)
        return {'PATH': f'{shim}:{os.environ["PATH"]}'}

    @staticmethod
    def _file(path, kib, age=0):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(b'x' * 1024 * kib)
        os.utime(path, (1_700_000_000 + age, 1_700_000_000 + age))

    @staticmethod
    def _export(pdb, content, stamp=True):
        """An export.pdb and, as the bridge writes it right after, the marker with its size and mtime (pdbStamp)."""
        pdb.parent.mkdir(parents=True, exist_ok=True)
        pdb.write_bytes(content)
        if stamp:
            st = pdb.stat()
            (pdb.parent/'.enginex-generated').write_text(f'{st.st_size}:{st.st_mtime_ns}')

    def _mountinfo(self, *mounted):
        """A mount table (mountinfo format) that has the root and one tmpfs at each of the paths."""
        lines = ['1 0 8:1 / / rw - ext4 /dev/root rw']
        lines += [f'{10 + i} 1 0:{50 + i} / {path} rw - tmpfs tmpfs rw' for i, path in enumerate(mounted)]
        table = self.root/'mountinfo'
        table.write_text('\n'.join(lines) + '\n')
        return {'ENGINEX_MOUNTINFO': str(table)}

    @staticmethod
    def _cleanup(script, **env):
        """prepare with a floor no disk meets, so that every candidate of every class is tried."""
        return subprocess.run(['sh', '-s'], input=script, capture_output=True,
                              env=dict(os.environ, ENGINEX_DATA_FLOOR_KIB=str(10**9), **env))

    def _seed_deck(self, data):
        """One file of every class prepare may remove (returned, in the order of its list) and of every class it never may."""
        e, put = data/'enginex', self._file
        put(e/'.prepare.old'/'f', 64, 1)
        put(e/'ui/.install.old'/'f', 64, 2)
        put(e/'bin/enginex-suite.9.tmp', 64, 3)
        put(e/'Stream/.part-x', 64, 4)
        put(e/'shadow-v3/peer-aa/.fetch-x', 64, 5)
        put(e/'ui/qml-cache/a.qmlc', 512, 6)
        reports = [e/'logs'/f'EngineX-boot-report-20261122T0{i}0000.000000000Z.txt' for i in range(1, 6)]
        for i, path in enumerate(reports):
            put(path, 128, 10 + i)
        (e/'ui/boot-report-latest').write_text(reports[0].name + '\n')    # the marker protects the oldest report
        anlz = e/'prolink-export/PIONEER/USBANLZ'
        put(anlz/'P001/ANLZ0000.DAT', 128, 20)
        put(anlz/'P002/ANLZ0000.DAT', 128, 21)
        self._export(e/'prolink-export/PIONEER/rekordbox/export.pdb', b'\x00 /PIONEER/USBANLZ/P001/ANLZ0000.DAT \x00')
        streams = [e/'Stream'/n for n in ('1.flac', '2.flac', 'spotify-abc.ogg')]
        for i, path in enumerate(streams):
            put(path, 512, 30 + i)
        never = [
            e/'Stream/my-own-mix.wav', e/'Stream/Stems/7/stem.flac', data/'Music/dj-track.mp3',
            data/'AppDataUser/AIR Music Technology/Engine/Engine Library/Database2/m.db',
            data/'AppDataUser/AIR Music Technology/Engine/Engine Library/Album Art/a.jpg',
            e/'ui/settings.conf', e/'state/deezer-owner.json', data/'StreamingAccounts.json',
            e/'hfs-libraries/vol/m.db', e/'hfs-lib/base/upper/m.db', data/'.prolink-cache/Player-1/t.mp3',
            e/'mods/m/mod.json', e/'mods-state/m/s.json', e/'ui/mods-overlay/T.qml', data/'inMusicTempFiles/copy.flac']
        for path in never:
            put(path, 64)
        (e/'authorized_keys').write_bytes(self.key.with_suffix('.pub').read_bytes())
        return {'leftovers': [e/'.prepare.old', e/'ui/.install.old', e/'bin/enginex-suite.9.tmp', e/'Stream/.part-x',
                              e/'shadow-v3/peer-aa/.fetch-x'],
                'qml': [e/'ui/qml-cache'], 'reports': reports, 'anlz': [anlz/'P001', anlz/'P002'], 'streams': streams,
                'never': never + [e/'authorized_keys', e/'ui/boot-report-latest']}

    def _used_kib(self, data):
        return int(subprocess.run(['du', '-sk', str(data)], capture_output=True, text=True).stdout.split()[0])

    def test_prepare_reads_free_space_once_when_there_is_room(self):
        seed, data, script = self._prepare_lab('room')
        log = self.root/'stat.log'
        env = dict(os.environ, **self._fake_stat(data, 10**7, log), ENGINEX_DATA_FLOOR_KIB='1000')
        for _ in range(2):
            p = subprocess.run(['sh', '-s'], input=script, capture_output=True, env=env)
            self.assertEqual(p.returncode, 0, p.stderr)
            self.assertNotIn(b'internal storage', p.stderr)
        self.assertEqual(len(log.read_text().split()), 2, 'one free-space reading per run')

    def test_prepare_frees_only_what_is_made_again_in_order_and_only_to_the_floor(self):
        floor = 4096
        # 0.7 MiB short of the floor: the leftovers (0.3 MiB) are not enough, with the QML cache (0.5 MiB) the floor is met
        seed, data, script = self._prepare_lab('early')
        seeded = self._seed_deck(data)
        env = dict(os.environ, **self._fake_stat(data, self._used_kib(data) + floor - 700), ENGINEX_DATA_FLOOR_KIB=str(floor))
        p = subprocess.run(['sh', '-s'], input=script, capture_output=True, env=env)
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertNotIn(b'internal storage', p.stderr)      # the floor was met: nothing to report
        for path in seeded['leftovers'] + seeded['qml']:
            self.assertFalse(path.exists(), path)
        for path in seeded['reports'] + seeded['anlz'] + seeded['streams']:
            self.assertTrue(path.exists(), path)             # later classes are not touched once the floor is met
        # 1.1 MiB short: every class before the stream cache is used, the stream cache only as far as the floor needs
        seed, data, script = self._prepare_lab('order')
        seeded = self._seed_deck(data)
        before = {q: q.read_bytes() for q in seeded['never']}
        env = dict(os.environ, **self._fake_stat(data, self._used_kib(data) + floor - 1204), ENGINEX_DATA_FLOOR_KIB=str(floor))
        p = subprocess.run(['sh', '-s'], input=script, capture_output=True, env=env)
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertNotIn(b'internal storage', p.stderr)
        for path in seeded['leftovers'] + seeded['qml'] + seeded['anlz'][1:] + seeded['reports'][1:2]:
            self.assertFalse(path.exists(), path)
        # kept: the newest three reports and the one the marker names, the export's named directory, the newer downloads
        for path in seeded['reports'][:1] + seeded['reports'][2:] + seeded['anlz'][:1] + seeded['streams'][1:]:
            self.assertTrue(path.exists(), path)
        self.assertFalse(seeded['streams'][0].exists(), 'the oldest download goes first')
        for path, content in before.items():
            self.assertEqual(path.read_bytes(), content, path)

    def test_prepare_cleanup_never_removes_a_mount_point_or_a_directory_with_one_below(self):
        seed, data, script = self._prepare_lab('mnt')
        e, put = data/'enginex', self._file
        anlz = e/'prolink-export/PIONEER/USBANLZ'
        mounted = [e/'Stream/1.flac', e/'.prepare.m', e/'ui/qml-cache/sub', anlz/'P002']
        for m in mounted:
            put(m/'keep', 1)
        put(e/'ui/qml-cache/a.qmlc', 1)
        put(anlz/'P003/ANLZ0000.DAT', 1)
        self._export(e/'prolink-export/PIONEER/rekordbox/export.pdb', b'\x00 /PIONEER/USBANLZ/P001/ANLZ0000.DAT \x00')
        for plain in (e/'Stream/2.flac', e/'.prepare.s/f'):
            put(plain, 1)
        put(data/'Music/dj-dir/precious.mp3', 1)
        (e/'.prepare.l').mkdir()
        (e/'.prepare.l/link').symlink_to(data/'Music/dj-dir')
        p = self._cleanup(script, **self._mountinfo(*mounted))
        self.assertEqual(p.returncode, 0, p.stderr)
        for m in mounted:
            self.assertTrue((m/'keep').exists(), f'{m} is a mount point (or has one below it) and was emptied')
        self.assertTrue((e/'ui/qml-cache/a.qmlc').exists(), 'a directory with a mount point below it stays as a whole')
        for plain in (e/'Stream/2.flac', e/'.prepare.s', anlz/'P003', e/'.prepare.l'):
            self.assertFalse(plain.exists(), f'{plain}: the cleanup goes on past a mount point')
        self.assertTrue((data/'Music/dj-dir/precious.mp3').exists(), 'a link inside a removable directory is not followed')

    def test_prepare_cleanup_removes_nothing_when_the_mount_table_cannot_be_read(self):
        seed, data, script = self._prepare_lab('nomounts')
        seeded = self._seed_deck(data)
        p = self._cleanup(script, ENGINEX_MOUNTINFO=str(self.root/'no-such-table'))
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertIn(b'internal storage is full', p.stderr)
        for path in seeded['leftovers'] + seeded['qml'] + seeded['reports'] + seeded['anlz'] + seeded['streams']:
            self.assertTrue(path.exists(), f'{path}: without a mount table nothing is removed')

    def test_prepare_cleanup_of_the_stream_cache_removes_regular_files_only_and_only_our_names(self):
        seed, data, script = self._prepare_lab('names')
        e, put = data/'enginex', self._file
        put(e/'Stream/1.flac/inner', 1)                            # a directory with a cache name
        put(data/'Music/dj.mp3', 1)
        (e/'Stream/2.flac').symlink_to(data/'Music/dj.mp3')        # a link with a cache name
        odd = [e/'Stream/with space.flac', e/'Stream/-dash.flac', e/'Stream/.part-with space', e/'.prepare.with space/f']
        for path in odd + [e/'Stream/3.flac', e/'Stream/.part-ok']:
            put(path, 1)
        p = self._cleanup(script, **self._mountinfo())
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertTrue((e/'Stream/1.flac/inner').exists(), 'only regular files go (Trim in deemixclient)')
        self.assertTrue((e/'Stream/2.flac').is_symlink() and (data/'Music/dj.mp3').exists())
        for path in odd:
            self.assertTrue(path.exists(), f'{path} is none of ours')
        for path in (e/'Stream/3.flac', e/'Stream/.part-ok'):
            self.assertFalse(path.exists(), path)

    def test_prepare_cleanup_removes_an_analysis_directory_only_when_the_export_is_whole(self):
        toy = b'\x00 /PIONEER/USBANLZ/P001/ANLZ0000.DAT \x00'
        for case in ('malformed', 'unstamped', 'stale', 'nonames', 'absent', 'empty', 'whole'):
            with self.subTest(case):
                seed, data, script = self._prepare_lab(f'pdb-{case}')
                anlz, pdb = data/'enginex/prolink-export/PIONEER/USBANLZ', data/'enginex/prolink-export/PIONEER/rekordbox/export.pdb'
                for n in ('P001', 'P002'):
                    self._file(anlz/n/'ANLZ0000.DAT', 1)
                pdb.parent.mkdir(parents=True)
                if case == 'malformed':
                    self._export(pdb, b'this is not a rekordbox export\n')
                elif case == 'unstamped':
                    self._export(pdb, toy, stamp=False)
                elif case == 'stale':
                    self._export(pdb, toy)
                    pdb.write_bytes(toy + b'x')                    # the file changed after the bridge stamped it
                elif case == 'nonames':
                    self._export(pdb, b'\x00 no analysis paths in here \x00')
                elif case == 'empty':
                    self._export(pdb, b'')
                elif case == 'whole':
                    self._export(pdb, toy)
                p = self._cleanup(script, **self._mountinfo())
                self.assertEqual(p.returncode, 0, p.stderr)
                self.assertTrue((anlz/'P001').exists(), 'the directory the export names stays')
                self.assertEqual((anlz/'P002').exists(), case != 'whole', 'only a whole export frees the one it does not name')

    def test_prepare_cleanup_goes_on_when_a_removal_fails(self):
        seed, data, script = self._prepare_lab('rmfail')
        e = data/'enginex'
        self._file(e/'Stream/.part-bad', 1, 1)
        self._file(e/'Stream/.part-ok', 1, 2)
        shim = self.root/'rmshim'
        shim.mkdir()
        (shim/'rm').write_text('#!/bin/sh\ncase "$*" in *.part-bad*) echo "rm: cannot remove: Operation not permitted" >&2; exit 1 ;; esac\nexec /bin/rm "$@"\n')
        (shim/'rm').chmod(0o755)
        p = self._cleanup(script, PATH=f'{shim}:{os.environ["PATH"]}', **self._mountinfo())
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertTrue((e/'Stream/.part-bad').exists())
        self.assertFalse((e/'Stream/.part-ok').exists(), 'the next candidate is still tried')

    def test_prepare_on_a_disk_that_takes_no_data_ends_with_success_and_keeps_every_file(self):
        import io, lzma, tarfile
        seed, data, script = self._prepare_lab('full')
        e = data/'enginex'
        own = self.key.with_suffix('.pub').read_bytes()
        old = b'ssh-ed25519 ' + az.RETIRED_RELEASE_KEY + b' release\n'
        subprocess.run(['sh', '-s'], input=script, check=True, capture_output=True)     # a first boot: host key, own key
        (e/'authorized_keys').write_bytes(old + own)          # the retired key is to go: a rewrite that cannot be written
        (e/'ui/packs/en').mkdir(parents=True)
        (e/'ui/packs/en/Main.qml').write_text('installed')
        (e/'ui/.pack-version').write_text('5.0.3\n')            # stale packs: an update that cannot be unpacked
        buf = io.BytesIO()
        with tarfile.open(fileobj=buf, mode='w') as t:
            info = tarfile.TarInfo('packs/en/Main.qml')
            info.size = 3
            t.addfile(info, io.BytesIO(b'new'))
        (seed/'ui-packs.tar.xz').write_bytes(lzma.compress(buf.getvalue(), check=lzma.CHECK_CRC32))
        conf = data/'.config/jp07.user.settings/AIR Music Technology/Engine.conf'
        conf.parent.mkdir(parents=True)
        conf.write_text('[General]\n' + ''.join(f'Setting{i}=value\n' for i in range(300)) + '[TimeDateSettings]\nTZEnvVar=UTC\n')
        (e/'state').mkdir()
        (e/'state/timezone').write_text('Europe/Berlin\n')
        os.utime(e/'state/timezone', (conf.stat().st_mtime + 120,) * 2)
        keep = {p: p.read_bytes() for p in (e/'authorized_keys', e/'ssh_host_ed25519_key', e/'ui/packs/en/Main.qml', conf)}
        zones = self.root/'zoneinfo'
        (zones/'Europe').mkdir(parents=True)
        (zones/'Europe/Berlin').write_text('tz')
        # nothing can be freed (no class has a file), the floor is out of reach, and every write of data fails (EFBIG
        # stands in for ENOSPC: an empty file can still be made, as on a full ext4)
        env = dict(os.environ, ENGINEX_DATA_FLOOR_KIB=str(10**9), ENGINEX_ZONEINFO_DIR=str(zones))
        p = subprocess.run(['sh', '-c', 'trap "" XFSZ; ulimit -f 0; exec sh -s'], input=script, capture_output=True, env=env)
        self.assertEqual(p.returncode, 0, p.stderr)
        lines = [l for l in p.stderr.decode().splitlines() if l.startswith('enginex-prepare: internal storage is full')]
        self.assertEqual(len(lines), 1, p.stderr)
        for word in ('SSH keys', 'UI packs'):
            self.assertIn(word, lines[0])
        for path, content in keep.items():
            self.assertEqual(path.read_bytes(), content, path)
        self.assertEqual(sorted(x.name for x in e.glob('.prepare.*')) + sorted(x.name for x in e.glob('*.new')), [])
        self.assertEqual(list((e/'ui').glob('.install.*')), [])
        # with room again the same disk is brought up to date
        p = subprocess.run(['sh', '-s'], input=script, capture_output=True, env=dict(os.environ, ENGINEX_ZONEINFO_DIR=str(zones)))
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertNotIn(b'internal storage', p.stderr)
        self.assertEqual((e/'authorized_keys').read_bytes(), own)
        self.assertEqual((e/'ui/packs/en/Main.qml').read_text(), 'new')
        self.assertIn('Europe/Berlin', conf.read_text())

    def test_prepare_keeps_engine_conf_when_awk_cuts_its_output_and_still_says_0(self):
        # BusyBox awk reported 0 for output it could not write; the empty copy then replaced the DJ's Engine.conf
        seed, data, script = self._prepare_lab('awk')
        conf = data/'.config/jp07.user.settings/AIR Music Technology/Engine.conf'
        conf.parent.mkdir(parents=True)
        conf.write_text('[General]\n' + ''.join(f'Setting{i}=value\n' for i in range(300)) + '[TimeDateSettings]\nTZEnvVar=UTC\n')
        (data/'enginex/state').mkdir(parents=True)
        (data/'enginex/state/timezone').write_text('Europe/Berlin\n')
        os.utime(data/'enginex/state/timezone', (conf.stat().st_mtime + 120,) * 2)
        zones = self.root/'zoneinfo'
        (zones/'Europe').mkdir(parents=True)
        (zones/'Europe/Berlin').write_text('tz')
        shim = self.root/'shim-awk'
        shim.mkdir()
        (shim/'awk').write_text('#!/bin/sh\ncase "$1$2" in -vtz=*) exit 0 ;; esac\nexec /usr/bin/awk "$@"\n')
        (shim/'awk').chmod(0o755)
        before = conf.read_bytes()
        env = dict(os.environ, PATH=f'{shim}:{os.environ["PATH"]}', ENGINEX_ZONEINFO_DIR=str(zones))
        p = subprocess.run(['sh', '-s'], input=script, capture_output=True, env=env)
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertEqual(conf.read_bytes(), before)
        self.assertEqual(list(conf.parent.glob('*.tmp.*')), [])
        self.assertIn(b'timezone not restored', p.stderr)
        # the same disk with a working awk is brought up to date
        p = subprocess.run(['sh', '-s'], input=script, capture_output=True, env=dict(os.environ, ENGINEX_ZONEINFO_DIR=str(zones)))
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertIn('TZEnvVar=Europe/Berlin\n', conf.read_text())
        self.assertIn('Setting299=value\n', conf.read_text())

    def test_prepare_uses_the_names_the_programs_use(self):
        # the shell copies of two Go patterns must not drift from the programs that own them
        fetch = (Path(__file__).resolve().parents[2]/'pkg/deemixclient/fetch.go').read_text()
        go = re.search(r'var evictable = regexp\.MustCompile\(`([^`]+)`\)', fetch).group(1)
        self.assertIn(go.replace(r'\d', '[0-9]').replace(r'\.', '[.]'), az.PREPARE.decode())
        report = (Path(__file__).resolve().parents[2]/'pkg/webui/boot_report.go').read_text()
        go = re.search(r'var bootReportName = regexp\.MustCompile\(`([^`]+)`\)', report).group(1)
        self.assertIn(go.replace('(?:', '(').replace(r'\.', '[.]'), az.PREPARE.decode())

    def test_public_image_ships_no_key_and_retires_the_old_release_key(self):
        # #46: public images seed no key, sshd needs a non-empty authorized_keys,
        # and the key public images up to 5.0.4.1 seeded is dropped from /data.
        az.inject(self.fs, None)
        self.assertNotIn('authorized_keys', self.fs.entries('/usr/share/enginex'))
        self.assertIn(b'ConditionFileNotEmpty=/data/enginex/authorized_keys',
                      self.fs.read('/usr/lib/systemd/system/enginex-sshd.service'))
        seed, data = self.root/'seed', self.root/'data'
        seed.mkdir()
        script = az.PREPARE.replace(b'/data', str(data).encode()).replace(
            b'/usr/share/enginex', str(seed).encode())
        run = lambda: subprocess.run(['sh', '-s'], input=script, capture_output=True)
        state = data/'enginex'
        p = run()
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertFalse((state/'authorized_keys').exists())
        self.assertTrue((state/'ssh_host_ed25519_key').exists())
        own = self.key.with_suffix('.pub').read_bytes()
        old = b'ssh-ed25519 ' + az.RETIRED_RELEASE_KEY + b' release\n'
        (state/'authorized_keys').write_bytes(old + own)
        p = run()
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertEqual((state/'authorized_keys').read_bytes(), own)
        self.assertEqual((state/'authorized_keys').stat().st_mode & 0o777, 0o600)
        (state/'authorized_keys').write_bytes(old)
        p = run()
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertEqual((state/'authorized_keys').read_bytes(), b'')
        # The maintainer's deck opts out; a private image seeding the same key keeps it.
        for keep in (state/'keep-release-key', seed/'authorized_keys'):
            keep.write_bytes(old)
            (state/'authorized_keys').write_bytes(old + own)
            p = run()
            self.assertEqual(p.returncode, 0, p.stderr)
            self.assertEqual((state/'authorized_keys').read_bytes(), old + own)
            keep.unlink()

    def test_prepare_and_etc_reset_rename_the_legacy_dir_losslessly(self):
        # Upgrade migration moves the real data directory once; no alias is written.
        seed, data = self.root/'seed', self.root/'data'
        seed.mkdir()
        old, new = data/legacy.DATA_NAME, data/'enginex'
        (old/'Stream').mkdir(parents=True)
        (old/'Stream/song.flac').write_text('x')
        (old/'settings.conf').write_text('s')
        own = self.key.with_suffix('.pub').read_bytes()
        (old/'authorized_keys').write_bytes(own)
        for template in (az.ETC_RESET, az.PREPARE):
            script = template.replace(b'/data', str(data).encode()).replace(
                b'/usr/share/enginex', str(seed).encode())
            for _ in range(2):
                p = subprocess.run(['sh', '-s'], input=script, capture_output=True)
                self.assertEqual(p.returncode, 0, p.stderr)
            self.assertFalse(os.path.lexists(old))
            self.assertEqual((new/'Stream/song.flac').read_text(), 'x')
            self.assertEqual((new/'settings.conf').read_text(), 's')
            self.assertEqual((new/'authorized_keys').read_bytes(), own)
        # Both real (a rollback image recreated the old one): enginex wins, the old content is kept aside.
        old.mkdir()
        (old/'settings.conf').write_text('older')
        (old/'extra').write_text('e')
        p = subprocess.run(['sh', '-s'], input=script, capture_output=True)
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertFalse(os.path.lexists(old))
        self.assertEqual((new/'settings.conf').read_text(), 's')
        self.assertEqual((new/'extra').read_text(), 'e')
        aside = list(new.glob('legacy-data.*'))
        self.assertEqual([(a/'settings.conf').read_text() for a in aside], ['older'])

    def test_234_prepare_migrates_an_existing_reverse_alias_once(self):
        # A pre-5.3.0.5 layout may have a reverse or dangling current alias.
        # Upgrading repairs it, preserves data and leaves only the current directory.
        seed, data = self.root/'seed', self.root/'data'
        seed.mkdir()
        old, new = data/legacy.DATA_NAME, data/'enginex'
        own = self.key.with_suffix('.pub').read_bytes()
        for link in (legacy.DATA_NAME, 'enginex'):
            for template in (az.ETC_RESET, az.PREPARE):
                shutil.rmtree(data, ignore_errors=True)
                (old/'Stream').mkdir(parents=True)
                (old/'Stream/song.flac').write_text('x')
                (old/'authorized_keys').write_bytes(own)
                new.symlink_to(link)
                script = template.replace(b'/data', str(data).encode()).replace(
                    b'/usr/share/enginex', str(seed).encode())
                p = subprocess.run(['sh', '-s'], input=script, capture_output=True)
                self.assertEqual(p.returncode, 0, (link, p.stderr))
                self.assertTrue(new.is_dir() and not new.is_symlink(), link)
                self.assertFalse(os.path.lexists(old), link)
                self.assertEqual((new/'Stream/song.flac').read_text(), 'x')
        # A link anywhere else is not EngineX's: prepare still refuses it.
        shutil.rmtree(data)
        old.mkdir(parents=True)
        (data/'elsewhere').mkdir()
        new.symlink_to('elsewhere')
        script = az.PREPARE.replace(b'/data', str(data).encode()).replace(b'/usr/share/enginex', str(seed).encode())
        p = subprocess.run(['sh', '-s'], input=script, capture_output=True)
        self.assertNotEqual(p.returncode, 0)
        self.assertTrue(new.is_symlink() and old.is_dir())

    def test_234_release_gate_reads_an_old_release_where_its_prepare_left_the_data(self):
        # #234 review R5: a 5.3.0.2 baseline keeps its data in the real /data/enginex; the old
        # phase wrote its language setting and read its version and host key in /data/enginex.
        gate = (Path(__file__).resolve().parent / 'release-gate.sh').read_text()
        func = re.search(r'^data_dir\(\) \{.*?^\}', gate, re.M | re.S)
        self.assertIsNotNone(func, 'release-gate.sh has no data_dir()')
        for layout, want in (('5.3.0.2', legacy.DATA_NAME), ('5.3.0.3', 'enginex')):
            w = self.root / f'gate-{layout}'
            shutil.rmtree(w, ignore_errors=True)
            if layout == '5.3.0.2':
                (w/'data'/legacy.DATA_NAME).mkdir(parents=True)
            else:
                (w/'data/enginex').mkdir(parents=True)
                (w/'data'/legacy.DATA_NAME).symlink_to('enginex')
            out = subprocess.run(['bash', '-c', func.group(0) + '\ndata_dir'], env={**os.environ, 'W': str(w), 'legacy_data': legacy.DATA_DIR},
                                 capture_output=True, text=True)
            self.assertEqual(out.stdout.strip(), str(w/'data'/want), layout)
        # The old phase's setting, version and host key, and the phase's UI dir, go through it.
        phase = gate[gate.index('\nphase() {'):gate.index('\ninner() {')]
        for use in ('D=$(data_dir)', 'printf \'lang=%s\\n\' "$LANG_PACK" > "$D/ui/settings.conf"',
                    'grep -qx "$OLDV" "$D/version.txt"', 'cp "$D/ssh_host_ed25519_key.pub" "$W/old-hostkey.pub"',
                    'local ui=$D/ui'):
            self.assertIn(use, phase)

    def test_234_release_gate_unit_check_ignores_its_own_work_dir_name(self):
        # RC 2026-11-15: the gate's work dir is /var/tmp/enginex-gate-*, and `ls` of several dirs
        # printed their paths, so a clean image failed "no unit named enginex is enabled".
        gate = (Path(__file__).resolve().parent / 'release-gate.sh').read_text()
        m = re.search(r'check "image: no unit under the old project name is enabled" bash -c "(.*)"$', gate, re.M)
        self.assertIsNotNone(m, 'release-gate.sh has no enginex unit check')
        root = self.root / 'enginex-gate-unitcheck' / 'new'
        wants = [root/'usr/lib/systemd/system/multi-user.target.wants',
                 root/'etc/systemd/system/multi-user.target.wants', root/'usr/lib/systemd/system/enginex.target.wants']
        for d in wants:
            d.mkdir(parents=True, exist_ok=True)
        (wants[1]/'enginex-prepare.service').touch()
        cmd = m.group(1).replace('$R', str(root)).replace('$legacy_unit', legacy.PROJECT)
        self.assertEqual(subprocess.run(['bash', '-c', cmd]).returncode, 0, 'a clean image must pass')
        (wants[0]/(legacy.PROJECT+'-prepare.service')).touch()
        self.assertNotEqual(subprocess.run(['bash', '-c', cmd]).returncode, 0, 'an old unit must fail')

    def test_etc_reset_drops_stale_434_overlay_once(self):
        data = self.root/'data'
        upper = data/'system/etc/overlay'
        for name in ('hosts', 'resolv.conf', 'connman/main.conf', 'dnsmasq.d/x.conf',
                     'edisksd/main.conf', 'systemd/timesyncd.conf', 'systemd/timesyncd.conf.d/old.conf', 'keep.conf'):
            (upper/name).parent.mkdir(parents=True, exist_ok=True)
            (upper/name).write_text('4.3.4')
        script = az.ETC_RESET.replace(b'/data', str(data).encode())
        passed = subprocess.run(['sh', '-s'], input=script, capture_output=True)
        self.assertEqual(passed.returncode, 0, passed.stderr)
        self.assertEqual(sorted(p.name for p in upper.iterdir()), ['keep.conf', 'systemd'])
        self.assertEqual(list((upper/'systemd').iterdir()), [])
        (upper/'hosts').write_text('user edit after upgrade')
        passed = subprocess.run(['sh', '-s'], input=script, capture_output=True)
        self.assertEqual(passed.returncode, 0, passed.stderr)
        self.assertEqual((upper/'hosts').read_text(), 'user edit after upgrade')
        (data/'enginex/etc-overlay-epoch').unlink()
        sentinel = self.root/'sentinel'
        sentinel.mkdir()
        (sentinel/'hosts').write_text('keep')
        (data/'system/etc/overlay').rename(self.root/'old-upper')
        (data/'system/etc/overlay').symlink_to(sentinel)
        refused = subprocess.run(['sh', '-s'], input=script, capture_output=True)
        self.assertNotEqual(refused.returncode, 0)
        self.assertEqual((sentinel/'hosts').read_text(), 'keep')

    def test_etc_reset_on_a_disk_that_takes_no_data_still_ends_with_success(self):
        # The removals are made, the epoch marker cannot be written (EFBIG stands in for ENOSPC), the next boot repeats them
        data = self.root/'data'
        upper = data/'system/etc/overlay'
        (upper/'hosts').parent.mkdir(parents=True)
        (upper/'hosts').write_text('4.3.4')
        (upper/'keep.conf').write_text('user')
        script = az.ETC_RESET.replace(b'/data', str(data).encode())
        p = subprocess.run(['sh', '-c', 'trap "" XFSZ; ulimit -f 0; exec sh -s'], input=script, capture_output=True)
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertIn(b'epoch marker not written', p.stderr)
        self.assertEqual(sorted(x.name for x in upper.iterdir()), ['keep.conf'])
        self.assertFalse((data/'enginex/etc-overlay-epoch').exists() and (data/'enginex/etc-overlay-epoch').stat().st_size)
        self.assertEqual(list((data/'enginex').glob('*.tmp')), [])
        p = subprocess.run(['sh', '-s'], input=script, capture_output=True)
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertTrue((data/'enginex/etc-overlay-epoch').read_text().strip())

    def test_reject_symlink_parent_existing_symlink_and_hardlink(self):
        self.fs.cmd('symlink /alias /usr/Engine/Firmware', True)
        with self.assertRaises(az.SafetyError):
            self.fs.write('/alias/motor.bin', b'bad')
        self.fs.cmd('symlink /etc/link /etc/shadow', True)
        with self.assertRaises(az.SafetyError):
            self.fs.write('/etc/link', b'bad')
        self.fs.cmd('link /etc/shadow /etc/hard', True)
        self.fs.cmd('set_inode_field /etc/shadow links_count 2', True)
        with self.assertRaises(az.SafetyError):
            self.fs.write('/etc/hard', b'bad')

    def test_protected_content_change_detected_without_trusting_manifest(self):
        before = self.fs.snapshot()
        self.fs.cmd('set_inode_field /usr/Engine/Firmware/motor.bin mode 0100700', True)
        self.assertNotEqual(before, self.fs.snapshot())

    def test_protected_extended_attributes_are_immutable(self):
        before = self.fs.snapshot()
        self.fs.cmd('ea_set /usr/Engine/Firmware/motor.bin user.test changed', True)
        self.assertNotEqual(before, self.fs.snapshot())

    def test_same_length_long_xattr_changes_are_detected(self):
        value = self.root/'attribute'
        value.write_bytes(b'A'*300)
        self.fs.cmd(f'ea_set -f "{value}" /usr/Engine/Firmware/motor.bin user.long', True)
        before = self.fs.snapshot()
        value.write_bytes(b'A'*299+b'B')
        self.fs.cmd(f'ea_set -f "{value}" /usr/Engine/Firmware/motor.bin user.long', True)
        self.assertNotEqual(before, self.fs.snapshot())

    def test_literal_systemd_escape_names_can_be_inventoried(self):
        source = self.root/'escaped'
        source.write_bytes(b'example')
        # debugfs quotes retain backslashes literally.
        self.fs.cmd(f'write "{source}" "/etc/secure\\x2dmedia.mount"', True)
        self.assertIn('/etc/secure\\x2dmedia.mount', dict(self.fs.walk()))
        self.fs.snapshot()

    def test_custom_service_gets_mount_ordering_and_target_registration(self):
        service = self.root/'enginex-test.service'
        service.write_text('[Service]\nExecStart=/usr/bin/true\n')
        az.inject(self.fs, az.public_key(self.key.with_suffix('.pub').read_text()), [service])
        self.assertIn(b'After=enginex-prepare.service', self.fs.read(
            '/usr/lib/systemd/system/enginex-test.service.d/enginex-ordering.conf'))
        self.assertIn('enginex-test.service', self.fs.entries(
            '/usr/lib/systemd/system/enginex.target.wants'))

    def test_overlay_mcu_and_protected_symlinks_rejected(self):
        overlay = self.root/'overlay'
        target = overlay/'usr/Engine/Firmware'
        target.mkdir(parents=True)
        (target/'motor.bin').write_bytes(b'bad')
        with self.assertRaises(az.SafetyError):
            az.inject(self.fs, b'key', overlay=overlay)
        shutil.rmtree(overlay)
        overlay.mkdir()
        (overlay/'alias').symlink_to('/boot')
        with self.assertRaises(az.SafetyError):
            az.inject(self.fs, b'key', overlay=overlay)

    def test_overlay_safe_relative_absolute_and_chained_links(self):
        overlay = self.root/'overlay'
        scripts = overlay/'usr/Engine/Scripts'
        scripts.mkdir(parents=True)
        (scripts/'cleanup-JP07.sh').write_bytes(b'#!/bin/sh\nexit 0\n')
        (scripts/'cleanup').symlink_to('cleanup-JP07.sh')
        (overlay/'absolute').symlink_to('/usr/Engine/Scripts/cleanup')
        (overlay/'relative').symlink_to('absolute')
        (overlay/'dangling').symlink_to('/missing/allowed-file')
        wants = overlay/'etc/systemd/system/multi-user.target.wants'
        wants.mkdir(parents=True)
        unit = wants.parent/'engine-guardian.service'
        unit.write_text('[Service]\nExecStart=/usr/bin/true\n')
        (wants/unit.name).symlink_to('../'+unit.name)
        before = self.fs.snapshot()
        az.inject(self.fs, b'key', overlay=overlay)
        self.assertEqual(self.fs.snapshot(), before)
        for path, target in (('/usr/Engine/Scripts/cleanup', 'cleanup-JP07.sh'),
                             ('/absolute', '/usr/Engine/Scripts/cleanup'),
                             ('/relative', 'absolute'),
                             ('/dangling', '/missing/allowed-file'),
                             ('/etc/systemd/system/multi-user.target.wants/'+unit.name,
                              '../'+unit.name)):
            self.assertIn('Fast link dest: "'+target+'"', self.fs.cmd(f'stat "{path}"'))

    def test_link_replaces_existing_nondirectory_but_preserves_directory(self):
        self.fs.write('/etc/enable', b'old')
        self.fs.link('/etc/enable', 'shadow')
        self.fs.link('/etc/enable', '/etc/shadow')
        self.assertIn('Fast link dest: "/etc/shadow"', self.fs.cmd('stat /etc/enable'))
        self.fs.cmd('link /etc/shadow /etc/hard', True)
        self.fs.cmd('set_inode_field /etc/shadow links_count 2', True)
        self.fs.link('/etc/hard', '/etc/shadow')
        self.assertIn(b'root:!', self.fs.read('/etc/shadow'))
        self.assertRegex(self.fs.cmd('stat /etc/shadow'), r'Links:\s+1\b')
        with self.assertRaises(az.SafetyError):
            self.fs.link('/etc', '/usr')

    def test_overlay_link_escape_protection_chains_and_cycles_refused_before_write(self):
        overlay = self.root/'overlay'
        attacks = [
            {'link': '../outside'}, {'link': '/../../etc/shadow'},
            {'link': '/boot/uboot'}, {'link': '/usr/Engine/Firmware/motor.bin'},
            {'link': 'other', 'other': '/boot/uboot'},
            {'link': '/alias/motor.bin', 'alias': '/usr/Engine/Firmware'},
            {'link': '/alias/../boot/uboot', 'alias': '/etc'},
            {'link': 'other', 'other': 'link'}, {'link': 'link'},
            {'link': 'bad"target'}, {'link': 'bad\\target'}, {'link': 'bad\ntarget'},
        ]
        before = self.fs.snapshot()
        for links in attacks:
            with self.subTest(links=links):
                overlay.mkdir()
                (overlay/'aaa-should-not-be-written').write_bytes(b'no')
                for path, target in links.items():
                    (overlay/path).symlink_to(target)
                with self.assertRaises(az.SafetyError):
                    az.inject(self.fs, b'key', overlay=overlay)
                self.assertNotIn('aaa-should-not-be-written', self.fs.entries('/'))
                self.assertEqual(self.fs.snapshot(), before)
                shutil.rmtree(overlay)

    def test_links_cannot_bypass_existing_guest_symlinks(self):
        self.fs.cmd('symlink /guest /usr/Engine/Firmware', True)
        self.fs.cmd('symlink /guest-chain guest', True)
        for target in ('/guest/motor.bin', '/guest-chain/motor.bin',
                       '/guest-chain/../outside', '/boot/../etc/shadow'):
            with self.subTest(target=target), self.assertRaises(az.SafetyError):
                self.fs.link('/safe-link', target)
        long_target = '/'+('a'*65)+'/../../boot/uboot'
        self.fs.cmd(f'symlink /long "{long_target}"', True)
        with self.assertRaises(az.SafetyError):
            self.fs.link('/safe-link', '/long')
        self.fs.cmd('symlink /guest-safe /etc/shadow', True)
        self.fs.link('/safe-link', '/guest-safe')
        self.assertIn('Fast link dest: "/guest-safe"', self.fs.cmd('stat /safe-link'))

    def test_complete_repository_overlay_is_accepted(self):
        overlay = Path(__file__).resolve().parents[2]/'rootfs-overlay'
        before = self.fs.snapshot()
        az.inject(self.fs, b'key', overlay=overlay)
        self.assertEqual(self.fs.snapshot(), before)
        for source in overlay.rglob('*'):
            dest = '/'+source.relative_to(overlay).as_posix()
            if source.is_symlink():
                self.assertIn('Fast link dest: "'+str(source.readlink())+'"',
                              self.fs.cmd(f'stat "{dest}"'))
            elif source.is_file():
                self.assertEqual(self.fs.read(dest), source.read_bytes())
        # Re-injection must replace existing wants links without corrupting ext2.
        az.inject(self.fs, b'key', overlay=overlay)
        self.assertEqual(self.fs.snapshot(), before)
        az.check_fs(self.disk)

    def test_set_version_writes_version_and_new_reset_epoch(self):
        overlay = Path(__file__).resolve().parents[2]/'rootfs-overlay'
        az.inject(self.fs, b'key', overlay=overlay, version='5.0.4.1')
        self.assertEqual(self.fs.read('/etc/enginex/version.txt'), b'5.0.4.1\n')
        reset = self.fs.read('/usr/libexec/enginex-etc-reset')
        self.assertRegex(reset, rb'epoch=5\.0\.4\.1-[0-9a-f]{12}\n')
        # Upper-layer copies of image-managed /etc files are dropped on upgrade (#33).
        self.assertIn(b' enginex/engine-ui-guard', reset)
        self.assertIn(b' enginex/engine-ui-guard', reset)
        self.assertIn(b' systemd/system/engine.service.d/override.conf', reset)
        with self.assertRaises(az.SafetyError):
            az.inject(self.fs, b'key', overlay=overlay, version='5.0.4.1; rm -rf /')
        self.assertTrue(az.VERSION_RE.fullmatch('5.3.0.1.1'))
        self.assertIsNone(az.VERSION_RE.fullmatch('5.3.0.1.1.1'))

    def test_234_image_has_enginex_names_and_no_obsolete_overlay_links(self):
        overlay = Path(__file__).resolve().parents[2]/'rootfs-overlay'
        az.inject(self.fs, None, overlay=overlay, version='5.3.0.5')
        entries = dict(self.fs.walk())
        self.assertFalse([p for p in entries if legacy.PROJECT in p])
        self.assertEqual(self.fs.read('/etc/enginex/version.txt'), b'5.3.0.5\n')
        for wants in ('/usr/lib/systemd/system/multi-user.target.wants', '/etc/systemd/system/multi-user.target.wants'):
            names = self.fs.entries(wants)
            self.assertFalse([n for n in names if legacy.PROJECT in n], wants)
        self.assertIn('enginex.target', self.fs.entries('/usr/lib/systemd/system/multi-user.target.wants'))
        self.assertTrue({'enginex-debloat.service', 'enginex-deemixd.service', 'enginex-perf-tune.service',
                         'enginex-storage-discover.service'} <= set(self.fs.entries('/etc/systemd/system/multi-user.target.wants')))
        # Every unit and drop-in the image enables says enginex, never the old target or prepare unit.
        for path, meta in entries.items():
            if stat.S_ISREG(meta[1]) and re.search(r'/systemd/system/.*\.(service|target|slice|conf)$', path):
                body = b'\n'.join(l for l in self.fs.read(path).splitlines() if not l.lstrip().startswith(b'#'))
                self.assertNotRegex(body, legacy.PROJECT.encode() + rb'-(custom|background|engine\.slice|storage|deemixd|debloat|perf)', path)
                self.assertNotRegex(body, rb'(?m)^Description=.*SC5000', path)
        az.check_fs(self.disk)

    def test_234_etc_reset_drops_old_and_new_named_upper_copies(self):
        overlay = Path(__file__).resolve().parents[2]/'rootfs-overlay'
        az.inject(self.fs, None, overlay=overlay, version='5.3.0.5')
        data = self.root/'data'
        upper = data/'system/etc/overlay'
        stale = [(legacy.PROJECT + '/libengine-ui-hook.so'), (legacy.PROJECT + '/version.txt'), (legacy.PROJECT + '/release-tag'),
                 ('systemd/system/' + legacy.DEEMIX + '.service'), ('systemd/system/' + legacy.PROJECT + '-background.slice'),
                 ('systemd/system/stream-sync.service.d/50-' + legacy.PROJECT + '-background.conf'),
                 ('systemd/system/edisksd.service.d/50-' + legacy.PROJECT + '-hook.conf'), ('default/' + legacy.PROJECT + '-perf'),
                 ('udev/rules.d/60-' + legacy.PROJECT + '-storage.rules'), ('systemd/journald.conf.d/' + legacy.PROJECT + '.conf'),
                 ('dnsmasq.d/' + legacy.PROJECT + '-telemetry.conf'), 'systemd/system/enginex-deemixd.service',
                 'default/enginex-perf']
        for name in stale + ['keep.conf', 'systemd/system/user-own.service']:
            (upper/name).parent.mkdir(parents=True, exist_ok=True)
            (upper/name).write_text('r4')
        (upper/'systemd/system/multi-user.target.wants').mkdir(parents=True)
        (upper/('systemd/system/multi-user.target.wants/' + legacy.DEEMIX + '.service')).symlink_to(('../' + legacy.DEEMIX + '.service'))
        # An r4 prepare on a 5.0.4 image linked /etc/enginex -> legacy in the upper layer: over the
        # image's /etc/legacy -> enginex link that is a loop, and the hook would never load.
        (upper/'enginex').symlink_to((legacy.PROJECT))
        script = self.fs.read('/usr/libexec/enginex-etc-reset').replace(b'/data', str(data).encode())
        p = subprocess.run(['sh', '-s'], input=script, capture_output=True)
        self.assertEqual(p.returncode, 0, p.stderr)
        for name in stale + ['enginex', ('systemd/system/multi-user.target.wants/' + legacy.DEEMIX + '.service')]:
            self.assertFalse(os.path.lexists(upper/name), name)
        self.assertEqual((upper/'keep.conf').read_text(), 'r4')
        self.assertEqual((upper/'systemd/system/user-own.service').read_text(), 'r4')

    def test_overlay_debloat_runs_offline(self):
        overlay = Path(__file__).resolve().parents[2]/'rootfs-overlay'
        self.fs.write('/usr/Engine/Scripts/engine', b'#!/bin/sh\n/usr/bin/az01-ethernet-speed 100\n'
                      b'\t\t\tUPDATE_FILE=$(cat /tmp/update-file-path)\n', 0o755)
        with unittest.mock.patch.dict(os.environ, {'ENGINEX_MIGRATION': ''}):
            az.inject(self.fs, b'key', overlay=overlay)
        self.assertIn(b'az01-ethernet-speed 1000', self.fs.read('/usr/Engine/Scripts/engine'))
        self.assertIn(b'\t\t\t' + GUARDED_UPDATE_FILE, self.fs.read('/usr/Engine/Scripts/engine'))
        self.assertIn(b'0.0.0.0 telemetry.inmusicbrands.com', self.fs.read('/etc/hosts'))
        az.check_fs(self.disk)

    @unittest.skipUnless(all(p.is_file() for p in STOCK_530), 'needs the official 5.3.0 images of the build-host lab')
    def test_overlay_debloat_guards_the_stock_update_file_step(self):
        # The stock scripts themselves: a base whose line no longer matches fails here instead of
        # silently keeping the stock step (an image on an HFS+ stick would then not install again).
        debloat = (Path(__file__).resolve().parents[2]/'rootfs-overlay/usr/local/bin/enginex-debloat').read_bytes()
        for image in STOCK_530:
            with self.subTest(image=image.name):
                stock = self.root/'stock.ext2'
                with stock.open('wb') as f:
                    az.inflate(az.Image(image.read_bytes()).payload(), f)
                script = az.ExtFS(stock).read('/usr/Engine/Scripts/engine')
                stock.unlink()
                self.assertEqual(script.count(b'UPDATE_FILE=$(cat /tmp/update-file-path)'), 1)
                self.fs.write('/usr/Engine/Scripts/engine', script, 0o755)
                az.offline_debloat(self.fs, debloat)
                patched = self.fs.read('/usr/Engine/Scripts/engine')
                self.assertNotIn(b'UPDATE_FILE=$(cat /tmp/update-file-path)', patched)
                self.assertEqual(patched.count(b'\t' + GUARDED_UPDATE_FILE), 1)

    def test_guardian_and_template_services(self):
        services = []
        for name in ('engine-guardian.service', 'enginex-storage@.service'):
            source = self.root/name
            source.write_text('[Service]\nExecStart=/usr/bin/true\n')
            services.append(source)
        az.inject(self.fs, b'key', services)
        wants = self.fs.entries('/usr/lib/systemd/system/enginex.target.wants')
        self.assertIn('engine-guardian.service', wants)
        self.assertNotIn('enginex-storage@.service', wants)
        for source in services:
            dest = '/usr/lib/systemd/system/'+source.name
            self.assertEqual(self.fs.read(dest), source.read_bytes())
            self.assertIn(b'After=enginex-prepare.service',
                          self.fs.read(dest+'.d/enginex-ordering.conf'))

    def test_invalid_and_duplicate_services_refused_before_write(self):
        for name in ('sshd.service', 'enginex-prepare.service',
                     'enginex-bad@@.service', 'enginex-foo@bar.service'):
            source = self.root/name
            source.write_text('[Service]\nExecStart=/usr/bin/true\n')
            with self.subTest(name=name), self.assertRaises(az.SafetyError):
                az.inject(self.fs, b'key', [source])
        source = self.root/'enginex-ok.service'
        source.write_text('[Service]\nExecStart=/usr/bin/true\n')
        with self.assertRaises(az.SafetyError):
            az.inject(self.fs, b'key', [source, source])
        self.assertNotIn('share', self.fs.entries('/usr'))

    def test_cli_unpack_noop_repack_and_failed_repack_publishes_nothing(self):
        original = self.root/'original.img'
        original.write_bytes(fixture(lzma.compress(self.disk.read_bytes())))
        unpacked = self.root/'unpacked'
        self.assertEqual(az.main(['unpack', str(original), '--output', str(unpacked)]), 0)
        self.assertEqual((unpacked/'rootfs.ext2').read_bytes(), self.disk.read_bytes())
        output = self.root/'repacked.img'
        az.main(['repack', str(original), '--rootfs', str(self.disk), '--output', str(output)])
        self.assertEqual(output.read_bytes(), original.read_bytes())
        self.fs.cmd('rm /usr/Engine/Firmware/motor.bin', True)
        failed = self.root/'must-not-exist.img'
        with self.assertRaises(SystemExit) as exc:
            az.main(['repack', str(original), '--rootfs', str(self.disk), '--output', str(failed)])
        self.assertEqual(exc.exception.code, 2)
        self.assertFalse(failed.exists())

    def test_cli_unpack_needs_no_debugfs(self):
        # enginex-installer unpacks 1st-gen images on macOS/Windows hosts without e2fsprogs.
        original = self.root/'nodebugfs.img'
        original.write_bytes(fixture(lzma.compress(self.disk.read_bytes())))
        unpacked = self.root/'unpacked-nodebugfs'
        which = shutil.which
        with unittest.mock.patch.object(az.shutil, 'which', lambda n: None if n == 'debugfs' else which(n)):
            self.assertEqual(az.main(['unpack', str(original), '--output', str(unpacked)]), 0)
            with self.assertRaisesRegex(az.SafetyError, 'debugfs is required'):
                az.ExtFS(unpacked/'rootfs.ext2').cmd('ls /')
        self.assertEqual((unpacked/'rootfs.ext2').read_bytes(), self.disk.read_bytes())

    def test_cli_unpack_accepts_a_windows_path(self):
        # BH2-A7-1: enginex-installer unpacks into %TEMP%\\enginex-rootfs-N\\unpacked on Windows;
        # the backslash, the separator there, was refused as an unsafe character in every path.
        original = self.root/'windows.img'
        original.write_bytes(fixture(lzma.compress(self.disk.read_bytes())))
        # On this host the same characters are one directory name.
        parent = self.root/'C:\\Users\\dj\\AppData\\Local\\Temp\\enginex-rootfs-1'
        parent.mkdir()
        with unittest.mock.patch.object(az, '_WINDOWS', True, create=True):
            self.assertEqual(az.main(['unpack', str(original), '--output', str(parent/'unpacked')]), 0)
            self.assertEqual((parent/'unpacked'/'rootfs.ext2').read_bytes(), self.disk.read_bytes())
            # Still refused there: what would end the quoted argument of a debugfs command.
            for bad in ('with"quote', 'with\nnewline'):
                with self.assertRaisesRegex(az.SafetyError, 'unsafe host output path'):
                    az.new_output(self.root/bad)
        # Where the backslash is a file-name character it stays refused.
        with self.assertRaisesRegex(az.SafetyError, 'unsafe host output path'):
            az.new_output(parent/'unpacked-again')
        with self.assertRaisesRegex(az.SafetyError, 'unsafe host image path'):
            az.ExtFS(parent/'unpacked'/'rootfs.ext2')

    def test_cli_repack_with_splash_and_recoverysplash(self):
        original = self.root/'orig_splash.img'
        original.write_bytes(fixture(lzma.compress(self.disk.read_bytes())))
        splash_file = self.root/'custom_splash.bin'
        splash_file.write_bytes(b'S' * 4096000)
        rec_file = self.root/'custom_rec.bin'
        rec_file.write_bytes(b'R' * 4096000)
        output = self.root/'repacked_splash.img'

        ret = az.main(['repack', str(original), '--rootfs', str(self.disk),
                       '--splash', str(splash_file),
                       '--recoverysplash', str(rec_file),
                       '--output', str(output)])
        self.assertEqual(ret, 0)
        im = az.Image(output.read_bytes())
        self.assertEqual(im.payload('splash'), b'S' * 4096000)
        self.assertEqual(im.payload('recoverysplash'), b'R' * 4096000)

    @unittest.skipUnless(shutil.which('resize2fs'), 'resize2fs required for resize attack fixture')
    def test_repack_refuses_rootfs_growth_before_publishing(self):
        original = self.root/'original.img'
        original.write_bytes(fixture(lzma.compress(self.disk.read_bytes())))
        larger = self.root/'larger.ext2'
        shutil.copyfile(self.disk, larger)
        with larger.open('ab') as f:
            f.truncate(32*1024**2)
        subprocess.run(['resize2fs', str(larger)], check=True, capture_output=True)
        output = self.root/'must-not-grow.img'
        with self.assertRaises(SystemExit) as exc:
            az.main(['repack', str(original), '--rootfs', str(larger), '--output', str(output)])
        self.assertEqual(exc.exception.code, 2)
        self.assertFalse(output.exists())

    def test_repack_refuses_boot_uuid_change(self):
        original = self.root/'original.img'
        original.write_bytes(fixture(lzma.compress(self.disk.read_bytes())))
        self.fs.cmd('set_super_value uuid 11111111-2222-3333-4444-555555555555', True)
        output = self.root/'must-not-change-uuid.img'
        with self.assertRaises(SystemExit) as exc:
            az.main(['repack', str(original), '--rootfs', str(self.disk), '--output', str(output)])
        self.assertEqual(exc.exception.code, 2)
        self.assertFalse(output.exists())

    @unittest.skipUnless(shutil.which('dumpe2fs'), 'dumpe2fs required')
    def test_room_counts_the_ext4_drivers_reserve(self):
        # The 5.3.0.5 RC numbers: 463 720 blocks of 1 KiB. With 4 147 free, deck1's df showed 53 KiB
        # available (e37b921d); with 4 064 free, every write to a mounted copy failed (d40ad7b2).
        self.assertEqual(az.ext4_reserve_blocks(463720, 1024, 1024), 4096)
        self.assertEqual(az.ext4_reserve_blocks(16384, 1024, 1024), 327)
        self.assertEqual(az.ext4_reserve_blocks(16384, 1024, 4096), 324)  # 81 clusters of 4 blocks
        out = subprocess.run(['dumpe2fs', '-h', str(self.disk)], capture_output=True, text=True).stdout
        room = self.fs.room()
        self.assertEqual(room['blocks'], int(re.search(r'^Block count:\s+(\d+)', out, re.M)[1]))
        self.assertEqual(room['free_blocks'], int(re.search(r'^Free blocks:\s+(\d+)', out, re.M)[1]))
        self.assertEqual(room['free_inodes'], int(re.search(r'^Free inodes:\s+(\d+)', out, re.M)[1]))
        reserve = az.ext4_reserve_blocks(room['blocks'], room['block_size'], room['block_size'])
        self.assertEqual(room['usable_kib'], (room['free_blocks'] - reserve) * room['block_size'] // 1024)

    @unittest.skipUnless(shutil.which('resize2fs'), 'resize2fs required')
    def test_patch_rootfs_grow_changes_only_the_size_and_repack_takes_only_that(self):
        original = self.root/'original.img'
        original.write_bytes(fixture(lzma.compress(self.disk.read_bytes())))
        out = self.root/'grown.ext2'
        with unittest.mock.patch.object(az, 'GEN1_ROOTFS_KIB', 20480), \
                unittest.mock.patch.object(az, 'GEN1_MIN_FREE_KIB', 1024), \
                unittest.mock.patch.object(az, 'GEN1_MIN_FREE_INODES', 64):
            self.assertEqual(az.main(['patch-rootfs', str(original), '--grow', '--output', str(out)]), 0)
            grown = az.ExtFS(out)
            self.assertEqual(out.stat().st_size, 20480 * 1024)
            keep = lambda g: {k: v for k, v in g.items() if k not in az.GROWN_FIELDS}
            self.assertEqual(keep(grown.geometry()), keep(self.fs.geometry()))
            self.assertEqual(grown.snapshot(), self.fs.snapshot())
            az.check_fs(out)
            self.assertGreater(grown.room()['usable_kib'], self.fs.room()['usable_kib'] + 3000)
            image = self.root/'grown.img'
            self.assertEqual(az.main(['repack', str(original), '--rootfs', str(out), '--grow',
                                      '--output', str(image)]), 0)
            unpacked = self.root/'grown-unpacked'
            self.assertEqual(az.main(['unpack', str(image), '--output', str(unpacked)]), 0)
            self.assertEqual((unpacked/'rootfs.ext2').read_bytes(), out.read_bytes())
            for argv in (['repack', str(original), '--rootfs', str(out)],          # growth not declared
                         ['repack', str(original), '--rootfs', str(self.disk), '--grow']):  # not grown
                with self.subTest(argv=argv[3:]), self.assertRaises(SystemExit) as exc:
                    az.main(argv + ['--output', str(self.root/'must-not-exist.img')])
                self.assertEqual(exc.exception.code, 2)
            self.assertFalse((self.root/'must-not-exist.img').exists())

    @unittest.skipUnless(shutil.which('resize2fs'), 'resize2fs required')
    def test_patch_rootfs_grow_refuses_a_rootfs_without_room(self):
        original = self.root/'original.img'
        original.write_bytes(fixture(lzma.compress(self.disk.read_bytes())))
        out = self.root/'too-full.ext2'
        with unittest.mock.patch.object(az, 'GEN1_ROOTFS_KIB', 20480), \
                unittest.mock.patch.object(az, 'GEN1_MIN_FREE_KIB', 20480):
            with self.assertRaises(SystemExit) as exc:
                az.main(['patch-rootfs', str(original), '--grow', '--output', str(out)])
        self.assertEqual(exc.exception.code, 2)
        self.assertFalse(out.exists())

    @unittest.skipUnless(shutil.which('resize2fs'), 'resize2fs required')
    def test_repack_grow_refuses_a_padded_file_whose_file_system_did_not_grow(self):
        # Review of b6f868f8: repack --grow checked only the file length, so padding passed as growth.
        original = self.root/'original.img'
        original.write_bytes(fixture(lzma.compress(self.disk.read_bytes())))
        padded = self.root/'padded.ext2'
        shutil.copyfile(self.disk, padded)
        with padded.open('r+b') as f:
            f.truncate(20480 * 1024)  # the file reaches the target; its file system stays 16 MiB
        az.check_fs(padded)
        self.assertEqual(az.ExtFS(padded).room()['blocks'], self.fs.room()['blocks'])
        out = self.root/'must-not-exist.img'
        with unittest.mock.patch.object(az, 'GEN1_ROOTFS_KIB', 20480), self.assertRaises(SystemExit) as exc:
            az.main(['repack', str(original), '--rootfs', str(padded), '--grow', '--output', str(out)])
        self.assertEqual(exc.exception.code, 2)
        self.assertFalse(out.exists())

    @unittest.skipUnless(shutil.which('resize2fs'), 'resize2fs required')
    def test_repack_grow_refuses_a_shrunk_file_system(self):
        # Review of b6f868f8: a real shrink in a file of the target length passed as growth.
        big = self.root/'big.ext2'
        shutil.copyfile(self.disk, big)
        with big.open('r+b') as f:
            f.truncate(24576 * 1024)
        subprocess.run(['resize2fs', '-f', str(big), '24576K'], check=True, capture_output=True)
        original = self.root/'original.img'
        original.write_bytes(fixture(lzma.compress(big.read_bytes())))
        shrunk = self.root/'shrunk.ext2'
        shutil.copyfile(big, shrunk)
        subprocess.run(['resize2fs', '-f', str(shrunk), '20480K'], check=True, capture_output=True)
        with shrunk.open('r+b') as f:
            f.truncate(20480 * 1024)
        az.check_fs(shrunk)
        self.assertEqual(az.ExtFS(shrunk).room()['blocks'], 20480)
        self.assertEqual(az.ExtFS(shrunk).snapshot(), az.ExtFS(big).snapshot())  # only the size differs
        out = self.root/'must-not-exist.img'
        with unittest.mock.patch.object(az, 'GEN1_ROOTFS_KIB', 20480), self.assertRaises(SystemExit) as exc:
            az.main(['repack', str(original), '--rootfs', str(shrunk), '--grow', '--output', str(out)])
        self.assertEqual(exc.exception.code, 2)
        self.assertFalse(out.exists())

    @unittest.skipUnless(shutil.which('resize2fs'), 'resize2fs required')
    def test_grow_never_shrinks_or_passes_the_proven_partition_size(self):
        size = self.disk.stat().st_size // 1024
        with self.assertRaisesRegex(az.SafetyError, 'growth past'):
            self.fs.grow(az.GEN1_ROOTFS_KIB + 1)
        with self.assertRaisesRegex(az.SafetyError, 'shrink'):
            self.fs.grow(size - 1024)
        self.assertEqual(self.disk.stat().st_size // 1024, size)
        # Below the smallest rootfs inMusic shipped for any 1st-gen group (5.0.4) and a JP07 deck's
        # measured partition; Gen1RootfsRoomTests checks the 5.0.4 images themselves in the lab.
        for what, kib in (('SC5000 5.0.4', 504076), ('SC6000 5.0.4', 504278), ('PRIME4 5.0.4', 503937),
                          ('JP07 partition', 512000)):
            self.assertLess(az.GEN1_ROOTFS_KIB, kib, what)

    def test_timesync_seed_dropin_and_non_clobbering_timezone_restore(self):
        import os
        overlay = Path(__file__).resolve().parents[2] / 'rootfs-overlay'
        az.inject(self.fs, None, overlay=overlay)

        # 1. enginex-etc-reset stays out of sysinit.target.wants and orders before var/var-lib mounts.
        etc_reset_unit = self.fs.read('/usr/lib/systemd/system/enginex-etc-reset.service')
        self.assertIn(b'Before=etc.mount\n', etc_reset_unit)
        self.assertNotIn(b'var.mount', etc_reset_unit)
        self.assertNotIn(b'sysinit.target', etc_reset_unit)
        with self.assertRaises(az.SafetyError):
            self.fs.entries('/usr/lib/systemd/system/sysinit.target.wants')

        # 2. systemd-timesyncd drop-in runs separate seed script with -+ and finite TimeoutStartSec.
        seed_dropin = self.fs.read('/usr/lib/systemd/system/systemd-timesyncd.service.d/10-enginex-seed.conf')
        self.assertIn(b'TimeoutStartSec=15s\n', seed_dropin)
        self.assertIn(b'ExecStartPre=-+/usr/libexec/enginex-timesync-seed\n', seed_dropin)
        seed_script = overlay / 'usr/libexec/enginex-timesync-seed'
        self.assertTrue(os.access(seed_script, os.X_OK))

        ts = self.root / 'timestamp'
        ts.write_text('20261030114129\n')
        build_epoch = 1793360489
        os.utime(ts, (build_epoch, build_epoch))

        # Missing lib_dir: seed script skips without creating lib_dir and exits 0.
        missing_lib = self.root / 'missing-var-lib'
        r = subprocess.run(['sh', str(seed_script)],
                           env=dict(os.environ, ENGINEX_TIMESTAMP_FILE=str(ts), ENGINEX_TIMESYNC_LIB_DIR=str(missing_lib)),
                           capture_output=True)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertFalse(missing_lib.exists())

        # Existing lib_dir: seeds missing/older clock to build_epoch, preserves newer clock, and never fails on symlink.
        lib_dir = self.root / 'var-lib'
        lib_dir.mkdir()
        seed_env = dict(os.environ, ENGINEX_TIMESTAMP_FILE=str(ts), ENGINEX_TIMESYNC_LIB_DIR=str(lib_dir))
        r = subprocess.run(['sh', str(seed_script)], env=seed_env, capture_output=True)
        self.assertEqual(r.returncode, 0, r.stderr)
        clock = lib_dir / 'systemd/timesync/clock'
        self.assertEqual(int(clock.stat().st_mtime), build_epoch)

        newer = build_epoch + 86400
        os.utime(clock, (newer, newer))
        r = subprocess.run(['sh', str(seed_script)], env=seed_env, capture_output=True)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(int(clock.stat().st_mtime), newer)

        os.utime(clock, (build_epoch - 3600, build_epoch - 3600))
        r = subprocess.run(['sh', str(seed_script)], env=seed_env, capture_output=True)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(int(clock.stat().st_mtime), build_epoch)

        clock.unlink()
        sentinel = self.root / 'clock-sentinel'
        sentinel.write_text('keep')
        os.utime(sentinel, (build_epoch - 7200, build_epoch - 7200))
        clock.symlink_to(sentinel)
        r = subprocess.run(['sh', str(seed_script)], env=seed_env, capture_output=True)
        self.assertEqual(r.returncode, 0)
        self.assertEqual(int(sentinel.stat().st_mtime), build_epoch - 7200)

        # 3. restore_timezone in PREPARE respects newer Engine.conf TZEnvVar and saves explicit UTC when state is newer.
        zdir = self.root / 'zoneinfo'
        for rel in ('Asia/Shanghai', 'Europe/London', 'Etc/UTC', 'UTC'):
            (zdir / rel).parent.mkdir(parents=True, exist_ok=True)
            (zdir / rel).write_bytes(b'TZif2')

        pcode = self.root / 'product-code'
        pcode.write_bytes(b'JP07\x00')
        data = self.root / 'data-ts'
        tz_state = data / 'enginex/state/timezone'
        tz_state.parent.mkdir(parents=True)
        tz_state.write_text('Asia/Shanghai\n')
        os.utime(tz_state, (1000, 1000))
        seed = self.root / 'seed-ts'
        seed.mkdir()

        env = dict(os.environ,
                   ENGINEX_TZ_STATE_FILE=str(tz_state),
                   ENGINEX_CONFIG_DIR=str(data / '.config'),
                   ENGINEX_ZONEINFO_DIR=str(zdir),
                   ENGINEX_PRODUCT_CODE_FILE=str(pcode))
        prep_script = az.PREPARE.replace(b'/data', str(data).encode()).replace(
            b'/usr/share/enginex', str(seed).encode())

        # Missing Engine.conf -> restores Asia/Shanghai.
        r = subprocess.run(['sh', '-s'], input=prep_script, env=env, capture_output=True)
        self.assertEqual(r.returncode, 0, r.stderr)
        conf = data / '.config/jp07.user.settings/AIR Music Technology/Engine.conf'
        self.assertEqual(conf.read_text(), '[TimeDateSettings]\nTZEnvVar=Asia/Shanghai\n')

        # Newer Engine.conf with TZEnvVar=Europe/London (clean shutdown) MUST NOT be overwritten!
        conf.write_text('[General]\nPlayer=1\n\n[TimeDateSettings]\n24HourFormat=true\nTZEnvVar=Europe/London\n')
        os.utime(conf, (2000, 2000))
        r = subprocess.run(['sh', '-s'], input=prep_script, env=env, capture_output=True)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn('TZEnvVar=Europe/London\n', conf.read_text())

        # Newer state/timezone (e.g. user changed zone to Asia/Shanghai or UTC, followed by SIGKILL) DOES restore!
        os.utime(tz_state, (3000, 3000))
        r = subprocess.run(['sh', '-s'], input=prep_script, env=env, capture_output=True)
        self.assertEqual(r.returncode, 0, r.stderr)
        text = conf.read_text()
        self.assertIn('Player=1\n', text)
        self.assertIn('24HourFormat=true\n', text)
        self.assertIn('TZEnvVar=Asia/Shanghai\n', text)

        # User explicitly chose UTC (state/timezone newer than Engine.conf) -> restores TZEnvVar=UTC!
        tz_state.write_text('UTC\n')
        os.utime(conf, (3000, 3000))
        os.utime(tz_state, (4000, 4000))
        r = subprocess.run(['sh', '-s'], input=prep_script, env=env, capture_output=True)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn('TZEnvVar=UTC\n', conf.read_text())


class OverlayOrderingTests(unittest.TestCase):
    OVERLAY = Path(__file__).resolve().parents[2]/'rootfs-overlay'

    def runtime_units(self):
        """Overlay services that write the runtime dir: they start the suite (enginex-suite-exec
        unpacks into /data/enginex/bin), run the guard or name the dir themselves."""
        bindir = self.OVERLAY/'usr/local/bin'
        suite = {'/usr/local/bin/'+p.name for p in bindir.iterdir()
                 if p.is_file() and p.name != 'enginex-suite-exec' and b'enginex-suite-exec' in p.read_bytes()}
        self.assertTrue({'/usr/local/bin/rb-bridge', '/usr/local/bin/enginex-deemixd',
                         '/usr/local/bin/stream-sync'} <= suite, suite)
        units = {}
        for unit in sorted((self.OVERLAY/'etc/systemd/system').glob('*.service')):
            text = unit.read_text() + ''.join(p.read_text() for p in sorted(unit.parent.glob(unit.name+'.d/*.conf')))
            progs = {re.sub(r'^[-+@!:]*', '', line.split('=', 1)[1]).split(' ')[0]
                     for line in text.splitlines() if re.match(r'Exec\w*=\S', line)}
            if progs & (suite | {'/etc/enginex/engine-ui-guard'}) or re.search(('/data/(' + legacy.DATA_NAME + '|enginex)\\b'), text):
                units[unit.name] = text
        return units

    def test_runtime_dir_writers_start_after_prepare_migrated_it(self):
        # #103: prepare renames /data/enginex to /data/enginex and then links the old name.
        # rb-bridge and enginex-deemixd started beside it on a blank /data or the first boot after
        # an update: enginex-suite-exec's mkdir -p /data/enginex/bin between the two steps
        # left a real old dir, so the UI hook's /data/enginex/ui was gone for that boot and
        # the next boot's merge set its writes aside.
        prepare = az.UNITS['enginex-prepare.service']
        before = set(re.search(r'^Before=(.*)$', prepare, re.M).group(1).split())
        units = self.runtime_units()
        self.assertTrue({'rb-bridge.service', 'enginex-deemixd.service', 'stream-sync.service',
                         'engine-guardian.service'} <= set(units), sorted(units))
        for name, text in units.items():
            after = {u for line in re.findall(r'^After=(.*)$', text, re.M) for u in line.split()}
            with self.subTest(unit=name):
                self.assertTrue(name in before or 'enginex-prepare.service' in after,
                                name+' is not ordered after enginex-prepare.service')
        # Ordering only: prepare itself must not wait for any of them (no cycle).
        prepare_after = {u for line in re.findall(r'^After=(.*)$', prepare, re.M) for u in line.split()}
        self.assertFalse(prepare_after & set(units), prepare_after & set(units))
        for name in ('rb-bridge.service', 'enginex-deemixd.service'):
            self.assertNotRegex(units[name], r'(?m)^(Requires|BindsTo|Requisite)=')

    def test_edisksd_hook_spawns_rb_bridge_only_after_prepare(self):
        # The edisksd hook runs the suite's rb-bridge for a rekordbox stick, and a stick
        # plugged in at power-on is mounted while prepare may be moving the runtime dir.
        hook = (self.OVERLAY.parent/'tools/edisksd-hook/edisksd_hook.c').read_text()
        self.assertIn('"/usr/local/bin/rb-bridge"', hook)
        dropin = (self.OVERLAY/'etc/systemd/system/edisksd.service.d/50-enginex-hook.conf').read_text()
        self.assertRegex(dropin, r'(?m)^After=.*\benginex-prepare\.service\b')
        # edisksd is a vendor unit: ordering only (CONTRIBUTING), and still preloads the hook.
        self.assertNotRegex(dropin, r'(?m)^(Requires|BindsTo|Requisite|Wants)=')
        self.assertIn('Environment=LD_PRELOAD=/etc/enginex/libedisksd-hook.so', dropin)


if __name__ == '__main__':
    unittest.main()


class Rename234UpgradeTests(unittest.TestCase):
    def test_234_r5_data_survives_migration_and_repeated_boot(self):
        with tempfile.TemporaryDirectory() as tmp:
            data = Path(tmp) / 'data'
            old, new = data / legacy.DATA_NAME, data / 'enginex'
            saved = {
                'ui/settings.conf': b'lang=zh-TW\nfour_deck_role=24\nconsole_https=1\nunknown=keep\n',
                'console-cert.pem': b'test-console-cert\n',
                'console-key.pem': b'test-console-key\n',
                'state/access-token': b'test-access-code\n',
                'state/deemix-config.json': b'{"quality":"flac"}\n',
                'ui/packs/en/Skins/Main.qml': b'// installed pack\n',
                'ui/mods-state.json': b'{"club":true}\n',
                'mods/club/mod.json': b'{"id":"club"}\n',
                'mods-state/club/.paused': b'enginex-deemixd.service\n',
                'ssh_host_ed25519_key': b'test-host-key\n',
                'ui-scale.env': b'ENGINEX_UI_SCALE=85\n',
                '.cache/account': b'cached-account\n',
            }
            for rel, content in saved.items():
                path = old / rel
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_bytes(content)
                path.chmod(0o600)
            native = {
                'StreamingAccounts.json': b'{"Deezer":"saved-account"}\n',
                '.config/jp07.user.settings/AIR Music Technology/Engine.conf': b'[Settings]\nName=My deck\n',
            }
            for rel, content in native.items():
                path = data / rel
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_bytes(content)
            script = az.MIGRATE.replace('/data', str(data))
            for boot in range(2):
                result = subprocess.run(['sh', '-eu', '-c', script], capture_output=True)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertTrue(new.is_dir() and not new.is_symlink())
                self.assertFalse(os.path.lexists(old))
                for rel, content in native.items():
                    self.assertEqual((data / rel).read_bytes(), content, rel)
                for rel, content in saved.items():
                    self.assertEqual((new / rel).read_bytes(), content, (boot, rel))
                    self.assertEqual((new / rel).stat().st_mode & 0o777, 0o600, rel)


MODINFO = shutil.which('modinfo', path=os.environ.get('PATH', '') + ':/usr/sbin:/sbin')


@unittest.skipUnless(MODINFO and all(shutil.which(t) for t in ('mke2fs', 'debugfs', 'objcopy')), 'mke2fs, debugfs, objcopy and modinfo needed')
class CheckHfsplusxTests(unittest.TestCase):
    """Gen-1 LoadPin loads kernel modules only from the rootfs: a release with no hfsplusx.ko there, or one
    built for another kernel, left every HFS+ stick read-only without a word (hfs-rw-deck.md). The
    build and the release gate check each 1st-gen rootfs with check-hfsplusx.sh."""

    KERNEL = '6.18.57-az01-2026-10-15-rt6'
    VERMAGIC = KERNEL + ' SMP preempt_rt ARMv7 p2v8 '

    def module(self, path, vermagic, name):
        path.parent.mkdir(parents=True, exist_ok=True)
        info = Path(str(path) + '.modinfo')
        info.write_bytes(f'vermagic={vermagic}\0name={name}\0'.encode())
        subprocess.run(['objcopy', '--add-section', f'.modinfo={info}', shutil.which('true'), str(path)], check=True)
        info.unlink()

    def check(self, tmp, modules, script='check-hfsplusx.sh', *args):
        root = Path(tmp)/'root'
        shutil.rmtree(root, ignore_errors=True)
        for kernel, vermagic in modules.items():
            moddir = root/'usr/lib/modules'/kernel
            if kernel == self.KERNEL:
                self.module(moddir/'kernel/net/wireless/cfg80211.ko', self.VERMAGIC, 'cfg80211')
                (moddir/'modules.dep').write_text('kernel/net/wireless/cfg80211.ko:\n')
            if vermagic:
                self.module(moddir/'extra/hfsplusx.ko', vermagic, 'hfsplusx')
        fs = Path(tmp)/'rootfs.ext2'
        fs.unlink(missing_ok=True)
        subprocess.run(['mke2fs', '-q', '-F', '-t', 'ext2', '-d', str(root), str(fs), '8M'], check=True)
        return subprocess.run(['bash', str(Path(__file__).resolve().parents[2]/script if '/' in script else Path(__file__).resolve().with_name(script)), str(fs), *args],
                              capture_output=True, text=True)

    def test_the_module_must_be_at_the_loadpin_path_for_the_images_kernel(self):
        with tempfile.TemporaryDirectory(prefix='hfsplusx-', dir=TEST_TMPDIR) as tmp:
            self.assertEqual(self.check(tmp, {self.KERNEL: self.VERMAGIC}).returncode, 0)
            for case, modules, why in [
                    ('missing', {self.KERNEL: None}, 'no /usr/lib/modules/'),
                    ('another build', {self.KERNEL: self.KERNEL + ' SMP ARMv7 p2v8 '}, 'vermagic'),
                    ('another kernel', {self.KERNEL: None, '6.18.58-az01': '6.18.58-az01 SMP preempt_rt ARMv7 p2v8 '}, 'want one kernel')]:
                with self.subTest(case):
                    r = self.check(tmp, modules)
                    self.assertNotEqual(r.returncode, 0, r.stderr)
                    self.assertIn(why, r.stderr)

    def test_the_release_build_builds_the_module_and_checks_every_group(self):
        script = Path(__file__).resolve().with_name('build-release.sh').read_text()
        # No hand-placed module (the lab directory, kernel-modules/out): built from the pinned sources unless HFSPLUSX_KO is set.
        self.assertIn('bash kernel-modules/build-release-module.sh "$WORK/stock-SC5000/rootfs.ext2" "$WORK/hfsplusx.ko"', script)
        self.assertNotIn('/lib/modules/"*/extra/hfsplusx.ko', script)
        self.assertIn('sh tools/az01-packer/check-hfsplusx.sh "$WORK/$base.ext2"', script)
        gate = Path(__file__).resolve().with_name('release-gate.sh').read_text()
        self.assertIn('check-hfsplusx.sh" "$W/new-img/rootfs.ext2"', gate)

    def test_the_module_build_refuses_a_kernel_its_sources_are_not_pinned_for(self):
        with tempfile.TemporaryDirectory(prefix='hfsplusx-', dir=TEST_TMPDIR) as tmp:
            r = self.check(tmp, {'6.18.58-az01': '6.18.58-az01 SMP preempt_rt ARMv7 p2v8 '}, 'kernel-modules/build-release-module.sh', str(Path(tmp)/'out.ko'))
            self.assertEqual(r.returncode, 1, r.stderr)
            self.assertIn("the rootfs kernel '6.18.58-az01' is not 6.18.57-*", r.stderr)
            self.assertFalse((Path(tmp)/'out.ko').exists())


@unittest.skipUnless(all(shutil.which(x) for x in ('debugfs', 'e2fsck', 'resize2fs')),
                     'needs e2fsprogs (debugfs, e2fsck, resize2fs)')
class Gen1RootfsRoomTests(unittest.TestCase):
    """5.3.0.5 RC: what EngineX adds left the stock 5.3.0 rootfs with 4 064 free 1 KiB blocks, under
    the 4 096 the ext4 driver keeps back, so the release gate's and nspawn boot's mounted copies
    could not take one new file. A release rootfs is grown and must keep its margin."""

    @unittest.skipUnless(all(p.is_file() for p in STOCK_530), 'needs the official 5.3.0 images of the build-host lab')
    def test_release_rootfs_of_every_gen1_group_keeps_room(self):
        script = (Path(__file__).resolve().parent/'build-release.sh').read_text()
        calls = re.findall(r'az01_packer\.py (patch-rootfs|repack) (.*?)>/dev/null', script, re.S)
        flags = {cmd: args.split() for cmd, args in calls}
        self.assertIn('--grow', flags['patch-rootfs'], 'the release build does not grow the rootfs')
        self.assertIn('--grow', flags['repack'], 'the release build repacks without --grow')
        self.assertNotIn('$STAGE/lib/modules', script, 'Gen-1 /lib is a symlink to usr/lib')
        self.assertIn('$STAGE/usr/lib/modules/$krel/extra', script)
        overlay = Path(__file__).resolve().parents[2]/'rootfs-overlay'
        with tempfile.TemporaryDirectory(prefix='az01-room-', dir=TEST_TMPDIR) as tmp:
            # The stage build-release.sh makes, its generated files at d40ad7b2's sizes plus headroom
            # (suite 6.4 MB -> 7 MiB, UI packs 0.4 MB -> 1 MiB, fsck.hfsplus 0.7 MB -> 768 KiB).
            stage = Path(tmp)/'overlay'
            shutil.copytree(overlay, stage, symlinks=True)
            (stage/'README.md').unlink(missing_ok=True)
            (stage/'etc/enginex/release-tag').write_text('v5.3.0.5\n')
            share = stage/'usr/share/enginex'
            share.mkdir(parents=True, exist_ok=True)
            (share/'enginex-suite.xz').write_bytes(os.urandom(7 << 20))
            (share/'ui-packs.tar.xz').write_bytes(os.urandom(1 << 20))
            (stage/'usr/sbin').mkdir(parents=True, exist_ok=True)
            (stage/'usr/sbin/fsck.hfsplus').write_bytes(os.urandom(768 << 10))
            moddir = stage/'usr/lib/modules/6.18.57-az01-2026-10-15-rt6/extra'
            moddir.mkdir(parents=True, exist_ok=True)
            (moddir/'hfsplusx.ko').write_bytes(os.urandom(256 << 10))
            for image in STOCK_530:
                with self.subTest(image=image.name):
                    out = Path(tmp)/(image.stem + '.ext2')
                    argv = ['patch-rootfs', str(image), '--overlay', str(stage), '--set-version', '5.3.0.5',
                            *[f for f in flags['patch-rootfs'] if f == '--grow'], '--output', str(out)]
                    self.assertEqual(az.main(argv), 0)
                    room = az.ExtFS(out).room()
                    self.assertGreaterEqual(room['usable_kib'], az.GEN1_MIN_FREE_KIB, room)
                    self.assertGreaterEqual(room['free_inodes'], az.GEN1_MIN_FREE_INODES, room)
                    self.assertLessEqual(out.stat().st_size, az.GEN1_ROOTFS_KIB * 1024)
                    out.unlink()

    @unittest.skipUnless(all((LAB/'fw'/f'{n}-5.0.4-Update.img').is_file() for n in ('SC5000', 'SC6000', 'PRIME4')),
                         'needs the official 5.0.4 images of the build-host lab')
    def test_grown_rootfs_fits_where_the_504_rootfs_was_installed(self):
        # Every 1st-gen deck took inMusic's 5.0.4 rootfs (the same board device trees as 5.3.0), and
        # the stock updater refuses data that does not fit: its size is a floor for the partition.
        with tempfile.TemporaryDirectory(prefix='az01-504-', dir=TEST_TMPDIR) as tmp:
            for name in ('SC5000', 'SC6000', 'PRIME4'):
                with self.subTest(group=name):
                    xz = Path(tmp)/'rootfs.xz'
                    xz.write_bytes(az.Image((LAB/'fw'/f'{name}-5.0.4-Update.img').read_bytes()).payload())
                    listing = subprocess.run(['xz', '--robot', '-l', str(xz)], capture_output=True, text=True,
                                             check=True).stdout
                    size = int(next(l for l in listing.splitlines() if l.startswith('totals')).split('\t')[4])
                    self.assertGreaterEqual(size, az.GEN1_ROOTFS_KIB * 1024)
                    xz.unlink()
