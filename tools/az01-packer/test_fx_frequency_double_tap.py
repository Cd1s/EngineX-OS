"""Tests for Issue #317: reset effect frequency to All Bands with a double tap."""
import importlib.util
from pathlib import Path
import shutil
import subprocess
import unittest

SOURCE = Path(__file__).parents[1] / "ui-mod" / "build_packs.py"
spec = importlib.util.spec_from_file_location("build_packs", SOURCE)
bp = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bp)


class FxFrequencyDoubleTapTest(unittest.TestCase):
    def test_build_packs_declares_progress_parameter_patch(self):
        text = SOURCE.read_text(encoding="utf-8")
        self.assertIn('ROOT + "Views/MainView/Fx/DJFxProgressParameter.qml"', text)
        self.assertIn('ROOT + "Views/MainView/Fx/DJFxParameter.qml"', text)
        self.assertIn('function resetToCenter()', text)
        self.assertIn('TapHandler {', text)
        self.assertIn('objectName: "DJFx_Parameter_DoubleTap"', text)
        self.assertIn('enabled: isTouchable && bipolar', text)
        self.assertIn('onDoubleTapped: slider.resetToCenter()', text)
        self.assertIn('function triggerValueAnimation()', text)

    def test_patch_applies_cleanly_to_stock_qml(self):
        lab = Path("/root/build/sc5000-lab/5.3.0/qrc")
        if not lab.is_dir():
            self.skipTest("Firmware lab 5.3.0/qrc not available")

        bp.ROOT = "Skins/Embedded/"
        # Test DJFxProgressParameter.qml
        f_prog = lab / "Skins/Embedded/Views/MainView/Fx/DJFxProgressParameter.qml"
        self.assertTrue(f_prog.is_file())
        prog_text = f_prog.read_text(encoding="utf-8")

        all_edits = bp.edits(dev=False)
        prog_patches = all_edits.get("Skins/Embedded/Views/MainView/Fx/DJFxProgressParameter.qml", [])
        self.assertTrue(prog_patches, "No patches for DJFxProgressParameter.qml")

        patched_prog = prog_text
        for old, new in prog_patches:
            self.assertIn(old, patched_prog)
            patched_prog = bp.patch(patched_prog, old, new, "DJFxProgressParameter.qml")

        self.assertIn("function resetToCenter()", patched_prog)
        self.assertIn("DJFx_Parameter_DoubleTap", patched_prog)

        # Test DJFxParameter.qml
        f_param = lab / "Skins/Embedded/Views/MainView/Fx/DJFxParameter.qml"
        self.assertTrue(f_param.is_file())
        param_text = f_param.read_text(encoding="utf-8")
        param_patches = all_edits.get("Skins/Embedded/Views/MainView/Fx/DJFxParameter.qml", [])
        self.assertTrue(param_patches, "No patches for DJFxParameter.qml")

        patched_param = param_text
        for old, new in param_patches:
            self.assertIn(old, patched_param)
            patched_param = bp.patch(patched_param, old, new, "DJFxParameter.qml")

        self.assertIn("function triggerValueAnimation()", patched_param)

    @unittest.skipUnless(shutil.which("node"), "node required")
    def test_reset_to_center_behavior_in_js(self):
        script = r'''
const assert = require('assert');

function makeSlider(options) {
    let triggered = false;
    const parent = {
        triggerValueAnimation: () => { triggered = true; }
    };
    const parameter = {
        value: options.initialValue,
        rangeStart: options.from !== undefined ? options.from : 0.0,
        rangeEnd: options.to !== undefined ? options.to : 1.0,
        isTouchable: options.isTouchable !== undefined ? options.isTouchable : true
    };
    const slider = {
        from: parameter.rangeStart,
        to: parameter.rangeEnd,
        value: parameter.value,
        bipolar: !!options.bipolar
    };
    const isTouchable = parameter.isTouchable;
    const bipolar = slider.bipolar;

    function resetToCenter() {
        if (!isTouchable || !bipolar || !parameter) return;
        const center = slider.from + (slider.to - slider.from) * 0.5;
        slider.value = center;
        parameter.value = center;
        if (typeof parent.triggerValueAnimation === "function") {
            parent.triggerValueAnimation();
        }
    }

    const tapHandler = {
        objectName: "DJFx_Parameter_DoubleTap",
        enabled: isTouchable && bipolar,
        doubleTapped: () => {
            if (tapHandler.enabled) resetToCenter();
        }
    };

    return { slider, parameter, tapHandler, getTriggered: () => triggered, resetToCenter };
}

// 1. Frequency parameter: bipolar, touchable, from=0, to=1, initial=0.8
{
    const env = makeSlider({ initialValue: 0.8, from: 0.0, to: 1.0, bipolar: true, isTouchable: true });
    assert.strictEqual(env.tapHandler.enabled, true);
    assert.strictEqual(env.slider.value, 0.8);
    assert.strictEqual(env.parameter.value, 0.8);

    env.tapHandler.doubleTapped();
    assert.strictEqual(env.slider.value, 0.5);
    assert.strictEqual(env.parameter.value, 0.5);
    assert.strictEqual(env.getTriggered(), true);
}

// 2. Unipolar parameter (e.g. Wet/Dry): bipolar=false
{
    const env = makeSlider({ initialValue: 0.8, from: 0.0, to: 1.0, bipolar: false, isTouchable: true });
    assert.strictEqual(env.tapHandler.enabled, false);

    env.tapHandler.doubleTapped();
    assert.strictEqual(env.slider.value, 0.8);
    assert.strictEqual(env.parameter.value, 0.8);
    assert.strictEqual(env.getTriggered(), false);
}

// 3. Hardware-only deck (not touchable): isTouchable=false
{
    const env = makeSlider({ initialValue: 0.8, from: 0.0, to: 1.0, bipolar: true, isTouchable: false });
    assert.strictEqual(env.tapHandler.enabled, false);

    env.tapHandler.doubleTapped();
    assert.strictEqual(env.slider.value, 0.8);
    assert.strictEqual(env.parameter.value, 0.8);
    assert.strictEqual(env.getTriggered(), false);
}

// 4. Custom range (e.g. -1 to 1): center is 0.0
{
    const env = makeSlider({ initialValue: 0.7, from: -1.0, to: 1.0, bipolar: true, isTouchable: true });
    assert.strictEqual(env.tapHandler.enabled, true);

    env.tapHandler.doubleTapped();
    assert.strictEqual(env.slider.value, 0.0);
    assert.strictEqual(env.parameter.value, 0.0);
    assert.strictEqual(env.getTriggered(), true);
}

console.log("ALL_JS_CHECKS_PASS");
'''
        res = subprocess.run(["node", "-e", script], capture_output=True, text=True, check=True)
        self.assertIn("ALL_JS_CHECKS_PASS", res.stdout)


if __name__ == "__main__":
    unittest.main()
