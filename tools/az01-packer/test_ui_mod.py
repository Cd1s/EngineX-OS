import importlib.util
import re
import subprocess
import tempfile
import unittest
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent.parent
BUILD_PACKS = REPO / "tools" / "ui-mod" / "build_packs.py"
LAB = Path("/root/build/sc5000-lab")

spec = importlib.util.spec_from_file_location("build_packs", BUILD_PACKS)
bp = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bp)
SRC = BUILD_PACKS.read_text()


class UiModSourceTest(unittest.TestCase):
    def test_translator_cache_replaces_shadow_lookups(self):
        self.assertIn("function scTr(path)", bp.SERVICES_MAIN)
        a = bp.SERVICES_MAIN.index("function scPress(")
        b = bp.SERVICES_MAIN.index("function scRestartCall")
        # scShadowLink is dead code now (no Register subscription), the rest must use the cache
        body = bp.SERVICES_MAIN[a:b]
        k = body.index("property var scShadowLinks")
        l = body.index("function scShadowLinkEdge")
        self.assertNotIn("Planck.getProperty", body[:k] + body[l:])

    def test_shadow_throttles(self):
        self.assertIn("interval: 40; repeat: true", bp.SERVICES_MAIN)
        self.assertIn("r.ver === undefined ? 120 : 25", bp.SERVICES_MAIN)
        self.assertIn("scShadowCueCache", bp.SERVICES_MAIN)
        self.assertIn('dbg: ""', bp.SERVICES_MAIN)
        self.assertIn("title: s.net", bp.SERVICES_MAIN)
        self.assertIn("artist: s.net", bp.SERVICES_MAIN)

    def test_shadow_unload_and_no_register_subscription(self):
        m = bp.SERVICES_MAIN
        self.assertNotIn("scShadowLink(d, at.link)", m)
        self.assertIn("now - at.emptyAt < 350", m)
        self.assertIn('"/Track/Unload"', m)
        self.assertIn("/RequestMaster", m)
        self.assertIn("e.loading", m)

    def test_zoom_is_ten_geometric_levels(self):
        self.assertIn("function scZoom(dir)", bp.ZOOM_STEP)  # tests/test_rootfs_overlay.sh greps this
        self.assertIn("Planck.getProperty(\"/Engine/Master/WaveformZoom\")", bp.ZOOM_STEP)
        self.assertIn("Math.pow(hi / lo, 1 / 9)", bp.ZOOM_STEP)
        self.assertIn("interval: 16", bp.ZOOM_STEP)

    def test_swipe_left_has_one_stems(self):
        self.assertEqual(SRC.count("!leftActions.some(action => /stem/i.test("), 2)
        self.assertEqual(SRC.count("/^stems?$/i.test("), 4)
        self.assertIn("/stem/i.test(String(leftActions[actionIndex]", SRC)

    def test_auto_loop_knob_and_link(self):
        self.assertIn("interval: 10000; running: true", SRC)
        self.assertIn("module.loopInactiveShiftTurnAction === Action.BeatJump || ", SRC)
        self.assertIn("LinkBeatJumpLoopSize", bp.SERVICES_MAIN)

    def test_jog_timer_slower(self):
        self.assertIn(": 250\n\t\trepeat: true", bp.SERVICES_MAIN)

    def test_cue_countdown_151(self):
        self.assertIn("function scNextCueInfo(d, tick)", bp.SERVICES_MAIN)
        self.assertNotIn('" BARS"', bp.SERVICES_MAIN)
        self.assertIn('String(bars) + "." + ((Math.floor(totalBeats) % 4) + 1),', bp.SERVICES_MAIN)
        cap = bp.cue_capsule("", "X", 30, 20)
        self.assertIn('objectName: "EngineX_CueCountdown"', cap)
        self.assertIn("visible: info !== null", cap)
        self.assertIn("Style.surface03Color", cap)
        self.assertNotIn("border", cap)
        self.assertEqual(cap.count("{"), cap.count("}"))

    def test_i18n_has_stem(self):
        rows = [l.split("\t") for l in (REPO / "tools/ui-mod/i18n.tsv").read_text().splitlines()]
        self.assertIn(["Stem", "分轨 Stems", "分軌 Stems"], rows)
        self.assertIn(["Tracklist Waveform", "曲目列表波形", "曲目列表波形"], rows)

    def test_tracklist_waveform_and_ui_scale(self):
        self.assertIn('text: "Tracklist Waveform"', bp.LIBRARY_ENTRIES)
        self.assertIn("root.scTracklistWaveform", bp.LIBRARY_ENTRIES)
        self.assertIn("tracklistWaveform=", bp.LIBRARY_ENTRIES)
        self.assertIn('"110", "105", "100"', bp.UI_SCALE_ENTRY)
        self.assertIn('"110%", "105%", "100%"', bp.UI_SCALE_ENTRY)
        self.assertIn("root.scTracklistWaveform", SRC)

    def test_stems_eq_and_grid_edit_guard_and_four_deck_roles(self):
        # #152: Stems EQ Mode syncs ChannelStrip/Mode, unlocks Shift+PFL, and latches Hot Cue double-tap
        self.assertIn('"/Engine/Mixer/Channel" + ch + "/ChannelStrip/Mode"', bp.SERVICES_MAIN)
        self.assertIn("Config.audio.hasStemsLevel || module.pflShiftAction == Action.ToggleChannelStripMode", SRC)
        self.assertIn("scStemsLatchedAt", SRC)
        # #157: holding Browse/Zoom encoder while playing must not enter BeatGrid Edit
        self.assertIn("if (!deckConfig.songLoaded || (!deckConfig.gridEditActive && deckConfig.playing))", SRC)
        self.assertIn("!deckConfig.gridEditActive &&\\n", SRC)
        # #160: 4-Deck Waveform and 3+4 role available across players and all-in-one consoles
        self.assertIn('"34"', bp.FOUR_DECK_ENTRIES)
        self.assertIn('"3+4"', bp.FOUR_DECK_ENTRIES)
        self.assertNotIn("visible: root.scPlayer", bp.FOUR_DECK_ENTRIES)
        self.assertIn("scFourDeckRole = v.fourDeckWaveform ? (v.fourDeckRoleActive || \"\") : \"\"", bp.SERVICES_MAIN)


