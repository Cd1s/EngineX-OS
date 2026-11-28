"""Regression tests for #336 E4: settings must take effect on a single click/gesture.

Covers:
1. SwitchEntry.qml (5.0.4, 5.3.0, UIKit) keeps T.Switch.checked in sync with checkedExternally
   so touch slides/drags on default-ON or externally updated switches emit toggled() on the
   first gesture, while leaving stock checked:-only bindings untouched.
2. STREAM_SWITCHES uses checkedExternally (not checked) and scSetSource updates scSources
   optimistically so rapid multi-service toggles never re-enable the first service.
3. scCall serializes setting POSTs in FIFO order (with safe adjacent single-key coalescing),
   applies setting changes optimistically, updates scUiDigest/scUiAppliedAt on completion,
   resyncs on non-200 failure, and blocks stale scUiPoll replies started before a write.
4. scTunnelToggle (Remote access) and scBtLoad/scBtSet (Bluetooth Output Source) apply
   optimistically, serialize rapid updates, and ignore stale polls/loads.
"""
import importlib.util
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import legacy_names as legacy

REPO = Path(__file__).resolve().parents[2]
BUILD_PACKS = REPO / "tools" / "ui-mod" / "build_packs.py"
LAB = Path(legacy.LAB)


def load_build_packs():
    spec = importlib.util.spec_from_file_location("build_packs", BUILD_PACKS)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def extract_function(text, name, optional=False):
    m = re.search(rf"(?m)^\tfunction {re.escape(name)}\([^\n]*\) \{{", text)
    if not m:
        if optional:
            return ""
        raise AssertionError(f"missing function {name}")
    i = m.end() - 1
    depth = 0
    for j in range(i, len(text)):
        if text[j] == "{":
            depth += 1
        elif text[j] == "}":
            depth -= 1
            if depth == 0:
                return text[m.start():j + 1]
    raise AssertionError(f"unclosed function {name}")


def run_node(script):
    res = subprocess.run(["node", "-e", script], capture_output=True, text=True)
    if res.returncode != 0:
        raise AssertionError(f"node failed ({res.returncode}):\nSTDOUT:\n{res.stdout}\nSTDERR:\n{res.stderr}")
    return res.stdout


