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


class PrimeGoSwipeStemsTest(unittest.TestCase):
    """#224: both JP11 swipe menus offer Stems without changing full-swipe load."""

    def test_swipe_edits_cover_both_sides_without_lab(self):
        old_root = bp.ROOT
        try:
            bp.ROOT = "Skins/Embedded/"
            edits = bp.edits(False)
            for side in ("left", "right"):
                with self.subTest(side=side):
                    path = bp.ROOT + f"Views/BrowseView/Components/Components/Swipe{side.title()}Item.qml"
                    self.assertIn(path, edits)
                    replacements = "".join(new for _, new in edits[path])
                    self.assertIn("readonly property bool scStem:", replacements)
                    self.assertIn("trackList.isStemsList", replacements)
                    self.assertIn(f"!{side}Actions.some(", replacements)
                    self.assertIn(f"actionIndex >= {side}Actions.length", replacements)
                    self.assertIn('.concat(scStem ? [root.scT("Stems")] : [])', replacements)
        finally:
            bp.ROOT = old_root

    @unittest.skipUnless(shutil.which("node"), "Node is required for #224")
    def test_both_swipes_offer_stems_except_in_stems_collection_or_on_az04(self):
        for version in ("5.0.4", "5.3.0"):
            qrc = LAB / version / "qrc"
            if not qrc.is_dir():
                if version == "5.0.4":
                    continue
                self.skipTest(f"{qrc} not found (run on Oracle-Batam-arm)")
            with tempfile.TemporaryDirectory() as tmp:
                out = Path(tmp) / "packs"
                built = subprocess.run(
                    ["python3", str(BUILD_PACKS), "--qrc", str(qrc),
                     "--out", str(out)], capture_output=True, text=True)
                self.assertEqual(built.returncode, 0, built.stderr)
                # The shared Swipe{Left,Right}Item edits are inside if v53().
                # Still build all legacy languages to catch patch-table regressions.
                if version == "5.0.4":
                    continue
                for lang in ("en", "zh-Hans", "zh-Hant"):
                    for side, deck in (("left", 1), ("right", 2)):
                        with self.subTest(version=version, lang=lang, side=side):
                            qml = (out / lang / "Skins/Embedded/Views/BrowseView/Components/Components"
                                   / f"Swipe{side.title()}Item.qml").read_text()
                            self.assertIn("readonly property bool scStem:", qml)
                            self.assertIn("root.scIsAZ04", qml)
                            self.assertIn('!(typeof trackList !== "undefined" && trackList && trackList.isStemsList)', qml)
                            self.assertIn(f"!{side}Actions.some(", qml)
                            self.assertIn('.concat(scStem ? [root.scT("Stems")] : [])', qml)
                            self.assertIn(".concat(scStem ? [Style.systemBlue01Color] : [])", qml)
                            self.assertIn("root.scStemStream(row.modelData.trackId)", qml)
                            self.assertIn("startRenderTracks(String(row.modelData.trackId))", qml)
                            self.assertIn('textSwipeInvalid: scDeck > 0 ? "" :', qml)
                            expressions = {
                                name: re.search(r"readonly property [^\n]+? " + name + r": (.*)", qml).group(1)
                                for name in ("scStem", "actionLabels", "actionCommitMessages", "actionColors")
                            }
                            label_fn = re.search(r"function scLabel\(s\) \{[^\n]+", qml).group(0)
                            commit = qml.split("onCommit: (actionIndex, viaButton) => {", 1)[1]
                            commit = commit.split(f"swiped{side.title()}(", 1)[0]
                            script = r"""
const assert = require('assert');
String.prototype.arg = function(n) { return this.replace('%1', n); };
const expr = EXPRESSIONS;
function run(opts = {}) {
    const calls = [], writes = {};
    const row = {index: 7, modelData: {trackId: opts.streaming ? 99 : 42},
                 isStreamingTrack: !!opts.streaming, streamingProviderName: ''};
    const trackList = {isStemsList: !!opts.stemsList,
                       pCurrentIndex: {translator: {unnormalized: -1}}};
    const root = {scIsAZ04: !!opts.az04, scT: s => s,
                  scStemStream: id => {calls.push(['stream', id]); return !!opts.streaming;}};
    const Style = {systemBlue01Color: 'blue'};
    const actions = (opts.labels || STOCK).map((label, i) => ({
        display: {label, commitMessage: label, color: 'stock' + i},
        trigger: index => calls.push(['stock', i, index])
    }));
    const ACTIONS = actions;
    const scDeck = opts.deck || 0;
    LABEL_FUNCTION
    const scStem = eval(expr.scStem);
    const labels = eval(expr.actionLabels);
    const messages = eval(expr.actionCommitMessages);
    const actionCommitMessages = messages;
    const colors = eval(expr.actionColors);
    const Planck = {
        librarianController: {stemsQueueManager: {
            startRenderTracks: id => calls.push(['render', id])}},
        getProperty: path => ({translator: new Proxy({}, {
            set: (o, key, value) => {writes[path] = value; return true;}})})
    };
    let textCommitValid;
    if (opts.commit !== undefined) {
        const actionIndex = opts.commit;
        COMMIT_BODY
    }
    return {labels, messages, colors, calls, writes,
            current: trackList.pCurrentIndex.translator.unnormalized};
}
const stock = STOCK;
for (const streaming of [false, true]) {
    const r = run({streaming, commit: 2});
    assert.deepStrictEqual(r.labels, [...stock, 'Stems']);
    assert.deepStrictEqual(r.messages, [...stock, 'Stems']);
    assert.deepStrictEqual(r.colors, ['stock0', 'stock1', 'blue']);
    assert.deepStrictEqual(r.calls, streaming ? [['stream', 99]] :
                                               [['stream', 42], ['render', '42']]);
}
for (const opts of [{stemsList: true}, {az04: true}]) {
    const r = run(opts);
    assert.deepStrictEqual(r.labels, stock);
    assert.deepStrictEqual(r.colors, ['stock0', 'stock1']);
}
for (const label of ['Stems', 'Remove Stems']) {
    const labels = [...stock, label];
    for (const stemsList of [false, true]) {
        assert.deepStrictEqual(run({labels, stemsList}).labels, labels);
    }
}
assert.deepStrictEqual(run({commit: 0}).calls, [['stock', 0, 7]]);
assert.deepStrictEqual(run({commit: 1}).calls, [['stock', 1, 7]]);
assert.deepStrictEqual(run({deck: 3, commit: 2}).calls, [['stream', 42], ['render', '42']]);
const routed = run({deck: 3, commit: 0});
assert.deepStrictEqual(routed.labels, ['Deck 3', stock[1], 'Stems']);
assert.deepStrictEqual(routed.calls, []);
assert.deepStrictEqual(routed.writes, {'/Client/Librarian/LoadToDeck3': true});
assert.strictEqual(routed.current, 7);
"""
                            stock = [f"Deck {deck}", "Prepare" if side == "left" else "Load"]
                            for key, value in (
                                ("EXPRESSIONS", json.dumps(expressions)),
                                ("STOCK", json.dumps(stock)),
                                ("ACTIONS", side + "Actions"),
                                ("LABEL_FUNCTION", label_fn),
                                ("COMMIT_BODY", commit),
                            ):
                                script = script.replace(key, value)
                            checked = subprocess.run(["node"], input=script, capture_output=True, text=True)
                            self.assertEqual(checked.returncode, 0, checked.stderr)
class TrackListArtworkPlaceholderAndReaderPriorityTest(unittest.TestCase):
    def test_built_track_art_keeps_native_source_and_synchronous_fallback(self):
        for version, skin in (("5.0.4", "EmbeddedCommon"), ("5.3.0", "Embedded")):
            qrc = LAB / version / "qrc"
            with self.subTest(version=version):
                if not qrc.is_dir():
                    self.skipTest(f"{qrc} not found (run on Oracle-Batam-arm)")
                with tempfile.TemporaryDirectory(prefix="track-art-test-") as tmp:
                    out = Path(tmp) / "packs"
                    result = subprocess.run(["python3", str(BUILD_PACKS), "--qrc", str(qrc), "--out", str(out)],
                                            capture_output=True, text=True)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    with tarfile.open(out.with_suffix(".tar.gz")) as archive:
                        for lang in ("en", "zh-Hans", "zh-Hant"):
                            with self.subTest(lang=lang):
                                path = f"packs/{lang}/Skins/{skin}/Views/BrowseView/Components/Components/AlbumArt.qml"
                                source = archive.extractfile(path).read().decode()
                                child = re.search(r'Image\s*\{\s*objectName: "albumArt_placeholder"(.*?)\n    \}', source, re.S)
                                self.assertIsNotNone(child, path)
                                placeholder = "AlbumArtPlaceholder.png" if version == "5.3.0" else "UIKit.1/UIKit/Icons/PlaceHolder.png"
                                self.assertTrue((qrc / placeholder).is_file())
                                self.assertIn(f'source: "qrc:/{placeholder}"', child[1])
                                self.assertIn("anchors.fill: parent", child[1])
                                self.assertIn("visible: !!parent && parent.status !== Image.Ready && String(parent.source) !== \"\"", child[1])
                                self.assertNotIn("asynchronous: true", child[1])
                                # Native SD and Remote Library tracks must retain image://planck.
                                line = next(line for line in source.splitlines() if "source: row.streamingProviderName" in line)
                                script = "const assert=require('node:assert/strict'); const row={streamingProviderName:'',modelData:{albumArt:'image://planck/track?size=100',song:'Song'}};\n"
                                script += "assert.equal((" + line.strip().removeprefix("source: ") + "), row.modelData.albumArt);"
                                checked = subprocess.run(["node", "-e", script], capture_output=True, text=True)
                                self.assertEqual(checked.returncode, 0, checked.stderr)

    def test_sqlite_busy_timeout_before_schema_pragmas(self):
        source = (REPO / "tools/ui-mod/engine_ui_hook.c").read_text()
        tune = source.split("static void tune_database(", 1)[1].split("\n}", 1)[0]
        sql = "".join(re.findall(r'"(PRAGMA [^"]*)"', tune))
        self.assertTrue(sql.startswith("PRAGMA busy_timeout = 5000;"))
        self.assertLess(sql.index("busy_timeout"), sql.index("synchronous"))
        self.assertLess(sql.index("busy_timeout"), sql.index("cache_size"))

    def test_only_background_workers_are_demoted(self):
        source = (REPO / "tools/ui-mod/engine_ui_hook.c").read_text()
        classes = source.split("static int thread_class(", 1)[1].split("\n}", 1)[0]
        self.assertIn('if (!strncmp(name, "FileInfoThread", 14)) return 3;', classes)
        self.assertIn('return !strncmp(name, "QQuickPixmapRea", 15);', classes)
        self.assertIn('return 2;', classes)
        guard = source.split("static void guard_named_thread(", 1)[1].split("\n}", 1)[0]
        self.assertIn("if (kind == 2) pthread_setschedparam(thread, SCHED_OTHER, &param);", guard)
        # Only the global pixmap reader returns before either demotion path.
        self.assertRegex(guard, r"guard_affinity\(thread, kind\);\s*(?://[^\n]*\n\s*)*if \(kind == 1\) \{\s*errno = saved_errno;\s*return;\s*\}")
        self.assertEqual(guard.count("lower_io_nice("), 2)
        self.assertEqual(guard.count("10, kind == 2"), 2)

    def test_release_gate_checks_live_tracklist_art_on_both_boots(self):
        probe = (REPO / "tools/az01-packer/gate-ui-probe.qml").read_text()
        gate = (REPO / "tools/az01-packer/release-gate.sh").read_text()
        self.assertIn("SCPROBE trackart|", probe)
        self.assertIn('"albumArt_tracklist"', probe)
        self.assertIn('"albumArt_placeholder"', probe)
        for stage in ("new", "boot2"):
            self.assertRegex(gate, rf'check "{stage}: [^\n]+grep -aqF \'SCPROBE trackart\|1\|1\|\'')
        self.assertNotRegex(probe, r"for\s*\(const\s+[^)]*\s+of\s+[^)]*\.children\)")
        for line in gate.splitlines():
            if "no QML load errors" in line:
                self.assertNotIn("ReferenceError|TypeError|", line)
        # Execute the actual probe with delayed attachment and device selection.
        script = r'''
const assert = require('node:assert/strict'), vm = require('node:vm');
const source = require('node:fs').readFileSync(0, 'utf8');
function scenario(initialDrive, connectDemo = true, renderArt = true) {
    let now = 0, due = 3000, pending = null, deviceSwitch = null, opens = 0;
    const connected = initialDrive ? [{rowName: 'SDCARD'}] : [];
    let selected = connected[0] || null;
    const props = {}, messages = [], roots = [];
    const stale = {objectName: 'Browser_TrackList', visible: false, children: []};
    const trackList = {objectName: 'Browser_TrackList', visible: true, children: []};
    const windowRoot = {children: [stale, trackList]};
    const probe = {phase: 1, demoConnected: false, samplerConnected: false, demoWait: 0,
        artWait: 0, artSelected: false, artOpened: false, artRoot: stale,
        artFallbackWalked: false, Window: {window: {contentItem: windowRoot}}};
    const devices = {selectedDeviceCraylistRootModelIndex: 'initial-root'};
    const model = {index: i => ({connectedIndex: i})};
    const bsc = {connectedSourceModel: () => model,
        select: index => {
            assert.equal(index.connectedIndex, initialDrive ? 1 : 0);
            selected = connected[index.connectedIndex];
            deviceSwitch = 'demo-root'; // The selected device root changes next tick.
        }, detach: () => {}};
    const sourceView = {visible: false, expanded: false,
        onQuickSourceButtonClicked() {this.visible = true;},
        onExpandButtonClicked() {this.expanded = true;}, close() {this.visible = false;}};
    const context = vm.createContext({probe, Set, Image: {Ready: 1},
        console: {warn: s => messages.push(s)}, CollectionNameCraylist: 'Craylist',
        PlanckGUI: {views: {sourceScreenView: sourceView}},
        Planck: {getProperty: key => props[key] ||= {translator: {}},
            librarianController: {browserSourceControllerPtr: () => bsc, devicesController: devices,
                craylistController: {select: index => {
                    opens++;
                    assert.equal(selected?.isDemo, true, 'Demo must be selected');
                    assert.equal(index, 'demo-root', 'device selection must settle before opening');
                    assert.equal(props['/Client/Librarian/Collections'].translator.string, 'Craylist');
                }}}},
        scConnSources: {get count() {return connected.length;}, itemAt: i => connected[i]},
        selectAvailableMatch: re => {pending = {at: now, isDemo: re.test('Demo'),
            isSampler: re.test('Sample')}; return true;},
        restart: () => {due = now + 3000;}, running: true});
    vm.runInContext(source.slice(source.indexOf('    function kids('), source.indexOf('    function report()')), context);
    const walk = context.walk;
    context.walk = (root, seen, visitor) => {
        if (seen.size === 0) {roots.push(root); now += root === windowRoot ? 35000 : 20;}
        walk(root, seen, visitor);
    };
    const start = source.indexOf('        onTriggered: {') + '        onTriggered: {'.length;
    const end = source.lastIndexOf('\n        }\n    }\n}');
    vm.runInContext('function tick() {' + source.slice(start, end) + '\n}', context);
    function advance() {
        now = Math.max(now, due);
        if (deviceSwitch) {devices.selectedDeviceCraylistRootModelIndex = deviceSwitch; deviceSwitch = null;}
        if (pending && now - pending.at >= 500 && (connectDemo || !pending.isDemo)) {
            connected.push(pending); pending = null;
        }
        due = now + 3000;
        context.tick();
    }
    context.tick();
    assert.equal(probe.phase, 2);
    assert.ok(due - pending.at >= 3000, 'Demo needs a fresh interval after a slow scan');
    if (connectDemo) {
        advance();
        assert.equal(probe.artSelected, true);
        assert.equal(probe.artOpened, false);
        assert.equal(opens, 0);
        assert.equal(devices.selectedDeviceCraylistRootModelIndex, 'initial-root');
        assert.equal(due - now, 3000);
        advance();
        assert.equal(probe.artOpened, true);
        assert.equal(opens, 1);
        roots.length = 0;
        advance(); // An empty cached TrackList must fall back once to the live tree.
        assert.equal(probe.phase, 2);
        assert.equal(probe.artRoot, trackList);
        assert.equal(roots.filter(root => root === windowRoot).length, 1);
        assert.equal(due - now, 3000);
        if (renderArt) {
            // Model Qt 5.12's non-iterable QQmlListProperty, including Image.children.
            const child = {objectName: 'albumArt_placeholder', source: 'qrc:/AlbumArtPlaceholder.png',
                asynchronous: false, status: 1, visible: true};
            trackList.children = {0: {objectName: 'albumArt_tracklist', visible: true, width: 64,
                source: 'image://planck/demo', status: 0, children: {0: child, length: 1}}, length: 1};
            advance();
        } else {
            for (let i = 1; i < 6; ++i) {
                assert.equal(probe.phase, 2);
                advance();
            }
            assert.equal(probe.artWait, 6);
        }
        assert.equal(probe.phase, 3);
        assert.equal(roots.filter(root => root === windowRoot).length, 1);
        assert.ok(due - pending.at >= 3000, 'Sampler needs a fresh interval');
    } else {
        for (let i = 0; i < 6; ++i) advance();
        assert.equal(probe.phase, 3);
        assert.equal(opens, 0, 'missing Demo must never open an unrelated drive');
        assert.equal(probe.artSelected, false);
    }
    advance();
    assert.ok(messages.includes(renderArt && connectDemo ? 'SCPROBE trackart|1|1|' : 'SCPROBE trackart|0|0|'));
    assert.equal(messages.filter(s => s.startsWith('SCPROBE trackart|')).length, 1);
    assert.ok(messages.includes(`SCPROBE demosource|${connectDemo ? 1 : 0}|1|`));
    assert.ok(messages.includes('SCPROBE done|'));
    assert.ok(!messages.some(s => s.startsWith('SCPROBE error|')), messages.join('\n'));
    assert.equal(context.running, false);
    // Even with no cached/live view, six retries get only one whole-window walk.
    probe.artRoot = null; probe.artFallbackWalked = false; probe.artWait = 0;
    windowRoot.children = []; roots.length = 0;
    for (let i = 0; i < 6; ++i) context.reportTrackArt();
    assert.equal(roots.filter(root => root === windowRoot).length, 1);
}
scenario(false);
scenario(true); // Populated initial drive: Demo is connected index 1.
scenario(true, true, false); // Six artwork retries time out into Sample.
scenario(true, false); // Demo never connects; no Collection is opened.
'''
        checked = subprocess.run(["node", "-e", script], input=probe, capture_output=True, text=True)
        self.assertEqual(checked.returncode, 0, checked.stderr)


