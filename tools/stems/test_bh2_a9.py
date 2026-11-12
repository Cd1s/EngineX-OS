import os
from pathlib import Path
import subprocess
import tempfile
import unittest


STEMS_DIR = Path(__file__).resolve().parent


class BH2A9Tests(unittest.TestCase):
    def test_run_home_worker_auto_attempts_demucs_install(self):
        script = STEMS_DIR / 'run-home-worker.sh'
        ps1 = (STEMS_DIR / 'run-home-worker.ps1').read_text(encoding='utf-8')
        with tempfile.TemporaryDirectory() as td:
            root = Path(td)
            home = root / 'home'
            venv_bin = home / 'venv' / 'bin'
            venv_bin.mkdir(parents=True)
            pip_log = root / 'pip.log'
            stemsd_args = root / 'stemsd.log'

            fake_py = venv_bin / 'python3'
            fake_py.write_text(
                '#!/bin/sh\n'
                'if [ "${1:-}" = "-c" ]; then\n'
                '  case "${2:-}" in\n'
                '    *"import numpy"*) exit 0 ;;\n'
                '    *"import demucs"*) [ -f "' + str(root / 'demucs_installed') + '" ] && exit 0 || exit 1 ;;\n'
                '    *"get_model"*) exit 0 ;;\n'
                '  esac\n'
                'fi\n'
                'if [ "${1:-}" = "-m" ] && [ "${2:-}" = "pip" ]; then\n'
                '  printf "%s\\n" "$*" >> "' + str(pip_log) + '"\n'
                '  touch "' + str(root / 'demucs_installed') + '"\n'
                '  exit 0\n'
                'fi\n'
                'if [ "${3:-}" = "auto" ] && [ "${4:-}" = "--print-backend" ]; then\n'
                '  if [ -f "' + str(root / 'demucs_installed') + '" ]; then echo htdemucs; else echo hpss; fi\n'
                '  exit 0\n'
                'fi\n'
                'exit 0\n'
            )
            fake_py.chmod(0o755)

            stemsd = root / 'enginex-stemsd'
            stemsd.write_text('#!/bin/sh\nprintf "%s\\n" "$@" > "' + str(stemsd_args) + '"\n')
            stemsd.chmod(0o755)

            env = dict(os.environ)
            env['ENGINEX_STEMS_HOME'] = str(home)
            env['ENGINEX_STEMSD'] = str(stemsd)
            res = subprocess.run(['/bin/sh', str(script), 'auto'], env=env, capture_output=True, text=True, timeout=10)
            self.assertEqual(res.returncode, 0, res.stderr)
            self.assertTrue(pip_log.exists(), 'run-home-worker.sh auto did not attempt pip install demucs')
            self.assertIn('demucs', pip_log.read_text(encoding='utf-8'))
            self.assertIn("'auto', 'demucs', 'htdemucs'", ps1)


if __name__ == '__main__':
    unittest.main()
