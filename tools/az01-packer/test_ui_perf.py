"""Regression checks for the live dictionaries and non-shadow QML hot paths."""
import importlib.util
import json
import re
import shutil
import subprocess
import unittest
from pathlib import Path

spec = importlib.util.spec_from_file_location("build_packs_perf", Path(__file__).resolve().parents[1] / "ui-mod/build_packs.py")
packs = importlib.util.module_from_spec(spec)
spec.loader.exec_module(packs)

_legacy_spec = importlib.util.spec_from_file_location("legacy_names_perf", Path(__file__).with_name("legacy_names.py"))
legacy = importlib.util.module_from_spec(_legacy_spec)
_legacy_spec.loader.exec_module(legacy)


class DictionaryTest(unittest.TestCase):
    def test_generated_languages_trace_only_one_dictionary(self):
        rows = packs.load_dict()
        dictionaries = packs.encoded_dicts(rows)
        original_entries = sum(len(json.loads(d)) for d in dictionaries.values())
        for lang in dictionaries:
            with self.subTest(lang=lang):
                generated = packs.render_language(packs.SERVICES_MAIN, lang, dictionaries)
                encoded = re.search(r"readonly property var scDicts: \((.+)\)", generated).group(1)
                data = json.loads(encoded)
                self.assertTrue(all(isinstance(value, str) for value in data.values()))
                selected = json.loads(data[lang])
                self.assertAlmostEqual(len(selected) / original_entries, 1 / len(packs.LANGUAGES))
                self.assertIn(f'property string scLang: "{lang}"', generated)
                self.assertIn("JSON.parse(scDicts[scLang] || scDicts.en)", generated)
                # Live switches must translate every baked spelling exactly as before.
                expected = {}
                for en, translations in rows.items():
                    for spelling in (en, *translations.values()):
                        expected[spelling] = en if lang == "en" else translations[lang]
                self.assertEqual(selected, expected)


