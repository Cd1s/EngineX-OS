import importlib.util
import json
import re
import shutil
import subprocess
import tempfile
import tarfile
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
    def test_px_selector_maps_before_selecting_stock_carrier(self):
        for name in ("Spiral", "Pitch Echo", "Slip Roll", "Dub Echo", "Space"):
            self.assertIn(name + " (PX)", bp.FX_CONTROLLER)
        qml = bp.FX_CONTROLLER
        selection = qml.split("function scFxAccept(index) {", 1)[1].split("property var scFxMix", 1)[0]
        self.assertLess(selection.index('root.scCall("fxUnit="'), selection.index('djFxView.selectEffect(mode ?'))
        self.assertLess(selection.index("function(reply)"), selection.index('djFxView.selectEffect(mode ?'))
        self.assertIn('scFxStock.indexOf("Flanger")', qml)
        self.assertIn('index - scFxStock.length + 1 : 0', qml)
        self.assertIn('Planck.createProperty(scFxBus + "Plus", false, 3)', qml)
        self.assertIn('ActionTranslatorStateChangeNotifier', qml)
        self.assertIn('scFxReady.translator.state = false', qml)
        self.assertIn('return scFxCanUse() ? px : fxConfig.channel[modelData].pEffectIndex.translator', bp.FX_JOG_TARGET)
        self.assertIn('visible: !root.scFxMasterBypassed', bp.PLAYBACK_ENTRIES)
        self.assertIn('scFxSelectorAvailable = scFxMasterBypassed && (Config.audio.fx.hasDJFx || Config.audio.fx.hasNHFx)', SRC)

    def test_auto_analyze_cannot_submit_native_work(self):
        # Saved-on settings, drive insertion, playback and arbitrary filesystem
        # contents must all have the same result: zero automatic submissions.
        # For this fail-closed implementation, reject any native Analyzer path
        # in the injected main/entry code, including batch and per-track calls.
        for qml in (bp.SERVICES_MAIN, bp.LIBRARY_ENTRIES):
            self.assertNotIn("/Client/Librarian/Analyzer/", qml)
            self.assertNotIn("scAutoAnalyzeTimer", qml)
            self.assertNotIn("onScAutoAnalyzeChanged", qml)
            self.assertNotIn("onScDriveCountChanged", qml)
        self.assertIn("readonly property bool scAutoAnalyzeAvailable: false", bp.SERVICES_MAIN)

    def test_auto_analyze_preserves_preference_and_labels_unavailability(self):
        self.assertIn("property bool scAutoAnalyze: false", bp.SERVICES_MAIN)
        self.assertIn("scAutoAnalyze = !!v.autoAnalyze", bp.SERVICES_MAIN)
        self.assertIn('values: ["Off", "On"]', bp.LIBRARY_ENTRIES)
        self.assertIn('currentValue: values[root.scAutoAnalyze ? 1 : 0]', bp.LIBRARY_ENTRIES)
        self.assertIn('root.scCall("autoAnalyze=" + index)', bp.LIBRARY_ENTRIES)
        label = "Auto-Analyze Drives (Unavailable)"
        self.assertIn('text: "' + label + '"', bp.LIBRARY_ENTRIES)
        translations = bp.load_dict()[label]
        self.assertIn("暂不可用", translations["zh-Hans"])
        self.assertIn("暫不可用", translations["zh-Hant"])

    def test_auto_analyze_gate_requires_fail_closed_capability(self):
        probe = (BUILD_PACKS.parent.parent / "az01-packer/gate-ui-probe.qml").read_text()
        gate = (BUILD_PACKS.parent.parent / "az01-packer/release-gate.sh").read_text()
        self.assertIn("main.scAutoAnalyzeAvailable", probe)
        self.assertNotIn("/Client/Preferences/AutoAnalysis", probe)
        self.assertIn("SCPROBE analyze|boolean|false|", gate)
        self.assertNotIn("SCPROBE analyze|boolean|1|", gate)

    def test_shadow_layer_uses_stock_owner_outside_layouts(self):
        for source, owner in (("RowLayout {\n    id: deckInfo\n}\n", "deckInfo"),
                              ("Item {\n    width: 800\n}\n", "scStockShadowHost")):
            layer = bp.shadow_layer("import QtQuick 2.0\n" + source, "track")
            self.assertIn("import QtQuick 2.15",layer)
            self.assertIn("readonly property Item owner: " + owner, layer)
            self.assertIn("parent: layoutParent()", layer)
            self.assertNotIn("parent: root.transformed", layer)
            self.assertNotIn("z: 10000", layer)
            self.assertNotIn("scShadowFrame", bp.SHADOW_LAYER)
            self.assertNotIn("@OWNER@", layer)
            self.assertEqual(layer.count("{"), layer.count("}"))

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
        self.assertIn("now - scShadowPostAt >= 500", bp.SERVICES_MAIN)
        self.assertIn("scShadowCueCache", bp.SERVICES_MAIN)
        self.assertIn('o.dbg = ""', bp.SERVICES_MAIN)
        self.assertIn("o.title = s.net", bp.SERVICES_MAIN)
        self.assertIn("o.artist = s.net", bp.SERVICES_MAIN)

    def test_shadow_exact_pitch_pll_and_debug(self):
        m = bp.SERVICES_MAIN
        self.assertIn('cb = g.current.unnormalized || 0', m)
        self.assertIn("Math.abs(exact - shown) < 0.15 ? exact : shown", m)
        self.assertIn("(p.playing ? 180 : 5)", m)
        self.assertIn("o.currentBpm = s.currentBpm", m)
        self.assertIn("const wantSync = !!e.sync && (!e.link || !root.scPlayer)", m)
        self.assertIn("function scShadowSpeed(d, pitch, want)", m)
        self.assertIn("at.sd * 0.85 + drift * 0.15", m)
        self.assertNotIn("at.postPlay", m)  # the gap after Play is closed by scShadowBend, not a second seek
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

    def test_zoom_and_browse_encoder_are_stock(self):
        # The maintainer wants the stock ZOOM / Browse knob: 0.5 s per detent, hold = Grid Edit,
        # Shift+hold = Cue Edit, double press = Instant Double, also while playing (#147, #157 undone).
        for gone in ("/GUI/Scripted/Zoom", "scZoom(", "BrowseEncoder.qml", "DeckAction.qml",
                     "!deckConfig.gridEditActive && deckConfig.playing"):
            self.assertNotIn(gone, SRC)
        self.assertIn("Planck.getProperty(\"/Engine/Master/WaveformZoom\")", bp.ZOOM_PROP)  # the shadow lane reads it

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

