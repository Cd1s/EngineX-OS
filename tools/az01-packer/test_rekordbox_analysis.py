import subprocess
import tempfile
import unittest
from pathlib import Path


class RekordboxStreamedAnalysisTest(unittest.TestCase):
    def test_statement_uri_and_update(self):
        source = Path(__file__).resolve().parents[1] / "ui-mod/test_rekordbox_analysis.c"
        with tempfile.TemporaryDirectory() as directory:
            binary = Path(directory) / "test_rekordbox_analysis"
            subprocess.run(["cc", "-O2", "-Wall", "-Wextra", "-Werror", "-fsanitize=address,undefined", str(source), "-o", str(binary)], check=True)
            subprocess.run([str(binary)], check=True)
