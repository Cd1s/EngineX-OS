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
                              ("scFxMsController", "scFxMsStep", "scFxMsKnob"))
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
const scFxControllers = [c], calls=[];
function scCall(q) { calls.push(q); scFxTimeMs=Number(q.split('=')[1]); }
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
        self.assertIn('property bool isTouchable: scMsActive || (parameter && parameter.isTouchable)', text)
        self.assertIn('spinner.scMsActive ? (root.scFxTimeMs + " ms")', text)
        self.assertIn('!spinner.scMsActive || !root.scFxMsStep(parameter, -1)', text)
        self.assertIn('!spinner.scMsActive || !root.scFxMsStep(parameter, 1)', text)
        self.assertNotIn('root.scCall("fxTimeMode=" + (root.scFxTimeMode', text)
        self.assertIn('out.setdefault(ROOT + "Views/MainView/Fx/DJFxSpinner.qml", []).extend', text)


if __name__ == "__main__":
    unittest.main()
