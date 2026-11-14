"""Behavioral checks for #162's gated PX UI, using the shipped QML functions."""
import importlib.util
from pathlib import Path
import re
import shutil
import subprocess
import unittest

SOURCE = Path(__file__).parents[1] / "ui-mod" / "build_packs.py"
spec = importlib.util.spec_from_file_location("fx_ms_packs", SOURCE)
bp = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bp)


def function(source, name):
    start = source.index("function " + name + "(")
    # These helpers have no nested named functions; match their QML indentation.
    match = re.search(r"\n([ \t]*)function " + name + r"\(", source)
    end = source.index("\n" + match[1] + "}", start) + len(match[1]) + 2
    return source[start:end]


@unittest.skipUnless(shutil.which("node"), "node required")
class FxMsUI(unittest.TestCase):
    def test_strict_config_reset_controls_and_stock_scope(self):
        functions = "\n".join(function(bp.FX_CONTROLLER, n) for n in
                              ("scFxMsController", "scFxMsStep", "scFxMsKnob", "scFxMsSet", "scFxMsFlush", "scFxMsExpire"))
        functions += "\n" + function(bp.SERVICES_MAIN, "scFxMsInteger")
        functions += "\n" + function(bp.SERVICES_MAIN, "scFxMsApply")
        functions += "\n" + function(bp.SERVICES_MAIN, "scParseSettingsConf")
        script = r'''
const assert = require('assert');
globalThis.root = globalThis;
let modeValue = 'beats', msValue = 500; const modeChanges=[];
Object.defineProperty(root, 'scFxTimeMode', {get:()=>modeValue,set:v=>{if(v!==modeValue)modeChanges.push(v);modeValue=v}});
Object.defineProperty(root, 'scFxTimeMs', {get:()=>msValue,set:v=>msValue=v});
let scCueCountdown, scTracklistWaveform;
const parameter = {value:1}, stock = {value:1};
const c = {scFxBusy:false,scFxStock:['Flanger','Echo'],scFxSelected:2,
           djFxView:{variantParameters:[parameter]}};
const scFxControllers = [c], calls=[], replies=[];
Object.assign(root, {scFxMsSending:false, scFxMsWanted:-1, scFxMsSent:-1, scFxMsRequest:0, scFxMsTimeout:{restart(){},stop(){}}});
let deferReplies=false;
function scCall(q, done) {
    calls.push(q);
    const reply=()=>{ scFxTimeMs=Number(q.split('=')[1]); if(done)done({}); };
    if(deferReplies)replies.push(reply); else reply();
}
'''
        script += functions + r'''
for (const v of ['ms','125ms','1.5','-1','+10','4294967296','NaN','Infinity',''])
    assert.equal(scFxMsInteger(v),500);
assert.equal(scFxMsInteger('0'),10);
assert.equal(scFxMsInteger('4294967295'),4000);
assert.equal(scFxMsStep(parameter,1),false); assert.equal(calls.length,0);
scParseSettingsConf('fx_time_mode=ms\nfx_time_ms=125\n');
assert.equal(scFxTimeMode,'ms'); assert.equal(scFxTimeMs,125);
modeChanges.length=0; scFxMsApply('ms',135); scFxMsApply('ms',125);
assert.deepEqual(modeChanges,[]); // Updating time must not destroy the touch buttons.
assert.equal(scFxMsController(stock),null);
assert.equal(scFxMsStep(parameter,1),true); assert.equal(scFxTimeMs,135);
scFxMsStep(parameter,-1); assert.equal(scFxTimeMs,125);
deferReplies=true;
scFxMsStep(parameter,1); scFxMsStep(parameter,1);
assert.equal(replies.length,1); replies.shift()();
assert.equal(replies.length,1); replies.shift()();
assert.equal(scFxTimeMs,145); assert.equal(scFxMsSending,false);
scFxMsStep(parameter,1); scFxMsExpire();
assert.equal(scFxMsSending,false);
scFxMsStep(parameter,1); replies.shift()();
assert.equal(scFxMsSending,true); // A late reply cannot clear the new request.
replies.shift()(); assert.equal(scFxMsSending,false);
deferReplies=false;
scParseSettingsConf('  fx_time_mode=ms  \r\n\tfx_time_ms=250 \r\n');
assert.equal(scFxTimeMode,'ms'); assert.equal(scFxTimeMs,250);
scParseSettingsConf('fx_time_ms= 250\n'); assert.equal(scFxTimeMs,500);
scParseSettingsConf('fx_time_mode=ms\nfx_time_ms=125\n');
const rates=[1/16,1/8,1/4,1/2,3/4,1,2,4,8,16,32];
const ms=[10,18,33,60,110,200,364,663,1207,2197,4000];
for (let i=0;i<11;i++) { parameter.value=rates[i];scFxMsKnob(parameter);assert.equal(scFxTimeMs,ms[i]); }
scFxMsStep(parameter,1); assert.equal(scFxTimeMs,4000);
for (const selected of [-1,4,6]) { c.scFxSelected=selected;assert.equal(scFxMsController(parameter),null); }
scParseSettingsConf(''); assert.equal(scFxTimeMode,'beats'); assert.equal(scFxTimeMs,500);
scParseSettingsConf('fx_time_mode=msjunk\nfx_time_ms=ms\n');
assert.equal(scFxTimeMode,'beats'); assert.equal(scFxTimeMs,500);
'''
        result = subprocess.run(["node", "-e", script], text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_spinner_patch_keeps_header_and_native_fallbacks(self):
        text = SOURCE.read_text()
        self.assertIn('restoreMode: Binding.RestoreBindingOrValue', text)
        self.assertNotIn("('    property bool isTouchable:", text)
        self.assertNotIn('text: spinner.scMsActive ? "Time (ms)"', text)
        self.assertIn('spinner.scMsActive ? (root.scFxTimeMs + " ms")', text)
        self.assertIn('!spinner.scMsActive || !root.scFxMsStep(parameter, -1)', text)
        self.assertIn('!spinner.scMsActive || !root.scFxMsStep(parameter, 1)', text)
        self.assertNotIn('root.scCall("fxTimeMode=" + (root.scFxTimeMode', text)
        self.assertIn('out.setdefault(ROOT + "Views/MainView/Fx/DJFxSpinner.qml", []).extend', text)

    def test_playback_fx_visibility_and_hidden_means_off(self):
        entries = {}
        for block in re.split(r"EntryBase\.(?:ListEntry|SwitchEntry) \{", bp.PLAYBACK_ENTRIES)[1:]:
            name = re.search(r'text:\s*"([^"]+)"', block).group(1)
            vis = re.search(r'visible:\s*([^\n]+)', block)
            entries[name] = vis.group(1).strip() if vis else "true"
        props = {}
        for line in bp.SERVICES_MAIN.splitlines():
            m = re.match(r"\s*readonly property \w+ (sc\w+):\s*(.+)$", line)
            if m and m.group(1) in ("scBoard", "scIsPlayerBoard", "scHasMixer", "scHasStemsEq", "scHasPxDelay", "scTransitionFxView"):
                props[m.group(1)] = m.group(2).strip()
        stems_handler = re.search(r"onScStemsEqModeChanged:\s*\{([\s\S]*?)\n\t\}", bp.SERVICES_MAIN).group(1)
        has_stems_m = re.search(r"onScHasStemsEqChanged:\s*\{([\s\S]*?)\n\t\}", bp.SERVICES_MAIN)
        has_stems_handler = has_stems_m.group(1) if has_stems_m else ""
        helpers = "\n".join(function(bp.SERVICES_MAIN, n) for n in ("scFxMsInteger", "scFxMsApply", "scParseSettingsConf", "scSyncStemsEqChannel", "scWatchStemsEq", "scApply"))
        prop_defs = "\n".join(
            f"Object.defineProperty(root, {k!r}, {{get() {{ return ({v}); }}, configurable: true}});"
            for k, v in props.items()
        )
        entry_defs = "\n".join(
            f"visible[{k!r}] = () => Boolean({v});"
            for k, v in entries.items()
        )
        script = f"""
const assert = require('assert');
globalThis.root = globalThis;
globalThis.Config = {{device: {{name: 'JP07', isPlayer: true}}, audio: {{hasStemsLevel: true, fx: {{hasDJFx: false, hasNHFx: false}}}}}};
const stripModes = {{1: {{index: 0}}, 2: {{index: 0}}, 3: {{index: 0}}, 4: {{index: 0}}}};
const transitionFx = {{enabled: false}};
globalThis.PlanckGUI = {{views: {{transitionFx}}}};
globalThis.Planck = {{getProperty: () => ({{translator: null}})}};
root.scTr = p => {{
    const m = /^\\/Engine\\/Mixer\\/Channel(\\d)\\/ChannelStrip\\/Mode$/.exec(p);
    return m ? stripModes[Number(m[1])] : null;
}};
root.scEnsureTidal = () => {{}};
root.scKeepPlayhead = () => {{}};
root.scSameItems = () => true;
root.scMidiInitBridge = () => {{}};
root.scHookTimeZonePicker = () => {{}};
root.scProbeCueClocks = () => {{}};
root.scT = s => s;
Object.assign(root, {{
    scPlayer: false, scUiLoaded: false, scFxMsWanted: -1,
    scMasterLimiter: false, scAutoGain: false, scUpfaderEcho: false,
    scStemsEqApplied: false, scStemsEqWatched: false, scBeatJumpPads: false,
    scPioneerFx: 'off', scFxTimeMode: 'beats', scFxTimeMs: 500,
    scFxSelectorAvailable: false, scFxMasterBypassed: false,
    scVersion: '', scBuildTag: '', scLang: 'en', scWave: 'denon', scQuality: 'flac',
    scNeteaseMode: 'official', scUpdateRepo: '', scAccessCode: '', scConsoleUrl: '',
    scLanStems: '', scAutoAnalyze: false, scOmniSource: false, scProlinkShare: false,
    scProlinkExport: false, scFourDeckRole: '', scFourDeckOn: false,
    scFourDeckRoleSetting: 'auto', scShadowMode: 'auto', scSources: [], scLoggedIn: [],
    scScope: 'deezer', scBroadcastOn: false, scCueCountdown: true, scDualColumn: false,
    scTracklistWaveform: true, scSyncGuardPct: 0, scSwapLoopOrder: false,
    scLedBrightness: 100, scUIScale: 100,
    scLastGoodTimeZone: '', scPendingTimeZone: '', scRestoringTimeZone: false,
}});
let stemsEqVal = false;
function onHasStemsEqChanged() {{
{has_stems_handler}
}}
Object.defineProperty(root, 'scStemsEqMode', {{
    get() {{ return stemsEqVal; }},
    set(v) {{
        const next = !!v;
        if (next === stemsEqVal) return;
        stemsEqVal = next;
        {stems_handler}
    }}
}});
{prop_defs}
{helpers}
const visible = {{}};
{entry_defs}

function resetUnit(board, opts = {{}}) {{
    Config.device.name = board;
    Config.device.isPlayer = opts.isPlayer !== undefined ? opts.isPlayer : ['JP07', 'JP08', 'JP13', 'JP14'].includes(board);
    Config.audio.hasStemsLevel = opts.hasStemsLevel !== undefined ? opts.hasStemsLevel : true;
    Config.audio.fx.hasDJFx = !!opts.hasDJFx;
    Config.audio.fx.hasNHFx = !!opts.hasNHFx;
    for (let ch = 1; ch <= 4; ch++) stripModes[ch].index = 0;
    transitionFx.enabled = false;
    scStemsEqApplied = false;
    stemsEqVal = false;
    scPlayer = false;
    scUpfaderEcho = false;
    scPioneerFx = 'off';
    scFxTimeMode = 'beats';
    scFxTimeMs = 500;
    scFxMasterBypassed = false;
    scFxSelectorAvailable = false;
    scUiLoaded = true;
}}

// 1. Standalone players (SC5000/SC5000M/SC6000/SC6000M, including 4-deck isPlayer=false):
for (const board of ['JP07', 'JP08', 'JP13', 'JP14']) {{
    resetUnit(board, {{isPlayer: false, hasStemsLevel: true}});
    scApply({{
        player: true, fxSelectorAvailable: false,
        upfaderEcho: true, stemsEqMode: true,
        pioneerFx: 'off', fxTimeMode: 'ms', fxTimeMs: 250,
    }});
    assert.equal(visible['Master Soft Clipper'](), true, board);
    assert.equal(visible['Auto-Gain (0 dB Normalize)'](), true, board);
    assert.equal(visible['Echo Out'](), false, board + ' Echo Out hidden');
    assert.equal(visible['Stems EQ Mode'](), false, board + ' Stems EQ hidden');
    assert.equal(visible['Color FX Style'](), false, board + ' Color FX Style hidden (no PX effects on standalone players)');
    assert.equal(visible['PX Delay Time Mode'](), false, board + ' PX Delay Time Mode hidden when Color FX Off');
    assert.equal(visible['PX Delay Time (ms)'](), false, board + ' PX Delay Time (ms) hidden when Color FX Off');
    // Hidden entries keep stored values and behave as Off:
    assert.equal(scUpfaderEcho, true);
    assert.equal(transitionFx.enabled, false, board + ' hidden Echo Out stays Off');
    assert.equal(scStemsEqMode, true);
    for (let ch = 1; ch <= 4; ch++) assert.equal(stripModes[ch].index, 0, board + ' hidden Stems EQ stays Off');
    assert.equal(scFxTimeMode, 'ms');
    assert.equal(scFxTimeMs, 250);

    // Non-delay styles (Slip Roll, Space) also keep PX Delay entries hidden:
    for (const style of ['slip_roll', 'space']) {{
        scPioneerFx = style;
        assert.equal(visible['PX Delay Time Mode'](), false, board + ' ' + style);
        assert.equal(visible['PX Delay Time (ms)'](), false, board + ' ' + style + ' ms');
        assert.equal(scFxTimeMode, 'ms');
        assert.equal(scFxTimeMs, 250);
    }}
    // A delay style stored before this rule does not bring the PX Delay rows back on a player:
    for (const style of ['spiral', 'pitch_echo', 'dub_echo']) {{
        scPioneerFx = style;
        assert.equal(visible['PX Delay Time Mode'](), false, board + ' ' + style);
        assert.equal(visible['PX Delay Time (ms)'](), false, board + ' ' + style + ' ms');
    }}
}}

// Late player detection after settings.conf parsed stems_eq_mode=1 resets ChannelStrip/Mode to Off:
resetUnit('', {{isPlayer: false, hasStemsLevel: true}});
scParseSettingsConf('stems_eq_mode=1\\n');
scPlayer = true;
onHasStemsEqChanged();
assert.equal(scStemsEqMode, true);
for (let ch = 1; ch <= 4; ch++) assert.equal(stripModes[ch].index, 0, 'late scPlayer resets Stems EQ to Off');

// 2. Mixer consoles without native PX selector (PRIME 2, PRIME GO, Mixstream, SC LIVE 4, PRIME 4+, AZ04):
for (const [board, hasStems] of [['JC16', true], ['JP11', true], ['NH08', true], ['JP21', true], ['JC11S', true], ['JP24', false], ['RMZ2', true]]) {{
    resetUnit(board, {{isPlayer: false, hasStemsLevel: hasStems, hasNHFx: board === 'NH08'}});
    scApply({{
        player: false, fxSelectorAvailable: false,
        upfaderEcho: true, stemsEqMode: true,
        pioneerFx: 'off', fxTimeMode: 'ms', fxTimeMs: 364,
    }});
    assert.equal(visible['Echo Out'](), true, board + ' Echo Out shown');
    assert.equal(transitionFx.enabled, true, board + ' Echo Out active');
    assert.equal(visible['Stems EQ Mode'](), hasStems, board + ' Stems EQ gate');
    for (let ch = 1; ch <= 4; ch++) assert.equal(stripModes[ch].index, hasStems ? 1 : 0, board + ' Stems EQ state');
    assert.equal(scStemsEqMode, true);
    assert.equal(visible['Color FX Style'](), true, board + ' Color FX Style shown');
    assert.equal(visible['PX Delay Time Mode'](), false, board + ' PX Delay hidden when Off');
    assert.equal(visible['PX Delay Time (ms)'](), false, board + ' PX Delay ms hidden when Off');
    assert.equal(scFxTimeMode, 'ms');
    assert.equal(scFxTimeMs, 364);
    scPioneerFx = 'dub_echo';
    assert.equal(visible['PX Delay Time Mode'](), true, board + ' PX Delay shown on dub_echo');
    assert.equal(visible['PX Delay Time (ms)'](), true, board + ' PX Delay ms shown on dub_echo');
}}

// 3. PRIME 4 (JC11, Engine 5.3.0) with native FX selector:
resetUnit('JC11', {{isPlayer: false, hasStemsLevel: true, hasDJFx: true}});
scApply({{
    player: false, fxSelectorAvailable: true,
    upfaderEcho: false, stemsEqMode: false,
    pioneerFx: 'off', fxTimeMode: 'beats', fxTimeMs: 500,
}});
assert.equal(visible['Echo Out'](), true, 'JC11 Echo Out shown');
assert.equal(visible['Stems EQ Mode'](), true, 'JC11 Stems EQ shown');
assert.equal(visible['Color FX Style'](), false, 'JC11 Color FX Style hidden');
assert.equal(visible['PX Delay Time Mode'](), true, 'JC11 PX Delay Time Mode shown');
assert.equal(visible['PX Delay Time (ms)'](), false, 'JC11 PX Delay Time (ms) hidden when beats');
scFxMsApply('ms', 663);
assert.equal(visible['PX Delay Time (ms)'](), true, 'JC11 PX Delay Time (ms) shown when ms');

// Safety fallback: master bypassed without selector flags hides all three:
resetUnit('JP07', {{isPlayer: true, hasStemsLevel: false, hasDJFx: false, hasNHFx: false}});
scApply({{player: true, fxSelectorAvailable: true, pioneerFx: 'spiral', fxTimeMode: 'ms', fxTimeMs: 500}});
assert.equal(visible['Color FX Style'](), false);
assert.equal(visible['PX Delay Time Mode'](), false);
assert.equal(visible['PX Delay Time (ms)'](), false);
"""
        result = subprocess.run(["node", "-e", script], text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_stems_eq_mode_applies_when_stemmed_track_loads(self):
        props = {}
        for line in bp.SERVICES_MAIN.splitlines():
            m = re.match(r"\s*readonly property \w+ (sc\w+):\s*(.+)$", line)
            if m and m.group(1) in ("scBoard", "scIsPlayerBoard", "scHasMixer", "scHasStemsEq", "scTransitionFxView"):
                props[m.group(1)] = m.group(2).strip()
        stems_handler = re.search(r"onScStemsEqModeChanged:\s*\{([\s\S]*?)\n\t\}", bp.SERVICES_MAIN).group(1)
        helpers = "\n".join(function(bp.SERVICES_MAIN, n) for n in ("scFxMsInteger", "scFxMsApply", "scParseSettingsConf", "scSyncStemsEqChannel", "scWatchStemsEq", "scApply"))
        prop_defs = "\n".join(
            f"Object.defineProperty(root, {k!r}, {{get() {{ return ({v}); }}, configurable: true}});"
            for k, v in props.items()
        )
        script = f"""
const assert = require('assert');
globalThis.root = globalThis;
globalThis.Config = {{device: {{name: 'JP21', isPlayer: false}}, audio: {{hasStemsLevel: true, fx: {{hasDJFx: false, hasNHFx: false}}}}}};
globalThis.PlanckGUI = {{views: {{transitionFx: {{enabled: false}}}}}};
globalThis.Planck = {{getProperty: () => ({{translator: null}})}};
function makeSig() {{
    const listeners = [];
    return {{
        connect(fn) {{ listeners.push(fn); }},
        emit() {{ for (const fn of listeners) fn(); }},
        get count() {{ return listeners.length; }},
    }};
}}
const stemmed = {{1: {{state: false, stateChanged: makeSig()}}, 2: {{state: false, stateChanged: makeSig()}}, 3: {{state: false, stateChanged: makeSig()}}, 4: {{state: false, stateChanged: makeSig()}}}};
const songLoaded = {{1: {{state: false, stateChanged: makeSig()}}, 2: {{state: false, stateChanged: makeSig()}}, 3: {{state: false, stateChanged: makeSig()}}, 4: {{state: false, stateChanged: makeSig()}}}};
// Simulates Engine ChannelStripControl::ModeAccessFilter: rejects Mode=1 when Track/Stemmed is false.
const stripModes = {{}};
for (let ch = 1; ch <= 4; ch++) {{
    let idx = 0;
    stripModes[ch] = {{
        writes: 0,
        get index() {{ return idx; }},
        set index(v) {{ this.writes++; if (v === 1 && !stemmed[ch].state) return; idx = v; }},
    }};
}}
root.scTr = p => {{
    let m = /^\\/Engine\\/Mixer\\/Channel(\\d)\\/ChannelStrip\\/Mode$/.exec(p);
    if (m) return stripModes[Number(m[1])];
    m = /^\\/Engine\\/Deck(\\d)\\/Track\\/Stemmed$/.exec(p);
    if (m) return stemmed[Number(m[1])];
    m = /^\\/Engine\\/Deck(\\d)\\/Track\\/SongLoaded$/.exec(p);
    if (m) return songLoaded[Number(m[1])];
    return null;
}};
root.scEnsureTidal = () => {{}};
root.scKeepPlayhead = () => {{}};
root.scSameItems = () => true;
root.scMidiInitBridge = () => {{}};
root.scHookTimeZonePicker = () => {{}};
root.scProbeCueClocks = () => {{}};
root.scT = s => s;
Object.assign(root, {{
    scPlayer: false, scUiLoaded: false, scFxMsWanted: -1,
    scMasterLimiter: false, scAutoGain: false, scUpfaderEcho: false,
    scStemsEqApplied: false, scStemsEqWatched: false, scBeatJumpPads: false,
    scPioneerFx: 'off', scFxTimeMode: 'beats', scFxTimeMs: 500,
    scFxSelectorAvailable: false, scFxMasterBypassed: false,
    scVersion: '', scBuildTag: '', scLang: 'en', scWave: 'denon', scQuality: 'flac',
    scNeteaseMode: 'official', scUpdateRepo: '', scAccessCode: '', scConsoleUrl: '',
    scLanStems: '', scAutoAnalyze: false, scOmniSource: false, scProlinkShare: false,
    scProlinkExport: false, scFourDeckRole: '', scFourDeckOn: false,
    scFourDeckRoleSetting: 'auto', scShadowMode: 'auto', scSources: [], scLoggedIn: [],
    scScope: 'deezer', scBroadcastOn: false, scCueCountdown: true, scDualColumn: false,
    scTracklistWaveform: true, scSyncGuardPct: 0, scSwapLoopOrder: false,
    scLedBrightness: 100, scUIScale: 100,
    scLastGoodTimeZone: '', scPendingTimeZone: '', scRestoringTimeZone: false,
}});
let stemsEqVal = false;
Object.defineProperty(root, 'scStemsEqMode', {{
    get() {{ return stemsEqVal; }},
    set(v) {{
        const next = !!v;
        if (next === stemsEqVal) return;
        stemsEqVal = next;
        {stems_handler}
    }}
}});
{prop_defs}
{helpers}

// Boot with Stems EQ Mode = On before any track is loaded (Engine rejects Mode=1 on empty decks).
scApply({{stemsEqMode: true}});
assert.equal(stripModes[1].index, 0);

// Loading a stemmed track on Deck 1 must automatically switch Channel 1 to Stems mode (1).
stemmed[1].state = true;
stemmed[1].stateChanged.emit();
songLoaded[1].state = true;
songLoaded[1].stateChanged.emit();
assert.equal(stripModes[1].index, 1, 'Channel 1 switches to Stems when stemmed track loads');
assert.equal(stripModes[2].index, 0, 'Empty Channel 2 stays EQ');

// Manual Shift+Cue toggle to EQ while the same track remains loaded is preserved.
stripModes[1].index = 0;
assert.equal(stripModes[1].index, 0);

// Loading another stemmed track re-applies Stems EQ Mode = On.
songLoaded[1].state = false;
songLoaded[1].stateChanged.emit();
songLoaded[1].state = true;
songLoaded[1].stateChanged.emit();
assert.equal(stripModes[1].index, 1);

// SongLoaded firing BEFORE Stemmed (Deck 3): Stemmed.stateChanged alone must switch to Stems.
songLoaded[3].state = true;
songLoaded[3].stateChanged.emit();
assert.equal(stripModes[3].index, 0);
stemmed[3].state = true;
stemmed[3].stateChanged.emit();
assert.equal(stripModes[3].index, 1, 'Stemmed after SongLoaded switches Channel 3 to Stems');

// Repeated scApply polls must not duplicate listeners or override manual Shift+Cue EQ toggle.
stripModes[1].index = 0;
const writesBefore = stripModes[1].writes;
for (let i = 0; i < 5; i++) scApply({{stemsEqMode: true}});
assert.equal(stemmed[1].stateChanged.count, 1, 'single Stemmed listener per deck');
assert.equal(songLoaded[1].stateChanged.count, 1, 'single SongLoaded listener per deck');
assert.equal(stripModes[1].index, 0, 'repeated scApply preserves manual Shift+Cue');
assert.equal(stripModes[1].writes, writesBefore);

// Turning Stems EQ Mode Off resets Channel 1 and does not re-enable on subsequent loads.
scApply({{stemsEqMode: false}});
assert.equal(stripModes[1].index, 0);
stemmed[2].state = true;
stemmed[2].stateChanged.emit();
assert.equal(stripModes[2].index, 0);

// #176: board without Stems EQ (e.g. JP24 hasStemsLevel=false) never writes ChannelStrip/Mode=1.
Config.device.name = 'JP24';
Config.audio.hasStemsLevel = false;
stemmed[4].state = true;
stripModes[4].index = 0;
const w4 = stripModes[4].writes;
scApply({{stemsEqMode: true}});
stemmed[4].stateChanged.emit();
songLoaded[4].state = true;
songLoaded[4].stateChanged.emit();
assert.equal(stripModes[4].index, 0, '#176 JP24 stays EQ');
assert.equal(stripModes[4].writes, w4);

// Initial sync on first UI load when a stemmed track is already loaded on Deck 4.
Config.device.name = 'JP21';
Config.audio.hasStemsLevel = true;
scUiLoaded = false;
scStemsEqWatched = false;
scStemsEqApplied = false;
scApply({{stemsEqMode: true}});
assert.equal(stripModes[4].index, 1, 'initial sync applies Stems on already-loaded Deck 4');
"""
        result = subprocess.run(["node", "-e", script], text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)


if __name__ == "__main__":
    unittest.main()
