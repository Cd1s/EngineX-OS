// Run production functions from generated QML, including stock gesture timing.
const assert = require('node:assert/strict');
const vm = require('node:vm');
const source = JSON.parse(require('node:fs').readFileSync(0, 'utf8'));
function body(text, start) {
    const at = text.indexOf('{', start);
    let depth = 1, end = at + 1;
    while (depth && end < text.length) {
        if (text[end] === '{') depth++;
        if (text[end++] === '}') depth--;
    }
    assert.equal(depth, 0);
    return text.slice(at + 1, end - 1);
}
function action(text, name) {
    const at = text.indexOf(name + ': function');
    assert.ok(at >= 0, name);
    return body(text, at);
}
const functions = [...source.main.matchAll(/^\tfunction (scView\w+)\([^\n]*\) \{[\s\S]*?^\t\}/gm)]
    .map(m => m[0]).join('\n');
const changed = source.main.match(/^\t\tfunction onCurrentViewChanged\(\) \{[\s\S]*?^\t\t\}/m)[0];
// The View button triggers from the controller's QML engine, through its own
// property object: the GUI engine sees only the property's state (stateTrueCount,
// stateChanged), which stock's ActionTranslatorStateChangeNotifier turns into
// changed(). A "triggered" signal fires only on the object trigger() was called on,
// so the fixture gives the controller its own object (`controller` below).
const cycle = source.main.match(/ActionTranslatorStateChangeNotifier \{\n\t\tprop: root\.scViewCycleProperty \|\| null\n\t\tonChanged: (root\.scViewSelect\([^\n]+)\n/);
// A receiver on the GUI object's own "triggered" (before the fix).
const ownTriggered = source.main.match(/function onTriggered\(\) \{ (root\.scViewSelect\([^\n]+) \}/);
const timerStart = source.main.indexOf('\tTimer {', source.main.indexOf('function scViewSelect('));
const timer = body(source.main, source.main.indexOf('onTriggered:', timerStart));
const keyboard = source.main.match(/shortcut: "Shift\+V"\s+onTriggered: ([^\n]+)/)[1];
const prefs = source.main.match(/readonly property var scViewPrefs:\s*(\[\[[\s\S]*?\]\])/)[1];
const names = ['deckStyle', 'deckLayout', 'deckCount', 'waveformCount', 'waveformOrientation'];
const boards = ['JP07', 'JP08', 'JP13', 'JP14', 'JC11', 'JC16', 'JP11', 'NH08',
    'JP21', 'JP20', 'JC11S', 'JP11S', 'NH08S', 'NH10', 'JP24', 'RMZ2'];

function fixture(board, mode, orientation, saved) {
    const player = ['JP07', 'JP08', 'JP13', 'JP14'].includes(board);
    let persisted = JSON.parse(JSON.stringify(saved)), now = 10000;
    const events = [], resets = [], pending = [];
    const c = { libraryView: { visibleSettingValue: false,
        performanceLibraryEnableClicked() { this.visibleSettingValue = !this.visibleSettingValue; } },
        deckController: { albumArtVisible: false,
            onDisplayAlbumArtClicked() { this.albumArtVisible = !this.albumArtVisible; } },
        waveformController: {}, layoutController: {} };
    const settings = Object.fromEntries(names.map(key => [key, {
        settingNames: key === 'waveformOrientation' ? ['Horizontal', 'Vertical'] :
            key === 'deckCount' ? ['2 Decks', '4 Decks'] :
            key === 'waveformCount' ? ['2 Waveforms', '4 Waveforms'] : ['Normal'],
        currentName: key === 'waveformOrientation' ? orientation :
            key === 'deckCount' ? (mode === '4-deck' ? '4 Decks' : '2 Decks') :
            key === 'waveformCount' ? '2 Waveforms' : 'Normal',
        setCurrentIndex(i) { this.currentName = this.settingNames[i]; },
    }]));
    Object.assign(c.deckController, { deckStyleSetting: settings.deckStyle, deckCountSetting: settings.deckCount });
    Object.assign(c.waveformController, { waveformCountSetting: settings.waveformCount,
        waveformOrientationSetting: settings.waveformOrientation });
    c.layoutController.layoutSetting = settings.deckLayout;
    const ctx = { console, Date: { now: () => now },
        scIsPlayerBoard: player, scFourDeckRole: player && mode !== 'stock' ? '13' : '',
        scPairedOn: mode === 'paired', scPairedDirty: false,
        scUiLoaded: true, scViewsRead: false, scViewReady: false, scViews: {}, scViewActive: 1,
        scViewSyncing: false, scViewQuiet: 0, scViewLast: '', scViewDirty: false,
        scViewPrefs: [], scWave: 'denon', scUIScale: 100,
        scTracklistWaveform: false, scDualColumn: false, scCueCountdown: false,
        scBeatJumpPads: false, scSwapLoopOrder: false,
        globalConfig: { shift: true },
        module: { basePath: '/Client/Librarian/NavigationButtons/' },
        navigationButton: { primaryPath: 'MenuButtonPress', secondaryPath: 'CycleViewPress' },
        PlanckGUI: { views: { performanceViewController: c, isPortrait: board === 'JP13' || board === 'JP14' } },
        scCall(query) {
            for (const [key, value] of new URLSearchParams(query)) {
                const pref = ctx.scViewPrefs.find(k => k[1] === key);
                assert.ok(pref, query);
                ctx[pref[0]] = pref[0] === 'scUIScale' ? value :
                    typeof ctx[pref[0]] === 'boolean' ? value === '1' : value;
            }
        },
        XMLHttpRequest: function () {
            this.open = (method, path) => { this.method = method; assert.ok(path.endsWith('/views')); };
            this.send = data => {
                if (this.method === 'POST') persisted = JSON.parse(data);
                else {
                    this.readyState = 4; this.status = 200; this.responseText = JSON.stringify(persisted);
                    this.onreadystatechange();
                }
            };
        },
    };
    const vsc = { _currentView: 0,
        get currentView() { return this._currentView; },
        set currentView(value) {
            if (value === this._currentView) return;
            this._currentView = value;
            if (!player && ctx.scViewReady) ctx.onCurrentViewChanged();
        },
        resetToDefault(index) { resets.push(index); },
    };
    c.viewStateController = vsc;
    ctx.scViewCtrl = vsc;
    ctx.root = ctx;
    // One state per property, shared by every engine's object for it.
    const shared = {};
    const object = (path, gui) => ({ translatorType: 3, translator: { trigger() {
        events.push(path);
        const st = shared[path] ||= { stateTrueCount: 0, seen: 0 };
        st.stateTrueCount++;
        if (path === '/GUI/Scripted/EngineXCycleView') {
            // "triggered" on the object trigger() was called on: only the GUI's own.
            if (gui && ownTriggered) vm.runInContext(ownTriggered[1], ctx);
            // The shared state reaches the GUI's notifier, whichever engine triggered.
            if (cycle && st.seen !== st.stateTrueCount) {
                st.seen = st.stateTrueCount;
                vm.runInContext(cycle[1], ctx);
            }
        } else if (path.endsWith('/Shift/SinglePress') || path.endsWith('/CycleViewPress')) {
            // Stock policy has only three presets; it does not know views.json.
            vsc.currentView = (vsc.currentView + 1) % 3;
        }
    } } });
    // The controller's assignment QML gets its own objects (`controller`).
    const controller = {};
    ctx.Planck = { getProperty(path) { return controller[path] ||= object(path, false); } };
    ctx.scViewCycleProperty = object('/GUI/Scripted/EngineXCycleView', true);
    vm.createContext(ctx);
    vm.runInContext(functions + '\n' + changed + '\nscViewPrefs = ' + prefs, ctx);
    function run(code) { vm.runInContext(code, ctx); }
    function start() {
        run('(function() {' + timer + '})()');
        run('(function() {' + timer + '})()');
        assert.equal(ctx.scViewReady, true);
    }
    function clock(ms) {
        const until = now + ms;
        for (;;) {
            pending.sort((a, b) => a.at - b.at);
            const item = pending[0];
            if (!item || item.at > until) break;
            pending.shift(); now = item.at;
            if (item.timer.running && item.timer.at === item.at) {
                item.timer.running = false; item.action();
            }
        }
        now = until;
    }
    function makeTimer(interval, action) {
        return { running: false, at: 0, start() {
            this.running = true; this.at = now + interval;
            pending.push({ at: this.at, timer: this, action });
        }, stop() { this.running = false; } };
    }
    ctx.singleEnabled = true;
    Object.defineProperty(ctx, 'doubleEnabled', { get() {
        return vm.runInContext(source.view.match(/doubleEnabled:\s*([^\n]+)/)[1], ctx);
    } });
    ctx.holdEnabled = true;
    ctx.singleAction = () => run(action(source.view, 'singleAction'));
    ctx.doubleAction = () => run(action(source.view, 'doubleAction'));
    ctx.holdAction = () => run(action(source.view, 'holdAction'));
    ctx.holdReleased = () => {};
    ctx.singleTimer = makeTimer(300, () => { if (!ctx.holdTimer.running) ctx.singleAction(); });
    ctx.holdTimer = makeTimer(500, ctx.holdAction);
    run('function setValue(channel, value, assignmentEnabled) {' +
        body(source.combo, source.combo.indexOf('function setValue(')) + '}');
    start();
    return { ctx, run, events, resets, persisted: () => persisted,
        restart() {
            ctx.scViewActive = 1; ctx.scViews = {}; ctx.scViewsRead = false; ctx.scViewReady = false;
            start();
        },
        press(gap = 400, input = 'hardware') {
            if (input === 'keyboard') run(keyboard);
            else if (board === 'RMZ2') run(action(source.navigation, 'singleAction'));
            else { ctx.setValue(0, 1, true); clock(30); ctx.setValue(0, 0, true); }
            clock(gap);
        }, clock,
    };
}

let cases = 0;
for (const board of boards) {
    const modes = board.startsWith('JP0') || ['JP13', 'JP14'].includes(board)
        ? ['stock', 'paired', '4-deck'] : ['2-deck', '4-deck'];
    for (const mode of modes) for (const orientation of ['Horizontal', 'Vertical']) {
        for (const input of ['hardware', 'keyboard'])
        for (const gap of [400, 50]) for (const initial of [1, 2, 3, 4, 5]) {
            const saved = { active: initial, views: Object.fromEntries([1, 2, 3, 4, 5].map(n =>
                [String(n), { scUIScale: String(80 + n * 5), pads: !!(n & 1),
                    waveformOrientation: n & 1 ? 'Horizontal' : 'Vertical' }])) };
            const f = fixture(board, mode, orientation, saved);
            assert.equal(f.ctx.scViewActive, initial);
            for (let press = 1; press <= 6; press++) {
                f.press(gap, input);
                const want = (initial + press - 1) % 5 + 1;
                const label = `${board}/${mode}/${orientation}/${input}/${gap}ms/press${press}`;
                assert.equal(f.ctx.scViewActive, want, label);
                assert.equal(String(f.ctx.scUIScale), String(80 + want * 5), label + ' layout');
                assert.equal(f.persisted().active, want, label + ' persisted');
            }
            const active = f.ctx.scViewActive;
            f.restart();
            assert.equal(f.ctx.scViewActive, active, 'restart');
            f.ctx.scWave = 'rgb'; f.run('scViewSave()');
            assert.equal(f.persisted().views[String(active)].scWave, 'rgb', 'Save View');
            if (!f.ctx.scIsPlayerBoard && active <= 3) {
                f.run('scViewReset()');
                assert.equal(f.persisted().views[String(active)], undefined, 'Reset View');
                assert.equal(f.resets.at(-1), active - 1, 'stock default');
            }
            f.ctx.globalConfig.shift = false;
            f.press();
            assert.ok(f.events.at(-1).endsWith(board === 'RMZ2' ? '/MenuButtonPress' : '/SinglePress'), 'unshifted View');
            assert.equal(f.ctx.scViewActive, active, 'unshifted View must not cycle');
            f.ctx.globalConfig.shift = true;
            if (board === 'RMZ2') {
                f.ctx.navigationButton.secondaryPath = 'SearchButtonPress';
                f.press();
                assert.ok(f.events.at(-1).endsWith('/NavigationButtons/SearchButtonPress'));
            } else {
                f.ctx.setValue(0, 1, true); f.clock(501); f.ctx.setValue(0, 0, true);
                assert.ok(f.events.at(-1).endsWith('/Shift/HoldPress'), 'shift-hold stays stock');
                f.ctx.globalConfig.shift = false;
                f.press(50); f.press(400);
                assert.ok(f.events.at(-1).endsWith('/DoublePress'), 'unshifted double stays stock');
            }
            assert.equal(f.ctx.scViewActive, active, 'other gestures must not cycle');
            cases++;
        }
    }
}
console.log(`PASS ${cases} board/mode/orientation/timing/restart cases`);