class UiModSourceTest(unittest.TestCase):
    def test_boot_ready_waits_for_main_splash_and_never_blocks_gui(self):
        self.assertIn('!scBootSplash.active && planckQuickView.isMainScreen', bp.BOOT_READY)
        self.assertNotIn('scBootReadyTimer.parent', bp.BOOT_READY)
        self.assertIn('/v1/sc5000/boot/ready', bp.BOOT_READY)
        self.assertIn('JSON.stringify({token: token})', bp.BOOT_READY)
        self.assertIn('request !== x', bp.BOOT_READY)
        self.assertIn('old.abort()', bp.BOOT_READY)
        self.assertNotRegex(SRC, r'\.open\([^\n]*, false\)')

    def test_boot_ready_rejects_a_stock_loader_id_collision(self):
        bp.verify_boot_ready_loader('SplashScreen {\n objectName: "SplashScreenLoader"\n active: true\n}\n')
        for source in (
            'SplashScreen {\n id: stockSplash\n objectName: "SplashScreenLoader"\n}\n',
            'SplashScreen {\n objectName: "SplashScreenLoader"\n id: stockSplash\n}\n',
            'Item {\n objectName: "other"\n}\n',
        ):
            with self.assertRaisesRegex(ValueError, "stock splash loader"):
                bp.verify_boot_ready_loader(source)

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

    def test_auto_analyze_preserves_preference_and_removes_dead_row(self):
        self.assertIn("property bool scAutoAnalyze: false", bp.SERVICES_MAIN)
        self.assertIn("scAutoAnalyze = !!v.autoAnalyze", bp.SERVICES_MAIN)
        self.assertNotIn("Auto-Analyze", bp.LIBRARY_ENTRIES)
        self.assertNotIn("scAutoAnalyze", bp.LIBRARY_ENTRIES)
        self.assertNotIn("autoAnalyze=", bp.LIBRARY_ENTRIES)
        self.assertNotIn("Auto-Analyze Drives (Unavailable)", bp.load_dict())

    def test_auto_analyze_gate_requires_fail_closed_capability(self):
        probe = (BUILD_PACKS.parent.parent / "az01-packer/gate-ui-probe.qml").read_text()
        gate = (BUILD_PACKS.parent.parent / "az01-packer/release-gate.sh").read_text()
        self.assertIn("main.scAutoAnalyzeAvailable", probe)
        self.assertNotIn("/Client/Preferences/AutoAnalysis", probe)
        self.assertIn("SCPROBE analyze|boolean|false|", gate)
        self.assertNotIn("SCPROBE analyze|boolean|1|", gate)

    def test_release_gate_runs_second_boot_after_power_cut_and_checks_splash(self):
        probe = (BUILD_PACKS.parent.parent / "az01-packer/gate-ui-probe.qml").read_text()
        gate = (BUILD_PACKS.parent.parent / "az01-packer/release-gate.sh").read_text()
        self.assertIn("splashScreenController", probe)
        self.assertIn("SplashScreenLoader", probe)
        self.assertIn("SCPROBE splash|", probe)
        self.assertIn("SCPROBE splash|0|1|", gate)
        self.assertIn("SC5000_GATE_LIBRARY=$W/drive", gate)
        self.assertIn("/media/az01-internal", gate)
        self.assertIn("boot2: main UI acknowledged the current boot attempt without fallback (#140)", gate)
        self.assertNotIn("SC5000_UI_ALIVE_CMD=", gate)  # uptime is not health (#140 boot safety)
        self.assertIn("--bounding-set=-sys_time", gate)
        self.assertIn("kill -KILL", gate)
        self.assertIn("--phase boot2", gate)
        # #206: the gate proves streaming loads under an active recorder on all-in-ones.
        self.assertIn("SCPROBE recstream|", probe)
        self.assertIn("SCPROBE recstream|1|1|1|0|0|", gate)
        self.assertIn("case $PRODUCT in JP07|JP08|JP13|JP14) applied=20 ;; *) applied=28 ;; esac", gate)
        self.assertIn("powercut-truncated.qmlc", gate)

    def test_connected_source_disables_listview_sections_to_prevent_demo_polish_loop(self):
        old_root = bp.ROOT
        try:
            bp.ROOT = "Skins/Embedded/"
            edits = bp.edits(False)
            cs_patches = edits["Skins/Embedded/Views/BrowserSource/ConnectedSource.qml"]
            stock_cs = (Path("/root/build/sc5000-lab/5.3.0/qrc/Skins/Embedded/Views/BrowserSource/ConnectedSource.qml").read_text()
                        if Path("/root/build/sc5000-lab/5.3.0/qrc/Skins/Embedded/Views/BrowserSource/ConnectedSource.qml").is_file()
                        else "ListView {\n    currentIndex: -1\n    delegate: ConnectedSourceDelegate {\n        height: 108\n        leftPadding: 8\n    }\n    section.property: \"typeText\"\n}\n")
            patched = stock_cs
            for old, new in cs_patches:
                patched = bp.patch(patched, old, new, "ConnectedSource.qml")
            self.assertIn('section.property: ""', patched)
            self.assertNotIn('section.property: "typeText"', patched)
            avail = edits["Skins/Embedded/Views/BrowserSource/AvailableSource.qml"]
            self.assertNotIn("Demo", avail)
            self.assertNotIn("Sampler", avail)
        finally:
            bp.ROOT = old_root
        probe = (BUILD_PACKS.parent.parent / "az01-packer/gate-ui-probe.qml").read_text()
        gate = (BUILD_PACKS.parent.parent / "az01-packer/release-gate.sh").read_text()
        self.assertIn("SCPROBE demosource|", probe)
        self.assertIn("SCPROBE demosource|1|1|", gate)
        self.assertIn("polish", gate)

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
        self.assertIn("Math.abs(exact - shown) < 0.15 || (shown === 100 && exact > 100) ? exact : shown", m)
        self.assertIn("(p.playing ? 180 : 5)", m)
        self.assertIn("o.currentBpm = s.currentBpm", m)
        self.assertIn("const wantSync = !!e.sync && (!e.link || !root.scPlayer || at.nativeSync)", m)
        self.assertIn("function scShadowSpeed(d, pitch, want)", m)
        self.assertIn("at.sd * 0.85 + drift * 0.15", m)
        self.assertNotIn("at.postPlay", m)  # the gap after Play is closed by scShadowBend, not a second seek
        self.assertIn("e.seq === at.lastSeq", m)
        self.assertIn('x.open("POST", "http://127.0.0.1:8080/v1/sc5000/debug/shadow")', m)
        self.assertIn("root.scShadowDebug = !!r.debug", m)
        self.assertIn("LoadOverlay/LoadOverlayDecks.qml", SRC)
        # Standalone EngineX peers share Sync/master; consoles (#160) still need shadow control
        self.assertIn("if ((!e.link || !root.scPlayer || at.nativeSync) && wantSync !== synced", m)
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
        for gone in ("/GUI/Scripted/Zoom", "scZoom(", "BrowseEncoder.qml", "DeckAction.qml", "Vinyl.qml", "JogWheel.qml",
                     "!deckConfig.gridEditActive && deckConfig.playing"):
            self.assertNotIn(gone, SRC)
        self.assertIn("Planck.getProperty(\"/Engine/Master/WaveformZoom\")", bp.ZOOM_PROP)  # the shadow lane reads it

    def test_swipe_left_has_one_stems(self):
        self.assertEqual(SRC.count("!leftActions.some(action => /stem/i.test("), 2)
        self.assertEqual(SRC.count("/^stems?$/i.test("), 4)
        self.assertIn("/stem/i.test(String(leftActions[actionIndex]", SRC)

    def test_gen1_stems_first_boot_suppressed_and_row_hidden(self):
        # Preferences -> Device -> Stems Support stays hidden on 5.3.0 packs.
        self.assertIn('out[prefs + "Entries/StemsDownload.qml"] = [("active: manager && manager.canModifyInstall", "active: false")]', SRC)
        hook = (REPO / "tools/ui-mod/engine_ui_hook.c").read_text(encoding="utf-8")
        # Gen1 5.3.0 only: prepare_config sets Config.hasStemsProcessorFactoryInstalled ([r0, #0x2a7] = 1)
        # so first-boot NewFeaturePopup never offers a Download button, with zero extra edit sites
        # and 5.0.4 / 5.3.0-sclive4 / AZ04 left untouched.
        cfg_fn = hook.split("prepare_config(uintptr_t base, size_t page) {", 1)[1].split("\n}\n", 1)[0]
        self.assertTrue('if (!strcmp(build->name, "5.3.0"))' in cfg_fn and "0xe5c022a7" in cfg_fn,
                        "missing gen1 5.3.0 Config[0x2a7]=1 in prepare_config")
        edits_504 = hook.split("edits_504[] = {", 1)[1].split("};", 1)[0]
        edits_530 = hook.split("edits_530[] = {", 1)[1].split("};", 1)[0]
        edits_sclive = hook.split("edits_sclive[] = {", 1)[1].split("};", 1)[0]
        edits_az04 = hook.split("edits_az04[] = {", 1)[1].split("};", 1)[0]
        for block in (edits_504, edits_530, edits_sclive, edits_az04):
            self.assertTrue("0x5070ecu" not in block and "0x8c5e84u" not in block
                            and "0x4e28ccu" not in block and "0x86520cu" not in block,
                            "edit tables must add no extra Stems popup sites")

    def test_auto_loop_knob_and_link(self):
        self.assertIn("interval: 10000; running: true", SRC)
        self.assertIn("module.loopInactiveShiftTurnAction === Action.BeatJump || ", SRC)
        self.assertNotIn("LinkBeatJumpLoopSize", bp.SERVICES_MAIN)
        self.assertNotIn("/Client/Preferences/Profile/Application/LinkBeatJumpLoopSize", SRC)
        self.assertIn(
            'prefs + "Entries/LinkBeatJumpLoopSize.qml": [("active: Config.audio.deck.beatJump.canLinkToAutoLoop", "active: true")]',
            SRC,
        )

    def test_jog_light_removed_and_view_deduped(self):
        self.assertNotIn("scJog", SRC)
        self.assertNotIn("JOG_ENTRY", SRC)
        self.assertNotIn("JOG_TILE", SRC)
        self.assertIn("WAVE_TILE", SRC)  # Control Center Waveform Style tile restored
        self.assertNotIn("WaveformStyle { }", bp.VIEW_ENTRIES)  # View preset does not duplicate Display -> WaveformStyle
        self.assertNotIn("DisplayTabContent.qml", SRC)  # stock Preferences -> Display WaveformStyle {} stays untouched
        self.assertIn('root.scWave = seg.waveId; root.scCall("waveform=" + seg.waveId)', bp.WAVE_TILE)
        rows = [l.split("\t") for l in (REPO / "tools/ui-mod/i18n.tsv").read_text(encoding="utf-8").splitlines() if l and not l.startswith("#")]
        keys = [r[0] for r in rows]
        self.assertEqual(len(keys), len(set(keys)), "duplicate keys in i18n.tsv")
        # "Static" (Display -> Time Format) and "Fast" (Mic -> Talkover Resume) are stock Engine translator options.
        for removed in ("Jog Light Mode", "Jog Light", "Psy Rainbow", "Psy Fast", "Psy BPM Sync", "Rainbow"):
            self.assertNotIn(removed, keys)
        for added in ("ENGINEX VIEW PRESET", "Shadow Deck Mode", "Pro DJ Link Export to CDJ (Beta)", "Fader Echo: Up", "Fader Echo: All", "Restart Engine to apply? Playback will stop."):
            self.assertIn(added, keys)
        self.assertFalse([k for k in keys if "Legacy Shadow Engine" in k], "the Legacy Shadow Engine row was replaced by Shadow Deck Mode")
        self.assertNotIn("scShadowLegacyEngine", bp.SERVICES_MAIN + bp.FOUR_DECK_ENTRIES)
        # ENGINEX VIEW PRESET must be on its own text: line so translate() matches it.
        self.assertRegex(bp.VIEW_ENTRIES, r'(?m)^\s*text:\s*"ENGINEX VIEW PRESET"\s*$')

    def test_board_capabilities_jp07_vs_jc11_and_build_tag(self):
        # Task B: board-level capability gating must NOT depend on Config.device.isPlayer
        for expr in (
            'visible: root.scHasMixer',
            'visible: root.scHasStemsEq',
            'visible: root.scIsMixstream',
            'visible: root.scHasAutoLoopKnob',
            'visible: root.scCanFourDeck',
        ):
            self.assertIn(expr, SRC)
        self.assertIn('root.scHasMixer', bp.UPFADER_ECHO_TILE)
        self.assertIn('root.scHasMixer', bp.PLAYBACK_ENTRIES)
        self.assertIn('root.scHasStemsEq', bp.PLAYBACK_ENTRIES)
        self.assertIn('root.scIsMixstream', bp.BEAT_JUMP_ENTRIES)
        self.assertIn('root.scHasAutoLoopKnob', bp.BEAT_JUMP_ENTRIES)
        self.assertIn('root.scCanFourDeck', bp.FOUR_DECK_ENTRIES)
        # Never rely on Config.device.isPlayer for our custom visibility gates.
        self.assertNotIn("Config.device.isPlayer", bp.UPFADER_ECHO_TILE + bp.PLAYBACK_ENTRIES + bp.BEAT_JUMP_ENTRIES + bp.FOUR_DECK_ENTRIES)
        # Stock TransitionFxEnable stays untouched in Control Center, UPFADER_ECHO_TILE hides where
        # stock tile or hardware Fader FX buttons exist, and Settings -> Mixer syncs with vfx.enabled.
        self.assertNotIn("ControlCenter/Tabs/ControlCenter/Entries/TransitionFxEnable.qml", SRC)
        self.assertIn("root.scHasMixer && !root.scHasStockTransitionTile", bp.UPFADER_ECHO_TILE)
        self.assertIn("scTransitionFxView", SRC)
        self.assertIn("function scSetTransitionFx(", SRC)
        self.assertIn("triggerSourceOptions", SRC)
        self.assertNotIn("setTriggerSource(index === 2 ? 1 : 2)", SRC)
        self.assertIn("function onEnabledChanged()", SRC)
        self.assertIn('values: ["Off", "All Faders", "Upfader Only"]', SRC)
        self.assertIn("Config.device.hasAutoLoopKnob !== undefined", SRC)
        self.assertIn("Config.audio.fx.hasNHFx !== undefined", SRC)

        if shutil.which("node"):
            code = r"""
const assert = require('assert');
function evalCaps(board, scPlayer, cfg, hasVfx) {
    const scBoard = String(board || "");
    const scIsPlayerBoard = !!scPlayer || ["JP07", "JP08", "JP13", "JP14"].indexOf(scBoard) >= 0;
    const scHasMixer = !scIsPlayerBoard;
    const scCanFourDeck = scIsPlayerBoard || ["JC11", "JC11S", "JP21", "JP24"].indexOf(scBoard) >= 0;
    const scHasAutoLoopKnob = cfg.device && cfg.device.hasAutoLoopKnob !== undefined ? !!cfg.device.hasAutoLoopKnob : ["NH08", "NH08S", "NH10"].indexOf(scBoard) < 0;
    const scIsMixstream = cfg.audio && cfg.audio.fx && cfg.audio.fx.hasNHFx !== undefined ? !!cfg.audio.fx.hasNHFx : ["NH08", "NH08S", "NH10"].indexOf(scBoard) >= 0;
    const scHasStemsEq = scHasMixer && (cfg.audio ? !!cfg.audio.hasStemsLevel : false);
    const scIsAZ04 = ["JP24", "RMZ2"].indexOf(scBoard) >= 0;
    const scHasStockTransitionTile = !!(cfg.audio && cfg.audio.fx && cfg.audio.fx.hasTransitionFx && cfg.device && !cfg.device.hasHardwareFaderFxButtons && hasVfx);
    const showCustomEchoTile = scHasMixer && !scHasStockTransitionTile && !(cfg.device && cfg.device.hasHardwareFaderFxButtons);
    return { scIsPlayerBoard, scHasMixer, scCanFourDeck, scHasAutoLoopKnob, scIsMixstream, scHasStemsEq, scIsAZ04, scHasStockTransitionTile, showCustomEchoTile };
}
// JP07 (SC5000) with 4-deck hook clearing Config.device.isPlayer to false:
const jp07 = evalCaps("JP07", true, { device: { isPlayer: false, hasAutoLoopKnob: true, hasHardwareFaderFxButtons: false }, audio: { hasStemsLevel: true, fx: { hasNHFx: false, hasTransitionFx: false } } }, true);
assert.deepStrictEqual(jp07, { scIsPlayerBoard: true, scHasMixer: false, scCanFourDeck: true, scHasAutoLoopKnob: true, scIsMixstream: false, scHasStemsEq: false, scIsAZ04: false, scHasStockTransitionTile: false, showCustomEchoTile: false });
// JC11 (PRIME 4) shows stock TransitionFxEnable tile, hides duplicate UPFADER_ECHO_TILE:
const jc11 = evalCaps("JC11", false, { device: { isPlayer: false, hasAutoLoopKnob: true, hasHardwareFaderFxButtons: false }, audio: { hasStemsLevel: true, fx: { hasNHFx: false, hasTransitionFx: true } } }, true);
assert.deepStrictEqual(jc11, { scIsPlayerBoard: false, scHasMixer: true, scCanFourDeck: true, scHasAutoLoopKnob: true, scIsMixstream: false, scHasStemsEq: true, scIsAZ04: false, scHasStockTransitionTile: true, showCustomEchoTile: false });
// NH08 (Mixstream Pro, 2 decks, no AutoLoop knob, hasNHFx, stock transition tile active):
const nh08 = evalCaps("NH08", false, { device: { isPlayer: false, hasAutoLoopKnob: false, hasHardwareFaderFxButtons: false }, audio: { hasStemsLevel: true, fx: { hasNHFx: true, hasTransitionFx: true } } }, true);
assert.deepStrictEqual(nh08, { scIsPlayerBoard: false, scHasMixer: true, scCanFourDeck: false, scHasAutoLoopKnob: false, scIsMixstream: true, scHasStemsEq: true, scIsAZ04: false, scHasStockTransitionTile: true, showCustomEchoTile: false });
// JP11 (PRIME GO, 2 decks, has mixer & AutoLoop knob, no 4-deck, stock transition tile active):
const jp11 = evalCaps("JP11", false, { device: { isPlayer: false, hasAutoLoopKnob: true, hasHardwareFaderFxButtons: false }, audio: { hasStemsLevel: true, fx: { hasNHFx: false, hasTransitionFx: true } } }, true);
assert.deepStrictEqual(jp11, { scIsPlayerBoard: false, scHasMixer: true, scCanFourDeck: false, scHasAutoLoopKnob: true, scIsMixstream: false, scHasStemsEq: true, scIsAZ04: false, scHasStockTransitionTile: true, showCustomEchoTile: false });
// JP24 (PRIME 4 G2, AZ04 with hardware Fader FX buttons):
const jp24 = evalCaps("JP24", false, { device: { isPlayer: false, hasAutoLoopKnob: true, hasHardwareFaderFxButtons: true }, audio: { hasStemsLevel: false, fx: { hasNHFx: false, hasTransitionFx: true } } }, true);
assert.deepStrictEqual(jp24, { scIsPlayerBoard: false, scHasMixer: true, scCanFourDeck: true, scHasAutoLoopKnob: true, scIsMixstream: false, scHasStemsEq: false, scIsAZ04: true, scHasStockTransitionTile: false, showCustomEchoTile: false });
// Verify scSetTransitionFx passes triggerSourceOptions strings ("Channel Faders" / "Both"):
const vfx = {
    enabled: false,
    triggerSource: 2,
    triggerSourceOptions: ["Crossfader", "Channel Faders", "Both"],
    setTriggerSource(s) {
        const idx = this.triggerSourceOptions.indexOf(s);
        if (idx < 0) throw new Error("no matching fader fx trigger source for " + s);
        this.triggerSource = idx;
    },
};
function scSetTransitionFx(v, on, upfaderOnly) {
    if (!v) return;
    if (!on) { v.enabled = false; return; }
    const opts = v.triggerSourceOptions;
    if (opts && opts.length > 0) {
        const idx = upfaderOnly ? 1 : 2;
        const val = opts[idx] !== undefined ? opts[idx] : opts[opts.length - 1];
        if (val !== undefined) v.setTriggerSource(String(val));
    }
    v.enabled = true;
}
scSetTransitionFx(vfx, true, true);
assert.strictEqual(vfx.enabled, true);
assert.strictEqual(vfx.triggerSource, 1);
scSetTransitionFx(vfx, true, false);
assert.strictEqual(vfx.enabled, true);
assert.strictEqual(vfx.triggerSource, 2);
scSetTransitionFx(vfx, false, false);
assert.strictEqual(vfx.enabled, false);
"""
            res = subprocess.run(["node", "-e", code], capture_output=True, text=True)
            self.assertEqual(res.returncode, 0, res.stderr)

        # Task C: build_packs.py --print-build-tag / --build-tag / --release
        self.assertIn("scAboutBuildTag", SRC)
        self.assertIn("scPackBuildTag", SRC)
        qrc = LAB / "5.3.0" / "qrc"
        if qrc.is_dir():
            with tempfile.TemporaryDirectory(prefix="ui-tag-test-") as tmp:
                dev_out = Path(tmp) / "dev"
                r1 = subprocess.run(["python3", str(BUILD_PACKS), "--qrc", str(qrc), "--out", str(dev_out), "--build-tag", "sbx 085a437"],
                                    capture_output=True, text=True)
                self.assertEqual(r1.returncode, 0, r1.stderr)
                em_dev = (dev_out / "en/Skins/Embedded/EmbeddedMain.qml").read_text()
                self.assertIn('readonly property string scPackBuildTag: "sbx 085a437"', em_dev)
                # Stock Preferences -> Display DisplayTabContent.qml stays un-overridden;
                # ControlCenter TransitionFxEnable.qml keeps stock active condition (only i18n text).
                disp = (dev_out / "en/Skins/Embedded/Views/ControlCenter/Tabs/Preferences/DisplayTabContent.qml")
                self.assertFalse(disp.exists())
                tfe = (dev_out / "en/Skins/Embedded/Views/ControlCenter/Tabs/ControlCenter/Entries/TransitionFxEnable.qml")
                self.assertTrue(tfe.exists())
                tfe_txt = tfe.read_text()
                self.assertIn("active: Config.audio.fx.hasTransitionFx && !Config.device.hasHardwareFaderFxButtons && view", tfe_txt)
                self.assertNotIn("active: false", tfe_txt)
                lbj = (dev_out / "en/Skins/Embedded/Views/ControlCenter/Tabs/Preferences/Entries/LinkBeatJumpLoopSize.qml")
                self.assertTrue(lbj.exists())
                lbj_txt = lbj.read_text()
                self.assertIn("active: true", lbj_txt)
                self.assertNotIn("canLinkToAutoLoop", lbj_txt)
                self.assertIn("WaveformStyle {}", (qrc / "Skins/Embedded/Views/ControlCenter/Tabs/Preferences/DisplayTabContent.qml").read_text())

                rel_out = Path(tmp) / "rel"
                r2 = subprocess.run(["python3", str(BUILD_PACKS), "--qrc", str(qrc), "--out", str(rel_out), "--release"],
                                    capture_output=True, text=True)
                self.assertEqual(r2.returncode, 0, r2.stderr)
                em_rel = (rel_out / "en/Skins/Embedded/EmbeddedMain.qml").read_text()
                self.assertIn('readonly property string scPackBuildTag: ""', em_rel)

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
        for dup in (bp.UI_SCALE_ENTRY, bp.FOUR_DECK_ENTRIES, bp.DUAL_COLUMN_ENTRY, bp.BEAT_JUMP_ENTRIES):
            self.assertIn(dup, bp.VIEW_ENTRIES)
            self.assertNotIn(dup, bp.LIBRARY_ENTRIES)
        self.assertNotIn("LANGUAGE_ENTRY + UI_SCALE_ENTRY", SRC)
        self.assertNotIn('("CuesLoops", "PausedSavedLoopBehavior"', SRC)
        rows = [l.split("\t") for l in (REPO / "tools/ui-mod/i18n.tsv").read_text().splitlines()]
        self.assertIn(["Cue Countdown", "热标拍数倒数", "熱標拍數倒數"], rows)

    def test_deck_list_follows_four_deck_role(self):
        self.assertIn('root.scFourDeckRole.split("").map(c => parseInt(c))', bp.SERVICES_MAIN)
        self.assertEqual(SRC.count("root.decks && root.decks.length ? root.decks : [1, 2]"), 1)  # only inside scDeckList

    def test_update_refusal_messages_are_translated(self):
        """#239: the deck shows the fixed refusal messages of pkg/webui/update_verify.go on its screen
        (scT(v.error)); each needs a Chinese line, and a failed update raises a warning prompt."""
        go = (REPO / "pkg/webui/update_verify.go").read_text(encoding="utf-8")
        msgs = re.findall(r'^\s*msgUpdate\w+\s*=\s*"([^"]+)"', go, re.M)
        self.assertGreaterEqual(len(msgs), 9, msgs)
        msgs.append("update download too large")  # update.go: a file over the size limit
        rows = {l.split("\t")[0]: l.split("\t")[1:] for l in (REPO / "tools/ui-mod/i18n.tsv").read_text(encoding="utf-8").splitlines() if l and not l.startswith("#")}
        for m in msgs + ["Update Failed"]:
            self.assertIn(m, rows, f"missing translation in tools/ui-mod/i18n.tsv: {m!r}")
            for lang, val in zip(("zh-Hans", "zh-Hant"), rows[m]):
                self.assertTrue(val.strip() and val != m, f"{m} ({lang}) is not translated")
        self.assertIn("function scUpdateFailed(err)", bp.SERVICES_MAIN)
        self.assertIn("type: Prompt.Warning", bp.SERVICES_MAIN)
        # The reboot notice also arrives as an error text, with progress 1: it is not a failure.
        self.assertIn("v.error && !(v.progress >= 1)) scUpdateFailed(v.error)", bp.SERVICES_MAIN)

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
        self.assertIn("entryWidth: narrow ? 104 : 88", bp.UI_SCALE_ENTRY)
        self.assertIn("entryWidth: 104", bp.FOUR_DECK_ENTRIES)
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

    def test_led_brightness_rgb_and_single_color_passthrough(self):
        self.assertIn('text: "Pad LED Brightness"', bp.LED_ENTRY)
        self.assertIn('readonly property var ids: [100, 80, 60, 40, 20]', bp.LED_ENTRY)
        self.assertIn('root.scCall("ledBrightness=" + ids[index])', bp.LED_ENTRY)
        rows = [l.split("\t") for l in (REPO / "tools/ui-mod/i18n.tsv").read_text().splitlines()]
        self.assertIn(["Pad LED Brightness", "打击垫亮度", "打擊墊亮度"], rows)
        self.assertIn(r'property real scBrightness: typeof globalConfig !== \"undefined\" && globalConfig && globalConfig.scPadBrightness ? globalConfig.scPadBrightness : 1', SRC)
        hook = (REPO / "tools/ui-mod/engine_ui_hook.c").read_text()
        self.assertNotIn("note_slots", hook)
        self.assertNotIn("led_scaled_note", hook)
        self.assertNotIn("scale_led_pwm", hook)
        self.assertIn("static const unsigned p127[4] = {64, 28, 12, 4}, p16[4] = {12, 8, 5, 2};", hook)
        self.assertIn("if (m <= 3) {\n        target = 1;", hook)
        self.assertIn("if (target < 2) target = 2;", hook)

    def test_four_deck_player_board_preserves_stock_player_ui(self):
        # #49: 4-deck mode clears Config.device.isPlayer in C++ so Engine builds the 4-deck
        # layout, while QML restores stock player behaviour on JP07/JP08/JP13/JP14 boards
        # without altering all-in-one consoles (JC11, JP11, JP21, etc.).
        self.assertIn("readonly property bool scPlayerColorsSynced:", bp.SERVICES_MAIN)
        self.assertIn("function scPlayerColorOf(tag)", bp.SERVICES_MAIN)
        for target in (
            'ROOT + "main.qml"',
            'prefs + "Entries/DeckColorSelect.qml"',
            'prefs + "Entries/private/DualLayerColorSelect.qml"',
            'ROOT + "StateNodes/States/PerformanceViewState.qml"',
            'ROOT + "StateNodes/States/PerformanceViewStreamingState.qml"',
            'ROOT + "StateNodes/ControlFocusNode.qml"',
            'prefs + "Entries/PlayerNumber.qml"',
            'prefs + "Entries/ShowLayerB.qml"',
            'v + "EngineConnect/EngineConnect.qml"',
            'v + "TouchFx/TouchFxViewLowerBar.qml"',
        ):
            self.assertIn(target, SRC)
        if shutil.which("node"):
            old_root = bp.ROOT
            try:
                bp.ROOT = "Skins/Embedded/"
                edits_map = bp.edits(False)
                deck_color_patch = edits_map[bp.ROOT + "Views/MainView/DeckInfo/DeckColor/DeckColor.qml"][0][1]
                dual_layer_patch = edits_map[bp.ROOT + "Views/ControlCenter/Tabs/Preferences/Entries/private/DualLayerColorSelect.qml"][0][1]
                engine_conn_patch = edits_map[bp.ROOT + "Views/EngineConnect/EngineConnect.qml"][1][1]
            finally:
                bp.ROOT = old_root
            js = re.sub(r"readonly property \w+ (\w+):", r"const \1 =", deck_color_patch)
            dual_js = re.sub(r"readonly property var scLayerTr:", "const scLayerTr = (() =>", dual_layer_patch)
            dual_js = dual_js.replace("        visible:", "        )();\n        const visible =")
            conn_js = re.sub(r"readonly property var scLayerTr:", "const scLayerTr = (() =>", engine_conn_patch)
            conn_js = conn_js.replace("                        deckLayer:", "                        )();\n                        const deckLayer =")
            script = r"""
const assert = require('assert');
const WHITE = {r: 1, g: 1, b: 1, a: 1, hex: '#ffffff'};
const BLACK = {r: 0, g: 0, b: 0, a: 1, hex: '#000000'};
const TRANSPARENT = {r: 0, g: 0, b: 0, a: 0, hex: 'transparent'};
const CONSOLE_BLUE = {r: 0.14, g: 0.27, b: 0.40, a: 1, hex: '#234567'};
const LAYER_PINK = {r: 0.67, g: 0.80, b: 0.94, a: 1, hex: '#abcdef'};
const OWN_GREEN = {r: 0, g: 0.9, b: 0.4, a: 1, hex: '#00e666'};
function evalDeckColor(board, isPlayerBoard, role, deckIndex, ownColor, prefs) {
    const Config = {device: {name: board}};
    const root = {scIsPlayerBoard: isPlayerBoard, scFourDeckRole: role};
    const deckView = {deckData: {deckColor: ownColor}};
    const Planck = {
        getProperty(p) {
            if (p === '/Client/Preferences/Player') return {translator: {string: '1'}};
            const k = p.replace('/Client/Preferences/Profile/Application/PlayerColor', '');
            return prefs[k] ? {translator: {color: prefs[k]}} : null;
        }
    };
""" + js + r"""
    return playerColor;
}
const prefs = {'1': CONSOLE_BLUE, '1A': LAYER_PINK, '3': BLACK};
for (const blank of [WHITE, BLACK, TRANSPARENT]) {
    // 1. All-in-one consoles (2-deck or 4-deck) must never take PlayerColor1A/1B:
    for (const board of ['JC11', 'JP11', 'JC16', 'NH08', 'JP21', 'JP24']) {
        for (const role of ['', '12', '13']) {
            assert.strictEqual(evalDeckColor(board, false, role, 1, blank, prefs), CONSOLE_BLUE);
            assert.strictEqual(evalDeckColor(board, false, role, 3, blank, prefs), CONSOLE_BLUE);
        }
    }
    // 2. Player board with 4-deck off (2-deck mode) must also keep pre-48109aa fallback:
    assert.strictEqual(evalDeckColor('JP07', true, '', 1, blank, prefs), CONSOLE_BLUE);
    assert.strictEqual(evalDeckColor('JP07', true, '', 3, blank, prefs), CONSOLE_BLUE);
    // 3. Player board with 4-deck enabled uses Layer preference:
    assert.strictEqual(evalDeckColor('JP07', true, '13', 1, blank, prefs), LAYER_PINK);
}
// Non-blank own color is preserved everywhere:
assert.strictEqual(evalDeckColor('JC11', false, '12', 1, OWN_GREEN, prefs), OWN_GREEN);
assert.strictEqual(evalDeckColor('JP07', true, '13', 1, OWN_GREEN, prefs), OWN_GREEN);

function evalLayerFallback(role, propMap, index = 0, deckLayerIndex = 0) {
    const root = {scFourDeckRole: role};
    const enabled = true;
    const asked = [];
    const Planck = { getProperty(p) { asked.push(p); return p in propMap ? propMap[p] : null; } };
    let visible, deckLayer;
    { """ + dual_js.replace("const visible =", "visible =") + r""" }
    { """ + conn_js.replace("const deckLayer =", "deckLayer =") + r""" }
    return {visible, deckLayer, asked};
}
// Branch 1: primary property is null, fallback exists on Left (role 13) or Right (role 24):
let r1 = evalLayerFallback('13', {'/GUI/Decks/Deck/Layer': null, '/GUI/Decks/DeckLeft/Layer': {translator: {numEntries: 2, entries: ['1', '3']}}}, 1, 1);
assert.strictEqual(r1.visible, true);
assert.strictEqual(r1.deckLayer, 'B');
assert.ok(r1.asked.includes('/GUI/Decks/DeckLeft/Layer'));
let r2 = evalLayerFallback('24', {'/GUI/Decks/Deck/Layer': null, '/GUI/Decks/DeckRight/Layer': {translator: {numEntries: 2, entries: ['2', '4']}}}, 0, 0);
assert.strictEqual(r2.visible, true);
assert.strictEqual(r2.deckLayer, 'A');
assert.ok(r2.asked.includes('/GUI/Decks/DeckRight/Layer'));
// Branch 2: primary property exists but translator is null:
let r3 = evalLayerFallback('13', {'/GUI/Decks/Deck/Layer': {translator: null}, '/GUI/Decks/DeckLeft/Layer': {translator: {numEntries: 2, entries: ['1', '3']}}}, 0, 0);
assert.strictEqual(r3.visible, true);
assert.strictEqual(r3.deckLayer, 'A');
// Branch 3: both primary and fallback are null / translator null (or 2-deck with missing property):
for (const map of [
    {},
    {'/GUI/Decks/Deck/Layer': null, '/GUI/Decks/DeckLeft/Layer': null},
    {'/GUI/Decks/Deck/Layer': {translator: null}, '/GUI/Decks/DeckLeft/Layer': {translator: null}},
]) {
    let rNone = evalLayerFallback('13', map, 0, 0);
    assert.strictEqual(rNone.visible, false);
    assert.strictEqual(rNone.deckLayer, '-');
    let rTwoDeck = evalLayerFallback('', map, 0, 0);
    assert.strictEqual(rTwoDeck.visible, false);
    assert.strictEqual(rTwoDeck.deckLayer, '-');
}
// Stock 2-deck path with primary translator present:
let rStock = evalLayerFallback('', {'/GUI/Decks/Deck/Layer': {translator: {numEntries: 2, entries: ['A', 'B']}}}, 1, 1);
assert.strictEqual(rStock.visible, true);
assert.strictEqual(rStock.deckLayer, 'B');
"""
            res = subprocess.run(["node", "-e", script], capture_output=True, text=True)
            self.assertEqual(res.returncode, 0, res.stderr)

    def test_display_only_shadow_header_matches_stock_deck_header(self):
        layer = bp.SHADOW_LAYER
        for item in (
            'EngineXEmbedded.TrackDisplayText {',
            'EngineXEmbedded.JP07SyncIndicator {',
            'EngineXEmbedded.Bpm {',
            'EngineXEmbedded.ColoredImage {',
            'AutoLoop {',
            'BeatJump {',
            'SoftTakeoverIndicators {',
            'SpeedInfo {',
            'objectName: "EngineX_TrackEndWarning"',
            'objectName: "Deck_TimeInfo_TextItem"',
            'objectName: "KeyInfo"',
            'objectName: "PerformanceView_Deck_BpmInfo"',
            'color: scHeader.isSync ? Style.active01Color : Style.font02Color',
            'isMaster: scHeader.isLead',
            'Planck.getKeyIndex(rawKey)',
        ):
            self.assertIn(item, layer)
        if shutil.which("node"):
            title_expr = re.search(r'objectName: "Deck_Title".*?text: ([^\n]+)', layer, re.S).group(1)
            artist_vis_expr = re.search(r'objectName: "Deck_Artist".*?visible: ([^\n]+)', layer, re.S).group(1)
            artist_expr = re.search(r'objectName: "Deck_Artist".*?text: ([^\n]+)', layer, re.S).group(1)
            tdt_patch = bp.legacy_qml_fixes()[bp.ROOT + "Items/Utils/TrackDisplayText.qml"]
            tdt_display_expr = tdt_patch[1][1].split("display: ", 1)[1].strip()
            js = f"""
const assert = require('assert');
function evalHeaderText(loaded, hideTitleArtist, showFilename, rawTitle, fileName, artistName, hasBottomRow = false) {{
    const canShowFilename = true;
    const shouldShowFilename = showFilename;
    const title = rawTitle, filename = fileName;
    const display = {tdt_display_expr};
    const scTitleDisplay = {{ hideTitleArtist, shouldShowFilename, display }};
    const scHeader = {{ loaded, rawTitle, fileName, artistName, hasBottomRow, deckLabel: 'Deck 2' }};
    return {{
        titleText: {title_expr},
        artistVisible: {artist_vis_expr},
        artistText: {artist_expr},
    }};
}}
// 4 combinations of Hide Title/Artist (off/on) x Display File Name (off/on):
assert.deepStrictEqual(evalHeaderText(true, false, false, 'My Song', '01_my_song.flac', 'DJ Test'),
    {{ titleText: 'My Song', artistVisible: true, artistText: 'DJ Test' }});
assert.deepStrictEqual(evalHeaderText(true, false, true, 'My Song', '01_my_song.flac', 'DJ Test'),
    {{ titleText: '01_my_song.flac', artistVisible: false, artistText: '' }});
assert.deepStrictEqual(evalHeaderText(true, true, false, 'My Song', '01_my_song.flac', 'DJ Test'),
    {{ titleText: '', artistVisible: false, artistText: '' }});
assert.deepStrictEqual(evalHeaderText(true, true, true, 'My Song', '01_my_song.flac', 'DJ Test'),
    {{ titleText: '', artistVisible: false, artistText: '' }});
// Unloaded fallback:
assert.deepStrictEqual(evalHeaderText(false, true, true, '', '', ''),
    {{ titleText: 'Deck 2', artistVisible: true, artistText: 'No Track Loaded' }});
"""
            res = subprocess.run(["node", "-e", js], capture_output=True, text=True)
            self.assertEqual(res.returncode, 0, res.stderr)
            # F4 & F5: blocker MouseArea stays above scHeaderLoader, forwards only time-slot presses to timeView.flip(),
            # and isLead respects explicit lead=false when master=true.
            self.assertLess(layer.index("id: scHeaderLoader"), layer.index('objectName: "EngineX_ShadowBlocker"'))
            flip_fn = re.search(r"function tryFlipTime\(x, y\) \{.*?\n                \}", layer, re.S).group(0)
            lead_expr = re.search(r"readonly property bool isLead: ([^\n]+)", layer).group(1)
            f4_js = f"""
const assert = require('assert');
let flips = 0;
const timeView = {{ flip() {{ flips++ }} }};
const scTimeSlot = {{ visible: true, x: 323, y: 3, width: 58, height: 27 }};
let loaded = true;
const scDisplay = {{
    performance: false,
    graphical: false,
    sample: null,
    mapToItem(item, x, y) {{ return {{ x: x - item.x, y: y - item.y }} }}
}};
{flip_fn}
// Press inside time slot (including +12px vertical touch padding): flips once
assert.strictEqual(tryFlipTime(350, 16), true);
assert.strictEqual(flips, 1);
assert.strictEqual(tryFlipTime(350, -5), true);
assert.strictEqual(flips, 2);
// Press outside time slot (e.g., on AutoLoop at x=80, y=45 or BeatJump at x=140, y=45): blocked, does not flip
assert.strictEqual(tryFlipTime(80, 45), false);
assert.strictEqual(tryFlipTime(140, 45), false);
assert.strictEqual(flips, 2);
// Unloaded deck: does not flip
loaded = false;
assert.strictEqual(tryFlipTime(350, 16), false);
assert.strictEqual(flips, 2);
// F5: lead=false with master=true must evaluate isLead to false; missing lead falls back to master
function evalIsLead(sample) {{ scDisplay.sample = sample; return {lead_expr}; }}
assert.strictEqual(evalIsLead({{ master: true, lead: false }}), false);
assert.strictEqual(evalIsLead({{ master: true, lead: true }}), true);
assert.strictEqual(evalIsLead({{ master: true }}), true);
assert.strictEqual(evalIsLead({{ master: false }}), false);
"""
            res_f4 = subprocess.run(["node", "-e", f4_js], capture_output=True, text=True)
            self.assertEqual(res_f4.returncode, 0, res_f4.stderr)

    def test_display_only_shadow_lane_and_overview_match_stock(self):
        layer = bp.SHADOW_LAYER
        for item in (
            'readonly property real pitchRate: wave && sample ? Math.max(0.05, 1 + (Number(sample.pitch) || 0) / 100) : 1',
            'readonly property real span: overview ? duration : Math.max(1000, (root.scZoomProp && root.scZoomProp.translator ? root.scZoomProp.translator.unnormalized : 8) * 1000) * pitchRate',
            'id: scOverviewStrip',
            'yStretch: scDisplay.half ? (scDisplay.rgb ? @RGB_SCALE@ : @BANDS_SCALE@) : 1',
            'transform: Scale { origin.y: scOverviewStrip.height; yScale: scOverviewStrip.yStretch }',
            'objectName: "Deck_WaveformOverview_TimeBar"',
            'z: bar ? 1 : -1',
            'color: scBeat.bar ? Style.error01Color : Style.gridlinesColor',
            'color: Style.icon01Color',
            'color: Style.font02Color; style: Text.Outline; styleColor: Style.surface01Color; font.pixelSize: 15; font.weight: Font.Medium',
            'objectName: "HotCueTriangle"',
            'objectName: "HotCueLabel"',
            'Qt.resolvedUrl("./img/cue.png")',
            'Qt.resolvedUrl("./img/icon_cue.png")',
        ):
            self.assertIn(item, layer)
        # Item 15: DeckTabs keeps stock activeDeck and uses greyOut for locked shadow decks
        old_root = bp.ROOT
        try:
            bp.ROOT = "Skins/Embedded/"
            tabs_patches = bp.edits(False)["Skins/EngineOS/UI/CompositeControls/Decks/DeckTabs.qml"]
        finally:
            bp.ROOT = old_root
        self.assertFalse(any("activeDeck:" in old for old, _ in tabs_patches))
        self.assertTrue(any("greyOut:" in old and "root.scDeckLocked" in new for old, new in tabs_patches))

@unittest.skipUnless(shutil.which("node"), "node needed for the display-state behavioral checks")
class ShadowDisplayTest(unittest.TestCase):
    """Run the actual QML JS functions with a fake clock and an Engine that rejects all access."""

    def js(self, body):
        names = ("scDeckLocked", "scShadowVisible", "scShadowDeckFor", "scShadowData", "scShadowRate", "scShadowPosition",
                 "scShadowBeatIndex", "scShadowBeatAt", "scShadowGrid", "scShadowNextCue", "scShadowCuesFor", "scShadowAsset",
                 "scShadowProperties", "scShadowTick", "scShadowPost", "scShadowPoll", "scShadowPollIdle", "scShadowDisplayFetch", "scShadowSameSample", "scShadowApplyDisplay", "scShadowCovers", "scShadowBusy", "scShadowApplyAll", "scNextCueInfo", "scNextCueCompute", "scCueTickFor",
                 "scShadowApply", "scShadowEmpty", "scShadowSpeed", "scShadowCues", "scShadowBeat",
                 "scShadowLoad", "scShadowWrong", "scShadowHolds", "scShadowEndGap", "scShadowEnd",
                 "scShadowSeek", "scShadowLink", "scShadowLinkEdge", "scPress", "scGcCheck", "scShadowWake", "scShadowAnyPlaying",
                 "scShadowLine", "scShadowLandingSeek", "scShadowLandingTick", "scShadowBend", "scShadowBendEnd", "scShadowMasterBack", "scPressWrite", "scMasterPresses",
                 "scLayerPath", "scPairedLanes", "scPairedFollow", "scShadowHeldBy", "scShadowEngineHeld")
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
    scFourDeckRole: '13', scPlayer: true, scShadowDisplayOnly: true, scShadowEngineMask: 0, scShadowLineMask: 0,
    scShadowExpiryMs: 3000, scShadowAt: {}, scShadowDisplay: {}, scShadowEmptyArray: [],
    scShadowDisplayRequests: {}, scShadowDisplayText: {}, scShadowGridCache: {}, scShadowLiveCueCache: {},
    scShadowCdjMaster: false, scPressQueue: [], scMasterOwn: [0, 0, 0, 0, 0], scShadowEmptyObject: {}, scShadowHead: {},
    scShadowMineRole: "?", scShadowMine: [], scShadowSampleSpare: {}, scShadowDeckProperties: {}, scShadowOwn: [],
    scShadowPostRequest: null, scShadowPollRequest: null, scShadowResponseVersion: "", scShadowPostPending: false,
    scShadowLinkId: '', scShadowLinkAt: -Infinity, scShadowTextAt: {},
    scShadowSent: [], scShadowFullAt: 0, scShadowPostFull: true, scShadowLive: {seen: 0},
    scShadowTickTimer: {interval: 40}, scShadowBusyAt: 0, scShadowRoleDirty: true,
    scGcLoadSeq: 0, scGcRuns: 0, scGcLastMs: 0, scGcProps: null,
    scGcState: {armed: true, quietSince: 0, lastRun: -Infinity, loadSig: 0, loadSeq: 0, loadAt: 0, followAt: 0},
    scShadowPrev: {}, scShadowCueCache: {}, scShadowPostAt: 0, scShadowPollAt: now, scShadowDebug: false,
    scShadowActiveDeck: 0, scPairedOn: false, scPairedEmpty: 0,
    scShadowLead: 0, scShadowPeerLead: 0, scShadowPeerPlaying: false, scShadowMasterAt: 0,
    scShadowGen: 0, scShadowVer: 0, scShadowIdle: false, scShadowLinePaths: [], scShadowFrame: 17, scCueInfoCache: {}, scCueInfoMemo: {},
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
assert.equal(legacy[0], 2);
// Engine has not loaded it yet: the slot is covered, not empty.
assert.equal(scShadowData(2).engine, true); assert.equal(scShadowData(2).copying, false);
assert.equal(scShadowVisible(2), true);
scShadowAt[2] = {path: '/old.wav', ready: true};
scShadowApplyAll([{deck: 2, path: '/old.wav'}], false);
assert.equal(scShadowVisible(2), false);
assert.deepEqual(scShadowDisplay, {});
""")

    def test_mixed_slots_and_source_switch(self):
        # Deck 2 = EngineX peer (Engine loads it), Deck 4 = CDJ (display renderer), from one
        # answer; then Deck 2's source becomes the CDJ and goes back.
        self.js(r"""
