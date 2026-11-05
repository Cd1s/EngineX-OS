import hashlib
import importlib.util
import io
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


class DataBundleTest(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.suite = self.tmp / 'sc5000-suite'
        self.suite.write_bytes(b'\x7fELF suite')
        self.hook = self.tmp / 'libengine-ui-hook.so'
        self.hook.write_bytes(b'\x7fELF hook')
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
            t.extractall(dest, filter='data')
        return dest / TOP

    def test_bundle_layout_and_manifest(self):
        r = self.build()
        bundle = Path(r['bundle'])
        self.assertEqual(bundle.name, TOP + '.tar.gz')
        self.assertEqual((bundle.parent / 'SHA256SUMS.txt').read_text(),
                         f"{hashlib.sha256(bundle.read_bytes()).hexdigest()}  {bundle.name}\n")
        src = self.extract(r)
        self.assertEqual((src / 'version.txt').read_text(), '5.3.0.1.1\n')
        self.assertEqual((src / 'bin/sc5000-suite').read_bytes(), b'\x7fELF suite')
        self.assertEqual((src / 'lib/libengine-ui-hook.so').read_bytes(), b'\x7fELF hook')
        for applet in bdb.APPLETS:
            self.assertIn(f'sc5000-suite" {applet} ', (src / 'bin' / applet).read_text())
            self.assertTrue((src / 'bin' / applet).stat().st_mode & 0o100)
        listed = {}
        for line in (src / 'SHA256SUMS').read_text().splitlines():
            digest, name = line.split('  ')
            listed[name] = digest
            self.assertEqual(hashlib.sha256((src / name).read_bytes()).hexdigest(), digest, name)
        files = {str(p.relative_to(src)) for p in src.rglob('*') if p.is_file()}
        self.assertEqual(set(listed) | {'SHA256SUMS'}, files)

    def test_rebuild_is_identical(self):
        a, b = self.build('a'), self.build('b')
        self.assertEqual(a['sha256'], b['sha256'])

    def test_refuses_bad_input(self):
        (self.tmp / 'taken').mkdir()
        for kw in ({'version': '5.3'}, {'version': '5.3.0-x'}, {'out': 'taken'},
                   {'suite': self.tmp / 'packs.tar.gz'},
                   {'ui_packs': self.ui_tar({'icons/x.svg': b''}, 'nopacks.tar.gz')},
                   {'ui_packs': self.ui_tar({'packs/x': b'', '../evil': b''}, 'evil.tar.gz')}):
            with self.subTest(kw=kw), self.assertRaises(SystemExit):
                self.build(**kw)

    def sh(self, script, dest, check=True):
        return subprocess.run(['sh', str(script)], env={'PATH': '/usr/bin:/bin', 'SC5000_CUSTOM': str(dest)},
                              capture_output=True, text=True, check=check)

    def test_install_then_uninstall_keeps_user_data(self):
        src = self.extract(self.build())
        dest = self.tmp / 'data/sc5000-custom'
        dest.mkdir(parents=True)
        (dest / 'authorized_keys').write_text('ssh-ed25519 AAAA test\n')
        (dest / 'ui').mkdir()
        (dest / 'ui/safe-mode').write_text('1')
        self.sh(src / 'install.sh', dest)
        self.assertEqual((dest / 'bundle/bin/sc5000-suite').read_bytes(), b'\x7fELF suite')
        self.assertEqual((dest / 'ui/packs/a/Main.qml').read_bytes(), b'Item {}')
        self.assertTrue((dest / 'ui/icons/x.svg').exists())
        self.assertFalse((dest / 'ui/safe-mode').exists())
        self.assertEqual((dest / 'version.txt').read_text(), '5.3.0.1.1\n')
        self.assertEqual(oct(dest.stat().st_mode & 0o777), '0o700')
        self.assertEqual([p.name for p in dest.glob('.bundle.*')], [])
        self.sh(src / 'install.sh', dest)  # reinstall over itself
        self.sh(dest / 'bundle/uninstall.sh', dest)
        self.assertEqual(sorted(p.name for p in dest.iterdir()), ['authorized_keys', 'ui'])
        self.assertEqual(list((dest / 'ui').iterdir()), [])
        self.assertNotEqual(self.sh(src / 'uninstall.sh', dest, check=False).returncode, 0)

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


if __name__ == '__main__':
    unittest.main()
