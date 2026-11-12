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
        self.host_commands = self.tmp / 'host-commands.log'
        self.host_commands.touch()
        shim_bin = self.tmp / 'safe-bin'
        shim_bin.mkdir()
        for name in ('systemctl', 'pkill', 'killall', 'reboot', 'mount', 'umount', 'ip'):
            shim = shim_bin / name
            shim.write_text('#!/bin/sh\nprintf "%s\\n" "$0 $*" >> "$HOST_COMMAND_LOG"\nexit 0\n')
            shim.chmod(0o755)
        self.env = {'PATH': f"{shim_bin}:{os.environ.get('PATH', '/usr/bin:/bin')}",
                    'HOST_COMMAND_LOG': str(self.host_commands)}
        self.suite = self.tmp / 'sc5000-suite'
        self.suite_bytes = fake_elf(bdb.EM_ARM, b'suite')
        self.suite.write_bytes(self.suite_bytes)
        self.hook = self.tmp / 'libengine-ui-hook.so'
        self.hook_bytes = fake_elf(bdb.EM_ARM, b'hook')
        self.hook.write_bytes(self.hook_bytes)
        self.ui = self.ui_tar({'packs/a/Main.qml': b'Item {}', 'icons/x.svg': b'<svg/>'})

    def tearDown(self):
        try:
            self.assertEqual(self.host_commands.read_text(), '', 'test called a host command')
        finally:
            shutil.rmtree(self.tmp)

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
        self.assertIn('Environment="QT_LOGGING_CONF=/data/logging.ini"',
                      (src / 'boot-activate.sh').read_text())
        self.assertIn('Environment=HOME=/tmp/sc5000-engine-home MESA_SHADER_CACHE_MAX_SIZE=32M',
                      (src / 'boot-activate.sh').read_text())
        self.assertIn('Environment=QML_DISABLE_DISK_CACHE=1',
                      (src / 'boot-activate.sh').read_text())
        self.assertIn('ExecStartPre=/bin/mkdir -p -m 0700 /tmp/sc5000-engine-home',
                      (src / 'boot-activate.sh').read_text())
        self.assertIn('EnvironmentFile=-/data/sc5000-custom/ui-scale.env',
                      (src / 'boot-activate.sh').read_text())
        self.assertIn('enginex-stream-sync.service',
                      (src / 'boot-activate.sh').read_text())
        self.assertIn('Wants=enginex-stream-sync.service',
                      (src / 'boot-activate.sh').read_text())
        overlay_ntp = (HERE.parents[1] / 'rootfs-overlay/etc/systemd/timesyncd.conf.d/10-enginex-ntp.conf').read_text()
        self.assertIn('/run/systemd/timesyncd.conf.d/10-enginex-ntp.conf', (src / 'boot-activate.sh').read_text())
        self.assertIn(overlay_ntp, (src / 'boot-activate.sh').read_text())
        self.assertIn('systemctl restart --no-block systemd-timesyncd.service', (src / 'boot-activate.sh').read_text())
        self.assertIn('/run/systemd/timesyncd.conf.d/10-enginex-ntp.conf', (src / 'uninstall.sh').read_text())
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

    def test_mixstream_gen2_stems_processor_delivery(self):
        # The read-only root's /usr/local/bin/stems-processor request is
        # redirected by the UI hook to bundle/bin/stems-processor.
        for prefix in ('MIXSTREAMPROPLUS', 'MIXSTREAMPROGO'):
            with self.subTest(prefix=prefix):
                result = self.build(out=prefix, prefix=prefix)
                top = f'{prefix}-5.3.0.1.1-EngineX-OS-Data-Bundle'
                with tarfile.open(result['bundle']) as bundle:
                    wrapper = bundle.getmember(f'{top}/bin/stems-processor')
                    self.assertEqual(wrapper.mode, 0o755)
                    self.assertEqual(bundle.extractfile(wrapper).read(),
                                     bdb.WRAPPER.format('stems-processor').encode())
                    self.assertEqual(bundle.extractfile(f'{top}/bin/sc5000-suite').read(), self.suite_bytes)

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
        env = {**self.env, 'SC5000_CUSTOM': str(dest), 'SC5000_ALSA_OVERLAY': str(alsa)}
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
        env = {**self.env, 'SC5000_CUSTOM': str(dest)}
        if alsa_dest:
            env['SC5000_ALSA_OVERLAY'] = str(alsa_dest)
        return subprocess.run(['sh', str(script)], env=env,
                              capture_output=True, text=True, check=check)

    def test_install_uninstall_does_not_call_host_commands(self):
        src = self.extract(self.build())
        dest = self.tmp / 'custom'
        alsa = self.tmp / 'alsa'
        self.sh(src / 'install.sh', dest, alsa_dest=alsa)
        self.sh(dest / 'bundle/uninstall.sh', dest, alsa_dest=alsa)
        self.assertFalse((dest / 'bundle').exists())
        self.assertEqual(self.host_commands.read_text(), '')

    def test_release_tag_is_baked_in_and_reaches_the_deck_by_both_install_paths(self):
        # Same-version re-release: version.txt stays, the tag says which release this build is.
        src = self.extract(self.build())
        self.assertEqual((src / 'release-tag').read_text(), 'v5.3.0.1.1\n')
        r = self.build(out='r2', release_tag='v5.3.0.1.1-r2')
        with tarfile.open(r['bundle']) as t:
            names = t.getnames()
            tag = t.extractfile(f'{TOP}/release-tag').read()
            sums = t.extractfile(f'{TOP}/SHA256SUMS').read().decode()
            install = t.extractfile(f'{TOP}/install.sh').read()
        self.assertEqual(tag, b'v5.3.0.1.1-r2\n')
        self.assertIn(f'{hashlib.sha256(tag).hexdigest()}  release-tag\n', sums)
        self.assertIn(f'{TOP}/version.txt', names)
        for bad in ('v5.3.0.2', '5.3.0.1.1-r2', 'v5.3.0.1.1-r0', 'v5.3.0.1.1-rc1', 'v5.3.0.1.1-r2 '):
            with self.subTest(bad=bad), self.assertRaises(SystemExit):
                self.build(out='bad', release_tag=bad)
        # install.sh (on-device, OTA bundle and bundle-carrying *-Update.img)
        shutil.rmtree(self.tmp / 'x')
        src = self.extract(r)
        self.assertEqual((src / 'install.sh').read_bytes(), install)
        dest = self.tmp / 'data/sc5000-custom'
        self.sh(src / 'install.sh', dest, alsa_dest=self.tmp / 'data/system/var/overlay/lib/alsa/conf.d')
        self.assertEqual((dest / 'bundle/release-tag').read_text(), 'v5.3.0.1.1-r2\n')
        self.assertEqual((dest / 'version.txt').read_text(), '5.3.0.1.1\n')
        # host-install-userdata.py (EngineX-Installer writing the data partition)
        for tool in ('debugfs', 'mkfs.ext4'):
            if subprocess.run(['which', tool], capture_output=True).returncode != 0:
                self.skipTest(f'{tool} not installed on this host')
        img = self.tmp / 'userdata.img'
        subprocess.run(['dd', 'if=/dev/zero', f'of={img}', 'bs=1M', 'count=32'], check=True, capture_output=True)
        subprocess.run(['mkfs.ext4', '-F', str(img)], check=True, capture_output=True)
        hspec = importlib.util.spec_from_file_location('host_install_userdata', HERE / 'host-install-userdata.py')
        hmod = importlib.util.module_from_spec(hspec)
        hspec.loader.exec_module(hmod)
        hmod.inject(img, r['bundle'])
        cat = subprocess.run(['debugfs', '-R', 'cat /enginex/bundle/release-tag', str(img)], capture_output=True, text=True).stdout
        self.assertEqual(cat, 'v5.3.0.1.1-r2\n')

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
        alsa = self.tmp / 'alsa'
        self.sh(src / 'install.sh', dest, alsa_dest=alsa)
        (dest / 'ui/.pack-id').write_text('image\n')  # the image's prepare unit took over
        self.sh(dest / 'bundle/uninstall.sh', dest, alsa_dest=alsa)
        self.assertTrue((dest / 'ui/packs/a/Main.qml').exists())
        self.assertFalse((dest / 'bundle').exists())

    def test_damaged_bundle_installs_nothing(self):
        src = self.extract(self.build())
        (src / 'bin/sc5000-suite').write_bytes(b'\x7fELF tampered')
        dest = self.tmp / 'custom'
        r = self.sh(src / 'install.sh', dest, alsa_dest=self.tmp / 'alsa', check=False)
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

        # #215: host-install-userdata.py also accepts *-Update.img directly, clears gen2-locked, and rejects corrupt payload.
        subprocess.run(['debugfs', '-w', '-R', 'write /etc/hosts /enginex/gen2-locked', str(img64)], check=True, capture_output=True)
        hmod.inject(img64, r64['image'])
        self.assertNotIn('Type: regular', subprocess.run(['debugfs', '-R', 'stat /enginex/gen2-locked', str(img64)],
                                                         capture_output=True, text=True).stdout)
        self.assertIn('Type: regular', subprocess.run(['debugfs', '-R', 'stat /enginex/gen2-unlocked', str(img64)],
                                                      capture_output=True, text=True).stdout)
        raw_img = Path(r64['image']).read_bytes()
        bad_img = self.tmp / 'corrupt-update.img'
        bad_img.write_bytes(raw_img[:-1] + bytes([raw_img[-1] ^ 1]))
        with self.assertRaises(SystemExit):
            hmod.inject(img64, bad_img)

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
        env = self.env
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

    def test_boot_activate_skips_missing_lib_uses_execstartpre_restore_tz_and_respects_newer_conf(self):
        r = self.build()
        src = self.extract(r)
        ba_text = (src / 'boot-activate.sh').read_text()

        # Never call 'systemctl stop engine.service' from boot-activate.sh (which runs in engine.service's cgroup);
        # restore Engine.conf via engine.service drop-in ExecStartPre=-.../boot-activate.sh --restore-tz + restart --no-block!
        self.assertNotIn('systemctl stop engine.service', ba_text)
        self.assertIn('ExecStartPre=-$bundle_dir/boot-activate.sh --restore-tz', ba_text)
        self.assertIn('systemctl restart --no-block engine.service', ba_text)

        root = self.tmp / 'gen2-boot'
        dest = root / 'enginex'
        (dest / 'bundle').mkdir(parents=True)
        shutil.copy2(src / 'boot-activate.sh', dest / 'bundle/boot-activate.sh')
        (dest / 'state').mkdir(parents=True)
        tz_state = dest / 'state/timezone'
        tz_state.write_text('America/New York\n')
        os.utime(tz_state, (1000, 1000))

        ts = self.tmp / 'timestamp'
        ts.write_text('20261030114129\n')
        build_epoch = 1793360489
        os.utime(ts, (build_epoch, build_epoch))

        zdir = self.tmp / 'zoneinfo'
        for rel in ('America/New_York', 'Europe/London', 'UTC'):
            (zdir / rel).parent.mkdir(parents=True, exist_ok=True)
            (zdir / rel).write_bytes(b'TZif2')

        pcode = self.tmp / 'product-code'
        pcode.write_text('JP21\n')

        run_dir = self.tmp / 'run'
        (run_dir / 'systemd/system').mkdir(parents=True)
        ba_relocated = self.tmp / 'boot-activate-reloc.sh'
        ba_relocated.write_text(ba_text.replace('/run/', str(run_dir) + '/'))
        ba_relocated.chmod(0o755)
        marker = run_dir / 'sc5000-tz-newer'
        lib_dir = root / 'system/var/overlay/lib'
        env = dict(self.env,
                   SC5000_CUSTOM=str(dest),
                   SC5000_TIMESTAMP_FILE=str(ts),
                   SC5000_TIMESYNC_LIB_DIR=str(lib_dir),
                   SC5000_CONFIG_DIR=str(root / '.config'),
                   SC5000_ZONEINFO_DIR=str(zdir),
                   SC5000_PRODUCT_CODE_FILE=str(pcode),
                   SC5000_TZ_NEWER_MARKER=str(marker))

        # 1. Missing lib_dir must NOT be created; ExecStartPre (--restore-tz) writes America/New_York.
        res = subprocess.run(['sh', str(ba_relocated)], env=env, capture_output=True)
        self.assertEqual(res.returncode, 0, res.stderr)
        cmds = self.host_commands.read_text()
        self.assertNotIn('stop engine.service', cmds)
        self.host_commands.write_text('')
        self.assertFalse(lib_dir.exists())

        res = subprocess.run(['sh', str(ba_relocated), '--restore-tz'], env=env, capture_output=True)
        self.assertEqual(res.returncode, 0, res.stderr)
        conf = root / '.config/jp21.user.settings/AIR Music Technology/Engine.conf'
        self.assertEqual(conf.read_text(), '[TimeDateSettings]\nTZEnvVar=America/New_York\n')

        # 2. Existing lib_dir gets clock seeded to build_epoch; newer Engine.conf (Europe/London) is preserved.
        lib_dir.mkdir(parents=True)
        conf.write_text('[TimeDateSettings]\nTZEnvVar=Europe/London\n')
        os.utime(conf, (2000, 2000))
        res = subprocess.run(['sh', str(ba_relocated)], env=env, capture_output=True)
        self.assertEqual(res.returncode, 0, res.stderr)
        self.host_commands.write_text('')
        res = subprocess.run(['sh', str(ba_relocated), '--restore-tz'], env=env, capture_output=True)
        self.assertEqual(res.returncode, 0, res.stderr)
        clock = lib_dir / 'systemd/timesync/clock'
        self.assertEqual(int(clock.stat().st_mtime), build_epoch)
        self.assertEqual(conf.read_text(), '[TimeDateSettings]\nTZEnvVar=Europe/London\n')

        # 3. Newer state/timezone (user chose UTC) sets marker before stop; even if old Engine destructor
        #    bumps Engine.conf mtime during stop, --restore-tz consumes marker and writes UTC!
        tz_state.write_text('UTC\n')
        os.utime(tz_state, (3000, 3000))
        res = subprocess.run(['sh', str(ba_relocated)], env=env, capture_output=True)
        self.assertEqual(res.returncode, 0, res.stderr)
        self.host_commands.write_text('')
        # Simulate unhooked Engine destructor writing stale Europe/London at t=4000 during restart teardown
        conf.write_text('[TimeDateSettings]\nTZEnvVar=Europe/London\n')
        os.utime(conf, (4000, 4000))
        res = subprocess.run(['sh', str(ba_relocated), '--restore-tz'], env=env, capture_output=True)
        self.assertEqual(res.returncode, 0, res.stderr)
        self.assertEqual(conf.read_text(), '[TimeDateSettings]\nTZEnvVar=UTC\n')
        self.assertFalse(marker.exists())

    def test_boot_activate_wraps_engine_and_updaters_and_marks_gen2_unlocked(self):
        r = self.build()
        src = self.extract(r)
        ba_text = (src / 'boot-activate.sh').read_text()

        root = self.tmp / 'gen2-preload'
        dest = root / 'enginex'
        (dest / 'bundle/bin').mkdir(parents=True)
        shutil.copy2(src / 'bin/engine-ui-guard', dest / 'bundle/bin/engine-ui-guard')
        run_dir = self.tmp / 'run-preload'
        (run_dir / 'systemd/system').mkdir(parents=True)
        engine_script = self.tmp / 'vendor-engine-script'
        stock_script = (
            '#!/bin/sh\n'
            'while [ 1 ]\n'
            'do\n'
            '\t/usr/Engine/Engine -d0 -loggerOptions "Type, Message, Thread, Category, SplitLongLines"\n'
            '\t\t/usr/sbin/az01-update --file "$UPDATE_FILE"\n'
            '\t\t/usr/bin/planck-remote-screen --update "$UPDATE_FILE"\n'
            '\t\taz01-update --erase-data\n'
            'done\n'
        )
        engine_script.write_text(stock_script)
        engine_script.chmod(0o755)

        ba_reloc = self.tmp / 'boot-activate-preload.sh'
        ba_reloc.write_text(
            ba_text.replace('/run/', str(run_dir) + '/')
                   .replace('/usr/Engine/Scripts/engine', str(engine_script))
        )
        ba_reloc.chmod(0o755)
        env = dict(self.env,
                   SC5000_CUSTOM=str(dest),
                   SC5000_TIMESTAMP_FILE=str(self.tmp / 'no-ts'),
                   SC5000_CONFIG_DIR=str(root / '.config'))

        # 1. Explicit gen2-locked must prevent boot-activate.sh from creating gen2-unlocked.
        (dest / 'gen2-locked').write_text('1\n')
        res = subprocess.run(['sh', str(ba_reloc)], env=env, capture_output=True, text=True)
        self.assertEqual(res.returncode, 0, res.stderr)
        self.assertFalse((dest / 'gen2-unlocked').exists())
        self.host_commands.write_text('')

        stage = run_dir / 'sc5000-engine-script'
        rewritten = stage.read_text()
        lib_hook = f'{dest}/bundle/lib/libengine-ui-hook.so'
        self.assertIn(f'\t{dest}/bundle/bin/engine-ui-guard launch /usr/Engine/Engine -d0', rewritten)
        self.assertIn(f'\t\tenv LD_PRELOAD="{lib_hook}" /usr/sbin/az01-update --file "$UPDATE_FILE"\n', rewritten)
        self.assertIn(f'\t\tenv LD_PRELOAD="{lib_hook}" /usr/bin/planck-remote-screen --update "$UPDATE_FILE"\n', rewritten)
        self.assertIn(f'\t\tenv LD_PRELOAD="{lib_hook}" /usr/sbin/az01-update --erase-data\n', rewritten)

        # 2. Removing gen2-locked lets boot-activate.sh mark gen2-unlocked; re-running on bound script is idempotent.
        (dest / 'gen2-locked').unlink()
        engine_script.write_text(rewritten)
        res = subprocess.run(['sh', str(ba_reloc)], env=env, capture_output=True, text=True)
        self.assertEqual(res.returncode, 0, res.stderr)
        self.assertEqual((dest / 'gen2-unlocked').read_text(), '1\n')
        self.assertEqual(engine_script.read_text(), rewritten)
        self.assertNotIn('mount --bind', self.host_commands.read_text())
        self.host_commands.write_text('')

        # 3. Live upgrade from an earlier 5.3.0.4 script (Engine wrapped, updaters still unwrapped) wraps updaters cleanly.
        legacy_5304 = stock_script.replace(
            '/usr/Engine/Engine ',
            f'{dest}/bundle/bin/engine-ui-guard launch /usr/Engine/Engine ',
        )
        engine_script.write_text(legacy_5304)
        res = subprocess.run(['sh', str(ba_reloc)], env=env, capture_output=True, text=True)
        self.assertEqual(res.returncode, 0, res.stderr)
        self.assertEqual(stage.read_text(), rewritten)
        self.host_commands.write_text('')

        # 4. Leading 'exec ' before updaters is preserved; missing /usr/sbin/az01-update emits a diagnostic warning.
        exec_script = (
            '#!/bin/sh\n'
            '\t/usr/Engine/Engine -d0\n'
            '\t\texec /usr/sbin/az01-update --file "$UPDATE_FILE"\n'
            '\t\texec /usr/bin/planck-remote-screen --update "$UPDATE_FILE"\n'
        )
        engine_script.write_text(exec_script)
        res = subprocess.run(['sh', str(ba_reloc)], env=env, capture_output=True, text=True)
        self.assertEqual(res.returncode, 0, res.stderr)
        self.assertIn(f'\t\texec env LD_PRELOAD="{lib_hook}" /usr/sbin/az01-update --file "$UPDATE_FILE"\n', stage.read_text())
        self.assertIn(f'\t\texec env LD_PRELOAD="{lib_hook}" /usr/bin/planck-remote-screen --update "$UPDATE_FILE"\n', stage.read_text())
        self.host_commands.write_text('')

        logger_shim = self.tmp / 'logger-bin'
        logger_shim.mkdir()
        (logger_shim / 'logger').write_text('#!/bin/sh\nexit 0\n')
        (logger_shim / 'logger').chmod(0o755)
        engine_script.write_text('#!/bin/sh\n\t/usr/Engine/Engine -d0\n')
        res = subprocess.run(['sh', str(ba_reloc)],
                             env={**env, 'PATH': f"{logger_shim}:{env['PATH']}"},
                             capture_output=True, text=True)
        self.assertEqual(res.returncode, 0, res.stderr)
        self.assertIn('/usr/sbin/az01-update preload hook missing', res.stderr)
        self.host_commands.write_text('')

    def test_release_scripts_emit_28_artifacts_and_sha256sums(self):
        rel_sh = (HERE / 'build-release.sh').read_text()
        self.assertIn('-Data-Bundle.tar.gz"', rel_sh)
        self.assertIn('sha256sum *-Update.img *-Data-Bundle.tar.gz > SHA256SUMS.txt', rel_sh)

        out_dir = self.tmp / 'release-out'
        out_dir.mkdir()
        gen1 = (
            ('SC5000', 'Denon-SC5000-Prime-JP07'),
            ('SC5000M', 'Denon-SC5000M-Prime-JP08'),
            ('PRIMEGO', 'Denon-PRIME-GO-JP11'),
            ('SC6000', 'Denon-SC6000-Prime-JP13'),
            ('SC6000M', 'Denon-SC6000M-Prime-JP14'),
            ('PRIME4', 'Denon-PRIME-4-JC11'),
            ('PRIME2', 'Denon-PRIME-2-JC16'),
            ('MIXSTREAMPRO', 'Numark-Mixstream-Pro-NH08'),
        )
        for pfx, model in gen1:
            (out_dir / f'{pfx}-5.3.0.4-EngineX-OS-{model}-Update.img').write_bytes(f'gen1-{pfx}'.encode())
        for pfx, model in bdb.MODEL_NAMES.items():
            (out_dir / f'{pfx}-5.3.0.4-EngineX-OS-{model}-Update.img').write_bytes(f'gen2-img-{pfx}'.encode())
            (out_dir / f'{pfx}-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz').write_bytes(f'gen2-bundle-{pfx}'.encode())

        fake_bin = self.tmp / 'fake-go-bin'
        fake_bin.mkdir()
        fake_go = fake_bin / 'go'
        fake_go.write_text(
            '#!/bin/sh\nout=""\nwhile [ $# -gt 0 ]; do\n'
            '  if [ "$1" = "-o" ]; then out="$2"; shift 2; else shift; fi\n'
            'done\nprintf "#!/bin/sh\\nexit 0\\n" > "$out"\nchmod 755 "$out"\n'
        )
        fake_go.chmod(0o755)
        env = {**self.env, 'PATH': f"{fake_bin}:{self.env['PATH']}"}
        res = subprocess.run(['sh', str(HERE / 'build-installer.sh'), '5.3.0.4', str(out_dir)],
                             env=env, capture_output=True, text=True)
        self.assertEqual(res.returncode, 0, res.stderr)
        self.assertEqual(len(list(out_dir.glob('*-Update.img'))), 16)
        self.assertEqual(len(list(out_dir.glob('*-Data-Bundle.tar.gz'))), 8)
        self.assertEqual(len(list(out_dir.glob('EngineX-Installer-5.3.0.4-*'))), 4)
        lines = (out_dir / 'SHA256SUMS.txt').read_text().splitlines()
        self.assertEqual(len(lines), 28, lines)
        for line in lines:
            digest, fname = line.split('  ')
            self.assertEqual(hashlib.sha256((out_dir / fname).read_bytes()).hexdigest(), digest, fname)

if __name__ == '__main__':
    unittest.main()
