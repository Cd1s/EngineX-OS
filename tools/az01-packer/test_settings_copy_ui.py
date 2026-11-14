"""Settings copy keeps native USB consent and explicitly confirms Engine restarts."""
import importlib.util
import json
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import unittest

REPO = Path(__file__).resolve().parents[2]
UI = REPO / "tools/ui-mod/settings_copy.qml"
BUILD = REPO / "tools/ui-mod/build_packs.py"
LAB = Path("/root/build/sc5000-lab/5.3.0/qrc")


def js_function(name):
    text = UI.read_text()
    start = text.index("function " + name + "(")
    brace = text.index("{", start)
    depth, end = 1, brace + 1
    while depth:
        depth += (text[end] == "{") - (text[end] == "}")
        end += 1
    return text[start:end]


class SettingsCopyUI(unittest.TestCase):
    @unittest.skipUnless(shutil.which("node"), "Node is required")
    def test_restart_requires_accept_and_refuses_playback(self):
        script = r'''
const assert = require('assert');
let playing = false, posted = [], finished, requests = [], quits = 0;
const Prompt = {Cancel: 1, Ok: 2, AcceptRole: 3};
const QuitReasons = {AnalyticsChange: 12};
const Planck = {quit: (reason, force) => {assert.equal(reason, 12); assert.equal(force, true); quits++;}};
const PlanckGUI = {views:{prompts:{post: p => {posted.push(p); return {finished:{connect:f => {finished=f}}};}}}};
const scT = s => s, scCopyEscape = s => s;
const scCopyPlaying = () => playing;
const scCopyNotice = s => posted.push(s);
const scCopyCapture = () => ({native:{DisplayFileName:{type:1,state:false}},entries:{DisplayFileName:[]}});
const scCopyCurrentDrive = () => '/media/Test';
const scCopySummary = () => 'Display: 1';
const scCopyRequest = (action, body, callback) => {requests.push({action,body}); callback({ok:true});};
FUNCTION
playing = true; scCopyConfirm(null,false,false);
assert.equal(requests.length,0); assert.equal(quits,0);
playing = false; scCopyConfirm(null,false,false);
assert(posted.at(-1).message.includes('Engine will restart and playback will stop.'));
finished(0); assert.equal(quits,0); assert.equal(requests.at(-1).action,'cancel');
scCopyConfirm(null,false,false); finished(Prompt.AcceptRole);
assert.equal(quits,1); assert.equal(requests.at(-1).action,'apply');
assert.equal(requests.at(-1).body.confirmed,true); assert.equal(requests.at(-1).body.playing,false);
playing = false; scCopyConfirm(null,true,false); playing = true; finished(Prompt.AcceptRole);
assert.equal(quits,1);
'''.replace("FUNCTION", js_function("scCopyConfirm"))
        subprocess.run(["node", "-e", script], check=True, capture_output=True, text=True)

    @unittest.skipUnless(shutil.which("node"), "Node is required")
    def test_source_allows_only_after_its_own_prompt(self):
        script = r'''
const assert=require('assert'); let finish, actions=[], captured=0, scCopySeenIncoming='';
const Prompt={No:1,Yes:2,AcceptRole:3};
const scT=x=>x, scCopyEscape=x=>x;
const PlanckGUI={views:{prompts:{post:p=>({finished:{connect:f=>finish=f}})}}};
const scCopyCapture=()=>{captured++;return {native:{DisplayFileName:{type:1,state:false}}}};
const scCopyRequest=(action,body,callback)=>{actions.push({action,body});callback({ok:true})};
const scCopyNotice=()=>{};
FUNCTION
scCopySourcePrompt({token:'request',peer:'192.0.2.20'});
assert.equal(actions.length,0);assert.equal(captured,0);
finish(0);assert.equal(actions.at(-1).action,'deny');assert.equal(captured,0);
scCopySourcePrompt({token:'next',peer:'192.0.2.20'});finish(Prompt.AcceptRole);
assert.equal(actions.at(-1).action,'allow');assert.equal(captured,1);
'''.replace("FUNCTION", js_function("scCopySourcePrompt"))
        subprocess.run(["node", "-e", script], check=True, capture_output=True, text=True)

    @unittest.skipUnless(LAB.is_dir(), "native QML fixture is on the ARM host")
    def test_native_dialog_and_all_languages(self):
        with tempfile.TemporaryDirectory() as tmp:
            out = Path(tmp) / "packs"
            subprocess.run(["python3", str(BUILD), "--qrc", str(LAB), "--out", str(out)], check=True, capture_output=True, text=True)
            for language in ("en", "zh-Hans", "zh-Hant"):
                base = out / language / "Skins/Embedded/Views/ControlCenter/Tabs/Preferences"
                dialog = (base / "Entries/LoadPreferencesDialog.qml").read_text()
                device = (base / "DeviceTabContent.qml").read_text()
                self.assertIn("root.scCopyUSBPrompt", dialog)
                self.assertIn("Prompt.No | Prompt.Yes", dialog)
                self.assertIn("if (role === Prompt.AcceptRole)", dialog)
                self.assertIn("loadPreferences()", dialog)
                self.assertIn("CheckBox", dialog)
                self.assertIn("scCopyIncludeAvailable", dialog)
                self.assertIn('objectName: "EngineX_CopySettings"', device)
                self.assertIn('objectName: "EngineX_UndoSettings"', device)
                main = (out / language / "Skins/Embedded/EmbeddedMain.qml").read_text()
                self.assertIn('body.confirmed = true; body.playing = false', main)
                self.assertIn('Planck.quit(QuitReasons.AnalyticsChange, true)', main)
