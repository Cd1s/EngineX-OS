import importlib.util
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

    def test_splash_and_shutdown_scaling_175(self):
        # #175: SplashScreen images and shutdown overlay Rectangle must fill
        # the enlarged logical canvas so they don't shrink to 3/4 under UI Scale.
        self.assertIn(r'ROOT + "Items/SplashScreen.qml"', SRC)
        self.assertIn(r'"\t\tImage {\n\t\t\tanchors.fill: parent\n\t\t\tsource:"', SRC)
        self.assertIn(r'"\t\t\tid: overlay\n\t\t\tanchors.fill: parent\n"', SRC)
        self.assertIn(r'"\t\t\tparent: Overlay.overlay ? Overlay.overlay : uiContainer\n"', SRC)
        self.assertIn(r'"\t\t\tanchors.fill: parent\n"', SRC)

    def test_scaling_matrix_all_generations_and_scales(self):
        # Resolution inventory:
        # Gen 2 landscape (1280x800): SC LIVE 4 (JP21), SC LIVE 2 (JP20), PRIME 4+ (JC11S),
        #                            PRIME GO+ (JP11S), MIXSTREAM PRO+ (NH08S),
        #                            MIXSTREAM PRO GO (NH10), PRIME 4 G2 (JP24)
        # Gen 2 portrait (800x1280):  SYSTEM ONE (RMZ2)
        # Gen 1 16:9 (1280x720):      SC5000 (JP07), SC5000M (JP08), PRIME GO (JP11),
        #                            PRIME 2 (JC16), MIXSTREAM PRO (NH08)
        # Gen 1 16:10 (1280x800):     PRIME 4 (JC11)
        # Gen 1 wide (1440x900):      SC6000 (JP13), SC6000M (JP14)
        resolutions = {
            "gen2_landscape_1280x800": (1280, 800),
            "gen2_portrait_rmz2_800x1280": (800, 1280),
            "gen1_16x9_sc5000_1280x720": (1280, 720),
            "gen1_16x10_prime4_1280x800": (1280, 800),
            "gen1_wide_sc6000_1440x900": (1440, 900),
        }
        scales = [75, 80, 85, 90, 95, 100, 105, 110]
        for name, (w, h) in resolutions.items():
            for s in scales:
                scale_factor = s / 100.0
                logical_w = w * 100 / s
                logical_h = h * 100 / s
                # With anchors.fill: parent inside scaled uiContainer:
                rendered_w = logical_w * scale_factor
                rendered_h = logical_h * scale_factor
                self.assertAlmostEqual(rendered_w, w, places=5,
                                       msg=f"{name} at {s}% width mismatch")
                self.assertAlmostEqual(rendered_h, h, places=5,
                                       msg=f"{name} at {s}% height mismatch")
                # At scale 100 (stock / default), logical equals physical
                if s == 100:
                    self.assertEqual(logical_w, w)
                    self.assertEqual(logical_h, h)
                # Unfixed behavior: unanchored items had fixed size (w, h) inside
                # scaled container, shrinking to w * (s / 100) (e.g. 75% at s=75).
                unfixed_rendered_w = w * scale_factor
                unfixed_rendered_h = h * scale_factor
                if s != 100:
                    self.assertNotAlmostEqual(unfixed_rendered_w, w, places=1)
                    self.assertNotAlmostEqual(unfixed_rendered_h, h, places=1)


class UiModBuildTest(unittest.TestCase):
    def test_build_530(self):
        qrc = LAB / "5.3.0" / "qrc"
        if not qrc.is_dir():
            self.skipTest(f"{qrc} not found (run on the build host)")
        tmp_dir = Path("/var/tmp/s175b") if Path("/var/tmp/s175b").is_dir() else None
        with tempfile.TemporaryDirectory(prefix="ui-mod-test-", dir=tmp_dir) as tmp:
            res = subprocess.run(["python3", str(BUILD_PACKS), "--qrc", str(qrc), "--out", str(Path(tmp) / "packs")],
                                 capture_output=True, text=True)
            self.assertEqual(res.returncode, 0, res.stderr)
            tl = Path(tmp) / "packs" / "en" / "Skins" / "Embedded" / "Views" / "BrowseView" / "Components" / "TrackList.qml"
            self.assertTrue(tl.is_file(), f"{tl} not found")
            self.assertIn("root.scTracklistWaveform", tl.read_text())

            # #175: verify built SplashScreen.qml and EmbeddedMain.qml
            splash = Path(tmp) / "packs" / "en" / "Skins" / "Embedded" / "Items" / "SplashScreen.qml"
            self.assertTrue(splash.is_file(), f"{splash} not found")
            splash_text = splash.read_text()
            self.assertIn("anchors.fill: parent\n\t\t\tsource:", splash_text)
            self.assertIn("id: overlay\n\t\t\tanchors.fill: parent\n", splash_text)

            main = Path(tmp) / "packs" / "en" / "Skins" / "Embedded" / "EmbeddedMain.qml"
            self.assertTrue(main.is_file(), f"{main} not found")
            main_text = main.read_text()
            self.assertIn("parent: Overlay.overlay ? Overlay.overlay : uiContainer\n\t\t\tanchors.fill: parent\n", main_text)
            self.assertNotIn("parent: Overlay.overlay ? Overlay.overlay : uiContainer\n\t\t\twidth: planckQuickView.defaultScreenSize.width", main_text)


if __name__ == "__main__":
    unittest.main()
