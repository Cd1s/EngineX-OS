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
                self.assertAlmostEqual(len(selected) / original_entries, 1 / 3)
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
        names = ("scDeckLocked", "scShadowVisible", "scTr", "scCuePlaying", "scProbeCueClocks", "scCueProps", "scNextCueInfo", "scNextCueCompute", "scCueTickFor", "scSameItems", "scCall", "scUiPollReply")
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
''' + "\n".join(functions) + "\nroot.scTr = scTr; root.scCuePlaying = scCuePlaying; root.scCueProbe = 0;\n" + r'''
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
// Every deck view evaluates on the same clock edge: computed once per deck and tick.
const edge = scNextCueInfo(1, 7), before = lookups;
set('Track/TrackPosition', {unnormalized: 3000});
assert.strictEqual(scNextCueInfo(1, 7), edge);
scCueInfoMemo[1].at -= 100; // a later re-evaluation on the same tick (a load or cue edit while paused)
assert.equal(scNextCueInfo(1, 7).text, '3.1'); assert.equal(edge.text, '4.1');
set('Track/TrackPosition', {unnormalized: 1000});
set('Track/SongLoaded', {state: false});
assert.equal(scNextCueInfo(1, 2), null);
assert.equal(scCueTickFor(2), 2);
assert.deepEqual(reads, [2]); // deck 2 never depends on the other three clocks
root.scCueCountdown = true;
set('PlayState', {state: true});
''' + "\n".join(f"assert.equal(!!({expr}), {str(i == 0).lower()});" for i, expr in enumerate(running)) + r'''
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


if __name__ == "__main__":
    unittest.main()