const writes = [], lookups = [], props = {};
globalThis.scTr = p => { lookups.push(p); return props[p] || (props[p] = {string: 'Off', set state(v) { writes.push(p + '=' + v); }, get state() { return undefined; }, set url(v) { writes.push(p + '=' + v); }}); };
globalThis.scPressTimer = {restart() {}};
const cdj = d => ({deck: d, displayOnly: true, key: 'cdj:x', title: 'CDJ', playing: true, at: now, positionMs: 500});
scShadowApplyAll([{deck: 2, path: '/peer.flac', playing: true}, cdj(4)], false);
assert.equal(scShadowDisplayOnly, false);
assert.equal(scShadowEngineMask, 30 & ~(1 << 4));
assert.deepEqual(writes, ['/Client/Librarian/AddToDatabaseAndLoadToDeck2=file:///peer.flac']);
assert.equal(scShadowData(2).engine, true); assert.equal(scShadowVisible(2), true); assert.equal(scDeckLocked(2), true); // covered while Engine loads
assert.equal(scShadowData(4).title, 'CDJ'); assert.equal(scShadowVisible(4), true);
assert.equal(requests.length, 1); assert(requests[0].url.endsWith('deck=4&key=cdj%3Ax'));
scShadowAt[2].ready = true; // Engine holds the peer's track: the cover leaves
scShadowApplyAll([{deck: 2, path: '/peer.flac', playing: true}, cdj(4)], false);
assert.equal(scShadowData(2), null); assert.equal(scShadowVisible(2), false); assert.equal(scDeckLocked(2), true);
// The display slot is never Engine's: no load, speed, seek or press reaches Deck 4.
scShadowApply({deck: 4, path: '/must-not-load.wav'}); scShadowSpeed(4, 12); scShadowSeek(4, {}, 1000);
scShadowCues(4, {}, {cues: []}, now); scShadowBeat(4, {}, {bpm: 120, beat: 3}, 0, now); scShadowLinkEdge(4, 'peer', 'play', '{"state":true}');
scPress('/Engine/Deck4/Sync');
assert.deepEqual(scPressQueue, []);
assert.equal(lookups.some(p => p.indexOf('Deck4') >= 0), false);
scPress('/Engine/Deck2/Sync'); // the Engine slot still takes presses
assert.equal(scPressQueue.length, 1);
// Deck 2's source becomes the CDJ: the display takes the slot at once, Engine's copy is unloaded after the debounce.
scShadowApplyAll([cdj(2)], false);
assert.equal(scShadowEngineMask, 30 & ~(1 << 2));
assert.equal(scShadowData(2).title, 'CDJ'); assert.equal(scShadowVisible(2), true); assert.equal(scShadowData(4), null);
assert.equal(writes.length, 1);
now += 400;
scShadowApplyAll([cdj(2)], false);
assert.deepEqual(writes.slice(1), ['/Engine/Deck2/Unload=true', '/Engine/Deck2/Track/Unload=true']);
assert.equal(scShadowAt[2], undefined);
now += 400;
scShadowApplyAll([cdj(2)], false);
assert.equal(writes.length, 3); // unloaded once, then left alone
// And back: Engine loads the peer's track again, the display leaves the slot.
scShadowApplyAll([{deck: 2, path: '/peer2.flac'}, cdj(4)], false);
assert.equal(writes[3], '/Client/Librarian/AddToDatabaseAndLoadToDeck2=file:///peer2.flac');
assert.equal(scShadowData(2).engine, true); assert.equal(scShadowData(2).title, ''); // the cover of this load, nothing of the CDJ's picture
scShadowAt[2].ready = true;
scShadowApplyAll([{deck: 2, path: '/peer2.flac'}, cdj(4)], false);
assert.equal(scShadowData(2), null); assert.equal(scShadowVisible(2), false);
assert.equal(scShadowData(4).title, 'CDJ'); assert.equal(scShadowVisible(4), true);
// Forced Engine mode on a console: a CDJ on a Line In deck is flagged for the on-screen hint.
scShadowApplyAll([{deck: 2, path: '/peer2.flac'}, {deck: 4, path: '/cdj.flac', lineIn: true}], false);
assert.equal(scShadowEngineMask, 30); assert.equal(scShadowLineMask, 1 << 4); assert.equal(scShadowData(4), null);
scShadowApplyAll([cdj(4)], true);
assert.equal(scShadowEngineMask, 0); assert.equal(scShadowLineMask, 0);
""")

    def test_shadow_deck_mode_switches_live_201(self):
        # Maintainer request (#201): Shadow Deck Mode no longer restarts Engine. stream-sync
        # reads the mode per request; the QML follows. Switched to All CDJ Mode while Engine
        # holds a shadow track it loaded, that track is stopped and unloaded (once), the
        # display renderer takes the slot, and nothing else touches Engine afterwards.
        e = bp.FOUR_DECK_ENTRIES
        start = e.index("id: scShadowModeEntry")
        entry = e[start:e.index("\n\t\t},", start)]
        self.assertIn('root.scCall("shadowMode=" + ids[index])', entry)
        self.assertNotIn("scRestartCall", entry)
        # An unchanged display reply is applied again while Engine still holds a shadow track.
        self.assertIn("version === root.scShadowResponseVersion && !root.scShadowEngineHeld()", bp.SERVICES_MAIN)
        self.js(r"""
const writes = [], lookups = [], props = {};
globalThis.scTr = p => { lookups.push(p); return props[p] || (props[p] = {string: 'Off', set state(v) { writes.push(p + '=' + v); }, get state() { return p === '/Engine/Deck2/Play' || p === '/Engine/Deck2/Track/SongLoaded' ? true : undefined; }, set url(v) { writes.push(p + '=' + v); }}); };
globalThis.scPressTimer = {restart() {}};
// Auto / All Engine Mode: Engine holds the peer's track on Deck 2.
scShadowApplyAll([{deck: 2, path: '/peer.flac', playing: true}], false);
assert.deepEqual(writes, ['/Client/Librarian/AddToDatabaseAndLoadToDeck2=file:///peer.flac']);
scShadowAt[2].ready = true;
assert.equal(scShadowEngineHeld(), true); assert.equal(scShadowHeldBy(1), false); // this unit's own deck is never a shadow
// Live switch to All CDJ Mode: the slot is stopped at once, the display draws it.
scShadowApplyAll([{deck: 2, key: 'cdj:x', title: 'CDJ', playing: true, at: now, positionMs: 500}], true);
assert.equal(scShadowDisplayOnly, true); assert.equal(scShadowEngineMask, 0);
assert.deepEqual(writes.slice(1), ['/Engine/Deck2/Play=false']);
assert.equal(scShadowData(2).title, 'CDJ');
// The next (unchanged) reply after the debounce unloads it.
now += 400;
scShadowApplyAll([{deck: 2, key: 'cdj:x', title: 'CDJ', playing: true, at: now, positionMs: 500}], true);
assert.deepEqual(writes.slice(2).filter(w => w.indexOf('Unload') >= 0), ['/Engine/Deck2/Unload=true', '/Engine/Deck2/Track/Unload=true']);
assert.equal(scShadowAt[2], undefined); assert.equal(scShadowEngineHeld(), false);
// From then on All CDJ Mode stays display-only: no Engine reads or writes.
const n = writes.length, m = lookups.length;
now += 400;
scShadowApplyAll([{deck: 2, key: 'cdj:x', title: 'CDJ', playing: true, at: now, positionMs: 500}], true);
assert.equal(writes.length, n); assert.equal(lookups.length, m);
// And back to Auto: Engine loads the peer's track again.
scShadowApplyAll([{deck: 2, path: '/peer.flac', playing: true}], false);
assert.equal(writes[n], '/Client/Librarian/AddToDatabaseAndLoadToDeck2=file:///peer.flac');
""")

    def test_live_cdj_switch_drops_cover_meta_and_unsyncs_201(self):
        # Review of 27e31e9: after a live switch to All CDJ Mode the slot leaves the cover of
        # an Engine load (header-only meta, detail: false). The display must start over (no
        # inherited meta, request or cached reply text), else the lane keeps the stretched
        # overview for good. And the UnSync of the slot being unloaded must reach Engine
        # although the display now draws that slot.
        self.js(r"""
const writes = [], props = {};
globalThis.scTr = p => props[p] || (props[p] = {string: p === '/Engine/Deck2/SyncMode' ? 'On' : 'Off', set state(v) { writes.push(p + '=' + v); }, get state() { return p === '/Engine/Deck2/Play' || p === '/Engine/Deck2/Track/SongLoaded' ? true : undefined; }, set url(v) { writes.push(p + '=' + v); }});
globalThis.scPressTimer = {restart() {}};
const coverMeta = {detail: false, header: true};
scShadowDisplay = {2: {key: 'peer:a', engine: true, meta: coverMeta, title: 'A'}};
const coverRequest = scShadowDisplayRequests[2] = {key: 'peer:a', busy: false, started: 0, next: 0, tries: 1};
scShadowDisplayText[2] = {key: 'peer:a', text: '{"detail":false}'};
scShadowAt[2] = {path: '/peer.flac', id: 'peer:a', ready: true};
scShadowApplyAll([{deck: 2, key: 'peer:a', title: 'A', playing: true, at: now, positionMs: 500}], true);
assert.equal(scShadowData(2).engine, false);
assert.equal(scShadowData(2).meta, null); // not the cover's header-only meta
assert.equal(scShadowDisplayText[2], undefined);
assert.notStrictEqual(scShadowDisplayRequests[2], coverRequest); // a fresh fetch, not the cover's
now += 400; // the unload after the debounce: its UnSync goes through although the display draws the slot
scShadowApplyAll([{deck: 2, key: 'peer:a', title: 'A', playing: true, at: now, positionMs: 500}], true);
assert(writes.indexOf('/Engine/Deck2/Unload=true') >= 0, JSON.stringify(writes));
assert(scPressQueue.some(e => e.path === '/Engine/Deck2/UnSync'), JSON.stringify(scPressQueue));
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
globalThis.scShadowApplyAll=(decks,displayOnly)=>{applied++;scShadowDisplayOnly=displayOnly;scShadowEngineMask=displayOnly?0:30;};
scShadowPoll();requests[0].finish(200,{role:'13',displayOnly:false,decks:[],ver:1});
scShadowPoll();requests[0].finish(200,{role:'13',displayOnly:false,decks:[],ver:1});
assert.equal(applied,2);assert.equal(requests.length,1);
header='1:true:13:false:false';
scShadowPoll();requests[0].finish(200,{role:'13',displayOnly:true,decks:[],ver:1});
scShadowPoll();requests[0].finish(200,{role:'13',displayOnly:true,decks:[],ver:1});
assert.equal(applied,3); // display-only can skip identical snapshots
""")

    def test_idle_response_never_changes_mode_or_skips_an_active_snapshot(self):
        self.js(r"""
