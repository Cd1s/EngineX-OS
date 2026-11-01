"""Exercise the smoke controller with a real Unix QMP socket, without QEMU."""
import json
from pathlib import Path
import socket
import subprocess
import sys
import tempfile
import threading
import time
import unittest


class SmokeRetryTest(unittest.TestCase):
    def test_transient_qmp_timeout_reconnects(self):
        script = Path(__file__).with_name('run_qengine.sh').read_text()
        code = script.split("<<'PY'\n", 1)[1].rsplit('\nPY', 1)[0]
        # The smoke controller is the script's final Python heredoc.
        if 'scope' not in code or 'guest-userspace' not in code:
            self.fail('smoke controller not found')
        for phase in ('greeting', 'query'):
            with self.subTest(phase=phase), tempfile.TemporaryDirectory() as tmp:
                work = Path(tmp)
                (work / 'serial.log').write_text('Linux version 6.1\nsystemd[1]: started\n')
                listener = socket.socket(socket.AF_UNIX)
                listener.bind(str(work / 'qmp.sock'))
                listener.listen(4)
                listener.settimeout(7)
                failures = []
                accepts = []

                def serve():
                    try:
                        while len(accepts) < 2:
                            conn, _ = listener.accept()
                            accepts.append(1)
                            with conn:
                                if len(accepts) == 1 and phase == 'greeting':
                                    time.sleep(1.2)
                                    continue
                                conn.sendall(b'{"QMP":{}}\n')
                                with conn.makefile('rb') as stream:
                                    for line in stream:
                                        command = json.loads(line)['execute']
                                        if len(accepts) == 1 and command == 'query-status':
                                            time.sleep(1.2)
                                            break
                                        values = {
                                            'qmp_capabilities': {},
                                            'query-status': {'running': True},
                                            'query-block': [{'device': 'rekordbox', 'inserted': {'ro': True}}],
                                            'human-monitor-command': 'usb-storage drive = "rekordbox"',
                                        }
                                        conn.sendall((json.dumps({'return': values[command]}) + '\n').encode())
                    except Exception as exc:
                        failures.append(exc)
                    finally:
                        listener.close()

                worker = threading.Thread(target=serve, daemon=True)
                worker.start()
                result = subprocess.run(
                    [sys.executable, '-c', code, tmp, '6', sys.executable, '-c', 'import time; time.sleep(15)'],
                    capture_output=True, text=True, timeout=12)
                worker.join(timeout=8)
                self.assertFalse(worker.is_alive(), 'QMP test server did not finish')
                self.assertFalse(failures, failures)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertEqual(json.loads((work / 'status.json').read_text())['result'], 'PASS')
                self.assertEqual(len(accepts), 2)


if __name__ == '__main__':
    unittest.main()
