"""#99: exercise the generated hardware handlers and five saved views together."""
import json
import legacy_names as legacy
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

REPO = Path(__file__).resolve().parents[2]
QRC = Path(legacy.LAB) / '5.3.0/qrc'


class ShiftViewTest(unittest.TestCase):
    def test_hardware_cycle_in_every_board_policy_and_restart(self):
        self.assertIsNotNone(shutil.which('node'), 'Node is required for the view fixture')
        with tempfile.TemporaryDirectory() as tmp:
            packs = Path(tmp) / 'packs'
            built = subprocess.run([
                'python3', str(REPO / 'tools/ui-mod/build_packs.py'),
                '--qrc', str(QRC), '--out', str(packs),
            ], capture_output=True, text=True)
            self.assertEqual(built.returncode, 0, built.stderr)
            modules = 'ControlSurfaceModules/ControlSurfaceModules/'
            for lang in ('en', 'zh-Hans', 'zh-Hant', 'es'):
                with self.subTest(lang=lang):
                    root = packs / lang
                    source = {
                        'main': (root / 'Skins/Embedded/EmbeddedMain.qml').read_text(),
                        'view': (root / modules / 'View.qml').read_text()
                        if (root / modules / 'View.qml').exists() else (QRC / modules / 'View.qml').read_text(),
                        'navigation': (root / modules / 'Navigation.qml').read_text()
                        if (root / modules / 'Navigation.qml').exists() else (QRC / modules / 'Navigation.qml').read_text(),
                        'combo': (QRC / 'airAssignments/InputAssignment/ComboActionOutput.qml').read_text(),
                    }
                    result = subprocess.run([
                        'node', str(Path(__file__).with_name('shift-view-fixture.js')),
                    ], input=json.dumps(source), capture_output=True, text=True)
                    self.assertEqual(result.returncode, 0, result.stdout + result.stderr)


if __name__ == '__main__':
    unittest.main()
