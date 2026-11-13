import importlib.util
from pathlib import Path
import subprocess
import tempfile
import unittest

REPO = Path(__file__).resolve().parent.parent.parent
BUILD_PACKS = REPO / "tools" / "ui-mod" / "build_packs.py"
LAB = Path("/root/build/sc5000-lab")


def load_build_packs():
    spec = importlib.util.spec_from_file_location("build_packs", BUILD_PACKS)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


class UiModBH2Test(unittest.TestCase):
    def test_connected_accounts_dialog_suppressed_for_custom_services_bh2_a6_2(self):
        bp = load_build_packs()
        for root in ("Skins/Embedded/", "Skins/EmbeddedCommon/"):
            bp.ROOT = root
            edits = bp.edits(False)
            key = root + "Views/Streaming/StreamingConnectedAccountsDialog.qml"
            self.assertTrue(key in edits, key)
            joined = "\n".join(new for _old, new in edits[key])
            self.assertIn("root.scQuiet() || root.scUnasked(provider)", joined)

    def test_composite_artist_song_has_skip_keys_for_primego_mixstream_bh2_a6_3(self):
        bp = load_build_packs()
        bp.ROOT = "Skins/Embedded/"
        edits = bp.edits(False)
        key = "Skins/EngineOS/UI/CompositeControls/Deck/DeckItem/ArtistSong.qml"
        joined = "\n".join(new for _old, new in edits[key])
        self.assertIn("id: scSkipKeys", joined)
        self.assertIn("scNoSkipKeys", joined)
        self.assertIn("PreviousTrack", joined)
        self.assertIn("NextTrack", joined)
        self.assertIn("eventPoint.position.x >= scSkipKeys.x", joined)


if __name__ == "__main__":
    unittest.main()