let headers={}, applied=[], emptied=[];
globalThis.scShadowPollTimer={interval:0,restart(){}};
XMLHttpRequest.prototype.getResponseHeader=k=>headers[k];
globalThis.scShadowApplyAll=(decks,displayOnly)=>{applied.push(decks);scShadowDisplayOnly=displayOnly;};
globalThis.scShadowEmpty=d=>emptied.push(d);
const reply=(h,r)=>{headers=h;scShadowPoll();requests[0].finish(200,r);};
reply({'X-Shadow-Idle':'1'},{role:'13',displayOnly:false,decks:[],ver:7});
assert.equal(applied.length,1);
reply({'X-Shadow-Unchanged':'1'},null); // the body must not be needed
assert.match(requests[0].url,/idle=1/);
reply({}, {unchanged:true,displayOnly:true}); // header stripped; even a mode field is ignored
assert.equal(scShadowDisplayOnly,false);
assert.deepEqual(emptied,[2,4,2,4]); // delayed unload keeps ticking
reply({}, {role:'13',displayOnly:false,decks:[{deck:2,path:'/copy',seq:2}],ver:7});
assert.equal(applied.length,2);
assert.equal(applied[1][0].seq,2);
assert.equal(scShadowIdle,false);
assert.equal(scShadowPollTimer.interval,60);
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

    def test_paired_lanes_follow_both_layers_201(self):
        # #201: lane 1 is this unit's active layer, lane 2 the other player's active deck.
        self.js(r"""
const L = (role, layer, decks) => scPairedLanes(role, layer, decks);
const peer = (a, b, active) => [{deck:a, key:'x', active: active === a}, {deck:b, active: active === b}];
// Role 1+3 (odd player): own side Left, the other player's 2/4 on the Right.
assert.deepEqual(L('13', 0, peer(2, 4, 2)), {lane1:1, lane2:2, side:'/GUI/Decks/DeckRight/Layer', layer:0, missing:false});
assert.deepEqual(L('13', 1, peer(2, 4, 2)), {lane1:3, lane2:2, side:'/GUI/Decks/DeckRight/Layer', layer:0, missing:false});
assert.deepEqual(L('13', 0, peer(2, 4, 4)), {lane1:1, lane2:4, side:'/GUI/Decks/DeckRight/Layer', layer:1, missing:false});
assert.deepEqual(L('13', 1, peer(2, 4, 4)), {lane1:3, lane2:4, side:'/GUI/Decks/DeckRight/Layer', layer:1, missing:false});
// Role 2+4 (even player): the mirror image.
assert.deepEqual(L('24', 0, peer(1, 3, 1)), {lane1:2, lane2:1, side:'/GUI/Decks/DeckLeft/Layer', layer:0, missing:false});
assert.deepEqual(L('24', 1, peer(1, 3, 3)), {lane1:4, lane2:3, side:'/GUI/Decks/DeckLeft/Layer', layer:1, missing:false});
// The other player's active layer is empty: lane 2 still follows it.
assert.equal(L('13', 0, [{deck:2, key:'x'}, {deck:4, active:true}]).lane2, 4);
// No active flag (a CDJ, an older EngineX): the deck with a track, else Layer A.
assert.equal(L('13', 0, [{deck:2}, {deck:4, key:'cdj:1'}]).lane2, 4);
assert.equal(L('24', 0, [{deck:1}, {deck:3}]).lane2, 1);
// Our own decks in the reply never count as the other player's.
assert.equal(L('13', 0, [{deck:1, active:true}, {deck:3, key:'x'}]).missing, true);
// No other player: lane 2 is the side's Layer A deck, marked missing.
assert.deepEqual(L('13', 1, []), {lane1:3, lane2:2, side:'/GUI/Decks/DeckRight/Layer', layer:0, missing:true});
assert.deepEqual(L('24', 0, []), {lane1:2, lane2:1, side:'/GUI/Decks/DeckLeft/Layer', layer:0, missing:true});
// Single-deck roles pair the same way; 1+2 and 3+4 have no pair (they keep the 4-deck view).
assert.equal(L('1', 1, peer(2, 4, 4)).lane1, 1); assert.equal(L('1', 0, peer(2, 4, 4)).lane2, 4);
assert.equal(L('2', 0, peer(1, 3, 3)).lane1, 2); assert.equal(L('2', 0, peer(1, 3, 3)).lane2, 3);
assert.equal(L('12', 0, peer(3, 4, 3)), null); assert.equal(L('34', 0, peer(1, 2, 1)), null); assert.equal(L('', 0, []), null);
""")

    def test_paired_follow_moves_only_the_other_side_201(self):
        self.js(r"""
const layers = {'/GUI/Decks/DeckLeft/Layer': {index: 1}, '/GUI/Decks/DeckRight/Layer': {index: 0}};
const writes = [];
for (const [path, t] of Object.entries(layers)) { let i = t.index; Object.defineProperty(t, 'index', {get(){return i}, set(v){writes.push(path + '=' + v); i = v}}) }
// Off (the 4-deck view, every existing test): no Engine access at all on the display path.
scShadowApplyAll([{deck:2, key:'x', active:true}], true);
globalThis.scTr = p => layers[p] || null;
scPairedOn = true; scFourDeckRole = '13';
scShadowApplyAll([{deck:2, key:'x'}, {deck:4, active:true}], true);
assert.deepEqual(writes, ['/GUI/Decks/DeckRight/Layer=1']); // the other player went to Layer B
assert.equal(layers['/GUI/Decks/DeckLeft/Layer'].index, 1); // our own Layer B untouched
scShadowApplyAll([{deck:2, key:'x'}, {deck:4, active:true}], true);
assert.equal(writes.length, 1); // unchanged: no write
assert.equal(scPairedEmpty, 0);
scShadowApplyAll([], true); // the other player is gone
assert.equal(scPairedEmpty, 2); assert.equal(layers['/GUI/Decks/DeckRight/Layer'].index, 0);
scShadowApplyAll([{deck:2, active:true}, {deck:4}], true);
assert.equal(scPairedEmpty, 0);
// A 2+4 player moves the Left side, never its own Right one.
writes.length = 0; scFourDeckRole = '24';
scShadowApplyAll([{deck:1, active:true}, {deck:3, key:'y'}], true);
assert.deepEqual(writes, ['/GUI/Decks/DeckLeft/Layer=0']);
scShadowApplyAll([{deck:1}, {deck:3, key:'y', active:true}], true);
assert.deepEqual(writes, ['/GUI/Decks/DeckLeft/Layer=0', '/GUI/Decks/DeckLeft/Layer=1']);
assert(!writes.some(w => w.startsWith('/GUI/Decks/DeckRight')), writes.join());
""")

    def test_sender_posts_its_active_layer_at_once_201(self):
        self.js(r"""
let bodies=[], pings=0, layer=0;
globalThis.XMLHttpRequest=class {
    constructor(){this.readyState=0;}
    open(m,url){this.url=url;this.readyState=1;}
    send(body){if(this.url.endsWith('/alive'))pings++;else bodies.push(JSON.parse(body));this.readyState=4;this.status=200;}
};
globalThis.scShadowPoll=()=>{};
scFourDeckRole='13';
globalThis.scTr=path=>path==='/GUI/Decks/DeckLeft/Layer'?{get index(){return layer}}:({
    get value(){return path.endsWith('/SampleRate')?48000:0}, get unnormalized(){return 0},
    get state(){return path.endsWith('/SongLoaded')}, get string(){return path.endsWith('/TrackUri')?'file:///a.mp3':''}, color:'#ffffff'});
const tick=ms=>{for(let t=0;t<ms;t+=40){now+=40;scShadowTick();}};
tick(1000); assert.equal(bodies.length,1);
assert.deepEqual(bodies[0].map(o=>[o.deck,o.active]), [[1,true],[3,false]]);
layer=1; tick(40); assert.equal(bodies.length,2,'a Layer press posts at once');
assert.deepEqual(bodies[1].map(o=>[o.deck,o.active]), [[1,false],[3,true]]);
tick(2000); assert.equal(bodies.length,2,'no layer change: pings only');
layer=0; tick(40); assert.equal(bodies.length,3); assert.equal(bodies[2][0].active,true);
""")

    def test_entering_paired_player_forces_the_next_shadow_apply_201(self):
        # Review of 27e31e9: Linked View 4 Decks -> Paired Player (live) must show lane 2 /
        # "No paired player" at once, also when the peer is unchanged in All CDJ Mode or
        # the poll is idle: the next /shadow answer goes through scShadowApplyAll.
        line = next(l for l in bp.SERVICES_MAIN.splitlines() if l.lstrip().startswith("onScPairedOnChanged:"))
        body = line.split(":", 1)[1]
        self.assertIn('/v1/sc5000/shadow?ver=" + scShadowVer + (scShadowIdle ? "&idle=1" : "")', Path(bp.__file__).read_text(encoding="utf-8"))
        if not shutil.which("node"):
            self.skipTest("node not installed")
        script = f"""
const assert = require('assert');
function run(on) {{
  const o = {{ scPairedOn: on, scPairedDirty: false, scPairedEmpty: 3, scShadowIdle: true, scShadowResponseVersion: '7:true:13:false:false:ab', scShadowVer: 7 }};
  with (o) {body}
  return o;
}}
const on = run(true);
assert.equal(on.scShadowIdle, false); assert.equal(on.scShadowResponseVersion, ''); assert.equal(on.scShadowVer, 0); assert.equal(on.scPairedDirty, true);
const off = run(false);
assert.equal(off.scPairedEmpty, 0); assert.equal(off.scShadowIdle, true); assert.equal(off.scShadowVer, 7);
"""
        res = subprocess.run(["node", "-e", script], capture_output=True, text=True)
        self.assertEqual(res.returncode, 0, res.stderr)
        # "Paired Player" does not fit the stock 136 entry width.
        e = bp.FOUR_DECK_ENTRIES
        start = e.index("id: scLinkedViewEntry")
        self.assertIn("entryWidth: 176", e[start:e.index("\n\t\t},", start)])

    def test_linked_view_selector_returns_to_stock_201(self):
        # Maintainer report: with Paired Player on there was no visible way back to the
        # stock view (that was a second switch, 4-Deck Waveform). One selector now holds
        # Stock / Paired Player / 4 Decks, and Stock always turns the 4-deck layout off.
        e = bp.FOUR_DECK_ENTRIES
        self.assertNotIn('text: "4-Deck Waveform"', e)
        self.assertEqual(e.count('text: "Paired Player"'), 0)
        self.assertEqual(e.count('text: "Linked View"'), 1)
        self.assertNotIn("Waveform View", e)
        # Under Stock a player gets one grey line on its own single / dual layer switches.
        hint = re.search(r"@HEADLINE@ \{ // Stock on a player.*?\n\t\t\},", e, re.S).group(0)
        self.assertIn("visible: root.scIsPlayerBoard && scLinkedViewEntry.at === 0", hint)
        self.assertIn('text: "Single / Dual layer: Shift + Layer, or Layout > PERFORMANCE LAYOUT > Deck Layout"', hint)
        at = re.search(r"readonly property int at: (.*)\n", e).group(1)
        values = re.search(r"\n\t\t\tvalues: (root\.scIsPlayerBoard \?.*)\n", e).group(1)
        body = re.search(r"onSelected: function\(index\) \{\n(.*?)\n\t\t\t\}", e, re.S).group(1)
        rows = {l.split("\t")[0]: l.split("\t")[1:] for l in (REPO / "tools/ui-mod/i18n.tsv").read_text(encoding="utf-8").splitlines() if l and not l.startswith("#")}
        for en in ("Linked View", "Stock", "Paired Player", "4 Decks", "Single / Dual layer: Shift + Layer, or Layout > PERFORMANCE LAYOUT > Deck Layout"):
            self.assertEqual(len(rows.get(en, [])), 2, en)
        self.assertEqual(rows["Linked View"], ["联机视图", "聯機視圖"])
        self.assertEqual(rows["Stock"], ["原厂", "原廠"])
        # The hint names the stock menu with the stock labels of each language.
        for i, (layout, perf, deck, single) in enumerate(zip(rows["Layout"], rows["PERFORMANCE LAYOUT"], rows["Deck Layout"], rows["Single"])):
            line = rows["Single / Dual layer: Shift + Layer, or Layout > PERFORMANCE LAYOUT > Deck Layout"][i]
            self.assertIn(f"{layout} > {perf} > {deck}", line)
            self.assertTrue(line.startswith(single), line)
        if not shutil.which("node"):
            self.skipTest("node not installed")
        script = f"""
const assert = require('assert');
function run(player, fourDeck, paired, index) {{
  const calls = [];
  const root = {{ scIsPlayerBoard: player, scFourDeckOn: fourDeck, scPairedSetting: paired,
    scRestartCall: q => calls.push('restart ' + q), scCall: q => calls.push('call ' + q) }};
  const values = {values};
  const at = {at};
  if (index !== undefined) (function(index) {{
{body}
  }})(index);
  return {{ shown: values[at], values, calls }};
}}
// Standalone player: three modes.
assert.deepStrictEqual(run(true, false, false).values, ['Stock', 'Paired Player', '4 Decks']);
assert.strictEqual(run(true, false, false).shown, 'Stock');
assert.strictEqual(run(true, true, true).shown, 'Paired Player');
assert.strictEqual(run(true, true, false).shown, '4 Decks');
// The bug: from Paired Player (or 4 Decks) back to stock in one step, Engine restarts.
assert.deepStrictEqual(run(true, true, true, 0).calls, ['restart fourDeckWaveform=0']);
assert.deepStrictEqual(run(true, true, false, 0).calls, ['restart fourDeckWaveform=0']);
// From stock into either mode is a restart; between the two 4-deck-layout modes it is not.
assert.deepStrictEqual(run(true, false, false, 1).calls, ['restart pairedView=1']);
assert.deepStrictEqual(run(true, false, false, 2).calls, ['restart fourDeckWaveform=1']);
assert.deepStrictEqual(run(true, true, true, 2).calls, ['call pairedView=0']);
assert.deepStrictEqual(run(true, true, false, 1).calls, ['call pairedView=1']);
// Re-selecting the shown mode does nothing.
assert.deepStrictEqual(run(true, true, true, 1).calls, []);
assert.deepStrictEqual(run(true, false, false, 0).calls, []);
// All-in-one with 4 decks: no Paired Player, a stale paired flag is ignored.
assert.deepStrictEqual(run(false, true, true).values, ['Stock', '4 Decks']);
assert.strictEqual(run(false, true, true).shown, '4 Decks');
assert.deepStrictEqual(run(false, true, false, 0).calls, ['restart fourDeckWaveform=0']);
assert.deepStrictEqual(run(false, false, false, 1).calls, ['restart fourDeckWaveform=1']);
"""
        res = subprocess.run(["node", "-e", script], capture_output=True, text=True)
        self.assertEqual(res.returncode, 0, res.stderr)

    def test_paired_player_entry_and_lane_order_201(self):
        self.assertIn('values: root.scIsPlayerBoard ? ["Stock", "Paired Player", "4 Decks"] : ["Stock", "4 Decks"]', bp.FOUR_DECK_ENTRIES)
        self.assertIn("visible: root.scIsPlayerBoard", bp.FOUR_DECK_ENTRIES)
        self.assertIn("scPairedSetting = !!v.fourDeckWaveform && !!v.pairedView", bp.SERVICES_MAIN)
        self.assertIn("if (scPairedOn) scPairedFollow(decks)", bp.SERVICES_MAIN)
        # Both hint lines go through translate(): no comparison on their text line.
        hint = next(l for l in bp.FOUR_DECK_ENTRIES.splitlines() if "Paired Player needs" in l)
        self.assertNotIn("==", hint)
        table = {"Paired Player needs 4-Deck Role Auto, 1+3, 2+4, 1 or 2": "X", "Paired Player: this player's active layer and the other player's active deck, one lane each": "Y"}
        self.assertIn('["Y", "X"]', bp.translate(hint, table, set()))
        rows = {l.split("\t")[0]: l.split("\t")[1:] for l in (REPO / "tools/ui-mod/i18n.tsv").read_text(encoding="utf-8").splitlines() if l and not l.startswith("#")}
        for en in ("Paired Player", "No paired player", "Paired Player: this player's active layer and the other player's active deck, one lane each",
                   "Paired Player needs 4-Deck Role Auto, 1+3, 2+4, 1 or 2"):
            self.assertIn(en, rows)
            self.assertEqual(len(rows[en]), 2)
        old_root = bp.ROOT
        try:
            bp.ROOT = "Skins/Embedded/"
            patches = bp.edits(False)["Skins/EngineOS/UI/CompositeControls/PerformanceView/MainViewResponsiveWaveforms.qml"]
        finally:
            bp.ROOT = old_root
        # Swapped only by Paired Player on a 2+4 player; the stock swap and the 4-deck view stay stock.
        self.assertEqual(sum(new.count("responsiveWaveform.scDecksSwapped") for _, new in patches), 4)
        self.assertIn("deckControl.decksSwapped || (typeof root !== \"undefined\" && !!root && !!root.scPairedSwap)", patches[0][1])
        self.assertIn('readonly property bool scPairedSwap: scPairedOn && (scFourDeckRole === "24" || scFourDeckRole === "2")', bp.SERVICES_MAIN)
        self.assertIn("readonly property bool scPairedOn: scPairedSetting && scIsPlayerBoard &&", bp.SERVICES_MAIN)
        # The lane label rides on every waveform lane, shown only for lane 2 without a paired player.
        layer = bp.shadow_layer("import QtQuick 2.0\nItem {\n    id: lane\n    property int deckIndex: 1\n}\n", "waveform")
        self.assertIn('objectName: "EngineX_PairedEmpty"', layer)
        self.assertIn("root.scPairedEmpty > 0 && root.scPairedEmpty === scDisplay.deck", layer)
        self.assertNotIn("EngineX_PairedEmpty", bp.shadow_layer("import QtQuick 2.0\nItem {\n    id: lane\n}\n", "overview"))

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

    def test_gc_check_initializes_and_collects_on_two_deck_hardware(self):
        self.js(r"""
let runs = 0, plays = [false, false], cmds = [false, false], lengths = [0, 0];
globalThis.gc = () => runs++;
// SC5000 / SC6000: only Decks 1 and 2 exist in Engine's property tree; Decks 3 and 4 return null.
globalThis.scTr = path => {
    const d = +path[12] - 1;
    if (d >= 2) return null;
    return path.endsWith('/PlayState') ? {get state(){ return plays[d]; }}
         : path.endsWith('/Play') ? {get state(){ return cmds[d]; }}
         : {get unnormalized(){ return lengths[d]; }};
};
scGcProps = null;
scGcState = {armed: true, quietSince: 0, lastRun: -Infinity, loadSig: 0, loadSeq: 0, loadAt: 0, followAt: 0};
assert.equal(scGcProps, null);
now += 500; scGcCheck();
assert.notEqual(scGcProps, null, "scGcProps must initialize on 2-deck hardware");
assert.equal(runs, 0); // 1.5 s settle has not elapsed yet
now += 1500; scGcCheck();
assert.equal(runs, 1, "startup gc() must run after 1.5s quiet settle on 2-deck hardware");
for (let i = 0; i < 20; i++) { now += 500; scGcCheck(); }
assert.equal(runs, 1, "startup gc() runs only once, not repeatedly");
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
                            self.assertIn("scShadowApplyDisplay(decks.filter(e => e.displayOnly === true).concat(scShadowCovers(decks)))", main)
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

            stems_row = Path(tmp) / "packs" / "en" / "Skins" / "Embedded" / "Views" / "ControlCenter" / "Tabs" / "Preferences" / "Entries" / "StemsDownload.qml"
            self.assertTrue(stems_row.is_file(), f"{stems_row} not found")
            stems_row_text = stems_row.read_text()
            self.assertIn("active: false", stems_row_text)
            self.assertNotIn("manager.canModifyInstall", stems_row_text)

            main = Path(tmp) / "packs" / "en" / "Skins" / "Embedded" / "EmbeddedMain.qml"
            self.assertTrue(main.is_file(), f"{main} not found")
            main_text = main.read_text()
            self.assertIn("parent: Overlay.overlay ? Overlay.overlay : uiContainer\n\t\t\tanchors.fill: parent\n", main_text)
            self.assertNotIn("parent: Overlay.overlay ? Overlay.overlay : uiContainer\n\t\t\twidth: planckQuickView.defaultScreenSize.width", main_text)

            # #137: verify MIDI bridge timer is stopped by default and scMidiApplyOps drives mock translators
            self.assertIn("id: scMidiPollTimer\n\t\tinterval: 16; running: false; repeat: true", main_text)
            midi_fn = re.search(r"(function scMidiApplyOps\(r\) \{.*?\n\t\})", main_text, re.S)
            self.assertIsNotNone(midi_fn)
            midi_script = (
                "const assert=require('node:assert/strict');\n"
                "let scMidiLastAckSeq=0; const trMap={};\n"
                "const root={scTr(p){if(!trMap[p])trMap[p]={state:false,value:0,index:0,entries:[0,1,2,3],incCalls:[],decCalls:[],setCalls:[],incValue(...a){this.incCalls.push(a)},decValue(...a){this.decCalls.push(a)},setValue(...a){this.setCalls.push(a)}};return trMap[p];}};\n"
                + midi_fn.group(1) + "\n"
                "scMidiApplyOps({seq:7,targetDeck:3,values:{vinylMode:true,jogTouch:true,jogMoveTicks:9600,nudgeFwd:0.4,nudgeBwd:0,speed:0.62,speedOffsetUp:true,speedOffsetDown:false,speedNeutral:false,cueHold:true},actions:[{action:'toggle_play'},{action:'cycle_tempo_range'},{action:'load_selected_track'},{action:'browse_scroll',steps:2}]});\n"
                "assert.equal(scMidiLastAckSeq,7);\n"
                "assert.equal(trMap['/Engine/Deck3/ExternalScratchWheelScratchEnable'].state,true);\n"
                "assert.equal(trMap['/Engine/Deck3/ExternalScratchWheelTouch'].value,1.0);\n"
                "assert.equal(trMap['/Engine/Deck3/ExternalScratchWheelMove'].incCalls[0][0],9600);\n"
                "assert.deepEqual(trMap['/Engine/Deck3/Speed'].setCalls[0],[0.62,1,0]);\n"
                "assert.equal(trMap['/Engine/Deck3/SpeedOffsetUp'].state,true);\n"
                "assert.equal(trMap['/Engine/Deck3/Track/CueAction'].value,1.0);\n"
                "assert.equal(trMap['/Engine/Deck3/Play'].state,true);\n"
                "assert.equal(trMap['/Engine/Deck3/SpeedRange'].index,1);\n"
                "assert.equal(trMap['/Client/Librarian/LoadToDeck3'].state,true);\n"
            )
            midi_run = subprocess.run(["node", "-e", midi_script], capture_output=True, text=True)
            self.assertEqual(midi_run.returncode, 0, midi_run.stderr)


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
        self.assertIn('Rectangle { visible: scBeat.bar; x: 1; y: 8; width: 2; height: scBeat.height - 16; color: Style.icon01Color }', layer)
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
        bounded = bp.cue_capsule("", "1", 22, 16)
        self.assertNotIn("fillWidth", bounded)
        self.assertIn("Layout.minimumWidth: 112", bounded)
        self.assertIn("Layout.preferredWidth: 112", bounded)
        self.assertIn("Layout.maximumWidth: 112", bounded)
        flex = bp.cue_capsule("", "1", 22, 16, fill_width=True)
        self.assertIn("Layout.fillWidth: true", flex)
        self.assertIn("Layout.minimumWidth: 50", flex)
        self.assertNotIn("Layout.maximumWidth", flex)
        for cap in (bounded, flex):
            self.assertIn("Layout.leftMargin: 3", cap)
            self.assertIn("Layout.rightMargin: 2", cap)
            self.assertIn("clip: true", cap)
            self.assertIn("parent.width - Math.ceil(cueCountText.implicitWidth) - 11", cap)
            self.assertIn("elide: Text.ElideRight", cap)
            self.assertIn("Reserve the Cue slot even without a cue", cap)

    def test_split_mini_and_main_keep_bounded_cue_and_stock_title_layout(self):
        qrc = LAB / "5.3.0" / "qrc"
        if not qrc.is_dir():
            self.skipTest(f"{qrc} not found (run on Oracle-Batam-arm)")
        old_root = bp.ROOT
        try:
            bp.ROOT = "Skins/Embedded/"
            edits = bp.edits(False)
        finally:
            bp.ROOT = old_root
        composite = "Skins/EngineOS/UI/CompositeControls/"
        split_rel = composite + "DeckTrackData/SplitMiniDeckTrackDataContent.qml"
        stock_split = (qrc / split_rel).read_text()
        patched_split = stock_split
        for old, new in edits[split_rel]:
            patched_split = bp.patch(patched_split, old, new, split_rel)
        # ArtistSong block in SplitMini must remain byte-for-byte stock.
        artist_pat = r"(?m)^    ArtistSong \{.*?^    \}"
        self.assertEqual(re.search(artist_pat, patched_split, re.S).group(),
                         re.search(artist_pat, stock_split, re.S).group())
        # Nothing after ArtistSong in SplitMini may set fillWidth: true (which would steal title width).
        after_artist = patched_split[re.search(artist_pat, patched_split, re.S).end():]
        self.assertNotIn("Layout.fillWidth: true", after_artist)
        self.assertIn("Layout.preferredWidth: 112\n            Layout.maximumWidth: 112", after_artist)
        # 2-deck Main / SplitMain (DeckPerformanceDataContent) cue capsule is also bounded (no fillWidth).
        perf_rel = composite + "DeckPerformanceData/DeckPerformanceDataContent.qml"
        perf_cue = next(new for _, new in edits[perf_rel] if "EngineX_CueCountdown" in new)
        self.assertIn("Layout.preferredWidth: 112\n        Layout.maximumWidth: 112", perf_cue)
        self.assertNotIn("Layout.fillWidth: true", perf_cue.split("Item {\n        id: spacer2")[0])
        # Only the 4-deck grouped header sets fillWidth: true on cue_capsule.
        mini_rel = composite + "DeckTrackData/GroupedMiniDeckTrackDataContent.qml"
        mini_cue = next(new for _, new in edits[mini_rel] if "EngineX_CueCountdown" in new)
        self.assertIn("Layout.fillWidth: true", mini_cue)

    def test_autoloop_beatjump_and_speedinfo_preserve_safe_widths(self):
        old_root = bp.ROOT
        try:
            bp.ROOT = "Skins/Embedded/"
            edits = bp.edits(False)
        finally:
            bp.ROOT = old_root
        # SpeedInfo measures worst-case strings ("±100" and "-100.00%") via TextMetrics
        # rather than live implicitWidth, and keeps stock minimum widths via Math.max.
        speed_patches = edits["Skins/EngineOS/UI/CompositeControls/Deck/DeckItem/SpeedInfo.qml"]
        self.assertFalse(any("columnSpacing" in old for old, _ in speed_patches))
        self.assertTrue(any('text: "\\u00b1100"' in new and 'text: "-100.00%"' in new for _, new in speed_patches))
        self.assertTrue(any("Math.max(speedRangeTextWidth, Math.ceil(scRangeWorstMetrics.width))" in new for _, new in speed_patches))
        self.assertTrue(any("Math.max(speedStateTextWidth, Math.ceil(scStateWorstMetrics.width))" in new for _, new in speed_patches))
        self.assertFalse(any("implicitWidth" in new for _, new in speed_patches))
        for item in ("AutoLoop", "BeatJump"):
            item_patches = edits[f"Skins/EngineOS/UI/CompositeControls/Deck/DeckItem/{item}.qml"]
            # Stock component roots must not set Layout.fillWidth: false globally;
            # fillWidth: false is scoped to our compact call sites, and inner text
            # fillWidth/minimumWidth:0 only activates at compact preferredWidth <= 64.
            self.assertFalse(any("Layout.fillWidth: false" in new for _, new in item_patches), item)
            self.assertTrue(any("parent.Layout.preferredWidth > 0 && parent.Layout.preferredWidth <= 64" in new and "fontSizeMode: Text.HorizontalFit" in new for _, new in item_patches), item)
        for m in re.finditer(r"speed(?:Range|State)TextWidth:\s*(\d+)", bp.SHADOW_LAYER):
            self.assertEqual(int(m.group(1)), 0)
        for m in re.finditer(r"AutoLoop\s*\{([^}]*?)\}", bp.SHADOW_LAYER, re.S):
            self.assertIn("Layout.fillWidth: false", m.group(1))
            self.assertGreaterEqual(int(re.search(r"Layout\.preferredWidth:\s*(\d+)", m.group(1)).group(1)), 52)
        for m in re.finditer(r"BeatJump\s*\{([^}]*?)\}", bp.SHADOW_LAYER, re.S):
            self.assertIn("Layout.fillWidth: false", m.group(1))
            self.assertGreaterEqual(int(re.search(r"Layout\.preferredWidth:\s*(\d+)", m.group(1)).group(1)), 64)
        for rel in (
            "Skins/EngineOS/UI/CompositeControls/DeckTrackData/GroupedMiniDeckTrackDataContent.qml",
            "Skins/EngineOS/UI/CompositeControls/DeckPerformanceData/DeckPerformanceDataContent.qml",
            "Skins/EngineOS/UI/CompositeControls/DeckTrackData/SplitMiniDeckTrackDataContent.qml",
        ):
            joined = "\n".join(new for _, new in edits[rel])
            self.assertIn("speedRangeTextWidth: 0", joined, rel)
            self.assertIn("speedStateTextWidth: 0", joined, rel)
            self.assertGreaterEqual(joined.count("Layout.fillWidth: false"), 3, rel)
            for m in re.finditer(r"speed(?:Range|State)TextWidth:\s*(\d+)", joined):
                self.assertEqual(int(m.group(1)), 0, rel)
            for m in re.finditer(r"(?:AutoLoop|target:\s*autoLoop)\b[^}]*?Layout\.preferredWidth:\s*(\d+)", joined, re.S):
                self.assertGreaterEqual(int(m.group(1)), 52, rel)
            for m in re.finditer(r"(?:BeatJump|target:\s*beatJump)\b[^}]*?Layout\.preferredWidth:\s*(\d+)", joined, re.S):
                self.assertGreaterEqual(int(m.group(1)), 64, rel)

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
                    self.assertIn("id: autoLoop\n        Layout.fillWidth: false\n        Layout.rightMargin: 4", performance)
                    self.assertIn("id: beatJump\n        Layout.fillWidth: false", performance)
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
                    # SpeedInfo preserves stock minimum widths via Math.max(speed*TextWidth, Math.ceil(implicitWidth))
                    # while compact rows set speed*TextWidth: 0, JP07SyncIndicator keeps a fixed 135 px width,
                    # and AutoLoop/BeatJump keep 52/64 px.
                    mini = (base / composite / "DeckTrackData/GroupedMiniDeckTrackDataContent.qml").read_text()
                    speed = (base / composite / "Deck/DeckItem/SpeedInfo.qml").read_text()
                    self.assertIn("Layout.fillWidth: false\n                    Layout.minimumWidth: 135\n                    Layout.preferredWidth: 135\n                    Layout.maximumWidth: 135", mini)
                    self.assertIn("speedRangeTextHorizontalAlignment: Text.AlignRight", mini)
                    self.assertNotIn("speedRangeTextHorizontalAlignment: Text.AlignLeft", mini)
                    self.assertIn("speedRangeTextHorizontalAlignment: Text.AlignRight", performance)
                    self.assertNotIn("speedRangeTextHorizontalAlignment: Text.AlignLeft", performance)
                    self.assertNotIn("speedRangeTextHorizontalAlignment: Text.AlignLeft", bp.SHADOW_LAYER)
                    self.assertIn("speedRangeTextWidth: 0", mini)
                    self.assertIn("speedStateTextWidth: 0", mini)
                    self.assertIn("speedRangeTextWidth: 0", performance)
                    self.assertIn("speedStateTextWidth: 0", performance)
                    self.assertIn("columnSpacing: 16", speed)
                    self.assertIn('TextMetrics { id: scRangeWorstMetrics; font: speedRangeText.font; text: "\\u00b1100" }', speed)
                    self.assertIn('TextMetrics { id: scStateWorstMetrics; font: speedStateText.font; text: "-100.00%" }', speed)
                    self.assertIn("speedRangeTextWidth > 0 ? speedRangeTextWidth : Math.max(speedRangeTextWidth, Math.ceil(scRangeWorstMetrics.width))", speed)
                    self.assertIn("speedStateTextWidth > 0 ? speedStateTextWidth : Math.max(speedStateTextWidth, Math.ceil(scStateWorstMetrics.width))", speed)
                    # Unpatched stock layouts (DeckOverview, DeckSharedItems, GroupedMainDeckLayout,
                    # StackedMainDeckLayout, StackedMiniDeckLayout) stay untouched vs 1de13b7.
                    for unpatched in (
                        "Deck/DeckLayout/GroupedMainDeckLayout.qml",
                        "Deck/DeckLayout/StackedMainDeckLayout.qml",
                        "Deck/DeckLayout/StackedMiniDeckLayout.qml",
                    ):
                        self.assertFalse((base / composite / unpatched).exists(), unpatched)
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
    def test_bpm_info_and_bpm_item_update_with_pitch_fader_before_waveform_analyzed(self):
        # Issue #231: Rekordbox / unanalyzed track loaded on deck (e.g. 125 BPM).
        # While the waveform is loading, SongAnalyzed is false. When the user moves the
        # pitch fader, the displayed BPM must update in real time from exact pitch
        # (deckData.speedState, not 1-decimal SpeedState.translator.string) and must not
        # jump when waveform loading completes.
        qrc = LAB / "5.3.0" / "qrc"
        if not qrc.is_dir():
            self.skipTest(f"{qrc} not found")
        with tempfile.TemporaryDirectory(prefix="ui-header-bpm-test-") as tmp:
            packs = Path(tmp) / "packs"
            result = subprocess.run(["python3", str(BUILD_PACKS), "--qrc", str(qrc), "--out", str(packs)],
                                    capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            composite = "Skins/EngineOS/UI/CompositeControls/"
            for lang in ("en", "zh-Hans", "zh-Hant"):
                info_lang = (packs / lang / composite / "Deck/DeckItem/BpmInfo.qml").read_text()
                bpm_lang = (packs / lang / "Skins/Embedded/Items/Bpm.qml").read_text()
                self.assertIn("scDeckView.deckData", info_lang, lang)
                self.assertIn("scDeckData.speedState", info_lang, lang)
                self.assertIn("OriginalBPM", info_lang, lang)
                self.assertIn("scDeckData.speedState", bpm_lang, lang)
                self.assertIn("OriginalBPM", bpm_lang, lang)

            info = (packs / "en" / composite / "Deck/DeckItem/BpmInfo.qml").read_text()
            m_info = re.search(r"readonly property var scBpm2: \{(.*?)\n    \}", info, re.S)
            self.assertIsNotNone(m_info, "BpmInfo.qml must compute scBpm2 dynamically before SongAnalyzed")
            # Simulate Engine's real SpeedState StringTranslator ("%+.1f%", 1 decimal, no .unnormalized)
            # alongside exact C++ deckData.speedState.
            script = r"""
            const assert = require('assert');
            function fmtSpeedState(p) {
                return (p >= 0 ? "+" : "") + p.toFixed(1) + "%";
            }
            function makeBpmInfo(isAnalyzed, currentBpm, pitchArg, metaBpmStr, bpmText, bpmFractionalText, opts = {}) {
                const isLoaded = opts.loaded !== undefined ? opts.loaded : !!(bpmText && bpmText !== "-");
                const origBpm = opts.origBpm !== undefined ? opts.origBpm : 0;
                const bpmInfoView = { bpmText: bpmText, bpmFractionalText: bpmFractionalText };
                const scDeckData = opts.deckData !== undefined ? opts.deckData : { songLoaded: isLoaded, speedState: pitchArg };
                const scLoaded = { translator: { state: isLoaded } };
                const scOrigBpm = { translator: { unnormalized: origBpm } };
                const scBpm = { translator: { unnormalized: currentBpm } };
                const scAnalyzed = { translator: { state: isAnalyzed } };
                const scSpeed = { translator: { string: fmtSpeedState(pitchArg) } };
                const scMetaBpm = { translator: { string: metaBpmStr } };
                """ + m_info.group(1) + r"""
            }
            // 1. Empty / unloaded deck: returns null even if stale deck translators remain non-zero
            assert.strictEqual(makeBpmInfo(false, 0, 0, "", "-", ""), null);
            assert.strictEqual(makeBpmInfo(false, 128.0, 1.6, "128.0", "-", "", { loaded: false, origBpm: 128.0 }), null);
            // 2. Track loaded with no BPM anywhere and unanalyzed: returns null (preserves stock "-")
            assert.strictEqual(makeBpmInfo(false, 0, 1.6, "", "-", "", { loaded: true }), null);
            // 3. Track loaded at 125 BPM, waveform loading (isAnalyzed: false), pitch at 0% / +1.60% / +3.20% / -4.00%
            assert.deepStrictEqual(makeBpmInfo(false, 125.0, 0, "125.0", "125", ".0"), ["125", "00"]);
            assert.deepStrictEqual(makeBpmInfo(false, 125.0, 1.6, "125.0", "125", ".0"), ["127", "00"]);
            assert.deepStrictEqual(makeBpmInfo(false, 125.0, 3.2, "125.0", "125", ".0"), ["129", "00"]);
            assert.deepStrictEqual(makeBpmInfo(false, 125.0, -4.0, "125.0", "125", ".0"), ["120", "00"]);
            // 4. Exact pitch precision from deckData.speedState vs 1-decimal SpeedState.translator.string:
            //    +1.5625% -> SpeedState.translator.string is "+1.6%" (which would wrongly give 130.05)
            //    +1.6953125% -> SpeedState.translator.string is "+1.7%" (which would wrongly give 130.18)
            assert.strictEqual(fmtSpeedState(1.5625), "+1.6%");
            assert.deepStrictEqual(makeBpmInfo(false, 128.0, 1.5625, "128.0", "128", ".0", { origBpm: 128.0 }), ["130", "00"]);
            assert.strictEqual(fmtSpeedState(1.6953125), "+1.7%");
            assert.deepStrictEqual(makeBpmInfo(false, 128.0, 1.6953125, "128.0", "128", ".0", { origBpm: 128.0 }), ["130", "17"]);
            // 5. CurrentBPM === 0 while waveform is loading
            assert.deepStrictEqual(makeBpmInfo(false, 0, 0, "125.0", "125", ".0"), ["125", "00"]);
            assert.deepStrictEqual(makeBpmInfo(false, 0, 1.6, "125.0", "125", ".0"), ["127", "00"]);
            assert.deepStrictEqual(makeBpmInfo(false, 0, 0, "128.4", "128", ".4", { origBpm: 128.36 }), ["128", "36"]);
            assert.deepStrictEqual(makeBpmInfo(false, 0, 1.6, "128.4", "128", ".4", { origBpm: 128.36 }), ["130", "41"]);
            // 6. Integer MetaDataBPM without decimal point ("128", stock bpmFractionalText is "")
            assert.deepStrictEqual(makeBpmInfo(false, 0, 0, "128", "128", ""), ["128", "00"]);
            assert.deepStrictEqual(makeBpmInfo(false, 0, 1.5625, "128", "128", ""), ["130", "00"]);
            assert.deepStrictEqual(makeBpmInfo(false, 128.0, 0, "128", "128", ""), ["128", "00"]);
            assert.deepStrictEqual(makeBpmInfo(false, 128.0, 1.5625, "128", "128", ""), ["130", "00"]);
            // 7. 2-decimal Rekordbox BPM (128.36) vs 1-decimal ("128.4") or integer ("128") MetaDataBPM:
            //    updates live with pitch fader and does NOT jump when SongAnalyzed flips to true.
            for (const origBpm of [128.36, 0]) {
                const unanalyzedZero = makeBpmInfo(false, 128.36, 0, "128.4", "128", ".4", { origBpm });
                const analyzedZero = makeBpmInfo(true, 128.36, 0, "128.4", "128", ".4", { origBpm: 128.36 });
                assert.deepStrictEqual(unanalyzedZero, ["128", "36"]);
                assert.deepStrictEqual(unanalyzedZero, analyzedZero);

                const unanalyzedPitched = makeBpmInfo(false, 128.36, 1.6, "128.4", "128", ".4", { origBpm });
                const analyzedPitched = makeBpmInfo(true, 128.36 * 1.016, 1.6, "128.4", "130", ".4", { origBpm: 128.36 });
                assert.deepStrictEqual(unanalyzedPitched, ["130", "41"]);
                assert.deepStrictEqual(unanalyzedPitched, analyzedPitched);

                const unanalyzedIntMeta = makeBpmInfo(false, 128.36, 1.6, "128", "128", "", { origBpm });
                assert.deepStrictEqual(unanalyzedIntMeta, ["130", "41"]);
                assert.deepStrictEqual(unanalyzedIntMeta, analyzedPitched);
            }
            // 8. Sync state: Engine cur already differs from base (e.g. 130.00), uses cur directly without double-pitching
            assert.deepStrictEqual(makeBpmInfo(false, 130.0, 1.6, "125.0", "125", ".0", { origBpm: 125.0 }), ["130", "00"]);
            assert.deepStrictEqual(makeBpmInfo(false, 130.0, 1.6, "125.0", "125", ".0"), ["130", "00"]);
            // 9. Analyzed track: uses exact current BPM in 2-decimal format
            assert.deepStrictEqual(makeBpmInfo(true, 127.0, 1.6, "125.0", "127", ".0", { origBpm: 125.0 }), ["127", "00"]);
            console.log("BpmInfo tests passed");
            """
            output = subprocess.check_output(["node", "-e", script], text=True)
            self.assertIn("BpmInfo tests passed", output)

            # Test Bpm.qml scPitch + currentBPMSplit behavior via node
            bpm_qml = (packs / "en" / "Skins/Embedded/Items/Bpm.qml").read_text()
            m_pitch = re.search(r"readonly property real scPitch: \{(.*?)\n    \}", bpm_qml, re.S)
            m_split = re.search(r"readonly property var currentBPMSplit: \{(.*?)\n    \}", bpm_qml, re.S)
            self.assertIsNotNone(m_pitch, "Bpm.qml must compute scPitch from exact deckData.speedState")
            self.assertIsNotNone(m_split, "Bpm.qml must compute currentBPMSplit dynamically")
            bpm_script = r"""
            const assert = require('assert');
            function fmtSpeedState(p) {
                return (p >= 0 ? "+" : "") + p.toFixed(1) + "%";
            }
            function evalPitch(bpmPropertyName, pitchArg, deckData) {
                const scDeckData = deckData !== undefined ? deckData : { speedState: pitchArg };
                const scSpeed = { translator: { string: fmtSpeedState(pitchArg) } };
                """ + m_pitch.group(1) + r"""
            }
            function makeBpm(songAnalyzed, bpmValue, pitchArg, metaDataBpm, opts = {}) {
                const decimalPlaces = opts.decimalPlaces !== undefined ? opts.decimalPlaces : 1;
                const bpmPropertyName = opts.bpmPropertyName || "CurrentBPM";
                const externalBpm = !!opts.externalBpm;
                const origBpm = opts.origBpm !== undefined ? opts.origBpm : 0;
                const bpm = { decimalPlaces };
                const scPitch = evalPitch(bpmPropertyName, pitchArg, opts.deckData);
                const scOrigBpm = { translator: { unnormalized: origBpm } };
                const hasMetaDataBpm = !!metaDataBpm;
                """ + m_split.group(1) + r"""
            }
            // 1. Unanalyzed without metadata or base BPM: returns [] (currentBPMSplitValid is false, displays "-")
            assert.deepStrictEqual(makeBpm(false, 0, 0, ""), []);
            // 2. Exact scPitch from deckData.speedState (not 1-decimal SpeedState.translator.string "+1.6%" / "+1.7%")
            assert.strictEqual(evalPitch("CurrentBPM", 1.5625), 1.5625);
            assert.strictEqual(evalPitch("OriginalBPM", 1.5625), 0);
            assert.deepStrictEqual(makeBpm(false, 128.0, 1.5625, "128.0", { decimalPlaces: 2, origBpm: 128.0 }), ["130", "00"]);
            assert.deepStrictEqual(makeBpm(false, 128.0, 1.6953125, "128.0", { decimalPlaces: 2, origBpm: 128.0 }), ["130", "17"]);
            // 3. CurrentBPM === 0 while waveform is loading
            assert.deepStrictEqual(makeBpm(false, 0, 0, "125.0"), ["125", "0"]);
            assert.deepStrictEqual(makeBpm(false, 0, 1.6, "125.0"), ["127", "0"]);
            assert.deepStrictEqual(makeBpm(false, 0, 1.6, "128.4", { decimalPlaces: 2, origBpm: 128.36 }), ["130", "41"]);
            // 4. Integer MetaDataBPM ("128"): always returns 2-part [integral, fraction] so currentBPMSplitValid === true
            assert.deepStrictEqual(makeBpm(false, 0, 0, "128"), ["128", "0"]);
            assert.deepStrictEqual(makeBpm(false, 128.0, 0, "128"), ["128", "0"]);
            assert.deepStrictEqual(makeBpm(false, 128.0, 0, "128", { decimalPlaces: 2 }), ["128", "00"]);
            assert.deepStrictEqual(makeBpm(false, 0, 1.5625, "128"), ["130", "0"]);
            assert.deepStrictEqual(makeBpm(false, 128.0, 1.5625, "128"), ["130", "0"]);
            assert.deepStrictEqual(makeBpm(false, 128.0, 1.5625, "128", { decimalPlaces: 2 }), ["130", "00"]);
            // 5. 2-decimal Rekordbox BPM (128.36) vs 1-decimal ("128.4") or integer ("128") MetaDataBPM: no jump on analyze
            for (const origBpm of [128.36, 0]) {
                assert.deepStrictEqual(makeBpm(false, 128.36, 0, "128.4", { decimalPlaces: 2, origBpm }), ["128", "36"]);
                assert.deepStrictEqual(
                    makeBpm(false, 128.36, 1.6, "128.4", { decimalPlaces: 2, origBpm }),
                    makeBpm(true, 128.36 * 1.016, 1.6, "128.4", { decimalPlaces: 2, origBpm: 128.36 })
                );
                assert.deepStrictEqual(
                    makeBpm(false, 128.36, 1.6, "128", { decimalPlaces: 2, origBpm }),
                    ["130", "41"]
                );
                assert.deepStrictEqual(
                    makeBpm(false, 128.36, 1.6, "128.4", { decimalPlaces: 1, origBpm }),
                    makeBpm(true, 128.36 * 1.016, 1.6, "128.4", { decimalPlaces: 1, origBpm: 128.36 })
                );
            }
            // 6. Sync state while unanalyzed: uses synced bpmValue without double-pitching
            assert.deepStrictEqual(makeBpm(false, 130.0, 1.6, "125.0", { origBpm: 125.0 }), ["130", "0"]);
            assert.deepStrictEqual(makeBpm(false, 130.0, 1.6, "125.0"), ["130", "0"]);
            // 7. OriginalBPM callers (DecksTabBarBpm / WaveformOverviewInfo / BPMLabel) ignore pitch
            assert.deepStrictEqual(makeBpm(false, 128.36, 1.6, "128.4", { decimalPlaces: 2, bpmPropertyName: "OriginalBPM", origBpm: 128.36 }), ["128", "36"]);
            // 8. Shadow deck externalBpm isolation: never leaks local deck OriginalBPM / MetaDataBPM when shadow tempo is 0
            assert.deepStrictEqual(makeBpm(false, 0, 1.6, "128.0", { decimalPlaces: 2, externalBpm: true, origBpm: 128.0 }), []);
            // 9. Analyzed -> 127.0
            assert.deepStrictEqual(makeBpm(true, 127.0, 1.6, "125.0"), ["127", "0"]);
            console.log("Bpm.qml tests passed");
            """
            output2 = subprocess.check_output(["node", "-e", bpm_script], text=True)
            self.assertIn("Bpm.qml tests passed", output2)

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


@unittest.skipUnless(shutil.which("node"), "node needed for the shadow-deck behavioral checks")
class ShadowSyncSnapTest(unittest.TestCase):
    """Sync changes the source's phase and rate; neither is ordinary clock drift."""
    js = ShadowDisplayTest.js

    def test_sender_posts_sync_with_its_tempo_step_inside_the_sync_cache_window(self):
        self.js(r"""
globalThis.XMLHttpRequest=class{open(){this.readyState=1}send(){this.readyState=4;this.status=200}};
globalThis.scShadowPoll=()=>{};scFourDeckRole='1';
let bpm=128,synced=false;
globalThis.scTr=path=>({value:path.endsWith('/SampleRate')?48000:0,color:'#fff',index:0,
 state:path.endsWith('/SongLoaded')||path.endsWith('/PlayState'),
 get unnormalized(){return path.endsWith('/OriginalBPM')?128:path.endsWith('/CurrentBPM')?bpm:
  path.endsWith('/TrackPosition')?10000*48:path.endsWith('/TrackLength')?48000*400:0},
 get string(){return path.endsWith('/SyncMode')?(synced?'BeatOrBarSync':'Off'):
  path.endsWith('/TrackUri')?'file:///a.wav':path.endsWith('/SpeedState')?((bpm/128-1)*100).toFixed(2)+'%':''}});
scShadowTick();
assert.equal(scShadowOwn[0].sync,false);
now+=40;bpm=131.84;synced=true;scShadowTick();
assert.equal(scShadowOwn[0].currentBpm,131.84);
assert.equal(scShadowOwn[0].sync,true,'the tempo step must not be posted with the previous Sync state');
""")

    def test_sender_publishes_native_phase_ramps_even_when_every_gui_tick_is_late(self):
        self.js(r"""
globalThis.XMLHttpRequest=class{open(){this.readyState=1}send(){this.readyState=4;this.status=200}};
globalThis.scShadowPoll=()=>{};scFourDeckRole='1';
let pos=10000,rate=1,synced=false;
globalThis.scTr=path=>({value:path.endsWith('/SampleRate')?48000:0,color:'#fff',index:0,
 state:path.endsWith('/SongLoaded')||path.endsWith('/PlayState'),
 get unnormalized(){return path.endsWith('/TrackPosition')?pos*48:path.endsWith('/TrackLength')?48000*400:
  path.endsWith('/OriginalBPM')?128:path.endsWith('/CurrentBPM')?128*rate:0},
 get string(){return path.endsWith('/SyncMode')?(synced?'BeatOrBarSync':'Off'):
  path.endsWith('/TrackUri')?'file:///a.wav':path.endsWith('/SpeedState')?((rate-1)*100).toFixed(2)+'%':''}});
const tick=(dt,phase=0)=>{now+=dt;pos+=dt*rate+phase;scShadowTick()};
const published=()=>{const s=scShadowOwn[0];return s.positionMs+(now-s.at)*(1+s.pitch/100)};
for(const direction of [-1,1]){
 synced=false;rate=1;for(let i=0;i<40;i++)tick(80);
 synced=true;rate=1.03;
 for(let i=0;i<9;i++){
  tick(175,direction*25);
  assert.ok(Math.abs(published()-pos)<1,'late tick concealed the native phase ramp');
 }
 for(let i=0;i<12;i++){
  tick(175);
  assert.ok(Math.abs(published()-pos)<2,'the drift envelope kept a tail after native Sync settled');
 }
}
// At the measured maximum pull, one sample remains ambiguous. Two samples
// must establish motion instead of accumulating a synthetic late-tick deficit.
for(const direction of [-1,1]){
 synced=false;rate=1;for(let i=0;i<40;i++)tick(80);
 synced=true;rate=1.03;
 for(let i=0;i<9;i++){
  tick(175,direction*0.27*175*rate);
  const oneSample=0.27*175*rate+Math.abs(rate-1)*175+1;
  assert.ok(Math.abs(published()-pos)<oneSample,'maximum native pull was concealed: '+(published()-pos));
 }
 tick(80);tick(80);
 assert.ok(Math.abs(published()-pos)<2,'maximum phase pull left a nominal-line tail');
}
""")

    def test_sync_transition_still_carries_short_stale_reads(self):
        self.js(r"""
globalThis.XMLHttpRequest=class{open(){this.readyState=1}send(){this.readyState=4;this.status=200}};
globalThis.scShadowPoll=()=>{};scFourDeckRole='1';
const T0=now;let stale=0,synced=false;
globalThis.scTr=path=>({value:path.endsWith('/SampleRate')?48000:0,color:'#fff',index:0,
 state:path.endsWith('/SongLoaded')||path.endsWith('/PlayState'),
 get unnormalized(){return path.endsWith('/TrackPosition')?(10000+now-T0-stale)*48:
  path.endsWith('/TrackLength')?48000*400:path.endsWith('/OriginalBPM')||path.endsWith('/CurrentBPM')?128:0},
 get string(){return path.endsWith('/SyncMode')?(synced?'BeatOrBarSync':'Off'):path.endsWith('/TrackUri')?'file:///a.wav':''}});
for(let i=0;i<40;i++){now+=80;scShadowTick()}
synced=true;now+=240;scShadowTick();now+=80;scShadowTick();
const jumps=scShadowPrev[1].jumps;
// A single delayed but advancing block must not bypass phase confirmation
// merely because the transition publisher uses raw positions.
now+=130;stale=60;scShadowTick();
let sent=scShadowOwn[0];
assert.ok(Math.abs(sent.positionMs+now-sent.at-(10000+now-T0))<2,'unconfirmed stale phase reached the peer');
stale=0;now+=80;scShadowTick();
assert.equal(scShadowPrev[1].jumps,jumps);
now+=426;stale=340;scShadowTick();
assert.equal(scShadowPrev[1].jumps,jumps);
assert.ok(Math.abs(scShadowOwn[0].positionMs-(10000+now-T0))<2,'stale read moved the source line');
stale=0;now+=80;scShadowTick();
assert.equal(scShadowPrev[1].jumps,jumps,'fresh recovery caused a seek');
// An advancing queued stream is still 180 ms old; do not turn its delayed
// delivery into a pair of backwards/forwards Sync seeks.
now+=426;stale=180;scShadowTick();
now+=175;scShadowTick();
assert.equal(scShadowPrev[1].jumps,jumps,'advancing stale positions became a phase step');
stale=0;now+=80;scShadowTick();
assert.equal(scShadowPrev[1].jumps,jumps,'advancing stale recovery became a jump');
// One late callback is not evidence of a platter held for its entire duration.
now+=700;stale=400;scShadowTick();
assert.ok(!scShadowPrev[1].phaseHold,'one long stale callback became a platter hold');
stale=0;now+=80;scShadowTick();
assert.equal(scShadowPrev[1].jumps,jumps,'a single long GUI stall caused a seek pair');
// A smaller advancing backlog fits inside one long callback's pull bound.
// Its constant deficit still needs a normal callback to disambiguate it.
synced=false;now+=240;scShadowTick();synced=true;now+=240;scShadowTick();
now+=80;scShadowTick();const smallBacklog=scShadowPrev[1].jumps;
now+=426;stale=150;scShadowTick();now+=175;scShadowTick();
assert.equal(scShadowPrev[1].jumps,smallBacklog,'a constant advancing backlog became a native ramp');
sent=scShadowOwn[0];
assert.ok(Math.abs(sent.positionMs+now-sent.at-(10000+now-T0))<2,'an advancing backlog reached the peer');
stale=0;now+=80;scShadowTick();
assert.equal(scShadowPrev[1].jumps,smallBacklog,'queued-stream recovery caused a jump');
""")

    def test_noop_sync_keeps_a_biased_block_line_and_a_late_hold_survives_the_deadline(self):
        self.js(r"""
globalThis.XMLHttpRequest=class{open(){this.readyState=1}send(){this.readyState=4;this.status=200}};
globalThis.scShadowPoll=()=>{};scFourDeckRole='1';
const T0=now;let pos=10000,synced=false,jitter=0;
globalThis.scTr=path=>({value:path.endsWith('/SampleRate')?48000:0,color:'#fff',index:0,
 state:path.endsWith('/SongLoaded')||path.endsWith('/PlayState'),
 get unnormalized(){return path.endsWith('/TrackPosition')?(pos+jitter)*48:path.endsWith('/TrackLength')?48000*400:
  path.endsWith('/OriginalBPM')||path.endsWith('/CurrentBPM')?128:0},
 get string(){return path.endsWith('/SyncMode')?(synced?'BeatOrBarSync':'Off'):path.endsWith('/TrackUri')?'file:///a.wav':''}});
const tick=(dt=80,move=dt)=>{now+=dt;pos+=move;scShadowTick()};
for(let i=0;i<70;i++){jitter=[0,-12,-28,-4,-18,-30,-8][i%7];tick()}
assert.ok(scShadowPrev[1].env-scShadowPrev[1].avg>10,'the fixture needs a measured block bias');
synced=true;const jumps=scShadowPrev[1].jumps;
for(let i=0;i<12;i++){
 jitter=[0,-12,-28,-4,-18,-30,-8][i%7];tick(175);
 const s=scShadowOwn[0],line=s.positionMs+now-s.at;
 assert.ok(Math.abs(line-pos)<=35,'no-op Sync published an excessive block excursion');
}
assert.equal(scShadowPrev[1].jumps,jumps,'no-op Sync jitter became a discontinuity');
for(let i=0;i<8;i++){
 jitter=[0,-12,-28,-4,-18,-30,-8][i%7];tick(175);
 const s=scShadowOwn[0],line=s.positionMs+now-s.at;
 assert.ok(Math.abs(line-pos)<=35,'leaving transition mode created a new phase step');
}
assert.equal(scShadowPrev[1].jumps,jumps,'transition expiry invented a discontinuity');
// Arm a new transition, then freeze the raw block beyond its two-second deadline.
synced=false;tick(240);synced=true;tick(240);jitter=0;
const held=pos,was=scShadowPrev[1].jumps;
for(let i=0;i<20;i++){
 tick(175,0);
 if(i>=4)assert.ok(scShadowPrev[1].phaseHold,'the hold was lost on a late GUI turn');
}
assert.equal(scShadowPrev[1].jumps,was+1,'the held block must be published once');
assert.ok(Math.abs(scShadowOwn[0].positionMs-held)<=1);
for(let i=0;i<8;i++)tick(175);
assert.equal(scShadowPrev[1].jumps,was+1,'release generated another discontinuity');
""")

    def test_sender_publishes_sync_phase_steps_without_filtering_them(self):
        self.js(r"""
globalThis.XMLHttpRequest = class { open(){this.readyState=1} send(){this.readyState=4;this.status=200} };
globalThis.scShadowPoll = () => {};
scFourDeckRole = '1';
let pos = 10000, rate = 1, synced = false;
globalThis.scTr = path => ({value:path.endsWith('/SampleRate')?48000:0, color:'#fff', index:0,
 state:path.endsWith('/SongLoaded')||path.endsWith('/PlayState'),
 get unnormalized(){return path.endsWith('/TrackPosition')?pos*48:path.endsWith('/TrackLength')?48000*400:
   path.endsWith('/OriginalBPM')?128:path.endsWith('/CurrentBPM')?128*rate:0},
 get string(){return path.endsWith('/TrackUri')?'file:///a.wav':path.endsWith('/SpeedState')?((rate-1)*100).toFixed(2)+'%':
   path.endsWith('/SyncMode')?(synced?'BeatOrBarSync':'Off'):''}
});
const tick = (dt=80) => {now+=dt;pos+=dt*rate;scShadowTick()};
for(let i=0;i<70;i++)tick();
// The phase goes backwards by less than the old 180 ms jump threshold.
synced=true;pos-=120;
tick(240); // outside the existing Sync string cache; GUI-stall protection still applies
tick();tick(); // the next audio block confirms a small phase discontinuity
assert.ok(Math.abs(scShadowOwn[0].positionMs-pos)<1,'Sync phase was left in the upper-envelope drift filter');
assert.equal(scShadowOwn[0].sync,true);
const jumps=scShadowPrev[1].jumps;
pos-=100;tick();tick();
assert.equal(scShadowPrev[1].jumps,jumps+1,'a phase step while synced must be a jump');
assert.ok(Math.abs(scShadowOwn[0].positionMs-pos)<1);
// Normal audio-block jitter at high rates must not count as phase jumps.
rate=2.0309;tick();
const stable=scShadowPrev[1].jumps;
for(let i=0;i<40;i++){const jitter=i%2?25:-25;pos+=jitter;tick();pos-=jitter;}
assert.equal(scShadowPrev[1].jumps,stable,'audio blocks became repeated seeks at over-range Sync');
""")

    ENGINE = r"""
Object.assign(globalThis,{scShadowDisplayOnly:false,scShadowEngineMask:30,scShadowBent:null});
const props=new Map(), seeks=[], speeds=[], presses=[];
let pos=10000, rate=1;
globalThis.scShadowBendTimer={stop(){},restart(){}};
globalThis.scShadowHolds=()=>true;
globalThis.scShadowEnd=()=>false;
globalThis.scShadowCues=()=>{};
globalThis.scShadowBeat=()=>{};
globalThis.scShadowSpeed=(d,p)=>speeds.push(p);
globalThis.scPress=p=>presses.push(p);
globalThis.scTr=path=>{
 if(props.has(path))return props.get(path);
 const p={state:path.endsWith('/PlayState')||path.endsWith('/SongLoaded'),value:path.endsWith('/SampleRate')?48000:0,
  string:path.endsWith('/SpeedRange')?'100':path.endsWith('/SyncMode')?'Off':'',
  get unnormalized(){return path.endsWith('/TrackPosition')?pos*48:path.endsWith('/TrackLength')?48000*400:0}};
 props.set(path,p);return p;
};
globalThis.scShadowSeek=(d,at,ms)=>{seeks.push(ms);pos=ms;at.seekAt=now;at.env=undefined};
scShadowAt[2]={path:'/a',id:'a',ready:true,seekAt:0,jumps:0};
let seq=0;
const apply=(extra={})=>scShadowApply(Object.assign({deck:2,path:'/a',id:'a',link:'peer',playing:true,
 sync:false,positionMs:pos,at:now,pitch:(rate-1)*100,bpm:128,currentBpm:128*rate,jumps:0,seq:++seq},extra));
apply();
"""

    def test_sync_engagement_seeks_and_releases_an_active_bend_on_the_first_update(self):
        self.js(self.ENGINE + r"""
for(const delta of [-120,120,-500,500]){
 apply({sync:false});
 const at=scShadowAt[2], bend={state:true};
 scShadowBent={at,p:bend};at.env=pos;at.envT0=now-4000;at.envRate=rate;at.fine=120;
 now+=80;pos+=80*rate;
 const target=pos+delta, before=seeks.length;
 apply({sync:true,positionMs:target});
 assert.equal(seeks.length,before+1,'Sync '+delta+' ms was bent instead of sought');
 assert.equal(pos,target);assert.equal(bend.state,false);assert.equal(scShadowBent,null);
 for(let i=0;i<10;i++){now+=80;pos+=80*rate;apply({sync:true})}
 assert.equal(seeks.length,before+1,'Sync heartbeat caused an extra seek');
}
assert.ok(!presses.some(p=>p.endsWith('/RequestMaster')||p.endsWith('/Sync')),'ordinary linked copies retain their ownership rules');
""")

    def test_tempo_steps_and_source_jumps_snap_but_small_drift_does_not_seek(self):
        self.js(self.ENGINE + r"""
now+=80;rate=1.03;
apply({positionMs:pos-110});
assert.equal(seeks.length,1,'tempo step must apply its phase with its rate');
assert.ok(Math.abs(speeds.at(-1)-3)<1e-9);
now+=80;apply({sync:true});
const before=seeks.length;
now+=80;apply({sync:true,positionMs:pos+90,jumps:1});
assert.equal(seeks.length,before+1,'a sub-180 ms source phase jump must seek');
const stable=seeks.length;
for(let i=0;i<100;i++){now+=80;pos+=80*rate;apply({sync:true,positionMs:pos+(i%2?20:-20),jumps:1})}
assert.equal(seeks.length,stable,'steady audio-block drift must not cause seeks');
""")

    def test_small_sync_drift_and_native_cdj_sync_keep_their_existing_paths(self):
        self.js(self.ENGINE + r"""
now+=80;apply({sync:true,positionMs:pos+20});
assert.equal(seeks.length,0,'Sync with small drift must not flush the decoder');
now+=80;rate=1.03;apply({sync:true,positionMs:pos-20});
assert.equal(seeks.length,0,'a tempo change with small drift only changes rate');
now+=80;apply({sync:false,link:''});
now+=80;apply({sync:true,link:'',positionMs:pos+120});
assert.ok(presses.includes('/Engine/Deck2/Sync'),'CDJ sources still use native Sync');
assert.equal(seeks.length,0,'native beat Sync must retain its existing phase control');
""")

    def test_phase_pull_after_sync_uses_the_new_source_line_without_repeated_heartbeat_seeks(self):
        self.js(self.ENGINE + r"""
now+=80;apply({sync:true});
assert.equal(seeks.length,0);
now+=200;pos+=200;
apply({sync:true,positionMs:pos+100});
assert.equal(seeks.length,1,'a phase pull after the Sync flag must not wait for a bend');
now+=200;pos+=200;
apply({sync:true,positionMs:pos-90});
assert.equal(seeks.length,2,'the later phase change must follow in either direction');
for(let i=0;i<25;i++){now+=80;pos+=80;apply({sync:true})}
assert.equal(seeks.length,2,'carried source heartbeats must not re-seek');
now+=80;apply({sync:true,positionMs:pos+80});
assert.equal(seeks.length,2,'normal drift after the transition stays on the bend path');
""")

    def test_sync_phase_between_20_and_40ms_does_not_wait_for_a_bend(self):
        self.js(self.ENGINE + r"""
now+=80;apply({sync:true,positionMs:pos+30});
assert.equal(seeks.length,1,'a 30 ms Sync step must meet the 20 ms alignment target');
now+=200;pos+=200;apply({sync:true,positionMs:pos+20});
assert.equal(seeks.length,1,'small drift still uses the bend path');
now+=200;pos+=200;apply({sync:true,positionMs:pos+30});
assert.equal(seeks.length,2,'a later 30 ms source phase pull must also snap');
for(let i=0;i<20;i++){now+=80;pos+=80;apply({sync:true})}
assert.equal(seeks.length,2,'steady carried source positions must not add seeks');
""")

    def test_synced_gui_stall_is_ignored_but_a_real_phase_jump_survives_it(self):
        self.js(r"""
globalThis.XMLHttpRequest=class{open(){this.readyState=1}send(){this.readyState=4;this.status=200}};
globalThis.scShadowPoll=()=>{};
scFourDeckRole='1';
const T0=now;let stale=0,moved=0;
globalThis.scTr=path=>({value:path.endsWith('/SampleRate')?48000:0,color:'#fff',index:0,
 state:path.endsWith('/SongLoaded')||path.endsWith('/PlayState'),
 get unnormalized(){return path.endsWith('/TrackPosition')?(10000+now-T0-stale+moved)*48:
  path.endsWith('/TrackLength')?48000*400:path.endsWith('/OriginalBPM')||path.endsWith('/CurrentBPM')?128:0},
 get string(){return path.endsWith('/SyncMode')?'BeatOrBarSync':path.endsWith('/TrackUri')?'file:///a.wav':''}});
for(let i=0;i<70;i++){now+=80;scShadowTick()}
const jumps=scShadowPrev[1].jumps;
// A queued position can lag even when the timer was held for less than
// the original 150 ms late-tick guard. Recovery must not seek twice.
for(const dt of [80,130]){
 now+=dt;stale=80;scShadowTick();
 assert.equal(scShadowPrev[1].jumps,jumps,'a short synced GUI stall became a jump');
 stale=0;now+=80;scShadowTick();
 assert.equal(scShadowPrev[1].jumps,jumps,'short GUI recovery became a jump');
}
// At the busy 40 ms cadence, several queued ticks may read exactly the
// same old block. A phase confirmation must observe playback advancing.
const frozen=10000+now-T0-50;
for(let i=0;i<8;i++){
 now+=40;stale=10000+now-T0-frozen;scShadowTick();
 assert.equal(scShadowPrev[1].jumps,jumps,'a repeated old block confirmed a false jump');
}
stale=0;now+=40;scShadowTick();
assert.equal(scShadowPrev[1].jumps,jumps,'queued block recovery became a jump');
now+=426;stale=340;scShadowTick();
assert.equal(scShadowPrev[1].jumps,jumps,'GUI stall became a Sync phase jump');
stale=0;now+=80;scShadowTick();
assert.equal(scShadowPrev[1].jumps,jumps,'GUI recovery became a Sync phase jump');
now+=426;moved=-100;scShadowTick();
assert.equal(scShadowPrev[1].jumps,jumps);
now+=80;scShadowTick();
assert.equal(scShadowPrev[1].jumps,jumps,'one stale block remains ambiguous');
now+=80;scShadowTick();
assert.equal(scShadowPrev[1].jumps,jumps+1,'a real small phase step during the stall was lost');
assert.ok(Math.abs(scShadowOwn[0].positionMs-(10000+now-T0+moved))<1);
""")

    def test_synced_platter_hold_leaves_the_stall_guard_and_keeps_the_jog_line(self):
        self.js(r"""
globalThis.XMLHttpRequest=class{open(){this.readyState=1}send(){this.readyState=4;this.status=200}};
globalThis.scShadowPoll=()=>{};scFourDeckRole='1';
const T0=now;let pos=10000;
globalThis.scTr=path=>({value:path.endsWith('/SampleRate')?48000:0,color:'#fff',index:0,
 state:path.endsWith('/SongLoaded')||path.endsWith('/PlayState'),
 get unnormalized(){return path.endsWith('/TrackPosition')?pos*48:path.endsWith('/TrackLength')?48000*400:
  path.endsWith('/OriginalBPM')||path.endsWith('/CurrentBPM')?128:0},
 get string(){return path.endsWith('/SyncMode')?'BeatOrBarSync':path.endsWith('/TrackUri')?'file:///a.wav':''}});
const tick=(move,dt=80)=>{now+=dt;pos+=move;scShadowTick()};
for(let i=0;i<70;i++)tick(80);
const jumps=scShadowPrev[1].jumps,held=pos;
for(let i=0;i<20;i++)tick(0);
assert.ok(scShadowPrev[1].jumps<=jumps+1,'a held platter must not seek on every sample');
assert.ok(Math.abs(scShadowOwn[0].positionMs-held)<500,'the source line kept playing during a platter hold');
const released=scShadowPrev[1].jumps;
for(let i=0;i<10;i++)tick(80);
assert.equal(scShadowPrev[1].jumps,released,'releasing the platter repeated the seek');
// Slow GUI turns must not clear the hold when the publication window ends.
for(let i=0;i<40;i++)tick(80);
const heldLate=pos,lateJumps=scShadowPrev[1].jumps;
for(let i=0;i<22;i++)tick(0,175);
assert.ok(scShadowPrev[1].jumps<=lateJumps+1);
assert.ok(Math.abs(scShadowOwn[0].positionMs-heldLate)<500,'late ticks lost the platter hold');
""")


@unittest.skipUnless(shutil.which("node"), "node needed for the landing behavioral checks")
class ShadowLandingTest(unittest.TestCase):
    """A playing seek freezes the decoder and can expose the old line first."""
    js = ShadowDisplayTest.js
    ENGINE = r"""
Object.assign(globalThis,{scShadowDisplayOnly:false,scShadowEngineMask:30,scShadowBent:null});
const T0=now, props=new Map(), seeks=[], presses=[];
let shift=0, synced=false, rate=1, seq=0, tick=0, pending=null, base=10000;
let losses=[90,90], oldFor=120, noise=true, ignored=false, readingOffset=0;
const jitter=()=>readingOffset+(noise?rate*[0,-12,-28,-4,-18,-30,-8][tick%7]:0);
function playback(){
 if(!pending)return base+(now-T0)*rate;
 if(ignored||now<pending.t+oldFor)return pending.old+(now-pending.t)*rate;
 return pending.target+Math.max(0,now-pending.t-pending.loss)*rate;
}
globalThis.scShadowBendTimer={stop(){},restart(){}};
globalThis.scShadowHolds=()=>true;globalThis.scShadowEnd=()=>false;
globalThis.scShadowCues=()=>{};globalThis.scShadowBeat=()=>{};
globalThis.scShadowSpeed=()=>{};globalThis.scPress=p=>presses.push(p);
globalThis.scTr=path=>{
 if(props.has(path))return props.get(path);
 const p={state:path.endsWith('/PlayState')||path.endsWith('/SongLoaded'),
 string:path.endsWith('/SpeedRange')?'100':path.endsWith('/SyncMode')?'Off':'',
 get unnormalized(){return path.endsWith('/TrackPosition')?(playback()+jitter())*48:
  path.endsWith('/TrackLength')?48000*400:0},
 get value(){return path.endsWith('/SampleRate')?48000:this.v||0},
 set value(v){this.v=v;if(path.endsWith('/SetSongPosition')){
  const old=playback(),loss=losses[Math.min(seeks.length,losses.length-1)];
  pending={t:now,target:v*400000,old:old,loss:loss};seeks.push(pending);
 }}};props.set(path,p);return p;
};
scShadowAt[2]={path:'/a',id:'a',ready:true,seekAt:0,jumps:0};
const apply=(extra={})=>scShadowApply(Object.assign({deck:2,path:'/a',id:'a',link:'peer',playing:true,
 sync:synced,positionMs:10000+(now-T0)*rate+shift,at:now,pitch:(rate-1)*100,
 bpm:128,currentBpm:128*rate,jumps:0,seq:++seq},extra));
const step=(dt=80,extra={})=>{now+=dt;tick++;scShadowLine(now);scShadowLandingTick(now);apply(extra)};
apply();for(let i=0;i<50;i++)step();
const engage=(delta=-120)=>{shift+=delta;synced=true;step()};
"""

    def test_delayed_landing_is_measured_and_corrected_once_on_unchanged_sequences(self):
        self.js(self.ENGINE + r"""
engage();const unchanged=seq;
for(let i=0;i<14;i++)step(80,{seq:unchanged});
assert.equal(seeks.length,2,'one confirmed landing permits one compensated seek');
assert.ok(Math.abs(scShadowAt[2].seekLoss-90)<=20,'seek loss must come from the issued command within block uncertainty');
assert.ok(Math.abs(playback()-(10000+now-T0+shift))<16,'landing correction left a long bend tail');
assert.ok(!scShadowAt[2].landing,'the second landing must be verified and closed');
assert.ok(!presses.some(p=>p.endsWith('/RequestMaster')));
""")

    def test_source_motion_is_coalesced_and_never_learned_as_seek_loss(self):
        self.js(self.ENGINE + r"""
engage();step();shift+=60;step();
for(let i=0;i<12;i++)step();
assert.equal(seeks.length,2,'source updates replenished the seek budget');
assert.ok(Math.abs(scShadowAt[2].seekLoss-90)<=20,'source phase movement contaminated learned loss');
assert.ok(Math.abs(playback()-(10000+now-T0+shift))<16,'the correction did not use the latest source target');
""")

    def test_a_different_second_landing_loss_cannot_start_a_retry_loop(self):
        self.js(self.ENGINE + r"""
losses=[90,144];engage();
for(let i=0;i<38;i++){if(i===12)shift+=45;step()}
assert.equal(seeks.length,2,'changed native latency or source movement started a third seek');
assert.equal(scShadowAt[2].transitionSeeks,2);
""")

    def test_a_temporarily_aligned_landing_keeps_its_one_compensation_budget(self):
        self.js(self.ENGINE + r"""
engage();step();shift-=90;
for(let i=0;i<8;i++)step();
assert.equal(seeks.length,1,'a coincident source phase change needs no correction');
assert.ok(scShadowAt[2].landing && scShadowAt[2].landing.learned,'a transient alignment discarded transition tracking');
shift+=60;
for(let i=0;i<14;i++)step(80,{jumps:2});
assert.equal(seeks.length,2,'later source phase must use the existing compensation');
assert.ok(Math.abs(playback()-(10000+now-T0+shift))<20);
for(let i=0;i<32;i++)step(80,{jumps:2});
assert.equal(seeks.length,2,'a coalesced jump was replayed after the transition expired');
""")

    def test_landing_loss_uses_track_time_at_half_and_double_speed(self):
        for speed in (0.5, 2):
            with self.subTest(speed=speed):
                engine = self.ENGINE.replace("synced=false, rate=1,", "synced=false, rate=" + str(speed) + ",")
                self.js(engine + r"""
engage(-120*rate);for(let i=0;i<16;i++)step();
assert.equal(seeks.length,2);
assert.ok(Math.abs(scShadowAt[2].seekLoss/rate-90)<=20);
assert.ok(Math.abs(playback()-(10000+(now-T0)*rate+shift))<20*rate);
""")

    def test_cold_raw_baseline_cannot_learn_an_ignored_seek_from_newer_old_reads(self):
        self.js(self.ENGINE + r"""
noise=false;ignored=true;readingOffset=-35;scShadowAt[2].env=undefined;
engage(120);readingOffset=0;
for(let i=0;i<22;i++)step();
assert.equal(seeks.length,1,'newer reads of an old line escaped the cold baseline band');
assert.ok(!scShadowAt[2].landing,'unobservable ignored seek did not expire');
""")

    def test_a_separate_large_source_jump_is_immediate_after_the_sync_budget_is_spent(self):
        self.js(self.ENGINE + r"""
engage();for(let i=0;i<16;i++)step();
assert.equal(seeks.length,2);
const before=seeks.length;shift+=1000;step(80,{jumps:1});
assert.equal(seeks.length,before+1,'the Sync correction budget delayed a separate cue/jump');
assert.ok(!scShadowAt[2].landing,'a separate discontinuity must keep its original one-seek path');
for(let i=0;i<12;i++)step(80,{jumps:1});
assert.equal(seeks.length,before+1,'a cue jump caused an extra decoder flush');
""")

    def test_repeated_cues_and_loop_wraps_each_keep_one_seek(self):
        self.js(self.ENGINE + r"""
engage();for(let i=0;i<16;i++)step();
let count=seeks.length,jumps=0;
for(const delta of [1000,-750,1200,-5000,-5000,-5000]){
 shift+=delta;step(80,{jumps:++jumps});
 assert.equal(seeks.length,++count,'a transport discontinuity was deferred');
 for(let i=0;i<9;i++)step(80,{jumps:jumps});
 assert.equal(seeks.length,count,'a cue/loop wrap replenished compensation');
 assert.ok(!scShadowAt[2].landing);
}
""")

    def test_unsynced_tempo_and_jump_keep_the_original_transport_path(self):
        self.js(self.ENGINE + r"""
rate=1.03;shift=-300;step();
assert.equal(seeks.length,1);
assert.ok(!scShadowAt[2].landing,'a tempo-only change armed Sync compensation');
shift+=1000;step(80,{jumps:1});
assert.equal(seeks.length,2,'an unsynced jump was held by the transition budget');
assert.ok(!scShadowAt[2].landing);
""")

    def test_observer_records_the_clamped_command_instead_of_an_unwritten_target(self):
        self.js(self.ENGINE + r"""
const at=scShadowAt[2];at.phaseHardUntil=now+4000;at.phaseStableAt=now;
scShadowLandingSeek(2,at,399980,playback(),now,200);
assert.equal(at.landing.target,400000,'loss was measured against an unclamped target');
scShadowLandingSeek(2,at,-200,playback(),now,0);
assert.equal(at.landing.target,0);
""")

    def test_old_advancing_reads_and_overlap_never_prove_a_landing(self):
        self.js(self.ENGINE + r"""
oldFor=450;engage();
for(let i=0;i<4;i++)step();
assert.equal(seeks.length,1,'old advancing reads caused a corrective seek');
for(let i=0;i<12;i++)step();
assert.equal(seeks.length,2,'a distinct backward landing was not recovered');
// A forward displacement equal to the native seek loss overlaps the old line.
now+=4500;pending=null;base=10000;shift=0;synced=false;noise=false;apply();
for(let i=0;i<30;i++)step();
const before=seeks.length;ignored=true;engage(90);
for(let i=0;i<20;i++)step();
assert.equal(seeks.length,before+1,'an ignored/ambiguous seek was learned and retried');
assert.ok(!scShadowAt[2].landing,'ambiguous landing must expire');
""")

    def test_noop_sync_with_block_jitter_does_not_flush_the_decoder(self):
        self.js(self.ENGINE + r"""
engage(0);for(let i=0;i<35;i++){shift=[-12,18,-10,14,-2][i%5];step()}
assert.equal(seeks.length,0,'a metadata-only Sync plus block jitter caused a seek');
""")

    def test_landing_cancels_on_pause_rate_role_source_loss_or_native_ownership(self):
        for condition in ("pause", "rate", "role", "loss", "native", "cdj", "seek", "unload", "console", "display", "track"):
            with self.subTest(condition=condition):
                self.js(self.ENGINE + r"""
engage();assert.ok(scShadowAt[2].landing);
const before=seeks.length;
""" + {
                    "pause": "step(80,{playing:false});",
                    "rate": "rate=1.03;step();",
                    "role": "scFourDeckRole='12';step();",
                    "loss": "now+=1600;scShadowLandingTick(now);",
                    "native": "rate=2.03;step();",
                    "cdj": "step(80,{link:''});",
                    "seek": "scShadowSeek(2,scShadowAt[2],30000);",
                    "unload": "scTr('/Engine/Deck2/Track/SongLoaded').state=false;step();",
                    "console": "scPlayer=false;step();",
                    "display": "scShadowDisplayOnly=true;step();",
                    "track": "globalThis.scShadowLoad=()=>{};step(80,{id:'replacement'});",
                }[condition] + r"""
assert.ok(!scShadowAt[2].landing,'invalid ownership/lifecycle kept an observer');
assert.ok(seeks.length<=before+1,'cancellation issued an extra correction');
""")


class ShadowIdentityTest(unittest.TestCase):
    """Engine mode: the shadow deck holds the source's track, or is loaded again (maintainer report 11-10)."""
    js = ShadowDisplayTest.js

    # An Engine deck 2 that loads what it is told once settle() runs, as Engine does a little later.
    ENGINE = r"""
scShadowDisplayOnly = false; scShadowEngineMask = 30; scFourDeckRole = '13';
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

    # Maintainer report 11-10: while the copy ran the shadow deck said "No Track Loaded" and looked stuck.
    COPY = r"""
const copying = (id, extra) => Object.assign({deck: 2, path: '', id, key: 'key-' + id, loading: true, title: 'T ' + id, artist: 'Art', bpm: 128, durationMs: 300000, pitch: 0}, extra || {});
const all = e => scShadowApplyAll([Object.assign({}, e, {at: now})], false);
const busy = () => { const s = scShadowData(2); return scShadowBusy(s, s && s.meta, false); };
"""

    def test_copy_then_engine_load_is_covered_until_engine_holds_the_track(self):
        self.js(self.ENGINE + self.COPY + r"""
all(copying('a'));
let s = scShadowData(2);
assert.equal(scShadowVisible(2), true); assert.equal(scDeckLocked(2), true);
assert.equal(s.title, 'T a'); assert.equal(s.artist, 'Art'); assert.equal(s.bpm, 128); assert.equal(s.durationMs, 300000);
assert.equal(busy(), 1); assert.equal(s.progress, 0); // no Content-Length yet: the turning ring
assert.equal(requests.length, 1); assert(requests[0].url.endsWith('deck=2&key=key-a')); // cover and key of the source's track
requests[0].finish(200, {key: 'key-a', artworkUrl: '/art', keyName: '8A', overviewUrl: '/o.png'});
assert.equal(scShadowData(2).meta.keyName, '8A');
all(copying('a', {progress: 0.42}));
assert.equal(scShadowData(2).progress, 0.42); assert.equal(scShadowData(2).meta.keyName, '8A'); assert.equal(busy(), 1);
assert.equal(eng.asked.length, 0); assert.equal(requests.length, 1);
globalThis.scPressTimer = {restart() {}};
scPress('/Engine/Deck2/Sync'); assert.equal(scPressQueue.length, 1); // a covered Engine slot is still Engine's
// The copy is there: Engine loads it. The cover stays, the deck is never shown empty.
const e = src(A, {key: 'key-a', title: 'T a'});
all(e);
assert.deepEqual(eng.asked, [A]);
s = scShadowData(2);
assert.equal(scShadowVisible(2), true); assert.equal(busy(), 2); assert.equal(s.progress, 0); assert.equal(s.title, 'T a'); assert.equal(s.meta.keyName, '8A');
for (let i = 0; i < 10; i++) { now += 180; all(e); assert.equal(scShadowVisible(2), true); } // Engine takes its time
settle(); now += 180; all(e);
assert.equal(scShadowAt[2].ready, true);
assert.equal(scShadowData(2), null); assert.equal(scShadowVisible(2), false); // Engine's own deck view from here
for (let i = 0; i < 20; i++) { now += 180; all(e); assert.equal(scShadowVisible(2), false); } // and no cover flashes while it holds
// The source loads another track: covered at once, over the old track, with nothing of the old one.
all(copying('b', {progress: 0.07}));
s = scShadowData(2);
assert.equal(scShadowVisible(2), true); assert.equal(s.title, 'T b'); assert.equal(s.progress, 0.07); assert.equal(s.meta, null); assert.equal(busy(), 1);
assert(requests[requests.length - 1].url.endsWith('key=key-b'));
// Engine swapped the held track for another (Continue): asked again, covered again.
all(src(A, {key: 'key-a'})); settle(); now += 1400; all(src(A, {key: 'key-a'}));
assert.equal(scShadowVisible(2), false);
eng.held = B; now += 1200; all(src(A, {key: 'key-a'}));
assert.equal(scShadowAt[2].ready, false); assert.equal(scShadowVisible(2), true); assert.equal(busy(), 2);
""")

    def test_copy_failure_names_the_reason_and_progress_starts_again(self):
        self.js(self.ENGINE + self.COPY + r"""
all(copying('a', {progress: 0.6}));
assert.equal(busy(), 1);
all(copying('a', {error: 'unavailable'})); assert.equal(busy(), 4); assert.equal(scShadowData(2).progress, 0);
all(copying('a', {error: 'timeout'})); assert.equal(busy(), 5);
all(copying('a', {error: 'interrupted'})); assert.equal(busy(), 6);
all(copying('a', {error: 'failed'})); assert.equal(busy(), 6);
assert.equal(scShadowVisible(2), true); assert.equal(scShadowData(2).title, 'T a'); // the head stays through the failures
all(copying('a', {progress: 0.2})); // the next attempt delivers
assert.equal(busy(), 1); assert.equal(scShadowData(2).error, ''); assert.equal(scShadowData(2).progress, 0.2);
// Another track never shows the last one's progress or failure.
all(copying('a', {error: 'timeout'})); all(copying('b'));
assert.equal(busy(), 1); assert.equal(scShadowData(2).error, ''); assert.equal(scShadowData(2).progress, 0); assert.equal(scShadowData(2).key, 'key-b');
// The source ejects: no cover over an empty slot.
scShadowApplyAll([], false);
assert.equal(scShadowData(2), null);
""")

    def test_busy_state_of_a_display_slot_and_of_no_slot(self):
        self.js(r"""
assert.equal(scShadowBusy(null, null, false), 0);
const pending = {detailPending: true}, done = {detail: true}, plain = {overviewUrl: '/o.png'};
assert.equal(scShadowBusy({loading: false}, null, false), 3);    // no display data yet
assert.equal(scShadowBusy({loading: false}, pending, false), 3); // the detail waveform is being computed
assert.equal(scShadowBusy({loading: false}, done, true), 0);
assert.equal(scShadowBusy({loading: false}, plain, false), 0);   // a source with an overview only
assert.equal(scShadowBusy({loading: true}, plain, false), 3);
assert.equal(scShadowBusy({stale: true}, null, false), 0); assert.equal(scShadowBusy({metadataOnly: true}, null, false), 0);
// Display mode hands the source's share of the detail waveform to the lane, and forgets it with the track.
scShadowApplyAll([{deck: 2, key: 'A', title: 'A', progress: 0.4}]);
assert.equal(scShadowData(2).progress, 0.4); assert.equal(scShadowData(2).engine, false);
const first = scShadowData(2);
scShadowApplyAll([{deck: 2, key: 'A', title: 'A', progress: 0.4}]);
assert.equal(scShadowData(2), first); // nothing new: no binding runs
scShadowApplyAll([{deck: 2, key: 'A', title: 'A', progress: 0.7}]);
assert.equal(scShadowData(2).progress, 0.7);
scShadowApplyAll([{deck: 2, key: 'B', title: 'B'}]);
assert.equal(scShadowData(2).progress, 0); assert.equal(scShadowData(2).meta, null);
""")

    def test_busy_indicator_is_the_stock_ring_on_waveform_lanes_and_translated(self):
        stock = "import QtQuick 2.0\nimport EngineOS.UI.Style 1.0\nItem {\n    id: w\n}\n"
        ringed = bp.shadow_layer(stock, "waveform", True)
        self.assertIn("import EngineOS.UI.Controls 1.0 as ScStock\n", ringed)
        self.assertEqual(ringed.count("ScStock.ProgressRing {"), 2)
        for stock_style in ("backgroundColor: Style.surface03Color; ringColor: Style.font02Color; strokeWidth: 2", "color: Style.font02Color; font: Style.fontH1"):
            self.assertIn(stock_style, ringed)  # Deck/DownloadProgress.qml
        self.assertIn("visible: scDisplay.busyState > 0 && scDisplay.busyState < 4", ringed)  # a failed copy, a fallback's reason: the text alone
        plain = bp.shadow_layer(stock, "waveform")  # a QRC without the stock ring (5.0.4)
        self.assertNotIn("ScStock", plain)
        self.assertIn('objectName: "EngineX_Shadow_Busy"', plain)
        for kind in ("overview", "track", "performance"):
            other = bp.shadow_layer(stock, kind, True)
            self.assertNotIn("ScStock", other)
            self.assertNotIn("EngineX_Shadow_Busy", other)
            self.assertNotIn("@BUSY@", other)
        table = {en: t["zh-Hans"] for en, t in bp.load_dict().items()}
        text = next(l for l in bp.translate(ringed, table, set(), live=True).split("\n") if "busyState - 1" in l)
        for hans in ("正在下载", "正在加载", "等待源碟机分析波形", "文件不可用。", "未能在规定时间内打开文件。", "无法通过网络加载。", "正在重试…",
                     "流媒体：CDJ 无文件可拷贝。", "rekordbox Link：CDJ 无文件可拷贝。", "CD 或第二 USB：CDJ 无文件可拷贝。"):
            self.assertIn('Math.scI18n.t("%s")' % hans, text)
        self.assertNotRegex(text, "Downloading|Retrying|no file to copy")
        # The reasons stream-sync gives (cdjFallback) each have a line.
        for why in ("streaming", "link", "media", "network", "timeout"):
            self.assertRegex(text, r"[{ ]%s: " % why)

    def test_cdj_without_a_file_goes_to_the_display_renderer_with_the_reason(self):
        self.js(self.ENGINE + self.COPY + r"""
// Auto mode, a CDJ playing from a stick: Engine's deck while the copy runs.
all(copying('a', {key: ''}));
assert.equal(busy(), 1); assert.equal(scShadowData(2).engine, true); assert.equal(scShadowData(2).fallback, '');
assert.equal((scShadowEngineMask >> 2) & 1, 1);
// The copy failed: stream-sync hands the deck to the display renderer and says why.
const cdj = (why, key) => ({deck: 2, displayOnly: true, key: key || 'cdj:a', title: 'T a', fallback: why});
all(cdj('network'));
const s = scShadowData(2);
assert.equal(s.fallback, 'network'); assert.equal(s.engine, false); assert.equal((scShadowEngineMask >> 2) & 1, 0);
all(cdj('network')); assert.equal(scShadowData(2), s); // nothing new: no binding runs
all(cdj('timeout')); assert.notEqual(scShadowData(2), s); assert.equal(scShadowData(2).fallback, 'timeout');
// A streamed track never is Engine's.
all(cdj('streaming', 'cdj:b'));
assert.equal(scShadowData(2).fallback, 'streaming'); assert.equal(scShadowData(2).key, 'cdj:b'); assert.equal(eng.asked.length, 0);
// A later attempt delivered: Engine loads the copy, the reason is gone.
all(src(A, {key: ''}));
assert.equal((scShadowEngineMask >> 2) & 1, 1); assert.deepEqual(eng.asked, [A]); assert.equal(scShadowData(2).fallback, '');
""")

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
scShadowDisplayOnly = false; scShadowEngineMask = 30; scFourDeckRole = '13';
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


class StockJogControlsTest(unittest.TestCase):
    def test_jog_and_vinyl_keep_stock_grid_edit_behavior(self):
        # #157 stays closed with stock behavior, including during playback.
        old_root = bp.ROOT
        try:
            for root in ("Skins/Embedded/", "Skins/EmbeddedCommon/"):
                bp.ROOT = root
                for dev in (False, True):
                    with self.subTest(root=root, dev=dev):
                        edits = bp.edits(dev)
                        for name in ("Vinyl", "JogWheel", "BrowseEncoder", "DeckAction"):
                            self.assertFalse(f"ControlSurfaceModules/ControlSurfaceModules/{name}.qml" in edits, name)
        finally:
            bp.ROOT = old_root


class MasterLedStockTest(unittest.TestCase):
    @unittest.skipUnless(shutil.which("node"), "node needed for the MASTER LED behavior checks")
    def test_own_master_path_keeps_stock_dim_level_and_manual_request(self):
        rel = "ControlSurfaceModules/ControlSurfaceModules/Sync.qml"
        qrc = LAB / "5.3.0/qrc"
        if not qrc.is_dir():
            self.skipTest(f"{qrc} not found")
        stock = (qrc / rel).read_text()
        old_root = bp.ROOT
        try:
            bp.ROOT = "Skins/Embedded/"
            text = stock
            for old, new in bp.edits(False)[rel]:
                text = bp.patch(text, old, new, rel)
        finally:
            bp.ROOT = old_root
        # Only the status path differs: the LED body and manual MASTER path stay stock.
        self.assertEqual(text.split('objectName: "Master Deck LED"', 1)[1],
                         stock.split('objectName: "Master Deck LED"', 1)[1])
        self.assertEqual(text.split("            ValueNoteAssignment {", 1)[1].split("            OutputAssignment {", 1)[0],
                         stock.split("            ValueNoteAssignment {", 1)[1].split("            OutputAssignment {", 1)[0])
        path = text.split("readonly property string isMasterPath:", 1)[1].split("ValueNoteAssignment {", 1)[0].strip()
        led = text.split('objectName: "Master Deck LED"', 1)[1].split("readonly property int ledValue: {", 1)[1].split("\n                }", 1)[0]
        script = "const assert = require('node:assert/strict'), vm = require('node:vm');\n"
        script += "const path = " + json.dumps(path) + ", led = " + json.dumps(led) + ";\n"
        script += r"""
const players = ['JP07', 'JP08', 'JP13', 'JP14'];
for (const board of [...players, 'JP11', 'JC11', 'JC16', 'NH08', 'NH08S', 'NH10', 'JP21', 'JC11S', 'JP24', 'JP20', 'JP11S', 'RMZ2']) {
    for (const isPlayer of [false, true]) for (const hasSyncing of [false, true]) {
        const Config = {device: {name: board, isPlayer}, network: {hasSyncing}};
        const deckConfig = {activeDeckPath: '/Engine/Deck3/', inDJMode: true};
        const own = !hasSyncing || (!isPlayer && players.includes(board));
        assert.equal(vm.runInNewContext(path, {Config, deckConfig}), own ? '/Engine/Deck3/DeckIsMaster' : '/Engine/Sync/Network/MasterStatus');
        for (const masterStatus of [false, true]) for (const inDJMode of [false, true]) {
            deckConfig.inDJMode = inDJMode;
            assert.equal(vm.runInNewContext(led, {Config, deckConfig, masterStatus}), inDJMode ? (masterStatus ? 127 : 1) : 0, board);
        }
    }
}
"""
        result = subprocess.run(["node", "-e", script], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)


class MasterPressTest(unittest.TestCase):
    """#217 review: MASTER pressed on the deck that is Engine's master already (a console with one deck
    loaded) must reach stream-sync, which asks the CDJ for the Pro DJ Link master again."""

    def test_user_presses_count_and_our_own_do_not(self):
        # As measured in Engine 5.3.0 (twin sandbox): RequestMaster is an "Action" translator, writing
        # state = true leaves state false, fires no stateChanged and adds one to stateTrueCount.
        ShadowDisplayTest.js(self, r"""
