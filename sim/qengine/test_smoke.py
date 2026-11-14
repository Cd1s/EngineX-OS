import os
import subprocess
import tempfile
import unittest
from pathlib import Path

class Tier2SmokeTest(unittest.TestCase):
    def test_tier2_arch_guard(self):
        root = Path(__file__).resolve().parent.parent.parent
        script = root / 'tests' / 'test_tier2_arch_guard.sh'
        res = subprocess.run(['/bin/bash', str(script)], capture_output=True, text=True)
        self.assertEqual(res.returncode, 0, res.stdout + res.stderr)

    def test_failed_builder_cleans_its_container_and_private_tmp(self):
        root = Path(__file__).resolve().parents[2]
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp)
            work = base / 'work'
            upstream = work / 'QEngine'
            (upstream / '.git').mkdir(parents=True)
            scripts = upstream / 'scripts/build_scripts'
            scripts.mkdir(parents=True)
            (upstream / 'build').mkdir()
            for name in ('vmlinuz-generic-armhf', 'initrd-generic-armhf'):
                (upstream / 'build' / name).write_text('fixture')
            sibling = work / 'tmp/sibling'
            sibling.mkdir(parents=True)
            firmware = base / 'firmware.img'
            firmware.touch()
            log = base / 'docker.log'
            tools = base / 'bin'
            tools.mkdir()

            def script(path, body):
                path.write_text('#!/bin/bash\nset -eu\n' + body)
                path.chmod(0o755)

            script(scripts / 'new_instance.sh', 'docker run fixture\n')
            script(tools / 'uname', 'echo x86_64\n')
            script(tools / 'git', 'echo 9c2dfd6f206c2e2c3d77cfd1ac483350c68a77c8\n')
            script(tools / 'docker', '''
if [ "$1" = run ]; then
    [ "$2" = --cidfile ] || exit 99
    printf 'fixture-cid\\n' > "$3"
    exit 42
fi
printf '%s\\n' "$*" >> "$DOCKER_TEST_LOG"
''')
            for name in ('qemu-system-aarch64', 'qemu-img', 'file', 'dumpe2fs',
                         'debugfs', 'losetup', 'umount'):
                script(tools / name, 'exit 0\n')
            script(tools / 'findmnt', '''
echo /
if [ "${SIMULATE_MOUNT:-0}" = 1 ]; then echo "$TMPDIR/mounted"; fi
''')
            env = dict(os.environ, PATH=f'{tools}:{os.environ["PATH"]}',
                       QENGINE_WORK_DIR=str(work), DOCKER_TEST_LOG=str(log))
            res = subprocess.run(['bash', str(root / 'sim/qengine/tier2.sh'),
                                  str(firmware)], env=env, capture_output=True,
                                 text=True, timeout=15)
            self.assertEqual(res.returncode, 42, res.stdout + res.stderr)
            self.assertEqual(log.read_text(), 'rm -f fixture-cid\n')
            self.assertTrue(sibling.is_dir())
            self.assertEqual(list(work.glob('tmp.*')), [])

            res = subprocess.run(['bash', str(root / 'sim/qengine/tier2.sh'),
                                  str(firmware)], env=dict(env, SIMULATE_MOUNT='1'),
                                 capture_output=True, text=True, timeout=15)
            self.assertEqual(res.returncode, 1, res.stdout + res.stderr)
            self.assertIn('mounts remain', res.stderr)
            self.assertEqual(len(list(work.glob('tmp.*'))), 1)
            self.assertTrue(sibling.is_dir())

if __name__ == '__main__':
    unittest.main()
