import importlib.util
import re
import subprocess
import unittest
from pathlib import Path


class RekordboxRowValuesTest(unittest.TestCase):
    def test_unloaded_and_analyzed_rows_prefer_rekordbox_values(self):
        path = Path(__file__).resolve().parents[1] / "ui-mod/build_packs.py"
        spec = importlib.util.spec_from_file_location("rbx_build_packs", path)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        functions = []
        for name in ("scPreview", "scRowValue"):
            match = re.search(r"(function " + name + r"\([^\n]*\) \{.*?\n\t\})", module.SERVICES_MAIN, re.S)
            self.assertIsNotNone(match, name)
            functions.append(match.group(1))
        script = "\n".join(functions) + r'''
const assert = require('assert');
const row = {providerName:'rekordbox [123.45 BPM 8A]', bpm:'', key:''};
assert.strictEqual(scRowValue(row,'bpm',1),'123.45');
assert.strictEqual(scRowValue(row,'key',2),'8A');
row.bpm = 98; row.key = '5B';
assert.strictEqual(scRowValue(row,'bpm',1),'123.45');
assert.strictEqual(scRowValue(row,'key',2),'8A');
row.providerName = 'Deezer [123 BPM 8A]';
assert.strictEqual(scRowValue(row,'bpm',1),98);
assert.strictEqual(scRowValue(row,'key',2),'5B');
'''
        result = subprocess.run(["node", "-e", script], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        source = path.read_text()
        for field, column in (("bpm", 1), ("key", 2)):
            self.assertIn(f'root.scRowValue(row.modelData, "{field}", {column})', source)
