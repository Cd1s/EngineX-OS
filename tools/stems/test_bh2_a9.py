import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

import render


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

    def test_render_deadline_reader_windows_pipe_and_timeout(self):
        class FakePipe:
            def fileno(self):
                return 7

        def win_select(*_args, **_kwargs):
            raise OSError(10038, 'An operation was attempted on something that is not a socket')

        fake_msvcrt = type('msvcrt', (), {'get_osfhandle': staticmethod(lambda fd: 4321)})
        polls = 0

        def fake_peek_ready(_h, _b, _sz, _rd, avail_ptr, _left):
            nonlocal polls
            polls += 1
            avail_ptr._obj.value = 0 if polls == 1 else 19
            return 1

        fake_windll = type('windll', (), {'kernel32': type('k32', (), {'PeekNamedPipe': staticmethod(fake_peek_ready)})})
        with mock.patch.dict(sys.modules, {'msvcrt': fake_msvcrt}), \
             mock.patch.object(render.os, 'name', 'nt'), \
             mock.patch.object(render.ctypes, 'windll', fake_windll, create=True), \
             mock.patch.object(render.select, 'select', side_effect=win_select), \
             mock.patch.object(render.os, 'read', return_value=b'session=1234\n\n'):
            dr = render.DeadlineReader(FakePipe(), timeout=0.2)
            self.assertEqual(dr.header(), {b'session': b'1234'})

        # Stalled Windows pipe still raises TimeoutError once deadline elapses.
        def fake_peek_empty(_h, _b, _sz, _rd, avail_ptr, _left):
            avail_ptr._obj.value = 0
            return 1

        stalled_windll = type('windll', (), {'kernel32': type('k32', (), {'PeekNamedPipe': staticmethod(fake_peek_empty)})})
        with mock.patch.dict(sys.modules, {'msvcrt': fake_msvcrt}), \
             mock.patch.object(render.os, 'name', 'nt'), \
             mock.patch.object(render.ctypes, 'windll', stalled_windll, create=True), \
             mock.patch.object(render.select, 'select', side_effect=win_select):
            dr = render.DeadlineReader(FakePipe(), timeout=0.02)
            with self.assertRaises(TimeoutError):
                dr.read(4)


if __name__ == '__main__':
    unittest.main()
