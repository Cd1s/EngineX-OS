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
        names = ("scDeckLocked", "scShadowVisible", "scTr", "scCuePlaying", "scProbeCueClocks", "scCueBeatOf", "scCuePosAt", "scCueBeatTable", "scCueProps", "scCueBeatsLeft", "scNextCueInfo", "scNextCueCompute", "scCueTickFor", "scSameItems", "scCall", "scUiPollReply")
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
        running = re.findall(r"id: scCueClock[1-4].*?running: ([^\n]+)", packs.SERVICES_MAIN, re.S)
        self.assertEqual(len(running), 4)
        script = r'''
const assert = require('node:assert/strict');
const root = globalThis;
var scShadowDisplayOnly = false, scShadowEngineMask = 30, scFourDeckRole = "", scPlayer = false, scShadowAt = {}, scShadowDisplay = {};
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
''' + "\n".join(functions) + "\nroot.scTr = scTr; root.scCuePlaying = scCuePlaying; root.scCueProbe = 0; root.scCueBeatOf = scCueBeatOf; root.scCueBeatsCache = {}; root.scCueSnap = {}; root.scCuePosPaths = ['', ...[1, 2, 3, 4].map(d => '/Engine/Deck' + d + '/Track/TrackPosition')];\nroot.scCueBeatPaths = ['', ...[1, 2, 3, 4].map(d => '/Engine/Deck' + d + '/BeatNumber')]; root.scCueBarPaths = ['', ...[1, 2, 3, 4].map(d => '/Engine/Deck' + d + '/Track/BarsElapsed')];\n" + "".join(f"Object.defineProperty(root, 'scCueBeat{d}', {{get() {{ return scCueBeatOf({d}, root.scCueProbe); }}, configurable: true}});\n" for d in range(1, 5)) + r'''
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
// A local deck always counts afresh, on a fallback-clock tick (negative) or a beat tick (positive): an edit, jump or reload inside the
// same tick must get its own answer, and a reused answer would skip the reads the binding is subscribed through.
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
assert.equal(scCueTickFor(2), -3); // the fallback clock's ticks are negative
assert.deepEqual(reads, [2]); // deck 2 never depends on the other three clocks
root.scCueCountdown = true;
set('PlayState', {state: true});
// On the beat Engine's own beat counter drives the countdown: no clock runs, PlayState or not (CUE preview does not set it).
set('BeatNumber', {unnormalized: 2.5});
''' + "\n".join(f"assert.equal(!!({expr}), false);" for expr in running) + r'''
assert.equal(scCueBeat1, 100002); assert.equal(scCueTickFor(1), 100004);
set('Track/BarsElapsed', {unnormalized: 3, string: '3'});
assert.equal(scCueBeat1, 100014); // the bar counts: 3 bars + beat 2
set('BeatNumber', {unnormalized: 2.9}); assert.equal(scCueBeat1, 100014); // inside a beat the key does not move
set('BeatNumber', {unnormalized: 3.0}); assert.equal(scCueBeat1, 100015);
// Bars before the first one are negative and keep their own keys (they were all squeezed into one).
set('Track/BarsElapsed', {unnormalized: -1, string: '-1'}); const k1 = scCueBeat1;
set('Track/BarsElapsed', {unnormalized: -2, string: '-2'}); const k2 = scCueBeat1;
assert.ok(k1 > 0 && k2 > 0 && k1 !== k2 && k1 - k2 === 4 && k1 !== 100015, k1 + ' ' + k2);
set('Track/BarsElapsed', {unnormalized: 3, string: '3'});
// No grid (BeatNumber below 0) or no beat property: the 200 ms clock is the fallback, while the deck plays.
set('BeatNumber', {unnormalized: -1});
assert.equal(scCueBeat1, -1);
''' + "\n".join(f"assert.equal(!!({expr}), {str(i == 0).lower()});" for i, expr in enumerate(running)) + r'''
// CUE held on a deck without a grid: PlayState is off, the CUE lamp is on, the fallback clock must run (it did not).
set('PlayState', {state: false}); set('Track/CueLEDState', {state: true});
''' + "\n".join(f"assert.equal(!!({expr}), {str(i == 0).lower()});" for i, expr in enumerate(running)) + r'''
set('Track/CueLEDState', {state: false});
''' + "\n".join(f"assert.equal(!!({expr}), false);" for expr in running) + r'''
set('PlayState', {state: true});
unavailable.add('/Engine/Deck2/BeatNumber'); delete scPropCache['/Engine/Deck2/BeatNumber'];
assert.equal(scCueBeat2, -2); // no beat property: the clock again
unavailable.delete('/Engine/Deck2/BeatNumber'); assert.equal(scCueBeat2, 100000);
void scCueBeat3; void scCueBeat4; // the readonly properties evaluate on their own at startup

root.scCueCountdown = false;
''' + "\n".join(f"assert.equal(!!({expr}), false);" for expr in running) + r'''
root.scCueCountdown = true;
const missingPath = '/Engine/Deck3/PlayState';
delete scPropCache[missingPath]; unavailable.add(missingPath);
assert.equal(scCuePlaying(3, root.scCueProbe), false);
scProbeCueClocks(); assert.equal(root.scCueProbe, 1); // settings poll wakes missing-property bindings
unavailable.delete(missingPath); properties[missingPath].state = true;
assert.equal(scCuePlaying(3, root.scCueProbe), true);
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
// External WebUI / helper sign-in adds active scope -> closes QR, invalidates playlists, enters list.
scUiPoll.headers = {'X-UI-Digest': 'd1'};
scUiPoll.responseText = JSON.stringify({loggedIn: ['spotify', 'applemusic']});
scUiPollReply();
assert.equal(scLogin, '');
assert.equal(invalidated, 1);
assert.equal(entered, 1);
assert.equal(scEnterTries, 8);

// "Switch account" opens QR while already logged in -> unchanged membership must not close QR.
scLogin = 'applemusic';
scUiPoll.headers = {'X-UI-Digest': 'd2'};
scUiPoll.responseText = JSON.stringify({loggedIn: ['spotify', 'applemusic']});
scUiPollReply();
assert.equal(scLogin, 'applemusic');
assert.equal(invalidated, 1);
assert.equal(entered, 1);

// Remote logout removes active scope -> invalidates stale playlists without auto-entering.
scLogin = '';
scUiPoll.headers = {'X-UI-Digest': 'd3'};
scUiPoll.responseText = JSON.stringify({loggedIn: ['spotify']});
scUiPollReply();
assert.equal(invalidated, 2);
assert.equal(entered, 1);
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


if __name__ == "__main__":
    unittest.main()
