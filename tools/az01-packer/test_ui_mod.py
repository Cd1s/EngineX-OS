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


@unittest.skipUnless(shutil.which("node"), "node needed for the display-state behavioral checks")
class ShadowDisplayTest(unittest.TestCase):
    """Run the actual QML JS functions with a fake clock and an Engine that rejects all access."""

    def js(self, body):
        names = ("scDeckLocked", "scShadowVisible", "scShadowDeckFor", "scShadowData", "scShadowPosition",
                 "scShadowBeatIndex", "scShadowBeatAt", "scShadowGrid", "scShadowNextCue", "scShadowCuesFor", "scShadowAsset",
                 "scShadowProperties", "scShadowTick", "scShadowPost", "scShadowPoll", "scShadowDisplayFetch", "scShadowSameSample", "scShadowApplyDisplay", "scShadowApplyAll", "scNextCueInfo", "scNextCueCompute", "scCueTickFor",
                 "scShadowApply", "scShadowEmpty", "scShadowSpeed", "scShadowCues", "scShadowBeat",
                 "scShadowSeek", "scShadowLink", "scShadowLinkEdge", "scPress", "scGcCheck", "scShadowWake", "scShadowAnyPlaying")
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
    scShadowCdjMaster: false, scPressQueue: [], scShadowEmptyObject: {},
    scShadowMineRole: "?", scShadowMine: [], scShadowSampleSpare: {}, scShadowDeckProperties: {}, scShadowOwn: [],
    scShadowPostRequest: null, scShadowPollRequest: null, scShadowResponseVersion: "", scShadowPostPending: false,
    scShadowLinkId: '', scShadowLinkAt: -Infinity, scShadowTextAt: {},
    scShadowSent: [], scShadowFullAt: 0, scShadowPostFull: true, scShadowLive: {seen: 0},
    scShadowTickTimer: {interval: 40}, scShadowBusyAt: 0, scShadowRoleDirty: true,
    scGcLoadSeq: 0, scGcRuns: 0, scGcLastMs: 0, scGcProps: null,
    scGcState: {armed: true, quietSince: 0, lastRun: -Infinity, loadSig: 0, loadSeq: 0, loadAt: 0, followAt: 0},
    scShadowPrev: {}, scShadowCueCache: {}, scShadowPostAt: 0, scShadowPollAt: now, scShadowDebug: false,
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
        return "with (scope) {\n" + "\n".join(parts) + "\n}\n"

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

    def test_shadow_layer_scroll_settle_and_grid_pool_behaviour(self):
        self.js(r"""
const starts=[];
const scroll={running:false,from:0,to:0,duration:0,stop(){this.running=false},start(){this.running=true;starts.push({from:this.from,to:this.to,duration:this.duration})}};
const scMoving={x:0};
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
assert.equal(starts[0].from,scope.contentX(10000)); assert.equal(starts[0].to,scope.contentX(200000)); assert.equal(starts[0].duration,190000);
// Grid pool: power of two above the window's beats; tiles under the window only.
assert(scope.grid.length>0 && scope.gridSlots>=scope.grid.length && (scope.gridSlots&(scope.gridSlots-1))===0 && scope.gridSlots>=32);
assert.equal(scope.tileLo,0); assert.equal(scope.tileHi,Math.floor((6000+12000)/20000));
now+=200; scope.refreshPosition(); assert.equal(starts.length,1); // on its line: keeps running
scope.sample=sample(10000+200+20,true); scope.refreshSample(); assert.equal(starts.length,2); // 20 ms off: restarted
scope.sample=sample(10220,true,{pitch:8}); scope.refreshSample(); assert.equal(starts.length,3); // new speed: restarted
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
assert(strings < 120, 'string reads ' + strings);
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


if __name__ == "__main__":
    unittest.main()
