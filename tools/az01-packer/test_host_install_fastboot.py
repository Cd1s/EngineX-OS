import os
import subprocess
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
SCRIPT = HERE / 'host-install-fastboot.sh'

# A fastboot that answers getvar from $FB_DATA / $FB_USERDATA and records every call.
FAKE = """#!/bin/sh
echo "$*" >> "$FB_LOG"
case "$*" in
"getvar partition-size:data") [ -z "$FB_DATA" ] && { echo "getvar:partition-size:data FAILED (remote: 'unknown partition')" >&2; exit 1; }
    echo "partition-size:data: $FB_DATA" >&2 ;;
esac
exit 0
"""


class HostInstallFastbootTest(unittest.TestCase):
    def flash_target(self, data):
        with tempfile.TemporaryDirectory() as d:
            tmp = Path(d)
            (tmp / 'bin').mkdir()
            fb = tmp / 'bin/fastboot'
            fb.write_text(FAKE)
            fb.chmod(0o755)
            img = tmp / 'userdata.img'
            img.write_bytes(b'image')
            env = {'PATH': f"{tmp / 'bin'}:{os.environ.get('PATH', '/usr/bin:/bin')}", 'FB_LOG': str(tmp / 'log'), 'FB_DATA': data}
            r = subprocess.run(['sh', str(SCRIPT), str(img)], env=env, capture_output=True, text=True)
            self.assertEqual(r.returncode, 0, r.stderr)
            flashes = [l.split() for l in (tmp / 'log').read_text().splitlines() if l.startswith('flash ')]
            self.assertEqual(len(flashes), 1, flashes)
            self.assertEqual(flashes[0][2], str(img))
            return flashes[0][1]

    def test_data_only_when_it_has_a_size(self):
        # BH2-A7-12: a bootloader without a "data" partition may answer size 0; that is no partition.
        for answer, want in (('0x1d1c00000', 'data'), ('7812939776', 'data'), ('0x0', 'userdata'), ('0', 'userdata'),
                             ('0x00000000', 'userdata'), ('', 'userdata')):
            with self.subTest(answer=answer):
                self.assertEqual(self.flash_target(answer), want)


if __name__ == '__main__':
    unittest.main()
