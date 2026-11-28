"""Regression checks for S23 (#336): persisting SearchMenuDialog search-filter fields across restarts."""
import importlib.util
import re
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from legacy_names import LAB as LAB_STR

REPO = Path(__file__).resolve().parents[2]
LAB = Path(LAB_STR)
BUILDER = REPO / "tools" / "ui-mod" / "build_packs.py"

spec = importlib.util.spec_from_file_location("build_packs_s23", BUILDER)
bp = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bp)

EXPECTED_ROLES = [
    "song", "artist", "album", "length", "key", "comment",
    "bpm", "genre", "label", "year", "dateImported", "filename",
]


def _extract_fn(source: str, name: str) -> str:
    m = re.search(rf"(?m)^\tfunction {re.escape(name)}\([^\n]*\) \{{", source)
    if not m:
        raise AssertionError(f"function {name} not found")
    line_end = source.index("\n", m.start())
    if source[m.start():line_end].rstrip().endswith("}"):
        return source[m.start():line_end]
    end = source.index("\n\t}", m.end()) + 3
    return source[m.start():end]


class SearchFieldsPersistenceTest(unittest.TestCase):
    def _build_pack(self, version: str, out: Path) -> Path:
        qrc = LAB / version / "qrc"
        if not qrc.is_dir():
            self.skipTest(f"firmware QRC fixture missing: {qrc}")
        res = subprocess.run(
            ["python3", str(BUILDER), "--qrc", str(qrc), "--out", str(out)],
            capture_output=True, text=True,
        )
        self.assertEqual(res.returncode, 0, f"{version} build failed:\n{res.stderr}\n{res.stdout}")
        return out

    def test_search_qml_and_embedded_main_patched_for_530_and_504_all_languages(self):
        for version, skin in (("5.3.0", "Skins/Embedded"), ("5.0.4", "Skins/EmbeddedCommon")):
            with self.subTest(version=version):
                with tempfile.TemporaryDirectory() as td:
                    out = self._build_pack(version, Path(td) / "packs")
                    for lang in ("en", "zh-Hans", "zh-Hant", "es"):
                        with self.subTest(lang=lang):
                            search_path = out / lang / skin / "Views/BrowseView/Components/Search.qml"
                            main_path = out / lang / skin / "EmbeddedMain.qml"
                            self.assertTrue(search_path.is_file(), f"missing {search_path}")
                            self.assertTrue(main_path.is_file(), f"missing {main_path}")
                            search_qml = search_path.read_text(encoding="utf-8")
                            main_qml = main_path.read_text(encoding="utf-8")

                            # All 12 stock role identifiers remain intact in every language pack.
                            for role in EXPECTED_ROLES:
                                self.assertIn(role, search_qml, f"{version}/{lang} lost role {role}")

                            # Search.qml syncs from Engine without saving on mount, suppresses dialog echo during sync,
                            # saves on user dialog edit, and re-syncs when EmbeddedMain restores or resets search fields.
                            self.assertIn("property bool scSyncingSearchFields: false", search_qml)
                            self.assertIn("if (search.scSyncingSearchFields) return", search_qml)
                            self.assertIn("function scSyncFromEngine()", search_qml)
                            self.assertIn("function onScSearchFieldsVersionChanged()", search_qml)
                            self.assertIn("root.scSaveSearchFields(", search_qml)
                            self.assertIn("search.scSyncFromEngine()", search_qml)
                            comp_tail = search_qml.split("Component.onCompleted:", 1)[1]
                            self.assertNotIn("scSaveSearchFields", comp_tail, "Component.onCompleted must not overwrite saved settings.conf")

                            # EmbeddedMain.qml wires restore, E4 single-flight save queue, passive reset, and offline settings.conf parsing.
                            for needle in (
                                "scSearchFieldOrder",
                                "scSearchFields",
                                "scSearchFieldsRetry",
                                "function scNormalizeSearchFields(",
                                "function scSearchFieldsWritePending(",
                                "function scSyncSearchFields(",
                                "function scSaveSearchFields(",
                                "function scResetSearchFields(",
                                "scSyncSearchFields(v.searchFields)",
                                'else if (key === "search_fields")',
                                "scSyncSearchFields(searchFields)",
                                "scResetAllProp",
                                'scCall("searchFields=" + encodeURIComponent(fields.join(",")))',
                            ):
                                self.assertIn(needle, main_qml, f"{version}/{lang} EmbeddedMain missing {needle}")
                            # S23 R1's separate 3 s overlapping queue must be gone (writes ride E4 scUiFlush).
                            self.assertNotIn("scFlushSearchFields", main_qml)
                            self.assertNotIn("scUiPollSearchSeq", main_qml)
                            # EmbeddedMain must observe ResetAll passively (Connections, not ActionTranslatorStateChangeNotifier).
                            self.assertNotIn(
                                'ActionTranslatorStateChangeNotifier {\n\t\tprop: Planck.getProperty("/Client/Preferences/ResetAll")',
                                main_qml,
                                f"{version}/{lang} EmbeddedMain must not clear /Client/Preferences/ResetAll state before Search.qml",
                            )

    @unittest.skipUnless(shutil.which("node"), "node required for QML JS behavior checks")
    def test_search_fields_state_machine_js(self):
        fns = "\n".join(
            _extract_fn(bp.SERVICES_MAIN, name)
            for name in (
                "scSameItems",
                "scNormalizeSearchFields",
                "scSearchFieldsWritePending",
                "scSyncSearchFields",
                "scSaveSearchFields",
                "scResetSearchFields",
                "scParseSettingsConf",
                "scUiReplyRev",
                "scSourceWritePending",
                "scSyncLoginDialog",
                "scUiSyncPending",
                "scApplyQuery",
                "scUiFlush",
                "scCall",
                "scUiPollReply",
            )
        )
        script = r"""
const assert = require('node:assert/strict');

let nowMs = 10000;
Date.now = () => nowMs;

let engineRoles = ["song", "artist"];
let searchAll = false;
const setCalls = [];
let resetCalls = 0, reads = 0, notifications = 0, normalizeCalls = 0;
const posted = [];
const inflight = [];

const Planck = {
    getProperty(path) {
        if (path === "/Client/Librarian/SearchAll") {
            return { translator: { get state() { return searchAll; }, set state(v) { searchAll = v; } } };
        }
        return null;
    },
    librarianController: {
        searchConstraintRolesList() { reads++; return engineRoles.slice(); },
        setSearchConstraint(roles) {
            setCalls.push(roles.slice());
            engineRoles = roles.slice();
        },
        resetSearchConstraintsToDefaults() {
            resetCalls++;
            engineRoles = ["song", "artist"];
        },
    },
};

const scSearchFieldOrder = ["song", "artist", "album", "length", "key", "comment", "bpm", "genre", "label", "year", "dateImported", "filename"];
let scSearchFields = ["song", "artist"];
let scSearchFieldsRestored = false;
let scSearchFieldsRetry = false;
let scSearchFieldsRetryAt = 0;
let scSearchFieldsVersion = 0;

// E4 + EmbeddedMain state variables
let scUiPoll = null, scScopePending = 0, scScope = "", scUiPollScope = "", scUiPollAt = 0, scUiDigest = "", scUiAppliedAt = 0, scUiLoaded = true;
let scLoggedIn = [], scSources = [], scLoginOpen = "", scLogin = "";
let scCueCountdown = true, scBpmDecimals = 2, scTracklistWaveform = true;
let scClientId = "qml-test-s23", scUiWriteSeq = 0, scUiAppliedSeq = 0;
let scUiWriteGen = 0, scUiPollGen = 0, scUiPending = 0, scUiQueue = [];
let scUiSending = false, scUiSendingAt = 0, scUiSendToken = 0, scUiSendXhr = null, scUiHadError = false;
let scTunnelPending = 0, scTunnelSending = false, scTunnelSendingAt = 0;
let serverEpoch = "ep-1", serverRev = 1;

function scFxMsInteger() { return 500; }
function scFxMsApply() {}
function scEnsureTidal() {}
function scOpen() {}
function scApply(v) {
    if (v && v.searchFields !== undefined) scSyncSearchFields(v.searchFields);
}
const root = {
    scIsPlayerBoard: false,
    scReverseJogLeft: false, scReverseJogRight: false,
    scSdHighSpeed: false, scHifiOutput: false, scBeatJumpPads: false,
    scRestoreBpmOnUnsync: false, scDualColumn: false, scTracklistWaveform: true,
    scCueCountdown: true, scBpmDecimals: 2, scSyncGuardPct: 0, scSwapLoopOrder: false,
    scLedBrightness: 100, scUIScale: 100, scStemsEqMode: false, scPitchBendBeatJump: false,
    scMixstreamFx: [], scPioneerFx: "off", scRbLibraryView: "folders",
    scUiRev: 0, scUiEpoch: "", scRetiredEpochs: {},
    scUiLowPollCount: 0, scUiLowPollLast: 0,
    scUiSendingQuery: "", scPendingTap: "", scScopeNeedRefresh: false,
    scRestartPending: false, scBtSending: false,
    get scSearchFields() { return scSearchFields; },
    set scSearchFields(v) { scSearchFields = v; },
};

let autoReply = false;
class XMLHttpRequest {
    constructor() { this.readyState = 0; this.status = 0; this.responseText = ""; this.headers = {}; this.reqHeaders = {}; this.aborted = false; }
    open(method, url) { this.method = method; this.url = url; this.readyState = 1; this.reqHeaders = {}; this.aborted = false; }
    setRequestHeader(k, v) { this.reqHeaders[k] = String(v); }
    abort() { this.aborted = true; this.readyState = 0; }
    getResponseHeader(k) { return this.headers[k] || ""; }
    send() {
        const q = this.url.indexOf("?") >= 0 ? this.url.split("?")[1] : "";
        if (this.method === "POST") {
            posted.push({ query: q, clientId: this.reqHeaders["X-UI-Client-Id"] || "", writeSeq: Number(this.reqHeaders["X-UI-Write-Seq"] || 0) });
        }
        const req = {
            xhr: this,
            method: this.method,
            query: q,
            reqHeaders: Object.assign({}, this.reqHeaders),
            reply(obj, status = 200, digest = "", opts = {}) {
                const payload = Object.assign({}, obj);
                const ep = opts.epoch !== undefined ? opts.epoch : serverEpoch;
                let rv = opts.rev;
                if (rv === undefined) {
                    rv = this.method === "POST" && status === 200 ? ++serverRev : serverRev;
                }
                if (status === 200 && opts.omitMeta !== true) {
                    if (payload.rev === undefined) payload.rev = rv;
                    if (payload.epoch === undefined && ep) payload.epoch = ep;
                    if (payload.sources === undefined) payload.sources = scSources.slice();
                    if (payload.loggedIn === undefined) payload.loggedIn = scLoggedIn.slice();
                }
                this.xhr.status = status;
                this.xhr.responseText = JSON.stringify(payload);
                const hdrs = {};
                if (digest) hdrs["X-UI-Digest"] = digest;
                if (opts.omitMeta !== true) {
                    if (payload.rev !== undefined) hdrs["X-UI-Rev"] = String(payload.rev);
                    if (payload.epoch) hdrs["X-UI-Epoch"] = String(payload.epoch);
                }
                this.xhr.headers = hdrs;
                this.xhr.readyState = 4;
                if (this.xhr.onreadystatechange) this.xhr.onreadystatechange();
            },
        };
        if (autoReply && q.startsWith("searchFields=")) {
            const parsed = decodeURIComponent(q.split("=")[1] || "").split(",").filter(Boolean);
            req.reply({ searchFields: parsed });
        } else {
            inflight.push(req);
        }
    }
}

""" + fns + r"""

const _origNormalize = scNormalizeSearchFields;
scNormalizeSearchFields = function(raw) {
    normalizeCalls++;
    return _origNormalize(raw);
};

// 1. Default boot + FIX 3 cost check: first default restore reads Engine once (0 setters),
// and 10 subsequent unchanged full applies perform 0 normalizations, 0 Engine reads, and 0 array replacements.
scSyncSearchFields(["song", "artist"]);
assert.equal(scSearchFieldsRestored, true);
assert.equal(reads, 1, "default boot should read Engine roles exactly once");
assert.equal(setCalls.length, 0, "default boot should not call setSearchConstraint");
assert.equal(scSearchFieldsVersion, 0);
assert.equal(normalizeCalls, 0, "canonical default boot should short-circuit normalization");
for (let i = 0; i < 10; i++) {
    const before = scSearchFields;
    scSyncSearchFields(["song", "artist"]);
    if (scSearchFields !== before) notifications++;
}
assert.equal(reads, 1, "unchanged full applies must not re-read Engine");
assert.equal(setCalls.length, 0, "unchanged full applies must not call setSearchConstraint");
assert.equal(notifications, 0, "10 unchanged full applies must produce 0 new array identities");
assert.equal(normalizeCalls, 0, "10 unchanged full applies must short-circuit before normalization");
assert.equal(searchAll, false);

// 2. Fresh reboot with saved custom fields: applies once, bumps version, never touches SearchAll;
// 10 subsequent unchanged custom applies also produce 0 array replacements and 0 normalizations.
scSearchFieldsRestored = false;
searchAll = true;
scSyncSearchFields(["bpm", "artist", "song", "comment", "key"]);
assert.deepEqual(engineRoles, ["song", "artist", "key", "comment", "bpm"]);
assert.equal(setCalls.length, 1);
assert.equal(scSearchFieldsVersion, 1);
assert.equal(searchAll, true, "SearchAll must remain owned by stock PlanckClientState.json");
const customNormBefore = normalizeCalls;
const customReadsBefore = reads;
let customNotifications = 0;
for (let i = 0; i < 10; i++) {
    const before = scSearchFields;
    scSyncSearchFields(["song", "artist", "key", "comment", "bpm"]);
    if (scSearchFields !== before) customNotifications++;
}
assert.equal(customNotifications, 0, "unchanged custom applies must produce 0 array replacements");
assert.equal(normalizeCalls, customNormBefore, "unchanged custom applies must skip normalization");
assert.equal(reads, customReadsBefore, "unchanged custom applies must not re-read Engine");

// 3. Rapid checkbox burst: single-flight through E4 scUiFlush, coalescing intermediate taps,
// carrying monotonic X-UI-Client-Id / X-UI-Write-Seq, and never letting interleaved POSTs revert Engine.
engineRoles = ["song", "artist", "bpm"];
scSaveSearchFields(["song", "artist", "bpm"]);
assert.equal(inflight.length, 1);
const firstSfReq = inflight.shift();
assert.equal(firstSfReq.query, "searchFields=song%2Cartist%2Cbpm");
assert.equal(firstSfReq.reqHeaders["X-UI-Client-Id"], scClientId);
assert.equal(Number(firstSfReq.reqHeaders["X-UI-Write-Seq"]), 1);
assert.equal(scUiPending, 1);

// Tap 2 and Tap 3 while first POST is still in flight -> coalesced in scUiQueue, still 1 POST in flight.
engineRoles = ["song", "artist", "key", "bpm"];
scSaveSearchFields(["song", "artist", "key", "bpm"]);
engineRoles = ["song", "artist", "key", "comment", "bpm"];
scSaveSearchFields(["song", "artist", "key", "comment", "bpm"]);
assert.equal(inflight.length, 0, "must keep at most one searchFields POST in flight during rapid burst");
assert.equal(scUiQueue.length, 1, "consecutive searchFields taps must coalesce in scUiQueue");

// First POST completes -> intermediate reply must NOT revert Engine, must flush coalesced final set with seq=2.
firstSfReq.reply({ searchFields: ["song", "artist", "bpm"] });
assert.deepEqual(engineRoles, ["song", "artist", "key", "comment", "bpm"], "intermediate reply must not clobber newer tap");
assert.equal(inflight.length, 1, "coalesced final selection must flush right after first POST");
const finalSfReq = inflight.shift();
assert.equal(finalSfReq.query, "searchFields=song%2Cartist%2Ckey%2Ccomment%2Cbpm");
assert.equal(Number(finalSfReq.reqHeaders["X-UI-Write-Seq"]), 2);

// Interleaved unrelated playhead POST queues cleanly behind finalSfReq via E4 FIFO queue.
scCall("playhead=0.42");
assert.equal(inflight.length, 0, "playhead POST must serialize behind active searchFields POST");
finalSfReq.reply({ searchFields: ["song", "artist", "key", "comment", "bpm"] });
const playheadReq = inflight.shift();
assert.equal(playheadReq.query, "playhead=0.42");
assert.equal(Number(playheadReq.reqHeaders["X-UI-Write-Seq"]), 3);
playheadReq.reply({ playhead: 0.42, searchFields: ["song", "artist", "key", "comment", "bpm"] });
assert.equal(scSearchFieldsRetry, false);
assert.equal(scUiPending, 0);
assert.deepEqual(engineRoles, ["song", "artist", "key", "comment", "bpm"]);

// 4. Stale GET /v1/enginex/ui poll started BEFORE a user tap must not revert Engine after save completes.
const revBeforeTap = serverRev;
scCall();
const stalePoll = inflight.shift();
engineRoles = ["genre", "label"];
scSaveSearchFields(["genre", "label"]);
const genreReq = inflight.shift();
genreReq.reply({ searchFields: ["genre", "label"] }, 200, "d-genre");
assert.equal(scSearchFieldsRetry, false);
stalePoll.reply({ searchFields: ["song", "artist", "key", "comment", "bpm"] }, 200, "old-digest", { rev: revBeforeTap });
assert.deepEqual(engineRoles, ["genre", "label"], "stale pre-tap GET poll must not revert Engine");
assert.equal(scUiDigest, "d-genre", "stale poll must not overwrite current digest cache");

// 5. Failed POST retry: 500 leaves scSearchFieldsRetry=true; immediate E4 recovery GET does not revert
// or cache digest, and next poll after >3 s retries POST with monotonic write-seq without reverting Engine.
engineRoles = ["album", "year"];
scSaveSearchFields(["album", "year"]);
const failedReq = inflight.shift();
const failedSeq = Number(failedReq.reqHeaders["X-UI-Write-Seq"]);
failedReq.reply({}, 500, "", { omitMeta: true });
assert.equal(scSearchFieldsRetry, true);
assert.equal(inflight.length, 1, "E4 triggers immediate recovery GET after 500");
const recoveryGet = inflight.shift();
nowMs += 500; // within 3 s retry backoff
recoveryGet.reply({ searchFields: ["genre", "label"] }, 200, "d-before-retry");
assert.deepEqual(engineRoles, ["album", "year"], "recovery GET must not revert unacknowledged user edit");
assert.equal(scUiDigest, "", "recovery GET during pending retry must not cache digest");
assert.equal(inflight.length, 0);
nowMs += 3500; // past 3 s retry window
scCall();
const retryPoll = inflight.shift();
retryPoll.reply({ searchFields: ["genre", "label"] }, 200, "d-before-retry");
assert.equal(inflight.length, 1, "poll past 3 s backoff must enqueue retry POST");
const retryReq = inflight.shift();
assert.equal(retryReq.query, "searchFields=album%2Cyear");
assert.ok(Number(retryReq.reqHeaders["X-UI-Write-Seq"]) > failedSeq, "retry POST must carry higher write-seq");
retryReq.reply({ searchFields: ["album", "year"] }, 200, "d-album-year");
assert.equal(scSearchFieldsRetry, false);
assert.deepEqual(engineRoles, ["album", "year"]);

// 6. External API / Settings update while clean applies to Engine and bumps version.
const verBefore = scSearchFieldsVersion;
scSyncSearchFields(["year", "filename"]);
assert.deepEqual(engineRoles, ["year", "filename"]);
assert.equal(scSearchFieldsVersion, verBefore + 1);

// 7. Reset All restores stock defaults and persists default clear.
autoReply = true;
scResetSearchFields();
assert.equal(resetCalls, 1);
assert.deepEqual(engineRoles, ["song", "artist"]);
assert.equal(posted[posted.length - 1].query, "searchFields=song%2Cartist");
assert.equal(scSearchFieldsRetry, false);

// 8. Offline settings.conf parsing applies search_fields= in canonical order.
scSearchFieldsRestored = false;
engineRoles = ["song", "artist"];
scParseSettingsConf("lang=en\nsearch_fields=filename,bpm,song\n");
assert.deepEqual(engineRoles, ["song", "bpm", "filename"]);
assert.deepEqual(scSearchFields, ["song", "bpm", "filename"]);

// 9. Permanent S23 R1 BLOCK 1 + FIX 2 reproducer (forum336-review-S23.md:68-104):
// (a) Delayed GET started while save is >3 s pending never reverts acknowledged tap (neither poll nor explicit GET).
// (b) Tapping BPM after Album has been pending 3.1 s NEVER spawns a second overlapping POST.
const failures = [];
function observe(label, actual, expected) {
    if (JSON.stringify(actual) !== JSON.stringify(expected))
        failures.push(label + ": got " + JSON.stringify(actual) + ", expected " + JSON.stringify(expected));
}
autoReply = false;
const prior = engineRoles.slice();
const snapRev = serverRev;
engineRoles = ["genre", "label"];
scSaveSearchFields(engineRoles);
const save = inflight.shift();
nowMs += 3100;
scCall(); // periodic poll blocked while write is pending
const delayedPoll = inflight.shift();
observe("no overlapping poll while save pending", delayedPoll ? 1 : 0, 0);
// Also exercise an explicit GET started during the pending save (e.g. scCall("", cb)) taking a pre-save snapshot.
scCall("", function() {});
const delayedExplicitGet = inflight.shift();
assert.ok(delayedExplicitGet, "explicit GET should exist to prove startedDuringWrite guard");
save.reply({ searchFields: ["genre", "label"] }, 200, "d-genre-2");
delayedExplicitGet.reply({ searchFields: prior }, 200, "old-snapshot", { rev: snapRev });
observe("late GET after acknowledged tap", engineRoles, ["genre", "label"]);
observe("digest preserved after late GET", scUiDigest, "d-genre-2");

engineRoles = ["album"];
scSaveSearchFields(engineRoles);
const older = inflight.shift();
nowMs += 3100;
engineRoles = ["album", "bpm"];
scSaveSearchFields(engineRoles);
const newer = inflight.shift();
observe("unanswered POSTs after 3.1s timeout", newer ? 2 : 1, 1);
if (newer) {
    newer.reply({ searchFields: ["album", "bpm"] });
    older.reply({ searchFields: ["album"] });
    scCall();
    inflight.shift().reply({ searchFields: ["album"] }, 200, "last-store");
    observe("roles after reversed POST handling", engineRoles, ["album", "bpm"]);
} else {
    older.reply({ searchFields: ["album"] });
    const flushed = inflight.shift();
    assert.ok(flushed, "coalesced album,bpm must flush after older completes");
    assert.ok(Number(flushed.reqHeaders["X-UI-Write-Seq"]) > Number(older.reqHeaders["X-UI-Write-Seq"]));
    flushed.reply({ searchFields: ["album", "bpm"] });
    observe("roles after single-flight flush", engineRoles, ["album", "bpm"]);
}

// 10. E4 (uiEpoch, uiRev) gating & server restart recovery for searchFields:
// (a) Same-epoch lower-rev poll cannot revert acknowledged searchFields.
const ackRev = root.scUiRev;
scCall();
const lowerRevPoll = inflight.shift();
lowerRevPoll.reply({ searchFields: ["song", "artist"] }, 200, "d-stale-rev", { rev: ackRev - 1, epoch: serverEpoch });
observe("lower-rev same-epoch poll dropped", engineRoles, ["album", "bpm"]);

// (b) Server restarts with new epoch & rev=1 (< ackRev): QML recovers immediately and retires old epoch.
const oldEpoch = serverEpoch;
serverEpoch = "ep-2";
serverRev = 1;
scCall();
const postRestartPoll = inflight.shift();
postRestartPoll.reply({ searchFields: ["album", "bpm"] }, 200, "d-ep2-r1", { rev: 1, epoch: "ep-2" });
observe("epoch updated after server restart", root.scUiEpoch, "ep-2");
observe("rev reset to new epoch rev", root.scUiRev, 1);
observe("old epoch retired", !!root.scRetiredEpochs[oldEpoch], true);

// (c) Late pre-restart poll from retired epoch carrying higher rev is dropped.
scCall();
const retiredPoll = inflight.shift();
retiredPoll.reply({ searchFields: ["song", "artist"] }, 200, "d-retired", { rev: ackRev + 50, epoch: oldEpoch });
observe("retired-epoch high-rev poll dropped", engineRoles, ["album", "bpm"]);

// (d) Extreme >20s stuck POST abort: aborts old XHR and issues monotonic higher X-UI-Write-Seq.
engineRoles = ["key", "bpm"];
scSaveSearchFields(["key", "bpm"]);
const hung = inflight.shift();
const hungSeq = Number(hung.reqHeaders["X-UI-Write-Seq"]);
nowMs += 21000;
engineRoles = ["key", "comment", "bpm"];
scSaveSearchFields(["key", "comment", "bpm"]);
assert.equal(hung.xhr.aborted, true, "hung POST >20s must be aborted before issuing replacement");
const replacement = inflight.shift();
assert.ok(replacement && Number(replacement.reqHeaders["X-UI-Write-Seq"]) > hungSeq);
replacement.reply({ searchFields: ["key", "comment", "bpm"] });
// Late callback on aborted XHR must be ignored.
hung.reply({ searchFields: ["key", "bpm"] }, 200, "d-aborted-late", { rev: serverRev + 10 });
observe("aborted late callback ignored", engineRoles, ["key", "comment", "bpm"]);

assert.deepEqual(failures, []);
"""
        res = subprocess.run(["node", "-e", script], capture_output=True, text=True)
        self.assertEqual(res.returncode, 0, f"node failed:\n{res.stderr}\n{res.stdout}")


if __name__ == "__main__":
    unittest.main()
