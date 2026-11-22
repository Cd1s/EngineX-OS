"""Button mapping: the EngineX actions' two halves (the assignment's Action property, the UI pack's reaction)."""
import json
import legacy_names as legacy
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import unittest

REPO = Path(__file__).resolve().parents[2]
QRC = Path(legacy.LAB) / '5.3.0/qrc'


class ButtonMappingActionsTest(unittest.TestCase):
    def test_the_actions_the_assignment_writes_are_the_ones_the_ui_pack_reacts_to(self):
        self.assertIsNotNone(shutil.which('node'), 'Node is required for the fixture')
        settings = (REPO / 'pkg/webui/ui_settings.go').read_text()
        waveforms = re.search(r'uiWaveforms\s*=\s*\[\]string\{([^}]*)\}', settings).group(1)
        with tempfile.TemporaryDirectory() as tmp:
            packs = Path(tmp) / 'packs'
            built = subprocess.run(['python3', str(REPO / 'tools/ui-mod/build_packs.py'), '--qrc', str(QRC), '--out', str(packs)],
                                   capture_output=True, text=True)
            self.assertEqual(built.returncode, 0, built.stderr)
            for lang in ('en', 'zh-Hans'):
                with self.subTest(lang=lang):
                    source = {
                        'main': (packs / lang / 'Skins/Embedded/EmbeddedMain.qml').read_text(),
                        'actionOutput': (QRC / 'airAssignments/InputAssignment/ActionOutput.qml').read_text(),
                        'combo': (QRC / 'airAssignments/InputAssignment/ComboActionOutput.qml').read_text(),
                        'notifier': (QRC / 'Skins/Common/Items/ActionTranslatorStateChangeNotifier.qml').read_text(),
                        'catalogue': json.loads((REPO / 'pkg/mapping/catalogue.json').read_text()),
                        'uiWaveforms': re.findall(r'"([^"]+)"', waveforms),
                        'settingsKeys': re.findall(r'case "([a-z_]+)":', settings),
                    }
                    result = subprocess.run(['node', str(Path(__file__).with_name('button-mapping-fixture.js'))],
                                            input=json.dumps(source), capture_output=True, text=True)
                    self.assertEqual(result.returncode, 0, result.stdout + result.stderr)


if __name__ == '__main__':
    unittest.main()