@unittest.skipUnless(shutil.which("node"), "node needed for the display-state behavioral checks")
class ShadowDisplayTest(unittest.TestCase):
    """Run the actual QML JS functions with a fake clock and an Engine that rejects all access."""

    def js(self, body):
        names = ("scDeckLocked", "scShadowVisible", "scShadowDeckFor", "scShadowData", "scShadowRate", "scShadowPosition",
                 "scShadowBeatIndex", "scShadowBeatAt", "scShadowGrid", "scShadowNextCue", "scShadowCuesFor", "scShadowAsset",
                 "scShadowProperties", "scShadowTick", "scShadowPost", "scShadowPoll", "scShadowDisplayFetch", "scShadowSameSample", "scShadowApplyDisplay", "scShadowApplyAll", "scNextCueInfo", "scNextCueCompute", "scCueTickFor",
                 "scShadowApply", "scShadowEmpty", "scShadowSpeed", "scShadowCues", "scShadowBeat",
                 "scShadowLoad", "scShadowWrong", "scShadowHolds", "scShadowEndGap", "scShadowEnd",
                 "scShadowSeek", "scShadowLink", "scShadowLinkEdge", "scPress", "scGcCheck", "scShadowWake", "scShadowAnyPlaying",
                 "scShadowLine", "scShadowBend", "scShadowBendEnd", "scShadowMasterBack")
        functions = []
        for name in names:
            start = re.search(r"(?m)^\tfunction " + name + r"\([^\n]*\) \{", bp.SERVICES_MAIN)
            self.assertIsNotNone(start, name)
            line_end = bp.SERVICES_MAIN.index("\n", start.start())
            if bp.SERVICES_MAIN[start.start():line_end].rstrip().endswith("}"):
                end = line_end
            else:
                end = bp.SERVICES_MAIN.index("\n\t}", start.end()) + 3
            functions.append(bp.SERVICES_MAIN[start.start():end])
        setup = r"""
const assert = require('assert');
let now = 10000;
Date.now = () => now;
globalThis.root = globalThis;
Object.assign(globalThis, {
    scFourDeckRole: '13', scPlayer: true, scShadowDisplayOnly: true,
    scShadowExpiryMs: 3000, scShadowAt: {}, scShadowDisplay: {}, scShadowEmptyArray: [],
    scShadowDisplayRequests: {}, scShadowDisplayText: {}, scShadowGridCache: {}, scShadowLiveCueCache: {},
    scShadowCdjMaster: false, scPressQueue: [], scShadowEmptyObject: {}, scShadowHead: {},
    scShadowMineRole: "?", scShadowMine: [], scShadowSampleSpare: {}, scShadowDeckProperties: {}, scShadowOwn: [],
    scShadowPostRequest: null, scShadowPollRequest: null, scShadowResponseVersion: "", scShadowPostPending: false,
    scShadowLinkId: '', scShadowLinkAt: -Infinity, scShadowTextAt: {},
    scShadowSent: [], scShadowFullAt: 0, scShadowPostFull: true, scShadowLive: {seen: 0},
    scShadowTickTimer: {interval: 40}, scShadowBusyAt: 0, scShadowRoleDirty: true,
    scGcLoadSeq: 0, scGcRuns: 0, scGcLastMs: 0, scGcProps: null,
    scGcState: {armed: true, quietSince: 0, lastRun: -Infinity, loadSig: 0, loadSeq: 0, loadAt: 0, followAt: 0},
    scShadowPrev: {}, scShadowCueCache: {}, scShadowPostAt: 0, scShadowPollAt: now, scShadowDebug: false,
    scShadowLead: 0, scShadowPeerLead: 0, scShadowPeerPlaying: false, scShadowMasterAt: 0,
    scShadowGen: 0, scShadowVer: 0, scShadowFrame: 17, scCueInfoCache: {}, scCueInfoMemo: {},
    scTr() { throw Error('Engine access forbidden'); },
    Planck: { getProperty() { throw Error('Engine access forbidden'); } }
});
const requests = [], legacy = [];
globalThis.XMLHttpRequest = class {
    constructor() { requests.push(this); }
    open(method, url) { this.url = url; assert.equal(method, 'GET'); }
    send() {}
    finish(status, data) {
        this.status = status; this.responseText = JSON.stringify(data); this.readyState = 4;
        this.onreadystatechange();
    }
};
"""
        res = subprocess.run(["node", "-e", setup + "\n".join(functions) + "\n" + body], capture_output=True, text=True)
        self.assertEqual(res.returncode, 0, res.stderr)

    def test_same_track_line_state_changes_are_applied(self):
        self.js(r"""
scPlayer = false; scFourDeckRole = '12';
scShadowApplyAll([{deck:3,key:'cdj:same',lineIn:false}]);
assert.equal(scShadowData(3).lineIn, false);
scShadowApplyAll([{deck:3,key:'cdj:same',lineIn:true}]);
assert.equal(scShadowData(3).lineIn, true);
scShadowApplyAll([{deck:3,key:'cdj:same',lineIn:false}]);
assert.equal(scShadowData(3).lineIn, false);
""")

    def test_line_hint_stays_stock_without_cdj(self):
        old_root = bp.ROOT
        try:
            bp.ROOT = "Skins/Embedded/"
            patch = bp.edits(False)[bp.ROOT + "Views/Common/Waveform/WaveformLineInHint.qml"][0][1]
        finally:
            bp.ROOT = old_root
        expression = patch.strip().removeprefix("visible: ")
        self.js("globalThis.deckIndex = 3; globalThis.pLine = {translator:{state:true}};\n"
                "scPlayer = false; scFourDeckRole = '12';\n"
                "scShadowApplyAll([{deck:3,key:'peer:sc'}]);\n"
                f"assert.equal(!!({expression}), true);\n"
                "scShadowApplyAll([{deck:3,key:'cdj:fixture',lineIn:true}]);\n"
                f"assert.equal(!!({expression}), false);\n"
                "scShadowApplyAll([]);\n"
                f"assert.equal(!!({expression}), true);\n")

    def test_cdj_cached_analysis_uses_normal_display_and_console_free_lanes(self):
        self.js(r"""
scPlayer = false; scFourDeckRole = '12';
assert.equal(scDeckLocked(3), false); assert.equal(scDeckLocked(4), false);
scShadowApplyAll([{deck:3,key:'cdj:fixture',metadataOnly:true,title:'CDJ'}, {deck:1,key:'local'}]);
assert.equal(requests.length, 0); assert.equal(scShadowData(1), null);
assert.equal(scDeckLocked(3), true); assert.equal(scDeckLocked(4), false);
// Analysis may arrive later for the same track, without loading it in Engine.
scShadowApplyAll([{deck:3,key:'cdj:fixture',metadataOnly:false,playing:true,at:now,positionMs:500}]);
assert.equal(requests.length, 1);
assert.ok(requests[0].url.includes('/shadow/display?deck=3'));
requests[0].finish(200,{key:'cdj:fixture',sampleRate:44100,durationMs:4000,detail:true,
    beats:[{ms:0,number:0},{ms:500,number:1},{ms:1000,number:2}],cues:[],waveformUrl:'/wave.png'});
assert.equal(scShadowPosition(scShadowData(3), now+100),600);
assert.equal(scShadowGrid(3, scShadowData(3).meta, 0, 1500).length,3);
assert.equal(scShadowVisible(3),true); assert.equal(scDeckLocked(1),false); assert.equal(scDeckLocked(2),false);
scShadowApplyAll([{deck:4,key:'peer:fixture'}]);
assert.equal(scDeckLocked(3),false); assert.equal(scDeckLocked(4),true);
scShadowApplyAll([]);
assert.equal(scDeckLocked(3),false); assert.equal(scDeckLocked(4),false);
assert.deepEqual(legacy,[]);
""")

    def test_safe_default_empty_path_ownership_and_legacy_opt_in(self):
        self.js(r"""
globalThis.scShadowApply = e => legacy.push(e.deck);
globalThis.scShadowEmpty = d => legacy.push('empty' + d);
scPressQueue = [{path: '/Engine/Deck1/RequestMaster', step: 1}];
scShadowApplyAll([{deck: 1, key: 'local'}, {deck: 2, key: 'A', path: '', title: 'Remote', playing: true, at: now, positionMs: 500}]);
assert.equal(scShadowDisplayOnly, true);
assert.equal(scShadowData(1), null);
assert.equal(scShadowData(2).title, 'Remote');
assert.equal(requests.length, 1);
assert.deepEqual(legacy, []);
assert.deepEqual(scPressQueue, [{path: '/Engine/Deck1/RequestMaster', step: 1}]); // local controls are preserved
assert.equal(scShadowVisible(0), false);
scPlayer = false; // an existing display source reserves the console lane without audio
assert.equal(scDeckLocked(2), true);
assert.equal(scShadowVisible(2), true);
scShadowApplyAll([{deck: 2, loading: true}]);
assert.equal(scShadowData(2).loading, true);
assert.equal(requests.length, 1);
scShadowApplyAll([]);
assert.equal(scShadowData(2), null);
assert.equal(scDeckLocked(2), false); // no source: a console can still load its local deck
assert.equal(scShadowVisible(2), false);
assert.deepEqual(legacy, []);
scShadowApplyAll([{deck: 2, path: '/old.wav'}], false);
assert.equal(scShadowDisplayOnly, false);
assert.equal(scShadowVisible(2), false);
assert.equal(legacy[0], 2);
assert.deepEqual(scShadowDisplay, {});
""")

    def test_legacy_helpers_and_remote_presses_are_inert_in_display_mode(self):
        self.js(r"""
scShadowApply({deck: 2, path: '/must-not-load.wav'});
scShadowEmpty(2, now);
scShadowSpeed(2, 12);
scShadowCues(2, {}, {cues: []}, now);
scShadowBeat(2, {}, {bpm: 120, beat: 3}, 0, now);
scShadowSeek(2, {}, 1000);
scShadowLoad(2, {}, {path: '/must-not-load.wav'}, now);
assert.equal(scShadowHolds(2, {}, {path: '/must-not-load.wav'}, now), false);
scShadowLink(2, 'peer');
scShadowLinkEdge(2, 'peer', 'play', '{"state":true}');
scPress('/Engine/Deck2/RequestMaster');
scPress('/Engine/Deck4/Sync');
assert.deepEqual(scShadowAt, {});
""")

    def test_interpolation_pause_jump_and_expiry(self):
        self.js(r"""
const s = {positionMs: 1000, at: 10000, playing: true, pitch: 20};
assert.equal(scShadowPosition(s, 11000), 2200);
assert.equal(scShadowPosition(s, 50000), 4600); // expires after 3 s
assert.equal(scShadowPosition(s, 9000), 1000); // clock ahead of receiver
s.playing = false;
assert.equal(scShadowPosition(s, 50000), 1000);
s.positionMs = 300; s.at = 20000; s.playing = true; s.pitch = -50;
assert.equal(scShadowPosition(s, 21000), 800);
s.meta = {durationMs: 600};
assert.equal(scShadowPosition(s, 21000), 600);
""")

    def test_key_cache_404_backoff_and_stale_completions(self):
        self.js(r"""
scShadowApplyAll([{deck: 2, key: 'A & B'}]);
assert(requests[0].url.endsWith('deck=2&key=A%20%26%20B'));
scShadowApplyAll([{deck: 2, key: 'A & B', positionMs: 123}]);
assert.equal(requests.length, 1); // repeated live packets never re-fetch metadata
requests[0].finish(404, {});
scShadowDisplayFetch(2, 'A & B');
assert.equal(requests.length, 1);
now += 500;
scShadowDisplayFetch(2, 'A & B');
assert.equal(requests.length, 2);
scShadowApplyAll([{deck: 2, key: 'C'}]);
assert.equal(requests.length, 3);
requests[1].finish(200, {key: 'A & B', detail: true});
assert.equal(scShadowData(2).meta, null);
requests[2].finish(200, {key: 'C', detail: false, overviewUrl: '/overview.png'});
assert.equal(scShadowData(2).meta.detail, false);
now += 60000;
scShadowDisplayFetch(2, 'C');
assert.equal(requests.length, 3); // overview is ready; only not-ready responses retry
scShadowApplyAll([]);
assert.deepEqual(scShadowDisplayRequests, {});
""")

    def test_source_grid_countdown_and_visible_grid_window(self):
        self.js(r"""
const beats = [{ms: 0, number: 0}, {ms: 1000, number: 1}, {ms: 1500, number: 2}, {ms: 1750, number: 3}];
const meta = {beats, cues: [{i: 1, ms: 1750, n: 'DROP', c: '#123456'}]};
scShadowDisplay[2] = {key: 'A', meta, positionMs: 1000, at: now, pitch: 0, playing: false, bpm: 999};
const cue = scNextCueInfo(2); // must not read Engine or invent a constant-BPM grid
assert.equal(cue.text, '0.3');
assert.equal(cue.label, 'DROP'); assert.equal(cue.color, '#123456');
assert.strictEqual(scNextCueInfo(2), cue); // retain shadow display object caching
assert.equal(scCueTickFor(2), 0); // a resting shadow deck's countdown has no clock at all
scShadowDisplay[2].playing = true;
assert.equal(scCueTickFor(2), 17); // a shadow countdown must not depend on stopped local Engine clocks
assert.equal(scShadowAnyPlaying(scShadowDisplay), true);
scShadowDisplay[2].stale = true; assert.equal(scCueTickFor(2), 0); assert.equal(scShadowAnyPlaying(scShadowDisplay), false);
scShadowDisplay[2].playing = false; scShadowDisplay[2].stale = false;
assert.equal(scNextCueInfo(4), null);
const grid = scShadowGrid(2, meta, 900, 1600);
assert.deepEqual(grid, beats.slice(1, 3));
assert.strictEqual(scShadowGrid(2, meta, 910, 1610), grid); // same delegates until a grid boundary
assert.equal(scShadowAsset('/wave.png'), 'http://127.0.0.1:8080/wave.png');
assert.equal(scShadowAsset('file:///audio.wav'), '');
""")

    def test_live_cues_change_on_same_key_using_source_sample_rate(self):
        self.js(r"""
const meta = {key: 'A', sampleRate: 48000, durationMs: 4000,
    beats: [{ms: 0, number: 0}, {ms: 500, number: 1}, {ms: 1000, number: 2}, {ms: 1500, number: 3}],
    cues: [{i: 1, ms: 500, n: 'OLD'}]};
scShadowApplyAll([{deck: 2, key: 'A', cues: [{i: 1, p: 72000, n: 'LIVE', c: '#ff0000'}]}]);
requests[0].finish(200, meta);
assert.equal(scShadowCuesFor(2, scShadowData(2))[0].ms, 1500);
assert.equal(scNextCueInfo(2).text, '0.4');
scShadowApplyAll([{deck: 2, key: 'A', cues: [{i: 1, p: 48000, n: 'EDIT'}]}]);
assert.equal(requests.length, 1);
assert.equal(scShadowCuesFor(2, scShadowData(2))[0].ms, 1000);
assert.equal(scNextCueInfo(2).label, 'EDIT');
assert.equal(scNextCueInfo(2).text, '0.3');
scShadowApplyAll([{deck: 2, key: 'A', cues: []}]);
assert.equal(scNextCueInfo(2), null); // deleting all source cues must clear saved markers
scShadowApplyAll([{deck: 2, key: 'B'}]);
assert.equal(scShadowData(2).liveCues, null);
""")


    def test_stale_state_freezes_without_clearing(self):
        self.js(r"""
scShadowApplyAll([{deck:2,key:'A',playing:true,positionMs:1234,at:now,stale:true}]);
assert.equal(scShadowPosition(scShadowData(2),now+30000),1234);
assert.equal(scShadowData(2).key,'A');
scShadowApplyAll([{deck:2,key:'A',playing:true,positionMs:1500,at:now}]);
assert.equal(scShadowPosition(scShadowData(2),now+100),1600);
scShadowApplyAll([{deck:2}]);
assert.equal(scShadowData(2),null); // explicit empty source, not a time-based eject
""")

    def test_live_main_cue_marker_is_separate_from_hot_cue_countdown(self):
        self.js(r"""
scShadowApplyAll([{deck:2,key:'A',positionMs:0,cuePos:24000,cues:[{i:1,p:72000,n:'DROP'}]}]);
requests[0].finish(200,{key:'A',sampleRate:48000,durationMs:4000,
    beats:[{ms:0,number:0},{ms:500,number:1},{ms:1000,number:2},{ms:1500,number:3}],cues:[]});
assert.equal(scShadowCuesFor(2,scShadowData(2)).find(c=>c.i===0).ms,500);
assert.equal(scShadowNextCue(2).label,'DROP');
scShadowApplyAll([{deck:2,key:'A',positionMs:0,cuePos:48000,cues:[]}]);
assert.equal(scShadowCuesFor(2,scShadowData(2)).find(c=>c.i===0).ms,1000);
assert.equal(scShadowNextCue(2),null);
""")

    def test_equivalent_replies_do_not_republish_shadow_state(self):
        self.js(r"""
scShadowApplyAll([{deck:2,key:'A',playing:false,positionMs:1000,at:now,cues:[{i:1,p:4800,n:'X',c:'#fff'}]}]);
requests[0].finish(200,{key:'A',durationMs:60000,beats:[]});
const map = scShadowDisplay, s = scShadowData(2), cues = s.liveCues;
now += 250; // paused heartbeat: new arrays from JSON.parse, same picture
scShadowApplyAll([{deck:2,key:'A',playing:false,positionMs:1000,at:now,cues:[{i:1,p:4800,n:'X',c:'#fff'}]}]);
assert.strictEqual(scShadowDisplay, map); // no binding of any shadow layer re-evaluates
assert.strictEqual(scShadowData(2), s);
assert.strictEqual(s.liveCues, cues);
assert.equal(s.at, now); // anchor refreshed in place: never expires while the peer is alive
scShadowApplyAll([{deck:2,key:'A',playing:true,positionMs:1000,at:now}]);
const p = scShadowData(2);
assert.notStrictEqual(p, s); // a play edge publishes at once
assert.strictEqual(p.meta, s.meta);
now += 250; // steady playback on its line
scShadowApplyAll([{deck:2,key:'A',playing:true,positionMs:1250,at:now}]);
assert.strictEqual(scShadowData(2), p);
assert.equal(scShadowPosition(p, now + 100), 1350);
now += 250; // a beat jump or tempo slip is published
scShadowApplyAll([{deck:2,key:'A',playing:true,positionMs:1530,at:now}]);
assert.notStrictEqual(scShadowData(2), p);
assert.equal(scShadowPosition(scShadowData(2), now), 1530);
const q = scShadowData(2);
scShadowApplyAll([{deck:2,key:'A',playing:true,positionMs:1530,at:now,stale:true}]);
assert.notStrictEqual(scShadowData(2), q); // the stale flag is part of the picture
assert.equal(requests.length, 1); // complete metadata is never re-fetched per reply
""")

    def test_pending_detail_refetch_backs_off(self):
        self.js(r"""
scShadowApplyAll([{deck:2,key:'A'}]);
requests[0].finish(200,{key:'A',detailPending:true,beats:[]});
const firstMeta=scShadowData(2).meta, firstMap=scShadowDisplay;
now+=1999; scShadowDisplayFetch(2,'A'); assert.equal(requests.length,1);
now+=1; scShadowDisplayFetch(2,'A'); assert.equal(requests.length,2); // 2 s
requests[1].finish(200,{key:'A',detailPending:true,beats:[]});
assert.strictEqual(scShadowData(2).meta,firstMeta); // the same pending reply: no new meta, no delegate churn
assert.strictEqual(scShadowDisplay,firstMap);
now+=3999; scShadowDisplayFetch(2,'A'); assert.equal(requests.length,2);
now+=1; scShadowDisplayFetch(2,'A'); assert.equal(requests.length,3); // 4 s
requests[2].finish(200,{key:'A',detailPending:true,beats:[]});
now+=7999; scShadowDisplayFetch(2,'A'); assert.equal(requests.length,3);
now+=1; scShadowDisplayFetch(2,'A'); assert.equal(requests.length,4); // 8 s
requests[3].finish(200,{key:'A',detailPending:true,beats:[]});
now+=1000; scShadowApplyAll([{deck:2,key:'A'}]); assert.equal(requests.length,4); // still pending: no re-read
scShadowApplyAll([{deck:2,key:'A',displayDetail:true}]); assert.equal(requests.length,5); // stream-sync has it: at once
requests[4].finish(200,{key:'A',detailPending:false,detail:true,beats:[]});
assert.notStrictEqual(scShadowData(2).meta,firstMeta); assert.equal(scShadowData(2).meta.detail,true);
now+=60000; scShadowDisplayFetch(2,'A'); assert.equal(requests.length,5);
""")

    def test_sender_filters_block_quantised_positions(self):
        self.js(r"""
// Engine moves TrackPosition in blocks (up to ~36 ms of track time measured at +59.5 %, 45 here): the
// posted line must not carry that noise (no re-anchoring full posts).
let bodies=[], pings=0, t0=now, playing=false;
globalThis.XMLHttpRequest=class {
    constructor(){this.readyState=0;}
    open(m,url){this.url=url;this.readyState=1;}
    send(body){if(this.url.endsWith('/alive'))pings++;else bodies.push(JSON.parse(body));this.readyState=4;this.status=200;}
};
globalThis.scShadowPoll=()=>{};
scFourDeckRole='1';
const truth=()=>playing?(now-t0)*1.595:0, block=45;
globalThis.scTr=path=>({
    get value(){return path.endsWith('/SampleRate')?1000:0},
    get unnormalized(){return path.endsWith('/TrackPosition')?Math.floor(truth()/block)*block:path.endsWith('/OriginalBPM')?82:path.endsWith('/CurrentBPM')?82*1.5954:0},
    get state(){return path.endsWith('/SongLoaded')||(path.endsWith('/PlayState')&&playing)},
    get string(){return path.endsWith('/TrackUri')?'file:///a.mp3':path.endsWith('/SpeedState')?'59.54':''}, color:'#ffffff'});
const tick=ms=>{for(let t=0;t<ms;t+=40){now+=40;scShadowTick();}};
tick(400); playing=true; t0=now; tick(40);
const start=bodies.length;
tick(20000);
assert(bodies.length-start<=5, 'noisy positions re-anchored '+(bodies.length-start)+' times in 20 s');
// What the peer extrapolates (the newest full post) stays within a few ms of the
// true playhead; the raw sample would anchor anywhere up to a block behind.
const b=bodies[bodies.length-1][0], seen=b.positionMs+(now-b.at)*(1+b.pitch/100);
assert(Math.abs(seen-truth())<=8, 'peer line off by '+(seen-truth()));
""")

    def test_late_song_name_is_not_cached_empty(self):
        self.js(r"""
let bodies=[], title='';
globalThis.XMLHttpRequest=class {
    constructor(){this.readyState=0;}
    open(m,url){this.url=url;this.readyState=1;}
    send(body){if(!this.url.endsWith('/alive'))bodies.push(JSON.parse(body));this.readyState=4;this.status=200;}
};
globalThis.scShadowPoll=()=>{};
scFourDeckRole='1';
globalThis.scTr=path=>({value:path.endsWith('/SampleRate')?48000:0,unnormalized:0,state:path.endsWith('/SongLoaded'),
    get string(){return path.endsWith('/TrackUri')?'file:///new.mp3':path.endsWith('/SongName')?title:''},color:'#fff'});
now+=40; scShadowTick(); assert.equal(bodies.length,1); assert.equal(bodies[0][0].title,''); // loaded before Engine named it
title='Late Name'; now+=520; scShadowTick();
assert.equal(bodies.length,2); assert.equal(bodies[1][0].title,'Late Name'); // next heartbeat, not 2 s later
""")

    def test_unchanged_version_keeps_samples_alive_without_parsing(self):
        self.js(r"""
let header='5:true:13:false:false:ab', parsed=0;
globalThis.scShadowPollTimer={interval:0,restart(){}};
XMLHttpRequest.prototype.getResponseHeader=()=>header;
const realParse=JSON.parse; JSON.parse=s=>{parsed++;return realParse(s)};
scShadowPoll();requests[0].finish(200,{role:'13',displayOnly:true,ver:5,decks:[{deck:2,key:'A',playing:true,positionMs:1000,at:now}]});
assert.equal(parsed,1); const s=scShadowData(2);
now+=10000;
for(let i=0;i<5;i++){scShadowPoll();requests[0].responseText='{"never":"read"}';requests[0].finish(200,{});} // finish stringifies; the QML must not parse
assert.equal(parsed,1,'an unchanged version was parsed');
assert.equal(scShadowLive.seen,now);
assert.equal(scShadowPosition(s,now),11000); // carried on 10 s: stream-sync vouches for it
now+=4000;
assert.equal(scShadowPosition(s,now),14000); // 3 s after the last confirmation it stops
JSON.parse=realParse;
""")

    def layer_js(self):
        """The real SHADOW_LAYER functions, bound to a mock QML scope with `with`."""
        layer = bp.SHADOW_LAYER
        parts = []
        for name in ("contentX", "metaDuration", "metaWaveDuration", "metaTotalPixels", "metaTilePixels",
                     "refreshSample", "refreshPosition", "restartScroll", "refreshDetails"):
            m = re.search(r"(?m)^        function " + name + r"\(", layer)
            self.assertIsNotNone(m, name)
            line_end = layer.index("\n", m.start())
            end = line_end if layer[m.start():line_end].rstrip().endswith("}") else layer.index("\n        }", m.start()) + 10
            parts.append("scope." + name + " = " + layer[m.start():end].strip())
        for handler in ("onMetaChanged", "onLiveChanged"):
            m = re.search(r"(?m)^        " + handler + r": (.*)$", layer)
            body = m.group(1)
            if body.startswith("{"):
                body = layer[m.start(1):layer.index("\n        }", m.start(1)) + 10].strip()
            parts.append("scope." + handler + " = function() " + (body if body.startswith("{") else "{ " + body + " }"))
        keys = "for (const [k, v] of Object.entries({scrollKey: '', scrollJumps: 0})) if (!(k in scope)) scope[k] = v\n"
        return keys + "with (scope) {\n" + "\n".join(parts) + "\n}\n"

    def test_detail_arriving_while_paused_draws_the_tiles_under_the_playhead(self):
        # QML runs onMetaChanged before the bindings that derive from meta (duration,
        # totalPixels, ...) see the new object: model that order. The left deck showed
        # an empty waveform (grid and CUE only) until playback moved the window.
        self.js(r"""
const scroll={running:false,from:0,to:0,duration:0,stop(){this.running=false},start(){this.running=true}};
const scMoving={x:0};
const beats=[]; for(let i=0;i<800;i++) beats.push({ms:i*300,number:i});
const pending={key:'A',durationMs:225045,waveformDurationMs:225045,beats,cues:[],loops:[],detail:false,detailPending:true,waveformWidth:2048,tileWidth:2048};
const detail={key:'A',durationMs:225076,waveformDurationMs:225076,beats,cues:[],loops:[],detail:true,waveformWidth:23633,tileWidth:2048,waveformTiles:Array(12).fill('/t.png')};
let metaValue=null, bound=null; // bound: what the bindings saw last (updated after the handler)
const derived=m=>({duration:m&&m.durationMs>0?m.durationMs:1,waveformDuration:m&&m.waveformDurationMs>0?m.waveformDurationMs:1,
  totalPixels:m?m.waveformWidth:0,tilePixels:m&&m.tileWidth?m.tileWidth:2048});
const scope={live:true,visible:true,wave:true,overview:false,graphical:true,deck:2,sample:null,cues:scShadowEmptyArray,loops:scShadowEmptyArray,
  position:0,stale:false,cueInfo:null,grid:scShadowEmptyArray,gridAnchor:-Infinity,gridSlots:0,tileLo:0,tileHi:31,scrollFrom:0,scrollAt:0,scrollSpeed:0,width:1000,
  scroll,scMoving,root:globalThis,
  get meta(){return metaValue}, set meta(v){if(v!==metaValue){metaValue=v;scope.onMetaChanged();bound=derived(v)}},
  get duration(){return bound?bound.duration:1}, get waveformDuration(){return bound?bound.waveformDuration:1},
  get totalPixels(){return bound?bound.totalPixels:0}, get tilePixels(){return bound?bound.tilePixels:2048},
  get span(){return 12180}, get playhead(){return 0.5}, get start(){return this.position-this.span*this.playhead}};
""" + self.layer_js() + r"""
const sample=(m,extra)=>Object.assign({key:'A',meta:m,positionMs:29310,at:now,playing:false,pitch:0,stale:false},extra||{});
scope.sample=sample(pending); scope.refreshSample();
scope.sample=sample(detail); scope.refreshSample(); // stream-sync has the detail; the deck stays paused
const tileMs=2048/23633*225076, from=29310-12180/2, pad=6090;
assert.equal(scope.tileLo,Math.floor((from-pad)/tileMs)); assert.equal(scope.tileHi,Math.floor((from+12180+pad)/tileMs));
assert(scope.tileLo<=Math.floor(29310/tileMs) && Math.floor(29310/tileMs)<=scope.tileHi,'the tile under the playhead is drawn');
assert.equal(scope.tileHi,2);
// A new track while playing: the scroll runs to the new track's end, not to the stale duration.
const next={key:'B',durationMs:180000,waveformDurationMs:180000,beats,cues:[],loops:[],detail:true,waveformWidth:20000,tileWidth:2048,waveformTiles:Array(10).fill('/t.png')};
scope.sample=Object.assign(sample(next,{key:'B',playing:true}),{positionMs:1000}); scope.refreshSample();
assert.equal(scroll.running,true); assert.equal(scroll.to,scope.contentX(180000));
""")

    def test_scrolling_lane_hides_the_stretched_overview_until_the_detail_arrives(self):
        # User report 11-10: the left deck's small waveform showed the whole song (a stretched overview).
        expr = re.search(r"(?m)^        readonly property string imageUrl: (.*)$", bp.SHADOW_LAYER).group(1)
        self.js("const url=(overview,detailed,meta)=>{with({overview,detailed,meta}){return " + expr + "}};\n" + r"""
const m={overviewUrl:'/o.png',waveformUrl:'/w.png'};
assert.equal(url(false,false,Object.assign({detailPending:true},m)),'');           // still coming: grid + playhead only
assert.equal(url(false,false,Object.assign({detailPending:false},m)),'/o.png');    // overview-only source: keep the fallback
assert.equal(url(false,true,m),'/w.png'); assert.equal(url(true,false,Object.assign({detailPending:true},m)),'/o.png');
assert.equal(url(false,false,null),'');
""")

    def test_shadow_follows_the_stock_playhead_and_overview_style(self):
        # 11-10: playhead setting left of centre, shadow lanes stayed centred; the
        # shadow overview was mirrored and had no minute marks beside the local one.
        layer = bp.SHADOW_LAYER
        expr = re.search(r"(?m)^        readonly property real playhead: (.*)$", layer).group(1).replace('"@KIND@" === "overview"', "overview")
        self.js("const at=(overview,wave,owner,position,duration)=>{with({overview,wave,owner,position,duration}){return " + expr + "}};\n" + r"""
assert.equal(at(false,true,{zeroPosition:0.2001}),0.2001);   // the lane: the local WaveFormItem's playhead
assert.equal(at(false,true,{}),0.5); assert.equal(at(false,true,{zeroPosition:0}),0.5);
assert.equal(at(true,false,{zeroPosition:0.2001},30000,120000),0.25); // the overview: the track position
""")
        self.assertIn("onPlayheadChanged: if (wave) { gridAnchor = -Infinity; restartScroll(); refreshDetails() }", layer)
        half = re.search(r"(?m)^        readonly property bool half: (.*)$", layer).group(1).replace('"@KIND@" === "overview"', "overview")
        self.js("const half=(overview,index)=>{with({overview,root:{scTr:p=>(assert.equal(p,'/Client/Preferences/Profile/Application/TrackOverviewStyle'),index===null?null:{index})}}){return " + half + "}};\n" + r"""
assert.equal(half(true,0),true); assert.equal(half(true,1),false); assert.equal(half(true,null),true); assert.equal(half(false,0),false);
""")
        self.assertIn('(half ? "&half=1" : "")', layer)
        self.assertIn("onHalfChanged: paletteUpdate.restart()", layer)
        self.assertIn("Style.icon02Color", layer)

    def test_shadow_layer_scroll_settle_and_grid_pool_behaviour(self):
        self.js(r"""
const starts=[];
const scMoving={x:0};
// Qt: stop() writes the value on screen back to x; a run shows its first value one frame (17 ms) on.
const scroll={running:false,from:0,to:0,duration:0,at:0,stop(){if(this.running)scMoving.x=this.from+(this.to-this.from)*Math.max(0,Math.min(1,(now-this.at-17)/this.duration));this.running=false},
  start(){this.running=true;this.at=now;starts.push({from:this.from,to:this.to,duration:this.duration})}};
const beats=[]; for(let i=0;i<660;i++) beats.push({ms:i*300,number:i}); // ~53 beats per window: not a power of two
const meta={key:'A',durationMs:200000,waveformDurationMs:200000,beats,cues:[],loops:[],detail:true,waveformWidth:20480,tileWidth:2048,waveformTiles:Array(10).fill('/t.png')};
let metaValue=null;
const scope={live:true,visible:true,wave:true,overview:false,graphical:true,deck:2,sample:null,cues:scShadowEmptyArray,loops:scShadowEmptyArray,
  position:0,stale:false,cueInfo:null,grid:scShadowEmptyArray,gridAnchor:-Infinity,gridSlots:0,tileLo:0,tileHi:31,scrollFrom:0,scrollAt:0,scrollSpeed:0,width:1000,
  scroll,scMoving,root:globalThis,
  get meta(){return metaValue}, set meta(v){if(v!==metaValue){metaValue=v;scope.onMetaChanged()}},
  get duration(){return metaValue&&metaValue.durationMs>0?metaValue.durationMs:1},
  get waveformDuration(){return metaValue&&metaValue.waveformDurationMs>0?metaValue.waveformDurationMs:this.duration},
  get span(){return 8000}, get playhead(){return 0.5}, get start(){return this.position-this.span*this.playhead},
  get totalPixels(){return metaValue?metaValue.waveformWidth:0}, get tilePixels(){return metaValue?metaValue.tileWidth:2048}};
""" + self.layer_js() + r"""
const sample=(pos,playing,extra)=>Object.assign({key:'A',meta,positionMs:pos,at:now,playing,pitch:0,stale:false},extra||{});
scope.sample=sample(10000,true); scope.refreshSample();
assert.strictEqual(scope.meta,meta);
assert.equal(starts.length,1); // one render-thread scroll to the end of the track
assert.equal(starts[0].from,scope.contentX(10017)); assert.equal(starts[0].to,scope.contentX(200000)); assert.equal(starts[0].duration,189983); // shown one frame on
// Grid pool: power of two above the window's beats; tiles under the window only.
assert(scope.grid.length>0 && scope.gridSlots>=scope.grid.length && (scope.gridSlots&(scope.gridSlots-1))===0 && scope.gridSlots>=32);
assert.equal(scope.tileLo,0); assert.equal(scope.tileHi,Math.floor((6000+12000)/20000));
now+=200; scope.refreshPosition(); assert.equal(starts.length,1); // same line: left to the 200 ms clock
scope.sample=sample(10000+200+20,true,{jumps:1}); scope.refreshSample(); assert.equal(starts.length,2); // 20 ms off after a jump: restarted
scope.sample=sample(10220,true,{jumps:1,pitch:8}); scope.refreshSample(); assert.equal(starts.length,2); // new speed, no jump: the clock takes it up
now+=200; scope.refreshPosition(true); assert.equal(starts.length,3); assert(scope.scrollSpeed>1.08&&scope.scrollSpeed<1.13); // 1.08 and the 16 ms it fell behind
// CUE released back on its spot: a 1 ms settle to x, never a bare x write the write-back would undo.
scope.sample=sample(10000,false); scope.refreshSample();
const settle=starts[starts.length-1];
assert.equal(starts.length,4); assert.equal(settle.from,scope.contentX(10000)); assert.equal(settle.to,settle.from); assert.equal(settle.duration,1);
assert.equal(scope.scrollSpeed,0);
scroll.running=false; scope.refreshPosition(); assert.equal(starts.length,4); // settled: nothing more
scope.sample=sample(12000,false); scope.refreshSample(); assert.equal(starts.length,4); assert.equal(scMoving.x,scope.contentX(12000)); // paused seek, no animation running
scope.sample=sample(12000,true); scope.refreshSample(); assert.equal(starts.length,5);
scope.restartScroll(); assert.equal(starts.length,6); // zoom/size/visibility: a fresh line
// Grid window moves with the playhead, the pool never shrinks while shown.
const slots=scope.gridSlots; scope.position=60000; scope.refreshDetails();
assert(scope.grid[0].ms>=60000-4000-4000-1 && scope.gridSlots===slots && scope.tileLo===Math.floor((56000-4000)/20000));
scope.live=false; scope.onLiveChanged(); assert.equal(scope.gridSlots,0); // hidden layer: no delegates
// track/performance layers recompute cueInfo on every sample.
let asked=0; const ci={text:'1.1'}; globalThis.scNextCueInfo=(d,t)=>{asked++;return ci};
scope.live=true; scope.wave=false; scope.graphical=false; scope.sample=sample(1000,false); scope.refreshSample();
assert.strictEqual(scope.cueInfo,ci); assert.equal(asked,1);
""")

    def test_shadow_layer_moves_only_on_the_render_thread(self):
        layer = bp.SHADOW_LAYER
        # Hidden stock lanes must not follow the shadow state at all.
        self.assertIn("readonly property bool live: active && inheritedVisible", layer)
        self.assertIn("readonly property var sample: live ? root.scShadowData(deck) : null", layer)
        # No GUI-thread position animation; the content scrolls with one XAnimator.
        self.assertNotIn("NumberAnimation", layer)
        self.assertIn("XAnimator { id: scroll; target: scMoving", layer)
        self.assertNotIn("glide", layer.lower()) # the 200 ms clock steers the one scroll
        self.assertIn("onTriggered: { scDisplay.refreshPosition(true); scDisplay.refreshDetails() }", layer)
        moving = layer[layer.index("id: scMoving"):layer.index("} // scMoving")]
        self.assertNotRegex(moving, r"(?m)^\s*x:\s*-?\s*scDisplay\.(start|position)")
        # Markers, loops and tiles sit in track coordinates: their x never depends on time.
        for x in re.findall(r"(?m)^\s*(?:x|readonly property real clip\w+):\s*(.+)$", moving):
            self.assertNotRegex(x, r"scDisplay\.(start|position|playhead)\b", x)
        # var properties notify on every write: copy only changed objects.
        for name in ("meta", "cues", "loops", "grid", "cueInfo"):
            self.assertNotRegex(layer, r"readonly property var " + name + r":")
        self.assertIn("if (m !== meta) meta = m", layer)
        self.assertIn("if (rows !== grid) grid = rows", layer)
        self.assertIn("model: scDisplay.loops", layer)
        # 7c29415: an empty shadow slot runs no 200 ms clock.
        self.assertIn("running: scDisplay.live && scDisplay.visible && scDisplay.sample !== null", layer)
        self.assertIn("scDisplay.sample !== null && scDisplay.sample.playing && !scDisplay.sample.stale; onTriggered", layer)
        self.assertIn('Binding { target: scDisplay.owner; property: "opacity"; value: 0; when: scDisplay.visible; restoreMode: Binding.RestoreBindingOrValue }', layer)
        self.assertIn("layer.enabled: live && !wave", layer)
        self.assertIn("onVisibleChanged: { restartScroll(); if (visible) refreshDetails() }", layer)
        # Grid lines are a pool: a new grid window must not recreate the delegates.
        self.assertIn("model: scDisplay.gridSlots", layer)
        self.assertNotIn("model: scDisplay.grid\n", layer)
        self.assertIn("scDisplay.grid[index]", layer)
        # Off-window tiles are not drawn; the window moves with the grid, not per frame.
        self.assertIn("visible: index >= scDisplay.tileLo && index <= scDisplay.tileHi", layer)
        self.assertIn("model: scDisplay.cues", layer)

    def test_legacy_polls_are_not_skipped_by_display_version_cache(self):
        self.js(r"""
let applied=0, header='1:false:13:false:false';
globalThis.scShadowPollTimer={interval:0,restart(){}};
XMLHttpRequest.prototype.getResponseHeader=()=>header;
globalThis.scShadowApplyAll=(decks,displayOnly)=>{applied++;scShadowDisplayOnly=displayOnly;};
scShadowPoll();requests[0].finish(200,{role:'13',displayOnly:false,decks:[],ver:1});
scShadowPoll();requests[0].finish(200,{role:'13',displayOnly:false,decks:[],ver:1});
assert.equal(applied,2);assert.equal(requests.length,1);
header='1:true:13:false:false';
scShadowPoll();requests[0].finish(200,{role:'13',displayOnly:true,decks:[],ver:1});
scShadowPoll();requests[0].finish(200,{role:'13',displayOnly:true,decks:[],ver:1});
assert.equal(applied,3); // display-only can skip identical snapshots
""")

    def test_cdj_metadata_only_does_not_request_unavailable_assets(self):
        self.js(r"""
scShadowApplyAll([{deck:2,key:'cdj:fixture',metadataOnly:true,title:'CDJ source',bpm:120,master:true}]);
assert.equal(scShadowData(2).title,'CDJ source');assert.equal(scShadowData(2).master,true);
assert.equal(requests.length,0);
""")

    def test_sampling_reuses_buffers_and_post_transport(self):
        self.js(r"""
let created=0, full=0, pings=0;
globalThis.XMLHttpRequest=class {
    constructor(){created++;this.readyState=0;}
    open(method,url){assert.equal(method,'POST');this.url=url;this.readyState=1;}
    send(body){
        if(this.url.endsWith('/decks/alive')){assert.equal(body,undefined);pings++}
        else{assert(this.url.endsWith('/decks'));assert.equal(JSON.parse(body).length,2);full++}
        this.readyState=4;this.status=this.url.endsWith('/alive')?204:200;if(this.onreadystatechange)this.onreadystatechange();}
};
globalThis.scShadowPoll=()=>{};
let strings=0;
globalThis.scTr=path=>({value:path.endsWith('/SampleRate')?48000:0,unnormalized:0,state:path.endsWith('/SongLoaded'),get string(){strings++;return path.endsWith('/TrackUri')?'file:///a.mp3':''},color:'#ffffff'});
scShadowTick();const first=scShadowPrev[1], own=scShadowOwn, post=scShadowPostRequest;
now+=40;scShadowTick();const second=scShadowPrev[1];
assert.notStrictEqual(first,second);
now+=40;scShadowTick();assert.strictEqual(scShadowPrev[1],first);
for(let i=0;i<100;i++){now+=40;scShadowTick();}
assert.strictEqual(scShadowOwn,own);assert.strictEqual(scShadowPostRequest,post);
assert.equal(created,1);
// 4 s at 25 Hz, 2 decks: each tick used to read SpeedState, TrackUri and SyncMode
// (600 JS strings); only changes and the slow refresh re-read them now.
// SpeedRange adds one read per deck, and one more whenever its index moves.
assert(strings <= 120, 'string reads ' + strings);
assert.equal(scShadowPrev[1].net, 'file:///a.mp3');
// 4 s of unchanged paused decks: one full post, then pings only (no JSON).
assert.equal(full, 1); assert(pings >= 7 && pings <= 9, 'pings ' + pings);
for(let i=0;i<30;i++){now+=40;scShadowTick();}
assert.equal(full, 2); // at least one full post every 5 s
scShadowPostRequest.status=409; scShadowPostRequest.onreadystatechange(); // stream-sync lost our decks
now+=520;scShadowTick(); assert.equal(full, 3);
""")

    def test_sender_posts_changes_and_pings_steady_playback(self):
        self.js(r"""
let bodies=[], pings=0, pos=0, playing=false, cueName='A';
globalThis.XMLHttpRequest=class {
    constructor(){this.readyState=0;}
    open(m,url){this.url=url;this.readyState=1;}
    send(body){if(this.url.endsWith('/alive'))pings++;else bodies.push(JSON.parse(body));this.readyState=4;this.status=200;}
};
globalThis.scShadowPoll=()=>{};
scFourDeckRole='1';
globalThis.scTr=path=>({
    get value(){return path.endsWith('/SampleRate')?48000:path.endsWith('QuickCue1Position')?96000:0},
    get unnormalized(){return path.endsWith('/TrackPosition')?pos:path.endsWith('BPM')?120:0},
    get state(){return path.endsWith('/SongLoaded')||path.endsWith('QuickCue1Exists')||(path.endsWith('/PlayState')&&playing)},
    get string(){return path.endsWith('/TrackUri')?'file:///a.mp3':path.endsWith('QuickCue1Name')?cueName:''}, color:'#ffffff'});
const tick=ms=>{for(let t=0;t<ms;t+=40){now+=40;if(playing)pos+=40*48;scShadowTick();}};
tick(1000); const n0=bodies.length; assert.equal(n0,1);
playing=true; tick(40); assert.equal(bodies.length,2); assert.equal(bodies[1][0].playing,true); // play edge at once
tick(4000); assert.equal(bodies.length,2,'steady playback must only ping'); assert(pings>=8);
pos-=48*20; tick(40); assert.equal(bodies.length,2); // a sample one block (20 ms) behind: still a ping
pos+=48*20; tick(6000); assert.equal(bodies.length,3); // the 5 s full post stays on the same line
const b2=bodies[2][0], b1=bodies[1][0];
assert(Math.abs(b2.positionMs-(b1.positionMs+(b2.at-b1.at)))<2, 'safety post moved the line');
pos+=48*200; tick(40); assert.equal(bodies.length,4); // a 200 ms slip: full post at once
assert(Math.abs(bodies[3][0].positionMs-pos/48)<1e-6);
cueName='DROP'; tick(2600); assert.equal(bodies.length,5); assert.equal(bodies[4][0].cues[0].n,'DROP'); // a renamed cue
""")

    def test_view_autosave_snapshots_only_after_a_change(self):
        fns = []
        for name in ("scViewCheck", "scViewMark"):
            m = re.search(r"(?m)^\tfunction " + name + r"\([^\n]*\) \{", bp.SERVICES_MAIN)
            self.assertIsNotNone(m, name)
            line_end = bp.SERVICES_MAIN.index("\n", m.start())
            end = line_end if bp.SERVICES_MAIN[m.start():line_end].rstrip().endswith("}") else bp.SERVICES_MAIN.index("\n\t}", m.end()) + 3
            fns.append(bp.SERVICES_MAIN[m.start():end])
        self.assertIn("onScWaveChanged: scViewDirty = true", bp.SERVICES_MAIN)
        self.assertIn("root.scViewCheck(Date.now())", bp.SERVICES_MAIN)
        js = r"""
const assert = require('assert');
globalThis.root = globalThis;
let snaps = 0, saves = 0, hooks = 0, view = {deckCount: '4 Decks'};
Object.assign(globalThis, {scViewQuiet: 0, scViewHooked: false, scViewDirty: true, scViewCheckAt: 0, scViewLast: '',
    scViewHook() { hooks++; scViewHooked = true }, scViewSnapshot() { snaps++; return Object.assign({}, view) }, scViewSave() { saves++ }});
""" + "\n".join(fns) + r"""
let now = 100000;
scViewQuiet = now + 3000; scViewCheck(now); assert.equal(snaps, 0); // an applied view settles first
now += 4000; scViewCheck(now); assert.equal(hooks, 1); assert.equal(snaps, 1); assert.equal(scViewLast, '{"deckCount":"4 Decks"}');
for (let i = 0; i < 10; i++) { now += 2000; scViewCheck(now); }
assert.equal(snaps, 1, 'no change signal: no snapshot');
view.deckCount = '2 Decks'; scViewMark(); now += 2000; scViewCheck(now);
assert.equal(snaps, 2); assert.equal(saves, 1); assert.equal(hooks, 1);
now += 2000; scViewCheck(now); assert.equal(snaps, 2);
now += 30000; scViewCheck(now); assert.equal(snaps, 3, 'a missed signal is caught within 30 s');
"""
        res = subprocess.run(["node", "-e", js], capture_output=True, text=True)
        self.assertEqual(res.returncode, 0, res.stderr)

    def test_sender_tick_rate_follows_activity(self):
        self.js(r"""
let bodies=[], pings=0, pos=0, playing=false, cue=false, wakes=[];
globalThis.XMLHttpRequest=class {
    constructor(){this.readyState=0;}
    open(m,url){this.url=url;this.readyState=1;}
    send(body){if(this.url.endsWith('/alive'))pings++;else bodies.push(JSON.parse(body));this.readyState=4;this.status=200;}
};
globalThis.scShadowPoll=()=>{};
scFourDeckRole='1';
const sig=()=>({connect(f){wakes.push(f)}});
globalThis.scTr=path=>({
    get value(){return path.endsWith('/SampleRate')?48000:0},
    get unnormalized(){return path.endsWith('/TrackPosition')?pos:path.endsWith('BPM')?120:0},
    get state(){return path.endsWith('/SongLoaded')||(path.endsWith('/PlayState')&&playing)||(path.endsWith('/CueLEDState')&&cue)},
    get string(){return path.endsWith('/TrackUri')?'file:///a.mp3':''}, color:'#ffffff', stateChanged: sig()});
// The timer fires at whatever interval the sender asked for.
const run=ms=>{const end=now+ms;while(now<end){const dt=scShadowTickTimer.interval;now+=dt;if(playing||cue)pos+=dt*48;scShadowTick();}};
run(400);
run(3000); assert.equal(scShadowTickTimer.interval,200,'resting decks sample at 5 Hz');
assert.equal(wakes.length,8); // PlayState and CueLEDState per deck (own deck 1 first)
playing=true; wakes[0](); assert.equal(scShadowTickTimer.interval,40,'a play press wakes the sender at once');
const n=bodies.length; run(40); assert.equal(bodies.length,n+1); assert.equal(bodies[n][0].playing,true);
run(2500); assert.equal(scShadowTickTimer.interval,80,'steady playback samples at 12.5 Hz');
const p0=pings; run(4000); assert(pings-p0>=6,'heartbeats keep at least ~2 Hz'); assert.equal(scShadowTickTimer.interval,80);
pos+=48*500; run(80); assert.equal(scShadowTickTimer.interval,40,'a jump goes back to 25 Hz');
playing=false; run(2500); assert.equal(scShadowTickTimer.interval,200);
pos+=48*300; run(200); assert.equal(scShadowTickTimer.interval,40,'a paused seek or scrub samples at 25 Hz');
run(2500); assert.equal(scShadowTickTimer.interval,200);
cue=true; wakes[1](); run(1000); assert.equal(scShadowTickTimer.interval,40); // CUE preview: moving with PlayState false
run(3000); assert.equal(scShadowTickTimer.interval,40,'a held CUE preview keeps 25 Hz');
cue=false; run(2500); assert.equal(scShadowTickTimer.interval,200);
""")

    def test_safe_gc_only_when_nothing_plays(self):
        self.js(r"""
let runs=0, plays=[false,false,false,false], cmds=[false,false,false,false], lengths=[0,0,0,0];
globalThis.gc=()=>runs++;
globalThis.scTr=path=>{const d=+path[12]-1;return path.endsWith('/PlayState')?{get state(){return plays[d]}}:path.endsWith('/Play')?{get state(){return cmds[d]}}:{get unnormalized(){return lengths[d]}}};
// A held CUE preview sounds and scrolls with PlayState false (scShadowTick's sample),
// and the Play command leads PlayState: neither may collect.
scShadowPrev[1]={playing:true};
for(let i=0;i<10;i++){now+=500;scGcCheck();}
assert.equal(runs,0);
scShadowPrev[1]={playing:false}; cmds[2]=true;
for(let i=0;i<10;i++){now+=500;scGcCheck();}
assert.equal(runs,0);
cmds[2]=false; scGcState.armed=false; // back to the scenario below
const missing=globalThis.scTr; globalThis.scTr=()=>null; scGcProps=null;
for(let i=0;i<10;i++){now+=500;assert.equal(scGcCheck(),false);} // Engine decks not built yet: never blind
globalThis.scTr=missing;
plays[0]=true;
for(let i=0;i<10;i++){now+=500;scGcCheck();}
assert.equal(runs,0); // never while a local deck plays
plays[0]=false; now+=500; scGcCheck(); now+=1000; scGcCheck();
assert.equal(runs,0); // 1.5 s settle after the stop
now+=500; scGcCheck(); assert.equal(runs,1);
for(let i=0;i<40;i++){now+=500;scGcCheck();}
assert.equal(runs,1); // once per stop, not periodically
lengths[1]=12345; now+=500; scGcCheck(); assert.equal(runs,1);
now+=1500; scGcCheck(); assert.equal(runs,2); // a finished load collects once
scShadowDisplay[2]={playing:true,stale:false};
lengths[1]=777; now+=500; scGcCheck();
for(let i=0;i<20;i++){now+=500;scGcCheck();}
assert.equal(runs,2); // a playing shadow deck blocks it too, and the load is dropped
scShadowDisplay[2]={playing:false,stale:false};
for(let i=0;i<4;i++){now+=500;scGcCheck();}
assert.equal(runs,3); // the stop of that shadow deck
scGcLoadSeq++; now+=500; scGcCheck(); now+=1500; scGcCheck();
assert.equal(runs,3); // at most one collection per 3 s
now+=1000; scGcCheck(); assert.equal(runs,4); // display data of a peer load
for(let i=0;i<11;i++){now+=500;scGcCheck();}
assert.equal(runs,4);
now+=500; scGcCheck(); assert.equal(runs,5); // once more 6 s after a load: Engine's own load garbage
for(let i=0;i<40;i++){now+=500;scGcCheck();}
assert.equal(runs,5); // and only once
lengths[2]=4242; now+=500; scGcCheck(); now+=1500; scGcCheck(); assert.equal(runs,6);
plays[3]=true; now+=500; scGcCheck(); plays[3]=false;
for(let i=0;i<13;i++){now+=500;scGcCheck();}
assert.equal(runs,7); // a play in between drops the follow-up (this one is the stop)
for(let i=0;i<20;i++){now+=500;scGcCheck();}
assert.equal(runs,7);
""")


