import hashlib
import importlib.util
import io
import json
import os
import shutil
import subprocess
import tarfile
import tempfile
import unittest
import zipfile
from unittest import mock
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
        self.suite = self.tmp / 'enginex-suite'
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
        self.assertEqual((src / 'bin/enginex-suite').read_bytes(), self.suite_bytes)
        self.assertEqual((src / 'lib/libengine-ui-hook.so').read_bytes(), self.hook_bytes)
        for applet in bdb.APPLETS:
            self.assertIn(f'enginex-suite" {applet} ', (src / 'bin' / applet).read_text())
            self.assertTrue((src / 'bin' / applet).stat().st_mode & 0o100)

        self.assertTrue((src / 'boot-activate.sh').exists())
        self.assertTrue((src / 'boot-activate.sh').stat().st_mode & 0o100)
        self.assertIn('Environment="QT_LOGGING_CONF=/data/logging.ini"',
                      (src / 'boot-activate.sh').read_text())
        self.assertIn('Environment=HOME=/tmp/enginex-engine-home MESA_SHADER_CACHE_MAX_SIZE=32M',
                      (src / 'boot-activate.sh').read_text())
        self.assertIn('Environment=QML_DISABLE_DISK_CACHE=1',
                      (src / 'boot-activate.sh').read_text())
        self.assertIn('ExecStartPre=/bin/mkdir -p -m 0700 /tmp/enginex-engine-home',
                      (src / 'boot-activate.sh').read_text())
        self.assertIn('EnvironmentFile=-/data/enginex/ui-scale.env',
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
                    self.assertEqual(bundle.extractfile(f'{top}/bin/enginex-suite').read(), self.suite_bytes)

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
        env = {**self.env, 'ENGINEX_CUSTOM': str(dest), 'ENGINEX_ALSA_OVERLAY': str(alsa)}
        # install.sh <img>, run from a copy of the bundle's own install.sh
        src = self.extract(r)
        run = lambda p: subprocess.run(['sh', str(src / 'install.sh'), str(p)], env=env, capture_output=True, text=True)
        out = run(img)
        self.assertEqual(out.returncode, 0, out.stderr)
        self.assertEqual((dest / 'gen2-unlocked').read_text(), '1\n')
        self.assertEqual((dest / 'version.txt').read_text(), '5.3.0.1.1\n')
        self.assertEqual([p.name for p in dest.glob('.img-install.*')], [])  # scratch dir removed
        # BH2-A7-8: what an install that lost power left behind goes with the next install;
        # a link of that name is not followed.
        stale = dest / '.img-install.AbCdEf'
        (stale / 'SCLIVE4-old/bin').mkdir(parents=True)
        (stale / 'SCLIVE4-old/bin/enginex-suite').write_bytes(b'x' * 4096)
        keep = self.tmp / 'not-ours'
        keep.mkdir()
        (keep / 'file').write_text('stays')
        (dest / '.img-install.link').symlink_to(keep)
        out = run(img)
        self.assertEqual(out.returncode, 0, out.stderr)
        self.assertEqual(sorted(p.name for p in dest.glob('.img-install.*')), ['.img-install.link'])
        self.assertEqual((keep / 'file').read_text(), 'stays')
        (dest / '.img-install.link').unlink()
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
        self.assertIn('edisksd.service.d/99-enginex.conf', boot)
        self.assertIn('systemctl restart edisksd.service', boot)  # blocking, only when the drop-in changed
        self.assertIn('cmp -s', boot)
        self.assertIn('--mount-path "$m"', boot)
        self.assertIn('edisksd.service.d/99-enginex.conf', (src / 'uninstall.sh').read_text())
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
        env = {**self.env, 'ENGINEX_CUSTOM': str(dest)}
        if alsa_dest:
            env['ENGINEX_ALSA_OVERLAY'] = str(alsa_dest)
        return subprocess.run(['sh', str(script)], env=env,
                              capture_output=True, text=True, check=check)

    def test_234_scripts_take_the_env_names_of_older_updaters(self):
        """#234: the updater, installer and USB hook of a release before 5.3.0.5 run this bundle's
        scripts with SC5000_CUSTOM / SC5000_ALSA_OVERLAY only; the new names win when both are set."""
        src = self.extract(self.build())
        dest, alsa = self.tmp / 'old-env/enginex', self.tmp / 'old-env-alsa'
        old = {**self.env, 'SC5000_CUSTOM': str(dest), 'SC5000_ALSA_OVERLAY': str(alsa)}
        res = subprocess.run(['sh', str(src / 'install.sh')], env=old, capture_output=True, text=True)
        self.assertEqual(res.returncode, 0, res.stderr)
        self.assertEqual((dest / 'version.txt').read_text(), '5.3.0.1.1\n')
        self.assertTrue((alsa / '99-sc5000-custom.conf').is_file())
        self.assertFalse((self.tmp / 'data').exists())  # never the default /data of a deck
        res = subprocess.run(['sh', str(dest / 'bundle/uninstall.sh')], env=old, capture_output=True, text=True)
        self.assertEqual(res.returncode, 0, res.stderr)
        self.assertFalse((alsa / '99-sc5000-custom.conf').exists())
        both = {**old, 'ENGINEX_CUSTOM': str(self.tmp / 'new-env'), 'ENGINEX_ALSA_OVERLAY': str(self.tmp / 'new-env-alsa')}
        res = subprocess.run(['sh', str(src / 'install.sh')], env=both, capture_output=True, text=True)
        self.assertEqual(res.returncode, 0, res.stderr)
        self.assertTrue((self.tmp / 'new-env/version.txt').is_file() and (self.tmp / 'new-env-alsa/99-sc5000-custom.conf').is_file())
        self.assertFalse((dest / 'bundle').exists())

    def test_install_refuses_a_bundle_for_another_cpu_or_model(self):
        """BH-G2-1: install.sh checked only SHA256SUMS, so an intact SYSTEM ONE (arm64) bundle
        installed on an SC LIVE 4 (armhf) and replaced working binaries with ones it cannot run."""
        compat = self.tmp / 'compatible'
        compat.write_bytes(b'inmusic,jp21\0rockchip,rk3288\0')
        engine = self.tmp / 'Engine'
        engine.write_bytes(fake_elf(bdb.EM_ARM, b'engine'))
        aarch64 = self.tmp / 'suite64'
        aarch64.write_bytes(fake_elf(bdb.EM_AARCH64, b'suite'))
        hook64 = self.tmp / 'hook64.so'
        hook64.write_bytes(fake_elf(bdb.EM_AARCH64, b'hook'))
        # The deck's BusyBox od knows -b but not GNU's -A/-t: a GNU-only call refused every bundle.
        shims = self.tmp / 'busybox-shims'
        shims.mkdir()
        od = shims / 'od'
        od.write_text('#!/bin/sh\nfor a; do case $a in -b) ;; -*) echo "od: invalid option $a" >&2; exit 1 ;; esac; done\n'
                      f'exec {shutil.which("od")} "$@"\n')
        od.chmod(0o755)
        deck = {'ENGINEX_COMPATIBLE': str(compat), 'ENGINEX_SYSTEM_ELF': str(engine),
                'PATH': f"{shims}:{self.env.get('PATH', os.environ['PATH'])}"}

        def install(result, name):
            shutil.rmtree(self.tmp / 'x', ignore_errors=True)
            self.extract(result)
            src = next((self.tmp / 'x').iterdir())  # <PREFIX>-<VERSION>-EngineX-OS-Data-Bundle
            env = {**self.env, **deck, 'ENGINEX_CUSTOM': str(self.tmp / name), 'ENGINEX_ALSA_OVERLAY': str(self.tmp / (name + '-alsa'))}
            return subprocess.run(['sh', str(src / 'install.sh')], env=env, capture_output=True, text=True)

        res = install(self.build(out='sys1', prefix='SYSTEMONE', suite=aarch64, hook=hook64), 'd1')
        self.assertNotEqual(res.returncode, 0)
        self.assertIn('another CPU', res.stderr)
        self.assertFalse((self.tmp / 'd1/bundle').exists())
        res = install(self.build(out='live2', prefix='SCLIVE2'), 'd2')
        self.assertNotEqual(res.returncode, 0)
        self.assertIn('bundle is for SCLIVE2', res.stderr)
        self.assertFalse((self.tmp / 'd2/bundle').exists())
        res = install(self.build(out='live4', prefix='SCLIVE4'), 'd3')
        self.assertEqual(res.returncode, 0, res.stderr)
        self.assertEqual((self.tmp / 'd3/bundle/model').read_text(), 'SCLIVE4\n')

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
        self.assertEqual((dest / 'bundle/bin/enginex-suite').read_bytes(), self.suite_bytes)
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

    def test_uninstall_removes_the_stand_in_tidal_account_and_both_alsa_files(self):
        # BH2-A7-11: boot-activate.sh's stand-in TIDAL account stayed in /data/StreamingAccounts.json
        # (stock Engine then shows TIDAL signed in and every request fails), and so did the ALSA
        # file in the directory this model does not use (the installer's image carries both).
        standin = ('"TIDAL": "{\\"provider\\":\\"TIDAL\\",\\"providerType\\":\\"TIDAL\\",'
                   '\\"accessToken\\":\\"enginex\\",\\"refreshToken\\":\\"enginex\\"}"')
        other = '"Beatport": "{\\"provider\\":\\"Beatport\\",\\"accessToken\\":\\"real-b\\"}"'
        third = '"SoundCloud": "{\\"provider\\":\\"SoundCloud\\",\\"accessToken\\":\\"real-s\\"}"'
        real = standin.replace('enginex', 'a-real-token')
        cases = {
            'only': ('{\n    %s\n}\n' % standin, {}),
            'first': ('{\n    %s,\n    %s\n}\n' % (standin, other), {'Beatport'}),
            'last': ('{\n    %s,\n    %s\n}\n' % (other, standin), {'Beatport'}),
            'middle': ('{\n    %s,\n    %s,\n    %s\n}\n' % (other, standin, third), {'Beatport', 'SoundCloud'}),
            # what boot-activate.sh makes of a one-line file
            'one-line': ('{\n    %s,%s}\n' % (standin, other), {'Beatport'}),
            'compact': ('{%s,%s}' % (other, standin.replace('": "', '":"')), {'Beatport'}),
            'real-login': ('{\n    %s,\n    %s\n}\n' % (real, other), {'TIDAL', 'Beatport'}),
            'no-tidal': ('{\n    %s\n}\n' % other, {'Beatport'}),
        }
        import json
        src = self.extract(self.build())
        for name, (text, want) in cases.items():
            with self.subTest(accounts=name):
                root = self.tmp / ('data-' + name)
                dest = root / 'enginex'
                used = root / 'system/var/overlay/lib/alsa/conf.d'
                unused = root / 'system/var-lib/alsa/conf.d'
                self.sh(src / 'install.sh', dest, alsa_dest=used)
                unused.mkdir(parents=True)
                shutil.copy(used / '99-sc5000-custom.conf', unused / '99-sc5000-custom.conf')
                accounts = root / 'StreamingAccounts.json'
                accounts.write_text(text)
                env = {**self.env, 'ENGINEX_CUSTOM': str(dest), 'ENGINEX_ALSA_OVERLAY': str(used),
                       'ENGINEX_STREAMING_ACCOUNTS': str(accounts)}
                subprocess.run(['sh', str(dest / 'bundle/uninstall.sh')], env=env, capture_output=True, text=True, check=True)
                left = json.loads(accounts.read_text())
                self.assertEqual(set(left), set(want), accounts.read_text())
                if name == 'real-login':
                    self.assertIn('a-real-token', left['TIDAL'])
                if name in ('real-login', 'no-tidal'):
                    self.assertEqual(accounts.read_text(), text)
                self.assertFalse((used / '99-sc5000-custom.conf').exists())
                self.assertFalse((unused / '99-sc5000-custom.conf').exists(), 'the ALSA file of the unused directory stayed')
                self.assertFalse((root / 'StreamingAccounts.json.tmp').exists())
        self.assertEqual(self.host_commands.read_text(), '')

    def test_damaged_bundle_installs_nothing(self):
        src = self.extract(self.build())
        (src / 'bin/enginex-suite').write_bytes(b'\x7fELF tampered')
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

        suite_out = subprocess.run(['debugfs', '-R', 'stat /enginex/bundle/bin/enginex-suite', str(img)], capture_output=True, text=True).stdout
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

    def test_host_install_fails_when_a_write_did_not_land(self):
        """BH-G2-3: debugfs exits 0 when a write fails and the readback only looked at three files,
        so an image without (or with a cut) UI hook was reported ok and flashed."""
        for tool in ('debugfs', 'mkfs.ext4'):
            if subprocess.run(['which', tool], capture_output=True).returncode != 0:
                self.skipTest(f'{tool} not installed on this host')
        r = self.build()
        hspec = importlib.util.spec_from_file_location('host_install_userdata', HERE / 'host-install-userdata.py')
        hmod = importlib.util.module_from_spec(hspec)
        hspec.loader.exec_module(hmod)
        short = self.tmp / 'short.so'
        short.write_bytes(b'\x7fELF')
        shims = self.tmp / 'debugfs-shim'
        shims.mkdir()
        # Only the batch write is altered: the UI hook's write reads another source, as when the
        # image fills up or a path is wrong; every other command runs on the real debugfs.
        (shims / 'debugfs').write_text(r'''#!/bin/sh
if [ "$1" = -w ] && [ "$2" = -f ]; then
    sed -i "s|^write \"[^\"]*/libengine-ui-hook.so\"|write \"$HOOK_SRC\"|" "$3"
fi
exec REAL "$@"
'''.replace('REAL', shutil.which('debugfs')))
        (shims / 'debugfs').chmod(0o755)
        for name, src in (('missing', '/nonexistent/libengine-ui-hook.so'), ('truncated', str(short))):
            with self.subTest(name):
                img = self.tmp / f'{name}.img'
                subprocess.run(['dd', 'if=/dev/zero', f'of={img}', 'bs=1M', 'count=32'], check=True, capture_output=True)
                subprocess.run(['mkfs.ext4', '-F', str(img)], check=True, capture_output=True)
                env = {'PATH': f"{shims}:{os.environ['PATH']}", 'HOOK_SRC': src}
                with mock.patch.dict(os.environ, env):
                    with self.assertRaises(SystemExit) as cm:
                        hmod.inject(img, r['bundle'])
                self.assertIn('libengine-ui-hook.so', str(cm.exception))
        img = self.tmp / 'good.img'  # unaltered: every file and the compat link read back
        subprocess.run(['dd', 'if=/dev/zero', f'of={img}', 'bs=1M', 'count=32'], check=True, capture_output=True)
        subprocess.run(['mkfs.ext4', '-F', str(img)], check=True, capture_output=True)
        self.assertEqual(hmod.inject(img, r['bundle'])['status'], 'ok')

    def install_ab(self, mv_on_packs):
        """Install version A, then version B with an mv that does `mv_on_packs` (shell) when B's staged
        ui/packs is moved into place, i.e. after B's bundle is already active (BH-G2-4)."""
        dest, alsa = self.tmp / 'gen', self.tmp / 'gen-alsa'

        def unpack(result):  # extract() names the default version's directory
            shutil.rmtree(self.tmp / 'x', ignore_errors=True)
            self.extract(result)
            return next((self.tmp / 'x').iterdir())
        self.sh(unpack(self.build('a')) / 'install.sh', dest, alsa_dest=alsa)
        b = self.build('b', version='5.3.0.2', ui_packs=self.ui_tar({'packs/a/Main.qml': b'Item { B }'}, 'b.tar.gz'))
        shims = self.tmp / 'mv-shim'
        shims.mkdir(exist_ok=True)
        (shims / 'mv').write_text(
            '#!/bin/sh\n'
            f'case "$1" in */.bundle.*/ui/packs) {mv_on_packs} ;; esac\n'
            f'exec {shutil.which("mv")} "$@"\n')
        (shims / 'mv').chmod(0o755)
        env = {**self.env, 'ENGINEX_CUSTOM': str(dest), 'ENGINEX_ALSA_OVERLAY': str(alsa),
               'PATH': f"{shims}:{self.env['PATH']}"}
        src = unpack(b)
        self.assertIn('5.3.0.2', src.name)
        res = subprocess.run(['sh', str(src / 'install.sh')], env=env, capture_output=True, text=True)
        return dest, alsa, res

    def assert_generation_a(self, dest, alsa):
        self.assertEqual((dest / 'bundle/version.txt').read_text(), '5.3.0.1.1\n')
        self.assertEqual((dest / 'ui/packs/a/Main.qml').read_bytes(), b'Item {}')
        self.assertEqual((dest / 'ui/icons/x.svg').read_bytes(), b'<svg/>')
        self.assertEqual((dest / 'ui/.pack-version').read_text(), '5.3.0.1.1\n')
        self.assertEqual((dest / 'version.txt').read_text(), '5.3.0.1.1\n')
        self.assertEqual((dest / 'gen2-unlocked').read_text(), '1\n')
        self.assertTrue((alsa / '99-sc5000-custom.conf').is_file())
        left = sorted(str(p.relative_to(dest)) for p in dest.rglob('*')
                      if p.name.startswith(('.install-', '.bundle.')) or p.name.endswith(('.replaced', '.undo')))
        self.assertEqual(left, [])

    def test_install_failure_keeps_the_previous_generation(self):
        """BH-G2-4: install.sh deleted the old bundle before the UI packs were in place, so a failure
        after that left B's bundle with no packs (A's parked in packs.replaced) and A's markers."""
        dest, alsa, res = self.install_ab('echo "mv: cannot move" >&2; exit 1')
        self.assertNotEqual(res.returncode, 0)
        self.assert_generation_a(dest, alsa)

    def test_boot_undoes_an_install_cut_off_half_way(self):
        """BH-G2-4: a power cut between swapping the bundle and the UI packs; the next boot puts the
        previous generation back and runs its activator instead of starting a half-installed B."""
        dest, alsa, res = self.install_ab('kill -9 $PPID; exit 1')
        self.assertNotEqual(res.returncode, 0)
        self.assertEqual((dest / 'bundle/version.txt').read_text(), '5.3.0.2\n')  # cut off half-way
        self.assertFalse((dest / 'ui/packs').exists())
        # The generation that comes back records that its own activator ran; the /run paths of the
        # activator that finds the half-done install stay in this test's scratch dir.
        mark = self.tmp / 'activated'
        for a in (dest / '.install-old/bundle/boot-activate.sh',):
            if a.exists():
                a.write_text(f'#!/bin/sh\necho "$0" > {mark}\n')
        run_dir = self.tmp / 'run'
        (run_dir / 'systemd/system').mkdir(parents=True)
        b_activate = dest / 'bundle/boot-activate.sh'
        b_activate.write_text(b_activate.read_text().replace('/run/', f'{run_dir}/'))
        env = {**self.env, 'ENGINEX_CUSTOM': str(dest), 'ENGINEX_ALSA_OVERLAY': str(alsa),
               'ENGINEX_PID1_COMM': str(self.pid1_systemd()),
               'ENGINEX_STREAMING_ACCOUNTS': str(self.tmp / 'StreamingAccounts.json'),
               'ENGINEX_PRODUCT_CODE_FILE': str(self.tmp / 'product-code'),
               'ENGINEX_TIMESYNC_LIB_DIR': str(self.tmp / 'no-lib')}
        res = subprocess.run(['sh', str(b_activate)], env=env, capture_output=True, text=True)
        self.assertEqual(res.returncode, 0, res.stderr)
        self.assert_generation_a(dest, alsa)
        self.assertEqual(mark.read_text().strip(), str(dest / 'bundle/boot-activate.sh'))

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
        marker = run_dir / 'enginex-tz-newer'
        lib_dir = root / 'system/var/overlay/lib'
        env = dict(self.env,
                   ENGINEX_CUSTOM=str(dest),
                   ENGINEX_TIMESTAMP_FILE=str(ts),
                   ENGINEX_TIMESYNC_LIB_DIR=str(lib_dir),
                   ENGINEX_CONFIG_DIR=str(root / '.config'),
                   ENGINEX_ZONEINFO_DIR=str(zdir),
                   ENGINEX_PRODUCT_CODE_FILE=str(pcode),
                   ENGINEX_TZ_NEWER_MARKER=str(marker),
                   # The host's PID 1 and /data stay out of it (a sandbox has neither).
                   ENGINEX_PID1_COMM=str(self.pid1_systemd()),
                   ENGINEX_STREAMING_ACCOUNTS=str(self.tmp / 'StreamingAccounts.json'))

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

    def test_234_boot_activate_over_an_older_releases_run(self):
        """#234: an update in place from a release before 5.3.0.5 (its updater ends its daemons by name
        and runs this boot-activate.sh; its installer runs it over SSH) leaves that release's units,
        drop-ins and runtime files in /run. Activation takes them out: every daemon has one unit, under
        its new name, the old deemixd unit is stopped and forgotten, and nothing outside /run moves."""
        src = self.extract(self.build())
        root = self.tmp / 'inplace'
        dest = root / 'enginex'
        res = subprocess.run(['sh', str(src / 'install.sh')], env={**self.env, 'ENGINEX_CUSTOM': str(dest),
                             'ENGINEX_ALSA_OVERLAY': str(root / 'alsa')}, capture_output=True, text=True)
        self.assertEqual(res.returncode, 0, res.stderr)
        run_dir = self.tmp / 'run-inplace'
        units = run_dir / 'systemd/system'
        r4 = {  # what r4's boot-activate.sh left loaded
            'systemd/system/enginex-sc5000-deemixd.service': '[Service]\nExecStart=/data/enginex/bundle/bin/sc5000-deemixd\n',
            'systemd/system/enginex-stream-sync.service': '[Service]\nExecStart=old\n',
            'systemd/system/engine.service.d/99-sc5000.conf': '[Service]\nEnvironment=SC5000_UI_HOOK=x\n',
            'systemd/system/edisksd.service.d/99-sc5000.conf': '[Service]\nEnvironment=LD_PRELOAD=x\n',
            'systemd/system/foreign.service': 'not ours\n',
            'sc5000-engine-script': '#!/bin/sh\n', 'sc5000-assignments/role': '13\n', 'sc5000/version.txt': '5.3.0.4\n',
            'sc5000-boot-converted': '', 'sc5000-tz-newer': '',
        }
        for name, text in r4.items():
            (run_dir / name).parent.mkdir(parents=True, exist_ok=True)
            (run_dir / name).write_text(text)
        ba = self.tmp / 'boot-activate-inplace.sh'
        ba.write_text((src / 'boot-activate.sh').read_text().replace('/run/', str(run_dir) + '/')
                      .replace('/usr/Engine/Scripts/engine', str(self.tmp / 'no-engine-script')))
        env = dict(self.env, ENGINEX_CUSTOM=str(dest), ENGINEX_TIMESTAMP_FILE=str(self.tmp / 'no-ts'),
                   ENGINEX_CONFIG_DIR=str(root / '.config'), ENGINEX_PID1_COMM=str(self.pid1_systemd()),
                   ENGINEX_STREAMING_ACCOUNTS=str(self.tmp / 'StreamingAccounts.json'))
        res = subprocess.run(['sh', str(ba)], env=env, capture_output=True, text=True)
        self.assertEqual(res.returncode, 0, res.stderr)
        cmds = self.host_commands.read_text()
        self.host_commands.write_text('')
        for gone in ('systemd/system/enginex-sc5000-deemixd.service', 'systemd/system/engine.service.d/99-sc5000.conf',
                     'systemd/system/edisksd.service.d/99-sc5000.conf', 'sc5000-engine-script', 'sc5000-assignments',
                     'sc5000', 'sc5000-boot-converted', 'sc5000-tz-newer'):
            self.assertFalse(os.path.lexists(run_dir / gone), gone)
        self.assertEqual(sorted(p.name for p in units.glob('enginex-*.service')),
                         ['enginex-deemixd.service', 'enginex-rb-bridge.service', 'enginex-stream-sync.service'])
        self.assertIn(f'ExecStart={dest}/bundle/bin/enginex-deemixd ', (units / 'enginex-deemixd.service').read_text())
        self.assertNotIn('ExecStart=old', (units / 'enginex-stream-sync.service').read_text())
        self.assertEqual([p.name for p in (units / 'engine.service.d').iterdir()], ['99-enginex.conf'])
        self.assertEqual((units / 'foreign.service').read_text(), 'not ours\n')
        self.assertTrue((run_dir / 'enginex-boot-converted').exists() and (run_dir / 'enginex-tz-newer').exists())
        self.assertIn('stop enginex-sc5000-deemixd.service', cmds)
        self.assertIn('reset-failed enginex-sc5000-deemixd.service', cmds)
        self.assertIn('restart --no-block enginex-stream-sync.service enginex-rb-bridge.service enginex-deemixd.service', cmds)
        drop = (units / 'engine.service.d/99-enginex.conf').read_text()
        self.assertNotIn('sc5000', drop.replace('/data/sc5000-custom', ''))
        self.assertIn('Wants=enginex-stream-sync.service enginex-rb-bridge.service enginex-deemixd.service', drop)
        # The leave step is in place for a later downgrade, outside /data.
        leave = run_dir / 'enginex/leave.sh'
        self.assertTrue(leave.is_file() and os.access(leave, os.X_OK))
        # A second run (the next boot) has nothing old left to stop.
        res = subprocess.run(['sh', str(ba)], env=env, capture_output=True, text=True)
        self.assertEqual(res.returncode, 0, res.stderr)
        self.assertNotIn('enginex-sc5000-deemixd', self.host_commands.read_text())
        self.host_commands.write_text('')

    def test_234_leave_step_installs_the_bundle_through_the_suite_first(self):
        """#234 review R1/R2: leave.sh --install hands the unpacked bundle to this release's suite (an
        older one goes in through a stage and atomic swaps, internal/leave) before anything else;
        when that fails this release keeps its units and nothing reboots."""
        root = self.tmp / 'leave-install'
        unit = root / 'run/systemd/system/enginex-deemixd.service'
        unit.parent.mkdir(parents=True)
        unit.write_text('unit')
        calls = self.tmp / 'suite.calls'
        suite = self.tmp / 'fake-suite'
        leave = self.tmp / 'leave.sh'
        leave.write_text(bdb.LEAVE_SH)
        env = {**self.env, 'ENGINEX_LEAVE_ROOT': str(root), 'ENGINEX_LEAVE_SUITE': str(suite)}
        for code in (1, 0):
            suite.write_text(f'#!/bin/sh\necho "$*" >> {calls}\nexit {code}\n')
            suite.chmod(0o755)
            res = subprocess.run(['sh', str(leave), '--install', '/data/.enginex-leave.incoming/b', '--no-reboot'],
                                 env=env, capture_output=True, text=True)
            self.assertEqual(res.returncode, code, res.stderr)
            self.assertEqual(unit.exists(), code == 1)
        self.assertEqual(calls.read_text().splitlines(), ['leave install /data/.enginex-leave.incoming/b'] * 2)

    def test_234_only_the_boot_run_puts_back_an_interrupted_downgrade(self):
        """#234 review R2: boot-activate.sh's boot run first asks the suite to put back a downgrade a
        power cut interrupted (leave recover), from the directory it was started from (the hook's
        fallback is /data/sc5000-custom/bundle); its runs on every Engine start never do."""
        src = self.extract(self.build())
        dest = self.tmp / 'recover' / 'enginex'
        (dest / 'bundle/bin').mkdir(parents=True)
        calls = self.tmp / 'recover.calls'
        suite = dest / 'bundle/bin/enginex-suite'
        suite.write_text(f'#!/bin/sh\necho "$*" >> {calls}\n')
        suite.chmod(0o755)
        run_dir = self.tmp / 'run-recover'
        ba = dest / 'bundle/boot-activate.sh'
        ba.write_text((src / 'boot-activate.sh').read_text().replace('/run/', str(run_dir) + '/'))
        env = dict(self.env, ENGINEX_CUSTOM=str(dest), ENGINEX_LEAVE_ROOT=str(self.tmp / 'recover'),
                   ENGINEX_PID1_COMM=str(self.pid1_systemd()), ENGINEX_CONFIG_DIR=str(self.tmp / 'recover/.config'),
                   ENGINEX_TIMESTAMP_FILE=str(self.tmp / 'no-ts'), ENGINEX_PRODUCT_CODE_FILE=str(self.tmp / 'no-product-code'))
        subprocess.run(['sh', str(ba), '--restore-tz'], env=env, capture_output=True, text=True)
        self.assertFalse(calls.exists())
        subprocess.run(['sh', str(ba)], env=env, capture_output=True, text=True)
        self.assertEqual(calls.read_text().splitlines()[:1], ['leave recover'])
        self.host_commands.write_text('')  # the boot run's systemctl calls went to the safe-bin stand-in

    def test_234_a_failed_recovery_stops_the_boot_run_before_activation(self):
        """#234 review round 3: when putting back an interrupted downgrade fails, the boot run stops
        there and says why: no migration, no Engine drop-in, no unit started. The deck keeps the
        generation it has and the journal stays for the next boot."""
        src = self.extract(self.build())
        dest = self.tmp / 'recover-fails' / 'enginex'
        (dest / 'bundle/bin').mkdir(parents=True)
        suite = dest / 'bundle/bin/enginex-suite'
        suite.write_text('#!/bin/sh\necho "leave: recover: input/output error" >&2\nexit 17\n')
        suite.chmod(0o755)
        run_dir = self.tmp / 'run-recover-fails'
        ba = dest / 'bundle/boot-activate.sh'
        ba.write_text((src / 'boot-activate.sh').read_text().replace('/run/', str(run_dir) + '/'))
        env = dict(self.env, ENGINEX_CUSTOM=str(dest), ENGINEX_LEAVE_ROOT=str(self.tmp / 'recover-fails'),
                   ENGINEX_PID1_COMM=str(self.pid1_systemd()), ENGINEX_CONFIG_DIR=str(self.tmp / 'recover-fails/.config'),
                   ENGINEX_TIMESTAMP_FILE=str(self.tmp / 'no-ts'), ENGINEX_PRODUCT_CODE_FILE=str(self.tmp / 'no-product-code'))
        res = subprocess.run(['sh', str(ba)], env=env, capture_output=True, text=True)
        self.assertNotEqual(res.returncode, 0)
        self.assertIn('input/output error', res.stderr)
        self.assertFalse((run_dir / 'systemd/system/engine.service.d/99-enginex.conf').exists())
        self.assertFalse(run_dir.exists() and any(run_dir.rglob('*.service')))
        self.assertEqual(self.host_commands.read_text(), '')

    def test_234_leave_step_removes_only_this_releases_units(self):
        """#234: the leave step before an older release is applied removes this release's enginex units,
        its 99-enginex.conf drop-ins, the bound Engine script and the suite cache, nothing else, and
        follows no symlinked directory (a units dir that points at /data/Music loses nothing)."""
        root = self.tmp / 'leave-root'
        units = root / 'run/systemd/system'
        ours = ['enginex-deemixd.service', 'enginex-stream-sync.service', 'enginex-rb-bridge.service',
                'engine.service.d/99-enginex.conf', 'edisksd.service.d/99-enginex.conf']
        theirs = ['engine-guardian.service', 'foreign.service', 'engine.service.d/50-user.conf',
                  'sshd.service.d/51-port.conf']
        for name in ours + theirs:
            (units / name).parent.mkdir(parents=True, exist_ok=True)
            (units / name).write_text(name)
        (units / 'enginex-masked.service').symlink_to('/dev/null')
        (root / 'run/enginex-engine-script').write_text('#!/bin/sh\n')
        (root / 'data/enginex/bin').mkdir(parents=True)
        for f in ('enginex-suite', 'enginex-suite.id', 'keep'):
            (root / 'data/enginex/bin' / f).write_text(f)
        leave = self.tmp / 'leave.sh'
        leave.write_text(bdb.LEAVE_SH)
        env = {**self.env, 'ENGINEX_LEAVE_ROOT': str(root)}
        res = subprocess.run(['sh', str(leave)], env=env, capture_output=True, text=True)
        self.assertEqual(res.returncode, 0, res.stderr)
        for name in ours + ['enginex-masked.service']:
            self.assertFalse(os.path.lexists(units / name), name)
        for name in theirs:
            self.assertEqual((units / name).read_text(), name)
        self.assertFalse((root / 'run/enginex-engine-script').exists())
        self.assertEqual(sorted(p.name for p in (root / 'data/enginex/bin').iterdir()), ['keep'])
        # Symlinked directories on the way are never followed.
        music = self.tmp / 'Music'
        (music / 'engine.service.d').mkdir(parents=True)
        for name in ('enginex-song.service', 'engine.service.d/99-enginex.conf'):
            (music / name).write_text('a song')
        shutil.rmtree(root / 'run/systemd')
        (root / 'run/systemd').mkdir()
        (root / 'run/systemd/system').symlink_to(music)
        res = subprocess.run(['sh', str(leave)], env=env, capture_output=True, text=True)
        self.assertEqual(res.returncode, 0, res.stderr)
        (root / 'run/systemd/system').unlink()
        (root / 'run/systemd/system').mkdir()
        (root / 'run/systemd/system/engine.service.d').symlink_to(music / 'engine.service.d')
        res = subprocess.run(['sh', str(leave)], env=env, capture_output=True, text=True)
        self.assertEqual(res.returncode, 0, res.stderr)
        self.assertEqual((music / 'enginex-song.service').read_text(), 'a song')
        self.assertEqual((music / 'engine.service.d/99-enginex.conf').read_text(), 'a song')
        # boot-activate.sh carries this very script.
        self.assertIn(bdb.LEAVE_SH, bdb.BOOT_ACTIVATE)

    def pid1_systemd(self):
        comm = self.tmp / 'pid1-comm'
        comm.write_text('systemd\n')
        return comm

    def test_boot_sticks_are_converted_between_stock_and_hooked_engine(self):
        """BH-F52-6: boot-activate converted a rekordbox stick present at boot while the stock Engine
        ran (and could have the stick open, or the DJ load from it), then restarted Engine under the
        DJ. The conversion now runs once per boot from the hooked Engine's ExecStartPre."""
        src = self.extract(self.build())
        root = self.tmp / 'gen2-convert'
        dest = root / 'enginex'
        (dest / 'bundle/bin').mkdir(parents=True)
        (dest / 'state').mkdir()
        calls = self.tmp / 'rb-bridge.calls'
        bridge = dest / 'bundle/bin/rb-bridge'
        bridge.write_text(f'#!/bin/sh\necho "$*" >> {calls}\n')
        bridge.chmod(0o755)
        media = self.tmp / 'media'
        (media / 'STICK/PIONEER').mkdir(parents=True)
        (media / 'PLAIN').mkdir()
        run_dir = self.tmp / 'run-convert'
        ba = self.tmp / 'boot-activate-convert.sh'
        ba.write_text((src / 'boot-activate.sh').read_text().replace('/run/', str(run_dir) + '/'))
        env = dict(self.env, ENGINEX_CUSTOM=str(dest), ENGINEX_MEDIA_ROOT=str(media),
                   ENGINEX_CONVERT_MARK=str(run_dir / 'boot-converted'),
                   ENGINEX_PID1_COMM=str(self.pid1_systemd()),
                   ENGINEX_ROOT_SSH_DIR=str(root / 'root-ssh'),
                   ENGINEX_STREAMING_ACCOUNTS=str(root / 'StreamingAccounts.json'),
                   ENGINEX_CONFIG_DIR=str(root / '.config'),
                   ENGINEX_TIMESYNC_LIB_DIR=str(root / 'timesync'),
                   ENGINEX_PRODUCT_CODE_FILE=str(self.tmp / 'no-product-code'),
                   ENGINEX_TZ_NEWER_MARKER=str(run_dir / 'tz-newer'))
        res = subprocess.run(['sh', str(ba)], env=env, capture_output=True, text=True)
        self.assertEqual(res.returncode, 0, res.stderr)
        self.assertIn('restart --no-block engine.service', self.host_commands.read_text())
        self.assertFalse(calls.exists(), 'converted while the stock Engine was still running')
        self.host_commands.write_text('')
        dropin = (run_dir / 'systemd/system/engine.service.d/99-enginex.conf').read_text().splitlines()
        pre = dropin.index(next(l for l in dropin if l.endswith('engine-ui-guard pre')))
        self.assertTrue(dropin[pre + 1].startswith('ExecStartPre=-') and dropin[pre + 1].endswith('boot-activate.sh --convert-mounted'),
                        dropin)
        for _ in range(2):  # the hooked Engine's first start converts; a later restart does not repeat it
            res = subprocess.run(['sh', str(ba), '--convert-mounted'], env=env, capture_output=True, text=True)
            self.assertEqual(res.returncode, 0, res.stderr)
        self.assertEqual(calls.read_text().splitlines(), [f'--mount-path {media}/STICK'])

    def test_boot_activate_follows_console_ssh_settings(self):
        """BH-GC2-9 / BH-GF4-15: boot-activate.sh started sshd whenever a key was stored, even
        after the console turned SSH off, never without a key (a root password set in the
        console did not survive a reboot), and always on port 22 (the console's port drop-in
        lives in /run)."""
        src = self.extract(self.build())
        root = self.tmp / 'gen2-ssh'
        dest = root / 'enginex'
        (dest / 'state').mkdir(parents=True)
        run_dir = self.tmp / 'run-ssh'
        ba = self.tmp / 'boot-activate-ssh.sh'
        ba.write_text((src / 'boot-activate.sh').read_text().replace('/run/', str(run_dir) + '/'))
        sshd = self.tmp / 'fake-sshd'
        sshd.write_text('#!/bin/sh\nexit 0\n')
        sshd.chmod(0o755)
        env = dict(self.env, ENGINEX_CUSTOM=str(dest), ENGINEX_SSHD=str(sshd),
                   ENGINEX_PID1_COMM=str(self.pid1_systemd()),
                   ENGINEX_ROOT_SSH_DIR=str(root / 'root-ssh'),
                   ENGINEX_STREAMING_ACCOUNTS=str(root / 'StreamingAccounts.json'),
                   ENGINEX_CONFIG_DIR=str(root / '.config'),
                   ENGINEX_TIMESYNC_LIB_DIR=str(root / 'timesync'),
                   ENGINEX_PRODUCT_CODE_FILE=str(self.tmp / 'no-product-code'),
                   ENGINEX_TZ_NEWER_MARKER=str(run_dir / 'tz-newer'))
        dropin = run_dir / 'systemd/system/sshd.service.d/51-port.conf'
        cfg = dest / 'state/ssh-config.json'
        key = dest / 'authorized_keys'

        def boot(key_text, config):
            if key_text:
                key.write_text(key_text)
            elif key.exists():
                key.unlink()
            if config is None:
                cfg.unlink(missing_ok=True)
            else:
                cfg.write_text(json.dumps(config, indent=2))
            if dropin.exists():
                dropin.unlink()
            self.host_commands.write_text('')
            res = subprocess.run(['sh', str(ba)], env=env, capture_output=True)
            self.assertEqual(res.returncode, 0, res.stderr)
            started = 'start --no-block sshd.service' in self.host_commands.read_text()
            self.host_commands.write_text('')
            return started

        k = 'ssh-ed25519 AAAA test\n'
        self.assertTrue(boot(k, None))  # #116: a key alone, no console settings: as before
        self.assertFalse(dropin.exists())
        self.assertFalse(boot(None, None))  # #46: no key, no SSH
        self.assertFalse(boot(k, {'enabled': False, 'port': 22}))  # turned off in the console
        self.assertTrue(boot(k, {'enabled': True, 'port': 2222}))
        self.assertIn('-p 2222', dropin.read_text())
        self.assertTrue(boot(None, {'enabled': True, 'port': 22, 'hasPassword': True}))
        self.assertFalse(boot(None, {'enabled': True, 'port': 2222, 'hasPassword': False}))
        self.assertFalse(dropin.exists())
        self.assertEqual((root / 'root-ssh/authorized_keys').read_text(), k)

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
            '/usr/bin/az01-ethernet-speed 100\n'
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
                   ENGINEX_CUSTOM=str(dest),
                   ENGINEX_TIMESTAMP_FILE=str(self.tmp / 'no-ts'),
                   ENGINEX_CONFIG_DIR=str(root / '.config'))

        # 1. Explicit gen2-locked must prevent boot-activate.sh from creating gen2-unlocked.
        (dest / 'gen2-locked').write_text('1\n')
        res = subprocess.run(['sh', str(ba_reloc)], env=env, capture_output=True, text=True)
        self.assertEqual(res.returncode, 0, res.stderr)
        self.assertFalse((dest / 'gen2-unlocked').exists())
        self.host_commands.write_text('')

        stage = run_dir / 'enginex-engine-script'
        rewritten = stage.read_text()
        lib_hook = f'{dest}/bundle/lib/libengine-ui-hook.so'
        self.assertIn(f'\t{dest}/bundle/bin/engine-ui-guard launch /usr/Engine/Engine -d0', rewritten)
        self.assertIn(f'\t\tenv LD_PRELOAD="{lib_hook}" /usr/sbin/az01-update --file "$UPDATE_FILE"\n', rewritten)
        self.assertIn(f'\t\tenv LD_PRELOAD="{lib_hook}" /usr/bin/planck-remote-screen --update "$UPDATE_FILE"\n', rewritten)
        self.assertIn(f'\t\tenv LD_PRELOAD="{lib_hook}" /usr/sbin/az01-update --erase-data\n', rewritten)
        # BH-GC2-6: Gigabit stays Gigabit, as on first-generation images.
        self.assertIn('\n/usr/bin/az01-ethernet-speed 1000\n', rewritten)
        self.assertNotIn('az01-ethernet-speed 100\n', rewritten)

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

    def test_installer_launchers_pass_their_arguments_on(self):
        """BH-G2-8: the macOS and Windows launchers started the installer without arguments, so
        `Install-EngineX.command --uninstall` ran an install."""
        out_dir = self.tmp / 'launchers'
        out_dir.mkdir()
        fake_bin = self.tmp / 'launcher-go-bin'
        fake_bin.mkdir()
        (fake_bin / 'go').write_text(
            '#!/bin/sh\nout=""\nwhile [ $# -gt 0 ]; do\n'
            '  if [ "$1" = "-o" ]; then out="$2"; shift 2; else shift; fi\n'
            'done\nprintf \'#!/bin/sh\\nprintf "%%s\\\\n" "$@" > "$ARGS_LOG"\\n\' > "$out"\nchmod 755 "$out"\n')
        (fake_bin / 'go').chmod(0o755)
        env = {**self.env, 'PATH': f"{fake_bin}:{self.env['PATH']}"}
        res = subprocess.run(['sh', str(HERE / 'build-installer.sh'), '5.3.0.4', str(out_dir)],
                             env=env, capture_output=True, text=True)
        self.assertEqual(res.returncode, 0, res.stderr)
        args = ['--uninstall', '--image', 'my deck.img']
        for t in ('linux-amd64', 'macos-arm64'):
            with self.subTest(t):
                d = self.tmp / f'unpacked-{t}'
                if t.startswith('linux'):
                    d.mkdir()
                    with tarfile.open(out_dir / f'EngineX-Installer-5.3.0.4-{t}.tar.gz') as tf:
                        tf.extractall(d)
                    launcher = d / 'install-enginex.sh'
                else:
                    with zipfile.ZipFile(out_dir / f'EngineX-Installer-5.3.0.4-{t}.zip') as z:
                        z.extractall(d)
                    for f in ('Install-EngineX.command', 'enginex-installer'):
                        (d / f).chmod(0o755)  # extractall drops the mode bits the zip keeps
                    launcher = d / 'Install-EngineX.command'
                log = self.tmp / f'args-{t}'
                res = subprocess.run(['sh', str(launcher), *args], env={**env, 'ARGS_LOG': str(log)},
                                     input='\n', capture_output=True, text=True)
                self.assertEqual(res.returncode, 0, res.stderr)
                self.assertEqual(log.read_text().splitlines(), args)
        with zipfile.ZipFile(out_dir / 'EngineX-Installer-5.3.0.4-windows-amd64.zip') as z:
            bat = z.read('Install-EngineX.bat').decode()
        self.assertIn('\r\nenginex-installer.exe %*\r\n', bat)

if __name__ == '__main__':
    unittest.main()