class SettingsSingleClickE4Test(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.bp = load_build_packs()

    def test_switch_entry_syncs_checked_with_checked_externally_and_stream_switches_use_it(self):
        for version, skin in (("5.0.4", "EmbeddedCommon"), ("5.3.0", "Embedded")):
            qrc = LAB / version / "qrc"
            if not qrc.is_dir():
                self.skipTest(f"{qrc} not found (run on build host)")
            with tempfile.TemporaryDirectory(prefix=f"e4-sw-{version}-") as tmp:
                out = Path(tmp) / "packs"
                built = subprocess.run(
                    ["python3", str(BUILD_PACKS), "--qrc", str(qrc), "--out", str(out)],
                    capture_output=True, text=True,
                )
                self.assertEqual(built.returncode, 0, built.stderr)
                for lang in ("en", "zh-Hans", "zh-Hant", "es"):
                    with self.subTest(version=version, lang=lang):
                        sw = (out / lang / f"Skins/{skin}/Views/ControlCenter/Tabs/Preferences/Entries/Base/SwitchEntry.qml").read_text(encoding="utf-8")
                        self.assertIn("property alias checkedExternally: control.checkedExternally", sw)
                        self.assertIn("Component.onCompleted: if (checked !== checkedExternally) checked = checkedExternally", sw)
                        self.assertIn("onCheckedExternallyChanged: if (checked !== checkedExternally) checked = checkedExternally", sw)
                        self.assertIn("parent.clicked()", sw)
                        self.assertRegex(sw, r"onToggled:\s*\{[\s\S]*parent\.clicked\(\)[\s\S]*if \(checked !== checkedExternally\) checked = checkedExternally[\s\S]*\}")
                        streaming = (out / lang / f"Skins/{skin}/Views/ControlCenter/Tabs/Preferences/Entries/StreamingServices.qml").read_text(encoding="utf-8")
                        self.assertIn("checkedExternally: root.scSources.indexOf(modelData.id) >= 0", streaming)
                        self.assertNotIn("checked: root.scSources.indexOf(modelData.id) >= 0", streaming)
                        if version == "5.3.0":
                            uikit_sw = (out / lang / "Skins/UIKit/Elements/Settings/Entries/SwitchEntry.qml").read_text(encoding="utf-8")
                            self.assertIn("Component.onCompleted: if (checked !== checkedExternally) checked = checkedExternally", uikit_sw)
                            self.assertIn("onCheckedExternallyChanged: if (checked !== checkedExternally) checked = checkedExternally", uikit_sw)

    @unittest.skipUnless(shutil.which("node"), "Node is required")
    def test_inflight_poll_and_stale_digest_never_clobber_setting_toggle(self):
        main = self.bp.SERVICES_MAIN
        code = "\n".join(filter(None, [
            extract_function(main, "scSameItems"),
            extract_function(main, "scFxMsInteger"),
            extract_function(main, "scFxMsApply"),
            extract_function(main, "scUiReplyRev", optional=True),
            extract_function(main, "scSourceWritePending", optional=True),
            extract_function(main, "scSyncLoginDialog", optional=True),
            extract_function(main, "scApplyQuery", optional=True),
            extract_function(main, "scUiSyncPending", optional=True),
            extract_function(main, "scUiFlush", optional=True),
            extract_function(main, "scCall"),
            extract_function(main, "scUiPollReply"),
        ]))
        script = f"""
const assert = require('node:assert/strict');
const reqs = [];
class XMLHttpRequest {{
    constructor() {{ this.readyState = 0; this.status = 0; this.responseText = ''; this.headers = {{}}; this.aborted = false; }}
    open(m, u) {{ this.method = m; this.url = u; this.readyState = 1; }}
    send() {{ reqs.push(this); }}
    abort() {{ this.aborted = true; this.readyState = 0; }}
    getResponseHeader(k) {{ return this.headers[k] || ''; }}
    finish(status, body, digest = '') {{
        this.status = status;
        this.responseText = typeof body === 'string' ? body : JSON.stringify(body);
        this.headers = digest ? {{'X-UI-Digest': digest}} : {{}};
        this.readyState = 4;
        if (this.onreadystatechange) this.onreadystatechange();
    }}
}}
let scScope = '', scScopePending = 0, scUiPoll = null, scUiPollScope = '', scUiPollAt = 0, scUiDigest = '', scUiAppliedAt = 0;
let scUiWriteGen = 0, scUiPollGen = 0, scUiPending = 0, scUiQueue = [], scUiSending = false, scUiSendingAt = 0, scUiSendToken = 0, scUiSendXhr = null, scUiHadError = false;
let scTunnelPending = 0, scUiLoaded = true, scIsPlayerBoard = true, scConsoleLineBoard = false, scFourDeckOn = false, scHasMixer = false, scTransitionFxView = null;
let scMasterLimiter = false, scAutoGain = false, scLedBrightness = 100, scLedBrightnessSaved = 100, scSources = ['tidal', 'spotify'], scLoggedIn = [];
let scFxTimeMode = 'beats', scFxTimeMs = 500, scFxMsWanted = -1;
let scPadDelayRestarts = 0;
const scPadBrightnessDelay = {{ restart() {{ scPadDelayRestarts++; }} }};
let applyCount = 0;
function scEnsureTidal() {{}}
function scOpen(s) {{ scScope = s; }}
function scApply(v) {{
    applyCount++;
    if (v.loggedIn) scLoggedIn = v.loggedIn;
    if (v.masterLimiter !== undefined) scMasterLimiter = !!v.masterLimiter;
    if (v.autoGain !== undefined) scAutoGain = !!v.autoGain;
    if (v.ledBrightness !== undefined) {{
        const brt = parseInt(v.ledBrightness) || 100;
        scLedBrightness = brt;
        scLedBrightnessSaved = brt;
    }}
    if (v.sources) scSources = v.sources;
}}
{code}
// 1. Initial poll completes with d0.
scCall('');
reqs[0].finish(200, {{masterLimiter: false, autoGain: false, loggedIn: []}}, 'd0');
assert.equal(scUiDigest, 'd0');
// 2. User enables Auto-Gain -> immediate optimistic update and digest updated to d1 on reply.
scCall('autoGain=1');
assert.equal(scAutoGain, true, 'autoGain must flip optimistically');
reqs[1].finish(200, {{masterLimiter: false, autoGain: true, loggedIn: []}}, 'd1');
assert.equal(scUiDigest, 'd1', 'POST reply must advance scUiDigest');
// 3. 3s poll starts (captures pre-write state d1), then user taps Master Soft Clipper.
scCall('');
scCall('masterLimiter=1');
const mlPost = reqs[reqs.length - 1];
assert.ok(mlPost.url.endsWith('?masterLimiter=1'));
assert.equal(scMasterLimiter, true, 'masterLimiter must flip optimistically');
// Even if POST finishes FIRST and stale poll arrives AFTER POST, stale poll must not revert masterLimiter.
mlPost.finish(200, {{masterLimiter: true, autoGain: true, loggedIn: []}}, 'd2');
assert.equal(scMasterLimiter, true);
assert.equal(scUiDigest, 'd2');
// Stale poll (reused scUiPoll) now arrives with old d1 and masterLimiter=false:
scUiAppliedAt -= 31000; // even past the 30s safety window!
scUiPoll.finish(200, {{masterLimiter: false, autoGain: true, loggedIn: []}}, 'd1');
assert.equal(scMasterLimiter, true, 'stale poll started before POST must never revert masterLimiter');
assert.equal(scUiDigest, 'd2', 'stale poll must not overwrite scUiDigest');
// 4. A fresh poll started AFTER the POST still applies external changes cleanly.
scCall('');
scUiPoll.finish(200, {{masterLimiter: false, autoGain: true, loggedIn: []}}, 'd3');
assert.equal(scMasterLimiter, false, 'fresh post-write poll must still reflect external console changes');
"""
        run_node(script)

        for case, action in {
            "read_during_write": """
scCall('masterLimiter=1');
const write = reqs.at(-1), count = reqs.length;
scCall('');
const read = reqs.length > count ? reqs.at(-1) : null;
write.finish(200, {masterLimiter: true}, 'accepted');
if (read) read.finish(200, {masterLimiter: false}, 'old');
assert.equal(scMasterLimiter, true, 'read during write reverted acknowledged switch');
""",
            "older_callback_read": """
let callbackCount = 0;
scCall('', () => callbackCount++);
const read = reqs.at(-1);
scCall('masterLimiter=1');
reqs.at(-1).finish(200, {masterLimiter: true}, 'accepted');
read.finish(200, {masterLimiter: false}, 'old');
assert.equal(callbackCount, 1);
assert.equal(scMasterLimiter, true, 'callback GET reverted acknowledged switch');
""",
            "hung_first_post": """
const zero = Date.now();
Date.now = () => zero;
scCall('masterLimiter=1');
const hungFirst = reqs.at(-1);
scCall('autoGain=0');
for (let n = 1; n <= 8; n++) {
    Date.now = () => zero + n * 3000;
    scCall('');
    const x = reqs.at(-1);
    if (x.method === 'GET' && x.readyState !== 4) x.finish(200, {autoGain: true}, 'remote');
}
const secondPost = reqs.find(x => x.method === 'POST' && x.url.endsWith('?autoGain=0'));
assert.ok(secondPost, 'second tap still unsent after eight watchdog ticks');
secondPost.finish(200, {masterLimiter: false, autoGain: false}, 'after-second');
assert.equal(scAutoGain, false);
assert.equal(scMasterLimiter, false);
// Delayed reply from the timed-out first POST must never supersede newer state.
hungFirst.finish(200, {masterLimiter: true, autoGain: true}, 'late-hung');
assert.equal(scAutoGain, false, 'delayed reply from timed-out POST must be ignored');
assert.equal(scMasterLimiter, false, 'delayed reply from timed-out POST must be ignored');
""",
            "hung_single_post_reconciles_state": """
const zero = Date.now();
Date.now = () => zero;
scCall('masterLimiter=1');
const hungOnly = reqs.at(-1);
assert.equal(scMasterLimiter, true);
for (let n = 1; n <= 7; n++) {
    Date.now = () => zero + n * 3000;
    scCall('');
}
const reconGet = reqs.at(-1);
assert.equal(reconGet.method, 'GET', 'watchdog must issue a fresh GET when no queued POST remains');
reconGet.finish(200, {masterLimiter: false}, 'reconciled');
assert.equal(scMasterLimiter, false, 'reconciled GET restores confirmed server state');
hungOnly.finish(200, {masterLimiter: true}, 'late-hung');
assert.equal(scMasterLimiter, false, 'late timed-out POST must not override reconciled state');
""",
            "login_on_write_reply": """
globalThis.root = globalThis;
globalThis.scLoginOpen = 'spotify';
globalThis.scLogin = 'spotify';
globalThis.scLoginReqGen = 0;
globalThis.scUiRev = 0;
globalThis.scPendingTap = '';
scCall('masterLimiter=1');
reqs.at(-1).finish(200, {masterLimiter: true, loggedIn: ['spotify'], rev: 2}, 'login');
assert.equal(scScope, '', 'external sign-in on write reply must stay passive (never auto-open scope)');
assert.equal(scLoginOpen, '');
assert.equal(scLogin, '');
assert.ok(scLoginReqGen >= 1, 'closing login dialog must bump scLoginReqGen');
scCall('');
reqs.at(-1).finish(200, {masterLimiter: true, loggedIn: ['spotify'], rev: 2}, 'login');
assert.equal(scScope, '', 'poll after passive close must not auto-open scope');
""",
            "qr_callback_during_write": extract_function(main, "scLoginDone") + """
globalThis.root = globalThis;
globalThis.scLoginOpen = 'spotify';
globalThis.scLogin = 'spotify';
globalThis.scLoginReqGen = 0;
globalThis.scUiRev = 0;
globalThis.scPendingTap = '';
let enterStarts = 0;
globalThis.scEnterTries = 0;
globalThis.scEnterTimer = {start() { enterStarts++; }};
globalThis.Planck = {librarianController: {streamingPlaylistsController: {invalidateUI() {}}}};
scCall('masterLimiter=1');
const writeReq = reqs.at(-1);
scLoginDone();
assert.equal(scLogin, '', 'scLoginDone closes QR dialog immediately');
assert.equal(scLoginOpen, '');
assert.equal(scScope, '', 'scLoginDone must stay passive and never auto-open scope');
assert.equal(enterStarts, 0, 'scLoginDone must never start scEnterTimer');
writeReq.finish(200, {masterLimiter: true, sources: ['spotify'], loggedIn: ['spotify'], rev: 2}, 'login');
assert.equal(scMasterLimiter, true, 'completed write keeps masterLimiter=true');
assert.deepEqual(scLoggedIn, ['spotify']);
assert.equal(scScope, '', 'completed write after passive scLoginDone must not auto-open scope');
""",
        }.items():
            with self.subTest(case=case):
                run_node(script + action)

    @unittest.skipUnless(shutil.which("node"), "Node is required")
    def test_rapid_setting_posts_serialize_coalesce_and_rollback_on_error(self):
        main = self.bp.SERVICES_MAIN
        code = "\n".join(filter(None, [
            extract_function(main, "scSameItems"),
            extract_function(main, "scFxMsInteger"),
            extract_function(main, "scFxMsApply"),
            extract_function(main, "scApplyQuery", optional=True),
            extract_function(main, "scSyncPadBrightness", optional=True),
            extract_function(main, "scUiSyncPending", optional=True),
            extract_function(main, "scUiFlush", optional=True),
            extract_function(main, "scCall"),
            extract_function(main, "scUiPollReply"),
        ]))
        script = f"""
const assert = require('node:assert/strict');
const reqs = [];
class XMLHttpRequest {{
    constructor() {{ this.readyState = 0; this.status = 0; this.responseText = ''; this.headers = {{}}; this.aborted = false; }}
    open(m, u) {{ this.method = m; this.url = u; this.readyState = 1; }}
    send() {{ reqs.push(this); }}
    abort() {{ this.aborted = true; this.readyState = 0; }}
    getResponseHeader(k) {{ return this.headers[k] || ''; }}
    finish(status, body, digest = '') {{
        this.status = status;
        this.responseText = typeof body === 'string' ? body : JSON.stringify(body);
        this.headers = digest ? {{'X-UI-Digest': digest}} : {{}};
        this.readyState = 4;
        if (this.onreadystatechange) this.onreadystatechange();
    }}
}}
let scScope = '', scScopePending = 0, scUiPoll = null, scUiPollScope = '', scUiPollAt = 0, scUiDigest = 'd0', scUiAppliedAt = Date.now();
let scUiWriteGen = 0, scUiPollGen = 0, scUiPending = 0, scUiQueue = [], scUiSending = false, scUiSendingAt = 0, scUiSendToken = 0, scUiSendXhr = null, scUiHadError = false;
let scTunnelPending = 0, scUiLoaded = true, scIsPlayerBoard = true, scConsoleLineBoard = false, scFourDeckOn = false, scHasMixer = true;
let scMasterLimiter = false, scAutoGain = false, scSdHighSpeed = false, scLedBrightness = 100, scLedBrightnessSaved = 100, scUpfaderEcho = false;
let scPadBrightnessSynced = false;
let publishedPadBrightness = 1.0;
const scPadBrightnessProp = {{ translator: {{ get value() {{ return publishedPadBrightness; }}, set value(v) {{ publishedPadBrightness = v; }} }} }};
globalThis.Planck = {{ getProperty() {{ return scPadBrightnessProp; }} }};
let scFxTimeMode = 'beats', scFxTimeMs = 500, scFxMsWanted = -1, scSources = [], scLoggedIn = [];
let transitionSets = 0;
const scTransitionFxView = {{
    _en: false,
    get enabled() {{ return this._en; }},
    set enabled(v) {{ this._en = !!v; transitionSets++; }},
}};
let scPadDelayRestarts = 0;
const scPadBrightnessDelay = {{ restart() {{ scPadDelayRestarts++; }} }};
function scEnsureTidal() {{}}
function scOpen(s) {{ scScope = s; }}
function scApply(v) {{
    if (v.masterLimiter !== undefined) scMasterLimiter = !!v.masterLimiter;
    if (v.autoGain !== undefined) scAutoGain = !!v.autoGain;
    if (v.sdHighSpeed !== undefined) scSdHighSpeed = !!v.sdHighSpeed;
    if (v.ledBrightness !== undefined) {{
        const brt = parseInt(v.ledBrightness) || 100;
        scLedBrightness = brt;
        scLedBrightnessSaved = brt;
    }}
    if (v.upfaderEcho !== undefined) scUpfaderEcho = !!v.upfaderEcho;
}}
{code}
// 1. Rapid ListEntry taps: 80 -> 60 -> 40. Only 80 is in flight; 60 coalesces into 40.
// Optimistic picker moves to 40, while hardware pad brightness waits for confirmed saves (#325).
scCall('ledBrightness=80');
scCall('ledBrightness=60');
scCall('ledBrightness=40');
assert.equal(scLedBrightness, 40, 'optimistic state tracks latest tap');
assert.equal(scLedBrightnessSaved, 100, 'confirmed hardware brightness stays at saved level while POST is in flight');
scSyncPadBrightness();
assert.equal(publishedPadBrightness, 1.0, 'slow POST must not publish unconfirmed brightness prematurely');
assert.equal(reqs.length, 1, 'only one setting POST in flight at once');
assert.ok(reqs[0].url.endsWith('?ledBrightness=80'));
// When 80 finishes (intermediate queued ACK), picker stays 40, confirmed hardware brightness advances to 80, and 40 sends next.
reqs[0].finish(200, {{ledBrightness: 80}}, 'd80');
assert.equal(scLedBrightness, 40, 'intermediate POST reply must not clobber queued tap');
assert.equal(scLedBrightnessSaved, 80, 'intermediate confirmed ACK updates hardware brightness');
scSyncPadBrightness();
assert.equal(publishedPadBrightness, 0.8);
assert.equal(reqs.length, 2);
assert.ok(reqs[1].url.endsWith('?ledBrightness=40'), '60 coalesced into 40');
reqs[1].finish(200, {{ledBrightness: 40}}, 'd40');
assert.equal(scLedBrightness, 40);
assert.equal(scLedBrightnessSaved, 40);
scSyncPadBrightness();
assert.equal(publishedPadBrightness, 0.4);
assert.equal(scUiPending, 0);
assert.ok(scPadDelayRestarts >= 2, '#325 pad brightness delay restarted on both intermediate and final confirmations');

// 2. Rapid distinct switch toggles: Master Soft Clipper ON then Auto-Gain ON.
scCall('masterLimiter=1');
scCall('autoGain=1');
assert.equal(scMasterLimiter, true);
assert.equal(scAutoGain, true);
assert.equal(reqs.length, 3);
assert.ok(reqs[2].url.endsWith('?masterLimiter=1'));
reqs[2].finish(200, {{masterLimiter: true, autoGain: false}}, 'dml');
assert.equal(scAutoGain, true, 'first switch reply must not flicker second switch off');
assert.equal(reqs.length, 4);
assert.ok(reqs[3].url.endsWith('?autoGain=1'));
reqs[3].finish(200, {{masterLimiter: true, autoGain: true}}, 'dag');
assert.equal(scMasterLimiter, true);
assert.equal(scAutoGain, true);
assert.equal(scUiPending, 0);

// 3. Fader Echo optimistic toggle also updates scTransitionFxView.enabled immediately.
scCall('upfaderEcho=1');
assert.equal(scUpfaderEcho, true);
assert.equal(scTransitionFxView.enabled, true);
assert.equal(transitionSets, 1);
reqs[4].finish(200, {{upfaderEcho: true}}, 'due');

// 4. Failed POST (e.g. sdHighSpeed 500) resyncs from server, aborting any stale in-flight poll.
scCall(''); // reqs[5] is scUiPoll (in flight with old gen)
scCall('sdHighSpeed=1'); // reqs[6]
assert.equal(scSdHighSpeed, true);
reqs[6].finish(500, {{error: 'script failed'}});
assert.equal(scUiPending, 0);
assert.equal(scUiDigest, '', 'error clears digest');
assert.equal(scUiPoll.aborted, true, 'stale in-flight poll aborted so resync runs immediately');
// Fresh GET resync runs on scUiPoll (reqs[7]) and restores server state sdHighSpeed=false.
scUiPoll.finish(200, {{sdHighSpeed: false}}, 'dsd0');
assert.equal(scSdHighSpeed, false, 'failed setting rolled back to server state');

// 5. Hung POST (>20s) recovers when the next click arrives.
scCall('masterLimiter=0'); // reqs[8]
const hungXhr = reqs[reqs.length - 1];
scUiSendingAt = Date.now() - 25000;
scCall('autoGain=0'); // aborts hungXhr and sends autoGain=0
assert.equal(hungXhr.aborted, true);
const freshXhr = reqs[reqs.length - 1];
assert.ok(freshXhr.url.endsWith('?autoGain=0'));
freshXhr.finish(200, {{masterLimiter: true, autoGain: false}}, 'drec');
assert.equal(scUiPending, 0);
"""
        run_node(script)

    @unittest.skipUnless(shutil.which("node"), "Node is required")
    def test_rapid_stream_source_toggles_keep_both_disabled(self):
        main = self.bp.SERVICES_MAIN
        code = "\n".join(filter(None, [
            extract_function(main, "scSameItems"),
            extract_function(main, "scFxMsInteger"),
            extract_function(main, "scFxMsApply"),
            extract_function(main, "scApplyQuery", optional=True),
            extract_function(main, "scUiSyncPending", optional=True),
            extract_function(main, "scUiFlush", optional=True),
            extract_function(main, "scCall"),
            extract_function(main, "scSetSource"),
        ]))
        script = f"""
const assert = require('node:assert/strict');
const reqs = [];
class XMLHttpRequest {{
    constructor() {{ this.readyState = 0; this.status = 0; this.responseText = ''; this.headers = {{}}; }}
    open(m, u) {{ this.method = m; this.url = u; this.readyState = 1; }}
    send() {{ reqs.push(this); }}
    abort() {{ this.readyState = 0; }}
    getResponseHeader(k) {{ return this.headers[k] || ''; }}
    finish(status, body, digest = '') {{
        this.status = status;
        this.responseText = JSON.stringify(body);
        this.headers = digest ? {{'X-UI-Digest': digest}} : {{}};
        this.readyState = 4;
        if (this.onreadystatechange) this.onreadystatechange();
    }}
}}
let scScope = '', scScopePending = 0, scUiPoll = null, scUiPollScope = '', scUiPollAt = 0, scUiDigest = '', scUiAppliedAt = 0;
let scUiWriteGen = 0, scUiPollGen = 0, scUiPending = 0, scUiQueue = [], scUiSending = false, scUiSendingAt = 0, scUiSendToken = 0, scUiSendXhr = null, scUiHadError = false;
let scTunnelPending = 0, scUiLoaded = true, scIsPlayerBoard = true, scConsoleLineBoard = false, scFourDeckOn = false, scHasMixer = false, scTransitionFxView = null;
let scSources = ['tidal', 'netease', 'spotify', 'deezer', 'soundcloud', 'beatport', 'applemusic', 'rekordbox'];
function scApply(v) {{ if (v.sources) scSources = v.sources; }}
{code}
// User disables SoundCloud, then immediately disables Deezer before POST 1 replies, while discovered rekordbox is present.
scSetSource('soundcloud', false);
scSetSource('deezer', false);
assert.equal(scSources.includes('soundcloud'), false, 'soundcloud disabled optimistically');
assert.equal(scSources.includes('deezer'), false, 'deezer disabled optimistically');
assert.equal(scSources.includes('rekordbox'), true, 'discovered rekordbox must stay visible optimistically');
assert.equal(reqs.length, 1, 'second source delta POST waits behind first');
assert.equal(reqs[0].url, 'http://127.0.0.1:8080/v1/enginex/ui?source_soundcloud=0');
reqs[0].finish(200, {{sources: ['tidal', 'netease', 'spotify', 'deezer', 'beatport', 'applemusic', 'rekordbox'], rev: 2}}, 'ds1');
assert.equal(reqs.length, 2);
assert.equal(reqs[1].url, 'http://127.0.0.1:8080/v1/enginex/ui?source_deezer=0', 'second POST must send per-provider delta without re-enabling soundcloud or serializing rekordbox');
reqs[1].finish(200, {{sources: ['tidal', 'netease', 'spotify', 'beatport', 'applemusic', 'rekordbox'], rev: 3}}, 'ds2');
assert.deepEqual(scSources, ['tidal', 'netease', 'spotify', 'beatport', 'applemusic', 'rekordbox']);
"""
        run_node(script)

    @unittest.skipUnless(shutil.which("node"), "Node is required")
    def test_remote_access_tunnel_toggle_optimistic_serialized_and_poll_guarded(self):
        main = self.bp.SERVICES_MAIN
        code = "\n".join(filter(None, [
            extract_function(main, "scTunnelApply"),
            extract_function(main, "scTunnelSend", optional=True),
            extract_function(main, "scTunnelToggle"),
            extract_function(main, "scUiSyncPending", optional=True),
            extract_function(main, "scUiFlush", optional=True),
            extract_function(main, "scCall"),
            extract_function(main, "scUiPollReply"),
        ]))
        script = f"""
const assert = require('node:assert/strict');
const reqs = [];
class XMLHttpRequest {{
    constructor() {{ this.readyState = 0; this.status = 0; this.responseText = ''; this.headers = {{}}; this.aborted = false; }}
    open(m, u) {{ this.method = m; this.url = u; this.readyState = 1; }}
    setRequestHeader() {{}}
    send(body) {{ this.body = body; reqs.push(this); }}
    abort() {{ this.aborted = true; this.readyState = 0; }}
    getResponseHeader(k) {{ return this.headers[k] || ''; }}
    finish(status, body, digest = '') {{
        this.status = status;
        this.responseText = JSON.stringify(body);
        this.headers = digest ? {{'X-UI-Digest': digest}} : {{}};
        this.readyState = 4;
        if (this.onreadystatechange) this.onreadystatechange();
    }}
}}
let scScope = '', scScopePending = 0, scUiPoll = null, scUiPollScope = '', scUiPollAt = 0, scUiDigest = 'd0', scUiAppliedAt = Date.now();
let scUiWriteGen = 0, scUiPollGen = 0, scUiPending = 0, scUiQueue = [], scUiSending = false, scUiSendingAt = 0, scUiSendToken = 0, scUiSendXhr = null, scUiHadError = false;
let scRemoteAccess = false, scRemoteAccessStatus = 'stopped', scRemoteAccessUrl = '', scRemoteAccessError = '';
let scTunnelPending = 0, scTunnelSending = false, scTunnelQueued = null, scTunnelSendingAt = 0, scTunnelSendToken = 0, scTunnelSendXhr = null;
let scUiLoaded = true, scLoggedIn = [], scSources = [];
const root = {{
    get scRemoteAccess() {{ return scRemoteAccess; }}, set scRemoteAccess(v) {{ scRemoteAccess = v; }},
    get scRemoteAccessStatus() {{ return scRemoteAccessStatus; }}, set scRemoteAccessStatus(v) {{ scRemoteAccessStatus = v; }},
    get scRemoteAccessUrl() {{ return scRemoteAccessUrl; }}, set scRemoteAccessUrl(v) {{ scRemoteAccessUrl = v; }},
    get scRemoteAccessError() {{ return scRemoteAccessError; }}, set scRemoteAccessError(v) {{ scRemoteAccessError = v; }},
    scTunnelApply(v) {{ scTunnelApply(v); }},
    scCall(q, d) {{ scCall(q, d); }},
}};
function scEnsureTidal() {{}}
function scApply(v) {{ if (v.tunnel) scTunnelApply(v.tunnel); }}
{code}
// In-flight UI poll started while tunnel is off.
scCall('');
// User turns Remote access ON, then quickly toggles OFF while POST 1 is in flight.
scTunnelToggle(true);
assert.equal(scRemoteAccess, true, 'remote access flips on optimistically');
assert.equal(scUiPending, 1);
// Stale UI poll arrives saying tunnel.enabled=false -> must be ignored.
scUiPoll.finish(200, {{tunnel: {{enabled: false, status: 'stopped'}}}}, 'd0');
assert.equal(scRemoteAccess, true, 'stale UI poll must not revert in-flight tunnel toggle');
scTunnelToggle(false);
assert.equal(scRemoteAccess, false);
assert.equal(reqs.length, 2, 'second tunnel POST waits behind first');
assert.equal(reqs[1].body, JSON.stringify({{enabled: true}}));
reqs[1].finish(200, {{enabled: true, status: 'starting'}});
assert.equal(scRemoteAccess, false, 'first tunnel reply must not clobber queued toggle');
assert.equal(reqs.length, 3);
assert.equal(reqs[2].body, JSON.stringify({{enabled: false}}));
reqs[2].finish(200, {{enabled: false, status: 'stopped'}});
assert.equal(scUiPending, 0);
assert.equal(scRemoteAccess, false);
// Complete the post-tunnel resync poll triggered by scTunnelSend.
const postTunnelPoll = reqs.at(-1);
assert.equal(postTunnelPoll.method, 'GET');
postTunnelPoll.finish(200, {{tunnel: {{enabled: false, status: 'stopped'}}}}, 'dt1');
// Hung tunnel POST (>15s) recovers via 3s scCall('') watchdog without stranding pending count.
const t0 = Date.now();
Date.now = () => t0;
scTunnelToggle(true);
const hungTunnel = reqs.at(-1);
assert.equal(scUiPending, 1);
Date.now = () => t0 + 16000;
scCall('');
assert.equal(hungTunnel.aborted, true, 'hung tunnel POST aborted by 3s watchdog');
assert.equal(scTunnelPending, 0, 'hung tunnel clears pending count');
assert.equal(scUiPending, 0);
const reconPoll = reqs.at(-1);
assert.equal(reconPoll.method, 'GET');
reconPoll.finish(200, {{tunnel: {{enabled: false, status: 'stopped'}}}}, 'dt2');
assert.equal(scRemoteAccess, false, 'reconciled poll restores server tunnel state');
hungTunnel.finish(200, {{enabled: true, status: 'running'}});
assert.equal(scRemoteAccess, false, 'delayed reply from timed-out tunnel POST must be ignored');
"""
        run_node(script)

    @unittest.skipUnless(shutil.which("node"), "Node is required")
    def test_bluetooth_output_source_ignores_stale_load_and_serializes_sets(self):
        main = self.bp.SERVICES_MAIN
        code = "\n".join(filter(None, [
            extract_function(main, "scBtLabel"),
            extract_function(main, "scBtLabels"),
            extract_function(main, "scBtReply"),
            extract_function(main, "scBtLoad"),
            extract_function(main, "scBtSend", optional=True),
            extract_function(main, "scBtSet"),
        ]))
        script = f"""
const assert = require('node:assert/strict');
const reqs = [];
class XMLHttpRequest {{
    constructor() {{ this.readyState = 0; this.status = 0; this.responseText = ''; }}
    open(m, u) {{ this.method = m; this.url = u; this.readyState = 1; }}
    send() {{ reqs.push(this); }}
    finish(status, body) {{
        this.status = status;
        this.responseText = JSON.stringify(body);
        this.readyState = 4;
        if (this.onreadystatechange) this.onreadystatechange();
    }}
}}
let scBtLayers = ['Master', 'Booth', 'Headphones'], scBtLayer = 'Master';
const root = {{ scBtGen: 0, scBtSending: false, scBtQueued: '', scBtSendingAt: 0, scBtSendToken: 0, scBtReload: false }};
{code}
// Dialog opens -> GET in flight. User quickly taps Booth then Headphones.
scBtLoad();
scBtSet('Booth');
scBtSet('Headphones');
assert.equal(scBtLayer, 'Headphones', 'optimistic update shows latest choice');
assert.equal(reqs.length, 2, 'only GET + first POST in flight');
// Stale GET returns Master -> must be ignored.
reqs[0].finish(200, {{layers: ['Master', 'Booth', 'Headphones'], assignLayer: 'Master'}});
assert.equal(scBtLayer, 'Headphones', 'stale scBtLoad must not revert scBtSet');
// First POST (Booth) returns -> must not revert to Booth, and must flush queued Headphones.
reqs[1].finish(200, {{layers: ['Master', 'Booth', 'Headphones'], assignLayer: 'Booth'}});
assert.equal(scBtLayer, 'Headphones');
assert.equal(reqs.length, 3);
assert.ok(reqs[2].url.endsWith('assignLayer=Headphones'));
reqs[2].finish(200, {{layers: ['Master', 'Booth', 'Headphones'], assignLayer: 'Headphones'}});
assert.equal(scBtLayer, 'Headphones');
// Reopening Bluetooth dialog during an in-flight assignment defers GET until write completes.
scBtSet('Booth');
const write = reqs.at(-1), count = reqs.length;
scBtLoad();
const read = reqs.length > count ? reqs.at(-1) : null;
assert.equal(read, null, 'scBtLoad during scBtSet must defer instead of racing');
write.finish(200, {{layers: ['Master', 'Booth'], assignLayer: 'Booth'}});
assert.equal(scBtLayer, 'Booth', 'reopening during write must keep acknowledged assignment');
assert.equal(reqs.length, count + 1, 'deferred scBtLoad must run once the in-flight assignment finishes');
assert.equal(reqs.at(-1).method, 'GET');
reqs.at(-1).finish(200, {{layers: ['Master', 'Booth', 'Zone'], assignLayer: 'Booth'}});
assert.deepEqual(scBtLayers, ['Master', 'Booth', 'Zone']);
assert.equal(scBtLayer, 'Booth');
"""
        run_node(script)

    def _round2_js_harness(self):
        main = self.bp.SERVICES_MAIN
        names = (
            "scUiReplyRev scSourceWritePending scSyncLoginDialog scSetSource "
            "scCall scUiPollReply scUiSyncPending scUiFlush scApplyQuery "
            "scSameItems scFxMsInteger scFxMsApply scLoginDone scBtLoad "
            "scBtSend scBtSet scBtReply scTunnelApply scTunnelSend scTunnelToggle"
        )
        main_js = "\n".join(filter(None, (extract_function(main, n, optional=True) for n in names.split())))
        fx = self.bp.FX_CONTROLLER.replace("    function ", "\tfunction ")
        knob_js = "\n".join(extract_function(fx, n) for n in ("scFxMsSet", "scFxMsFlush", "scFxMsExpire"))
        return """
const assert = require('node:assert/strict'), reqs = [];
let now = 100000;
Date.now = () => now;
globalThis.root = globalThis;
Object.assign(root, {
    scClientId: 'qml-r2', scUiWriteSeq: 0, scBtWriteSeq: 0, scTunnelWriteSeq: 0,
    scUiAppliedSeq: 0, scUiRev: 0, scUiEpoch: '', scRetiredEpochs: {}, scUiLowPollCount: 0, scUiLowPollLast: 0,
    scLoginReqGen: 0, scPendingTap: '', scScopeNeedRefresh: false,
    scUiWriteGen: 0, scUiPollGen: 0, scUiPending: 0, scUiQueue: [], scUiSending: false,
    scUiSendingAt: 0, scUiSendToken: 0, scUiSendXhr: null, scUiSendingQuery: '', scUiHadError: false, scRestartPending: false,
    scTunnelPending: 0, scTunnelSending: false, scTunnelQueued: null, scTunnelSendingAt: 0,
    scTunnelSendToken: 0, scTunnelSendXhr: null,
    scRemoteAccess: false, scRemoteAccessStatus: 'stopped', scRemoteAccessUrl: '', scRemoteAccessError: '',
    scScope: '', scScopePending: 0, scUiPoll: null, scUiPollScope: '', scUiPollAt: 0,
    scUiDigest: '', scUiAppliedAt: 0, scUiLoaded: true, scLoggedIn: [], scSources: ['spotify'],
    scLoginOpen: '', scLogin: '', scLoginStatus: '', scLoginLocal: false, scLoginSerial: 0,
    scEnterTries: 0, scEnterTimer: {start() {}},
    Planck: {librarianController: {streamingPlaylistsController: {invalidateUI() {}}}},
    scBtSending: false, scBtSendingAt: 0, scBtQueued: '', scBtGen: 0, scBtReadSeq: 0, scBtAppliedReadSeq: 0, scBtSendToken: 0,
    scBtSendXhr: null, scBtReload: false, scBtLayers: ['Master', 'Booth', 'Headphones'], scBtLayer: 'Master',
    scSdHighSpeed: false, scMasterLimiter: false, scAutoGain: false, scLedBrightness: 100, scLedBrightnessSaved: 100,
    scFxTimeMode: 'ms', scFxTimeMs: 500, scFxMsSending: false, scFxMsRequest: 0,
    scFxMsWanted: -1, scFxMsSent: -1,
    scWebdav: [], scNasServers: [],
});
class XMLHttpRequest {
    constructor() { this.readyState = 0; this.reqHeaders = {}; this.aborted = false; this.digest = ''; this.revHeader = ''; this.epochHeader = ''; }
    open(method, url) { Object.assign(this, {method, url, readyState: 1}); }
    setRequestHeader(k, v) { this.reqHeaders[k] = v; }
    send(body) { this.body = body; reqs.push(this); }
    abort() { this.aborted = true; this.readyState = 0; }
    getResponseHeader(k) {
        if (k === 'X-UI-Rev') return this.revHeader || '';
        if (k === 'X-UI-Epoch') return this.epochHeader || '';
        return this.digest || '';
    }
    finish(data, digest = '', status = 200, rev = 0, epoch = '') {
        const effectiveRev = rev > 0 ? rev : (data && typeof data === 'object' && typeof data.rev === 'number' && Number.isInteger(data.rev) && data.rev > 0 ? data.rev : 0);
        const effectiveEpoch = epoch || (data && typeof data === 'object' && typeof data.epoch === 'string' ? data.epoch : '');
        Object.assign(this, {
            readyState: 4,
            status,
            responseText: typeof data === 'string' ? data : JSON.stringify(data),
            digest,
            revHeader: effectiveRev > 0 ? String(effectiveRev) : '',
            epochHeader: effectiveEpoch,
        });
        if (this.onreadystatechange) this.onreadystatechange();
    }
}
function scEnsureTidal() {}
function scOpen(id) { scScope = id; }
function scApply(v) {
    if (v.scope !== undefined) scScope = v.scope;
    if (Array.isArray(v.loggedIn)) scLoggedIn = v.loggedIn;
    if (Array.isArray(v.sources)) scSources = v.sources;
    if (v.fxTimeMs !== undefined) scFxMsApply('ms', v.fxTimeMs);
    if (v.tunnel) scTunnelApply(v.tunnel);
}
""" + main_js + "\n" + knob_js + "\n"

    @unittest.skipUnless(shutil.which("node"), "Node is required")
    def test_qr_callback_keeps_newer_login_list(self):
        run_node(self._round2_js_harness() + """
// Case 1: newer settings ACK (rev 3) reports Spotify revoked before older pre-revocation GET (rev 2) returns.
scLoginOpen = 'spotify';
scLogin = 'spotify';
scCall('', () => {});
const older = reqs.at(-1);
scLoginDone();
assert.equal(scLogin, '');
assert.equal(scLoginOpen, '');
scCall('masterLimiter=1');
const revoked = {sources: ['spotify'], loggedIn: [], rev: 3};
reqs.at(-1).finish(revoked, 'revoked');
older.finish({sources: ['spotify'], loggedIn: ['spotify'], rev: 2}, 'before-revocation');
scCall('');
reqs.at(-1).finish(revoked, 'revoked');
assert.deepEqual(scLoggedIn, [], 'older rev 2 GET restored revoked Spotify despite rev 3');
assert.equal(scUiRev, 3);
assert.notEqual(scScope, 'spotify', 'passive scLoginDone or older GET must not open a revoked provider');

// Case 2: newer periodic poll (rev 5) reports Spotify revoked before older GET (rev 4) returns.
scLoginOpen = 'spotify';
scLogin = 'spotify';
scCall('', () => {});
const older2 = reqs.at(-1);
scLoginDone();
scCall('');
reqs.at(-1).finish({sources: ['spotify'], loggedIn: [], rev: 5}, 'revoked-poll');
older2.finish({sources: ['spotify'], loggedIn: ['spotify'], rev: 4}, 'before-poll-revocation');
assert.deepEqual(scLoggedIn, [], 'older rev 4 GET must not override newer rev 5 poll login list');
assert.equal(scUiRev, 5);
assert.notEqual(scScope, 'spotify');
""")

    @unittest.skipUnless(shutil.which("node"), "Node is required")
    def test_older_poll_before_newer_qr_result(self):
        run_node(self._round2_js_harness() + """
scLoginOpen = 'spotify';
scLogin = 'spotify';
scCall('');
const oldPoll = reqs.at(-1);
scLoginDone();
assert.equal(scLogin, '');
assert.equal(scLoginOpen, '');
const loginRead = reqs.at(-1);
oldPoll.finish({sources: ['spotify'], loggedIn: [], rev: 1}, 'pre-login');
loginRead.finish({sources: ['spotify'], loggedIn: ['spotify'], rev: 2}, 'signed-in');
assert.deepEqual(scLoggedIn, ['spotify']);
assert.equal(scUiRev, 2);
assert.equal(scScope, '', 'passive scan refreshes loggedIn without auto-opening scope');
scOpen('spotify');
assert.equal(scScope, 'spotify', 'explicit tap after passive scan opens on first click');
""")

    @unittest.skipUnless(shutil.which("node"), "Node is required")
    def test_newer_source_choice_wins_over_qr_callback(self):
        run_node(self._round2_js_harness() + """
scSources = ['spotify', 'deezer'];
scLoggedIn = ['deezer'];
scLoginOpen = 'spotify';
scLogin = 'spotify';
scLoginDone();
const oldLogin = reqs.at(-1);
scOpen('deezer');
scCall('scope=deezer', () => {});
reqs.at(-1).finish({scope: 'deezer', sources: ['spotify', 'deezer'], loggedIn: ['spotify', 'deezer'], rev: 3}, 'new-choice');
oldLogin.finish({sources: ['spotify', 'deezer'], loggedIn: ['spotify', 'deezer'], rev: 2}, 'old-login');
assert.equal(scScope, 'deezer', 'QR completion overwrote a newer selected source');
""")

    @unittest.skipUnless(shutil.which("node"), "Node is required")
    def test_stripped_ack_does_not_adopt_its_digest(self):
        run_node(self._round2_js_harness() + """
// 1. scUiFlush must not cache digest when reply.scope was stripped.
scScope = 'deezer';
scCall('masterLimiter=1');
const older = reqs.at(-1);
scOpen('spotify');
scCall('scope=spotify', () => {});
reqs.at(-1).finish({scope: 'spotify', masterLimiter: true, rev: 1}, 'new-scope');
older.finish({scope: 'deezer', masterLimiter: true, rev: 2}, 'stripped-scope');
assert.equal(scScope, 'spotify');
assert.notEqual(scUiDigest, 'stripped-scope', 'scUiFlush cached the digest of a partially applied snapshot');

// 2. scCall callback GET must not cache digest when reply.scope was stripped.
scScope = 'deezer';
scUiDigest = 'before-get';
scCall('', () => {});
const cbGet = reqs.at(-1);
scScope = 'spotify';
cbGet.finish({scope: 'deezer', masterLimiter: true, rev: 3}, 'stripped-get-digest');
assert.equal(scScope, 'spotify');
assert.notEqual(scUiDigest, 'stripped-get-digest', 'scCall cached the digest of a scope-stripped GET snapshot');
""")

    @unittest.skipUnless(shutil.which("node"), "Node is required")
    def test_rejected_scope_recovers_after_overlapping_setting(self):
        run_node(self._round2_js_harness() + """
scScope = 'deezer';
scCall('masterLimiter=1');
const setting = reqs.at(-1);
scScope = 'spotify';
scCall('scope=spotify', () => { throw Error('a rejected choice cannot succeed'); });
reqs.at(-1).finish({error: 'source is not logged in', rev: 1}, '', 409);
const state = {scope: 'deezer', masterLimiter: true, rev: 2};
reqs.at(-1).finish(state, 'server');
setting.finish(state, 'server');
for (let i = 0; i < 2; i++) {
    now += 3000;
    scCall('');
    reqs.at(-1).finish(state, 'server');
}
assert.equal(scScope, 'deezer', '409 recovery cached a digest without restoring server scope');
""")

    @unittest.skipUnless(shutil.which("node"), "Node is required")
    def test_bluetooth_queue_recovers_without_another_tap(self):
        run_node(self._round2_js_harness() + """
scBtSet('Booth');
const hungBooth = reqs.at(-1);
scBtSet('Headphones');
scBtLoad();
let hp = null;
for (let tick = 1; tick <= 8; tick++) {
    now += 3000;
    scCall('');
    const poll = reqs.at(-1);
    if (poll.method === 'GET' && poll.readyState !== 4) poll.finish({rev: 1}, 'same');
    if (!hp) {
        hp = reqs.find(x => x.method === 'POST' && x.url.endsWith('assignLayer=Headphones'));
        if (hp) {
            assert.equal(hungBooth.aborted, true, 'hung Booth assignment must be aborted by 3s watchdog');
            const beforeCount = reqs.length;
            hp.finish({layers: ['Master', 'Booth', 'Headphones'], assignLayer: 'Headphones'});
            assert.equal(scBtLayer, 'Headphones');
            // Deferred scBtLoad runs after the recovered write finishes.
            assert.equal(reqs.length, beforeCount + 1);
            assert.equal(reqs.at(-1).method, 'GET');
            assert.ok(reqs.at(-1).url.endsWith('/v1/enginex/bluetooth'));
            reqs.at(-1).finish({layers: ['Master', 'Booth', 'Headphones'], assignLayer: 'Headphones'});
        }
    }
}
assert.ok(hp, 'latest Bluetooth choice was never sent after 24 seconds');
// Late reply from hungBooth must be ignored.
hungBooth.finish({layers: ['Master', 'Booth', 'Headphones'], assignLayer: 'Booth'});
assert.equal(scBtLayer, 'Headphones');
""")

    @unittest.skipUnless(shutil.which("node"), "Node is required")
    def test_knob_keeps_last_step_while_another_setting_is_slow(self):
        run_node(self._round2_js_harness() + """
let deadline = 0, persisted = 500, rev = 1;
const scFxMsTimeout = { restart() { deadline = now + 2000; }, stop() { deadline = 0; } };
function completeKnobWrites() {
    let x;
    while ((x = reqs.find(q => q.method === 'POST' && q.readyState !== 4 && q.url.includes('?fxTimeMs=')))) {
        persisted = Number(x.url.split('=').at(-1));
        x.finish({fxTimeMs: persisted, rev: ++rev}, String(persisted));
    }
}
scCall('sdHighSpeed=1');
const slow = reqs[0];
scFxMsSet(510);
scFxMsSet(520);
completeKnobWrites();
now += 2100;
if (deadline && now >= deadline) scFxMsExpire();
slow.finish({fxTimeMs: persisted, rev: ++rev}, 'sd-ready');
completeKnobWrites();
assert.equal(persisted, 520, 'the last knob step was discarded while waiting for the unrelated write');
""")

    @unittest.skipUnless(shutil.which("node"), "Node is required")
    def test_write_seq_headers_monotonic_across_endpoints(self):
        run_node(self._round2_js_harness() + """
scCall('ledBrightness=80');
const ui1 = reqs.at(-1);
assert.equal(ui1.reqHeaders['X-UI-Client-Id'], 'qml-r2');
assert.equal(ui1.reqHeaders['X-UI-Write-Seq'], '1');
ui1.finish({ledBrightness: 80, rev: 1}, 'u1');
scCall('ledBrightness=60');
const ui2 = reqs.at(-1);
assert.equal(ui2.reqHeaders['X-UI-Client-Id'], 'qml-r2');
assert.equal(ui2.reqHeaders['X-UI-Write-Seq'], '2');
ui2.finish({ledBrightness: 60, rev: 2}, 'u2');

scBtSet('Booth');
const bt1 = reqs.at(-1);
assert.equal(bt1.reqHeaders['X-UI-Client-Id'], 'qml-r2');
assert.equal(bt1.reqHeaders['X-UI-Write-Seq'], '1');
bt1.finish({layers: ['Master', 'Booth'], assignLayer: 'Booth'});
scBtSet('Headphones');
const bt2 = reqs.at(-1);
assert.equal(bt2.reqHeaders['X-UI-Write-Seq'], '2');
bt2.finish({layers: ['Master', 'Booth', 'Headphones'], assignLayer: 'Headphones'});

scTunnelToggle(true);
const tun1 = reqs.at(-1);
assert.equal(tun1.reqHeaders['X-UI-Client-Id'], 'qml-r2');
assert.equal(tun1.reqHeaders['X-UI-Write-Seq'], '1');
tun1.finish({enabled: true, status: 'running'});
scTunnelToggle(false);
const tun2 = reqs.at(-1);
assert.equal(tun2.reqHeaders['X-UI-Write-Seq'], '2');
""")

    def test_sandbox_dbus_cleanup_trap_enforces_stop_and_fails_on_lost_pid(self):
        if not Path("/proc/self/stat").exists():
            self.skipTest("Linux /proc required to verify sandbox D-Bus cleanup traps")
        launchers = [
            (REPO / "tools/az01-packer/test_settings_click_sandbox.sh", "cleanup"),
            (REPO / "tools/az01-packer/test_fx_selector_sandbox.sh", "fx_cleanup"),
        ]
        for path, fn_name in launchers:
            src = path.read_text(encoding="utf-8")
            m = re.search(rf"(?ms)^{fn_name}\(\) \{{.*?^\}}", src)
            self.assertIsNotNone(m, f"{path.name} missing {fn_name}()")
            trap_fn = m.group(0)
            self.assertIn("--print-pid=1", src)
            self.assertIn("dbus_started=1", src)
            self.assertIn('kill -0 "$dbus_pid"', src)
            for mode, want_rc in (("normal", 0), ("fail-exit", 7), ("lose-pid", 1)):
                with self.subTest(launcher=path.name, mode=mode):
                    with tempfile.TemporaryDirectory(prefix="e4-dbus-trap-") as tmp:
                        pidfile = Path(tmp) / "dummy.pid"
                        script = f"""
set -euo pipefail
R=""
board=JP14
PRODUCT=JP07
server=""
http_pid=""
dbus_started=1
dbus_pid=""
{trap_fn}
trap {fn_name} EXIT
sleep 30 >/dev/null 2>&1 &
dbus_pid=$!
printf '%s\\n' "$dbus_pid" > "{pidfile}"
for _w in $(seq 1 50); do
    case "$(readlink "/proc/$dbus_pid/exe" 2>/dev/null || true)" in
        */sleep) break ;;
    esac
    sleep 0.01
done
if [ "{mode}" = "lose-pid" ]; then
    dbus_pid=""
fi
if [ "{mode}" = "fail-exit" ]; then
    exit 7
fi
exit 0
"""
                        res = subprocess.run(["bash", "-c", script], capture_output=True, text=True, timeout=10)
                        dummy_pid = int(pidfile.read_text(encoding="utf-8").strip())
                        try:
                            self.assertEqual(res.returncode, want_rc, f"stderr={res.stderr}")
                            if mode in ("normal", "fail-exit"):
                                alive = subprocess.run(["kill", "-0", str(dummy_pid)], capture_output=True).returncode == 0
                                self.assertFalse(alive, f"{path.name} {fn_name}() left dummy daemon {dummy_pid} alive")
                            else:
                                self.assertIn("dbus_cleanup_pid_lost", res.stderr)
                        finally:
                            subprocess.run(["kill", "-9", str(dummy_pid)], capture_output=True)

    @unittest.skipUnless(shutil.which("node"), "Node is required")
    def test_missing_logged_in_does_not_cache_digest(self):
        run_node(self._round2_js_harness() + """
scUiDigest = '';
// 1. Callback GET without loggedIn array must not cache digest.
scCall('', () => {});
reqs.at(-1).finish({masterLimiter: true, rev: 1}, 'no-login-get');
assert.equal(scUiDigest, '', 'scCall cached digest without Array.isArray(reply.loggedIn)');

// 2. Setting POST reply without loggedIn array must not cache digest.
scCall('masterLimiter=1');
reqs.at(-1).finish({masterLimiter: true, rev: 2}, 'no-login-post');
assert.equal(scUiDigest, '', 'scUiFlush cached digest without Array.isArray(reply.loggedIn)');

// 3. Periodic poll missing loggedIn array must not adopt digest (and clears prior digest).
scUiDigest = 'prior';
scCall('');
scUiPoll.finish({scope: '', sources: ['spotify'], rev: 3}, 'missing-logins');
assert.equal(scUiDigest, '', 'scUiPollReply cached digest without Array.isArray(reply.loggedIn)');

// 4. Periodic poll scLoginOpen path missing loggedIn must decline digest caching and keep scLoginOpen intact.
scUiDigest = 'prior-open';
scLoggedIn = ['spotify'];
scSources = ['spotify'];
scLoginOpen = 'spotify';
scLogin = 'spotify';
scCall('');
scUiPoll.finish({scope: '', sources: ['spotify'], rev: 4}, 'missing-logins-open');
assert.equal(scUiDigest, '', 'scUiPollReply early-return cached digest without Array.isArray(reply.loggedIn)');
assert.equal(scLoginOpen, 'spotify', 'incomplete poll must not consume scLoginOpen');
assert.equal(scLogin, 'spotify', 'incomplete poll must not close active login dialog');
scLoginOpen = '';
scLogin = '';
scScope = '';
scLoggedIn = [];

// 5. Once Array.isArray(reply.loggedIn) is present, POST and poll cache the digest cleanly.
scCall('autoGain=1');
reqs.at(-1).finish({masterLimiter: true, autoGain: true, loggedIn: [], rev: 5}, 'with-login-post');
assert.equal(scUiDigest, 'with-login-post');
now += 3000;
scCall('');
scUiPoll.finish({masterLimiter: true, autoGain: true, loggedIn: ['spotify'], rev: 6}, 'with-login-poll');
assert.equal(scUiDigest, 'with-login-poll');
assert.deepEqual(scLoggedIn, ['spotify']);
""")

    @unittest.skipUnless(shutil.which("node"), "Node is required")
    def test_invalid_200_body_releases_next_write_and_recovers(self):
        run_node(self._round2_js_harness() + """
// 1. Truncated/empty 200 body on scUiFlush releases queued write immediately without wedging.
scCall('masterLimiter=1');
const first = reqs.at(-1);
scCall('autoGain=1');
Object.assign(first, {readyState: 4, status: 200, responseText: ''});
first.onreadystatechange();
const next = reqs.find(x => x.url.endsWith('autoGain=1'));
assert.ok(next, 'truncated 200 body wedged next queued write');
next.finish({masterLimiter: false, autoGain: true, loggedIn: [], rev: 1}, 'after-next');
assert.equal(scUiPending, 0);

// 2. Non-object JSON (array/null) on empty-queue scUiFlush triggers reconciliation GET without throwing.
scCall('masterLimiter=1');
const arrReply = reqs.at(-1);
Object.assign(arrReply, {readyState: 4, status: 200, responseText: '[]'});
arrReply.onreadystatechange();
assert.equal(scUiPending, 0);
assert.equal(scUiDigest, '');
assert.equal(reqs.at(-1).method, 'GET', 'invalid final 200 body must trigger reconciliation GET');
reqs.at(-1).finish({masterLimiter: false, autoGain: true, loggedIn: [], rev: 2}, 'reconciled');

// 3. Invalid 200 body on scCall scope change triggers recovery without firing done.
scScope = 'spotify';
let doneCalled = 0;
scCall('scope=spotify', () => { doneCalled++; });
const badScope = reqs.at(-1);
Object.assign(badScope, {readyState: 4, status: 200, responseText: '{truncated'});
badScope.onreadystatechange();
assert.equal(doneCalled, 0, 'invalid 200 body must not invoke done');
assert.equal(scScopePending, 0);
assert.equal(reqs.at(-1).method, 'GET', 'failed scope reply must schedule resync GET');
reqs.at(-1).finish({scope: 'tidal', sources: ['spotify'], loggedIn: [], rev: 3}, 'scope-resync');
assert.equal(scScope, 'tidal');

// 4. Invalid 200 body on scUiPollReply must not advance scUiRev or digest.
scUiRev = 4;
scCall('');
Object.assign(scUiPoll, {readyState: 4, status: 200, responseText: 'null', digest: 'bad-poll', revHeader: '99'});
scUiPoll.onreadystatechange();
assert.equal(root.scUiRev, 4, 'invalid poll body must not advance scUiRev');
assert.notEqual(scUiDigest, 'bad-poll');
""")

    @unittest.skipUnless(shutil.which("node"), "Node is required")
    def test_restart_call_defers_quit_until_all_writes_drain(self):
        restart = extract_function(self.bp.SERVICES_MAIN, "scRestartCall")
        run_node(self._round2_js_harness() + restart + """
root.scT = msg => msg;
root.Prompt = {Warning: 1, Cancel: 2, Ok: 4, AcceptRole: 8};
root.QuitReasons = {AnalyticsChange: 1};
root.PlanckGUI = {views: {prompts: {post: () => ({finished: {connect: cb => cb(Prompt.AcceptRole)}})}}};
let quits = [];
Planck.quit = (reason, force) => { quits.push({reason, force, reqCount: reqs.length, pending: scUiPending, btSending: root.scBtSending}); };

// 1. Role change followed by two queued UI taps waits for all queued writes before quitting.
scRestartCall('fourDeckRole=1');
const layout = reqs.at(-1);
scCall('autoGain=1');
scCall('cueCountdown=0');
layout.finish({loggedIn: [], sources: [], rev: 1}, 'layout');
assert.equal(quits.length, 0, 'must not quit while autoGain/cueCountdown are queued');
assert.equal(root.scRestartPending, true);
const auto = reqs.find(x => x.url.endsWith('autoGain=1'));
assert.ok(auto);
auto.finish({autoGain: true, loggedIn: [], rev: 2}, 'auto');
assert.equal(quits.length, 0, 'must not quit while cueCountdown is still queued');
const cue = reqs.find(x => x.url.endsWith('cueCountdown=0'));
assert.ok(cue);
cue.finish({autoGain: true, cueCountdown: false, loggedIn: [], rev: 3}, 'cue');
assert.equal(quits.length, 1, 'must quit once final queued write completes');
assert.equal(root.scRestartPending, false);
assert.equal(quits[0].pending, 0);

// 2. Role change with in-flight tunnel and Bluetooth writes waits until all three subsystems drain.
quits = [];
scTunnelToggle(true);
const tun = reqs.at(-1);
scBtSet('Booth');
const bt = reqs.at(-1);
scRestartCall('fourDeckRole=24');
const layout2 = reqs.at(-1);
layout2.finish({loggedIn: [], sources: [], rev: 4}, 'layout2');
assert.equal(quits.length, 0, 'must wait for tunnel and Bluetooth writes');
assert.equal(root.scRestartPending, true);
const reqsBeforeDrain = reqs.length;
tun.finish({enabled: true, status: 'running'});
assert.equal(quits.length, 0, 'must still wait for Bluetooth write after tunnel drains');
bt.finish({layers: ['Master', 'Booth'], assignLayer: 'Booth'});
assert.equal(quits.length, 1, 'must quit once tunnel and Bluetooth both drain');
assert.equal(root.scRestartPending, false);
assert.equal(reqs.length, reqsBeforeDrain, 'draining into restart must not fire redundant GET reloads');

// 3. Role POST failure (HTTP 500 or invalid 200 body) must NOT set scRestartPending or call Planck.quit,
//    while still flushing later queued writes.
quits = [];
scRestartCall('fourDeckRole=1');
const fail500 = reqs.at(-1);
scCall('masterLimiter=1');
fail500.finish({error: 'boom', rev: 4}, '', 500);
assert.equal(root.scRestartPending, false);
assert.equal(quits.length, 0);
const afterFail = reqs.find(x => x.url.endsWith('masterLimiter=1'));
assert.ok(afterFail);
afterFail.finish({masterLimiter: true, loggedIn: [], rev: 5}, 'after-fail');
assert.equal(quits.length, 0, 'failed role POST must never trigger quit');

scRestartCall('fourDeckRole=1');
const badBody = reqs.at(-1);
Object.assign(badBody, {readyState: 4, status: 200, responseText: ''});
badBody.onreadystatechange();
assert.equal(root.scRestartPending, false);
assert.equal(quits.length, 0, 'invalid 200 role reply must never trigger quit');

// 4. Watchdog drain when a queued write hangs after role ACK still completes deferred quit without extra GET.
quits = [];
scRestartCall('fourDeckRole=1');
const layout3 = reqs.at(-1);
scCall('autoGain=0');
layout3.finish({loggedIn: [], sources: [], rev: 6}, 'layout3');
assert.equal(root.scRestartPending, true);
assert.equal(quits.length, 0);
const reqCountAtHang = reqs.length;
now += 21000;
scCall('');
assert.equal(quits.length, 1, 'watchdog draining final hung write must complete deferred quit');
assert.equal(root.scRestartPending, false);
assert.equal(reqs.length, reqCountAtHang, 'watchdog quit must not fire redundant poll GET');
""")

    @unittest.skipUnless(shutil.which("node"), "Node is required")
    def test_bluetooth_get_ordering_and_invalid_body_recovery(self):
        run_node(self._round2_js_harness() + """
const layers = ['Master', 'Booth', 'Headphones'];
// 1. Reopen during Booth write schedules deferred GET; newer GET (Headphones) arriving first wins over delayed deferred GET (Booth).
scBtSet('Booth');
const write = reqs.at(-1);
scBtLoad();
const beforeAck = reqs.length;
write.finish({layers, assignLayer: 'Booth'});
const deferred = reqs.length > beforeAck ? reqs.at(-1) : null;
assert.ok(deferred && deferred.method === 'GET');
scBtLoad();
const newest = reqs.at(-1);
assert.notEqual(newest, deferred);
newest.finish({layers, assignLayer: 'Headphones'});
assert.equal(scBtLayer, 'Headphones');
deferred.finish({layers, assignLayer: 'Booth'});
assert.equal(scBtLayer, 'Headphones', 'older Bluetooth GET must not replace newer result');

// 2. Write invalidation: GET started before scBtSet is still discarded after scBtSet finishes.
scBtLoad();
const preWriteGet = reqs.at(-1);
scBtSet('Booth');
reqs.at(-1).finish({layers, assignLayer: 'Booth'});
preWriteGet.finish({layers, assignLayer: 'Master'});
assert.equal(scBtLayer, 'Booth', 'write-invalidated Bluetooth GET must still be discarded');

// 3. Invalid/non-object 200 body on scBtSend triggers reload GET; invalid GET body does not throw or bump applied read seq.
const countBeforeBadPost = reqs.length;
scBtSet('Headphones');
const badPost = reqs.at(-1);
Object.assign(badPost, {readyState: 4, status: 200, responseText: ''});
badPost.onreadystatechange();
assert.equal(reqs.length, countBeforeBadPost + 2, 'invalid 200 BT write must trigger scBtLoad recovery');
const recoverGet = reqs.at(-1);
assert.equal(recoverGet.method, 'GET');
const seqBeforeBadGet = root.scBtAppliedReadSeq;
Object.assign(recoverGet, {readyState: 4, status: 200, responseText: '[]'});
recoverGet.onreadystatechange();
assert.equal(root.scBtAppliedReadSeq, seqBeforeBadGet, 'invalid BT GET body must not advance scBtAppliedReadSeq');
scBtLoad();
reqs.at(-1).finish({layers, assignLayer: 'Booth'});
assert.equal(scBtLayer, 'Booth');
""")

    @unittest.skipUnless(shutil.which("node"), "Node is required")
    def test_unsigned_provider_choice_blocks_stale_qr_both_firmwares(self):
        begin = extract_function(self.bp.SERVICES_MAIN, "scLoginBegin")
        login_req = extract_function(self.bp.SERVICES_MAIN, "scLoginRequest")
        for firmware in ("504", "530"):
            with self.subTest(firmware=firmware):
                source = getattr(self.bp, "TIDAL_OPEN_" + firmware)
                real_open = extract_function(source, "scOpen")
                script = self._round2_js_harness().replace("function scOpen(id) { scScope = id; }", real_open)
                run_node(script + begin + login_req + """
scSources = ['spotify', 'deezer'];
scScope = 'tidal';
scLogin = scLoginOpen = 'spotify';
scLoginDone();
const spotifyRead = reqs.at(-1);
scOpen('deezer');
assert.equal(scLoginOpen, 'deezer');
assert.equal(scLogin, 'deezer');
spotifyRead.finish({sources: scSources, scope: 'tidal', loggedIn: ['spotify'], rev: 1}, 'after-login');
assert.equal(scScope, 'tidal', 'old QR must not hijack scope after user opened another provider login');
assert.equal(scLoginOpen, 'deezer');
assert.equal(scLogin, 'deezer');
""")

    @unittest.skipUnless(shutil.which("node"), "Node is required")
    def test_newer_equal_digest_poll_beats_older_qr_get(self):
        run_node(self._round2_js_harness() + """
scSources = ['spotify'];
scScope = 'tidal';
const revoked = {sources: scSources, scope: 'tidal', loggedIn: [], rev: 2};
scCall('');
scUiPoll.finish(revoked, 'revoked');
assert.equal(scUiRev, 2);
scCall('', () => {});
const oldGet = reqs.at(-1);
scLoginOpen = scLogin = 'spotify';
scLoginDone();
now += 3000;
scCall('');
scUiPoll.finish(revoked, 'revoked');
oldGet.finish({sources: ['spotify'], scope: 'tidal', loggedIn: ['spotify'], rev: 1}, 'older-sign-in');
assert.deepEqual(scLoggedIn, [], 'old rev 1 GET must not overwrite a newer rev 2 equal-digest poll');
assert.equal(scUiRev, 2);
assert.equal(scScope, 'tidal');
assert.notEqual(scUiDigest, 'older-sign-in');
""")

    @unittest.skipUnless(shutil.which("node"), "Node is required")
    def test_explicit_features_off_and_shadow_paused_follow_clear_stale_qml_state(self):
        apply_src = extract_function(self.bp.SERVICES_MAIN, "scApply")
        feature_lines = [
            line.strip()
            for line in apply_src.splitlines()
            if line.strip().startswith((
                "if (Array.isArray(v.featuresOff))",
                "if (v.shadowPausedFollow !== undefined)",
            ))
        ]
        self.assertEqual(len(feature_lines), 2, f"missing scApply feature lines: {feature_lines}")
        harness = self._round2_js_harness().replace(
            "function scApply(v) {",
            "root.scFeaturesOff = []; root.scHookFollows = false;\nfunction scApply(v) {\n    "
            + "\n    ".join(feature_lines),
        )
        run_node(harness + """
// Start with stale QML hook feature state (stems hidden, paused follow on).
scFeaturesOff = ['stems', 'shadow-paused-follow'];
scHookFollows = true;

// 1. E4 setting POST reply (scUiFlush -> scApply) carrying explicit featuresOff: [] and shadowPausedFollow: false clears stale state.
scCall('autoGain=1');
reqs.at(-1).finish({autoGain: true, loggedIn: [], featuresOff: [], shadowPausedFollow: false, rev: 1}, 'd-cleared');
assert.deepEqual(scFeaturesOff, [], 'scUiFlush did not clear stale scFeaturesOff on explicit []');
assert.equal(scHookFollows, false, 'scUiFlush did not clear stale scHookFollows on explicit false');
assert.equal(scUiDigest, 'd-cleared');

// 2. Re-populate stale state, then verify 3 s poll (scUiPollReply -> scApply) clears it on explicit [] / false.
scFeaturesOff = ['stems'];
scHookFollows = true;
now += 3000;
scCall('');
scUiPoll.finish({autoGain: true, loggedIn: [], featuresOff: [], shadowPausedFollow: false, rev: 2}, 'd-poll-cleared');
assert.deepEqual(scFeaturesOff, [], 'scUiPollReply did not clear stale scFeaturesOff on explicit []');
assert.equal(scHookFollows, false, 'scUiPollReply did not clear stale scHookFollows on explicit false');
assert.equal(scUiDigest, 'd-poll-cleared');
""")

    @unittest.skipUnless(shutil.which("node"), "Node is required")
    def test_connected_source_scan_button_blocks_stale_qr_both_firmwares(self):
        snippets = [v for v in vars(self.bp).values() if isinstance(v, str) and "root.scLoginBegin(modelData.id)" in v]
        click_line = next(
            (ln for s in snippets for ln in s.splitlines() if "onClicked:" in ln and "root.scLoginBegin(modelData.id)" in ln),
            None,
        )
        self.assertIsNotNone(click_line, "missing ConnectedSource scan button onClicked handler")
        click_js = click_line.split("onClicked:", 1)[1].strip()
        begin = extract_function(self.bp.SERVICES_MAIN, "scLoginBegin")
        login_req = extract_function(self.bp.SERVICES_MAIN, "scLoginRequest")
        for firmware in ("504", "530"):
            with self.subTest(firmware=firmware):
                real_open = extract_function(getattr(self.bp, "TIDAL_OPEN_" + firmware), "scOpen")
                script = self._round2_js_harness().replace("function scOpen(id) { scScope = id; }", real_open)
                run_node(script + begin + login_req + f"""
scScope = 'tidal';
scSources = ['spotify', 'deezer'];
scLoggedIn = [];
scLogin = scLoginOpen = 'spotify';
scLoginDone();
const oldQR = reqs.at(-1);
const modelData = {{id: 'deezer'}};
{click_js}
assert.equal(scLogin, 'deezer');
assert.ok(reqs.at(-1).url.endsWith('provider=deezer'));
oldQR.finish({{scope: 'tidal', sources: ['spotify', 'deezer'], loggedIn: ['spotify'], rev: 1}}, 'old-qr');
assert.equal(scScope, 'tidal', 'stale QR must not override explicit ConnectedSource scan button');
assert.equal(scLogin, 'deezer');
assert.ok(!reqs.some(x => x.url.endsWith('scope=spotify')), 'stale QR must not send scope=spotify');
""")

    @unittest.skipUnless(shutil.which("node"), "Node is required")
    def test_incomplete_reply_preserves_pending_tap_until_authoritative_reply(self):
        run_node(self._round2_js_harness() + """
scScope = 'tidal';
scSources = ['tidal', 'spotify'];
scLoggedIn = [];
scPendingTap = 'spotify';

// Incomplete polls (missing, null, non-array loggedIn) must clear digest without dropping parked scPendingTap.
for (const payload of [
    {scope: 'tidal', sources: ['tidal', 'spotify'], rev: 2},
    {scope: 'tidal', sources: ['tidal', 'spotify'], loggedIn: null, rev: 2},
    {scope: 'tidal', sources: ['tidal', 'spotify'], loggedIn: 'spotify', rev: 2},
]) {
    scUiDigest = 'prev';
    scCall('');
    scUiPoll.finish(payload, 'partial-poll');
    assert.equal(scUiDigest, '', 'incomplete poll must clear digest');
    assert.equal(scPendingTap, 'spotify', 'incomplete poll must not drop parked scPendingTap');
    assert.equal(scScope, 'tidal', 'incomplete poll must not prematurely open parked tap');
}

// Incomplete setting POST reply (scUiFlush) must also preserve parked scPendingTap.
scCall('autoGain=1');
reqs.at(-1).finish({scope: 'tidal', sources: ['tidal', 'spotify'], autoGain: true, rev: 3}, 'partial-post');
assert.equal(scPendingTap, 'spotify', 'incomplete POST reply must not drop parked scPendingTap');
assert.equal(scUiDigest, '');

// Next authoritative poll carrying Array loggedIn resolves scPendingTap cleanly.
scCall('');
scUiPoll.finish({scope: 'tidal', sources: ['tidal', 'spotify'], loggedIn: ['spotify'], rev: 4}, 'complete');
assert.equal(scScope, 'spotify', 'authoritative poll must resolve parked scPendingTap');
assert.equal(scPendingTap, '');
assert.equal(scUiDigest, 'complete');
""")

    @unittest.skipUnless(shutil.which("node"), "Node is required")
    def test_multiple_confirmed_restarts_quit_once_across_combos(self):
        restart = extract_function(self.bp.SERVICES_MAIN, "scRestartCall")
        run_node(self._round2_js_harness() + restart + """
root.scT = s => s;
root.Prompt = {Warning: 1, Cancel: 2, Ok: 4, AcceptRole: 8};
root.QuitReasons = {AnalyticsChange: 1};
root.PlanckGUI = {views: {prompts: {post: () => ({finished: {connect: cb => cb(Prompt.AcceptRole)}})}}};
let exits = 0;
Planck.quit = () => {
    exits++;
    assert.equal(scUiPending, 0, 'quit with UI/tunnel still pending');
    assert.equal(root.scBtSending, false, 'quit with Bluetooth still sending');
};

// 1. Two confirmed role changes drain to exactly one Planck.quit.
scRestartCall('fourDeckRole=13');
const r1 = reqs.at(-1);
scRestartCall('fourDeckRole=24');
r1.finish({fourDeckRole: '13', loggedIn: [], rev: 1}, 'r1');
assert.equal(exits, 0);
const r2 = reqs.at(-1);
assert.ok(r2.url.endsWith('fourDeckRole=24'));
r2.finish({fourDeckRole: '24', loggedIn: [], rev: 2}, 'r2');
assert.equal(exits, 1, 'two accepted role changes must call Planck.quit exactly once');
assert.equal(root.scRestartPending, false);

// 2. Three confirmed role changes drain to exactly one Planck.quit.
exits = 0;
scRestartCall('fourDeckRole=13');
const t1 = reqs.at(-1);
scRestartCall('fourDeckRole=24');
scRestartCall('fourDeckRole=auto');
t1.finish({loggedIn: [], rev: 3}, 't1');
assert.equal(exits, 0);
reqs.at(-1).finish({loggedIn: [], rev: 4}, 't2');
assert.equal(exits, 0);
reqs.at(-1).finish({loggedIn: [], rev: 5}, 't3');
assert.equal(exits, 1, 'three accepted role changes must call Planck.quit exactly once');
assert.equal(root.scRestartPending, false);

// 3. Two confirmed role changes + concurrent tunnel and Bluetooth writes quit once after all drain.
exits = 0;
scTunnelToggle(true);
const tun = reqs.at(-1);
scBtSet('Booth');
const bt = reqs.at(-1);
scRestartCall('fourDeckRole=13');
const c1 = reqs.at(-1);
scRestartCall('fourDeckRole=24');
c1.finish({loggedIn: [], rev: 6}, 'c1');
reqs.at(-1).finish({loggedIn: [], rev: 7}, 'c2');
assert.equal(exits, 0);
assert.equal(root.scRestartPending, true);
tun.finish({enabled: true, status: 'running'});
assert.equal(exits, 0);
bt.finish({layers: ['Master', 'Booth'], assignLayer: 'Booth'});
assert.equal(exits, 1, 'combined multi-restart + tunnel + BT must quit exactly once');
assert.equal(root.scRestartPending, false);

// 4. First succeeds, second fails (500) -> still quits once for the accepted first role change.
exits = 0;
scRestartCall('fourDeckRole=13');
const e1 = reqs.at(-1);
scRestartCall('fourDeckRole=24');
e1.finish({loggedIn: [], rev: 8}, 'e1');
assert.equal(exits, 0);
reqs.at(-1).finish({error: 'fail', rev: 8}, '', 500);
assert.equal(exits, 1, 'prior accepted role change must still quit once if second fails');
assert.equal(root.scRestartPending, false);

// 5. First fails (500), second succeeds -> quits once; both fail -> 0 quits.
exits = 0;
scRestartCall('fourDeckRole=13');
const f1 = reqs.at(-1);
scRestartCall('fourDeckRole=24');
f1.finish({error: 'fail', rev: 8}, '', 500);
assert.equal(exits, 0);
reqs.at(-1).finish({loggedIn: [], rev: 9}, 'f2');
assert.equal(exits, 1);

exits = 0;
scRestartCall('fourDeckRole=13');
const b1 = reqs.at(-1);
scRestartCall('fourDeckRole=24');
b1.finish({error: 'fail', rev: 9}, '', 500);
reqs.at(-1).finish({error: 'fail', rev: 9}, '', 500);
assert.equal(exits, 0, 'two failed restarts must not quit');
assert.equal(root.scRestartPending, false);
""")

    @unittest.skipUnless(shutil.which("node"), "Node is required")
    def test_partial_poll_non_array_sources_or_logged_in_preserves_state_both_firmwares(self):
        actual_apply = extract_function(self.bp.SERVICES_MAIN, "scApply")
        sources_line = next(
            (ln.strip() for ln in actual_apply.splitlines() if "scSources = v.sources" in ln),
            None,
        )
        login_line = next(
            (ln.strip() for ln in actual_apply.splitlines() if "scLoggedIn = v.loggedIn" in ln),
            None,
        )
        self.assertIsNotNone(sources_line, "missing scSources assignment in scApply")
        self.assertIsNotNone(login_line, "missing scLoggedIn assignment in scApply")
        begin = extract_function(self.bp.SERVICES_MAIN, "scLoginBegin")
        login_req = extract_function(self.bp.SERVICES_MAIN, "scLoginRequest")
        for firmware in ("504", "530"):
            real_open = extract_function(getattr(self.bp, "TIDAL_OPEN_" + firmware), "scOpen")
            harness = (
                self._round2_js_harness()
                .replace("function scOpen(id) { scScope = id; }", real_open)
                .replace("if (Array.isArray(v.sources)) scSources = v.sources;", sources_line)
                .replace("if (Array.isArray(v.loggedIn)) scLoggedIn = v.loggedIn;", login_line)
                + begin
                + login_req
            )
            for case in ("signed-out", "signed-in", "bad-sources"):
                with self.subTest(firmware=firmware, case=case):
                    run_node(harness + f"const mode = {case!r};\n" + """
scScope = 'tidal';
scSources = ['tidal', 'spotify'];
scLoggedIn = [];
scLogin = scLoginOpen = 'spotify';
scLoginDone();
assert.equal(scLogin, '');
assert.equal(scLoginOpen, '');
scCall('');
if (mode === 'signed-out') {
    scUiPoll.finish({scope: 'tidal', sources: ['tidal', 'spotify'], loggedIn: 'spotify', rev: 2}, 'partial');
} else if (mode === 'signed-in') {
    scUiPoll.finish({scope: 'tidal', sources: ['tidal', 'spotify'], loggedIn: {}, rev: 2}, 'partial');
} else {
    scUiPoll.finish({scope: 'tidal', sources: 'spotify', loggedIn: {}, rev: 2}, 'partial');
    scCall('');
    scUiPoll.finish({scope: 'tidal', sources: {}, loggedIn: 'spotify', rev: 2}, 'partial2');
}
assert.ok(Array.isArray(scSources), 'non-array v.sources corrupted scSources');
assert.ok(Array.isArray(scLoggedIn), 'non-array v.loggedIn corrupted scLoggedIn');
assert.equal(scUiDigest, '', 'malformed poll must invalidate digest');
const wantSigned = mode !== 'signed-out';
const full = {scope: 'tidal', sources: ['tidal', 'spotify'], loggedIn: wantSigned ? ['spotify'] : [], rev: 3};
scCall('');
scUiPoll.finish(full, 'healthy');
assert.equal(reqs.filter(x => x.url.endsWith('scope=spotify')).length, 0, 'passive refresh must never auto-open');
scOpen('spotify');
const scopeWrites = reqs.filter(x => x.url.endsWith('scope=spotify')).length;
const loginWrites = reqs.filter(x => x.url.includes('/v1/enginex/login?provider=spotify')).length;
assert.deepEqual([scopeWrites, loginWrites], wantSigned ? [1, 0] : [0, 1], 'explicit tap after healthy poll must act on first click');
""")

    @unittest.skipUnless(shutil.which("node"), "Node is required")
    def test_malformed_sources_preserves_pending_tap_and_invalidates_digest_across_all_reply_paths(self):
        actual_apply = extract_function(self.bp.SERVICES_MAIN, "scApply")
        sources_line = next(ln.strip() for ln in actual_apply.splitlines() if "scSources = v.sources" in ln)
        login_line = next(ln.strip() for ln in actual_apply.splitlines() if "scLoggedIn = v.loggedIn" in ln)
        for firmware in ("504", "530"):
            real_open = extract_function(getattr(self.bp, "TIDAL_OPEN_" + firmware), "scOpen")
            harness = (
                self._round2_js_harness()
                .replace("function scOpen(id) { scScope = id; }", real_open)
                .replace("if (Array.isArray(v.sources)) scSources = v.sources;", sources_line)
                .replace("if (Array.isArray(v.loggedIn)) scLoggedIn = v.loggedIn;", login_line)
            )
            for path in ("poll", "flush", "callback-get"):
                for enabled in (False, True):
                    with self.subTest(firmware=firmware, path=path, enabled=enabled):
                        run_node(harness + f"const path = {path!r}, enabled = {str(enabled).lower()};\n" + """
const writes = () => reqs.filter(r => r.url.endsWith('scope=spotify')).length;
scScope = 'tidal';
scSources = ['tidal', 'spotify'];
scLoggedIn = ['spotify'];
scPendingTap = 'spotify';
const current = {scope: 'tidal', sources: enabled ? ['tidal', 'spotify'] : ['tidal'], loggedIn: ['spotify'], rev: 3};
if (path === 'poll') {
    scUiDigest = 'prev';
    scCall('');
    scUiPoll.finish({...current, sources: {}, rev: 2}, 'bad-poll-sources');
} else if (path === 'flush') {
    scUiDigest = 'prev';
    scCall('masterLimiter=1');
    reqs.at(-1).finish({...current, sources: {}, masterLimiter: true, rev: 2}, 'bad-flush-sources');
} else {
    scUiDigest = 'prev';
    scCall('', () => {});
    reqs.at(-1).finish({...current, sources: {}, rev: 2}, 'bad-cb-sources');
}
assert.equal(writes(), 0, path + ' must not send premature scope=spotify on malformed sources');
assert.equal(scPendingTap, 'spotify', path + ' must preserve parked scPendingTap on malformed sources');
assert.equal(scUiDigest, '', path + ' must invalidate digest on malformed sources');

scCall('');
scUiPoll.finish(current, 'healthy');
assert.equal(writes(), enabled ? 1 : 0, path + ' failed to converge on healthy reply');
assert.equal(scPendingTap, '');
assert.equal(scUiDigest, enabled ? '' : 'healthy');
""")

            # Omitted optional `sources` field preserves existing contract across flush and poll.
            with self.subTest(firmware=firmware, path="omitted-sources"):
                run_node(harness + """
function scSelectTidal() { return true; }
function scTidalProvider() { return null; }
function scGatewayLogin() {}
var scSelectPending = false, scTidalLoginPending = false, scTidalLogoutPending = false, scAskedAt = 0;
scScope = 'tidal';
scSources = ['tidal', 'spotify'];
scLoggedIn = ['spotify'];
scOpen('spotify');
assert.equal(reqs.filter(r => r.url.endsWith('scope=spotify')).length, 1);
reqs.at(-1).finish({scope: 'spotify', loggedIn: ['spotify'], rev: 2}, 'scope-ack');
scCall('autoGain=1');
reqs.at(-1).finish({autoGain: true, loggedIn: ['spotify'], rev: 3}, 'omit-flush');
assert.equal(scUiDigest, 'omit-flush', 'scUiFlush omitting optional sources must still cache digest');
now += 3000;
scCall('');
scUiPoll.finish({autoGain: true, loggedIn: ['spotify'], rev: 3}, 'omit-poll');
assert.equal(scUiDigest, 'omit-poll', 'scUiPollReply omitting optional sources must still cache digest');
""")

    def test_legacy_client_interleaving_properties_removed(self):
        legacy_props = (
            "scUiReqSeq",
            "scUiAppliedStartSeq",
            "scUiPollStartSeq",
            "scChoiceGen",
            "scSourcesStale",
            "scLoginStale",
            "scSourcesConfirmed",
            "scSourcesConfirmedStartSeq",
            "scLoggedInConfirmed",
            "scLoggedInConfirmedStartSeq",
            "scQrInFlight",
            "scQrInFlightGen",
            "scQrInFlightStartSeq",
            "scLoginOpenStartSeq",
            "scLoginOpenNeedsPrompt",
        )
        bp_text = BUILD_PACKS.read_text(encoding="utf-8")
        for prop in legacy_props:
            self.assertNotIn(prop, bp_text, f"removed legacy interleaving property {prop} still present in build_packs.py")
        for prop in ("property int scUiRev: 0", "property int scLoginReqGen: 0", 'property string scPendingTap: ""'):
            self.assertIn(prop, self.bp.SERVICES_MAIN, f"missing 3-pillar property {prop}")

    @unittest.skipUnless(shutil.which("node"), "Node is required")
    def test_pillar1_monotonic_server_rev_drops_stale_replies_everywhere_both_firmwares(self):
        actual_apply = extract_function(self.bp.SERVICES_MAIN, "scApply")
        sources_line = next(ln.strip() for ln in actual_apply.splitlines() if "scSources = v.sources" in ln)
        login_line = next(ln.strip() for ln in actual_apply.splitlines() if "scLoggedIn = v.loggedIn" in ln)
        begin = extract_function(self.bp.SERVICES_MAIN, "scLoginBegin")
        login_req = extract_function(self.bp.SERVICES_MAIN, "scLoginRequest")
        for firmware in ("504", "530"):
            real_open = extract_function(getattr(self.bp, "TIDAL_OPEN_" + firmware), "scOpen")
            harness = (
                self._round2_js_harness()
                .replace("function scOpen(id) { scScope = id; }", real_open)
                .replace("if (Array.isArray(v.sources)) scSources = v.sources;", sources_line)
                .replace("if (Array.isArray(v.loggedIn)) scLoggedIn = v.loggedIn;", login_line)
                + begin
                + login_req
                + """
function scSelectTidal() { return true; }
function scTidalProvider() { return null; }
function scGatewayLogin() {}
var scSelectPending = false, scTidalLoginPending = false, scTidalLogoutPending = false, scAskedAt = 0;
let applyCount = 0;
const _origApply = scApply;
scApply = function(v) { applyCount++; return _origApply(v); };
"""
            )
            with self.subTest(firmware=firmware, case="pre-commit-get-dropped-after-post-ack"):
                run_node(harness + """
// R14 F2 exact repro: pre-commit poll (rev 2, spotify OFF) arrives AFTER committed source_spotify=1 POST (rev 3, spotify ON).
scScope = 'tidal';
scSources = ['tidal'];
scLoggedIn = ['spotify'];
scUiRev = 2;
scCall('', () => {});
const preCommitGet = reqs.at(-1);
scSetSource('spotify', true);
const enablePost = reqs.at(-1);
assert.equal(enablePost.url, 'http://127.0.0.1:8080/v1/enginex/ui?source_spotify=1');
enablePost.finish({scope: 'tidal', sources: ['tidal', 'spotify'], loggedIn: ['spotify'], rev: 3}, 'rev3');
assert.equal(scUiRev, 3);
assert.deepEqual(scSources, ['tidal', 'spotify']);
// Delayed pre-commit GET with rev 2 arrives now: MUST be dropped unconditionally.
preCommitGet.finish({scope: 'tidal', sources: ['tidal'], loggedIn: ['spotify'], rev: 2}, 'rev2');
assert.equal(scUiRev, 3, 'lower rev 2 must never regress scUiRev');
assert.deepEqual(scSources, ['tidal', 'spotify'], 'lower rev 2 pre-commit GET must not revert spotify');
// Immediate footer tap succeeds on the FIRST click!
scOpen('spotify');
const scopePosts = reqs.filter(r => r.url.endsWith('scope=spotify'));
assert.equal(scopePosts.length, 1, 'footer tap after dropped pre-commit GET must open on first click');
""")
            with self.subTest(firmware=firmware, case="poll-header-rev-drops-before-parse"):
                run_node(harness + """
scScope = 'tidal';
scSources = ['tidal', 'spotify'];
scLoggedIn = ['spotify'];
scUiRev = 5;
scUiDigest = 'd5';
scUiAppliedAt = now;
const beforeApply = applyCount;
scCall('');
// Header X-UI-Rev: 4 < scUiRev (5) with different digest AND unparseable body proves drop happens before JSON.parse.
Object.assign(scUiPoll, {readyState: 4, status: 200, responseText: '{not-json', digest: 'd4', revHeader: '4'});
scUiPoll.onreadystatechange();
assert.equal(applyCount, beforeApply, 'poll with X-UI-Rev < scUiRev must be dropped before JSON.parse');
assert.equal(scUiRev, 5);
assert.equal(scUiDigest, 'd5');
// Poll without X-UI-Rev header but body rev: 4 < 5 is also dropped cleanly.
scCall('');
Object.assign(scUiPoll, {readyState: 4, status: 200, responseText: JSON.stringify({sources: ['tidal'], loggedIn: [], rev: 4}), digest: 'd4b', revHeader: ''});
scUiPoll.onreadystatechange();
assert.equal(applyCount, beforeApply, 'poll with body rev < scUiRev must be dropped');
assert.equal(scUiRev, 5);
assert.deepEqual(scSources, ['tidal', 'spotify']);
""")
            with self.subTest(firmware=firmware, case="flush-lower-rev-drops-and-resyncs"):
                run_node(harness + """
scScope = 'tidal';
scSources = ['tidal', 'spotify'];
scLoggedIn = ['spotify'];
scUiRev = 10;
scCall('autoGain=1');
const post = reqs.at(-1);
// Delayed POST reply carrying rev 9 < scUiRev 10 must NOT overwrite state or regress scUiRev, and must schedule resync GET.
post.finish({autoGain: false, sources: ['tidal'], loggedIn: [], rev: 9}, 'd9');
assert.equal(scUiRev, 10, 'stale POST reply rev 9 must not regress scUiRev 10');
assert.deepEqual(scSources, ['tidal', 'spotify'], 'stale POST reply rev 9 must not revert scSources');
assert.equal(scUiDigest, '', 'stale POST reply clears digest to trigger resync');
assert.equal(reqs.at(-1).method, 'GET', 'stale POST reply must trigger resync GET');
reqs.at(-1).finish({autoGain: true, sources: ['tidal', 'spotify'], loggedIn: ['spotify'], rev: 11}, 'd11');
assert.equal(scUiRev, 11);
assert.equal(scAutoGain, true);
""")
            with self.subTest(firmware=firmware, case="login-request-rev-and-reqgen-guards"):
                run_node(harness + """
scUiRev = 5;
scSources = ['tidal', 'spotify'];
scLoggedIn = [];
scLoginBegin('spotify');
const postLogin = reqs.at(-1);
assert.equal(scLoginStatus, 'starting');
// Unrelated setting write bumps scUiRev to 6 while POST /v1/enginex/login (rev 5, waiting) is in flight.
scCall('autoGain=1');
reqs.at(-1).finish({autoGain: true, sources: ['tidal', 'spotify'], loggedIn: [], rev: 6}, 'd6');
assert.equal(scUiRev, 6);
// POST /v1/enginex/login returning waiting at lower rev 5 < 6 MUST be ignored (R15 Finding 3: no POST carve-out).
postLogin.finish({provider: 'spotify', status: 'waiting', target: 'http://127.0.0.1:8080/m/spotify?t=abc', rev: 5});
assert.equal(scLoginStatus, 'starting', 'lower-rev POST login reply must not change dialog status');
assert.equal(scLoginSerial, 0, 'lower-rev POST login reply must not bump scLoginSerial');
assert.equal(scUiRev, 6, 'older POST login reply must not regress scUiRev');
// Fresh POST login at rev 6 >= scUiRev 6 displays QR cleanly.
scLoginBegin('spotify');
reqs.at(-1).finish({provider: 'spotify', status: 'waiting', target: 'http://127.0.0.1:8080/m/spotify?t=def', rev: 6});
assert.equal(scLoginStatus, 'waiting');
assert.equal(scLoginSerial, 1);
assert.equal(scUiRev, 6);
// GET /v1/enginex/login poll with rev 5 < scUiRev 6 MUST be dropped!
let doneHits = 0;
scLoginRequest('GET', 'spotify', () => { doneHits++; });
reqs.at(-1).finish({provider: 'spotify', status: 'done', rev: 5});
assert.equal(doneHits, 0, 'GET login status with rev < scUiRev must be dropped');
// GET /v1/enginex/login poll with rev 7 >= scUiRev 6 advances scUiRev and fires callback.
scLoginRequest('GET', 'spotify', (v) => { if (v.status === 'done') doneHits++; });
reqs.at(-1).finish({provider: 'spotify', status: 'done', rev: 7});
assert.equal(doneHits, 1);
assert.equal(scUiRev, 7);
// Stale login reply from superseded scLoginReqGen is blocked even with a higher rev.
scLoginBegin('spotify');
const oldLoginReq = reqs.at(-1);
scLoginDone(); // closes dialog & bumps scLoginReqGen
scLoginBegin('spotify'); // reopens dialog with a new scLoginReqGen
oldLoginReq.finish({provider: 'spotify', status: 'expired', rev: 99});
assert.equal(scLoginStatus, 'starting', 'stale scLoginReqGen reply must be ignored even with higher rev');
assert.equal(scUiRev, 7, 'stale scLoginReqGen reply must not advance scUiRev');
""")
            with self.subTest(firmware=firmware, case="malformed-200-never-advances-rev-and-equal-digest-perf"):
                run_node(harness + """
scUiRev = 4;
scCall('');
scUiPoll.finish({scope: 'tidal', sources: ['tidal', 'spotify'], loggedIn: ['spotify'], rev: 5}, 'd5');
assert.equal(scUiRev, 5);
const afterPrime = applyCount;
// 5 unchanged polls with matching digest & rev=5 trigger 0 extra scApply calls.
for (let i = 0; i < 5; i++) {
    now += 3000;
    scCall('');
    scUiPoll.finish({scope: 'tidal', sources: ['tidal', 'spotify'], loggedIn: ['spotify'], rev: 5}, 'd5');
}
assert.equal(applyCount, afterPrime, '5 unchanged polls must trigger 0 extra scApply calls');
// Malformed 200 JSON with X-UI-Rev: 99 never advances scUiRev on flush, poll, or callback GET.
scCall('autoGain=1');
Object.assign(reqs.at(-1), {readyState: 4, status: 200, responseText: '{bad', revHeader: '99'});
reqs.at(-1).onreadystatechange();
assert.equal(scUiRev, 5, 'malformed flush 200 must not advance scUiRev');
reqs.at(-1).finish({autoGain: false, sources: ['tidal', 'spotify'], loggedIn: ['spotify'], rev: 5}, 'd5');
scCall('', () => {});
Object.assign(reqs.at(-1), {readyState: 4, status: 200, responseText: '[]', revHeader: '99'});
reqs.at(-1).onreadystatechange();
assert.equal(scUiRev, 5, 'malformed callback GET 200 must not advance scUiRev');
""")

    @unittest.skipUnless(shutil.which("node"), "Node is required")
    def test_pillar2_source_delta_preserves_other_clients_and_coalescing_both_firmwares(self):
        actual_apply = extract_function(self.bp.SERVICES_MAIN, "scApply")
        sources_line = next(ln.strip() for ln in actual_apply.splitlines() if "scSources = v.sources" in ln)
        login_line = next(ln.strip() for ln in actual_apply.splitlines() if "scLoggedIn = v.loggedIn" in ln)
        for firmware in ("504", "530"):
            real_open = extract_function(getattr(self.bp, "TIDAL_OPEN_" + firmware), "scOpen")
            harness = (
                self._round2_js_harness()
                .replace("function scOpen(id) { scScope = id; }", real_open)
                .replace("if (Array.isArray(v.sources)) scSources = v.sources;", sources_line)
                .replace("if (Array.isArray(v.loggedIn)) scLoggedIn = v.loggedIn;", login_line)
            )
            with self.subTest(firmware=firmware, case="r14-f1-console-disable-preserved-across-deck-delta"):
                run_node(harness + """
// R14 F1 exact repro: Deck still sees deezer ON; Web Console disabled deezer (rev 2);
// Deck enables soundcloud -> sends ONLY source_soundcloud=1 (never full sources= list).
scSources = ['tidal', 'deezer', 'rekordbox'];
scUiRev = 1;
scSetSource('soundcloud', true);
const post = reqs.at(-1);
assert.equal(post.url, 'http://127.0.0.1:8080/v1/enginex/ui?source_soundcloud=1', 'deck must send single-provider delta');
assert.ok(!post.url.includes('sources='), 'deck must not send full sources= overwrite');
assert.ok(!post.url.includes('rekordbox'), 'deck must never serialize runtime rekordbox');
// Server applies delta against current state (deezer already OFF from Web Console) and replies with rev 3.
post.finish({sources: ['tidal', 'soundcloud', 'rekordbox'], loggedIn: ['rekordbox'], rev: 3}, 'rev3');
assert.deepEqual(scSources, ['tidal', 'soundcloud', 'rekordbox'], 'Web Console disable of deezer must be preserved');
assert.equal(scUiRev, 3);
""")
            with self.subTest(firmware=firmware, case="same-provider-coalesce-and-distinct-provider-fifo"):
                run_node(harness + """
scSources = ['tidal', 'spotify', 'deezer', 'rekordbox'];
// Hold an unrelated write in flight so queued source deltas can coalesce or queue.
scCall('autoGain=1');
const first = reqs.at(-1);
// Same provider rapid flip (spotify 0 -> 1) coalesces into source_spotify=1.
scSetSource('spotify', false);
scSetSource('spotify', true);
// Distinct provider toggles (deezer 0, soundcloud 1) do NOT coalesce into each other or spotify.
scSetSource('deezer', false);
scSetSource('soundcloud', true);
assert.deepEqual(scUiQueue.map(q => q.query), ['source_spotify=1', 'source_deezer=0', 'source_soundcloud=1']);
assert.ok(scSources.includes('spotify') && !scSources.includes('deezer') && scSources.includes('soundcloud') && scSources.includes('rekordbox'));
// Drain all four in FIFO order.
first.finish({autoGain: true, sources: ['tidal', 'spotify', 'deezer', 'rekordbox'], loggedIn: [], rev: 2}, 'r2');
reqs.at(-1).finish({autoGain: true, sources: ['tidal', 'spotify', 'deezer', 'rekordbox'], loggedIn: [], rev: 3}, 'r3');
reqs.at(-1).finish({autoGain: true, sources: ['tidal', 'spotify', 'rekordbox'], loggedIn: [], rev: 4}, 'r4');
reqs.at(-1).finish({autoGain: true, sources: ['tidal', 'spotify', 'soundcloud', 'rekordbox'], loggedIn: [], rev: 5}, 'r5');
assert.deepEqual(scSources, ['tidal', 'spotify', 'soundcloud', 'rekordbox']);
assert.equal(scUiPending, 0);
""")
            with self.subTest(firmware=firmware, case="disable-clears-active-login-and-pending-tap"):
                run_node(harness + """
scSources = ['tidal', 'spotify'];
scLogin = 'spotify';
scLoginOpen = 'spotify';
scPendingTap = 'spotify';
const prevGen = scLoginReqGen;
scSetSource('spotify', false);
assert.equal(scLogin, '', 'disabling provider closes its active QR dialog');
assert.equal(scLoginOpen, '');
assert.equal(scPendingTap, '', 'disabling provider clears its parked pending tap');
assert.equal(scLoginReqGen, prevGen + 1, 'disabling provider invalidates in-flight login requests');
assert.equal(reqs.at(-1).url, 'http://127.0.0.1:8080/v1/enginex/ui?source_spotify=0');
""")

    @unittest.skipUnless(shutil.which("node"), "Node is required")
    def test_pillar3_passive_scan_and_pending_tap_schedules_both_firmwares(self):
        actual_apply = extract_function(self.bp.SERVICES_MAIN, "scApply")
        sources_line = next(ln.strip() for ln in actual_apply.splitlines() if "scSources = v.sources" in ln)
        login_line = next(ln.strip() for ln in actual_apply.splitlines() if "scLoggedIn = v.loggedIn" in ln)
        begin = extract_function(self.bp.SERVICES_MAIN, "scLoginBegin")
        login_req = extract_function(self.bp.SERVICES_MAIN, "scLoginRequest")
        for firmware in ("504", "530"):
            real_open = extract_function(getattr(self.bp, "TIDAL_OPEN_" + firmware), "scOpen")
            harness = (
                self._round2_js_harness()
                .replace("function scOpen(id) { scScope = id; }", real_open)
                .replace("if (Array.isArray(v.sources)) scSources = v.sources;", sources_line)
                .replace("if (Array.isArray(v.loggedIn)) scLoggedIn = v.loggedIn;", login_line)
                + begin
                + login_req
                + """
function scSelectTidal() { return true; }
function scTidalProvider() { return null; }
function scGatewayLogin() {}
var scSelectPending = false, scTidalLoginPending = false, scTidalLogoutPending = false, scAskedAt = 0;
let enterStarts = 0;
scEnterTimer = {start() { enterStarts++; }};
const scopePosts = () => reqs.filter(r => r.url.endsWith('scope=spotify'));
const loginPosts = () => reqs.filter(r => r.method === 'POST' && r.url.includes('/v1/enginex/login?provider=spotify'));
const sourcePosts = () => reqs.filter(r => r.url.includes('?source_spotify='));
"""
            )
            # 1. R14 F3: scLoginDone() and external sign-in across poll/flush/callback-GET are 100% passive.
            for via in ("scan-done", "external-poll", "external-flush", "external-callback-get"):
                with self.subTest(firmware=firmware, case="passive-signin", via=via):
                    run_node(harness + f"const via = {via!r};\n" + """
scScope = 'tidal';
scSources = ['tidal', 'spotify'];
scLoggedIn = [];
scLogin = 'spotify';
scLoginOpen = 'spotify';
const gen0 = scLoginReqGen;
if (via === 'scan-done') {
    scCall('autoGain=1');
    const tailWrite = reqs.at(-1);
    scLoginDone();
    assert.equal(scLogin, '');
    assert.equal(scLoginOpen, '');
    assert.equal(scLoginReqGen, gen0 + 1);
    tailWrite.finish({scope: 'tidal', sources: ['tidal', 'spotify'], loggedIn: ['spotify'], autoGain: true, rev: 3}, 'r3');
    scCall('');
    scUiPoll.finish({scope: 'tidal', sources: ['tidal', 'spotify'], loggedIn: ['spotify'], autoGain: true, rev: 3}, 'r3');
} else if (via === 'external-poll') {
    scCall('');
    scUiPoll.finish({scope: 'tidal', sources: ['tidal', 'spotify'], loggedIn: ['spotify'], rev: 2}, 'r2');
} else if (via === 'external-flush') {
    scCall('autoGain=1');
    reqs.at(-1).finish({scope: 'tidal', sources: ['tidal', 'spotify'], loggedIn: ['spotify'], autoGain: true, rev: 2}, 'r2');
} else {
    scCall('', () => {});
    reqs.at(-1).finish({scope: 'tidal', sources: ['tidal', 'spotify'], loggedIn: ['spotify'], rev: 2}, 'r2');
}
assert.equal(scLogin, '', via + ' must close scLogin dialog');
assert.equal(scLoginOpen, '', via + ' must clear scLoginOpen');
assert.ok(scLoginReqGen > gen0, via + ' must bump scLoginReqGen');
assert.deepEqual(scLoggedIn, ['spotify'], via + ' must refresh loggedIn state');
assert.equal(scopePosts().length, 0, via + ' must NEVER auto-open scope=spotify');
assert.equal(loginPosts().length, 0, via + ' must NEVER call scLoginBegin');
assert.equal(enterStarts, 0, via + ' must NEVER start scEnterTimer');
// Explicit tap after passive sign-in opens on first click!
scOpen('spotify');
assert.equal(scopePosts().length, 1, 'explicit tap after ' + via + ' must open on first click');
""")

            # 2. Intermediate confirmed enable ACK acts immediately on parked footer tap before trailing cueCountdown=0 completes.
            for signed_out in (False, True):
                for second_tap in (False, True):
                    with self.subTest(firmware=firmware, case="pending-tap-intermediate-enable",
                                      signedOut=signed_out, secondTap=second_tap):
                        run_node(harness + f"const out={str(signed_out).lower()}, again={str(second_tap).lower()};\n" + """
const good = {scope: 'tidal', sources: ['tidal', 'spotify'], loggedIn: ['spotify'], rev: 1};
const disabled = {scope: 'tidal', sources: ['tidal'], loggedIn: [], rev: 2};
scCall('');
scUiPoll.finish(good, 'initial');
scSetSource('spotify', false);
scCall('autoGain=1');
sourcePosts()[0].finish(disabled, 'disable');
const ordinary = reqs.find(r => r.url.endsWith('autoGain=1'));
scSetSource('spotify', true);
scCall('cueCountdown=0');
scOpen('spotify'); // parked in scPendingTap because source_spotify=1 is queued
assert.equal(scPendingTap, 'spotify');
assert.deepEqual([loginPosts().length, scopePosts().length], [0, 0]);
ordinary.finish({...disabled, autoGain: true, rev: 3}, 'old');
const on = out ? {scope: 'tidal', sources: ['tidal', 'spotify'], loggedIn: [], rev: 4}
               : {scope: 'tidal', sources: ['tidal', 'spotify'], loggedIn: ['spotify'], rev: 4};
sourcePosts()[1].finish(on, 'enable-success');
const tail = reqs.find(r => r.url.endsWith('cueCountdown=0'));
assert.ok(tail && tail.readyState === 1, 'trailing unrelated save must still be in flight');
assert.equal(scPendingTap, '', 'intermediate enable ACK must resolve parked scPendingTap immediately');
const want = out ? [1, 0] : [0, 1];
assert.deepEqual([loginPosts().length, scopePosts().length], want, 'parked footer tap must act immediately at intermediate enable ACK');
if (again) scOpen('spotify');
tail.finish({...on, cueCountdown: false, rev: 5}, 'tail-done');
assert.deepEqual([loginPosts().length, scopePosts().length], want);
""")

            # 3. Intermediate enable omitting loggedIn waits until trailing save confirms login state.
            with self.subTest(firmware=firmware, case="pending-tap-omitted-login-waits"):
                run_node(harness + """
scCall('');
scUiPoll.finish({scope: 'tidal', sources: ['tidal'], loggedIn: [], rev: 1}, 'init');
scSetSource('spotify', true);
scCall('cueCountdown=0');
scOpen('spotify');
assert.equal(scPendingTap, 'spotify');
sourcePosts()[0].finish({scope: 'tidal', sources: ['tidal', 'spotify'], rev: 2}, 'enable-no-login');
assert.deepEqual([loginPosts().length, scopePosts().length], [0, 0], 'omitted loggedIn at intermediate enable must wait');
assert.equal(scPendingTap, 'spotify');
const tail = reqs.find(r => r.url.endsWith('cueCountdown=0'));
assert.ok(tail && tail.readyState === 1);
tail.finish({scope: 'tidal', sources: ['tidal', 'spotify'], loggedIn: [], cueCountdown: false, rev: 3}, 'tail-confirms-out');
assert.deepEqual([loginPosts().length, scopePosts().length], [1, 0]);
assert.equal(scLogin, 'spotify');
assert.equal(scPendingTap, '');
""")

            # 4. Rejected enable (HTTP 503 or server omits spotify) clears scPendingTap with 0 logins/scopes;
            #    an unrelated HTTP 500 ahead of queued source_spotify=1 preserves scPendingTap!
            with self.subTest(firmware=firmware, case="pending-tap-error-and-rejection"):
                run_node(harness + """
scSources = ['tidal'];
scLoggedIn = ['spotify'];
scSetSource('spotify', true);
scOpen('spotify');
assert.equal(scPendingTap, 'spotify');
sourcePosts()[0].finish({error: 'busy'}, '', 503);
assert.equal(scPendingTap, '', 'failed enable POST must clear scPendingTap');
assert.deepEqual([loginPosts().length, scopePosts().length], [0, 0]);
reqs.at(-1).finish({scope: 'tidal', sources: ['tidal'], loggedIn: ['spotify'], rev: 2}, 'resync');
assert.deepEqual([loginPosts().length, scopePosts().length], [0, 0]);

// Unrelated sdHighSpeed=1 HTTP 500 ahead of queued source_spotify=1 preserves scPendingTap until source_spotify=1 settles.
scCall('sdHighSpeed=1');
const badSd = reqs.at(-1);
scSetSource('spotify', true);
scOpen('spotify');
assert.equal(scPendingTap, 'spotify');
badSd.finish({error: 'sd-fail'}, '', 500);
assert.equal(scPendingTap, 'spotify', 'unrelated earlier 500 must not clear scPendingTap for still-queued source_spotify=1');
const en = sourcePosts().at(-1);
en.finish({scope: 'tidal', sources: ['tidal', 'spotify'], loggedIn: ['spotify'], rev: 3}, 'enabled');
assert.equal(scPendingTap, '');
assert.equal(scopePosts().length, 1);
""")

            # 5. Partial scope ACK omitting sources/loggedIn still retries superseded scOpen cleanly (#269).
            for shape in ("bare", "no-login"):
                for last in ("spotify", "tidal"):
                    with self.subTest(firmware=firmware, case="partial-scope-retries", shape=shape, last=last):
                        run_node(harness + f"const shape={shape!r}, last={last!r};\n" + """
const good = {scope: 'spotify', sources: ['tidal', 'spotify'], loggedIn: ['spotify'], rev: 1};
scCall('');
scUiPoll.finish(good, 'initial');
scOpen('tidal');
scOpen('spotify');
if (last === 'tidal') scOpen('tidal');
const ack = {scope: 'tidal', rev: 2};
if (shape === 'no-login') ack.sources = good.sources;
reqs.find(r => r.url.endsWith('scope=tidal')).finish(ack, 'partial');
for (const req of scopePosts()) if (req.readyState === 1) req.finish({...good, scope: 'spotify', rev: 3}, 'retried');
assert.deepEqual([scopePosts().length, scScope, loginPosts().length], [Number(last === 'spotify'), last, 0]);
""")

    @unittest.skipUnless(shutil.which("node"), "Node is required")
    def test_r16_epoch_restart_strict_rev_stale_callbacks_and_parked_tap(self):
        """Permanent regression suite for all R15 client findings (BLOCK 2 + FIX 3/4/5) across 5.0.4 and 5.3.0."""
        main = self.bp.SERVICES_MAIN
        for prop in ("property string scUiEpoch: \"\"", "property var scRetiredEpochs: ({})"):
            self.assertIn(prop, main)
        actual_apply = extract_function(main, "scApply")
        sources_line = next(ln.strip() for ln in actual_apply.splitlines() if "scSources = v.sources" in ln)
        login_line = next(ln.strip() for ln in actual_apply.splitlines() if "scLoggedIn = v.loggedIn" in ln)
        begin = extract_function(main, "scLoginBegin")
        login_req = extract_function(main, "scLoginRequest")
        for firmware in ("504", "530"):
            real_open = extract_function(getattr(self.bp, "TIDAL_OPEN_" + firmware), "scOpen")
            harness = (
                self._round2_js_harness()
                .replace("function scOpen(id) { scScope = id; }", real_open)
                .replace("if (Array.isArray(v.sources)) scSources = v.sources;", sources_line)
                .replace("if (Array.isArray(v.loggedIn)) scLoggedIn = v.loggedIn;", login_line)
                + begin
                + login_req
                + """
function scSelectTidal() { return true; }
function scTidalProvider() { return null; }
function scGatewayLogin() {}
var scSelectPending = false, scTidalLoginPending = false, scTidalLogoutPending = false, scAskedAt = 0;
let enterStarts = 0;
scEnterTimer = {start() { enterStarts++; }};
const scopePosts = () => reqs.filter(r => r.method === 'POST' && r.url.endsWith('scope=spotify'));
const loginPosts = () => reqs.filter(r => r.method === 'POST' && r.url.includes('/v1/enginex/login?provider=spotify'));
const sourcePosts = () => reqs.filter(r => r.method === 'POST' && r.url.includes('?source_spotify='));
"""
            )
            with self.subTest(firmware=firmware, case="block2-epoch-restart-and-retired-inflight"):
                run_node(harness + """
// Server starts on epoch E1 at rev 10 with Spotify enabled & logged in.
scCall('');
scUiPoll.finish({scope: 'tidal', sources: ['tidal', 'spotify'], loggedIn: ['spotify'], rev: 10, epoch: 'E1'}, 'd10');
assert.equal(scUiEpoch, 'E1');
assert.equal(scUiRev, 10);
assert.ok(scSources.includes('spotify'));

// Capture a pre-restart in-flight GET from E1 (will arrive AFTER E2 restart).
scCall('', () => {});
const latePreRestart = reqs.at(-1);

// Server restarts with new epoch E2 and low ephemeral rev 1 (Spotify now disabled).
// Must recover on the VERY FIRST poll, retire E1, adopt E2/rev=1, and apply fresh state.
now += 3000;
scCall('');
scUiPoll.finish({scope: 'tidal', sources: ['tidal'], loggedIn: [], rev: 1, epoch: 'E2'}, 'd10'); // even same digest!
assert.equal(scUiEpoch, 'E2');
assert.equal(scUiRev, 1);
assert.equal(scRetiredEpochs['E1'], true);
assert.ok(!scSources.includes('spotify'), 'new epoch must apply immediately on first poll');

// Late in-flight pre-restart reply from retired E1 with higher rev 99 MUST be dropped!
latePreRestart.finish({scope: 'tidal', sources: ['tidal', 'spotify'], loggedIn: ['spotify'], rev: 99, epoch: 'E1'}, 'd99');
assert.equal(scUiEpoch, 'E2');
assert.equal(scUiRev, 1);
assert.ok(!scSources.includes('spotify'), 'retired epoch pre-restart in-flight reply must be dropped');

// Advance E2 to rev 5, then verify same-epoch lower rev 2 is ALWAYS dropped (even repeated 5 times).
now += 3000;
scCall('');
scUiPoll.finish({scope: 'tidal', sources: ['tidal', 'spotify'], loggedIn: ['spotify'], rev: 5, epoch: 'E2'}, 'd5');
assert.equal(scUiRev, 5);
for (let i = 0; i < 5; i++) {
    now += 3000;
    scCall('');
    scUiPoll.finish({scope: 'tidal', sources: ['tidal'], loggedIn: [], rev: 2, epoch: 'E2'}, 'd2-' + i);
}
assert.equal(scUiRev, 5);
assert.ok(scSources.includes('spotify'), 'same-epoch lower rev must never reset state');

// Persisted monotonic restart (E2 rev 5 -> E3 rev 6) retires E2 and adopts E3/rev 6 cleanly.
now += 3000;
scCall('');
scUiPoll.finish({scope: 'tidal', sources: ['tidal'], loggedIn: [], rev: 6, epoch: 'E3'}, 'd6');
assert.equal(scUiEpoch, 'E3');
assert.equal(scUiRev, 6);
assert.equal(scRetiredEpochs['E2'], true);
assert.ok(!scSources.includes('spotify'));
""")

            with self.subTest(firmware=firmware, case="block2-epochless-restart-fallback"):
                run_node(harness + """
// Harness omitting epoch: rev 10 -> 3 consecutive non-decreasing low-rev polls (1, 1, 1) recover,
// while descending stale polls (9, 8, 7) never trigger restart recovery.
scCall('');
scUiPoll.finish({scope: 'tidal', sources: ['tidal', 'spotify'], loggedIn: ['spotify'], rev: 10}, 'd10');
for (const r of [9, 8, 7]) {
    now += 3000;
    scCall('');
    scUiPoll.finish({scope: 'tidal', sources: ['tidal'], loggedIn: [], rev: r}, 'd' + r);
}
assert.ok(scSources.includes('spotify'), 'descending stale polls must not trigger restart recovery');
assert.equal(scUiRev, 10);
for (let i = 0; i < 3; i++) {
    now += 3000;
    scCall('');
    scUiPoll.finish({scope: 'tidal', sources: ['tidal'], loggedIn: [], rev: 1}, 'r1-' + i);
}
assert.ok(!scSources.includes('spotify'), 'epochless 3-poll restart fallback must recover surviving UI');
assert.equal(scUiRev, 1);
""")

            with self.subTest(firmware=firmware, case="fix3-lower-rev-login-and-stale-scope-and-flush-ack"):
                run_node(harness + """
scCall('');
scUiPoll.finish({scope: 'tidal', sources: ['tidal', 'spotify'], loggedIn: [], rev: 10}, 'd10');
// 1. Lower-rev POST login replies for waiting/expired/done must all be ignored.
for (const st of ['waiting', 'expired', 'done']) {
    scLoginBegin('spotify');
    const post = loginPosts().at(-1);
    now += 3000;
    scCall('');
    scUiPoll.finish({scope: 'tidal', sources: ['tidal', 'spotify'], loggedIn: [], rev: 20}, 'd20-' + st);
    const snap = [scLoginStatus, scLoginSerial, scLoginLocal, scLogin, scUiRev];
    post.finish({status: st, target: '/m/spotify', rev: 11});
    assert.deepEqual([scLoginStatus, scLoginSerial, scLoginLocal, scLogin, scUiRev], snap, 'lower POST login ' + st + ' mutated dialog');
}
// 2. Stale scope ACK after higher-rev GET or poll decrements scScopePending without firing scEnterTimer.
scLogin = '';
now += 3000;
scCall('');
scUiPoll.finish({scope: 'tidal', sources: ['tidal', 'spotify'], loggedIn: ['spotify'], rev: 21}, 'd21');
for (const via of ['get', 'poll']) {
    scScope = 'tidal';
    scOpen('spotify');
    const oldScope = scopePosts().at(-1);
    assert.equal(scScopePending, 1);
    if (via === 'get') {
        scCall('', () => {});
        reqs.at(-1).finish({scope: 'spotify', sources: ['tidal', 'spotify'], loggedIn: ['spotify'], rev: 30}, 'd30-' + via);
    } else {
        now += 3000;
        scCall('');
        scUiPoll.finish({scope: 'spotify', sources: ['tidal', 'spotify'], loggedIn: ['spotify'], rev: 30}, 'd30-' + via);
    }
    const beforeEnter = enterStarts;
    oldScope.finish({scope: 'spotify', sources: ['tidal', 'spotify'], loggedIn: ['spotify'], rev: 22}, 'd22');
    assert.equal(scScopePending, 0, 'stale scope ACK must keep request bookkeeping');
    assert.equal(enterStarts, beforeEnter, 'stale scope ACK must never fire scEnterTimer via ' + via);
    scUiRev = 21;
}
// 3. Stale queued POST ACK in scUiFlush keeps queue bookkeeping without firing item.done.
scUiRev = 30;
let flushDone = 0;
scCall('autoGain=1', () => { flushDone++; });
const staleFlush = reqs.at(-1);
staleFlush.finish({autoGain: true, sources: ['tidal', 'spotify'], loggedIn: ['spotify'], rev: 15}, 'd15');
assert.equal(flushDone, 0, 'stale scUiFlush ACK must not invoke item.done');
assert.equal(scUiPending, 0, 'stale scUiFlush ACK must clear pending queue');
assert.equal(reqs.at(-1).method, 'GET', 'stale scUiFlush ACK must trigger resync GET');
""")

            with self.subTest(firmware=firmware, case="fix4-invalid-rev-rejected-and-fx-probe-independent"):
                run_node(harness + """
// Cold start (scUiRev === 0): missing/0/negative/float/non-numeric rev must be rejected and refresh!
for (const bad of [{sources: ['tidal'], loggedIn: []}, {sources: ['tidal'], loggedIn: [], rev: 0}, {sources: ['tidal'], loggedIn: [], rev: -1}, {sources: ['tidal'], loggedIn: [], rev: 1.5}, {sources: ['tidal'], loggedIn: [], rev: 'bad'}, {sources: ['tidal'], loggedIn: [], rev: '12abc'}]) {
    const beforeReqs = reqs.length;
    scCall('');
    scUiPoll.finish(bad, 'bad-cold');
    assert.equal(scUiRev, 0, 'invalid cold-start rev must not advance scUiRev');
    assert.ok(reqs.length > beforeReqs + 1, 'invalid cold-start rev must trigger refresh');
    // Reset scUiPoll for next sub-iteration
    scUiPoll.readyState = 4;
}
// After valid rev 10: GET, poll, final ACK, and login POST/GET all reject missing/0/bad/mismatch rev.
scCall('');
scUiPoll.finish({scope: 'tidal', sources: ['tidal', 'spotify'], loggedIn: ['spotify'], rev: 10}, 'd10');
assert.equal(scUiRev, 10);
for (const kind of ['get', 'poll', 'ack', 'login']) {
    for (const shape of ['missing', 'zero', 'bad', 'mismatch']) {
        const beforeSources = scSources.slice(), beforeLogin = scLoggedIn.slice();
        if (kind === 'login') {
            scLoginBegin('spotify');
            const x = loginPosts().at(-1);
            const snap = [scLoginStatus, scLoginSerial];
            const body = {status: 'expired'};
            if (shape === 'zero') body.rev = 0;
            else if (shape === 'bad') body.rev = 'bad';
            else if (shape === 'mismatch') body.rev = 12;
            x.finish(body, '', 200, shape === 'mismatch' ? 11 : 0);
            assert.deepEqual([scLoginStatus, scLoginSerial], snap, 'invalid login rev applied: ' + shape);
            scLogin = '';
            continue;
        }
        let cbCalled = 0;
        if (kind === 'poll') { now += 3000; scCall(''); }
        else if (kind === 'get') scCall('', () => { cbCalled++; });
        else scCall('autoGain=1', () => { cbCalled++; });
        const x = kind === 'poll' ? scUiPoll : reqs.at(-1);
        const body = {scope: 'tidal', sources: ['tidal'], loggedIn: [], autoGain: false};
        if (shape === 'zero') body.rev = 0;
        else if (shape === 'bad') body.rev = 'bad';
        else if (shape === 'mismatch') body.rev = 12;
        x.finish(body, 'd-bad', 200, shape === 'mismatch' ? 11 : 0);
        assert.equal(cbCalled, 0, kind + '/' + shape + ' must not invoke callback');
        assert.deepEqual(scSources, beforeSources, kind + '/' + shape + ' clobbered scSources');
        assert.deepEqual(scLoggedIn, beforeLogin, kind + '/' + shape + ' clobbered scLoggedIn');
        assert.equal(scUiRev, 10);
        if (scUiPoll) scUiPoll.readyState = 4;
    }
}
// FX readiness probe payload {fxSelectorReady: true} stays independent of Services rev.
let fxDone = 0;
scCall('fxUnit=1&fxMode=2', (r) => { if (r && r.fxSelectorReady) fxDone++; });
reqs.at(-1).finish({fxSelectorReady: true, fxBind: true}, '');
assert.equal(fxDone, 1, 'FX readiness probe must stay independent of Services rev');
""")

            with self.subTest(firmware=firmware, case="fix5-parked-tap-authoritative-fields-and-unrelated-503"):
                run_node(harness + """
// 1. Omitted sources in enable ACK must NOT fire early from optimistic local scSources.
for (const signedIn of [false, true]) {
    scUiRev = 0; scUiEpoch = ''; scRetiredEpochs = {};
    scLogin = ''; scScope = 'tidal'; scScopePending = 0; scPendingTap = '';
    const beforeLogins = loginPosts().length, beforeScopes = scopePosts().length;
    scCall('');
    scUiPoll.finish({scope: 'tidal', sources: ['tidal'], loggedIn: [], rev: 10}, 'd10');
    scSetSource('spotify', true);
    scOpen('spotify');
    assert.equal(scPendingTap, 'spotify');
    sourcePosts().at(-1).finish({loggedIn: signedIn ? ['spotify'] : [], rev: 11}, 'd11');
    assert.equal(loginPosts().length, beforeLogins, 'must not open login before sources confirmed');
    assert.equal(scopePosts().length, beforeScopes, 'must not open scope before sources confirmed');
    assert.equal(scPendingTap, 'spotify');
    now += 3000;
    scCall('');
    scUiPoll.finish({scope: 'tidal', sources: ['tidal', 'spotify'], loggedIn: signedIn ? ['spotify'] : [], rev: 12}, 'd12');
    assert.equal(loginPosts().length - beforeLogins, signedIn ? 0 : 1);
    assert.equal(scopePosts().length - beforeScopes, signedIn ? 1 : 0);
    if (signedIn) scopePosts().at(-1).finish({scope: 'spotify', sources: ['tidal', 'spotify'], loggedIn: ['spotify'], rev: 13}, 'd13');
}
// 2. Enable ACK omitting loggedIn + unrelated trailing cueCountdown=0 returning 503 preserves parked tap.
for (const signedIn of [false, true]) {
    scUiRev = 0; scUiEpoch = ''; scRetiredEpochs = {};
    scLogin = ''; scScope = 'tidal'; scScopePending = 0; scPendingTap = '';
    const beforeLogins = loginPosts().length, beforeScopes = scopePosts().length;
    scCall('');
    scUiPoll.finish({scope: 'tidal', sources: ['tidal'], loggedIn: [], rev: 10}, 'd10');
    scSetSource('spotify', true);
    scCall('cueCountdown=0');
    scOpen('spotify');
    sourcePosts().at(-1).finish({sources: ['tidal', 'spotify'], rev: 11}, 'd11');
    assert.equal(scPendingTap, 'spotify');
    const tail = reqs.findLast(r => r.method === 'POST' && r.url.endsWith('cueCountdown=0'));
    tail.finish(null, '', 503);
    assert.equal(scPendingTap, 'spotify', 'unrelated trailing 503 must not clear accepted parked tap');
    // Recovery poll completes both fields and fires the parked tap.
    const rec = reqs.at(-1);
    assert.equal(rec.method, 'GET');
    rec.finish({scope: 'tidal', sources: ['tidal', 'spotify'], loggedIn: signedIn ? ['spotify'] : [], rev: 13}, 'd13');
    assert.equal(loginPosts().length - beforeLogins, signedIn ? 0 : 1);
    assert.equal(scopePosts().length - beforeScopes, signedIn ? 1 : 0);
    if (signedIn) scopePosts().at(-1).finish({scope: 'spotify', sources: ['tidal', 'spotify'], loggedIn: ['spotify'], rev: 14}, 'd14');
}
""")

    @unittest.skipUnless(shutil.which("node"), "Node is required")
    def test_r16_full_services_matrix_274_plus_epoch_cases_both_firmwares(self):
        """Permanent 274-case R15 matrix (178 R11-R14 + 96 adversarial) + epoch/header cases across 5.0.4 and 5.3.0."""
        from itertools import product

        names = (
            "scCall scUiFlush scUiPollReply scUiSyncPending scApplyQuery scSameItems "
            "scApply scOpen scSetSource scLoginBegin scLoginRequest scLoginDone "
            "scUiReplyRev scSourceWritePending scSyncLoginDialog"
        ).split()

        fixture_js = r"""
Object.assign(root,{scUpdateRepo:'',scIsPlayerBoard:true,scConsoleLineBoard:false,scUpfaderEcho:false,scHasMixer:false,scHasStemsEq:false,scPendingTimeZone:'',scLoginSerial:0,scLoginStatus:'',scLoginLocal:false,scWebdav:[],scNasServers:[]});
Planck.getProperty=()=>({});
function scKeepPlayhead(){} function scProbeCueClocks(){}
function scSelectTidal(){return true;} function scTidalProvider(){return null;} function scGatewayLogin(){}
var scSelectPending=false,scTidalLoginPending=false,scTidalLogoutPending=false,scAskedAt=0;
XMLHttpRequest.prototype.getResponseHeader=function(k){
 if(k==='X-UI-Rev')return String(this.wireRev||'');
 if(k==='X-UI-Epoch')return String(this.wireEpoch||'');
 return k==='X-UI-Digest'?(this.digest||''):'';
};
const inp=['tidal','spotify'], basic={scope:'tidal',sources:inp,loggedIn:['spotify']};
const out={...basic,loggedIn:[]}, off={...out,sources:['tidal']};
scSources=inp.slice(); scScope='tidal'; scLoggedIn=[];
const posted=(key)=>reqs.filter(r=>r.method==='POST' && r.url.split('?')[1]===key);
const scopePosts=id=>posted('scope='+id);
const loginPosts=id=>reqs.filter(r=>r.method==='POST' && r.url.endsWith('/login?provider='+id));
const writes=()=>reqs.filter(r=>r.method==='POST' && /[?](?:source_|sources=)/.test(r.url));
const ob=()=>[loginPosts('spotify').length,scopePosts('spotify').length];
let opens=0,begins=0,ticks=0,applies=0;
const realOpen=scOpen,realBegin=scLoginBegin,realApply=scApply;
scOpen=function(...a){opens++;return realOpen(...a);};
scLoginBegin=function(...a){begins++;return realBegin(...a);};
scApply=function(...a){applies++;return realApply(...a);};
scEnterTimer.start=()=>{ticks++;};
function counts(){return [opens,begins,ticks];}
function finish(x,body,rev,status=200,digest='d'+rev,epoch='',hdrEpoch=null,hdrRev=null) {
 assert.ok(x&&x.readyState===1,'expected a real outstanding request');
 x.wireRev=hdrRev!==null?hdrRev:rev;
 x.wireEpoch=hdrEpoch!==null?hdrEpoch:epoch;
 x.status=status;x.readyState=4;x.digest=digest;
 const payload=(body && typeof body==='object')?{...body,...(rev>0?{rev}:{}),...(epoch?{epoch}:{})}:body;
 x.responseText=typeof payload==='string'?payload:JSON.stringify(payload);
 if(x.onreadystatechange)x.onreadystatechange();
}
function read(path){
 if(path==='poll'){now+=3000;scCall('');return scUiPoll;}
 if(path==='get')scCall('',()=>{});else scCall('autoGain=1');
 return reqs.at(-1);
}
function init(body=out,epoch=''){finish(read('poll'),body,10,200,'d10',epoch);}
function uintap(id='spotify'){tap({id});}
function scan(){uintap();finish(loginPosts('spotify').at(-1),{status:'waiting',target:'/m/spotify'},10);}
function qr(){
 const n=reqs.length;scLoginDone();return reqs.length>n?reqs.at(-1):null;
}
function flushPoll(body,rev,epoch=''){const x=read('poll');if(x&&x.readyState===1)finish(x,body,rev,200,'d'+rev,epoch);}
function loginShape(v,shape){
 const b={...v}; if(shape==='omit'||shape==='no-login'||shape==='neither')delete b.loggedIn;
 if(shape==='bad'||shape==='bad-login')b.loggedIn={};
 if(shape==='no-sources'||shape==='neither')delete b.sources;
 if(shape==='bad-sources')b.sources={};
 return b;
}
function sourceShape(v,shape){const x={...v};if(shape==='disagree')x.sources=['tidal'];if(shape==='bad')x.sources={};if(shape==='omit')delete x.sources;return x;}
function finishScope(id,body=basic,rev=15){
 const x=scopePosts(id).findLast(r=>r.readyState===1);if(x)finish(x,{...body,scope:id},rev);
}
function afterScan(q,b,rev,code=200){if(q&&q.readyState===1)finish(q,b,rev,code);}
function enableFlow(when,tail=false,last='spotify',again=false,repeat=false){
 init(basic);scSetSource('spotify',false);scCall('autoGain=1');
 finish(writes()[0],off,11);
 scSetSource('spotify',true);if(tail)scCall('cueCountdown=0');
 uintap();if(last!=='spotify'||repeat)uintap(last);
 assert.deepEqual(ob(),[0,0],'action before enable');
 finish(posted('autoGain=1')[0],off,12);
 const end=when==='in'?basic:when==='reject'?off:out;
 finish(writes()[1],when==='reject'?null:end,13,when==='reject'?503:200);
 const desired=[Number(last==='spotify'&&when==='out'),Number(last==='spotify'&&when==='in')];
 assert.deepEqual(ob(),desired,'action at enable');
 if(tail){if(again)uintap();const t=posted('cueCountdown=0')[0];finish(t,end,14);}
 for(const x of reqs.slice())if(x.method==='GET'&&x.readyState===1&&x.url.endsWith('/ui'))finish(x,end,14);
 if(!again)assert.deepEqual(ob(),desired);
}
"""
        cases_js = r"""
function exercise(group,args){
 const [a,b,c]=args;
 const checkPassive=before=>assert.deepEqual(counts(),before,'passive scan or sign-in initiated an action');
 if(group==='R11.queue'){
  init(out);scan();flushPoll(off,11);const hold=read('ack');scSetSource('spotify',true);if(a)scCall('cueCountdown=0');
  const before=counts();const q=qr();finish(hold,off,11);finish(writes()[0],out,12);
  afterScan(q,b===200?basic:null,10,b);if(a)finish(posted('cueCountdown=0')[0],out,13);
  flushPoll(basic,14);checkPassive(before);uintap();assert.equal(scopePosts('spotify').length,1);return;
 }
 if(group==='R11.middle'||group==='R11.direct'){
  const incoming=group==='R11.middle'&&a?basic:out;
  init(group==='R11.middle'?out:basic);
  const hold=read('ack');scCall('cueCountdown=0');finish(hold,incoming,11);
  if(group==='R11.middle'||a==='middle')uintap();
  finish(posted('cueCountdown=0')[0],incoming,12);
  if(group==='R11.direct'&&a==='final')uintap();
  assert.deepEqual(ob(),incoming===basic?[0,1]:[1,0]);return;
 }
 if(group==='R11.fresh'||group==='R12.partial'||group==='R12.repair'||group==='R12.older'){
  init(out);scan();
  if(group==='R12.repair')flushPoll({...out,loggedIn:{}},11);
  const ordinary=read('ack');
  if(group==='R12.partial'){scCall('cueCountdown=0');scCall('masterLimiter=1');}
  if(group==='R12.older')scCall('cueCountdown=0');
  const before=counts(),q=qr();
  let fresh={...basic};if(group==='R12.repair'){fresh=loginShape(fresh,a);if(b==='omit')delete fresh.sources;}
  if(group!=='R12.older'||a)afterScan(q,fresh,14);
  let old={...out,autoGain:true};if(group==='R12.older'){if(b==='in')old.loggedIn=['spotify'];if(b==='omit')delete old.loggedIn;}
  finish(ordinary,old,12);
  if(group==='R12.older'&&!a)afterScan(q,fresh,14);
  if(group==='R12.partial'){
   finish(posted('cueCountdown=0')[0],a==='fx'?{fxSelectorReady:true}:loginShape(out,a),13);
   finish(posted('masterLimiter=1')[0],basic,15);
  }
  if(group==='R12.older')finish(posted('cueCountdown=0')[0],basic,15);
  flushPoll(basic,16);checkPassive(before);uintap();assert.equal(scopePosts('spotify').length,1);return;
 }
 if(group==='R12.footer'){enableFlow(a);return;}
 if(group==='R13.replace'){enableFlow(b,false,a,false,true);return;}
 if(group==='R13.boundary'){enableFlow(a?'out':'in',true,'spotify',b);return;}
 if(group==='R12.retry'||group==='R13.retry'){
  init({...basic,scope:'spotify'});uintap('tidal');uintap('spotify');
  if(b==='tidal')uintap('tidal');
  finish(scopePosts('tidal')[0],loginShape({...basic,scope:'tidal'},a),11);
  if(b!=='tidal')finishScope('spotify',basic,12);
  assert.deepEqual(ob(),[0,Number(b!=='tidal')]);assert.equal(scScope,b==='tidal'?'tidal':'spotify');return;
 }
 if(group==='R13.order'){
  init(out);scan();const isWrite=a==='middle'||a==='final';
  const old=read(isWrite?'ack':a);if(a==='middle')scCall('cueCountdown=0');
  const before=counts(),q=qr();
  if(q&&q!==old)afterScan(q,basic,13);
  if(old.readyState===1)finish(old,sourceShape(out,b),11);
  if(a==='middle')finish(posted('cueCountdown=0')[0],basic,14);
  flushPoll(basic,15);checkPassive(before);uintap();assert.deepEqual(ob(),[1,1]);
  assert.ok(scSources.includes('spotify')&&scLoggedIn.includes('spotify'));return;
 }
 if(group==='R14.own'){
  init(out);scLoginBegin('spotify');finish(loginPosts('spotify')[0],{status:'waiting'},10);
  scSetSource('deezer',true);const own=writes()[0];if(a)scCall('cueCountdown=0');
  const q=qr(),end={...basic,sources:[...inp,'deezer']},snap=b==='after'?end:{...basic};
  if(b==='omit')delete snap.sources;
  afterScan(q,snap,b==='after'?12:11);
  finish(own,end,12);assert.ok(scSources.includes('deezer'));uintap('deezer');
  assert.equal(loginPosts('deezer').length,1);assert.equal(own.url.split('?')[1],'source_deezer=1');return;
 }
 if(group==='R14.peer'){
  const plus=[...inp,'deezer'],oldState={...basic,sources:a?inp:plus},fresh={...basic,sources:a?plus:inp};
  init(oldState);scLoginBegin('spotify');finish(loginPosts('spotify')[0],{status:'waiting'},10);
  const old=read(b==='final'?'ack':b),q=qr();if(q&&q!==old)afterScan(q,fresh,12);
  if(old.readyState===1)finish(old,c?fresh:oldState,c?12:10);
  scSetSource('soundcloud',true);const query=writes().at(-1).url.split('?')[1];
  assert.equal(query,'source_soundcloud=1','unrelated source is sent on the wire');
  let persisted=new Set(fresh.sources);persisted.add('soundcloud');
  finish(writes().at(-1),{...fresh,sources:[...persisted]},13);
  assert.equal(scSources.includes('deezer'),a);assert.ok(scSources.includes('soundcloud'));return;
 }
 if(group==='R14.passive'){
  init(out);scan();scSetSource('deezer',true);scSetSource('soundcloud',true);scCall('cueCountdown=0');
  const before=counts(),q=qr();afterScan(q,basic,11);checkPassive(before);
  const first={...basic,sources:[...inp,'deezer']};
  finish(writes()[0],first,12);
  const state=loginShape({...first,sources:[...first.sources,'soundcloud']},a);
  if(a==='out')state.loggedIn=[];
  finish(writes()[1],state,13);finish(posted('cueCountdown=0')[0],state,14);checkPassive(before);
  flushPoll(a==='out'?out:basic,15);
  if(b==='tap'){uintap();assert.deepEqual(ob(),a==='out'?[2,0]:[1,1]);}
  else checkPassive(before);return;
 }
 if(group==='A.login'){
  init(out);scLoginBegin('spotify');const old=loginPosts('spotify')[0];
  let x=old,calls=0;
  if(a==='GET'){finish(old,{status:'waiting'},10);scLoginRequest('GET','spotify',()=>{calls++;});x=reqs.at(-1);}
  flushPoll(out,20);
  const snapshot=[scLoginStatus,scLoginSerial,scLoginLocal,scLogin,scUiRev];
  finish(x,{status:b,target:'/m/spotify'},11);
  assert.equal(calls,0);assert.deepEqual([scLoginStatus,scLoginSerial,scLoginLocal,scLogin,scUiRev],snapshot,'lower login reply changed current dialog');return;
 }
 if(group==='A.missing'){
  init(basic);
  if(a==='login'){
   scLoginBegin('spotify');const x=loginPosts('spotify')[0],before=[scLoginStatus,scLoginSerial];
   finish(x,{status:'expired',...(b==='missing'?{}:{rev:b==='zero'?0:'bad'})},0);
   assert.deepEqual([scLoginStatus,scLoginSerial],before,'unversioned login reply applied');return;
  }
  const x=read(a),v={...off,autoGain:false};if(b!=='missing')v.rev=b==='zero'?0:'bad';
  finish(x,v,0);assert.ok(scSources.includes('spotify')&&scLoggedIn.includes('spotify'),'unversioned state replaced known revision');return;
 }
 if(group==='A.failure'){
  init(off);
  const head=read('ack');scSetSource('spotify',true);scCall('cueCountdown=0');uintap();assert.deepEqual(ob(),[0,0]);
  const reject=x=>finish(x,a===200?'broken body':null,11,a);
  if(b==='head')reject(head);else finish(head,off,11);
  const enable=writes()[0];
  if(b==='enable')reject(enable);else finish(enable,out,12);
  const tail=posted('cueCountdown=0')[0];
  if(b==='tail')reject(tail);else finish(tail,b==='enable'?off:out,13);
  for(const x of reqs.slice())if(x.method==='GET'&&x.readyState===1)finish(x,b==='enable'?off:out,14);
  assert.deepEqual(ob(),b==='enable'?[0,0]:[1,0]);return;
 }
 if(group==='A.digest'){
  init(basic);let old=applies;for(let i=0;i<3;i++){const x=read('poll');finish(x,basic,10,200,'d10');}
  assert.equal(applies,old);
  const x=read(a?'get':'poll');finish(x,out,9,200,'older');assert.ok(scLoggedIn.includes('spotify'));return;
 }
 if(group==='A.scope'){
  init(basic);uintap();const old=scopePosts('spotify')[0];
  if(a==='get')finish(read('get'),{...basic,scope:'spotify'},20);
  else flushPoll({...basic,scope:'spotify'},20);
  const before=counts();finish(old,{...basic,scope:'spotify'},11);
  assert.deepEqual(counts(),before,'old scope ACK started navigation timer');return;
 }
 if(group==='A.tailLost'){
  init(off);scSetSource('spotify',true);scCall('cueCountdown=0');uintap();
  finish(writes()[0],{sources:inp},11);assert.deepEqual(ob(),[0,0]);
  finish(posted('cueCountdown=0')[0],null,12,503);
  flushPoll(a?basic:out,13);
  assert.deepEqual(ob(),a?[0,1]:[1,0],'healthy recovery lost accepted pending tap');return;
 }
 if(group==='A.omitSource'){
  init(off);scSetSource('spotify',true);uintap();
  finish(writes()[0],{loggedIn:a?['spotify']:[]},11);
  assert.deepEqual(ob(),[0,0],'action before source membership confirmed');
  flushPoll(a?basic:out,12);assert.deepEqual(ob(),a?[0,1]:[1,0]);return;
 }
 if(group==='A.restart'){
  init(basic);for(let i=0;i<3;i++)flushPoll(off,1);
  assert.ok(!scSources.includes('spotify'),'restarted service cannot refresh surviving UI');return;
 }
 if(group==='A.epochRestart'){
  init(basic,'E1');
  const staleInflight=read('get');
  flushPoll(off,a?11:1,'E2');
  assert.equal(scUiEpoch,'E2');
  assert.equal(scUiRev,a?11:1);
  assert.ok(!scSources.includes('spotify'),'epoch restart must refresh surviving UI on first poll');
  finish(staleInflight,basic,99,200,'d99','E1');
  assert.equal(scUiEpoch,'E2');
  assert.ok(!scSources.includes('spotify'),'retired epoch reply must be dropped');
  return;
 }
 if(group==='A.headerMismatch'){
  init(basic,'E1');
  const x=read(a);
  if(b==='rev')finish(x,off,12,200,'d12','E1','E1',11);
  else finish(x,off,12,200,'d12','E1','E2',12);
  assert.ok(scSources.includes('spotify')&&scLoggedIn.includes('spotify'),'header/body mismatch must be rejected');
  return;
 }
 if(group==='A.external'){
  const initial={...out,sources:[...new Set([...inp,a])]};
  init(initial);scLoginBegin(a);finish(loginPosts(a)[0],{status:'waiting'},10);
  const before=counts();flushPoll({...initial,loggedIn:[a],tidalLoggedIn:a==='tidal'},11);
  checkPassive(before);assert.equal(scLogin,'');assert.ok(scLoggedIn.includes(a));return;
 }
 if(group==='A.partialTap'){
  init(off);scSetSource('spotify',true);scCall('cueCountdown=0');uintap();
  finish(writes()[0],loginShape(out,a),11);assert.deepEqual(ob(),[0,0]);
  finish(posted('cueCountdown=0')[0],b?basic:out,12);
  assert.deepEqual(ob(),b?[0,1]:[1,0]);return;
 }
 throw Error('unknown schedule');
}
"""
        dims = lambda *axes: list(product(*axes))
        binary = [False, True]
        sources = ["same", "disagree", "omit", "bad"]
        logins = ["out", "omit", "in"]
        shapes = ["full", "no-login", "no-sources", "neither"]
        groups = {
            "R11.queue": dims(binary, [200, 503]),
            "R11.middle": dims(binary),
            "R11.fresh": [[]],
            "R11.direct": dims(["middle", "final"]),
            "R12.footer": dims(["reject", "in", "out"]),
            "R12.older": dims(binary, logins),
            "R12.retry": dims(shapes),
            "R12.partial": dims(shapes + ["fx"]),
            "R12.repair": dims(["valid", "omit", "bad"], ["valid", "omit"]),
            "R13.order": dims(["middle", "final", "get", "poll"], sources),
            "R13.replace": dims(["spotify", "tidal"], ["out", "in", "reject"]),
            "R13.retry": dims(["no-login", "neither"], ["spotify", "tidal"]),
            "R13.boundary": dims(binary, binary),
            "R14.own": dims(binary, ["before", "after", "omit"]),
            "R14.peer": dims(binary, ["final", "get", "poll"], binary),
            "R14.passive": dims(["out", "in", "omit", "bad"], ["scan", "tap"]),
            "A.login": dims(["POST", "GET"], ["waiting", "expired", "done"]),
            "A.missing": dims(["get", "poll", "ack", "login"], ["missing", "zero", "invalid"]),
            "A.failure": dims([200, 503], ["head", "enable", "tail"]),
            "A.digest": dims(binary),
            "A.scope": dims(["get", "poll"]),
            "A.tailLost": dims(binary),
            "A.omitSource": dims(binary),
            "A.restart": [[]],
            "A.epochRestart": dims(binary),
            "A.headerMismatch": dims(["get", "poll", "ack"], ["rev", "epoch"]),
            "A.external": dims(["deezer", "applemusic", "tidal", "spotify", "netease"]),
            "A.partialTap": dims(["no-login", "no-sources", "neither", "bad-login", "bad-sources"], binary),
        }
        # 89 R11-R14 + 48 R15 adversarial = 137 per firmware (274 across 5.0.4 & 5.3.0),
        # plus 8 epoch/header-mismatch = 145 per firmware (290 across both firmwares).
        total_per_fw = sum(len(v) for v in groups.values())
        self.assertEqual(total_per_fw, 145)

        runner = """
const vm = require('node:vm'), fs = require('node:fs'), p = JSON.parse(fs.readFileSync(0, 'utf8'));
for (const [group, rows] of Object.entries(p.groups)) {
  const code = new vm.Script(p.driver + '\\n' + p.logic + '\\nexercise(group, JSON.parse(args));');
  for (const args of rows) {
    const c = {require, console: {warn(){}, log(){}}, group, args: JSON.stringify(args)};
    try {
      code.runInNewContext(c);
      console.log(JSON.stringify({group, args, ok: true}));
    } catch (e) {
      console.log(JSON.stringify({group, args, ok: false, why: e.message}));
    }
  }
}
"""

        def run_matrix(driver, label):
            payload = {"driver": driver, "logic": cases_js, "groups": groups}
            res = subprocess.run(["node", "-e", runner], input=json.dumps(payload), capture_output=True, text=True, check=True)
            rows = [json.loads(line) for line in res.stdout.splitlines() if line.strip()]
            self.assertEqual(len(rows), total_per_fw, f"{label}: expected {total_per_fw} results, got {len(rows)}")
            bad = [r for r in rows if not r["ok"]]
            self.assertEqual(bad, [], f"{label}: {len(bad)}/{total_per_fw} matrix failures: {bad[:5]}")

        # 1. Always run against build_packs.py source for both 5.0.4 and 5.3.0.
        for fw in ("504", "530"):
            combined = self.bp.SERVICES_MAIN + "\n" + getattr(self.bp, "TIDAL_OPEN_" + fw)
            methods = "\n".join(extract_function(combined, n) for n in names)
            driver = self._round2_js_harness() + "\n" + methods + "\nconst tap = modelData => root.scOpen(modelData.id);\n" + fixture_js
            with self.subTest(source="build_packs", firmware=fw):
                run_matrix(driver, f"build_packs/{fw}")

        # 2. When firmware QRC lab is present, also build real 5.0.4 and 5.3.0 packs and run matrix + shadow check.
        lab = Path(os.environ.get("ENGINEX_LAB", str(LAB)))
        if (lab / "5.0.4" / "qrc").is_dir() and (lab / "5.3.0" / "qrc").is_dir():
            with tempfile.TemporaryDirectory(prefix="e4-matrix-packs-") as td:
                for ver in ("5.0.4", "5.3.0"):
                    out_dir = Path(td) / f"packs-{ver}"
                    cmd = [sys.executable, str(REPO / "tools" / "ui-mod" / "build_packs.py"),
                           "--qrc", str(lab / ver / "qrc"), "--out", str(out_dir)]
                    subprocess.run(cmd, check=True, capture_output=True, text=True)
                    en = out_dir / "en"
                    main_qml = next(en.rglob("EmbeddedMain.qml"))
                    main_text = main_qml.read_text(encoding="utf-8")
                    footer = next(p for p in en.rglob("*.qml") if "onClicked: root.scOpen(modelData.id)" in p.read_text(encoding="utf-8"))
                    tap_expr = re.search(r"onClicked: (root\.scOpen\(modelData\.id\))", footer.read_text(encoding="utf-8")).group(1)
                    methods = "\n".join(extract_function(main_text, n) for n in names)
                    driver = self._round2_js_harness() + "\n" + methods + f"\nconst tap = modelData => {tap_expr};\n" + fixture_js
                    with self.subTest(source="release_pack", firmware=ver):
                        run_matrix(driver, f"release_pack/{ver}")
                    # Shadow clock + revision + hook flags + library view picker check on real built pack.
                    rb_row = next(p for p in en.rglob("*.qml") if "id: scRbLibraryViewEntry" in p.read_text(encoding="utf-8"))
                    section = rb_row.read_text(encoding="utf-8").split("id: scRbLibraryViewEntry", 1)[1]
                    handler = re.search(r"onSelected: (function\(index\) \{[^}]*})", section).group(1)
                    ids = re.search(r"property var ids: (\[[^\n]+\])", section).group(1)
                    shadow_js = (
                        driver + "\n" + extract_function(main_text, "scDeckClockWatch")
                        + """
const probes=[]; let ticksSeen=0;
function scDeckClockDeckProps(d,n){probes.push(d);return d===2;}
function scDeckClockTick(){ticksSeen++;}
Object.assign(root,{scFourDeckRole:'',scDeckClockWatched:true,scFeaturesOff:[],scHookFollows:false});
const live={...basic,featuresOff:['stems'],shadowPausedFollow:true,deckClock:true,rbLibraryView:'folders'};
init(live,'E1');assert.deepEqual([scFeaturesOff,scHookFollows],[['stems'],true]);
scDeckClockWatched=false;flushPoll(live,10,'E1');
assert.deepEqual([probes,ticksSeen],[[1,2,3,4],1]);
const ack=read('ack');finish(ack,{...live,featuresOff:[],shadowPausedFollow:false,autoGain:true},11,200,'d11','E1');
assert.deepEqual([scFeaturesOff,scHookFollows,scUiRev],[[],false,11]);
flushPoll(live,10,'E1');assert.deepEqual([scFeaturesOff,scHookFollows,scUiRev],[[],false,11]);
flushPoll(live,12,'E1');assert.deepEqual([scFeaturesOff,scHookFollows,scUiRev],[['stems'],true,12]);
"""
                        + f"\nconst ids={ids};let at=ids.indexOf(scRbLibraryView);const choose={handler};\n"
                        + """
choose(ids.indexOf('merged'));
const selected=posted('rbLibraryView=merged');assert.equal(selected.length,1);
finish(selected[0],{...live,rbLibraryView:'merged'},13,200,'d13','E1');
assert.equal(scRbLibraryView,'merged');assert.equal(scUiRev,13);
"""
                    )
                    with self.subTest(source="release_pack_shadow", firmware=ver):
                        run_node(shadow_js)


if __name__ == "__main__":
    unittest.main()
