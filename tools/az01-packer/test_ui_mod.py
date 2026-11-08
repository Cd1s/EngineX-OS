import importlib.util
import json
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

    def test_cue_info_cached_and_master_not_fought(self):
        self.assertIn("property var scCueInfoCache", bp.SERVICES_MAIN)
        self.assertIn("c.text === text && c.label === label && c.color === color && c.urgent === urgent", bp.SERVICES_MAIN)
        self.assertNotIn("onTriggered: root.scCueTick", bp.SERVICES_MAIN)
        self.assertIn("if ((!e.link || !root.scPlayer) && e.master && !at.master) at.wantMaster", bp.SERVICES_MAIN)

    def test_shadow_throttles(self):
        self.assertIn("interval: 40; repeat: true", bp.SERVICES_MAIN)
        self.assertIn("r.ver === undefined ? 120 : 60", bp.SERVICES_MAIN)
        self.assertIn("now - scShadowPostAt >= 250", bp.SERVICES_MAIN)
        self.assertIn("scShadowCueCache", bp.SERVICES_MAIN)
        self.assertIn('dbg: ""', bp.SERVICES_MAIN)
        self.assertIn("title: s.net", bp.SERVICES_MAIN)
        self.assertIn("artist: s.net", bp.SERVICES_MAIN)

    def test_shadow_exact_pitch_pll_and_debug(self):
        m = bp.SERVICES_MAIN
        self.assertIn('(g(d, "/CurrentBPM") || {}).unnormalized', m)
        self.assertIn("Math.abs(exact - shown) < 0.15 ? exact : shown", m)
        self.assertIn("(p.playing ? 180 : 5)", m)
        self.assertIn("currentBpm: s.currentBpm", m)
        self.assertIn("const wantSync = !!e.sync && (!e.link || !root.scPlayer)", m)
        self.assertIn("function scShadowSpeed(d, pitch)", m)
        self.assertIn("at.sd * 0.85 + drift * 0.15", m)
        self.assertIn("at.postPlay = now + 380", m)
        self.assertIn("e.seq === at.lastSeq", m)
        self.assertIn('x.open("POST", "http://127.0.0.1:8080/v1/sc5000/debug/shadow")', m)
        self.assertIn("root.scShadowDebug = !!r.debug", m)
        self.assertIn("LoadOverlay/LoadOverlayDecks.qml", SRC)
        # Standalone EngineX peers share Sync/master; consoles (#160) still need shadow control
        self.assertIn("if ((!e.link || !root.scPlayer) && wantSync !== synced", m)
        self.assertIn('if ((!at.link || !root.scPlayer) && (root.scTr("/Engine/Deck" + d + "/DeckIsMaster")', m)

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
        # smooth gradient: 25 fps in every mode, hue advanced by elapsed time
        self.assertIn("interval: 40\n\t\trepeat: true", bp.SERVICES_MAIN)
        self.assertIn("scJogLastTick", bp.SERVICES_MAIN)
        self.assertNotIn("< 0.015) return", bp.SERVICES_MAIN)
        # #157: holding the Browse encoder must not toggle Grid Edit
        self.assertIn("BrowseEncoder.qml", SRC)

    def test_cue_countdown_151(self):
        self.assertIn("function scNextCueInfo(d, tick)", bp.SERVICES_MAIN)
        self.assertNotIn('" BARS"', bp.SERVICES_MAIN)
        self.assertIn('String(bars) + "." + ((Math.floor(totalBeats) % 4) + 1)', bp.SERVICES_MAIN)
        cap = bp.cue_capsule("", "X", 30, 20)
        self.assertIn('objectName: "EngineX_CueCountdown"', cap)
        self.assertIn("visible: info !== null && root.scCueCountdown", cap)
        self.assertIn("Style.surface03Color", cap)
        self.assertNotIn("border", cap)
        self.assertEqual(cap.count("{"), cap.count("}"))

    def test_cue_countdown_toggle_only_in_view(self):
        self.assertIn('["scCueCountdown", "cueCountdown"]', SRC)
        self.assertIn('root.scCall("cueCountdown=" + index)', bp.CUE_COUNTDOWN_ENTRY)
        self.assertIn(bp.CUE_COUNTDOWN_ENTRY, bp.VIEW_ENTRIES)
        for dup in (bp.UI_SCALE_ENTRY, bp.JOG_ENTRY, bp.FOUR_DECK_ENTRIES, bp.DUAL_COLUMN_ENTRY, bp.BEAT_JUMP_ENTRIES):
            self.assertIn(dup, bp.VIEW_ENTRIES)
            self.assertNotIn(dup, bp.LIBRARY_ENTRIES)
        self.assertNotIn("LANGUAGE_ENTRY + UI_SCALE_ENTRY", SRC)
        self.assertNotIn('("CuesLoops", "PausedSavedLoopBehavior"', SRC)
        rows = [l.split("\t") for l in (REPO / "tools/ui-mod/i18n.tsv").read_text().splitlines()]
        self.assertIn(["Cue Countdown", "热标拍数倒数", "熱標拍數倒數"], rows)

    def test_deck_list_follows_four_deck_role(self):
        self.assertIn('root.scFourDeckRole.split("").map(c => parseInt(c))', bp.SERVICES_MAIN)
        self.assertEqual(SRC.count("root.decks && root.decks.length ? root.decks : [1, 2]"), 1)  # only inside scDeckList
        self.assertIn("function scJogResetDecks()", bp.SERVICES_MAIN)

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
            # Exercise the actual generated packs too, not only the render helper.
            for lang in ("en", "zh-Hans", "zh-Hant"):
                main = Path(tmp) / "packs" / lang / "Skins/Embedded/EmbeddedMain.qml"
                text = main.read_text()
                dictionaries = json.loads(re.search(r"readonly property var scDicts: \((.+)\)", text).group(1))
                self.assertTrue(all(isinstance(d, str) for d in dictionaries.values()))
                entries = len(json.loads(dictionaries[lang]))
                old_total = sum(len(json.loads(d)) for d in dictionaries.values())
                self.assertEqual(entries * 3, old_total)


if __name__ == "__main__":
    unittest.main()
