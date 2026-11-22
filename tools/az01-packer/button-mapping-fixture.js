// Button mapping, the EngineX actions: the on-disk assignment file (pkg/mapping) sets an Action
// property from the controller's QML engine; the UI pack's engine reacts. This runs the stock
// code for the first half and the generated QML's own functions for the second, both from the
// 5.3.0 qrc, on a model of the property and stock's ActionTranslatorStateChangeNotifier.
// Only the deck can show that a property made with createProperty(name, false, 3) reports
// valueType "Action" to these outputs; the model takes it from stock's own use of type 3.
const assert = require('node:assert/strict');
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
const fn = (text, name) => {
    const at = text.indexOf('function ' + name + '(');
    assert.ok(at >= 0, name);
    return body(text, at);
};

// stock's notifier, as stock's file has it (checked against the qrc below)
for (const line of ['enabled: prop && prop.translator && prop.translator.state', 'prop.translatorType === 3',
    'if (previousStateTrueCount !== stateTrueCount) {', 'previousStateTrueCount = stateTrueCount', 'prop.translator.state = false'])
    assert.ok(source.notifier.includes(line), line);

// an Action property as two engines see it: one state, one count of the times it went true
function property() {
    const p = { valueType: 'Action', editable: true, _state: false, stateTrueCount: 0 };
    Object.defineProperty(p, 'state', {
        get() { return this._state; },
        set(v) { if (v && !this._state) this.stateTrueCount++; this._state = !!v; },
    });
    return p;
}
function notifier(translator, onChanged) {
    let previous = 0;
    return { poll() {
        if (!translator.state) return;
        if (previous !== translator.stateTrueCount) { previous = translator.stateTrueCount; translator.state = false; onChanged(); }
    } };
}

// 1. what the generated assignment does to the property: stock's ActionOutput (press, shift) and
// ComboActionOutput (double, hold)
const actionSetValue = new Function('target', 'behaviour', 'force', 'alwaysTrigger', 'debug', 'privatePart', 'resetStoredPropertyValue', 'hold',
    'channel', 'value', 'assignmentEnabled', body(source.actionOutput, source.actionOutput.indexOf('function setValue(')));
const force = { auto: 0, action: 1, hold: 2, twoState: 3 };
const press = (translator, value, enabled = true) => actionSetValue({ get: () => translator }, 0, force, false, false,
    { enabledTargets: {}, heldPropertyPath: '' }, () => {}, () => {}, 0, value, enabled);
const modifyTarget = new Function('target', body(source.combo, source.combo.indexOf('function modifyTarget(')));

{
    const t = property();
    press(t, 127, false);
    assert.equal(t.state, false, 'a disabled assignment (Shift in the other state) does nothing');
    press(t, 0);
    assert.equal(t.state, false, 'a release does nothing');
    press(t, 127);
    assert.equal(t.state, true, 'a press latches the Action');
    const c = property();
    modifyTarget({ get: () => c });
    assert.equal(c.state, true, 'a double or hold latches it through the combo');
    c.editable = false;
    c.state = false;
    modifyTarget({ get: () => c });
    assert.equal(c.state, false, 'a property that is not editable is left alone');
}

// 2. what the UI pack does with it
const main = source.main;
const catalogue = source.catalogue;
const enginexActions = catalogue.actions.filter(a => a.kind === 'enginex');
assert.ok(enginexActions.length >= 2, 'the catalogue has the waveform and one more EngineX action');
const handlers = { EngineXMapWaveform: 'scMapWaveformNext', EngineXMapCueCountdown: 'scMapCueCountdownToggle' };
for (const a of enginexActions) {
    assert.ok(a.path.startsWith('/GUI/Scripted/'), a.id);
    const name = a.path.slice('/GUI/Scripted/'.length);
    assert.ok(handlers[name], `${a.id}: the UI pack has no reaction for ${name}`);
    assert.ok(main.includes(`Planck.createProperty("${name}", false, 3)`), `${name} is created as an Action property`);
    assert.ok(main.includes(`Planck.getProperty("${a.path}")`), `${name} is read back at the path the assignment writes`);
    assert.match(main, new RegExp(`ActionTranslatorStateChangeNotifier \\{\\n\\t\\tprop: root\\.\\w+ \\|\\| null\\n\\t\\tonChanged: root\\.${handlers[name]}\\(\\)\\n\\t\\}`), name);
    assert.match(main, new RegExp(`readonly property var \\w+: scMapActionRefs && Planck\\.getProperty\\("${a.path}"\\)`), name);
}
assert.equal(Object.keys(handlers).length, enginexActions.length, 'no UI reaction without a catalogue action');

const order = JSON.parse(main.match(/readonly property var scMapWaveforms: (\[[^\]]*\])/)[1]);
assert.deepEqual(order, source.uiWaveforms, 'the cycle visits the waveform types the settings API accepts, in its order');

const calls = [], waves = [];
let scWave = 'denon', scCueCountdown = true;
const next = new Function('scMapWaveforms', 'scWave', 'scSetWave', fn(main, 'scMapWaveformNext'));
const toggle = new Function('scCueCountdown', 'scCall', fn(main, 'scMapCueCountdownToggle'));
const scSetWave = id => { waves.push(id); scWave = id; };

// press -> property -> notifier -> the function: three presses walk the three types and wrap
{
    const t = property();
    const n = notifier(t, () => next(order, scWave, scSetWave));
    for (let i = 0; i < 3; i++) { press(t, 127); n.poll(); press(t, 0); n.poll(); }
    assert.deepEqual(waves, ['rgb', '3band', 'denon']);
    assert.equal(t.state, false, 'stock\'s notifier reset the latch each time');
    press(t, 127); press(t, 127); n.poll(); // two presses before the GUI looked: the property is latched once
    assert.equal(waves.length, 4, 'two presses inside one GUI turn are one step (the latch)');
    const stray = notifier(property(), () => assert.fail('a property nobody pressed'));
    stray.poll();
    next(order, 'bogus', scSetWave);
    assert.equal(waves[waves.length - 1], 'denon', 'an unknown current type starts the cycle again');
}
// the cue countdown goes through the same call the Settings switch makes
{
    const t = property();
    const n = notifier(t, () => toggle(scCueCountdown, q => calls.push(q)));
    press(t, 127); n.poll();
    assert.deepEqual(calls, ['cueCountdown=0']);
    scCueCountdown = false;
    press(t, 127); n.poll();
    assert.deepEqual(calls, ['cueCountdown=0', 'cueCountdown=1']);
}
assert.ok(main.includes('function scSetWave(id) {') && fn(main, 'scMapWaveformNext').includes('scSetWave('),
    'the waveform goes through scSetWave: the same prompt and the same call as the Settings page');
// the setting keys the catalogue names are the ones the settings API reads
for (const a of enginexActions) assert.ok(source.settingsKeys.includes(a.setting), `${a.id}: ${a.setting} is not a settings key`);
console.log('ok');
