import subprocess
import sys
import unittest
from pathlib import Path

class Tier2SmokeTest(unittest.TestCase):
    def test_tier2_arch_guard(self):
        root = Path(__file__).resolve().parent.parent.parent
        script = root / 'tests' / 'test_tier2_arch_guard.sh'
        res = subprocess.run(['/bin/bash', str(script)], capture_output=True, text=True)
        self.assertEqual(res.returncode, 0, res.stdout + res.stderr)

if __name__ == '__main__':
    unittest.main()
