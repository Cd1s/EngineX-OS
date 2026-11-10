import hashlib
import importlib.util
import io
import os
import shutil
import subprocess
import tarfile
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location('build_data_bundle', HERE / 'build-data-bundle.py')
bdb = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bdb)

TOP = 'SCLIVE4-5.3.0.1.1-EngineX-OS-Data-Bundle'


def fake_elf(machine, tag):
    return b'\x7fELF' + bytes(14) + machine.to_bytes(2, 'little') + tag


class DataBundleTest(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.suite = self.tmp / 'sc5000-suite'
        self.suite_bytes = fake_elf(bdb.EM_ARM, b'suite')
        self.suite.write_bytes(self.suite_bytes)
        self.hook = self.tmp / 'libengine-ui-hook.so'
        self.hook_bytes = fake_elf(bdb.EM_ARM, b'hook')
        self.hook.write_bytes(self.hook_bytes)
        self.ui = self.ui_tar({'packs/a/Main.qml': b'Item {}', 'icons/x.svg': b'<svg/>'})

    def tearDown(self):
        subprocess.run(['rm', '-rf', str(self.tmp)], check=True)

    def ui_tar(self, files, name='packs.tar.gz'):
        path = self.tmp / name
        with tarfile.open(path, 'w:gz') as t:
            for n, data in files.items():
                info = tarfile.TarInfo(n)
                info.size = len(data)
                t.addfile(info, io.BytesIO(data))
        return path

    def build(self, out='out', **kw):
        args = dict(version='5.3.0.1.1', suite=self.suite, hook=self.hook, ui_packs=self.ui, out=self.tmp / out)
        args.update(kw)
        return bdb.build(**args)

    def extract(self, result):
        dest = self.tmp / 'x'
        with tarfile.open(result['bundle']) as t:
            if hasattr(tarfile, 'data_filter'):
                t.extractall(dest, filter='data')
            else:
                t.extractall(dest)
        return dest / TOP

    def test_bundle_layout_and_manifest(self):
        r = self.build()
        bundle = Path(r['bundle'])
        self.assertEqual(bundle.name, TOP + '.tar.gz')
        self.assertEqual((bundle.parent / 'SHA256SUMS.txt').read_text(),
                         f"{hashlib.sha256(bundle.read_bytes()).hexdigest()}  {bundle.name}\n")
        src = self.extract(r)
        self.assertEqual((src / 'version.txt').read_text(), '5.3.0.1.1\n')
        self.assertEqual((src / 'bin/sc5000-suite').read_bytes(), self.suite_bytes)
        self.assertEqual((src / 'lib/libengine-ui-hook.so').read_bytes(), self.hook_bytes)
        for applet in bdb.APPLETS:
            self.assertIn(f'sc5000-suite" {applet} ', (src / 'bin' / applet).read_text())
            self.assertTrue((src / 'bin' / applet).stat().st_mode & 0o100)
        self.assertTrue((src / 'boot-activate.sh').exists())
        self.assertTrue((src / 'boot-activate.sh').stat().st_mode & 0o100)
        boot = (src / 'boot-activate.sh').read_text()
        self.assertIn('EnvironmentFile=-/data/sc5000-custom/ui-scale.env', boot)
        self.assertIn('Environment=HOME=/tmp/sc5000-engine-home MESA_SHADER_CACHE_MAX_SIZE=32M', boot)
        self.assertIn('Environment=QML_DISABLE_DISK_CACHE=1', boot)
        self.assertIn('ExecStartPre=-/bin/mkdir -p -m 0700 /tmp/sc5000-engine-home', boot)
        self.assertIn('ExecStartPost=-/bin/sh /run/sc5000/perf-tune.sh', boot)
        self.assertIn('enginex-stream-sync.service', boot)
        self.assertIn('Wants=enginex-stream-sync.service', boot)
        self.assertEqual(boot.count('CPUAffinity=3'), 3)
        for token in (
            'scaling_governor',
            'scaling_min_freq',
            '1200000',
            'up_threshold:50',
            'sampling_down_factor:10',
            'ignore_nice_load:1',
            'ffb20000:0 ffa30000:1 ehci_hcd:2 ff540000:2 eth0:2',
            'smp_affinity_list',
            'smp_affinity',
            'taskset -pc',
            'chrt -f -p 99',
        ):
            self.assertIn(token, boot)
        self.assertIn('/run/sc5000/perf-tune.sh', (src / 'uninstall.sh').read_text())
        self.assertTrue((src / '99-sc5000-custom.conf').exists())
        self.assertTrue((src / 'host-install-userdata.py').exists())
        self.assertTrue((src / 'host-install-fastboot.sh').exists())
        self.assertTrue((src / 'host-install-usbupdate.py').stat().st_mode & 0o100)
        code = [l for l in (src / 'boot-activate.sh').read_text().splitlines() if not l.lstrip().startswith('#')]
        self.assertFalse([l for l in code if 'KillMode' in l])  # #116
        listed = {}
        for line in (src / 'SHA256SUMS').read_text().splitlines():
            digest, name = line.split('  ')
            listed[name] = digest
            self.assertEqual(hashlib.sha256((src / name).read_bytes()).hexdigest(), digest, name)
        files = {str(p.relative_to(src)) for p in src.rglob('*') if p.is_file()}
        self.assertEqual(set(listed) | {'SHA256SUMS'}, files)

    def test_gen2_img_wraps_the_bundle_and_installs_with_unlock(self):
        r = self.build()
        img = Path(r['image'])
        self.assertEqual(img.name, 'SCLIVE4-5.3.0.1.1-EngineX-OS-Denon-SC-LIVE-4-JP21-Update.img')
        raw = img.read_bytes()
        bundle = Path(r['bundle']).read_bytes()
        self.assertEqual(raw[128:], bundle)
        lines = raw[:128].rstrip(b'\x00').decode().split('\n')
        self.assertEqual(lines[:4], ['ENGINEX_DATA_BUNDLE_IMG_V1', hashlib.sha256(bundle).hexdigest(), 'SCLIVE4', '5.3.0.1.1'])
        for prefix in bdb.PREFIXES:
            self.assertIn(prefix, bdb.MODEL_NAMES)
        dest = self.tmp / 'data/enginex'
        alsa = self.tmp / 'data/system/var/overlay/lib/alsa/conf.d'
        env = {'PATH': os.environ.get('PATH', '/usr/bin:/bin'), 'SC5000_CUSTOM': str(dest), 'SC5000_ALSA_OVERLAY': str(alsa)}
        # install.sh <img>, run from a copy of the bundle's own install.sh
        src = self.extract(r)
        run = lambda p: subprocess.run(['sh', str(src / 'install.sh'), str(p)], env=env, capture_output=True, text=True)
        out = run(img)
        self.assertEqual(out.returncode, 0, out.stderr)
        self.assertEqual((dest / 'gen2-unlocked').read_text(), '1\n')
        self.assertEqual((dest / 'version.txt').read_text(), '5.3.0.1.1\n')
        self.assertEqual([p.name for p in dest.glob('.img-install.*')], [])  # scratch dir removed
        # a flipped byte in the payload and a truncated image install nothing
        shutil.rmtree(dest)
        bad = self.tmp / 'bad.img'
        bad.write_bytes(raw[:-1] + bytes([raw[-1] ^ 1]))
        self.assertNotEqual(run(bad).returncode, 0)
        bad.write_bytes(raw[:len(raw) // 2])
        self.assertNotEqual(run(bad).returncode, 0)
        self.assertFalse((dest / 'version.txt').exists())

    def test_edisksd_hook_is_packaged_and_activated(self):
        self.assertFalse((self.extract(self.build('none')) / 'lib/libedisksd-hook.so').exists())
        shutil.rmtree(self.tmp / 'x')
        edisksd = self.tmp / 'libedisksd-hook.so'  # found next to --hook
        edisksd.write_bytes(fake_elf(bdb.EM_ARM, b'edisksd'))
        src = self.extract(self.build('out-edisksd'))
        self.assertEqual((src / 'lib/libedisksd-hook.so').read_bytes(), edisksd.read_bytes())
        self.assertTrue((src / 'lib/libedisksd-hook.so').stat().st_mode & 0o100)
        self.assertIn('libedisksd-hook.so', (src / 'SHA256SUMS').read_text())
        boot = (src / 'boot-activate.sh').read_text()
        self.assertIn('edisksd.service.d/99-sc5000.conf', boot)
        self.assertIn('systemctl restart edisksd.service', boot)  # blocking, only when the drop-in changed
        self.assertIn('cmp -s', boot)
        self.assertIn('--mount-path "$m"', boot)
        self.assertIn('edisksd.service.d/99-sc5000.conf', (src / 'uninstall.sh').read_text())
        edisksd.write_bytes(fake_elf(bdb.EM_AARCH64, b'edisksd'))
        with self.assertRaises(SystemExit):
            self.build('out-wrong-arch')

    def test_rebuild_is_identical(self):
        a, b = self.build('a'), self.build('b')
        self.assertEqual(a['sha256'], b['sha256'])

    def test_second_generation_prefixes(self):
        # These run SC LIVE 4's Engine on signed AZ0x firmware (#68, #69).
        for prefix in ('PRIME4PLUS', 'PRIMEGOPLUS', 'SCLIVE2', 'MIXSTREAMPROPLUS', 'MIXSTREAMPROGO'):
            with self.subTest(prefix=prefix):
                r = self.build(out=f'out-{prefix}', prefix=prefix)
                self.assertTrue(os.path.basename(r['bundle']).startswith(f'{prefix}-5.3.0.1.1-EngineX-OS-Data-Bundle'))
                with tarfile.open(r['bundle']) as t:
                    self.assertTrue(all(n.startswith(f'{prefix}-5.3.0.1.1-EngineX-OS-Data-Bundle') for n in t.getnames()))
                    top = f'{prefix}-5.3.0.1.1-EngineX-OS-Data-Bundle'
                    readme = t.extractfile(f'{top}/README-INSTALL.txt').read().decode()
                    self.assertIn(bdb.MODELS[prefix], readme)
                    self.assertIn(f'tar -xzf {top}.tar.gz', readme)

    def test_az04_bundle_is_64_bit_with_var_lib_alsa(self):
        # PRIME 4 G2 / SYSTEM ONE (#76): AArch64 Engine, /var/lib bound from /data/system/var-lib.
        a64 = {'suite': self.tmp / 'suite64', 'hook': self.tmp / 'hook64'}
        a64['suite'].write_bytes(fake_elf(bdb.EM_AARCH64, b'suite'))
        a64['hook'].write_bytes(fake_elf(bdb.EM_AARCH64, b'hook'))
        for prefix in bdb.AZ04:
            with self.subTest(prefix=prefix):
                with self.assertRaises(SystemExit):  # armhf parts
                    self.build(out=f'bad-{prefix}', prefix=prefix)
                r = self.build(out=f'out-{prefix}', prefix=prefix, **a64)
                top = f'{prefix}-5.3.0.1.1-EngineX-OS-Data-Bundle'
                with tarfile.open(r['bundle']) as t:
                    self.assertEqual(t.extractfile(f'{top}/alsa-dir').read(), b'system/var-lib/alsa/conf.d\n')
                    readme = t.extractfile(f'{top}/README-INSTALL.txt').read().decode()
                self.assertIn('/data/system/var-lib/alsa/conf.d/99-sc5000-custom.conf', readme)
                self.assertIn('64-bit (AZ04)', readme)
        with self.assertRaises(SystemExit):  # 64-bit parts in an armhf bundle
            self.build(out='bad-sclive4', **a64)
        src = self.extract(self.build())
        self.assertEqual((src / 'alsa-dir').read_text(), 'system/var/overlay/lib/alsa/conf.d\n')
        self.assertNotIn('64-bit', (src / 'README-INSTALL.txt').read_text())

    def test_install_uses_bundle_alsa_dir(self):
        hook64 = self.tmp / 'hook64'
        hook64.write_bytes(fake_elf(bdb.EM_AARCH64, b'hook'))
        suite64 = self.tmp / 'suite64'
        suite64.write_bytes(fake_elf(bdb.EM_AARCH64, b'suite'))
        r = self.build(prefix='PRIME4G2', suite=suite64, hook=hook64)
        dest = self.tmp / 'root/sc5000-custom'  # no /data on the test host: relative to dest
        with tarfile.open(r['bundle']) as t:
            t.extractall(self.tmp / 'x', **({'filter': 'data'} if hasattr(tarfile, 'data_filter') else {}))
        src = self.tmp / 'x/PRIME4G2-5.3.0.1.1-EngineX-OS-Data-Bundle'
        if Path('/data').is_dir():
            self.skipTest('/data exists on this host')
        self.sh(src / 'install.sh', dest)
        conf = self.tmp / 'root/system/var-lib/alsa/conf.d/99-sc5000-custom.conf'
        self.assertTrue(conf.exists())
        self.sh(dest / 'bundle/uninstall.sh', dest)
        self.assertFalse(conf.exists())

    def test_refuses_bad_input(self):
        (self.tmp / 'taken').mkdir()
        for kw in ({'version': '5.3'}, {'version': '5.3.0-x'}, {'out': 'taken'},
                   {'suite': self.tmp / 'packs.tar.gz'},
                   {'ui_packs': self.ui_tar({'icons/x.svg': b''}, 'nopacks.tar.gz')},
                   {'ui_packs': self.ui_tar({'packs/x': b'', '../evil': b''}, 'evil.tar.gz')},
                   {'prefix': 'PRIME4'}, {'prefix': '../x'}):
            with self.subTest(kw=kw), self.assertRaises(SystemExit):
                self.build(**kw)

    def sh(self, script, dest, alsa_dest=None, check=True):
        env = {'PATH': os.environ.get('PATH', '/usr/local/bin:/usr/bin:/bin:/sbin'), 'SC5000_CUSTOM': str(dest)}
        if alsa_dest:
            env['SC5000_ALSA_OVERLAY'] = str(alsa_dest)
        return subprocess.run(['sh', str(script)], env=env,
                              capture_output=True, text=True, check=check)

    def test_install_then_uninstall_keeps_user_data(self):
        src = self.extract(self.build())
        dest = self.tmp / 'data/sc5000-custom'
        alsa_dest = self.tmp / 'data/system/var/overlay/lib/alsa/conf.d'
        dest.mkdir(parents=True)
        (dest / 'authorized_keys').write_text('ssh-ed25519 AAAA test\n')
        (dest / 'ui').mkdir()
        (dest / 'ui/safe-mode').write_text('1')
        self.sh(src / 'install.sh', dest, alsa_dest=alsa_dest)
        self.assertEqual((dest / 'bundle/bin/sc5000-suite').read_bytes(), self.suite_bytes)
        self.assertEqual((dest / 'ui/packs/a/Main.qml').read_bytes(), b'Item {}')
        self.assertTrue((dest / 'ui/icons/x.svg').exists())
        self.assertFalse((dest / 'ui/safe-mode').exists())
        self.assertTrue((dest / 'bundle/boot-activate.sh').exists())
        self.assertTrue((dest / 'bundle/boot-activate.sh').stat().st_mode & 0o100)
        self.assertTrue((dest / 'bundle/99-sc5000-custom.conf').exists())
        self.assertTrue((alsa_dest / '99-sc5000-custom.conf').exists())
        self.assertEqual((dest / 'version.txt').read_text(), '5.3.0.1.1\n')
        self.assertEqual(oct(dest.stat().st_mode & 0o777), '0o700')
        self.assertEqual([p.name for p in dest.glob('.bundle.*')], [])
        self.sh(src / 'install.sh', dest, alsa_dest=alsa_dest)  # reinstall over itself
        self.sh(dest / 'bundle/uninstall.sh', dest, alsa_dest=alsa_dest)
        self.assertEqual(sorted(p.name for p in dest.iterdir()), ['authorized_keys', 'ui'])
        self.assertEqual(list((dest / 'ui').iterdir()), [])
        self.assertFalse((alsa_dest / '99-sc5000-custom.conf').exists())
        self.assertNotEqual(self.sh(src / 'uninstall.sh', dest, alsa_dest=alsa_dest, check=False).returncode, 0)

    def test_uninstall_keeps_packs_installed_by_someone_else(self):
        src = self.extract(self.build())
        dest = self.tmp / 'custom'
        self.sh(src / 'install.sh', dest)
        (dest / 'ui/.pack-id').write_text('image\n')  # the image's prepare unit took over
        self.sh(dest / 'bundle/uninstall.sh', dest)
        self.assertTrue((dest / 'ui/packs/a/Main.qml').exists())
        self.assertFalse((dest / 'bundle').exists())

    def test_damaged_bundle_installs_nothing(self):
        src = self.extract(self.build())
        (src / 'bin/sc5000-suite').write_bytes(b'\x7fELF tampered')
        dest = self.tmp / 'custom'
        r = self.sh(src / 'install.sh', dest, check=False)
        self.assertNotEqual(r.returncode, 0)
        self.assertIn('SHA256SUMS mismatch', r.stderr)
        self.assertFalse(dest.exists())

    def test_host_install_userdata(self):
        if subprocess.run(['which', 'debugfs'], capture_output=True).returncode != 0:
            self.skipTest('debugfs not installed on this host')
        if subprocess.run(['which', 'mkfs.ext4'], capture_output=True).returncode != 0:
            self.skipTest('mkfs.ext4 not installed on this host')
        r = self.build()
        img = self.tmp / 'userdata.img'
        subprocess.run(['dd', 'if=/dev/zero', f'of={img}', 'bs=1M', 'count=32'], check=True, capture_output=True)
        subprocess.run(['mkfs.ext4', '-F', str(img)], check=True, capture_output=True)

        # Pre-seed an existing user file in userdata.img
        subprocess.run(['debugfs', '-w', '-R', 'mkdir /Music', str(img)], check=True, capture_output=True)
        subprocess.run(['debugfs', '-w', '-R', 'write /etc/hosts /Music/my_track.mp3', str(img)], check=True, capture_output=True)

        hspec = importlib.util.spec_from_file_location('host_install_userdata', HERE / 'host-install-userdata.py')
        hmod = importlib.util.module_from_spec(hspec)
        hspec.loader.exec_module(hmod)

        res = hmod.inject(img, r['bundle'])
        self.assertEqual(res['status'], 'ok')

        fsck = subprocess.run(['e2fsck', '-f', '-y', str(img)], capture_output=True, text=True)
        self.assertIn(fsck.returncode, (0, 1, 2))

        stat_out = subprocess.run(['debugfs', '-R', 'stat /Music/my_track.mp3', str(img)], capture_output=True, text=True).stdout
        self.assertIn('regular', stat_out)

        hook_out = subprocess.run(['debugfs', '-R', 'stat /system/var/overlay/lib/alsa/conf.d/99-sc5000-custom.conf', str(img)], capture_output=True, text=True).stdout
        self.assertIn('regular', hook_out)
        self.assertIn('0644', hook_out)

        suite_out = subprocess.run(['debugfs', '-R', 'stat /enginex/bundle/bin/sc5000-suite', str(img)], capture_output=True, text=True).stdout
        self.assertIn('0755', suite_out)
        # #103: the old path is a symlink, and the ALSA hook's old-path library resolves through it.
        for rel, kind in (('/sc5000-custom', 'symlink'), ('/sc5000-custom/bundle/lib/libengine-ui-hook.so', 'regular')):
            out = subprocess.run(['debugfs', '-R', f'stat {rel}', str(img)], capture_output=True, text=True).stdout
            self.assertIn(f'Type: {kind}', out, rel)

        # AZ04 (#76): the ALSA file goes where its var-lib.mount reads /var/lib from.
        for part in ('suite64', 'hook64'):
            (self.tmp / part).write_bytes(fake_elf(bdb.EM_AARCH64, part.encode()))
        r64 = self.build('out64', prefix='SYSTEMONE', suite=self.tmp / 'suite64', hook=self.tmp / 'hook64')
        img64 = self.tmp / 'userdata64.img'
        subprocess.run(['dd', 'if=/dev/zero', f'of={img64}', 'bs=1M', 'count=32'], check=True, capture_output=True)
        subprocess.run(['mkfs.ext4', '-F', str(img64)], check=True, capture_output=True)
        hmod.inject(img64, r64['bundle'])
        hook_out = subprocess.run(['debugfs', '-R', 'stat /system/var-lib/alsa/conf.d/99-sc5000-custom.conf', str(img64)], capture_output=True, text=True).stdout
        self.assertIn('regular', hook_out)

        # #116: --ssh-key lands as /enginex/authorized_keys, 0600; a missing or empty key injects nothing.
        self.assertNotIn('Type: regular', subprocess.run(['debugfs', '-R', 'stat /enginex/authorized_keys', str(img64)],
                                                          capture_output=True, text=True).stdout)
        key = self.tmp / 'id_ed25519.pub'
        key.write_text('ssh-ed25519 AAAAtest user@host\n')
        hmod.inject(img64, r64['bundle'], ssh_key=key)
        st = subprocess.run(['debugfs', '-R', 'stat /enginex/authorized_keys', str(img64)], capture_output=True, text=True).stdout
        self.assertIn('Mode:  0600', st)
        got = subprocess.run(['debugfs', '-R', 'cat /enginex/authorized_keys', str(img64)], capture_output=True, text=True).stdout
        self.assertEqual(got, key.read_text())
        (self.tmp / 'empty.pub').write_text('\n')
        for bad in (self.tmp / 'empty.pub', self.tmp / 'missing.pub'):
            with self.assertRaises(SystemExit):
                hmod.inject(img64, r64['bundle'], ssh_key=bad)

    def test_host_install_keeps_a_legacy_dir_for_the_first_boot_to_rename(self):
        for tool in ('debugfs', 'mkfs.ext4'):
            if subprocess.run(['which', tool], capture_output=True).returncode != 0:
                self.skipTest(f'{tool} not installed on this host')
        r = self.build()
        img = self.tmp / 'legacy.img'
        subprocess.run(['dd', 'if=/dev/zero', f'of={img}', 'bs=1M', 'count=32'], check=True, capture_output=True)
        subprocess.run(['mkfs.ext4', '-F', str(img)], check=True, capture_output=True)
        subprocess.run(['debugfs', '-w', '-R', 'mkdir /sc5000-custom', str(img)], check=True, capture_output=True)
        hspec = importlib.util.spec_from_file_location('host_install_userdata', HERE / 'host-install-userdata.py')
        hmod = importlib.util.module_from_spec(hspec)
        hspec.loader.exec_module(hmod)
        hmod.inject(img, r['bundle'])
        stat = lambda rel: subprocess.run(['debugfs', '-R', f'stat {rel}', str(img)], capture_output=True, text=True).stdout
        self.assertIn('Type: regular', stat('/sc5000-custom/version.txt'))
        self.assertNotIn('Type:', stat('/enginex'))

    def migrate(self, legacy_files, new_files=None):
        """Run the bundle's install.sh with /data pointed at a scratch dir."""
        src = self.extract(self.build())
        data = self.tmp / 'data'
        for name, files in (('sc5000-custom', legacy_files), ('enginex', new_files)):
            for rel, text in (files or {}).items():
                (data / name / rel).parent.mkdir(parents=True, exist_ok=True)
                (data / name / rel).write_text(text)
        script = src / 'install.sh'
        script.write_text(script.read_text().replace('/data', str(data)))
        sums = src / 'SHA256SUMS'  # install.sh is listed: re-seal it after the path swap
        sums.write_text(''.join(
            (hashlib.sha256(script.read_bytes()).hexdigest() + '  install.sh\n') if l.endswith(' install.sh') or l.endswith('  install.sh') else l + '\n'
            for l in sums.read_text().splitlines()))
        env = {'PATH': os.environ.get('PATH', '/usr/bin:/bin')}
        r = subprocess.run(['sh', str(script)], env=env, capture_output=True, text=True)
        self.assertEqual(r.returncode, 0, r.stderr)
        return data

    def test_install_renames_legacy_dir_and_leaves_a_symlink(self):
        data = self.migrate({'authorized_keys': 'key', 'Stream/song.flac': 'x', 'settings.conf': 's'})
        old, new = data / 'sc5000-custom', data / 'enginex'
        self.assertTrue(old.is_symlink() and os.readlink(old) == 'enginex')
        self.assertEqual((new / 'authorized_keys').read_text(), 'key')
        self.assertEqual((new / 'Stream/song.flac').read_text(), 'x')
        self.assertTrue((old / 'bundle/boot-activate.sh').exists())  # old path still reaches it
        self.assertEqual((old / 'version.txt').read_text(), '5.3.0.1.1\n')

    def test_install_merges_when_both_dirs_are_real_and_loses_nothing(self):
        data = self.migrate({'authorized_keys': 'old', 'a.txt': 'A', 'ui/x': 'old-ui'},
                            {'authorized_keys': 'new'})
        new = data / 'enginex'
        self.assertTrue((data / 'sc5000-custom').is_symlink())
        self.assertEqual((new / 'authorized_keys').read_text(), 'new')  # enginex wins
        self.assertEqual((new / 'a.txt').read_text(), 'A')
        leftovers = list(new.glob('legacy-sc5000-custom.*'))
        self.assertEqual(len(leftovers), 1)
        self.assertEqual((leftovers[0] / 'authorized_keys').read_text(), 'old')

    def test_fresh_install_creates_enginex_and_the_old_path(self):
        data = self.migrate({})
        self.assertTrue((data / 'enginex/bundle').is_dir())
        self.assertTrue((data / 'sc5000-custom/version.txt').exists())

    def test_boot_activate_perf_tune_runtime(self):
        src = self.extract(self.build())
        boot = (src / 'boot-activate.sh').read_text()
        start = boot.index("cat <<'EOF' > /run/sc5000/perf-tune.sh\n") + len("cat <<'EOF' > /run/sc5000/perf-tune.sh\n")
        end = boot.index("\nEOF\n", start)
        perf_script = self.tmp / 'perf-tune.sh'
        perf_script.write_text(boot[start:end] + '\n')
        perf_script.chmod(0o755)
        self.assertEqual(subprocess.run(['sh', '-n', str(src / 'boot-activate.sh')], capture_output=True).returncode, 0)
        self.assertEqual(subprocess.run(['sh', '-n', str(perf_script)], capture_output=True).returncode, 0)

        sys_root = self.tmp / 'sys'
        proc_root = self.tmp / 'proc'
        for cpu in range(4):
            d = sys_root / f'devices/system/cpu/cpu{cpu}/cpufreq'
            d.mkdir(parents=True)
            (d / 'scaling_governor').write_text('interactive\n')
            (d / 'scaling_min_freq').write_text('408000\n')
        ondemand = sys_root / 'devices/system/cpu/cpufreq/ondemand'
        ondemand.mkdir(parents=True)
        for k in ('up_threshold', 'sampling_down_factor', 'ignore_nice_load'):
            (ondemand / k).write_text('0\n')
        (proc_root / 'interrupts').parent.mkdir(parents=True, exist_ok=True)
        (proc_root / 'interrupts').write_text(
            '           CPU0       CPU1       CPU2       CPU3\n'
            ' 41:          0          0          0          1     GIC-0  41 Level     ffb20000.i2s\n'
            ' 42:          0          1          0          0     GIC-0  42 Level     ffa30000.gpu\n'
            ' 43:          0          1          0          0     GIC-0  43 Level     ehci_hcd:usb1\n'
            ' 44:          0          1          0          0     GIC-0  44 Level     ff540000.usb\n'
            ' 45:          0          1          0          0     GIC-0  45 Level     eth0\n'
        )
        for irq in (41, 42, 43, 44):
            d = proc_root / f'irq/{irq}'
            d.mkdir(parents=True)
            (d / 'smp_affinity_list').write_text('1\n')
        d45 = proc_root / 'irq/45'
        d45.mkdir(parents=True)
        (d45 / 'smp_affinity').write_text('2\n')
        for pid, comm in ((101, 'irq/41-ffb20000'), (102, 'irq/42-ffa30000'), (103, 'irq/45-eth0')):
            p = proc_root / str(pid)
            p.mkdir()
            (p / 'comm').write_text(f'{comm}\n')

        bindir = self.tmp / 'bin'
        bindir.mkdir()
        log = self.tmp / 'calls.log'
        for tool in ('taskset', 'chrt'):
            wrapper = bindir / tool
            wrapper.write_text(f'#!/bin/sh\necho "{tool} $*" >> "{log}"\n')
            wrapper.chmod(0o755)
        env = {
            'PATH': f'{bindir}:{os.environ.get("PATH", "/usr/bin:/bin")}',
            'SC5000_SYS_ROOT': str(sys_root),
            'SC5000_PROC_ROOT': str(proc_root),
        }
        r = subprocess.run(['sh', str(perf_script)], env=env, capture_output=True, text=True)
        self.assertEqual(r.returncode, 0, r.stderr)
        for cpu in range(4):
            d = sys_root / f'devices/system/cpu/cpu{cpu}/cpufreq'
            self.assertEqual((d / 'scaling_governor').read_text().strip(), 'ondemand')
            self.assertEqual((d / 'scaling_min_freq').read_text().strip(), '1200000')
        self.assertEqual((ondemand / 'up_threshold').read_text().strip(), '50')
        self.assertEqual((ondemand / 'sampling_down_factor').read_text().strip(), '10')
        self.assertEqual((ondemand / 'ignore_nice_load').read_text().strip(), '1')
        self.assertEqual((proc_root / 'irq/41/smp_affinity_list').read_text().strip(), '0')
        self.assertEqual((proc_root / 'irq/42/smp_affinity_list').read_text().strip(), '1')
        self.assertEqual((proc_root / 'irq/43/smp_affinity_list').read_text().strip(), '2')
        self.assertEqual((proc_root / 'irq/44/smp_affinity_list').read_text().strip(), '2')
        self.assertEqual((proc_root / 'irq/45/smp_affinity').read_text().strip(), '4')
        calls = log.read_text().splitlines()
        self.assertIn('taskset -pc 0 101', calls)
        self.assertIn('chrt -f -p 99 101', calls)
        self.assertIn('taskset -pc 1 102', calls)
        self.assertIn('taskset -pc 2 103', calls)
        self.assertFalse([c for c in calls if c.startswith('chrt ') and not c.endswith('101')])


if __name__ == '__main__':
    unittest.main()