class UiModBuildTest(unittest.TestCase):
    def test_build_530(self):
        qrc = LAB / "5.3.0" / "qrc"
        if not qrc.is_dir():
            self.skipTest(f"{qrc} not found (run on Oracle-Batam-arm)")
        with tempfile.TemporaryDirectory(prefix="ui-mod-test-") as tmp:
            res = subprocess.run(["python3", str(BUILD_PACKS), "--qrc", str(qrc), "--out", str(Path(tmp) / "packs")],
                                 capture_output=True, text=True)
            self.assertEqual(res.returncode, 0, res.stderr)
            tl = Path(tmp) / "packs" / "en" / "Skins" / "Embedded" / "Views" / "BrowseView" / "Components" / "TrackList.qml"
            self.assertTrue(tl.is_file(), f"{tl} not found")
            self.assertIn("root.scTracklistWaveform", tl.read_text())
            for lang in ("en", "zh-Hans", "zh-Hant"):
                selector = (Path(tmp) / "packs" / lang / "Skins/Embedded/Views/MixerMenu/Pages/ChannelsPageItems/InputSelect.qml").read_text()
                indexes = re.search(r"readonly property var scInputIndexes: (\{.*?\n    \})", selector, re.S)
                self.assertIsNotNone(indexes)
                # Run the JavaScript from the generated QML: JP21's visible
                # option 1 must map to native index 3; other boards stay whole.
                script = "const assert=require('node:assert/strict'); const Config={device:{name:'JP21'}};\n"
                script += "function indexes(controller) " + indexes.group(1) + "\n"
                script += """
const four={inputs:['Engine','Bluetooth','Computer','Line In']};
assert.deepEqual(indexes(four),[0,3]);
assert.equal(four.inputs[indexes(four)[1]],'Line In');
assert.deepEqual(indexes({inputs:['Engine','Bluetooth','Computer']}),[0]);
for(const board of ['JP20','JP22','JP24','JC11S','JP21X']) {
    Config.device.name=board;
    assert.deepEqual(indexes(four),[0,1,2,3]);
}
"""
                mapping = subprocess.run(["node", "-e", script], capture_output=True, text=True)
                self.assertEqual(mapping.returncode, 0, mapping.stderr)


if __name__ == "__main__":
    unittest.main()
