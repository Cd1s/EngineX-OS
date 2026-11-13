#!/usr/bin/env python3
"""This release says sc5000 where a later one says enginex. It is the release users
roll back to, so its bundle scripts remove what the later release left behind."""
import shutil
import subprocess
import unittest

import test_data_bundle as tdb


class RenameCompatTest(unittest.TestCase):
    setUp = tdb.DataBundleTest.setUp
    tearDown = tdb.DataBundleTest.tearDown
    build = tdb.DataBundleTest.build
    extract = tdb.DataBundleTest.extract
    ui_tar = tdb.DataBundleTest.ui_tar
    sh = tdb.DataBundleTest.sh

    def activate(self, src, foreign):
        """Runs boot-activate.sh with /run moved into the test directory."""
        dest = self.tmp / 'deck/enginex'
        (dest / 'bundle/bin').mkdir(parents=True, exist_ok=True)
        shutil.copy2(src / 'bin/engine-ui-guard', dest / 'bundle/bin/engine-ui-guard')
        (dest / 'bundle/alsa-dir').write_text('system/var-lib/alsa/conf.d\n')
        stub = dest / 'bundle/bin/stream-sync'
        stub.write_text('#!/bin/sh\nexit 0\n')
        stub.chmod(0o755)
        run, alsa = self.tmp / 'run', self.tmp / 'alsa'
        for d in (run / 'systemd/system/engine.service.d', run / 'systemd/system/edisksd.service.d', alsa):
            d.mkdir(parents=True, exist_ok=True)
        engine_script = self.tmp / 'vendor-engine-script'
        engine_script.write_text('#!/bin/sh\n\t/usr/Engine/Engine -d0\n')
        (alsa / '99-sc5000-custom.conf').write_text('ours\n')
        (run / 'systemd/system/enginex-stream-sync.service').write_text('a unit of this name from before\n')
        if foreign:
            (run / 'systemd/system/enginex-deemixd.service').write_text('[Service]\nExecStart=/data/enginex/bundle/bin/enginex-deemixd\n')
            (run / 'systemd/system/enginex-suite-watch.service').write_text('[Service]\n')
            (run / 'systemd/system/engine.service.d/99-enginex.conf').write_text('[Service]\nEnvironment=LD_PRELOAD=/x\n')
            (run / 'systemd/system/edisksd.service.d/99-enginex.conf').write_text('[Service]\n')
            (run / 'enginex-engine-script').write_text('#!/bin/sh\n')
            (alsa / '99-enginex.conf').write_text('theirs\n')
        script = self.tmp / 'boot-activate-moved.sh'
        script.write_text((src / 'boot-activate.sh').read_text().replace('/run/', str(run) + '/')
                          .replace('/usr/Engine/Scripts/engine', str(engine_script)))
        env = dict(self.env, SC5000_CUSTOM=str(dest), SC5000_ALSA_OVERLAY=str(alsa),
                   SC5000_TIMESTAMP_FILE=str(self.tmp / 'no-ts'), SC5000_CONFIG_DIR=str(self.tmp / 'deck/.config'))
        res = subprocess.run(['sh', str(script)], env=env, capture_output=True, text=True)
        self.assertEqual(res.returncode, 0, res.stderr)
        calls = self.host_commands.read_text()
        self.host_commands.write_text('')
        units = sorted(p.name for p in (run / 'systemd/system').glob('*.service'))
        dropins = sorted(str(p.relative_to(run / 'systemd/system')) for p in (run / 'systemd/system').glob('*.d/*.conf'))
        return run, alsa, units, dropins, calls

    def test_activation_after_a_later_release_leaves_one_set_of_units(self):
        src = self.extract(self.build())
        run, alsa, units, dropins, calls = self.activate(src, foreign=True)
        # only this release's units: nothing the later release named is left to run next to them
        self.assertEqual([u for u in units if u.startswith('enginex-')], ['enginex-stream-sync.service'])
        self.assertNotIn('a unit of this name', (run / 'systemd/system/enginex-stream-sync.service').read_text())
        self.assertEqual(dropins, ['engine.service.d/99-sc5000.conf'])
        self.assertFalse((run / 'enginex-engine-script').exists())
        self.assertEqual(sorted(p.name for p in alsa.iterdir()), ['99-sc5000-custom.conf'])
        self.assertIn('pkill -x enginex-suite', calls)

    def test_activation_without_a_later_release_touches_nothing_else(self):
        src = self.extract(self.build())
        run, alsa, units, dropins, calls = self.activate(src, foreign=False)
        self.assertEqual([u for u in units if u.startswith('enginex-')], ['enginex-stream-sync.service'])
        self.assertEqual(dropins, ['engine.service.d/99-sc5000.conf'])
        self.assertEqual(sorted(p.name for p in alsa.iterdir()), ['99-sc5000-custom.conf'])
        self.assertNotIn('enginex-suite', calls)

    def test_uninstall_removes_the_alsa_file_of_a_later_release(self):
        src = self.extract(self.build())
        dest, alsa = self.tmp / 'custom', self.tmp / 'alsa'
        self.sh(src / 'install.sh', dest, alsa_dest=alsa)
        (alsa / '99-enginex.conf').write_text('theirs\n')
        (alsa / 'someone-elses.conf').write_text('kept\n')
        self.sh(dest / 'bundle/uninstall.sh', dest, alsa_dest=alsa)
        self.assertEqual(sorted(p.name for p in alsa.iterdir()), ['someone-elses.conf'])


if __name__ == '__main__':
    unittest.main()
