import subprocess
import tempfile
import unittest
from pathlib import Path


class RekordboxNativeRowValuesTest(unittest.TestCase):
    def test_carrier_boundary(self):
        source = Path(__file__).resolve().parents[1] / "ui-mod/test_rekordbox_rows.c"
        with tempfile.TemporaryDirectory() as directory:
            binary = Path(directory) / "test_rekordbox_rows"
            subprocess.run(["cc", "-O2", "-Wall", "-Wextra", "-Werror", str(source), "-o", str(binary)], check=True)
            subprocess.run([str(binary)], check=True)