const action = () => ({stateTrueCount: 0, get state() { return false }, set state(on) { if (on) this.stateTrueCount++ }});
const t1 = action(), t3 = action();
assert.equal(scMasterPresses(1, t1), 0);
t1.state = true; // the control surface (ActionOutput): true on the press, nothing on the release
assert.equal(scMasterPresses(1, t1), 1);
scPressWrite(t1, '/Engine/Deck1/RequestMaster', true); scPressWrite(t1, '/Engine/Deck1/RequestMaster', false); // our own press
assert.deepEqual([t1.stateTrueCount, scMasterPresses(1, t1)], [2, 1]);
scPressWrite(t3, '/Engine/Deck3/RequestMaster', true); // ours on another deck is not this deck's
t1.state = true;
assert.deepEqual([scMasterPresses(1, t1), scMasterPresses(3, t3)], [2, 0]);
const sync = action();
scPressWrite(sync, '/Engine/Deck1/Sync', true); // another button of the deck
assert.deepEqual([sync.stateTrueCount, scMasterPresses(1, t1)], [1, 2]);
assert.equal(scMasterPresses(2, {}), 0); // no such property on this model
""")

    def test_the_count_is_read_and_posted(self):
        main = bp.SERVICES_MAIN
        # Every press we make goes through the one writer that keeps ours apart.
        self.assertIn("\t\t\t\troot.scPressWrite(t, e.path, e.step === 1)\n", main)
        self.assertEqual(main.count("t.state = e.step === 1"), 0)
        self.assertIn('["requestMaster", "/RequestMaster"]', main)
        # The count goes out with the deck, and a new press alone makes a full post.
        self.assertIn("o.masterPress = scMasterPresses(d, g.requestMaster)\n", main)
        self.assertIn("sent.masterPress !== o.masterPress", main)
        self.assertIn("sent.masterPress = o.masterPress;", main)


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

    def normal_tick(self, pitch, moved):
        ShadowDisplayTest.js(self, r"""