class UiModBuildTest(unittest.TestCase):
    def test_jc11_jc11s_packs_contain_shadow_display(self):
        # Each supported console gets a separately built archive. The patcher
        # is board independent; stock 5.3.0 QML is the shared input for these
        # runtime board gates (the board name must affect the output under test).
        qrc = LAB / "5.3.0" / "qrc"
        if not qrc.is_dir():
            self.skipTest(f"{qrc} not found (run on Oracle-Batam-arm)")
        with tempfile.TemporaryDirectory(prefix="ui-console-test-") as tmp:
            for board in ("JC11", "JC11S", "JP21", "JP24"):
                with self.subTest(board=board):
                    out = Path(tmp) / board / "packs"
                    res = subprocess.run(["python3", str(BUILD_PACKS), "--qrc", str(qrc), "--out", str(out)],
                                         capture_output=True, text=True)
                    self.assertEqual(res.returncode, 0, f"{board}: {res.stderr}")
                    with tarfile.open(out.with_suffix(".tar.gz")) as pack:
                        for lang in ("en", "zh-Hans", "zh-Hant"):
                            main = pack.extractfile(f"packs/{lang}/Skins/Embedded/EmbeddedMain.qml").read().decode()
                            self.assertIn("function scShadowApplyDisplay(decks)", main)
                            self.assertIn("s.metadataOnly", main)
                            self.assertIn("/v1/sc5000/shadow/display?deck=", main)
                            self.assertIn("scDeckLocked", main)
                            for component, name in (("Waveform", "waveform"), ("WaveformOverview", "overview")):
                                wave = pack.extractfile(f"packs/{lang}/Skins/Embedded/Views/Common/{component}/{component}.qml").read().decode()
                                self.assertIn(f'objectName: "EngineX_Shadow_{name}"', wave)

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
                micro = (Path(tmp) / "packs" / lang / "Skins/EngineOS/UI/CompositeControls/Deck/MicroDeck.qml").read_text()
                states = re.findall(r"PropertyChanges \{\s+target: albumArt\s+visible: microDeck.albumArtVisible\s+(.*?)\n            \}", micro, re.S)
                self.assertTrue(states)
                for state in states:
                    self.assertNotRegex(state, r"\bTimer\s*\{")
                    self.assertNotRegex(state, r"\bproperty\s")
                self.assertTrue(any("albumArt.scArtTick" in state for state in states))
                image = re.search(r"Image \{\s+id: albumArt\s+(.*?)objectName:", micro, re.S)
                self.assertIsNotNone(image)
                self.assertIn("property int scArtTick", image.group(1))
                self.assertIn("Timer {", image.group(1))
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
            embedded = Path(tmp) / "packs" / "en" / "Skins" / "Embedded"
            wave = (embedded / "Views/Common/Waveform/Waveform.qml").read_text()
            self.assertIn('objectName: "EngineX_Shadow_waveform"', wave)
            self.assertIn("sourceSize.width: scDisplay.overview || !scDisplay.detailed ? 1024 : 2048", wave)
            self.assertIn("sourceSize.height: scDisplay.overview || !scDisplay.detailed ? 64 : 128", wave)
            self.assertNotIn("Canvas {", bp.SHADOW_LAYER)
            overview = (embedded / "Views/Common/WaveformOverview/WaveformOverview.qml").read_text()
            self.assertIn('objectName: "EngineX_Shadow_overview"', overview)

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


