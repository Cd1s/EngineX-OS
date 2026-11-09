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
        self.assertIn("now - scShadowPostAt >= 250", bp.SERVICES_MAIN)
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
                 "scShadowProperties", "scShadowTick", "scShadowPost", "scShadowPoll", "scShadowDisplayFetch", "scShadowApplyDisplay", "scShadowApplyAll", "scNextCueInfo", "scCueTickFor",
                 "scShadowApply", "scShadowEmpty", "scShadowSpeed", "scShadowCues", "scShadowBeat",
                 "scShadowSeek", "scShadowLink", "scShadowLinkEdge", "scPress")
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
    scShadowExpiryMs: 3000, scShadowAt: {}, scShadowDisplay: {}, scShadowDisplaySpare: {}, scShadowEmptyArray: [],
    scShadowDisplayRequests: {}, scShadowGridCache: {}, scShadowLiveCueCache: {},
    scShadowCdjMaster: false, scPressQueue: [], scShadowEmptyObject: {},
    scShadowMineRole: "?", scShadowMine: [], scShadowSampleSpare: {}, scShadowDeckProperties: {}, scShadowOwn: [],
    scShadowPostRequest: null, scShadowPollRequest: null, scShadowResponseVersion: "", scShadowPostPending: false,
    scShadowPrev: {}, scShadowCueCache: {}, scShadowPostAt: 0, scShadowPollAt: now, scShadowDebug: false,
    scShadowGen: 0, scShadowVer: 0, scShadowFrame: 17, scCueInfoCache: {},
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
assert.equal(scCueTickFor(2), 17); // a shadow countdown must not depend on stopped local Engine clocks
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
let created=0;
globalThis.XMLHttpRequest=class {
    constructor(){created++;this.readyState=0;}
    open(method,url){assert.equal(method,'POST');this.readyState=1;}
    send(body){assert.equal(JSON.parse(body).length,2);this.readyState=4;}
};
globalThis.scShadowPoll=()=>{};
globalThis.scTr=path=>({value:path.endsWith('/SampleRate')?48000:0,unnormalized:0,state:false,string:'',color:'#ffffff'});
scShadowTick();const first=scShadowPrev[1], own=scShadowOwn, post=scShadowPostRequest;
now+=40;scShadowTick();const second=scShadowPrev[1];
assert.notStrictEqual(first,second);
now+=40;scShadowTick();assert.strictEqual(scShadowPrev[1],first);
for(let i=0;i<100;i++){now+=40;scShadowTick();}
assert.strictEqual(scShadowOwn,own);assert.strictEqual(scShadowPostRequest,post);
assert.equal(created,1);
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