globalThis.XMLHttpRequest = class { open() { this.readyState = 1 } send() { this.readyState = 4; this.status = 200 } };
globalThis.scShadowPoll = () => {};
const T0 = now, pitch = PITCH, speed = 1 + pitch / 100;
let moved = 0;
globalThis.scTr = path => ({value: path.endsWith('/SampleRate') ? 44100 : 0, color: '#ffffff', index: 0,
    state: path.startsWith('/Engine/Deck1/') && (path.endsWith('/SongLoaded') || path.endsWith('/PlayState')),
    get unnormalized() { return path.endsWith('/Deck1/Track/TrackPosition') ? (10000 + (now - T0) * speed + moved) * 44.1 : path.endsWith('/TrackLength') ? 400 * 44100 : path.endsWith('/OriginalBPM') ? 120 : path.endsWith('/CurrentBPM') ? 120 * speed : 0 },
    get string() { return path === '/Engine/Deck1/Track/TrackUri' ? 'file:///a.mp3' : path.endsWith('/SpeedState') ? '+' + pitch + '%' : '' }});
for (let i = 0; i < 80; i++) { now += scShadowTickTimer.interval; scShadowTick() }
assert.equal(scShadowTickTimer.interval, 80);
assert.equal(scShadowPrev[1].pitch, pitch);
const jumps = scShadowPrev[1].jumps;
moved = MOVED; now += 80; scShadowTick();
assert.ok(Math.abs(scShadowPrev[1].posMs - (10000 + (now - T0) * speed + moved)) < 0.001,
    'a normal 80 ms tick must use the sampled position');
assert.equal(scShadowPrev[1].jumps, jumps + (Math.abs(moved) > 180 ? 1 : 0));
""".replace('PITCH', str(pitch)).replace('MOVED', str(moved)))

    def test_normal_80ms_at_pitch_100(self):
        self.normal_tick(100, -40)

    def test_normal_80ms_at_pitch_zero(self):
        self.normal_tick(0, -20)

    def test_jump_back_5s_at_pitch_100(self):
        self.normal_tick(100, -5000)


@unittest.skipUnless(shutil.which("node"), "node needed for the shadow-deck behavioral checks")
class ShadowHeadSyncLeadTest(unittest.TestCase):
    """Engine mode: the copy is in neither Sync nor the master selection, so its stock header said "not synced"
    while the source's BPM was green, and drew the phase meter while the source showed LEAD (g10-e-z3-L)."""

    def test_final_bindings_use_explicit_false_and_fall_back_only_when_missing(self):
        bp.ROOT = 'Skins/Embedded/'
        patches = bp.edits(False)
        for path, marker in (
            ('Skins/Embedded/Views/MainView/Components/JP07SyncIndicator.qml', 'property bool isMaster:'),
            ('Skins/EngineOS/UI/CompositeControls/Deck/DeckItem/BpmInfo.qml', 'property bool syncEnabled:'),
        ):
            replacement = next(new for old, new in patches[path] if marker in new)
            expression = replacement.split(':', 1)[1].strip()
            ShadowDisplayTest.js(self, r"""
globalThis.deckIndex = 2;
globalThis.pDeckIsMaster = {translator: {state: true}};
globalThis.deckController = {syncInfoViewController: {syncEnabled: true}};
let source = {master: false, sync: false};
globalThis.scShadowHeadOf = () => source;
const result = () => !!(EXPRESSION);
assert.equal(result(), false, 'source false overrides local true');
pDeckIsMaster.translator.state = deckController.syncInfoViewController.syncEnabled = false;
source = {master: true, sync: true}; assert.equal(result(), true);
source = {}; assert.equal(result(), false, 'missing field keeps the stock local binding');
pDeckIsMaster.translator.state = deckController.syncInfoViewController.syncEnabled = true;
assert.equal(result(), true);
""".replace('EXPRESSION', expression))

    def test_the_shadow_header_gets_the_sources_sync_and_master(self):
        ShadowDisplayTest.js(self, r"""
Object.assign(globalThis, {scShadowApply() {}, scShadowEmpty() {}});
const deck = (d, o) => Object.assign({deck: d, path: '/c/a.mp3', id: 'k'}, o);
const decks = [deck(2, {sync: true}), deck(4, {lead: true, master: true}), deck(1, {lead: true}), {deck: 3, sync: true}];
scShadowApplyAll(decks, false);
// Role 13: decks 1 and 3 are this player's own, their headers stay Engine's.
const flags = () => Object.keys(scShadowHead).map(d => d + (scShadowHead[d].master ? 'M' : '') + (scShadowHead[d].sync ? 'S' : '')).join();
assert.equal(flags(), '2S,4M');
// `master` alone: the player holds the network master, no deck of it shows LEAD (R2 after a restart).
decks[1] = deck(4, {master: true, lead: false}); scShadowApplyAll(decks, false); assert.equal(flags(), '2S,4');
decks[1] = deck(4, {lead: true}); scShadowApplyAll(decks, false); assert.equal(flags(), '2S,4M'); // stream-sync clears `master` for the copy
decks[1] = deck(4, {master: true}); scShadowApplyAll(decks, false); assert.equal(flags(), '2S,4M'); // an older sender omitted LEAD
const same = scShadowHead;
scShadowApplyAll(decks, false);
assert.strictEqual(scShadowHead, same, 'an unchanged state must not re-evaluate the header bindings');
decks[1].path = ''; // the source ejected
scShadowApplyAll(decks, false);
assert.equal(flags(), '2S');
scShadowApplyAll([], true); // display mode draws its own header
assert.deepEqual(scShadowHead, {});
""")
        self.assertIn('head("deckIndex", "master",', SRC)
        self.assertIn("o.lead = s.master;", SRC)  # the source posts its own DeckIsMaster, what its header shows
        self.assertIn("sent.lead !== o.lead", SRC)  # and posts at once when only that changes
        self.assertIn('head("deckIndex", "sync",', SRC)


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
assert.deepEqual(scShadowHead[2], {master: undefined, sync: false, title: 'A', artist: 'One', keyName: '-', loop: false, keyOffset: 0, stems: false, slip: 0});
assert.deepEqual(scShadowHead[4], {master: undefined, sync: false, title: '', artist: '', keyName: '', loop: false, keyOffset: 0, stems: false, slip: 0}); // a CDJ or an older sender: stock texts
""")
        for anchor in ('scaled[bj + "Deck/DeckItem/ArtistSong.qml"]', 'scaled[bj + "KeyEditor/KeyInfo.qml"]', "root.scShadowHeadOf(microDeck.deckIndex).title"):
            self.assertIn(anchor, SRC)


@unittest.skipUnless(shutil.which("node"), "node needed for the shadow-deck behavioral checks")
class ShadowHeadStaysWithLastTrackTest(unittest.TestCase):
    """Engine mode: the source loaded another track and, until the last one was unloaded 0.4-0.5 s later, the
    shadow header read the copy's file name and analysed key ("Young Thug - … Throw Sum Mo.mp3", 8B; hw19-before-R2)."""

    def test_the_header_keeps_the_sources_texts_until_the_last_track_is_unloaded(self):
        ShadowDisplayTest.js(self, r"""
const tr = {};
globalThis.scTr = path => tr[path] || (tr[path] = {state: path.endsWith('/SongLoaded'), string: 'Off', value: 0});
Object.assign(globalThis, {scShadowApply() {}, scPlayer: true});
const a = {deck: 2, path: '/c/One - A.mp3', id: 'k', title: 'A', artist: 'One', keyName: '6B'};
scShadowApplyAll([a], false);
scShadowAt[2] = {path: a.path, id: a.id}; // loaded
const next = {deck: 2, path: '', id: 'j', loading: true, title: 'B', artist: 'Two', keyName: '-'};
scShadowApplyAll([next], false);
assert.equal(scShadowHead[2].title, 'A', 'the last track is still on the deck: its title, not the file name');
assert.equal(scShadowHead[2].keyName, '6B');
now += 400; scShadowApplyAll([next], false); // unloaded 350 ms after the change
assert.equal(scShadowAt[2], undefined);
assert.equal(scShadowHead[2], undefined, 'an empty deck has no source texts');
scShadowApplyAll([Object.assign({}, next, {path: '/c/Two - B.mp3', loading: false})], false);
assert.equal(scShadowHead[2].title, 'B');
// The peer went away: the same, until the deck is emptied.
scShadowAt[2] = {path: '/c/Two - B.mp3', id: 'j'};
scShadowApplyAll([], false); assert.equal(scShadowHead[2].title, 'B');
now += 400; scShadowApplyAll([], false); assert.equal(scShadowHead[2], undefined);
""")


@unittest.skipUnless(shutil.which("node"), "node needed for the shadow-deck behavioral checks")
class ShadowHeadDeckStateTest(unittest.TestCase):
    """Engine mode: the source's header lit its loop, showed a key shift (+1) and the Stems icon, and its overview
    drew the slip line; the shadow deck of it showed none of them (the copy takes no loop, shift or slip)."""

    def test_the_source_posts_loop_key_shift_stems_and_slip_and_the_shadow_deck_shows_them(self):
        ShadowDisplayTest.js(self, r"""
const bodies = [];
globalThis.XMLHttpRequest = class {
    open(method, url) { this.url = url; this.readyState = 1 }
    send(body) { if (body) bodies.push(JSON.parse(body)[0]); this.readyState = 4; this.status = 200; if (this.onreadystatechange) this.onreadystatechange() }
};
globalThis.scShadowPoll = () => {};
const deck = {key: '6B', loop: false, shift: 0, stems: false, slip: false, slipAt: 0};
const states = {'/SongLoaded': () => true, '/Track/LoopEnableState': () => deck.loop, '/Track/Stemmed': () => deck.stems, '/Track/SlipModeActive': () => deck.slip};
globalThis.scTr = path => ({value: path.endsWith('/SampleRate') ? 44100 : 0, color: '#ffffff', index: 0,
    get state() { const f = path.startsWith('/Engine/Deck1/') && Object.keys(states).find(k => path.endsWith(k)); return f ? states[f]() : false },
    get unnormalized() { return !path.startsWith('/Engine/Deck1/') ? 0 : path.endsWith('/TrackLength') ? 200 * 44100 : path.endsWith('/KeyOffset') ? deck.shift : path.endsWith('/SlipModePosition') ? deck.slipAt : 0 },
    get string() { return !path.startsWith('/Engine/Deck1/') ? '' : path.endsWith('/TrackUri') ? 'file:///a.mp3' : path.endsWith('/SongName') ? 'A' : path.endsWith('/ArtistName') ? 'One' : path.endsWith('/Track/CurrentKey') ? deck.key : '' }});
const run = ms => { for (let t = 0; t < ms; t += 40) { now += 40; scShadowTick() } };
const last = () => bodies[bodies.length - 1];
run(3000);
assert.deepEqual([last().loop, last().keyOffset, last().stems, last().slip], [false, 0, false, 0]);
// Each of them is told at the next heartbeat (0.5 s), not at the 5 s full post; a shift tells the key's new name with it.
let n = bodies.length;
deck.loop = true; run(520); assert.ok(bodies.length > n && last().loop, 'a loop is told within 0.5 s');
n = bodies.length; deck.shift = 1; deck.key = '1B'; run(520);
assert.ok(bodies.length > n, 'a key shift is told within 0.5 s'); assert.deepEqual([last().keyOffset, last().keyName], [1, '1B']);
n = bodies.length; deck.stems = true; run(520); assert.ok(bodies.length > n && last().stems);
n = bodies.length; deck.slip = true; deck.slipAt = 441000; run(520); assert.ok(bodies.length > n, 'slip is told within 0.5 s'); assert.equal(last().slip, 441000);
// The slip timeline runs on: told at every heartbeat while slip is active, then its end.
n = bodies.length; for (let i = 0; i < 50; i++) { deck.slipAt += 1764; run(40) }
assert.ok(bodies.length - n === 4, 'slip posts in 2 s: ' + (bodies.length - n));
assert.equal(last().slip > 441000, true);
n = bodies.length; deck.slip = false; run(520); assert.ok(bodies.length > n, 'the end of slip is told within 0.5 s'); assert.equal(last().slip, 0);
n = bodies.length; run(1000); assert.equal(bodies.length, n, 'nothing changed: no full post');
// The peer: its header and overview read them from the source's post.
Object.assign(globalThis, {scShadowApply() {}, scShadowEmpty() {}});
scShadowApplyAll([{deck: 2, path: '/c/One - A.mp3', id: 'k', title: 'A', artist: 'One', keyName: '1B', loop: true, keyOffset: 1, stems: true, slip: 441000}], false);
assert.deepEqual(scShadowHead[2], {master: undefined, sync: false, title: 'A', artist: 'One', keyName: '1B', loop: true, keyOffset: 1, stems: true, slip: 441000});
""")
        for anchor in ('scaled.setdefault(bj + "Deck/DeckItem/AutoLoop.qml", [])', "scHead.title ? scHead.stems : view.stemmed", "scHead.title ? scHead.keyOffset :",
                       "root.scShadowHeadOf(deckIndex).slip || 0", "scSlip > 0 ? scSlip : isSlipModeActive"):
            self.assertIn(anchor, SRC)


class PatchTableIntegrityTest(unittest.TestCase):
    def test_patch_dict_rejects_duplicate_key_overwrites_during_pack_build(self):
        import ast

        d = bp.PatchDict({"a.qml": [("old1", "new1")]})
        d["a.qml"] += [("old2", "new2")]
        d["a.qml"].extend([("old3", "new3")])
        self.assertEqual(len(d["a.qml"]), 3)
        with self.assertRaises(ValueError):
            d["a.qml"] = [("other", "replacement")]

        tree = ast.parse(SRC)
        for node in ast.walk(tree):
            if isinstance(node, ast.Dict):
                seen = set()
                for k in node.keys:
                    if k is None:
                        continue
                    expr = ast.unparse(k)
                    self.assertNotIn(expr, seen, f"duplicate dict literal key at line {k.lineno}: {expr}")
                    seen.add(expr)

        old_root = getattr(bp, "ROOT", None)
        try:
            for root in ("Skins/Embedded/", "Skins/EmbeddedCommon/"):
                bp.ROOT = root
                self.assertIsInstance(bp.legacy_qml_fixes(), bp.PatchDict)
                for dev in (False, True):
                    out = bp.edits(dev)
                    self.assertIsInstance(out, bp.PatchDict)
        finally:
            bp.ROOT = old_root

        for ver in ("5.0.4", "5.3.0"):
            qrc = LAB / ver / "qrc"
            if not qrc.is_dir():
                continue
            with tempfile.TemporaryDirectory(prefix=f"ui-dedup-{ver}-") as tmp:
                for extra in ([], ["--dev"]):
                    res = subprocess.run(
                        ["python3", str(BUILD_PACKS), "--qrc", str(qrc), "--out", str(Path(tmp) / "packs"), *extra],
                        capture_output=True, text=True,
                    )
                    self.assertEqual(res.returncode, 0, res.stderr)

    def test_artist_song_retains_both_shadow_head_and_hide_title_artist_patches(self):
        qrc = LAB / "5.3.0" / "qrc"
        if not qrc.is_dir():
            self.skipTest(f"{qrc} not found (run on Oracle-Batam-arm)")
        with tempfile.TemporaryDirectory(prefix="ui-artistsong-") as tmp:
            out = Path(tmp) / "packs"
            res = subprocess.run(["python3", str(BUILD_PACKS), "--qrc", str(qrc), "--out", str(out)],
                                 capture_output=True, text=True)
            self.assertEqual(res.returncode, 0, res.stderr)
            for lang in ("en", "zh-Hans", "zh-Hant"):
                qml = (out / lang / "Skins/EngineOS/UI/CompositeControls/Deck/DeckItem/ArtistSong.qml").read_text()
                for expected in (
                    'readonly property var scHead: typeof root !== "undefined" && root && root.scShadowHeadOf ? root.scShadowHeadOf(deckIndex) : null',
                    'readonly property string songTextString: deckLoaded ? (displayText.hideTitleArtist ? "" : (scHead && scHead.title) || displayText.display) : deckLabel',
                    'readonly property string artistTextString: deckLoaded ? (displayText.hideTitleArtist ? "" : (scHead && scHead.title ? scHead.artist : artistName)) : "No Track Loaded"',
                    'readonly property bool onlyFileName: view.onlyFileName && !(scHead && scHead.title)',
                    'readonly property bool isStemmed: scHead && scHead.title ? scHead.stems : view.stemmed',
                    'respectHideTitleArtist: true',
                    'visible: artistSong.artistVisible && !artistSong.onlyFileName && !displayText.hideTitleArtist && artistSong.artistTextString.length > 0',
                ):
                    self.assertIn(expected, qml)

        if shutil.which("node"):
            ShadowDisplayTest.js(self, r"""
let deckLoaded = true, deckLabel = 'Deck 2', artistName = 'Local Artist', view = {onlyFileName: false, stemmed: false};
let displayText = {hideTitleArtist: false, display: 'Local Song'};
let scHead = {title: 'Shadow Song', artist: 'Shadow Artist', stems: true};
const songTextString = () => deckLoaded ? (displayText.hideTitleArtist ? "" : (scHead && scHead.title) || displayText.display) : deckLabel;
const artistTextString = () => deckLoaded ? (displayText.hideTitleArtist ? "" : (scHead && scHead.title ? scHead.artist : artistName)) : "No Track Loaded";
const onlyFileName = () => view.onlyFileName && !(scHead && scHead.title);
const isStemmed = () => scHead && scHead.title ? scHead.stems : view.stemmed;

assert.equal(songTextString(), 'Shadow Song');
assert.equal(artistTextString(), 'Shadow Artist');
assert.equal(onlyFileName(), false);
assert.equal(isStemmed(), true);

displayText.hideTitleArtist = true;
displayText.display = '';
assert.equal(songTextString(), '', 'shadow song title hidden when Hide Title/Artist is on');
assert.equal(artistTextString(), '', 'shadow artist hidden when Hide Title/Artist is on');

scHead = null;
assert.equal(songTextString(), '', 'local song title hidden when Hide Title/Artist is on');
assert.equal(artistTextString(), '', 'local artist hidden when Hide Title/Artist is on');

displayText.hideTitleArtist = false;
displayText.display = 'track_file.flac';
view.onlyFileName = true;
assert.equal(songTextString(), 'track_file.flac', 'Display File Name preserved on local deck');
assert.equal(artistTextString(), 'Local Artist');
assert.equal(onlyFileName(), true);
""")


