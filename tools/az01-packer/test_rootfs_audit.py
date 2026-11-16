import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

import rootfs_audit as ra
import legacy_names as legacy


class AuditTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        base = Path(self.tmp.name)
        self.root, self.overlay = base/'root', base/'overlay'
        for d in ('usr/bin', 'usr/sbin', 'usr/lib/systemd/system', 'etc/systemd/system', 'usr/libexec'):
            (self.root/d).mkdir(parents=True)
        os.symlink('usr/bin', self.root/'bin')        # merged /usr, as on Engine OS
        os.symlink('/usr/sbin', self.root/'sbin')
        for tool in ('usr/bin/sh', 'usr/sbin/sshd'):
            (self.root/tool).write_text('')
        (self.root/'usr/lib/systemd/system/data.mount').write_text('[Mount]\n')
        (self.root/'usr/lib/systemd/system/engine.service').write_text('[Service]\n')
        (self.root/'etc/fstab').write_text('/dev/mmcblk0p9 /var/lib ext4 defaults 0 2\n')

    def tearDown(self):
        self.tmp.cleanup()

    def ship(self, rel, text):
        for top in (self.root, self.overlay):
            (top/rel).parent.mkdir(parents=True, exist_ok=True)
            (top/rel).write_text(text)

    def audit(self, require=('sshd',)):
        a = ra.Audit(str(self.root), str(self.overlay))
        return a.run(require), a

    def test_resolve_follows_absolute_and_relative_links_inside_the_root(self):
        self.assertEqual(ra.resolve(str(self.root), '/sbin/sshd'), str(self.root) + '/usr/sbin/sshd')
        self.assertEqual(ra.resolve(str(self.root), '/bin/sh'), str(self.root) + '/usr/bin/sh')
        self.assertIsNone(ra.resolve(str(self.root), '/bin/iw'))

    def test_cli_runs_with_isolated_python(self):
        self.ship('usr/lib/systemd/system/enginex-prepare.service', '[Service]\nExecStart=/bin/sh\n')
        result = subprocess.run([sys.executable, '-I', ra.__file__, str(self.root),
                                 '--overlay', str(self.overlay)], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn('0 FAIL', result.stdout)

    def test_migration_prefixes_pass_but_a_missing_command_still_fails(self):
        source = Path(__file__).resolve().parents[2] / 'rootfs-overlay/usr/libexec/enginex-migration'
        script = source.read_text()
        self.ship('usr/libexec/enginex-migration', script)
        ok, a = self.audit()
        self.assertTrue(ok, a.fails)
        prefixes = subprocess.check_output(['sh', '-c',
            '. "$1"; printf "%s\\n" "$legacy_libexec" "$legacy_bin"', 'sh', str(source)], text=True)
        self.assertEqual(prefixes.splitlines(), ['/usr/libexec/' + legacy.DATA_NAME,
                                                '/usr/local/bin/' + legacy.PROJECT])
        self.ship('usr/libexec/enginex-migration', script + '\n/usr/bin/gone\n')
        ok, a = self.audit()
        self.assertFalse(ok)
        self.assertIn('/usr/libexec/enginex-migration: calls /usr/bin/gone, not in this rootfs', a.fails)

    def test_hard_requires_on_a_vendor_mount_fails(self):
        # The 5.3.0.1 upgrade bug: 5.3.0 has no etc.mount, so prepare and SSH never ran.
        self.ship('etc/systemd/system/x.service', '[Unit]\nRequires=etc.mount\n[Service]\nExecStart=/bin/sh\n')
        ok, a = self.audit()
        self.assertFalse(ok)
        self.assertTrue(any('Requires=etc.mount is a vendor unit' in m for m in a.fails), a.fails)

    def test_legacy_image_units_are_still_audited(self):
        self.overlay.mkdir()
        unit = 'usr/lib/systemd/system/' + legacy.PROJECT + '-custom-prepare.service'
        (self.root / unit).write_text('[Service]\nExecStart=/usr/bin/gone\n')
        ok, a = self.audit()
        self.assertFalse(ok)
        self.assertIn('/' + unit + ': ExecStart command /usr/bin/gone not found', a.fails)

    def test_soft_and_mount_dependencies_pass(self):
        self.ship('etc/systemd/system/x.service', '[Unit]\nRequiresMountsFor=/data /etc\nWants=etc.mount var-lib.mount\n'
                  'After=data.mount etc.mount\nBindsTo=dev-%i.device\n[Service]\nExecStart=/sbin/sshd -D\n'
                  'ExecStartPre=-/usr/bin/missing\n')
        self.ship('etc/systemd/system/engine.service.d/o.conf', '[Unit]\nAfter=x.service\n')
        ok, a = self.audit()
        self.assertTrue(ok, a.fails)
        self.assertIn('/etc/systemd/system/x.service: Wants=etc.mount does not exist in this rootfs', a.warns)
        self.assertTrue(any('/usr/bin/missing not found' in m for m in a.warns))
        self.assertFalse(any('var-lib.mount' in m for m in a.warns))  # from /etc/fstab

    def test_missing_exec_suite_tool_and_unguarded_script_path_fail(self):
        self.ship('etc/systemd/system/x.service', '[Service]\nExecStart=/usr/bin/gone\n')
        self.ship('usr/local/bin/tune', '#!/bin/sh\n/usr/sbin/iw dev wlan0 set power_save off\n'
                  'if command -v ethtool >/dev/null; then ethtool eth0; fi\n[ -x /usr/bin/opt ] && /usr/bin/opt\n')
        ok, a = self.audit(('sshd', 'pkill'))
        self.assertFalse(ok)
        self.assertIn('/etc/systemd/system/x.service: ExecStart command /usr/bin/gone not found', a.fails)
        self.assertIn('/usr/local/bin/tune: calls /usr/sbin/iw, not in this rootfs', a.fails)
        self.assertIn('the suite runs pkill, not in this rootfs', a.fails)
        self.assertIn('/usr/local/bin/tune: calls /usr/bin/opt, not in this rootfs (guarded)', a.warns)
        self.assertIn('/usr/local/bin/tune: optional tool ethtool is absent, that branch never runs', a.warns)

    def test_overlay_file_missing_from_the_image_fails(self):
        (self.overlay/'etc').mkdir(parents=True)
        (self.overlay/'etc/only-in-overlay').write_text('x')
        ok, a = self.audit()
        self.assertIn('/etc/only-in-overlay: shipped by the overlay but missing from the image', a.fails)


if __name__ == '__main__':
    unittest.main()