@unittest.skipUnless(shutil.which("node"), "Node is needed to exercise QML JavaScript")
class HotPathTest(unittest.TestCase):
    def test_cue_bindings_and_status_polling(self):
        names = ("scDeckLocked", "scShadowVisible", "scTr", "scProbeCueClocks", "scCueProps", "scCueTableOf", "scCueWatch", "scCueStep", "scCueCount", "scCuePlayhead", "scCueFeedLine", "scCueBeatsLeft", "scNextCueInfo", "scNextCueCompute", "scCueTickFor", "scSameItems", "scCall", "scUiPollReply")
        functions = []
        for name in names:
            start = re.search(r"(?m)^\tfunction " + name + r"\([^\n]*\) \{", packs.SERVICES_MAIN)
            self.assertIsNotNone(start, name)
            line_end = packs.SERVICES_MAIN.index("\n", start.start())
            if packs.SERVICES_MAIN[start.start():line_end].rstrip().endswith("}"):
                end = line_end
            else:
                end = packs.SERVICES_MAIN.index("\n\t}", start.end()) + 3
            functions.append(packs.SERVICES_MAIN[start.start():end])
        # No Timer drives the countdown: Engine's beat counter does, and only on a deck that has a hot cue table.
        self.assertNotIn("scCueClock", packs.SERVICES_MAIN)
        for d in range(1, 5):
            self.assertIn(f"Connections {{ target: root.scCueTable{d} ? root.scCueTable{d}.p.beat : null; function onUnnormalizedChanged() {{ root.scCueStep({d}) }} }}", packs.SERVICES_MAIN)
            self.assertIn(f"Connections {{ target: root.scCueWatch(root.scCueTable{d}, root.scCueNoBeat & {1 << d}); function onUnnormalizedChanged() {{ root.scCueStep({d}) }} }}", packs.SERVICES_MAIN)
        script = r'''
const assert = require('node:assert/strict');
const root = globalThis;
var scShadowDisplayOnly = false, scShadowEngineMask = 30, scFourDeckRole = "", scPlayer = false, scShadowAt = {}, scShadowDisplay = {}, scPausedShadowFollow = false;
let lookups = 0, made = 0, applied = 0;
const properties = {};
const unavailable = new Set();
const Planck = {getProperty(path) {
    lookups++;
    if (unavailable.has(path)) return null;
    return {translator: properties[path] ||= {state: false, value: 0, unnormalized: 0, string: ""}};
}};
var scPropCache = {}, scCuePropCache = {}, scCueInfoCache = {}, scCueInfoMemo = {}, scUiPoll = null, scUiPollAt = 0, scUiDigest = "", scUiAppliedAt = 0, ensured = 0;
var scScope = "", scScopePending = 0, scUiPollScope = "";
function scEnsureTidal() { ensured++; }
const reads = [];
const clock = d => ({get tick() { reads.push(d); return d; }});
const scCueClock1 = clock(1), scCueClock2 = clock(2), scCueClock3 = clock(3), scCueClock4 = clock(4);
function scApply(v) { applied++; assert.equal(v.language, 'en'); }
class XMLHttpRequest {
    constructor() { made++; this.readyState = 0; }
    open(method, url) { this.readyState = 1; }
    send() {}
    abort() { this.readyState = 0; this.aborted = true; }
    getResponseHeader(name) { return (this.headers || {})[name] || null; }
}
''' + "\n".join(functions) + "\nroot.scTr = scTr; root.scCueProbe = 0; root.scCueCountdown = true; root.scCueTexts = []; root.scCueNoBeat = 0; for (let d = 1; d <= 4; d++) root['scCueShown' + d] = {};\n" + "".join(
            # The table is a binding and the count runs in change handlers: here every read works them out afresh.
            f"Object.defineProperty(root, 'scCueTable{d}', {{get() {{ return scCueTableOf({d}); }}, configurable: true}});\n"
            f"Object.defineProperty(root, 'scCueInfo{d}', {{get() {{ return scCueCount(scCueTableOf({d}), root.scCueShown{d}); }}, set(v) {{}}, configurable: true}});\n" for d in range(1, 5)) + r'''
const set = (key, fields) => Object.assign(scTr('/Engine/Deck1/' + key), fields);
set('Track/SongLoaded', {state: true});
set('Track/SampleRate', {value: 1000});
set('OriginalBPM', {unnormalized: 120});
set('Track/QuickCue1Exists', {state: true});
set('Track/QuickCue1Position', {value: 9000});
set('Track/QuickCue1Name', {string: 'DROP'});
const info = scNextCueInfo(1, 0);
assert.equal(info.text, '4.3');
assert.equal(info.label, 'DROP');
const initialLookups = lookups;
for (let i = 0; i < 60; i++) assert.equal(scNextCueInfo(1, i), info);
assert.equal(lookups, initialLookups); // same translators and same display object
set('Track/TrackPosition', {unnormalized: 1000});
assert.equal(scNextCueInfo(1, 1).text, '4.1');
// A local deck's views read the count its handlers left; a new count (an edit, a jump or a reload inside the same beat) is its own answer.
const before = lookups;
set('Track/TrackPosition', {unnormalized: 3000});
assert.equal(scNextCueInfo(1, -7).text, '3.1');
set('Track/TrackPosition', {unnormalized: 1000});
assert.equal(scNextCueInfo(1, -7).text, '4.1');
assert.equal(scNextCueInfo(1, 100004).text, '4.1');
set('Track/TrackPosition', {unnormalized: 3000});
assert.equal(scNextCueInfo(1, 100004).text, '3.1');
set('Track/TrackPosition', {unnormalized: 1000});
set('Track/SongLoaded', {state: false});
assert.equal(scNextCueInfo(1, 2), null);
assert.equal(scCueTickFor(2), 0); // a local deck has no tick: its views read scCueInfo2
assert.deepEqual(reads, []); // and no deck depends on a clock
// Steps: the handler of every beat notification. One table per deck here (a getter would hand each step a new one).
const table = {};
for (let d = 1; d <= 4; d++) Object.defineProperty(root, 'scCueTable' + d, {get() { return table[d] || null; }, configurable: true});
let counts = 0; const realCount = scCueCount; scCueCount = (t, o) => { counts++; return realCount(t, o); };
const step = () => { const before = counts; scCueStep(1); return counts - before; };
// 120 BPM at 1000 samples a second: a beat is 500 samples, grid lines at 250 + 500 k (the cue at 9000 is half a beat off: mid-beat
// it would have no room for a grid shift, so it moves onto a line).
set('Track/QuickCue1Position', {value: 9250});
const at = (pos) => { set('Track/TrackPosition', {unnormalized: pos}); set('BeatNumber', {unnormalized: (((pos - 250) / 500) % 4 + 4) % 4}); };
set('Track/SongLoaded', {state: true}); at(1500);
table[1] = scCueTableOf(1);
assert.equal(step(), 1); assert.equal(table[1].k, 5); // a new table counts; twice beat 2, + 1 past its line
assert.equal(scCueInfo1.text, '3.4'); // 15.5 beats to the cue: 16 lines ahead of the playhead's, half a beat into its own
const lookupsBefore = lookups;
for (const pos of [1550, 1600, 1745]) { at(pos); assert.equal(step(), 0, 'inside beat 2 at ' + pos); }
at(1750); assert.equal(step(), 1); assert.equal(table[1].k, 6); // standing on the line itself
at(1755); assert.equal(step(), 1); assert.equal(table[1].k, 7); // past it: the count is one beat less
at(1900); assert.equal(step(), 0);
// A jump of whole bars leaves BeatNumber where it was: the playhead moved more than two beats (1000 samples at 120 BPM).
at(3900); assert.equal(step(), 1);
at(3950); assert.equal(step(), 0);
// Beat Grid Edit moves the lines under a resting playhead: its beat changes, its position does not.
set('BeatNumber', {unnormalized: 3.9}); assert.equal(step(), 1, 'grid shifted by 0.5 beat inside the same beat');
at(3950); assert.equal(step(), 1);
// The playhead is watched only on a deck at rest or without a beat position; a playing deck with a grid is left to its beat counter.
assert.equal(scCueWatch(table[1], 0), scTr('/Engine/Deck1/Track/TrackPosition'));
set('PlayState', {state: true}); assert.equal(scCueWatch(table[1], 0), null); assert.equal(scCueWatch(table[1], 2), scTr('/Engine/Deck1/Track/TrackPosition'));
assert.equal(scCueWatch(null, 2), null);
// Passing a hot cue inside a beat (50 ms before it the next one counts).
at(9190); assert.equal(step(), 1); assert.equal(scCueNoBeat, 0);
at(9210); assert.equal(step(), 1); at(9220); assert.equal(step(), 0);
at(9190); assert.equal(step(), 1); // and back before it
assert.equal(lookups, lookupsBefore); // no property lookup on any step
// No beat position (no grid): the deck's bit is set, which connects its position; steps are whole beats of distance.
set('Track/TrackPosition', {unnormalized: 1020}); set('BeatNumber', {unnormalized: -1});
assert.equal(step(), 1); assert.equal(scCueNoBeat, 2); assert.equal(table[1].k, -1); assert.equal(scCueInfo1.text, '4.1'); // 16.46 beats
set('Track/TrackPosition', {unnormalized: 1200}); assert.equal(step(), 0);
set('Track/TrackPosition', {unnormalized: 1260}); assert.equal(step(), 1); // 15.98
set('Track/TrackPosition', {unnormalized: 1200}); assert.equal(step(), 1); // and back
at(1300); assert.equal(step(), 1); assert.equal(scCueNoBeat, 0);
// Decks are apart: another deck's step leaves this deck's table and count alone.
for (const k of ['Track/SongLoaded', 'Track/QuickCue1Exists']) scTr('/Engine/Deck2/' + k).state = true;
scTr('/Engine/Deck2/Track/QuickCue1Position').value = 5000; scTr('/Engine/Deck2/OriginalBPM').unnormalized = 120; scTr('/Engine/Deck2/Track/SampleRate').value = 1000;
const kept = JSON.stringify(table[1]);
table[2] = scCueTableOf(2); scCueStep(2);
assert.equal(JSON.stringify(table[1]), kept);
// An optional property Engine did not have when the deck was first looked up is asked for again at the next table.
unavailable.add('/Engine/Deck2/BeatNumber'); delete scPropCache['/Engine/Deck2/BeatNumber']; delete scCuePropCache[2];
table[2] = scCueTableOf(2); scCueStep(2);
assert.equal(table[2].k, -1); assert.equal(scCueNoBeat, 4); // no beat property: the position is watched instead
unavailable.delete('/Engine/Deck2/BeatNumber');
table[2] = scCueTableOf(2); scCueStep(2);
assert.equal(table[2].k, 0); assert.equal(scCueNoBeat, 0);
// A track load is such a table: BeatNumber and PlayState missing at first, there when the next track loads (SongLoaded off, then on).
for (const key of ['BeatNumber', 'PlayState']) { unavailable.add('/Engine/Deck2/' + key); delete scPropCache['/Engine/Deck2/' + key]; }
delete scCuePropCache[2];
table[2] = scCueTableOf(2); assert.equal(table[2].p.beat, null); assert.equal(table[2].p.play, null);
scTr('/Engine/Deck2/Track/SongLoaded').state = false; table[2] = scCueTableOf(2); assert.equal(table[2], null);
unavailable.delete('/Engine/Deck2/BeatNumber'); unavailable.delete('/Engine/Deck2/PlayState');
scTr('/Engine/Deck2/Track/SongLoaded').state = true; table[2] = scCueTableOf(2); scCueStep(2);
assert.equal(table[2].p.beat, scTr('/Engine/Deck2/BeatNumber')); assert.equal(table[2].p.play, scTr('/Engine/Deck2/PlayState')); assert.equal(scCueNoBeat, 0);
// Unloaded, empty, without a hot cue or switched off: no table, so nothing is connected and a stray step counts nothing.
scTr('/Engine/Deck2/Track/SongLoaded').state = false; assert.equal(scCueTableOf(2), null);
scTr('/Engine/Deck2/Track/SongLoaded').state = true; scTr('/Engine/Deck2/Track/QuickCue1Exists').state = false; assert.equal(scCueTableOf(2), null);
scTr('/Engine/Deck2/Track/QuickCue1Exists').state = true; assert.ok(scCueTableOf(2));
root.scCueCountdown = false;
const quiet = lookups; for (let d = 1; d <= 4; d++) assert.equal(scCueTableOf(d), null); assert.equal(lookups, quiet);
root.scCueCountdown = true;
// A deck Engine has not built yet: the settings poll wakes its table binding until the deck is found.
for (let d = 1; d <= 4; d++) void scCueTableOf(d);
const missingDeck = '/Engine/Deck3/Track/SongLoaded';
delete scCuePropCache[3]; delete scPropCache[missingDeck]; unavailable.add(missingDeck);
assert.equal(scCueTableOf(3), null);
scProbeCueClocks(); assert.equal(root.scCueProbe, 1);
unavailable.delete(missingDeck); assert.equal(scCueTableOf(3), null); assert.ok(scCuePropCache[3]);
scProbeCueClocks(); assert.equal(root.scCueProbe, 1); // stop probing once discovered
assert(scSameItems(['tidal', 'spotify'], ['tidal', 'spotify']));
assert(!scSameItems(['tidal'], ['spotify']));
assert(!scSameItems(['tidal'], ['tidal', 'spotify']));
assert(!scSameItems(['tidal', 'spotify'], ['spotify', 'tidal']));
const oldSources = ['tidal'];
let sources = oldSources;
if (!scSameItems(sources, ['tidal'])) sources = ['tidal'];
assert.equal(sources, oldSources); // Repeater model retains its identity
scCall('');
scCall('');
assert.equal(made, 1); // no overlapping polls
scUiPoll.readyState = 4; scUiPoll.status = 200; scUiPoll.responseText = '{"language":"en"}';
scUiPoll.onreadystatechange();
scCall('');
assert.equal(made, 1); // reuse both the XHR and its callback
assert.equal(applied, 1);
// stream-sync's digest: an unchanged body is neither parsed nor applied (30 s safety re-apply).
const reply = digest => { scUiPoll.headers = {'X-UI-Digest': digest}; scUiPoll.readyState = 4; scUiPoll.onreadystatechange(); };
reply('a1'); assert.equal(applied, 2);
reply('a1'); reply('a1'); assert.equal(applied, 2); assert.equal(ensured, 2);
reply('b2'); assert.equal(applied, 3);
scUiAppliedAt -= 31000; reply('b2'); assert.equal(applied, 4);
scCall(''); // the next poll is in flight again
scUiPollAt = Date.now() - 11000;
scCall('');
assert(scUiPoll.aborted); // a hung server cannot disable polling forever
assert.equal(made, 1);
'''
        result = subprocess.run(["node"], input=script, text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_ui_poll_refreshes_active_provider_on_login_change(self):
        start = re.search(r"(?m)^\tfunction scUiPollReply\([^\n]*\) \{", packs.SERVICES_MAIN)
        self.assertIsNotNone(start)
        end = packs.SERVICES_MAIN.index("\n\t}", start.end()) + 3
        fn = packs.SERVICES_MAIN[start.start():end]
        script = r'''
const assert = require('node:assert/strict');
var scUiPoll = {readyState: 4, status: 200, responseText: '{}', headers: {}, getResponseHeader(k) { return this.headers[k] || ''; }};
var scScopePending = 0, scScope = 'applemusic', scUiPollScope = 'applemusic';
var scUiDigest = '', scUiAppliedAt = 0, scUiLoaded = true;
var scLoggedIn = ['spotify'], scLogin = 'applemusic', scEnterTries = 0;
let invalidated = 0, entered = 0;
const scEnterTimer = {start() { entered++; }};
const Planck = {librarianController: {streamingPlaylistsController: {invalidateUI() { invalidated++; }}}};
function scEnsureTidal() {}
function scApply(v) { if (v.loggedIn) scLoggedIn = v.loggedIn; }
''' + fn + r'''
// External WebUI / helper sign-in adds active scope -> closes QR and invalidates playlists passively (never auto-enters).
scUiPoll.headers = {'X-UI-Digest': 'd1'};
scUiPoll.responseText = JSON.stringify({loggedIn: ['spotify', 'applemusic']});
scUiPollReply();
assert.equal(scLogin, '');
assert.equal(invalidated, 1);
assert.equal(entered, 0);
assert.equal(scEnterTries, 0);

// "Switch account" opens QR while already logged in -> unchanged membership must not close QR.
scLogin = 'applemusic';
scUiPoll.headers = {'X-UI-Digest': 'd2'};
scUiPoll.responseText = JSON.stringify({loggedIn: ['spotify', 'applemusic']});
scUiPollReply();
assert.equal(scLogin, 'applemusic');
assert.equal(invalidated, 1);
assert.equal(entered, 0);

// Remote logout removes active scope -> invalidates stale playlists without auto-entering.
scLogin = '';
scUiPoll.headers = {'X-UI-Digest': 'd3'};
scUiPoll.responseText = JSON.stringify({loggedIn: ['spotify']});
scUiPollReply();
assert.equal(invalidated, 2);
assert.equal(entered, 0);
'''
        result = subprocess.run(["node"], input=script, text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_ui_poll_scope_change_does_not_auto_enter(self):
        start = re.search(r"(?m)^\tfunction scUiPollReply\([^\n]*\) \{", packs.SERVICES_MAIN)
        self.assertIsNotNone(start)
        end = packs.SERVICES_MAIN.index("\n\t}", start.end()) + 3
        fn = packs.SERVICES_MAIN[start.start():end]
        script = r'''
const assert = require('node:assert/strict');
var scUiPoll = {readyState: 4, status: 200, responseText: '{}', headers: {}, getResponseHeader(k) { return this.headers[k] || ''; }};
var scScopePending = 0, scScope = 'spotify', scUiPollScope = 'spotify';
var scUiDigest = '', scUiAppliedAt = 0, scUiLoaded = true;
var scLoggedIn = ['spotify'], scLogin = '', scEnterTries = 0;
let invalidated = 0, entered = 0;
const scEnterTimer = {start() { entered++; }};
const Planck = {librarianController: {streamingPlaylistsController: {invalidateUI() { invalidated++; }}}};
function scEnsureTidal() {}
function scApply(v) { if (v.loggedIn) scLoggedIn = v.loggedIn; if (v.scope !== undefined) scScope = v.scope; }
''' + fn + r'''
// Web console switches scope from Spotify (logged in) to Apple Music (not logged in) with no login change.
scUiPoll.headers = {'X-UI-Digest': 'a'};
scUiPoll.responseText = JSON.stringify({scope: 'applemusic', loggedIn: ['spotify']});
scUiPollReply();
assert.equal(invalidated, 0);
assert.equal(entered, 0);

// Web console switches scope from Apple Music (not logged in) to Spotify (logged in) with no login change.
scScope = 'applemusic'; scUiPollScope = 'applemusic'; scLoggedIn = ['spotify']; invalidated = 0; entered = 0;
scUiPoll.headers = {'X-UI-Digest': 'b'};
scUiPoll.responseText = JSON.stringify({scope: 'spotify', loggedIn: ['spotify']});
scUiPollReply();
assert.equal(invalidated, 0);
assert.equal(entered, 0);
'''
        result = subprocess.run(["node"], input=script, text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)


class SessionLongevityTest(unittest.TestCase):
    """S7: BreadcrumbView proxy-model lifetime and bounded deck/overlay/TrackInfo album art."""

    def _edits_for(self, root: str):
        saved = packs.ROOT
        try:
            packs.ROOT = root
            return packs.edits(False)
        finally:
            packs.ROOT = saved

    @staticmethod
    def _join_replacements(fixes):
        return "\n".join(new for _, new in fixes)

    @staticmethod
    def _extract_js_block(text: str, marker: str) -> str:
        start = text.index("{", text.index(marker))
        depth = 1
        for idx in range(start + 1, len(text)):
            ch = text[idx]
            if ch == "{":
                depth += 1
            elif ch == "}":
                depth -= 1
                if depth == 0:
                    return text[start + 1:idx]
        raise ValueError(f"unterminated block for {marker}")

    def test_breadcrumb_proxy_model_ownership_edits_registered_on_both_bases(self):
        for root, label in (("Skins/Embedded/", "5.3.0"), ("Skins/EmbeddedCommon/", "5.0.4")):
            with self.subTest(base=label):
                ed = self._edits_for(root)
                svh = root + "Views/BrowseView/Components/BreadcrumbView/Handlers/StackViewHandler.qml"
                bsv = root + "Views/BrowseView/Components/BreadcrumbView/BreadcrumbStackView.qml"
                bv = root + "Views/BrowseView/Components/BreadcrumbView/BreadcrumbView.qml"
                self.assertTrue(svh in ed, f"{label}: missing StackViewHandler.qml ownership patch")
                self.assertTrue(bsv in ed, f"{label}: missing BreadcrumbStackView.qml lifetime patch")
                self.assertTrue(bv in ed, f"{label}: missing BreadcrumbView.qml selection refresh patch")
                svh_new = self._join_replacements(ed[svh])
                bsv_new = self._join_replacements(ed[bsv])
                bv_new = self._join_replacements(ed[bv])
                self.assertIn("scOwnsModel: true", svh_new)
                self.assertIn("function scNotifyChildModels(", bsv_new)
                self.assertIn("function scReleaseChildModel(", bsv_new)
                self.assertIn("property bool scOwnsModel: false", bsv_new)
                self.assertIn("property int scChildModelsRev: 0", bsv_new)
                self.assertIn("if (scOwnsModel && model && typeof model.destroy === \"function\")", bsv_new)
                self.assertGreaterEqual(bsv_new.count("scOwnsModel: true"), 2)
                self.assertGreaterEqual(bsv_new.count("stackView.scReleaseChildModel("), 3)
                self.assertGreaterEqual(bsv_new.count("stackView.scNotifyChildModels("), 3)
                self.assertIn("scChildModelsRev", bv_new)

    @unittest.skipUnless(shutil.which("node"), "Node is needed to exercise QML JavaScript")
    def test_breadcrumb_release_and_handoff_state_machine_under_node(self):
        for root, label in (("Skins/Embedded/", "5.3.0"), ("Skins/EmbeddedCommon/", "5.0.4")):
            with self.subTest(base=label):
                ed = self._edits_for(root)
                svh = root + "Views/BrowseView/Components/BreadcrumbView/Handlers/StackViewHandler.qml"
                bsv = root + "Views/BrowseView/Components/BreadcrumbView/BreadcrumbStackView.qml"
                bv = root + "Views/BrowseView/Components/BreadcrumbView/BreadcrumbView.qml"
                self.assertTrue(svh in ed and bsv in ed and bv in ed, f"{label}: missing BreadcrumbView edits")
                bsv_fixes = ed[bsv]
                bsv_new = self._join_replacements(bsv_fixes)
                svh_new = self._join_replacements(ed[svh])
                bv_expr = next(
                    (new.strip() for old, new in ed[bv] if "childModels[currentIndex]" in old),
                    None,
                )
                self.assertIsNotNone(bv_expr, f"{label}: missing currentItemChildModel binding edit")
                self.assertIn("scOwnsModel: true", svh_new, f"{label}: StackViewHandler must own replaced root model")

                notify_body = self._extract_js_block(bsv_new, "function scNotifyChildModels(cv)")
                release_body = self._extract_js_block(bsv_new, "function scReleaseChildModel(m)")
                destructor_new = next(
                    (new for old, new in bsv_fixes if "breadcrumbView.stackviewContentDestroyed()" in old),
                    None,
                )
                self.assertIsNotNone(destructor_new, f"{label}: missing Component.onDestruction patch")
                destructor_body = destructor_new[:destructor_new.rfind("}")]

                store_body = next(
                    (new for old, new in bsv_fixes if "contentView.childModels[rowIndex] = newChildModel" in old),
                    None,
                )
                splice_body = next(
                    (new for old, new in bsv_fixes if "contentView.childModels.splice(" in old),
                    None,
                )
                clear_new = next(
                    (new for old, new in bsv_fixes if "contentView.childModels = []" in old),
                    None,
                )
                self.assertIsNotNone(store_body, f"{label}: missing delegate childModels store patch")
                self.assertIsNotNone(splice_body, f"{label}: missing onRowsRemoved patch")
                self.assertIsNotNone(clear_new, f"{label}: missing onRowsInserted patch")
                clear_body = (
                    self._extract_js_block(clear_new, "function onRowsInserted(")
                    if "function onRowsInserted(" in clear_new else clear_new
                )

                script = (
                    "const assert = require('node:assert/strict');\n"
                    "const StackView = { DontLoad: 1 };\n"
                    "let nextSerial = 1, live = 0, destroyed = 0, doubleDestroyed = 0, destroyedCallbacks = 0;\n"
                    "const pendingDeletes = [];\n"
                    "function makeModel(hasChildren = true) {\n"
                    "    live++;\n"
                    "    const obj = {\n"
                    "        serial: nextSerial++,\n"
                    "        hasChildren,\n"
                    "        alive: true,\n"
                    "        pendingDelete: false,\n"
                    "        destroy() {\n"
                    "            if (!this.alive) { doubleDestroyed++; return; }\n"
                    "            if (!this.pendingDelete) { this.pendingDelete = true; pendingDeletes.push(this); }\n"
                    "        }\n"
                    "    };\n"
                    "    return obj;\n"
                    "}\n"
                    "function drainDeferredDeletes() {\n"
                    "    while (pendingDeletes.length) {\n"
                    "        const obj = pendingDeletes.shift();\n"
                    "        if (obj.alive) { obj.alive = false; live--; destroyed++; }\n"
                    "    }\n"
                    "    // Qt QObject* property references become null once deferred deletion completes.\n"
                    "    if (currentItemChildModelVal && !currentItemChildModelVal.alive) currentItemChildModelVal = null;\n"
                    "}\n"
                    "const stack = [];\n"
                    "let currentIndex = -1;\n"
                    "let currentItemChildModelVal = null;\n"
                    "let bindingDeps = new Set();\n"
                    "let activeEval = null;\n"
                    "let inDelegateStore = false;\n"
                    "const scChildModelsSeq = { n: 0 };\n"
                    "const breadcrumbView = { stackviewContentDestroyed() { destroyedCallbacks++; } };\n"
                    "const contentContainer = {};\n"
                    "function makeContentView(model, scOwnsModel) {\n"
                    "    let _childModels = [];\n"
                    "    let _rev = 0;\n"
                    "    const cv = {\n"
                    "        model,\n"
                    "        scOwnsModel,\n"
                    "        parented: [],\n"
                    "        get childModels() { if (activeEval) activeEval.add('childModels'); return _childModels; },\n"
                    "        set childModels(v) { _childModels = v; notifyProp(cv, 'childModels'); },\n"
                    "        get scChildModelsRev() {\n"
                    "            assert.equal(inDelegateStore && !activeEval, false, 'binding loop: store read scChildModelsRev');\n"
                    "            if (activeEval) activeEval.add('scChildModelsRev');\n"
                    "            return _rev;\n"
                    "        },\n"
                    "        set scChildModelsRev(v) { if (v !== _rev) { _rev = v; notifyProp(cv, 'scChildModelsRev'); } },\n"
                    "    };\n"
                    "    return cv;\n"
                    "}\n"
                    "function evalCurrentItemChildModel() {\n"
                    "    const deps = new Set();\n"
                    "    activeEval = deps;\n"
                    "    try {\n"
                    "        currentItemChildModelVal = (currentIndex >= 0) ? (" + bv_expr + ") : null;\n"
                    "    } finally {\n"
                    "        activeEval = null;\n"
                    "    }\n"
                    "    bindingDeps = deps;\n"
                    "}\n"
                    "function notifyProp(cv, prop) {\n"
                    "    if (cv === stackView.currentItem && bindingDeps.has(prop)) evalCurrentItemChildModel();\n"
                    "}\n"
                    "function runGeneratedDestructor(cv) {\n"
                    "    const scOwnsModel = cv.scOwnsModel;\n"
                    "    const model = (cv.model && cv.model.alive) ? cv.model : null;\n"
                    + destructor_body + "\n"
                    "    // Qt QObject::~QObject also destroys direct QObject children parented to contentView.\n"
                    "    for (const m of cv.parented) if (m && m.alive) m.destroy();\n"
                    "}\n"
                    "const stackView = {\n"
                    "    get depth() { return stack.length; },\n"
                    "    get currentItem() { return stack.length ? stack[stack.length - 1] : null; },\n"
                    "    get(i, mode) { assert.equal(mode, StackView.DontLoad); return stack[i]; },\n"
                    "    replace(cv) {\n"
                    "        const old = stack.pop();\n"
                    "        if (old) runGeneratedDestructor(old);\n"
                    "        stack.push(cv);\n"
                    "        evalCurrentItemChildModel();\n"
                    "    },\n"
                    "    push(cv) { stack.push(cv); evalCurrentItemChildModel(); },\n"
                    "    pop() {\n"
                    "        const old = stack.pop();\n"
                    "        if (old) runGeneratedDestructor(old);\n"
                    "        evalCurrentItemChildModel();\n"
                    "    },\n"
                    "    scNotifyChildModels(cv) {" + notify_body + "},\n"
                    "    scReleaseChildModel(m) {" + release_body + "},\n"
                    "};\n"
                    "contentContainer.stackView = stackView;\n"
                    "function storeChildModel(contentView, rowIndex, newChildModel) {\n"
                    "    contentView.parented.push(newChildModel);\n"
                    "    inDelegateStore = true;\n"
                    "    try {\n"
                    + store_body + "\n"
                    "    } finally {\n"
                    "        inDelegateStore = false;\n"
                    "    }\n"
                    "}\n"
                    "function runRowsRemoved(contentView, first, last) {\n"
                    + splice_body + "\n"
                    "}\n"
                    "function runRowsInserted(contentView) {\n"
                    + clear_body + "\n"
                    "}\n"
                    "function pushSelectedFolder() {\n"
                    "    const list = stackView.currentItem;\n"
                    "    if (!list || !currentItemChildModelVal) return false;\n"
                    "    if (!currentItemChildModelVal.hasChildren) return false;\n"
                    "    stackView.push(makeContentView(currentItemChildModelVal, false));\n"
                    "    return true;\n"
                    "}\n"
                    "\n"
                    "// 1. 50 source switches (StackViewHandler.replace with scOwnsModel=true) run the actual generated\n"
                    "//    Component.onDestruction handler and must release all 49 replaced root models.\n"
                    "for (let i = 0; i < 50; i++) {\n"
                    "    stackView.replace(makeContentView(makeModel(), true));\n"
                    "    drainDeferredDeletes();\n"
                    "}\n"
                    "assert.equal(destroyedCallbacks, 49);\n"
                    "assert.equal(live, 1, 'only active root model stays live (fails if generated model.destroy() is disabled)');\n"
                    "assert.equal(doubleDestroyed, 0);\n"
                    "\n"
                    "// 2. Same-row refresh of highlighted folder (Qt var-array non-notification regression check):\n"
                    "//    must update currentItemChildModel to replacement (2,2,2) BEFORE releasing serial 1,\n"
                    "//    and stock push() must still open the highlighted folder without reselecting.\n"
                    "const rootItem = stackView.currentItem;\n"
                    "const firstFolder = makeModel(true);\n"
                    "storeChildModel(rootItem, 0, firstFolder);\n"
                    "currentIndex = 0;\n"
                    "evalCurrentItemChildModel();\n"
                    "assert.equal([firstFolder.serial, rootItem.childModels[0].serial, currentItemChildModelVal ? currentItemChildModelVal.serial : 0].join(','),\n"
                    "    [firstFolder.serial, firstFolder.serial, firstFolder.serial].join(','));\n"
                    "const refreshedFolder = makeModel(true);\n"
                    "storeChildModel(rootItem, 0, refreshedFolder);\n"
                    "drainDeferredDeletes();\n"
                    "assert.equal(firstFolder.alive, false, 'replaced folder model destroyed after refresh');\n"
                    "assert.equal(refreshedFolder.alive, true, 'replacement folder model stays live');\n"
                    "assert.equal(currentItemChildModelVal, refreshedFolder, 'currentItemChildModel updated to replacement (2,2,2)');\n"
                    "assert.equal(pushSelectedFolder(), true, 'highlighted folder still opens after same-row refresh');\n"
                    "assert.equal(stackView.depth, 2);\n"
                    "assert.equal(stackView.currentItem.model, refreshedFolder);\n"
                    "stackView.pop();\n"
                    "drainDeferredDeletes();\n"
                    "assert.equal(refreshedFolder.alive, true, 'borrowed folder model owned by root survives pop');\n"
                    "assert.equal(stackView.depth, 1);\n"
                    "\n"
                    "// 3. 4 scroll sweeps x 50 rows keep live childModels bounded (1 root + 50 rows).\n"
                    "for (let sweep = 0; sweep < 4; sweep++) {\n"
                    "    for (let row = 0; row < 50; row++) {\n"
                    "        storeChildModel(rootItem, row, makeModel(true));\n"
                    "    }\n"
                    "    drainDeferredDeletes();\n"
                    "}\n"
                    "assert.equal(live, 51, '1 root + 50 active row childModels after 4 sweeps');\n"
                    "assert.equal(doubleDestroyed, 0);\n"
                    "\n"
                    "// 4. Borrowed model used by a pushed page survives same-row refresh and is destroyed by\n"
                    "//    the generated destructor only when that page pops.\n"
                    "const pushedModel = rootItem.childModels[5];\n"
                    "stackView.push(makeContentView(pushedModel, false));\n"
                    "storeChildModel(rootItem, 5, makeModel(true));\n"
                    "drainDeferredDeletes();\n"
                    "assert.equal(pushedModel.alive, true, 'borrowed model stays alive while pushed page uses it');\n"
                    "assert.equal(stack[1].scOwnsModel, true, 'ownership transferred to pushed page');\n"
                    "stackView.pop();\n"
                    "drainDeferredDeletes();\n"
                    "assert.equal(pushedModel.alive, false, 'transferred model destroyed by generated destructor on pop');\n"
                    "assert.equal(live, 51);\n"
                    "\n"
                    "// 5. Generated onRowsRemoved updates currentItemChildModel before releasing removed rows,\n"
                    "//    and generated onRowsInserted releases unreferenced rows while sparing stacked models.\n"
                    "currentIndex = 0;\n"
                    "evalCurrentItemChildModel();\n"
                    "const row0BeforeSplice = rootItem.childModels[0];\n"
                    "const row1BeforeSplice = rootItem.childModels[1];\n"
                    "runRowsRemoved(rootItem, 0, 0);\n"
                    "drainDeferredDeletes();\n"
                    "assert.equal(row0BeforeSplice.alive, false, 'spliced row 0 destroyed');\n"
                    "assert.equal(currentItemChildModelVal, row1BeforeSplice, 'selection updated to shifted row before release');\n"
                    "assert.equal(live, 50);\n"
                    "rootItem.childModels[55] = undefined;\n"
                    "runRowsRemoved(rootItem, 10, 19);\n"
                    "drainDeferredDeletes();\n"
                    "assert.equal(live, 40);\n"
                    "const pushedBeforeClear = rootItem.childModels[2];\n"
                    "stackView.push(makeContentView(pushedBeforeClear, false));\n"
                    "runRowsInserted(rootItem);\n"
                    "drainDeferredDeletes();\n"
                    "assert.equal(pushedBeforeClear.alive, true, 'stacked model survives onRowsInserted clear');\n"
                    "assert.equal(live, 2, 'only root + stacked model stay live after clear');\n"
                    "stackView.pop();\n"
                    "drainDeferredDeletes();\n"
                    "assert.equal(pushedBeforeClear.alive, false, 'stacked model destroyed on pop');\n"
                    "assert.equal(live, 1);\n"
                    "\n"
                    "// 6. deepSelect push (scOwnsModel=true) releases model via generated destructor on pop.\n"
                    "const deepM = makeModel(true);\n"
                    "stackView.push(makeContentView(deepM, true));\n"
                    "assert.equal(live, 2);\n"
                    "stackView.pop();\n"
                    "drainDeferredDeletes();\n"
                    "assert.equal(deepM.alive, false, 'deepSelect owned model destroyed by generated destructor on pop');\n"
                    "assert.equal(live, 1);\n"
                    "assert.equal(doubleDestroyed, 0);\n"
                )
                result = subprocess.run(["node"], input=script, text=True, capture_output=True)
                self.assertEqual(result.returncode, 0, f"{label}: {result.stderr}")

    def test_non_browse_album_art_images_bound_source_size_and_async_on_both_bases(self):
        ed53 = self._edits_for("Skins/Embedded/")
        for rel, expected_sz in (
            ("Skins/EngineOS/UI/CompositeControls/Deck/DeckItem/AlbumArt.qml", "Qt.size(256, 256)"),
            ("Skins/EngineOS/UI/CompositeControls/Deck/MicroDeck.qml", "Qt.size(160, 160)"),
            ("Skins/EngineOS/UI/CompositeControls/LoadOverlay/LoadOverlayTrackInfo.qml", "Qt.size(160, 160)"),
            ("Skins/Embedded/Items/TrackInfo.qml", "Qt.size(144, 144)"),
        ):
            with self.subTest(base="5.3.0", rel=rel):
                self.assertTrue(rel in ed53, f"5.3.0: missing edits for {rel}")
                joined = self._join_replacements(ed53[rel])
                self.assertIn(f"sourceSize: {expected_sz}", joined)
                self.assertIn("asynchronous: true", joined)
                if rel.endswith("/TrackInfo.qml"):
                    self.assertEqual(joined.count(f"sourceSize: {expected_sz}"), 2)
                    self.assertEqual(joined.count("asynchronous: true"), 2)

        ed50 = self._edits_for("Skins/EmbeddedCommon/")
        for rel, expected_sz, count in (
            ("Skins/EmbeddedCommon/Items/TrackInfo.qml", "Qt.size(144, 144)", 2),
            ("Skins/EmbeddedCommon/Views/LoadToDeckMenu/LoadToDeckTrackInfo.qml", "Qt.size(144, 144)", 1),
        ):
            with self.subTest(base="5.0.4", rel=rel):
                self.assertTrue(rel in ed50, f"5.0.4: missing edits for {rel}")
                joined = self._join_replacements(ed50[rel])
                self.assertEqual(joined.count(f"sourceSize: {expected_sz}"), count)
                self.assertEqual(joined.count("asynchronous: true"), count)

        # Row 69 / Row 185 pin: BrowseView AlbumArt placeholder & native image://planck handling stay intact.
        for root in ("Skins/Embedded/", "Skins/EmbeddedCommon/"):
            ed = self._edits_for(root)
            browse_art = self._join_replacements(ed[root + "Views/BrowseView/Components/Components/AlbumArt.qml"])
            self.assertIn('objectName: "albumArt_placeholder"', browse_art)
            self.assertIn('row.modelData.albumArt === "image://streaming/"', browse_art)

    def test_generated_packs_compile_and_contain_s7_fixes_when_lab_present(self):
        lab = Path(legacy.LAB)
        lab_roots = [
            ("5.3.0", lab / "5.3.0/qrc", "Skins/Embedded/"),
            ("5.0.4", lab / "5.0.4/qrc", "Skins/EmbeddedCommon/"),
        ]
        found_any = False
        import tempfile
        for ver, qrc, root in lab_roots:
            if not (qrc / root / "EmbeddedMain.qml").is_file():
                continue
            found_any = True
            with tempfile.TemporaryDirectory() as tmp:
                out = Path(tmp) / "packs"
                res = subprocess.run(
                    ["python3", str(Path(__file__).resolve().parents[1] / "ui-mod/build_packs.py"),
                     "--qrc", str(qrc), "--out", str(out)],
                    capture_output=True, text=True,
                )
                self.assertEqual(res.returncode, 0, f"{ver} build_packs failed: {res.stderr}")
                for lang in packs.LANGUAGES:
                    pdir = out / lang
                    svh = (pdir / root / "Views/BrowseView/Components/BreadcrumbView/Handlers/StackViewHandler.qml").read_text()
                    bsv = (pdir / root / "Views/BrowseView/Components/BreadcrumbView/BreadcrumbStackView.qml").read_text()
                    bv = (pdir / root / "Views/BrowseView/Components/BreadcrumbView/BreadcrumbView.qml").read_text()
                    ti = (pdir / root / "Items/TrackInfo.qml").read_text()
                    self.assertIn("scOwnsModel: true", svh)
                    self.assertIn("function scNotifyChildModels(cv)", bsv)
                    self.assertIn("function scReleaseChildModel(m)", bsv)
                    self.assertIn("scChildModelsRev", bv)
                    self.assertEqual(ti.count("sourceSize: Qt.size(144, 144)"), 2)
                    self.assertEqual(ti.count("asynchronous: true"), 2)
                    # BrowseView AlbumArt Row 69/185 untouched across all languages.
                    ba = (pdir / root / "Views/BrowseView/Components/Components/AlbumArt.qml").read_text()
                    self.assertIn('objectName: "albumArt_placeholder"', ba)
                    self.assertIn("sourceSize.width: parent.height", ba)
                    if ver == "5.3.0":
                        for rel, sz in (
                            ("Skins/EngineOS/UI/CompositeControls/Deck/DeckItem/AlbumArt.qml", "Qt.size(256, 256)"),
                            ("Skins/EngineOS/UI/CompositeControls/Deck/MicroDeck.qml", "Qt.size(160, 160)"),
                            ("Skins/EngineOS/UI/CompositeControls/LoadOverlay/LoadOverlayTrackInfo.qml", "Qt.size(160, 160)"),
                        ):
                            txt = (pdir / rel).read_text()
                            self.assertIn(f"sourceSize: {sz}", txt, f"{ver}/{lang}/{rel}")
                            self.assertIn("asynchronous: true", txt, f"{ver}/{lang}/{rel}")
                    else:
                        lt = (pdir / root / "Views/LoadToDeckMenu/LoadToDeckTrackInfo.qml").read_text()
                        self.assertIn("sourceSize: Qt.size(144, 144)", lt)
                        self.assertIn("asynchronous: true", lt)
        if not found_any:
            self.skipTest("no firmware QRC lab directory on this host")


if __name__ == "__main__":
    unittest.main()