@unittest.skipUnless(shutil.which("node"), "node needed for the shadow-deck behavioral checks")
class ShadowNativeOverRangeTest(unittest.TestCase):
    def test_sender_uses_native_tempo_when_the_speed_display_is_clipped(self):
        ShadowDisplayTest.js(self, r"""
globalThis.XMLHttpRequest = class { open(){this.readyState=1} send(){this.readyState=4;this.status=200} };
globalThis.scShadowPoll = () => {};
const T0=now, ratio=144.2/71.00345611572266;
let shown='+100.0%';
globalThis.scTr = path => ({
 value:path.endsWith('/SampleRate')?44100:0, color:'#fff', index:0,
 state:path.startsWith('/Engine/Deck1/') && (path.endsWith('/SongLoaded') || path.endsWith('/PlayState')),
 get unnormalized(){return path.endsWith('/Deck1/Track/TrackPosition') ? (10000+(now-T0)*ratio)*44.1 :
   path.endsWith('/TrackLength') ? 400*44100 : path.endsWith('/OriginalBPM') ? 71.00345611572266 : path.endsWith('/CurrentBPM') ? 144.2 : 0},
 get string(){return path.endsWith('/SpeedState')?shown:path.endsWith('/SyncMode')?'BeatOrBarSync':path.endsWith('/TrackUri')?'file:///a.mp3':''}
});
for(let i=0;i<40;i++){now+=80;scShadowTick();}
assert.equal(scShadowPrev[1].pitch,103.09);
assert.equal(scShadowPrev[1].jumps,0,'the true tempo must not become a jump on every tick');
shown='+0.0%';now+=1001;scShadowTick();
assert.equal(scShadowPrev[1].pitch,0,'mismatched BPMs during a load must still fall back to the unclipped display');
""")

    def test_peer_copy_uses_native_sync_and_releases_it_without_a_clipped_write(self):
        ShadowDisplayTest.js(self, r"""
Object.assign(globalThis,{scPlayer:true,scFourDeckRole:'13',scShadowDisplayOnly:false,scShadowEngineMask:30});
const props=new Map(), presses=[], speeds=[];
globalThis.scShadowBent=null;
let sync='Off';
globalThis.scTr = path => {
 if(props.has(path))return props.get(path);
 const p={
  state:path.endsWith('/SongLoaded')||path.endsWith('/PlayState'),
  entries:['4','8','10','20','50','100'],index:5,value:path.endsWith('/SampleRate')?44100:0,url:'',
  get string(){return path.endsWith('/SyncMode')?sync:path.endsWith('/SpeedRange')?'100':''},
  get unnormalized(){return path.endsWith('/TrackLength')?44100*400:path.endsWith('/TrackPosition')?5000*44.1:0}
 };props.set(path,p);return p;
};
globalThis.scShadowHolds=()=>true;
globalThis.scShadowCues=()=>{};
globalThis.scShadowEnd=()=>false;
globalThis.scShadowBeat=()=>{};
globalThis.scShadowSpeed=(d,p,r)=>speeds.push([d,p,r]);
globalThis.scPress=p=>{presses.push(p);if(p.endsWith('/UnSync'))sync='Off';else if(p.endsWith('/Sync'))sync='BeatOrBarSync'};
scShadowAt[2]={path:'/a',id:'a',ready:true,seekAt:now};
let seq=0;
const apply=(extra={})=>{
 now+=1000;
 scShadowApply(Object.assign({deck:2,path:'/a',id:'a',link:'peer',sync:true,playing:true,
  positionMs:5000,at:now,pitch:103.09,bpm:71.003456,currentBpm:144.2,range:100,seq:++seq},extra));
};
apply();
assert.equal(presses.pop(),'/Engine/Deck2/Sync');
assert.equal(speeds.length,0,'a clipped fader must never replace native Sync');
assert.equal(scShadowAt[2].nativeSync,true);
apply();
assert.equal(speeds.length,0);
assert.equal(presses.length,0,'do not keep pressing an acknowledged Sync');
// Beat Sync can select another beat: the copy must retain its position loop.
for(let i=0;i<3;i++)apply({positionMs:6000});
assert.equal(scShadowAt[2].seeks,1,'native Sync must not leave a one-beat song-position error');
apply({sync:false});
assert.equal(presses.pop(),'/Engine/Deck2/UnSync');
apply({sync:false});
assert.equal(speeds.length,0,'UnSync retains its native rate until the source moves the fader');
apply({sync:false,pitch:20,currentBpm:85.2041472});
assert.equal(scShadowAt[2].nativeSync,false);
assert.equal(speeds.length,1,'normal fader control resumes inside the range');
assert.ok(Math.abs(speeds[0][1]-20)<0.001);

// Ordinary linked Sync stays on the existing fader/position path.
presses.length=0;speeds.length=0;
apply({pitch:3,currentBpm:73.13355968});
assert.equal(presses.length,0);
assert.equal(speeds.length,1);
// A replacement track retains ownership so its Sync can be released.
scShadowAt[2].nativeSync=true;
apply({path:'/b',id:'b',sync:false});
assert.equal(scShadowAt[2].nativeSync,true);
// Eject releases the owned native Sync too.
sync='TempoSync';scShadowAt[2].link='peer';scShadowAt[2].emptyAt=now-1000;
scShadowEmpty(2,now);
assert.ok(presses.includes('/Engine/Deck2/UnSync'));
assert.ok(!presses.some(p=>p.endsWith('/RequestMaster')),'a copy must not request MASTER');
""")


class I18nTranslateRegressionTest(unittest.TestCase):
    # Official English terms intentionally kept identical in zh-Hans / zh-Hant, with reasons.
    KEEP_ENGLISH = {
        "Layer A": "official SC5000/SC6000 hardware deck layer name",
        "Layer B": "official SC5000/SC6000 hardware deck layer name",
        "Denon": "official hardware brand name in Waveform Style",
        "RGB": "standard DJ RGB waveform color acronym",
        "Cue / Loop": "standard DJ Cue / Loop tab name",
        "Engine": "official Engine OS input/brand name",
        "ENGINE CONNECT": "official Denon DJ network protocol name",
        "Spiral": "official Pioneer DJ Color/Beat FX proper name",
        "Pitch Echo": "official Pioneer DJ Color FX proper name",
        "Slip Roll": "official Pioneer DJ Beat FX proper name",
        "Dub Echo": "official Pioneer DJ / Denon Sweep FX proper name",
        "Space": "official Pioneer DJ Color FX proper name",
        "Open Key": "official key notation system proper name",
        "Camelot": "official Camelot wheel key notation proper name",
        "Ableton Link": "official network sync protocol/product name",
        "Wi-Fi": "universal wireless standard name",
        "Spotify": "official streaming service brand name",
        "320kbps": "universal audio bitrate notation",
        "English": "self-named English language option",
        "3-Band": "waveform style name; the maintainer keeps it English (never 三频段)",
        "Tri-Band": "waveform style name; the maintainer keeps it English (never 三频段)",
    }

    DISPLAY_TAB_STRINGS = (
        "Display",
        "Reset Played Tracks", "Reset",
        "Track Title", "Filename", "Metadata",
        "Time Format", "Static", "Dynamic",
        "Track End Warning", "Off",
        "On Air Mode",
        "Sync Indicator", "Beat Phase", "Beat Keeper",
        "Waveform Style", "Denon", "RGB", "3-Band", "Tri-Band",
        "RGB Waveform Creation",
        "RGB Waveforms and Track Overviews will be built when loading to deck.",
        "Track Overview Style", "Half", "Full",
    )

    MIXER_TAB_STRINGS = (
        "Mixer",
        "Sampler Output", "Layer A", "Layer B", "Ch 1", "Ch 2", "Ch 3", "Ch 4", "Main",
        "Playlist Deck Crossfade Time", "Off",
        "Master Limiter",
        "VU Meter Mode", "Channel",
        "EQ Type", "Isolate", "Normal",
        "ISO EQ High Xover", "ISO EQ Low Xover",
        "Headphone Gain",
        "Smart Headphone Cue",
        "Cue Solo Mode",
        "Split Cue Output", "Invert",
        "Sum Main Out to Mono",
        "Output Equalizer", "Open",
        "X-Fader Cut In Length Left", "X-Fader Cut In Length Right",
        "Fader Echo", "All Faders", "Upfader Only",
        "Calibrate Crossfader", "Calibrate",
    )

    OTHER_SETTINGS_REQUIRED = (
        # Layout
        "Classic", "Wave", "Art", "Single", "Dual (Stacked)", "Dual (Grouped)",
        "2 Decks", "4 Decks", "2 Waveforms", "4 Waveforms", "Horizontal", "Vertical",
        "Regular", "Swapped",
        # Profile / Playback / Cues & Loops / Safety / Library
        "Save to my drive", "Saved", "Load drive profile to device?",
        "Instant", "Motor Torque", "Cue Position", "Track Start", "Censor",
        "Acapella/Instrumental FX", "Primary", "Min",
        "Bar", "Beat", "Tempo", "Toggle", "Shift Disable",
        "Pitch Bend", "Range", "Fixed", "Progressive", "Strict", "Fuzzy",
        "Momentary", "Trigger", "Reloop", "Disable", "Enable",
        "Long Press", "Sharps", "Flats", "Match", "Compatible",
        # Device / Fx / Mic / Services
        "Smart", "15m", "30m", "60m", "MPCe LED Bloom",
        "Kill", "Bleed", "Fader Echo Beats", "Fader Echo Feedback", "Default", "Recall",
        "Fast", "Mic Attenuation", "Mic 1 Attenuation", "Mic 2 Attenuation",
        "Gate Threshold", "Talkover Threshold", "Mic 1 EQ", "Mic 2 EQ", "EQ",
        "Mic Effects", "Mic FX", "Mic FX Amount", "Mic FX Select",
        "Ableton Link Offset",
    )

    def test_empty_quote_does_not_desync_shadow_badge_literals_and_labeltext_translates(self):
        rows = bp.load_dict()
        for lang, expected in (
            ("zh-Hans", ("离线 · ", "仅曲目信息", "等待源碟机分析波形", "概览波形", "关闭")),
            ("zh-Hant", ("離線 · ", "僅曲目資訊", "等待來源碟機分析波形", "概覽波形", "關閉")),
        ):
            table = {en: t[lang] for en, t in rows.items()}
            badge = (
                '            text: !scDisplay.sample ? "" : (scDisplay.stale ? "OFFLINE · " : "") + '
                '(scDisplay.sample.metadataOnly ? "REMOTE" : scDisplay.sample.engine || scDisplay.busyState > 0 ? "" : '
                '!scDisplay.meta ? "Waiting for source analysis" : !scDisplay.detailed ? "OVERVIEW" : "")'
            )
            translated = bp.translate(badge, table, set(), live=True)
            for item in expected[:4]:
                self.assertIn(item, translated)
            btn = bp.translate('            labelText: "Dismiss"', table, set(), live=True)
            self.assertIn(expected[4], btn)

    def test_layer_stays_english_and_player_uses_deck_term(self):
        rows = bp.load_dict()
        for layer_key in ("Layer A", "Layer B"):
            self.assertIn(layer_key, rows)
            self.assertEqual(rows[layer_key]["zh-Hans"], layer_key)
            self.assertEqual(rows[layer_key]["zh-Hant"], layer_key)
        for en, t in rows.items():
            for lang in ("zh-Hans", "zh-Hant"):
                self.assertNotIn("图层", t[lang], f"{en} ({lang}) must never translate deck Layer as 图层")
                self.assertNotIn("圖層", t[lang], f"{en} ({lang}) must never translate deck Layer as 圖層")
                if "player" in en.lower():
                    self.assertNotIn("播放器", t[lang], f"{en} ({lang}) must use 碟机, not 播放器")
                    self.assertNotIn("播放機", t[lang], f"{en} ({lang}) must use 碟機, not 播放機")
        self.assertEqual(rows["Player"], {"zh-Hans": "碟机", "zh-Hant": "碟機"})
        for band in ("3-Band", "Tri-Band"):  # waveform style stays English on the deck
            self.assertNotIn(band, rows)
        self.assertEqual(rows["Player #"], {"zh-Hans": "碟机编号 #", "zh-Hant": "碟機編號 #"})
        self.assertEqual(rows["PLAYER "], {"zh-Hans": "碟机 ", "zh-Hant": "碟機 "})

    def test_display_and_mixer_tabs_and_settings_options_complete(self):
        rows = bp.load_dict()
        # Dynamically include stock 5.3.0 QML literals from DisplayTabContent and MixerTabContent when available.
        extra_qml = set()
        qrc = LAB / "5.3.0" / "qrc"
        if qrc.is_dir():
            prefs_dir = qrc / "Skins/Embedded/Views/ControlCenter/Tabs/Preferences"
            for tab_qml in ("DisplayTabContent.qml", "MixerTabContent.qml"):
                tab_text = (prefs_dir / tab_qml).read_text(encoding="utf-8")
                for comp in re.findall(r"\b([A-Z][A-Za-z0-9_]*)\s*\{\}", tab_text):
                    entry_file = prefs_dir / "Entries" / f"{comp}.qml"
                    if entry_file.is_file():
                        src = entry_file.read_text(encoding="utf-8")
                        for m in re.finditer(r'\b(?:text|buttonText|title|message)\s*:\s*"([^"\\\n]+)"', src):
                            extra_qml.add(m.group(1))

        checked = set(self.DISPLAY_TAB_STRINGS) | set(self.MIXER_TAB_STRINGS) | set(self.OTHER_SETTINGS_REQUIRED) | extra_qml
        for s in sorted(checked):
            if s in self.KEEP_ENGLISH and s not in rows:
                self.assertTrue(bool(self.KEEP_ENGLISH[s].strip()), f"{s}: empty keep-English reason")
                continue
            self.assertIn(s, rows, f"missing translation in tools/ui-mod/i18n.tsv: {s!r}")
            for lang in ("zh-Hans", "zh-Hant"):
                val = rows[s][lang]
                self.assertTrue(bool(val.strip()), f"{s} ({lang}) is blank")
                if val == s:
                    self.assertIn(
                        s, self.KEEP_ENGLISH,
                        f"{s!r} ({lang}) stays English without an explicit KEEP_ENGLISH reason",
                    )
                    self.assertTrue(bool(self.KEEP_ENGLISH[s].strip()))

    def test_option_value_paths_translate_spinbox_slider_and_open_button(self):
        old_root = bp.ROOT
        try:
            bp.ROOT = "Skins/Embedded/"
            patch_table = bp.edits(False)
        finally:
            bp.ROOT = old_root
        prefs = "Skins/Embedded/Views/ControlCenter/Tabs/Preferences/"
        list_patches = dict(patch_table[prefs + "Entries/Base/ListEntry.qml"])
        self.assertIn("text: root.scT(modelData)\n", "".join(list_patches.values()))
        self.assertIn("text: root.scT(control.textFromValue(control.value, control.locale))\n", "".join(list_patches.values()))
        self.assertIn("down: pressed || isSelected(modelData)", "".join(list_patches.values()))

        slider_patches = dict(patch_table[prefs + "Entries/Base/PrefsSliderEntry.qml"])
        self.assertIn("text: root.scT(entry.sliderValueText)\n", "".join(slider_patches.values()))

        eq_patches = dict(patch_table[prefs + "Entries/OutputEQ.qml"])
        self.assertIn('buttonText: root.scT("Open")', "".join(eq_patches.values()))

        uikit_list = dict(patch_table["Skins/UIKit/Elements/Settings/Entries/ListEntry.qml"])
        self.assertIn("Math.scI18n.t(modelData)", "".join(uikit_list.values()))
        self.assertIn("Math.scI18n.t(control.textFromValue(control.value, control.locale))", "".join(uikit_list.values()))

        radio = dict(patch_table["Skins/UIKit/Elements/RadioSlider.qml"])
        self.assertIn("Math.scI18n.t(modelData)", "".join(radio.values()))
        self.assertIn("control.currentValue === modelData", "".join(radio.values()))


class ShadowSyncAlignmentGuardTest(unittest.TestCase):
    """Sync alignment review (11-11): neighbours of the native-pull follower that must hold."""
    js = ShadowDisplayTest.js
    SENDER = r"""
globalThis.XMLHttpRequest=class{open(){this.readyState=1}send(){this.readyState=4;this.status=200}};
globalThis.scShadowPoll=()=>{};scFourDeckRole='1';
let pos=10000,nominal=10000,rate=1,synced=false,k=0,stale=0,pull=0,pullSpeed=0.2;
// TrackPosition moves in audio blocks: a read is 0-23 ms of deck time old.
const lag=()=>[0,9,21,4,15,23,11][k%7];
globalThis.scTr=path=>({value:path.endsWith('/SampleRate')?48000:0,color:'#fff',index:0,
 state:path.endsWith('/SongLoaded')||path.endsWith('/PlayState'),
 get unnormalized(){return path.endsWith('/TrackPosition')?(pos-lag()-stale)*48:path.endsWith('/TrackLength')?48000*400:
  path.endsWith('/OriginalBPM')?128:path.endsWith('/CurrentBPM')?128*rate:0},
 get string(){return path.endsWith('/SyncMode')?(synced?'BeatOrBarSync':'Off'):
  path.endsWith('/TrackUri')?'file:///a.wav':path.endsWith('/SpeedState')?((rate-1)*100).toFixed(2)+'%':''}});
// Engine's pull: the deck runs pullSpeed faster or slower until the phase is in.
const tick=(dt=40)=>{now+=dt;k++;const run=dt*rate;nominal+=run;pos+=run;
 if(pull){const m=Math.sign(pull)*Math.min(Math.abs(pull),run*pullSpeed);pos+=m;pull-=m}
 scShadowTick()};
const published=()=>{const s=scShadowOwn[0];return s.positionMs+(now-s.at)*(1+s.pitch/100)};
const jumps=()=>scShadowPrev[1].jumps;
const steady=()=>{synced=false;rate=1;for(let i=0;i<40;i++)tick()};
const engage=(ms,speed=0.2)=>{synced=true;rate=1.03;pull=ms;pullSpeed=speed;nominal=pos;tick()};
"""
    ENGINE = r"""
Object.assign(globalThis,{scShadowDisplayOnly:false,scShadowEngineMask:30,scShadowBent:null});
const T0=now, props=new Map(), seeks=[], presses=[];
let shift=0, synced=false, rate=1, seq=0, tick=0, pending=null, jumpCount=0;
let losses=[90], noise=true;
const jitter=()=>noise?-rate*[0,12,28,4,18,30,8][tick%7]:0;
// A playing seek: the old line for a moment, then the target, held back by the loss.
function playback(){
 if(!pending)return 10000+(now-T0)*rate;
 if(now<pending.t+40)return pending.old+(now-pending.t)*rate;
 return pending.target+Math.max(0,now-pending.t-pending.loss)*rate;
}
globalThis.scShadowBendTimer={stop(){},restart(){}};
globalThis.scShadowHolds=()=>true;globalThis.scShadowEnd=()=>false;
globalThis.scShadowCues=()=>{};globalThis.scShadowBeat=()=>{};
const speeds=[];globalThis.scShadowSpeed=(d,pitch)=>speeds.push(pitch);globalThis.scPress=p=>presses.push(p);
globalThis.scTr=path=>{
 if(props.has(path))return props.get(path);
 const p={state:path.endsWith('/PlayState')||path.endsWith('/SongLoaded'),
 string:path.endsWith('/SpeedRange')?'100':path.endsWith('/SyncMode')?'Off':'',
 get unnormalized(){return path.endsWith('/TrackPosition')?(playback()+jitter())*48:
  path.endsWith('/TrackLength')?48000*400:0},
 get value(){return path.endsWith('/SampleRate')?48000:this.v||0},
 set value(v){this.v=v;if(path.endsWith('/SetSongPosition')){
  pending={t:now,target:v*400000,old:playback(),loss:losses[Math.min(seeks.length,losses.length-1)]};seeks.push(pending);
 }}};props.set(path,p);return p;
};
scShadowAt[2]={path:'/a',id:'a',ready:true,seekAt:0,jumps:0};
const source=()=>10000+(now-T0)*rate+shift;
const apply=(extra={})=>scShadowApply(Object.assign({deck:2,path:'/a',id:'a',link:'peer',playing:true,
 sync:synced,positionMs:source(),at:now,pitch:(rate-1)*100,bpm:128,currentBpm:128*rate,jumps:jumpCount,seq:++seq},extra));
const step=(dt=80,extra={})=>{now+=dt;tick++;scShadowLive.seen=now;scShadowLine(now);scShadowLandingTick(now);apply(extra)};
apply();for(let i=0;i<50;i++)step();
"""

    def test_the_shadow_js_avoids_names_qml_reserves(self):
        # Engine's QML engine still reserves the ES3 future words: "native" as a name
        # failed EmbeddedMain.qml to load (twin, 11-11), which Node accepts.
        reserved = "abstract|boolean|byte|char|double|final|float|goto|int|long|native|short|synchronized|throws|transient|volatile"
        hits = re.findall(r"(?:\b(?:const|let|var)\s+|,\s*)(" + reserved + r")\s*=", bp.SERVICES_MAIN)
        self.assertEqual(hits, [])

    def test_a_noop_sync_posts_its_tempo_without_a_jump(self):
        self.js(self.SENDER + r"""
steady();const before=jumps();engage(0);
assert.equal(scShadowOwn[0].sync,true,'Sync was not posted with its tempo step');
assert.equal(scShadowOwn[0].currentBpm,128*1.03);
for(let i=0;i<80;i++){tick();assert.ok(Math.abs(published()-pos)<25,'line moved without a pull')}
assert.equal(jumps(),before,'a Sync without a pull became a jump');
// A small pull (under 20 ms) moves the line without a seek on the peer.
steady();const small=jumps();engage(12);
for(let i=0;i<80;i++)tick();
assert.equal(jumps(),small,'a 12 ms pull became a jump');
assert.ok(Math.abs(published()-pos)<12,'the small pull was not taken into the line');
""")

    def test_a_hot_cue_during_the_pull_is_its_own_single_jump(self):
        self.js(self.SENDER + r"""
steady();const before=jumps();engage(150);
for(let i=0;i<6;i++)tick();
pos+=8000;pull=0;tick();
assert.equal(jumps(),before+1,'the hot cue was not a jump');
assert.ok(Math.abs(published()-pos)<25,'the hot cue line was not posted');
for(let i=0;i<60;i++)tick();
assert.equal(jumps(),before+1,'the hot cue was followed by a second jump');
""")

    def test_cues_and_loop_wraps_stay_single_seeks_without_a_trim(self):
        self.js(self.ENGINE + r"""
synced=true;for(let i=0;i<60;i++)step(); // long after the Sync edge
losses=[150];
for(const delta of [4000,-2000,-455]){
 const before=seeks.length;
 shift+=delta;jumpCount++;step();
 for(let i=0;i<20;i++)step();
 assert.equal(seeks.length,before+1,'a cue or loop wrap of '+delta+' ms was not exactly one seek');
}
""")


class TimeZonePersistenceTest(unittest.TestCase):
    def test_timezone_capture_normalizes_spaces_guards_boot_fallback_and_saves_picker_utc(self):
        self.assertIn("function onSystemTimeZoneSetSucceeded() { root.scOnTimeZoneChanged() }", bp.SERVICES_MAIN)
        self.assertIn("function onTimeZoneSelect() { root.scTimeZonePickerUsed = true }", bp.SERVICES_MAIN)
        self.assertIn("http://127.0.0.1:8080/v1/sc5000/ui?timezone=", bp.SERVICES_MAIN)
        import shutil, subprocess
        if not shutil.which("node"):
            return
        start = bp.SERVICES_MAIN.index("function scHookTimeZonePicker()")
        end = bp.SERVICES_MAIN.index("\tConnections {\n\t\ttarget: typeof Planck")
        js_funcs = bp.SERVICES_MAIN[start:end]
        script = f"""
const assert = require('node:assert/strict');
let fakeNow = 1000;
Date.now = () => fakeNow;
let scTimeZoneBootWindowOpen = true;
let scLastGoodTimeZone = '';
let scPendingTimeZone = '';
let scRestoringTimeZone = false;
let scTimeZonePickerUsed = false;
let scTimeZonePickerHooked = false;
let pickerVisible = false;
const posts = [];
globalThis.root = {{
    get scPendingTimeZone() {{ return scPendingTimeZone; }},
    set scPendingTimeZone(v) {{ scPendingTimeZone = v; }},
    get scTimeZonePickerUsed() {{ return scTimeZonePickerUsed; }},
    set scTimeZonePickerUsed(v) {{ scTimeZonePickerUsed = v; }},
}};
globalThis.XMLHttpRequest = class {{
    open(method, url) {{ this.method = method; this.url = url; }}
    send() {{
        posts.push(this.url);
        this.readyState = 4;
        this.status = 200;
        if (this.onreadystatechange) this.onreadystatechange();
    }}
}};
let currentDisplayTz = 'Universal';
let currentIsUtc = true;
const pickerListeners = [];
globalThis.Planck = {{
    getProperty: (path) => {{
        if (path !== '/GUI/ShowTimeZoneView') return null;
        return {{
            translator: {{
                get state() {{ return pickerVisible; }},
                set state(v) {{
                    pickerVisible = !!v;
                    for (const fn of pickerListeners) fn();
                }},
                stateChanged: {{ connect: (fn) => pickerListeners.push(fn) }},
            }},
        }};
    }},
    timeDateController: {{
        isSystemTimeZoneUTC: () => currentIsUtc,
        getSystemTimeZone: () => currentDisplayTz,
        setSystemTimeZone: (tz) => {{
            currentDisplayTz = tz.replace(/_/g, ' ');
            currentIsUtc = (tz === 'UTC' || tz === 'Etc/UTC' || tz === 'Universal');
            scOnTimeZoneChanged();
        }},
    }},
}};
{js_funcs}
// 1. Auto-detected zone with space display formatting ("America/New York") normalizes to "America/New_York".
Planck.timeDateController.setSystemTimeZone('America/New_York');
assert.equal(scLastGoodTimeZone, 'America/New_York');
assert.equal(posts.at(-1), 'http://127.0.0.1:8080/v1/sc5000/ui?timezone=America%2FNew_York');

// 2. Manual selection of Asia/Shanghai posts Asia/Shanghai.
Planck.timeDateController.setSystemTimeZone('Asia/Shanghai');
assert.equal(scLastGoodTimeZone, 'Asia/Shanghai');
assert.equal(posts.at(-1), 'http://127.0.0.1:8080/v1/sc5000/ui?timezone=Asia%2FShanghai');

// 3. Unsolicited boot-window UTC (picker NOT used) stays guarded even if NTP jumps wall-clock Date.now() +12 days!
fakeNow += 12 * 86400 * 1000;
const postsBeforeFallback = posts.length;
Planck.timeDateController.setSystemTimeZone('UTC');
assert.equal(scLastGoodTimeZone, 'Asia/Shanghai');
assert.equal(currentDisplayTz, 'Asia/Shanghai');
assert.equal(currentIsUtc, false);
assert.equal(posts.length, postsBeforeFallback);

// 4. User opens the Time Zone picker (/GUI/ShowTimeZoneView = true) and chooses UTC: saved like any zone!
Planck.getProperty('/GUI/ShowTimeZoneView').translator.state = true;
Planck.timeDateController.setSystemTimeZone('UTC');
Planck.getProperty('/GUI/ShowTimeZoneView').translator.state = false;
assert.equal(scTimeZonePickerUsed, true);
assert.equal(scLastGoodTimeZone, 'UTC');
assert.equal(currentDisplayTz, 'UTC');
assert.equal(currentIsUtc, true);
assert.equal(posts.at(-1), 'http://127.0.0.1:8080/v1/sc5000/ui?timezone=UTC');

// 5. Once the 30s monotonic Timer expires (scTimeZoneBootWindowOpen = false), UTC is saved even without picker.
scTimeZonePickerUsed = false;
Planck.timeDateController.setSystemTimeZone('Asia/Shanghai');
scTimeZoneBootWindowOpen = false;
Planck.timeDateController.setSystemTimeZone('UTC');
assert.equal(scLastGoodTimeZone, 'UTC');
assert.equal(posts.at(-1), 'http://127.0.0.1:8080/v1/sc5000/ui?timezone=UTC');
"""
        r = subprocess.run(["node", "-e", script], capture_output=True, text=True)
        self.assertEqual(r.returncode, 0, r.stderr)