class ShadowNudgeTest(unittest.TestCase):
    """#5/#7: a jog nudge and the peer's clock drift move the shadow lane without jumps."""
    js = ShadowDisplayTest.js
    layer_js = ShadowDisplayTest.layer_js

    RECEIVER = r"""
const starts=[];
const scMoving={x:0};
// Qt: stop() writes the value on screen back to x; a run shows its first value one frame
// (17 ms) on. `fast` is the Animator's clock against the wall clock.
let fast=1;
const scroll={running:false,from:0,to:0,duration:0,at:0,stop(){if(this.running)scMoving.x=this.from+(this.to-this.from)*Math.min(1,((now-this.at)*fast-17)/this.duration);this.running=false},
  start(){this.running=true;this.at=now;starts.push({from:this.from,to:this.to,duration:this.duration})}};
const meta={key:'A',durationMs:2000000,waveformDurationMs:2000000,beats:[],cues:[],loops:[],detail:true,waveformWidth:20480,tileWidth:2048,waveformTiles:Array(10).fill('/t.png')};
const scope={live:true,visible:true,wave:true,overview:false,graphical:true,deck:2,sample:null,cues:scShadowEmptyArray,loops:scShadowEmptyArray,
  position:0,stale:false,cueInfo:null,grid:scShadowEmptyArray,gridAnchor:-Infinity,gridSlots:0,tileLo:0,tileHi:31,scrollSpeed:0,width:1000,
  scroll,scMoving,root:globalThis,meta,duration:2000000,waveformDuration:2000000,span:8000,playhead:0.5,totalPixels:20480,tilePixels:2048,
  get start(){return this.position-this.span*this.playhead}};
const sample=(pos,extra)=>Object.assign({key:'A',meta,positionMs:pos,at:now,playing:true,pitch:0,jumps:0,stale:false},extra||{});
const near=(a,b,d)=>assert(Math.abs(a-b)<=(d||0.001),a+' != '+b);
// Track ms under the playhead on screen, without disturbing the run.
const shown=()=>{const r=scroll.running,x=scMoving.x;scroll.stop();const ms=(scope.playhead-scMoving.x/scope.width)*scope.span;scroll.running=r;scMoving.x=x;return ms};
"""

    def test_receiver_steers_over_a_slip_and_jumps_only_on_a_jump(self):
        self.js(self.RECEIVER + self.layer_js() + r"""
scope.sample=sample(10000); scope.refreshSample(); assert.equal(starts.length,1); near(starts[0].from,scope.contentX(10017));
// 20 ms off (the peer's clock, a jog nudge): the sample alone restarts nothing.
now+=100; scope.sample=sample(10100+20); scope.refreshSample(); assert.equal(starts.length,1);
// The clock goes on from what is shown, at the speed that closes the 20 ms in 0.4 s.
now+=100; near(shown(),10200); scope.refreshPosition(true); assert.equal(starts.length,2);
near(scope.scrollSpeed,1.05); near(starts[1].from,scope.contentX(10200+17*1.05)); near(starts[1].duration,Math.round((2000000-10200-17*1.05)/1.05));
now+=200; near(shown(),10200+200*1.05); scope.refreshPosition(true); near(scope.scrollSpeed,1.025); // 10 ms left
// A brake: back the same way, never backwards.
scope.sample=sample(10420-30); scope.refreshSample(); now+=200; scope.refreshPosition(true); near(scope.scrollSpeed,1-(30-5)/400);
scope.sample=sample(shown()-299); scope.refreshSample(); now+=200; scope.refreshPosition(true); assert(scope.scrollSpeed>=0.25&&scope.scrollSpeed<0.3);
// More than 300 ms off, a jump (seek, hot cue, loop) or another track: a fresh line at once.
let n=starts.length;
scope.sample=sample(50000); scope.refreshSample(); assert.equal(starts.length,n); now+=200; scope.refreshPosition(true);
assert.equal(starts.length,n+1); near(starts[n].from,scope.contentX(50200+17)); assert.equal(scope.scrollSpeed,1);
scope.sample=sample(50220,{jumps:1}); scope.refreshSample(); assert.equal(starts.length,n+2); near(starts[n+1].from,scope.contentX(50220+17));
scope.sample=sample(50240,{jumps:1,key:'B'}); scope.refreshSample(); assert.equal(starts.length,n+3); near(starts[n+2].from,scope.contentX(50240+17));
// Paused: settles on the spot.
scope.sample=sample(50260,{jumps:1,key:'B',playing:false}); scope.refreshSample();
assert.equal(scope.scrollSpeed,0); assert.equal(starts[n+3].from,scope.contentX(50260)); assert.equal(starts[n+3].duration,1);
""")

    SENDER = r"""
let bodies=[], pos=0, rate=1;
globalThis.XMLHttpRequest=class {
    constructor(){this.readyState=0;}
    open(m,url){this.url=url;this.readyState=1;}
    send(body){if(!this.url.endsWith('/alive'))bodies.push(JSON.parse(body));this.readyState=4;this.status=200;}
};
globalThis.scShadowPoll=()=>{};
scFourDeckRole='1';
globalThis.scTr=path=>({
    get value(){return path.endsWith('/SampleRate')?48000:0},
    get unnormalized(){return path.endsWith('/TrackPosition')?pos:path.endsWith('BPM')?120:0},
    get state(){return path.endsWith('/SongLoaded')||path.endsWith('/PlayState')},
    get string(){return path.endsWith('/TrackUri')?'file:///a.mp3':''}, color:'#ffffff', stateChanged:{connect(){}}});
const run=ms=>{const end=now+ms;while(now<end){const dt=scShadowTickTimer.interval;now+=dt;pos+=dt*48*rate;scShadowTick();}};
// Where the peer shows the deck (the last full post carried on at pitch 0) minus where it is.
const ahead=()=>{const b=bodies[bodies.length-1][0];return b.positionMs+(now-b.at)-pos/48};
"""

    def test_sender_follows_a_jog_nudge(self):
        # The peer carries the posted line on at the pitch: a nudge must reach it while it
        # happens (it arrived in 25 ms jumps, a brake only after 180 ms).
        self.js(self.SENDER + r"""
run(6000); let n=bodies.length;
run(3000); assert(bodies.length-n<=1,'steady playback: heartbeats only'); assert.equal(scShadowTickTimer.interval,80);
rate=1.05; n=bodies.length; run(1000); assert(bodies.length-n>=3,'a jog push is posted as it happens'); assert(Math.abs(ahead())<=16,'push '+ahead());
rate=1; run(3000);
rate=0.95; n=bodies.length; run(2000); assert(bodies.length-n>=2,'a jog brake is followed');
assert(ahead()>=0&&ahead()<60,'brake '+ahead()); // of 100 ms; these positions have no audio-block lag (17 ms of it)
rate=1; run(3000); assert(Math.abs(ahead())<60); assert.equal(bodies[bodies.length-1][0].jumps,0,'a nudge is not a jump');
""")

    def test_sender_line_moves_without_steps(self):
        # The line closes in on a slower deck a little per tick: every post is one slip of
        # 15 ms, and a position that arrives one tick late does not move the line at all.
        self.js(self.SENDER + r"""
const shifts=from=>bodies.slice(from).map((b,i,a)=>i?b[0].positionMs-a[i-1][0].positionMs-(b[0].at-a[i-1][0].at):0);
run(6000); let n=bodies.length;
rate=0.95; run(2000); rate=1; run(2000);
assert(bodies.length-n>=4); assert(shifts(n).every(s=>Math.abs(s)<22),'brake '+shifts(n));
run(6000); n=bodies.length; const late=pos; now+=80; scShadowTick(); pos=late+80*48; run(3000);
assert(bodies.length-n<=1,'a late position is not a slip'); assert(shifts(n).every(s=>Math.abs(s)<2),'late '+shifts(n));
""")

    def test_receiver_holds_the_line_on_a_fast_animation_clock(self):
        # The players' panel runs at 59.34 Hz and Qt steps Animators by 1/59 s a frame:
        # 0.57 % fast, 3.4 s in ten minutes of an unsteered scroll.
        self.js(self.RECEIVER + self.layer_js() + r"""
fast=1.0057;
const t0=now, model=()=>10000+(now-t0)*1.0312;
scope.sample=sample(10000,{pitch:3.12}); scope.refreshSample();
let worst=0;
for(let i=1;i<=3000;i++){ // ten minutes of the 200 ms clock
    now+=200; scShadowLive.seen=now; // unchanged replies keep the sample alive
    if(i%25===0){scope.sample=sample(model(),{pitch:3.12}); scope.refreshSample();} // a full reply every 5 s
    scope.refreshPosition(true);
    if(i===1500){scope.span=4000; scope.restartScroll(); near(shown(),model(),0.01);} // zoom: on the line at once
    worst=Math.max(worst,Math.abs(shown()-model()));
}
assert(worst<3,'worst '+worst); near(shown(),model(),3); // minute 10 as minute 1
assert.equal(starts.length,3002);
""")


    def test_receiver_carries_the_position_on_at_the_measured_rate(self):
        # stream-sync measures how fast the other player's deck really runs (its audio
        # clock is -165 ppm on ours, its pitch is rounded): `rate` in the reply.
        self.js(r"""
const deck=(pos,extra)=>Object.assign({deck:2,key:'A',playing:true,positionMs:pos,at:now,pitch:3.12},extra||{});
scShadowApplyDisplay([deck(1000)]);
let s=scShadowData(2); assert.equal(s.rate,0); assert.equal(scShadowRate(s),1.0312); // none yet: by the pitch
now+=60000; scShadowLive.seen=now;
// The line is 10 ms behind the pitch after a minute: the same sample takes the rate.
scShadowApplyDisplay([deck(1000+60000*1.0312-10,{rate:1.031028})]);
assert.strictEqual(scShadowData(2),s); assert.equal(s.rate,1.031028); assert.equal(scShadowRate(s),1.031028);
const at=now, pos=s.positionMs;
now+=600000; scShadowLive.seen=now;
assert(Math.abs(scShadowPosition(s,now)-(pos+600000*1.031028))<1e-6); // ten minutes on: by the rate, 103 ms short of the pitch
// A reply on that line is the same picture; one on the pitch's line is not.
scShadowApplyDisplay([deck(pos+600000*1.031028+3,{rate:1.031028})]); assert.strictEqual(scShadowData(2),s);
scShadowApplyDisplay([deck(pos+600000*1.031028+50,{rate:1.031028})]); assert.notStrictEqual(scShadowData(2),s);
assert.equal(scShadowData(2).rate,1.031028);
""")

    def test_shadow_lane_takes_the_stock_main_colours_and_grid(self):
        # User report 11-10: the lane looked translucent. The stock height*Color set
        # is the Fade one (#73ffffff, #9908f302, #800759d5 on a player).
        layer = bp.shadow_layer("import QtQuick 2.0\nItem {\n    id: w\n}\n", "waveform")
        for band in ("Low", "Mid", "High"):
            self.assertIn(f'{band.lower()}Color: (!rgb && typeof owner.color{band}Color !== "undefined" ? owner.color{band}Color : typeof owner.height{band}Color', layer)
        self.assertNotIn("owner.colorLowColor", bp.shadow_layer("import QtQuick 2.0\nItem {\n    id: w\n}\n", "overview"))
        self.assertIn('color: wave ? "black" : "#101114"', layer)
        # Stock grid: edge ticks on every beat; only a bar has the line and its number.
        self.assertNotIn("0.55 : 0.22", layer)
        self.assertIn('Rectangle { visible: scBeat.bar; x: 1; y: 8; width: 2; height: scBeat.height - 16; color: "#b7b7b7" }', layer)
        self.assertIn('text: scBeat.bar ? scBeat.beat.number / 4 + 1 : ""', layer)

    def test_shadow_layer_restores_the_stock_visible_binding_not_a_value(self):
        # 11-10, Engine mode: no waveform on the shadow decks. Binding read the
        # effective visible (false, view off screen) and wrote it back on exit.
        layer = bp.shadow_layer("import QtQuick 2.0\nItem {\n    id: w\n}\n", "waveform")
        self.assertIn('property: "visible"; value: true; when: scDisplay.active; restoreMode: Binding.RestoreBinding }', layer)


