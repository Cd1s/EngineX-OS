import importlib.util
import sys
import unittest
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
BUILD_PACKS = REPO / "tools/ui-mod/build_packs.py"
sys.path.insert(0, str(REPO / "tools/az01-packer"))
import legacy_names

LAB = Path(legacy_names.LAB)
spec = importlib.util.spec_from_file_location("build_packs_issue314", BUILD_PACKS)
bp = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bp)


class TrackMetadataEditTest(unittest.TestCase):
    def test_track_details_patch_uses_stock_text_dialog_and_deck_writeback(self):
        rel = "Skins/Embedded/Items/TrackInfo.qml"
        bases = [(name, LAB / sub / "qrc") for name, sub in (
            ("5.3.0", "5.3.0"),
            ("5.3.0-sclive4", "5.3.0-sclive4"),
            ("5.3.0-az04", "5.3.0-az04"),
        ) if (LAB / sub / "qrc" / rel).is_file()]
        if not bases:
            self.skipTest("5.3.0 UI bases are not available")
        for name, qrc in bases:
            with self.subTest(base=name):
                stock = (qrc / rel).read_text(encoding="utf-8")
                old_root = bp.ROOT
                try:
                    bp.ROOT = "Skins/Embedded/"
                    patched = stock
                    for old, new in bp.edits(False).get(rel, []):
                        patched = bp.patch(patched, old, new, rel)
                finally:
                    bp.ROOT = old_root
                self.assertIn("TextInputDialog", patched)
                self.assertIn("/v1/enginex/track-metadata", patched)
                self.assertIn("TrackInfo_EditGenre", patched)
                self.assertIn("TrackInfo_EditLabel", patched)
                self.assertIn("TrackInfo_EditComment", patched)
                self.assertIn("TrackUri", patched)
                self.assertIn("TrackNetworkPath", patched)
                self.assertIn("stockEdit", patched)
                self.assertIn("/Track/${field}", patched)


if __name__ == "__main__":
    unittest.main()