class StemsHqHintAndInstallerTest(unittest.TestCase):
    def test_stems_hq_status_line_and_translations(self):
        rows = bp.load_dict()
        offline_en = "On-deck stems (basic quality). For high-quality stems, run the EngineX Stems service on a computer on the same network."
        online_prefix = "High-quality stems:"
        self.assertIn(offline_en, rows)
        self.assertIn(online_prefix, rows)
        self.assertIn("EngineX Stems", rows[offline_en]["zh-Hans"])
        self.assertIn("EngineX Stems", rows[offline_en]["zh-Hant"])
        self.assertEqual(rows[online_prefix]["zh-Hans"], "高音质分轨：")
        self.assertEqual(rows[online_prefix]["zh-Hant"], "高音質分軌：")
        self.assertIn('LibraryTabContent.qml"].append', SRC)
        self.assertIn("root.scLanStemsLine(root.scLanStemsInfo, root.scLang)", SRC)
        fn = re.search(r"function scLanStemsLine\([^)]*\)\s*\{[\s\S]*?\n\t\}", bp.SERVICES_MAIN).group(0)
        script = f"""
const assert = require('assert');
const rows = {json.dumps(rows, ensure_ascii=False)};
let scLang = 'en';
function scT(s) {{
    if (scLang === 'en') return s;
    return (rows[s] && rows[s][scLang]) || s;
}}
{fn}
assert.equal(scLanStemsLine(null, 'en'), {json.dumps(offline_en)});
assert.equal(scLanStemsLine({{online: false, hostname: 'studio-mac', backend: 'htdemucs'}}, 'en'), {json.dumps(offline_en)});
assert.equal(scLanStemsLine({{online: true, hostname: 'studio-mac', backend: 'htdemucs'}}, 'en'), 'High-quality stems: studio-mac (htdemucs)');
assert.equal(scLanStemsLine({{online: true, endpoint: 'http://192.168.1.9:17890'}}, 'en'), 'High-quality stems: http://192.168.1.9:17890 (htdemucs)');
scLang = 'zh-Hans';
assert.equal(scLanStemsLine(null, scLang), rows[{json.dumps(offline_en)}]['zh-Hans']);
assert.equal(scLanStemsLine({{online: true, hostname: 'studio-mac', backend: 'htdemucs'}}, scLang), '高音质分轨：studio-mac (htdemucs)');
scLang = 'zh-Hant';
assert.equal(scLanStemsLine(null, scLang), rows[{json.dumps(offline_en)}]['zh-Hant']);
assert.equal(scLanStemsLine({{online: true, hostname: 'studio-mac', backend: 'hpss'}}, scLang), '高音質分軌：studio-mac (hpss)');
"""
        r = subprocess.run(["node", "-e", script], capture_output=True, text=True)
        self.assertEqual(r.returncode, 0, r.stderr)

    def test_home_worker_scripts_default_to_demucs_and_keep_hpss(self):
        import os
        sh_txt = (REPO / "tools/stems/run-home-worker.sh").read_text(encoding="utf-8")
        ps_txt = (REPO / "tools/stems/run-home-worker.ps1").read_text(encoding="utf-8")
        self.assertIn("ENGINEX_STEMS_BACKEND:-demucs", sh_txt)
        self.assertIn("else { 'demucs' }", ps_txt)
        self.assertIn("ValidateSet('auto', 'hpss', 'demucs', 'htdemucs')", ps_txt)
        with tempfile.TemporaryDirectory(prefix="home-worker-test-") as tmp:
            tdir = Path(tmp)
            stems_dir = tdir / "stems"
            stems_dir.mkdir()
            for name in ("run-home-worker.sh", "worker.py"):
                dst = stems_dir / name
                dst.write_text((REPO / "tools/stems" / name).read_text(encoding="utf-8"), encoding="utf-8")
                dst.chmod(0o755)
            log_file = tdir / "calls.log"
            # Mock venv python3 + enginex-stemsd so no network/pip is touched.
            venv_bin = tdir / "home/venv/bin"
            venv_bin.mkdir(parents=True)
            py_mock = venv_bin / "python3"
            py_mock.write_text(
                f"#!/bin/sh\necho \"py:$*\" >> {str(log_file)!r}\n"
                "case \"$*\" in\n"
                "  *\"import demucs\"*) exit 1 ;;\n"
                "  *\"--print-backend\"*)\n"
                "    case \"$*\" in *\"--backend hpss\"*) echo hpss ;; *) echo htdemucs ;; esac\n"
                "    exit 0 ;;\n"
                "esac\nexit 0\n",
                encoding="utf-8",
            )
            py_mock.chmod(0o755)
            stemsd_mock = stems_dir / "enginex-stemsd"
            stemsd_mock.write_text(f"#!/bin/sh\necho \"stemsd:$*\" >> {str(log_file)!r}\nexit 0\n", encoding="utf-8")
            stemsd_mock.chmod(0o755)
            env = {**os.environ, "ENGINEX_STEMS_HOME": str(tdir / "home")}
            env.pop("ENGINEX_STEMS_BACKEND", None)
            env.pop("ENGINEX_STEMSD", None)
            env.pop("SC5000_STEMSD", None)
            r_default = subprocess.run(["/bin/sh", str(stems_dir / "run-home-worker.sh")], env=env, capture_output=True, text=True)
            self.assertEqual(r_default.returncode, 0, r_default.stderr)
            calls_default = log_file.read_text(encoding="utf-8")
            self.assertIn("pip install -q demucs", calls_default)
            self.assertIn('get_model("htdemucs")', calls_default)
            self.assertIn("-backend demucs", calls_default)

            log_file.write_text("", encoding="utf-8")
            r_hpss = subprocess.run(["/bin/sh", str(stems_dir / "run-home-worker.sh"), "hpss"], env=env, capture_output=True, text=True)
            self.assertEqual(r_hpss.returncode, 0, r_hpss.stderr)
            calls_hpss = log_file.read_text(encoding="utf-8")
            self.assertNotIn("pip install -q demucs", calls_hpss)
            self.assertIn("-backend hpss", calls_hpss)

    def test_build_installer_bundles_stems_service_and_trilingual_readme(self):
        import os, zipfile
        readme = (REPO / "tools/stems/README.md").read_text(encoding="utf-8")
        for marker in ("## English", "## 简体中文", "## 繁體中文", "run-home-worker.sh", "run-home-worker.ps1"):
            self.assertIn(marker, readme)
        with tempfile.TemporaryDirectory(prefix="installer-pkg-test-") as tmp:
            tdir = Path(tmp)
            fake_bin = tdir / "bin"
            out_dir = tdir / "out"
            fake_bin.mkdir()
            out_dir.mkdir()
            fake_go = fake_bin / "go"
            fake_go.write_text(
                '#!/bin/sh\nout=""\nwhile [ $# -gt 0 ]; do\n'
                '  if [ "$1" = "-o" ]; then out="$2"; shift 2; else shift; fi\n'
                'done\nprintf "#!/bin/sh\\nexit 0\\n" > "$out"\nchmod 755 "$out"\n',
                encoding="utf-8",
            )
            fake_go.chmod(0o755)
            env = {**os.environ, "PATH": f"{fake_bin}:{os.environ.get('PATH', '')}"}
            r = subprocess.run(
                ["/bin/sh", str(REPO / "tools/az01-packer/build-installer.sh"), "5.3.0.4", str(out_dir)],
                env=env, capture_output=True, text=True,
            )
            self.assertEqual(r.returncode, 0, r.stderr)
            import tarfile, zipfile
            with tarfile.open(out_dir / "EngineX-Installer-5.3.0.4-linux-amd64.tar.gz", "r:gz") as tf:
                names = {m.name.lstrip("./") for m in tf.getmembers()}
                for req in ("enginex-installer", "install-enginex.sh", "stems/enginex-stemsd",
                            "stems/worker.py", "stems/run-home-worker.sh", "stems/run-home-worker.ps1", "stems/README.md"):
                    self.assertIn(req, names)
            for zname, stems_bin in (
                ("EngineX-Installer-5.3.0.4-macos-arm64.zip", "stems/enginex-stemsd"),
                ("EngineX-Installer-5.3.0.4-macos-amd64.zip", "stems/enginex-stemsd"),
                ("EngineX-Installer-5.3.0.4-windows-amd64.zip", "stems/enginex-stemsd.exe"),
            ):
                with zipfile.ZipFile(out_dir / zname) as zf:
                    znames = set(zf.namelist())
                    for req in (stems_bin, "stems/worker.py", "stems/run-home-worker.sh", "stems/run-home-worker.ps1", "stems/README.md"):
                        self.assertIn(req, znames)
                    sh_mode = (zf.getinfo("stems/run-home-worker.sh").external_attr >> 16) & 0o777
                    self.assertEqual(sh_mode & 0o111, 0o111)

    def test_deck_clock_feed_for_dmx_without_panel(self):
        m = bp.SERVICES_MAIN
        self.assertIn("running: root.scDeckClock && root.scFourDeckRole === \"\"", m)
        self.assertIn("scDeckClock = !!v.deckClock", m)
        fn = m.split("function scDeckClockDeckProps(", 1)[1].split("\t// 25 Hz around every edge", 1)[0]
        self.assertNotIn("Planck.getProperty", fn)
        self.assertNotRegex(fn, r'\.string\b')
        self.assertIn('http://127.0.0.1:8080/v1/sc5000/clock', fn)
        if shutil.which("node"):
            tr_fn = m.split("function scTr(path) {", 1)[1].split("// #151:", 1)[0]
            script = r'''
const assert = require('node:assert/strict');
const root = globalThis;
var scPropCache = {}, scDeckClock = true, scFourDeckRole = "", scShadowEmptyObject = {},
    scDeckClockRequest = null, scDeckClockProps = {}, scDeckClockProbeAt = {},
    scDeckClockPrevPos = {}, scDeckClockPrevCue = {}, scDeckClockPrevLoaded = {},
    scDeckClockPrevLen = {}, scDeckClockOut = [],
    scDeckClockPostAt = 0, scDeckClockSig = "";
const scDeckClockTimer = { interval: 100 };
let lookups = 0, posts = [];
const props = {
    "/Engine/Deck1/PlayState": { state: false },
    "/Engine/Deck1/Track/SongLoaded": { state: true },
    "/Engine/Deck1/Track/TrackLength": { unnormalized: 8820000 },
    "/Engine/Deck1/Track/CueLEDState": { state: true },
    "/Engine/Deck1/DeckIsMaster": { state: true },
    "/Engine/Deck1/OriginalBPM": { unnormalized: 120 },
    "/Engine/Deck1/CurrentBPM": { unnormalized: 124.8 },
    "/Engine/Deck1/BeatNumber": { unnormalized: 2.25 },
    "/Engine/Deck1/Track/TrackPosition": { unnormalized: 44100 },
    "/Engine/Deck1/Track/SampleRate": { value: 44100 },
    "/Engine/Deck2/PlayState": { state: false },
};
const Planck = { getProperty(p) { lookups++; return props[p] ? { translator: props[p] } : null; } };
class XMLHttpRequest {
    constructor() { this.readyState = 0; }
    open(m, u) { this.readyState = 1; this.method = m; this.url = u; }
    send(body) { this.readyState = 4; posts.push(JSON.parse(body)); }
    abort() { this.readyState = 0; }
}
function scTr(path) {''' + tr_fn + "\nfunction scDeckClockDeckProps(" + fn + r'''
root.scTr = scTr;
scDeckClockTick();
assert.equal(posts.length, 1);
assert.equal(posts[0].length, 2);
assert.equal(posts[0][0].deck, 1);
assert.equal(posts[0][0].master, true);
assert.equal(posts[0][0].currentBpm, 124.8);
assert.equal(posts[0][0].beat, 2.25);
assert.equal(posts[0][0].playing, false);
// CUE held on same track: PlayState false, CueLEDState true, TrackPosition advances -> playing: true
props["/Engine/Deck1/Track/TrackPosition"].unnormalized = 48510;
scDeckClockTick();
assert.equal(posts.length, 2);
assert.equal(posts[1][0].cue, true);
assert.equal(posts[1][0].playing, true);
assert.equal(scDeckClockTimer.interval, 100);
// Loading a different track with a higher TrackPosition must NOT false-trigger CUE.
props["/Engine/Deck1/Track/TrackLength"].unnormalized = 9990000;
props["/Engine/Deck1/Track/TrackPosition"].unnormalized = 66150;
scDeckClockTick();
assert.equal(posts[posts.length - 1][0].cue, false);
assert.equal(posts[posts.length - 1][0].playing, false);
// Missing decks 3 & 4 are cached negatively for 3 s (no lookup storm).
const beforeLookups = lookups;
scDeckClockTick();
assert.equal(lookups, beforeLookups);
'''
            res = subprocess.run(["node"], input=script, text=True, capture_output=True)
            self.assertEqual(res.returncode, 0, res.stderr)


class OneLibraryNoChooseDbTest(unittest.TestCase):
    STOCK_TAIL = (
        "Item {\n"
        "    id: messageBox\n"
        '    objectName: "OneLibraryMessageBox"\n'
        '    property string pendingDevicePath: ""\n'
        "\n"
        "    Connections {\n"
        "        target: Planck.librarianController.librarySelectionController\n"
        "        function onSelectionRequested(devicePath) {\n"
        "            messageBox.pendingDevicePath = devicePath\n"
        "\n"
        "            const prompt = PlanckGUI.views.prompts.post({\n"
        '                id: "OneLibrarySelection",\n'
        '                title: "Choose your Database",\n'
        '                message: "Placeholder text",\n'
        "                options: [],\n"
        "            })\n"
        "            prompt.extras = messageBox.extrasComponent\n"
        "            prompt.finished.connect(messageBox.onFinished)\n"
        "        }\n"
        "    }\n"
        "\n"
        "    Connections {\n"
        "        target: Planck.librarianController.devicesController\n"
        "        function onDeviceRemoved(path, databaseUuid) {\n"
        "            if (path === messageBox.pendingDevicePath) {\n"
        '                PlanckGUI.views.prompts.drop("OneLibrarySelection")\n'
        "            }\n"
        "        }\n"
        "    }\n"
        "}\n"
    )

    def test_patch_registered_for_530_only(self):
        old_root = bp.ROOT
        try:
            bp.ROOT = "Skins/EmbeddedCommon/"
            self.assertNotIn(
                "Skins/EmbeddedCommon/Items/MessageBoxes/OneLibraryMessageBox.qml",
                bp.edits(False),
            )
            bp.ROOT = "Skins/Embedded/"
            for dev in (False, True):
                out = bp.edits(dev)
                self.assertIn(
                    "Skins/Embedded/Items/MessageBoxes/OneLibraryMessageBox.qml",
                    out,
                )
        finally:
            bp.ROOT = old_root

    def test_one_library_message_box_never_shows_prompt_and_selects_right_db(self):
        import re, tarfile

        rel = "Skins/Embedded/Items/MessageBoxes/OneLibraryMessageBox.qml"
        old_root = bp.ROOT
        try:
            bp.ROOT = "Skins/Embedded/"
            changes = bp.edits(False)[rel]
        finally:
            bp.ROOT = old_root

        patched = self.STOCK_TAIL
        for old, new in changes:
            patched = bp.patch(patched, old, new, rel)
        self.assertNotIn("prompts.post", patched)
        self.assertNotIn("Choose your Database", patched)
        self.assertNotIn("onDeviceAdded", patched)
        self.assertIn("/v1/sc5000/library-status?path=", patched)

        qrc = LAB / "5.3.0" / "qrc"
        if qrc.is_dir():
            with tempfile.TemporaryDirectory(prefix="ui-no-choose-db-") as tmp:
                out = Path(tmp) / "packs"
                res = subprocess.run(
                    ["python3", str(BUILD_PACKS), "--release", "--qrc", str(qrc), "--out", str(out)],
                    capture_output=True, text=True,
                )
                self.assertEqual(res.returncode, 0, res.stderr)
                for lang in ("en", "zh-Hans", "zh-Hant"):
                    p = out / lang / rel
                    self.assertTrue(p.is_file(), f"missing {rel} in {lang} pack")
                    qml = p.read_text(encoding="utf-8")
                    self.assertNotIn("prompts.post", qml, f"{lang} pack still posts Choose your Database prompt")
                    self.assertNotIn("Choose your Database", qml, f"{lang} pack still has Choose your Database title")
                    self.assertNotIn("onDeviceAdded", qml, f"{lang} pack still carries dead onDeviceAdded handler")
                    for expected in (
                        "function scHasRegisteredDatabaseUuid(devicePath)",
                        "function scFinishSelection(devicePath, seq, useEngineLibrary)",
                        "function scExpirePendingRequests(nowMs)",
                        "/v1/sc5000/library-status?path=",
                        "controller.chooseEngineLibrary(devicePath)",
                        "controller.chooseOneLibrary(devicePath)",
                    ):
                        self.assertIn(expected, qml)
                    patched = qml
                with tarfile.open(out.with_suffix(".tar.gz"), "r:gz") as tf:
                    members = {m.name.lstrip("./") for m in tf.getmembers()}
                    for lang in ("en", "zh-Hans", "zh-Hant"):
                        self.assertIn(f"packs/{lang}/{rel}", members)

        if shutil.which("node"):
            has_fn = re.search(
                r"function scHasRegisteredDatabaseUuid\(devicePath\)\s*\{.*?\n    \}",
                patched, re.S,
            )
            fin_fn = re.search(
                r"function scFinishSelection\(devicePath, seq, useEngineLibrary\)\s*\{.*?\n    \}",
                patched, re.S,
            )
            exp_fn = re.search(
                r"function scExpirePendingRequests\(nowMs\)\s*\{.*?\n    \}",
                patched, re.S,
            )
            sel_fn = re.search(
                r"function onSelectionRequested\(devicePath\)\s*\{.*?\n        \}",
                patched, re.S,
            )
            rem_fn = re.search(
                r"function onDeviceRemoved\(path, databaseUuid\)\s*\{.*?\n        \}",
                patched, re.S,
            )
            self.assertIsNotNone(has_fn)
            self.assertIsNotNone(fin_fn)
            self.assertIsNotNone(exp_fn)
            self.assertIsNotNone(sel_fn)
            self.assertIsNotNone(rem_fn)
            js_code = f"""
const assert = require("assert").strict;
let nowMs = 10000;
Date.now = () => nowMs;
const uuids = {{}};
const server = {{}};
const calls = [];
const dropped = [];
let heldXhr = null;
const scRequestWatchdog = {{
    running: false,
    start() {{ this.running = true; }},
    stop() {{ this.running = false; }},
}};
class XMLHttpRequest {{
    constructor() {{ this.readyState = 0; this.status = 0; this.responseText = ""; this.aborted = false; }}
    open(method, url) {{ this.method = method; this.url = url; this.readyState = 1; }}
    send() {{
        const u = new URL(this.url);
        assert.equal(u.pathname, "/v1/sc5000/library-status");
        const p = u.searchParams.get("path");
        const entry = server[p];
        if (entry === "hold") {{
            heldXhr = this;
            return;
        }}
        if (!entry) {{
            this.status = 500;
            this.responseText = "error";
        }} else {{
            this.status = entry.status;
            this.responseText = entry.body;
        }}
        this.readyState = 4;
        if (this.onreadystatechange) this.onreadystatechange();
    }}
    abort() {{
        this.aborted = true;
        this.status = 0;
        this.readyState = 4;
        if (this.onreadystatechange) this.onreadystatechange();
    }}
}}
const Planck = {{
    librarianController: {{
        devicesController: {{
            getDatabaseUuid(p) {{ return uuids[p] || ""; }},
        }},
        librarySelectionController: {{
            chooseEngineLibrary(p) {{ calls.push(["engine", p]); }},
            chooseOneLibrary(p) {{ calls.push(["one", p]); }},
            cancel(p) {{ calls.push(["cancel", p]); }},
        }},
    }},
}};
const PlanckGUI = {{
    views: {{
        prompts: {{
            post() {{ throw new Error("Choose your Database prompt must never be posted"); }},
            drop(id) {{ dropped.push(id); }},
        }},
    }},
}};
const messageBox = {{
    pendingDevicePath: "",
    scNextRequestSeq: 0,
    scPendingRequests: {{}},
    {has_fn.group(0).replace("function scHasRegisteredDatabaseUuid", "scHasRegisteredDatabaseUuid")},
    {fin_fn.group(0).replace("function scFinishSelection", "scFinishSelection")},
    {exp_fn.group(0).replace("function scExpirePendingRequests", "scExpirePendingRequests")},
}};
{sel_fn.group(0)}
{rem_fn.group(0)}

// 1. Stick converted by rb-bridge (UUID registered + gateway reports converted=true).
uuids["/media/RB_READY"] = "8d4f2c10-1234-4a5b-8c9d-0123456789ab";
server["/media/RB_READY"] = {{ status: 200, body: JSON.stringify({{ converted: true }}) }};
onSelectionRequested("/media/RB_READY");
assert.deepEqual(calls, [["engine", "/media/RB_READY"]]);
assert.equal(scRequestWatchdog.running, false);

// 2. Writable Rekordbox 7 stick whose pre-mount conversion missed budget:
//    stock Engine creates an empty ~156 KB skeleton m.db so getDatabaseUuid IS non-empty,
//    while gateway reports converted=false -> MUST pick OneLibrary, never empty Engine Library!
calls.length = 0;
uuids["/media/RB7_SKELETON"] = "e7a89052-6441-43bb-9c6d-e3bed7011d00";
server["/media/RB7_SKELETON"] = {{ status: 200, body: JSON.stringify({{ converted: false }}) }};
onSelectionRequested("/media/RB7_SKELETON");
assert.deepEqual(calls, [["one", "/media/RB7_SKELETON"]]);

// 3. Read-only HFS+ stick without writable view (UUID empty) -> immediate OneLibrary without HTTP.
calls.length = 0;
uuids["/media/HFS_RO"] = "";
onSelectionRequested("/media/HFS_RO");
assert.deepEqual(calls, [["one", "/media/HFS_RO"]]);

// 4. Gateway error / malformed JSON / timeout -> fall back to OneLibrary.
calls.length = 0;
uuids["/media/RB_UNKNOWN"] = "11111111-2222-4333-8444-555555555555";
server["/media/RB_UNKNOWN"] = {{ status: 502, body: "bad gateway" }};
onSelectionRequested("/media/RB_UNKNOWN");
assert.deepEqual(calls, [["one", "/media/RB_UNKNOWN"]]);

calls.length = 0;
uuids["/media/RB_HUNG"] = "22222222-3333-4444-8555-666666666666";
server["/media/RB_HUNG"] = "hold";
onSelectionRequested("/media/RB_HUNG");
assert.deepEqual(calls, []);
assert.equal(scRequestWatchdog.running, true);
nowMs += 1600;
messageBox.scExpirePendingRequests(nowMs);
assert.deepEqual(calls, [["one", "/media/RB_HUNG"]]);
assert.equal(heldXhr.aborted, true);
assert.equal(scRequestWatchdog.running, false);

// 5. Device removed while request is in flight cancels selection cleanly;
//    later re-mount with converted tracks picks Engine Library on next selectionRequested.
calls.length = 0;
heldXhr = null;
uuids["/media/RB7_SKELETON"] = "e7a89052-6441-43bb-9c6d-e3bed7011d00";
server["/media/RB7_SKELETON"] = "hold";
onSelectionRequested("/media/RB7_SKELETON");
onDeviceRemoved("/media/RB7_SKELETON", "e7a89052-6441-43bb-9c6d-e3bed7011d00");
assert.equal(heldXhr.aborted, true);
assert.deepEqual(calls, []);
server["/media/RB7_SKELETON"] = {{ status: 200, body: JSON.stringify({{ converted: true }}) }};
onSelectionRequested("/media/RB7_SKELETON");
assert.deepEqual(calls, [["engine", "/media/RB7_SKELETON"]]);
"""
            proc = subprocess.run(["node", "-e", js_code], capture_output=True, text=True)
            self.assertEqual(proc.returncode, 0, proc.stderr)


class EngineLightingDefaultOffAndPersistTest(unittest.TestCase):
    def test_guard_restores_saved_choice_without_changing_other_state(self):
        import os
        from copy import deepcopy
        enabled = {"state": True, "type": 1}
        gui = {"childorder": ["Decks", "PadsViewEnabled", "SongListLayoutStyle", "SoundSwitchViewEnabled"],
               "children": {"SoundSwitchViewEnabled": enabled,
                            "PadsViewEnabled": enabled,
                            "SongListLayoutStyle": {"string": "Large", "type": 4},
                            "Decks": {"childorder": ["Deck"], "children": {"Deck": {"childorder": [], "type": "Node"}}, "type": "Node"}},
               "type": "Node"}
        missing = {"childorder": ["Localization"], "children": {"EcoMode": {"state": False, "type": 1}}}
        empty = {"childorder": [], "children": {}}
        for initial, on, safe in ((None, False, False), (gui, False, False),
                                  (gui, True, False), (None, True, False),
                                  (missing, False, False), (missing, True, False),
                                  (empty, False, False), (empty, True, False),
                                  (gui, False, True), (None, False, True)):
            for compact, busybox in ((False, False), (True, False), (False, True), (True, True)):
                with self.subTest(initial=initial, on=on, safe=safe, compact=compact, busybox=busybox), tempfile.TemporaryDirectory() as tmp:
                    root = Path(tmp)
                    ui, state = root / "ui", root / "state"
                    ui.mkdir()
                    state.mkdir()
                    path = state / "PlanckGuiState.json"
                    before = None
                    if initial is not None:
                        value = deepcopy(initial)
                        if on and "SoundSwitchViewEnabled" in value["children"]:
                            value["children"]["SoundSwitchViewEnabled"] = {"state": False, "type": 1}
                        before = json.dumps({"data": {"GUI": value, "Other": {"state": True, "type": 1}}}, indent=None if compact else 4)
                        path.write_text(before)
                    if on:
                        (ui / "settings.conf").write_text("engine_lighting=1\n")
                    if safe:
                        (ui / "safe-mode").touch()
                    path_env = os.environ["PATH"]
                    if busybox:
                        binary = Path(os.environ.get("SC5000_TEST_BUSYBOX", shutil.which("busybox") or str(LAB / "sclive4-rea61-followup/rootfs/usr/bin/busybox")))
                        if not binary.is_file():
                            self.skipTest("stock BusyBox fixture unavailable")
                        bin_dir = root / "bin"
                        bin_dir.mkdir()
                        (bin_dir / "awk").symlink_to(binary)
                        path_env = str(bin_dir) + ":" + path_env
                    env = dict(os.environ, PATH=path_env, SC5000_UI_DIR=str(ui), SC5000_PRIMARY_UI_DIR=str(ui),
                               SC5000_LEGACY_UI_DIR=str(ui), SC5000_STATE_DIR=str(state),
                               SC5000_STREAMING_ACCOUNTS=str(root / "accounts"),
                               SC5000_ENGINE_DATA_DIR=str(root / "engine"),
                               SC5000_ASSIGNMENT_DIR=str(root / "assign"),
                               SC5000_ASSIGNMENT_PATCHED=str(root / "patched"), SC5000_ENGINE_GUARDIAN="/bin/false")
                    result = subprocess.run(["sh", str(REPO / "rootfs-overlay/etc/sc5000/engine-ui-guard"), "pre"], env=env, capture_output=True, text=True)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    if safe:
                        self.assertEqual(path.read_text() if path.exists() else None, before)
                        continue
                    self.assertTrue(path.is_file(), "guard did not create PlanckGuiState.json")
                    result = json.loads(path.read_text())
                    actual = result["data"]["GUI"]
                    self.assertEqual(actual["children"].get("SoundSwitchViewEnabled"), {"state": on, "type": 1})
                    self.assertIn("SoundSwitchViewEnabled", actual["childorder"])
                    self.assertEqual(actual["childorder"].count("SoundSwitchViewEnabled"), 1)
                    if initial is not None:
                        original = json.loads(before)
                        expected = original["data"]["GUI"]
                        expected["children"]["SoundSwitchViewEnabled"] = {"state": on, "type": 1}
                        if "SoundSwitchViewEnabled" not in expected["childorder"]:
                            expected["childorder"].insert(0, "SoundSwitchViewEnabled")
                        self.assertEqual(result, original)

    def test_stock_switch_and_non_toggleable_startup_in_all_packs(self):
        for version, skin in (("5.0.4", "EmbeddedCommon"), ("5.3.0", "Embedded")):
            qrc = LAB / version / "qrc"
            self.assertTrue(qrc.is_dir(), str(qrc))
            with tempfile.TemporaryDirectory() as tmp:
                out = Path(tmp) / "packs"
                result = subprocess.run(["python3", "-I", str(BUILD_PACKS), "--qrc", str(qrc), "--out", str(out)], capture_output=True, text=True)
                self.assertEqual(result.returncode, 0, result.stderr)
                for lang in ("en", "zh-Hans", "zh-Hant"):
                    with self.subTest(version=version, lang=lang):
                        switch = (out / lang / f"Skins/{skin}/Views/ControlCenter/Tabs/Preferences/Entries/SoundSwitch.qml").read_text()
                        self.assertIn('Planck.getProperty("/GUI/SoundSwitchViewEnabled")', switch)
                        self.assertIn('checked: pSoundSwitchEnabled.translator.state', switch)
                        self.assertIn('const next = !pSoundSwitchEnabled.translator.state', switch)
                        self.assertIn('pSoundSwitchEnabled.translator.state = next', switch)
                        self.assertIn('const SoundSwitch = root.scLightingBackend', switch)
                        self.assertRegex(switch, r'if\s*\(!next\s*&&[\s\S]+?SoundSwitch.stopBackend\(\)')
                        self.assertIn('typeof SoundSwitch.stopBackend === "function"', switch)
                        self.assertIn('http://127.0.0.1:8080/v1/sc5000/ui?engineLighting=', switch)
                        self.assertIn('XMLHttpRequest', switch)
                        self.assertIn('interval: 1500', switch)
                        self.assertIn('req.status !== 200', switch)
                        main = (out / lang / f"Skins/{skin}/EmbeddedMain.qml").read_text()
                        self.assertIn('Qt.createQmlObject', main)
                        self.assertIn('qrc:/Skins/Common/Items/', main)
                        self.assertIn('EngineLightingBackend.qml', main)
                        self.assertIn('root.scLightingBackend = helper ? helper.backend : null', main)
                        self.assertIn('Config.integrations.soundSwitch.isLightingIntegrationToggleable === false', main)
                        self.assertIn('const ssEn = Planck.getProperty("/GUI/SoundSwitchViewEnabled")', main)
                        self.assertIn('if (ssEn && ssEn.translator) ssEn.translator.state = true', main)
        self.assertNotIn('engineLighting=', (REPO / "pkg/webui/index.html").read_text())
        self.assertNotIn('engineLighting=', bp.SERVICES_ENTRIES)


@unittest.skipUnless(shutil.which("node"), "node needed for the overview scale behavioral checks")
class OverviewFullButterflyScaleTest(unittest.TestCase):
    """#233: mirrored Full peaks fit even the smallest stacked overview lane."""

    def check_overview(self, main, overview):
        scale = re.search(r"function scOverviewScale\([^)]*\)\s*\{.*?\n\s*\}", main, re.S)
        self.assertIsNotNone(scale)
        transform = re.search(r"transform: Scale \{ origin.y: ([^;]+); yScale: ([^\n]+?) \}", overview)
        self.assertIsNotNone(transform)
        js = "const assert = require('assert/strict');\n" + scale.group(0) + "\n"
        js += "const geometry = (overview, WaveformOverviewView) => { const root = {scOverviewScale}; return {origin: (" + transform.group(1) + "), scale: (" + transform.group(2) + ")}; };\n"
        js += r"""
// Stock lane heights: full player, StackedMiniDeckLayout / Player_Mini,
// 5.0.4 passive WaveformOverviews, and four-deck Console_Mini.
// Check symbolic test values and the native JP13 enum (Full preference 1 maps to Butterfly 0).
for (const WaveformOverviewView of [{Praline: 0, Butterfly: 1}, {Praline: 1, Butterfly: 0}]) {
    for (const height of [86, 46, 44, 30]) {
        for (const rgbMode of [false, true]) {
            for (const path of ['../Contents/Artist/Album/track.mp3',
                                '../Engine Library/Music/track.flac', '',
                                '../Stream/download.mp3', '../NotContents/track.mp3']) {
                for (const displayType of [0, 1]) {
                    const got = geometry({height, rgbMode, displayType, trackData: {path}}, WaveformOverviewView);
                    const full = displayType === WaveformOverviewView.Butterfly;
                    const expected = !full && path === '../Contents/Artist/Album/track.mp3'
                        ? (rgbMode ? 1.3 : 2.2) : 1;
                    const context = JSON.stringify({height, rgbMode, path, displayType, got});
                    const mappedTop = got.origin + (0 - got.origin) * got.scale;
                    const mappedBottom = got.origin + (height - got.origin) * got.scale;
                    assert.equal(got.scale, expected, 'yScale: ' + context +
                        ' mapped=[' + mappedTop + ',' + mappedBottom + ']');
                    assert.equal(got.origin, full ? height / 2 : height, 'origin.y: ' + context);
                    if (full) {
                        assert.ok(mappedTop >= 0, 'Full top peak clipped: ' + context);
                        assert.ok(mappedBottom <= height, 'Full bottom peak clipped: ' + context);
                    }
                }
            }
            for (const trackData of [null, {}, {path: null}]) {
                for (const displayType of [0, 1]) {
                    assert.equal(geometry({height, rgbMode, displayType, trackData}, WaveformOverviewView).scale, 1);
                }
            }
        }
    }
}
"""
        result = subprocess.run([shutil.which("node"), "-e", js], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_patch_expressions_keep_full_peaks_inside_every_lane(self):
        old_root = bp.ROOT
        try:
            for version, root in (("5.0.4", "Skins/EmbeddedCommon/"), ("5.3.0", "Skins/Embedded/")):
                with self.subTest(version=version):
                    bp.ROOT = root
                    patches = bp.edits(False)
                    # Minimal stock anchors also exercise the patch without lab fixtures in CI.
                    main = "\n".join(bp.patch(old, old, new, root + "EmbeddedMain.qml")
                                     for old, new in patches[root + "EmbeddedMain.qml"])
                    overview = "\n".join(bp.patch(old, old, new, root + "Views/Common/WaveformOverview/WaveformOverview.qml")
                                         for old, new in patches[root + "Views/Common/WaveformOverview/WaveformOverview.qml"])
                    self.check_overview(main, overview)
        finally:
            bp.ROOT = old_root

    def test_built_packs_keep_full_peaks_inside_every_lane(self):
        built = 0
        for version, root in (("5.0.4", "Skins/EmbeddedCommon/"), ("5.3.0", "Skins/Embedded/")):
            qrc = LAB / version / "qrc"
            if qrc.is_dir():
                built += 1
                with self.subTest(version=version), tempfile.TemporaryDirectory(prefix="ui-full-overview-") as tmp:
                    packs = Path(tmp) / "packs"
                    result = subprocess.run(["python3", str(BUILD_PACKS), "--qrc", str(qrc), "--out", str(packs)],
                                            capture_output=True, text=True)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    for language in ("en", "zh-Hans", "zh-Hant"):
                        with self.subTest(language=language):
                            base = packs / language / root
                            self.check_overview((base / "EmbeddedMain.qml").read_text(),
                                                (base / "Views/Common/WaveformOverview/WaveformOverview.qml").read_text())
        if not built:
            self.skipTest(f"{LAB} QRC fixtures not found")

    def test_shadow_full_overview_keeps_unit_stretch(self):
        strip = bp.SHADOW_LAYER.split("id: scOverviewStrip", 1)[1]
        stretch = re.search(r"readonly property real yStretch: ([^\n]+)", strip).group(1)
        stretch = stretch.replace("@RGB_SCALE@", "1.3").replace("@BANDS_SCALE@", "2.2")
        js = "const assert = require('assert/strict');\n"
        js += "const stretch = scDisplay => (" + stretch + ");\n"
        js += "for (const rgb of [false, true]) { assert.equal(stretch({half: false, rgb}), 1); assert.equal(stretch({half: true, rgb}), rgb ? 1.3 : 2.2); }"
        result = subprocess.run([shutil.which("node"), "-e", js], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)


if __name__ == "__main__":
    unittest.main()