# Keep this workstream's regression methods together at the end to avoid
# overlapping the existing UI and shadow tests during integration.
class DeckHeaderLayoutTest(unittest.TestCase):
    js = ShadowDisplayTest.js

    def test_countdown_has_a_bounded_slot_and_elides_only_its_name(self):
        cap = bp.cue_capsule("", "1", 22, 16)
        self.assertIn("Layout.maximumWidth: 96", cap)
        self.assertIn("elide: Text.ElideRight", cap)
        self.assertIn("Reserve the Cue slot even without a cue", cap)

    def test_performance_header_preserves_sync_row_and_stock_bpm_formatter(self):
        qrc = LAB / "5.3.0" / "qrc"
        if not qrc.is_dir():
            self.skipTest(f"{qrc} not found (run on Oracle-Batam-arm)")
        with tempfile.TemporaryDirectory(prefix="ui-header-test-") as tmp:
            packs = Path(tmp) / "packs"
            result = subprocess.run(["python3", str(BUILD_PACKS), "--qrc", str(qrc), "--out", str(packs)],
                                    capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            composite = "Skins/EngineOS/UI/CompositeControls/"
            for lang in ("en", "zh-Hans", "zh-Hant"):
                with self.subTest(lang=lang):
                    base = packs / lang
                    bpm = (base / "Skins/Embedded/Items/Bpm.qml").read_text()
                    self.assertIn("property int decimalPlaces: 1", bpm)  # stock default for stock users
                    self.assertIn('property string fractionSeparator: ""', bpm)
                    self.assertIn("bpmValue.toFixed(bpm.decimalPlaces)", bpm)
                    self.assertIn('padEnd(bpm.decimalPlaces, "0")', bpm)
                    info = (base / composite / "Deck/DeckItem/BpmInfo.qml").read_text()
                    # Number format only: stock's Row and its own text for "-" / unanalysed stay.
                    self.assertNotIn("EmbeddedItems.Bpm {", info)
                    self.assertIn("unnormalized.toFixed(2)", info)
                    self.assertIn("scAnalyzed.translator.state", info)
                    self.assertIn("bpmInfo.scBpm2 ? bpmInfo.scBpm2[0] : bpmInfoView && bpmInfoView.bpmText", info)
                    self.assertIn(': bpmInfoView && bpmInfoView.bpmFractionalText', info)
                    self.assertIn("topPadding: bpmInfo.fractionTextTopPadding", info)
                    self.assertIn("onBpmClicked()", info)
                    performance = (base / composite / "DeckPerformanceData/DeckPerformanceDataContent.qml").read_text()
                    self.assertIn("id: autoLoop\n        Layout.rightMargin: 4", performance)
                    for name, text in (
                        ("Performance", performance),
                        ("GroupedMini", (base / composite / "DeckTrackData/GroupedMiniDeckTrackDataContent.qml").read_text()),
                        ("SplitMini", (base / composite / "DeckTrackData/SplitMiniDeckTrackDataContent.qml").read_text()),
                    ):
                        self.assertEqual(text.count('objectName: "EngineX_CueCountdown"'), 1)
                        cue = text.index('objectName: "EngineX_CueCountdown"')
                        self.assertLess(text.index("AutoLoop {"), text.index("BeatJump {"))
                        self.assertLess(text.index("BeatJump {"), text.index("JP07SyncIndicator {"))
                        self.assertLess(text.index("JP07SyncIndicator {"), cue)
                        self.assertLess(cue, text.index("SpeedInfo {"))
                        if name == "SplitMini":
                            self.assertIn("Layout.minimumWidth: 160", text)
                    # The actual time controls (elapsed/remaining and flip touch
                    # target) remain byte-for-byte stock in all four variants.
                    for name in ("GroupedMain", "SplitMain", "GroupedMini", "SplitMini"):
                        rel = composite + f"DeckTrackData/{name}DeckTrackDataContent.qml"
                        stock = (qrc / rel).read_text()
                        text = (base / rel).read_text() if (base / rel).exists() else stock
                        pattern = r"(?m)^( +)DeckInfoTimeInfo \{.*?^\1\}"
                        self.assertEqual(re.search(pattern, text, re.S).group(), re.search(pattern, stock, re.S).group())
                    # Conserving the mini row's total fixed width preserves the
                    # stock flexible sync bar, including when Cue is visible.
                    mini = (base / composite / "DeckTrackData/GroupedMiniDeckTrackDataContent.qml").read_text()
                    def preferred(kind, text):
                        block = re.search(kind + r" \{(.*?)^                \}", text, re.S | re.M).group(1)
                        return int(re.search(r"Layout.preferredWidth: (\d+)", block).group(1))
                    stock = (qrc / composite / "DeckTrackData/GroupedMiniDeckTrackDataContent.qml").read_text()
                    cue_width = int(re.search(r"Layout.preferredWidth: (\d+)", bp.cue_capsule("", "1", 22, 16)).group(1))
                    self.assertEqual(preferred("AutoLoop", stock) + preferred("BeatJump", stock) + 6 * 16,
                                     preferred("AutoLoop", mini) + preferred("BeatJump", mini) + cue_width + 7 * 4)
                    jump = (base / composite / "Deck/DeckItem/BeatJump.qml").read_text()
                    forward = jump.index('objectName: "Deck_BeatJump_Forward"')
                    self.assertLess(jump.index('objectName: "Deck_BeatJump_Text"'), forward)
                    self.assertLess(forward, jump.index("id: beatJumpTextAnimation"))
                    for action in ("JumpBack", "JumpForward", "BeatJumpIndex"):
                        self.assertIn("/Track/BeatJump/" + action, jump)
                    self.assertNotIn("anchors.margins: -6", jump)
                    self.assertIn("rightPadding: 14", jump)
                    self.assertIn("EngineXEmbedded.Bpm {", performance)
                    self.assertIn("s.currentBpm ||", performance)
                    self.assertNotIn(".toFixed(1)", bp.SHADOW_LAYER)
            if shutil.which("node"):
                # Execute the stock formatter expression with its new default,
                # including trailing zeroes and an exact fractional tempo.
                expression = re.search(r"(bpmValue\.toFixed\(bpm\.decimalPlaces\)\.split\(\"\.\"\))", bpm).group(1)
                script = 'const bpm={decimalPlaces:2}; const values=[128,174.25]; console.log(JSON.stringify(values.map(bpmValue=>' + expression + ')));'
                output = subprocess.check_output(["node", "-e", script], text=True)
                self.assertEqual(json.loads(output), [["128", "00"], ["174", "25"]])

    @unittest.skipUnless(shutil.which("node"), "node needed for the display-state behavioral checks")
    def test_shadow_preserves_exact_tempo_and_applies_tempo_only_changes(self):
        value = re.search(r"bpmValue: \{(.*?)\n\s+\}", bp.SHADOW_LAYER, re.S).group(1)
        self.js(r"""
const entry={deck:2,key:'tempo',bpm:128,pitch:1.7,currentBpm:130.17};
scShadowApplyDisplay([entry]);
const first=scShadowData(2);
assert.equal(first.currentBpm,130.17);
const scDisplay={sample:first,meta:null};
const value=()=>{ """ + value + r""" };
assert.equal(value().toFixed(2),'130.17');
assert.equal((128*1.017).toFixed(2),'130.18'); // rounded pitch is insufficient
now+=500;scShadowApplyDisplay([entry]);
assert.strictEqual(scShadowData(2),first); // no repaint for a heartbeat
scShadowApplyDisplay([{...entry,currentBpm:130.18}]);
assert.notStrictEqual(scShadowData(2),first);
scDisplay.sample=scShadowData(2);
assert.equal(value().toFixed(2),'130.18');
// Sources without a native current tempo retain the pitch-derived fallback.
scShadowApplyDisplay([{...entry,currentBpm:undefined}]);
scDisplay.sample=scShadowData(2);
assert.equal(value().toFixed(2),'130.18');
""")

    @unittest.skipUnless(shutil.which("node"), "node needed for the display-state behavioral checks")
    def test_sender_posts_exact_tempo_changes_at_the_same_rounded_pitch(self):
        self.js(r"""
let current=130.17,bodies=[],pings=0;
globalThis.XMLHttpRequest=class {
    constructor(){this.readyState=0;}
    open(m,url){this.url=url;this.readyState=1;}
    send(body){if(this.url.endsWith('/alive'))pings++;else bodies.push(JSON.parse(body));this.readyState=4;this.status=200;}
};
globalThis.scShadowPoll=()=>{};
scFourDeckRole='1';
globalThis.scTr=path=>({
    value:path.endsWith('/SampleRate')?48000:0,
    get unnormalized(){return path.endsWith('/OriginalBPM')?128:path.endsWith('/CurrentBPM')?current:0},
    state:path.endsWith('/SongLoaded'),
    string:path.endsWith('/TrackUri')?'file:///tempo.mp3':path.endsWith('/SpeedState')?'1.70%':'',color:'#ffffff'});
scShadowTick();assert.equal(bodies.length,1);
assert.equal(bodies[0][0].currentBpm,130.17);assert.equal(bodies[0][0].pitch,1.7);
current=130.18;now+=520;scShadowTick();
assert.equal(bodies.length,2,'tempo-only change must not wait for the five-second full post');
assert.equal(bodies[1][0].currentBpm,130.18);assert.equal(bodies[1][0].pitch,1.7);
now+=520;scShadowTick();assert.equal(bodies.length,2);assert.equal(pings,1);
""")


@unittest.skipUnless(shutil.which("node"), "node needed for the follower behavioral checks")
class ShadowFollowerBendTest(unittest.TestCase):
    """Engine mode: the shadow deck plays at the source's exact tempo and closes a gap with PITCH BEND, not a seek."""

    def test_gap_below_the_seek_limit_is_bent_closed_at_the_exact_tempo(self):
        ShadowDisplayTest.js(self, r"""
const T0 = now, props = new Map();
let ahead = 80, seeks = 0;
globalThis.scShadowBent = null;
globalThis.scShadowBendTimer = {interval: 0, running: false, restart() { this.running = true }, stop() { this.running = false }};
globalThis.scTr = path => {
    let p = props.get(path);
    if (p) return p;
    const deck2 = path.startsWith('/Engine/Deck2/');
    p = {state: deck2 && (path.endsWith('/SongLoaded') || path.endsWith('/PlayState')), string: path.endsWith('/SpeedRange') ? '8' : path.endsWith('/SyncMode') ? 'Off' : '',
        entries: ['4', '8', '10', '20', '50', '100'], index: 1, url: '',
        _v: path.endsWith('/SampleRate') ? 44100 : path.endsWith('/Speed') ? 0.5 : 0,
        get value() { return this._v }, set value(v) { this._v = v; if (path.endsWith('/SetSongPosition')) seeks++ },
        // Engine moves TrackPosition in audio blocks: 18 ms old on average
        get unnormalized() { return path.endsWith('/TrackLength') ? 44100 * 400 : path.endsWith('/TrackPosition') ? (Math.floor((now - T0) / 36) * 36 + 5000 + ahead) * 44.1 : 0 }};
    props.set(path, p);
    return p;
};
let seq = 0;
const apply = () => scShadowApplyAll([{deck: 2, path: '/data/enginex/shadow-v3/peer-1/a.flac', playing: true, link: '{l}', sync: true,
    positionMs: now - T0 + 5000, at: now, pitch: 0.01, bpm: 138, currentBpm: 138.0089, jumps: 0, seq: ++seq}], false);
const run = ms => { for (let t = 0; t < ms; t += 40) { now += 40; scShadowLine(now); if (t % 520 === 0) apply() } };
const back = scTr('/Engine/Deck2/Track/NudgeBackward'), fwd = scTr('/Engine/Deck2/Track/NudgeForward');
apply();
assert.ok(scTr('/Client/Librarian/AddToDatabaseAndLoadToDeck2').url.endsWith('/a.flac'));
run(1960);
assert.equal(back.state, false, 'no bend before the line is measured');
run(2600);
assert.equal(back.state, true, '80 ms ahead: bend back');
assert.equal(fwd.state, false);
assert.equal(seeks, 0, 'no seek for a gap below the limit');
assert.ok(Math.abs(scShadowBendTimer.interval - 80 * 0.8 / 0.07) < 40, 'bend time ' + scShadowBendTimer.interval);
assert.ok(Math.abs(scTr('/Engine/Deck2/Speed').value - (0.5 + (138.0089 / 138 - 1) * 100 / 16)) < 1e-9, 'fader at the exact tempo ratio');
assert.equal(scTr('/Engine/Deck2/SyncMode').string, 'Off', 'a linked shadow deck is never synced by us');
ahead = 4; // what the bend left
scShadowBendEnd();
assert.equal(back.state, false);
run(4000);
assert.equal(back.state || fwd.state, false, 'within 6 ms: left alone');
assert.equal(seeks, 0);
ahead = -40; run(5200);
assert.equal(fwd.state, true, '40 ms behind: bend forward');
scShadowApplyAll([{deck: 2, path: '/data/enginex/shadow-v3/peer-1/a.flac', playing: false, link: '{l}', positionMs: now - T0 + 5000, at: now, pitch: 0.01, bpm: 138, currentBpm: 138.0089, jumps: 0, seq: ++seq}], false);
assert.equal(fwd.state, false, 'a pause releases the bend');
""")


class ShadowIdentityTest(unittest.TestCase):
    """Engine mode: the shadow deck holds the source's track, or is loaded again (maintainer report 11-10)."""
    js = ShadowDisplayTest.js

    # An Engine deck 2 that loads what it is told once settle() runs, as Engine does a little later.
    ENGINE = r"""
scShadowDisplayOnly = false; scFourDeckRole = '13';
globalThis.scShadowBent = null; // no bend runs here: the fake deck's line is never sampled
const lib = {
    '/media/USB/Contents/a.flac': {title: 'A', lenMs: 300000, bpm: 128},
    '/media/USB/Contents/b.flac': {title: 'B', lenMs: 200000, bpm: 140},
    '/media/USB/Contents/next.flac': {title: 'Next', lenMs: 456800, bpm: 149.3},
    '/data/enginex/shadow-v3/peer-1/same.flac': {title: 'Same', lenMs: 240000, bpm: 120},
    '/data/enginex/shadow-v3/peer-2/same.flac': {title: 'Same', lenMs: 250000, bpm: 120},
};
const eng = {held: '', loading: false, playing: false, pos: 0, pending: [], asked: [], plays: [], seeks: [], autoplay: false, lock: false, fail: 0, slow: 0, named: true};
const track = () => lib[eng.held] || {title: '', lenMs: 0, bpm: 0};
const netName = f => f.startsWith('/media/') ? 'net://uuid/' + f.split('/')[2] + ' (USB 1)/' + f.split('/').slice(3).join('/') : 'net://uuid/(Unknown)/' + f.split('/').slice(2).join('/');
const ask = url => {
    const f = decodeURI(url.replace('file://', ''));
    eng.asked.push(f);
    if (eng.lock && eng.playing) return; // Lock Playing Deck
    if (eng.fail > 0) { eng.fail--; return; }
    eng.loading = true; eng.pending.push(f);
};
// Engine finishes its loads; the newest by default, `order` picks another one (two loads racing).
const settle = order => {
    if (eng.slow > 0) { eng.slow--; return; }
    if (!eng.pending.length) return;
    eng.held = eng.pending[order === undefined ? eng.pending.length - 1 : order];
    eng.pending = []; eng.loading = false; eng.playing = false; eng.pos = 0;
};
const sr = 44100;
globalThis.scTr = path => {
    const tail = path.replace('/Engine/Deck2', '');
    if (path === '/Client/Librarian/AddToDatabaseAndLoadToDeck2') return {set url(v) { ask(v); }};
    if (path === '/Client/Preferences/Profile/Application/AutoPlay') return {state: eng.autoplay};
    if (!path.startsWith('/Engine/Deck2/')) return null;
    const t = track();
    return {
        get state() { return tail === '/Track/SongLoaded' ? !!eng.held : tail === '/Track/SongLoading' ? eng.loading : tail === '/PlayState' ? eng.playing : false; },
        set state(v) { if (tail === '/Play') { eng.playing = !!v; eng.plays.push(!!v); } },
        get string() { return tail === '/Track/TrackNetworkPath' ? (eng.held && eng.named ? netName(eng.held) : '') : tail === '/Track/SongName' ? t.title : tail === '/SyncMode' ? 'Off' : tail === '/SpeedRange' ? '8' : ''; },
        get value() { return tail === '/Track/SampleRate' ? (eng.held ? sr : 0) : tail === '/Speed' ? 0.5 : 0; },
        set value(v) { if (tail === '/Track/SetSongPosition') { eng.pos = v * t.lenMs; eng.seeks.push(v); } },
        get unnormalized() { return tail === '/Track/TrackLength' ? t.lenMs * sr / 1000 : tail === '/Track/TrackPosition' ? eng.pos * sr / 1000 : tail === '/OriginalBPM' ? t.bpm : 0; },
    };
};
const src = (path, extra) => Object.assign({deck: 2, path, id: path, title: lib[path].title, durationMs: lib[path].lenMs, bpm: lib[path].bpm, playing: false, positionMs: 0, pitch: 0, at: now, jumps: 0}, extra || {});
// The source's state, applied every 180 ms as the long poll does.
const run = (ms, e) => { for (const end = now + ms; now < end;) { now += 180; scShadowApply(Object.assign({}, e, {at: now})); } };
const A = '/media/USB/Contents/a.flac', B = '/media/USB/Contents/b.flac', NEXT = '/media/USB/Contents/next.flac';
"""

    def test_quick_a_b_a_ends_on_a_whatever_order_engine_loads_in(self):
        self.js(self.ENGINE + r"""
scShadowApply(src(A)); now += 60; scShadowApply(src(B)); now += 60; scShadowApply(src(A));
assert.deepEqual(eng.asked, [A, B, A]);
settle(1); // Engine finished B last
assert.equal(eng.held, B);
run(2000, src(A, {playing: true, positionMs: 5000}));
assert.equal(eng.asked.length, 3); assert.deepEqual(eng.plays, []); assert.deepEqual(eng.seeks, []); // B is never steered as A
assert.equal(scShadowAt[2].ready, false);
run(1500, src(A, {playing: true, positionMs: 5000})); // 3 s after the last load: asked again
assert.deepEqual(eng.asked, [A, B, A, A]); assert.equal(scShadowAt[2].why, 'length');
settle();
run(1500, src(A, {playing: true, positionMs: 5000}));
assert.equal(eng.held, A); assert.equal(eng.playing, true); assert.equal(scShadowAt[2].ready, true);
assert.equal(eng.asked.length, 4);
run(5000, src(A, {playing: true, positionMs: 5000})); // and stays: no further loads
assert.equal(eng.asked.length, 4); assert.equal(scShadowAt[2].tries, 0);
""")

    def test_same_path_or_same_name_is_told_apart_by_the_source_id(self):
        self.js(self.ENGINE + r"""
// One path, the source loaded another file there (a new key): a new load.
scShadowApply(src(A, {id: 'key-1'})); settle(); run(1500, src(A, {id: 'key-1'}));
assert.equal(scShadowAt[2].ready, true); assert.equal(eng.asked.length, 1);
run(400, src(A, {id: 'key-2'}));
assert.deepEqual(eng.asked, [A, A]); assert.equal(scShadowAt[2].id, 'key-2');
settle(); run(1500, src(A, {id: 'key-2'})); assert.equal(scShadowAt[2].ready, true); assert.equal(eng.asked.length, 2);
// No id from an older Go: the path alone, as before.
run(400, Object.assign(src(B), {id: undefined})); assert.equal(eng.asked[2], B); assert.equal(scShadowAt[2].id, B);
// Two copies named same.flac: Engine holding the first is not the second.
const S1 = '/data/enginex/shadow-v3/peer-1/same.flac', S2 = '/data/enginex/shadow-v3/peer-2/same.flac';
settle(); run(400, src(S1, {id: 'k1'})); settle(); run(1500, src(S1, {id: 'k1'})); assert.equal(scShadowAt[2].ready, true);
const n = eng.asked.length;
eng.fail = 1; // Engine drops the load of the second and keeps the first
run(1400, src(S2, {id: 'k2'}));
assert.equal(eng.asked.length, n + 1); assert.equal(eng.held, S1); assert.equal(scShadowAt[2].ready, false);
assert.equal(scShadowWrong(2, src(S2, {id: 'k2'})), 'length');
run(2000, src(S2, {id: 'k2'})); settle(); run(1500, src(S2, {id: 'k2'}));
assert.equal(eng.held, S2); assert.equal(scShadowAt[2].ready, true);
// The data directory under its old name is the same file.
eng.held = S2; assert.equal(scShadowWrong(2, src(S2)), '');
assert.equal(scShadowWrong(2, Object.assign(src(S2), {path: '/data/sc5000-custom/shadow-v3/peer-2/same.flac'})), '');
""")

    def test_engine_load_failure_is_asked_again_with_backoff_and_never_steered(self):
        self.js(self.ENGINE + r"""
eng.fail = 3;
const e = src(A, {playing: true, positionMs: 9000});
scShadowApply(e); const t0 = now;
const times = [];
let seen = 1;
for (let i = 0; i < 400 && eng.fail > 0; i++) { run(180, e); if (eng.asked.length > seen) { seen = eng.asked.length; times.push(Math.round((now - t0) / 1000)); } }
assert.deepEqual(times, [3, 9]); // 3 s, then 6 s later
assert.deepEqual(eng.plays, []); assert.deepEqual(eng.seeks, []); assert.equal(scShadowAt[2].why, 'empty');
run(13000, e); assert.equal(eng.asked.length, 4); // 12 s later: this one loads
settle(); run(1500, e);
assert.equal(eng.held, A); assert.equal(eng.playing, true); assert.equal(scShadowAt[2].tries, 0);
// Engine is slow, not failing: no second request while it is loading.
eng.slow = 1e9; const n = eng.asked.length;
run(400, src(B)); run(20000, src(B));
assert.equal(eng.asked.length, n + 1); assert.equal(eng.loading, true);
eng.slow = 0; settle(); run(400, src(B)); assert.equal(scShadowAt[2].ready, true); assert.equal(eng.asked.length, n + 1);
""")

    def test_lock_playing_deck_stops_the_deck_and_loads_again(self):
        self.js(self.ENGINE + r"""
eng.lock = true;
scShadowApply(src(A)); settle(); run(1500, src(A, {playing: true, positionMs: 1000}));
assert.equal(eng.playing, true);
// The source changes track while the shadow deck plays: Engine refuses, the deck is stopped first.
const e = src(B, {playing: true, positionMs: 0});
scShadowApply(e);
assert.equal(eng.playing, false); assert.deepEqual(eng.asked, [A, B]); assert.equal(eng.pending.length, 1);
// A deck that was still playing when asked (the stop took a frame) is asked again after 1.5 s.
eng.pending = []; eng.loading = false; eng.held = A;
run(1300, e); assert.equal(eng.asked.length, 2); assert.equal(eng.playing, false); // A stays stopped meanwhile
run(400, e); assert.deepEqual(eng.asked, [A, B, B]);
settle(); run(1500, e); assert.equal(eng.held, B); assert.equal(eng.playing, true);
""")

    def test_track_swapped_by_continue_is_found_stopped_and_loaded_again(self):
        self.js(self.ENGINE + r"""
const e = src(A, {playing: true, positionMs: 100000});
scShadowApply(e); settle(); run(1500, e);
assert.equal(scShadowAt[2].ready, true); assert.equal(eng.playing, true);
// 11-10 on the right player: Engine's Continue loaded and played the next track.
eng.held = NEXT; eng.pos = 0; eng.playing = true; eng.plays.length = 0; eng.seeks.length = 0;
run(180, e);
assert.equal(eng.playing, false); assert.deepEqual(eng.plays, [false]); assert.deepEqual(eng.seeks, []); // stopped, never steered
assert.deepEqual(eng.asked, [A, A]); assert.equal(scShadowAt[2].why, 'length');
settle(); run(1500, e);
assert.equal(eng.held, A); assert.equal(eng.playing, true); assert.equal(scShadowAt[2].ready, true);
// Another file of the same length and tempo: Engine's name for it and the title tell.
lib['/media/USB/Contents/same-length.flac'] = {title: 'Other', lenMs: 300000, bpm: 128};
eng.held = '/media/USB/Contents/same-length.flac'; eng.playing = true;
run(1200, e); assert.equal(scShadowAt[2].why, 'file'); assert.equal(eng.asked.length, 3);
settle(); run(1500, e); assert.equal(eng.held, A);
// Engine gives no path: the length tells.
eng.named = false; eng.held = NEXT; eng.playing = true;
run(180, e); assert.equal(scShadowAt[2].why, 'length'); assert.equal(eng.asked.length, 4);
settle(); run(1500, e); assert.equal(eng.held, A);
// A swap while paused, same length: found within a second by title and BPM.
lib['/media/USB/Contents/twin.flac'] = {title: 'Twin', lenMs: 300000, bpm: 99};
eng.held = '/media/USB/Contents/twin.flac';
run(1200, e); assert.equal(scShadowAt[2].why, 'title'); assert.equal(eng.asked.length, 5);
""")

    def test_title_or_bpm_alone_and_a_named_file_do_not_load_again(self):
        self.js(self.ENGINE + r"""
scShadowApply(src(A)); settle(); run(1500, src(A));
assert.equal(scShadowWrong(2, src(A)), '');
// Engine names our file: nothing else is asked, whatever the source reports.
assert.equal(scShadowWrong(2, src(A, {title: 'rekordbox title', bpm: 64, durationMs: 111})), '');
eng.named = false; // no path from Engine
assert.equal(scShadowWrong(2, src(A, {title: 'Other title'})), ''); // a CDJ's rekordbox title
assert.equal(scShadowWrong(2, src(A, {bpm: 64})), ''); // half tempo
assert.equal(scShadowWrong(2, src(A, {bpm: 150})), ''); // another analysis
assert.equal(scShadowWrong(2, src(A, {title: 'Other', bpm: 150})), 'title');
assert.equal(scShadowWrong(2, src(A, {durationMs: 296000})), 'length');
assert.equal(scShadowWrong(2, src(A, {durationMs: 301500})), '');
assert.equal(scShadowWrong(2, src(A, {durationMs: 0, title: '', bpm: 0})), ''); // an older peer: nothing to compare
eng.held = ''; assert.equal(scShadowWrong(2, src(A)), 'empty');
// Checked once a second, by one number in between.
eng.held = A; eng.named = true;
let strings = 0; const tr = scTr;
globalThis.scTr = p => { const o = tr(p); if (p.endsWith('/TrackNetworkPath')) strings++; return o; };
run(18000, src(A)); assert.ok(strings >= 17 && strings <= 19, strings); assert.equal(eng.asked.length, 1);
""")

    def test_continue_on_parks_the_shadow_deck_short_of_its_end(self):
        self.js(self.ENGINE + r"""
const e = src(A, {playing: true, positionMs: 100000});
scShadowApply(e); settle(); run(1500, e);
eng.autoplay = true; eng.seeks.length = 0;
// The source plays out: the shadow deck stops 700 ms before its end and stays loaded.
const end = src(A, {playing: true, positionMs: 299900});
run(1000, end);
assert.equal(eng.playing, false); assert.equal(eng.held, A); assert.equal(eng.asked.length, 1);
assert.ok(eng.seeks.length >= 1); for (const v of eng.seeks) assert.ok(v <= 299300 / 300000 + 1e-9, v);
assert.ok(Math.abs(eng.pos - 299300) < 1);
const n = eng.seeks.length; run(3000, end); assert.equal(eng.seeks.length, n); assert.equal(eng.playing, false); // parked, not seeking on
// The source jumps back: the deck follows again.
run(1500, src(A, {playing: true, positionMs: 50000, jumps: 1}));
assert.equal(eng.playing, true); assert.ok(eng.pos < 60000);
// A seek asked past the end is held back too.
scShadowSeek(2, scShadowAt[2], 400000); assert.ok(Math.abs(eng.pos - 299300) < 1);
// The shadow deck itself ran to the gap ahead of the source: stopped before Continue fires.
eng.pos = 299400; eng.playing = true; eng.plays.length = 0; run(180, src(A, {playing: true, positionMs: 298000}));
assert.equal(eng.plays[0], false); assert.ok(eng.pos < 299300); // and lined up with the source again
// Continue off: as before, to the very end.
eng.autoplay = false; scShadowSeek(2, scShadowAt[2], 400000); assert.equal(eng.pos, 300000);
""")


class ShadowRangeTest(unittest.TestCase):
    """Engine mode: the shadow deck takes the source's pitch range and key lock (maintainer report 11-10)."""

    def test_shadow_deck_takes_the_sources_range_and_key_lock(self):
        ShadowDisplayTest.js(self, r"""
scShadowDisplayOnly = false; scFourDeckRole = '13';
globalThis.scShadowBent = null;
const props = new Map();
globalThis.scTr = path => {
    let p = props.get(path);
    if (p) return p;
    p = {state: path.endsWith('/SongLoaded'), entries: ['4', '8', '10', '20', '50', '100'], index: 1, url: '', value: path.endsWith('/Speed') ? 0.5 : 0,
        get string() { return path.endsWith('/SpeedRange') ? this.entries[this.index] : path.endsWith('/SyncMode') ? 'Off' : '' },
        get unnormalized() { return path.endsWith('/TrackLength') ? 44100 * 400 : path.endsWith('/SampleRate') ? 44100 : 0 }};
    if (path.endsWith('/SampleRate')) p.value = 44100;
    props.set(path, p);
    return p;
};
let seq = 0;
const apply = more => { now += 2000; scShadowApplyAll([Object.assign({deck: 2, path: '/data/enginex/shadow-v3/peer-1/a.flac', playing: false, link: '{l}',
    positionMs: 0, at: now, pitch: 0.01, bpm: 138, currentBpm: 138.0138, jumps: 0, seq: ++seq}, more)], false) };
const range = scTr('/Engine/Deck2/SpeedRange'), speed = scTr('/Engine/Deck2/Speed'), lock = scTr('/Engine/Deck2/Track/KeyLock');
apply({range: 100, keylock: true}); // loads
apply({range: 100, keylock: true});
assert.equal(range.string, '100', "the source's range");
assert.ok(Math.abs(speed.value - (0.5 + 0.01 / 200)) < 1e-9, 'fader at +0.01 % of 100: ' + speed.value);
assert.equal(lock.state, true);
apply({range: 8, keylock: false});
assert.equal(range.string, '8', 'the source went back to 8');
assert.ok(Math.abs(speed.value - (0.5 + 0.01 / 16)) < 1e-9);
assert.equal(lock.state, false);
lock.state = true;
apply({}); // a CDJ or an older sender: no range, no key lock sent
assert.equal(range.string, '8');
assert.equal(lock.state, true, 'left alone');
apply({range: 8, pitch: 11.1, currentBpm: 153.318}); // synced past its own range: widened, never stuck at the end stop
assert.equal(range.string, '20');
// The sender reads both with the Sync state (5 Hz) and posts on a change.
const own = {'/SpeedRange': {string: '50'}, '/Track/KeyLock': {state: true}};
scShadowDeckProperties = {};
globalThis.scTr = path => { for (const k in own) if (path === '/Engine/Deck1' + k) return own[k]; return null };
const g = scShadowProperties(1);
assert.equal(parseFloat(g.range.string), 50);
assert.equal(g.keylock.state, true);
""")
        src = bp.SERVICES_MAIN
        self.assertIn("s.range = p && p.rangeIndex === s.rangeIndex ? p.range : parseFloat(g.range.string) || 0", src)
        self.assertIn("o.range = s.range; o.keylock = s.keylock", src)
        self.assertIn("s.range !== p.range || s.keylock !== p.keylock) edge = true", src)
        self.assertIn("sent.range !== o.range || sent.keylock !== o.keylock", src)


if __name__ == "__main__":
    unittest.main()


@unittest.skipUnless(shutil.which("node"), "node needed for the follower behavioral checks")
class ShadowFollowerNoiseTest(unittest.TestCase):
    """Engine mode: raw positions as two SC5000 gave them (rec10-a) must not bend an aligned shadow deck."""

    def test_aligned_deck_is_left_alone_and_a_real_gap_is_closed(self):
        ShadowDisplayTest.js(self, r"""
const T0 = now, props = new Map();
let ahead = 0, tick = 0, bends = 0;
globalThis.scShadowBent = null;
// A bend moves the deck by 7 % of the time it is held.
globalThis.scShadowBendTimer = {interval: 0, running: false, stop() { this.running = false },
    restart() { this.running = true; bends++; ahead += this.interval * 0.07 * (scTr('/Engine/Deck2/Track/NudgeForward').state ? 1 : -1) }};
// Raw positions read 9 ms either side of their mean, one in sixty 22 ms ahead.
const jitter = () => tick % 61 === 60 ? 22 : tick % 7 < 3 ? 9 : -9;
globalThis.scTr = path => {
    let p = props.get(path);
    if (p) return p;
    const deck2 = path.startsWith('/Engine/Deck2/');
    p = {state: deck2 && (path.endsWith('/SongLoaded') || path.endsWith('/PlayState')), string: path.endsWith('/SpeedRange') ? '8' : path.endsWith('/SyncMode') ? 'Off' : '',
        entries: ['4', '8', '10', '20', '50', '100'], index: 1, url: '',
        _v: path.endsWith('/SampleRate') ? 44100 : path.endsWith('/Speed') ? 0.5 : 0,
        get value() { return this._v }, set value(v) { this._v = v },
        get unnormalized() { return path.endsWith('/TrackLength') ? 44100 * 400 : path.endsWith('/TrackPosition') ? (now - T0 + 5000 + ahead + jitter()) * 44.1 : 0 }};
    props.set(path, p);
    return p;
};
let seq = 0;
// The source's line: its newest block, 18 ms ahead of the mean of its raw positions, +-6 ms.
const apply = () => scShadowApplyAll([{deck: 2, path: '/data/enginex/shadow-v3/peer-1/a.flac', playing: true, link: '{l}',
    positionMs: now - T0 + 5000 + 18 + 3 * Math.sin(seq / 9) + 3 * Math.sin(seq / 1.5), at: now, pitch: 0, bpm: 131, currentBpm: 131, jumps: 0, seq: ++seq}], false);
const run = ms => { for (let t = 0; t < ms; t += 200) { now += 200; tick++; if (scShadowBendTimer.running) scShadowBendEnd(); scShadowLine(now); if (t % 600 === 0) apply() } };
apply();
run(60000);
assert.equal(bends, 0, 'an aligned deck was bent ' + bends + ' times in a minute');
assert.ok(Math.abs(scShadowAt[2].fine) < 4, 'gap read ' + scShadowAt[2].fine);
ahead = -10; run(30000);
assert.ok(bends === 1 || bends === 2, '10 ms behind: bent ' + bends + ' times');
assert.ok(Math.abs(ahead) < 6, 'bent to ' + ahead);
const closed = bends;
run(60000);
assert.equal(bends, closed, 'bent again once closed: ' + ahead);
""")


@unittest.skipUnless(shutil.which("node"), "node needed for the shadow-deck behavioral checks")
class ShadowTrackChangeTest(unittest.TestCase):
    """Engine mode: the source's previous track leaves the shadow deck while the next one is copied."""

    def test_previous_track_is_unloaded_while_the_next_is_copied(self):
        ShadowDisplayTest.js(self, r"""
const props = {};
const P = p => props[p] || (props[p] = {state: false, string: 'Off'});
globalThis.scTr = P;
globalThis.scShadowApply = e => {};
const unloaded = () => P('/Engine/Deck2/Unload').state && P('/Engine/Deck2/Track/Unload').state;
scShadowAt[2] = {path: '/copies/a.mp3', id: 'A', link: 'peer'};
P('/Engine/Deck2/Play').state = true;
P('/Engine/Deck2/Track/SongLoaded').state = true;
for (let i = 0; i < 3; i++) { scShadowApplyAll([{deck: 2, path: '/copies/a.mp3', id: 'A'}], false); now += 500; }
assert.ok(P('/Engine/Deck2/Play').state && !unloaded() && scShadowAt[2], 'the track the source still holds was touched');
// The source loaded B: stream-sync is still copying it.
const copying = [{deck: 2, path: '', loading: true, id: 'B'}];
scShadowApplyAll(copying, false);
assert.equal(P('/Engine/Deck2/Play').state, false);
now += 400; scShadowApplyAll(copying, false);
assert.ok(unloaded(), 'the previous track stayed on the deck');
assert.equal(scShadowAt[2], undefined);
P('/Engine/Deck2/Unload').state = P('/Engine/Deck2/Track/Unload').state = false;
for (let i = 0; i < 3; i++) { now += 400; scShadowApplyAll(copying, false); }
assert.ok(!unloaded(), 'unloaded again while the copy goes on');
// The same track asked again while its copy is not there yet (a first load): nothing to unload.
scShadowAt[4] = {path: '/copies/c.mp3', id: 'C', link: 'peer'};
for (let i = 0; i < 3; i++) { now += 400; scShadowApplyAll(copying.concat([{deck: 4, path: '', loading: true, id: 'C'}]), false); }
assert.ok(!P('/Engine/Deck4/Unload').state && scShadowAt[4]);
""")


@unittest.skipUnless(shutil.which("node"), "node needed for the shadow-deck behavioral checks")
class ShadowFollowerLineTest(unittest.TestCase):
    """Engine mode: the deck's own line must not average in a position from before a seek landed."""

    def test_a_seek_that_lands_after_the_line_began_starts_the_line_again(self):
        ShadowDisplayTest.js(self, r"""
const T0 = now, props = new Map();
let ahead = 0, tick = 0, bends = 0, most = 0;
globalThis.scShadowBent = null;
globalThis.scShadowBendTimer = {interval: 0, running: false, stop() { this.running = false },
    restart() { this.running = true; bends++; ahead += this.interval * 0.07 * (scTr('/Engine/Deck2/Track/NudgeForward').state ? 1 : -1); most = Math.max(most, ahead) }};
const jitter = () => tick % 7 < 3 ? 9 : -9;
globalThis.scTr = path => {
    let p = props.get(path);
    if (p) return p;
    const deck2 = path.startsWith('/Engine/Deck2/');
    p = {state: deck2 && (path.endsWith('/SongLoaded') || path.endsWith('/PlayState')), string: path.endsWith('/SpeedRange') ? '8' : path.endsWith('/SyncMode') ? 'Off' : '',
        entries: ['4', '8', '10', '20', '50', '100'], index: 1, url: '',
        _v: path.endsWith('/SampleRate') ? 44100 : path.endsWith('/Speed') ? 0.5 : 0,
        get value() { return this._v }, set value(v) { this._v = v },
        get unnormalized() { return path.endsWith('/TrackLength') ? 44100 * 400 : path.endsWith('/TrackPosition') ? (now - T0 + 50000 + ahead + jitter()) * 44.1 : 0 }};
    props.set(path, p);
    return p;
};
let seq = 0;
const apply = () => scShadowApplyAll([{deck: 2, path: '/data/enginex/shadow-v3/peer-1/a.flac', playing: true, link: '{l}',
    positionMs: now - T0 + 50000 + 18, at: now, pitch: 0, bpm: 131, currentBpm: 131, jumps: 0, seq: ++seq}], false);
const step = () => { now += 200; tick++; if (scShadowBendTimer.running) scShadowBendEnd(); scShadowLine(now) };
const run = ms => { for (let t = 0; t < ms; t += 200) { step(); if (t % 600 === 0) apply() } };
apply();
run(3000);
// A seek of ours: Engine still reads the old position (41 s back) on the next tick,
// then lands 55 ms behind the source.
scShadowSeek(2, scShadowAt[2], now - T0 + 50000);
ahead = -41000; step();
ahead = -55; run(30000);
assert.ok(most <= 15, 'bent ' + most + ' ms past the source');
assert.ok(Math.abs(ahead) < 12 && bends >= 1 && bends <= 3, 'ended ' + ahead + ' ms off after ' + bends + ' bends');
""")


@unittest.skipUnless(shutil.which("node"), "node needed for the follower behavioral checks")
class ShadowFollowerStartTest(unittest.TestCase):
    """Engine mode: a play start lands ~110 ms off (rec12-a); at -50 % a bend moves the deck half as far."""

    def test_start_gap_is_bent_after_a_second_and_a_bend_counts_the_decks_speed(self):
        ShadowDisplayTest.js(self, r"""
for (const rate of [1, 0.5]) {
    const T0 = now, props = new Map();
    let ahead = 110 * rate, tick = 0, bends = 0, first = 0, closed = 0, ready = 0;
    globalThis.scShadowBent = null;
    delete scShadowAt[2];
    globalThis.scShadowBendTimer = {interval: 0, running: false, stop() { this.running = false },
        restart() { this.running = true; bends++; first = first || now - T0; ahead += this.interval * 0.07 * rate * (scTr('/Engine/Deck2/Track/NudgeForward').state ? 1 : -1) }};
    const jitter = () => tick % 7 < 3 ? 9 * rate : -9 * rate;
    globalThis.scTr = path => {
        let p = props.get(path);
        if (p) return p;
        const deck2 = path.startsWith('/Engine/Deck2/');
        p = {state: deck2 && (path.endsWith('/SongLoaded') || path.endsWith('/PlayState')), string: path.endsWith('/SpeedRange') ? '100' : path.endsWith('/SyncMode') ? 'Off' : '',
            entries: ['4', '8', '10', '20', '50', '100'], index: 5, url: '',
            _v: path.endsWith('/SampleRate') ? 44100 : path.endsWith('/Speed') ? rate / 2 : 0,
            get value() { return this._v }, set value(v) { this._v = v },
            get unnormalized() { return path.endsWith('/TrackLength') ? 44100 * 400 : path.endsWith('/TrackPosition') ? ((now - T0) * rate + 50000 + ahead + jitter()) * 44.1 : 0 }};
        props.set(path, p);
        return p;
    };
    let seq = 0;
    const apply = () => scShadowApplyAll([{deck: 2, path: '/data/enginex/shadow-v3/peer-1/a.flac', playing: true, link: '{l}', range: 100,
        positionMs: (now - T0) * rate + 50000 + 18 * rate, at: now, pitch: (rate - 1) * 100, rate: rate, bpm: 140, currentBpm: 140 * rate, jumps: 0, seq: ++seq}], false);
    apply();
    for (let t = 0; t < 12000; t += 200) {
        now += 200; tick++;
        if (scShadowBendTimer.running) scShadowBendEnd();
        scShadowLine(now);
        if (t % 600 === 0) apply();
        ready = ready || (scShadowAt[2] && scShadowAt[2].ready ? now - T0 : 0);
        if (!closed && Math.abs(ahead) < 10 * rate) closed = now - T0;
    }
    assert.ok(first > 0 && first - ready <= 1400, 'rate ' + rate + ': first bend ' + (first - ready) + ' ms after the deck was ready');
    assert.ok(closed > 0 && closed <= 9000 && Math.abs(ahead) < 10 * rate, 'rate ' + rate + ': ' + ahead + ' ms off, closed after ' + closed + ' ms, ' + bends + ' bends');
}
""")


class JogGridEditGuardTest(unittest.TestCase):
    def test_jog_guards_stay_while_zoom_is_stock(self):
        # #157's jog half is not a ZOOM / Browse change: Shift+Vinyl while playing and Jog Touch in Grid Edit.
        self.assertIn("(globalConfig.shift && !deckConfig.playing) || deckConfig.gridEditActive", SRC)
        self.assertIn("!deckConfig.gridEditActive &&\\n", SRC)
        self.assertNotIn("BrowseEncoder.qml", SRC)


class ShadowMasterBackTest(unittest.TestCase):
    """Engine Link: the master pauses, its copy on the peer plays on for the trip and is left master (i8b, 42aa60a)."""

    def test_a_copy_that_holds_master_gives_it_to_a_real_deck(self):
        ShadowDisplayTest.js(self, r"""
const pressed = [];
globalThis.scPress = path => pressed.push(path);
const play = {2: false};
globalThis.scShadowProperties = d => ({play: {state: !!play[d]}});
const run = (lead, peerLead, peerPlaying, prev) => {
    pressed.length = 0; now += 1000;
    Object.assign(globalThis, {scShadowLead: lead, scShadowPeerLead: peerLead, scShadowPeerPlaying: peerPlaying, scShadowPrev: prev});
    scShadowMasterBack([1, 3], now);
    return pressed.join();
};
const deck = (ps, loaded, master) => ({ps: ps, loaded: loaded !== false, master: !!master});
// The peer's copy of deck 1 holds master: deck 1 takes it back, stopped (as on stock) or playing.
assert.equal(run(0, 1, false, {1: deck(false), 3: deck(false)}), '/Engine/Deck1/RequestMaster');
assert.equal(run(0, 1, true, {1: deck(true), 3: deck(false)}), '/Engine/Deck1/RequestMaster');
// Deck 1 stopped: the deck that plays takes it, ours or (its own QML) the peer's.
assert.equal(run(0, 1, false, {1: deck(false), 3: deck(true)}), '/Engine/Deck3/RequestMaster');
assert.equal(run(0, 1, true, {1: deck(false), 3: deck(false)}), '');
// Nothing to take it with, nothing to do, or already done.
assert.equal(run(0, 1, false, {1: deck(false, false), 3: deck(false)}), '');
assert.equal(run(0, 0, false, {1: deck(true), 3: deck(false)}), '');
assert.equal(run(0, 1, false, {1: deck(false, true, true), 3: deck(false)}), '');
// Our copy (deck 2) holds master: our playing deck takes it once the copy stopped; else the peer does.
assert.equal(run(2, 0, false, {1: deck(true), 3: deck(false)}), '/Engine/Deck1/RequestMaster');
assert.equal(run(2, 0, false, {1: deck(false), 3: deck(false)}), '');
play[2] = true;
assert.equal(run(2, 0, true, {1: deck(true), 3: deck(false)}), '');
// One press per 800 ms.
play[2] = false;
Object.assign(globalThis, {scShadowLead: 0, scShadowPeerLead: 1, scShadowPeerPlaying: false, scShadowPrev: {1: deck(false), 3: deck(false)}});
pressed.length = 0; now += 1000;
scShadowMasterBack([1, 3], now); now += 400; scShadowMasterBack([1, 3], now); now += 400; scShadowMasterBack([1, 3], now);
assert.equal(pressed.length, 2);
// The reply names the copy and whether a real deck of the peer plays; a stale one says nothing.
globalThis.scShadowApplyDisplay = () => {};
scShadowApplyAll([{deck: 2, shadowMaster: 1, playing: true}, {deck: 4, shadowMaster: 1}], true);
assert.deepEqual([scShadowPeerLead, scShadowPeerPlaying], [1, true]);
scShadowApplyAll([{deck: 2, shadowMaster: 1, playing: true, stale: true}], true);
assert.deepEqual([scShadowPeerLead, scShadowPeerPlaying], [0, false]);
""")
        # The copy is told to the peer with every deck, and a change posts at once.
        self.assertIn("o.shadowMaster = scShadowLead", SRC)
        self.assertIn("if (lead !== scShadowLead) { scShadowLead = lead; edge = true }", SRC)


@unittest.skipUnless(shutil.which("node"), "node needed for the follower behavioral checks")
class ShadowFollowerWallTimeTest(unittest.TestCase):
    """Engine mode: the bend thresholds are wall time. At -50 % a copy 8 ms of its own time (16 ms on screen) off was left for 10 s (rec14-a)."""

    def test_bend_thresholds_count_the_decks_speed(self):
        ShadowDisplayTest.js(self, r"""
const bendsIn9s = rate => {
    const T0 = now, props = new Map();
    let ahead = 8, tick = 0, bends = 0;
    globalThis.scShadowBent = null;
    delete scShadowAt[2];
    globalThis.scShadowBendTimer = {interval: 0, running: false, stop() { this.running = false }, restart() { this.running = true; bends++ }};
    const jitter = () => tick % 7 < 3 ? 4 * rate : -3 * rate;
    globalThis.scTr = path => {
        let p = props.get(path);
        if (p) return p;
        const deck2 = path.startsWith('/Engine/Deck2/');
        p = {state: deck2 && (path.endsWith('/SongLoaded') || path.endsWith('/PlayState')), string: path.endsWith('/SpeedRange') ? '100' : path.endsWith('/SyncMode') ? 'Off' : '',
            entries: ['4', '8', '10', '20', '50', '100'], index: 5, url: '',
            _v: path.endsWith('/SampleRate') ? 44100 : path.endsWith('/Speed') ? rate / 2 : 0,
            get value() { return this._v }, set value(v) { this._v = v },
            get unnormalized() { return path.endsWith('/TrackLength') ? 44100 * 400 : path.endsWith('/TrackPosition') ? ((now - T0) * rate + 50000 + ahead + jitter()) * 44.1 : 0 }};
        props.set(path, p);
        return p;
    };
    let seq = 0;
    const apply = () => scShadowApplyAll([{deck: 2, path: '/data/enginex/shadow-v3/peer-1/a.flac', playing: true, link: '{l}', range: 100,
        positionMs: (now - T0) * rate + 50000 + 18 * rate, at: now, pitch: (rate - 1) * 100, rate: rate, bpm: 140, currentBpm: 140 * rate, jumps: 0, seq: ++seq}], false);
    apply();
    for (let t = 0; t < 9000; t += 200) {
        now += 200; tick++;
        scShadowLine(now);
        if (t % 600 === 0) apply();
    }
    return bends;
};
assert.equal(bendsIn9s(1), 0, '8 ms at full speed is under the young line\'s 12 ms');
assert.ok(bendsIn9s(0.5) >= 1, '8 ms of a deck at half speed is 16 ms of wall time: bent');
""")


@unittest.skipUnless(shutil.which("node"), "node needed for the shadow-deck behavioral checks")
class ShadowSenderReplacedTrackTest(unittest.TestCase):
    """A track loaded over another: Engine names the new file ~0.2 s before its title, artist and length (o5-R2).
    The peer names its copy by the first title it is told: it got the last track's (c5-b)."""

    def test_last_tracks_title_is_not_posted_for_the_new_file(self):
        ShadowDisplayTest.js(self, r"""
const bodies = [];
globalThis.XMLHttpRequest = class {
    open(method, url) { this.url = url; this.readyState = 1 }
    send(body) { if (body) bodies.push(JSON.parse(body)[0]); this.readyState = 4; this.status = 200; if (this.onreadystatechange) this.onreadystatechange() }
};
globalThis.scShadowPoll = () => {};
const deck = {uri: 'file:///a.mp3', title: 'A', artist: 'One', len: 219};
globalThis.scTr = path => ({value: path.endsWith('/SampleRate') ? 44100 : 0, state: path.endsWith('/SongLoaded'), color: '#ffffff', index: 0,
    get unnormalized() { return path.endsWith('/Deck1/Track/TrackLength') ? deck.len * 44100 : 0 },
    get string() { return !path.startsWith('/Engine/Deck1/') ? '' : path.endsWith('/TrackUri') ? deck.uri : path.endsWith('/SongName') ? deck.title : path.endsWith('/ArtistName') ? deck.artist : '' }});
const run = ms => { for (let t = 0; t < ms; t += 40) { now += 40; scShadowTick() } };
run(3000);
assert.deepEqual([bodies[0].title, bodies[0].artist, bodies[0].durationMs], ['A', 'One', 219000]);
bodies.length = 0;
deck.uri = 'file:///b.mp3'; run(120); // sampled inside the gap (the uri is cached for up to 500 ms)
scShadowPrev[1].netAt = 0; run(80);
assert.ok(bodies.length >= 1, 'the new file was not posted');
for (const b of bodies) assert.deepEqual([b.net, b.title, b.artist, b.durationMs || 0], ['file:///b.mp3', '', '', 0]);
Object.assign(deck, {title: 'B', artist: 'Two', len: 171}); run(600);
const b = bodies[bodies.length - 1];
assert.deepEqual([b.title, b.artist, b.durationMs], ['B', 'Two', 171000]);
// The same title again on another file (a duplicate): told once the second has passed.
bodies.length = 0; deck.uri = 'file:///b2.mp3'; scShadowPrev[1].netAt = 0; run(2600);
assert.equal(bodies[0].title, ''); assert.equal(bodies[bodies.length - 1].title, 'B');
""")


@unittest.skipUnless(shutil.which("node"), "node needed for the shadow-deck behavioral checks")
class ShadowSenderLateTickTest(unittest.TestCase):
    """A tick after a GUI stall can read the position of before the stall (zs15-b: 340 ms behind after a 426 ms stall).
    It was posted as a jump back, then a jump forward: the peer's copy was seeked 200 ms back (rec15-d)."""

    def test_a_stale_position_after_a_stall_is_not_a_jump(self):
        ShadowDisplayTest.js(self, r"""
globalThis.XMLHttpRequest = class { open() { this.readyState = 1 } send() { this.readyState = 4; this.status = 200 } };
globalThis.scShadowPoll = () => {};
const T0 = now;
let stale = 0, moved = 0;
globalThis.scTr = path => ({value: path.endsWith('/SampleRate') ? 44100 : 0, color: '#ffffff', index: 0,
    state: path.startsWith('/Engine/Deck1/') && (path.endsWith('/SongLoaded') || path.endsWith('/PlayState')),
    get unnormalized() { return path.endsWith('/Deck1/Track/TrackPosition') ? (now - T0 - stale + moved) * 44.1 : path.endsWith('/Deck1/Track/TrackLength') ? 400 * 44100 : 0 },
    get string() { return path === '/Engine/Deck1/Track/TrackUri' ? 'file:///a.mp3' : '' }});
const run = ms => { for (let t = 0; t < ms; t += 80) { now += 80; scShadowTick() } };
run(2000);
const jumps = scShadowPrev[1].jumps, line = () => scShadowPrev[1].lineMs - (now - T0);
const before = line();
now += 426; stale = 340; scShadowTick(); // the timer runs before the queued position update
assert.equal(scShadowPrev[1].jumps, jumps, 'a stale position was posted as a jump');
assert.ok(Math.abs(line() - before) < 2, 'the line moved by ' + (line() - before));
stale = 0; run(400);
assert.equal(scShadowPrev[1].jumps, jumps);
assert.ok(Math.abs(line() - before) < 2);
// A jump made during a stall is told by the next tick.
now += 426; moved = -5000; scShadowTick(); run(80);
assert.equal(scShadowPrev[1].jumps, jumps + 1);
assert.ok(Math.abs(line() - before + 5000) < 40);
""")


@unittest.skipUnless(shutil.which("node"), "node needed for the shadow-deck behavioral checks")
class ShadowHeadSyncLeadTest(unittest.TestCase):
    """Engine mode: the copy is in neither Sync nor the master selection, so its stock header said "not synced"
    while the source's BPM was green, and drew the phase meter while the source showed LEAD (g10-e-z3-L)."""

    def test_the_shadow_header_gets_the_sources_sync_and_master(self):
        ShadowDisplayTest.js(self, r"""
Object.assign(globalThis, {scShadowApply() {}, scShadowEmpty() {}});
const deck = (d, o) => Object.assign({deck: d, path: '/c/a.mp3', id: 'k'}, o);
const decks = [deck(2, {sync: true}), deck(4, {lead: true}), deck(1, {master: true}), {deck: 3, sync: true}];
scShadowApplyAll(decks, false);
// Role 13: decks 1 and 3 are this player's own, their headers stay Engine's.
const flags = () => Object.keys(scShadowHead).map(d => d + (scShadowHead[d].master ? 'M' : '') + (scShadowHead[d].sync ? 'S' : '')).join();
assert.equal(flags(), '2S,4M'); // lead: Engine Link shares master, stream-sync clears `master` for the copy
decks[1] = deck(4, {master: true}); scShadowApplyAll(decks, false); assert.equal(flags(), '2S,4M'); // a console's peer keeps `master`
const same = scShadowHead;
scShadowApplyAll(decks, false);
assert.strictEqual(scShadowHead, same, 'an unchanged state must not re-evaluate the header bindings');
decks[1].path = ''; // the source ejected
scShadowApplyAll(decks, false);
assert.equal(flags(), '2S');
scShadowApplyAll([], true); // display mode draws its own header
assert.deepEqual(scShadowHead, {});
""")
        self.assertIn('head("deckIndex", "master")', SRC)
        self.assertIn('head("deckIndex", "sync")', SRC)


@unittest.skipUnless(shutil.which("node"), "node needed for the shadow-deck behavioral checks")
class ShadowHeadSourceTextTest(unittest.TestCase):
    """Engine mode: the peer named the copy by its file ("Throw Sum Mo.mp3") and showed the key it analysed (8B)
    while the source's header read its library's title and no key ("-")."""

    def test_the_source_posts_its_key_and_the_shadow_header_takes_title_artist_and_key(self):
        ShadowDisplayTest.js(self, r"""
const bodies = [];
globalThis.XMLHttpRequest = class {
    open(method, url) { this.url = url; this.readyState = 1 }
    send(body) { if (body) bodies.push(JSON.parse(body)[0]); this.readyState = 4; this.status = 200; if (this.onreadystatechange) this.onreadystatechange() }
};
globalThis.scShadowPoll = () => {};
const deck = {key: ''};
globalThis.scTr = path => ({value: path.endsWith('/SampleRate') ? 44100 : 0, state: path.endsWith('/SongLoaded'), color: '#ffffff', index: 0,
    get unnormalized() { return path.endsWith('/Deck1/Track/TrackLength') ? 200 * 44100 : 0 },
    get string() { return !path.startsWith('/Engine/Deck1/') ? '' : path.endsWith('/TrackUri') ? 'file:///a.mp3' : path.endsWith('/SongName') ? 'A' : path.endsWith('/ArtistName') ? 'One' : path.endsWith('/Track/CurrentKey') ? deck.key : '' }});
const run = ms => { for (let t = 0; t < ms; t += 40) { now += 40; scShadowTick() } };
run(3000);
assert.equal(bodies[bodies.length - 1].keyName, '-', 'a track without a key is told as the header writes it');
deck.key = '8B'; run(2600); // analysed, or shifted: told at the next text read
assert.equal(bodies[bodies.length - 1].keyName, '8B');
// The peer: its header takes the three texts from the source, not from the copy.
Object.assign(globalThis, {scShadowApply() {}, scShadowEmpty() {}});
scShadowApplyAll([{deck: 2, path: '/c/One - A.mp3', id: 'k', title: 'A', artist: 'One', keyName: '-'}, {deck: 4, path: '/c/b.mp3', id: 'j'}], false);
assert.deepEqual(scShadowHead[2], {master: false, sync: false, title: 'A', artist: 'One', keyName: '-'});
assert.deepEqual(scShadowHead[4], {master: false, sync: false, title: '', artist: '', keyName: ''}); // a CDJ or an older sender: stock texts
""")
        for anchor in ('scaled[bj + "Deck/DeckItem/ArtistSong.qml"]', 'scaled[bj + "KeyEditor/KeyInfo.qml"]', "root.scShadowHeadOf(microDeck.deckIndex).title"):
            self.assertIn(anchor, SRC)
